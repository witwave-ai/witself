package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// dshTestCaptureConfig installs a dsh binding whose homes are pinned to temp
// trees, so no test can read or write a developer's real ~/.dsh or ~/.witself.
func dshTestCaptureConfig(t *testing.T, mode string) Config {
	t.Helper()
	base := t.TempDir()
	dshHome := filepath.Join(base, "dsh")
	witselfHome := filepath.Join(base, ".witself")
	t.Setenv("DSH_HOME", dshHome)
	t.Setenv("WITSELF_HOME", witselfHome)
	location, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Runtime: RuntimeDSH, CaptureMode: mode,
		RuntimeCLICommand:    filepath.Join(base, "bin", "dsh"),
		MCPCommand:           filepath.Join(base, "bin", "witself"),
		MCPEnvironment:       map[string]string{"DSH_HOME": dshHome, "WITSELF_HOME": witselfHome},
		RuntimeConfigRoot:    dshHome,
		RuntimeMCPConfigPath: filepath.Join(dshHome, "cordis.patch.yml"),
		HookMode:             HookModeUser,
		HookConfigPath:       filepath.Join(dshHome, "hooks.json"),
		Account:              "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: location,
	}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestDSHSealedToolNamesAreSuppressed pins the exact public tool names the
// harness's MCP bridge publishes for Witself's sealed plane.
// @deepseek-ai/dsh-mcp-client builds `mcp__<server>__<raw>`, rewrites every
// character outside `[A-Za-z0-9_-]`, and — because that rewrite is lossy for
// every dotted Witself name — appends an underscore plus twelve hex characters
// of a SHA-256 identity digest. Without trimming that digest the fence would
// never fire under dsh.
func TestDSHSealedToolNamesAreSuppressed(t *testing.T) {
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_HOME", t.TempDir())
	for _, tc := range []struct {
		name   string
		tool   string
		sealed bool
	}{
		{"secret create", "mcp__witself__witself_secret_create_760c43ded311", true},
		{"secret reveal", "mcp__witself__witself_secret_reveal_0564d00a4f41", true},
		{"password generate", "mcp__witself__witself_password_generate_825f66a732f2", true},
		{"totp code", "mcp__witself__witself_totp_code_b069907ef04c", true},
		// A name short enough to survive normalization keeps no digest.
		{"undigested form", "mcp__witself__witself_secret_reveal", true},
		{"bare raw name", "witself.secret.reveal", true},
		// An ordinary Witself tool takes the same digest path and must stay
		// capturable: over-trimming would silence the whole namespace.
		{"memory recall", "mcp__witself__witself_memory_recall_f8fcdd58962b", false},
		{"unrelated provider tool", "mcp__other__read_file_9a1b2c3d4e5f", false},
		{"digest-shaped but unsealed", "mcp__witself__witself_fact_get_0123456789ab", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dshSensitiveToolPayload(tc.tool); got != tc.sealed {
				t.Fatalf("dshSensitiveToolPayload(%q) = %t, want %t", tc.tool, got, tc.sealed)
			}
			if tc.sealed && !sensitiveWrappedToolName(normalizeDSHToolName(tc.tool)) {
				t.Fatalf("sensitiveWrappedToolName(%q) = false", tc.tool)
			}
		})
	}
}

// TestDSHSealedToolHookIsSuppressedEndToEnd drives the sealed name through the
// real hook path the bridge produces.
func TestDSHSealedToolHookIsSuppressedEndToEnd(t *testing.T) {
	const canary = "dsh-sealed-canary-8813"
	dshTestCaptureConfig(t, ModeRaw)

	raw, err := json.Marshal(map[string]any{
		"session_id": "session-dsh-sealed", "transcript_path": "",
		"cwd": "/tmp/project", "hook_event_name": "PostToolUse",
		"tool_name":     "mcp__witself__witself_secret_reveal_0564d00a4f41",
		"tool_use_id":   "call-sealed-1",
		"tool_input":    `{"subject":"github"}`,
		"tool_response": canary,
	})
	if err != nil {
		t.Fatal(err)
	}
	event := enqueueTestHook(t, RuntimeDSH, string(raw))
	assertSensitiveHookEventRedacted(t,
		event, "mcp__witself__witself_secret_reveal_0564d00a4f41", "call-sealed-1", canary)

	pending, err := Pending(RuntimeDSH)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(persisted, []byte(canary)) {
		t.Fatalf("persisted sealed dsh tool hook contains plaintext: %s", persisted)
	}
}

