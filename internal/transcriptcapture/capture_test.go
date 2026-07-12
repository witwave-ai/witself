package transcriptcapture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeCaptureCorrelatesSessionTurnAndLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	loc, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(Config{
		Runtime: RuntimeClaudeCode, CaptureMode: ModeRaw,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: loc,
	}); err != nil {
		t.Fatal(err)
	}

	start := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"session-1","hook_event_name":"SessionStart","cwd":"/src/witself","source":"startup"}`)
	prompt := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"session-1","hook_event_name":"UserPromptSubmit","cwd":"/src/witself","prompt":"hello"}`)
	stop := enqueueTestHook(t, RuntimeClaudeCode, `{"session_id":"session-1","hook_event_name":"Stop","cwd":"/src/witself","last_assistant_message":"hi there"}`)

	if start.RunID == "" || prompt.RunID != start.RunID || stop.RunID != start.RunID {
		t.Fatalf("run ids = %q / %q / %q", start.RunID, prompt.RunID, stop.RunID)
	}
	if prompt.TurnID == "" || stop.TurnID != prompt.TurnID {
		t.Fatalf("turn ids = %q / %q", prompt.TurnID, stop.TurnID)
	}
	if stop.ReplyToEventID != prompt.ID {
		t.Fatalf("reply event = %q, want %q", stop.ReplyToEventID, prompt.ID)
	}
	if got := stop.Entries()[0].ReplyToExternalID; got != prompt.ID+":0" {
		t.Fatalf("reply external id = %q", got)
	}
	if got := prompt.TranscriptExternalID(); got != RuntimeClaudeCode+":"+loc.ID+":session-1" {
		t.Fatalf("transcript external id = %q", got)
	}
	var metadata map[string]any
	if err := json.Unmarshal(prompt.TranscriptMetadata(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["agent_name"] != "scott" || metadata["runtime"] != RuntimeClaudeCode {
		t.Fatalf("metadata = %#v", metadata)
	}
	pending, err := Pending(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending = %d, want 3", len(pending))
	}
}

func TestLocationLabelIsOptionalAndPreserved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	loc, err := EnsureLocation("")
	if err != nil {
		t.Fatal(err)
	}
	if loc.ID == "" || loc.Name != "" {
		t.Fatalf("unlabeled location = %#v", loc)
	}
	loc, err = EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	loc, err = EnsureLocation("")
	if err != nil {
		t.Fatal(err)
	}
	if loc.Name != "home" {
		t.Fatalf("location label = %q", loc.Name)
	}
	title := Event{AgentName: "scott", Runtime: RuntimeCodex, Location: Location{ID: loc.ID}, CWD: "/src/witself"}.TranscriptTitle()
	if title != "scott / codex / witself" {
		t.Fatalf("title = %q", title)
	}
}

