package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RegisterVaultKey binds public AVK identity to the authenticated agent. It
// can never accept or recover key bytes.
func (s *Store) RegisterVaultKey(ctx context.Context, p Principal, in RegisterVaultKeyInput) (VaultKeyBinding, SecretMutationReceipt, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return VaultKeyBinding{}, SecretMutationReceipt{}, err
	}
	in, err := normalizeRegisterVaultKeyInput(in)
	if err != nil {
		return VaultKeyBinding{}, SecretMutationReceipt{}, err
	}
	requestHash, err := secretMutationFingerprint(struct {
		ID          string `json:"id"`
		KeyVersion  int64  `json:"key_version"`
		Algorithm   string `json:"algorithm"`
		Fingerprint string `json:"fingerprint"`
	}{in.ID, in.KeyVersion, in.Algorithm, in.Fingerprint})
	if err != nil {
		return VaultKeyBinding{}, SecretMutationReceipt{}, err
	}
	keyHash := secretIdempotencyKeyHash(in.IdempotencyKey)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VaultKeyBinding{}, SecretMutationReceipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return VaultKeyBinding{}, SecretMutationReceipt{}, err
	}
	// Lock the stable owner row, not the possibly absent current-key row. This
	// serializes first registration for one agent even when concurrent callers
	// use different retry keys, while registrations for different agents remain
	// independent.
	if err := lockSecretOwnerAgentTx(ctx, tx, p); err != nil {
		return VaultKeyBinding{}, SecretMutationReceipt{}, err
	}
	if err := lockSecretIdempotencyKey(ctx, tx, p, "key_register", keyHash); err != nil {
		return VaultKeyBinding{}, SecretMutationReceipt{}, err
	}
	if receipt, replayed, err := replaySecretReceiptTx(ctx, tx, p,
		"key_register", keyHash, requestHash); err != nil {
		return VaultKeyBinding{}, SecretMutationReceipt{}, err
	} else if replayed {
		key, err := getVaultKeyByIDTx(ctx, tx, p, receipt.TargetID)
		if err != nil {
			return VaultKeyBinding{}, SecretMutationReceipt{}, err
		}
		receipt.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return VaultKeyBinding{}, SecretMutationReceipt{}, err
		}
		return key, receipt, nil
	}

	existing, err := lockCurrentVaultKeyTx(ctx, tx, p)
	if err != nil {
		return VaultKeyBinding{}, SecretMutationReceipt{}, err
	}
	inserted := false
	var key VaultKeyBinding
	if existing != nil {
		if existing.ID != in.ID || existing.KeyVersion != in.KeyVersion ||
			existing.Algorithm != in.Algorithm || existing.Fingerprint != in.Fingerprint {
			return VaultKeyBinding{}, SecretMutationReceipt{}, ErrVaultKeyConflict
		}
		key = *existing
	} else {
		err = tx.QueryRow(ctx, `
			INSERT INTO agent_vault_keys
			       (id, account_id, realm_id, owner_agent_id, key_version,
			        algorithm, fingerprint, lifecycle_state)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'current')
			RETURNING id, account_id, realm_id, owner_agent_id, key_version,
			          algorithm, fingerprint, lifecycle_state, row_version,
			          created_at, retired_at`, in.ID, p.AccountID, p.RealmID, p.ID,
			in.KeyVersion, in.Algorithm, in.Fingerprint).Scan(
			&key.ID, &key.AccountID, &key.RealmID, &key.OwnerAgentID,
			&key.KeyVersion, &key.Algorithm, &key.Fingerprint,
			&key.LifecycleState, &key.RowVersion, &key.CreatedAt, &key.RetiredAt)
		if err != nil {
			if secretUniqueViolation(err) {
				return VaultKeyBinding{}, SecretMutationReceipt{}, ErrVaultKeyConflict
			}
			return VaultKeyBinding{}, SecretMutationReceipt{}, fmt.Errorf("register vault key: %w", err)
		}
		inserted = true
	}
	receipt, err := insertSecretReceiptTx(ctx, tx, p, "key_register",
		keyHash, requestHash, "key_epoch", key.ID, key.RowVersion, 0)
	if err != nil {
		return VaultKeyBinding{}, SecretMutationReceipt{}, err
	}
	if inserted {
		if err := logEventTx(ctx, tx, EventInput{
			AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
			Verb: VerbVaultKeyRegistered,
			Metadata: map[string]any{
				"agent_id": p.ID, "key_id": key.ID,
				"key_version": fmt.Sprint(key.KeyVersion), "algorithm": key.Algorithm,
			},
		}); err != nil {
			return VaultKeyBinding{}, SecretMutationReceipt{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return VaultKeyBinding{}, SecretMutationReceipt{}, err
	}
	return key, receipt, nil
}

// CreateSecret atomically stores public metadata plus client-encrypted field
// envelopes. No plaintext sensitive value is accepted by this type or SQL.
func (s *Store) CreateSecret(ctx context.Context, p Principal, in CreateSecretInput) (SecretMutationResult, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return SecretMutationResult{}, err
	}
	in, err := normalizeCreateSecretInput(in)
	if err != nil {
		return SecretMutationResult{}, err
	}
	wrappingKeyID, wrappingKeyVersion, sensitiveCount, encryptedBytes, err :=
		secretCreateEnvelopeSummary(in.Fields)
	if err != nil {
		return SecretMutationResult{}, err
	}
	requestInput := in
	requestInput.IdempotencyKey = ""
	requestHash, err := secretMutationFingerprint(requestInput)
	if err != nil {
		return SecretMutationResult{}, err
	}
	keyHash := secretIdempotencyKeyHash(in.IdempotencyKey)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SecretMutationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return SecretMutationResult{}, err
	}
	if err := lockSecretIdempotencyKey(ctx, tx, p, "secret_create", keyHash); err != nil {
		return SecretMutationResult{}, err
	}
	if receipt, replayed, err := replaySecretReceiptTx(ctx, tx, p,
		"secret_create", keyHash, requestHash); err != nil {
		return SecretMutationResult{}, err
	} else if replayed {
		secret, err := getSecret(ctx, tx, p, receipt.TargetID, true, true)
		if err != nil {
			return SecretMutationResult{}, err
		}
		receipt.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return SecretMutationResult{}, err
		}
		return SecretMutationResult{Secret: secret, Receipt: receipt}, nil
	}

	if sensitiveCount > 0 {
		// The stable owner-row lock serializes the last pre-rotation sensitive
		// create against rotation snapshot creation. Once a run is open, no new
		// wrapped DEK may appear outside its immutable item set.
		if err := lockSecretOwnerAgentTx(ctx, tx, p); err != nil {
			return SecretMutationResult{}, err
		}
		if err := ensureNoOpenVaultKeyRotationTx(ctx, tx, p); err != nil {
			return SecretMutationResult{}, err
		}
		key, err := lockCurrentVaultKeyTx(ctx, tx, p)
		if err != nil {
			return SecretMutationResult{}, err
		}
		if key == nil {
			return SecretMutationResult{}, ErrVaultKeyUnavailable
		}
		if key.ID != wrappingKeyID || key.KeyVersion != wrappingKeyVersion ||
			key.Algorithm != SecretAEADAlgorithm {
			return SecretMutationResult{}, ErrVaultKeyMismatch
		}
	}
	tagsJSON, err := json.Marshal(in.Tags)
	if err != nil {
		return SecretMutationResult{}, ErrSecretInputInvalid
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secrets
		       (id, account_id, realm_id, owner_agent_id, name, description,
		        template, tags)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb)`, in.ID, p.AccountID,
		p.RealmID, p.ID, in.Name, in.Description, in.Template, string(tagsJSON)); err != nil {
		if secretUniqueViolation(err) {
			return SecretMutationResult{}, ErrSecretConflict
		}
		return SecretMutationResult{}, fmt.Errorf("insert secret metadata: %w", err)
	}
	for _, field := range in.Fields {
		if err := insertSecretFieldTx(ctx, tx, p, in.ID, field); err != nil {
			return SecretMutationResult{}, err
		}
	}
	receipt, err := insertSecretReceiptTx(ctx, tx, p, "secret_create",
		keyHash, requestHash, "secret", in.ID, 1, 0)
	if err != nil {
		return SecretMutationResult{}, err
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
		Verb: VerbSecretCreated,
		Metadata: map[string]any{
			"agent_id": p.ID, "secret_id": in.ID,
			"field_count":           fmt.Sprint(len(in.Fields)),
			"sensitive_field_count": fmt.Sprint(sensitiveCount),
		},
	}); err != nil {
		return SecretMutationResult{}, err
	}
	usageMetadata, err := json.Marshal(map[string]any{
		"field_count": len(in.Fields), "sensitive_field_count": sensitiveCount,
	})
	if err != nil {
		return SecretMutationResult{}, err
	}
	if _, err := recordUsageEventTx(ctx, tx, usageEventInput{
		AccountID: p.AccountID, RealmID: p.RealmID, AgentID: p.ID,
		Dimension: UsageDimensionStoredSecret, Quantity: 1, Unit: UsageUnitSecret,
		SubjectType: "secret", SubjectID: in.ID,
		IdempotencyKey: "secret-create:" + p.ID + ":" + keyHash + ":stored",
		Metadata:       usageMetadata,
	}); err != nil {
		return SecretMutationResult{}, err
	}
	if encryptedBytes > 0 {
		if _, err := recordUsageEventTx(ctx, tx, usageEventInput{
			AccountID: p.AccountID, RealmID: p.RealmID, AgentID: p.ID,
			Dimension: UsageDimensionEncryptedStorage, Quantity: int64(encryptedBytes), Unit: UsageUnitByte,
			SubjectType: "secret", SubjectID: in.ID,
			IdempotencyKey: "secret-create:" + p.ID + ":" + keyHash + ":encrypted-bytes",
			Metadata:       json.RawMessage(`{"material":"ciphertext_and_wrapped_dek"}`),
		}); err != nil {
			return SecretMutationResult{}, err
		}
	}
	secret, err := getSecret(ctx, tx, p, in.ID, true, false)
	if err != nil {
		return SecretMutationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SecretMutationResult{}, err
	}
	return SecretMutationResult{Secret: secret, Receipt: receipt}, nil
}

// ArchiveSecret reversibly removes one self-owned secret from ordinary list,
// show, and material-access paths.
func (s *Store) ArchiveSecret(ctx context.Context, p Principal, secretID string, in SecretLifecycleInput) (SecretMutationResult, error) {
	return s.mutateSecretLifecycle(ctx, p, secretID, "secret_archive", in)
}

// RestoreSecret returns one self-owned archived secret to ordinary reads. Its
// live name must still be available for that agent.
func (s *Store) RestoreSecret(ctx context.Context, p Principal, secretID string, in SecretLifecycleInput) (SecretMutationResult, error) {
	return s.mutateSecretLifecycle(ctx, p, secretID, "secret_restore", in)
}

func (s *Store) mutateSecretLifecycle(ctx context.Context, p Principal, secretID, operation string, in SecretLifecycleInput) (SecretMutationResult, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return SecretMutationResult{}, err
	}
	secretID = strings.TrimSpace(secretID)
	if !validSecretGeneratedID(secretID, "sec") {
		return SecretMutationResult{}, ErrSecretNotFound
	}
	in, err := normalizeSecretLifecycleInput(in)
	if err != nil {
		return SecretMutationResult{}, err
	}
	if operation != "secret_archive" && operation != "secret_restore" {
		return SecretMutationResult{}, ErrSecretInputInvalid
	}
	requestHash, err := secretMutationFingerprint(struct {
		Operation          string `json:"operation"`
		SecretID           string `json:"secret_id"`
		ExpectedRowVersion int64  `json:"expected_row_version"`
	}{operation, secretID, in.ExpectedRowVersion})
	if err != nil {
		return SecretMutationResult{}, err
	}
	keyHash := secretIdempotencyKeyHash(in.IdempotencyKey)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SecretMutationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return SecretMutationResult{}, err
	}
	if err := lockSecretIdempotencyKey(ctx, tx, p, operation, keyHash); err != nil {
		return SecretMutationResult{}, err
	}
	if receipt, replayed, err := replaySecretReceiptTx(ctx, tx, p,
		operation, keyHash, requestHash); err != nil {
		return SecretMutationResult{}, err
	} else if replayed {
		secret, err := getSecret(ctx, tx, p, receipt.TargetID, true, true)
		if err != nil {
			return SecretMutationResult{}, err
		}
		wantLifecycle := SecretLifecycleArchived
		if operation == "secret_restore" {
			wantLifecycle = SecretLifecycleActive
		}
		// A mutable secret cannot reconstruct an old lifecycle result after a
		// later state change. Fail closed instead of presenting current state as
		// the result of an older retry receipt.
		if secret.RowVersion != receipt.ResultRevision || secret.Lifecycle != wantLifecycle {
			return SecretMutationResult{}, ErrSecretConflict
		}
		receipt.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return SecretMutationResult{}, err
		}
		return SecretMutationResult{Secret: secret, Receipt: receipt}, nil
	}

	var currentRevision int64
	var archivedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT row_version, archived_at
		  FROM secrets
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND id=$4 AND deleted_at IS NULL
		 FOR UPDATE`, p.AccountID, p.RealmID, p.ID, secretID).
		Scan(&currentRevision, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SecretMutationResult{}, ErrSecretNotFound
	}
	if err != nil {
		return SecretMutationResult{}, fmt.Errorf("lock secret lifecycle: %w", err)
	}
	if currentRevision != in.ExpectedRowVersion ||
		(operation == "secret_archive" && archivedAt != nil) ||
		(operation == "secret_restore" && archivedAt == nil) {
		return SecretMutationResult{}, ErrSecretConflict
	}

	var resultRevision int64
	if operation == "secret_archive" {
		err = tx.QueryRow(ctx, `
			UPDATE secrets
			   SET archived_at=clock_timestamp(), updated_at=clock_timestamp(),
			       row_version=row_version+1
			 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
			   AND id=$4 AND deleted_at IS NULL AND archived_at IS NULL
			   AND row_version=$5
			 RETURNING row_version`, p.AccountID, p.RealmID, p.ID, secretID,
			in.ExpectedRowVersion).Scan(&resultRevision)
	} else {
		err = tx.QueryRow(ctx, `
			UPDATE secrets
			   SET archived_at=NULL, updated_at=clock_timestamp(),
			       row_version=row_version+1
			 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
			   AND id=$4 AND deleted_at IS NULL AND archived_at IS NOT NULL
			   AND row_version=$5
			 RETURNING row_version`, p.AccountID, p.RealmID, p.ID, secretID,
			in.ExpectedRowVersion).Scan(&resultRevision)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return SecretMutationResult{}, ErrSecretConflict
	}
	if err != nil {
		if secretUniqueViolation(err) {
			return SecretMutationResult{}, ErrSecretConflict
		}
		return SecretMutationResult{}, fmt.Errorf("change secret lifecycle: %w", err)
	}
	receipt, err := insertSecretReceiptTx(ctx, tx, p, operation, keyHash,
		requestHash, "secret", secretID, resultRevision, 0)
	if err != nil {
		return SecretMutationResult{}, err
	}
	verb := VerbSecretArchived
	if operation == "secret_restore" {
		verb = VerbSecretRestored
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
		Verb: verb,
		Metadata: map[string]any{
			"agent_id": p.ID, "secret_id": secretID,
			"secret_revision": fmt.Sprint(resultRevision),
		},
	}); err != nil {
		return SecretMutationResult{}, err
	}
	secret, err := getSecret(ctx, tx, p, secretID, true, true)
	if err != nil {
		return SecretMutationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SecretMutationResult{}, err
	}
	return SecretMutationResult{Secret: secret, Receipt: receipt}, nil
}

