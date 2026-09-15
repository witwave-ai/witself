package transcriptcapture

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type dshHashedToolCase struct {
	name, tool, input, response string
}

func dshHashedToolCases() []dshHashedToolCase {
	return []dshHashedToolCase{
		{"direct", "mcp__witself__witself_secret_reveal_0564d00a4f41", `{}`, `{"value":"bridge-value-canary"}`},
		{"wrapper input", "mcp__gateway__call_mcp_tool_a06e8493b426", `{"toolName":"witself.secret.reveal"}`, `{"value":"bridge-value-canary"}`},
		{"wrapper response", "mcp__gateway__call_mcp_tool_a06e8493b426", `{}`, `{"toolName":"witself.secret.reveal","value":"bridge-value-canary"}`},
		{"wrapped bridge name", "mcp__gateway__call_mcp_tool_a06e8493b426", `{"call":{"toolName":"mcp__witself__witself_secret_reveal_0564d00a4f41"}}`, `{"value":"bridge-value-canary"}`},
		{"shell input", "mcp__runner__exec_command_a06e8493b426", `{"command":"witself secret reveal fixture --json"}`, `{"value":"bridge-value-canary"}`},
		{"shell response", "mcp__runner__exec_command_a06e8493b426", `{}`, `{"command":"witself secret reveal fixture --json","value":"bridge-value-canary"}`},
	}
}

// Both hook payload locations can reveal a sealed operation. Its actual
// published identity must survive suppression, while all later copies vanish.
func TestDSHHashedToolHookCaptureSealsPayloadAndCopies(t *testing.T) {
	for _, tc := range dshHashedToolCases() {
		t.Run(tc.name, func(t *testing.T) {
			dshTestCaptureConfig(t, ModeRaw)
			const session = "hashed-hook"
			writeDSHSessionLog(t, "/tmp/project", session, "session.v3.jsonl", newDSHLog(session, "/tmp/project").plain())
			enqueueDSHHook(t, map[string]any{"session_id": session, "hook_event_name": "UserPromptSubmit", "prompt": "read the fixture", "cwd": "/tmp/project"})
			tool := enqueueDSHHook(t, map[string]any{
				"session_id": session, "hook_event_name": "PostToolUse", "cwd": "/tmp/project",
				"tool_name": tc.tool, "tool_use_id": "hashed-call", "tool_input": json.RawMessage(tc.input), "tool_response": json.RawMessage(tc.response),
			})
			if len(tool.Raw) != 0 || !strings.Contains(tool.Body, tc.tool) {
				t.Fatal("hashed tool hook was not suppressed with its published identity intact")
			}
			assertDSHBridgeCanaryAbsent(t, tool)
			enqueueDSHHook(t, map[string]any{"session_id": session, "hook_event_name": "Stop", "cwd": "/tmp/project", "last_assistant_message": "copy bridge-value-canary"})
			pending, err := Pending(RuntimeDSH)
			if err != nil {
				t.Fatal(err)
			}
			assertDSHBridgeCanaryAbsent(t, pending)
		})
	}
}

