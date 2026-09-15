package transcriptcapture

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDSHNativeRetryBudgetSurvivesReloadAndExpires(t *testing.T) {
	dshTestCaptureConfig(t, ModeTrace)
	writeDSHSessionLog(t, "/tmp/project", "retry-budget", "session.v3.jsonl", newDSHLog("retry-budget", "/tmp/project").plain())
	enqueueDSHHook(t, map[string]any{"session_id": "retry-budget", "cwd": "/tmp/project", "hook_event_name": "Stop"})
	pending, err := Pending(RuntimeDSH)
	if err != nil || len(pending) != 1 {
		t.Fatal("expected one isolated pending Stop")
	}
	initial, err := initDSHNativeRetry(pending[0])
	if err != nil {
		t.Fatal(err)
	}
	var first dshNativeRetryState
	if json.Unmarshal(initial.Event.Data, &first) != nil || first.Deadline.IsZero() {
		t.Fatal("retry deadline was not initialized")
	}
	_, expired, err := finishDSHNativeRetry(initial)
	if err != nil || expired {
		t.Fatal("new retry budget was not scheduled")
	}
	// Reconstruct all state from disk as a newly started detached process
	// does; no local deadline or timer from the first attempt is retained.
	reloaded, err := Pending(RuntimeDSH)
	if err != nil || len(reloaded) != 1 {
		t.Fatal("retry state was not durable")
	}
	restarted, err := initDSHNativeRetry(reloaded[0])
	if err != nil {
		t.Fatal(err)
	}
	var second dshNativeRetryState
	if json.Unmarshal(restarted.Event.Data, &second) != nil || !second.Deadline.Equal(first.Deadline) || !second.At.After(first.At) {
		t.Fatal("restart reset the retry deadline or lost the next attempt")
	}
	if at, ok := DSHNativeRetryAt(restarted); !ok || !at.Equal(second.At) {
		t.Fatal("detached flusher could not recover the durable schedule")
	}
	otherRuntime := restarted
	otherRuntime.Event.Runtime = RuntimeCodex
	if _, ok := DSHNativeRetryAt(otherRuntime); ok {
		t.Fatal("dsh retry schedule changed another runtime")
	}

	past := time.Now().UTC().Add(-time.Second)
	expiring, err := persistDSHNativeRetry(restarted, dshNativeRetryState{Deadline: past, At: past})
	if err != nil {
		t.Fatal(err)
	}
	_, expired, err = finishDSHNativeRetry(expiring)
	if err != nil || !expired {
		t.Fatal("exhausted durable retry budget restarted")
	}
	var stripped map[string]any
	if json.Unmarshal(withoutDSHNativeRetry(expiring.Event.Data), &stripped) != nil {
		t.Fatal("retry metadata could not be stripped")
	}
	for _, key := range []string{"dsh_native_retry_deadline", "dsh_native_retry_at"} {
		if _, ok := stripped[key]; ok {
			t.Fatal("local retry metadata survived finalization")
		}
	}
}
