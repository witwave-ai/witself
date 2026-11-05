package store

import (
	"context"
	"io/fs"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/witwave-ai/witself/internal/testenv"
)

// Use the same predicate for the negative control and positive observation.
// The database and locktype guards also exclude same-numbered foreign objects.
const captureDrainWaitSQL = `SELECT EXISTS(
 SELECT 1 FROM pg_locks w JOIN pg_locks h
 ON h.database=w.database AND h.relation=w.relation
 WHERE w.locktype='relation' AND h.locktype='relation'
 AND w.database=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND w.relation=$1 AND w.mode='AccessExclusiveLock' AND NOT w.granted
 AND h.granted AND h.pid=$2 AND h.mode='RowExclusiveLock')`

func captureDrainAllowance(elapsed time.Duration) time.Duration {
	// Core migration includes this database's migration-lock contention. Allow
	// repeated foreign migrations at that observed pace, not just a fixed 10s.
	return max(10*time.Second, 10*elapsed)
}

type captureDrainObserver struct {
	query syncVersionReader
	oid   uint32
}

func newCaptureDrainObserver(ctx context.Context, t *testing.T, st *Store) *captureDrainObserver {
	t.Helper()
	conn, err := pgx.ConnectConfig(ctx, st.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(captureObservationFailure(err))
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if conn.Close(closeCtx) != nil {
			t.Error("capture observer close failed")
		}
	})
	observer := &captureDrainObserver{query: conn}
	// Bind the fixture's relation while no installer can be queued. Subsequent
	// observations need neither a pooled connection nor name resolution.
	if err = conn.QueryRow(ctx, `SELECT 'accounts'::regclass::oid`).Scan(&observer.oid); err != nil {
		t.Fatal(captureObservationFailure(err))
	}
	return observer
}

func (o *captureDrainObserver) waiting(ctx context.Context, pid uint32) (bool, error) {
	var waiting bool
	err := o.query.QueryRow(ctx, captureDrainWaitSQL, o.oid, pid).Scan(&waiting)
	return waiting, err
}

