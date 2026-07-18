package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/avatar"
)

const (
	defaultAvatarHistoryLimit                = 20
	maxAvatarHistoryLimit                    = 100
	maxAvatarMutationRequestBytes      int64 = 16 * 1024
	maxAvatarProposalRequestBytes      int64 = 768 * 1024
	maxAvatarStyleRequestBytes         int64 = 2 * 1024 * 1024
	maxAvatarIdempotencyKeyBytes             = 512
	maxAvatarReasonCodeBytes                 = 128
	minAvatarRetainedPayloadCountLimit       = 4
	maxAvatarRetainedPayloadCountLimit       = 1000
	minAvatarRetainedPayloadByteLimit  int64 = 512 * 1024
	maxAvatarRetainedPayloadByteLimit  int64 = 64 * 1024 * 1024
)

var (
	avatarTargetPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}$`)
	avatarVersionPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	avatarReasonPattern  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	// ErrAvatarPayloadQuotaExceeded is the transport sentinel for a hard,
	// non-retryable avatar creative-payload quota refusal.
	ErrAvatarPayloadQuotaExceeded = errors.New("avatar payload quota exceeded")
)

// AvatarActor identifies the authenticated principal responsible for a
// version or lifecycle mutation.
type AvatarActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// AvatarClientProvenance is untrusted, self-reported generation metadata. It
// is stored for provenance only and never treated as authentication or policy.
type AvatarClientProvenance struct {
	Runtime       string `json:"runtime,omitempty"`
	Model         string `json:"model,omitempty"`
	Recipe        string `json:"recipe,omitempty"`
	RecipeVersion string `json:"recipe_version,omitempty"`
}

// AvatarProfile is the mutable state and immutable-version pointers for one
// account-scoped agent.
type AvatarProfile struct {
	AccountID                 string                `json:"account_id"`
	RealmID                   string                `json:"realm_id"`
	AgentID                   string                `json:"agent_id"`
	SubjectForm               avatar.SubjectForm    `json:"subject_form"`
	AutonomyPolicy            avatar.AutonomyPolicy `json:"autonomy_policy"`
	Status                    avatar.Status         `json:"status"`
	Style                     avatar.StylePackRef   `json:"style"`
	LineageGeneration         int64                 `json:"lineage_generation"`
	ProfileRevision           int64                 `json:"profile_revision"`
	LatestVersion             int64                 `json:"latest_avatar_version,omitempty"`
	ActiveVersion             int64                 `json:"active_avatar_version,omitempty"`
	ProposedVersion           int64                 `json:"proposed_avatar_version,omitempty"`
	AttemptCount              int                   `json:"attempt_count"`
	RetryAfter                *time.Time            `json:"retry_after,omitempty"`
	FallbackSeed              string                `json:"fallback_seed,omitempty"`
	FailureCode               string                `json:"failure_code,omitempty"`
	RetainedPayloadCountLimit int                   `json:"retained_payload_count_limit"`
	RetainedPayloadByteLimit  int64                 `json:"retained_payload_byte_limit"`
	RetainedPayloadCount      int                   `json:"retained_payload_count"`
	RetainedPayloadBytes      int64                 `json:"retained_payload_bytes"`
	RollbackPayloadFloor      int                   `json:"rollback_payload_floor"`
	CreatedAt                 time.Time             `json:"created_at"`
	UpdatedAt                 time.Time             `json:"updated_at"`
}

// AvatarVersion is one immutable sanitized SVG and its model-neutral metadata.
type AvatarVersion struct {
	ID                      string                 `json:"id"`
	AccountID               string                 `json:"account_id"`
	RealmID                 string                 `json:"realm_id"`
	AgentID                 string                 `json:"agent_id"`
	Version                 int64                  `json:"version"`
	ParentVersion           *int64                 `json:"parent_version,omitempty"`
	LineageGeneration       int64                  `json:"lineage_generation"`
	SubjectForm             avatar.SubjectForm     `json:"subject_form"`
	Description             string                 `json:"description,omitempty"`
	VisualSpec              json.RawMessage        `json:"visual_spec,omitempty"`
	SVG                     string                 `json:"svg,omitempty"`
	SVGSHA256               string                 `json:"svg_sha256"`
	LockedLayersSHA256      string                 `json:"locked_layers_sha256"`
	Style                   avatar.StylePackRef    `json:"style"`
	Provenance              AvatarClientProvenance `json:"provenance,omitempty"`
	ProposedBy              AvatarActor            `json:"proposed_by"`
	ProposedAt              time.Time              `json:"proposed_at"`
	IsActive                bool                   `json:"is_active"`
	IsProposed              bool                   `json:"is_proposed"`
	WasActivated            bool                   `json:"was_activated"`
	RollbackEligible        bool                   `json:"rollback_eligible"`
	Rejected                bool                   `json:"rejected"`
	LastActivatedAt         *time.Time             `json:"last_activated_at,omitempty"`
	RejectedAt              *time.Time             `json:"rejected_at,omitempty"`
	PayloadState            avatar.PayloadState    `json:"payload_state"`
	PayloadBytes            int64                  `json:"payload_bytes"`
	PayloadCompactedAt      *time.Time             `json:"payload_compacted_at,omitempty"`
	PayloadCompactionReason string                 `json:"payload_compaction_reason,omitempty"`
}

// AvatarVersionSummary is the payload-free history representation used to
// select an exact immutable version for a subsequent detail or rollback read.
type AvatarVersionSummary struct {
	ID                      string              `json:"id"`
	AccountID               string              `json:"account_id"`
	RealmID                 string              `json:"realm_id"`
	AgentID                 string              `json:"agent_id"`
	Version                 int64               `json:"version"`
	ParentVersion           *int64              `json:"parent_version,omitempty"`
	LineageGeneration       int64               `json:"lineage_generation"`
	SubjectForm             avatar.SubjectForm  `json:"subject_form"`
	SVGSHA256               string              `json:"svg_sha256"`
	LockedLayersSHA256      string              `json:"locked_layers_sha256"`
	Style                   avatar.StylePackRef `json:"style"`
	ProposedBy              AvatarActor         `json:"proposed_by"`
	ProposedAt              time.Time           `json:"proposed_at"`
	IsActive                bool                `json:"is_active"`
	IsProposed              bool                `json:"is_proposed"`
	WasActivated            bool                `json:"was_activated"`
	RollbackEligible        bool                `json:"rollback_eligible"`
	Rejected                bool                `json:"rejected"`
	LastActivatedAt         *time.Time          `json:"last_activated_at,omitempty"`
	RejectedAt              *time.Time          `json:"rejected_at,omitempty"`
	PayloadState            avatar.PayloadState `json:"payload_state"`
	PayloadBytes            int64               `json:"payload_bytes"`
	PayloadCompactedAt      *time.Time          `json:"payload_compacted_at,omitempty"`
	PayloadCompactionReason string              `json:"payload_compaction_reason,omitempty"`
}

// AvatarView contains the profile plus its exact active and pending payloads.
type AvatarView struct {
	Profile  AvatarProfile  `json:"profile"`
	Active   *AvatarVersion `json:"active,omitempty"`
	Proposed *AvatarVersion `json:"proposed,omitempty"`
}

// AvatarHistoryPage is the bounded immutable version history response.
type AvatarHistoryPage struct {
	SchemaVersion     string                 `json:"schema_version,omitempty"`
	Versions          []AvatarVersionSummary `json:"versions"`
	NextBeforeVersion int64                  `json:"next_before_version,omitempty"`
}

// AvatarHistoryOptions selects one exclusive version-cursor page.
type AvatarHistoryOptions struct {
	Limit         int
	BeforeVersion int64
}

// AvatarStyleView is a realm's selected immutable style-pack version.
type AvatarStyleView struct {
	RealmID       string              `json:"realm_id"`
	StyleRevision int64               `json:"style_revision"`
	StylePack     avatar.StylePack    `json:"style_pack"`
	Rollout       *AvatarStyleRollout `json:"rollout,omitempty"`
	CreatedAt     time.Time           `json:"created_at"`
	UpdatedAt     time.Time           `json:"updated_at"`
}

// AvatarStyleRollout exposes bounded, value-free propagation progress to an
// authorized realm operator.
type AvatarStyleRollout struct {
	StyleRevision         int64      `json:"style_revision"`
	StylePackID           string     `json:"style_pack_id"`
	StylePackVersion      int        `json:"style_pack_version"`
	Status                string     `json:"status"`
	TargetProfileCount    *int64     `json:"target_profile_count,omitempty"`
	ProcessedProfileCount int64      `json:"processed_profile_count"`
	BatchCount            int64      `json:"batch_count"`
	LastBatchSize         int        `json:"last_batch_size"`
	FailureCount          int        `json:"failure_count"`
	RetryAfter            *time.Time `json:"retry_after,omitempty"`
	LastFailureCode       string     `json:"last_failure_code,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	StartedAt             *time.Time `json:"started_at,omitempty"`
	UpdatedAt             time.Time  `json:"updated_at"`
	CompletedAt           *time.Time `json:"completed_at,omitempty"`
	SupersededAt          *time.Time `json:"superseded_at,omitempty"`
}

