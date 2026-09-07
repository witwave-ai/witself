package loadquality

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// Relevance evidence measures a fixed, independently labeled synthetic corpus.
// Bounds are fixture limits, not quality thresholds or production controls.
const (
	RelevanceCorpusSchemaV1  = "witself.memory-relevance-corpus.v1"
	RelevanceResultSchemaV1  = "witself.memory-relevance-result.v1"
	RelevanceHarnessVersion  = "1"
	RelevanceFixtureClock    = "indexed-seconds-v1"
	MaximumRelevanceMemories = 24
	MaximumRelevanceQueries  = 12
	MaximumRelevanceTopK     = 5

	EnvRelevanceResultsPath  = "WITSELF_MEMORY_RELEVANCE_RESULTS"
	EnvRelevanceRelease      = "WITSELF_MEMORY_RELEVANCE_RELEASE"
	EnvRelevanceCommit       = "WITSELF_MEMORY_RELEVANCE_COMMIT"
	EnvRelevanceProvider     = "WITSELF_MEMORY_RELEVANCE_PROVIDER"
	EnvRelevanceHardwareTier = "WITSELF_MEMORY_RELEVANCE_HARDWARE_TIER"
)

//go:embed testdata/relevance-corpus.v1.json
var relevanceCorpusJSON []byte

//go:embed testdata/relevance-result-schema.v1.json
var relevanceResultSchemaJSON []byte

// RelevanceCorpus keeps synthetic source text and adjudication separate from
// retained results. RelevantLabels are gold judgments, never inferred from rank.
type RelevanceCorpus struct {
	Schema   string            `json:"schema"`
	Memories []RelevanceMemory `json:"memories"`
	Queries  []RelevanceQuery  `json:"queries"`
}

// RelevanceMemory is one labeled synthetic source, excluded from result evidence.
type RelevanceMemory struct {
	Label   string `json:"label"`
	Content string `json:"content"`
}

// RelevanceQuery records a fixed query and its independently adjudicated gold set.
type RelevanceQuery struct {
	Name           string   `json:"name"`
	Query          string   `json:"query"`
	TopK           int      `json:"top_k"`
	RelevantLabels []string `json:"relevant_labels"`
	Rationale      string   `json:"rationale"`
}

// RelevanceOptions accepts evidence destination and metadata only. There is no
// external corpus, query, ranking, workload, or database option.
type RelevanceOptions struct {
	ResultsPath  string
	Release      string
	Commit       string
	Provider     string
	HardwareTier string
}

// RelevanceScore reports the complete top-K observation, including missing
// secondary hits and irrelevant returned hits. Precision uses K even when the
// list is underfilled. Recall is null when the gold set is empty. Ranks are
// sorted, one-based positions; no query, content, or durable ID is retained.
type RelevanceScore struct {
	Name           string   `json:"name"`
	TopK           int      `json:"top_k"`
	RetrievedCount int      `json:"retrieved_count"`
	RelevantCount  int      `json:"relevant_count"`
	RelevantRanks  []int    `json:"relevant_ranks"`
	TruePositives  int      `json:"true_positives"`
	FalsePositives int      `json:"false_positives"`
	PrecisionAtK   float64  `json:"precision_at_k"`
	RecallAtK      *float64 `json:"recall_at_k"`
	ReciprocalRank float64  `json:"reciprocal_rank"`
}

// RelevanceResult records measured evidence, never an invented quality pass.
type RelevanceResult struct {
	Schema            string           `json:"schema"`
	HarnessVersion    string           `json:"harness_version"`
	StartedAt         time.Time        `json:"started_at"`
	CompletedAt       time.Time        `json:"completed_at"`
	Outcome           string           `json:"outcome"`
	PostgreSQLVersion string           `json:"postgresql_version"`
	Environment       SafeMetadata     `json:"environment"`
	FixtureClock      string           `json:"fixture_clock"`
	RecallAsOf        time.Time        `json:"recall_as_of"`
	CorpusSHA256      string           `json:"corpus_sha256"`
	CorpusMemories    int              `json:"corpus_memories"`
	QueryCount        int              `json:"query_count"`
	Cases             []RelevanceScore `json:"cases"`
}

