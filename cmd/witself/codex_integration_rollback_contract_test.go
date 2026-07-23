package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestProviderIntegrationContractCodexRollbackRestoresPriorInstall(t *testing.T) {
	home := t.TempDir()
	setInstallExecutableForTest(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", codexHome)
	provider := buildFakeProviderCLI(t, home)
	t.Setenv("CODEX_CLI_PATH", provider.Path)
	siblingMCP := []string{"sibling-mcp", "serve", "--preserve"}
	provider.writeRegistry(t, map[string][]string{"sibling": siblingMCP})

	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(codexHome, "hooks.json"),
		[]byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"sibling-hook"}]}]}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(codexHome, "AGENTS.md"),
		[]byte("# Sibling instructions\n\nPreserve this text.\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	const tokenValue = "synthetic-rollback-token"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer "+tokenValue {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"schema_version":"witself.v0","identity":{"account_id":"acc_rollback","realm_id":"realm_rollback","realm_name":"default","agent_id":"agent_rollback","agent_name":"codex-test-bot"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`)
	}))
	t.Cleanup(backend.Close)
	tokenPath := filepath.Join(home, "agent.token")
	if err := os.WriteFile(tokenPath, []byte(tokenValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	installArgs := []string{
		"codex", "--account", "default", "--realm", "default", "--agent", "codex-test-bot",
		"--location", "acceptance", "--capture", "raw", "--endpoint", backend.URL,
		"--token-file", tokenPath, "--user-hooks",
	}
	if code := installCmd(installArgs); code != 0 {
		t.Fatalf("initial install code = %d", code)
	}

	hooksPath := filepath.Join(codexHome, "hooks.json")
	instructionsPath := filepath.Join(codexHome, "AGENTS.md")
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	read := func(path string) []byte {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	priorHooks := read(hooksPath)
	priorInstructions := read(instructionsPath)
	priorConfig := read(configPath)
	priorState := provider.readRegistry(t)
	priorMCP := append([]string(nil), priorState.Servers["witself"]...)
	if len(priorMCP) == 0 || !slices.Equal(priorState.Servers["sibling"], siblingMCP) {
		t.Fatalf("initial provider state = %#v", priorState.Servers)
	}
	callFence := len(provider.invocations(t))
	failureSentinel := provider.failNextMCPAdd(t)

	failedInstallArgs := append([]string(nil), installArgs...)
	for index := range failedInstallArgs {
		if failedInstallArgs[index] == "acceptance" && index > 0 && failedInstallArgs[index-1] == "--location" {
			failedInstallArgs[index] = "acceptance-upgraded"
		}
	}
	if code := installCmd(failedInstallArgs); code != 1 {
		t.Fatalf("failed reinstall code = %d, want 1", code)
	}
	if _, err := os.Stat(failureSentinel); !os.IsNotExist(err) {
		t.Fatalf("one-shot MCP failure was not consumed: %v", err)
	}
	for name, snapshot := range map[string]struct {
		path string
		raw  []byte
	}{
		"hooks":        {path: hooksPath, raw: priorHooks},
		"instructions": {path: instructionsPath, raw: priorInstructions},
		"config":       {path: configPath, raw: priorConfig},
	} {
		if current := read(snapshot.path); !slices.Equal(current, snapshot.raw) {
			t.Fatalf("rollback changed %s:\n%s\n---\n%s", name, snapshot.raw, current)
		}
	}
	restored := provider.readRegistry(t)
	if len(restored.Servers) != len(priorState.Servers) ||
		!slices.Equal(restored.Servers["witself"], priorMCP) ||
		!slices.Equal(restored.Servers["sibling"], siblingMCP) {
		t.Fatalf("rollback provider state = %#v, want %#v", restored.Servers, priorState.Servers)
	}

	var adds, removes [][]string
	calls := provider.invocations(t)
	for _, call := range calls[callFence:] {
		switch {
		case len(call.Args) >= 4 && call.Args[0] == "mcp" && call.Args[1] == "add" && call.Args[2] == "witself":
			adds = append(adds, call.Args)
		case len(call.Args) == 3 && call.Args[0] == "mcp" && call.Args[1] == "remove":
			removes = append(removes, call.Args)
		}
	}
	stored, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	failedMCP := append([]string(nil), priorMCP...)
	for index := range failedMCP {
		if failedMCP[index] == "acceptance" && index > 0 && failedMCP[index-1] == "--location" {
			failedMCP[index] = "acceptance-upgraded"
		}
	}
	wantAdd := append([]string{"mcp", "add", "witself", "--env", "WITSELF_HOME=" + stored.MCPEnvironment["WITSELF_HOME"], "--"}, failedMCP...)
	if len(adds) != 1 || !slices.Equal(adds[0], wantAdd) {
		t.Fatalf("failed add = %#v, want one exact %#v", adds, wantAdd)
	}
	wantRemove := []string{"mcp", "remove", "witself"}
	if len(removes) != 1 || !slices.Equal(removes[0], wantRemove) {
		t.Fatalf("reinstall remove = %#v, want one exact %#v", removes, wantRemove)
	}
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("rollback removed token file: %v", err)
	}
}
