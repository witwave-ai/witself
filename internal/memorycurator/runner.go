package memorycurator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/id"
)

// ApplyPolicy controls whether a completed curation plan is previewed or applied.
type ApplyPolicy string

const (
	// ApplyPolicyPreview validates a plan and then abandons the run without applying it.
	ApplyPolicyPreview ApplyPolicy = "preview"
	// ApplyPolicyApply applies an approved plan to durable memory.
	ApplyPolicyApply ApplyPolicy = "apply"
)

const (
	defaultMaxMemories          = 20
	defaultMaxEvidence          = 50
	defaultMaxTranscriptEntries = 100
	defaultPageSize             = 50
	defaultMaximumActions       = 32
	defaultLeaseDuration        = 5 * time.Minute
	defaultPlannerTimeout       = 4 * time.Minute
	defaultRenewBefore          = 1 * time.Minute
	cleanupTimeout              = 10 * time.Second
	maximumCollectedInputs      = 10_000
)

var (
	// ErrApplyNotApproved indicates that apply mode lacks explicit local approval.
	ErrApplyNotApproved = errors.New("curator apply policy requires explicit approval")
	// ErrSensitiveRequest indicates that local policy forbids a request's sensitive inputs.
	ErrSensitiveRequest = errors.New("curator request includes sensitive inputs but the local policy does not")
	// ErrCurationTerminal indicates that a terminal run cannot be resumed or rebased.
	ErrCurationTerminal = errors.New("curation run is terminal and will not be rebased")
	// ErrProtocolResponse indicates that the server returned inconsistent curation state.
	ErrProtocolResponse = errors.New("invalid curation protocol response")
)

// Options configures one curation launch and its local safety limits.
type Options struct {
	RequestID      string
	ApplyPolicy    ApplyPolicy
	ApproveApply   bool
	AllowSensitive bool
	Caps           client.MemoryCurationInputCaps
	PageSize       int
	MaximumActions int
	LeaseDuration  time.Duration
	PlannerTimeout time.Duration
	RenewBefore    time.Duration
	Client         client.MemoryClientProvenance
}

// Result records the server objects and outcome produced by a curation launch.
type Result struct {
	LaunchID   string
	NoWork     bool
	Recovered  bool
	Request    *client.MemoryCurationRequest
	Run        *client.MemoryCurationRun
	InputCount int
	Plan       *client.PlanMemoryCurationResult
	Apply      *client.ApplyMemoryCurationResult
	Abandon    *client.FinishMemoryCurationResult
}

// Runner owns curation transport and retry state. Planner is intentionally
// invoked only after all inputs have been frozen and paged from one fenced run.
type Runner struct {
	API     API
	Planner Planner
	State   StateStore
	Now     func() time.Time
	NewID   func(string) (string, error)
}

// Run selects one due or specified request and drives its curation protocol.
func (r Runner) Run(ctx context.Context, options Options) (Result, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return Result{}, err
	}
	if err := r.validate(); err != nil {
		return Result{}, err
	}

	requestID := strings.TrimSpace(options.RequestID)
	var selected *client.MemoryCurationRequest
	if requestID == "" {
		page, err := r.API.ListRequests(ctx, client.MemoryCurationRequestListOptions{
			Limit: 1, ExcludeSensitive: !options.AllowSensitive,
		})
		if err != nil {
			return Result{}, fmt.Errorf("list due curation work: %w", err)
		}
		if page == nil {
			return Result{}, fmt.Errorf("%w: due request list is missing", ErrProtocolResponse)
		}
		if len(page.Requests) == 0 {
			return Result{NoWork: true}, nil
		}
		request := page.Requests[0]
		selected = &request
		requestID = request.ID
		if request.Scope.IncludeSensitive && !options.AllowSensitive {
			return Result{}, ErrSensitiveRequest
		}
	}
	if requestID == "" {
		return Result{}, fmt.Errorf("%w: due request has no id", ErrProtocolResponse)
	}

	launchID, err := r.newID("curl")
	if err != nil {
		return Result{}, fmt.Errorf("create curator launch id: %w", err)
	}
	now := r.now()
	state := LaunchState{
		Schema: LaunchStateSchemaV1, LaunchID: launchID, Phase: PhaseStarting,
		ApplyPolicy: options.ApplyPolicy, RequestID: requestID,
		Caps: options.Caps, PageSize: options.PageSize,
		LeaseSeconds:          int64(options.LeaseDuration / time.Second),
		PlannerTimeoutSeconds: int64(options.PlannerTimeout / time.Second),
		RenewBeforeSeconds:    int64(options.RenewBefore / time.Second),
		MaximumActions:        options.MaximumActions,
		StartKey:              launchID + ":start", PlanKey: launchID + ":plan:1",
		ApplyKey: launchID + ":apply", AbandonKey: launchID + ":abandon",
		PlanAttempt: 1, Client: options.Client, CreatedAt: now, UpdatedAt: now,
	}
	if selected != nil {
		state.RequestGeneration = selected.RequestGeneration
		state.IncludesSensitive = selected.Scope.IncludeSensitive
	}
	if err := r.save(&state); err != nil {
		return Result{}, err
	}
	result := Result{LaunchID: launchID, Request: selected}
	return r.startAndExecute(ctx, &state, options, result)
}

