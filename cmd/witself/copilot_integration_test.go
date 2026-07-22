package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestConfigureCopilotBindingPinsRootsAndEnvironment(t *testing.T) {
	base := t.TempDir()
	copilotHome := filepath.Join(base, "copilot")
	witselfHome := filepath.Join(base, "witself")
	runtimeCLI := copilotTestFile(t, base, "bin", "copilot")
	executable := copilotTestFile(t, base, "bin", "witself")
	t.Setenv("COPILOT_HOME", copilotHome)
	t.Setenv("WITSELF_HOME", witselfHome)

	cfg := copilotTestConfig()
	if err := configureCopilotBinding(&cfg, runtimeCLI, executable); err != nil {
		t.Fatal(err)
	}
	canonicalCLI, _ := cleanCopilotInvocationPath("test CLI", runtimeCLI)
	canonicalExecutable, _ := cleanCopilotAbsolutePath("test executable", executable)
	canonicalCopilotHome, _ := cleanCopilotAbsolutePath("test COPILOT_HOME", copilotHome)
	canonicalWitselfHome, _ := cleanCopilotAbsolutePath("test WITSELF_HOME", witselfHome)
	if cfg.RuntimeCLICommand != canonicalCLI || cfg.MCPCommand != canonicalExecutable ||
		cfg.RuntimeConfigRoot != canonicalCopilotHome ||
		cfg.RuntimeMCPConfigPath != filepath.Join(canonicalCopilotHome, "mcp-config.json") ||
		!reflect.DeepEqual(cfg.MCPEnvironment, map[string]string{"WITSELF_HOME": canonicalWitselfHome}) {
		t.Fatalf("configured Copilot binding = %#v", cfg)
	}
}

func TestConfigureCopilotBindingPreservesStableCLISymlink(t *testing.T) {
	base := t.TempDir()
	copilotHome := filepath.Join(base, "copilot-home")
	witselfHome := filepath.Join(base, "witself-home")
	firstTarget := copilotTestFile(t, base, "versions", "1.0.73", "copilot")
	secondTarget := copilotTestFile(t, base, "versions", "1.0.74", "copilot")
	stableCLI := filepath.Join(base, "bin", "copilot")
	if err := os.MkdirAll(filepath.Dir(stableCLI), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(firstTarget, stableCLI); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPILOT_HOME", copilotHome)
	t.Setenv("WITSELF_HOME", witselfHome)

	first := copilotTestConfig()
	if err := configureCopilotBinding(&first, stableCLI, copilotTestFile(t, base, "bin", "witself")); err != nil {
		t.Fatal(err)
	}
	if first.RuntimeCLICommand != stableCLI {
		t.Fatalf("persisted CLI = %q, want stable symlink %q", first.RuntimeCLICommand, stableCLI)
	}

	if err := os.Remove(stableCLI); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secondTarget, stableCLI); err != nil {
		t.Fatal(err)
	}
	second := copilotTestConfig()
	if err := configureCopilotBinding(&second, stableCLI, first.MCPCommand); err != nil {
		t.Fatal(err)
	}
	if second.RuntimeCLICommand != first.RuntimeCLICommand {
		t.Fatalf("provider upgrade changed stable CLI binding from %q to %q", first.RuntimeCLICommand, second.RuntimeCLICommand)
	}
}

func TestValidateCopilotRuntimeVersion(t *testing.T) {
	for _, version := range []string{"1.0.73", "1.0.74", "1.1.0", "2.0.0-beta.1"} {
		if err := validateCopilotRuntimeVersion(version); err != nil {
			t.Fatalf("version %s rejected: %v", version, err)
		}
	}
	for _, version := range []string{"", "1.0", "1.0.72", "1.0.73-beta.1", "development"} {
		if err := validateCopilotRuntimeVersion(version); err == nil {
			t.Fatalf("version %q unexpectedly accepted", version)
		}
	}
}

