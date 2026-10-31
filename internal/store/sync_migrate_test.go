package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"github.com/witwave-ai/witself/internal/testenv"
)

func newSyncSchemaTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal("core migration failed")
	}
	return st, dsn
}

func TestSyncMigrationNamespacePostgres(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncSchemaTestStore(t)
	core := SchemaVersion()
	if version, err := st.AppliedSyncSchemaVersion(ctx); err != nil || version != 0 {
		t.Fatal("core migration installed sync namespace")
	}
	// Poison global Goose filesystem state: an isolated sync provider must not
	// read it or repair it. Core migrator has already finished for this fixture.
	poison := fstest.MapFS{"9000_forbidden.sql": {Data: []byte("-- +goose Up\nSELECT 1/0;")}}
	goose.SetBaseFS(poison)
	t.Cleanup(func() { goose.SetBaseFS(migrationsFS) })
	for i := 0; i < 2; i++ {
		if err := st.MigrateSync(ctx); err != nil {
			t.Fatalf("sync migration pass %d: %v", i, err)
		}
		if version, err := st.AppliedSyncSchemaVersion(ctx); err != nil || version != SyncSchemaVersion() {
			t.Fatal("sync version differs from independent compiled coordinate")
		}
		if version, err := st.AppliedSchemaVersion(ctx); err != nil || version != core {
			t.Fatal("sync migration changed core schema coordinate")
		}
	}
	// A public Goose call still sees the poisoned global state. Sync did not
	// mutate the global filesystem while using its private provider.
	files, err := goose.CollectMigrations(".", 0, 9999)
	if err != nil || len(files) != 1 || files[0].Version != 9000 {
		t.Fatal("sync migration changed process-global Goose filesystem")
	}
	var count, budgets int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema=current_schema() AND table_name LIKE 'sync\_%' ESCAPE '\'`).Scan(&count); err != nil || count != 21 {
		t.Fatal("sync namespace table count differs from reviewed inventory")
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM sync_resource_budgets`).Scan(&budgets); err != nil || budgets != 0 {
		t.Fatal("sync migration seeded product budgets")
	}
}

func TestSyncMigrationAheadAndDowngradeRefusePostgres(t *testing.T) {
	ctx := context.Background()
	st, dsn := newSyncSchemaTestStore(t)
	if err := st.MigrateSync(ctx); err != nil {
		t.Fatal(err)
	}
	seedSyncSchemaRows(t, st, "acc_sync_schema")
	if _, err := st.pool.Exec(ctx, `INSERT INTO witself_sync_schema_version(version_id,is_applied) VALUES($1,true)`, SyncSchemaVersion()+1); err != nil {
		t.Fatal("prepare future sync coordinate failed")
	}
	if err := st.MigrateSync(ctx); !errors.Is(err, ErrSyncSchemaAhead) || err.Error() != "sync schema is newer than this binary" {
		t.Fatalf("future sync coordinate error: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM witself_sync_schema_version WHERE version_id=$1`, SyncSchemaVersion()+1); err != nil {
		t.Fatal("remove synthetic future coordinate failed")
	}
	db := migrationTestSQLDB(t, dsn)
	t.Cleanup(func() { _ = db.Close() })
	fsys, err := fs.Sub(syncMigrationsFS, "sync_migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys,
		goose.WithTableName(syncSchemaVersionTable), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal("configure isolated downgrade provider failed")
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("sync namespace downgrade succeeded")
	}
	if version, err := st.AppliedSyncSchemaVersion(ctx); err != nil || version != SyncSchemaVersion() {
		t.Fatal("refused downgrade changed sync version")
	}
	for _, name := range syncSchemaFixtureTables {
		var count int
		if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{name}.Sanitize()).Scan(&count); err != nil || count != 1 {
			t.Errorf("refused ahead/downgrade changed %s", name)
		}
	}
	assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
}

