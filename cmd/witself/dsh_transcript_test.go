package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const dshFlushSession = "dsh-flush-session"

// configureDSHCaptureFlushTest installs a dsh binding pointed at a fake ledger.
// Both homes are temp trees, so the test can never touch a real ~/.dsh.
func configureDSHCaptureFlushTest(t *testing.T, endpoint string) string {
	t.Helper()
	base := t.TempDir()
	dshHome := filepath.Join(base, "dsh")
	witselfHome := filepath.Join(base, ".witself")
	t.Setenv("DSH_HOME", dshHome)
	t.Setenv("WITSELF_HOME", witselfHome)
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	tokenPath := filepath.Join(base, "agent.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeDSH, CaptureMode: transcriptcapture.ModeTrace,
		RuntimeCLICommand:    filepath.Join(base, "bin", "dsh"),
		MCPCommand:           filepath.Join(base, "bin", "witself"),
		MCPEnvironment:       map[string]string{"DSH_HOME": dshHome, "WITSELF_HOME": witselfHome},
		RuntimeConfigRoot:    dshHome,
		RuntimeMCPConfigPath: filepath.Join(dshHome, dshPatchFileName),
		HookMode:             transcriptcapture.HookModeUser,
		HookConfigPath:       filepath.Join(dshHome, dshHookConfigFileName),
		Account:              "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: location,
		Endpoint: endpoint, TokenFile: tokenPath,
	}); err != nil {
		t.Fatal(err)
	}
	return dshHome
}

func enqueueDSHFlushHook(t *testing.T, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeDSH, raw); err != nil {
		t.Fatal(err)
	}
}

// writeDSHFlushSessionLog lays down a Zstandard-framed session log for one
// finished turn under `$DSH_HOME/sessions/<projectKey>/<sessionId>/`.
func writeDSHFlushSessionLog(t *testing.T, dshHome, cwd, sessionID, answer, toolName string) {
	t.Helper()
	lines := []string{
		mustDSHTestJSON(map[string]any{
			"type": "session", "version": 3, "id": sessionID, "createdAt": 0,
			"delegationDepth": 0, "cwd": cwd, "isSeeded": false,
		}),
		mustDSHTestEvent(1, "turn/start", map[string]any{"turn": 1}),
		mustDSHTestEvent(2, "step/start", map[string]any{"turn": 1, "step": 1}),
		mustDSHTestEvent(3, "user/message", map[string]any{
			"id": "m1", "role": "user", "source": map[string]any{"kind": "user"},
			"content": []any{map[string]any{"type": "text", "text": "rename the helper"}},
		}),
		mustDSHTestEvent(4, "user/message", map[string]any{
			"id": "plugin-context", "role": "user", "source": map[string]any{"kind": "plugin"},
			"content": []any{map[string]any{"type": "text", "text": "synthetic plugin context"}},
		}),
		mustDSHTestEvent(5, "assistant/message", map[string]any{
			"turn": 1, "step": 1, "stream": []any{},
			"message": map[string]any{
				"id": "m2", "role": "assistant",
				"content": []any{
					map[string]any{"type": "reasoning", "text": "synthetic reasoning must stay local"},
					map[string]any{"type": "text", "text": "Working on it."},
				},
				"source": map[string]any{"kind": "model", "provider": "deepseek-official", "model": "deepseek-flash"},
			},
		}),
		mustDSHTestEvent(6, "tool/call", map[string]any{
			"turn": 1, "step": 1, "callId": "call-1", "name": toolName,
			"arguments": `{"path":"helper.go"}`,
		}),
		mustDSHTestEvent(7, "tool/result", map[string]any{
			"turn": 1, "step": 1,
			"message": map[string]any{
				"id": "tool-result-1", "role": "user", "source": map[string]any{"kind": "tool", "callId": "call-1"},
				"content": []any{map[string]any{
					"type": "tool-result", "toolCallId": "call-1", "content": []any{map[string]any{"type": "text", "text": "edited helper.go"}},
				}},
			},
		}),
		mustDSHTestEvent(8, "step/end", map[string]any{"turn": 1, "step": 1}),
		mustDSHTestEvent(9, "step/start", map[string]any{"turn": 1, "step": 2}),
		mustDSHTestEvent(10, "assistant/message", map[string]any{
			"turn": 1, "step": 2, "stream": []any{},
			"message": map[string]any{
				"id": "m3", "role": "assistant",
				"content": []any{
					map[string]any{"type": "reasoning", "text": "synthetic final reasoning must stay local"},
					map[string]any{"type": "text", "text": answer},
				},
				"source": map[string]any{"kind": "model", "provider": "deepseek-official", "model": "deepseek-flash"},
			},
		}),
		mustDSHTestEvent(11, "step/end", map[string]any{"turn": 1, "step": 2}),
		mustDSHTestEvent(12, "turn/end", map[string]any{
			"turn": 1, "reason": map[string]any{"kind": "completed"},
		}),
	}
	writeDSHFlushSessionLines(t, dshHome, cwd, sessionID, lines)
}

