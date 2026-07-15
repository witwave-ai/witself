package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/messagerunner"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const messageRunnerTestToken = "message-runner-test-token"

func TestMessageRunnerEnableStatusDisableWithoutService(t *testing.T) {
	identity := client.SelfIdentity{
		AccountID: "acc_runner", RealmID: "realm_runner", RealmName: "default",
		AgentID: "agent_bob", AgentName: "Bob",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer "+messageRunnerTestToken {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(client.SelfDigest{Identity: identity})
	}))
	defer server.Close()
	binding, providerPath := setupMessageRunnerCLI(t, server.URL, identity)

	stdout, stderr, code := runMessageRunnerCLI(t, func() int {
		return messageRunnerCmd([]string{
			"enable", "--runtime", binding.Runtime, "--provider", "claude-code",
			"--provider-path", providerPath, "--max-turns", "9", "--no-service", "--json",
		})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("enable = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var enabled messageRunnerStatus
	if err := json.Unmarshal([]byte(stdout), &enabled); err != nil {
		t.Fatal(err)
	}
	if !enabled.Configured || !enabled.Config.Enabled || enabled.Config.AgentID != identity.AgentID ||
		enabled.Config.MaximumTurns != 9 || enabled.Config.Provider != "claude-code" || enabled.Service.Installed {
		t.Fatalf("enabled status = %#v", enabled)
	}
	store, err := messagerunner.DefaultConfigStore(binding.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordNotification(context.Background(), client.Message{
		ID: "msg_terminal", ThreadID: "thr_terminal", Kind: "result",
		From:      client.MessageAgent{AgentID: "agent_scott", AgentName: "Scott"},
		CreatedAt: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code = runMessageRunnerCLI(t, func() int {
		return messageRunnerCmd([]string{"status", "--runtime", binding.Runtime, "--json"})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("status = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var inspected messageRunnerStatus
	if err := json.Unmarshal([]byte(stdout), &inspected); err != nil {
		t.Fatal(err)
	}
	if !inspected.Configured || !inspected.Config.Enabled || inspected.Service.Installed || inspected.NotificationCount != 1 {
		t.Fatalf("inspected status = %#v", inspected)
	}

	stdout, stderr, code = runMessageRunnerCLI(t, func() int {
		return messageRunnerCmd([]string{"notifications", "--runtime", binding.Runtime, "--json"})
	})
	if code != 0 || stderr != "" || !strings.Contains(stdout, "msg_terminal") || strings.Contains(stdout, "private body") {
		t.Fatalf("notifications = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runMessageRunnerCLI(t, func() int {
		return messageRunnerCmd([]string{"notifications", "--runtime", binding.Runtime, "--clear", "msg_terminal", "--json"})
	})
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"cleared":1`) {
		t.Fatalf("clear notifications = %d stdout=%q stderr=%q", code, stdout, stderr)
	}

	releaseRunner, acquired, err := store.Acquire()
	if err != nil || !acquired {
		t.Fatalf("acquire foreground runner lock = %t / %v", acquired, err)
	}
	stdout, stderr, code = runMessageRunnerCLI(t, func() int {
		return messageRunnerCmd([]string{"disable", "--runtime", binding.Runtime, "--json"})
	})
	if code != 1 || stdout != "" || !strings.Contains(stderr, "still active") {
		t.Fatalf("active disable = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stillEnabled, err := store.Load()
	if err != nil || !stillEnabled.Enabled {
		t.Fatalf("active disable changed config = %#v / %v", stillEnabled, err)
	}
	if err := releaseRunner(); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code = runMessageRunnerCLI(t, func() int {
		return messageRunnerCmd([]string{"disable", "--runtime", binding.Runtime, "--json"})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("disable = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var disabled messageRunnerStatus
	if err := json.Unmarshal([]byte(stdout), &disabled); err != nil {
		t.Fatal(err)
	}
	if !disabled.Configured || disabled.Config.Enabled {
		t.Fatalf("disabled status = %#v", disabled)
	}
}

func TestMessageRunnerRunOnceCompletesFencedHTTPFlow(t *testing.T) {
	identity := client.SelfIdentity{
		AccountID: "acc_runner", RealmID: "realm_runner", RealmName: "default",
		AgentID: "agent_bob", AgentName: "Bob",
	}
	message := client.Message{
		ID: "msg_1", AccountID: identity.AccountID, RealmID: identity.RealmID,
		From:    client.MessageAgent{Kind: "agent", AgentID: "agent_scott", AgentName: "Scott"},
		To:      client.MessageRecipient{Kind: "agent", AgentID: identity.AgentID, AgentName: identity.AgentName},
		Subject: "Delegation", Kind: "request", Body: "Prepare the report.", ThreadID: "thr_1",
		CausalDepth: 1,
		CreatedAt:   time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
		Delivery:    client.MessageDelivery{State: "delivered"},
		ReadState:   client.MessageReadState{State: "unread"},
		Processing:  client.MessageProcessing{State: "available"},
	}
	metadata := message
	metadata.Body = ""
	message.Processing = client.MessageProcessing{State: "claimed", Generation: 1}
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+messageRunnerTestToken {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/self":
			_ = json.NewEncoder(w).Encode(client.SelfDigest{Identity: identity})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/message-requests":
			_ = json.NewEncoder(w).Encode(client.MessageRequestPage{Requests: []client.MessageRequest{}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages:listen":
			_ = json.NewEncoder(w).Encode(client.MessageListenResult{Messages: []client.Message{metadata}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/msg_1:claim":
			if !strings.Contains(r.Header.Get("Idempotency-Key"), "message-runner/claim/") {
				t.Errorf("claim idempotency key = %q", r.Header.Get("Idempotency-Key"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"processing": client.MessageProcessing{
				State: "claimed", ClaimID: "mcl_1", Generation: 1,
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/msg_1:read":
			_ = json.NewEncoder(w).Encode(map[string]any{"message": message})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/msg_1:complete":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode completion: %v", err)
			}
			if request["claim_id"] != "mcl_1" || request["generation"] != float64(1) ||
				request["kind"] != "result" || request["body"] != "native message result" || request["to"] != nil {
				t.Errorf("completion request = %#v", request)
			}
			payload, _ := request["payload"].(map[string]any)
			if payload["_witself_runner"] == nil {
				t.Errorf("completion payload lacks continuation context: %#v", payload)
			}
			processing := client.MessageProcessing{
				State: "completed", ClaimID: "mcl_1", Generation: 1, ResultMessageID: "msg_2",
			}
			_ = json.NewEncoder(w).Encode(client.CompleteMessageResult{
				Processing: processing,
				Message:    client.Message{ID: "msg_2", AccountID: identity.AccountID, RealmID: identity.RealmID},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/msg_1:ack":
			_ = json.NewEncoder(w).Encode(map[string]any{"message": metadata})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	binding, providerPath := setupMessageRunnerCLI(t, server.URL, identity)
	store, err := messagerunner.DefaultConfigStore(binding.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enable(messagerunner.Settings{
		Runtime: binding.Runtime, AccountID: binding.AccountID, RealmID: binding.RealmID,
		AgentID: binding.AgentID, AgentName: binding.AgentName,
		Provider: "claude-code", ProviderPath: providerPath, MaximumTurns: 12,
	}); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runMessageRunnerCLI(t, func() int {
		return messageRunnerRunContext(context.Background(), []string{
			"--runtime", binding.Runtime, "--once", "--json",
		}, false)
	})
	if code != 0 || stderr != "" {
		t.Fatalf("run = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var result messagerunner.RunResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != messagerunner.RunStatusCompleted || result.MessageID != "msg_1" || result.ResultMessageID != "msg_2" {
		t.Fatalf("run result = %#v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"GET /v1/self", "GET /v1/message-requests", "GET /v1/message-requests", "GET /v1/message-requests",
		"POST /v1/messages:listen", "POST /v1/messages/msg_1:claim",
		"POST /v1/messages/msg_1:read", "POST /v1/messages/msg_1:complete", "POST /v1/messages/msg_1:ack",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestMessageRunnerEnableFailsClosedForCodexNativeMode(t *testing.T) {
	identity := client.SelfIdentity{
		AccountID: "acc_runner", RealmID: "realm_runner", AgentID: "agent_bob", AgentName: "Bob",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.SelfDigest{Identity: identity})
	}))
	defer server.Close()
	binding, _ := setupMessageRunnerCLI(t, server.URL, identity)
	providerPath := writeFakeNativeProvider(t, t.TempDir(), `
if [ "$#" -eq 1 ] && [ "$1" = "--version" ]; then printf '%s\n' 'codex-cli 99.0.0'; exit 0; fi
if [ "$#" -eq 2 ] && [ "$1" = "exec" ] && [ "$2" = "--help" ]; then printf '%s\n' '--sandbox read-only'; exit 0; fi
exit 1
`)
	stdout, stderr, code := runMessageRunnerCLI(t, func() int {
		return messageRunnerCmd([]string{
			"enable", "--runtime", binding.Runtime, "--provider", "codex",
			"--provider-path", providerPath, "--no-service",
		})
	})
	if code != 1 || stdout != "" || !strings.Contains(stderr, "no-tools/no-shell") {
		t.Fatalf("enable = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func setupMessageRunnerCLI(t *testing.T, endpoint string, identity client.SelfIdentity) (transcriptcapture.Config, string) {
	t.Helper()
	// Service definitions are user-home scoped rather than WITSELF_HOME scoped.
	// Isolate both roots so tests never inspect or remove the developer's real
	// launchd/systemd service for the same runtime label.
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(userHome, ".config"))
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	tokenPath := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenPath, []byte(messageRunnerTestToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	location, err := transcriptcapture.EnsureLocation("message-runner-test")
	if err != nil {
		t.Fatal(err)
	}
	binding := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeClaudeCode, CaptureMode: transcriptcapture.ModeRaw,
		Account: "default", AccountID: identity.AccountID, Realm: "default", RealmID: identity.RealmID,
		Agent: "bob", AgentID: identity.AgentID, AgentName: identity.AgentName,
		Endpoint: endpoint, TokenFile: tokenPath, Location: location,
	}
	if err := transcriptcapture.SaveConfig(binding); err != nil {
		t.Fatal(err)
	}
	providerPath := writeFakeNativeProvider(t, t.TempDir(), `
if [ "$#" -eq 1 ] && [ "$1" = "--version" ]; then
  printf '%s\n' '99.0.0 (Claude Code)'
  exit 0
fi
if [ "$#" -eq 1 ] && [ "$1" = "--help" ]; then
  printf '%s\n' '--print --input-format --output-format --safe-mode --no-session-persistence --disable-slash-commands --strict-mcp-config --mcp-config --tools --permission-mode --no-chrome --model'
  exit 0
fi
cat >/dev/null
printf '%s' '{"schema":"witself.message-turn-result.v1","outcome":"result","body":"native message result"}'
`)
	return binding, providerPath
}

func runMessageRunnerCLI(t *testing.T, call func() int) (stdout, stderr string, code int) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code = call()
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()
	outRaw, outReadErr := io.ReadAll(outR)
	errRaw, errReadErr := io.ReadAll(errR)
	_ = outR.Close()
	_ = errR.Close()
	if outReadErr != nil || errReadErr != nil {
		t.Fatalf("capture output: %v, %v", outReadErr, errReadErr)
	}
	return string(outRaw), string(errRaw), code
}
