package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// VaultEnrollmentStatePending is a newly requested transfer awaiting approval.
	VaultEnrollmentStatePending = "pending"
	// VaultEnrollmentStateApproved is an approved transfer awaiting consumption.
	VaultEnrollmentStateApproved = "approved"
	// VaultEnrollmentStateConsumed is a transfer installed by its intended recipient.
	VaultEnrollmentStateConsumed = "consumed"
	// VaultEnrollmentStateCancelled is a transfer cancelled before consumption.
	VaultEnrollmentStateCancelled = "cancelled"
	// VaultEnrollmentStateExpired is a transfer that passed its bounded lifetime.
	VaultEnrollmentStateExpired = "expired"

	// VaultEnrollmentVaultKeyAlgorithm identifies the enrolled AVK algorithm.
	VaultEnrollmentVaultKeyAlgorithm = "AES_256_GCM_RANDOM_NONCE_V1"
	// VaultEnrollmentTargetKeyAlgorithm identifies the recipient exchange-key format.
	VaultEnrollmentTargetKeyAlgorithm = "X25519_RAW_32_BASE64URL_V1"
	// VaultEnrollmentTransferAlgorithm identifies the recipient-bound transfer envelope.
	VaultEnrollmentTransferAlgorithm = "X25519_HKDF_SHA256_AES_256_GCM_V1"

	vaultEnrollmentPublicKeyBytes     = 32
	vaultEnrollmentCommitmentBytes    = 32
	vaultEnrollmentMinCiphertextBytes = 64
	vaultEnrollmentMaxCiphertextBytes = 4096
)

// ErrInvalidVaultEnrollmentResponse reports a malformed or inconsistent server response.
var ErrInvalidVaultEnrollmentResponse = errors.New("vault key enrollment response is invalid")

// VaultKeyEnrollment is the public, value-free lifecycle state for one
// short-lived AVK transfer. It contains target public-key material and public
// AVK identity only; transfer ciphertext is returned exclusively by Receive.
type VaultKeyEnrollment struct {
	ID                  string     `json:"id"`
	AccountID           string     `json:"account_id"`
	RealmID             string     `json:"realm_id"`
	OwnerAgentID        string     `json:"owner_agent_id"`
	VaultKeyID          string     `json:"vault_key_id"`
	VaultKeyVersion     int64      `json:"vault_key_version"`
	VaultKeyAlgorithm   string     `json:"vault_key_algorithm"`
	VaultKeyFingerprint string     `json:"vault_key_fingerprint"`
	TargetLocationID    string     `json:"target_location_id"`
	TargetLocationName  string     `json:"target_location_name,omitempty"`
	TargetPublicKey     string     `json:"target_public_key"`
	TargetKeyAlgorithm  string     `json:"target_key_algorithm"`
	PairingCommitment   string     `json:"pairing_commitment"`
	LifecycleState      string     `json:"lifecycle_state"`
	SourceLocationID    string     `json:"source_location_id,omitempty"`
	TransferAlgorithm   string     `json:"transfer_algorithm,omitempty"`
	RowVersion          int64      `json:"row_version"`
	CreatedAt           time.Time  `json:"created_at"`
	ExpiresAt           time.Time  `json:"expires_at"`
	ApprovedAt          *time.Time `json:"approved_at,omitempty"`
	ConsumedAt          *time.Time `json:"consumed_at,omitempty"`
	CancelledAt         *time.Time `json:"cancelled_at,omitempty"`
	ExpiredAt           *time.Time `json:"expired_at,omitempty"`
}

// VaultKeyEnrollmentTransfer is an opaque, recipient-bound ciphertext package.
// It deliberately has no AVK, plaintext, pairing-secret, or consume-proof field.
type VaultKeyEnrollmentTransfer struct {
	Enrollment               VaultKeyEnrollment `json:"enrollment"`
	SourceEphemeralPublicKey string             `json:"source_ephemeral_public_key"`
	Ciphertext               []byte             `json:"ciphertext"`
	ConsumeCommitment        string             `json:"consume_commitment"`
}

// CreateVaultKeyEnrollmentInput starts one recipient-bound enrollment ceremony.
type CreateVaultKeyEnrollmentInput struct {
	ID                 string    `json:"id"`
	TargetLocationID   string    `json:"target_location_id"`
	TargetLocationName string    `json:"target_location_name,omitempty"`
	TargetPublicKey    string    `json:"target_public_key"`
	TargetKeyAlgorithm string    `json:"target_key_algorithm"`
	PairingCommitment  string    `json:"pairing_commitment"`
	ExpiresAt          time.Time `json:"expires_at"`
	IdempotencyKey     string    `json:"-"`
}

