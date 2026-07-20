package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// GetVaultKeyRotation returns one authenticated agent-owned rotation. It never
// returns AVK bytes; a complete open run includes a deterministic plan hash over
// the staged opaque wrappers.
func (s *Store) GetVaultKeyRotation(ctx context.Context, p Principal, rotationID string) (VaultKeyRotation, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return VaultKeyRotation{}, err
	}
	rotationID, err := validateVaultKeyRotationID(rotationID)
	if err != nil {
		return VaultKeyRotation{}, err
	}
	rotation, err := getVaultKeyRotation(ctx, s.pool, p, rotationID)
	if err != nil {
		return VaultKeyRotation{}, err
	}
	if rotation.LifecycleState == VaultKeyRotationOpen && rotation.StagedCount == rotation.ItemCount {
		planHash, count, err := calculateVaultKeyRotationPlanHash(ctx, s.pool, p, rotation)
		if err != nil {
			return VaultKeyRotation{}, err
		}
		if count != rotation.ItemCount {
			return VaultKeyRotation{}, ErrVaultKeyRotationConflict
		}
		rotation.StagedPlanHash = planHash
	}
	return rotation, nil
}

// GetOpenVaultKeyRotation returns the one open rotation for the authenticated
// agent, or nil when there is no work to resume. The partial unique index on
// open rotations makes this a canonical value-free crash-recovery lookup.
func (s *Store) GetOpenVaultKeyRotation(ctx context.Context, p Principal) (*VaultKeyRotation, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return nil, err
	}
	var out VaultKeyRotation
	err := s.pool.QueryRow(ctx, `
		SELECT r.id, r.account_id, r.realm_id, r.owner_agent_id,
		       r.source_key_id, r.source_key_version, sk.algorithm, sk.fingerprint,
		       r.target_key_id, r.target_key_version, tk.algorithm, tk.fingerprint,
		       r.lifecycle_state, COALESCE(r.recovery_disposition_mode, ''),
		       COALESCE(r.recovery_artifact_sha256, ''),
		       r.item_count, r.staged_count, r.row_version,
		       r.created_at, r.updated_at, r.committed_at, r.cancelled_at
		  FROM agent_vault_key_rotations r
		  JOIN agent_vault_keys sk
		    ON sk.account_id=r.account_id AND sk.realm_id=r.realm_id
		   AND sk.owner_agent_id=r.owner_agent_id AND sk.id=r.source_key_id
		   AND sk.key_version=r.source_key_version
		  JOIN agent_vault_keys tk
		    ON tk.account_id=r.account_id AND tk.realm_id=r.realm_id
		   AND tk.owner_agent_id=r.owner_agent_id AND tk.id=r.target_key_id
		   AND tk.key_version=r.target_key_version
		 WHERE r.account_id=$1 AND r.realm_id=$2 AND r.owner_agent_id=$3
		   AND r.lifecycle_state='open'`, p.AccountID, p.RealmID, p.ID).Scan(
		&out.ID, &out.AccountID, &out.RealmID, &out.OwnerAgentID,
		&out.SourceKeyID, &out.SourceKeyVersion, &out.SourceKeyAlgorithm, &out.SourceKeyFingerprint,
		&out.TargetKeyID, &out.TargetKeyVersion, &out.TargetKeyAlgorithm, &out.TargetKeyFingerprint,
		&out.LifecycleState, &out.RecoveryDispositionMode, &out.RecoveryArtifactSHA256,
		&out.ItemCount, &out.StagedCount, &out.RowVersion,
		&out.CreatedAt, &out.UpdatedAt, &out.CommittedAt, &out.CancelledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read open vault key rotation: %w", err)
	}
	if out.StagedCount == out.ItemCount {
		planHash, count, err := calculateVaultKeyRotationPlanHash(ctx, s.pool, p, out)
		if err != nil {
			return nil, err
		}
		if count != out.ItemCount {
			return nil, ErrVaultKeyRotationConflict
		}
		out.StagedPlanHash = planHash
	}
	return &out, nil
}

