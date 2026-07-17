package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestCodexAutoReviewRemainsInternalAndPreservesParentTurn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	loc, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(Config{
		Runtime: RuntimeCodex, CaptureMode: ModeRaw,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: loc,
	}); err != nil {
		t.Fatal(err)
	}

	start := enqueueTestHook(t, RuntimeCodex, `{"session_id":"session-1","hook_event_name":"SessionStart","model":"gpt-5.6-sol"}`)
	prompt := enqueueTestHook(t, RuntimeCodex, `{"session_id":"session-1","hook_event_name":"UserPromptSubmit","turn_id":"parent-turn","model":"gpt-5.6-sol","prompt":"real user prompt"}`)
	permission := enqueueTestHook(t, RuntimeCodex, `{"session_id":"session-1","hook_event_name":"PermissionRequest","turn_id":"parent-turn","model":"gpt-5.6-sol","tool_name":"mcp__witself__write"}`)
	review := enqueueTestHook(t, RuntimeCodex, `{"session_id":"session-1","hook_event_name":"UserPromptSubmit","turn_id":"review-turn","model":"codex-auto-review","prompt":"internal-review-canary","tool_input":{"secret":"internal-tool-canary"},"reason":"internal-reason-canary"}`)
	stop := enqueueTestHook(t, RuntimeCodex, `{"session_id":"session-1","hook_event_name":"Stop","model":"gpt-5.6-sol","last_assistant_message":"done"}`)

	if start.RunID == "" || prompt.RunID != start.RunID || permission.RunID != start.RunID ||
		review.RunID != start.RunID || stop.RunID != start.RunID {
		t.Fatal("nested review changed the parent run")
	}
	if review.HookEvent != HookEventCodexPermissionReview || review.NativeHookEvent != "UserPromptSubmit" ||
		review.Kind != "permission.review.started" || review.Role != "system" ||
		review.Body != "automatic permission review started" || review.TurnID != "review-turn" ||
		review.Model != codexAutoReviewModel || review.ModelSource != "hook" {
		t.Fatalf("normalized review metadata = %#v", review)
	}
	if len(review.Raw) != 0 || len(review.Data) != 0 || strings.Contains(review.Body, "internal-review-canary") {
		t.Fatal("internal approval envelope was retained as transcript content")
	}
	reviewEntries := review.Entries()
	if len(reviewEntries) != 1 || reviewEntries[0].Role != "system" {
		t.Fatalf("normalized review entry retained internal prompt material: %#v", reviewEntries)
	}
	for _, canary := range []string{"internal-review-canary", "internal-tool-canary", "internal-reason-canary"} {
		if bytes.Contains(reviewEntries[0].Payload, []byte(canary)) || strings.Contains(reviewEntries[0].Body, canary) {
			t.Fatalf("normalized review entry retained %q: %#v", canary, reviewEntries)
		}
	}
	if stop.TurnID != prompt.TurnID || stop.ReplyToEventID != prompt.ID ||
		stop.Entries()[0].ReplyToExternalID != prompt.ID+":0" {
		t.Fatalf("parent reply linkage changed: prompt=%#v stop=%#v", prompt, stop)
	}

	pending, err := Pending(RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	userEntries := 0
	for _, item := range pending {
		for _, entry := range item.Event.Entries() {
			if entry.Role == "user" {
				userEntries++
			}
		}
	}
	if len(pending) != 5 || userEntries != 1 {
		t.Fatalf("pending/user entries = %d/%d, want 5/1", len(pending), userEntries)
	}
}