func TestPinnedHookAgentMustMatchInstalledBinding(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	loc, err := EnsureLocation("")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(Config{
		Runtime: RuntimeCodex, CaptureMode: ModeRaw,
		Account: "default", Realm: "default", Agent: "agent-under-test",
		AgentID: "agent_1", AgentName: "agent-under-test", Location: loc,
	}); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"session_id":"session-1","hook_event_name":"SessionStart"}`)
	if _, err := EnqueueHookForAgent(RuntimeCodex, "different-agent", raw); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatch error = %v", err)
	}
	if _, err := EnqueueHookForBinding(RuntimeCodex, "default", "default", "agent-under-test", "work", raw); err == nil || !strings.Contains(err.Error(), "location") {
		t.Fatalf("location mismatch error = %v", err)
	}
	if _, err := EnqueueHookForBinding(RuntimeCodex, "another-account", "default", "agent-under-test", "", raw); err == nil || !strings.Contains(err.Error(), "account") {
		t.Fatalf("account mismatch error = %v", err)
	}
	if _, err := EnqueueHookForBinding(RuntimeCodex, "default", "another-realm", "agent-under-test", "", raw); err == nil || !strings.Contains(err.Error(), "realm") {
		t.Fatalf("realm mismatch error = %v", err)
	}
	pending, err := Pending(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("mismatched hook queued %d events", len(pending))
	}
	if _, err := EnqueueHookForAgent(RuntimeCodex, "agent-under-test", raw); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureChunksWithoutTruncation(t *testing.T) {
	body := strings.Repeat("abcd", entryBodyChunkSize/2)
	event := Event{
		SchemaVersion: SchemaVersion, ID: "evt_1", Runtime: RuntimeCodex,
		CaptureMode: ModeMessages, SessionID: "s", RunID: "r",
		HookEvent: "UserPromptSubmit", Kind: "message.user", Role: "user",
		Body: body,
	}
	entries := event.Entries()
	if len(entries) < 2 {
		t.Fatalf("entries = %d, want chunks", len(entries))
	}
	var joined strings.Builder
	for i, entry := range entries {
		joined.WriteString(entry.Body)
		if entry.ExternalID != fmt.Sprintf("evt_1:%d", i) {
			t.Fatalf("entry %d external id = %q", i, entry.ExternalID)
		}
	}
	if joined.String() != body {
		t.Fatal("chunked body was not preserved")
	}
	unicodeTitle := Event{
		AgentName: strings.Repeat("é", 150), Runtime: RuntimeCodex,
		Location: Location{Name: "home"}, CWD: "/src/witself",
	}.TranscriptTitle()
	if len(unicodeTitle) > 256 || !json.Valid([]byte(`"`+unicodeTitle+`"`)) {
		t.Fatalf("invalid bounded title: %d bytes", len(unicodeTitle))
	}
}

func TestToolFailureKeepsToolIdentityAndInput(t *testing.T) {
	var event Event
	setEventContent(&event, hookInput{
		HookEventName: "PostToolUseFailure",
		ToolName:      "Bash",
		ToolUseID:     "tool_1",
		ToolInput:     json.RawMessage(`{"command":"go test ./..."}`),
		Error:         json.RawMessage(`"exit status 1"`),
	}, nil)
	if event.Kind != "tool.error" || event.Role != "tool" {
		t.Fatalf("event = %#v", event)
	}
	for _, want := range []string{`"tool_name":"Bash"`, `"tool_use_id":"tool_1"`, `"command":"go test ./..."`, `"error":"exit status 1"`} {
		if !strings.Contains(event.Body, want) {
			t.Fatalf("tool failure body %q does not contain %q", event.Body, want)
		}
	}
}

func TestInstallHooksPreservesOthersAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{"env":{"EXISTING":"yes"},"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"custom-check"}]}]}}`
	if err := os.WriteFile(settings, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := InstallHooks(RuntimeClaudeCode, ModeRaw, "/usr/local/bin/witself", "default", "default", "scott", "home"); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), hookCommandMarker) != 15 {
		t.Fatalf("witself hook count = %d, want 15\n%s", strings.Count(string(raw), hookCommandMarker), raw)
	}
	if !strings.Contains(string(raw), "custom-check") || !strings.Contains(string(raw), "EXISTING") {
		t.Fatalf("unrelated settings were lost:\n%s", raw)
	}
	if !strings.Contains(string(raw), "--agent 'scott'") {
		t.Fatalf("hook does not pin its agent:\n%s", raw)
	}
	if !strings.Contains(string(raw), "--account 'default' --realm 'default'") {
		t.Fatalf("hook does not pin its account and realm:\n%s", raw)
	}
	if !strings.Contains(string(raw), "--location 'home'") {
		t.Fatalf("hook does not pin its supplied location:\n%s", raw)
	}
}

