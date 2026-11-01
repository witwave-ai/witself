package store

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn" // LC-S6C1 fix 1 (F6)
	"github.com/pressly/goose/v3"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/syncharness"
	"github.com/witwave-ai/witself/internal/testenv"
)

func newSyncCaptureFixture(t *testing.T, active bool) (*Store, Principal) {
	t.Helper()
	st, _ := newSyncSchemaTestStore(t)
	ctx := context.Background()
	if err := st.MigrateSync(ctx); err != nil {
		t.Fatal("capture fixture migration failed")
	}
	if err := st.SelectSyncCapture(runtimeconfig.Config{Local: true, SyncSchema: true}); err != nil {
		t.Fatal("capture fixture selection failed")
	}
	p := Principal{Kind: PrincipalAgent, AccountID: "capture_account", RealmID: "capture_realm", ID: "capture_agent", AccountStatus: "active"}
	for _, sql := range []string{
		`INSERT INTO accounts(id,display_name,is_default,status) VALUES('capture_account','Capture fixture',false,'active')`,
		`INSERT INTO realms(id,account_id,name) VALUES('capture_realm','capture_account','default')`,
		`INSERT INTO agents(id,realm_id,name) VALUES('capture_agent','capture_realm','first'),('capture_second','capture_realm','second')`,
		`INSERT INTO sync_incarnations(account_id,lifecycle_epoch,restore_marker,allocation_id,transition_id,authority_installation,state) VALUES('capture_account',1,decode(repeat('01',16),'hex'),decode(repeat('02',16),'hex'),decode(repeat('03',16),'hex'),'local','installed')`,
	} {
		if _, err := st.pool.Exec(ctx, sql); err != nil {
			t.Fatalf("capture fixture seed SQLSTATE %s", syncTestSQLState(err))
		}
	}
	if err := st.EnableSyncWriter(ctx, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability); err != nil {
		logSyncCaptureActivityTypes(t, st)
		t.Fatal("capture fixture capability failed")
	}
	if active {
		activateSyncFixture(t, st, p.AccountID)
	}
	return st, p
}

func activateSyncFixture(t *testing.T, st *Store, account string) {
	t.Helper()
	ctx := context.Background()
	a, err := st.ActivateSyncCapture(ctx, account, SyncCaptureOptions{LifecycleEpoch: 1, RestoreMarker: bytes.Repeat([]byte{1}, 16), Capability: SyncPreferencesCapability})
	if err != nil {
		logSyncCaptureActivityTypes(t, st)
		t.Fatal("capture fixture activation failed")
	}
	defer a.Close()
	p, err := a.PinSnapshot(ctx)
	if err != nil {
		t.Fatal("capture fixture snapshot failed")
	}
	defer func() { _ = p.Close(ctx) }()
	if a.Acknowledge(ctx, p) != nil || !p.Acknowledged() {
		t.Fatal("capture fixture acknowledgment failed")
	}
}

func captureEvidence(t *testing.T, checks, required int) {
	t.Helper()
	if t.Failed() {
		return
	}
	if checks != required {
		t.Log("capture evidence omitted for partial scenario execution")
		return
	}
	r, err := syncharness.VerifyCaptureRuntimeArtifact(t.Name(), checks)
	if err != nil {
		t.Fatal("capture evidence binding failed")
	}
	t.Logf("capture evidence capability=%s class=%s build=%s checks=%d", r.Capability, r.EvidenceClass, r.Executable, checks)
}

func capturePoll(t *testing.T, st *Store, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var ok bool
		if st.pool.QueryRow(ctx, query, args...).Scan(&ok) != nil {
			t.Fatal("capture lock observation failed")
		}
		if ok {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("capture lock observation timed out")
		case <-ticker.C:
		}
	}
}