// AvatarMutationReceipt is value-free and safe to retain for retry auditing.
type AvatarMutationReceipt struct {
	Operation               string      `json:"operation"`
	Actor                   AvatarActor `json:"actor"`
	RequestHash             string      `json:"request_hash"`
	ResultRevision          int64       `json:"result_revision"`
	ResultVersion           int64       `json:"result_version,omitempty"`
	ResultLineageGeneration int64       `json:"result_lineage_generation,omitempty"`
	Replayed                bool        `json:"replayed,omitempty"`
	CreatedAt               time.Time   `json:"created_at"`
}

// AvatarMutationResult is the post-mutation view and retry-safe receipt.
type AvatarMutationResult struct {
	Avatar  AvatarView            `json:"avatar"`
	Receipt AvatarMutationReceipt `json:"receipt"`
}

// AvatarStyleMutationResult is the selected realm style and retry receipt.
type AvatarStyleMutationResult struct {
	Style   AvatarStyleView       `json:"style"`
	Receipt AvatarMutationReceipt `json:"receipt"`
}

// ProposeAvatarRequest submits a client-generated candidate. Identity is
// deliberately absent and always comes from the bearer token or operator path.
type ProposeAvatarRequest struct {
	ExpectedProfileRevision int64                  `json:"expected_profile_revision"`
	ParentVersion           int64                  `json:"parent_version,omitempty"`
	StylePackID             string                 `json:"style_pack_id"`
	StylePackVersion        int                    `json:"style_pack_version"`
	SubjectForm             avatar.SubjectForm     `json:"subject_form"`
	Description             string                 `json:"description"`
	VisualSpec              json.RawMessage        `json:"visual_spec"`
	SVG                     string                 `json:"svg"`
	Provenance              AvatarClientProvenance `json:"provenance,omitempty"`
	IdempotencyKey          string                 `json:"-"`
}

