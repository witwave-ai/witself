package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/memorycurator"
	"github.com/witwave-ai/witself/internal/memoryhydration"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestVerifyInstallAgentIdentityPinsRequestedScope(t *testing.T) {
	identity := client.SelfIdentity{
		AccountID: "acc_1", RealmID: "rlm_1", RealmName: "default",
		AgentID: "agt_1", AgentName: "scott",
	}
	tests := []struct {
		name    string
		conn    agentConnection
		mutate  func(*client.SelfIdentity)
		wantErr string
	}{
		{name: "managed binding", conn: agentConnection{AccountID: "acc_1", RealmName: "default", AgentName: "scott"}},
		{name: "explicit token without managed account id", conn: agentConnection{RealmName: "default", AgentName: "scott"}},
		{name: "managed account mismatch", conn: agentConnection{AccountID: "acc_other", RealmName: "default", AgentName: "scott"}, wantErr: "local account"},
		{name: "requested realm mismatch", conn: agentConnection{AccountID: "acc_1", RealmName: "other", AgentName: "scott"}, wantErr: "requested realm"},
		{name: "requested agent mismatch", conn: agentConnection{AccountID: "acc_1", RealmName: "default", AgentName: "other"}, wantErr: "requested agent"},
		{name: "incomplete server identity", conn: agentConnection{AccountID: "acc_1", RealmName: "default", AgentName: "scott"}, mutate: func(identity *client.SelfIdentity) {
			identity.RealmID = ""
		}, wantErr: "incomplete agent identity"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotIdentity := identity
			if tc.mutate != nil {
				tc.mutate(&gotIdentity)
			}
			err := verifyInstallAgentIdentity(tc.conn, gotIdentity)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("identity error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestManagedHookSupportMatchesNativePlatformContract(t *testing.T) {
	for _, tc := range []struct {
		platform string
		runtime  string
		want     bool
	}{
		{platform: "darwin", runtime: transcriptcapture.RuntimeCodex, want: true},
		{platform: "linux", runtime: transcriptcapture.RuntimeCodex, want: true},
		{platform: "windows", runtime: transcriptcapture.RuntimeCodex, want: false},
		{platform: "darwin", runtime: transcriptcapture.RuntimeClaudeCode, want: true},
		{platform: "linux", runtime: transcriptcapture.RuntimeClaudeCode, want: true},
		{platform: "windows", runtime: transcriptcapture.RuntimeClaudeCode, want: false},
		{platform: "darwin", runtime: transcriptcapture.RuntimeGrokBuild, want: false},
	} {
		t.Run(tc.platform+"/"+tc.runtime, func(t *testing.T) {
			if got := supportsManagedHooksForPlatform(tc.runtime, tc.platform); got != tc.want {
				t.Fatalf("managed hooks = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestTranscriptHookSupportMatchesNativePlatformContract(t *testing.T) {
	for _, tc := range []struct {
		platform string
		runtime  string
		want     bool
	}{
		{platform: "darwin", runtime: transcriptcapture.RuntimeCodex, want: true},
		{platform: "linux", runtime: transcriptcapture.RuntimeClaudeCode, want: true},
		{platform: "linux", runtime: transcriptcapture.RuntimeGrokBuild, want: true},
		{platform: "linux", runtime: transcriptcapture.RuntimeCursor, want: true},
		{platform: "windows", runtime: transcriptcapture.RuntimeCodex, want: true},
		{platform: "windows", runtime: transcriptcapture.RuntimeClaudeCode, want: false},
		{platform: "windows", runtime: transcriptcapture.RuntimeGrokBuild, want: false},
		{platform: "windows", runtime: transcriptcapture.RuntimeCursor, want: false},
		{platform: "linux", runtime: transcriptcapture.RuntimeOpenClaw, want: false},
		{platform: "linux", runtime: transcriptcapture.RuntimeAntigravity, want: false},
		{platform: "linux", runtime: transcriptcapture.RuntimeCopilot, want: false},
	} {
		t.Run(tc.platform+"/"+tc.runtime, func(t *testing.T) {
			if got := supportsTranscriptHooksForPlatform(tc.runtime, tc.platform); got != tc.want {
				t.Fatalf("transcript hooks = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestIntegrationInstallPlatformRejectsNativeWindowsCursor(t *testing.T) {
	err := validateIntegrationInstallPlatform(transcriptcapture.RuntimeCursor, "windows")
	if err == nil || !strings.Contains(err.Error(), "same WSL") {
		t.Fatalf("Windows Cursor platform error = %v", err)
	}
	if err := validateIntegrationInstallPlatform(transcriptcapture.RuntimeCursor, "linux"); err != nil {
		t.Fatalf("Linux Cursor platform error = %v", err)
	}
}

func TestRuntimeCLIPlatformBoundaryRejectsWindowsInteropExecutableInLinux(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.exe")
	if err := os.WriteFile(path, []byte{'M', 'Z', 0, 0}, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeCLIPlatformBoundaryForPlatform("linux", path); !errors.Is(err, errRuntimeCLIIncompatible) {
		t.Fatalf("Linux boundary error = %v", err)
	}
	if err := validateRuntimeCLIPlatformBoundaryForPlatform("windows", path); err != nil {
		t.Fatalf("Windows boundary error = %v", err)
	}
}

func TestProviderIntegrationContractCodex(t *testing.T) {
	home := t.TempDir()
	setInstallExecutableForTest(t)
	witselfHome := filepath.Join(home, ".witself")
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", witselfHome)
	t.Setenv("CODEX_HOME", codexHome)
	provider := buildFakeProviderCLI(t, home)
	t.Setenv("CODEX_CLI_PATH", provider.Path)
	siblingMCP := []string{"sibling-mcp", "serve", "--safe"}
	provider.writeRegistry(t, map[string][]string{"sibling": siblingMCP})

	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	originalHooks := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"custom-check"}]}]}}`
	hooksPath := filepath.Join(codexHome, "hooks.json")
	if err := os.WriteFile(hooksPath, []byte(originalHooks), 0o600); err != nil {
		t.Fatal(err)
	}
	originalInstructions := "# Personal Codex instructions\n\nKeep this paragraph.\n"
	instructionsPath := filepath.Join(codexHome, "AGENTS.md")
	if err := os.WriteFile(instructionsPath, []byte(originalInstructions), 0o600); err != nil {
		t.Fatal(err)
	}

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
	installArgs := []string{
		"codex", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--capture", "raw", "--endpoint", srv.URL,
		"--token-file", tokenPath, "--user-hooks",
	}
	if code := installCmd(installArgs); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccountID != "acc_1" || cfg.RealmID != "realm_1" || cfg.AgentID != "agent_1" ||
		cfg.AgentName != "scott" || cfg.Location.Name != "home" || cfg.RuntimeVersion != "1.2.3" {
		t.Fatalf("config = %#v", cfg)
	}
	hooks, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	instructions, err := os.ReadFile(instructionsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hooks), "custom-check") ||
		!strings.Contains(string(hooks), "transcript hook --runtime codex") ||
		!strings.Contains(string(hooks), "--account 'default' --realm 'default' --agent 'scott' --location 'home'") {
		t.Fatalf("installed hooks = %s", hooks)
	}
	if !strings.Contains(string(instructions), originalInstructions) ||
		strings.Count(string(instructions), codexMemoryRoutingInstructions) != 1 {
		t.Fatalf("installed instructions = %s", instructions)
	}
	installedHooks := append([]byte(nil), hooks...)
	installedInstructions := append([]byte(nil), instructions...)

	state := provider.readRegistry(t)
	if !slices.Equal(state.Servers["sibling"], siblingMCP) {
		t.Fatalf("sibling MCP entry = %q, want %q", state.Servers["sibling"], siblingMCP)
	}
	witselfServeArgs := state.Servers["witself"]
	if len(witselfServeArgs) == 0 {
		t.Fatalf("Witself MCP entry is missing: %#v", state.Servers)
	}

	// A second install is the supported upgrade/reinstall operation. It must
	// replace the exact Witself MCP entry without duplicating hooks, routing, or
	// disturbing provider-owned siblings.
	if code := installCmd(installArgs); code != 0 {
		t.Fatalf("reinstall code = %d", code)
	}
	reinstalledHooks, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	reinstalledInstructions, err := os.ReadFile(instructionsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reinstalledHooks, installedHooks) {
		t.Fatalf("reinstall changed hooks:\n%s\n---\n%s", installedHooks, reinstalledHooks)
	}
	if !slices.Equal(reinstalledInstructions, installedInstructions) {
		t.Fatalf("reinstall changed managed instructions:\n%s\n---\n%s", installedInstructions, reinstalledInstructions)
	}
	state = provider.readRegistry(t)
	if !slices.Equal(state.Servers["sibling"], siblingMCP) || !slices.Equal(state.Servers["witself"], witselfServeArgs) {
		t.Fatalf("reinstall changed provider siblings or MCP args: %#v", state.Servers)
	}

	if code := uninstallCmd([]string{"codex"}); code != 0 {
		t.Fatalf("uninstall code = %d", code)
	}
	uninstalledHooks, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(uninstalledHooks), "custom-check") || strings.Contains(string(uninstalledHooks), "transcript hook --runtime codex") {
		t.Fatalf("uninstall did not preserve only sibling hooks: %s", uninstalledHooks)
	}
	uninstalledInstructions, err := os.ReadFile(instructionsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(uninstalledInstructions) != originalInstructions {
		t.Fatalf("uninstall instructions = %q, want %q", uninstalledInstructions, originalInstructions)
	}
	state = provider.readRegistry(t)
	if _, exists := state.Servers["witself"]; exists || !slices.Equal(state.Servers["sibling"], siblingMCP) {
		t.Fatalf("uninstall changed provider siblings or retained Witself: %#v", state.Servers)
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("integration config remains after uninstall: %v", err)
	}
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("uninstall removed token file: %v", err)
	}

	calls := provider.invocations(t)
	var adds, removes [][]string
	for _, call := range calls {
		switch {
		case len(call.Args) >= 2 && call.Args[0] == "mcp" && call.Args[1] == "add" &&
			(len(call.Args) != 3 || call.Args[2] != "--help"):
			adds = append(adds, call.Args)
		case len(call.Args) >= 2 && call.Args[0] == "mcp" && call.Args[1] == "remove":
			removes = append(removes, call.Args)
		}
	}
	if len(adds) != 1 || len(removes) != 1 {
		t.Fatalf("MCP lifecycle calls: adds=%#v removes=%#v all=%#v", adds, removes, calls)
	}
	for _, remove := range removes {
		if !slices.Equal(remove, []string{"mcp", "remove", "witself"}) {
			t.Fatalf("MCP remove args = %q", remove)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	wantAdd := append([]string{"mcp", "add", "witself", "--env", "WITSELF_HOME=" + cfg.MCPEnvironment["WITSELF_HOME"], "--"}, runtimeMCPServeArgs(
		transcriptcapture.RuntimeCodex, executable, "default", "default", "scott", "home",
	)...)
	for _, add := range adds {
		if !slices.Equal(add, wantAdd) {
			t.Fatalf("MCP add args = %q, want %q", add, wantAdd)
		}
	}
	providerArtifacts := string(installedHooks) + string(installedInstructions)
	for _, call := range calls {
		providerArtifacts += strings.Join(call.Args, "\x00")
	}
	registryRaw, err := os.ReadFile(provider.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	providerArtifacts += string(registryRaw)
	if strings.Contains(providerArtifacts, "agent-token") {
		t.Fatal("agent token was embedded in a Codex hook, instruction, MCP argument, or provider registry")
	}
}

func TestBareInstallReusesBindingAndDefaultsToManagedHooks(t *testing.T) {
	home := t.TempDir()
	setInstallExecutableForTest(t)
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv(managedHooksTestRootEnv, filepath.Join(home, "managed"))
	logPath := filepath.Join(home, "codex-args.log")
	t.Setenv("FAKE_CLI_LOG", logPath)
	t.Setenv(fakeGenericRuntimeEnv, transcriptcapture.RuntimeCodex)
	t.Setenv(fakeGenericLogEnv, filepath.Join(home, "codex-invocations.jsonl"))
	t.Setenv(fakeGenericStateEnv, filepath.Join(home, "codex-provider-state.json"))
	fakeCLI := copyGenericProviderFixture(t, filepath.Join(home, "codex"))
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
	loc, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCodex, CaptureMode: transcriptcapture.ModeRaw,
		HookMode: transcriptcapture.HookModeUser, Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Endpoint: srv.URL, TokenFile: tokenPath, Location: loc,
	}); err != nil {
		t.Fatal(err)
	}
	legacy, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := currentExecutablePath()
	if err != nil {
		t.Fatal(err)
	}
	legacy, err = hydrateLegacyGenericProviderConfig(legacy, fakeCLI, executable)
	if err != nil {
		t.Fatal(err)
	}
	legacyBinding, err := genericMCPBindingFromConfig(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := addGenericMCPUnchecked(fakeCLI, legacy, legacyBinding); err != nil {
		t.Fatal(err)
	}
	if code := installCmd([]string{"codex"}); code != 0 {
		t.Fatalf("bare install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HookMode != transcriptcapture.HookModeManaged || cfg.Agent != "scott" || cfg.Location.Name != "home" {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestInferInstallAgentRequiresOnlyAmbiguousChoice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	if err := local.Save("default", local.Account{ID: "acc_1"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	writeAgent := func(name string) {
		t.Helper()
		path, err := local.AgentTokenPath("default", "default", name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAgent("scott")
	name, err := inferInstallAgent("", "")
	if err != nil || name != "scott" {
		t.Fatalf("inferred %q, err %v", name, err)
	}
	writeAgent("alex")
	if _, err := inferInstallAgent("", ""); err == nil || !strings.Contains(err.Error(), "multiple local agents") {
		t.Fatalf("ambiguous error = %v", err)
	}
}

func TestRuntimeTargetsNormalizeAliasesAndPreserveOrder(t *testing.T) {
	got, err := runtimeTargets("claude,codex,grok,cursor,agy,github-copilot,claude-code,antigravity,copilot")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor,
		transcriptcapture.RuntimeAntigravity,
		transcriptcapture.RuntimeCopilot,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	if _, err := runtimeTargets("codex,,cursor"); err == nil {
		t.Fatal("empty runtime item was accepted")
	}
}

func TestCommaSeparatedInstallConfiguresEveryRuntime(t *testing.T) {
	home := t.TempDir()
	setInstallExecutableForTest(t)
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	t.Setenv("CURSOR_CONFIG_DIR", "")
	managedRoot := filepath.Join(home, "managed")
	t.Setenv(managedHooksTestRootEnv, managedRoot)
	t.Setenv(fakeGenericRuntimeEnv, "")
	t.Setenv(fakeGenericLogEnv, filepath.Join(home, "generic-invocations.jsonl"))
	t.Setenv(fakeGenericStateEnv, filepath.Join(home, "generic-provider-state.json"))

	for envName, cliName := range map[string]string{
		"CODEX_CLI_PATH":  "codex",
		"CLAUDE_CLI_PATH": "claude",
		"GROK_CLI_PATH":   "grok",
		"CURSOR_CLI_PATH": "cursor",
	} {
		fakeCLI := copyGenericProviderFixture(t, filepath.Join(home, cliName))
		t.Setenv(envName, fakeCLI)
	}

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
		"claude,codex,grok,cursor", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--capture", "raw", "--endpoint", srv.URL, "--token-file", tokenPath,
	}); code != 0 {
		t.Fatalf("install code = %d", code)
	}

	for _, tc := range []struct {
		runtime  string
		hookMode string
	}{
		{transcriptcapture.RuntimeClaudeCode, transcriptcapture.HookModeManaged},
		{transcriptcapture.RuntimeCodex, transcriptcapture.HookModeManaged},
		{transcriptcapture.RuntimeGrokBuild, transcriptcapture.HookModeUser},
		{transcriptcapture.RuntimeCursor, transcriptcapture.HookModeUser},
	} {
		cfg, err := transcriptcapture.LoadConfig(tc.runtime)
		if err != nil {
			t.Fatalf("load %s config: %v", tc.runtime, err)
		}
		if cfg.HookMode != tc.hookMode || cfg.Agent != "scott" || cfg.Location.Name != "home" {
			t.Fatalf("%s config = %#v", tc.runtime, cfg)
		}
	}
	for _, path := range []string{
		filepath.Join(managedRoot, "claude-code", "managed-settings.d", "50-witself.json"),
		filepath.Join(managedRoot, "codex", "requirements.toml"),
		filepath.Join(home, ".grok", "hooks", "witself.json"),
		filepath.Join(home, ".cursor", "hooks.json"),
		filepath.Join(home, ".cursor", "rules", cursorMemoryRoutingRuleFile),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected hook configuration %s: %v", path, err)
		}
	}
}

func TestCommaSeparatedUninstallRemovesEveryRuntimeBinding(t *testing.T) {
	home := t.TempDir()
	setInstallExecutableForTest(t)
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	t.Setenv("CURSOR_CONFIG_DIR", "")
	t.Setenv(fakeGenericRuntimeEnv, "")
	t.Setenv(fakeGenericLogEnv, filepath.Join(home, "generic-invocations.jsonl"))
	t.Setenv(fakeGenericStateEnv, filepath.Join(home, "generic-provider-state.json"))
	providerCommands := map[string]string{}
	for runtimeName, cliName := range map[string]string{
		transcriptcapture.RuntimeClaudeCode: "claude",
		transcriptcapture.RuntimeCodex:      "codex",
		transcriptcapture.RuntimeGrokBuild:  "grok",
		transcriptcapture.RuntimeCursor:     "cursor",
	} {
		providerCommands[runtimeName] = copyGenericProviderFixture(t, filepath.Join(home, cliName))
	}
	t.Setenv("CLAUDE_CLI_PATH", providerCommands[transcriptcapture.RuntimeClaudeCode])
	t.Setenv("CODEX_CLI_PATH", providerCommands[transcriptcapture.RuntimeCodex])
	t.Setenv("GROK_CLI_PATH", providerCommands[transcriptcapture.RuntimeGrokBuild])
	t.Setenv("CURSOR_CLI_PATH", providerCommands[transcriptcapture.RuntimeCursor])
	loc, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	for _, runtimeName := range []string{
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor,
	} {
		if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
			Runtime: runtimeName, CaptureMode: transcriptcapture.ModeRaw, HookMode: transcriptcapture.HookModeUser,
			Account: "default", Realm: "default", Agent: "scott",
			AgentID: "agent_1", AgentName: "scott", Location: loc,
		}); err != nil {
			t.Fatal(err)
		}
		legacy, err := transcriptcapture.LoadConfig(runtimeName)
		if err != nil {
			t.Fatal(err)
		}
		executable, err := currentExecutablePath()
		if err != nil {
			t.Fatal(err)
		}
		legacy, err = hydrateLegacyGenericProviderConfig(legacy, providerCommands[runtimeName], executable)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(legacy.RuntimeConfigRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		binding, err := genericMCPBindingFromConfig(legacy)
		if err != nil {
			t.Fatal(err)
		}
		if err := addGenericMCPUnchecked(providerCommands[runtimeName], legacy, binding); err != nil {
			t.Fatal(err)
		}
	}
	if code := uninstallCmd([]string{"claude,codex,grok,cursor"}); code != 0 {
		t.Fatalf("uninstall code = %d", code)
	}
	for _, runtimeName := range []string{
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor,
	} {
		path, err := transcriptcapture.ConfigPath(runtimeName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s binding remains: %v", runtimeName, err)
		}
	}
}

func TestManagedInstallAndUninstallAcrossRuntimes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		installName   string
		runtime       string
		cliEnv        string
		configRootEnv string
		configDir     string
		policyPath    func(string) string
		routingPath   func(string) string
		routingText   string
		removeRouting bool
	}{
		{
			name: "codex", installName: "codex", runtime: transcriptcapture.RuntimeCodex,
			cliEnv: "CODEX_CLI_PATH", configRootEnv: "CODEX_HOME", configDir: ".codex",
			policyPath:  func(root string) string { return filepath.Join(root, "codex", "requirements.toml") },
			routingPath: func(root string) string { return filepath.Join(root, "AGENTS.md") },
			routingText: codexMemoryRoutingInstructions,
		},
		{
			name: "claude", installName: "claude", runtime: transcriptcapture.RuntimeClaudeCode,
			cliEnv: "CLAUDE_CLI_PATH", configRootEnv: "CLAUDE_CONFIG_DIR", configDir: ".claude",
			policyPath: func(root string) string {
				return filepath.Join(root, "claude-code", "managed-settings.d", "50-witself.json")
			},
			routingPath:   func(root string) string { return filepath.Join(root, "rules", claudeMemoryRoutingRuleFile) },
			routingText:   runtimeNeutralMemoryRoutingInstructions,
			removeRouting: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setInstallExecutableForTest(t)
			witselfHome := filepath.Join(home, ".witself")
			runtimeRoot := filepath.Join(home, tc.configDir)
			managedRoot := filepath.Join(home, "managed")
			t.Setenv("HOME", home)
			t.Setenv("WITSELF_HOME", witselfHome)
			t.Setenv(tc.configRootEnv, runtimeRoot)
			t.Setenv(managedHooksTestRootEnv, managedRoot)

			logPath := filepath.Join(home, tc.name+"-args.log")
			t.Setenv("FAKE_CLI_LOG", logPath)
			t.Setenv(fakeGenericRuntimeEnv, tc.runtime)
			t.Setenv(fakeGenericLogEnv, filepath.Join(home, tc.name+"-invocations.jsonl"))
			t.Setenv(fakeGenericStateEnv, filepath.Join(home, tc.name+"-provider-state.json"))
			fakeCLI := copyGenericProviderFixture(t, filepath.Join(home, tc.name))
			t.Setenv(tc.cliEnv, fakeCLI)

			settingsPath := filepath.Join(runtimeRoot, "settings.json")
			if tc.runtime == transcriptcapture.RuntimeCodex {
				settingsPath = filepath.Join(runtimeRoot, "hooks.json")
			}
			if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
				t.Fatal(err)
			}
			unrelated := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"custom-check"}]}]}}`
			if err := os.WriteFile(settingsPath, []byte(unrelated), 0o600); err != nil {
				t.Fatal(err)
			}

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
				tc.installName, "--account", "default", "--realm", "default", "--agent", "scott",
				"--location", "home", "--capture", "raw", "--endpoint", srv.URL,
				"--token-file", tokenPath, "--managed-hooks",
			}); code != 0 {
				t.Fatalf("install code = %d", code)
			}
			cfg, err := transcriptcapture.LoadConfig(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.HookMode != transcriptcapture.HookModeManaged {
				t.Fatalf("hook mode = %q", cfg.HookMode)
			}
			policy, err := os.ReadFile(tc.policyPath(managedRoot))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(policy), "--account 'default' --realm 'default' --agent 'scott' --location 'home'") {
				t.Fatalf("managed hook does not pin verified binding:\n%s", policy)
			}
			raw, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "custom-check") || strings.Contains(string(raw), "transcript hook --runtime") {
				t.Fatalf("user settings = %s", raw)
			}
			routing, err := os.ReadFile(tc.routingPath(runtimeRoot))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(routing), tc.routingText) {
				t.Fatalf("managed memory routing = %s", routing)
			}

			if code := uninstallCmd([]string{tc.installName}); code != 0 {
				t.Fatalf("uninstall code = %d", code)
			}
			if _, err := os.Stat(tc.policyPath(managedRoot)); !os.IsNotExist(err) {
				t.Fatalf("managed policy still exists: %v", err)
			}
			configPath, err := transcriptcapture.ConfigPath(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Fatalf("integration config still exists: %v", err)
			}
			if _, err := os.Stat(tokenPath); err != nil {
				t.Fatalf("token was removed: %v", err)
			}
			if tc.removeRouting {
				if _, err := os.Stat(tc.routingPath(runtimeRoot)); !os.IsNotExist(err) {
					t.Fatalf("dedicated memory-routing rule still exists: %v", err)
				}
			} else {
				routing, err := os.ReadFile(tc.routingPath(runtimeRoot))
				if err != nil {
					t.Fatal(err)
				}
				if len(routing) != 0 {
					t.Fatalf("shared instruction file retained managed bytes: %q", routing)
				}
			}
		})
	}
}

func TestGlobalUserInstallAndUninstallAcrossNativeRuntimes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		installName   string
		runtime       string
		cliEnv        string
		configRootEnv string
		hookPath      func(string) string
		routingPath   func(string) string
		routingText   string
		removeRouting bool
	}{
		{
			name: "grok", installName: "grok", runtime: transcriptcapture.RuntimeGrokBuild,
			cliEnv: "GROK_CLI_PATH", configRootEnv: "GROK_HOME",
			hookPath:    func(root string) string { return filepath.Join(root, "hooks", "witself.json") },
			routingPath: func(root string) string { return filepath.Join(root, "AGENTS.md") },
			routingText: grokPortableMemoryRoutingInstructions,
		},
		{
			name: "cursor", installName: "cursor", runtime: transcriptcapture.RuntimeCursor,
			cliEnv:        "CURSOR_CLI_PATH",
			hookPath:      func(root string) string { return filepath.Join(root, "hooks.json") },
			routingPath:   func(root string) string { return filepath.Join(root, "rules", cursorMemoryRoutingRuleFile) },
			routingText:   cursorMemoryRoutingInstructions,
			removeRouting: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setInstallExecutableForTest(t)
			runtimeRoot := filepath.Join(home, "."+tc.name)
			t.Setenv("HOME", home)
			t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
			if tc.configRootEnv != "" {
				t.Setenv(tc.configRootEnv, runtimeRoot)
			}

			logPath := filepath.Join(home, tc.name+"-args.log")
			t.Setenv("FAKE_CLI_LOG", logPath)
			t.Setenv(fakeGenericRuntimeEnv, tc.runtime)
			t.Setenv(fakeGenericLogEnv, filepath.Join(home, tc.name+"-invocations.jsonl"))
			t.Setenv(fakeGenericStateEnv, filepath.Join(home, tc.name+"-provider-state.json"))
			fakeCLI := copyGenericProviderFixture(t, filepath.Join(home, tc.name))
			t.Setenv(tc.cliEnv, fakeCLI)

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
				tc.installName, "--account", "default", "--realm", "default", "--agent", "scott",
				"--location", "home", "--capture", "raw", "--endpoint", srv.URL, "--token-file", tokenPath,
			}); code != 0 {
				t.Fatalf("install code = %d", code)
			}
			cfg, err := transcriptcapture.LoadConfig(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.HookMode != transcriptcapture.HookModeUser || cfg.Agent != "scott" || cfg.Location.Name != "home" {
				t.Fatalf("config = %#v", cfg)
			}
			hooks, err := os.ReadFile(tc.hookPath(runtimeRoot))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(hooks), "--runtime "+tc.runtime) || !strings.Contains(string(hooks), "--agent 'scott' --location 'home'") {
				t.Fatalf("hooks = %s", hooks)
			}
			if tc.routingPath != nil {
				routing, err := os.ReadFile(tc.routingPath(runtimeRoot))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(routing), tc.routingText) {
					t.Fatalf("managed memory routing = %s", routing)
				}
			}
			if tc.runtime == transcriptcapture.RuntimeGrokBuild {
				log, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(log), "mcp add --scope user --env WITSELF_HOME=") || !strings.Contains(string(log), "mcp serve --runtime grok-build") {
					t.Fatalf("Grok CLI log = %s", log)
				}
			} else {
				mcpConfig, err := os.ReadFile(filepath.Join(runtimeRoot, "mcp.json"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(mcpConfig), `"witself"`) || !strings.Contains(string(mcpConfig), `"cursor"`) || !strings.Contains(string(mcpConfig), `"scott"`) {
					t.Fatalf("Cursor MCP config = %s", mcpConfig)
				}
				log, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(log), "mcp enable witself") {
					t.Fatalf("Cursor CLI log = %s", log)
				}
			}

			if code := uninstallCmd([]string{tc.installName}); code != 0 {
				t.Fatalf("uninstall code = %d", code)
			}
			if _, err := os.Stat(tc.hookPath(runtimeRoot)); !os.IsNotExist(err) {
				t.Fatalf("hook file still exists: %v", err)
			}
			configPath, err := transcriptcapture.ConfigPath(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Fatalf("integration config still exists: %v", err)
			}
			if _, err := os.Stat(tokenPath); err != nil {
				t.Fatalf("token was removed: %v", err)
			}
			if tc.routingPath != nil {
				if tc.removeRouting {
					if _, err := os.Stat(tc.routingPath(runtimeRoot)); !os.IsNotExist(err) {
						t.Fatalf("dedicated memory-routing rule still exists: %v", err)
					}
				} else {
					routing, err := os.ReadFile(tc.routingPath(runtimeRoot))
					if err != nil {
						t.Fatal(err)
					}
					if len(routing) != 0 {
						t.Fatalf("shared instruction file retained managed bytes: %q", routing)
					}
				}
			}
			if tc.runtime == transcriptcapture.RuntimeCursor {
				log, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(log), "mcp disable witself") {
					t.Fatalf("Cursor uninstall log = %s", log)
				}
			}
		})
	}
}

