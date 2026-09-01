package loadquality

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// Concurrency load-result schemas, deterministic defaults, and hard workload
// bounds. ConcurrencyAgentBatchDeadline is one proportional deadline budget
// unit for the fixed agent batch; it is an execution guard, not an SLO.
const (
	ConcurrencyResultSchemaV1 = "witself.memory-concurrency-load-result.v1"
	ConcurrencyHarnessVersion = "1"

	ConcurrencyAgentBatchDeadline = 2 * time.Minute
	ConcurrencyAgentBatchSize     = 8

	DefaultConcurrencySeed                  = 20260901
	DefaultConcurrencyAccounts              = 4
	DefaultConcurrencyRealmsPerAccount      = 2
	DefaultConcurrencyAgentsPerRealm        = 4
	DefaultConcurrencySeedMemoriesPerAgent  = 4
	DefaultConcurrencyWorkersPerAgent       = 2
	DefaultConcurrencyOperationsPerWorker   = 2
	DefaultConcurrencyIsolationIterations   = 2
	DefaultConcurrencyClaimWorkers          = 4
	MinimumConcurrencyAccounts              = 2
	MaximumConcurrencyAccounts              = 8
	MinimumConcurrencyRealmsPerAccount      = 2
	MaximumConcurrencyRealmsPerAccount      = 4
	MinimumConcurrencyAgentsPerRealm        = 2
	MaximumConcurrencyAgentsPerRealm        = 8
	MaximumConcurrencySeedMemoriesPerAgent  = 64
	MaximumConcurrencyWorkersPerAgent       = 8
	MaximumConcurrencyOperationsPerWorker   = 50
	MaximumConcurrencyIsolationIterations   = 50
	MaximumConcurrencyClaimWorkers          = 32
	ConcurrencyIsolationDimensions          = 3
	ConcurrencyIsolationCallsPerProbeRound  = 7
	ConcurrencyForeignClaimProbesPerRequest = 3
)

// Environment variables accepted by ParseConcurrencyOptions.
const (
	EnvConcurrencyResultsPath          = "WITSELF_MEMORY_CONCURRENCY_LOAD_RESULTS"
	EnvConcurrencySeed                 = "WITSELF_MEMORY_CONCURRENCY_LOAD_SEED"
	EnvConcurrencyAccounts             = "WITSELF_MEMORY_CONCURRENCY_LOAD_ACCOUNTS"
	EnvConcurrencyRealmsPerAccount     = "WITSELF_MEMORY_CONCURRENCY_LOAD_REALMS_PER_ACCOUNT"
	EnvConcurrencyAgentsPerRealm       = "WITSELF_MEMORY_CONCURRENCY_LOAD_AGENTS_PER_REALM"
	EnvConcurrencySeedMemoriesPerAgent = "WITSELF_MEMORY_CONCURRENCY_LOAD_SEED_MEMORIES_PER_AGENT"
	EnvConcurrencyWorkersPerAgent      = "WITSELF_MEMORY_CONCURRENCY_LOAD_WORKERS_PER_AGENT"
	EnvConcurrencyOperationsPerWorker  = "WITSELF_MEMORY_CONCURRENCY_LOAD_OPERATIONS_PER_WORKER"
	EnvConcurrencyIsolationIterations  = "WITSELF_MEMORY_CONCURRENCY_LOAD_ISOLATION_ITERATIONS"
	EnvConcurrencyClaimWorkers         = "WITSELF_MEMORY_CONCURRENCY_LOAD_CLAIM_WORKERS"
	EnvConcurrencyRelease              = "WITSELF_MEMORY_CONCURRENCY_LOAD_RELEASE"
	EnvConcurrencyCommit               = "WITSELF_MEMORY_CONCURRENCY_LOAD_COMMIT"
	EnvConcurrencyProvider             = "WITSELF_MEMORY_CONCURRENCY_LOAD_PROVIDER"
	EnvConcurrencyHardwareTier         = "WITSELF_MEMORY_CONCURRENCY_LOAD_HARDWARE_TIER"
)

//go:embed testdata/concurrency-result-schema.v1.json
var concurrencyResultSchemaJSON []byte

