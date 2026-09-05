package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func orphanSubmissionFixture(t *testing.T, canary string) (eventPath, snapshotPath string) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"session_id": canary, "hook_event_name": "SessionStart", "reason": canary,
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, raw)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pending {
		if item.Event.ID != event.ID {
			continue
		}
		if err := transcriptcapture.MarkPendingSubmitted(item); err != nil {
			t.Fatal(err)
		}
		snapshot := filepath.Join(filepath.Dir(item.Path), ".submissions", filepath.Base(item.Path))
		if data, err := os.ReadFile(snapshot); err != nil || !bytes.Contains(data, []byte(canary)) {
			t.Fatalf("fixture lacks plaintext submission canary: %v", err)
		}
		return item.Path, snapshot
	}
	t.Fatal("missing queued fixture event")
	return "", ""
}

func TestTranscriptFlushOrphanSubmissionAfterInterruptedAcknowledgement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory unlink permissions require Unix modes")
	}
	const canary = "ORPHAN_INTERRUPTED_ACK_CANARY"
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, "http://127.0.0.1:1")
	event, snapshot := orphanSubmissionFixture(t, canary)
	dir := filepath.Dir(snapshot)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.RemovePending(event); err == nil {
		t.Skip("environment does not enforce directory unlink permissions")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("interrupted acknowledgement = %v", err)
	}
	if _, err := os.Stat(event); err != nil {
		t.Errorf("snapshot unlink failure lost the discoverable event: %v", err)
	}
	if raw, err := os.ReadFile(snapshot); err != nil || !bytes.Contains(raw, []byte(canary)) {
		t.Fatalf("failed acknowledgement did not retain the test snapshot: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Reproduce residue left by the pre-fix event-first implementation (or a
	// previously interrupted upgrade): the plaintext snapshot has no event.
	if err := os.Remove(event); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode})
	}); code != 0 || strings.Contains(stderr, canary) {
		t.Fatalf("orphan-only flush exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plaintext snapshot outlived its event by a complete flush: %v", err)
	}
}

func TestTranscriptFlushOrphanSubmissionCleanupFailureIsNonfatal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory unlink permissions require Unix modes")
	}
	const canary = "ORPHAN_CLEANUP_FAILURE_CANARY"
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, "http://127.0.0.1:1")
	var snapshots []string
	for _, suffix := range []string{"-one", "-two"} {
		event, snapshot := orphanSubmissionFixture(t, canary+suffix)
		if err := os.Remove(event); err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, snapshot)
	}
	dir := filepath.Dir(snapshots[0])
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(snapshots[0]); err == nil {
		t.Skip("environment does not enforce directory unlink permissions")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("permission precondition = %v", err)
	}
	_, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode})
	})
	if code != 0 || strings.Count(stderr, "clean orphaned capture submissions:") != 1 || strings.Contains(stderr, canary) {
		t.Fatalf("cleanup failure must log once without failing flush or exposing plaintext: exit %d, %q", code, stderr)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode})
	}); code != 0 || strings.Contains(stderr, canary) {
		t.Fatalf("cleanup retry exit %d: %s", code, stderr)
	}
	for _, snapshot := range snapshots {
		if _, err := os.Stat(snapshot); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("next flush retained an orphan after permissions were restored: %v", err)
		}
	}
}

func TestTranscriptReleaseSweepsOrphanSubmissionSnapshot(t *testing.T) {
	const canary = "RELEASE_ORPHAN_SNAPSHOT_CANARY"
	for _, mode := range []string{"apply", "default-preview", "explicit-dry-run"} {
		for _, unreadable := range []bool{false, true} {
			t.Run(mode+"/"+map[bool]string{false: "readable", true: "unreadable"}[unreadable], func(t *testing.T) {
				t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
				t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
				configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, "http://127.0.0.1:1")
				event, snapshot := orphanSubmissionFixture(t, canary)
				if unreadable {
					if err := os.Chmod(snapshot, 0); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(snapshot, 0o600) })
				}
				if err := os.Remove(event); err != nil {
					t.Fatal(err)
				}
				if mode == "apply" {
					if count, err := transcriptcapture.ReleaseResidue(transcriptcapture.RuntimeClaudeCode, "", 0, true); err != nil || count != 0 {
						t.Fatalf("orphan-only release = %d, %v", count, err)
					}
				} else {
					args := []string{"--runtime", transcriptcapture.RuntimeClaudeCode}
					if mode == "explicit-dry-run" {
						args = append(args, "--dry-run", "--yes", "--all")
					}
					if stdout, stderr, code := captureFactDeleteCLI(t, func() int { return transcriptRelease(args) }); code != 0 || strings.Contains(stdout+stderr, canary) {
						t.Fatalf("preview cleanup failed or leaked plaintext: exit %d, %q, %q", code, stdout, stderr)
					}
				}
				if _, err := os.Stat(snapshot); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("release retained the plaintext orphan snapshot: %v", err)
				}
			})
		}
	}
}
