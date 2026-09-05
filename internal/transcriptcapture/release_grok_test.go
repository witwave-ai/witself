package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReleaseGrokLateStopFinalizesWithoutNativeTranscript(t *testing.T) {
	cfg := setupFenceCapture(t, ModeRaw)
	cfg.Runtime = RuntimeGrokBuild
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	prompt := enqueueTestHook(t, RuntimeGrokBuild, `{"sessionId":"released-grok","hookEventName":"user_prompt_submit","promptId":"prompt-1","prompt":"keep this prompt"}`)
	if count, err := ReleaseResidue(RuntimeGrokBuild, prompt.SessionID, 0, true); err != nil || count != 1 {
		t.Fatalf("release = %d, %v", count, err)
	}
	// No native transcript is available. A released Stop must retain its
	// captured content and never wait for or recover provider content.
	raw, _ := json.Marshal(map[string]string{
		"sessionId": prompt.SessionID, "hookEventName": "stop", "promptId": prompt.TurnID,
		"transcriptPath": filepath.Join(t.TempDir(), "missing-updates.jsonl"),
	})
	stop := enqueueTestHook(t, RuntimeGrokBuild, string(raw))
	pending, err := Pending(RuntimeGrokBuild)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pending {
		if item.Event.ID != stop.ID {
			continue
		}
		if !operatorReleasedEventSafe(item.Event) || item.Event.TurnID != prompt.TurnID {
			t.Fatal("late Stop escaped release suppression")
		}
		finalized, ready, err := finalizePendingWithin(item, time.Millisecond, time.Millisecond)
		if err != nil || !ready || !finalized.Event.NativeTurnFinalized {
			t.Fatalf("released Stop finalization = ready %t, finalized %t, error %v", ready, finalized.Event.NativeTurnFinalized, err)
		}
		if !operatorReleasedEventSafe(finalized.Event) || finalized.Event.Body != stop.Body || finalized.Event.Kind != stop.Kind ||
			!bytes.Equal(finalized.Event.Data, item.Event.Data) || !NewReadinessIndex(pending).UploadReady(finalized) {
			t.Fatal("finalization changed captured content or left the Stop gated")
		}
		before, err := os.ReadFile(item.Path)
		if err != nil {
			t.Fatal(err)
		}
		var persisted Event
		if err := json.Unmarshal(before, &persisted); err != nil || !persisted.NativeTurnFinalized {
			t.Fatalf("finalization was not persisted: %v", err)
		}
		if _, ready, err := FinalizePending(PendingEvent{Path: item.Path, Event: persisted}); err != nil || !ready {
			t.Fatalf("finalization retry = %t, %v", ready, err)
		}
		after, err := os.ReadFile(item.Path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("finalization retry changed durable bytes: %v", err)
		}
		return
	}
	t.Fatal("late Stop is missing from the outbox")
}