func getVaultKeyRotation(ctx context.Context, q secretQuerier, p Principal, rotationID string) (VaultKeyRotation, error) {
	var out VaultKeyRotation
	err := q.QueryRow(ctx, `
		SELECT r.id, r.account_id, r.realm_id, r.owner_agent_id,
		       r.source_key_id, r.source_key_version, sk.algorithm, sk.fingerprint,
		       r.target_key_id, r.target_key_version, tk.algorithm, tk.fingerprint,
		       r.lifecycle_state, COALESCE(r.recovery_disposition_mode, ''),
		       COALESCE(r.recovery_artifact_sha256, ''),
		       r.item_count, r.staged_count, r.row_version,
		       r.created_at, r.updated_at, r.committed_at, r.cancelled_at
		  FROM agent_vault_key_rotations r
		  JOIN agent_vault_keys sk
		    ON sk.account_id=r.account_id AND sk.realm_id=r.realm_id
		   AND sk.owner_agent_id=r.owner_agent_id AND sk.id=r.source_key_id
		   AND sk.key_version=r.source_key_version
		  JOIN agent_vault_keys tk
		    ON tk.account_id=r.account_id AND tk.realm_id=r.realm_id
		   AND tk.owner_agent_id=r.owner_agent_id AND tk.id=r.target_key_id
		   AND tk.key_version=r.target_key_version
		 WHERE r.account_id=$1 AND r.realm_id=$2 AND r.owner_agent_id=$3 AND r.id=$4`,
		p.AccountID, p.RealmID, p.ID, rotationID).Scan(
		&out.ID, &out.AccountID, &out.RealmID, &out.OwnerAgentID,
		&out.SourceKeyID, &out.SourceKeyVersion, &out.SourceKeyAlgorithm, &out.SourceKeyFingerprint,
		&out.TargetKeyID, &out.TargetKeyVersion, &out.TargetKeyAlgorithm, &out.TargetKeyFingerprint,
		&out.LifecycleState, &out.RecoveryDispositionMode, &out.RecoveryArtifactSHA256,
		&out.ItemCount, &out.StagedCount, &out.RowVersion,
		&out.CreatedAt, &out.UpdatedAt, &out.CommittedAt, &out.CancelledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyRotation{}, ErrVaultKeyRotationNotFound
	}
	if err != nil {
		return VaultKeyRotation{}, fmt.Errorf("read vault key rotation: %w", err)
	}
	return out, nil
}

func lockVaultKeyRotationTx(ctx context.Context, tx pgx.Tx, p Principal, rotationID string) (VaultKeyRotation, error) {
	var out VaultKeyRotation
	err := tx.QueryRow(ctx, `
		SELECT r.id, r.account_id, r.realm_id, r.owner_agent_id,
		       r.source_key_id, r.source_key_version, sk.algorithm, sk.fingerprint,
		       r.target_key_id, r.target_key_version, tk.algorithm, tk.fingerprint,
		       r.lifecycle_state, COALESCE(r.recovery_disposition_mode, ''),
		       COALESCE(r.recovery_artifact_sha256, ''),
		       r.item_count, r.staged_count, r.row_version,
		       r.created_at, r.updated_at, r.committed_at, r.cancelled_at
		  FROM agent_vault_key_rotations r
		  JOIN agent_vault_keys sk
		    ON sk.account_id=r.account_id AND sk.realm_id=r.realm_id
		   AND sk.owner_agent_id=r.owner_agent_id AND sk.id=r.source_key_id
		   AND sk.key_version=r.source_key_version
		  JOIN agent_vault_keys tk
		    ON tk.account_id=r.account_id AND tk.realm_id=r.realm_id
		   AND tk.owner_agent_id=r.owner_agent_id AND tk.id=r.target_key_id
		   AND tk.key_version=r.target_key_version
		 WHERE r.account_id=$1 AND r.realm_id=$2 AND r.owner_agent_id=$3 AND r.id=$4
		 FOR UPDATE OF r`, p.AccountID, p.RealmID, p.ID, rotationID).Scan(
		&out.ID, &out.AccountID, &out.RealmID, &out.OwnerAgentID,
		&out.SourceKeyID, &out.SourceKeyVersion, &out.SourceKeyAlgorithm, &out.SourceKeyFingerprint,
		&out.TargetKeyID, &out.TargetKeyVersion, &out.TargetKeyAlgorithm, &out.TargetKeyFingerprint,
		&out.LifecycleState, &out.RecoveryDispositionMode, &out.RecoveryArtifactSHA256,
		&out.ItemCount, &out.StagedCount, &out.RowVersion,
		&out.CreatedAt, &out.UpdatedAt, &out.CommittedAt, &out.CancelledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return VaultKeyRotation{}, ErrVaultKeyRotationNotFound
	}
	if err != nil {
		return VaultKeyRotation{}, fmt.Errorf("lock vault key rotation: %w", err)
	}
	return out, nil
}

