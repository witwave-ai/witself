package memorycurator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

var emptyPlan = json.RawMessage(`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}`)
var emptyAcceptedPlan = json.RawMessage(`{"schema":"witself.memory-plan.v1","plan_revision":1,"actions":[]}`)

type plannerFunc func(context.Context, PlannerEnvelope) (json.RawMessage, error)

func (f plannerFunc) Plan(ctx context.Context, envelope PlannerEnvelope) (json.RawMessage, error) {
	return f(ctx, envelope)
}

type inputPage struct {
	inputs []client.MemoryCurationRunInput
	next   string
}

type fakeCurationAPI struct {
	mu sync.Mutex

	request client.MemoryCurationRequest
	run     client.MemoryCurationRun
	pages   map[string]inputPage

	noWork           bool
	listErr          error
	startErr         error
	renewErr         error
	planErr          error
	applyErr         error
	abandonErr       error
	getRunErr        error
	getPlanErr       error
	statusErr        error
	applyBeforeError bool
	planBeforeError  bool
	renewed          chan struct{}
	renewedOnce      sync.Once
	renewSignalAfter int
	listCalls        int
	lastList         client.MemoryCurationRequestListOptions
	startCalls       int
	inputCalls       int
	getPlanCalls     int
	renewCalls       int
	planCalls        int
	applyCalls       int
	abandonCalls     int
	getRunCalls      int
	statusCalls      int
	lastStart        client.StartMemoryCurationInput
	lastPlan         client.PlanMemoryCurationInput
	lastApply        client.ApplyMemoryCurationInput
	lastAbandon      client.FinishMemoryCurationInput
	acceptedPlan     json.RawMessage
	getPlanOverride  json.RawMessage
	callOrder        []string
}

func newFakeCurationAPI() *fakeCurationAPI {
	expires := time.Now().UTC().Add(10 * time.Minute)
	request := client.MemoryCurationRequest{
		ID: "mcrq_test", RequestGeneration: 1, State: "queued",
		Scope: client.MemoryCurationScope{Sources: []string{"memory"}},
	}
	run := client.MemoryCurationRun{
		ID: "mrun_test", RequestID: request.ID, RequestGeneration: request.RequestGeneration,
		FencingGeneration: 1, State: "open", LeaseExpiresAt: &expires,
	}
	return &fakeCurationAPI{
		request:      request,
		run:          run,
		pages:        map[string]inputPage{"": {}},
		acceptedPlan: append(json.RawMessage(nil), emptyAcceptedPlan...),
	}
}

func (f *fakeCurationAPI) ListRequests(_ context.Context, opts client.MemoryCurationRequestListOptions) (*client.MemoryCurationRequestPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	f.lastList = opts
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.noWork {
		return &client.MemoryCurationRequestPage{Requests: []client.MemoryCurationRequest{}}, nil
	}
	return &client.MemoryCurationRequestPage{Requests: []client.MemoryCurationRequest{f.request}}, nil
}

func (f *fakeCurationAPI) Start(_ context.Context, in client.StartMemoryCurationInput) (*client.StartMemoryCurationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	f.lastStart = in
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &client.StartMemoryCurationResult{Run: f.run, Request: f.request}, nil
}

func (f *fakeCurationAPI) GetInputs(_ context.Context, _ string, _ int64, cursor string, _ int) (*client.MemoryCurationRunInputPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputCalls++
	f.callOrder = append(f.callOrder, "inputs")
	page, ok := f.pages[cursor]
	if !ok {
		return nil, fmt.Errorf("unexpected cursor %q", cursor)
	}
	inputs := append([]client.MemoryCurationRunInput(nil), page.inputs...)
	return &client.MemoryCurationRunInputPage{Run: f.run, Inputs: inputs, NextCursor: page.next}, nil
}

func (f *fakeCurationAPI) GetPlan(_ context.Context, runID string, fence int64) (*client.GetMemoryCurationPlanResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getPlanCalls++
	f.callOrder = append(f.callOrder, "get-plan")
	if f.getPlanErr != nil {
		return nil, f.getPlanErr
	}
	if runID != f.run.ID || fence != f.run.FencingGeneration {
		return nil, errors.New("unexpected plan review coordinates")
	}
	plan := f.acceptedPlan
	if f.getPlanOverride != nil {
		plan = f.getPlanOverride
	}
	return &client.GetMemoryCurationPlanResult{
		Run:  f.run,
		Plan: append(json.RawMessage(nil), plan...),
	}, nil
}

