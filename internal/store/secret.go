package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// SecretAEADAlgorithm is the only field and key-wrap primitive accepted by
	// the v1 sealed plane. The backend validates this label but never owns the
	// corresponding key or performs the operation.
	SecretAEADAlgorithm = "AES_256_GCM_RANDOM_NONCE_V1"
	// SecretAADVersion is the only accepted authenticated-data encoding version.
	SecretAADVersion = int64(1)
	// SecretEnvelopeVersion is the only accepted sealed-field envelope version.
	SecretEnvelopeVersion = int64(1)

	// SecretLifecycleActive identifies a live secret available for use.
	SecretLifecycleActive = "active"
	// SecretLifecycleArchived identifies a secret retained but unavailable for use.
	SecretLifecycleArchived = "archived"

	// SecretFieldText identifies an ordinary text field.
	SecretFieldText = "text"
	// SecretFieldUsername identifies an account username field.
	SecretFieldUsername = "username"
	// SecretFieldPassword identifies a password field.
	SecretFieldPassword = "password"
	// SecretFieldURL identifies a URL field.
	SecretFieldURL = "url"
	// SecretFieldAPIKey identifies an API key field.
	SecretFieldAPIKey = "api_key"
	// SecretFieldToken identifies an authentication token field.
	SecretFieldToken = "token"
	// SecretFieldPrivateKey identifies a private key field.
	SecretFieldPrivateKey = "private_key"
	// SecretFieldTOTP identifies a sealed TOTP enrollment payload.
	SecretFieldTOTP = "totp"
	// SecretFieldRecoveryCode identifies an account recovery code field.
	SecretFieldRecoveryCode = "recovery_code"
	// SecretFieldNote identifies a free-form note field.
	SecretFieldNote = "note"

	// SecretEncodingUTF8 identifies a UTF-8 field value.
	SecretEncodingUTF8 = "utf8"
	// SecretEncodingJSON identifies a JSON field value.
	SecretEncodingJSON = "json"
	// SecretEncodingBinary identifies an opaque binary field value.
	SecretEncodingBinary = "binary"

	// UsageDimensionStoredSecret meters persisted secrets.
	UsageDimensionStoredSecret = "stored_secret"
	// UsageDimensionSecretRead meters delivery of encrypted secret material.
	UsageDimensionSecretRead = "secret_read"
	// UsageDimensionEncryptedStorage meters encrypted bytes at rest.
	UsageDimensionEncryptedStorage = "encrypted_storage_byte"
	// UsageDimensionTOTPCode meters locally generated TOTP code operations.
	UsageDimensionTOTPCode = "totp_code"
	// UsageDimensionRuntimeInjection meters secret injection into a runtime.
	UsageDimensionRuntimeInjection = "runtime_injection"
	// UsageUnitSecret is the unit for stored-secret usage.
	UsageUnitSecret = "secret"
	// UsageUnitSecretAccess is the unit for secret access usage.
	UsageUnitSecretAccess = "access"
)

const (
	maxSecretNameBytes           = 256
	maxSecretDescriptionBytes    = 4096
	maxSecretTags                = 64
	maxSecretFields              = 64
	maxSecretPublicValueBytes    = 64 * 1024
	maxSecretCiphertextBytes     = 64*1024 + 28
	minSecretCiphertextBytes     = 1 + 28
	secretWrappedDEKBytes        = 32 + 28
	maxSecretMutationBytes       = 1024 * 1024
	maxSecretIdempotencyKeyBytes = 512
	defaultSecretPageSize        = 50
	maxSecretPageSize            = 100
)

var (
	secretFieldNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	secretTemplatePattern  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	secretTagPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
)

