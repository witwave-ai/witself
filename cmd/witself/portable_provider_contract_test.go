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
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type portableProviderContract struct {
	home              string
	witselfHome       string
	witselfExecutable string
	tokenPath         string
	endpoint          string
	provider          fakeProviderCLI
}

func setupPortableProviderContract(t *testing.T, kind string) portableProviderContract {
	t.Helper()
	clearProviderCLIPathOverridesForTest(t)
	home := t.TempDir()
	if canonical, err := filepath.EvalSymlinks(home); err == nil {
		home = canonical
	}
	witselfHome := filepath.Join(home, ".witself")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WITSELF_HOME", witselfHome)
	t.Setenv("WITSELF_ACCOUNT", "")
	t.Setenv("WITSELF_REALM", "")
	t.Setenv("WITSELF_AGENT", "")
	t.Setenv("WITSELF_LOCATION", "")
	witselfExecutable := genericProviderContractWitselfExecutable(t)

	provider := buildFakeProviderCLI(t, home)
	isolateProviderDiscoveryPATHForTest(t)
	provider.useKind(t, kind)
	tokenPath := filepath.Join(home, "agent.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/self" || request.Header.Get("Authorization") != "Bearer agent-token" {
			http.NotFound(writer, request)
			return
		}
		_, _ = fmt.Fprint(writer, `{"schema_version":"witself.v0","identity":{"account_id":"acc_contract","realm_id":"realm_contract","realm_name":"default","agent_id":"agent_contract","agent_name":"provider-contract-bot"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`)
	}))
	t.Cleanup(server.Close)
	return portableProviderContract{
		home: home, witselfHome: witselfHome, witselfExecutable: witselfExecutable,
		tokenPath: tokenPath, endpoint: server.URL, provider: provider,
	}
}

func (fixture portableProviderContract) installArgs(runtimeName string) []string {
	return []string{
		runtimeName,
		"--account", "default",
		"--realm", "default",
		"--agent", "provider-contract-bot",
		"--location", "home",
		"--endpoint", fixture.endpoint,
		"--token-file", fixture.tokenPath,
	}
}