// Resume recovers only value-free protocol state. It never reconstructs inputs
// or a plan from disk; open runs re-page their immutable inputs and reason
// again, while planned/applied runs recover their server state.
func (r Runner) Resume(ctx context.Context, launchID string, options Options) (Result, error) {
	if err := r.validate(); err != nil {
		return Result{}, err
	}
	state, err := r.State.Load(launchID)
	if err != nil {
		return Result{}, err
	}
	options = optionsFromState(state, options)
	options, err = normalizeOptions(options)
	if err != nil {
		return Result{}, err
	}
	if options.ApplyPolicy != state.ApplyPolicy {
		return Result{}, errors.New("resume cannot change the launch apply policy")
	}
	result := Result{LaunchID: launchID, Recovered: true}
	if state.RunID == "" {
		return r.startAndExecute(ctx, &state, options, result)
	}

	status, statusErr := r.API.Status(ctx, state.RunID)
	run, runErr := r.API.GetRun(ctx, state.RunID)
	if runErr != nil {
		if statusErr != nil {
			return result, errors.Join(fmt.Errorf("recover curation status: %w", statusErr), fmt.Errorf("recover curation run: %w", runErr))
		}
		return result, fmt.Errorf("recover curation run: %w", runErr)
	}
	if statusErr == nil && status != nil {
		result.Request = status.Request
		if status.Run != nil && status.Run.ID != run.ID {
			return result, fmt.Errorf("%w: status returned a different run", ErrProtocolResponse)
		}
	}
	if run.ID != state.RunID || run.FencingGeneration != state.FencingGeneration {
		return result, fmt.Errorf("%w: recovered run does not match launch fence", ErrProtocolResponse)
	}
	result.Run = run
	if result.Request == nil {
		result.Request = &client.MemoryCurationRequest{ID: run.RequestID, RequestGeneration: run.RequestGeneration}
	}
	if (run.State == "open" || run.State == "planned") && (state.IncludesSensitive || result.Request.Scope.IncludeSensitive) && !options.AllowSensitive {
		return r.failAndAbandon(ctx, &state, result, ErrSensitiveRequest, "sensitive_scope_refused")
	}
	persistedPhase := state.Phase
	switch run.State {
	case "open":
		if persistedPhase == PhaseFailureAbandoning {
			return r.failAndAbandon(ctx, &state, result, errors.New("recovering interrupted failure cleanup"), "recovered_failure")
		}
		if persistedPhase == PhasePreviewAbandoning || persistedPhase == PhasePreviewed || persistedPhase == PhaseAbandoned || persistedPhase == PhaseApplied || persistedPhase == PhaseTerminal {
			return result, fmt.Errorf("%w: open run conflicts with persisted phase %s", ErrProtocolResponse, persistedPhase)
		}
		if state.Phase == PhasePlanning {
			state.PlanAttempt++
			state.PlanKey = fmt.Sprintf("%s:plan:%d", state.LaunchID, state.PlanAttempt)
		}
		state.Phase = PhaseStarted
		state.LeaseExpiresAt = run.LeaseExpiresAt
		if err := r.save(&state); err != nil {
			return result, err
		}
		return r.executeOpen(ctx, &state, options, result, run)
	case "planned":
		if persistedPhase == PhaseFailureAbandoning {
			return r.failAndAbandon(ctx, &state, result, errors.New("recovering interrupted failure cleanup"), "recovered_failure")
		}
		if persistedPhase == PhaseApplied || persistedPhase == PhaseAbandoned || persistedPhase == PhaseTerminal {
			return result, fmt.Errorf("%w: planned run conflicts with persisted phase %s", ErrProtocolResponse, persistedPhase)
		}
		if run.PlanRevision < 1 || !planHashPattern.MatchString(run.PlanHash) {
			return r.failAndAbandon(ctx, &state, result, fmt.Errorf("%w: recovered planned run lacks revision and hash", ErrProtocolResponse), "plan_response_invalid")
		}
		state.PlanRevision, state.PlanHash = run.PlanRevision, run.PlanHash
		state.Phase = PhasePlanned
		if err := r.save(&state); err != nil {
			return result, err
		}
		return r.finishPlanned(ctx, &state, options, result, run, nil)
	case "applied":
		if run.ApplyReceiptID == "" || run.PlanRevision < 1 || !planHashPattern.MatchString(run.PlanHash) {
			return result, fmt.Errorf("%w: recovered applied run lacks receipt and plan coordinates", ErrProtocolResponse)
		}
		state.Phase, state.ApplyReceiptID = PhaseApplied, run.ApplyReceiptID
		if err := r.save(&state); err != nil {
			return result, err
		}
		return result, nil
	case "abandoned":
		if state.Phase == PhasePreviewAbandoning || state.Phase == PhasePreviewed {
			state.Phase = PhasePreviewed
			if err := r.save(&state); err != nil {
				return result, err
			}
			return result, nil
		}
		state.Phase = PhaseAbandoned
		_ = r.save(&state)
		return result, fmt.Errorf("%w: run was abandoned", ErrCurationTerminal)
	default:
		state.Phase = PhaseTerminal
		_ = r.save(&state)
		return result, fmt.Errorf("%w: run state is %s", ErrCurationTerminal, run.State)
	}
}

