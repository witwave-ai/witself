// Package loadquality defines the deterministic, value-safe contract shared by
// the opt-in PostgreSQL narrative-memory load and quality harness.
//
// It deliberately contains no model client, embedding provider, database DSN,
// account identifier, agent identifier, memory identifier, or memory value in
// its result schema. The store integration harness owns fixture creation and
// emits only the aggregate and labeled outcomes defined here.
package loadquality

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Load-quality schemas, defaults, and hard workload bounds.
const (
	CorpusSchemaV1 = "witself.memory-load-quality-corpus.v1"
	ResultSchemaV1 = "witself.memory-load-quality-result.v1"
	HarnessVersion = "1"

	DefaultSeed            = 20260717
	DefaultNoiseMemories   = 250
	DefaultQueryIterations = 25
	DefaultConcurrency     = 4

	MaximumNoiseMemories   = 10_000
	MaximumQueryIterations = 10_000
	MaximumConcurrency     = 64
)

// Environment variables accepted by ParseOptions.
const (
	EnvResultsPath     = "WITSELF_MEMORY_LOAD_QUALITY_RESULTS"
	EnvSeed            = "WITSELF_MEMORY_LOAD_QUALITY_SEED"
	EnvNoiseMemories   = "WITSELF_MEMORY_LOAD_QUALITY_NOISE_MEMORIES"
	EnvQueryIterations = "WITSELF_MEMORY_LOAD_QUALITY_QUERY_ITERATIONS"
	EnvConcurrency     = "WITSELF_MEMORY_LOAD_QUALITY_CONCURRENCY"
	EnvRelease         = "WITSELF_MEMORY_LOAD_QUALITY_RELEASE"
	EnvCommit          = "WITSELF_MEMORY_LOAD_QUALITY_COMMIT"
	EnvProvider        = "WITSELF_MEMORY_LOAD_QUALITY_PROVIDER"
	EnvHardwareTier    = "WITSELF_MEMORY_LOAD_QUALITY_HARDWARE_TIER"
)

//go:embed testdata/corpus.v1.json
var defaultCorpusJSON []byte

//go:embed testdata/result-schema.v1.json
var resultSchemaJSON []byte

// Corpus is a small, labeled, provider-neutral retrieval fixture. Content is
// synthetic and is never copied into the sanitized result artifact.
type Corpus struct {
	Schema           string         `json:"schema"`
	Memories         []CorpusMemory `json:"memories"`
	RelevanceQueries []QueryCase    `json:"relevance_queries"`
	SensitiveProbe   QueryCase      `json:"sensitive_probe"`
}

// CorpusMemory is one synthetic labeled retrieval fixture.
type CorpusMemory struct {
	Label     string   `json:"label"`
	Content   string   `json:"content"`
	Kind      string   `json:"kind"`
	Tags      []string `json:"tags"`
	Salience  float64  `json:"salience"`
	Sensitive bool     `json:"sensitive"`
}

// QueryCase names one synthetic retrieval expectation.
type QueryCase struct {
	Name          string `json:"name"`
	Query         string `json:"query"`
	ExpectedLabel string `json:"expected_label"`
	MaximumRank   int    `json:"maximum_rank"`
}

// Options contains only bounded workload controls and safe evidence metadata.
// The database DSN stays outside this type so it cannot enter a result by
// accident.
type Options struct {
	ResultsPath     string
	Seed            int64
	NoiseMemories   int
	QueryIterations int
	Concurrency     int
	Release         string
	Commit          string
	Provider        string
	HardwareTier    string
}

// SafeMetadata is operator-supplied release/environment context with a narrow
// character set. It must never contain a hostname, DSN, resource id, or secret.
type SafeMetadata struct {
	Release      string `json:"release"`
	Commit       string `json:"commit"`
	Provider     string `json:"provider"`
	HardwareTier string `json:"hardware_tier"`
	GoVersion    string `json:"go_version"`
	GOOS         string `json:"goos"`
	GOARCH       string `json:"goarch"`
	LogicalCPUs  int    `json:"logical_cpus"`
}

