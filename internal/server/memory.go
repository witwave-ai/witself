package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// encoding/json can expand one input byte to six bytes (for example '<'
	// becomes "\\u003c"). The store accepts 256 KiB content, 256 links of
	// 2 KiB, 64 tags of 128 bytes, 32 evidence locators of 2 KiB, and bounded
	// client metadata. Eight MiB conservatively covers that worst-case capture
	// envelope after JSON escaping and structural overhead.
	maxMemoryCaptureRequestBytes int64 = 8 << 20

	// An adjustment may carry independent maximum add and remove sets, so its
	// worst-case bounded link metadata is twice a capture's. Keep a separate
	// ceiling rather than making valid store inputs depend on JSON overhead.
	maxMemoryAdjustRequestBytes int64 = 16 << 20

	// A supersession may contain 32 legal capture envelopes. One additional
	// MiB covers the outer reason, provenance, retry keys, and JSON structure.
	maxMemorySupersedeRequestBytes int64 = 32*maxMemoryCaptureRequestBytes + (1 << 20)
)

// MemoryOwner identifies the stable agent that owns a narrative memory.
type MemoryOwner struct {
	Kind      string `json:"kind"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name,omitempty"`
}

// MemoryActor is the token-derived principal responsible for one version.
type MemoryActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// MemoryClientProvenance is self-reported client inference provenance. It is
// metadata only and never participates in authentication or authorization.
type MemoryClientProvenance struct {
	Runtime       string `json:"runtime,omitempty"`
	Model         string `json:"model,omitempty"`
	Recipe        string `json:"recipe,omitempty"`
	RecipeVersion string `json:"recipe_version,omitempty"`
}

// MemoryEvidenceInput attaches resolved, pending, or unavailable evidence to
// a new capture. The store validates the state-specific locator shape.
type MemoryEvidenceInput struct {
	State               string `json:"state"`
	Type                string `json:"type,omitempty"`
	Role                string `json:"role,omitempty"`
	ExternalLocator     string `json:"external_locator,omitempty"`
	TranscriptID        string `json:"transcript_id,omitempty"`
	EntryFromSequence   *int64 `json:"entry_from_sequence,omitempty"`
	EntryUntilSequence  *int64 `json:"entry_until_sequence,omitempty"`
	SourceMemoryID      string `json:"source_memory_id,omitempty"`
	SourceMemoryVersion *int64 `json:"source_memory_version,omitempty"`
	MessageID           string `json:"message_id,omitempty"`
	ImportArtifactID    string `json:"import_artifact_id,omitempty"`
	UnavailableReason   string `json:"unavailable_reason,omitempty"`
	SourceDigest        string `json:"source_digest,omitempty"`
}

// MemoryEvidence is one append-only, version-aware evidence record.
type MemoryEvidence struct {
	ID                  string      `json:"id"`
	MemoryID            string      `json:"memory_id"`
	MemoryVersion       int64       `json:"memory_version"`
	State               string      `json:"state"`
	Type                string      `json:"type,omitempty"`
	Role                string      `json:"role"`
	PendingEvidenceID   string      `json:"pending_evidence_id,omitempty"`
	ExternalLocator     string      `json:"external_locator,omitempty"`
	TranscriptID        string      `json:"transcript_id,omitempty"`
	EntryFromSequence   *int64      `json:"entry_from_sequence,omitempty"`
	EntryUntilSequence  *int64      `json:"entry_until_sequence,omitempty"`
	SourceMemoryID      string      `json:"source_memory_id,omitempty"`
	SourceMemoryVersion *int64      `json:"source_memory_version,omitempty"`
	MessageID           string      `json:"message_id,omitempty"`
	ImportArtifactID    string      `json:"import_artifact_id,omitempty"`
	UnavailableReason   string      `json:"unavailable_reason,omitempty"`
	UnresolvableReason  string      `json:"unresolvable_reason,omitempty"`
	SourceDigest        string      `json:"source_digest,omitempty"`
	ChangeSeq           int64       `json:"change_seq"`
	Actor               MemoryActor `json:"actor"`
	CreatedAt           time.Time   `json:"created_at"`
}