var (
	// ErrSecretForbidden reports a principal outside the self-owned vault
	// boundary. Operator-directed plaintext use still runs through the active
	// agent; an operator token is not a decrypt principal.
	ErrSecretForbidden = errors.New("secret access forbidden")
	// ErrSecretNotFound hides missing and cross-tenant resource ids behind one
	// stable result.
	ErrSecretNotFound = errors.New("secret not found")
	// ErrSecretFieldNotFound reports an unavailable field inside an authorized
	// secret without revealing another secret's field ids.
	ErrSecretFieldNotFound = errors.New("secret field not found")
	// ErrSecretInputInvalid reports malformed public metadata or sealed
	// envelopes. It never includes field values or envelope bytes.
	ErrSecretInputInvalid = errors.New("invalid secret input")
	// ErrSecretConflict reports optimistic-concurrency, lifecycle, or live-name
	// conflicts.
	ErrSecretConflict = errors.New("secret changed concurrently")
	// ErrSecretIdempotencyConflict reports retry-key reuse for different
	// normalized semantics.
	ErrSecretIdempotencyConflict = errors.New("secret idempotency key conflict")
	// ErrVaultKeyUnavailable means the backend has a public key binding while
	// this installation lacks its matching local key. The caller must not
	// generate a replacement.
	ErrVaultKeyUnavailable = errors.New("agent vault key unavailable")
	// ErrVaultKeyMismatch means local public key identity differs from the
	// backend binding. Neither side may be overwritten automatically.
	ErrVaultKeyMismatch = errors.New("agent vault key mismatch")
	// ErrVaultKeyConflict reports an attempt to register a different current
	// epoch for an already-bound agent.
	ErrVaultKeyConflict = errors.New("agent vault key already registered")
)

// VaultKeyBinding is the public, non-decrypting identity of one AVK epoch.
// It is safe for status output and account archives; key bytes never have a
// representation in the store package.
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

// RegisterVaultKeyInput registers only public AVK identity after the client
// has resolved the fail-closed local/server bootstrap state machine.
type RegisterVaultKeyInput struct {
	ID             string
	KeyVersion     int64
	Algorithm      string
	Fingerprint    string
	IdempotencyKey string
}

// Secret is the redacted structured-secret view. Sensitive fields never carry
// ciphertext or plaintext here; exact material has a separate response type.
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
	SensitiveCount int           `json:"sensitive_field_count"`
}

// SecretField is one redacted field projection. PublicValue is populated only
// for a field explicitly stored as non-sensitive. Redacted is always true for
// sensitive fields.
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

// SealedDEKInput is a client-authored wrapped data-encryption key. The backend
// validates scope and shape and stores these opaque bytes without unwrapping.
type SealedDEKInput struct {
	ID                 string `json:"id"`
	Generation         int64  `json:"generation"`
	WrappedDEK         []byte `json:"wrapped_dek"`
	WrapAlgorithm      string `json:"wrap_algorithm"`
	AADVersion         int64  `json:"aad_version"`
	WrapRevision       int64  `json:"wrap_revision"`
	WrappingKeyID      string `json:"wrapping_key_id"`
	WrappingKeyVersion int64  `json:"wrapping_key_version"`
}

// SealedFieldInput is one ciphertext plus the separately wrapped DEK that can
// open it. Client-generated ids exist before AAD construction.
type SealedFieldInput struct {
	EnvelopeVersion int64          `json:"envelope_version"`
	Ciphertext      []byte         `json:"ciphertext"`
	Algorithm       string         `json:"algorithm"`
	AADVersion      int64          `json:"aad_version"`
	DEK             SealedDEKInput `json:"dek"`
}

