//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type antigravityIntegrationFixture struct {
	home      string
	witself   string
	cli       string
	log       string
	token     string
	serverURL string
}

func TestConfigureAntigravityBindingPreservesStableInvocationSymlinks(t *testing.T) {
	base := t.TempDir()
	realHome := filepath.Join(base, "home")
	if err := os.MkdirAll(realHome, 0o700); err != nil {
		t.Fatal(err)
	}
	firstCLI := copilotTestFile(t, base, "versions", "1.1.5", "agy")
	secondCLI := copilotTestFile(t, base, "versions", "1.1.6", "agy")
	firstWitself := copilotTestFile(t, base, "Cellar", "witself", "0.0.200", "bin", "witself")
	secondWitself := copilotTestFile(t, base, "Cellar", "witself", "0.0.201", "bin", "witself")
	stableCLI := filepath.Join(base, "bin", "agy")
	stableWitself := filepath.Join(base, "bin", "witself")
	if err := os.MkdirAll(filepath.Dir(stableCLI), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(firstCLI, stableCLI); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(firstWitself, stableWitself); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", realHome)
	t.Setenv("WITSELF_HOME", filepath.Join(realHome, ".witself"))

	first := copilotTestConfig()
	first.Runtime = transcriptcapture.RuntimeAntigravity
	if err := configureAntigravityBinding(&first, stableCLI, stableWitself); err != nil {
		t.Fatal(err)
	}
	if first.RuntimeCLICommand != stableCLI || first.MCPCommand != stableWitself {
		t.Fatalf("stable Antigravity binding = %#v", first)
	}

	if err := os.Remove(stableCLI); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stableWitself); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(firstCLI); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(firstWitself); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secondCLI, stableCLI); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secondWitself, stableWitself); err != nil {
		t.Fatal(err)
	}

	second := copilotTestConfig()
	second.Runtime = transcriptcapture.RuntimeAntigravity
	if err := configureAntigravityBinding(&second, stableCLI, stableWitself); err != nil {
		t.Fatal(err)
	}
	if second.RuntimeCLICommand != first.RuntimeCLICommand || second.MCPCommand != first.MCPCommand {
		t.Fatalf("package upgrade changed stable Antigravity binding from %#v to %#v", first, second)
	}
}

func setupAntigravityIntegrationFixture(t *testing.T) antigravityIntegrationFixture {
	t.Helper()
	home := t.TempDir()
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	fixture := antigravityIntegrationFixture{
		home: home, witself: executable,
		cli: filepath.Join(home, "agy"), log: filepath.Join(home, "agy.log"),
		token: filepath.Join(home, "agent.token"),
	}
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("ANTIGRAVITY_CLI_PATH", fixture.cli)
	t.Setenv(witselfExecutableTestEnv, executable)
	t.Setenv("FAKE_AGY_LOG", fixture.log)
	t.Setenv("FAKE_AGY_BREAK_CONFIG", "")
	t.Setenv("FAKE_AGY_COLLISION_PATH", "")
	t.Setenv("FAKE_AGY_COLLISION_SERVER", "")
	t.Setenv("FAKE_AGY_MUTATE_SOURCE", "")
	t.Setenv("FAKE_AGY_FAIL_VALIDATE", "")
	t.Setenv("FAKE_AGY_SLEEP_VALIDATE", "")
	t.Setenv("FAKE_AGY_SLEEP_VERSION", "")

	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_AGY_LOG"
if [ "$1" = "--version" ]; then
	if [ -n "$FAKE_AGY_SLEEP_VERSION" ]; then
	  exec /bin/sleep "$FAKE_AGY_SLEEP_VERSION"
	fi
  printf '%s\n' 'agy version 1.1.1'
  exit 0
fi
if [ "$1 $2" = "plugin validate" ]; then
	test -f "$3/plugin.json" || exit 21
	test ! -e "$3/mcp_config.json" || exit 22
	test -f "$3/rules/witself.md" || exit 23
	if [ -n "$FAKE_AGY_FAIL_VALIDATE" ]; then
	  printf '%s\n' 'simulated validation failure' >&2
	  exit 25
	fi
	if [ -n "$FAKE_AGY_SLEEP_VALIDATE" ]; then
	  exec /bin/sleep "$FAKE_AGY_SLEEP_VALIDATE"
	fi
	if [ -n "$FAKE_AGY_MUTATE_SOURCE" ]; then
	  printf '%s\n' 'mutated during validation' > "$3/rules/witself.md"
	fi
	if [ -n "$FAKE_AGY_COLLISION_PATH" ]; then
	  printf '{"mcpServers":{"%s":{"command":"/foreign"}}}\n' "$FAKE_AGY_COLLISION_SERVER" > "$FAKE_AGY_COLLISION_PATH"
	fi
  if [ -n "$FAKE_AGY_BREAK_CONFIG" ]; then
    /bin/rm -f "$FAKE_AGY_BREAK_CONFIG"
    /bin/mkdir "$FAKE_AGY_BREAK_CONFIG"
  fi
  printf '%s\n' 'Plugin validation passed'
  exit 0
fi
exit 24
`
	if err := os.WriteFile(fixture.cli, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.token, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer agent-token" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"schema_version":"witself.v0","identity":{"account_id":"acc_1","realm_id":"realm_1","realm_name":"default","agent_id":"agent_1","agent_name":"scott"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`)
	}))
	t.Cleanup(server.Close)
	fixture.serverURL = server.URL
	return fixture
}

func (fixture antigravityIntegrationFixture) installArgs() []string {
	return []string{
		"antigravity", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--endpoint", fixture.serverURL, "--token-file", fixture.token,
	}
}

func (fixture antigravityIntegrationFixture) configRoot() string {
	return filepath.Join(fixture.home, ".gemini", "config")
}

func (fixture antigravityIntegrationFixture) namingConfig() transcriptcapture.Config {
	return transcriptcapture.Config{
		AccountID: "acc_1", RealmID: "realm_1", AgentID: "agent_1",
		MCPEnvironment: map[string]string{"WITSELF_HOME": filepath.Join(fixture.home, ".witself")},
	}
}

func (fixture antigravityIntegrationFixture) pluginName(t *testing.T) string {
	t.Helper()
	name, err := antigravityPluginName(fixture.namingConfig())
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func (fixture antigravityIntegrationFixture) serverName(t *testing.T) string {
	t.Helper()
	name, err := antigravityMCPServerName(fixture.namingConfig())
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func (fixture antigravityIntegrationFixture) pluginPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(fixture.configRoot(), "plugins", fixture.pluginName(t))
}

func sharedMCPContainsServer(raw []byte, name string) bool {
	var config antigravityMCPConfig
	if json.Unmarshal(raw, &config) != nil {
		return false
	}
	_, ok := config.Servers[name]
	return ok
}

func sharedMCPHasExactServer(raw []byte, name string, expected antigravityMCPServer) bool {
	var config antigravityMCPConfig
	if json.Unmarshal(raw, &config) != nil {
		return false
	}
	actual, ok := config.Servers[name]
	return ok && actual.Command == expected.Command && equalOrderedStrings(actual.Args, expected.Args) &&
		equalStringMaps(actual.Env, expected.Env)
}

func sharedMCPHasRootField(raw []byte, name, expectedJSON string) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return false
	}
	actual, ok := root[name]
	if !ok {
		return false
	}
	var actualCompact, expectedCompact bytes.Buffer
	if json.Compact(&actualCompact, actual) != nil || json.Compact(&expectedCompact, []byte(expectedJSON)) != nil {
		return false
	}
	return actualCompact.String() == expectedCompact.String()
}

