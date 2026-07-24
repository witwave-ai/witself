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

// VaultKeyBinding is the backend's public identity for one client-held AVK
// epoch. Private key bytes never have an HTTP client representation.
type VaultKeyBinding struct {
	ID             string     `json:"id"`
	AccountID      string     `json:"account_id"`
	RealmID        string     `json:"realm_id"`
	OwnerAgentID   string     `json:"owner_agent_id"`
	KeyVersion     int64      `json:"key_version"`
	Algorithm      string     `json:"algorithm"`
	Fingerprint    string     `json:"fingerprint"`
	LifecycleState string     `json:"lifecycle_state"`
	RowVersion     int64      `json:"row_version"`
	CreatedAt      time.Time  `json:"created_at"`
	RetiredAt      *time.Time `json:"retired_at,omitempty"`
}

// RegisterVaultKeyInput carries public AVK metadata and the retry key used for
// the Idempotency-Key header. It never contains AVK bytes.
type RegisterVaultKeyInput struct {
	ID             string `json:"id"`
	KeyVersion     int64  `json:"key_version"`
	Algorithm      string `json:"algorithm"`
	Fingerprint    string `json:"fingerprint"`
	IdempotencyKey string `json:"-"`
}

// Secret is the redacted client projection of one structured secret.
// Sensitive plaintext and encrypted material are excluded from this shape.
type Secret struct {
	ID             string        `json:"id"`
	AccountID      string        `json:"account_id"`
	RealmID        string        `json:"realm_id"`
	OwnerAgentID   string        `json:"owner_agent_id"`
	Name           string        `json:"name"`
	Description    string        `json:"description,omitempty"`
	Template       string        `json:"template"`
	Tags           []string      `json:"tags"`
	Fields         []SecretField `json:"fields,omitempty"`
	Lifecycle      string        `json:"lifecycle"`
	RowVersion     int64         `json:"row_version"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
	ArchivedAt     *time.Time    `json:"archived_at,omitempty"`
	DeletedAt      *time.Time    `json:"deleted_at,omitempty"`
	SensitiveCount int           `json:"sensitive_field_count"`
}

// SecretField is one redacted field projection. PublicValue is populated only
// for a field explicitly stored as non-sensitive.
type SecretField struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	Sensitive     bool    `json:"sensitive"`
	Encoding      string  `json:"encoding"`
	ValueVersion  int64   `json:"value_version"`
	PublicValue   *string `json:"public_value,omitempty"`
	Redacted      bool    `json:"redacted"`
	RowVersion    int64   `json:"row_version"`
	DEKGeneration int64   `json:"dek_generation,omitempty"`
}

// SealedDEK is the wire representation of a field DEK wrapped locally by the
// client-held AVK.
type SealedDEK struct {
	ID                 string `json:"id"`
	Generation         int64  `json:"generation"`
	WrappedDEK         []byte `json:"wrapped_dek"`
	WrapAlgorithm      string `json:"wrap_algorithm"`
	AADVersion         int64  `json:"aad_version"`
	WrapRevision       int64  `json:"wrap_revision"`
	WrappingKeyID      string `json:"wrapping_key_id"`
	WrappingKeyVersion int64  `json:"wrapping_key_version"`
}

// SealedField carries client-produced ciphertext and its wrapped DEK. It has no
// plaintext value representation.
type SealedField struct {
	EnvelopeVersion int64     `json:"envelope_version"`
	Ciphertext      []byte    `json:"ciphertext"`
	Algorithm       string    `json:"algorithm"`
	AADVersion      int64     `json:"aad_version"`
	DEK             SealedDEK `json:"dek"`
}

// CreateSecretFieldInput is one public or already-sealed field in a create
// request. Exactly one of PublicValue and Sealed is valid for each field.
type CreateSecretFieldInput struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Kind         string       `json:"kind"`
	Sensitive    bool         `json:"sensitive"`
	Encoding     string       `json:"encoding"`
	ValueVersion int64        `json:"value_version"`
	PublicValue  *string      `json:"public_value,omitempty"`
	Sealed       *SealedField `json:"sealed,omitempty"`
}

// CreateSecretInput is a complete client-generated secret create request. Its
// sensitive fields are sealed before this value reaches the HTTP client.
type CreateSecretInput struct {
	ID             string                   `json:"id"`
	Name           string                   `json:"name"`
	Description    string                   `json:"description,omitempty"`
	Template       string                   `json:"template"`
	Tags           []string                 `json:"tags,omitempty"`
	Fields         []CreateSecretFieldInput `json:"fields"`
	IdempotencyKey string                   `json:"-"`
}

// SecretLifecycleInput carries the optimistic row-version fence and the retry
// key used for the Idempotency-Key header.
type SecretLifecycleInput struct {
	ExpectedRowVersion int64  `json:"expected_row_version"`
	IdempotencyKey     string `json:"-"`
}

// SecretMaterial carries exactly one encrypted field and the wrapped DEK
// needed for local decryption. It deliberately has no plaintext value field.
type SecretMaterial struct {
	SecretID        string    `json:"secret_id"`
	FieldID         string    `json:"field_id"`
	FieldName       string    `json:"field_name"`
	FieldKind       string    `json:"field_kind"`
	Encoding        string    `json:"encoding"`
	ValueVersion    int64     `json:"value_version"`
	EnvelopeVersion int64     `json:"envelope_version"`
	Ciphertext      []byte    `json:"ciphertext"`
	Algorithm       string    `json:"algorithm"`
	AADVersion      int64     `json:"aad_version"`
	DEK             SealedDEK `json:"dek"`
	SecretRevision  int64     `json:"secret_revision"`
	FieldRevision   int64     `json:"field_revision"`
}

// SecretMutationReceipt is the value-free receipt for a secret or vault-key
// mutation.
type SecretMutationReceipt struct {
	Operation          string    `json:"operation"`
	RequestHash        string    `json:"request_hash"`
	TargetKind         string    `json:"target_kind"`
	TargetID           string    `json:"target_id"`
	ResultRevision     int64     `json:"result_revision"`
	ResultValueVersion int64     `json:"result_value_version,omitempty"`
	Replayed           bool      `json:"replayed"`
	CreatedAt          time.Time `json:"created_at"`
}

// SecretMutationResult returns the redacted secret and its mutation receipt.
type SecretMutationResult struct {
	Secret  Secret                `json:"secret"`
	Receipt SecretMutationReceipt `json:"receipt"`
}

// SecretLimitStatus is the authenticated owner's retained-secret capacity.
// Null max/remaining means the applied plan snapshot is unlimited.
type SecretLimitStatus struct {
	Used      int64  `json:"used"`
	Max       *int64 `json:"max"`
	Remaining *int64 `json:"remaining"`
	Unlimited bool   `json:"unlimited"`
	OverLimit bool   `json:"over_limit"`
}

// VaultKeyMutationResult returns public key-epoch metadata and its mutation
// receipt.
type VaultKeyMutationResult struct {
	KeyEpoch VaultKeyBinding       `json:"key_epoch"`
	Receipt  SecretMutationReceipt `json:"receipt"`
}

// SecretPage is one cursor-paged collection of redacted secrets.
type SecretPage struct {
	Items      []Secret `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// SecretListOptions controls public metadata filtering, pagination, and whether
// redacted field projections are included.
type SecretListOptions struct {
	Query         string
	Lifecycle     string
	Template      string
	Tags          []string
	Limit         int
	Cursor        string
	IncludeFields bool
}

// GetCurrentVaultKey reads public key-epoch identity. A nil result is the
// valid state where this agent has not registered an AVK yet.
func GetCurrentVaultKey(ctx context.Context, endpoint, token string) (*VaultKeyBinding, error) {
	var out struct {
		KeyEpoch *VaultKeyBinding `json:"key_epoch"`
	}
	if err := doJSON(ctx, http.MethodGet, vaultKeyEpochsURL(endpoint)+"/current", token, nil, &out); err != nil {
		return nil, err
	}
	return out.KeyEpoch, nil
}

// RegisterVaultKey registers public AVK metadata. The AVK itself never enters
// this input or any HTTP request.
func RegisterVaultKey(ctx context.Context, endpoint, token string, in RegisterVaultKeyInput) (*VaultKeyMutationResult, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out VaultKeyMutationResult
	if err := doJSONWithHeaders(ctx, http.MethodPost, vaultKeyEpochsURL(endpoint), token,
		secretIdempotencyHeaders(in.IdempotencyKey), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateSecret sends public metadata and already-encrypted field envelopes.
func CreateSecret(ctx context.Context, endpoint, token string, in CreateSecretInput) (*SecretMutationResult, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out SecretMutationResult
	if err := doJSONWithHeaders(ctx, http.MethodPost, secretsURL(endpoint), token,
		secretIdempotencyHeaders(in.IdempotencyKey), body, &out); err != nil {
		return nil, err
	}
	redactClientSecret(&out.Secret)
	return &out, nil
}

// GetSecretLimitStatus returns value-free retained-secret capacity.
func GetSecretLimitStatus(ctx context.Context, endpoint, token string) (*SecretLimitStatus, error) {
	var out struct {
		Limit SecretLimitStatus `json:"limit"`
	}
	if err := doJSON(ctx, http.MethodGet, secretsURL(endpoint)+":status", token, nil, &out); err != nil {
		return nil, err
	}
	return &out.Limit, nil
}

// ListSecrets searches public metadata and explicitly non-sensitive values.
func ListSecrets(ctx context.Context, endpoint, token string, opts SecretListOptions) (*SecretPage, error) {
	values := url.Values{}
	if opts.Query != "" {
		values.Set("q", opts.Query)
	}
	if opts.Lifecycle != "" {
		values.Set("lifecycle", opts.Lifecycle)
	}
	if opts.Template != "" {
		values.Set("template", opts.Template)
	}
	for _, tag := range opts.Tags {
		values.Add("tag", tag)
	}
	if opts.Limit != 0 {
		values.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		values.Set("cursor", opts.Cursor)
	}
	if opts.IncludeFields {
		values.Set("include_fields", "true")
	}
	requestURL := secretsURL(endpoint)
	if encoded := values.Encode(); encoded != "" {
		requestURL += "?" + encoded
	}
	var out SecretPage
	if err := doJSON(ctx, http.MethodGet, requestURL, token, nil, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []Secret{}
	}
	for index := range out.Items {
		redactClientSecret(&out.Items[index])
	}
	return &out, nil
}

// GetSecret returns redacted detail and explicitly public field values.
func GetSecret(ctx context.Context, endpoint, token, secretID string) (*Secret, error) {
	var out struct {
		Secret Secret `json:"secret"`
	}
	if err := doJSON(ctx, http.MethodGet, secretsURL(endpoint)+"/"+url.PathEscape(secretID), token, nil, &out); err != nil {
		return nil, err
	}
	redactClientSecret(&out.Secret)
	return &out.Secret, nil
}

// ArchiveSecret reversibly removes a secret from ordinary list, show, and
// material-access paths using an optimistic revision fence.
func ArchiveSecret(ctx context.Context, endpoint, token, secretID string, in SecretLifecycleInput) (*SecretMutationResult, error) {
	return mutateSecretLifecycle(ctx, endpoint, token, secretID, "archive", in)
}

// RestoreSecret returns an archived secret to ordinary reads when its live
// name is still available.
func RestoreSecret(ctx context.Context, endpoint, token, secretID string, in SecretLifecycleInput) (*SecretMutationResult, error) {
	return mutateSecretLifecycle(ctx, endpoint, token, secretID, "restore", in)
}

// DeleteSecret tombstones an active or archived secret and releases retained
// capacity while preserving the value-free mutation receipt.
func DeleteSecret(ctx context.Context, endpoint, token, secretID string, in SecretLifecycleInput) (*SecretMutationResult, error) {
	return mutateSecretLifecycle(ctx, endpoint, token, secretID, "delete", in)
}

func mutateSecretLifecycle(ctx context.Context, endpoint, token, secretID, operation string, in SecretLifecycleInput) (*SecretMutationResult, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	requestURL := secretsURL(endpoint) + "/" + url.PathEscape(secretID) + ":" + operation
	var out SecretMutationResult
	if err := doJSONWithHeaders(ctx, http.MethodPost, requestURL, token,
		secretIdempotencyHeaders(in.IdempotencyKey), body, &out); err != nil {
		return nil, err
	}
	redactClientSecret(&out.Secret)
	return &out, nil
}

// AccessSecretField retrieves one encrypted field package for local unwrap and
// decryption. The idempotency key fences its usage/audit delivery.
func AccessSecretField(ctx context.Context, endpoint, token, secretID, fieldID, idempotencyKey string) (*SecretMaterial, error) {
	var out struct {
		Material SecretMaterial `json:"material"`
	}
	requestURL := secretsURL(endpoint) + "/" + url.PathEscape(secretID) +
		"/fields/" + url.PathEscape(fieldID) + ":access"
	if err := doJSONWithHeaders(ctx, http.MethodPost, requestURL, token,
		secretIdempotencyHeaders(idempotencyKey), nil, &out); err != nil {
		return nil, err
	}
	return &out.Material, nil
}

func redactClientSecret(value *Secret) {
	if value == nil {
		return
	}
	if value.Tags == nil {
		value.Tags = []string{}
	}
	for index := range value.Fields {
		if value.Fields[index].Sensitive {
			value.Fields[index].PublicValue = nil
			value.Fields[index].Redacted = true
		}
	}
}

func secretIdempotencyHeaders(key string) map[string]string {
	if strings.TrimSpace(key) == "" {
		return nil
	}
	return map[string]string{"Idempotency-Key": key}
}

func vaultKeyEpochsURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/vault/key-epochs"
}

func secretsURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/secrets"
}
