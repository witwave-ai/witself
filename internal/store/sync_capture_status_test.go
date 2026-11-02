package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

func TestSyncCaptureAdministrativeCallsRequireSelection(t *testing.T) {
	st := &Store{}
	ctx := context.Background()
	if err := st.EnableSyncWriter(ctx, "fixture", SyncPreferencesPrincipal, SyncPreferencesCapability); !errors.Is(err, ErrSyncWriterNotAdmitted) {
		t.Fatal("capture selection: unselected capability enabling reached database")
	}
	if _, err := st.ActivateSyncCapture(ctx, "fixture", SyncCaptureOptions{LifecycleEpoch: 1, RestoreMarker: bytes.Repeat([]byte{1}, 16), Capability: SyncPreferencesCapability}); !errors.Is(err, ErrSyncWriterNotAdmitted) {
		t.Fatal("capture selection: unselected activation reached database")
	}
	if err := st.SuspendSyncCapture(ctx, "fixture"); !errors.Is(err, ErrSyncWriterNotAdmitted) {
		t.Fatal("capture selection: unselected suspension reached database")
	}
	if status, err := st.SyncCaptureStatus(ctx, "fixture"); err != nil || status != runtimeconfig.SyncDisabled {
		t.Fatal("capture selection: unselected status reached database")
	}
}

func TestSyncCaptureStatusAndExplicitSuspension(t *testing.T) {
	st, p := newSyncCaptureFixture(t, false)
	ctx := context.Background()
	if status, err := st.SyncCaptureStatus(ctx, p.AccountID); err != nil || status != runtimeconfig.SyncDisabled {
		t.Fatal("capture status: missing control row was not disabled")
	}
	activateSyncFixture(t, st, p.AccountID)
	if status, err := st.SyncCaptureStatus(ctx, p.AccountID); err != nil || status != runtimeconfig.SyncBootstrapping {
		t.Fatal("capture status: acknowledged pin did not remain bootstrapping")
	}
	if err := st.SuspendSyncCapture(ctx, p.AccountID); err != nil {
		t.Fatal("capture status: explicit suspension failed")
	}
	if status, err := st.SyncCaptureStatus(ctx, p.AccountID); err != nil || status != runtimeconfig.SyncSuspended {
		t.Fatal("capture status: explicit suspension did not persist")
	}
	var epoch int64
	var reason string
	if err := st.pool.QueryRow(ctx, `SELECT capture_epoch,suspension_reason FROM sync_capture WHERE account_id=$1`, p.AccountID).Scan(&epoch, &reason); err != nil || epoch != 1 || reason != "coverage_lost" {
		t.Fatal("capture status: suspension changed epoch or fixed reason")
	}
}