func (r Runner) startAndExecute(ctx context.Context, state *LaunchState, options Options, result Result) (Result, error) {
	budgets, _ := json.Marshal(map[string]int64{
		"maximum_actions":         int64(options.MaximumActions),
		"planner_timeout_seconds": int64(options.PlannerTimeout / time.Second),
	})
	started, err := r.API.Start(ctx, client.StartMemoryCurationInput{
		RequestID: state.RequestID, Caps: options.Caps, LeaseDuration: options.LeaseDuration,
		Client: options.Client, Budgets: budgets, IdempotencyKey: state.StartKey,
	})
	if err != nil {
		return result, fmt.Errorf("start curation: %w", err)
	}
	if started == nil || started.Run.ID == "" || started.Run.RequestID != state.RequestID || started.Request.ID != state.RequestID ||
		started.Run.RequestGeneration != started.Request.RequestGeneration || started.Run.FencingGeneration < 1 || started.Run.State != "open" || started.Run.LeaseExpiresAt == nil {
		return result, fmt.Errorf("%w: start returned incomplete run coordinates", ErrProtocolResponse)
	}
	if started.Request.Scope.IncludeSensitive && !options.AllowSensitive {
		state.RunID, state.FencingGeneration, state.IncludesSensitive = started.Run.ID, started.Run.FencingGeneration, true
		state.LeaseExpiresAt = started.Run.LeaseExpiresAt
		result.Request, result.Run = &started.Request, &started.Run
		return r.failAndAbandon(ctx, state, result, ErrSensitiveRequest, "sensitive_scope_refused")
	}
	state.RequestGeneration = started.Run.RequestGeneration
	state.IncludesSensitive = started.Request.Scope.IncludeSensitive
	state.RunID, state.FencingGeneration = started.Run.ID, started.Run.FencingGeneration
	state.LeaseExpiresAt, state.Phase = started.Run.LeaseExpiresAt, PhaseStarted
	result.Request, result.Run = &started.Request, &started.Run
	if err := r.save(state); err != nil {
		return r.failAndAbandon(ctx, state, result, err, "state_write_failed")
	}
	return r.executeOpen(ctx, state, options, result, &started.Run)
}