// ActivateAvatarRequest advances the active pointer under optimistic
// concurrency.
type ActivateAvatarRequest struct {
	Version                 int64  `json:"version"`
	ExpectedProfileRevision int64  `json:"expected_profile_revision"`
	IdempotencyKey          string `json:"-"`
}

// RollbackAvatarRequest reactivates an earlier immutable version.
type RollbackAvatarRequest struct {
	Version                 int64  `json:"version"`
	ExpectedProfileRevision int64  `json:"expected_profile_revision"`
	IdempotencyKey          string `json:"-"`
}

// ResetAvatarRequest retires the current avatar lineage without purging its
// immutable history and returns the profile to generation-due state.
type ResetAvatarRequest struct {
	ExpectedProfileRevision int64  `json:"expected_profile_revision"`
	ReasonCode              string `json:"reason_code,omitempty"`
	IdempotencyKey          string `json:"-"`
}

// RejectAvatarRequest rejects one pending proposal using a bounded reason code.
type RejectAvatarRequest struct {
	Version                 int64  `json:"version"`
	ExpectedProfileRevision int64  `json:"expected_profile_revision"`
	ReasonCode              string `json:"reason_code,omitempty"`
	IdempotencyKey          string `json:"-"`
}

// AvatarGenerationFailureRequest records one bounded failed attempt.
type AvatarGenerationFailureRequest struct {
	ExpectedProfileRevision int64  `json:"expected_profile_revision"`
	ReasonCode              string `json:"reason_code"`
	IdempotencyKey          string `json:"-"`
}

// UpdateAvatarPolicyRequest updates one agent's activation policy.
type UpdateAvatarPolicyRequest struct {
	Policy                  avatar.AutonomyPolicy `json:"policy"`
	ExpectedProfileRevision int64                 `json:"expected_profile_revision"`
	IdempotencyKey          string                `json:"-"`
}

// UpdateAvatarQuotaRequest updates one agent's retained-content limits.
type UpdateAvatarQuotaRequest struct {
	RetainedPayloadCountLimit int    `json:"retained_payload_count_limit"`
	RetainedPayloadByteLimit  int64  `json:"retained_payload_byte_limit"`
	ExpectedProfileRevision   int64  `json:"expected_profile_revision"`
	IdempotencyKey            string `json:"-"`
}

// CreateAvatarStyleVersionRequest publishes and selects one immutable realm
// style-pack version under optimistic concurrency.
type CreateAvatarStyleVersionRequest struct {
	ExpectedStyleRevision int64            `json:"expected_style_revision"`
	StylePack             avatar.StylePack `json:"style_pack"`
	IdempotencyKey        string           `json:"-"`
}

func getSelfAvatarHandler(
	auth PrincipalAuthFunc,
	get func(context.Context, DomainPrincipal) (AvatarView, error),
) http.HandlerFunc {
	return avatarAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !avatarNoQuery(w, r) {
			return
		}
		view, err := get(r.Context(), p)
		if writeAvatarError(w, err, "read avatar") {
			return
		}
		writeAvatarView(w, http.StatusOK, view)
	})
}

func getSelfAvatarHistoryHandler(
	auth PrincipalAuthFunc,
	get func(context.Context, DomainPrincipal, AvatarHistoryOptions) (AvatarHistoryPage, error),
) http.HandlerFunc {
	return avatarAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		opts, ok := parseAvatarHistoryOptions(w, r)
		if !ok {
			return
		}
		page, err := get(r.Context(), p, opts)
		if writeAvatarError(w, err, "read avatar history") {
			return
		}
		page.SchemaVersion = "witself.v0"
		if page.Versions == nil {
			page.Versions = []AvatarVersionSummary{}
		}
		writeAvatarJSON(w, http.StatusOK, page)
	})
}

