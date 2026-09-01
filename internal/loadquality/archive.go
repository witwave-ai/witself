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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// Archive load-result schemas, deterministic defaults, and hard workload
// bounds. Each named workload in the store driver receives this deadline. It
// is an execution guard, not a latency assertion or SLO.
const (
	ArchiveResultSchemaV1 = "witself.memory-archive-load-result.v1"
	ArchiveHarnessVersion = "1"

	ArchiveWorkloadDeadline = 2 * time.Minute

	DefaultArchiveSeed                               = 20260831
	DefaultArchiveCardinalities                      = "100,500,2000"
	DefaultArchiveVersionsPerMemory                  = 2
	DefaultArchiveEvidencePerMemory                  = 2
	DefaultArchiveRelationsPerMemory                 = 1
	DefaultArchiveTagsPerVersion                     = 3
	DefaultArchiveTranscriptSharePercent             = 25
	DefaultArchiveTranscriptEntriesPerSelectedMemory = 2
	DefaultArchiveVectorDimensions                   = 32

	MinimumArchiveCardinalityCount                   = 2
	MaximumArchiveCardinalityCount                   = 5
	MinimumArchiveCardinality                        = 10
	MaximumArchiveCardinality                        = 10_000
	MaximumArchiveVersionsPerMemory                  = 8
	MaximumArchiveEvidencePerMemory                  = 8
	MaximumArchiveRelationsPerMemory                 = 4
	MaximumArchiveTagsPerVersion                     = 16
	MaximumArchiveTranscriptEntriesPerSelectedMemory = 8
	MaximumArchiveVectorDimensions                   = 4_096

	ArchiveLexicalQueryCount  = 2
	ArchiveHybridQueryCount   = 2
	ArchiveSafetyCallCount    = 4
	ArchiveFormatVersion      = 1
	ArchivePortableTableCount = 74

	ArchiveManifestPurposeSelf  = "self"
	ArchiveManifestStatus       = "suspended"
	ArchiveScoreComparisonExact = "exact"
	ArchiveRecallModeLexical    = "lexical"
	ArchiveRecallModeHybrid     = "hybrid"

	ArchiveRecallCaseLexicalPrimary   = "lexical_primary"
	ArchiveRecallCaseLexicalSecondary = "lexical_secondary"
	ArchiveRecallCaseHybridPrimary    = "hybrid_primary"
	ArchiveRecallCaseHybridSecondary  = "hybrid_secondary"
)

// Environment variables accepted by ParseArchiveOptions.
const (
	EnvArchiveResultsPath                        = "WITSELF_MEMORY_ARCHIVE_LOAD_RESULTS"
	EnvArchiveSeed                               = "WITSELF_MEMORY_ARCHIVE_LOAD_SEED"
	EnvArchiveCardinalities                      = "WITSELF_MEMORY_ARCHIVE_LOAD_CARDINALITIES"
	EnvArchiveVersionsPerMemory                  = "WITSELF_MEMORY_ARCHIVE_LOAD_VERSIONS_PER_MEMORY"
	EnvArchiveEvidencePerMemory                  = "WITSELF_MEMORY_ARCHIVE_LOAD_EVIDENCE_PER_MEMORY"
	EnvArchiveRelationsPerMemory                 = "WITSELF_MEMORY_ARCHIVE_LOAD_RELATIONS_PER_MEMORY"
	EnvArchiveTagsPerVersion                     = "WITSELF_MEMORY_ARCHIVE_LOAD_TAGS_PER_VERSION"
	EnvArchiveTranscriptSharePercent             = "WITSELF_MEMORY_ARCHIVE_LOAD_TRANSCRIPT_SHARE_PERCENT"
	EnvArchiveTranscriptEntriesPerSelectedMemory = "WITSELF_MEMORY_ARCHIVE_LOAD_TRANSCRIPT_ENTRIES_PER_SELECTED_MEMORY"
	EnvArchiveVectorDimensions                   = "WITSELF_MEMORY_ARCHIVE_LOAD_VECTOR_DIMENSIONS"
	EnvArchiveRelease                            = "WITSELF_MEMORY_ARCHIVE_LOAD_RELEASE"
	EnvArchiveCommit                             = "WITSELF_MEMORY_ARCHIVE_LOAD_COMMIT"
	EnvArchiveProvider                           = "WITSELF_MEMORY_ARCHIVE_LOAD_PROVIDER"
	EnvArchiveHardwareTier                       = "WITSELF_MEMORY_ARCHIVE_LOAD_HARDWARE_TIER"
)

//go:embed testdata/archive-result-schema.v1.json
var archiveResultSchemaJSON []byte

