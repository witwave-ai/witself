package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	// VaultKeyRotationOpen is a rotation still accepting replacement wrappers.
	VaultKeyRotationOpen = "open"
	// VaultKeyRotationCommitted is a rotation whose target AVK became current.
	VaultKeyRotationCommitted = "committed"
	// VaultKeyRotationCancelled is a rotation retired without changing the source.
	VaultKeyRotationCancelled = "cancelled"

	// VaultKeyRotationRecoveryArtifact records a verified recovery copy.
	VaultKeyRotationRecoveryArtifact = "recovery_artifact"
	// VaultKeyRotationRiskAccepted records explicit unrecoverable-loss acceptance.
	VaultKeyRotationRiskAccepted = "risk_accepted"

	defaultVaultKeyRotationItemPageSize = 50
	maxVaultKeyRotationItemPageSize     = 100
	maxVaultKeyRotationStageItems       = 100
	maxVaultKeyRotationStageBytes       = 128 * 1024
	vaultKeyRotationWrappedDEKBytes     = 60
)

var (
	// ErrVaultKeyRotationNotFound means the scoped rotation does not exist.
	ErrVaultKeyRotationNotFound = errors.New("vault key rotation not found")
	// ErrVaultKeyRotationInProgress means the owner already has an open rotation.
	ErrVaultKeyRotationInProgress = errors.New("vault key rotation already in progress")
	// ErrVaultKeyRotationConflict means the rotation changed concurrently.
	ErrVaultKeyRotationConflict = errors.New("vault key rotation changed concurrently")
	// ErrVaultKeyRotationIncomplete means not every immutable item was staged.
	ErrVaultKeyRotationIncomplete = errors.New("vault key rotation is incomplete")
)

// VaultKeyRotation is the value-free public state of one client-driven AVK
// rotation. StagedPlanHash is populated only when every immutable snapshot item
// has a replacement wrapper.
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

// StartVaultKeyRotationInput registers a new public pending AVK epoch and
// snapshots every retained wrapped DEK. The source fields fence the exact
// current epoch observed by the client.
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

// VaultKeyRotationItem is one immutable source wrapper snapshot and its
// optional staged replacement. FieldKind lets the client select the canonical
// field-vs-TOTP AAD domain without exposing any field value or ciphertext.
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

// VaultKeyRotationItemListOptions controls pagination of rotation items.
type VaultKeyRotationItemListOptions struct {
	Limit  int
	Cursor string
}

// VaultKeyRotationItemPage contains one page of opaque DEK-wrapper rotation
// work and the cursor needed to continue.
type VaultKeyRotationItemPage struct {
	Items      []VaultKeyRotationItem `json:"items"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

// StageVaultKeyRotationItemInput supplies one locally rewrapped DEK and the
// exact source fences used to derive it.
type StageVaultKeyRotationItemInput struct {
	DEKID                       string `json:"dek_id"`
	ExpectedSourceDEKRowVersion int64  `json:"expected_source_dek_row_version"`
	ExpectedSourceWrapRevision  int64  `json:"expected_source_wrap_revision"`
	TargetWrappedDEK            []byte `json:"target_wrapped_dek"`
	TargetWrapRevision          int64  `json:"target_wrap_revision"`
}

// StageVaultKeyRotationInput atomically stages a bounded batch of locally
// rewrapped DEKs against an exact rotation revision.
type StageVaultKeyRotationInput struct {
	ExpectedRotationRowVersion int64                            `json:"expected_rotation_row_version"`
	Items                      []StageVaultKeyRotationItemInput `json:"items"`
	IdempotencyKey             string                           `json:"-"`
}

// CommitVaultKeyRotationInput supplies completed-plan fences and the client's
// value-free recovery disposition for an AVK rotation.
type CommitVaultKeyRotationInput struct {
	ExpectedRotationRowVersion int64                               `json:"expected_rotation_row_version"`
	ExpectedItemCount          int64                               `json:"expected_item_count"`
	ExpectedPlanHash           string                              `json:"expected_plan_hash"`
	RecoveryDisposition        VaultKeyRotationRecoveryDisposition `json:"recovery_disposition"`
	IdempotencyKey             string                              `json:"-"`
}

// VaultKeyRotationRecoveryDisposition is the value-free proof-or-risk choice
// the client must make before committing a rotation. ArtifactSHA256 binds the
// commit only to a client-verified recovery artifact; the backend never
// receives the artifact, its location, its passphrase, or any key material.
type VaultKeyRotationRecoveryDisposition struct {
	Mode           string `json:"mode"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
}

// CancelVaultKeyRotationInput fences cancellation of an open rotation.
type CancelVaultKeyRotationInput struct {
	ExpectedRotationRowVersion int64  `json:"expected_rotation_row_version"`
	IdempotencyKey             string `json:"-"`
}