func TestAntigravityBindingNamesAreDeterministicAndScoped(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	base := fixture.namingConfig()
	suffix, err := antigravityBindingSuffix(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(suffix) != 24 {
		t.Fatalf("binding suffix length = %d", len(suffix))
	}
	if _, err := hex.DecodeString(suffix); err != nil || strings.ToLower(suffix) != suffix {
		t.Fatalf("binding suffix is not lowercase hex: %q, %v", suffix, err)
	}
	again, err := antigravityBindingSuffix(base)
	if err != nil || again != suffix {
		t.Fatalf("binding suffix is not deterministic: %q, %v", again, err)
	}
	pluginName, err := antigravityPluginName(base)
	if err != nil || pluginName != antigravityPluginNamePrefix+suffix {
		t.Fatalf("plugin name = %q, %v", pluginName, err)
	}
	serverName, err := antigravityMCPServerName(base)
	if err != nil || serverName != antigravityMCPServerNamePrefix+suffix[:antigravityMCPServerSuffixLength] {
		t.Fatalf("server name = %q, %v", serverName, err)
	}
	for _, raw := range []string{base.AccountID, base.RealmID, base.AgentID, base.MCPEnvironment["WITSELF_HOME"]} {
		if strings.Contains(pluginName, raw) || strings.Contains(serverName, raw) {
			t.Fatalf("binding name leaks raw selector %q", raw)
		}
	}
	mutations := []func(*transcriptcapture.Config){
		func(cfg *transcriptcapture.Config) { cfg.AccountID += "x" },
		func(cfg *transcriptcapture.Config) { cfg.RealmID += "x" },
		func(cfg *transcriptcapture.Config) { cfg.AgentID += "x" },
		func(cfg *transcriptcapture.Config) { cfg.MCPEnvironment["WITSELF_HOME"] += "-other" },
	}
	for index, mutate := range mutations {
		candidate := base
		candidate.MCPEnvironment = cloneStringMap(base.MCPEnvironment)
		mutate(&candidate)
		got, err := antigravityBindingSuffix(candidate)
		if err != nil || got == suffix {
			t.Errorf("mutation %d suffix = %q, %v", index, got, err)
		}
	}
	for _, invalid := range []transcriptcapture.Config{
		{RealmID: base.RealmID, AgentID: base.AgentID, MCPEnvironment: cloneStringMap(base.MCPEnvironment)},
		{AccountID: base.AccountID + "\n", RealmID: base.RealmID, AgentID: base.AgentID, MCPEnvironment: cloneStringMap(base.MCPEnvironment)},
	} {
		if _, err := antigravityBindingSuffix(invalid); err == nil {
			t.Fatalf("invalid binding identity was accepted: %#v", invalid)
		}
	}
}

func TestAntigravityInstallReinstallAndUninstallLifecycle(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	sharedMCP := filepath.Join(fixture.configRoot(), "mcp_config.json")
	if err := os.MkdirAll(filepath.Dir(sharedMCP), 0o700); err != nil {
		t.Fatal(err)
	}
	sharedMCPRaw := []byte(`{"providerSettings":{"enabled":true},"mcpServers":{"witself":{"command":"/legacy-unrelated"}}}`)
	if err := os.WriteFile(sharedMCP, sharedMCPRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	unrelatedPlugin := filepath.Join(fixture.configRoot(), "plugins", "witself")
	if err := os.MkdirAll(unrelatedPlugin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelatedPlugin, "plugin.json"), []byte(`{"name":"witself"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(fixture.configRoot(), "import_manifest.json")
	manifest := []byte(`{"imports":[{"name":"witself","source":"legacy-unrelated"}]}`)
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	if code := installCmd(fixture.installArgs()); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
	if err != nil {
		t.Fatal(err)
	}
	serverName := fixture.serverName(t)
	toolPrefix := "mcp_" + serverName + "_"
	if cfg.HookMode != transcriptcapture.HookModeNone || cfg.RuntimeCLICommand != fixture.cli ||
		cfg.MCPCommand != fixture.witself || cfg.RuntimeVersion != "1.1.1" ||
		cfg.RuntimeConfigRoot != fixture.configRoot() || cfg.RuntimePluginPath != fixture.pluginPath(t) ||
		cfg.RuntimeMCPConfigPath != sharedMCP ||
		len(cfg.MCPEnvironment) != 1 || cfg.MCPEnvironment["WITSELF_HOME"] != filepath.Join(fixture.home, ".witself") ||
		len(cfg.RuntimePluginDigest) != 64 || filepath.Base(cfg.RuntimePluginSource) != cfg.RuntimePluginDigest {
		t.Fatalf("Antigravity config = %#v", cfg)
	}
	bundle, err := verifiedAntigravitySourceBundle(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
		t.Fatal(err)
	}
	if err := validateAntigravityInstalledTopology(cfg); err != nil {
		t.Fatal(err)
	}
	rule, err := os.ReadFile(filepath.Join(cfg.RuntimePluginPath, "rules", "witself.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		toolPrefix + "witself.self.show", toolPrefix + "witself.memory.recall",
		toolPrefix + "witself.email.listen", toolPrefix + "witself.avatar.show",
		toolPrefix + "witself.secret.search", "no Witself transcript hooks",
	} {
		if !strings.Contains(string(rule), required) {
			t.Errorf("managed Antigravity rule lacks %q", required)
		}
	}
	if len(rule) > antigravityRuleCharacterLimit || utf8.RuneCount(rule) > antigravityRuleCharacterLimit {
		t.Fatalf("managed Antigravity rule exceeds provider limit: bytes=%d chars=%d", len(rule), utf8.RuneCount(rule))
	}
	if strings.Contains(string(rule), "witself__witself-") || strings.Contains(string(rule), "OpenClaw") {
		t.Fatalf("managed Antigravity rule retains another runtime's names: %s", rule)
	}
	if _, err := os.Lstat(filepath.Join(cfg.RuntimePluginPath, "mcp_config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rules-only plugin unexpectedly contains MCP config: %v", err)
	}
	mcpRaw, err := os.ReadFile(sharedMCP)
	if err != nil {
		t.Fatal(err)
	}
	var mcp antigravityMCPConfig
	if err := json.Unmarshal(mcpRaw, &mcp); err != nil {
		t.Fatal(err)
	}
	server := mcp.Servers[serverName]
	if server.Command != fixture.witself || len(server.Env) != 1 || server.Env["WITSELF_HOME"] != filepath.Join(fixture.home, ".witself") ||
		!containsOrderedStrings(server.Args, []string{"mcp", "serve", "--runtime", "antigravity", "--account", "default", "--realm", "default", "--agent", "scott", "--location", "home"}) {
		t.Fatalf("Antigravity MCP server = %#v", server)
	}

	if code := installCmd(fixture.installArgs()); code != 0 {
		t.Fatalf("idempotent reinstall code = %d", code)
	}
	if raw, err := os.ReadFile(sharedMCP); err != nil || !sharedMCPHasExactServer(raw, "witself", antigravityMCPServer{Command: "/legacy-unrelated"}) ||
		!sharedMCPHasExactServer(raw, serverName, server) || !sharedMCPHasRootField(raw, "providerSettings", `{"enabled":true}`) {
		t.Fatalf("shared MCP config lost a server after reinstall: %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(manifestPath); err != nil || string(raw) != string(manifest) {
		t.Fatalf("import manifest changed: %q, %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(unrelatedPlugin, "plugin.json")); err != nil {
		t.Fatalf("unrelated plugin changed: %v", err)
	}

	if err := os.Remove(fixture.cli); err != nil {
		t.Fatal(err)
	}
	if code := uninstallCmd([]string{"agy"}); code != 0 {
		t.Fatalf("uninstall without provider CLI code = %d", code)
	}
	if _, err := os.Lstat(cfg.RuntimePluginPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plugin remains after uninstall: %v", err)
	}
	if _, err := os.Lstat(cfg.RuntimePluginSource); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery source remains after uninstall: %v", err)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("integration config remains after uninstall: %v", err)
	}
	if raw, err := os.ReadFile(sharedMCP); err != nil || !sharedMCPHasExactServer(raw, "witself", antigravityMCPServer{Command: "/legacy-unrelated"}) ||
		sharedMCPContainsServer(raw, serverName) || !sharedMCPHasRootField(raw, "providerSettings", `{"enabled":true}`) {
		t.Fatalf("shared MCP config was not entry-scoped on uninstall: %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(manifestPath); err != nil || string(raw) != string(manifest) {
		t.Fatalf("import manifest changed on uninstall: %q, %v", raw, err)
	}
	logRaw, err := os.ReadFile(fixture.log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logRaw), "plugin install") || strings.Contains(string(logRaw), "plugin uninstall") {
		t.Fatalf("unsafe Antigravity plugin mutation CLI was invoked: %s", logRaw)
	}
}

func TestAntigravityRefusesForeignDriftedDisabledAndManifestOwnedPlugins(t *testing.T) {
	tests := []struct {
		name  string
		drift func(t *testing.T, fixture antigravityIntegrationFixture, cfg transcriptcapture.Config)
	}{
		{"content drift", func(t *testing.T, _ antigravityIntegrationFixture, cfg transcriptcapture.Config) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(cfg.RuntimePluginPath, "rules", "witself.md"), []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"extra file", func(t *testing.T, _ antigravityIntegrationFixture, cfg transcriptcapture.Config) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(cfg.RuntimePluginPath, "extra.txt"), []byte("foreign\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"disabled", func(t *testing.T, _ antigravityIntegrationFixture, cfg transcriptcapture.Config) {
			t.Helper()
			if err := os.Rename(filepath.Join(cfg.RuntimePluginPath, "plugin.json"), filepath.Join(cfg.RuntimePluginPath, "plugin.json.disabled")); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, _ antigravityIntegrationFixture, cfg transcriptcapture.Config) {
			t.Helper()
			path := filepath.Join(cfg.RuntimePluginPath, "rules", "witself.md")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(cfg.RuntimePluginSource, "rules", "witself.md"), path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := setupAntigravityIntegrationFixture(t)
			if code := installCmd(fixture.installArgs()); code != 0 {
				t.Fatalf("install code = %d", code)
			}
			cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
			if err != nil {
				t.Fatal(err)
			}
			test.drift(t, fixture, cfg)
			if err := validateAntigravityInstalledTopology(cfg); err == nil {
				t.Fatal("drifted plugin passed startup topology validation")
			}
			for _, runtimeName := range []string{"antigravity", "agy"} {
				if code := mcpCmd([]string{"serve", "--runtime", runtimeName}); code != 1 {
					t.Errorf("drifted %s MCP startup code = %d", runtimeName, code)
				}
			}
			if code := installCmd(fixture.installArgs()); code != 1 {
				t.Fatalf("drifted reinstall code = %d", code)
			}
			if code := uninstallCmd([]string{"antigravity"}); code != 1 {
				t.Fatalf("drifted uninstall code = %d", code)
			}
			if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); err != nil {
				t.Fatalf("config was removed despite drift: %v", err)
			}
		})
	}

	t.Run("foreign first install", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		path := fixture.pluginPath(t)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		foreign := []byte(`{"name":"foreign"}`)
		if err := os.WriteFile(filepath.Join(path, "plugin.json"), foreign, 0o600); err != nil {
			t.Fatal(err)
		}
		if code := installCmd(fixture.installArgs()); code != 1 {
			t.Fatalf("foreign install code = %d", code)
		}
		if raw, err := os.ReadFile(filepath.Join(path, "plugin.json")); err != nil || string(raw) != string(foreign) {
			t.Fatalf("foreign plugin changed: %q, %v", raw, err)
		}
		if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed foreign install retained config: %v", err)
		}
	})

	t.Run("manifest ownership collision", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		if err := os.MkdirAll(fixture.configRoot(), 0o700); err != nil {
			t.Fatal(err)
		}
		manifest := filepath.Join(fixture.configRoot(), "import_manifest.json")
		foreign := []byte(fmt.Sprintf(`{"imports":[{"name":%q,"source":"antigravity"}]}`, fixture.pluginName(t)))
		if err := os.WriteFile(manifest, foreign, 0o600); err != nil {
			t.Fatal(err)
		}
		if code := installCmd(fixture.installArgs()); code != 1 {
			t.Fatalf("manifest collision install code = %d", code)
		}
		if _, err := os.Lstat(fixture.pluginPath(t)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("plugin written despite manifest collision: %v", err)
		}
	})

	t.Run("shared MCP ownership collision", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		if err := os.MkdirAll(fixture.configRoot(), 0o700); err != nil {
			t.Fatal(err)
		}
		sharedMCP := filepath.Join(fixture.configRoot(), "mcp_config.json")
		foreign := []byte(fmt.Sprintf(`{"mcpServers":{%q:{"command":"/foreign"}}}`, strings.ToUpper(fixture.serverName(t))))
		if err := os.WriteFile(sharedMCP, foreign, 0o600); err != nil {
			t.Fatal(err)
		}
		if code := installCmd(fixture.installArgs()); code != 1 {
			t.Fatalf("shared MCP collision install code = %d", code)
		}
		if raw, err := os.ReadFile(sharedMCP); err != nil || string(raw) != string(foreign) {
			t.Fatalf("foreign shared MCP config changed: %q, %v", raw, err)
		}
		if _, err := os.Lstat(fixture.pluginPath(t)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("plugin written despite shared MCP collision: %v", err)
		}
		if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed collision install retained config: %v", err)
		}
	})
}

func TestAntigravityStartupAndMutationsFailClosedOnDiscoveryDrift(t *testing.T) {
	tests := []struct {
		name   string
		drift  func(t *testing.T, fixture antigravityIntegrationFixture, cfg transcriptcapture.Config)
		verify func(t *testing.T, fixture antigravityIntegrationFixture, cfg transcriptcapture.Config)
	}{
		{
			name: "recovery source drift",
			drift: func(t *testing.T, _ antigravityIntegrationFixture, cfg transcriptcapture.Config) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(cfg.RuntimePluginSource, "rules", "witself.md"), []byte("foreign source\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, _ antigravityIntegrationFixture, cfg transcriptcapture.Config) {
				t.Helper()
				raw, err := os.ReadFile(filepath.Join(cfg.RuntimePluginSource, "rules", "witself.md"))
				if err != nil || string(raw) != "foreign source\n" {
					t.Fatalf("recovery-source drift changed: %q, %v", raw, err)
				}
			},
		},
		{
			name: "post-install import manifest collision",
			drift: func(t *testing.T, fixture antigravityIntegrationFixture, cfg transcriptcapture.Config) {
				t.Helper()
				pluginName := filepath.Base(cfg.RuntimePluginPath)
				if err := os.WriteFile(filepath.Join(fixture.configRoot(), "import_manifest.json"), []byte(fmt.Sprintf(`{"imports":[{"name":%q}]}`, pluginName)), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, fixture antigravityIntegrationFixture, cfg transcriptcapture.Config) {
				t.Helper()
				raw, err := os.ReadFile(filepath.Join(fixture.configRoot(), "import_manifest.json"))
				expected := fmt.Sprintf(`{"imports":[{"name":%q}]}`, filepath.Base(cfg.RuntimePluginPath))
				if err != nil || string(raw) != expected {
					t.Fatalf("import-manifest collision changed: %q, %v", raw, err)
				}
			},
		},
		{
			name: "post-install shared MCP collision",
			drift: func(t *testing.T, fixture antigravityIntegrationFixture, cfg transcriptcapture.Config) {
				t.Helper()
				serverName, err := antigravityMCPServerName(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fixture.configRoot(), "mcp_config.json"), []byte(fmt.Sprintf(`{"mcpServers":{%q:{"command":"/foreign"}}}`, serverName)), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, fixture antigravityIntegrationFixture, cfg transcriptcapture.Config) {
				t.Helper()
				raw, err := os.ReadFile(filepath.Join(fixture.configRoot(), "mcp_config.json"))
				serverName, nameErr := antigravityMCPServerName(cfg)
				expected := fmt.Sprintf(`{"mcpServers":{%q:{"command":"/foreign"}}}`, serverName)
				if nameErr != nil || err != nil || string(raw) != expected {
					t.Fatalf("shared MCP collision changed: %q, %v", raw, err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := setupAntigravityIntegrationFixture(t)
			if code := installCmd(fixture.installArgs()); code != 0 {
				t.Fatalf("install code = %d", code)
			}
			cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := antigravityBundleFromConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeAntigravity)
			if err != nil {
				t.Fatal(err)
			}
			configBefore, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			test.drift(t, fixture, cfg)
			for _, runtimeName := range []string{"antigravity", "agy"} {
				if code := mcpCmd([]string{"serve", "--runtime", runtimeName}); code != 1 {
					t.Errorf("drifted %s MCP startup code = %d", runtimeName, code)
				}
			}
			if code := installCmd(fixture.installArgs()); code != 1 {
				t.Fatalf("drifted reinstall code = %d", code)
			}
			if code := uninstallCmd([]string{"antigravity"}); code != 1 {
				t.Fatalf("drifted uninstall code = %d", code)
			}
			configAfter, err := os.ReadFile(configPath)
			if err != nil || string(configAfter) != string(configBefore) {
				t.Fatalf("integration config changed despite discovery drift: %v", err)
			}
			if err := verifyAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
				t.Fatalf("installed plugin changed despite discovery drift: %v", err)
			}
			test.verify(t, fixture, cfg)
		})
	}
}

func TestAntigravityFirstInstallRollsBackPluginWhenFinalConfigWriteFails(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeAntigravity)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_AGY_BREAK_CONFIG", configPath)
	if code := installCmd(fixture.installArgs()); code != 1 {
		t.Fatalf("install code = %d", code)
	}
	pluginPath := fixture.pluginPath(t)
	if _, err := os.Lstat(pluginPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plugin remained after failed final config write: %v", err)
	}
	if _, err := os.Lstat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("integration config remained after rollback: %v", err)
	}
}

func TestAntigravityRefusesRecoverySourceMutationDuringCLIValidation(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	t.Setenv("FAKE_AGY_MUTATE_SOURCE", "1")
	if code := installCmd(fixture.installArgs()); code != 1 {
		t.Fatalf("install code = %d", code)
	}
	if _, err := os.Lstat(fixture.pluginPath(t)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plugin was installed from a source mutated during validation: %v", err)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed install retained integration config: %v", err)
	}
	bundlesPath := filepath.Join(fixture.home, ".witself", "integrations", "antigravity", "bundles")
	entries, err := os.ReadDir(bundlesPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("mutated recovery source remains after rollback: %#v", entries)
	}
}

func TestAntigravityRejectsOwnershipRootSymlinkDrift(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	if code := installCmd(fixture.installArgs()); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
	if err != nil {
		t.Fatal(err)
	}
	relocatedRoot := cfg.RuntimeConfigRoot + "-relocated"
	if err := os.Rename(cfg.RuntimeConfigRoot, relocatedRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relocatedRoot, cfg.RuntimeConfigRoot); err != nil {
		t.Fatal(err)
	}
	if code := mcpCmd([]string{"serve", "--runtime", "antigravity"}); code != 1 {
		t.Fatalf("MCP startup with symlinked config root code = %d", code)
	}
	if code := uninstallCmd([]string{"antigravity"}); code != 1 {
		t.Fatalf("uninstall with symlinked config root code = %d", code)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); err != nil {
		t.Fatalf("integration config changed despite symlink drift: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(relocatedRoot, "plugins", filepath.Base(cfg.RuntimePluginPath))); err != nil {
		t.Fatalf("relocated plugin changed despite symlink drift: %v", err)
	}
}

func TestAntigravityUninstallRejectsChangedHOMEBeforeMutatingInstalledRoot(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	if code := installCmd(fixture.installArgs()); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := verifiedAntigravitySourceBundle(cfg)
	if err != nil {
		t.Fatal(err)
	}
	otherHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", otherHome)
	if code := uninstallCmd([]string{"antigravity"}); code != 1 {
		t.Fatalf("uninstall with changed HOME code = %d", code)
	}
	if err := verifyAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
		t.Fatalf("old-HOME plugin changed without its lock: %v", err)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); err != nil {
		t.Fatalf("integration config changed with HOME mismatch: %v", err)
	}
	if _, err := os.Lstat(antigravityTransactionPath(cfg.RuntimeConfigRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old-HOME transaction journal was created: %v", err)
	}
}

func TestAntigravityRefusesDiscoveryCollisionCreatedDuringCLIValidation(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	sharedMCP := filepath.Join(fixture.configRoot(), "mcp_config.json")
	if err := os.MkdirAll(filepath.Dir(sharedMCP), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_AGY_COLLISION_PATH", sharedMCP)
	t.Setenv("FAKE_AGY_COLLISION_SERVER", fixture.serverName(t))
	if code := installCmd(fixture.installArgs()); code != 1 {
		t.Fatalf("install code = %d", code)
	}
	if _, err := os.Lstat(fixture.pluginPath(t)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plugin was installed despite a collision created during validation: %v", err)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed collision install retained integration config: %v", err)
	}
	raw, err := os.ReadFile(sharedMCP)
	if err != nil || !strings.Contains(string(raw), fixture.serverName(t)) {
		t.Fatalf("foreign discovery collision changed: %q, %v", raw, err)
	}
}

func TestAntigravityPluginValidationFailuresAreBounded(t *testing.T) {
	t.Run("nonzero", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		t.Setenv("FAKE_AGY_FAIL_VALIDATE", "1")
		if code := installCmd(fixture.installArgs()); code != 1 {
			t.Fatalf("install code = %d", code)
		}
		if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed validation retained config: %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		t.Setenv("FAKE_AGY_SLEEP_VALIDATE", "10")
		originalTimeout := antigravityPluginValidationTimeout
		originalWait := antigravityPluginValidationWait
		antigravityPluginValidationTimeout = 25 * time.Millisecond
		antigravityPluginValidationWait = 25 * time.Millisecond
		t.Cleanup(func() {
			antigravityPluginValidationTimeout = originalTimeout
			antigravityPluginValidationWait = originalWait
		})
		started := time.Now()
		if code := installCmd(fixture.installArgs()); code != 1 {
			t.Fatalf("install code = %d", code)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("timed-out validator returned after %s", elapsed)
		}
	})

	t.Run("output cap", func(t *testing.T) {
		output := &antigravityValidationOutput{limit: 8}
		raw := []byte("0123456789abcdef")
		if written, err := output.Write(raw); err != nil || written != len(raw) {
			t.Fatalf("write = %d, %v", written, err)
		}
		if value := output.String(); value != "01234567\n[validation output truncated]" {
			t.Fatalf("bounded output = %q", value)
		}
	})
}

func TestAntigravityVersionProbeTimeoutIsBounded(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	t.Setenv("FAKE_AGY_SLEEP_VERSION", "10")
	originalTimeout := runtimeVersionProbeTimeout
	originalWait := runtimeVersionProbeWait
	runtimeVersionProbeTimeout = 25 * time.Millisecond
	runtimeVersionProbeWait = 25 * time.Millisecond
	t.Cleanup(func() {
		runtimeVersionProbeTimeout = originalTimeout
		runtimeVersionProbeWait = originalWait
	})
	started := time.Now()
	if version := detectRuntimeVersion(transcriptcapture.RuntimeAntigravity, fixture.cli); version != "" {
		t.Fatalf("timed-out version probe = %q", version)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timed-out version probe returned after %s", elapsed)
	}
}

func TestAntigravityInitialConfigWriteFailureRemovesUnrecordedSource(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	originalSave := saveRuntimeIntegrationConfig
	saveRuntimeIntegrationConfig = func(cfg transcriptcapture.Config) error {
		if cfg.Runtime == transcriptcapture.RuntimeAntigravity {
			return errors.New("simulated initial config write failure")
		}
		return originalSave(cfg)
	}
	t.Cleanup(func() { saveRuntimeIntegrationConfig = originalSave })
	if code := installCmd(fixture.installArgs()); code != 1 {
		t.Fatalf("install code = %d", code)
	}
	bundlesPath := filepath.Join(fixture.home, ".witself", "integrations", "antigravity", "bundles")
	entries, err := os.ReadDir(bundlesPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unrecorded Antigravity source remains: %#v", entries)
	}
	if _, err := os.Lstat(fixture.pluginPath(t)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plugin was installed despite initial config failure: %v", err)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("integration config exists after initial save failure: %v", err)
	}
}

func TestAntigravityUpgradeFinalConfigFailureRestoresPriorBinding(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	if code := installCmd(fixture.installArgs()); code != 0 {
		t.Fatalf("initial install code = %d", code)
	}
	previous, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
	if err != nil {
		t.Fatal(err)
	}
	previousBundle, err := verifiedAntigravitySourceBundle(previous)
	if err != nil {
		t.Fatal(err)
	}
	upgradedExecutable := filepath.Join(fixture.home, "witself-v2")
	if err := os.WriteFile(upgradedExecutable, []byte("preview binary\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(witselfExecutableTestEnv, upgradedExecutable)
	attempted := previous
	attempted.MCPCommand = upgradedExecutable
	attemptedBundle, err := antigravityBundleFromConfig(attempted)
	if err != nil {
		t.Fatal(err)
	}
	attemptedDigest := attemptedBundle.digest()
	attemptedSource := filepath.Join(
		attempted.MCPEnvironment["WITSELF_HOME"], "integrations", "antigravity", "bundles", attemptedDigest,
	)

	originalFinalize := finalizeRuntimeIntegrationConfig
	finalizeRuntimeIntegrationConfig = func(cfg transcriptcapture.Config) error {
		if cfg.Runtime == transcriptcapture.RuntimeAntigravity && cfg.MCPCommand == upgradedExecutable {
			mutateAntigravitySharedMCPForTest(t, cfg, func(_ string, root, _ map[string]json.RawMessage) {
				root["concurrentProviderEdit"] = json.RawMessage(`{"preserved":true}`)
			})
			return errors.New("simulated final config write failure")
		}
		return originalFinalize(cfg)
	}
	t.Cleanup(func() { finalizeRuntimeIntegrationConfig = originalFinalize })
	if code := installCmd(fixture.installArgs()); code != 1 {
		t.Fatalf("upgrade code = %d", code)
	}
	loaded, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MCPCommand != previous.MCPCommand || loaded.RuntimePluginDigest != previous.RuntimePluginDigest ||
		loaded.RuntimePluginSource != previous.RuntimePluginSource {
		t.Fatalf("prior config was not restored: %#v", loaded)
	}
	if err := verifyAntigravityBundleDirectory(previous.RuntimePluginPath, previousBundle); err != nil {
		t.Fatalf("prior plugin was not restored: %v", err)
	}
	if err := verifyAntigravityBundleDirectory(previous.RuntimePluginSource, previousBundle); err != nil {
		t.Fatalf("prior recovery source was not preserved: %v", err)
	}
	if attemptedSource != previous.RuntimePluginSource {
		if _, err := os.Lstat(attemptedSource); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("attempted upgrade source remains: %v", err)
		}
	}
	if err := verifyAntigravitySharedMCPState(previous); err != nil {
		t.Fatalf("prior shared MCP entry was not restored: %v", err)
	}
	if raw, err := os.ReadFile(previous.RuntimeMCPConfigPath); err != nil ||
		!sharedMCPHasRootField(raw, "concurrentProviderEdit", `{"preserved":true}`) {
		t.Fatalf("concurrent shared MCP sibling edit was not preserved: %q, %v", raw, err)
	}
	assertNoAntigravityScratchDirectories(t, fixture.configRoot())
}

func TestAntigravityUninstallConfigFailureRestoresPlugin(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	if code := installCmd(fixture.installArgs()); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := verifiedAntigravitySourceBundle(cfg)
	if err != nil {
		t.Fatal(err)
	}
	originalRemove := removeRuntimeIntegrationConfig
	removeRuntimeIntegrationConfig = func(runtime string) error {
		if normalized, _ := transcriptcapture.NormalizeRuntime(runtime); normalized == transcriptcapture.RuntimeAntigravity {
			return errors.New("simulated config removal failure")
		}
		return originalRemove(runtime)
	}
	t.Cleanup(func() { removeRuntimeIntegrationConfig = originalRemove })
	if code := uninstallCmd([]string{"antigravity"}); code != 1 {
		t.Fatalf("uninstall code = %d", code)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); err != nil {
		t.Fatalf("config was not preserved: %v", err)
	}
	if err := verifyAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
		t.Fatalf("plugin was not restored: %v", err)
	}
	if err := verifyAntigravityBundleDirectory(cfg.RuntimePluginSource, bundle); err != nil {
		t.Fatalf("recovery source was not preserved: %v", err)
	}
	assertNoAntigravityScratchDirectories(t, fixture.configRoot())
}

func TestAntigravityInterruptedTransactionsRecoverExactState(t *testing.T) {
	t.Run("first install before plugin creation", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		if code := installCmd(fixture.installArgs()); code != 0 {
			t.Fatalf("seed install code = %d", code)
		}
		cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
		if err != nil {
			t.Fatal(err)
		}
		bundle, err := verifiedAntigravitySourceBundle(cfg)
		if err != nil {
			t.Fatal(err)
		}
		journal, err := beginAntigravityTransaction(antigravityTransactionInstall, nil, &cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := removeExactAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
			t.Fatal(err)
		}
		if _, err := convergeAntigravitySharedMCP(&cfg, nil); err != nil {
			t.Fatal(err)
		}
		if err := transcriptcapture.RemoveConfig(transcriptcapture.RuntimeAntigravity); err != nil {
			t.Fatal(err)
		}
		if err := recoverAntigravityTransaction(cfg.RuntimeConfigRoot); err != nil {
			t.Fatal(err)
		}
		assertAntigravityTransactionAbsent(t, cfg.RuntimeConfigRoot, journal)
		if _, err := os.Lstat(cfg.RuntimePluginSource); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rolled-back first-install source remains: %v", err)
		}
	})

	t.Run("first install shared MCP durable before plugin", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		cfg := installAntigravityFixtureConfig(t, fixture)
		bundle, err := verifiedAntigravitySourceBundle(cfg)
		if err != nil {
			t.Fatal(err)
		}
		journal, err := beginAntigravityTransaction(antigravityTransactionInstall, nil, &cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := removeExactAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
			t.Fatal(err)
		}
		if err := recoverAntigravityTransaction(cfg.RuntimeConfigRoot); err != nil {
			t.Fatal(err)
		}
		if present, err := antigravitySharedMCPMatches(nil, &cfg); err != nil || !present {
			t.Fatalf("unsafe shared MCP entry remained after recovery: present=%t err=%v", present, err)
		}
		if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("first-install config remained after recovery: %v", err)
		}
		if _, err := os.Lstat(cfg.RuntimePluginSource); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rolled-back first-install source remains: %v", err)
		}
		assertAntigravityTransactionAbsent(t, cfg.RuntimeConfigRoot, journal)
	})

	t.Run("upgrade before atomic exchange", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		previous := installAntigravityFixtureConfig(t, fixture)
		previousBundle, err := verifiedAntigravitySourceBundle(previous)
		if err != nil {
			t.Fatal(err)
		}
		desired, desiredBundle := prepareAntigravityUpgrade(t, fixture, previous)
		journal, err := beginAntigravityTransaction(antigravityTransactionInstall, &previous, &desired)
		if err != nil {
			t.Fatal(err)
		}
		if err := transcriptcapture.SaveConfig(desired); err != nil {
			t.Fatal(err)
		}
		swapPath := antigravityBundleSwapPath(desired.RuntimePluginPath, desiredBundle)
		if err := os.Mkdir(swapPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := populateAntigravityBundleDirectory(swapPath, desiredBundle); err != nil {
			t.Fatal(err)
		}
		if err := recoverAntigravityTransaction(desired.RuntimeConfigRoot); err != nil {
			t.Fatal(err)
		}
		loaded, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
		if err != nil || !equalAntigravityTransactionConfig(loaded, previous) {
			t.Fatalf("previous config was not restored: %#v, %v", loaded, err)
		}
		if err := verifyAntigravityBundleDirectory(previous.RuntimePluginPath, previousBundle); err != nil {
			t.Fatalf("previous plugin was not preserved: %v", err)
		}
		if desired.RuntimePluginSource != previous.RuntimePluginSource {
			if _, err := os.Lstat(desired.RuntimePluginSource); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rolled-back upgrade source remains: %v", err)
			}
		}
		assertAntigravityTransactionAbsent(t, desired.RuntimeConfigRoot, journal)
	})

	t.Run("upgrade after plugin exchange before shared MCP", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		previous := installAntigravityFixtureConfig(t, fixture)
		previousBundle, err := verifiedAntigravitySourceBundle(previous)
		if err != nil {
			t.Fatal(err)
		}
		desired, desiredBundle := prepareAntigravityUpgrade(t, fixture, previous)
		journal, err := beginAntigravityTransaction(antigravityTransactionInstall, &previous, &desired)
		if err != nil {
			t.Fatal(err)
		}
		if err := transcriptcapture.SaveConfig(desired); err != nil {
			t.Fatal(err)
		}
		swapPath := antigravityBundleSwapPath(desired.RuntimePluginPath, desiredBundle)
		if err := os.Mkdir(swapPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := populateAntigravityBundleDirectory(swapPath, desiredBundle); err != nil {
			t.Fatal(err)
		}
		if err := exchangeManagedInstructionFiles(desired.RuntimePluginPath, swapPath); err != nil {
			t.Fatal(err)
		}
		if err := recoverAntigravityTransaction(desired.RuntimeConfigRoot); err != nil {
			t.Fatal(err)
		}
		loaded, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
		if err != nil || !equalAntigravityTransactionConfig(loaded, previous) {
			t.Fatalf("previous config was not restored: %#v, %v", loaded, err)
		}
		if err := verifyAntigravityBundleDirectory(previous.RuntimePluginPath, previousBundle); err != nil {
			t.Fatalf("previous plugin was not restored: %v", err)
		}
		if err := verifyAntigravitySharedMCPState(previous); err != nil {
			t.Fatalf("previous shared MCP entry was not restored: %v", err)
		}
		assertAntigravityTransactionAbsent(t, desired.RuntimeConfigRoot, journal)
	})

	t.Run("upgrade after plugin and shared MCP commit", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		previous := installAntigravityFixtureConfig(t, fixture)
		desired, desiredBundle := prepareAntigravityUpgrade(t, fixture, previous)
		journal, err := beginAntigravityTransaction(antigravityTransactionInstall, &previous, &desired)
		if err != nil {
			t.Fatal(err)
		}
		if err := transcriptcapture.SaveConfig(desired); err != nil {
			t.Fatal(err)
		}
		swapPath := antigravityBundleSwapPath(desired.RuntimePluginPath, desiredBundle)
		if err := os.Mkdir(swapPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := populateAntigravityBundleDirectory(swapPath, desiredBundle); err != nil {
			t.Fatal(err)
		}
		if err := exchangeManagedInstructionFiles(desired.RuntimePluginPath, swapPath); err != nil {
			t.Fatal(err)
		}
		if _, err := convergeAntigravitySharedMCP(&previous, &desired); err != nil {
			t.Fatal(err)
		}
		if err := recoverAntigravityTransaction(desired.RuntimeConfigRoot); err != nil {
			t.Fatal(err)
		}
		loaded, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
		if err != nil || !equalAntigravityTransactionConfig(loaded, desired) {
			t.Fatalf("desired config was not committed: %#v, %v", loaded, err)
		}
		if err := verifyAntigravityBundleDirectory(desired.RuntimePluginPath, desiredBundle); err != nil {
			t.Fatalf("desired plugin was not committed: %v", err)
		}
		if err := verifyAntigravitySharedMCPState(desired); err != nil {
			t.Fatalf("desired shared MCP entry was not committed: %v", err)
		}
		if previous.RuntimePluginSource != desired.RuntimePluginSource {
			if _, err := os.Lstat(previous.RuntimePluginSource); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("superseded source remains after recovery: %v", err)
			}
		}
		assertAntigravityTransactionAbsent(t, desired.RuntimeConfigRoot, journal)
	})

	t.Run("upgrade with partial app-owned scratch", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		previous := installAntigravityFixtureConfig(t, fixture)
		previousBundle, err := verifiedAntigravitySourceBundle(previous)
		if err != nil {
			t.Fatal(err)
		}
		desired, desiredBundle := prepareAntigravityUpgrade(t, fixture, previous)
		journal, err := beginAntigravityTransaction(antigravityTransactionInstall, &previous, &desired)
		if err != nil {
			t.Fatal(err)
		}
		if err := transcriptcapture.SaveConfig(desired); err != nil {
			t.Fatal(err)
		}
		swapPath := antigravityBundleSwapPath(desired.RuntimePluginPath, desiredBundle)
		if err := os.Mkdir(swapPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(swapPath, "partial"), []byte("interrupted\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := recoverAntigravityTransaction(desired.RuntimeConfigRoot); err != nil {
			t.Fatal(err)
		}
		if err := verifyAntigravityBundleDirectory(previous.RuntimePluginPath, previousBundle); err != nil {
			t.Fatalf("previous plugin was not preserved: %v", err)
		}
		if _, err := os.Lstat(swapPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial app-owned scratch remains: %v", err)
		}
		assertAntigravityTransactionAbsent(t, desired.RuntimeConfigRoot, journal)
	})

	t.Run("uninstall after quarantine", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		cfg := installAntigravityFixtureConfig(t, fixture)
		bundle, err := verifiedAntigravitySourceBundle(cfg)
		if err != nil {
			t.Fatal(err)
		}
		journal, err := beginAntigravityTransaction(antigravityTransactionUninstall, &cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := convergeAntigravitySharedMCP(&cfg, nil); err != nil {
			t.Fatal(err)
		}
		removalPath := antigravityBundleRemovalPath(cfg.RuntimePluginPath, bundle)
		if err := renameManagedInstructionFileNoReplace(cfg.RuntimePluginPath, removalPath); err != nil {
			t.Fatal(err)
		}
		if err := recoverAntigravityTransaction(cfg.RuntimeConfigRoot); err != nil {
			t.Fatal(err)
		}
		if err := verifyAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
			t.Fatalf("quarantined plugin was not restored: %v", err)
		}
		if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); err != nil {
			t.Fatalf("installed config was not preserved: %v", err)
		}
		if err := verifyAntigravitySharedMCPState(cfg); err != nil {
			t.Fatalf("shared MCP entry was not restored: %v", err)
		}
		assertAntigravityTransactionAbsent(t, cfg.RuntimeConfigRoot, journal)
	})

	t.Run("uninstall after partial quarantine deletion", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		cfg := installAntigravityFixtureConfig(t, fixture)
		bundle, err := verifiedAntigravitySourceBundle(cfg)
		if err != nil {
			t.Fatal(err)
		}
		journal, err := beginAntigravityTransaction(antigravityTransactionUninstall, &cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := convergeAntigravitySharedMCP(&cfg, nil); err != nil {
			t.Fatal(err)
		}
		removalPath := antigravityBundleRemovalPath(cfg.RuntimePluginPath, bundle)
		if err := renameManagedInstructionFileNoReplace(cfg.RuntimePluginPath, removalPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(removalPath, "rules", "witself.md")); err != nil {
			t.Fatal(err)
		}
		if err := recoverAntigravityTransaction(cfg.RuntimeConfigRoot); err != nil {
			t.Fatal(err)
		}
		if err := verifyAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
			t.Fatalf("plugin was not rebuilt after partial quarantine deletion: %v", err)
		}
		if _, err := os.Lstat(removalPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial removal scratch remains: %v", err)
		}
		if err := verifyAntigravitySharedMCPState(cfg); err != nil {
			t.Fatalf("shared MCP entry was not restored: %v", err)
		}
		assertAntigravityTransactionAbsent(t, cfg.RuntimeConfigRoot, journal)
	})

	t.Run("uninstall after config removal", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		cfg := installAntigravityFixtureConfig(t, fixture)
		bundle, err := verifiedAntigravitySourceBundle(cfg)
		if err != nil {
			t.Fatal(err)
		}
		journal, err := beginAntigravityTransaction(antigravityTransactionUninstall, &cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := convergeAntigravitySharedMCP(&cfg, nil); err != nil {
			t.Fatal(err)
		}
		if err := removeExactAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
			t.Fatal(err)
		}
		if err := transcriptcapture.RemoveConfig(transcriptcapture.RuntimeAntigravity); err != nil {
			t.Fatal(err)
		}
		if code := uninstallCmd([]string{"antigravity"}); code != 0 {
			t.Fatalf("recovering uninstall code = %d", code)
		}
		if _, err := os.Lstat(cfg.RuntimePluginSource); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("completed uninstall source remains: %v", err)
		}
		assertAntigravityTransactionAbsent(t, cfg.RuntimeConfigRoot, journal)
	})

	t.Run("uninstall quiesces exact shared MCP entry recreated after config removal", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		cfg := installAntigravityFixtureConfig(t, fixture)
		bundle, err := verifiedAntigravitySourceBundle(cfg)
		if err != nil {
			t.Fatal(err)
		}
		journal, err := beginAntigravityTransaction(antigravityTransactionUninstall, &cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := convergeAntigravitySharedMCP(&cfg, nil); err != nil {
			t.Fatal(err)
		}
		if err := removeExactAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
			t.Fatal(err)
		}
		if err := transcriptcapture.RemoveConfig(transcriptcapture.RuntimeAntigravity); err != nil {
			t.Fatal(err)
		}
		if _, err := convergeAntigravitySharedMCP(nil, &cfg); err != nil {
			t.Fatal(err)
		}
		if err := recoverAntigravityTransaction(cfg.RuntimeConfigRoot); err != nil {
			t.Fatal(err)
		}
		if absent, err := antigravitySharedMCPMatches(nil, &cfg); err != nil || !absent {
			t.Fatalf("recreated shared MCP entry survived recovery: absent=%t err=%v", absent, err)
		}
		assertAntigravityTransactionAbsent(t, cfg.RuntimeConfigRoot, journal)
	})
}

func TestAntigravityTransactionRecoveryUsesPersistedRootAfterSelectorDrift(t *testing.T) {
	for _, operation := range []string{antigravityTransactionInstall, antigravityTransactionUninstall} {
		t.Run(operation, func(t *testing.T) {
			fixture := setupAntigravityIntegrationFixture(t)
			cfg := installAntigravityFixtureConfig(t, fixture)
			if operation == antigravityTransactionUninstall {
				if _, err := beginAntigravityTransaction(operation, &cfg, nil); err != nil {
					t.Fatal(err)
				}
			} else if _, err := beginAntigravityTransaction(operation, &cfg, &cfg); err != nil {
				t.Fatal(err)
			}

			driftHome := t.TempDir()
			driftHome, err := filepath.EvalSymlinks(driftHome)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", driftHome)
			t.Setenv("USERPROFILE", driftHome)

			recoveryRoot, err := antigravityOperationLockRoot()
			if err != nil {
				t.Fatal(err)
			}
			if recoveryRoot != cfg.RuntimeConfigRoot {
				t.Fatalf("recovery root = %q, want persisted %q", recoveryRoot, cfg.RuntimeConfigRoot)
			}
			release, err := acquireProviderIntegrationOperationLock(transcriptcapture.RuntimeAntigravity)
			if err != nil {
				t.Fatal(err)
			}
			release()
			if _, err := os.Lstat(filepath.Join(cfg.RuntimeConfigRoot, antigravityOperationLockFile)); err != nil {
				t.Fatalf("persisted-root operation lock is missing: %v", err)
			}
			driftRoot := filepath.Join(driftHome, ".gemini", "config")
			if _, err := os.Lstat(filepath.Join(driftRoot, antigravityOperationLockFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("selector-drift root received an operation lock: %v", err)
			}

			if err := recoverAntigravityTransaction(recoveryRoot); err != nil {
				t.Fatalf("recover %s after selector drift: %v", operation, err)
			}
			if _, err := os.Lstat(antigravityTransactionPath(cfg.RuntimeConfigRoot)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("persisted-root journal remains after %s recovery: %v", operation, err)
			}
			installed, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
			if err != nil || !equalAntigravityTransactionConfig(installed, cfg) {
				t.Fatalf("recovered %s config = %#v, %v", operation, installed, err)
			}
		})
	}
}

func TestAntigravityInstallStopsWhenRecoveryChangesLockRootAfterHOMEDrift(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	cfg := installAntigravityFixtureConfig(t, fixture)
	bundle, err := verifiedAntigravitySourceBundle(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beginAntigravityTransaction(antigravityTransactionInstall, nil, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := removeExactAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
		t.Fatal(err)
	}
	if _, err := convergeAntigravitySharedMCP(&cfg, nil); err != nil {
		t.Fatal(err)
	}

	driftHome := t.TempDir()
	driftHome, err = filepath.EvalSymlinks(driftHome)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", driftHome)
	t.Setenv("USERPROFILE", driftHome)

	_, stderr, code := captureIntegrationsCLI(t, func() int {
		return installCmd(fixture.installArgs())
	})
	if code != 1 || !strings.Contains(stderr, "recovery changed the provider lock root") ||
		!strings.Contains(stderr, "rerun install") {
		t.Fatalf("install after root-changing recovery code=%d stderr=%q", code, stderr)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rolled-back first-install config remains: %v", err)
	}
	if _, err := os.Lstat(antigravityTransactionPath(cfg.RuntimeConfigRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rolled-back first-install journal remains: %v", err)
	}
	driftRoot := filepath.Join(driftHome, ".gemini", "config")
	if _, err := os.Lstat(filepath.Join(driftRoot, antigravityOperationLockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("install mutated or locked the new selector root before rerun: %v", err)
	}
}

func TestAntigravitySourceStageRecoversOwnedPartialScratchBeforeJournal(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	previous := installAntigravityFixtureConfig(t, fixture)
	desired := previous
	desired.MCPCommand = filepath.Join(fixture.home, "witself-stage-preview")
	if err := os.WriteFile(desired.MCPCommand, []byte("preview\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	bundle, err := antigravityBundleFromConfig(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired.RuntimePluginDigest = bundle.digest()
	desired.RuntimePluginSource = filepath.Join(
		desired.MCPEnvironment["WITSELF_HOME"], "integrations", "antigravity", "bundles", desired.RuntimePluginDigest,
	)
	if err := os.MkdirAll(filepath.Dir(desired.RuntimePluginSource), 0o700); err != nil {
		t.Fatal(err)
	}
	staged := antigravityBundleSwapPath(desired.RuntimePluginSource, bundle)
	if err := os.Mkdir(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "partial"), []byte("interrupted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stageAntigravitySourceBundle(desired); err != nil {
		t.Fatal(err)
	}
	if err := verifyAntigravityBundleDirectory(desired.RuntimePluginSource, bundle); err != nil {
		t.Fatalf("recovered source bundle is not exact: %v", err)
	}
	if _, err := os.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial pre-journal source scratch remains: %v", err)
	}
}

func TestAntigravityTransactionJournalCannotBeClearedAfterMutation(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	cfg := installAntigravityFixtureConfig(t, fixture)
	journal, err := beginAntigravityTransaction(antigravityTransactionUninstall, &cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := antigravityTransactionPath(cfg.RuntimeConfigRoot)
	mutated := journal
	mutatedPrevious := *mutated.Previous
	mutatedPrevious.Endpoint = "https://foreign.invalid"
	mutated.Previous = &mutatedPrevious
	raw, err := json.MarshalIndent(mutated, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearAntigravityTransaction(cfg.RuntimeConfigRoot, journal); err == nil {
		t.Fatal("mutated transaction journal was cleared")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("mutated transaction journal was removed: %v", err)
	}
}

func TestAntigravityRecoveryRejectsMutatedJournalShape(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	cfg := installAntigravityFixtureConfig(t, fixture)
	bundle, err := verifiedAntigravitySourceBundle(cfg)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := beginAntigravityTransaction(antigravityTransactionUninstall, &cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	mutated := journal
	desired := cfg
	mutated.Desired = &desired
	raw, err := json.MarshalIndent(mutated, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(antigravityTransactionPath(cfg.RuntimeConfigRoot), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverAntigravityTransaction(cfg.RuntimeConfigRoot); err == nil {
		t.Fatal("mutated uninstall journal shape was recovered")
	}
	if err := verifyAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
		t.Fatalf("mutated journal changed the live plugin: %v", err)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); err != nil {
		t.Fatalf("mutated journal changed the config: %v", err)
	}
}

func TestAntigravityTransactionJournalRejectsUnknownFieldsAndCLITampering(t *testing.T) {
	for _, variant := range []string{"unknown", "cli"} {
		t.Run(variant, func(t *testing.T) {
			fixture := setupAntigravityIntegrationFixture(t)
			previous := installAntigravityFixtureConfig(t, fixture)
			desired := previous
			if _, err := beginAntigravityTransaction(antigravityTransactionInstall, &previous, &desired); err != nil {
				t.Fatal(err)
			}
			path := antigravityTransactionPath(previous.RuntimeConfigRoot)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			if variant == "unknown" {
				document["foreign"] = true
			} else {
				desiredDocument := document["desired"].(map[string]any)
				desiredDocument["runtime_cli_command"] = filepath.Join(fixture.home, "foreign-agy")
			}
			raw, err = json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadAntigravityTransactionJournal(previous.RuntimeConfigRoot); err == nil {
				t.Fatalf("%s journal tampering was accepted", variant)
			}
		})
	}
}

func TestAntigravityRecordedPolicyArtifactSurvivesBinaryPolicyChanges(t *testing.T) {
	t.Run("upgrade", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		legacy := installLegacyAntigravityPolicyConfig(t, fixture)
		if code := installCmd(fixture.installArgs()); code != 0 {
			t.Fatalf("upgrade legacy policy code = %d", code)
		}
		upgraded, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
		if err != nil {
			t.Fatal(err)
		}
		if upgraded.RuntimePluginDigest == legacy.RuntimePluginDigest {
			t.Fatal("legacy policy digest was not upgraded")
		}
		if err := validateAntigravityInstalledTopology(upgraded); err != nil {
			t.Fatalf("upgraded topology is invalid: %v", err)
		}
		if _, err := os.Lstat(legacy.RuntimePluginSource); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy recovery artifact remains after upgrade: %v", err)
		}
	})

	t.Run("uninstall without old generator", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		legacy := installLegacyAntigravityPolicyConfig(t, fixture)
		if err := os.Remove(fixture.cli); err != nil {
			t.Fatal(err)
		}
		if code := uninstallCmd([]string{"antigravity"}); code != 0 {
			t.Fatalf("uninstall legacy policy code = %d", code)
		}
		if _, err := os.Lstat(legacy.RuntimePluginPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy plugin remains after uninstall: %v", err)
		}
		if _, err := os.Lstat(legacy.RuntimePluginSource); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy recovery artifact remains after uninstall: %v", err)
		}
	})
}

func TestAntigravityLegacyPluginMCPMigratesAndUninstalls(t *testing.T) {
	t.Run("reinstall migrates to canonical shared MCP", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		legacy := installLegacyAntigravityPluginMCPConfig(t, fixture)
		canonicalPath := filepath.Join(fixture.configRoot(), "mcp_config.json")
		if err := os.WriteFile(canonicalPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(canonicalPath, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(legacy.RuntimePluginPath, "mcp_config.json")); err != nil {
			t.Fatalf("legacy plugin MCP is missing: %v", err)
		}
		if code := installCmd(fixture.installArgs()); code != 0 {
			t.Fatalf("legacy migration code = %d", code)
		}
		migrated, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
		if err != nil {
			t.Fatal(err)
		}
		if migrated.RuntimeMCPConfigPath != filepath.Join(fixture.configRoot(), "mcp_config.json") {
			t.Fatalf("migrated MCP config path = %q", migrated.RuntimeMCPConfigPath)
		}
		if _, err := os.Lstat(filepath.Join(migrated.RuntimePluginPath, "mcp_config.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("migrated plugin still contains MCP config: %v", err)
		}
		if err := validateAntigravityInstalledTopology(migrated); err != nil {
			t.Fatalf("migrated topology is invalid: %v", err)
		}
		if legacy.RuntimePluginSource != migrated.RuntimePluginSource {
			if _, err := os.Lstat(legacy.RuntimePluginSource); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("legacy source remains after migration: %v", err)
			}
		}
	})

	t.Run("legacy direct uninstall needs no shared entry or provider CLI", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		legacy := installLegacyAntigravityPluginMCPConfig(t, fixture)
		canonicalPath := filepath.Join(fixture.configRoot(), "mcp_config.json")
		if err := os.WriteFile(canonicalPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(canonicalPath, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(fixture.cli); err != nil {
			t.Fatal(err)
		}
		if code := uninstallCmd([]string{"agy"}); code != 0 {
			t.Fatalf("legacy direct uninstall code = %d", code)
		}
		if _, err := os.Lstat(legacy.RuntimePluginPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy plugin remains after uninstall: %v", err)
		}
		raw, err := os.ReadFile(canonicalPath)
		if err != nil || len(raw) != 0 {
			t.Fatalf("legacy uninstall changed preexisting shared MCP config: %q, %v", raw, err)
		}
		info, err := os.Lstat(canonicalPath)
		if err != nil {
			t.Fatalf("inspect legacy shared MCP config after uninstall: %v", err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("legacy uninstall changed preexisting shared MCP mode: %v", info.Mode())
		}
	})
}

func installAntigravityFixtureConfig(t *testing.T, fixture antigravityIntegrationFixture) transcriptcapture.Config {
	t.Helper()
	if code := installCmd(fixture.installArgs()); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAntigravityLocalReadsAreBoundedAndStrict(t *testing.T) {
	t.Run("oversized import manifest", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		if err := os.MkdirAll(fixture.configRoot(), 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(fixture.configRoot(), "import_manifest.json")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(antigravityManifestReadLimit + 1); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if err := rejectAntigravityManifestCollision(fixture.configRoot(), fixture.pluginName(t)); err == nil ||
			!strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized manifest error = %v", err)
		}
	})

	t.Run("duplicate import manifest keys", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		if err := os.MkdirAll(fixture.configRoot(), 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(fixture.configRoot(), "import_manifest.json")
		if err := os.WriteFile(path, []byte(`{"imports":[],"imports":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := rejectAntigravityManifestCollision(fixture.configRoot(), fixture.pluginName(t)); err == nil ||
			!strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("duplicate manifest key error = %v", err)
		}
	})

	t.Run("oversized owned plugin file", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		cfg := installAntigravityFixtureConfig(t, fixture)
		path := filepath.Join(cfg.RuntimePluginPath, "plugin.json")
		if err := os.Truncate(path, antigravityPluginFileReadLimit+1); err != nil {
			t.Fatal(err)
		}
		if _, err := readAntigravityBundleDirectory(cfg.RuntimePluginPath); err == nil ||
			!strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized plugin file error = %v", err)
		}
	})
}

