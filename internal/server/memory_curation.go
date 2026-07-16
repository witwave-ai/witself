package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxMemoryCurationMetadataRequestBytes int64 = 128 << 10
	maxMemoryCurationPlanRequestBytes     int64 = 33 << 20
	maxMemoryCurationRollbackRequestBytes int64 = 1 << 20
	maxMemoryCurationCursorBytes                = 8192
	maxMemoryCurationPageSize                   = 200
	maxMemoryCurationJSONDepth                  = 64
)

var (
	// ErrMemoryCurationBusy indicates that the owner already has an active curation run.
	ErrMemoryCurationBusy = errors.New("memory curation owner lane is busy")
	// ErrMemoryCurationNotDue indicates that a queued request is not eligible to start yet.
	ErrMemoryCurationNotDue = errors.New("memory curation request is not due")
	// ErrMemoryCurationLeaseExpired indicates that the run's lease is no longer valid.
	ErrMemoryCurationLeaseExpired = errors.New("memory curation lease expired")
	// ErrMemoryCurationFenceMismatch indicates that the supplied run generation is stale.
	ErrMemoryCurationFenceMismatch = errors.New("memory curation fence mismatch")
)

type memoryCurationPermission string

const (
	memoryCurationPermissionRequest  memoryCurationPermission = "request"
	memoryCurationPermissionList     memoryCurationPermission = "list"
	memoryCurationPermissionGet      memoryCurationPermission = "get"
	memoryCurationPermissionStart    memoryCurationPermission = "start"
	memoryCurationPermissionInputs   memoryCurationPermission = "inputs"
	memoryCurationPermissionGetPlan  memoryCurationPermission = "get_plan"
	memoryCurationPermissionRenew    memoryCurationPermission = "renew"
	memoryCurationPermissionPlan     memoryCurationPermission = "plan"
	memoryCurationPermissionApply    memoryCurationPermission = "apply"
	memoryCurationPermissionCancel   memoryCurationPermission = "cancel"
	memoryCurationPermissionAbandon  memoryCurationPermission = "abandon"
	memoryCurationPermissionRollback memoryCurationPermission = "rollback"
	memoryCurationPermissionStatus   memoryCurationPermission = "status"
)

// MemoryCurationScope is deterministic source selection, never a model prompt.
type MemoryCurationScope struct {
	Sources              []string `json:"sources"`
	MemoryStates         []string `json:"memory_states"`
	IncludeSensitive     bool     `json:"include_sensitive"`
	MaxMemories          int      `json:"max_memories"`
	MaxEvidence          int      `json:"max_evidence"`
	MaxTranscriptEntries int      `json:"max_transcript_entries"`
}

// RequestMemoryCurationRequest describes a durable request to curate selected memory sources.
type RequestMemoryCurationRequest struct {
	Scope             MemoryCurationScope `json:"scope"`
	CoalescingKey     string              `json:"coalescing_key"`
	TriggerReason     string              `json:"trigger_reason"`
	TriggerGeneration int64               `json:"trigger_generation,omitempty"`
	Priority          int                 `json:"priority,omitempty"`
	DueAt             *time.Time          `json:"due_at,omitempty"`
	MaxAttempts       int                 `json:"max_attempts,omitempty"`
	IdempotencyKey    string              `json:"-"`
}

// MemoryCurationRequestListOptions selects a bounded page of curation requests.
type MemoryCurationRequestListOptions struct {
	State            string
	Limit            int
	Cursor           string
	ExcludeSensitive bool
}

// MemoryCurationInputCaps bounds the source material returned to a curator.
type MemoryCurationInputCaps struct {
	MaxMemories          int `json:"max_memories,omitempty"`
	MaxEvidence          int `json:"max_evidence,omitempty"`
	MaxTranscriptEntries int `json:"max_transcript_entries,omitempty"`
}

// StartMemoryCurationRequest acquires a lease and starts one queued curation request.
type StartMemoryCurationRequest struct {
	RequestID      string
	Caps           MemoryCurationInputCaps
	LeaseDuration  time.Duration
	Client         MemoryClientProvenance
	Budgets        json.RawMessage
	IdempotencyKey string
}