func captureRefusedSQL(t *testing.T, err error) { // LC-S6C1 fix 2 (F6)
	t.Helper()                                                                                                 // LC-S6C1 fix 2 (F6)
	var pgerr *pgconn.PgError                                                                                  // LC-S6C1 fix 2 (F6)
	if !errors.As(err, &pgerr) || pgerr.Code != "P0001" || pgerr.Message != ErrSyncWriterNotAdmitted.Error() { // LC-S6C1 fix 2 (F6)
		t.Fatal("F6 SQL guard refusal contract failed") // LC-S6C1 fix 2 (F6)
	} // LC-S6C1 fix 2 (F6)
} // LC-S6C1 fix 2 (F6)
func captureRefusedAPI(t *testing.T, err error) { // LC-S6C1 fix 2 (F6)
	t.Helper()                                                               // LC-S6C1 fix 2 (F6)
	var pgerr *pgconn.PgError                                                // LC-S6C1 fix 2 (F6)
	if !errors.Is(err, ErrSyncWriterNotAdmitted) || errors.As(err, &pgerr) { // LC-S6C1 fix 2 (F6)
		t.Fatal("F6 API guard refusal contract failed") // LC-S6C1 fix 2 (F6)
	} // LC-S6C1 fix 2 (F6)
} // LC-S6C1 fix 2 (F6)
func captureRefusedCoverage(t *testing.T, err error) { // LC-S6C1 fix 2 (F6)
	t.Helper()                // LC-S6C1 fix 2 (F6)
	var pgerr *pgconn.PgError // LC-S6C1 fix 2 (F6)
	// All five raises in witself_sync_check_coverage use this one fixed message. // LC-S6C1 fix 2 (F6)
	if !errors.As(err, &pgerr) || pgerr.Code != "P0001" || pgerr.Message != "sync writer not admitted" { // LC-S6C1 fix 2 (F6)
		t.Fatal("F6 coverage refusal contract failed") // LC-S6C1 fix 2 (F6)
	} // LC-S6C1 fix 2 (F6)
} // LC-S6C1 fix 2 (F6)
func captureRefusedSnapshot(t *testing.T, err error) { // LC-S6C1 fix 2 (F6)
	t.Helper() // LC-S6C1 fix 2 (F6)
	// Preserve the existing serialization, missing-control sentinel, and exact raw fence alternatives. // LC-S6C1 fix 2 (F6)
	if syncTestSQLState(err) == "40001" || errors.Is(err, ErrSyncWriterNotAdmitted) { // LC-S6C1 fix 2 (F6)
		return // LC-S6C1 fix 2 (F6)
	} // LC-S6C1 fix 2 (F6)
	var pgerr *pgconn.PgError                                                                                  // LC-S6C1 fix 2 (F6)
	if !errors.As(err, &pgerr) || pgerr.Code != "P0001" || pgerr.Message != ErrSyncWriterNotAdmitted.Error() { // LC-S6C1 fix 2 (F6)
		t.Fatal("F6 snapshot refusal contract failed") // LC-S6C1 fix 2 (F6)
	} // LC-S6C1 fix 2 (F6)
} // LC-S6C1 fix 2 (F6)
func TestSyncCaptureError(t *testing.T) { // LC-S6C1 fix 2 (F6)
	t.Run("exact_refusal", func(t *testing.T) { // LC-S6C1 fix 2 (F6)
		err := &pgconn.PgError{Code: "P0001", Message: ErrSyncWriterNotAdmitted.Error()} // LC-S6C1 fix 2 (F6)
		if syncCaptureError(err) != ErrSyncWriterNotAdmitted {                           // LC-S6C1 fix 2 (F6)
			t.Fatal("F6 map exact did not return bare sentinel") // LC-S6C1 fix 2 (F6)
		} // LC-S6C1 fix 2 (F6)
	}) // LC-S6C1 fix 2 (F6)
	t.Run("different_message", func(t *testing.T) { // LC-S6C1 fix 2 (F6)
		err := &pgconn.PgError{Code: "P0001", Message: "synthetic other refusal"}                 // LC-S6C1 fix 2 (F6)
		if got := syncCaptureError(err); got != err || errors.Is(got, ErrSyncWriterNotAdmitted) { // LC-S6C1 fix 2 (F6)
			t.Fatal("F6 map other message was normalized") // LC-S6C1 fix 2 (F6)
		} // LC-S6C1 fix 2 (F6)
	}) // LC-S6C1 fix 2 (F6)
	t.Run("different_sqlstate", func(t *testing.T) { // LC-S6C1 fix 2 (F6)
		err := &pgconn.PgError{Code: "23514", Message: ErrSyncWriterNotAdmitted.Error()} // LC-S6C1 fix 2 (F6)
		if syncCaptureError(err) != err {                                                // LC-S6C1 fix 2 (F6)
			t.Fatal("F6 map state changed the original error") // LC-S6C1 fix 2 (F6)
		} // LC-S6C1 fix 2 (F6)
	}) // LC-S6C1 fix 2 (F6)
	t.Run("non_pgerror", func(t *testing.T) { // LC-S6C1 fix 2 (F6)
		err := errors.New("synthetic non PostgreSQL error") // LC-S6C1 fix 2 (F6)
		if syncCaptureError(err) != err {                   // LC-S6C1 fix 2 (F6)
			t.Fatal("F6 map plain changed the original error") // LC-S6C1 fix 2 (F6)
		} // LC-S6C1 fix 2 (F6)
	}) // LC-S6C1 fix 2 (F6)
} // LC-S6C1 fix 2 (F6)

