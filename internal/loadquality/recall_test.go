package loadquality

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseRecallOptionsDefaultsAndSignedSeed(t *testing.T) {
	values := map[string]string{EnvRecallResultsPath: filepath.Join(t.TempDir(), "result.json")}
	getenv := func(name string) string { return values[name] }
	opts, err := ParseRecallOptions(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Seed != DefaultRecallSeed || !reflect.DeepEqual(opts.Cardinalities, []int{100, 500, 2000}) ||
		opts.QueryIterations != DefaultRecallQueryIterations || opts.Concurrency != DefaultRecallConcurrency ||
		opts.VectorDimensions != DefaultRecallVectorDimensions ||
		!reflect.DeepEqual(opts.CoveragePercentages, []int{100, 50}) ||
		opts.PaginationLimit != DefaultRecallPaginationLimit || opts.ResultBudget != DefaultRecallResultBudget ||
		opts.Release != "dev" || opts.Provider != "local" {
		t.Fatalf("default recall options = %#v", opts)
	}

	values[EnvRecallSeed] = "-9223372036854775808"
	values[EnvRecallCardinalities] = " 20, 300, 10000 "
	values[EnvRecallCoveragePercentages] = "100,75,5"
	opts, err = ParseRecallOptions(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Seed != -9223372036854775808 || !reflect.DeepEqual(opts.Cardinalities, []int{20, 300, 10000}) ||
		!reflect.DeepEqual(opts.CoveragePercentages, []int{100, 75, 5}) {
		t.Fatalf("explicit recall options = %#v", opts)
	}

	// Parsed defaults are fresh and cannot be mutated across calls.
	opts.Cardinalities[0] = 99
	opts.CoveragePercentages[0] = 99
	delete(values, EnvRecallCardinalities)
	delete(values, EnvRecallCoveragePercentages)
	again, err := ParseRecallOptions(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Cardinalities, []int{100, 500, 2000}) ||
		!reflect.DeepEqual(again.CoveragePercentages, []int{100, 50}) {
		t.Fatalf("reparsed recall slices = %#v, %#v", again.Cardinalities, again.CoveragePercentages)
	}
}

func TestParseRecallOptionsRejectsUnboundedOrPartialShapes(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
	}{
		{"concurrency below concurrent minimum", map[string]string{EnvRecallConcurrency: "1"}},
		{"iterations below workers", map[string]string{EnvRecallQueryIterations: "3", EnvRecallConcurrency: "4"}},
		{"iterations above maximum", map[string]string{EnvRecallQueryIterations: "1001"}},
		{"duplicate cardinality", map[string]string{EnvRecallCardinalities: "100,100,500"}},
		{"descending cardinality", map[string]string{EnvRecallCardinalities: "100,500,300"}},
		{"too few cardinalities", map[string]string{EnvRecallCardinalities: "500"}},
		{"coverage tenant above candidate budget", map[string]string{EnvRecallCardinalities: "257,500"}},
		{"pagination tenant not above candidate budget", map[string]string{EnvRecallCardinalities: "100,256"}},
		{"cardinality above maximum", map[string]string{EnvRecallCardinalities: "100,10001"}},
		{"zero-sized partial coverage", map[string]string{EnvRecallCardinalities: "10,500", EnvRecallCoveragePercentages: "100,1"}},
		{"coverage does not start full", map[string]string{EnvRecallCoveragePercentages: "90,50"}},
		{"coverage not decreasing", map[string]string{EnvRecallCoveragePercentages: "100,50,75"}},
		{"duplicate coverage", map[string]string{EnvRecallCoveragePercentages: "100,50,50"}},
		{"one coverage case", map[string]string{EnvRecallCoveragePercentages: "100"}},
		{"vector dimension below minimum", map[string]string{EnvRecallVectorDimensions: "1"}},
		{"vector dimension above maximum", map[string]string{EnvRecallVectorDimensions: "4097"}},
		{"page limit above store maximum", map[string]string{EnvRecallPaginationLimit: "101"}},
		{"budget does not force pages", map[string]string{EnvRecallPaginationLimit: "64", EnvRecallResultBudget: "64"}},
		{"budget above maximum", map[string]string{EnvRecallResultBudget: "257"}},
		{"unsafe release", map[string]string{EnvRecallRelease: "postgres://user:password@host/db"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseRecallOptions(func(name string) string { return test.overrides[name] })
			if err == nil {
				t.Fatal("invalid recall options unexpectedly accepted")
			}
		})
	}
}

func TestParseRecallOptionsRejectsDottedLabels(t *testing.T) {
	if _, err := ParseRecallOptions(func(name string) string {
		if name == EnvRecallProvider {
			return "pg17.db.internal.example.net"
		}
		return ""
	}); err == nil {
		t.Fatal("dotted recall provider label was accepted")
	}
	if _, err := ParseRecallOptions(func(name string) string {
		if name == EnvRecallHardwareTier {
			return "tier.internal"
		}
		return ""
	}); err == nil {
		t.Fatal("dotted recall hardware tier label was accepted")
	}
	opts, err := ParseRecallOptions(func(name string) string {
		switch name {
		case EnvRecallRelease:
			return "v0.0.270"
		case EnvRecallCommit:
			return "release.270+dirty"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("dotted safe metadata rejected: %v", err)
	}
	if opts.Release != "v0.0.270" || opts.Commit != "release.270+dirty" {
		t.Fatalf("safe metadata = %q, %q", opts.Release, opts.Commit)
	}
}

func TestParseRecallOptionsDefaultsPidScopedResultsPath(t *testing.T) {
	opts, err := ParseRecallOptions(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("/tmp/witself-memory-recall-load-%d.json", os.Getpid())
	if opts.ResultsPath != want {
		t.Fatalf("default results path = %q, want %q", opts.ResultsPath, want)
	}
}

func TestDeterministicVectorIsSignedStableAndUnitNormalized(t *testing.T) {
	first, err := DeterministicVector(-9223372036854775808, 17, 33)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DeterministicVector(-9223372036854775808, 17, 33)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, again) {
		t.Fatal("identical signed seed and index produced different vectors")
	}
	wantPrefix := []float64{
		-0.22960559348209786,
		0.051057189311995731,
		-0.23612545283584879,
		0.014082821425544075,
	}
	if !reflect.DeepEqual(first[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("deterministic vector prefix = %#v, want %#v", first[:len(wantPrefix)], wantPrefix)
	}
	otherSeed, err := DeterministicVector(9223372036854775807, 17, 33)
	if err != nil {
		t.Fatal(err)
	}
	otherIndex, err := DeterministicVector(-9223372036854775808, 18, 33)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(first, otherSeed) || reflect.DeepEqual(first, otherIndex) {
		t.Fatal("different signed seed or index produced an identical vector")
	}
	normSquared := 0.0
	for _, component := range first {
		if math.IsNaN(component) || math.IsInf(component, 0) {
			t.Fatalf("non-finite vector component %v", component)
		}
		normSquared += component * component
	}
	if difference := math.Abs(math.Sqrt(normSquared) - 1); difference > 1e-12 {
		t.Fatalf("vector norm difference = %g", difference)
	}
	if _, err := DeterministicVector(1, -1, 32); err == nil {
		t.Fatal("negative vector index was accepted")
	}
	if _, err := DeterministicVector(1, 0, 1); err == nil {
		t.Fatal("undersized vector was accepted")
	}
	if _, err := DeterministicVector(1, 0, MaximumRecallVectorDimensions+1); err == nil {
		t.Fatal("oversized vector was accepted")
	}
}

func TestRecallCoverageCountAndRatio(t *testing.T) {
	count, err := RecallCoverageCount(101, 50)
	if err != nil || count != 50 {
		t.Fatalf("coverage count = %d, %v", count, err)
	}
	if ratio := RecallRatio(1, 3); ratio != float64(1)/3 {
		t.Fatalf("recall ratio = %v", ratio)
	}
	if ratio := RecallRatio(4, 3); ratio != 0 {
		t.Fatalf("invalid recall ratio = %v", ratio)
	}
}

func TestRecallResultSchemaIsCheckedInResolvedAndValidatesInstances(t *testing.T) {
	first := RecallResultJSONSchema()
	second := RecallResultJSONSchema()
	if len(first) == 0 || len(second) == 0 {
		t.Fatal("embedded recall result schema is empty")
	}
	first[0] = 'x'
	if second[0] != '{' {
		t.Fatal("recall result schema did not return a fresh copy")
	}
	var schema map[string]any
	if err := json.Unmarshal(second, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" ||
		schema["$id"] != "https://witself.witwave.ai/schemas/memory-recall-load-result.v1.schema.json" {
		t.Fatalf("recall result schema identity = %#v", schema)
	}
	if _, err := resolvedRecallResultSchema(); err != nil {
		t.Fatalf("resolve checked-in Draft 2020-12 schema: %v", err)
	}

	result := validRecallTestResult(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	raw, err := MarshalRecallResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecallResultJSON(raw); err != nil {
		t.Fatalf("validate valid recall result JSON: %v", err)
	}
	result.Workload.Seed = -9223372036854775808
	if _, err := MarshalRecallResult(result); err != nil {
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
	if err := ValidateRecallResultJSON(invalidRaw); err == nil {
		t.Fatal("schema accepted an unexpected top-level property")
	}
	delete(invalid, "unexpected")
	pagination := invalid["outcomes"].(map[string]any)["pagination"].(map[string]any)
	pagination["ordering_stable"] = false
	invalidRaw, err = json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecallResultJSON(invalidRaw); err == nil {
		t.Fatal("schema accepted failed ordering evidence")
	}
	pagination["ordering_stable"] = true
	pagination["candidate_limit"] = float64(300)
	invalidRaw, err = json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecallResultJSON(invalidRaw); err == nil {
		t.Fatal("schema accepted a relaxed candidate budget")
	}
	pagination["candidate_limit"] = float64(RecallCandidateLimit)
	coverageCases := invalid["outcomes"].(map[string]any)["vector_coverage"].(map[string]any)["cases"].([]any)
	coverageCases[0].(map[string]any)["candidate_limit"] = float64(300)
	invalidRaw, err = json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecallResultJSON(invalidRaw); err == nil {
		t.Fatal("schema accepted a relaxed coverage candidate budget")
	}
	coverageCases[0].(map[string]any)["candidate_limit"] = float64(RecallCandidateLimit)
	qualityCases := invalid["outcomes"].(map[string]any)["hybrid_quality"].(map[string]any)["cases"].([]any)
	qualityCases[0].(map[string]any)["maximum_rank"] = float64(2)
	invalidRaw, err = json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecallResultJSON(invalidRaw); err == nil {
		t.Fatal("schema accepted a relaxed relevance threshold")
	}
}

func TestValidateRecallResultRejectsPartialOrFailedRuns(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*RecallResult)
		wantError string
	}{
		{"ladder count", func(result *RecallResult) { result.Measurements.CardinalityLadder[1].LexicalRecall.Count-- }, "cardinality ladder measurements"},
		{"vector attach count", func(result *RecallResult) { result.Measurements.VectorCoverage[1].VectorAttach.Count-- }, "vector coverage measurements"},
		{"coverage match formula", func(result *RecallResult) { result.Outcomes.VectorCoverage.Cases[1].VectorMatches-- }, "vector coverage outcomes"},
		{"full degraded", func(result *RecallResult) { result.Outcomes.VectorCoverage.Cases[0].Degraded = true }, "full vector coverage"},
		{"quality calls", func(result *RecallResult) { result.Measurements.HybridQuality.Count-- }, "workload measurements"},
		{"quality signal", func(result *RecallResult) { result.Outcomes.HybridQuality.Cases[0].LexicalUsed = true }, "vector-only relevance"},
		{"safety assertion", func(result *RecallResult) { result.Outcomes.VectorSafety.CrossAccountIsolated = false }, "vector safety"},
		{"pagination calls", func(result *RecallResult) { result.Measurements.Pagination.Count-- }, "pagination"},
		{"pagination fraction", func(result *RecallResult) { result.Outcomes.Pagination.TenantVectorFraction = 0.5 }, "pagination"},
		{"coverage candidate budget", func(result *RecallResult) { result.Outcomes.VectorCoverage.Cases[0].CandidateLimit = 300 }, "vector coverage"},
		{"candidate budget", func(result *RecallResult) { result.Outcomes.Pagination.CandidateLimit = 300 }, "pagination"},
		{"quality threshold", func(result *RecallResult) { result.Outcomes.HybridQuality.Cases[0].MaximumRank = 2 }, "relevance case"},
		{"fixture agents", func(result *RecallResult) { result.Workload.SyntheticAgents-- }, "workload"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := validRecallTestResult(time.Now().UTC())
			test.mutate(&result)
			err := ValidateRecallResult(result)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("validation error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestWriteRecallResultIsPrivateAtomicAndSanitized(t *testing.T) {
	result := validRecallTestResult(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "nested", "recall-result.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := WriteRecallResult(path, result)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("recall result mode = %o, want 600", mode)
	}
	for _, forbidden := range []string{
		"postgres://", "password", "hostname", "account_id", "agent_id",
		"memory_id", "profile_id", "\"query\":", "content_hash", "\"tags\":",
		"query vector fixture", "secret fixture", "[0.1,0.2]",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("recall result contains forbidden value %q:\n%s", forbidden, raw)
		}
	}
	var decoded RecallResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != RecallResultSchemaV1 || !decoded.Outcomes.Pagination.OrderingStable {
		t.Fatalf("decoded recall result = %#v", decoded)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("result directory contains temporary artifacts: %#v", entries)
	}
}

func validRecallTestResult(started time.Time) RecallResult {
	stats := func(count int) OperationStats {
		return OperationStats{
			Count: count, WallDurationMS: float64(count), ThroughputPerSecond: 1000,
			MinimumMS: 0.1, P50MS: 0.2, P95MS: 0.4, P99MS: 0.5, MaximumMS: 0.5,
		}
	}
	workload := RecallWorkload{
		Seed: DefaultRecallSeed, SyntheticAccounts: 3, SyntheticAgents: 4,
		Cardinalities: []int{100, 500, 2000}, QueryIterations: 10, Concurrency: 4,
		VectorDimensions: 32, CoveragePercentages: []int{100, 50},
		PaginationLimit: 64, ResultBudget: 256,
	}
	return RecallResult{
		Schema: RecallResultSchemaV1, HarnessVersion: RecallHarnessVersion,
		StartedAt: started, CompletedAt: started.Add(time.Second), Outcome: "pass",
		PostgreSQLVersion: "18.0",
		Environment: SafeMetadata{
			Release: "v0.0.267", Commit: "release.267+dirty", Provider: "gcp",
			HardwareTier: "db-custom-2-7680", GoVersion: "go1.26.6",
			GOOS: "darwin", GOARCH: "arm64", LogicalCPUs: 8,
		},
		Workload: workload,
		Measurements: RecallMeasurements{
			CardinalityLadder: []RecallCardinalityMeasurement{
				{MemoryCount: 100, LexicalRecall: stats(10)},
				{MemoryCount: 500, LexicalRecall: stats(10)},
				{MemoryCount: 2000, LexicalRecall: stats(10)},
			},
			VectorCoverage: []RecallVectorCoverageMeasurement{
				{CoveragePercent: 100, VectorAttach: stats(100), HybridRecall: stats(10)},
				{CoveragePercent: 50, VectorAttach: stats(50), HybridRecall: stats(10)},
			},
			HybridQuality: stats(30), VectorSafety: stats(4), Pagination: stats(8),
		},
		Outcomes: RecallOutcomes{
			CardinalityLadder: RecallCardinalityLadderOutcome{
				Tenants: 3, SeededMemories: 2600, RecallCalls: 30,
				AllLexical: true, AllComplete: true,
			},
			VectorCoverage: RecallVectorCoverageOutcome{
				Cases: []RecallVectorCoverageCase{
					{
						CoveragePercent: 100, EligibleMemories: 100, AttachedVectors: 100,
						RecallCalls: 10, VectorCandidates: 100, VectorMatches: 100,
						ReportedVectorCoverage: 1, CandidateLimit: 256,
						HybridUsed: true, MetadataStable: true,
					},
					{
						CoveragePercent: 50, EligibleMemories: 100, AttachedVectors: 50,
						RecallCalls: 10, VectorCandidates: 100, VectorMatches: 50,
						ReportedVectorCoverage: 0.5, Degraded: true, CandidateLimit: 256,
						HybridUsed: true, MetadataStable: true,
					},
				},
				AllProfilesListed: true,
			},
			HybridQuality: RecallHybridQualityOutcome{
				Cases: []RecallHybridRelevanceCase{
					{Name: RecallHybridCaseVectorOnly, Passed: true, ObservedRank: 1, MaximumRank: 1, VectorUsed: true, SimilarityUsed: true},
					{Name: RecallHybridCaseLexicalOnly, Passed: true, ObservedRank: 1, MaximumRank: 1, LexicalUsed: true},
					{Name: RecallHybridCaseBothSignals, Passed: true, ObservedRank: 1, MaximumRank: 1, VectorUsed: true, LexicalUsed: true, SimilarityUsed: true},
				},
				RecallCalls: 30, ScoreComponentsVerified: true, AllRanksPassed: true,
			},
			VectorSafety: RecallVectorSafetyOutcome{
				RecallCalls: 4, SensitiveBroadRedacted: true, SensitiveExactOwnerVisible: true,
				CrossAgentIsolated: true, CrossAccountIsolated: true, AllVectorQueries: true,
			},
			Pagination: RecallPaginationOutcome{
				RepeatRuns: 2, PagesPerRun: []int{4, 4}, HitsPerRun: []int{256, 256}, RecallCalls: 8,
				ResultBudget: 256, AttachedVectors: 256, VectorCandidates: 256,
				VectorMatches: 256, ReportedVectorCoverage: 1,
				TenantVectorFraction: 0.128, CandidateLimit: 256, CandidateTruncated: true,
				PageLimitsHonored: true, ResultBudgetReached: true,
				NoDuplicateIDs: true, OrderingStable: true,
			},
		},
	}
}
