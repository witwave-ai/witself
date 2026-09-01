package loadquality

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// Recall load-result schemas, deterministic defaults, and hard workload
// bounds. The defaults exercise several realistic tenant cardinalities while
// keeping this opt-in store harness bounded.
const (
	RecallResultSchemaV1 = "witself.memory-recall-load-result.v1"
	RecallHarnessVersion = "1"

	DefaultRecallSeed                    = 20260831
	DefaultRecallCardinalities           = "100,500,2000"
	DefaultRecallQueryIterations         = 10
	DefaultRecallConcurrency             = 4
	DefaultRecallVectorDimensions        = 32
	DefaultRecallCoveragePercentages     = "100,50"
	DefaultRecallPaginationLimit         = 64
	DefaultRecallResultBudget            = 256
	DefaultRecallPaginationRepeats       = 2
	RecallCandidateLimit                 = 256
	RecallHybridQualityCaseCount         = 3
	RecallVectorSafetyCallCount          = 4
	MaximumRecallCardinality             = 10_000
	MaximumRecallQueryIterations         = 1_000
	MaximumRecallConcurrency             = 64
	MaximumRecallVectorDimensions        = 4_096
	MaximumRecallPaginationLimit         = 100
	MaximumRecallResultBudget            = RecallCandidateLimit
	MinimumRecallCardinalityCount        = 2
	MaximumRecallCardinalityCount        = 5
	MinimumRecallCoveragePercentageCount = 2
	MaximumRecallCoveragePercentageCount = 4
	RecallHybridCaseVectorOnly           = "vector_only"
	RecallHybridCaseLexicalOnly          = "lexical_only"
	RecallHybridCaseBothSignals          = "both_signals"
)

// Environment variables accepted by ParseRecallOptions.
const (
	EnvRecallResultsPath         = "WITSELF_MEMORY_RECALL_LOAD_RESULTS"
	EnvRecallSeed                = "WITSELF_MEMORY_RECALL_LOAD_SEED"
	EnvRecallCardinalities       = "WITSELF_MEMORY_RECALL_LOAD_CARDINALITIES"
	EnvRecallQueryIterations     = "WITSELF_MEMORY_RECALL_LOAD_QUERY_ITERATIONS"
	EnvRecallConcurrency         = "WITSELF_MEMORY_RECALL_LOAD_CONCURRENCY"
	EnvRecallVectorDimensions    = "WITSELF_MEMORY_RECALL_LOAD_VECTOR_DIMENSIONS"
	EnvRecallCoveragePercentages = "WITSELF_MEMORY_RECALL_LOAD_VECTOR_COVERAGE_PERCENTAGES"
	EnvRecallPaginationLimit     = "WITSELF_MEMORY_RECALL_LOAD_PAGINATION_LIMIT"
	EnvRecallResultBudget        = "WITSELF_MEMORY_RECALL_LOAD_RESULT_BUDGET"
	EnvRecallRelease             = "WITSELF_MEMORY_RECALL_LOAD_RELEASE"
	EnvRecallCommit              = "WITSELF_MEMORY_RECALL_LOAD_COMMIT"
	EnvRecallProvider            = "WITSELF_MEMORY_RECALL_LOAD_PROVIDER"
	EnvRecallHardwareTier        = "WITSELF_MEMORY_RECALL_LOAD_HARDWARE_TIER"
)

//go:embed testdata/recall-result-schema.v1.json
var recallResultSchemaJSON []byte

// RecallOptions contains only bounded workload controls and safe evidence
// metadata. Database location and credentials are deliberately absent.
type RecallOptions struct {
	ResultsPath         string
	Seed                int64
	Cardinalities       []int
	QueryIterations     int
	Concurrency         int
	VectorDimensions    int
	CoveragePercentages []int
	PaginationLimit     int
	ResultBudget        int
	Release             string
	Commit              string
	Provider            string
	HardwareTier        string
}

// RecallWorkload records the complete bounded fixture and call shape.
type RecallWorkload struct {
	Seed                int64 `json:"seed"`
	SyntheticAccounts   int   `json:"synthetic_accounts"`
	SyntheticAgents     int   `json:"synthetic_agents"`
	Cardinalities       []int `json:"cardinalities"`
	QueryIterations     int   `json:"query_iterations"`
	Concurrency         int   `json:"concurrency"`
	VectorDimensions    int   `json:"vector_dimensions"`
	CoveragePercentages []int `json:"coverage_percentages"`
	PaginationLimit     int   `json:"pagination_limit"`
	ResultBudget        int   `json:"result_budget"`
}