func (f *fakeCurationAPI) Renew(_ context.Context, in client.RenewMemoryCurationInput) (*client.RenewMemoryCurationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewCalls++
	if f.renewErr != nil {
		return nil, f.renewErr
	}
	expires := time.Now().UTC().Add(in.Extension)
	f.run.LeaseExpiresAt = &expires
	if f.renewed != nil && (f.renewSignalAfter == 0 || f.renewCalls >= f.renewSignalAfter) {
		f.renewedOnce.Do(func() { close(f.renewed) })
	}
	return &client.RenewMemoryCurationResult{Run: f.run}, nil
}

func (f *fakeCurationAPI) Plan(_ context.Context, in client.PlanMemoryCurationInput) (*client.PlanMemoryCurationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.planCalls++
	f.callOrder = append(f.callOrder, "plan")
	f.lastPlan = in
	if f.planErr != nil && !f.planBeforeError {
		return nil, f.planErr
	}
	hash := strings.Repeat("a", 64)
	f.run.State = "planned"
	f.run.PlanSchema = MemoryPlanSchemaV1
	f.run.PlanRevision = 1
	f.run.PlanHash = hash
	var draft struct {
		Schema  string          `json:"schema"`
		Actions json.RawMessage `json:"actions"`
	}
	if err := json.Unmarshal(in.Draft, &draft); err != nil {
		return nil, err
	}
	f.acceptedPlan, _ = json.Marshal(struct {
		Schema       string          `json:"schema"`
		PlanRevision int64           `json:"plan_revision"`
		Actions      json.RawMessage `json:"actions"`
	}{Schema: draft.Schema, PlanRevision: 1, Actions: draft.Actions})
	result := &client.PlanMemoryCurationResult{
		Run:  f.run,
		Plan: append(json.RawMessage(nil), f.acceptedPlan...),
		Receipt: client.MemoryCurationPlanReceipt{
			ID: "mplan_test", Operation: "plan", RequestID: f.run.RequestID,
			RunID: f.run.ID, RequestGeneration: f.run.RequestGeneration,
			FencingGeneration: f.run.FencingGeneration, PlanSchema: MemoryPlanSchemaV1,
			PlanRevision: 1, PlanHash: hash, ResultState: "planned",
		},
	}
	if f.planErr != nil {
		return nil, f.planErr
	}
	return result, nil
}

func (f *fakeCurationAPI) Apply(_ context.Context, in client.ApplyMemoryCurationInput) (*client.ApplyMemoryCurationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applyCalls++
	f.callOrder = append(f.callOrder, "apply")
	f.lastApply = in
	if f.applyErr != nil && !f.applyBeforeError {
		return nil, f.applyErr
	}
	f.run.State = "applied"
	f.run.ApplyReceiptID = "mapply_test"
	result := &client.ApplyMemoryCurationResult{
		Run: f.run,
		Receipt: client.MemoryCurationApplyReceipt{
			ID: "mapply_test", RunID: f.run.ID, FencingGeneration: f.run.FencingGeneration,
			PlanRevision: in.PlanRevision, PlanHash: in.PlanHash,
		},
	}
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return result, nil
}

func (f *fakeCurationAPI) Abandon(_ context.Context, in client.FinishMemoryCurationInput) (*client.FinishMemoryCurationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abandonCalls++
	f.lastAbandon = in
	if f.abandonErr != nil {
		return nil, f.abandonErr
	}
	f.run.State = "abandoned"
	f.run.LeaseExpiresAt = nil
	return &client.FinishMemoryCurationResult{Run: f.run}, nil
}

func (f *fakeCurationAPI) GetRun(context.Context, string) (*client.MemoryCurationRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getRunCalls++
	if f.getRunErr != nil {
		return nil, f.getRunErr
	}
	run := f.run
	return &run, nil
}

func (f *fakeCurationAPI) Status(context.Context, string) (*client.MemoryCurationStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	request, run := f.request, f.run
	return &client.MemoryCurationStatus{Request: &request, Run: &run}, nil
}

