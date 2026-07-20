package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/export"
)

// ErrAccountNotExportable is returned when an export is attempted against an
// account that is neither suspended nor closed. Exports require the write
// freeze for consistency; closed tombstones are exportable because they must
// survive their cell's decommissioning (accounts live forever).
var ErrAccountNotExportable = errors.New("account must be suspended (or closed) to export")

// ErrVaultLifecycleInProgress prevents one vault-key lifecycle from crossing a
// mutually exclusive operation or a cell move from silently abandoning active
// enrollment or rotation work. It also fences orphan pending key epochs that
// can only arise from legacy or manually damaged state.
var ErrVaultLifecycleInProgress = errors.New("vault key enrollment or rotation is still active")

// ErrVaultLifecycleIntegrity prevents ExportAccount from writing a
// checksummed archive whose terminal AVK lifecycle cannot pass the current
// importer. The message is intentionally value-free: callers should not need
// key or secret identifiers to handle damaged lifecycle state safely.
var ErrVaultLifecycleIntegrity = errors.New("vault key lifecycle state is not portable")

// SchemaVersion is the highest embedded migration number — the schema
// coordinate written into every archive manifest.
func SchemaVersion() int {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return 0
	}
	highest := 0
	for _, e := range entries {
		name := e.Name()
		if i := strings.IndexByte(name, '_'); i > 0 {
			if n, err := strconv.Atoi(name[:i]); err == nil && n > highest {
				highest = n
			}
		}
	}
	return highest
}