// Memory is a stable resource and its current immutable version, flattened for
// ordinary reads and mutations. List implementations omit sensitive content
// and set Redacted unless IncludeSensitive was explicitly authorized.
type Memory struct {
	ID                            string                 `json:"id"`
	AccountID                     string                 `json:"account_id"`
	RealmID                       string                 `json:"realm_id"`
	Owner                         MemoryOwner            `json:"owner"`
	Origin                        string                 `json:"origin"`
	CaptureReason                 string                 `json:"capture_reason"`
	OriginalAuthor                MemoryActor            `json:"original_author"`
	Version                       int64                  `json:"version"`
	PreviousVersion               *int64                 `json:"previous_version,omitempty"`
	ChangeSeq                     int64                  `json:"change_seq"`
	Content                       string                 `json:"content,omitempty"`
	ContentEncoding               string                 `json:"content_encoding"`
	Kind                          string                 `json:"kind"`
	Tags                          []string               `json:"tags"`
	Salience                      float64                `json:"salience"`
	Links                         []string               `json:"links"`
	Sensitive                     bool                   `json:"sensitive"`
	Redacted                      bool                   `json:"redacted,omitempty"`
	OccurredFrom                  *time.Time             `json:"occurred_from,omitempty"`
	OccurredUntil                 *time.Time             `json:"occurred_until,omitempty"`
	State                         string                 `json:"state"`
	StateReason                   string                 `json:"state_reason,omitempty"`
	PriorState                    string                 `json:"prior_state,omitempty"`
	ContentHash                   string                 `json:"content_hash"`
	Operation                     string                 `json:"operation"`
	SupersessionSetID             string                 `json:"supersession_set_id,omitempty"`
	SupersessionSetRevision       int64                  `json:"supersession_set_revision,omitempty"`
	SupersessionReplacementCount  int64                  `json:"supersession_replacement_count,omitempty"`
	SupersessionReplacementDigest string                 `json:"supersession_replacement_digest,omitempty"`
	ActiveSupersessionSetID       string                 `json:"active_supersession_set_id,omitempty"`
	ActiveSupersessionSetRevision int64                  `json:"active_supersession_set_revision,omitempty"`
	Actor                         MemoryActor            `json:"actor"`
	Client                        MemoryClientProvenance `json:"client,omitempty"`
	Evidence                      []MemoryEvidence       `json:"evidence"`
	CreatedAt                     time.Time              `json:"created_at"`
	UpdatedAt                     time.Time              `json:"updated_at"`
}

// MemoryVersion is one immutable full payload snapshot in memory history.
type MemoryVersion struct {
	MemoryID                      string                 `json:"memory_id"`
	Version                       int64                  `json:"version"`
	PreviousVersion               *int64                 `json:"previous_version,omitempty"`
	ChangeSeq                     int64                  `json:"change_seq"`
	Content                       string                 `json:"content,omitempty"`
	ContentEncoding               string                 `json:"content_encoding"`
	Kind                          string                 `json:"kind"`
	Tags                          []string               `json:"tags"`
	Salience                      float64                `json:"salience"`
	Links                         []string               `json:"links"`
	Sensitive                     bool                   `json:"sensitive"`
	Redacted                      bool                   `json:"redacted,omitempty"`
	OccurredFrom                  *time.Time             `json:"occurred_from,omitempty"`
	OccurredUntil                 *time.Time             `json:"occurred_until,omitempty"`
	State                         string                 `json:"state"`
	StateReason                   string                 `json:"state_reason,omitempty"`
	PriorState                    string                 `json:"prior_state,omitempty"`
	ContentHash                   string                 `json:"content_hash"`
	Operation                     string                 `json:"operation"`
	SupersessionSetID             string                 `json:"supersession_set_id,omitempty"`
	SupersessionSetRevision       int64                  `json:"supersession_set_revision,omitempty"`
	SupersessionReplacementCount  int64                  `json:"supersession_replacement_count,omitempty"`
	SupersessionReplacementDigest string                 `json:"supersession_replacement_digest,omitempty"`
	ActiveSupersessionSetID       string                 `json:"active_supersession_set_id,omitempty"`
	ActiveSupersessionSetRevision int64                  `json:"active_supersession_set_revision,omitempty"`
	Actor                         MemoryActor            `json:"actor"`
	Client                        MemoryClientProvenance `json:"client,omitempty"`
	Evidence                      []MemoryEvidence       `json:"evidence"`
	CreatedAt                     time.Time              `json:"created_at"`
}

// MemoryMutationReceipt is a value-free response suitable for exact retries.
type MemoryMutationReceipt struct {
	Operation            string      `json:"operation"`
	Actor                MemoryActor `json:"actor"`
	IdempotencyKey       string      `json:"idempotency_key"`
	CanonicalRequestHash string      `json:"canonical_request_hash"`
	MemoryID             string      `json:"memory_id"`
	Version              int64       `json:"version"`
	Replayed             bool        `json:"replayed,omitempty"`
	CreatedAt            time.Time   `json:"created_at"`
}

// MemoryMutationResult combines the new head and its value-free receipt.
type MemoryMutationResult struct {
	Memory  Memory                `json:"memory"`
	Receipt MemoryMutationReceipt `json:"receipt"`
}

// MemoryVersionReference is a value-free exact version locator. It is safe to
// retain in retry receipts because it carries no memory or evidence value.
type MemoryVersionReference struct {
	MemoryID string `json:"memory_id"`
	Version  int64  `json:"version"`
}

