package messagerunner

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

func TestRunnerOffersForPendingMessageRequestBeforeMailbox(t *testing.T) {
	detail := messageRequestCandidateFixture(requestPhaseCollectingOffers)
	api := newFakeMessageRequestRunnerAPI()
	api.pages[requestPhaseCollectingOffers] = client.MessageRequestPage{Requests: []client.MessageRequest{detail.Request}}
	api.details[detail.Request.ID] = detail
	provider := &fakeProvider{result: TurnResult{
		Schema: TurnResultSchemaV1, Outcome: OutcomeResult,
		Subject: "Storage and tests", Body: "I can implement the storage and tests.",
		Payload: json.RawMessage(`{"eta_minutes":20}`),
	}}
	offerMessage := requestOfferMessage(detail, "msg_offer", "agent_bob", "Bob")
	offerMessage.Subject, offerMessage.Body, offerMessage.Payload = provider.result.Subject, provider.result.Body, provider.result.Payload
	api.offerResult = client.OfferMessageRequestResult{
		Request: detail.Request,
		Offer: client.MessageRequestOffer{
			Agent:   client.MessageAgent{Kind: "agent", AgentID: "agent_bob", AgentName: "Bob"},
			Message: offerMessage,
		},
	}

	result, err := testRunner(api, provider).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusRequestOffered || result.RequestID != detail.Request.ID ||
		result.MessageID != detail.OpeningMessage.ID || result.ResultMessageID != offerMessage.ID {
		t.Fatalf("result = %+v", result)
	}
	if provider.envelope.Policy.RequestOperation != requestOperationOffer ||
		!reflect.DeepEqual(provider.envelope.Policy.AllowedOutcomes, []string{OutcomeResult, OutcomeDecline}) ||
		provider.envelope.Message.To.AgentID != "agent_bob" {
		t.Fatalf("provider envelope = %+v", provider.envelope)
	}
	if api.offerInput.IdempotencyKey != "message-runner/request-offer/runner-1/"+detail.Request.ID ||
		api.offerInput.Body != provider.result.Body {
		t.Fatalf("offer input = %+v", api.offerInput)
	}
	if containsCall(api.callsSnapshot(), "listen") {
		t.Fatalf("mailbox was consulted before request work: %v", api.callsSnapshot())
	}
}

func TestRunnerDeclinesPendingMessageRequestBeforeMailbox(t *testing.T) {
	detail := messageRequestCandidateFixture(requestPhaseCollectingOffers)
	api := newFakeMessageRequestRunnerAPI()
	api.pages[requestPhaseCollectingOffers] = client.MessageRequestPage{Requests: []client.MessageRequest{detail.Request}}
	api.details[detail.Request.ID] = detail
	api.declineResult = detail.Request
	provider := &fakeProvider{result: TurnResult{
		Schema: TurnResultSchemaV1, Outcome: OutcomeDecline, Body: "This request is outside my text-only capability.",
	}}

	result, err := testRunner(api, provider).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusRequestDeclined || result.RequestID != detail.Request.ID {
		t.Fatalf("result = %+v", result)
	}
	if api.declineKey != "message-runner/request-decline/runner-1/"+detail.Request.ID ||
		containsCall(api.callsSnapshot(), "request_offer") || containsCall(api.callsSnapshot(), "listen") {
		t.Fatalf("calls=%v decline_key=%q", api.callsSnapshot(), api.declineKey)
	}
}