func getSelfAvatarVersionHandler(
	auth PrincipalAuthFunc,
	get func(context.Context, DomainPrincipal, int64) (AvatarVersion, error),
) http.HandlerFunc {
	return avatarAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		version, ok := avatarPathVersion(w, r)
		if !ok || !avatarNoQuery(w, r) {
			return
		}
		avatarVersion, err := get(r.Context(), p, version)
		if writeAvatarError(w, err, "read avatar version") {
			return
		}
		writeAvatarVersion(w, http.StatusOK, avatarVersion)
	})
}

func getSelfAvatarStyleHandler(
	auth PrincipalAuthFunc,
	get func(context.Context, DomainPrincipal) (AvatarStyleView, error),
) http.HandlerFunc {
	return avatarAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !avatarNoQuery(w, r) {
			return
		}
		style, err := get(r.Context(), p)
		if writeAvatarError(w, err, "read avatar style") {
			return
		}
		// Defense in depth for alternate backends: self routes never expose
		// realm-wide rollout counts or scheduling timestamps.
		style.Rollout = nil
		writeAvatarStyle(w, http.StatusOK, style)
	})
}

func proposeSelfAvatarHandler(
	auth PrincipalAuthFunc,
	propose func(context.Context, DomainPrincipal, ProposeAvatarRequest) (AvatarMutationResult, error),
) http.HandlerFunc {
	return avatarAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		var in ProposeAvatarRequest
		if !decodeAvatarMutation(w, r, &in, maxAvatarProposalRequestBytes) || !normalizeAvatarProposal(w, &in) {
			return
		}
		result, err := propose(r.Context(), p, in)
		if writeAvatarError(w, err, "propose avatar") {
			return
		}
		writeAvatarMutation(w, http.StatusCreated, result)
	})
}

func activateSelfAvatarHandler(
	auth PrincipalAuthFunc,
	activate func(context.Context, DomainPrincipal, ActivateAvatarRequest) (AvatarMutationResult, error),
) http.HandlerFunc {
	return avatarAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		var in ActivateAvatarRequest
		if !decodeAvatarMutation(w, r, &in, maxAvatarMutationRequestBytes) || !validateAvatarVersionMutation(w, in.Version, in.ExpectedProfileRevision) {
			return
		}
		result, err := activate(r.Context(), p, in)
		if writeAvatarError(w, err, "activate avatar") {
			return
		}
		writeAvatarMutation(w, http.StatusOK, result)
	})
}

func rollbackSelfAvatarHandler(
	auth PrincipalAuthFunc,
	rollback func(context.Context, DomainPrincipal, RollbackAvatarRequest) (AvatarMutationResult, error),
) http.HandlerFunc {
	return avatarAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		var in RollbackAvatarRequest
		if !decodeAvatarMutation(w, r, &in, maxAvatarMutationRequestBytes) || !validateAvatarVersionMutation(w, in.Version, in.ExpectedProfileRevision) {
			return
		}
		result, err := rollback(r.Context(), p, in)
		if writeAvatarError(w, err, "rollback avatar") {
			return
		}
		writeAvatarMutation(w, http.StatusOK, result)
	})
}

func resetSelfAvatarHandler(
	auth PrincipalAuthFunc,
	reset func(context.Context, DomainPrincipal, ResetAvatarRequest) (AvatarMutationResult, error),
) http.HandlerFunc {
	return avatarAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		var in ResetAvatarRequest
		if !decodeAvatarMutation(w, r, &in, maxAvatarMutationRequestBytes) || !normalizeAvatarReset(w, &in) {
			return
		}
		result, err := reset(r.Context(), p, in)
		if writeAvatarError(w, err, "reset avatar") {
			return
		}
		writeAvatarMutation(w, http.StatusOK, result)
	})
}

func avatarGenerationFailureHandler(
	auth PrincipalAuthFunc,
	report func(context.Context, DomainPrincipal, AvatarGenerationFailureRequest) (AvatarMutationResult, error),
) http.HandlerFunc {
	return avatarAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		var in AvatarGenerationFailureRequest
		if !decodeAvatarMutation(w, r, &in, maxAvatarMutationRequestBytes) {
			return
		}
		in.ReasonCode = strings.TrimSpace(in.ReasonCode)
		if in.ExpectedProfileRevision < 1 || !validAvatarReason(in.ReasonCode, false) {
			writeJSONError(w, http.StatusBadRequest, "invalid avatar generation failure request")
			return
		}
		result, err := report(r.Context(), p, in)
		if writeAvatarError(w, err, "record avatar generation failure") {
			return
		}
		writeAvatarMutation(w, http.StatusOK, result)
	})
}

func getAgentAvatarHandler(
	auth AuthFunc,
	get func(context.Context, string, string, string) (AvatarView, error),
) http.HandlerFunc {
	return avatarOperatorHandler(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agentID, ok := avatarPathTarget(w, r, "agent")
		if !ok || !avatarNoQuery(w, r) {
			return
		}
		view, err := get(r.Context(), p.accountID, p.operatorID, agentID)
		if writeAvatarError(w, err, "read avatar") {
			return
		}
		writeAvatarView(w, http.StatusOK, view)
	})
}