// Workload records the bounded deterministic fixture shape.
type Workload struct {
	Seed              int64  `json:"seed"`
	CorpusSHA256      string `json:"corpus_sha256"`
	SyntheticAccounts int    `json:"synthetic_accounts"`
	SyntheticAgents   int    `json:"synthetic_agents"`
	CorpusMemories    int    `json:"corpus_memories"`
	NoiseMemories     int    `json:"noise_memories"`
	QueryIterations   int    `json:"query_iterations"`
	Concurrency       int    `json:"concurrency"`
}

// OperationStats is an intentionally small first baseline. Wall time drives
// throughput so concurrent recalls are not misreported as serialized work.
type OperationStats struct {
	Count               int     `json:"count"`
	WallDurationMS      float64 `json:"wall_duration_ms"`
	ThroughputPerSecond float64 `json:"throughput_per_second"`
	MinimumMS           float64 `json:"minimum_ms"`
	P50MS               float64 `json:"p50_ms"`
	P95MS               float64 `json:"p95_ms"`
	P99MS               float64 `json:"p99_ms"`
	MaximumMS           float64 `json:"maximum_ms"`
}

// Measurements contains capture and recall aggregate statistics.
type Measurements struct {
	Capture OperationStats `json:"capture"`
	Recall  OperationStats `json:"recall"`
}

// RelevanceCaseResult identifies a corpus label, never a query, memory value,
// or durable record id.
type RelevanceCaseResult struct {
	Name         string `json:"name"`
	Passed       bool   `json:"passed"`
	ObservedRank int    `json:"observed_rank"`
	MaximumRank  int    `json:"maximum_rank"`
}

// Quality contains value-free relevance, redaction, and isolation outcomes.
type Quality struct {
	RelevanceCases             []RelevanceCaseResult `json:"relevance_cases"`
	RelevancePassRate          float64               `json:"relevance_pass_rate"`
	SensitiveBroadRedacted     bool                  `json:"sensitive_broad_redacted"`
	SensitiveExactOwnerVisible bool                  `json:"sensitive_exact_owner_visible"`
	CrossAgentIsolated         bool                  `json:"cross_agent_isolated"`
	CrossTenantIsolated        bool                  `json:"cross_tenant_isolated"`
}

// Result is safe to retain as CI or release evidence. PostgreSQLVersion is
// server software metadata only; endpoint and database identity are excluded.
type Result struct {
	Schema            string       `json:"schema"`
	HarnessVersion    string       `json:"harness_version"`
	StartedAt         time.Time    `json:"started_at"`
	CompletedAt       time.Time    `json:"completed_at"`
	Outcome           string       `json:"outcome"`
	PostgreSQLVersion string       `json:"postgresql_version"`
	Environment       SafeMetadata `json:"environment"`
	Workload          Workload     `json:"workload"`
	Measurements      Measurements `json:"measurements"`
	Quality           Quality      `json:"quality"`
}

// DefaultCorpus returns a fresh copy of the checked-in corpus and its digest.
func DefaultCorpus() (Corpus, string, error) {
	var corpus Corpus
	if err := json.Unmarshal(defaultCorpusJSON, &corpus); err != nil {
		return Corpus{}, "", fmt.Errorf("decode embedded load-quality corpus: %w", err)
	}
	if err := ValidateCorpus(corpus); err != nil {
		return Corpus{}, "", err
	}
	digest := sha256.Sum256(defaultCorpusJSON)
	return corpus, hex.EncodeToString(digest[:]), nil
}

// ResultJSONSchema returns a fresh copy of the checked-in public result schema.
func ResultJSONSchema() []byte { return append([]byte(nil), resultSchemaJSON...) }

