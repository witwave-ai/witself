package loadquality

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseCurationOptionsDefaultsAndSignedSeed(t *testing.T) {
	values := map[string]string{EnvCurationResultsPath: filepath.Join(t.TempDir(), "result.json")}
	getenv := func(name string) string { return values[name] }
	opts, err := ParseCurationOptions(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Seed != DefaultCurationSeed || opts.CoalescingRequests != DefaultCurationCoalescingRequests ||
		opts.ClaimRequests != DefaultCurationClaimRequests || opts.ClaimWorkers != DefaultCurationClaimWorkers ||
		!reflect.DeepEqual(opts.PagingCardinalities, []int{4, 16, 64}) ||
		opts.PageSize != DefaultCurationPageSize || opts.ChainBacklog != DefaultCurationChainBacklog ||
		opts.ChainCap != DefaultCurationChainCap || opts.LeaseCycles != DefaultCurationLeaseCycles ||
		opts.MaxAttempts != DefaultCurationMaxAttempts || opts.Release != "dev" || opts.Provider != "local" {
		t.Fatalf("default curation options = %#v", opts)
	}
	depth, err := CurationChainDepth(opts.ChainBacklog, opts.ChainCap)
	if err != nil || depth != 4 {
		t.Fatalf("default curation chain depth = %d, %v", depth, err)
	}

	values[EnvCurationSeed] = "-9223372036854775808"
	values[EnvCurationPagingCardinalities] = " 1, 9, 500 "
	opts, err = ParseCurationOptions(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Seed != -9223372036854775808 || !reflect.DeepEqual(opts.PagingCardinalities, []int{1, 9, 500}) {
		t.Fatalf("explicit curation options = %#v", opts)
	}

	// Parsed defaults are fresh and cannot be mutated across calls.
	opts.PagingCardinalities[0] = 99
	delete(values, EnvCurationPagingCardinalities)
	again, err := ParseCurationOptions(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.PagingCardinalities, []int{4, 16, 64}) {
		t.Fatalf("reparsed cardinalities = %#v", again.PagingCardinalities)
	}
}

func TestParseCurationOptionsRejectsUnboundedOrAmbiguousShapes(t *testing.T) {
	base := map[string]string{EnvCurationResultsPath: filepath.Join(t.TempDir(), "result.json")}
	tests := []struct {
		name  string
		env   string
		value string
	}{
		{"claim workers below contention minimum", EnvCurationClaimWorkers, "1"},
		{"claim workers above maximum", EnvCurationClaimWorkers, "65"},
		{"duplicate paging cardinality", EnvCurationPagingCardinalities, "4,4"},
		{"descending paging cardinality", EnvCurationPagingCardinalities, "16,4"},
		{"too few paging cardinalities", EnvCurationPagingCardinalities, "4"},
		{"too many paging cardinalities", EnvCurationPagingCardinalities, "1,2,3,4,5,6"},
		{"paging cardinality above maximum", EnvCurationPagingCardinalities, "4,501"},
		{"page size above maximum", EnvCurationPageSize, "201"},
		{"chain cap does not force follow-up", EnvCurationChainCap, "24"},
		{"chain depth above maximum", EnvCurationChainBacklog, "385"},
		{"lease cycles above maximum", EnvCurationLeaseCycles, "21"},
		{"max attempts below repeat minimum", EnvCurationMaxAttempts, "1"},
		{"max attempts above maximum", EnvCurationMaxAttempts, "21"},
		{"unsafe metadata", EnvCurationRelease, "postgres://user:password@host/db"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := make(map[string]string, len(base)+1)
			for name, value := range base {
				values[name] = value
			}
			values[test.env] = test.value
			if test.env == EnvCurationChainBacklog {
				values[EnvCurationChainCap] = "6"
			}
			_, err := ParseCurationOptions(func(name string) string { return values[name] })
			if err == nil {
				t.Fatal("invalid curation options unexpectedly accepted")
			}
		})
	}
}

func TestCurationChainDepthAndRatioAreDeterministic(t *testing.T) {
	depth, err := CurationChainDepth(25, 6)
	if err != nil || depth != 5 {
		t.Fatalf("chain depth = %d, %v", depth, err)
	}
	if _, err := CurationChainDepth(6, 6); err == nil {
		t.Fatal("single-cycle chain unexpectedly accepted")
	}
	if got := CurationRatio(23, 24); got != 0.958 {
		t.Fatalf("curation ratio = %v, want 0.958", got)
	}
	if got := CurationRatio(2, 1); got != 0 {
		t.Fatalf("invalid curation ratio = %v, want 0", got)
	}
}

func TestCurationResultSchemaIsCheckedInResolvedAndValidatesInstances(t *testing.T) {
	first := CurationResultJSONSchema()
	second := CurationResultJSONSchema()
	if len(first) == 0 || len(second) == 0 {
		t.Fatal("embedded curation result schema is empty")
	}
	first[0] = 'x'
	if second[0] != '{' {
		t.Fatal("curation result schema did not return a fresh copy")
	}
	var schema map[string]any
	if err := json.Unmarshal(second, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" ||
		schema["$id"] != "https://witself.witwave.ai/schemas/memory-curation-load-result.v1.schema.json" {
		t.Fatalf("curation result schema identity = %#v", schema)
	}
	if _, err := resolvedCurationResultSchema(); err != nil {
		t.Fatalf("resolve checked-in Draft 2020-12 schema: %v", err)
	}

	result := validCurationTestResult(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	raw, err := MarshalCurationResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurationResultJSON(raw); err != nil {
		t.Fatalf("validate valid curation result JSON: %v", err)
	}
	result.Workload.Seed = -9223372036854775808
	if _, err := MarshalCurationResult(result); err != nil {
		t.Fatalf("marshal signed 64-bit minimum seed: %v", err)
	}

	var invalid map[string]any
	if err := json.Unmarshal(raw, &invalid); err != nil {
		t.Fatal(err)
	}
	invalid["unexpected"] = true
	invalidRaw, err := json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurationResultJSON(invalidRaw); err == nil {
		t.Fatal("schema accepted an unexpected top-level property")
	}
	delete(invalid, "unexpected")
	lease := invalid["outcomes"].(map[string]any)["lease_churn"].(map[string]any)
	lease["no_double_apply"] = false
	invalidRaw, err = json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurationResultJSON(invalidRaw); err == nil {
		t.Fatal("schema accepted failed no-double-apply evidence")
	}
}

func TestValidateCurationResultRejectsPartialOrFailedRuns(t *testing.T) {
	result := validCurationTestResult(time.Now().UTC())
	result.Measurements.ClaimStart.Count--
	if err := ValidateCurationResult(result); err == nil || !strings.Contains(err.Error(), "claim contention") {
		t.Fatalf("claim count mismatch error = %v", err)
	}

	result = validCurationTestResult(time.Now().UTC())
	result.Outcomes.LeaseChurn.NoDoubleApply = false
	if err := ValidateCurationResult(result); err == nil || !strings.Contains(err.Error(), "lease churn") {
		t.Fatalf("double apply error = %v", err)
	}

	result = validCurationTestResult(time.Now().UTC())
	result.Workload.SyntheticAgents--
	if err := ValidateCurationResult(result); err == nil || !strings.Contains(err.Error(), "workload") {
		t.Fatalf("partial fixture error = %v", err)
	}

	result = validCurationTestResult(time.Now().UTC())
	result.Outcomes.InputPaging.Inputs--
	if err := ValidateCurationResult(result); err == nil || !strings.Contains(err.Error(), "input paging") {
		t.Fatalf("paging formula error = %v", err)
	}

	result = validCurationTestResult(time.Now().UTC())
	result.Outcomes.PlanLifecycle.EmptyApplies = 1
	result.Outcomes.PlanLifecycle.CreateApplies = 3
	result.Outcomes.PlanLifecycle.CreateActions = 3
	result.Outcomes.PlanLifecycle.EmptyCursorAdvances = 1
	if err := ValidateCurationResult(result); err == nil || !strings.Contains(err.Error(), "plan lifecycle") {
		t.Fatalf("deterministic plan mix error = %v", err)
	}

	result = validCurationTestResult(time.Now().UTC())
	result.Outcomes.StalePlanConflict.StaleFenceRefusals = 8
	result.Outcomes.StalePlanConflict.TypedRefusals = 10
	result.Measurements.TypedRefusal.Count = 10
	if err := ValidateCurationResult(result); err == nil || !strings.Contains(err.Error(), "stale-plan") {
		t.Fatalf("stale-fence formula error = %v", err)
	}

	result = validCurationTestResult(time.Now().UTC())
	result.Outcomes.AbandonRequeue.FailureAbandons = 3
	result.Outcomes.AbandonRequeue.ExpiryInterruptions = 0
	if err := ValidateCurationResult(result); err == nil || !strings.Contains(err.Error(), "abandon") {
		t.Fatalf("deterministic abandon mix error = %v", err)
	}
}

func TestWriteCurationResultIsPrivateAtomicAndSanitized(t *testing.T) {
	result := validCurationTestResult(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "nested", "curation-result.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := WriteCurationResult(path, result)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("curation result mode = %o, want 600", got)
	}
	for _, forbidden := range []string{
		"postgres://", "password", "hostname", "account_id", "agent_id",
		"memory_id", "request_id", "run_id", "\"plan_hash\":", "token",
		"synthetic transcript content", "secret fixture",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("curation result contains forbidden value %q:\n%s", forbidden, raw)
		}
	}
	var decoded CurationResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != CurationResultSchemaV1 || !decoded.Outcomes.LeaseChurn.NoDoubleApply {
		t.Fatalf("decoded curation result = %#v", decoded)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("result directory contains temporary artifacts: %#v", entries)
	}
}

func validCurationTestResult(started time.Time) CurationResult {
	workload := CurationWorkload{
		Seed: DefaultCurationSeed, SyntheticAccounts: 2, SyntheticAgents: 17,
		CoalescingRequests: 24, ClaimRequests: 6, ClaimWorkers: 4,
		PagingCardinalities: []int{4, 16, 64}, PageSize: 8,
		ChainBacklog: 24, ChainCap: 6, ChainDepth: 4,
		LeaseCycles: 3, MaxAttempts: 3,
	}
	stats := func(count int) OperationStats {
		return OperationStats{
			Count: count, WallDurationMS: float64(count), ThroughputPerSecond: 1000,
			MinimumMS: 0.1, P50MS: 0.2, P95MS: 0.4, P99MS: 0.5, MaximumMS: 0.5,
		}
	}
	return CurationResult{
		Schema: CurationResultSchemaV1, HarnessVersion: CurationHarnessVersion,
		StartedAt: started, CompletedAt: started.Add(time.Second), Outcome: "pass",
		PostgreSQLVersion: "18.0",
		Environment: SafeMetadata{
			Release: "v0.0.267", Commit: "67ec81d", Provider: "gcp",
			HardwareTier: "db-custom-2-7680", GoVersion: "go1.26.6",
			GOOS: "darwin", GOARCH: "arm64", LogicalCPUs: 8,
		},
		Workload: workload,
		Measurements: CurationMeasurements{
			RequestCoalescing: stats(24), ClaimStart: stats(24), InputPage: stats(45),
			Plan: stats(4), PlanGet: stats(4), Apply: stats(4),
			LeaseRenew: stats(6), LeaseApplyRace: stats(4), TypedRefusal: stats(11),
			Abandon: stats(4),
		},
		Outcomes: CurationOutcomes{
			RequestCoalescing: CurationRequestCoalescingOutcome{
				Calls: 24, Created: 1, Coalesced: 23, QueueDepth: 1,
				CoalescingRatio: 0.958, AllCoalesced: true,
			},
			ClaimContention: CurationClaimContentionOutcome{
				Requests: 6, Attempts: 24, Wins: 6, Losses: 18,
				WinRate: 0.25, LossRate: 0.75, SingleWinnerPerRequest: true,
			},
			InputPaging: CurationInputPagingOutcome{
				Runs: 3, Pages: 45, Inputs: 342, ExhaustedRuns: 3,
				DuplicateInputs: 0, PagedToExhaustion: true,
			},
			PlanLifecycle: CurationPlanLifecycleOutcome{
				Plans: 4, PlanGets: 4, Applies: 4, EmptyApplies: 2,
				CreateApplies: 2, CreateActions: 2, EmptyCursorAdvances: 2,
				FollowUpRequests: 3, MaxChainDepth: 4, DrainedChains: 1,
				EmptyPlanAdvancedCursors: true, BacklogDrained: true,
			},
			LeaseChurn: CurationLeaseChurnOutcome{
				Cycles: 3, LiveRenewals: 3, RenewAfterExpiry: 3,
				Reconciliations: 3, Requeues: 3, ApplyRaceAttempts: 4,
				ApplyRaceWins: 1, ApplyRaceRefusals: 3, StaleFenceRefusals: 3,
				DoubleApplySuccesses: 0, ExpiredRenewReconciled: true, NoDoubleApply: true,
			},
			StalePlanConflict: CurationStalePlanConflictOutcome{
				WrongPlanHashRefusals: 1, DuplicatePlanRefusals: 1,
				StaleFenceRefusals: 9, TypedRefusals: 11, AllRefusalsTyped: true,
			},
			AbandonRequeue: CurationAbandonRequeueOutcome{
				PreviewAbandons: 1, PreviewRequeues: 1,
				PreviewAttemptCountBefore: 0, PreviewAttemptCountAfter: 0,
				FailureAbandons: 2, ExpiryInterruptions: 1, RetryRequeues: 2,
				DeadLetters: 1, TerminalAttemptCount: 3, PostTerminalStartRefusals: 1,
				PreviewBudgetPreserved: true, DeadLetterTerminal: true,
			},
		},
	}
}

func TestParseCurationOptionsRejectsDottedLabels(t *testing.T) {
	base := map[string]string{}
	lookup := func(overrides map[string]string) func(string) string {
		return func(key string) string {
			if value, ok := overrides[key]; ok {
				return value
			}
			return base[key]
		}
	}
	if _, err := ParseCurationOptions(lookup(map[string]string{
		EnvCurationProvider: "pg17.db.internal.example.net",
	})); err == nil {
		t.Fatal("dotted provider label was accepted")
	}
	if _, err := ParseCurationOptions(lookup(map[string]string{
		EnvCurationHardwareTier: "tier.internal",
	})); err == nil {
		t.Fatal("dotted hardware tier label was accepted")
	}
	opts, err := ParseCurationOptions(lookup(map[string]string{
		EnvCurationRelease: "v0.0.270",
		EnvCurationCommit:  "abc123",
	}))
	if err != nil {
		t.Fatalf("dotted release rejected: %v", err)
	}
	if opts.Release != "v0.0.270" {
		t.Fatalf("release = %q", opts.Release)
	}
}

func TestParseCurationOptionsDefaultsPidScopedResultsPath(t *testing.T) {
	opts, err := ParseCurationOptions(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("/tmp/witself-memory-curation-load-%d.json", os.Getpid())
	if opts.ResultsPath != want {
		t.Fatalf("default results path = %q, want %q", opts.ResultsPath, want)
	}
}
