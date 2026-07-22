package main

import (
	"bytes"
	"context"
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

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type openClawIntegrationFixture struct {
	home       string
	workspace  string
	witself    string
	cli        string
	state      string
	log        string
	token      string
	serverURL  string
	agentsJSON string
	stateDir   string
	configPath string
	profile    string
}

func setupOpenClawIntegrationFixture(t *testing.T) openClawIntegrationFixture {
	t.Helper()
	home := t.TempDir()
	workspace := filepath.Join(home, "custom-main-workspace")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixture := openClawIntegrationFixture{
		home: home, workspace: workspace, witself: executable,
		cli: filepath.Join(home, "openclaw"), state: filepath.Join(home, "openclaw-mcp.json"),
		log: filepath.Join(home, "openclaw-args.log"), token: filepath.Join(home, "agent.token"),
		stateDir: filepath.Join(home, "openclaw-state"), configPath: filepath.Join(home, "openclaw-state", "openclaw.json"),
		profile: "integration-test",
	}
	agents, err := json.Marshal([]openClawAgent{{ID: "main", Workspace: workspace, IsDefault: true}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.agentsJSON = string(agents)
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("OPENCLAW_STATE_DIR", fixture.stateDir)
	t.Setenv("OPENCLAW_CONFIG_PATH", fixture.configPath)
	t.Setenv("OPENCLAW_PROFILE", fixture.profile)
	for _, key := range openClawUnsupportedSelectorEnvironment {
		t.Setenv(key, "")
	}
	t.Setenv("OPENCLAW_CLI_PATH", fixture.cli)
	t.Setenv(witselfExecutableTestEnv, executable)
	t.Setenv("FAKE_OPENCLAW_AGENTS", fixture.agentsJSON)
	t.Setenv("FAKE_OPENCLAW_STATE", fixture.state)
	t.Setenv("FAKE_OPENCLAW_LOG", fixture.log)
	t.Setenv("FAKE_OPENCLAW_EXPECT_WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("FAKE_OPENCLAW_EXPECT_STATE_DIR", fixture.stateDir)
	t.Setenv("FAKE_OPENCLAW_EXPECT_CONFIG_PATH", fixture.configPath)
	t.Setenv("FAKE_OPENCLAW_EXPECT_PROFILE", fixture.profile)

	script := `#!/bin/sh
if [ "$WITSELF_HOME" != "$FAKE_OPENCLAW_EXPECT_WITSELF_HOME" ] || [ "$OPENCLAW_STATE_DIR" != "$FAKE_OPENCLAW_EXPECT_STATE_DIR" ] || [ "$OPENCLAW_CONFIG_PATH" != "$FAKE_OPENCLAW_EXPECT_CONFIG_PATH" ] || [ "$OPENCLAW_PROFILE" != "$FAKE_OPENCLAW_EXPECT_PROFILE" ]; then
  printf '%s\n' 'unexpected OpenClaw selector environment' >&2
  exit 11
fi
printf '%s\n' "$*" >> "$FAKE_OPENCLAW_LOG"
if [ "$1" = "--version" ]; then
  printf '%s\n' 'OpenClaw 2026.7.1-2 (test)'
  exit 0
fi
if [ "$1 $2 $3" = "mcp add --help" ]; then
  exit 0
fi
if [ "$1 $2 $3" = "agents list --json" ]; then
  printf '%s\n' "$FAKE_OPENCLAW_AGENTS"
  exit 0
fi
if [ "$1 $2 $3" = "mcp list --json" ]; then
  if [ -f "$FAKE_OPENCLAW_STATE" ]; then
    if [ -n "$FAKE_OPENCLAW_VERIFY_MISMATCH_ONCE" ] && [ ! -f "$FAKE_OPENCLAW_VERIFY_MISMATCH_ONCE" ]; then
      printf '%s\n' 'used' > "$FAKE_OPENCLAW_VERIFY_MISMATCH_ONCE"
      printf '%s\n' '{"witself":{"command":"/foreign/witself","args":[]}}'
      exit 0
    fi
    /bin/cat "$FAKE_OPENCLAW_STATE"
  else
    printf '%s\n' '{}'
  fi
  exit 0
fi
if [ "$1 $2 $3" = "mcp add witself" ]; then
  if [ -f "$FAKE_OPENCLAW_STATE" ]; then
    printf '%s\n' 'witself already exists' >&2
    exit 9
  fi
  printf '%s\n' "$FAKE_OPENCLAW_BINDING" > "$FAKE_OPENCLAW_STATE"
  exit 0
fi
if [ "$1 $2 $3" = "mcp unset witself" ]; then
  if [ ! -f "$FAKE_OPENCLAW_STATE" ]; then
    printf '%s\n' 'No MCP server named "witself"' >&2
    exit 1
  fi
  if [ -n "$FAKE_OPENCLAW_REQUIRE_ROUTING" ] && ! /usr/bin/grep -q 'BEGIN WITSELF MANAGED OPENCLAW ROUTING' "$FAKE_OPENCLAW_REQUIRE_ROUTING"; then
    printf '%s\n' 'managed OpenClaw routing was removed before MCP' >&2
    exit 10
  fi
  if [ -n "$FAKE_OPENCLAW_UNSET_FAIL" ]; then
    printf '%s\n' 'simulated MCP unset failure' >&2
    exit 12
  fi
  /bin/rm -f "$FAKE_OPENCLAW_STATE"
  if [ -n "$FAKE_OPENCLAW_BREAK_ROUTING" ]; then
    printf '%s\n' '<!-- BEGIN WITSELF MANAGED OPENCLAW ROUTING -->' > "$FAKE_OPENCLAW_BREAK_ROUTING"
  fi
  exit 0
fi
exit 2
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
	fixture.setExpectedBinding(t, "scott")
	return fixture
}

func (fixture openClawIntegrationFixture) setExpectedBinding(t *testing.T, agent string) {
	t.Helper()
	binding, err := openClawMCPBindingFromServeArgs(runtimeMCPServeArgs(
		transcriptcapture.RuntimeOpenClaw,
		fixture.witself,
		"default",
		"default",
		agent,
		"home",
	))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]openClawMCPBinding{"witself": binding})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OPENCLAW_BINDING", string(raw))
}

func (fixture openClawIntegrationFixture) mcpEnvironment(t *testing.T) map[string]string {
	t.Helper()
	environment, err := captureOpenClawMCPEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	return environment
}

func (fixture openClawIntegrationFixture) installArgs() []string {
	return []string{
		"openclaw", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--endpoint", fixture.serverURL, "--token-file", fixture.token,
	}
}

func TestOpenClawInstallReinstallAndUninstallLifecycle(t *testing.T) {
	fixture := setupOpenClawIntegrationFixture(t)
	if err := os.MkdirAll(fixture.workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	agentsPath := filepath.Join(fixture.workspace, "AGENTS.md")
	original := []byte("# Existing OpenClaw guidance\n")
	if err := os.WriteFile(agentsPath, original, 0o640); err != nil {
		t.Fatal(err)
	}

	if code := installCmd(fixture.installArgs()); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HookMode != transcriptcapture.HookModeNone || cfg.RuntimeCLICommand != fixture.cli || cfg.MCPCommand != fixture.witself ||
		cfg.RuntimeWorkspace != fixture.workspace || cfg.RuntimeAgentID != "main" ||
		cfg.RuntimeVersion != "2026.7.1-2" || cfg.MCPConnectTimeoutSeconds != openClawMCPConnectTimeoutSeconds ||
		!equalOpenClawMCPEnvironment(cfg.MCPEnvironment, fixture.mcpEnvironment(t)) {
		t.Fatalf("OpenClaw config = %#v", cfg)
	}
	if _, err := os.Stat(filepath.Join(fixture.home, ".openclaw", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("OpenClaw hook file exists: %v", err)
	}
	installedInstructions, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(installedInstructions, openClawMemoryRoutingBlock) || !bytes.HasSuffix(installedInstructions, original) {
		t.Fatalf("managed OpenClaw AGENTS.md = %q", installedInstructions)
	}
	if code := installCmd(fixture.installArgs()); code != 0 {
		t.Fatalf("idempotent reinstall code = %d", code)
	}
	reinstalledInstructions, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reinstalledInstructions, installedInstructions) {
		t.Fatal("idempotent reinstall changed AGENTS.md")
	}
	for _, drift := range []struct {
		key      string
		value    string
		restored string
	}{
		{key: "OPENCLAW_STATE_DIR", value: filepath.Join(fixture.home, "drifted-openclaw-state"), restored: fixture.stateDir},
		{key: "OPENCLAW_CONFIG_PATH", value: filepath.Join(fixture.home, "drifted-openclaw.json"), restored: fixture.configPath},
		{key: "OPENCLAW_PROFILE", value: "drifted-profile", restored: fixture.profile},
	} {
		t.Setenv(drift.key, drift.value)
		if code := installCmd(fixture.installArgs()); code != 1 {
			t.Fatalf("%s drift reinstall code = %d", drift.key, code)
		}
		afterSelectorDrift, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw)
		if err != nil {
			t.Fatal(err)
		}
		if !equalOpenClawMCPEnvironment(afterSelectorDrift.MCPEnvironment, cfg.MCPEnvironment) {
			t.Fatalf("%s drift reinstall replaced environment with %#v", drift.key, afterSelectorDrift.MCPEnvironment)
		}
		t.Setenv(drift.key, drift.restored)
	}
	otherCLI := filepath.Join(fixture.home, "other-openclaw")
	cliScript, err := os.ReadFile(fixture.cli)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherCLI, cliScript, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCLAW_CLI_PATH", otherCLI)
	if code := installCmd(fixture.installArgs()); code != 1 {
		t.Fatalf("changed-CLI reinstall code = %d", code)
	}
	afterCLIDrift, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw)
	if err != nil {
		t.Fatal(err)
	}
	if afterCLIDrift.RuntimeCLICommand != fixture.cli {
		t.Fatalf("changed-CLI reinstall replaced pin with %q", afterCLIDrift.RuntimeCLICommand)
	}
	t.Setenv("OPENCLAW_CLI_PATH", fixture.cli)
	log, err := os.ReadFile(fixture.log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "mcp add witself --command") != 1 ||
		!strings.Contains(string(log), "--no-probe --connect-timeout 60") ||
		!strings.Contains(string(log), "--env OPENCLAW_CONFIG_PATH="+fixture.configPath) ||
		!strings.Contains(string(log), "--env OPENCLAW_PROFILE="+fixture.profile) ||
		!strings.Contains(string(log), "--env OPENCLAW_STATE_DIR="+fixture.stateDir) ||
		!strings.Contains(string(log), "--env WITSELF_HOME="+filepath.Join(fixture.home, ".witself")) ||
		!strings.Contains(string(log), "--arg mcp --arg serve --arg --runtime --arg openclaw") {
		t.Fatalf("OpenClaw CLI log = %s", log)
	}

	// Teardown uses the persisted workspace and exact MCP binding, so it remains
	// safe even when OpenClaw gains another agent after installation.
	agents, err := json.Marshal([]openClawAgent{
		{ID: "main", Workspace: fixture.workspace, IsDefault: true},
		{ID: "other", Workspace: filepath.Join(fixture.home, "other"), IsDefault: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OPENCLAW_AGENTS", string(agents))
	t.Setenv("FAKE_OPENCLAW_REQUIRE_ROUTING", agentsPath)
	if code := uninstallCmd([]string{"openclaw"}); code != 0 {
		t.Fatalf("uninstall code = %d", code)
	}
	if _, err := os.Stat(fixture.state); !os.IsNotExist(err) {
		t.Fatalf("MCP state remains: %v", err)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw); !os.IsNotExist(err) &&
		(err == nil || !strings.Contains(err.Error(), "no such file")) {
		t.Fatalf("integration config remains: %v", err)
	}
	removedInstructions, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(removedInstructions, original) {
		t.Fatalf("uninstalled AGENTS.md = %q", removedInstructions)
	}
}

func TestOpenClawInstallRejectsHookFlagsMultipleAgentsAndForeignMCP(t *testing.T) {
	t.Run("hook flag", func(t *testing.T) {
		fixture := setupOpenClawIntegrationFixture(t)
		if code := installCmd(append(fixture.installArgs(), "--capture", "raw")); code != 2 {
			t.Fatalf("explicit capture code = %d", code)
		}
	})

	t.Run("multiple agents", func(t *testing.T) {
		fixture := setupOpenClawIntegrationFixture(t)
		agents, err := json.Marshal([]openClawAgent{
			{ID: "main", Workspace: fixture.workspace, IsDefault: true},
			{ID: "other", Workspace: filepath.Join(fixture.home, "other"), IsDefault: false},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("FAKE_OPENCLAW_AGENTS", string(agents))
		if code := installCmd(fixture.installArgs()); code != 1 {
			t.Fatalf("multiple-agent install code = %d", code)
		}
	})

	t.Run("foreign MCP", func(t *testing.T) {
		fixture := setupOpenClawIntegrationFixture(t)
		if err := os.MkdirAll(fixture.workspace, 0o700); err != nil {
			t.Fatal(err)
		}
		routingPath := filepath.Join(fixture.workspace, "AGENTS.md")
		originalRouting := []byte("# Foreign-owned workspace\n")
		if err := os.WriteFile(routingPath, originalRouting, 0o600); err != nil {
			t.Fatal(err)
		}
		oldTime := time.Unix(1_700_000_000, 0)
		if err := os.Chtimes(routingPath, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
		foreign := []byte(`{"witself":{"command":"/usr/local/bin/foreign","args":["serve"]}}`)
		if err := os.WriteFile(fixture.state, foreign, 0o600); err != nil {
			t.Fatal(err)
		}
		if code := installCmd(fixture.installArgs()); code != 1 {
			t.Fatalf("foreign-MCP install code = %d", code)
		}
		got, err := os.ReadFile(fixture.state)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, foreign) {
			t.Fatalf("foreign MCP registration changed: %s", got)
		}
		if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw); err == nil {
			t.Fatal("failed install retained an integration config")
		}
		routingInfo, err := os.Stat(routingPath)
		if err != nil {
			t.Fatal(err)
		}
		if !routingInfo.ModTime().Equal(oldTime) {
			t.Fatalf("foreign MCP preflight touched routing file; mtime = %s", routingInfo.ModTime())
		}
	})

	t.Run("unowned exact MCP", func(t *testing.T) {
		fixture := setupOpenClawIntegrationFixture(t)
		if err := os.WriteFile(fixture.state, []byte(os.Getenv("FAKE_OPENCLAW_BINDING")), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := installCmd(fixture.installArgs()); code != 1 {
			t.Fatalf("unowned exact MCP install code = %d", code)
		}
	})

	t.Run("unsupported selector", func(t *testing.T) {
		fixture := setupOpenClawIntegrationFixture(t)
		t.Setenv("OPENCLAW_HOME", filepath.Join(fixture.home, "unsupported-home"))
		if code := installCmd(fixture.installArgs()); code != 1 {
			t.Fatalf("unsupported-selector install code = %d", code)
		}
		if _, err := os.Stat(fixture.state); !os.IsNotExist(err) {
			t.Fatalf("unsupported-selector install changed MCP state: %v", err)
		}
		if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw); err == nil {
			t.Fatal("unsupported-selector install retained an integration config")
		}
	})
}

func TestOpenClawProfileOnlyDerivesAndPersistsProfileNamespace(t *testing.T) {
	fixture := setupOpenClawIntegrationFixture(t)
	t.Setenv("OPENCLAW_STATE_DIR", "")
	t.Setenv("OPENCLAW_CONFIG_PATH", "")
	derivedState := filepath.Join(fixture.home, ".openclaw-"+fixture.profile)
	derivedConfig := filepath.Join(derivedState, "openclaw.json")
	t.Setenv("FAKE_OPENCLAW_EXPECT_STATE_DIR", derivedState)
	t.Setenv("FAKE_OPENCLAW_EXPECT_CONFIG_PATH", derivedConfig)
	fixture.setExpectedBinding(t, "scott")
	if code := installCmd(fixture.installArgs()); code != 0 {
		t.Fatalf("profile-only install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPEnvironment["OPENCLAW_PROFILE"] != fixture.profile || cfg.MCPEnvironment["OPENCLAW_STATE_DIR"] != derivedState || cfg.MCPEnvironment["OPENCLAW_CONFIG_PATH"] != derivedConfig {
		t.Fatalf("profile-only persisted environment = %#v", cfg.MCPEnvironment)
	}
	log, err := os.ReadFile(fixture.log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "--env OPENCLAW_STATE_DIR="+derivedState) || !strings.Contains(string(log), "--env OPENCLAW_CONFIG_PATH="+derivedConfig) {
		t.Fatalf("profile-only OpenClaw CLI log = %s", log)
	}
}

func TestOpenClawInstallRecoversAfterFirstInstallInterruption(t *testing.T) {
	fixture := setupOpenClawIntegrationFixture(t)
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	staged := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeOpenClaw, RuntimeVersion: "2026.7.1-2",
		RuntimeCLICommand: fixture.cli, MCPCommand: fixture.witself, MCPEnvironment: fixture.mcpEnvironment(t), MCPConnectTimeoutSeconds: 60,
		RuntimeWorkspace: fixture.workspace, RuntimeAgentID: "main",
		CaptureMode: transcriptcapture.ModeRaw, HookMode: transcriptcapture.HookModeNone,
		Account: "default", AccountID: "acc_1", Realm: "default", RealmID: "realm_1",
		Agent: "scott", AgentID: "agent_1", AgentName: "scott",
		Endpoint: fixture.serverURL, TokenFile: fixture.token, Location: location,
	}
	if err := transcriptcapture.SaveConfig(staged); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.state, []byte(os.Getenv("FAKE_OPENCLAW_BINDING")), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := installCmd(fixture.installArgs()); code != 0 {
		t.Fatalf("recovery install code = %d", code)
	}
	log, err := os.ReadFile(fixture.log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "mcp add witself --command") || strings.Contains(string(log), "mcp unset witself") {
		t.Fatalf("recovery changed an already exact MCP registration:\n%s", log)
	}
}

func TestOpenClawFirstInstallRollbackPreservesPolicyAndRecoveryStateWhenMCPRemovalFails(t *testing.T) {
	fixture := setupOpenClawIntegrationFixture(t)
	if err := os.MkdirAll(fixture.workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	routingPath := filepath.Join(fixture.workspace, "AGENTS.md")
	original := []byte("# Existing policy\n")
	if err := os.WriteFile(routingPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OPENCLAW_VERIFY_MISMATCH_ONCE", filepath.Join(fixture.home, "verification-mismatch-used"))
	t.Setenv("FAKE_OPENCLAW_UNSET_FAIL", "1")
	if code := installCmd(fixture.installArgs()); code != 1 {
		t.Fatalf("failed-verification install code = %d", code)
	}
	if _, err := os.Stat(fixture.state); err != nil {
		t.Fatalf("persisted MCP state was unexpectedly removed: %v", err)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw)
	if err != nil {
		t.Fatalf("recovery integration record was removed: %v", err)
	}
	if cfg.MCPConnectTimeoutSeconds != openClawMCPConnectTimeoutSeconds || !equalOpenClawMCPEnvironment(cfg.MCPEnvironment, fixture.mcpEnvironment(t)) {
		t.Fatalf("recovery integration record = %#v", cfg)
	}
	routing, err := os.ReadFile(routingPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(routing, openClawMemoryRoutingBlock) || !bytes.HasSuffix(routing, original) {
		t.Fatalf("managed routing was removed while MCP remained live: %q", routing)
	}
	log, err := os.ReadFile(fixture.log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "mcp unset witself") {
		t.Fatalf("rollback never attempted exact MCP removal:\n%s", log)
	}
}

func TestOpenClawInterruptedStageNeverClaimsChangedForeignMCP(t *testing.T) {
	fixture := setupOpenClawIntegrationFixture(t)
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	staged := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeOpenClaw, RuntimeVersion: "2026.7.1-2",
		RuntimeCLICommand: fixture.cli, MCPCommand: fixture.witself, MCPEnvironment: fixture.mcpEnvironment(t), MCPConnectTimeoutSeconds: 60,
		RuntimeWorkspace: fixture.workspace, RuntimeAgentID: "main",
		CaptureMode: transcriptcapture.ModeRaw, HookMode: transcriptcapture.HookModeNone,
		Account: "default", AccountID: "acc_1", Realm: "default", RealmID: "realm_1",
		Agent: "scott", AgentID: "agent_1", AgentName: "scott",
		Endpoint: fixture.serverURL, TokenFile: fixture.token, Location: location,
	}
	if err := transcriptcapture.SaveConfig(staged); err != nil {
		t.Fatal(err)
	}
	foreign := []byte(`{"witself":{"command":"/usr/local/bin/foreign","args":["serve"]}}`)
	if err := os.WriteFile(fixture.state, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := installCmd(fixture.installArgs()); code != 1 {
		t.Fatalf("interrupted-stage foreign install code = %d", code)
	}
	got, err := os.ReadFile(fixture.state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, foreign) {
		t.Fatalf("interrupted stage changed foreign registration: %s", got)
	}
	loaded, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RuntimeCLICommand != staged.RuntimeCLICommand || loaded.MCPCommand != staged.MCPCommand {
		t.Fatalf("interrupted stage config changed: %#v", loaded)
	}
}

func TestOpenClawUninstallRejectsChangedMCPAndRestoresRouting(t *testing.T) {
	fixture := setupOpenClawIntegrationFixture(t)
	if code := installCmd(fixture.installArgs()); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	agentsPath := filepath.Join(fixture.workspace, "AGENTS.md")
	installedInstructions, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	foreign := []byte(`{"witself":{"command":"/usr/local/bin/foreign","args":["serve"]}}`)
	if err := os.WriteFile(fixture.state, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := uninstallCmd([]string{"openclaw"}); code != 1 {
		t.Fatalf("foreign-MCP uninstall code = %d", code)
	}
	got, err := os.ReadFile(fixture.state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, foreign) {
		t.Fatalf("foreign MCP registration changed: %s", got)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw); err != nil {
		t.Fatalf("integration config was removed: %v", err)
	}
	restoredInstructions, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restoredInstructions, installedInstructions) {
		t.Fatalf("routing rollback = %q", restoredInstructions)
	}
}

func TestOpenClawUninstallLeavesMCPAbsentWhenRoutingChangesAfterPreflight(t *testing.T) {
	fixture := setupOpenClawIntegrationFixture(t)
	if code := installCmd(fixture.installArgs()); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	routingPath := filepath.Join(fixture.workspace, "AGENTS.md")
	t.Setenv("FAKE_OPENCLAW_BREAK_ROUTING", routingPath)
	if code := uninstallCmd([]string{"openclaw"}); code != 1 {
		t.Fatalf("routing-race uninstall code = %d", code)
	}
	if _, err := os.Stat(fixture.state); !os.IsNotExist(err) {
		t.Fatalf("MCP was restored without proven routing policy: %v", err)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw); err != nil {
		t.Fatalf("integration config was not preserved for retry: %v", err)
	}
}

func TestOpenClawRoutingBlockUsesVisibleToolNamesAndSafetyBoundaries(t *testing.T) {
	contract := string(openClawMemoryRoutingBlock)
	for _, want := range []string{
		"witself__witself-self-show",
		"witself__witself-memory-recall",
		"witself__witself-fact-set",
		"witself__witself-fact-propose",
		"witself__witself-memory-capture",
		"Merely stated facts",
		"Plain \"forget\" is ambiguous",
		"MEMORY.md",
		"User work comes first",
		"witself__witself-memory-curation-run-get",
		"witself__witself-memory-curation-plan-get",
		"witself__witself-message-request-select",
		"witself__witself-message-reply",
		"witself__witself-message-complete",
		"witself__witself-email-code-candidates",
		"witself__witself-email-complete",
		"witself__witself-avatar-generation-fail",
		"witself__witself-secret-search",
		"witself__witself-totp-code",
		"final answer must repeat every authorized requested answer/value",
	} {
		if !strings.Contains(contract, want) {
			t.Errorf("OpenClaw routing block does not contain %q", want)
		}
	}
	if strings.Contains(contract, "witself.self.show") || strings.Contains(contract, "automatic context injection is available") {
		t.Fatalf("OpenClaw routing block uses the wrong runtime contract:\n%s", contract)
	}
	if len(openClawMemoryRoutingBlock) > 12_000 {
		t.Fatalf("OpenClaw routing block is unexpectedly large: %d bytes", len(openClawMemoryRoutingBlock))
	}
	if len(openClawMemoryRoutingBlock) >= openClawBootstrapMaxFileBytes {
		t.Fatalf("OpenClaw routing block is %d bytes, bootstrap limit %d", len(openClawMemoryRoutingBlock), openClawBootstrapMaxFileBytes)
	}
}

func TestOpenClawRoutingFailsClosedBeforeBootstrapTruncation(t *testing.T) {
	fixture := setupOpenClawIntegrationFixture(t)
	if err := os.MkdirAll(fixture.workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.workspace, "AGENTS.md")
	original := []byte(strings.Repeat("x", openClawBootstrapMaxFileBytes-len(openClawMemoryRoutingBlock)))
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := installCmd(fixture.installArgs()); code != 1 {
		t.Fatalf("oversized bootstrap install code = %d", code)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("oversized bootstrap file changed")
	}
	if _, err := os.Stat(fixture.state); !os.IsNotExist(err) {
		t.Fatalf("MCP registration was changed before bootstrap refusal: %v", err)
	}
}

func TestOpenClawInstalledTopologyFailsClosedOnAgentDrift(t *testing.T) {
	fixture := setupOpenClawIntegrationFixture(t)
	cfg := transcriptcapture.Config{
		RuntimeCLICommand: fixture.cli, MCPEnvironment: fixture.mcpEnvironment(t), RuntimeWorkspace: fixture.workspace, RuntimeAgentID: "main",
	}
	if err := validateOpenClawInstalledTopology(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITSELF_HOME", filepath.Join(fixture.home, "host-drift-witself"))
	t.Setenv("OPENCLAW_STATE_DIR", filepath.Join(fixture.home, "host-drift-state"))
	t.Setenv("OPENCLAW_CONFIG_PATH", filepath.Join(fixture.home, "host-drift-config.json"))
	t.Setenv("OPENCLAW_PROFILE", "host-drift-profile")
	if err := validateOpenClawInstalledTopology(cfg); err != nil {
		t.Fatalf("persisted environment did not override host drift: %v", err)
	}
	replaced, err := json.Marshal([]openClawAgent{{ID: "replacement", Workspace: fixture.workspace, IsDefault: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OPENCLAW_AGENTS", string(replaced))
	if err := validateOpenClawInstalledTopology(cfg); err == nil || !strings.Contains(err.Error(), "default agent changed") {
		t.Fatalf("replaced-agent topology error = %v", err)
	}

	agents, err := json.Marshal([]openClawAgent{
		{ID: "main", Workspace: fixture.workspace, IsDefault: true},
		{ID: "other", Workspace: filepath.Join(fixture.home, "other"), IsDefault: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OPENCLAW_AGENTS", string(agents))
	if err := validateOpenClawInstalledTopology(cfg); err == nil || !strings.Contains(err.Error(), "requires exactly one") {
		t.Fatalf("multi-agent topology error = %v", err)
	}

	moved := filepath.Join(fixture.home, "moved")
	agents, err = json.Marshal([]openClawAgent{{ID: "main", Workspace: moved, IsDefault: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OPENCLAW_AGENTS", string(agents))
	if err := validateOpenClawInstalledTopology(cfg); err == nil || !strings.Contains(err.Error(), "default workspace changed") {
		t.Fatalf("moved-workspace topology error = %v", err)
	}
}

func TestRestoreOpenClawMCPBindingUsesPersistedPreviousCommand(t *testing.T) {
	fixture := setupOpenClawIntegrationFixture(t)
	previous := transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeOpenClaw, MCPCommand: "/opt/previous/witself",
		MCPEnvironment: fixture.mcpEnvironment(t), MCPConnectTimeoutSeconds: 45,
		Account: "default", Realm: "default", Agent: "scott", AgentName: "scott",
		Location: transcriptcapture.Location{Name: "home"},
	}
	attempted := previous
	attempted.MCPCommand = "/opt/attempted/witself"
	attempted.MCPConnectTimeoutSeconds = 60
	attemptedBinding, err := openClawMCPBindingFromConfig(fixture.witself, attempted)
	if err != nil {
		t.Fatal(err)
	}
	current, err := json.Marshal(map[string]openClawMCPBinding{"witself": attemptedBinding})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.state, current, 0o600); err != nil {
		t.Fatal(err)
	}
	previousBinding, err := openClawMCPBindingFromConfig(fixture.witself, previous)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := json.Marshal(map[string]openClawMCPBinding{"witself": previousBinding})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OPENCLAW_BINDING", string(restored))
	if err := restoreRuntimeMCPBinding(
		transcriptcapture.RuntimeOpenClaw,
		fixture.cli,
		fixture.witself,
		&previous,
		&attempted,
	); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(fixture.state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), restored) {
		t.Fatalf("restored MCP binding = %s, want %s", got, restored)
	}
	log, err := os.ReadFile(fixture.log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "--connect-timeout 45") {
		t.Fatalf("rollback did not restore persisted timeout:\n%s", log)
	}
}

func TestTranscriptHookSupportIsExplicitlyAllowlisted(t *testing.T) {
	for _, runtimeName := range []string{
		transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor,
	} {
		if !supportsTranscriptHooks(runtimeName) {
			t.Errorf("%s should support transcript hooks", runtimeName)
		}
	}
	for _, runtimeName := range []string{
		transcriptcapture.RuntimeOpenClaw,
		transcriptcapture.RuntimeAntigravity,
		transcriptcapture.RuntimeCopilot,
		"future-runtime",
	} {
		if supportsTranscriptHooks(runtimeName) {
			t.Errorf("%s unexpectedly supports transcript hooks", runtimeName)
		}
	}
}

func TestOpenClawIsExcludedFromTranscriptDependentWorkflows(t *testing.T) {
	if code := memoryAcceptancePrepare([]string{"--runtime", "openclaw", "--peer-agent", "peer"}); code != 2 {
		t.Fatalf("OpenClaw memory acceptance code = %d", code)
	}
	if _, code := parseMemoryCurateAutoRuntime("status", []string{"--runtime", "openclaw"}, false); code != 2 {
		t.Fatalf("OpenClaw automatic curator parse code = %d", code)
	}
}

func TestOpenClawMCPEnvironmentCaptureAndExactOwnership(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, "state", "..", ".witself"))
	t.Setenv("OPENCLAW_CONFIG_PATH", "~/profiles/work.json")
	t.Setenv("OPENCLAW_STATE_DIR", "~/profiles/state")
	t.Setenv("OPENCLAW_PROFILE", "work-profile")
	for _, key := range openClawUnsupportedSelectorEnvironment {
		t.Setenv(key, "")
	}
	environment, err := captureOpenClawMCPEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"WITSELF_HOME":         filepath.Join(home, ".witself"),
		"OPENCLAW_CONFIG_PATH": filepath.Join(home, "profiles", "work.json"),
		"OPENCLAW_STATE_DIR":   filepath.Join(home, "profiles", "state"),
		"OPENCLAW_PROFILE":     "work-profile",
	}
	if !equalOpenClawMCPEnvironment(environment, want) {
		t.Fatalf("captured environment = %#v, want %#v", environment, want)
	}
	serveArgs := []string{"/usr/local/bin/witself", "mcp", "serve"}
	binding, err := openClawMCPBindingFromServeArgsAndEnvironment(serveArgs, environment)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ConnectTimeout != openClawMCPConnectTimeoutSeconds || !equalOpenClawMCPEnvironment(binding.Env, want) {
		t.Fatalf("binding = %#v", binding)
	}
	for _, key := range []string{"WITSELF_HOME", "OPENCLAW_CONFIG_PATH", "OPENCLAW_STATE_DIR", "OPENCLAW_PROFILE"} {
		driftedEnvironment := cloneOpenClawEnvironment(environment)
		driftedEnvironment[key] += "-drift"
		drifted, err := openClawMCPBindingFromServeArgsAndEnvironment(serveArgs, driftedEnvironment)
		if err != nil {
			t.Fatalf("build %s-drift binding: %v", key, err)
		}
		if equalOpenClawMCPBinding(binding, drifted) {
			t.Errorf("binding ownership ignored %s drift", key)
		}
	}
	for _, profile := range []string{"-work", "work.profile", "work profile", strings.Repeat("a", 65)} {
		if err := validateOpenClawProfile(profile); err == nil {
			t.Errorf("profile %q unexpectedly validated", profile)
		}
	}
}

func TestOpenClawCommandEnvironmentScrubsCaseInsensitiveSelectorDrift(t *testing.T) {
	overlay := map[string]string{
		"WITSELF_HOME":         "/persisted/witself",
		"OPENCLAW_CONFIG_PATH": "/persisted/openclaw.json",
		"OPENCLAW_STATE_DIR":   "/persisted/state",
		"OPENCLAW_PROFILE":     "work",
	}
	result := openClawCommandEnvironment([]string{
		"KEEP=value",
		"Witself_Home=/ambient/witself",
		"OpenClaw_Config_Path=/ambient/openclaw.json",
		"openclaw_state_dir=/ambient/state",
		"openclaw_profile=ambient",
		"OpenClaw_Workspace_Dir=/ambient/workspace",
	}, overlay)
	seen := map[string]string{}
	for _, entry := range result {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed command environment entry %q", entry)
		}
		for existingKey := range seen {
			if strings.EqualFold(existingKey, key) {
				t.Fatalf("case-insensitive duplicate environment keys %q and %q in %#v", existingKey, key, result)
			}
		}
		seen[key] = value
	}
	if seen["KEEP"] != "value" {
		t.Fatalf("unrelated environment was not preserved: %#v", result)
	}
	for key, value := range overlay {
		if seen[key] != value {
			t.Errorf("%s = %q, want %q", key, seen[key], value)
		}
	}
	for key := range seen {
		if strings.EqualFold(key, "OPENCLAW_WORKSPACE_DIR") {
			t.Fatalf("unsupported selector survived scrubbing: %#v", result)
		}
	}
}

func TestOpenClawCLIJSONTimeoutIsBoundedAndDetectable(t *testing.T) {
	home := t.TempDir()
	cli := filepath.Join(home, "slow-openclaw")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n/bin/sleep 1\nprintf '%s\\n' '{}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := openClawCLIJSONWithTimeout(cli, nil, 25*time.Millisecond, "mcp", "list", "--json")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 750*time.Millisecond {
		t.Fatalf("bounded timeout took %s", elapsed)
	}
}
