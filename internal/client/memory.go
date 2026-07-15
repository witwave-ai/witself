package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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

// MemoryClientProvenance is self-reported client inference provenance. The
// server stores it as metadata and never treats it as authentication input.
type MemoryClientProvenance struct {
	Runtime       string `json:"runtime,omitempty"`
	Model         string `json:"model,omitempty"`
	Recipe        string `json:"recipe,omitempty"`
	RecipeVersion string `json:"recipe_version,omitempty"`
}

// MemoryEvidenceInput attaches evidence to a newly captured memory. State is
// resolved, pending, or unavailable. The state determines which locator fields
// are legal; the server validates the state-specific shape.
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

// Memory is the stable resource and its current immutable version, flattened
// for the ordinary read/list/mutation surfaces.
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

// MemoryMutationReceipt is a value-free retry receipt. It is safe to persist
// locally because it never contains memory or evidence content.
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

// MemoryMutationResult is returned by capture and lifecycle mutations.
type MemoryMutationResult struct {
	Memory  Memory                `json:"memory"`
	Receipt MemoryMutationReceipt `json:"receipt"`
}

// MemoryVersionReference is a value-free exact version locator used by an
// atomic supersession receipt.
type MemoryVersionReference struct {
	MemoryID string `json:"memory_id"`
	Version  int64  `json:"version"`
}

// MemorySupersessionReceipt identifies an exact supersession set without
// repeating any memory or evidence values.
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

// SupersedeMemoryResult returns the full authorized source and replacement
// snapshots together with a value-free retry receipt.
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

