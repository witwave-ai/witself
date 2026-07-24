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
)

const (
	maxVaultKeyRequestBytes int64 = 8 * 1024
	// A store mutation may contain up to 1 MiB of encrypted material. JSON's
	// base64 expansion plus field metadata must still fit at the HTTP boundary.
	maxSecretCreateRequestBytes    int64 = 2 * 1024 * 1024
	maxSecretAccessRequestBytes    int64 = 1024
	maxSecretLifecycleRequestBytes int64 = 1024
	maxSecretIdempotencyKeyBytes         = 512
	maxSecretListLimit                   = 100
)

var secretResourceIDPattern = regexp.MustCompile(`^(?:avk|sec|fld|dek|enr|vkr)_[a-z2-7]{16}$`)

// ErrSecretVaultKeyUnavailable and ErrSecretVaultKeyMismatch are stable
// transport sentinels for the fail-closed client-custody bootstrap state. They
// never carry key material.
var (
	ErrSecretVaultKeyUnavailable = errors.New("agent vault key unavailable")
	ErrSecretVaultKeyMismatch    = errors.New("agent vault key mismatch")
	ErrSecretLimitReached        = errors.New("stored secret limit reached")
)

// VaultKeyBinding is public AVK identity metadata. A private AVK has no server
// request or response representation.
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

// RegisterVaultKeyRequest registers public AVK identity after the client has
// resolved its local/server bootstrap state. IdempotencyKey travels only in the
// Idempotency-Key header.
type RegisterVaultKeyRequest struct {
	ID             string `json:"id"`
	KeyVersion     int64  `json:"key_version"`
	Algorithm      string `json:"algorithm"`
	Fingerprint    string `json:"fingerprint"`
	IdempotencyKey string `json:"-"`
}

// Secret is the redacted structured-secret projection. Ciphertext is not part
// of ordinary list/show shapes and sensitive plaintext cannot fit this type.
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

// SecretField exposes a public value only for an explicitly non-sensitive
// field. Sensitive fields are always forced to redacted at the HTTP boundary.
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

// SealedDEK is the server wire representation of a field DEK already wrapped
// by the client-held AVK.
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

// SealedField carries client-produced ciphertext and its wrapped DEK. The
// server stores and returns this material without decrypting it.
type SealedField struct {
	EnvelopeVersion int64     `json:"envelope_version"`
	Ciphertext      []byte    `json:"ciphertext"`
	Algorithm       string    `json:"algorithm"`
	AADVersion      int64     `json:"aad_version"`
	DEK             SealedDEK `json:"dek"`
}

// CreateSecretFieldRequest is one public or already-sealed field in a create
// request. Exactly one of PublicValue and Sealed is valid for each field.
type CreateSecretFieldRequest struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Kind         string       `json:"kind"`
	Sensitive    bool         `json:"sensitive"`
	Encoding     string       `json:"encoding"`
	ValueVersion int64        `json:"value_version"`
	PublicValue  *string      `json:"public_value,omitempty"`
	Sealed       *SealedField `json:"sealed,omitempty"`
}

// CreateSecretRequest is already sealed before it reaches the backend. Owner,
// account, and realm identity are always derived from the bearer token.
type CreateSecretRequest struct {
	ID             string                     `json:"id"`
	Name           string                     `json:"name"`
	Description    string                     `json:"description,omitempty"`
	Template       string                     `json:"template"`
	Tags           []string                   `json:"tags,omitempty"`
	Fields         []CreateSecretFieldRequest `json:"fields"`
	IdempotencyKey string                     `json:"-"`
}

// SecretMaterial is the exact encrypted package for one sensitive field.
// Decryption remains local to the authenticated agent's client.
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

// AccessSecretFieldRequest carries the retry key used for the
// Idempotency-Key header when requesting one encrypted field package.
type AccessSecretFieldRequest struct {
	IdempotencyKey string `json:"-"`
}