func TestSyncMigrationLockCancellationPostgres(t *testing.T) {
	st, dsn := newSyncSchemaTestStore(t)
	db := migrationTestSQLDB(t, dsn)
	t.Cleanup(func() { _ = db.Close() })
	gate, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal("open lock gate failed")
	}
	t.Cleanup(func() { _ = gate.Close() })
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			if err := acquireMigrationAdvisoryLock(context.Background(), gate); err != nil {
				t.Fatal("hold existing core migration lock failed")
			}
			held := true
			t.Cleanup(func() {
				if held {
					_ = releaseMigrationAdvisoryLock(context.Background(), gate)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- st.MigrateSync(ctx) }()
			if mode == "cancel" {
				timer := time.NewTimer(150 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-result:
					t.Fatal("sync migration ignored existing core lock")
				case <-timer.C:
					cancel()
				}
			}
			assertMigrationCanceled(t, awaitMigrationResult(t, result, 5*time.Second), "advisory lock")
			if version, err := st.AppliedSyncSchemaVersion(context.Background()); err != nil || version != 0 {
				t.Fatal("canceled lock wait changed sync state")
			}
			if err := releaseMigrationAdvisoryLock(context.Background(), gate); err != nil {
				t.Fatal("release migration gate failed")
			}
			held = false
			assertMigrationLockAvailable(t, gate)
		})
	}
	if err := st.MigrateSync(context.Background()); err != nil {
		t.Fatal("sync migration did not resume after canceled waits")
	}
}