// The immutable header proves that these hook fixtures belong to a root
// session. Prompt and completion batches remain unpublished until the test
// explicitly writes them, preserving the native persistence timing boundary.
func writeDSHFlushSessionHeader(t *testing.T, dshHome, cwd, sessionID string) {
	t.Helper()
	writeDSHFlushSessionLines(t, dshHome, cwd, sessionID, []string{mustDSHTestJSON(map[string]any{
		"type": "session", "version": 3, "id": sessionID, "createdAt": 0,
		"delegationDepth": 0, "cwd": cwd, "isSeeded": false,
	})})
}

func writeDSHFlushSessionLines(t *testing.T, dshHome, cwd, sessionID string, lines []string) {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = encoder.Close() }()
	var content []byte
	for _, line := range lines {
		content = append(content, encoder.EncodeAll([]byte(line+"\n"), nil)...)
	}
	dir := filepath.Join(dshHome, "sessions", dshTestProjectKey(cwd), sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.v3.jsonl.zstd"), content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustDSHTestEvent(seq int, recordType string, data map[string]any) string {
	return mustDSHTestJSON(map[string]any{
		"type": recordType, "seq": seq, "time": 1700000000000 + seq, "data": data,
	})
}

func mustDSHTestJSON(value map[string]any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// dshTestProjectKey mirrors the harness's readable project-directory name for
// the simple absolute paths these tests use.
func dshTestProjectKey(cwd string) string {
	return "--" + strings.TrimLeft(strings.ReplaceAll(cwd, "/", "-"), "-") + "--"
}

// TestDSHFlushUploadsTheWholeTurnInCaptureOrder drives a real turn from hooks
// through the session-log finalizer to the ledger.
func TestDSHFlushUploadsTheWholeTurnInCaptureOrder(t *testing.T) {
	var appended []string
	srv := startCaptureLedgerServer(t, &appended)
	dshHome := configureDSHCaptureFlushTest(t, srv.URL)
	const cwd = "/tmp/project"
	writeDSHFlushSessionHeader(t, dshHome, cwd, dshFlushSession)

	enqueueDSHFlushHook(t, map[string]any{
		"session_id": dshFlushSession, "hook_event_name": "SessionStart",
		"transcript_path": "", "cwd": cwd, "source": "startup",
	})
	enqueueDSHFlushHook(t, map[string]any{
		"session_id": dshFlushSession, "hook_event_name": "UserPromptSubmit",
		"transcript_path": "", "cwd": cwd, "prompt": "rename the helper",
	})
	enqueueDSHFlushHook(t, map[string]any{
		"session_id": dshFlushSession, "hook_event_name": "PreToolUse",
		"transcript_path": "", "cwd": cwd,
		"tool_name": "str_replace_editor", "tool_use_id": "call-1",
		"tool_input": `{"path":"helper.go"}`,
	})
	enqueueDSHFlushHook(t, map[string]any{
		"session_id": dshFlushSession, "hook_event_name": "PostToolUse",
		"transcript_path": "", "cwd": cwd,
		"tool_name": "str_replace_editor", "tool_use_id": "call-1",
		"tool_input": `{"path":"helper.go"}`, "tool_response": "edited helper.go",
	})
	enqueueDSHFlushHook(t, map[string]any{
		"session_id": dshFlushSession, "hook_event_name": "Stop",
		"transcript_path": "", "cwd": cwd, "stop_hook_active": false,
	})

	// The harness writes its turn fence after the synchronous Stop hook
	// returns. Before its prompt batch exists the Stop is deferred, never lost,
	// while the hook-captured events of the fenced turn already upload.
	_, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeDSH})
	})
	if code == 0 || !strings.Contains(stderr, "flushed 4 dsh transcript event(s); deferred 1") {
		t.Fatalf("pre-log flush = code %d stderr %q", code, stderr)
	}

	writeDSHFlushSessionLog(t, dshHome, cwd, dshFlushSession, "Renamed the helper.", "str_replace_editor")
	_, stderr, code = captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeDSH})
	})
	if code != 0 || !strings.Contains(stderr, "flushed 1 dsh transcript event(s)") {
		t.Fatalf("flush = code %d stderr %q", code, stderr)
	}
	want := []string{
		"session started",
		"rename the helper",
		`{"tool_name":"str_replace_editor","tool_use_id":"call-1","value":"{\"path\":\"helper.go\"}"}`,
		`{"tool_name":"str_replace_editor","tool_use_id":"call-1","value":"edited helper.go"}`,
		// The turn's earlier step text arrives as a child of the Stop event,
		// immediately before the finalized assistant message.
		"Working on it.",
		"Renamed the helper.",
	}
	if !slices.Equal(appended, want) {
		t.Fatalf("uploaded bodies = %#v, want %#v in capture order", appended, want)
	}
}

