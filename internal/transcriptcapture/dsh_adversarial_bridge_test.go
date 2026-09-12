package transcriptcapture

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func dshBridgeMessage(id, text, kind string) map[string]any {
	return map[string]any{
		"id": id, "role": "user", "source": map[string]any{"kind": kind},
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
}

func dshBridgeClaim(log *dshLogBuilder, target, id, text, kind string, turn int) *dshLogBuilder {
	return log.event("agent/inbox/spliced", map[string]any{
		"target": target, "start": 0, "inserted": []any{dshBridgeMessage(id, text, kind)},
	}).event("agent/inbox/spliced", map[string]any{
		"target": target, "start": 0, "removedCount": 1, "inserted": []any{},
	}).event("hook/invoked", map[string]any{
		"turn": turn, "point": "UserPromptSubmit", "dialect": "claude-code", "handlerId": "claude-code:UserPromptSubmit:" + id,
	})
}

// The generated native records use an explicit fixture clock. Put the local
// sealing observation on that same clock rather than using wall-clock sleeps.
func dshSetFixtureSealedTime(t *testing.T, session string, unixMilli int64) {
	t.Helper()
	state, err := loadSessionState(RuntimeDSH, session)
	if err != nil {
		t.Fatal("load fixture sealed state")
	}
	state.DSHSealedAtUnixMilli = unixMilli
	if saveSessionState(RuntimeDSH, session, state) != nil {
		t.Fatal("save fixture sealed clock")
	}
}

func TestDSHBridgeDelayedSessionStartKeepsPromptRun(t *testing.T) {
	for _, source := range []string{"startup", "resume"} {
		t.Run(source, func(t *testing.T) {
			dshTestCaptureConfig(t, ModeTrace)
			const session, cwd = "session-delayed-start", "/tmp/project"
			log := dshBridgeClaim(newDSHLog(session, cwd).event("turn/start", map[string]any{"turn": 1}), "next-turn", "first", "first prompt", "user", 1)
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			prompt := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "first prompt"})
			start := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "SessionStart", "source": source})
			stop := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
			if start.RunID != prompt.RunID || stop.RunID != prompt.RunID || stop.TurnID != prompt.TurnID {
				t.Fatal("detached SessionStart split the first prompt from its Stop")
			}
		})
	}
}

func TestDSHBridgePluginPromptPreservesSealedTurn(t *testing.T) {
	dshTestCaptureConfig(t, ModeTrace)
	const session, cwd, canary = "session-plugin-sealed", "/tmp/project", "dsh-plugin-sealed-canary"
	log := dshBridgeClaim(newDSHLog(session, cwd).event("turn/start", map[string]any{"turn": 1}), "next-turn", "first", "first prompt", "user", 1)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	prompt := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "first prompt"})
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "PreToolUse", "tool_name": "mcp__witself__witself_secret_reveal", "tool_use_id": "sealed"})
	log.event("step/start", map[string]any{"turn": 1, "step": 1}).prompt("first prompt").toolCall(1, 1, "sealed", "mcp__witself__witself_secret_reveal", "{}").toolResult(1, 1, "sealed", canary).event("step/end", map[string]any{"turn": 1, "step": 1})
	dshBridgeClaim(log, "next-step", "notice", "job finished", "plugin", 1)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	continuation := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "job finished"})
	sink := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "PostToolUse", "tool_name": "bash", "tool_use_id": "sink", "tool_response": canary})
	stop := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
	if strings.Contains(sink.Body, canary) || !eventSealedContentOmitted(stop.Data) {
		t.Fatal("plugin pre-step prompt cleared the sealed turn and retained the sink value")
	}
	if continuation.TurnID != prompt.TurnID {
		t.Fatal("plugin pre-step prompt created a second Witself turn")
	}
}

