package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// Memory curation constants define request and run states plus frozen input
// and source kinds understood by the store.
const (
	MemoryCurationRequestQueued     = "queued"
	MemoryCurationRequestClaimed    = "claimed"
	MemoryCurationRequestRetryWait  = "retry_wait"
	MemoryCurationRequestFulfilled  = "fulfilled"
	MemoryCurationRequestCancelled  = "cancelled"
	MemoryCurationRequestDeadLetter = "dead_letter"

	MemoryCurationRunOpen        = "open"
	MemoryCurationRunPlanned     = "planned"
	MemoryCurationRunApplied     = "applied"
	MemoryCurationRunRolledBack  = "rolled_back"
	MemoryCurationRunAbandoned   = "abandoned"
	MemoryCurationRunInterrupted = "interrupted"
	MemoryCurationRunConflict    = "conflict"

	MemoryCurationInputMemory     = "memory"
	MemoryCurationInputEvidence   = "evidence"
	MemoryCurationInputTranscript = "transcript"
	MemoryCurationInputCursor     = "cursor"

	MemoryCurationSourceMemory     = "memory"
	MemoryCurationSourceEvidence   = "evidence"
	MemoryCurationSourceTranscript = "transcript"

	defaultMemoryCurationPageSize        = 50
	maxMemoryCurationPageSize            = 200
	defaultMemoryCurationMemories        = 200
	maxMemoryCurationContextMemories     = 24
	maxMemoryCurationContextEntries      = 64
	maxMemoryCurationContextBodyChars    = 8192
	maxMemoryCurationContextTerms        = 128
	defaultMemoryCurationEvidence        = 500
	defaultMemoryCurationTranscriptItems = 500
	maxMemoryCurationMemories            = 500
	maxMemoryCurationEvidence            = 1000
	maxMemoryCurationTranscriptItems     = 2000
	defaultMemoryCurationAttempts        = 5
	maxMemoryCurationAttempts            = 20
	defaultMemoryCurationLease           = 5 * time.Minute
	minMemoryCurationLease               = 30 * time.Second
	maxMemoryCurationLease               = 30 * time.Minute
	maxMemoryCurationBackoff             = 24 * time.Hour
	memoryCurationPreviewCooldown        = 24 * time.Hour
	maxMemoryCurationGeneration          = int64(1<<62 - 1)

	// Byte budgets keep one materialized input and one input page small enough
	// for MCP transports that reject oversized frames. Freeze chunks a large
	// transcript window into multiple contiguous inputs; hydration elides
	// oversized bodies from windows frozen before chunking existed. Elided
	// content stays retrievable in full through the transcript tools.
	maxMemoryCurationInputBytes     = 256 * 1024
	maxMemoryCurationEntryBodyBytes = 16 * 1024
	maxMemoryCurationPageBytes      = 1 << 20
)

// Memory curation errors classify stable authorization, validation, lifecycle,
// lease, and idempotency failures returned by the store.
var (
	ErrMemoryCurationNotFound            = errors.New("memory curation resource not found")
	ErrMemoryCurationForbidden           = errors.New("memory curation access forbidden")
	ErrMemoryCurationInputInvalid        = errors.New("invalid memory curation input")
	ErrMemoryCurationConflict            = errors.New("memory curation state conflict")
	ErrMemoryCurationIdempotencyConflict = errors.New("memory curation idempotency key conflict")
	ErrMemoryCurationBusy                = errors.New("memory curation owner lane is busy")
	ErrMemoryCurationNotDue              = errors.New("memory curation request is not due")
	ErrMemoryCurationLeaseExpired        = errors.New("memory curation lease expired")
	ErrMemoryCurationFenceMismatch       = errors.New("memory curation fence mismatch")
)

// MemoryCurationScope is deterministic queue metadata, not a semantic prompt.
// Sources and states are finite server-understood filters. Sensitive content is
// excluded unless the caller opts in explicitly.
type MemoryCurationScope struct {
	Sources              []string `json:"sources"`
	MemoryStates         []string `json:"memory_states"`
	IncludeSensitive     bool     `json:"include_sensitive"`
	MaxMemories          int      `json:"max_memories"`
	MaxEvidence          int      `json:"max_evidence"`
	MaxTranscriptEntries int      `json:"max_transcript_entries"`
}

// RequestMemoryCurationInput describes one idempotent request for curator work.
type RequestMemoryCurationInput struct {
	Scope             MemoryCurationScope `json:"scope"`
	CoalescingKey     string              `json:"coalescing_key"`
	TriggerReason     string              `json:"trigger_reason"`
	TriggerGeneration int64               `json:"trigger_generation,omitempty"`
	Priority          int                 `json:"priority,omitempty"`
	DueAt             *time.Time          `json:"due_at,omitempty"`
	MaxAttempts       int                 `json:"max_attempts,omitempty"`
	IdempotencyKey    string              `json:"idempotency_key"`
}

// MemoryCurationInputCaps bounds the inputs materialized for one curator run.
type MemoryCurationInputCaps struct {
	MaxMemories          int `json:"max_memories,omitempty"`
	MaxEvidence          int `json:"max_evidence,omitempty"`
	MaxTranscriptEntries int `json:"max_transcript_entries,omitempty"`
}

// StartMemoryCurationInput describes an idempotent attempt to claim queue work.
type StartMemoryCurationInput struct {
	RequestID      string                  `json:"request_id"`
	Caps           MemoryCurationInputCaps `json:"caps,omitempty"`
	LeaseDuration  time.Duration           `json:"lease_duration,omitempty"`
	Client         MemoryClientProvenance  `json:"client,omitempty"`
	Budgets        json.RawMessage         `json:"budgets,omitempty"`
	IdempotencyKey string                  `json:"idempotency_key"`
}

// RenewMemoryCurationInput describes a fenced, idempotent lease extension.
type RenewMemoryCurationInput struct {
	FencingGeneration int64         `json:"fencing_generation"`
	Extension         time.Duration `json:"extension"`
	IdempotencyKey    string        `json:"idempotency_key"`
}

// FinishMemoryCurationInput describes a fenced cancel or abandon operation.
type FinishMemoryCurationInput struct {
	FencingGeneration int64  `json:"fencing_generation"`
	Reason            string `json:"reason,omitempty"`
	IdempotencyKey    string `json:"idempotency_key"`
}

// MemoryCurationLane holds queue generation and fencing state for one owner.
type MemoryCurationLane struct {
	AccountID         string    `json:"account_id"`
	RealmID           string    `json:"realm_id"`
	OwnerKind         string    `json:"owner_kind"`
	OwnerID           string    `json:"owner_id"`
	RequestGeneration int64     `json:"request_generation"`
	FencingGeneration int64     `json:"fencing_generation"`
	ActiveRunID       string    `json:"active_run_id,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// MemoryCurationRequest is one durable unit of queued curator work.
type MemoryCurationRequest struct {
	ID                  string              `json:"id"`
	AccountID           string              `json:"account_id"`
	RealmID             string              `json:"realm_id"`
	OwnerKind           string              `json:"owner_kind"`
	OwnerID             string              `json:"owner_id"`
	Scope               MemoryCurationScope `json:"scope"`
	CoalescingKey       string              `json:"coalescing_key"`
	TriggerReason       string              `json:"trigger_reason"`
	RequestGeneration   int64               `json:"request_generation"`
	Priority            int                 `json:"priority"`
	DueAt               time.Time           `json:"due_at"`
	State               string              `json:"state"`
	AttemptCount        int                 `json:"attempt_count"`
	MaxAttempts         int                 `json:"max_attempts"`
	ClaimedRunID        string              `json:"claimed_run_id,omitempty"`
	FulfilledGeneration int64               `json:"fulfilled_generation,omitempty"`
	ReplayRunID         string              `json:"replay_run_id,omitempty"`
	ReadOnlyReplay      bool                `json:"read_only_replay,omitempty"`
	ActorKind           string              `json:"actor_kind"`
	ActorID             string              `json:"actor_id"`
	IdempotencyKey      string              `json:"idempotency_key"`
	RequestHash         string              `json:"request_hash"`
	ClaimedAt           *time.Time          `json:"claimed_at,omitempty"`
	FulfilledAt         *time.Time          `json:"fulfilled_at,omitempty"`
	CancelledAt         *time.Time          `json:"cancelled_at,omitempty"`
	DeadLetteredAt      *time.Time          `json:"dead_lettered_at,omitempty"`
	CreatedAt           time.Time           `json:"created_at"`
	UpdatedAt           time.Time           `json:"updated_at"`
}

// MemoryCurationRun records one fenced attempt to process a curation request.
type MemoryCurationRun struct {
	ID                   string                 `json:"id"`
	AccountID            string                 `json:"account_id"`
	RealmID              string                 `json:"realm_id"`
	OwnerKind            string                 `json:"owner_kind"`
	OwnerID              string                 `json:"owner_id"`
	RequestID            string                 `json:"request_id"`
	RequestGeneration    int64                  `json:"request_generation"`
	FencingGeneration    int64                  `json:"fencing_generation"`
	State                string                 `json:"state"`
	ActorKind            string                 `json:"actor_kind"`
	ActorID              string                 `json:"actor_id"`
	IdempotencyKey       string                 `json:"idempotency_key"`
	RequestHash          string                 `json:"request_hash"`
	LeaseExpiresAt       *time.Time             `json:"lease_expires_at,omitempty"`
	Client               MemoryClientProvenance `json:"client"`
	MemoryChangeUpper    int64                  `json:"memory_change_upper"`
	EvidenceChangeUpper  int64                  `json:"evidence_change_upper"`
	InputCount           int                    `json:"input_count"`
	MemoryInputCount     int                    `json:"memory_input_count"`
	EvidenceInputCount   int                    `json:"evidence_input_count"`
	TranscriptInputCount int                    `json:"transcript_input_count"`
	CursorInputCount     int                    `json:"cursor_input_count"`
	PlanSchema           string                 `json:"plan_schema,omitempty"`
	PlanRevision         int64                  `json:"plan_revision,omitempty"`
	PlanHash             string                 `json:"plan_hash,omitempty"`
	ApplyReceiptID       string                 `json:"apply_receipt_id,omitempty"`
	RollbackReceiptID    string                 `json:"rollback_receipt_id,omitempty"`
	Budgets              json.RawMessage        `json:"budgets"`
	ConflictReasonCode   string                 `json:"conflict_reason_code,omitempty"`
	TerminalReasonCode   string                 `json:"terminal_reason_code,omitempty"`
	ScrubbedAt           *time.Time             `json:"scrubbed_at,omitempty"`
	ScrubbedReasonCode   string                 `json:"scrubbed_reason_code,omitempty"`
	CreatedAt            time.Time              `json:"created_at"`
	UpdatedAt            time.Time              `json:"updated_at"`
	StartedAt            time.Time              `json:"started_at"`
	PlannedAt            *time.Time             `json:"planned_at,omitempty"`
	AppliedAt            *time.Time             `json:"applied_at,omitempty"`
	RolledBackAt         *time.Time             `json:"rolled_back_at,omitempty"`
	AbandonedAt          *time.Time             `json:"abandoned_at,omitempty"`
	InterruptedAt        *time.Time             `json:"interrupted_at,omitempty"`
	TerminalAt           *time.Time             `json:"terminal_at,omitempty"`
}

// MemoryCurationMutationReceipt records a value-free idempotent state change.
type MemoryCurationMutationReceipt struct {
	Operation         string     `json:"operation"`
	ActorID           string     `json:"actor_id"`
	IdempotencyKey    string     `json:"idempotency_key"`
	RequestHash       string     `json:"request_hash"`
	RequestID         string     `json:"request_id,omitempty"`
	RunID             string     `json:"run_id,omitempty"`
	RequestGeneration int64      `json:"request_generation,omitempty"`
	FencingGeneration int64      `json:"fencing_generation,omitempty"`
	LeaseExpiresAt    *time.Time `json:"lease_expires_at,omitempty"`
	PlanRevision      int64      `json:"plan_revision,omitempty"`
	PlanHash          string     `json:"plan_hash,omitempty"`
	ResultState       string     `json:"result_state"`
	ReceiptID         string     `json:"receipt_id,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	Replayed          bool       `json:"replayed,omitempty"`
}

// RequestMemoryCurationResult returns a queue request and its mutation receipt.
type RequestMemoryCurationResult struct {
	Request MemoryCurationRequest         `json:"request"`
	Receipt MemoryCurationMutationReceipt `json:"receipt"`
}

// StartMemoryCurationResult returns a claimed run and its first input cursor.
type StartMemoryCurationResult struct {
	Run              MemoryCurationRun             `json:"run"`
	Request          MemoryCurationRequest         `json:"request"`
	Receipt          MemoryCurationMutationReceipt `json:"receipt"`
	FirstInputCursor string                        `json:"first_input_cursor"`
}

// RenewMemoryCurationResult returns the updated run and mutation receipt.
type RenewMemoryCurationResult struct {
	Run     MemoryCurationRun             `json:"run"`
	Receipt MemoryCurationMutationReceipt `json:"receipt"`
}

// FinishMemoryCurationResult returns a terminal run and mutation receipt.
type FinishMemoryCurationResult = RenewMemoryCurationResult

// MemoryCurationRunInput contains an immutable membership reference and, for
// data inputs, the exact payload resolved from that reference. Cursor inputs
// carry only value-free expected/upper sequence bounds.
type MemoryCurationRunInput struct {
	RunID               string            `json:"run_id"`
	Ordinal             int64             `json:"ordinal"`
	Kind                string            `json:"kind"`
	MemoryID            string            `json:"memory_id,omitempty"`
	MemoryVersion       int64             `json:"memory_version,omitempty"`
	EvidenceID          string            `json:"evidence_id,omitempty"`
	TranscriptID        string            `json:"transcript_id,omitempty"`
	SequenceFrom        int64             `json:"sequence_from,omitempty"`
	SequenceUntil       int64             `json:"sequence_until,omitempty"`
	CursorSourceKind    string            `json:"cursor_source_kind,omitempty"`
	CursorStreamID      string            `json:"cursor_stream_id,omitempty"`
	CursorExpectedPrior int64             `json:"cursor_expected_prior,omitempty"`
	CursorUpper         int64             `json:"cursor_upper,omitempty"`
	Memory              *Memory           `json:"memory,omitempty"`
	Evidence            *MemoryEvidence   `json:"evidence,omitempty"`
	TranscriptEntries   []TranscriptEntry `json:"transcript_entries,omitempty"`
	CreatedAt           time.Time         `json:"created_at"`
}