// ListVaultKeyRotationItems returns bounded source-wrapper snapshots in stable
// DEK-id order. The cursor is opaque to callers and tied only to the last DEK id.
func (s *Store) ListVaultKeyRotationItems(ctx context.Context, p Principal, rotationID string, options VaultKeyRotationItemListOptions) (VaultKeyRotationItemPage, error) {
	if err := requireSelfSecretPrincipal(p); err != nil {
		return VaultKeyRotationItemPage{}, err
	}
	rotationID, err := validateVaultKeyRotationID(rotationID)
	if err != nil {
		return VaultKeyRotationItemPage{}, err
	}
	options, after, err := normalizeVaultKeyRotationItemListOptions(options)
	if err != nil {
		return VaultKeyRotationItemPage{}, err
	}
	rotation, err := getVaultKeyRotation(ctx, s.pool, p, rotationID)
	if err != nil {
		return VaultKeyRotationItemPage{}, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT i.rotation_id, i.secret_id, i.field_id, f.field_kind,
		       i.dek_id, i.dek_generation, i.source_dek_row_version,
		       i.source_wrap_revision, i.source_wrapped_dek,
		       i.source_wrap_algorithm, i.source_aad_version,
		       i.source_wrapping_key_id, i.source_wrapping_key_version,
		       i.target_wrapped_dek, i.target_wrap_revision,
		       i.target_wrapper_sha256, i.staged_at
		  FROM agent_vault_key_rotation_items i
		  JOIN secret_fields f
		    ON f.account_id=i.account_id AND f.realm_id=i.realm_id
		   AND f.owner_agent_id=i.owner_agent_id AND f.secret_id=i.secret_id
		   AND f.id=i.field_id
		 WHERE i.account_id=$1 AND i.realm_id=$2 AND i.owner_agent_id=$3
		   AND i.rotation_id=$4 AND ($5='' OR i.dek_id>$5)
		 ORDER BY i.dek_id
		 LIMIT $6`, p.AccountID, p.RealmID, p.ID, rotationID, after, options.Limit+1)
	if err != nil {
		return VaultKeyRotationItemPage{}, fmt.Errorf("list vault key rotation items: %w", err)
	}
	defer rows.Close()
	items := make([]VaultKeyRotationItem, 0, options.Limit+1)
	for rows.Next() {
		item, err := scanVaultKeyRotationItem(rows, rotation)
		if err != nil {
			return VaultKeyRotationItemPage{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return VaultKeyRotationItemPage{}, err
	}
	hasMore := len(items) > options.Limit
	if hasMore {
		items = items[:options.Limit]
	}
	page := VaultKeyRotationItemPage{Items: items}
	if hasMore && len(items) > 0 {
		page.NextCursor, err = encodeVaultKeyRotationItemCursor(items[len(items)-1].DEKID)
		if err != nil {
			return VaultKeyRotationItemPage{}, err
		}
	}
	return page, nil
}

type vaultKeyRotationItemScanner interface {
	Scan(dest ...any) error
}

func scanVaultKeyRotationItem(scanner vaultKeyRotationItemScanner, rotation VaultKeyRotation) (VaultKeyRotationItem, error) {
	var (
		out                 VaultKeyRotationItem
		targetWrappedDEK    []byte
		targetWrapRevision  *int64
		targetWrapperSHA256 *string
	)
	if err := scanner.Scan(
		&out.RotationID, &out.SecretID, &out.FieldID, &out.FieldKind,
		&out.DEKID, &out.DEKGeneration, &out.SourceDEKRowVersion,
		&out.SourceWrapRevision, &out.SourceWrappedDEK, &out.SourceWrapAlgorithm,
		&out.SourceAADVersion, &out.SourceWrappingKeyID, &out.SourceWrappingKeyVersion,
		&targetWrappedDEK, &targetWrapRevision, &targetWrapperSHA256, &out.StagedAt,
	); err != nil {
		return VaultKeyRotationItem{}, err
	}
	out.SourceWrappedDEK = append([]byte(nil), out.SourceWrappedDEK...)
	out.TargetWrappingKeyID = rotation.TargetKeyID
	out.TargetWrappingKeyVersion = rotation.TargetKeyVersion
	if out.StagedAt != nil {
		if targetWrapRevision == nil || targetWrapperSHA256 == nil ||
			len(targetWrappedDEK) != vaultKeyRotationWrappedDEKBytes {
			return VaultKeyRotationItem{}, ErrVaultKeyRotationConflict
		}
		out.TargetWrappedDEK = append([]byte(nil), targetWrappedDEK...)
		out.TargetWrapRevision = *targetWrapRevision
		out.TargetWrapperSHA256 = *targetWrapperSHA256
	}
	return out, nil
}

func calculateVaultKeyRotationPlanHash(ctx context.Context, q secretQuerier, p Principal, rotation VaultKeyRotation) (string, int64, error) {
	rows, err := q.Query(ctx, `
		SELECT i.rotation_id, i.secret_id, i.field_id, f.field_kind,
		       i.dek_id, i.dek_generation, i.source_dek_row_version,
		       i.source_wrap_revision, i.source_wrapped_dek,
		       i.source_wrap_algorithm, i.source_aad_version,
		       i.source_wrapping_key_id, i.source_wrapping_key_version,
		       i.target_wrapped_dek, i.target_wrap_revision,
		       i.target_wrapper_sha256, i.staged_at
		  FROM agent_vault_key_rotation_items i
		  JOIN secret_fields f
		    ON f.account_id=i.account_id AND f.realm_id=i.realm_id
		   AND f.owner_agent_id=i.owner_agent_id AND f.secret_id=i.secret_id
		   AND f.id=i.field_id
		 WHERE i.account_id=$1 AND i.realm_id=$2 AND i.owner_agent_id=$3
		   AND i.rotation_id=$4
		 ORDER BY i.dek_id`, p.AccountID, p.RealmID, p.ID, rotation.ID)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	hasher := newVaultKeyRotationPlanHasher(rotation)
	var count int64
	for rows.Next() {
		item, err := scanVaultKeyRotationItem(rows, rotation)
		if err != nil {
			return "", 0, err
		}
		if item.StagedAt == nil || item.TargetWrapperSHA256 != vaultKeyRotationWrapperHash(item.TargetWrappedDEK) {
			return "", 0, ErrVaultKeyRotationIncomplete
		}
		hasher.Add(item)
		count++
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	return hasher.Sum(), count, nil
}
