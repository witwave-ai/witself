package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestSyncCaptureSnapshotAcknowledgmentDeathCut(t *testing.T) {
	st, principal := newSyncCaptureFixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a, err := st.ActivateSyncCapture(ctx, principal.AccountID, SyncCaptureOptions{
		LifecycleEpoch: 1, RestoreMarker: bytes.Repeat([]byte{1}, 16), Capability: SyncPreferencesCapability,
	})
	if err != nil {
		t.Fatal("capture final acknowledgment activation failed")
	}
	defer a.Close()
	pin, err := a.PinSnapshot(ctx)
	if err != nil {
		t.Fatal("capture final acknowledgment snapshot failed")
	}
	defer func() { _ = pin.Close(ctx) }()
	cutReached := false
	err = a.acknowledge(ctx, pin, func() {
		cutReached = true
		var terminated bool
		if st.pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, a.pid).Scan(&terminated) != nil || !terminated {
			t.Fatal("capture final acknowledgment termination failed")
		}
		capturePoll(t, st, `SELECT NOT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND granted)`, a.pid)
		// S was just verified alive, but its owner is now gone and a new
		// admitted writer can commit before the attempted acknowledgment.
		if _, err := st.PutDashboardPreferences(ctx, principal, syncPreferenceDocument("amber")); err != nil {
			t.Fatal("capture final acknowledgment released writer failed")
		}
	})
	if !cutReached || !errors.Is(err, ErrSyncWriterNotAdmitted) || pin.Acknowledged() {
		t.Fatal("capture final acknowledgment accepted a dead gate owner")
	}
	t.Log("capture observed gate death after snapshot liveness and before final physical-session acknowledgment; candidate refused")
}