func TestCopilotMCPServerNameUsesStableBindingIDs(t *testing.T) {
	cfg := copilotTestConfig()
	first, err := copilotMCPServerName(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := copilotMCPServerName(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != len(copilotMCPServerNamePrefix)+copilotMCPServerHashLength ||
		!strings.HasPrefix(first, copilotMCPServerNamePrefix) {
		t.Fatalf("unstable or malformed name %q / %q", first, second)
	}
	changed := cfg
	changed.Location.ID = "loc_other"
	third, err := copilotMCPServerName(changed)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("location identity did not affect Copilot MCP server name")
	}
	changed = cfg
	changed.AgentID = ""
	if _, err := copilotMCPServerName(changed); err == nil || !strings.Contains(err.Error(), "agent id") {
		t.Fatalf("missing stable id error = %v", err)
	}
}

func TestParseCopilotMCPBindingAcceptsLocalAndDocumentedStdio(t *testing.T) {
	for _, transport := range []string{"local", "stdio"} {
		t.Run(transport, func(t *testing.T) {
			enabled := true
			raw, err := json.Marshal(copilotMCPBinding{
				Tools: []string{"*"}, Type: transport, Command: "/opt/witself",
				Args: []string{"mcp", "serve"}, Env: map[string]string{"WITSELF_HOME": "/tmp/witself"},
				Source: "user", Enabled: &enabled,
			})
			if err != nil {
				t.Fatal(err)
			}
			binding, err := parseCopilotMCPBinding(raw)
			if err != nil {
				t.Fatal(err)
			}
			if binding.Type != transport {
				t.Fatalf("type = %q", binding.Type)
			}
		})
	}
	if _, err := parseCopilotMCPBinding([]byte(`{"tools":["*"],"type":"local","command":"/opt/witself","args":[],"env":{"WITSELF_HOME":"/tmp/w"},"cwd":"/tmp"}`)); err == nil || !strings.Contains(err.Error(), "non-standard field") {
		t.Fatalf("unknown field error = %v", err)
	}
	if _, err := parseCopilotMCPBinding([]byte(`{"tools":["*"],"type":"http","command":"/opt/witself","args":[],"env":{"WITSELF_HOME":"/tmp/w"}}`)); err == nil || !strings.Contains(err.Error(), "unsupported MCP type") {
		t.Fatalf("HTTP binding error = %v", err)
	}
}

func TestCanonicalCopilotJSONComparesSemanticsWithoutLosingLargeNumbers(t *testing.T) {
	left, err := canonicalCopilotJSON([]byte(`{"b":1.0,"a":[true,null]}`))
	if err != nil {
		t.Fatal(err)
	}
	right, err := canonicalCopilotJSON([]byte(`{"a":[true,null],"b":1e0}`))
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("semantically equal JSON differs: %q / %q", left, right)
	}
	largeLeft, err := canonicalCopilotJSON([]byte(`9007199254740992`))
	if err != nil {
		t.Fatal(err)
	}
	largeRight, err := canonicalCopilotJSON([]byte(`9007199254740993`))
	if err != nil {
		t.Fatal(err)
	}
	if largeLeft == largeRight {
		t.Fatal("large distinct JSON numbers collapsed")
	}
}