// SecretLifecycleRequest carries only the optimistic revision fence. The raw
// retry key remains in the Idempotency-Key header.
type SecretLifecycleRequest struct {
	ExpectedRowVersion int64  `json:"expected_row_version"`
	IdempotencyKey     string `json:"-"`
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

// SecretLimitStatus is the value-free retained-secret capacity for the
// authenticated owner. Null max/remaining means unlimited.
type SecretLimitStatus struct {
	Used      int64  `json:"used"`
	Max       *int64 `json:"max"`
	Remaining *int64 `json:"remaining"`
	Unlimited bool   `json:"unlimited"`
	OverLimit bool   `json:"over_limit"`
}

// SecretLimitError carries a safe status snapshot for a non-retryable create
// refusal.
type SecretLimitError struct {
	Status SecretLimitStatus
}

func (e *SecretLimitError) Error() string { return ErrSecretLimitReached.Error() }
func (e *SecretLimitError) Unwrap() error { return ErrSecretLimitReached }

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

func registerVaultKeyHandler(auth PrincipalAuthFunc, register func(context.Context, DomainPrincipal, RegisterVaultKeyRequest) (VaultKeyMutationResult, error)) http.HandlerFunc {
	return secretAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		var in RegisterVaultKeyRequest
		if !decodeSecretRequest(w, r, &in, maxVaultKeyRequestBytes) || !setSecretIdempotencyKey(w, r, &in.IdempotencyKey) {
			return
		}
		result, err := register(r.Context(), p, in)
		if writeSecretError(w, err, "register vault key") {
			return
		}
		writeSecretJSON(w, http.StatusCreated, map[string]any{
			"schema_version": "witself.v0", "key_epoch": result.KeyEpoch, "receipt": result.Receipt,
		})
	})
}

func getCurrentVaultKeyHandler(auth PrincipalAuthFunc, get func(context.Context, DomainPrincipal) (*VaultKeyBinding, error)) http.HandlerFunc {
	return secretAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		key, err := get(r.Context(), p)
		if writeSecretError(w, err, "read vault key") {
			return
		}
		writeSecretJSON(w, http.StatusOK, map[string]any{"schema_version": "witself.v0", "key_epoch": key})
	})
}

func createSecretHandler(auth PrincipalAuthFunc, create func(context.Context, DomainPrincipal, CreateSecretRequest) (SecretMutationResult, error)) http.HandlerFunc {
	return secretAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		var in CreateSecretRequest
		if !decodeSecretRequest(w, r, &in, maxSecretCreateRequestBytes) || !setSecretIdempotencyKey(w, r, &in.IdempotencyKey) {
			return
		}
		result, err := create(r.Context(), p, in)
		if writeSecretError(w, err, "create secret") {
			return
		}
		redactSecret(&result.Secret)
		writeSecretJSON(w, http.StatusCreated, map[string]any{
			"schema_version": "witself.v0", "secret": result.Secret, "receipt": result.Receipt,
		})
	})
}

func listSecretsHandler(auth PrincipalAuthFunc, list func(context.Context, DomainPrincipal, SecretListOptions) (SecretPage, error)) http.HandlerFunc {
	return secretAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		opts, ok := parseSecretListOptions(w, r)
		if !ok {
			return
		}
		page, err := list(r.Context(), p, opts)
		if writeSecretError(w, err, "list secrets") {
			return
		}
		if page.Items == nil {
			page.Items = []Secret{}
		}
		for i := range page.Items {
			if !opts.IncludeFields {
				page.Items[i].Fields = nil
			}
			redactSecret(&page.Items[i])
		}
		writeSecretJSON(w, http.StatusOK, map[string]any{
			"schema_version": "witself.v0", "items": page.Items, "next_cursor": page.NextCursor,
		})
	})
}

func secretLimitStatusHandler(auth PrincipalAuthFunc, status func(context.Context, DomainPrincipal) (SecretLimitStatus, error)) http.HandlerFunc {
	return secretAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		value, err := status(r.Context(), p)
		if writeSecretError(w, err, "read secret limit status") {
			return
		}
		writeSecretJSON(w, http.StatusOK, map[string]any{
			"schema_version": "witself.v0", "limit": value,
		})
	})
}

func getSecretHandler(auth PrincipalAuthFunc, get func(context.Context, DomainPrincipal, string) (Secret, error)) http.HandlerFunc {
	return secretAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		secretID, ok := secretPathID(w, r, "secret", "sec")
		if !ok {
			return
		}
		value, err := get(r.Context(), p, secretID)
		if writeSecretError(w, err, "read secret") {
			return
		}
		redactSecret(&value)
		writeSecretJSON(w, http.StatusOK, map[string]any{"schema_version": "witself.v0", "secret": value})
	})
}