func getAgentAvatarHistoryHandler(
	auth AuthFunc,
	get func(context.Context, string, string, string, AvatarHistoryOptions) (AvatarHistoryPage, error),
) http.HandlerFunc {
	return avatarOperatorHandler(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agentID, ok := avatarPathTarget(w, r, "agent")
		if !ok {
			return
		}
		opts, ok := parseAvatarHistoryOptions(w, r)
		if !ok {
			return
		}
		page, err := get(r.Context(), p.accountID, p.operatorID, agentID, opts)
		if writeAvatarError(w, err, "read avatar history") {
			return
		}
		page.SchemaVersion = "witself.v0"
		if page.Versions == nil {
			page.Versions = []AvatarVersionSummary{}
		}
		writeAvatarJSON(w, http.StatusOK, page)
	})
}

func getAgentAvatarVersionHandler(
	auth AuthFunc,
	get func(context.Context, string, string, string, int64) (AvatarVersion, error),
) http.HandlerFunc {
	return avatarOperatorHandler(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agentID, ok := avatarPathTarget(w, r, "agent")
		if !ok {
			return
		}
		version, ok := avatarPathVersion(w, r)
		if !ok || !avatarNoQuery(w, r) {
			return
		}
		avatarVersion, err := get(r.Context(), p.accountID, p.operatorID, agentID, version)
		if writeAvatarError(w, err, "read avatar version") {
			return
		}
		writeAvatarVersion(w, http.StatusOK, avatarVersion)
	})
}

func proposeAgentAvatarHandler(
	auth AuthFunc,
	propose func(context.Context, string, string, string, ProposeAvatarRequest) (AvatarMutationResult, error),
) http.HandlerFunc {
	return avatarOperatorHandler(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agentID, ok := avatarPathTarget(w, r, "agent")
		if !ok {
			return
		}
		var in ProposeAvatarRequest
		if !decodeAvatarMutation(w, r, &in, maxAvatarProposalRequestBytes) || !normalizeAvatarProposal(w, &in) {
			return
		}
		result, err := propose(r.Context(), p.accountID, p.operatorID, agentID, in)
		if writeAvatarError(w, err, "propose avatar") {
			return
		}
		writeAvatarMutation(w, http.StatusCreated, result)
	})
}

func activateAgentAvatarHandler(
	auth AuthFunc,
	activate func(context.Context, string, string, string, ActivateAvatarRequest) (AvatarMutationResult, error),
) http.HandlerFunc {
	return avatarOperatorHandler(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agentID, ok := avatarPathTarget(w, r, "agent")
		if !ok {
			return
		}
		var in ActivateAvatarRequest
		if !decodeAvatarMutation(w, r, &in, maxAvatarMutationRequestBytes) || !validateAvatarVersionMutation(w, in.Version, in.ExpectedProfileRevision) {
			return
		}
		result, err := activate(r.Context(), p.accountID, p.operatorID, agentID, in)
		if writeAvatarError(w, err, "activate avatar") {
			return
		}
		writeAvatarMutation(w, http.StatusOK, result)
	})
}

func rejectAgentAvatarHandler(
	auth AuthFunc,
	reject func(context.Context, string, string, string, RejectAvatarRequest) (AvatarMutationResult, error),
) http.HandlerFunc {
	return avatarOperatorHandler(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agentID, ok := avatarPathTarget(w, r, "agent")
		if !ok {
			return
		}
		var in RejectAvatarRequest
		if !decodeAvatarMutation(w, r, &in, maxAvatarMutationRequestBytes) {
			return
		}
		in.ReasonCode = strings.TrimSpace(in.ReasonCode)
		if !validateAvatarVersionMutation(w, in.Version, in.ExpectedProfileRevision) {
			return
		}
		if !validAvatarReason(in.ReasonCode, true) {
			writeJSONError(w, http.StatusBadRequest, "invalid avatar rejection request")
			return
		}
		result, err := reject(r.Context(), p.accountID, p.operatorID, agentID, in)
		if writeAvatarError(w, err, "reject avatar") {
			return
		}
		writeAvatarMutation(w, http.StatusOK, result)
	})
}

func rollbackAgentAvatarHandler(
	auth AuthFunc,
	rollback func(context.Context, string, string, string, RollbackAvatarRequest) (AvatarMutationResult, error),
) http.HandlerFunc {
	return avatarOperatorHandler(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agentID, ok := avatarPathTarget(w, r, "agent")
		if !ok {
			return
		}
		var in RollbackAvatarRequest
		if !decodeAvatarMutation(w, r, &in, maxAvatarMutationRequestBytes) || !validateAvatarVersionMutation(w, in.Version, in.ExpectedProfileRevision) {
			return
		}
		result, err := rollback(r.Context(), p.accountID, p.operatorID, agentID, in)
		if writeAvatarError(w, err, "rollback avatar") {
			return
		}
		writeAvatarMutation(w, http.StatusOK, result)
	})
}

