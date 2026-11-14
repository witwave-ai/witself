package testenv

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
	"github.com/witwave-ai/witself/internal/store"
)

// The two synthetic packages go through the real provisioner and derivation.
// Only their disposable schemas are removed: package databases are reusable.
func TestPostgresPackageDatabaseIsolation(t *testing.T) {
	configured := os.Getenv(databaseURLEnv)
	selected := RequirePostgres(t)
	baseConfig, baseErr := pgx.ParseConfig(configured)
	selectedConfig, selectedErr := pgx.ParseConfig(selected)
	if baseErr != nil || selectedErr != nil {
		t.Fatal("proof database configuration unavailable")
	}
	if selectedConfig.Database == baseConfig.Database {
		t.Skip("package database isolation proof requires provisioning")
	}
	ctx, cancel := context.WithTimeout(t.Context(), packageDatabaseTimeout)
	defer cancel()
	dsnA, err := provisionPostgresDatabase(ctx, configured, "github.com/witwave-ai/witself/proof/package_a")
	if err != nil {
		t.Fatal("package A proof provisioning failed")
	}
	dsnB, err := provisionPostgresDatabase(ctx, configured, "github.com/witwave-ai/witself/proof/package_b")
	if err != nil {
		t.Fatal("package B proof provisioning failed")
	}
	if dsnA == dsnB {
		t.Fatal("synthetic packages selected the same database")
	}
	t.Run("MigrationLock", func(t *testing.T) { provePackageMigrationLock(t, dsnA, dsnB) })
	t.Run("OldSnapshot", func(t *testing.T) { provePackageOldSnapshot(t, dsnA, dsnB) })
}

func proofSQLDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("proof SQL connection configuration failed")
	}
	t.Cleanup(func() {
		if db.Close() != nil {
			t.Error("proof SQL pool close failed")
		}
	})
	return db
}

func proofConnection(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal("proof connection failed")
	}
	t.Cleanup(func() {
		if conn.Close(context.Background()) != nil {
			t.Error("proof connection close failed")
		}
	})
	return conn
}

func proofStore(t *testing.T, dsn string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal("proof store configuration failed")
	}
	t.Cleanup(st.Close)
	return st
}