func TestCurrentExecutablePathRejectsGoRunBinary(t *testing.T) {
	t.Setenv(witselfExecutableTestEnv, "")
	original := os.Args[0]
	os.Args[0] = filepath.Join(t.TempDir(), "go-build123", "b001", "exe", "witself")
	t.Cleanup(func() { os.Args[0] = original })
	if _, err := currentExecutablePath(); err == nil || !strings.Contains(err.Error(), "temporary executable") {
		t.Fatalf("error = %v", err)
	}
}

func TestDetectRuntimeVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime string
		output  string
		stderr  string
		want    string
	}{
		{"codex", transcriptcapture.RuntimeCodex, "codex-cli 0.30.0", "", "0.30.0"},
		{"claude", transcriptcapture.RuntimeClaudeCode, "2.1.197 (Claude Code)", "", "2.1.197"},
		{"grok", transcriptcapture.RuntimeGrokBuild, "grok 0.2.93 (f00f96316d4b) [stable]", "", "0.2.93"},
		{"cursor", transcriptcapture.RuntimeCursor, "2026.07.16-899851b", "", "2026.07.16-899851b"},
		{"cursor diagnostic", transcriptcapture.RuntimeCursor, "2026.07.16-899851b", "[0716/234658.202288:ERROR:electron] failure", "2026.07.16-899851b"},
		{"copilot", transcriptcapture.RuntimeCopilot, "GitHub Copilot CLI 1.0.73.", "", "1.0.73"},
		{"fallback", transcriptcapture.RuntimeCodex, "development-build", "", "development-build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtime")
			script := "#!/bin/sh\n"
			if tc.stderr != "" {
				script += "printf '%s\\n' '" + tc.stderr + "' >&2\n"
			}
			script += "printf '%s\\n' '" + tc.output + "'\n"
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if got := detectRuntimeVersion(tc.runtime, path); got != tc.want {
				t.Fatalf("version = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectRuntimeVersionUsesCursorAgentBuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor-agent")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != --version ]; then exit 2; fi\n" +
		"printf '%s\\n' '2026.07.16-899851b'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := detectRuntimeVersion(transcriptcapture.RuntimeCursor, path); got != "2026.07.16-899851b" {
		t.Fatalf("Cursor Agent version = %q", got)
	}
}

