package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestMessagePostgresRoundTrip is opt-in because it needs a disposable real
// Postgres database. It covers direct send, recipient-only causal replies,
// oldest-unacknowledged receive state, and account archive export/import as
// one lifecycle.
func TestMessagePostgresRoundTrip(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx, "message-test@witwave.ai", "message test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "sender")
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	bystander, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "bystander")
	if err != nil {
		t.Fatal(err)
	}

	senderPrincipal := Principal{
		Kind: PrincipalAgent, ID: sender.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: sender.Name, AccountStatus: "active",
	}
	recipientPrincipal := Principal{
		Kind: PrincipalAgent, ID: recipient.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: recipient.Name, AccountStatus: "active",
	}
	bystanderPrincipal := Principal{
		Kind: PrincipalAgent, ID: bystander.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: bystander.Name, AccountStatus: "active",
	}

	msg, err := st.SendMessage(ctx, senderPrincipal, SendMessageInput{
		ToAgent: recipient.ID, Kind: "request", Body: "preserve me",
		Payload: json.RawMessage(`{"task":42}`), IdempotencyKey: "round-trip-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.SendMessage(ctx, senderPrincipal, SendMessageInput{
		ToAgent: recipient.ID, Kind: "request", Body: "second request",
		ThreadID: msg.ThreadID, IdempotencyKey: "round-trip-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.CausalDepth != 1 || second.CausalDepth != 1 {
		t.Fatalf("direct message depths in reused thread = %d/%d, want 1/1", msg.CausalDepth, second.CausalDepth)
	}
	baseTime := time.Now().UTC().Add(-time.Hour)
	if _, err := st.pool.Exec(ctx, `UPDATE agent_messages SET created_at=$2 WHERE id=$1`, msg.ID, baseTime); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE agent_messages SET created_at=$2 WHERE id=$1`, second.ID, baseTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	claim, err := st.ClaimMessage(ctx, recipientPrincipal, msg.ID, ClaimMessageInput{
		IdempotencyKey: "claim-round-trip-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Processing.State != MessageProcessingClaimed || claim.Processing.Generation != 1 ||
		claim.Processing.FailureCount != 0 ||
		claim.Processing.ClaimID == "" || claim.Processing.LeaseExpiresAt == nil ||
		claim.Body != "" || len(claim.Payload) != 0 || claim.ReadState.State != MessageReadUnread {
		t.Fatalf("initial message claim = %#v", claim)
	}
	if _, err := st.AckMessage(ctx, recipientPrincipal, msg.ID); !errors.Is(err, ErrMessageBusy) {
		t.Fatalf("ack live claim error = %v, want ErrMessageBusy", err)
	}
	claimedPage, err := st.ListMessages(ctx, recipientPrincipal, MessageFilter{Unacked: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var claimedProjection Message
	for _, candidate := range claimedPage.Messages {
		if candidate.ID == msg.ID {
			claimedProjection = candidate
			break
		}
	}
	if claimedProjection.ID == "" || claimedProjection.Processing.State != MessageProcessingClaimed ||
		claimedProjection.Processing.Generation != 1 || claimedProjection.Processing.FailureCount != 0 ||
		claimedProjection.Processing.ClaimID != "" ||
		claimedProjection.Processing.LeaseExpiresAt != nil {
		t.Fatalf("general claimed projection exposed fence = %#v", claimedProjection)
	}
	sendRetry, err := st.SendMessage(ctx, senderPrincipal, SendMessageInput{
		ToAgent: recipient.ID, Kind: "request", Body: "preserve me",
		Payload: json.RawMessage(`{"task":42}`), IdempotencyKey: "round-trip-1",
	})
	if err != nil || sendRetry.ID != msg.ID || sendRetry.Processing.State != MessageProcessingClaimed ||
		sendRetry.Processing.ClaimID != "" || sendRetry.Processing.LeaseExpiresAt != nil {
		t.Fatalf("general send retry projection = %#v / %v", sendRetry, err)
	}
	claimRetry, err := st.ClaimMessage(ctx, recipientPrincipal, msg.ID, ClaimMessageInput{
		LeaseDuration: 15 * time.Minute, IdempotencyKey: "claim-round-trip-1",
	})
	if err != nil || claimRetry.Processing.ClaimID != claim.Processing.ClaimID ||
		claimRetry.Processing.Generation != claim.Processing.Generation ||
		!claimRetry.Processing.LeaseExpiresAt.Equal(*claim.Processing.LeaseExpiresAt) {
		t.Fatalf("same-key claim retry = %#v / %v", claimRetry, err)
	}
	if _, err := st.ClaimMessage(ctx, recipientPrincipal, msg.ID, ClaimMessageInput{
		IdempotencyKey: "other-active-claim",
	}); !errors.Is(err, ErrMessageBusy) {
		t.Fatalf("other active claim error = %v, want ErrMessageBusy", err)
	}
	if _, err := st.ClaimMessage(ctx, bystanderPrincipal, msg.ID, ClaimMessageInput{
		IdempotencyKey: "bystander-claim",
	}); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("bystander claim error = %v, want ErrMessageNotFound", err)
	}
	if _, err := st.RenewMessageClaim(ctx, recipientPrincipal, msg.ID, RenewMessageClaimInput{
		ClaimID: claim.Processing.ClaimID, ProcessingGeneration: 2,
	}); !errors.Is(err, ErrMessageClaimLost) {
		t.Fatalf("stale-generation renew error = %v, want ErrMessageClaimLost", err)
	}
	renewed, err := st.RenewMessageClaim(ctx, recipientPrincipal, msg.ID, RenewMessageClaimInput{
		ClaimID: claim.Processing.ClaimID, ProcessingGeneration: 1,
		LeaseDuration: 10 * time.Minute,
	})
	if err != nil || renewed.Processing.LeaseExpiresAt == nil ||
		!renewed.Processing.LeaseExpiresAt.After(*claim.Processing.LeaseExpiresAt) {
		t.Fatalf("renewed claim = %#v / %v", renewed, err)
	}
	released, err := st.ReleaseMessageClaim(ctx, recipientPrincipal, msg.ID, ReleaseMessageClaimInput{
		ClaimID: claim.Processing.ClaimID, ProcessingGeneration: 1,
	})
	if err != nil || released.Processing.State != MessageProcessingAvailable ||
		released.Processing.Generation != 1 || released.Processing.FailureCount != 0 || released.Processing.ClaimID != "" {
		t.Fatalf("released claim = %#v / %v", released, err)
	}
	if _, err := st.ReleaseMessageClaim(ctx, recipientPrincipal, msg.ID, ReleaseMessageClaimInput{
		ClaimID: claim.Processing.ClaimID, ProcessingGeneration: 1,
	}); !errors.Is(err, ErrMessageClaimLost) {
		t.Fatalf("released fence retry error = %v, want ErrMessageClaimLost", err)
	}
	secondClaim, err := st.ClaimMessage(ctx, recipientPrincipal, msg.ID, ClaimMessageInput{
		IdempotencyKey: "claim-round-trip-2",
	})
	if err != nil || secondClaim.Processing.Generation != 2 {
		t.Fatalf("second claim = %#v / %v", secondClaim, err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_message_deliveries
		SET lease_expires_at=clock_timestamp()-interval '1 second'
		WHERE message_id=$1`, msg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AckMessage(ctx, recipientPrincipal, msg.ID); !errors.Is(err, ErrMessageBusy) {
		t.Fatalf("ack expired claim error = %v, want ErrMessageBusy", err)
	}
	takeover, err := st.ClaimMessage(ctx, recipientPrincipal, msg.ID, ClaimMessageInput{
		IdempotencyKey: "claim-round-trip-3",
	})
	if err != nil || takeover.Processing.Generation != 3 ||
		takeover.Processing.ClaimID == secondClaim.Processing.ClaimID {
		t.Fatalf("expired takeover = %#v / %v", takeover, err)
	}
	if _, err := st.CompleteMessage(ctx, recipientPrincipal, msg.ID, CompleteMessageInput{
		ClaimID: secondClaim.Processing.ClaimID, ProcessingGeneration: 2,
		IdempotencyKey: "complete-round-trip-stale", Body: "stale result",
	}); !errors.Is(err, ErrMessageClaimLost) {
		t.Fatalf("stale completion error = %v, want ErrMessageClaimLost", err)
	}
	completed, err := st.CompleteMessage(ctx, recipientPrincipal, msg.ID, CompleteMessageInput{
		ClaimID: takeover.Processing.ClaimID, ProcessingGeneration: 3,
		IdempotencyKey: "complete-round-trip-1", Body: "task complete",
		Payload: json.RawMessage(`{"status":"done","count":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Processing.State != MessageProcessingCompleted ||
		completed.Processing.ResultMessageID != completed.ResultMessage.ID ||
		completed.ResultMessage.From.ID != recipient.ID || completed.ResultMessage.To.ID != sender.ID ||
		completed.ResultMessage.ThreadID != msg.ThreadID || completed.ResultMessage.ReplyToMessageID != msg.ID ||
		completed.ResultMessage.CausalDepth != 2 || completed.ResultMessage.Kind != "result" || completed.ResultMessage.Body != "task complete" {
		t.Fatalf("completed result = %#v", completed)
	}
	completionRetry, err := st.CompleteMessage(ctx, recipientPrincipal, msg.ID, CompleteMessageInput{
		ClaimID: takeover.Processing.ClaimID, ProcessingGeneration: 3,
		IdempotencyKey: "complete-round-trip-1", Body: "task complete",
		Payload: json.RawMessage(`{"count":1,"status":"done"}`),
	})
	if err != nil || completionRetry.ResultMessage.ID != completed.ResultMessage.ID {
		t.Fatalf("completion retry = %#v / %v", completionRetry, err)
	}
	for _, tc := range []struct {
		name    string
		subject string
		kind    string
		body    string
		payload json.RawMessage
	}{
		{name: "subject", subject: "changed", body: "task complete", payload: json.RawMessage(`{"status":"done","count":1}`)},
		{name: "kind", kind: "changed", body: "task complete", payload: json.RawMessage(`{"status":"done","count":1}`)},
		{name: "body", body: "changed", payload: json.RawMessage(`{"status":"done","count":1}`)},
		{name: "payload", body: "task complete", payload: json.RawMessage(`{"status":"changed","count":1}`)},
	} {
		t.Run("completion retry changed "+tc.name, func(t *testing.T) {
			_, retryErr := st.CompleteMessage(ctx, recipientPrincipal, msg.ID, CompleteMessageInput{
				ClaimID: takeover.Processing.ClaimID, ProcessingGeneration: 3,
				IdempotencyKey: "complete-round-trip-1", Subject: tc.subject,
				Kind: tc.kind, Body: tc.body, Payload: tc.payload,
			})
			if !errors.Is(retryErr, ErrMessageConflict) {
				t.Fatalf("changed %s retry error = %v, want ErrMessageConflict", tc.name, retryErr)
			}
		})
	}
	if _, err := st.CompleteMessage(ctx, recipientPrincipal, msg.ID, CompleteMessageInput{
		ClaimID: takeover.Processing.ClaimID, ProcessingGeneration: 3,
		IdempotencyKey: "complete-round-trip-other", Body: "task complete",
	}); !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("different completion key error = %v, want ErrMessageConflict", err)
	}
	completedClaim, err := st.ClaimMessage(ctx, recipientPrincipal, msg.ID, ClaimMessageInput{
		IdempotencyKey: "claim-after-completion",
	})
	if err != nil || completedClaim.Processing.State != MessageProcessingCompleted ||
		completedClaim.Processing.ResultMessageID != completed.ResultMessage.ID ||
		completedClaim.Processing.ClaimID != takeover.Processing.ClaimID {
		t.Fatalf("claim completed message = %#v / %v", completedClaim, err)
	}
	var processingEvents, leakedProcessingEvents int
	if err := st.pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE metadata ?| ARRAY[
		         'body','payload','subject','claim_id','claim_key_hash',
		         'complete_key_hash','idempotency_key'])
		FROM account_events
		WHERE account_id=$1 AND verb LIKE 'message.processing.%'
		  AND metadata->>'message_id'=$2`, provisioned.AccountID, msg.ID).
		Scan(&processingEvents, &leakedProcessingEvents); err != nil {
		t.Fatal(err)
	}
	if processingEvents != 6 || leakedProcessingEvents != 0 {
		t.Fatalf("processing audit events/leaks = %d/%d, want 6/0", processingEvents, leakedProcessingEvents)
	}

	concurrentMessage, err := st.SendMessage(ctx, senderPrincipal, SendMessageInput{
		ToAgent: recipient.ID, Kind: "request", Body: "concurrent claim",
		IdempotencyKey: "round-trip-concurrent",
	})
	if err != nil {
		t.Fatal(err)
	}
	type claimOutcome struct {
		message Message
		err     error
	}
	start := make(chan struct{})
	outcomes := make(chan claimOutcome, 2)
	for _, key := range []string{"concurrent-claim-a", "concurrent-claim-b"} {
		go func(key string) {
			<-start
			m, claimErr := st.ClaimMessage(ctx, recipientPrincipal, concurrentMessage.ID, ClaimMessageInput{IdempotencyKey: key})
			outcomes <- claimOutcome{message: m, err: claimErr}
		}(key)
	}
	close(start)
	var concurrentWinner Message
	var successes, busy int
	for range 2 {
		outcome := <-outcomes
		switch {
		case outcome.err == nil:
			successes++
			concurrentWinner = outcome.message
		case errors.Is(outcome.err, ErrMessageBusy):
			busy++
		default:
			t.Fatalf("concurrent claim error = %v", outcome.err)
		}
	}
	if successes != 1 || busy != 1 {
		t.Fatalf("concurrent claims successes/busy = %d/%d", successes, busy)
	}
	if _, err := st.ReleaseMessageClaim(ctx, recipientPrincipal, concurrentMessage.ID, ReleaseMessageClaimInput{
		ClaimID:              concurrentWinner.Processing.ClaimID,
		ProcessingGeneration: concurrentWinner.Processing.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AckMessage(ctx, recipientPrincipal, concurrentMessage.ID); err != nil {
		t.Fatal(err)
	}

	reply, err := st.ReplyMessage(ctx, recipientPrincipal, msg.ID, ReplyMessageInput{
		Body: "I can take it", Payload: json.RawMessage(`{"accepted":true}`),
		IdempotencyKey: "reply-round-trip-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.From.ID != recipient.ID || reply.To.ID != sender.ID || reply.ThreadID != msg.ThreadID ||
		reply.ReplyToMessageID != msg.ID || reply.CausalDepth != 2 || reply.Kind != "reply" {
		t.Fatalf("derived reply routing = %#v, parent = %#v", reply, msg)
	}
	retry, err := st.ReplyMessage(ctx, recipientPrincipal, msg.ID, ReplyMessageInput{
		Body: "I can take it", Payload: json.RawMessage(`{"accepted":true}`),
		IdempotencyKey: "reply-round-trip-1",
	})
	if err != nil || retry.ID != reply.ID {
		t.Fatalf("reply retry = %q / %v, want %q", retry.ID, err, reply.ID)
	}
	if _, err := st.ReplyMessage(ctx, recipientPrincipal, msg.ID, ReplyMessageInput{
		Body: "changed", IdempotencyKey: "reply-round-trip-1",
	}); !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("changed reply retry error = %v, want ErrMessageConflict", err)
	}
	if _, err := st.ReplyMessage(ctx, senderPrincipal, msg.ID, ReplyMessageInput{Body: "forged"}); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("sender reply-to-own-message error = %v, want ErrMessageNotFound", err)
	}
	if _, err := st.ReplyMessage(ctx, bystanderPrincipal, msg.ID, ReplyMessageInput{Body: "forged"}); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("bystander reply error = %v, want ErrMessageNotFound", err)
	}

	listUnacked := func() MessagePage {
		t.Helper()
		page, err := st.ListMessages(ctx, recipientPrincipal, MessageFilter{
			Unacked: true, OldestFirst: true, Limit: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	page := listUnacked()
	if len(page.Messages) != 2 || page.Messages[0].ID != msg.ID || page.Messages[1].ID != second.ID {
		t.Fatalf("oldest unacknowledged messages = %#v", page.Messages)
	}
	if page.Messages[0].ReadState.State != MessageReadUnread || page.Messages[0].Body != "" || len(page.Messages[0].Payload) != 0 {
		t.Fatalf("reply mutated parent or list exposed content: %#v", page.Messages[0])
	}
	if page.Messages[0].Processing.State != MessageProcessingCompleted ||
		page.Messages[0].Processing.ResultMessageID != completed.ResultMessage.ID ||
		page.Messages[0].Processing.ClaimID != "" || page.Messages[0].Processing.LeaseExpiresAt != nil {
		t.Fatalf("list processing state = %#v", page.Messages[0].Processing)
	}
	read, err := st.ReadMessage(ctx, recipientPrincipal, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.ReadState.State != MessageReadRead || read.Body != "preserve me" ||
		read.Processing.ClaimID != "" || read.Processing.LeaseExpiresAt != nil {
		t.Fatalf("read message = %#v", read)
	}
	page = listUnacked()
	if len(page.Messages) != 2 || page.Messages[0].ID != msg.ID || page.Messages[0].ReadState.State != MessageReadRead {
		t.Fatalf("read but unacknowledged mailbox = %#v", page.Messages)
	}
	if acked, err := st.AckMessage(ctx, recipientPrincipal, msg.ID); err != nil || acked.ReadState.State != MessageReadAcked ||
		acked.Body != "" || len(acked.Payload) != 0 || acked.Processing.ClaimID != "" ||
		acked.Processing.LeaseExpiresAt != nil {
		t.Fatalf("ack parent = %#v / %v", acked, err)
	}
	page = listUnacked()
	if len(page.Messages) != 1 || page.Messages[0].ID != second.ID {
		t.Fatalf("mailbox after parent ack = %#v", page.Messages)
	}
	failureClaim, err := st.ClaimMessage(ctx, recipientPrincipal, second.ID, ClaimMessageInput{
		IdempotencyKey: "deterministic-failure-claim",
	})
	if err != nil || failureClaim.Processing.Generation != 1 || failureClaim.Processing.FailureCount != 0 {
		t.Fatalf("deterministic failure claim = %#v / %v", failureClaim, err)
	}
	failureRelease, err := st.ReleaseMessageClaim(ctx, recipientPrincipal, second.ID, ReleaseMessageClaimInput{
		ClaimID: failureClaim.Processing.ClaimID, ProcessingGeneration: failureClaim.Processing.Generation,
		DeterministicFailure: true,
	})
	if err != nil || failureRelease.Processing.Generation != 1 || failureRelease.Processing.FailureCount != 1 {
		t.Fatalf("deterministic failure release = %#v / %v", failureRelease, err)
	}
	if _, err := st.ReleaseMessageClaim(ctx, recipientPrincipal, second.ID, ReleaseMessageClaimInput{
		ClaimID: failureClaim.Processing.ClaimID, ProcessingGeneration: failureClaim.Processing.Generation,
		DeterministicFailure: true,
	}); !errors.Is(err, ErrMessageClaimLost) {
		t.Fatalf("deterministic release retry error = %v, want ErrMessageClaimLost", err)
	}
	ordinaryClaim, err := st.ClaimMessage(ctx, recipientPrincipal, second.ID, ClaimMessageInput{
		IdempotencyKey: "ordinary-release-claim",
	})
	if err != nil || ordinaryClaim.Processing.Generation != 2 || ordinaryClaim.Processing.FailureCount != 1 {
		t.Fatalf("ordinary release claim = %#v / %v", ordinaryClaim, err)
	}
	ordinaryRelease, err := st.ReleaseMessageClaim(ctx, recipientPrincipal, second.ID, ReleaseMessageClaimInput{
		ClaimID: ordinaryClaim.Processing.ClaimID, ProcessingGeneration: ordinaryClaim.Processing.Generation,
	})
	if err != nil || ordinaryRelease.Processing.Generation != 2 || ordinaryRelease.Processing.FailureCount != 1 {
		t.Fatalf("ordinary release = %#v / %v", ordinaryRelease, err)
	}
	activeImportedClaim, err := st.ClaimMessage(ctx, recipientPrincipal, second.ID, ClaimMessageInput{
		IdempotencyKey: "active-at-export",
	})
	if err != nil || activeImportedClaim.Processing.Generation != 3 || activeImportedClaim.Processing.FailureCount != 1 {
		t.Fatalf("active export claim = %#v / %v", activeImportedClaim, err)
	}

	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID, "evacuation", "archive round trip"); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, provisioned.AccountID, "test-source", "test", &archive); err != nil {
		t.Fatal(err)
	}
	if err := deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportAccount(ctx, provisioned.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}

	var body string
	var payload json.RawMessage
	var readAt, ackedAt, completedAt *time.Time
	var processingState, resultMessageID string
	var processingGeneration, failureCount int64
	if err := st.pool.QueryRow(ctx, `
		SELECT m.body, m.payload, d.read_at, d.acked_at,
		       d.processing_state,d.processing_generation,d.failure_count,d.completed_at,d.result_message_id
		FROM agent_messages m
		JOIN agent_message_deliveries d ON d.message_id = m.id
		WHERE m.id = $1`, msg.ID).Scan(&body, &payload, &readAt, &ackedAt,
		&processingState, &processingGeneration, &failureCount, &completedAt, &resultMessageID); err != nil {
		t.Fatal(err)
	}
	if body != "preserve me" || !rawJSONEqual(payload, json.RawMessage(`{"task":42}`)) || readAt == nil || ackedAt == nil ||
		processingState != MessageProcessingCompleted || processingGeneration != 3 || failureCount != 0 || completedAt == nil ||
		resultMessageID != completed.ResultMessage.ID {
		t.Fatalf("restored message = body:%q payload:%s read:%v acked:%v processing:%s/%d/%v/%s",
			body, payload, readAt, ackedAt, processingState, processingGeneration, completedAt, resultMessageID)
	}
	var importedState, importedClaimHash string
	var importedGeneration, importedFailureCount int64
	var importedClaimID, importedLease any
	if err := st.pool.QueryRow(ctx, `
		SELECT processing_state,processing_generation,failure_count,claim_id,claim_key_hash,lease_expires_at
		FROM agent_message_deliveries WHERE message_id=$1`, second.ID).
		Scan(&importedState, &importedGeneration, &importedFailureCount, &importedClaimID, &importedClaimHash, &importedLease); err != nil {
		t.Fatal(err)
	}
	if importedState != MessageProcessingAvailable || importedGeneration != 4 || importedFailureCount != 1 ||
		importedClaimID != nil || importedClaimHash != "" || importedLease != nil {
		t.Fatalf("imported active claim = %q/%d/%v/%q/%v", importedState, importedGeneration,
			importedClaimID, importedClaimHash, importedLease)
	}
	var replyBody, replyThread, replyParent, replyFrom, replyTo string
	var replyDepth int64
	var replyPayload json.RawMessage
	if err := st.pool.QueryRow(ctx, `
		SELECT body, payload, thread_id, reply_to_message_id, from_agent_id, to_agent_id, causal_depth
		FROM agent_messages WHERE id=$1`, reply.ID).
		Scan(&replyBody, &replyPayload, &replyThread, &replyParent, &replyFrom, &replyTo, &replyDepth); err != nil {
		t.Fatal(err)
	}
	if replyBody != "I can take it" || !rawJSONEqual(replyPayload, json.RawMessage(`{"accepted":true}`)) ||
		replyThread != msg.ThreadID || replyParent != msg.ID || replyFrom != recipient.ID || replyTo != sender.ID || replyDepth != 2 {
		t.Fatalf("restored reply = body:%q payload:%s thread:%q parent:%q route:%s->%s",
			replyBody, replyPayload, replyThread, replyParent, replyFrom, replyTo)
	}
}

func TestMessageCompletionAfterSenderDeletionPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx, "message-deleted-sender-test@witwave.ai", "deleted sender test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	deletedSender, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "deleted-sender")
	if err != nil {
		t.Fatal(err)
	}
	liveSender, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "live-sender")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	principal := func(agent Agent) Principal {
		return Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
		}
	}
	deletedSenderPrincipal := principal(deletedSender)
	liveSenderPrincipal := principal(liveSender)
	workerPrincipal := principal(worker)

	parent, err := st.SendMessage(ctx, deletedSenderPrincipal, SendMessageInput{
		ToAgent: worker.ID, Kind: "request", Body: "finish after I am deleted",
		IdempotencyKey: "deleted-sender-parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAgent(ctx, provisioned.AccountID, realm.ID, deletedSender.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ListMessages(ctx, deletedSenderPrincipal, MessageFilter{Limit: 10}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("deleted agent stale-principal list error = %v, want ErrAgentNotFound", err)
	}
	idNameCollision, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, deletedSender.ID)
	if err != nil {
		t.Fatal(err)
	}
	idNameCollisionPrincipal := principal(idNameCollision)
	if _, err := st.SendMessage(ctx, liveSenderPrincipal, SendMessageInput{
		ToAgent: deletedSender.ID, Kind: "request", Body: "must not route",
	}); !errors.Is(err, ErrMessageRecipientMissing) {
		t.Fatalf("direct send to deleted agent error = %v, want ErrMessageRecipientMissing", err)
	}
	var misrouted int
	if err := st.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM agent_messages
		WHERE account_id=$1 AND from_agent_id=$2 AND to_agent_id=$3`,
		provisioned.AccountID, liveSender.ID, idNameCollision.ID).Scan(&misrouted); err != nil {
		t.Fatal(err)
	}
	if misrouted != 0 {
		t.Fatalf("stale deleted agent id misrouted %d message(s) by name", misrouted)
	}
	if addressed, err := st.SendMessage(ctx, liveSenderPrincipal, SendMessageInput{
		ToAgent: idNameCollision.ID, Kind: "note", Body: "exact live id still works",
	}); err != nil || addressed.To.ID != idNameCollision.ID {
		t.Fatalf("exact collision-agent id send = %#v / %v", addressed, err)
	}
	collisionMessage, err := st.SendMessage(ctx, idNameCollisionPrincipal, SendMessageInput{
		ToAgent: worker.ID, Kind: "note", Body: "must not match stale sender id",
		IdempotencyKey: "sender-filter-collision",
	})
	if err != nil {
		t.Fatal(err)
	}
	staleIDPage, err := st.ListMessages(ctx, workerPrincipal, MessageFilter{
		From: deletedSender.ID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(staleIDPage.Messages) != 1 || staleIDPage.Messages[0].ID != parent.ID ||
		staleIDPage.Messages[0].From.ID != deletedSender.ID {
		t.Fatalf("exact stale sender id filter mixed name collision: %#v", staleIDPage.Messages)
	}
	collisionIDPage, err := st.ListMessages(ctx, workerPrincipal, MessageFilter{
		From: idNameCollision.ID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(collisionIDPage.Messages) != 1 || collisionIDPage.Messages[0].ID != collisionMessage.ID ||
		collisionIDPage.Messages[0].From.ID != idNameCollision.ID {
		t.Fatalf("exact collision sender id filter = %#v", collisionIDPage.Messages)
	}
	if _, err := st.AckMessage(ctx, workerPrincipal, collisionMessage.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplyMessage(ctx, workerPrincipal, parent.ID, ReplyMessageInput{
		Body: "ordinary reply must remain live-only",
	}); !errors.Is(err, ErrMessageRecipientMissing) {
		t.Fatalf("ordinary reply to deleted sender error = %v, want ErrMessageRecipientMissing", err)
	}

	claim, err := st.ClaimMessage(ctx, workerPrincipal, parent.ID, ClaimMessageInput{
		IdempotencyKey: "deleted-sender-claim",
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := st.CompleteMessage(ctx, workerPrincipal, parent.ID, CompleteMessageInput{
		ClaimID: claim.Processing.ClaimID, ProcessingGeneration: claim.Processing.Generation,
		IdempotencyKey: "deleted-sender-complete", Kind: "result", Body: "terminal result",
		Payload: json.RawMessage(`{"status":"terminal"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Processing.State != MessageProcessingCompleted ||
		completed.Processing.ResultMessageID != completed.ResultMessage.ID ||
		completed.ResultMessage.To.ID != deletedSender.ID ||
		completed.ResultMessage.Delivery.State != MessageDeliveryFailed ||
		completed.ResultMessage.Delivery.DeliveredAt != nil ||
		completed.ResultMessage.ReplyToMessageID != parent.ID ||
		completed.ResultMessage.ThreadID != parent.ThreadID || completed.ResultMessage.CausalDepth != 2 {
		t.Fatalf("deleted-sender completion = %#v", completed)
	}
	retry, err := st.CompleteMessage(ctx, workerPrincipal, parent.ID, CompleteMessageInput{
		ClaimID: claim.Processing.ClaimID, ProcessingGeneration: claim.Processing.Generation,
		IdempotencyKey: "deleted-sender-complete", Kind: "result", Body: "terminal result",
		Payload: json.RawMessage(`{"status":"terminal"}`),
	})
	if err != nil || retry.ResultMessage.ID != completed.ResultMessage.ID ||
		retry.ResultMessage.Delivery.State != MessageDeliveryFailed || retry.ResultMessage.Delivery.DeliveredAt != nil {
		t.Fatalf("deleted-sender completion retry = %#v / %v", retry, err)
	}

	var failedEvents, deliveredEvents, leakedEvents int
	if err := st.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE verb=$2),
		       COUNT(*) FILTER (WHERE verb=$3),
		       COUNT(*) FILTER (WHERE metadata ?| ARRAY['body','payload','subject'])
		FROM account_events
		WHERE account_id=$1 AND metadata->>'message_id'=$4`,
		provisioned.AccountID, VerbMessageDeliveryFailed, VerbMessageDelivered,
		completed.ResultMessage.ID).Scan(&failedEvents, &deliveredEvents, &leakedEvents); err != nil {
		t.Fatal(err)
	}
	if failedEvents != 1 || deliveredEvents != 0 || leakedEvents != 0 {
		t.Fatalf("failed delivery audit = failed:%d delivered:%d leaked:%d", failedEvents, deliveredEvents, leakedEvents)
	}
	if _, err := st.AckMessage(ctx, workerPrincipal, parent.ID); err != nil {
		t.Fatal(err)
	}

	next, err := st.SendMessage(ctx, liveSenderPrincipal, SendMessageInput{
		ToAgent: worker.ID, Kind: "request", Body: "next mailbox item",
		IdempotencyKey: "post-deletion-progress",
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := st.ListMessages(ctx, workerPrincipal, MessageFilter{
		Unacked: true, OldestFirst: true, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].ID != next.ID {
		t.Fatalf("mailbox did not advance after terminal failed delivery: %#v", page.Messages)
	}
	nextClaim, err := st.ClaimMessage(ctx, workerPrincipal, next.ID, ClaimMessageInput{
		IdempotencyKey: "post-deletion-claim",
	})
	if err != nil {
		t.Fatal(err)
	}
	nextCompleted, err := st.CompleteMessage(ctx, workerPrincipal, next.ID, CompleteMessageInput{
		ClaimID: nextClaim.Processing.ClaimID, ProcessingGeneration: nextClaim.Processing.Generation,
		IdempotencyKey: "post-deletion-complete", Body: "progressed",
	})
	if err != nil || nextCompleted.ResultMessage.Delivery.State != MessageDeliveryDelivered ||
		nextCompleted.ResultMessage.Delivery.DeliveredAt == nil {
		t.Fatalf("subsequent completion = %#v / %v", nextCompleted, err)
	}
	if _, err := st.AckMessage(ctx, workerPrincipal, next.ID); err != nil {
		t.Fatal(err)
	}

	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID, "evacuation", "deleted sender archive round trip"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ListMessages(ctx, workerPrincipal, MessageFilter{Limit: 10}); !errors.Is(err, ErrAccountNotActive) {
		t.Fatalf("suspended account stale-principal list error = %v, want ErrAccountNotActive", err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, provisioned.AccountID, "test-source", "test", &archive); err != nil {
		t.Fatal(err)
	}
	if err := deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportAccount(ctx, provisioned.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	var restoredDelivery, restoredProcessing, restoredResultID string
	var restoredDeliveredAt *time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT result.state, result.delivered_at,
		       parent.processing_state, parent.result_message_id
		FROM agent_message_deliveries result
		JOIN agent_message_deliveries parent ON parent.message_id=$2
		WHERE result.message_id=$1`, completed.ResultMessage.ID, parent.ID).
		Scan(&restoredDelivery, &restoredDeliveredAt, &restoredProcessing, &restoredResultID); err != nil {
		t.Fatal(err)
	}
	if restoredDelivery != MessageDeliveryFailed || restoredDeliveredAt != nil ||
		restoredProcessing != MessageProcessingCompleted || restoredResultID != completed.ResultMessage.ID {
		t.Fatalf("restored terminal delivery = %q/%v parent:%q/%q",
			restoredDelivery, restoredDeliveredAt, restoredProcessing, restoredResultID)
	}
}

func TestMessageIdempotentReplaySurvivesRecipientDeletionPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx, "message-replay-tombstone-test@witwave.ai", "message replay tombstone test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	createAgent := func(name string) Agent {
		t.Helper()
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		return agent
	}
	principal := func(agent Agent) Principal {
		return Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
		}
	}

	alice := createAgent("alice")
	directTarget := createAgent("bob")
	replyOrigin := createAgent("carol")
	alicePrincipal := principal(alice)

	directInput := SendMessageInput{
		ToAgent: directTarget.Name, Kind: "request", Body: "durable direct request",
		IdempotencyKey: "tombstone-direct-replay",
	}
	direct, err := st.SendMessage(ctx, alicePrincipal, directInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAgent(ctx, provisioned.AccountID, realm.ID, directTarget.ID); err != nil {
		t.Fatal(err)
	}
	directReplay, err := st.SendMessage(ctx, alicePrincipal, directInput)
	if err != nil {
		t.Fatalf("replay direct message after recipient deletion: %v", err)
	}
	if directReplay.ID != direct.ID {
		t.Fatalf("direct replay id = %q, want %q", directReplay.ID, direct.ID)
	}
	directConflict := directInput
	directConflict.Body = "different request"
	if _, err := st.SendMessage(ctx, alicePrincipal, directConflict); !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("conflicting direct replay error = %v, want ErrMessageConflict", err)
	}
	newDirect := directInput
	newDirect.IdempotencyKey = "tombstone-direct-new"
	if _, err := st.SendMessage(ctx, alicePrincipal, newDirect); !errors.Is(err, ErrMessageRecipientMissing) {
		t.Fatalf("new send to tombstone error = %v, want ErrMessageRecipientMissing", err)
	}

	parent, err := st.SendMessage(ctx, principal(replyOrigin), SendMessageInput{
		ToAgent: alice.ID, Kind: "request", Body: "please answer",
		IdempotencyKey: "tombstone-reply-parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	replyInput := ReplyMessageInput{
		Kind: "reply", Body: "durable reply", IdempotencyKey: "tombstone-reply-replay",
	}
	reply, err := st.ReplyMessage(ctx, alicePrincipal, parent.ID, replyInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAgent(ctx, provisioned.AccountID, realm.ID, replyOrigin.ID); err != nil {
		t.Fatal(err)
	}
	replyReplay, err := st.ReplyMessage(ctx, alicePrincipal, parent.ID, replyInput)
	if err != nil {
		t.Fatalf("replay reply after recipient deletion: %v", err)
	}
	if replyReplay.ID != reply.ID {
		t.Fatalf("reply replay id = %q, want %q", replyReplay.ID, reply.ID)
	}
	replyConflict := replyInput
	replyConflict.Body = "different reply"
	if _, err := st.ReplyMessage(ctx, alicePrincipal, parent.ID, replyConflict); !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("conflicting reply replay error = %v, want ErrMessageConflict", err)
	}
	newReply := replyInput
	newReply.IdempotencyKey = "tombstone-reply-new"
	if _, err := st.ReplyMessage(ctx, alicePrincipal, parent.ID, newReply); !errors.Is(err, ErrMessageRecipientMissing) {
		t.Fatalf("new reply to tombstone error = %v, want ErrMessageRecipientMissing", err)
	}
}