// TestDSHSealedTurnFlushUploadsRedactedBodiesOnly proves the session log can
// never reintroduce what the sealed-plane fence suppressed.
func TestDSHSealedTurnFlushUploadsRedactedBodiesOnly(t *testing.T) {
	const canary = "dsh-flush-sealed-canary-9042"
	var appended []string
	srv := startCaptureLedgerServer(t, &appended)
	dshHome := configureDSHCaptureFlushTest(t, srv.URL)
	const cwd = "/tmp/project"
	const sealedTool = "mcp__witself__witself_secret_reveal_0564d00a4f41"
	writeDSHFlushSessionHeader(t, dshHome, cwd, dshFlushSession)

	enqueueDSHFlushHook(t, map[string]any{
		"session_id": dshFlushSession, "hook_event_name": "UserPromptSubmit",
		"transcript_path": "", "cwd": cwd, "prompt": "reveal my " + canary + " token",
	})
	enqueueDSHFlushHook(t, map[string]any{
		"session_id": dshFlushSession, "hook_event_name": "PostToolUse",
		"transcript_path": "", "cwd": cwd,
		"tool_name": sealedTool, "tool_use_id": "call-sealed", "tool_response": canary,
	})
	enqueueDSHFlushHook(t, map[string]any{
		"session_id": dshFlushSession, "hook_event_name": "Stop",
		"transcript_path": "", "cwd": cwd, "stop_hook_active": false,
	})
	writeDSHFlushSessionLog(t, dshHome, cwd, dshFlushSession, "Your token is "+canary, sealedTool)

	_, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeDSH})
	})
	if code != 0 || !strings.Contains(stderr, "flushed 3 dsh transcript event(s)") {
		t.Fatalf("sealed flush = code %d stderr %q", code, stderr)
	}
	// The sealed tool sealed the whole turn, so the already-queued prompt and
	// the tool event are both rewritten to value-free markers before upload,
	// and the Stop event is settled without consulting the session log.
	want := []string{
		"prompt omitted from portable transcript because this turn used sealed secrets",
		"tool payload omitted from portable transcript because this turn used sealed secrets",
		"turn completed",
	}
	if !slices.Equal(appended, want) {
		t.Fatalf("uploaded sealed bodies = %#v, want %#v", appended, want)
	}
	for _, body := range appended {
		if strings.Contains(body, canary) {
			t.Fatalf("sealed turn uploaded plaintext: %q", body)
		}
	}
}

// TestDSHLatestFenceClosesAHeldRun covers the generic fence path for a dsh
// session whose provider never reached its Stop hook.
func TestDSHLatestFenceClosesAHeldRun(t *testing.T) {
	var appended []string
	srv := startCaptureLedgerServer(t, &appended)
	dshHome := configureDSHCaptureFlushTest(t, srv.URL)
	const cwd = "/tmp/project"
	writeDSHFlushSessionHeader(t, dshHome, cwd, dshFlushSession)

	enqueueDSHFlushHook(t, map[string]any{
		"session_id": dshFlushSession, "hook_event_name": "UserPromptSubmit",
		"transcript_path": "", "cwd": cwd, "prompt": "start the job",
	})
	enqueueDSHFlushHook(t, map[string]any{
		"session_id": dshFlushSession, "hook_event_name": "PreToolUse",
		"transcript_path": "", "cwd": cwd,
		"tool_name": "bash", "tool_use_id": "call-1", "tool_input": `{"command":"make"}`,
	})

	_, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeDSH})
	})
	if code == 0 || !strings.Contains(stderr, "no-fence 2") {
		t.Fatalf("unfenced flush = code %d stderr %q", code, stderr)
	}

	_, stderr, code = captureFactDeleteCLI(t, func() int {
		return transcriptCmd([]string{
			"fence", "--runtime", "dsh", "--session", dshFlushSession,
			"--latest", "--reason", "job-completed",
		})
	})
	if code != 0 || stderr != "fenced 1 held dsh turn(s)\n" {
		t.Fatalf("launcher fence = code %d stderr %q", code, stderr)
	}
	_, stderr, code = captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeDSH})
	})
	if code != 0 || !strings.Contains(stderr, "flushed 3 dsh transcript event(s)") {
		t.Fatalf("fenced flush = code %d stderr %q", code, stderr)
	}
	want := []string{
		"start the job",
		`{"tool_name":"bash","tool_use_id":"call-1","value":"{\"command\":\"make\"}"}`,
		"delegation job completed",
	}
	if !slices.Equal(appended, want) {
		t.Fatalf("uploaded bodies = %#v, want %#v", appended, want)
	}
}