type memoryStateStore struct {
	mu     sync.Mutex
	states map[string]LaunchState
}

func newMemoryStateStore() *memoryStateStore {
	return &memoryStateStore{states: make(map[string]LaunchState)}
}

func (s *memoryStateStore) Save(state LaunchState) error {
	if err := validateLaunchState(state); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[state.LaunchID] = state
	return nil
}

func (s *memoryStateStore) Load(launchID string) (LaunchState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[launchID]
	if !ok {
		return LaunchState{}, os.ErrNotExist
	}
	return state, nil
}

func testRunner(api API, planner Planner, state StateStore) Runner {
	return Runner{
		API: api, Planner: planner, State: state,
		NewID: func(string) (string, error) { return "curl_test", nil },
	}
}

func validLaunchState(phase string) LaunchState {
	now := time.Now().UTC()
	return LaunchState{
		Schema: LaunchStateSchemaV1, LaunchID: "curl_resume", Phase: phase,
		ApplyPolicy: ApplyPolicyPreview, RequestID: "mcrq_test", RequestGeneration: 1,
		RunID: "mrun_test", FencingGeneration: 1,
		Caps: client.MemoryCurationInputCaps{
			MaxMemories: 20, MaxEvidence: 50, MaxTranscriptEntries: 100,
		},
		PageSize: 50, LeaseSeconds: 300, PlannerTimeoutSeconds: 240,
		RenewBeforeSeconds: 60, MaximumActions: 32,
		StartKey: "curl_resume:start", PlanKey: "curl_resume:plan:1",
		ApplyKey: "curl_resume:apply", AbandonKey: "curl_resume:abandon",
		PlanAttempt: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func TestRunnerNoDueWork(t *testing.T) {
	api := newFakeCurationAPI()
	api.noWork = true
	var plannerCalls atomic.Int32
	runner := testRunner(api, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		plannerCalls.Add(1)
		return emptyPlan, nil
	}), newMemoryStateStore())

	result, err := runner.Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.NoWork || plannerCalls.Load() != 0 || api.startCalls != 0 {
		t.Fatalf("unexpected no-work result: %#v, planner=%d start=%d", result, plannerCalls.Load(), api.startCalls)
	}
	if api.lastList.Limit != 1 || !api.lastList.ExcludeSensitive {
		t.Fatalf("default due selection = %#v", api.lastList)
	}
}

func TestRunnerDueSelectionTracksSensitivePolicy(t *testing.T) {
	api := newFakeCurationAPI()
	api.noWork = true
	runner := testRunner(api, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		t.Fatal("planner must not run without due work")
		return nil, nil
	}), newMemoryStateStore())

	result, err := runner.Run(context.Background(), Options{AllowSensitive: true})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.NoWork || api.lastList.Limit != 1 || api.lastList.ExcludeSensitive {
		t.Fatalf("sensitive-enabled due selection = result %#v, options %#v", result, api.lastList)
	}
}

func TestRunnerNonSensitivePolicyAcceptsFullCredentialTranscriptWork(t *testing.T) {
	api := newFakeCurationAPI()
	api.request.Scope.Sources = []string{"transcript"}
	state := newMemoryStateStore()
	runner := testRunner(api, plannerFunc(func(_ context.Context, envelope PlannerEnvelope) (json.RawMessage, error) {
		if envelope.Policy.IncludeSensitive {
			t.Fatal("transcript-only work must not enable explicit sensitive inputs")
		}
		return emptyPlan, nil
	}), state)

	result, err := runner.Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Request == nil || len(result.Request.Scope.Sources) != 1 ||
		result.Request.Scope.Sources[0] != "transcript" || result.Abandon == nil {
		t.Fatalf("transcript preview result = %#v", result)
	}
	if !api.lastList.ExcludeSensitive {
		t.Fatalf("transcript due selection = %#v", api.lastList)
	}
}