func TestDSHBridgeChildSealedResultDoesNotLeakThroughParent(t *testing.T) {
	dshTestCaptureConfig(t, ModeTrace)
	const parent, child, cwd, canary = "session-relay-parent", "session-relay-child", "/tmp/project", "dsh-child-relay-canary"
	parentLog := dshBridgeClaim(newDSHLog(parent, cwd).event("turn/start", map[string]any{"turn": 1}), "next-turn", "parent-first", "delegate", "user", 1)
	writeDSHSessionLog(t, cwd, parent, "session.v3.jsonl", parentLog.plain())
	enqueueDSHHook(t, map[string]any{"session_id": parent, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "delegate"})
	enqueueDSHHook(t, map[string]any{"session_id": parent, "cwd": cwd, "hook_event_name": "PreToolUse", "tool_name": "subagent", "tool_use_id": "delegation", "tool_input": map[string]any{"prompt": "perform authorized reveal", "run_in_background": false}})
	childLog := newDSHLog(child, cwd)
	var header map[string]any
	if err := json.Unmarshal([]byte(childLog.lines[0]), &header); err != nil {
		t.Fatal("decode generated header")
	}
	header["parentSession"], header["delegationDepth"] = parent, 1
	childLog.lines[0] = mustDSHJSON(header)
	dshBridgeClaim(childLog.event("turn/start", map[string]any{"turn": 1}), "next-turn", "child-first", "perform authorized reveal", "user", 1)
	writeDSHSessionLog(t, cwd, child, "session.v3.jsonl", childLog.plain())
	enqueueDSHHook(t, map[string]any{"session_id": child, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "perform authorized reveal"})
	enqueueDSHHook(t, map[string]any{"session_id": child, "cwd": cwd, "hook_event_name": "PreToolUse", "tool_name": "mcp__witself__witself_secret_reveal", "tool_use_id": "child-secret"})
	enqueueDSHHook(t, map[string]any{"session_id": child, "cwd": cwd, "hook_event_name": "Stop"})
	result := enqueueDSHHook(t, map[string]any{"session_id": parent, "cwd": cwd, "hook_event_name": "PostToolUse", "tool_name": "subagent", "tool_use_id": "delegation", "tool_response": canary})
	stop := enqueueDSHHook(t, map[string]any{"session_id": parent, "cwd": cwd, "hook_event_name": "Stop"})
	if strings.Contains(result.Body, canary) || !eventSealedContentOmitted(stop.Data) {
		t.Fatal("parent subagent result retained a value sealed in the child session")
	}
}