func TestRuntimeCLICapabilityProbeAllowsColdGUIStartup(t *testing.T) {
	if runtimeCLICapabilityProbeTimeout < 20*time.Second {
		t.Fatalf("runtime CLI capability probe timeout = %s, want at least 20s", runtimeCLICapabilityProbeTimeout)
	}
}

func TestFindRuntimeCLICursorRequiresAgentMCPHelpContract(t *testing.T) {
	for _, tc := range []struct {
		name       string
		helpOutput string
		wantError  bool
	}{
		{name: "exact Cursor Agent capability", helpOutput: "Manage MCP servers\n"},
		{name: "desktop launcher false positive", helpOutput: "Cursor command line launcher\n", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "cursor")
			script := "#!/bin/sh\n" +
				"if [ \"$1 $2\" != \"mcp --help\" ]; then exit 9; fi\n" +
				"printf '%s' '" + tc.helpOutput + "'\n"
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("PATH", home)
			t.Setenv("CURSOR_CLI_PATH", path)
			got, err := findRuntimeCLI(transcriptcapture.RuntimeCursor)
			if tc.wantError {
				if err == nil {
					t.Fatalf("desktop launcher accepted as Cursor Agent: %s", got)
				}
				return
			}
			if err != nil || got != path {
				t.Fatalf("Cursor Agent selection = %q, %v; want %q", got, err, path)
			}
		})
	}
}

func setInstallExecutableForTest(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(witselfExecutableTestEnv, executable)
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
	if err := registerMCP(transcriptcapture.RuntimeClaudeCode, fakeCLI, "/usr/local/bin/witself", "account-under-test", "realm-under-test", "agent-under-test", "home"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	for _, want := range []string{
		"mcp remove --scope user witself",
		"mcp add --scope user --transport stdio witself -- /usr/local/bin/witself mcp serve --runtime claude-code --account account-under-test --realm realm-under-test --agent agent-under-test --location home",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("Claude registration log %q does not contain %q", log, want)
		}
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := registerMCP(transcriptcapture.RuntimeClaudeCode, fakeCLI, "/usr/local/bin/witself", "account-under-test", "realm-under-test", "agent-under-test", ""); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "--account account-under-test --realm realm-under-test --agent agent-under-test") || strings.Contains(string(raw), "--location") {
		t.Fatalf("unlabeled MCP registration = %q", raw)
	}
}

func TestCaptureAppendBatchesStayUnderServerJSONLimit(t *testing.T) {
	event := transcriptcapture.Event{
		ID:          "event-large",
		Runtime:     transcriptcapture.RuntimeCodex,
		HookEvent:   "PostToolUse",
		Kind:        "tool.result",
		Role:        "tool",
		CaptureMode: transcriptcapture.ModeRaw,
		SessionID:   "session-large",
		// Quotes and backslashes reproduce the important live failure mode:
		// a body that fits locally but grows past the server's 8 MiB decoder
		// limit after JSON escaping.
		Body: strings.Repeat("\"\\", 2*1024*1024),
	}
	entries := event.Entries()
	oldRequest, err := json.Marshal(map[string]any{"entries": entries})
	if err != nil {
		t.Fatal(err)
	}
	if len(oldRequest) <= 8*1024*1024 {
		t.Fatalf("regression fixture bytes = %d, want more than the server's 8 MiB limit", len(oldRequest))
	}
	batches, err := captureAppendBatches(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) < 2 {
		t.Fatalf("batches = %d, want size-based split", len(batches))
	}

	flattened := make([]client.AppendTranscriptEntryInput, 0, len(entries))
	for i, batch := range batches {
		if len(batch) == 0 || len(batch) > maxCaptureAppendBatchEntries {
			t.Fatalf("batch %d entries = %d", i, len(batch))
		}
		raw, err := json.Marshal(map[string]any{"entries": batch})
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > maxCaptureAppendRequestBytes {
			t.Fatalf("batch %d bytes = %d, limit = %d", i, len(raw), maxCaptureAppendRequestBytes)
		}
		flattened = append(flattened, batch...)
	}
	if len(flattened) != len(entries) {
		t.Fatalf("flattened entries = %d, want %d", len(flattened), len(entries))
	}
	for i, input := range flattened {
		if input.ExternalID != entries[i].ExternalID || input.Body != entries[i].Body ||
			string(input.Payload) != string(entries[i].Payload) {
			t.Fatalf("entry %d changed or reordered", i)
		}
	}
}

func TestCaptureAppendBatchesRetainCountLimitAndOrder(t *testing.T) {
	entries := make([]transcriptcapture.Entry, maxCaptureAppendBatchEntries+1)
	for i := range entries {
		entries[i] = transcriptcapture.Entry{
			ExternalID: fmt.Sprintf("event:%03d", i),
			Role:       "system",
			Body:       "small",
		}
	}
	batches, err := captureAppendBatches(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[0]) != maxCaptureAppendBatchEntries || len(batches[1]) != 1 {
		t.Fatalf("batch sizes = %d/%d", len(batches[0]), len(batches[1]))
	}
	for i, input := range append(batches[0], batches[1]...) {
		if input.ExternalID != entries[i].ExternalID {
			t.Fatalf("entry %d external id = %q, want %q", i, input.ExternalID, entries[i].ExternalID)
		}
	}
}

func TestLatestCaptureActivitySelectionsUseFirstLatestObservation(t *testing.T) {
	base := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	event := func(id, session string, occurredAt time.Time) transcriptcapture.PendingEvent {
		return transcriptcapture.PendingEvent{Path: id, Event: transcriptcapture.Event{
			ID: id, Runtime: transcriptcapture.RuntimeCodex,
			Location: transcriptcapture.Location{ID: "loc_1"}, SessionID: session,
			OccurredAt: occurredAt,
		}}
	}
	ready := []transcriptcapture.PendingEvent{
		event("a-old", "session-a", base),
		event("b-first-latest", "session-b", base.Add(2*time.Second)),
		event("a-latest", "session-a", base.Add(2*time.Second)),
		event("b-equal-later", "session-b", base.Add(2*time.Second)),
	}
	selections := latestCaptureActivitySelections(ready)
	if len(selections) != 2 || selections[0].Event.ID != "b-first-latest" || selections[1].Event.ID != "a-latest" {
		t.Fatalf("latest activity selections = %#v", selections)
	}
}

func TestCapturePendingAppendBatchesNeverSplitChunkedEventAtSharedBoundary(t *testing.T) {
	pending := make([]transcriptcapture.PendingEvent, 0, maxCaptureAppendBatchEntries+1)
	for i := 0; i < maxCaptureAppendBatchEntries-1; i++ {
		id := fmt.Sprintf("small-%03d", i)
		pending = append(pending, transcriptcapture.PendingEvent{Path: id + ".json", Event: transcriptcapture.Event{
			ID: id, Runtime: transcriptcapture.RuntimeCodex, SessionID: "session-1", Body: "small",
		}})
	}
	spanningPath := "spanning.json"
	pending = append(pending, transcriptcapture.PendingEvent{Path: spanningPath, Event: transcriptcapture.Event{
		ID: "spanning", Runtime: transcriptcapture.RuntimeCodex, SessionID: "session-1",
		Body: strings.Repeat("x", 61*1024),
	}})
	pending = append(pending, transcriptcapture.PendingEvent{Path: "trailing.json", Event: transcriptcapture.Event{
		ID: "trailing", Runtime: transcriptcapture.RuntimeCodex, SessionID: "session-1", Body: "last",
	}})

	batches, remaining, err := capturePendingAppendBatches(pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[0].inputs) != maxCaptureAppendBatchEntries-1 || len(batches[1].inputs) != 3 {
		t.Fatalf("owned batch sizes = %d / %d", len(batches[0].inputs), len(batches[1].inputs))
	}
	containsPath := func(batch pendingCaptureAppendBatch, path string) bool {
		for _, pendingEvent := range batch.events {
			if pendingEvent.Path == path {
				return true
			}
		}
		return false
	}
	if remaining[spanningPath] != 1 || containsPath(batches[0], spanningPath) ||
		!containsPath(batches[1], spanningPath) {
		t.Fatalf("spanning file ownership = remaining %d / first %v / second %v",
			remaining[spanningPath], batches[0].events, batches[1].events)
	}
	if remaining["trailing.json"] != 1 || containsPath(batches[0], "trailing.json") ||
		!containsPath(batches[1], "trailing.json") {
		t.Fatalf("trailing file ownership = remaining %d / first %v / second %v",
			remaining["trailing.json"], batches[0].events, batches[1].events)
	}
	flattened := append(append([]client.AppendTranscriptEntryInput{}, batches[0].inputs...), batches[1].inputs...)
	if flattened[maxCaptureAppendBatchEntries-1].ExternalID != "spanning:0" ||
		flattened[maxCaptureAppendBatchEntries].ExternalID != "spanning:1" ||
		flattened[maxCaptureAppendBatchEntries+1].ExternalID != "trailing:0" {
		t.Fatalf("coalesced order or idempotency keys changed around boundary: %#v", flattened[maxCaptureAppendBatchEntries-1:])
	}
}

