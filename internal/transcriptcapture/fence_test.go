package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func setupFenceCapture(t *testing.T, mode string) Config {
	t.Helper()
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	location, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Runtime: RuntimeCodex, CaptureMode: mode, Account: "default", Realm: "default", Agent: "scott", AgentID: "agent_1", AgentName: "scott", Location: location}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func fenceTestPromptAndTool(t *testing.T, sensitive bool) Event {
	t.Helper()
	prompt := enqueueTestHook(t, RuntimeCodex, `{"session_id":"delegated","hook_event_name":"UserPromptSubmit","prompt":"prompt-fence-canary","transcript_path":"/tmp/delegated-rollout.jsonl"}`)
	tool := "ordinary_browser_tool"
	if sensitive {
		tool = "mcp__witself__witself_secret_reveal"
	}
	raw, _ := json.Marshal(map[string]any{
		"session_id": "delegated", "hook_event_name": "PreToolUse", "tool_name": tool,
		"tool_use_id": "tool-1", "tool_input": map[string]any{"value": "tool-fence-canary"},
		"transcript_path": "/tmp/delegated-rollout.jsonl",
	})
	enqueueTestHook(t, RuntimeCodex, string(raw))
	return prompt
}

func TestEnqueueFenceMakesTurnReadyAndIsIdempotentAfterUpload(t *testing.T) {
	setupFenceCapture(t, ModeRaw)
	prompt := fenceTestPromptAndTool(t, false)
	pending, err := Pending(RuntimeCodex)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending before fence = %d, %v", len(pending), err)
	}
	for _, item := range pending {
		if NewReadinessIndex(pending).UploadReady(item) {
			t.Fatal("open prompt/tool turn is upload-ready")
		}
	}
	event, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "job-completed")
	if err != nil || !created || event.Kind != "turn.completed" || event.Role != "system" || event.Body != "delegation job completed" ||
		event.HookEvent != "Stop" || event.TurnID != prompt.TurnID || event.RunID != prompt.RunID || event.ReplyToEventID != prompt.ID ||
		event.SourceTranscriptPath != prompt.SourceTranscriptPath {
		t.Fatalf("fence = %#v, %t, %v", event, created, err)
	}
	var data map[string]any
	if err := json.Unmarshal(event.Data, &data); err != nil || data["synthetic_fence"] != true || data["reason"] != "job-completed" {
		t.Fatalf("fence metadata = %s, %v", event.Data, err)
	}
	pending, err = Pending(RuntimeCodex)
	if err != nil || len(pending) != 3 {
		t.Fatalf("pending after fence = %d, %v", len(pending), err)
	}
	index := NewReadinessIndex(pending)
	for _, item := range pending {
		if !index.UploadReady(item) {
			t.Fatalf("fenced event not upload-ready: %s", item.Event.HookEvent)
		}
	}
	if _, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "different-reason"); err != nil || created {
		t.Fatalf("duplicate fence = %t, %v", created, err)
	}
	for _, item := range pending {
		if err := os.Remove(item.Path); err != nil {
			t.Fatal(err)
		}
	}
	if _, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, ""); err != nil || created {
		t.Fatalf("uploaded duplicate fence = %t, %v", created, err)
	}
	nextPrompt := fenceTestPromptAndTool(t, false)
	if next, created, err := EnqueueFence(RuntimeCodex, "delegated", nextPrompt.RunID, nextPrompt.TurnID, ""); err != nil || !created || next.ID == event.ID || next.TurnID == event.TurnID {
		t.Fatalf("next turn fence = %#v, %t, %v", next, created, err)
	}
}

