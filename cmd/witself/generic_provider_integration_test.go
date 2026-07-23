package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const (
	fakeGenericRuntimeEnv      = "WITSELF_FAKE_GENERIC_RUNTIME"
	fakeGenericLogEnv          = "WITSELF_FAKE_GENERIC_LOG"
	fakeGenericStateEnv        = "WITSELF_FAKE_GENERIC_STATE"
	fakeGenericFailAddEnv      = "WITSELF_FAKE_GENERIC_FAIL_NEXT_ADD"
	fakeGenericFailRemoveEnv   = "WITSELF_FAKE_GENERIC_FAIL_AFTER_REMOVE"
	fakeGenericLargeErrorEnv   = "WITSELF_FAKE_GENERIC_LARGE_ERROR_BYTES"
	fakeGenericLargeOutputEnv  = "WITSELF_FAKE_GENERIC_LARGE_OUTPUT_BYTES"
	fakeProviderCWDArtifactEnv = "WITSELF_FAKE_PROVIDER_CWD_ARTIFACT"
)

var genericProviderTestRuntimes = []string{
	transcriptcapture.RuntimeCodex,
	transcriptcapture.RuntimeClaudeCode,
	transcriptcapture.RuntimeGrokBuild,
	transcriptcapture.RuntimeCursor,
}

var (
	genericProviderFixtureBuildOnce sync.Once
	genericProviderFixturePath      string
	genericProviderFixtureBuildErr  error
	genericProviderFixtureBuildLog  []byte
)

func genericProviderFixtureExecutable(t *testing.T) string {
	t.Helper()
	genericProviderFixtureBuildOnce.Do(func() {
		buildRoot, err := os.MkdirTemp("", "witself-generic-provider-fixture-")
		if err != nil {
			genericProviderFixtureBuildErr = err
			return
		}
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			genericProviderFixtureBuildErr = errors.New("resolve generic provider fixture source")
			return
		}
		executableName := "generic-provider"
		if runtime.GOOS == "windows" {
			executableName += ".exe"
		}
		genericProviderFixturePath = filepath.Join(buildRoot, executableName)
		build := exec.Command("go", "build", "-o", genericProviderFixturePath, ".")
		build.Dir = filepath.Join(filepath.Dir(thisFile), "testdata", "genericprovider")
		genericProviderFixtureBuildLog, genericProviderFixtureBuildErr = build.CombinedOutput()
	})
	if genericProviderFixtureBuildErr != nil {
		t.Fatalf("build generic provider fixture: %v\n%s", genericProviderFixtureBuildErr, genericProviderFixtureBuildLog)
	}
	return genericProviderFixturePath
}

type genericProviderTestInvocation struct {
	Args             []string `json:"args"`
	CodexHome        string   `json:"codex_home"`
	ClaudeConfigDir  string   `json:"claude_config_dir"`
	GrokHome         string   `json:"grok_home"`
	CursorConfigDir  string   `json:"cursor_config_dir"`
	WorkingDirectory string   `json:"working_directory"`
}

type genericProviderTestFixture struct {
	runtime      string
	root         string
	cli          string
	witself      string
	witselfHome  string
	selectorName string
	selectorRoot string
	cliEnv       string
	logPath      string
	statePath    string
	cfg          transcriptcapture.Config
}