func resetAgentAvatarHandler(
	auth AuthFunc,
	reset func(context.Context, string, string, string, ResetAvatarRequest) (AvatarMutationResult, error),
) http.HandlerFunc {
	return avatarOperatorHandler(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agentID, ok := avatarPathTarget(w, r, "agent")
		if !ok {
			return
		}
		var in ResetAvatarRequest
		if !decodeAvatarMutation(w, r, &in, maxAvatarMutationRequestBytes) || !normalizeAvatarReset(w, &in) {
			return
		}
		result, err := reset(r.Context(), p.accountID, p.operatorID, agentID, in)
		if writeAvatarError(w, err, "reset avatar") {
			return
		}
		writeAvatarMutation(w, http.StatusOK, result)
	})
}

func updateAgentAvatarPolicyHandler(
	auth AuthFunc,
	update func(context.Context, string, string, string, UpdateAvatarPolicyRequest) (AvatarMutationResult, error),
) http.HandlerFunc {
	return avatarOperatorHandler(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agentID, ok := avatarPathTarget(w, r, "agent")
		if !ok {
			return
		}
		var in UpdateAvatarPolicyRequest
		if !decodeAvatarMutation(w, r, &in, maxAvatarMutationRequestBytes) {
			return
		}
		if in.ExpectedProfileRevision < 1 || in.Policy.Validate() != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid avatar policy request")
			return
		}
		result, err := update(r.Context(), p.accountID, p.operatorID, agentID, in)
		if writeAvatarError(w, err, "update avatar policy") {
			return
		}
		writeAvatarMutation(w, http.StatusOK, result)
	})
}

func updateAgentAvatarQuotaHandler(
	auth AuthFunc,
	update func(context.Context, string, string, string, UpdateAvatarQuotaRequest) (AvatarMutationResult, error),
) http.HandlerFunc {
	return avatarOperatorHandler(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agentID, ok := avatarPathTarget(w, r, "agent")
		if !ok {
			return
		}
		var in UpdateAvatarQuotaRequest
		if !decodeAvatarMutation(w, r, &in, maxAvatarMutationRequestBytes) {
			return
		}
		if in.ExpectedProfileRevision < 1 ||
			in.RetainedPayloadCountLimit < minAvatarRetainedPayloadCountLimit ||
			in.RetainedPayloadCountLimit > maxAvatarRetainedPayloadCountLimit ||
			in.RetainedPayloadByteLimit < minAvatarRetainedPayloadByteLimit ||
			in.RetainedPayloadByteLimit > maxAvatarRetainedPayloadByteLimit {
			writeJSONError(w, http.StatusBadRequest, "invalid avatar payload quota request")
			return
		}
		result, err := update(r.Context(), p.accountID, p.operatorID, agentID, in)
		if writeAvatarError(w, err, "update avatar payload quota") {
			return
		}
		writeAvatarMutation(w, http.StatusOK, result)
	})
}

func getRealmAvatarStyleHandler(
	auth AuthFunc,
	get func(context.Context, string, string, string) (AvatarStyleView, error),
) http.HandlerFunc {
	return avatarOperatorHandler(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		realmID, ok := avatarPathTarget(w, r, "realm")
		if !ok || !avatarNoQuery(w, r) {
			return
		}
		style, err := get(r.Context(), p.accountID, p.operatorID, realmID)
		if writeAvatarError(w, err, "read avatar style") {
			return
		}
		writeAvatarStyle(w, http.StatusOK, style)
	})
}

func createRealmAvatarStyleVersionHandler(
	auth AuthFunc,
	create func(context.Context, string, string, string, CreateAvatarStyleVersionRequest) (AvatarStyleMutationResult, error),
) http.HandlerFunc {
	return avatarOperatorHandler(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		realmID, ok := avatarPathTarget(w, r, "realm")
		if !ok {
			return
		}
		var in CreateAvatarStyleVersionRequest
		if !decodeAvatarMutation(w, r, &in, maxAvatarStyleRequestBytes) {
			return
		}
		if in.ExpectedStyleRevision < 1 || in.StylePack.Validate() != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid avatar style request")
			return
		}
		result, err := create(r.Context(), p.accountID, p.operatorID, realmID, in)
		if writeAvatarError(w, err, "create avatar style version") {
			return
		}
		writeAvatarStyleMutation(w, http.StatusCreated, result)
	})
}

func avatarAgentHandler(auth PrincipalAuthFunc, handler func(http.ResponseWriter, *http.Request, DomainPrincipal)) http.HandlerFunc {
	return avatarNoStore(requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent || strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.AccountID) == "" || strings.TrimSpace(p.RealmID) == "" {
			writeJSONError(w, http.StatusForbidden, "only a full agent token may use the self avatar route")
			return
		}
		handler(w, r, p)
	}))
}