// GenerateNoise creates deterministic low-salience distractors without using
// an RNG implementation whose sequence could drift across Go versions.
func GenerateNoise(seed int64, count int) ([]CorpusMemory, error) {
	if count < 0 || count > MaximumNoiseMemories {
		return nil, fmt.Errorf("noise memory count must be between 0 and %d", MaximumNoiseMemories)
	}
	adjectives := [...]string{"amber", "brisk", "calm", "delta", "even", "frost", "gentle", "harbor"}
	nouns := [...]string{"packet", "ledger", "window", "signal", "bridge", "queue", "metric", "sample"}
	kinds := [...]string{"note", "session", "observation"}
	out := make([]CorpusMemory, 0, count)
	for i := 0; i < count; i++ {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", seed, i)))
		adjective := adjectives[int(digest[0])%len(adjectives)]
		noun := nouns[int(digest[1])%len(nouns)]
		bucket := int(digest[2]) % 16
		salience := 0.05 + float64(binary.BigEndian.Uint16(digest[3:5])%2500)/10_000
		out = append(out, CorpusMemory{
			Label: fmt.Sprintf("noise_%05d", i),
			Content: fmt.Sprintf(
				"Synthetic background record %05d tracks %s %s cadence %s.",
				i, adjective, noun, hex.EncodeToString(digest[5:9]),
			),
			Kind:     kinds[int(digest[9])%len(kinds)],
			Tags:     []string{"synthetic-load", fmt.Sprintf("bucket_%02d", bucket)},
			Salience: salience,
		})
	}
	return out, nil
}

// ValidateCorpus rejects unsafe, ambiguous, or unbounded fixtures.
func ValidateCorpus(corpus Corpus) error {
	if corpus.Schema != CorpusSchemaV1 {
		return fmt.Errorf("load-quality corpus schema must be %q", CorpusSchemaV1)
	}
	if len(corpus.Memories) < 3 || len(corpus.Memories) > 100 {
		return errors.New("load-quality corpus must contain between 3 and 100 memories")
	}
	labels := make(map[string]CorpusMemory, len(corpus.Memories))
	for _, memory := range corpus.Memories {
		if !safeLabel(memory.Label) || memory.Content == "" || len(memory.Content) > 4096 ||
			!safeLabel(memory.Kind) || len(memory.Tags) > 20 ||
			math.IsNaN(memory.Salience) || math.IsInf(memory.Salience, 0) ||
			memory.Salience < 0 || memory.Salience > 1 {
			return fmt.Errorf("invalid load-quality corpus memory %q", memory.Label)
		}
		if _, exists := labels[memory.Label]; exists {
			return fmt.Errorf("duplicate load-quality corpus memory label %q", memory.Label)
		}
		for _, tag := range memory.Tags {
			if !safeLabel(tag) {
				return fmt.Errorf("invalid load-quality corpus tag for %q", memory.Label)
			}
		}
		labels[memory.Label] = memory
	}
	if len(corpus.RelevanceQueries) < 1 || len(corpus.RelevanceQueries) > 50 {
		return errors.New("load-quality corpus must contain between 1 and 50 relevance queries")
	}
	queryNames := make(map[string]struct{}, len(corpus.RelevanceQueries)+1)
	validateQuery := func(query QueryCase) (CorpusMemory, error) {
		if !safeLabel(query.Name) || strings.TrimSpace(query.Query) == "" || len(query.Query) > 512 ||
			query.MaximumRank < 1 || query.MaximumRank > 20 {
			return CorpusMemory{}, fmt.Errorf("invalid load-quality query %q", query.Name)
		}
		if _, exists := queryNames[query.Name]; exists {
			return CorpusMemory{}, fmt.Errorf("duplicate load-quality query name %q", query.Name)
		}
		queryNames[query.Name] = struct{}{}
		memory, exists := labels[query.ExpectedLabel]
		if !exists {
			return CorpusMemory{}, fmt.Errorf("load-quality query %q references unknown label", query.Name)
		}
		return memory, nil
	}
	for _, query := range corpus.RelevanceQueries {
		memory, err := validateQuery(query)
		if err != nil {
			return err
		}
		if memory.Sensitive {
			return fmt.Errorf("load-quality relevance query %q must reference a non-sensitive memory", query.Name)
		}
	}
	sensitiveMemory, err := validateQuery(corpus.SensitiveProbe)
	if err != nil {
		return err
	}
	if !sensitiveMemory.Sensitive {
		return errors.New("load-quality sensitive probe must reference a sensitive memory")
	}
	return nil
}

