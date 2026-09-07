package loadquality

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// This independent gold fixture deliberately gives two memories the same
// relevant role and includes unrelated results. Expected metrics below are
// hand-calculated, not produced by the result validator or scoring helper.
func relevanceScorerCorpus() RelevanceCorpus {
	return RelevanceCorpus{
		Schema: RelevanceCorpusSchemaV1,
		Memories: []RelevanceMemory{
			{Label: "a", Content: "Synthetic cedar inspection procedure."},
			{Label: "b", Content: "Synthetic cedar inspection checklist."},
			{Label: "c", Content: "Synthetic ferry timetable."},
			{Label: "d", Content: "Synthetic kiln cooling procedure."},
		},
		Queries: []RelevanceQuery{
			{Name: "inspection", Query: "cedar inspection", TopK: 3, RelevantLabels: []string{"a", "b"}, Rationale: "Both inspection documents answer the request; transport and cooling do not."},
			{Name: "no_answer", Query: "synthetic ocean temperature", TopK: 2, RelevantLabels: []string{}, Rationale: "No document states an ocean temperature."},
		},
	}
}

func TestScoreRelevanceMeasuresAllReturnedRanks(t *testing.T) {
	zero, half, one := 0.0, 0.5, 1.0
	tests := []struct {
		name   string
		query  string
		labels []string
		want   RelevanceScore
	}{
		{"relevant plus irrelevant", "inspection", []string{"a", "c"}, RelevanceScore{Name: "inspection", TopK: 3, RetrievedCount: 2, RelevantCount: 2, RelevantRanks: []int{1}, TruePositives: 1, FalsePositives: 1, PrecisionAtK: 1.0 / 3, RecallAtK: &half, ReciprocalRank: 1}},
		{"underfilled relevant only", "inspection", []string{"a"}, RelevanceScore{Name: "inspection", TopK: 3, RetrievedCount: 1, RelevantCount: 2, RelevantRanks: []int{1}, TruePositives: 1, FalsePositives: 0, PrecisionAtK: 1.0 / 3, RecallAtK: &half, ReciprocalRank: 1}},
		{"secondary relevant found", "inspection", []string{"a", "b"}, RelevanceScore{Name: "inspection", TopK: 3, RetrievedCount: 2, RelevantCount: 2, RelevantRanks: []int{1, 2}, TruePositives: 2, FalsePositives: 0, PrecisionAtK: 2.0 / 3, RecallAtK: &one, ReciprocalRank: 1}},
		{"first relevant shifted", "inspection", []string{"c", "a", "b"}, RelevanceScore{Name: "inspection", TopK: 3, RetrievedCount: 3, RelevantCount: 2, RelevantRanks: []int{2, 3}, TruePositives: 2, FalsePositives: 1, PrecisionAtK: 2.0 / 3, RecallAtK: &one, ReciprocalRank: 0.5}},
		{"no relevant returned", "inspection", []string{"c", "d"}, RelevanceScore{Name: "inspection", TopK: 3, RetrievedCount: 2, RelevantCount: 2, RelevantRanks: []int{}, TruePositives: 0, FalsePositives: 2, PrecisionAtK: 0, RecallAtK: &zero, ReciprocalRank: 0}},
		{"empty retrieval known answer", "inspection", nil, RelevanceScore{Name: "inspection", TopK: 3, RetrievedCount: 0, RelevantCount: 2, RelevantRanks: []int{}, TruePositives: 0, FalsePositives: 0, PrecisionAtK: 0, RecallAtK: &zero, ReciprocalRank: 0}},
		{"empty retrieval no answer", "no_answer", nil, RelevanceScore{Name: "no_answer", TopK: 2, RetrievedCount: 0, RelevantCount: 0, RelevantRanks: []int{}, TruePositives: 0, FalsePositives: 0, PrecisionAtK: 0, RecallAtK: nil, ReciprocalRank: 0}},
		{"false positive no answer", "no_answer", []string{"c"}, RelevanceScore{Name: "no_answer", TopK: 2, RetrievedCount: 1, RelevantCount: 0, RelevantRanks: []int{}, TruePositives: 0, FalsePositives: 1, PrecisionAtK: 0, RecallAtK: nil, ReciprocalRank: 0}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ScoreRelevance(relevanceScorerCorpus(), test.query, test.labels)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("score = %#v; want %#v", got, test.want)
			}
		})
	}
}

