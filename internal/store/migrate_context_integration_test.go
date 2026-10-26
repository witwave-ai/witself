package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMigrateContextAdvisoryLockCanceledPostgres(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			setupStarted := time.Now()
			st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
			db := migrationTestSQLDB(t, dsn)
			t.Cleanup(func() { _ = db.Close() })
			gate, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = gate.Close() })
			if err := acquireMigrationAdvisoryLock(context.Background(), gate); err != nil {
				t.Fatal(err)
			}
			held := true
			t.Cleanup(func() {
				if held {
					_ = releaseMigrationAdvisoryLock(context.Background(), gate)
				}
			})
			// Derive the return allowance from this fixture's own connection/setup
			// cost and its chosen wait, including the production 100ms poll period.
			wait := 2*time.Since(setupStarted) + 200*time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- st.MigrateContext(ctx) }()
			if mode == "cancel" {
				timer := time.NewTimer(wait / 2)
				defer timer.Stop()
				select {
				case err := <-result:
					t.Fatalf("migration returned while the lock was held: %v", err)
				case <-timer.C:
					cancel()
				}
			}
			err = awaitMigrationResult(t, result, 10*wait)
			assertMigrationCanceled(t, err, "advisory lock")
			var tables int
			if err := db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema()`).Scan(&tables); err != nil {
				t.Fatal(err)
			}
			if tables != 0 {
				t.Fatalf("canceled migration created %d tables", tables)
			}
			if err := releaseMigrationAdvisoryLock(context.Background(), gate); err != nil {
				t.Fatal(err)
			}
			held = false
			assertMigrationLockAvailable(t, gate)
		})
	}
}

func TestMigrateSchemaCanceledReturnsDedicatedConnectionPostgres(t *testing.T) {
	started := time.Now()
	st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	// Keep the pool under test separate from the session holding the gate so
	// InUse measures only the dedicated connection acquired by the migrator.
	db := migrationTestSQLDB(t, dsn)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	var migrationPID int
	if err := db.QueryRow(`SELECT pg_backend_pid()`).Scan(&migrationPID); err != nil {
		t.Fatal(err)
	}
	gateDB := migrationTestSQLDB(t, dsn)
	t.Cleanup(func() { _ = gateDB.Close() })
	gate, err := gateDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gate.Close() })
	if err := acquireMigrationAdvisoryLock(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	held := true
	t.Cleanup(func() {
		if held {
			_ = releaseMigrationAdvisoryLock(context.Background(), gate)
		}
	})

	allowance := 20 * (time.Since(started) + 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := migrateSchemaWithAdvisoryLock(ctx, db, int64(SchemaVersion()))
		result <- err
	}()
	// Observe an unsuccessful lock attempt before canceling. This also avoids
	// depending on cancellation of an in-flight SQL query to clean up the pool.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), allowance)
	defer waitCancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := st.pool.QueryRow(waitCtx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND pid=$1 AND state='idle'
			  AND query LIKE '%pg_try_advisory_lock%')`, migrationPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting && db.Stats().InUse == 1 {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("migration returned while its lock was held: %v", err)
		case <-waitCtx.Done():
			t.Fatal("migration did not acquire its dedicated connection and attempt the lock")
		case <-ticker.C:
		}
	}
	cancel()
	assertMigrationCanceled(t, awaitMigrationResult(t, result, allowance), "advisory lock")
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Errorf("canceled migration left %d dedicated connections checked out, want 0", inUse)
	}
	if err := releaseMigrationAdvisoryLock(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	held = false
	assertMigrationLockAvailable(t, gate)
}