// MemoryCurationRunInputOptions selects a fenced page of inputs for an active run.
type MemoryCurationRunInputOptions struct {
	FencingGeneration int64
	Cursor            string
	Limit             int
}

// RenewMemoryCurationRequest extends the lease for a fenced active run.
type RenewMemoryCurationRequest struct {
	FencingGeneration int64
	Extension         time.Duration
	IdempotencyKey    string
}

// PlanMemoryCurationRequest records a client-inferred plan for a fenced run.
type PlanMemoryCurationRequest struct {
	FencingGeneration int64           `json:"fencing_generation"`
	Draft             json.RawMessage `json:"draft"`
	IdempotencyKey    string          `json:"-"`
}

// ApplyMemoryCurationRequest atomically applies an exact recorded plan revision.
type ApplyMemoryCurationRequest struct {
	FencingGeneration int64  `json:"fencing_generation"`
	PlanRevision      int64  `json:"plan_revision"`
	PlanHash          string `json:"plan_hash"`
	IdempotencyKey    string `json:"-"`
}

// FinishMemoryCurationRequest finishes a fenced run without applying another plan.
type FinishMemoryCurationRequest struct {
	FencingGeneration int64  `json:"fencing_generation"`
	Reason            string `json:"reason,omitempty"`
	IdempotencyKey    string `json:"-"`
}

// RollbackMemoryCurationRequest reverses one apply receipt when its outputs remain unchanged.
type RollbackMemoryCurationRequest struct {
	ApplyReceiptID        string                   `json:"apply_receipt_id"`
	ExpectedProducedHeads []MemoryVersionReference `json:"expected_produced_heads"`
	Reason                string                   `json:"reason,omitempty"`
	IdempotencyKey        string                   `json:"-"`
}

// MemoryCurationRollbackBlocker identifies a live dependency that prevents rollback.
type MemoryCurationRollbackBlocker struct {
	Kind       string `json:"kind"`
	MemoryID   string `json:"memory_id,omitempty"`
	Version    int64  `json:"version,omitempty"`
	ResourceID string `json:"resource_id,omitempty"`
}

// MemoryCurationRollbackBlockedError reports dependencies that make rollback unsafe.
type MemoryCurationRollbackBlockedError struct {
	Blockers []MemoryCurationRollbackBlocker
}

func (e *MemoryCurationRollbackBlockedError) Error() string {
	return "memory curation rollback has live dependencies"
}

func (e *MemoryCurationRollbackBlockedError) Unwrap() error { return ErrConflict }

// MemoryCurationPreflight is an authenticated, effective authorization
// document for a client-side curator. Unlike /v1/capabilities it describes the
// presented bearer token, not merely which deployment features exist.
type MemoryCurationPreflight struct {
	Principal   MemoryCurationPreflightPrincipal   `json:"principal"`
	Credential  MemoryCurationPreflightCredential  `json:"credential"`
	Protocol    MemoryCurationPreflightProtocol    `json:"protocol"`
	Permissions MemoryCurationPreflightPermissions `json:"permissions"`
	Limits      MemoryCurationPreflightLimits      `json:"limits"`
}

// MemoryCurationPreflightPrincipal identifies the authenticated curator and its scope.
type MemoryCurationPreflightPrincipal struct {
	AccountID string `json:"account_id"`
	RealmID   string `json:"realm_id"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
}

// MemoryCurationPreflightCredential describes the effective token presented by the curator.
type MemoryCurationPreflightCredential struct {
	TokenID       string     `json:"token_id"`
	AccessProfile string     `json:"access_profile"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
}

// MemoryCurationPreflightProtocol describes the supported client-side planning contract.
type MemoryCurationPreflightProtocol struct {
	PlanSchema              string   `json:"plan_schema"`
	AllowedPrimitives       []string `json:"allowed_primitives"`
	BackendInference        bool     `json:"backend_inference"`
	ClientInferenceRequired bool     `json:"client_inference_required"`
}