// TestDSHHookPayloadShapesMapToCaptureEvents walks one whole turn through the
// five claude-code-shaped payloads @deepseek-ai/dsh-hooks-claude-code emits.
func TestDSHHookPayloadShapesMapToCaptureEvents(t *testing.T) {
	dshTestCaptureConfig(t, ModeTrace)
	const session = "session-dsh-shapes"
	writeDSHSessionLog(t, "/tmp/project", session, "session.v3.jsonl", newDSHLog(session, "/tmp/project").plain())

	start := enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "SessionStart",
		"transcript_path": "", "cwd": "/tmp/project", "source": "startup",
	})
	if start.Kind != "session.started" || start.Role != "system" || start.RunID == "" {
		t.Fatalf("SessionStart event = %#v", start)
	}

	prompt := enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "UserPromptSubmit",
		"transcript_path": "", "cwd": "/tmp/project", "prompt": "rename the helper",
	})
	if prompt.Kind != "message.user" || prompt.Role != "user" || prompt.Body != "rename the helper" {
		t.Fatalf("UserPromptSubmit event = %#v", prompt)
	}
	if prompt.TurnID == "" {
		t.Fatal("UserPromptSubmit did not open a turn")
	}

	call := enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "PreToolUse",
		"transcript_path": "", "cwd": "/tmp/project",
		"tool_name": "str_replace_editor", "tool_use_id": "call-1",
		"tool_input": `{"path":"helper.go"}`,
	})
	if call.Kind != "tool.call" || call.Role != "tool" || call.TurnID != prompt.TurnID {
		t.Fatalf("PreToolUse event = %#v", call)
	}
	result := enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "PostToolUse",
		"transcript_path": "", "cwd": "/tmp/project",
		"tool_name": "str_replace_editor", "tool_use_id": "call-1",
		"tool_input": `{"path":"helper.go"}`, "tool_response": "edited helper.go",
	})
	if result.Kind != "tool.result" || result.Role != "tool" || !strings.Contains(result.Body, "call-1") {
		t.Fatalf("PostToolUse event = %#v", result)
	}

	stop := enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "Stop",
		"transcript_path": "", "cwd": "/tmp/project", "stop_hook_active": false,
	})
	// The bridge's Stop payload carries no assistant text and an empty
	// transcript path, so the event must be value-free and marked unresolved.
	if stop.Kind != "turn.completed" || stop.Role != "system" || stop.NativeTurnFinalized {
		t.Fatalf("Stop event = %#v", stop)
	}
	if stop.TurnID != prompt.TurnID || stop.ReplyToEventID != prompt.ID {
		t.Fatalf("Stop event lost its turn correlation: %#v", stop)
	}
	if stop.CWD != "/tmp/project" {
		t.Fatalf("Stop event dropped the cwd its session log is resolved from: %#v", stop)
	}
	var data struct {
		Ordinal int `json:"dsh_turn_ordinal"`
	}
	if err := json.Unmarshal(stop.Data, &data); err != nil || data.Ordinal != 1 {
		t.Fatalf("Stop event ordinal = %#v (%v)", data, err)
	}

	// The ordinal is what names the turn in the session log, so a second
	// prompt in the same run must advance it.
	enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "UserPromptSubmit",
		"transcript_path": "", "cwd": "/tmp/project", "prompt": "now add a test",
	})
	second := enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "Stop",
		"transcript_path": "", "cwd": "/tmp/project", "stop_hook_active": false,
	})
	if err := json.Unmarshal(second.Data, &data); err != nil || data.Ordinal != 2 {
		t.Fatalf("second Stop ordinal = %#v (%v)", data, err)
	}
}