// RelevanceRecallAsOf is the fixed query clock for indexed fixture timestamps
// starting at 2026-01-01T00:00:00Z, one second apart in corpus order.
func RelevanceRecallAsOf() time.Time {
	return time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
}

// DefaultRelevanceCorpus returns a fresh copy and the digest of the exact
// checked-in bytes. The result validator always uses this frozen corpus.
func DefaultRelevanceCorpus() (RelevanceCorpus, string, error) {
	var corpus RelevanceCorpus
	if err := decodeEvidenceJSON(relevanceCorpusJSON, &corpus); err != nil {
		return RelevanceCorpus{}, "", errors.New("invalid embedded relevance corpus JSON")
	}
	if err := ValidateRelevanceCorpus(corpus); err != nil {
		return RelevanceCorpus{}, "", err
	}
	digest := sha256.Sum256(relevanceCorpusJSON)
	return corpus, hex.EncodeToString(digest[:]), nil
}

// ValidateRelevanceCorpus rejects ambiguous and unbounded adjudication. Empty
// gold sets are explicit empty arrays; missing gold labels are not no-answer
// judgments. Errors never echo source text, labels, or other supplied values.
func ValidateRelevanceCorpus(corpus RelevanceCorpus) error {
	if corpus.Schema != RelevanceCorpusSchemaV1 || len(corpus.Memories) < 1 ||
		len(corpus.Memories) > MaximumRelevanceMemories || len(corpus.Queries) < 1 ||
		len(corpus.Queries) > MaximumRelevanceQueries {
		return errors.New("invalid relevance corpus envelope or bounds")
	}
	labels := make(map[string]bool, len(corpus.Memories))
	contents := make(map[string]bool, len(corpus.Memories))
	for _, memory := range corpus.Memories {
		content := strings.TrimSpace(memory.Content)
		if !safeLabel(memory.Label) || content == "" || len(memory.Content) > 4096 ||
			labels[memory.Label] || contents[content] {
			return errors.New("invalid or duplicate relevance memory")
		}
		labels[memory.Label], contents[content] = true, true
	}
	names := make(map[string]bool, len(corpus.Queries))
	for _, query := range corpus.Queries {
		if !safeLabel(query.Name) || names[query.Name] || strings.TrimSpace(query.Query) == "" ||
			len(query.Query) > 512 || query.TopK < 1 || query.TopK > MaximumRelevanceTopK ||
			query.RelevantLabels == nil || len(query.RelevantLabels) > len(corpus.Memories) ||
			strings.TrimSpace(query.Rationale) == "" || len(query.Rationale) > 2048 {
			return errors.New("invalid or duplicate relevance query")
		}
		names[query.Name] = true
		gold := make(map[string]bool, len(query.RelevantLabels))
		for _, label := range query.RelevantLabels {
			if !labels[label] || gold[label] {
				return errors.New("unknown or duplicate relevance gold label")
			}
			gold[label] = true
		}
	}
	return nil
}

// ScoreRelevance consumes only known, unique corpus labels from the driver.
// Unknown IDs must fail in the driver's ID-to-label mapping, not be dropped.
func ScoreRelevance(corpus RelevanceCorpus, queryName string, returnedLabels []string) (RelevanceScore, error) {
	if err := ValidateRelevanceCorpus(corpus); err != nil {
		return RelevanceScore{}, err
	}
	var query *RelevanceQuery
	for i := range corpus.Queries {
		if corpus.Queries[i].Name == queryName {
			query = &corpus.Queries[i]
			break
		}
	}
	if query == nil || len(returnedLabels) > query.TopK {
		return RelevanceScore{}, errors.New("unknown relevance query or excess returned labels")
	}
	known := make(map[string]bool, len(corpus.Memories))
	for _, memory := range corpus.Memories {
		known[memory.Label] = true
	}
	gold := make(map[string]bool, len(query.RelevantLabels))
	for _, label := range query.RelevantLabels {
		gold[label] = true
	}
	seen := make(map[string]bool, len(returnedLabels))
	ranks := make([]int, 0, len(returnedLabels))
	for i, label := range returnedLabels {
		if !known[label] || seen[label] {
			return RelevanceScore{}, errors.New("unknown or duplicate returned relevance label")
		}
		seen[label] = true
		if gold[label] {
			ranks = append(ranks, i+1)
		}
	}
	return relevanceScoreFromRanks(*query, len(returnedLabels), ranks), nil
}