func TestSyncCaptureInstallationDrain(t *testing.T) {
	_ = testenv.RequirePostgres(t) // Skip the whole adapter before any subcase or evidence record.
	completed := 0
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "commit", false: "rollback"}[commit], func(t *testing.T) {
			ctx := context.Background()
			st, dsn := newSyncSchemaTestStore(t)
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
			tx, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal("capture drain begin failed")
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err = tx.Exec(ctx, `INSERT INTO accounts(id,display_name,is_default) VALUES('drain_account','drain',false)`); err != nil {
				t.Fatal("capture drain write failed")
			}
			var pid uint32
			if tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid) != nil {
				t.Fatal("capture drain backend failed")
			}
			result := make(chan error, 1)
			go func() { result <- st.InstallSyncGuards(ctx) }()
			capturePoll(t, st, `SELECT EXISTS(SELECT 1 FROM pg_locks w JOIN pg_locks h ON h.relation=w.relation WHERE w.relation='accounts'::regclass AND w.mode='AccessExclusiveLock' AND NOT w.granted AND h.granted AND h.pid=$1 AND h.mode='RowExclusiveLock')`, pid)
			t.Log("capture observed installation relation wait on accounts")
			if commit {
				err = tx.Commit(ctx)
			} else {
				err = tx.Rollback(ctx)
			}
			if err != nil {
				t.Fatal("capture drain release failed")
			}
			select {
			case err = <-result:
				if err != nil {
					t.Fatal("capture drain install failed")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("capture drain completion timed out")
			}
			if validateInstalledSyncWriterInventory(ctx, st) != nil {
				t.Fatal("capture drain installed guard inventory failed")
			}
			var found bool
			if st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id='drain_account')`).Scan(&found) != nil || found != commit {
				t.Fatal("capture drain visibility differs")
			}
			var prepared int
			if st.pool.QueryRow(ctx, `SELECT current_setting('max_prepared_transactions')::integer`).Scan(&prepared) != nil {
				t.Fatal("capture prepared setting failed")
			}
			if prepared == 0 {
				t.Log("NOT RUN prepared_transactions: max_prepared_transactions=0")
			} else {
				t.Fatal("capture prepared case needs isolated owner execution")
			}
			completed++
		})
	}
	captureEvidence(t, completed, 2)
}

func TestSyncCaptureLateFirstWrite(t *testing.T) {
	_ = testenv.RequirePostgres(t) // Skip the whole adapter before any subcase or evidence record.
	ctx := context.Background()
	st, _ := newSyncSchemaTestStore(t)
	if _, err := st.pool.Exec(ctx, `INSERT INTO accounts(id,display_name,is_default) VALUES('late_account','late',false)`); err != nil {
		t.Fatal("capture late seed failed")
	}
	old, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal("capture late begin failed")
	}
	defer func() { _ = old.Rollback(ctx) }()
	if st.MigrateSync(ctx) != nil || st.SelectSyncCapture(runtimeconfig.Config{Local: true, SyncSchema: true}) != nil {
		t.Fatal("capture late installation failed")
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO sync_incarnations(account_id,lifecycle_epoch,restore_marker,allocation_id,transition_id,authority_installation,state) VALUES('late_account',1,decode(repeat('01',16),'hex'),decode(repeat('02',16),'hex'),decode(repeat('03',16),'hex'),'local','installed')`); err != nil {
		t.Fatal("capture late incarnation failed")
	}
	if st.EnableSyncWriter(ctx, "late_account", SyncPreferencesPrincipal, SyncPreferencesCapability) != nil {
		t.Fatal("capture late capability failed")
	}
	activateSyncFixture(t, st, "late_account")
	_, err = old.Exec(ctx, `UPDATE accounts SET display_name='refused' WHERE id='late_account'`)
	captureRefusedSQL(t, err) // LC-S6C1 fix 2 (F6)
	captureEvidence(t, 1, 1)
}