// ConcurrencyOptions contains only bounded workload controls and safe evidence
// metadata. Database location, principals, ids, fixture values, and queries are
// deliberately absent.
type ConcurrencyOptions struct {
	ResultsPath          string
	Seed                 int64
	Accounts             int
	RealmsPerAccount     int
	AgentsPerRealm       int
	SeedMemoriesPerAgent int
	WorkersPerAgent      int
	OperationsPerWorker  int
	IsolationIterations  int
	ClaimWorkers         int
	Release              string
	Commit               string
	Provider             string
	HardwareTier         string
}

// ConcurrencyWorkload records the complete bounded synthetic fleet shape.
type ConcurrencyWorkload struct {
	Seed                 int64 `json:"seed"`
	SyntheticAccounts    int   `json:"synthetic_accounts"`
	RealmsPerAccount     int   `json:"realms_per_account"`
	AgentsPerRealm       int   `json:"agents_per_realm"`
	SyntheticRealms      int   `json:"synthetic_realms"`
	SyntheticPrincipals  int   `json:"synthetic_principals"`
	SeedMemoriesPerAgent int   `json:"seed_memories_per_agent"`
	WorkersPerAgent      int   `json:"workers_per_agent"`
	OperationsPerWorker  int   `json:"operations_per_worker"`
	IsolationIterations  int   `json:"isolation_iterations"`
	ClaimWorkers         int   `json:"claim_workers"`
}

// ConcurrencyMeasurements retains one latency distribution for every store
// operation family exercised across the whole synthetic fleet.
type ConcurrencyMeasurements struct {
	Seed            OperationStats `json:"seed"`
	MixedCapture    OperationStats `json:"mixed_capture"`
	MixedRecall     OperationStats `json:"mixed_recall"`
	MixedAdjust     OperationStats `json:"mixed_adjust"`
	IsolationProbe  OperationStats `json:"isolation_probe"`
	CurationRequest OperationStats `json:"curation_request"`
	CurationClaim   OperationStats `json:"curation_claim"`
	CurationApply   OperationStats `json:"curation_apply"`
	SensitiveFanout OperationStats `json:"sensitive_fanout"`
}

// ConcurrencyTopologyOutcome proves the complete account/realm/agent fixture
// was created with one sensitive row and a unique canary set per principal.
type ConcurrencyTopologyOutcome struct {
	Accounts            int  `json:"accounts"`
	Realms              int  `json:"realms"`
	Principals          int  `json:"principals"`
	CanaryMemories      int  `json:"canary_memories"`
	SensitiveMemories   int  `json:"sensitive_memories"`
	SeededMemories      int  `json:"seeded_memories"`
	AllPrincipalsSeeded bool `json:"all_principals_seeded"`
	AllCanariesUnique   bool `json:"all_canaries_unique"`
	AllSensitiveSeeded  bool `json:"all_sensitive_seeded"`
}

// ConcurrencyMixedOperationsOutcome records exact row-level assertions made
// inline while each worker captures, recalls, and adjusts its own fixtures.
type ConcurrencyMixedOperationsOutcome struct {
	Workers                     int  `json:"workers"`
	OperationBatches            int  `json:"operation_batches"`
	CaptureCalls                int  `json:"capture_calls"`
	RecallCalls                 int  `json:"recall_calls"`
	AdjustCalls                 int  `json:"adjust_calls"`
	RecallHits                  int  `json:"recall_hits"`
	OwnerChecks                 int  `json:"owner_checks"`
	ForeignHits                 int  `json:"foreign_hits"`
	OverlapOperationSamples     int  `json:"overlap_operation_samples"`
	ExactRecallValues           bool `json:"exact_recall_values"`
	ExactAdjustValues           bool `json:"exact_adjust_values"`
	AllHitsExactOwner           bool `json:"all_hits_exact_owner"`
	WholeFleetStartSynchronized bool `json:"whole_fleet_start_synchronized"`
	AllOperationsComplete       bool `json:"all_operations_complete"`
}

