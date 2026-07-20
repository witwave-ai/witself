package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestImportedVaultLifecycleAcceptsOnlyTerminalValueFreeHistory(t *testing.T) {
	ic := testSecretImportContext(t)
	rows := testVaultLifecycleArchiveRows(t)
	for _, row := range rows {
		if err := ic.validateAndRecord(row.table, row.value); err != nil {
			t.Fatalf("validate %s: %v", row.table, err)
		}
	}
	if got := ic.vaultEnrollments["enr_dddddddddddddddd"].state; got != VaultEnrollmentStateConsumed {
		t.Fatalf("enrollment state = %q", got)
	}
	if got := ic.vaultRotations["vkr_eeeeeeeeeeeeeeee"].state; got != VaultKeyRotationCommitted {
		t.Fatalf("rotation state = %q", got)
	}
}

func TestImportedCommittedVaultRotationAcceptsRiskDispositionWithoutDigest(t *testing.T) {
	ic := testSecretImportContext(t)
	rows := testVaultLifecycleArchiveRows(t)
	rotation := rowByTable(rows, "agent_vault_key_rotations")
	rotation["recovery_disposition_mode"] = VaultKeyRotationRiskAccepted
	rotation["recovery_artifact_sha256"] = nil
	for _, row := range rows {
		if err := ic.validateAndRecord(row.table, row.value); err != nil {
			t.Fatalf("validate %s: %v", row.table, err)
		}
	}
}

func TestImportedVaultLifecycleResolvesReusedRetiredVersionByFullIdentity(t *testing.T) {
	ic := testSecretImportContext(t)
	base := testVaultLifecycleArchiveRows(t)
	exportedAt := testSecretArchiveTime(t)
	createdAt := exportedAt.Add(-time.Hour)
	cancelledAt := createdAt.Add(4 * time.Minute)
	cancelledKeyID := "avk_gggggggggggggggg"
	cancelledKey := map[string]any{
		"id": cancelledKeyID, "account_id": testSecretAccountID,
		"realm_id": testSecretRealmID, "owner_agent_id": testSecretAgentID,
		"key_version": int64(2), "algorithm": SecretAEADAlgorithm,
		"fingerprint": strings.Repeat("c", 64), "lifecycle_state": "retired",
		"row_version": int64(2), "created_at": createdAt.Add(2 * time.Minute).Format(time.RFC3339Nano),
		"retired_at": cancelledAt.Format(time.RFC3339Nano),
	}
	cancelledRotation := map[string]any{
		"id": "vkr_gggggggggggggggg", "account_id": testSecretAccountID,
		"realm_id": testSecretRealmID, "owner_agent_id": testSecretAgentID,
		"source_key_id": testSecretKeyID, "source_key_version": int64(1),
		"target_key_id": cancelledKeyID, "target_key_version": int64(2),
		"lifecycle_state": VaultKeyRotationCancelled, "item_count": int64(0),
		"recovery_disposition_mode": nil, "recovery_artifact_sha256": nil,
		"staged_count": int64(0), "row_version": int64(2),
		"created_at": createdAt.Add(2 * time.Minute).Format(time.RFC3339Nano),
		"updated_at": cancelledAt.Format(time.RFC3339Nano), "committed_at": nil,
		"cancelled_at": cancelledAt.Format(time.RFC3339Nano),
	}
	rows := []sealedArchiveTestRow{
		base[0],
		{table: "agent_vault_keys", value: cancelledKey},
		base[1],
		base[2], base[3],
		{table: "agent_vault_key_rotations", value: cancelledRotation},
		base[4], base[5],
	}
	for _, row := range rows {
		if err := ic.validateAndRecord(row.table, row.value); err != nil {
			t.Fatalf("validate %s: %v", row.table, err)
		}
	}
	if err := ic.validateImportedSecretGraph(); err != nil {
		t.Fatalf("validate reused-version graph: %v", err)
	}
	cancelledIdentity := secretVaultKeyIdentityImportKey{id: cancelledKeyID, version: 2}
	currentIdentity := secretVaultKeyIdentityImportKey{id: "avk_cccccccccccccccc", version: 2}
	if ic.vaultKeyIdentities[cancelledIdentity].state != "retired" ||
		ic.vaultKeyIdentities[currentIdentity].state != "current" ||
		ic.vaultRotations["vkr_gggggggggggggggg"].state != VaultKeyRotationCancelled {
		t.Fatalf("reused version identities were not preserved: %#v", ic.vaultKeyIdentities)
	}
}

