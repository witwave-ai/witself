package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// StartVaultKeyRotation creates a pending public target epoch and an immutable
// snapshot of every retained wrapped DEK. Live wrappers and the current epoch do
// not change until CommitVaultKeyRotation succeeds.
func (s *Store) StartVaultKeyRotation(ctx context.Context, p Principal, in StartVaultKeyRotationInput) (VaultKeyRotation, VaultKeyRotationReceipt, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	in, err := normalizeStartVaultKeyRotationInput(in)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	requestInput := in
	requestInput.IdempotencyKey = ""
	requestHash, err := vaultKeyRotationMutationFingerprint(requestInput)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	keyHash := secretIdempotencyKeyHash(in.IdempotencyKey)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := lockSecretOwnerAgentTx(ctx, tx, p); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := lockSecretIdempotencyKey(ctx, tx, p, "rotation_start", keyHash); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if receipt, replayed, err := replayVaultKeyRotationReceiptTx(ctx, tx, p,
		"rotation_start", keyHash, requestHash); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	} else if replayed {
		rotation, err := getVaultKeyRotation(ctx, tx, p, receipt.RotationID)
		if err != nil {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
		receipt.Replayed = true
		if rotation.LifecycleState == VaultKeyRotationOpen && rotation.StagedCount == rotation.ItemCount {
			rotation.StagedPlanHash, _, err = calculateVaultKeyRotationPlanHash(ctx, tx, p, rotation)
			if err != nil {
				return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
		return rotation, receipt, nil
	}
	// Enrollment and rotation share the same stable owner-row lock. Expire due
	// requests and reject every still-authoritative transfer before creating the
	// pending key epoch or snapshot. This makes the check race-free against
	// Create/Approve/ConsumeVaultKeyEnrollment, which take the same lock and
	// reject an open rotation.
	if err := ensureNoActiveVaultKeyEnrollmentTx(ctx, tx, p); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}

	var openID string
	err = tx.QueryRow(ctx, `
		SELECT id FROM agent_vault_key_rotations
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND lifecycle_state='open'`, p.AccountID, p.RealmID, p.ID).Scan(&openID)
	if err == nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationInProgress
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	current, err := lockCurrentVaultKeyTx(ctx, tx, p)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if current == nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyUnavailable
	}
	if current.ID != in.ExpectedSourceKeyID || current.KeyVersion != in.ExpectedSourceKeyVersion ||
		current.RowVersion != in.ExpectedSourceKeyRowVersion || current.Algorithm != SecretAEADAlgorithm {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	if in.TargetKeyVersion != current.KeyVersion+1 {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}

	var itemCount, wrongSourceCount int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE
		       wrapping_key_id<>$4 OR wrapping_key_version<>$5 OR
		       wrap_algorithm<>$6 OR aad_version<>$7)
		  FROM secret_deks
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3`,
		p.AccountID, p.RealmID, p.ID, current.ID, current.KeyVersion,
		SecretAEADAlgorithm, SecretAADVersion).Scan(&itemCount, &wrongSourceCount); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if wrongSourceCount != 0 {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_vault_keys
		       (id, account_id, realm_id, owner_agent_id, key_version,
		        algorithm, fingerprint, lifecycle_state)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'pending')`, in.TargetKeyID,
		p.AccountID, p.RealmID, p.ID, in.TargetKeyVersion,
		in.TargetAlgorithm, in.TargetFingerprint); err != nil {
		if secretUniqueViolation(err) {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
		}
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, fmt.Errorf("insert pending vault key epoch: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_vault_key_rotations
		       (id, account_id, realm_id, owner_agent_id,
		        source_key_id, source_key_version, target_key_id, target_key_version,
		        item_count)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, in.ID, p.AccountID, p.RealmID,
		p.ID, current.ID, current.KeyVersion, in.TargetKeyID,
		in.TargetKeyVersion, itemCount); err != nil {
		if secretUniqueViolation(err) {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationInProgress
		}
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, fmt.Errorf("insert vault key rotation: %w", err)
	}
	result, err := tx.Exec(ctx, `
		INSERT INTO agent_vault_key_rotation_items
		       (rotation_id, account_id, realm_id, owner_agent_id,
		        secret_id, field_id, dek_id, dek_generation,
		        source_dek_row_version, source_wrap_revision, source_wrapped_dek,
		        source_wrap_algorithm, source_aad_version,
		        source_wrapping_key_id, source_wrapping_key_version)
		SELECT $1, account_id, realm_id, owner_agent_id,
		       secret_id, field_id, id, dek_generation,
		       row_version, wrap_revision, wrapped_dek, wrap_algorithm, aad_version,
		       wrapping_key_id, wrapping_key_version
		  FROM secret_deks
		 WHERE account_id=$2 AND realm_id=$3 AND owner_agent_id=$4
		 ORDER BY id`, in.ID, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, fmt.Errorf("snapshot vault key rotation items: %w", err)
	}
	if result.RowsAffected() != itemCount {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	rotation, err := getVaultKeyRotation(ctx, tx, p, in.ID)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if itemCount == 0 {
		rotation.StagedPlanHash = newVaultKeyRotationPlanHasher(rotation).Sum()
	}
	receipt, err := insertVaultKeyRotationReceiptTx(ctx, tx, p, "rotation_start",
		keyHash, requestHash, in.ID, rotation.RowVersion)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
		Verb: VerbVaultKeyRotationStarted,
		Metadata: map[string]any{
			"agent_id": p.ID, "rotation_id": rotation.ID,
			"source_key_id": rotation.SourceKeyID, "source_key_version": fmt.Sprint(rotation.SourceKeyVersion),
			"target_key_id": rotation.TargetKeyID, "target_key_version": fmt.Sprint(rotation.TargetKeyVersion),
			"item_count": fmt.Sprint(rotation.ItemCount),
		},
	}); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	return rotation, receipt, nil
}