// RecallCardinalityMeasurement retains lexical recall statistics for one
// declared tenant cardinality.
type RecallCardinalityMeasurement struct {
	MemoryCount   int            `json:"memory_count"`
	LexicalRecall OperationStats `json:"lexical_recall"`
}

// RecallVectorCoverageMeasurement retains vector attachment and hybrid recall
// statistics for one declared coverage percentage.
type RecallVectorCoverageMeasurement struct {
	CoveragePercent int            `json:"coverage_percent"`
	VectorAttach    OperationStats `json:"vector_attach"`
	HybridRecall    OperationStats `json:"hybrid_recall"`
}

// RecallMeasurements retains one latency distribution for every requested
// workload. Cardinality and coverage measurements remain independently useful.
type RecallMeasurements struct {
	CardinalityLadder []RecallCardinalityMeasurement    `json:"cardinality_ladder"`
	VectorCoverage    []RecallVectorCoverageMeasurement `json:"vector_coverage"`
	HybridQuality     OperationStats                    `json:"hybrid_quality"`
	VectorSafety      OperationStats                    `json:"vector_safety"`
	Pagination        OperationStats                    `json:"pagination"`
}

// RecallCardinalityLadderOutcome is a value-free roll-up of the inline lexical
// mode and completion assertions made at every configured cardinality.
type RecallCardinalityLadderOutcome struct {
	Tenants        int  `json:"tenants"`
	SeededMemories int  `json:"seeded_memories"`
	RecallCalls    int  `json:"recall_calls"`
	AllLexical     bool `json:"all_lexical"`
	AllComplete    bool `json:"all_complete"`
}

// RecallVectorCoverageCase records aggregate store-reported vector metadata;
// it never retains a profile id, memory id, query, or vector component.
type RecallVectorCoverageCase struct {
	CoveragePercent        int     `json:"coverage_percent"`
	EligibleMemories       int     `json:"eligible_memories"`
	AttachedVectors        int     `json:"attached_vectors"`
	RecallCalls            int     `json:"recall_calls"`
	VectorCandidates       int     `json:"vector_candidates"`
	VectorMatches          int     `json:"vector_matches"`
	ReportedVectorCoverage float64 `json:"reported_vector_coverage"`
	Degraded               bool    `json:"degraded"`
	CandidateLimit         int     `json:"candidate_limit"`
	CandidateTruncated     bool    `json:"candidate_truncated"`
	HybridUsed             bool    `json:"hybrid_used"`
	MetadataStable         bool    `json:"metadata_stable"`
}

// RecallVectorCoverageOutcome contains one full-coverage case followed by one
// or more partial-coverage cases and the profile-list assertion roll-up.
type RecallVectorCoverageOutcome struct {
	Cases             []RecallVectorCoverageCase `json:"cases"`
	AllProfilesListed bool                       `json:"all_profiles_listed"`
}

// RecallHybridRelevanceCase records ranks and which score signals the expected
// hit actually used. Names are fixed labels, never retained query text.
type RecallHybridRelevanceCase struct {
	Name           string `json:"name"`
	Passed         bool   `json:"passed"`
	ObservedRank   int    `json:"observed_rank"`
	MaximumRank    int    `json:"maximum_rank"`
	VectorUsed     bool   `json:"vector_used"`
	LexicalUsed    bool   `json:"lexical_used"`
	SimilarityUsed bool   `json:"similarity_used"`
}

// RecallHybridQualityOutcome is a roll-up of inline rank and score-component
// assertions over the three deliberately labeled relevance cases.
type RecallHybridQualityOutcome struct {
	Cases                   []RecallHybridRelevanceCase `json:"cases"`
	RecallCalls             int                         `json:"recall_calls"`
	ScoreComponentsVerified bool                        `json:"score_components_verified"`
	AllRanksPassed          bool                        `json:"all_ranks_passed"`
}