// MemorySupersessionReceipt identifies the immutable source and replacement
// versions in one server-assigned supersession set without repeating values.
type MemorySupersessionReceipt struct {
	Operation               string                   `json:"operation"`
	Actor                   MemoryActor              `json:"actor"`
	IdempotencyKey          string                   `json:"idempotency_key"`
	CanonicalRequestHash    string                   `json:"canonical_request_hash"`
	SupersessionSetID       string                   `json:"supersession_set_id"`
	SupersessionSetRevision int64                    `json:"supersession_set_revision"`
	ReplacementCount        int64                    `json:"replacement_count"`
	ReplacementDigest       string                   `json:"replacement_digest"`
	Source                  MemoryVersionReference   `json:"source"`
	Replacements            []MemoryVersionReference `json:"replacements"`
	CreatedAt               time.Time                `json:"created_at"`
	Replayed                bool                     `json:"replayed,omitempty"`
}

// SupersedeMemoryResult contains the full authorized source and replacement
// snapshots plus a value-free retry receipt.
type SupersedeMemoryResult struct {
	Source       Memory                    `json:"source"`
	Replacements []Memory                  `json:"replacements"`
	Receipt      MemorySupersessionReceipt `json:"receipt"`
}

// MemoryPage is one opaque-cursor page of current memory heads.
type MemoryPage struct {
	SchemaVersion string   `json:"schema_version,omitempty"`
	Items         []Memory `json:"items"`
	NextCursor    string   `json:"next_cursor,omitempty"`
}

// MemoryHistoryPage is one opaque-cursor page of immutable versions.
type MemoryHistoryPage struct {
	SchemaVersion string          `json:"schema_version,omitempty"`
	Versions      []MemoryVersion `json:"versions"`
	NextCursor    string          `json:"next_cursor,omitempty"`
}

// MemoryRecallRequest is a structured deterministic search. Natural-language
// time interpretation and optional reranking belong to the client.
type MemoryRecallRequest struct {
	Query            string     `json:"query,omitempty"`
	Kind             string     `json:"kind,omitempty"`
	Tags             []string   `json:"tags,omitempty"`
	Links            []string   `json:"links,omitempty"`
	Origin           string     `json:"origin,omitempty"`
	CaptureReason    string     `json:"capture_reason,omitempty"`
	IncludeSensitive bool       `json:"include_sensitive,omitempty"`
	OccurredFrom     *time.Time `json:"occurred_from,omitempty"`
	OccurredUntil    *time.Time `json:"occurred_until,omitempty"`
	CapturedFrom     *time.Time `json:"captured_from,omitempty"`
	CapturedUntil    *time.Time `json:"captured_until,omitempty"`
	Limit            int        `json:"limit,omitempty"`
	Cursor           string     `json:"cursor,omitempty"`
	VectorProfileID  string     `json:"vector_profile_id,omitempty"`
	QueryVector      []float64  `json:"query_vector,omitempty"`
}

// MemoryRecallScore describes the deterministic components used to rank one recall hit.
type MemoryRecallScore struct {
	Similarity float64 `json:"similarity"`
	VectorUsed bool    `json:"vector_used"`
	Lexical    float64 `json:"lexical"`
	Salience   float64 `json:"salience"`
	Recency    float64 `json:"recency"`
	Total      float64 `json:"total"`
}

// MemoryRecallHit pairs an authorized memory with its recall ranking score.
type MemoryRecallHit struct {
	Memory Memory            `json:"memory"`
	Score  MemoryRecallScore `json:"score"`
}

// MemoryRecallPage is one opaque-cursor page of ranked memory recall results.
type MemoryRecallPage struct {
	SchemaVersion      string            `json:"schema_version"`
	Hits               []MemoryRecallHit `json:"hits"`
	NextCursor         string            `json:"next_cursor,omitempty"`
	RetrievalMode      string            `json:"retrieval_mode"`
	VectorCoverage     float64           `json:"vector_coverage"`
	VectorProfileID    string            `json:"vector_profile_id,omitempty"`
	VectorCandidates   int               `json:"vector_candidates,omitempty"`
	VectorMatches      int               `json:"vector_matches,omitempty"`
	CandidateTruncated bool              `json:"candidate_truncated,omitempty"`
	CandidateLimit     int               `json:"candidate_limit,omitempty"`
	Degraded           bool              `json:"degraded"`
	DegradedReason     string            `json:"degraded_reason,omitempty"`
}

// CaptureMemoryRequest is the POST /v1/memories request body. Actor, account,
// realm, stable owner, and origin are derived at the authenticated boundary.
type CaptureMemoryRequest struct {
	Content         string                 `json:"content"`
	ContentEncoding string                 `json:"content_encoding,omitempty"`
	Kind            string                 `json:"kind,omitempty"`
	Tags            []string               `json:"tags,omitempty"`
	Salience        *float64               `json:"salience,omitempty"`
	Sensitive       bool                   `json:"sensitive,omitempty"`
	Links           []string               `json:"links,omitempty"`
	OccurredFrom    *time.Time             `json:"occurred_from,omitempty"`
	OccurredUntil   *time.Time             `json:"occurred_until,omitempty"`
	Evidence        []MemoryEvidenceInput  `json:"evidence"`
	CaptureReason   string                 `json:"capture_reason"`
	Client          MemoryClientProvenance `json:"client,omitempty"`
	IdempotencyKey  string                 `json:"-"`
}

