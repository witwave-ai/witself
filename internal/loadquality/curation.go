package loadquality

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// Curation load-result schemas, deterministic defaults, and hard workload
// bounds. The harness bounds are intentionally smaller than some store limits:
// this is a repeatable lifecycle probe, not an unbounded capacity benchmark.
const (
	CurationResultSchemaV1 = "witself.memory-curation-load-result.v1"
	CurationHarnessVersion = "1"

	DefaultCurationSeed                = 20260831
	DefaultCurationCoalescingRequests  = 24
	DefaultCurationClaimRequests       = 6
	DefaultCurationClaimWorkers        = 4
	DefaultCurationPagingCardinalities = "4,16,64"
	DefaultCurationPageSize            = 8
	DefaultCurationChainBacklog        = 24
	DefaultCurationChainCap            = 6
	DefaultCurationLeaseCycles         = 3
	DefaultCurationMaxAttempts         = 3

	MaximumCurationCoalescingRequests = 10_000
	MaximumCurationClaimRequests      = 64
	MaximumCurationClaimWorkers       = 64
	MaximumCurationPagingCardinality  = 500
	MaximumCurationPageSize           = 200
	MaximumCurationChainBacklog       = 32_000
	MaximumCurationChainCap           = 500
	MaximumCurationChainDepth         = 64
	MaximumCurationLeaseCycles        = 20
	MaximumCurationMaxAttempts        = 20
)

// Environment variables accepted by ParseCurationOptions.
const (
	EnvCurationResultsPath         = "WITSELF_MEMORY_CURATION_LOAD_RESULTS"
	EnvCurationSeed                = "WITSELF_MEMORY_CURATION_LOAD_SEED"
	EnvCurationCoalescingRequests  = "WITSELF_MEMORY_CURATION_LOAD_COALESCING_REQUESTS"
	EnvCurationClaimRequests       = "WITSELF_MEMORY_CURATION_LOAD_CLAIM_REQUESTS"
	EnvCurationClaimWorkers        = "WITSELF_MEMORY_CURATION_LOAD_CLAIM_WORKERS"
	EnvCurationPagingCardinalities = "WITSELF_MEMORY_CURATION_LOAD_PAGING_CARDINALITIES"
	EnvCurationPageSize            = "WITSELF_MEMORY_CURATION_LOAD_PAGE_SIZE"
	EnvCurationChainBacklog        = "WITSELF_MEMORY_CURATION_LOAD_CHAIN_BACKLOG"
	EnvCurationChainCap            = "WITSELF_MEMORY_CURATION_LOAD_CHAIN_CAP"
	EnvCurationLeaseCycles         = "WITSELF_MEMORY_CURATION_LOAD_LEASE_CYCLES"
	EnvCurationMaxAttempts         = "WITSELF_MEMORY_CURATION_LOAD_MAX_ATTEMPTS"
	EnvCurationRelease             = "WITSELF_MEMORY_CURATION_LOAD_RELEASE"
	EnvCurationCommit              = "WITSELF_MEMORY_CURATION_LOAD_COMMIT"
	EnvCurationProvider            = "WITSELF_MEMORY_CURATION_LOAD_PROVIDER"
	EnvCurationHardwareTier        = "WITSELF_MEMORY_CURATION_LOAD_HARDWARE_TIER"
)

//go:embed testdata/curation-result-schema.v1.json
var curationResultSchemaJSON []byte

// CurationOptions contains only bounded workload controls and safe evidence
// metadata. In particular, the database DSN is deliberately absent.
type CurationOptions struct {
	ResultsPath         string
	Seed                int64
	CoalescingRequests  int
	ClaimRequests       int
	ClaimWorkers        int
	PagingCardinalities []int
	PageSize            int
	ChainBacklog        int
	ChainCap            int
	LeaseCycles         int
	MaxAttempts         int
	Release             string
	Commit              string
	Provider            string
	HardwareTier        string
}