func TestSyncMigrationFinishesFileBeforeCancellationPostgres(t *testing.T) {
	st, _ := newSyncSchemaTestStore(t)
	ctx := context.Background()
	gate, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin migration DDL gate failed")
	}
	defer func() { _ = gate.Rollback(ctx) }()
	if _, err := gate.Exec(ctx, `LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal("hold migration DDL gate failed")
	}
	migrateCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- st.MigrateSync(migrateCtx) }()
	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := st.pool.QueryRow(waitCtx, `SELECT EXISTS (SELECT 1 FROM pg_locks
			WHERE relation='accounts'::regclass AND NOT granted)`).Scan(&blocked); err != nil {
			t.Fatal("observe sync DDL lock failed")
		}
		if blocked {
			break
		}
		select {
		case <-result:
			t.Fatal("sync migration did not wait for relation DDL lock")
		case <-waitCtx.Done():
			t.Fatal("sync migration never reached relation DDL lock")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case <-result:
		t.Fatal("cancellation interrupted a sync migration file")
	case <-time.After(100 * time.Millisecond):
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal("release sync DDL gate failed")
	}
	assertMigrationCanceled(t, awaitMigrationResult(t, result, 10*time.Second), "verify")
	if version, err := st.AppliedSyncSchemaVersion(ctx); err != nil || version != SyncSchemaVersion() {
		t.Fatal("cancellation did not preserve complete applied migration file")
	}
	if err := st.MigrateSync(ctx); err != nil {
		t.Fatal("sync migration did not resume after file completion")
	}
}

func TestSyncMigrationInvalidNamespacePostgres(t *testing.T) {
	st, _ := newSyncSchemaTestStore(t)
	ctx := context.Background()
	if _, err := st.pool.Exec(ctx, `CREATE TABLE sync_unversioned(account_id text)`); err != nil {
		t.Fatal("prepare unversioned namespace failed")
	}
	if _, err := st.AppliedSyncSchemaVersion(ctx); !errors.Is(err, ErrSyncSchemaStateInvalid) {
		t.Fatal("unversioned namespace appeared core-only")
	}
	if err := st.MigrateSync(ctx); !errors.Is(err, ErrSyncSchemaStateInvalid) {
		t.Fatal("migration accepted unversioned sync tables")
	}
	if _, err := st.pool.Exec(ctx, `DROP TABLE sync_unversioned`); err != nil {
		t.Fatal("remove synthetic unversioned table failed")
	}
	if err := st.MigrateSync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `DROP TABLE sync_resource_reservations`); err != nil {
		t.Fatal("remove required sync relation failed")
	}
	if _, err := st.AppliedSyncSchemaVersion(ctx); !errors.Is(err, ErrSyncSchemaStateInvalid) {
		t.Fatal("partial installed namespace remained valid")
	}
	if err := st.MigrateSync(ctx); !errors.Is(err, ErrSyncSchemaStateInvalid) {
		t.Fatal("migration silently repaired a partial installed namespace")
	}
	if _, err := st.pool.Exec(ctx, `DROP TABLE witself_sync_schema_version`); err != nil {
		t.Fatal("remove synthetic sync version table failed")
	}
	if _, err := st.AppliedSyncSchemaVersion(ctx); !errors.Is(err, ErrSyncSchemaStateInvalid) {
		t.Fatal("dropped version table erased installed sync requirement")
	}
}

var syncSchemaFixtureTables = []string{
	"sync_incarnations", "sync_operations", "sync_operation_payloads", "sync_verifier_history",
	"sync_receipts", "sync_reuse_refusals", "sync_capture", "sync_writer_capabilities",
	"sync_tx_admissions", "sync_tx_touches", "sync_finalized_counter", "sync_bundles",
	"sync_bundle_operations", "sync_publication_outbox", "sync_acceptance_counter",
	"sync_resource_budgets", "sync_resource_reservations", "sync_deletion_knowledge",
	"sync_retained_evidence", "sync_evidence_pins", "sync_payload_retirements",
}

// seedSyncSchemaRows supplies one independently valid row per production table.
// Only test values live here; no fixture principals or capacity defaults are
// included in production migrations. A single transaction exercises the
// deferred terminal-operation/receipt relationship used by archive import.
func seedSyncSchemaRows(t *testing.T, st *Store, accountID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin sync fixture failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO accounts(id,display_name,is_default) VALUES($1,'Sync schema fixture',false) ON CONFLICT(id) DO NOTHING`, accountID); err != nil {
		t.Fatal("insert fixture account failed")
	}
	values := []string{
		`$1,1,decode(repeat('01',16),'hex'),NULL,NULL,decode(repeat('02',16),'hex'),decode(repeat('03',16),'hex'),'installation','installed',NULL`,
		`$1,decode(repeat('04',16),'hex'),1,decode(repeat('01',16),'hex'),1,'installation','replica','lineage',1,'fact.set',1,1,1,'comparison',decode(repeat('05',32),'hex'),'terminal',decode(repeat('04',16),'hex')`,
		`$1,decode(repeat('04',16),'hex'),decode('01','hex'),NULL`,
		`$1,'issuer','stream','signer',decode(repeat('06',32),'hex'),'2026-01-01'::timestamptz,NULL,NULL`,
		`$1,decode(repeat('04',16),'hex'),1,decode(repeat('01',16),'hex'),1,'installation','replica','lineage',1,'issuer',1,decode(repeat('01',16),'hex'),'stream','authority',1,'comparison',decode(repeat('05',32),'hex'),NULL,'accepted','applied',decode('01','hex'),'signer',decode('01','hex'),decode(repeat('07',64),'hex'),'2026-01-01'::timestamptz,NULL,'2026-03-01'::timestamptz`,
		`$1,decode(repeat('04',16),'hex'),'comparison',decode(repeat('08',32),'hex'),decode('01','hex'),decode(repeat('09',64),'hex'),'2026-03-01'::timestamptz`,
		`$1,1,1,decode(repeat('01',16),'hex'),'capability','suspended'`,
		`$1,'test_principal','capability','manifest',0,false`,
		`$1,1,'capability',1,decode(repeat('01',16),'hex'),1,true,decode(repeat('04',16),'hex'),'group'`,
		`$1,1,1,'{}'::jsonb,'[]'::jsonb,decode(repeat('04',16),'hex'),'group'`,
		`$1,1,1`,
		`$1,decode(repeat('0a',16),'hex'),1,1,1,decode(repeat('01',16),'hex'),1,true,'original','[]'::jsonb,'[]'::jsonb`,
		`$1,decode(repeat('0a',16),'hex'),0,decode(repeat('04',16),'hex'),'group'`,
		`$1,'stream',1,decode(repeat('0b',32),'hex'),decode('01','hex'),decode(repeat('04',16),'hex'),'receipt','pending',NULL`,
		`$1,'stream',1`,
		`$1,500,10000000,1,1`,
		`$1,decode(repeat('04',16),'hex'),1,0,true`,
		`$1,'fact','object',decode(repeat('0c',16),'hex'),decode(repeat('0d',16),'hex'),decode(repeat('04',16),'hex'),'stream',1,'evidence','2026-01-01'::timestamptz,'2026-03-01'::timestamptz`,
		`$1,'identity','evidence','2026-03-01'::timestamptz`,
		`$1,'identity','evidence','owner','unresolved',NULL`,
		`$1,decode(repeat('04',16),'hex'),'2026-01-02'::timestamptz`,
	}
	for i, name := range syncSchemaFixtureTables {
		if _, err := tx.Exec(ctx, "INSERT INTO "+pgx.Identifier{name}.Sanitize()+" VALUES("+values[i]+")", accountID); err != nil {
			t.Fatalf("seed %s failed with SQLSTATE %s", name, syncTestSQLState(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit sync fixture failed with SQLSTATE %s", syncTestSQLState(err))
	}
}

func syncTestSQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return "unclassified"
}

func TestSyncSchemaConstraintsPostgres(t *testing.T) {
	st, _ := newSyncSchemaTestStore(t)
	ctx := context.Background()
	if err := st.MigrateSync(ctx); err != nil {
		t.Fatal(err)
	}
	seedSyncSchemaRows(t, st, "acc_sync_constraints")
	// This account exists but owns none of the source account's sync parents.
	// Reusing source parent identifiers must fail a scoped composite FK, even
	// though the direct accounts FK is satisfied.
	if _, err := st.pool.Exec(ctx, `INSERT INTO accounts(id,display_name,is_default)
		VALUES('acc_sync_existing_other','Other sync schema fixture',false)`); err != nil {
		t.Fatal("insert other fixture account failed")
	}
	compositeParents := map[string]bool{
		"sync_operations": true, "sync_operation_payloads": true,
		"sync_receipts": true, "sync_reuse_refusals": true,
		"sync_tx_admissions": true, "sync_tx_touches": true,
		"sync_bundle_operations": true, "sync_publication_outbox": true,
		"sync_resource_reservations": true, "sync_deletion_knowledge": true,
		"sync_evidence_pins": true, "sync_payload_retirements": true,
	}
	patches := []string{
		`{"restore_marker":"\\x01"}`, `{"input_commitment":"\\x01"}`, `{"canonical_semantic_envelope":"\\x"}`,
		`{"public_key":"\\x01"}`, `{"status":"accepted","reason":"unauthorized"}`, `{"conflicting_commitment":"\\x01"}`,
		`{"coverage_state":"active"}`, `{"enabled":true}`, `{"atomic_group":null}`, `{"atomic_group":""}`,
		`{"last_finalized":-1}`, `{"sealed":false}`, `{"member_order":-1}`, `{"witness_proof_ref":"unexpected"}`,
		`{"last_accepted":-1}`, `{"pending_operations":501}`, `{"envelope_bytes":-1}`,
		`{"retain_until":"2026-01-02T00:00:00Z"}`, `{"evidence_kind":"unsupported"}`, `{"evidence_id":"missing"}`,
		`{"operation_id":"\\x01"}`,
	}
	for i, name := range syncSchemaFixtureTables {
		t.Run(name, func(t *testing.T) {
			var scoped bool
			if err := st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint
				WHERE conrelid=to_regclass($1) AND confrelid='accounts'::regclass AND contype='f')`, name).Scan(&scoped); err != nil || !scoped {
				t.Fatal("table lacks direct account foreign key")
			}
			quoted := pgx.Identifier{name}.Sanitize()
			query := `INSERT INTO ` + quoted + ` SELECT (jsonb_populate_record(NULL::` + quoted + `,to_jsonb(src)||$1::jsonb)).* FROM ` + quoted + ` src`
			_, err := st.pool.Exec(ctx, query, patches[i])
			want := "23514"
			if name == "sync_evidence_pins" {
				want = "23503"
			}
			if syncTestSQLState(err) != want {
				t.Fatalf("invalid table row SQLSTATE=%s want=%s", syncTestSQLState(err), want)
			}
			_, err = st.pool.Exec(ctx, query, `{"account_id":"acc_sync_missing"}`)
			if syncTestSQLState(err) != "23503" {
				t.Fatalf("unscoped account SQLSTATE=%s want=23503", syncTestSQLState(err))
			}
			var hasCompositeParent bool
			if err := st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint
				WHERE conrelid=to_regclass($1) AND contype='f' AND cardinality(conkey)>1)`, name).Scan(&hasCompositeParent); err != nil {
				t.Fatal("inspect scoped parent constraints failed")
			}
			if hasCompositeParent != compositeParents[name] {
				t.Fatal("scoped parent constraint fixture inventory differs")
			}
			if !hasCompositeParent {
				return
			}
			t.Run("existing_other_account_parent", func(t *testing.T) {
				_, err := st.pool.Exec(ctx, query, `{"account_id":"acc_sync_existing_other"}`)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
					t.Fatalf("existing other-account parent SQLSTATE=%s want=23503", syncTestSQLState(err))
				}
				var compositeRefused bool
				if err := st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint
					WHERE conrelid=to_regclass($1) AND conname=$2 AND contype='f'
					AND cardinality(conkey)>1 AND confrelid<>'accounts'::regclass)`, name, pgErr.ConstraintName).Scan(&compositeRefused); err != nil || !compositeRefused {
					t.Fatal("cross-account refusal did not come from a scoped composite parent constraint")
				}
			})
		})
	}
	for _, refusal := range []struct{ table, patch string }{
		{"sync_incarnations", `{"predecessor_epoch":1}`},
		{"sync_operations", `{"receipt_ref":null}`},
		{"sync_operations", `{"admission_state":"admitted"}`},
		{"sync_resource_budgets", `{"max_pending_operations":0}`},
		{"sync_resource_budgets", `{"max_retained_bytes":0}`},
	} {
		quoted := pgx.Identifier{refusal.table}.Sanitize()
		_, err := st.pool.Exec(ctx, `INSERT INTO `+quoted+` SELECT (jsonb_populate_record(NULL::`+quoted+`,to_jsonb(src)||$1::jsonb)).* FROM `+quoted+` src`, refusal.patch)
		if syncTestSQLState(err) != "23514" {
			t.Fatalf("%s closed-pair/budget constraint did not refuse", refusal.table)
		}
	}
	for _, patch := range []string{
		`{"retain_until":"2026-02-01T00:00:00Z"}`,
		`{"retired_at":"2026-02-20T00:00:00Z"}`,
		`{"signed_bytes":"\\x01"}`,
	} {
		_, err := st.pool.Exec(ctx, `INSERT INTO sync_receipts SELECT (jsonb_populate_record(NULL::sync_receipts,to_jsonb(src)||$1::jsonb)).* FROM sync_receipts src`, patch)
		if syncTestSQLState(err) != "23514" {
			t.Fatal("receipt floor/signature constraint did not refuse")
		}
	}
}

func TestSyncSchemaReceiptOriginalIdentityForeignKeyPostgres(t *testing.T) {
	st, _ := newSyncSchemaTestStore(t)
	ctx := context.Background()
	if err := st.MigrateSync(ctx); err != nil {
		t.Fatal(err)
	}
	seedSyncSchemaRows(t, st, "acc_sync_receipt_identity")
	// A second valid operation avoids colliding with the fixture receipt's
	// primary key. Its commitment and original identity match the fixture, and
	// its admitted state does not require a receipt before this test inserts one.
	if _, err := st.pool.Exec(ctx, `INSERT INTO sync_operations
		SELECT (jsonb_populate_record(NULL::sync_operations,to_jsonb(src)||
			jsonb_build_object('operation_id',decode(repeat('0e',16),'hex'),
				'admission_state','admitted','receipt_ref',NULL))).*
		FROM sync_operations src WHERE account_id='acc_sync_receipt_identity'`); err != nil {
		t.Fatalf("prepare receipt operation SQLSTATE=%s", syncTestSQLState(err))
	}
	const insertReceipt = `INSERT INTO sync_receipts
		SELECT (jsonb_populate_record(NULL::sync_receipts,to_jsonb(src)||
			jsonb_build_object('operation_id',decode(repeat('0e',16),'hex'),
				'acceptance_ordinal',2)||$1::jsonb)).*
		FROM sync_receipts src WHERE account_id='acc_sync_receipt_identity'`
	// Prove the common account, operation, commitment, issuer, and ordinal are
	// valid before varying identity. Roll back so each case has the same inputs.
	control, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin receipt identity control failed")
	}
	defer func() { _ = control.Rollback(ctx) }()
	result, err := control.Exec(ctx, insertReceipt, `{}`)
	if err != nil || result.RowsAffected() != 1 {
		t.Fatalf("valid receipt control rows=%d SQLSTATE=%s", result.RowsAffected(), syncTestSQLState(err))
	}
	if err := control.Rollback(ctx); err != nil {
		t.Fatal("rollback receipt identity control failed")
	}
	for _, tc := range []struct{ name, patch string }{
		{"original_lifecycle_epoch", `{"original_lifecycle_epoch":2}`},
		{"original_restore_marker", `{"original_restore_marker":"\\x02020202020202020202020202020202"}`},
		{"original_capture_epoch", `{"original_capture_epoch":2}`},
		{"original_installation", `{"original_installation":"other-installation"}`},
		{"original_replica", `{"original_replica":"other-replica"}`},
		{"original_write_lineage", `{"original_write_lineage":"other-lineage"}`},
		{"original_sequence", `{"original_sequence":2}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal("begin receipt identity refusal failed")
			}
			defer func() { _ = tx.Rollback(ctx) }()
			// Only one original identity component differs from the accepted
			// control. The separate commitment and issuer FKs stay satisfied.
			result, err := tx.Exec(ctx, insertReceipt, tc.patch)
			if syncTestSQLState(err) != "23503" {
				t.Fatalf("changed receipt identity rows=%d SQLSTATE=%s want=23503", result.RowsAffected(), syncTestSQLState(err))
			}
		})
	}
}

