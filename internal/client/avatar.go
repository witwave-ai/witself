package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/avatar"
)

const maxAvatarHistoryLimit = 100

// AvatarActor identifies the authenticated principal responsible for an
// avatar version or lifecycle mutation.
type AvatarActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// AvatarClientProvenance is self-reported generation provenance. The server
// stores it as metadata and never treats it as authentication input.
type AvatarClientProvenance struct {
	Runtime       string `json:"runtime,omitempty"`
	Model         string `json:"model,omitempty"`
	Recipe        string `json:"recipe,omitempty"`
	RecipeVersion string `json:"recipe_version,omitempty"`
}

// AvatarProfile is the mutable pointer and policy state for one agent's
// immutable avatar-version history.
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

// AvatarVersion is one immutable, sanitized avatar snapshot. SVG and visual
// specification content are returned only by explicit avatar reads, never by
// the value-free self checkpoint.
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
	RendererProfile         avatar.RendererProfile `json:"renderer_profile"`
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

// AvatarVersionSummary is the compact immutable metadata and lifecycle state
// returned by history reads. Fetch one exact version for creative payloads.
type AvatarVersionSummary struct {
	ID                      string                 `json:"id"`
	AccountID               string                 `json:"account_id"`
	RealmID                 string                 `json:"realm_id"`
	AgentID                 string                 `json:"agent_id"`
	Version                 int64                  `json:"version"`
	ParentVersion           *int64                 `json:"parent_version,omitempty"`
	LineageGeneration       int64                  `json:"lineage_generation"`
	SubjectForm             avatar.SubjectForm     `json:"subject_form"`
	SVGSHA256               string                 `json:"svg_sha256"`
	LockedLayersSHA256      string                 `json:"locked_layers_sha256"`
	RendererProfile         avatar.RendererProfile `json:"renderer_profile"`
	Style                   avatar.StylePackRef    `json:"style"`
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

// AvatarView is the exact profile plus its current active and pending version
// payloads. Proposed is absent when no proposal awaits a lifecycle decision.
type AvatarView struct {
	Profile  AvatarProfile  `json:"profile"`
	Active   *AvatarVersion `json:"active,omitempty"`
	Proposed *AvatarVersion `json:"proposed,omitempty"`
}

// AvatarHistoryPage is the immutable version history returned by the bounded
// initial history route.
type AvatarHistoryPage struct {
	SchemaVersion     string                 `json:"schema_version,omitempty"`
	Versions          []AvatarVersionSummary `json:"versions"`
	NextBeforeVersion int64                  `json:"next_before_version,omitempty"`
}

// AvatarHistoryOptions selects a bounded newest-first history page.
type AvatarHistoryOptions struct {
	Limit         int
	BeforeVersion int64
}

// AvatarStyleView is the realm's active immutable style-pack version and its
// optimistic-concurrency revision.
type AvatarStyleView struct {
	RealmID       string              `json:"realm_id"`
	StyleRevision int64               `json:"style_revision"`
	StylePack     avatar.StylePack    `json:"style_pack"`
	Rollout       *AvatarStyleRollout `json:"rollout,omitempty"`
	CreatedAt     time.Time           `json:"created_at"`
	UpdatedAt     time.Time           `json:"updated_at"`
}

// AvatarStyleRollout is durable, value-free profile propagation progress.
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

// AvatarMutationReceipt is a value-free, retry-safe lifecycle receipt.
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

// AvatarMutationResult contains the post-mutation view and value-free receipt.
type AvatarMutationResult struct {
	Avatar  AvatarView            `json:"avatar"`
	Receipt AvatarMutationReceipt `json:"receipt"`
}

// AvatarStyleMutationResult contains the active realm style and retry receipt.
type AvatarStyleMutationResult struct {
	Style   AvatarStyleView       `json:"style"`
	Receipt AvatarMutationReceipt `json:"receipt"`
}

// ProposeAvatarInput submits one client-generated SVG candidate. Account,
// realm, and agent identity are always derived from the bearer token.
type ProposeAvatarInput struct {
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

// ActivateAvatarInput advances the active pointer to one exact immutable
// version under an expected profile revision.
type ActivateAvatarInput struct {
	Version                 int64  `json:"version"`
	ExpectedProfileRevision int64  `json:"expected_profile_revision"`
	IdempotencyKey          string `json:"-"`
}

// RollbackAvatarInput moves the active pointer to an earlier exact version.
type RollbackAvatarInput struct {
	Version                 int64  `json:"version"`
	ExpectedProfileRevision int64  `json:"expected_profile_revision"`
	IdempotencyKey          string `json:"-"`
}

// ResetAvatarInput retires the current lineage without deleting immutable
// versions and returns the profile to generation-due state.
type ResetAvatarInput struct {
	ExpectedProfileRevision int64  `json:"expected_profile_revision"`
	ReasonCode              string `json:"reason_code,omitempty"`
	IdempotencyKey          string `json:"-"`
}

// RejectAvatarInput rejects one pending immutable proposal without exposing
// arbitrary operator prose in the lifecycle event stream.
type RejectAvatarInput struct {
	Version                 int64  `json:"version"`
	ExpectedProfileRevision int64  `json:"expected_profile_revision"`
	ReasonCode              string `json:"reason_code,omitempty"`
	IdempotencyKey          string `json:"-"`
}

// AvatarGenerationFailureInput records one bounded failed client generation
// attempt so a later session can honor server-controlled retry state.
type AvatarGenerationFailureInput struct {
	ExpectedProfileRevision int64  `json:"expected_profile_revision"`
	ReasonCode              string `json:"reason_code"`
	IdempotencyKey          string `json:"-"`
}

// UpdateAvatarPolicyInput changes activation authority under optimistic
// concurrency. The target agent is carried only in the operator route.
type UpdateAvatarPolicyInput struct {
	Policy                  avatar.AutonomyPolicy `json:"policy"`
	ExpectedProfileRevision int64                 `json:"expected_profile_revision"`
	IdempotencyKey          string                `json:"-"`
}

// UpdateAvatarQuotaInput changes one target agent's retained payload limits.
type UpdateAvatarQuotaInput struct {
	RetainedPayloadCountLimit int    `json:"retained_payload_count_limit"`
	RetainedPayloadByteLimit  int64  `json:"retained_payload_byte_limit"`
	ExpectedProfileRevision   int64  `json:"expected_profile_revision"`
	IdempotencyKey            string `json:"-"`
}

// CreateAvatarStyleVersionInput publishes and activates one immutable realm
// style-pack version under the current style revision.
type CreateAvatarStyleVersionInput struct {
	ExpectedStyleRevision int64            `json:"expected_style_revision"`
	StylePack             avatar.StylePack `json:"style_pack"`
	IdempotencyKey        string           `json:"-"`
}

// GetSelfAvatar returns the authenticated agent's exact avatar profile.
func GetSelfAvatar(ctx context.Context, endpoint, token string) (*AvatarView, error) {
	return getAvatar(ctx, selfAvatarURL(endpoint), token)
}

// GetSelfAvatarHistory returns the authenticated agent's immutable versions.
func GetSelfAvatarHistory(ctx context.Context, endpoint, token string) (*AvatarHistoryPage, error) {
	return GetSelfAvatarHistoryPage(ctx, endpoint, token, AvatarHistoryOptions{})
}

// GetSelfAvatarHistoryPage returns one cursor-bounded metadata history page.
func GetSelfAvatarHistoryPage(ctx context.Context, endpoint, token string, opts AvatarHistoryOptions) (*AvatarHistoryPage, error) {
	return getAvatarHistory(ctx, selfAvatarURL(endpoint)+"/history", token, opts)
}

func getAvatarHistory(ctx context.Context, route, token string, opts AvatarHistoryOptions) (*AvatarHistoryPage, error) {
	if opts.Limit < 0 || opts.Limit > maxAvatarHistoryLimit || opts.BeforeVersion < 0 {
		return nil, fmt.Errorf("invalid avatar history options")
	}
	query := url.Values{}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.BeforeVersion > 0 {
		query.Set("before_version", strconv.FormatInt(opts.BeforeVersion, 10))
	}
	if encoded := query.Encode(); encoded != "" {
		route += "?" + encoded
	}
	var out AvatarHistoryPage
	if err := doJSON(ctx, http.MethodGet, route, token, nil, &out); err != nil {
		return nil, err
	}
	if out.Versions == nil {
		out.Versions = []AvatarVersionSummary{}
	}
	for i := range out.Versions {
		normalizeAvatarVersionSummaryRendererProfile(&out.Versions[i])
	}
	return &out, nil
}

// GetSelfAvatarVersion returns one exact immutable creative payload for the
// authenticated agent.
func GetSelfAvatarVersion(ctx context.Context, endpoint, token string, version int64) (*AvatarVersion, error) {
	return getAvatarVersion(ctx, selfAvatarURL(endpoint), token, version)
}

// GetSelfAvatarStyle returns the full realm style needed by the active client
// to generate a consistent candidate.
func GetSelfAvatarStyle(ctx context.Context, endpoint, token string) (*AvatarStyleView, error) {
	return getAvatarStyle(ctx, selfAvatarURL(endpoint)+"/style", token)
}

// ProposeSelfAvatar stores one immutable candidate for the authenticated agent.
func ProposeSelfAvatar(ctx context.Context, endpoint, token string, in ProposeAvatarInput) (*AvatarMutationResult, error) {
	return mutateAvatar(ctx, http.MethodPost, selfAvatarURL(endpoint)+"/proposals", token, in.IdempotencyKey, in)
}

// ActivateSelfAvatar activates one proposal when the agent's autonomy policy
// permits self-activation.
func ActivateSelfAvatar(ctx context.Context, endpoint, token string, in ActivateAvatarInput) (*AvatarMutationResult, error) {
	return mutateAvatar(ctx, http.MethodPost, selfAvatarURL(endpoint)+":activate", token, in.IdempotencyKey, in)
}

// RollbackSelfAvatar reactivates an earlier version when policy permits it.
func RollbackSelfAvatar(ctx context.Context, endpoint, token string, in RollbackAvatarInput) (*AvatarMutationResult, error) {
	return mutateAvatar(ctx, http.MethodPost, selfAvatarURL(endpoint)+":rollback", token, in.IdempotencyKey, in)
}

// ResetSelfAvatar retires the authenticated agent's current lineage when its
// autonomy policy permits self-management. It does not purge avatar history.
func ResetSelfAvatar(ctx context.Context, endpoint, token string, in ResetAvatarInput) (*AvatarMutationResult, error) {
	return mutateAvatar(ctx, http.MethodPost, selfAvatarURL(endpoint)+":reset", token, in.IdempotencyKey, in)
}

// ReportSelfAvatarGenerationFailure records one bounded failed attempt while
// retaining the deterministic active placeholder.
func ReportSelfAvatarGenerationFailure(ctx context.Context, endpoint, token string, in AvatarGenerationFailureInput) (*AvatarMutationResult, error) {
	return mutateAvatar(ctx, http.MethodPost, selfAvatarURL(endpoint)+":generation-failed", token, in.IdempotencyKey, in)
}

// GetAgentAvatar returns an operator-authorized account agent's exact avatar.
func GetAgentAvatar(ctx context.Context, endpoint, token, agentID string) (*AvatarView, error) {
	return getAvatar(ctx, agentAvatarURL(endpoint, agentID), token)
}

// GetAgentAvatarHistory returns an operator-authorized account agent's
// immutable avatar versions.
func GetAgentAvatarHistory(ctx context.Context, endpoint, token, agentID string) (*AvatarHistoryPage, error) {
	return GetAgentAvatarHistoryPage(ctx, endpoint, token, agentID, AvatarHistoryOptions{})
}

// GetAgentAvatarHistoryPage returns one operator-authorized cursor-bounded
// metadata history page.
func GetAgentAvatarHistoryPage(ctx context.Context, endpoint, token, agentID string, opts AvatarHistoryOptions) (*AvatarHistoryPage, error) {
	return getAvatarHistory(ctx, agentAvatarURL(endpoint, agentID)+"/history", token, opts)
}

// GetAgentAvatarVersion returns one exact operator-authorized immutable
// creative payload for an account agent.
func GetAgentAvatarVersion(ctx context.Context, endpoint, token, agentID string, version int64) (*AvatarVersion, error) {
	return getAvatarVersion(ctx, agentAvatarURL(endpoint, agentID), token, version)
}

func getAvatarVersion(ctx context.Context, avatarRoute, token string, version int64) (*AvatarVersion, error) {
	if version < 1 {
		return nil, fmt.Errorf("avatar version must be positive")
	}
	var out struct {
		Version AvatarVersion `json:"version"`
	}
	if err := doJSON(ctx, http.MethodGet, avatarRoute+"/versions/"+strconv.FormatInt(version, 10), token, nil, &out); err != nil {
		return nil, err
	}
	normalizeAvatarVersionRendererProfile(&out.Version)
	return &out.Version, nil
}

// ProposeAgentAvatar stores one operator-authored immutable candidate for a
// target account agent. The target remains path-bound and is never copied into
// the JSON body.
func ProposeAgentAvatar(ctx context.Context, endpoint, token, agentID string, in ProposeAvatarInput) (*AvatarMutationResult, error) {
	return mutateAvatar(ctx, http.MethodPost, agentAvatarURL(endpoint, agentID)+"/proposals", token, in.IdempotencyKey, in)
}

// ActivateAgentAvatar activates one exact version through the operator surface.
func ActivateAgentAvatar(ctx context.Context, endpoint, token, agentID string, in ActivateAvatarInput) (*AvatarMutationResult, error) {
	return mutateAvatar(ctx, http.MethodPost, agentAvatarURL(endpoint, agentID)+":activate", token, in.IdempotencyKey, in)
}

// RejectAgentAvatar rejects one pending proposal through the operator surface.
func RejectAgentAvatar(ctx context.Context, endpoint, token, agentID string, in RejectAvatarInput) (*AvatarMutationResult, error) {
	return mutateAvatar(ctx, http.MethodPost, agentAvatarURL(endpoint, agentID)+":reject", token, in.IdempotencyKey, in)
}

// RollbackAgentAvatar reactivates an earlier version through the operator surface.
func RollbackAgentAvatar(ctx context.Context, endpoint, token, agentID string, in RollbackAvatarInput) (*AvatarMutationResult, error) {
	return mutateAvatar(ctx, http.MethodPost, agentAvatarURL(endpoint, agentID)+":rollback", token, in.IdempotencyKey, in)
}

// ResetAgentAvatar retires a target agent's current lineage through the
// operator surface without purging immutable avatar history.
func ResetAgentAvatar(ctx context.Context, endpoint, token, agentID string, in ResetAvatarInput) (*AvatarMutationResult, error) {
	return mutateAvatar(ctx, http.MethodPost, agentAvatarURL(endpoint, agentID)+":reset", token, in.IdempotencyKey, in)
}

// UpdateAgentAvatarPolicy changes one target agent's activation authority.
func UpdateAgentAvatarPolicy(ctx context.Context, endpoint, token, agentID string, in UpdateAvatarPolicyInput) (*AvatarMutationResult, error) {
	return mutateAvatar(ctx, http.MethodPatch, agentAvatarURL(endpoint, agentID)+"-policy", token, in.IdempotencyKey, in)
}

// UpdateAgentAvatarQuota changes one target agent's retained creative-payload limits.
func UpdateAgentAvatarQuota(ctx context.Context, endpoint, token, agentID string, in UpdateAvatarQuotaInput) (*AvatarMutationResult, error) {
	return mutateAvatar(ctx, http.MethodPatch, agentAvatarURL(endpoint, agentID)+"-quota", token, in.IdempotencyKey, in)
}

// GetRealmAvatarStyle returns the operator-authorized realm style.
func GetRealmAvatarStyle(ctx context.Context, endpoint, token, realmID string) (*AvatarStyleView, error) {
	return getAvatarStyle(ctx, realmAvatarStyleURL(endpoint, realmID), token)
}

// CreateRealmAvatarStyleVersion publishes and activates a realm style version.
func CreateRealmAvatarStyleVersion(ctx context.Context, endpoint, token, realmID string, in CreateAvatarStyleVersionInput) (*AvatarStyleMutationResult, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out AvatarStyleMutationResult
	if err := doJSONWithHeaders(ctx, http.MethodPost, realmAvatarStyleURL(endpoint, realmID)+"/versions", token,
		avatarIdempotencyHeaders(in.IdempotencyKey), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func getAvatar(ctx context.Context, requestURL, token string) (*AvatarView, error) {
	var out struct {
		Avatar AvatarView `json:"avatar"`
	}
	if err := doJSON(ctx, http.MethodGet, requestURL, token, nil, &out); err != nil {
		return nil, err
	}
	normalizeAvatarViewRendererProfiles(&out.Avatar)
	return &out.Avatar, nil
}

func getAvatarStyle(ctx context.Context, requestURL, token string) (*AvatarStyleView, error) {
	var out struct {
		Style AvatarStyleView `json:"style"`
	}
	if err := doJSON(ctx, http.MethodGet, requestURL, token, nil, &out); err != nil {
		return nil, err
	}
	return &out.Style, nil
}

func mutateAvatar(ctx context.Context, method, requestURL, token, idempotencyKey string, in any) (*AvatarMutationResult, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out AvatarMutationResult
	if err := doJSONWithHeaders(ctx, method, requestURL, token,
		avatarIdempotencyHeaders(idempotencyKey), body, &out); err != nil {
		return nil, err
	}
	normalizeAvatarViewRendererProfiles(&out.Avatar)
	return &out, nil
}

// normalizeAvatarViewRendererProfiles is a rollout-only read compatibility
// seam. A v186 client can briefly receive avatar JSON from a still-running
// v185 server that did not emit renderer_profile. Empty means legacy on the
// wire only; this helper never upgrades or persists renderer provenance.
func normalizeAvatarViewRendererProfiles(view *AvatarView) {
	if view == nil {
		return
	}
	normalizeAvatarVersionRendererProfile(view.Active)
	normalizeAvatarVersionRendererProfile(view.Proposed)
}

func normalizeAvatarVersionRendererProfile(version *AvatarVersion) {
	if version != nil && version.RendererProfile == "" {
		version.RendererProfile = avatar.RendererProfileLegacy
	}
}

func normalizeAvatarVersionSummaryRendererProfile(version *AvatarVersionSummary) {
	if version != nil && version.RendererProfile == "" {
		version.RendererProfile = avatar.RendererProfileLegacy
	}
}

func avatarIdempotencyHeaders(key string) map[string]string {
	if strings.TrimSpace(key) == "" {
		return nil
	}
	return map[string]string{"Idempotency-Key": key}
}

func selfAvatarURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/self/avatar"
}

func agentAvatarURL(endpoint, agentID string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/agents/" + url.PathEscape(agentID) + "/avatar"
}

func realmAvatarStyleURL(endpoint, realmID string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/realms/" + url.PathEscape(realmID) + "/avatar-style"
}