// TestDSHStopIsFinalizedFromTheSessionLog closes the loop: the value-free Stop
// event becomes the turn's assistant message once the harness has written its
// turn fence, and the turn's intermediate work rides along as children.
func TestDSHStopIsFinalizedFromTheSessionLog(t *testing.T) {
	dshTestCaptureConfig(t, ModeTrace)
	const session, cwd = "session-finalize-1", "/tmp/project"
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl.zstd", newDSHLog(session, cwd).zstd(t))

	prompt := enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "UserPromptSubmit",
		"transcript_path": "", "cwd": cwd, "prompt": "rename the helper",
	})
	// One tool of the turn was already captured from hooks; the other was not.
	enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "PreToolUse",
		"transcript_path": "", "cwd": cwd,
		"tool_name": "str_replace_editor", "tool_use_id": "call-1",
		"tool_input": `{"path":"helper.go"}`,
	})
	stop := enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "Stop",
		"transcript_path": "", "cwd": cwd, "stop_hook_active": false,
	})

	pendingStop := findPendingDSHEvent(t, stop.ID)
	// Before native prompt records exist the event is not ready. The root
	// session header is already durable, so delegated provenance is resolved.
	if _, ready, err := finalizePendingWithin(pendingStop, time.Millisecond, time.Millisecond); err != nil || ready {
		t.Fatalf("unwritten session log = ready %t, %v", ready, err)
	}

	log := newDSHLog(session, cwd).
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("rename the helper").
		assistant(1, 1, "Looking at the file.", "deepseek-v3",
			map[string]any{"inputTokens": 100, "outputTokens": 10}).
		toolCall(1, 1, "call-1", "str_replace_editor", `{"path":"helper.go"}`).
		toolCall(1, 1, "call-2", "bash", `{"command":"go build ./..."}`).
		toolResult(1, 1, "call-2", "ok").
		event("step/start", map[string]any{"turn": 1, "step": 2}).
		assistant(1, 2, "Renamed the helper.", "deepseek-v3",
			map[string]any{"inputTokens": 200, "outputTokens": 20}).
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl.zstd", log.zstd(t))

	finalized, ready, err := finalizePendingWithin(pendingStop, 100*time.Millisecond, 5*time.Millisecond)
	if err != nil || !ready {
		t.Fatalf("finalize = ready %t, %v", ready, err)
	}
	event := finalized.Event
	if event.Kind != "message.assistant" || event.Role != "assistant" ||
		event.Body != "Renamed the helper." || !event.NativeTurnFinalized {
		t.Fatalf("finalized Stop event = %#v", event)
	}
	if event.Model != "deepseek-v3" || event.ModelSource != "native_transcript" ||
		event.ModelProvider != "deepseek-official" {
		t.Fatalf("finalized model attribution = %#v", event)
	}
	if event.TurnID != prompt.TurnID {
		t.Fatalf("finalized event changed turns: %#v", event)
	}
	var data struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(event.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Usage.InputTokens != 300 || data.Usage.OutputTokens != 30 {
		t.Fatalf("finalized usage = %#v", data.Usage)
	}

	kinds := map[string]int{}
	for _, recovered := range event.RecoveredMessages {
		kinds[recovered.Kind]++
		if strings.Contains(recovered.Body, "helper.go") {
			t.Fatalf("a hook-captured tool call was replayed from the session log: %#v", recovered)
		}
	}
	if kinds["agent.thought"] != 1 || kinds["tool.call"] != 1 || kinds["tool.result"] != 1 {
		t.Fatalf("recovered messages = %#v", event.RecoveredMessages)
	}

	// Finalization is idempotent, and Entries() puts the recovered children
	// ahead of the assistant message so capture order survives upload.
	again, ready, err := finalizePendingWithin(finalized, time.Millisecond, time.Millisecond)
	if err != nil || !ready {
		t.Fatalf("refinalize = ready %t, %v", ready, err)
	}
	entries := again.Event.Entries()
	if len(entries) < 4 || entries[len(entries)-1].Role != "assistant" {
		t.Fatalf("entry order = %#v", entries)
	}
}

