package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
				old, err := st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: isolation})
				if err != nil {
					t.Fatal("capture capability old transaction failed")
				}
				defer func() { _ = old.Rollback(ctx) }()
				var enabled bool
				if old.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_writer_capabilities w
					WHERE w.account_id=c.account_id AND w.enabled)
					FROM sync_capture c WHERE c.account_id=$1`, principal.AccountID).Scan(&enabled) != nil || enabled {
					t.Fatal("capture capability old snapshot was not inert")
				}
				// No new activation or epoch change follows this explicit enable.
				if st.EnableSyncWriter(ctx, principal.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability) != nil {
					t.Fatal("capture capability explicit reenable failed")
				}
				_, err = old.Exec(ctx, `UPDATE agent_dashboard_preferences SET prefs=$2::jsonb WHERE agent_id=$1`,
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