func TestCapturePendingAppendBatchesPreserveOversizedEventBatches(t *testing.T) {
	oversized := transcriptcapture.Event{
		ID: "oversized", Runtime: transcriptcapture.RuntimeCodex, SessionID: "session-1",
		HookEvent: "SessionEnd", Kind: "session.ended", Role: "system",
	}
	oversized.RecoveredMessages = make([]transcriptcapture.RecoveredMessage, maxCaptureAppendBatchEntries)
	for index := range oversized.RecoveredMessages {
		oversized.RecoveredMessages[index] = transcriptcapture.RecoveredMessage{
			ID: fmt.Sprintf("recovered-%03d", index), Kind: "message.assistant", Role: "assistant", Body: "small",
		}
	}
	wantOversized, err := captureAppendBatches(oversized.Entries())
	if err != nil {
		t.Fatal(err)
	}
	if len(wantOversized) != 2 {
		t.Fatalf("old per-event batches = %d, want 2", len(wantOversized))
	}
	pending := []transcriptcapture.PendingEvent{
		{Path: "before.json", Event: transcriptcapture.Event{ID: "before", Runtime: transcriptcapture.RuntimeCodex, SessionID: "session-1"}},
		{Path: "oversized.json", Event: oversized},
		{Path: "after.json", Event: transcriptcapture.Event{ID: "after", Runtime: transcriptcapture.RuntimeCodex, SessionID: "session-1"}},
	}
	batches, remaining, err := capturePendingAppendBatches(pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != len(wantOversized)+2 || remaining["oversized.json"] != len(wantOversized) {
		t.Fatalf("coalesced batch count/ownership = %d/%d", len(batches), remaining["oversized.json"])
	}
	for index, want := range wantOversized {
		gotBatch := batches[index+1]
		if len(gotBatch.events) != 1 || gotBatch.events[0].Path != "oversized.json" {
			t.Fatalf("oversized batch %d ownership = %#v", index, gotBatch.events)
		}
		gotJSON, gotErr := json.Marshal(gotBatch.inputs)
		wantJSON, wantErr := json.Marshal(want)
		if gotErr != nil || wantErr != nil {
			t.Fatalf("marshal old/new batch %d = %v/%v", index, gotErr, wantErr)
		}
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("oversized batch %d changed from the old per-event path", index)
		}
	}
}

func TestCapturePendingAppendBatchesAccountForInterEventSeparators(t *testing.T) {
	const envelopeBytes = len(`{"entries":[]}`)
	makeEvent := func(index, quoteCount int) transcriptcapture.PendingEvent {
		id := fmt.Sprintf("event-%03d", index)
		return transcriptcapture.PendingEvent{Path: id + ".json", Event: transcriptcapture.Event{
			ID: id, Runtime: transcriptcapture.RuntimeCodex, SessionID: "session-1",
			Body: strings.Repeat("\"", quoteCount),
		}}
	}
	contribution := func(pendingEvent transcriptcapture.PendingEvent) int {
		t.Helper()
		batches, err := captureAppendBatches(pendingEvent.Event.Entries())
		if err != nil {
			t.Fatal(err)
		}
		if len(batches) != 1 || len(batches[0]) != 1 {
			t.Fatalf("fixture event batches = %#v", batches)
		}
		requestBytes, err := captureAppendRequestBytes(batches[0])
		if err != nil {
			t.Fatal(err)
		}
		return requestBytes - envelopeBytes
	}
	findEvent := func(index, limit int) (transcriptcapture.PendingEvent, int) {
		t.Helper()
		low, high := 0, 60*1024
		var selected transcriptcapture.PendingEvent
		selectedBytes := -1
		for low <= high {
			middle := low + (high-low)/2
			candidate := makeEvent(index, middle)
			candidateBytes := contribution(candidate)
			if candidateBytes <= limit {
				selected, selectedBytes = candidate, candidateBytes
				low = middle + 1
			} else {
				high = middle - 1
			}
		}
		if selectedBytes < 0 {
			t.Fatalf("no fixture event fits %d bytes", limit)
		}
		return selected, selectedBytes
	}

	maxEventBytes := contribution(makeEvent(0, 60*1024))
	minEventBytes := contribution(makeEvent(0, 0))
	pending := make([]transcriptcapture.PendingEvent, 0, maxCaptureAppendBatchEntries)
	accountedBytes := envelopeBytes
	for maxCaptureAppendRequestBytes-accountedBytes > 2*maxEventBytes {
		event := makeEvent(len(pending), 60*1024)
		pending = append(pending, event)
		accountedBytes += contribution(event)
	}
	remaining := maxCaptureAppendRequestBytes - accountedBytes
	firstLimit := min(maxEventBytes, remaining-minEventBytes)
	first, firstBytes := findEvent(len(pending), firstLimit)
	pending = append(pending, first)
	accountedBytes += firstBytes
	second, secondBytes := findEvent(len(pending), maxCaptureAppendRequestBytes-accountedBytes)
	pending = append(pending, second)
	accountedBytes += secondBytes
	if len(pending) > maxCaptureAppendBatchEntries || accountedBytes > maxCaptureAppendRequestBytes ||
		accountedBytes+len(pending)-1 <= maxCaptureAppendRequestBytes {
		t.Fatalf("separator fixture events/accounted/encoded = %d/%d/%d",
			len(pending), accountedBytes, accountedBytes+len(pending)-1)
	}

	batches, _, err := capturePendingAppendBatches(pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) < 2 {
		t.Fatalf("separator-aware batching produced %d batch(es)", len(batches))
	}
	for index, batch := range batches {
		raw, err := json.Marshal(struct {
			Entries []client.AppendTranscriptEntryInput `json:"entries"`
		}{Entries: batch.inputs})
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > maxCaptureAppendRequestBytes {
			t.Fatalf("batch %d bytes = %d, limit = %d", index, len(raw), maxCaptureAppendRequestBytes)
		}
	}
}

func TestCaptureOutboxFlushesWholeVisibleTurn(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	var mu sync.Mutex
	var appended []map[string]any
	var activityBodies []map[string]any
	routeMisses := 0
	createRequests := 0
	appendRequests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/self/activity":
			routeMisses++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode activity: %v", err)
			}
			activityBodies = append(activityBodies, body)
			http.NotFound(w, r) // pre-activity server during a rolling upgrade
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			createRequests++
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			appendRequests++
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
	if routeMisses != 1 || len(activityBodies) != 1 || activityBodies[0]["event"] != "Stop" {
		t.Fatalf("latest activity compatibility probe = %d / %#v, want one Stop", routeMisses, activityBodies)
	}
	if createRequests != 1 || appendRequests != 1 {
		t.Fatalf("transcript create/append requests = %d/%d, want 1/1", createRequests, appendRequests)
	}
	if appended[1]["body"] != "the full original prompt" || appended[2]["body"] != "the full final response" {
		t.Fatalf("visible turn = %#v", appended)
	}
	if appended[2]["reply_to_external_id"] != appended[1]["external_id"] {
		t.Fatalf("reply link = %#v", appended[2])
	}
}

