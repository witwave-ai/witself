package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func TestMessageRequestClientRankedLifecyclePostgres(t *testing.T) {
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

	provisioned, err := st.ProvisionAccount(ctx, "message-request-test@witwave.ai", "message request test", time.Hour)
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
	scott := createAgent("scott")
	bob := createAgent("bob")
	alice := createAgent("alice")
	charlie := createAgent("charlie")
	scottPrincipal := principal(scott)
	bobPrincipal := principal(bob)
	alicePrincipal := principal(alice)
	charliePrincipal := principal(charlie)

	openInput := OpenMessageRequestInput{
		Subject: "ship the request slice", Body: "Offer a safe implementation plan.",
		Payload: json.RawMessage(`{"ticket":"MSG-1"}`), MaxAssignees: 1,
		OfferWindow: 2 * time.Minute, ExpiresIn: time.Hour,
		IdempotencyKey: "open-request-1",
	}
	opened, err := st.OpenMessageRequest(ctx, scottPrincipal, openInput)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Request.CandidateCount != 3 || opened.Request.PendingCount != 3 ||
		opened.Request.Phase != MessageRequestPhaseCollectingOffers ||
		opened.OpeningMessage.To.Kind != MessageRecipientRealm || opened.OpeningMessage.To.Count != 3 {
		t.Fatalf("opened request = %#v / opening=%#v", opened.Request, opened.OpeningMessage)
	}
	replayed, err := st.OpenMessageRequest(ctx, scottPrincipal, openInput)
	if err != nil || replayed.Request.ID != opened.Request.ID || replayed.OpeningMessage.ID != opened.OpeningMessage.ID {
		t.Fatalf("open replay = %#v / %v", replayed, err)
	}
	conflictingOpen := openInput
	conflictingOpen.MaxAssignees = 2
	if _, err := st.OpenMessageRequest(ctx, scottPrincipal, conflictingOpen); !errors.Is(err, ErrMessageRequestConflict) {
		t.Fatalf("conflicting open error = %v", err)
	}

	bobOfferInput := OfferMessageRequestInput{
		Body: "I can implement storage and tests.", Payload: json.RawMessage(`{"eta_minutes":20}`),
		IdempotencyKey: "bob-offer-1",
	}
	bobOffer, err := st.OfferMessageRequest(ctx, bobPrincipal, opened.Request.ID, bobOfferInput)
	if err != nil {
		t.Fatal(err)
	}
	if bobOffer.Offer.Message.Kind != "offer" || bobOffer.Offer.Message.ReplyToMessageID != opened.OpeningMessage.ID ||
		bobOffer.Offer.Message.To.ID != scott.ID || bobOffer.Request.OfferCount != 1 {
		t.Fatalf("bob offer = %#v", bobOffer)
	}
	bobOfferReplay, err := st.OfferMessageRequest(ctx, bobPrincipal, opened.Request.ID, bobOfferInput)
	if err != nil || bobOfferReplay.Offer.Message.ID != bobOffer.Offer.Message.ID {
		t.Fatalf("bob offer replay = %#v / %v", bobOfferReplay, err)
	}
	if _, err := st.DeclineMessageRequest(ctx, bobPrincipal, opened.Request.ID, DeclineMessageRequestInput{}); !errors.Is(err, ErrMessageRequestConflict) {
		t.Fatalf("decline after offer error = %v", err)
	}
	if _, err := st.DeclineMessageRequest(ctx, alicePrincipal, opened.Request.ID, DeclineMessageRequestInput{}); err != nil {
		t.Fatal(err)
	}
	charlieOffer, err := st.OfferMessageRequest(ctx, charliePrincipal, opened.Request.ID, OfferMessageRequestInput{
		Body: "I can take the implementation if Bob is unavailable.", IdempotencyKey: "charlie-offer-1",
	})
	if err != nil || charlieOffer.Request.Phase != MessageRequestPhaseAwaitingSelection {
		t.Fatalf("charlie offer = %#v / %v", charlieOffer, err)
	}

	selectBobInput := SelectMessageRequestInput{
		SelectedAgentIDs: []string{bob.ID}, Reservation: 2 * time.Minute,
		IdempotencyKey: "select-bob-1",
	}
	selectedBob, err := st.SelectMessageRequest(ctx, scottPrincipal, opened.Request.ID, selectBobInput)
	if err != nil {
		t.Fatal(err)
	}
	if len(selectedBob.Claims) != 1 || selectedBob.Claims[0].State != MessageRequestClaimReserved ||
		selectedBob.Request.Phase != MessageRequestPhaseAssigned {
		t.Fatalf("selected Bob = %#v", selectedBob)
	}
	selectionReplay, err := st.SelectMessageRequest(ctx, scottPrincipal, opened.Request.ID, selectBobInput)
	if err != nil || selectionReplay.Selection.ID != selectedBob.Selection.ID {
		t.Fatalf("selection replay = %#v / %v", selectionReplay, err)
	}
	if _, err := st.SelectMessageRequest(ctx, scottPrincipal, opened.Request.ID, SelectMessageRequestInput{
		SelectedAgentIDs: []string{charlie.ID}, Reservation: 2 * time.Minute,
		IdempotencyKey: "select-charlie-too-early",
	}); !errors.Is(err, ErrMessageRequestConflict) {
		t.Fatalf("over-capacity selection error = %v", err)
	}

	bobClaimInput := ClaimMessageRequestInput{LeaseDuration: 2 * time.Minute, IdempotencyKey: "bob-claim-1"}
	bobClaim, err := st.ClaimMessageRequest(ctx, bobPrincipal, opened.Request.ID, bobClaimInput)
	if err != nil {
		t.Fatal(err)
	}
	if bobClaim.State != MessageRequestClaimClaimed || bobClaim.Generation != 1 || bobClaim.LeaseExpiresAt == nil {
		t.Fatalf("bob claim = %#v", bobClaim)
	}
	bobClaimReplay, err := st.ClaimMessageRequest(ctx, bobPrincipal, opened.Request.ID, bobClaimInput)
	if err != nil || bobClaimReplay.ClaimID != bobClaim.ClaimID {
		t.Fatalf("bob claim replay = %#v / %v", bobClaimReplay, err)
	}
	if _, err := st.ClaimMessageRequest(ctx, charliePrincipal, opened.Request.ID, ClaimMessageRequestInput{
		LeaseDuration: time.Minute, IdempotencyKey: "charlie-unselected-claim",
	}); !errors.Is(err, ErrMessageRequestNotFound) {
		t.Fatalf("unselected claim error = %v", err)
	}
	if _, err := st.RenewMessageRequest(ctx, bobPrincipal, opened.Request.ID, RenewMessageRequestInput{
		ClaimID: bobClaim.ClaimID, Generation: bobClaim.Generation, LeaseDuration: 3 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	releasedBob, err := st.ReleaseMessageRequest(ctx, bobPrincipal, opened.Request.ID, ReleaseMessageRequestInput{
		ClaimID: bobClaim.ClaimID, Generation: bobClaim.Generation, DeterministicFailure: true,
	})
	if err != nil || releasedBob.State != MessageRequestClaimReleased || releasedBob.FailureCount != 1 {
		t.Fatalf("release Bob = %#v / %v", releasedBob, err)
	}
	if _, err := st.CompleteMessageRequest(ctx, bobPrincipal, opened.Request.ID, CompleteMessageRequestInput{
		ClaimID: bobClaim.ClaimID, Generation: bobClaim.Generation, Body: "stale result", IdempotencyKey: "stale-bob-result",
	}); !errors.Is(err, ErrMessageRequestClaimLost) {
		t.Fatalf("stale completion error = %v", err)
	}

	selectedCharlie, err := st.SelectMessageRequest(ctx, scottPrincipal, opened.Request.ID, SelectMessageRequestInput{
		SelectedAgentIDs: []string{charlie.ID}, Reservation: 2 * time.Minute,
		IdempotencyKey: "select-charlie-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	charlieClaim, err := st.ClaimMessageRequest(ctx, charliePrincipal, opened.Request.ID, ClaimMessageRequestInput{
		LeaseDuration: 2 * time.Minute, IdempotencyKey: "charlie-claim-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := st.CompleteMessageRequest(ctx, charliePrincipal, opened.Request.ID, CompleteMessageRequestInput{
		ClaimID: charlieClaim.ClaimID, Generation: charlieClaim.Generation,
		Subject: "implementation complete", Body: "Storage and tests are complete.",
		Payload: json.RawMessage(`{"tests":"passed"}`), IdempotencyKey: "charlie-complete-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Request.State != MessageRequestStateCompleted || completed.Claim.State != MessageRequestClaimCompleted ||
		completed.Message.Kind != "result" || completed.Message.To.ID != scott.ID ||
		completed.Message.ReplyToMessageID != opened.OpeningMessage.ID {
		t.Fatalf("completion = %#v", completed)
	}
	completionReplay, err := st.CompleteMessageRequest(ctx, charliePrincipal, opened.Request.ID, CompleteMessageRequestInput{
		ClaimID: charlieClaim.ClaimID, Generation: charlieClaim.Generation,
		Subject: "implementation complete", Body: "Storage and tests are complete.",
		Payload: json.RawMessage(`{"tests":"passed"}`), IdempotencyKey: "charlie-complete-1",
	})
	if err != nil || completionReplay.Message.ID != completed.Message.ID {
		t.Fatalf("completion replay = %#v / %v", completionReplay, err)
	}
	if selectedCharlie.Selection.Generation != selectedBob.Selection.Generation+1 {
		t.Fatalf("selection generations = %d then %d", selectedBob.Selection.Generation, selectedCharlie.Selection.Generation)
	}

	detail, err := st.GetMessageRequest(ctx, scottPrincipal, opened.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Candidates) != 3 || len(detail.Offers) != 2 || len(detail.Selections) != 2 || len(detail.Claims) != 2 {
		t.Fatalf("coordinator detail counts = %d/%d/%d/%d", len(detail.Candidates), len(detail.Offers), len(detail.Selections), len(detail.Claims))
	}
	candidateDetail, err := st.GetMessageRequest(ctx, charliePrincipal, opened.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidateDetail.Candidates) != 1 || len(candidateDetail.Offers) != 1 || len(candidateDetail.Claims) != 1 {
		t.Fatalf("candidate detail widened = %#v", candidateDetail)
	}
	page, err := st.ListMessageRequests(ctx, scottPrincipal, MessageRequestFilter{Role: "coordinator", State: "completed", Limit: 10})
	if err != nil || len(page.Requests) != 1 || page.Requests[0].ID != opened.Request.ID {
		t.Fatalf("coordinator list = %#v / %v", page, err)
	}

	cancellable, err := st.OpenMessageRequest(ctx, scottPrincipal, OpenMessageRequestInput{
		Body: "This request will be cancelled.", OfferWindow: time.Minute,
		ExpiresIn: time.Hour, IdempotencyKey: "open-request-cancel",
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := st.CancelMessageRequest(ctx, scottPrincipal, cancellable.Request.ID)
	if err != nil || cancelled.State != MessageRequestStateCancelled {
		t.Fatalf("cancelled = %#v / %v", cancelled, err)
	}
	cancelledReplay, err := st.CancelMessageRequest(ctx, scottPrincipal, cancellable.Request.ID)
	if err != nil || cancelledReplay.State != MessageRequestStateCancelled {
		t.Fatalf("cancel replay = %#v / %v", cancelledReplay, err)
	}

	raced, err := st.OpenMessageRequest(ctx, scottPrincipal, OpenMessageRequestInput{
		Body: "Exactly one candidate may win this race.", OfferWindow: time.Minute,
		ExpiresIn: time.Hour, MaxAssignees: 1, IdempotencyKey: "open-request-race",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, offer := range []struct {
		principal Principal
		key       string
	}{
		{principal: bobPrincipal, key: "race-offer-bob"},
		{principal: charliePrincipal, key: "race-offer-charlie"},
	} {
		if _, err := st.OfferMessageRequest(ctx, offer.principal, raced.Request.ID, OfferMessageRequestInput{
			Body: "I can win the race.", IdempotencyKey: offer.key,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DeclineMessageRequest(ctx, alicePrincipal, raced.Request.ID, DeclineMessageRequestInput{}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, selection := range []struct {
		agentID string
		key     string
	}{{bob.ID, "race-select-bob"}, {charlie.ID, "race-select-charlie"}} {
		selection := selection
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := st.SelectMessageRequest(context.Background(), scottPrincipal, raced.Request.ID, SelectMessageRequestInput{
				SelectedAgentIDs: []string{selection.agentID}, Reservation: time.Minute,
				IdempotencyKey: selection.key,
			})
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrMessageRequestConflict):
			conflicted++
		default:
			t.Fatalf("selection race error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("selection race = %d succeeded, %d conflicted", succeeded, conflicted)
	}
	rows, err := st.pool.Query(ctx, `
		SELECT verb,count(*) FROM account_events
		WHERE account_id=$1 AND verb LIKE 'message.request.%'
		GROUP BY verb`, provisioned.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	auditCounts := map[string]int{}
	for rows.Next() {
		var verb string
		var count int
		if err := rows.Scan(&verb, &count); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		auditCounts[verb] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	wantAuditCounts := map[string]int{
		VerbMessageRequestOpened: 3, VerbMessageRequestOffered: 4,
		VerbMessageRequestDeclined: 2, VerbMessageRequestSelected: 3,
		VerbMessageRequestClaimed: 2, VerbMessageRequestRenewed: 1,
		VerbMessageRequestReleased: 1, VerbMessageRequestCompleted: 1,
		VerbMessageRequestCancelled: 1,
	}
	for verb, want := range wantAuditCounts {
		if got := auditCounts[verb]; got != want {
			t.Fatalf("audit %s count = %d, want %d (all=%v)", verb, got, want, auditCounts)
		}
	}
}