// ParseOptions reads bounded controls through an injected lookup function so
// tests never mutate the process environment.
func ParseOptions(getenv func(string) string) (Options, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	opts := Options{
		ResultsPath: strings.TrimSpace(getenv(EnvResultsPath)),
		Seed:        DefaultSeed, NoiseMemories: DefaultNoiseMemories,
		QueryIterations: DefaultQueryIterations, Concurrency: DefaultConcurrency,
		Release:      metadataOrDefault(getenv(EnvRelease), "dev"),
		Commit:       metadataOrDefault(getenv(EnvCommit), "none"),
		Provider:     metadataOrDefault(getenv(EnvProvider), "local"),
		HardwareTier: metadataOrDefault(getenv(EnvHardwareTier), "unspecified"),
	}
	if opts.ResultsPath == "" {
		return Options{}, fmt.Errorf("%s is required", EnvResultsPath)
	}
	var err error
	if opts.Seed, err = parseInt64(getenv(EnvSeed), DefaultSeed, math.MinInt64, math.MaxInt64, EnvSeed); err != nil {
		return Options{}, err
	}
	if opts.NoiseMemories, err = parseInt(getenv(EnvNoiseMemories), DefaultNoiseMemories, 0, MaximumNoiseMemories, EnvNoiseMemories); err != nil {
		return Options{}, err
	}
	if opts.QueryIterations, err = parseInt(getenv(EnvQueryIterations), DefaultQueryIterations, 1, MaximumQueryIterations, EnvQueryIterations); err != nil {
		return Options{}, err
	}
	if opts.Concurrency, err = parseInt(getenv(EnvConcurrency), DefaultConcurrency, 1, MaximumConcurrency, EnvConcurrency); err != nil {
		return Options{}, err
	}
	for name, value := range map[string]string{
		EnvRelease: opts.Release, EnvCommit: opts.Commit,
		EnvProvider: opts.Provider, EnvHardwareTier: opts.HardwareTier,
	} {
		if !safeMetadata(value) {
			return Options{}, fmt.Errorf("%s contains unsafe evidence metadata", name)
		}
	}
	return opts, nil
}

// Environment builds the sanitized runner metadata retained in evidence.
func Environment(opts Options) SafeMetadata {
	return SafeMetadata{
		Release: opts.Release, Commit: opts.Commit, Provider: opts.Provider,
		HardwareTier: opts.HardwareTier, GoVersion: runtime.Version(),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, LogicalCPUs: runtime.NumCPU(),
	}
}