func (r Runner) executeOpen(ctx context.Context, state *LaunchState, options Options, result Result, run *client.MemoryCurationRun) (Result, error) {
	inputs, err := r.collectInputs(ctx, state, options, run)
	if err != nil {
		return r.failAndAbandon(ctx, state, result, fmt.Errorf("page curation inputs: %w", err), "input_read_failed")
	}
	state.InputCount, state.Phase = len(inputs), PhasePlanning
	result.InputCount, result.Run = len(inputs), run
	if err := r.save(state); err != nil {
		return r.failAndAbandon(ctx, state, result, err, "state_write_failed")
	}
	envelope := PlannerEnvelope{
		Schema: PlannerEnvelopeSchemaV1, RequestID: run.RequestID,
		RequestGeneration: run.RequestGeneration, RunID: run.ID,
		FencingGeneration: run.FencingGeneration, LeaseExpiresAt: run.LeaseExpiresAt,
		Policy: PlannerPolicy{
			PlanSchema:        MemoryPlanSchemaV1,
			AllowedOperations: []string{"create", "replace", "supersede", "relate", "propose_fact"},
			IncludeSensitive:  state.IncludesSensitive, MaximumActions: options.MaximumActions,
		},
		MaterializedInputs: inputs,
	}
	draft, err := r.planWithRenewal(ctx, state, options, run, envelope)
	if err != nil {
		return r.failAndAbandon(ctx, state, result, fmt.Errorf("curator planner: %w", err), "planner_failed")
	}
	if err := validatePlanDraftForLimit(draft, options.MaximumActions); err != nil {
		return r.failAndAbandon(ctx, state, result, err, "planner_output_invalid")
	}
	planned, err := r.API.Plan(ctx, client.PlanMemoryCurationInput{
		RunID: run.ID, FencingGeneration: run.FencingGeneration,
		Draft: draft, IdempotencyKey: state.PlanKey,
	})
	if err != nil {
		return r.failAndAbandon(ctx, state, result, fmt.Errorf("validate curation plan: %w", err), "plan_rejected")
	}
	if planned == nil || planned.Run.ID != run.ID || planned.Run.FencingGeneration != run.FencingGeneration || planned.Run.State != "planned" ||
		planned.Receipt.ID == "" || planned.Receipt.RunID != run.ID || planned.Receipt.FencingGeneration != run.FencingGeneration ||
		planned.Receipt.PlanRevision < 1 || !planHashPattern.MatchString(planned.Receipt.PlanHash) ||
		planned.Run.PlanRevision != planned.Receipt.PlanRevision || planned.Run.PlanHash != planned.Receipt.PlanHash {
		return r.failAndAbandon(ctx, state, result, fmt.Errorf("%w: plan returned incomplete acceptance", ErrProtocolResponse), "plan_response_invalid")
	}
	state.PlanRevision, state.PlanHash = planned.Receipt.PlanRevision, planned.Receipt.PlanHash
	state.PlanReceiptID, state.Phase = planned.Receipt.ID, PhasePlanned
	state.LeaseExpiresAt = planned.Run.LeaseExpiresAt
	result.Plan, result.Run = planned, &planned.Run
	if err := r.save(state); err != nil {
		return r.failAndAbandon(ctx, state, result, err, "state_write_failed")
	}
	return r.finishPlanned(ctx, state, options, result, &planned.Run, planned)
}

