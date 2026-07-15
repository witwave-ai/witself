package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	archiveexport "github.com/witwave-ai/witself/internal/export"
)

type messageRequestHardeningFixture struct {
	ctx        context.Context
	st         *Store
	accountID  string
	realm      Realm
	agents     map[string]Agent
	principals map[string]Principal
}

func newMessageRequestHardeningFixture(t *testing.T, names ...string) *messageRequestHardeningFixture {
	t.Helper()
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		fmt.Sprintf("message-request-hardening-%d@witwave.ai", time.Now().UnixNano()),
		"message request hardening", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID)
	})
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &messageRequestHardeningFixture{
		ctx: ctx, st: st, accountID: provisioned.AccountID, realm: realm,
		agents: map[string]Agent{}, principals: map[string]Principal{},
	}
	for _, name := range names {
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		fixture.agents[name] = agent
		fixture.principals[name] = Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
		}
	}
	return fixture
}

func (f *messageRequestHardeningFixture) open(
	t *testing.T,
	coordinator string,
	key string,
	maxAssignees int,
) OpenMessageRequestResult {
	t.Helper()
	opened, err := f.st.OpenMessageRequest(f.ctx, f.principals[coordinator], OpenMessageRequestInput{
		Body: "Perform the bounded request.", MaxAssignees: maxAssignees,
		OfferWindow: time.Minute, ExpiresIn: time.Hour, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

func (f *messageRequestHardeningFixture) offer(
	t *testing.T,
	opened OpenMessageRequestResult,
	agent string,
	key string,
) {
	t.Helper()
	if _, err := f.st.OfferMessageRequest(f.ctx, f.principals[agent], opened.Request.ID, OfferMessageRequestInput{
		Body: "I can do this work.", IdempotencyKey: key,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *messageRequestHardeningFixture) selectAgents(
	t *testing.T,
	opened OpenMessageRequestResult,
	coordinator string,
	key string,
	agents ...string,
) []MessageRequestClaim {
	t.Helper()
	ids := make([]string, len(agents))
	for i, name := range agents {
		ids[i] = f.agents[name].ID
	}
	selected, err := f.st.SelectMessageRequest(f.ctx, f.principals[coordinator], opened.Request.ID, SelectMessageRequestInput{
		SelectedAgentIDs: ids, Reservation: 2 * time.Minute, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return selected.Claims
}

func (f *messageRequestHardeningFixture) claim(
	t *testing.T,
	opened OpenMessageRequestResult,
	agent string,
	key string,
) MessageRequestClaim {
	t.Helper()
	claim, err := f.st.ClaimMessageRequest(f.ctx, f.principals[agent], opened.Request.ID, ClaimMessageRequestInput{
		LeaseDuration: 2 * time.Minute, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func (f *messageRequestHardeningFixture) closeOfferWindow(t *testing.T, opened OpenMessageRequestResult) {
	t.Helper()
	if _, err := f.st.pool.Exec(f.ctx, `
		UPDATE agent_message_requests
		SET created_at=clock_timestamp()-interval '2 seconds',
		    offer_deadline=clock_timestamp()-interval '1 second'
		WHERE id=$1`, opened.Request.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMessageRequestConcurrentOpenReplaysWinningRequestPostgres(t *testing.T) {
	f := newMessageRequestHardeningFixture(t, "scott", "bob")
	input := OpenMessageRequestInput{
		Body: "Open this request exactly once.", MaxAssignees: 1,
		OfferWindow: time.Minute, ExpiresIn: time.Hour,
		IdempotencyKey: "concurrent-open-one-key",
	}
	const workers = 8
	start := make(chan struct{})
	type outcome struct {
		result OpenMessageRequestResult
		err    error
	}
	outcomes := make(chan outcome, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := f.st.OpenMessageRequest(f.ctx, f.principals["scott"], input)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(outcomes)
	var requestID, openingID string
	for outcome := range outcomes {
		if outcome.err != nil {
			t.Fatalf("concurrent open: %v", outcome.err)
		}
		if requestID == "" {
			requestID, openingID = outcome.result.Request.ID, outcome.result.OpeningMessage.ID
		}
		if outcome.result.Request.ID != requestID || outcome.result.OpeningMessage.ID != openingID {
			t.Fatalf("concurrent replay diverged: %#v", outcome.result)
		}
	}
	var requestCount, messageCount, eventCount int
	if err := f.st.pool.QueryRow(f.ctx, `
		SELECT
		  (SELECT count(*) FROM agent_message_requests WHERE account_id=$1),
		  (SELECT count(*) FROM agent_messages WHERE account_id=$1 AND kind='open_request'),
		  (SELECT count(*) FROM account_events WHERE account_id=$1 AND verb=$2)`,
		f.accountID, VerbMessageRequestOpened).Scan(&requestCount, &messageCount, &eventCount); err != nil {
		t.Fatal(err)
	}
	if requestCount != 1 || messageCount != 1 || eventCount != 1 {
		t.Fatalf("concurrent open counts = request %d message %d event %d", requestCount, messageCount, eventCount)
	}
}

func TestMessageRequestDurableExpiryAndAtMostCapacityPostgres(t *testing.T) {
	f := newMessageRequestHardeningFixture(t, "scott", "bob", "charlie")

	t.Run("lazy expiry is durable and exact once", func(t *testing.T) {
		opened := f.open(t, "scott", "durable-expiry-open", 1)
		f.offer(t, opened, "bob", "durable-expiry-offer")
		f.closeOfferWindow(t, opened)
		f.selectAgents(t, opened, "scott", "durable-expiry-select", "bob")
		claim := f.claim(t, opened, "bob", "durable-expiry-claim")
		if _, err := f.st.pool.Exec(f.ctx, `
			UPDATE agent_message_requests
			SET created_at=clock_timestamp()-interval '3 seconds',
			    offer_deadline=clock_timestamp()-interval '2 seconds',
			    expires_at=clock_timestamp()-interval '1 second',
			    updated_at=clock_timestamp()-interval '1 second'
			WHERE id=$1`, opened.Request.ID); err != nil {
			t.Fatal(err)
		}
		detail, err := f.st.GetMessageRequest(f.ctx, f.principals["scott"], opened.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Request.State != MessageRequestStateExpired || detail.Request.ExpiredAt == nil ||
			len(detail.Claims) != 1 || detail.Claims[0].State != MessageRequestClaimCancelled {
			t.Fatalf("expired detail = %#v", detail)
		}
		page, err := f.st.ListMessageRequests(f.ctx, f.principals["scott"], MessageRequestFilter{
			State: MessageRequestStateExpired, Limit: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, request := range page.Requests {
			found = found || request.ID == opened.Request.ID
		}
		if !found {
			t.Fatalf("expired request absent from page: %#v", page)
		}
		var requestState, claimState string
		var expiredAt *time.Time
		var eventCount int
		if err := f.st.pool.QueryRow(f.ctx, `
			SELECT r.state,r.expired_at,c.state,
			  (SELECT count(*) FROM account_events e
			   WHERE e.account_id=r.account_id AND e.verb=$3
			     AND e.metadata->>'request_id'=r.id)
			FROM agent_message_requests r
			JOIN agent_message_request_claims c ON c.id=$2
			WHERE r.id=$1`, opened.Request.ID, claim.ClaimID, VerbMessageRequestExpired).
			Scan(&requestState, &expiredAt, &claimState, &eventCount); err != nil {
			t.Fatal(err)
		}
		if requestState != MessageRequestStateExpired || expiredAt == nil ||
			claimState != MessageRequestClaimCancelled || eventCount != 1 {
			t.Fatalf("durable expiry = %s/%v claim=%s events=%d", requestState, expiredAt, claimState, eventCount)
		}
	})

	t.Run("one selected result can complete below capacity", func(t *testing.T) {
		opened := f.open(t, "scott", "at-most-one-open", 2)
		f.offer(t, opened, "bob", "at-most-one-offer")
		f.closeOfferWindow(t, opened)
		f.selectAgents(t, opened, "scott", "at-most-one-select", "bob")
		claim := f.claim(t, opened, "bob", "at-most-one-claim")
		completed, err := f.st.CompleteMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, CompleteMessageRequestInput{
			ClaimID: claim.ClaimID, Generation: claim.Generation,
			Body: "One selected result is complete.", IdempotencyKey: "at-most-one-complete",
		})
		if err != nil {
			t.Fatal(err)
		}
		if completed.Request.State != MessageRequestStateCompleted || completed.Request.CompletedClaimCount != 1 {
			t.Fatalf("single selected completion = %#v", completed)
		}
	})

	t.Run("two live selections wait for both", func(t *testing.T) {
		opened := f.open(t, "scott", "at-most-two-open", 2)
		f.offer(t, opened, "bob", "at-most-two-bob-offer")
		if _, err := f.st.SelectMessageRequest(f.ctx, f.principals["scott"], opened.Request.ID, SelectMessageRequestInput{
			SelectedAgentIDs: []string{f.agents["bob"].ID}, Reservation: time.Minute,
			IdempotencyKey: "at-most-two-early-select",
		}); !errors.Is(err, ErrMessageRequestConflict) {
			t.Fatalf("early selection error = %v", err)
		}
		f.offer(t, opened, "charlie", "at-most-two-charlie-offer")
		f.selectAgents(t, opened, "scott", "at-most-two-select", "bob", "charlie")
		bobDetail, err := f.st.GetMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(bobDetail.Request.SelectedAgentIDs) != 1 || bobDetail.Request.SelectedAgentIDs[0] != f.agents["bob"].ID ||
			len(bobDetail.Selections) != 1 || len(bobDetail.Selections[0].SelectedAgentIDs) != 1 ||
			bobDetail.Selections[0].SelectedAgentIDs[0] != f.agents["bob"].ID ||
			len(bobDetail.Claims) != 1 || bobDetail.Claims[0].Agent.ID != f.agents["bob"].ID {
			t.Fatalf("candidate detail leaked co-assignee ids: %#v", bobDetail)
		}
		bobPage, err := f.st.ListMessageRequests(f.ctx, f.principals["bob"], MessageRequestFilter{
			Role: "candidate", Limit: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, request := range bobPage.Requests {
			if request.ID == opened.Request.ID &&
				(len(request.SelectedAgentIDs) != 1 || request.SelectedAgentIDs[0] != f.agents["bob"].ID) {
				t.Fatalf("candidate list leaked co-assignee ids: %#v", request)
			}
		}
		bobClaim := f.claim(t, opened, "bob", "at-most-two-bob-claim")
		charlieClaim := f.claim(t, opened, "charlie", "at-most-two-charlie-claim")
		first, err := f.st.CompleteMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, CompleteMessageRequestInput{
			ClaimID: bobClaim.ClaimID, Generation: bobClaim.Generation,
			Body: "Bob is complete.", IdempotencyKey: "at-most-two-bob-complete",
		})
		if err != nil {
			t.Fatal(err)
		}
		if first.Request.State != MessageRequestStateOpen || first.Request.ActiveClaimCount != 1 {
			t.Fatalf("first of two completion = %#v", first)
		}
		second, err := f.st.CompleteMessageRequest(f.ctx, f.principals["charlie"], opened.Request.ID, CompleteMessageRequestInput{
			ClaimID: charlieClaim.ClaimID, Generation: charlieClaim.Generation,
			Body: "Charlie is complete.", IdempotencyKey: "at-most-two-charlie-complete",
		})
		if err != nil {
			t.Fatal(err)
		}
		if second.Request.State != MessageRequestStateCompleted || second.Request.CompletedClaimCount != 2 {
			t.Fatalf("second of two completion = %#v", second)
		}
	})

	t.Run("released sibling no longer blocks completion", func(t *testing.T) {
		opened := f.open(t, "scott", "at-most-release-open", 2)
		f.offer(t, opened, "bob", "at-most-release-bob-offer")
		f.offer(t, opened, "charlie", "at-most-release-charlie-offer")
		f.selectAgents(t, opened, "scott", "at-most-release-select", "bob", "charlie")
		bobClaim := f.claim(t, opened, "bob", "at-most-release-bob-claim")
		charlieClaim := f.claim(t, opened, "charlie", "at-most-release-charlie-claim")
		if _, err := f.st.ReleaseMessageRequest(f.ctx, f.principals["charlie"], opened.Request.ID, ReleaseMessageRequestInput{
			ClaimID: charlieClaim.ClaimID, Generation: charlieClaim.Generation,
		}); err != nil {
			t.Fatal(err)
		}
		completed, err := f.st.CompleteMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, CompleteMessageRequestInput{
			ClaimID: bobClaim.ClaimID, Generation: bobClaim.Generation,
			Body: "The remaining live work is complete.", IdempotencyKey: "at-most-release-complete",
		})
		if err != nil {
			t.Fatal(err)
		}
		if completed.Request.State != MessageRequestStateCompleted {
			t.Fatalf("completion after sibling release = %#v", completed)
		}
	})

	t.Run("releasing last sibling after a result completes the batch", func(t *testing.T) {
		opened := f.open(t, "scott", "at-most-result-release-open", 2)
		f.offer(t, opened, "bob", "at-most-result-release-bob-offer")
		f.offer(t, opened, "charlie", "at-most-result-release-charlie-offer")
		f.selectAgents(t, opened, "scott", "at-most-result-release-select", "bob", "charlie")
		bobClaim := f.claim(t, opened, "bob", "at-most-result-release-bob-claim")
		charlieClaim := f.claim(t, opened, "charlie", "at-most-result-release-charlie-claim")
		first, err := f.st.CompleteMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, CompleteMessageRequestInput{
			ClaimID: bobClaim.ClaimID, Generation: bobClaim.Generation,
			Body: "Bob produced the accepted result.", IdempotencyKey: "at-most-result-release-bob-complete",
		})
		if err != nil {
			t.Fatal(err)
		}
		if first.Request.State != MessageRequestStateOpen {
			t.Fatalf("request closed before live sibling release = %#v", first)
		}
		if _, err := f.st.ReleaseMessageRequest(f.ctx, f.principals["charlie"], opened.Request.ID, ReleaseMessageRequestInput{
			ClaimID: charlieClaim.ClaimID, Generation: charlieClaim.Generation,
		}); err != nil {
			t.Fatal(err)
		}
		detail, err := f.st.GetMessageRequest(f.ctx, f.principals["scott"], opened.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Request.State != MessageRequestStateCompleted || detail.Request.CompletedClaimCount != 1 {
			t.Fatalf("request after final sibling release = %#v", detail)
		}
	})

	t.Run("lazy sibling lease expiry completes a successful batch", func(t *testing.T) {
		opened := f.open(t, "scott", "at-most-result-expiry-open", 2)
		f.offer(t, opened, "bob", "at-most-result-expiry-bob-offer")
		f.offer(t, opened, "charlie", "at-most-result-expiry-charlie-offer")
		f.selectAgents(t, opened, "scott", "at-most-result-expiry-select", "bob", "charlie")
		bobClaim := f.claim(t, opened, "bob", "at-most-result-expiry-bob-claim")
		charlieClaim := f.claim(t, opened, "charlie", "at-most-result-expiry-charlie-claim")
		first, err := f.st.CompleteMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, CompleteMessageRequestInput{
			ClaimID: bobClaim.ClaimID, Generation: bobClaim.Generation,
			Body: "Bob produced the accepted result.", IdempotencyKey: "at-most-result-expiry-bob-complete",
		})
		if err != nil {
			t.Fatal(err)
		}
		if first.Request.State != MessageRequestStateOpen {
			t.Fatalf("request closed before live sibling expiry = %#v", first)
		}
		if _, err := f.st.pool.Exec(f.ctx, `
			UPDATE agent_message_request_claims
			SET lease_expires_at=clock_timestamp()-interval '1 second'
			WHERE id=$1`, charlieClaim.ClaimID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.SelectMessageRequest(f.ctx, f.principals["scott"], opened.Request.ID, SelectMessageRequestInput{
			SelectedAgentIDs: []string{f.agents["charlie"].ID}, Reservation: time.Minute,
			IdempotencyKey: "at-most-result-expiry-reselect",
		}); !errors.Is(err, ErrMessageRequestConflict) {
			t.Fatalf("selection after successful batch exhaustion error = %v", err)
		}
		detail, err := f.st.GetMessageRequest(f.ctx, f.principals["scott"], opened.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Request.State != MessageRequestStateCompleted || detail.Request.CompletedClaimCount != 1 {
			t.Fatalf("request after lazy sibling expiry = %#v", detail)
		}
		var siblingState string
		if err := f.st.pool.QueryRow(f.ctx, `SELECT state FROM agent_message_request_claims WHERE id=$1`, charlieClaim.ClaimID).
			Scan(&siblingState); err != nil {
			t.Fatal(err)
		}
		if siblingState != MessageRequestClaimCancelled {
			t.Fatalf("expired sibling state = %s", siblingState)
		}
	})
}

func TestMessageRequestCompletionRejectsLeaseThatExpiresDuringWorkPostgres(t *testing.T) {
	f := newMessageRequestHardeningFixture(t, "scott", "bob")
	opened := f.open(t, "scott", "stalled-complete-open", 1)
	f.offer(t, opened, "bob", "stalled-complete-offer")
	f.selectAgents(t, opened, "scott", "stalled-complete-select", "bob")
	claim := f.claim(t, opened, "bob", "stalled-complete-claim")
	if _, err := f.st.pool.Exec(f.ctx, `
		UPDATE agent_message_request_claims
		SET lease_expires_at=clock_timestamp()+interval '2 seconds'
		WHERE id=$1`, claim.ClaimID); err != nil {
		t.Fatal(err)
	}
	blocker, err := f.st.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	var lockedOpening string
	if err := blocker.QueryRow(f.ctx, `SELECT id FROM agent_messages WHERE id=$1 FOR UPDATE`, opened.OpeningMessage.ID).
		Scan(&lockedOpening); err != nil {
		t.Fatal(err)
	}
	completionErr := make(chan error, 1)
	go func() {
		_, err := f.st.CompleteMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, CompleteMessageRequestInput{
			ClaimID: claim.ClaimID, Generation: claim.Generation,
			Body:           "This result must not survive an expired fence.",
			IdempotencyKey: "stalled-complete-result",
		})
		completionErr <- err
	}()
	deadline := time.Now().Add(time.Second)
	claimLocked := false
	for time.Now().Before(deadline) {
		probe, err := f.st.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		var id string
		err = probe.QueryRow(f.ctx, `
			SELECT id FROM agent_message_request_claims WHERE id=$1 FOR UPDATE NOWAIT`, claim.ClaimID).Scan(&id)
		_ = probe.Rollback(f.ctx)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			claimLocked = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !claimLocked {
		t.Fatal("completion did not acquire its claim fence before blocking")
	}
	time.Sleep(2200 * time.Millisecond)
	if err := blocker.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-completionErr:
		if !errors.Is(err, ErrMessageRequestClaimLost) {
			t.Fatalf("stalled completion error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled completion did not finish")
	}
	var claimState string
	var resultCount int
	if err := f.st.pool.QueryRow(f.ctx, `
		SELECT c.state,
		  (SELECT count(*) FROM agent_messages m
		   WHERE m.account_id=c.account_id AND m.kind='result'
		     AND m.reply_to_message_id=$2)
		FROM agent_message_request_claims c WHERE c.id=$1`,
		claim.ClaimID, opened.OpeningMessage.ID).Scan(&claimState, &resultCount); err != nil {
		t.Fatal(err)
	}
	if claimState != MessageRequestClaimCancelled || resultCount != 0 {
		t.Fatalf("stale completion persisted state=%s results=%d", claimState, resultCount)
	}
}

func TestMessageRequestOfferAndSelectionRejectDeadlinesCrossedDuringWorkPostgres(t *testing.T) {
	f := newMessageRequestHardeningFixture(t, "scott", "bob")

	t.Run("offer rolls back its message after offer deadline", func(t *testing.T) {
		opened := f.open(t, "scott", "stalled-offer-open", 1)
		if _, err := f.st.pool.Exec(f.ctx, `
			UPDATE agent_message_requests
			SET offer_deadline=clock_timestamp()+interval '2 seconds',
			    expires_at=clock_timestamp()+interval '1 hour'
			WHERE id=$1`, opened.Request.ID); err != nil {
			t.Fatal(err)
		}
		blocker, err := f.st.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = blocker.Rollback(context.Background()) }()
		var openingID string
		if err := blocker.QueryRow(f.ctx, `SELECT id FROM agent_messages WHERE id=$1 FOR UPDATE`, opened.OpeningMessage.ID).
			Scan(&openingID); err != nil {
			t.Fatal(err)
		}
		offerErr := make(chan error, 1)
		go func() {
			_, err := f.st.OfferMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, OfferMessageRequestInput{
				Body: "This late offer must roll back.", IdempotencyKey: "stalled-offer-result",
			})
			offerErr <- err
		}()
		deadline := time.Now().Add(time.Second)
		candidateLocked := false
		for time.Now().Before(deadline) {
			probe, err := f.st.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			var requestID string
			err = probe.QueryRow(f.ctx, `
				SELECT request_id FROM agent_message_request_candidates
				WHERE request_id=$1 AND agent_id=$2 FOR UPDATE NOWAIT`,
				opened.Request.ID, f.agents["bob"].ID).Scan(&requestID)
			_ = probe.Rollback(f.ctx)
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
				candidateLocked = true
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !candidateLocked {
			t.Fatal("offer did not lock its candidate before blocking")
		}
		time.Sleep(2200 * time.Millisecond)
		if err := blocker.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-offerErr:
			if !errors.Is(err, ErrMessageRequestConflict) {
				t.Fatalf("stalled offer error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stalled offer did not finish")
		}
		var responseState string
		var offerMessages int
		if err := f.st.pool.QueryRow(f.ctx, `
			SELECT c.response_state,
			  (SELECT count(*) FROM agent_messages m
			   WHERE m.account_id=c.account_id AND m.kind='offer'
			     AND m.reply_to_message_id=$3)
			FROM agent_message_request_candidates c
			WHERE c.request_id=$1 AND c.agent_id=$2`, opened.Request.ID,
			f.agents["bob"].ID, opened.OpeningMessage.ID).Scan(&responseState, &offerMessages); err != nil {
			t.Fatal(err)
		}
		if responseState != MessageRequestCandidatePending || offerMessages != 0 {
			t.Fatalf("late offer persisted response=%s messages=%d", responseState, offerMessages)
		}
	})

	t.Run("selection rolls back claims after request expiry", func(t *testing.T) {
		opened := f.open(t, "scott", "stalled-selection-open", 1)
		f.offer(t, opened, "bob", "stalled-selection-offer")
		if _, err := f.st.pool.Exec(f.ctx, `
			UPDATE agent_message_requests
			SET created_at=clock_timestamp()-interval '2 seconds',
			    offer_deadline=clock_timestamp()-interval '1 second',
			    expires_at=clock_timestamp()+interval '2 seconds'
			WHERE id=$1`, opened.Request.ID); err != nil {
			t.Fatal(err)
		}
		blocker, err := f.st.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = blocker.Rollback(context.Background()) }()
		if _, err := blocker.Exec(f.ctx, `LOCK TABLE account_events IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		selectionErr := make(chan error, 1)
		go func() {
			_, err := f.st.SelectMessageRequest(f.ctx, f.principals["scott"], opened.Request.ID, SelectMessageRequestInput{
				SelectedAgentIDs: []string{f.agents["bob"].ID}, Reservation: time.Minute,
				IdempotencyKey: "stalled-selection-result",
			})
			selectionErr <- err
		}()
		deadline := time.Now().Add(time.Second)
		requestLocked := false
		for time.Now().Before(deadline) {
			probe, err := f.st.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			var requestID string
			err = probe.QueryRow(f.ctx, `
				SELECT id FROM agent_message_requests WHERE id=$1 FOR UPDATE NOWAIT`, opened.Request.ID).
				Scan(&requestID)
			_ = probe.Rollback(f.ctx)
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
				requestLocked = true
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !requestLocked {
			t.Fatal("selection did not lock its request before blocking")
		}
		time.Sleep(2200 * time.Millisecond)
		if err := blocker.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-selectionErr:
			if !errors.Is(err, ErrMessageRequestConflict) {
				t.Fatalf("stalled selection error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stalled selection did not finish")
		}
		var state string
		var selections, claims, selectedEvents, expiredEvents int
		if err := f.st.pool.QueryRow(f.ctx, `
			SELECT r.state,
			  (SELECT count(*) FROM agent_message_request_selections s WHERE s.request_id=r.id),
			  (SELECT count(*) FROM agent_message_request_claims c WHERE c.request_id=r.id),
			  (SELECT count(*) FROM account_events e WHERE e.account_id=r.account_id
			    AND e.verb=$2 AND e.metadata->>'request_id'=r.id),
			  (SELECT count(*) FROM account_events e WHERE e.account_id=r.account_id
			    AND e.verb=$3 AND e.metadata->>'request_id'=r.id)
			FROM agent_message_requests r WHERE r.id=$1`, opened.Request.ID,
			VerbMessageRequestSelected, VerbMessageRequestExpired).
			Scan(&state, &selections, &claims, &selectedEvents, &expiredEvents); err != nil {
			t.Fatal(err)
		}
		if state != MessageRequestStateExpired || selections != 0 || claims != 0 ||
			selectedEvents != 0 || expiredEvents != 1 {
			t.Fatalf("late selection persisted state=%s selections=%d claims=%d selected_events=%d expired_events=%d",
				state, selections, claims, selectedEvents, expiredEvents)
		}
	})

	t.Run("decline refuses a closed offer window", func(t *testing.T) {
		opened := f.open(t, "scott", "closed-decline-open", 1)
		if _, err := f.st.pool.Exec(f.ctx, `
			UPDATE agent_message_requests
			SET created_at=clock_timestamp()-interval '2 seconds',
			    offer_deadline=clock_timestamp()-interval '1 second'
			WHERE id=$1`, opened.Request.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.DeclineMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, DeclineMessageRequestInput{}); !errors.Is(err, ErrMessageRequestConflict) {
			t.Fatalf("closed-window decline error = %v", err)
		}
		var responseState string
		if err := f.st.pool.QueryRow(f.ctx, `
			SELECT response_state FROM agent_message_request_candidates
			WHERE request_id=$1 AND agent_id=$2`, opened.Request.ID, f.agents["bob"].ID).
			Scan(&responseState); err != nil {
			t.Fatal(err)
		}
		if responseState != MessageRequestCandidatePending {
			t.Fatalf("late decline response state = %s", responseState)
		}
	})
}

func TestMessageRequestAgentDeletionCleanupAndRevocationPostgres(t *testing.T) {
	f := newMessageRequestHardeningFixture(t, "scott", "bob", "alice", "charlie", "coordinator")

	pending := f.open(t, "scott", "delete-pending-open", 1)
	alicePrincipal := f.principals["alice"]
	if err := f.st.DeleteAgent(f.ctx, f.accountID, f.realm.ID, f.agents["alice"].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.GetMessageRequest(f.ctx, alicePrincipal, pending.Request.ID); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("deleted candidate get error = %v", err)
	}
	if _, err := f.st.ListMessageRequests(f.ctx, alicePrincipal, MessageRequestFilter{Limit: 10}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("deleted candidate list error = %v", err)
	}
	var responseState string
	var respondedAt *time.Time
	if err := f.st.pool.QueryRow(f.ctx, `
		SELECT response_state,responded_at
		FROM agent_message_request_candidates
		WHERE request_id=$1 AND agent_id=$2`, pending.Request.ID, f.agents["alice"].ID).
		Scan(&responseState, &respondedAt); err != nil {
		t.Fatal(err)
	}
	if responseState != MessageRequestCandidateDeclined || respondedAt == nil {
		t.Fatalf("deleted pending candidate = %s/%v", responseState, respondedAt)
	}

	assigned := f.open(t, "scott", "delete-assignee-open", 2)
	f.offer(t, assigned, "bob", "delete-assignee-bob-offer")
	f.offer(t, assigned, "charlie", "delete-assignee-offer")
	f.closeOfferWindow(t, assigned)
	f.selectAgents(t, assigned, "scott", "delete-assignee-select", "bob", "charlie")
	assignedBobClaim := f.claim(t, assigned, "bob", "delete-assignee-bob-claim")
	charlieClaim := f.claim(t, assigned, "charlie", "delete-assignee-claim")
	first, err := f.st.CompleteMessageRequest(f.ctx, f.principals["bob"], assigned.Request.ID, CompleteMessageRequestInput{
		ClaimID: assignedBobClaim.ClaimID, Generation: assignedBobClaim.Generation,
		Body: "Bob produced the accepted result.", IdempotencyKey: "delete-assignee-bob-complete",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Request.State != MessageRequestStateOpen {
		t.Fatalf("request closed before assignee deletion = %#v", first)
	}
	if err := f.st.DeleteAgent(f.ctx, f.accountID, f.realm.ID, f.agents["charlie"].ID); err != nil {
		t.Fatal(err)
	}
	var assignedState, deletedClaimState, deletedCandidateState, linkedOfferMessageID string
	var offerHistory int
	if err := f.st.pool.QueryRow(f.ctx, `
		SELECT r.state,c.state,candidate.response_state,
		       COALESCE(candidate.offer_message_id,''),
		       (SELECT count(*) FROM agent_messages m
		        WHERE m.realm_id=r.realm_id
		          AND m.kind='offer'
		          AND m.reply_to_message_id=r.opening_message_id
		          AND m.from_agent_id=$3)
		FROM agent_message_requests r
		JOIN agent_message_request_claims c ON c.id=$2
		JOIN agent_message_request_candidates candidate
		  ON candidate.request_id=r.id AND candidate.agent_id=$3
		WHERE r.id=$1`, assigned.Request.ID, charlieClaim.ClaimID, f.agents["charlie"].ID).
		Scan(&assignedState, &deletedClaimState, &deletedCandidateState, &linkedOfferMessageID, &offerHistory); err != nil {
		t.Fatal(err)
	}
	if assignedState != MessageRequestStateCompleted ||
		deletedClaimState != MessageRequestClaimCancelled ||
		deletedCandidateState != MessageRequestCandidateOffered ||
		linkedOfferMessageID == "" || offerHistory != 1 {
		t.Fatalf("deleted assignee cleanup = request %s claim %s candidate %s linked_offer %q offer_history %d",
			assignedState, deletedClaimState, deletedCandidateState, linkedOfferMessageID, offerHistory)
	}

	coordinated := f.open(t, "coordinator", "delete-coordinator-open", 1)
	f.offer(t, coordinated, "bob", "delete-coordinator-offer")
	f.closeOfferWindow(t, coordinated)
	f.selectAgents(t, coordinated, "coordinator", "delete-coordinator-select", "bob")
	bobClaim := f.claim(t, coordinated, "bob", "delete-coordinator-claim")
	if err := f.st.DeleteAgent(f.ctx, f.accountID, f.realm.ID, f.agents["coordinator"].ID); err != nil {
		t.Fatal(err)
	}
	detail, err := f.st.GetMessageRequest(f.ctx, f.principals["bob"], coordinated.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Request.State != MessageRequestStateCancelled ||
		len(detail.Claims) != 1 || detail.Claims[0].State != MessageRequestClaimCancelled {
		t.Fatalf("deleted coordinator detail = %#v", detail)
	}
	var cancellationEvents int
	if err := f.st.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM account_events
		WHERE account_id=$1 AND verb=$2 AND metadata->>'request_id'=$3
		  AND metadata->>'reason_code'='coordinator_deleted'`,
		f.accountID, VerbMessageRequestCancelledSystem, coordinated.Request.ID).
		Scan(&cancellationEvents); err != nil {
		t.Fatal(err)
	}
	if cancellationEvents != 1 {
		t.Fatalf("coordinator deletion cancellation events = %d", cancellationEvents)
	}
	var persistedClaimState string
	if err := f.st.pool.QueryRow(f.ctx, `SELECT state FROM agent_message_request_claims WHERE id=$1`, bobClaim.ClaimID).
		Scan(&persistedClaimState); err != nil {
		t.Fatal(err)
	}
	if persistedClaimState != MessageRequestClaimCancelled {
		t.Fatalf("coordinator deletion claim state = %s", persistedClaimState)
	}
}

func TestMessageRequestClaimUsesNewestSelectionGenerationPostgres(t *testing.T) {
	f := newMessageRequestHardeningFixture(t, "scott", "bob")
	opened := f.open(t, "scott", "claim-selection-order-open", 1)
	f.offer(t, opened, "bob", "claim-selection-order-offer")

	firstClaims := f.selectAgents(t, opened, "scott", "claim-selection-order-first", "bob")
	first := f.claim(t, opened, "bob", "claim-selection-order-first-claim")
	if _, err := f.st.ReleaseMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, ReleaseMessageRequestInput{
		ClaimID: first.ClaimID, Generation: first.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	secondClaims := f.selectAgents(t, opened, "scott", "claim-selection-order-second", "bob")

	// imported archives and cross-host clocks can make selected_at disagree
	// with immutable selection order. Generation remains the authority.
	if _, err := f.st.pool.Exec(f.ctx, `
		UPDATE agent_message_request_claims
		SET selected_at=CASE id
		  WHEN $1 THEN clock_timestamp()+interval '1 day'
		  WHEN $2 THEN clock_timestamp()-interval '1 day'
		END
		WHERE id IN ($1,$2)`, firstClaims[0].ClaimID, secondClaims[0].ClaimID); err != nil {
		t.Fatal(err)
	}
	claimed, err := f.st.ClaimMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, ClaimMessageRequestInput{
		LeaseDuration: 2 * time.Minute, IdempotencyKey: "claim-selection-order-second-claim",
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ClaimID != secondClaims[0].ClaimID || claimed.SelectionID != secondClaims[0].SelectionID {
		t.Fatalf("claimed stale selection: got %#v, want claim %s selection %s",
			claimed, secondClaims[0].ClaimID, secondClaims[0].SelectionID)
	}
}

func TestMessageRequestSelectionHistoryCeilingPostgres(t *testing.T) {
	f := newMessageRequestHardeningFixture(t, "scott", "bob")
	opened := f.open(t, "scott", "selection-history-ceiling-open", 1)
	f.offer(t, opened, "bob", "selection-history-ceiling-offer")
	if _, err := f.st.pool.Exec(f.ctx, `
		UPDATE agent_message_requests SET selection_generation=$2 WHERE id=$1`,
		opened.Request.ID, maxMessageRequestSelectionHistory); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SelectMessageRequest(f.ctx, f.principals["scott"], opened.Request.ID, SelectMessageRequestInput{
		SelectedAgentIDs: []string{f.agents["bob"].ID}, Reservation: 2 * time.Minute,
		IdempotencyKey: "selection-history-ceiling-select",
	}); !errors.Is(err, ErrMessageRequestConflict) {
		t.Fatalf("selection beyond history ceiling error = %v", err)
	}
	var selections int
	if err := f.st.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM agent_message_request_selections WHERE request_id=$1`, opened.Request.ID).
		Scan(&selections); err != nil {
		t.Fatal(err)
	}
	if selections != 0 {
		t.Fatalf("selections inserted beyond history ceiling = %d", selections)
	}
}

func TestMessageRequestCandidateOpeningUsesOwnDeliveryProjectionPostgres(t *testing.T) {
	f := newMessageRequestHardeningFixture(t, "scott", "bob", "charlie")
	opened := f.open(t, "scott", "candidate-delivery-projection-open", 1)
	if _, err := f.st.ReadMessage(f.ctx, f.principals["bob"], opened.OpeningMessage.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.pool.Exec(f.ctx, `
		UPDATE agent_message_deliveries SET failure_count=7
		WHERE message_id=$1 AND recipient_agent_id=$2`,
		opened.OpeningMessage.ID, f.agents["charlie"].ID); err != nil {
		t.Fatal(err)
	}

	bobDetail, err := f.st.GetMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bobDetail.OpeningMessage.ReadState.State != MessageReadRead ||
		bobDetail.OpeningMessage.Processing.State != MessageProcessingAvailable ||
		bobDetail.OpeningMessage.Processing.Generation != 0 ||
		bobDetail.OpeningMessage.Processing.FailureCount != 0 {
		t.Fatalf("bob opening projected realm aggregate: %#v", bobDetail.OpeningMessage)
	}

	charlieDetail, err := f.st.GetMessageRequest(f.ctx, f.principals["charlie"], opened.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if charlieDetail.OpeningMessage.ReadState.State != MessageReadUnread ||
		charlieDetail.OpeningMessage.Processing.State != MessageProcessingAvailable ||
		charlieDetail.OpeningMessage.Processing.Generation != 0 ||
		charlieDetail.OpeningMessage.Processing.FailureCount != 7 ||
		charlieDetail.OpeningMessage.Processing.ClaimID != "" {
		t.Fatalf("charlie opening does not use redacted own delivery: %#v", charlieDetail.OpeningMessage)
	}
}

func TestMessageRequestProtocolMessagesRejectOrdinaryProcessingPostgres(t *testing.T) {
	f := newMessageRequestHardeningFixture(t, "scott", "bob")
	opened := f.open(t, "scott", "protocol-processing-guard-open", 1)
	assertOrdinaryClaimForbidden := func(principal Principal, messageID, key string) {
		t.Helper()
		if _, err := f.st.ReadMessage(f.ctx, principal, messageID); err != nil {
			t.Fatalf("read protocol message %s: %v", messageID, err)
		}
		if _, err := f.st.ClaimMessage(f.ctx, principal, messageID, ClaimMessageInput{
			LeaseDuration: 2 * time.Minute, IdempotencyKey: key,
		}); !errors.Is(err, ErrMessageForbidden) {
			t.Fatalf("ordinary claim for protocol message %s error = %v", messageID, err)
		}
	}
	assertOrdinaryClaimForbidden(
		f.principals["bob"], opened.OpeningMessage.ID, "protocol-processing-opening-claim",
	)

	offered, err := f.st.OfferMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, OfferMessageRequestInput{
		Body: "I can handle this request.", IdempotencyKey: "protocol-processing-offer",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertOrdinaryClaimForbidden(
		f.principals["scott"], offered.Offer.Message.ID, "protocol-processing-offer-claim",
	)
	claims := f.selectAgents(t, opened, "scott", "protocol-processing-select", "bob")
	claimed := f.claim(t, opened, "bob", "protocol-processing-request-claim")
	if claimed.ClaimID != claims[0].ClaimID {
		t.Fatalf("request-specific claim = %s, want %s", claimed.ClaimID, claims[0].ClaimID)
	}
	completed, err := f.st.CompleteMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, CompleteMessageRequestInput{
		ClaimID: claimed.ClaimID, Generation: claimed.Generation,
		Body: "Request-specific completion.", IdempotencyKey: "protocol-processing-complete",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertOrdinaryClaimForbidden(
		f.principals["scott"], completed.Message.ID, "protocol-processing-result-claim",
	)
}

func TestMessageRequestListReconcilesOneBoundedBatchPostgres(t *testing.T) {
	f := newMessageRequestHardeningFixture(t, "scott", "bob")
	total := maxMessageRequestReconcileBatch + 3
	for i := 0; i < total; i++ {
		f.open(t, "scott", fmt.Sprintf("bounded-reconcile-open-%03d", i), 1)
	}
	if _, err := f.st.pool.Exec(f.ctx, `
		UPDATE agent_message_requests
		SET created_at=clock_timestamp()-interval '3 seconds',
		    offer_deadline=clock_timestamp()-interval '2 seconds',
		    expires_at=clock_timestamp()-interval '1 second',
		    updated_at=clock_timestamp()-interval '1 second'
		WHERE account_id=$1 AND state='open'`, f.accountID); err != nil {
		t.Fatal(err)
	}
	assertCounts := func(want int) {
		t.Helper()
		var expired, audits int
		if err := f.st.pool.QueryRow(f.ctx, `
			SELECT
			  (SELECT count(*) FROM agent_message_requests
			   WHERE account_id=$1 AND state='expired'),
			  (SELECT count(*) FROM account_events
			   WHERE account_id=$1 AND verb=$2)`,
			f.accountID, VerbMessageRequestExpired).Scan(&expired, &audits); err != nil {
			t.Fatal(err)
		}
		if expired != want || audits != want {
			t.Fatalf("bounded reconciliation = expired %d audits %d, want %d", expired, audits, want)
		}
	}
	if _, err := f.st.ListMessageRequests(f.ctx, f.principals["scott"], MessageRequestFilter{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	assertCounts(maxMessageRequestReconcileBatch)
	if _, err := f.st.ListMessageRequests(f.ctx, f.principals["scott"], MessageRequestFilter{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	assertCounts(total)
}

func TestMessageRequestExportMaterializesExpiryPostgres(t *testing.T) {
	f := newMessageRequestHardeningFixture(t, "scott", "bob")
	opened := f.open(t, "scott", "export-expiry-open", 1)
	f.offer(t, opened, "bob", "export-expiry-offer")
	f.selectAgents(t, opened, "scott", "export-expiry-select", "bob")
	claim := f.claim(t, opened, "bob", "export-expiry-claim")
	for i := 0; i < maxMessageRequestReconcileBatch; i++ {
		f.open(t, "scott", fmt.Sprintf("export-expiry-batch-%03d", i), 1)
	}
	if _, err := f.st.pool.Exec(f.ctx, `
		UPDATE agent_message_requests
		SET created_at=clock_timestamp()-interval '3 seconds',
		    offer_deadline=clock_timestamp()-interval '2 seconds',
		    expires_at=clock_timestamp()-interval '1 second',
		    updated_at=clock_timestamp()-interval '1 second'
		WHERE account_id=$1 AND state='open'`, f.accountID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SuspendAccountSystem(f.ctx, f.accountID, "evacuation", "message request expiry export"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := f.st.ExportAccount(f.ctx, f.accountID, "source-cell", "test", &archive); err != nil {
		t.Fatal(err)
	}
	archivedRequestState, archivedClaimState := "", ""
	archivedExpiredRequests, archivedExpiryEvents := 0, 0
	if _, err := archiveexport.Read(f.ctx, bytes.NewReader(archive.Bytes()), archiveexport.ImportOptions{
		CurrentSchema: SchemaVersion(),
		Row: func(table string, row []byte) error {
			var object map[string]any
			if err := json.Unmarshal(row, &object); err != nil {
				return err
			}
			switch table {
			case "agent_message_requests":
				if object["state"] == MessageRequestStateExpired {
					archivedExpiredRequests++
				}
				if object["id"] == opened.Request.ID {
					archivedRequestState, _ = object["state"].(string)
				}
			case "agent_message_request_claims":
				if object["id"] == claim.ClaimID {
					archivedClaimState, _ = object["state"].(string)
				}
			case "account_events":
				if object["verb"] == VerbMessageRequestExpired {
					archivedExpiryEvents++
				}
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if archivedRequestState != MessageRequestStateExpired ||
		archivedClaimState != MessageRequestClaimCancelled ||
		archivedExpiredRequests != maxMessageRequestReconcileBatch+1 ||
		archivedExpiryEvents != maxMessageRequestReconcileBatch+1 {
		t.Fatalf("archived expiry = request %q claim %q requests %d events %d",
			archivedRequestState, archivedClaimState,
			archivedExpiredRequests, archivedExpiryEvents)
	}
}

func TestMessageRequestExpiredClaimAndRenewFencesStayClosedPostgres(t *testing.T) {
	f := newMessageRequestHardeningFixture(t, "scott", "bob")
	opened := f.open(t, "scott", "expired-fence-open", 1)
	f.offer(t, opened, "bob", "expired-fence-offer")
	claims := f.selectAgents(t, opened, "scott", "expired-fence-select", "bob")
	if _, err := f.st.pool.Exec(f.ctx, `
		UPDATE agent_message_request_claims
		SET lease_expires_at=clock_timestamp()-interval '1 second'
		WHERE id=$1`, claims[0].ClaimID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ClaimMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, ClaimMessageRequestInput{
		LeaseDuration: time.Minute, IdempotencyKey: "expired-reservation-claim",
	}); !errors.Is(err, ErrMessageRequestClaimLost) {
		t.Fatalf("expired reservation claim error = %v", err)
	}
	var state string
	var generation int64
	if err := f.st.pool.QueryRow(f.ctx, `SELECT state,generation FROM agent_message_request_claims WHERE id=$1`, claims[0].ClaimID).
		Scan(&state, &generation); err != nil {
		t.Fatal(err)
	}
	if state != MessageRequestClaimReserved || generation != 0 {
		t.Fatalf("expired reservation mutated = %s/%d", state, generation)
	}

	replacement := f.selectAgents(t, opened, "scott", "expired-fence-reselect", "bob")
	claimed := f.claim(t, opened, "bob", "expired-fence-claim")
	if claimed.ClaimID != replacement[0].ClaimID {
		t.Fatalf("claimed replacement %s, want %s", claimed.ClaimID, replacement[0].ClaimID)
	}
	if _, err := f.st.pool.Exec(f.ctx, `
		UPDATE agent_message_request_claims
		SET lease_expires_at=clock_timestamp()-interval '1 second'
		WHERE id=$1`, claimed.ClaimID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.RenewMessageRequest(f.ctx, f.principals["bob"], opened.Request.ID, RenewMessageRequestInput{
		ClaimID: claimed.ClaimID, Generation: claimed.Generation, LeaseDuration: time.Minute,
	}); !errors.Is(err, ErrMessageRequestClaimLost) {
		t.Fatalf("expired renew error = %v", err)
	}
}