// RecallVectorSafetyOutcome records the same redaction and owner-boundary
// assertions as the lexical slice, with a query vector supplied to every call.
type RecallVectorSafetyOutcome struct {
	RecallCalls                int  `json:"recall_calls"`
	SensitiveBroadRedacted     bool `json:"sensitive_broad_redacted"`
	SensitiveExactOwnerVisible bool `json:"sensitive_exact_owner_visible"`
	CrossAgentIsolated         bool `json:"cross_agent_isolated"`
	CrossAccountIsolated       bool `json:"cross_account_isolated"`
	AllVectorQueries           bool `json:"all_vector_queries"`
}

// RecallPaginationOutcome records only counts and assertion roll-ups from two
// identical traversals of the largest tenant's bounded hybrid candidate set.
type RecallPaginationOutcome struct {
	RepeatRuns             int     `json:"repeat_runs"`
	PagesPerRun            []int   `json:"pages_per_run"`
	HitsPerRun             []int   `json:"hits_per_run"`
	RecallCalls            int     `json:"recall_calls"`
	ResultBudget           int     `json:"result_budget"`
	AttachedVectors        int     `json:"attached_vectors"`
	VectorCandidates       int     `json:"vector_candidates"`
	VectorMatches          int     `json:"vector_matches"`
	ReportedVectorCoverage float64 `json:"reported_vector_coverage"`
	TenantVectorFraction   float64 `json:"tenant_vector_fraction"`
	CandidateLimit         int     `json:"candidate_limit"`
	CandidateTruncated     bool    `json:"candidate_truncated"`
	PageLimitsHonored      bool    `json:"page_limits_honored"`
	ResultBudgetReached    bool    `json:"result_budget_reached"`
	NoDuplicateIDs         bool    `json:"no_duplicate_ids"`
	OrderingStable         bool    `json:"ordering_stable"`
}

// RecallOutcomes groups correctness evidence for all five requested workloads.
type RecallOutcomes struct {
	CardinalityLadder RecallCardinalityLadderOutcome `json:"cardinality_ladder"`
	VectorCoverage    RecallVectorCoverageOutcome    `json:"vector_coverage"`
	HybridQuality     RecallHybridQualityOutcome     `json:"hybrid_quality"`
	VectorSafety      RecallVectorSafetyOutcome      `json:"vector_safety"`
	Pagination        RecallPaginationOutcome        `json:"pagination"`
}

// RecallResult is a sanitized artifact safe to retain as CI or release
// evidence. PostgreSQLVersion is software metadata, never endpoint identity.
type RecallResult struct {
	Schema            string             `json:"schema"`
	HarnessVersion    string             `json:"harness_version"`
	StartedAt         time.Time          `json:"started_at"`
	CompletedAt       time.Time          `json:"completed_at"`
	Outcome           string             `json:"outcome"`
	PostgreSQLVersion string             `json:"postgresql_version"`
	Environment       SafeMetadata       `json:"environment"`
	Workload          RecallWorkload     `json:"workload"`
	Measurements      RecallMeasurements `json:"measurements"`
	Outcomes          RecallOutcomes     `json:"outcomes"`
}

// RecallResultJSONSchema returns a fresh copy of the checked-in schema.
func RecallResultJSONSchema() []byte {
	return append([]byte(nil), recallResultSchemaJSON...)
}

