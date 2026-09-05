package transcriptcapture

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSubmissionAcknowledgementSweepValidatesEvidenceAndPreservesCrashOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory unlink permissions require Unix modes")
	}
	const canary = "ACKNOWLEDGEMENT_TOMBSTONE_CANARY"
	cfg := setupFenceCapture(t, ModeRaw)
	cfg.Runtime = RuntimeClaudeCode
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	enqueueTestHook(t, cfg.Runtime, `{"session_id":"acknowledgement-evidence","hook_event_name":"SessionStart","reason":"`+canary+`"}`)
	pending, err := Pending(cfg.Runtime)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending event = %d, %v", len(pending), err)
	}
	item := pending[0]
	if err := MarkPendingSubmitted(item); err != nil {
		t.Fatal(err)
	}
	snapshot := submissionPath(item.Path)
	original, err := os.ReadFile(snapshot)
	if err != nil || !bytes.Contains(original, []byte(canary)) {
		t.Fatalf("attempted snapshot lacks canary: %v", err)
	}
	outbox := filepath.Dir(item.Path)
	dir := filepath.Dir(snapshot)
	t.Cleanup(func() {
		_ = os.Chmod(outbox, 0o700)
		_ = os.Chmod(dir, 0o700)
	})
	if err := os.Chmod(outbox, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := RemovePending(item.Path); err == nil {
		t.Skip("environment does not enforce directory unlink permissions")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("interrupted acknowledgement = %v", err)
	}
	if err := os.Chmod(outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	ackPath := snapshot + ".ack"
	acknowledgement, err := os.ReadFile(ackPath)
	if err != nil || bytes.Contains(acknowledgement, []byte(canary)) {
		t.Fatalf("interrupted acknowledgement lacks value-free evidence: %v", err)
	}
	// Invalid identity and untrusted writable evidence may never authorize
	// removing the event, even though the plaintext snapshot is already gone.
	invalid := bytes.Replace(acknowledgement, []byte(item.Event.ID), []byte("unrelated-event"), 1)
	if err := os.WriteFile(ackPath, invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SweepOrphanSubmissions(cfg.Runtime); err == nil {
		t.Fatal("sweep accepted acknowledgement for a different event")
	}
	if _, err := os.Stat(item.Path); err != nil {
		t.Fatalf("invalid acknowledgement removed the event: %v", err)
	}
	if err := os.WriteFile(ackPath, acknowledgement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ackPath, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := SweepOrphanSubmissions(cfg.Runtime); err == nil {
		t.Fatal("sweep accepted untrusted acknowledgement")
	}
	if _, err := os.Stat(item.Path); err != nil {
		t.Fatalf("untrusted acknowledgement removed the event: %v", err)
	}
	if err := os.Chmod(ackPath, 0o600); err != nil {
		t.Fatal(err)
	}
	// Recreate the earlier crash boundary: evidence was published but snapshot
	// unlink has not succeeded. Sweep must still remove snapshot before event.
	if err := os.WriteFile(snapshot, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := SweepOrphanSubmissions(cfg.Runtime); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("snapshot unlink refusal = %v", err)
	}
	for _, path := range []string{snapshot, item.Path, ackPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("snapshot unlink failure lost required retry state: %v", err)
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SweepOrphanSubmissions(cfg.Runtime); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{snapshot, item.Path, ackPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("sweep retained acknowledged content or evidence: %v", err)
		}
	}
}
