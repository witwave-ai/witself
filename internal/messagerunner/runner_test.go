package messagerunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

func TestRunnerCompletesAndAcknowledgesOneActionableMessage(t *testing.T) {
	api := newFakeRunnerAPI()
	provider := &fakeProvider{result: TurnResult{
		Schema: TurnResultSchemaV1, Outcome: OutcomeResult,
		Subject: "Completed", Body: "The work is complete.",
	}}
	runner := testRunner(api, provider)

	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusCompleted || result.MessageID != "msg_1" || result.ResultMessageID != "msg_2" {
		t.Fatalf("unexpected run result: %+v", result)
	}
	if got, want := api.callsSnapshot(), []string{"self", "listen", "claim", "read", "complete", "ack"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if api.claimInput.IdempotencyKey != "message-runner/claim/runner-1/msg_1" || api.claimInput.LeaseSeconds != 30 {
		t.Fatalf("unexpected claim input: %+v", api.claimInput)
	}
	if api.completeInput.ClaimID != "mcl_1" || api.completeInput.Generation != 4 ||
		api.completeInput.Kind != "result" || api.completeInput.Body != "The work is complete." ||
		api.completeInput.IdempotencyKey != "message-runner/complete/runner-1/msg_1/4" {
		t.Fatalf("unexpected completion input: %+v", api.completeInput)
	}
	if provider.envelope.Message.Body != "do the work" || provider.envelope.Identity.AgentID != "agent_bob" ||
		!provider.envelope.Policy.MessageIsUntrusted || provider.envelope.Policy.CurrentTurn != 1 ||
		provider.envelope.Policy.MaximumTurns != 12 || provider.envelope.Message.Processing.ClaimID != "" {
		t.Fatalf("unexpected provider envelope: %+v", provider.envelope)
	}
	providerPayload, history, turn := decodeTurnPayload(api.completeInput.Payload)
	if len(providerPayload) != 0 || turn != 2 || len(history) != 1 || history[0].Body != "do the work" {
		t.Fatalf("unexpected continuation payload: provider=%s history=%#v turn=%d", providerPayload, history, turn)
	}
}

func TestRunnerReleasesClaimWhenProviderFails(t *testing.T) {
	api := newFakeRunnerAPI()
	provider := &fakeProvider{err: errors.New("inference unavailable")}
	runner := testRunner(api, provider)

	result, err := runner.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "inference unavailable") {
		t.Fatalf("result=%+v error=%v, want provider failure", result, err)
	}
	if got, want := api.callsSnapshot(), []string{"self", "listen", "claim", "read", "release"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if api.releaseInput.ClaimID != "mcl_1" || api.releaseInput.Generation != 4 {
		t.Fatalf("unexpected release fence: %+v", api.releaseInput)
	}
	if !api.releaseInput.DeterministicFailure {
		t.Fatalf("deterministic provider failure was not counted: %+v", api.releaseInput)
	}
}

func TestRunnerEscalatesRepeatedProviderFailureAtBackendFailureLimit(t *testing.T) {
	api := newFakeRunnerAPI()
	api.processing.Generation = 1
	api.processing.FailureCount = 4
	api.message.Processing.Generation = 1
	api.message.Processing.FailureCount = 4
	api.completed.Processing.Generation = 1
	api.completed.Processing.FailureCount = 4
	provider := &fakeProvider{err: errors.New("provider failed without private content")}
	runner := testRunner(api, provider)

	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusCompleted || api.completeInput.Kind != "escalation" ||
		!strings.Contains(api.completeInput.Body, "repeated attempts") {
		t.Fatalf("result=%+v completion=%+v", result, api.completeInput)
	}
	if got, want := api.callsSnapshot(), []string{"self", "listen", "claim", "read", "complete", "ack"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestRunnerDoesNotEscalateProviderWideOutageAtGenerationLimit(t *testing.T) {
	api := newFakeRunnerAPI()
	api.processing.Generation = 9
	api.processing.FailureCount = 19
	api.message.Processing.Generation = 9
	api.message.Processing.FailureCount = 19
	provider := &fakeProvider{err: fmt.Errorf("%w: unavailable", ErrNativeProviderCommand)}
	runner := testRunner(api, provider)

	result, err := runner.RunOnce(context.Background())
	if !errors.Is(err, ErrNativeProviderCommand) {
		t.Fatalf("result=%+v error=%v, want provider-wide failure", result, err)
	}
	if got, want := api.callsSnapshot(), []string{"self", "listen", "claim", "read", "release"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if api.releaseInput.DeterministicFailure {
		t.Fatalf("provider-wide outage consumed message failure budget: %+v", api.releaseInput)
	}
}

func TestRunnerDoesNotEscalateHighGenerationWithLowFailureCount(t *testing.T) {
	api := newFakeRunnerAPI()
	api.processing.Generation = 99
	api.processing.FailureCount = 0
	api.message.Processing.Generation = 99
	api.message.Processing.FailureCount = 0
	provider := &fakeProvider{err: errors.New("message-specific inference failure")}
	runner := testRunner(api, provider)

	result, err := runner.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "message-specific inference failure") {
		t.Fatalf("result=%+v error=%v, want deterministic failure", result, err)
	}
	if got, want := api.callsSnapshot(), []string{"self", "listen", "claim", "read", "release"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if !api.releaseInput.DeterministicFailure {
		t.Fatalf("deterministic failure release = %+v", api.releaseInput)
	}
}

func TestRunnerDoesNotCountProviderUnavailableAsMessageFailure(t *testing.T) {
	api := newFakeRunnerAPI()
	api.processing.FailureCount = 4
	api.message.Processing.FailureCount = 4
	provider := &fakeProvider{err: fmt.Errorf("%w: host setup failed", ErrProviderUnavailable)}
	runner := testRunner(api, provider)

	result, err := runner.RunOnce(context.Background())
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("result=%+v error=%v, want provider unavailable", result, err)
	}
	if got, want := api.callsSnapshot(), []string{"self", "listen", "claim", "read", "release"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if api.releaseInput.DeterministicFailure {
		t.Fatalf("provider unavailability consumed message failure budget: %+v", api.releaseInput)
	}
}

func TestRunnerRecoversCompletedUnacknowledgedMessage(t *testing.T) {
	api := newFakeRunnerAPI()
	api.processing = client.MessageProcessing{
		State: "completed", ClaimID: "mcl_1", Generation: 4, ResultMessageID: "msg_existing",
	}
	provider := &fakeProvider{err: errors.New("must not be invoked")}
	runner := testRunner(api, provider)

	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusRecovered || result.ResultMessageID != "msg_existing" || provider.calls != 0 {
		t.Fatalf("unexpected recovery result=%+v provider_calls=%d", result, provider.calls)
	}
	if got, want := api.callsSnapshot(), []string{"self", "listen", "claim", "ack"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestRunnerAcknowledgesNonActionableMessageWithoutReadingOrInference(t *testing.T) {
	api := newFakeRunnerAPI()
	api.listen.Messages[0].Kind = "result"
	api.message.Kind = "result"
	provider := &fakeProvider{err: errors.New("must not be invoked")}
	runner := testRunner(api, provider)

	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusNotified || provider.calls != 0 {
		t.Fatalf("unexpected result=%+v provider_calls=%d", result, provider.calls)
	}
	if got, want := api.callsSnapshot(), []string{"self", "listen", "read", "ack"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	state := runner.State.(*fakeOperationalState)
	if len(state.notifications) != 1 || state.notifications[0].ID != "msg_1" {
		t.Fatalf("notification state = %#v", state.notifications)
	}
}

func TestRunnerRejectsReboundTokenBeforeMailboxAccess(t *testing.T) {
	api := newFakeRunnerAPI()
	api.self.Identity.AgentID = "agent_alice"
	runner := testRunner(api, &fakeProvider{})

	_, err := runner.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pinned account, realm, and agent") {
		t.Fatalf("error = %v, want identity mismatch", err)
	}
	if got, want := api.callsSnapshot(), []string{"self"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestRunnerRenewsLeaseDuringProviderInvocation(t *testing.T) {
	api := newFakeRunnerAPI()
	provider := &fakeProvider{
		waitFor: api.renewed,
		result:  TurnResult{Schema: TurnResultSchemaV1, Outcome: OutcomeQuestion, Body: "Which environment?"},
	}
	runner := testRunner(api, provider)
	runner.Config.RenewInterval = time.Millisecond

	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusCompleted || api.completeInput.Kind != "question" {
		t.Fatalf("unexpected run result=%+v completion=%+v", result, api.completeInput)
	}
	calls := api.callsSnapshot()
	if !containsCall(calls, "renew") || indexCall(calls, "renew") > indexCall(calls, "complete") {
		t.Fatalf("renewal was not completed before completion: %v", calls)
	}
}

func TestRunnerUsesReplyKindForAnswerToQuestion(t *testing.T) {
	api := newFakeRunnerAPI()
	api.listen.Messages[0].Kind = "question"
	api.message.Kind = "question"
	runner := testRunner(api, &fakeProvider{result: TurnResult{
		Schema: TurnResultSchemaV1, Outcome: OutcomeResult, Body: "Production.",
	}})
	if _, err := runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.completeInput.Kind != "reply" {
		t.Fatalf("completion kind = %q, want reply", api.completeInput.Kind)
	}
}

func TestRunnerSuppliesPriorConversationHistory(t *testing.T) {
	api := newFakeRunnerAPI()
	payload, err := encodeTurnPayload(json.RawMessage(`{"machine":"blue"}`), 2, []TurnHistoryEntry{
		{AgentID: "agent_scott", AgentName: "Scott", Kind: "request", Body: "Deploy the service"},
		{AgentID: "agent_bob", AgentName: "Bob", Kind: "question", Body: "Which environment?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	api.message.Payload = payload
	api.listen.Messages[0].CausalDepth = 3
	api.message.CausalDepth = 3
	provider := &fakeProvider{result: TurnResult{Schema: TurnResultSchemaV1, Outcome: OutcomeResult, Body: "Done."}}
	runner := testRunner(api, provider)
	if _, err := runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.envelope.Policy.CurrentTurn != 3 || len(provider.envelope.History) != 2 ||
		provider.envelope.History[0].Body != "Deploy the service" ||
		string(provider.envelope.Message.Payload) != `{"machine":"blue"}` {
		t.Fatalf("provider continuation envelope = %#v", provider.envelope)
	}
}

func TestRunnerIgnoresForgedPayloadTurnCounterForEnforcement(t *testing.T) {
	api := newFakeRunnerAPI()
	payload, err := encodeTurnPayload(nil, 64, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.message.Payload = payload
	provider := &fakeProvider{result: TurnResult{Schema: TurnResultSchemaV1, Outcome: OutcomeResult, Body: "Done."}}
	runner := testRunner(api, provider)
	if _, err := runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.envelope.Policy.CurrentTurn != 1 {
		t.Fatalf("provider current turn = %d, want backend causal depth 1", provider.envelope.Policy.CurrentTurn)
	}
}

func TestRunnerEscalatesWithoutInferenceWhenTurnBudgetIsExhausted(t *testing.T) {
	api := newFakeRunnerAPI()
	payload, err := encodeTurnPayload(nil, 12, []TurnHistoryEntry{{
		AgentID: "agent_scott", AgentName: "Scott", Kind: "request", Body: "Original objective",
	}})
	if err != nil {
		t.Fatal(err)
	}
	api.message.Payload = payload
	api.listen.Messages[0].CausalDepth = 13
	api.message.CausalDepth = 13
	provider := &fakeProvider{err: errors.New("must not be invoked")}
	runner := testRunner(api, provider)
	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls != 0 || result.Status != RunStatusCompleted || api.completeInput.Kind != "escalation" ||
		!strings.Contains(api.completeInput.Body, "turn limit") {
		t.Fatalf("unexpected turn exhaustion: calls=%d result=%+v completion=%+v", provider.calls, result, api.completeInput)
	}
}

func TestRunnerIdleTimeoutDoesNotMutateMailbox(t *testing.T) {
	api := newFakeRunnerAPI()
	api.listen = client.MessageListenResult{TimedOut: true, Messages: []client.Message{}}
	runner := testRunner(api, &fakeProvider{})
	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusIdle {
		t.Fatalf("status = %q, want idle", result.Status)
	}
	if got, want := api.callsSnapshot(), []string{"self", "listen"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestRunnerServeRetriesTransientFailureAndStopsOnCancellation(t *testing.T) {
	api := newFakeRunnerAPI()
	api.listenErrors = []error{errors.New("temporary network failure")}
	api.listen = client.MessageListenResult{TimedOut: true, Messages: []client.Message{}}
	runner := testRunner(api, &fakeProvider{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var observed []error
	err := runner.Serve(ctx, LoopOptions{
		FailureBackoff: time.Millisecond, MaximumBackoff: 2 * time.Millisecond,
		Observe: func(_ RunResult, err error) {
			observed = append(observed, err)
			if len(observed) == 2 {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 || observed[0] == nil || observed[1] != nil {
		t.Fatalf("observed errors = %v, want transient then success", observed)
	}
}

func TestRunnerServeFailsClosedOnIdentityMismatch(t *testing.T) {
	api := newFakeRunnerAPI()
	api.self.Identity.AgentID = "agent_alice"
	runner := testRunner(api, &fakeProvider{})
	err := runner.Serve(context.Background(), LoopOptions{
		FailureBackoff: time.Hour, MaximumBackoff: time.Hour,
	})
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error = %v, want identity mismatch", err)
	}
}

func TestRunnerServeDoesNotObserveIntentionalCancellationAsFailure(t *testing.T) {
	api := newFakeRunnerAPI()
	api.listenUntilCanceled = true
	api.listenStarted = make(chan struct{})
	runner := testRunner(api, &fakeProvider{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-api.listenStarted
		cancel()
	}()
	var observed []error
	if err := runner.Serve(ctx, LoopOptions{Observe: func(_ RunResult, err error) {
		observed = append(observed, err)
	}}); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 0 {
		t.Fatalf("intentional cancellation was observed as a cycle: %v", observed)
	}
}

func TestRunnerServeHonorsMaximumConsecutiveFailures(t *testing.T) {
	api := newFakeRunnerAPI()
	api.listenErrors = []error{errors.New("first"), errors.New("second")}
	runner := testRunner(api, &fakeProvider{})
	err := runner.Serve(context.Background(), LoopOptions{
		FailureBackoff: time.Millisecond, MaximumBackoff: time.Millisecond, MaximumFailures: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "2 consecutive failures") {
		t.Fatalf("error = %v, want bounded failure", err)
	}
}

func testRunner(api API, provider Provider) Runner {
	return Runner{
		API: api, Provider: provider, State: newFakeOperationalState(),
		Config: Config{
			RunnerID: "runner-1",
			ExpectedIdentity: client.SelfIdentity{
				AccountID: "account_1", RealmID: "realm_1", AgentID: "agent_bob", AgentName: "Bob",
			},
			LeaseSeconds:      30,
			ListenWaitSeconds: 1,
		},
	}
}

type fakeOperationalState struct {
	mu            sync.Mutex
	notifications []client.Message
	err           error
}

func newFakeOperationalState() *fakeOperationalState {
	return &fakeOperationalState{}
}

func (s *fakeOperationalState) RecordNotification(_ context.Context, message client.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	for _, existing := range s.notifications {
		if existing.ID == message.ID {
			return nil
		}
	}
	s.notifications = append(s.notifications, message)
	return nil
}

type fakeProvider struct {
	mu       sync.Mutex
	result   TurnResult
	err      error
	waitFor  <-chan struct{}
	envelope TurnEnvelope
	calls    int
}

func (p *fakeProvider) Invoke(ctx context.Context, envelope TurnEnvelope) (TurnResult, error) {
	p.mu.Lock()
	p.calls++
	p.envelope = envelope
	p.mu.Unlock()
	if p.waitFor != nil {
		select {
		case <-p.waitFor:
		case <-ctx.Done():
			return TurnResult{}, ctx.Err()
		}
	}
	return p.result, p.err
}

type fakeRunnerAPI struct {
	mu sync.Mutex

	calls      []string
	self       client.SelfDigest
	listen     client.MessageListenResult
	processing client.MessageProcessing
	message    client.Message
	completed  client.CompleteMessageResult
	renewed    chan struct{}
	renewOnce  sync.Once

	claimInput          client.ClaimMessageInput
	releaseInput        client.MessageClaimInput
	completeInput       client.CompleteMessageInput
	listenErrors        []error
	listenUntilCanceled bool
	listenStarted       chan struct{}
	listenStartOnce     sync.Once
}

func newFakeRunnerAPI() *fakeRunnerAPI {
	identity := client.SelfIdentity{
		AccountID: "account_1", RealmID: "realm_1", AgentID: "agent_bob", AgentName: "Bob",
	}
	metadata := client.Message{
		ID: "msg_1", AccountID: "account_1", RealmID: "realm_1",
		From:    client.MessageAgent{Kind: "agent", AgentID: "agent_scott", AgentName: "Scott"},
		To:      client.MessageRecipient{Kind: "agent", AgentID: "agent_bob", AgentName: "Bob"},
		Subject: "Work", Kind: "request", ThreadID: "thr_1",
		CausalDepth: 1,
	}
	message := metadata
	message.Body = "do the work"
	message.Processing = client.MessageProcessing{State: "claimed", ClaimID: "mcl_1", Generation: 4, FailureCount: 2}
	processing := client.MessageProcessing{State: "claimed", ClaimID: "mcl_1", Generation: 4, FailureCount: 2}
	resultMessage := client.Message{ID: "msg_2", AccountID: "account_1", RealmID: "realm_1"}
	return &fakeRunnerAPI{
		self:       client.SelfDigest{Identity: identity},
		listen:     client.MessageListenResult{Messages: []client.Message{metadata}},
		processing: processing,
		message:    message,
		completed: client.CompleteMessageResult{
			Processing: client.MessageProcessing{State: "completed", ClaimID: "mcl_1", Generation: 4, FailureCount: 2, ResultMessageID: "msg_2"},
			Message:    resultMessage,
		},
		renewed: make(chan struct{}),
	}
}

func (a *fakeRunnerAPI) record(call string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, call)
}

func (a *fakeRunnerAPI) callsSnapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func (a *fakeRunnerAPI) GetSelf(context.Context) (client.SelfDigest, error) {
	a.record("self")
	return a.self, nil
}

func (a *fakeRunnerAPI) ListenMessages(ctx context.Context, _ client.MessageListenOptions) (client.MessageListenResult, error) {
	a.record("listen")
	if a.listenUntilCanceled {
		a.listenStartOnce.Do(func() { close(a.listenStarted) })
		<-ctx.Done()
		return client.MessageListenResult{}, ctx.Err()
	}
	a.mu.Lock()
	if len(a.listenErrors) != 0 {
		err := a.listenErrors[0]
		a.listenErrors = a.listenErrors[1:]
		a.mu.Unlock()
		return client.MessageListenResult{}, err
	}
	a.mu.Unlock()
	return a.listen, nil
}

func (a *fakeRunnerAPI) ClaimMessage(_ context.Context, _ string, in client.ClaimMessageInput) (client.MessageProcessing, error) {
	a.record("claim")
	a.claimInput = in
	return a.processing, nil
}

func (a *fakeRunnerAPI) RenewMessageClaim(_ context.Context, _ string, _ client.RenewMessageClaimInput) (client.MessageProcessing, error) {
	a.record("renew")
	a.renewOnce.Do(func() { close(a.renewed) })
	return a.processing, nil
}

func (a *fakeRunnerAPI) ReleaseMessageClaim(_ context.Context, _ string, in client.MessageClaimInput) (client.MessageProcessing, error) {
	a.record("release")
	a.releaseInput = in
	failureCount := a.processing.FailureCount
	if in.DeterministicFailure {
		failureCount++
	}
	return client.MessageProcessing{State: "available", Generation: in.Generation, FailureCount: failureCount}, nil
}

func (a *fakeRunnerAPI) ReadMessage(context.Context, string) (client.Message, error) {
	a.record("read")
	return a.message, nil
}

func (a *fakeRunnerAPI) CompleteMessage(_ context.Context, _ string, in client.CompleteMessageInput) (client.CompleteMessageResult, error) {
	a.record("complete")
	a.completeInput = in
	return a.completed, nil
}

func (a *fakeRunnerAPI) AckMessage(context.Context, string) (client.Message, error) {
	a.record("ack")
	return client.Message{}, nil
}

func containsCall(calls []string, target string) bool {
	return indexCall(calls, target) >= 0
}

func indexCall(calls []string, target string) int {
	for i, call := range calls {
		if call == target {
			return i
		}
	}
	return -1
}
