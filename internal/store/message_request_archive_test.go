package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMessageRequestArchiveRoundTripPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx,
		"message-request-archive@witwave.ai", "message request archive", time.Hour)
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
		return Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active"}
	}
	coordinator := createAgent("coordinator")
	worker := createAgent("worker")
	observer := createAgent("observer")
	coordinatorPrincipal := principal(coordinator)
	workerPrincipal := principal(worker)
	observerPrincipal := principal(observer)

	open := func(key string) OpenMessageRequestResult {
		t.Helper()
		opened, err := st.OpenMessageRequest(ctx, coordinatorPrincipal, OpenMessageRequestInput{
			Subject: "portable work", Body: "offer on this portable request",
			Payload: json.RawMessage(`{"archive":true}`), OfferWindow: 2 * time.Minute,
			ExpiresIn: time.Hour, IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return opened
	}
	offerSelectClaim := func(opened OpenMessageRequestResult, prefix string) (MessageRequestOffer, MessageRequestClaim) {
		t.Helper()
		offered, err := st.OfferMessageRequest(ctx, workerPrincipal, opened.Request.ID, OfferMessageRequestInput{
			Body: "I can do this work", IdempotencyKey: prefix + "-offer",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DeclineMessageRequest(ctx, observerPrincipal, opened.Request.ID, DeclineMessageRequestInput{}); err != nil {
			t.Fatal(err)
		}
		selected, err := st.SelectMessageRequest(ctx, coordinatorPrincipal, opened.Request.ID, SelectMessageRequestInput{
			SelectedAgentIDs: []string{worker.ID}, Reservation: 5 * time.Minute,
			IdempotencyKey: prefix + "-select",
		})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := st.ClaimMessageRequest(ctx, workerPrincipal, opened.Request.ID, ClaimMessageRequestInput{
			LeaseDuration: 5 * time.Minute, IdempotencyKey: prefix + "-claim",
		})
		if err != nil {
			t.Fatal(err)
		}
		if selected.Claims[0].ClaimID != claimed.ClaimID {
			t.Fatalf("selected claim %q != claimed %q", selected.Claims[0].ClaimID, claimed.ClaimID)
		}
		return offered.Offer, claimed
	}

	completedRequest := open("archive-completed-open")
	completedOffer, completedClaim := offerSelectClaim(completedRequest, "archive-completed")
	completed, err := st.CompleteMessageRequest(ctx, workerPrincipal, completedRequest.Request.ID, CompleteMessageRequestInput{
		ClaimID: completedClaim.ClaimID, Generation: completedClaim.Generation,
		Body: "portable completed result", IdempotencyKey: "archive-completed-result",
	})
	if err != nil {
		t.Fatal(err)
	}
	activeRequest := open("archive-active-open")
	_, activeClaim := offerSelectClaim(activeRequest, "archive-active")

	// Deleting an assigned candidate revokes its live fence while preserving
	// the immutable offer that made the historical selection valid. The open
	// request must remain portable even though that candidate is now a tombstone.
	deletedWorker := createAgent("deleted-worker")
	deletedWorkerPrincipal := principal(deletedWorker)
	deletedRequest := open("archive-deleted-worker-open")
	deletedOffer, err := st.OfferMessageRequest(ctx, deletedWorkerPrincipal, deletedRequest.Request.ID, OfferMessageRequestInput{
		Body: "I can do this work before deletion", IdempotencyKey: "archive-deleted-worker-offer",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, candidate := range []Principal{workerPrincipal, observerPrincipal} {
		if _, err := st.DeclineMessageRequest(ctx, candidate, deletedRequest.Request.ID, DeclineMessageRequestInput{
			IdempotencyKey: "archive-deleted-worker-decline-" + string(rune('a'+i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	deletedSelection, err := st.SelectMessageRequest(ctx, coordinatorPrincipal, deletedRequest.Request.ID, SelectMessageRequestInput{
		SelectedAgentIDs: []string{deletedWorker.ID}, Reservation: 5 * time.Minute,
		IdempotencyKey: "archive-deleted-worker-select",
	})
	if err != nil {
		t.Fatal(err)
	}
	deletedClaim, err := st.ClaimMessageRequest(ctx, deletedWorkerPrincipal, deletedRequest.Request.ID, ClaimMessageRequestInput{
		LeaseDuration: 5 * time.Minute, IdempotencyKey: "archive-deleted-worker-claim",
	})
	if err != nil {
		t.Fatal(err)
	}
	if deletedSelection.Claims[0].ClaimID != deletedClaim.ClaimID {
		t.Fatalf("deleted worker selection/claim mismatch = %s/%s",
			deletedSelection.Claims[0].ClaimID, deletedClaim.ClaimID)
	}
	if err := st.DeleteAgent(ctx, provisioned.AccountID, realm.ID, deletedWorker.ID); err != nil {
		t.Fatal(err)
	}
	delayedRequest, err := st.OpenMessageRequest(ctx, coordinatorPrincipal, OpenMessageRequestInput{
		Body:        "Expire while this archive is moving between cells.",
		OfferWindow: time.Second, ExpiresIn: 5 * time.Second,
		IdempotencyKey: "archive-delayed-import-open",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID, "evacuation", "archive round trip"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, provisioned.AccountID, "source-cell", "test", &archive); err != nil {
		t.Fatal(err)
	}
	if err := deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID); err != nil {
		t.Fatal(err)
	}
	if wait := time.Until(delayedRequest.Request.ExpiresAt) + 100*time.Millisecond; wait > 0 {
		time.Sleep(wait)
	}
	if _, err := st.ImportAccount(ctx, provisioned.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}

	var openingAudience string
	var openingIsFanout bool
	var openingDeliveries int
	if err := st.pool.QueryRow(ctx, `
		SELECT m.audience_kind, m.to_agent_id IS NULL, count(d.*)
		FROM agent_messages m
		JOIN agent_message_deliveries d ON d.message_id=m.id
		WHERE m.id=$1
		GROUP BY m.id`, completedRequest.OpeningMessage.ID).Scan(
		&openingAudience, &openingIsFanout, &openingDeliveries,
	); err != nil {
		t.Fatal(err)
	}
	if openingAudience != MessageRecipientRealm || !openingIsFanout || openingDeliveries != 2 {
		t.Fatalf("restored opening audience = %q / fanout=%v / deliveries=%d", openingAudience, openingIsFanout, openingDeliveries)
	}

	var restoredOfferID, restoredResultID, restoredCompletedState string
	if err := st.pool.QueryRow(ctx, `
		SELECT candidate.offer_message_id, claim.result_message_id, claim.state
		FROM agent_message_request_candidates candidate
		JOIN agent_message_request_claims claim
		  ON claim.request_id=candidate.request_id AND claim.agent_id=candidate.agent_id
		WHERE candidate.request_id=$1`, completedRequest.Request.ID).Scan(
		&restoredOfferID, &restoredResultID, &restoredCompletedState,
	); err != nil {
		t.Fatal(err)
	}
	if restoredOfferID != completedOffer.Message.ID || restoredResultID != completed.Message.ID ||
		restoredCompletedState != MessageRequestClaimCompleted {
		t.Fatalf("restored completed links = offer %q result %q state %q", restoredOfferID, restoredResultID, restoredCompletedState)
	}

	var restoredActiveState string
	var restoredActiveGeneration int64
	var restoredActiveLease *time.Time
	var restoredActiveCancelledAt *time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT state, generation, lease_expires_at, cancelled_at
		FROM agent_message_request_claims WHERE id=$1`, activeClaim.ClaimID).Scan(
		&restoredActiveState, &restoredActiveGeneration,
		&restoredActiveLease, &restoredActiveCancelledAt,
	); err != nil {
		t.Fatal(err)
	}
	if restoredActiveState != MessageRequestClaimCancelled ||
		restoredActiveGeneration != activeClaim.Generation+1 || restoredActiveLease != nil ||
		restoredActiveCancelledAt == nil {
		t.Fatalf("restored active fence = state %q generation %d lease %v cancelled %v",
			restoredActiveState, restoredActiveGeneration, restoredActiveLease, restoredActiveCancelledAt)
	}

	var deletedRequestState, deletedCandidateState, deletedOfferID, deletedClaimState string
	var deletedAt *time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT r.state,candidate.response_state,COALESCE(candidate.offer_message_id,''),
		       claim.state,agent.deleted_at
		FROM agent_message_requests r
		JOIN agent_message_request_candidates candidate
		  ON candidate.request_id=r.id AND candidate.agent_id=$2
		JOIN agent_message_request_claims claim
		  ON claim.request_id=r.id AND claim.agent_id=$2
		JOIN agents agent ON agent.id=$2
		WHERE r.id=$1`, deletedRequest.Request.ID, deletedWorker.ID).Scan(
		&deletedRequestState, &deletedCandidateState, &deletedOfferID,
		&deletedClaimState, &deletedAt,
	); err != nil {
		t.Fatal(err)
	}
	if deletedRequestState != MessageRequestStateOpen ||
		deletedCandidateState != MessageRequestCandidateOffered ||
		deletedOfferID != deletedOffer.Offer.Message.ID ||
		deletedClaimState != MessageRequestClaimCancelled || deletedAt == nil {
		t.Fatalf("restored deleted candidate graph = request %s candidate %s offer %q claim %s deleted %v",
			deletedRequestState, deletedCandidateState, deletedOfferID, deletedClaimState, deletedAt)
	}

	var delayedState string
	var delayedExpiredAt *time.Time
	var delayedExpiryEvents int
	if err := st.pool.QueryRow(ctx, `
		SELECT r.state,r.expired_at,
		       (SELECT count(*) FROM account_events e
		        WHERE e.account_id=r.account_id AND e.verb=$2
		          AND e.metadata->>'request_id'=r.id)
		FROM agent_message_requests r WHERE r.id=$1`,
		delayedRequest.Request.ID, VerbMessageRequestExpired).Scan(
		&delayedState, &delayedExpiredAt, &delayedExpiryEvents,
	); err != nil {
		t.Fatal(err)
	}
	if delayedState != MessageRequestStateExpired || delayedExpiredAt == nil || delayedExpiryEvents != 1 {
		t.Fatalf("delayed imported request = state %s expired %v events %d",
			delayedState, delayedExpiredAt, delayedExpiryEvents)
	}
}

func TestNormalizeImportedMessageRequestClaimCancelsActiveAuthority(t *testing.T) {
	importedAt := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	base := func(state string) map[string]any {
		return map[string]any{
			"id": "mrc_aaaaaaaaaaaaaaaa", "request_id": "mrq_aaaaaaaaaaaaaaaa",
			"selection_id": "msel_aaaaaaaaaaaaaaaa", "account_id": "acc_1",
			"realm_id": "rlm_1", "agent_id": "agent_1", "state": state,
			"generation": 0, "claim_key_hash": "", "lease_expires_at": nil,
			"failure_count": 0, "complete_key_hash": "", "result_message_id": nil,
			"selected_at": "2026-07-15T10:00:00Z", "claimed_at": nil,
			"released_at": nil, "completed_at": nil, "cancelled_at": nil,
			"updated_at": "2026-07-15T10:00:00Z",
		}
	}

	reserved := base("reserved")
	reserved["lease_expires_at"] = "2026-07-15T13:00:00Z"
	if err := newImportCtx("acc_1").normalizeImportedMessageRequestClaim(
		"agent_message_request_claims", reserved, importedAt,
	); err != nil {
		t.Fatal(err)
	}
	if reserved["state"] != "cancelled" || reserved["generation"] != 0 ||
		reserved["lease_expires_at"] != nil || reserved["cancelled_at"] != importedAt.Format(time.RFC3339Nano) {
		t.Fatalf("normalized reservation = %#v", reserved)
	}
	if _, err := validateImportedMessageRequestClaimContent(reserved); err != nil {
		t.Fatalf("normalized reservation is not a valid cancelled row: %v", err)
	}

	claimed := base("claimed")
	claimed["generation"] = 7
	claimed["claim_key_hash"] = strings.Repeat("a", 64)
	claimed["lease_expires_at"] = "2026-07-15T13:00:00Z"
	claimed["claimed_at"] = "2026-07-15T10:30:00Z"
	if err := newImportCtx("acc_1").normalizeImportedMessageRequestClaim(
		"agent_message_request_claims", claimed, importedAt,
	); err != nil {
		t.Fatal(err)
	}
	if claimed["state"] != "cancelled" || claimed["generation"] != int64(8) ||
		claimed["lease_expires_at"] != nil || claimed["claim_key_hash"] != strings.Repeat("a", 64) {
		t.Fatalf("normalized claim = %#v", claimed)
	}
	if _, err := validateImportedMessageRequestClaimContent(claimed); err != nil {
		t.Fatalf("normalized claim is not a valid cancelled row: %v", err)
	}

	completed := base("completed")
	completed["generation"] = 3
	completed["claim_key_hash"] = strings.Repeat("b", 64)
	completed["claimed_at"] = "2026-07-15T10:30:00Z"
	completed["completed_at"] = "2026-07-15T11:00:00Z"
	completed["complete_key_hash"] = strings.Repeat("c", 64)
	completed["result_message_id"] = "msg_cccccccccccccccc"
	before := maps.Clone(completed)
	if err := newImportCtx("acc_1").normalizeImportedMessageRequestClaim(
		"agent_message_request_claims", completed, importedAt,
	); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(completed, before) {
		t.Fatalf("completed request claim changed on import: %#v", completed)
	}

	exhausted := base("claimed")
	exhausted["generation"] = maxMessageProcessingGeneration
	exhausted["claim_key_hash"] = strings.Repeat("d", 64)
	exhausted["lease_expires_at"] = "2026-07-15T13:00:00Z"
	exhausted["claimed_at"] = "2026-07-15T10:30:00Z"
	if err := newImportCtx("acc_1").normalizeImportedMessageRequestClaim(
		"agent_message_request_claims", exhausted, importedAt,
	); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("exhausted request claim error = %v", err)
	}
}

func TestMessageRequestArchiveValidationPreservesSnapshotOffersSelectionsAndResults(t *testing.T) {
	const (
		accountID     = "acc_1"
		realmID       = "rlm_1"
		coordinatorID = "agent_coordinator"
		candidateID   = "agent_candidate"
		openingID     = "msg_aaaaaaaaaaaaaaaa"
		offerID       = "msg_bbbbbbbbbbbbbbbb"
		resultID      = "msg_cccccccccccccccc"
		requestID     = "mrq_aaaaaaaaaaaaaaaa"
		selectionID   = "msel_aaaaaaaaaaaaaaaa"
		claimID       = "mrc_aaaaaaaaaaaaaaaa"
	)
	ic := newImportCtx(accountID)
	ic.exportedAt = time.Date(2026, 7, 15, 10, 2, 0, 0, time.UTC)
	ic.realms[realmID] = true
	for _, agentID := range []string{coordinatorID, candidateID} {
		ic.agents[agentID] = true
		ic.liveAgents[agentID] = true
		ic.agentRealms[agentID] = realmID
	}
	feed := func(table string, row map[string]any) {
		t.Helper()
		if err := ic.validateAndRecord(table, row); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
	}
	message := func(id, from, to, audience, fingerprint, kind, parent string, depth int) map[string]any {
		var toValue any = to
		if to == "" {
			toValue = nil
		}
		return map[string]any{
			"id": id, "account_id": accountID, "realm_id": realmID,
			"from_agent_id": from, "to_agent_id": toValue,
			"audience_kind": audience, "audience_fingerprint": fingerprint,
			"kind":      kind,
			"thread_id": "thr_archive", "reply_to_message_id": archiveNullableString(parent),
			"causal_depth": depth, "created_at": "2026-07-15T10:00:00Z",
		}
	}
	delivery := func(messageID, recipientID string) map[string]any {
		return map[string]any{
			"message_id": messageID, "account_id": accountID, "realm_id": realmID,
			"recipient_agent_id": recipientID, "state": "delivered",
			"processing_state": "available", "processing_generation": 0,
			"failure_count": 0, "claim_id": nil, "claim_key_hash": "",
			"lease_expires_at": nil, "completed_at": nil,
			"complete_key_hash": "", "result_message_id": nil,
		}
	}
	futureMessage := message("msg_dddddddddddddddd", coordinatorID, candidateID, "agent", "", "note", "", 1)
	futureMessage["created_at"] = "9999-12-31T23:59:59Z"
	if err := ic.validateAndRecord("agent_messages", futureMessage); err == nil ||
		!strings.Contains(err.Error(), "exported_at") {
		t.Fatalf("year-9999 message timestamp error = %v", err)
	}

	feed("agent_messages", message(openingID, coordinatorID, "", "realm", messageRealmAudienceFingerprint(), "open_request", "", 1))
	feed("agent_messages", message(offerID, candidateID, coordinatorID, "agent", "", "offer", openingID, 2))
	feed("agent_messages", message(resultID, candidateID, coordinatorID, "agent", "", "result", openingID, 2))
	feed("agent_message_deliveries", delivery(openingID, candidateID))
	feed("agent_message_deliveries", delivery(offerID, coordinatorID))
	feed("agent_message_deliveries", delivery(resultID, coordinatorID))

	requestRow := map[string]any{
		"id": requestID, "account_id": accountID, "realm_id": realmID,
		"opening_message_id": openingID, "coordinator_agent_id": coordinatorID,
		"selection_policy": "client_ranked", "state": "completed", "max_assignees": 1,
		"offer_window_seconds": 15, "expires_in_seconds": 3600,
		"offer_deadline": "2026-07-15T10:00:15Z", "expires_at": "2026-07-15T11:00:00Z",
		"selection_generation": 1, "completed_at": "2026-07-15T10:00:30Z", "cancelled_at": nil,
		"expired_at": nil, "created_at": "2026-07-15T10:00:00Z",
		"updated_at": "2026-07-15T10:01:00Z",
	}
	feed("agent_message_requests", requestRow)
	futureRequest := maps.Clone(requestRow)
	futureRequest["id"] = "mrq_dddddddddddddddd"
	futureRequest["created_at"] = "9999-12-31T23:00:00Z"
	futureRequest["updated_at"] = "9999-12-31T23:00:00Z"
	if _, err := ic.validateImportedMessageRequest(futureRequest); err == nil ||
		!strings.Contains(err.Error(), "exported_at") {
		t.Fatalf("year-9999 request timestamp error = %v", err)
	}
	exhaustedHistory := maps.Clone(requestRow)
	exhaustedHistory["id"] = "mrq_eeeeeeeeeeeeeeee"
	exhaustedHistory["selection_generation"] = maxMessageRequestSelectionHistory + 1
	if _, err := ic.validateImportedMessageRequest(exhaustedHistory); err == nil ||
		!strings.Contains(err.Error(), "selection_generation") {
		t.Fatalf("selection history ceiling import error = %v", err)
	}
	feed("agent_message_request_candidates", map[string]any{
		"request_id": requestID, "account_id": accountID, "realm_id": realmID,
		"agent_id": candidateID, "response_state": "offered", "offer_message_id": offerID,
		"offer_key_hash":     strings.Repeat("a", 64),
		"offer_request_hash": strings.Repeat("b", 64),
		"responded_at":       "2026-07-15T10:00:10Z", "created_at": "2026-07-15T10:00:00Z",
	})
	feed("agent_message_request_selections", map[string]any{
		"id": selectionID, "request_id": requestID, "account_id": accountID,
		"realm_id": realmID, "coordinator_agent_id": coordinatorID, "generation": 1,
		"idempotency_key_hash": strings.Repeat("c", 64),
		"selection_hash":       strings.Repeat("d", 64), "created_at": "2026-07-15T10:00:20Z",
	})
	feed("agent_message_request_claims", map[string]any{
		"id": claimID, "request_id": requestID, "selection_id": selectionID,
		"account_id": accountID, "realm_id": realmID, "agent_id": candidateID,
		"state": "completed", "generation": 1,
		"claim_key_hash": strings.Repeat("e", 64), "lease_expires_at": nil,
		"failure_count": 0, "complete_key_hash": strings.Repeat("f", 64),
		"result_message_id": resultID, "selected_at": "2026-07-15T10:00:20Z",
		"claimed_at": "2026-07-15T10:00:21Z", "released_at": nil,
		"completed_at": "2026-07-15T10:00:30Z", "cancelled_at": nil,
		"updated_at": "2026-07-15T10:00:30Z",
	})

	if err := validateImportedMessageAudienceSnapshots(ic.messages); err != nil {
		t.Fatal(err)
	}
	if err := validateImportedMessageReplies(ic.messages); err != nil {
		t.Fatal(err)
	}
	if err := validateImportedMessageProcessingLinks(ic.messages, ic.deliveries); err != nil {
		t.Fatal(err)
	}
	if err := validateImportedMessageRequestGraph(ic); err != nil {
		t.Fatal(err)
	}
	partialCapacity := ic.messageRequests[requestID]
	partialCapacity.maxAssignees = 2
	ic.messageRequests[requestID] = partialCapacity
	if err := validateImportedMessageRequestGraph(ic); err != nil {
		t.Fatalf("completed request with one result below max capacity: %v", err)
	}

	invalidDeadlines := maps.Clone(requestRow)
	invalidDeadlines["offer_deadline"] = "2026-07-16T10:00:15Z"
	invalidDeadlines["expires_at"] = "2026-07-16T11:00:00Z"
	if _, err := ic.validateImportedMessageRequest(invalidDeadlines); err == nil ||
		!strings.Contains(err.Error(), "deadlines") {
		t.Fatalf("mismatched request deadlines error = %v", err)
	}

	openRequest := maps.Clone(requestRow)
	openRequest["state"] = MessageRequestStateOpen
	openRequest["completed_at"] = nil
	ic.liveAgents[coordinatorID] = false
	if _, err := ic.validateImportedMessageRequest(openRequest); err == nil ||
		!strings.Contains(err.Error(), "coordinator") || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("deleted open-request coordinator error = %v", err)
	}
	ic.liveAgents[coordinatorID] = true
	openScope, err := ic.validateImportedMessageRequest(openRequest)
	if err != nil {
		t.Fatal(err)
	}
	ic.messageRequests[requestID] = openScope
	pendingCandidate := map[string]any{
		"request_id": requestID, "account_id": accountID, "realm_id": realmID,
		"agent_id": candidateID, "response_state": "pending", "offer_message_id": nil,
		"offer_key_hash": "", "offer_request_hash": "", "responded_at": nil,
		"created_at": "2026-07-15T10:00:00Z",
	}
	ic.liveAgents[candidateID] = false
	if _, _, err := ic.validateImportedMessageRequestCandidate(pendingCandidate); err == nil ||
		!strings.Contains(err.Error(), "candidate") || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("deleted pending candidate error = %v", err)
	}
	ic.liveAgents[candidateID] = true
	ic.messageRequests[requestID] = partialCapacity

	delete(ic.messageRequestCandidates, messageRequestCandidateImportKey{requestID: requestID, agentID: candidateID})
	if err := validateImportedMessageRequestGraph(ic); err == nil ||
		!strings.Contains(err.Error(), "candidate snapshot") {
		t.Fatalf("missing candidate snapshot error = %v", err)
	}
}

func archiveNullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