// ParseRecallOptions reads bounded controls through an injected lookup so unit
// tests never mutate the process environment.
func ParseRecallOptions(getenv func(string) string) (RecallOptions, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	opts := RecallOptions{
		ResultsPath:      strings.TrimSpace(getenv(EnvRecallResultsPath)),
		Seed:             DefaultRecallSeed,
		QueryIterations:  DefaultRecallQueryIterations,
		Concurrency:      DefaultRecallConcurrency,
		VectorDimensions: DefaultRecallVectorDimensions,
		PaginationLimit:  DefaultRecallPaginationLimit,
		ResultBudget:     DefaultRecallResultBudget,
		Release:          metadataOrDefault(getenv(EnvRecallRelease), "dev"),
		Commit:           metadataOrDefault(getenv(EnvRecallCommit), "none"),
		Provider:         metadataOrDefault(getenv(EnvRecallProvider), "local"),
		HardwareTier:     metadataOrDefault(getenv(EnvRecallHardwareTier), "unspecified"),
	}
	if opts.ResultsPath == "" {
		opts.ResultsPath = fmt.Sprintf("/tmp/witself-memory-recall-load-%d.json", os.Getpid())
	}
	var err error
	if opts.Seed, err = parseInt64(getenv(EnvRecallSeed), DefaultRecallSeed, math.MinInt64, math.MaxInt64, EnvRecallSeed); err != nil {
		return RecallOptions{}, err
	}
	if opts.Cardinalities, err = parseRecallCardinalities(getenv(EnvRecallCardinalities)); err != nil {
		return RecallOptions{}, err
	}
	if opts.QueryIterations, err = parseInt(getenv(EnvRecallQueryIterations), DefaultRecallQueryIterations, 1, MaximumRecallQueryIterations, EnvRecallQueryIterations); err != nil {
		return RecallOptions{}, err
	}
	if opts.Concurrency, err = parseInt(getenv(EnvRecallConcurrency), DefaultRecallConcurrency, 2, MaximumRecallConcurrency, EnvRecallConcurrency); err != nil {
		return RecallOptions{}, err
	}
	if opts.VectorDimensions, err = parseInt(getenv(EnvRecallVectorDimensions), DefaultRecallVectorDimensions, 2, MaximumRecallVectorDimensions, EnvRecallVectorDimensions); err != nil {
		return RecallOptions{}, err
	}
	if opts.CoveragePercentages, err = parseRecallCoveragePercentages(getenv(EnvRecallCoveragePercentages)); err != nil {
		return RecallOptions{}, err
	}
	if opts.PaginationLimit, err = parseInt(getenv(EnvRecallPaginationLimit), DefaultRecallPaginationLimit, 1, MaximumRecallPaginationLimit, EnvRecallPaginationLimit); err != nil {
		return RecallOptions{}, err
	}
	if opts.ResultBudget, err = parseInt(getenv(EnvRecallResultBudget), DefaultRecallResultBudget, 2, MaximumRecallResultBudget, EnvRecallResultBudget); err != nil {
		return RecallOptions{}, err
	}
	if opts.ResultBudget <= opts.PaginationLimit || opts.ResultBudget > opts.Cardinalities[len(opts.Cardinalities)-1] {
		return RecallOptions{}, fmt.Errorf("%s must exceed %s and not exceed the largest cardinality", EnvRecallResultBudget, EnvRecallPaginationLimit)
	}
	if opts.QueryIterations < opts.Concurrency {
		return RecallOptions{}, fmt.Errorf("%s must be at least %s so every configured worker receives work", EnvRecallQueryIterations, EnvRecallConcurrency)
	}
	coverageCount, _ := RecallCoverageCount(opts.Cardinalities[0], opts.CoveragePercentages[len(opts.CoveragePercentages)-1])
	if opts.Cardinalities[0] > MaximumRecallResultBudget || coverageCount < 1 ||
		opts.Cardinalities[len(opts.Cardinalities)-1] <= MaximumRecallResultBudget {
		return RecallOptions{}, errors.New("recall cardinalities must keep coverage at or below 256, pagination above 256, and every coverage case non-empty")
	}
	for _, item := range []struct {
		name  string
		value string
	}{{EnvRecallRelease, opts.Release}, {EnvRecallCommit, opts.Commit}} {
		if !safeMetadata(item.value) {
			return RecallOptions{}, fmt.Errorf("%s contains unsafe evidence metadata", item.name)
		}
	}
	for _, item := range []struct {
		name  string
		value string
	}{{EnvRecallProvider, opts.Provider}, {EnvRecallHardwareTier, opts.HardwareTier}} {
		if !curationLabelMetadata(item.value) {
			return RecallOptions{}, fmt.Errorf("%s must be a dotless label (letters, digits, '+', '_', '-')", item.name)
		}
	}
	return opts, nil
}

// RecallCoverageCount returns the deterministic whole-memory floor of a
// configured percentage.
func RecallCoverageCount(memoryCount, percentage int) (int, error) {
	if memoryCount < 1 || memoryCount > MaximumRecallCardinality || percentage < 1 || percentage > 100 {
		return 0, errors.New("recall vector coverage inputs are outside harness bounds")
	}
	return memoryCount * percentage / 100, nil
}