// archivePortableTableNamesV1 is the complete canonical archive registry at
// result-contract version 1, sorted for stable value-free evidence. A registry
// change must update this contract rather than allowing a partial table list to
// validate itself merely by lowering its reported manifest count.
var archivePortableTableNamesV1 = [...]string{
	"account_events",
	"accounts",
	"agent_activity",
	"agent_avatar_activations",
	"agent_avatar_profiles",
	"agent_avatar_rejections",
	"agent_avatar_resets",
	"agent_avatar_versions",
	"agent_dashboard_preferences",
	"agent_email_address_domains",
	"agent_email_addresses",
	"agent_email_custom_domain_routes",
	"agent_email_deliveries",
	"agent_email_mailboxes",
	"agent_email_messages",
	"agent_email_outbound_messages",
	"agent_email_outbound_provider_events",
	"agent_email_outbound_recipient_suppressions",
	"agent_email_realm_aliases",
	"agent_email_realm_receive_controls",
	"agent_email_realm_send_controls",
	"agent_email_retry_canary_arms",
	"agent_email_send_controls",
	"agent_message_deliveries",
	"agent_message_request_candidates",
	"agent_message_request_claims",
	"agent_message_request_selections",
	"agent_message_requests",
	"agent_messages",
	"agent_vault_key_enrollments",
	"agent_vault_key_rotation_items",
	"agent_vault_key_rotations",
	"agent_vault_keys",
	"agents",
	"avatar_mutation_receipts",
	"avatar_style_pack_versions",
	"avatar_style_packs",
	"avatar_style_rollout_jobs",
	"fact_assertions",
	"fact_candidates",
	"fact_mutation_tombstones",
	"fact_subjects",
	"facts",
	"memories",
	"memory_change_clocks",
	"memory_curation_actions",
	"memory_curation_cursors",
	"memory_curation_lanes",
	"memory_curation_mutations",
	"memory_curation_requests",
	"memory_curation_run_inputs",
	"memory_curation_runs",
	"memory_deleted_references",
	"memory_evidence",
	"memory_relations",
	"memory_vector_profiles",
	"memory_vectors",
	"memory_versions",
	"operators",
	"realm_avatar_styles",
	"realms",
	"secret_deks",
	"secret_fields",
	"secret_mutation_receipts",
	"secrets",
	"support_ticket_messages",
	"support_tickets",
	"tokens",
	"transcript_conversations",
	"transcript_entries",
	"usage_events",
	"usage_rollups",
	"vault_key_enrollment_receipts",
	"vault_key_rotation_receipts",
}

// ArchiveOptions contains only bounded workload controls and safe evidence
// metadata. Database location and credentials are deliberately absent.
type ArchiveOptions struct {
	ResultsPath                        string
	Seed                               int64
	Cardinalities                      []int
	VersionsPerMemory                  int
	EvidencePerMemory                  int
	RelationsPerMemory                 int
	TagsPerVersion                     int
	TranscriptSharePercent             int
	TranscriptEntriesPerSelectedMemory int
	VectorDimensions                   int
	Release                            string
	Commit                             string
	Provider                           string
	HardwareTier                       string
}

// ArchiveWorkload records the complete bounded fixture and call shape.
type ArchiveWorkload struct {
	Seed                               int64 `json:"seed"`
	SyntheticAccounts                  int   `json:"synthetic_accounts"`
	SyntheticAgents                    int   `json:"synthetic_agents"`
	Cardinalities                      []int `json:"cardinalities"`
	VersionsPerMemory                  int   `json:"versions_per_memory"`
	EvidencePerMemory                  int   `json:"evidence_per_memory"`
	RelationsPerMemory                 int   `json:"relations_per_memory"`
	TagsPerVersion                     int   `json:"tags_per_version"`
	TranscriptSharePercent             int   `json:"transcript_share_percent"`
	TranscriptEntriesPerSelectedMemory int   `json:"transcript_entries_per_selected_memory"`
	VectorDimensions                   int   `json:"vector_dimensions"`
	LexicalQueries                     int   `json:"lexical_queries"`
	HybridQueries                      int   `json:"hybrid_queries"`
	SafetyCalls                        int   `json:"safety_calls"`
}

// ArchiveCardinalityMeasurement retains phase and recall measurements for one
// declared account memory cardinality. Phase OperationStats each describe one
// complete call; explicit row and byte rates carry transfer throughput without
// manufacturing per-row latency samples.
type ArchiveCardinalityMeasurement struct {
	MemoryCount          int            `json:"memory_count"`
	Seed                 OperationStats `json:"seed"`
	Export               OperationStats `json:"export"`
	Verify               OperationStats `json:"verify"`
	Import               OperationStats `json:"import"`
	LexicalRecallBefore  OperationStats `json:"lexical_recall_before"`
	LexicalRecallAfter   OperationStats `json:"lexical_recall_after"`
	HybridRecallBefore   OperationStats `json:"hybrid_recall_before"`
	HybridRecallAfter    OperationStats `json:"hybrid_recall_after"`
	PostImportSafety     OperationStats `json:"post_import_safety"`
	ExportRowsPerSecond  float64        `json:"export_rows_per_second"`
	ExportBytesPerSecond float64        `json:"export_bytes_per_second"`
	ImportRowsPerSecond  float64        `json:"import_rows_per_second"`
	ImportBytesPerSecond float64        `json:"import_bytes_per_second"`
}

// ArchiveMeasurements keeps each cardinality independently comparable.
type ArchiveMeasurements struct {
	CardinalityLadder []ArchiveCardinalityMeasurement `json:"cardinality_ladder"`
}