// ApproveVaultKeyEnrollmentInput supplies the source-produced encrypted transfer.
type ApproveVaultKeyEnrollmentInput struct {
	ExpectedRowVersion       int64  `json:"expected_row_version"`
	SourceLocationID         string `json:"source_location_id"`
	SourceEphemeralPublicKey string `json:"source_ephemeral_public_key"`
	TransferCiphertext       []byte `json:"transfer_ciphertext"`
	TransferAlgorithm        string `json:"transfer_algorithm"`
	ConsumeCommitment        string `json:"consume_commitment"`
	IdempotencyKey           string `json:"-"`
}

// ReceiveVaultKeyEnrollmentInput identifies the intended recipient installation.
type ReceiveVaultKeyEnrollmentInput struct {
	TargetLocationID string `json:"target_location_id"`
}

// ConsumeVaultKeyEnrollmentInput proves successful recipient installation.
type ConsumeVaultKeyEnrollmentInput struct {
	ExpectedRowVersion int64  `json:"expected_row_version"`
	TargetLocationID   string `json:"target_location_id"`
	ConsumeProof       []byte `json:"consume_proof"`
	IdempotencyKey     string `json:"-"`
}

// CancelVaultKeyEnrollmentInput fences a pending enrollment cancellation.
type CancelVaultKeyEnrollmentInput struct {
	ExpectedRowVersion int64  `json:"expected_row_version"`
	IdempotencyKey     string `json:"-"`
}

// VaultKeyEnrollmentListOptions filters and bounds enrollment discovery.
type VaultKeyEnrollmentListOptions struct {
	State string
	Limit int
}

type vaultKeyEnrollmentResponse struct {
	SchemaVersion string             `json:"schema_version"`
	Enrollment    VaultKeyEnrollment `json:"enrollment"`
}

type vaultKeyEnrollmentListResponse struct {
	SchemaVersion string               `json:"schema_version"`
	Items         []VaultKeyEnrollment `json:"items"`
}

type vaultKeyEnrollmentTransferResponse struct {
	SchemaVersion string                     `json:"schema_version"`
	Transfer      VaultKeyEnrollmentTransfer `json:"transfer"`
}

func (out *vaultKeyEnrollmentResponse) UnmarshalJSON(data []byte) error {
	type wire vaultKeyEnrollmentResponse
	return unmarshalStrictVaultEnrollmentJSON(data, (*wire)(out))
}

func (out *vaultKeyEnrollmentListResponse) UnmarshalJSON(data []byte) error {
	type wire vaultKeyEnrollmentListResponse
	return unmarshalStrictVaultEnrollmentJSON(data, (*wire)(out))
}

func (out *vaultKeyEnrollmentTransferResponse) UnmarshalJSON(data []byte) error {
	type wire vaultKeyEnrollmentTransferResponse
	return unmarshalStrictVaultEnrollmentJSON(data, (*wire)(out))
}