func TestProviderIntegrationContractOpenClaw(t *testing.T) {
	fixture := setupPortableProviderContract(t, "openclaw")
	workspace := filepath.Join(fixture.home, "openclaw-workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	instructionsPath := filepath.Join(workspace, "AGENTS.md")
	originalInstructions := []byte("# User-owned OpenClaw guidance\n")
	if err := os.WriteFile(instructionsPath, originalInstructions, 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := map[string]any{"command": "foreign-provider", "args": []string{"serve"}}
	fixture.provider.writeRawState(t, map[string]any{"sibling": sibling})
	t.Setenv(fakeProviderWorkspaceEnv, workspace)
	t.Setenv("OPENCLAW_CLI_PATH", fixture.provider.Path)
	t.Setenv("OPENCLAW_STATE_DIR", filepath.Join(fixture.home, "openclaw-state"))
	t.Setenv("OPENCLAW_CONFIG_PATH", filepath.Join(fixture.home, "openclaw-state", "openclaw.json"))
	t.Setenv("OPENCLAW_PROFILE", "contract")
	for _, key := range openClawUnsupportedSelectorEnvironment {
		t.Setenv(key, "")
	}

	installArgs := fixture.installArgs(transcriptcapture.RuntimeOpenClaw)
	if code := runGenericProviderContractCLI(t, fixture.witselfExecutable, "install", installArgs...); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOpenClawInstalledIntegration(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeCLICommand != fixture.provider.Path || cfg.RuntimeWorkspace != workspace ||
		cfg.RuntimeAgentID != "main" || cfg.HookMode != transcriptcapture.HookModeNone ||
		cfg.MCPEnvironment["WITSELF_HOME"] != fixture.witselfHome {
		t.Fatalf("persisted OpenClaw binding = %#v", cfg)
	}
	assertPortableProviderVerification(t, fixture, transcriptcapture.RuntimeOpenClaw, integrationVerificationHealthy)
	state := fixture.provider.readRawState(t)
	assertJSONEquivalent(t, state["sibling"], sibling)
	var installedBinding openClawMCPBinding
	if err := json.Unmarshal(state["witself"], &installedBinding); err != nil {
		t.Fatal(err)
	}
	expectedBinding, err := openClawMCPBindingFromConfig(cfg.MCPCommand, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !equalOpenClawMCPBinding(installedBinding, expectedBinding) {
		t.Fatalf("installed OpenClaw MCP binding = %#v, want %#v", installedBinding, expectedBinding)
	}
	installedInstructions, err := os.ReadFile(instructionsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(installedInstructions, openClawMemoryRoutingBlock) ||
		!bytes.HasSuffix(installedInstructions, originalInstructions) {
		t.Fatalf("installed OpenClaw instructions = %q", installedInstructions)
	}

	if code := runGenericProviderContractCLI(t, fixture.witselfExecutable, "install", installArgs...); code != 0 {
		t.Fatalf("reinstall code = %d", code)
	}
	reinstalledState := fixture.provider.readRawState(t)
	assertJSONEquivalent(t, reinstalledState["sibling"], sibling)
	assertJSONEquivalent(t, reinstalledState["witself"], installedBinding)
	if current, err := os.ReadFile(instructionsPath); err != nil || !bytes.Equal(current, installedInstructions) {
		t.Fatalf("reinstall changed OpenClaw instructions: %q, %v", current, err)
	}
	assertPortableProviderVerification(t, fixture, transcriptcapture.RuntimeOpenClaw, integrationVerificationHealthy)

	if code := runGenericProviderContractCLI(t, fixture.witselfExecutable, "uninstall", transcriptcapture.RuntimeOpenClaw); code != 0 {
		t.Fatalf("uninstall code = %d", code)
	}
	state = fixture.provider.readRawState(t)
	if _, exists := state["witself"]; exists {
		t.Fatal("managed OpenClaw MCP binding remains after uninstall")
	}
	assertJSONEquivalent(t, state["sibling"], sibling)
	if current, err := os.ReadFile(instructionsPath); err != nil || !bytes.Equal(current, originalInstructions) {
		t.Fatalf("OpenClaw uninstall changed user instructions: %q, %v", current, err)
	}
	assertIntegrationConfigAbsent(t, transcriptcapture.RuntimeOpenClaw)
	assertPortableProviderVerification(t, fixture, transcriptcapture.RuntimeOpenClaw, integrationVerificationNotInstalled)
	assertProviderMutationCounts(t, fixture.provider, "add", 1, "unset", 1)
}

func TestProviderIntegrationContractAntigravity(t *testing.T) {
	fixture := setupPortableProviderContract(t, "antigravity")
	t.Setenv("ANTIGRAVITY_CLI_PATH", fixture.provider.Path)
	configRoot := filepath.Join(fixture.home, ".gemini", "config")
	if err := os.MkdirAll(configRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	sibling := antigravityMCPServer{
		Command: "foreign-provider",
		Args:    []string{"serve"},
		Env:     map[string]string{"FOREIGN": "preserved"},
	}
	initial := map[string]any{
		"providerSettings": map[string]any{"enabled": true, "revision": 7},
		"mcpServers":       map[string]any{"sibling": sibling},
	}
	writeJSONFile(t, filepath.Join(configRoot, "mcp_config.json"), initial)
	unrelatedPlugin := filepath.Join(configRoot, "plugins", "user-owned")
	if err := os.MkdirAll(unrelatedPlugin, 0o700); err != nil {
		t.Fatal(err)
	}
	unrelatedPluginFile := filepath.Join(unrelatedPlugin, "plugin.json")
	if err := os.WriteFile(unrelatedPluginFile, []byte(`{"name":"user-owned"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	installArgs := fixture.installArgs(transcriptcapture.RuntimeAntigravity)
	if code := runGenericProviderContractCLI(t, fixture.witselfExecutable, "install", installArgs...); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAntigravityInstalledTopology(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeCLICommand != fixture.provider.Path || cfg.RuntimeConfigRoot != configRoot ||
		cfg.HookMode != transcriptcapture.HookModeNone ||
		cfg.MCPEnvironment["WITSELF_HOME"] != fixture.witselfHome {
		t.Fatalf("persisted Antigravity binding = %#v", cfg)
	}
	assertPortableProviderVerification(t, fixture, transcriptcapture.RuntimeAntigravity, integrationVerificationHealthy)
	serverName, err := antigravityMCPServerName(cfg)
	if err != nil {
		t.Fatal(err)
	}
	installedMCP := readContractJSONObject(t, cfg.RuntimeMCPConfigPath)
	servers := installedMCP["mcpServers"].(map[string]any)
	assertJSONEquivalent(t, servers["sibling"], sibling)
	if _, exists := servers[serverName]; !exists {
		t.Fatalf("managed Antigravity MCP server %s is missing", serverName)
	}
	assertJSONEquivalent(t, installedMCP["providerSettings"], initial["providerSettings"])
	installedMCPRaw, err := os.ReadFile(cfg.RuntimeMCPConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	installedRule, err := os.ReadFile(filepath.Join(cfg.RuntimePluginPath, "rules", "witself.md"))
	if err != nil {
		t.Fatal(err)
	}

	if code := runGenericProviderContractCLI(t, fixture.witselfExecutable, "install", installArgs...); code != 0 {
		t.Fatalf("reinstall code = %d", code)
	}
	if current, err := os.ReadFile(cfg.RuntimeMCPConfigPath); err != nil || !bytes.Equal(current, installedMCPRaw) {
		t.Fatalf("reinstall changed Antigravity MCP registry: %q, %v", current, err)
	}
	if current, err := os.ReadFile(filepath.Join(cfg.RuntimePluginPath, "rules", "witself.md")); err != nil ||
		!bytes.Equal(current, installedRule) {
		t.Fatalf("reinstall changed Antigravity rule: %q, %v", current, err)
	}
	assertPortableProviderVerification(t, fixture, transcriptcapture.RuntimeAntigravity, integrationVerificationHealthy)

	if code := runGenericProviderContractCLI(t, fixture.witselfExecutable, "uninstall", transcriptcapture.RuntimeAntigravity); code != 0 {
		t.Fatalf("uninstall code = %d", code)
	}
	uninstalledMCP := readContractJSONObject(t, cfg.RuntimeMCPConfigPath)
	uninstalledServers := uninstalledMCP["mcpServers"].(map[string]any)
	if _, exists := uninstalledServers[serverName]; exists {
		t.Fatal("managed Antigravity MCP server remains after uninstall")
	}
	assertJSONEquivalent(t, uninstalledServers["sibling"], sibling)
	assertJSONEquivalent(t, uninstalledMCP["providerSettings"], initial["providerSettings"])
	if _, err := os.Stat(unrelatedPluginFile); err != nil {
		t.Fatalf("Antigravity uninstall changed unrelated plugin: %v", err)
	}
	if _, err := os.Lstat(cfg.RuntimePluginPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed Antigravity plugin remains after uninstall: %v", err)
	}
	assertIntegrationConfigAbsent(t, transcriptcapture.RuntimeAntigravity)
	assertPortableProviderVerification(t, fixture, transcriptcapture.RuntimeAntigravity, integrationVerificationNotInstalled)
	assertProviderInvocation(t, fixture.provider, []string{"plugin", "validate"}, 2)
}

func TestProviderIntegrationContractCopilot(t *testing.T) {
	fixture := setupPortableProviderContract(t, "copilot")
	t.Setenv("COPILOT_CLI_PATH", fixture.provider.Path)
	copilotHome := filepath.Join(fixture.home, ".copilot-contract")
	t.Setenv("COPILOT_HOME", copilotHome)
	configPath := filepath.Join(copilotHome, "mcp-config.json")
	sibling := map[string]any{"type": "http", "url": "https://example.test/mcp"}
	initial := map[string]any{
		"registryMetadata": map[string]any{"owner": "user", "revision": 7},
		"mcpServers":       map[string]any{"sibling": sibling},
	}
	writeJSONFile(t, configPath, initial)
	instructionsDirectory := filepath.Join(copilotHome, "instructions")
	if err := os.MkdirAll(instructionsDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	siblingInstruction := filepath.Join(instructionsDirectory, "user-owned.instructions.md")
	siblingInstructionRaw := []byte("# User-owned Copilot instruction\n")
	if err := os.WriteFile(siblingInstruction, siblingInstructionRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	installArgs := fixture.installArgs(transcriptcapture.RuntimeCopilot)
	if code := runGenericProviderContractCLI(t, fixture.witselfExecutable, "install", installArgs...); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeCopilot)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCopilotInstalledTopology(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeCLICommand != fixture.provider.Path || cfg.RuntimeConfigRoot != copilotHome ||
		cfg.RuntimeMCPConfigPath != configPath || cfg.HookMode != transcriptcapture.HookModeNone ||
		cfg.MCPEnvironment["WITSELF_HOME"] != fixture.witselfHome {
		t.Fatalf("persisted Copilot binding = %#v", cfg)
	}
	assertPortableProviderVerification(t, fixture, transcriptcapture.RuntimeCopilot, integrationVerificationHealthy)
	serverName, expectedBinding, err := copilotMCPBindingFromConfig(cfg.MCPCommand, cfg)
	if err != nil {
		t.Fatal(err)
	}
	installedMCP := readContractJSONObject(t, configPath)
	servers := installedMCP["mcpServers"].(map[string]any)
	assertJSONEquivalent(t, servers["sibling"], sibling)
	var installedBinding copilotMCPBinding
	bindingRaw, err := json.Marshal(servers[serverName])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bindingRaw, &installedBinding); err != nil {
		t.Fatal(err)
	}
	if !equalCopilotMCPBinding(installedBinding, expectedBinding) {
		t.Fatalf("installed Copilot MCP binding = %#v, want %#v", installedBinding, expectedBinding)
	}
	assertJSONEquivalent(t, installedMCP["registryMetadata"], initial["registryMetadata"])
	managedInstructions := filepath.Join(instructionsDirectory, copilotMemoryRoutingRuleFile)
	installedInstructions, err := os.ReadFile(managedInstructions)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(installedInstructions, append(bytes.Clone(copilotMemoryRoutingBlock), '\n')) {
		t.Fatal("installed Copilot instructions do not match the exact managed block")
	}

	if code := runGenericProviderContractCLI(t, fixture.witselfExecutable, "install", installArgs...); code != 0 {
		t.Fatalf("reinstall code = %d", code)
	}
	reinstalledMCP := readContractJSONObject(t, configPath)
	assertJSONEquivalent(t, reinstalledMCP, installedMCP)
	if current, err := os.ReadFile(managedInstructions); err != nil || !bytes.Equal(current, installedInstructions) {
		t.Fatalf("reinstall changed Copilot instructions: %q, %v", current, err)
	}
	assertPortableProviderVerification(t, fixture, transcriptcapture.RuntimeCopilot, integrationVerificationHealthy)

	if code := runGenericProviderContractCLI(t, fixture.witselfExecutable, "uninstall", transcriptcapture.RuntimeCopilot); code != 0 {
		t.Fatalf("uninstall code = %d", code)
	}
	uninstalledMCP := readContractJSONObject(t, configPath)
	uninstalledServers := uninstalledMCP["mcpServers"].(map[string]any)
	if _, exists := uninstalledServers[serverName]; exists {
		t.Fatal("managed Copilot MCP server remains after uninstall")
	}
	assertJSONEquivalent(t, uninstalledServers["sibling"], sibling)
	assertJSONEquivalent(t, uninstalledMCP["registryMetadata"], initial["registryMetadata"])
	if _, err := os.Stat(managedInstructions); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed Copilot instructions remain after uninstall: %v", err)
	}
	if current, err := os.ReadFile(siblingInstruction); err != nil || !bytes.Equal(current, siblingInstructionRaw) {
		t.Fatalf("Copilot uninstall changed sibling instruction: %q, %v", current, err)
	}
	assertIntegrationConfigAbsent(t, transcriptcapture.RuntimeCopilot)
	assertPortableProviderVerification(t, fixture, transcriptcapture.RuntimeCopilot, integrationVerificationNotInstalled)
	assertProviderMutationCounts(t, fixture.provider, "add", 1, "remove", 1)
}

func assertPortableProviderVerification(
	t *testing.T,
	fixture portableProviderContract,
	runtimeName string,
	wantState string,
) {
	t.Helper()
	var report integrationsReport
	if strings.TrimSpace(os.Getenv(installedCommandAcceptanceBinaryEnv)) == "" {
		report = collectIntegrationsReportWithVerification(true)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		command := exec.CommandContext(
			ctx,
			fixture.witselfExecutable,
			"integrations", "--verify", "--json",
		)
		command.Env = os.Environ()
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			if ctx.Err() != nil {
				t.Fatalf("installed Witself integrations verification timed out: %v\nstderr:\n%s", ctx.Err(), stderr.String())
			}
			t.Fatalf("installed Witself integrations verification failed: %v\nstdout:\n%s\nstderr:\n%s",
				err, stdout.String(), stderr.String())
		}
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("decode installed Witself integrations verification: %v\nstdout:\n%s\nstderr:\n%s",
				err, stdout.String(), stderr.String())
		}
	}
	if report.SchemaVersion != integrationsSchemaVersion {
		t.Fatalf("integrations schema_version = %q, want %q", report.SchemaVersion, integrationsSchemaVersion)
	}
	for _, status := range report.Runtimes {
		if status.Runtime != runtimeName {
			continue
		}
		if status.Verification == nil || status.Verification.State != wantState {
			t.Fatalf("%s integrations verification = %#v, want %q", runtimeName, status.Verification, wantState)
		}
		return
	}
	t.Fatalf("integrations verification omitted runtime %s", runtimeName)
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readContractJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func assertJSONEquivalent(t *testing.T, actual, expected any) {
	t.Helper()
	normalize := func(value any) any {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	if left, right := normalize(actual), normalize(expected); !reflect.DeepEqual(left, right) {
		t.Fatalf("JSON differs:\nactual:   %#v\nexpected: %#v", left, right)
	}
}

func assertIntegrationConfigAbsent(t *testing.T, runtimeName string) {
	t.Helper()
	if _, err := transcriptcapture.LoadConfig(runtimeName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s integration config remains: %v", runtimeName, err)
	}
}

func assertProviderMutationCounts(
	t *testing.T,
	provider fakeProviderCLI,
	addOperation string,
	wantAdds int,
	removeOperation string,
	wantRemoves int,
) {
	t.Helper()
	adds := 0
	removes := 0
	for _, call := range provider.invocations(t) {
		if len(call.Args) < 2 || call.Args[0] != "mcp" {
			continue
		}
		switch {
		case call.Args[1] == addOperation && (len(call.Args) != 3 || call.Args[2] != "--help"):
			adds++
		case call.Args[1] == removeOperation:
			removes++
		}
	}
	if adds != wantAdds || removes != wantRemoves {
		t.Fatalf("provider mutation calls: %s=%d want %d, %s=%d want %d",
			addOperation, adds, wantAdds, removeOperation, removes, wantRemoves)
	}
}

func assertProviderInvocation(t *testing.T, provider fakeProviderCLI, prefix []string, want int) {
	t.Helper()
	count := 0
	for _, call := range provider.invocations(t) {
		if len(call.Args) >= len(prefix) && strings.Join(call.Args[:len(prefix)], "\x00") == strings.Join(prefix, "\x00") {
			count++
		}
	}
	if count != want {
		t.Fatalf("provider invocation prefix %v count = %d, want %d", prefix, count, want)
	}
}