// CurationWorkload records only bounded, synthetic fixture dimensions.
type CurationWorkload struct {
	Seed                int64 `json:"seed"`
	SyntheticAccounts   int   `json:"synthetic_accounts"`
	SyntheticAgents     int   `json:"synthetic_agents"`
	CoalescingRequests  int   `json:"coalescing_requests"`
	ClaimRequests       int   `json:"claim_requests"`
	ClaimWorkers        int   `json:"claim_workers"`
	PagingCardinalities []int `json:"paging_cardinalities"`
	PageSize            int   `json:"page_size"`
	ChainBacklog        int   `json:"chain_backlog"`
	ChainCap            int   `json:"chain_cap"`
	ChainDepth          int   `json:"chain_depth"`
	LeaseCycles         int   `json:"lease_cycles"`
	MaxAttempts         int   `json:"max_attempts"`
}

// CurationMeasurements retains one latency distribution for each measured
// curation operation or expected-refusal class. Expected typed refusals are
// measurements, not harness failures.
type CurationMeasurements struct {
	RequestCoalescing OperationStats `json:"request_coalescing"`
	ClaimStart        OperationStats `json:"claim_start"`
	InputPage         OperationStats `json:"input_page"`
	Plan              OperationStats `json:"plan"`
	PlanGet           OperationStats `json:"plan_get"`
	Apply             OperationStats `json:"apply"`
	LeaseRenew        OperationStats `json:"lease_renew"`
	LeaseApplyRace    OperationStats `json:"lease_apply_race"`
	TypedRefusal      OperationStats `json:"typed_refusal"`
	Abandon           OperationStats `json:"abandon"`
}

// CurationRequestCoalescingOutcome measures one owner lane receiving repeated
// request calls. It contains no request or owner identifiers.
type CurationRequestCoalescingOutcome struct {
	Calls           int     `json:"calls"`
	Created         int     `json:"created"`
	Coalesced       int     `json:"coalesced"`
	QueueDepth      int     `json:"queue_depth"`
	CoalescingRatio float64 `json:"coalescing_ratio"`
	AllCoalesced    bool    `json:"all_coalesced"`
}

// CurationClaimContentionOutcome aggregates concurrent StartCuration races.
type CurationClaimContentionOutcome struct {
	Requests               int     `json:"requests"`
	Attempts               int     `json:"attempts"`
	Wins                   int     `json:"wins"`
	Losses                 int     `json:"losses"`
	WinRate                float64 `json:"win_rate"`
	LossRate               float64 `json:"loss_rate"`
	SingleWinnerPerRequest bool    `json:"single_winner_per_request"`
}

// CurationInputPagingOutcome records complete frozen-input traversal.
type CurationInputPagingOutcome struct {
	Runs              int  `json:"runs"`
	Pages             int  `json:"pages"`
	Inputs            int  `json:"inputs"`
	ExhaustedRuns     int  `json:"exhausted_runs"`
	DuplicateInputs   int  `json:"duplicate_inputs"`
	PagedToExhaustion bool `json:"paged_to_exhaustion"`
}

// CurationPlanLifecycleOutcome aggregates accepted-plan review and apply,
// including empty plans and bounded follow-up chains.
type CurationPlanLifecycleOutcome struct {
	Plans                    int  `json:"plans"`
	PlanGets                 int  `json:"plan_gets"`
	Applies                  int  `json:"applies"`
	EmptyApplies             int  `json:"empty_applies"`
	CreateApplies            int  `json:"create_applies"`
	CreateActions            int  `json:"create_actions"`
	EmptyCursorAdvances      int  `json:"empty_cursor_advances"`
	FollowUpRequests         int  `json:"follow_up_requests"`
	MaxChainDepth            int  `json:"max_chain_depth"`
	DrainedChains            int  `json:"drained_chains"`
	EmptyPlanAdvancedCursors bool `json:"empty_plan_advanced_cursors"`
	BacklogDrained           bool `json:"backlog_drained"`
}