func TestDSHBridgeTwoStopsKeepSeparateSegments(t *testing.T) {
	dshTestCaptureConfig(t, ModeTrace)
	const session, cwd = "session-two-stops", "/tmp/project"
	log := dshBridgeClaim(newDSHLog(session, cwd).event("turn/start", map[string]any{"turn": 1}), "next-turn", "first", "first prompt", "user", 1)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "first prompt"})
	log.event("step/start", map[string]any{"turn": 1, "step": 1}).prompt("first prompt").assistant(1, 1, "first answer", "deepseek-flash", map[string]any{"inputTokens": 11, "outputTokens": 3}).event("step/end", map[string]any{"turn": 1, "step": 1}).event("hook/invoked", map[string]any{"turn": 1, "point": "Stop", "dialect": "claude-code", "handlerId": "first-stop"})
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	first := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
	dshBridgeClaim(log, "next-step", "second", "second prompt", "user", 1)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "second prompt"})
	log.event("step/start", map[string]any{"turn": 1, "step": 2}).prompt("second prompt").assistant(1, 2, "second answer", "deepseek-flash", map[string]any{"inputTokens": 17, "outputTokens": 5}).event("step/end", map[string]any{"turn": 1, "step": 2}).event("hook/invoked", map[string]any{"turn": 1, "point": "Stop", "dialect": "claude-code", "handlerId": "second-stop"})
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	second := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
	log.event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	for _, item := range []struct {
		event Event
		body  string
		input int64
	}{{first, "first answer", 11}, {second, "second answer", 17}} {
		finalized, ready, err := finalizePendingWithin(findPendingDSHEvent(t, item.event.ID), time.Millisecond, time.Millisecond)
		if err != nil || !ready {
			t.Fatal("completed native turn did not finalize")
		}
		var data struct {
			Usage struct {
				Input int64 `json:"input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(finalized.Event.Data, &data) != nil || finalized.Event.Body != item.body || data.Usage.Input != item.input || len(finalized.Event.RecoveredMessages) != 0 {
			t.Fatal("multiple Stop events duplicated the native turn's final answer, usage, or intermediate content")
		}
	}
}

func TestDSHPromptProvenanceUsesClaimedSources(t *testing.T) {
	for _, kind := range []string{"user", "plugin", "goal", "agent-instructions"} {
		t.Run(kind, func(t *testing.T) {
			dshTestCaptureConfig(t, ModeTrace)
			const session, cwd = "session-provenance", "/tmp/project"
			log := dshBridgeClaim(newDSHLog(session, cwd).event("turn/start", map[string]any{"turn": 1}), "next-turn", "message", "input", kind, 1)
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			user, known, hookSeq := dshPromptProvenance(session, cwd, "input", 0)
			if !known || (kind == "user" && user != "input") || (kind != "user" && user != "") {
				t.Fatal("claimed source was not preserved")
			}
			if _, known, _ := dshPromptProvenance(session, cwd, "input", 0, hookSeq); known {
				t.Fatal("a previously observed hook supplied provenance for a repeated prompt")
			}
			if kind == "user" {
				invokedAt := int64(1700000000000) + hookSeq
				for _, cutoff := range []int64{invokedAt, invokedAt + 1} {
					if _, known, _ := dshPromptProvenance(session, cwd, "input", 0, 0, cutoff); known {
						t.Fatal("an equal or earlier native invocation granted user provenance after sealing")
					}
				}
				if _, known, _ := dshPromptProvenance(session, cwd, "input", 0, 0, invokedAt-1); !known {
					t.Fatal("a later native invocation lost its user provenance")
				}
			}
			if _, known, _ := dshPromptProvenance(session, cwd, "different input", 0); known {
				t.Fatal("an unmatched native hook supplied provenance")
			}
			log.event("hook/result", map[string]any{"turn": 1, "point": "UserPromptSubmit", "handlerId": "claude-code:UserPromptSubmit:message"})
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			if _, known, _ := dshPromptProvenance(session, cwd, "input", 0); known {
				t.Fatal("a completed hook supplied current provenance")
			}
		})
	}
}

func TestDSHPromptProvenanceHandlesMixedAndCanceledInbox(t *testing.T) {
	dshTestCaptureConfig(t, ModeTrace)
	const session, cwd = "session-provenance-mixed", "/tmp/project"
	log := newDSHLog(session, cwd).
		event("agent/inbox/spliced", map[string]any{"target": "next-turn", "start": 0, "inserted": []any{dshBridgeMessage("canceled", "discarded", "user"), dshBridgeMessage("real", "user input", "user")}}).
		event("agent/inbox/spliced", map[string]any{"target": "next-turn", "start": 0, "removedCount": 1, "inserted": []any{}, "outcome": "canceled"}).
		event("agent/inbox/spliced", map[string]any{"target": "next-step", "start": 0, "inserted": []any{dshBridgeMessage("plugin", "context ", "plugin")}}).
		event("turn/start", map[string]any{"turn": 1}).
		event("agent/inbox/spliced", map[string]any{"target": "next-step", "start": 0, "removedCount": 1, "inserted": []any{}}).
		event("agent/inbox/spliced", map[string]any{"target": "next-turn", "start": 0, "removedCount": 1, "inserted": []any{}}).
		event("hook/invoked", map[string]any{"turn": 1, "point": "UserPromptSubmit", "dialect": "claude-code", "handlerId": "other-handler"}).
		event("hook/result", map[string]any{"turn": 1, "point": "UserPromptSubmit", "handlerId": "other-handler"}).
		event("hook/invoked", map[string]any{"turn": 1, "point": "UserPromptSubmit", "dialect": "claude-code", "handlerId": "witself-handler"})
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	user, known, _ := dshPromptProvenance(session, cwd, "context user input", 0)
	if !known || user != "user input" {
		t.Fatal("mixed inbox batch lost user text or included canceled/plugin text")
	}
	log.event("agent/inbox/spliced", map[string]any{"target": "next-step", "start": 99, "removedCount": 1, "inserted": []any{}})
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	if _, known, _ := dshPromptProvenance(session, cwd, "context user input", 0); known {
		t.Fatal("malformed inbox splice granted provenance")
	}
}

func TestDSHDelegationConservativelyOmitsSafeAnswers(t *testing.T) {
	for _, tool := range []string{"subagent", "subagent_fork", "str_replace_editor"} {
		t.Run(tool, func(t *testing.T) {
			dshTestCaptureConfig(t, ModeTrace)
			const session, cwd = "session-safe-delegation", "/tmp/project"
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", newDSHLog(session, cwd).plain())
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "run a safe task"})
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "PreToolUse", "tool_name": tool, "tool_use_id": "call"})
			result := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "PostToolUse", "tool_name": tool, "tool_use_id": "call", "tool_response": "ordinary safe result"})
			stop := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
			delegation := tool != "str_replace_editor"
			if eventSealedContentOmitted(stop.Data) != delegation || strings.Contains(result.Body, "ordinary safe result") == delegation {
				t.Fatal("delegation omission rule changed an ordinary tool or exposed a delegated result")
			}
		})
	}
}

func TestDSHSealedFenceRequiresUserSourceAcrossNativeTurns(t *testing.T) {
	for _, kind := range []string{"plugin", "goal", "user", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			dshTestCaptureConfig(t, ModeTrace)
			const session, cwd, canary = "session-sealed-next-turn", "/tmp/project", "dsh-next-turn-sealed-canary"
			log := newDSHLog(session, cwd)
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "SessionStart", "source": "startup"})
			dshBridgeClaim(log.event("turn/start", map[string]any{"turn": 1}), "next-turn", "first", "authorized reveal", "user", 1)
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "authorized reveal"})
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "PreToolUse", "tool_name": "mcp__witself__witself_secret_reveal", "tool_use_id": "reveal"})
			dshSetFixtureSealedTime(t, session, 1700000000000+int64(log.seq)+1)
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
			log.event("step/start", map[string]any{"turn": 1, "step": 1}).prompt("authorized reveal").toolCall(1, 1, "reveal", "mcp__witself__witself_secret_reveal", "{}").toolResult(1, 1, "reveal", canary).event("step/end", map[string]any{"turn": 1, "step": 1}).event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
			log.event("turn/start", map[string]any{"turn": 2})
			if kind != "unknown" {
				dshBridgeClaim(log, "next-turn", "next", "continue", kind, 2)
			}
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "continue"})
			result := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "PostToolUse", "tool_name": "bash", "tool_use_id": "sink", "tool_response": canary})
			stop := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
			sealed := kind != "user"
			if eventSealedContentOmitted(stop.Data) != sealed || strings.Contains(result.Body, canary) == sealed {
				t.Fatal("native turn boundary replaced the required user-source fence")
			}
		})
	}
}

func TestDSHSealedFenceRejectsUnobservedStalePromptPrefix(t *testing.T) {
	dshTestCaptureConfig(t, ModeTrace)
	const session, cwd, canary = "session-unobserved-stale", "/tmp/project", "dsh-stale-source-canary"
	log := newDSHLog(session, cwd)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "SessionStart", "source": "startup"})
	// The first prompt's native invocation is still in the asynchronous
	// persistence queue, so this hook cannot remember its native sequence.
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "repeated text"})
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "PreToolUse", "tool_name": "mcp__witself__witself_secret_reveal", "tool_use_id": "sealed"})
	dshSetFixtureSealedTime(t, session, 1700000000010)
	// An older drained batch can expose only the first user hook's unpaired
	// prefix while newer result, tool, and plugin-claim records remain queued.
	dshBridgeClaim(log.event("turn/start", map[string]any{"turn": 1}), "next-turn", "old", "repeated text", "user", 1)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "repeated text"})
	result := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "PostToolUse", "tool_name": "bash", "tool_use_id": "sink", "tool_response": canary})
	stop := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
	if strings.Contains(result.Body, canary) || !eventSealedContentOmitted(stop.Data) {
		t.Fatal("an unobserved stale prompt prefix cleared the sealed fence")
	}
}

func TestDSHBridgeDelayedResumeAfterPriorStartKeepsSealedTurn(t *testing.T) {
	dshTestCaptureConfig(t, ModeTrace)
	const session, cwd, canary = "session-delayed-resume", "/tmp/project", "dsh-resume-sealed-canary"
	log := newDSHLog(session, cwd)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "SessionStart", "source": "startup"})
	dshBridgeClaim(log.event("turn/start", map[string]any{"turn": 1}), "next-turn", "old", "old prompt", "user", 1)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "old prompt"})
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
	log.event("step/start", map[string]any{"turn": 1, "step": 1}).prompt("old prompt").assistant(1, 1, "old answer", "deepseek-flash", nil).event("step/end", map[string]any{"turn": 1, "step": 1}).event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	dshBridgeClaim(log.event("turn/start", map[string]any{"turn": 2}), "next-turn", "new", "new prompt", "user", 2)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	prompt := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "new prompt"})
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "PreToolUse", "tool_name": "mcp__witself__witself_secret_reveal", "tool_use_id": "reveal"})
	start := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "SessionStart", "source": "resume"})
	sink := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "PostToolUse", "tool_name": "bash", "tool_use_id": "sink", "tool_response": canary})
	stop := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
	if start.RunID != prompt.RunID || stop.TurnID != prompt.TurnID || strings.Contains(sink.Body, canary) || !eventSealedContentOmitted(stop.Data) {
		t.Fatal("a delayed later resume split or unsealed the open prompt")
	}
}

func TestDSHBridgePluginContinuationSeparatesStopsWithMultipleHandlers(t *testing.T) {
	dshTestCaptureConfig(t, ModeTrace)
	const session, cwd = "session-plugin-two-stops", "/tmp/project"
	log := dshBridgeClaim(newDSHLog(session, cwd).event("turn/start", map[string]any{"turn": 1}), "next-turn", "first", "first prompt", "user", 1)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "first prompt"})
	log.event("step/start", map[string]any{"turn": 1, "step": 1}).prompt("first prompt").assistant(1, 1, "first answer", "deepseek-flash", map[string]any{"inputTokens": 11, "outputTokens": 3}).event("step/end", map[string]any{"turn": 1, "step": 1})
	// Every matching command hook gets its own invocation marker; these two
	// handlers represent a single physical Stop for step 1.
	log.event("hook/invoked", map[string]any{"turn": 1, "point": "Stop", "dialect": "claude-code", "handlerId": "foreign-first"}).event("hook/result", map[string]any{"turn": 1, "point": "Stop", "handlerId": "foreign-first"}).event("hook/invoked", map[string]any{"turn": 1, "point": "Stop", "dialect": "claude-code", "handlerId": "witself-first"})
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	first := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
	log.event("hook/result", map[string]any{"turn": 1, "point": "Stop", "handlerId": "witself-first"})
	// A denied Stop hook can steer this plugin message before the native
	// loop exits, so no new user-sourced prompt or native turn is created.
	dshBridgeClaim(log, "next-step", "continuation", "continue after hook denial", "plugin", 1)
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "UserPromptSubmit", "prompt": "continue after hook denial"})
	log.event("step/start", map[string]any{"turn": 1, "step": 2}).injected("continue after hook denial").assistant(1, 2, "second answer", "deepseek-flash", map[string]any{"inputTokens": 17, "outputTokens": 5}).event("step/end", map[string]any{"turn": 1, "step": 2}).event("hook/invoked", map[string]any{"turn": 1, "point": "Stop", "dialect": "claude-code", "handlerId": "foreign-second"}).event("hook/result", map[string]any{"turn": 1, "point": "Stop", "handlerId": "foreign-second"}).event("hook/invoked", map[string]any{"turn": 1, "point": "Stop", "dialect": "claude-code", "handlerId": "witself-second"})
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	second := enqueueDSHHook(t, map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop"})
	log.event("hook/result", map[string]any{"turn": 1, "point": "Stop", "handlerId": "witself-second"}).event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	for _, item := range []struct {
		event Event
		body  string
		input int64
	}{{first, "first answer", 11}, {second, "second answer", 17}} {
		finalized, ready, err := finalizePendingWithin(findPendingDSHEvent(t, item.event.ID), time.Millisecond, time.Millisecond)
		if err != nil || !ready {
			t.Fatal("completed native plugin continuation did not finalize")
		}
		var data struct {
			Usage struct {
				Input int64 `json:"input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(finalized.Event.Data, &data) != nil || finalized.Event.Body != item.body || data.Usage.Input != item.input || len(finalized.Event.RecoveredMessages) != 0 {
			t.Fatal("plugin continuation or multiple hook handlers duplicated a Stop segment")
		}
	}
}
