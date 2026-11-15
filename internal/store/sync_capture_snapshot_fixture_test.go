package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func syncCaptureFixtureFailure(failure string, expectedHorizon bool) string {
	if expectedHorizon && failure == "capture fixture foreign snapshot horizon persisted" {
		return "capture fixture expected test transaction snapshot horizon persisted"
	}
	return failure
}

// A preactivation snapshot must postdate the account tuple's horizon. An idle
// read-committed writer can lend its older XID to a new snapshot even though it
// holds no backend_xmin itself. Release unsuitable candidates before retrying.
func beginSyncCaptureSnapshot(t *testing.T, st *Store, account string, iso pgx.TxIsoLevel, onUnsafe ...func()) pgx.Tx {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), syncCaptureFixtureRetryBudget(0))
	defer cancel()
	var tx pgx.Tx
	captureObserve(ctx, t, func(ctx context.Context) (bool, error) {
		var err error
		tx, err = st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
		if err != nil {
			return false, err
		}
		var safe bool
		err = tx.QueryRow(ctx, `SELECT age(pg_snapshot_xmin(pg_current_snapshot())::xid)<age(xmin)
 FROM accounts WHERE id=$1`, account).Scan(&safe)
		if safe && err == nil {
			return true, nil
		}
		if rollbackErr := tx.Rollback(context.Background()); rollbackErr != nil {
			return false, rollbackErr
		}
		if err == nil {
			for _, observe := range onUnsafe {
				observe()
			}
		}
		return false, err
	})
	return tx
}

func TestSyncCaptureInheritedHorizonSnapshot(t *testing.T) {
	ctx := context.Background()
	var older pgx.Tx
	st, p, work := seedSyncCaptureFixture(t, func(st *Store) {
		var err error
		older, err = st.pool.Begin(ctx)
		if err != nil {
			t.Fatal("inherited horizon older begin failed")
		}
		t.Cleanup(func() { _ = older.Rollback(context.Background()) })
		if _, err = older.Exec(ctx, `SELECT pg_current_xact_id()`); err != nil {
			t.Fatal("inherited horizon assign failed")
		}
	})
	if _, failure := retrySyncCaptureFixtureAdmission(ctx, st, p.AccountID, false, work, t.Logf); failure != "" {
		t.Fatal(failure)
	}
	activateSyncFixture(t, st, p.AccountID)
	rejected := false
	v := beginSyncCaptureSnapshot(t, st, p.AccountID, pgx.RepeatableRead, func() {
		if !rejected && older.Rollback(ctx) != nil {
			t.Fatal("inherited horizon older release failed")
		}
		rejected = true
	})
	defer func() { _ = v.Rollback(ctx) }()
	// End the original writer even if no candidate was rejected.
	_ = older.Rollback(ctx)
	var count int
	if v.QueryRow(ctx, `SELECT count(*) FROM sync_capture WHERE account_id=$1`, p.AccountID).Scan(&count) != nil || count != 1 {
		t.Fatal("inherited horizon snapshot failed")
	}
	activateSyncFixture(t, st, p.AccountID, syncFixtureActivation{expectedPID: v.Conn().PgConn().PID()})
	_, err := admitSyncTx(ctx, v, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability)
	captureRefusedSnapshot(t, err)
	if !rejected {
		t.Fatal("inherited horizon unsafe candidate was not released")
	}
}
