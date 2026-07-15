package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/id"
)

const (
	// MemoryStateActive identifies a memory eligible for ordinary recall.
	MemoryStateActive = "active"
	// MemoryStateSuperseded identifies a memory replaced by newer memories.
	MemoryStateSuperseded = "superseded"
	// MemoryStateForgotten identifies a memory removed from ordinary recall.
	MemoryStateForgotten = "forgotten"
	// MemoryStateReverted identifies a version compensated by curation rollback.
	MemoryStateReverted = "reverted"

	// MemoryEvidencePending identifies evidence awaiting deterministic resolution.
	MemoryEvidencePending = "pending"
	// MemoryEvidenceResolved identifies evidence bound to a durable source.
	MemoryEvidenceResolved = "resolved"
	// MemoryEvidenceUnresolvable identifies evidence that cannot be bound to a source.
	MemoryEvidenceUnresolvable = "unresolvable"
	// MemoryEvidenceUnavailable identifies evidence that the runtime did not retain.
	MemoryEvidenceUnavailable = "unavailable"

	// MemoryEvidenceSupports identifies evidence supporting the memory content.
	MemoryEvidenceSupports = "supports"
	// MemoryEvidenceContradicts identifies evidence contradicting the memory content.
	MemoryEvidenceContradicts = "contradicts"
	// MemoryEvidenceContext identifies evidence supplying non-assertive context.
	MemoryEvidenceContext = "context"

	defaultMemoryPageSize = 50
	maxMemoryPageSize     = 500
	maxMemoryContentBytes = 256 * 1024
	maxMemoryChangeSeq    = int64(1<<62 - 1)

	// Imports retain a bounded reserve so a valid restore cannot immediately
	// exhaust the owner's mutation lane. Runtime allocation may consume that
	// reserve up to maxMemoryChangeSeq, then fails closed before BIGINT overflow.
	memoryChangeSeqImportReserve = int64(1_000_000)
	maxImportedMemoryChangeSeq   = maxMemoryChangeSeq - memoryChangeSeqImportReserve
)