func TestImportedVaultKeysRejectDuplicateLiveLogicalVersion(t *testing.T) {
	ic := testSecretImportContext(t)
	base := testVaultLifecycleArchiveRows(t)
	for _, row := range base[:2] {
		if err := ic.validateAndRecord(row.table, row.value); err != nil {
			t.Fatal(err)
		}
	}
	duplicate := map[string]any{
		"id": "avk_gggggggggggggggg", "account_id": testSecretAccountID,
		"realm_id": testSecretRealmID, "owner_agent_id": testSecretAgentID,
		"key_version": int64(2), "algorithm": SecretAEADAlgorithm,
		"fingerprint": strings.Repeat("c", 64), "lifecycle_state": "pending",
		"row_version": int64(1),
		"created_at":  base[1].value["created_at"], "retired_at": nil,
	}
	if err := ic.validateAndRecord("agent_vault_keys", duplicate); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("duplicate live logical version error = %v", err)
	}
}

func TestImportedVaultLifecycleRejectsLiveAuthority(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]sealedArchiveTestRow)
	}{
		{
			name: "orphan pending vault key",
			mutate: func(rows []sealedArchiveTestRow) {
				key := rows[1].value
				key["lifecycle_state"] = "pending"
				key["row_version"] = int64(1)
			},
		},
		{
			name: "pending enrollment",
			mutate: func(rows []sealedArchiveTestRow) {
				enrollment := rowByTable(rows, "agent_vault_key_enrollments")
				enrollment["lifecycle_state"] = VaultEnrollmentStatePending
				enrollment["source_location_id"] = nil
				enrollment["approved_at"] = nil
				enrollment["consumed_at"] = nil
			},
		},
		{
			name: "terminal enrollment with capsule",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "agent_vault_key_enrollments")["transfer_ciphertext"] = byteaOfLength(96)
			},
		},
		{
			name: "open rotation",
			mutate: func(rows []sealedArchiveTestRow) {
				rotation := rowByTable(rows, "agent_vault_key_rotations")
				rotation["lifecycle_state"] = VaultKeyRotationOpen
				rotation["committed_at"] = nil
			},
		},
		{
			name: "rotation key id version mismatch",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "agent_vault_key_rotations")["target_key_version"] = int64(3)
			},
		},
		{
			name: "committed rotation missing recovery disposition",
			mutate: func(rows []sealedArchiveTestRow) {
				delete(rowByTable(rows, "agent_vault_key_rotations"), "recovery_disposition_mode")
			},
		},
		{
			name: "committed rotation unknown recovery disposition",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "agent_vault_key_rotations")["recovery_disposition_mode"] = "backup_exists"
			},
		},
		{
			name: "artifact disposition missing digest",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "agent_vault_key_rotations")["recovery_artifact_sha256"] = nil
			},
		},
		{
			name: "artifact disposition uppercase digest",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "agent_vault_key_rotations")["recovery_artifact_sha256"] = strings.Repeat("A", 64)
			},
		},
		{
			name: "risk disposition carries digest",
			mutate: func(rows []sealedArchiveTestRow) {
				rotation := rowByTable(rows, "agent_vault_key_rotations")
				rotation["recovery_disposition_mode"] = VaultKeyRotationRiskAccepted
			},
		},
		{
			name: "rotation staging row",
			mutate: func(rows []sealedArchiveTestRow) {
				rows[5] = sealedArchiveTestRow{table: "agent_vault_key_rotation_items", value: map[string]any{
					"account_id": testSecretAccountID,
				}}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ic := testSecretImportContext(t)
			rows := testVaultLifecycleArchiveRows(t)
			tc.mutate(rows)
			var got error
			for _, row := range rows {
				got = ic.validateAndRecord(row.table, row.value)
				if got != nil {
					break
				}
			}
			if !errors.Is(got, ErrArchiveContent) {
				t.Fatalf("error = %v, want ErrArchiveContent", got)
			}
		})
	}
}

