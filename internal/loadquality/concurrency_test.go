package loadquality

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseConcurrencyOptionsDefaultsAndSignedSeed(t *testing.T) {
	opts, err := ParseConcurrencyOptions(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if opts.Seed != DefaultConcurrencySeed || opts.Accounts != DefaultConcurrencyAccounts ||
		opts.RealmsPerAccount != DefaultConcurrencyRealmsPerAccount ||
		opts.AgentsPerRealm != DefaultConcurrencyAgentsPerRealm ||
		opts.SeedMemoriesPerAgent != DefaultConcurrencySeedMemoriesPerAgent ||
		opts.WorkersPerAgent != DefaultConcurrencyWorkersPerAgent ||
		opts.OperationsPerWorker != DefaultConcurrencyOperationsPerWorker ||
		opts.IsolationIterations != DefaultConcurrencyIsolationIterations ||
		opts.ClaimWorkers != DefaultConcurrencyClaimWorkers || opts.Release != "dev" ||
		opts.Commit != "none" || opts.Provider != "local" || opts.HardwareTier != "unspecified" {
		t.Fatalf("default concurrency options = %#v", opts)
	}

	values := map[string]string{
		EnvConcurrencySeed:                 "-9223372036854775808",
		EnvConcurrencyAccounts:             "8",
		EnvConcurrencyRealmsPerAccount:     "4",
		EnvConcurrencyAgentsPerRealm:       "8",
		EnvConcurrencySeedMemoriesPerAgent: "8",
		EnvConcurrencyWorkersPerAgent:      "8",
		EnvConcurrencyOperationsPerWorker:  "50",
		EnvConcurrencyIsolationIterations:  "50",
		EnvConcurrencyClaimWorkers:         "32",
	}
	opts, err = ParseConcurrencyOptions(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if opts.Seed != -9223372036854775808 || opts.Accounts != 8 || opts.RealmsPerAccount != 4 ||
		opts.AgentsPerRealm != 8 || opts.SeedMemoriesPerAgent != 8 || opts.WorkersPerAgent != 8 ||
		opts.OperationsPerWorker != 50 || opts.IsolationIterations != 50 || opts.ClaimWorkers != 32 {
		t.Fatalf("explicit concurrency options = %#v", opts)
	}
}

func TestParseConcurrencyOptionsRejectsUnboundedOrPartialShapes(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
	}{
		{"too few accounts", map[string]string{EnvConcurrencyAccounts: "1"}},
		{"too many accounts", map[string]string{EnvConcurrencyAccounts: "9"}},
		{"too few realms", map[string]string{EnvConcurrencyRealmsPerAccount: "1"}},
		{"too many realms", map[string]string{EnvConcurrencyRealmsPerAccount: "5"}},
		{"too few agents", map[string]string{EnvConcurrencyAgentsPerRealm: "1"}},
		{"too many agents", map[string]string{EnvConcurrencyAgentsPerRealm: "9"}},
		{"no seed memories", map[string]string{EnvConcurrencySeedMemoriesPerAgent: "0"}},
		{"too many seed memories", map[string]string{EnvConcurrencySeedMemoriesPerAgent: "65"}},
		{"one worker", map[string]string{EnvConcurrencyWorkersPerAgent: "1"}},
		{"too many workers", map[string]string{EnvConcurrencyWorkersPerAgent: "9", EnvConcurrencySeedMemoriesPerAgent: "9"}},
		{"workers exceed adjustment targets", map[string]string{EnvConcurrencyWorkersPerAgent: "5", EnvConcurrencySeedMemoriesPerAgent: "4"}},
		{"no mixed operations", map[string]string{EnvConcurrencyOperationsPerWorker: "0"}},
		{"too many mixed operations", map[string]string{EnvConcurrencyOperationsPerWorker: "51"}},
		{"no isolation iteration", map[string]string{EnvConcurrencyIsolationIterations: "0"}},
		{"too many isolation iterations", map[string]string{EnvConcurrencyIsolationIterations: "51"}},
		{"one claim worker", map[string]string{EnvConcurrencyClaimWorkers: "1"}},
		{"too many claim workers", map[string]string{EnvConcurrencyClaimWorkers: "33"}},
		{"unsafe release", map[string]string{EnvConcurrencyRelease: "postgres://user:password@host/db"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseConcurrencyOptions(func(name string) string { return test.overrides[name] })
			if err == nil {
				t.Fatal("invalid concurrency options unexpectedly accepted")
			}
		})
	}
}

func TestParseConcurrencyOptionsRejectsDottedLabels(t *testing.T) {
	if _, err := ParseConcurrencyOptions(func(name string) string {
		if name == EnvConcurrencyProvider {
			return "pg17.db.internal.example.net"
		}
		return ""
	}); err == nil {
		t.Fatal("dotted concurrency provider label was accepted")
	}
	if _, err := ParseConcurrencyOptions(func(name string) string {
		if name == EnvConcurrencyHardwareTier {
			return "tier.internal"
		}
		return ""
	}); err == nil {
		t.Fatal("dotted concurrency hardware tier label was accepted")
	}
	opts, err := ParseConcurrencyOptions(func(name string) string {
		switch name {
		case EnvConcurrencyRelease:
			return "v0.0.270"
		case EnvConcurrencyCommit:
			return "release.270+dirty"
		case EnvConcurrencyProvider:
			return "gcp-prod"
		case EnvConcurrencyHardwareTier:
			return "db-custom-2-7680"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("safe concurrency metadata rejected: %v", err)
	}
	if opts.Release != "v0.0.270" || opts.Commit != "release.270+dirty" ||
		opts.Provider != "gcp-prod" || opts.HardwareTier != "db-custom-2-7680" {
		t.Fatalf("safe concurrency metadata = %#v", opts)
	}
}

func TestParseConcurrencyOptionsDefaultsPidScopedResultsPath(t *testing.T) {
	opts, err := ParseConcurrencyOptions(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("/tmp/witself-memory-concurrency-load-%d.json", os.Getpid())
	if opts.ResultsPath != want {
		t.Fatalf("default results path = %q, want %q", opts.ResultsPath, want)
	}
}

func TestConcurrencyPrincipalCount(t *testing.T) {
	count, err := ConcurrencyPrincipalCount(4, 2, 4)
	if err != nil || count != 32 {
		t.Fatalf("principal count = %d, %v", count, err)
	}
	if _, err := ConcurrencyPrincipalCount(1, 2, 4); err == nil {
		t.Fatal("undersized topology was accepted")
	}
}

func TestConcurrencyPhaseDeadlineIsProportionalToAgentBatches(t *testing.T) {
	tests := []struct {
		principals int
		want       time.Duration
	}{
		{8, 2 * time.Minute},
		{9, 4 * time.Minute},
		{32, 8 * time.Minute},
		{256, 64 * time.Minute},
	}
	for _, test := range tests {
		got, err := ConcurrencyPhaseDeadline(test.principals)
		if err != nil || got != test.want {
			t.Fatalf("phase deadline for %d principals = %s, %v, want %s", test.principals, got, err, test.want)
		}
	}
	for _, principals := range []int{0, 7, 257} {
		if _, err := ConcurrencyPhaseDeadline(principals); err == nil {
			t.Fatalf("phase deadline accepted %d principals", principals)
		}
	}
}

func TestConcurrencyResultSchemaIsCheckedInResolvedAndStrict(t *testing.T) {
	first := ConcurrencyResultJSONSchema()
	second := ConcurrencyResultJSONSchema()
	if len(first) == 0 || len(second) == 0 {
		t.Fatal("embedded concurrency result schema is empty")
	}
	first[0] = 'x'
	if second[0] != '{' {
		t.Fatal("concurrency result schema did not return a fresh copy")
	}
	var schema map[string]any
	if err := json.Unmarshal(second, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" ||
		schema["$id"] != "https://witself.witwave.ai/schemas/memory-concurrency-load-result.v1.schema.json" {
		t.Fatalf("concurrency result schema identity = %#v", schema)
	}
	if _, err := resolvedConcurrencyResultSchema(); err != nil {
		t.Fatalf("resolve checked-in Draft 2020-12 schema: %v", err)
	}

	result := validConcurrencyTestResult(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	raw, err := MarshalConcurrencyResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConcurrencyResultJSON(raw); err != nil {
		t.Fatalf("validate valid concurrency result JSON: %v", err)
	}
	result.Workload.Seed = -9223372036854775808
	if _, err := MarshalConcurrencyResult(result); err != nil {
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
	if err := ValidateConcurrencyResultJSON(invalidRaw); err == nil {
		t.Fatal("schema accepted an unexpected top-level property")
	}
	delete(invalid, "unexpected")
	workload := invalid["workload"].(map[string]any)
	workload["account_id"] = "not-retainable"
	invalidRaw, err = json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConcurrencyResultJSON(invalidRaw); err == nil {
		t.Fatal("schema accepted a leaked identifier field")
	}
	delete(workload, "account_id")
	isolation := invalid["outcomes"].(map[string]any)["isolation"].(map[string]any)
	isolation["no_sensitive_content"] = false
	invalidRaw, err = json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConcurrencyResultJSON(invalidRaw); err == nil {
		t.Fatal("schema accepted failed sensitive-content evidence")
	}
}

func TestValidateConcurrencyResultRejectsPartialOrFailedRuns(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*ConcurrencyResult)
		wantError string
	}{
		{"topology seed measurement", func(result *ConcurrencyResult) { result.Measurements.Seed.Count-- }, "topology"},
		{"mixed recall measurement", func(result *ConcurrencyResult) { result.Measurements.MixedRecall.Count-- }, "mixed-operation"},
		{"mixed exact hit count", func(result *ConcurrencyResult) { result.Outcomes.MixedOperations.RecallHits-- }, "mixed-operation"},
		{"mixed owner assertion", func(result *ConcurrencyResult) { result.Outcomes.MixedOperations.AllHitsExactOwner = false }, "mixed-operation"},
		{"whole fleet release", func(result *ConcurrencyResult) { result.Outcomes.MixedOperations.WholeFleetStartSynchronized = false }, "mixed-operation"},
		{"isolation call measurement", func(result *ConcurrencyResult) { result.Measurements.IsolationProbe.Count-- }, "isolation-under-load"},
		{"broad expected rows", func(result *ConcurrencyResult) { result.Outcomes.Isolation.BroadHits-- }, "isolation-under-load"},
		{"own control rows", func(result *ConcurrencyResult) { result.Outcomes.Isolation.OwnControlHits-- }, "isolation-under-load"},
		{"cross realm assertion", func(result *ConcurrencyResult) { result.Outcomes.Isolation.CrossRealmIsolated = false }, "isolation-under-load"},
		{"curation claim measurement", func(result *ConcurrencyResult) { result.Measurements.CurationClaim.Count-- }, "curation-claim"},
		{"typed foreign refusals", func(result *ConcurrencyResult) { result.Outcomes.CurationClaims.TypedForeignRefusals-- }, "curation-claim"},
		{"cursor ownership", func(result *ConcurrencyResult) { result.Outcomes.CurationClaims.OnlyOwnerCursorAdvanced = false }, "curation-claim"},
		{"sensitive fanout count", func(result *ConcurrencyResult) { result.Measurements.SensitiveFanout.Count-- }, "sensitive fan-out"},
		{"owner exact read", func(result *ConcurrencyResult) { result.Outcomes.SensitiveFanout.OwnerExactReadSucceeded = false }, "sensitive fan-out"},
		{"principal formula", func(result *ConcurrencyResult) { result.Workload.SyntheticPrincipals-- }, "workload"},
		{"worker targets", func(result *ConcurrencyResult) { result.Workload.SeedMemoriesPerAgent = 1 }, "workload"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := validConcurrencyTestResult(time.Now().UTC())
			test.mutate(&result)
			err := ValidateConcurrencyResult(result)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("validation error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestValidateConcurrencyResultOverlapOperationSamples(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*ConcurrencyResult)
		wantError bool
	}{
		{
			name: "positive sample with multiple isolation iterations",
			mutate: func(result *ConcurrencyResult) {
				result.Outcomes.MixedOperations.OverlapOperationSamples = 1
			},
		},
		{
			name: "zero samples with multiple isolation iterations",
			mutate: func(result *ConcurrencyResult) {
				result.Outcomes.MixedOperations.OverlapOperationSamples = 0
			},
			wantError: true,
		},
		{
			name: "zero samples with one isolation iteration",
			mutate: func(result *ConcurrencyResult) {
				setConcurrencyTestIsolationIterations(result, 1)
				result.Outcomes.MixedOperations.OverlapOperationSamples = 0
			},
		},
		{
			name: "samples above probe-call maximum",
			mutate: func(result *ConcurrencyResult) {
				maximumSamples := ConcurrencyIsolationCallsPerProbeRound *
					result.Workload.SyntheticPrincipals * result.Workload.IsolationIterations
				result.Outcomes.MixedOperations.OverlapOperationSamples = maximumSamples + 1
			},
			wantError: true,
		},
		{
			name: "negative samples",
			mutate: func(result *ConcurrencyResult) {
				result.Outcomes.MixedOperations.OverlapOperationSamples = -1
			},
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := validConcurrencyTestResult(time.Now().UTC())
			test.mutate(&result)
			err := ValidateConcurrencyResult(result)
			if test.wantError {
				if err == nil {
					t.Fatal("invalid overlap-operation sample count unexpectedly accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("valid overlap-operation sample count rejected: %v", err)
			}
			if _, err := MarshalConcurrencyResult(result); err != nil {
				t.Fatalf("valid overlap-operation sample count failed schema validation: %v", err)
			}
		})
	}
}

func TestWriteConcurrencyResultIsPrivateAtomicAndSanitized(t *testing.T) {
	result := validConcurrencyTestResult(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "nested", "concurrency-result.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := WriteConcurrencyResult(path, result)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("concurrency result mode = %o, want 600", mode)
	}
	for _, forbidden := range []string{
		"postgres://", "password", "hostname", "database_host", "account_id",
		"realm_id", "agent_id", "memory_id", "request_id", "run_id",
		"\"query\":", "query_text", "\"content\":", "content_hash", "\"tags\":",
		"\"vector\":", "query_vector", "\"embedding\":",
		"canary fixture value", "sensitive fixture value", "secret fixture value",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("concurrency result contains forbidden value %q:\n%s", forbidden, raw)
		}
	}
	var decoded ConcurrencyResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != ConcurrencyResultSchemaV1 || !decoded.Outcomes.Isolation.NoForeignCanaries {
		t.Fatalf("decoded concurrency result = %#v", decoded)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("result directory contains temporary artifacts: %#v", entries)
	}
}

func validConcurrencyTestResult(started time.Time) ConcurrencyResult {
	stats := func(count int) OperationStats {
		return OperationStats{
			Count: count, WallDurationMS: float64(count), ThroughputPerSecond: 1000,
			MinimumMS: 0.1, P50MS: 0.2, P95MS: 0.4, P99MS: 0.5, MaximumMS: 0.5,
		}
	}
	workload := ConcurrencyWorkload{
		Seed: DefaultConcurrencySeed, SyntheticAccounts: 4, RealmsPerAccount: 2,
		AgentsPerRealm: 4, SyntheticRealms: 8, SyntheticPrincipals: 32,
		SeedMemoriesPerAgent: 4, WorkersPerAgent: 2, OperationsPerWorker: 2,
		IsolationIterations: 2, ClaimWorkers: 4,
	}
	return ConcurrencyResult{
		Schema: ConcurrencyResultSchemaV1, HarnessVersion: ConcurrencyHarnessVersion,
		StartedAt: started, CompletedAt: started.Add(time.Second), Outcome: "pass",
		PostgreSQLVersion: "18.0",
		Environment: SafeMetadata{
			Release: "v0.0.267", Commit: "release.267+dirty", Provider: "gcp",
			HardwareTier: "db-custom-2-7680", GoVersion: "go1.26.6",
			GOOS: "darwin", GOARCH: "arm64", LogicalCPUs: 8,
		},
		Workload: workload,
		Measurements: ConcurrencyMeasurements{
			Seed: stats(160), MixedCapture: stats(128), MixedRecall: stats(128),
			MixedAdjust: stats(128), IsolationProbe: stats(448),
			CurationRequest: stats(32), CurationClaim: stats(224),
			CurationApply: stats(32), SensitiveFanout: stats(32),
		},
		Outcomes: ConcurrencyOutcomes{
			Topology: ConcurrencyTopologyOutcome{
				Accounts: 4, Realms: 8, Principals: 32, CanaryMemories: 128,
				SensitiveMemories: 32, SeededMemories: 160,
				AllPrincipalsSeeded: true, AllCanariesUnique: true, AllSensitiveSeeded: true,
			},
			MixedOperations: ConcurrencyMixedOperationsOutcome{
				Workers: 64, OperationBatches: 128, CaptureCalls: 128, RecallCalls: 128,
				AdjustCalls: 128, RecallHits: 128, OwnerChecks: 128,
				OverlapOperationSamples: 1,
				ExactRecallValues:       true, ExactAdjustValues: true,
				AllHitsExactOwner: true, WholeFleetStartSynchronized: true,
				AllOperationsComplete: true,
			},
			Isolation: ConcurrencyIsolationOutcome{
				ProbeAgents: 32, ProbeRounds: 64, BroadRecallCalls: 64, BroadHits: 320,
				BroadVisibleCanaries: 256, BroadSensitiveRedactions: 64,
				OwnControlRecallCalls: 192, OwnControlHits: 192,
				CrossAccountRecallCalls: 64, CrossRealmRecallCalls: 64, CrossAgentRecallCalls: 64,
				MarkerScans: 512, BroadCountsExact: true, OwnCountsExact: true,
				AllHitsExactOwner: true, NoForeignCanaries: true, NoSensitiveContent: true,
				CrossAccountIsolated: true, CrossRealmIsolated: true, CrossAgentIsolated: true,
			},
			CurationClaims: ConcurrencyCurationClaimsOutcome{
				Requests: 32, RequestCalls: 32, OwnerClaimAttempts: 128,
				OwnerClaimWins: 32, OwnerClaimLosses: 96, ForeignClaimAttempts: 96,
				CrossAccountRefusals: 32, CrossRealmRefusals: 32, CrossAgentRefusals: 32,
				TypedForeignRefusals: 96, ApplyCalls: 32, OwnerCursorAdvances: 32,
				SingleWinnerPerRequest: true, AllForeignClaimsTyped: true,
				OnlyOwnerCursorAdvanced: true, AllRequestsApplied: true,
			},
			SensitiveFanout: ConcurrencySensitiveFanoutOutcome{
				QueryCalls: 32, OwnerQueryCalls: 1, ForeignQueryCalls: 31, OwnerHits: 1,
				OwnerExactReadSucceeded: true, AllForeignQueriesIsolated: true,
			},
		},
	}
}

func setConcurrencyTestIsolationIterations(result *ConcurrencyResult, iterations int) {
	probeRounds := result.Workload.SyntheticPrincipals * iterations
	result.Workload.IsolationIterations = iterations
	result.Measurements.IsolationProbe.Count = ConcurrencyIsolationCallsPerProbeRound * probeRounds
	result.Outcomes.Isolation.ProbeRounds = probeRounds
	result.Outcomes.Isolation.BroadRecallCalls = probeRounds
	result.Outcomes.Isolation.BroadHits = probeRounds * (result.Workload.SeedMemoriesPerAgent + 1)
	result.Outcomes.Isolation.BroadVisibleCanaries = probeRounds * result.Workload.SeedMemoriesPerAgent
	result.Outcomes.Isolation.BroadSensitiveRedactions = probeRounds
	result.Outcomes.Isolation.OwnControlRecallCalls = ConcurrencyIsolationDimensions * probeRounds
	result.Outcomes.Isolation.OwnControlHits = ConcurrencyIsolationDimensions * probeRounds
	result.Outcomes.Isolation.CrossAccountRecallCalls = probeRounds
	result.Outcomes.Isolation.CrossRealmRecallCalls = probeRounds
	result.Outcomes.Isolation.CrossAgentRecallCalls = probeRounds
	result.Outcomes.Isolation.MarkerScans = result.Outcomes.Isolation.BroadHits +
		result.Outcomes.Isolation.OwnControlHits
}
