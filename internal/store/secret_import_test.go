package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testSecretAccountID = "acc_archive"
	testSecretRealmID   = "realm_archive"
	testSecretAgentID   = "agent_archive"
	testSecretKeyID     = "avk_aaaaaaaaaaaaaaaa"
	testSecretID        = "sec_bbbbbbbbbbbbbbbb"
	testSecretFieldID   = "fld_cccccccccccccccc"
	testSecretDEKID     = "dek_dddddddddddddddd"
)

func TestImportedSecretGraphAcceptsCiphertextOnlyVault(t *testing.T) {
	ic := testSecretImportContext(t)
	for _, row := range testSealedArchiveRows(t) {
		if err := ic.validateAndRecord(row.table, row.value); err != nil {
			t.Fatalf("validate %s: %v", row.table, err)
		}
	}
	if err := ic.validateImportedSecretGraph(); err != nil {
		t.Fatalf("validate secret graph: %v", err)
	}

	field := ic.secretFields[testSecretFieldID]
	if !field.sensitive || field.dekID != testSecretDEKID || field.dekGeneration != 1 {
		t.Fatalf("imported field scope = %#v", field)
	}
	if ic.vaultKeys[testSecretKeyID].fingerprint == "" {
		t.Fatal("public key binding was not recorded")
	}
}

func TestImportedSecretRowsRejectPlaintextAndBrokenEnvelopeGraph(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(rows []sealedArchiveTestRow)
		wantTable string
	}{
		{
			name: "empty secret name",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "secrets")["name"] = ""
			},
			wantTable: "secrets",
		},
		{
			name: "public field contains a NUL",
			mutate: func(rows []sealedArchiveTestRow) {
				field := rowByTable(rows, "secret_fields")
				field["field_kind"] = "username"
				field["sensitive"] = false
				field["public_value"] = "agent\x00name"
				field["envelope_version"] = nil
				field["ciphertext"] = nil
				field["aead_algorithm"] = nil
				field["aad_version"] = nil
				field["dek_id"] = nil
				field["dek_generation"] = nil
			},
			wantTable: "secret_fields",
		},
		{
			name: "sensitive field carries public plaintext",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "secret_fields")["public_value"] = "plaintext-canary"
			},
			wantTable: "secret_fields",
		},
		{
			name: "field crosses agent scope",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "secret_fields")["owner_agent_id"] = "agent_other"
			},
			wantTable: "secret_fields",
		},
		{
			name: "wrapped DEK has wrong size",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "secret_deks")["wrapped_dek"] = byteaOfLength(59)
			},
			wantTable: "secret_deks",
		},
		{
			name: "DEK names unknown vault key",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "secret_deks")["wrapping_key_id"] = "avk_eeeeeeeeeeeeeeee"
			},
			wantTable: "secret_deks",
		},
		{
			name: "receipt revision is ahead of target",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "secret_mutation_receipts")["result_revision"] = int64(2)
			},
			wantTable: "secret_mutation_receipts",
		},
		{
			name: "create receipt carries a value version",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "secret_mutation_receipts")["result_value_version"] = int64(1)
			},
			wantTable: "secret_mutation_receipts",
		},
		{
			name: "receipt operation and target disagree",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "secret_mutation_receipts")["operation"] = "key_register"
			},
			wantTable: "secret_mutation_receipts",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ic := testSecretImportContext(t)
			rows := testSealedArchiveRows(t)
			tc.mutate(rows)
			var got error
			for _, row := range rows {
				got = ic.validateAndRecord(row.table, row.value)
				if got != nil {
					if row.table != tc.wantTable {
						t.Fatalf("failed at %s, want %s: %v", row.table, tc.wantTable, got)
					}
					break
				}
			}
			if got == nil || !errors.Is(got, ErrArchiveContent) {
				t.Fatalf("error = %v, want ErrArchiveContent", got)
			}
			if strings.Contains(got.Error(), "plaintext-canary") {
				t.Fatalf("value-bearing import error = %q", got)
			}
		})
	}
}

func TestImportedSecretGraphRequiresFieldsAndCurrentInitializedVault(t *testing.T) {
	t.Run("secret without fields", func(t *testing.T) {
		ic := testSecretImportContext(t)
		rows := testSealedArchiveRows(t)
		for _, row := range rows[:2] {
			if err := ic.validateAndRecord(row.table, row.value); err != nil {
				t.Fatal(err)
			}
		}
		if err := ic.validateImportedSecretGraph(); err == nil {
			t.Fatal("fieldless secret graph accepted")
		}
	})

	t.Run("initialized live vault without current epoch", func(t *testing.T) {
		ic := testSecretImportContext(t)
		key := testSealedArchiveRows(t)[0].value
		key["lifecycle_state"] = "retired"
		key["retired_at"] = testSecretArchiveTime(t).Add(-time.Minute).Format(time.RFC3339Nano)
		if err := ic.validateAndRecord("agent_vault_keys", key); err != nil {
			t.Fatal(err)
		}
		if err := ic.validateImportedSecretGraph(); err == nil {
			t.Fatal("initialized live vault without a current key accepted")
		}
	})
}