// TestDSHSealedStopIsNeverRehydrated proves a sealed turn's Stop event is
// settled without ever opening the harness log.
func TestDSHSealedStopIsNeverRehydrated(t *testing.T) {
	const canary = "dsh-sealed-stop-canary-2210"
	dshTestCaptureConfig(t, ModeTrace)
	const session, cwd = "session-sealed-stop", "/tmp/project"

	enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "UserPromptSubmit",
		"transcript_path": "", "cwd": cwd, "prompt": "reveal my token",
	})
	enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "PostToolUse",
		"transcript_path": "", "cwd": cwd,
		"tool_name":   "mcp__witself__witself_secret_reveal_0564d00a4f41",
		"tool_use_id": "call-sealed", "tool_response": canary,
	})
	stop := enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "Stop",
		"transcript_path": "", "cwd": cwd, "stop_hook_active": false,
	})

	log := newDSHLog(session, cwd).
		event("turn/start", map[string]any{"turn": 1}).
		prompt("reveal my token").
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		assistant(1, 1, "Your token is "+canary, "deepseek-v3", nil).
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())

	finalized, ready, err := finalizePendingWithin(
		findPendingDSHEvent(t, stop.ID), 100*time.Millisecond, 5*time.Millisecond)
	if err != nil || !ready {
		t.Fatalf("sealed finalize = ready %t, %v", ready, err)
	}
	if finalized.Event.Kind != "turn.completed" || !finalized.Event.NativeTurnFinalized {
		t.Fatalf("sealed Stop event = %#v", finalized.Event)
	}
	raw, err := json.Marshal(finalized.Event)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(canary)) {
		t.Fatalf("sealed Stop event was rehydrated from the session log: %s", raw)
	}
}

// TestDSHOversizeSessionLogSettlesTheStopEvent proves a bound the log has
// already crossed does not strand the session. Flush blocks every pending event
// of a transcript behind a returned error, and a session log only grows, so a
// hard failure here would stop this session's later prompts and tool events from
// ever uploading.
func TestDSHOversizeSessionLogSettlesTheStopEvent(t *testing.T) {
	dshTestCaptureConfig(t, ModeTrace)
	const session, cwd = "session-oversize", "/tmp/project"
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", newDSHLog(session, cwd).plain())

	enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "UserPromptSubmit",
		"transcript_path": "", "cwd": cwd, "prompt": "rename the helper",
	})
	stop := enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "Stop",
		"transcript_path": "", "cwd": cwd, "stop_hook_active": false,
	})

	log := newDSHLog(session, cwd).
		event("turn/start", map[string]any{"turn": 1}).
		event("step/start", map[string]any{"turn": 1, "step": 1}).
		prompt("rename the helper").
		assistant(1, 1, "Renamed the helper.", "deepseek-v3", nil).
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})
	path := writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", log.plain())
	// A sparse truncate crosses the cap without writing 64 MiB.
	if err := os.Truncate(path, dshSessionLogMaxBytes+1); err != nil {
		t.Fatal(err)
	}

	finalized, ready, err := finalizePendingWithin(
		findPendingDSHEvent(t, stop.ID), 10*time.Millisecond, time.Millisecond)
	if err != nil || !ready {
		t.Fatalf("oversize finalize = ready %t, %v", ready, err)
	}
	// The turn's content is unreachable, so the event settles as the value-free
	// completion marker it was captured as, never as an assistant message.
	if finalized.Event.Kind != "turn.completed" || finalized.Event.Role != "system" ||
		!finalized.Event.NativeTurnFinalized {
		t.Fatalf("settled Stop event = %#v", finalized.Event)
	}
	var reason struct {
		NativeFinalization string `json:"dsh_native_finalization"`
	}
	if json.Unmarshal(finalized.Event.Data, &reason) != nil || reason.NativeFinalization != "bound_exceeded" {
		t.Fatal("bounded native omission has no value-free diagnostic")
	}
}