func TestRegisterAndUnregisterCopilotMCPPreserveSiblingServers(t *testing.T) {
	cfg := configuredCopilotTestConfig(t)
	fixture := installCopilotCLIFixture(t)
	sibling := json.RawMessage(`{"type":"http","url":"https://example.test/mcp"}`)
	fixture.setRaw(t, "sibling", sibling)

	if err := registerCopilotMCP(cfg.RuntimeCLICommand, cfg); err != nil {
		t.Fatal(err)
	}
	name, expected, err := copilotMCPBindingFromConfig(cfg.MCPCommand, cfg)
	if err != nil {
		t.Fatal(err)
	}
	current, exists, err := fixture.binding(name)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || !equalCopilotMCPBinding(current, expected) {
		t.Fatalf("registered binding = %#v exists=%t", current, exists)
	}
	if !reflect.DeepEqual(fixture.servers["sibling"], sibling) {
		t.Fatal("Copilot registration modified a sibling server")
	}
	if err := registerCopilotMCP(cfg.RuntimeCLICommand, cfg); err != nil {
		t.Fatal(err)
	}
	if got := fixture.operationCount("add"); got != 1 {
		t.Fatalf("idempotent registration issued %d add operations", got)
	}
	if err := unregisterCopilotMCP(cfg.RuntimeCLICommand, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, exists := fixture.servers[name]; exists {
		t.Fatal("managed Copilot server remained after uninstall")
	}
	if !reflect.DeepEqual(fixture.servers["sibling"], sibling) {
		t.Fatal("Copilot removal modified a sibling server")
	}
	for _, root := range fixture.roots {
		if root != cfg.RuntimeConfigRoot {
			t.Fatalf("Copilot command used root %q, want %q", root, cfg.RuntimeConfigRoot)
		}
	}
}

func TestCopilotMutationRejectsNonTargetRegistryChanges(t *testing.T) {
	cfg := configuredCopilotTestConfig(t)
	fixture := installCopilotCLIFixture(t)
	fixture.setRaw(t, "sibling", json.RawMessage(`{"type":"http","url":"https://example.test/mcp"}`))
	fixture.setRootField(t, "registryMetadata", json.RawMessage(`{"owner":"user"}`))
	fixture.mutateOnAdd = true
	if err := registerCopilotMCP(cfg.RuntimeCLICommand, cfg); err == nil || !strings.Contains(err.Error(), "non-target root fields") {
		t.Fatalf("non-target mutation error = %v", err)
	}
}

func TestInspectCopilotMCPRejectsAmbiguousPersistedRegistry(t *testing.T) {
	t.Run("duplicate keys", func(t *testing.T) {
		cfg := configuredCopilotTestConfig(t)
		_ = installCopilotCLIFixture(t)
		if err := os.MkdirAll(filepath.Dir(cfg.RuntimeMCPConfigPath), 0o700); err != nil {
			t.Fatal(err)
		}
		raw := []byte(`{"mcpServers":{},"mcpServers":{}}`)
		if err := os.WriteFile(cfg.RuntimeMCPConfigPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := inspectCopilotMCP(cfg.RuntimeCLICommand, cfg); err == nil || !strings.Contains(err.Error(), "duplicate JSON object key") {
			t.Fatalf("duplicate registry error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		cfg := configuredCopilotTestConfig(t)
		_ = installCopilotCLIFixture(t)
		if err := os.MkdirAll(filepath.Dir(cfg.RuntimeMCPConfigPath), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "real-mcp-config.json")
		if err := os.WriteFile(target, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, cfg.RuntimeMCPConfigPath); err != nil {
			t.Fatal(err)
		}
		if _, _, err := inspectCopilotMCP(cfg.RuntimeCLICommand, cfg); err == nil || !strings.Contains(err.Error(), "is a symlink") {
			t.Fatalf("symlink registry error = %v", err)
		}
	})
}

func TestInspectCopilotMCPRejectsWorkspaceShadow(t *testing.T) {
	cfg := configuredCopilotTestConfig(t)
	fixture := installCopilotCLIFixture(t)
	name, binding, err := copilotMCPBindingFromConfig(cfg.MCPCommand, cfg)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setBinding(t, name, binding)
	enabled := true
	binding.Source = "workspace"
	binding.Enabled = &enabled
	shadow, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	fixture.listOverrides[name] = shadow
	if _, _, err := inspectCopilotMCP(cfg.RuntimeCLICommand, cfg); err == nil || !strings.Contains(err.Error(), "not the user registry") {
		t.Fatalf("workspace shadow error = %v", err)
	}
}

func TestRunCopilotCLIIsolatesCallerWorkspace(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".mcp.json"), []byte(`{"mcpServers":{"shadow":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "cwd.log")
	t.Setenv("FAKE_COPILOT_CWD_LOG", logPath)
	cli := filepath.Join(t.TempDir(), "copilot")
	script := `#!/bin/sh
printf '%s\n' "$PWD" > "$FAKE_COPILOT_CWD_LOG"
if [ -e .mcp.json ] || [ -e .github/mcp.json ]; then
  printf '%s\n' '{"mcpServers":{"shadow":{"source":"workspace"}}}'
else
  printf '%s\n' '{"mcpServers":{}}'
fi
`
	if err := os.WriteFile(cli, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(original) }()
	raw, err := runCopilotCLICommand(cli, filepath.Join(t.TempDir(), "copilot-home"), 5*time.Second, "mcp", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("shadow")) {
		t.Fatalf("workspace MCP shadow leaked into isolated Copilot output: %s", raw)
	}
	commandCWD, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(commandCWD)) == workspace {
		t.Fatal("Copilot CLI ran in the caller workspace")
	}
}

func TestPrepareCopilotMCPInstallRequiresDurableOwnership(t *testing.T) {
	cfg := configuredCopilotTestConfig(t)
	fixture := installCopilotCLIFixture(t)
	name, binding, err := copilotMCPBindingFromConfig(cfg.MCPCommand, cfg)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setBinding(t, name, binding)

	touched, err := prepareCopilotMCPInstall(cfg.RuntimeCLICommand, cfg.MCPCommand, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "without a Witself integration record") || touched {
		t.Fatalf("foreign ownership result touched=%t err=%v", touched, err)
	}
	if _, exists := fixture.servers[name]; !exists {
		t.Fatal("unowned exact-looking Copilot server was removed")
	}
}

func TestPrepareCopilotMCPInstallReplacesOnlyExactPreviousBinding(t *testing.T) {
	previous := configuredCopilotTestConfig(t)
	fixture := installCopilotCLIFixture(t)
	previousName, previousBinding, err := copilotMCPBindingFromConfig(previous.MCPCommand, previous)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setBinding(t, previousName, previousBinding)

	desired := previous
	desired.MCPCommand = copilotTestFile(t, filepath.Dir(filepath.Dir(previous.MCPCommand)), "upgrade", "witself")
	touched, err := prepareCopilotMCPInstall(desired.RuntimeCLICommand, desired.MCPCommand, desired, &previous)
	if err != nil {
		t.Fatal(err)
	}
	if !touched {
		t.Fatal("exact prior binding was not removed before replacement")
	}
	if _, exists := fixture.servers[previousName]; exists {
		t.Fatal("prior binding remained after prepare")
	}
	if err := registerCopilotMCP(desired.RuntimeCLICommand, desired); err != nil {
		t.Fatal(err)
	}
	current, exists, err := fixture.binding(previousName)
	if err != nil {
		t.Fatal(err)
	}
	_, desiredBinding, err := copilotMCPBindingFromConfig(desired.MCPCommand, desired)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || !equalCopilotMCPBinding(current, desiredBinding) {
		t.Fatalf("upgraded binding = %#v exists=%t", current, exists)
	}
}

func TestPrepareCopilotMCPInstallMigratesCollisionResistantName(t *testing.T) {
	previous := configuredCopilotTestConfig(t)
	fixture := installCopilotCLIFixture(t)
	previousName, previousBinding, err := copilotMCPBindingFromConfig(previous.MCPCommand, previous)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setBinding(t, previousName, previousBinding)

	desired := previous
	desired.AgentID = "agt_new"
	desired.Agent = "new-agent"
	desired.AgentName = "new-agent"
	desiredName, _, err := copilotMCPBindingFromConfig(desired.MCPCommand, desired)
	if err != nil {
		t.Fatal(err)
	}
	if desiredName == previousName {
		t.Fatal("test identities produced the same server name")
	}
	touched, err := prepareCopilotMCPInstall(desired.RuntimeCLICommand, desired.MCPCommand, desired, &previous)
	if err != nil || !touched {
		t.Fatalf("name migration touched=%t err=%v", touched, err)
	}
	if _, exists := fixture.servers[previousName]; exists {
		t.Fatal("old collision-resistant name remained")
	}
	if err := registerCopilotMCP(desired.RuntimeCLICommand, desired); err != nil {
		t.Fatal(err)
	}
	if _, exists := fixture.servers[desiredName]; !exists {
		t.Fatal("new collision-resistant name was not registered")
	}
}

func TestPrepareCopilotMCPInstallRecoversInterruptedNameRebind(t *testing.T) {
	previous := configuredCopilotTestConfig(t)
	fixture := installCopilotCLIFixture(t)
	desired := previous
	desired.AgentID = "agt_new"
	desired.Agent = "new-agent"
	desired.AgentName = "new-agent"
	desiredName, desiredBinding, err := copilotMCPBindingFromConfig(desired.MCPCommand, desired)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setBinding(t, desiredName, desiredBinding)

	touched, err := prepareCopilotMCPInstall(desired.RuntimeCLICommand, desired.MCPCommand, desired, &previous)
	if err != nil || touched {
		t.Fatalf("interrupted rebind recovery touched=%t err=%v", touched, err)
	}
	if got := fixture.operationCount("remove"); got != 0 {
		t.Fatalf("interrupted rebind recovery issued %d removes", got)
	}
	if err := registerCopilotMCP(desired.RuntimeCLICommand, desired); err != nil {
		t.Fatal(err)
	}
	if got := fixture.operationCount("add"); got != 0 {
		t.Fatalf("interrupted rebind recovery issued %d adds", got)
	}

	previousName, previousBinding, err := copilotMCPBindingFromConfig(previous.MCPCommand, previous)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setBinding(t, previousName, previousBinding)
	if touched, err := prepareCopilotMCPInstall(desired.RuntimeCLICommand, desired.MCPCommand, desired, &previous); err == nil ||
		!strings.Contains(err.Error(), "both exist") || touched {
		t.Fatalf("ambiguous interrupted rebind touched=%t err=%v", touched, err)
	}
}

func TestPrepareCopilotMCPInstallRefusesDriftedPreviousBinding(t *testing.T) {
	previous := configuredCopilotTestConfig(t)
	fixture := installCopilotCLIFixture(t)
	name, binding, err := copilotMCPBindingFromConfig(previous.MCPCommand, previous)
	if err != nil {
		t.Fatal(err)
	}
	binding.Args = append(binding.Args, "--foreign")
	fixture.setBinding(t, name, binding)
	desired := previous
	desired.MCPCommand = copilotTestFile(t, filepath.Dir(filepath.Dir(previous.MCPCommand)), "upgrade", "witself")

	touched, err := prepareCopilotMCPInstall(desired.RuntimeCLICommand, desired.MCPCommand, desired, &previous)
	if err == nil || !strings.Contains(err.Error(), "differs from both") || touched {
		t.Fatalf("drifted binding result touched=%t err=%v", touched, err)
	}
	if _, exists := fixture.servers[name]; !exists {
		t.Fatal("drifted prior binding was removed")
	}
}

func TestInspectCopilotMCPRejectsCaseInsensitiveAliasCollision(t *testing.T) {
	cfg := configuredCopilotTestConfig(t)
	fixture := installCopilotCLIFixture(t)
	name, _, err := copilotMCPBindingFromConfig(cfg.MCPCommand, cfg)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setRaw(t, strings.ToUpper(name), json.RawMessage(`{"type":"http","url":"https://example.test"}`))
	if _, _, err := inspectCopilotMCP(cfg.RuntimeCLICommand, cfg); err == nil || !strings.Contains(err.Error(), "collides case-insensitively") {
		t.Fatalf("alias collision error = %v", err)
	}
}

func TestValidateCopilotInstalledTopologyDetectsDrift(t *testing.T) {
	cfg := configuredCopilotTestConfig(t)
	fixture := installCopilotCLIFixture(t)
	routing, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeCopilot)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerCopilotMCP(cfg.RuntimeCLICommand, cfg); err != nil {
		t.Fatal(err)
	}
	if err := validateCopilotInstalledTopology(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(routing.path, append(append(bytes.Clone(copilotMemoryRoutingBlock), '\n'), []byte("foreign\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateCopilotInstalledTopology(cfg); err == nil || !strings.Contains(err.Error(), "managed instructions") {
		t.Fatalf("instruction topology error = %v", err)
	}
	if err := os.WriteFile(routing.path, append(bytes.Clone(copilotMemoryRoutingBlock), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	name, binding, err := copilotMCPBindingFromConfig(cfg.MCPCommand, cfg)
	if err != nil {
		t.Fatal(err)
	}
	binding.Args = append(binding.Args, "--drift")
	fixture.setBinding(t, name, binding)
	if err := validateCopilotInstalledTopology(cfg); err == nil || !strings.Contains(err.Error(), "no longer matches") {
		t.Fatalf("topology drift error = %v", err)
	}
}

func TestRestoreRuntimeMCPBindingRollsBackCopilotUpgrade(t *testing.T) {
	previous := configuredCopilotTestConfig(t)
	fixture := installCopilotCLIFixture(t)
	attempted := previous
	attempted.MCPCommand = copilotTestFile(t, filepath.Dir(filepath.Dir(previous.MCPCommand)), "upgrade", "witself")
	name, attemptedBinding, err := copilotMCPBindingFromConfig(attempted.MCPCommand, attempted)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setBinding(t, name, attemptedBinding)
	if err := restoreRuntimeMCPBinding(
		transcriptcapture.RuntimeCopilot,
		attempted.RuntimeCLICommand,
		attempted.MCPCommand,
		&previous,
		&attempted,
	); err != nil {
		t.Fatal(err)
	}
	current, exists, err := fixture.binding(name)
	if err != nil {
		t.Fatal(err)
	}
	_, previousBinding, err := copilotMCPBindingFromConfig(previous.MCPCommand, previous)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || !equalCopilotMCPBinding(current, previousBinding) {
		t.Fatalf("restored binding = %#v exists=%t", current, exists)
	}
}

func TestCopilotInstallAndUninstallLifecycle(t *testing.T) {
	home := t.TempDir()
	witselfHome := filepath.Join(home, ".witself")
	copilotHome := filepath.Join(home, ".copilot")
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", witselfHome)
	t.Setenv("COPILOT_HOME", copilotHome)
	setInstallExecutableForTest(t)

	cli := filepath.Join(home, "copilot")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf '%s\n' 'GitHub Copilot CLI 1.0.73'
  exit 0
fi
if [ "$1 $2 $3" = "mcp add --help" ]; then
  exit 0
fi
exit 2
`
	if err := os.WriteFile(cli, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPILOT_CLI_PATH", cli)
	fixture := installCopilotCLIFixture(t)
	sibling := json.RawMessage(`{"type":"http","url":"https://example.test/mcp"}`)
	fixture.setRaw(t, "sibling", sibling)
	fixture.setRootField(t, "registryMetadata", json.RawMessage(`{"owner":"user","revision":1}`))

	tokenPath := filepath.Join(home, "agent.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/self" || request.Header.Get("Authorization") != "Bearer agent-token" {
			http.NotFound(w, request)
			return
		}
		_, _ = fmt.Fprint(w, `{"schema_version":"witself.v0","identity":{"account_id":"acc_1","realm_id":"realm_1","realm_name":"default","agent_id":"agent_1","agent_name":"copilot-test-bot"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`)
	}))
	t.Cleanup(server.Close)

	if code := installCmd([]string{
		"copilot", "--account", "default", "--realm", "default", "--agent", "copilot-test-bot",
		"--location", "home", "--endpoint", server.URL, "--token-file", tokenPath,
	}); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCopilot)
	if err != nil {
		t.Fatal(err)
	}
	canonicalCLI, _ := cleanCopilotInvocationPath("test CLI", cli)
	canonicalCopilotHome, _ := cleanCopilotAbsolutePath("test COPILOT_HOME", copilotHome)
	canonicalWitselfHome, _ := cleanCopilotAbsolutePath("test WITSELF_HOME", witselfHome)
	if cfg.HookMode != transcriptcapture.HookModeNone || cfg.RuntimeVersion != "1.0.73" ||
		cfg.RuntimeCLICommand != canonicalCLI || cfg.RuntimeConfigRoot != canonicalCopilotHome ||
		cfg.RuntimeMCPConfigPath != filepath.Join(canonicalCopilotHome, "mcp-config.json") ||
		!equalCopilotEnvironment(cfg.MCPEnvironment, map[string]string{"WITSELF_HOME": canonicalWitselfHome}) ||
		cfg.AccountID != "acc_1" || cfg.RealmID != "realm_1" || cfg.AgentID != "agent_1" || cfg.Location.ID == "" {
		t.Fatalf("persisted Copilot config = %#v", cfg)
	}
	name, expected, err := copilotMCPBindingFromConfig(cfg.MCPCommand, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, copilotMCPServerNamePrefix) || len(name) != len(copilotMCPServerNamePrefix)+copilotMCPServerHashLength {
		t.Fatalf("managed server name = %q", name)
	}
	current, exists, err := fixture.binding(name)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || !equalCopilotMCPBinding(current, expected) {
		t.Fatalf("installed server = %#v exists=%t", current, exists)
	}
	persisted, err := readCopilotMCPConfigSnapshot(cfg.RuntimeMCPConfigPath, name)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.targetExists || persisted.mode.Perm() != 0o600 ||
		persisted.siblings["sibling"] == "" || persisted.rootFields["registryMetadata"] == "" {
		t.Fatalf("persisted MCP registry projection = %#v", persisted)
	}
	instructionsPath := filepath.Join(canonicalCopilotHome, "instructions", copilotMemoryRoutingRuleFile)
	instructions, err := os.ReadFile(instructionsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(instructions, append(bytes.Clone(copilotMemoryRoutingBlock), '\n')) {
		t.Fatal("installed Copilot instructions are not the exact managed block")
	}

	if code := uninstallCmd([]string{"copilot"}); code != 0 {
		t.Fatalf("uninstall code = %d", code)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCopilot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Copilot integration config remains: %v", err)
	}
	if _, err := os.Stat(instructionsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Copilot instructions remain: %v", err)
	}
	if _, exists := fixture.servers[name]; exists {
		t.Fatal("managed Copilot server remains after uninstall")
	}
	if !reflect.DeepEqual(fixture.servers["sibling"], sibling) || fixture.rootFields["registryMetadata"] == nil {
		t.Fatal("Copilot uninstall modified sibling or root registry state")
	}
}

func TestCopilotCommandEnvironmentPinsOnlyCopilotHome(t *testing.T) {
	got := copilotCommandEnvironment([]string{
		"PATH=/bin", "COPILOT_HOME=/foreign", "copilot_home=/also-foreign", "WITSELF_HOME=/current",
	}, "/exact/copilot")
	want := []string{"PATH=/bin", "WITSELF_HOME=/current", "COPILOT_HOME=/exact/copilot"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
}

func copilotTestConfig() transcriptcapture.Config {
	return transcriptcapture.Config{
		Runtime:        transcriptcapture.RuntimeCopilot,
		RuntimeVersion: "1.0.73",
		Account:        "default",
		AccountID:      "acc_test",
		Realm:          "default",
		RealmID:        "rlm_test",
		Agent:          "copilot-test-bot",
		AgentID:        "agt_test",
		AgentName:      "copilot-test-bot",
		Location:       transcriptcapture.Location{ID: "loc_test", Name: "home"},
	}
}

func configuredCopilotTestConfig(t *testing.T) transcriptcapture.Config {
	t.Helper()
	base := t.TempDir()
	copilotHome := filepath.Join(base, "copilot")
	witselfHome := filepath.Join(base, "witself")
	if err := os.MkdirAll(witselfHome, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeCLI := copilotTestFile(t, base, "bin", "copilot")
	executable := copilotTestFile(t, base, "bin", "witself")
	t.Setenv("COPILOT_HOME", copilotHome)
	t.Setenv("WITSELF_HOME", witselfHome)
	cfg := copilotTestConfig()
	if err := configureCopilotBinding(&cfg, runtimeCLI, executable); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func copilotTestFile(t *testing.T, root string, parts ...string) string {
	t.Helper()
	pathParts := append([]string{root}, parts...)
	path := filepath.Join(pathParts...)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("test"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

type copilotCLIFixture struct {
	mu            sync.Mutex
	configPath    string
	rootFields    map[string]json.RawMessage
	servers       map[string]json.RawMessage
	listOverrides map[string]json.RawMessage
	calls         [][]string
	roots         []string
	mutateOnAdd   bool
}

func installCopilotCLIFixture(t *testing.T) *copilotCLIFixture {
	t.Helper()
	root, err := currentCopilotConfigRoot()
	if err != nil {
		t.Fatal(err)
	}
	fixture := &copilotCLIFixture{
		configPath: filepath.Join(root, "mcp-config.json"),
		rootFields: map[string]json.RawMessage{}, servers: map[string]json.RawMessage{},
		listOverrides: map[string]json.RawMessage{},
	}
	previous := runCopilotCLI
	runCopilotCLI = fixture.run
	t.Cleanup(func() { runCopilotCLI = previous })
	return fixture
}

func (fixture *copilotCLIFixture) run(_ string, root string, _ time.Duration, args ...string) ([]byte, error) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.calls = append(fixture.calls, append([]string(nil), args...))
	fixture.roots = append(fixture.roots, root)
	if reflect.DeepEqual(args, []string{"mcp", "add", "--help"}) {
		return nil, nil
	}
	if reflect.DeepEqual(args, []string{"--version"}) {
		return []byte("GitHub Copilot CLI 1.0.73\n"), nil
	}
	if reflect.DeepEqual(args, []string{"mcp", "list", "--json"}) {
		servers := make(map[string]json.RawMessage, len(fixture.servers))
		for name, raw := range fixture.servers {
			var binding copilotMCPBinding
			if err := json.Unmarshal(raw, &binding); err == nil && binding.Command != "" {
				enabled := true
				binding.Source = "user"
				binding.Enabled = &enabled
				decorated, err := json.Marshal(binding)
				if err != nil {
					return nil, err
				}
				servers[name] = decorated
			} else {
				servers[name] = append(json.RawMessage(nil), raw...)
			}
		}
		for name, raw := range fixture.listOverrides {
			servers[name] = append(json.RawMessage(nil), raw...)
		}
		return json.Marshal(copilotMCPList{Servers: servers})
	}
	if len(args) >= 3 && args[0] == "mcp" && args[1] == "remove" {
		if _, exists := fixture.servers[args[2]]; !exists {
			return nil, fmt.Errorf("no MCP server named %s", args[2])
		}
		delete(fixture.servers, args[2])
		if err := fixture.persistLocked(); err != nil {
			return nil, err
		}
		return []byte("removed"), nil
	}
	if len(args) >= 7 && args[0] == "mcp" && args[1] == "add" {
		name := args[2]
		if _, exists := fixture.servers[name]; exists {
			return nil, fmt.Errorf("server %s already exists", name)
		}
		binding := copilotMCPBinding{Type: "local", Env: map[string]string{}}
		index := 3
		for index < len(args) && args[index] != "--" {
			if index+1 >= len(args) {
				return nil, errors.New("missing add option value")
			}
			switch args[index] {
			case "--tools":
				binding.Tools = []string{args[index+1]}
			case "--env":
				key, value, ok := strings.Cut(args[index+1], "=")
				if !ok {
					return nil, errors.New("invalid env option")
				}
				binding.Env[key] = value
			default:
				return nil, fmt.Errorf("unexpected add option %s", args[index])
			}
			index += 2
		}
		if index+1 >= len(args) || args[index] != "--" {
			return nil, errors.New("missing command separator")
		}
		binding.Command = args[index+1]
		binding.Args = append([]string(nil), args[index+2:]...)
		fixture.setBindingLocked(name, binding)
		if fixture.mutateOnAdd {
			fixture.rootFields["foreignMutation"] = json.RawMessage(`true`)
		}
		if err := fixture.persistLocked(); err != nil {
			return nil, err
		}
		return []byte("added"), nil
	}
	return nil, fmt.Errorf("unexpected Copilot command: %v", args)
}

func (fixture *copilotCLIFixture) setBinding(t *testing.T, name string, binding copilotMCPBinding) {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.setBindingLocked(name, binding)
	if err := fixture.persistLocked(); err != nil {
		t.Fatal(err)
	}
}

func (fixture *copilotCLIFixture) setBindingLocked(name string, binding copilotMCPBinding) {
	binding.Source = ""
	binding.Enabled = nil
	raw, err := json.Marshal(binding)
	if err != nil {
		panic(err)
	}
	fixture.servers[name] = raw
}

func (fixture *copilotCLIFixture) setRaw(t *testing.T, name string, raw json.RawMessage) {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.servers[name] = append(json.RawMessage(nil), raw...)
	if err := fixture.persistLocked(); err != nil {
		t.Fatal(err)
	}
}

func (fixture *copilotCLIFixture) setRootField(t *testing.T, name string, raw json.RawMessage) {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.rootFields[name] = append(json.RawMessage(nil), raw...)
	if err := fixture.persistLocked(); err != nil {
		t.Fatal(err)
	}
}

func (fixture *copilotCLIFixture) persistLocked() error {
	root := make(map[string]json.RawMessage, len(fixture.rootFields)+1)
	for name, raw := range fixture.rootFields {
		root[name] = append(json.RawMessage(nil), raw...)
	}
	servers, err := json.Marshal(fixture.servers)
	if err != nil {
		return err
	}
	root["mcpServers"] = servers
	raw, err := json.Marshal(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(fixture.configPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(fixture.configPath, raw, 0o600); err != nil {
		return err
	}
	return os.Chmod(fixture.configPath, 0o600)
}

func (fixture *copilotCLIFixture) binding(name string) (copilotMCPBinding, bool, error) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	raw, exists := fixture.servers[name]
	if !exists {
		return copilotMCPBinding{}, false, nil
	}
	binding, err := parseCopilotMCPBinding(raw)
	return binding, true, err
}

func (fixture *copilotCLIFixture) operationCount(operation string) int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	count := 0
	for _, args := range fixture.calls {
		if len(args) >= 2 && args[0] == "mcp" && args[1] == operation {
			count++
		}
	}
	return count
}
