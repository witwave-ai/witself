package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestInstallCodexRegistersMCPAndHooksWithoutEmbeddingToken(t *testing.T) {
	home := t.TempDir()
	witselfHome := filepath.Join(home, ".witself")
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", witselfHome)
	t.Setenv("CODEX_HOME", codexHome)
	logPath := filepath.Join(home, "codex-args.log")
	t.Setenv("FAKE_CLI_LOG", logPath)
	fakeCLI := filepath.Join(home, "codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_CLI_LOG\"\nexit 0\n"
	if err := os.WriteFile(fakeCLI, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_CLI_PATH", fakeCLI)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer agent-token" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","realm_id":"realm_1","realm_name":"default","agent_id":"agent_1","agent_name":"scott"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`))
	}))
	defer srv.Close()
	tokenPath := filepath.Join(home, "agent.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := installCmd([]string{
		"codex", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--capture", "raw", "--endpoint", srv.URL,
		"--token-file", tokenPath,
	}); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentID != "agent_1" || cfg.AgentName != "scott" || cfg.Location.Name != "home" {
		t.Fatalf("config = %#v", cfg)
	}
	hooks, err := os.ReadFile(filepath.Join(codexHome, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	cliLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	combined := string(hooks) + string(cliLog)
	if strings.Contains(combined, "agent-token") {
		t.Fatal("agent token was embedded in runtime configuration")
	}
	if !strings.Contains(string(hooks), "transcript hook --runtime codex") ||
		!strings.Contains(string(cliLog), "mcp add witself --") ||
		!strings.Contains(string(cliLog), "mcp serve --runtime codex") {
		t.Fatalf("hooks/log = %s\n---\n%s", hooks, cliLog)
	}
}

func TestRegisterMCPClaudeUsesUserScopedStdio(t *testing.T) {
	home := t.TempDir()
	logPath := filepath.Join(home, "claude-args.log")
	t.Setenv("FAKE_CLI_LOG", logPath)
	fakeCLI := filepath.Join(home, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_CLI_LOG\"\nexit 0\n"
	if err := os.WriteFile(fakeCLI, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := registerMCP(transcriptcapture.RuntimeClaudeCode, fakeCLI, "/usr/local/bin/witself"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	for _, want := range []string{
		"mcp remove --scope user witself",
		"mcp add --scope user --transport stdio witself -- /usr/local/bin/witself mcp serve --runtime claude-code",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("Claude registration log %q does not contain %q", log, want)
		}
	}
}

func TestCaptureOutboxFlushesWholeVisibleTurn(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	var mu sync.Mutex
	var appended []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			var body struct {
				Entries []map[string]any `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode batch: %v", err)
				http.Error(w, "bad batch", http.StatusBadRequest)
				return
			}
			mu.Lock()
			appended = append(appended, body.Entries...)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loc, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeClaudeCode, CaptureMode: transcriptcapture.ModeRaw,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: loc,
		Endpoint: srv.URL, TokenFile: tokenPath,
	}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"session_id":"session-1","hook_event_name":"SessionStart","cwd":"/src/witself"}`,
		`{"session_id":"session-1","hook_event_name":"UserPromptSubmit","prompt":"the full original prompt"}`,
		`{"session_id":"session-1","hook_event_name":"Stop","last_assistant_message":"the full final response"}`,
	} {
		if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if code := transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode}); code != 0 {
		t.Fatalf("flush code = %d", code)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %d", len(pending))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(appended) != 3 {
		t.Fatalf("appended = %#v", appended)
	}
	if appended[1]["body"] != "the full original prompt" || appended[2]["body"] != "the full final response" {
		t.Fatalf("visible turn = %#v", appended)
	}
	if appended[2]["reply_to_external_id"] != appended[1]["external_id"] {
		t.Fatalf("reply link = %#v", appended[2])
	}
}

func TestCaptureFlushDrainsEventsQueuedWhileRunning(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	started := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	appended := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			once.Do(func() {
				close(started)
				<-resume
			})
			mu.Lock()
			appended++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loc, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCodex, CaptureMode: transcriptcapture.ModeMessages,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: loc,
		Endpoint: srv.URL, TokenFile: tokenPath,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, []byte(`{"session_id":"session-1","hook_event_name":"SessionStart"}`)); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeCodex}) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("flush did not start")
	}
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, []byte(`{"session_id":"session-1","hook_event_name":"UserPromptSubmit","prompt":"queued during flush"}`)); err != nil {
		t.Fatal(err)
	}
	close(resume)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("flush code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("flush did not finish")
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pending) != 0 || appended != 2 {
		t.Fatalf("pending/appended = %d/%d", len(pending), appended)
	}
}