func TestEnqueueFenceSensitiveTurnRedactsLikeStop(t *testing.T) {
	for _, mode := range []string{ModeRaw, ModeMessages} {
		t.Run(mode, func(t *testing.T) {
			setupFenceCapture(t, mode)
			prompt := fenceTestPromptAndTool(t, true)
			event, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "reason-fence-canary")
			if err != nil || !created || !eventSealedContentOmitted(event.Data) {
				t.Fatalf("sensitive fence = %#v, %t, %v", event, created, err)
			}
			var data map[string]any
			if err := json.Unmarshal(event.Data, &data); err != nil || data["synthetic_fence"] != true || data["reason"] != nil {
				t.Fatalf("sensitive fence metadata = %s, %v", event.Data, err)
			}
			pending, err := Pending(RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			index := NewReadinessIndex(pending)
			for _, item := range pending {
				raw, err := os.ReadFile(item.Path)
				if err != nil || bytes.Contains(raw, []byte("fence-canary")) || len(item.Event.Raw) != 0 || !index.UploadReady(item) {
					t.Fatalf("sensitive event leaked or remained held: %s, %v", item.Event.HookEvent, err)
				}
			}
			state, err := loadSessionState(RuntimeCodex, "delegated")
			if err != nil || !state.SensitiveTurn || state.TurnID != "" || state.PromptEventID != "" {
				t.Fatalf("sealed state fence was weakened: %#v, %v", state, err)
			}
		})
	}
}

func TestEnqueueFenceRefusesUnknownClosedEphemeralAndWrongBinding(t *testing.T) {
	for _, scenario := range []string{"unknown", "session-start", "real-stop", "agent-response", "ephemeral", "wrong-binding"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := setupFenceCapture(t, ModeRaw)
			prompt := Event{RunID: "unknown-run", TurnID: "unknown-turn"}
			switch scenario {
			case "session-start":
				prompt = enqueueTestHook(t, RuntimeCodex, `{"session_id":"delegated","hook_event_name":"SessionStart"}`)
			case "real-stop", "agent-response":
				prompt = fenceTestPromptAndTool(t, false)
				hook := "Stop"
				if scenario == "agent-response" {
					hook = "AgentResponse"
				}
				raw, _ := json.Marshal(map[string]any{"session_id": "delegated", "hook_event_name": hook})
				enqueueTestHook(t, RuntimeCodex, string(raw))
			case "ephemeral":
				prompt = Event{RunID: "run_legacy", TurnID: "turn_legacy"}
				if err := saveSessionState(RuntimeCodex, "delegated", sessionState{RunID: "run_legacy", TurnID: "turn_legacy"}); err != nil {
					t.Fatal(err)
				}
			case "wrong-binding":
				prompt = fenceTestPromptAndTool(t, false)
				cfg.AgentID = "agent_2"
				if err := SaveConfig(cfg); err != nil {
					t.Fatal(err)
				}
			}
			if prompt.TurnID == "" {
				prompt.TurnID = "no-open-turn"
			}
			before, err := Pending(RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			if _, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, ""); err == nil || created {
				t.Fatalf("refused fence = %t, %v", created, err)
			}
			after, err := Pending(RuntimeCodex)
			if err != nil || len(after) != len(before) {
				t.Fatalf("refused fence changed outbox: %d -> %d, %v", len(before), len(after), err)
			}
		})
	}
}

func TestEnqueueFenceConcurrentCallsCreateOneEvent(t *testing.T) {
	setupFenceCapture(t, ModeRaw)
	prompt := fenceTestPromptAndTool(t, false)
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	errors := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "")
			results <- created
			errors <- err
		})
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	createdCount := 0
	for created := range results {
		if created {
			createdCount++
		}
	}
	pending, err := Pending(RuntimeCodex)
	if err != nil || createdCount != 1 || len(pending) != 3 {
		t.Fatalf("concurrent fence = %d created, %d pending, %v", createdCount, len(pending), err)
	}
}

