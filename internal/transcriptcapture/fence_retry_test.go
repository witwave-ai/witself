package transcriptcapture

import (
	"bytes"
	"maps"
	"os"
	"testing"
)

type fenceRetrySnapshot struct {
	state  []byte
	outbox map[string]string
}

func snapshotFenceRetry(t *testing.T, sessionID string) fenceRetrySnapshot {
	t.Helper()
	path, err := sessionStatePath(RuntimeCodex, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Pending(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := fenceRetrySnapshot{state: state, outbox: make(map[string]string, len(pending))}
	for _, item := range pending {
		raw, err := os.ReadFile(item.Path)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.outbox[item.Path] = string(raw)
	}
	return snapshot
}

func assertFenceRetryUnchanged(t *testing.T, sessionID string, before fenceRetrySnapshot) {
	t.Helper()
	after := snapshotFenceRetry(t, sessionID)
	if !bytes.Equal(before.state, after.state) || !maps.Equal(before.outbox, after.outbox) {
		t.Fatal("completion retry changed the session state or outbox")
	}
}

func TestEnqueueFenceDelayedRetryCannotReleaseResumedSensitiveTurn(t *testing.T) {
	for _, mode := range []string{ModeRaw, ModeMessages} {
		for _, uploaded := range []bool{false, true} {
			name := mode + "/pending"
			if uploaded {
				name = mode + "/uploaded"
			}
			t.Run(name, func(t *testing.T) {
				setupFenceCapture(t, mode)
				first := enqueueTestHook(t, RuntimeCodex, `{"session_id":"s","transcript_path":"/tmp/s.jsonl","hook_event_name":"UserPromptSubmit","prompt":"A"}`)
				if _, created, err := EnqueueFence(RuntimeCodex, "s", first.RunID, first.TurnID, "job-completed"); err != nil || !created {
					t.Fatalf("first completion: created=%t, error=%v", created, err)
				}
				if uploaded {
					pending, err := Pending(RuntimeCodex)
					if err != nil {
						t.Fatal(err)
					}
					for _, item := range pending {
						if err := RemovePending(item.Path); err != nil {
							t.Fatal(err)
						}
					}
				}
				resumed := enqueueTestHook(t, RuntimeCodex, `{"session_id":"s","transcript_path":"/tmp/s.jsonl","hook_event_name":"UserPromptSubmit","prompt":"SECRET_CANARY"}`)
				before := snapshotFenceRetry(t, "s")
				if _, created, err := EnqueueFence(RuntimeCodex, "s", first.RunID, first.TurnID, "job-completed"); err != nil || created {
					t.Fatalf("delayed completion: created=%t, error=%v", created, err)
				}
				assertFenceRetryUnchanged(t, "s", before)
				pending, err := Pending(RuntimeCodex)
				if err != nil {
					t.Fatal(err)
				}
				index := NewReadinessIndex(pending)
				for _, item := range pending {
					if item.Event.TurnID == resumed.TurnID && index.UploadReady(item) {
						t.Fatal("delayed completion released the new prompt before its sealed hook")
					}
				}
				// No turn_id: the sealed hook must inherit the still-open new turn.
				enqueueTestHook(t, RuntimeCodex, `{"session_id":"s","transcript_path":"/tmp/s.jsonl","hook_event_name":"PreToolUse","tool_name":"mcp__witself__witself_secret_reveal"}`)
				pending, err = Pending(RuntimeCodex)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range pending {
					raw, err := os.ReadFile(item.Path)
					if err != nil || bytes.Contains(raw, []byte("SECRET_CANARY")) {
						t.Fatalf("sealed hook failed to redact the resumed turn: %v", err)
					}
				}
				state, err := loadSessionState(RuntimeCodex, "s")
				if err != nil || state.TurnID != resumed.TurnID || !state.SensitiveTurn {
					t.Fatalf("delayed completion lost the resumed sealed-turn identity: %v", err)
				}
				if _, created, err := EnqueueFence(RuntimeCodex, "s", resumed.RunID, resumed.TurnID, "job-completed"); err != nil || !created {
					t.Fatalf("resumed completion: created=%t, error=%v", created, err)
				}
				pending, err = Pending(RuntimeCodex)
				if err != nil {
					t.Fatal(err)
				}
				index = NewReadinessIndex(pending)
				for _, item := range pending {
					if item.Event.TurnID == resumed.TurnID && (!index.UploadReady(item) || !eventSealedContentOmitted(item.Event.Data)) {
						t.Fatal("resumed completion did not release only redacted content")
					}
				}
				// Once another completion replaces the retained marker, an older
				// callback is stale rather than a completion of that newer turn.
				before = snapshotFenceRetry(t, "s")
				if _, created, err := EnqueueFence(RuntimeCodex, "s", first.RunID, first.TurnID, "job-completed"); err == nil || created {
					t.Fatalf("older completion was not refused: created=%t, error=%v", created, err)
				}
				assertFenceRetryUnchanged(t, "s", before)
			})
		}
	}
}

func TestEnqueueFenceRejectsMissingAndStaleIdentitiesWithoutMutation(t *testing.T) {
	for _, scenario := range []string{"missing-run", "missing-turn", "uncompleted-old-turn", "previous-run-reused-turn"} {
		t.Run(scenario, func(t *testing.T) {
			setupFenceCapture(t, ModeRaw)
			first := enqueueTestHook(t, RuntimeCodex, `{"session_id":"s","transcript_path":"/tmp/s.jsonl","hook_event_name":"UserPromptSubmit","prompt":"A","turn_id":"same-native-turn"}`)
			expectedRun, expectedTurn := first.RunID, first.TurnID
			switch scenario {
			case "missing-run":
				expectedRun = ""
			case "missing-turn":
				expectedTurn = ""
			case "uncompleted-old-turn":
				enqueueTestHook(t, RuntimeCodex, `{"session_id":"s","transcript_path":"/tmp/s.jsonl","hook_event_name":"UserPromptSubmit","prompt":"B"}`)
			case "previous-run-reused-turn":
				if _, _, err := EnqueueFence(RuntimeCodex, "s", first.RunID, first.TurnID, ""); err != nil {
					t.Fatal(err)
				}
				enqueueTestHook(t, RuntimeCodex, `{"session_id":"s","transcript_path":"/tmp/s.jsonl","hook_event_name":"SessionStart"}`)
				enqueueTestHook(t, RuntimeCodex, `{"session_id":"s","transcript_path":"/tmp/s.jsonl","hook_event_name":"UserPromptSubmit","prompt":"B","turn_id":"same-native-turn"}`)
			}
			before := snapshotFenceRetry(t, "s")
			if _, created, err := EnqueueFence(RuntimeCodex, "s", expectedRun, expectedTurn, ""); err == nil || created {
				t.Fatalf("invalid completion accepted: created=%t, error=%v", created, err)
			}
			assertFenceRetryUnchanged(t, "s", before)
		})
	}
}

func TestEnqueueFenceRejectsStaleIdentityBeforePendingReplay(t *testing.T) {
	setupFenceCapture(t, ModeRaw)
	prompt := fenceTestPromptAndTool(t, false)
	fence, _, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "")
	if err != nil {
		t.Fatal(err)
	}
	state, err := loadSessionState(RuntimeCodex, "delegated")
	if err != nil {
		t.Fatal(err)
	}
	state.PendingFence = &fence
	if err := saveSessionState(RuntimeCodex, "delegated", state); err != nil {
		t.Fatal(err)
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
	for _, identity := range [][2]string{{"wrong-run", prompt.TurnID}, {prompt.RunID, "wrong-turn"}} {
		before := snapshotFenceRetry(t, "delegated")
		if _, created, err := EnqueueFence(RuntimeCodex, "delegated", identity[0], identity[1], ""); err == nil || created {
			t.Fatalf("stale completion replayed the pending fence: created=%t, error=%v", created, err)
		}
		assertFenceRetryUnchanged(t, "delegated", before)
	}
	replayed, created, err := EnqueueFence(RuntimeCodex, "delegated", prompt.RunID, prompt.TurnID, "")
	if err != nil || !created || replayed.ID != fence.ID || !replayed.OccurredAt.Equal(fence.OccurredAt) {
		t.Fatalf("exact completion did not replay its original fence: created=%t, error=%v", created, err)
	}
}