func newGenericProviderTestFixture(t *testing.T, runtimeName string) genericProviderTestFixture {
	t.Helper()
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	witselfHome := filepath.Join(root, "witself-state")
	if err := os.MkdirAll(userHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(witselfHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome)
	t.Setenv("WITSELF_HOME", witselfHome)
	t.Setenv(managedHooksTestRootEnv, filepath.Join(root, "managed-hooks"))
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "GROK_HOME", "CURSOR_CONFIG_DIR"} {
		t.Setenv(name, "")
	}
	for _, name := range []string{"CODEX_CLI_PATH", "CLAUDE_CLI_PATH", "GROK_CLI_PATH", "CURSOR_CLI_PATH"} {
		t.Setenv(name, "")
	}

	selectorName := map[string]string{
		transcriptcapture.RuntimeCodex:      "CODEX_HOME",
		transcriptcapture.RuntimeClaudeCode: "CLAUDE_CONFIG_DIR",
		transcriptcapture.RuntimeGrokBuild:  "GROK_HOME",
		transcriptcapture.RuntimeCursor:     "CURSOR_CONFIG_DIR",
	}[runtimeName]
	cliEnv := map[string]string{
		transcriptcapture.RuntimeCodex:      "CODEX_CLI_PATH",
		transcriptcapture.RuntimeClaudeCode: "CLAUDE_CLI_PATH",
		transcriptcapture.RuntimeGrokBuild:  "GROK_CLI_PATH",
		transcriptcapture.RuntimeCursor:     "CURSOR_CLI_PATH",
	}[runtimeName]
	if runtimeName != transcriptcapture.RuntimeCursor {
		t.Setenv(selectorName, filepath.Join(root, "provider"))
	}

	cli := genericProviderFixtureExecutable(t)
	t.Setenv(cliEnv, cli)
	t.Setenv(fakeGenericRuntimeEnv, runtimeName)
	logPath := filepath.Join(root, "invocations.jsonl")
	statePath := filepath.Join(root, "provider-state.json")
	t.Setenv(fakeGenericLogEnv, logPath)
	t.Setenv(fakeGenericStateEnv, statePath)
	t.Setenv(fakeGenericFailAddEnv, "")
	t.Setenv(fakeGenericFailRemoveEnv, "")
	t.Setenv(fakeGenericLargeErrorEnv, "")
	t.Setenv(fakeGenericLargeOutputEnv, "")

	witself, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(witselfExecutableTestEnv, witself)
	cfg := transcriptcapture.Config{
		Runtime: runtimeName, RuntimeVersion: "1.0.0",
		CaptureMode: transcriptcapture.ModeRaw, HookMode: transcriptcapture.HookModeUser,
		Account: "default", Realm: "default", Agent: "provider-test-bot",
		AgentID: "agent_provider_test", AgentName: "provider-test-bot",
		Location: transcriptcapture.Location{ID: "loc_provider_test", Name: "home"},
	}
	if err := configureGenericProviderBinding(&cfg, cli, witself); err != nil {
		t.Fatal(err)
	}
	return genericProviderTestFixture{
		runtime: runtimeName, root: root, cli: cfg.RuntimeCLICommand, witself: cfg.MCPCommand,
		witselfHome: cfg.MCPEnvironment["WITSELF_HOME"], selectorName: selectorName, selectorRoot: cfg.RuntimeConfigRoot,
		cliEnv: cliEnv, logPath: logPath, statePath: statePath, cfg: cfg,
	}
}

func TestRunGenericProviderCLIOutputIsBounded(t *testing.T) {
	fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeCodex)
	t.Setenv(fakeGenericLargeErrorEnv, "131072")
	output, err := runGenericProviderCLI(fixture.cli, fixture.cfg, time.Second, "--version")
	if err == nil {
		t.Fatal("large provider failure unexpectedly succeeded")
	}
	marker := "[validation output truncated]"
	if len(output) > genericProviderCLIOutputLimit+len(marker)+2 || !strings.Contains(string(output), marker) {
		t.Fatalf("bounded output length = %d, marker present = %t", len(output), strings.Contains(string(output), marker))
	}
	if len(err.Error()) > genericProviderCLIOutputLimit+len(marker)+2 || !strings.Contains(err.Error(), marker) {
		t.Fatalf("bounded error length = %d, marker present = %t", len(err.Error()), strings.Contains(err.Error(), marker))
	}
}

func TestRunLegacyProviderCLIOutputIsBounded(t *testing.T) {
	fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeCodex)
	t.Setenv(fakeGenericLargeErrorEnv, "131072")
	output, err := runLegacyProviderCLI(fixture.cli, time.Second, "--version")
	if err == nil {
		t.Fatal("large legacy provider failure unexpectedly succeeded")
	}
	marker := "[validation output truncated]"
	if len(output) > genericProviderCLIOutputLimit+len(marker)+2 || !strings.Contains(string(output), marker) {
		t.Fatalf("bounded legacy output length = %d, marker present = %t", len(output), strings.Contains(string(output), marker))
	}
	if len(err.Error()) > genericProviderCLIOutputLimit+len(marker)+2 || !strings.Contains(err.Error(), marker) {
		t.Fatalf("bounded legacy error length = %d, marker present = %t", len(err.Error()), strings.Contains(err.Error(), marker))
	}
}