// CreateSecretFieldInput has exactly one branch: PublicValue for a
// non-sensitive UTF-8 field, or Sealed for a sensitive field.
type CreateSecretFieldInput struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`
	Sensitive    bool              `json:"sensitive"`
	Encoding     string            `json:"encoding"`
	ValueVersion int64             `json:"value_version"`
	PublicValue  *string           `json:"public_value,omitempty"`
	Sealed       *SealedFieldInput `json:"sealed,omitempty"`
}

// CreateSecretInput is already encrypted when it crosses the store boundary.
// Scope and owner are always derived from the authenticated agent principal.
type CreateSecretInput struct {
	ID             string                   `json:"id"`
	Name           string                   `json:"name"`
	Description    string                   `json:"description,omitempty"`
	Template       string                   `json:"template"`
	Tags           []string                 `json:"tags,omitempty"`
	Fields         []CreateSecretFieldInput `json:"fields"`
	IdempotencyKey string                   `json:"-"`
}

// SecretMaterial is the exact one-field package authorized for local
// decryption. No sibling fields or plaintext can fit in this response type.
type SecretMaterial struct {
	SecretID        string         `json:"secret_id"`
	FieldID         string         `json:"field_id"`
	FieldName       string         `json:"field_name"`
	FieldKind       string         `json:"field_kind"`
	Encoding        string         `json:"encoding"`
	ValueVersion    int64          `json:"value_version"`
	EnvelopeVersion int64          `json:"envelope_version"`
	Ciphertext      []byte         `json:"ciphertext"`
	Algorithm       string         `json:"algorithm"`
	AADVersion      int64          `json:"aad_version"`
	DEK             SealedDEKInput `json:"dek"`
	SecretRevision  int64          `json:"secret_revision"`
	FieldRevision   int64          `json:"field_revision"`
}

// AccessSecretFieldInput supplies a retry fence for the auditable encrypted
// material delivery. The raw key is hashed before persistence.
type AccessSecretFieldInput struct {
	IdempotencyKey string `json:"-"`
}

// SecretLifecycleInput fences a reversible archive or restore against the
// exact redacted secret revision the client last observed. The retry key is
// hashed before it enters PostgreSQL.
type SecretLifecycleInput struct {
	ExpectedRowVersion int64  `json:"expected_row_version"`
	IdempotencyKey     string `json:"-"`
}

// SecretMutationReceipt is a value-free durable retry result. RequestHash is
// a canonical digest; the raw retry key and payload are never returned.
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

// SecretMutationResult returns a redacted post-mutation view and receipt.
type SecretMutationResult struct {
	Secret  Secret                `json:"secret"`
	Receipt SecretMutationReceipt `json:"receipt"`
}

// SecretPage is one bounded metadata/redacted-field page.
type SecretPage struct {
	Secrets    []Secret `json:"secrets"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// SecretListOptions searches only public metadata and explicitly
// non-sensitive values. Sensitive contents never enter a search expression.
type SecretListOptions struct {
	Query         string
	Lifecycle     string
	Template      string
	Tags          []string
	Limit         int
	Cursor        string
	IncludeFields bool
}

func requireSelfSecretPrincipal(p Principal) error {
	if p.Kind != PrincipalAgent ||
		(strings.TrimSpace(p.AccessProfile) != "" && p.AccessProfile != AccessProfileFull) ||
		p.AccountID == "" || p.RealmID == "" || p.ID == "" {
		return ErrSecretForbidden
	}
	return nil
}

func normalizeRegisterVaultKeyInput(in RegisterVaultKeyInput) (RegisterVaultKeyInput, error) {
	in.ID = strings.TrimSpace(in.ID)
	in.Algorithm = strings.TrimSpace(in.Algorithm)
	in.Fingerprint = strings.ToLower(strings.TrimSpace(in.Fingerprint))
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if !validSecretGeneratedID(in.ID, "avk") || in.KeyVersion != 1 ||
		in.Algorithm != SecretAEADAlgorithm || !validFactSHA256(in.Fingerprint) ||
		len(in.IdempotencyKey) < 1 || len(in.IdempotencyKey) > maxSecretIdempotencyKeyBytes {
		return RegisterVaultKeyInput{}, ErrSecretInputInvalid
	}
	return in, nil
}

