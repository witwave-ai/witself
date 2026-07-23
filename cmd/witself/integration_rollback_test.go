package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type rollbackIntegrationFixture struct {
	home          string
	codexHome     string
	witselfHome   string
	serverURL     string
	tokenPath     string
	runtimeCLI    string
	mcpStatePath  string
	mcpLogPath    string
	removeFailLog string
	genericLog    string
	genericState  string
}

func setupRollbackIntegrationFixture(t *testing.T, authenticatedAgent string) rollbackIntegrationFixture {
	t.Helper()

	home := t.TempDir()
	fixture := rollbackIntegrationFixture{
		home:          home,
		codexHome:     filepath.Join(home, ".codex"),
		witselfHome:   filepath.Join(home, ".witself"),
		tokenPath:     filepath.Join(home, "agent.token"),
		runtimeCLI:    filepath.Join(home, "codex"),
		mcpStatePath:  filepath.Join(home, "mcp-state"),
		mcpLogPath:    filepath.Join(home, "mcp-args.log"),
		removeFailLog: filepath.Join(home, "remove-failed-once"),
		genericLog:    filepath.Join(home, "provider-invocations.jsonl"),
		genericState:  filepath.Join(home, "provider-effective-state.json"),
	}
	t.Setenv("HOME", fixture.home)
	t.Setenv("WITSELF_HOME", fixture.witselfHome)
	t.Setenv("CODEX_HOME", fixture.codexHome)
	t.Setenv("CODEX_CLI_PATH", fixture.runtimeCLI)
	t.Setenv(managedHooksTestRootEnv, filepath.Join(home, "managed"))
	t.Setenv("FAKE_MCP_STATE", fixture.mcpStatePath)
	t.Setenv("FAKE_MCP_LOG", fixture.mcpLogPath)
	t.Setenv("FAKE_CLI_LOG", fixture.mcpLogPath)
	t.Setenv("FAKE_REMOVE_FAILED", fixture.removeFailLog)
	t.Setenv("FAKE_FAIL_ADD_AGENT", "")
	t.Setenv("FAKE_FAIL_REMOVE_ONCE", "")
	t.Setenv(fakeGenericRuntimeEnv, transcriptcapture.RuntimeCodex)
	t.Setenv(fakeGenericLogEnv, fixture.genericLog)
	t.Setenv(fakeGenericStateEnv, fixture.genericState)
	t.Setenv(fakeGenericFailAddEnv, "")
	t.Setenv(fakeGenericFailRemoveEnv, "")
	t.Setenv(fakeGenericLargeErrorEnv, "")
	t.Setenv(fakeGenericLargeOutputEnv, "")
	t.Setenv("WITSELF_FAKE_GENERIC_EMPTY_MCP_LIST", "")
	setInstallExecutableForTest(t)
	copyGenericProviderFixture(t, fixture.runtimeCLI)
	if err := os.WriteFile(fixture.tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer agent-token" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"schema_version":"witself.v0","identity":{"account_id":"acc_1","realm_id":"realm_1","realm_name":"default","agent_id":"agent_authenticated","agent_name":%q},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`, authenticatedAgent)
	}))
	t.Cleanup(srv.Close)
	fixture.serverURL = srv.URL
	return fixture
}