func TestGrokJSONCLIRejectsOversizedOutput(t *testing.T) {
	fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeGrokBuild)
	t.Setenv(fakeGenericLargeOutputEnv, fmt.Sprintf("%d", grokJSONOutputLimit+1))
	if _, err := runGrokJSONCLI(fixture.cli, 10*time.Second, "inspect", "--json"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized Grok JSON error = %v", err)
	}
}

func TestGenericProviderCursorUsesNativeHomeConfigAndRejectsSelector(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CURSOR_CONFIG_DIR", "")
	root, path, err := genericProviderConfigPaths(transcriptcapture.RuntimeCursor)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, err := cleanCopilotAbsolutePath("Cursor test config root", filepath.Join(home, ".cursor"))
	if err != nil {
		t.Fatal(err)
	}
	if root != wantRoot || path != filepath.Join(wantRoot, "mcp.json") {
		t.Fatalf("Cursor native config = (%q, %q), want (%q, %q)", root, path, wantRoot, filepath.Join(wantRoot, "mcp.json"))
	}
	t.Setenv("CURSOR_CONFIG_DIR", wantRoot)
	if _, _, err := genericProviderConfigPaths(transcriptcapture.RuntimeCursor); err == nil || !strings.Contains(err.Error(), "not supported by cursor-agent") {
		t.Fatalf("CURSOR_CONFIG_DIR rejection = %v", err)
	}
}

