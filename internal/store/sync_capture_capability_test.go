package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSyncCaptureCapabilityEnableFencesOldSnapshots(t *testing.T) {
	for _, isolation := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		for _, capabilityState := range []string{"disabled", "missing"} {
			t.Run(string(isolation)+"/"+capabilityState, func(t *testing.T) {
				st, principal := newSyncCaptureFixture(t, true)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if _, err := st.PutDashboardPreferences(ctx, principal, syncPreferenceDocument("amber")); err != nil {
					t.Fatal("capture capability baseline preference failed")
				}
				change := `UPDATE sync_writer_capabilities SET enabled=false WHERE account_id=$1`
				if capabilityState == "missing" {
					change = `DELETE FROM sync_writer_capabilities WHERE account_id=$1`
				}
				if _, err := st.pool.Exec(ctx, change, principal.AccountID); err != nil {
					t.Fatal("capture capability inert fixture failed")
				}
				// A snapshot that inherits an older running XID from any database on the
				// server is a horizon the enable below refuses while it stays open.
				old := beginSyncCaptureSnapshot(t, st, principal.AccountID, isolation)
				defer func() { _ = old.Rollback(ctx) }()
				var enabled bool
				if old.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_writer_capabilities w
					WHERE w.account_id=c.account_id AND w.enabled)
					FROM sync_capture c WHERE c.account_id=$1`, principal.AccountID).Scan(&enabled) != nil || enabled {
					t.Fatal("capture capability old snapshot was not inert")
				}
				// No new activation or epoch change follows this explicit enable.
				if err := st.EnableSyncWriter(ctx, principal.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability); err != nil {
					own := logSyncCaptureActivityTypes(t, st, syncCaptureActivityScope{account: principal.AccountID, expectedPID: old.Conn().PgConn().PID()})
					t.Fatalf("capture capability explicit reenable failed class %s", syncCaptureEnableFailureClass(err, own))
				}
				_, err := old.Exec(ctx, `UPDATE agent_dashboard_preferences SET prefs=$2::jsonb WHERE agent_id=$1`,
					principal.ID, syncPreferenceDocument("console"))
				if syncTestSQLState(err) != "40001" && !errors.Is(syncCaptureError(err), ErrSyncWriterNotAdmitted) {
					t.Fatalf("capture capability old writer did not refuse SQLSTATE: %s", syncTestSQLState(err))
				}
				_ = old.Rollback(ctx)
				var theme string
				if st.pool.QueryRow(ctx, `SELECT prefs->>'theme' FROM agent_dashboard_preferences WHERE agent_id=$1`, principal.ID).Scan(&theme) != nil || theme != "amber" {
					t.Fatal("capture capability old writer changed domain state")
				}
				t.Log("capture observed stale inert capability snapshot refusal after enable without activation")
			})
		}
	}
}

// EnableSyncWriter reports every refusal as ErrSyncWriterNotAdmitted with no
// SQLSTATE. Name the cause in a fixed word: a wrapped SQLSTATE's class, the
// row's own snapshot when it matches the production horizon, else not admitted.
func syncCaptureEnableFailureClass(err error, ownHorizon bool) string {
	state := syncTestSQLState(err)
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return "context_ended"
	case len(state) == 5:
		if class, ok := map[string]string{"08": "connection_exception", "40": "transaction_rollback", "53": "insufficient_resources",
			"55": "object_not_in_prerequisite_state", "57": "operator_intervention"}[state[:2]]; ok {
			return class
		}
		return "other_sqlstate"
	case !errors.Is(err, ErrSyncWriterNotAdmitted):
		return "unclassified"
	case ownHorizon:
		return "own_snapshot_horizon"
	}
	return "not_admitted"
}

func TestSyncCaptureEnableFailureClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		own  bool
		want string
	}{
		{ErrSyncWriterNotAdmitted, true, "own_snapshot_horizon"},
		{ErrSyncWriterNotAdmitted, false, "not_admitted"},
		{fmt.Errorf("%w %w", ErrSyncWriterNotAdmitted, &pgconn.PgError{Code: "53300"}), true, "insufficient_resources"},
		{&pgconn.PgError{Code: "XX000"}, false, "other_sqlstate"},
		{context.DeadlineExceeded, true, "context_ended"},
		{errors.New("unrelated failure"), true, "unclassified"},
	} {
		if got := syncCaptureEnableFailureClass(tc.err, tc.own); got != tc.want {
			t.Fatalf("capture capability enable class differs got %s want %s", got, tc.want)
		}
	}
}