func runCaptureInstallationDrain(t *testing.T, commit, exhaustPool bool) {
	t.Helper()
	started := time.Now()
	ctx := context.Background()
	st, dsn := newSyncSchemaTestStore(t)
	if exhaustPool {
		config := st.pool.Config()
		st.pool.Close()
		config.MaxConns, config.MinConns = 2, 0
		var err error
		st.pool, err = pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatal("capture pressure pool setup failed")
		}
	}
	db := migrationTestSQLDB(t, dsn)
	defer func() { _ = db.Close() }()
	fsys, err := fs.Sub(syncMigrationsFS, "sync_migrations")
	if err != nil {
		t.Fatal("capture drain namespace failed")
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithTableName(syncSchemaVersionTable), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal("capture drain provider failed")
	}
	if _, err = provider.UpTo(ctx, 1); err != nil {
		t.Fatal("capture drain base sync failed")
	}
	allowance := captureDrainAllowance(time.Since(started))
	setupCtx, setupCancel := context.WithTimeout(ctx, allowance)
	defer setupCancel()
	observer := newCaptureDrainObserver(setupCtx, t, st)
	tx, err := st.pool.Begin(setupCtx)
	if err != nil {
		t.Fatal("capture drain begin failed")
	}
	// Also cover failures before the installer starts.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(ctx, allowance)
		defer cancel()
		_ = tx.Rollback(cleanupCtx)
	}()
	if _, err = tx.Exec(setupCtx, `INSERT INTO accounts(id,display_name,is_default) VALUES('drain_account','drain',false)`); err != nil {
		t.Fatal("capture drain write failed")
	}
	pid := tx.Conn().PgConn().PID()
	if waiting, err := observer.waiting(setupCtx, pid); err != nil {
		t.Fatal(captureObservationFailure(err))
	} else if waiting {
		t.Fatal("capture drain accepted granted lock without queued installer")
	}
	installCtx, cancelInstall := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() { result <- st.InstallSyncGuards(installCtx) }()
	joined := false
	var held *pgxpool.Conn
	// Fatal observation must release the writer and join the installer BEFORE
	// fixture pool/schema cleanup; cancellation alone cannot stop atomic DDL.
	defer func() {
		cancelInstall()
		if held != nil {
			held.Release()
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, allowance)
		defer cancel()
		_ = tx.Rollback(cleanupCtx)
		if !joined {
			select {
			case <-result:
			case <-cleanupCtx.Done():
				t.Error("capture drain installer cleanup timed out")
			}
		}
	}()
	observe := func(ctx context.Context) (bool, error) { return observer.waiting(ctx, pid) }
	pollCtx, pollCancel := context.WithTimeout(ctx, allowance)
	defer pollCancel()
	captureObserve(pollCtx, t, observe)
	t.Log("capture observed installation relation wait on accounts")
	if exhaustPool {
		// A queued AccessExclusiveLock does not itself prevent regclass resolution.
		// Retain this control instead of attributing a deadline to catalog blocking.
		var resolved uint32
		if err := observer.query.QueryRow(pollCtx, `SELECT 'accounts'::regclass::oid`).Scan(&resolved); err != nil {
			t.Fatal(captureObservationFailure(err))
		}
		if resolved != observer.oid {
			t.Fatal("capture queued relation identity changed")
		}
		t.Log("capture resolved regclass while installer relation lock queued")
		// Wait for preflight to finish before saturating: it legitimately uses the
		// fixture pool. The queued installer now owns its independent DDL session.
		held, err = st.pool.Acquire(pollCtx)
		if err != nil {
			t.Fatal(captureObservationFailure(capturePoolAcquireError{err}))
		}
		if st.pool.Stat().AcquiredConns() != st.pool.Stat().MaxConns() {
			t.Fatal("capture pressure pool was not exhausted")
		}
		pressureCtx, pressureCancel := context.WithTimeout(ctx, allowance)
		defer pressureCancel()
		captureObserve(pressureCtx, t, observe)
		pressureCancel()
		held.Release()
		held = nil
		t.Log("capture observed exact installation wait with fixture pool exhausted")
	}
	// The observer never substitutes completion for the required lock evidence.
	select {
	case <-result:
		joined = true
		t.Fatal("capture installer completed before writer release")
	default:
	}
	releaseCtx, releaseCancel := context.WithTimeout(ctx, allowance)
	defer releaseCancel()
	if commit {
		err = tx.Commit(releaseCtx)
	} else {
		err = tx.Rollback(releaseCtx)
	}
	if err != nil {
		t.Fatal("capture drain release failed")
	}
	completionCtx, completionCancel := context.WithTimeout(ctx, allowance)
	defer completionCancel()
	select {
	case err = <-result:
		joined = true
		if err != nil {
			t.Fatal("capture drain install failed")
		}
	case <-completionCtx.Done():
		t.Fatal("capture drain completion timed out")
	}
	if validateInstalledSyncWriterInventory(completionCtx, st) != nil {
		t.Fatal("capture drain installed guard inventory failed")
	}
	var found bool
	if st.pool.QueryRow(completionCtx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id='drain_account')`).Scan(&found) != nil || found != commit {
		t.Fatal("capture drain visibility differs")
	}
	var prepared int
	if st.pool.QueryRow(completionCtx, `SELECT current_setting('max_prepared_transactions')::integer`).Scan(&prepared) != nil {
		t.Fatal("capture prepared setting failed")
	}
	if prepared == 0 {
		t.Log("NOT RUN prepared_transactions: max_prepared_transactions=0")
	} else {
		t.Fatal("capture prepared case needs isolated owner execution")
	}
}

func TestSyncCaptureDrainObserverPoolPressure(t *testing.T) {
	_ = testenv.RequirePostgres(t)
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "commit", false: "rollback"}[commit], func(t *testing.T) { runCaptureInstallationDrain(t, commit, true) })
	}
}