func TestCodexAutoReviewNormalizationRequiresExactRuntimeAndModel(t *testing.T) {
	for _, tc := range []struct {
		name, runtimeName, model, wantEvent string
		wantPrompt                          string
	}{
		{"exact Codex internal model", RuntimeCodex, codexAutoReviewModel, HookEventCodexPermissionReview, ""},
		{"different Codex model", RuntimeCodex, "codex-auto-review-preview", "UserPromptSubmit", "prompt-canary"},
		{"other runtime", RuntimeClaudeCode, codexAutoReviewModel, "UserPromptSubmit", "prompt-canary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := hookInput{HookEventName: "UserPromptSubmit", Model: tc.model, Prompt: "prompt-canary"}
			if err := normalizeHookInput(tc.runtimeName, &input); err != nil {
				t.Fatal(err)
			}
			if input.HookEventName != tc.wantEvent || input.Prompt != tc.wantPrompt ||
				input.NativeHookEvent != "UserPromptSubmit" {
				t.Fatalf("normalized input = %#v", input)
			}
		})
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

func TestActivityObservationCanonicalizesEveryCurrentRuntimeWithoutContent(t *testing.T) {
	for _, test := range []struct {
		runtime, raw, wantEvent string
	}{
		{RuntimeCodex, `{"session_id":"session-1","hook_event_name":"UserPromptSubmit","prompt":"private codex prompt","cwd":"/private/codex"}`, "UserPromptSubmit"},
		{RuntimeClaudeCode, `{"session_id":"session-1","hook_event_name":"UserPromptSubmit","prompt":"private claude prompt","cwd":"/private/claude"}`, "UserPromptSubmit"},
		{RuntimeGrokBuild, `{"sessionId":"session-1","hookEventName":"user_prompt_submit","promptId":"prompt-1","prompt":"private grok prompt","cwd":"/private/grok"}`, "UserPromptSubmit"},
		{RuntimeCursor, `{"conversation_id":"session-1","generation_id":"generation-1","hook_event_name":"afterAgentResponse","text":"private cursor response","cwd":"/private/cursor"}`, "AgentResponse"},
	} {
		t.Run(test.runtime, func(t *testing.T) {
			t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
			location, err := EnsureLocation("home")
			if err != nil {
				t.Fatal(err)
			}
			if err := SaveConfig(Config{
				Runtime: test.runtime, CaptureMode: ModeRaw,
				Account: "default", Realm: "default", Agent: "scott",
				AgentID: "agent_1", AgentName: "scott", Location: location,
			}); err != nil {
				t.Fatal(err)
			}
			event := enqueueTestHook(t, test.runtime, test.raw)
			observation := event.ActivityObservation()
			if observation.Runtime != test.runtime || observation.LocationID != location.ID ||
				observation.Location != "home" || observation.Event != test.wantEvent ||
				observation.EventID != event.ID || !observation.EventOccurredAt.Equal(event.OccurredAt) {
				t.Fatalf("activity observation = %#v", observation)
			}
			formatted := fmt.Sprintf("%#v", observation)
			for _, forbidden := range []string{"private ", "/private/", "session-1", "prompt-1"} {
				if strings.Contains(formatted, forbidden) {
					t.Fatalf("activity observation leaked %q: %s", forbidden, formatted)
				}
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
	prompt := enqueueTestHook(t, RuntimeCursor, `{"session_id":"untrusted-session","conversation_id":"conversation-1","generation_id":"generation-1","hook_event_name":"beforeSubmitPrompt","cwd":"/src/witself","prompt":"<timestamp>2026-07-17T07:08:09.123456-06:00</timestamp>\n<user_query>\nhello\n</user_query>"}`)
	response := enqueueTestHook(t, RuntimeCursor, `{"conversation_id":"conversation-1","generation_id":"generation-1","hook_event_name":"afterAgentResponse","cwd":"/src/witself","text":"hi there","input_tokens":12,"output_tokens":4}`)
	if prompt.HookEvent != "UserPromptSubmit" || prompt.NativeHookEvent != "beforeSubmitPrompt" ||
		prompt.TurnID != "generation-1" || prompt.SessionID != "conversation-1" || prompt.Body != "hello" {
		t.Fatalf("prompt = %#v", prompt)
	}
	if !bytes.Contains(prompt.Raw, []byte("<timestamp>2026-07-17T07:08:09.123456-06:00</timestamp>")) {
		t.Fatalf("raw provider envelope was not retained: %s", prompt.Raw)
	}
	if response.Kind != "message.assistant" || response.Body != "hi there" || response.ReplyToEventID != prompt.ID {
		t.Fatalf("response = %#v", response)
	}
	entries := response.Entries()
	if len(entries) != 1 || !strings.Contains(string(entries[0].Payload), `"input_tokens":12`) || !strings.Contains(string(entries[0].Payload), `"native_event":"afterAgentResponse"`) {
		t.Fatalf("payload = %s", entries[0].Payload)
	}
}

func TestCursorPromptNormalizationRequiresExactNativeEnvelope(t *testing.T) {
	timestamp := "2026-07-17T07:08:09.123456-06:00"
	wrap := func(body string) string {
		return "<timestamp>" + timestamp + "</timestamp>\n<user_query>\n" + body + "\n</user_query>"
	}
	for _, tc := range []struct {
		name    string
		runtime string
		input   string
		want    string
	}{
		{name: "exact cursor envelope", runtime: RuntimeCursor, input: wrap("hello"), want: "hello"},
		{name: "multiline prompt and ordinary markup", runtime: RuntimeCursor, input: wrap("first\n<div>second</div>"), want: "first\n<div>second</div>"},
		{name: "empty prompt", runtime: RuntimeCursor, input: wrap(""), want: ""},
		{name: "cursor only", runtime: RuntimeClaudeCode, input: wrap("hello"), want: wrap("hello")},
		{name: "grok unchanged", runtime: RuntimeGrokBuild, input: wrap("hello"), want: wrap("hello")},
		{name: "missing timestamp envelope", runtime: RuntimeCursor, input: "<user_query>\nhello\n</user_query>", want: "<user_query>\nhello\n</user_query>"},
		{name: "empty timestamp", runtime: RuntimeCursor, input: "<timestamp></timestamp>\n<user_query>\nhello\n</user_query>", want: "<timestamp></timestamp>\n<user_query>\nhello\n</user_query>"},
		{name: "blank timestamp", runtime: RuntimeCursor, input: "<timestamp> \t</timestamp>\n<user_query>\nhello\n</user_query>", want: "<timestamp> \t</timestamp>\n<user_query>\nhello\n</user_query>"},
		{name: "multiline timestamp", runtime: RuntimeCursor, input: "<timestamp>first\nsecond</timestamp>\n<user_query>\nhello\n</user_query>", want: "<timestamp>first\nsecond</timestamp>\n<user_query>\nhello\n</user_query>"},
		{name: "unicode line separator in timestamp", runtime: RuntimeCursor, input: "<timestamp>first\u2028second</timestamp>\n<user_query>\nhello\n</user_query>", want: "<timestamp>first\u2028second</timestamp>\n<user_query>\nhello\n</user_query>"},
		{name: "angle bracket in timestamp", runtime: RuntimeCursor, input: "<timestamp>first>second</timestamp>\n<user_query>\nhello\n</user_query>", want: "<timestamp>first>second</timestamp>\n<user_query>\nhello\n</user_query>"},
		{name: "maximum timestamp bytes", runtime: RuntimeCursor, input: "<timestamp>" + strings.Repeat("t", 256) + "</timestamp>\n<user_query>\nhello\n</user_query>", want: "hello"},
		{name: "oversized timestamp bytes", runtime: RuntimeCursor, input: "<timestamp>" + strings.Repeat("t", 257) + "</timestamp>\n<user_query>\nhello\n</user_query>", want: "<timestamp>" + strings.Repeat("t", 257) + "</timestamp>\n<user_query>\nhello\n</user_query>"},
		{name: "leading bytes", runtime: RuntimeCursor, input: "prefix" + wrap("hello"), want: "prefix" + wrap("hello")},
		{name: "trailing newline", runtime: RuntimeCursor, input: wrap("hello") + "\n", want: wrap("hello") + "\n"},
		{name: "trailing bytes", runtime: RuntimeCursor, input: wrap("hello") + "suffix", want: wrap("hello") + "suffix"},
		{name: "crlf separators", runtime: RuntimeCursor, input: "<timestamp>" + timestamp + "</timestamp>\r\n<user_query>\r\nhello\r\n</user_query>", want: "<timestamp>" + timestamp + "</timestamp>\r\n<user_query>\r\nhello\r\n</user_query>"},
		{name: "missing query newline", runtime: RuntimeCursor, input: "<timestamp>" + timestamp + "</timestamp>\n<user_query>hello\n</user_query>", want: "<timestamp>" + timestamp + "</timestamp>\n<user_query>hello\n</user_query>"},
		{name: "repeated envelopes", runtime: RuntimeCursor, input: wrap("first") + wrap("second"), want: wrap("first") + wrap("second")},
		{name: "nested exact query", runtime: RuntimeCursor, input: wrap("<user_query>\ninner\n</user_query>"), want: wrap("<user_query>\ninner\n</user_query>")},
		{name: "nested malformed query", runtime: RuntimeCursor, input: wrap("<user_query role=user>inner</user_query role=user>"), want: wrap("<user_query role=user>inner</user_query role=user>")},
		{name: "timestamp tag in body", runtime: RuntimeCursor, input: wrap("explain <timestamp>now</timestamp>"), want: wrap("explain <timestamp>now</timestamp>")},
		{name: "missing query close", runtime: RuntimeCursor, input: "<timestamp>" + timestamp + "</timestamp>\n<user_query>\nhello", want: "<timestamp>" + timestamp + "</timestamp>\n<user_query>\nhello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeUserPromptBody(tc.runtime, tc.input); got != tc.want {
				t.Fatalf("normalized prompt = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCursorHeadlessSessionEndBackfillsVisibleNativeMessages(t *testing.T) {
	home := t.TempDir()
	cursorHome := filepath.Join(home, ".cursor")
	t.Setenv("HOME", home)
	t.Setenv("CURSOR_DATA_DIR", cursorHome)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	loc, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(Config{
		Runtime: RuntimeCursor, RuntimeVersion: "3.12.10", CaptureMode: ModeRaw,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: loc,
	}); err != nil {
		t.Fatal(err)
	}

	sessionDir := filepath.Join(cursorHome, "projects", "workspace", "agent-transcripts", "session-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "session-1.jsonl")
	transcript := strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>2026-07-17T07:08:09.123456-06:00</timestamp>\n<user_query>\nheadless prompt\n</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"working"},{"type":"tool_use","name":"CallMcpTool","input":{}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"CallMcpTool","input":{}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"finished"}]}}`,
		`{"type":"turn_ended","status":"success"}`,
	}, "\n") + "\n"
	// Cursor currently creates its native transcripts as 0644. They remain
	// trusted because both the file and directory chain are owned by this user
	// and none is writable by another user.
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}

	start := enqueueTestHook(t, RuntimeCursor, `{"conversation_id":"session-1","generation_id":"generation-1","hook_event_name":"sessionStart","cursor_version":"3.12.10"}`)
	endRaw, _ := json.Marshal(map[string]any{
		"conversation_id": "session-1", "generation_id": "generation-1",
		"hook_event_name": "sessionEnd", "cursor_version": "3.12.10",
		"model": "cursor-grok-4.5-high", "transcript_path": transcriptPath,
	})
	end := enqueueTestHook(t, RuntimeCursor, string(endRaw))
	pending, err := Pending(RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending events = %d, want one atomic session start and session end bundle", len(pending))
	}
	if pending[1].Event.ID != end.ID {
		t.Fatalf("persisted terminal event = %#v, want id %s", pending[1].Event, end.ID)
	}
	end = pending[1].Event
	if len(end.RecoveredMessages) != 2 {
		t.Fatalf("recovered messages = %#v", end.RecoveredMessages)
	}
	prompt := end.RecoveredMessages[0]
	response := end.RecoveredMessages[1]
	if prompt.HookEvent != "UserPromptSubmit" || prompt.NativeHookEvent != "nativeTranscriptUser" ||
		prompt.Kind != "message.user" || prompt.Role != "user" || prompt.Body != "headless prompt" {
		t.Fatalf("fallback prompt = %#v", prompt)
	}
	if response.HookEvent != "AgentResponse" || response.NativeHookEvent != "nativeTranscriptAssistant" ||
		response.Kind != "message.assistant" || response.Role != "assistant" || response.Body != "finished" ||
		response.ReplyToEventID != prompt.ID {
		t.Fatalf("fallback response = %#v", response)
	}
	if end.RunID != start.RunID || end.RuntimeVersion != "3.12.10" ||
		prompt.TurnID == "" || response.TurnID != prompt.TurnID {
		t.Fatalf("fallback provenance/order mismatch: start=%#v prompt=%#v response=%#v end=%#v", start, prompt, response, end)
	}
	var fallbackData map[string]any
	if err := json.Unmarshal(prompt.Data, &fallbackData); err != nil {
		t.Fatal(err)
	}
	if fallbackData["source"] != "cursor_native_transcript" || fallbackData["fallback"] != true {
		t.Fatalf("fallback data = prompt=%s response=%s", prompt.Data, response.Data)
	}
	entries := end.Entries()
	if len(entries) != 3 {
		t.Fatalf("expanded entries = %#v", entries)
	}
	if entries[0].Role != "user" || entries[0].Body != "headless prompt" ||
		entries[1].Role != "assistant" || entries[1].Body != "finished" ||
		entries[1].ReplyToExternalID != entries[0].ExternalID ||
		entries[2].ExternalID != entryExternalID(end.ID, 0) || entries[0].Model != "cursor-grok-4.5-high" {
		t.Fatalf("expanded message order/linkage = %#v", entries)
	}
	var promptPayload, responsePayload, endPayload map[string]any
	for raw, target := range map[string]*map[string]any{
		string(entries[0].Payload): &promptPayload,
		string(entries[1].Payload): &responsePayload,
		string(entries[2].Payload): &endPayload,
	} {
		if err := json.Unmarshal([]byte(raw), target); err != nil {
			t.Fatal(err)
		}
	}
	promptProvenance, _ := promptPayload["provenance"].(map[string]any)
	promptData, _ := promptPayload["data"].(map[string]any)
	if promptPayload["event_id"] != prompt.ID || responsePayload["event_id"] != response.ID ||
		promptPayload["source_transcript_path"] != transcriptPath || promptProvenance["runtime"] != RuntimeCursor ||
		promptProvenance["runtime_version"] != "3.12.10" || promptData["source"] != "cursor_native_transcript" ||
		promptData["fallback"] != true || promptPayload["raw"] != nil || responsePayload["raw"] != nil ||
		promptPayload["run_id"] != nil || promptPayload["occurred_at"] != nil ||
		endPayload["run_id"] == nil || endPayload["occurred_at"] == nil || endPayload["raw"] == nil {
		t.Fatalf("expanded payloads = prompt=%s response=%s end=%s", entries[0].Payload, entries[1].Payload, entries[2].Payload)
	}

	retry := enqueueTestHook(t, RuntimeCursor, string(endRaw))
	retryEntries := retry.Entries()
	if len(retry.RecoveredMessages) != 2 || retry.RecoveredMessages[0].ID != prompt.ID ||
		retry.RecoveredMessages[1].ID != response.ID || retryEntries[0].ExternalID != entries[0].ExternalID ||
		retryEntries[1].ExternalID != entries[1].ExternalID {
		t.Fatalf("recovered ids are not stable across SessionEnd retry: first=%#v retry=%#v", end.RecoveredMessages, retry.RecoveredMessages)
	}
	for i := 0; i < 2; i++ {
		if retryEntries[i].Role != entries[i].Role || retryEntries[i].Body != entries[i].Body ||
			retryEntries[i].Model != entries[i].Model || retryEntries[i].ReplyToExternalID != entries[i].ReplyToExternalID ||
			!bytes.Equal(retryEntries[i].Payload, entries[i].Payload) {
			t.Fatalf("recovered entry %d changed across SessionEnd retry:\nfirst=%#v\nretry=%#v", i, entries[i], retryEntries[i])
		}
	}
}

func TestCursorNativeHooksDoNotDuplicateTranscriptFallback(t *testing.T) {
	home := t.TempDir()
	cursorHome := filepath.Join(home, ".cursor")
	t.Setenv("HOME", home)
	t.Setenv("CURSOR_CONFIG_DIR", cursorHome)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	loc, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(Config{
		Runtime: RuntimeCursor, RuntimeVersion: "3.12.10", CaptureMode: ModeRaw,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: loc,
	}); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(cursorHome, "projects", "workspace", "agent-transcripts", "session-2")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "session-2.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"native prompt"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"native response"}]}}`,
	}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	enqueueTestHook(t, RuntimeCursor, `{"conversation_id":"session-2","generation_id":"generation-2","hook_event_name":"sessionStart","cursor_version":"3.12.10"}`)
	enqueueTestHook(t, RuntimeCursor, `{"conversation_id":"session-2","generation_id":"generation-2","hook_event_name":"beforeSubmitPrompt","cursor_version":"3.12.10","prompt":"native prompt"}`)
	enqueueTestHook(t, RuntimeCursor, `{"conversation_id":"session-2","generation_id":"generation-2","hook_event_name":"afterAgentResponse","cursor_version":"3.12.10","text":"native response"}`)
	endRaw, _ := json.Marshal(map[string]any{
		"conversation_id": "session-2", "generation_id": "generation-2",
		"hook_event_name": "sessionEnd", "cursor_version": "3.12.10", "transcript_path": transcriptPath,
	})
	enqueueTestHook(t, RuntimeCursor, string(endRaw))
	pending, err := Pending(RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 4 {
		t.Fatalf("pending events = %d, want only the four native hook events", len(pending))
	}
	for _, item := range pending {
		if strings.HasPrefix(item.Event.NativeHookEvent, "nativeTranscript") || len(item.Event.RecoveredMessages) != 0 {
			t.Fatalf("unexpected fallback event: %#v", item.Event)
		}
	}
}

func TestCursorMessagesModeDoesNotUseNativeTranscriptFallback(t *testing.T) {
	home := t.TempDir()
	cursorHome := filepath.Join(home, ".cursor")
	t.Setenv("HOME", home)
	t.Setenv("CURSOR_DATA_DIR", cursorHome)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	loc, err := EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(Config{
		Runtime: RuntimeCursor, RuntimeVersion: "3.12.10", CaptureMode: ModeMessages,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: loc,
	}); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(cursorHome, "projects", "workspace", "agent-transcripts", "session-3")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "session-3.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(strings.Join([]string{
		`{"role":"user","message":{"content":"prompt"}}`,
		`{"role":"assistant","message":{"content":"response"}}`,
	}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	enqueueTestHook(t, RuntimeCursor, `{"conversation_id":"session-3","hook_event_name":"sessionStart","cursor_version":"3.12.10"}`)
	endRaw, _ := json.Marshal(map[string]any{
		"conversation_id": "session-3", "hook_event_name": "sessionEnd",
		"cursor_version": "3.12.10", "transcript_path": transcriptPath,
	})
	end := enqueueTestHook(t, RuntimeCursor, string(endRaw))
	pending, err := Pending(RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("messages-mode events = %d, want only session start/end", len(pending))
	}
	if len(end.RecoveredMessages) != 0 {
		t.Fatalf("messages mode unexpectedly recovered raw transcript messages: %#v", end.RecoveredMessages)
	}
}

func TestCursorVisibleMessagesRejectsUntrustedOrAmbiguousTranscript(t *testing.T) {
	home := t.TempDir()
	cursorHome := filepath.Join(home, ".cursor")
	t.Setenv("HOME", home)
	t.Setenv("CURSOR_CONFIG_DIR", cursorHome)
	projectsRoot := filepath.Join(cursorHome, "projects")
	if err := os.MkdirAll(projectsRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(home, "outside.jsonl")
	if err := os.WriteFile(outside, []byte(`{"role":"user","message":{"content":"prompt"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readCursorVisibleMessages(outside, "outside"); err == nil {
		t.Fatal("accepted a Cursor transcript outside the trusted project store")
	}

	sessionDir := filepath.Join(projectsRoot, "workspace", "agent-transcripts", "session-3")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ambiguous := filepath.Join(sessionDir, "session-3.jsonl")
	if err := os.WriteFile(ambiguous, []byte(strings.Join([]string{
		`{"role":"user","message":{"content":"first"}}`,
		`{"role":"assistant","message":{"content":"answer"}}`,
		`{"role":"user","message":{"content":"second"}}`,
	}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readCursorVisibleMessages(ambiguous, "session-3"); err == nil {
		t.Fatal("accepted an ambiguous multi-prompt Cursor headless transcript")
	}
	if _, _, err := readCursorVisibleMessages(ambiguous, "another-session"); err == nil {
		t.Fatal("accepted a Cursor transcript for a different conversation id")
	}

	symlinkDir := filepath.Join(projectsRoot, "workspace", "agent-transcripts", "session-4")
	if err := os.MkdirAll(symlinkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(symlinkDir, "session-4.jsonl")
	if err := os.Symlink(outside, symlinkPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readCursorVisibleMessages(symlinkPath, "session-4"); err == nil {
		t.Fatal("accepted a symlinked Cursor transcript")
	}

	writableDir := filepath.Join(projectsRoot, "workspace", "agent-transcripts", "writable")
	if err := os.MkdirAll(writableDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writablePath := filepath.Join(writableDir, "writable.jsonl")
	valid := strings.Join([]string{
		`{"role":"user","message":{"content":"prompt"}}`,
		`{"role":"assistant","message":{"content":"response"}}`,
		`{"type":"turn_ended","status":"success"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(writablePath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writablePath, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readCursorVisibleMessages(writablePath, "writable"); err == nil {
		t.Fatal("accepted a group/world-writable Cursor transcript")
	}
	if err := os.Chmod(writablePath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writableDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readCursorVisibleMessages(writablePath, "writable"); err == nil {
		t.Fatal("accepted a Cursor transcript through a group/world-writable directory")
	}

	for name, terminal := range map[string]string{
		"missing-terminal": "",
		"failed-terminal":  `{"type":"turn_ended","status":"error"}`,
	} {
		dir := filepath.Join(projectsRoot, "workspace", "agent-transcripts", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		lines := []string{
			`{"role":"user","message":{"content":"prompt"}}`,
			`{"role":"assistant","message":{"content":"response"}}`,
		}
		if terminal != "" {
			lines = append(lines, terminal)
		}
		path := filepath.Join(dir, name+".jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readCursorVisibleMessages(path, name); err == nil {
			t.Fatalf("accepted %s Cursor transcript", name)
		}
	}
}

func TestGrokCaptureNormalizesPayloadAndDefersNativeResponse(t *testing.T) {
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
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(grokHome, "sessions"), filepath.Join(grokHome, "sessions", "workspace"), sessionDir} {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
	updates := strings.Join([]string{
		`{"method":"session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","_meta":{"modelId":"grok-4.5"},"content":{"type":"text","text":"first"}}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"second"}}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-other"},"update":{"sessionUpdate":"agent_message_chunk","_meta":{"modelId":"wrong-model"},"content":{"type":"text","text":"unrelated"}}}}`,
		`{"method":"session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1"}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(updates), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(transcriptPath, 0o644); err != nil {
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
	if stop.Kind != "turn.completed" || stop.Role != "system" || stop.ReplyToEventID != prompt.ID || stop.Model != "" || stop.ModelSource != "" {
		t.Fatalf("stop = %#v", stop)
	}
	body, model, complete, err := readGrokAssistantTurn(transcriptPath, "prompt-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !complete || body != "first\n\nsecond" || model != "grok-4.5" {
		t.Fatalf("native response = %t / %q / %q", complete, body, model)
	}
}

func TestFinalizePendingGrokStopAfterHookReturns(t *testing.T) {
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
		Runtime: RuntimeGrokBuild, RuntimeVersion: "0.2.101", CaptureMode: ModeRaw,
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
	if err := os.WriteFile(transcriptPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	promptRaw, _ := json.Marshal(map[string]any{
		"sessionId": "session-1", "hookEventName": "user_prompt_submit", "promptId": "prompt-1",
		"prompt": "hello", "transcriptPath": transcriptPath,
	})
	prompt := enqueueTestHook(t, RuntimeGrokBuild, string(promptRaw))
	stopRaw, _ := json.Marshal(map[string]any{
		"sessionId": "session-1", "hookEventName": "stop", "promptId": "prompt-1",
		"reason": "end_turn", "transcriptPath": transcriptPath,
	})
	started := time.Now()
	stop := enqueueTestHook(t, RuntimeGrokBuild, string(stopRaw))
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Stop hook waited for provider transcript: %s", elapsed)
	}
	if stop.Kind != "turn.completed" || stop.Role != "system" {
		t.Fatalf("unresolved Stop = %#v", stop)
	}
	pending, err := Pending(RuntimeGrokBuild)
	if err != nil {
		t.Fatal(err)
	}
	var pendingStop PendingEvent
	for _, item := range pending {
		if item.Event.ID == stop.ID {
			pendingStop = item
		}
	}
	if pendingStop.Path == "" {
		t.Fatalf("Stop event not found in outbox: %#v", pending)
	}
	original, err := os.ReadFile(pendingStop.Path)
	if err != nil {
		t.Fatal(err)
	}

	updates := strings.Join([]string{
		`{"method":"session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-other"},"update":{"sessionUpdate":"agent_message_chunk","_meta":{"modelId":"wrong-model"},"content":{"text":"wrong"}}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","_meta":{"modelId":"grok-4.5"},"content":{"text":"final answer"}}}}`,
		`{"method":"session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1"}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}

	finalized, ready, err := finalizePendingWithin(pendingStop, 100*time.Millisecond, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !ready || finalized.Event.ID != stop.ID || finalized.Event.Kind != "message.assistant" ||
		finalized.Event.Role != "assistant" || finalized.Event.Body != "final answer" ||
		finalized.Event.Model != "grok-4.5" || finalized.Event.ModelSource != "native_transcript" ||
		finalized.Event.ReplyToEventID != prompt.ID || !finalized.Event.OccurredAt.Equal(stop.OccurredAt) {
		t.Fatalf("finalized Stop = %#v", finalized.Event)
	}
	persisted, err := os.ReadFile(pendingStop.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(original, persisted) {
		t.Fatal("finalized Stop was not persisted before upload")
	}
	retry, ready, err := finalizePendingWithin(finalized, 100*time.Millisecond, 5*time.Millisecond)
	if err != nil || !ready {
		t.Fatalf("finalized retry = %t / %v", ready, err)
	}
	retryPersisted, err := os.ReadFile(pendingStop.Path)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Event.ID != finalized.Event.ID || !bytes.Equal(persisted, retryPersisted) ||
		fmt.Sprintf("%#v", retry.Event.Entries()) != fmt.Sprintf("%#v", finalized.Event.Entries()) {
		t.Fatalf("finalized retry changed durable content:\nfirst=%#v\nretry=%#v", finalized.Event, retry.Event)
	}
}

func TestFinalizePendingGrokStopDefersWithoutTerminalFence(t *testing.T) {
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
	if err := os.WriteFile(transcriptPath, []byte(strings.Join([]string{
		`{"method":"session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"not yet fenced"}}}}`,
	}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	promptRaw, _ := json.Marshal(map[string]any{
		"sessionId": "session-1", "hookEventName": "user_prompt_submit", "promptId": "prompt-1", "prompt": "hello",
	})
	enqueueTestHook(t, RuntimeGrokBuild, string(promptRaw))
	stopRaw, _ := json.Marshal(map[string]any{
		"sessionId": "session-1", "hookEventName": "stop", "promptId": "prompt-1",
		"reason": "end_turn", "transcriptPath": transcriptPath,
	})
	stop := enqueueTestHook(t, RuntimeGrokBuild, string(stopRaw))
	pending, err := Pending(RuntimeGrokBuild)
	if err != nil {
		t.Fatal(err)
	}
	var pendingStop PendingEvent
	for _, item := range pending {
		if item.Event.ID == stop.ID {
			pendingStop = item
		}
	}
	before, err := os.ReadFile(pendingStop.Path)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, ready, err := finalizePendingWithin(pendingStop, 50*time.Millisecond, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(pendingStop.Path)
	if err != nil {
		t.Fatal(err)
	}
	if ready || unchanged.Event.Kind != "turn.completed" || !bytes.Equal(before, after) {
		t.Fatalf("unfenced Stop was changed or released: ready=%t event=%#v", ready, unchanged.Event)
	}
}

func TestFinalizePendingContentFreeGrokTurnPersistsRetryFence(t *testing.T) {
	home := t.TempDir()
	grokHome := filepath.Join(home, ".grok")
	t.Setenv("HOME", home)
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	sessionDir := filepath.Join(grokHome, "sessions", "workspace", "session-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
	updates := strings.Join([]string{
		`{"method":"session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
		`{"method":"session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1"}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"prompt_id": "prompt-1", "reason": "end_turn"})
	event := Event{
		SchemaVersion: SchemaVersion, ID: "evt_content_free", Runtime: RuntimeGrokBuild,
		CaptureMode: ModeRaw, SessionID: "session-1", TurnID: "prompt-1",
		HookEvent: "Stop", NativeHookEvent: "stop", Kind: "turn.completed", Role: "system", Body: "end_turn",
		SourceTranscriptPath: transcriptPath, Data: data, OccurredAt: time.Now().UTC(),
	}
	if err := writeOutboxEvent(event); err != nil {
		t.Fatal(err)
	}
	pending, err := Pending(RuntimeGrokBuild)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %#v / %v", pending, err)
	}
	finalized, ready, err := finalizePendingWithin(pending[0], 100*time.Millisecond, 5*time.Millisecond)
	if err != nil || !ready || !finalized.Event.NativeTurnFinalized || finalized.Event.Kind != "turn.completed" {
		t.Fatalf("content-free finalization = ready %t / event %#v / err %v", ready, finalized.Event, err)
	}
	persisted, err := os.ReadFile(finalized.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(transcriptPath); err != nil {
		t.Fatal(err)
	}
	retry, ready, err := finalizePendingWithin(finalized, 100*time.Millisecond, 5*time.Millisecond)
	if err != nil || !ready {
		t.Fatalf("content-free retry = ready %t / err %v", ready, err)
	}
	retryPersisted, err := os.ReadFile(retry.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(persisted, retryPersisted) ||
		fmt.Sprintf("%#v", retry.Event.Entries()) != fmt.Sprintf("%#v", finalized.Event.Entries()) {
		t.Fatalf("content-free retry changed durable event:\nfirst=%#v\nretry=%#v", finalized.Event, retry.Event)
	}
}

func TestCompleteGrokAssistantTurnWaitsForDelayedTerminalFence(t *testing.T) {
	grokHome := filepath.Join(t.TempDir(), ".grok")
	t.Setenv("GROK_HOME", grokHome)
	sessionDir := filepath.Join(grokHome, "sessions", "workspace", "session-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
	if err := os.WriteFile(transcriptPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	writeErr := make(chan error, 1)
	go func() {
		time.Sleep(75 * time.Millisecond)
		file, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			writeErr <- err
			return
		}
		defer func() { _ = file.Close() }()
		first := strings.Join([]string{
			`{"method":"session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
			`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","_meta":{"modelId":"grok-4.5"},"content":{"type":"text","text":"first"}}}}`,
		}, "\n") + "\n"
		if _, err := file.WriteString(first); err != nil {
			writeErr <- err
			return
		}
		time.Sleep(100 * time.Millisecond)
		second := strings.Join([]string{
			`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"second"}}}}`,
			`{"method":"session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1"}}}`,
		}, "\n") + "\n"
		_, err = file.WriteString(second)
		writeErr <- err
	}()

	body, model, complete, err := readCompleteGrokAssistantTurn(transcriptPath, "prompt-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if !complete || body != "first\n\nsecond" || model != "grok-4.5" {
		t.Fatalf("complete Grok response = %t / %q / %q", complete, body, model)
	}
}

func TestCompleteGrokAssistantTurnWaitsForIncompleteTrailingLine(t *testing.T) {
	grokHome := filepath.Join(t.TempDir(), ".grok")
	t.Setenv("GROK_HOME", grokHome)
	sessionDir := filepath.Join(grokHome, "sessions", "workspace", "session-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
	first := strings.Join([]string{
		`{"method":"session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","_meta":{"modelId":"grok-4.5"},"content":{"type":"text","text":"first"}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}

	writeErr := make(chan error, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		file, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			writeErr <- err
			return
		}
		defer func() { _ = file.Close() }()
		second := `{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"second"}}}}` + "\n"
		midpoint := len(second) / 2
		if _, err := file.WriteString(second[:midpoint]); err != nil {
			writeErr <- err
			return
		}
		time.Sleep(300 * time.Millisecond)
		terminal := `{"method":"session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1"}}}` + "\n"
		_, err = file.WriteString(second[midpoint:] + terminal)
		writeErr <- err
	}()

	body, model, complete, err := readCompleteGrokAssistantTurn(transcriptPath, "prompt-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if !complete || body != "first\n\nsecond" || model != "grok-4.5" {
		t.Fatalf("complete Grok response = %t / %q / %q", complete, body, model)
	}
}

func TestCompleteGrokAssistantTurnFailsClosedWithoutTerminalFence(t *testing.T) {
	grokHome := filepath.Join(t.TempDir(), ".grok")
	t.Setenv("GROK_HOME", grokHome)
	sessionDir := filepath.Join(grokHome, "sessions", "workspace", "session-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
	line := strings.Join([]string{
		`{"method":"session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","_meta":{"modelId":"grok-4.5"},"content":{"type":"text","text":"possibly partial"}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	body, model, complete, err := readCompleteGrokAssistantTurnWithin(
		transcriptPath, "prompt-1", "session-1",
		75*time.Millisecond, 10*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	if complete || body != "possibly partial" || model != "grok-4.5" {
		t.Fatalf("unfenced Grok response = %t / %q / %q", complete, body, model)
	}
}

func TestGrokAssistantTurnRejectsMalformedCompletedRecordAfterStop(t *testing.T) {
	grokHome := filepath.Join(t.TempDir(), ".grok")
	t.Setenv("GROK_HOME", grokHome)
	sessionDir := filepath.Join(grokHome, "sessions", "workspace", "session-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
	updates := strings.Join([]string{
		`{"method":"session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"first"}}}}`,
		`{"malformed":`,
		`{"method":"session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1"}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readGrokAssistantTurn(transcriptPath, "prompt-1", "session-1"); err == nil ||
		!strings.Contains(err.Error(), "malformed record") {
		t.Fatalf("malformed post-Stop record error = %v", err)
	}
}

func TestGrokAssistantTurnIgnoresUnrelatedStructuredUpdateAfterStop(t *testing.T) {
	grokHome := filepath.Join(t.TempDir(), ".grok")
	t.Setenv("GROK_HOME", grokHome)
	sessionDir := filepath.Join(grokHome, "sessions", "workspace", "session-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
	updates := strings.Join([]string{
		`{"method":"session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"tool_call_update","content":[{"type":"content","content":{"type":"text","text":"provider detail"}}]}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"answer"}}}}`,
		`{"method":"session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1"}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}
	body, model, complete, err := readGrokAssistantTurn(transcriptPath, "prompt-1", "session-1")
	if err != nil || !complete || body != "answer" || model != "" {
		t.Fatalf("Grok response with unrelated structured update = %t / %q / %q / %v", complete, body, model, err)
	}
}

func TestGrokAssistantTurnLeavesStructuredUpdateWithoutTerminalFenceIncomplete(t *testing.T) {
	grokHome := filepath.Join(t.TempDir(), ".grok")
	t.Setenv("GROK_HOME", grokHome)
	sessionDir := filepath.Join(grokHome, "sessions", "workspace", "session-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
	updates := strings.Join([]string{
		`{"method":"session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"tool_call_update","content":[{"type":"content","content":{"type":"text","text":"provider detail"}}]}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}
	body, model, complete, err := readGrokAssistantTurn(transcriptPath, "prompt-1", "session-1")
	if err != nil || complete || body != "" || model != "" {
		t.Fatalf("unfenced Grok structured update = %t / %q / %q / %v", complete, body, model, err)
	}
}

func TestGrokAssistantTurnRejectsUnsupportedExactAssistantChunkAfterStop(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "array", content: `[{"type":"text","text":"unsupported"}]`},
		{name: "empty object", content: `{}`},
		{name: "null", content: `null`},
		{name: "nested text", content: `{"message":{"text":"unsupported"}}`},
		{name: "renamed text", content: `{"message":"unsupported"}`},
		{name: "null text", content: `{"text":null}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			grokHome := filepath.Join(t.TempDir(), ".grok")
			t.Setenv("GROK_HOME", grokHome)
			sessionDir := filepath.Join(grokHome, "sessions", "workspace", "session-1")
			if err := os.MkdirAll(sessionDir, 0o700); err != nil {
				t.Fatal(err)
			}
			transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
			updates := strings.Join([]string{
				`{"method":"session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
				`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":` + tc.content + `}}}`,
				`{"method":"session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1"}}}`,
			}, "\n") + "\n"
			if err := os.WriteFile(transcriptPath, []byte(updates), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readGrokAssistantTurn(transcriptPath, "prompt-1", "session-1"); err == nil ||
				!strings.Contains(err.Error(), "unsupported relevant record") {
				t.Fatalf("unsupported exact Grok assistant chunk error = %v", err)
			}
		})
	}
}

func TestGrokAssistantTurnIgnoresUnsupportedChunkForDifferentPrompt(t *testing.T) {
	grokHome := filepath.Join(t.TempDir(), ".grok")
	t.Setenv("GROK_HOME", grokHome)
	sessionDir := filepath.Join(grokHome, "sessions", "workspace", "session-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
	updates := strings.Join([]string{
		`{"method":"session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-2"},"update":{"sessionUpdate":"agent_message_chunk","content":null}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"answer"}}}}`,
		`{"method":"session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1"}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}
	body, model, complete, err := readGrokAssistantTurn(transcriptPath, "prompt-1", "session-1")
	if err != nil || !complete || body != "answer" || model != "" {
		t.Fatalf("Grok response with unsupported foreign chunk = %t / %q / %q / %v", complete, body, model, err)
	}
}

func TestGrokAssistantTurnRequiresNewTerminalAfterLateExactChunk(t *testing.T) {
	tests := []struct {
		name           string
		secondTerminal bool
		wantComplete   bool
	}{
		{name: "late chunk clears prior terminal"},
		{name: "later terminal completes turn", secondTerminal: true, wantComplete: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			grokHome := filepath.Join(t.TempDir(), ".grok")
			t.Setenv("GROK_HOME", grokHome)
			sessionDir := filepath.Join(grokHome, "sessions", "workspace", "session-1")
			if err := os.MkdirAll(sessionDir, 0o700); err != nil {
				t.Fatal(err)
			}
			updates := []string{
				`{"method":"session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
				`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"first"}}}}`,
				`{"method":"session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1"}}}`,
				`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"late"}}}}`,
			}
			if tc.secondTerminal {
				updates = append(updates, `{"method":"session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1"}}}`)
			}
			transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
			if err := os.WriteFile(transcriptPath, []byte(strings.Join(updates, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			body, model, complete, err := readGrokAssistantTurn(transcriptPath, "prompt-1", "session-1")
			if err != nil || complete != tc.wantComplete || body != "first\n\nlate" || model != "" {
				t.Fatalf("Grok response after late exact chunk = %t / %q / %q / %v", complete, body, model, err)
			}
		})
	}
}

func TestGrokAssistantTurnBoundsOnlyTheSelectedTailTurn(t *testing.T) {
	grokHome := filepath.Join(t.TempDir(), ".grok")
	t.Setenv("GROK_HOME", grokHome)
	sessionDir := filepath.Join(grokHome, "sessions", "workspace", "session-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var updates strings.Builder
	for range maxNativeTranscriptRecords + 100 {
		updates.WriteString("{}\n")
	}
	updates.WriteString(strings.Join([]string{
		`{"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"answer after long history"}}}}`,
		`{"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1"}}}`,
	}, "\n") + "\n")
	transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(updates.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	body, model, complete, err := readGrokAssistantTurn(transcriptPath, "prompt-1", "session-1")
	if err != nil || !complete || body != "answer after long history" || model != "" {
		t.Fatalf("long-session tail turn = %t / %q / %q / %v", complete, body, model, err)
	}
}

func TestGrokTranscriptTailStartPreservesExactRecordBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	raw := []byte("old\nstop\nassistant\nterminal\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	exactWindow := len(raw) - len("old\n")
	start, err := grokTranscriptTailStart(file, int64(len(raw)), exactWindow)
	if err != nil || start != int64(len("old\n")) {
		t.Fatalf("exact-boundary tail start = %d / %v", start, err)
	}
	partialWindow := len(raw) - (len("old\n") - 1)
	start, err = grokTranscriptTailStart(file, int64(len(raw)), partialWindow)
	if err != nil || start != int64(len("old\n")) {
		t.Fatalf("partial-record tail start = %d / %v", start, err)
	}
}

func TestCompleteGrokAssistantTurnRejectsUntrustedPathWithoutPolling(t *testing.T) {
	root := t.TempDir()
	grokHome := filepath.Join(root, ".grok")
	t.Setenv("GROK_HOME", grokHome)
	if err := os.MkdirAll(filepath.Join(grokHome, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(root, "updates.jsonl")
	if err := os.WriteFile(transcriptPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readCompleteGrokAssistantTurn(transcriptPath, "prompt-1", "session-1"); err == nil || !strings.Contains(err.Error(), "outside the session store") {
		t.Fatalf("untrusted Grok transcript error = %v", err)
	}
}

func TestGrokAssistantMessageRejectsUntrustedNativeTranscript(t *testing.T) {
	for _, tc := range []struct {
		name          string
		promptID      string
		sessionID     string
		mutate        func(string, string) error
		wantErrSubstr string
	}{
		{name: "session mismatch", promptID: "prompt-1", sessionID: "session-other", wantErrSubstr: "outside the session store"},
		{name: "empty prompt id", sessionID: "session-1", wantErrSubstr: "requires prompt and session ids"},
		{
			name: "symlink file", promptID: "prompt-1", sessionID: "session-1", wantErrSubstr: "must not be a symlink",
			mutate: func(_ string, path string) error {
				target := path + ".target"
				if err := os.Rename(path, target); err != nil {
					return err
				}
				return os.Symlink(target, path)
			},
		},
		{
			name: "writable file", promptID: "prompt-1", sessionID: "session-1", wantErrSubstr: "not a trusted regular file",
			mutate: func(_ string, path string) error { return os.Chmod(path, 0o666) },
		},
		{
			name: "writable directory", promptID: "prompt-1", sessionID: "session-1", wantErrSubstr: "not a trusted regular file",
			mutate: func(dir, _ string) error { return os.Chmod(dir, 0o777) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grokHome := filepath.Join(t.TempDir(), ".grok")
			t.Setenv("GROK_HOME", grokHome)
			sessionDir := filepath.Join(grokHome, "sessions", "workspace", "session-1")
			if err := os.MkdirAll(sessionDir, 0o755); err != nil {
				t.Fatal(err)
			}
			transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
			line := `{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"answer"}}}}` + "\n"
			if err := os.WriteFile(transcriptPath, []byte(line), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				if err := tc.mutate(sessionDir, transcriptPath); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, _, err := readGrokAssistantTurn(transcriptPath, tc.promptID, tc.sessionID); err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Fatalf("untrusted Grok transcript error = %v", err)
			}
		})
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

func TestFlushLockReclaimsDeadOwnerAndPreservesLiveOwner(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pid          int
		wantAcquired bool
	}{
		{name: "dead owner", pid: 2147483647, wantAcquired: true},
		{name: "live owner", pid: os.Getpid(), wantAcquired: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
			dir, err := outboxDir(RuntimeCursor)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, ".flush.lock")
			if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", tc.pid)), 0o600); err != nil {
				t.Fatal(err)
			}
			release, acquired, err := AcquireFlushLock(RuntimeCursor)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if acquired != tc.wantAcquired {
				t.Fatalf("acquired = %v, want %v", acquired, tc.wantAcquired)
			}
		})
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
