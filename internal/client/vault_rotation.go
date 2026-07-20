package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// VaultKeyRotationOpen is a rotation still accepting staged DEK wrappers.
	VaultKeyRotationOpen = "open"
	// VaultKeyRotationCommitted is a rotation whose target AVK is current.
	VaultKeyRotationCommitted = "committed"
	// VaultKeyRotationCancelled is a rotation retired without changing the current AVK.
	VaultKeyRotationCancelled = "cancelled"

	// VaultKeyRotationRecoveryArtifact records a verified external recovery copy.
	VaultKeyRotationRecoveryArtifact = "recovery_artifact"
	// VaultKeyRotationRiskAccepted records explicit acceptance of unrecoverable-key risk.
	VaultKeyRotationRiskAccepted = "risk_accepted"

	vaultKeyRotationWrappedDEKBytes = 60
)

// ErrInvalidVaultKeyRotationResponse reports a malformed or inconsistent server response.
var ErrInvalidVaultKeyRotationResponse = errors.New("vault key rotation response is invalid")

// VaultKeyRotation is value-free public progress for a client-driven AVK
// epoch change. StagedPlanHash is returned only after every replacement
// wrapper has been durably staged.
type VaultKeyRotation struct {
	ID                      string     `json:"id"`
	AccountID               string     `json:"account_id"`
	RealmID                 string     `json:"realm_id"`
	OwnerAgentID            string     `json:"owner_agent_id"`
	SourceKeyID             string     `json:"source_key_id"`
	SourceKeyVersion        int64      `json:"source_key_version"`
	SourceKeyAlgorithm      string     `json:"source_key_algorithm"`
	SourceKeyFingerprint    string     `json:"source_key_fingerprint"`
	TargetKeyID             string     `json:"target_key_id"`
	TargetKeyVersion        int64      `json:"target_key_version"`
	TargetKeyAlgorithm      string     `json:"target_key_algorithm"`
	TargetKeyFingerprint    string     `json:"target_key_fingerprint"`
	LifecycleState          string     `json:"lifecycle_state"`
	RecoveryDispositionMode string     `json:"recovery_disposition_mode,omitempty"`
	RecoveryArtifactSHA256  string     `json:"recovery_artifact_sha256,omitempty"`
	ItemCount               int64      `json:"item_count"`
	StagedCount             int64      `json:"staged_count"`
	RowVersion              int64      `json:"row_version"`
	StagedPlanHash          string     `json:"staged_plan_hash,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
	CommittedAt             *time.Time `json:"committed_at,omitempty"`
	CancelledAt             *time.Time `json:"cancelled_at,omitempty"`
}

// VaultKeyRotationItem carries one immutable source wrapper and its optional
// staged replacement. It never carries a DEK or field plaintext.
type VaultKeyRotationItem struct {
	RotationID               string     `json:"rotation_id"`
	SecretID                 string     `json:"secret_id"`
	FieldID                  string     `json:"field_id"`
	FieldKind                string     `json:"field_kind"`
	DEKID                    string     `json:"dek_id"`
	DEKGeneration            int64      `json:"dek_generation"`
	SourceDEKRowVersion      int64      `json:"source_dek_row_version"`
	SourceWrapRevision       int64      `json:"source_wrap_revision"`
	SourceWrappedDEK         []byte     `json:"source_wrapped_dek"`
	SourceWrapAlgorithm      string     `json:"source_wrap_algorithm"`
	SourceAADVersion         int64      `json:"source_aad_version"`
	SourceWrappingKeyID      string     `json:"source_wrapping_key_id"`
	SourceWrappingKeyVersion int64      `json:"source_wrapping_key_version"`
	TargetWrappingKeyID      string     `json:"target_wrapping_key_id"`
	TargetWrappingKeyVersion int64      `json:"target_wrapping_key_version"`
	TargetWrappedDEK         []byte     `json:"target_wrapped_dek,omitempty"`
	TargetWrapRevision       int64      `json:"target_wrap_revision,omitempty"`
	TargetWrapperSHA256      string     `json:"target_wrapper_sha256,omitempty"`
	StagedAt                 *time.Time `json:"staged_at,omitempty"`
}

// VaultKeyRotationReceipt is the value-free retry receipt for one rotation mutation.
type VaultKeyRotationReceipt struct {
	Operation      string    `json:"operation"`
	RequestHash    string    `json:"request_hash"`
	RotationID     string    `json:"rotation_id"`
	ResultRevision int64     `json:"result_revision"`
	Replayed       bool      `json:"replayed"`
	CreatedAt      time.Time `json:"created_at"`
}

// VaultKeyRotationMutationResult returns rotation state and its durable receipt.
type VaultKeyRotationMutationResult struct {
	Rotation VaultKeyRotation        `json:"rotation"`
	Receipt  VaultKeyRotationReceipt `json:"receipt"`
}

// VaultKeyRotationItemPage is one bounded page of immutable rotation items.
type VaultKeyRotationItemPage struct {
	Items      []VaultKeyRotationItem `json:"items"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

// StartVaultKeyRotationInput creates one source-fenced target AVK rotation.
type StartVaultKeyRotationInput struct {
	ID                          string `json:"id"`
	ExpectedSourceKeyID         string `json:"expected_source_key_id"`
	ExpectedSourceKeyVersion    int64  `json:"expected_source_key_version"`
	ExpectedSourceKeyRowVersion int64  `json:"expected_source_key_row_version"`
	TargetKeyID                 string `json:"target_key_id"`
	TargetKeyVersion            int64  `json:"target_key_version"`
	TargetAlgorithm             string `json:"target_algorithm"`
	TargetFingerprint           string `json:"target_fingerprint"`
	IdempotencyKey              string `json:"-"`
}

// VaultKeyRotationItemListOptions controls bounded rotation-item pagination.
type VaultKeyRotationItemListOptions struct {
	Limit  int
	Cursor string
}

// StageVaultKeyRotationItemInput supplies one target-AVK-wrapped DEK replacement.
type StageVaultKeyRotationItemInput struct {
	DEKID                       string `json:"dek_id"`
	ExpectedSourceDEKRowVersion int64  `json:"expected_source_dek_row_version"`
	ExpectedSourceWrapRevision  int64  `json:"expected_source_wrap_revision"`
	TargetWrappedDEK            []byte `json:"target_wrapped_dek"`
	TargetWrapRevision          int64  `json:"target_wrap_revision"`
}

// StageVaultKeyRotationInput atomically stages a bounded replacement batch.
type StageVaultKeyRotationInput struct {
	ExpectedRotationRowVersion int64                            `json:"expected_rotation_row_version"`
	Items                      []StageVaultKeyRotationItemInput `json:"items"`
	IdempotencyKey             string                           `json:"-"`
}

// CommitVaultKeyRotationInput verifies and atomically flips the completed plan.
type CommitVaultKeyRotationInput struct {
	ExpectedRotationRowVersion int64                               `json:"expected_rotation_row_version"`
	ExpectedItemCount          int64                               `json:"expected_item_count"`
	ExpectedPlanHash           string                              `json:"expected_plan_hash"`
	RecoveryDisposition        VaultKeyRotationRecoveryDisposition `json:"recovery_disposition"`
	IdempotencyKey             string                              `json:"-"`
}

// VaultKeyRotationRecoveryDisposition carries only the commit's durable
// recovery proof digest or its explicit risk acceptance. Recovery artifacts,
// locations, passphrases, and keys never cross this wire boundary.
type VaultKeyRotationRecoveryDisposition struct {
	Mode           string `json:"mode"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
}

// CancelVaultKeyRotationInput fences cancellation of an open rotation.
type CancelVaultKeyRotationInput struct {
	ExpectedRotationRowVersion int64  `json:"expected_rotation_row_version"`
	IdempotencyKey             string `json:"-"`
}

type vaultKeyRotationResponse struct {
	SchemaVersion string           `json:"schema_version"`
	Rotation      VaultKeyRotation `json:"rotation"`
}

type vaultKeyRotationOptionalResponse struct {
	SchemaVersion string            `json:"schema_version"`
	Rotation      *VaultKeyRotation `json:"rotation"`
}

type vaultKeyRotationMutationResponse struct {
	SchemaVersion string                  `json:"schema_version"`
	Rotation      VaultKeyRotation        `json:"rotation"`
	Receipt       VaultKeyRotationReceipt `json:"receipt"`
}

type vaultKeyRotationItemsResponse struct {
	SchemaVersion string                 `json:"schema_version"`
	Items         []VaultKeyRotationItem `json:"items"`
	NextCursor    string                 `json:"next_cursor,omitempty"`
}

func (out *vaultKeyRotationResponse) UnmarshalJSON(data []byte) error {
	type wire vaultKeyRotationResponse
	return unmarshalStrictVaultRotationJSON(data, (*wire)(out))
}

func (out *vaultKeyRotationOptionalResponse) UnmarshalJSON(data []byte) error {
	type wire vaultKeyRotationOptionalResponse
	return unmarshalStrictVaultRotationJSON(data, (*wire)(out))
}

func (out *vaultKeyRotationMutationResponse) UnmarshalJSON(data []byte) error {
	type wire vaultKeyRotationMutationResponse
	return unmarshalStrictVaultRotationJSON(data, (*wire)(out))
}

func (out *vaultKeyRotationItemsResponse) UnmarshalJSON(data []byte) error {
	type wire vaultKeyRotationItemsResponse
	return unmarshalStrictVaultRotationJSON(data, (*wire)(out))
}

// StartVaultKeyRotation creates one source-fenced target AVK rotation.
func StartVaultKeyRotation(ctx context.Context, endpoint, token string, in StartVaultKeyRotationInput) (*VaultKeyRotationMutationResult, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out vaultKeyRotationMutationResponse
	if err := doJSONWithHeaders(ctx, http.MethodPost, vaultKeyRotationsURL(endpoint), token,
		secretIdempotencyHeaders(in.IdempotencyKey), body, &out); err != nil {
		return nil, err
	}
	return validateVaultKeyRotationMutationResponse(&out)
}

// GetVaultKeyRotation returns one value-free rotation lifecycle record.
func GetVaultKeyRotation(ctx context.Context, endpoint, token, rotationID string) (*VaultKeyRotation, error) {
	var out vaultKeyRotationResponse
	if err := doJSON(ctx, http.MethodGet, vaultKeyRotationURL(endpoint, rotationID), token, nil, &out); err != nil {
		return nil, err
	}
	if out.SchemaVersion != "witself.v0" || validateVaultKeyRotation(&out.Rotation) != nil {
		return nil, ErrInvalidVaultKeyRotationResponse
	}
	return &out.Rotation, nil
}

// GetOpenVaultKeyRotation discovers the one crash-resumable open rotation for
// the authenticated agent. A nil result is the valid no-open-rotation state.
func GetOpenVaultKeyRotation(ctx context.Context, endpoint, token string) (*VaultKeyRotation, error) {
	var out vaultKeyRotationOptionalResponse
	if err := doJSON(ctx, http.MethodGet, vaultKeyRotationsURL(endpoint)+"/open", token, nil, &out); err != nil {
		return nil, err
	}
	if out.SchemaVersion != "witself.v0" {
		return nil, ErrInvalidVaultKeyRotationResponse
	}
	if out.Rotation == nil {
		return nil, nil
	}
	if validateVaultKeyRotation(out.Rotation) != nil || out.Rotation.LifecycleState != VaultKeyRotationOpen {
		return nil, ErrInvalidVaultKeyRotationResponse
	}
	return out.Rotation, nil
}

// ListVaultKeyRotationItems returns one bounded page of a rotation's DEK wrappers.
func ListVaultKeyRotationItems(ctx context.Context, endpoint, token, rotationID string, opts VaultKeyRotationItemListOptions) (*VaultKeyRotationItemPage, error) {
	query := url.Values{}
	if opts.Limit != 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		query.Set("cursor", opts.Cursor)
	}
	requestURL := vaultKeyRotationURL(endpoint, rotationID) + "/items"
	if encoded := query.Encode(); encoded != "" {
		requestURL += "?" + encoded
	}
	var out vaultKeyRotationItemsResponse
	if err := doJSON(ctx, http.MethodGet, requestURL, token, nil, &out); err != nil {
		return nil, err
	}
	if out.SchemaVersion != "witself.v0" {
		return nil, ErrInvalidVaultKeyRotationResponse
	}
	if out.Items == nil {
		out.Items = []VaultKeyRotationItem{}
	}
	for index := range out.Items {
		if validateVaultKeyRotationItem(&out.Items[index], rotationID) != nil {
			return nil, ErrInvalidVaultKeyRotationResponse
		}
	}
	return &VaultKeyRotationItemPage{Items: out.Items, NextCursor: out.NextCursor}, nil
}

// StageVaultKeyRotation persists one validated batch of target DEK wrappers.
func StageVaultKeyRotation(ctx context.Context, endpoint, token, rotationID string, in StageVaultKeyRotationInput) (*VaultKeyRotationMutationResult, error) {
	return mutateVaultKeyRotation(ctx, endpoint, token, rotationID, "stage", in.IdempotencyKey, in)
}

// CommitVaultKeyRotation atomically makes the verified target AVK current.
func CommitVaultKeyRotation(ctx context.Context, endpoint, token, rotationID string, in CommitVaultKeyRotationInput) (*VaultKeyRotationMutationResult, error) {
	return mutateVaultKeyRotation(ctx, endpoint, token, rotationID, "commit", in.IdempotencyKey, in)
}

// CancelVaultKeyRotation retires an open target AVK without changing the source.
func CancelVaultKeyRotation(ctx context.Context, endpoint, token, rotationID string, in CancelVaultKeyRotationInput) (*VaultKeyRotationMutationResult, error) {
	return mutateVaultKeyRotation(ctx, endpoint, token, rotationID, "cancel", in.IdempotencyKey, in)
}

func mutateVaultKeyRotation(ctx context.Context, endpoint, token, rotationID, action, idempotencyKey string, input any) (*VaultKeyRotationMutationResult, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var out vaultKeyRotationMutationResponse
	if err := doJSONWithHeaders(ctx, http.MethodPost, vaultKeyRotationURL(endpoint, rotationID)+":"+action, token,
		secretIdempotencyHeaders(idempotencyKey), body, &out); err != nil {
		return nil, err
	}
	return validateVaultKeyRotationMutationResponse(&out)
}

func validateVaultKeyRotationMutationResponse(out *vaultKeyRotationMutationResponse) (*VaultKeyRotationMutationResult, error) {
	if out == nil || out.SchemaVersion != "witself.v0" || validateVaultKeyRotation(&out.Rotation) != nil ||
		out.Receipt.Operation == "" || !validVaultRotationHash(out.Receipt.RequestHash) ||
		out.Receipt.RotationID != out.Rotation.ID || out.Receipt.ResultRevision < 1 ||
		out.Receipt.ResultRevision > out.Rotation.RowVersion || out.Receipt.CreatedAt.IsZero() {
		return nil, ErrInvalidVaultKeyRotationResponse
	}
	return &VaultKeyRotationMutationResult{Rotation: out.Rotation, Receipt: out.Receipt}, nil
}

func validateVaultKeyRotation(value *VaultKeyRotation) error {
	if value == nil || !validVaultRotationID(value.ID, "vkr") || value.AccountID == "" ||
		value.RealmID == "" || value.OwnerAgentID == "" || !validVaultRotationID(value.SourceKeyID, "avk") ||
		!validVaultRotationID(value.TargetKeyID, "avk") || value.SourceKeyID == value.TargetKeyID ||
		value.SourceKeyVersion < 1 || value.SourceKeyVersion == int64(^uint64(0)>>1) ||
		value.TargetKeyVersion != value.SourceKeyVersion+1 ||
		value.SourceKeyAlgorithm != VaultEnrollmentVaultKeyAlgorithm ||
		!validVaultRotationHash(value.SourceKeyFingerprint) ||
		value.TargetKeyAlgorithm != VaultEnrollmentVaultKeyAlgorithm ||
		!validVaultRotationHash(value.TargetKeyFingerprint) ||
		value.ItemCount < 0 || value.StagedCount < 0 || value.StagedCount > value.ItemCount ||
		value.RowVersion < 1 || value.CreatedAt.IsZero() || value.UpdatedAt.Before(value.CreatedAt) {
		return ErrInvalidVaultKeyRotationResponse
	}
	switch value.LifecycleState {
	case VaultKeyRotationOpen:
		if value.CommittedAt != nil || value.CancelledAt != nil ||
			value.RecoveryDispositionMode != "" || value.RecoveryArtifactSHA256 != "" ||
			(value.StagedCount == value.ItemCount) != (value.StagedPlanHash != "") ||
			value.StagedPlanHash != "" && !validVaultRotationHash(value.StagedPlanHash) {
			return ErrInvalidVaultKeyRotationResponse
		}
	case VaultKeyRotationCommitted:
		if value.CommittedAt == nil || value.CancelledAt != nil || value.StagedCount != value.ItemCount ||
			value.StagedPlanHash != "" || value.CommittedAt.Before(value.CreatedAt) || value.CommittedAt.After(value.UpdatedAt) {
			return ErrInvalidVaultKeyRotationResponse
		}
		if !validClientVaultKeyRotationRecoveryDisposition(value.RecoveryDispositionMode, value.RecoveryArtifactSHA256) {
			return ErrInvalidVaultKeyRotationResponse
		}
	case VaultKeyRotationCancelled:
		if value.CommittedAt != nil || value.CancelledAt == nil || value.StagedPlanHash != "" ||
			value.RecoveryDispositionMode != "" || value.RecoveryArtifactSHA256 != "" ||
			value.CancelledAt.Before(value.CreatedAt) || value.CancelledAt.After(value.UpdatedAt) {
			return ErrInvalidVaultKeyRotationResponse
		}
	default:
		return ErrInvalidVaultKeyRotationResponse
	}
	return nil
}

func validClientVaultKeyRotationRecoveryDisposition(mode, artifactSHA256 string) bool {
	switch mode {
	case VaultKeyRotationRecoveryArtifact:
		return validVaultRotationHash(artifactSHA256)
	case VaultKeyRotationRiskAccepted:
		return artifactSHA256 == ""
	default:
		return false
	}
}

func validateVaultKeyRotationItem(value *VaultKeyRotationItem, rotationID string) error {
	if value == nil || value.RotationID != rotationID || !validVaultRotationID(value.RotationID, "vkr") ||
		!validVaultRotationID(value.SecretID, "sec") || !validVaultRotationID(value.FieldID, "fld") ||
		!validVaultRotationID(value.DEKID, "dek") || value.FieldKind == "" || value.DEKGeneration < 1 ||
		value.SourceDEKRowVersion < 1 || value.SourceWrapRevision < 1 ||
		len(value.SourceWrappedDEK) != vaultKeyRotationWrappedDEKBytes ||
		value.SourceWrapAlgorithm != VaultEnrollmentVaultKeyAlgorithm || value.SourceAADVersion != 1 ||
		!validVaultRotationID(value.SourceWrappingKeyID, "avk") || value.SourceWrappingKeyVersion < 1 ||
		!validVaultRotationID(value.TargetWrappingKeyID, "avk") ||
		value.TargetWrappingKeyVersion <= value.SourceWrappingKeyVersion {
		return ErrInvalidVaultKeyRotationResponse
	}
	if value.StagedAt == nil {
		if len(value.TargetWrappedDEK) != 0 || value.TargetWrapRevision != 0 || value.TargetWrapperSHA256 != "" {
			return ErrInvalidVaultKeyRotationResponse
		}
	} else if len(value.TargetWrappedDEK) != vaultKeyRotationWrappedDEKBytes ||
		value.TargetWrapRevision != value.SourceWrapRevision+1 || !validVaultRotationHash(value.TargetWrapperSHA256) ||
		vaultRotationWrapperHash(value.TargetWrappedDEK) != value.TargetWrapperSHA256 {
		return ErrInvalidVaultKeyRotationResponse
	}
	return nil
}

func validVaultRotationID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix+"_") || len(value) != len(prefix)+1+16 {
		return false
	}
	for _, char := range value[len(prefix)+1:] {
		if (char < 'a' || char > 'z') && (char < '2' || char > '7') {
			return false
		}
	}
	return true
}

func validVaultRotationHash(value string) bool {
	return validVaultEnrollmentCommitment(value)
}

func vaultRotationWrapperHash(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func unmarshalStrictVaultRotationJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrInvalidVaultKeyRotationResponse
	}
	return nil
}

func vaultKeyRotationsURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/vault/rotations"
}

func vaultKeyRotationURL(endpoint, rotationID string) string {
	return vaultKeyRotationsURL(endpoint) + "/" + url.PathEscape(rotationID)
}