func TestMigrateContextSerializesReplicasPostgres(t *testing.T) {
	started := time.Now()
	observer, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	migrationTestUpTo(t, dsn, 61)
	// Give only these two migration callers an application name so the gate
	// is not released until both have actually attempted its advisory lock.
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	applicationName := t.Name()
	query.Set("application_name", applicationName)
	parsed.RawQuery = query.Encode()
	var replicas [2]*Store
	for i := range replicas {
		replicas[i], err = Open(context.Background(), parsed.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(replicas[i].Close)
	}
	gateDB := migrationTestSQLDB(t, dsn)
	t.Cleanup(func() { _ = gateDB.Close() })
	gate, err := gateDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gate.Close() })
	if err := acquireMigrationAdvisoryLock(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	held := true
	t.Cleanup(func() {
		if held {
			_ = releaseMigrationAdvisoryLock(context.Background(), gate)
		}
	})

	// The setup includes applying the first 61 files. Scale the generous
	// concurrent-migration budget from that work and the production poll period.
	allowance := 100 * (time.Since(started) + 100*time.Millisecond)
	results := make(chan error, len(replicas))
	for _, replica := range replicas {
		ctx, cancel := context.WithTimeout(context.Background(), allowance)
		defer cancel()
		go func() { results <- replica.MigrateContext(ctx) }()
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), allowance)
	defer waitCancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		if err := observer.pool.QueryRow(waitCtx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database() AND application_name=$1 AND state='idle'
			  AND query LIKE '%pg_try_advisory_lock%'`, applicationName).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == len(replicas) {
			break
		}
		select {
		case err := <-results:
			t.Fatalf("context migration returned before the gate was released: %v", err)
		case <-waitCtx.Done():
			t.Fatal("both context migrations did not reach the advisory lock")
		case <-ticker.C:
		}
	}
	if err := releaseMigrationAdvisoryLock(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	held = false
	for range replicas {
		if err := awaitMigrationResult(t, results, allowance); err != nil {
			t.Fatalf("concurrent MigrateContext: %v", err)
		}
	}
	assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
	assertMigrationLockAvailable(t, gate)
}

func TestMigrateContextPreflightCanceledReleasesLockPostgres(t *testing.T) {
	started := time.Now()
	st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	db := migrationTestSQLDB(t, dsn)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE accounts (id text)`); err != nil {
		t.Fatal(err)
	}
	gate, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gate.Close() })
	tx, err := gate.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	wait := 2*time.Since(started) + 200*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- st.MigrateContext(ctx) }()
	assertMigrationCanceled(t, awaitMigrationResult(t, result, 10*wait), "preflight")
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertMigrationLockAvailable(t, gate)
	var versionTable bool
	if err := db.QueryRow(`SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&versionTable); err != nil {
		t.Fatal(err)
	}
	if versionTable {
		t.Fatal("preflight cancellation initialized Goose state")
	}
}

func TestMigrateContextFinishesFileBeforeCancellationPostgres(t *testing.T) {
	for _, mode := range []string{"transaction", "no transaction"} {
		t.Run(mode, func(t *testing.T) {
			started := time.Now()
			st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
			db := migrationTestSQLDB(t, dsn)
			t.Cleanup(func() { _ = db.Close() })
			gate, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = gate.Close() })
			const gateKey = `hashtextextended(current_schema() || ':migration-file-test', 0)`
			if _, err := gate.ExecContext(context.Background(), `SELECT pg_advisory_lock(`+gateKey+`)`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = gate.ExecContext(context.Background(), `SELECT pg_advisory_unlock(`+gateKey+`)`) })
			prefix := ""
			if mode == "no transaction" {
				prefix = "-- +goose NO TRANSACTION\n"
			}
			goose.SetBaseFS(fstest.MapFS{
				"migrations/00001_first.sql": &fstest.MapFile{Data: []byte(prefix + "-- +goose Up\nSELECT pg_advisory_xact_lock(" + gateKey + ");\nCREATE TABLE file_completed (id integer);\n")},
				"migrations/00002_next.sql":  &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE next_file_started (id integer);\n")},
			})
			t.Cleanup(func() { goose.SetBaseFS(migrationsFS) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- applyMigrationsContext(ctx, db) }()
			wait := 20 * (time.Since(started) + 100*time.Millisecond)
			waitCtx, waitCancel := context.WithTimeout(context.Background(), wait)
			defer waitCancel()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var blocked bool
				if err := st.pool.QueryRow(waitCtx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%:migration-file-test%')`).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("migration returned before file was blocked: %v", err)
				case <-waitCtx.Done():
					t.Fatal("migration did not enter its first file")
				case <-ticker.C:
				}
			}
			cancel()
			select {
			case err := <-result:
				t.Fatalf("cancellation interrupted a migration file: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			if _, err := gate.ExecContext(context.Background(), `SELECT pg_advisory_unlock(`+gateKey+`)`); err != nil {
				t.Fatal(err)
			}
			assertMigrationCanceled(t, awaitMigrationResult(t, result, 10*time.Since(started)), "verify")
			var first, next bool
			if err := db.QueryRow(`SELECT to_regclass('file_completed') IS NOT NULL, to_regclass('next_file_started') IS NOT NULL`).Scan(&first, &next); err != nil {
				t.Fatal(err)
			}
			if !first || next {
				t.Fatalf("file completion: first=%t next=%t", first, next)
			}
			version, err := goose.GetDBVersion(db)
			if err != nil || version != 1 {
				t.Fatalf("version after canceled application = %d, err=%v", version, err)
			}
		})
	}
}

func TestMigrateContextFreshAndLegacyPostgres(t *testing.T) {
	for _, mode := range []string{"context", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
			var err error
			if mode == "legacy" {
				err = st.Migrate()
			} else {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				err = st.MigrateContext(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
		})
	}
}

func assertMigrationCanceled(t *testing.T, err error, phase string) {
	t.Helper()
	if !errors.Is(err, ErrMigrationWaitCanceled) || MigrationWaitPhase(err) != phase || err.Error() != "migration wait canceled: "+phase {
		t.Fatalf("error = %v, want value-free cancellation in %s", err, phase)
	}
}

func awaitMigrationResult(t *testing.T, result <-chan error, wait time.Duration) error {
	t.Helper()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		t.Fatal("migration did not return within the fixture-relative allowance")
		return nil
	}
}

func assertMigrationLockAvailable(t *testing.T, conn *sql.Conn) {
	t.Helper()
	var acquired bool
	if err := conn.QueryRowContext(context.Background(), migrationAdvisoryTryLockSQL).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("canceled migration retained the advisory lock")
	}
	if err := releaseMigrationAdvisoryLock(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
}