func TestMessageDeletionAndSuspensionRacesAreFencedPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx, "message-revocation-race-test@witwave.ai", "message revocation race test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	createAgent := func(name string) Agent {
		t.Helper()
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		return agent
	}
	sender := createAgent("sender")
	recipient := createAgent("recipient")
	worker := createAgent("worker")
	listener := createAgent("listener")
	principal := func(agent Agent) Principal {
		return Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
		}
	}
	senderPrincipal := principal(sender)
	workerPrincipal := principal(worker)
	listenerPrincipal := principal(listener)
	workMessage, err := st.SendMessage(ctx, senderPrincipal, SendMessageInput{
		ToAgent: worker.ID, Kind: "request", Body: "must remain unacknowledged",
		IdempotencyKey: "revocation-race-work",
	})
	if err != nil {
		t.Fatal(err)
	}
	listenerMessage, err := st.SendMessage(ctx, senderPrincipal, SendMessageInput{
		ToAgent: listener.ID, Kind: "note", Body: "metadata must be revoked",
		IdempotencyKey: "revocation-race-listener",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Hold an uncommitted recipient tombstone, then start a direct send that is
	// provably blocked by that exact backend. After the tombstone commits, the
	// live-recipient SELECT must recheck it rather than insert a delivery.
	blocker, blockerPID := tombstoneAgentForMessageRaceTest(ctx, t, st, recipient.ID)
	defer func(tx pgx.Tx) { _ = tx.Rollback(context.Background()) }(blocker)
	type sendOutcome struct {
		message Message
		err     error
	}
	sendDone := make(chan sendOutcome, 1)
	go func() {
		message, err := st.SendMessage(ctx, senderPrincipal, SendMessageInput{
			ToAgent: recipient.ID, Kind: "request", Body: "must not deliver to tombstone",
			IdempotencyKey: "revocation-race-send",
		})
		sendDone <- sendOutcome{message: message, err: err}
	}()
	waitForPostgresBlockWaiters(ctx, t, st, blockerPID, 1)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var sent sendOutcome
	select {
	case sent = <-sendDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !errors.Is(sent.err, ErrMessageRecipientMissing) || sent.message.ID != "" {
		t.Fatalf("send after queued recipient deletion = %#v / %v", sent.message, sent.err)
	}
	var misdelivered int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_messages
		WHERE account_id=$1 AND from_agent_id=$2 AND to_agent_id=$3
		  AND body='must not deliver to tombstone'`,
		provisioned.AccountID, sender.ID, recipient.ID).Scan(&misdelivered); err != nil {
		t.Fatal(err)
	}
	if misdelivered != 0 {
		t.Fatalf("queued delete/send race inserted %d tombstone delivery", misdelivered)
	}

	// The same committed-first ordering must fence a recipient mutation. The
	// stale principal cannot acknowledge, and the durable delivery is unchanged.
	blocker, blockerPID = tombstoneAgentForMessageRaceTest(ctx, t, st, worker.ID)
	defer func(tx pgx.Tx) { _ = tx.Rollback(context.Background()) }(blocker)
	ackDone := make(chan error, 1)
	go func() {
		_, err := st.AckMessage(ctx, workerPrincipal, workMessage.ID)
		ackDone <- err
	}()
	waitForPostgresBlockWaiters(ctx, t, st, blockerPID, 1)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := receiveMessageRaceError(ctx, t, ackDone); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("ack after queued agent deletion error = %v, want ErrAgentNotFound", err)
	}
	var ackedAt *time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT acked_at FROM agent_message_deliveries
		WHERE message_id=$1 AND recipient_agent_id=$2`, workMessage.ID, worker.ID).Scan(&ackedAt); err != nil {
		t.Fatal(err)
	}
	if ackedAt != nil {
		t.Fatalf("queued delete/ack race advanced acked_at to %v", ackedAt)
	}

	// A long-poll iteration uses ListMessages. Commit its caller tombstone first
	// and prove the stale authenticated principal receives no metadata or success.
	blocker, blockerPID = tombstoneAgentForMessageRaceTest(ctx, t, st, listener.ID)
	defer func(tx pgx.Tx) { _ = tx.Rollback(context.Background()) }(blocker)
	type listOutcome struct {
		page MessagePage
		err  error
	}
	listDone := make(chan listOutcome, 1)
	go func() {
		page, err := st.ListMessages(ctx, listenerPrincipal, MessageFilter{Unacked: true, Limit: 10})
		listDone <- listOutcome{page: page, err: err}
	}()
	waitForPostgresBlockWaiters(ctx, t, st, blockerPID, 1)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var listed listOutcome
	select {
	case listed = <-listDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !errors.Is(listed.err, ErrAgentNotFound) || len(listed.page.Messages) != 0 {
		t.Fatalf("list after queued agent deletion = %#v / %v", listed.page, listed.err)
	}
	if listenerMessage.ID == "" {
		t.Fatal("listener fixture message is empty")
	}

	// Hold an uncommitted account suspension and prove the mailbox snapshot is
	// blocked by that exact backend. Once committed, the stale principal must
	// observe ErrAccountNotActive instead of metadata.
	accountBlocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = accountBlocker.Rollback(context.Background()) }()
	accountBlockerPID := postgresBackendPIDForMessageRaceTest(ctx, t, accountBlocker)
	tag, err := accountBlocker.Exec(ctx, `
		UPDATE accounts
		SET status='suspended', suspended_at=clock_timestamp(),
		    suspended_for='evacuation', suspended_reason='message list revocation race'
		WHERE id=$1 AND status='active'`, provisioned.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("suspend account rows = %d, want 1", tag.RowsAffected())
	}
	listDone = make(chan listOutcome, 1)
	go func() {
		page, err := st.ListMessages(ctx, senderPrincipal, MessageFilter{Direction: MessageDirectionOutbox, Limit: 10})
		listDone <- listOutcome{page: page, err: err}
	}()
	waitForPostgresBlockWaiters(ctx, t, st, accountBlockerPID, 1)
	if err := accountBlocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case listed = <-listDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !errors.Is(listed.err, ErrAccountNotActive) || len(listed.page.Messages) != 0 {
		t.Fatalf("list after queued account suspension = %#v / %v", listed.page, listed.err)
	}
}

func tombstoneAgentForMessageRaceTest(ctx context.Context, t *testing.T, st *Store, agentID string) (pgx.Tx, int) {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backendPID := postgresBackendPIDForMessageRaceTest(ctx, t, tx)
	tag, err := tx.Exec(ctx, `
		UPDATE agents SET deleted_at=clock_timestamp(), updated_at=clock_timestamp()
		WHERE id=$1 AND deleted_at IS NULL`, agentID)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		_ = tx.Rollback(ctx)
		t.Fatalf("tombstone agent rows = %d, want 1", tag.RowsAffected())
	}
	return tx, backendPID
}