// VaultKeyRotationReceipt records a durable idempotent rotation mutation.
type VaultKeyRotationReceipt struct {
	Operation      string    `json:"operation"`
	RequestHash    string    `json:"request_hash"`
	RotationID     string    `json:"rotation_id"`
	ResultRevision int64     `json:"result_revision"`
	Replayed       bool      `json:"replayed"`
	CreatedAt      time.Time `json:"created_at"`
}

func normalizeStartVaultKeyRotationInput(in StartVaultKeyRotationInput) (StartVaultKeyRotationInput, error) {
	in.ID = strings.TrimSpace(in.ID)
	in.ExpectedSourceKeyID = strings.TrimSpace(in.ExpectedSourceKeyID)
	in.TargetKeyID = strings.TrimSpace(in.TargetKeyID)
	in.TargetAlgorithm = strings.TrimSpace(in.TargetAlgorithm)
	in.TargetFingerprint = strings.ToLower(strings.TrimSpace(in.TargetFingerprint))
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if !validSecretGeneratedID(in.ID, "vkr") ||
		!validSecretGeneratedID(in.ExpectedSourceKeyID, "avk") ||
		!validSecretGeneratedID(in.TargetKeyID, "avk") ||
		in.TargetKeyID == in.ExpectedSourceKeyID ||
		in.ExpectedSourceKeyVersion < 1 || in.ExpectedSourceKeyRowVersion < 1 ||
		in.ExpectedSourceKeyVersion == int64(^uint64(0)>>1) ||
		in.TargetKeyVersion != in.ExpectedSourceKeyVersion+1 ||
		in.TargetAlgorithm != SecretAEADAlgorithm || !validFactSHA256(in.TargetFingerprint) ||
		len(in.IdempotencyKey) < 1 || len(in.IdempotencyKey) > maxSecretIdempotencyKeyBytes {
		return StartVaultKeyRotationInput{}, ErrSecretInputInvalid
	}
	return in, nil
}

func normalizeVaultKeyRotationItemListOptions(in VaultKeyRotationItemListOptions) (VaultKeyRotationItemListOptions, string, error) {
	in.Cursor = strings.TrimSpace(in.Cursor)
	if in.Limit == 0 {
		in.Limit = defaultVaultKeyRotationItemPageSize
	}
	if in.Limit < 1 || in.Limit > maxVaultKeyRotationItemPageSize || len(in.Cursor) > 1024 {
		return VaultKeyRotationItemListOptions{}, "", ErrSecretInputInvalid
	}
	after := ""
	if in.Cursor != "" {
		var err error
		after, err = decodeVaultKeyRotationItemCursor(in.Cursor)
		if err != nil {
			return VaultKeyRotationItemListOptions{}, "", err
		}
	}
	return in, after, nil
}

func normalizeStageVaultKeyRotationInput(in StageVaultKeyRotationInput) (StageVaultKeyRotationInput, error) {
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.ExpectedRotationRowVersion < 1 || len(in.Items) < 1 || len(in.Items) > maxVaultKeyRotationStageItems ||
		len(in.IdempotencyKey) < 1 || len(in.IdempotencyKey) > maxSecretIdempotencyKeyBytes {
		return StageVaultKeyRotationInput{}, ErrSecretInputInvalid
	}
	seen := make(map[string]bool, len(in.Items))
	total := 0
	for index := range in.Items {
		item := &in.Items[index]
		item.DEKID = strings.TrimSpace(item.DEKID)
		if !validSecretGeneratedID(item.DEKID, "dek") || seen[item.DEKID] ||
			item.ExpectedSourceDEKRowVersion < 1 || item.ExpectedSourceWrapRevision < 1 ||
			item.ExpectedSourceWrapRevision == int64(^uint64(0)>>1) ||
			item.TargetWrapRevision != item.ExpectedSourceWrapRevision+1 ||
			len(item.TargetWrappedDEK) != vaultKeyRotationWrappedDEKBytes {
			return StageVaultKeyRotationInput{}, ErrSecretInputInvalid
		}
		seen[item.DEKID] = true
		total += len(item.TargetWrappedDEK) + len(item.DEKID) + 32
		item.TargetWrappedDEK = append([]byte(nil), item.TargetWrappedDEK...)
	}
	if total > maxVaultKeyRotationStageBytes {
		return StageVaultKeyRotationInput{}, ErrSecretInputInvalid
	}
	sort.Slice(in.Items, func(i, j int) bool { return in.Items[i].DEKID < in.Items[j].DEKID })
	return in, nil
}

