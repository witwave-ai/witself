package transcriptcapture

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruneSkippedSessionsUsesMtimeAndEnforcesCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	writeMarker := func(name string, modTime time.Time) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("not parsed by prune\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatal(err)
		}
		return path
	}

	current := writeMarker("current.json", now.Add(-60*24*time.Hour))
	staleOne := writeMarker("stale-one.json", now.Add(-50*24*time.Hour))
	staleTwo := writeMarker("stale-two.json", now.Add(-40*24*time.Hour))
	oldestFresh := writeMarker("oldest-fresh.json", now.Add(-29*24*time.Hour))
	secondOldestFresh := writeMarker("second-oldest-fresh.json", now.Add(-28*24*time.Hour))
	var retainedFresh string
	for index := 0; index < 511; index++ {
		path := writeMarker(fmt.Sprintf("fresh-%03d.json", index), now.Add(-time.Duration(index)*time.Minute))
		if index == 0 {
			retainedFresh = path
		}
	}
	lockPath := writeMarker("current.json.lock", now.Add(-90*24*time.Hour))

	pruneSkippedSessions(dir, current, now)

	for _, path := range []string{staleOne, staleTwo, oldestFresh, secondOldestFresh} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("pruned path %s still exists: %v", filepath.Base(path), err)
		}
	}
	for _, path := range []string{current, retainedFresh, lockPath} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("retained path %s: %v", filepath.Base(path), err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	markers := 0
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr == nil && info.Mode().IsRegular() && filepath.Ext(entry.Name()) == ".json" {
			markers++
		}
	}
	if markers != maxSkippedSessionMarkers {
		t.Fatalf("marker count after prune = %d, want %d", markers, maxSkippedSessionMarkers)
	}
}

func TestSkippedSessionLockDoesNotStealLiveOwnerAndReleaseChecksPID(t *testing.T) {
	oldWait := skippedSessionLockWait
	skippedSessionLockWait = 25 * time.Millisecond
	t.Cleanup(func() { skippedSessionLockWait = oldWait })

	markerPath := filepath.Join(t.TempDir(), "marker.json")
	release, err := acquireSkippedSessionLock(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := markerPath + ".lock"
	old := time.Now().Add(-2 * skippedSessionLockStale)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}

	unexpectedRelease, err := acquireSkippedSessionLock(markerPath)
	if err == nil {
		unexpectedRelease()
		t.Fatal("second acquire stole a stale-mtime lock from the live test process")
	}
	if !strings.Contains(err.Error(), "timed out acquiring skipped session marker lock") {
		t.Fatalf("second acquire error = %v", err)
	}
	wantOwner := fmt.Sprintf("%d\n", os.Getpid())
	if raw, readErr := os.ReadFile(lockPath); readErr != nil || string(raw) != wantOwner {
		t.Fatalf("live-owner lock after timeout = %q / %v, want %q", raw, readErr, wantOwner)
	}

	foreignOwner := fmt.Sprintf("%d\n", os.Getpid()+1)
	if err := os.WriteFile(lockPath, []byte(foreignOwner), 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	if raw, readErr := os.ReadFile(lockPath); readErr != nil || string(raw) != foreignOwner {
		t.Fatalf("release changed lock it did not own = %q / %v, want %q", raw, readErr, foreignOwner)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
}

func TestCopyThenRemoveCrashRecovery(t *testing.T) {
	t.Run("identical destination", func(t *testing.T) {
		dir := t.TempDir()
		source := filepath.Join(dir, "source.json")
		destination := filepath.Join(dir, "destination.json")
		want := []byte("identical quarantine bytes\n")
		if err := os.WriteFile(source, want, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, want, 0o600); err != nil {
			t.Fatal(err)
		}

		if err := copyThenRemove(source, destination); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reconciled source still exists: %v", err)
		}
		if got, err := os.ReadFile(destination); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("reconciled destination = %q / %v", got, err)
		}
	})

	t.Run("leftover temp", func(t *testing.T) {
		dir := t.TempDir()
		source := filepath.Join(dir, "source.json")
		destination := filepath.Join(dir, "destination.json")
		tempPath := filepath.Join(dir, ".tmp-"+filepath.Base(destination))
		want := []byte("new quarantine bytes\n")
		if err := os.WriteFile(source, want, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tempPath, []byte("interrupted copy bytes\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := copyThenRemove(source, destination); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("copied source still exists: %v", err)
		}
		if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("leftover temp still exists: %v", err)
		}
		if got, err := os.ReadFile(destination); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("destination after temp recovery = %q / %v", got, err)
		}
		if info, err := os.Stat(destination); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("destination permissions = %v / %v", info, err)
		}
	})
}