func (r Runner) finishPlanned(ctx context.Context, state *LaunchState, options Options, result Result, run *client.MemoryCurationRun, planned *client.PlanMemoryCurationResult) (Result, error) {
	if state.ApplyPolicy == ApplyPolicyPreview {
		state.Phase = PhasePreviewAbandoning
		state.AbandonReason = "preview_complete"
		if err := r.save(state); err != nil {
			return result, err
		}
		abandoned, err := r.abandon(ctx, state, state.AbandonReason)
		if err != nil {
			return result, fmt.Errorf("requeue previewed curation: %w", err)
		}
		state.Phase = PhasePreviewed
		result.Abandon, result.Run = abandoned, &abandoned.Run
		if err := r.save(state); err != nil {
			return result, err
		}
		return result, nil
	}
	if state.ApplyPolicy != ApplyPolicyApply || !options.ApproveApply {
		return r.failAndAbandon(ctx, state, result, ErrApplyNotApproved, "apply_not_approved")
	}
	if state.PlanRevision < 1 || !planHashPattern.MatchString(state.PlanHash) {
		return r.failAndAbandon(ctx, state, result, fmt.Errorf("%w: planned run lacks revision and hash", ErrProtocolResponse), "plan_response_invalid")
	}
	state.Phase = PhaseApplying
	if err := r.save(state); err != nil {
		return result, err
	}
	applied, err := r.API.Apply(ctx, client.ApplyMemoryCurationInput{
		RunID: run.ID, FencingGeneration: run.FencingGeneration,
		PlanRevision: state.PlanRevision, PlanHash: state.PlanHash,
		IdempotencyKey: state.ApplyKey,
	})
	if err != nil {
		return r.recoverApplyFailure(ctx, state, options, result, err)
	}
	if applied == nil || applied.Run.ID != run.ID || applied.Run.FencingGeneration != run.FencingGeneration || applied.Run.State != "applied" ||
		applied.Receipt.ID == "" || applied.Receipt.RunID != run.ID || applied.Receipt.FencingGeneration != run.FencingGeneration ||
		applied.Receipt.PlanRevision != state.PlanRevision || applied.Receipt.PlanHash != state.PlanHash {
		return r.recoverApplyFailure(ctx, state, options, result, fmt.Errorf("%w: apply returned incomplete receipt", ErrProtocolResponse))
	}
	state.Phase, state.ApplyReceiptID = PhaseApplied, applied.Receipt.ID
	result.Apply, result.Run = applied, &applied.Run
	if err := r.save(state); err != nil {
		return result, err
	}
	_ = planned
	return result, nil
}

func (r Runner) recoverApplyFailure(ctx context.Context, state *LaunchState, _ Options, result Result, applyErr error) (Result, error) {
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	run, err := r.API.GetRun(cleanupCtx, state.RunID)
	if err != nil {
		return result, errors.Join(fmt.Errorf("apply curation: %w", applyErr), fmt.Errorf("recover apply result: %w", err))
	}
	result.Run = run
	if run == nil || run.ID != state.RunID || run.FencingGeneration != state.FencingGeneration {
		return result, fmt.Errorf("%w: apply recovery returned a different run", ErrProtocolResponse)
	}
	switch run.State {
	case "applied":
		if run.ApplyReceiptID == "" || run.PlanRevision != state.PlanRevision || run.PlanHash != state.PlanHash {
			return result, fmt.Errorf("%w: recovered applied run lacks the accepted receipt", ErrProtocolResponse)
		}
		state.Phase, state.ApplyReceiptID = PhaseApplied, run.ApplyReceiptID
		if err := r.save(state); err != nil {
			return result, err
		}
		return result, nil
	case "open", "planned":
		return r.failAndAbandon(ctx, state, result, fmt.Errorf("apply curation: %w", applyErr), "apply_failed")
	default:
		state.Phase = PhaseTerminal
		_ = r.save(state)
		return result, fmt.Errorf("%w: apply failed and run state is %s: %v", ErrCurationTerminal, run.State, applyErr)
	}
}

func (r Runner) collectInputs(ctx context.Context, state *LaunchState, options Options, run *client.MemoryCurationRun) ([]client.MemoryCurationRunInput, error) {
	inputs := make([]client.MemoryCurationRunInput, 0)
	cursor := ""
	seen := make(map[string]struct{})
	for {
		if err := r.renewIfDue(ctx, state, options, run); err != nil {
			return nil, err
		}
		page, err := r.API.GetInputs(ctx, run.ID, run.FencingGeneration, cursor, options.PageSize)
		if err != nil {
			return nil, err
		}
		if page == nil || page.Run.ID != run.ID || page.Run.FencingGeneration != run.FencingGeneration || page.Run.State != "open" || page.Run.LeaseExpiresAt == nil {
			return nil, fmt.Errorf("%w: input page does not match run fence", ErrProtocolResponse)
		}
		*run = page.Run
		state.LeaseExpiresAt = run.LeaseExpiresAt
		for _, input := range page.Inputs {
			expectedOrdinal := int64(len(inputs) + 1)
			if input.RunID != run.ID || input.Ordinal != expectedOrdinal {
				return nil, fmt.Errorf("%w: input membership is not contiguous for this run", ErrProtocolResponse)
			}
			inputs = append(inputs, input)
		}
		if len(inputs) > maximumCollectedInputs {
			return nil, fmt.Errorf("%w: input membership exceeds client safety bound", ErrProtocolResponse)
		}
		next := page.NextCursor
		if next == "" {
			if run.InputCount != len(inputs) {
				return nil, fmt.Errorf("%w: paged %d inputs but the frozen run declares %d", ErrProtocolResponse, len(inputs), run.InputCount)
			}
			break
		}
		if _, duplicate := seen[next]; duplicate || next == cursor {
			return nil, fmt.Errorf("%w: repeated input cursor", ErrProtocolResponse)
		}
		seen[next] = struct{}{}
		cursor = next
	}
	return inputs, nil
}