func saveRollbackBinding(t *testing.T, fixture rollbackIntegrationFixture, agent string) transcriptcapture.Config {
	t.Helper()
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	cfg := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCodex, RuntimeVersion: "0.9.0",
		CaptureMode: transcriptcapture.ModeRaw, HookMode: transcriptcapture.HookModeUser,
		Account: "default", AccountID: "acc_1", Realm: "default", RealmID: "realm_1",
		Agent: agent, AgentID: "agent_previous", AgentName: agent,
		Endpoint: fixture.serverURL, TokenFile: fixture.tokenPath, Location: location,
	}
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	stored, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func saveCursorRollbackBinding(t *testing.T, fixture rollbackIntegrationFixture, agent string) transcriptcapture.Config {
	t.Helper()
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	cfg := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCursor, RuntimeVersion: "3.11.13",
		CaptureMode: transcriptcapture.ModeRaw, HookMode: transcriptcapture.HookModeUser,
		Account: "default", AccountID: "acc_1", Realm: "default", RealmID: "realm_1",
		Agent: agent, AgentID: "agent_previous", AgentName: agent,
		Endpoint: fixture.serverURL, TokenFile: fixture.tokenPath, Location: location,
	}
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	stored, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func setupCursorRollbackCLI(t *testing.T, fixture rollbackIntegrationFixture, cursorHome string) string {
	t.Helper()
	enabledPath := filepath.Join(fixture.home, "cursor-mcp-enabled")
	t.Setenv("CURSOR_CONFIG_DIR", "")
	t.Setenv("CURSOR_CLI_PATH", fixture.runtimeCLI)
	t.Setenv("FAKE_CURSOR_MCP_PATH", filepath.Join(cursorHome, "mcp.json"))
	t.Setenv("FAKE_CURSOR_ENABLED", enabledPath)
	t.Setenv("FAKE_CURSOR_FAIL_ENABLE_AGENT", "")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf '%s\n' 'Cursor 3.11.13'
  exit 0
fi
if [ "$1 $2" = "mcp --help" ]; then
  printf '%s\n' 'Manage MCP servers'
  exit 0
fi
printf '%s\n' "$*" >> "$FAKE_MCP_LOG"
if [ "$1 $2 $3" = "mcp enable witself" ]; then
  if [ -n "${FAKE_CURSOR_FAIL_ENABLE_AGENT:-}" ] && grep -Fq "$FAKE_CURSOR_FAIL_ENABLE_AGENT" "$FAKE_CURSOR_MCP_PATH"; then
    printf '%s\n' 'forced Cursor MCP enable failure' >&2
    exit 9
  fi
  : > "$FAKE_CURSOR_ENABLED"
  exit 0
fi
if [ "$1 $2 $3" = "mcp disable witself" ]; then
  rm -f "$FAKE_CURSOR_ENABLED"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(fixture.runtimeCLI, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return enabledPath
}

func TestCursorInstallAndUninstallManageRequiredMCPPermission(t *testing.T) {
	fixture := setupRollbackIntegrationFixture(t, "cursor-test-bot")
	cursorHome := filepath.Join(fixture.home, ".cursor")
	setupCursorRollbackCLI(t, fixture, cursorHome)
	if err := os.MkdirAll(cursorHome, 0o700); err != nil {
		t.Fatal(err)
	}
	cliConfigPath := filepath.Join(cursorHome, "cli-config.json")
	if err := os.WriteFile(cliConfigPath, []byte(`{"permissions":{"allow":["Shell(ls)"],"deny":[]},"other":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"cursor", "--account", "default", "--realm", "default", "--agent", "cursor-test-bot",
		"--location", "home", "--capture", "raw", "--endpoint", fixture.serverURL,
		"--token-file", fixture.tokenPath,
	}
	if code := installCmd(args); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	installed := readTestJSONObject(t, cliConfigPath)
	allow := installed["permissions"].(map[string]any)["allow"].([]any)
	if len(allow) != 2 || allow[0] != "Shell(ls)" || allow[1] != cursorWitselfMCPPermission {
		t.Fatalf("installed Cursor allow list = %#v", allow)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	if !cursorConfigManagesWitselfMCPPermission(cfg.ManagedPermissions) {
		t.Fatalf("managed permissions = %#v", cfg.ManagedPermissions)
	}
	// A later user-owned duplicate is indistinguishable by value, so uninstall
	// must decrement the count by exactly the one entry Witself owns.
	permissions := installed["permissions"].(map[string]any)
	permissions["allow"] = append(allow, cursorWitselfMCPPermission)
	if err := writeJSONObjectAtomic(cliConfigPath, installed); err != nil {
		t.Fatal(err)
	}

	if code := uninstallCmd([]string{"cursor"}); code != 0 {
		t.Fatalf("uninstall code = %d", code)
	}
	uninstalled := readTestJSONObject(t, cliConfigPath)
	allow = uninstalled["permissions"].(map[string]any)["allow"].([]any)
	if len(allow) != 2 || allow[0] != "Shell(ls)" || allow[1] != cursorWitselfMCPPermission || uninstalled["other"] != true {
		t.Fatalf("uninstalled Cursor config = %#v", uninstalled)
	}
}

func TestCursorInstallPreservesPreexistingUserOwnedMCPPermission(t *testing.T) {
	fixture := setupRollbackIntegrationFixture(t, "cursor-test-bot")
	cursorHome := filepath.Join(fixture.home, ".cursor")
	setupCursorRollbackCLI(t, fixture, cursorHome)
	if err := os.MkdirAll(cursorHome, 0o700); err != nil {
		t.Fatal(err)
	}
	cliConfigPath := filepath.Join(cursorHome, "cli-config.json")
	original := []byte(`{"permissions":{"allow":["Shell(ls)","Mcp(witself:*)"]},"other":true}`)
	if err := os.WriteFile(cliConfigPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	if code := installCmd([]string{
		"cursor", "--account", "default", "--realm", "default", "--agent", "cursor-test-bot",
		"--location", "home", "--capture", "raw", "--endpoint", fixture.serverURL,
		"--token-file", fixture.tokenPath,
	}); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	if cursorConfigManagesWitselfMCPPermission(cfg.ManagedPermissions) {
		t.Fatalf("pre-existing permission became installer-owned: %#v", cfg.ManagedPermissions)
	}

	if code := uninstallCmd([]string{"cursor"}); code != 0 {
		t.Fatalf("uninstall code = %d", code)
	}
	got := readTestJSONObject(t, cliConfigPath)
	allow := got["permissions"].(map[string]any)["allow"].([]any)
	if len(allow) != 2 || allow[0] != "Shell(ls)" || allow[1] != cursorWitselfMCPPermission || got["other"] != true {
		t.Fatalf("user-owned Cursor permission changed on uninstall: %#v", got)
	}
}

func rollbackTestExecutable(t *testing.T) string {
	t.Helper()
	executable, err := currentExecutablePath()
	if err != nil {
		t.Fatal(err)
	}
	return executable
}

func TestFailedReinstallRestoresPriorMCPBinding(t *testing.T) {
	fixture := setupRollbackIntegrationFixture(t, "new-agent")
	previous := saveRollbackBinding(t, fixture, "prior-agent")
	executable := rollbackTestExecutable(t)
	if err := registerMCP(
		transcriptcapture.RuntimeCodex,
		fixture.runtimeCLI,
		executable,
		previous.Account,
		previous.Realm,
		previous.Agent,
		previous.Location.Name,
	); err != nil {
		t.Fatal(err)
	}
	providerConfigPath := filepath.Join(fixture.codexHome, "config.toml")
	wantProviderConfig, err := os.ReadFile(providerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.mcpLogPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_FAIL_ADD_AGENT", "new-agent")

	if code := installCmd([]string{
		"codex", "--account", "default", "--realm", "default", "--agent", "new-agent",
		"--location", "home", "--capture", "raw", "--endpoint", fixture.serverURL,
		"--token-file", fixture.tokenPath, "--user-hooks",
	}); code != 1 {
		t.Fatalf("install code = %d, want 1", code)
	}

	gotProviderConfig, err := os.ReadFile(providerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotProviderConfig, wantProviderConfig) {
		t.Fatalf("restored Codex provider config = %q, want exact bytes %q", gotProviderConfig, wantProviderConfig)
	}
	if strings.Contains(string(gotProviderConfig), "new-agent") || !strings.Contains(string(gotProviderConfig), "prior-agent") {
		t.Fatalf("failed reinstall retained the wrong MCP principal: %q", gotProviderConfig)
	}
	verifiedPrevious, err := hydrateLegacyGenericProviderConfig(previous, fixture.runtimeCLI, executable)
	if err != nil {
		t.Fatal(err)
	}
	currentBinding, exists, _, err := inspectGenericMCP(verifiedPrevious)
	if err != nil {
		t.Fatal(err)
	}
	wantBinding, err := genericMCPBindingFromConfig(verifiedPrevious)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || !equalGenericMCPBinding(currentBinding, wantBinding) {
		t.Fatalf("restored Codex binding does not match the prior exact provider contract: %#v", currentBinding)
	}
	restoredConfig, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if restoredConfig.Agent != previous.Agent || restoredConfig.AgentID != previous.AgentID ||
		restoredConfig.Location.Name != previous.Location.Name {
		t.Fatalf("restored config = %#v, want prior binding %#v", restoredConfig, previous)
	}
	if _, err := os.Stat(filepath.Join(fixture.codexHome, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("failed reinstall retained newly installed routing file: %v", err)
	}
	log, err := os.ReadFile(fixture.mcpLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "--agent new-agent") ||
		!strings.Contains(string(log), "mcp remove witself") ||
		!strings.Contains(string(wantProviderConfig), "prior-agent") {
		t.Fatalf("MCP rollback was not exercised: %s", log)
	}
}

func TestGrokFailedReinstallRestoresUserScopedMCPBindingAndAgentsFile(t *testing.T) {
	fixture := setupRollbackIntegrationFixture(t, "new-agent")
	grokHome := filepath.Join(fixture.home, ".grok")
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("GROK_CLI_PATH", fixture.runtimeCLI)
	t.Setenv(fakeGenericRuntimeEnv, transcriptcapture.RuntimeGrokBuild)

	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	previous := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeGrokBuild, RuntimeVersion: "0.2.98",
		CaptureMode: transcriptcapture.ModeRaw, HookMode: transcriptcapture.HookModeUser,
		Account: "default", AccountID: "acc_1", Realm: "default", RealmID: "realm_1",
		Agent: "prior-agent", AgentID: "agent_previous", AgentName: "prior-agent",
		Endpoint: fixture.serverURL, TokenFile: fixture.tokenPath, Location: location,
	}
	if err := transcriptcapture.SaveConfig(previous); err != nil {
		t.Fatal(err)
	}
	previous, err = transcriptcapture.LoadConfig(transcriptcapture.RuntimeGrokBuild)
	if err != nil {
		t.Fatal(err)
	}
	executable := rollbackTestExecutable(t)
	if err := registerMCP(
		transcriptcapture.RuntimeGrokBuild,
		fixture.runtimeCLI,
		executable,
		previous.Account,
		previous.Realm,
		previous.Agent,
		previous.Location.Name,
	); err != nil {
		t.Fatal(err)
	}
	providerConfigPath := filepath.Join(grokHome, "config.toml")
	wantProviderConfig, err := os.ReadFile(providerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	wantEffectiveState, err := os.ReadFile(fixture.genericState)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wantProviderConfig), "prior-agent") ||
		!strings.Contains(string(wantProviderConfig), "grok-build") {
		t.Fatalf("seeded Grok binding does not use the expected portable provider contract: %q", wantProviderConfig)
	}
	if err := os.MkdirAll(grokHome, 0o700); err != nil {
		t.Fatal(err)
	}
	routingPath := filepath.Join(grokHome, "AGENTS.md")
	originalRouting := []byte("# Personal Grok rules\n")
	if err := os.WriteFile(routingPath, originalRouting, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.mcpLogPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_FAIL_ADD_AGENT", "new-agent")

	if code := installCmd([]string{
		"grok", "--account", "default", "--realm", "default", "--agent", "new-agent",
		"--location", "home", "--capture", "raw", "--endpoint", fixture.serverURL,
		"--token-file", fixture.tokenPath,
	}); code != 1 {
		t.Fatalf("install code = %d, want 1", code)
	}

	gotProviderConfig, err := os.ReadFile(providerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotProviderConfig, wantProviderConfig) || !strings.Contains(string(gotProviderConfig), "prior-agent") {
		t.Fatalf("restored Grok provider config = %q, want exact bytes %q", gotProviderConfig, wantProviderConfig)
	}
	gotEffectiveState, err := os.ReadFile(fixture.genericState)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotEffectiveState, wantEffectiveState) {
		t.Fatalf("restored Grok effective MCP state = %q, want %q", gotEffectiveState, wantEffectiveState)
	}
	verifiedPrevious, err := hydrateLegacyGenericProviderConfig(previous, fixture.runtimeCLI, executable)
	if err != nil {
		t.Fatal(err)
	}
	wantBinding, err := genericMCPBindingFromConfig(verifiedPrevious)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyGrokNativeMCPBindingForConfig(fixture.runtimeCLI, verifiedPrevious, wantBinding); err != nil {
		t.Fatalf("restored Grok effective binding verification failed: %v", err)
	}
	gotRouting, err := os.ReadFile(routingPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRouting, originalRouting) {
		t.Fatalf("restored Grok AGENTS.md = %q, want %q", gotRouting, originalRouting)
	}
	if info, err := os.Stat(routingPath); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("Grok AGENTS.md mode after rollback: info=%v err=%v", info, err)
	}
	restoredConfig, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeGrokBuild)
	if err != nil {
		t.Fatal(err)
	}
	if restoredConfig.Agent != previous.Agent || restoredConfig.AgentID != previous.AgentID {
		t.Fatalf("restored Grok config = %#v, want prior binding %#v", restoredConfig, previous)
	}
	log, err := os.ReadFile(fixture.mcpLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "mcp remove --scope user witself") != 1 ||
		strings.Count(string(log), "mcp add --scope user") != 2 ||
		!strings.Contains(string(log), "--agent new-agent") ||
		!strings.Contains(string(log), "--agent prior-agent") {
		t.Fatalf("unexpected Grok reinstall rollback sequence: %s", log)
	}
}

func TestGrokInstallFailsClosedOnEmptyInspectionBeforeMutation(t *testing.T) {
	fixture := setupRollbackIntegrationFixture(t, "grok-test-bot")
	grokHome := filepath.Join(fixture.home, ".grok")
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("GROK_CLI_PATH", fixture.runtimeCLI)
	script := `#!/bin/sh
if [ "$1 $2 $3" = "mcp add --help" ]; then
  exit 0
fi
printf '%s\n' "$*" >> "$FAKE_MCP_LOG"
if [ "$1" = "--version" ]; then
  printf '%s\n' 'grok 0.2.101 (test) [stable]'
fi
exit 0
`
	if err := os.WriteFile(fixture.runtimeCLI, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	if code := installCmd([]string{
		"grok", "--account", "default", "--realm", "default", "--agent", "grok-test-bot",
		"--location", "home", "--capture", "raw", "--endpoint", fixture.serverURL,
		"--token-file", fixture.tokenPath,
	}); code != 1 {
		t.Fatalf("install code = %d, want 1", code)
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeGrokBuild)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		configPath,
		filepath.Join(grokHome, "AGENTS.md"),
		filepath.Join(grokHome, "hooks", "witself.json"),
		fixture.mcpStatePath,
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("empty inspection mutated %s: %v", path, err)
		}
	}
	log, err := os.ReadFile(fixture.mcpLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "inspect --json") || strings.Contains(string(log), "mcp add --scope user witself") {
		t.Fatalf("empty inspection command sequence = %s", log)
	}
}

func TestGrokInstallRollsBackOnEmptyMCPList(t *testing.T) {
	fixture := setupRollbackIntegrationFixture(t, "grok-test-bot")
	grokHome := filepath.Join(fixture.home, ".grok")
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("GROK_CLI_PATH", fixture.runtimeCLI)
	t.Setenv(fakeGenericRuntimeEnv, transcriptcapture.RuntimeGrokBuild)
	t.Setenv("WITSELF_FAKE_GENERIC_EMPTY_MCP_LIST", "1")

	if code := installCmd([]string{
		"grok", "--account", "default", "--realm", "default", "--agent", "grok-test-bot",
		"--location", "home", "--capture", "raw", "--endpoint", fixture.serverURL,
		"--token-file", fixture.tokenPath,
	}); code != 1 {
		t.Fatalf("install code = %d, want 1", code)
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeGrokBuild)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		configPath,
		filepath.Join(grokHome, "config.toml"),
		filepath.Join(grokHome, "AGENTS.md"),
		filepath.Join(grokHome, "hooks", "witself.json"),
		fixture.mcpStatePath,
		fixture.genericState,
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("empty MCP verification left mutation at %s: %v", path, err)
		}
	}
	log, err := os.ReadFile(fixture.mcpLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "mcp list --json") ||
		strings.Count(string(log), "mcp remove --scope user witself") != 1 ||
		strings.Count(string(log), "mcp add --scope user") != 1 {
		t.Fatalf("empty MCP verification rollback sequence = %s", log)
	}
}

func TestCursorFailedReinstallRestoresPriorBindingHooksMCPAndRule(t *testing.T) {
	fixture := setupRollbackIntegrationFixture(t, "new-agent")
	cursorHome := filepath.Join(fixture.home, ".cursor")
	enabledPath := setupCursorRollbackCLI(t, fixture, cursorHome)
	previous := saveCursorRollbackBinding(t, fixture, "prior-agent")
	executable := rollbackTestExecutable(t)

	if err := os.MkdirAll(cursorHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cursorHome, "mcp.json"),
		[]byte(`{"mcpServers":{"existing":{"command":"existing-mcp"}},"other":true}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := registerMCP(
		transcriptcapture.RuntimeCursor,
		fixture.runtimeCLI,
		executable,
		previous.Account,
		previous.Realm,
		previous.Agent,
		previous.Location.Name,
	); err != nil {
		t.Fatal(err)
	}
	hooksPath, err := transcriptcapture.InstallHooks(
		transcriptcapture.RuntimeCursor,
		previous.CaptureMode,
		executable,
		previous.Account,
		previous.Realm,
		previous.Agent,
		previous.Location.Name,
	)
	if err != nil {
		t.Fatal(err)
	}
	routingPath, err := cursorMemoryRoutingPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(routingPath), 0o700); err != nil {
		t.Fatal(err)
	}
	priorRouting := []byte(
		cursorMemoryRoutingBeginMarker + "\n" +
			"description: Prior Witself Cursor routing policy\n" +
			"alwaysApply: true\n" +
			"---\n" +
			"## Prior managed routing contract\n" +
			cursorMemoryRoutingEndMarker + "\n",
	)
	if err := os.WriteFile(routingPath, priorRouting, 0o640); err != nil {
		t.Fatal(err)
	}
	mcpPath, err := cursorMCPPath()
	if err != nil {
		t.Fatal(err)
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		path string
		raw  []byte
		mode os.FileMode
	}{}
	for name, path := range map[string]string{
		"routing": routingPath,
		"hooks":   hooksPath,
		"MCP":     mcpPath,
		"config":  configPath,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read initial %s: %v", name, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat initial %s: %v", name, err)
		}
		want[name] = struct {
			path string
			raw  []byte
			mode os.FileMode
		}{path: path, raw: raw, mode: info.Mode().Perm()}
	}
	if err := os.WriteFile(fixture.mcpLogPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CURSOR_FAIL_ENABLE_AGENT", "new-agent")

	if code := installCmd([]string{
		"cursor", "--account", "default", "--realm", "default", "--agent", "new-agent",
		"--location", "home", "--capture", "raw", "--endpoint", fixture.serverURL,
		"--token-file", fixture.tokenPath,
	}); code != 1 {
		t.Fatalf("install code = %d, want 1", code)
	}

	for name, state := range want {
		got, err := os.ReadFile(state.path)
		if err != nil {
			t.Fatalf("read restored %s: %v", name, err)
		}
		if !bytes.Equal(got, state.raw) {
			t.Fatalf("restored %s = %q, want %q", name, got, state.raw)
		}
		info, err := os.Stat(state.path)
		if err != nil {
			t.Fatalf("stat restored %s: %v", name, err)
		}
		if info.Mode().Perm() != state.mode {
			t.Fatalf("restored %s mode = %04o, want %04o", name, info.Mode().Perm(), state.mode)
		}
	}
	gotMCP, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gotMCP), "new-agent") || !strings.Contains(string(gotMCP), "prior-agent") ||
		!strings.Contains(string(gotMCP), "existing-mcp") {
		t.Fatalf("failed Cursor reinstall retained the wrong MCP state: %s", gotMCP)
	}
	restoredConfig, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	if restoredConfig.Agent != previous.Agent || restoredConfig.AgentID != previous.AgentID ||
		restoredConfig.Location.Name != previous.Location.Name {
		t.Fatalf("restored Cursor config = %#v, want prior binding %#v", restoredConfig, previous)
	}
	if _, err := os.Stat(enabledPath); err != nil {
		t.Fatalf("prior Cursor MCP binding was not re-enabled: %v", err)
	}
	log, err := os.ReadFile(fixture.mcpLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "mcp enable witself") != 2 {
		t.Fatalf("unexpected Cursor reinstall rollback sequence: %s", log)
	}
	transactionPath, err := genericProviderTransactionPath(transcriptcapture.RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transactionPath); !os.IsNotExist(err) {
		t.Fatalf("successful Cursor reinstall rollback retained transaction journal: %v", err)
	}
}

