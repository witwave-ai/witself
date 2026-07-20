package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/sealed"
)

const (
	// VaultEnrollmentStatePending is a request awaiting source approval.
	VaultEnrollmentStatePending = "pending"
	// VaultEnrollmentStateApproved is an approved transfer awaiting consumption.
	VaultEnrollmentStateApproved = "approved"
	// VaultEnrollmentStateConsumed is a transfer installed by its recipient.
	VaultEnrollmentStateConsumed = "consumed"
	// VaultEnrollmentStateCancelled is a transfer cancelled before consumption.
	VaultEnrollmentStateCancelled = "cancelled"
	// VaultEnrollmentStateExpired is a transfer whose bounded lifetime elapsed.
	VaultEnrollmentStateExpired = "expired"

	// VaultEnrollmentTargetKeyAlgorithm identifies recipient exchange keys.
	VaultEnrollmentTargetKeyAlgorithm = "X25519_RAW_32_BASE64URL_V1"
	// VaultEnrollmentTransferAlgorithm identifies recipient-bound AVK envelopes.
	VaultEnrollmentTransferAlgorithm = "X25519_HKDF_SHA256_AES_256_GCM_V1"

	minVaultEnrollmentTTL        = time.Minute
	maxVaultEnrollmentTTL        = time.Hour
	maxActiveVaultEnrollments    = 5
	maxVaultEnrollmentCiphertext = 4096
)

var (
	// ErrVaultEnrollmentNotFound means the scoped enrollment does not exist.
	ErrVaultEnrollmentNotFound = errors.New("vault key enrollment not found")
	// ErrVaultEnrollmentConflict means the enrollment changed concurrently.
	ErrVaultEnrollmentConflict = errors.New("vault key enrollment changed concurrently")
	// ErrVaultEnrollmentExpired means the enrollment can no longer be completed.
	ErrVaultEnrollmentExpired = errors.New("vault key enrollment expired")
	// ErrVaultEnrollmentLimit means the owner has too many active enrollments.
	ErrVaultEnrollmentLimit = errors.New("too many active vault key enrollments")
	// ErrVaultEnrollmentProof means the recipient consume proof is invalid.
	ErrVaultEnrollmentProof = errors.New("vault key enrollment proof is invalid")
)

// VaultKeyEnrollment is the public, value-free state of one short-lived
// installation enrollment. Transfer ciphertext is deliberately available only
// through AccessVaultKeyEnrollmentTransfer.
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

// VaultKeyEnrollmentTransfer is the opaque, recipient-bound capsule returned
// only to a completing client. It contains no plaintext AVK.
type VaultKeyEnrollmentTransfer struct {
	Enrollment               VaultKeyEnrollment `json:"enrollment"`
	SourceEphemeralPublicKey string             `json:"source_ephemeral_public_key"`
	Ciphertext               []byte             `json:"ciphertext"`
	ConsumeCommitment        string             `json:"consume_commitment"`
}

// CreateVaultKeyEnrollmentInput contains the target installation's public
// enrollment material and requested expiry.
type CreateVaultKeyEnrollmentInput struct {
	ID                 string
	TargetLocationID   string
	TargetLocationName string
	TargetPublicKey    string
	TargetKeyAlgorithm string
	PairingCommitment  string
	ExpiresAt          time.Time
	IdempotencyKey     string
}

// ApproveVaultKeyEnrollmentInput contains the encrypted transfer produced by
// an installation that already holds the AVK and its concurrency fence.
type ApproveVaultKeyEnrollmentInput struct {
	ExpectedRowVersion       int64
	SourceLocationID         string
	SourceEphemeralPublicKey string
	TransferCiphertext       []byte
	TransferAlgorithm        string
	ConsumeCommitment        string
	IdempotencyKey           string
}

// ConsumeVaultKeyEnrollmentInput proves that the target installation decrypted
// its transfer and fences the state transition to consumed.
type ConsumeVaultKeyEnrollmentInput struct {
	ExpectedRowVersion int64
	TargetLocationID   string
	ConsumeProof       []byte
	IdempotencyKey     string
}

// CancelVaultKeyEnrollmentInput fences cancellation of an active enrollment.
type CancelVaultKeyEnrollmentInput struct {
	ExpectedRowVersion int64
	IdempotencyKey     string
}

// VaultKeyEnrollmentListOptions filters and bounds an enrollment listing.
type VaultKeyEnrollmentListOptions struct {
	State string
	Limit int
}