func testVaultLifecycleArchiveRows(t *testing.T) []sealedArchiveTestRow {
	t.Helper()
	exportedAt := testSecretArchiveTime(t)
	createdAt := exportedAt.Add(-time.Hour)
	approvedAt := createdAt.Add(10 * time.Minute)
	consumedAt := createdAt.Add(20 * time.Minute)
	committedAt := createdAt.Add(30 * time.Minute)
	firstKey := map[string]any{
		"id": testSecretKeyID, "account_id": testSecretAccountID,
		"realm_id": testSecretRealmID, "owner_agent_id": testSecretAgentID,
		"key_version": int64(1), "algorithm": SecretAEADAlgorithm,
		"fingerprint": strings.Repeat("a", 64), "lifecycle_state": "retired",
		"row_version": int64(2), "created_at": createdAt.Format(time.RFC3339Nano),
		"retired_at": committedAt.Format(time.RFC3339Nano),
	}
	secondKey := map[string]any{
		"id": "avk_cccccccccccccccc", "account_id": testSecretAccountID,
		"realm_id": testSecretRealmID, "owner_agent_id": testSecretAgentID,
		"key_version": int64(2), "algorithm": SecretAEADAlgorithm,
		"fingerprint": strings.Repeat("b", 64), "lifecycle_state": "current",
		"row_version": int64(2), "created_at": createdAt.Add(5 * time.Minute).Format(time.RFC3339Nano),
		"retired_at": nil,
	}
	enrollment := map[string]any{
		"id": "enr_dddddddddddddddd", "account_id": testSecretAccountID,
		"realm_id": testSecretRealmID, "owner_agent_id": testSecretAgentID,
		"vault_key_id": testSecretKeyID, "vault_key_version": int64(1),
		"target_location_id": "loc_aaaaaaaaaaaaaaaa", "target_location_name": "work laptop",
		"target_public_key":           strings.Repeat("A", 43),
		"target_key_algorithm":        VaultEnrollmentTargetKeyAlgorithm,
		"pairing_commitment":          strings.Repeat("c", 64),
		"lifecycle_state":             VaultEnrollmentStateConsumed,
		"source_location_id":          "loc_bbbbbbbbbbbbbbbb",
		"source_ephemeral_public_key": nil, "transfer_ciphertext": nil,
		"transfer_algorithm": nil, "consume_commitment": nil,
		"row_version": int64(3), "created_at": createdAt.Format(time.RFC3339Nano),
		"expires_at":   createdAt.Add(45 * time.Minute).Format(time.RFC3339Nano),
		"approved_at":  approvedAt.Format(time.RFC3339Nano),
		"consumed_at":  consumedAt.Format(time.RFC3339Nano),
		"cancelled_at": nil, "expired_at": nil,
	}
	enrollmentReceipt := map[string]any{
		"account_id": testSecretAccountID, "realm_id": testSecretRealmID,
		"owner_agent_id": testSecretAgentID, "operation": "enrollment_consume",
		"idempotency_key_hash": strings.Repeat("d", 64),
		"request_hash":         strings.Repeat("e", 64), "enrollment_id": "enr_dddddddddddddddd",
		"result_revision": int64(3), "created_at": consumedAt.Format(time.RFC3339Nano),
	}
	rotation := map[string]any{
		"id": "vkr_eeeeeeeeeeeeeeee", "account_id": testSecretAccountID,
		"realm_id": testSecretRealmID, "owner_agent_id": testSecretAgentID,
		"source_key_id": testSecretKeyID, "source_key_version": int64(1),
		"target_key_id": "avk_cccccccccccccccc", "target_key_version": int64(2),
		"lifecycle_state": VaultKeyRotationCommitted, "item_count": int64(0),
		"recovery_disposition_mode": VaultKeyRotationRecoveryArtifact,
		"recovery_artifact_sha256":  strings.Repeat("2", 64),
		"staged_count":              int64(0), "row_version": int64(2),
		"created_at":   createdAt.Add(5 * time.Minute).Format(time.RFC3339Nano),
		"updated_at":   committedAt.Format(time.RFC3339Nano),
		"committed_at": committedAt.Format(time.RFC3339Nano), "cancelled_at": nil,
	}
	rotationReceipt := map[string]any{
		"account_id": testSecretAccountID, "realm_id": testSecretRealmID,
		"owner_agent_id": testSecretAgentID, "operation": "rotation_commit",
		"idempotency_key_hash": strings.Repeat("f", 64),
		"request_hash":         strings.Repeat("1", 64), "rotation_id": "vkr_eeeeeeeeeeeeeeee",
		"result_revision": int64(2), "created_at": committedAt.Format(time.RFC3339Nano),
	}
	return []sealedArchiveTestRow{
		{table: "agent_vault_keys", value: firstKey},
		{table: "agent_vault_keys", value: secondKey},
		{table: "agent_vault_key_enrollments", value: enrollment},
		{table: "vault_key_enrollment_receipts", value: enrollmentReceipt},
		{table: "agent_vault_key_rotations", value: rotation},
		{table: "vault_key_rotation_receipts", value: rotationReceipt},
	}
}