func postgresBackendPIDForMessageRaceTest(ctx context.Context, t *testing.T, tx pgx.Tx) int {
	t.Helper()
	var backendPID int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	return backendPID
}

func waitForPostgresBlockWaiters(ctx context.Context, t *testing.T, st *Store, blockerPID, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var count int
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity a
			WHERE a.datname=current_database() AND a.pid<>pg_backend_pid()
			  AND $1::int = ANY(pg_blocking_pids(a.pid))`, blockerPID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Postgres waiters blocked by pid %d = %d, want at least %d", blockerPID, count, want)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func receiveMessageRaceError(ctx context.Context, t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return ctx.Err()
	}
}

func deleteAccountForIntegrationTest(ctx context.Context, st *Store, accountID string) error {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	statements := []string{
		`DELETE FROM agent_message_deliveries WHERE account_id = $1`,
		`DELETE FROM agent_messages WHERE account_id = $1`,
		`DELETE FROM usage_rollups WHERE account_id = $1`,
		`DELETE FROM usage_events WHERE account_id = $1`,
		`DELETE FROM transcript_entries WHERE account_id = $1`,
		`DELETE FROM transcript_conversations WHERE account_id = $1`,
		`DELETE FROM support_ticket_messages WHERE account_id = $1`,
		`DELETE FROM support_tickets WHERE account_id = $1`,
		`DELETE FROM account_events WHERE account_id = $1`,
		`DELETE FROM tokens WHERE account_id = $1`,
		`DELETE FROM agents WHERE realm_id IN (SELECT id FROM realms WHERE account_id = $1)`,
		`DELETE FROM realms WHERE account_id = $1`,
		`DELETE FROM operators WHERE account_id = $1`,
		`DELETE FROM accounts WHERE id = $1`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, accountID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