func insertSecretFieldTx(ctx context.Context, tx pgx.Tx, p Principal, secretID string, field CreateSecretFieldInput) error {
	if !field.Sensitive {
		if _, err := tx.Exec(ctx, `
			INSERT INTO secret_fields
			       (id, account_id, realm_id, owner_agent_id, secret_id,
			        name, field_kind, sensitive, value_encoding, value_version,
			        public_value)
			VALUES ($1,$2,$3,$4,$5,$6,$7,false,$8,$9,$10)`, field.ID,
			p.AccountID, p.RealmID, p.ID, secretID, field.Name, field.Kind,
			field.Encoding, field.ValueVersion, *field.PublicValue); err != nil {
			if secretUniqueViolation(err) {
				return ErrSecretConflict
			}
			return fmt.Errorf("insert public secret field: %w", err)
		}
		return nil
	}
	sealed := field.Sealed
	if _, err := tx.Exec(ctx, `
		INSERT INTO secret_fields
		       (id, account_id, realm_id, owner_agent_id, secret_id,
		        name, field_kind, sensitive, value_encoding, value_version,
		        envelope_version, ciphertext, aead_algorithm, aad_version,
		        dek_id, dek_generation)
		VALUES ($1,$2,$3,$4,$5,$6,$7,true,$8,$9,$10,$11,$12,$13,$14,$15)`,
		field.ID, p.AccountID, p.RealmID, p.ID, secretID, field.Name,
		field.Kind, field.Encoding, field.ValueVersion, sealed.EnvelopeVersion, sealed.Ciphertext,
		sealed.Algorithm, sealed.AADVersion, sealed.DEK.ID,
		sealed.DEK.Generation); err != nil {
		if secretUniqueViolation(err) {
			return ErrSecretConflict
		}
		return fmt.Errorf("insert sensitive secret field: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secret_deks
		       (id, account_id, realm_id, owner_agent_id, secret_id, field_id,
		        dek_generation, wrapped_dek, wrap_algorithm, aad_version,
		        wrap_revision, wrapping_key_id, wrapping_key_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, sealed.DEK.ID,
		p.AccountID, p.RealmID, p.ID, secretID, field.ID,
		sealed.DEK.Generation, sealed.DEK.WrappedDEK, sealed.DEK.WrapAlgorithm,
		sealed.DEK.AADVersion, sealed.DEK.WrapRevision,
		sealed.DEK.WrappingKeyID, sealed.DEK.WrappingKeyVersion); err != nil {
		if secretUniqueViolation(err) {
			return ErrSecretConflict
		}
		return fmt.Errorf("insert wrapped secret DEK: %w", err)
	}
	return nil
}

func secretCreateEnvelopeSummary(fields []CreateSecretFieldInput) (string, int64, int, int, error) {
	keyID := ""
	var keyVersion int64
	sensitiveCount := 0
	encryptedBytes := 0
	for _, field := range fields {
		if !field.Sensitive {
			continue
		}
		sensitiveCount++
		sealed := field.Sealed
		if keyID == "" {
			keyID = sealed.DEK.WrappingKeyID
			keyVersion = sealed.DEK.WrappingKeyVersion
		} else if keyID != sealed.DEK.WrappingKeyID || keyVersion != sealed.DEK.WrappingKeyVersion {
			return "", 0, 0, 0, ErrSecretInputInvalid
		}
		encryptedBytes += len(sealed.Ciphertext) + len(sealed.DEK.WrappedDEK)
	}
	return keyID, keyVersion, sensitiveCount, encryptedBytes, nil
}

func lockCurrentVaultKeyTx(ctx context.Context, tx pgx.Tx, p Principal) (*VaultKeyBinding, error) {
	var key VaultKeyBinding
	err := tx.QueryRow(ctx, `
		SELECT id, account_id, realm_id, owner_agent_id, key_version,
		       algorithm, fingerprint, lifecycle_state, row_version,
		       created_at, retired_at
		  FROM agent_vault_keys
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND lifecycle_state='current'
		 FOR UPDATE`, p.AccountID, p.RealmID, p.ID).Scan(
		&key.ID, &key.AccountID, &key.RealmID, &key.OwnerAgentID,
		&key.KeyVersion, &key.Algorithm, &key.Fingerprint,
		&key.LifecycleState, &key.RowVersion, &key.CreatedAt, &key.RetiredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock current vault key binding: %w", err)
	}
	return &key, nil
}

func lockSecretOwnerAgentTx(ctx context.Context, tx pgx.Tx, p Principal) error {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT true
		  FROM agents a
		  JOIN realms r ON r.id=a.realm_id
		 WHERE r.account_id=$1 AND r.id=$2 AND a.id=$3 AND a.deleted_at IS NULL
		 FOR UPDATE OF a`, p.AccountID, p.RealmID, p.ID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSecretForbidden
	}
	if err != nil {
		return fmt.Errorf("lock secret owner: %w", err)
	}
	return nil
}

func getVaultKeyByIDTx(ctx context.Context, tx pgx.Tx, p Principal, keyID string) (VaultKeyBinding, error) {
	var key VaultKeyBinding
	err := tx.QueryRow(ctx, `
		SELECT id, account_id, realm_id, owner_agent_id, key_version,
		       algorithm, fingerprint, lifecycle_state, row_version,
		       created_at, retired_at
		  FROM agent_vault_keys
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4`,
		p.AccountID, p.RealmID, p.ID, keyID).Scan(
		&key.ID, &key.AccountID, &key.RealmID, &key.OwnerAgentID,
		&key.KeyVersion, &key.Algorithm, &key.Fingerprint,
		&key.LifecycleState, &key.RowVersion, &key.CreatedAt, &key.RetiredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyBinding{}, ErrVaultKeyConflict
	}
	if err != nil {
		return VaultKeyBinding{}, err
	}
	return key, nil
}

func lockSecretIdempotencyKey(ctx context.Context, tx pgx.Tx, p Principal, operation, keyHash string) error {
	lockName := p.AccountID + "\x00" + p.RealmID + "\x00" + p.ID + "\x00" + operation + "\x00" + keyHash
	sum := sha256.Sum256([]byte(lockName))
	lockID := int64(binary.BigEndian.Uint64(sum[:8]))
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockID)
	return err
}

func secretMutationFingerprint(payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", ErrSecretInputInvalid
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func replaySecretReceiptTx(ctx context.Context, tx pgx.Tx, p Principal, operation, keyHash, requestHash string) (SecretMutationReceipt, bool, error) {
	var receipt SecretMutationReceipt
	var existingHash string
	err := tx.QueryRow(ctx, `
		SELECT request_hash, target_kind, target_id, result_revision,
		       coalesce(result_value_version,0), created_at
		  FROM secret_mutation_receipts
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND actor_kind=$4 AND actor_id=$5 AND operation=$6
		   AND idempotency_key_hash=$7`, p.AccountID, p.RealmID, p.ID,
		p.Kind, p.ID, operation, keyHash).Scan(&existingHash,
		&receipt.TargetKind, &receipt.TargetID, &receipt.ResultRevision,
		&receipt.ResultValueVersion, &receipt.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SecretMutationReceipt{}, false, nil
	}
	if err != nil {
		return SecretMutationReceipt{}, false, err
	}
	if existingHash != requestHash {
		return SecretMutationReceipt{}, false, ErrSecretIdempotencyConflict
	}
	receipt.Operation = operation
	receipt.RequestHash = requestHash
	return receipt, true, nil
}

func insertSecretReceiptTx(ctx context.Context, tx pgx.Tx, p Principal, operation, keyHash, requestHash, targetKind, targetID string, resultRevision, resultValueVersion int64) (SecretMutationReceipt, error) {
	var valueVersion any
	if resultValueVersion > 0 {
		valueVersion = resultValueVersion
	}
	var createdAt time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO secret_mutation_receipts
		       (account_id, realm_id, owner_agent_id, actor_kind, actor_id,
		        operation, idempotency_key_hash, request_hash, target_kind,
		        target_id, result_revision, result_value_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING created_at`, p.AccountID, p.RealmID, p.ID, p.Kind, p.ID,
		operation, keyHash, requestHash, targetKind, targetID,
		resultRevision, valueVersion).Scan(&createdAt)
	if err != nil {
		return SecretMutationReceipt{}, fmt.Errorf("insert secret mutation receipt: %w", err)
	}
	return SecretMutationReceipt{
		Operation: operation, RequestHash: requestHash,
		TargetKind: targetKind, TargetID: targetID,
		ResultRevision: resultRevision, ResultValueVersion: resultValueVersion,
		CreatedAt: createdAt,
	}, nil
}

func secretUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}