// ConcurrencyIsolationOutcome aggregates the seven-call probe round executed
// per principal and iteration. Its booleans are roll-ups of inline exact count,
// owner, marker, redaction, and typed isolation assertions.
type ConcurrencyIsolationOutcome struct {
	ProbeAgents              int  `json:"probe_agents"`
	ProbeRounds              int  `json:"probe_rounds"`
	BroadRecallCalls         int  `json:"broad_recall_calls"`
	BroadHits                int  `json:"broad_hits"`
	BroadVisibleCanaries     int  `json:"broad_visible_canaries"`
	BroadSensitiveRedactions int  `json:"broad_sensitive_redactions"`
	OwnControlRecallCalls    int  `json:"own_control_recall_calls"`
	OwnControlHits           int  `json:"own_control_hits"`
	CrossAccountRecallCalls  int  `json:"cross_account_recall_calls"`
	CrossRealmRecallCalls    int  `json:"cross_realm_recall_calls"`
	CrossAgentRecallCalls    int  `json:"cross_agent_recall_calls"`
	MarkerScans              int  `json:"marker_scans"`
	ForeignHits              int  `json:"foreign_hits"`
	ForeignCanaryHits        int  `json:"foreign_canary_hits"`
	SensitiveContentHits     int  `json:"sensitive_content_hits"`
	BroadCountsExact         bool `json:"broad_counts_exact"`
	OwnCountsExact           bool `json:"own_counts_exact"`
	AllHitsExactOwner        bool `json:"all_hits_exact_owner"`
	NoForeignCanaries        bool `json:"no_foreign_canaries"`
	NoSensitiveContent       bool `json:"no_sensitive_content"`
	CrossAccountIsolated     bool `json:"cross_account_isolated"`
	CrossRealmIsolated       bool `json:"cross_realm_isolated"`
	CrossAgentIsolated       bool `json:"cross_agent_isolated"`
}

// ConcurrencyCurationClaimsOutcome aggregates one request and apply per owner,
// an owner-only claim race, and three typed foreign-scope probes per request.
type ConcurrencyCurationClaimsOutcome struct {
	Requests                int  `json:"requests"`
	RequestCalls            int  `json:"request_calls"`
	OwnerClaimAttempts      int  `json:"owner_claim_attempts"`
	OwnerClaimWins          int  `json:"owner_claim_wins"`
	OwnerClaimLosses        int  `json:"owner_claim_losses"`
	ForeignClaimAttempts    int  `json:"foreign_claim_attempts"`
	CrossAccountRefusals    int  `json:"cross_account_refusals"`
	CrossRealmRefusals      int  `json:"cross_realm_refusals"`
	CrossAgentRefusals      int  `json:"cross_agent_refusals"`
	TypedForeignRefusals    int  `json:"typed_foreign_refusals"`
	ForeignClaimWins        int  `json:"foreign_claim_wins"`
	ApplyCalls              int  `json:"apply_calls"`
	OwnerCursorAdvances     int  `json:"owner_cursor_advances"`
	ForeignCursorAdvances   int  `json:"foreign_cursor_advances"`
	SingleWinnerPerRequest  bool `json:"single_winner_per_request"`
	AllForeignClaimsTyped   bool `json:"all_foreign_claims_typed"`
	OnlyOwnerCursorAdvanced bool `json:"only_owner_cursor_advanced"`
	AllRequestsApplied      bool `json:"all_requests_applied"`
}

// ConcurrencySensitiveFanoutOutcome proves the exact owner can read one
// sensitive fixture while every other principal receives zero rows.
type ConcurrencySensitiveFanoutOutcome struct {
	QueryCalls                int  `json:"query_calls"`
	OwnerQueryCalls           int  `json:"owner_query_calls"`
	ForeignQueryCalls         int  `json:"foreign_query_calls"`
	OwnerHits                 int  `json:"owner_hits"`
	ForeignHits               int  `json:"foreign_hits"`
	SensitiveContentLeaks     int  `json:"sensitive_content_leaks"`
	OwnerExactReadSucceeded   bool `json:"owner_exact_read_succeeded"`
	AllForeignQueriesIsolated bool `json:"all_foreign_queries_isolated"`
}