// ArchiveTableRows contains registry-wide row counts without retaining row
// values or durable record identifiers.
type ArchiveTableRows struct {
	Name     string `json:"name"`
	Exported int    `json:"exported"`
	Verified int    `json:"verified"`
	Imported int    `json:"imported"`
}

// ArchiveFocalCounts makes the issue's high-cardinality fixture dimensions
// explicit. TagAssignments is an inline-asserted JSON-array assignment count,
// not a PostgreSQL table-row count.
type ArchiveFocalCounts struct {
	Memories                int `json:"memories"`
	MemoryVersions          int `json:"memory_versions"`
	MemoryEvidence          int `json:"memory_evidence"`
	MemoryRelations         int `json:"memory_relations"`
	TranscriptConversations int `json:"transcript_conversations"`
	TranscriptEntries       int `json:"transcript_entries"`
	MemoryVectorProfiles    int `json:"memory_vector_profiles"`
	MemoryVectors           int `json:"memory_vectors"`
	TagAssignments          int `json:"tag_assignments"`
}

// ArchiveRecallEquivalenceCase retains only a safe case label, mode, counts,
// and roll-ups of inline comparisons. Ranked ids and scores are never retained.
type ArchiveRecallEquivalenceCase struct {
	Name                 string `json:"name"`
	Mode                 string `json:"mode"`
	BeforeHits           int    `json:"before_hits"`
	AfterHits            int    `json:"after_hits"`
	RankedIDsIdentical   bool   `json:"ranked_ids_identical"`
	ScoreComponentsExact bool   `json:"score_components_exact"`
	MetadataExact        bool   `json:"metadata_exact"`
}

// ArchiveRecallEquivalenceOutcome is the retrieval-projection proof. The
// driver compares identical pinned-snapshot queries before export and after
// import; generated FTS state is never exported or rebuilt by a separate call.
type ArchiveRecallEquivalenceOutcome struct {
	Cases                     []ArchiveRecallEquivalenceCase `json:"cases"`
	BeforeCalls               int                            `json:"before_calls"`
	AfterCalls                int                            `json:"after_calls"`
	ScoreComparison           string                         `json:"score_comparison"`
	ScoreTolerance            float64                        `json:"score_tolerance"`
	AllRankingsIdentical      bool                           `json:"all_rankings_identical"`
	AllScoreComponentsExact   bool                           `json:"all_score_components_exact"`
	AllMetadataExact          bool                           `json:"all_metadata_exact"`
	RetrievalProjectionProved bool                           `json:"retrieval_projection_proved"`
}

// ArchiveSafetyOutcome records post-import value-redaction and isolation
// assertions without retaining the sensitive fixture or any owner id.
type ArchiveSafetyOutcome struct {
	RecallCalls                int  `json:"recall_calls"`
	SensitiveBroadRedacted     bool `json:"sensitive_broad_redacted"`
	SensitiveExactOwnerVisible bool `json:"sensitive_exact_owner_visible"`
	CrossAgentIsolated         bool `json:"cross_agent_isolated"`
	CrossAccountIsolated       bool `json:"cross_account_isolated"`
}

// ArchiveCardinalityOutcome records one complete export, verification,
// delete/import, recall-equivalence, and safety cycle.
type ArchiveCardinalityOutcome struct {
	MemoryCount                int                             `json:"memory_count"`
	ManifestFormatVersion      int                             `json:"manifest_format_version"`
	ManifestSchemaVersion      int                             `json:"manifest_schema_version"`
	ManifestPurpose            string                          `json:"manifest_purpose"`
	ManifestStatus             string                          `json:"manifest_status"`
	ArchiveBytes               int64                           `json:"archive_bytes"`
	ChunkBytes                 int64                           `json:"chunk_bytes"`
	ManifestTables             int                             `json:"manifest_tables"`
	NonEmptyTables             int                             `json:"non_empty_tables"`
	ChunkCount                 int                             `json:"chunk_count"`
	ExportedRows               int                             `json:"exported_rows"`
	VerifiedRows               int                             `json:"verified_rows"`
	ImportedRows               int                             `json:"imported_rows"`
	TableRows                  []ArchiveTableRows              `json:"table_rows"`
	FocalCounts                ArchiveFocalCounts              `json:"focal_counts"`
	RecallEquivalence          ArchiveRecallEquivalenceOutcome `json:"recall_equivalence"`
	Safety                     ArchiveSafetyOutcome            `json:"safety"`
	ChecksumsRead              bool                            `json:"checksums_read"`
	AllChunksVerified          bool                            `json:"all_chunks_verified"`
	AllTablesVerified          bool                            `json:"all_tables_verified"`
	AccountRemovedBeforeImport bool                            `json:"account_removed_before_import"`
	SameStoreRoundTrip         bool                            `json:"same_store_round_trip"`
	ExactTableRowCounts        bool                            `json:"exact_table_row_counts"`
	ExactFixtureCounts         bool                            `json:"exact_fixture_counts"`
}

