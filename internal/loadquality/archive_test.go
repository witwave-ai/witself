package loadquality

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestParseArchiveOptionsDefaultsAndSignedSeed(t *testing.T) {
	values := map[string]string{EnvArchiveResultsPath: filepath.Join(t.TempDir(), "result.json")}
	getenv := func(name string) string { return values[name] }
	opts, err := ParseArchiveOptions(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Seed != DefaultArchiveSeed || !reflect.DeepEqual(opts.Cardinalities, []int{100, 500, 2000}) ||
		opts.VersionsPerMemory != 2 || opts.EvidencePerMemory != 2 ||
		opts.RelationsPerMemory != 1 || opts.TagsPerVersion != 3 ||
		opts.TranscriptSharePercent != 25 || opts.TranscriptEntriesPerSelectedMemory != 2 ||
		opts.VectorDimensions != 32 || opts.Release != "dev" || opts.Provider != "local" {
		t.Fatalf("default archive options = %#v", opts)
	}

	values[EnvArchiveSeed] = "-9223372036854775808"
	values[EnvArchiveCardinalities] = " 20, 300, 10000 "
	values[EnvArchiveVersionsPerMemory] = "8"
	values[EnvArchiveTranscriptSharePercent] = "5"
	opts, err = ParseArchiveOptions(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Seed != -9223372036854775808 ||
		!reflect.DeepEqual(opts.Cardinalities, []int{20, 300, 10000}) ||
		opts.VersionsPerMemory != 8 || opts.TranscriptSharePercent != 5 {
		t.Fatalf("explicit archive options = %#v", opts)
	}

	// Parsed defaults are fresh and cannot be mutated across calls.
	opts.Cardinalities[0] = 99
	delete(values, EnvArchiveCardinalities)
	delete(values, EnvArchiveVersionsPerMemory)
	delete(values, EnvArchiveTranscriptSharePercent)
	again, err := ParseArchiveOptions(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Cardinalities, []int{100, 500, 2000}) {
		t.Fatalf("reparsed archive cardinalities = %#v", again.Cardinalities)
	}
}

func TestParseArchiveOptionsRejectsUnboundedOrPartialShapes(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
	}{
		{"duplicate cardinality", map[string]string{EnvArchiveCardinalities: "100,100,500"}},
		{"descending cardinality", map[string]string{EnvArchiveCardinalities: "100,500,300"}},
		{"too few cardinalities", map[string]string{EnvArchiveCardinalities: "500"}},
		{"too many cardinalities", map[string]string{EnvArchiveCardinalities: "10,20,30,40,50,60"}},
		{"cardinality below minimum", map[string]string{EnvArchiveCardinalities: "9,500"}},
		{"cardinality above maximum", map[string]string{EnvArchiveCardinalities: "100,10001"}},
		{"versions below minimum", map[string]string{EnvArchiveVersionsPerMemory: "1"}},
		{"versions above maximum", map[string]string{EnvArchiveVersionsPerMemory: "9"}},
		{"evidence below minimum", map[string]string{EnvArchiveEvidencePerMemory: "0"}},
		{"evidence above maximum", map[string]string{EnvArchiveEvidencePerMemory: "9"}},
		{"relations below minimum", map[string]string{EnvArchiveRelationsPerMemory: "0"}},
		{"relations above maximum", map[string]string{EnvArchiveRelationsPerMemory: "5"}},
		{"tags below minimum", map[string]string{EnvArchiveTagsPerVersion: "0"}},
		{"tags above maximum", map[string]string{EnvArchiveTagsPerVersion: "17"}},
		{"share below minimum", map[string]string{EnvArchiveTranscriptSharePercent: "0"}},
		{"share above maximum", map[string]string{EnvArchiveTranscriptSharePercent: "101"}},
		{"share selects no memory", map[string]string{EnvArchiveCardinalities: "10,500", EnvArchiveTranscriptSharePercent: "1"}},
		{"entries below minimum", map[string]string{EnvArchiveTranscriptEntriesPerSelectedMemory: "0"}},
		{"entries above maximum", map[string]string{EnvArchiveTranscriptEntriesPerSelectedMemory: "9"}},
		{"vector dimension below minimum", map[string]string{EnvArchiveVectorDimensions: "1"}},
		{"vector dimension above maximum", map[string]string{EnvArchiveVectorDimensions: "4097"}},
		{"unsafe release", map[string]string{EnvArchiveRelease: "postgres://user:password@host/db"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseArchiveOptions(func(name string) string { return test.overrides[name] })
			if err == nil {
				t.Fatal("invalid archive options unexpectedly accepted")
			}
		})
	}
}

func TestParseArchiveOptionsRejectsDottedLabels(t *testing.T) {
	if _, err := ParseArchiveOptions(func(name string) string {
		if name == EnvArchiveProvider {
			return "pg17.db.internal.example.net"
		}
		return ""
	}); err == nil {
		t.Fatal("dotted archive provider label was accepted")
	}
	if _, err := ParseArchiveOptions(func(name string) string {
		if name == EnvArchiveHardwareTier {
			return "tier.internal"
		}
		return ""
	}); err == nil {
		t.Fatal("dotted archive hardware tier label was accepted")
	}
	opts, err := ParseArchiveOptions(func(name string) string {
		switch name {
		case EnvArchiveRelease:
			return "v0.0.270"
		case EnvArchiveCommit:
			return "release.270+dirty"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("dotted safe archive metadata rejected: %v", err)
	}
	if opts.Release != "v0.0.270" || opts.Commit != "release.270+dirty" {
		t.Fatalf("safe archive metadata = %q, %q", opts.Release, opts.Commit)
	}
}

func TestParseArchiveOptionsDefaultsPidScopedResultsPath(t *testing.T) {
	opts, err := ParseArchiveOptions(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("/tmp/witself-memory-archive-load-%d.json", os.Getpid())
	if opts.ResultsPath != want {
		t.Fatalf("default archive results path = %q, want %q", opts.ResultsPath, want)
	}
}

func TestArchivePortableTableNamesAreCompleteSortedAndFresh(t *testing.T) {
	first := ArchivePortableTableNames()
	second := ArchivePortableTableNames()
	if len(first) != ArchivePortableTableCount || len(second) != ArchivePortableTableCount {
		t.Fatalf("portable table count = %d / %d, want %d", len(first), len(second), ArchivePortableTableCount)
	}
	if !sort.StringsAreSorted(first) {
		t.Fatalf("portable tables are not sorted: %#v", first)
	}
	for index := 1; index < len(first); index++ {
		if first[index] == first[index-1] {
			t.Fatalf("duplicate portable table %q", first[index])
		}
	}
	first[0] = "mutated"
	if second[0] != "account_events" {
		t.Fatal("portable table names did not return a fresh copy")
	}
	for _, required := range []string{
		"memories", "memory_versions", "memory_evidence", "memory_relations",
		"memory_vector_profiles", "memory_vectors", "transcript_conversations", "transcript_entries",
	} {
		index := sort.SearchStrings(second, required)
		if index == len(second) || second[index] != required {
			t.Fatalf("portable table registry is missing %q", required)
		}
	}
}

func TestArchiveFocalCountsAndThroughputAreDeterministic(t *testing.T) {
	workload := validArchiveTestWorkload()
	selected, err := ArchiveTranscriptSelectedCount(100, 25)
	if err != nil || selected != 25 {
		t.Fatalf("selected transcript memories = %d, %v", selected, err)
	}
	counts, err := ArchiveFocalCountsFor(100, workload)
	if err != nil {
		t.Fatal(err)
	}
	want := ArchiveFocalCounts{
		Memories: 100, MemoryVersions: 200, MemoryEvidence: 200,
		MemoryRelations: 100, TranscriptConversations: 1, TranscriptEntries: 50,
		MemoryVectorProfiles: 1, MemoryVectors: 100, TagAssignments: 600,
	}
	if counts != want {
		t.Fatalf("archive focal counts = %#v, want %#v", counts, want)
	}
	if got := ArchiveThroughput(10, 4); got != 2500 {
		t.Fatalf("archive throughput = %v, want 2500", got)
	}
	if got := ArchiveThroughput(0, 4); got != 0 {
		t.Fatalf("invalid archive throughput = %v", got)
	}
	if _, err := ArchiveTranscriptSelectedCount(10, 1); err == nil {
		t.Fatal("zero-sized transcript selection was accepted")
	}
	invalid := workload
	invalid.TagsPerVersion = 0
	if _, err := ArchiveFocalCountsFor(100, invalid); err == nil {
		t.Fatal("invalid focal workload was accepted")
	}
}

func TestArchiveResultSchemaIsCheckedInResolvedAndValidatesInstances(t *testing.T) {
	first := ArchiveResultJSONSchema()
	second := ArchiveResultJSONSchema()
	if len(first) == 0 || len(second) == 0 {
		t.Fatal("embedded archive result schema is empty")
	}
	first[0] = 'x'
	if second[0] != '{' {
		t.Fatal("archive result schema did not return a fresh copy")
	}
	var schema map[string]any
	if err := json.Unmarshal(second, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" ||
		schema["$id"] != "https://witself.witwave.ai/schemas/memory-archive-load-result.v1.schema.json" {
		t.Fatalf("archive result schema identity = %#v", schema)
	}
	if _, err := resolvedArchiveResultSchema(); err != nil {
		t.Fatalf("resolve checked-in archive Draft 2020-12 schema: %v", err)
	}

	result := validArchiveTestResult(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	raw, err := MarshalArchiveResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateArchiveResultJSON(raw); err != nil {
		t.Fatalf("validate valid archive result JSON: %v", err)
	}
	result.Workload.Seed = -9223372036854775808
	if _, err := MarshalArchiveResult(result); err != nil {
		t.Fatalf("marshal archive signed 64-bit minimum seed: %v", err)
	}

	reject := func(name string, mutate func(map[string]any)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			var invalid map[string]any
			if err := json.Unmarshal(raw, &invalid); err != nil {
				t.Fatal(err)
			}
			mutate(invalid)
			invalidRaw, err := json.Marshal(invalid)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateArchiveResultJSON(invalidRaw); err == nil {
				t.Fatal("archive schema accepted invalid evidence")
			}
		})
	}
	reject("top-level property", func(instance map[string]any) { instance["unexpected"] = true })
	reject("active manifest", func(instance map[string]any) {
		archiveJSONOutcome(instance, 0)["manifest_status"] = "active"
	})
	reject("vector limitation", func(instance map[string]any) {
		instance["outcomes"].(map[string]any)["vector_portability"].(map[string]any)["vector_portability_limited"] = true
	})
	reject("score tolerance", func(instance map[string]any) {
		archiveJSONOutcome(instance, 0)["recall_equivalence"].(map[string]any)["score_tolerance"] = 0.001
	})
	reject("nested table property", func(instance map[string]any) {
		archiveJSONOutcome(instance, 0)["table_rows"].([]any)[0].(map[string]any)["unexpected"] = true
	})
}

func TestValidateArchiveResultRejectsPartialOrFailedRuns(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*ArchiveResult)
		wantError string
	}{
		{"missing measurement rung", func(result *ArchiveResult) {
			result.Measurements.CardinalityLadder = result.Measurements.CardinalityLadder[:2]
		}, "ladder completeness"},
		{"missing outcome rung", func(result *ArchiveResult) {
			result.Outcomes.CardinalityLadder = result.Outcomes.CardinalityLadder[:2]
		}, "ladder completeness"},
		{"fixture account count", func(result *ArchiveResult) {
			result.Workload.SyntheticAccounts--
		}, "workload"},
		{"phase count", func(result *ArchiveResult) {
			result.Measurements.CardinalityLadder[0].Export.Count = 2
		}, "cardinality measurements"},
		{"rung order", func(result *ArchiveResult) {
			result.Measurements.CardinalityLadder[0].MemoryCount = 500
		}, "cardinality measurements"},
		{"partial registry", func(result *ArchiveResult) {
			outcome := &result.Outcomes.CardinalityLadder[0]
			outcome.TableRows = outcome.TableRows[:len(outcome.TableRows)-1]
			outcome.ManifestTables--
		}, "manifest and artifact"},
		{"table order", func(result *ArchiveResult) {
			rows := result.Outcomes.CardinalityLadder[0].TableRows
			rows[0], rows[1] = rows[1], rows[0]
		}, "table row outcomes"},
		{"verified row mismatch", func(result *ArchiveResult) {
			index := archiveTestTableIndex(result.Outcomes.CardinalityLadder[0].TableRows, "memories")
			result.Outcomes.CardinalityLadder[0].TableRows[index].Verified--
		}, "table row outcomes"},
		{"table total", func(result *ArchiveResult) {
			result.Outcomes.CardinalityLadder[0].ExportedRows--
		}, "table row totals"},
		{"focal formula", func(result *ArchiveResult) {
			result.Outcomes.CardinalityLadder[0].FocalCounts.MemoryVersions--
		}, "focal counts"},
		{"focal vector table", func(result *ArchiveResult) {
			outcome := &result.Outcomes.CardinalityLadder[0]
			index := archiveTestTableIndex(outcome.TableRows, "memory_vectors")
			outcome.TableRows[index].Exported--
			outcome.TableRows[index].Verified--
			outcome.TableRows[index].Imported--
			outcome.ExportedRows--
			outcome.VerifiedRows--
			outcome.ImportedRows--
		}, "focal table counts"},
		{"checksum assertion", func(result *ArchiveResult) {
			result.Outcomes.CardinalityLadder[0].ChecksumsRead = false
		}, "verification outcomes"},
		{"recall calls", func(result *ArchiveResult) {
			result.Outcomes.CardinalityLadder[0].RecallEquivalence.AfterCalls--
		}, "recall equivalence outcomes"},
		{"recall ordering", func(result *ArchiveResult) {
			result.Outcomes.CardinalityLadder[0].RecallEquivalence.Cases[0].RankedIDsIdentical = false
		}, "recall equivalence case"},
		{"score tolerance", func(result *ArchiveResult) {
			result.Outcomes.CardinalityLadder[0].RecallEquivalence.ScoreTolerance = 0.001
		}, "recall equivalence outcomes"},
		{"cross-account safety", func(result *ArchiveResult) {
			result.Outcomes.CardinalityLadder[0].Safety.CrossAccountIsolated = false
		}, "safety outcomes"},
		{"transfer rate", func(result *ArchiveResult) {
			result.Measurements.CardinalityLadder[0].ImportBytesPerSecond++
		}, "transfer rates"},
		{"vector portability", func(result *ArchiveResult) {
			result.Outcomes.VectorPortability.MemoryVectorsPortable = false
		}, "vector portability"},
		{"completion roll-up", func(result *ArchiveResult) {
			result.Outcomes.AllCardinalitiesComplete = false
		}, "completion outcomes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := validArchiveTestResult(time.Now().UTC())
			test.mutate(&result)
			err := ValidateArchiveResult(result)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("archive validation error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestValidateArchiveResultAcceptsSlowCompleteMeasurements(t *testing.T) {
	result := validArchiveTestResult(time.Now().UTC())
	result.CompletedAt = result.StartedAt.Add(12 * time.Hour)
	for index := range result.Measurements.CardinalityLadder {
		measurement := &result.Measurements.CardinalityLadder[index]
		measurement.Seed = archiveSlowTestStats(1)
		measurement.Export = archiveSlowTestStats(1)
		measurement.Verify = archiveSlowTestStats(1)
		measurement.Import = archiveSlowTestStats(1)
		measurement.LexicalRecallBefore = archiveSlowTestStats(2)
		measurement.LexicalRecallAfter = archiveSlowTestStats(2)
		measurement.HybridRecallBefore = archiveSlowTestStats(2)
		measurement.HybridRecallAfter = archiveSlowTestStats(2)
		measurement.PostImportSafety = archiveSlowTestStats(4)
		outcome := result.Outcomes.CardinalityLadder[index]
		measurement.ExportRowsPerSecond = ArchiveThroughput(int64(outcome.ExportedRows), measurement.Export.WallDurationMS)
		measurement.ExportBytesPerSecond = ArchiveThroughput(outcome.ArchiveBytes, measurement.Export.WallDurationMS)
		measurement.ImportRowsPerSecond = ArchiveThroughput(int64(outcome.ImportedRows), measurement.Import.WallDurationMS)
		measurement.ImportBytesPerSecond = ArchiveThroughput(outcome.ArchiveBytes, measurement.Import.WallDurationMS)
	}
	if err := ValidateArchiveResult(result); err != nil {
		t.Fatalf("slow but complete archive evidence was rejected: %v", err)
	}
}

func TestValidateArchiveResultAcceptsSummarizedNonRoundWallDuration(t *testing.T) {
	result := validArchiveTestResult(time.Now().UTC())
	for index := range result.Measurements.CardinalityLadder {
		measurement := &result.Measurements.CardinalityLadder[index]
		measurement.Seed = archiveSummarizedTestStats(t, 1)
		measurement.Export = archiveSummarizedTestStats(t, 1)
		measurement.Verify = archiveSummarizedTestStats(t, 1)
		measurement.Import = archiveSummarizedTestStats(t, 1)
		measurement.LexicalRecallBefore = archiveSummarizedTestStats(t, 2)
		measurement.LexicalRecallAfter = archiveSummarizedTestStats(t, 2)
		measurement.HybridRecallBefore = archiveSummarizedTestStats(t, 2)
		measurement.HybridRecallAfter = archiveSummarizedTestStats(t, 2)
		measurement.PostImportSafety = archiveSummarizedTestStats(t, 4)
		outcome := result.Outcomes.CardinalityLadder[index]
		measurement.ExportRowsPerSecond = ArchiveThroughput(int64(outcome.ExportedRows), measurement.Export.WallDurationMS)
		measurement.ExportBytesPerSecond = ArchiveThroughput(outcome.ArchiveBytes, measurement.Export.WallDurationMS)
		measurement.ImportRowsPerSecond = ArchiveThroughput(int64(outcome.ImportedRows), measurement.Import.WallDurationMS)
		measurement.ImportBytesPerSecond = ArchiveThroughput(outcome.ArchiveBytes, measurement.Import.WallDurationMS)
	}
	if err := ValidateArchiveResult(result); err != nil {
		t.Fatalf("archive evidence produced by Summarize was rejected: %v", err)
	}
}

func TestWriteArchiveResultIsPrivateAtomicAndSanitized(t *testing.T) {
	result := validArchiveTestResult(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "nested", "archive-result.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := WriteArchiveResult(path, result)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("archive result mode = %o, want 600", mode)
	}
	for _, forbidden := range []string{
		"postgres://", "password", "hostname", "account_id", "agent_id",
		"memory_id", "profile_id", "\"query\":", "content_hash", "\"tags\":",
		"archive lexical fixture", "sensitive fixture", "[0.1,0.2]",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("archive result contains forbidden value %q:\n%s", forbidden, raw)
		}
	}
	var decoded ArchiveResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != ArchiveResultSchemaV1 || !decoded.Outcomes.AllCardinalitiesComplete ||
		decoded.Outcomes.VectorPortability.VectorPortabilityLimited {
		t.Fatalf("decoded archive result = %#v", decoded)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("archive result directory contains temporary artifacts: %#v", entries)
	}
}

func archiveJSONOutcome(instance map[string]any, index int) map[string]any {
	return instance["outcomes"].(map[string]any)["cardinality_ladder"].([]any)[index].(map[string]any)
}

func archiveTestStats(count int) OperationStats {
	const wallMS = 10.0
	return OperationStats{
		Count: count, WallDurationMS: wallMS,
		ThroughputPerSecond: rounded(float64(count) * 1000 / wallMS),
		MinimumMS:           0.1, P50MS: 0.2, P95MS: 0.4, P99MS: 0.5, MaximumMS: 0.5,
	}
}

func archiveSlowTestStats(count int) OperationStats {
	const wallMS = 600_000.0
	return OperationStats{
		Count: count, WallDurationMS: wallMS,
		ThroughputPerSecond: rounded(float64(count) * 1000 / wallMS),
		MinimumMS:           100, P50MS: 200, P95MS: 400, P99MS: 500, MaximumMS: 500,
	}
}

func archiveSummarizedTestStats(t *testing.T, count int) OperationStats {
	t.Helper()
	durations := make([]time.Duration, count)
	for index := range durations {
		durations[index] = time.Duration(index+1) * 137 * time.Microsecond
	}
	stats, err := Summarize(durations, 1_234_567*time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ThroughputPerSecond == ArchiveThroughput(int64(count), stats.WallDurationMS) {
		t.Fatal("test fixture does not exercise independent wall and throughput rounding")
	}
	return stats
}

func validArchiveTestWorkload() ArchiveWorkload {
	cardinalities := []int{100, 500, 2000}
	return ArchiveWorkload{
		Seed: DefaultArchiveSeed, SyntheticAccounts: len(cardinalities) + 1,
		SyntheticAgents: 2*len(cardinalities) + 1, Cardinalities: cardinalities,
		VersionsPerMemory: 2, EvidencePerMemory: 2, RelationsPerMemory: 1,
		TagsPerVersion: 3, TranscriptSharePercent: 25,
		TranscriptEntriesPerSelectedMemory: 2, VectorDimensions: 32,
		LexicalQueries: 2, HybridQueries: 2, SafetyCalls: 4,
	}
}

func validArchiveTestResult(started time.Time) ArchiveResult {
	workload := validArchiveTestWorkload()
	measurements := make([]ArchiveCardinalityMeasurement, 0, len(workload.Cardinalities))
	outcomes := make([]ArchiveCardinalityOutcome, 0, len(workload.Cardinalities))
	for _, memoryCount := range workload.Cardinalities {
		measurement, outcome := validArchiveTestCardinality(memoryCount, workload)
		measurements = append(measurements, measurement)
		outcomes = append(outcomes, outcome)
	}
	return ArchiveResult{
		Schema: ArchiveResultSchemaV1, HarnessVersion: ArchiveHarnessVersion,
		StartedAt: started, CompletedAt: started.Add(time.Second), Outcome: "pass",
		PostgreSQLVersion: "18.0",
		Environment: SafeMetadata{
			Release: "v0.0.267", Commit: "release.267+dirty", Provider: "gcp",
			HardwareTier: "db-custom-2-7680", GoVersion: "go1.26.6",
			GOOS: "darwin", GOARCH: "arm64", LogicalCPUs: 8,
		},
		Workload:     workload,
		Measurements: ArchiveMeasurements{CardinalityLadder: measurements},
		Outcomes: ArchiveOutcomes{
			CardinalityLadder: outcomes,
			VectorPortability: ArchiveVectorPortabilityOutcome{
				VectorProfilesPortable: true, MemoryVectorsPortable: true,
				HybridEquivalenceExercised: true, VectorPortabilityLimited: false,
				VectorProfilesRoundTripped: true, MemoryVectorsRoundTripped: true,
			},
			AllCardinalitiesComplete: true,
		},
	}
}

func validArchiveTestCardinality(memoryCount int, workload ArchiveWorkload) (ArchiveCardinalityMeasurement, ArchiveCardinalityOutcome) {
	focal, err := ArchiveFocalCountsFor(memoryCount, workload)
	if err != nil {
		panic(err)
	}
	tableCounts := map[string]int{
		"accounts": 1, "agents": 2,
		"memories": focal.Memories, "memory_versions": focal.MemoryVersions,
		"memory_evidence": focal.MemoryEvidence, "memory_relations": focal.MemoryRelations,
		"transcript_conversations": focal.TranscriptConversations,
		"transcript_entries":       focal.TranscriptEntries,
		"memory_vector_profiles":   focal.MemoryVectorProfiles,
		"memory_vectors":           focal.MemoryVectors,
	}
	tables := make([]ArchiveTableRows, 0, ArchivePortableTableCount)
	totalRows := 0
	nonEmpty := 0
	for _, name := range ArchivePortableTableNames() {
		count := tableCounts[name]
		tables = append(tables, ArchiveTableRows{Name: name, Exported: count, Verified: count, Imported: count})
		totalRows += count
		if count > 0 {
			nonEmpty++
		}
	}
	archiveBytes := int64(totalRows*100 + 1000)
	outcome := ArchiveCardinalityOutcome{
		MemoryCount: memoryCount, ManifestFormatVersion: 1, ManifestSchemaVersion: 94,
		ManifestPurpose: "self", ManifestStatus: "suspended",
		ArchiveBytes: archiveBytes, ChunkBytes: int64(totalRows*120 + 800),
		ManifestTables: ArchivePortableTableCount, NonEmptyTables: nonEmpty, ChunkCount: nonEmpty,
		ExportedRows: totalRows, VerifiedRows: totalRows, ImportedRows: totalRows,
		TableRows: tables, FocalCounts: focal,
		RecallEquivalence: ArchiveRecallEquivalenceOutcome{
			Cases: []ArchiveRecallEquivalenceCase{
				{Name: ArchiveRecallCaseLexicalPrimary, Mode: "lexical", BeforeHits: 10, AfterHits: 10, RankedIDsIdentical: true, ScoreComponentsExact: true, MetadataExact: true},
				{Name: ArchiveRecallCaseLexicalSecondary, Mode: "lexical", BeforeHits: 10, AfterHits: 10, RankedIDsIdentical: true, ScoreComponentsExact: true, MetadataExact: true},
				{Name: ArchiveRecallCaseHybridPrimary, Mode: "hybrid", BeforeHits: 10, AfterHits: 10, RankedIDsIdentical: true, ScoreComponentsExact: true, MetadataExact: true},
				{Name: ArchiveRecallCaseHybridSecondary, Mode: "hybrid", BeforeHits: 10, AfterHits: 10, RankedIDsIdentical: true, ScoreComponentsExact: true, MetadataExact: true},
			},
			BeforeCalls: 4, AfterCalls: 4, ScoreComparison: "exact", ScoreTolerance: 0,
			AllRankingsIdentical: true, AllScoreComponentsExact: true,
			AllMetadataExact: true, RetrievalProjectionProved: true,
		},
		Safety: ArchiveSafetyOutcome{
			RecallCalls: 4, SensitiveBroadRedacted: true, SensitiveExactOwnerVisible: true,
			CrossAgentIsolated: true, CrossAccountIsolated: true,
		},
		ChecksumsRead: true, AllChunksVerified: true, AllTablesVerified: true,
		AccountRemovedBeforeImport: true, SameStoreRoundTrip: true,
		ExactTableRowCounts: true, ExactFixtureCounts: true,
	}
	measurement := ArchiveCardinalityMeasurement{
		MemoryCount: memoryCount,
		Seed:        archiveTestStats(1), Export: archiveTestStats(1),
		Verify: archiveTestStats(1), Import: archiveTestStats(1),
		LexicalRecallBefore: archiveTestStats(2), LexicalRecallAfter: archiveTestStats(2),
		HybridRecallBefore: archiveTestStats(2), HybridRecallAfter: archiveTestStats(2),
		PostImportSafety: archiveTestStats(4),
	}
	measurement.ExportRowsPerSecond = ArchiveThroughput(int64(outcome.ExportedRows), measurement.Export.WallDurationMS)
	measurement.ExportBytesPerSecond = ArchiveThroughput(outcome.ArchiveBytes, measurement.Export.WallDurationMS)
	measurement.ImportRowsPerSecond = ArchiveThroughput(int64(outcome.ImportedRows), measurement.Import.WallDurationMS)
	measurement.ImportBytesPerSecond = ArchiveThroughput(outcome.ArchiveBytes, measurement.Import.WallDurationMS)
	return measurement, outcome
}

func archiveTestTableIndex(rows []ArchiveTableRows, name string) int {
	for index := range rows {
		if rows[index].Name == name {
			return index
		}
	}
	panic("archive test table not found: " + name)
}