var (
	// ErrMemoryNotFound reports a memory outside the token-bound collection.
	ErrMemoryNotFound = errors.New("memory not found")
	// ErrMemoryForbidden reports a principal that cannot use agent memory.
	ErrMemoryForbidden = errors.New("memory access forbidden")
	// ErrMemoryInputInvalid reports caller-correctable memory input.
	ErrMemoryInputInvalid = errors.New("invalid memory input")
	// ErrMemoryConflict reports a stale expected version or invalid lifecycle
	// transition. The caller must reread the current head before retrying.
	ErrMemoryConflict = errors.New("memory changed or lifecycle transition conflicts")
	// ErrMemoryIdempotencyConflict reports reuse of a mutation retry key with
	// different normalized input.
	ErrMemoryIdempotencyConflict = errors.New("memory idempotency key conflict")
	// ErrMemoryEvidenceConflict reports an already-terminal pending locator or
	// an evidence retry whose normalized input differs.
	ErrMemoryEvidenceConflict = errors.New("memory evidence resolution conflict")
	// ErrMemoryDeleted reports a retry key whose original result was purged by
	// permanent deletion. Delayed retries may not recreate deleted content.
	ErrMemoryDeleted = errors.New("memory was permanently deleted")
	// ErrMemoryChangeSeqExhausted reports an owner mutation lane that reached
	// its explicit sequence ceiling. It is a domain conflict, never a database
	// integer-overflow error.
	ErrMemoryChangeSeqExhausted = errors.New("memory change sequence exhausted")

	memoryKindPattern     = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	memoryEvidencePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	memoryCodePattern     = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

// MemoryClientProvenance is self-reported client metadata. It is diagnostic
// provenance only and never grants authority.
type MemoryClientProvenance struct {
	Runtime       string `json:"runtime,omitempty"`
	Model         string `json:"model,omitempty"`
	Recipe        string `json:"recipe,omitempty"`
	RecipeVersion string `json:"recipe_version,omitempty"`
}

// Memory is one stable memory identity joined to one immutable version. Exact
// reads return Content even when Sensitive; list pages redact it by default.
type Memory struct {
	ID                string     `json:"id"`
	AccountID         string     `json:"account_id"`
	RealmID           string     `json:"realm_id"`
	OwnerKind         string     `json:"owner_kind"`
	OwnerID           string     `json:"owner_id"`
	Origin            string     `json:"origin"`
	CaptureReason     string     `json:"capture_reason,omitempty"`
	AuthoredByAgentID string     `json:"authored_by_agent_id"`
	Version           int64      `json:"version"`
	PreviousVersion   int64      `json:"previous_version,omitempty"`
	ChangeSeq         int64      `json:"change_seq"`
	Content           string     `json:"content,omitempty"`
	ContentEncoding   string     `json:"content_encoding"`
	Kind              string     `json:"kind"`
	Tags              []string   `json:"tags"`
	Links             []string   `json:"links"`
	Salience          float64    `json:"salience"`
	Sensitive         bool       `json:"sensitive"`
	Redacted          bool       `json:"redacted,omitempty"`
	OccurredFrom      *time.Time `json:"occurred_from,omitempty"`
	OccurredUntil     *time.Time `json:"occurred_until,omitempty"`
	State             string     `json:"state"`
	PriorState        string     `json:"prior_state,omitempty"`
	LifecycleReason   string     `json:"lifecycle_reason,omitempty"`
	ContentHash       string     `json:"content_hash"`
	ActorKind         string     `json:"actor_kind"`
	ActorID           string     `json:"actor_id"`
	Operation         string     `json:"operation"`
	IdempotencyKey    string     `json:"idempotency_key"`
	RequestHash       string     `json:"request_hash"`
	CurationRunID     string     `json:"curation_run_id,omitempty"`
	CurationActionID  string     `json:"curation_action_id,omitempty"`
	// Supersession* is the immutable value-free receipt on the exact version
	// created by atomic supersede. ActiveSupersession* is the current,
	// relation-derived view for the stable memory identity.
	SupersessionSetID             string                 `json:"supersession_set_id,omitempty"`
	SupersessionSetRevision       int64                  `json:"supersession_set_revision,omitempty"`
	SupersessionReplacementCount  int64                  `json:"supersession_replacement_count,omitempty"`
	SupersessionReplacementDigest string                 `json:"supersession_replacement_digest,omitempty"`
	ActiveSupersessionSetID       string                 `json:"active_supersession_set_id,omitempty"`
	ActiveSupersessionSetRevision int64                  `json:"active_supersession_set_revision,omitempty"`
	Client                        MemoryClientProvenance `json:"client"`
	Evidence                      []MemoryEvidence       `json:"evidence,omitempty"`
	CreatedAt                     time.Time              `json:"created_at"`
	UpdatedAt                     time.Time              `json:"updated_at"`
}

// MemoryVersion is an alias because history rows carry the same full snapshot
// and stable identity metadata as exact reads.
type MemoryVersion = Memory

// MemoryEvidence is one append-only evidence observation or resolution.
type MemoryEvidence struct {
	ID                  string    `json:"id"`
	MemoryID            string    `json:"memory_id"`
	TargetVersion       int64     `json:"target_version"`
	EvidenceChangeSeq   int64     `json:"evidence_change_seq"`
	Type                string    `json:"type"`
	Role                string    `json:"role"`
	ResolutionState     string    `json:"resolution_state"`
	ExternalLocator     string    `json:"external_locator,omitempty"`
	PendingEvidenceID   string    `json:"pending_evidence_id,omitempty"`
	ResolvedKind        string    `json:"resolved_kind,omitempty"`
	SourceTranscriptID  string    `json:"source_transcript_id,omitempty"`
	SourceSequenceFrom  int64     `json:"source_sequence_from,omitempty"`
	SourceSequenceUntil int64     `json:"source_sequence_until,omitempty"`
	SourceMemoryID      string    `json:"source_memory_id,omitempty"`
	SourceMemoryVersion int64     `json:"source_memory_version,omitempty"`
	SourceMessageID     string    `json:"source_message_id,omitempty"`
	SourceImportLocator string    `json:"source_import_locator,omitempty"`
	ArtifactExcerpt     []byte    `json:"artifact_excerpt,omitempty"`
	ArtifactSensitive   bool      `json:"artifact_sensitive"`
	TerminalReasonCode  string    `json:"terminal_reason_code,omitempty"`
	SourceDigest        string    `json:"source_digest,omitempty"`
	ActorID             string    `json:"actor_id"`
	IdempotencyKey      string    `json:"idempotency_key,omitempty"`
	RequestHash         string    `json:"request_hash,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
}

// MemoryEvidenceInput is accepted at capture. Capture must include at least one
// exact, pending, or explicitly unavailable evidence row.
type MemoryEvidenceInput struct {
	Type                string `json:"type"`
	Role                string `json:"role,omitempty"`
	ResolutionState     string `json:"resolution_state"`
	ExternalLocator     string `json:"external_locator,omitempty"`
	ResolvedKind        string `json:"resolved_kind,omitempty"`
	SourceTranscriptID  string `json:"source_transcript_id,omitempty"`
	SourceSequenceFrom  int64  `json:"source_sequence_from,omitempty"`
	SourceSequenceUntil int64  `json:"source_sequence_until,omitempty"`
	SourceMemoryID      string `json:"source_memory_id,omitempty"`
	SourceMemoryVersion int64  `json:"source_memory_version,omitempty"`
	SourceMessageID     string `json:"source_message_id,omitempty"`
	SourceImportLocator string `json:"source_import_locator,omitempty"`
	ArtifactExcerpt     []byte `json:"artifact_excerpt,omitempty"`
	ArtifactSensitive   bool   `json:"artifact_sensitive"`
	TerminalReasonCode  string `json:"terminal_reason_code,omitempty"`
	SourceDigest        string `json:"source_digest,omitempty"`
}

// CaptureMemoryInput creates version one and its initial evidence atomically.
type CaptureMemoryInput struct {
	Content         string                 `json:"content"`
	ContentEncoding string                 `json:"content_encoding,omitempty"`
	Kind            string                 `json:"kind,omitempty"`
	Tags            []string               `json:"tags,omitempty"`
	Links           []string               `json:"links,omitempty"`
	Salience        *float64               `json:"salience,omitempty"`
	Sensitive       bool                   `json:"sensitive,omitempty"`
	OccurredFrom    *time.Time             `json:"occurred_from,omitempty"`
	OccurredUntil   *time.Time             `json:"occurred_until,omitempty"`
	Origin          string                 `json:"origin,omitempty"`
	CaptureReason   string                 `json:"capture_reason,omitempty"`
	Evidence        []MemoryEvidenceInput  `json:"evidence"`
	Client          MemoryClientProvenance `json:"client,omitempty"`
	IdempotencyKey  string                 `json:"idempotency_key"`
}

// AdjustMemoryInput carries a partial replacement. Set Tags/Links for an exact
// replacement or use Add/Remove, but not both forms for one field.
type AdjustMemoryInput struct {
	ExpectedVersion    int64                  `json:"expected_version"`
	Content            *string                `json:"set_content,omitempty"`
	ContentEncoding    *string                `json:"set_content_encoding,omitempty"`
	Kind               *string                `json:"set_kind,omitempty"`
	Tags               *[]string              `json:"set_tags,omitempty"`
	AddTags            []string               `json:"add_tags,omitempty"`
	RemoveTags         []string               `json:"remove_tags,omitempty"`
	Links              *[]string              `json:"set_links,omitempty"`
	AddLinks           []string               `json:"add_links,omitempty"`
	RemoveLinks        []string               `json:"remove_links,omitempty"`
	Salience           *float64               `json:"set_salience,omitempty"`
	Sensitive          *bool                  `json:"set_sensitive,omitempty"`
	OccurredFrom       *time.Time             `json:"set_occurred_from,omitempty"`
	ClearOccurredFrom  bool                   `json:"clear_occurred_from,omitempty"`
	OccurredUntil      *time.Time             `json:"set_occurred_until,omitempty"`
	ClearOccurredUntil bool                   `json:"clear_occurred_until,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	Client             MemoryClientProvenance `json:"client,omitempty"`
	IdempotencyKey     string                 `json:"idempotency_key"`
}

// MemoryLifecycleInput is shared by forget, restore, and reactivate.
type MemoryLifecycleInput struct {
	ExpectedVersion                 int64                  `json:"expected_version"`
	ExpectedSupersessionSetRevision *int64                 `json:"expected_supersession_set_revision,omitempty"`
	Reason                          string                 `json:"reason,omitempty"`
	Client                          MemoryClientProvenance `json:"client,omitempty"`
	IdempotencyKey                  string                 `json:"idempotency_key"`
}

// ResolveMemoryEvidenceInput deterministically terminates one pending locator.
// Set UnresolvableReason instead of ResolvedKind when the locator cannot resolve.
type ResolveMemoryEvidenceInput struct {
	ResolvedKind        string `json:"resolved_kind,omitempty"`
	SourceTranscriptID  string `json:"source_transcript_id,omitempty"`
	SourceSequenceFrom  int64  `json:"source_sequence_from,omitempty"`
	SourceSequenceUntil int64  `json:"source_sequence_until,omitempty"`
	SourceMemoryID      string `json:"source_memory_id,omitempty"`
	SourceMemoryVersion int64  `json:"source_memory_version,omitempty"`
	SourceMessageID     string `json:"source_message_id,omitempty"`
	SourceImportLocator string `json:"source_import_locator,omitempty"`
	ArtifactExcerpt     []byte `json:"artifact_excerpt,omitempty"`
	ArtifactSensitive   bool   `json:"artifact_sensitive"`
	UnresolvableReason  string `json:"unresolvable_reason,omitempty"`
	SourceDigest        string `json:"source_digest,omitempty"`
	IdempotencyKey      string `json:"idempotency_key"`
}

// MemoryMutationReceipt is a value-free exact-retry receipt.
type MemoryMutationReceipt struct {
	Operation      string    `json:"operation"`
	ActorID        string    `json:"actor_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	RequestHash    string    `json:"request_hash"`
	MemoryID       string    `json:"memory_id"`
	Version        int64     `json:"version"`
	ChangeSeq      int64     `json:"change_seq"`
	CreatedAt      time.Time `json:"created_at"`
	Replayed       bool      `json:"replayed,omitempty"`
}

// MemoryMutationResult pairs the resulting memory snapshot with its durable,
// value-free mutation receipt.
type MemoryMutationResult struct {
	Memory  Memory                `json:"memory"`
	Receipt MemoryMutationReceipt `json:"receipt"`
}

// MemoryListOptions selects and paginates an owner-local memory collection.
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
	OrderBySalience  bool
	Limit            int
	Cursor           string
	AfterSalience    *float64
	AfterUpdatedAt   *time.Time
	AfterID          string
}

// MemoryPage contains one bounded page of memories and its continuation state.
type MemoryPage struct {
	Memories      []Memory
	NextCursor    string
	NextUpdatedAt *time.Time
	NextID        string
}

// MemoryHistoryOptions selects one bounded, oldest-first page of immutable
// versions. Cursor is the public continuation token; AfterVersion exists for
// trusted in-process callers and may not be combined with Cursor.
type MemoryHistoryOptions struct {
	Limit        int
	Cursor       string
	AfterVersion int64
}

// MemoryHistoryPage is one bounded page of immutable memory versions. The
// evidence for all returned versions is loaded in one batched query.
type MemoryHistoryPage struct {
	Versions    []MemoryVersion
	NextCursor  string
	NextVersion int64
}

// CaptureMemory commits a durable version-one memory and evidence capsule.
func (s *Store) CaptureMemory(ctx context.Context, p Principal, in CaptureMemoryInput) (MemoryMutationResult, error) {
	if p.Kind != PrincipalAgent {
		return MemoryMutationResult{}, ErrMemoryForbidden
	}
	in, err := normalizeCaptureMemoryInput(in)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	requestHash, err := memoryRequestHash(struct {
		Operation string             `json:"operation"`
		Input     CaptureMemoryInput `json:"input"`
	}{Operation: "added", Input: in})
	if err != nil {
		return MemoryMutationResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return MemoryMutationResult{}, err
	}
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MemoryMutationResult{}, err
	}
	if replay, ok, err := memoryMutationReplay(ctx, tx, p, in.IdempotencyKey, requestHash, "added"); err != nil || ok {
		return replay, err
	}
	lane, err := lockMemoryCurationSourceLaneTx(ctx, tx, p)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	// Recheck after the owner lane lock. A concurrent exact source retry may
	// have committed while this transaction waited for the lane.
	if replay, ok, err := memoryMutationReplay(ctx, tx, p, in.IdempotencyKey, requestHash, "added"); err != nil || ok {
		return replay, err
	}
	if err := markMemoryCurationDueTx(ctx, tx, p, &lane,
		"memory_changed", MemoryCurationSourceMemory, in.IdempotencyKey); err != nil {
		return MemoryMutationResult{}, err
	}
	changeSeq, err := allocateMemoryChangeSeq(ctx, tx, p)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	// The owner clock serializes mutations. Recheck after obtaining it so two
	// concurrent first attempts cannot turn a safe retry into a unique error.
	if replay, ok, err := memoryMutationReplay(ctx, tx, p, in.IdempotencyKey, requestHash, "added"); err != nil || ok {
		return replay, err
	}
	memoryID, err := id.New("mem")
	if err != nil {
		return MemoryMutationResult{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO memories
		  (id, account_id, realm_id, owner_kind, owner_id, origin,
		   capture_reason, authored_by_agent_id, current_version)
		VALUES ($1,$2,$3,'agent',$4,$5,$6,$4,1)`,
		memoryID, p.AccountID, p.RealmID, p.ID, in.Origin, in.CaptureReason)
	if err != nil {
		return MemoryMutationResult{}, fmt.Errorf("insert memory head: %w", err)
	}
	snapshot := Memory{
		ID: memoryID, AccountID: p.AccountID, RealmID: p.RealmID,
		OwnerKind: "agent", OwnerID: p.ID, Origin: in.Origin,
		CaptureReason: in.CaptureReason, AuthoredByAgentID: p.ID,
		Version: 1, ChangeSeq: changeSeq, Content: in.Content,
		ContentEncoding: in.ContentEncoding, Kind: in.Kind, Tags: in.Tags,
		Links: in.Links, Salience: *in.Salience, Sensitive: in.Sensitive,
		OccurredFrom: in.OccurredFrom, OccurredUntil: in.OccurredUntil,
		State: MemoryStateActive, ContentHash: memoryContentHash(in.Content),
		ActorKind: "agent", ActorID: p.ID, Operation: "added",
		IdempotencyKey: in.IdempotencyKey, RequestHash: requestHash,
		Client: in.Client,
	}
	createdAt, err := insertMemoryVersionTx(ctx, tx, snapshot)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	for i := range in.Evidence {
		if err := validateMemoryEvidenceSourceTx(ctx, tx, p, in.Evidence[i]); err != nil {
			return MemoryMutationResult{}, fmt.Errorf("evidence %d: %w", i, err)
		}
		if _, err := insertMemoryEvidenceTx(ctx, tx, p, memoryID, 1, in.Evidence[i], "", ""); err != nil {
			return MemoryMutationResult{}, fmt.Errorf("evidence %d: %w", i, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE memories SET updated_at=$2 WHERE id=$1`, memoryID, createdAt); err != nil {
		return MemoryMutationResult{}, fmt.Errorf("update memory head timestamp: %w", err)
	}
	out, err := loadMemoryAtVersion(ctx, tx, p, memoryID, 1, true)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	if err := logMemoryVersionEventTx(ctx, tx, p, out); err != nil {
		return MemoryMutationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MemoryMutationResult{}, err
	}
	return memoryMutationResult(out), nil
}

// GetMemory returns the current head with full authorized content and evidence.
func (s *Store) GetMemory(ctx context.Context, p Principal, memoryID string) (Memory, error) {
	if p.Kind != PrincipalAgent {
		return Memory{}, ErrMemoryForbidden
	}
	if !validMemoryID(memoryID) {
		return Memory{}, ErrMemoryInputInvalid
	}
	return loadCurrentMemory(ctx, s.pool, p, memoryID, true)
}

// ListMemories returns deterministic current-head inventory ordered by activity.
func (s *Store) ListMemories(ctx context.Context, p Principal, opts MemoryListOptions) (MemoryPage, error) {
	if p.Kind != PrincipalAgent {
		return MemoryPage{}, ErrMemoryForbidden
	}
	var err error
	opts, err = normalizeMemoryListOptions(p, opts)
	if err != nil {
		return MemoryPage{}, err
	}
	args := []any{p.AccountID, p.RealmID, p.ID}
	where := []string{"m.account_id=$1", "m.realm_id=$2", "m.owner_kind='agent'", "m.owner_id=$3", "m.current_version IS NOT NULL"}
	where = append(where, "v.version=m.current_version")
	addArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	where = append(where, "v.state="+addArg(opts.State))
	if opts.Kind != "" {
		where = append(where, "v.kind="+addArg(opts.Kind))
	}
	if opts.Origin != "" {
		where = append(where, "m.origin="+addArg(opts.Origin))
	}
	if opts.CaptureReason != "" {
		where = append(where, "m.capture_reason="+addArg(opts.CaptureReason))
	}
	if len(opts.Tags) > 0 {
		raw, _ := json.Marshal(opts.Tags)
		where = append(where, "v.tags @> "+addArg(string(raw))+"::jsonb")
	}
	if opts.OccurredFrom != nil {
		where = append(where,
			"COALESCE(v.occurred_until,v.occurred_from) >= "+addArg(*opts.OccurredFrom))
	}
	if opts.OccurredUntil != nil {
		where = append(where,
			"COALESCE(v.occurred_from,v.occurred_until) <= "+addArg(*opts.OccurredUntil))
	}
	if opts.CapturedFrom != nil {
		where = append(where, "m.created_at >= "+addArg(*opts.CapturedFrom))
	}
	if opts.CapturedUntil != nil {
		where = append(where, "m.created_at <= "+addArg(*opts.CapturedUntil))
	}
	if opts.AfterUpdatedAt != nil {
		at := addArg(*opts.AfterUpdatedAt)
		idArg := addArg(opts.AfterID)
		if opts.OrderBySalience {
			salienceArg := addArg(*opts.AfterSalience)
			where = append(where, fmt.Sprintf(
				"(v.salience, m.updated_at, m.id) < (%s, %s, %s)",
				salienceArg, at, idArg))
		} else {
			where = append(where, fmt.Sprintf("(m.updated_at, m.id) < (%s, %s)", at, idArg))
		}
	}
	limitArg := addArg(opts.Limit + 1)
	orderBy := "m.updated_at DESC, m.id DESC"
	if opts.OrderBySalience {
		orderBy = "v.salience DESC, m.updated_at DESC, m.id DESC"
	}
	rows, err := s.pool.Query(ctx, memorySelectSQL+`
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY `+orderBy+`
		LIMIT `+limitArg, args...)
	if err != nil {
		return MemoryPage{}, fmt.Errorf("list memories: %w", err)
	}
	defer rows.Close()
	page := MemoryPage{Memories: make([]Memory, 0, opts.Limit)}
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			return MemoryPage{}, err
		}
		if len(page.Memories) == opts.Limit {
			last := page.Memories[len(page.Memories)-1]
			page.NextUpdatedAt = &last.UpdatedAt
			page.NextID = last.ID
			page.NextCursor, err = encodeMemoryListCursorForOrder(
				last.UpdatedAt, last.ID, opts.OrderBySalience, last.Salience)
			if err != nil {
				return MemoryPage{}, err
			}
			break
		}
		if memory.Sensitive && !opts.IncludeSensitive {
			redactMemoryForBroadRead(&memory)
		}
		page.Memories = append(page.Memories, memory)
	}
	if err := rows.Err(); err != nil {
		return MemoryPage{}, fmt.Errorf("list memories: %w", err)
	}
	return page, nil
}

// redactMemoryForBroadRead removes every caller-authored value-bearing field
// that is not needed to identify or rank a sensitive memory. In particular,
// lifecycle reasons are free-form user text and must not leak merely because
// the content body itself was cleared.
func redactMemoryForBroadRead(memory *Memory) {
	memory.Content = ""
	memory.ContentHash = ""
	memory.Tags = []string{}
	memory.Links = []string{}
	memory.CaptureReason = ""
	memory.LifecycleReason = ""
	memory.OccurredFrom = nil
	memory.OccurredUntil = nil
	memory.IdempotencyKey = ""
	memory.RequestHash = ""
	memory.Client = MemoryClientProvenance{}
	memory.Evidence = nil
	memory.Redacted = true
}

// CountMemories counts current heads matching the same filters as ListMemories
// without loading value-bearing content. Limit and cursor do not affect count.
func (s *Store) CountMemories(ctx context.Context, p Principal, opts MemoryListOptions) (int, error) {
	if p.Kind != PrincipalAgent {
		return 0, ErrMemoryForbidden
	}
	var err error
	opts, err = normalizeMemoryListOptions(p, opts)
	if err != nil {
		return 0, err
	}
	args := []any{p.AccountID, p.RealmID, p.ID}
	where := []string{
		"m.account_id=$1", "m.realm_id=$2", "m.owner_kind='agent'",
		"m.owner_id=$3", "m.current_version IS NOT NULL",
		"v.version=m.current_version",
	}
	addArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	where = append(where, "v.state="+addArg(opts.State))
	if opts.Kind != "" {
		where = append(where, "v.kind="+addArg(opts.Kind))
	}
	if opts.Origin != "" {
		where = append(where, "m.origin="+addArg(opts.Origin))
	}
	if opts.CaptureReason != "" {
		where = append(where, "m.capture_reason="+addArg(opts.CaptureReason))
	}
	if len(opts.Tags) > 0 {
		raw, _ := json.Marshal(opts.Tags)
		where = append(where, "v.tags @> "+addArg(string(raw))+"::jsonb")
	}
	if opts.OccurredFrom != nil {
		where = append(where,
			"COALESCE(v.occurred_until,v.occurred_from) >= "+addArg(*opts.OccurredFrom))
	}
	if opts.OccurredUntil != nil {
		where = append(where,
			"COALESCE(v.occurred_from,v.occurred_until) <= "+addArg(*opts.OccurredUntil))
	}
	if opts.CapturedFrom != nil {
		where = append(where, "m.created_at >= "+addArg(*opts.CapturedFrom))
	}
	if opts.CapturedUntil != nil {
		where = append(where, "m.created_at <= "+addArg(*opts.CapturedUntil))
	}
	var count int
	err = s.pool.QueryRow(ctx, `
		SELECT count(*) FROM memories m
		JOIN memory_versions v ON v.memory_id=m.id
		WHERE `+strings.Join(where, " AND "), args...).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count memories: %w", err)
	}
	return count, nil
}

// GetMemoryHistoryPage returns one bounded page of immutable snapshots oldest
// first. Version rows and their evidence use two bounded queries regardless of
// the requested page size, avoiding a query per version.
func (s *Store) GetMemoryHistoryPage(ctx context.Context, p Principal, memoryID string, opts MemoryHistoryOptions) (MemoryHistoryPage, error) {
	if p.Kind != PrincipalAgent {
		return MemoryHistoryPage{}, ErrMemoryForbidden
	}
	if !validMemoryID(memoryID) {
		return MemoryHistoryPage{}, ErrMemoryInputInvalid
	}
	var err error
	opts, err = normalizeMemoryHistoryOptions(memoryID, opts)
	if err != nil {
		return MemoryHistoryPage{}, err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM memories WHERE id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4 AND current_version IS NOT NULL
	)`, memoryID, p.AccountID, p.RealmID, p.ID).Scan(&exists); err != nil {
		return MemoryHistoryPage{}, fmt.Errorf("authorize memory history: %w", err)
	}
	if !exists {
		return MemoryHistoryPage{}, ErrMemoryNotFound
	}
	rows, err := s.pool.Query(ctx, memorySelectSQL+`
		WHERE m.id=$1 AND m.account_id=$2 AND m.realm_id=$3
		  AND m.owner_kind='agent' AND m.owner_id=$4 AND v.version>$5
		ORDER BY v.version
		LIMIT $6`, memoryID, p.AccountID, p.RealmID, p.ID,
		opts.AfterVersion, opts.Limit+1)
	if err != nil {
		return MemoryHistoryPage{}, fmt.Errorf("read memory history: %w", err)
	}
	versions := make([]MemoryVersion, 0, opts.Limit+1)
	for rows.Next() {
		version, err := scanMemory(rows)
		if err != nil {
			rows.Close()
			return MemoryHistoryPage{}, err
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return MemoryHistoryPage{}, fmt.Errorf("read memory history: %w", err)
	}
	rows.Close()

	page := MemoryHistoryPage{Versions: versions}
	if len(page.Versions) > opts.Limit {
		page.Versions = page.Versions[:opts.Limit]
		last := page.Versions[len(page.Versions)-1]
		page.NextVersion = last.Version
		page.NextCursor, err = encodeMemoryHistoryCursor(memoryID, last.Version)
		if err != nil {
			return MemoryHistoryPage{}, err
		}
	}
	if len(page.Versions) == 0 {
		return page, nil
	}
	byVersion, err := listMemoryEvidenceForVersions(ctx, s.pool, p, memoryID, page.Versions)
	if err != nil {
		return MemoryHistoryPage{}, err
	}
	for i := range page.Versions {
		page.Versions[i].Evidence = byVersion[page.Versions[i].Version]
	}
	return page, nil
}

// AdjustMemory appends a new version containing a normalized partial update.
func (s *Store) AdjustMemory(ctx context.Context, p Principal, memoryID string, in AdjustMemoryInput) (MemoryMutationResult, error) {
	if p.Kind != PrincipalAgent {
		return MemoryMutationResult{}, ErrMemoryForbidden
	}
	var err error
	in, err = normalizeAdjustMemoryInput(in)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	hash, err := memoryRequestHash(struct {
		Operation string            `json:"operation"`
		MemoryID  string            `json:"memory_id"`
		Input     AdjustMemoryInput `json:"input"`
	}{"adjusted", memoryID, in})
	if err != nil {
		return MemoryMutationResult{}, err
	}
	return s.mutateMemory(ctx, p, memoryID, "adjusted", in.ExpectedVersion,
		in.IdempotencyKey, hash, in.Client, in.Reason, nil,
		func(current *Memory) error { return applyMemoryAdjustment(current, in) })
}

// ForgetMemory appends a version that removes a memory from active recall.
func (s *Store) ForgetMemory(ctx context.Context, p Principal, memoryID string, in MemoryLifecycleInput) (MemoryMutationResult, error) {
	return s.lifecycleMemory(ctx, p, memoryID, "forgotten", in)
}

// RestoreMemory appends a version that restores a previously forgotten memory.
func (s *Store) RestoreMemory(ctx context.Context, p Principal, memoryID string, in MemoryLifecycleInput) (MemoryMutationResult, error) {
	return s.lifecycleMemory(ctx, p, memoryID, "restored", in)
}

// ReactivateMemory appends a version that makes a superseded memory active again.
func (s *Store) ReactivateMemory(ctx context.Context, p Principal, memoryID string, in MemoryLifecycleInput) (MemoryMutationResult, error) {
	return s.lifecycleMemory(ctx, p, memoryID, "reactivated", in)
}

func (s *Store) lifecycleMemory(ctx context.Context, p Principal, memoryID, operation string, in MemoryLifecycleInput) (MemoryMutationResult, error) {
	if p.Kind != PrincipalAgent {
		return MemoryMutationResult{}, ErrMemoryForbidden
	}
	var err error
	in, err = normalizeMemoryLifecycleInput(in, operation)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	hash, err := memoryRequestHash(struct {
		Operation string               `json:"operation"`
		MemoryID  string               `json:"memory_id"`
		Input     MemoryLifecycleInput `json:"input"`
	}{operation, memoryID, in})
	if err != nil {
		return MemoryMutationResult{}, err
	}
	return s.mutateMemory(ctx, p, memoryID, operation, in.ExpectedVersion,
		in.IdempotencyKey, hash, in.Client, in.Reason,
		in.ExpectedSupersessionSetRevision, nil)
}

func (s *Store) mutateMemory(
	ctx context.Context,
	p Principal,
	memoryID, operation string,
	expectedVersion int64,
	idempotencyKey, requestHash string,
	client MemoryClientProvenance,
	reason string,
	expectedSupersessionRevision *int64,
	adjust func(*Memory) error,
) (MemoryMutationResult, error) {
	if !validMemoryID(memoryID) {
		return MemoryMutationResult{}, ErrMemoryInputInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return MemoryMutationResult{}, err
	}
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MemoryMutationResult{}, err
	}
	if replay, ok, err := memoryMutationReplay(ctx, tx, p, idempotencyKey, requestHash, operation); err != nil || ok {
		return replay, err
	}
	lane, err := lockMemoryCurationSourceLaneTx(ctx, tx, p)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	if replay, ok, err := memoryMutationReplay(ctx, tx, p, idempotencyKey, requestHash, operation); err != nil || ok {
		return replay, err
	}
	if err := markMemoryCurationDueTx(ctx, tx, p, &lane,
		"memory_changed", MemoryCurationSourceMemory, idempotencyKey); err != nil {
		return MemoryMutationResult{}, err
	}
	changeSeq, err := allocateMemoryChangeSeq(ctx, tx, p)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	if replay, ok, err := memoryMutationReplay(ctx, tx, p, idempotencyKey, requestHash, operation); err != nil || ok {
		return replay, err
	}
	var currentVersion sql.NullInt64
	err = tx.QueryRow(ctx, `
		SELECT current_version FROM memories
		WHERE id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4
		FOR UPDATE`, memoryID, p.AccountID, p.RealmID, p.ID).Scan(&currentVersion)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !currentVersion.Valid) {
		return MemoryMutationResult{}, ErrMemoryNotFound
	}
	if err != nil {
		return MemoryMutationResult{}, fmt.Errorf("lock memory: %w", err)
	}
	current, err := loadMemoryAtVersion(ctx, tx, p, memoryID, currentVersion.Int64, false)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	if current.Version != expectedVersion {
		return MemoryMutationResult{}, ErrMemoryConflict
	}

	next := current
	next.PreviousVersion = current.Version
	next.Version = current.Version + 1
	next.ChangeSeq = changeSeq
	next.ActorKind = "agent"
	next.ActorID = p.ID
	next.Operation = operation
	next.IdempotencyKey = idempotencyKey
	next.RequestHash = requestHash
	next.Client = client
	next.LifecycleReason = reason
	next.Evidence = nil
	// Supersession membership is a receipt for the exact version created by
	// the atomic supersede primitive. Later lifecycle versions do not claim a
	// second supersession operation even when restore copies lineage edges.
	next.SupersessionSetID = ""
	next.SupersessionSetRevision = 0
	next.SupersessionReplacementCount = 0
	next.SupersessionReplacementDigest = ""
	switch operation {
	case "adjusted":
		if current.State != MemoryStateActive {
			return MemoryMutationResult{}, ErrMemoryConflict
		}
		before := next
		if err := adjust(&next); err != nil {
			return MemoryMutationResult{}, err
		}
		if memoryPayloadEqual(before, next) {
			return MemoryMutationResult{}, fmt.Errorf("%w: adjustment makes no payload change", ErrMemoryInputInvalid)
		}
	case "forgotten":
		if current.State != MemoryStateActive && current.State != MemoryStateSuperseded {
			return MemoryMutationResult{}, ErrMemoryConflict
		}
		next.State = MemoryStateForgotten
		next.PriorState = current.State
	case "restored":
		if current.State != MemoryStateForgotten {
			return MemoryMutationResult{}, ErrMemoryConflict
		}
		if current.PriorState == MemoryStateSuperseded {
			var activeSet bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM memory_relations
				WHERE to_memory_id=$1 AND to_version=$2 AND relation_type='supersedes'
				  AND reverted_at IS NULL
			)`, memoryID, current.PreviousVersion).Scan(&activeSet); err != nil {
				return MemoryMutationResult{}, fmt.Errorf("validate superseded restore: %w", err)
			}
			if !activeSet {
				return MemoryMutationResult{}, ErrMemoryConflict
			}
		}
		next.State = current.PriorState
		next.PriorState = ""
	case "reactivated":
		if current.State != MemoryStateSuperseded && current.State != MemoryStateReverted {
			return MemoryMutationResult{}, ErrMemoryConflict
		}
		if current.State == MemoryStateSuperseded {
			if expectedSupersessionRevision == nil || *expectedSupersessionRevision < 1 {
				return MemoryMutationResult{}, ErrMemoryConflict
			}
			var setID string
			var revision int64
			err := tx.QueryRow(ctx, `
				SELECT supersession_set_id, supersession_set_revision
				FROM memory_relations
				WHERE to_memory_id=$1 AND to_version=$2 AND relation_type='supersedes'
				  AND reverted_at IS NULL
				ORDER BY id LIMIT 1`, memoryID, current.Version).Scan(&setID, &revision)
			if errors.Is(err, pgx.ErrNoRows) || revision != *expectedSupersessionRevision {
				return MemoryMutationResult{}, ErrMemoryConflict
			}
			if err != nil {
				return MemoryMutationResult{}, fmt.Errorf("load supersession set: %w", err)
			}
			tag, err := tx.Exec(ctx, `
				UPDATE memory_relations SET reverted_at=clock_timestamp()
				WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
				  AND relation_type='supersedes' AND supersession_set_id=$4
				  AND supersession_set_revision=$5
				  AND reverted_at IS NULL`, p.AccountID, p.RealmID, p.ID, setID, revision)
			if err != nil {
				return MemoryMutationResult{}, fmt.Errorf("revert supersession set: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return MemoryMutationResult{}, ErrMemoryConflict
			}
		}
		next.State = MemoryStateActive
		next.PriorState = ""
	default:
		return MemoryMutationResult{}, ErrMemoryInputInvalid
	}
	if err := validateMemorySnapshot(next); err != nil {
		return MemoryMutationResult{}, err
	}
	next.ContentHash = memoryContentHash(next.Content)
	createdAt, err := insertMemoryVersionTx(ctx, tx, next)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	if operation == "restored" && next.State == MemoryStateSuperseded {
		if err := copySupersessionRelationsTx(ctx, tx, p, memoryID,
			current.PreviousVersion, next.Version); err != nil {
			return MemoryMutationResult{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memories SET current_version=$2, updated_at=$3 WHERE id=$1`,
		memoryID, next.Version, createdAt); err != nil {
		return MemoryMutationResult{}, fmt.Errorf("move memory head: %w", err)
	}
	out, err := loadMemoryAtVersion(ctx, tx, p, memoryID, next.Version, true)
	if err != nil {
		return MemoryMutationResult{}, err
	}
	if err := logMemoryVersionEventTx(ctx, tx, p, out); err != nil {
		return MemoryMutationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MemoryMutationResult{}, err
	}
	return memoryMutationResult(out), nil
}

// ResolveMemoryEvidence appends one terminal row; the pending row is immutable.
func (s *Store) ResolveMemoryEvidence(ctx context.Context, p Principal, pendingID string, in ResolveMemoryEvidenceInput) (MemoryEvidence, error) {
	if p.Kind != PrincipalAgent {
		return MemoryEvidence{}, ErrMemoryForbidden
	}
	if !strings.HasPrefix(pendingID, "mev_") {
		return MemoryEvidence{}, ErrMemoryInputInvalid
	}
	var err error
	in, err = normalizeResolveMemoryEvidenceInput(in)
	if err != nil {
		return MemoryEvidence{}, err
	}
	hash, err := memoryRequestHash(struct {
		PendingID string                     `json:"pending_id"`
		Input     ResolveMemoryEvidenceInput `json:"input"`
	}{pendingID, in})
	if err != nil {
		return MemoryEvidence{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MemoryEvidence{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return MemoryEvidence{}, err
	}
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MemoryEvidence{}, err
	}
	if replay, ok, err := memoryEvidenceReplay(ctx, tx, p, in.IdempotencyKey, hash); err != nil || ok {
		return replay, err
	}
	lane, err := lockMemoryCurationSourceLaneTx(ctx, tx, p)
	if err != nil {
		return MemoryEvidence{}, err
	}
	if replay, ok, err := memoryEvidenceReplay(ctx, tx, p, in.IdempotencyKey, hash); err != nil || ok {
		return replay, err
	}
	if err := markMemoryCurationDueTx(ctx, tx, p, &lane,
		"evidence_resolved", MemoryCurationSourceEvidence, in.IdempotencyKey); err != nil {
		return MemoryEvidence{}, err
	}
	evidenceSeq, err := allocateMemoryChangeSeq(ctx, tx, p)
	if err != nil {
		return MemoryEvidence{}, err
	}
	if replay, ok, err := memoryEvidenceReplay(ctx, tx, p, in.IdempotencyKey, hash); err != nil || ok {
		return replay, err
	}
	var memoryID, evidenceType, role, state string
	var targetVersion int64
	err = tx.QueryRow(ctx, `
		SELECT memory_id, target_version, evidence_type, role, resolution_state
		FROM memory_evidence
		WHERE id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4
		FOR UPDATE`, pendingID, p.AccountID, p.RealmID, p.ID).
		Scan(&memoryID, &targetVersion, &evidenceType, &role, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryEvidence{}, ErrMemoryNotFound
	}
	if err != nil {
		return MemoryEvidence{}, fmt.Errorf("lock pending memory evidence: %w", err)
	}
	if state != MemoryEvidencePending {
		return MemoryEvidence{}, ErrMemoryEvidenceConflict
	}
	var terminalExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM memory_evidence WHERE pending_evidence_id=$1
		  AND resolution_state IN ('resolved','unresolvable')
	)`, pendingID).Scan(&terminalExists); err != nil {
		return MemoryEvidence{}, fmt.Errorf("check evidence resolution: %w", err)
	}
	if terminalExists {
		return MemoryEvidence{}, ErrMemoryEvidenceConflict
	}
	evidence := MemoryEvidenceInput{
		Type: evidenceType, Role: role, ResolvedKind: in.ResolvedKind,
		SourceTranscriptID:  in.SourceTranscriptID,
		SourceSequenceFrom:  in.SourceSequenceFrom,
		SourceSequenceUntil: in.SourceSequenceUntil,
		SourceMemoryID:      in.SourceMemoryID,
		SourceMemoryVersion: in.SourceMemoryVersion,
		SourceMessageID:     in.SourceMessageID,
		SourceImportLocator: in.SourceImportLocator,
		ArtifactExcerpt:     in.ArtifactExcerpt,
		ArtifactSensitive:   in.ArtifactSensitive,
		SourceDigest:        in.SourceDigest,
	}
	if in.UnresolvableReason != "" {
		evidence.ResolutionState = MemoryEvidenceUnresolvable
		evidence.TerminalReasonCode = in.UnresolvableReason
	} else {
		evidence.ResolutionState = MemoryEvidenceResolved
	}
	evidence, err = normalizeMemoryEvidenceInput(evidence)
	if err != nil {
		return MemoryEvidence{}, err
	}
	if err := validateMemoryEvidenceSourceTx(ctx, tx, p, evidence); err != nil {
		return MemoryEvidence{}, err
	}
	out, err := insertMemoryEvidenceResolutionTx(ctx, tx, p, memoryID,
		targetVersion, pendingID, evidenceSeq, evidence, in.IdempotencyKey, hash)
	if err != nil {
		return MemoryEvidence{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE memories SET updated_at=$2 WHERE id=$1`, memoryID, out.CreatedAt); err != nil {
		return MemoryEvidence{}, fmt.Errorf("touch memory after evidence resolution: %w", err)
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
		Verb: VerbMemoryEvidenceResolved,
		Metadata: map[string]any{
			"memory_id": out.MemoryID, "memory_version": strconv.FormatInt(out.TargetVersion, 10),
			"evidence_id": out.ID, "pending_evidence_id": out.PendingEvidenceID,
			"resolution_state": out.ResolutionState,
		},
	}); err != nil {
		return MemoryEvidence{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MemoryEvidence{}, err
	}
	return out, nil
}