// MemoryCurationRunInputPage contains one page of immutable run inputs.
type MemoryCurationRunInputPage struct {
	Run        MemoryCurationRun        `json:"run"`
	Inputs     []MemoryCurationRunInput `json:"inputs"`
	NextCursor string                   `json:"next_cursor,omitempty"`
}

// MemoryCurationStatus summarizes an owner lane and its selected request and run.
type MemoryCurationStatus struct {
	Lane    MemoryCurationLane     `json:"lane"`
	Request *MemoryCurationRequest `json:"request,omitempty"`
	Run     *MemoryCurationRun     `json:"run,omitempty"`
}

type memoryCurationInputCursor struct {
	Version int    `json:"v"`
	RunID   string `json:"run_id"`
	Fence   int64  `json:"fence"`
	After   int64  `json:"after"`
}

const memoryCurationLaneSelectSQL = `
	SELECT account_id,realm_id,owner_kind,owner_id,request_generation,
	       fencing_generation,COALESCE(active_run_id,''),created_at,updated_at
	FROM memory_curation_lanes`

const memoryCurationRequestSelectSQL = `
	SELECT id,account_id,realm_id,owner_kind,owner_id,scope,coalescing_key,
	       trigger_reason,request_generation,priority,due_at,state,attempt_count,
	       max_attempts,COALESCE(claimed_run_id,''),fulfilled_generation,
	       COALESCE(replay_run_id,''),read_only_replay,actor_kind,actor_id,
	       idempotency_key,request_hash,claimed_at,fulfilled_at,cancelled_at,
	       dead_lettered_at,created_at,updated_at
	FROM memory_curation_requests`

const memoryCurationRunSelectSQL = `
	SELECT id,account_id,realm_id,owner_kind,owner_id,request_id,
	       request_generation,fencing_generation,state,actor_kind,actor_id,
	       idempotency_key,request_hash,lease_expires_at,client_runtime,
	       client_model,client_recipe,client_recipe_version,memory_change_upper,
	       evidence_change_upper,input_count,memory_input_count,
	       evidence_input_count,transcript_input_count,cursor_input_count,
	       plan_schema,plan_revision,plan_hash,apply_receipt_id,
	       rollback_receipt_id,conflict_reason_code,terminal_reason_code,budgets,
	       scrubbed_at,scrubbed_reason_code,started_at,planned_at,applied_at,
	       rolled_back_at,terminal_at,created_at,updated_at
	FROM memory_curation_runs`

type memoryCurationScanner interface {
	Scan(dest ...any) error
}

func scanMemoryCurationLane(row memoryCurationScanner) (MemoryCurationLane, error) {
	var out MemoryCurationLane
	err := row.Scan(&out.AccountID, &out.RealmID, &out.OwnerKind, &out.OwnerID,
		&out.RequestGeneration, &out.FencingGeneration, &out.ActiveRunID,
		&out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return MemoryCurationLane{}, err
	}
	return out, nil
}

func scanMemoryCurationRequest(row memoryCurationScanner) (MemoryCurationRequest, error) {
	var out MemoryCurationRequest
	var scopeJSON []byte
	var claimedAt, fulfilledAt, cancelledAt, deadLetteredAt sql.NullTime
	err := row.Scan(&out.ID, &out.AccountID, &out.RealmID, &out.OwnerKind, &out.OwnerID,
		&scopeJSON, &out.CoalescingKey, &out.TriggerReason, &out.RequestGeneration,
		&out.Priority, &out.DueAt, &out.State, &out.AttemptCount, &out.MaxAttempts,
		&out.ClaimedRunID, &out.FulfilledGeneration, &out.ReplayRunID,
		&out.ReadOnlyReplay, &out.ActorKind, &out.ActorID, &out.IdempotencyKey,
		&out.RequestHash, &claimedAt, &fulfilledAt, &cancelledAt, &deadLetteredAt,
		&out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return MemoryCurationRequest{}, err
	}
	if err := json.Unmarshal(scopeJSON, &out.Scope); err != nil {
		return MemoryCurationRequest{}, fmt.Errorf("decode curation request scope: %w", err)
	}
	out.ClaimedAt = nullTimePointer(claimedAt)
	out.FulfilledAt = nullTimePointer(fulfilledAt)
	out.CancelledAt = nullTimePointer(cancelledAt)
	out.DeadLetteredAt = nullTimePointer(deadLetteredAt)
	return out, nil
}

func scanMemoryCurationRun(row memoryCurationScanner) (MemoryCurationRun, error) {
	var out MemoryCurationRun
	var lease, scrubbed, planned, applied, rolledBack, terminal sql.NullTime
	var budgets []byte
	err := row.Scan(&out.ID, &out.AccountID, &out.RealmID, &out.OwnerKind,
		&out.OwnerID, &out.RequestID, &out.RequestGeneration,
		&out.FencingGeneration, &out.State, &out.ActorKind, &out.ActorID,
		&out.IdempotencyKey, &out.RequestHash, &lease, &out.Client.Runtime,
		&out.Client.Model, &out.Client.Recipe, &out.Client.RecipeVersion,
		&out.MemoryChangeUpper, &out.EvidenceChangeUpper, &out.InputCount,
		&out.MemoryInputCount, &out.EvidenceInputCount,
		&out.TranscriptInputCount, &out.CursorInputCount, &out.PlanSchema,
		&out.PlanRevision, &out.PlanHash, &out.ApplyReceiptID,
		&out.RollbackReceiptID, &out.ConflictReasonCode,
		&out.TerminalReasonCode, &budgets, &scrubbed, &out.ScrubbedReasonCode,
		&out.StartedAt, &planned, &applied, &rolledBack, &terminal,
		&out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return MemoryCurationRun{}, err
	}
	out.LeaseExpiresAt = nullTimePointer(lease)
	out.ScrubbedAt = nullTimePointer(scrubbed)
	out.PlannedAt = nullTimePointer(planned)
	out.AppliedAt = nullTimePointer(applied)
	out.RolledBackAt = nullTimePointer(rolledBack)
	out.TerminalAt = nullTimePointer(terminal)
	if terminal.Valid {
		switch out.State {
		case MemoryCurationRunAbandoned:
			out.AbandonedAt = out.TerminalAt
		case MemoryCurationRunInterrupted:
			out.InterruptedAt = out.TerminalAt
		}
	}
	out.Budgets = append(json.RawMessage(nil), budgets...)
	return out, nil
}

func nullTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	v := value.Time
	return &v
}

func loadMemoryCurationLaneTx(ctx context.Context, tx pgx.Tx, p Principal, forUpdate bool) (MemoryCurationLane, error) {
	query := memoryCurationLaneSelectSQL + ` WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	out, err := scanMemoryCurationLane(tx.QueryRow(ctx, query, p.AccountID, p.RealmID, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryCurationLane{}, ErrMemoryCurationNotFound
	}
	return out, err
}

func loadMemoryCurationRequest(ctx context.Context, q memoryQuerier, p Principal, requestID string, forUpdate bool) (MemoryCurationRequest, error) {
	query := memoryCurationRequestSelectSQL + ` WHERE id=$1 AND account_id=$2 AND realm_id=$3 AND owner_kind='agent' AND owner_id=$4`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	out, err := scanMemoryCurationRequest(q.QueryRow(ctx, query, requestID, p.AccountID, p.RealmID, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryCurationRequest{}, ErrMemoryCurationNotFound
	}
	if err != nil {
		return MemoryCurationRequest{}, fmt.Errorf("load curation request: %w", err)
	}
	return out, nil
}

func loadMemoryCurationRun(ctx context.Context, q memoryQuerier, p Principal, runID string, forUpdate bool) (MemoryCurationRun, error) {
	query := memoryCurationRunSelectSQL + ` WHERE id=$1 AND account_id=$2 AND realm_id=$3 AND owner_kind='agent' AND owner_id=$4`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	out, err := scanMemoryCurationRun(q.QueryRow(ctx, query, runID, p.AccountID, p.RealmID, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryCurationRun{}, ErrMemoryCurationNotFound
	}
	if err != nil {
		return MemoryCurationRun{}, fmt.Errorf("load curation run: %w", err)
	}
	return out, nil
}

func isRestrictedMemoryCurator(p Principal) bool {
	return p.AccessProfile == AccessProfileCuratorPreview ||
		p.AccessProfile == AccessProfileCuratorApply
}

func authorizeMemoryCurationRequestScope(p Principal, request MemoryCurationRequest) error {
	// Transcript entries do not yet carry a trustworthy sensitivity label. Raw
	// bodies, payloads, and artifacts can contain private values even when the
	// request's include_sensitive bit is false, so restricted credentials must
	// treat every transcript-bearing scope as sensitive-by-default. Full agent
	// credentials retain the existing transcript workflow.
	if isRestrictedMemoryCurator(p) && (request.Scope.IncludeSensitive ||
		curationHasSource(request.Scope, MemoryCurationSourceTranscript)) {
		return ErrMemoryCurationForbidden
	}
	return nil
}

// authorizeMemoryCurationRequestID derives the sensitive-content boundary from
// immutable, persisted request scope. Curator credentials must not be able to
// widen that boundary by changing tokens after a request or run was created.
func authorizeMemoryCurationRequestID(
	ctx context.Context,
	q memoryQuerier,
	p Principal,
	requestID string,
) error {
	if !isRestrictedMemoryCurator(p) {
		return nil
	}
	request, err := loadMemoryCurationRequest(ctx, q, p, requestID, false)
	if err != nil {
		return err
	}
	return authorizeMemoryCurationRequestScope(p, request)
}

func authorizeMemoryCurationRunID(
	ctx context.Context,
	q memoryQuerier,
	p Principal,
	runID string,
) error {
	if !isRestrictedMemoryCurator(p) {
		return nil
	}
	run, err := loadMemoryCurationRun(ctx, q, p, runID, false)
	if err != nil {
		return err
	}
	return authorizeMemoryCurationRequestID(ctx, q, p, run.RequestID)
}

func loadMemoryCurationMutation(ctx context.Context, q memoryQuerier, p Principal, operation, key, requestHash string) (MemoryCurationMutationReceipt, bool, error) {
	var out MemoryCurationMutationReceipt
	var lease sql.NullTime
	err := q.QueryRow(ctx, `
		SELECT operation,actor_id,idempotency_key,request_hash,
		       COALESCE(request_id,''),COALESCE(run_id,''),request_generation,
		       fencing_generation,plan_revision,plan_hash,lease_expires_at,
		       result_state,receipt_id,created_at
		FROM memory_curation_mutations
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND actor_kind='agent' AND actor_id=$3 AND operation=$4
		  AND idempotency_key=$5`, p.AccountID, p.RealmID, p.ID, operation, key).
		Scan(&out.Operation, &out.ActorID, &out.IdempotencyKey, &out.RequestHash,
			&out.RequestID, &out.RunID, &out.RequestGeneration,
			&out.FencingGeneration, &out.PlanRevision, &out.PlanHash, &lease,
			&out.ResultState, &out.ReceiptID, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryCurationMutationReceipt{}, false, nil
	}
	if err != nil {
		return MemoryCurationMutationReceipt{}, false, fmt.Errorf("load curation mutation receipt: %w", err)
	}
	if out.RequestHash != requestHash {
		return MemoryCurationMutationReceipt{}, false, ErrMemoryCurationIdempotencyConflict
	}
	out.LeaseExpiresAt = nullTimePointer(lease)
	out.Replayed = true
	return out, true, nil
}

func insertMemoryCurationMutation(ctx context.Context, tx pgx.Tx, p Principal, receipt MemoryCurationMutationReceipt) (MemoryCurationMutationReceipt, error) {
	mutationID, err := id.New("mcmu")
	if err != nil {
		return MemoryCurationMutationReceipt{}, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO memory_curation_mutations
		  (id,account_id,realm_id,owner_kind,owner_id,actor_kind,actor_id,
		   operation,idempotency_key,request_hash,request_id,run_id,
		   request_generation,fencing_generation,plan_revision,plan_hash,
		   lease_expires_at,result_state,receipt_id)
		VALUES ($1,$2,$3,'agent',$4,'agent',$4,$5,$6,$7,NULLIF($8,''),
		        NULLIF($9,''),$10,$11,$12,$13,$14,$15,$16)
		RETURNING created_at`, mutationID, p.AccountID, p.RealmID, p.ID,
		receipt.Operation, receipt.IdempotencyKey, receipt.RequestHash,
		receipt.RequestID, receipt.RunID, receipt.RequestGeneration,
		receipt.FencingGeneration, receipt.PlanRevision, receipt.PlanHash,
		receipt.LeaseExpiresAt, receipt.ResultState, receipt.ReceiptID).
		Scan(&receipt.CreatedAt)
	if err != nil {
		return MemoryCurationMutationReceipt{}, fmt.Errorf("insert curation mutation receipt: %w", err)
	}
	return receipt, nil
}

func prepareMemoryCurationMutationTx(ctx context.Context, tx pgx.Tx, p Principal, surface, key string) error {
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return err
	}
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return err
	}
	if err := lockFactIdempotencyKey(ctx, tx, p, "memory.curation."+surface, key); err != nil {
		return fmt.Errorf("lock curation idempotency key: %w", err)
	}
	return nil
}