func TestSyncCaptureStaleSnapshots(t *testing.T) {
	_ = testenv.RequirePostgres(t) // Skip the whole adapter before any subcase or evidence record.
	completed := 0
	for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		for _, missing := range []bool{false, true} {
			t.Run(string(iso)+map[bool]string{true: "_missing", false: "_old"}[missing], func(t *testing.T) {
				st, p := newSyncCaptureFixture(t, !missing)
				ctx := context.Background()
				v, err := st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
				if err != nil {
					t.Fatal("capture stale begin failed")
				}
				defer func() { _ = v.Rollback(ctx) }()
				var count int
				if v.QueryRow(ctx, `SELECT count(*) FROM sync_capture WHERE account_id=$1`, p.AccountID).Scan(&count) != nil {
					t.Fatal("capture stale snapshot failed")
				}
				activateSyncFixture(t, st, p.AccountID)
				_, err = admitSyncTx(ctx, v, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability)
				captureRefusedSnapshot(t, err) // LC-S6C1 fix 2 (F6)
				t.Logf("capture stale snapshot refusal SQLSTATE=%s", syncTestSQLState(err))
				completed++
			})
		}
	}
	captureEvidence(t, completed, 4)
}

func TestSyncCaptureAccountGate(t *testing.T) {
	_ = testenv.RequirePostgres(t) // Skip the whole adapter before any subcase or evidence record.
	completed := 0
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "commit", false: "rollback"}[commit], func(t *testing.T) {
			st, p := newSyncCaptureFixture(t, false)
			ctx := context.Background()
			tx, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal("capture gate begin failed")
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(capture_lock_key($1))`, p.AccountID); err != nil {
				t.Fatal("capture gate shared failed")
			}
			observer, err := st.pool.Acquire(ctx)
			if err != nil {
				t.Fatal("capture gate observer failed")
			}
			defer observer.Release()
			var locked bool
			if observer.QueryRow(ctx, `SELECT pg_try_advisory_lock(capture_lock_key($1))`, p.AccountID).Scan(&locked) != nil || locked {
				t.Fatal("capture gate failed to drain writer")
			}
			capturePoll(t, st, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND mode='ShareLock' AND granted AND pid=$1)`, tx.Conn().PgConn().PID())
			type activationResult struct {
				owner *SyncCaptureActivation
				err   error
			}
			started := make(chan activationResult, 1)                                   // LC-S6C1 fix 1 (F7)
			activationCtx, cancelActivation := context.WithTimeout(ctx, 15*time.Second) // LC-S6C1 fix 1 (F7)
			var result activationResult                                                 // LC-S6C1 fix 1 (F7)
			received := false                                                           // LC-S6C1 fix 1 (F7)
			// Register before observation so Fatal also drains the goroutine and releases its pinned connection. // LC-S6C1 fix 1 (F7)
			t.Cleanup(func() { // LC-S6C1 fix 1 (F7)
				cleanupStart := time.Now() // LC-S6C1 fix 1 (F7)
				cancelActivation()         // LC-S6C1 fix 1 (F7)
				if !received {             // LC-S6C1 fix 1 (F7)
					result = <-started // LC-S6C1 fix 1 (F7)
				} // LC-S6C1 fix 1 (F7)
				if result.owner != nil { // LC-S6C1 fix 1 (F7)
					result.owner.Close() // LC-S6C1 fix 1 (F7)
				} // LC-S6C1 fix 1 (F7)
				if time.Since(cleanupStart) > 5*time.Second { // LC-S6C1 fix 1 (F7)
					t.Error("f7cleanupbound activation cleanup exceeded five seconds") // LC-S6C1 fix 1 (F7)
				} // LC-S6C1 fix 1 (F7)
				t.Log("f7cleanupdone activation canceled result drained owner released") // LC-S6C1 fix 1 (F7)
			}) // LC-S6C1 fix 1 (F7)
			go func() { // LC-S6C1 fix 1 (F7)
				a, err := st.ActivateSyncCapture(activationCtx, p.AccountID, SyncCaptureOptions{LifecycleEpoch: 1, RestoreMarker: bytes.Repeat([]byte{1}, 16), Capability: SyncPreferencesCapability}) // LC-S6C1 fix 1 (F7)
				started <- activationResult{a, err}                                                                                                                                                    // LC-S6C1 fix 1 (F7)
			}() // LC-S6C1 fix 1 (F7)
			capturePoll(t, st, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>$1 AND query='SELECT pg_try_advisory_lock(capture_lock_key($1))')`, observer.Conn().PgConn().PID())
			if commit {
				err = tx.Commit(ctx)
			} else {
				err = tx.Rollback(ctx)
			}
			if err != nil {
				t.Fatal("capture gate release failed")
			}
			select {
			case result = <-started:
				received = true // LC-S6C1 fix 1 (F7)
			case <-time.After(10 * time.Second):
				t.Fatal("capture production activation timed out")
			}
			if result.err != nil || result.owner == nil {
				t.Fatal("capture production activation failed")
			}
			capturePoll(t, st, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND mode='ExclusiveLock' AND granted)`, result.owner.pid)
			pin, err := result.owner.PinSnapshot(ctx)
			if err != nil {
				t.Fatal("capture gate snapshot failed")
			}
			defer func() { _ = pin.Close(ctx) }()
			if result.owner.Acknowledge(ctx, pin) != nil {
				t.Fatal("capture gate acknowledgment failed")
			}
			t.Log("capture observed shared gate, try-lock false then true")
			completed++
		})
	}
	captureEvidence(t, completed, 2)
}

