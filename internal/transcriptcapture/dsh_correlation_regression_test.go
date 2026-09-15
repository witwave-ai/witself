package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"
)

func appendDSHCompletedPrompt(log *dshLogBuilder, turn int, prompt, answer string, input int) *dshLogBuilder {
	return log.event("turn/start", map[string]any{"turn": turn}).
		event("step/start", map[string]any{"turn": turn, "step": 1}).prompt(prompt).
		assistant(turn, 1, answer, "deepseek-flash", map[string]any{"inputTokens": input, "outputTokens": input / 2}).
		event("step/end", map[string]any{"turn": turn, "step": 1}).
		event("turn/end", map[string]any{"turn": turn, "reason": map[string]any{"kind": "completed"}})
}

func TestDSHResumedFreshStateWaitsForMatchingFourthTurn(t *testing.T) {
	for _, mode := range []string{ModeTrace, ModeMessages} {
		for _, sessionStart := range []bool{true, false} {
			t.Run(mode+"/session-start="+strconv.FormatBool(sessionStart), func(t *testing.T) {
				dshTestCaptureConfig(t, mode)
				const session, cwd = "session-resume-correlation", "/tmp/project"
				log := newDSHLog(session, cwd)
				for turn := 1; turn <= 3; turn++ {
					appendDSHCompletedPrompt(log, turn, "old prompt "+strconv.Itoa(turn), "old answer", turn*10)
				}
				path := writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
				if sessionStart {
					enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "SessionStart", "source": "resume"})
				}
				prompt := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "fourth prompt"})
				log.event("turn/start", map[string]any{"turn": 4}).
					event("step/start", map[string]any{"turn": 4, "step": 1}).prompt("fourth prompt").
					assistant(4, 1, "fourth answer", "deepseek-flash", map[string]any{"inputTokens": 440, "outputTokens": 44}).
					event("step/end", map[string]any{"turn": 4, "step": 1})
				if err := os.WriteFile(path, log.plain(), 0o600); err != nil {
					t.Fatal(err)
				}
				stop := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
				var localData struct {
					Ordinal int    `json:"dsh_turn_ordinal"`
					Digest  string `json:"dsh_prompt_sha256"`
				}
				if json.Unmarshal(stop.Data, &localData) != nil || localData.Digest != dshPromptSHA256("fourth prompt") {
					t.Fatal("Stop lacks the private prompt correlation")
				}
				if (sessionStart && localData.Ordinal != 4) || (!sessionStart && localData.Ordinal != 0) {
					t.Fatal("Stop ordinal is not anchored to the observed SessionStart")
				}
				pending := findPendingDSHEvent(t, stop.ID)
				if _, ready, err := finalizePendingWithin(pending, 0, time.Millisecond); err != nil || ready {
					t.Fatal("Stop finalized before the fourth native turn ended")
				}
				assertDSHDigestStaysLocal(t, stop, localData.Digest)
				log.event("turn/end", map[string]any{"turn": 4, "reason": map[string]any{"kind": "completed"}})
				if err := os.WriteFile(path, log.plain(), 0o600); err != nil {
					t.Fatal(err)
				}
				final, ready, err := finalizePendingWithin(pending, 0, time.Millisecond)
				if err != nil || !ready || final.Event.Body != "fourth answer" || final.Event.Model != "deepseek-flash" || final.Event.TurnID != prompt.TurnID {
					t.Fatal("Stop did not finalize from its fourth prompt")
				}
				var usage struct {
					Usage struct {
						Input  int `json:"input_tokens"`
						Output int `json:"output_tokens"`
					} `json:"usage"`
				}
				if json.Unmarshal(final.Event.Data, &usage) != nil || usage.Usage.Input != 440 || usage.Usage.Output != 44 {
					t.Fatal("Stop borrowed accounting from a historical turn")
				}
				if bytes.Contains(final.Event.Data, []byte("dsh_prompt_sha256")) || len(final.Event.RecoveredMessages) != 0 {
					t.Fatal("finalization retained private correlation or replayed history")
				}
				assertDSHDigestStaysLocal(t, final.Event, localData.Digest)
			})
		}
	}
}

func assertDSHDigestStaysLocal(t *testing.T, event Event, digest string) {
	t.Helper()
	encoded, err := json.Marshal(event.Entries())
	if err != nil || bytes.Contains(encoded, []byte("dsh_prompt_sha256")) || bytes.Contains(encoded, []byte(digest)) {
		t.Fatal("private prompt digest reached an upload entry")
	}
}

func TestDSHSealingClearsPrivatePromptDigest(t *testing.T) {
	dshTestCaptureConfig(t, ModeRaw)
	const session = "session-private-digest"
	enqueueDSHHook(t, map[string]any{"session_id": session, "hook_event_name": "UserPromptSubmit", "prompt": "authorize one sealed operation"})
	enqueueDSHHook(t, map[string]any{"session_id": session, "hook_event_name": "PreToolUse", "tool_name": "mcp__witself__witself_secret_reveal", "tool_use_id": "sealed"})
	stop := enqueueDSHHook(t, map[string]any{"session_id": session, "hook_event_name": "Stop"})
	state, err := loadSessionState(RuntimeDSH, session)
	if err != nil || state.DSHPromptSHA256 != "" || bytes.Contains(stop.Data, []byte("dsh_prompt_sha256")) {
		t.Fatal("sealed turn retained prompt correlation digest")
	}
}

func TestDSHRecoveryDeduplicatesEachToolPhase(t *testing.T) {
	for _, hooks := range [][]string{{"PreToolUse"}, {"PostToolUse"}, {"PreToolUse", "PostToolUse"}} {
		t.Run(strconv.Itoa(len(hooks))+"/"+hooks[0], func(t *testing.T) {
			dshTestCaptureConfig(t, ModeTrace)
			const session, cwd = "session-phase-recovery", "/tmp/project"
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", newDSHLog(session, cwd).plain())
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "inspect file"})
			for _, hook := range hooks {
				enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": hook, "tool_use_id": "call", "tool_name": "bash", "tool_response": "result"})
			}
			stop := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
			log := newDSHLog(session, cwd).event("turn/start", map[string]any{"turn": 1}).
				event("step/start", map[string]any{"turn": 1, "step": 1}).prompt("inspect file").
				assistant(1, 1, "checking", "deepseek-flash", nil).
				toolCall(1, 1, "call", "bash", "{}").toolResult(1, 1, "call", "result").
				event("step/end", map[string]any{"turn": 1, "step": 1}).
				event("step/start", map[string]any{"turn": 1, "step": 2}).assistant(1, 2, "done", "deepseek-flash", nil).
				event("step/end", map[string]any{"turn": 1, "step": 2}).
				event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			final, ready, err := finalizePendingWithin(findPendingDSHEvent(t, stop.ID), 0, time.Millisecond)
			if err != nil || !ready {
				t.Fatal("phase recovery did not finalize")
			}
			counts := map[string]int{}
			for _, hook := range hooks {
				counts[hook]++
			}
			for _, recovered := range final.Event.RecoveredMessages {
				counts[recovered.HookEvent]++
			}
			if counts["PreToolUse"] != 1 || counts["PostToolUse"] != 1 {
				t.Fatal("missing or duplicate recovered tool phase")
			}
		})
	}
}