func logMemoryCurationEventTx(ctx context.Context, tx pgx.Tx, p Principal, verb, requestID, runID string, requestGeneration, fence int64, state string) error {
	metadata := map[string]any{
		"request_id": requestID, "request_generation": strconv.FormatInt(requestGeneration, 10),
		"state": state,
	}
	if runID != "" {
		metadata["run_id"] = runID
	}
	if fence > 0 {
		metadata["fencing_generation"] = strconv.FormatInt(fence, 10)
	}
	return logEventTx(ctx, tx, EventInput{
		AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
		Verb: verb, Metadata: metadata,
	})
}

// RequestCuration increments the server-owned owner generation and creates or
// coalesces deterministic queue work. Every caller intent has an independent
// mutation receipt even when several intents resolve to one queue row.
func (s *Store) RequestCuration(ctx context.Context, p Principal, in RequestMemoryCurationInput) (RequestMemoryCurationResult, error) {
	if p.Kind != PrincipalAgent || isRestrictedMemoryCurator(p) {
		return RequestMemoryCurationResult{}, ErrMemoryCurationForbidden
	}
	in, err := normalizeRequestMemoryCurationInput(in)
	if err != nil {
		return RequestMemoryCurationResult{}, err
	}
	hashInput := in
	hashInput.IdempotencyKey = ""
	requestHash, err := memoryRequestHash(struct {
		Operation string                     `json:"operation"`
		Input     RequestMemoryCurationInput `json:"input"`
	}{Operation: "request", Input: hashInput})
	if err != nil {
		return RequestMemoryCurationResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RequestMemoryCurationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prepareMemoryCurationMutationTx(ctx, tx, p, "request", in.IdempotencyKey); err != nil {
		return RequestMemoryCurationResult{}, err
	}
	if receipt, ok, err := loadMemoryCurationMutation(ctx, tx, p, "request", in.IdempotencyKey, requestHash); err != nil || ok {
		if err != nil {
			return RequestMemoryCurationResult{}, err
		}
		request, err := loadMemoryCurationRequest(ctx, tx, p, receipt.RequestID, false)
		if err != nil {
			return RequestMemoryCurationResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RequestMemoryCurationResult{}, err
		}
		return RequestMemoryCurationResult{Request: request, Receipt: receipt}, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_curation_lanes
		  (account_id,realm_id,owner_kind,owner_id)
		VALUES ($1,$2,'agent',$3)
		ON CONFLICT DO NOTHING`, p.AccountID, p.RealmID, p.ID); err != nil {
		return RequestMemoryCurationResult{}, fmt.Errorf("initialize curation lane: %w", err)
	}
	lane, err := loadMemoryCurationLaneTx(ctx, tx, p, true)
	if err != nil {
		return RequestMemoryCurationResult{}, err
	}
	if lane.RequestGeneration >= maxMemoryCurationGeneration {
		return RequestMemoryCurationResult{}, fmt.Errorf("%w: request generation exhausted", ErrMemoryCurationConflict)
	}
	nextGeneration := lane.RequestGeneration + 1
	if in.TriggerGeneration > nextGeneration {
		nextGeneration = in.TriggerGeneration
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_curation_lanes
		SET request_generation=$5,updated_at=clock_timestamp()
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND request_generation=$4`, p.AccountID, p.RealmID, p.ID,
		lane.RequestGeneration, nextGeneration); err != nil {
		return RequestMemoryCurationResult{}, fmt.Errorf("advance curation request generation: %w", err)
	}

	scopeJSON, err := json.Marshal(in.Scope)
	if err != nil {
		return RequestMemoryCurationResult{}, err
	}
	var request MemoryCurationRequest
	request, err = scanMemoryCurationRequest(tx.QueryRow(ctx,
		memoryCurationRequestSelectSQL+`
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND coalescing_key=$4 AND state IN ('queued','claimed','retry_wait')
		ORDER BY created_at,id LIMIT 1 FOR UPDATE`, p.AccountID, p.RealmID, p.ID,
		in.CoalescingKey))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		requestID, mintErr := id.New("mcrq")
		if mintErr != nil {
			return RequestMemoryCurationResult{}, mintErr
		}
		request, err = scanMemoryCurationRequest(tx.QueryRow(ctx, `
			INSERT INTO memory_curation_requests
			  (id,account_id,realm_id,owner_kind,owner_id,scope,coalescing_key,
			   trigger_reason,request_generation,priority,due_at,state,attempt_count,
			   max_attempts,fulfilled_generation,read_only_replay,actor_kind,actor_id,
			   idempotency_key,request_hash)
			VALUES ($1,$2,$3,'agent',$4,$5::jsonb,$6,$7,$8,$9,
			        COALESCE($10,clock_timestamp()),'queued',0,$11,0,false,
			        'agent',$4,$12,$13)
			RETURNING id,account_id,realm_id,owner_kind,owner_id,scope,coalescing_key,
			          trigger_reason,request_generation,priority,due_at,state,
			          attempt_count,max_attempts,COALESCE(claimed_run_id,''),
			          fulfilled_generation,COALESCE(replay_run_id,''),read_only_replay,
			          actor_kind,actor_id,idempotency_key,request_hash,claimed_at,
			          fulfilled_at,cancelled_at,dead_lettered_at,created_at,updated_at`,
			requestID, p.AccountID, p.RealmID, p.ID, scopeJSON, in.CoalescingKey,
			in.TriggerReason, nextGeneration, in.Priority, in.DueAt,
			in.MaxAttempts, in.IdempotencyKey, requestHash))
	case err != nil:
		return RequestMemoryCurationResult{}, fmt.Errorf("find coalesced curation request: %w", err)
	default:
		existingScope, _ := json.Marshal(request.Scope)
		if string(existingScope) != string(scopeJSON) {
			return RequestMemoryCurationResult{}, fmt.Errorf("%w: coalescing key is already active with a different scope", ErrMemoryCurationConflict)
		}
		request, err = scanMemoryCurationRequest(tx.QueryRow(ctx, `
			UPDATE memory_curation_requests
			SET request_generation=$2,priority=GREATEST(priority,$3),
			    due_at=LEAST(due_at,COALESCE($4,clock_timestamp())),
			    max_attempts=GREATEST(max_attempts,$5),
			    state=CASE WHEN state='retry_wait' AND
			                    LEAST(due_at,COALESCE($4,clock_timestamp())) <= clock_timestamp()
			               THEN 'queued' ELSE state END,
			    updated_at=clock_timestamp()
			WHERE id=$1
			RETURNING id,account_id,realm_id,owner_kind,owner_id,scope,coalescing_key,
			          trigger_reason,request_generation,priority,due_at,state,
			          attempt_count,max_attempts,COALESCE(claimed_run_id,''),
			          fulfilled_generation,COALESCE(replay_run_id,''),read_only_replay,
			          actor_kind,actor_id,idempotency_key,request_hash,claimed_at,
			          fulfilled_at,cancelled_at,dead_lettered_at,created_at,updated_at`,
			request.ID, nextGeneration, in.Priority, in.DueAt, in.MaxAttempts))
	}
	if err != nil {
		return RequestMemoryCurationResult{}, fmt.Errorf("store curation request: %w", err)
	}
	receipt, err := insertMemoryCurationMutation(ctx, tx, p, MemoryCurationMutationReceipt{
		Operation: "request", ActorID: p.ID, IdempotencyKey: in.IdempotencyKey,
		RequestHash: requestHash, RequestID: request.ID,
		RequestGeneration: nextGeneration, ResultState: request.State,
	})
	if err != nil {
		return RequestMemoryCurationResult{}, err
	}
	if err := logMemoryCurationEventTx(ctx, tx, p, VerbMemoryCurationRequested,
		request.ID, "", nextGeneration, 0, request.State); err != nil {
		return RequestMemoryCurationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RequestMemoryCurationResult{}, err
	}
	return RequestMemoryCurationResult{Request: request, Receipt: receipt}, nil
}

func interruptExpiredCurationRunTx(ctx context.Context, tx pgx.Tx, p Principal, lane *MemoryCurationLane) (bool, error) {
	if lane.ActiveRunID == "" {
		return false, nil
	}
	run, err := loadMemoryCurationRun(ctx, tx, p, lane.ActiveRunID, true)
	if err != nil {
		return false, err
	}
	if run.State != MemoryCurationRunOpen && run.State != MemoryCurationRunPlanned {
		return false, fmt.Errorf("%w: active lane points to terminal run", ErrMemoryCurationConflict)
	}
	var expired bool
	if err := tx.QueryRow(ctx, `
		SELECT lease_expires_at IS NULL OR lease_expires_at <= clock_timestamp()
		FROM memory_curation_runs WHERE id=$1`, run.ID).Scan(&expired); err != nil {
		return false, fmt.Errorf("check curation lease: %w", err)
	}
	if !expired {
		return false, nil
	}
	if lane.FencingGeneration >= maxMemoryCurationGeneration {
		return false, fmt.Errorf("%w: fencing generation exhausted", ErrMemoryCurationConflict)
	}
	request, err := loadMemoryCurationRequest(ctx, tx, p, run.RequestID, true)
	if err != nil {
		return false, err
	}
	nextAttempt := request.AttemptCount + 1
	requestState := MemoryCurationRequestRetryWait
	terminalReason := "lease_expired"
	var dueAt any
	if nextAttempt >= request.MaxAttempts {
		requestState = MemoryCurationRequestDeadLetter
		dueAt = request.DueAt
	} else {
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return false, err
		}
		dueAt = now.Add(curationBackoff(nextAttempt))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_curation_runs
		SET state='interrupted',lease_expires_at=NULL,
		    terminal_reason_code=$2,terminal_at=clock_timestamp(),
		    updated_at=clock_timestamp()
		WHERE id=$1 AND state IN ('open','planned')`, run.ID, terminalReason); err != nil {
		return false, fmt.Errorf("interrupt expired curation run: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_curation_requests
		SET state=$2,attempt_count=$3,claimed_run_id=NULL,claimed_at=NULL,
		    due_at=$4,dead_lettered_at=CASE WHEN $2='dead_letter'
		                                    THEN clock_timestamp() ELSE NULL END,
		    updated_at=clock_timestamp()
		WHERE id=$1 AND state='claimed' AND claimed_run_id=$5`,
		request.ID, requestState, nextAttempt, dueAt, run.ID); err != nil {
		return false, fmt.Errorf("requeue expired curation request: %w", err)
	}
	lane.FencingGeneration++
	lane.ActiveRunID = ""
	if _, err := tx.Exec(ctx, `
		UPDATE memory_curation_lanes
		SET active_run_id=NULL,fencing_generation=$5,updated_at=clock_timestamp()
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND fencing_generation=$4`, p.AccountID, p.RealmID, p.ID,
		lane.FencingGeneration-1, lane.FencingGeneration); err != nil {
		return false, fmt.Errorf("fence expired curation run: %w", err)
	}
	if err := logMemoryCurationEventTx(ctx, tx, p, VerbMemoryCurationInterrupted,
		request.ID, run.ID, run.RequestGeneration, run.FencingGeneration,
		MemoryCurationRunInterrupted); err != nil {
		return false, err
	}
	return true, nil
}

func loadMemoryCurationCursorPosition(ctx context.Context, tx pgx.Tx, p Principal, sourceKind, streamID string) (int64, error) {
	var position int64
	err := tx.QueryRow(ctx, `
		SELECT position FROM memory_curation_cursors
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND source_kind=$4 AND source_stream_id=$5
		FOR SHARE`, p.AccountID, p.RealmID, p.ID, sourceKind, streamID).Scan(&position)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO memory_curation_cursors
			  (account_id,realm_id,owner_kind,owner_id,source_kind,source_stream_id,position)
			VALUES ($1,$2,'agent',$3,$4,$5,0)
			ON CONFLICT DO NOTHING`, p.AccountID, p.RealmID, p.ID, sourceKind, streamID); err != nil {
			return 0, fmt.Errorf("initialize curation cursor: %w", err)
		}
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load curation cursor: %w", err)
	}
	return position, nil
}

type memoryCurationInputCounts struct {
	total, memories, evidence, transcripts, cursors int
}

func insertMemoryCurationRunInputTx(ctx context.Context, tx pgx.Tx, p Principal, input *MemoryCurationRunInput, orderKey string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO memory_curation_run_inputs
		  (run_id,ordinal,account_id,realm_id,owner_kind,owner_id,input_kind,
		   order_key,memory_id,memory_version,evidence_id,transcript_id,
		   sequence_from,sequence_until,cursor_source_kind,cursor_stream_id,
		   cursor_expected_prior,cursor_upper)
		VALUES ($1,$2,$3,$4,'agent',$5,$6,$7,NULLIF($8,''),NULLIF($9,0),
		        NULLIF($10,''),NULLIF($11,''),NULLIF($12,0),NULLIF($13,0),
		        NULLIF($14,''),NULLIF($15,''),$16,$17)`, input.RunID, input.Ordinal,
		p.AccountID, p.RealmID, p.ID, input.Kind, orderKey, input.MemoryID,
		input.MemoryVersion, input.EvidenceID, input.TranscriptID,
		input.SequenceFrom, input.SequenceUntil, input.CursorSourceKind,
		input.CursorStreamID, nullableCurationCursorValue(input.Kind, input.CursorExpectedPrior),
		nullableCurationCursorValue(input.Kind, input.CursorUpper))
	if err != nil {
		return fmt.Errorf("insert curation run input: %w", err)
	}
	return nil
}

func nullableCurationCursorValue(kind string, value int64) any {
	if kind != MemoryCurationInputCursor {
		return nil
	}
	return value
}