func TestRunnerCoordinatorSelectsOnlyDurableOfferedCandidate(t *testing.T) {
	identity := client.SelfIdentity{
		AccountID: "account_1", RealmID: "realm_1", AgentID: "agent_scott", AgentName: "Scott",
	}
	detail := messageRequestCoordinatorFixture()
	api := newFakeMessageRequestRunnerAPI()
	api.self.Identity = identity
	api.pages[requestPhaseAwaitingSelection] = client.MessageRequestPage{Requests: []client.MessageRequest{detail.Request}}
	api.details[detail.Request.ID] = detail
	selectedRequest := detail.Request
	selectedRequest.Phase = requestPhaseAssigned
	selectedRequest.SelectedAgentIDs = []string{"agent_bob"}
	selectedRequest.SelectionGeneration = 1
	api.selectResult = client.SelectMessageRequestResult{
		Request: selectedRequest,
		Selection: client.MessageRequestSelection{
			ID: "msel_1", Generation: 1, Coordinator: detail.Request.Coordinator,
			SelectedAgentIDs: []string{"agent_bob"},
		},
		Claims: []client.MessageRequestClaim{{
			ClaimID: "mrc_1", RequestID: detail.Request.ID, SelectionID: "msel_1",
			Agent: client.MessageAgent{Kind: "agent", AgentID: "agent_bob", AgentName: "Bob"}, State: "reserved",
		}},
	}
	provider := &fakeProvider{result: TurnResult{
		Schema: TurnResultSchemaV1, Outcome: OutcomeResult, Body: "Bob is the strongest fit.",
		Payload: json.RawMessage(`{"selected_agent_ids":["agent_bob"]}`),
	}}
	runner := testRunner(api, provider)
	runner.Config.ExpectedIdentity = identity

	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusRequestSelected || result.RequestID != detail.Request.ID {
		t.Fatalf("result = %+v", result)
	}
	if provider.envelope.Policy.RequestOperation != requestOperationSelect ||
		!strings.Contains(provider.envelope.Message.Body, "agent_bob") {
		t.Fatalf("selection envelope = %+v", provider.envelope)
	}
	if !reflect.DeepEqual(api.selectInput.SelectedAgentIDs, []string{"agent_bob"}) ||
		api.selectInput.ReservationSeconds != 30 ||
		api.selectInput.IdempotencyKey != "message-runner/request-select/runner-1/"+detail.Request.ID+"/1" {
		t.Fatalf("selection input = %+v", api.selectInput)
	}
}

