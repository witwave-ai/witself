package transcriptcapture

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompletedFenceRejectsStaleSealedSnapshotsAfterTerminalUpload(t *testing.T) {
	setupFenceCapture(t, ModeRaw)
	prompt := fenceTestPromptAndTool(t, false)
	stale, err := Pending(RuntimeCodex)
	if err != nil || len(stale) != 2 {
		t.Fatalf("pending before sealed hook = %d, %v", len(stale), err)
	}
	enqueueTestHook(t, RuntimeCodex, `{"session_id":"delegated","transcript_path":"/tmp/delegated-rollout.jsonl","hook_event_name":"PreToolUse","tool_name":"mcp__witself__witself_secret_reveal"}`)
	fence, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "")
	if err != nil || !created {
		t.Fatalf("sealed fence: created=%t, error=%v", created, err)
	}
	laterPrompt := prompt
	laterPrompt.TurnID = "next-turn"
	laterPrompt.OccurredAt = fence.OccurredAt.Add(time.Second)
	for _, test := range []struct {
		name  string
		extra []PendingEvent
	}{
		{name: "marker-only"},
		{name: "pending-terminal", extra: []PendingEvent{{Event: fence}}},
		{name: "later-prompt", extra: []PendingEvent{{Event: laterPrompt}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := append(append([]PendingEvent(nil), stale...), test.extra...)
			index := NewReadinessIndex(snapshot)
			for _, item := range stale {
				if index.UploadReady(item) || PendingEventUploadReady(item, snapshot) {
					t.Fatalf("sealed completion released stale %s content", item.Event.HookEvent)
				}
			}
		})
	}
	pending, err := Pending(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pending {
		if item.Event.ID == fence.ID {
			if err := RemovePending(item.Path); err != nil {
				t.Fatal(err)
			}
		}
	}
	pending, err = Pending(RuntimeCodex)
	if err != nil || len(pending) != 3 {
		t.Fatalf("retained sealed events = %d, %v", len(pending), err)
	}
	index := NewReadinessIndex(pending)
	for _, item := range pending {
		raw, err := os.ReadFile(item.Path)
		if err != nil || bytes.Contains(raw, []byte("fence-canary")) || !eventSealedContentOmitted(item.Event.Data) ||
			len(item.Event.Raw) != 0 || len(item.Event.RecoveredMessages) != 0 || !index.UploadReady(item) ||
			!PendingEventUploadReady(item, pending) {
			t.Fatalf("retained sealed %s leaked or remained blocked: %v", item.Event.HookEvent, err)
		}
	}
	for _, field := range []string{"raw", "recovered-messages"} {
		t.Run(field, func(t *testing.T) {
			unsafe := pending[0]
			if field == "raw" {
				unsafe.Event.Raw = []byte(`{"prompt":"sealed-canary"}`)
			} else {
				unsafe.Event.RecoveredMessages = []RecoveredMessage{{Body: "sealed-canary"}}
			}
			if PendingEventUploadReady(unsafe, []PendingEvent{unsafe, {Event: fence}}) {
				t.Fatal("sealed omission flag released a snapshot with retained content")
			}
		})
	}
}

