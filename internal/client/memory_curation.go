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

// MemoryCurationScope is deterministic source-selection metadata. It is not a
// model prompt; all synthesis remains in the calling client.
type MemoryCurationScope struct {
	Sources              []string `json:"sources"`
	MemoryStates         []string `json:"memory_states"`
	IncludeSensitive     bool     `json:"include_sensitive"`
	MaxMemories          int      `json:"max_memories"`
	MaxEvidence          int      `json:"max_evidence"`
	MaxTranscriptEntries int      `json:"max_transcript_entries"`
}

// RequestMemoryCurationInput schedules or coalesces one client-side curation
// request.
type RequestMemoryCurationInput struct {
	Scope             MemoryCurationScope `json:"scope"`
	CoalescingKey     string              `json:"coalescing_key"`
	TriggerReason     string              `json:"trigger_reason"`
	TriggerGeneration int64               `json:"trigger_generation,omitempty"`
	Priority          int                 `json:"priority,omitempty"`
	DueAt             *time.Time          `json:"due_at,omitempty"`
	MaxAttempts       int                 `json:"max_attempts,omitempty"`
	IdempotencyKey    string              `json:"-"`
}

// MemoryCurationRequestListOptions selects one bounded request queue page.
type MemoryCurationRequestListOptions struct {
	State            string
	Limit            int
	Cursor           string
	ExcludeSensitive bool
}

// MemoryCurationInputCaps bounds the inputs materialized for one run.
type MemoryCurationInputCaps struct {
	MaxMemories          int `json:"max_memories,omitempty"`
	MaxEvidence          int `json:"max_evidence,omitempty"`
	MaxTranscriptEntries int `json:"max_transcript_entries,omitempty"`
}

// MemoryCurationPreflight is the server's effective authorization and protocol
// contract for the presented bearer token. Clients must use this instead of
// assuming that a deployment capability implies that this credential may use
// it.
type MemoryCurationPreflight struct {
	Principal   MemoryCurationPreflightPrincipal   `json:"principal"`
	Credential  MemoryCurationPreflightCredential  `json:"credential"`
	Protocol    MemoryCurationPreflightProtocol    `json:"protocol"`
	Permissions MemoryCurationPreflightPermissions `json:"permissions"`
	Limits      MemoryCurationPreflightLimits      `json:"limits"`
}

// MemoryCurationPreflightPrincipal identifies the token-derived curation
// principal.
type MemoryCurationPreflightPrincipal struct {
	AccountID string `json:"account_id"`
	RealmID   string `json:"realm_id"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
}

// MemoryCurationPreflightCredential describes the presented credential and its
// effective access profile.
type MemoryCurationPreflightCredential struct {
	TokenID       string     `json:"token_id"`
	AccessProfile string     `json:"access_profile"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
}

// MemoryCurationPreflightProtocol describes the accepted plan protocol and
// inference boundary.
type MemoryCurationPreflightProtocol struct {
	PlanSchema              string   `json:"plan_schema"`
	AllowedPrimitives       []string `json:"allowed_primitives"`
	BackendInference        bool     `json:"backend_inference"`
	ClientInferenceRequired bool     `json:"client_inference_required"`
}

// MemoryCurationPreflightPermissions enumerates operations authorized for the
// presented credential.
type MemoryCurationPreflightPermissions struct {
	ListRequests       bool `json:"list_requests"`
	GetRequest         bool `json:"get_request"`
	Start              bool `json:"start"`
	GetRun             bool `json:"get_run"`
	GetInputs          bool `json:"get_inputs"`
	Renew              bool `json:"renew"`
	Plan               bool `json:"plan"`
	Abandon            bool `json:"abandon"`
	Apply              bool `json:"apply"`
	CreateRequest      bool `json:"create_request"`
	Cancel             bool `json:"cancel"`
	Rollback           bool `json:"rollback"`
	IncludeSensitive   bool `json:"include_sensitive"`
	DirectMemoryWrite  bool `json:"direct_memory_write"`
	CanonicalFactWrite bool `json:"canonical_fact_write"`
	MessageWrite       bool `json:"message_write"`
	PermanentDelete    bool `json:"permanent_delete"`
}