func TestScoreRelevanceRejectsAmbiguousOrExcessReturns(t *testing.T) {
	for _, test := range []struct {
		name, query string
		labels      []string
	}{
		{"unknown query", "missing", nil},
		{"unknown label", "inspection", []string{"a", "missing"}},
		{"duplicate relevant", "inspection", []string{"a", "a"}},
		{"duplicate irrelevant", "inspection", []string{"c", "c"}},
		{"above K", "inspection", []string{"a", "b", "c", "d"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ScoreRelevance(relevanceScorerCorpus(), test.query, test.labels); err == nil {
				t.Fatal("invalid returned labels accepted")
			}
		})
	}
}

func TestDefaultRelevanceCorpusIsFreshAndFingerprintIsExact(t *testing.T) {
	corpus, digest, err := DefaultRelevanceCorpus()
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(relevanceCorpusJSON)
	if digest != hex.EncodeToString(want[:]) {
		t.Fatalf("corpus fingerprint = %q", digest)
	}
	corpus.Memories[0].Content = "changed"
	corpus.Queries[0].Name = "changed"
	for i := range corpus.Queries {
		if len(corpus.Queries[i].RelevantLabels) > 0 {
			corpus.Queries[i].RelevantLabels[0] = "changed"
		}
	}
	again, secondDigest, err := DefaultRelevanceCorpus()
	if err != nil || secondDigest != digest || again.Memories[0].Content == "changed" || again.Queries[0].Name == "changed" {
		t.Fatalf("embedded corpus was mutated across reads: %v", err)
	}
	if err := ValidateRelevanceCorpus(again); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRelevanceCorpusRejectsMissingOrAmbiguousGold(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*RelevanceCorpus)
	}{
		{"schema", func(c *RelevanceCorpus) { c.Schema = "other" }},
		{"no memories", func(c *RelevanceCorpus) { c.Memories = nil }},
		{"too many memories", func(c *RelevanceCorpus) { c.Memories = make([]RelevanceMemory, MaximumRelevanceMemories+1) }},
		{"duplicate memory", func(c *RelevanceCorpus) { c.Memories[1].Label = c.Memories[0].Label }},
		{"duplicate content", func(c *RelevanceCorpus) { c.Memories[1].Content = c.Memories[0].Content }},
		{"blank content", func(c *RelevanceCorpus) { c.Memories[0].Content = "  " }},
		{"oversized content", func(c *RelevanceCorpus) { c.Memories[0].Content = strings.Repeat("x", 4097) }},
		{"unsafe label", func(c *RelevanceCorpus) { c.Memories[0].Label = "not a label" }},
		{"no queries", func(c *RelevanceCorpus) { c.Queries = nil }},
		{"too many queries", func(c *RelevanceCorpus) { c.Queries = make([]RelevanceQuery, MaximumRelevanceQueries+1) }},
		{"duplicate query", func(c *RelevanceCorpus) { c.Queries[1].Name = c.Queries[0].Name }},
		{"blank query", func(c *RelevanceCorpus) { c.Queries[0].Query = " " }},
		{"oversized query", func(c *RelevanceCorpus) { c.Queries[0].Query = strings.Repeat("x", 513) }},
		{"zero K", func(c *RelevanceCorpus) { c.Queries[0].TopK = 0 }},
		{"excess K", func(c *RelevanceCorpus) { c.Queries[0].TopK = MaximumRelevanceTopK + 1 }},
		{"missing gold", func(c *RelevanceCorpus) { c.Queries[0].RelevantLabels = nil }},
		{"unknown gold", func(c *RelevanceCorpus) { c.Queries[0].RelevantLabels = []string{"missing"} }},
		{"duplicate gold", func(c *RelevanceCorpus) { c.Queries[0].RelevantLabels = []string{"a", "a"} }},
		{"blank rationale", func(c *RelevanceCorpus) { c.Queries[0].Rationale = " " }},
		{"oversized rationale", func(c *RelevanceCorpus) { c.Queries[0].Rationale = strings.Repeat("x", 2049) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			corpus := relevanceScorerCorpus()
			test.mutate(&corpus)
			if err := ValidateRelevanceCorpus(corpus); err == nil {
				t.Fatal("invalid corpus accepted")
			}
		})
	}
}