func relevanceScoreFromRanks(query RelevanceQuery, retrieved int, ranks []int) RelevanceScore {
	out := RelevanceScore{
		Name: query.Name, TopK: query.TopK, RetrievedCount: retrieved,
		RelevantCount: len(query.RelevantLabels), RelevantRanks: ranks,
		TruePositives: len(ranks), FalsePositives: retrieved - len(ranks),
		PrecisionAtK: float64(len(ranks)) / float64(query.TopK),
	}
	if len(query.RelevantLabels) > 0 {
		recall := float64(len(ranks)) / float64(len(query.RelevantLabels))
		out.RecallAtK = &recall
	}
	if len(ranks) > 0 {
		out.ReciprocalRank = 1 / float64(ranks[0])
	}
	return out
}

// ParseRelevanceOptions reads only the evidence path and bounded runner metadata.
func ParseRelevanceOptions(getenv func(string) string) (RelevanceOptions, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	opts := RelevanceOptions{
		ResultsPath:  strings.TrimSpace(getenv(EnvRelevanceResultsPath)),
		Release:      metadataOrDefault(getenv(EnvRelevanceRelease), "dev"),
		Commit:       metadataOrDefault(getenv(EnvRelevanceCommit), "none"),
		Provider:     metadataOrDefault(getenv(EnvRelevanceProvider), "local"),
		HardwareTier: metadataOrDefault(getenv(EnvRelevanceHardwareTier), "unspecified"),
	}
	if opts.ResultsPath == "" {
		opts.ResultsPath = fmt.Sprintf("/tmp/witself-memory-relevance-%d.json", os.Getpid())
	}
	if !safeMetadata(opts.Release) || !safeMetadata(opts.Commit) ||
		!curationLabelMetadata(opts.Provider) || !curationLabelMetadata(opts.HardwareTier) {
		return RelevanceOptions{}, errors.New("invalid relevance evidence metadata")
	}
	return opts, nil
}

// RelevanceEnvironment records safe metadata for the current Go test process.
func RelevanceEnvironment(opts RelevanceOptions) SafeMetadata {
	return SafeMetadata{
		Release: opts.Release, Commit: opts.Commit, Provider: opts.Provider,
		HardwareTier: opts.HardwareTier, GoVersion: runtime.Version(),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, LogicalCPUs: runtime.NumCPU(),
	}
}

var (
	relevancePostgreSQLVersion = regexp.MustCompile(`^[1-9][0-9]?(\.[0-9]{1,2}){0,2}$`)
	relevanceGoVersion         = regexp.MustCompile(`^go1\.[0-9]{1,2}(\.[0-9]{1,2}|(rc|beta)[0-9]{1,2})?$`)
)

func validRelevanceEnvironment(env SafeMetadata) bool {
	return validRecallEnvironment(env) && relevanceGoVersion.MatchString(env.GoVersion) &&
		relevanceListed(env.GOOS, "aix android darwin dragonfly freebsd hurd illumos ios js linux netbsd openbsd plan9 solaris wasip1 windows") &&
		relevanceListed(env.GOARCH, "386 amd64 arm arm64 loong64 mips mipsle mips64 mips64le ppc64 ppc64le riscv64 s390x wasm")
}

func relevanceListed(value, allowed string) bool {
	for _, candidate := range strings.Fields(allowed) {
		if value == candidate {
			return true
		}
	}
	return false
}