// MemoryCurationPreflightLimits contains the server-enforced curation bounds.
type MemoryCurationPreflightLimits struct {
	MaxPageSize          int   `json:"max_page_size"`
	MaxMemories          int   `json:"max_memories"`
	MaxEvidence          int   `json:"max_evidence"`
	MaxTranscriptEntries int   `json:"max_transcript_entries"`
	MinLeaseSeconds      int64 `json:"min_lease_seconds"`
	MaxLeaseSeconds      int64 `json:"max_lease_seconds"`
	MaxPlanActions       int   `json:"max_plan_actions"`
	MaxPlanBytes         int64 `json:"max_plan_bytes"`
}

// StartMemoryCurationInput claims a due request and configures its leased,
// immutable input snapshot.
type StartMemoryCurationInput struct {
	RequestID      string                  `json:"-"`
	Caps           MemoryCurationInputCaps `json:"caps,omitempty"`
	LeaseDuration  time.Duration           `json:"-"`
	Client         MemoryClientProvenance  `json:"client,omitempty"`
	Budgets        json.RawMessage         `json:"budgets,omitempty"`
	IdempotencyKey string                  `json:"-"`
}

// RenewMemoryCurationInput extends one fenced run lease.
type RenewMemoryCurationInput struct {
	RunID             string        `json:"-"`
	FencingGeneration int64         `json:"fencing_generation"`
	Extension         time.Duration `json:"-"`
	IdempotencyKey    string        `json:"-"`
}

// FinishMemoryCurationInput supplies the fence and reason used to cancel or
// abandon a curation resource.
type FinishMemoryCurationInput struct {
	RunID             string `json:"-"`
	FencingGeneration int64  `json:"fencing_generation"`
	Reason            string `json:"reason,omitempty"`
	IdempotencyKey    string `json:"-"`
}

// PlanMemoryCurationInput submits a client-authored draft for validation and
// immutable acceptance.
type PlanMemoryCurationInput struct {
	RunID             string          `json:"-"`
	FencingGeneration int64           `json:"fencing_generation"`
	Draft             json.RawMessage `json:"draft"`
	IdempotencyKey    string          `json:"-"`
}

// ApplyMemoryCurationInput identifies one exact accepted plan for atomic apply.
type ApplyMemoryCurationInput struct {
	RunID             string `json:"-"`
	FencingGeneration int64  `json:"fencing_generation"`
	PlanRevision      int64  `json:"plan_revision"`
	PlanHash          string `json:"plan_hash"`
	IdempotencyKey    string `json:"-"`
}

// RollbackMemoryCurationInput identifies one apply receipt and the exact heads
// eligible for compensation.
type RollbackMemoryCurationInput struct {
	RunID                 string                   `json:"-"`
	ApplyReceiptID        string                   `json:"apply_receipt_id"`
	ExpectedProducedHeads []MemoryVersionReference `json:"expected_produced_heads"`
	Reason                string                   `json:"reason,omitempty"`
	IdempotencyKey        string                   `json:"-"`
}

// MemoryCurationLane is the serialized request and run state for one owner.
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

// MemoryCurationRequest is one queued or terminal unit of curation work.
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

// MemoryCurationRun is one leased, fenced snapshot processed by a client-side
// curator.
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

// MemoryCurationMutationReceipt is a value-free idempotency receipt for a
// curation state transition.
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

// RequestMemoryCurationResult returns the effective request and mutation
// receipt.
type RequestMemoryCurationResult struct {
	Request MemoryCurationRequest         `json:"request"`
	Receipt MemoryCurationMutationReceipt `json:"receipt"`
}