func TestCodexHookSetUsesOnlySupportedEvents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	path, err := InstallHooks(RuntimeCodex, ModeRaw, "/usr/local/bin/witself", "default", "default", "scott", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{
		"SessionStart", "UserPromptSubmit", "Stop", "SubagentStart", "SubagentStop",
		"PreCompact", "PostCompact", "PreToolUse", "PermissionRequest", "PostToolUse",
	} {
		if !strings.Contains(string(raw), `"`+event+`"`) {
			t.Errorf("missing %s", event)
		}
	}
	for _, event := range []string{"SessionEnd", "StopFailure", "PostToolUseFailure"} {
		if strings.Contains(string(raw), `"`+event+`"`) {
			t.Errorf("unsupported Codex event %s was installed", event)
		}
	}
	if strings.Contains(string(raw), "--location") {
		t.Fatalf("unlabeled install contains a location flag:\n%s", raw)
	}
}

func TestRawHookCoverageByRuntime(t *testing.T) {
	for _, tc := range []struct {
		runtime string
		want    []string
	}{
		{RuntimeCodex, []string{
			"SessionStart", "UserPromptSubmit", "Stop", "SubagentStart", "SubagentStop", "PreCompact", "PostCompact",
			"PreToolUse", "PermissionRequest", "PostToolUse",
		}},
		{RuntimeClaudeCode, []string{
			"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd", "SubagentStart", "SubagentStop", "PreCompact", "PostCompact",
			"PreToolUse", "PermissionRequest", "PermissionDenied", "PostToolUse", "PostToolUseFailure", "Notification",
		}},
		{RuntimeGrokBuild, []string{
			"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd", "SubagentStart", "SubagentStop", "PreCompact", "PostCompact",
			"PreToolUse", "PermissionDenied", "PostToolUse", "PostToolUseFailure", "Notification",
		}},
		{RuntimeCursor, []string{
			"sessionStart", "beforeSubmitPrompt", "afterAgentResponse", "stop", "sessionEnd", "subagentStart", "subagentStop", "preCompact",
			"afterAgentThought", "preToolUse", "postToolUse", "postToolUseFailure",
		}},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			got := hookEvents(tc.runtime, ModeRaw)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("hook events:\n got: %v\nwant: %v", got, tc.want)
			}
		})
	}
}

func TestGrokHooksUseDedicatedGlobalFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	path, err := InstallHooks(RuntimeGrokBuild, ModeRaw, "/usr/local/bin/witself", "default", "default", "scott", "home")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, ".grok", "hooks", "witself.json") {
		t.Fatalf("path = %q", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{
		"SessionStart", "SessionEnd", "UserPromptSubmit", "Stop", "StopFailure",
		"SubagentStart", "SubagentStop", "PreCompact", "PostCompact",
		"PreToolUse", "PermissionDenied", "PostToolUse", "PostToolUseFailure", "Notification",
	} {
		if !strings.Contains(string(raw), `"`+event+`"`) {
			t.Errorf("missing %s", event)
		}
	}
	if strings.Count(string(raw), hookCommandMarker) != 14 {
		t.Fatalf("hook count = %d\n%s", strings.Count(string(raw), hookCommandMarker), raw)
	}
	if _, err := RemoveHooks(RuntimeGrokBuild); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Grok hook file still exists: %v", err)
	}
}