// MemoryCurationPreflightPermissions enumerates the token's effective curator permissions.
type MemoryCurationPreflightPermissions struct {
	ListRequests       bool `json:"list_requests"`
	GetRequest         bool `json:"get_request"`
	Start              bool `json:"start"`
	GetRun             bool `json:"get_run"`
	GetInputs          bool `json:"get_inputs"`
	GetPlan            bool `json:"get_plan"`
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

// MemoryCurationPreflightLimits reports server-enforced bounds for curation work.
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

func getMemoryCurationPreflightHandler(auth PrincipalAuthFunc) http.HandlerFunc {
	protected := requireDomainPrincipalAnyProfile(auth, func(w http.ResponseWriter, _ *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may preflight memory curation")
			return
		}
		profile := effectiveAccessProfile(p)
		allowed := func(permission memoryCurationPermission) bool {
			return memoryCurationProfileAllows(profile, permission)
		}
		full := profile == AccessProfileFull
		result := MemoryCurationPreflight{
			Principal: MemoryCurationPreflightPrincipal{
				AccountID: p.AccountID, RealmID: p.RealmID, AgentID: p.ID, AgentName: p.AgentName,
			},
			Credential: MemoryCurationPreflightCredential{
				TokenID: p.TokenID, AccessProfile: profile, ExpiresAt: p.TokenExpiresAt,
			},
			Protocol: MemoryCurationPreflightProtocol{
				PlanSchema:        "witself.memory-plan.v1",
				AllowedPrimitives: []string{"create", "replace", "supersede", "relate", "propose_fact"},
				BackendInference:  false, ClientInferenceRequired: true,
			},
			Permissions: MemoryCurationPreflightPermissions{
				ListRequests:     allowed(memoryCurationPermissionList),
				GetRequest:       allowed(memoryCurationPermissionGet),
				Start:            allowed(memoryCurationPermissionStart),
				GetRun:           allowed(memoryCurationPermissionGet),
				GetInputs:        allowed(memoryCurationPermissionInputs),
				GetPlan:          allowed(memoryCurationPermissionGetPlan),
				Renew:            allowed(memoryCurationPermissionRenew),
				Plan:             allowed(memoryCurationPermissionPlan),
				Abandon:          allowed(memoryCurationPermissionAbandon),
				Apply:            allowed(memoryCurationPermissionApply),
				CreateRequest:    allowed(memoryCurationPermissionRequest),
				Cancel:           allowed(memoryCurationPermissionCancel),
				Rollback:         allowed(memoryCurationPermissionRollback),
				IncludeSensitive: full, DirectMemoryWrite: full,
				CanonicalFactWrite: full, MessageWrite: full, PermanentDelete: full,
			},
			Limits: MemoryCurationPreflightLimits{
				MaxPageSize: 200, MaxMemories: 500, MaxEvidence: 1000,
				MaxTranscriptEntries: 2000, MinLeaseSeconds: 30,
				MaxLeaseSeconds: 1800, MaxPlanActions: 128,
				MaxPlanBytes: 32 << 20,
			},
		}
		writeMemoryCurationResult(w, http.StatusOK, result)
	})
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		protected(w, r)
	}
}

func requestMemoryCurationHandler(
	auth PrincipalAuthFunc,
	request func(context.Context, DomainPrincipal, RequestMemoryCurationRequest) (any, error),
) http.HandlerFunc {
	return requireMemoryCurationAgent(auth, memoryCurationPermissionRequest, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		var in RequestMemoryCurationRequest
		if !decodeStrictMemoryCurationJSON(w, r, &in, maxMemoryCurationMetadataRequestBytes) {
			return
		}
		if !setMemoryCurationIdempotencyKey(w, r, &in.IdempotencyKey) {
			return
		}
		result, err := request(r.Context(), p, in)
		if writeMemoryCurationError(w, err) {
			return
		}
		writeMemoryCurationResult(w, http.StatusCreated, result)
	})
}