// SupersedeMemoryReplacementRequest is one client-authored replacement
// capsule. Unlike a top-level capture, its retry key travels in the JSON body
// so the entire nonempty replacement set can commit atomically.
type SupersedeMemoryReplacementRequest struct {
	Content         string                 `json:"content"`
	ContentEncoding string                 `json:"content_encoding,omitempty"`
	Kind            string                 `json:"kind,omitempty"`
	Tags            []string               `json:"tags,omitempty"`
	Salience        *float64               `json:"salience,omitempty"`
	Sensitive       bool                   `json:"sensitive,omitempty"`
	Links           []string               `json:"links,omitempty"`
	OccurredFrom    *time.Time             `json:"occurred_from,omitempty"`
	OccurredUntil   *time.Time             `json:"occurred_until,omitempty"`
	Evidence        []MemoryEvidenceInput  `json:"evidence"`
	CaptureReason   string                 `json:"capture_reason"`
	Client          MemoryClientProvenance `json:"client,omitempty"`
	IdempotencyKey  string                 `json:"idempotency_key"`
}

// SupersedeMemoryRequest atomically supersedes one exact active version with
// one or more replacement capsules. The operation retry key comes only from
// the Idempotency-Key header.
type SupersedeMemoryRequest struct {
	ExpectedVersion int64                               `json:"expected_version"`
	Replacements    []SupersedeMemoryReplacementRequest `json:"replacements"`
	Reason          string                              `json:"reason,omitempty"`
	Client          MemoryClientProvenance              `json:"client,omitempty"`
	IdempotencyKey  string                              `json:"-"`
}

// MemoryListOptions selects a bounded current-head inventory.
type MemoryListOptions struct {
	OwnerAgentID     string
	State            string
	Kind             string
	Tags             []string
	Origin           string
	CaptureReason    string
	IncludeSensitive bool
	OccurredFrom     *time.Time
	OccurredUntil    *time.Time
	CapturedFrom     *time.Time
	CapturedUntil    *time.Time
	Limit            int
	Cursor           string
}

// MemoryHistoryOptions selects one bounded history page.
type MemoryHistoryOptions struct {
	Limit  int
	Cursor string
}