func TestRunnerPreviewPagesAllInputsAndRequeues(t *testing.T) {
	api := newFakeCurationAPI()
	api.run.InputCount = 2
	api.pages = map[string]inputPage{
		"": {
			inputs: []client.MemoryCurationRunInput{{RunID: api.run.ID, Ordinal: 1, Kind: "cursor"}},
			next:   "page-2",
		},
		"page-2": {
			inputs: []client.MemoryCurationRunInput{{RunID: api.run.ID, Ordinal: 2, Kind: "cursor"}},
		},
	}
	state := newMemoryStateStore()
	var got PlannerEnvelope
	runner := testRunner(api, plannerFunc(func(_ context.Context, envelope PlannerEnvelope) (json.RawMessage, error) {
		got = envelope
		return emptyPlan, nil
	}), state)

	result, err := runner.Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Plan == nil || result.Abandon == nil || result.Apply != nil || result.InputCount != 2 {
		t.Fatalf("unexpected preview result: %#v", result)
	}
	if got.Schema != PlannerEnvelopeSchemaV1 || got.Policy.PlanSchema != MemoryPlanSchemaV1 || got.Policy.IncludeSensitive || len(got.MaterializedInputs) != 2 {
		t.Fatalf("unexpected planner envelope: %#v", got)
	}
	if api.inputCalls != 2 || api.planCalls != 1 || api.applyCalls != 0 || api.abandonCalls != 1 || api.lastAbandon.Reason != "preview_complete" {
		t.Fatalf("unexpected API calls: inputs=%d plan=%d apply=%d abandon=%d reason=%q", api.inputCalls, api.planCalls, api.applyCalls, api.abandonCalls, api.lastAbandon.Reason)
	}
	saved, err := state.Load("curl_test")
	if err != nil || saved.Phase != PhasePreviewed || saved.InputCount != 2 {
		t.Fatalf("unexpected saved state: %#v, err=%v", saved, err)
	}
}

func TestRunnerAppliesOnlyWithExplicitApproval(t *testing.T) {
	api := newFakeCurationAPI()
	api.getPlanOverride = json.RawMessage(`{"actions":[],"plan_revision":1,"schema":"witself.memory-plan.v1"}`)
	state := newMemoryStateStore()
	runner := testRunner(api, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		return emptyPlan, nil
	}), state)

	result, err := runner.Run(context.Background(), Options{ApplyPolicy: ApplyPolicyApply, ApproveApply: true})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Apply == nil || result.Abandon != nil || api.getPlanCalls != 1 || api.applyCalls != 1 || api.abandonCalls != 0 {
		t.Fatalf("unexpected apply result: %#v calls apply=%d abandon=%d", result, api.applyCalls, api.abandonCalls)
	}
	if got := strings.Join(api.callOrder, ","); got != "inputs,plan,get-plan,apply" {
		t.Fatalf("apply call order = %q", got)
	}
	if api.lastApply.PlanRevision != 1 || api.lastApply.PlanHash != strings.Repeat("a", 64) {
		t.Fatalf("apply did not use accepted plan coordinates: %#v", api.lastApply)
	}

	api2 := newFakeCurationAPI()
	runner2 := testRunner(api2, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		return emptyPlan, nil
	}), newMemoryStateStore())
	_, err = runner2.Run(context.Background(), Options{ApplyPolicy: ApplyPolicyApply})
	if !errors.Is(err, ErrApplyNotApproved) || api2.listCalls != 0 {
		t.Fatalf("unapproved apply error = %v, list calls = %d", err, api2.listCalls)
	}
}

func TestRunnerNeverAppliesWhenAcceptedPlanReviewDiffers(t *testing.T) {
	api := newFakeCurationAPI()
	api.getPlanOverride = json.RawMessage(`{"schema":"witself.memory-plan.v1","plan_revision":1,"actions":[{"ordinal":1,"operation":"relate","relate":{}}]}`)
	runner := testRunner(api, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		return emptyPlan, nil
	}), newMemoryStateStore())

	result, err := runner.Run(context.Background(), Options{ApplyPolicy: ApplyPolicyApply, ApproveApply: true})
	if !errors.Is(err, ErrProtocolResponse) || result.Abandon == nil || api.getPlanCalls != 1 || api.applyCalls != 0 || api.abandonCalls != 1 {
		t.Fatalf("mismatched review result=%#v err=%v get-plan=%d apply=%d abandon=%d", result, err, api.getPlanCalls, api.applyCalls, api.abandonCalls)
	}
}

