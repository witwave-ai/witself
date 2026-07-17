package loadquality

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultCorpusIsVersionedLabeledAndStable(t *testing.T) {
	corpus, digest, err := DefaultCorpus()
	if err != nil {
		t.Fatal(err)
	}
	if corpus.Schema != CorpusSchemaV1 || len(corpus.Memories) != 4 ||
		len(corpus.RelevanceQueries) != 2 || corpus.SensitiveProbe.ExpectedLabel != "ember_sensitive" {
		t.Fatalf("unexpected default corpus shape: %#v", corpus)
	}
	const expectedDigest = "a46728a4a09cca2dbcf0bc50c680e041e6567102cb443f6847bbb5ec15d855a7"
	if digest != expectedDigest {
		t.Fatalf("default corpus digest = %q, want %q", digest, expectedDigest)
	}
}

func TestResultJSONSchemaIsCheckedInAndVersioned(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(ResultJSONSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" ||
		schema["$id"] != "https://witself.witwave.ai/schemas/memory-load-quality-result.v1.schema.json" {
		t.Fatalf("result JSON schema identity = %#v", schema)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || len(properties) != 10 {
		t.Fatalf("result JSON schema properties = %#v", schema["properties"])
	}
}

func TestValidateCorpusRejectsAmbiguousOrUnsafeLabels(t *testing.T) {
	corpus, _, err := DefaultCorpus()
	if err != nil {
		t.Fatal(err)
	}

	duplicate := corpus
	duplicate.Memories = append([]CorpusMemory(nil), corpus.Memories...)
	duplicate.Memories[1].Label = duplicate.Memories[0].Label
	if err := ValidateCorpus(duplicate); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate label error = %v", err)
	}

	unsafe := corpus
	unsafe.RelevanceQueries = append([]QueryCase(nil), corpus.RelevanceQueries...)
	unsafe.RelevanceQueries[0].Name = "contains space"
	if err := ValidateCorpus(unsafe); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("unsafe query error = %v", err)
	}

	notSensitive := corpus
	notSensitive.SensitiveProbe.ExpectedLabel = "quasar_authority"
	if err := ValidateCorpus(notSensitive); err == nil || !strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("non-sensitive probe error = %v", err)
	}

	sensitiveRelevance := corpus
	sensitiveRelevance.RelevanceQueries = append([]QueryCase(nil), corpus.RelevanceQueries...)
	sensitiveRelevance.RelevanceQueries[0].ExpectedLabel = "ember_sensitive"
	if err := ValidateCorpus(sensitiveRelevance); err == nil || !strings.Contains(err.Error(), "non-sensitive") {
		t.Fatalf("sensitive relevance error = %v", err)
	}
}

func TestGenerateNoiseIsDeterministicBoundedAndDoesNotUseCorpusTerms(t *testing.T) {
	first, err := GenerateNoise(42, 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateNoise(42, 3)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("deterministic noise differs:\n%s\n%s", firstJSON, secondJSON)
	}
	if len(first) != 3 || first[0].Label != "noise_00000" || first[2].Label != "noise_00002" {
		t.Fatalf("noise shape = %#v", first)
	}
	for _, memory := range first {
		if memory.Salience < 0.05 || memory.Salience >= 0.30 || memory.Sensitive {
			t.Fatalf("noise bounds = %#v", memory)
		}
		for _, corpusTerm := range []string{"quasar", "cedar", "ember", "postgresql"} {
			if strings.Contains(strings.ToLower(memory.Content), corpusTerm) {
				t.Fatalf("noise contains corpus term %q: %q", corpusTerm, memory.Content)
			}
		}
	}
	if _, err := GenerateNoise(1, MaximumNoiseMemories+1); err == nil {
		t.Fatal("oversized noise generation unexpectedly accepted")
	}
}