func TestRunnerRejectsCoordinatorSelectionOutsideOffersAndCapacity(t *testing.T) {
	identity := client.SelfIdentity{
		AccountID: "account_1", RealmID: "realm_1", AgentID: "agent_scott", AgentName: "Scott",
	}
	for _, test := range []struct {
		name    string
		payload json.RawMessage
	}{
		{name: "not an offerer", payload: json.RawMessage(`{"selected_agent_ids":["agent_alice"]}`)},
		{name: "over capacity", payload: json.RawMessage(`{"selected_agent_ids":["agent_bob","agent_alice"]}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail := messageRequestCoordinatorFixture()
			api := newFakeMessageRequestRunnerAPI()
			api.self.Identity = identity
			api.pages[requestPhaseAwaitingSelection] = client.MessageRequestPage{Requests: []client.MessageRequest{detail.Request}}
			api.details[detail.Request.ID] = detail
			provider := &fakeProvider{result: TurnResult{
				Schema: TurnResultSchemaV1, Outcome: OutcomeResult, Body: "selection", Payload: test.payload,
			}}
			runner := testRunner(api, provider)
			runner.Config.ExpectedIdentity = identity

			result, err := runner.RunOnce(context.Background())
			if err == nil || !errors.Is(err, ErrProviderResultInvalid) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if containsCall(api.callsSnapshot(), "request_select") {
				t.Fatalf("invalid selection reached API: %v", api.callsSnapshot())
			}
		})
	}
}

func TestRunnerSelectedRequestRenewsAndCompletesBeforeMailbox(t *testing.T) {
	detail := messageRequestCandidateFixture(requestPhaseAssigned)
	detail.Request.SelectedAgentIDs = []string{"agent_bob"}
	detail.Candidates[0].ResponseState = "offered"
	detail.Candidates[0].OfferMessageID = "msg_offer"
	leaseExpires := time.Now().Add(time.Minute)
	reservation := client.MessageRequestClaim{
		ClaimID: "mrc_1", RequestID: detail.Request.ID, SelectionID: "msel_1",
		Agent: client.MessageAgent{Kind: "agent", AgentID: "agent_bob", AgentName: "Bob"}, State: "reserved",
		LeaseExpiresAt: &leaseExpires,
	}
	detail.Claims = []client.MessageRequestClaim{reservation}
	detail.Request.SelectionGeneration = 1
	detail.Selections = []client.MessageRequestSelection{{
		ID: reservation.SelectionID, Generation: 1,
		Coordinator: detail.Request.Coordinator, SelectedAgentIDs: []string{"agent_bob"},
	}}
	api := newFakeMessageRequestRunnerAPI()
	api.pages[requestPhaseAssigned] = client.MessageRequestPage{Requests: []client.MessageRequest{detail.Request}}
	api.details[detail.Request.ID] = detail
	api.claimResult = reservation
	api.claimResult.State = "claimed"
	api.claimResult.Generation = 1
	api.completeResult = client.CompleteMessageRequestResult{
		Request: func() client.MessageRequest {
			request := detail.Request
			request.State, request.Phase = "completed", ""
			request.SelectedAgentIDs = []string{}
			return request
		}(),
		Claim: func() client.MessageRequestClaim {
			claim := api.claimResult
			claim.State, claim.ResultMessageID = "completed", "msg_result"
			return claim
		}(),
		Message: client.Message{
			ID: "msg_result", AccountID: "account_1", RealmID: "realm_1",
			From: client.MessageAgent{Kind: "agent", AgentID: "agent_bob", AgentName: "Bob"},
			To:   client.MessageRecipient{Kind: "agent", AgentID: "agent_scott", AgentName: "Scott"},
			Kind: "result", Body: "The requested work is complete.", ThreadID: detail.OpeningMessage.ThreadID,
			ReplyToMessageID: detail.OpeningMessage.ID, CausalDepth: 2,
		},
	}
	releaseProvider := make(chan struct{})
	provider := &fakeProvider{waitFor: releaseProvider, result: TurnResult{
		Schema: TurnResultSchemaV1, Outcome: OutcomeResult, Body: "The requested work is complete.",
	}}
	runner := testRunner(api, provider)
	runner.Config.RenewInterval = time.Millisecond
	done := make(chan struct{})
	var result RunResult
	var runErr error
	go func() {
		result, runErr = runner.RunOnce(context.Background())
		close(done)
	}()
	select {
	case <-api.requestRenewed:
		close(releaseProvider)
	case <-time.After(time.Second):
		t.Fatal("request claim was not renewed")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request completion did not finish")
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if result.Status != RunStatusRequestCompleted || result.ResultMessageID != "msg_result" {
		t.Fatalf("result = %+v", result)
	}
	if api.requestClaimInput.LeaseSeconds != 30 ||
		api.requestClaimInput.IdempotencyKey != "message-runner/request-claim/runner-1/"+detail.Request.ID+"/mrc_1" {
		t.Fatalf("claim input = %+v", api.requestClaimInput)
	}
	if api.requestCompleteInput.ClaimID != "mrc_1" || api.requestCompleteInput.Generation != 1 ||
		api.requestCompleteInput.IdempotencyKey != "message-runner/request-complete/runner-1/"+detail.Request.ID+"/mrc_1/1" {
		t.Fatalf("completion input = %+v", api.requestCompleteInput)
	}
	if provider.envelope.Policy.RequestOperation != requestOperationExecute || containsCall(api.callsSnapshot(), "listen") {
		t.Fatalf("provider/calls = %+v / %v", provider.envelope.Policy, api.callsSnapshot())
	}
}

func TestSelectedRequestClaimUsesNewestSelectionGenerationDespiteClockSkew(t *testing.T) {
	detail, prior, _ := selectedMessageRequestFixture()
	prior.SelectedAt = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	priorLease := prior.SelectedAt.Add(time.Second)
	prior.LeaseExpiresAt = &priorLease
	current := prior
	current.ClaimID = "mrc_2"
	current.SelectionID = "msel_2"
	current.SelectedAt = prior.SelectedAt.Add(-24 * time.Hour)
	currentLease := current.SelectedAt.Add(time.Minute)
	current.LeaseExpiresAt = &currentLease
	detail.Claims = []client.MessageRequestClaim{prior, current}
	detail.Request.SelectionGeneration = 2
	detail.Selections = []client.MessageRequestSelection{
		{ID: prior.SelectionID, Generation: 1},
		{ID: current.SelectionID, Generation: 2},
	}

	got, selected, err := selectedRequestClaim(detail, client.SelfIdentity{
		AccountID: "account_1", RealmID: "realm_1", AgentID: "agent_bob", AgentName: "Bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !selected || got.ClaimID != current.ClaimID {
		t.Fatalf("selected=%t claim=%+v, want %s", selected, got, current.ClaimID)
	}
}

func TestRunnerResumesPastUnselectedPageWithoutStarvingMailbox(t *testing.T) {
	detail, reservation, claimed := selectedMessageRequestFixture()
	api := newFakeMessageRequestRunnerAPI()
	unselected := detail.Request
	unselected.ID, unselected.OpeningMessageID = "mrq_unselected", "msg_unselected"
	unselected.SelectedAgentIDs = []string{"agent_alice"}
	api.pages[requestPhaseAssigned] = client.MessageRequestPage{
		Requests: []client.MessageRequest{unselected}, NextCursor: "selected-page-2",
	}
	api.cursorPages[requestPhaseAssigned+"|selected-page-2"] = client.MessageRequestPage{
		Requests: []client.MessageRequest{detail.Request},
	}
	api.details[detail.Request.ID] = detail
	api.claimResult = claimed
	provider := &fakeProvider{result: TurnResult{
		Schema: TurnResultSchemaV1, Outcome: OutcomeResult, Body: "Completed after pagination.",
	}}
	api.completeResult = completedMessageRequestFixture(detail, claimed, provider.result)
	api.listen = client.MessageListenResult{TimedOut: true}
	runner := testRunner(api, provider)

	first, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != RunStatusIdle || !containsCall(api.callsSnapshot(), "listen") {
		t.Fatalf("first bounded scan result/calls = %+v / %v", first, api.callsSnapshot())
	}
	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusRequestCompleted || result.RequestID != detail.Request.ID {
		t.Fatalf("result = %+v", result)
	}
	calls := api.callsSnapshot()
	if countCall(calls, "request_list:"+requestPhaseAssigned) != 2 ||
		countCall(calls, "request_get") != 1 || countCall(calls, "listen") != 1 {
		t.Fatalf("resumed calls = %v", calls)
	}
	if api.requestClaimInput.IdempotencyKey != requestClaimIdempotencyKey("runner-1", detail.Request.ID, reservation.ClaimID) {
		t.Fatalf("claim input = %+v", api.requestClaimInput)
	}
}

func TestRunnerEscalatesSelectedRequestAtAggregatedFailureLimitWithoutRelease(t *testing.T) {
	detail, _, claimed := selectedMessageRequestFixture()
	detail.Request.SelectionGeneration = 2
	detail.Selections = []client.MessageRequestSelection{
		{ID: "msel_old", Generation: 1, Coordinator: detail.Request.Coordinator, SelectedAgentIDs: []string{"agent_bob"}},
		{ID: claimed.SelectionID, Generation: 2, Coordinator: detail.Request.Coordinator, SelectedAgentIDs: []string{"agent_bob"}},
	}
	detail.Claims = append([]client.MessageRequestClaim{{
		ClaimID: "mrc_old", RequestID: detail.Request.ID, SelectionID: "msel_old",
		Agent: client.MessageAgent{Kind: "agent", AgentID: "agent_bob", AgentName: "Bob"},
		State: "released", FailureCount: 2,
	}}, detail.Claims...)
	api := newFakeMessageRequestRunnerAPI()
	api.pages[requestPhaseAssigned] = client.MessageRequestPage{Requests: []client.MessageRequest{detail.Request}}
	api.details[detail.Request.ID] = detail
	api.claimResult = claimed
	escalation := TurnResult{
		Schema: TurnResultSchemaV1, Outcome: OutcomeResult, Subject: "Automated request escalation",
		Body: "Automated handling could not safely complete after repeated attempts and requires review.",
	}
	api.completeResult = completedMessageRequestFixture(detail, claimed, escalation)
	provider := &fakeProvider{err: errors.New("deterministic request failure")}
	runner := testRunner(api, provider)
	runner.Config.MaximumFailures = 3

	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusRequestCompleted || provider.calls != 1 ||
		api.requestCompleteInput.Body != escalation.Body || containsCall(api.callsSnapshot(), "request_release") {
		t.Fatalf("result=%+v provider_calls=%d complete=%+v calls=%v",
			result, provider.calls, api.requestCompleteInput, api.callsSnapshot())
	}
}

func TestRunnerReleasesSelectedRequestProviderOutageWithoutPoisonIncrement(t *testing.T) {
	detail, _, claimed := selectedMessageRequestFixture()
	api := newFakeMessageRequestRunnerAPI()
	api.pages[requestPhaseAssigned] = client.MessageRequestPage{Requests: []client.MessageRequest{detail.Request}}
	api.details[detail.Request.ID] = detail
	api.claimResult = claimed
	provider := &fakeProvider{err: ErrProviderUnavailable}

	result, err := testRunner(api, provider).RunOnce(context.Background())
	if err == nil || !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if api.requestReleaseInput.DeterministicFailure || !containsCall(api.callsSnapshot(), "request_release") {
		t.Fatalf("release=%+v calls=%v", api.requestReleaseInput, api.callsSnapshot())
	}
}

func TestRunnerFallsBackToMailboxWhenOptionalRequestAPIHasNoWork(t *testing.T) {
	api := newFakeMessageRequestRunnerAPI()
	api.listen = client.MessageListenResult{TimedOut: true}
	provider := &fakeProvider{}

	result, err := testRunner(api, provider).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusIdle || provider.calls != 0 {
		t.Fatalf("result=%+v provider_calls=%d", result, provider.calls)
	}
	want := []string{
		"self", "request_list:assigned", "request_list:collecting_offers",
		"request_list:awaiting_selection", "listen",
	}
	if got := api.callsSnapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls=%v want=%v", got, want)
	}
}

func TestRunnerAcceptsRealmFanoutNotificationForPinnedDelivery(t *testing.T) {
	api := newFakeRunnerAPI()
	api.listen.Messages[0].Kind = "open_request"
	api.listen.Messages[0].To = client.MessageRecipient{Kind: "realm", Count: 3}
	api.message = api.listen.Messages[0]
	api.message.Body = "Who can take this work?"
	runner := testRunner(api, &fakeProvider{})

	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusNotified {
		t.Fatalf("result = %+v", result)
	}
	if got, want := api.callsSnapshot(), []string{"self", "listen", "read", "ack"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls=%v want=%v", got, want)
	}
}

func messageRequestCandidateFixture(phase string) client.MessageRequestDetail {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	request := client.MessageRequest{
		ID: "mrq_1", AccountID: "account_1", RealmID: "realm_1", OpeningMessageID: "msg_open",
		Coordinator:     client.MessageAgent{Kind: "agent", AgentID: "agent_scott", AgentName: "Scott"},
		SelectionPolicy: "client_ranked", State: "open", Phase: phase,
		MaxAssignees: 1, CandidateCount: 2, SelectionGeneration: 0,
		OfferDeadline: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	opening := client.Message{
		ID: "msg_open", AccountID: "account_1", RealmID: "realm_1", From: request.Coordinator,
		To: client.MessageRecipient{Kind: "realm", Count: 2}, Subject: "Implement storage",
		Kind: "open_request", Body: "Implement the durable storage and focused tests.",
		ThreadID: "thr_1", CausalDepth: 1, CreatedAt: now,
	}
	return client.MessageRequestDetail{
		Request: request, OpeningMessage: opening,
		Candidates: []client.MessageRequestCandidate{{
			Agent:         client.MessageAgent{Kind: "agent", AgentID: "agent_bob", AgentName: "Bob"},
			ResponseState: "pending", CreatedAt: now,
		}},
	}
}

func messageRequestCoordinatorFixture() client.MessageRequestDetail {
	detail := messageRequestCandidateFixture(requestPhaseAwaitingSelection)
	detail.Request.Coordinator = client.MessageAgent{Kind: "agent", AgentID: "agent_scott", AgentName: "Scott"}
	detail.OpeningMessage.From = detail.Request.Coordinator
	detail.Candidates[0].ResponseState = "offered"
	detail.Candidates[0].OfferMessageID = "msg_offer"
	detail.Request.OfferCount = 1
	detail.Offers = []client.MessageRequestOffer{{
		Agent:   detail.Candidates[0].Agent,
		Message: requestOfferMessage(detail, "msg_offer", "agent_bob", "Bob"),
	}}
	return detail
}

func requestOfferMessage(detail client.MessageRequestDetail, messageID, agentID, agentName string) client.Message {
	return client.Message{
		ID: messageID, AccountID: detail.Request.AccountID, RealmID: detail.Request.RealmID,
		From: client.MessageAgent{Kind: "agent", AgentID: agentID, AgentName: agentName},
		To:   client.MessageRecipient{Kind: "agent", AgentID: detail.Request.Coordinator.AgentID, AgentName: detail.Request.Coordinator.AgentName},
		Kind: "offer", Body: "I can implement it.", ThreadID: detail.OpeningMessage.ThreadID,
		ReplyToMessageID: detail.OpeningMessage.ID, CausalDepth: 2,
	}
}

func selectedMessageRequestFixture() (client.MessageRequestDetail, client.MessageRequestClaim, client.MessageRequestClaim) {
	detail := messageRequestCandidateFixture(requestPhaseAssigned)
	detail.Request.SelectedAgentIDs = []string{"agent_bob"}
	detail.Request.SelectionGeneration = 1
	detail.Candidates[0].ResponseState = "offered"
	detail.Candidates[0].OfferMessageID = "msg_offer"
	leaseExpires := time.Now().Add(time.Minute)
	reservation := client.MessageRequestClaim{
		ClaimID: "mrc_1", RequestID: detail.Request.ID, SelectionID: "msel_1",
		Agent: client.MessageAgent{Kind: "agent", AgentID: "agent_bob", AgentName: "Bob"},
		State: "reserved", LeaseExpiresAt: &leaseExpires,
	}
	detail.Claims = []client.MessageRequestClaim{reservation}
	detail.Selections = []client.MessageRequestSelection{{
		ID: reservation.SelectionID, Generation: 1,
		Coordinator: detail.Request.Coordinator, SelectedAgentIDs: []string{"agent_bob"},
	}}
	claimed := reservation
	claimed.State, claimed.Generation = "claimed", 1
	return detail, reservation, claimed
}

func completedMessageRequestFixture(
	detail client.MessageRequestDetail,
	claim client.MessageRequestClaim,
	result TurnResult,
) client.CompleteMessageRequestResult {
	request := detail.Request
	request.State, request.Phase = "completed", ""
	request.SelectedAgentIDs = []string{}
	completedClaim := claim
	completedClaim.State, completedClaim.ResultMessageID = "completed", "msg_result"
	return client.CompleteMessageRequestResult{
		Request: request, Claim: completedClaim,
		Message: client.Message{
			ID: "msg_result", AccountID: detail.Request.AccountID, RealmID: detail.Request.RealmID,
			From: claim.Agent,
			To: client.MessageRecipient{
				Kind: "agent", AgentID: detail.Request.Coordinator.AgentID, AgentName: detail.Request.Coordinator.AgentName,
			},
			Subject: result.Subject, Kind: "result", Body: result.Body, Payload: result.Payload,
			ThreadID: detail.OpeningMessage.ThreadID, ReplyToMessageID: detail.OpeningMessage.ID,
			CausalDepth: detail.OpeningMessage.CausalDepth + 1,
		},
	}
}

func countCall(calls []string, target string) int {
	count := 0
	for _, call := range calls {
		if call == target {
			count++
		}
	}
	return count
}

type fakeMessageRequestRunnerAPI struct {
	*fakeRunnerAPI

	requestMu   sync.Mutex
	pages       map[string]client.MessageRequestPage
	cursorPages map[string]client.MessageRequestPage
	details     map[string]client.MessageRequestDetail

	offerResult    client.OfferMessageRequestResult
	declineResult  client.MessageRequest
	selectResult   client.SelectMessageRequestResult
	claimResult    client.MessageRequestClaim
	completeResult client.CompleteMessageRequestResult

	offerInput           client.OfferMessageRequestInput
	declineKey           string
	selectInput          client.SelectMessageRequestInput
	requestClaimInput    client.ClaimMessageRequestInput
	requestRenewInput    client.RenewMessageRequestInput
	requestReleaseInput  client.ReleaseMessageRequestInput
	requestCompleteInput client.CompleteMessageRequestInput
	requestRenewed       chan struct{}
	requestRenewOnce     sync.Once
}

func newFakeMessageRequestRunnerAPI() *fakeMessageRequestRunnerAPI {
	return &fakeMessageRequestRunnerAPI{
		fakeRunnerAPI: newFakeRunnerAPI(), pages: map[string]client.MessageRequestPage{},
		cursorPages: map[string]client.MessageRequestPage{}, details: map[string]client.MessageRequestDetail{},
		requestRenewed: make(chan struct{}),
	}
}

func (a *fakeMessageRequestRunnerAPI) ListMessageRequests(_ context.Context, opts client.MessageRequestListOptions) (client.MessageRequestPage, error) {
	a.record("request_list:" + opts.Phase)
	a.requestMu.Lock()
	defer a.requestMu.Unlock()
	if page, ok := a.cursorPages[opts.Phase+"|"+opts.Cursor]; ok {
		return page, nil
	}
	return a.pages[opts.Phase], nil
}

func (a *fakeMessageRequestRunnerAPI) GetMessageRequest(_ context.Context, requestID string) (client.MessageRequestDetail, error) {
	a.record("request_get")
	a.requestMu.Lock()
	defer a.requestMu.Unlock()
	return a.details[requestID], nil
}

func (a *fakeMessageRequestRunnerAPI) OfferMessageRequest(_ context.Context, _ string, in client.OfferMessageRequestInput) (client.OfferMessageRequestResult, error) {
	a.record("request_offer")
	a.offerInput = in
	return a.offerResult, nil
}

func (a *fakeMessageRequestRunnerAPI) DeclineMessageRequest(_ context.Context, _ string, idempotencyKey string) (client.MessageRequest, error) {
	a.record("request_decline")
	a.declineKey = idempotencyKey
	return a.declineResult, nil
}

func (a *fakeMessageRequestRunnerAPI) SelectMessageRequest(_ context.Context, _ string, in client.SelectMessageRequestInput) (client.SelectMessageRequestResult, error) {
	a.record("request_select")
	a.selectInput = in
	return a.selectResult, nil
}

func (a *fakeMessageRequestRunnerAPI) ClaimMessageRequest(_ context.Context, _ string, in client.ClaimMessageRequestInput) (client.MessageRequestClaim, error) {
	a.record("request_claim")
	a.requestClaimInput = in
	return a.claimResult, nil
}

func (a *fakeMessageRequestRunnerAPI) RenewMessageRequest(_ context.Context, _ string, in client.RenewMessageRequestInput) (client.MessageRequestClaim, error) {
	a.record("request_renew")
	a.requestRenewInput = in
	a.requestRenewOnce.Do(func() { close(a.requestRenewed) })
	return a.claimResult, nil
}

func (a *fakeMessageRequestRunnerAPI) ReleaseMessageRequest(_ context.Context, _ string, in client.ReleaseMessageRequestInput) (client.MessageRequestClaim, error) {
	a.record("request_release")
	a.requestReleaseInput = in
	result := a.claimResult
	result.State = "released"
	return result, nil
}

func (a *fakeMessageRequestRunnerAPI) CompleteMessageRequest(_ context.Context, _ string, in client.CompleteMessageRequestInput) (client.CompleteMessageRequestResult, error) {
	a.record("request_complete")
	a.requestCompleteInput = in
	return a.completeResult, nil
}