// RecallRatio returns the exact float64 ratio reported by the store contract.
func RecallRatio(numerator, denominator int) float64 {
	if numerator < 0 || denominator <= 0 || numerator > denominator {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

// DeterministicVector expands SHA-256 over unambiguous signed seed, index, and
// block bytes into the requested dimensions and returns an L2-normalized
// client-side synthetic vector. No RNG, model, embedding provider, or backend
// inference participates.
func DeterministicVector(seed, index int64, dimensions int) ([]float64, error) {
	if index < 0 || dimensions < 2 || dimensions > MaximumRecallVectorDimensions {
		return nil, errors.New("deterministic recall vector inputs are outside harness bounds")
	}
	vector := make([]float64, dimensions)
	normSquared := 0.0
	var blockInput [24]byte
	binary.BigEndian.PutUint64(blockInput[0:8], uint64(seed))
	binary.BigEndian.PutUint64(blockInput[8:16], uint64(index))
	for componentIndex := 0; componentIndex < dimensions; componentIndex++ {
		blockIndex := componentIndex / 4
		chunkIndex := componentIndex % 4
		binary.BigEndian.PutUint64(blockInput[16:24], uint64(blockIndex))
		block := sha256.Sum256(blockInput[:])
		// Mapping 53 stable digest bits into (-1,1) avoids integer-to-float
		// precision drift while providing non-zero signed components.
		chunkStart := chunkIndex * 8
		mantissa := binary.BigEndian.Uint64(block[chunkStart:chunkStart+8]) >> 11
		component := (float64(mantissa)+0.5)/float64(uint64(1)<<53)*2 - 1
		vector[componentIndex] = component
		normSquared += component * component
	}
	norm := math.Sqrt(normSquared)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, errors.New("deterministic recall vector has invalid norm")
	}
	for componentIndex := range vector {
		vector[componentIndex] /= norm
	}
	return vector, nil
}

// RecallEnvironment builds the sanitized runner metadata retained in evidence.
func RecallEnvironment(opts RecallOptions) SafeMetadata {
	return SafeMetadata{
		Release: opts.Release, Commit: opts.Commit, Provider: opts.Provider,
		HardwareTier: opts.HardwareTier, GoVersion: runtime.Version(),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, LogicalCPUs: runtime.NumCPU(),
	}
}

// ValidateRecallResult requires a complete passing run and exact agreement
// between declared workload, measurements, counters, and assertion roll-ups.
func ValidateRecallResult(result RecallResult) error {
	if result.Schema != RecallResultSchemaV1 || result.HarnessVersion != RecallHarnessVersion ||
		result.Outcome != "pass" || result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) ||
		strings.TrimSpace(result.PostgreSQLVersion) == "" || len(result.PostgreSQLVersion) > 128 {
		return errors.New("invalid recall load result envelope")
	}
	if !validRecallEnvironment(result.Environment) {
		return errors.New("invalid recall load result environment")
	}
	if err := validateRecallWorkload(result.Workload); err != nil {
		return err
	}
	if err := validateRecallMeasurements(result.Workload, result.Measurements); err != nil {
		return err
	}
	if err := validateRecallOutcomes(result.Workload, result.Measurements, result.Outcomes); err != nil {
		return err
	}
	return nil
}

// MarshalRecallResult performs semantic checks and validates the exact JSON
// instance against the checked-in Draft 2020-12 schema.
func MarshalRecallResult(result RecallResult) ([]byte, error) {
	if err := ValidateRecallResult(result); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := ValidateRecallResultJSON(raw); err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// ValidateRecallResultJSON validates one JSON instance with the checked-in
// Draft 2020-12 result schema.
func ValidateRecallResultJSON(raw []byte) error {
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return fmt.Errorf("decode recall load result JSON: %w", err)
	}
	resolved, err := resolvedRecallResultSchema()
	if err != nil {
		return err
	}
	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("validate recall load result schema: %w", err)
	}
	return nil
}