const memorySelectSQL = `
	SELECT m.id, m.account_id, m.realm_id, m.owner_kind, m.owner_id,
	       m.origin, m.capture_reason, m.authored_by_agent_id,
	       v.version, COALESCE(v.previous_version,0), v.change_seq,
	       v.content, v.content_encoding, v.kind, v.tags, v.links,
	       v.salience, v.sensitive, v.occurred_from, v.occurred_until,
	       v.state, COALESCE(v.prior_state,''), v.lifecycle_reason,
	       v.content_hash, v.actor_kind, v.actor_id, v.operation,
	       v.idempotency_key, v.request_hash, v.client_runtime,
	       v.client_model, v.client_recipe, v.client_recipe_version,
	       COALESCE(v.curation_run_id,''),COALESCE(v.curation_action_id,''),
	       v.supersession_set_id, v.supersession_set_revision,
	       v.supersession_replacement_count, v.supersession_replacement_digest,
	       COALESCE(active_supersession.supersession_set_id,''),
	       COALESCE(active_supersession.supersession_set_revision,0),
	       v.created_at, m.updated_at
	FROM memories m
	JOIN memory_versions v ON v.memory_id=m.id
	LEFT JOIN LATERAL (
		SELECT r.supersession_set_id, r.supersession_set_revision
		FROM memory_relations r
		WHERE r.account_id=m.account_id AND r.realm_id=m.realm_id
		  AND r.owner_kind=m.owner_kind AND r.owner_id=m.owner_id
		  AND r.to_memory_id=m.id AND r.relation_type='supersedes'
		  AND r.reverted_at IS NULL
		ORDER BY r.supersession_set_revision DESC, r.supersession_set_id, r.id
		LIMIT 1
	) active_supersession ON true`