func TestCursorUnownedRoutingRuleBlocksInstallBeforeMCPOrHooks(t *testing.T) {
	fixture := setupRollbackIntegrationFixture(t, "new-agent")
	cursorHome := filepath.Join(fixture.home, ".cursor")
	enabledPath := setupCursorRollbackCLI(t, fixture, cursorHome)
	previous := saveCursorRollbackBinding(t, fixture, "prior-agent")
	executable := rollbackTestExecutable(t)
	if err := os.MkdirAll(cursorHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cursorHome, "mcp.json"),
		[]byte(`{"mcpServers":{"existing":{"command":"existing-mcp"}},"other":true}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := registerMCP(
		transcriptcapture.RuntimeCursor,
		fixture.runtimeCLI,
		executable,
		previous.Account,
		previous.Realm,
		previous.Agent,
		previous.Location.Name,
	); err != nil {
		t.Fatal(err)
	}
	hooksPath, err := transcriptcapture.InstallHooks(
		transcriptcapture.RuntimeCursor,
		previous.CaptureMode,
		executable,
		previous.Account,
		previous.Realm,
		previous.Agent,
		previous.Location.Name,
	)
	if err != nil {
		t.Fatal(err)
	}
	routingPath, err := cursorMemoryRoutingPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(routingPath), 0o700); err != nil {
		t.Fatal(err)
	}
	unownedRouting := []byte("---\ndescription: Personal Cursor rule\nalwaysApply: true\n---\n# Keep this rule\n")
	if err := os.WriteFile(routingPath, unownedRouting, 0o640); err != nil {
		t.Fatal(err)
	}
	mcpPath, err := cursorMCPPath()
	if err != nil {
		t.Fatal(err)
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		path string
		raw  []byte
		mode os.FileMode
	}{}
	for name, path := range map[string]string{
		"routing": routingPath,
		"hooks":   hooksPath,
		"MCP":     mcpPath,
		"config":  configPath,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read initial %s: %v", name, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat initial %s: %v", name, err)
		}
		want[name] = struct {
			path string
			raw  []byte
			mode os.FileMode
		}{path: path, raw: raw, mode: info.Mode().Perm()}
	}
	if err := os.WriteFile(fixture.mcpLogPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if code := installCmd([]string{
		"cursor", "--account", "default", "--realm", "default", "--agent", "new-agent",
		"--location", "home", "--capture", "raw", "--endpoint", fixture.serverURL,
		"--token-file", fixture.tokenPath,
	}); code != 1 {
		t.Fatalf("install code = %d, want 1", code)
	}

	for name, state := range want {
		got, err := os.ReadFile(state.path)
		if err != nil {
			t.Fatalf("read preserved %s: %v", name, err)
		}
		if !bytes.Equal(got, state.raw) {
			t.Fatalf("preserved %s = %q, want %q", name, got, state.raw)
		}
		info, err := os.Stat(state.path)
		if err != nil {
			t.Fatalf("stat preserved %s: %v", name, err)
		}
		if info.Mode().Perm() != state.mode {
			t.Fatalf("preserved %s mode = %04o, want %04o", name, info.Mode().Perm(), state.mode)
		}
	}
	restoredConfig, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	if restoredConfig.Agent != previous.Agent || restoredConfig.AgentID != previous.AgentID {
		t.Fatalf("restored Cursor config = %#v, want prior binding %#v", restoredConfig, previous)
	}
	if _, err := os.Stat(enabledPath); err != nil {
		t.Fatalf("pre-existing Cursor MCP binding was disabled: %v", err)
	}
	log, err := os.ReadFile(fixture.mcpLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "mcp enable witself") ||
		strings.Contains(string(log), "mcp disable witself") {
		t.Fatalf("Cursor MCP changed before routing preflight completed: %s", log)
	}
}

func TestHookInstallFailureRollsBackMCPConfigAndRouting(t *testing.T) {
	fixture := setupRollbackIntegrationFixture(t, "scott")
	if err := os.MkdirAll(fixture.codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	routingPath := filepath.Join(fixture.codexHome, "AGENTS.md")
	originalRouting := []byte("# Personal routing instructions\n")
	if err := os.WriteFile(routingPath, originalRouting, 0o640); err != nil {
		t.Fatal(err)
	}
	hooksPath := filepath.Join(fixture.codexHome, "hooks.json")
	originalHooks := []byte("{not valid json\n")
	if err := os.WriteFile(hooksPath, originalHooks, 0o600); err != nil {
		t.Fatal(err)
	}

	if code := installCmd([]string{
		"codex", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--capture", "raw", "--endpoint", fixture.serverURL,
		"--token-file", fixture.tokenPath, "--user-hooks",
	}); code != 1 {
		t.Fatalf("install code = %d, want 1", code)
	}

	if _, err := os.Stat(fixture.mcpStatePath); !os.IsNotExist(err) {
		t.Fatalf("failed install retained MCP binding: %v", err)
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("failed install retained integration config: %v", err)
	}
	gotRouting, err := os.ReadFile(routingPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRouting, originalRouting) {
		t.Fatalf("routing rollback = %q, want %q", gotRouting, originalRouting)
	}
	if info, err := os.Stat(routingPath); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("routing mode after rollback: info=%v err=%v", info, err)
	}
	gotHooks, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotHooks, originalHooks) {
		t.Fatalf("failed hook install changed original hooks: %q", gotHooks)
	}
	log, err := os.ReadFile(fixture.mcpLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "mcp add witself") || strings.Count(string(log), "mcp remove witself") != 1 {
		t.Fatalf("expected successful MCP add followed by rollback removal: %s", log)
	}
}

func TestCursorHookInstallFailureRollsBackMCPConfigAndRouting(t *testing.T) {
	fixture := setupRollbackIntegrationFixture(t, "scott")
	cursorHome := filepath.Join(fixture.home, ".cursor")
	t.Setenv("CURSOR_CONFIG_DIR", "")
	t.Setenv("CURSOR_CLI_PATH", fixture.runtimeCLI)
	if err := os.MkdirAll(cursorHome, 0o700); err != nil {
		t.Fatal(err)
	}
	hooksPath := filepath.Join(cursorHome, "hooks.json")
	originalHooks := []byte("{not valid json\n")
	if err := os.WriteFile(hooksPath, originalHooks, 0o600); err != nil {
		t.Fatal(err)
	}

	if code := installCmd([]string{
		"cursor", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--capture", "raw", "--endpoint", fixture.serverURL,
		"--token-file", fixture.tokenPath,
	}); code != 1 {
		t.Fatalf("install code = %d, want 1", code)
	}

	for name, path := range map[string]string{
		"MCP configuration": filepath.Join(cursorHome, "mcp.json"),
		"CLI permissions":   filepath.Join(cursorHome, "cli-config.json"),
		"routing rule":      filepath.Join(cursorHome, "rules", cursorMemoryRoutingRuleFile),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("failed Cursor install retained %s: %v", name, err)
		}
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("failed Cursor install retained integration config: %v", err)
	}
	gotHooks, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotHooks, originalHooks) {
		t.Fatalf("failed Cursor hook install changed original hooks: %q", gotHooks)
	}
	log, err := os.ReadFile(fixture.mcpLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "mcp enable witself") ||
		!strings.Contains(string(log), "mcp disable witself") {
		t.Fatalf("expected Cursor MCP enable followed by rollback disable: %s", log)
	}
}

func TestCursorMalformedRoutingPreflightPreservesIntegration(t *testing.T) {
	home := t.TempDir()
	witselfHome := filepath.Join(home, ".witself")
	runtimeCLI := filepath.Join(home, "cursor")
	cliLog := filepath.Join(home, "cursor-args.log")
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", witselfHome)
	t.Setenv("CURSOR_CONFIG_DIR", "")
	t.Setenv("CURSOR_CLI_PATH", runtimeCLI)
	t.Setenv("FAKE_CURSOR_LOG", cliLog)
	setInstallExecutableForTest(t)
	if err := os.WriteFile(runtimeCLI, []byte(
		"#!/bin/sh\n"+
			"if [ \"$1 $2\" = \"mcp --help\" ]; then printf '%s\\n' 'Manage MCP servers'; exit 0; fi\n"+
			"printf '%s\\n' \"$*\" >> \"$FAKE_CURSOR_LOG\"\nexit 0\n",
	), 0o700); err != nil {
		t.Fatal(err)
	}
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCursor, RuntimeVersion: "3.11.13",
		CaptureMode: transcriptcapture.ModeRaw, HookMode: transcriptcapture.HookModeUser,
		Account: "default", AccountID: "acc_1", Realm: "default", RealmID: "realm_1",
		Agent: "scott", AgentID: "agent_1", AgentName: "scott", Location: location,
	}); err != nil {
		t.Fatal(err)
	}
	executable := rollbackTestExecutable(t)
	hooksPath, err := transcriptcapture.InstallHooks(
		transcriptcapture.RuntimeCursor,
		transcriptcapture.ModeRaw,
		executable,
		"default",
		"default",
		"scott",
		"home",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerCursorMCP([]string{
		executable, "mcp", "serve", "--runtime", transcriptcapture.RuntimeCursor,
		"--account", "default", "--realm", "default", "--agent", "scott", "--location", "home",
	}); err != nil {
		t.Fatal(err)
	}
	mcpPath, err := cursorMCPPath()
	if err != nil {
		t.Fatal(err)
	}
	routingPath, err := cursorMemoryRoutingPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(routingPath), 0o700); err != nil {
		t.Fatal(err)
	}
	malformedRouting := []byte(cursorMemoryRoutingBeginMarker + "\ndescription: incomplete\n")
	if err := os.WriteFile(routingPath, malformedRouting, 0o600); err != nil {
		t.Fatal(err)
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{}
	for name, path := range map[string]string{
		"routing": routingPath,
		"hooks":   hooksPath,
		"MCP":     mcpPath,
		"config":  configPath,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read initial %s: %v", name, err)
		}
		want[name] = raw
	}

	if code := uninstallCmd([]string{"cursor"}); code != 1 {
		t.Fatalf("uninstall code = %d, want 1", code)
	}
	for name, path := range map[string]string{
		"routing": routingPath,
		"hooks":   hooksPath,
		"MCP":     mcpPath,
		"config":  configPath,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read preserved %s: %v", name, err)
		}
		if !bytes.Equal(got, want[name]) {
			t.Fatalf("%s changed after malformed-routing preflight: got %q want %q", name, got, want[name])
		}
	}
	if _, err := os.Stat(cliLog); !os.IsNotExist(err) {
		t.Fatalf("Cursor CLI ran before routing preflight completed: %v", err)
	}
}

func TestCursorFailedUninstallRestoresRuleHooksMCPAndBinding(t *testing.T) {
	fixture := setupRollbackIntegrationFixture(t, "scott")
	cursorHome := filepath.Join(fixture.home, ".cursor")
	enabledPath := setupCursorRollbackCLI(t, fixture, cursorHome)
	if err := os.MkdirAll(cursorHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cursorHome, "mcp.json"),
		[]byte(`{"mcpServers":{"existing":{"command":"existing-mcp"}},"other":true}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	if code := installCmd([]string{
		"cursor", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--capture", "raw", "--endpoint", fixture.serverURL,
		"--token-file", fixture.tokenPath,
	}); code != 0 {
		t.Fatalf("initial install code = %d", code)
	}
	hooksPath := filepath.Join(cursorHome, "hooks.json")
	routingPath, err := cursorMemoryRoutingPath()
	if err != nil {
		t.Fatal(err)
	}
	mcpPath, err := cursorMCPPath()
	if err != nil {
		t.Fatal(err)
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		path string
		raw  []byte
		mode os.FileMode
	}{}
	for name, path := range map[string]string{
		"routing": routingPath,
		"hooks":   hooksPath,
		"MCP":     mcpPath,
		"CLI":     filepath.Join(cursorHome, "cli-config.json"),
		"config":  configPath,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read initial %s: %v", name, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat initial %s: %v", name, err)
		}
		want[name] = struct {
			path string
			raw  []byte
			mode os.FileMode
		}{path: path, raw: raw, mode: info.Mode().Perm()}
	}
	if err := os.WriteFile(fixture.mcpLogPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	originalRemove := removeRuntimeIntegrationConfig
	removeRuntimeIntegrationConfig = func(_ string) error {
		return os.ErrPermission
	}
	t.Cleanup(func() { removeRuntimeIntegrationConfig = originalRemove })

	if code := uninstallCmd([]string{"cursor"}); code != 1 {
		t.Fatalf("uninstall code = %d, want 1", code)
	}

	for name, state := range want {
		got, err := os.ReadFile(state.path)
		if err != nil {
			t.Fatalf("read restored %s: %v", name, err)
		}
		if !bytes.Equal(got, state.raw) {
			t.Fatalf("restored %s = %q, want %q", name, got, state.raw)
		}
		info, err := os.Stat(state.path)
		if err != nil {
			t.Fatalf("stat restored %s: %v", name, err)
		}
		if info.Mode().Perm() != state.mode {
			t.Fatalf("restored %s mode = %04o, want %04o", name, info.Mode().Perm(), state.mode)
		}
	}
	if _, err := os.Stat(enabledPath); err != nil {
		t.Fatalf("Cursor MCP binding was not re-enabled after rollback: %v", err)
	}
	log, err := os.ReadFile(fixture.mcpLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "mcp disable witself") ||
		!strings.Contains(string(log), "mcp enable witself") {
		t.Fatalf("Cursor uninstall rollback did not disable and restore MCP: %s", log)
	}
	transactionPath, err := genericProviderTransactionPath(transcriptcapture.RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transactionPath); !os.IsNotExist(err) {
		t.Fatalf("successful Cursor uninstall rollback retained transaction journal: %v", err)
	}
}