// CreateVaultKeyEnrollment registers one target installation's public request.
// Target private-key and pairing-secret material have no representation here.
func CreateVaultKeyEnrollment(ctx context.Context, endpoint, token string, in CreateVaultKeyEnrollmentInput) (*VaultKeyEnrollment, error) {
	if !in.ExpiresAt.IsZero() {
		in.ExpiresAt = time.Unix(in.ExpiresAt.Unix(), 0).UTC()
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out vaultKeyEnrollmentResponse
	if err := doJSONWithHeaders(ctx, http.MethodPost, vaultKeyEnrollmentsURL(endpoint), token,
		secretIdempotencyHeaders(in.IdempotencyKey), body, &out); err != nil {
		return nil, err
	}
	if err := validateVaultKeyEnrollmentResponse(out.SchemaVersion, &out.Enrollment); err != nil {
		return nil, err
	}
	return &out.Enrollment, nil
}

// ListVaultKeyEnrollments returns bounded, value-free enrollment state.
func ListVaultKeyEnrollments(ctx context.Context, endpoint, token string, opts VaultKeyEnrollmentListOptions) ([]VaultKeyEnrollment, error) {
	query := neturl.Values{}
	if opts.State != "" {
		query.Set("state", opts.State)
	}
	if opts.Limit != 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	requestURL := vaultKeyEnrollmentsURL(endpoint)
	if encoded := query.Encode(); encoded != "" {
		requestURL += "?" + encoded
	}
	var out vaultKeyEnrollmentListResponse
	if err := doJSON(ctx, http.MethodGet, requestURL, token, nil, &out); err != nil {
		return nil, err
	}
	if out.SchemaVersion != "witself.v0" {
		return nil, ErrInvalidVaultEnrollmentResponse
	}
	if out.Items == nil {
		out.Items = []VaultKeyEnrollment{}
	}
	for index := range out.Items {
		if err := validateVaultKeyEnrollment(&out.Items[index]); err != nil {
			return nil, err
		}
	}
	return out.Items, nil
}

// GetVaultKeyEnrollment returns one value-free enrollment by id.
func GetVaultKeyEnrollment(ctx context.Context, endpoint, token, enrollmentID string) (*VaultKeyEnrollment, error) {
	var out vaultKeyEnrollmentResponse
	if err := doJSON(ctx, http.MethodGet, vaultKeyEnrollmentURL(endpoint, enrollmentID), token, nil, &out); err != nil {
		return nil, err
	}
	if err := validateVaultKeyEnrollmentResponse(out.SchemaVersion, &out.Enrollment); err != nil {
		return nil, err
	}
	return &out.Enrollment, nil
}

// ApproveVaultKeyEnrollment stores only an opaque recipient-bound transfer.
func ApproveVaultKeyEnrollment(ctx context.Context, endpoint, token, enrollmentID string, in ApproveVaultKeyEnrollmentInput) (*VaultKeyEnrollment, error) {
	return mutateVaultKeyEnrollment(ctx, endpoint, token, enrollmentID, "approve", in.IdempotencyKey, in)
}

// ReceiveVaultKeyEnrollment returns the approved opaque transfer to its target.
// This read is intentionally not an idempotent mutation and sends no retry key.
func ReceiveVaultKeyEnrollment(ctx context.Context, endpoint, token, enrollmentID string, in ReceiveVaultKeyEnrollmentInput) (*VaultKeyEnrollmentTransfer, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out vaultKeyEnrollmentTransferResponse
	if err := doJSON(ctx, http.MethodPost, vaultKeyEnrollmentActionURL(endpoint, enrollmentID, "receive"), token, body, &out); err != nil {
		return nil, err
	}
	if out.SchemaVersion != "witself.v0" || validateVaultKeyEnrollmentTransfer(&out.Transfer) != nil {
		return nil, ErrInvalidVaultEnrollmentResponse
	}
	return &out.Transfer, nil
}

// ConsumeVaultKeyEnrollment proves durable target receipt and purges the opaque
// transfer from live backend state.
func ConsumeVaultKeyEnrollment(ctx context.Context, endpoint, token, enrollmentID string, in ConsumeVaultKeyEnrollmentInput) (*VaultKeyEnrollment, error) {
	return mutateVaultKeyEnrollment(ctx, endpoint, token, enrollmentID, "consume", in.IdempotencyKey, in)
}

// CancelVaultKeyEnrollment terminates one pending or approved request.
func CancelVaultKeyEnrollment(ctx context.Context, endpoint, token, enrollmentID string, in CancelVaultKeyEnrollmentInput) (*VaultKeyEnrollment, error) {
	return mutateVaultKeyEnrollment(ctx, endpoint, token, enrollmentID, "cancel", in.IdempotencyKey, in)
}

func mutateVaultKeyEnrollment(ctx context.Context, endpoint, token, enrollmentID, action, idempotencyKey string, in any) (*VaultKeyEnrollment, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out vaultKeyEnrollmentResponse
	if err := doJSONWithHeaders(ctx, http.MethodPost, vaultKeyEnrollmentActionURL(endpoint, enrollmentID, action), token,
		secretIdempotencyHeaders(idempotencyKey), body, &out); err != nil {
		return nil, err
	}
	if err := validateVaultKeyEnrollmentResponse(out.SchemaVersion, &out.Enrollment); err != nil {
		return nil, err
	}
	return &out.Enrollment, nil
}

func validateVaultKeyEnrollmentResponse(schema string, enrollment *VaultKeyEnrollment) error {
	if schema != "witself.v0" {
		return ErrInvalidVaultEnrollmentResponse
	}
	return validateVaultKeyEnrollment(enrollment)
}

func validateVaultKeyEnrollment(value *VaultKeyEnrollment) error {
	if value == nil || value.ID == "" || value.AccountID == "" || value.RealmID == "" ||
		value.OwnerAgentID == "" || value.VaultKeyID == "" || value.VaultKeyVersion < 1 ||
		value.VaultKeyAlgorithm != VaultEnrollmentVaultKeyAlgorithm ||
		!validVaultEnrollmentCommitment(value.VaultKeyFingerprint) || value.TargetLocationID == "" ||
		!validVaultEnrollmentPublicKey(value.TargetPublicKey) ||
		value.TargetKeyAlgorithm != VaultEnrollmentTargetKeyAlgorithm ||
		!validVaultEnrollmentCommitment(value.PairingCommitment) || value.RowVersion < 1 ||
		value.CreatedAt.IsZero() || value.ExpiresAt.IsZero() || !value.ExpiresAt.After(value.CreatedAt) {
		return ErrInvalidVaultEnrollmentResponse
	}
	switch value.LifecycleState {
	case VaultEnrollmentStatePending:
		if value.SourceLocationID != "" || value.TransferAlgorithm != "" || value.ApprovedAt != nil ||
			value.ConsumedAt != nil || value.CancelledAt != nil || value.ExpiredAt != nil {
			return ErrInvalidVaultEnrollmentResponse
		}
	case VaultEnrollmentStateApproved:
		if value.SourceLocationID == "" || value.TransferAlgorithm != VaultEnrollmentTransferAlgorithm ||
			value.ApprovedAt == nil || value.ConsumedAt != nil || value.CancelledAt != nil || value.ExpiredAt != nil {
			return ErrInvalidVaultEnrollmentResponse
		}
	case VaultEnrollmentStateConsumed:
		if value.SourceLocationID == "" || value.TransferAlgorithm != "" || value.ApprovedAt == nil ||
			value.ConsumedAt == nil || value.CancelledAt != nil || value.ExpiredAt != nil {
			return ErrInvalidVaultEnrollmentResponse
		}
	case VaultEnrollmentStateCancelled:
		if value.TransferAlgorithm != "" || value.ConsumedAt != nil || value.CancelledAt == nil || value.ExpiredAt != nil {
			return ErrInvalidVaultEnrollmentResponse
		}
	case VaultEnrollmentStateExpired:
		if value.TransferAlgorithm != "" || value.ConsumedAt != nil || value.CancelledAt != nil || value.ExpiredAt == nil {
			return ErrInvalidVaultEnrollmentResponse
		}
	default:
		return ErrInvalidVaultEnrollmentResponse
	}
	return nil
}

func validateVaultKeyEnrollmentTransfer(value *VaultKeyEnrollmentTransfer) error {
	if value == nil || validateVaultKeyEnrollment(&value.Enrollment) != nil ||
		value.Enrollment.LifecycleState != VaultEnrollmentStateApproved ||
		!validVaultEnrollmentPublicKey(value.SourceEphemeralPublicKey) ||
		len(value.Ciphertext) < vaultEnrollmentMinCiphertextBytes ||
		len(value.Ciphertext) > vaultEnrollmentMaxCiphertextBytes ||
		!validVaultEnrollmentCommitment(value.ConsumeCommitment) {
		return ErrInvalidVaultEnrollmentResponse
	}
	return nil
}

func validVaultEnrollmentPublicKey(value string) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(vaultEnrollmentPublicKeyBytes) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != vaultEnrollmentPublicKeyBytes {
		return false
	}
	return base64.RawURLEncoding.EncodeToString(raw) == value
}

func validVaultEnrollmentCommitment(value string) bool {
	if len(value) != 2*vaultEnrollmentCommitmentBytes {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func unmarshalStrictVaultEnrollmentJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrInvalidVaultEnrollmentResponse
	}
	return nil
}

func vaultKeyEnrollmentsURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/vault/enrollments"
}

func vaultKeyEnrollmentURL(endpoint, enrollmentID string) string {
	return vaultKeyEnrollmentsURL(endpoint) + "/" + neturl.PathEscape(enrollmentID)
}

func vaultKeyEnrollmentActionURL(endpoint, enrollmentID, action string) string {
	return vaultKeyEnrollmentURL(endpoint, enrollmentID) + ":" + action
}