func TestQuarantineEphemeralReconcilesIdenticalDestination(t *testing.T) {
	home := t.TempDir()
	witselfHome := filepath.Join(home, ".witself")
	t.Setenv("WITSELF_HOME", witselfHome)
	event := Event{
		SchemaVersion: SchemaVersion,
		ID:            "evt-ephemeral-interrupted-move",
		Runtime:       RuntimeCodex,
		SessionID:     "ephemeral-interrupted-move",
		HookEvent:     "SessionStart",
		OccurredAt:    time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
	}
	if err := writeOutboxEvent(event); err != nil {
		t.Fatal(err)
	}
	pending, err := Pending(RuntimeCodex)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %#v / %v", pending, err)
	}
	sourceRaw, err := os.ReadFile(pending[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(witselfHome, "capture", "quarantine", RuntimeCodex)
	if err := os.MkdirAll(quarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(quarantine, filepath.Base(pending[0].Path))
	if err := os.WriteFile(destination, sourceRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	moved, remaining, err := QuarantineEphemeral(RuntimeCodex, pending)
	if err != nil || len(moved) != 1 || len(remaining) != 0 || moved[0].Path != destination {
		t.Fatalf("moved/remaining/error = %#v/%#v/%v", moved, remaining, err)
	}
	if _, err := os.Stat(pending[0].Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reconciled pending source still exists: %v", err)
	}
	if got, err := os.ReadFile(destination); err != nil || !bytes.Equal(got, sourceRaw) {
		t.Fatalf("reconciled destination = %q / %v", got, err)
	}
	markers, err := SkippedSessions(RuntimeCodex)
	if err != nil || len(markers) != 1 || markers[0].Events != 1 {
		t.Fatalf("reconciled skip markers = %#v / %v", markers, err)
	}
}

func TestQuarantineEphemeralUsesEventTimeForMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	occurredAt := time.Date(2024, 3, 4, 5, 6, 7, 0, time.FixedZone("event-zone", -7*60*60))
	event := Event{
		SchemaVersion: SchemaVersion,
		ID:            "evt-ephemeral-event-time",
		Runtime:       RuntimeCodex,
		SessionID:     "ephemeral-event-time",
		HookEvent:     "SessionStart",
		OccurredAt:    occurredAt,
	}
	if err := writeOutboxEvent(event); err != nil {
		t.Fatal(err)
	}
	pending, err := Pending(RuntimeCodex)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %#v / %v", pending, err)
	}
	beforeQuarantine := time.Now().Add(-time.Second)
	moved, remaining, err := QuarantineEphemeral(RuntimeCodex, pending)
	if err != nil || len(moved) != 1 || len(remaining) != 0 {
		t.Fatalf("moved/remaining/error = %d/%d/%v", len(moved), len(remaining), err)
	}
	markers, err := SkippedSessions(RuntimeCodex)
	if err != nil || len(markers) != 1 {
		t.Fatalf("markers = %#v / %v", markers, err)
	}
	wantSeen := occurredAt.UTC()
	if !markers[0].FirstSeen.Equal(wantSeen) || !markers[0].LastSeen.Equal(wantSeen) ||
		markers[0].FirstSeen.Location() != time.UTC || markers[0].LastSeen.Location() != time.UTC {
		t.Fatalf("marker event times = %s / %s, want %s UTC", markers[0].FirstSeen, markers[0].LastSeen, wantSeen)
	}
	markerPath := filepath.Join(home, ".witself", "capture", "skipped", RuntimeCodex, markers[0].SessionHash+".json")
	if info, statErr := os.Stat(markerPath); statErr != nil || info.ModTime().Before(beforeQuarantine) {
		t.Fatalf("marker mtime = %v / %v, want quarantine wall clock", info, statErr)
	}
}

func TestSkippedSessionMarkerReviewerDoesNotReplaceRealModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	for _, sessionID := range []string{"real-first", "review-first"} {
		if sessionID == "real-first" {
			if err := recordSkippedSession(RuntimeCodex, sessionID, "SessionStart", "0.149.0", "gpt-5.6-sol", base); err != nil {
				t.Fatal(err)
			}
			if err := recordSkippedSession(RuntimeCodex, sessionID, HookEventCodexPermissionReview, "0.149.0", codexAutoReviewModel, base.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := recordSkippedSession(RuntimeCodex, sessionID, HookEventCodexPermissionReview, "0.149.0", codexAutoReviewModel, base); err != nil {
				t.Fatal(err)
			}
			if err := recordSkippedSession(RuntimeCodex, sessionID, "SessionStart", "0.149.0", "gpt-5.6-sol", base.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	markers, err := SkippedSessions(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 2 {
		t.Fatalf("markers = %#v, want two", markers)
	}
	for _, marker := range markers {
		if marker.Model != "gpt-5.6-sol" || marker.Events != 2 || marker.HookEvents[HookEventCodexPermissionReview] != 1 {
			t.Fatalf("marker after reviewer event = %#v", marker)
		}
	}
}