// CurationLeaseChurnOutcome records durable expired-lease reconciliation and
// a fenced apply race without retaining any run or receipt identifiers.
type CurationLeaseChurnOutcome struct {
	Cycles                 int  `json:"cycles"`
	LiveRenewals           int  `json:"live_renewals"`
	RenewAfterExpiry       int  `json:"renew_after_expiry"`
	Reconciliations        int  `json:"reconciliations"`
	Requeues               int  `json:"requeues"`
	ApplyRaceAttempts      int  `json:"apply_race_attempts"`
	ApplyRaceWins          int  `json:"apply_race_wins"`
	ApplyRaceRefusals      int  `json:"apply_race_refusals"`
	StaleFenceRefusals     int  `json:"stale_fence_refusals"`
	DoubleApplySuccesses   int  `json:"double_apply_successes"`
	ExpiredRenewReconciled bool `json:"expired_renew_reconciled"`
	NoDoubleApply          bool `json:"no_double_apply"`
}

// CurationStalePlanConflictOutcome counts stable typed refusals.
type CurationStalePlanConflictOutcome struct {
	WrongPlanHashRefusals int  `json:"wrong_plan_hash_refusals"`
	DuplicatePlanRefusals int  `json:"duplicate_plan_refusals"`
	StaleFenceRefusals    int  `json:"stale_fence_refusals"`
	TypedRefusals         int  `json:"typed_refusals"`
	AllRefusalsTyped      bool `json:"all_refusals_typed"`
}

// CurationAbandonRequeueOutcome covers preview_complete and retry-budget
// exhaustion without exposing the terminal request or run.
type CurationAbandonRequeueOutcome struct {
	PreviewAbandons           int  `json:"preview_abandons"`
	PreviewRequeues           int  `json:"preview_requeues"`
	PreviewAttemptCountBefore int  `json:"preview_attempt_count_before"`
	PreviewAttemptCountAfter  int  `json:"preview_attempt_count_after"`
	FailureAbandons           int  `json:"failure_abandons"`
	ExpiryInterruptions       int  `json:"expiry_interruptions"`
	RetryRequeues             int  `json:"retry_requeues"`
	DeadLetters               int  `json:"dead_letters"`
	TerminalAttemptCount      int  `json:"terminal_attempt_count"`
	PostTerminalStartRefusals int  `json:"post_terminal_start_refusals"`
	PreviewBudgetPreserved    bool `json:"preview_budget_preserved"`
	DeadLetterTerminal        bool `json:"dead_letter_terminal"`
}

// CurationOutcomes contains aggregate correctness evidence for all seven
// requested workloads.
type CurationOutcomes struct {
	RequestCoalescing CurationRequestCoalescingOutcome `json:"request_coalescing"`
	ClaimContention   CurationClaimContentionOutcome   `json:"claim_contention"`
	InputPaging       CurationInputPagingOutcome       `json:"input_paging"`
	PlanLifecycle     CurationPlanLifecycleOutcome     `json:"plan_lifecycle"`
	LeaseChurn        CurationLeaseChurnOutcome        `json:"lease_churn"`
	StalePlanConflict CurationStalePlanConflictOutcome `json:"stale_plan_conflict"`
	AbandonRequeue    CurationAbandonRequeueOutcome    `json:"abandon_requeue"`
}

// CurationResult is a value-free artifact safe to retain as CI or release
// evidence. PostgreSQLVersion is software metadata, never endpoint identity.
type CurationResult struct {
	Schema            string               `json:"schema"`
	HarnessVersion    string               `json:"harness_version"`
	StartedAt         time.Time            `json:"started_at"`
	CompletedAt       time.Time            `json:"completed_at"`
	Outcome           string               `json:"outcome"`
	PostgreSQLVersion string               `json:"postgresql_version"`
	Environment       SafeMetadata         `json:"environment"`
	Workload          CurationWorkload     `json:"workload"`
	Measurements      CurationMeasurements `json:"measurements"`
	Outcomes          CurationOutcomes     `json:"outcomes"`
}