func TestSyncCaptureSnapshotSession(t *testing.T) {
	_ = testenv.RequirePostgres(t) // Skip the whole adapter before any subcase or evidence record.
	completed := 0
	for _, cut := range []string{"ack", "gate_death", "gate_reconnect"} {
		t.Run(cut, func(t *testing.T) {
			st, p := newSyncCaptureFixture(t, false)
			ctx := context.Background()
			a, err := st.ActivateSyncCapture(ctx, p.AccountID, SyncCaptureOptions{LifecycleEpoch: 1, RestoreMarker: bytes.Repeat([]byte{1}, 16), Capability: SyncPreferencesCapability})
			if err != nil {
				t.Fatal("capture session activation failed")
			}
			defer a.Close()
			if cut != "ack" {
				var dead bool
				if st.pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, a.pid).Scan(&dead) != nil || !dead {
					t.Fatal("capture session death failed")
				}
				capturePoll(t, st, `SELECT NOT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND granted)`, a.pid)
				if _, err := st.PutDashboardPreferences(ctx, p, []byte(`{"schema":"witself.dashboard-prefs.v1","theme":"amber"}`)); err != nil {
					t.Fatal("capture session postdeath writer failed")
				}
			}
			pin, err := a.PinSnapshot(ctx)
			if err != nil {
				t.Fatal("capture session candidate failed")
			}
			defer func() { _ = pin.Close(ctx) }()
			if cut == "gate_reconnect" {
				replacement, err := st.pool.Acquire(ctx)
				if err != nil {
					t.Fatal("capture session reconnect failed")
				}
				defer replacement.Release()
				var acquired bool
				if replacement.QueryRow(ctx, `SELECT pg_try_advisory_lock(capture_lock_key($1))`, p.AccountID).Scan(&acquired) != nil || !acquired {
					t.Fatal("capture reconnect acquisition failed")
				}
				defer func() { _, _ = replacement.Exec(ctx, `SELECT pg_advisory_unlock(capture_lock_key($1))`, p.AccountID) }()
				if replacement.Conn().PgConn().PID() == a.pid {
					t.Fatal("capture reconnect reused dead backend")
				}
			}
			err = a.Acknowledge(ctx, pin)
			if cut == "ack" {
				if err != nil || !pin.Acknowledged() {
					t.Fatal("capture same session acknowledgment failed")
				}
			} else {
				captureRefusedAPI(t, err) // LC-S6C1 fix 2 (F6)
				if pin.Acknowledged() {
					t.Fatal("capture dead session candidate accepted")
				}
				t.Log("capture observed gate_death and ack_refuse")
			}
			completed++
		})
	}
	captureEvidence(t, completed, 3)
}