// StageVaultKeyRotation atomically stores a bounded batch of replacement
// wrappers away from the live secret_deks rows.
func (s *Store) StageVaultKeyRotation(ctx context.Context, p Principal, rotationID string, in StageVaultKeyRotationInput) (VaultKeyRotation, VaultKeyRotationReceipt, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	rotationID, err := validateVaultKeyRotationID(rotationID)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	in, err = normalizeStageVaultKeyRotationInput(in)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	requestInput := in
	requestInput.IdempotencyKey = ""
	requestHash, err := vaultKeyRotationMutationFingerprint(struct {
		RotationID string                     `json:"rotation_id"`
		Request    StageVaultKeyRotationInput `json:"request"`
	}{rotationID, requestInput})
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	keyHash := secretIdempotencyKeyHash(in.IdempotencyKey)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := lockSecretOwnerAgentTx(ctx, tx, p); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := lockSecretIdempotencyKey(ctx, tx, p, "rotation_stage", keyHash); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if receipt, replayed, err := replayVaultKeyRotationReceiptTx(ctx, tx, p,
		"rotation_stage", keyHash, requestHash); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	} else if replayed {
		rotation, err := getVaultKeyRotation(ctx, tx, p, receipt.RotationID)
		if err != nil {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
		receipt.Replayed = true
		if rotation.LifecycleState == VaultKeyRotationOpen && rotation.StagedCount == rotation.ItemCount {
			rotation.StagedPlanHash, _, err = calculateVaultKeyRotationPlanHash(ctx, tx, p, rotation)
			if err != nil {
				return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
		return rotation, receipt, nil
	}
	rotation, err := lockVaultKeyRotationTx(ctx, tx, p, rotationID)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if rotation.LifecycleState != VaultKeyRotationOpen || rotation.RowVersion != in.ExpectedRotationRowVersion {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	var newlyStaged int64
	for _, candidate := range in.Items {
		var (
			sourceRowVersion, sourceWrapRevision, sourceKeyVersion int64
			sourceWrapped, targetWrapped                           []byte
			sourceAlgorithm, sourceKeyID, targetHash               string
			sourceAADVersion                                       int64
			targetRevisionNullable                                 *int64
			stagedAt                                               *time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT i.source_dek_row_version, i.source_wrap_revision,
			       i.source_wrapped_dek, i.source_wrap_algorithm,
			       i.source_aad_version, i.source_wrapping_key_id,
			       i.source_wrapping_key_version,
			       i.target_wrapped_dek, i.target_wrap_revision,
			       coalesce(i.target_wrapper_sha256,''), i.staged_at
			  FROM agent_vault_key_rotation_items i
			  JOIN secret_deks d
			    ON d.account_id=i.account_id AND d.realm_id=i.realm_id
			   AND d.owner_agent_id=i.owner_agent_id AND d.secret_id=i.secret_id
			   AND d.field_id=i.field_id AND d.id=i.dek_id
			   AND d.dek_generation=i.dek_generation
			 WHERE i.account_id=$1 AND i.realm_id=$2 AND i.owner_agent_id=$3
			   AND i.rotation_id=$4 AND i.dek_id=$5
			   AND d.row_version=i.source_dek_row_version
			   AND d.wrap_revision=i.source_wrap_revision
			   AND d.wrapped_dek=i.source_wrapped_dek
			   AND d.wrap_algorithm=i.source_wrap_algorithm
			   AND d.aad_version=i.source_aad_version
			   AND d.wrapping_key_id=i.source_wrapping_key_id
			   AND d.wrapping_key_version=i.source_wrapping_key_version
			 FOR UPDATE OF i, d`, p.AccountID, p.RealmID, p.ID, rotationID,
			candidate.DEKID).Scan(&sourceRowVersion, &sourceWrapRevision,
			&sourceWrapped, &sourceAlgorithm, &sourceAADVersion, &sourceKeyID,
			&sourceKeyVersion, &targetWrapped, &targetRevisionNullable, &targetHash, &stagedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
		}
		if err != nil {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
		if sourceRowVersion != candidate.ExpectedSourceDEKRowVersion ||
			sourceWrapRevision != candidate.ExpectedSourceWrapRevision ||
			sourceAlgorithm != SecretAEADAlgorithm || sourceAADVersion != SecretAADVersion ||
			sourceKeyID != rotation.SourceKeyID || sourceKeyVersion != rotation.SourceKeyVersion ||
			len(sourceWrapped) != vaultKeyRotationWrappedDEKBytes {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
		}
		wrapperHash := vaultKeyRotationWrapperHash(candidate.TargetWrappedDEK)
		if stagedAt != nil {
			if targetRevisionNullable == nil || *targetRevisionNullable != candidate.TargetWrapRevision ||
				targetHash != wrapperHash || !bytes.Equal(targetWrapped, candidate.TargetWrappedDEK) {
				return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
			}
			continue
		}
		result, err := tx.Exec(ctx, `
			UPDATE agent_vault_key_rotation_items
			   SET target_wrapped_dek=$6, target_wrap_revision=$7,
			       target_wrapper_sha256=$8, staged_at=clock_timestamp()
			 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
			   AND rotation_id=$4 AND dek_id=$5
			   AND target_wrapped_dek IS NULL`, p.AccountID, p.RealmID, p.ID,
			rotationID, candidate.DEKID, candidate.TargetWrappedDEK,
			candidate.TargetWrapRevision, wrapperHash)
		if err != nil {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
		if result.RowsAffected() != 1 {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
		}
		newlyStaged++
	}
	if newlyStaged > 0 {
		err = tx.QueryRow(ctx, `
			UPDATE agent_vault_key_rotations
			   SET staged_count=staged_count+$5, row_version=row_version+1,
			       updated_at=clock_timestamp()
			 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4
			   AND lifecycle_state='open' AND row_version=$6
			 RETURNING id, account_id, realm_id, owner_agent_id,
			           source_key_id, source_key_version, target_key_id, target_key_version,
			           lifecycle_state, item_count, staged_count, row_version,
			           created_at, updated_at, committed_at, cancelled_at`,
			p.AccountID, p.RealmID, p.ID, rotationID, newlyStaged,
			in.ExpectedRotationRowVersion).Scan(
			&rotation.ID, &rotation.AccountID, &rotation.RealmID, &rotation.OwnerAgentID,
			&rotation.SourceKeyID, &rotation.SourceKeyVersion, &rotation.TargetKeyID, &rotation.TargetKeyVersion,
			&rotation.LifecycleState, &rotation.ItemCount, &rotation.StagedCount, &rotation.RowVersion,
			&rotation.CreatedAt, &rotation.UpdatedAt, &rotation.CommittedAt, &rotation.CancelledAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
		}
		if err != nil {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
	}
	if rotation.StagedCount > rotation.ItemCount {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	if rotation.StagedCount == rotation.ItemCount {
		rotation.StagedPlanHash, _, err = calculateVaultKeyRotationPlanHash(ctx, tx, p, rotation)
		if err != nil {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
	}
	receipt, err := insertVaultKeyRotationReceiptTx(ctx, tx, p, "rotation_stage",
		keyHash, requestHash, rotationID, rotation.RowVersion)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if newlyStaged > 0 {
		if err := logEventTx(ctx, tx, EventInput{
			AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
			Verb: VerbVaultKeyRotationStaged,
			Metadata: map[string]any{
				"agent_id": p.ID, "rotation_id": rotation.ID,
				"staged_batch_count": fmt.Sprint(newlyStaged), "staged_count": fmt.Sprint(rotation.StagedCount),
				"item_count": fmt.Sprint(rotation.ItemCount), "rotation_revision": fmt.Sprint(rotation.RowVersion),
			},
		}); err != nil {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	return rotation, receipt, nil
}

// CommitVaultKeyRotation atomically replaces every snapshotted wrapper and
// flips the public epoch. PostgreSQL rollback guarantees that no observer can
// see a partially rewrapped current vault.
func (s *Store) CommitVaultKeyRotation(ctx context.Context, p Principal, rotationID string, in CommitVaultKeyRotationInput) (VaultKeyRotation, VaultKeyRotationReceipt, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	rotationID, err := validateVaultKeyRotationID(rotationID)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	in, err = normalizeCommitVaultKeyRotationInput(in)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	requestHash, err := vaultKeyRotationCommitRequestHash(rotationID, in)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	keyHash := secretIdempotencyKeyHash(in.IdempotencyKey)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := lockSecretOwnerAgentTx(ctx, tx, p); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := lockSecretIdempotencyKey(ctx, tx, p, "rotation_commit", keyHash); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if receipt, replayed, err := replayVaultKeyRotationReceiptTx(ctx, tx, p,
		"rotation_commit", keyHash, requestHash); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	} else if replayed {
		rotation, err := getVaultKeyRotation(ctx, tx, p, receipt.RotationID)
		if err != nil {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
		receipt.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
		return rotation, receipt, nil
	}
	rotation, err := lockVaultKeyRotationTx(ctx, tx, p, rotationID)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if rotation.LifecycleState != VaultKeyRotationOpen ||
		rotation.RowVersion != in.ExpectedRotationRowVersion ||
		rotation.ItemCount != in.ExpectedItemCount || rotation.StagedCount != rotation.ItemCount {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationIncomplete
	}
	source, err := lockVaultKeyEpochTx(ctx, tx, p, rotation.SourceKeyID, rotation.SourceKeyVersion)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	target, err := lockVaultKeyEpochTx(ctx, tx, p, rotation.TargetKeyID, rotation.TargetKeyVersion)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if source.LifecycleState != "current" || target.LifecycleState != "pending" {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	rows, err := tx.Query(ctx, `
		SELECT d.id
		  FROM agent_vault_key_rotation_items i
		  JOIN secret_deks d
		    ON d.account_id=i.account_id AND d.realm_id=i.realm_id
		   AND d.owner_agent_id=i.owner_agent_id AND d.secret_id=i.secret_id
		   AND d.field_id=i.field_id AND d.id=i.dek_id
		   AND d.dek_generation=i.dek_generation
		 WHERE i.account_id=$1 AND i.realm_id=$2 AND i.owner_agent_id=$3
		   AND i.rotation_id=$4
		 ORDER BY d.id
		 FOR UPDATE OF i, d`, p.AccountID, p.RealmID, p.ID, rotationID)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	var lockedCount int64
	for rows.Next() {
		var dekID string
		if err := rows.Scan(&dekID); err != nil {
			rows.Close()
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
		lockedCount++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	rows.Close()
	if lockedCount != rotation.ItemCount {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	var eligibleCount int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		  FROM agent_vault_key_rotation_items i
		  JOIN secret_deks d
		    ON d.account_id=i.account_id AND d.realm_id=i.realm_id
		   AND d.owner_agent_id=i.owner_agent_id AND d.secret_id=i.secret_id
		   AND d.field_id=i.field_id AND d.id=i.dek_id
		   AND d.dek_generation=i.dek_generation
		 WHERE i.account_id=$1 AND i.realm_id=$2 AND i.owner_agent_id=$3
		   AND i.rotation_id=$4
		   AND i.target_wrapped_dek IS NOT NULL AND i.target_wrap_revision IS NOT NULL
		   AND i.target_wrapper_sha256 IS NOT NULL AND i.staged_at IS NOT NULL
		   AND d.row_version=i.source_dek_row_version
		   AND d.wrap_revision=i.source_wrap_revision
		   AND d.wrapped_dek=i.source_wrapped_dek
		   AND d.wrap_algorithm=i.source_wrap_algorithm
		   AND d.aad_version=i.source_aad_version
		   AND d.wrapping_key_id=i.source_wrapping_key_id
		   AND d.wrapping_key_version=i.source_wrapping_key_version`,
		p.AccountID, p.RealmID, p.ID, rotationID).Scan(&eligibleCount); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if eligibleCount != rotation.ItemCount {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	planHash, count, err := calculateVaultKeyRotationPlanHash(ctx, tx, p, rotation)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if count != rotation.ItemCount || planHash != in.ExpectedPlanHash {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	result, err := tx.Exec(ctx, `
		UPDATE secret_deks d
		   SET wrapped_dek=i.target_wrapped_dek,
		       wrap_revision=i.target_wrap_revision,
		       wrapping_key_id=$5, wrapping_key_version=$6,
		       row_version=d.row_version+1
		  FROM agent_vault_key_rotation_items i
		 WHERE i.account_id=$1 AND i.realm_id=$2 AND i.owner_agent_id=$3
		   AND i.rotation_id=$4
		   AND d.account_id=i.account_id AND d.realm_id=i.realm_id
		   AND d.owner_agent_id=i.owner_agent_id AND d.secret_id=i.secret_id
		   AND d.field_id=i.field_id AND d.id=i.dek_id
		   AND d.dek_generation=i.dek_generation
		   AND d.row_version=i.source_dek_row_version
		   AND d.wrap_revision=i.source_wrap_revision
		   AND d.wrapped_dek=i.source_wrapped_dek
		   AND d.wrapping_key_id=i.source_wrapping_key_id
		   AND d.wrapping_key_version=i.source_wrapping_key_version`,
		p.AccountID, p.RealmID, p.ID, rotationID,
		rotation.TargetKeyID, rotation.TargetKeyVersion)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, fmt.Errorf("commit rotated DEK wrappers: %w", err)
	}
	if result.RowsAffected() != rotation.ItemCount {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	if result, err = tx.Exec(ctx, `
		UPDATE agent_vault_keys
		   SET lifecycle_state='retired', retired_at=clock_timestamp(),
		       row_version=row_version+1
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND id=$4 AND key_version=$5 AND lifecycle_state='current'`,
		p.AccountID, p.RealmID, p.ID, rotation.SourceKeyID, rotation.SourceKeyVersion); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	} else if result.RowsAffected() != 1 {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	if result, err = tx.Exec(ctx, `
		UPDATE agent_vault_keys
		   SET lifecycle_state='current', row_version=row_version+1
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND id=$4 AND key_version=$5 AND lifecycle_state='pending'`,
		p.AccountID, p.RealmID, p.ID, rotation.TargetKeyID, rotation.TargetKeyVersion); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	} else if result.RowsAffected() != 1 {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	err = tx.QueryRow(ctx, `
		UPDATE agent_vault_key_rotations
		   SET lifecycle_state='committed', committed_at=clock_timestamp(),
		       recovery_disposition_mode=$6, recovery_artifact_sha256=$7,
		       updated_at=clock_timestamp(), row_version=row_version+1
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4
		   AND lifecycle_state='open' AND row_version=$5
		 RETURNING id, account_id, realm_id, owner_agent_id,
		           source_key_id, source_key_version, target_key_id, target_key_version,
		           lifecycle_state, recovery_disposition_mode,
		           COALESCE(recovery_artifact_sha256, ''),
		           item_count, staged_count, row_version,
		           created_at, updated_at, committed_at, cancelled_at`,
		p.AccountID, p.RealmID, p.ID, rotationID, in.ExpectedRotationRowVersion,
		in.RecoveryDisposition.Mode, nullableVaultKeyRotationArtifactSHA256(in.RecoveryDisposition)).Scan(
		&rotation.ID, &rotation.AccountID, &rotation.RealmID, &rotation.OwnerAgentID,
		&rotation.SourceKeyID, &rotation.SourceKeyVersion, &rotation.TargetKeyID, &rotation.TargetKeyVersion,
		&rotation.LifecycleState, &rotation.RecoveryDispositionMode, &rotation.RecoveryArtifactSHA256,
		&rotation.ItemCount, &rotation.StagedCount, &rotation.RowVersion,
		&rotation.CreatedAt, &rotation.UpdatedAt, &rotation.CommittedAt, &rotation.CancelledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	receipt, err := insertVaultKeyRotationReceiptTx(ctx, tx, p, "rotation_commit",
		keyHash, requestHash, rotationID, rotation.RowVersion)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
		Verb:     VerbVaultKeyRotationCommitted,
		Metadata: vaultKeyRotationCommitEventMetadata(p, rotation, in.RecoveryDisposition),
	}); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_vault_key_rotation_items
		WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND rotation_id=$4`,
		p.AccountID, p.RealmID, p.ID, rotationID); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	return rotation, receipt, nil
}

func vaultKeyRotationCommitRequestHash(rotationID string, in CommitVaultKeyRotationInput) (string, error) {
	in.IdempotencyKey = ""
	return vaultKeyRotationMutationFingerprint(struct {
		RotationID string                      `json:"rotation_id"`
		Request    CommitVaultKeyRotationInput `json:"request"`
	}{rotationID, in})
}

func vaultKeyRotationCommitEventMetadata(p Principal, rotation VaultKeyRotation, disposition VaultKeyRotationRecoveryDisposition) map[string]any {
	metadata := map[string]any{
		"agent_id": p.ID, "rotation_id": rotation.ID,
		"source_key_id": rotation.SourceKeyID, "source_key_version": fmt.Sprint(rotation.SourceKeyVersion),
		"target_key_id": rotation.TargetKeyID, "target_key_version": fmt.Sprint(rotation.TargetKeyVersion),
		"item_count":                fmt.Sprint(rotation.ItemCount),
		"recovery_disposition_mode": disposition.Mode,
	}
	if disposition.Mode == VaultKeyRotationRecoveryArtifact {
		metadata["recovery_artifact_sha256"] = disposition.ArtifactSHA256
	}
	return metadata
}

func nullableVaultKeyRotationArtifactSHA256(disposition VaultKeyRotationRecoveryDisposition) any {
	if disposition.Mode == VaultKeyRotationRecoveryArtifact {
		return disposition.ArtifactSHA256
	}
	return nil
}

// CancelVaultKeyRotation is a harm-reducing safety write: it is allowed for a
// suspended account, leaves the source epoch/current wrappers untouched, and
// retires the never-current target epoch.
func (s *Store) CancelVaultKeyRotation(ctx context.Context, p Principal, rotationID string, in CancelVaultKeyRotationInput) (VaultKeyRotation, VaultKeyRotationReceipt, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	rotationID, err := validateVaultKeyRotationID(rotationID)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	in, err = normalizeCancelVaultKeyRotationInput(in)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	requestInput := in
	requestInput.IdempotencyKey = ""
	requestHash, err := vaultKeyRotationMutationFingerprint(struct {
		RotationID string                      `json:"rotation_id"`
		Request    CancelVaultKeyRotationInput `json:"request"`
	}{rotationID, requestInput})
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	keyHash := secretIdempotencyKeyHash(in.IdempotencyKey)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForSafetyWrite(ctx, tx, p.AccountID); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := lockSecretOwnerAgentTx(ctx, tx, p); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := lockSecretIdempotencyKey(ctx, tx, p, "rotation_cancel", keyHash); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if receipt, replayed, err := replayVaultKeyRotationReceiptTx(ctx, tx, p,
		"rotation_cancel", keyHash, requestHash); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	} else if replayed {
		rotation, err := getVaultKeyRotation(ctx, tx, p, receipt.RotationID)
		if err != nil {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
		receipt.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
		}
		return rotation, receipt, nil
	}
	rotation, err := lockVaultKeyRotationTx(ctx, tx, p, rotationID)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if rotation.LifecycleState != VaultKeyRotationOpen || rotation.RowVersion != in.ExpectedRotationRowVersion {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	result, err := tx.Exec(ctx, `
		UPDATE agent_vault_keys
		   SET lifecycle_state='retired', retired_at=clock_timestamp(),
		       row_version=row_version+1
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND id=$4 AND key_version=$5 AND lifecycle_state='pending'`,
		p.AccountID, p.RealmID, p.ID, rotation.TargetKeyID, rotation.TargetKeyVersion)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if result.RowsAffected() != 1 {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	err = tx.QueryRow(ctx, `
		UPDATE agent_vault_key_rotations
		   SET lifecycle_state='cancelled', cancelled_at=clock_timestamp(),
		       updated_at=clock_timestamp(), row_version=row_version+1
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4
		   AND lifecycle_state='open' AND row_version=$5
		 RETURNING id, account_id, realm_id, owner_agent_id,
		           source_key_id, source_key_version, target_key_id, target_key_version,
		           lifecycle_state, item_count, staged_count, row_version,
		           created_at, updated_at, committed_at, cancelled_at`,
		p.AccountID, p.RealmID, p.ID, rotationID, in.ExpectedRotationRowVersion).Scan(
		&rotation.ID, &rotation.AccountID, &rotation.RealmID, &rotation.OwnerAgentID,
		&rotation.SourceKeyID, &rotation.SourceKeyVersion, &rotation.TargetKeyID, &rotation.TargetKeyVersion,
		&rotation.LifecycleState, &rotation.ItemCount, &rotation.StagedCount, &rotation.RowVersion,
		&rotation.CreatedAt, &rotation.UpdatedAt, &rotation.CommittedAt, &rotation.CancelledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, ErrVaultKeyRotationConflict
	}
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	receipt, err := insertVaultKeyRotationReceiptTx(ctx, tx, p, "rotation_cancel",
		keyHash, requestHash, rotationID, rotation.RowVersion)
	if err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: p.AccountID, ActorKind: ActorAgent, ActorID: p.ID,
		Verb: VerbVaultKeyRotationCancelled,
		Metadata: map[string]any{
			"agent_id": p.ID, "rotation_id": rotation.ID,
			"source_key_id": rotation.SourceKeyID, "source_key_version": fmt.Sprint(rotation.SourceKeyVersion),
			"target_key_id": rotation.TargetKeyID, "target_key_version": fmt.Sprint(rotation.TargetKeyVersion),
			"item_count": fmt.Sprint(rotation.ItemCount), "staged_count": fmt.Sprint(rotation.StagedCount),
		},
	}); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_vault_key_rotation_items
		WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND rotation_id=$4`,
		p.AccountID, p.RealmID, p.ID, rotationID); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return VaultKeyRotation{}, VaultKeyRotationReceipt{}, err
	}
	return rotation, receipt, nil
}

func ensureNoOpenVaultKeyRotationTx(ctx context.Context, tx pgx.Tx, p Principal) error {
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM agent_vault_key_rotations
		   WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		     AND lifecycle_state='open'
		)`, p.AccountID, p.RealmID, p.ID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrVaultKeyRotationInProgress
	}
	return nil
}

func ensureNoActiveVaultKeyEnrollmentTx(ctx context.Context, tx pgx.Tx, p Principal) error {
	if err := expireVaultKeyEnrollmentsTx(ctx, tx, p); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM agent_vault_key_enrollments
		   WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		     AND lifecycle_state IN ('pending','approved')
		)`, p.AccountID, p.RealmID, p.ID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrVaultLifecycleInProgress
	}
	return nil
}

func lockVaultKeyEpochTx(ctx context.Context, tx pgx.Tx, p Principal, keyID string, keyVersion int64) (VaultKeyBinding, error) {
	var key VaultKeyBinding
	err := tx.QueryRow(ctx, `
		SELECT id, account_id, realm_id, owner_agent_id, key_version,
		       algorithm, fingerprint, lifecycle_state, row_version,
		       created_at, retired_at
		  FROM agent_vault_keys
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND id=$4 AND key_version=$5
		 FOR UPDATE`, p.AccountID, p.RealmID, p.ID, keyID, keyVersion).Scan(
		&key.ID, &key.AccountID, &key.RealmID, &key.OwnerAgentID,
		&key.KeyVersion, &key.Algorithm, &key.Fingerprint,
		&key.LifecycleState, &key.RowVersion, &key.CreatedAt, &key.RetiredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyBinding{}, ErrVaultKeyRotationConflict
	}
	if err != nil {
		return VaultKeyBinding{}, err
	}
	return key, nil
}

func replayVaultKeyRotationReceiptTx(ctx context.Context, tx pgx.Tx, p Principal, operation, keyHash, requestHash string) (VaultKeyRotationReceipt, bool, error) {
	var receipt VaultKeyRotationReceipt
	var existingHash string
	err := tx.QueryRow(ctx, `
		SELECT request_hash, rotation_id, result_revision, created_at
		  FROM vault_key_rotation_receipts
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND operation=$4 AND idempotency_key_hash=$5`, p.AccountID,
		p.RealmID, p.ID, operation, keyHash).Scan(&existingHash,
		&receipt.RotationID, &receipt.ResultRevision, &receipt.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyRotationReceipt{}, false, nil
	}
	if err != nil {
		return VaultKeyRotationReceipt{}, false, err
	}
	if existingHash != requestHash {
		return VaultKeyRotationReceipt{}, false, ErrSecretIdempotencyConflict
	}
	receipt.Operation = operation
	receipt.RequestHash = requestHash
	return receipt, true, nil
}

func insertVaultKeyRotationReceiptTx(ctx context.Context, tx pgx.Tx, p Principal, operation, keyHash, requestHash, rotationID string, resultRevision int64) (VaultKeyRotationReceipt, error) {
	var createdAt time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO vault_key_rotation_receipts
		       (account_id, realm_id, owner_agent_id, operation,
		        idempotency_key_hash, request_hash, rotation_id, result_revision)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING created_at`, p.AccountID, p.RealmID, p.ID, operation,
		keyHash, requestHash, rotationID, resultRevision).Scan(&createdAt)
	if err != nil {
		return VaultKeyRotationReceipt{}, fmt.Errorf("insert vault key rotation receipt: %w", err)
	}
	return VaultKeyRotationReceipt{
		Operation: operation, RequestHash: requestHash, RotationID: rotationID,
		ResultRevision: resultRevision, CreatedAt: createdAt,
	}, nil
}