// TestDSHRecoveryIsSkippedWhenTheDedupKeyOverflows proves a turn that ran more
// tools than the Stop event's dedup key can hold never re-emits an already
// captured tool record under a second identity the server cannot collapse.
func TestDSHRecoveryIsSkippedWhenTheDedupKeyOverflows(t *testing.T) {
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_HOME", t.TempDir())
	turn := dshSessionTurn{Complete: true, Steps: []dshSessionStep{
		{
			Step: 1, Text: "looking",
			ToolCalls:   []dshSessionToolCall{{CallID: "call-1", Name: "bash"}},
			ToolResults: []dshSessionToolResult{{CallID: "call-1", Name: "bash", Body: "ok"}},
		},
		{Step: 2, Text: "done"},
	}}
	event := Event{SessionID: "session-overflow", TurnID: "turn_1"}

	kinds := map[string]int{}
	for _, recovered := range dshRecoveredTurnSteps(event, turn, nil, false) {
		kinds[recovered.Kind]++
	}
	if kinds["tool.call"] != 1 || kinds["tool.result"] != 1 || kinds["agent.thought"] != 1 {
		t.Fatalf("recovered messages without overflow = %#v", kinds)
	}

	kinds = map[string]int{}
	for _, recovered := range dshRecoveredTurnSteps(event, turn, nil, true) {
		kinds[recovered.Kind]++
	}
	if kinds["tool.call"] != 0 || kinds["tool.result"] != 0 {
		t.Fatalf("overflowed dedup key still replayed tool records = %#v", kinds)
	}
	if kinds["agent.thought"] != 1 {
		t.Fatalf("overflow dropped the turn's assistant steps = %#v", kinds)
	}
}

// TestDSHTurnToolUseOverflowIsRecordedOnTheStopEvent pins the state bookkeeping
// the skip above depends on.
func TestDSHTurnToolUseOverflowIsRecordedOnTheStopEvent(t *testing.T) {
	dshTestCaptureConfig(t, ModeTrace)
	const session, cwd = "session-overflow-state", "/tmp/project"
	writeDSHSessionLog(t, cwd, session, "session.v3.jsonl", newDSHLog(session, cwd).plain())

	enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "UserPromptSubmit",
		"transcript_path": "", "cwd": cwd, "prompt": "run everything",
	})
	for index := 0; index <= maxDSHTurnToolUseIDs; index++ {
		enqueueDSHHook(t, map[string]any{
			"session_id": session, "hook_event_name": "PreToolUse",
			"transcript_path": "", "cwd": cwd, "tool_name": "bash",
			"tool_use_id": "call-" + strconv.Itoa(index),
			"tool_input":  `{"command":"go build ./..."}`,
		})
	}
	stop := enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "Stop",
		"transcript_path": "", "cwd": cwd, "stop_hook_active": false,
	})

	var data struct {
		HookedTool []string `json:"dsh_hooked_tool_use_ids"`
		Overflow   bool     `json:"dsh_hooked_tool_use_overflow"`
	}
	if err := json.Unmarshal(stop.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.HookedTool) != maxDSHTurnToolUseIDs || !data.Overflow {
		t.Fatalf("Stop event dedup key = %d ids, overflow %t", len(data.HookedTool), data.Overflow)
	}

	// The next prompt opens a turn whose tool events are all captured again.
	second := enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "UserPromptSubmit",
		"transcript_path": "", "cwd": cwd, "prompt": "now one tool",
	})
	if second.TurnID == "" {
		t.Fatal("second prompt did not open a turn")
	}
	secondStop := enqueueDSHHook(t, map[string]any{
		"session_id": session, "hook_event_name": "Stop",
		"transcript_path": "", "cwd": cwd, "stop_hook_active": false,
	})
	data.Overflow = false
	if err := json.Unmarshal(secondStop.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Overflow {
		t.Fatal("overflow survived the next prompt")
	}
}

func findPendingDSHEvent(t *testing.T, eventID string) PendingEvent {
	t.Helper()
	pending, err := Pending(RuntimeDSH)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pending {
		if item.Event.ID == eventID {
			return item
		}
	}
	t.Fatalf("pending event %s not found", eventID)
	return PendingEvent{}
}

func enqueueDSHHook(t *testing.T, payload map[string]any) Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return enqueueTestHook(t, RuntimeDSH, string(raw))
}