// MemoryCurationRequestPage is one opaque-cursor page of curation requests.
type MemoryCurationRequestPage struct {
	Requests   []MemoryCurationRequest `json:"requests"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

// StartMemoryCurationResult returns the claimed run, updated request, receipt,
// and initial input cursor.
type StartMemoryCurationResult struct {
	Run              MemoryCurationRun             `json:"run"`
	Request          MemoryCurationRequest         `json:"request"`
	Receipt          MemoryCurationMutationReceipt `json:"receipt"`
	FirstInputCursor string                        `json:"first_input_cursor"`
}

// RenewMemoryCurationResult returns the renewed or terminal run and its
// mutation receipt.
type RenewMemoryCurationResult struct {
	Run     MemoryCurationRun             `json:"run"`
	Receipt MemoryCurationMutationReceipt `json:"receipt"`
}

// FinishMemoryCurationResult is the result of cancelling or abandoning a run.
type FinishMemoryCurationResult = RenewMemoryCurationResult

// MemoryCurationRunInput contains an immutable membership reference and its
// materialized payload, or value-free cursor bounds.
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

// MemoryCurationRunInputPage is one opaque-cursor page of frozen run inputs.
type MemoryCurationRunInputPage struct {
	Run        MemoryCurationRun        `json:"run"`
	Inputs     []MemoryCurationRunInput `json:"inputs"`
	NextCursor string                   `json:"next_cursor,omitempty"`
}

// MemoryCurationStatus reports the owner lane and any selected request or run.
type MemoryCurationStatus struct {
	Lane    MemoryCurationLane     `json:"lane"`
	Request *MemoryCurationRequest `json:"request,omitempty"`
	Run     *MemoryCurationRun     `json:"run,omitempty"`
}

// MemoryCurationPreallocatedMemoryID maps a plan-local reference to its stable
// memory ID.
type MemoryCurationPreallocatedMemoryID struct {
	LocalRef string `json:"local_ref"`
	MemoryID string `json:"memory_id"`
}

// MemoryCurationImpactPreview summarizes an accepted plan using counts only.
type MemoryCurationImpactPreview struct {
	ActionCount           int `json:"action_count"`
	CreateActions         int `json:"create_actions"`
	ReplaceActions        int `json:"replace_actions"`
	SupersedeActions      int `json:"supersede_actions"`
	RelateActions         int `json:"relate_actions"`
	ProposeFactActions    int `json:"propose_fact_actions"`
	NewMemories           int `json:"new_memories"`
	MemoryVersionWrites   int `json:"memory_version_writes"`
	EvidenceRows          int `json:"evidence_rows"`
	RelationRows          int `json:"relation_rows"`
	ExpectedVersionChecks int `json:"expected_version_checks"`
	FactCandidates        int `json:"fact_candidates"`
}

// MemoryCurationPlanReceipt is the immutable, value-free acceptance receipt
// for a normalized plan.
type MemoryCurationPlanReceipt struct {
	ID                string    `json:"id"`
	Operation         string    `json:"operation"`
	ActorID           string    `json:"actor_id"`
	IdempotencyKey    string    `json:"idempotency_key"`
	RequestHash       string    `json:"request_hash"`
	RequestID         string    `json:"request_id"`
	RunID             string    `json:"run_id"`
	RequestGeneration int64     `json:"request_generation"`
	FencingGeneration int64     `json:"fencing_generation"`
	PlanSchema        string    `json:"plan_schema"`
	PlanRevision      int64     `json:"plan_revision"`
	PlanHash          string    `json:"plan_hash"`
	ResultState       string    `json:"result_state"`
	CreatedAt         time.Time `json:"created_at"`
	Replayed          bool      `json:"replayed,omitempty"`
}

// PlanMemoryCurationResult returns the normalized plan, preallocated IDs,
// impact preview, and acceptance receipt.
type PlanMemoryCurationResult struct {
	Run                   MemoryCurationRun                    `json:"run"`
	Plan                  json.RawMessage                      `json:"plan"`
	PreallocatedMemoryIDs []MemoryCurationPreallocatedMemoryID `json:"preallocated_memory_ids,omitempty"`
	Preview               MemoryCurationImpactPreview          `json:"preview"`
	Receipt               MemoryCurationPlanReceipt            `json:"receipt"`
}

// MemoryCurationCursorInterval records one frozen source interval advanced by
// a successful apply.
type MemoryCurationCursorInterval struct {
	SourceKind     string `json:"source_kind"`
	SourceStreamID string `json:"source_stream_id"`
	ExpectedPrior  int64  `json:"expected_prior"`
	Upper          int64  `json:"upper"`
}

// MemoryCurationActionApplyResult records the value-free resources produced by
// one applied action.
type MemoryCurationActionApplyResult struct {
	ActionID                   string                   `json:"action_id"`
	Ordinal                    int64                    `json:"ordinal"`
	Operation                  string                   `json:"operation"`
	BeforeHeads                []MemoryVersionReference `json:"before_heads"`
	AfterHeads                 []MemoryVersionReference `json:"after_heads"`
	CreatedMemoryIDs           []string                 `json:"created_memory_ids"`
	EvidenceIDs                []string                 `json:"evidence_ids"`
	RelationIDs                []string                 `json:"relation_ids"`
	CandidateIDs               []string                 `json:"candidate_ids"`
	SupersessionSetID          string                   `json:"supersession_set_id,omitempty"`
	SupersessionSetRevision    int64                    `json:"supersession_set_revision,omitempty"`
	SupersessionReplacementIDs []MemoryVersionReference `json:"supersession_replacements,omitempty"`
}

// MemoryCurationApplyReceipt is an exact, value-free replay receipt for an
// applied plan.
type MemoryCurationApplyReceipt struct {
	ID                 string                            `json:"id"`
	Operation          string                            `json:"operation"`
	ActorID            string                            `json:"actor_id"`
	IdempotencyKey     string                            `json:"idempotency_key"`
	RequestHash        string                            `json:"request_hash"`
	RequestID          string                            `json:"request_id"`
	RunID              string                            `json:"run_id"`
	RequestGeneration  int64                             `json:"request_generation"`
	FencingGeneration  int64                             `json:"fencing_generation"`
	PlanRevision       int64                             `json:"plan_revision"`
	PlanHash           string                            `json:"plan_hash"`
	ActionResults      []MemoryCurationActionApplyResult `json:"action_results"`
	CursorIntervals    []MemoryCurationCursorInterval    `json:"cursor_intervals"`
	FollowUpRequestID  string                            `json:"follow_up_request_id,omitempty"`
	FollowUpGeneration int64                             `json:"follow_up_generation,omitempty"`
	CreatedAt          time.Time                         `json:"created_at"`
	Replayed           bool                              `json:"replayed,omitempty"`
}

// ApplyMemoryCurationResult returns the applied run, fulfilled request,
// optional follow-up request, and receipt.
type ApplyMemoryCurationResult struct {
	Run             MemoryCurationRun          `json:"run"`
	Request         MemoryCurationRequest      `json:"request"`
	FollowUpRequest *MemoryCurationRequest     `json:"follow_up_request,omitempty"`
	Receipt         MemoryCurationApplyReceipt `json:"receipt"`
}

// MemoryCurationActionRollbackResult records the value-free compensation for
// one applied action.
type MemoryCurationActionRollbackResult struct {
	ActionID              string                   `json:"action_id"`
	Ordinal               int64                    `json:"ordinal"`
	Operation             string                   `json:"operation"`
	CompensationHeads     []MemoryVersionReference `json:"compensation_heads"`
	RevertedRelationIDs   []string                 `json:"reverted_relation_ids"`
	WithdrawnCandidateIDs []string                 `json:"withdrawn_candidate_ids"`
}

// MemoryCurationRollbackReceipt is the value-free replay receipt for one
// compensating rollback.
type MemoryCurationRollbackReceipt struct {
	ID                    string                               `json:"id"`
	Operation             string                               `json:"operation"`
	ActorID               string                               `json:"actor_id"`
	IdempotencyKey        string                               `json:"idempotency_key"`
	RequestHash           string                               `json:"request_hash"`
	RequestID             string                               `json:"request_id"`
	RunID                 string                               `json:"run_id"`
	ApplyReceiptID        string                               `json:"apply_receipt_id"`
	ExpectedProducedHeads []MemoryVersionReference             `json:"expected_produced_heads"`
	ActionResults         []MemoryCurationActionRollbackResult `json:"action_results"`
	ReplayRequestID       string                               `json:"replay_request_id"`
	ReplayGeneration      int64                                `json:"replay_generation"`
	CreatedAt             time.Time                            `json:"created_at"`
	Replayed              bool                                 `json:"replayed,omitempty"`
}

// RollbackMemoryCurationResult returns the rolled-back run, replay request, and
// compensation receipt.
type RollbackMemoryCurationResult struct {
	Run           MemoryCurationRun             `json:"run"`
	ReplayRequest MemoryCurationRequest         `json:"replay_request"`
	Receipt       MemoryCurationRollbackReceipt `json:"receipt"`
}

// GetMemoryCurationPreflight returns the effective curation contract for the
// presented credential.
func GetMemoryCurationPreflight(ctx context.Context, endpoint, token string) (*MemoryCurationPreflight, error) {
	var out MemoryCurationPreflight
	requestURL := strings.TrimRight(endpoint, "/") + "/v1/memory-curation-preflight"
	if err := doJSON(ctx, http.MethodGet, requestURL, token, nil, &out); err != nil {
		return nil, err
	}
	if out.Protocol.AllowedPrimitives == nil {
		out.Protocol.AllowedPrimitives = []string{}
	}
	return &out, nil
}

// RequestMemoryCuration creates or coalesces one curation request.
func RequestMemoryCuration(ctx context.Context, endpoint, token string, in RequestMemoryCurationInput) (*RequestMemoryCurationResult, error) {
	var out RequestMemoryCurationResult
	if err := doMemoryCurationMutation(ctx, http.MethodPost, memoryCurationRequestsURL(endpoint), token, in.IdempotencyKey, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListMemoryCurationRequests retrieves one bounded request queue page.
func ListMemoryCurationRequests(ctx context.Context, endpoint, token string, opts MemoryCurationRequestListOptions) (*MemoryCurationRequestPage, error) {
	q := url.Values{}
	if opts.State != "" {
		q.Set("state", opts.State)
	}
	if opts.Limit != 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.ExcludeSensitive {
		q.Set("exclude_sensitive", "true")
	}
	requestURL := memoryCurationRequestsURL(endpoint)
	if len(q) != 0 {
		requestURL += "?" + q.Encode()
	}
	var out MemoryCurationRequestPage
	if err := doJSON(ctx, http.MethodGet, requestURL, token, nil, &out); err != nil {
		return nil, err
	}
	if out.Requests == nil {
		out.Requests = []MemoryCurationRequest{}
	}
	return &out, nil
}

// GetMemoryCurationRequest retrieves one curation request by ID.
func GetMemoryCurationRequest(ctx context.Context, endpoint, token, requestID string) (*MemoryCurationRequest, error) {
	var out struct {
		Request MemoryCurationRequest `json:"request"`
	}
	if err := doJSON(ctx, http.MethodGet, memoryCurationRequestURL(endpoint, requestID), token, nil, &out); err != nil {
		return nil, err
	}
	return &out.Request, nil
}

// StartMemoryCuration claims one due request and materializes its input
// snapshot.
func StartMemoryCuration(ctx context.Context, endpoint, token string, in StartMemoryCurationInput) (*StartMemoryCurationResult, error) {
	body := struct {
		Caps         MemoryCurationInputCaps `json:"caps,omitempty"`
		LeaseSeconds int64                   `json:"lease_seconds,omitempty"`
		Client       MemoryClientProvenance  `json:"client,omitempty"`
		Budgets      json.RawMessage         `json:"budgets,omitempty"`
	}{in.Caps, durationSeconds(in.LeaseDuration), in.Client, in.Budgets}
	var out StartMemoryCurationResult
	requestURL := memoryCurationRequestURL(endpoint, in.RequestID) + "/start"
	if err := doMemoryCurationMutation(ctx, http.MethodPost, requestURL, token, in.IdempotencyKey, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMemoryCurationRun retrieves one curation run by ID.
func GetMemoryCurationRun(ctx context.Context, endpoint, token, runID string) (*MemoryCurationRun, error) {
	var out struct {
		Run MemoryCurationRun `json:"run"`
	}
	if err := doJSON(ctx, http.MethodGet, memoryCurationRunURL(endpoint, runID), token, nil, &out); err != nil {
		return nil, err
	}
	return &out.Run, nil
}

// GetMemoryCurationRunInputs retrieves one fenced page of frozen run inputs.
func GetMemoryCurationRunInputs(ctx context.Context, endpoint, token, runID string, fencingGeneration int64, cursor string, limit int) (*MemoryCurationRunInputPage, error) {
	q := url.Values{"fencing_generation": {strconv.FormatInt(fencingGeneration, 10)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit != 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out MemoryCurationRunInputPage
	requestURL := memoryCurationRunURL(endpoint, runID) + "/inputs?" + q.Encode()
	if err := doJSON(ctx, http.MethodGet, requestURL, token, nil, &out); err != nil {
		return nil, err
	}
	if out.Inputs == nil {
		out.Inputs = []MemoryCurationRunInput{}
	}
	return &out, nil
}

// RenewMemoryCuration extends one active run lease.
func RenewMemoryCuration(ctx context.Context, endpoint, token string, in RenewMemoryCurationInput) (*RenewMemoryCurationResult, error) {
	body := struct {
		FencingGeneration int64 `json:"fencing_generation"`
		ExtensionSeconds  int64 `json:"extension_seconds"`
	}{in.FencingGeneration, durationSeconds(in.Extension)}
	var out RenewMemoryCurationResult
	if err := doMemoryCurationMutation(ctx, http.MethodPost, memoryCurationRunURL(endpoint, in.RunID)+"/renew", token, in.IdempotencyKey, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlanMemoryCuration validates and accepts one client-authored curation draft.
func PlanMemoryCuration(ctx context.Context, endpoint, token string, in PlanMemoryCurationInput) (*PlanMemoryCurationResult, error) {
	body := struct {
		FencingGeneration int64           `json:"fencing_generation"`
		Draft             json.RawMessage `json:"draft"`
	}{in.FencingGeneration, in.Draft}
	var out PlanMemoryCurationResult
	if err := doMemoryCurationMutation(ctx, http.MethodPost, memoryCurationRunURL(endpoint, in.RunID)+"/plan", token, in.IdempotencyKey, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ApplyMemoryCuration atomically applies one exact accepted plan.
func ApplyMemoryCuration(ctx context.Context, endpoint, token string, in ApplyMemoryCurationInput) (*ApplyMemoryCurationResult, error) {
	body := struct {
		FencingGeneration int64  `json:"fencing_generation"`
		PlanRevision      int64  `json:"plan_revision"`
		PlanHash          string `json:"plan_hash"`
	}{in.FencingGeneration, in.PlanRevision, in.PlanHash}
	var out ApplyMemoryCurationResult
	if err := doMemoryCurationMutation(ctx, http.MethodPost, memoryCurationRunURL(endpoint, in.RunID)+"/apply", token, in.IdempotencyKey, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelMemoryCuration cancels one request or active run using its fence.
func CancelMemoryCuration(ctx context.Context, endpoint, token string, in FinishMemoryCurationInput) (*FinishMemoryCurationResult, error) {
	return finishMemoryCuration(ctx, endpoint, token, "cancel", in)
}

// AbandonMemoryCuration terminates one active run without applying its plan.
func AbandonMemoryCuration(ctx context.Context, endpoint, token string, in FinishMemoryCurationInput) (*FinishMemoryCurationResult, error) {
	return finishMemoryCuration(ctx, endpoint, token, "abandon", in)
}

// RollbackMemoryCuration appends compensation for one exact apply receipt.
func RollbackMemoryCuration(ctx context.Context, endpoint, token string, in RollbackMemoryCurationInput) (*RollbackMemoryCurationResult, error) {
	var out RollbackMemoryCurationResult
	if err := doMemoryCurationMutation(ctx, http.MethodPost, memoryCurationRunURL(endpoint, in.RunID)+"/rollback", token, in.IdempotencyKey, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMemoryCurationStatus reports the current owner lane and optionally one run.
func GetMemoryCurationStatus(ctx context.Context, endpoint, token, runID string) (*MemoryCurationStatus, error) {
	requestURL := strings.TrimRight(endpoint, "/") + "/v1/memory-curation-status"
	if runID != "" {
		requestURL += "?" + url.Values{"run_id": {runID}}.Encode()
	}
	var out MemoryCurationStatus
	if err := doJSON(ctx, http.MethodGet, requestURL, token, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func finishMemoryCuration(ctx context.Context, endpoint, token, action string, in FinishMemoryCurationInput) (*FinishMemoryCurationResult, error) {
	body := struct {
		FencingGeneration int64  `json:"fencing_generation"`
		Reason            string `json:"reason,omitempty"`
	}{in.FencingGeneration, in.Reason}
	var out FinishMemoryCurationResult
	if err := doMemoryCurationMutation(ctx, http.MethodPost, memoryCurationRunURL(endpoint, in.RunID)+"/"+action, token, in.IdempotencyKey, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func doMemoryCurationMutation(ctx context.Context, method, requestURL, token, idempotencyKey string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return doJSONWithHeaders(ctx, method, requestURL, token,
		map[string]string{"Idempotency-Key": idempotencyKey}, body, out)
}

func durationSeconds(value time.Duration) int64 {
	if value == 0 {
		return 0
	}
	return int64(value / time.Second)
}

func memoryCurationRequestsURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/memory-curation-requests"
}

func memoryCurationRequestURL(endpoint, requestID string) string {
	return memoryCurationRequestsURL(endpoint) + "/" + url.PathEscape(requestID)
}

func memoryCurationRunURL(endpoint, runID string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/memory-curation-runs/" + url.PathEscape(runID)
}