type memoryScanner interface {
	Scan(dest ...any) error
}

func scanMemory(row memoryScanner) (Memory, error) {
	var out Memory
	var tags, links []byte
	var occurredFrom, occurredUntil sql.NullTime
	err := row.Scan(
		&out.ID, &out.AccountID, &out.RealmID, &out.OwnerKind, &out.OwnerID,
		&out.Origin, &out.CaptureReason, &out.AuthoredByAgentID,
		&out.Version, &out.PreviousVersion, &out.ChangeSeq,
		&out.Content, &out.ContentEncoding, &out.Kind, &tags, &links,
		&out.Salience, &out.Sensitive, &occurredFrom, &occurredUntil,
		&out.State, &out.PriorState, &out.LifecycleReason,
		&out.ContentHash, &out.ActorKind, &out.ActorID, &out.Operation,
		&out.IdempotencyKey, &out.RequestHash, &out.Client.Runtime,
		&out.Client.Model, &out.Client.Recipe, &out.Client.RecipeVersion,
		&out.CurationRunID, &out.CurationActionID,
		&out.SupersessionSetID, &out.SupersessionSetRevision,
		&out.SupersessionReplacementCount, &out.SupersessionReplacementDigest,
		&out.ActiveSupersessionSetID, &out.ActiveSupersessionSetRevision,
		&out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return Memory{}, err
	}
	if err := json.Unmarshal(tags, &out.Tags); err != nil {
		return Memory{}, fmt.Errorf("decode memory tags: %w", err)
	}
	if err := json.Unmarshal(links, &out.Links); err != nil {
		return Memory{}, fmt.Errorf("decode memory links: %w", err)
	}
	if occurredFrom.Valid {
		out.OccurredFrom = &occurredFrom.Time
	}
	if occurredUntil.Valid {
		out.OccurredUntil = &occurredUntil.Time
	}
	return out, nil
}

func loadCurrentMemory(ctx context.Context, q memoryQuerier, p Principal, memoryID string, includeEvidence bool) (Memory, error) {
	row := q.QueryRow(ctx, memorySelectSQL+`
		WHERE m.id=$1 AND m.account_id=$2 AND m.realm_id=$3
		  AND m.owner_kind='agent' AND m.owner_id=$4
		  AND v.version=m.current_version`, memoryID, p.AccountID, p.RealmID, p.ID)
	out, err := scanMemory(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Memory{}, ErrMemoryNotFound
	}
	if err != nil {
		return Memory{}, fmt.Errorf("read memory: %w", err)
	}
	if includeEvidence {
		out.Evidence, err = listMemoryEvidence(ctx, q, p, memoryID, 0)
	}
	return out, err
}

func loadMemoryAtVersion(ctx context.Context, q memoryQuerier, p Principal, memoryID string, version int64, includeEvidence bool) (Memory, error) {
	row := q.QueryRow(ctx, memorySelectSQL+`
		WHERE m.id=$1 AND m.account_id=$2 AND m.realm_id=$3
		  AND m.owner_kind='agent' AND m.owner_id=$4 AND v.version=$5`,
		memoryID, p.AccountID, p.RealmID, p.ID, version)
	out, err := scanMemory(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Memory{}, ErrMemoryNotFound
	}
	if err != nil {
		return Memory{}, fmt.Errorf("read memory version: %w", err)
	}
	if includeEvidence {
		out.Evidence, err = listMemoryEvidence(ctx, q, p, memoryID, 0)
	}
	return out, err
}

type memoryQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func listMemoryEvidence(ctx context.Context, q memoryQuerier, p Principal, memoryID string, targetVersion int64) ([]MemoryEvidence, error) {
	args := []any{memoryID, p.AccountID, p.RealmID, p.ID}
	versionClause := ""
	if targetVersion > 0 {
		args = append(args, targetVersion)
		versionClause = " AND target_version=$5"
	}
	rows, err := q.Query(ctx, `
		SELECT id, memory_id, target_version, evidence_change_seq,
		       evidence_type, role, resolution_state,
		       COALESCE(external_locator,''), COALESCE(pending_evidence_id,''),
		       COALESCE(resolved_kind,''), COALESCE(source_transcript_id,''),
		       COALESCE(source_sequence_from,0), COALESCE(source_sequence_until,0),
		       COALESCE(source_memory_id,''), COALESCE(source_memory_version,0),
		       COALESCE(source_message_id,''), COALESCE(source_import_locator,''),
		       artifact_excerpt, artifact_sensitive,
		       COALESCE(terminal_reason_code,''), COALESCE(source_digest,''),
		       actor_id, COALESCE(idempotency_key,''), COALESCE(request_hash,''),
		       created_at
		FROM memory_evidence
		WHERE memory_id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4`+versionClause+`
		ORDER BY evidence_change_seq, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list memory evidence: %w", err)
	}
	defer rows.Close()
	out := make([]MemoryEvidence, 0)
	for rows.Next() {
		evidence, err := scanMemoryEvidence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, evidence)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list memory evidence: %w", err)
	}
	return out, nil
}

func listMemoryEvidenceForVersions(ctx context.Context, q memoryQuerier, p Principal, memoryID string, versions []MemoryVersion) (map[int64][]MemoryEvidence, error) {
	targetVersions := make([]int64, len(versions))
	for i := range versions {
		targetVersions[i] = versions[i].Version
	}
	rows, err := q.Query(ctx, `
		SELECT id, memory_id, target_version, evidence_change_seq,
		       evidence_type, role, resolution_state,
		       COALESCE(external_locator,''), COALESCE(pending_evidence_id,''),
		       COALESCE(resolved_kind,''), COALESCE(source_transcript_id,''),
		       COALESCE(source_sequence_from,0), COALESCE(source_sequence_until,0),
		       COALESCE(source_memory_id,''), COALESCE(source_memory_version,0),
		       COALESCE(source_message_id,''), COALESCE(source_import_locator,''),
		       artifact_excerpt, artifact_sensitive,
		       COALESCE(terminal_reason_code,''), COALESCE(source_digest,''),
		       actor_id, COALESCE(idempotency_key,''), COALESCE(request_hash,''),
		       created_at
		FROM memory_evidence
		WHERE memory_id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4
		  AND target_version=ANY($5::bigint[])
		ORDER BY target_version, evidence_change_seq, id`,
		memoryID, p.AccountID, p.RealmID, p.ID, targetVersions)
	if err != nil {
		return nil, fmt.Errorf("list memory history evidence: %w", err)
	}
	defer rows.Close()
	byVersion := make(map[int64][]MemoryEvidence, len(versions))
	for rows.Next() {
		evidence, err := scanMemoryEvidence(rows)
		if err != nil {
			return nil, err
		}
		byVersion[evidence.TargetVersion] = append(byVersion[evidence.TargetVersion], evidence)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list memory history evidence: %w", err)
	}
	return byVersion, nil
}

func scanMemoryEvidence(row memoryScanner) (MemoryEvidence, error) {
	var out MemoryEvidence
	err := row.Scan(&out.ID, &out.MemoryID, &out.TargetVersion,
		&out.EvidenceChangeSeq, &out.Type, &out.Role, &out.ResolutionState,
		&out.ExternalLocator, &out.PendingEvidenceID, &out.ResolvedKind,
		&out.SourceTranscriptID, &out.SourceSequenceFrom, &out.SourceSequenceUntil,
		&out.SourceMemoryID, &out.SourceMemoryVersion, &out.SourceMessageID,
		&out.SourceImportLocator, &out.ArtifactExcerpt, &out.ArtifactSensitive,
		&out.TerminalReasonCode, &out.SourceDigest, &out.ActorID,
		&out.IdempotencyKey, &out.RequestHash, &out.CreatedAt)
	if err != nil {
		return MemoryEvidence{}, fmt.Errorf("scan memory evidence: %w", err)
	}
	return out, nil
}

func allocateMemoryChangeSeq(ctx context.Context, tx pgx.Tx, p Principal) (int64, error) {
	_, err := tx.Exec(ctx, `
		INSERT INTO memory_change_clocks
		  (account_id, realm_id, owner_kind, owner_id)
		VALUES ($1,$2,'agent',$3)
		ON CONFLICT DO NOTHING`, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return 0, fmt.Errorf("initialize memory change clock: %w", err)
	}
	var seq int64
	err = tx.QueryRow(ctx, `
		UPDATE memory_change_clocks
		SET last_change_seq=last_change_seq+1, updated_at=clock_timestamp()
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND last_change_seq < $4
		RETURNING last_change_seq`, p.AccountID, p.RealmID, p.ID, maxMemoryChangeSeq).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrMemoryChangeSeqExhausted
	}
	if err != nil {
		return 0, fmt.Errorf("advance memory change clock: %w", err)
	}
	return seq, nil
}

func insertMemoryVersionTx(ctx context.Context, tx pgx.Tx, m Memory) (time.Time, error) {
	tags, err := json.Marshal(m.Tags)
	if err != nil {
		return time.Time{}, err
	}
	links, err := json.Marshal(m.Links)
	if err != nil {
		return time.Time{}, err
	}
	var previous any
	if m.PreviousVersion > 0 {
		previous = m.PreviousVersion
	}
	var prior any
	if m.PriorState != "" {
		prior = m.PriorState
	}
	var createdAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO memory_versions
		  (memory_id, version, account_id, realm_id, owner_kind, owner_id,
		   previous_version, change_seq, content, content_encoding, kind,
		   tags, links, salience, sensitive, occurred_from, occurred_until,
		   state, prior_state, lifecycle_reason, content_hash, actor_kind,
		   actor_id, operation, idempotency_key, request_hash, client_runtime,
		   client_model, client_recipe, client_recipe_version,
		   curation_run_id,curation_action_id,
		   supersession_set_id, supersession_set_revision,
		   supersession_replacement_count, supersession_replacement_digest)
		VALUES ($1,$2,$3,$4,'agent',$5,$6,$7,$8,$9,$10,$11::jsonb,$12::jsonb,
		        $13,$14,$15,$16,$17,$18,$19,$20,'agent',$21,$22,$23,$24,
		        $25,$26,$27,$28,NULLIF($29,''),NULLIF($30,''),$31,$32,$33,$34)
		RETURNING created_at`,
		m.ID, m.Version, m.AccountID, m.RealmID, m.OwnerID, previous,
		m.ChangeSeq, m.Content, m.ContentEncoding, m.Kind, string(tags),
		string(links), m.Salience, m.Sensitive, m.OccurredFrom, m.OccurredUntil,
		m.State, prior, m.LifecycleReason, m.ContentHash, m.ActorID,
		m.Operation, m.IdempotencyKey, m.RequestHash, m.Client.Runtime,
		m.Client.Model, m.Client.Recipe, m.Client.RecipeVersion,
		m.CurationRunID, m.CurationActionID,
		m.SupersessionSetID, m.SupersessionSetRevision,
		m.SupersessionReplacementCount, m.SupersessionReplacementDigest).Scan(&createdAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return time.Time{}, ErrMemoryIdempotencyConflict
		}
		return time.Time{}, fmt.Errorf("insert memory version: %w", err)
	}
	return createdAt, nil
}

func copySupersessionRelationsTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	memoryID string,
	fromTargetVersion, toTargetVersion int64,
) error {
	rows, err := tx.Query(ctx, `
		SELECT from_memory_id, from_version, supersession_set_id,
		       supersession_set_revision, curation_run_id, curation_action_id
		FROM memory_relations
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND to_memory_id=$4 AND to_version=$5 AND relation_type='supersedes'
		  AND reverted_at IS NULL
		ORDER BY id`, p.AccountID, p.RealmID, p.ID, memoryID, fromTargetVersion)
	if err != nil {
		return fmt.Errorf("read supersession relations for restore: %w", err)
	}
	type relation struct {
		fromMemoryID string
		fromVersion  int64
		setID        string
		setRevision  int64
		runID        sql.NullString
		actionID     sql.NullString
	}
	relations := make([]relation, 0)
	for rows.Next() {
		var item relation
		if err := rows.Scan(&item.fromMemoryID, &item.fromVersion, &item.setID,
			&item.setRevision, &item.runID, &item.actionID); err != nil {
			rows.Close()
			return fmt.Errorf("scan supersession relation for restore: %w", err)
		}
		relations = append(relations, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read supersession relations for restore: %w", err)
	}
	rows.Close()
	if len(relations) == 0 {
		return ErrMemoryConflict
	}
	for _, item := range relations {
		relationID, err := id.New("mrel")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO memory_relations
			  (id, account_id, realm_id, owner_kind, owner_id,
			   from_memory_id, from_version, to_memory_id, to_version,
			   relation_type, supersession_set_id, supersession_set_revision,
			   curation_run_id, curation_action_id)
			VALUES ($1,$2,$3,'agent',$4,$5,$6,$7,$8,'supersedes',$9,$10,$11,$12)`,
			relationID, p.AccountID, p.RealmID, p.ID,
			item.fromMemoryID, item.fromVersion, memoryID, toTargetVersion,
			item.setID, item.setRevision, nullableString(item.runID),
			nullableString(item.actionID))
		if err != nil {
			return fmt.Errorf("copy supersession relation for restore: %w", err)
		}
	}
	return nil
}

func nullableString(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func insertMemoryEvidenceTx(ctx context.Context, tx pgx.Tx, p Principal, memoryID string, targetVersion int64, in MemoryEvidenceInput, idempotencyKey, requestHash string) (MemoryEvidence, error) {
	return insertMemoryEvidenceWithPendingTx(ctx, tx, p, memoryID, targetVersion,
		"", 0, in, idempotencyKey, requestHash)
}

func insertMemoryEvidenceResolutionTx(ctx context.Context, tx pgx.Tx, p Principal, memoryID string, targetVersion int64, pendingID string, evidenceSeq int64, in MemoryEvidenceInput, idempotencyKey, requestHash string) (MemoryEvidence, error) {
	return insertMemoryEvidenceWithPendingTx(ctx, tx, p, memoryID, targetVersion,
		pendingID, evidenceSeq, in, idempotencyKey, requestHash)
}

func insertMemoryEvidenceWithPendingTx(ctx context.Context, tx pgx.Tx, p Principal, memoryID string, targetVersion int64, pendingID string, evidenceSeq int64, in MemoryEvidenceInput, idempotencyKey, requestHash string) (MemoryEvidence, error) {
	evidenceID, err := id.New("mev")
	if err != nil {
		return MemoryEvidence{}, err
	}
	if evidenceSeq == 0 {
		evidenceSeq, err = allocateMemoryChangeSeq(ctx, tx, p)
		if err != nil {
			return MemoryEvidence{}, err
		}
	}
	var out MemoryEvidence
	err = tx.QueryRow(ctx, `
		INSERT INTO memory_evidence
		  (id, account_id, realm_id, owner_kind, owner_id, memory_id,
		   target_version, evidence_change_seq, evidence_type, role,
		   resolution_state, external_locator, pending_evidence_id, resolved_kind,
		   source_transcript_id, source_sequence_from, source_sequence_until,
		   source_memory_id, source_memory_version, source_message_id,
		   source_import_locator, artifact_excerpt, artifact_sensitive,
		   terminal_reason_code, source_digest, actor_id, idempotency_key,
		   request_hash)
		VALUES ($1,$2,$3,'agent',$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),
		        NULLIF($12,''),NULLIF($13,''),NULLIF($14,''),NULLIF($15,0),
		        NULLIF($16,0),NULLIF($17,''),NULLIF($18,0),NULLIF($19,''),
		        NULLIF($20,''),$21,$22,NULLIF($23,''),NULLIF($24,''),$4,
		        NULLIF($25,''),NULLIF($26,''))
		RETURNING id, memory_id, target_version, evidence_change_seq,
		          evidence_type, role, resolution_state,
		          COALESCE(external_locator,''), COALESCE(pending_evidence_id,''),
		          COALESCE(resolved_kind,''), COALESCE(source_transcript_id,''),
		          COALESCE(source_sequence_from,0), COALESCE(source_sequence_until,0),
		          COALESCE(source_memory_id,''), COALESCE(source_memory_version,0),
		          COALESCE(source_message_id,''), COALESCE(source_import_locator,''),
		          artifact_excerpt, artifact_sensitive,
		          COALESCE(terminal_reason_code,''), COALESCE(source_digest,''),
		          actor_id, COALESCE(idempotency_key,''), COALESCE(request_hash,''),
		          created_at`,
		evidenceID, p.AccountID, p.RealmID, p.ID, memoryID, targetVersion,
		evidenceSeq, in.Type, in.Role, in.ResolutionState, in.ExternalLocator, pendingID,
		in.ResolvedKind, in.SourceTranscriptID, in.SourceSequenceFrom,
		in.SourceSequenceUntil, in.SourceMemoryID, in.SourceMemoryVersion,
		in.SourceMessageID, in.SourceImportLocator, nullableBytes(in.ArtifactExcerpt),
		in.ArtifactSensitive, in.TerminalReasonCode, in.SourceDigest,
		idempotencyKey, requestHash).Scan(
		&out.ID, &out.MemoryID, &out.TargetVersion, &out.EvidenceChangeSeq,
		&out.Type, &out.Role, &out.ResolutionState, &out.ExternalLocator,
		&out.PendingEvidenceID, &out.ResolvedKind, &out.SourceTranscriptID,
		&out.SourceSequenceFrom, &out.SourceSequenceUntil, &out.SourceMemoryID,
		&out.SourceMemoryVersion, &out.SourceMessageID, &out.SourceImportLocator,
		&out.ArtifactExcerpt, &out.ArtifactSensitive, &out.TerminalReasonCode,
		&out.SourceDigest, &out.ActorID, &out.IdempotencyKey, &out.RequestHash,
		&out.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return MemoryEvidence{}, ErrMemoryEvidenceConflict
		}
		return MemoryEvidence{}, fmt.Errorf("insert memory evidence: %w", err)
	}
	return out, nil
}

func memoryMutationReplay(ctx context.Context, q memoryQuerier, p Principal, key, requestHash, operation string) (MemoryMutationResult, bool, error) {
	blocked, err := memoryRetryBlocked(ctx, q, p, operation, key)
	if err != nil {
		return MemoryMutationResult{}, false, err
	}
	if blocked {
		return MemoryMutationResult{}, false, ErrMemoryDeleted
	}
	var memoryID, storedHash, storedOperation string
	var version int64
	err = q.QueryRow(ctx, `
		SELECT memory_id, version, request_hash, operation
		FROM memory_versions
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND idempotency_key=$4`, p.AccountID, p.RealmID, p.ID, key).
		Scan(&memoryID, &version, &storedHash, &storedOperation)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryMutationResult{}, false, nil
	}
	if err != nil {
		return MemoryMutationResult{}, false, fmt.Errorf("find memory retry: %w", err)
	}
	if storedHash != requestHash || storedOperation != operation {
		return MemoryMutationResult{}, false, ErrMemoryIdempotencyConflict
	}
	memory, err := loadMemoryAtVersion(ctx, q, p, memoryID, version, true)
	if err != nil {
		return MemoryMutationResult{}, false, err
	}
	result := memoryMutationResult(memory)
	result.Receipt.Replayed = true
	return result, true, nil
}

func memoryEvidenceReplay(ctx context.Context, q memoryQuerier, p Principal, key, requestHash string) (MemoryEvidence, bool, error) {
	blocked, err := memoryRetryBlocked(ctx, q, p, "evidence_resolution", key)
	if err != nil {
		return MemoryEvidence{}, false, err
	}
	if blocked {
		return MemoryEvidence{}, false, ErrMemoryDeleted
	}
	row := q.QueryRow(ctx, `
		SELECT id, memory_id, target_version, evidence_change_seq,
		       evidence_type, role, resolution_state,
		       COALESCE(external_locator,''), COALESCE(pending_evidence_id,''),
		       COALESCE(resolved_kind,''), COALESCE(source_transcript_id,''),
		       COALESCE(source_sequence_from,0), COALESCE(source_sequence_until,0),
		       COALESCE(source_memory_id,''), COALESCE(source_memory_version,0),
		       COALESCE(source_message_id,''), COALESCE(source_import_locator,''),
		       artifact_excerpt, artifact_sensitive,
		       COALESCE(terminal_reason_code,''), COALESCE(source_digest,''),
		       actor_id, COALESCE(idempotency_key,''), COALESCE(request_hash,''),
		       created_at
		FROM memory_evidence
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND idempotency_key=$4`, p.AccountID, p.RealmID, p.ID, key)
	out, err := scanMemoryEvidence(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryEvidence{}, false, nil
	}
	if err != nil {
		return MemoryEvidence{}, false, err
	}
	if out.RequestHash != requestHash {
		return MemoryEvidence{}, false, ErrMemoryEvidenceConflict
	}
	return out, true, nil
}

func memoryRetryBlocked(ctx context.Context, q memoryQuerier, p Principal, surface, key string) (bool, error) {
	var blocked bool
	err := q.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM memory_deleted_references
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND former_reference_kind=$4 AND related_resource_id=$5
	)`, p.AccountID, p.RealmID, p.ID, "idempotency."+surface,
		memoryIdempotencyKeyHash(key)).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("check deleted memory retry shield: %w", err)
	}
	return blocked, nil
}