func TestImportedSecretDeleteRequiresExactDeletedValueFreeTarget(t *testing.T) {
	deletedRows := func(t *testing.T) []sealedArchiveTestRow {
		t.Helper()
		rows := testSealedArchiveRows(t)
		secret := rowByTable(rows, "secrets")
		secret["name"] = testSecretID
		secret["description"] = ""
		secret["template"] = "generic"
		secret["tags"] = []any{}
		secret["row_version"] = int64(2)
		secret["archived_at"] = nil
		secret["deleted_at"] = testSecretArchiveTime(t).
			Add(-time.Minute).Format(time.RFC3339Nano)
		receipt := rowByTable(rows, "secret_mutation_receipts")
		receipt["operation"] = "secret_delete"
		receipt["result_revision"] = int64(2)
		// Deleted tombstones intentionally have no value-bearing children.
		return []sealedArchiveTestRow{rows[0], rows[1], rows[4]}
	}

	t.Run("accept exact value-free tombstone", func(t *testing.T) {
		ic := testSecretImportContext(t)
		for _, row := range deletedRows(t) {
			if err := ic.validateAndRecord(row.table, row.value); err != nil {
				t.Fatalf("validate %s: %v", row.table, err)
			}
		}
		if err := ic.validateImportedSecretGraph(); err != nil {
			t.Fatalf("validate deleted graph: %v", err)
		}
	})

	for _, test := range []struct {
		name   string
		mutate func([]sealedArchiveTestRow)
	}{
		{
			name: "live target",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "secrets")["deleted_at"] = nil
			},
		},
		{
			name: "stale delete revision",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "secret_mutation_receipts")["result_revision"] = int64(1)
			},
		},
		{
			name: "retained deleted metadata",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "secrets")["description"] = "must not survive"
			},
		},
		{
			name: "delete receipt value version",
			mutate: func(rows []sealedArchiveTestRow) {
				rowByTable(rows, "secret_mutation_receipts")["result_value_version"] = int64(1)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ic := testSecretImportContext(t)
			rows := deletedRows(t)
			test.mutate(rows)
			var got error
			for _, row := range rows {
				got = ic.validateAndRecord(row.table, row.value)
				if got != nil {
					break
				}
			}
			if got == nil || !errors.Is(got, ErrArchiveContent) {
				t.Fatalf("delete receipt error = %v, want ErrArchiveContent", got)
			}
		})
	}

	t.Run("deleted target retains a field", func(t *testing.T) {
		ic := testSecretImportContext(t)
		rows := testSealedArchiveRows(t)
		secret := rowByTable(rows, "secrets")
		secret["name"] = testSecretID
		secret["description"] = ""
		secret["template"] = "generic"
		secret["tags"] = []any{}
		secret["row_version"] = int64(2)
		secret["deleted_at"] = testSecretArchiveTime(t).
			Add(-time.Minute).Format(time.RFC3339Nano)
		for _, row := range rows[:4] {
			if err := ic.validateAndRecord(row.table, row.value); err != nil {
				t.Fatalf("validate %s: %v", row.table, err)
			}
		}
		if err := ic.validateImportedSecretGraph(); err == nil ||
			!strings.Contains(err.Error(), "retains 1 fields") {
			t.Fatalf("deleted graph error = %v", err)
		}
	})
}

func TestImportedSecretGraphRequiresCurrentDEKAndCurrentVaultKey(t *testing.T) {
	t.Run("missing DEK", func(t *testing.T) {
		ic := testSecretImportContext(t)
		rows := testSealedArchiveRows(t)
		for _, row := range rows[:3] {
			if err := ic.validateAndRecord(row.table, row.value); err != nil {
				t.Fatal(err)
			}
		}
		if err := ic.validateImportedSecretGraph(); err == nil {
			t.Fatal("missing DEK graph accepted")
		}
	})

	t.Run("retired wrapping key", func(t *testing.T) {
		ic := testSecretImportContext(t)
		rows := testSealedArchiveRows(t)
		key := rowByTable(rows, "agent_vault_keys")
		key["lifecycle_state"] = "retired"
		key["retired_at"] = testSecretArchiveTime(t).Add(-time.Minute).Format(time.RFC3339Nano)
		for _, row := range rows[:4] {
			if err := ic.validateAndRecord(row.table, row.value); err != nil {
				t.Fatal(err)
			}
		}
		if err := ic.validateImportedSecretGraph(); err == nil {
			t.Fatal("current DEK wrapped by retired vault key accepted")
		}
	})
}

