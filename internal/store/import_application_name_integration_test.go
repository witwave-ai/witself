package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestImportTransactionApplicationNamePostgres(t *testing.T) {
	for _, test := range []struct {
		name            string
		kind            string
		applicationName string
		lease           bool
		cancel          bool
	}{
		{name: "account", kind: "account", applicationName: "witself-import-account"},
		{name: "evacuation", kind: "evacuation", applicationName: "witself-import-evacuation"},
		{name: "evacuation_lease", kind: "evacuation", applicationName: "witself-import-evacuation", lease: true},
		{name: "validation", kind: "validation", applicationName: "witself-import-validation"},
		{name: "validation_lease", kind: "validation", applicationName: "witself-import-validation", lease: true},
		{name: "evacuation_cancel", kind: "evacuation", applicationName: "witself-import-evacuation", cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
			if err := st.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			t.Cleanup(cancel)
			account := provisionActiveEvacuationTestAccount(ctx, t, st, "import-application-name-"+test.name)
			const evacuationID = "evac_import_application_name"
			const backupID = "backup_import_application_name"
			var archive bytes.Buffer
			switch test.kind {
			case "account":
				if err := st.SuspendAccountSystem(ctx, account.AccountID, "evacuation", "import application name test"); err != nil {
					t.Fatal(err)
				}
				if err := st.ExportAccount(ctx, account.AccountID, "source-cell", "test", &archive); err != nil {
					t.Fatal(err)
				}
			case "evacuation":
				if _, err := st.BeginAccountEvacuation(ctx, account.AccountID, evacuationID, "import application name test"); err != nil {
					t.Fatal(err)
				}
				if err := st.ExportAccountEvacuation(ctx, account.AccountID, evacuationID, "source-cell", "test", &archive); err != nil {
					t.Fatal(err)
				}
			case "validation":
				if err := st.ExportAccountBackup(ctx, account.AccountID, backupID, "source-cell", "test", &archive); err != nil {
					t.Fatal(err)
				}
			}
			if test.kind == "evacuation" {
				if _, err := st.FinalizeAccountEvacuationSource(ctx, account.AccountID, evacuationID); err != nil {
					t.Fatal(err)
				}
			} else if err := deleteAccountForIntegrationTest(ctx, st, account.AccountID); err != nil {
				t.Fatal(err)
			}

			// Register observer cleanup before the leases so LIFO cleanup closes
			// the pinned session before releasing the observer. Acquisition itself
			// follows the fixture and lease setup, before the accounts table lock.
			var obs *pgxpool.Conn
			t.Cleanup(func() {
				if obs != nil {
					obs.Release()
				}
			})
			var importLease *AccountImportLease
			var validationLease *BackupValidationLease
			var leaseConn *pgxpool.Conn
			if test.lease {
				var err error
				if test.kind == "evacuation" {
					importLease, err = st.AcquireAccountImportLease(ctx, account.AccountID, evacuationID)
					if err == nil {
						t.Cleanup(importLease.Close)
						leaseConn = importLease.conn
					}
				} else {
					validationLease, err = st.AcquireBackupValidationLease(ctx, account.AccountID, backupID)
					if err == nil {
						t.Cleanup(validationLease.Close)
						leaseConn = validationLease.conn
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			var err error
			obs, err = st.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var sessionDefault string
			if err := obs.QueryRow(ctx, `SHOW application_name`).Scan(&sessionDefault); err != nil {
				t.Fatal(err)
			}

			locker, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cleanupCancel()
				if err := locker.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
					t.Errorf("roll back import table lock: %v", err)
				}
			})
			var lockerPID int
			if err := locker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&lockerPID); err != nil {
				t.Fatal(err)
			}
			if _, err := locker.Exec(ctx, `LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}

			importCtx, importCancel := context.WithCancel(ctx)
			done := make(chan struct{})
			var importErr error
			// LIFO cleanup cancels and drains the importer, rolls back the table
			// lock, closes any lease, releases the observer, and then permits the
			// fixture account cleanup, Store.Close and isolated schema deletion.
			t.Cleanup(func() {
				importCancel()
				timer := time.NewTimer(30 * time.Second)
				defer timer.Stop()
				select {
				case <-done:
				case <-timer.C:
					t.Error("application name importer did not drain after cancellation")
				}
			})
			go func() {
				defer close(done)
				reader := bytes.NewReader(archive.Bytes())
				switch {
				case importLease != nil:
					_, _, importErr = importLease.Import(importCtx, reader)
				case validationLease != nil:
					_, importErr = validationLease.Validate(importCtx, reader)
				case test.kind == "account":
					_, importErr = st.ImportAccount(importCtx, account.AccountID, reader)
				case test.kind == "evacuation":
					_, _, importErr = st.ImportAccountEvacuation(importCtx, account.AccountID, evacuationID, reader)
				case test.kind == "validation":
					_, importErr = st.ValidateAccountBackup(importCtx, account.AccountID, backupID, reader)
				}
			}()

			// Find this schema's importer by the lock it is waiting for, never
			// by the name under test. The observer remains outside a transaction
			// so each pg_stat_activity query takes a fresh activity snapshot.
			// Activity and blocking-pid observations can straddle wait entry;
			// keep polling until the activity snapshot also confirms Lock.
			var pid int
			var applicationName, backendType, waitEventType string
			var openTransaction bool
			deadline := time.Now().Add(30 * time.Second)
			for {
				rows, err := obs.Query(ctx, `
					SELECT pid, application_name, backend_type, wait_event_type, xact_start IS NOT NULL
					  FROM pg_stat_activity
					 WHERE $1::int = ANY(pg_blocking_pids(pid))
					   AND wait_event_type = 'Lock'`, lockerPID)
				if err != nil {
					t.Fatal(err)
				}
				var matchingRows []string
				for rows.Next() {
					if err := rows.Scan(&pid, &applicationName, &backendType, &waitEventType, &openTransaction); err != nil {
						rows.Close()
						t.Fatal(err)
					}
					matchingRows = append(matchingRows, fmt.Sprintf("pid=%d application_name=%q backend_type=%q wait_event_type=%q", pid, applicationName, backendType, waitEventType))
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if len(matchingRows) > 0 {
					if len(matchingRows) != 1 {
						t.Fatalf("blocked import backends = %d, want 1; matching rows: %v", len(matchingRows), matchingRows)
					}
					break
				}
				select {
				case <-done:
					t.Fatalf("import ended before waiting on the accounts lock: %v", importErr)
				default:
				}
				if !time.Now().Before(deadline) {
					t.Fatal("import did not wait on the accounts lock within 30s")
				}
				time.Sleep(50 * time.Millisecond)
			}
			if applicationName != test.applicationName {
				t.Errorf("blocked import application_name = %q, want %q", applicationName, test.applicationName)
			}
			if backendType != "client backend" || waitEventType != "Lock" || !openTransaction {
				t.Errorf("blocked import state = %q / %q / open=%v, want client backend / Lock / open=true", backendType, waitEventType, openTransaction)
			}
			if leaseConn != nil && uint32(pid) != leaseConn.Conn().PgConn().PID() {
				t.Errorf("blocked import pid = %d, want pinned lease pid %d", pid, leaseConn.Conn().PgConn().PID())
			}

			if test.cancel {
				started := time.Now()
				importCancel()
				timer := time.NewTimer(10 * time.Second)
				select {
				case <-done:
					timer.Stop()
				case <-timer.C:
					t.Fatal("cancelled import did not return within 10s")
				}
				if importErr == nil || importCtx.Err() == nil {
					t.Fatalf("cancelled import error = %v, context error = %v, want both non-nil", importErr, importCtx.Err())
				}
				t.Logf("cancelled import error = %v; errors.Is(context.Canceled) = %v", importErr, errors.Is(importErr, context.Canceled))
				// pgx asynchronously sends CancelRequest after the cancelled read.
				// Keep the locker held to prove cancellation clears the wait itself.
				deadline := time.Now().Add(5 * time.Second)
				for {
					var name, wait string
					err := obs.QueryRow(ctx, `SELECT application_name, COALESCE(wait_event_type, '') FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&name, &wait)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						t.Fatal(err)
					}
					if errors.Is(err, pgx.ErrNoRows) || name != test.applicationName || wait != "Lock" {
						t.Logf("cancelled import backend exited or cleared its named Lock wait after %s with locker held", time.Since(started))
						break
					}
					if !time.Now().Before(deadline) {
						t.Errorf("cancelled import backend retained its named Lock wait for 5s with locker held")
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
			if err := locker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if !test.cancel {
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("import did not drain after releasing the accounts lock")
				}
				if importErr != nil {
					t.Fatalf("complete blocked import: %v", importErr)
				}
			}

			// P1 checks the exact backend even after it leaves the pool. P2
			// independently inspects every reusable idle session, and executes
			// even if P1 reports a transaction-local application_name leak.
			checkPID := func() (bool, error) {
				var name string
				err := obs.QueryRow(ctx, `SELECT application_name FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&name)
				if errors.Is(err, pgx.ErrNoRows) {
					return true, nil
				}
				return err == nil && name == sessionDefault, err
			}
			if test.cancel {
				started := time.Now()
				deadline := started.Add(10 * time.Second)
				for {
					reset, err := checkPID()
					if reset && err == nil {
						t.Logf("P1 cancelled import backend reset or exited after %s", time.Since(started))
						break
					}
					if !time.Now().Before(deadline) {
						t.Errorf("P1 cancelled import backend did not reset or exit within 10s: %v", err)
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
			} else if reset, err := checkPID(); err != nil || !reset {
				t.Errorf("P1 import backend did not restore session application_name after transaction: %v", err)
			}
			idle := st.pool.AcquireAllIdle(ctx)
			if !test.cancel && len(idle) == 0 {
				t.Error("P2 no idle session available after successful import")
			}
			for _, conn := range idle {
				var name string
				err := conn.QueryRow(ctx, `SHOW application_name`).Scan(&name)
				conn.Release()
				if err != nil {
					t.Errorf("P2 read idle session application_name: %v", err)
				} else if name != sessionDefault {
					t.Errorf("P2 idle session application_name = %q, want session default %q", name, sessionDefault)
				}
			}
			if leaseConn != nil {
				var name string
				if err := leaseConn.QueryRow(ctx, `SHOW application_name`).Scan(&name); err != nil {
					t.Errorf("read retained lease session application_name: %v", err)
				} else if name != sessionDefault {
					t.Errorf("retained lease session application_name = %q, want session default %q", name, sessionDefault)
				}
			}
		})
	}
}

// FU115-2: a dead client can leave a lock waiter when its cancel request is lost
// and the server does not check client connections while waiting. Keep this
// transport failure independent of import cancellation, where pgx sends cancel.
func TestOrphanedLockWaiterPersistsWithoutConnectionCheck(t *testing.T) {
	_, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	connect := func(config *pgx.ConnConfig) *pgx.Conn {
		t.Helper()
		conn, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()
			if err := conn.Close(cleanupCtx); err != nil {
				t.Errorf("close orphan test connection: %v", err)
			}
		})
		return conn
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	obs := connect(config.Copy())
	var interval string
	var connectionChecksEnabled bool
	if err := obs.QueryRow(ctx, `SELECT current_setting('client_connection_check_interval'), current_setting('client_connection_check_interval')::interval > interval '0'`).Scan(&interval, &connectionChecksEnabled); err != nil {
		t.Fatal(err)
	}
	if connectionChecksEnabled {
		t.Skipf("FU115-2 orphan persistence requires disabled connection checks; server client_connection_check_interval=%s", interval)
	}
	t.Logf("FU115-2 server client_connection_check_interval=%s", interval)
	if _, err := obs.Exec(ctx, `CREATE TABLE orphan_lock_waiter (id integer PRIMARY KEY); INSERT INTO orphan_lock_waiter VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	lockerConn := connect(config.Copy())
	locker, err := lockerConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := locker.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("roll back orphan test row lock: %v", err)
		}
	})
	if _, err := locker.Exec(ctx, `SELECT id FROM orphan_lock_waiter WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	var refuseDials atomic.Bool
	var refusedDials atomic.Int32
	waiterConfig := config.Copy()
	dial := waiterConfig.DialFunc
	waiterConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		if refuseDials.Load() {
			refusedDials.Add(1)
			return nil, errors.New("FU115-2 injected cancel dial refusal")
		}
		return dial(ctx, network, address)
	}
	waiter := connect(waiterConfig)
	waiterPID := waiter.PgConn().PID()
	lockerPID := lockerConn.PgConn().PID()
	done := make(chan error, 1)
	go func() {
		_, err := waiter.Exec(ctx, `SELECT id FROM orphan_lock_waiter WHERE id = 1 FOR UPDATE`)
		done <- err
	}()
	// Closing the raw connection also drains the goroutine on setup failures.
	// Do not call pgx methods concurrently with the running statement.
	t.Cleanup(func() {
		_ = waiter.PgConn().Conn().Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("orphan test waiter did not drain")
		}
	})
	const pollInterval = 50 * time.Millisecond
	lockWait := func() bool {
		t.Helper()
		var waiting bool
		if err := obs.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			 WHERE pid = $1 AND wait_event_type = 'Lock'
			   AND $2::int = ANY(pg_blocking_pids(pid)))`, waiterPID, lockerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		return waiting
	}
	deadline := time.Now().Add(30 * time.Second)
	for !lockWait() {
		if !time.Now().Before(deadline) {
			t.Fatal("orphan test waiter did not enter Lock wait within 30s")
		}
		time.Sleep(pollInterval)
	}
	refuseDials.Store(true)
	started := time.Now()
	if err := waiter.PgConn().Conn().Close(); err != nil {
		t.Fatal(err)
	}
	for {
		if !lockWait() {
			t.Fatalf("orphan backend left Lock wait after %s with locker held, want at least 2s", time.Since(started))
		}
		if time.Since(started) >= 2*time.Second {
			break
		}
		time.Sleep(pollInterval)
	}
	if refusedDials.Load() == 0 {
		t.Error("orphan test did not intercept a pgx cancel dial")
	}
	t.Logf("FU115-2 orphan remained in Lock wait for %s with cancel dial refused and locker held", time.Since(started))
	if err := locker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	deadline = started.Add(5 * time.Second)
	for {
		var present bool
		if err := obs.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, waiterPID).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if !present {
			t.Logf("FU115-2 orphan backend exited %s after locker release", time.Since(started))
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("orphan backend still present 5s after locker release")
		}
		time.Sleep(pollInterval)
	}
}
