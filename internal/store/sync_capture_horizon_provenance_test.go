package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/testenv"
)

// A snapshot can see an account yet retain a horizon older than its insert.
// The transaction supplying that horizon may already have ended. Therefore an
// idle backend in activation diagnostics is not, by itself, evidence of a leak.
func TestSyncCaptureAccountSnapshotHorizonProvenance(t *testing.T) {
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if _, err := st.pool.Exec(ctx, `CREATE TABLE accounts(id text PRIMARY KEY)`); err != nil {
		t.Fatal("horizon provenance table setup failed")
	}
	older, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal("horizon provenance older transaction failed")
	}
	defer func() { _ = older.Rollback(ctx) }()
	if _, err := older.Exec(ctx, `SELECT pg_current_xact_id()`); err != nil {
		t.Fatal("horizon provenance older transaction identity failed")
	}
	const account = "horizon_provenance_account"
	if _, err := st.pool.Exec(ctx, `INSERT INTO accounts(id) VALUES($1)`, account); err != nil {
		t.Fatal("horizon provenance account insert failed")
	}
	visible, err := st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal("horizon provenance visible transaction failed")
	}
	defer func() { _ = visible.Rollback(ctx) }()
	var exists bool
	if visible.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, account).Scan(&exists) != nil || !exists {
		t.Fatal("horizon provenance account must be visible")
	}
	if older.Rollback(ctx) != nil {
		t.Fatal("horizon provenance older transaction release failed")
	}
	probe, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal("horizon provenance safety probe failed")
	}
	defer func() { _ = probe.Rollback(ctx) }()
	if syncCaptureAccountSnapshotSafe(ctx, probe, account) {
		t.Fatal("horizon provenance expected conservative refusal of visible account snapshot")
	}
	logSyncCaptureActivityTypes(t, st, syncCaptureActivityScope{account: account, expectedPID: visible.Conn().PgConn().PID()})
	if visible.Rollback(ctx) != nil {
		t.Fatal("horizon provenance visible transaction release failed")
	}
	if !syncCaptureAccountSnapshotSafe(ctx, probe, account) {
		t.Fatal("horizon provenance refusal remained after exact visible transaction closed")
	}
	t.Log("horizon provenance: current test snapshot saw account; older transaction closed; current test snapshot alone caused conservative refusal")
}