// Summarize computes wall-time throughput and nearest-rank latency percentiles.
func Summarize(durations []time.Duration, wall time.Duration) (OperationStats, error) {
	if len(durations) == 0 || wall <= 0 {
		return OperationStats{}, errors.New("load-quality measurements require durations and positive wall time")
	}
	ordered := append([]time.Duration(nil), durations...)
	for _, duration := range ordered {
		if duration < 0 {
			return OperationStats{}, errors.New("load-quality duration cannot be negative")
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	toMS := func(duration time.Duration) float64 {
		return rounded(float64(duration) / float64(time.Millisecond))
	}
	return OperationStats{
		Count: len(ordered), WallDurationMS: toMS(wall),
		ThroughputPerSecond: rounded(float64(len(ordered)) / wall.Seconds()),
		MinimumMS:           toMS(ordered[0]), P50MS: toMS(nearestRank(ordered, 0.50)),
		P95MS: toMS(nearestRank(ordered, 0.95)), P99MS: toMS(nearestRank(ordered, 0.99)),
		MaximumMS: toMS(ordered[len(ordered)-1]),
	}, nil
}

// ValidateResult requires a complete passing result before it can be retained.
func ValidateResult(result Result) error {
	if result.Schema != ResultSchemaV1 || result.HarnessVersion != HarnessVersion ||
		result.Outcome != "pass" || result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) ||
		strings.TrimSpace(result.PostgreSQLVersion) == "" || len(result.PostgreSQLVersion) > 128 {
		return errors.New("invalid load-quality result envelope")
	}
	if result.Workload.SyntheticAccounts != 2 || result.Workload.SyntheticAgents != 3 ||
		result.Workload.CorpusMemories < 3 || result.Workload.NoiseMemories < 0 ||
		result.Workload.NoiseMemories > MaximumNoiseMemories ||
		result.Workload.QueryIterations < 1 || result.Workload.QueryIterations > MaximumQueryIterations ||
		result.Workload.Concurrency < 1 || result.Workload.Concurrency > MaximumConcurrency ||
		len(result.Workload.CorpusSHA256) != 64 {
		return errors.New("invalid load-quality result workload")
	}
	if !safeMetadata(result.Environment.Release) || !safeMetadata(result.Environment.Commit) ||
		!safeMetadata(result.Environment.Provider) || !safeMetadata(result.Environment.HardwareTier) ||
		result.Environment.LogicalCPUs < 1 {
		return errors.New("invalid load-quality result environment")
	}
	if result.Measurements.Capture.Count != result.Workload.CorpusMemories+result.Workload.NoiseMemories ||
		result.Measurements.Recall.Count != result.Workload.QueryIterations*len(result.Quality.RelevanceCases) {
		return errors.New("load-quality result measurement count mismatch")
	}
	if !validOperationStats(result.Measurements.Capture) || !validOperationStats(result.Measurements.Recall) {
		return errors.New("invalid load-quality operation measurements")
	}
	if len(result.Quality.RelevanceCases) == 0 || result.Quality.RelevancePassRate != 1 ||
		!result.Quality.SensitiveBroadRedacted || !result.Quality.SensitiveExactOwnerVisible ||
		!result.Quality.CrossAgentIsolated || !result.Quality.CrossTenantIsolated {
		return errors.New("load-quality result cannot pass with failed quality checks")
	}
	for _, item := range result.Quality.RelevanceCases {
		if !safeLabel(item.Name) || !item.Passed || item.ObservedRank < 1 || item.ObservedRank > item.MaximumRank {
			return errors.New("invalid load-quality relevance result")
		}
	}
	return nil
}

// MarshalResult validates and renders one stable, indented evidence document.
func MarshalResult(result Result) ([]byte, error) {
	if err := ValidateResult(result); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// WriteResult writes with private permissions and atomically replaces an older
// result only after the new document is complete.
func WriteResult(path string, result Result) ([]byte, error) {
	raw, err := MarshalResult(result)
	if err != nil {
		return nil, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("load-quality result path is required")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create load-quality result directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".memory-load-quality-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create load-quality result: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("protect load-quality result: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("write load-quality result: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("sync load-quality result: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("close load-quality result: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return nil, fmt.Errorf("publish load-quality result: %w", err)
	}
	return raw, nil
}

var (
	labelPattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	metadataPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
)

func safeLabel(value string) bool { return labelPattern.MatchString(value) }

func safeMetadata(value string) bool { return metadataPattern.MatchString(value) }

func validOperationStats(stats OperationStats) bool {
	values := []float64{
		stats.WallDurationMS, stats.ThroughputPerSecond, stats.MinimumMS,
		stats.P50MS, stats.P95MS, stats.P99MS, stats.MaximumMS,
	}
	if stats.Count < 1 {
		return false
	}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return false
		}
	}
	return stats.WallDurationMS > 0 && stats.ThroughputPerSecond > 0 &&
		stats.MinimumMS <= stats.P50MS && stats.P50MS <= stats.P95MS &&
		stats.P95MS <= stats.P99MS && stats.P99MS <= stats.MaximumMS
}

func metadataOrDefault(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func parseInt(value string, fallback, minimum, maximum int, name string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, minimum, maximum)
	}
	return parsed, nil
}

func parseInt64(value string, fallback, minimum, maximum int64, name string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("%s must be a signed 64-bit integer", name)
	}
	return parsed, nil
}

func nearestRank(ordered []time.Duration, percentile float64) time.Duration {
	index := int(math.Ceil(percentile*float64(len(ordered)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(ordered) {
		index = len(ordered) - 1
	}
	return ordered[index]
}

func rounded(value float64) float64 { return math.Round(value*1000) / 1000 }