func TestSyncCaptureCoverageLossSuspendsAndPreservesJournal(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"domain_disabled", `ALTER TABLE agent_dashboard_preferences DISABLE TRIGGER sync_writer_guard`},
		{"domain_wrong_events", `DROP TRIGGER sync_writer_guard ON agent_dashboard_preferences;
		 CREATE TRIGGER sync_writer_guard BEFORE INSERT ON agent_dashboard_preferences FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard()`},
		{"coverage_disabled", `ALTER TABLE sync_tx_admissions DISABLE TRIGGER sync_tx_coverage`},
		{"coverage_wrong_events", `DROP TRIGGER sync_tx_coverage ON sync_tx_admissions;
		 CREATE CONSTRAINT TRIGGER sync_tx_coverage AFTER DELETE ON sync_tx_admissions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION witself_sync_check_coverage()`},
		{"capability_revoked", `UPDATE sync_writer_capabilities SET enabled=false,revoked_generation=1 WHERE account_id='capture_account'`},
		{"manifest_changed", `UPDATE sync_capture SET coverage_manifest_commitment=decode(repeat('00',32),'hex') WHERE account_id='capture_account'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, p := newSyncCaptureFixture(t, true)
			ctx := context.Background()
			if _, err := st.PutDashboardPreferences(ctx, p, syncPreferenceDocument("amber")); err != nil {
				t.Fatal("capture coverage: initial journaled preference failed")
			}
			var before []byte
			// Compare canonical ordered row JSON in memory. Journal commitments and
			// payloads are never printed, including when this assertion fails.
			const retained = `SELECT jsonb_build_object(
			 'preferences',(SELECT jsonb_agg(to_jsonb(p) ORDER BY p.agent_id) FROM agent_dashboard_preferences p WHERE account_id=$1),
			 'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY o.operation_id) FROM sync_operations o WHERE account_id=$1),
			 'payloads',(SELECT jsonb_agg(to_jsonb(p) ORDER BY p.operation_id) FROM sync_operation_payloads p WHERE account_id=$1),
			 'admissions',(SELECT jsonb_agg(to_jsonb(a) ORDER BY a.tx_id) FROM sync_tx_admissions a WHERE account_id=$1),
			 'touches',(SELECT jsonb_agg(to_jsonb(t) ORDER BY t.tx_id,t.touch_id) FROM sync_tx_touches t WHERE account_id=$1),
			 'bundles',(SELECT jsonb_agg(to_jsonb(b) ORDER BY b.capture_epoch,b.finalized) FROM sync_bundles b WHERE account_id=$1),
			 'members',(SELECT jsonb_agg(to_jsonb(m) ORDER BY m.bundle_id,m.member_order) FROM sync_bundle_operations m WHERE account_id=$1),
			 'counters',(SELECT jsonb_agg(to_jsonb(c) ORDER BY c.capture_epoch) FROM sync_finalized_counter c WHERE account_id=$1))::text`
			if err := st.pool.QueryRow(ctx, retained, p.AccountID).Scan(&before); err != nil {
				t.Fatal("capture coverage: retained evidence baseline failed")
			}
			if _, err := st.pool.Exec(ctx, tc.sql); err != nil {
				t.Fatal("capture coverage: isolated coverage fault failed")
			}
			if status, err := st.SyncCaptureStatus(ctx, p.AccountID); err != nil || status != runtimeconfig.SyncSuspended {
				t.Fatal("capture coverage: coverage loss did not suspend status")
			}
			var state, reason string
			if err := st.pool.QueryRow(ctx, `SELECT coverage_state,suspension_reason FROM sync_capture WHERE account_id=$1`, p.AccountID).Scan(&state, &reason); err != nil || state != "suspended" || reason != "coverage_lost" {
				t.Fatal("capture coverage: suspension was not durable and value-free")
			}
			var after []byte
			if err := st.pool.QueryRow(ctx, retained, p.AccountID).Scan(&after); err != nil || !bytes.Equal(before, after) {
				t.Fatal("capture coverage: suspension changed journal or domain bytes")
			}
		})
	}
}

func TestSyncCaptureAcknowledgmentRefusesCoverageLoss(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"deferred_trigger", `ALTER TABLE sync_tx_admissions DISABLE TRIGGER sync_tx_coverage`},
		{"capability", `UPDATE sync_writer_capabilities SET enabled=false,revoked_generation=1 WHERE account_id='capture_account'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, p := newSyncCaptureFixture(t, false)
			ctx := context.Background()
			activation, err := st.ActivateSyncCapture(ctx, p.AccountID, SyncCaptureOptions{LifecycleEpoch: 1, RestoreMarker: bytes.Repeat([]byte{1}, 16), Capability: SyncPreferencesCapability})
			if err != nil {
				t.Fatal("capture coverage: activation before acknowledgment failed")
			}
			defer activation.Close()
			pin, err := activation.PinSnapshot(ctx)
			if err != nil {
				t.Fatal("capture coverage: candidate snapshot failed")
			}
			defer func() { _ = pin.Close(ctx) }()
			if _, err := st.pool.Exec(ctx, tc.sql); err != nil {
				t.Fatal("capture coverage: pre-acknowledgment fault failed")
			}
			if err := activation.Acknowledge(ctx, pin); !errors.Is(err, ErrSyncWriterNotAdmitted) || pin.Acknowledged() {
				t.Fatal("capture coverage: incomplete coverage acknowledged snapshot")
			}
		})
	}
}