func TestSyncSchemaEvidenceAndEvacuationGuardsPostgres(t *testing.T) {
	st, _ := newSyncSchemaTestStore(t)
	ctx := context.Background()
	if err := st.MigrateSync(ctx); err != nil {
		t.Fatal(err)
	}
	seedSyncSchemaRows(t, st, "acc_sync_guards")
	immutable := []string{"sync_incarnations", "sync_operations", "sync_operation_payloads", "sync_verifier_history", "sync_receipts", "sync_reuse_refusals", "sync_bundles", "sync_bundle_operations", "sync_deletion_knowledge", "sync_retained_evidence", "sync_evidence_pins", "sync_payload_retirements", "sync_publication_outbox"}
	for _, name := range immutable {
		for _, query := range []string{"UPDATE %s SET account_id=account_id", "DELETE FROM %s", "TRUNCATE %s CASCADE"} {
			_, err := st.pool.Exec(ctx, fmt.Sprintf(query, pgx.Identifier{name}.Sanitize()))
			if syncTestSQLState(err) != "23514" {
				t.Errorf("%s evidence mutation was not refused: SQLSTATE=%s", name, syncTestSQLState(err))
			}
		}
	}
	t.Run("publication_transition", func(t *testing.T) {
		if _, err := st.pool.Exec(ctx, `INSERT INTO accounts(id,display_name,is_default)
			VALUES('acc_sync_outbox_other','Other outbox fixture',false)`); err != nil {
			t.Fatal("insert outbox comparison account failed")
		}
		for _, mutation := range []struct{ name, assignment string }{
			{"account_id", "account_id='acc_sync_outbox_other'"},
			{"issuer_stream", "issuer_stream='other-stream'"},
			{"through_ordinal", "through_ordinal=2"},
			{"prefix_commitment", "prefix_commitment=decode(repeat('0c',32),'hex')"},
			{"prefix_members", "prefix_members=decode('02','hex')"},
			{"operation_id", "operation_id=NULL"},
			{"proof_kind", "proof_kind='control'"},
		} {
			t.Run(mutation.name, func(t *testing.T) {
				// Both state fields request an otherwise valid transition. A
				// rejection therefore exercises immutable evidence comparison,
				// rather than the separate pending-to-pinned transition check.
				_, err := st.pool.Exec(ctx, `UPDATE sync_publication_outbox SET
					state='pinned',witness_proof_ref='fixture-proof',`+mutation.assignment+`
					WHERE account_id='acc_sync_guards'`)
				if syncTestSQLState(err) != "23514" {
					t.Fatalf("outbox evidence transition SQLSTATE=%s want=23514", syncTestSQLState(err))
				}
			})
		}
		t.Run("valid_pending_to_pinned", func(t *testing.T) {
			tag, err := st.pool.Exec(ctx, `UPDATE sync_publication_outbox SET
				state='pinned',witness_proof_ref='fixture-proof'
				WHERE account_id='acc_sync_guards'`)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("valid outbox transition failed with SQLSTATE=%s", syncTestSQLState(err))
			}
			var state, proof string
			if err := st.pool.QueryRow(ctx, `SELECT state,witness_proof_ref FROM sync_publication_outbox
				WHERE account_id='acc_sync_guards'`).Scan(&state, &proof); err != nil || state != "pinned" || proof != "fixture-proof" {
				t.Fatal("valid outbox transition did not preserve its publication result")
			}
		})
		t.Run("pinned_is_terminal", func(t *testing.T) {
			_, err := st.pool.Exec(ctx, `UPDATE sync_publication_outbox SET witness_proof_ref='replacement-proof'
				WHERE account_id='acc_sync_guards'`)
			if syncTestSQLState(err) != "23514" {
				t.Fatalf("terminal outbox mutation SQLSTATE=%s want=23514", syncTestSQLState(err))
			}
		})
	})
	rows, err := st.pool.Query(ctx, `SELECT c.relname FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid
		JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema()
		AND c.relname LIKE 'sync\_%' ESCAPE '\' AND t.tgname='account_evacuation_fence' ORDER BY c.relname`)
	if err != nil {
		t.Fatal("read sync evacuation coverage failed")
	}
	defer rows.Close()
	var guarded []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal("read sync evacuation row failed")
		}
		guarded = append(guarded, name)
	}
	if err := rows.Err(); err != nil || len(guarded) != len(syncSchemaFixtureTables) {
		t.Fatal("not every sync relation has the existing evacuation fence")
	}
	// The existing fence is enforced, not merely registered. Invalid duplicate
	// rows must hit the account fence before normal uniqueness constraints.
	gate, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin account evacuation fence failed")
	}
	defer func() { _ = gate.Rollback(ctx) }()
	if _, err := gate.Exec(ctx, `SELECT set_config('witself.evacuation_id','sync-test-evacuation',true)`); err != nil {
		t.Fatal("authorize account evacuation fence failed")
	}
	if _, err := gate.Exec(ctx, `UPDATE accounts SET evacuation_id='sync-test-evacuation',
		evacuation_started_at=now(),evacuation_role='source' WHERE id='acc_sync_guards'`); err != nil {
		t.Fatal("prepare account evacuation fence failed")
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal("commit account evacuation fence failed")
	}
	for _, name := range syncSchemaFixtureTables {
		quoted := pgx.Identifier{name}.Sanitize()
		_, err := st.pool.Exec(ctx, "INSERT INTO "+quoted+" SELECT * FROM "+quoted)
		if syncTestSQLState(err) != "55006" {
			t.Errorf("%s evacuation write was not refused: SQLSTATE=%s", name, syncTestSQLState(err))
		}
	}
	// Core relations gain no new sync capture guard in this slice.
	var guards int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid
		JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema()
		AND c.relname NOT LIKE 'sync\_%' ESCAPE '\' AND t.tgname LIKE 'sync\_%' ESCAPE '\'`).Scan(&guards); err != nil || guards != 0 {
		t.Fatal("sync migration installed a domain row guard")
	}
}

func TestSyncSchemaFixtureInventory(t *testing.T) {
	ddl, err := syncMigrationsFS.ReadFile("sync_migrations/0001_sync_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(string(ddl), "\n") {
		if strings.HasPrefix(line, "CREATE TABLE ") {
			names = append(names, strings.Fields(line)[2])
		}
	}
	if !reflect.DeepEqual(names, syncSchemaFixtureTables) {
		t.Fatal("production sync table has no populated constraint fixture")
	}
}