func TestParseRelevanceOptionsHasOnlyEvidenceControls(t *testing.T) {
	var requested []string
	opts, err := ParseRelevanceOptions(func(name string) string {
		requested = append(requested, name)
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{EnvRelevanceResultsPath, EnvRelevanceRelease, EnvRelevanceCommit, EnvRelevanceProvider, EnvRelevanceHardwareTier}
	if !reflect.DeepEqual(requested, wantKeys) || opts.ResultsPath != fmt.Sprintf("/tmp/witself-memory-relevance-%d.json", os.Getpid()) ||
		opts.Release != "dev" || opts.Commit != "none" || opts.Provider != "local" || opts.HardwareTier != "unspecified" {
		t.Fatalf("unexpected options or controls: %#v; %#v", opts, requested)
	}
	values := map[string]string{EnvRelevanceResultsPath: " /tmp/custom-evidence.json ", EnvRelevanceRelease: "v0.0.277", EnvRelevanceCommit: "release.277+dirty", EnvRelevanceProvider: "local-test", EnvRelevanceHardwareTier: "small_2"}
	opts, err = ParseRelevanceOptions(func(name string) string { return values[name] })
	if err != nil || opts.ResultsPath != "/tmp/custom-evidence.json" || opts.Release != "v0.0.277" || opts.Commit != "release.277+dirty" {
		t.Fatalf("safe explicit evidence options: %#v, %v", opts, err)
	}
	if environment := RelevanceEnvironment(opts); !validRelevanceEnvironment(environment) {
		t.Fatalf("current pinned runner metadata is invalid: %#v", environment)
	}
	for _, key := range wantKeys[1:] {
		for _, bad := range []string{"postgres://private:secret@host/db", "private value", strings.Repeat("x", 129)} {
			_, err := ParseRelevanceOptions(func(name string) string {
				if name == key {
					return bad
				}
				return ""
			})
			if err == nil || strings.Contains(err.Error(), bad) {
				t.Fatalf("unsafe metadata accepted or echoed for %s", key)
			}
		}
	}
	for _, key := range []string{EnvRelevanceProvider, EnvRelevanceHardwareTier} {
		if _, err := ParseRelevanceOptions(func(name string) string {
			if name == key {
				return "db.internal.example"
			}
			return ""
		}); err == nil {
			t.Fatal("dotted environment label accepted")
		}
	}
}

func validRelevanceTestResult(t *testing.T) RelevanceResult {
	t.Helper()
	corpus, digest, err := DefaultRelevanceCorpus()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)
	result := RelevanceResult{
		Schema: RelevanceResultSchemaV1, HarnessVersion: RelevanceHarnessVersion,
		StartedAt: started, CompletedAt: started.Add(time.Second), Outcome: "measured",
		PostgreSQLVersion: "17.6",
		Environment:       SafeMetadata{Release: "dev", Commit: "none", Provider: "local", HardwareTier: "unspecified", GoVersion: "go1.26.6", GOOS: "linux", GOARCH: "amd64", LogicalCPUs: 2},
		FixtureClock:      RelevanceFixtureClock, RecallAsOf: RelevanceRecallAsOf(),
		CorpusSHA256: digest, CorpusMemories: len(corpus.Memories), QueryCount: len(corpus.Queries),
		Cases: make([]RelevanceScore, 0, len(corpus.Queries)),
	}
	for _, query := range corpus.Queries {
		labels := query.RelevantLabels
		if len(labels) > query.TopK {
			labels = labels[:query.TopK]
		}
		item, err := ScoreRelevance(corpus, query.Name, labels)
		if err != nil {
			t.Fatal(err)
		}
		result.Cases = append(result.Cases, item)
	}
	return result
}

func TestRelevanceResultSchemaAndSemantics(t *testing.T) {
	first, second := RelevanceResultJSONSchema(), RelevanceResultJSONSchema()
	if len(first) == 0 || first[0] != '{' {
		t.Fatal("missing checked-in result schema")
	}
	first[0] = 'x'
	if second[0] != '{' {
		t.Fatal("schema bytes alias embedded data")
	}
	var schema map[string]any
	if err := json.Unmarshal(second, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" || schema["$id"] != "https://witself.witwave.ai/schemas/memory-relevance-result.v1.schema.json" {
		t.Fatal("wrong relevance schema identity")
	}
	result := validRelevanceTestResult(t)
	raw, err := MarshalRelevanceResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRelevanceResultJSON(raw); err != nil {
		t.Fatal(err)
	}
	var decoded RelevanceResult
	if err := json.Unmarshal(raw, &decoded); err != nil || !reflect.DeepEqual(decoded, result) {
		t.Fatalf("result round trip changed values: %v", err)
	}
	// Raw JSON must invoke semantic validation, not just schema shape checks.
	result.Cases[0].PrecisionAtK = 0.123
	forged, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRelevanceResultJSON(forged); err == nil {
		t.Fatal("JSON validator accepted forged metric")
	}
	duplicate := bytes.Replace(raw, []byte(`"outcome": "measured"`), []byte(`"outcome": "measured", "outcome": "measured"`), 1)
	if bytes.Equal(duplicate, raw) {
		t.Fatal("duplicate-key canary was not installed")
	}
	if err := ValidateRelevanceResultJSON(duplicate); err == nil {
		t.Fatal("duplicate JSON property accepted")
	}
	if err := ValidateRelevanceResultJSON(append(append([]byte(nil), raw...), []byte(`{}`)...)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestRelevanceSchemaPinsFrozenCorpusAndCaseOrder(t *testing.T) {
	schema, err := resolvedRelevanceResultSchema()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalRelevanceResult(validRelevanceTestResult(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong digest", func(d map[string]any) { d["corpus_sha256"] = strings.Repeat("a", 64) }},
		{"wrong corpus count", func(d map[string]any) { d["corpus_memories"] = float64(23) }},
		{"wrong query count", func(d map[string]any) { d["query_count"] = float64(11) }},
		{"missing case", func(d map[string]any) { d["cases"] = d["cases"].([]any)[1:] }},
		{"swapped case", func(d map[string]any) { cases := d["cases"].([]any); cases[0], cases[1] = cases[1], cases[0] }},
		{"changed K", func(d map[string]any) { d["cases"].([]any)[0].(map[string]any)["top_k"] = float64(4) }},
		{"changed gold count", func(d map[string]any) { d["cases"].([]any)[0].(map[string]any)["relevant_count"] = float64(1) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var document map[string]any
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			test.mutate(document)
			if err := schema.Validate(document); err == nil {
				t.Fatal("checked-in schema accepted changed frozen contract")
			}
		})
	}
}

func TestValidateRelevanceResultRejectsPartialAndForgedEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*RelevanceResult)
	}{
		{"quality pass claim", func(r *RelevanceResult) { r.Outcome = "pass" }},
		{"missing start", func(r *RelevanceResult) { r.StartedAt = time.Time{} }},
		{"backward time", func(r *RelevanceResult) { r.CompletedAt = r.StartedAt.Add(-time.Second) }},
		{"wrong clock policy", func(r *RelevanceResult) { r.FixtureClock = "wall-clock" }},
		{"wrong recall time", func(r *RelevanceResult) { r.RecallAsOf = r.RecallAsOf.Add(time.Second) }},
		{"wrong digest", func(r *RelevanceResult) { r.CorpusSHA256 = strings.Repeat("a", 64) }},
		{"wrong corpus size", func(r *RelevanceResult) { r.CorpusMemories-- }},
		{"wrong query count", func(r *RelevanceResult) { r.QueryCount-- }},
		{"missing case", func(r *RelevanceResult) { r.Cases = r.Cases[1:] }},
		{"duplicate case", func(r *RelevanceResult) { r.Cases = append(r.Cases, r.Cases[0]) }},
		{"unknown case", func(r *RelevanceResult) { r.Cases[0].Name = "unknown" }},
		{"changed K", func(r *RelevanceResult) { r.Cases[0].TopK++ }},
		{"changed gold count", func(r *RelevanceResult) { r.Cases[0].RelevantCount++ }},
		{"negative returned count", func(r *RelevanceResult) { r.Cases[0].RetrievedCount = -1 }},
		{"excess returned count", func(r *RelevanceResult) { r.Cases[0].RetrievedCount = r.Cases[0].TopK + 1 }},
		{"missing rank list", func(r *RelevanceResult) { r.Cases[0].RelevantRanks = nil }},
		{"zero rank", func(r *RelevanceResult) { r.Cases[0].RelevantRanks = []int{0} }},
		{"duplicate ranks", func(r *RelevanceResult) { r.Cases[0].RelevantRanks = []int{1, 1} }},
		{"descending ranks", func(r *RelevanceResult) { r.Cases[0].RelevantRanks = []int{2, 1} }},
		{"rank outside returned", func(r *RelevanceResult) { r.Cases[0].RelevantRanks = []int{r.Cases[0].RetrievedCount + 1} }},
		{"forged TP", func(r *RelevanceResult) { r.Cases[0].TruePositives++ }},
		{"forged FP", func(r *RelevanceResult) { r.Cases[0].FalsePositives++ }},
		{"forged precision", func(r *RelevanceResult) { r.Cases[0].PrecisionAtK = 0.123 }},
		{"forged recall", func(r *RelevanceResult) { value := 0.123; r.Cases[0].RecallAtK = &value }},
		{"forged reciprocal rank", func(r *RelevanceResult) { r.Cases[0].ReciprocalRank = 0.123 }},
		{"NaN precision", func(r *RelevanceResult) { r.Cases[0].PrecisionAtK = math.NaN() }},
		{"infinite reciprocal rank", func(r *RelevanceResult) { r.Cases[0].ReciprocalRank = math.Inf(1) }},
		{"NaN recall", func(r *RelevanceResult) { value := math.NaN(); r.Cases[0].RecallAtK = &value }},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := validRelevanceTestResult(t)
			test.mutate(&result)
			if err := ValidateRelevanceResult(result); err == nil {
				t.Fatal("invalid relevance evidence accepted")
			}
		})
	}
	result := validRelevanceTestResult(t)
	if len(result.Cases) < 2 {
		t.Fatal("frozen corpus requires multiple independent cases")
	}
	result.Cases[0], result.Cases[1] = result.Cases[1], result.Cases[0]
	if err := ValidateRelevanceResult(result); err == nil {
		t.Fatal("reordered cases accepted")
	}
	for _, empty := range []bool{false, true} {
		result := validRelevanceTestResult(t)
		found := false
		for i := range result.Cases {
			if (result.Cases[i].RelevantCount == 0) != empty {
				continue
			}
			found = true
			if empty {
				value := 0.0
				result.Cases[i].RecallAtK = &value
			} else {
				result.Cases[i].RecallAtK = nil
			}
			break
		}
		if !found {
			t.Fatal("frozen corpus requires both answer and no-answer judgments")
		}
		if err := ValidateRelevanceResult(result); err == nil {
			t.Fatal("wrong nullable recall convention accepted")
		}
	}
}