func normalizeCreateSecretInput(in CreateSecretInput) (CreateSecretInput, error) {
	in.ID = strings.TrimSpace(in.ID)
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	in.Template = strings.TrimSpace(in.Template)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if !validSecretGeneratedID(in.ID, "sec") || len(in.Name) < 1 ||
		len(in.Name) > maxSecretNameBytes || strings.ContainsAny(in.Name, "\x00\r\n") ||
		len(in.Description) > maxSecretDescriptionBytes || strings.ContainsRune(in.Description, '\x00') ||
		!secretTemplatePattern.MatchString(in.Template) ||
		len(in.IdempotencyKey) < 1 || len(in.IdempotencyKey) > maxSecretIdempotencyKeyBytes ||
		len(in.Fields) < 1 || len(in.Fields) > maxSecretFields {
		return CreateSecretInput{}, ErrSecretInputInvalid
	}

	tags, err := normalizeSecretTags(in.Tags)
	if err != nil {
		return CreateSecretInput{}, err
	}
	in.Tags = tags
	fieldNames := make(map[string]bool, len(in.Fields))
	fieldIDs := make(map[string]bool, len(in.Fields))
	dekIDs := make(map[string]bool, len(in.Fields))
	totalBytes := len(in.Name) + len(in.Description) + len(in.Template)
	for index := range in.Fields {
		field, err := normalizeCreateSecretField(in.Fields[index])
		if err != nil || fieldNames[field.Name] || fieldIDs[field.ID] {
			return CreateSecretInput{}, ErrSecretInputInvalid
		}
		fieldNames[field.Name] = true
		fieldIDs[field.ID] = true
		if field.Sealed != nil {
			if dekIDs[field.Sealed.DEK.ID] {
				return CreateSecretInput{}, ErrSecretInputInvalid
			}
			dekIDs[field.Sealed.DEK.ID] = true
			totalBytes += len(field.Sealed.Ciphertext) + len(field.Sealed.DEK.WrappedDEK)
		} else if field.PublicValue != nil {
			totalBytes += len(*field.PublicValue)
		}
		in.Fields[index] = field
	}
	if totalBytes > maxSecretMutationBytes {
		return CreateSecretInput{}, ErrSecretInputInvalid
	}
	sort.Slice(in.Fields, func(i, j int) bool {
		if in.Fields[i].Name == in.Fields[j].Name {
			return in.Fields[i].ID < in.Fields[j].ID
		}
		return in.Fields[i].Name < in.Fields[j].Name
	})
	return in, nil
}

func normalizeSecretLifecycleInput(in SecretLifecycleInput) (SecretLifecycleInput, error) {
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.ExpectedRowVersion < 1 || len(in.IdempotencyKey) < 1 ||
		len(in.IdempotencyKey) > maxSecretIdempotencyKeyBytes {
		return SecretLifecycleInput{}, ErrSecretInputInvalid
	}
	return in, nil
}

func normalizeCreateSecretField(field CreateSecretFieldInput) (CreateSecretFieldInput, error) {
	field.ID = strings.TrimSpace(field.ID)
	field.Name = strings.TrimSpace(field.Name)
	field.Kind = strings.TrimSpace(field.Kind)
	field.Encoding = strings.TrimSpace(field.Encoding)
	if !validSecretGeneratedID(field.ID, "fld") ||
		!secretFieldNamePattern.MatchString(field.Name) ||
		!validSecretFieldKind(field.Kind) || field.ValueVersion != 1 ||
		secretFieldKindRequiresProtection(field.Kind) && !field.Sensitive {
		return CreateSecretFieldInput{}, ErrSecretInputInvalid
	}
	if !field.Sensitive {
		if field.Encoding != SecretEncodingUTF8 || field.PublicValue == nil ||
			field.Sealed != nil || len(*field.PublicValue) > maxSecretPublicValueBytes ||
			strings.ContainsRune(*field.PublicValue, '\x00') {
			return CreateSecretFieldInput{}, ErrSecretInputInvalid
		}
		return field, nil
	}
	if field.PublicValue != nil || field.Sealed == nil ||
		(field.Encoding != SecretEncodingUTF8 && field.Encoding != SecretEncodingJSON && field.Encoding != SecretEncodingBinary) {
		return CreateSecretFieldInput{}, ErrSecretInputInvalid
	}
	sealed := field.Sealed
	sealed.Algorithm = strings.TrimSpace(sealed.Algorithm)
	sealed.DEK.ID = strings.TrimSpace(sealed.DEK.ID)
	sealed.DEK.WrapAlgorithm = strings.TrimSpace(sealed.DEK.WrapAlgorithm)
	sealed.DEK.WrappingKeyID = strings.TrimSpace(sealed.DEK.WrappingKeyID)
	if sealed.EnvelopeVersion != SecretEnvelopeVersion ||
		len(sealed.Ciphertext) < minSecretCiphertextBytes || len(sealed.Ciphertext) > maxSecretCiphertextBytes ||
		sealed.Algorithm != SecretAEADAlgorithm || sealed.AADVersion != SecretAADVersion ||
		!validSecretGeneratedID(sealed.DEK.ID, "dek") || sealed.DEK.Generation != 1 ||
		len(sealed.DEK.WrappedDEK) != secretWrappedDEKBytes ||
		sealed.DEK.WrapAlgorithm != SecretAEADAlgorithm || sealed.DEK.AADVersion != SecretAADVersion ||
		sealed.DEK.WrapRevision < 1 || !validSecretGeneratedID(sealed.DEK.WrappingKeyID, "avk") ||
		sealed.DEK.WrappingKeyVersion < 1 {
		return CreateSecretFieldInput{}, ErrSecretInputInvalid
	}
	return field, nil
}