// ArchiveVectorPortabilityOutcome pins the current canonical archive registry:
// vector profiles and vectors are portable, so hybrid equivalence is required.
type ArchiveVectorPortabilityOutcome struct {
	VectorProfilesPortable     bool `json:"vector_profiles_portable"`
	MemoryVectorsPortable      bool `json:"memory_vectors_portable"`
	HybridEquivalenceExercised bool `json:"hybrid_equivalence_exercised"`
	VectorPortabilityLimited   bool `json:"vector_portability_limited"`
	VectorProfilesRoundTripped bool `json:"vector_profiles_round_tripped"`
	MemoryVectorsRoundTripped  bool `json:"memory_vectors_round_tripped"`
}

// ArchiveOutcomes groups complete per-cardinality correctness evidence and
// the registry-wide vector-portability premise.
type ArchiveOutcomes struct {
	CardinalityLadder        []ArchiveCardinalityOutcome     `json:"cardinality_ladder"`
	VectorPortability        ArchiveVectorPortabilityOutcome `json:"vector_portability"`
	AllCardinalitiesComplete bool                            `json:"all_cardinalities_complete"`
}

// ArchiveResult is a value-free artifact safe to retain as CI or release
// evidence. PostgreSQLVersion is software metadata, never endpoint identity.
type ArchiveResult struct {
	Schema            string              `json:"schema"`
	HarnessVersion    string              `json:"harness_version"`
	StartedAt         time.Time           `json:"started_at"`
	CompletedAt       time.Time           `json:"completed_at"`
	Outcome           string              `json:"outcome"`
	PostgreSQLVersion string              `json:"postgresql_version"`
	Environment       SafeMetadata        `json:"environment"`
	Workload          ArchiveWorkload     `json:"workload"`
	Measurements      ArchiveMeasurements `json:"measurements"`
	Outcomes          ArchiveOutcomes     `json:"outcomes"`
}

// ArchiveResultJSONSchema returns a fresh copy of the checked-in schema.
func ArchiveResultJSONSchema() []byte {
	return append([]byte(nil), archiveResultSchemaJSON...)
}

// ArchivePortableTableNames returns a fresh copy of the result-contract v1
// canonical archive registry, sorted lexicographically.
func ArchivePortableTableNames() []string {
	return append([]string(nil), archivePortableTableNamesV1[:]...)
}

// ParseArchiveOptions reads bounded controls through an injected lookup so
// unit tests never mutate the process environment.
func ParseArchiveOptions(getenv func(string) string) (ArchiveOptions, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	opts := ArchiveOptions{
		ResultsPath:                        strings.TrimSpace(getenv(EnvArchiveResultsPath)),
		Seed:                               DefaultArchiveSeed,
		VersionsPerMemory:                  DefaultArchiveVersionsPerMemory,
		EvidencePerMemory:                  DefaultArchiveEvidencePerMemory,
		RelationsPerMemory:                 DefaultArchiveRelationsPerMemory,
		TagsPerVersion:                     DefaultArchiveTagsPerVersion,
		TranscriptSharePercent:             DefaultArchiveTranscriptSharePercent,
		TranscriptEntriesPerSelectedMemory: DefaultArchiveTranscriptEntriesPerSelectedMemory,
		VectorDimensions:                   DefaultArchiveVectorDimensions,
		Release:                            metadataOrDefault(getenv(EnvArchiveRelease), "dev"),
		Commit:                             metadataOrDefault(getenv(EnvArchiveCommit), "none"),
		Provider:                           metadataOrDefault(getenv(EnvArchiveProvider), "local"),
		HardwareTier:                       metadataOrDefault(getenv(EnvArchiveHardwareTier), "unspecified"),
	}
	if opts.ResultsPath == "" {
		opts.ResultsPath = fmt.Sprintf("/tmp/witself-memory-archive-load-%d.json", os.Getpid())
	}
	var err error
	if opts.Seed, err = parseInt64(getenv(EnvArchiveSeed), DefaultArchiveSeed, math.MinInt64, math.MaxInt64, EnvArchiveSeed); err != nil {
		return ArchiveOptions{}, err
	}
	if opts.Cardinalities, err = parseArchiveCardinalities(getenv(EnvArchiveCardinalities)); err != nil {
		return ArchiveOptions{}, err
	}
	if opts.VersionsPerMemory, err = parseInt(getenv(EnvArchiveVersionsPerMemory), DefaultArchiveVersionsPerMemory, 2, MaximumArchiveVersionsPerMemory, EnvArchiveVersionsPerMemory); err != nil {
		return ArchiveOptions{}, err
	}
	if opts.EvidencePerMemory, err = parseInt(getenv(EnvArchiveEvidencePerMemory), DefaultArchiveEvidencePerMemory, 1, MaximumArchiveEvidencePerMemory, EnvArchiveEvidencePerMemory); err != nil {
		return ArchiveOptions{}, err
	}
	if opts.RelationsPerMemory, err = parseInt(getenv(EnvArchiveRelationsPerMemory), DefaultArchiveRelationsPerMemory, 1, MaximumArchiveRelationsPerMemory, EnvArchiveRelationsPerMemory); err != nil {
		return ArchiveOptions{}, err
	}
	if opts.TagsPerVersion, err = parseInt(getenv(EnvArchiveTagsPerVersion), DefaultArchiveTagsPerVersion, 1, MaximumArchiveTagsPerVersion, EnvArchiveTagsPerVersion); err != nil {
		return ArchiveOptions{}, err
	}
	if opts.TranscriptSharePercent, err = parseInt(getenv(EnvArchiveTranscriptSharePercent), DefaultArchiveTranscriptSharePercent, 1, 100, EnvArchiveTranscriptSharePercent); err != nil {
		return ArchiveOptions{}, err
	}
	if opts.TranscriptEntriesPerSelectedMemory, err = parseInt(getenv(EnvArchiveTranscriptEntriesPerSelectedMemory), DefaultArchiveTranscriptEntriesPerSelectedMemory, 1, MaximumArchiveTranscriptEntriesPerSelectedMemory, EnvArchiveTranscriptEntriesPerSelectedMemory); err != nil {
		return ArchiveOptions{}, err
	}
	if opts.VectorDimensions, err = parseInt(getenv(EnvArchiveVectorDimensions), DefaultArchiveVectorDimensions, 2, MaximumArchiveVectorDimensions, EnvArchiveVectorDimensions); err != nil {
		return ArchiveOptions{}, err
	}
	for _, cardinality := range opts.Cardinalities {
		if _, err := ArchiveTranscriptSelectedCount(cardinality, opts.TranscriptSharePercent); err != nil {
			return ArchiveOptions{}, err
		}
	}
	for _, item := range []struct {
		name  string
		value string
	}{{EnvArchiveRelease, opts.Release}, {EnvArchiveCommit, opts.Commit}} {
		if !safeMetadata(item.value) {
			return ArchiveOptions{}, fmt.Errorf("%s contains unsafe evidence metadata", item.name)
		}
	}
	for _, item := range []struct {
		name  string
		value string
	}{{EnvArchiveProvider, opts.Provider}, {EnvArchiveHardwareTier, opts.HardwareTier}} {
		if !curationLabelMetadata(item.value) {
			return ArchiveOptions{}, fmt.Errorf("%s must be a dotless label (letters, digits, '+', '_', '-')", item.name)
		}
	}
	return opts, nil
}

