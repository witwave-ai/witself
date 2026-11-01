package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestSyncCaptureRelationGuards(t *testing.T) {
	ctx := context.Background()
	st, p := newSyncCaptureFixture(t, false)
	for _, query := range []string{
		`INSERT INTO agent_dashboard_preferences(account_id,realm_id,agent_id,prefs,updated_at) VALUES('capture_account','capture_realm','capture_agent','{}',now()),('capture_account','capture_realm','capture_second','{}',now())`,
		`INSERT INTO accounts(id,display_name,is_default,status) VALUES('capture_inactive','Inactive fixture',false,'active')`,
	} {
		if _, err := st.pool.Exec(ctx, query); err != nil {
			t.Fatalf("capture guard fixture SQLSTATE: %s", syncTestSQLState(err))
		}
	}
	activateSyncFixture(t, st, p.AccountID)
	for _, tc := range []struct{ name, sql string }{
		{"insert", `INSERT INTO realms(id,account_id,name) VALUES('capture_refused','capture_account','refused')`},
		{"update", `UPDATE agent_dashboard_preferences SET prefs='{}' WHERE account_id='capture_account'`},
		{"delete", `DELETE FROM agent_dashboard_preferences WHERE account_id='capture_account'`},
		{"multi_account", `UPDATE accounts SET display_name='refused' WHERE id IN ('capture_account','capture_inactive')`},
		{"indirect_agent", `UPDATE agents SET name='refused' WHERE id='capture_agent'`},
		{"cascade", `DELETE FROM realms WHERE id='capture_realm'`},
		{"truncate", `TRUNCATE agent_dashboard_preferences`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.pool.Exec(ctx, tc.sql)
			captureRefusedSQL(t, err) // LC-S6C1 fix 2 (F6)
		})
	}
	t.Run("copy", func(t *testing.T) {
		_, err := st.pool.CopyFrom(ctx, pgx.Identifier{"realms"}, []string{"id", "account_id", "name"}, pgx.CopyFromRows([][]any{{"capture_copy", "capture_account", "copy"}}))
		captureRefusedSQL(t, err) // LC-S6C1 fix 2 (F6)
	})
	t.Run("statement_savepoint", func(t *testing.T) {
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal("capture guard transaction begin failed")
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err = tx.Exec(ctx, "SAVEPOINT rejected_statement"); err != nil {
			t.Fatal("capture guard savepoint begin failed")
		}
		_, err = tx.Exec(ctx, `UPDATE accounts SET display_name='refused' WHERE id='capture_account'`)
		captureRefusedSQL(t, err) // LC-S6C1 fix 2 (F6)
		if _, err = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT rejected_statement"); err != nil {
			t.Fatal("capture guard savepoint rollback failed")
		}
		if _, err = tx.Exec(ctx, `UPDATE accounts SET display_name='accepted' WHERE id='capture_inactive'`); err != nil {
			t.Fatalf("capture guard inactive write SQLSTATE: %s", syncTestSQLState(err))
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatalf("capture guard inactive commit SQLSTATE: %s", syncTestSQLState(err))
		}
	})
	t.Run("api_savepoint", func(t *testing.T) { // LC-S6C1 fix 2 (F6)
		tx, err := st.AdmitSyncTx(ctx, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability) // LC-S6C1 fix 2 (F6)
		if err != nil {                                                                                  // LC-S6C1 fix 2 (F6)
			t.Fatal("F6 API begin admission failed") // LC-S6C1 fix 2 (F6)
		} // LC-S6C1 fix 2 (F6)
		defer func() { _ = tx.Rollback(ctx) }()             // LC-S6C1 fix 2 (F6)
		err = tx.Savepoint(ctx, func(child *SyncTx) error { // LC-S6C1 fix 2 (F6)
			if _, err := child.Tx().Exec(ctx, `SELECT set_config('witself.sync_tx','',true)`); err != nil { // LC-S6C1 fix 2 (F6)
				t.Fatal("F6 API setup guard precondition failed") // LC-S6C1 fix 2 (F6)
			} // LC-S6C1 fix 2 (F6)
			_, err := child.Tx().Exec(ctx, `UPDATE accounts SET display_name='refused' WHERE id='capture_account'`) // LC-S6C1 fix 2 (F6)
			return err                                                                                              // LC-S6C1 fix 2 (F6)
		}) // LC-S6C1 fix 2 (F6)
		captureRefusedAPI(t, err) // LC-S6C1 fix 2 (F6)
	}) // LC-S6C1 fix 2 (F6)
	var count int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM agent_dashboard_preferences WHERE account_id='capture_account'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("capture guard rollback count mismatch: %d", count)
	}
}