// MemoryRecallInput is a structured lexical/time/tag retrieval request. The
// client may resolve natural-language dates before sending this shape.
type MemoryRecallInput struct {
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

// MemoryRecallScore contains the deterministic ranking components for one hit.
type MemoryRecallScore struct {
	Similarity float64 `json:"similarity"`
	VectorUsed bool    `json:"vector_used"`
	Lexical    float64 `json:"lexical"`
	Salience   float64 `json:"salience"`
	Recency    float64 `json:"recency"`
	Total      float64 `json:"total"`
}

// MemoryRecallHit pairs a recalled memory with its ranking score.
type MemoryRecallHit struct {
	Memory Memory            `json:"memory"`
	Score  MemoryRecallScore `json:"score"`
}

// MemoryRecallPage is one opaque-cursor page of ranked memories and retrieval
// diagnostics.
type MemoryRecallPage struct {
	SchemaVersion      string            `json:"schema_version,omitempty"`
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

// CaptureMemoryInput carries a client-authored narrative capsule.
type CaptureMemoryInput struct {
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

// SupersedeMemoryReplacementInput is one client-authored replacement. Its
// retry key is part of the JSON body because all replacements commit together.
type SupersedeMemoryReplacementInput struct {
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

// SupersedeMemoryInput atomically replaces one expected active version. The
// operation retry key is sent as Idempotency-Key, never inside the body.
type SupersedeMemoryInput struct {
	MemoryID        string                            `json:"-"`
	ExpectedVersion int64                             `json:"expected_version"`
	Replacements    []SupersedeMemoryReplacementInput `json:"replacements"`
	Reason          string                            `json:"reason,omitempty"`
	Client          MemoryClientProvenance            `json:"client,omitempty"`
	IdempotencyKey  string                            `json:"-"`
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

// MemoryHistoryOptions selects one bounded page of immutable versions.
type MemoryHistoryOptions struct {
	Limit  int
	Cursor string
}

// AdjustMemoryInput applies a sparse patch against an expected current
// version. A nil set field leaves that property unchanged.
type AdjustMemoryInput struct {
	MemoryID           string                 `json:"-"`
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

// MemoryLifecycleInput carries the optimistic-concurrency and retry guards
// for forget, restore, and reactivate.
type MemoryLifecycleInput struct {
	MemoryID                        string                 `json:"-"`
	ExpectedVersion                 int64                  `json:"expected_version"`
	ExpectedSupersessionSetRevision *int64                 `json:"expected_supersession_set_revision,omitempty"`
	Reason                          string                 `json:"reason,omitempty"`
	Client                          MemoryClientProvenance `json:"client,omitempty"`
	IdempotencyKey                  string                 `json:"-"`
}

// ResolveMemoryEvidenceInput terminates one pending evidence locator with one
// exact source or an explicit unresolvable reason.
type ResolveMemoryEvidenceInput struct {
	EvidenceID          string `json:"-"`
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

// MemoryDeletionReceipt is a value-free permanent-deletion preview or apply
// receipt. It deliberately contains no memory content, evidence locators,
// client provenance, raw retry keys, or content-derived hashes.
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

// DeleteMemoryInput binds an apply or exact replay to the value-free guards
// returned by PreviewDeleteMemory.
type DeleteMemoryInput struct {
	MemoryID             string
	ExpectedVersion      int64
	ScrubSetRevision     string
	IdempotencyKey       string
	DirectUserAuthorized bool
}

// CaptureMemory persists one client-authored narrative capsule.
func CaptureMemory(ctx context.Context, endpoint, token string, in CaptureMemoryInput) (*MemoryMutationResult, error) {
	var out MemoryMutationResult
	if err := doMemoryMutation(ctx, http.MethodPost, memoriesURL(endpoint), token, in.IdempotencyKey, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMemory reads one current memory head by stable id.
func GetMemory(ctx context.Context, endpoint, token, memoryID string) (*Memory, error) {
	var out struct {
		Memory Memory `json:"memory"`
	}
	if err := doJSON(ctx, http.MethodGet, memoryURL(endpoint, memoryID), token, nil, &out); err != nil {
		return nil, err
	}
	return &out.Memory, nil
}

// ListMemories retrieves one bounded page of current memory heads.
func ListMemories(ctx context.Context, endpoint, token string, opts MemoryListOptions) (*MemoryPage, error) {
	q := url.Values{}
	if opts.OwnerAgentID != "" {
		q.Set("owner_agent_id", opts.OwnerAgentID)
	}
	if opts.State != "" {
		q.Set("state", opts.State)
	}
	if opts.Kind != "" {
		q.Set("kind", opts.Kind)
	}
	for _, tag := range opts.Tags {
		q.Add("tag", tag)
	}
	if opts.Origin != "" {
		q.Set("origin", opts.Origin)
	}
	if opts.CaptureReason != "" {
		q.Set("capture_reason", opts.CaptureReason)
	}
	if opts.IncludeSensitive {
		q.Set("include_sensitive", "true")
	}
	setMemoryTimeQuery(q, "occurred_from", opts.OccurredFrom)
	setMemoryTimeQuery(q, "occurred_until", opts.OccurredUntil)
	setMemoryTimeQuery(q, "captured_from", opts.CapturedFrom)
	setMemoryTimeQuery(q, "captured_until", opts.CapturedUntil)
	if opts.Limit != 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	requestURL := memoriesURL(endpoint)
	if len(q) != 0 {
		requestURL += "?" + q.Encode()
	}
	var out MemoryPage
	if err := doJSON(ctx, http.MethodGet, requestURL, token, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RecallMemories performs deterministic backend ranking without requiring a
// server-side inference or embedding provider.
func RecallMemories(ctx context.Context, endpoint, token string, in MemoryRecallInput) (*MemoryRecallPage, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out MemoryRecallPage
	if err := doJSON(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/v1/memories:recall", token, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMemoryHistory retrieves immutable versions for one memory.
func GetMemoryHistory(ctx context.Context, endpoint, token, memoryID string, opts MemoryHistoryOptions) (*MemoryHistoryPage, error) {
	q := url.Values{}
	if opts.Limit != 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	requestURL := memoryURL(endpoint, memoryID) + "/history"
	if len(q) != 0 {
		requestURL += "?" + q.Encode()
	}
	var out MemoryHistoryPage
	if err := doJSON(ctx, http.MethodGet, requestURL, token, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AdjustMemory appends a sparse, expected-version guarded memory update.
func AdjustMemory(ctx context.Context, endpoint, token string, in AdjustMemoryInput) (*MemoryMutationResult, error) {
	var out MemoryMutationResult
	if err := doMemoryMutation(ctx, http.MethodPatch, memoryURL(endpoint, in.MemoryID), token, in.IdempotencyKey, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SupersedeMemory atomically replaces one exact active memory version with a
// nonempty client-authored replacement set.
func SupersedeMemory(ctx context.Context, endpoint, token string, in SupersedeMemoryInput) (*SupersedeMemoryResult, error) {
	var out SupersedeMemoryResult
	requestURL := memoryURL(endpoint, in.MemoryID) + "/supersede"
	if err := doMemoryMutation(ctx, http.MethodPost, requestURL, token, in.IdempotencyKey, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ForgetMemory appends a forgotten state version.
func ForgetMemory(ctx context.Context, endpoint, token string, in MemoryLifecycleInput) (*MemoryMutationResult, error) {
	return memoryLifecycle(ctx, endpoint, token, "forget", in)
}

// RestoreMemory restores a forgotten memory to its valid prior state.
func RestoreMemory(ctx context.Context, endpoint, token string, in MemoryLifecycleInput) (*MemoryMutationResult, error) {
	return memoryLifecycle(ctx, endpoint, token, "restore", in)
}

// ReactivateMemory explicitly makes a reverted or invalidly-restorable memory
// active. Superseded memories also require the supersession-set revision.
func ReactivateMemory(ctx context.Context, endpoint, token string, in MemoryLifecycleInput) (*MemoryMutationResult, error) {
	return memoryLifecycle(ctx, endpoint, token, "reactivate", in)
}

// ResolveMemoryEvidence appends one terminal result without mutating the
// original pending evidence row.
func ResolveMemoryEvidence(ctx context.Context, endpoint, token string, in ResolveMemoryEvidenceInput) (*MemoryEvidence, error) {
	var out struct {
		Evidence MemoryEvidence `json:"evidence"`
	}
	requestURL := strings.TrimRight(endpoint, "/") + "/v1/memory-evidence/" + url.PathEscape(in.EvidenceID) + "/resolution"
	if err := doMemoryMutation(ctx, http.MethodPost, requestURL, token, in.IdempotencyKey, in, &out); err != nil {
		return nil, err
	}
	return &out.Evidence, nil
}

// PreviewDeleteMemory returns the value-free physical scrub set and
// concurrency guards without retrieving or exposing the memory value.
func PreviewDeleteMemory(ctx context.Context, endpoint, token, memoryID string) (*MemoryDeletionReceipt, error) {
	q := url.Values{"dry_run": {"true"}}
	requestURL := memoryURL(endpoint, memoryID) + "?" + q.Encode()
	var out struct {
		Deletion MemoryDeletionReceipt `json:"deletion"`
	}
	if err := doJSONWithHeaders(ctx, http.MethodDelete, requestURL, token, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Deletion, nil
}

// DeleteMemory permanently scrubs one exact narrative memory while retaining
// only a value-free tombstone and retry shields. Callers must have direct
// current-user authority before invoking this irreversible operation.
func DeleteMemory(ctx context.Context, endpoint, token string, in DeleteMemoryInput) (*MemoryDeletionReceipt, error) {
	q := url.Values{
		"expected_version":   {strconv.FormatInt(in.ExpectedVersion, 10)},
		"scrub_set_revision": {in.ScrubSetRevision},
	}
	requestURL := memoryURL(endpoint, in.MemoryID) + "?" + q.Encode()
	var out struct {
		Deletion MemoryDeletionReceipt `json:"deletion"`
	}
	headers := map[string]string{"Idempotency-Key": in.IdempotencyKey}
	if in.DirectUserAuthorized {
		headers["X-Witself-Direct-User-Authorized"] = "true"
	}
	if err := doJSONWithHeaders(ctx, http.MethodDelete, requestURL, token, headers, nil, &out); err != nil {
		return nil, err
	}
	return &out.Deletion, nil
}

func memoryLifecycle(ctx context.Context, endpoint, token, action string, in MemoryLifecycleInput) (*MemoryMutationResult, error) {
	var out MemoryMutationResult
	requestURL := memoryURL(endpoint, in.MemoryID) + ":" + action
	if err := doMemoryMutation(ctx, http.MethodPost, requestURL, token, in.IdempotencyKey, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func doMemoryMutation(ctx context.Context, method, requestURL, token, idempotencyKey string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return doJSONWithHeaders(ctx, method, requestURL, token,
		map[string]string{"Idempotency-Key": idempotencyKey}, body, out)
}

func setMemoryTimeQuery(q url.Values, name string, value *time.Time) {
	if value != nil {
		q.Set(name, value.UTC().Format(time.RFC3339Nano))
	}
}

func memoriesURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/memories"
}

func memoryURL(endpoint, memoryID string) string {
	return memoriesURL(endpoint) + "/" + url.PathEscape(memoryID)
}
