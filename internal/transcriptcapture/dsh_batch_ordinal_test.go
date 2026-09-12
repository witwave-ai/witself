package transcriptcapture

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestDSHUserBatchThenRepeatedPromptRefreshesNativeOrdinal(t *testing.T) {
	for _, mode := range []string{ModeTrace, ModeMessages} {
		t.Run(mode, func(t *testing.T) {
			dshTestCaptureConfig(t, mode)
			const session, cwd = "session-batched-repeat", "/tmp/project"
			const repeatedPrompt = "repeat this request"
			log := newDSHLog(session, cwd)
			path := writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			persist := func() {
				t.Helper()
				if err := os.WriteFile(path, log.plain(), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "SessionStart", "source": "startup"})
			log.event("turn/start", map[string]any{"turn": 1}).
				event("agent/inbox/spliced", map[string]any{
					"target": "next-turn", "start": 0,
					"inserted": []any{dshBridgeMessage("first-part", "repeat ", "user"), dshBridgeMessage("second-part", "this request", "user")},
				}).
				event("agent/inbox/spliced", map[string]any{"target": "next-turn", "start": 0, "removedCount": 2, "inserted": []any{}}).
				event("hook/invoked", map[string]any{"turn": 1, "point": "UserPromptSubmit", "dialect": "claude-code", "handlerId": "first-batch"})
			persist()
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": repeatedPrompt})
			log.event("step/start", map[string]any{"turn": 1, "step": 1}).
				prompt("repeat ").prompt("this request").
				assistant(1, 1, "old batch answer", "old-model", map[string]any{"inputTokens": 11}).
				event("step/end", map[string]any{"turn": 1, "step": 1}).
				event("hook/invoked", map[string]any{"turn": 1, "point": "Stop", "dialect": "claude-code", "handlerId": "first-stop"})
			persist()
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
			log.event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})

			// The first hook persisted two native user records. This later hook
			// repeats their concatenated text, so digest validation alone cannot
			// distinguish the old group from this newly opened turn.
			dshBridgeClaim(log.event("turn/start", map[string]any{"turn": 2}), "next-turn", "repeated", repeatedPrompt, "user", 2)
			persist()
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": repeatedPrompt})
			log.event("step/start", map[string]any{"turn": 2, "step": 1}).
				prompt(repeatedPrompt).
				assistant(2, 1, "new repeated answer", "new-model", map[string]any{"inputTokens": 37}).
				event("step/end", map[string]any{"turn": 2, "step": 1}).
				event("hook/invoked", map[string]any{"turn": 2, "point": "Stop", "dialect": "claude-code", "handlerId": "second-stop"})
			persist()
			stop := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
			var local struct {
				Ordinal int `json:"dsh_turn_ordinal"`
			}
			if json.Unmarshal(stop.Data, &local) != nil || local.Ordinal != 3 {
				t.Fatal("repeated prompt did not advance past both native records in the previous batch")
			}
			pending := findPendingDSHEvent(t, stop.ID)
			if _, ready, err := finalizePendingWithin(pending, 0, time.Millisecond); err != nil || ready {
				t.Fatal("repeated prompt finalized from its identically worded completed predecessor")
			}
			log.event("turn/end", map[string]any{"turn": 2, "reason": map[string]any{"kind": "completed"}})
			persist()
			finalized, ready, err := finalizePendingWithin(pending, 0, time.Millisecond)
			var finalData struct {
				Usage struct {
					Input int `json:"input_tokens"`
				} `json:"usage"`
			}
			if err != nil || !ready || finalized.Event.Body != "new repeated answer" || finalized.Event.Model != "new-model" ||
				json.Unmarshal(finalized.Event.Data, &finalData) != nil || finalData.Usage.Input != 37 {
				t.Fatal("repeated prompt borrowed the previous batch's answer, model, or accounting")
			}
		})
	}
}