func TestRunnerRenewsLeaseWhilePlannerRuns(t *testing.T) {
	api := newFakeCurationAPI()
	api.renewed = make(chan struct{})
	api.renewSignalAfter = 2 // the first renewal precedes input paging
	runner := testRunner(api, plannerFunc(func(ctx context.Context, _ PlannerEnvelope) (json.RawMessage, error) {
		select {
		case <-api.renewed:
			return emptyPlan, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}), newMemoryStateStore())

	result, err := runner.Run(context.Background(), Options{
		LeaseDuration: 2 * time.Second, RenewBefore: time.Second, PlannerTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Abandon == nil || api.renewCalls < 1 {
		t.Fatalf("lease was not renewed: result=%#v renew calls=%d", result, api.renewCalls)
	}
}

func TestRunnerLeaseRenewalIgnoresClientWallClockSkew(t *testing.T) {
	api := newFakeCurationAPI()
	state := newMemoryStateStore()
	runner := testRunner(api, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		return emptyPlan, nil
	}), state)
	runner.Now = func() time.Time { return time.Now().UTC().Add(48 * time.Hour) }

	result, err := runner.Run(context.Background(), Options{})
	if err != nil || result.Abandon == nil || api.renewCalls < 1 {
		t.Fatalf("skewed client clock affected backend-authoritative renewal: result=%#v err=%v renew=%d", result, err, api.renewCalls)
	}
}

func TestRunnerRejectsEmptyInputPageWithAdvancingCursor(t *testing.T) {
	api := newFakeCurationAPI()
	api.pages = map[string]inputPage{"": {next: "unbounded-empty-page"}}
	runner := testRunner(api, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		return emptyPlan, nil
	}), newMemoryStateStore())

	result, err := runner.Run(context.Background(), Options{})
	if !errors.Is(err, ErrProtocolResponse) || result.Abandon == nil || api.inputCalls != 1 || api.planCalls != 0 {
		t.Fatalf("empty cursor page result=%#v err=%v inputs=%d plan=%d", result, err, api.inputCalls, api.planCalls)
	}
}

func TestRunnerPlannerFailuresAbandonWithoutMutation(t *testing.T) {
	tests := []struct {
		name    string
		planner Planner
		options Options
	}{
		{
			name: "error",
			planner: plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
				return nil, errors.New("model unavailable")
			}),
		},
		{
			name: "invalid output",
			planner: plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
				return json.RawMessage(`{"schema":"wrong","draft_revision":1,"actions":[]}`), nil
			}),
		},
		{
			name: "policy action cap",
			planner: plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
				return json.RawMessage(`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[{"ordinal":1,"operation":"relate","relate":{}},{"ordinal":2,"operation":"relate","relate":{}}]}`), nil
			}),
			options: Options{MaximumActions: 1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newFakeCurationAPI()
			state := newMemoryStateStore()
			runner := testRunner(api, test.planner, state)
			result, err := runner.Run(context.Background(), test.options)
			if err == nil || result.Abandon == nil || api.planCalls != 0 || api.applyCalls != 0 || api.abandonCalls != 1 {
				t.Fatalf("unexpected failure result: result=%#v err=%v calls plan=%d apply=%d abandon=%d", result, err, api.planCalls, api.applyCalls, api.abandonCalls)
			}
			saved, loadErr := state.Load("curl_test")
			if loadErr != nil || saved.Phase != PhaseAbandoned {
				t.Fatalf("unexpected failure state: %#v, err=%v", saved, loadErr)
			}
		})
	}
}

func TestRunnerPlannerTimeoutAbandons(t *testing.T) {
	api := newFakeCurationAPI()
	runner := testRunner(api, plannerFunc(func(ctx context.Context, _ PlannerEnvelope) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}), newMemoryStateStore())

	result, err := runner.Run(context.Background(), Options{
		LeaseDuration: 3 * time.Second, RenewBefore: time.Second, PlannerTimeout: time.Second,
	})
	if !errors.Is(err, context.DeadlineExceeded) || result.Abandon == nil || api.abandonCalls != 1 || api.applyCalls != 0 {
		t.Fatalf("timeout result=%#v err=%v abandon=%d apply=%d", result, err, api.abandonCalls, api.applyCalls)
	}
}