// ValidateRelevanceResult binds complete case coverage and order to the frozen
// corpus. Every metric is recomputed from bounded ranks and the frozen gold
// count; internally inconsistent, missing, unknown, or non-finite data fails.
func ValidateRelevanceResult(result RelevanceResult) error {
	if result.Schema != RelevanceResultSchemaV1 || result.HarnessVersion != RelevanceHarnessVersion ||
		result.Outcome != "measured" || result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) ||
		result.FixtureClock != RelevanceFixtureClock || result.RecallAsOf.Format(time.RFC3339Nano) != "2026-01-02T00:00:00Z" ||
		!relevancePostgreSQLVersion.MatchString(result.PostgreSQLVersion) || !validRelevanceEnvironment(result.Environment) {
		return errors.New("invalid relevance result envelope or environment")
	}
	corpus, digest, err := DefaultRelevanceCorpus()
	if err != nil {
		return err
	}
	if result.CorpusSHA256 != digest || result.CorpusMemories != len(corpus.Memories) ||
		result.QueryCount != len(corpus.Queries) || len(result.Cases) != len(corpus.Queries) {
		return errors.New("relevance result must cover the complete frozen corpus")
	}
	for i, item := range result.Cases {
		query := corpus.Queries[i]
		if item.Name != query.Name || item.TopK != query.TopK || item.RelevantCount != len(query.RelevantLabels) ||
			item.RetrievedCount < 0 || item.RetrievedCount > query.TopK || item.RetrievedCount > len(corpus.Memories) ||
			item.RelevantRanks == nil || len(item.RelevantRanks) > len(query.RelevantLabels) {
			return errors.New("invalid relevance case identity or counts")
		}
		previous := 0
		for _, rank := range item.RelevantRanks {
			if rank <= previous || rank > item.RetrievedCount {
				return errors.New("invalid relevance case ranks")
			}
			previous = rank
		}
		want := relevanceScoreFromRanks(query, item.RetrievedCount, item.RelevantRanks)
		if item.TruePositives != want.TruePositives || item.FalsePositives != want.FalsePositives ||
			item.FalsePositives > len(corpus.Memories)-len(query.RelevantLabels) ||
			item.PrecisionAtK != want.PrecisionAtK || item.ReciprocalRank != want.ReciprocalRank ||
			(item.RecallAtK == nil) != (want.RecallAtK == nil) ||
			(item.RecallAtK != nil && *item.RecallAtK != *want.RecallAtK) {
			return errors.New("inconsistent relevance case metrics")
		}
	}
	return nil
}

// RelevanceResultJSONSchema returns a fresh copy of the closed result schema.
func RelevanceResultJSONSchema() []byte {
	return append([]byte(nil), relevanceResultSchemaJSON...)
}

// ValidateRelevanceResultJSON checks closed shape and semantic consistency.
// Strict decoding rejects duplicate keys before ordinary JSON decoding can
// silently overwrite them. Failure messages never repeat supplied values.
func ValidateRelevanceResultJSON(raw []byte) error {
	var result RelevanceResult
	if err := decodeEvidenceJSON(raw, &result); err != nil {
		return errors.New("invalid relevance result JSON")
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return errors.New("invalid relevance result JSON")
	}
	schema, err := resolvedRelevanceResultSchema()
	if err != nil {
		return err
	}
	if err := schema.Validate(instance); err != nil {
		return errors.New("invalid relevance result schema instance")
	}
	return ValidateRelevanceResult(result)
}

// MarshalRelevanceResult validates semantics and schema before encoding evidence.
func MarshalRelevanceResult(result RelevanceResult) ([]byte, error) {
	if err := ValidateRelevanceResult(result); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, errors.New("encode relevance result JSON")
	}
	if err := ValidateRelevanceResultJSON(raw); err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// WriteRelevanceResult publishes only a fully validated document using the
// shared private, atomic evidence writer. Failed validation preserves old data.
func WriteRelevanceResult(path string, result RelevanceResult) ([]byte, error) {
	raw, err := MarshalRelevanceResult(result)
	if err != nil {
		return nil, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("relevance result path is required")
	}
	if err := writePrivateEvidence(path, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

var (
	relevanceResultSchemaOnce     sync.Once
	relevanceResultSchemaResolved *jsonschema.Resolved
	relevanceResultSchemaErr      error
)

func resolvedRelevanceResultSchema() (*jsonschema.Resolved, error) {
	relevanceResultSchemaOnce.Do(func() {
		var schema jsonschema.Schema
		if err := json.Unmarshal(relevanceResultSchemaJSON, &schema); err != nil {
			relevanceResultSchemaErr = errors.New("invalid embedded relevance result schema")
			return
		}
		relevanceResultSchemaResolved, relevanceResultSchemaErr = schema.Resolve(nil)
		if relevanceResultSchemaErr != nil {
			relevanceResultSchemaErr = errors.New("resolve embedded relevance result schema")
		}
	})
	return relevanceResultSchemaResolved, relevanceResultSchemaErr
}