func TestFailedUninstallMCPRemovalRestoresHooksRoutingAndBinding(t *testing.T) {
	fixture := setupRollbackIntegrationFixture(t, "scott")
	if err := os.MkdirAll(fixture.codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	routingPath := filepath.Join(fixture.codexHome, "AGENTS.md")
	if err := os.WriteFile(routingPath, []byte("# Keep this personal rule\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if code := installCmd([]string{
		"codex", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--capture", "raw", "--endpoint", fixture.serverURL,
		"--token-file", fixture.tokenPath, "--user-hooks",
	}); code != 0 {
		t.Fatalf("initial install code = %d", code)
	}

	hooksPath := filepath.Join(fixture.codexHome, "hooks.json")
	wantHooks, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	wantRouting, err := os.ReadFile(routingPath)
	if err != nil {
		t.Fatal(err)
	}
	providerConfigPath := filepath.Join(fixture.codexHome, "config.toml")
	wantMCP, err := os.ReadFile(providerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	wantConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.mcpLogPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.removeFailLog, []byte("fail next removal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeGenericFailRemoveEnv, fixture.removeFailLog)

	if code := uninstallCmd([]string{"codex"}); code != 1 {
		t.Fatalf("uninstall code = %d, want 1", code)
	}
	if _, err := os.Stat(fixture.removeFailLog); !os.IsNotExist(err) {
		t.Fatalf("MCP removal failure marker was not consumed: %v", err)
	}

	for name, tc := range map[string]struct {
		path string
		want []byte
	}{
		"hooks":   {hooksPath, wantHooks},
		"routing": {routingPath, wantRouting},
		"MCP":     {providerConfigPath, wantMCP},
		"config":  {configPath, wantConfig},
	} {
		got, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read restored %s: %v", name, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("restored %s = %q, want %q", name, got, tc.want)
		}
	}
	log, err := os.ReadFile(fixture.mcpLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "mcp remove witself") != 1 || strings.Contains(string(log), "mcp add witself") {
		t.Fatalf("unexpected MCP rollback sequence: %s", log)
	}
}

func TestFailedUninstallRestoresPreexistingUserAndManagedHookTiers(t *testing.T) {
	fixture := setupRollbackIntegrationFixture(t, "scott")
	if code := installCmd([]string{
		"codex", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--capture", "raw", "--endpoint", fixture.serverURL,
		"--token-file", fixture.tokenPath, "--user-hooks",
	}); code != 0 {
		t.Fatalf("initial install code = %d", code)
	}
	executable := rollbackTestExecutable(t)
	managedPath, err := installManagedRuntimeHooks(
		transcriptcapture.RuntimeCodex,
		transcriptcapture.ModeRaw,
		executable,
		"default",
		"default",
		"scott",
		"home",
	)
	if err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(fixture.codexHome, "hooks.json")
	wantUser, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatal(err)
	}
	wantManaged, err := os.ReadFile(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := snapshotRuntimeHooks(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if !state.userPresent || !state.managedPresent {
		t.Fatalf("pre-mutation hook snapshot = %#v, want both tiers", state)
	}
	if err := os.WriteFile(fixture.mcpLogPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_FAIL_REMOVE_ONCE", "1")

	if code := uninstallCmd([]string{"codex", "--managed-hooks"}); code != 1 {
		t.Fatalf("uninstall code = %d, want 1", code)
	}
	for name, tc := range map[string]struct {
		path string
		want []byte
	}{
		"user hooks":    {path: userPath, want: wantUser},
		"managed hooks": {path: managedPath, want: wantManaged},
	} {
		got, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read restored %s: %v", name, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("restored %s = %q, want %q", name, got, tc.want)
		}
	}
	state, err = snapshotRuntimeHooks(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if !state.userPresent || !state.managedPresent {
		t.Fatalf("restored hook snapshot = %#v, want both tiers", state)
	}
}

type claudeUninstallFixture struct {
	routingPath string
	hooksPath   string
	configPath  string
}

func setupClaudeUninstallFixture(t *testing.T, runtimeCLI string) claudeUninstallFixture {
	t.Helper()
	home := t.TempDir()
	claudeHome := filepath.Join(home, ".claude")
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CLAUDE_CLI_PATH", runtimeCLI)
	setInstallExecutableForTest(t)

	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeClaudeCode, CaptureMode: transcriptcapture.ModeRaw,
		HookMode: transcriptcapture.HookModeUser, Account: "default", Realm: "default",
		Agent: "scott", AgentID: "agent_1", AgentName: "scott", Location: location,
	}); err != nil {
		t.Fatal(err)
	}
	executable := rollbackTestExecutable(t)
	hooksPath, err := transcriptcapture.InstallHooks(
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.ModeRaw,
		executable,
		"default",
		"default",
		"scott",
		"home",
	)
	if err != nil {
		t.Fatal(err)
	}
	routing, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	return claudeUninstallFixture{
		routingPath: routing.path,
		hooksPath:   hooksPath,
		configPath:  configPath,
	}
}

func TestUninstallMissingRuntimeCLIPreservesLocalIntegration(t *testing.T) {
	home := t.TempDir()
	missingCLI := filepath.Join(home, "missing-claude")
	t.Setenv("PATH", filepath.Join(home, "empty-path"))
	fixture := setupClaudeUninstallFixture(t, missingCLI)

	want := map[string][]byte{}
	for name, path := range map[string]string{
		"routing": fixture.routingPath,
		"hooks":   fixture.hooksPath,
		"config":  fixture.configPath,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want[name] = raw
	}

	if code := uninstallCmd([]string{"claude"}); code != 1 {
		t.Fatalf("uninstall code = %d, want 1", code)
	}
	for name, path := range map[string]string{
		"routing": fixture.routingPath,
		"hooks":   fixture.hooksPath,
		"config":  fixture.configPath,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read preserved %s: %v", name, err)
		}
		if !bytes.Equal(got, want[name]) {
			t.Fatalf("%s changed without a runtime CLI: got %q want %q", name, got, want[name])
		}
	}
}

func TestUninstallAlreadyMissingMCPPreservesLocalState(t *testing.T) {
	home := t.TempDir()
	logPath := filepath.Join(home, "claude-args.log")
	runtimeCLI := filepath.Join(home, "claude")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_CLAUDE_LOG"
if [ "$1" = "mcp" ] && [ "$2" = "add-json" ] && [ "$3" = "--help" ]; then
  exit 0
fi
if [ "$1" = "mcp" ] && [ "$2" = "remove" ]; then
  printf '%s\n' 'No MCP server named "witself" in user scope' >&2
  exit 1
fi
exit 1
`
	if err := os.WriteFile(runtimeCLI, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLAUDE_LOG", logPath)
	t.Setenv("PATH", filepath.Join(home, "empty-path"))
	fixture := setupClaudeUninstallFixture(t, runtimeCLI)
	want := map[string][]byte{}
	for name, path := range map[string]string{
		"routing": fixture.routingPath,
		"hooks":   fixture.hooksPath,
		"config":  fixture.configPath,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want[name] = raw
	}

	if code := uninstallCmd([]string{"claude"}); code != 1 {
		t.Fatalf("uninstall code = %d, want 1", code)
	}
	for name, path := range map[string]string{
		"routing": fixture.routingPath,
		"hooks":   fixture.hooksPath,
		"config":  fixture.configPath,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read preserved %s: %v", name, err)
		}
		if !bytes.Equal(got, want[name]) {
			t.Fatalf("%s changed after missing-provider preflight: got %q want %q", name, got, want[name])
		}
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "mcp remove --scope user witself") {
		t.Fatalf("missing exact-owned MCP state triggered an unsafe provider mutation: %s", log)
	}
}

func TestMCPRegistrationAlreadyMissingIsNarrow(t *testing.T) {
	for _, output := range [][]byte{
		[]byte("No MCP server named 'witself' found."),
		[]byte(`No MCP server named "witself" in user scope`),
		[]byte("No MCP server named 'witself' in user config"),
	} {
		if !mcpRegistrationAlreadyMissing(output) {
			t.Errorf("did not recognize missing Witself registration: %q", output)
		}
	}
	for _, output := range [][]byte{
		nil,
		[]byte("permission denied while removing witself"),
		[]byte("No MCP server named 'other' in user scope"),
		[]byte("configuration parse failure"),
	} {
		if mcpRegistrationAlreadyMissing(output) {
			t.Errorf("misclassified MCP removal failure: %q", output)
		}
	}
}

func TestUninstallWithoutConfigRefusesUnsafeCleanup(t *testing.T) {
	home := t.TempDir()
	claudeHome := filepath.Join(home, ".claude")
	runtimeCLI := filepath.Join(home, "claude")
	script := `#!/bin/sh
if [ "$1" = "mcp" ] && [ "$2" = "add" ] && [ "$3" = "--help" ]; then
  exit 0
fi
if [ "$1" = "mcp" ] && [ "$2" = "remove" ]; then
  printf '%s\n' 'forced MCP removal failure' >&2
  exit 19
fi
exit 1
`
	if err := os.WriteFile(runtimeCLI, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CLAUDE_CLI_PATH", runtimeCLI)
	t.Setenv("PATH", filepath.Join(home, "empty-path"))
	setInstallExecutableForTest(t)
	executable := rollbackTestExecutable(t)
	hooksPath, err := transcriptcapture.InstallHooks(
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.ModeRaw,
		executable,
		"default",
		"default",
		"scott",
		"home",
	)
	if err != nil {
		t.Fatal(err)
	}
	routing, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	wantHooks, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	wantRouting, err := os.ReadFile(routing.path)
	if err != nil {
		t.Fatal(err)
	}

	if code := uninstallCmd([]string{"claude"}); code != 1 {
		t.Fatalf("uninstall code = %d, want 1", code)
	}
	for name, tc := range map[string]struct {
		path string
		want []byte
	}{
		"hooks":   {hooksPath, wantHooks},
		"routing": {routing.path, wantRouting},
	} {
		got, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read preserved %s: %v", name, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("%s changed after failed no-config uninstall: got %q want %q", name, got, tc.want)
		}
	}
}

func TestRestoreUserHooksRemovesManagedHooksWhenUserRestoreFails(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	managedRoot := filepath.Join(home, "managed")
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv(managedHooksTestRootEnv, managedRoot)
	setInstallExecutableForTest(t)
	executable := rollbackTestExecutable(t)

	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "hooks.json"), []byte("{malformed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policyPath, err := installManagedRuntimeHooks(
		transcriptcapture.RuntimeCodex,
		transcriptcapture.ModeRaw,
		executable,
		"default",
		"default",
		"scott",
		"home",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(policyPath); err != nil {
		t.Fatal(err)
	}
	previous := &transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCodex, CaptureMode: transcriptcapture.ModeRaw,
		HookMode: transcriptcapture.HookModeUser, Account: "default", Realm: "default",
		Agent: "scott", AgentName: "scott", Location: transcriptcapture.Location{Name: "home"},
	}
	if err := restoreRuntimeHooksBinding(transcriptcapture.RuntimeCodex, executable, previous); err == nil {
		t.Fatal("hook restoration unexpectedly accepted malformed user hooks")
	}
	if _, err := os.Stat(policyPath); !os.IsNotExist(err) {
		t.Fatalf("managed hook policy remained after rollback: %v", err)
	}
}