func normalizeSecretTags(tags []string) ([]string, error) {
	if len(tags) > maxSecretTags {
		return nil, ErrSecretInputInvalid
	}
	out := make([]string, 0, len(tags))
	seen := make(map[string]bool, len(tags))
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if !secretTagPattern.MatchString(tag) || seen[tag] {
			return nil, ErrSecretInputInvalid
		}
		seen[tag] = true
		out = append(out, tag)
	}
	sort.Strings(out)
	return out, nil
}

func validSecretGeneratedID(value, prefix string) bool {
	body := strings.TrimPrefix(value, prefix+"_")
	if body == value || len(body) != 16 {
		return false
	}
	for _, char := range body {
		if (char < 'a' || char > 'z') && (char < '2' || char > '7') {
			return false
		}
	}
	return true
}

func secretIdempotencyKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func validSecretFieldKind(kind string) bool {
	switch kind {
	case SecretFieldText, SecretFieldUsername, SecretFieldPassword,
		SecretFieldURL, SecretFieldAPIKey, SecretFieldToken,
		SecretFieldPrivateKey, SecretFieldTOTP, SecretFieldRecoveryCode,
		SecretFieldNote:
		return true
	default:
		return false
	}
}

func secretFieldKindRequiresProtection(kind string) bool {
	switch kind {
	case SecretFieldPassword, SecretFieldAPIKey, SecretFieldToken,
		SecretFieldPrivateKey, SecretFieldTOTP, SecretFieldRecoveryCode:
		return true
	default:
		return false
	}
}

func normalizeSecretListOptions(options SecretListOptions) (SecretListOptions, error) {
	options.Query = strings.TrimSpace(options.Query)
	options.Lifecycle = strings.TrimSpace(options.Lifecycle)
	options.Template = strings.TrimSpace(options.Template)
	options.Cursor = strings.TrimSpace(options.Cursor)
	if len(options.Query) > 512 || strings.ContainsRune(options.Query, '\x00') ||
		(options.Lifecycle != "" && options.Lifecycle != SecretLifecycleActive && options.Lifecycle != SecretLifecycleArchived) ||
		(options.Template != "" && !secretTemplatePattern.MatchString(options.Template)) ||
		len(options.Cursor) > 1024 {
		return SecretListOptions{}, ErrSecretInputInvalid
	}
	tags, err := normalizeSecretTags(options.Tags)
	if err != nil {
		return SecretListOptions{}, err
	}
	options.Tags = tags
	if options.Lifecycle == "" {
		options.Lifecycle = SecretLifecycleActive
	}
	if options.Limit == 0 {
		options.Limit = defaultSecretPageSize
	}
	if options.Limit < 1 || options.Limit > maxSecretPageSize {
		return SecretListOptions{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrSecretInputInvalid, maxSecretPageSize)
	}
	return options, nil
}