// CurationResultJSONSchema returns a fresh copy of the checked-in schema.
func CurationResultJSONSchema() []byte {
	return append([]byte(nil), curationResultSchemaJSON...)
}

// ParseCurationOptions reads bounded controls through an injected lookup so
// unit tests do not mutate the process environment.
func ParseCurationOptions(getenv func(string) string) (CurationOptions, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	opts := CurationOptions{
		ResultsPath:        strings.TrimSpace(getenv(EnvCurationResultsPath)),
		Seed:               DefaultCurationSeed,
		CoalescingRequests: DefaultCurationCoalescingRequests,
		ClaimRequests:      DefaultCurationClaimRequests,
		ClaimWorkers:       DefaultCurationClaimWorkers,
		PageSize:           DefaultCurationPageSize,
		ChainBacklog:       DefaultCurationChainBacklog,
		ChainCap:           DefaultCurationChainCap,
		LeaseCycles:        DefaultCurationLeaseCycles,
		MaxAttempts:        DefaultCurationMaxAttempts,
		Release:            metadataOrDefault(getenv(EnvCurationRelease), "dev"),
		Commit:             metadataOrDefault(getenv(EnvCurationCommit), "none"),
		Provider:           metadataOrDefault(getenv(EnvCurationProvider), "local"),
		HardwareTier:       metadataOrDefault(getenv(EnvCurationHardwareTier), "unspecified"),
	}
	if opts.ResultsPath == "" {
		// A pid-scoped default keeps two concurrent harness runs from
		// atomically renaming over each other's retained evidence document.
		opts.ResultsPath = fmt.Sprintf(
			"/tmp/witself-memory-curation-load-%d.json", os.Getpid(),
		)
	}
	var err error
	if opts.Seed, err = parseInt64(getenv(EnvCurationSeed), DefaultCurationSeed, math.MinInt64, math.MaxInt64, EnvCurationSeed); err != nil {
		return CurationOptions{}, err
	}
	if opts.CoalescingRequests, err = parseInt(getenv(EnvCurationCoalescingRequests), DefaultCurationCoalescingRequests, 2, MaximumCurationCoalescingRequests, EnvCurationCoalescingRequests); err != nil {
		return CurationOptions{}, err
	}
	if opts.ClaimRequests, err = parseInt(getenv(EnvCurationClaimRequests), DefaultCurationClaimRequests, 1, MaximumCurationClaimRequests, EnvCurationClaimRequests); err != nil {
		return CurationOptions{}, err
	}
	if opts.ClaimWorkers, err = parseInt(getenv(EnvCurationClaimWorkers), DefaultCurationClaimWorkers, 2, MaximumCurationClaimWorkers, EnvCurationClaimWorkers); err != nil {
		return CurationOptions{}, err
	}
	if opts.PagingCardinalities, err = parseCurationCardinalities(getenv(EnvCurationPagingCardinalities)); err != nil {
		return CurationOptions{}, err
	}
	if opts.PageSize, err = parseInt(getenv(EnvCurationPageSize), DefaultCurationPageSize, 1, MaximumCurationPageSize, EnvCurationPageSize); err != nil {
		return CurationOptions{}, err
	}
	if opts.ChainBacklog, err = parseInt(getenv(EnvCurationChainBacklog), DefaultCurationChainBacklog, 2, MaximumCurationChainBacklog, EnvCurationChainBacklog); err != nil {
		return CurationOptions{}, err
	}
	if opts.ChainCap, err = parseInt(getenv(EnvCurationChainCap), DefaultCurationChainCap, 1, MaximumCurationChainCap, EnvCurationChainCap); err != nil {
		return CurationOptions{}, err
	}
	if _, err := CurationChainDepth(opts.ChainBacklog, opts.ChainCap); err != nil {
		return CurationOptions{}, err
	}
	if opts.LeaseCycles, err = parseInt(getenv(EnvCurationLeaseCycles), DefaultCurationLeaseCycles, 1, MaximumCurationLeaseCycles, EnvCurationLeaseCycles); err != nil {
		return CurationOptions{}, err
	}
	if opts.MaxAttempts, err = parseInt(getenv(EnvCurationMaxAttempts), DefaultCurationMaxAttempts, 2, MaximumCurationMaxAttempts, EnvCurationMaxAttempts); err != nil {
		return CurationOptions{}, err
	}
	for _, item := range []struct {
		name  string
		value string
	}{
		{EnvCurationRelease, opts.Release},
		{EnvCurationCommit, opts.Commit},
	} {
		if !safeMetadata(item.value) {
			return CurationOptions{}, fmt.Errorf("%s contains unsafe evidence metadata", item.name)
		}
	}
	// Provider and hardware tier are operator-typed labels retained verbatim
	// in the sanitized evidence document. Rejecting dots keeps a pasted
	// database hostname from ever passing as a label; releases and commits
	// legitimately need dots and carry no hostname risk shape.
	for _, item := range []struct {
		name  string
		value string
	}{
		{EnvCurationProvider, opts.Provider},
		{EnvCurationHardwareTier, opts.HardwareTier},
	} {
		if !curationLabelMetadata(item.value) {
			return CurationOptions{}, fmt.Errorf(
				"%s must be a dotless label (letters, digits, '+', '_', '-')", item.name,
			)
		}
	}
	return opts, nil
}