func TestCursorHooksPreserveUnrelatedGlobalHandlers(t *testing.T) {
	home := t.TempDir()
	cursorHome := filepath.Join(home, ".cursor")
	t.Setenv("HOME", home)
	t.Setenv("CURSOR_CONFIG_DIR", cursorHome)
	path := filepath.Join(cursorHome, "hooks.json")
	if err := os.MkdirAll(cursorHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"hooks":{"stop":[{"command":"custom-check"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := InstallHooks(RuntimeCursor, ModeRaw, "/usr/local/bin/witself", "default", "default", "scott", "home"); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "custom-check") || strings.Count(string(raw), hookCommandMarker) != 12 {
		t.Fatalf("Cursor hooks were not merged idempotently:\n%s", raw)
	}
	for _, event := range []string{
		"sessionStart", "sessionEnd", "beforeSubmitPrompt", "afterAgentResponse", "afterAgentThought",
		"stop", "subagentStart", "subagentStop", "preCompact", "preToolUse", "postToolUse", "postToolUseFailure",
	} {
		if !strings.Contains(string(raw), `"`+event+`"`) {
			t.Errorf("missing %s", event)
		}
	}
	if _, err := RemoveHooks(RuntimeCursor); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "custom-check") || strings.Contains(string(raw), hookCommandMarker) {
		t.Fatalf("Cursor uninstall damaged unrelated hooks:\n%s", raw)
	}
}

func TestCursorCaptureNormalizesConversationAndStructuredResponse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	loc, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(Config{
		Runtime: RuntimeCursor, CaptureMode: ModeRaw,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: loc,
	}); err != nil {
		t.Fatal(err)
	}
	prompt := enqueueTestHook(t, RuntimeCursor, `{"conversation_id":"conversation-1","generation_id":"generation-1","hook_event_name":"beforeSubmitPrompt","cwd":"/src/witself","prompt":"hello"}`)
	response := enqueueTestHook(t, RuntimeCursor, `{"conversation_id":"conversation-1","generation_id":"generation-1","hook_event_name":"afterAgentResponse","cwd":"/src/witself","text":"hi there","input_tokens":12,"output_tokens":4}`)
	if prompt.HookEvent != "UserPromptSubmit" || prompt.NativeHookEvent != "beforeSubmitPrompt" || prompt.TurnID != "generation-1" {
		t.Fatalf("prompt = %#v", prompt)
	}
	if response.Kind != "message.assistant" || response.Body != "hi there" || response.ReplyToEventID != prompt.ID {
		t.Fatalf("response = %#v", response)
	}
	entries := response.Entries()
	if len(entries) != 1 || !strings.Contains(string(entries[0].Payload), `"input_tokens":12`) || !strings.Contains(string(entries[0].Payload), `"native_event":"afterAgentResponse"`) {
		t.Fatalf("payload = %s", entries[0].Payload)
	}
}

func TestGrokCaptureNormalizesPayloadAndReadsAssistantChunks(t *testing.T) {
	home := t.TempDir()
	grokHome := filepath.Join(home, ".grok")
	t.Setenv("HOME", home)
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	loc, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(Config{
		Runtime: RuntimeGrokBuild, CaptureMode: ModeRaw,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: loc,
	}); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(grokHome, "sessions", "workspace", "session-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
	updates := strings.Join([]string{
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","_meta":{"modelId":"grok-4.5"},"content":{"type":"text","text":"first"}}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"second"}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}
	promptRaw, _ := json.Marshal(map[string]any{
		"sessionId": "session-1", "hookEventName": "user_prompt_submit", "promptId": "prompt-1",
		"prompt": "<user_query>\nhello\n</user_query>", "transcriptPath": transcriptPath,
	})
	prompt := enqueueTestHook(t, RuntimeGrokBuild, string(promptRaw))
	stopRaw, _ := json.Marshal(map[string]any{
		"sessionId": "session-1", "hookEventName": "stop", "promptId": "prompt-1",
		"reason": "end_turn", "transcriptPath": transcriptPath,
	})
	stop := enqueueTestHook(t, RuntimeGrokBuild, string(stopRaw))
	if prompt.Body != "hello" || prompt.TurnID != "prompt-1" {
		t.Fatalf("prompt = %#v", prompt)
	}
	if stop.Body != "first\n\nsecond" || stop.Kind != "message.assistant" || stop.ReplyToEventID != prompt.ID || stop.Model != "grok-4.5" || stop.ModelSource != "native_transcript" {
		t.Fatalf("stop = %#v", stop)
	}
}

func TestClaudeCaptureReadsModelFromNativeTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	loc, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(Config{
		Runtime: RuntimeClaudeCode, RuntimeVersion: "2.1.197", CaptureMode: ModeRaw,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: loc,
	}); err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(home, ".claude", "projects", "workspace")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(projectDir, "session-1.jsonl")
	transcript := strings.Join([]string{
		`{"type":"user","message":{"role":"user"}}`,
		`{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-7"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"session_id": "session-1", "hook_event_name": "Stop",
		"last_assistant_message": "done", "transcript_path": transcriptPath,
	})
	event := enqueueTestHook(t, RuntimeClaudeCode, string(raw))
	if event.Model != "claude-opus-4-7" || event.ModelSource != "native_transcript" {
		t.Fatalf("event model provenance = %q / %q", event.Model, event.ModelSource)
	}
	entries := event.Entries()
	if len(entries) != 1 || entries[0].Model != "claude-opus-4-7" || !strings.Contains(string(entries[0].Payload), `"model":"claude-opus-4-7"`) || !strings.Contains(string(entries[0].Payload), `"model":"native_transcript"`) {
		t.Fatalf("entry = %#v", entries)
	}
}

func TestGrokAndCursorRejectCrossRuntimeHookPayloads(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime string
		raw     string
	}{
		{
			name:    "Grok payload sent to Cursor",
			runtime: RuntimeCursor,
			raw:     `{"sessionId":"session-1","hookEventName":"user_prompt_submit","prompt":"hello"}`,
		},
		{
			name:    "Cursor payload sent to Grok",
			runtime: RuntimeGrokBuild,
			raw:     `{"conversation_id":"conversation-1","hook_event_name":"beforeSubmitPrompt","prompt":"hello"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
			loc, err := EnsureLocation("home")
			if err != nil {
				t.Fatal(err)
			}
			if err := SaveConfig(Config{
				Runtime: tc.runtime, CaptureMode: ModeRaw,
				Account: "default", Realm: "default", Agent: "scott",
				AgentID: "agent_1", AgentName: "scott", Location: loc,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := EnqueueHook(tc.runtime, []byte(tc.raw)); err == nil || !strings.Contains(err.Error(), "native "+tc.runtime+" payload") {
				t.Fatalf("cross-runtime hook error = %v", err)
			}
			pending, err := Pending(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatalf("cross-runtime hook queued %d events", len(pending))
			}
		})
	}
}

func TestFourRuntimeProvenanceUsesOneNullableContract(t *testing.T) {
	for _, tc := range []struct {
		name               string
		runtime            string
		configuredVersion  string
		hook               string
		wantRuntimeVersion string
		wantVersionSource  string
		wantModel          string
		wantModelSource    string
		wantModelProvider  string
		wantProviderSource string
	}{
		{
			name: "codex", runtime: RuntimeCodex, configuredVersion: "0.30.0",
			hook:               `{"session_id":"session-1","hook_event_name":"UserPromptSubmit","prompt":"hello","model":"gpt-5.6-sol"}`,
			wantRuntimeVersion: "0.30.0", wantVersionSource: "cli", wantModel: "gpt-5.6-sol", wantModelSource: "hook",
		},
		{
			name: "claude", runtime: RuntimeClaudeCode, configuredVersion: "2.1.197",
			hook:               `{"session_id":"session-1","hook_event_name":"UserPromptSubmit","prompt":"hello"}`,
			wantRuntimeVersion: "2.1.197", wantVersionSource: "cli",
		},
		{
			name: "grok", runtime: RuntimeGrokBuild, configuredVersion: "0.2.93",
			hook:               `{"sessionId":"session-1","hookEventName":"user_prompt_submit","promptId":"prompt-1","prompt":"<user_query>\nhello\n</user_query>"}`,
			wantRuntimeVersion: "0.2.93", wantVersionSource: "cli",
		},
		{
			name: "cursor", runtime: RuntimeCursor, configuredVersion: "3.10.0",
			hook:               `{"conversation_id":"session-1","generation_id":"generation-1","hook_event_name":"beforeSubmitPrompt","prompt":"hello","model":"grok-4.5","cursor_version":"3.11.13"}`,
			wantRuntimeVersion: "3.11.13", wantVersionSource: "hook", wantModel: "grok-4.5", wantModelSource: "hook",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
			loc, err := EnsureLocation("home")
			if err != nil {
				t.Fatal(err)
			}
			if err := SaveConfig(Config{
				Runtime: tc.runtime, RuntimeVersion: tc.configuredVersion, CaptureMode: ModeRaw,
				Account: "default", Realm: "default", Agent: "scott",
				AgentID: "agent_1", AgentName: "scott", Location: loc,
			}); err != nil {
				t.Fatal(err)
			}
			event := enqueueTestHook(t, tc.runtime, tc.hook)
			entries := event.Entries()
			if len(entries) != 1 {
				t.Fatalf("entries = %d", len(entries))
			}
			var payload map[string]any
			if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			provenance, ok := payload["provenance"].(map[string]any)
			if !ok {
				t.Fatalf("provenance = %#v", payload["provenance"])
			}
			assertNullableProvenance(t, provenance, "runtime", tc.runtime)
			assertNullableProvenance(t, provenance, "runtime_version", tc.wantRuntimeVersion)
			assertNullableProvenance(t, provenance, "model_provider", tc.wantModelProvider)
			assertNullableProvenance(t, provenance, "model", tc.wantModel)
			sources, ok := provenance["sources"].(map[string]any)
			if !ok {
				t.Fatalf("sources = %#v", provenance["sources"])
			}
			assertNullableProvenance(t, sources, "runtime", "integration")
			assertNullableProvenance(t, sources, "runtime_version", tc.wantVersionSource)
			assertNullableProvenance(t, sources, "model_provider", tc.wantProviderSource)
			assertNullableProvenance(t, sources, "model", tc.wantModelSource)
			if entries[0].Model != tc.wantModel {
				t.Fatalf("entry model = %q, want %q", entries[0].Model, tc.wantModel)
			}
		})
	}
}

func TestRuntimeVersionIsPinnedToRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	loc, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	save := func(version string) {
		t.Helper()
		if err := SaveConfig(Config{
			Runtime: RuntimeCodex, RuntimeVersion: version, CaptureMode: ModeRaw,
			Account: "default", Realm: "default", Agent: "scott",
			AgentID: "agent_1", AgentName: "scott", Location: loc,
		}); err != nil {
			t.Fatal(err)
		}
	}
	save("1.0.0")
	start := enqueueTestHook(t, RuntimeCodex, `{"session_id":"session-1","hook_event_name":"SessionStart"}`)
	save("2.0.0")
	prompt := enqueueTestHook(t, RuntimeCodex, `{"session_id":"session-1","hook_event_name":"UserPromptSubmit","prompt":"hello"}`)
	if prompt.RunID != start.RunID || prompt.RuntimeVersion != "1.0.0" {
		t.Fatalf("same run provenance changed: start=%#v prompt=%#v", start, prompt)
	}
	resumed := enqueueTestHook(t, RuntimeCodex, `{"session_id":"session-1","hook_event_name":"SessionStart"}`)
	if resumed.RunID == start.RunID || resumed.RuntimeVersion != "2.0.0" {
		t.Fatalf("resumed run = %#v", resumed)
	}
}

func assertNullableProvenance(t *testing.T, value map[string]any, key, want string) {
	t.Helper()
	got, ok := value[key]
	if !ok {
		t.Fatalf("missing provenance key %q", key)
	}
	if want == "" {
		if got != nil {
			t.Fatalf("%s = %#v, want null", key, got)
		}
		return
	}
	if got != want {
		t.Fatalf("%s = %#v, want %q", key, got, want)
	}
}

func enqueueTestHook(t *testing.T, runtime, raw string) Event {
	t.Helper()
	event, err := EnqueueHook(runtime, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return event
}
