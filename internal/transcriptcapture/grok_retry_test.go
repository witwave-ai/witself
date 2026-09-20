package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func grokOrphanStopFixture(t *testing.T) PendingEvent {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	t.Setenv("DSH_HOME", filepath.Join(home, ".dsh"))
	saveTestCaptureConfig(t, RuntimeGrokBuild)
	raw, err := os.ReadFile("testdata/grok-stop-without-turn-or-transcript.json")
	if err != nil {
		t.Fatal(err)
	}
	event := enqueueTestHook(t, RuntimeGrokBuild, string(raw))
	if event.TurnID != "" || event.SourceTranscriptPath != "" || event.NativeTurnFinalized {
		t.Fatal("fixture did not reproduce the unresolved Stop shape")
	}
	pending, err := Pending(RuntimeGrokBuild)
	if err != nil || len(pending) != 1 {
		t.Fatal("expected one isolated Stop")
	}
	return pending[0]
}

func TestGrokOrphanStopRetrySurvivesReloadAndSettlesValueFree(t *testing.T) {
	pending := grokOrphanStopFixture(t)
	initial, ready, err := FinalizePending(pending)
	if err != nil || ready {
		t.Fatalf("orphan Stop must enter a bounded retry: ready=%t err=%v", ready, err)
	}
	var state struct {
		Deadline time.Time `json:"grok_native_retry_deadline"`
	}
	if json.Unmarshal(initial.Event.Data, &state) != nil || !state.Deadline.After(time.Now()) {
		t.Fatal("missing durable Grok retry deadline")
	}
	firstDeadline := state.Deadline
	if at, ok := GrokNativeRetryAt(initial); !ok || !at.Equal(firstDeadline) {
		t.Fatal("detached flusher cannot recover the orphan deadline")
	}
	projection, err := json.Marshal(initial.Event.Entries())
	if err != nil || bytes.Contains(projection, []byte("grok_native_retry_deadline")) {
		t.Fatal("local retry deadline leaked into transcript projection")
	}
	reloaded, err := Pending(RuntimeGrokBuild)
	if err != nil || len(reloaded) != 1 {
		t.Fatal("could not reload pending Stop")
	}
	retried, ready, err := FinalizePending(reloaded[0])
	if err != nil || ready || json.Unmarshal(retried.Event.Data, &state) != nil || !state.Deadline.Equal(firstDeadline) {
		t.Fatal("restarting the flusher reset the retry budget")
	}
	// Expire the durable deadline without a wall-clock sleep, and inject
	// synthetic content to prove the terminal fallback drops every content field.
	past := time.Now().UTC().Add(-time.Second)
	retried.Event.Data, _ = json.Marshal(map[string]any{
		"grok_native_retry_deadline": past, "reason": "orphan-content-canary",
		"usage": map[string]any{"input_tokens": 7},
	})
	retried.Event.Body = "orphan-content-canary"
	retried.Event.Raw = json.RawMessage(`{"value":"orphan-content-canary"}`)
	retried.Event.Model, retried.Event.ModelSource = "orphan-content-canary", "hook"
	retried.Event.ModelProvider, retried.Event.ModelProviderSource = "orphan-content-canary", "hook"
	retried.Event.RecoveredMessages = []RecoveredMessage{{Body: "orphan-content-canary"}}
	if err := writeJSONAtomic(retried.Path, retried.Event); err != nil {
		t.Fatal(err)
	}
	final, ready, err := FinalizePending(retried)
	if err != nil || !ready || !final.Event.NativeTurnFinalized || final.Event.Kind != "turn.completed" || final.Event.Role != "system" {
		t.Fatalf("expired orphan Stop did not settle: ready=%t err=%v", ready, err)
	}
	if _, scheduled := GrokNativeRetryAt(final); scheduled {
		t.Fatal("settled Stop retained a retry schedule")
	}
	if final.Event.ID != pending.Event.ID || !final.Event.OccurredAt.Equal(pending.Event.OccurredAt) {
		t.Fatal("settling changed event identity")
	}
	var details map[string]any
	if json.Unmarshal(final.Event.Data, &details) != nil || len(details) != 1 || details["grok_native_finalization"] != "unresolved_prompt_without_transcript" {
		t.Fatal("terminal reason missing or extra content retained")
	}
	persisted, err := os.ReadFile(final.Path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := json.Marshal(final.Event.Entries())
	if err != nil || bytes.Contains(persisted, []byte("orphan-content-canary")) || bytes.Contains(entries, []byte("orphan-content-canary")) || bytes.Contains(entries, []byte("grok_native_retry_deadline")) {
		t.Fatal("settled Stop retained content or local retry metadata")
	}
	reloaded, err = Pending(RuntimeGrokBuild)
	if err != nil || len(reloaded) != 1 {
		t.Fatal("could not reload settled Stop")
	}
	_, ready, err = FinalizePending(reloaded[0])
	after, readErr := os.ReadFile(final.Path)
	if err != nil || readErr != nil || !ready || !bytes.Equal(persisted, after) {
		t.Fatal("settled Stop was rewritten on retry")
	}
}

func TestGrokOrphanStopKeepsIdentifiableAndProtectedStopsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Event)
	}{
		{"turn", func(e *Event) { e.TurnID = "known-turn" }},
		{"prompt", func(e *Event) { e.Data = json.RawMessage(`{"prompt_id":"known-prompt"}`) }},
		{"transcript", func(e *Event) { e.SourceTranscriptPath = "/unreadable/native/updates.jsonl" }},
		{"invalid data", func(e *Event) { e.Data = json.RawMessage(`{"prompt_id":17}`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending := grokOrphanStopFixture(t)
			tc.mutate(&pending.Event)
			if err := writeJSONAtomic(pending.Path, pending.Event); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(pending.Path)
			if err != nil {
				t.Fatal(err)
			}
			_, ready, err := FinalizePending(pending)
			if ready || err == nil {
				t.Fatal("identifiable or invalid Stop was settled as an orphan")
			}
			after, err := os.ReadFile(pending.Path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("existing unresolved Stop behavior changed")
			}
			if _, scheduled := GrokNativeRetryAt(pending); scheduled {
				t.Fatal("non-orphan Stop acquired a deadline")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Event)
	}{
		{"other runtime", func(e *Event) { e.Runtime = RuntimeCodex }},
		{"other hook", func(e *Event) { e.HookEvent = "SessionEnd" }},
		{"assistant", func(e *Event) { e.Kind, e.Role = "message.assistant", "assistant" }},
		{"finalized", func(e *Event) { e.NativeTurnFinalized = true }},
		{"synthetic", func(e *Event) { e.Data = json.RawMessage(`{"synthetic_fence":true}`) }},
		{"sealed", func(e *Event) { e.Data = json.RawMessage(`{"sealed_content_omitted":true}`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending := grokOrphanStopFixture(t)
			tc.mutate(&pending.Event)
			final, ready, err := FinalizePending(pending)
			if err != nil || !ready || bytes.Contains(final.Event.Data, []byte("grok_native_")) {
				t.Fatal("protected or completed event entered orphan finalization")
			}
		})
	}
}

func TestGrokOrphanStopRejectsInvalidRetryDeadline(t *testing.T) {
	pending := grokOrphanStopFixture(t)
	pending.Event.Data = json.RawMessage(`{"grok_native_retry_deadline":"invalid"}`)
	if err := writeJSONAtomic(pending.Path, pending.Event); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(pending.Path)
	if err != nil {
		t.Fatal(err)
	}
	_, ready, err := FinalizePending(pending)
	if ready || err == nil {
		t.Fatal("invalid durable deadline was replaced or settled")
	}
	after, err := os.ReadFile(pending.Path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid retry state was rewritten")
	}
}

func TestGrokOrphanStopBoundsMissingOrFutureTimestamp(t *testing.T) {
	for _, occurredAt := range []time.Time{{}, time.Now().Add(24 * time.Hour)} {
		t.Run(occurredAt.String(), func(t *testing.T) {
			pending := grokOrphanStopFixture(t)
			pending.Event.OccurredAt = occurredAt
			before := time.Now()
			initial, ready, err := FinalizePending(pending)
			if err != nil || ready {
				t.Fatal("legacy orphan did not get a retry budget")
			}
			deadline, scheduled := GrokNativeRetryAt(initial)
			if !scheduled || deadline.Before(before.Add(grokNativeRetryWindow)) || deadline.After(time.Now().Add(grokNativeRetryWindow)) {
				t.Fatal("missing or future hook timestamp created an unbounded deadline")
			}
			reloaded, err := Pending(RuntimeGrokBuild)
			if err != nil || len(reloaded) != 1 {
				t.Fatal("could not reload legacy retry budget")
			}
			retried, ready, err := FinalizePending(reloaded[0])
			again, scheduled := GrokNativeRetryAt(retried)
			if err != nil || ready || !scheduled || !again.Equal(deadline) {
				t.Fatal("legacy timestamp retry window restarted")
			}
		})
	}
}