func (fixture genericProviderTestFixture) seedNonTargetConfig(t *testing.T) genericMCPConfigSnapshot {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(fixture.cfg.RuntimeMCPConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if fixture.runtime == transcriptcapture.RuntimeClaudeCode || fixture.runtime == transcriptcapture.RuntimeCursor {
		raw = []byte("{\n  \"foreignSetting\": {\"keep\": true},\n  \"mcpServers\": {\n    \"foreign\": {\"command\": \"/foreign/tool\", \"args\": [\"--keep\"], \"env\": {\"KEEP\": \"yes\"}}\n  }\n}\n")
	} else {
		raw = []byte("foreign_setting = \"keep\"\n\n[mcp_servers.foreign]\ncommand = \"/foreign/tool\"\nargs = [\"--keep\"]\nenv = { KEEP = \"yes\" }\n")
	}
	if err := os.WriteFile(fixture.cfg.RuntimeMCPConfigPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := readGenericMCPConfig(fixture.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (fixture genericProviderTestFixture) installForeignTarget(t *testing.T) []byte {
	t.Helper()
	fixture.seedNonTargetConfig(t)
	path := fixture.cfg.RuntimeMCPConfigPath
	if fixture.runtime == transcriptcapture.RuntimeClaudeCode || fixture.runtime == transcriptcapture.RuntimeCursor {
		root := readTestJSONObject(t, path)
		servers := root["mcpServers"].(map[string]any)
		foreign := map[string]any{"command": "/foreign/witself", "args": []any{"--foreign"}, "env": map[string]any{}}
		if fixture.runtime == transcriptcapture.RuntimeClaudeCode {
			foreign["type"] = "stdio"
		}
		servers["witself"] = foreign
		if err := writeJSONObjectAtomic(path, root); err != nil {
			t.Fatal(err)
		}
	} else {
		extra := "\n[mcp_servers.witself]\ncommand = \"/foreign/witself\"\nargs = [\"--foreign\"]\nenv = {}\n"
		if fixture.runtime == transcriptcapture.RuntimeGrokBuild {
			extra += "enabled = true\n"
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(extra); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (fixture genericProviderTestFixture) invocations(t *testing.T) []genericProviderTestInvocation {
	t.Helper()
	file, err := os.Open(fixture.logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close provider invocation log: %v", err)
		}
	}()
	var result []genericProviderTestInvocation
	decoder := json.NewDecoder(file)
	for {
		var item genericProviderTestInvocation
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			return result
		} else if err != nil {
			t.Fatal(err)
		}
		result = append(result, item)
	}
}

func expectedGenericAddArgs(cfg transcriptcapture.Config, binding genericMCPBinding) []string {
	home := "WITSELF_HOME=" + binding.Environment["WITSELF_HOME"]
	switch cfg.Runtime {
	case transcriptcapture.RuntimeCodex:
		return append(append([]string{"mcp", "add", "witself", "--env", home, "--", binding.Command}, binding.Args...), []string{}...)
	case transcriptcapture.RuntimeClaudeCode:
		definition, err := json.Marshal(struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		}{
			Type: "stdio", Command: binding.Command, Args: binding.Args, Env: binding.Environment,
		})
		if err != nil {
			panic(err)
		}
		return []string{"mcp", "add-json", "--scope", "user", "witself", string(definition)}
	case transcriptcapture.RuntimeGrokBuild:
		return append([]string{"mcp", "add", "--scope", "user", "--env", home, "witself", "--", binding.Command}, binding.Args...)
	case transcriptcapture.RuntimeCursor:
		return []string{"mcp", "enable", "witself"}
	default:
		return nil
	}
}

func TestGenericProviderRefusesForeignWitselfWithoutMutation(t *testing.T) {
	for _, runtimeName := range genericProviderTestRuntimes {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := newGenericProviderTestFixture(t, runtimeName)
			before := fixture.installForeignTarget(t)
			if err := prepareGenericMCPInstall(fixture.cli, fixture.cfg, nil); err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("foreign ownership error = %v", err)
			}
			after, err := os.ReadFile(fixture.cfg.RuntimeMCPConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("foreign provider bytes changed during first-install refusal")
			}
			if calls := fixture.invocations(t); len(calls) != 0 {
				t.Fatalf("provider CLI was invoked during refusal: %#v", calls)
			}
		})
	}
}

func TestGenericProviderExactOwnedLifecycleAndSerialization(t *testing.T) {
	for _, runtimeName := range genericProviderTestRuntimes {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := newGenericProviderTestFixture(t, runtimeName)
			before := fixture.seedNonTargetConfig(t)
			if err := prepareGenericMCPInstall(fixture.cli, fixture.cfg, nil); err != nil {
				t.Fatal(err)
			}
			if err := registerGenericMCP(fixture.cli, fixture.cfg, nil); err != nil {
				t.Fatal(err)
			}
			expected, err := genericMCPBindingFromConfig(fixture.cfg)
			if err != nil {
				t.Fatal(err)
			}
			current, exists, installed, err := inspectGenericMCP(fixture.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !exists || !equalGenericMCPBinding(current, expected) {
				t.Fatalf("installed binding = %#v, exists=%t, want %#v", current, exists, expected)
			}
			if installed.nonTarget != before.nonTarget {
				t.Fatal("provider changed foreign configuration semantics during install")
			}
			calls := fixture.invocations(t)
			if len(calls) == 0 || !reflect.DeepEqual(calls[0].Args, expectedGenericAddArgs(fixture.cfg, expected)) {
				t.Fatalf("first provider call = %#v, want %#v", calls, expectedGenericAddArgs(fixture.cfg, expected))
			}
			selectedRoot := map[string]string{
				transcriptcapture.RuntimeCodex: calls[0].CodexHome, transcriptcapture.RuntimeClaudeCode: calls[0].ClaudeConfigDir,
				transcriptcapture.RuntimeGrokBuild: calls[0].GrokHome, transcriptcapture.RuntimeCursor: calls[0].CursorConfigDir,
			}[runtimeName]
			wantSelectedRoot := fixture.selectorRoot
			if runtimeName == transcriptcapture.RuntimeCursor {
				wantSelectedRoot = ""
			}
			if selectedRoot != wantSelectedRoot {
				t.Fatalf("provider selector = %q, want %q", selectedRoot, wantSelectedRoot)
			}
			if err := unregisterGenericMCP(fixture.cli, fixture.cfg); err != nil {
				t.Fatal(err)
			}
			_, exists, removed, err := inspectGenericMCP(fixture.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if exists || removed.nonTarget != before.nonTarget {
				t.Fatalf("remove exists=%t; foreign configuration was not preserved", exists)
			}
		})
	}
}

func TestGenericProviderReinstallRollbackAndDriftRefusal(t *testing.T) {
	for _, runtimeName := range genericProviderTestRuntimes {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := newGenericProviderTestFixture(t, runtimeName)
			fixture.seedNonTargetConfig(t)
			previous := fixture.cfg
			previous.Agent = "old-provider-bot"
			previous.AgentName = previous.Agent
			if err := registerGenericMCP(fixture.cli, previous, nil); err != nil {
				t.Fatal(err)
			}
			desired := fixture.cfg
			desired.Agent = "new-provider-bot"
			desired.AgentName = desired.Agent
			if err := prepareGenericMCPInstall(fixture.cli, desired, &previous); err != nil {
				t.Fatal(err)
			}
			if err := registerGenericMCP(fixture.cli, desired, &previous); err != nil {
				t.Fatal(err)
			}
			if err := restoreGenericMCPBinding(fixture.cli, &previous, &desired); err != nil {
				t.Fatal(err)
			}
			current, exists, _, err := inspectGenericMCP(previous)
			if err != nil {
				t.Fatal(err)
			}
			expectedPrevious, _ := genericMCPBindingFromConfig(previous)
			if !exists || !equalGenericMCPBinding(current, expectedPrevious) {
				t.Fatalf("rollback binding = %#v, exists=%t, want %#v", current, exists, expectedPrevious)
			}

			beforeFailure, err := os.ReadFile(previous.RuntimeMCPConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(fixture.root, "fail-next-add")
			if err := os.WriteFile(marker, []byte("fail\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(fakeGenericFailAddEnv, marker)
			if err := registerGenericMCP(fixture.cli, desired, &previous); err == nil {
				t.Fatal("reinstall unexpectedly succeeded after forced add failure")
			}
			afterFailure, err := os.ReadFile(previous.RuntimeMCPConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(afterFailure, beforeFailure) {
				t.Fatal("failed reinstall did not restore exact provider bytes")
			}

			tampered := bytes.Replace(afterFailure, []byte("old-provider-bot"), []byte("tampered-bot"), 1)
			if bytes.Equal(tampered, afterFailure) {
				t.Fatal("test could not tamper target binding")
			}
			if err := os.WriteFile(previous.RuntimeMCPConfigPath, tampered, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := prepareGenericMCPInstall(fixture.cli, desired, &previous); err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("reinstall drift error = %v", err)
			}
			if err := unregisterGenericMCP(fixture.cli, previous); err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("uninstall drift error = %v", err)
			}
			afterRefusal, err := os.ReadFile(previous.RuntimeMCPConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(afterRefusal, tampered) {
				t.Fatal("drift refusal changed provider bytes")
			}
		})
	}
}

func TestGenericProviderLegacyMigrationAndUninstall(t *testing.T) {
	for _, runtimeName := range genericProviderTestRuntimes {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := newGenericProviderTestFixture(t, runtimeName)
			fixture.seedNonTargetConfig(t)
			legacyRecord := fixture.cfg
			legacyRecord.RuntimeCLICommand = ""
			legacyRecord.MCPCommand = ""
			legacyRecord.MCPEnvironment = nil
			legacyRecord.RuntimeConfigRoot = ""
			legacyRecord.RuntimeMCPConfigPath = ""
			legacyExpected, err := hydrateLegacyGenericProviderConfig(legacyRecord, fixture.cli, fixture.witself)
			if err != nil {
				t.Fatal(err)
			}
			if len(legacyExpected.MCPEnvironment) != 0 {
				t.Fatalf("legacy expected environment = %#v, want empty", legacyExpected.MCPEnvironment)
			}
			legacyBinding, err := genericMCPBindingFromConfig(legacyExpected)
			if err != nil {
				t.Fatal(err)
			}
			if err := addGenericMCPUnchecked(fixture.cli, legacyExpected, legacyBinding); err != nil {
				t.Fatal(err)
			}
			if err := prepareGenericMCPInstall(fixture.cli, fixture.cfg, &legacyExpected); err != nil {
				t.Fatalf("legacy migration preflight: %v", err)
			}
			if err := registerGenericMCP(fixture.cli, fixture.cfg, &legacyExpected); err != nil {
				t.Fatalf("legacy migration: %v", err)
			}
			current, exists, _, err := inspectGenericMCP(fixture.cfg)
			if err != nil {
				t.Fatal(err)
			}
			desired, _ := genericMCPBindingFromConfig(fixture.cfg)
			if !exists || !equalGenericMCPBinding(current, desired) || current.Environment["WITSELF_HOME"] != fixture.witselfHome {
				t.Fatalf("migrated binding = %#v, exists=%t", current, exists)
			}
			if err := unregisterGenericMCP(fixture.cli, fixture.cfg); err != nil {
				t.Fatal(err)
			}

			if err := addGenericMCPUnchecked(fixture.cli, legacyExpected, legacyBinding); err != nil {
				t.Fatal(err)
			}
			if err := unregisterGenericMCP(fixture.cli, legacyExpected); err != nil {
				t.Fatalf("legacy uninstall: %v", err)
			}
		})
	}
}

func TestGenericProviderSelectorCLIHomeAndSymlinkDriftFailClosed(t *testing.T) {
	for _, runtimeName := range genericProviderTestRuntimes {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := newGenericProviderTestFixture(t, runtimeName)
			fixture.seedNonTargetConfig(t)

			t.Setenv(fixture.selectorName, filepath.Join(fixture.root, "other-provider"))
			wantSelectorError := "selector changed"
			if runtimeName == transcriptcapture.RuntimeCursor {
				wantSelectorError = "not supported by cursor-agent"
			}
			if _, err := validateGenericProviderCurrentSelection(fixture.cfg); err == nil || !strings.Contains(err.Error(), wantSelectorError) {
				t.Fatalf("selector drift error = %v", err)
			}
			if runtimeName == transcriptcapture.RuntimeCursor {
				t.Setenv(fixture.selectorName, "")
			} else {
				t.Setenv(fixture.selectorName, fixture.selectorRoot)
			}

			t.Setenv("WITSELF_HOME", filepath.Join(fixture.root, "other-witself"))
			if _, err := validateGenericProviderCurrentSelection(fixture.cfg); err == nil || !strings.Contains(err.Error(), "WITSELF_HOME changed") {
				t.Fatalf("WITSELF_HOME drift error = %v", err)
			}
			t.Setenv("WITSELF_HOME", fixture.witselfHome)

			alternateCLI := filepath.Join(fixture.root, "alternate-provider")
			if runtime.GOOS == "windows" {
				alternateCLI += ".exe"
			}
			rawCLI, err := os.ReadFile(fixture.cli)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(alternateCLI, rawCLI, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv(fixture.cliEnv, alternateCLI)
			if _, err := validateGenericProviderCurrentSelection(fixture.cfg); err == nil || !strings.Contains(err.Error(), "CLI selection changed") {
				t.Fatalf("CLI drift error = %v", err)
			}

			t.Setenv(fixture.cliEnv, fixture.cli)
			if err := os.Remove(fixture.cfg.RuntimeMCPConfigPath); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(fixture.root, "foreign-config")
			if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, fixture.cfg.RuntimeMCPConfigPath); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			if _, _, _, err := inspectGenericMCP(fixture.cfg); err == nil || !strings.Contains(err.Error(), "non-symlink") {
				t.Fatalf("symlink config error = %v", err)
			}
		})
	}
}