// ConcurrencyOutcomes groups value-free aggregate evidence for all five
// concurrency and isolation workloads.
type ConcurrencyOutcomes struct {
	Topology        ConcurrencyTopologyOutcome        `json:"topology"`
	MixedOperations ConcurrencyMixedOperationsOutcome `json:"mixed_operations"`
	Isolation       ConcurrencyIsolationOutcome       `json:"isolation"`
	CurationClaims  ConcurrencyCurationClaimsOutcome  `json:"curation_claims"`
	SensitiveFanout ConcurrencySensitiveFanoutOutcome `json:"sensitive_fanout"`
}

// ConcurrencyResult is a sanitized artifact safe to retain as CI or release
// evidence. PostgreSQLVersion is software metadata, never endpoint identity.
type ConcurrencyResult struct {
	Schema            string                  `json:"schema"`
	HarnessVersion    string                  `json:"harness_version"`
	StartedAt         time.Time               `json:"started_at"`
	CompletedAt       time.Time               `json:"completed_at"`
	Outcome           string                  `json:"outcome"`
	PostgreSQLVersion string                  `json:"postgresql_version"`
	Environment       SafeMetadata            `json:"environment"`
	Workload          ConcurrencyWorkload     `json:"workload"`
	Measurements      ConcurrencyMeasurements `json:"measurements"`
	Outcomes          ConcurrencyOutcomes     `json:"outcomes"`
}

// ConcurrencyResultJSONSchema returns a fresh copy of the checked-in schema.
func ConcurrencyResultJSONSchema() []byte {
	return append([]byte(nil), concurrencyResultSchemaJSON...)
}