func avatarOperatorHandler(auth AuthFunc, handler func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
	return avatarNoStore(requireOperator(auth, handler))
}

func decodeAvatarMutation(w http.ResponseWriter, r *http.Request, dst any, maximum int64) bool {
	if !avatarNoQuery(w, r) {
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maximum)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON avatar body")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON avatar body")
		return false
	}
	idempotencyKey, ok := avatarIdempotencyKey(w, r)
	if !ok {
		return false
	}
	switch in := dst.(type) {
	case *ProposeAvatarRequest:
		in.IdempotencyKey = idempotencyKey
	case *ActivateAvatarRequest:
		in.IdempotencyKey = idempotencyKey
	case *RollbackAvatarRequest:
		in.IdempotencyKey = idempotencyKey
	case *ResetAvatarRequest:
		in.IdempotencyKey = idempotencyKey
	case *RejectAvatarRequest:
		in.IdempotencyKey = idempotencyKey
	case *AvatarGenerationFailureRequest:
		in.IdempotencyKey = idempotencyKey
	case *UpdateAvatarPolicyRequest:
		in.IdempotencyKey = idempotencyKey
	case *UpdateAvatarQuotaRequest:
		in.IdempotencyKey = idempotencyKey
	case *CreateAvatarStyleVersionRequest:
		in.IdempotencyKey = idempotencyKey
	default:
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return false
	}
	return true
}

func normalizeAvatarProposal(w http.ResponseWriter, in *ProposeAvatarRequest) bool {
	in.StylePackID = strings.TrimSpace(in.StylePackID)
	if in.ExpectedProfileRevision < 1 || in.ParentVersion < 0 ||
		(avatar.StylePackRef{RealmID: "realm", StylePackID: in.StylePackID, Version: in.StylePackVersion}).Validate() != nil ||
		in.SubjectForm.Validate() != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid avatar proposal request")
		return false
	}
	description, err := avatar.NormalizeDescription(in.Description)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid avatar proposal description")
		return false
	}
	visualSpec, err := avatar.NormalizeSpecJSON(in.VisualSpec)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid avatar proposal visual_spec")
		return false
	}
	svg, err := avatar.SanitizeSVG([]byte(in.SVG))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid avatar proposal svg")
		return false
	}
	if !normalizeAvatarProvenance(&in.Provenance) {
		writeJSONError(w, http.StatusBadRequest, "invalid avatar proposal provenance")
		return false
	}
	in.Description = description
	in.VisualSpec = visualSpec
	in.SVG = string(svg)
	return true
}

func normalizeAvatarProvenance(in *AvatarClientProvenance) bool {
	values := []*string{&in.Runtime, &in.Model, &in.Recipe, &in.RecipeVersion}
	for _, value := range values {
		normalized, err := avatar.NormalizeClientProvenanceLabel(*value)
		if err != nil {
			return false
		}
		*value = normalized
	}
	return true
}

func validAvatarMetadata(value string, maximum int) bool {
	if value == "" {
		return true
	}
	if len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validateAvatarVersionMutation(w http.ResponseWriter, version, revision int64) bool {
	if version < 1 || revision < 1 {
		writeJSONError(w, http.StatusBadRequest, "version and expected_profile_revision are required")
		return false
	}
	return true
}

func normalizeAvatarReset(w http.ResponseWriter, in *ResetAvatarRequest) bool {
	in.ReasonCode = strings.TrimSpace(in.ReasonCode)
	if in.ExpectedProfileRevision < 1 || !validAvatarReason(in.ReasonCode, true) {
		writeJSONError(w, http.StatusBadRequest, "invalid avatar reset request")
		return false
	}
	return true
}

func validAvatarReason(value string, optional bool) bool {
	if value == "" {
		return optional
	}
	return len(value) <= maxAvatarReasonCodeBytes && avatarReasonPattern.MatchString(value)
}

func avatarPathTarget(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	value := strings.TrimSpace(r.PathValue(name))
	if !avatarTargetPattern.MatchString(value) {
		writeJSONError(w, http.StatusNotFound, "avatar resource not found")
		return "", false
	}
	return value, true
}

func avatarPathVersion(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("version")
	if !avatarVersionPattern.MatchString(raw) {
		writeJSONError(w, http.StatusNotFound, "avatar resource not found")
		return 0, false
	}
	version, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || version < 1 {
		writeJSONError(w, http.StatusNotFound, "avatar resource not found")
		return 0, false
	}
	return version, true
}

func avatarIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if value == "" || len(value) > maxAvatarIdempotencyKeyBytes || !validAvatarMetadata(value, maxAvatarIdempotencyKeyBytes) {
		writeJSONError(w, http.StatusBadRequest, "valid Idempotency-Key header is required")
		return "", false
	}
	return value, true
}

func avatarNoQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		writeJSONError(w, http.StatusBadRequest, "avatar routes do not accept query parameters")
		return false
	}
	return true
}