func listMemoryCurationRequestsHandler(
	auth PrincipalAuthFunc,
	list func(context.Context, DomainPrincipal, MemoryCurationRequestListOptions) (any, error),
) http.HandlerFunc {
	return requireMemoryCurationAgent(auth, memoryCurationPermissionList, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if err := requireMemoryCurationQuery(r.URL.Query(), "state", "limit", "cursor", "exclude_sensitive"); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		limit, err := parseMemoryCurationLimit(r.URL.Query().Get("limit"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
		if len(cursor) > maxMemoryCurationCursorBytes {
			writeJSONError(w, http.StatusBadRequest, "cursor is too long")
			return
		}
		excludeSensitive := false
		if raw := strings.TrimSpace(r.URL.Query().Get("exclude_sensitive")); raw != "" {
			excludeSensitive, err = strconv.ParseBool(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "exclude_sensitive must be true or false")
				return
			}
		}
		result, err := list(r.Context(), p, MemoryCurationRequestListOptions{
			State: strings.TrimSpace(r.URL.Query().Get("state")), Limit: limit, Cursor: cursor,
			ExcludeSensitive: excludeSensitive,
		})
		if writeMemoryCurationError(w, err) {
			return
		}
		writeMemoryCurationResult(w, http.StatusOK, result)
	})
}

func getMemoryCurationRequestHandler(
	auth PrincipalAuthFunc,
	get func(context.Context, DomainPrincipal, string) (any, error),
) http.HandlerFunc {
	return requireMemoryCurationAgent(auth, memoryCurationPermissionGet, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		requestID := strings.TrimSpace(r.PathValue("request"))
		if requestID == "" {
			writeJSONError(w, http.StatusBadRequest, "memory curation request id is required")
			return
		}
		result, err := get(r.Context(), p, requestID)
		if writeMemoryCurationError(w, err) {
			return
		}
		writeMemoryCurationResult(w, http.StatusOK, map[string]any{"request": result})
	})
}

func startMemoryCurationHandler(
	auth PrincipalAuthFunc,
	start func(context.Context, DomainPrincipal, StartMemoryCurationRequest) (any, error),
) http.HandlerFunc {
	type wireRequest struct {
		Caps         MemoryCurationInputCaps `json:"caps,omitempty"`
		LeaseSeconds int64                   `json:"lease_seconds,omitempty"`
		Client       MemoryClientProvenance  `json:"client,omitempty"`
		Budgets      json.RawMessage         `json:"budgets,omitempty"`
	}
	return requireMemoryCurationAgent(auth, memoryCurationPermissionStart, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		var wire wireRequest
		if !decodeStrictMemoryCurationJSON(w, r, &wire, maxMemoryCurationMetadataRequestBytes) {
			return
		}
		lease, ok := memoryCurationSeconds(w, wire.LeaseSeconds, "lease_seconds")
		if !ok {
			return
		}
		in := StartMemoryCurationRequest{
			RequestID: strings.TrimSpace(r.PathValue("request")), Caps: wire.Caps,
			LeaseDuration: lease, Client: wire.Client, Budgets: wire.Budgets,
		}
		if in.RequestID == "" || !setMemoryCurationIdempotencyKey(w, r, &in.IdempotencyKey) {
			if in.RequestID == "" {
				writeJSONError(w, http.StatusBadRequest, "memory curation request id is required")
			}
			return
		}
		result, err := start(r.Context(), p, in)
		if writeMemoryCurationError(w, err) {
			return
		}
		writeMemoryCurationResult(w, http.StatusCreated, result)
	})
}

func getMemoryCurationRunHandler(
	auth PrincipalAuthFunc,
	get func(context.Context, DomainPrincipal, string) (any, error),
) http.HandlerFunc {
	return requireMemoryCurationAgent(auth, memoryCurationPermissionGet, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		runID := strings.TrimSpace(r.PathValue("run"))
		if runID == "" {
			writeJSONError(w, http.StatusBadRequest, "memory curation run id is required")
			return
		}
		result, err := get(r.Context(), p, runID)
		if writeMemoryCurationError(w, err) {
			return
		}
		writeMemoryCurationResult(w, http.StatusOK, map[string]any{"run": result})
	})
}