// WriteRecallResult writes with private permissions and atomically replaces an
// older artifact only after the complete document is synced and closed.
func WriteRecallResult(path string, result RecallResult) ([]byte, error) {
	raw, err := MarshalRecallResult(result)
	if err != nil {
		return nil, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("recall load result path is required")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create recall load result directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".memory-recall-load-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create recall load result: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("protect recall load result: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("write recall load result: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("sync recall load result: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("close recall load result: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return nil, fmt.Errorf("publish recall load result: %w", err)
	}
	return raw, nil
}

func parseRecallCardinalities(value string) ([]int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = DefaultRecallCardinalities
	}
	return parseRecallStrictlyIncreasingList(value, EnvRecallCardinalities,
		MinimumRecallCardinalityCount, MaximumRecallCardinalityCount, 10, MaximumRecallCardinality)
}

func parseRecallCoveragePercentages(value string) ([]int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = DefaultRecallCoveragePercentages
	}
	parts := strings.Split(value, ",")
	if len(parts) < MinimumRecallCoveragePercentageCount || len(parts) > MaximumRecallCoveragePercentageCount {
		return nil, fmt.Errorf("%s must contain %d to %d comma-separated integers", EnvRecallCoveragePercentages, MinimumRecallCoveragePercentageCount, MaximumRecallCoveragePercentageCount)
	}
	out := make([]int, 0, len(parts))
	previous := 101
	for position, part := range parts {
		parsed, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || parsed < 1 || parsed > 100 || parsed >= previous || position == 0 && parsed != 100 {
			return nil, fmt.Errorf("%s must start with 100 and then contain strictly decreasing integers between 1 and 99", EnvRecallCoveragePercentages)
		}
		out = append(out, parsed)
		previous = parsed
	}
	return out, nil
}

func parseRecallStrictlyIncreasingList(value, name string, minimumItems, maximumItems, minimumValue, maximumValue int) ([]int, error) {
	parts := strings.Split(value, ",")
	if len(parts) < minimumItems || len(parts) > maximumItems {
		return nil, fmt.Errorf("%s must contain %d to %d comma-separated integers", name, minimumItems, maximumItems)
	}
	out := make([]int, 0, len(parts))
	previous := minimumValue - 1
	for _, part := range parts {
		parsed, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || parsed < minimumValue || parsed > maximumValue || parsed <= previous {
			return nil, fmt.Errorf("%s must be strictly increasing integers between %d and %d", name, minimumValue, maximumValue)
		}
		out = append(out, parsed)
		previous = parsed
	}
	return out, nil
}

func validRecallEnvironment(environment SafeMetadata) bool {
	return safeMetadata(environment.Release) && safeMetadata(environment.Commit) &&
		curationLabelMetadata(environment.Provider) && curationLabelMetadata(environment.HardwareTier) &&
		len(environment.GoVersion) >= 1 && len(environment.GoVersion) <= 64 &&
		len(environment.GOOS) >= 1 && len(environment.GOOS) <= 32 &&
		len(environment.GOARCH) >= 1 && len(environment.GOARCH) <= 32 && environment.LogicalCPUs >= 1
}

func validateRecallWorkload(workload RecallWorkload) error {
	if workload.SyntheticAccounts != len(workload.Cardinalities) || workload.SyntheticAgents != len(workload.Cardinalities)+1 ||
		!validRecallCardinalities(workload.Cardinalities) ||
		workload.QueryIterations < 1 || workload.QueryIterations > MaximumRecallQueryIterations ||
		workload.Concurrency < 2 || workload.Concurrency > MaximumRecallConcurrency ||
		workload.QueryIterations < workload.Concurrency ||
		workload.VectorDimensions < 2 || workload.VectorDimensions > MaximumRecallVectorDimensions ||
		!validRecallCoveragePercentages(workload.CoveragePercentages) ||
		workload.PaginationLimit < 1 || workload.PaginationLimit > MaximumRecallPaginationLimit ||
		workload.ResultBudget < 2 || workload.ResultBudget > MaximumRecallResultBudget ||
		workload.ResultBudget <= workload.PaginationLimit ||
		workload.ResultBudget > workload.Cardinalities[len(workload.Cardinalities)-1] ||
		workload.Cardinalities[0] > MaximumRecallResultBudget {
		return errors.New("invalid recall load result workload")
	}
	attached, _ := RecallCoverageCount(
		workload.Cardinalities[0], workload.CoveragePercentages[len(workload.CoveragePercentages)-1],
	)
	if attached < 1 || workload.Cardinalities[len(workload.Cardinalities)-1] <= MaximumRecallResultBudget {
		return errors.New("invalid recall load result workload cardinality relationship")
	}
	return nil
}

func validRecallCardinalities(values []int) bool {
	if len(values) < MinimumRecallCardinalityCount || len(values) > MaximumRecallCardinalityCount {
		return false
	}
	previous := 9
	for _, value := range values {
		if value < 10 || value > MaximumRecallCardinality || value <= previous {
			return false
		}
		previous = value
	}
	return true
}