func parseAvatarHistoryOptions(w http.ResponseWriter, r *http.Request) (AvatarHistoryOptions, bool) {
	values := r.URL.Query()
	for key, entries := range values {
		if (key != "limit" && key != "before_version") || len(entries) != 1 {
			writeJSONError(w, http.StatusBadRequest, "invalid avatar history query")
			return AvatarHistoryOptions{}, false
		}
	}
	opts := AvatarHistoryOptions{Limit: defaultAvatarHistoryLimit}
	if entries, present := values["limit"]; present {
		raw := entries[0]
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxAvatarHistoryLimit {
			writeJSONError(w, http.StatusBadRequest, "invalid avatar history query")
			return AvatarHistoryOptions{}, false
		}
		opts.Limit = limit
	}
	if entries, present := values["before_version"]; present {
		raw := entries[0]
		before, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || before < 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid avatar history query")
			return AvatarHistoryOptions{}, false
		}
		opts.BeforeVersion = before
	}
	return opts, true
}

func writeAvatarError(w http.ResponseWriter, err error, operation string) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrBadInput), avatarDomainInputError(err):
		writeJSONError(w, http.StatusBadRequest, "invalid avatar request")
	case errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusForbidden, "avatar access forbidden")
	case errors.Is(err, ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "avatar resource not found")
	case errors.Is(err, ErrIdempotencyConflict):
		writeJSONError(w, http.StatusConflict, "idempotency key was reused for a different avatar mutation")
	case errors.Is(err, ErrAvatarPayloadQuotaExceeded):
		writeJSONError(w, http.StatusConflict, "avatar_payload_quota_exceeded")
	case errors.Is(err, ErrConflict):
		writeJSONError(w, http.StatusConflict, "avatar revision conflict")
	default:
		writeJSONError(w, http.StatusInternalServerError, "could not "+operation)
	}
	return true
}

func avatarDomainInputError(err error) bool {
	return errors.Is(err, avatar.ErrInvalidSubjectForm) ||
		errors.Is(err, avatar.ErrInvalidAutonomyPolicy) ||
		errors.Is(err, avatar.ErrInvalidStatus) ||
		errors.Is(err, avatar.ErrInvalidEventType) ||
		errors.Is(err, avatar.ErrInvalidPayloadState) ||
		errors.Is(err, avatar.ErrInvalidDescription) ||
		errors.Is(err, avatar.ErrInvalidSpecJSON) ||
		errors.Is(err, avatar.ErrInvalidStylePack) ||
		errors.Is(err, avatar.ErrInvalidSVG) ||
		errors.Is(err, avatar.ErrUnsafeSVG) ||
		errors.Is(err, avatar.ErrInvalidPlaceholderSeed)
}

func writeAvatarView(w http.ResponseWriter, status int, view AvatarView) {
	writeAvatarJSON(w, status, map[string]any{
		"schema_version": "witself.v0",
		"avatar":         view,
	})
}

func writeAvatarVersion(w http.ResponseWriter, status int, version AvatarVersion) {
	writeAvatarJSON(w, status, map[string]any{
		"schema_version": "witself.v0",
		"version":        version,
	})
}

func writeAvatarStyle(w http.ResponseWriter, status int, style AvatarStyleView) {
	normalizeAvatarStylePack(&style.StylePack)
	writeAvatarJSON(w, status, map[string]any{
		"schema_version": "witself.v0",
		"style":          style,
	})
}

func writeAvatarMutation(w http.ResponseWriter, status int, result AvatarMutationResult) {
	writeAvatarJSON(w, status, map[string]any{
		"schema_version": "witself.v0",
		"avatar":         result.Avatar,
		"receipt":        result.Receipt,
	})
}

func writeAvatarStyleMutation(w http.ResponseWriter, status int, result AvatarStyleMutationResult) {
	normalizeAvatarStylePack(&result.Style.StylePack)
	writeAvatarJSON(w, status, map[string]any{
		"schema_version": "witself.v0",
		"style":          result.Style,
		"receipt":        result.Receipt,
	})
}

func normalizeAvatarStylePack(pack *avatar.StylePack) {
	if pack.Palette == nil {
		pack.Palette = []avatar.ColorSpec{}
	}
	if pack.Layers == nil {
		pack.Layers = []avatar.LayerSpec{}
	}
	if pack.SupportedSubjectForms == nil {
		pack.SupportedSubjectForms = []avatar.SubjectForm{}
	}
	if pack.References == nil {
		pack.References = []avatar.StyleReference{}
	}
}

func writeAvatarJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func avatarNoStore(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		next(w, r)
	}
}

func avatarNoStoreMux(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if avatarRequestPath(r.URL.Path) {
			w.Header().Set("Cache-Control", "private, no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func avatarRequestPath(path string) bool {
	if strings.HasPrefix(path, "/v1/self/avatar") {
		return true
	}
	if strings.HasPrefix(path, "/v1/agents/") && strings.Contains(path, "/avatar") {
		return true
	}
	return strings.HasPrefix(path, "/v1/realms/") && strings.Contains(path, "/avatar-style")
}