// ParseConcurrencyOptions reads bounded controls through an injected lookup
// so unit tests never mutate the process environment.
func ParseConcurrencyOptions(getenv func(string) string) (ConcurrencyOptions, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	opts := ConcurrencyOptions{
		ResultsPath:          strings.TrimSpace(getenv(EnvConcurrencyResultsPath)),
		Seed:                 DefaultConcurrencySeed,
		Accounts:             DefaultConcurrencyAccounts,
		RealmsPerAccount:     DefaultConcurrencyRealmsPerAccount,
		AgentsPerRealm:       DefaultConcurrencyAgentsPerRealm,
		SeedMemoriesPerAgent: DefaultConcurrencySeedMemoriesPerAgent,
		WorkersPerAgent:      DefaultConcurrencyWorkersPerAgent,
		OperationsPerWorker:  DefaultConcurrencyOperationsPerWorker,
		IsolationIterations:  DefaultConcurrencyIsolationIterations,
		ClaimWorkers:         DefaultConcurrencyClaimWorkers,
		Release:              metadataOrDefault(getenv(EnvConcurrencyRelease), "dev"),
		Commit:               metadataOrDefault(getenv(EnvConcurrencyCommit), "none"),
		Provider:             metadataOrDefault(getenv(EnvConcurrencyProvider), "local"),
		HardwareTier:         metadataOrDefault(getenv(EnvConcurrencyHardwareTier), "unspecified"),
	}
	if opts.ResultsPath == "" {
		opts.ResultsPath = fmt.Sprintf("/tmp/witself-memory-concurrency-load-%d.json", os.Getpid())
	}
	var err error
	if opts.Seed, err = parseInt64(getenv(EnvConcurrencySeed), DefaultConcurrencySeed, math.MinInt64, math.MaxInt64, EnvConcurrencySeed); err != nil {
		return ConcurrencyOptions{}, err
	}
	if opts.Accounts, err = parseInt(getenv(EnvConcurrencyAccounts), DefaultConcurrencyAccounts, MinimumConcurrencyAccounts, MaximumConcurrencyAccounts, EnvConcurrencyAccounts); err != nil {
		return ConcurrencyOptions{}, err
	}
	if opts.RealmsPerAccount, err = parseInt(getenv(EnvConcurrencyRealmsPerAccount), DefaultConcurrencyRealmsPerAccount, MinimumConcurrencyRealmsPerAccount, MaximumConcurrencyRealmsPerAccount, EnvConcurrencyRealmsPerAccount); err != nil {
		return ConcurrencyOptions{}, err
	}
	if opts.AgentsPerRealm, err = parseInt(getenv(EnvConcurrencyAgentsPerRealm), DefaultConcurrencyAgentsPerRealm, MinimumConcurrencyAgentsPerRealm, MaximumConcurrencyAgentsPerRealm, EnvConcurrencyAgentsPerRealm); err != nil {
		return ConcurrencyOptions{}, err
	}
	if opts.SeedMemoriesPerAgent, err = parseInt(getenv(EnvConcurrencySeedMemoriesPerAgent), DefaultConcurrencySeedMemoriesPerAgent, 1, MaximumConcurrencySeedMemoriesPerAgent, EnvConcurrencySeedMemoriesPerAgent); err != nil {
		return ConcurrencyOptions{}, err
	}
	if opts.WorkersPerAgent, err = parseInt(getenv(EnvConcurrencyWorkersPerAgent), DefaultConcurrencyWorkersPerAgent, 2, MaximumConcurrencyWorkersPerAgent, EnvConcurrencyWorkersPerAgent); err != nil {
		return ConcurrencyOptions{}, err
	}
	if opts.OperationsPerWorker, err = parseInt(getenv(EnvConcurrencyOperationsPerWorker), DefaultConcurrencyOperationsPerWorker, 1, MaximumConcurrencyOperationsPerWorker, EnvConcurrencyOperationsPerWorker); err != nil {
		return ConcurrencyOptions{}, err
	}
	if opts.IsolationIterations, err = parseInt(getenv(EnvConcurrencyIsolationIterations), DefaultConcurrencyIsolationIterations, 1, MaximumConcurrencyIsolationIterations, EnvConcurrencyIsolationIterations); err != nil {
		return ConcurrencyOptions{}, err
	}
	if opts.ClaimWorkers, err = parseInt(getenv(EnvConcurrencyClaimWorkers), DefaultConcurrencyClaimWorkers, 2, MaximumConcurrencyClaimWorkers, EnvConcurrencyClaimWorkers); err != nil {
		return ConcurrencyOptions{}, err
	}
	if opts.WorkersPerAgent > opts.SeedMemoriesPerAgent {
		return ConcurrencyOptions{}, fmt.Errorf("%s must not exceed %s so every worker has a unique adjustment target", EnvConcurrencyWorkersPerAgent, EnvConcurrencySeedMemoriesPerAgent)
	}
	for _, item := range []struct {
		name  string
		value string
	}{{EnvConcurrencyRelease, opts.Release}, {EnvConcurrencyCommit, opts.Commit}} {
		if !safeMetadata(item.value) {
			return ConcurrencyOptions{}, fmt.Errorf("%s contains unsafe evidence metadata", item.name)
		}
	}
	for _, item := range []struct {
		name  string
		value string
	}{{EnvConcurrencyProvider, opts.Provider}, {EnvConcurrencyHardwareTier, opts.HardwareTier}} {
		if !curationLabelMetadata(item.value) {
			return ConcurrencyOptions{}, fmt.Errorf("%s must be a dotless label (letters, digits, '+', '_', '-')", item.name)
		}
	}
	return opts, nil
}

// ConcurrencyPrincipalCount returns the exact bounded account/realm/agent
// product used by every measurement formula.
func ConcurrencyPrincipalCount(accounts, realmsPerAccount, agentsPerRealm int) (int, error) {
	if accounts < MinimumConcurrencyAccounts || accounts > MaximumConcurrencyAccounts ||
		realmsPerAccount < MinimumConcurrencyRealmsPerAccount || realmsPerAccount > MaximumConcurrencyRealmsPerAccount ||
		agentsPerRealm < MinimumConcurrencyAgentsPerRealm || agentsPerRealm > MaximumConcurrencyAgentsPerRealm {
		return 0, errors.New("concurrency topology is outside harness bounds")
	}
	return accounts * realmsPerAccount * agentsPerRealm, nil
}