func TestRunnerRecoversAcceptedPlanWithoutReplanning(t *testing.T) {
	api := newFakeCurationAPI()
	api.run.State = "planned"
	api.run.PlanRevision = 1
	api.run.PlanHash = strings.Repeat("b", 64)
	state := newMemoryStateStore()
	if err := state.Save(validLaunchState(PhasePlanning)); err != nil {
		t.Fatal(err)
	}
	var plannerCalls atomic.Int32
	runner := testRunner(api, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		plannerCalls.Add(1)
		return emptyPlan, nil
	}), state)

	result, err := runner.Resume(context.Background(), "curl_resume", Options{})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if !result.Recovered || result.Abandon == nil || plannerCalls.Load() != 0 || api.planCalls != 0 || api.abandonCalls != 1 {
		t.Fatalf("unexpected recovery: %#v planner=%d plan=%d abandon=%d", result, plannerCalls.Load(), api.planCalls, api.abandonCalls)
	}
}

func TestRunnerResumedPlannedApplyPagesInputsReviewsAndNeverReplans(t *testing.T) {
	api := newFakeCurationAPI()
	api.run.State = "planned"
	api.run.PlanSchema = MemoryPlanSchemaV1
	api.run.PlanRevision = 1
	api.run.PlanHash = strings.Repeat("a", 64)
	api.run.InputCount = 2
	api.pages = map[string]inputPage{
		"": {
			inputs: []client.MemoryCurationRunInput{{RunID: api.run.ID, Ordinal: 1, Kind: "cursor"}},
			next:   "page-2",
		},
		"page-2": {
			inputs: []client.MemoryCurationRunInput{{RunID: api.run.ID, Ordinal: 2, Kind: "cursor"}},
		},
	}
	state := newMemoryStateStore()
	launch := validLaunchState(PhasePlanned)
	launch.ApplyPolicy = ApplyPolicyApply
	launch.InputCount = 2
	launch.PlanRevision = 1
	launch.PlanHash = api.run.PlanHash
	launch.PlanReceiptID = "mplan_same_launch"
	if err := state.Save(launch); err != nil {
		t.Fatal(err)
	}
	var plannerCalls atomic.Int32
	runner := testRunner(api, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		plannerCalls.Add(1)
		return emptyPlan, nil
	}), state)

	result, err := runner.Resume(context.Background(), launch.LaunchID, Options{ApproveApply: true})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if result.Apply == nil || result.InputCount != 2 || plannerCalls.Load() != 0 || api.planCalls != 0 ||
		api.inputCalls != 2 || api.getPlanCalls != 1 || api.applyCalls != 1 {
		t.Fatalf("resumed apply=%#v planner=%d plan=%d inputs=%d get-plan=%d apply=%d",
			result, plannerCalls.Load(), api.planCalls, api.inputCalls, api.getPlanCalls, api.applyCalls)
	}
	if got := strings.Join(api.callOrder, ","); got != "inputs,inputs,get-plan,apply" {
		t.Fatalf("resumed apply call order = %q", got)
	}
}

func TestRunnerNeverAppliesForeignPlannedRunWithoutSameLaunchReceipt(t *testing.T) {
	api := newFakeCurationAPI()
	api.run.State = "planned"
	api.run.PlanSchema = MemoryPlanSchemaV1
	api.run.PlanRevision = 1
	api.run.PlanHash = strings.Repeat("b", 64)
	state := newMemoryStateStore()
	launch := validLaunchState(PhasePlanning)
	launch.ApplyPolicy = ApplyPolicyApply
	if err := state.Save(launch); err != nil {
		t.Fatal(err)
	}
	var plannerCalls atomic.Int32
	runner := testRunner(api, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		plannerCalls.Add(1)
		return emptyPlan, nil
	}), state)

	result, err := runner.Resume(context.Background(), launch.LaunchID, Options{ApproveApply: true})
	if !errors.Is(err, ErrProtocolResponse) || result.Abandon == nil || plannerCalls.Load() != 0 ||
		api.inputCalls != 0 || api.getPlanCalls != 0 || api.planCalls != 0 || api.applyCalls != 0 {
		t.Fatalf("foreign plan result=%#v err=%v planner=%d inputs=%d get-plan=%d plan=%d apply=%d",
			result, err, plannerCalls.Load(), api.inputCalls, api.getPlanCalls, api.planCalls, api.applyCalls)
	}
}