type sealedArchiveTestRow struct {
	table string
	value map[string]any
}

func testSecretImportContext(t *testing.T) *importCtx {
	t.Helper()
	ic := newImportCtx(testSecretAccountID)
	ic.exportedAt = testSecretArchiveTime(t)
	ic.realms[testSecretRealmID] = true
	ic.agents[testSecretAgentID] = true
	ic.liveAgents[testSecretAgentID] = true
	ic.agentRealms[testSecretAgentID] = testSecretRealmID
	return ic
}

func testSecretArchiveTime(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2026, 7, 18, 20, 0, 0, 0, time.UTC)
}

func testSealedArchiveRows(t *testing.T) []sealedArchiveTestRow {
	t.Helper()
	exportedAt := testSecretArchiveTime(t)
	createdAt := exportedAt.Add(-time.Hour).Format(time.RFC3339Nano)
	updatedAt := exportedAt.Add(-30 * time.Minute).Format(time.RFC3339Nano)
	return []sealedArchiveTestRow{
		{table: "agent_vault_keys", value: map[string]any{
			"id": testSecretKeyID, "account_id": testSecretAccountID,
			"realm_id": testSecretRealmID, "owner_agent_id": testSecretAgentID,
			"key_version": int64(1), "algorithm": SecretAEADAlgorithm,
			"fingerprint": strings.Repeat("a", 64), "lifecycle_state": "current",
			"row_version": int64(1), "created_at": createdAt, "retired_at": nil,
		}},
		{table: "secrets", value: map[string]any{
			"id": testSecretID, "account_id": testSecretAccountID,
			"realm_id": testSecretRealmID, "owner_agent_id": testSecretAgentID,
			"name": "GitHub account", "description": "Agent login",
			"template": "login", "tags": []any{"developer", "github"},
			"row_version": int64(1), "created_at": createdAt, "updated_at": updatedAt,
			"archived_at": nil, "deleted_at": nil,
		}},
		{table: "secret_fields", value: map[string]any{
			"id": testSecretFieldID, "account_id": testSecretAccountID,
			"realm_id": testSecretRealmID, "owner_agent_id": testSecretAgentID,
			"secret_id": testSecretID, "name": "password", "field_kind": "password",
			"sensitive": true, "value_encoding": "utf8", "value_version": int64(1),
			"public_value": nil, "envelope_version": int64(1),
			"ciphertext":     byteaOfLength(29),
			"aead_algorithm": SecretAEADAlgorithm, "aad_version": int64(1),
			"dek_id": testSecretDEKID, "dek_generation": int64(1),
			"row_version": int64(1), "created_at": createdAt, "updated_at": updatedAt,
		}},
		{table: "secret_deks", value: map[string]any{
			"id": testSecretDEKID, "account_id": testSecretAccountID,
			"realm_id": testSecretRealmID, "owner_agent_id": testSecretAgentID,
			"secret_id": testSecretID, "field_id": testSecretFieldID,
			"dek_generation": int64(1), "wrapped_dek": byteaOfLength(60),
			"wrap_algorithm": SecretAEADAlgorithm, "aad_version": int64(1),
			"wrap_revision": int64(1), "wrapping_key_id": testSecretKeyID,
			"wrapping_key_version": int64(1), "row_version": int64(1),
			"created_at": createdAt, "retired_at": nil,
		}},
		{table: "secret_mutation_receipts", value: map[string]any{
			"account_id": testSecretAccountID, "realm_id": testSecretRealmID,
			"owner_agent_id": testSecretAgentID, "actor_kind": "agent",
			"actor_id": testSecretAgentID, "operation": "secret_create",
			"idempotency_key_hash": strings.Repeat("b", 64),
			"request_hash":         strings.Repeat("c", 64), "target_kind": "secret",
			"target_id": testSecretID, "result_revision": int64(1),
			"result_value_version": nil, "created_at": updatedAt,
		}},
	}
}

func rowByTable(rows []sealedArchiveTestRow, table string) map[string]any {
	for _, row := range rows {
		if row.table == table {
			return row.value
		}
	}
	return nil
}

func byteaOfLength(length int) string {
	return `\x` + strings.Repeat("00", length)
}