func secretLifecycleHandler(
	auth PrincipalAuthFunc,
	archive func(context.Context, DomainPrincipal, string, SecretLifecycleRequest) (SecretMutationResult, error),
	restore func(context.Context, DomainPrincipal, string, SecretLifecycleRequest) (SecretMutationResult, error),
	deleteSecret func(context.Context, DomainPrincipal, string, SecretLifecycleRequest) (SecretMutationResult, error),
) http.HandlerFunc {
	return secretAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		secretID, operation, ok := secretLifecyclePath(w, r)
		if !ok {
			return
		}
		var in SecretLifecycleRequest
		if !decodeSecretRequest(w, r, &in, maxSecretLifecycleRequestBytes) {
			return
		}
		if in.ExpectedRowVersion < 1 {
			writeJSONError(w, http.StatusBadRequest, "expected_row_version must be positive")
			return
		}
		if !setSecretIdempotencyKey(w, r, &in.IdempotencyKey) {
			return
		}
		var (
			result SecretMutationResult
			err    error
		)
		switch operation {
		case "archive":
			if archive == nil {
				writeJSONError(w, http.StatusNotFound, "secret resource not found")
				return
			}
			result, err = archive(r.Context(), p, secretID, in)
		case "restore":
			if restore == nil {
				writeJSONError(w, http.StatusNotFound, "secret resource not found")
				return
			}
			result, err = restore(r.Context(), p, secretID, in)
		case "delete":
			if deleteSecret == nil {
				writeJSONError(w, http.StatusNotFound, "secret resource not found")
				return
			}
			result, err = deleteSecret(r.Context(), p, secretID, in)
		}
		if writeSecretError(w, err, operation+" secret") {
			return
		}
		redactSecret(&result.Secret)
		writeSecretJSON(w, http.StatusOK, map[string]any{
			"schema_version": "witself.v0", "secret": result.Secret, "receipt": result.Receipt,
		})
	})
}

func accessSecretFieldHandler(auth PrincipalAuthFunc, access func(context.Context, DomainPrincipal, string, string, AccessSecretFieldRequest) (SecretMaterial, error)) http.HandlerFunc {
	return secretAgentHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if !secretNoQuery(w, r) {
			return
		}
		secretID, ok := secretPathID(w, r, "secret", "sec")
		if !ok {
			return
		}
		fieldID, ok := secretFieldAccessPathID(w, r)
		if !ok {
			return
		}
		if !decodeSecretAccessRequest(w, r) {
			return
		}
		request := AccessSecretFieldRequest{}
		if !setSecretIdempotencyKey(w, r, &request.IdempotencyKey) {
			return
		}
		material, err := access(r.Context(), p, secretID, fieldID, request)
		if writeSecretError(w, err, "access secret field") {
			return
		}
		writeSecretJSON(w, http.StatusOK, map[string]any{"schema_version": "witself.v0", "material": material})
	})
}

func decodeSecretAccessRequest(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxSecretAccessRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var body struct{}
	err := decoder.Decode(&body)
	if errors.Is(err, io.EOF) {
		return true
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "secret request body exceeds the supported limit")
		} else {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON secret body")
		}
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON secret body")
		return false
	}
	return true
}

func secretAgentHandler(auth PrincipalAuthFunc, next func(http.ResponseWriter, *http.Request, DomainPrincipal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
			if p.Kind != PrincipalKindAgent || strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.AccountID) == "" || strings.TrimSpace(p.RealmID) == "" {
				writeJSONError(w, http.StatusForbidden, "only a full agent token may use the secret vault")
				return
			}
			next(w, r, p)
		})(w, r)
	}
}

func decodeSecretRequest(w http.ResponseWriter, r *http.Request, dst any, maximum int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maximum)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "secret request body exceeds the supported limit")
		} else {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON secret body")
		}
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON secret body")
		return false
	}
	return true
}

func setSecretIdempotencyKey(w http.ResponseWriter, r *http.Request, dst *string) bool {
	value := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if value == "" || len(value) > maxSecretIdempotencyKeyBytes || !utf8.ValidString(value) {
		writeJSONError(w, http.StatusBadRequest, "valid Idempotency-Key header is required")
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			writeJSONError(w, http.StatusBadRequest, "valid Idempotency-Key header is required")
			return false
		}
	}
	*dst = value
	return true
}

func secretNoQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		writeJSONError(w, http.StatusBadRequest, "secret route does not accept query parameters")
		return false
	}
	return true
}

func secretPathID(w http.ResponseWriter, r *http.Request, pathName, prefix string) (string, bool) {
	value := strings.TrimSpace(r.PathValue(pathName))
	if !secretResourceIDPattern.MatchString(value) || !strings.HasPrefix(value, prefix+"_") {
		writeJSONError(w, http.StatusNotFound, "secret resource not found")
		return "", false
	}
	return value, true
}

func secretFieldAccessPathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	action := strings.TrimSpace(r.PathValue("action"))
	const suffix = ":access"
	if !strings.HasSuffix(action, suffix) {
		writeJSONError(w, http.StatusNotFound, "secret resource not found")
		return "", false
	}
	fieldID := strings.TrimSuffix(action, suffix)
	if !secretResourceIDPattern.MatchString(fieldID) || !strings.HasPrefix(fieldID, "fld_") {
		writeJSONError(w, http.StatusNotFound, "secret resource not found")
		return "", false
	}
	return fieldID, true
}

func secretLifecyclePath(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	action := strings.TrimSpace(r.PathValue("action"))
	for _, operation := range []string{"archive", "restore", "delete"} {
		suffix := ":" + operation
		if !strings.HasSuffix(action, suffix) {
			continue
		}
		secretID := strings.TrimSuffix(action, suffix)
		if secretResourceIDPattern.MatchString(secretID) && strings.HasPrefix(secretID, "sec_") {
			return secretID, operation, true
		}
		break
	}
	writeJSONError(w, http.StatusNotFound, "secret resource not found")
	return "", "", false
}

func parseSecretListOptions(w http.ResponseWriter, r *http.Request) (SecretListOptions, bool) {
	query := r.URL.Query()
	for key, values := range query {
		if key == "tag" {
			if len(values) == 0 {
				writeJSONError(w, http.StatusBadRequest, "invalid secret list query")
				return SecretListOptions{}, false
			}
			continue
		}
		switch key {
		case "q", "lifecycle", "template", "limit", "cursor", "include_fields":
			if len(values) != 1 {
				writeJSONError(w, http.StatusBadRequest, "invalid secret list query")
				return SecretListOptions{}, false
			}
		default:
			writeJSONError(w, http.StatusBadRequest, "invalid secret list query")
			return SecretListOptions{}, false
		}
	}
	opts := SecretListOptions{
		Query: strings.TrimSpace(query.Get("q")), Lifecycle: strings.TrimSpace(query.Get("lifecycle")),
		Template: strings.TrimSpace(query.Get("template")), Tags: query["tag"], Cursor: strings.TrimSpace(query.Get("cursor")),
	}
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxSecretListLimit {
			writeJSONError(w, http.StatusBadRequest, "secret list limit must be between 1 and 100")
			return SecretListOptions{}, false
		}
		opts.Limit = limit
	}
	if raw := strings.TrimSpace(query.Get("include_fields")); raw != "" {
		include, err := strconv.ParseBool(raw)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "include_fields must be true or false")
			return SecretListOptions{}, false
		}
		opts.IncludeFields = include
	}
	return opts, true
}

func redactSecret(value *Secret) {
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

func writeSecretJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeSecretError(w http.ResponseWriter, err error, operation string) bool {
	if err == nil {
		return false
	}
	var limitErr *SecretLimitError
	switch {
	case errors.Is(err, ErrBadInput):
		writeJSONError(w, http.StatusBadRequest, "invalid secret request")
	case errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusForbidden, "secret access forbidden")
	case errors.Is(err, ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "secret resource not found")
	case errors.Is(err, ErrIdempotencyConflict):
		writeJSONError(w, http.StatusConflict, "idempotency key was reused for a different secret operation")
	case errors.Is(err, ErrSecretVaultKeyUnavailable):
		writeJSONError(w, http.StatusConflict, ErrSecretVaultKeyUnavailable.Error())
	case errors.Is(err, ErrSecretVaultKeyMismatch):
		writeSecretCodedError(w, http.StatusConflict, "secret_vault_key_mismatch", ErrSecretVaultKeyMismatch.Error())
	case errors.As(err, &limitErr):
		writeSecretLimitError(w, limitErr.Status)
	case errors.Is(err, ErrConflict):
		writeJSONError(w, http.StatusConflict, "secret state conflict")
	default:
		writeJSONError(w, http.StatusInternalServerError, "could not "+operation)
	}
	return true
}

func writeSecretLimitError(w http.ResponseWriter, status SecretLimitStatus) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema_version": "witself.v0",
		"code":           "stored_secret_limit_reached",
		"error":          ErrSecretLimitReached.Error(),
		"retryable":      false,
		"limit":          status,
	})
}

func writeSecretCodedError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"schema_version": "witself.v0",
		"code":           code,
		"error":          message,
	})
}