func validRecallCoveragePercentages(values []int) bool {
	if len(values) < MinimumRecallCoveragePercentageCount || len(values) > MaximumRecallCoveragePercentageCount || values[0] != 100 {
		return false
	}
	previous := 101
	for _, value := range values {
		if value < 1 || value > 100 || value >= previous {
			return false
		}
		previous = value
	}
	return true
}

func validateRecallMeasurements(workload RecallWorkload, measurements RecallMeasurements) error {
	if len(measurements.CardinalityLadder) != len(workload.Cardinalities) {
		return errors.New("invalid cardinality ladder measurements")
	}
	for index, measurement := range measurements.CardinalityLadder {
		if measurement.MemoryCount != workload.Cardinalities[index] ||
			measurement.LexicalRecall.Count != workload.QueryIterations ||
			!validOperationStats(measurement.LexicalRecall) {
			return errors.New("invalid cardinality ladder measurements")
		}
	}
	coverageCardinality := workload.Cardinalities[0]
	if len(measurements.VectorCoverage) != len(workload.CoveragePercentages) {
		return errors.New("invalid vector coverage measurements")
	}
	for index, measurement := range measurements.VectorCoverage {
		attached, _ := RecallCoverageCount(coverageCardinality, workload.CoveragePercentages[index])
		if measurement.CoveragePercent != workload.CoveragePercentages[index] ||
			measurement.VectorAttach.Count != attached || measurement.HybridRecall.Count != workload.QueryIterations ||
			!validOperationStats(measurement.VectorAttach) || !validOperationStats(measurement.HybridRecall) {
			return errors.New("invalid vector coverage measurements")
		}
	}
	if measurements.HybridQuality.Count != workload.QueryIterations*RecallHybridQualityCaseCount ||
		measurements.VectorSafety.Count != RecallVectorSafetyCallCount ||
		!validOperationStats(measurements.HybridQuality) ||
		!validOperationStats(measurements.VectorSafety) ||
		!validOperationStats(measurements.Pagination) {
		return errors.New("invalid recall workload measurements")
	}
	return nil
}