func TestRunnerNeverRebasesTerminalRun(t *testing.T) {
	api := newFakeCurationAPI()
	api.run.State = "interrupted"
	api.run.LeaseExpiresAt = nil
	state := newMemoryStateStore()
	if err := state.Save(validLaunchState(PhasePlanning)); err != nil {
		t.Fatal(err)
	}
	var plannerCalls atomic.Int32
	runner := testRunner(api, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		plannerCalls.Add(1)
		return emptyPlan, nil
	}), state)

	result, err := runner.Resume(context.Background(), "curl_resume", Options{})
	if !errors.Is(err, ErrCurationTerminal) || result.Run == nil || result.Run.State != "interrupted" || plannerCalls.Load() != 0 || api.startCalls != 0 || api.planCalls != 0 {
		t.Fatalf("terminal recovery rebased work: result=%#v err=%v planner=%d start=%d plan=%d", result, err, plannerCalls.Load(), api.startCalls, api.planCalls)
	}
}

func TestRunnerRecoveryFinishesRecordedFailure(t *testing.T) {
	api := newFakeCurationAPI()
	state := newMemoryStateStore()
	launch := validLaunchState(PhaseFailureAbandoning)
	launch.AbandonReason = "planner_failed"
	if err := state.Save(launch); err != nil {
		t.Fatal(err)
	}
	var plannerCalls atomic.Int32
	runner := testRunner(api, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		plannerCalls.Add(1)
		return emptyPlan, nil
	}), state)

	result, err := runner.Resume(context.Background(), "curl_resume", Options{})
	if err == nil || result.Abandon == nil || plannerCalls.Load() != 0 || api.inputCalls != 0 || api.abandonCalls != 1 || api.lastAbandon.Reason != "planner_failed" {
		t.Fatalf("unexpected failure recovery: result=%#v err=%v planner=%d input=%d abandon=%d", result, err, plannerCalls.Load(), api.inputCalls, api.abandonCalls)
	}
}

func TestRunnerRecoveryRefusesSensitiveInputsWithoutFreshApproval(t *testing.T) {
	api := newFakeCurationAPI()
	api.request.Scope.IncludeSensitive = true
	state := newMemoryStateStore()
	launch := validLaunchState(PhaseStarted)
	launch.IncludesSensitive = true
	if err := state.Save(launch); err != nil {
		t.Fatal(err)
	}
	var plannerCalls atomic.Int32
	runner := testRunner(api, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		plannerCalls.Add(1)
		return emptyPlan, nil
	}), state)

	result, err := runner.Resume(context.Background(), "curl_resume", Options{})
	if !errors.Is(err, ErrSensitiveRequest) || result.Abandon == nil || plannerCalls.Load() != 0 || api.inputCalls != 0 {
		t.Fatalf("unexpected sensitive recovery: result=%#v err=%v planner=%d inputs=%d", result, err, plannerCalls.Load(), api.inputCalls)
	}
}

func TestRunnerRecoversApplyReceiptAfterLostResponse(t *testing.T) {
	api := newFakeCurationAPI()
	api.applyErr = errors.New("connection reset")
	api.applyBeforeError = true
	state := newMemoryStateStore()
	runner := testRunner(api, plannerFunc(func(context.Context, PlannerEnvelope) (json.RawMessage, error) {
		return emptyPlan, nil
	}), state)

	result, err := runner.Run(context.Background(), Options{ApplyPolicy: ApplyPolicyApply, ApproveApply: true})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Run == nil || result.Run.State != "applied" || api.getRunCalls != 1 || api.abandonCalls != 0 {
		t.Fatalf("unexpected apply recovery: %#v get=%d abandon=%d", result, api.getRunCalls, api.abandonCalls)
	}
	saved, loadErr := state.Load("curl_test")
	if loadErr != nil || saved.Phase != PhaseApplied || saved.ApplyReceiptID != "mapply_test" {
		t.Fatalf("unexpected recovered apply state: %#v err=%v", saved, loadErr)
	}
}