// ExportAccount streams the account's complete logical archive to w. The
// account must be suspended or closed: the operational write freeze prevents
// legitimate new mutations, while one REPEATABLE READ transaction holds an
// exclusive row lock on the account and guarantees every table streams from
// the same PostgreSQL snapshot. The row lock serializes concurrent exports as
// well as preventing a concurrent resume until the archive has finished. Row order
// inside tables is stable (primary key) so repeated exports are deterministic
// apart from manifest time metadata.
func (s *Store) ExportAccount(ctx context.Context, accountID, cellName, serverVersion string, w io.Writer) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead,
	})
	if err != nil {
		return fmt.Errorf("begin export snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// PostgreSQL's bytea_output setting is role/session configurable. Archives
	// are a portable wire format, so force the canonical hex representation
	// instead of allowing a role configured for legacy escape output to produce
	// an archive that another cell cannot validate.
	if _, err := tx.Exec(ctx, `SET LOCAL bytea_output = 'hex'`); err != nil {
		return fmt.Errorf("set canonical export bytea encoding: %w", err)
	}

	var status string
	err = tx.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id = $1 FOR UPDATE`, accountID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("verify export target: %w", err)
	}
	if status != "suspended" && status != "closed" {
		return ErrAccountNotExportable
	}
	if err := expireAccountVaultKeyEnrollmentsTx(ctx, tx, accountID); err != nil {
		return fmt.Errorf("expire vault key enrollments before export: %w", err)
	}
	var activeEnrollments, openRotations, orphanPendingKeys int64
	if err := tx.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM agent_vault_key_enrollments
		    WHERE account_id=$1 AND lifecycle_state IN ('pending','approved')),
		  (SELECT count(*) FROM agent_vault_key_rotations
		    WHERE account_id=$1 AND lifecycle_state='open'),
		  (SELECT count(*) FROM agent_vault_keys k
		    WHERE k.account_id=$1 AND k.lifecycle_state='pending'
		      AND NOT EXISTS (
		        SELECT 1 FROM agent_vault_key_rotations r
		         WHERE r.account_id=k.account_id
		           AND r.realm_id=k.realm_id
		           AND r.owner_agent_id=k.owner_agent_id
		           AND r.target_key_id=k.id
		           AND r.target_key_version=k.key_version
		           AND r.lifecycle_state='open'
		      ))`, accountID).Scan(
		&activeEnrollments, &openRotations, &orphanPendingKeys); err != nil {
		return fmt.Errorf("check vault key lifecycle before export: %w", err)
	}
	if activeEnrollments != 0 || openRotations != 0 || orphanPendingKeys != 0 {
		return ErrVaultLifecycleInProgress
	}
	// Terminal lifecycle history is portable only when no staging workspace
	// remains and its cross-table key states match the import contract. These
	// invariants are maintained transactionally by normal mutations but cannot
	// be expressed as row-local SQL constraints, so catch manually damaged or
	// legacy state before the archive writer receives any bytes.
	var nonPortableVaultLifecycle bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM agent_vault_key_rotation_items
		   WHERE account_id=$1
		) OR EXISTS (
		  SELECT 1
		    FROM agent_vault_key_rotations r
		    JOIN agent_vault_keys source
		      ON source.account_id=r.account_id
		     AND source.realm_id=r.realm_id
		     AND source.owner_agent_id=r.owner_agent_id
		     AND source.id=r.source_key_id
		     AND source.key_version=r.source_key_version
		    JOIN agent_vault_keys target
		      ON target.account_id=r.account_id
		     AND target.realm_id=r.realm_id
		     AND target.owner_agent_id=r.owner_agent_id
		     AND target.id=r.target_key_id
		     AND target.key_version=r.target_key_version
		   WHERE r.account_id=$1
		     AND (
		       source.lifecycle_state='pending'
		       OR (r.lifecycle_state='committed' AND
		           (target.lifecycle_state='pending' OR r.staged_count<>r.item_count))
		       OR (r.lifecycle_state='cancelled' AND target.lifecycle_state<>'retired')
		     )
		)`, accountID).Scan(&nonPortableVaultLifecycle); err != nil {
		return fmt.Errorf("check terminal vault key lifecycle before export: %w", err)
	}
	if nonPortableVaultLifecycle {
		return ErrVaultLifecycleIntegrity
	}
	// A schema-50 pod may have inserted a full avatar while schema 51 was
	// already live. The frozen account cannot acquire another legitimate avatar
	// write, so repair those nullable application-derived digests in bounded
	// memory before streaming a current-schema portable archive. A compacted row
	// without a digest is unrecoverable and fails the export closed.
	if _, err := backfillAvatarLockedLayerDigestsTx(ctx, tx,
		avatarLockedLayerDigestBackfillFilter{accountID: accountID}); err != nil {
		return fmt.Errorf("repair avatar digests before export: %w", err)
	}
	// Export itself is a legitimate lazy-expiry touch. Materialize every due
	// request and cancel its active fences before the snapshot streams, so an
	// archive can never carry time-expired authority as state=open.
	if _, _, err := drainMessageRequestReconciliationTx(ctx, tx, accountID); err != nil {
		return fmt.Errorf("expire message requests before export: %w", err)
	}
	// Bind archive time to the same database clock that authored row
	// timestamps. This guarantees every legitimate profile/vector timestamp is
	// at or before the manifest even when the app host and database clocks have
	// small differences.
	var exportedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&exportedAt); err != nil {
		return fmt.Errorf("read export snapshot time: %w", err)
	}

	// Tables stream in FOREIGN-KEY DEPENDENCY ORDER (tokens reference
	// operators and agents; agents reference realms) so a streaming importer
	// can insert every row the moment it arrives, no buffering, no deferred
	// constraints.
	sources := []export.RowSource{
		&querySource{tx: tx, table: "accounts", q: `
			SELECT jsonb_build_object(
			  'id', id, 'is_default', is_default, 'display_name', display_name,
			  'email', email, 'status', status, 'created_at', created_at,
			  'closed_at', closed_at, 'closed_reason', closed_reason,
			  'suspended_at', suspended_at, 'suspended_for', suspended_for,
			  'suspended_reason', suspended_reason,
			  'support_policy', support_policy,
			  'plan', plan, 'plan_limits', plan_limits,
			  'plan_features', plan_features,
			  'placement_policy', placement_policy)
			FROM accounts WHERE id = $1`, arg: accountID},
		&querySource{tx: tx, table: "operators", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'role', role, 'is_root', is_root,
			  'display_name', display_name, 'created_at', created_at,
			  'updated_at', updated_at, 'deleted_at', deleted_at)
			FROM operators WHERE account_id = $1 ORDER BY id`, arg: accountID},
		&querySource{tx: tx, table: "realms", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'name', name,
			  'created_at', created_at, 'updated_at', updated_at,
			  'deleted_at', deleted_at)
			FROM realms WHERE account_id = $1 ORDER BY id`, arg: accountID},
		// Realm avatar style heads and immutable versions precede agents so
		// profiles can reference the selected style as soon as they stream.
		// The current-version head foreign key is deferred because the head and
		// immutable version intentionally form one portable aggregate.
		&querySource{tx: tx, table: "avatar_style_packs", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id, 'id', id,
			  'current_version', current_version, 'revision', revision,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM avatar_style_packs WHERE account_id = $1
			ORDER BY realm_id, id`, arg: accountID},
		&querySource{tx: tx, table: "avatar_style_pack_versions", q: `
			WITH RECURSIVE style_order AS (
			  SELECT v.*, 0 AS chain_depth
			    FROM avatar_style_pack_versions v
			   WHERE v.account_id = $1 AND v.previous_version IS NULL
			  UNION ALL
			  SELECT child.*, parent.chain_depth + 1
			    FROM avatar_style_pack_versions child
			    JOIN style_order parent
			      ON child.account_id=parent.account_id
			     AND child.realm_id=parent.realm_id
			     AND child.style_pack_id=parent.style_pack_id
			     AND child.previous_version=parent.version
			   WHERE child.account_id = $1
			)
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'style_pack_id', style_pack_id, 'version', version,
			  'previous_version', previous_version, 'name', name,
			  'description', description, 'style_spec', style_spec,
			  'reference_examples', reference_examples,
			  'provenance', provenance, 'created_by_kind', created_by_kind,
			  'created_by_id', created_by_id, 'created_at', created_at)
			FROM style_order
			ORDER BY realm_id, style_pack_id, chain_depth, version`, arg: accountID},
		&querySource{tx: tx, table: "realm_avatar_styles", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'style_pack_id', style_pack_id,
			  'style_pack_version', style_pack_version,
			  'revision', revision, 'created_at', created_at,
			  'updated_at', updated_at)
			FROM realm_avatar_styles WHERE account_id = $1
			ORDER BY realm_id`, arg: accountID},
		&querySource{tx: tx, table: "avatar_style_rollout_jobs", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'style_revision', style_revision,
			  'style_pack_id', style_pack_id,
			  'style_pack_version', style_pack_version,
			  'status', status, 'target_profile_count', target_profile_count,
			  'processed_profile_count', processed_profile_count,
			  'batch_count', batch_count, 'last_batch_size', last_batch_size,
			  'failure_count', failure_count, 'retry_after', retry_after,
			  'last_failure_code', last_failure_code,
			  'created_at', created_at, 'started_at', started_at,
			  'updated_at', updated_at, 'completed_at', completed_at,
			  'superseded_at', superseded_at)
			FROM avatar_style_rollout_jobs WHERE account_id = $1
			ORDER BY realm_id, style_revision`, arg: accountID},
		&querySource{tx: tx, table: "agents", q: `
			SELECT jsonb_build_object(
			  'id', a.id, 'realm_id', a.realm_id, 'name', a.name,
			  'created_at', a.created_at, 'updated_at', a.updated_at,
			  'deleted_at', a.deleted_at)
			FROM agents a JOIN realms r ON r.id = a.realm_id
			WHERE r.account_id = $1 ORDER BY a.id`, arg: accountID},
		// The AVK itself is never exported. These streams preserve only its
		// public binding plus byte-identical ciphertext and wrapped DEKs so the
		// same client-held key can reopen the vault after a cell move.
		&querySource{tx: tx, table: "agent_vault_keys", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'key_version', key_version,
			  'algorithm', algorithm, 'fingerprint', fingerprint,
			  'lifecycle_state', lifecycle_state, 'row_version', row_version,
			  'created_at', created_at, 'retired_at', retired_at)
			FROM agent_vault_keys WHERE account_id = $1
			ORDER BY realm_id, owner_agent_id, key_version, id`, arg: accountID},
		&querySource{tx: tx, table: "agent_vault_key_enrollments", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'vault_key_id', vault_key_id,
			  'vault_key_version', vault_key_version,
			  'target_location_id', target_location_id,
			  'target_location_name', target_location_name,
			  'target_public_key', target_public_key,
			  'target_key_algorithm', target_key_algorithm,
			  'pairing_commitment', pairing_commitment,
			  'lifecycle_state', lifecycle_state,
			  'source_location_id', source_location_id,
			  'source_ephemeral_public_key', source_ephemeral_public_key,
			  'transfer_ciphertext', CASE WHEN transfer_ciphertext IS NULL THEN NULL
			                           ELSE E'\\x' || encode(transfer_ciphertext, 'hex') END,
			  'transfer_algorithm', transfer_algorithm,
			  'consume_commitment', consume_commitment,
			  'row_version', row_version, 'created_at', created_at,
			  'expires_at', expires_at, 'approved_at', approved_at,
			  'consumed_at', consumed_at, 'cancelled_at', cancelled_at,
			  'expired_at', expired_at)
			FROM agent_vault_key_enrollments WHERE account_id = $1
			ORDER BY realm_id, owner_agent_id, created_at, id`, arg: accountID},
		&querySource{tx: tx, table: "vault_key_enrollment_receipts", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'operation', operation,
			  'idempotency_key_hash', idempotency_key_hash,
			  'request_hash', request_hash, 'enrollment_id', enrollment_id,
			  'result_revision', result_revision, 'created_at', created_at)
			FROM vault_key_enrollment_receipts WHERE account_id = $1
			ORDER BY realm_id, owner_agent_id, operation, idempotency_key_hash`, arg: accountID},
		&querySource{tx: tx, table: "secrets", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'name', name,
			  'description', description, 'template', template, 'tags', tags,
			  'row_version', row_version, 'created_at', created_at,
			  'updated_at', updated_at, 'archived_at', archived_at,
			  'deleted_at', deleted_at)
			FROM secrets WHERE account_id = $1
			ORDER BY realm_id, owner_agent_id, created_at, id`, arg: accountID},
		&querySource{tx: tx, table: "secret_fields", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'secret_id', secret_id,
			  'name', name, 'field_kind', field_kind, 'sensitive', sensitive,
			  'value_encoding', value_encoding, 'value_version', value_version,
			  'public_value', public_value, 'envelope_version', envelope_version,
			  'ciphertext', CASE WHEN ciphertext IS NULL THEN NULL
			                     ELSE E'\\x' || encode(ciphertext, 'hex') END,
			  'aead_algorithm', aead_algorithm, 'aad_version', aad_version,
			  'dek_id', dek_id, 'dek_generation', dek_generation,
			  'row_version', row_version, 'created_at', created_at,
			  'updated_at', updated_at)
			FROM secret_fields WHERE account_id = $1
			ORDER BY realm_id, owner_agent_id, secret_id, name, id`, arg: accountID},
		&querySource{tx: tx, table: "secret_deks", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'secret_id', secret_id,
			  'field_id', field_id, 'dek_generation', dek_generation,
			  'wrapped_dek', E'\\x' || encode(wrapped_dek, 'hex'),
			  'wrap_algorithm', wrap_algorithm,
			  'aad_version', aad_version, 'wrap_revision', wrap_revision,
			  'wrapping_key_id', wrapping_key_id,
			  'wrapping_key_version', wrapping_key_version,
			  'row_version', row_version, 'created_at', created_at,
			  'retired_at', retired_at)
			FROM secret_deks WHERE account_id = $1
			ORDER BY realm_id, owner_agent_id, secret_id, field_id,
			         dek_generation, id`, arg: accountID},
		&querySource{tx: tx, table: "agent_vault_key_rotations", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id,
			  'source_key_id', source_key_id, 'source_key_version', source_key_version,
			  'target_key_id', target_key_id, 'target_key_version', target_key_version,
			  'lifecycle_state', lifecycle_state,
			  'recovery_disposition_mode', recovery_disposition_mode,
			  'recovery_artifact_sha256', recovery_artifact_sha256,
			  'item_count', item_count,
			  'staged_count', staged_count, 'row_version', row_version,
			  'created_at', created_at, 'updated_at', updated_at,
			  'committed_at', committed_at, 'cancelled_at', cancelled_at)
			FROM agent_vault_key_rotations WHERE account_id = $1
			ORDER BY realm_id, owner_agent_id, created_at, id`, arg: accountID},
		&querySource{tx: tx, table: "agent_vault_key_rotation_items", q: `
			SELECT jsonb_build_object(
			  'rotation_id', rotation_id, 'account_id', account_id,
			  'realm_id', realm_id, 'owner_agent_id', owner_agent_id,
			  'secret_id', secret_id, 'field_id', field_id, 'dek_id', dek_id,
			  'dek_generation', dek_generation,
			  'source_dek_row_version', source_dek_row_version,
			  'source_wrap_revision', source_wrap_revision,
			  'source_wrapped_dek', E'\\x' || encode(source_wrapped_dek, 'hex'),
			  'source_wrap_algorithm', source_wrap_algorithm,
			  'source_aad_version', source_aad_version,
			  'source_wrapping_key_id', source_wrapping_key_id,
			  'source_wrapping_key_version', source_wrapping_key_version,
			  'target_wrapped_dek', CASE WHEN target_wrapped_dek IS NULL THEN NULL
			                         ELSE E'\\x' || encode(target_wrapped_dek, 'hex') END,
			  'target_wrap_revision', target_wrap_revision,
			  'target_wrapper_sha256', target_wrapper_sha256,
			  'staged_at', staged_at)
			FROM agent_vault_key_rotation_items WHERE account_id = $1
			ORDER BY realm_id, owner_agent_id, rotation_id, dek_id`, arg: accountID},
		&querySource{tx: tx, table: "vault_key_rotation_receipts", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'operation', operation,
			  'idempotency_key_hash', idempotency_key_hash,
			  'request_hash', request_hash, 'rotation_id', rotation_id,
			  'result_revision', result_revision, 'created_at', created_at)
			FROM vault_key_rotation_receipts WHERE account_id = $1
			ORDER BY realm_id, owner_agent_id, operation, idempotency_key_hash`, arg: accountID},
		&querySource{tx: tx, table: "secret_mutation_receipts", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'actor_kind', actor_kind,
			  'actor_id', actor_id, 'operation', operation,
			  'idempotency_key_hash', idempotency_key_hash,
			  'request_hash', request_hash, 'target_kind', target_kind,
			  'target_id', target_id, 'result_revision', result_revision,
			  'result_value_version', result_value_version,
			  'created_at', created_at)
			FROM secret_mutation_receipts WHERE account_id = $1
			ORDER BY realm_id, owner_agent_id, actor_kind, actor_id,
			         operation, idempotency_key_hash`, arg: accountID},
		&querySource{tx: tx, table: "agent_avatar_profiles", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'agent_id', agent_id, 'status', status,
			  'lineage_generation', lineage_generation,
			  'autonomy_policy', autonomy_policy,
			  'style_pack_id', style_pack_id,
			  'style_pack_version', style_pack_version,
			  'style_revision', style_revision,
			  'latest_avatar_version', latest_avatar_version,
			  'proposed_avatar_version', proposed_avatar_version,
			  'active_avatar_version', active_avatar_version,
			  'subject_form', subject_form, 'attempt_count', attempt_count,
			  'retry_after', retry_after, 'fallback_seed', fallback_seed,
			  'failure_code', failure_code,
			  'retained_payload_count_limit', retained_payload_count_limit,
			  'retained_payload_byte_limit', retained_payload_byte_limit,
			  'payload_quota_reconciliation_required',
			    payload_quota_reconciliation_required,
			  'revision', revision,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM agent_avatar_profiles WHERE account_id = $1
			ORDER BY realm_id, agent_id`, arg: accountID},
		&querySource{tx: tx, table: "agent_avatar_versions", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'agent_id', agent_id, 'id', id, 'version', version,
			  'lineage_generation', lineage_generation,
			  'parent_version', parent_version,
			  'style_pack_id', style_pack_id,
			  'style_pack_version', style_pack_version,
			  'subject_form', subject_form, 'svg', svg,
			  'description', description, 'visual_spec', visual_spec,
			  'svg_sha256', svg_sha256,
			  'locked_layers_sha256', locked_layers_sha256,
			  'renderer_profile', renderer_profile,
			  'continuity_fingerprint', continuity_fingerprint,
			  'provenance', provenance,
			  'payload_state', payload_state, 'payload_bytes', payload_bytes,
			  'payload_compacted_at', payload_compacted_at,
			  'payload_compaction_reason', payload_compaction_reason,
			  'proposed_by_kind', proposed_by_kind,
			  'proposed_by_id', proposed_by_id,
			  'proposed_at', proposed_at)
			FROM agent_avatar_versions WHERE account_id = $1
			ORDER BY realm_id, agent_id, version`, arg: accountID},
		&querySource{tx: tx, table: "agent_avatar_activations", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'agent_id', agent_id, 'sequence', sequence,
			  'lineage_generation', lineage_generation,
			  'avatar_version', avatar_version,
			  'prior_active_version', prior_active_version, 'action', action,
			  'activated_by_kind', activated_by_kind,
			  'activated_by_id', activated_by_id,
			  'activated_at', activated_at)
			FROM agent_avatar_activations WHERE account_id = $1
			ORDER BY realm_id, agent_id, sequence`, arg: accountID},
		&querySource{tx: tx, table: "agent_avatar_rejections", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'agent_id', agent_id, 'avatar_version', avatar_version,
			  'reason_code', reason_code,
			  'rejected_by_kind', rejected_by_kind,
			  'rejected_by_id', rejected_by_id,
			  'rejected_at', rejected_at)
			FROM agent_avatar_rejections WHERE account_id = $1
			ORDER BY rejected_at, id`, arg: accountID},
		&querySource{tx: tx, table: "agent_avatar_resets", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'agent_id', agent_id, 'sequence', sequence,
			  'retired_lineage_generation', retired_lineage_generation,
			  'new_lineage_generation', new_lineage_generation,
			  'retired_active_version', retired_active_version,
			  'retired_proposed_version', retired_proposed_version,
			  'reason_code', reason_code,
			  'reset_by_kind', reset_by_kind, 'reset_by_id', reset_by_id,
			  'reset_at', reset_at)
			FROM agent_avatar_resets WHERE account_id = $1
			ORDER BY realm_id, agent_id, sequence`, arg: accountID},
		&querySource{tx: tx, table: "avatar_mutation_receipts", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'target_kind', target_kind, 'target_id', target_id,
			  'actor_kind', actor_kind, 'actor_id', actor_id,
			  'operation', operation, 'idempotency_key', idempotency_key,
			  'request_hash', request_hash, 'result_revision', result_revision,
			  'result_version', result_version,
			  'result_lineage_generation', result_lineage_generation,
			  'created_at', created_at)
			FROM avatar_mutation_receipts WHERE account_id = $1
			ORDER BY realm_id, target_kind, target_id, actor_kind, actor_id,
			         operation, idempotency_key`, arg: accountID},
		&querySource{tx: tx, table: "agent_activity", q: `
			SELECT jsonb_build_object(
			  'agent_id', aa.agent_id, 'runtime', aa.runtime,
			  'location_id', aa.location_id, 'location', aa.location,
			  'last_event', aa.last_event, 'last_event_id', aa.last_event_id,
			  'last_event_occurred_at', aa.last_event_occurred_at,
			  'last_activity_at', aa.last_activity_at)
			FROM agent_activity aa
			JOIN agents a ON a.id = aa.agent_id
			JOIN realms r ON r.id = a.realm_id
			WHERE r.account_id = $1
			ORDER BY aa.agent_id, aa.runtime, aa.location_id`, arg: accountID},
		// Dashboard UI preferences are agent-owned account data: the theme
		// choice follows the agent across cells, so the strictly validated
		// prefs document rides the archive like every other per-agent row.
		&querySource{tx: tx, table: "agent_dashboard_preferences", q: `
			SELECT jsonb_build_object(
			  'agent_id', agent_id, 'account_id', account_id,
			  'realm_id', realm_id, 'prefs', prefs, 'updated_at', updated_at)
			FROM agent_dashboard_preferences WHERE account_id = $1
			ORDER BY agent_id`, arg: accountID},
		&querySource{tx: tx, table: "fact_subjects", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'canonical_key', canonical_key,
			  'display_name', display_name, 'aliases', aliases,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM fact_subjects WHERE account_id = $1 ORDER BY id`, arg: accountID},
		&querySource{tx: tx, table: "facts", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'subject_id', subject_id,
			  'predicate', predicate, 'cardinality', cardinality,
			  'sensitive', sensitive, 'resolved_assertion_id', resolved_assertion_id,
			  'deleted_at', deleted_at,
			  'deleted_by_agent_id', deleted_by_agent_id,
			  'delete_receipt_id', delete_receipt_id,
			  'delete_idempotency_key_hash', delete_idempotency_key_hash,
			  'deleted_prior_assertion_id', deleted_prior_assertion_id,
			  'deleted_assertion_count', deleted_assertion_count,
			  'deleted_candidate_count', deleted_candidate_count,
			  'deleted_usage_count', deleted_usage_count,
			  'deleted_mutation_key_count', deleted_mutation_key_count,
			  'deleted_candidate_revision', deleted_candidate_revision,
			  'recreated_at', recreated_at,
			  'replacement_fact_id', replacement_fact_id,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM facts WHERE account_id = $1 ORDER BY id`, arg: accountID},
		&querySource{tx: tx, table: "fact_mutation_tombstones", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'fact_id', fact_id,
			  'surface', surface,
			  'idempotency_key_hash', idempotency_key_hash,
			  'deleted_at', deleted_at)
			FROM fact_mutation_tombstones WHERE account_id = $1
			ORDER BY fact_id, surface, id`, arg: accountID},
		&querySource{tx: tx, table: "fact_assertions", q: `
			WITH RECURSIVE assertion_order AS (
			  SELECT a.*, 0 AS chain_depth
			  FROM fact_assertions a
			  WHERE a.account_id = $1 AND a.supersedes_id IS NULL
			  UNION ALL
			  SELECT child.*, parent.chain_depth + 1
			  FROM fact_assertions child
			  JOIN assertion_order parent ON child.supersedes_id = parent.id
			  WHERE child.account_id = $1
			)
			SELECT jsonb_build_object(
			  'id', id, 'fact_id', fact_id, 'account_id', account_id,
			  'realm_id', realm_id, 'asserted_by_agent_id', asserted_by_agent_id,
			  'value_type', value_type, 'value', value,
			  'recurrence', recurrence,
			  'source_kind', source_kind, 'source_ref', source_ref,
			  'confidence', confidence, 'observed_at', observed_at,
			  'confirmed_at', confirmed_at, 'valid_from', valid_from,
			  'valid_until', valid_until, 'supersedes_id', supersedes_id,
			  'idempotency_key', idempotency_key,
			  'idempotency_fingerprint', idempotency_fingerprint,
			  'created_at', created_at)
			FROM assertion_order
			ORDER BY fact_id, chain_depth, created_at, id`, arg: accountID},
		&querySource{tx: tx, table: "fact_candidates", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'subject_key', subject_key,
			  'predicate', predicate, 'value_type', value_type, 'value', value,
			  'recurrence', recurrence,
			  'cardinality', cardinality, 'sensitive', sensitive,
			  'source_ref', source_ref, 'confidence', confidence,
			  'observed_at', observed_at, 'valid_from', valid_from,
			  'valid_until', valid_until,
			  'reason', reason, 'status', status,
			  'conflict_fact_id', conflict_fact_id,
			  'observed_assertion_id', observed_assertion_id,
			  'resolved_fact_id', resolved_fact_id,
			  'decision_assertion_id', decision_assertion_id,
			  'idempotency_key', idempotency_key,
			  'idempotency_fingerprint', idempotency_fingerprint,
			  'decision_idempotency_key', decision_idempotency_key,
			  'curation_run_id', curation_run_id,
			  'curation_action_id', curation_action_id,
			  'withdrawal_reason', withdrawal_reason,
			  'withdrawal_idempotency_key', withdrawal_idempotency_key,
			  'withdrawal_request_hash', withdrawal_request_hash,
			  'proposed_at', proposed_at, 'decided_at', decided_at)
			FROM fact_candidates WHERE account_id = $1
			ORDER BY proposed_at, id`, arg: accountID},
		&querySource{tx: tx, table: "tokens", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'operator_id', operator_id,
			  'agent_id', agent_id, 'kind', kind, 'token_hash', token_hash,
			  'display_name', display_name, 'access_profile', access_profile,
			  'created_at', created_at,
			  'expires_at', expires_at, 'consumed_at', consumed_at)
			FROM tokens WHERE account_id = $1 ORDER BY id`, arg: accountID},
		// Transcript conversations depend on realms + agents; entries depend
		// on their conversation and may reply only to an earlier entry. Stable
		// sequence order therefore makes this stream directly insertable.
		&querySource{tx: tx, table: "transcript_conversations", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'external_id', external_id,
			  'title', title, 'metadata', metadata,
			  'next_sequence', next_sequence,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM transcript_conversations WHERE account_id = $1
			ORDER BY created_at, id`, arg: accountID},
		&querySource{tx: tx, table: "transcript_entries", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id,
			  'transcript_id', transcript_id, 'realm_id', realm_id,
			  'recorded_by_agent_id', recorded_by_agent_id,
			  'sequence', sequence, 'external_id', external_id,
			  'role', role, 'body', body,
			  'payload', payload, 'model', model,
			  'reply_to_entry_id', reply_to_entry_id,
			  'artifacts', artifacts, 'created_at', created_at)
			FROM transcript_entries WHERE account_id = $1
			ORDER BY transcript_id, sequence, id`, arg: accountID},
		// Usage facts and their fast projections are account-owned data. Both
		// are preserved so a moved account keeps exact history and can serve
		// usage immediately without rebuilding rollups during import.
		&querySource{tx: tx, table: "usage_events", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'agent_id', agent_id, 'dimension', dimension,
			  'quantity', quantity, 'unit', unit,
			  'subject_type', subject_type, 'subject_id', subject_id,
			  'idempotency_key', idempotency_key, 'metadata', metadata,
			  'occurred_at', occurred_at, 'created_at', created_at)
			FROM usage_events WHERE account_id = $1
			ORDER BY occurred_at, id`, arg: accountID},
		&querySource{tx: tx, table: "usage_rollups", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'agent_id', agent_id, 'dimension', dimension, 'unit', unit,
			  'bucket', bucket, 'bucket_start', bucket_start,
			  'quantity', quantity, 'event_count', event_count,
			  'updated_at', updated_at)
			FROM usage_rollups WHERE account_id = $1
			ORDER BY bucket, bucket_start, agent_id, dimension, unit`, arg: accountID},
		// Messages depend on realms + agents; recipient delivery state depends
		// on its message. Preserve bodies here because the account archive is
		// the durable, encrypted migration unit for all account-owned data.
		&querySource{tx: tx, table: "agent_messages", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'from_agent_id', from_agent_id, 'to_agent_id', to_agent_id,
			  'audience_kind', audience_kind,
			  'audience_fingerprint', audience_fingerprint,
			  'subject', subject, 'kind', kind, 'body', body,
			  'payload', payload, 'thread_id', thread_id,
			  'reply_to_message_id', reply_to_message_id,
			  'causal_depth', causal_depth,
			  'idempotency_key', idempotency_key, 'created_at', created_at)
			FROM agent_messages WHERE account_id = $1
			ORDER BY created_at, id`, arg: accountID},
		&querySource{tx: tx, table: "agent_message_deliveries", q: `
			SELECT jsonb_build_object(
			  'message_id', message_id, 'account_id', account_id,
			  'realm_id', realm_id, 'recipient_agent_id', recipient_agent_id,
			  'state', state, 'delivered_at', delivered_at,
			  'read_at', read_at, 'acked_at', acked_at,
			  'processing_state', processing_state,
			  'processing_generation', processing_generation,
			  'failure_count', failure_count,
			  'claim_id', claim_id, 'claim_key_hash', claim_key_hash,
			  'lease_expires_at', lease_expires_at,
			  'completed_at', completed_at,
			  'complete_key_hash', complete_key_hash,
			  'result_message_id', result_message_id,
			  'created_at', created_at)
			FROM agent_message_deliveries WHERE account_id = $1
			ORDER BY created_at, message_id, recipient_agent_id`, arg: accountID},
		&querySource{tx: tx, table: "agent_message_requests", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'opening_message_id', opening_message_id,
			  'coordinator_agent_id', coordinator_agent_id,
			  'selection_policy', selection_policy, 'state', state,
			  'max_assignees', max_assignees,
			  'offer_window_seconds', offer_window_seconds,
			  'expires_in_seconds', expires_in_seconds,
			  'offer_deadline', offer_deadline, 'expires_at', expires_at,
			  'selection_generation', selection_generation,
			  'completed_at', completed_at, 'cancelled_at', cancelled_at,
			  'expired_at', expired_at,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM agent_message_requests WHERE account_id = $1
			ORDER BY created_at, id`, arg: accountID},
		&querySource{tx: tx, table: "agent_message_request_candidates", q: `
			SELECT jsonb_build_object(
			  'request_id', request_id, 'account_id', account_id,
			  'realm_id', realm_id, 'agent_id', agent_id,
			  'response_state', response_state,
			  'offer_message_id', offer_message_id,
			  'offer_key_hash', offer_key_hash,
			  'offer_request_hash', offer_request_hash,
			  'responded_at', responded_at, 'created_at', created_at)
			FROM agent_message_request_candidates WHERE account_id = $1
			ORDER BY request_id, agent_id`, arg: accountID},
		&querySource{tx: tx, table: "agent_message_request_selections", q: `
			SELECT jsonb_build_object(
			  'id', id, 'request_id', request_id,
			  'account_id', account_id, 'realm_id', realm_id,
			  'coordinator_agent_id', coordinator_agent_id,
			  'generation', generation,
			  'idempotency_key_hash', idempotency_key_hash,
			  'selection_hash', selection_hash, 'created_at', created_at)
			FROM agent_message_request_selections WHERE account_id = $1
			ORDER BY request_id, generation, id`, arg: accountID},
		&querySource{tx: tx, table: "agent_message_request_claims", q: `
			SELECT jsonb_build_object(
			  'id', id, 'request_id', request_id,
			  'selection_id', selection_id,
			  'account_id', account_id, 'realm_id', realm_id,
			  'agent_id', agent_id, 'state', state,
			  'generation', generation,
			  'claim_key_hash', claim_key_hash,
			  'lease_expires_at', lease_expires_at,
			  'failure_count', failure_count,
			  'complete_key_hash', complete_key_hash,
			  'result_message_id', result_message_id,
			  'selected_at', selected_at, 'claimed_at', claimed_at,
			  'released_at', released_at, 'completed_at', completed_at,
			  'cancelled_at', cancelled_at, 'updated_at', updated_at)
			FROM agent_message_request_claims WHERE account_id = $1
			ORDER BY request_id, selection_id, agent_id, id`, arg: accountID},
		// Memory evidence may refer to transcripts, messages, or other memory
		// versions, so the external interaction sources above must land first.
		// Heads and versions form a deferrable FK cycle and can stream directly
		// in this order inside the importer's single transaction.
		&querySource{tx: tx, table: "memory_change_clocks", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'last_change_seq', last_change_seq,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM memory_change_clocks WHERE account_id = $1
			ORDER BY realm_id, owner_kind, owner_id`, arg: accountID},
		&querySource{tx: tx, table: "memories", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'origin', origin, 'capture_reason', capture_reason,
			  'authored_by_agent_id', authored_by_agent_id,
			  'current_version', current_version,
			  'permanently_deleted_at', permanently_deleted_at,
			  'permanently_deleted_by_id', permanently_deleted_by_id,
			  'permanent_delete_reason', permanent_delete_reason,
			  'delete_receipt_id', delete_receipt_id,
			  'delete_idempotency_key_hash', delete_idempotency_key_hash,
			  'deleted_prior_version', deleted_prior_version,
			  'deleted_scrub_set_revision', deleted_scrub_set_revision,
			  'deleted_version_count', deleted_version_count,
			  'deleted_evidence_count', deleted_evidence_count,
			  'deleted_relation_count', deleted_relation_count,
			  'deleted_retry_shield_count', deleted_retry_shield_count,
			  'deleted_retry_shield_digest', deleted_retry_shield_digest,
			  'deleted_curation_run_count', deleted_curation_run_count,
			  'deleted_curation_action_count', deleted_curation_action_count,
			  'deleted_curation_input_count', deleted_curation_input_count,
			  'deleted_curation_mutation_count', deleted_curation_mutation_count,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM memories WHERE account_id = $1
			ORDER BY created_at, id`, arg: accountID},
		&querySource{tx: tx, table: "memory_versions", q: `
			SELECT jsonb_build_object(
			  'memory_id', memory_id, 'version', version,
			  'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'previous_version', previous_version, 'change_seq', change_seq,
			  'content', content, 'content_encoding', content_encoding,
			  'kind', kind, 'tags', tags, 'links', links,
			  'salience', salience, 'sensitive', sensitive,
			  'occurred_from', occurred_from, 'occurred_until', occurred_until,
			  'state', state, 'prior_state', prior_state,
			  'lifecycle_reason', lifecycle_reason,
			  'content_hash', content_hash,
			  'actor_kind', actor_kind, 'actor_id', actor_id,
			  'operation', operation, 'idempotency_key', idempotency_key,
			  'request_hash', request_hash,
			  'client_runtime', client_runtime, 'client_model', client_model,
			  'client_recipe', client_recipe,
			  'client_recipe_version', client_recipe_version,
			  'supersession_set_id', supersession_set_id,
			  'supersession_set_revision', supersession_set_revision,
			  'supersession_replacement_count', supersession_replacement_count,
			  'supersession_replacement_digest', supersession_replacement_digest,
			  'curation_run_id', curation_run_id,
			  'curation_action_id', curation_action_id,
			  'created_at', created_at)
			FROM memory_versions WHERE account_id = $1
			ORDER BY memory_id, version`, arg: accountID},
		&querySource{tx: tx, table: "memory_vector_profiles", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'provider', provider, 'model', model, 'recipe', recipe,
			  'recipe_version', recipe_version, 'dimensions', dimensions,
			  'distance_metric', distance_metric,
			  'normalization', normalization, 'contract_hash', contract_hash,
			  'created_by_agent_id', created_by_agent_id,
			  'created_at', created_at)
			FROM memory_vector_profiles WHERE account_id = $1
			ORDER BY created_at, id`, arg: accountID},
		&querySource{tx: tx, table: "memory_vectors", q: `
			SELECT jsonb_build_object(
			  'profile_id', profile_id, 'memory_id', memory_id,
			  'memory_version', memory_version,
			  'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'content_hash', content_hash, 'vector', vector,
			  'vector_hash', vector_hash,
			  'created_by_agent_id', created_by_agent_id,
			  'created_at', created_at)
			FROM memory_vectors WHERE account_id = $1
			ORDER BY profile_id, memory_id, memory_version`, arg: accountID},
		&querySource{tx: tx, table: "memory_evidence", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'memory_id', memory_id, 'target_version', target_version,
			  'evidence_change_seq', evidence_change_seq,
			  'evidence_type', evidence_type, 'role', role,
			  'resolution_state', resolution_state,
			  'external_locator', external_locator,
			  'pending_evidence_id', pending_evidence_id,
			  'resolved_kind', resolved_kind,
			  'source_transcript_id', source_transcript_id,
			  'source_sequence_from', source_sequence_from,
			  'source_sequence_until', source_sequence_until,
			  'source_memory_id', source_memory_id,
			  'source_memory_version', source_memory_version,
			  'source_message_id', source_message_id,
			  'source_import_locator', source_import_locator,
			  'artifact_excerpt', artifact_excerpt,
			  'artifact_sensitive', artifact_sensitive,
			  'terminal_reason_code', terminal_reason_code,
			  'source_digest', source_digest, 'actor_id', actor_id,
			  'idempotency_key', idempotency_key,
			  'request_hash', request_hash,
			  'created_at', created_at)
			FROM memory_evidence WHERE account_id = $1
			ORDER BY memory_id, target_version, evidence_change_seq, id`, arg: accountID},
		&querySource{tx: tx, table: "memory_relations", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'from_memory_id', from_memory_id, 'from_version', from_version,
			  'to_memory_id', to_memory_id, 'to_version', to_version,
			  'relation_type', relation_type,
			  'supersession_set_id', supersession_set_id,
			  'supersession_set_revision', supersession_set_revision,
			  'curation_run_id', curation_run_id,
			  'curation_action_id', curation_action_id,
			  'reverted_by_run_id', reverted_by_run_id,
			  'reverted_by_action_id', reverted_by_action_id,
			  'reverted_at', reverted_at, 'created_at', created_at)
			FROM memory_relations WHERE account_id = $1
			ORDER BY created_at, id`, arg: accountID},
		&querySource{tx: tx, table: "memory_deleted_references", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'deleted_memory_id', deleted_memory_id,
			  'former_reference_kind', former_reference_kind,
			  'related_resource_id', related_resource_id,
			  'curation_run_id', curation_run_id,
			  'curation_request_id', curation_request_id,
			  'reason_code', reason_code, 'created_at', created_at)
			FROM memory_deleted_references WHERE account_id = $1
			ORDER BY deleted_memory_id, created_at, id`, arg: accountID},
		&querySource{tx: tx, table: "memory_curation_lanes", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'request_generation', request_generation,
			  'fencing_generation', fencing_generation,
			  'active_run_id', active_run_id,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM memory_curation_lanes WHERE account_id = $1
			ORDER BY realm_id, owner_kind, owner_id`, arg: accountID},
		&querySource{tx: tx, table: "memory_curation_cursors", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'source_kind', source_kind, 'source_stream_id', source_stream_id,
			  'position', position,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM memory_curation_cursors WHERE account_id = $1
			ORDER BY realm_id, owner_kind, owner_id, source_kind, source_stream_id`, arg: accountID},
		&querySource{tx: tx, table: "memory_curation_requests", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'scope', scope, 'coalescing_key', coalescing_key,
			  'trigger_reason', trigger_reason,
			  'request_generation', request_generation,
			  'priority', priority, 'due_at', due_at, 'state', state,
			  'attempt_count', attempt_count, 'max_attempts', max_attempts,
			  'claimed_run_id', claimed_run_id,
			  'fulfilled_generation', fulfilled_generation,
			  'replay_run_id', replay_run_id,
			  'read_only_replay', read_only_replay,
			  'actor_kind', actor_kind, 'actor_id', actor_id,
			  'idempotency_key', idempotency_key, 'request_hash', request_hash,
			  'claimed_at', claimed_at, 'fulfilled_at', fulfilled_at,
			  'cancelled_at', cancelled_at, 'dead_lettered_at', dead_lettered_at,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM memory_curation_requests WHERE account_id = $1
			ORDER BY created_at, id`, arg: accountID},
		&querySource{tx: tx, table: "memory_curation_runs", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'request_id', request_id,
			  'request_generation', request_generation,
			  'fencing_generation', fencing_generation, 'state', state,
			  'actor_kind', actor_kind, 'actor_id', actor_id,
			  'idempotency_key', idempotency_key, 'request_hash', request_hash,
			  'lease_expires_at', lease_expires_at,
			  'client_runtime', client_runtime, 'client_model', client_model,
			  'client_recipe', client_recipe,
			  'client_recipe_version', client_recipe_version,
			  'memory_change_upper', memory_change_upper,
			  'evidence_change_upper', evidence_change_upper,
			  'input_count', input_count,
			  'memory_input_count', memory_input_count,
			  'evidence_input_count', evidence_input_count,
			  'transcript_input_count', transcript_input_count,
			  'cursor_input_count', cursor_input_count,
			  'plan_schema', plan_schema, 'plan_revision', plan_revision,
			  'plan_hash', plan_hash, 'apply_receipt_id', apply_receipt_id,
			  'rollback_receipt_id', rollback_receipt_id,
			  'conflict_reason_code', conflict_reason_code,
			  'terminal_reason_code', terminal_reason_code,
			  'budgets', budgets, 'scrubbed_at', scrubbed_at,
			  'scrubbed_reason_code', scrubbed_reason_code,
			  'started_at', started_at, 'planned_at', planned_at,
			  'applied_at', applied_at, 'rolled_back_at', rolled_back_at,
			  'terminal_at', terminal_at,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM memory_curation_runs WHERE account_id = $1
			ORDER BY created_at, id`, arg: accountID},
		&querySource{tx: tx, table: "memory_curation_run_inputs", q: `
			SELECT jsonb_build_object(
			  'run_id', run_id, 'ordinal', ordinal,
			  'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'input_kind', input_kind, 'order_key', order_key,
			  'memory_id', memory_id, 'memory_version', memory_version,
			  'evidence_id', evidence_id, 'transcript_id', transcript_id,
			  'sequence_from', sequence_from, 'sequence_until', sequence_until,
			  'cursor_source_kind', cursor_source_kind,
			  'cursor_stream_id', cursor_stream_id,
			  'cursor_expected_prior', cursor_expected_prior,
			  'cursor_upper', cursor_upper, 'created_at', created_at)
			FROM memory_curation_run_inputs WHERE account_id = $1
			ORDER BY run_id, ordinal`, arg: accountID},
		&querySource{tx: tx, table: "memory_curation_actions", q: `
			SELECT jsonb_build_object(
			  'id', id, 'run_id', run_id,
			  'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'ordinal', ordinal, 'plan_revision', plan_revision,
			  'primitive', primitive, 'state', state, 'local_ref', local_ref,
			  'input_refs', input_refs, 'expected_heads', expected_heads,
			  'proposed_payload', proposed_payload,
			  'validation_result', validation_result,
			  'applied_result', applied_result, 'rollback_result', rollback_result,
			  'action_hash', action_hash, 'scrubbed_at', scrubbed_at,
			  'scrubbed_reason_code', scrubbed_reason_code,
			  'created_at', created_at, 'validated_at', validated_at,
			  'applied_at', applied_at, 'reverted_at', reverted_at)
			FROM memory_curation_actions WHERE account_id = $1
			ORDER BY run_id, ordinal`, arg: accountID},
		&querySource{tx: tx, table: "memory_curation_mutations", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_kind', owner_kind, 'owner_id', owner_id,
			  'actor_kind', actor_kind, 'actor_id', actor_id,
			  'operation', operation, 'idempotency_key', idempotency_key,
			  'request_hash', request_hash, 'request_id', request_id,
			  'run_id', run_id, 'request_generation', request_generation,
			  'fencing_generation', fencing_generation,
			  'plan_revision', plan_revision, 'plan_hash', plan_hash,
			  'lease_expires_at', lease_expires_at,
			  'result_state', result_state, 'receipt_id', receipt_id,
			  'created_at', created_at)
			FROM memory_curation_mutations WHERE account_id = $1
			ORDER BY created_at, id`, arg: accountID},
		// account_events streams last because it has no outbound FKs
		// beyond account_id, and it is the append-only ledger — its rows
		// point AT the state changes recorded above, not the other way
		// around, so ordering it here keeps the restore side inserting
		// in the natural read order.
		&querySource{tx: tx, table: "account_events", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'occurred_at', occurred_at,
			  'actor_kind', actor_kind, 'actor_id', actor_id,
			  'verb', verb, 'metadata', metadata, 'retain_until', retain_until)
			FROM account_events WHERE account_id = $1
			ORDER BY occurred_at, id`, arg: accountID},
		// support_tickets + messages stream after account_events because
		// messages FK-depend on tickets AND on accounts; the importCtx
		// FK-validation reads ic.tickets which the tickets query
		// populates. Both queries emit every column of the base
		// migration so the round-trip preserves the shape exactly.
		&querySource{tx: tx, table: "support_tickets", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'opened_at', opened_at,
			  'opened_by_kind', opened_by_kind, 'opened_by_id', opened_by_id,
			  'subject', subject, 'category', category, 'state', state,
			  'priority', priority, 'first_response_at', first_response_at,
			  'resolved_at', resolved_at, 'closed_at', closed_at,
			  'last_activity_at', last_activity_at, 'last_message_id', last_message_id,
			  'correlation', correlation, 'metadata', metadata,
			  'retain_until', retain_until)
			FROM support_tickets WHERE account_id = $1
			ORDER BY opened_at, id`, arg: accountID},
		&querySource{tx: tx, table: "support_ticket_messages", q: `
			SELECT jsonb_build_object(
			  'id', id, 'ticket_id', ticket_id, 'account_id', account_id,
			  'posted_at', posted_at,
			  'author_kind', author_kind, 'author_id', author_id,
			  'body', body, 'attachments', attachments, 'metadata', metadata)
			FROM support_ticket_messages WHERE account_id = $1
			ORDER BY posted_at, id`, arg: accountID},
	}

	m := export.Manifest{
		SchemaVersion: SchemaVersion(),
		ServerVersion: serverVersion,
		AccountID:     accountID,
		Cell:          cellName,
		Status:        status,
		ExportedAt:    exportedAt.UTC(),
	}
	if err := export.Write(ctx, w, m, sources); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit export snapshot: %w", err)
	}
	return nil
}

// querySource streams one table's rows as JSON objects built by Postgres
// itself (jsonb_build_object), so field names are explicit and stable — the
// logical-format contract — and rows never pass through Go structs that
// could silently drop columns.
type querySource struct {
	tx    pgx.Tx
	table string
	q     string
	arg   string

	rows pgx.Rows
	done bool
}

func (qs *querySource) Table() string { return qs.table }

func (qs *querySource) Next(ctx context.Context) ([]byte, error) {
	if qs.done {
		return nil, nil
	}
	if qs.rows == nil {
		rows, err := qs.tx.Query(ctx, qs.q, qs.arg)
		if err != nil {
			return nil, err
		}
		qs.rows = rows
	}
	if !qs.rows.Next() {
		qs.done = true
		err := qs.rows.Err()
		qs.rows.Close()
		return nil, err
	}
	var raw json.RawMessage
	if err := qs.rows.Scan(&raw); err != nil {
		qs.rows.Close()
		qs.done = true
		return nil, err
	}
	// jsonb text output is already a single line — NDJSON-safe as-is.
	return raw, nil
}