func TestGenericProviderSnapshotRejectsIdentityContentAndParentReplacement(t *testing.T) {
	fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeCursor)
	snapshot := fixture.seedNonTargetConfig(t)
	raw := append([]byte(nil), snapshot.raw...)

	replacement := filepath.Join(fixture.root, "same-bytes-replacement.json")
	if err := os.WriteFile(replacement, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, fixture.cfg.RuntimeMCPConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := genericSnapshotStillCurrent(fixture.cfg, snapshot); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("same-content identity replacement error = %v", err)
	}

	snapshot, _, err := readGenericMCPConfig(fixture.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.cfg.RuntimeMCPConfigPath, append(raw, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := genericSnapshotStillCurrent(fixture.cfg, snapshot); err == nil || !strings.Contains(err.Error(), "changed before mutation") {
		t.Fatalf("content replacement error = %v", err)
	}

	realRoot := fixture.selectorRoot + "-moved"
	if err := os.Rename(fixture.selectorRoot, realRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, fixture.selectorRoot); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	if _, _, _, err := inspectGenericMCP(fixture.cfg); err == nil || !strings.Contains(err.Error(), "non-symlink directory") {
		t.Fatalf("provider root replacement error = %v", err)
	}
}

func TestGrokGenericTopologyVerifiesEffectiveNativeRegistration(t *testing.T) {
	fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeGrokBuild)
	fixture.seedNonTargetConfig(t)
	if err := registerGenericMCP(fixture.cli, fixture.cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err := validateGenericInstalledTopology(fixture.cfg); err != nil {
		t.Fatalf("healthy Grok topology: %v", err)
	}

	if err := os.Remove(fixture.statePath); err != nil {
		t.Fatal(err)
	}
	err := validateGenericInstalledTopology(fixture.cfg)
	if err == nil || !strings.Contains(err.Error(), "contains 0 native witself registrations") {
		t.Fatalf("missing Grok effective registration error = %v", err)
	}
	var classified *integrationTopologyClassError
	if errors.As(err, &classified) {
		t.Fatalf("missing Grok registration was misclassified as %s: %v", classified.class, err)
	}

	t.Setenv(fakeGenericLargeErrorEnv, "1")
	err = validateGenericInstalledTopology(fixture.cfg)
	classified = nil
	if !errors.As(err, &classified) || classified.class != integrationVerificationUnavailable {
		t.Fatalf("unavailable Grok effective probe error = %v", err)
	}
	t.Setenv(fakeGenericLargeErrorEnv, "")

	drifted := map[string]any{
		"command": "/foreign/grok-shadow", "args": []string{"serve"},
		"env": map[string]string{}, "enabled": true, "name": "witself", "scope": "user",
	}
	raw, err := json.Marshal(drifted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err = validateGenericInstalledTopology(fixture.cfg)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("effective Grok drift error = %v", err)
	}
	classified = nil
	if errors.As(err, &classified) {
		t.Fatalf("semantic Grok drift was misclassified as %s: %v", classified.class, err)
	}
}

func TestGenericProviderAppearanceRaceReportsUntouchedAndPreservesForeignBinding(t *testing.T) {
	for _, runtimeName := range genericProviderTestRuntimes {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := newGenericProviderTestFixture(t, runtimeName)
			fixture.seedNonTargetConfig(t)
			if err := prepareGenericMCPInstall(fixture.cli, fixture.cfg, nil); err != nil {
				t.Fatal(err)
			}
			binding, err := genericMCPBindingFromConfig(fixture.cfg)
			if err != nil {
				t.Fatal(err)
			}
			// This byte-identical registration has no Witself integration record;
			// it therefore remains foreign even though its payload matches what
			// Witself intended to add after the completed preflight.
			if err := addGenericMCPUnchecked(fixture.cli, fixture.cfg, binding); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(fixture.cfg.RuntimeMCPConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			touched, err := registerGenericMCPWithMutation(fixture.cli, fixture.cfg, nil)
			if err == nil || !strings.Contains(err.Error(), "appeared before registration") {
				t.Fatalf("appearance race error = %v", err)
			}
			if touched {
				t.Fatal("appearance race was incorrectly reported as a Witself mutation")
			}
			after, err := os.ReadFile(fixture.cfg.RuntimeMCPConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("foreign exact binding changed after appearance-race refusal")
			}
		})
	}
}