type plannerResult struct {
	draft json.RawMessage
	err   error
}

func (r Runner) planWithRenewal(ctx context.Context, state *LaunchState, options Options, run *client.MemoryCurationRun, envelope PlannerEnvelope) (json.RawMessage, error) {
	plannerCtx, cancel := context.WithTimeout(ctx, options.PlannerTimeout)
	defer cancel()
	result := make(chan plannerResult, 1)
	go func() {
		draft, err := r.Planner.Plan(plannerCtx, envelope)
		result <- plannerResult{draft: draft, err: err}
	}()
	for {
		delay, err := r.renewalDelay(run.LeaseExpiresAt, options.RenewBefore)
		if err != nil {
			return nil, err
		}
		timer := time.NewTimer(delay)
		select {
		case planned := <-result:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return planned.draft, planned.err
		case <-plannerCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, plannerCtx.Err()
		case <-timer.C:
			if err := r.renew(ctx, state, options, run); err != nil {
				cancel()
				return nil, err
			}
		}
	}
}

func (r Runner) renewIfDue(ctx context.Context, state *LaunchState, options Options, run *client.MemoryCurationRun) error {
	delay, err := r.renewalDelay(run.LeaseExpiresAt, options.RenewBefore)
	if err != nil {
		return err
	}
	if delay > 0 {
		return nil
	}
	return r.renew(ctx, state, options, run)
}

func (r Runner) renew(ctx context.Context, state *LaunchState, options Options, run *client.MemoryCurationRun) error {
	state.RenewalCount++
	state.LastRenewKey = fmt.Sprintf("%s:renew:%d", state.LaunchID, state.RenewalCount)
	if err := r.save(state); err != nil {
		return err
	}
	renewed, err := r.API.Renew(ctx, client.RenewMemoryCurationInput{
		RunID: run.ID, FencingGeneration: run.FencingGeneration,
		Extension: options.LeaseDuration, IdempotencyKey: state.LastRenewKey,
	})
	if err != nil {
		return fmt.Errorf("renew curation lease: %w", err)
	}
	if renewed == nil || renewed.Run.ID != run.ID || renewed.Run.FencingGeneration != run.FencingGeneration || renewed.Run.State != "open" ||
		renewed.Run.LeaseExpiresAt == nil || !renewed.Run.LeaseExpiresAt.After(r.now()) {
		return fmt.Errorf("%w: renewal returned invalid lease", ErrProtocolResponse)
	}
	*run = renewed.Run
	state.LeaseExpiresAt = run.LeaseExpiresAt
	return r.save(state)
}

func (r Runner) renewalDelay(expiresAt *time.Time, lead time.Duration) (time.Duration, error) {
	if expiresAt == nil {
		return 0, fmt.Errorf("%w: run has no lease expiry", ErrProtocolResponse)
	}
	delay := expiresAt.Sub(r.now()) - lead
	if delay < 0 {
		return 0, nil
	}
	return delay, nil
}

func (r Runner) failAndAbandon(ctx context.Context, state *LaunchState, result Result, cause error, reason string) (Result, error) {
	if state.RunID == "" || state.FencingGeneration < 1 {
		return result, cause
	}
	state.Phase = PhaseFailureAbandoning
	if state.AbandonReason == "" {
		state.AbandonReason = reason
	}
	_ = r.save(state)
	abandoned, err := r.abandon(ctx, state, state.AbandonReason)
	if err != nil {
		return result, errors.Join(cause, fmt.Errorf("abandon failed curation: %w", err))
	}
	state.Phase = PhaseAbandoned
	result.Abandon, result.Run = abandoned, &abandoned.Run
	if err := r.save(state); err != nil {
		return result, errors.Join(cause, err)
	}
	return result, cause
}