func getMemoryCurationRunInputsHandler(
	auth PrincipalAuthFunc,
	get func(context.Context, DomainPrincipal, string, MemoryCurationRunInputOptions) (any, error),
) http.HandlerFunc {
	return requireMemoryCurationAgent(auth, memoryCurationPermissionInputs, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if err := requireMemoryCurationQuery(r.URL.Query(), "fencing_generation", "cursor", "limit"); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		fence, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("fencing_generation")), 10, 64)
		if err != nil || fence < 1 {
			writeJSONError(w, http.StatusBadRequest, "fencing_generation must be positive")
			return
		}
		limit, err := parseMemoryCurationLimit(r.URL.Query().Get("limit"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
		if len(cursor) > maxMemoryCurationCursorBytes {
			writeJSONError(w, http.StatusBadRequest, "cursor is too long")
			return
		}
		result, err := get(r.Context(), p, strings.TrimSpace(r.PathValue("run")), MemoryCurationRunInputOptions{
			FencingGeneration: fence, Cursor: cursor, Limit: limit,
		})
		if writeMemoryCurationError(w, err) {
			return
		}
		writeMemoryCurationResult(w, http.StatusOK, result)
	})
}

func getMemoryCurationPlanHandler(
	auth PrincipalAuthFunc,
	get func(context.Context, DomainPrincipal, string, int64) (any, error),
) http.HandlerFunc {
	return requireMemoryCurationAgent(auth, memoryCurationPermissionGetPlan, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if err := requireMemoryCurationQuery(r.URL.Query(), "fencing_generation"); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		fence, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("fencing_generation")), 10, 64)
		if err != nil || fence < 1 {
			writeJSONError(w, http.StatusBadRequest, "fencing_generation must be positive")
			return
		}
		runID := strings.TrimSpace(r.PathValue("run"))
		if runID == "" {
			writeJSONError(w, http.StatusBadRequest, "memory curation run id is required")
			return
		}
		result, err := get(r.Context(), p, runID, fence)
		if writeMemoryCurationError(w, err) {
			return
		}
		writeMemoryCurationResult(w, http.StatusOK, result)
	})
}

func renewMemoryCurationHandler(
	auth PrincipalAuthFunc,
	renew func(context.Context, DomainPrincipal, string, RenewMemoryCurationRequest) (any, error),
) http.HandlerFunc {
	type wireRequest struct {
		FencingGeneration int64 `json:"fencing_generation"`
		ExtensionSeconds  int64 `json:"extension_seconds"`
	}
	return requireMemoryCurationAgent(auth, memoryCurationPermissionRenew, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		var wire wireRequest
		if !decodeStrictMemoryCurationJSON(w, r, &wire, maxMemoryCurationMetadataRequestBytes) {
			return
		}
		extension, ok := memoryCurationSeconds(w, wire.ExtensionSeconds, "extension_seconds")
		if !ok {
			return
		}
		in := RenewMemoryCurationRequest{FencingGeneration: wire.FencingGeneration, Extension: extension}
		if !setMemoryCurationIdempotencyKey(w, r, &in.IdempotencyKey) {
			return
		}
		result, err := renew(r.Context(), p, strings.TrimSpace(r.PathValue("run")), in)
		if writeMemoryCurationError(w, err) {
			return
		}
		writeMemoryCurationResult(w, http.StatusOK, result)
	})
}

func planMemoryCurationHandler(
	auth PrincipalAuthFunc,
	plan func(context.Context, DomainPrincipal, string, PlanMemoryCurationRequest) (any, error),
) http.HandlerFunc {
	return requireMemoryCurationAgent(auth, memoryCurationPermissionPlan, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		var in PlanMemoryCurationRequest
		if !decodeStrictMemoryCurationJSON(w, r, &in, maxMemoryCurationPlanRequestBytes) ||
			!setMemoryCurationIdempotencyKey(w, r, &in.IdempotencyKey) {
			return
		}
		result, err := plan(r.Context(), p, strings.TrimSpace(r.PathValue("run")), in)
		if writeMemoryCurationError(w, err) {
			return
		}
		writeMemoryCurationResult(w, http.StatusOK, result)
	})
}