// CurationChainDepth returns the exact number of capped cycles needed to drain
// a synthetic backlog and rejects shapes that do not exercise a follow-up or
// that exceed the harness bound.
func CurationChainDepth(backlog, chainCap int) (int, error) {
	if backlog < 2 || backlog > MaximumCurationChainBacklog || chainCap < 1 || chainCap > MaximumCurationChainCap || chainCap >= backlog {
		return 0, fmt.Errorf("curation chain requires backlog greater than cap within bounds")
	}
	depth := (backlog + chainCap - 1) / chainCap
	if depth < 2 || depth > MaximumCurationChainDepth {
		return 0, fmt.Errorf("curation chain depth must be between 2 and %d", MaximumCurationChainDepth)
	}
	return depth, nil
}

// curationLabelPattern is the dotless variant of the shared metadata pattern:
// provider and hardware-tier labels must never be able to carry a hostname
// into the retained evidence document.
var curationLabelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+_-]{0,127}$`)

func curationLabelMetadata(value string) bool { return curationLabelPattern.MatchString(value) }

// CurationRatio returns a stable three-decimal rate for sanitized evidence.
func CurationRatio(numerator, denominator int) float64 {
	if numerator < 0 || denominator <= 0 || numerator > denominator {
		return 0
	}
	return rounded(float64(numerator) / float64(denominator))
}

// CurationEnvironment builds the safe runner metadata retained in evidence.
func CurationEnvironment(opts CurationOptions) SafeMetadata {
	return SafeMetadata{
		Release: opts.Release, Commit: opts.Commit, Provider: opts.Provider,
		HardwareTier: opts.HardwareTier, GoVersion: runtime.Version(),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, LogicalCPUs: runtime.NumCPU(),
	}
}

// ValidateCurationResult requires a complete passing result and exact coherence
// between declared workload, operation counts, and aggregate outcomes.
func ValidateCurationResult(result CurationResult) error {
	if result.Schema != CurationResultSchemaV1 || result.HarnessVersion != CurationHarnessVersion ||
		result.Outcome != "pass" || result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) ||
		strings.TrimSpace(result.PostgreSQLVersion) == "" || len(result.PostgreSQLVersion) > 128 {
		return errors.New("invalid curation load result envelope")
	}
	if !validCurationEnvironment(result.Environment) {
		return errors.New("invalid curation load result environment")
	}
	if err := validateCurationWorkload(result.Workload); err != nil {
		return err
	}
	stats := []OperationStats{
		result.Measurements.RequestCoalescing, result.Measurements.ClaimStart,
		result.Measurements.InputPage, result.Measurements.Plan,
		result.Measurements.PlanGet, result.Measurements.Apply,
		result.Measurements.LeaseRenew, result.Measurements.LeaseApplyRace,
		result.Measurements.TypedRefusal, result.Measurements.Abandon,
	}
	for _, measurement := range stats {
		if !validOperationStats(measurement) {
			return errors.New("invalid curation operation measurements")
		}
	}
	if err := validateCurationOutcomes(result.Workload, result.Measurements, result.Outcomes); err != nil {
		return err
	}
	return nil
}

// MarshalCurationResult performs semantic checks and then validates the exact
// JSON instance against the checked-in Draft 2020-12 schema.
func MarshalCurationResult(result CurationResult) ([]byte, error) {
	if err := ValidateCurationResult(result); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := ValidateCurationResultJSON(raw); err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// ValidateCurationResultJSON validates one JSON instance with the checked-in
// Draft 2020-12 result schema. It is exported so schema conformance can be
// tested independently from the stronger semantic result validator.
func ValidateCurationResultJSON(raw []byte) error {
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return fmt.Errorf("decode curation load result JSON: %w", err)
	}
	resolved, err := resolvedCurationResultSchema()
	if err != nil {
		return err
	}
	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("validate curation load result schema: %w", err)
	}
	return nil
}

// WriteCurationResult writes with private permissions and atomically replaces
// an older artifact only after the complete document is synced and closed.
func WriteCurationResult(path string, result CurationResult) ([]byte, error) {
	raw, err := MarshalCurationResult(result)
	if err != nil {
		return nil, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("curation load result path is required")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create curation load result directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".memory-curation-load-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create curation load result: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("protect curation load result: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("write curation load result: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("sync curation load result: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("close curation load result: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return nil, fmt.Errorf("publish curation load result: %w", err)
	}
	return raw, nil
}

func parseCurationCardinalities(value string) ([]int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = DefaultCurationPagingCardinalities
	}
	parts := strings.Split(value, ",")
	if len(parts) < 2 || len(parts) > 5 {
		return nil, fmt.Errorf("%s must contain 2 to 5 comma-separated integers", EnvCurationPagingCardinalities)
	}
	out := make([]int, 0, len(parts))
	previous := 0
	for _, part := range parts {
		part = strings.TrimSpace(part)
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 1 || parsed > MaximumCurationPagingCardinality || parsed <= previous {
			return nil, fmt.Errorf("%s must be strictly increasing integers between 1 and %d", EnvCurationPagingCardinalities, MaximumCurationPagingCardinality)
		}
		out = append(out, parsed)
		previous = parsed
	}
	return out, nil
}

func validCurationEnvironment(environment SafeMetadata) bool {
	return safeMetadata(environment.Release) && safeMetadata(environment.Commit) &&
		curationLabelMetadata(environment.Provider) &&
		curationLabelMetadata(environment.HardwareTier) &&
		len(environment.GoVersion) >= 1 && len(environment.GoVersion) <= 64 &&
		len(environment.GOOS) >= 1 && len(environment.GOOS) <= 32 &&
		len(environment.GOARCH) >= 1 && len(environment.GOARCH) <= 32 &&
		environment.LogicalCPUs >= 1
}

func validateCurationWorkload(workload CurationWorkload) error {
	wantAgents := 5 + workload.ClaimRequests + len(workload.PagingCardinalities) + workload.LeaseCycles
	if workload.SyntheticAccounts != 2 || workload.SyntheticAgents != wantAgents ||
		workload.CoalescingRequests < 2 || workload.CoalescingRequests > MaximumCurationCoalescingRequests ||
		workload.ClaimRequests < 1 || workload.ClaimRequests > MaximumCurationClaimRequests ||
		workload.ClaimWorkers < 2 || workload.ClaimWorkers > MaximumCurationClaimWorkers ||
		workload.PageSize < 1 || workload.PageSize > MaximumCurationPageSize ||
		workload.LeaseCycles < 1 || workload.LeaseCycles > MaximumCurationLeaseCycles ||
		workload.MaxAttempts < 2 || workload.MaxAttempts > MaximumCurationMaxAttempts {
		return errors.New("invalid curation load result workload")
	}
	if !validCurationCardinalities(workload.PagingCardinalities) {
		return errors.New("invalid curation paging cardinalities")
	}
	depth, err := CurationChainDepth(workload.ChainBacklog, workload.ChainCap)
	if err != nil || workload.ChainDepth != depth {
		return errors.New("invalid curation follow-up chain workload")
	}
	return nil
}

func validCurationCardinalities(values []int) bool {
	if len(values) < 2 || len(values) > 5 {
		return false
	}
	previous := 0
	for _, value := range values {
		if value < 1 || value > MaximumCurationPagingCardinality || value <= previous {
			return false
		}
		previous = value
	}
	return true
}

func validateCurationOutcomes(workload CurationWorkload, measurements CurationMeasurements, outcomes CurationOutcomes) error {
	coalescing := outcomes.RequestCoalescing
	if coalescing.Calls != workload.CoalescingRequests || coalescing.Created != 1 ||
		coalescing.Coalesced != coalescing.Calls-coalescing.Created || coalescing.QueueDepth != coalescing.Created ||
		coalescing.CoalescingRatio != CurationRatio(coalescing.Coalesced, coalescing.Calls) ||
		!coalescing.AllCoalesced || measurements.RequestCoalescing.Count != coalescing.Calls {
		return errors.New("invalid request coalescing outcomes")
	}

	claims := outcomes.ClaimContention
	if claims.Requests != workload.ClaimRequests || claims.Attempts != claims.Requests*workload.ClaimWorkers ||
		claims.Wins != claims.Requests || claims.Losses != claims.Attempts-claims.Wins ||
		claims.WinRate != CurationRatio(claims.Wins, claims.Attempts) ||
		claims.LossRate != CurationRatio(claims.Losses, claims.Attempts) ||
		!claims.SingleWinnerPerRequest || measurements.ClaimStart.Count != claims.Attempts {
		return errors.New("invalid claim contention outcomes")
	}

	paging := outcomes.InputPaging
	wantInputs := 0
	wantPages := 0
	for _, cardinality := range workload.PagingCardinalities {
		// The paging fixture creates c memories, c resolved evidence rows, and c
		// one-entry transcript streams. Freeze adds one input for each source row,
		// one cursor per transcript stream, and the memory/evidence cursors.
		inputs := 4*cardinality + 2
		wantInputs += inputs
		wantPages += (inputs + workload.PageSize - 1) / workload.PageSize
	}
	if paging.Runs != len(workload.PagingCardinalities) || paging.Pages != wantPages ||
		paging.Inputs != wantInputs || paging.ExhaustedRuns != paging.Runs || paging.DuplicateInputs != 0 ||
		!paging.PagedToExhaustion || measurements.InputPage.Count != paging.Pages {
		return errors.New("invalid input paging outcomes")
	}

	plans := outcomes.PlanLifecycle
	if plans.Plans != workload.ChainDepth || plans.PlanGets != plans.Plans || plans.Applies != plans.Plans ||
		plans.EmptyApplies != (workload.ChainDepth+1)/2 || plans.CreateApplies != workload.ChainDepth/2 ||
		plans.Applies != plans.EmptyApplies+plans.CreateApplies || plans.CreateActions != plans.CreateApplies ||
		plans.EmptyCursorAdvances != plans.EmptyApplies || plans.FollowUpRequests != workload.ChainDepth-1 ||
		plans.MaxChainDepth != workload.ChainDepth || plans.DrainedChains != 1 ||
		!plans.EmptyPlanAdvancedCursors || !plans.BacklogDrained ||
		measurements.Plan.Count != plans.Plans || measurements.PlanGet.Count != plans.PlanGets ||
		measurements.Apply.Count != plans.Applies {
		return errors.New("invalid plan lifecycle outcomes")
	}

	leases := outcomes.LeaseChurn
	if leases.Cycles != workload.LeaseCycles || leases.LiveRenewals != leases.Cycles ||
		leases.RenewAfterExpiry != leases.Cycles ||
		leases.Reconciliations != leases.Cycles || leases.Requeues != leases.Cycles ||
		leases.ApplyRaceAttempts != workload.ClaimWorkers || leases.ApplyRaceWins != 1 ||
		leases.ApplyRaceRefusals != workload.ClaimWorkers-1 || leases.StaleFenceRefusals != leases.Cycles ||
		leases.DoubleApplySuccesses != 0 || !leases.ExpiredRenewReconciled || !leases.NoDoubleApply ||
		measurements.LeaseRenew.Count != leases.LiveRenewals+leases.RenewAfterExpiry ||
		measurements.LeaseApplyRace.Count != leases.ApplyRaceAttempts {
		return errors.New("invalid lease churn outcomes")
	}

	conflicts := outcomes.StalePlanConflict
	wantTyped := conflicts.WrongPlanHashRefusals + conflicts.DuplicatePlanRefusals + conflicts.StaleFenceRefusals
	if conflicts.WrongPlanHashRefusals != 1 || conflicts.DuplicatePlanRefusals != 1 ||
		conflicts.StaleFenceRefusals != workload.ClaimRequests+workload.LeaseCycles ||
		conflicts.TypedRefusals != wantTyped ||
		!conflicts.AllRefusalsTyped || measurements.TypedRefusal.Count != conflicts.TypedRefusals {
		return errors.New("invalid stale-plan conflict outcomes")
	}

	abandon := outcomes.AbandonRequeue
	if abandon.PreviewAbandons != 1 || abandon.PreviewRequeues != 1 ||
		abandon.PreviewAttemptCountBefore != 0 || abandon.PreviewAttemptCountAfter != 0 ||
		abandon.FailureAbandons != (workload.MaxAttempts+1)/2 ||
		abandon.ExpiryInterruptions != workload.MaxAttempts/2 ||
		abandon.RetryRequeues != workload.MaxAttempts-1 || abandon.DeadLetters != 1 ||
		abandon.TerminalAttemptCount != workload.MaxAttempts || abandon.PostTerminalStartRefusals != 1 ||
		!abandon.PreviewBudgetPreserved || !abandon.DeadLetterTerminal ||
		measurements.Abandon.Count != abandon.PreviewAbandons+abandon.FailureAbandons+abandon.ExpiryInterruptions {
		return errors.New("invalid abandon and requeue outcomes")
	}
	return nil
}

var (
	curationResultSchemaOnce     sync.Once
	curationResultSchemaResolved *jsonschema.Resolved
	curationResultSchemaErr      error
)

func resolvedCurationResultSchema() (*jsonschema.Resolved, error) {
	curationResultSchemaOnce.Do(func() {
		var schema jsonschema.Schema
		if err := json.Unmarshal(curationResultSchemaJSON, &schema); err != nil {
			curationResultSchemaErr = fmt.Errorf("decode embedded curation result schema: %w", err)
			return
		}
		curationResultSchemaResolved, curationResultSchemaErr = schema.Resolve(nil)
		if curationResultSchemaErr != nil {
			curationResultSchemaErr = fmt.Errorf("resolve embedded curation result schema: %w", curationResultSchemaErr)
		}
	})
	return curationResultSchemaResolved, curationResultSchemaErr
}