// ArchiveTranscriptSelectedCount returns the deterministic whole-memory floor
// receiving transcript-backed fixture data.
func ArchiveTranscriptSelectedCount(memoryCount, sharePercent int) (int, error) {
	if memoryCount < MinimumArchiveCardinality || memoryCount > MaximumArchiveCardinality ||
		sharePercent < 1 || sharePercent > 100 {
		return 0, errors.New("archive transcript selection inputs are outside harness bounds")
	}
	selected := memoryCount * sharePercent / 100
	if selected < 1 {
		return 0, errors.New("archive transcript share must select at least one memory")
	}
	return selected, nil
}

// ArchiveFocalCountsFor returns the exact focal fixture-count formulas for one
// cardinality and a validated workload shape.
func ArchiveFocalCountsFor(memoryCount int, workload ArchiveWorkload) (ArchiveFocalCounts, error) {
	if workload.VersionsPerMemory < 2 || workload.VersionsPerMemory > MaximumArchiveVersionsPerMemory ||
		workload.EvidencePerMemory < 1 || workload.EvidencePerMemory > MaximumArchiveEvidencePerMemory ||
		workload.RelationsPerMemory < 1 || workload.RelationsPerMemory > MaximumArchiveRelationsPerMemory ||
		workload.TagsPerVersion < 1 || workload.TagsPerVersion > MaximumArchiveTagsPerVersion ||
		workload.TranscriptEntriesPerSelectedMemory < 1 || workload.TranscriptEntriesPerSelectedMemory > MaximumArchiveTranscriptEntriesPerSelectedMemory {
		return ArchiveFocalCounts{}, errors.New("archive focal-count workload is outside harness bounds")
	}
	selected, err := ArchiveTranscriptSelectedCount(memoryCount, workload.TranscriptSharePercent)
	if err != nil {
		return ArchiveFocalCounts{}, err
	}
	return ArchiveFocalCounts{
		Memories:                memoryCount,
		MemoryVersions:          memoryCount * workload.VersionsPerMemory,
		MemoryEvidence:          memoryCount * workload.EvidencePerMemory,
		MemoryRelations:         memoryCount * workload.RelationsPerMemory,
		TranscriptConversations: 1,
		TranscriptEntries:       selected * workload.TranscriptEntriesPerSelectedMemory,
		MemoryVectorProfiles:    1,
		MemoryVectors:           memoryCount,
		TagAssignments:          memoryCount * workload.VersionsPerMemory * workload.TagsPerVersion,
	}, nil
}

// ArchiveThroughput returns a stable three-decimal unit rate from a measured
// wall duration. Invalid inputs return zero and are rejected by result
// validation.
func ArchiveThroughput(units int64, wallDurationMS float64) float64 {
	if units <= 0 || wallDurationMS <= 0 || math.IsNaN(wallDurationMS) || math.IsInf(wallDurationMS, 0) {
		return 0
	}
	return rounded(float64(units) * 1000 / wallDurationMS)
}