func (r Runner) abandon(ctx context.Context, state *LaunchState, reason string) (*client.FinishMemoryCurationResult, error) {
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	abandoned, err := r.API.Abandon(cleanupCtx, client.FinishMemoryCurationInput{
		RunID: state.RunID, FencingGeneration: state.FencingGeneration,
		Reason: reason, IdempotencyKey: state.AbandonKey,
	})
	if err != nil {
		return nil, err
	}
	if abandoned == nil || abandoned.Run.ID != state.RunID || abandoned.Run.FencingGeneration != state.FencingGeneration || abandoned.Run.State != "abandoned" {
		return nil, fmt.Errorf("%w: abandon returned incomplete terminal state", ErrProtocolResponse)
	}
	return abandoned, nil
}

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

func (r Runner) validate() error {
	if r.API == nil || r.Planner == nil || r.State == nil {
		return errors.New("curator runner requires API, Planner, and State")
	}
	return nil
}

func (r Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r Runner) newID(prefix string) (string, error) {
	if r.NewID != nil {
		return r.NewID(prefix)
	}
	return id.New(prefix)
}

func (r Runner) save(state *LaunchState) error {
	state.UpdatedAt = r.now()
	if err := r.State.Save(*state); err != nil {
		return fmt.Errorf("save curator launch state: %w", err)
	}
	return nil
}

func normalizeOptions(options Options) (Options, error) {
	if options.ApplyPolicy == "" {
		options.ApplyPolicy = ApplyPolicyPreview
	}
	if options.ApplyPolicy != ApplyPolicyPreview && options.ApplyPolicy != ApplyPolicyApply {
		return Options{}, fmt.Errorf("unknown curator apply policy %q", options.ApplyPolicy)
	}
	if options.ApplyPolicy == ApplyPolicyApply && !options.ApproveApply {
		return Options{}, ErrApplyNotApproved
	}
	if options.Caps.MaxMemories == 0 {
		options.Caps.MaxMemories = defaultMaxMemories
	}
	if options.Caps.MaxEvidence == 0 {
		options.Caps.MaxEvidence = defaultMaxEvidence
	}
	if options.Caps.MaxTranscriptEntries == 0 {
		options.Caps.MaxTranscriptEntries = defaultMaxTranscriptEntries
	}
	if options.Caps.MaxMemories < 1 || options.Caps.MaxEvidence < 1 || options.Caps.MaxTranscriptEntries < 1 {
		return Options{}, errors.New("curator input caps must be positive")
	}
	if options.PageSize == 0 {
		options.PageSize = defaultPageSize
	}
	if options.PageSize < 1 || options.PageSize > 200 {
		return Options{}, errors.New("curator page size must be between 1 and 200")
	}
	if options.MaximumActions == 0 {
		options.MaximumActions = defaultMaximumActions
	}
	if options.MaximumActions < 1 || options.MaximumActions > 128 {
		return Options{}, errors.New("curator maximum actions must be between 1 and 128")
	}
	if options.LeaseDuration == 0 {
		options.LeaseDuration = defaultLeaseDuration
	}
	if options.PlannerTimeout == 0 {
		options.PlannerTimeout = defaultPlannerTimeout
	}
	if options.RenewBefore == 0 {
		options.RenewBefore = defaultRenewBefore
	}
	if options.LeaseDuration <= 0 || options.PlannerTimeout <= 0 || options.RenewBefore < 0 || options.RenewBefore >= options.LeaseDuration {
		return Options{}, errors.New("curator lease, timeout, or renewal window is invalid")
	}
	if options.LeaseDuration%time.Second != 0 || options.PlannerTimeout%time.Second != 0 || options.RenewBefore%time.Second != 0 {
		return Options{}, errors.New("curator lease, timeout, and renewal window must use whole seconds")
	}
	return options, nil
}

func optionsFromState(state LaunchState, supplied Options) Options {
	supplied.RequestID = state.RequestID
	supplied.ApplyPolicy = state.ApplyPolicy
	supplied.Caps = state.Caps
	supplied.PageSize = state.PageSize
	supplied.MaximumActions = state.MaximumActions
	supplied.LeaseDuration = time.Duration(state.LeaseSeconds) * time.Second
	supplied.PlannerTimeout = time.Duration(state.PlannerTimeoutSeconds) * time.Second
	supplied.RenewBefore = time.Duration(state.RenewBeforeSeconds) * time.Second
	supplied.Client = state.Client
	return supplied
}