func normalizeCommitVaultKeyRotationInput(in CommitVaultKeyRotationInput) (CommitVaultKeyRotationInput, error) {
	in.ExpectedPlanHash = strings.ToLower(strings.TrimSpace(in.ExpectedPlanHash))
	in.RecoveryDisposition.Mode = strings.TrimSpace(in.RecoveryDisposition.Mode)
	in.RecoveryDisposition.ArtifactSHA256 = strings.TrimSpace(in.RecoveryDisposition.ArtifactSHA256)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.ExpectedRotationRowVersion < 1 || in.ExpectedItemCount < 0 ||
		!validFactSHA256(in.ExpectedPlanHash) || len(in.IdempotencyKey) < 1 ||
		len(in.IdempotencyKey) > maxSecretIdempotencyKeyBytes ||
		!validVaultKeyRotationRecoveryDisposition(in.RecoveryDisposition) {
		return CommitVaultKeyRotationInput{}, ErrSecretInputInvalid
	}
	return in, nil
}

func validVaultKeyRotationRecoveryDisposition(in VaultKeyRotationRecoveryDisposition) bool {
	switch in.Mode {
	case VaultKeyRotationRecoveryArtifact:
		return validFactSHA256(in.ArtifactSHA256) && in.ArtifactSHA256 == strings.ToLower(in.ArtifactSHA256)
	case VaultKeyRotationRiskAccepted:
		return in.ArtifactSHA256 == ""
	default:
		return false
	}
}

func normalizeCancelVaultKeyRotationInput(in CancelVaultKeyRotationInput) (CancelVaultKeyRotationInput, error) {
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.ExpectedRotationRowVersion < 1 || len(in.IdempotencyKey) < 1 ||
		len(in.IdempotencyKey) > maxSecretIdempotencyKeyBytes {
		return CancelVaultKeyRotationInput{}, ErrSecretInputInvalid
	}
	return in, nil
}

func vaultKeyRotationWrapperHash(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

type vaultKeyRotationPlanHasher struct {
	h hash.Hash
}

func newVaultKeyRotationPlanHasher(rotation VaultKeyRotation) *vaultKeyRotationPlanHasher {
	h := sha256.New()
	_, _ = h.Write([]byte("witself/vault-key-rotation-plan/v1\x00"))
	writeVaultRotationHashString(h, rotation.ID)
	writeVaultRotationHashString(h, rotation.SourceKeyID)
	writeVaultRotationHashInt(h, rotation.SourceKeyVersion)
	writeVaultRotationHashString(h, rotation.TargetKeyID)
	writeVaultRotationHashInt(h, rotation.TargetKeyVersion)
	writeVaultRotationHashInt(h, rotation.ItemCount)
	return &vaultKeyRotationPlanHasher{h: h}
}

func (h *vaultKeyRotationPlanHasher) Add(item VaultKeyRotationItem) {
	writeVaultRotationHashString(h.h, item.DEKID)
	writeVaultRotationHashString(h.h, item.SecretID)
	writeVaultRotationHashString(h.h, item.FieldID)
	writeVaultRotationHashString(h.h, item.FieldKind)
	writeVaultRotationHashInt(h.h, item.DEKGeneration)
	writeVaultRotationHashInt(h.h, item.SourceDEKRowVersion)
	writeVaultRotationHashInt(h.h, item.SourceWrapRevision)
	writeVaultRotationHashString(h.h, vaultKeyRotationWrapperHash(item.SourceWrappedDEK))
	writeVaultRotationHashString(h.h, item.SourceWrapAlgorithm)
	writeVaultRotationHashInt(h.h, item.SourceAADVersion)
	writeVaultRotationHashString(h.h, item.SourceWrappingKeyID)
	writeVaultRotationHashInt(h.h, item.SourceWrappingKeyVersion)
	writeVaultRotationHashInt(h.h, item.TargetWrapRevision)
	writeVaultRotationHashString(h.h, item.TargetWrapperSHA256)
}

func (h *vaultKeyRotationPlanHasher) Sum() string {
	return hex.EncodeToString(h.h.Sum(nil))
}

func writeVaultRotationHashString(h hash.Hash, value string) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write([]byte(value))
}

func writeVaultRotationHashInt(h hash.Hash, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = h.Write(encoded[:])
}

func encodeVaultKeyRotationItemCursor(dekID string) (string, error) {
	raw, err := json.Marshal(struct {
		DEKID string `json:"dek_id"`
	}{DEKID: dekID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeVaultKeyRotationItemCursor(value string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) > 1024 {
		return "", ErrSecretInputInvalid
	}
	var cursor struct {
		DEKID string `json:"dek_id"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || !validSecretGeneratedID(cursor.DEKID, "dek") {
		return "", ErrSecretInputInvalid
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", ErrSecretInputInvalid
	}
	return cursor.DEKID, nil
}

func validateVaultKeyRotationID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !validSecretGeneratedID(value, "vkr") {
		return "", ErrVaultKeyRotationNotFound
	}
	return value, nil
}

func vaultKeyRotationMutationFingerprint(value any) (string, error) {
	digest, err := secretMutationFingerprint(value)
	if err != nil {
		return "", fmt.Errorf("fingerprint vault key rotation mutation: %w", err)
	}
	return digest, nil
}