// ArchiveEnvironment builds the sanitized runner metadata retained in
// evidence.
func ArchiveEnvironment(opts ArchiveOptions) SafeMetadata {
	return SafeMetadata{
		Release: opts.Release, Commit: opts.Commit, Provider: opts.Provider,
		HardwareTier: opts.HardwareTier, GoVersion: runtime.Version(),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, LogicalCPUs: runtime.NumCPU(),
	}
}

// ValidateArchiveResult requires a complete passing run and exact agreement
// between workload, measurements, registry counts, formulas, and assertion
// roll-ups. It never imposes an absolute wall-clock performance threshold.
func ValidateArchiveResult(result ArchiveResult) error {
	if result.Schema != ArchiveResultSchemaV1 || result.HarnessVersion != ArchiveHarnessVersion ||
		result.Outcome != "pass" || result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) ||
		strings.TrimSpace(result.PostgreSQLVersion) == "" || len(result.PostgreSQLVersion) > 128 {
		return errors.New("invalid archive load result envelope")
	}
	if !validArchiveEnvironment(result.Environment) {
		return errors.New("invalid archive load result environment")
	}
	if err := validateArchiveWorkload(result.Workload); err != nil {
		return err
	}
	if len(result.Measurements.CardinalityLadder) != len(result.Workload.Cardinalities) ||
		len(result.Outcomes.CardinalityLadder) != len(result.Workload.Cardinalities) {
		return errors.New("invalid archive cardinality ladder completeness")
	}
	for index, memoryCount := range result.Workload.Cardinalities {
		measurement := result.Measurements.CardinalityLadder[index]
		outcome := result.Outcomes.CardinalityLadder[index]
		if err := validateArchiveCardinalityMeasurement(memoryCount, measurement); err != nil {
			return err
		}
		if err := validateArchiveCardinalityOutcome(result.Workload, memoryCount, outcome); err != nil {
			return err
		}
		if measurement.ExportRowsPerSecond != ArchiveThroughput(int64(outcome.ExportedRows), measurement.Export.WallDurationMS) ||
			measurement.ExportBytesPerSecond != ArchiveThroughput(outcome.ArchiveBytes, measurement.Export.WallDurationMS) ||
			measurement.ImportRowsPerSecond != ArchiveThroughput(int64(outcome.ImportedRows), measurement.Import.WallDurationMS) ||
			measurement.ImportBytesPerSecond != ArchiveThroughput(outcome.ArchiveBytes, measurement.Import.WallDurationMS) {
			return errors.New("invalid archive transfer rates")
		}
	}
	vectors := result.Outcomes.VectorPortability
	if !vectors.VectorProfilesPortable || !vectors.MemoryVectorsPortable ||
		!vectors.HybridEquivalenceExercised || vectors.VectorPortabilityLimited ||
		!vectors.VectorProfilesRoundTripped || !vectors.MemoryVectorsRoundTripped {
		return errors.New("invalid archive vector portability outcomes")
	}
	if !result.Outcomes.AllCardinalitiesComplete {
		return errors.New("invalid archive cardinality completion outcomes")
	}
	return nil
}

// MarshalArchiveResult performs semantic checks and then validates the exact
// JSON instance against the checked-in Draft 2020-12 schema.
func MarshalArchiveResult(result ArchiveResult) ([]byte, error) {
	if err := ValidateArchiveResult(result); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := ValidateArchiveResultJSON(raw); err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// ValidateArchiveResultJSON validates one JSON instance with the checked-in
// Draft 2020-12 result schema.
func ValidateArchiveResultJSON(raw []byte) error {
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return fmt.Errorf("decode archive load result JSON: %w", err)
	}
	resolved, err := resolvedArchiveResultSchema()
	if err != nil {
		return err
	}
	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("validate archive load result schema: %w", err)
	}
	return nil
}