// AdjustMemoryRequest is a sparse expected-version guarded patch. Nil set
// fields preserve their current value.
type AdjustMemoryRequest struct {
	ExpectedVersion    int64                  `json:"expected_version"`
	SetContent         *string                `json:"set_content,omitempty"`
	SetContentEncoding *string                `json:"set_content_encoding,omitempty"`
	SetKind            *string                `json:"set_kind,omitempty"`
	AddTags            []string               `json:"add_tags,omitempty"`
	RemoveTags         []string               `json:"remove_tags,omitempty"`
	SetSalience        *float64               `json:"set_salience,omitempty"`
	AddLinks           []string               `json:"add_links,omitempty"`
	RemoveLinks        []string               `json:"remove_links,omitempty"`
	SetSensitive       *bool                  `json:"set_sensitive,omitempty"`
	SetOccurredFrom    *time.Time             `json:"set_occurred_from,omitempty"`
	ClearOccurredFrom  bool                   `json:"clear_occurred_from,omitempty"`
	SetOccurredUntil   *time.Time             `json:"set_occurred_until,omitempty"`
	ClearOccurredUntil bool                   `json:"clear_occurred_until,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	Client             MemoryClientProvenance `json:"client,omitempty"`
	IdempotencyKey     string                 `json:"-"`
}

// MemoryLifecycleRequest carries the concurrency and retry guards for forget,
// restore, and reactivate.
type MemoryLifecycleRequest struct {
	ExpectedVersion                 int64                  `json:"expected_version"`
	ExpectedSupersessionSetRevision *int64                 `json:"expected_supersession_set_revision,omitempty"`
	Reason                          string                 `json:"reason,omitempty"`
	Client                          MemoryClientProvenance `json:"client,omitempty"`
	IdempotencyKey                  string                 `json:"-"`
}

// ResolveMemoryEvidenceRequest appends a terminal resolution for one pending
// evidence locator. Exactly one concrete source, or one unresolvable reason,
// is accepted. The pending row itself remains immutable.
type ResolveMemoryEvidenceRequest struct {
	TranscriptID        string `json:"transcript_id,omitempty"`
	EntryFromSequence   *int64 `json:"entry_from_sequence,omitempty"`
	EntryUntilSequence  *int64 `json:"entry_until_sequence,omitempty"`
	SourceMemoryID      string `json:"source_memory_id,omitempty"`
	SourceMemoryVersion *int64 `json:"source_memory_version,omitempty"`
	MessageID           string `json:"message_id,omitempty"`
	ImportArtifactID    string `json:"import_artifact_id,omitempty"`
	UnresolvableReason  string `json:"unresolvable_reason,omitempty"`
	SourceDigest        string `json:"source_digest,omitempty"`
	IdempotencyKey      string `json:"-"`
}

// DeleteMemoryRequest is value-free. Preview accepts only MemoryID; apply
// binds the preview's exact current version and deterministic scrub revision.
type DeleteMemoryRequest struct {
	MemoryID         string
	ExpectedVersion  int64
	ScrubSetRevision string
	ReasonCode       string
	IdempotencyKey   string
	Apply            bool
}

// MemoryDeletionReceipt deliberately excludes all memory and evidence values,
// content-derived hashes, raw retry keys, and client provenance.
type MemoryDeletionReceipt struct {
	MemoryID                      string     `json:"memory_id"`
	ReceiptID                     string     `json:"receipt_id,omitempty"`
	PriorVersion                  int64      `json:"prior_version"`
	ScrubSetRevision              string     `json:"scrub_set_revision"`
	VersionCount                  int64      `json:"version_count"`
	EvidenceCount                 int64      `json:"evidence_count"`
	RelationCount                 int64      `json:"relation_count"`
	RetryShieldCount              int64      `json:"retry_shield_count"`
	RetryShieldDigest             string     `json:"retry_shield_digest"`
	IncomingEvidenceCount         int64      `json:"incoming_evidence_count"`
	ActiveRelationDependencyCount int64      `json:"active_relation_dependency_count"`
	ActiveCurationDependencyCount int64      `json:"active_curation_dependency_count"`
	CurationRunCount              int64      `json:"curation_run_count"`
	CurationActionCount           int64      `json:"curation_action_count"`
	CurationInputCount            int64      `json:"curation_input_count"`
	CurationMutationCount         int64      `json:"curation_mutation_count"`
	Blocked                       bool       `json:"blocked"`
	DeletedAt                     *time.Time `json:"deleted_at,omitempty"`
	Applied                       bool       `json:"applied"`
	Replayed                      bool       `json:"replayed,omitempty"`
}

func decodeMemoryRequestJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64, invalidMessage string) bool {
	err := decodeLimitedJSON(w, r, dst, maxBytes)
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "memory request body exceeds the supported limit")
		return false
	}
	writeJSONError(w, http.StatusBadRequest, invalidMessage)
	return false
}

func captureMemoryHandler(auth PrincipalAuthFunc, capture func(context.Context, DomainPrincipal, CaptureMemoryRequest) (MemoryMutationResult, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may capture memories")
			return
		}
		var req CaptureMemoryRequest
		if !decodeMemoryRequestJSON(w, r, &req, maxMemoryCaptureRequestBytes, "invalid JSON memory body") {
			return
		}
		if req.ContentEncoding == "" {
			req.ContentEncoding = "plain"
		}
		if len(req.Evidence) == 0 {
			writeJSONError(w, http.StatusBadRequest,
				"memory capture requires exact, pending, or explicitly unavailable evidence")
			return
		}
		if !setMemoryIdempotencyKey(w, r, &req.IdempotencyKey) {
			return
		}
		result, err := capture(r.Context(), p, req)
		if writeMemoryError(w, err, "capture memory") {
			return
		}
		writeMemoryResult(w, http.StatusCreated, result)
	})
}

func supersedeMemoryHandler(auth PrincipalAuthFunc, supersede func(context.Context, DomainPrincipal, string, SupersedeMemoryRequest) (SupersedeMemoryResult, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may supersede memories")
			return
		}
		memoryID := strings.TrimSpace(r.PathValue("memory"))
		if memoryID == "" {
			writeJSONError(w, http.StatusBadRequest, "memory id is required")
			return
		}
		var req SupersedeMemoryRequest
		if !decodeMemoryRequestJSON(w, r, &req, maxMemorySupersedeRequestBytes, "invalid JSON memory supersession body") {
			return
		}
		if req.ExpectedVersion < 1 {
			writeJSONError(w, http.StatusBadRequest, "expected_version must be positive")
			return
		}
		if len(req.Replacements) == 0 {
			writeJSONError(w, http.StatusBadRequest, "memory supersession requires at least one replacement")
			return
		}
		for i := range req.Replacements {
			replacement := &req.Replacements[i]
			if replacement.ContentEncoding == "" {
				replacement.ContentEncoding = "plain"
			}
			replacement.IdempotencyKey = strings.TrimSpace(replacement.IdempotencyKey)
			if replacement.IdempotencyKey == "" {
				writeJSONError(w, http.StatusBadRequest, "each memory replacement requires idempotency_key")
				return
			}
			if len(replacement.Evidence) == 0 {
				writeJSONError(w, http.StatusBadRequest,
					"each memory replacement requires exact, pending, or explicitly unavailable evidence")
				return
			}
		}
		if !setMemoryIdempotencyKey(w, r, &req.IdempotencyKey) {
			return
		}
		result, err := supersede(r.Context(), p, memoryID, req)
		if writeMemoryError(w, err, "supersede memory") {
			return
		}
		for i := range result.Replacements {
			normalizeMemory(&result.Replacements[i])
		}
		normalizeMemory(&result.Source)
		if result.Replacements == nil {
			result.Replacements = []Memory{}
		}
		if result.Receipt.Replacements == nil {
			result.Receipt.Replacements = []MemoryVersionReference{}
		}
		setMemoryResponseHeaders(w)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"source":         result.Source,
			"replacements":   result.Replacements,
			"receipt":        result.Receipt,
		})
	})
}

func getMemoryHandler(auth PrincipalAuthFunc, get func(context.Context, DomainPrincipal, string) (Memory, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		memory, err := get(r.Context(), p, strings.TrimSpace(r.PathValue("memory")))
		if writeMemoryError(w, err, "read memory") {
			return
		}
		normalizeMemory(&memory)
		setMemoryResponseHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"memory":         memory,
		})
	})
}

func listMemoriesHandler(auth PrincipalAuthFunc, list func(context.Context, DomainPrincipal, MemoryListOptions) (MemoryPage, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		opts, err := memoryListOptions(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		page, err := list(r.Context(), p, opts)
		if writeMemoryError(w, err, "list memories") {
			return
		}
		if page.Items == nil {
			page.Items = []Memory{}
		}
		for i := range page.Items {
			if page.Items[i].Sensitive && !opts.IncludeSensitive {
				redactMemoryForBroadResponse(&page.Items[i])
			}
			normalizeMemory(&page.Items[i])
		}
		page.SchemaVersion = "witself.v0"
		setMemoryResponseHeaders(w)
		_ = json.NewEncoder(w).Encode(page)
	})
}

func memoryHistoryHandler(auth PrincipalAuthFunc, history func(context.Context, DomainPrincipal, string, MemoryHistoryOptions) (MemoryHistoryPage, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		opts, err := memoryHistoryOptions(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		page, err := history(r.Context(), p, strings.TrimSpace(r.PathValue("memory")), opts)
		if writeMemoryError(w, err, "read memory history") {
			return
		}
		if page.Versions == nil {
			page.Versions = []MemoryVersion{}
		}
		for i := range page.Versions {
			normalizeMemoryVersion(&page.Versions[i])
		}
		page.SchemaVersion = "witself.v0"
		setMemoryResponseHeaders(w)
		_ = json.NewEncoder(w).Encode(page)
	})
}

func recallMemoriesHandler(auth PrincipalAuthFunc, recall func(context.Context, DomainPrincipal, MemoryRecallRequest) (MemoryRecallPage, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may recall memories")
			return
		}
		var req MemoryRecallRequest
		if err := decodeLimitedJSON(w, r, &req, 96*1024); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON memory recall body")
			return
		}
		if req.Limit < 0 || req.Limit > 100 {
			writeJSONError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		page, err := recall(r.Context(), p, req)
		if writeMemoryError(w, err, "recall memories") {
			return
		}
		if page.Hits == nil {
			page.Hits = []MemoryRecallHit{}
		}
		for i := range page.Hits {
			if page.Hits[i].Memory.Sensitive && !req.IncludeSensitive {
				redactMemoryForBroadResponse(&page.Hits[i].Memory)
			}
			normalizeMemory(&page.Hits[i].Memory)
		}
		page.SchemaVersion = "witself.v0"
		setMemoryResponseHeaders(w)
		_ = json.NewEncoder(w).Encode(page)
	})
}

func adjustMemoryHandler(auth PrincipalAuthFunc, adjust func(context.Context, DomainPrincipal, string, AdjustMemoryRequest) (MemoryMutationResult, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may adjust memories")
			return
		}
		var req AdjustMemoryRequest
		if !decodeMemoryRequestJSON(w, r, &req, maxMemoryAdjustRequestBytes, "invalid JSON memory body") {
			return
		}
		if req.ExpectedVersion < 1 {
			writeJSONError(w, http.StatusBadRequest, "expected_version must be positive")
			return
		}
		if !setMemoryIdempotencyKey(w, r, &req.IdempotencyKey) {
			return
		}
		result, err := adjust(r.Context(), p, strings.TrimSpace(r.PathValue("memory")), req)
		if writeMemoryError(w, err, "adjust memory") {
			return
		}
		writeMemoryResult(w, http.StatusOK, result)
	})
}

func memoryLifecycleHandler(
	auth PrincipalAuthFunc,
	forget, restore, reactivate func(context.Context, DomainPrincipal, string, MemoryLifecycleRequest) (MemoryMutationResult, error),
) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may change memory lifecycle")
			return
		}
		memoryID, action, ok := parseMemoryAction(r.PathValue("action"))
		if !ok {
			writeJSONError(w, http.StatusNotFound, "memory action not found")
			return
		}
		var mutate func(context.Context, DomainPrincipal, string, MemoryLifecycleRequest) (MemoryMutationResult, error)
		switch action {
		case "forget":
			mutate = forget
		case "restore":
			mutate = restore
		case "reactivate":
			mutate = reactivate
		}
		if mutate == nil {
			writeJSONError(w, http.StatusNotFound, "memory action not found")
			return
		}
		var req MemoryLifecycleRequest
		if err := decodeLimitedJSON(w, r, &req, 32*1024); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON memory lifecycle body")
			return
		}
		if req.ExpectedVersion < 1 {
			writeJSONError(w, http.StatusBadRequest, "expected_version must be positive")
			return
		}
		if !setMemoryIdempotencyKey(w, r, &req.IdempotencyKey) {
			return
		}
		result, err := mutate(r.Context(), p, memoryID, req)
		if writeMemoryError(w, err, action+" memory") {
			return
		}
		writeMemoryResult(w, http.StatusOK, result)
	})
}

func resolveMemoryEvidenceHandler(auth PrincipalAuthFunc, resolve func(context.Context, DomainPrincipal, string, ResolveMemoryEvidenceRequest) (MemoryEvidence, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may resolve memory evidence")
			return
		}
		var req ResolveMemoryEvidenceRequest
		if err := decodeLimitedJSON(w, r, &req, 96*1024); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON memory evidence body")
			return
		}
		if !setMemoryIdempotencyKey(w, r, &req.IdempotencyKey) {
			return
		}
		evidence, err := resolve(r.Context(), p, strings.TrimSpace(r.PathValue("evidence")), req)
		if writeMemoryError(w, err, "resolve memory evidence") {
			return
		}
		setMemoryResponseHeaders(w)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"evidence":       evidence,
		})
	})
}

func deleteMemoryHandler(auth PrincipalAuthFunc, deleteMemory func(context.Context, DomainPrincipal, DeleteMemoryRequest) (MemoryDeletionReceipt, error)) http.HandlerFunc {
	protected := requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may permanently delete memories")
			return
		}
		dryRun := false
		if raw := r.URL.Query().Get("dry_run"); raw != "" {
			var err error
			dryRun, err = strconv.ParseBool(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "dry_run must be true or false")
				return
			}
		}
		req := DeleteMemoryRequest{
			MemoryID:         strings.TrimSpace(r.PathValue("memory")),
			ScrubSetRevision: strings.TrimSpace(r.URL.Query().Get("scrub_set_revision")),
			IdempotencyKey:   strings.TrimSpace(r.Header.Get("Idempotency-Key")),
			Apply:            !dryRun,
		}
		directUserAuthorization := strings.TrimSpace(r.Header.Get("X-Witself-Direct-User-Authorized"))
		if _, supplied := r.URL.Query()["reason_code"]; supplied {
			writeJSONError(w, http.StatusBadRequest, "memory deletion reason is server-owned")
			return
		}
		if raw := strings.TrimSpace(r.URL.Query().Get("expected_version")); raw != "" {
			version, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "expected_version must be a positive integer")
				return
			}
			req.ExpectedVersion = version
		}
		if req.MemoryID == "" {
			writeJSONError(w, http.StatusBadRequest, "memory id is required")
			return
		}
		if !req.Apply && (req.ExpectedVersion != 0 || req.ScrubSetRevision != "" || req.ReasonCode != "" || req.IdempotencyKey != "" || directUserAuthorization != "") {
			writeJSONError(w, http.StatusBadRequest, "deletion preview accepts no mutation guards")
			return
		}
		if req.Apply && (req.ExpectedVersion < 1 || req.ScrubSetRevision == "" || req.IdempotencyKey == "") {
			writeJSONError(w, http.StatusBadRequest, "expected_version, scrub_set_revision, and Idempotency-Key are required")
			return
		}
		if req.Apply && directUserAuthorization != "true" {
			writeJSONError(w, http.StatusForbidden, "direct current-user authorization is required for permanent memory deletion")
			return
		}
		receipt, err := deleteMemory(r.Context(), p, req)
		if writeDeleteMemoryError(w, err) {
			return
		}
		setMemoryResponseHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"deletion":       receipt,
		})
	})
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		protected(w, r)
	}
}

func writeDeleteMemoryError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrBadInput):
		writeJSONError(w, http.StatusBadRequest, "invalid memory deletion request")
	case errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusForbidden, "memory deletion forbidden")
	case errors.Is(err, ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "memory not found")
	case errors.Is(err, ErrMemoryDeleted):
		writeJSONError(w, http.StatusGone, "memory already permanently deleted")
	case errors.Is(err, ErrMemoryDependency):
		writeJSONError(w, http.StatusConflict, "memory has live dependencies")
	case errors.Is(err, ErrIdempotencyConflict):
		writeJSONError(w, http.StatusConflict, "idempotency key was reused for a different memory deletion")
	case errors.Is(err, ErrConflict):
		writeJSONError(w, http.StatusConflict, "memory changed since deletion preview")
	default:
		writeJSONError(w, http.StatusInternalServerError, "could not permanently delete memory")
	}
	return true
}

func memoryListOptions(r *http.Request) (MemoryListOptions, error) {
	q := r.URL.Query()
	opts := MemoryListOptions{
		OwnerAgentID:  strings.TrimSpace(q.Get("owner_agent_id")),
		State:         strings.TrimSpace(q.Get("state")),
		Kind:          strings.TrimSpace(q.Get("kind")),
		Tags:          q["tag"],
		Origin:        strings.TrimSpace(q.Get("origin")),
		CaptureReason: strings.TrimSpace(q.Get("capture_reason")),
		Cursor:        strings.TrimSpace(q.Get("cursor")),
	}
	var err error
	if opts.IncludeSensitive, err = optionalMemoryBool(q.Get("include_sensitive")); err != nil {
		return MemoryListOptions{}, errors.New("include_sensitive must be true or false")
	}
	if opts.Limit, err = optionalMemoryLimit(q.Get("limit")); err != nil {
		return MemoryListOptions{}, err
	}
	for _, item := range []struct {
		name string
		dst  **time.Time
	}{
		{"occurred_from", &opts.OccurredFrom},
		{"occurred_until", &opts.OccurredUntil},
		{"captured_from", &opts.CapturedFrom},
		{"captured_until", &opts.CapturedUntil},
	} {
		value, parseErr := optionalMemoryTime(q.Get(item.name))
		if parseErr != nil {
			return MemoryListOptions{}, errors.New(item.name + " must be RFC3339")
		}
		*item.dst = value
	}
	return opts, nil
}

func memoryHistoryOptions(r *http.Request) (MemoryHistoryOptions, error) {
	limit, err := optionalMemoryLimit(r.URL.Query().Get("limit"))
	if err != nil {
		return MemoryHistoryOptions{}, err
	}
	return MemoryHistoryOptions{Limit: limit, Cursor: strings.TrimSpace(r.URL.Query().Get("cursor"))}, nil
}

func optionalMemoryLimit(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 500 {
		return 0, errors.New("limit must be between 1 and 500")
	}
	return limit, nil
}

func optionalMemoryBool(raw string) (bool, error) {
	if raw == "" {
		return false, nil
	}
	return strconv.ParseBool(raw)
}

func optionalMemoryTime(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func parseMemoryAction(raw string) (memoryID, action string, ok bool) {
	idx := strings.LastIndexByte(raw, ':')
	if idx <= 0 || idx == len(raw)-1 {
		return "", "", false
	}
	memoryID = strings.TrimSpace(raw[:idx])
	action = strings.TrimSpace(raw[idx+1:])
	if memoryID == "" {
		return "", "", false
	}
	switch action {
	case "forget", "restore", "reactivate":
		return memoryID, action, true
	default:
		return "", "", false
	}
}

func setMemoryIdempotencyKey(w http.ResponseWriter, r *http.Request, dst *string) bool {
	*dst = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if *dst == "" {
		writeJSONError(w, http.StatusBadRequest, "Idempotency-Key header is required")
		return false
	}
	return true
}

func writeMemoryError(w http.ResponseWriter, err error, operation string) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrBadInput):
		writeJSONError(w, http.StatusBadRequest, "invalid memory request")
	case errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusForbidden, "memory access forbidden")
	case errors.Is(err, ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "memory not found")
	case errors.Is(err, ErrMemoryDeleted):
		writeJSONError(w, http.StatusGone, "memory was permanently deleted")
	case errors.Is(err, ErrConflict):
		writeJSONError(w, http.StatusConflict, "memory version conflict")
	case errors.Is(err, ErrIdempotencyConflict):
		writeJSONError(w, http.StatusConflict, "idempotency key was reused for a different memory mutation")
	default:
		writeJSONError(w, http.StatusInternalServerError, "could not "+operation)
	}
	return true
}

func writeMemoryResult(w http.ResponseWriter, status int, result MemoryMutationResult) {
	normalizeMemory(&result.Memory)
	setMemoryResponseHeaders(w)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema_version": "witself.v0",
		"memory":         result.Memory,
		"receipt":        result.Receipt,
	})
}

func normalizeMemory(memory *Memory) {
	if memory.ContentEncoding == "" {
		memory.ContentEncoding = "plain"
	}
	if memory.Tags == nil {
		memory.Tags = []string{}
	}
	if memory.Links == nil {
		memory.Links = []string{}
	}
	if memory.Evidence == nil {
		memory.Evidence = []MemoryEvidence{}
	}
}

// redactMemoryForBroadResponse is a transport-boundary backstop. Store-backed
// callbacks already scrub sensitive list and recall results, but alternative
// backends must not be able to leak caller-authored values by returning an
// unredacted sensitive record.
func redactMemoryForBroadResponse(memory *Memory) {
	memory.Content = ""
	memory.ContentHash = ""
	memory.Tags = []string{}
	memory.Links = []string{}
	memory.CaptureReason = ""
	memory.StateReason = ""
	memory.OccurredFrom = nil
	memory.OccurredUntil = nil
	memory.Client = MemoryClientProvenance{}
	memory.Evidence = []MemoryEvidence{}
	memory.Redacted = true
}

func normalizeMemoryVersion(version *MemoryVersion) {
	if version.ContentEncoding == "" {
		version.ContentEncoding = "plain"
	}
	if version.Tags == nil {
		version.Tags = []string{}
	}
	if version.Links == nil {
		version.Links = []string{}
	}
	if version.Evidence == nil {
		version.Evidence = []MemoryEvidence{}
	}
}

func setMemoryResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
}