func TestTranscriptFlushQuarantinesEphemeralCodexBacklog(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	var appendedIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/self/activity":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			var body struct {
				Entries []struct {
					ExternalID string `json:"external_id"`
				} `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode append: %v", err)
				http.Error(w, "bad append", http.StatusBadRequest)
				return
			}
			for _, entry := range body.Entries {
				appendedIDs = append(appendedIDs, entry.ExternalID)
			}
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	configureCaptureFlushTest(t, transcriptcapture.RuntimeCodex, srv.URL)

	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	base := transcriptcapture.Event{
		SchemaVersion: transcriptcapture.SchemaVersion, Runtime: transcriptcapture.RuntimeCodex,
		CaptureMode: cfg.CaptureMode, Account: cfg.Account, Realm: cfg.Realm,
		Agent: cfg.Agent, AgentID: cfg.AgentID, AgentName: cfg.AgentName, Location: cfg.Location,
		RunID: "run_backlog", HookEvent: "SessionStart", NativeHookEvent: "SessionStart",
		Kind: "session.started", Role: "system", CWD: "/src/witself",
	}
	events := []transcriptcapture.Event{base, base, base, base}
	events[0].ID, events[0].SessionID, events[0].SourceTranscriptPath = "evt_persisted_1", "persisted-session-1", "/tmp/.codex/sessions/persisted-1/rollout.jsonl"
	events[1].ID, events[1].SessionID, events[1].SourceTranscriptPath = "evt_ephemeral_1", "internal-session-canary-1", ""
	events[1].Body = "ephemeral-backlog-content-canary-1"
	events[2].ID, events[2].SessionID, events[2].SourceTranscriptPath = "evt_persisted_2", "persisted-session-2", "/tmp/.codex/sessions/persisted-2/rollout.jsonl"
	events[3].ID, events[3].SessionID, events[3].SourceTranscriptPath = "evt_ephemeral_2", "internal-session-canary-2", ""
	events[3].Body = "ephemeral-backlog-content-canary-2"
	outbox := filepath.Join(home, "capture", "outbox", transcriptcapture.RuntimeCodex)
	if err := os.MkdirAll(outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	wantQuarantined := make(map[string][]byte)
	for index, event := range events {
		event.OccurredAt = time.Date(2026, 7, 29, 10, 0, index, 0, time.UTC)
		raw, err := json.MarshalIndent(event, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, '\n')
		basename := fmt.Sprintf("%020d-%s.json", index+1, event.ID)
		if err := os.WriteFile(filepath.Join(outbox, basename), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if event.SourceTranscriptPath == "" {
			wantQuarantined[basename] = raw
		}
	}

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeCodex})
	})
	if code != 0 || stdout != "" {
		t.Fatalf("Codex quarantine flush = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "flushed 2 codex transcript event(s); quarantined 2 ephemeral event(s)") ||
		!strings.Contains(stderr, "skipped ephemeral sessions: 2 session(s), 2 event(s) (not captured; see ~/.witself/capture/skipped/codex/)") {
		t.Fatalf("Codex quarantine flush stderr = %q", stderr)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after quarantine flush = %#v", pending)
	}
	quarantineDir := filepath.Join(home, "capture", "quarantine", transcriptcapture.RuntimeCodex)
	quarantinedPaths, err := filepath.Glob(filepath.Join(quarantineDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantinedPaths) != len(wantQuarantined) {
		t.Fatalf("quarantined paths = %v", quarantinedPaths)
	}
	for _, path := range quarantinedPaths {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want, exists := wantQuarantined[filepath.Base(path)]
		if !exists || !slices.Equal(got, want) {
			t.Fatalf("quarantined %s bytes changed or basename was not preserved", path)
		}
	}
	if len(appendedIDs) != 2 || !strings.HasPrefix(appendedIDs[0], "evt_persisted_") ||
		!strings.HasPrefix(appendedIDs[1], "evt_persisted_") {
		t.Fatalf("uploaded event IDs = %v", appendedIDs)
	}
	skipped, err := transcriptcapture.SkippedSessions(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 2 || skipped[0].Events != 1 || skipped[1].Events != 1 {
		t.Fatalf("skipped backlog inventory = %#v", skipped)
	}
}

func TestTranscriptFlushQuarantinesAllEphemeralCodexBacklogWithoutHTTP(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer srv.Close()
	configureCaptureFlushTest(t, transcriptcapture.RuntimeCodex, srv.URL)

	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	base := transcriptcapture.Event{
		SchemaVersion: transcriptcapture.SchemaVersion, Runtime: transcriptcapture.RuntimeCodex,
		CaptureMode: cfg.CaptureMode, Account: cfg.Account, Realm: cfg.Realm,
		Agent: cfg.Agent, AgentID: cfg.AgentID, AgentName: cfg.AgentName, Location: cfg.Location,
		RunID: "run_all_ephemeral", HookEvent: "SessionStart", NativeHookEvent: "SessionStart",
		Kind: "session.started", Role: "system", CWD: "/src/witself",
	}
	wantQuarantined := make(map[string][]byte, 2)
	sourcePaths := make([]string, 0, 2)
	for index := range 2 {
		event := base
		event.ID = fmt.Sprintf("evt_ephemeral_%d", index+1)
		event.SessionID = fmt.Sprintf("ephemeral-session-%d", index+1)
		event.OccurredAt = time.Date(2026, 9, 2, 10, 0, index, 0, time.UTC)
		basename := fmt.Sprintf("%020d-%s.json", index+1, event.ID)
		path, raw, err := writeCapturePendingFixture(home, transcriptcapture.RuntimeCodex, basename, event)
		if err != nil {
			t.Fatal(err)
		}
		sourcePaths = append(sourcePaths, path)
		wantQuarantined[basename] = raw
	}

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeCodex})
	})
	wantStderr := "flushed 0 codex transcript event(s); quarantined 2 ephemeral event(s)\n" +
		"skipped ephemeral sessions: 2 session(s), 2 event(s) (not captured; see ~/.witself/capture/skipped/codex/)\n"
	if code != 0 || stdout != "" || stderr != wantStderr {
		t.Fatalf("all-ephemeral flush = code %d stdout %q stderr %q, want stderr %q", code, stdout, stderr, wantStderr)
	}
	if requests != 0 {
		t.Fatalf("all-ephemeral flush made %d HTTP request(s)", requests)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after all-ephemeral flush = %#v / %v", pending, err)
	}
	for _, source := range sourcePaths {
		if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("quarantined source %s still exists: %v", source, err)
		}
	}
	quarantineDir := filepath.Join(home, "capture", "quarantine", transcriptcapture.RuntimeCodex)
	quarantinedPaths, err := filepath.Glob(filepath.Join(quarantineDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantinedPaths) != len(wantQuarantined) {
		t.Fatalf("quarantined paths = %v", quarantinedPaths)
	}
	for _, path := range quarantinedPaths {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want, exists := wantQuarantined[filepath.Base(path)]
		if !exists || !slices.Equal(got, want) {
			t.Fatalf("quarantined %s bytes changed or basename was not preserved", path)
		}
	}
}

func TestTranscriptFlushReportsQuarantineBeforeLoadConfigError(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	base := transcriptcapture.Event{
		SchemaVersion: transcriptcapture.SchemaVersion, Runtime: transcriptcapture.RuntimeCodex,
		CaptureMode: transcriptcapture.ModeRaw, Account: "default", Realm: "default",
		Agent: "scott", AgentID: "agent_1", AgentName: "scott",
		RunID: "run_missing_config", HookEvent: "SessionStart", NativeHookEvent: "SessionStart",
		Kind: "session.started", Role: "system", CWD: "/src/witself",
	}
	persisted := base
	persisted.ID = "evt_persisted_missing_config"
	persisted.SessionID = "persisted-session-missing-config"
	persisted.SourceTranscriptPath = "/tmp/.codex/sessions/persisted-missing-config/rollout.jsonl"
	persisted.OccurredAt = time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	if _, _, err := writeCapturePendingFixture(
		home, transcriptcapture.RuntimeCodex,
		"00000000000000000001-evt_persisted_missing_config.json", persisted,
	); err != nil {
		t.Fatal(err)
	}
	ephemeral := base
	ephemeral.ID = "evt_ephemeral_missing_config"
	ephemeral.SessionID = "ephemeral-session-missing-config"
	ephemeral.OccurredAt = time.Date(2026, 9, 2, 10, 0, 1, 0, time.UTC)
	if _, _, err := writeCapturePendingFixture(
		home, transcriptcapture.RuntimeCodex,
		"00000000000000000002-evt_ephemeral_missing_config.json", ephemeral,
	); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeCodex})
	})
	if code != 1 || stdout != "" ||
		!strings.Contains(stderr, "flushed 0 codex transcript event(s); quarantined 1 ephemeral event(s)\n") ||
		!strings.Contains(stderr, "skipped ephemeral sessions: 1 session(s), 1 event(s) (not captured; see ~/.witself/capture/skipped/codex/)\n") {
		t.Fatalf("missing-config quarantine flush = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || len(pending) != 1 || pending[0].Event.ID != persisted.ID {
		t.Fatalf("pending after missing-config quarantine = %#v / %v", pending, err)
	}
}

func TestTranscriptFlushQuarantineDoesNotAffectClaudeCodeMissingPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	appended := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/self/activity":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_claude","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_claude/entries:batch":
			appended++
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, srv.URL)
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(
		`{"session_id":"claude-session","hook_event_name":"SessionStart","cwd":"/src/witself"}`,
	)); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode})
	})
	if code != 0 || stdout != "" || appended != 1 {
		t.Fatalf("Claude flush = code %d stdout %q stderr %q appended %d", code, stdout, stderr, appended)
	}
	if strings.Contains(stderr, "quarantined") || strings.Contains(stderr, "skipped ephemeral sessions") {
		t.Fatalf("Claude missing-path event was treated as ephemeral: %q", stderr)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil || len(pending) != 0 {
		t.Fatalf("Claude pending after flush = %#v / %v", pending, err)
	}
	quarantined, err := filepath.Glob(filepath.Join(home, "capture", "quarantine", transcriptcapture.RuntimeClaudeCode, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantined) != 0 {
		t.Fatalf("Claude quarantine files = %v", quarantined)
	}
	skipped, err := transcriptcapture.SkippedSessions(transcriptcapture.RuntimeClaudeCode)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("Claude skipped inventory = %#v / %v", skipped, err)
	}
}

func TestTranscriptFlushQuarantineFailureStaysPending(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	configureCaptureFlushTest(t, transcriptcapture.RuntimeCodex, "http://127.0.0.1:1")
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	event := transcriptcapture.Event{
		SchemaVersion: transcriptcapture.SchemaVersion, ID: "evt_ephemeral_failed", Runtime: transcriptcapture.RuntimeCodex,
		CaptureMode: cfg.CaptureMode, Account: cfg.Account, AccountID: cfg.AccountID,
		Realm: cfg.Realm, RealmID: cfg.RealmID, Agent: cfg.Agent, AgentID: cfg.AgentID,
		AgentName: cfg.AgentName, Location: cfg.Location, SessionID: "ephemeral-failed", RunID: "run_failed",
		HookEvent: "SessionStart", NativeHookEvent: "SessionStart", Kind: "session.started", Role: "system",
		OccurredAt: time.Now().UTC(),
	}
	raw, err := json.MarshalIndent(event, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	basename := "00000000000000000001-" + event.ID + ".json"
	outbox := filepath.Join(home, "capture", "outbox", transcriptcapture.RuntimeCodex)
	if err := os.MkdirAll(outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(outbox, basename)
	if err := os.WriteFile(source, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(home, "capture", "quarantine", transcriptcapture.RuntimeCodex)
	if err := os.MkdirAll(quarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	const collisionCanary = "existing-quarantine-file-canary\n"
	destination := filepath.Join(quarantine, basename)
	if err := os.WriteFile(destination, []byte(collisionCanary), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeCodex})
	})
	if code != 1 || stdout != "" || strings.Count(stderr, "destination already exists") != 1 ||
		!strings.Contains(stderr, "flushed 0 codex transcript event(s); deferred 1 incomplete or mismatched event(s)") ||
		strings.Contains(stderr, "connect capture agent") || strings.Contains(stderr, "quarantined 1 ephemeral event(s)") {
		t.Fatalf("failed-quarantine flush = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	gotSource, err := os.ReadFile(source)
	if err != nil || !slices.Equal(gotSource, raw) {
		t.Fatalf("failed-quarantine source = %q / %v", gotSource, err)
	}
	gotDestination, err := os.ReadFile(destination)
	if err != nil || string(gotDestination) != collisionCanary {
		t.Fatalf("failed-quarantine destination = %q / %v", gotDestination, err)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || len(pending) != 1 || pending[0].Path != source {
		t.Fatalf("failed-quarantine pending = %#v / %v", pending, err)
	}
	skipped, err := transcriptcapture.SkippedSessions(transcriptcapture.RuntimeCodex)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("failed-quarantine skipped inventory = %#v / %v", skipped, err)
	}
}

func TestCaptureFlushBadRequestIsolatesRejectedMiddleEvent(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	var rejectedEventID string
	var requests [][]string
	touches := 0
	creates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/self/activity":
			touches++
			_, _ = w.Write([]byte(`{"activity":{"last_activity_at":"2026-09-01T12:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			creates++
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			var body struct {
				Entries []struct {
					ExternalID string `json:"external_id"`
				} `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode append: %v", err)
				http.Error(w, "bad append", http.StatusBadRequest)
				return
			}
			ids := make([]string, len(body.Entries))
			rejected := false
			for index, entry := range body.Entries {
				ids[index] = entry.ExternalID
				if strings.HasPrefix(entry.ExternalID, rejectedEventID+":") {
					rejected = true
				}
			}
			requests = append(requests, ids)
			if rejected {
				http.Error(w, "poison capture event", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, srv.URL)

	events := make([]transcriptcapture.Event, 3)
	for index := range events {
		event, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(
			`{"session_id":"session-1","hook_event_name":"SessionStart","cwd":"/src/witself"}`,
		))
		if err != nil {
			t.Fatal(err)
		}
		events[index] = event
	}
	rejectedEventID = events[1].ID

	if code := transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode}); code != 1 {
		t.Fatalf("isolating flush code = %d, want 1", code)
	}
	assertOnlyCaptureEventPending(t, transcriptcapture.RuntimeClaudeCode, rejectedEventID)
	wantRequests := [][]string{
		{events[0].ID + ":0", events[1].ID + ":0", events[2].ID + ":0"},
		{events[0].ID + ":0"},
		{events[1].ID + ":0"},
		{events[2].ID + ":0"},
	}
	if len(requests) != len(wantRequests) {
		t.Fatalf("append requests = %#v, want %#v", requests, wantRequests)
	}
	for index := range wantRequests {
		if !slices.Equal(requests[index], wantRequests[index]) {
			t.Fatalf("append request %d = %v, want %v", index, requests[index], wantRequests[index])
		}
	}
	if touches != 1 || creates != 1 {
		t.Fatalf("touch/create attempts = %d/%d, want 1/1", touches, creates)
	}

	if code := transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode}); code != 1 {
		t.Fatalf("poison retry flush code = %d, want 1", code)
	}
	assertOnlyCaptureEventPending(t, transcriptcapture.RuntimeClaudeCode, rejectedEventID)
	if len(requests) != 5 || !slices.Equal(requests[4], []string{rejectedEventID + ":0"}) {
		t.Fatalf("poison retry requests = %#v", requests)
	}
}

func TestCaptureFlushRejectedFirstEventDoesNotSkipLaterSameTranscriptEvents(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	var rejectedEventID string
	var requests [][]string
	touches := 0
	creates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/self/activity":
			touches++
			_, _ = w.Write([]byte(`{"activity":{"last_activity_at":"2026-09-01T12:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			creates++
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			var body struct {
				Entries []struct {
					ExternalID string `json:"external_id"`
				} `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode append: %v", err)
				http.Error(w, "bad append", http.StatusBadRequest)
				return
			}
			ids := make([]string, len(body.Entries))
			rejected := false
			for index, entry := range body.Entries {
				ids[index] = entry.ExternalID
				if strings.HasPrefix(entry.ExternalID, rejectedEventID+":") {
					rejected = true
				}
			}
			requests = append(requests, ids)
			if rejected {
				http.Error(w, "poison first event", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, srv.URL)

	first, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(
		`{"session_id":"session-a","hook_event_name":"SessionStart","cwd":"/src/a"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	separator, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(
		`{"session_id":"session-b","hook_event_name":"SessionStart","cwd":"/src/b"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	second, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(
		`{"session_id":"session-a","hook_event_name":"SessionStart","cwd":"/src/a"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	third, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(
		`{"session_id":"session-a","hook_event_name":"SessionStart","cwd":"/src/a"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	rejectedEventID = first.ID

	if code := transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode}); code != 1 {
		t.Fatalf("first-poison flush code = %d, want 1", code)
	}
	assertOnlyCaptureEventPending(t, transcriptcapture.RuntimeClaudeCode, first.ID)
	wantRequests := [][]string{
		{first.ID + ":0"},
		{separator.ID + ":0"},
		{second.ID + ":0", third.ID + ":0"},
	}
	if len(requests) != len(wantRequests) {
		t.Fatalf("append requests = %#v, want %#v", requests, wantRequests)
	}
	for index := range wantRequests {
		if !slices.Equal(requests[index], wantRequests[index]) {
			t.Fatalf("append request %d = %v, want %v", index, requests[index], wantRequests[index])
		}
	}
	if touches != 2 || creates != 2 {
		t.Fatalf("touch/create attempts = %d/%d, want 2/2", touches, creates)
	}
}

func TestCaptureFlushRejectedOversizedEventSkipsItsLaterBatchesButContinues(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	var requests [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/self/activity":
			_, _ = w.Write([]byte(`{"activity":{"last_activity_at":"2026-09-01T12:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			var body struct {
				Entries []struct {
					ExternalID string `json:"external_id"`
				} `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode append: %v", err)
				http.Error(w, "bad append", http.StatusBadRequest)
				return
			}
			ids := make([]string, len(body.Entries))
			rejected := false
			for index, entry := range body.Entries {
				ids[index] = entry.ExternalID
				if strings.HasPrefix(entry.ExternalID, "recovered-000:") {
					rejected = true
				}
			}
			requests = append(requests, ids)
			if rejected {
				http.Error(w, "poison oversized event", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, srv.URL)

	oversized, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(
		`{"session_id":"session-1","hook_event_name":"SessionStart","cwd":"/src/witself"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	oversized.RecoveredMessages = make([]transcriptcapture.RecoveredMessage, maxCaptureAppendBatchEntries)
	for index := range oversized.RecoveredMessages {
		oversized.RecoveredMessages[index] = transcriptcapture.RecoveredMessage{
			ID: fmt.Sprintf("recovered-%03d", index), Kind: "message.assistant", Role: "assistant", Body: "small",
		}
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil || len(pending) != 1 {
		t.Fatalf("oversized pending event = %d / %v", len(pending), err)
	}
	raw, err := json.Marshal(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending[0].Path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	following, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(
		`{"session_id":"session-1","hook_event_name":"SessionStart","cwd":"/src/witself"}`,
	))
	if err != nil {
		t.Fatal(err)
	}

	if code := transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode}); code != 1 {
		t.Fatalf("oversized-poison flush code = %d, want 1", code)
	}
	assertOnlyCaptureEventPending(t, transcriptcapture.RuntimeClaudeCode, oversized.ID)
	if len(requests) != 2 || len(requests[0]) != maxCaptureAppendBatchEntries ||
		!slices.Equal(requests[1], []string{following.ID + ":0"}) {
		t.Fatalf("oversized rejection requests = %#v", requests)
	}
	for _, request := range requests {
		if slices.Contains(request, oversized.ID+":0") {
			t.Fatalf("later oversized sub-batch was uploaded: %#v", requests)
		}
	}
}

func TestCaptureFlushSlicesSameTranscriptGroupsAt256Events(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	var batchSizes []int
	touches := 0
	creates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/self/activity":
			touches++
			_, _ = w.Write([]byte(`{"activity":{"last_activity_at":"2026-09-01T12:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			creates++
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			var body struct {
				Entries []json.RawMessage `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode append: %v", err)
				http.Error(w, "bad append", http.StatusBadRequest)
				return
			}
			batchSizes = append(batchSizes, len(body.Entries))
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	configureCaptureFlushTest(t, transcriptcapture.RuntimeClaudeCode, srv.URL)

	for range maxCaptureFlushSliceEvents + 1 {
		if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeClaudeCode, []byte(
			`{"session_id":"session-1","hook_event_name":"SessionStart","cwd":"/src/witself"}`,
		)); err != nil {
			t.Fatal(err)
		}
	}
	if code := transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeClaudeCode}); code != 0 {
		t.Fatalf("sliced flush code = %d", code)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 || !slices.Equal(batchSizes, []int{100, 100, 56, 1}) {
		t.Fatalf("pending/batch sizes = %d/%v", len(pending), batchSizes)
	}
	if touches != 1 || creates != 1 {
		t.Fatalf("touch/create attempts = %d/%d, want 1/1", touches, creates)
	}
}

func configureCaptureFlushTest(t *testing.T, runtime, endpoint string) {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: runtime, CaptureMode: transcriptcapture.ModeRaw,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: location,
		Endpoint: endpoint, TokenFile: tokenPath,
	}); err != nil {
		t.Fatal(err)
	}
}

func writeCapturePendingFixture(
	home, runtime, basename string,
	event transcriptcapture.Event,
) (string, []byte, error) {
	raw, err := json.MarshalIndent(event, "", "  ")
	if err != nil {
		return "", nil, err
	}
	raw = append(raw, '\n')
	outbox := filepath.Join(home, "capture", "outbox", runtime)
	if err := os.MkdirAll(outbox, 0o700); err != nil {
		return "", nil, err
	}
	path := filepath.Join(outbox, basename)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", nil, err
	}
	return path, raw, nil
}

func assertOnlyCaptureEventPending(t *testing.T, runtime, eventID string) {
	t.Helper()
	pending, err := transcriptcapture.Pending(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Event.ID != eventID {
		t.Fatalf("pending capture events = %#v, want only %q", pending, eventID)
	}
}

func waitForCaptureFlushLock(
	t *testing.T,
	lockPath string,
	done <-chan int,
	fixturePollInterval time.Duration,
) {
	t.Helper()
	poll := time.NewTicker(fixturePollInterval / 5)
	defer poll.Stop()
	deadline := time.NewTimer(20 * fixturePollInterval)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(lockPath); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect transcript flush lock: %v", err)
		}
		select {
		case code := <-done:
			t.Fatalf("transcript flush returned %d before acquiring its observable lock", code)
		case <-poll.C:
		case <-deadline.C:
			if _, err := os.Stat(lockPath); err == nil {
				return
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("inspect transcript flush lock at deadline: %v", err)
			}
			t.Fatalf("transcript flush did not acquire %s within %s", lockPath, 20*fixturePollInterval)
		}
	}
}

func TestGrokCaptureFlushFinalizesResponseAfterStopHookReturns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	var mu sync.Mutex
	var appended []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/self/activity":
			_, _ = w.Write([]byte(`{"activity":{"last_activity_at":"2026-07-17T10:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_grok","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_grok/entries:batch":
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
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeGrokBuild, RuntimeVersion: "0.2.101", CaptureMode: transcriptcapture.ModeRaw,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: location,
		Endpoint: srv.URL, TokenFile: tokenPath,
	}); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(home, ".grok", "sessions", "workspace", "session-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(sessionDir, "updates.jsonl")
	if err := os.WriteFile(transcriptPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	promptRaw, _ := json.Marshal(map[string]any{
		"sessionId": "session-1", "hookEventName": "user_prompt_submit", "promptId": "prompt-1",
		"prompt": "delayed response prompt", "transcriptPath": transcriptPath,
	})
	prompt, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeGrokBuild, promptRaw)
	if err != nil {
		t.Fatal(err)
	}
	stopRaw, _ := json.Marshal(map[string]any{
		"sessionId": "session-1", "hookEventName": "stop", "promptId": "prompt-1",
		"reason": "end_turn", "transcriptPath": transcriptPath,
	})
	stop, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeGrokBuild, stopRaw)
	if err != nil {
		t.Fatal(err)
	}
	if stop.Kind != "turn.completed" {
		t.Fatalf("Stop was finalized inside the synchronous hook: %#v", stop)
	}
	pendingBeforeFlush, err := transcriptcapture.Pending(transcriptcapture.RuntimeGrokBuild)
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingBeforeFlush) == 0 {
		t.Fatal("Grok fixture has no pending event to flush")
	}
	flushLockPath := filepath.Join(filepath.Dir(pendingBeforeFlush[0].Path), ".flush.lock")

	done := make(chan int, 1)
	go func() {
		done <- transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeGrokBuild})
	}()
	waitForCaptureFlushLock(t, flushLockPath, done, foregroundFlushLockPollPeriod)
	updates := strings.Join([]string{
		`{"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"hook_execution","event_name":"stop","prompt_id":"prompt-1"}}}`,
		`{"method":"session/update","params":{"_meta":{"promptId":"prompt-1"},"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"delayed final response"}}}}`,
		`{"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1"}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("flush code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Grok flush did not observe the post-hook terminal fence")
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeGrokBuild)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %#v", pending)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(appended) != 2 || appended[0]["body"] != "delayed response prompt" ||
		appended[1]["body"] != "delayed final response" || appended[1]["role"] != "assistant" ||
		appended[1]["reply_to_external_id"] != appended[0]["external_id"] ||
		appended[0]["external_id"] != prompt.ID+":0" || appended[1]["external_id"] != stop.ID+":0" {
		t.Fatalf("post-hook Grok entries = %#v", appended)
	}
	if model, exists := appended[1]["model"]; exists && model != "" {
		t.Fatalf("unscoped native model was attributed to the assistant entry: %#v", appended[1])
	}
}

func TestPrepareTranscriptFlushRejectsBindingChanges(t *testing.T) {
	event := transcriptcapture.Event{
		Runtime: transcriptcapture.RuntimeGrokBuild, Account: "default", Realm: "default",
		Agent: "old-agent", AgentID: "agent_old", AgentName: "old-agent",
		Location:  transcriptcapture.Location{ID: "loc_1", Name: "home"},
		SessionID: "session-1", HookEvent: "UserPromptSubmit", Kind: "message.user", Role: "user",
	}
	pending := []transcriptcapture.PendingEvent{{Path: "/unused/event.json", Event: event}}
	blocked := map[string]error{}
	held := map[string]error{}
	ready, err := prepareTranscriptFlushEvents(pending, transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeGrokBuild, Account: "default", Realm: "default",
		Agent: "new-agent", AgentID: "agent_new", AgentName: "new-agent",
		Location: transcriptcapture.Location{ID: "loc_1", Name: "home"},
	}, blocked, held)
	if err == nil || !strings.Contains(err.Error(), "does not match") || len(ready) != 0 || len(blocked) != 0 || len(held) != 1 {
		t.Fatalf("binding-change preparation = ready %#v / blocked %#v / held %#v / err %v", ready, blocked, held, err)
	}
	if hasUnblockedCaptureEvent(pending, blocked, held) || countBlockedCaptureEvents(pending, blocked, held) != 1 {
		t.Fatal("a held event counted as uploadable work")
	}
}

// Events captured while the runtime was temporarily bound to another agent
// share the identity-free transcript key with the installed identity's later
// events of the same session. Holding them aside must not block those events:
// on the founder's machine 39 such events blocked 1,201 later events for ten
// hours (issue #323).
func TestPrepareTranscriptFlushHoldsMismatchedEventsWithoutBlockingTheTranscript(t *testing.T) {
	cfg := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeClaudeCode, Account: "default", Realm: "default",
		Agent: "scott", AgentID: "agent_1", AgentName: "scott",
		Location: transcriptcapture.Location{ID: "loc_1", Name: "home"},
	}
	matching := transcriptcapture.Event{
		Runtime: cfg.Runtime, Account: cfg.Account, Realm: cfg.Realm,
		Agent: cfg.Agent, AgentID: cfg.AgentID, AgentName: cfg.AgentName, Location: cfg.Location,
		SessionID: "session-shared", ID: "evt_scott", HookEvent: "SessionStart", Kind: "session.started", Role: "system",
	}
	foreign := matching
	foreign.ID = "evt_foreign"
	foreign.Agent, foreign.AgentID, foreign.AgentName = "cc-accept-1", "agent_foreign", "cc-accept-1"
	pending := []transcriptcapture.PendingEvent{
		{Path: "/unused/foreign.json", Event: foreign},
		{Path: "/unused/scott.json", Event: matching},
	}
	blocked := map[string]error{}
	held := map[string]error{}
	ready, err := prepareTranscriptFlushEvents(pending, cfg, blocked, held)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("held preparation err = %v", err)
	}
	if len(ready) != 1 || ready[0].Event.ID != matching.ID {
		t.Fatalf("installed identity's event was not selected: ready %#v", ready)
	}
	if len(blocked) != 0 {
		t.Fatalf("mismatched event blocked the shared transcript: %#v", blocked)
	}
	if _, isHeld := held["/unused/foreign.json"]; !isHeld || len(held) != 1 {
		t.Fatalf("held = %#v", held)
	}
	if hasUnblockedCaptureEvent(pending[:1], blocked, held) {
		t.Fatal("a held-only outbox would respawn a successor flush forever")
	}
	if countBlockedCaptureEvents(pending, blocked, held) != 1 {
		t.Fatalf("deferred count = %d, want 1", countBlockedCaptureEvents(pending, blocked, held))
	}
}

func TestCaptureEventBindingUsesStableLocationID(t *testing.T) {
	event := transcriptcapture.Event{
		Runtime: transcriptcapture.RuntimeGrokBuild, Account: "default", AccountID: "acc_1",
		Realm: "old-realm", RealmID: "realm_1",
		Agent: "scott", AgentID: "agent_1", AgentName: "scott",
		Location: transcriptcapture.Location{ID: "loc_1", Name: "old-label"},
	}
	cfg := transcriptcapture.Config{
		Runtime: event.Runtime, Account: "renamed-account", AccountID: "acc_1",
		Realm: "new-realm", RealmID: "realm_1",
		Agent: event.Agent, AgentID: event.AgentID, AgentName: event.AgentName,
		Location: transcriptcapture.Location{ID: "loc_1", Name: "new-label"},
	}
	if err := captureEventBindingError(event, cfg); err != nil {
		t.Fatalf("mutable account, realm, or location label stranded a pending event: %v", err)
	}
	cfg.Realm = event.Realm
	cfg.RealmID = ""
	if err := captureEventBindingError(event, cfg); err == nil {
		t.Fatal("asymmetric stable realm identity fell back to a mutable name")
	}
	cfg.RealmID = "realm_other"
	if err := captureEventBindingError(event, cfg); err == nil {
		t.Fatal("stable realm identity mismatch was accepted because labels matched")
	}
	event.AccountID, event.RealmID = "", ""
	cfg.Account, cfg.Realm = event.Account, event.Realm
	cfg.AccountID, cfg.RealmID = "acc_1", "realm_1"
	if err := captureEventBindingError(event, cfg); err != nil {
		t.Fatalf("legacy event with matching stable agent and location could not migrate: %v", err)
	}
	event.AccountID = "acc_1"
	if err := captureEventBindingError(event, cfg); err == nil {
		t.Fatal("event missing only stable realm identity fell back to a mutable name")
	}
	event.AccountID, event.RealmID = "", "realm_1"
	if err := captureEventBindingError(event, cfg); err == nil {
		t.Fatal("event missing only stable account identity fell back to a mutable name")
	}
	event.AccountID, event.RealmID = "", ""
	cfg.AgentID = "agent_other"
	if err := captureEventBindingError(event, cfg); err == nil {
		t.Fatal("legacy event migrated without a matching stable agent identity")
	}
}

func TestPrepareTranscriptFlushBlocksOnlyTheDeferredTranscript(t *testing.T) {
	cfg := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeGrokBuild, Account: "default", Realm: "default",
		Agent: "scott", AgentID: "agent_1", AgentName: "scott",
		Location: transcriptcapture.Location{ID: "loc_1", Name: "home"},
	}
	base := transcriptcapture.Event{
		Runtime: cfg.Runtime, Account: cfg.Account, Realm: cfg.Realm,
		Agent: cfg.Agent, AgentID: cfg.AgentID, AgentName: cfg.AgentName, Location: cfg.Location,
		HookEvent: "UserPromptSubmit", Kind: "message.user", Role: "user",
	}
	deferred := base
	deferred.SessionID = "session-a"
	deferred.ID = "evt_a_later"
	readyEvent := base
	readyEvent.SessionID = "session-b"
	readyEvent.ID = "evt_b"
	blocked := map[string]error{deferred.TranscriptExternalID(): nil}
	pending := []transcriptcapture.PendingEvent{
		{Path: "/unused/a.json", Event: deferred},
		{Path: "/unused/b.json", Event: readyEvent},
	}
	ready, err := prepareTranscriptFlushEvents(pending, cfg, blocked, map[string]error{})
	if err != nil || len(ready) != 1 || ready[0].Event.ID != readyEvent.ID {
		t.Fatalf("per-transcript preparation = ready %#v / blocked %#v / err %v", ready, blocked, err)
	}

	seen := map[string]struct{}{pending[0].Path: {}}
	reopenRetryableBlockedTranscripts(pending[:1], blocked, seen)
	if hasUnblockedCaptureEvent(pending[:1], blocked, map[string]error{}) {
		t.Fatal("deferred-only outbox became uploadable without a new hook")
	}
	newSameTranscript := transcriptcapture.PendingEvent{Path: "/unused/a-new.json", Event: deferred}
	newSameTranscript.Event.ID = "evt_a_new"
	reopenRetryableBlockedTranscripts([]transcriptcapture.PendingEvent{pending[0], newSameTranscript}, blocked, seen)
	if !hasUnblockedCaptureEvent([]transcriptcapture.PendingEvent{pending[0], newSameTranscript}, blocked, map[string]error{}) {
		t.Fatal("a new same-transcript hook did not reopen retryable finalization")
	}
}

func TestForegroundTranscriptFlushWaitsForActiveDetachedFlush(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv(captureDetachedFlushEnv, "")
	release, acquired, err := transcriptcapture.AcquireFlushLock(transcriptcapture.RuntimeGrokBuild)
	if err != nil || !acquired {
		t.Fatalf("acquire test lock = %t / %v", acquired, err)
	}
	done := make(chan int, 1)
	go func() {
		done <- transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeGrokBuild})
	}()
	select {
	case code := <-done:
		release()
		t.Fatalf("foreground flush returned %d while another flusher held the lock", code)
	case <-time.After(150 * time.Millisecond):
	}
	release()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("foreground flush code after lock release = %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreground flush did not acquire the released lock")
	}
}

func TestDetachedTranscriptFlushDoesNotWaitForActiveFlush(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv(captureDetachedFlushEnv, "1")
	release, acquired, err := transcriptcapture.AcquireFlushLock(transcriptcapture.RuntimeGrokBuild)
	if err != nil || !acquired {
		t.Fatalf("acquire test lock = %t / %v", acquired, err)
	}
	defer release()
	started := time.Now()
	if code := transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeGrokBuild}); code != 0 {
		t.Fatalf("detached flush code = %d", code)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("detached flush waited for active owner: %s", elapsed)
	}
}

func TestTranscriptFlushContextBoundsOnlyDetachedWork(t *testing.T) {
	if foregroundFlushLockMaxWait <= detachedFlushMaxDuration {
		t.Fatalf("foreground lock wait %s does not cover detached work window %s", foregroundFlushLockMaxWait, detachedFlushMaxDuration)
	}
	foreground, cancelForeground := transcriptFlushContext(false)
	defer cancelForeground()
	if _, bounded := foreground.Deadline(); bounded {
		t.Fatal("foreground transcript delivery fence has a global deadline")
	}

	detached, cancelDetached := transcriptFlushContext(true)
	defer cancelDetached()
	deadline, bounded := detached.Deadline()
	if !bounded {
		t.Fatal("detached transcript flush has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > detachedFlushMaxDuration {
		t.Fatalf("detached deadline remaining = %s", remaining)
	}
}

func TestCaptureFlushActivityFailureDoesNotBlockTranscriptAndRetriesSameEvent(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	var order []string
	var touches []map[string]any
	createAttempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/self/activity":
			order = append(order, "activity")
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode activity: %v", err)
				http.Error(w, "bad activity", http.StatusBadRequest)
				return
			}
			if len(body) != 6 || body["runtime"] != "cursor" || body["location"] != "home" ||
				body["event"] != "SessionStart" || body["location_id"] == "" ||
				body["event_id"] == "" || body["event_occurred_at"] == "" {
				t.Errorf("activity body = %#v", body)
			}
			for _, forbidden := range []string{"cwd", "body", "model", "raw", "availability", "session_id"} {
				if _, exists := body[forbidden]; exists {
					t.Errorf("activity body exposed %s: %#v", forbidden, body)
				}
			}
			touches = append(touches, body)
			if len(touches) == 1 {
				http.Error(w, "temporary activity failure", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"activity":{"last_activity_at":"2026-07-15T20:00:00Z","last_runtime":"cursor","last_location":"home","last_event":"UserPromptSubmit"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			order = append(order, "transcript")
			createAttempts++
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			order = append(order, "batch")
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
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCursor, CaptureMode: transcriptcapture.ModeMessages,
		Account: "default", Realm: "default", Agent: "scott",
		AgentID: "agent_1", AgentName: "scott", Location: location,
		Endpoint: srv.URL, TokenFile: tokenPath,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCursor, []byte(
		`{"conversation_id":"conversation-1","hook_event_name":"sessionStart","cwd":"/private/worktree"}`,
	)); err != nil {
		t.Fatal(err)
	}
	if code := transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeCursor}); code != 1 {
		t.Fatalf("activity-failed flush code = %d, want retryable failure", code)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCursor)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending after failed activity = %d / %v", len(pending), err)
	}
	if createAttempts != 1 {
		t.Fatalf("transcript attempts after activity failure = %d, want 1", createAttempts)
	}
	if code := transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeCursor}); code != 0 {
		t.Fatalf("successful retry flush code = %d", code)
	}
	pending, err = transcriptcapture.Pending(transcriptcapture.RuntimeCursor)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after retry = %d / %v", len(pending), err)
	}
	if got := strings.Join(order, ","); got != "activity,transcript,batch,activity,transcript,batch" {
		t.Fatalf("request order = %s", got)
	}
	if len(touches) != 2 || touches[0]["event_id"] != touches[1]["event_id"] ||
		touches[0]["event_occurred_at"] != touches[1]["event_occurred_at"] {
		t.Fatalf("retry touches = %#v", touches)
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
	touches := 0
	creates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/self/activity":
			mu.Lock()
			touches++
			mu.Unlock()
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			mu.Lock()
			creates++
			mu.Unlock()
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
	if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, []byte(`{"session_id":"session-1","hook_event_name":"SessionStart","transcript_path":"/tmp/.codex/sessions/session-1/rollout.jsonl"}`)); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeCodex}) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("flush did not start")
	}
	// Queued while the first iteration is mid-flight: one more event for the
	// transcript already being drained, and one for a transcript this run has
	// not seen yet. Both must land in the same foreground invocation.
	for _, raw := range []string{
		`{"session_id":"session-1","hook_event_name":"SessionStart","transcript_path":"/tmp/.codex/sessions/session-1/rollout.jsonl"}`,
		`{"session_id":"session-2","hook_event_name":"SessionStart","transcript_path":"/tmp/.codex/sessions/session-2/rollout.jsonl"}`,
	} {
		if _, err := transcriptcapture.EnqueueHook(transcriptcapture.RuntimeCodex, []byte(raw)); err != nil {
			t.Fatal(err)
		}
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
	// session-1 is touched once per outer iteration (twice) and created once;
	// session-2 is touched and created once in the second iteration.
	if len(pending) != 0 || appended != 3 || touches != 3 || creates != 2 {
		t.Fatalf("pending/appended/touched/created = %d/%d/%d/%d, want 0/3/3/2",
			len(pending), appended, touches, creates)
	}
}

func TestCaptureFlushQuarantinesEphemeralCodexEventQueuedWhileDraining(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	// A nonexistent override makes any unexpected post-release respawn fail the
	// foreground invocation instead of silently launching a test subprocess.
	t.Setenv(witselfExecutableTestEnv, filepath.Join(t.TempDir(), "background-flush-must-not-start"))

	var arrival transcriptcapture.Event
	const arrivalBasename = "00000000000000000002-evt_ephemeral_mid_drain.json"
	var once sync.Once
	var mu sync.Mutex
	var requestPaths []string
	var appendedIDs []string
	var arrivalPath string
	var arrivalRaw []byte
	var arrivalErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestPaths = append(requestPaths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/self/activity":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts":
			_, _ = w.Write([]byte(`{"transcript":{"id":"trn_1","metadata":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcripts/trn_1/entries:batch":
			var body struct {
				Entries []struct {
					ExternalID string `json:"external_id"`
				} `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad append", http.StatusBadRequest)
				return
			}
			once.Do(func() {
				path, raw, err := writeCapturePendingFixture(
					home, transcriptcapture.RuntimeCodex, arrivalBasename, arrival,
				)
				mu.Lock()
				arrivalPath, arrivalRaw, arrivalErr = path, raw, err
				mu.Unlock()
			})
			mu.Lock()
			writeErr := arrivalErr
			for _, entry := range body.Entries {
				appendedIDs = append(appendedIDs, entry.ExternalID)
			}
			mu.Unlock()
			if writeErr != nil {
				http.Error(w, "queue mid-drain event", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"entries":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	configureCaptureFlushTest(t, transcriptcapture.RuntimeCodex, srv.URL)

	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	base := transcriptcapture.Event{
		SchemaVersion: transcriptcapture.SchemaVersion, Runtime: transcriptcapture.RuntimeCodex,
		CaptureMode: cfg.CaptureMode, Account: cfg.Account, Realm: cfg.Realm,
		Agent: cfg.Agent, AgentID: cfg.AgentID, AgentName: cfg.AgentName, Location: cfg.Location,
		RunID: "run_mid_drain", HookEvent: "SessionStart", NativeHookEvent: "SessionStart",
		Kind: "session.started", Role: "system", CWD: "/src/witself",
	}
	persisted := base
	persisted.ID = "evt_persisted_mid_drain"
	persisted.SessionID = "persisted-session-mid-drain"
	persisted.SourceTranscriptPath = "/tmp/.codex/sessions/persisted-mid-drain/rollout.jsonl"
	persisted.OccurredAt = time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	if _, _, err := writeCapturePendingFixture(
		home, transcriptcapture.RuntimeCodex,
		"00000000000000000001-evt_persisted_mid_drain.json", persisted,
	); err != nil {
		t.Fatal(err)
	}
	arrival = base
	arrival.ID = "evt_ephemeral_mid_drain"
	arrival.SessionID = "ephemeral-session-mid-drain"
	arrival.OccurredAt = time.Date(2026, 9, 2, 10, 0, 1, 0, time.UTC)

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptFlush([]string{"--runtime", transcriptcapture.RuntimeCodex})
	})
	if code != 0 || stdout != "" || !strings.Contains(stderr,
		"flushed 1 codex transcript event(s); quarantined 1 ephemeral event(s)") {
		t.Fatalf("mid-drain quarantine flush = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	mu.Lock()
	gotRequestPaths := append([]string(nil), requestPaths...)
	gotAppendedIDs := append([]string(nil), appendedIDs...)
	gotArrivalPath := arrivalPath
	gotArrivalRaw := append([]byte(nil), arrivalRaw...)
	gotArrivalErr := arrivalErr
	mu.Unlock()
	if gotArrivalErr != nil {
		t.Fatalf("queue mid-drain fixture: %v", gotArrivalErr)
	}
	wantRequestPaths := []string{
		"POST /v1/self/activity",
		"POST /v1/transcripts",
		"POST /v1/transcripts/trn_1/entries:batch",
	}
	if !slices.Equal(gotRequestPaths, wantRequestPaths) {
		t.Fatalf("mid-drain request paths = %v, want %v", gotRequestPaths, wantRequestPaths)
	}
	// Entry external IDs are "<event id>:<entry index>"; the persisted event's
	// single entry must be the only one appended.
	if !slices.Equal(gotAppendedIDs, []string{persisted.ID + ":0"}) {
		t.Fatalf("mid-drain appended entry IDs = %v, want only %q", gotAppendedIDs, persisted.ID+":0")
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after mid-drain quarantine = %#v / %v", pending, err)
	}
	if _, err := os.Stat(gotArrivalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mid-drain source %s still exists: %v", gotArrivalPath, err)
	}
	quarantinedPath := filepath.Join(
		home, "capture", "quarantine", transcriptcapture.RuntimeCodex, arrivalBasename,
	)
	quarantinedRaw, err := os.ReadFile(quarantinedPath)
	if err != nil || !slices.Equal(quarantinedRaw, gotArrivalRaw) {
		t.Fatalf("mid-drain quarantined bytes = %q / %v", quarantinedRaw, err)
	}
	skipped, err := transcriptcapture.SkippedSessions(transcriptcapture.RuntimeCodex)
	if err != nil || len(skipped) != 1 || skipped[0].Events != 1 {
		t.Fatalf("mid-drain skipped inventory = %#v / %v", skipped, err)
	}
}

func TestCuratorSessionHookDoesNotCaptureItself(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CURATOR_SESSION", "1")
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCodex, CaptureMode: transcriptcapture.ModeRaw,
		HookMode: transcriptcapture.HookModeUser, Account: "default", Realm: "default",
		Agent: "scott", AgentID: "agent_1", AgentName: "scott", Location: location,
	}); err != nil {
		t.Fatal(err)
	}

	input, err := os.CreateTemp(t.TempDir(), "curator-hook-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	if _, err := input.WriteString(`{"session_id":"curator-session","hook_event_name":"SessionStart","transcript_path":"/tmp/.codex/sessions/curator/rollout.jsonl"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	previousStdin := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = previousStdin })

	if code := transcriptHook([]string{"--runtime", transcriptcapture.RuntimeCodex}); code != 0 {
		t.Fatalf("hook code = %d", code)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("curator hook queued %d self-capture events", len(pending))
	}
	skipped, err := transcriptcapture.SkippedSessions(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("curator hook recorded %d skipped self-capture session(s)", len(skipped))
	}
}

func TestTranscriptHookSkipsEphemeralCodexWithoutHydrationOutput(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_CURATOR_SESSION", "")
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "")
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCodex, CaptureMode: transcriptcapture.ModeRaw,
		HookMode: transcriptcapture.HookModeUser, Account: "default", Realm: "default",
		Agent: "scott", AgentID: "agent_1", AgentName: "scott", Location: location,
		Endpoint: "http://127.0.0.1:1", TokenFile: filepath.Join(t.TempDir(), "unused.token"),
	}); err != nil {
		t.Fatal(err)
	}

	input, err := os.CreateTemp(t.TempDir(), "ephemeral-codex-hook-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	const promptCanary = "ephemeral-prompt-must-never-be-hydrated-or-queued"
	if _, err := input.WriteString(`{"session_id":"codex-internal-session-canary","hook_event_name":"UserPromptSubmit","transcript_path":null,"prompt":"` + promptCanary + `","runtime_version":"0.149.0","model":"gpt-5.6"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	previousStdin := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = previousStdin })

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptHook([]string{
			"--runtime", transcriptcapture.RuntimeCodex,
			"--account", "default", "--realm", "default", "--agent", "scott", "--location", "home",
		})
	})
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("ephemeral Codex hook = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("ephemeral Codex hook queued %d event(s)", len(pending))
	}
	skipped, err := transcriptcapture.SkippedSessions(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0].Events != 1 || skipped[0].HookEvents["UserPromptSubmit"] != 1 ||
		skipped[0].Reason != "ephemeral_session" || skipped[0].RuntimeVersion != "0.149.0" || skipped[0].Model != "gpt-5.6" {
		t.Fatalf("skipped session inventory = %#v", skipped)
	}
	markers, err := filepath.Glob(filepath.Join(home, "capture", "skipped", transcriptcapture.RuntimeCodex, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 1 {
		t.Fatalf("skipped marker files = %v", markers)
	}
	markerRaw, err := os.ReadFile(markers[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{promptCanary, "codex-internal-session-canary"} {
		if strings.Contains(string(markerRaw), forbidden) {
			t.Fatalf("skipped marker contains %q: %s", forbidden, markerRaw)
		}
	}
}

func TestAutomaticHydrationHookCurrentRuntimeConformance(t *testing.T) {
	tests := []struct {
		name, runtime, event, body string
		wantCalls                  int
		wantOutput                 string
		pendingCheckpoint          bool
		pendingMessage             bool
	}{
		{name: "Codex session", runtime: transcriptcapture.RuntimeCodex, event: memoryhydration.EventSessionStart, wantCalls: 1, wantOutput: "hookSpecificOutput"},
		{name: "Codex prompt", runtime: transcriptcapture.RuntimeCodex, event: memoryhydration.EventUserPromptSubmit, body: "resume our prior database decision", wantCalls: 2, wantOutput: "hookSpecificOutput"},
		{name: "Codex ordinary idle", runtime: transcriptcapture.RuntimeCodex, event: memoryhydration.EventUserPromptSubmit, body: "write a parser", wantCalls: 1},
		{name: "Codex ordinary checkpoint", runtime: transcriptcapture.RuntimeCodex, event: memoryhydration.EventUserPromptSubmit, body: "write a parser", wantCalls: 1, wantOutput: "hookSpecificOutput", pendingCheckpoint: true},
		{name: "Codex ordinary message", runtime: transcriptcapture.RuntimeCodex, event: memoryhydration.EventUserPromptSubmit, body: "write a parser", wantCalls: 1, wantOutput: "hookSpecificOutput", pendingMessage: true},
		{name: "Claude session", runtime: transcriptcapture.RuntimeClaudeCode, event: memoryhydration.EventSessionStart, wantCalls: 1, wantOutput: "hookSpecificOutput"},
		{name: "Claude prompt", runtime: transcriptcapture.RuntimeClaudeCode, event: memoryhydration.EventUserPromptSubmit, body: "pick up where we left off", wantCalls: 2, wantOutput: "hookSpecificOutput"},
		{name: "Claude ordinary idle", runtime: transcriptcapture.RuntimeClaudeCode, event: memoryhydration.EventUserPromptSubmit, body: "write a parser", wantCalls: 1},
		{name: "Claude ordinary checkpoint", runtime: transcriptcapture.RuntimeClaudeCode, event: memoryhydration.EventUserPromptSubmit, body: "write a parser", wantCalls: 1, wantOutput: "hookSpecificOutput", pendingCheckpoint: true},
		{name: "Claude ordinary message", runtime: transcriptcapture.RuntimeClaudeCode, event: memoryhydration.EventUserPromptSubmit, body: "write a parser", wantCalls: 1, wantOutput: "hookSpecificOutput", pendingMessage: true},
		{name: "Cursor session fallback", runtime: transcriptcapture.RuntimeCursor, event: memoryhydration.EventSessionStart},
		{name: "Cursor prompt fallback", runtime: transcriptcapture.RuntimeCursor, event: memoryhydration.EventUserPromptSubmit, body: "resume our prior plan"},
		{name: "Grok session fallback", runtime: transcriptcapture.RuntimeGrokBuild, event: memoryhydration.EventSessionStart},
		{name: "Grok prompt fallback", runtime: transcriptcapture.RuntimeGrokBuild, event: memoryhydration.EventUserPromptSubmit, body: "resume our prior plan"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
			var mu sync.Mutex
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				if r.Header.Get("Authorization") != "Bearer hydration-token-canary" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/v1/self":
					if r.URL.Query().Get("include_counts") != "false" || r.URL.Query().Get("include_checkpoint") != "true" ||
						r.URL.Query().Get("include_message_checkpoint") != "true" ||
						r.URL.Query().Get("include_email_checkpoint") != "true" ||
						r.URL.Query().Get("include_sensitive") != "true" {
						t.Errorf("unsafe automatic hydration query: %s", r.URL.RawQuery)
					}
					digest := client.SelfDigest{
						SchemaVersion:   "witself.v0",
						Identity:        client.SelfIdentity{AccountID: "acc_1", RealmID: "rlm_1", RealmName: "default", AgentID: "agt_1", AgentName: "atlas"},
						PrimaryFacts:    []client.SelfFact{{ID: "fact_1", Name: "database", Value: "Postgres", Primary: true}},
						SalientMemories: []client.SelfMemory{{ID: "mem_self", Kind: "decision", Snippet: "Use portable narrative memory", Salience: .8}},
						Index:           client.SelfIndex{Kinds: []string{"decision"}, Tags: []string{}, Counts: map[string]int{"facts": 1, "memories": 1}},
					}
					if test.pendingCheckpoint {
						digest.MemoryCheckpoint = &client.SelfMemoryCheckpoint{
							Pending: true, RequestID: "mcrq_hook", RequestGeneration: 3,
						}
					}
					if test.pendingMessage {
						digest.MessageCheckpoint = &client.SelfMessageCheckpoint{
							Pending: true, MailboxPending: true,
						}
					}
					_ = json.NewEncoder(w).Encode(digest)
				case "/v1/memories:recall":
					var input client.MemoryRecallInput
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Errorf("decode recall: %v", err)
					}
					if !input.IncludeSensitive || input.Query == "" || input.Limit != memoryhydration.DefaultRecallLimit {
						t.Errorf("unsafe recall input = %#v", input)
					}
					_ = json.NewEncoder(w).Encode(client.MemoryRecallPage{
						RetrievalMode: "lexical",
						Hits: []client.MemoryRecallHit{{
							Memory: client.Memory{ID: "mem_1", AccountID: "acc_1", RealmID: "rlm_1", Owner: client.MemoryOwner{AgentID: "agt_1"}, Content: "Postgres is canonical", ContentEncoding: "plain", Kind: "decision"},
							Score:  client.MemoryRecallScore{Total: .9},
						}},
					})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			tokenPath := filepath.Join(t.TempDir(), "agent.token")
			if err := os.WriteFile(tokenPath, []byte("hydration-token-canary\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			location, err := transcriptcapture.EnsureLocation("home")
			if err != nil {
				t.Fatal(err)
			}
			if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
				Runtime: test.runtime, CaptureMode: transcriptcapture.ModeMessages, HookMode: transcriptcapture.HookModeUser,
				Account: "default", AccountID: "acc_1", Realm: "default", RealmID: "rlm_1",
				Agent: "atlas", AgentID: "agt_1", AgentName: "atlas", Location: location,
				Endpoint: server.URL, TokenFile: tokenPath,
			}); err != nil {
				t.Fatal(err)
			}
			output, err := automaticHydrationHook(context.Background(), transcriptcapture.Event{
				Runtime: test.runtime, HookEvent: test.event, Body: test.body,
			})
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			gotCalls := calls
			mu.Unlock()
			if gotCalls != test.wantCalls {
				t.Fatalf("server calls = %d, want %d", gotCalls, test.wantCalls)
			}
			if test.wantOutput == "" {
				if len(output) != 0 {
					t.Fatalf("fallback emitted hook output: %s", output)
				}
				return
			}
			if !json.Valid(output) || !strings.Contains(string(output), test.wantOutput) ||
				!strings.Contains(string(output), "WITSELF_AUTOMATIC_CONTEXT_V1") ||
				strings.Contains(string(output), "hydration-token-canary") {
				t.Fatalf("hook output = %s", output)
			}
			if test.pendingCheckpoint && (!strings.Contains(string(output), "mcrq_hook") ||
				!strings.Contains(string(output), "empty actions plan")) {
				t.Fatalf("hook checkpoint output = %s", output)
			}
			if test.pendingMessage && (!strings.Contains(string(output), "message_checkpoint") ||
				!strings.Contains(string(output), "mailbox_pending")) {
				t.Fatalf("hook message checkpoint output = %s", output)
			}
		})
	}
}

func TestTranscriptHookHydrationFailsOpenOnIdentityMismatch(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/self" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(client.SelfDigest{Identity: client.SelfIdentity{
			AccountID: "acc_1", RealmID: "rlm_wrong", RealmName: "other", AgentID: "agt_1", AgentName: "atlas",
		}})
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenPath, []byte("identity-token-canary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeCodex, CaptureMode: transcriptcapture.ModeMessages, HookMode: transcriptcapture.HookModeUser,
		Account: "default", AccountID: "acc_1", Realm: "default", RealmID: "rlm_1",
		Agent: "atlas", AgentID: "agt_1", AgentName: "atlas", Location: location,
		Endpoint: server.URL, TokenFile: tokenPath,
	}); err != nil {
		t.Fatal(err)
	}
	input, err := os.CreateTemp(t.TempDir(), "hydration-hook-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	if _, err := input.WriteString(`{"session_id":"session-1","hook_event_name":"SessionStart","cwd":"/src/witself","transcript_path":"/tmp/.codex/sessions/session-1/rollout.jsonl"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	previousStdin := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = previousStdin })

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return transcriptHook([]string{
			"--runtime", transcriptcapture.RuntimeCodex,
			"--account", "default", "--realm", "default", "--agent", "atlas", "--location", "home",
		})
	})
	if code != 0 || stdout != "" || !strings.Contains(stderr, "continuing without automatic context") ||
		strings.Contains(stderr, "identity-token-canary") {
		t.Fatalf("fail-open hook = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	pending, err := transcriptcapture.Pending(transcriptcapture.RuntimeCodex)
	if err != nil || len(pending) != 1 {
		t.Fatalf("durable capture after hydration failure = %d / %v", len(pending), err)
	}
}

func TestLegacyAutomaticCuratorContinuationExposesNoCredential(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	binding := transcriptcapture.Config{
		Runtime:   transcriptcapture.RuntimeClaudeCode,
		AccountID: "acc_1", RealmID: "rlm_1", AgentID: "agt_1",
		TokenFile: "/private/curator-token-canary",
	}
	store, err := memorycurator.DefaultAutoStore(binding.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enable(memorycurator.AutoSettings{
		AccountID: binding.AccountID, RealmID: binding.RealmID, AgentID: binding.AgentID,
		Provider: memorycurator.ProviderClaudeCode, ProviderPath: filepath.Join(t.TempDir(), "claude"),
		ApplyPolicy: memorycurator.ApplyPolicyPreview, AllowTranscriptContent: true,
		Debounce: time.Second, MinimumInterval: 0, MaxRuns: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordWake(memorycurator.AutoWakeScheduledPoll); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "auto-args.log")
	helperPath := filepath.Join(t.TempDir(), "witself-helper")
	helper := "#!/bin/sh\ntmp=\"$AUTO_ARGS_FILE.tmp.$$\"\nprintf '%s\\n' \"$@\" > \"$tmp\"\nmv \"$tmp\" \"$AUTO_ARGS_FILE\"\n"
	if err := os.WriteFile(helperPath, []byte(helper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(witselfExecutableTestEnv, helperPath)
	t.Setenv("AUTO_ARGS_FILE", logPath)
	if err := startBackgroundAutomaticCuratorIfPending(transcriptcapture.RuntimeClaudeCode, binding); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, err = os.ReadFile(logPath)
		if err == nil && len(raw) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)
	if args != "memory\ncurate\nauto\nrun\n--runtime\nclaude-code\n--supervise\n" {
		t.Fatalf("background argv = %q", args)
	}
	if strings.Contains(args, binding.TokenFile) || strings.Contains(args, "claude") && strings.Contains(args, "--provider") {
		t.Fatalf("background argv exposed credential/provider configuration: %q", args)
	}
}