// ConcurrencyPhaseDeadline returns one complete deadline budget unit per
// eight-principal agent batch. Keeping the ceil(P/8) calculation in the shared
// contract prevents a scaling driver from regressing to one flat deadline.
func ConcurrencyPhaseDeadline(principals int) (time.Duration, error) {
	minimumPrincipals := MinimumConcurrencyAccounts * MinimumConcurrencyRealmsPerAccount * MinimumConcurrencyAgentsPerRealm
	maximumPrincipals := MaximumConcurrencyAccounts * MaximumConcurrencyRealmsPerAccount * MaximumConcurrencyAgentsPerRealm
	if principals < minimumPrincipals || principals > maximumPrincipals {
		return 0, errors.New("concurrency principal count is outside harness bounds")
	}
	batches := (principals + ConcurrencyAgentBatchSize - 1) / ConcurrencyAgentBatchSize
	return time.Duration(batches) * ConcurrencyAgentBatchDeadline, nil
}

// ConcurrencyEnvironment builds the safe runner metadata retained in evidence.
func ConcurrencyEnvironment(opts ConcurrencyOptions) SafeMetadata {
	return SafeMetadata{
		Release: opts.Release, Commit: opts.Commit, Provider: opts.Provider,
		HardwareTier: opts.HardwareTier, GoVersion: runtime.Version(),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, LogicalCPUs: runtime.NumCPU(),
	}
}

// ValidateConcurrencyResult requires a complete passing run and exact
// agreement between every measurement count, counter, workload formula, and
// assertion roll-up. It imposes no absolute latency threshold.
func ValidateConcurrencyResult(result ConcurrencyResult) error {
	if result.Schema != ConcurrencyResultSchemaV1 || result.HarnessVersion != ConcurrencyHarnessVersion ||
		result.Outcome != "pass" || result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) ||
		strings.TrimSpace(result.PostgreSQLVersion) == "" || len(result.PostgreSQLVersion) > 128 {
		return errors.New("invalid concurrency load result envelope")
	}
	if !validConcurrencyEnvironment(result.Environment) {
		return errors.New("invalid concurrency load result environment")
	}
	if err := validateConcurrencyWorkload(result.Workload); err != nil {
		return err
	}
	for _, stats := range []OperationStats{
		result.Measurements.Seed, result.Measurements.MixedCapture,
		result.Measurements.MixedRecall, result.Measurements.MixedAdjust,
		result.Measurements.IsolationProbe, result.Measurements.CurationRequest,
		result.Measurements.CurationClaim, result.Measurements.CurationApply,
		result.Measurements.SensitiveFanout,
	} {
		if !validOperationStats(stats) {
			return errors.New("invalid concurrency operation measurements")
		}
	}
	if err := validateConcurrencyOutcomes(result.Workload, result.Measurements, result.Outcomes); err != nil {
		return err
	}
	return nil
}

// MarshalConcurrencyResult performs semantic checks and then validates the
// exact JSON instance against the checked-in Draft 2020-12 schema.
func MarshalConcurrencyResult(result ConcurrencyResult) ([]byte, error) {
	if err := ValidateConcurrencyResult(result); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := ValidateConcurrencyResultJSON(raw); err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// ValidateConcurrencyResultJSON validates one JSON instance with the
// checked-in Draft 2020-12 result schema.
func ValidateConcurrencyResultJSON(raw []byte) error {
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return fmt.Errorf("decode concurrency load result JSON: %w", err)
	}
	resolved, err := resolvedConcurrencyResultSchema()
	if err != nil {
		return err
	}
	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("validate concurrency load result schema: %w", err)
	}
	return nil
}