func TestCompletedFenceSurvivesSessionLifecycleAndKeepsIdentityBoundaries(t *testing.T) {
	setupFenceCapture(t, ModeRaw)
	prompt := fenceTestPromptAndTool(t, false)
	fence, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "")
	if err != nil || !created {
		t.Fatalf("first fence: created=%t, error=%v", created, err)
	}
	pending, err := Pending(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	var retained PendingEvent
	for _, item := range pending {
		if item.Event.HookEvent == "PreToolUse" {
			retained = item
		}
	}
	if retained.Path == "" {
		t.Fatal("missing retained tool event")
	}
	assertRetainedReady := func(t *testing.T) {
		t.Helper()
		pending, err := Pending(RuntimeCodex)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range pending {
			if item.Path != retained.Path {
				if err := RemovePending(item.Path); err != nil {
					t.Fatal(err)
				}
			}
		}
		pending, err = Pending(RuntimeCodex)
		if err != nil || len(pending) != 1 || !NewReadinessIndex(pending).UploadReady(retained) ||
			!PendingEventUploadReady(retained, pending) {
			t.Fatalf("retained tool lost completed-turn readiness: pending=%d, error=%v", len(pending), err)
		}
	}
	assertRetainedReady(t)
	nextPrompt := fenceTestPromptAndTool(t, false)
	if _, created, err := EnqueueFence(RuntimeCodex, "delegated", nextPrompt.RunID, nextPrompt.TurnID, ""); err != nil || !created {
		t.Fatalf("next fence: created=%t, error=%v", created, err)
	}
	assertRetainedReady(t)
	enqueueTestHook(t, RuntimeCodex, `{"session_id":"delegated","transcript_path":"/tmp/delegated-rollout.jsonl","hook_event_name":"SessionEnd"}`)
	statePath, err := sessionStatePath(RuntimeCodex, "delegated")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SessionEnd did not remove session state: %v", err)
	}
	assertRetainedReady(t)
	started := enqueueTestHook(t, RuntimeCodex, `{"session_id":"delegated","transcript_path":"/tmp/delegated-rollout.jsonl","hook_event_name":"SessionStart"}`)
	assertRetainedReady(t)
	if started.RunID == retained.Event.RunID {
		t.Fatal("SessionStart did not create a new run")
	}
	for _, test := range []struct {
		name string
		run  string
		at   time.Time
	}{
		{name: "new-run-same-turn", run: started.RunID, at: retained.Event.OccurredAt},
		{name: "same-run-later-event", run: retained.Event.RunID, at: fence.OccurredAt.Add(time.Second)},
	} {
		t.Run(test.name, func(t *testing.T) {
			other := retained
			other.Event.RunID = test.run
			other.Event.OccurredAt = test.at
			if PendingEventUploadReady(other, []PendingEvent{other}) {
				t.Fatal("completed fence released an event outside its run or completion boundary")
			}
		})
	}
}

func TestCompletedFenceWriteFailureKeepsExactPendingFenceForRetry(t *testing.T) {
	setupFenceCapture(t, ModeRaw)
	prompt := fenceTestPromptAndTool(t, false)
	markerPath, err := completedFencePath(prompt)
	if err != nil {
		t.Fatal(err)
	}
	markerDir := filepath.Dir(markerPath)
	if err := os.MkdirAll(filepath.Dir(markerDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerDir, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "original-reason"); err == nil || created {
		t.Fatalf("failed marker write: created=%t, error=%v", created, err)
	}
	state, err := loadSessionState(RuntimeCodex, "delegated")
	if err != nil || state.PendingFence == nil {
		t.Fatalf("failed marker write lost pending fence: %v", err)
	}
	expected := *state.PendingFence
	pending, err := Pending(RuntimeCodex)
	if err != nil || len(pending) != 2 {
		t.Fatalf("marker failure published a terminal: pending=%d, error=%v", len(pending), err)
	}
	for _, item := range pending {
		if NewReadinessIndex(pending).UploadReady(item) {
			t.Fatal("marker failure released an incomplete turn")
		}
	}
	if err := os.Remove(markerDir); err != nil {
		t.Fatal(err)
	}
	replayed, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "changed-reason")
	if err != nil || !created || replayed.ID != expected.ID || !replayed.OccurredAt.Equal(expected.OccurredAt) ||
		!bytes.Equal(replayed.Data, expected.Data) {
		t.Fatalf("marker retry changed the durable fence: created=%t, error=%v", created, err)
	}
	pending, err = Pending(RuntimeCodex)
	if err != nil || len(pending) != 3 {
		t.Fatalf("marker retry did not publish one terminal: pending=%d, error=%v", len(pending), err)
	}
	if marker := loadCompletedFence(prompt); !marker.OccurredAt.Equal(expected.OccurredAt) {
		t.Fatal("marker retry did not persist the original completion timestamp")
	}
}
