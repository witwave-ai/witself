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
		if _, err := InstallHooks(RuntimeClaudeCode, ModeRaw, "/usr/local/bin/witself"); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), hookCommandMarker) != 8 {
		t.Fatalf("witself hook count = %d, want 8\n%s", strings.Count(string(raw), hookCommandMarker), raw)
	}
	if !strings.Contains(string(raw), "custom-check") || !strings.Contains(string(raw), "EXISTING") {
		t.Fatalf("unrelated settings were lost:\n%s", raw)
	}
}

func TestCodexHookSetUsesOnlySupportedEvents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	path, err := InstallHooks(RuntimeCodex, ModeRaw, "/usr/local/bin/witself")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "PreToolUse", "PostToolUse"} {
		if !strings.Contains(string(raw), `"`+event+`"`) {
			t.Errorf("missing %s", event)
		}
	}
	for _, event := range []string{"SessionEnd", "StopFailure", "PostToolUseFailure"} {
		if strings.Contains(string(raw), `"`+event+`"`) {
			t.Errorf("unsupported Codex event %s was installed", event)
		}
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