// WriteArchiveResult writes with private permissions and atomically replaces
// an older artifact only after the complete document is synced and closed.
func WriteArchiveResult(path string, result ArchiveResult) ([]byte, error) {
	raw, err := MarshalArchiveResult(result)
	if err != nil {
		return nil, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("archive load result path is required")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create archive load result directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".memory-archive-load-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create archive load result: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("protect archive load result: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("write archive load result: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("sync archive load result: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("close archive load result: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return nil, fmt.Errorf("publish archive load result: %w", err)
	}
	return raw, nil
}

func parseArchiveCardinalities(value string) ([]int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = DefaultArchiveCardinalities
	}
	parts := strings.Split(value, ",")
	if len(parts) < MinimumArchiveCardinalityCount || len(parts) > MaximumArchiveCardinalityCount {
		return nil, fmt.Errorf("%s must contain %d to %d comma-separated integers", EnvArchiveCardinalities, MinimumArchiveCardinalityCount, MaximumArchiveCardinalityCount)
	}
	out := make([]int, 0, len(parts))
	previous := MinimumArchiveCardinality - 1
	for _, part := range parts {
		parsed, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || parsed < MinimumArchiveCardinality || parsed > MaximumArchiveCardinality || parsed <= previous {
			return nil, fmt.Errorf("%s must be strictly increasing integers between %d and %d", EnvArchiveCardinalities, MinimumArchiveCardinality, MaximumArchiveCardinality)
		}
		out = append(out, parsed)
		previous = parsed
	}
	return out, nil
}

func validArchiveEnvironment(environment SafeMetadata) bool {
	return safeMetadata(environment.Release) && safeMetadata(environment.Commit) &&
		curationLabelMetadata(environment.Provider) && curationLabelMetadata(environment.HardwareTier) &&
		len(environment.GoVersion) >= 1 && len(environment.GoVersion) <= 64 &&
		len(environment.GOOS) >= 1 && len(environment.GOOS) <= 32 &&
		len(environment.GOARCH) >= 1 && len(environment.GOARCH) <= 32 && environment.LogicalCPUs >= 1
}

func validateArchiveWorkload(workload ArchiveWorkload) error {
	if !validArchiveCardinalities(workload.Cardinalities) ||
		workload.SyntheticAccounts != len(workload.Cardinalities)+1 ||
		workload.SyntheticAgents != 2*len(workload.Cardinalities)+1 ||
		workload.VersionsPerMemory < 2 || workload.VersionsPerMemory > MaximumArchiveVersionsPerMemory ||
		workload.EvidencePerMemory < 1 || workload.EvidencePerMemory > MaximumArchiveEvidencePerMemory ||
		workload.RelationsPerMemory < 1 || workload.RelationsPerMemory > MaximumArchiveRelationsPerMemory ||
		workload.TagsPerVersion < 1 || workload.TagsPerVersion > MaximumArchiveTagsPerVersion ||
		workload.TranscriptSharePercent < 1 || workload.TranscriptSharePercent > 100 ||
		workload.TranscriptEntriesPerSelectedMemory < 1 || workload.TranscriptEntriesPerSelectedMemory > MaximumArchiveTranscriptEntriesPerSelectedMemory ||
		workload.VectorDimensions < 2 || workload.VectorDimensions > MaximumArchiveVectorDimensions ||
		workload.LexicalQueries != ArchiveLexicalQueryCount ||
		workload.HybridQueries != ArchiveHybridQueryCount ||
		workload.SafetyCalls != ArchiveSafetyCallCount {
		return errors.New("invalid archive load result workload")
	}
	for _, cardinality := range workload.Cardinalities {
		if _, err := ArchiveTranscriptSelectedCount(cardinality, workload.TranscriptSharePercent); err != nil {
			return errors.New("invalid archive load result workload transcript relationship")
		}
	}
	return nil
}

func validArchiveCardinalities(values []int) bool {
	if len(values) < MinimumArchiveCardinalityCount || len(values) > MaximumArchiveCardinalityCount {
		return false
	}
	previous := MinimumArchiveCardinality - 1
	for _, value := range values {
		if value < MinimumArchiveCardinality || value > MaximumArchiveCardinality || value <= previous {
			return false
		}
		previous = value
	}
	return true
}

func validateArchiveCardinalityMeasurement(memoryCount int, measurement ArchiveCardinalityMeasurement) error {
	if measurement.MemoryCount != memoryCount ||
		measurement.Seed.Count != 1 || measurement.Export.Count != 1 ||
		measurement.Verify.Count != 1 || measurement.Import.Count != 1 ||
		measurement.LexicalRecallBefore.Count != ArchiveLexicalQueryCount ||
		measurement.LexicalRecallAfter.Count != ArchiveLexicalQueryCount ||
		measurement.HybridRecallBefore.Count != ArchiveHybridQueryCount ||
		measurement.HybridRecallAfter.Count != ArchiveHybridQueryCount ||
		measurement.PostImportSafety.Count != ArchiveSafetyCallCount {
		return errors.New("invalid archive cardinality measurements")
	}
	for _, stats := range []OperationStats{
		measurement.Seed, measurement.Export, measurement.Verify, measurement.Import,
		measurement.LexicalRecallBefore, measurement.LexicalRecallAfter,
		measurement.HybridRecallBefore, measurement.HybridRecallAfter,
		measurement.PostImportSafety,
	} {
		if !validOperationStats(stats) {
			return errors.New("invalid archive cardinality measurements")
		}
	}
	return nil
}

func validateArchiveCardinalityOutcome(workload ArchiveWorkload, memoryCount int, outcome ArchiveCardinalityOutcome) error {
	if len(archivePortableTableNamesV1) != ArchivePortableTableCount {
		return errors.New("invalid archive portable table registry")
	}
	if outcome.MemoryCount != memoryCount || outcome.ManifestFormatVersion != ArchiveFormatVersion ||
		outcome.ManifestSchemaVersion < 1 || outcome.ManifestPurpose != ArchiveManifestPurposeSelf ||
		outcome.ManifestStatus != ArchiveManifestStatus || outcome.ArchiveBytes < 1 || outcome.ChunkBytes < 1 ||
		outcome.ManifestTables != ArchivePortableTableCount ||
		len(outcome.TableRows) != outcome.ManifestTables || outcome.NonEmptyTables < 1 ||
		outcome.ChunkCount < outcome.NonEmptyTables || outcome.ExportedRows < 1 ||
		outcome.VerifiedRows < 1 || outcome.ImportedRows < 1 {
		return errors.New("invalid archive manifest and artifact outcomes")
	}
	tableCounts := make(map[string]int, len(outcome.TableRows))
	var exportedRows, verifiedRows, importedRows int64
	nonEmpty := 0
	for index, item := range outcome.TableRows {
		if item.Name != archivePortableTableNamesV1[index] ||
			item.Exported < 0 || item.Verified < 0 || item.Imported < 0 ||
			item.Exported != item.Verified || item.Exported != item.Imported {
			return errors.New("invalid archive table row outcomes")
		}
		tableCounts[item.Name] = item.Exported
		exportedRows += int64(item.Exported)
		verifiedRows += int64(item.Verified)
		importedRows += int64(item.Imported)
		if item.Exported > 0 {
			nonEmpty++
		}
	}
	if exportedRows != int64(outcome.ExportedRows) || verifiedRows != int64(outcome.VerifiedRows) ||
		importedRows != int64(outcome.ImportedRows) || nonEmpty != outcome.NonEmptyTables {
		return errors.New("invalid archive table row totals")
	}
	wantFocal, err := ArchiveFocalCountsFor(memoryCount, workload)
	if err != nil || outcome.FocalCounts != wantFocal {
		return errors.New("invalid archive focal counts")
	}
	for table, want := range map[string]int{
		"memories":                 wantFocal.Memories,
		"memory_versions":          wantFocal.MemoryVersions,
		"memory_evidence":          wantFocal.MemoryEvidence,
		"memory_relations":         wantFocal.MemoryRelations,
		"transcript_conversations": wantFocal.TranscriptConversations,
		"transcript_entries":       wantFocal.TranscriptEntries,
		"memory_vector_profiles":   wantFocal.MemoryVectorProfiles,
		"memory_vectors":           wantFocal.MemoryVectors,
	} {
		if tableCounts[table] != want {
			return errors.New("invalid archive focal table counts")
		}
	}
	if !outcome.ChecksumsRead || !outcome.AllChunksVerified || !outcome.AllTablesVerified ||
		!outcome.AccountRemovedBeforeImport || !outcome.SameStoreRoundTrip ||
		!outcome.ExactTableRowCounts || !outcome.ExactFixtureCounts {
		return errors.New("invalid archive verification outcomes")
	}
	if err := validateArchiveRecallEquivalence(outcome.RecallEquivalence); err != nil {
		return err
	}
	safety := outcome.Safety
	if safety.RecallCalls != ArchiveSafetyCallCount || !safety.SensitiveBroadRedacted ||
		!safety.SensitiveExactOwnerVisible || !safety.CrossAgentIsolated || !safety.CrossAccountIsolated {
		return errors.New("invalid archive safety outcomes")
	}
	return nil
}

func validateArchiveRecallEquivalence(outcome ArchiveRecallEquivalenceOutcome) error {
	expected := [...]struct {
		name string
		mode string
	}{
		{ArchiveRecallCaseLexicalPrimary, ArchiveRecallModeLexical},
		{ArchiveRecallCaseLexicalSecondary, ArchiveRecallModeLexical},
		{ArchiveRecallCaseHybridPrimary, ArchiveRecallModeHybrid},
		{ArchiveRecallCaseHybridSecondary, ArchiveRecallModeHybrid},
	}
	if len(outcome.Cases) != len(expected) || outcome.BeforeCalls != len(expected) ||
		outcome.AfterCalls != len(expected) || outcome.ScoreComparison != ArchiveScoreComparisonExact ||
		outcome.ScoreTolerance != 0 || !outcome.AllRankingsIdentical ||
		!outcome.AllScoreComponentsExact || !outcome.AllMetadataExact ||
		!outcome.RetrievalProjectionProved {
		return errors.New("invalid archive recall equivalence outcomes")
	}
	for index, item := range outcome.Cases {
		if item.Name != expected[index].name || item.Mode != expected[index].mode ||
			item.BeforeHits < 1 || item.AfterHits != item.BeforeHits ||
			!item.RankedIDsIdentical || !item.ScoreComponentsExact || !item.MetadataExact {
			return errors.New("invalid archive recall equivalence case")
		}
	}
	return nil
}

var (
	archiveResultSchemaOnce     sync.Once
	archiveResultSchemaResolved *jsonschema.Resolved
	archiveResultSchemaErr      error
)

func resolvedArchiveResultSchema() (*jsonschema.Resolved, error) {
	archiveResultSchemaOnce.Do(func() {
		var schema jsonschema.Schema
		if err := json.Unmarshal(archiveResultSchemaJSON, &schema); err != nil {
			archiveResultSchemaErr = fmt.Errorf("decode embedded archive result schema: %w", err)
			return
		}
		archiveResultSchemaResolved, archiveResultSchemaErr = schema.Resolve(nil)
		if archiveResultSchemaErr != nil {
			archiveResultSchemaErr = fmt.Errorf("resolve embedded archive result schema: %w", archiveResultSchemaErr)
		}
	})
	return archiveResultSchemaResolved, archiveResultSchemaErr
}
