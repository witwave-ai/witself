package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func dshDelegatedFixture(t *testing.T, session, cwd string, depth int, seeded bool) *dshLogBuilder {
	t.Helper()
	log := newDSHLog(session, cwd)
	var header map[string]any
	if json.Unmarshal([]byte(log.lines[0]), &header) != nil {
		t.Fatal("decode generated session header")
	}
	header["delegationDepth"], header["isSeeded"] = depth, seeded
	log.lines[0] = mustDSHJSON(header)
	return log
}

func TestDSHParentSecretNeverEntersDelegatedChildCapture(t *testing.T) {
	for _, tc := range []struct {
		name   string
		depth  int
		seeded bool
	}{
		{name: "subagent", depth: 1},
		{name: "seeded fork", seeded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dshTestCaptureConfig(t, ModeRaw)
			const parent, child, cwd = "delegating-parent", "delegated-child", "/tmp/project"
			const canary = "synthetic-parent-to-child-private-value"
			parentLog := dshBridgeClaim(newDSHLog(parent, cwd).event("turn/start", map[string]any{"turn": 1}), "next-turn", "parent", "use the authorized secret", "user", 1)
			writeDSHSessionLog(t, cwd, parent, "session.v3.jsonl", parentLog.plain())
			enqueueDSHHook(t, map[string]any{"session_id": parent, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "use the authorized secret"})
			enqueueDSHHook(t, map[string]any{"session_id": parent, "cwd": cwd, "hook_event_name": "PreToolUse", "tool_name": "witself.secret.reveal", "tool_use_id": "reveal"})
			enqueueDSHHook(t, map[string]any{"session_id": parent, "cwd": cwd, "hook_event_name": "PostToolUse", "tool_name": "witself.secret.reveal", "tool_use_id": "reveal", "tool_response": canary})
			enqueueDSHHook(t, map[string]any{"session_id": parent, "cwd": cwd, "hook_event_name": "PreToolUse", "tool_name": "subagent", "tool_use_id": "delegate", "tool_input": map[string]any{"prompt": canary}})

			// The real driver labels a delegated prompt as source:user. The
			// child invokes no sealed tool; only the session header identifies it.
			childLog := dshBridgeClaim(dshDelegatedFixture(t, child, cwd, tc.depth, tc.seeded).event("turn/start", map[string]any{"turn": 1}), "next-turn", "child", canary, "user", 1)
			writeDSHSessionLog(t, cwd, child, "session.v3.jsonl", childLog.plain())
			prompt := enqueueDSHHook(t, map[string]any{"session_id": child, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": canary})
			if !eventSealedContentOmitted(prompt.Data) || prompt.Body != "" {
				t.Fatal("delegated source:user prompt retained parent secret before any child tool")
			}
			childLog.event("step/start", map[string]any{"turn": 1, "step": 1}).prompt(canary).
				assistant(1, 1, canary, "deepseek-v3", map[string]any{"inputTokens": 11, "outputTokens": 3}).
				event("step/end", map[string]any{"turn": 1, "step": 1}).event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
			writeDSHSessionLog(t, cwd, child, "session.v3.jsonl", childLog.plain())
			stop := enqueueDSHHook(t, map[string]any{"session_id": child, "cwd": cwd, "hook_event_name": "Stop"})
			finalized, ready, err := finalizePendingWithin(findPendingDSHEvent(t, stop.ID), time.Millisecond, time.Millisecond)
			if err != nil || !ready || !eventSealedContentOmitted(finalized.Event.Data) {
				t.Fatal("delegated child Stop did not finalize with sealed omission")
			}
			native, err := readCompleteDSHTurnWithin(child, cwd, 1, time.Millisecond, time.Millisecond)
			if err != nil || !native.Complete || !native.Sealed || native.Body != "" || len(native.Steps) != 0 {
				t.Fatal("native recovery exposed delegated child text without a sealed tool")
			}
			// An apparent human boundary inside the child remains inherited;
			// the header, not source:user, is the decisive provenance.
			dshSetFixtureSealedTime(t, child, 1700000000000)
			dshBridgeClaim(childLog.event("turn/start", map[string]any{"turn": 2}), "next-turn", "child-again", canary, "user", 2)
			writeDSHSessionLog(t, cwd, child, "session.v3.jsonl", childLog.plain())
			nextPrompt := enqueueDSHHook(t, map[string]any{"session_id": child, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": canary})
			if !eventSealedContentOmitted(nextPrompt.Data) || nextPrompt.Body != "" {
				t.Fatal("later delegated source:user prompt reopened child capture")
			}
			enqueueDSHHook(t, map[string]any{"session_id": parent, "cwd": cwd, "hook_event_name": "PostToolUse", "tool_name": "subagent", "tool_use_id": "delegate", "tool_response": canary})
			enqueueDSHHook(t, map[string]any{"session_id": parent, "cwd": cwd, "hook_event_name": "Stop"})
			pending, err := Pending(RuntimeDSH)
			if err != nil {
				t.Fatal("read generated capture outbox")
			}
			for _, item := range pending {
				raw, err := json.Marshal(item.Event)
				if err != nil || bytes.Contains(raw, []byte(canary)) {
					t.Fatal("outbox retained delegated secret in a prompt, raw hook, or response")
				}
				entries, err := json.Marshal(item.Event.Entries())
				if err != nil || bytes.Contains(entries, []byte(canary)) {
					t.Fatal("upload entries retained delegated secret")
				}
			}
		})
	}
}

func TestDSHUnknownSessionHeaderOmitsUntilRootIsProven(t *testing.T) {
	dshTestCaptureConfig(t, ModeRaw)
	const session, cwd = "unknown-session-header", "/tmp/project"
	const canary = "synthetic-unpublished-child-prompt"
	prompt := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": canary})
	if prompt.Body != "" || !eventSealedContentOmitted(prompt.Data) {
		t.Fatal("missing header permitted a possibly delegated first prompt")
	}
	if state, err := loadSessionState(RuntimeDSH, session); err != nil || state.DSHSessionCaptureSuppressed {
		t.Fatal("unknown provenance permanently latched the entire session")
	}
	dshSetFixtureSealedTime(t, session, 1700000000000)
	log := dshBridgeClaim(newDSHLog(session, cwd).event("turn/start", map[string]any{"turn": 1}), "next-turn", "safe", "safe root prompt", "user", 1)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	prompt = enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "safe root prompt"})
	if prompt.Body != "safe root prompt" || eventSealedContentOmitted(prompt.Data) {
		t.Fatal("verified root session failed to reopen at a proven human boundary")
	}
}