func TestRelevanceResultRejectsValuesAndUnknownFieldsWithoutEcho(t *testing.T) {
	const secret = "postgres://private-person:credential@private-host/database"
	for _, mutate := range []func(*RelevanceResult){
		func(r *RelevanceResult) { r.PostgreSQLVersion = secret },
		func(r *RelevanceResult) { r.Environment.GoVersion = secret },
		func(r *RelevanceResult) { r.Environment.GOOS = secret },
		func(r *RelevanceResult) { r.Environment.GOARCH = secret },
		func(r *RelevanceResult) { r.Environment.Provider = "private-host.internal" },
		func(r *RelevanceResult) { r.Environment.HardwareTier = "private-host.internal" },
		func(r *RelevanceResult) { r.Cases[0].Name = secret },
	} {
		result := validRelevanceTestResult(t)
		mutate(&result)
		if _, err := MarshalRelevanceResult(result); err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("private supplied value accepted or echoed")
		}
	}
	raw, err := MarshalRelevanceResult(validRelevanceTestResult(t))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolvedRelevanceResultSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"query", "content", "memory_id", "account_id", "agent_id", "returned_labels", "rationale", "tags"} {
		for _, location := range []string{"root", "environment", "case"} {
			t.Run(field+"/"+location, func(t *testing.T) {
				var document map[string]any
				if err := json.Unmarshal(raw, &document); err != nil {
					t.Fatal(err)
				}
				target := document
				if location == "environment" {
					target = document["environment"].(map[string]any)
				}
				if location == "case" {
					target = document["cases"].([]any)[0].(map[string]any)
				}
				target[field] = secret
				if err := resolved.Validate(document); err == nil {
					t.Fatal("closed schema accepted an unknown private field")
				}
				injected, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				if err := ValidateRelevanceResultJSON(injected); err == nil || strings.Contains(err.Error(), secret) {
					t.Fatal("unknown private field accepted or echoed")
				}
			})
		}
	}
}