func prepareAntigravityUpgrade(t *testing.T, fixture antigravityIntegrationFixture, previous transcriptcapture.Config) (transcriptcapture.Config, antigravityPluginBundle) {
	t.Helper()
	desired := previous
	desired.MCPCommand = filepath.Join(fixture.home, "witself-upgrade")
	if err := os.WriteFile(desired.MCPCommand, []byte("upgrade preview\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	bundle, err := antigravityBundleFromConfig(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired.RuntimePluginDigest = bundle.digest()
	desired.RuntimePluginSource = filepath.Join(
		desired.MCPEnvironment["WITSELF_HOME"], "integrations", "antigravity", "bundles", desired.RuntimePluginDigest,
	)
	if err := stageAntigravitySourceBundle(desired); err != nil {
		t.Fatal(err)
	}
	return desired, bundle
}

func installLegacyAntigravityPolicyConfig(t *testing.T, fixture antigravityIntegrationFixture) transcriptcapture.Config {
	t.Helper()
	cfg := installAntigravityFixtureConfig(t, fixture)
	current, err := verifiedAntigravitySourceBundle(cfg)
	if err != nil {
		t.Fatal(err)
	}
	legacy := antigravityPluginBundle{files: make(map[string][]byte, len(current.files))}
	for path, raw := range current.files {
		legacy.files[path] = append([]byte(nil), raw...)
	}
	legacy.files["rules/witself.md"] = append(legacy.files["rules/witself.md"], []byte("\n<!-- legacy policy revision -->\n")...)
	legacyDigest := legacy.digest()
	legacySource := filepath.Join(cfg.MCPEnvironment["WITSELF_HOME"], "integrations", "antigravity", "bundles", legacyDigest)
	if err := installAntigravityBundleDirectory(legacySource, legacy, nil); err != nil {
		t.Fatal(err)
	}
	if err := installAntigravityBundleDirectory(cfg.RuntimePluginPath, legacy, &current); err != nil {
		t.Fatal(err)
	}
	currentSource := cfg.RuntimePluginSource
	cfg.RuntimePluginDigest = legacyDigest
	cfg.RuntimePluginSource = legacySource
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := removeExactAntigravityBundleDirectory(currentSource, current); err != nil {
		t.Fatal(err)
	}
	if _, err := verifiedAntigravitySourceBundle(cfg); err != nil {
		t.Fatalf("legacy recorded artifact is not self-validating: %v", err)
	}
	return cfg
}

func installLegacyAntigravityPluginMCPConfig(t *testing.T, fixture antigravityIntegrationFixture) transcriptcapture.Config {
	t.Helper()
	current := installAntigravityFixtureConfig(t, fixture)
	currentBundle, err := verifiedAntigravitySourceBundle(current)
	if err != nil {
		t.Fatal(err)
	}
	legacy := current
	legacy.RuntimeMCPConfigPath = ""
	legacyBundle, err := antigravityBundleFromConfig(legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacy.RuntimePluginDigest = legacyBundle.digest()
	legacy.RuntimePluginSource = filepath.Join(
		legacy.MCPEnvironment["WITSELF_HOME"], "integrations", "antigravity", "bundles", legacy.RuntimePluginDigest,
	)
	if err := stageAntigravitySourceBundle(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := convergeAntigravitySharedMCP(&current, nil); err != nil {
		t.Fatal(err)
	}
	if err := installAntigravityBundleDirectory(legacy.RuntimePluginPath, legacyBundle, &currentBundle); err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(legacy); err != nil {
		t.Fatal(err)
	}
	if current.RuntimePluginSource != legacy.RuntimePluginSource {
		if err := removeExactAntigravityBundleDirectory(current.RuntimePluginSource, currentBundle); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateAntigravityInstalledTopology(legacy); err != nil {
		t.Fatalf("legacy plugin-MCP topology is invalid: %v", err)
	}
	return legacy
}

func assertAntigravityTransactionAbsent(t *testing.T, configRoot string, journal antigravityTransactionJournal) {
	t.Helper()
	paths := []string{
		antigravityTransactionPath(configRoot),
		antigravitySharedMCPScratchPathForJournal(configRoot, journal),
		antigravitySharedMCPStagePath(antigravitySharedMCPScratchPathForJournal(configRoot, journal)),
		antigravitySharedMCPMutationPath(configRoot, journal),
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Antigravity transaction artifact remains at %s: %v", path, err)
		}
	}
}

func TestAntigravityFlagsAndHookDependentWorkflowsFailClosed(t *testing.T) {
	if code := installCmd([]string{"antigravity", "--routing-only"}); code != 2 {
		t.Fatalf("routing-only code = %d", code)
	}
	for _, args := range [][]string{{"--capture", "raw"}, {"--managed-hooks"}, {"--user-hooks"}} {
		if code := installCmd(append([]string{"agy"}, args...)); code != 2 {
			t.Errorf("%v code = %d", args, code)
		}
	}
	if code := memoryAcceptancePrepare([]string{"--runtime", "antigravity", "--peer-agent", "peer"}); code != 2 {
		t.Fatalf("acceptance prepare code = %d", code)
	}
	if _, code := parseMemoryCurateAutoRuntime("status", []string{"--runtime", "agy"}, false); code != 2 {
		t.Fatalf("automatic curation parse code = %d", code)
	}
}

func TestAntigravityMCPInstructionsUseRuntimeVisibleToolPrefix(t *testing.T) {
	serverName := "ws-0123456789abcdef"
	prefix := "mcp_" + serverName + "_"
	for _, instructions := range []string{
		antigravityMCPInstructions(mcpInstructionsForMode(
			transcriptcapture.RuntimeAntigravity,
			"witself.self.show",
			"witself.message.list",
			false,
		), serverName),
		antigravityMCPInstructions(mcpInstructionsForMode(
			transcriptcapture.RuntimeAntigravity,
			"witself.self.show",
			"witself.message.list",
			true,
		), serverName),
	} {
		for _, name := range []string{
			prefix + "witself.self.show",
			prefix + "witself.memory.recall",
		} {
			if !strings.Contains(instructions, name) {
				t.Errorf("Antigravity MCP instructions omit %s: %s", name, instructions)
			}
		}
		if strings.Contains(instructions, "`witself.") {
			t.Errorf("Antigravity MCP instructions retain bare model-visible tool name: %s", instructions)
		}
	}
	curator := antigravityMCPInstructions("Use `witself.memory.curation.get` and `witself.memory.curation.plan.get`.", serverName)
	if !strings.Contains(curator, prefix+"witself.memory.curation.get") || strings.Contains(curator, "`witself.") {
		t.Errorf("Antigravity curator instructions use the wrong tool namespace: %s", curator)
	}
}

func TestAntigravityFirstCreationUsesAtomicNoReplaceRename(t *testing.T) {
	parent := t.TempDir()
	stage := filepath.Join(parent, "stage")
	destination := filepath.Join(parent, "witself")
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "stage-marker"), []byte("stage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(destination, "foreign-marker")
	if err := os.WriteFile(foreign, []byte("foreign\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := renameManagedInstructionFileNoReplace(stage, destination); err == nil {
		t.Fatal("no-replace rename replaced a destination that appeared at the boundary")
	}
	if raw, err := os.ReadFile(foreign); err != nil || string(raw) != "foreign\n" {
		t.Fatalf("racing destination changed: %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(filepath.Join(stage, "stage-marker")); err != nil || string(raw) != "stage\n" {
		t.Fatalf("staged source changed after no-replace failure: %q, %v", raw, err)
	}
}

func TestAntigravityOperationLockSerializesMutations(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	firstRelease, err := acquireAntigravityOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	if release, err := acquireAntigravityOperationLock(); err == nil {
		release()
		t.Fatal("second Antigravity operation acquired the live lock")
	}
	firstRelease()
	thirdRelease, err := acquireAntigravityOperationLock()
	if err != nil {
		t.Fatalf("lock remained held after release: %v", err)
	}
	thirdRelease()
	path := filepath.Join(fixture.configRoot(), ".witself-antigravity-operation.lock")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("operation lock mode = %v", info.Mode())
	}

	t.Run("install command", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		release, err := acquireAntigravityOperationLock()
		if err != nil {
			t.Fatal(err)
		}
		if code := installCmd(fixture.installArgs()); code != 1 {
			t.Fatalf("install under held lock code = %d", code)
		}
		release()
		if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("held-lock install changed config: %v", err)
		}
		if _, err := os.Lstat(fixture.pluginPath(t)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("held-lock install changed plugin: %v", err)
		}
	})

	t.Run("uninstall command", func(t *testing.T) {
		fixture := setupAntigravityIntegrationFixture(t)
		if code := installCmd(fixture.installArgs()); code != 0 {
			t.Fatalf("install code = %d", code)
		}
		cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
		if err != nil {
			t.Fatal(err)
		}
		bundle, err := verifiedAntigravitySourceBundle(cfg)
		if err != nil {
			t.Fatal(err)
		}
		release, err := acquireAntigravityOperationLock()
		if err != nil {
			t.Fatal(err)
		}
		if code := uninstallCmd([]string{"antigravity"}); code != 1 {
			t.Fatalf("uninstall under held lock code = %d", code)
		}
		release()
		if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); err != nil {
			t.Fatalf("held-lock uninstall changed config: %v", err)
		}
		if err := verifyAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
			t.Fatalf("held-lock uninstall changed plugin: %v", err)
		}
	})
}

func containsOrderedStrings(values, expected []string) bool {
	if len(values) != len(expected) {
		return false
	}
	for index := range values {
		if values[index] != expected[index] {
			return false
		}
	}
	return true
}

func assertNoAntigravityScratchDirectories(t *testing.T, configRoot string) {
	t.Helper()
	entries, err := os.ReadDir(configRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".witself-plugin-") ||
			strings.HasPrefix(entry.Name(), ".witself-antigravity-mcp-") {
			t.Errorf("Antigravity scratch directory remains live: %s", entry.Name())
		}
	}
}