// WriteConcurrencyResult writes with private permissions and atomically
// replaces an older artifact only after the complete document is synced.
func WriteConcurrencyResult(path string, result ConcurrencyResult) ([]byte, error) {
	raw, err := MarshalConcurrencyResult(result)
	if err != nil {
		return nil, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("concurrency load result path is required")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create concurrency load result directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".memory-concurrency-load-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create concurrency load result: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("protect concurrency load result: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("write concurrency load result: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("sync concurrency load result: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("close concurrency load result: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return nil, fmt.Errorf("publish concurrency load result: %w", err)
	}
	return raw, nil
}

func validConcurrencyEnvironment(environment SafeMetadata) bool {
	return safeMetadata(environment.Release) && safeMetadata(environment.Commit) &&
		curationLabelMetadata(environment.Provider) && curationLabelMetadata(environment.HardwareTier) &&
		len(environment.GoVersion) >= 1 && len(environment.GoVersion) <= 64 &&
		len(environment.GOOS) >= 1 && len(environment.GOOS) <= 32 &&
		len(environment.GOARCH) >= 1 && len(environment.GOARCH) <= 32 && environment.LogicalCPUs >= 1
}

func validateConcurrencyWorkload(workload ConcurrencyWorkload) error {
	principals, err := ConcurrencyPrincipalCount(
		workload.SyntheticAccounts, workload.RealmsPerAccount, workload.AgentsPerRealm,
	)
	if err != nil || workload.SyntheticRealms != workload.SyntheticAccounts*workload.RealmsPerAccount ||
		workload.SyntheticPrincipals != principals ||
		workload.SeedMemoriesPerAgent < 1 || workload.SeedMemoriesPerAgent > MaximumConcurrencySeedMemoriesPerAgent ||
		workload.WorkersPerAgent < 2 || workload.WorkersPerAgent > MaximumConcurrencyWorkersPerAgent ||
		workload.WorkersPerAgent > workload.SeedMemoriesPerAgent ||
		workload.OperationsPerWorker < 1 || workload.OperationsPerWorker > MaximumConcurrencyOperationsPerWorker ||
		workload.IsolationIterations < 1 || workload.IsolationIterations > MaximumConcurrencyIsolationIterations ||
		workload.ClaimWorkers < 2 || workload.ClaimWorkers > MaximumConcurrencyClaimWorkers {
		return errors.New("invalid concurrency load result workload")
	}
	return nil
}

func validateConcurrencyOutcomes(workload ConcurrencyWorkload, measurements ConcurrencyMeasurements, outcomes ConcurrencyOutcomes) error {
	principals := workload.SyntheticPrincipals
	realms := workload.SyntheticAccounts * workload.RealmsPerAccount
	canaries := principals * workload.SeedMemoriesPerAgent
	sensitive := principals
	topology := outcomes.Topology
	if topology.Accounts != workload.SyntheticAccounts || topology.Realms != realms ||
		topology.Principals != principals || topology.CanaryMemories != canaries ||
		topology.SensitiveMemories != sensitive || topology.SeededMemories != canaries+sensitive ||
		!topology.AllPrincipalsSeeded || !topology.AllCanariesUnique || !topology.AllSensitiveSeeded ||
		measurements.Seed.Count != topology.SeededMemories {
		return errors.New("invalid concurrency topology outcomes")
	}

	mixedCalls := principals * workload.WorkersPerAgent * workload.OperationsPerWorker
	probeRounds := principals * workload.IsolationIterations
	maximumOverlapSamples := ConcurrencyIsolationCallsPerProbeRound * probeRounds
	mixed := outcomes.MixedOperations
	if mixed.Workers != principals*workload.WorkersPerAgent || mixed.OperationBatches != mixedCalls ||
		mixed.CaptureCalls != mixedCalls || mixed.RecallCalls != mixedCalls || mixed.AdjustCalls != mixedCalls ||
		mixed.RecallHits != mixedCalls || mixed.OwnerChecks != mixed.RecallHits || mixed.ForeignHits != 0 ||
		mixed.OverlapOperationSamples < 0 || mixed.OverlapOperationSamples > maximumOverlapSamples ||
		(workload.IsolationIterations >= 2 && mixed.OverlapOperationSamples < 1) ||
		!mixed.ExactRecallValues || !mixed.ExactAdjustValues || !mixed.AllHitsExactOwner ||
		!mixed.WholeFleetStartSynchronized || !mixed.AllOperationsComplete ||
		measurements.MixedCapture.Count != mixed.CaptureCalls ||
		measurements.MixedRecall.Count != mixed.RecallCalls || measurements.MixedAdjust.Count != mixed.AdjustCalls {
		return errors.New("invalid concurrent mixed-operation outcomes")
	}

	isolation := outcomes.Isolation
	wantBroadHits := probeRounds * (workload.SeedMemoriesPerAgent + 1)
	wantOwnCalls := ConcurrencyIsolationDimensions * probeRounds
	wantProbeCalls := ConcurrencyIsolationCallsPerProbeRound * probeRounds
	if isolation.ProbeAgents != principals || isolation.ProbeRounds != probeRounds ||
		isolation.BroadRecallCalls != probeRounds || isolation.BroadHits != wantBroadHits ||
		isolation.BroadVisibleCanaries != probeRounds*workload.SeedMemoriesPerAgent ||
		isolation.BroadSensitiveRedactions != probeRounds ||
		isolation.OwnControlRecallCalls != wantOwnCalls || isolation.OwnControlHits != wantOwnCalls ||
		isolation.CrossAccountRecallCalls != probeRounds || isolation.CrossRealmRecallCalls != probeRounds ||
		isolation.CrossAgentRecallCalls != probeRounds ||
		isolation.MarkerScans != isolation.BroadHits+isolation.OwnControlHits ||
		isolation.ForeignHits != 0 || isolation.ForeignCanaryHits != 0 || isolation.SensitiveContentHits != 0 ||
		!isolation.BroadCountsExact || !isolation.OwnCountsExact || !isolation.AllHitsExactOwner ||
		!isolation.NoForeignCanaries || !isolation.NoSensitiveContent ||
		!isolation.CrossAccountIsolated || !isolation.CrossRealmIsolated || !isolation.CrossAgentIsolated ||
		measurements.IsolationProbe.Count != wantProbeCalls {
		return errors.New("invalid isolation-under-load outcomes")
	}

	curation := outcomes.CurationClaims
	wantOwnerClaims := principals * workload.ClaimWorkers
	wantForeignClaims := principals * ConcurrencyForeignClaimProbesPerRequest
	if curation.Requests != principals || curation.RequestCalls != principals ||
		curation.OwnerClaimAttempts != wantOwnerClaims || curation.OwnerClaimWins != principals ||
		curation.OwnerClaimLosses != wantOwnerClaims-principals ||
		curation.ForeignClaimAttempts != wantForeignClaims ||
		curation.CrossAccountRefusals != principals || curation.CrossRealmRefusals != principals ||
		curation.CrossAgentRefusals != principals || curation.TypedForeignRefusals != wantForeignClaims ||
		curation.ForeignClaimWins != 0 || curation.ApplyCalls != principals ||
		curation.OwnerCursorAdvances != principals || curation.ForeignCursorAdvances != 0 ||
		!curation.SingleWinnerPerRequest || !curation.AllForeignClaimsTyped ||
		!curation.OnlyOwnerCursorAdvanced || !curation.AllRequestsApplied ||
		measurements.CurationRequest.Count != curation.RequestCalls ||
		measurements.CurationClaim.Count != wantOwnerClaims+wantForeignClaims ||
		measurements.CurationApply.Count != curation.ApplyCalls {
		return errors.New("invalid concurrent curation-claim outcomes")
	}

	fanout := outcomes.SensitiveFanout
	if fanout.QueryCalls != principals || fanout.OwnerQueryCalls != 1 ||
		fanout.ForeignQueryCalls != principals-1 || fanout.OwnerHits != 1 ||
		fanout.ForeignHits != 0 || fanout.SensitiveContentLeaks != 0 ||
		!fanout.OwnerExactReadSucceeded || !fanout.AllForeignQueriesIsolated ||
		measurements.SensitiveFanout.Count != fanout.QueryCalls {
		return errors.New("invalid sensitive fan-out outcomes")
	}
	return nil
}

var (
	concurrencyResultSchemaOnce     sync.Once
	concurrencyResultSchemaResolved *jsonschema.Resolved
	concurrencyResultSchemaErr      error
)

func resolvedConcurrencyResultSchema() (*jsonschema.Resolved, error) {
	concurrencyResultSchemaOnce.Do(func() {
		var schema jsonschema.Schema
		if err := json.Unmarshal(concurrencyResultSchemaJSON, &schema); err != nil {
			concurrencyResultSchemaErr = fmt.Errorf("decode embedded concurrency result schema: %w", err)
			return
		}
		concurrencyResultSchemaResolved, concurrencyResultSchemaErr = schema.Resolve(nil)
		if concurrencyResultSchemaErr != nil {
			concurrencyResultSchemaErr = fmt.Errorf("resolve embedded concurrency result schema: %w", concurrencyResultSchemaErr)
		}
	})
	return concurrencyResultSchemaResolved, concurrencyResultSchemaErr
}