func memoryMutationResult(memory Memory) MemoryMutationResult {
	return MemoryMutationResult{
		Memory: memory,
		Receipt: MemoryMutationReceipt{
			Operation: memory.Operation, ActorID: memory.ActorID,
			IdempotencyKey: memory.IdempotencyKey, RequestHash: memory.RequestHash,
			MemoryID: memory.ID, Version: memory.Version,
			ChangeSeq: memory.ChangeSeq, CreatedAt: memory.CreatedAt,
		},
	}
}

func logMemoryVersionEventTx(ctx context.Context, tx pgx.Tx, p Principal, memory Memory) error {
	verb := map[string]string{
		"added":       VerbMemoryAdded,
		"adjusted":    VerbMemoryAdjusted,
		"superseded":  VerbMemorySuperseded,
		"forgotten":   VerbMemoryForgotten,
		"restored":    VerbMemoryRestored,
		"reactivated": VerbMemoryReactivated,
	}[memory.Operation]
	if verb == "" {
		return fmt.Errorf("unregistered memory audit operation %q", memory.Operation)
	}
	return logEventTx(ctx, tx, EventInput{
		AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID, Verb: verb,
		Metadata: map[string]any{
			"memory_id": memory.ID, "version": strconv.FormatInt(memory.Version, 10),
			"state": memory.State,
		},
	})
}