func TestGenericProviderFreshDefaultClaudeRootIsCreatedByOperationLock(t *testing.T) {
	fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeClaudeCode)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	cfg := fixture.cfg
	if err := configureGenericProviderBinding(&cfg, fixture.cli, fixture.witself); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.RuntimeConfigRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh default Claude root unexpectedly exists: %v", err)
	}
	release, err := acquireRuntimeIntegrationOperationLock(transcriptcapture.RuntimeClaudeCode)
	if err != nil {
		t.Fatalf("create fresh Claude operation root: %v", err)
	}
	defer release()
	if info, err := os.Lstat(cfg.RuntimeConfigRoot); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("fresh Claude root info=%v err=%v", info, err)
	}
	if cfg.RuntimeMCPConfigPath != filepath.Join(filepath.Dir(cfg.RuntimeConfigRoot), ".claude.json") {
		t.Fatalf("default Claude MCP path = %s", cfg.RuntimeMCPConfigPath)
	}
	if err := prepareGenericMCPInstall(fixture.cli, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if touched, err := registerGenericMCPWithMutation(fixture.cli, cfg, nil); err != nil || !touched {
		t.Fatalf("fresh default Claude registration touched=%t err=%v", touched, err)
	}
	if err := validateGenericInstalledTopology(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestGenericProviderRemoveThenErrorRestoresExactBinding(t *testing.T) {
	for _, runtimeName := range []string{
		transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeGrokBuild,
	} {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := newGenericProviderTestFixture(t, runtimeName)
			fixture.seedNonTargetConfig(t)
			previous := fixture.cfg
			previous.Agent = "previous-agent"
			previous.AgentName = previous.Agent
			if err := registerGenericMCP(fixture.cli, previous, nil); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(previous.RuntimeMCPConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			failAfterRemove := func() {
				marker := filepath.Join(fixture.root, "fail-after-remove")
				if err := os.WriteFile(marker, []byte("fail\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv(fakeGenericFailRemoveEnv, marker)
			}

			failAfterRemove()
			if err := unregisterGenericMCP(fixture.cli, previous); err == nil || !strings.Contains(err.Error(), "after removal") {
				t.Fatalf("uninstall remove-then-error = %v", err)
			}
			after, err := os.ReadFile(previous.RuntimeMCPConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("uninstall remove-then-error did not restore exact provider bytes")
			}

			desired := previous
			desired.Agent = "desired-agent"
			desired.AgentName = desired.Agent
			failAfterRemove()
			touched, err := registerGenericMCPWithMutation(fixture.cli, desired, &previous)
			if err == nil || !touched || !strings.Contains(err.Error(), "after removal") {
				t.Fatalf("reinstall remove-then-error touched=%t err=%v", touched, err)
			}
			after, err = os.ReadFile(previous.RuntimeMCPConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("reinstall remove-then-error did not restore exact provider bytes")
			}
		})
	}
}

func TestGenericProviderRollbackRefusesConcurrentTargetReplacement(t *testing.T) {
	fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeCursor)
	fixture.seedNonTargetConfig(t)
	_, _, before, err := inspectGenericMCP(fixture.cfg)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := genericMCPBindingFromConfig(fixture.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCursorMCPBinding(fixture.cfg, binding); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(fixture.cfg.RuntimeMCPConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	foreign := bytes.Replace(raw, []byte(fixture.cfg.MCPCommand), []byte("/foreign/concurrent-command"), 1)
	if bytes.Equal(foreign, raw) {
		t.Fatal("test could not replace attempted target")
	}
	if err := os.WriteFile(fixture.cfg.RuntimeMCPConfigPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	err = restoreGenericSnapshotAfterFailure(fixture.cli, fixture.cfg, before, errors.New("simulated provider failure"))
	if err == nil || !strings.Contains(err.Error(), "foreign or concurrent") {
		t.Fatalf("concurrent target rollback error = %v", err)
	}
	after, err := os.ReadFile(fixture.cfg.RuntimeMCPConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, foreign) {
		t.Fatal("rollback overwrote a concurrent foreign target")
	}
}
