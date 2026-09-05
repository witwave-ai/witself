package transcriptcapture

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRemovePendingSubmissionCrashOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory unlink permissions require Unix modes")
	}
	const canary = "SUBMISSION_CRASH_ORDER_CANARY"
	cfg := setupFenceCapture(t, ModeRaw)
	cfg.Runtime = RuntimeClaudeCode
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	enqueueTestHook(t, cfg.Runtime, `{"session_id":"crash-order","hook_event_name":"SessionStart","reason":"`+canary+`"}`)
	pending, err := Pending(cfg.Runtime)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending event = %d, %v", len(pending), err)
	}
	item := pending[0]
	if err := MarkPendingSubmitted(item); err != nil {
		t.Fatal(err)
	}
	snapshot := submissionPath(item.Path)
	if raw, err := os.ReadFile(snapshot); err != nil || !bytes.Contains(raw, []byte(canary)) {
		t.Fatalf("fixture lacks plaintext snapshot canary: %v", err)
	}
	outbox := filepath.Dir(item.Path)
	t.Cleanup(func() { _ = os.Chmod(outbox, 0o700) })
	if err := os.Chmod(outbox, 0o500); err != nil {
		t.Fatal(err)
	}
	// Prevent only the event unlink. This deterministically stops acknowledgement
	// between its two unlink operations, at the same boundary as a process crash.
	if err := RemovePending(item.Path); err == nil {
		t.Skip("environment does not enforce directory unlink permissions")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("interrupted acknowledgement = %v", err)
	}
	if _, err := os.Stat(item.Path); err != nil {
		t.Fatalf("event is no longer discoverable after interrupted acknowledgement: %v", err)
	}
	if _, err := os.Stat(snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plaintext snapshot survived the first unlink boundary: %v", err)
	}
	if err := os.Chmod(outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RemovePending(item.Path); err != nil {
		t.Fatalf("next acknowledgement cannot finish cleanup: %v", err)
	}
	if _, err := os.Stat(item.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry retained the event: %v", err)
	}
}