// CreateVaultKeyEnrollment creates a short-lived request for another
// installation to receive the principal agent's current vault key.
func (s *Store) CreateVaultKeyEnrollment(ctx context.Context, p Principal, in CreateVaultKeyEnrollmentInput) (VaultKeyEnrollment, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	var err error
	if in, err = normalizeCreateVaultKeyEnrollmentInput(in); err != nil {
		return VaultKeyEnrollment{}, err
	}
	requestForHash := in
	requestForHash.IdempotencyKey = ""
	requestHash, err := secretMutationFingerprint(requestForHash)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := lockSecretOwnerAgentTx(ctx, tx, p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := lockSecretIdempotencyKey(ctx, tx, p, "enrollment_request", secretIdempotencyKeyHash(in.IdempotencyKey)); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if receipt, replayed, err := replayVaultEnrollmentReceiptTx(ctx, tx, p, "enrollment_request", in.IdempotencyKey, requestHash); err != nil {
		return VaultKeyEnrollment{}, err
	} else if replayed {
		value, err := getVaultKeyEnrollmentTx(ctx, tx, p, receipt.enrollmentID, false)
		if err != nil {
			return VaultKeyEnrollment{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return VaultKeyEnrollment{}, err
		}
		return value, nil
	}
	if err := ensureNoOpenVaultKeyRotationTx(ctx, tx, p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := expireVaultKeyEnrollmentsTx(ctx, tx, p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	var active int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM agent_vault_key_enrollments
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND lifecycle_state IN ('pending','approved')
		   AND expires_at > clock_timestamp()`, p.AccountID, p.RealmID, p.ID).Scan(&active); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if active >= maxActiveVaultEnrollments {
		return VaultKeyEnrollment{}, ErrVaultEnrollmentLimit
	}
	key, err := lockCurrentVaultKeyTx(ctx, tx, p)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	if key == nil {
		return VaultKeyEnrollment{}, ErrVaultKeyUnavailable
	}
	var canonicalExpiry time.Time
	err = tx.QueryRow(ctx, `
		SELECT date_trunc('second', $1::timestamptz)
		 WHERE $1::timestamptz >= clock_timestamp() + $2::bigint * interval '1 second'
		   AND $1::timestamptz <= clock_timestamp() + $3::bigint * interval '1 second'`,
		in.ExpiresAt, int64(minVaultEnrollmentTTL/time.Second), int64(maxVaultEnrollmentTTL/time.Second)).Scan(&canonicalExpiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyEnrollment{}, ErrSecretInputInvalid
	}
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO agent_vault_key_enrollments
		       (id, account_id, realm_id, owner_agent_id,
		        vault_key_id, vault_key_version,
		        target_location_id, target_location_name, target_public_key,
		        target_key_algorithm, pairing_commitment, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		in.ID, p.AccountID, p.RealmID, p.ID, key.ID, key.KeyVersion,
		in.TargetLocationID, in.TargetLocationName, in.TargetPublicKey,
		in.TargetKeyAlgorithm, in.PairingCommitment, canonicalExpiry)
	if err != nil {
		if secretUniqueViolation(err) {
			return VaultKeyEnrollment{}, ErrVaultEnrollmentConflict
		}
		return VaultKeyEnrollment{}, fmt.Errorf("create vault enrollment: %w", err)
	}
	value, err := getVaultKeyEnrollmentTx(ctx, tx, p, in.ID, false)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := insertVaultEnrollmentReceiptTx(ctx, tx, p, "enrollment_request", in.IdempotencyKey, requestHash, in.ID, value.RowVersion); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := logEventTx(ctx, tx, EventInput{AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
		Verb: VerbVaultEnrollmentRequested, Metadata: vaultEnrollmentEventMetadata(value, false)}); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return VaultKeyEnrollment{}, err
	}
	return value, nil
}

// ApproveVaultKeyEnrollment stores the recipient-bound encrypted transfer for
// a pending enrollment without exposing the AVK to the backend.
func (s *Store) ApproveVaultKeyEnrollment(ctx context.Context, p Principal, enrollmentID string, in ApproveVaultKeyEnrollmentInput) (VaultKeyEnrollment, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	enrollmentID = strings.TrimSpace(enrollmentID)
	var err error
	if in, err = normalizeApproveVaultKeyEnrollmentInput(in); err != nil || !validSecretGeneratedID(enrollmentID, "enr") {
		return VaultKeyEnrollment{}, ErrSecretInputInvalid
	}
	requestForHash := in
	requestForHash.IdempotencyKey = ""
	requestHash, err := secretMutationFingerprint(struct {
		EnrollmentID string
		Input        ApproveVaultKeyEnrollmentInput
	}{enrollmentID, requestForHash})
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := lockSecretOwnerAgentTx(ctx, tx, p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := lockSecretIdempotencyKey(ctx, tx, p, "enrollment_approve", secretIdempotencyKeyHash(in.IdempotencyKey)); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if receipt, replayed, err := replayVaultEnrollmentReceiptTx(ctx, tx, p, "enrollment_approve", in.IdempotencyKey, requestHash); err != nil {
		return VaultKeyEnrollment{}, err
	} else if replayed {
		value, err := getVaultKeyEnrollmentTx(ctx, tx, p, receipt.enrollmentID, false)
		if err != nil {
			return VaultKeyEnrollment{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return VaultKeyEnrollment{}, err
		}
		return value, nil
	}
	if err := ensureNoOpenVaultKeyRotationTx(ctx, tx, p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := expireVaultKeyEnrollmentsTx(ctx, tx, p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	value, err := getVaultKeyEnrollmentTx(ctx, tx, p, enrollmentID, true)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	if value.LifecycleState == VaultEnrollmentStateExpired {
		return VaultKeyEnrollment{}, ErrVaultEnrollmentExpired
	}
	if value.LifecycleState != VaultEnrollmentStatePending || value.RowVersion != in.ExpectedRowVersion ||
		value.TargetLocationID == in.SourceLocationID {
		return VaultKeyEnrollment{}, ErrVaultEnrollmentConflict
	}
	key, err := lockCurrentVaultKeyTx(ctx, tx, p)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	if key == nil || key.ID != value.VaultKeyID || key.KeyVersion != value.VaultKeyVersion {
		return VaultKeyEnrollment{}, ErrVaultKeyMismatch
	}
	row := tx.QueryRow(ctx, `
		UPDATE agent_vault_key_enrollments
		   SET lifecycle_state='approved', source_location_id=$5,
		       source_ephemeral_public_key=$6, transfer_ciphertext=$7,
		       transfer_algorithm=$8, consume_commitment=$9,
		       approved_at=clock_timestamp(), row_version=row_version+1
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4
		   AND lifecycle_state='pending' AND row_version=$10
		   AND expires_at > clock_timestamp()
		 RETURNING row_version`, p.AccountID, p.RealmID, p.ID, enrollmentID,
		in.SourceLocationID, in.SourceEphemeralPublicKey, in.TransferCiphertext,
		in.TransferAlgorithm, in.ConsumeCommitment, in.ExpectedRowVersion)
	var revision int64
	if err := row.Scan(&revision); errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyEnrollment{}, ErrVaultEnrollmentConflict
	} else if err != nil {
		return VaultKeyEnrollment{}, err
	}
	value, err = getVaultKeyEnrollmentTx(ctx, tx, p, enrollmentID, false)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := insertVaultEnrollmentReceiptTx(ctx, tx, p, "enrollment_approve", in.IdempotencyKey, requestHash, enrollmentID, revision); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := logEventTx(ctx, tx, EventInput{AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
		Verb: VerbVaultEnrollmentApproved, Metadata: vaultEnrollmentEventMetadata(value, true)}); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return VaultKeyEnrollment{}, err
	}
	return value, nil
}

// AccessVaultKeyEnrollmentTransfer returns an approved encrypted transfer only
// to the enrollment's designated target installation.
func (s *Store) AccessVaultKeyEnrollmentTransfer(ctx context.Context, p Principal, enrollmentID, targetLocationID string) (VaultKeyEnrollmentTransfer, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return VaultKeyEnrollmentTransfer{}, err
	}
	enrollmentID, targetLocationID = strings.TrimSpace(enrollmentID), strings.TrimSpace(targetLocationID)
	if !validSecretGeneratedID(enrollmentID, "enr") || !validSecretGeneratedID(targetLocationID, "loc") {
		return VaultKeyEnrollmentTransfer{}, ErrVaultEnrollmentNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VaultKeyEnrollmentTransfer{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := expireVaultKeyEnrollmentsTx(ctx, tx, p); err != nil {
		return VaultKeyEnrollmentTransfer{}, err
	}
	value, err := getVaultKeyEnrollmentTx(ctx, tx, p, enrollmentID, true)
	if err != nil {
		return VaultKeyEnrollmentTransfer{}, err
	}
	if value.TargetLocationID != targetLocationID {
		return VaultKeyEnrollmentTransfer{}, ErrVaultEnrollmentNotFound
	}
	if value.LifecycleState == VaultEnrollmentStateExpired {
		return VaultKeyEnrollmentTransfer{}, ErrVaultEnrollmentExpired
	}
	if value.LifecycleState != VaultEnrollmentStateApproved {
		return VaultKeyEnrollmentTransfer{}, ErrVaultEnrollmentConflict
	}
	var out VaultKeyEnrollmentTransfer
	out.Enrollment = value
	err = tx.QueryRow(ctx, `
		SELECT source_ephemeral_public_key, transfer_ciphertext, consume_commitment
		  FROM agent_vault_key_enrollments
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4
		   AND lifecycle_state='approved' AND expires_at > clock_timestamp()
		 FOR SHARE`, p.AccountID, p.RealmID, p.ID, enrollmentID).Scan(
		&out.SourceEphemeralPublicKey, &out.Ciphertext, &out.ConsumeCommitment)
	if errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyEnrollmentTransfer{}, ErrVaultEnrollmentConflict
	}
	if err != nil {
		return VaultKeyEnrollmentTransfer{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return VaultKeyEnrollmentTransfer{}, err
	}
	return out, nil
}

// ConsumeVaultKeyEnrollment verifies possession of the transferred AVK and
// marks an approved enrollment consumed.
func (s *Store) ConsumeVaultKeyEnrollment(ctx context.Context, p Principal, enrollmentID string, in ConsumeVaultKeyEnrollmentInput) (VaultKeyEnrollment, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	enrollmentID = strings.TrimSpace(enrollmentID)
	var err error
	if in, err = normalizeConsumeVaultKeyEnrollmentInput(in); err != nil || !validSecretGeneratedID(enrollmentID, "enr") {
		return VaultKeyEnrollment{}, ErrSecretInputInvalid
	}
	requestForHash := in
	requestForHash.IdempotencyKey = ""
	requestHash, err := secretMutationFingerprint(struct {
		EnrollmentID string
		Input        ConsumeVaultKeyEnrollmentInput
	}{enrollmentID, requestForHash})
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := lockSecretOwnerAgentTx(ctx, tx, p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := lockSecretIdempotencyKey(ctx, tx, p, "enrollment_consume", secretIdempotencyKeyHash(in.IdempotencyKey)); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if receipt, replayed, err := replayVaultEnrollmentReceiptTx(ctx, tx, p, "enrollment_consume", in.IdempotencyKey, requestHash); err != nil {
		return VaultKeyEnrollment{}, err
	} else if replayed {
		value, err := getVaultKeyEnrollmentTx(ctx, tx, p, receipt.enrollmentID, false)
		if err != nil {
			return VaultKeyEnrollment{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return VaultKeyEnrollment{}, err
		}
		return value, nil
	}
	if err := ensureNoOpenVaultKeyRotationTx(ctx, tx, p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := expireVaultKeyEnrollmentsTx(ctx, tx, p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	value, err := getVaultKeyEnrollmentTx(ctx, tx, p, enrollmentID, true)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	if value.LifecycleState == VaultEnrollmentStateExpired {
		return VaultKeyEnrollment{}, ErrVaultEnrollmentExpired
	}
	if value.LifecycleState != VaultEnrollmentStateApproved || value.RowVersion != in.ExpectedRowVersion ||
		value.TargetLocationID != in.TargetLocationID {
		return VaultKeyEnrollment{}, ErrVaultEnrollmentConflict
	}
	var commitment string
	if err := tx.QueryRow(ctx, `
		SELECT consume_commitment
		  FROM agent_vault_key_enrollments
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4
		 FOR UPDATE`, p.AccountID, p.RealmID, p.ID, enrollmentID).Scan(&commitment); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if !sealed.VerifyAVKEnrollmentConsumeProof(enrollmentID, commitment, in.ConsumeProof) {
		return VaultKeyEnrollment{}, ErrVaultEnrollmentProof
	}
	var revision int64
	err = tx.QueryRow(ctx, `
		UPDATE agent_vault_key_enrollments
		   SET lifecycle_state='consumed', source_ephemeral_public_key=NULL,
		       transfer_ciphertext=NULL, transfer_algorithm=NULL,
		       consume_commitment=NULL, consumed_at=clock_timestamp(),
		       row_version=row_version+1
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4
		   AND lifecycle_state='approved' AND row_version=$5
		   AND expires_at > clock_timestamp()
		 RETURNING row_version`, p.AccountID, p.RealmID, p.ID, enrollmentID, in.ExpectedRowVersion).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyEnrollment{}, ErrVaultEnrollmentConflict
	}
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	value, err = getVaultKeyEnrollmentTx(ctx, tx, p, enrollmentID, false)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := insertVaultEnrollmentReceiptTx(ctx, tx, p, "enrollment_consume", in.IdempotencyKey, requestHash, enrollmentID, revision); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := logEventTx(ctx, tx, EventInput{AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
		Verb: VerbVaultEnrollmentConsumed, Metadata: vaultEnrollmentEventMetadata(value, true)}); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return VaultKeyEnrollment{}, err
	}
	return value, nil
}

// CancelVaultKeyEnrollment cancels a pending or approved enrollment at the
// expected row version.
func (s *Store) CancelVaultKeyEnrollment(ctx context.Context, p Principal, enrollmentID string, in CancelVaultKeyEnrollmentInput) (VaultKeyEnrollment, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	enrollmentID = strings.TrimSpace(enrollmentID)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if !validSecretGeneratedID(enrollmentID, "enr") || in.ExpectedRowVersion < 1 ||
		len(in.IdempotencyKey) < 1 || len(in.IdempotencyKey) > maxSecretIdempotencyKeyBytes {
		return VaultKeyEnrollment{}, ErrSecretInputInvalid
	}
	requestHash, err := secretMutationFingerprint(struct {
		EnrollmentID string
		Revision     int64
	}{enrollmentID, in.ExpectedRowVersion})
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForSafetyWrite(ctx, tx, p.AccountID); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := lockSecretOwnerAgentTx(ctx, tx, p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := lockSecretIdempotencyKey(ctx, tx, p, "enrollment_cancel", secretIdempotencyKeyHash(in.IdempotencyKey)); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if receipt, replayed, err := replayVaultEnrollmentReceiptTx(ctx, tx, p, "enrollment_cancel", in.IdempotencyKey, requestHash); err != nil {
		return VaultKeyEnrollment{}, err
	} else if replayed {
		value, err := getVaultKeyEnrollmentTx(ctx, tx, p, receipt.enrollmentID, false)
		if err != nil {
			return VaultKeyEnrollment{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return VaultKeyEnrollment{}, err
		}
		return value, nil
	}
	if err := expireVaultKeyEnrollmentsTx(ctx, tx, p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	value, err := getVaultKeyEnrollmentTx(ctx, tx, p, enrollmentID, true)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	if value.LifecycleState == VaultEnrollmentStateExpired {
		return VaultKeyEnrollment{}, ErrVaultEnrollmentExpired
	}
	if (value.LifecycleState != VaultEnrollmentStatePending && value.LifecycleState != VaultEnrollmentStateApproved) ||
		value.RowVersion != in.ExpectedRowVersion {
		return VaultKeyEnrollment{}, ErrVaultEnrollmentConflict
	}
	var revision int64
	err = tx.QueryRow(ctx, `
		UPDATE agent_vault_key_enrollments
		   SET lifecycle_state='cancelled', source_ephemeral_public_key=NULL,
		       transfer_ciphertext=NULL, transfer_algorithm=NULL,
		       consume_commitment=NULL, cancelled_at=clock_timestamp(),
		       row_version=row_version+1
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4
		   AND lifecycle_state IN ('pending','approved') AND row_version=$5
		 RETURNING row_version`, p.AccountID, p.RealmID, p.ID, enrollmentID, in.ExpectedRowVersion).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyEnrollment{}, ErrVaultEnrollmentConflict
	}
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	value, err = getVaultKeyEnrollmentTx(ctx, tx, p, enrollmentID, false)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := insertVaultEnrollmentReceiptTx(ctx, tx, p, "enrollment_cancel", in.IdempotencyKey, requestHash, enrollmentID, revision); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := logEventTx(ctx, tx, EventInput{AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
		Verb: VerbVaultEnrollmentCancelled, Metadata: vaultEnrollmentEventMetadata(value, false)}); err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return VaultKeyEnrollment{}, err
	}
	return value, nil
}

// GetVaultKeyEnrollment returns value-free lifecycle metadata for one
// enrollment owned by the principal agent.
func (s *Store) GetVaultKeyEnrollment(ctx context.Context, p Principal, enrollmentID string) (VaultKeyEnrollment, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	enrollmentID = strings.TrimSpace(enrollmentID)
	if !validSecretGeneratedID(enrollmentID, "enr") {
		return VaultKeyEnrollment{}, ErrVaultEnrollmentNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := expireVaultKeyEnrollmentsTx(ctx, tx, p); err != nil {
		return VaultKeyEnrollment{}, err
	}
	value, err := getVaultKeyEnrollmentTx(ctx, tx, p, enrollmentID, false)
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return VaultKeyEnrollment{}, err
	}
	return value, nil
}

// ListVaultKeyEnrollments returns value-free enrollment metadata owned by the
// principal agent, optionally filtered by lifecycle state.
func (s *Store) ListVaultKeyEnrollments(ctx context.Context, p Principal, opts VaultKeyEnrollmentListOptions) ([]VaultKeyEnrollment, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return nil, err
	}
	opts.State = strings.ToLower(strings.TrimSpace(opts.State))
	if opts.Limit == 0 {
		opts.Limit = 25
	}
	if opts.Limit < 1 || opts.Limit > 100 || (opts.State != "" && opts.State != "active" &&
		opts.State != VaultEnrollmentStatePending && opts.State != VaultEnrollmentStateApproved &&
		opts.State != VaultEnrollmentStateConsumed && opts.State != VaultEnrollmentStateCancelled &&
		opts.State != VaultEnrollmentStateExpired) {
		return nil, ErrSecretInputInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := expireVaultKeyEnrollmentsTx(ctx, tx, p); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT id
		  FROM agent_vault_key_enrollments
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND ($4='' OR ($4='active' AND lifecycle_state IN ('pending','approved')) OR lifecycle_state=$4)
		 ORDER BY created_at DESC, id DESC
		 LIMIT $5`, p.AccountID, p.RealmID, p.ID, opts.State, opts.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	items := make([]VaultKeyEnrollment, 0, len(ids))
	for _, id := range ids {
		item, err := getVaultKeyEnrollmentTx(ctx, tx, p, id, false)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func normalizeCreateVaultKeyEnrollmentInput(in CreateVaultKeyEnrollmentInput) (CreateVaultKeyEnrollmentInput, error) {
	in.ID = strings.TrimSpace(in.ID)
	in.TargetLocationID = strings.TrimSpace(in.TargetLocationID)
	in.TargetLocationName = strings.TrimSpace(in.TargetLocationName)
	in.TargetKeyAlgorithm = strings.TrimSpace(in.TargetKeyAlgorithm)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if !validSecretGeneratedID(in.ID, "enr") || !validSecretGeneratedID(in.TargetLocationID, "loc") ||
		len(in.TargetLocationName) > 256 || strings.ContainsRune(in.TargetLocationName, '\x00') || !utf8.ValidString(in.TargetLocationName) ||
		!validVaultEnrollmentPublicKey(in.TargetPublicKey) || in.TargetKeyAlgorithm != VaultEnrollmentTargetKeyAlgorithm ||
		!validFactSHA256(in.PairingCommitment) || in.ExpiresAt.IsZero() ||
		!in.ExpiresAt.Equal(in.ExpiresAt.Truncate(time.Second)) || len(in.IdempotencyKey) < 1 ||
		len(in.IdempotencyKey) > maxSecretIdempotencyKeyBytes {
		return CreateVaultKeyEnrollmentInput{}, ErrSecretInputInvalid
	}
	return in, nil
}

func normalizeApproveVaultKeyEnrollmentInput(in ApproveVaultKeyEnrollmentInput) (ApproveVaultKeyEnrollmentInput, error) {
	in.SourceLocationID = strings.TrimSpace(in.SourceLocationID)
	in.TransferAlgorithm = strings.TrimSpace(in.TransferAlgorithm)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.ExpectedRowVersion < 1 || !validSecretGeneratedID(in.SourceLocationID, "loc") ||
		!validVaultEnrollmentPublicKey(in.SourceEphemeralPublicKey) || len(in.TransferCiphertext) < 64 ||
		len(in.TransferCiphertext) > maxVaultEnrollmentCiphertext ||
		in.TransferAlgorithm != VaultEnrollmentTransferAlgorithm || !validFactSHA256(in.ConsumeCommitment) ||
		len(in.IdempotencyKey) < 1 || len(in.IdempotencyKey) > maxSecretIdempotencyKeyBytes {
		return ApproveVaultKeyEnrollmentInput{}, ErrSecretInputInvalid
	}
	in.TransferCiphertext = append([]byte(nil), in.TransferCiphertext...)
	return in, nil
}

func normalizeConsumeVaultKeyEnrollmentInput(in ConsumeVaultKeyEnrollmentInput) (ConsumeVaultKeyEnrollmentInput, error) {
	in.TargetLocationID = strings.TrimSpace(in.TargetLocationID)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.ExpectedRowVersion < 1 || !validSecretGeneratedID(in.TargetLocationID, "loc") ||
		len(in.ConsumeProof) != 32 || len(in.IdempotencyKey) < 1 ||
		len(in.IdempotencyKey) > maxSecretIdempotencyKeyBytes {
		return ConsumeVaultKeyEnrollmentInput{}, ErrSecretInputInvalid
	}
	in.ConsumeProof = append([]byte(nil), in.ConsumeProof...)
	return in, nil
}

type vaultEnrollmentReceipt struct {
	enrollmentID string
	revision     int64
}

func replayVaultEnrollmentReceiptTx(ctx context.Context, tx pgx.Tx, p Principal, operation, idempotencyKey, requestHash string) (vaultEnrollmentReceipt, bool, error) {
	keyHash := secretIdempotencyKeyHash(idempotencyKey)
	var receipt vaultEnrollmentReceipt
	var storedHash string
	err := tx.QueryRow(ctx, `
		SELECT request_hash, enrollment_id, result_revision
		  FROM vault_key_enrollment_receipts
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND operation=$4 AND idempotency_key_hash=$5`,
		p.AccountID, p.RealmID, p.ID, operation, keyHash).Scan(
		&storedHash, &receipt.enrollmentID, &receipt.revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return vaultEnrollmentReceipt{}, false, nil
	}
	if err != nil {
		return vaultEnrollmentReceipt{}, false, err
	}
	if storedHash != requestHash {
		return vaultEnrollmentReceipt{}, false, ErrSecretIdempotencyConflict
	}
	return receipt, true, nil
}

func insertVaultEnrollmentReceiptTx(ctx context.Context, tx pgx.Tx, p Principal, operation, idempotencyKey, requestHash, enrollmentID string, revision int64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO vault_key_enrollment_receipts
		       (account_id, realm_id, owner_agent_id, operation,
		        idempotency_key_hash, request_hash, enrollment_id, result_revision)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, p.AccountID, p.RealmID, p.ID,
		operation, secretIdempotencyKeyHash(idempotencyKey), requestHash, enrollmentID, revision)
	if err != nil {
		if secretUniqueViolation(err) {
			return ErrSecretIdempotencyConflict
		}
		return err
	}
	return nil
}

func getVaultKeyEnrollmentTx(ctx context.Context, tx pgx.Tx, p Principal, enrollmentID string, lock bool) (VaultKeyEnrollment, error) {
	query := `
		SELECT e.id, e.account_id, e.realm_id, e.owner_agent_id,
		       e.vault_key_id, e.vault_key_version, k.algorithm, k.fingerprint,
		       e.target_location_id, e.target_location_name, e.target_public_key,
		       e.target_key_algorithm, e.pairing_commitment, e.lifecycle_state,
		       coalesce(e.source_location_id, ''), coalesce(e.transfer_algorithm, ''),
		       e.row_version, e.created_at, e.expires_at, e.approved_at, e.consumed_at,
		       e.cancelled_at, e.expired_at
		  FROM agent_vault_key_enrollments e
		  JOIN agent_vault_keys k
		    ON k.account_id=e.account_id AND k.realm_id=e.realm_id
		   AND k.owner_agent_id=e.owner_agent_id AND k.id=e.vault_key_id
		   AND k.key_version=e.vault_key_version
		 WHERE e.account_id=$1 AND e.realm_id=$2 AND e.owner_agent_id=$3 AND e.id=$4`
	if lock {
		query += " FOR UPDATE"
	}
	var out VaultKeyEnrollment
	err := tx.QueryRow(ctx, query, p.AccountID, p.RealmID, p.ID, enrollmentID).Scan(
		&out.ID, &out.AccountID, &out.RealmID, &out.OwnerAgentID,
		&out.VaultKeyID, &out.VaultKeyVersion, &out.VaultKeyAlgorithm, &out.VaultKeyFingerprint,
		&out.TargetLocationID, &out.TargetLocationName, &out.TargetPublicKey,
		&out.TargetKeyAlgorithm, &out.PairingCommitment, &out.LifecycleState,
		&out.SourceLocationID, &out.TransferAlgorithm, &out.RowVersion,
		&out.CreatedAt, &out.ExpiresAt, &out.ApprovedAt, &out.ConsumedAt,
		&out.CancelledAt, &out.ExpiredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyEnrollment{}, ErrVaultEnrollmentNotFound
	}
	if err != nil {
		return VaultKeyEnrollment{}, err
	}
	return out, nil
}

func expireVaultKeyEnrollmentsTx(ctx context.Context, tx pgx.Tx, p Principal) error {
	rows, err := tx.Query(ctx, `
		UPDATE agent_vault_key_enrollments
		   SET lifecycle_state='expired', source_ephemeral_public_key=NULL,
		       transfer_ciphertext=NULL, transfer_algorithm=NULL,
		       consume_commitment=NULL, expired_at=clock_timestamp(),
		       row_version=row_version+1
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND lifecycle_state IN ('pending','approved')
		   AND expires_at <= clock_timestamp()
		 RETURNING id, vault_key_id, vault_key_version, target_location_id`,
		p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return err
	}
	type expiredEnrollment struct {
		enrollmentID     string
		keyID            string
		keyVersion       int64
		targetLocationID string
	}
	expired := make([]expiredEnrollment, 0)
	for rows.Next() {
		var value expiredEnrollment
		if err := rows.Scan(&value.enrollmentID, &value.keyID, &value.keyVersion, &value.targetLocationID); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	// pgx does not permit another statement on this transaction connection
	// while UPDATE ... RETURNING rows remain open. Materialize the bounded
	// expiry result first, then close it before writing per-enrollment audit.
	rows.Close()
	for _, value := range expired {
		if err := logEventTx(ctx, tx, EventInput{AccountID: p.AccountID, ActorKind: ActorSystem,
			Verb: VerbVaultEnrollmentExpired, Metadata: map[string]any{
				"agent_id": p.ID, "enrollment_id": value.enrollmentID, "key_id": value.keyID,
				"key_version": fmt.Sprint(value.keyVersion), "target_location_id": value.targetLocationID,
			}}); err != nil {
			return err
		}
	}
	return nil
}

// expireAccountVaultKeyEnrollmentsTx materializes every due enrollment in an
// account while ExportAccount holds the account write-fence. Keeping this
// account-scoped variant inside the export transaction guarantees that an
// archive cannot contain an expired-but-still-authoritative transfer capsule.
func expireAccountVaultKeyEnrollmentsTx(ctx context.Context, tx pgx.Tx, accountID string) error {
	rows, err := tx.Query(ctx, `
		UPDATE agent_vault_key_enrollments
		   SET lifecycle_state='expired', source_ephemeral_public_key=NULL,
		       transfer_ciphertext=NULL, transfer_algorithm=NULL,
		       consume_commitment=NULL, expired_at=clock_timestamp(),
		       row_version=row_version+1
		 WHERE account_id=$1
		   AND lifecycle_state IN ('pending','approved')
		   AND expires_at <= clock_timestamp()
		 RETURNING owner_agent_id, id, vault_key_id, vault_key_version,
		           target_location_id`, accountID)
	if err != nil {
		return err
	}
	type expiredEnrollment struct {
		agentID          string
		enrollmentID     string
		keyID            string
		keyVersion       int64
		targetLocationID string
	}
	expired := make([]expiredEnrollment, 0)
	for rows.Next() {
		var value expiredEnrollment
		if err := rows.Scan(&value.agentID, &value.enrollmentID, &value.keyID, &value.keyVersion, &value.targetLocationID); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	// The account-scoped export fence can expire many agents at once, but all
	// returned rows must still be closed before the first audit INSERT on pgx.
	rows.Close()
	for _, value := range expired {
		if err := logEventTx(ctx, tx, EventInput{AccountID: accountID, ActorKind: ActorSystem,
			Verb: VerbVaultEnrollmentExpired, Metadata: map[string]any{
				"agent_id": value.agentID, "enrollment_id": value.enrollmentID, "key_id": value.keyID,
				"key_version": fmt.Sprint(value.keyVersion), "target_location_id": value.targetLocationID,
			}}); err != nil {
			return err
		}
	}
	return nil
}

func vaultEnrollmentEventMetadata(value VaultKeyEnrollment, withSource bool) map[string]any {
	out := map[string]any{
		"agent_id": value.OwnerAgentID, "enrollment_id": value.ID,
		"key_id": value.VaultKeyID, "key_version": fmt.Sprint(value.VaultKeyVersion),
		"target_location_id": value.TargetLocationID,
	}
	if withSource {
		out["source_location_id"] = value.SourceLocationID
	}
	return out
}

func validVaultEnrollmentPublicKey(value string) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(32) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != value {
		clear(raw)
		return false
	}
	clear(raw)
	return true
}