// Lost tool hooks must use the same names and payload classification when the
// native log restores the Stop, including nested tool-result content.
func TestDSHHashedToolNativeRecoverySealsPayloadAndCopies(t *testing.T) {
	for _, tc := range dshHashedToolCases() {
		t.Run(tc.name, func(t *testing.T) {
			dshTestCaptureConfig(t, ModeTrace)
			const session, cwd = "hashed-recovery", "/tmp/project"
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", newDSHLog(session, cwd).plain())
			enqueueDSHHook(t, map[string]any{"session_id": session, "hook_event_name": "UserPromptSubmit", "prompt": "read the fixture", "cwd": cwd})
			stop := enqueueDSHHook(t, map[string]any{"session_id": session, "hook_event_name": "Stop", "cwd": cwd})
			log := newDSHLog(session, cwd).
				event("turn/start", map[string]any{"turn": 1}).
				event("step/start", map[string]any{"turn": 1, "step": 1}).prompt("read the fixture").
				assistant(1, 1, "reading", "deepseek-v3", nil).
				toolCall(1, 1, "hashed-call", tc.tool, tc.input).toolResult(1, 1, "hashed-call", tc.response).
				event("step/start", map[string]any{"turn": 1, "step": 2}).
				assistant(1, 2, "copy bridge-value-canary", "deepseek-v3", map[string]any{"inputTokens": 20, "outputTokens": 5}).
				toolCall(1, 2, "later-call", "write_file", `{"text":"bridge-value-canary"}`).toolResult(1, 2, "later-call", "copied bridge-value-canary").
				event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
			writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
			finalized, ready, err := finalizePendingWithin(findPendingDSHEvent(t, stop.ID), 100*time.Millisecond, time.Millisecond)
			if err != nil || !ready || !finalized.Event.NativeTurnFinalized {
				t.Fatalf("native recovery did not finalize: ready=%t err=%v", ready, err)
			}
			assertDSHBridgeCanaryAbsent(t, finalized.Event)
			assertDSHBridgeCanaryAbsent(t, finalized.Event.Entries())
		})
	}
}

func assertDSHBridgeCanaryAbsent(t *testing.T, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "bridge-value-canary") {
		t.Fatal("capture retained the generated sealed-value canary")
	}
}

// A digest-shaped suffix has no special meaning in another runtime. Before
// dsh support this ordinary documentation name did not match a sealed suffix.
func TestNonDSHDigestShapedDocumentationToolPreservesCapture(t *testing.T) {
	for _, runtime := range []string{RuntimeCodex, RuntimeClaudeCode, RuntimeCursor, RuntimeGrokBuild, RuntimeCopilot, RuntimeOpenClaw} {
		t.Run(runtime, func(t *testing.T) {
			t.Setenv("DSH_HOME", t.TempDir())
			t.Setenv("WITSELF_HOME", t.TempDir())
			input := hookInput{HookEventName: "PostToolUse", ToolName: "mcp__docs__lookup_witself_secret_reveal_abcdef012345", ToolResponse: json.RawMessage(`"ordinary documentation"`)}
			if err := normalizeHookInput(runtime, &input); err != nil {
				t.Fatal(err)
			}
			state := sessionState{}
			protectSensitiveToolPayload(&input, &state)
			if state.SensitiveTurn || input.SensitiveToolEvent || string(input.ToolResponse) != `"ordinary documentation"` {
				t.Fatal("dsh bridge normalization changed another runtime's sealed-name matching")
			}
		})
	}
	t.Run("codex queued prompt and answer", func(t *testing.T) {
		t.Setenv("DSH_HOME", t.TempDir())
		t.Setenv("WITSELF_HOME", t.TempDir())
		saveTestCaptureConfig(t, RuntimeCodex)
		transcript := filepath.Join(t.TempDir(), "synthetic-codex.jsonl")
		for _, hook := range []map[string]any{
			{"hook_event_name": "UserPromptSubmit", "prompt": "explain the API"},
			{"hook_event_name": "PostToolUse", "tool_name": "mcp__docs__lookup_witself_secret_reveal_abcdef012345", "tool_use_id": "docs-call", "tool_response": "ordinary documentation"},
			{"hook_event_name": "Stop", "last_assistant_message": "API explanation"},
		} {
			hook["session_id"], hook["transcript_path"] = "docs-session", transcript
			enqueueTestHook(t, RuntimeCodex, mustDSHJSON(hook))
		}
		pending, err := Pending(RuntimeCodex)
		if err != nil {
			t.Fatal(err)
		}
		bodies := make(map[string]bool)
		for _, item := range pending {
			bodies[item.Event.Body] = true
			if eventSealedContentOmitted(item.Event.Data) {
				t.Fatal("ordinary documentation caused queued Codex content to be redacted")
			}
		}
		if !bodies["explain the API"] || !bodies["API explanation"] {
			t.Fatal("ordinary Codex prompt or response was lost")
		}
	})
}