func TestSyncCaptureInactiveAccountlessReceipts(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncCaptureFixture(t, true)
	// These receipt tables deliberately have no accounts foreign key. R1's
	// absent-control behavior preserves their valid INSERT/UPDATE semantics,
	// even while an unrelated account has active capture.
	const account = "capture_receipt_account"
	for _, query := range []string{
		`INSERT INTO account_evacuation_finalizations(account_id,evacuation_id,source_status,finalized_at)
		 VALUES($1,'capture_receipt_evacuation','suspended',now())`,
		`UPDATE account_evacuation_finalizations SET source_status='closed' WHERE account_id=$1`,
		`INSERT INTO account_provision_receipts(provision_id,account_id,request_fingerprint,bootstrap_token_id)
		 VALUES('capture_receipt_provision',$1,repeat('a',64),'capture_receipt_bootstrap')`,
		`UPDATE account_provision_receipts SET issue_count=issue_count+1 WHERE account_id=$1`,
	} {
		if _, err := st.pool.Exec(ctx, query, account); err != nil {
			t.Fatalf("capture accountless receipt SQLSTATE: %s", syncTestSQLState(err))
		}
	}
	var accounts, controls, finalizations, provisions, bundles int
	if err := st.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM accounts WHERE id=$1),
		(SELECT count(*) FROM sync_capture WHERE account_id=$1),
		(SELECT count(*) FROM account_evacuation_finalizations WHERE account_id=$1 AND source_status='closed'),
		(SELECT count(*) FROM account_provision_receipts WHERE account_id=$1 AND issue_count=2),
		(SELECT count(*) FROM sync_bundles WHERE account_id=$1)`, account).Scan(&accounts, &controls, &finalizations, &provisions, &bundles); err != nil || accounts != 0 || controls != 0 || finalizations != 1 || provisions != 1 || bundles != 0 {
		t.Fatal("capture accountless receipt domain behavior changed")
	}
}

func TestSyncCaptureTruncateStaleSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name            string
		isolation       pgx.TxIsoLevel
		existingControl bool
	}{
		{"repeatable_read_missing", pgx.RepeatableRead, false},
		{"serializable_missing", pgx.Serializable, false},
		{"repeatable_read_inert", pgx.RepeatableRead, true},
		{"serializable_inert", pgx.Serializable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, principal := newSyncCaptureFixture(t, false)
			if _, err := st.PutDashboardPreferences(ctx, principal, syncPreferenceDocument("amber")); err != nil {
				t.Fatal("capture truncate inactive fixture write failed")
			}
			if tc.existingControl {
				if _, err := st.pool.Exec(ctx, `INSERT INTO sync_capture(account_id,capture_epoch,lifecycle_epoch,restore_marker,capability,coverage_state)
					VALUES($1,1,1,decode(repeat('01',16),'hex'),$2,'bootstrapping')`, principal.AccountID, SyncPreferencesCapability); err != nil {
					t.Fatal("capture truncate inert control fixture failed")
				}
				if _, err := st.pool.Exec(ctx, `UPDATE sync_writer_capabilities SET enabled=false WHERE account_id=$1`, principal.AccountID); err != nil {
					t.Fatal("capture truncate inert capability fixture failed")
				}
			}
			tx, err := st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: tc.isolation})
			if err != nil {
				t.Fatal("capture truncate snapshot begin failed")
			}
			defer func() { _ = tx.Rollback(ctx) }()
			var controlExists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_capture WHERE account_id=$1)`, principal.AccountID).Scan(&controlExists); err != nil || controlExists != tc.existingControl {
				t.Fatal("capture truncate preactivation snapshot failed")
			}
			if tc.existingControl {
				if err := st.EnableSyncWriter(ctx, principal.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability); err != nil {
					t.Fatal("capture truncate enable capability failed")
				}
			}
			activateSyncFixture(t, st, principal.AccountID)
			_, err = tx.Exec(ctx, `TRUNCATE agent_dashboard_preferences`)
			captureRefusedSnapshot(t, err) // LC-S6C1 fix 2 (F6)
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal("capture truncate refused transaction rollback failed")
			}
			var preferences, bundles int
			if err := st.pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM agent_dashboard_preferences WHERE account_id=$1),
				(SELECT count(*) FROM sync_bundles WHERE account_id=$1)`, principal.AccountID).Scan(&preferences, &bundles); err != nil || preferences != 1 || bundles != 0 {
				t.Fatal("capture truncate stale snapshot changed durable state")
			}
		})
	}
}

// These mutations preserve row counts wherever possible. The deferred trigger
// must bind exact actual-event identity, reviewed operation, bundle membership,
// sealed semantic bytes, and the committed counter rather than count rows.
func TestSyncCaptureDeferredSemanticIdentity(t *testing.T) {
	ctx := context.Background()
	st, principal := newSyncCaptureFixture(t, true)
	for _, tc := range []struct{ name, mutation, immutableTable string }{
		{"wrong_table", `UPDATE sync_tx_touches SET table_name='realms' WHERE account_id=$1`, ""},
		{"unknown_operation", "unreviewed", ""},
		{"missing_bundle_member", `DELETE FROM sync_bundle_operations WHERE account_id=$1`, "sync_bundle_operations"},
		{"divergent_semantics", `UPDATE sync_bundles SET semantic_effects='[]'::jsonb WHERE account_id=$1`, "sync_bundles"},
		{"divergent_counter", `UPDATE sync_finalized_counter SET last_finalized=last_finalized+1 WHERE account_id=$1`, ""},
		{"unsupported_touch", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := st.AdmitSyncTx(ctx, principal.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability)
			if err != nil {
				t.Fatal("capture semantic fixture admission failed")
			}
			defer func() { _ = tx.Rollback(ctx) }()
			effect := syncPreferenceDocument("amber")
			if _, err := putSyncDashboardPreferencesSQL(ctx, tx.Tx(), principal, effect); err != nil {
				t.Fatalf("capture semantic fixture DML SQLSTATE: %s", syncTestSQLState(err))
			}
			operation := SyncPreferencesCapability
			if tc.mutation == "" {
				operation = "unreviewed.put"
			}
			err = tx.Touch(ctx, operation, "preferences", effect, json.RawMessage(`[]`))
			if tc.mutation == "" {
				captureRefusedAPI(t, err) // LC-S6C1 fix 2 (F6)
			} else {
				if err != nil {
					t.Fatal("capture semantic fixture touch failed")
				}
				// Construct an unreviewed operation before sealing, without updating
				// immutable evidence or disabling its deferred foreign keys.
				if tc.name == "unknown_operation" {
					if _, err := tx.Tx().Exec(ctx, `INSERT INTO sync_operations SELECT (jsonb_populate_record(NULL::sync_operations,to_jsonb(o)||jsonb_build_object('operation_id',decode(repeat('ef',16),'hex'),'operation_kind','unreviewed.put'))).* FROM sync_operations o WHERE account_id=$1`, principal.AccountID); err != nil {
						t.Fatal("capture semantic unreviewed operation setup failed")
					}
					if _, err := tx.Tx().Exec(ctx, `UPDATE sync_tx_touches SET operation_id=decode(repeat('ef',16),'hex') WHERE account_id=$1`, principal.AccountID); err != nil {
						t.Fatal("capture semantic unreviewed touch setup failed")
					}
				}
				if err := tx.Seal(ctx); err != nil {
					t.Fatal("capture semantic fixture seal failed")
				}
				// Isolate the deferred check from the existing immediate evidence
				// immutability guard. This exact trigger is disabled only for a
				// targeted fixture mutation and restored before commit; transactional
				// DDL also rolls back on failure. This is not principal-bypass proof.
				if tc.immutableTable != "" {
					if _, err := tx.Tx().Exec(ctx, "ALTER TABLE "+pgx.Identifier{tc.immutableTable}.Sanitize()+" DISABLE TRIGGER sync_evidence_immutable"); err != nil {
						t.Fatalf("capture semantic fixture disable SQLSTATE: %s", syncTestSQLState(err))
					}
				}
				if tc.name != "unknown_operation" {
					if _, err := tx.Tx().Exec(ctx, tc.mutation, principal.AccountID); err != nil {
						t.Fatalf("capture semantic tamper SQLSTATE: %s", syncTestSQLState(err))
					}
				}
				if tc.immutableTable != "" {
					if _, err := tx.Tx().Exec(ctx, "ALTER TABLE "+pgx.Identifier{tc.immutableTable}.Sanitize()+" ENABLE TRIGGER sync_evidence_immutable"); err != nil {
						t.Fatalf("capture semantic fixture enable SQLSTATE: %s", syncTestSQLState(err))
					}
				}
			}
			// Inspect the deferred trigger before SyncTx.Commit normalizes its error. // LC-S6C1 fix 2 (F6)
			captureRefusedCoverage(t, tx.Tx().Commit(ctx)) // LC-S6C1 fix 2 (F6)
			if tc.immutableTable != "" {
				var enabled bool
				if err := st.pool.QueryRow(ctx, `SELECT tgenabled='O' FROM pg_trigger WHERE tgrelid=$1::regclass AND tgname='sync_evidence_immutable'`, tc.immutableTable).Scan(&enabled); err != nil || !enabled {
					t.Fatal("capture semantic fixture did not restore immutable guard")
				}
			}
			for _, table := range []string{"agent_dashboard_preferences", "sync_tx_admissions", "sync_tx_touches", "sync_operations", "sync_operation_payloads", "sync_bundles", "sync_bundle_operations"} {
				var count int
				if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE account_id=$1`, principal.AccountID).Scan(&count); err != nil || count != 0 {
					t.Fatalf("capture semantic rollback left reviewed rows: %s", table)
				}
			}
			var finalized int64
			if err := st.pool.QueryRow(ctx, `SELECT last_finalized FROM sync_finalized_counter WHERE account_id=$1`, principal.AccountID).Scan(&finalized); err != nil || finalized != 0 {
				t.Fatal("capture semantic rollback changed finalized prefix")
			}
		})
	}
}