func applyMemoryCurationHandler(
	auth PrincipalAuthFunc,
	apply func(context.Context, DomainPrincipal, string, ApplyMemoryCurationRequest) (any, error),
) http.HandlerFunc {
	return requireMemoryCurationAgent(auth, memoryCurationPermissionApply, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		var in ApplyMemoryCurationRequest
		if !decodeStrictMemoryCurationJSON(w, r, &in, maxMemoryCurationMetadataRequestBytes) ||
			!setMemoryCurationIdempotencyKey(w, r, &in.IdempotencyKey) {
			return
		}
		result, err := apply(r.Context(), p, strings.TrimSpace(r.PathValue("run")), in)
		if writeMemoryCurationError(w, err) {
			return
		}
		writeMemoryCurationResult(w, http.StatusOK, result)
	})
}

func finishMemoryCurationHandler(
	auth PrincipalAuthFunc,
	permission memoryCurationPermission,
	finish func(context.Context, DomainPrincipal, string, FinishMemoryCurationRequest) (any, error),
) http.HandlerFunc {
	return requireMemoryCurationAgent(auth, permission, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		var in FinishMemoryCurationRequest
		if !decodeStrictMemoryCurationJSON(w, r, &in, maxMemoryCurationMetadataRequestBytes) ||
			!setMemoryCurationIdempotencyKey(w, r, &in.IdempotencyKey) {
			return
		}
		result, err := finish(r.Context(), p, strings.TrimSpace(r.PathValue("run")), in)
		if writeMemoryCurationError(w, err) {
			return
		}
		writeMemoryCurationResult(w, http.StatusOK, result)
	})
}

func rollbackMemoryCurationHandler(
	auth PrincipalAuthFunc,
	rollback func(context.Context, DomainPrincipal, string, RollbackMemoryCurationRequest) (any, error),
) http.HandlerFunc {
	return requireMemoryCurationAgent(auth, memoryCurationPermissionRollback, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		var in RollbackMemoryCurationRequest
		if !decodeStrictMemoryCurationJSON(w, r, &in, maxMemoryCurationRollbackRequestBytes) ||
			!setMemoryCurationIdempotencyKey(w, r, &in.IdempotencyKey) {
			return
		}
		result, err := rollback(r.Context(), p, strings.TrimSpace(r.PathValue("run")), in)
		if writeMemoryCurationError(w, err) {
			return
		}
		writeMemoryCurationResult(w, http.StatusOK, result)
	})
}

func getMemoryCurationStatusHandler(
	auth PrincipalAuthFunc,
	status func(context.Context, DomainPrincipal, string) (any, error),
) http.HandlerFunc {
	return requireMemoryCurationAgent(auth, memoryCurationPermissionStatus, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if err := requireMemoryCurationQuery(r.URL.Query(), "run_id"); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err := status(r.Context(), p, strings.TrimSpace(r.URL.Query().Get("run_id")))
		if writeMemoryCurationError(w, err) {
			return
		}
		writeMemoryCurationResult(w, http.StatusOK, result)
	})
}

func requireMemoryCurationAgent(
	auth PrincipalAuthFunc,
	permission memoryCurationPermission,
	h func(http.ResponseWriter, *http.Request, DomainPrincipal),
) http.HandlerFunc {
	protected := requireDomainPrincipalAnyProfile(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may curate memories")
			return
		}
		if !memoryCurationProfileAllows(effectiveAccessProfile(p), permission) {
			writeJSONError(w, http.StatusForbidden, "credential profile is not authorized for this memory curation operation")
			return
		}
		h(w, r, p)
	})
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		protected(w, r)
	}
}

func memoryCurationProfileAllows(profile string, permission memoryCurationPermission) bool {
	if profile == AccessProfileFull {
		return true
	}
	previewAllowed := permission == memoryCurationPermissionList ||
		permission == memoryCurationPermissionGet ||
		permission == memoryCurationPermissionStart ||
		permission == memoryCurationPermissionInputs ||
		permission == memoryCurationPermissionGetPlan ||
		permission == memoryCurationPermissionRenew ||
		permission == memoryCurationPermissionPlan ||
		permission == memoryCurationPermissionAbandon ||
		permission == memoryCurationPermissionStatus
	if profile == AccessProfileCuratorPreview {
		return previewAllowed
	}
	return profile == AccessProfileCuratorApply &&
		(previewAllowed || permission == memoryCurationPermissionApply)
}

func decodeStrictMemoryCurationJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "memory curation request body exceeds the supported limit")
		} else {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON memory curation body")
		}
		return false
	}
	if err := validateUniqueMemoryCurationJSON(raw); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON memory curation body")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON memory curation body")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON memory curation body")
		return false
	}
	return true
}

func validateUniqueMemoryCurationJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeUniqueMemoryCurationJSONValue(decoder, 0); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func consumeUniqueMemoryCurationJSONValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= maxMemoryCurationJSONDepth {
		return errors.New("JSON nesting depth exceeds the supported limit")
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object member name is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object member %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeUniqueMemoryCurationJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueMemoryCurationJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if closeDelimiter, ok := closing.(json.Delim); !ok ||
		(delimiter == '{' && closeDelimiter != '}') ||
		(delimiter == '[' && closeDelimiter != ']') {
		return errors.New("mismatched JSON delimiter")
	}
	return nil
}

func setMemoryCurationIdempotencyKey(w http.ResponseWriter, r *http.Request, dst *string) bool {
	*dst = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if *dst == "" {
		writeJSONError(w, http.StatusBadRequest, "Idempotency-Key header is required")
		return false
	}
	return true
}

func writeMemoryCurationError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var blocked *MemoryCurationRollbackBlockedError
	switch {
	case errors.As(err, &blocked):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "error": blocked.Error(), "blockers": blocked.Blockers,
		})
	case errors.Is(err, ErrBadInput):
		writeJSONError(w, http.StatusBadRequest, "invalid memory curation request")
	case errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusForbidden, "memory curation access forbidden")
	case errors.Is(err, ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "memory curation resource not found")
	case errors.Is(err, ErrIdempotencyConflict):
		writeJSONError(w, http.StatusConflict, "idempotency key was reused for a different memory curation mutation")
	case errors.Is(err, ErrMemoryCurationBusy):
		writeJSONError(w, http.StatusConflict, ErrMemoryCurationBusy.Error())
	case errors.Is(err, ErrMemoryCurationNotDue):
		writeJSONError(w, http.StatusConflict, ErrMemoryCurationNotDue.Error())
	case errors.Is(err, ErrMemoryCurationLeaseExpired):
		writeJSONError(w, http.StatusConflict, ErrMemoryCurationLeaseExpired.Error())
	case errors.Is(err, ErrMemoryCurationFenceMismatch):
		writeJSONError(w, http.StatusConflict, ErrMemoryCurationFenceMismatch.Error())
	case errors.Is(err, ErrConflict):
		writeJSONError(w, http.StatusConflict, "memory curation state conflict")
	default:
		writeJSONError(w, http.StatusInternalServerError, "could not complete memory curation request")
	}
	return true
}

func writeMemoryCurationResult(w http.ResponseWriter, status int, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not render memory curation response")
		return
	}
	document := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &document); err != nil || document == nil {
		writeJSONError(w, http.StatusInternalServerError, "could not render memory curation response")
		return
	}
	document["schema_version"] = json.RawMessage(`"witself.v0"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(document)
}

func requireMemoryCurationQuery(query url.Values, allowed ...string) error {
	valid := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		valid[key] = true
	}
	for key, values := range query {
		if !valid[key] {
			return fmt.Errorf("unknown query parameter %q", key)
		}
		if len(values) != 1 {
			return fmt.Errorf("query parameter %q must appear once", key)
		}
	}
	return nil
}

func parseMemoryCurationLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > maxMemoryCurationPageSize {
		return 0, fmt.Errorf("limit must be between 1 and %d", maxMemoryCurationPageSize)
	}
	return value, nil
}

func memoryCurationSeconds(w http.ResponseWriter, seconds int64, field string) (time.Duration, bool) {
	if seconds > math.MaxInt64/int64(time.Second) || seconds < math.MinInt64/int64(time.Second) {
		writeJSONError(w, http.StatusBadRequest, field+" is outside the supported range")
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}