func provePackageMigrationLock(t *testing.T, dsnA, dsnB string) {
	started := time.Now()
	baseline := NewPostgresSchema(t, dsnB)
	if proofStore(t, baseline.DSN()).MigrateContext(t.Context()) != nil {
		t.Fatal("calibrate full migration proof failed")
	}
	// A full migration is the baseline, not connection setup: filesystem and
	// catalog work can dominate a fast local connection on a loaded host.
	budget := 20 * time.Since(started)
	a := NewPostgresSchema(t, dsnA)
	b := NewPostgresSchema(t, dsnB)
	holder := proofConnection(t, a.DSN())
	observer := proofConnection(t, a.DSN())
	stA, stB := proofStore(t, a.DSN()), proofStore(t, b.DSN())
	ctx := context.Background()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended(current_database() || ':witself:store:migrate:v1', 0))`); err != nil {
		t.Fatal("proof migration lock acquisition failed")
	}
	held := true
	release := func() {
		if held {
			held = false
			if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended(current_database() || ':witself:store:migrate:v1', 0))`); err != nil {
				t.Error("proof migration lock release failed")
			}
		}
	}
	defer release()
	// Bounds scale with observed setup work; no fixed completion-time assertion.
	positiveCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if stB.MigrateContext(positiveCtx) != nil {
		t.Fatal("package B full migration blocked behind package A lock")
	}
	budget = 20 * time.Since(started)
	negativeCtx, negativeCancel := context.WithTimeout(ctx, budget)
	defer negativeCancel()
	done := make(chan error, 1)
	go func() { done <- stA.MigrateContext(negativeCtx) }()
	defer func() {
		release()
		proofAwaitResult(t, done, budget, "same database migration after release")
	}()
	proofObserveBlocked(t, observer, done, budget, `SELECT EXISTS (
		SELECT 1 FROM pg_stat_activity a WHERE datname=current_database()
		AND pid <> pg_backend_pid() AND pid <> $1 AND state='idle'
		AND query LIKE '%pg_try_advisory_lock%'
		AND NOT EXISTS (SELECT 1 FROM pg_locks l WHERE l.pid=a.pid
		AND l.locktype='advisory' AND l.granted))`, holder.PgConn().PID())
	var tables int
	if observer.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema=$1`, a.Name()).Scan(&tables) != nil || tables != 0 {
		t.Fatal("same database negative control migrated while lock was held")
	}
	t.Log("different database full migration completed; same database migration waited for the lock")
}

func provePackageOldSnapshot(t *testing.T, dsnA, dsnB string) {
	started := time.Now()
	anchor := NewPostgresSchema(t, dsnA)
	a := NewPostgresSchema(t, dsnA)
	b := NewPostgresSchema(t, dsnB)
	holder := proofConnection(t, anchor.DSN())
	observer := proofConnection(t, a.DSN())
	dbA, dbB := proofSQLDB(t, a.DSN()), proofSQLDB(t, b.DSN())
	goose.SetBaseFS(store.MigrationFiles())
	if goose.SetDialect("postgres") != nil || goose.UpTo(dbA, "migrations", 52) != nil || goose.UpTo(dbB, "migrations", 52) != nil {
		t.Fatal("prepare concurrent index migration proof failed")
	}
	ctx := context.Background()
	if _, err := holder.Exec(ctx, `CREATE TABLE snapshot_anchor (id integer)`); err != nil {
		t.Fatal("create snapshot proof anchor failed")
	}
	tx, err := holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal("park repeatable read snapshot failed")
	}
	held := true
	release := func() {
		if held {
			held = false
			if tx.Rollback(ctx) != nil {
				t.Error("release repeatable read snapshot failed")
			}
		}
	}
	defer release()
	if _, err := tx.Exec(ctx, `SELECT * FROM snapshot_anchor`); err != nil {
		t.Fatal("acquire repeatable read snapshot failed")
	}
	budget := 20 * time.Since(started)
	positiveCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if goose.UpByOneContext(positiveCtx, dbB, "migrations") != nil {
		t.Fatal("package B concurrent index migration blocked behind package A snapshot")
	}
	version, err := goose.GetDBVersion(dbB)
	if err != nil || version != 53 {
		t.Fatal("positive control did not execute the real concurrent index migration")
	}
	done := make(chan error, 1)
	negativeCtx, negativeCancel := context.WithTimeout(ctx, budget)
	defer negativeCancel()
	go func() { done <- goose.UpByOneContext(negativeCtx, dbA, "migrations") }()
	defer func() {
		release()
		proofAwaitResult(t, done, budget, "same database index migration after release")
	}()
	proofObserveBlocked(t, observer, done, budget, `SELECT EXISTS (
		SELECT 1 FROM pg_stat_progress_create_index WHERE datname=current_database()
		AND phase='waiting for old snapshots' AND current_locker_pid=$1)`, holder.PgConn().PID())
	t.Log("different database migration 0053 completed; same database index waited for the old snapshot")
}

func proofObserveBlocked(t *testing.T, observer *pgx.Conn, done chan error, budget time.Duration, query string, pid uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	for {
		var blocked bool
		if observer.QueryRow(ctx, query, pid).Scan(&blocked) != nil {
			t.Fatal("negative control wait observation failed")
		}
		select {
		case err := <-done:
			done <- err // Preserve the result for independent deferred cleanup.
			t.Fatal("negative control returned while its holder remained parked")
		default:
		}
		if blocked {
			return
		}
		// One catalog round trip is the polling interval, avoiding a timing oracle.
	}
}

func proofAwaitResult(t *testing.T, done <-chan error, budget time.Duration, label string) {
	t.Helper()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Error(label + " failed")
		}
	case <-timer.C:
		t.Error(label + " exceeded proportional completion budget")
	}
}