func materializeMemoryCurationInputsTx(ctx context.Context, tx pgx.Tx, p Principal, runID string, scope MemoryCurationScope, caps MemoryCurationInputCaps) (memoryCurationInputCounts, int64, int64, error) {
	counts := memoryCurationInputCounts{}
	ordinal := int64(0)
	appendInput := func(input MemoryCurationRunInput, orderKey string) error {
		ordinal++
		input.RunID = runID
		input.Ordinal = ordinal
		if err := insertMemoryCurationRunInputTx(ctx, tx, p, &input, orderKey); err != nil {
			return err
		}
		counts.total++
		switch input.Kind {
		case MemoryCurationInputMemory:
			counts.memories++
		case MemoryCurationInputEvidence:
			counts.evidence++
		case MemoryCurationInputTranscript:
			counts.transcripts++
		case MemoryCurationInputCursor:
			counts.cursors++
		}
		return nil
	}

	var memoryUpper int64
	err := tx.QueryRow(ctx, `
		SELECT last_change_seq FROM memory_change_clocks
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		FOR SHARE`, p.AccountID, p.RealmID, p.ID).Scan(&memoryUpper)
	if errors.Is(err, pgx.ErrNoRows) {
		memoryUpper = 0
	} else if err != nil {
		return counts, 0, 0, fmt.Errorf("snapshot memory change clock: %w", err)
	}
	evidenceUpper := memoryUpper

	if curationHasSource(scope, MemoryCurationSourceMemory) {
		streamID, err := memoryCurationFilteredStreamID(MemoryCurationSourceMemory, scope)
		if err != nil {
			return counts, 0, 0, err
		}
		expected, err := loadMemoryCurationCursorPosition(ctx, tx, p, MemoryCurationSourceMemory, streamID)
		if err != nil {
			return counts, 0, 0, err
		}
		type ref struct {
			id      string
			version int64
			seq     int64
		}
		rows, err := tx.Query(ctx, `
			SELECT v.memory_id,v.version,v.change_seq
			FROM memories m
			JOIN memory_versions v ON v.memory_id=m.id AND v.version=m.current_version
			WHERE m.account_id=$1 AND m.realm_id=$2 AND m.owner_kind='agent' AND m.owner_id=$3
			  AND v.change_seq > $4 AND v.change_seq <= $5
			  AND v.state=ANY($6::text[]) AND ($7 OR NOT v.sensitive)
			ORDER BY v.change_seq,v.memory_id
			LIMIT $8`, p.AccountID, p.RealmID, p.ID, expected, memoryUpper,
			scope.MemoryStates, scope.IncludeSensitive, caps.MaxMemories+1)
		if err != nil {
			return counts, 0, 0, fmt.Errorf("select curation memory inputs: %w", err)
		}
		refs := make([]ref, 0, caps.MaxMemories+1)
		for rows.Next() {
			var item ref
			if err := rows.Scan(&item.id, &item.version, &item.seq); err != nil {
				rows.Close()
				return counts, 0, 0, err
			}
			refs = append(refs, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return counts, 0, 0, err
		}
		rows.Close()
		cursorUpper := memoryUpper
		if len(refs) > caps.MaxMemories {
			refs = refs[:caps.MaxMemories]
			cursorUpper = refs[len(refs)-1].seq
		}
		if cursorUpper > expected {
			if err := appendInput(MemoryCurationRunInput{
				Kind: MemoryCurationInputCursor, CursorSourceKind: MemoryCurationSourceMemory,
				CursorStreamID: streamID, CursorExpectedPrior: expected,
				CursorUpper: cursorUpper,
			}, "01/cursor/memory/"+streamID); err != nil {
				return counts, 0, 0, err
			}
		}
		for _, item := range refs {
			if err := appendInput(MemoryCurationRunInput{
				Kind: MemoryCurationInputMemory, MemoryID: item.id, MemoryVersion: item.version,
			}, fmt.Sprintf("02/memory/%020d/%s", item.seq, item.id)); err != nil {
				return counts, 0, 0, err
			}
		}
	}

	if curationHasSource(scope, MemoryCurationSourceEvidence) {
		streamID, err := memoryCurationFilteredStreamID(MemoryCurationSourceEvidence, scope)
		if err != nil {
			return counts, 0, 0, err
		}
		expected, err := loadMemoryCurationCursorPosition(ctx, tx, p, MemoryCurationSourceEvidence, streamID)
		if err != nil {
			return counts, 0, 0, err
		}
		type ref struct {
			id  string
			seq int64
		}
		rows, err := tx.Query(ctx, `
			SELECT e.id,e.evidence_change_seq
			FROM memory_evidence e
			JOIN memory_versions v ON v.memory_id=e.memory_id AND v.version=e.target_version
			WHERE e.account_id=$1 AND e.realm_id=$2 AND e.owner_kind='agent' AND e.owner_id=$3
			  AND e.evidence_change_seq > $4 AND e.evidence_change_seq <= $5
			  AND v.state=ANY($6::text[])
			  AND ($7 OR (NOT v.sensitive AND NOT e.artifact_sensitive))
			ORDER BY e.evidence_change_seq,e.id
			LIMIT $8`, p.AccountID, p.RealmID, p.ID, expected, evidenceUpper,
			scope.MemoryStates, scope.IncludeSensitive, caps.MaxEvidence+1)
		if err != nil {
			return counts, 0, 0, fmt.Errorf("select curation evidence inputs: %w", err)
		}
		refs := make([]ref, 0, caps.MaxEvidence+1)
		for rows.Next() {
			var item ref
			if err := rows.Scan(&item.id, &item.seq); err != nil {
				rows.Close()
				return counts, 0, 0, err
			}
			refs = append(refs, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return counts, 0, 0, err
		}
		rows.Close()
		cursorUpper := evidenceUpper
		if len(refs) > caps.MaxEvidence {
			refs = refs[:caps.MaxEvidence]
			cursorUpper = refs[len(refs)-1].seq
		}
		if cursorUpper > expected {
			if err := appendInput(MemoryCurationRunInput{
				Kind: MemoryCurationInputCursor, CursorSourceKind: MemoryCurationSourceEvidence,
				CursorStreamID: streamID, CursorExpectedPrior: expected,
				CursorUpper: cursorUpper,
			}, "03/cursor/evidence/"+streamID); err != nil {
				return counts, 0, 0, err
			}
		}
		for _, item := range refs {
			if err := appendInput(MemoryCurationRunInput{
				Kind: MemoryCurationInputEvidence, EvidenceID: item.id,
			}, fmt.Sprintf("04/evidence/%020d/%s", item.seq, item.id)); err != nil {
				return counts, 0, 0, err
			}
		}
	}

	if curationHasSource(scope, MemoryCurationSourceTranscript) && caps.MaxTranscriptEntries > 0 {
		rows, err := tx.Query(ctx, `
			SELECT c.id,c.next_sequence-1
			FROM transcript_conversations c
			LEFT JOIN memory_curation_cursors cursor
			  ON cursor.account_id=c.account_id AND cursor.realm_id=c.realm_id
			 AND cursor.owner_kind='agent' AND cursor.owner_id=c.owner_agent_id
			 AND cursor.source_kind='transcript' AND cursor.source_stream_id=c.id
			JOIN transcript_entries oldest_unread
			  ON oldest_unread.transcript_id=c.id
			 AND oldest_unread.account_id=c.account_id
			 AND oldest_unread.realm_id=c.realm_id
			 AND oldest_unread.recorded_by_agent_id=c.owner_agent_id
			 AND oldest_unread.sequence=COALESCE(cursor.position,0)+1
			WHERE c.account_id=$1 AND c.realm_id=$2 AND c.owner_agent_id=$3
			  AND c.next_sequence-1 > COALESCE(cursor.position,0)
			ORDER BY oldest_unread.created_at,oldest_unread.id,c.id
			LIMIT $4 FOR SHARE OF c`, p.AccountID, p.RealmID, p.ID,
			caps.MaxTranscriptEntries)
		if err != nil {
			return counts, 0, 0, fmt.Errorf("lock curation transcript streams: %w", err)
		}
		type stream struct {
			id    string
			upper int64
		}
		streams := make([]stream, 0)
		for rows.Next() {
			var item stream
			if err := rows.Scan(&item.id, &item.upper); err != nil {
				rows.Close()
				return counts, 0, 0, err
			}
			streams = append(streams, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return counts, 0, 0, err
		}
		rows.Close()
		remaining := caps.MaxTranscriptEntries
		for index, stream := range streams {
			if remaining == 0 {
				break
			}
			expected, err := loadMemoryCurationCursorPosition(ctx, tx, p, MemoryCurationSourceTranscript, stream.id)
			if err != nil {
				return counts, 0, 0, err
			}
			if stream.upper <= expected {
				continue
			}
			upper := stream.upper
			available := upper - expected
			// Share the remaining entry budget across every selected stream. A
			// single noisy conversation therefore cannot consume the entire run
			// while another selected conversation receives no cursor interval.
			streamsRemaining := len(streams) - index
			fairShare := remaining / streamsRemaining
			if fairShare < 1 {
				fairShare = 1
			}
			if available > int64(fairShare) {
				upper = expected + int64(fairShare)
			}
			if err := appendInput(MemoryCurationRunInput{
				Kind: MemoryCurationInputCursor, CursorSourceKind: MemoryCurationSourceTranscript,
				CursorStreamID: stream.id, CursorExpectedPrior: expected, CursorUpper: upper,
			}, fmt.Sprintf("05/cursor/transcript/%s", stream.id)); err != nil {
				return counts, 0, 0, err
			}
			sizes, err := loadMemoryCurationTranscriptEntrySizes(ctx, tx, p,
				stream.id, expected+1, upper)
			if err != nil {
				return counts, 0, 0, err
			}
			for _, slice := range chunkMemoryCurationTranscriptWindow(sizes,
				expected+1, upper, maxMemoryCurationInputBytes) {
				if err := appendInput(MemoryCurationRunInput{
					Kind: MemoryCurationInputTranscript, TranscriptID: stream.id,
					SequenceFrom: slice.From, SequenceUntil: slice.Until,
				}, fmt.Sprintf("06/transcript/%s/%020d", stream.id, slice.From)); err != nil {
					return counts, 0, 0, err
				}
			}
			remaining -= int(upper - expected)
		}
	}

	// A transcript delta often refines a narrative that was reviewed in an
	// earlier run. Changed-memory inputs alone cannot authorize a replace or
	// supersede in that case, so freeze a small, deterministic set of existing
	// heads as comparison context. Rank indexed lexical matches to a bounded
	// sample of the frozen transcript evidence before filling remaining slots
	// by salience. These inputs do not advance the memory cursor or create
	// backlog work; they only let the foreground curator avoid duplicating a
	// durable narrative and safely target a current head when a refinement is
	// warranted. Respect the requested memory states and memory-source scope, and
	// keep the total inside the caller's memory cap.
	if counts.transcripts > 0 && curationHasSource(scope, MemoryCurationSourceMemory) &&
		counts.memories < caps.MaxMemories {
		contextLimit := caps.MaxMemories - counts.memories
		if contextLimit > maxMemoryCurationContextMemories {
			contextLimit = maxMemoryCurationContextMemories
		}
		rows, err := tx.Query(ctx, `
			WITH sampled_transcript_entries AS MATERIALIZED (
			  SELECT e.body,
			         row_number() OVER (
			           ORDER BY i.ordinal DESC,e.sequence DESC,e.id DESC
			         ) AS evidence_rank
			  FROM memory_curation_run_inputs i
			  JOIN transcript_entries e
			    ON e.transcript_id=i.transcript_id
			   AND e.sequence BETWEEN i.sequence_from AND i.sequence_until
			   AND e.account_id=i.account_id AND e.realm_id=i.realm_id
			   AND e.recorded_by_agent_id=i.owner_id
			  WHERE i.run_id=$6 AND i.input_kind='transcript'
			  ORDER BY i.ordinal DESC,e.sequence DESC,e.id DESC
			  LIMIT $7
			), topic_terms AS MATERIALIZED (
			  SELECT term.lexeme,min(sample.evidence_rank) AS evidence_rank,
			         count(*) AS evidence_count
			  FROM sampled_transcript_entries sample
			  CROSS JOIN LATERAL unnest(
			    tsvector_to_array(to_tsvector('simple'::regconfig,left(sample.body,$8)))
			  ) AS term(lexeme)
			  GROUP BY term.lexeme
			  ORDER BY min(sample.evidence_rank),count(*) DESC,
			           char_length(term.lexeme) DESC,term.lexeme
			  LIMIT $9
			), topic AS MATERIALIZED (
			  SELECT CASE WHEN count(*)=0 THEN NULL::tsquery
			         ELSE to_tsquery(
			           'simple'::regconfig,
			           string_agg(
			             quote_literal(lexeme),' | '
			             ORDER BY evidence_rank,evidence_count DESC,
			                      char_length(lexeme) DESC,lexeme
			           )
			         ) END AS query
			  FROM topic_terms
			), relevant AS MATERIALIZED (
			  SELECT m.id,v.version,
			         ts_rank_cd(v.search_document,topic.query,32) AS relevance,
			         v.salience,m.updated_at
			  FROM topic
			  JOIN memory_versions v
			    ON topic.query IS NOT NULL AND v.search_document @@ topic.query
			  JOIN memories m
			    ON m.id=v.memory_id AND m.current_version=v.version
			  WHERE m.account_id=$1 AND m.realm_id=$2
			    AND m.owner_kind='agent' AND m.owner_id=$3
			    AND v.state=ANY($4::text[]) AND ($5 OR NOT v.sensitive)
			    AND NOT EXISTS (
			      SELECT 1 FROM memory_curation_run_inputs i
			      WHERE i.run_id=$6 AND i.input_kind='memory' AND i.memory_id=m.id
			    )
			  ORDER BY relevance DESC,v.salience DESC,m.updated_at DESC,m.id DESC
			  LIMIT $10
			), fallback AS MATERIALIZED (
			  SELECT m.id,v.version,0::real AS relevance,v.salience,m.updated_at
			  FROM memories m
			  JOIN memory_versions v
			    ON v.memory_id=m.id AND v.version=m.current_version
			  WHERE m.account_id=$1 AND m.realm_id=$2
			    AND m.owner_kind='agent' AND m.owner_id=$3
			    AND v.state=ANY($4::text[]) AND ($5 OR NOT v.sensitive)
			    AND NOT EXISTS (
			      SELECT 1 FROM memory_curation_run_inputs i
			      WHERE i.run_id=$6 AND i.input_kind='memory' AND i.memory_id=m.id
			    )
			    AND NOT EXISTS (SELECT 1 FROM relevant r WHERE r.id=m.id)
			  ORDER BY v.salience DESC,m.updated_at DESC,m.id DESC
			  LIMIT $10
			), candidates AS (
			  SELECT * FROM relevant
			  UNION ALL
			  SELECT * FROM fallback
			)
			SELECT id,version
			FROM candidates
			ORDER BY relevance DESC,salience DESC,updated_at DESC,id DESC
			LIMIT $10`, p.AccountID, p.RealmID, p.ID, scope.MemoryStates,
			scope.IncludeSensitive, runID, maxMemoryCurationContextEntries,
			maxMemoryCurationContextBodyChars, maxMemoryCurationContextTerms,
			contextLimit)
		if err != nil {
			return counts, 0, 0, fmt.Errorf("select curation memory context: %w", err)
		}
		type contextRef struct {
			id      string
			version int64
		}
		refs := make([]contextRef, 0, contextLimit)
		for rows.Next() {
			var item contextRef
			if err := rows.Scan(&item.id, &item.version); err != nil {
				rows.Close()
				return counts, 0, 0, err
			}
			refs = append(refs, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return counts, 0, 0, err
		}
		rows.Close()
		for index, item := range refs {
			if err := appendInput(MemoryCurationRunInput{
				Kind: MemoryCurationInputMemory, MemoryID: item.id, MemoryVersion: item.version,
			}, fmt.Sprintf("07/memory-context/%06d/%s", index+1, item.id)); err != nil {
				return counts, 0, 0, err
			}
		}
	}
	return counts, memoryUpper, evidenceUpper, nil
}

// memoryCurationTranscriptEntrySize carries one stored entry's sequence and
// its raw body+payload+artifacts byte size, ordered by sequence.
type memoryCurationTranscriptEntrySize struct {
	Sequence int64
	Bytes    int64
}

// memoryCurationTranscriptSlice is one contiguous frozen transcript window.
type memoryCurationTranscriptSlice struct {
	From, Until int64
}

func loadMemoryCurationTranscriptEntrySizes(ctx context.Context, tx pgx.Tx, p Principal, transcriptID string, from, until int64) ([]memoryCurationTranscriptEntrySize, error) {
	rows, err := tx.Query(ctx, `
		SELECT sequence,
		       octet_length(body)+COALESCE(octet_length(payload::text),0)+
		       octet_length(artifacts::text)
		FROM transcript_entries
		WHERE transcript_id=$1 AND account_id=$2 AND realm_id=$3
		  AND recorded_by_agent_id=$4 AND sequence BETWEEN $5 AND $6
		ORDER BY sequence`, transcriptID, p.AccountID, p.RealmID, p.ID, from, until)
	if err != nil {
		return nil, fmt.Errorf("size curation transcript window: %w", err)
	}
	defer rows.Close()
	sizes := make([]memoryCurationTranscriptEntrySize, 0)
	for rows.Next() {
		var item memoryCurationTranscriptEntrySize
		if err := rows.Scan(&item.Sequence, &item.Bytes); err != nil {
			return nil, err
		}
		sizes = append(sizes, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sizes, nil
}

// chunkMemoryCurationTranscriptWindow splits [from,until] into contiguous
// slices whose stored entry bytes each fit one input budget, so no single
// frozen input materializes larger than the transport can return. Coverage is
// exact: slice boundaries only move where a sized entry exists, the first
// slice starts at from, and the last slice ends at until. An entry larger than
// the budget still gets its own slice.
func chunkMemoryCurationTranscriptWindow(sizes []memoryCurationTranscriptEntrySize, from, until, budget int64) []memoryCurationTranscriptSlice {
	slices := make([]memoryCurationTranscriptSlice, 0, 1)
	chunkFrom, chunkBytes, last := from, int64(0), from-1
	for _, item := range sizes {
		if item.Sequence < from || item.Sequence > until {
			continue
		}
		if chunkBytes > 0 && chunkBytes+item.Bytes > budget {
			slices = append(slices, memoryCurationTranscriptSlice{From: chunkFrom, Until: last})
			chunkFrom, chunkBytes = last+1, 0
		}
		chunkBytes += item.Bytes
		last = item.Sequence
	}
	return append(slices, memoryCurationTranscriptSlice{From: chunkFrom, Until: until})
}

// StartCuration claims one due request and materializes immutable input
// membership under a short transaction. No model work is performed here.
func (s *Store) StartCuration(ctx context.Context, p Principal, in StartMemoryCurationInput) (StartMemoryCurationResult, error) {
	if p.Kind != PrincipalAgent {
		return StartMemoryCurationResult{}, ErrMemoryCurationForbidden
	}
	in, err := normalizeStartMemoryCurationInput(in)
	if err != nil {
		return StartMemoryCurationResult{}, err
	}
	hashInput := in
	hashInput.IdempotencyKey = ""
	requestHash, err := memoryRequestHash(struct {
		Operation string                   `json:"operation"`
		Input     StartMemoryCurationInput `json:"input"`
	}{Operation: "start", Input: hashInput})
	if err != nil {
		return StartMemoryCurationResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StartMemoryCurationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prepareMemoryCurationMutationTx(ctx, tx, p, "start", in.IdempotencyKey); err != nil {
		return StartMemoryCurationResult{}, err
	}
	if err := authorizeMemoryCurationRequestID(ctx, tx, p, in.RequestID); err != nil {
		return StartMemoryCurationResult{}, err
	}
	lane, err := loadMemoryCurationLaneTx(ctx, tx, p, true)
	if err != nil {
		return StartMemoryCurationResult{}, err
	}
	if lane.ActiveRunID != "" {
		if err := authorizeMemoryCurationRunID(ctx, tx, p, lane.ActiveRunID); err != nil {
			return StartMemoryCurationResult{}, err
		}
	}
	expired, err := interruptExpiredCurationRunTx(ctx, tx, p, &lane)
	if err != nil {
		return StartMemoryCurationResult{}, err
	}
	if receipt, ok, err := loadMemoryCurationMutation(ctx, tx, p, "start", in.IdempotencyKey, requestHash); err != nil || ok {
		if err != nil {
			return StartMemoryCurationResult{}, err
		}
		run, err := loadMemoryCurationRun(ctx, tx, p, receipt.RunID, false)
		if err != nil {
			return StartMemoryCurationResult{}, err
		}
		request, err := loadMemoryCurationRequest(ctx, tx, p, receipt.RequestID, false)
		if err != nil {
			return StartMemoryCurationResult{}, err
		}
		if err := authorizeMemoryCurationRequestScope(p, request); err != nil {
			return StartMemoryCurationResult{}, err
		}
		cursor, err := encodeMemoryCurationInputCursor(run.ID, run.FencingGeneration, 0)
		if err != nil {
			return StartMemoryCurationResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return StartMemoryCurationResult{}, err
		}
		return StartMemoryCurationResult{Run: run, Request: request, Receipt: receipt, FirstInputCursor: cursor}, nil
	}
	if lane.ActiveRunID != "" {
		return StartMemoryCurationResult{}, ErrMemoryCurationBusy
	}
	request, err := loadMemoryCurationRequest(ctx, tx, p, in.RequestID, true)
	if err != nil {
		if expired {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return StartMemoryCurationResult{}, commitErr
			}
		}
		return StartMemoryCurationResult{}, err
	}
	if err := authorizeMemoryCurationRequestScope(p, request); err != nil {
		return StartMemoryCurationResult{}, err
	}
	if request.State != MemoryCurationRequestQueued && request.State != MemoryCurationRequestRetryWait {
		domainErr := fmt.Errorf("%w: request is %s", ErrMemoryCurationConflict, request.State)
		if expired {
			if err := tx.Commit(ctx); err != nil {
				return StartMemoryCurationResult{}, err
			}
		}
		return StartMemoryCurationResult{}, domainErr
	}
	var due bool
	if err := tx.QueryRow(ctx, `SELECT due_at <= clock_timestamp() FROM memory_curation_requests WHERE id=$1`, request.ID).Scan(&due); err != nil {
		return StartMemoryCurationResult{}, err
	}
	if !due {
		if expired {
			if err := tx.Commit(ctx); err != nil {
				return StartMemoryCurationResult{}, err
			}
		}
		return StartMemoryCurationResult{}, ErrMemoryCurationNotDue
	}
	if request.AttemptCount >= request.MaxAttempts {
		return StartMemoryCurationResult{}, fmt.Errorf("%w: retry ceiling reached", ErrMemoryCurationConflict)
	}
	if !expired {
		if lane.FencingGeneration >= maxMemoryCurationGeneration {
			return StartMemoryCurationResult{}, fmt.Errorf("%w: fencing generation exhausted", ErrMemoryCurationConflict)
		}
		lane.FencingGeneration++
	}
	runID, err := id.New("mrun")
	if err != nil {
		return StartMemoryCurationResult{}, err
	}
	var dbNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		return StartMemoryCurationResult{}, err
	}
	leaseExpiresAt := dbNow.Add(in.LeaseDuration)
	_, err = tx.Exec(ctx, `
		INSERT INTO memory_curation_runs
		  (id,account_id,realm_id,owner_kind,owner_id,request_id,
		   request_generation,fencing_generation,state,actor_kind,actor_id,
		   idempotency_key,request_hash,lease_expires_at,client_runtime,
		   client_model,client_recipe,client_recipe_version,budgets)
		VALUES ($1,$2,$3,'agent',$4,$5,$6,$7,'open','agent',$4,$8,$9,$10,
		        $11,$12,$13,$14,$15::jsonb)`, runID, p.AccountID, p.RealmID, p.ID,
		request.ID, request.RequestGeneration, lane.FencingGeneration,
		in.IdempotencyKey, requestHash, leaseExpiresAt, in.Client.Runtime,
		in.Client.Model, in.Client.Recipe, in.Client.RecipeVersion, in.Budgets)
	if err != nil {
		return StartMemoryCurationResult{}, fmt.Errorf("insert curation run: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_curation_requests
		SET state='claimed',claimed_run_id=$2,claimed_at=clock_timestamp(),
		    updated_at=clock_timestamp()
		WHERE id=$1 AND state IN ('queued','retry_wait')`, request.ID, runID); err != nil {
		return StartMemoryCurationResult{}, fmt.Errorf("claim curation request: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_curation_lanes
		SET active_run_id=$4,fencing_generation=$5,updated_at=clock_timestamp()
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND active_run_id IS NULL`, p.AccountID, p.RealmID, p.ID, runID,
		lane.FencingGeneration); err != nil {
		return StartMemoryCurationResult{}, fmt.Errorf("activate curation run: %w", err)
	}
	var counts memoryCurationInputCounts
	var memoryUpper, evidenceUpper int64
	if request.ReadOnlyReplay {
		counts, memoryUpper, evidenceUpper, err = materializeMemoryCurationReplayInputsTx(
			ctx, tx, p, runID, request.ReplayRunID, request.Scope)
	} else {
		counts, memoryUpper, evidenceUpper, err = materializeMemoryCurationInputsTx(
			ctx, tx, p, runID, request.Scope, effectiveMemoryCurationCaps(request.Scope, in.Caps))
	}
	if err != nil {
		return StartMemoryCurationResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_curation_runs
		SET memory_change_upper=$2,evidence_change_upper=$3,input_count=$4,
		    memory_input_count=$5,evidence_input_count=$6,
		    transcript_input_count=$7,cursor_input_count=$8,
		    updated_at=clock_timestamp()
		WHERE id=$1`, runID, memoryUpper, evidenceUpper, counts.total,
		counts.memories, counts.evidence, counts.transcripts, counts.cursors); err != nil {
		return StartMemoryCurationResult{}, fmt.Errorf("finalize curation run snapshot: %w", err)
	}
	run, err := loadMemoryCurationRun(ctx, tx, p, runID, false)
	if err != nil {
		return StartMemoryCurationResult{}, err
	}
	request, err = loadMemoryCurationRequest(ctx, tx, p, request.ID, false)
	if err != nil {
		return StartMemoryCurationResult{}, err
	}
	receipt, err := insertMemoryCurationMutation(ctx, tx, p, MemoryCurationMutationReceipt{
		Operation: "start", ActorID: p.ID, IdempotencyKey: in.IdempotencyKey,
		RequestHash: requestHash, RequestID: request.ID, RunID: run.ID,
		RequestGeneration: run.RequestGeneration,
		FencingGeneration: run.FencingGeneration, LeaseExpiresAt: run.LeaseExpiresAt,
		ResultState: run.State,
	})
	if err != nil {
		return StartMemoryCurationResult{}, err
	}
	if err := logMemoryCurationEventTx(ctx, tx, p, VerbMemoryCurationStarted,
		request.ID, run.ID, run.RequestGeneration, run.FencingGeneration,
		run.State); err != nil {
		return StartMemoryCurationResult{}, err
	}
	firstCursor, err := encodeMemoryCurationInputCursor(run.ID, run.FencingGeneration, 0)
	if err != nil {
		return StartMemoryCurationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return StartMemoryCurationResult{}, err
	}
	return StartMemoryCurationResult{Run: run, Request: request, Receipt: receipt, FirstInputCursor: firstCursor}, nil
}

// RenewCuration is a fenced heartbeat. An exact retry returns its original
// receipt; a stale or expired worker can never extend a successor's lease.
func (s *Store) RenewCuration(ctx context.Context, p Principal, runID string, in RenewMemoryCurationInput) (RenewMemoryCurationResult, error) {
	if p.Kind != PrincipalAgent {
		return RenewMemoryCurationResult{}, ErrMemoryCurationForbidden
	}
	runID = strings.TrimSpace(runID)
	if !validCurationID(runID, "mrun") {
		return RenewMemoryCurationResult{}, ErrMemoryCurationInputInvalid
	}
	in, err := normalizeRenewMemoryCurationInput(in)
	if err != nil {
		return RenewMemoryCurationResult{}, err
	}
	hashInput := in
	hashInput.IdempotencyKey = ""
	requestHash, err := memoryRequestHash(struct {
		Operation string                   `json:"operation"`
		RunID     string                   `json:"run_id"`
		Input     RenewMemoryCurationInput `json:"input"`
	}{Operation: "renew", RunID: runID, Input: hashInput})
	if err != nil {
		return RenewMemoryCurationResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RenewMemoryCurationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prepareMemoryCurationMutationTx(ctx, tx, p, "renew", in.IdempotencyKey); err != nil {
		return RenewMemoryCurationResult{}, err
	}
	if err := authorizeMemoryCurationRunID(ctx, tx, p, runID); err != nil {
		return RenewMemoryCurationResult{}, err
	}
	lane, err := loadMemoryCurationLaneTx(ctx, tx, p, true)
	if err != nil {
		return RenewMemoryCurationResult{}, err
	}
	commitExpired := func(expiredLease *time.Time) (RenewMemoryCurationResult, error) {
		run, err := loadMemoryCurationRun(ctx, tx, p, runID, false)
		if err != nil {
			return RenewMemoryCurationResult{}, err
		}
		receipt, err := insertMemoryCurationMutation(ctx, tx, p, MemoryCurationMutationReceipt{
			Operation: "renew", ActorID: p.ID, IdempotencyKey: in.IdempotencyKey,
			RequestHash: requestHash, RequestID: run.RequestID, RunID: run.ID,
			RequestGeneration: run.RequestGeneration,
			FencingGeneration: run.FencingGeneration, LeaseExpiresAt: expiredLease,
			ResultState: run.State,
		})
		if err != nil {
			return RenewMemoryCurationResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RenewMemoryCurationResult{}, err
		}
		return RenewMemoryCurationResult{Run: run, Receipt: receipt}, ErrMemoryCurationLeaseExpired
	}
	if receipt, ok, err := loadMemoryCurationMutation(ctx, tx, p, "renew", in.IdempotencyKey, requestHash); err != nil || ok {
		if err != nil {
			return RenewMemoryCurationResult{}, err
		}
		run, err := loadMemoryCurationRun(ctx, tx, p, receipt.RunID, false)
		if err != nil {
			return RenewMemoryCurationResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RenewMemoryCurationResult{}, err
		}
		result := RenewMemoryCurationResult{Run: run, Receipt: receipt}
		if receipt.ResultState == MemoryCurationRunInterrupted && run.TerminalReasonCode == "lease_expired" {
			return result, ErrMemoryCurationLeaseExpired
		}
		return result, nil
	}
	// A fresh key may reconcile only the exact active run it names. Checking
	// the lane before expiry handling prevents a stale worker for run A from
	// interrupting an unrelated expired successor run B and then recording a
	// receipt with A's provenance.
	if lane.ActiveRunID != runID || lane.FencingGeneration != in.FencingGeneration {
		return RenewMemoryCurationResult{}, ErrMemoryCurationFenceMismatch
	}
	run, err := loadMemoryCurationRun(ctx, tx, p, runID, true)
	if err != nil {
		return RenewMemoryCurationResult{}, err
	}
	if run.FencingGeneration != in.FencingGeneration ||
		(run.State != MemoryCurationRunOpen && run.State != MemoryCurationRunPlanned) {
		return RenewMemoryCurationResult{}, ErrMemoryCurationFenceMismatch
	}
	// Preserve the lease coordinate that this renew attempt observed. Expiry
	// reconciliation clears the run's live lease, but renew receipts are
	// constrained to carry a non-null lease and exact replays must return the
	// same value-free coordinate.
	expiredLease := run.LeaseExpiresAt
	expired, err := interruptExpiredCurationRunTx(ctx, tx, p, &lane)
	if err != nil {
		return RenewMemoryCurationResult{}, err
	}
	if expired {
		return commitExpired(expiredLease)
	}
	var dbNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		return RenewMemoryCurationResult{}, err
	}
	newExpiry := dbNow.Add(in.Extension)
	maximumExpiry := dbNow.Add(maxMemoryCurationLease)
	if run.LeaseExpiresAt != nil && run.LeaseExpiresAt.After(newExpiry) {
		newExpiry = *run.LeaseExpiresAt
	}
	if newExpiry.After(maximumExpiry) {
		newExpiry = maximumExpiry
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_curation_runs
		SET lease_expires_at=$2,updated_at=clock_timestamp()
		WHERE id=$1 AND fencing_generation=$3 AND state IN ('open','planned')
		  AND lease_expires_at=$4 AND lease_expires_at > clock_timestamp()`,
		run.ID, newExpiry, in.FencingGeneration, *run.LeaseExpiresAt)
	if err != nil {
		return RenewMemoryCurationResult{}, fmt.Errorf("renew curation lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		expired, interruptErr := interruptExpiredCurationRunTx(ctx, tx, p, &lane)
		if interruptErr != nil {
			return RenewMemoryCurationResult{}, interruptErr
		}
		if !expired {
			return RenewMemoryCurationResult{}, ErrMemoryCurationFenceMismatch
		}
		return commitExpired(expiredLease)
	}
	run, err = loadMemoryCurationRun(ctx, tx, p, runID, false)
	if err != nil {
		return RenewMemoryCurationResult{}, err
	}
	receipt, err := insertMemoryCurationMutation(ctx, tx, p, MemoryCurationMutationReceipt{
		Operation: "renew", ActorID: p.ID, IdempotencyKey: in.IdempotencyKey,
		RequestHash: requestHash, RequestID: run.RequestID, RunID: run.ID,
		RequestGeneration: run.RequestGeneration,
		FencingGeneration: run.FencingGeneration, LeaseExpiresAt: run.LeaseExpiresAt,
		ResultState: run.State,
	})
	if err != nil {
		return RenewMemoryCurationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RenewMemoryCurationResult{}, err
	}
	return RenewMemoryCurationResult{Run: run, Receipt: receipt}, nil
}

// CancelCuration abandons the active run and cancels its queue request.
func (s *Store) CancelCuration(ctx context.Context, p Principal, runID string, in FinishMemoryCurationInput) (FinishMemoryCurationResult, error) {
	return s.finishCuration(ctx, p, runID, in, false)
}

// AbandonCuration terminates the worker run but safely requeues its request
// (or dead-letters it at the retry ceiling).
func (s *Store) AbandonCuration(ctx context.Context, p Principal, runID string, in FinishMemoryCurationInput) (FinishMemoryCurationResult, error) {
	return s.finishCuration(ctx, p, runID, in, true)
}

func (s *Store) finishCuration(ctx context.Context, p Principal, runID string, in FinishMemoryCurationInput, requeue bool) (FinishMemoryCurationResult, error) {
	if p.Kind != PrincipalAgent {
		return FinishMemoryCurationResult{}, ErrMemoryCurationForbidden
	}
	runID = strings.TrimSpace(runID)
	if !validCurationID(runID, "mrun") {
		return FinishMemoryCurationResult{}, ErrMemoryCurationInputInvalid
	}
	in, err := normalizeFinishMemoryCurationInput(in)
	if err != nil {
		return FinishMemoryCurationResult{}, err
	}
	operation := "cancel"
	defaultReason := "cancelled"
	verb := VerbMemoryCurationCancelled
	if requeue {
		operation = "abandon"
		defaultReason = "worker_abandoned"
		verb = VerbMemoryCurationInterrupted
	}
	if in.Reason == "" {
		in.Reason = defaultReason
	}
	hashInput := in
	hashInput.IdempotencyKey = ""
	requestHash, err := memoryRequestHash(struct {
		Operation string                    `json:"operation"`
		RunID     string                    `json:"run_id"`
		Input     FinishMemoryCurationInput `json:"input"`
	}{Operation: operation, RunID: runID, Input: hashInput})
	if err != nil {
		return FinishMemoryCurationResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FinishMemoryCurationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prepareMemoryCurationMutationTx(ctx, tx, p, operation, in.IdempotencyKey); err != nil {
		return FinishMemoryCurationResult{}, err
	}
	if err := authorizeMemoryCurationRunID(ctx, tx, p, runID); err != nil {
		return FinishMemoryCurationResult{}, err
	}
	lane, err := loadMemoryCurationLaneTx(ctx, tx, p, true)
	if err != nil {
		return FinishMemoryCurationResult{}, err
	}
	expired, err := interruptExpiredCurationRunTx(ctx, tx, p, &lane)
	if err != nil {
		return FinishMemoryCurationResult{}, err
	}
	if receipt, ok, err := loadMemoryCurationMutation(ctx, tx, p, operation, in.IdempotencyKey, requestHash); err != nil || ok {
		if err != nil {
			return FinishMemoryCurationResult{}, err
		}
		run, err := loadMemoryCurationRun(ctx, tx, p, receipt.RunID, false)
		if err != nil {
			return FinishMemoryCurationResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return FinishMemoryCurationResult{}, err
		}
		return FinishMemoryCurationResult{Run: run, Receipt: receipt}, nil
	}
	if expired {
		if err := tx.Commit(ctx); err != nil {
			return FinishMemoryCurationResult{}, err
		}
		return FinishMemoryCurationResult{}, ErrMemoryCurationLeaseExpired
	}
	if lane.ActiveRunID != runID || lane.FencingGeneration != in.FencingGeneration {
		return FinishMemoryCurationResult{}, ErrMemoryCurationFenceMismatch
	}
	if lane.FencingGeneration >= maxMemoryCurationGeneration {
		return FinishMemoryCurationResult{}, fmt.Errorf("%w: fencing generation exhausted", ErrMemoryCurationConflict)
	}
	run, err := loadMemoryCurationRun(ctx, tx, p, runID, true)
	if err != nil {
		return FinishMemoryCurationResult{}, err
	}
	if run.FencingGeneration != in.FencingGeneration ||
		(run.State != MemoryCurationRunOpen && run.State != MemoryCurationRunPlanned) {
		return FinishMemoryCurationResult{}, ErrMemoryCurationFenceMismatch
	}
	request, err := loadMemoryCurationRequest(ctx, tx, p, run.RequestID, true)
	if err != nil {
		return FinishMemoryCurationResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_curation_runs
		SET state='abandoned',lease_expires_at=NULL,terminal_reason_code=$2,
		    terminal_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE id=$1 AND state IN ('open','planned')`, run.ID, in.Reason); err != nil {
		return FinishMemoryCurationResult{}, fmt.Errorf("abandon curation run: %w", err)
	}
	requestResultState := MemoryCurationRequestCancelled
	if requeue {
		// An accepted preview is a successful, non-mutating inspection rather
		// than a worker failure. Keep it retryable after a bounded cooldown (or
		// immediately when a new source generation marks it due), but do not
		// consume the failure budget or eventually dead-letter healthy work.
		previewComplete := in.Reason == "preview_complete" && run.State == MemoryCurationRunPlanned
		nextAttempt := request.AttemptCount
		if !previewComplete {
			nextAttempt++
		}
		requestResultState = MemoryCurationRequestRetryWait
		var dueAt any
		if previewComplete {
			var now time.Time
			if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
				return FinishMemoryCurationResult{}, err
			}
			dueAt = now.Add(memoryCurationPreviewCooldown)
		} else if nextAttempt >= request.MaxAttempts {
			requestResultState = MemoryCurationRequestDeadLetter
			dueAt = request.DueAt
		} else {
			var now time.Time
			if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
				return FinishMemoryCurationResult{}, err
			}
			dueAt = now.Add(curationBackoff(nextAttempt))
		}
		if _, err := tx.Exec(ctx, `
			UPDATE memory_curation_requests
			SET state=$2,attempt_count=$3,claimed_run_id=NULL,claimed_at=NULL,
			    due_at=$4,dead_lettered_at=CASE WHEN $2='dead_letter'
			                                    THEN clock_timestamp() ELSE NULL END,
			    updated_at=clock_timestamp()
			WHERE id=$1 AND state='claimed' AND claimed_run_id=$5`, request.ID,
			requestResultState, nextAttempt, dueAt, run.ID); err != nil {
			return FinishMemoryCurationResult{}, fmt.Errorf("requeue abandoned curation request: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE memory_curation_requests
			SET state='cancelled',cancelled_at=clock_timestamp(),updated_at=clock_timestamp()
			WHERE id=$1 AND state='claimed' AND claimed_run_id=$2`, request.ID, run.ID); err != nil {
			return FinishMemoryCurationResult{}, fmt.Errorf("cancel curation request: %w", err)
		}
	}
	lane.FencingGeneration++
	if _, err := tx.Exec(ctx, `
		UPDATE memory_curation_lanes
		SET active_run_id=NULL,fencing_generation=$5,updated_at=clock_timestamp()
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND active_run_id=$4`, p.AccountID, p.RealmID, p.ID, run.ID,
		lane.FencingGeneration); err != nil {
		return FinishMemoryCurationResult{}, fmt.Errorf("release curation lane: %w", err)
	}
	run, err = loadMemoryCurationRun(ctx, tx, p, runID, false)
	if err != nil {
		return FinishMemoryCurationResult{}, err
	}
	receipt, err := insertMemoryCurationMutation(ctx, tx, p, MemoryCurationMutationReceipt{
		Operation: operation, ActorID: p.ID, IdempotencyKey: in.IdempotencyKey,
		RequestHash: requestHash, RequestID: run.RequestID, RunID: run.ID,
		RequestGeneration: run.RequestGeneration,
		FencingGeneration: run.FencingGeneration, ResultState: run.State,
	})
	if err != nil {
		return FinishMemoryCurationResult{}, err
	}
	if err := logMemoryCurationEventTx(ctx, tx, p, verb, request.ID, run.ID,
		run.RequestGeneration, run.FencingGeneration, run.State); err != nil {
		return FinishMemoryCurationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FinishMemoryCurationResult{}, err
	}
	_ = requestResultState
	return FinishMemoryCurationResult{Run: run, Receipt: receipt}, nil
}

// GetCurationRunInputs reads only the membership frozen by StartCuration. A
// matching live fence is mandatory; status reads use the separate methods
// below and intentionally do not require a lease.
func (s *Store) GetCurationRunInputs(ctx context.Context, p Principal, runID string, fencingGeneration int64, cursor string, limit int) (MemoryCurationRunInputPage, error) {
	if p.Kind != PrincipalAgent {
		return MemoryCurationRunInputPage{}, ErrMemoryCurationForbidden
	}
	runID = strings.TrimSpace(runID)
	if !validCurationID(runID, "mrun") || fencingGeneration < 1 {
		return MemoryCurationRunInputPage{}, ErrMemoryCurationInputInvalid
	}
	after, err := decodeMemoryCurationInputCursor(cursor, runID, fencingGeneration)
	if err != nil {
		return MemoryCurationRunInputPage{}, err
	}
	if limit == 0 {
		limit = defaultMemoryCurationPageSize
	}
	if limit < 1 || limit > maxMemoryCurationPageSize {
		return MemoryCurationRunInputPage{}, ErrMemoryCurationInputInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MemoryCurationRunInputPage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return MemoryCurationRunInputPage{}, err
	}
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MemoryCurationRunInputPage{}, err
	}
	if err := authorizeMemoryCurationRunID(ctx, tx, p, runID); err != nil {
		return MemoryCurationRunInputPage{}, err
	}
	lane, err := loadMemoryCurationLaneTx(ctx, tx, p, false)
	if err != nil {
		return MemoryCurationRunInputPage{}, err
	}
	if lane.ActiveRunID != runID || lane.FencingGeneration != fencingGeneration {
		return MemoryCurationRunInputPage{}, ErrMemoryCurationFenceMismatch
	}
	run, err := loadMemoryCurationRun(ctx, tx, p, runID, false)
	if err != nil {
		return MemoryCurationRunInputPage{}, err
	}
	if run.FencingGeneration != fencingGeneration ||
		(run.State != MemoryCurationRunOpen && run.State != MemoryCurationRunPlanned) {
		return MemoryCurationRunInputPage{}, ErrMemoryCurationFenceMismatch
	}
	var expired bool
	if err := tx.QueryRow(ctx, `
		SELECT lease_expires_at IS NULL OR lease_expires_at <= clock_timestamp()
		FROM memory_curation_runs WHERE id=$1`, run.ID).Scan(&expired); err != nil {
		return MemoryCurationRunInputPage{}, fmt.Errorf("check curation lease: %w", err)
	}
	if expired {
		return MemoryCurationRunInputPage{}, ErrMemoryCurationLeaseExpired
	}
	rows, err := tx.Query(ctx, `
		SELECT run_id,ordinal,input_kind,COALESCE(memory_id,''),
		       COALESCE(memory_version,0),COALESCE(evidence_id,''),
		       COALESCE(transcript_id,''),COALESCE(sequence_from,0),
		       COALESCE(sequence_until,0),COALESCE(cursor_source_kind,''),
		       COALESCE(cursor_stream_id,''),COALESCE(cursor_expected_prior,0),
		       COALESCE(cursor_upper,0),created_at
		FROM memory_curation_run_inputs
		WHERE run_id=$1 AND account_id=$2 AND realm_id=$3 AND owner_kind='agent'
		  AND owner_id=$4 AND ordinal>$5
		ORDER BY ordinal LIMIT $6`, runID, p.AccountID, p.RealmID, p.ID, after,
		limit+1)
	if err != nil {
		return MemoryCurationRunInputPage{}, fmt.Errorf("list curation run inputs: %w", err)
	}
	inputs := make([]MemoryCurationRunInput, 0, limit+1)
	for rows.Next() {
		var input MemoryCurationRunInput
		if err := rows.Scan(&input.RunID, &input.Ordinal, &input.Kind,
			&input.MemoryID, &input.MemoryVersion, &input.EvidenceID,
			&input.TranscriptID, &input.SequenceFrom, &input.SequenceUntil,
			&input.CursorSourceKind, &input.CursorStreamID,
			&input.CursorExpectedPrior, &input.CursorUpper, &input.CreatedAt); err != nil {
			rows.Close()
			return MemoryCurationRunInputPage{}, err
		}
		inputs = append(inputs, input)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return MemoryCurationRunInputPage{}, err
	}
	rows.Close()
	hasMore := len(inputs) > limit
	if hasMore {
		inputs = inputs[:limit]
	}
	// A page stops early once its hydrated inputs exceed the page byte budget
	// so one response never outgrows the transport, regardless of the
	// requested limit. The cursor resumes from the last returned ordinal.
	pageBytes := 0
	for i := range inputs {
		if err := hydrateMemoryCurationRunInput(ctx, tx, p, &inputs[i]); err != nil {
			return MemoryCurationRunInputPage{}, err
		}
		encoded, err := json.Marshal(inputs[i])
		if err != nil {
			return MemoryCurationRunInputPage{}, err
		}
		pageBytes += len(encoded)
		if pageBytes >= maxMemoryCurationPageBytes && i+1 < len(inputs) {
			inputs = inputs[:i+1]
			hasMore = true
			break
		}
	}
	nextCursor := ""
	if hasMore {
		nextCursor, err = encodeMemoryCurationInputCursor(runID, fencingGeneration,
			inputs[len(inputs)-1].Ordinal)
		if err != nil {
			return MemoryCurationRunInputPage{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return MemoryCurationRunInputPage{}, err
	}
	return MemoryCurationRunInputPage{Run: run, Inputs: inputs, NextCursor: nextCursor}, nil
}

// GetMemoryCurationRunInputs is a descriptive alias for in-process callers.
func (s *Store) GetMemoryCurationRunInputs(ctx context.Context, p Principal, runID string, fencingGeneration int64, cursor string, limit int) (MemoryCurationRunInputPage, error) {
	return s.GetCurationRunInputs(ctx, p, runID, fencingGeneration, cursor, limit)
}

func hydrateMemoryCurationRunInput(ctx context.Context, q memoryQuerier, p Principal, input *MemoryCurationRunInput) error {
	switch input.Kind {
	case MemoryCurationInputMemory:
		memory, err := loadMemoryAtVersion(ctx, q, p, input.MemoryID, input.MemoryVersion, false)
		if err != nil {
			return err
		}
		// These fields are a mutable relation-derived projection rather than
		// part of the immutable version referenced by this run input. Plans may
		// query current heads separately; frozen input pages never smuggle in a
		// later relation change.
		memory.ActiveSupersessionSetID = ""
		memory.ActiveSupersessionSetRevision = 0
		input.Memory = &memory
	case MemoryCurationInputEvidence:
		evidence, err := scanMemoryEvidence(q.QueryRow(ctx, `
			SELECT id,memory_id,target_version,evidence_change_seq,evidence_type,role,
			       resolution_state,COALESCE(external_locator,''),
			       COALESCE(pending_evidence_id,''),COALESCE(resolved_kind,''),
			       COALESCE(source_transcript_id,''),COALESCE(source_sequence_from,0),
			       COALESCE(source_sequence_until,0),COALESCE(source_memory_id,''),
			       COALESCE(source_memory_version,0),COALESCE(source_message_id,''),
			       COALESCE(source_import_locator,''),artifact_excerpt,
			       artifact_sensitive,COALESCE(terminal_reason_code,''),
			       COALESCE(source_digest,''),actor_id,COALESCE(idempotency_key,''),
			       COALESCE(request_hash,''),created_at
			FROM memory_evidence
			WHERE id=$1 AND account_id=$2 AND realm_id=$3 AND owner_kind='agent'
			  AND owner_id=$4`, input.EvidenceID, p.AccountID, p.RealmID, p.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMemoryCurationConflict
		}
		if err != nil {
			return err
		}
		input.Evidence = &evidence
	case MemoryCurationInputTranscript:
		rows, err := q.Query(ctx, `
			SELECT id,account_id,transcript_id,realm_id,recorded_by_agent_id,
			       sequence,COALESCE(external_id,''),role,body,payload,
			       COALESCE(model,''),COALESCE(reply_to_entry_id,''),artifacts,created_at
			FROM transcript_entries
			WHERE transcript_id=$1 AND account_id=$2 AND realm_id=$3
			  AND recorded_by_agent_id=$4 AND sequence BETWEEN $5 AND $6
			ORDER BY sequence,id`, input.TranscriptID, p.AccountID, p.RealmID, p.ID,
			input.SequenceFrom, input.SequenceUntil)
		if err != nil {
			return fmt.Errorf("load curation transcript input: %w", err)
		}
		for rows.Next() {
			var entry TranscriptEntry
			if err := rows.Scan(&entry.ID, &entry.AccountID, &entry.TranscriptID,
				&entry.RealmID, &entry.RecordedByAgentID, &entry.Sequence,
				&entry.ExternalID, &entry.Role, &entry.Body, &entry.Payload,
				&entry.Model, &entry.ReplyToEntryID, &entry.Artifacts,
				&entry.CreatedAt); err != nil {
				rows.Close()
				return err
			}
			input.TranscriptEntries = append(input.TranscriptEntries, entry)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if int64(len(input.TranscriptEntries)) != input.SequenceUntil-input.SequenceFrom+1 {
			return ErrMemoryCurationConflict
		}
		boundMemoryCurationTranscriptEntries(input.TranscriptEntries)
	case MemoryCurationInputCursor:
		return nil
	default:
		return ErrMemoryCurationConflict
	}
	return nil
}

// boundMemoryCurationTranscriptEntries elides oversized entry content from the
// materialized view of one frozen transcript input. Membership stays exact:
// every frozen sequence remains present with its role and metadata, and an
// in-band note documents each elision so the curator can read the full entry
// through the transcript tools. Windows frozen before byte-budget chunking
// existed hydrate within the same bound as newly frozen ones.
func boundMemoryCurationTranscriptEntries(entries []TranscriptEntry) {
	remaining := maxMemoryCurationInputBytes
	for i := range entries {
		remaining -= boundMemoryCurationTranscriptEntry(&entries[i], remaining)
	}
}

// boundMemoryCurationTranscriptEntry rewrites oversized fields in place and
// returns the bytes the bounded entry still carries. Once the shared input
// budget is spent, later entries keep only elision notes.
func boundMemoryCurationTranscriptEntry(entry *TranscriptEntry, remaining int) int {
	if remaining < 0 {
		remaining = 0
	}
	bodyCap := maxMemoryCurationEntryBodyBytes
	if bodyCap > remaining {
		bodyCap = remaining
	}
	if len(entry.Body) > bodyCap {
		prefix := truncateMemoryCurationUTF8(entry.Body, bodyCap)
		entry.Body = prefix + memoryCurationElisionNote(len(entry.Body)-len(prefix))
	}
	retained := len(entry.Body)
	if len(entry.Payload) > 0 && len(entry.Payload) > remaining-retained {
		entry.Payload = json.RawMessage(fmt.Sprintf(
			`{"witself_elided":true,"omitted_bytes":%d}`, len(entry.Payload)))
	}
	retained += len(entry.Payload)
	if isElidableCurationJSONArray(entry.Artifacts) &&
		len(entry.Artifacts) > remaining-retained {
		entry.Artifacts = json.RawMessage(fmt.Sprintf(
			`[{"witself_elided":true,"omitted_bytes":%d}]`, len(entry.Artifacts)))
	}
	retained += len(entry.Artifacts)
	return retained
}

func memoryCurationElisionNote(omitted int) string {
	return fmt.Sprintf(
		"\n[witself:elided omitted_bytes=%d; read the full entry via the transcript tools]",
		omitted)
}

// isElidableCurationJSONArray reports whether replacing artifacts with an
// elision stub would actually shrink it; the default empty array never
// qualifies.
func isElidableCurationJSONArray(raw json.RawMessage) bool {
	return len(raw) > 64
}

func truncateMemoryCurationUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// GetCurationRun returns value-free status metadata without requiring an
// active lease. It never includes input or plan payloads.
func (s *Store) GetCurationRun(ctx context.Context, p Principal, runID string) (MemoryCurationRun, error) {
	if p.Kind != PrincipalAgent {
		return MemoryCurationRun{}, ErrMemoryCurationForbidden
	}
	runID = strings.TrimSpace(runID)
	if !validCurationID(runID, "mrun") {
		return MemoryCurationRun{}, ErrMemoryCurationInputInvalid
	}
	run, err := loadMemoryCurationRun(ctx, s.pool, p, runID, false)
	if err != nil {
		return MemoryCurationRun{}, err
	}
	if err := authorizeMemoryCurationRequestID(ctx, s.pool, p, run.RequestID); err != nil {
		return MemoryCurationRun{}, err
	}
	return run, nil
}

// GetCurationRequest returns value-free queue metadata after scope authorization.
func (s *Store) GetCurationRequest(ctx context.Context, p Principal, requestID string) (MemoryCurationRequest, error) {
	if p.Kind != PrincipalAgent {
		return MemoryCurationRequest{}, ErrMemoryCurationForbidden
	}
	requestID = strings.TrimSpace(requestID)
	if !validCurationID(requestID, "mcrq") {
		return MemoryCurationRequest{}, ErrMemoryCurationInputInvalid
	}
	request, err := loadMemoryCurationRequest(ctx, s.pool, p, requestID, false)
	if err != nil {
		return MemoryCurationRequest{}, err
	}
	if err := authorizeMemoryCurationRequestScope(p, request); err != nil {
		return MemoryCurationRequest{}, err
	}
	return request, nil
}

// GetCurationStatus returns the owner lane plus one requested/active/latest
// run and request. Expired leases are reported as stored; a mutating lease path
// performs the durable interrupt transition.
func (s *Store) GetCurationStatus(ctx context.Context, p Principal, runID string) (MemoryCurationStatus, error) {
	if p.Kind != PrincipalAgent {
		return MemoryCurationStatus{}, ErrMemoryCurationForbidden
	}
	if runID != "" && !validCurationID(strings.TrimSpace(runID), "mrun") {
		return MemoryCurationStatus{}, ErrMemoryCurationInputInvalid
	}
	lane, err := scanMemoryCurationLane(s.pool.QueryRow(ctx,
		memoryCurationLaneSelectSQL+` WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3`,
		p.AccountID, p.RealmID, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryCurationStatus{}, ErrMemoryCurationNotFound
	}
	if err != nil {
		return MemoryCurationStatus{}, err
	}
	status := MemoryCurationStatus{Lane: lane}
	selectedRunID := strings.TrimSpace(runID)
	if selectedRunID == "" {
		selectedRunID = lane.ActiveRunID
	}
	if selectedRunID != "" {
		run, err := loadMemoryCurationRun(ctx, s.pool, p, selectedRunID, false)
		if err != nil {
			return MemoryCurationStatus{}, err
		}
		if err := authorizeMemoryCurationRequestID(ctx, s.pool, p, run.RequestID); err != nil {
			return MemoryCurationStatus{}, err
		}
		status.Run = &run
		request, err := loadMemoryCurationRequest(ctx, s.pool, p, run.RequestID, false)
		if err != nil {
			return MemoryCurationStatus{}, err
		}
		status.Request = &request
		return status, nil
	}
	request, err := scanMemoryCurationRequest(s.pool.QueryRow(ctx,
		memoryCurationRequestSelectSQL+`
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		ORDER BY updated_at DESC,id DESC LIMIT 1`, p.AccountID, p.RealmID, p.ID))
	if err == nil {
		if err := authorizeMemoryCurationRequestScope(p, request); err != nil {
			return MemoryCurationStatus{}, err
		}
		status.Request = &request
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return MemoryCurationStatus{}, err
	}
	return status, nil
}

func normalizeMemoryCurationScope(in MemoryCurationScope) (MemoryCurationScope, error) {
	if len(in.Sources) == 0 {
		in.Sources = []string{MemoryCurationSourceMemory, MemoryCurationSourceEvidence, MemoryCurationSourceTranscript}
	}
	if len(in.MemoryStates) == 0 {
		in.MemoryStates = []string{MemoryStateActive}
	}
	var err error
	in.Sources, err = normalizeCurationCodes(in.Sources, map[string]bool{
		MemoryCurationSourceMemory: true, MemoryCurationSourceEvidence: true, MemoryCurationSourceTranscript: true,
	})
	if err != nil {
		return MemoryCurationScope{}, err
	}
	in.MemoryStates, err = normalizeCurationCodes(in.MemoryStates, map[string]bool{
		MemoryStateActive: true, MemoryStateSuperseded: true, MemoryStateForgotten: true,
	})
	if err != nil {
		return MemoryCurationScope{}, err
	}
	in.MaxMemories, err = normalizeCurationLimit(in.MaxMemories, defaultMemoryCurationMemories, maxMemoryCurationMemories, "max_memories")
	if err != nil {
		return MemoryCurationScope{}, err
	}
	in.MaxEvidence, err = normalizeCurationLimit(in.MaxEvidence, defaultMemoryCurationEvidence, maxMemoryCurationEvidence, "max_evidence")
	if err != nil {
		return MemoryCurationScope{}, err
	}
	in.MaxTranscriptEntries, err = normalizeCurationLimit(in.MaxTranscriptEntries, defaultMemoryCurationTranscriptItems, maxMemoryCurationTranscriptItems, "max_transcript_entries")
	if err != nil {
		return MemoryCurationScope{}, err
	}
	return in, nil
}

func normalizeRequestMemoryCurationInput(in RequestMemoryCurationInput) (RequestMemoryCurationInput, error) {
	var err error
	in.Scope, err = normalizeMemoryCurationScope(in.Scope)
	if err != nil {
		return RequestMemoryCurationInput{}, err
	}
	in.CoalescingKey = strings.TrimSpace(in.CoalescingKey)
	if in.CoalescingKey == "" {
		in.CoalescingKey = "owner"
	}
	if in.CoalescingKey == automaticMemoryCurationCoalescingKey {
		return RequestMemoryCurationInput{}, fmt.Errorf("%w: coalescing_key is reserved", ErrMemoryCurationInputInvalid)
	}
	in.TriggerReason = strings.TrimSpace(in.TriggerReason)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if !memoryCodePattern.MatchString(in.CoalescingKey) || len(in.CoalescingKey) > 128 {
		return RequestMemoryCurationInput{}, fmt.Errorf("%w: invalid coalescing_key", ErrMemoryCurationInputInvalid)
	}
	if !memoryCodePattern.MatchString(in.TriggerReason) {
		return RequestMemoryCurationInput{}, fmt.Errorf("%w: invalid trigger_reason", ErrMemoryCurationInputInvalid)
	}
	if in.TriggerGeneration < 0 || in.TriggerGeneration > maxMemoryCurationGeneration || in.Priority < 0 || in.Priority > 100 {
		return RequestMemoryCurationInput{}, fmt.Errorf("%w: invalid generation or priority", ErrMemoryCurationInputInvalid)
	}
	if in.MaxAttempts == 0 {
		in.MaxAttempts = defaultMemoryCurationAttempts
	}
	if in.MaxAttempts < 1 || in.MaxAttempts > maxMemoryCurationAttempts {
		return RequestMemoryCurationInput{}, fmt.Errorf("%w: max_attempts must be between 1 and %d", ErrMemoryCurationInputInvalid, maxMemoryCurationAttempts)
	}
	if in.DueAt != nil {
		v := in.DueAt.UTC()
		in.DueAt = &v
	}
	if err := validateMemoryIdempotencyKey(in.IdempotencyKey); err != nil {
		return RequestMemoryCurationInput{}, fmt.Errorf("%w: invalid idempotency key", ErrMemoryCurationInputInvalid)
	}
	return in, nil
}

func normalizeStartMemoryCurationInput(in StartMemoryCurationInput) (StartMemoryCurationInput, error) {
	in.RequestID = strings.TrimSpace(in.RequestID)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if !validCurationID(in.RequestID, "mcrq") {
		return StartMemoryCurationInput{}, fmt.Errorf("%w: invalid request id", ErrMemoryCurationInputInvalid)
	}
	if err := validateMemoryIdempotencyKey(in.IdempotencyKey); err != nil {
		return StartMemoryCurationInput{}, fmt.Errorf("%w: invalid idempotency key", ErrMemoryCurationInputInvalid)
	}
	if in.LeaseDuration == 0 {
		in.LeaseDuration = defaultMemoryCurationLease
	}
	if in.LeaseDuration < minMemoryCurationLease || in.LeaseDuration > maxMemoryCurationLease {
		return StartMemoryCurationInput{}, fmt.Errorf("%w: lease must be between %s and %s", ErrMemoryCurationInputInvalid, minMemoryCurationLease, maxMemoryCurationLease)
	}
	if err := normalizeMemoryClient(&in.Client); err != nil {
		return StartMemoryCurationInput{}, fmt.Errorf("%w: invalid client provenance", ErrMemoryCurationInputInvalid)
	}
	if len(in.Budgets) == 0 {
		in.Budgets = json.RawMessage(`{}`)
	}
	var budgetObject map[string]any
	if len(in.Budgets) > 16*1024 || validateUniqueJSONObject(in.Budgets) != nil || json.Unmarshal(in.Budgets, &budgetObject) != nil || budgetObject == nil {
		return StartMemoryCurationInput{}, fmt.Errorf("%w: budgets must be a bounded JSON object", ErrMemoryCurationInputInvalid)
	}
	canonicalBudgets, err := json.Marshal(budgetObject)
	if err != nil {
		return StartMemoryCurationInput{}, fmt.Errorf("%w: invalid budgets", ErrMemoryCurationInputInvalid)
	}
	in.Budgets = canonicalBudgets
	if in.Caps.MaxMemories < 0 || in.Caps.MaxEvidence < 0 || in.Caps.MaxTranscriptEntries < 0 {
		return StartMemoryCurationInput{}, fmt.Errorf("%w: caps cannot be negative", ErrMemoryCurationInputInvalid)
	}
	if in.Caps.MaxMemories > maxMemoryCurationMemories || in.Caps.MaxEvidence > maxMemoryCurationEvidence || in.Caps.MaxTranscriptEntries > maxMemoryCurationTranscriptItems {
		return StartMemoryCurationInput{}, fmt.Errorf("%w: input cap exceeds server maximum", ErrMemoryCurationInputInvalid)
	}
	return in, nil
}

func normalizeRenewMemoryCurationInput(in RenewMemoryCurationInput) (RenewMemoryCurationInput, error) {
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.FencingGeneration < 1 || in.Extension < minMemoryCurationLease || in.Extension > maxMemoryCurationLease {
		return RenewMemoryCurationInput{}, fmt.Errorf("%w: invalid fence or extension", ErrMemoryCurationInputInvalid)
	}
	if err := validateMemoryIdempotencyKey(in.IdempotencyKey); err != nil {
		return RenewMemoryCurationInput{}, fmt.Errorf("%w: invalid idempotency key", ErrMemoryCurationInputInvalid)
	}
	return in, nil
}

func normalizeFinishMemoryCurationInput(in FinishMemoryCurationInput) (FinishMemoryCurationInput, error) {
	in.Reason = strings.TrimSpace(in.Reason)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.FencingGeneration < 1 || len(in.Reason) > 128 || (in.Reason != "" && !memoryCodePattern.MatchString(in.Reason)) {
		return FinishMemoryCurationInput{}, fmt.Errorf("%w: invalid fence or reason", ErrMemoryCurationInputInvalid)
	}
	if err := validateMemoryIdempotencyKey(in.IdempotencyKey); err != nil {
		return FinishMemoryCurationInput{}, fmt.Errorf("%w: invalid idempotency key", ErrMemoryCurationInputInvalid)
	}
	return in, nil
}

func normalizeCurationCodes(values []string, allowed map[string]bool) ([]string, error) {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !allowed[value] {
			return nil, fmt.Errorf("%w: unsupported scope code %q", ErrMemoryCurationInputInvalid, value)
		}
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out, nil
}

func normalizeCurationLimit(value, defaultValue, maximum int, name string) (int, error) {
	if value == 0 {
		return defaultValue, nil
	}
	if value < 1 || value > maximum {
		return 0, fmt.Errorf("%w: %s must be between 1 and %d", ErrMemoryCurationInputInvalid, name, maximum)
	}
	return value, nil
}

func effectiveMemoryCurationCaps(scope MemoryCurationScope, caps MemoryCurationInputCaps) MemoryCurationInputCaps {
	out := MemoryCurationInputCaps{MaxMemories: scope.MaxMemories, MaxEvidence: scope.MaxEvidence, MaxTranscriptEntries: scope.MaxTranscriptEntries}
	if caps.MaxMemories > 0 && caps.MaxMemories < out.MaxMemories {
		out.MaxMemories = caps.MaxMemories
	}
	if caps.MaxEvidence > 0 && caps.MaxEvidence < out.MaxEvidence {
		out.MaxEvidence = caps.MaxEvidence
	}
	if caps.MaxTranscriptEntries > 0 && caps.MaxTranscriptEntries < out.MaxTranscriptEntries {
		out.MaxTranscriptEntries = caps.MaxTranscriptEntries
	}
	return out
}

func memoryCurationFilteredStreamID(source string, scope MemoryCurationScope) (string, error) {
	digest, err := memoryRequestHash(struct {
		Source           string   `json:"source"`
		MemoryStates     []string `json:"memory_states"`
		IncludeSensitive bool     `json:"include_sensitive"`
	}{Source: source, MemoryStates: scope.MemoryStates, IncludeSensitive: scope.IncludeSensitive})
	if err != nil {
		return "", err
	}
	return "filtered_v1_" + digest, nil
}

func encodeMemoryCurationInputCursor(runID string, fence, after int64) (string, error) {
	raw, err := json.Marshal(memoryCurationInputCursor{Version: 1, RunID: runID, Fence: fence, After: after})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeMemoryCurationInputCursor(value, runID string, fence int64) (int64, error) {
	if value == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, ErrMemoryCurationInputInvalid
	}
	var cursor memoryCurationInputCursor
	if json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.RunID != runID || cursor.Fence != fence || cursor.After < 0 {
		return 0, ErrMemoryCurationInputInvalid
	}
	return cursor.After, nil
}

func validCurationID(value, prefix string) bool {
	body := strings.TrimPrefix(value, prefix+"_")
	if len(body) != 16 || len(value) != len(prefix)+17 {
		return false
	}
	for _, r := range body {
		if (r < 'a' || r > 'z') && (r < '2' || r > '7') {
			return false
		}
	}
	return true
}

func curationHasSource(scope MemoryCurationScope, source string) bool {
	index := sort.SearchStrings(scope.Sources, source)
	return index < len(scope.Sources) && scope.Sources[index] == source
}

func curationBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 10 {
		shift = 10
	}
	d := time.Minute * time.Duration(1<<shift)
	if d > maxMemoryCurationBackoff {
		return maxMemoryCurationBackoff
	}
	return d
}

// validateUniqueJSONObject rejects duplicate keys before ordinary json.Unmarshal
// canonicalizes the budgets object. Duplicate-key acceptance would let two byte
// sequences with ambiguous semantics share a normalized request hash.
func validateUniqueJSONObject(raw []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] {
					return errors.New("duplicate or invalid JSON object key")
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("invalid JSON delimiter")
		}
	}
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("budgets must be an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok || seen[key] {
			return errors.New("duplicate or invalid JSON object key")
		}
		seen[key] = true
		if err := walk(); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing JSON value")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

// Keep database/sql imported here because the scanners below use NullTime and
// NullString. The full store implementation follows these normalization types.
var _ = sql.NullTime{}
var _ = strconv.FormatInt
var _ = context.Background
var _ pgx.Tx
var _ = id.New