func TestSyncCaptureCommittedPrefix(t *testing.T) {
	_ = testenv.RequirePostgres(t) // Skip the whole adapter before any subcase or evidence record.
	completed := 0
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "commit", false: "rollback"}[commit], func(t *testing.T) {
			st, p := newSyncCaptureFixture(t, true)
			ctx := context.Background()
			begin := func(agent string) *SyncTx {
				tx, err := st.AdmitSyncTx(ctx, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability)
				if err != nil {
					t.Fatal("capture prefix admission failed")
				}
				if _, err = tx.Tx().Exec(ctx, `INSERT INTO agent_dashboard_preferences(agent_id,account_id,realm_id,prefs) VALUES($1,$2,$3,'{"schema":"witself.dashboard-prefs.v1","theme":"amber"}')`, agent, p.AccountID, p.RealmID); err != nil {
					t.Fatalf("capture prefix domain SQLSTATE %s", syncTestSQLState(err))
				}
				if tx.Touch(ctx, SyncPreferencesCapability, "preferences", []byte(`{"schema":"witself.dashboard-prefs.v1","theme":"amber"}`), []byte(`[]`)) != nil {
					t.Fatal("capture prefix touch failed")
				}
				return tx
			}
			one := begin(p.ID)
			defer func() { _ = one.Rollback(ctx) }()
			if one.Seal(ctx) != nil {
				t.Fatal("capture prefix first seal failed")
			}
			two := begin("capture_second")
			defer func() { _ = two.Rollback(ctx) }()
			result := make(chan error, 1)
			go func() { result <- two.Seal(ctx) }()
			capturePoll(t, st, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted AND locktype='transactionid') AND EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND query LIKE '%sync_finalized_counter%')`, two.Tx().Conn().PgConn().PID())
			var prefix, count int
			if st.pool.QueryRow(ctx, `SELECT last_finalized,(SELECT count(*) FROM sync_bundles WHERE account_id=$1) FROM sync_finalized_counter WHERE account_id=$1 AND capture_epoch=1`, p.AccountID).Scan(&prefix, &count) != nil || prefix != 0 || count != 0 {
				t.Fatal("capture prefix published uncommitted gap")
			}
			var err error
			if commit {
				err = one.Commit(ctx)
			} else {
				err = one.Rollback(ctx)
			}
			if err != nil {
				t.Fatal("capture prefix first completion failed")
			}
			select {
			case err = <-result:
				if err != nil {
					t.Fatal("capture prefix second seal failed")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("capture prefix second seal timed out")
			}
			if two.Commit(ctx) != nil {
				t.Fatal("capture prefix second commit failed")
			}
			want := 1
			if commit {
				want = 2
			}
			if st.pool.QueryRow(ctx, `SELECT last_finalized,(SELECT count(*) FROM sync_bundles WHERE account_id=$1) FROM sync_finalized_counter WHERE account_id=$1 AND capture_epoch=1`, p.AccountID).Scan(&prefix, &count) != nil || prefix != want || count != want {
				t.Fatal("capture prefix final range has gap")
			}
			t.Log("capture observed counter transaction lock and contiguous committed prefix")
			completed++
		})
	}
	source, err := os.ReadFile("sync_capture_tx.go")
	if err != nil {
		t.Fatal("capture prefix source read failed")
	}
	for _, forbidden := range []string{"nextval(", "currval(", "setval(", "max(finalized)"} {
		if bytes.Contains(bytes.ToLower(source), []byte(forbidden)) {
			t.Fatal("capture prefix sequence maximum forbidden")
		}
	}
	captureEvidence(t, completed, 2)
}

func TestSyncCaptureRawMissingControlSnapshots(t *testing.T) {
	for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		t.Run(string(iso), func(t *testing.T) {
			st, p := newSyncCaptureFixture(t, false)
			ctx := context.Background()
			var before, after []byte
			if st.pool.QueryRow(ctx, `SELECT to_jsonb(a) FROM accounts a WHERE id=$1`, p.AccountID).Scan(&before) != nil {
				t.Fatal("capture raw account bytes failed")
			}
			old, err := st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
			if err != nil {
				t.Fatal("capture raw snapshot begin failed")
			}
			defer func() { _ = old.Rollback(ctx) }()
			var count int
			if old.QueryRow(ctx, `SELECT count(*) FROM sync_capture WHERE account_id=$1`, p.AccountID).Scan(&count) != nil || count != 0 {
				t.Fatal("capture raw missing snapshot failed")
			}
			activateSyncFixture(t, st, p.AccountID)
			_, err = old.Exec(ctx, `INSERT INTO agent_dashboard_preferences(agent_id,account_id,realm_id,prefs) VALUES($1,$2,$3,'{"schema":"witself.dashboard-prefs.v1","theme":"amber"}')`, p.ID, p.AccountID, p.RealmID)
			captureRefusedSnapshot(t, err) // LC-S6C1 fix 2 (F6)
			if st.pool.QueryRow(ctx, `SELECT to_jsonb(a) FROM accounts a WHERE id=$1`, p.AccountID).Scan(&after) != nil || !bytes.Equal(before, after) {
				t.Fatal("capture raw activation changed account bytes")
			}
			t.Log("capture raw missing-row snapshot refused with account tuple fence")
		})
	}
} // LC-S6C1 fix 1 (F1)
// LC-S6C1 fix 1 (F1)
func TestSyncCaptureAdmissionEpochGuard(t *testing.T) { // LC-S6C1 fix 1 (F1)
	_ = testenv.RequirePostgres(t) // LC-S6C1 fix 1 (F1)
	for _, tc := range []struct {  // LC-S6C1 fix 1 (F1)
		name string // LC-S6C1 fix 1 (F1)
		sql  string // LC-S6C1 fix 1 (F1)
	}{ // LC-S6C1 fix 1 (F1)
		{"capture_epoch", `UPDATE sync_tx_admissions SET capture_epoch=capture_epoch+1 WHERE account_id=$1 AND tx_id=$2`},                 // LC-S6C1 fix 1 (F1)
		{"lifecycle_epoch", `UPDATE sync_tx_admissions SET lifecycle_epoch=lifecycle_epoch+1 WHERE account_id=$1 AND tx_id=$2`},           // LC-S6C1 fix 1 (F1)
		{"restore_marker", `UPDATE sync_tx_admissions SET restore_marker=decode(repeat('04',16),'hex') WHERE account_id=$1 AND tx_id=$2`}, // LC-S6C1 fix 1 (F1)
	} { // LC-S6C1 fix 1 (F1)
		t.Run(tc.name, func(t *testing.T) { // LC-S6C1 fix 1 (F1)
			st, p := newSyncCaptureFixture(t, true)                                                                // LC-S6C1 fix 1 (F1)
			ctx := context.Background()                                                                            // LC-S6C1 fix 1 (F1)
			admitted, err := st.AdmitSyncTx(ctx, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability) // LC-S6C1 fix 1 (F1)
			if err != nil {                                                                                        // LC-S6C1 fix 1 (F1)
				t.Fatal("f1admitbegin failed") // LC-S6C1 fix 1 (F1)
			} // LC-S6C1 fix 1 (F1)
			defer func() { _ = admitted.Rollback(ctx) }()                             // LC-S6C1 fix 1 (F1)
			changed, err := admitted.Tx().Exec(ctx, tc.sql, p.AccountID, admitted.id) // LC-S6C1 fix 1 (F1)
			if err != nil || changed.RowsAffected() != 1 {                            // LC-S6C1 fix 1 (F1)
				t.Fatal("f1admittamper failed") // LC-S6C1 fix 1 (F1)
			} // LC-S6C1 fix 1 (F1)
			// No Seal or commit occurs so only the immediate row guard can refuse this DML. // LC-S6C1 fix 1 (F1)
			_, err = admitted.Tx().Exec(ctx, `UPDATE accounts SET display_name='epoch refused' WHERE id=$1`, p.AccountID) // LC-S6C1 fix 1 (F1)
			var pgerr *pgconn.PgError                                                                                     // LC-S6C1 fix 1 (F1)
			if !errors.As(err, &pgerr) || pgerr.Code != "P0001" || pgerr.Message != ErrSyncWriterNotAdmitted.Error() {    // LC-S6C1 fix 1 (F1)
				t.Fatal("f1guardexact refusal missing before commit") // LC-S6C1 fix 1 (F1)
			} // LC-S6C1 fix 1 (F1)
		}) // LC-S6C1 fix 1 (F1)
	} // LC-S6C1 fix 1 (F1)
} // LC-S6C1 fix 1 (F1)
func TestSyncCaptureAdmissionDisabledCapability(t *testing.T) { // LC-S6C1 fix 1 (F2)
	_ = testenv.RequirePostgres(t) // LC-S6C1 fix 1 (F2)
	for _, tc := range []struct {  // LC-S6C1 fix 1 (F2)
		name    string // LC-S6C1 fix 1 (F2)
		revoked int64  // LC-S6C1 fix 1 (F2)
	}{ // LC-S6C1 fix 1 (F2)
		{"disabled", 0}, // LC-S6C1 fix 1 (F2)
		{"revoked", 1},  // LC-S6C1 fix 1 (F2)
	} { // LC-S6C1 fix 1 (F2)
		t.Run(tc.name, func(t *testing.T) { // LC-S6C1 fix 1 (F2)
			st, p := newSyncCaptureFixture(t, true) // LC-S6C1 fix 1 (F2)
			ctx := context.Background()             // LC-S6C1 fix 1 (F2)
			// A revoked capability must also be disabled to satisfy its enabled-check constraint. // LC-S6C1 fix 1 (F2)
			changed, err := st.pool.Exec(ctx, `UPDATE sync_writer_capabilities SET enabled=false,revoked_generation=$4 WHERE account_id=$1 AND principal=$2 AND capability=$3`, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability, tc.revoked) // LC-S6C1 fix 1 (F2)
			if err != nil || changed.RowsAffected() != 1 {                                                                                                                                                                                                    // LC-S6C1 fix 1 (F2)
				t.Fatal("f2capdisable failed") // LC-S6C1 fix 1 (F2)
			} // LC-S6C1 fix 1 (F2)
			admitted, err := st.AdmitSyncTx(ctx, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability) // LC-S6C1 fix 1 (F2)
			if admitted != nil {                                                                                   // LC-S6C1 fix 1 (F2)
				_ = admitted.Rollback(ctx)                        // LC-S6C1 fix 1 (F2)
				t.Fatal("f2nilhandlecheck returned an admission") // LC-S6C1 fix 1 (F2)
			} // LC-S6C1 fix 1 (F2)
			if err != ErrSyncWriterNotAdmitted { // LC-S6C1 fix 1 (F2)
				t.Fatal("f2exacterrorcheck refusal missing") // LC-S6C1 fix 1 (F2)
			} // LC-S6C1 fix 1 (F2)
			var durable int                                                                                                              // LC-S6C1 fix 1 (F2)
			if st.pool.QueryRow(ctx, `SELECT count(*) FROM sync_tx_admissions WHERE account_id=$1`, p.AccountID).Scan(&durable) != nil { // LC-S6C1 fix 1 (F2)
				t.Fatal("f2durableread failed") // LC-S6C1 fix 1 (F2)
			} // LC-S6C1 fix 1 (F2)
			if durable != 0 { // LC-S6C1 fix 1 (F2)
				t.Fatal("f2durablezerocheck admission remained") // LC-S6C1 fix 1 (F2)
			} // LC-S6C1 fix 1 (F2)
		}) // LC-S6C1 fix 1 (F2)
	} // LC-S6C1 fix 1 (F2)
} // LC-S6C1 fix 1 (F2)
func TestSyncCaptureAdmissionStaleControlOnly(t *testing.T) { // LC-S6C1 fix 1 (F3)
	_ = testenv.RequirePostgres(t)                                               // LC-S6C1 fix 1 (F3)
	for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} { // LC-S6C1 fix 1 (F3)
		t.Run(string(iso), func(t *testing.T) { // LC-S6C1 fix 1 (F3)
			st, p := newSyncCaptureFixture(t, true)                       // LC-S6C1 fix 1 (F3)
			ctx := context.Background()                                   // LC-S6C1 fix 1 (F3)
			tx, err := st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso}) // LC-S6C1 fix 1 (F3)
			if err != nil {                                               // LC-S6C1 fix 1 (F3)
				t.Fatal("f3snapshotbegin failed") // LC-S6C1 fix 1 (F3)
			} // LC-S6C1 fix 1 (F3)
			defer func() { _ = tx.Rollback(ctx) }()                                                                                                                                 // LC-S6C1 fix 1 (F3)
			var pinnedMinimum int                                                                                                                                                   // LC-S6C1 fix 1 (F3)
			if tx.QueryRow(ctx, `SELECT min_writer_schema FROM sync_capture WHERE account_id=$1`, p.AccountID).Scan(&pinnedMinimum) != nil || pinnedMinimum > SyncSchemaVersion() { // LC-S6C1 fix 1 (F3)
				t.Fatal("f3snapshotpin failed") // LC-S6C1 fix 1 (F3)
			} // LC-S6C1 fix 1 (F3)
			// Change only the control tuple so the unchanged capability FOR SHARE cannot mask its missing lock. // LC-S6C1 fix 1 (F3)
			changed, err := st.pool.Exec(ctx, `UPDATE sync_capture SET min_writer_schema=$2 WHERE account_id=$1`, p.AccountID, SyncSchemaVersion()+1) // LC-S6C1 fix 1 (F3)
			if err != nil || changed.RowsAffected() != 1 {                                                                                            // LC-S6C1 fix 1 (F3)
				t.Fatal("f3controlupdate failed") // LC-S6C1 fix 1 (F3)
			} // LC-S6C1 fix 1 (F3)
			admitted, err := admitSyncTx(ctx, tx, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability) // LC-S6C1 fix 1 (F3)
			if admitted != nil {                                                                                    // LC-S6C1 fix 1 (F3)
				t.Fatal("f3stalehandlecheck returned an admission") // LC-S6C1 fix 1 (F3)
			} // LC-S6C1 fix 1 (F3)
			if syncTestSQLState(err) != "40001" && err != ErrSyncWriterNotAdmitted { // LC-S6C1 fix 1 (F3)
				t.Fatal("f3stalerefusalcheck missing") // LC-S6C1 fix 1 (F3)
			} // LC-S6C1 fix 1 (F3)
		}) // LC-S6C1 fix 1 (F3)
	} // LC-S6C1 fix 1 (F3)
} // LC-S6C1 fix 1 (F3)