func validateMemoryEvidenceSourceTx(ctx context.Context, q memoryQuerier, p Principal, in MemoryEvidenceInput) error {
	if in.ResolutionState != MemoryEvidenceResolved {
		return nil
	}
	var exists bool
	switch in.ResolvedKind {
	case "transcript":
		err := q.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM transcript_conversations c
			WHERE c.id=$1 AND c.account_id=$2 AND c.realm_id=$3
			  AND c.owner_agent_id=$4
			  AND (SELECT count(*) FROM transcript_entries e
			       WHERE e.transcript_id=c.id AND e.sequence BETWEEN $5 AND $6) = $6-$5+1
		)`, in.SourceTranscriptID, p.AccountID, p.RealmID, p.ID,
			in.SourceSequenceFrom, in.SourceSequenceUntil).Scan(&exists)
		if err != nil {
			return fmt.Errorf("validate transcript evidence: %w", err)
		}
	case "memory":
		err := q.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM memory_versions v JOIN memories m ON m.id=v.memory_id
			WHERE v.memory_id=$1 AND v.version=$2 AND m.account_id=$3
			  AND m.realm_id=$4 AND m.owner_kind='agent' AND m.owner_id=$5
		)`, in.SourceMemoryID, in.SourceMemoryVersion, p.AccountID,
			p.RealmID, p.ID).Scan(&exists)
		if err != nil {
			return fmt.Errorf("validate source memory evidence: %w", err)
		}
	case "message":
		err := q.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM agent_messages
			WHERE id=$1 AND account_id=$2 AND realm_id=$3
			  AND (from_agent_id=$4 OR to_agent_id=$4)
		)`, in.SourceMessageID, p.AccountID, p.RealmID, p.ID).Scan(&exists)
		if err != nil {
			return fmt.Errorf("validate message evidence: %w", err)
		}
	case "import_artifact", "artifact":
		exists = true
	default:
		return ErrMemoryInputInvalid
	}
	if !exists {
		return fmt.Errorf("%w: evidence source is outside the memory owner scope", ErrMemoryInputInvalid)
	}
	return nil
}

func normalizeCaptureMemoryInput(in CaptureMemoryInput) (CaptureMemoryInput, error) {
	// Capture owns its normalized request. Never write through caller-owned
	// slices or retain caller-owned pointers: clients may safely reuse one
	// immutable input for concurrent idempotent retries.
	in = cloneCaptureMemoryInput(in)
	if strings.TrimSpace(in.Content) == "" || len(in.Content) > maxMemoryContentBytes {
		return CaptureMemoryInput{}, fmt.Errorf("%w: content must contain 1-%d bytes", ErrMemoryInputInvalid, maxMemoryContentBytes)
	}
	if in.ContentEncoding == "" {
		in.ContentEncoding = "plain"
	}
	if err := validateMemoryContentEncoding(in.Content, in.ContentEncoding); err != nil {
		return CaptureMemoryInput{}, err
	}
	if in.Kind == "" {
		in.Kind = "episodic"
	}
	if !memoryKindPattern.MatchString(in.Kind) {
		return CaptureMemoryInput{}, fmt.Errorf("%w: invalid kind", ErrMemoryInputInvalid)
	}
	var err error
	in.Tags, err = normalizeMemoryStrings(in.Tags, 64, 128, "tag")
	if err != nil {
		return CaptureMemoryInput{}, err
	}
	in.Links, err = normalizeMemoryStrings(in.Links, 256, 2048, "link")
	if err != nil {
		return CaptureMemoryInput{}, err
	}
	if in.Salience == nil {
		value := 0.5
		in.Salience = &value
	}
	if *in.Salience < 0 || *in.Salience > 1 {
		return CaptureMemoryInput{}, fmt.Errorf("%w: salience must be between 0 and 1", ErrMemoryInputInvalid)
	}
	if in.OccurredFrom != nil {
		v := in.OccurredFrom.UTC()
		in.OccurredFrom = &v
	}
	if in.OccurredUntil != nil {
		v := in.OccurredUntil.UTC()
		in.OccurredUntil = &v
	}
	if in.OccurredFrom != nil && in.OccurredUntil != nil && in.OccurredUntil.Before(*in.OccurredFrom) {
		return CaptureMemoryInput{}, fmt.Errorf("%w: occurred_until precedes occurred_from", ErrMemoryInputInvalid)
	}
	in.Origin = strings.TrimSpace(in.Origin)
	if in.Origin == "" {
		in.Origin = "self"
	}
	in.CaptureReason = strings.TrimSpace(in.CaptureReason)
	if in.CaptureReason == "" {
		in.CaptureReason = "manual"
	}
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if !memoryCodePattern.MatchString(in.Origin) ||
		!memoryCodePattern.MatchString(in.CaptureReason) {
		return CaptureMemoryInput{}, fmt.Errorf("%w: origin and capture reason must be bounded codes", ErrMemoryInputInvalid)
	}
	if err := validateMemoryIdempotencyKey(in.IdempotencyKey); err != nil {
		return CaptureMemoryInput{}, err
	}
	if len(in.Evidence) == 0 || len(in.Evidence) > 32 {
		return CaptureMemoryInput{}, fmt.Errorf("%w: evidence must contain 1-32 rows", ErrMemoryInputInvalid)
	}
	for i := range in.Evidence {
		in.Evidence[i], err = normalizeMemoryEvidenceInput(in.Evidence[i])
		if err != nil {
			return CaptureMemoryInput{}, fmt.Errorf("evidence %d: %w", i, err)
		}
		if in.Evidence[i].ResolutionState == MemoryEvidenceUnresolvable {
			return CaptureMemoryInput{}, fmt.Errorf(
				"evidence %d: %w: unresolvable is only valid when resolving a pending row",
				i, ErrMemoryInputInvalid)
		}
	}
	if err := normalizeMemoryClient(&in.Client); err != nil {
		return CaptureMemoryInput{}, err
	}
	return in, nil
}

func cloneCaptureMemoryInput(in CaptureMemoryInput) CaptureMemoryInput {
	out := in
	out.Tags = slices.Clone(in.Tags)
	out.Links = slices.Clone(in.Links)
	out.Evidence = slices.Clone(in.Evidence)
	for i := range out.Evidence {
		out.Evidence[i].ArtifactExcerpt = slices.Clone(in.Evidence[i].ArtifactExcerpt)
	}
	if in.Salience != nil {
		value := *in.Salience
		out.Salience = &value
	}
	if in.OccurredFrom != nil {
		value := *in.OccurredFrom
		out.OccurredFrom = &value
	}
	if in.OccurredUntil != nil {
		value := *in.OccurredUntil
		out.OccurredUntil = &value
	}
	return out
}

func normalizeMemoryEvidenceInput(in MemoryEvidenceInput) (MemoryEvidenceInput, error) {
	in.Type = strings.TrimSpace(in.Type)
	if in.Type == "" {
		in.Type = "conversation"
	}
	if !memoryEvidencePattern.MatchString(in.Type) {
		return MemoryEvidenceInput{}, fmt.Errorf("%w: invalid evidence type", ErrMemoryInputInvalid)
	}
	in.Role = strings.TrimSpace(in.Role)
	if in.Role == "" {
		in.Role = MemoryEvidenceSupports
	}
	switch in.Role {
	case MemoryEvidenceSupports, MemoryEvidenceContradicts, MemoryEvidenceContext:
	default:
		return MemoryEvidenceInput{}, fmt.Errorf("%w: invalid evidence role", ErrMemoryInputInvalid)
	}
	in.ResolutionState = strings.TrimSpace(in.ResolutionState)
	in.ExternalLocator = strings.TrimSpace(in.ExternalLocator)
	in.ResolvedKind = strings.TrimSpace(in.ResolvedKind)
	in.SourceTranscriptID = strings.TrimSpace(in.SourceTranscriptID)
	in.SourceMemoryID = strings.TrimSpace(in.SourceMemoryID)
	in.SourceMessageID = strings.TrimSpace(in.SourceMessageID)
	in.SourceImportLocator = strings.TrimSpace(in.SourceImportLocator)
	in.TerminalReasonCode = strings.TrimSpace(in.TerminalReasonCode)
	in.SourceDigest = strings.ToLower(strings.TrimSpace(in.SourceDigest))
	if len(in.ExternalLocator) > 2048 || len(in.SourceImportLocator) > 2048 || len(in.ArtifactExcerpt) > 65536 {
		return MemoryEvidenceInput{}, fmt.Errorf("%w: evidence locator or artifact is too large", ErrMemoryInputInvalid)
	}
	if in.SourceDigest != "" && !isSHA256Hex(in.SourceDigest) {
		return MemoryEvidenceInput{}, fmt.Errorf("%w: source digest must be lowercase SHA-256", ErrMemoryInputInvalid)
	}
	shapeCount := 0
	if in.SourceTranscriptID != "" && in.SourceSequenceFrom > 0 && in.SourceSequenceUntil >= in.SourceSequenceFrom {
		if in.SourceSequenceUntil-in.SourceSequenceFrom > 10000 {
			return MemoryEvidenceInput{}, fmt.Errorf("%w: transcript evidence range is too large", ErrMemoryInputInvalid)
		}
		shapeCount++
	}
	if in.SourceMemoryID != "" && in.SourceMemoryVersion > 0 {
		shapeCount++
	}
	if in.SourceMessageID != "" {
		shapeCount++
	}
	if in.SourceImportLocator != "" {
		shapeCount++
	}
	if len(in.ArtifactExcerpt) > 0 {
		shapeCount++
	}
	switch in.ResolutionState {
	case MemoryEvidencePending:
		if in.ExternalLocator == "" || in.ResolvedKind != "" || shapeCount != 0 || in.TerminalReasonCode != "" {
			return MemoryEvidenceInput{}, fmt.Errorf("%w: pending evidence requires only an external locator", ErrMemoryInputInvalid)
		}
	case MemoryEvidenceUnavailable:
		if in.TerminalReasonCode == "" || len(in.TerminalReasonCode) > 128 || in.ExternalLocator != "" || in.ResolvedKind != "" || shapeCount != 0 {
			return MemoryEvidenceInput{}, fmt.Errorf("%w: unavailable evidence requires only a reason code", ErrMemoryInputInvalid)
		}
	case MemoryEvidenceUnresolvable:
		if in.TerminalReasonCode == "" || len(in.TerminalReasonCode) > 128 || in.ExternalLocator != "" || in.ResolvedKind != "" || shapeCount != 0 {
			return MemoryEvidenceInput{}, fmt.Errorf("%w: unresolvable evidence requires only a reason code", ErrMemoryInputInvalid)
		}
	case MemoryEvidenceResolved:
		if in.ExternalLocator != "" || in.TerminalReasonCode != "" || shapeCount != 1 {
			return MemoryEvidenceInput{}, fmt.Errorf("%w: resolved evidence requires exactly one source", ErrMemoryInputInvalid)
		}
		validKind := (in.ResolvedKind == "transcript" && in.SourceTranscriptID != "") ||
			(in.ResolvedKind == "memory" && in.SourceMemoryID != "") ||
			(in.ResolvedKind == "message" && in.SourceMessageID != "") ||
			(in.ResolvedKind == "import_artifact" && in.SourceImportLocator != "") ||
			(in.ResolvedKind == "artifact" && len(in.ArtifactExcerpt) > 0)
		if !validKind {
			return MemoryEvidenceInput{}, fmt.Errorf("%w: resolved kind does not match source", ErrMemoryInputInvalid)
		}
	default:
		return MemoryEvidenceInput{}, fmt.Errorf("%w: invalid evidence resolution state", ErrMemoryInputInvalid)
	}
	return in, nil
}

func normalizeAdjustMemoryInput(in AdjustMemoryInput) (AdjustMemoryInput, error) {
	if in.ExpectedVersion < 1 {
		return AdjustMemoryInput{}, fmt.Errorf("%w: expected_version must be positive", ErrMemoryInputInvalid)
	}
	if in.Tags != nil && (len(in.AddTags) > 0 || len(in.RemoveTags) > 0) {
		return AdjustMemoryInput{}, fmt.Errorf("%w: set_tags cannot be combined with add/remove tags", ErrMemoryInputInvalid)
	}
	if in.Links != nil && (len(in.AddLinks) > 0 || len(in.RemoveLinks) > 0) {
		return AdjustMemoryInput{}, fmt.Errorf("%w: set_links cannot be combined with add/remove links", ErrMemoryInputInvalid)
	}
	if in.OccurredFrom != nil && in.ClearOccurredFrom || in.OccurredUntil != nil && in.ClearOccurredUntil {
		return AdjustMemoryInput{}, fmt.Errorf("%w: occurrence set and clear are mutually exclusive", ErrMemoryInputInvalid)
	}
	var err error
	if in.Tags != nil {
		value, e := normalizeMemoryStrings(*in.Tags, 64, 128, "tag")
		if e != nil {
			return AdjustMemoryInput{}, e
		}
		in.Tags = &value
	}
	in.AddTags, err = normalizeMemoryStrings(in.AddTags, 64, 128, "tag")
	if err != nil {
		return AdjustMemoryInput{}, err
	}
	in.RemoveTags, err = normalizeMemoryStrings(in.RemoveTags, 64, 128, "tag")
	if err != nil {
		return AdjustMemoryInput{}, err
	}
	if in.Links != nil {
		value, e := normalizeMemoryStrings(*in.Links, 256, 2048, "link")
		if e != nil {
			return AdjustMemoryInput{}, e
		}
		in.Links = &value
	}
	in.AddLinks, err = normalizeMemoryStrings(in.AddLinks, 256, 2048, "link")
	if err != nil {
		return AdjustMemoryInput{}, err
	}
	in.RemoveLinks, err = normalizeMemoryStrings(in.RemoveLinks, 256, 2048, "link")
	if err != nil {
		return AdjustMemoryInput{}, err
	}
	if in.Content != nil && (strings.TrimSpace(*in.Content) == "" || len(*in.Content) > maxMemoryContentBytes) {
		return AdjustMemoryInput{}, fmt.Errorf("%w: content must contain 1-%d bytes", ErrMemoryInputInvalid, maxMemoryContentBytes)
	}
	if in.ContentEncoding != nil {
		value := strings.TrimSpace(*in.ContentEncoding)
		in.ContentEncoding = &value
	}
	if in.Kind != nil {
		value := strings.TrimSpace(*in.Kind)
		if !memoryKindPattern.MatchString(value) {
			return AdjustMemoryInput{}, fmt.Errorf("%w: invalid kind", ErrMemoryInputInvalid)
		}
		in.Kind = &value
	}
	if in.Salience != nil && (*in.Salience < 0 || *in.Salience > 1) {
		return AdjustMemoryInput{}, fmt.Errorf("%w: salience must be between 0 and 1", ErrMemoryInputInvalid)
	}
	if in.OccurredFrom != nil {
		v := in.OccurredFrom.UTC()
		in.OccurredFrom = &v
	}
	if in.OccurredUntil != nil {
		v := in.OccurredUntil.UTC()
		in.OccurredUntil = &v
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Reason) > 2048 {
		return AdjustMemoryInput{}, fmt.Errorf("%w: reason is too large", ErrMemoryInputInvalid)
	}
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if err := validateMemoryIdempotencyKey(in.IdempotencyKey); err != nil {
		return AdjustMemoryInput{}, err
	}
	if err := normalizeMemoryClient(&in.Client); err != nil {
		return AdjustMemoryInput{}, err
	}
	return in, nil
}

func applyMemoryAdjustment(current *Memory, in AdjustMemoryInput) error {
	if in.Content != nil {
		current.Content = *in.Content
	}
	if in.ContentEncoding != nil {
		current.ContentEncoding = *in.ContentEncoding
	}
	if in.Kind != nil {
		current.Kind = *in.Kind
	}
	if in.Tags != nil {
		current.Tags = append([]string(nil), (*in.Tags)...)
	} else {
		current.Tags = adjustMemoryStrings(current.Tags, in.AddTags, in.RemoveTags)
	}
	if in.Links != nil {
		current.Links = append([]string(nil), (*in.Links)...)
	} else {
		current.Links = adjustMemoryStrings(current.Links, in.AddLinks, in.RemoveLinks)
	}
	if in.Salience != nil {
		current.Salience = *in.Salience
	}
	if in.Sensitive != nil {
		current.Sensitive = *in.Sensitive
	}
	if in.ClearOccurredFrom {
		current.OccurredFrom = nil
	} else if in.OccurredFrom != nil {
		v := *in.OccurredFrom
		current.OccurredFrom = &v
	}
	if in.ClearOccurredUntil {
		current.OccurredUntil = nil
	} else if in.OccurredUntil != nil {
		v := *in.OccurredUntil
		current.OccurredUntil = &v
	}
	return validateMemorySnapshot(*current)
}

func validateMemorySnapshot(m Memory) error {
	if strings.TrimSpace(m.Content) == "" || len(m.Content) > maxMemoryContentBytes {
		return fmt.Errorf("%w: invalid memory content", ErrMemoryInputInvalid)
	}
	if err := validateMemoryContentEncoding(m.Content, m.ContentEncoding); err != nil {
		return err
	}
	if !memoryKindPattern.MatchString(m.Kind) {
		return fmt.Errorf("%w: invalid kind", ErrMemoryInputInvalid)
	}
	if m.Salience < 0 || m.Salience > 1 {
		return fmt.Errorf("%w: salience must be between 0 and 1", ErrMemoryInputInvalid)
	}
	if m.OccurredFrom != nil && m.OccurredUntil != nil && m.OccurredUntil.Before(*m.OccurredFrom) {
		return fmt.Errorf("%w: occurred_until precedes occurred_from", ErrMemoryInputInvalid)
	}
	return nil
}

func normalizeMemoryLifecycleInput(in MemoryLifecycleInput, operation string) (MemoryLifecycleInput, error) {
	if in.ExpectedVersion < 1 {
		return MemoryLifecycleInput{}, fmt.Errorf("%w: expected_version must be positive", ErrMemoryInputInvalid)
	}
	if operation != "reactivated" && in.ExpectedSupersessionSetRevision != nil {
		return MemoryLifecycleInput{}, fmt.Errorf("%w: supersession revision is only valid for reactivate", ErrMemoryInputInvalid)
	}
	if in.ExpectedSupersessionSetRevision != nil && *in.ExpectedSupersessionSetRevision < 1 {
		return MemoryLifecycleInput{}, fmt.Errorf("%w: supersession revision must be positive", ErrMemoryInputInvalid)
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Reason) > 2048 {
		return MemoryLifecycleInput{}, fmt.Errorf("%w: reason is too large", ErrMemoryInputInvalid)
	}
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if err := validateMemoryIdempotencyKey(in.IdempotencyKey); err != nil {
		return MemoryLifecycleInput{}, err
	}
	if err := normalizeMemoryClient(&in.Client); err != nil {
		return MemoryLifecycleInput{}, err
	}
	return in, nil
}

func normalizeResolveMemoryEvidenceInput(in ResolveMemoryEvidenceInput) (ResolveMemoryEvidenceInput, error) {
	in.ResolvedKind = strings.TrimSpace(in.ResolvedKind)
	in.SourceTranscriptID = strings.TrimSpace(in.SourceTranscriptID)
	in.SourceMemoryID = strings.TrimSpace(in.SourceMemoryID)
	in.SourceMessageID = strings.TrimSpace(in.SourceMessageID)
	in.SourceImportLocator = strings.TrimSpace(in.SourceImportLocator)
	in.UnresolvableReason = strings.TrimSpace(in.UnresolvableReason)
	in.SourceDigest = strings.ToLower(strings.TrimSpace(in.SourceDigest))
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if err := validateMemoryIdempotencyKey(in.IdempotencyKey); err != nil {
		return ResolveMemoryEvidenceInput{}, err
	}
	if in.UnresolvableReason != "" {
		if len(in.UnresolvableReason) > 128 || in.ResolvedKind != "" || in.SourceTranscriptID != "" || in.SourceMemoryID != "" || in.SourceMessageID != "" || in.SourceImportLocator != "" || len(in.ArtifactExcerpt) > 0 {
			return ResolveMemoryEvidenceInput{}, fmt.Errorf("%w: unresolvable reason cannot include a source", ErrMemoryInputInvalid)
		}
	} else if in.ResolvedKind == "" {
		return ResolveMemoryEvidenceInput{}, fmt.Errorf("%w: resolved kind or unresolvable reason is required", ErrMemoryInputInvalid)
	}
	if in.SourceDigest != "" && !isSHA256Hex(in.SourceDigest) {
		return ResolveMemoryEvidenceInput{}, fmt.Errorf("%w: source digest must be lowercase SHA-256", ErrMemoryInputInvalid)
	}
	if len(in.SourceImportLocator) > 2048 || len(in.ArtifactExcerpt) > 65536 {
		return ResolveMemoryEvidenceInput{}, fmt.Errorf("%w: evidence source is too large", ErrMemoryInputInvalid)
	}
	return in, nil
}

func normalizeMemoryListOptions(p Principal, opts MemoryListOptions) (MemoryListOptions, error) {
	opts.OwnerAgentID = strings.TrimSpace(opts.OwnerAgentID)
	if opts.OwnerAgentID == "" {
		opts.OwnerAgentID = p.ID
	}
	if opts.OwnerAgentID != p.ID {
		return MemoryListOptions{}, ErrMemoryForbidden
	}
	if opts.State == "" {
		opts.State = MemoryStateActive
	}
	switch opts.State {
	case MemoryStateActive, MemoryStateSuperseded, MemoryStateForgotten, MemoryStateReverted:
	default:
		return MemoryListOptions{}, fmt.Errorf("%w: invalid state filter", ErrMemoryInputInvalid)
	}
	if opts.Kind != "" && !memoryKindPattern.MatchString(opts.Kind) {
		return MemoryListOptions{}, fmt.Errorf("%w: invalid kind filter", ErrMemoryInputInvalid)
	}
	opts.Origin = strings.TrimSpace(opts.Origin)
	opts.CaptureReason = strings.TrimSpace(opts.CaptureReason)
	if opts.Origin != "" && !memoryCodePattern.MatchString(opts.Origin) ||
		opts.CaptureReason != "" && !memoryCodePattern.MatchString(opts.CaptureReason) {
		return MemoryListOptions{}, fmt.Errorf("%w: origin or capture reason filter is invalid", ErrMemoryInputInvalid)
	}
	var err error
	opts.Tags, err = normalizeMemoryStrings(opts.Tags, 64, 128, "tag")
	if err != nil {
		return MemoryListOptions{}, err
	}
	if opts.Limit == 0 {
		opts.Limit = defaultMemoryPageSize
	}
	if opts.Limit < 1 || opts.Limit > maxMemoryPageSize {
		return MemoryListOptions{}, fmt.Errorf("%w: limit must be 1-%d", ErrMemoryInputInvalid, maxMemoryPageSize)
	}
	if opts.Cursor != "" && (opts.AfterUpdatedAt != nil || opts.AfterID != "" || opts.AfterSalience != nil) {
		return MemoryListOptions{}, fmt.Errorf("%w: cursor cannot be combined with internal page keys", ErrMemoryInputInvalid)
	}
	if opts.Cursor != "" {
		updatedAt, memoryID, bySalience, salience, err := decodeMemoryListCursor(opts.Cursor)
		if err != nil {
			return MemoryListOptions{}, err
		}
		if bySalience != opts.OrderBySalience {
			return MemoryListOptions{}, fmt.Errorf("%w: cursor order does not match request", ErrMemoryInputInvalid)
		}
		opts.AfterUpdatedAt = &updatedAt
		opts.AfterID = memoryID
		opts.AfterSalience = salience
	}
	if (opts.AfterUpdatedAt == nil) != (opts.AfterID == "") {
		return MemoryListOptions{}, fmt.Errorf("%w: pagination activity and id must be supplied together", ErrMemoryInputInvalid)
	}
	if opts.AfterUpdatedAt != nil {
		value := opts.AfterUpdatedAt.UTC()
		opts.AfterUpdatedAt = &value
	}
	if opts.OrderBySalience && opts.AfterUpdatedAt != nil && opts.AfterSalience == nil {
		return MemoryListOptions{}, fmt.Errorf("%w: salient cursor requires salience", ErrMemoryInputInvalid)
	}
	for _, item := range []**time.Time{
		&opts.OccurredFrom, &opts.OccurredUntil,
		&opts.CapturedFrom, &opts.CapturedUntil,
	} {
		if *item != nil {
			value := (*item).UTC()
			*item = &value
		}
	}
	if opts.OccurredFrom != nil && opts.OccurredUntil != nil &&
		opts.OccurredUntil.Before(*opts.OccurredFrom) {
		return MemoryListOptions{}, fmt.Errorf("%w: occurred range is reversed", ErrMemoryInputInvalid)
	}
	if opts.CapturedFrom != nil && opts.CapturedUntil != nil &&
		opts.CapturedUntil.Before(*opts.CapturedFrom) {
		return MemoryListOptions{}, fmt.Errorf("%w: captured range is reversed", ErrMemoryInputInvalid)
	}
	return opts, nil
}

func normalizeMemoryHistoryOptions(memoryID string, opts MemoryHistoryOptions) (MemoryHistoryOptions, error) {
	if opts.Limit == 0 {
		opts.Limit = defaultMemoryPageSize
	}
	if opts.Limit < 1 || opts.Limit > maxMemoryPageSize {
		return MemoryHistoryOptions{}, fmt.Errorf("%w: limit must be 1-%d", ErrMemoryInputInvalid, maxMemoryPageSize)
	}
	opts.Cursor = strings.TrimSpace(opts.Cursor)
	if opts.Cursor != "" && opts.AfterVersion != 0 {
		return MemoryHistoryOptions{}, fmt.Errorf("%w: cursor cannot be combined with an internal page key", ErrMemoryInputInvalid)
	}
	if opts.AfterVersion < 0 {
		return MemoryHistoryOptions{}, fmt.Errorf("%w: after version must not be negative", ErrMemoryInputInvalid)
	}
	if opts.Cursor != "" {
		cursorMemoryID, afterVersion, err := decodeMemoryHistoryCursor(opts.Cursor)
		if err != nil {
			return MemoryHistoryOptions{}, err
		}
		if cursorMemoryID != memoryID {
			return MemoryHistoryOptions{}, fmt.Errorf("%w: memory history cursor does not match resource", ErrMemoryInputInvalid)
		}
		opts.AfterVersion = afterVersion
	}
	return opts, nil
}

type memoryHistoryCursor struct {
	Version      int    `json:"v"`
	MemoryID     string `json:"memory_id"`
	AfterVersion int64  `json:"after_version"`
}

func encodeMemoryHistoryCursor(memoryID string, afterVersion int64) (string, error) {
	if !validMemoryID(memoryID) || afterVersion < 1 {
		return "", fmt.Errorf("%w: invalid memory history cursor key", ErrMemoryInputInvalid)
	}
	raw, err := json.Marshal(memoryHistoryCursor{
		Version: 1, MemoryID: memoryID, AfterVersion: afterVersion,
	})
	if err != nil {
		return "", fmt.Errorf("encode memory history cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeMemoryHistoryCursor(cursor string) (string, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil {
		return "", 0, fmt.Errorf("%w: invalid memory history cursor", ErrMemoryInputInvalid)
	}
	var value memoryHistoryCursor
	if err := json.Unmarshal(raw, &value); err != nil || value.Version != 1 ||
		!validMemoryID(value.MemoryID) || value.AfterVersion < 1 {
		return "", 0, fmt.Errorf("%w: invalid memory history cursor", ErrMemoryInputInvalid)
	}
	return value.MemoryID, value.AfterVersion, nil
}

type memoryListCursor struct {
	Version   int       `json:"v"`
	Order     string    `json:"order"`
	Salience  *float64  `json:"salience,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	MemoryID  string    `json:"memory_id"`
}