func TestParseOptionsDefaultsBoundsAndRejectsUnsafeMetadata(t *testing.T) {
	values := map[string]string{EnvResultsPath: filepath.Join(t.TempDir(), "result.json")}
	getenv := func(name string) string { return values[name] }
	opts, err := ParseOptions(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Seed != DefaultSeed || opts.NoiseMemories != DefaultNoiseMemories ||
		opts.QueryIterations != DefaultQueryIterations || opts.Concurrency != DefaultConcurrency ||
		opts.Release != "dev" || opts.Provider != "local" {
		t.Fatalf("default options = %#v", opts)
	}

	values[EnvNoiseMemories] = "10001"
	if _, err := ParseOptions(getenv); err == nil || !strings.Contains(err.Error(), EnvNoiseMemories) {
		t.Fatalf("oversized noise error = %v", err)
	}
	values[EnvNoiseMemories] = "12"
	values[EnvRelease] = "postgres://user:password@host/database"
	if _, err := ParseOptions(getenv); err == nil || !strings.Contains(err.Error(), "unsafe evidence metadata") {
		t.Fatalf("unsafe metadata error = %v", err)
	}
}

func TestSummarizeUsesWallTimeAndNearestRankPercentiles(t *testing.T) {
	stats, err := Summarize([]time.Duration{
		5 * time.Millisecond,
		1 * time.Millisecond,
		3 * time.Millisecond,
		2 * time.Millisecond,
		4 * time.Millisecond,
	}, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Count != 5 || stats.ThroughputPerSecond != 500 || stats.MinimumMS != 1 ||
		stats.P50MS != 3 || stats.P95MS != 5 || stats.P99MS != 5 || stats.MaximumMS != 5 {
		t.Fatalf("summary = %#v", stats)
	}
	if _, err := Summarize(nil, time.Second); err == nil {
		t.Fatal("empty measurement unexpectedly accepted")
	}
}

func TestWriteResultIsPrivateAndContainsOnlySanitizedEvidence(t *testing.T) {
	started := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	result := validTestResult(started)
	path := filepath.Join(t.TempDir(), "nested", "result.json")
	raw, err := WriteResult(path, result)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("result mode = %o, want 600", got)
	}
	for _, forbidden := range []string{
		"postgres://", "password", "account_id", "agent_id", "memory_id",
		"Project Quasar", "opal 739", "query\"",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("result contains forbidden value %q:\n%s", forbidden, raw)
		}
	}
	var decoded Result
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != ResultSchemaV1 || decoded.Quality.RelevanceCases[0].Name != "quasar_postgresql_authority" {
		t.Fatalf("decoded result = %#v", decoded)
	}
}

func TestValidateResultRejectsFailedQualityAsPassing(t *testing.T) {
	result := validTestResult(time.Now().UTC())
	result.Quality.CrossTenantIsolated = false
	if err := ValidateResult(result); err == nil || !strings.Contains(err.Error(), "failed quality checks") {
		t.Fatalf("failed quality result error = %v", err)
	}
}

func validTestResult(started time.Time) Result {
	relevance := []RelevanceCaseResult{
		{Name: "quasar_postgresql_authority", Passed: true, ObservedRank: 1, MaximumRank: 1},
		{Name: "cedar_lexical_rollback", Passed: true, ObservedRank: 1, MaximumRank: 1},
	}
	return Result{
		Schema: ResultSchemaV1, HarnessVersion: HarnessVersion,
		StartedAt: started, CompletedAt: started.Add(time.Second), Outcome: "pass",
		PostgreSQLVersion: "18.0",
		Environment: SafeMetadata{
			Release: "v0.0.172", Commit: "67ec81d", Provider: "gcp",
			HardwareTier: "db-custom-2-7680", GoVersion: "go1.26.5",
			GOOS: "darwin", GOARCH: "arm64", LogicalCPUs: 8,
		},
		Workload: Workload{
			Seed: DefaultSeed, CorpusSHA256: strings.Repeat("a", 64),
			SyntheticAccounts: 2, SyntheticAgents: 3, CorpusMemories: 4,
			NoiseMemories: 6, QueryIterations: 5, Concurrency: 2,
		},
		Measurements: Measurements{
			Capture: OperationStats{Count: 10, WallDurationMS: 10, ThroughputPerSecond: 1000},
			Recall:  OperationStats{Count: 10, WallDurationMS: 10, ThroughputPerSecond: 1000},
		},
		Quality: Quality{
			RelevanceCases: relevance, RelevancePassRate: 1,
			SensitiveBroadRedacted: true, SensitiveExactOwnerVisible: true,
			CrossAgentIsolated: true, CrossTenantIsolated: true,
		},
	}
}