func TestEnqueueFenceRetriesExactEventAfterOutboxWriteFailure(t *testing.T) {
	setupFenceCapture(t, ModeRaw)
	prompt := fenceTestPromptAndTool(t, false)
	// A saved pending fence models interruption after durable state closure,
	// before outbox persistence. Force replay to fail, then make it writable.
	event, _, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "")
	if err != nil {
		t.Fatal(err)
	}
	state, err := loadSessionState(RuntimeCodex, "delegated")
	if err != nil {
		t.Fatal(err)
	}
	state.PendingFence = &event
	if err := saveSessionState(RuntimeCodex, "delegated", state); err != nil {
		t.Fatal(err)
	}
	dir, err := outboxDir(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, ""); err == nil || created {
		t.Fatalf("failed replay = %t, %v", created, err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir+".saved", dir); err != nil {
		t.Fatal(err)
	}
	replayed, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "changed-reason")
	var replayedData bytes.Buffer
	if compactErr := json.Compact(&replayedData, replayed.Data); compactErr != nil {
		t.Fatal(compactErr)
	}
	if err != nil || !created || replayed.ID != event.ID || !replayed.OccurredAt.Equal(event.OccurredAt) || !bytes.Equal(replayedData.Bytes(), event.Data) {
		t.Fatalf("replayed event = %#v, %t, %v", replayed, created, err)
	}
	pending, err := Pending(RuntimeCodex)
	if err != nil || len(pending) != 3 {
		t.Fatalf("replay duplicated event: %d, %v", len(pending), err)
	}
}

func TestEnqueueFenceRetriesRequiredSensitiveRedaction(t *testing.T) {
	for _, failRedaction := range []bool{false, true} {
		name := "redacts-marked-turn"
		if failRedaction {
			name = "failed-redaction-keeps-turn-open"
		}
		t.Run(name, func(t *testing.T) {
			setupFenceCapture(t, ModeRaw)
			prompt := fenceTestPromptAndTool(t, false)
			state, err := loadSessionState(RuntimeCodex, "delegated")
			if err != nil {
				t.Fatal(err)
			}
			state.SensitiveTurn = true
			if err := saveSessionState(RuntimeCodex, "delegated", state); err != nil {
				t.Fatal(err)
			}
			pending, err := Pending(RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			if failRedaction {
				path := pending[0].Path
				target := filepath.Join(t.TempDir(), "outside-event.json")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			_, created, fenceErr := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "reason-fence-canary")
			pending, err = Pending(RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			if failRedaction {
				stateAfter, err := loadSessionState(RuntimeCodex, "delegated")
				if fenceErr == nil || created || len(pending) != 2 || err != nil || stateAfter.TurnID != state.TurnID || !stateAfter.SensitiveTurn {
					t.Fatalf("failed redaction released the turn: created=%t, fence err=%v, state err=%v", created, fenceErr, err)
				}
				for _, item := range pending {
					if NewReadinessIndex(pending).UploadReady(item) {
						t.Fatal("unredacted turn became upload-ready")
					}
				}
				return
			}
			if fenceErr != nil || !created || len(pending) != 3 {
				t.Fatalf("retry redaction fence = %t, %v, %d events", created, fenceErr, len(pending))
			}
			for _, item := range pending {
				raw, err := os.ReadFile(item.Path)
				if err != nil || bytes.Contains(raw, []byte("fence-canary")) || !NewReadinessIndex(pending).UploadReady(item) {
					t.Fatalf("marked turn retained content or gate: %s, %v", item.Event.HookEvent, err)
				}
			}
		})
	}
}

func TestEnqueueFenceSensitiveHookWaitsForSessionLock(t *testing.T) {
	setupFenceCapture(t, ModeRaw)
	prompt := fenceTestPromptAndTool(t, false)
	release, err := acquireSessionStateLock(RuntimeCodex, "delegated")
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(release) }
	t.Cleanup(unblock)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := EnqueueHook(RuntimeCodex, []byte(`{"session_id":"delegated","hook_event_name":"PreToolUse","tool_name":"mcp__witself__witself_secret_reveal","tool_use_id":"sealed-1","tool_input":{"value":"sealed-fence-canary"},"transcript_path":"/tmp/delegated-rollout.jsonl"}`))
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("sealed hook did not wait for the session lock: %v", err)
	case <-time.After(2100 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sealed hook did not resume after session unlock")
	}
	if _, _, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, ""); err != nil {
		t.Fatal(err)
	}
	pending, err := Pending(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pending {
		raw, err := os.ReadFile(item.Path)
		if err != nil || bytes.Contains(raw, []byte("fence-canary")) || !NewReadinessIndex(pending).UploadReady(item) {
			t.Fatalf("lock contention lost sealed suppression: %s, %v", item.Event.HookEvent, err)
		}
	}
}