func validateRecallOutcomes(workload RecallWorkload, measurements RecallMeasurements, outcomes RecallOutcomes) error {
	wantSeeded := 0
	for _, cardinality := range workload.Cardinalities {
		wantSeeded += cardinality
	}
	ladder := outcomes.CardinalityLadder
	if ladder.Tenants != len(workload.Cardinalities) || ladder.SeededMemories != wantSeeded ||
		ladder.RecallCalls != workload.QueryIterations*ladder.Tenants || !ladder.AllLexical || !ladder.AllComplete {
		return errors.New("invalid cardinality ladder outcomes")
	}

	coverageCardinality := workload.Cardinalities[0]
	largest := workload.Cardinalities[len(workload.Cardinalities)-1]
	coverage := outcomes.VectorCoverage
	if len(coverage.Cases) != len(workload.CoveragePercentages) || !coverage.AllProfilesListed {
		return errors.New("invalid vector coverage outcomes")
	}
	for index, item := range coverage.Cases {
		percentage := workload.CoveragePercentages[index]
		attached, _ := RecallCoverageCount(coverageCardinality, percentage)
		if item.CoveragePercent != percentage || item.EligibleMemories != coverageCardinality ||
			item.AttachedVectors != attached || item.RecallCalls != workload.QueryIterations ||
			item.RecallCalls != measurements.VectorCoverage[index].HybridRecall.Count ||
			item.AttachedVectors != measurements.VectorCoverage[index].VectorAttach.Count ||
			item.VectorCandidates != item.EligibleMemories ||
			item.CandidateLimit != RecallCandidateLimit ||
			item.VectorCandidates > item.CandidateLimit || item.VectorMatches != item.AttachedVectors ||
			item.ReportedVectorCoverage != RecallRatio(item.VectorMatches, item.VectorCandidates) ||
			item.CandidateTruncated ||
			!item.HybridUsed || !item.MetadataStable {
			return errors.New("invalid vector coverage outcomes")
		}
		if percentage == 100 {
			if item.VectorMatches != item.VectorCandidates || item.ReportedVectorCoverage != 1 || item.Degraded {
				return errors.New("invalid full vector coverage outcomes")
			}
		} else if item.VectorMatches >= item.VectorCandidates || item.ReportedVectorCoverage <= 0 ||
			item.ReportedVectorCoverage >= 1 || !item.Degraded {
			return errors.New("invalid partial vector coverage outcomes")
		}
	}

	quality := outcomes.HybridQuality
	if len(quality.Cases) != RecallHybridQualityCaseCount ||
		quality.RecallCalls != workload.QueryIterations*RecallHybridQualityCaseCount ||
		quality.RecallCalls != measurements.HybridQuality.Count ||
		!quality.ScoreComponentsVerified || !quality.AllRanksPassed {
		return errors.New("invalid hybrid quality outcomes")
	}
	wantNames := [...]string{RecallHybridCaseVectorOnly, RecallHybridCaseLexicalOnly, RecallHybridCaseBothSignals}
	for index, item := range quality.Cases {
		if item.Name != wantNames[index] || !item.Passed || item.ObservedRank != 1 || item.MaximumRank != 1 {
			return errors.New("invalid hybrid quality relevance case")
		}
		switch item.Name {
		case RecallHybridCaseVectorOnly:
			if !item.VectorUsed || item.LexicalUsed || !item.SimilarityUsed {
				return errors.New("invalid vector-only relevance signals")
			}
		case RecallHybridCaseLexicalOnly:
			if item.VectorUsed || !item.LexicalUsed || item.SimilarityUsed {
				return errors.New("invalid lexical-only relevance signals")
			}
		case RecallHybridCaseBothSignals:
			if !item.VectorUsed || !item.LexicalUsed || !item.SimilarityUsed {
				return errors.New("invalid both-signals relevance signals")
			}
		}
	}

	safety := outcomes.VectorSafety
	if safety.RecallCalls != RecallVectorSafetyCallCount || safety.RecallCalls != measurements.VectorSafety.Count ||
		!safety.SensitiveBroadRedacted || !safety.SensitiveExactOwnerVisible ||
		!safety.CrossAgentIsolated || !safety.CrossAccountIsolated || !safety.AllVectorQueries {
		return errors.New("invalid vector safety outcomes")
	}

	pagination := outcomes.Pagination
	wantPages := (workload.ResultBudget + workload.PaginationLimit - 1) / workload.PaginationLimit
	wantCalls := DefaultRecallPaginationRepeats * wantPages
	if pagination.RepeatRuns != DefaultRecallPaginationRepeats ||
		len(pagination.PagesPerRun) != pagination.RepeatRuns || len(pagination.HitsPerRun) != pagination.RepeatRuns ||
		pagination.RecallCalls != wantCalls ||
		pagination.RecallCalls != measurements.Pagination.Count || pagination.ResultBudget != workload.ResultBudget ||
		pagination.CandidateLimit != RecallCandidateLimit ||
		pagination.AttachedVectors != RecallCandidateLimit ||
		pagination.VectorCandidates != RecallCandidateLimit || pagination.VectorMatches != RecallCandidateLimit ||
		pagination.ReportedVectorCoverage != 1 ||
		pagination.TenantVectorFraction <= 0 ||
		pagination.TenantVectorFraction != RecallRatio(RecallCandidateLimit, largest) ||
		!pagination.CandidateTruncated ||
		!pagination.PageLimitsHonored || !pagination.ResultBudgetReached ||
		!pagination.NoDuplicateIDs || !pagination.OrderingStable {
		return errors.New("invalid pagination outcomes")
	}
	for runIndex := 0; runIndex < pagination.RepeatRuns; runIndex++ {
		if pagination.PagesPerRun[runIndex] != wantPages || pagination.HitsPerRun[runIndex] != workload.ResultBudget {
			return errors.New("invalid pagination per-run counters")
		}
	}
	return nil
}

var (
	recallResultSchemaOnce     sync.Once
	recallResultSchemaResolved *jsonschema.Resolved
	recallResultSchemaErr      error
)

func resolvedRecallResultSchema() (*jsonschema.Resolved, error) {
	recallResultSchemaOnce.Do(func() {
		var schema jsonschema.Schema
		if err := json.Unmarshal(recallResultSchemaJSON, &schema); err != nil {
			recallResultSchemaErr = fmt.Errorf("decode embedded recall result schema: %w", err)
			return
		}
		recallResultSchemaResolved, recallResultSchemaErr = schema.Resolve(nil)
		if recallResultSchemaErr != nil {
			recallResultSchemaErr = fmt.Errorf("resolve embedded recall result schema: %w", recallResultSchemaErr)
		}
	})
	return recallResultSchemaResolved, recallResultSchemaErr
}