func TestWriteRelevanceResultIsPrivateAtomicAndValueFree(t *testing.T) {
	result := validRelevanceTestResult(t)
	path := filepath.Join(t.TempDir(), "nested", "result.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := WriteRelevanceResult(path, result)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private artifact mode: %v", err)
	}
	stored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(stored, raw) {
		t.Fatalf("artifact differs from validated bytes: %v", err)
	}
	corpus, _, err := DefaultRelevanceCorpus()
	if err != nil {
		t.Fatal(err)
	}
	for _, memory := range corpus.Memories {
		if bytes.Contains(raw, []byte(memory.Content)) {
			t.Fatal("source content leaked into result")
		}
	}
	for _, query := range corpus.Queries {
		for _, value := range []string{query.Query, query.Rationale} {
			if bytes.Contains(raw, []byte(value)) {
				t.Fatal("query or rationale leaked into result")
			}
		}
	}
	result.Cases = nil
	if _, err := WriteRelevanceResult(path, result); err == nil {
		t.Fatal("invalid result replaced retained evidence")
	}
	retained, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(retained, raw) {
		t.Fatalf("failed write damaged prior evidence: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != "result.json" {
		t.Fatalf("temporary evidence remains: %v, %#v", err, entries)
	}
	if _, err := WriteRelevanceResult(" ", validRelevanceTestResult(t)); err == nil {
		t.Fatal("empty output path accepted")
	}
	blocked := filepath.Join(t.TempDir(), "existing-directory")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteRelevanceResult(blocked, validRelevanceTestResult(t)); err == nil {
		t.Fatal("rename over directory unexpectedly succeeded")
	}
	entries, err = os.ReadDir(filepath.Dir(blocked))
	if err != nil || len(entries) != 1 || entries[0].Name() != "existing-directory" {
		t.Fatalf("failed publish leaked temporary file: %v, %#v", err, entries)
	}
}