func encodeMemoryListCursor(updatedAt time.Time, memoryID string) (string, error) {
	return encodeMemoryListCursorForOrder(updatedAt, memoryID, false, 0)
}

func encodeMemoryListCursorForOrder(updatedAt time.Time, memoryID string, bySalience bool, salience float64) (string, error) {
	order := "activity"
	var cursorSalience *float64
	if bySalience {
		order = "salience"
		cursorSalience = &salience
	}
	raw, err := json.Marshal(memoryListCursor{
		Version: 1, Order: order, Salience: cursorSalience,
		UpdatedAt: updatedAt.UTC(), MemoryID: memoryID,
	})
	if err != nil {
		return "", fmt.Errorf("encode memory cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeMemoryListCursor(cursor string) (time.Time, string, bool, *float64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil {
		return time.Time{}, "", false, nil, fmt.Errorf("%w: invalid memory cursor", ErrMemoryInputInvalid)
	}
	var value memoryListCursor
	if err := json.Unmarshal(raw, &value); err != nil || value.Version != 1 ||
		value.UpdatedAt.IsZero() || !validMemoryID(value.MemoryID) ||
		(value.Order != "activity" && value.Order != "salience") ||
		(value.Order == "salience" && value.Salience == nil) ||
		(value.Order == "activity" && value.Salience != nil) {
		return time.Time{}, "", false, nil, fmt.Errorf("%w: invalid memory cursor", ErrMemoryInputInvalid)
	}
	return value.UpdatedAt.UTC(), value.MemoryID, value.Order == "salience", value.Salience, nil
}

func normalizeMemoryClient(client *MemoryClientProvenance) error {
	client.Runtime = strings.TrimSpace(client.Runtime)
	client.Model = strings.TrimSpace(client.Model)
	client.Recipe = strings.TrimSpace(client.Recipe)
	client.RecipeVersion = strings.TrimSpace(client.RecipeVersion)
	if len(client.Runtime) > 128 || len(client.Model) > 256 || len(client.Recipe) > 128 || len(client.RecipeVersion) > 128 {
		return fmt.Errorf("%w: client provenance is too large", ErrMemoryInputInvalid)
	}
	return nil
}

func normalizeMemoryStrings(values []string, maxItems, maxBytes int, label string) ([]string, error) {
	if len(values) > maxItems {
		return nil, fmt.Errorf("%w: too many %ss", ErrMemoryInputInvalid, label)
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > maxBytes {
			return nil, fmt.Errorf("%w: invalid %s", ErrMemoryInputInvalid, label)
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

func adjustMemoryStrings(current, add, remove []string) []string {
	set := make(map[string]struct{}, len(current)+len(add))
	for _, value := range current {
		set[value] = struct{}{}
	}
	for _, value := range add {
		set[value] = struct{}{}
	}
	for _, value := range remove {
		delete(set, value)
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func validateMemoryContentEncoding(content, encoding string) error {
	switch encoding {
	case "plain":
		return nil
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil || base64.StdEncoding.EncodeToString(decoded) != content {
			return fmt.Errorf("%w: content is not canonical base64", ErrMemoryInputInvalid)
		}
		return nil
	default:
		return fmt.Errorf("%w: content_encoding must be plain or base64", ErrMemoryInputInvalid)
	}
}

func validateMemoryIdempotencyKey(key string) error {
	if len(key) < 1 || len(key) > 512 {
		return fmt.Errorf("%w: idempotency key must contain 1-512 bytes", ErrMemoryInputInvalid)
	}
	return nil
}

func memoryRequestHash(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("canonicalize memory request: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func memoryContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func memoryIdempotencyKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func isSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func validMemoryID(value string) bool {
	return strings.HasPrefix(value, "mem_") && len(value) > 4 && len(value) <= 128
}

func memoryPayloadEqual(a, b Memory) bool {
	if a.Content != b.Content || a.ContentEncoding != b.ContentEncoding || a.Kind != b.Kind || a.Salience != b.Salience || a.Sensitive != b.Sensitive {
		return false
	}
	if !equalStrings(a.Tags, b.Tags) || !equalStrings(a.Links, b.Links) {
		return false
	}
	return equalOptionalTime(a.OccurredFrom, b.OccurredFrom) && equalOptionalTime(a.OccurredUntil, b.OccurredUntil)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
