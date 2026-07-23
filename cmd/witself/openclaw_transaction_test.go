package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func openClawTransactionTestConfig(t *testing.T, fixture openClawIntegrationFixture) transcriptcapture.Config {
	t.Helper()
	location, err := transcriptcapture.EnsureLocation("home")
	if err != nil {
		t.Fatal(err)
	}
	return transcriptcapture.Config{
		SchemaVersion:  transcriptcapture.SchemaVersion,
		Runtime:        transcriptcapture.RuntimeOpenClaw,
		RuntimeVersion: "2026.7.1-2", RuntimeCLICommand: fixture.cli,
		MCPCommand: fixture.witself, MCPEnvironment: fixture.mcpEnvironment(t),
		MCPConnectTimeoutSeconds: openClawMCPConnectTimeoutSeconds,
		RuntimeWorkspace:         fixture.workspace, RuntimeAgentID: "main",
		CaptureMode: transcriptcapture.ModeRaw, HookMode: transcriptcapture.HookModeNone,
		Account: "default", AccountID: "acc_1", Realm: "default", RealmID: "realm_1",
		Agent: "scott", AgentID: "agent_1", AgentName: "scott",
		Endpoint: fixture.serverURL, TokenFile: fixture.token, Location: location,
		InstalledAt: time.Now().UTC(),
	}
}

func writeOpenClawTransactionTestBinding(
	t *testing.T,
	fixture openClawIntegrationFixture,
	cfg transcriptcapture.Config,
) {
	t.Helper()
	binding, err := openClawMCPBindingFromConfig(cfg.MCPCommand, cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]openClawMCPBinding{"witself": binding})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.state, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenClawTransactionRecoversInterruptedInstallBoundaries(t *testing.T) {
	for _, boundary := range []string{"after add", "after config finalize"} {
		t.Run(boundary, func(t *testing.T) {
			fixture := setupOpenClawIntegrationFixture(t)
			if err := os.MkdirAll(fixture.workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			desired := openClawTransactionTestConfig(t, fixture)
			journal, err := beginOpenClawTransaction(openClawTransactionInstall, nil, &desired)
			if err != nil {
				t.Fatal(err)
			}
			writeOpenClawTransactionTestBinding(t, fixture, desired)
			if boundary == "after config finalize" {
				if err := transcriptcapture.SaveConfig(desired); err != nil {
					t.Fatal(err)
				}
			}

			if err := recoverOpenClawTransaction(fixture.stateDir); err != nil {
				t.Fatalf("recover %s: %v", boundary, err)
			}
			loaded, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw)
			if err != nil {
				t.Fatal(err)
			}
			if !equalOpenClawTransactionConfig(loaded, desired) {
				t.Fatalf("recovered config = %#v, want %#v", loaded, desired)
			}
			current, exists, _, err := inspectOpenClawMCPState(fixture.cli, desired.MCPEnvironment)
			if err != nil || !exists {
				t.Fatalf("recovered MCP exists=%t err=%v", exists, err)
			}
			expected, err := openClawMCPBindingFromConfig(desired.MCPCommand, desired)
			if err != nil || !equalOpenClawMCPBinding(current, expected) {
				t.Fatalf("recovered MCP = %#v, want %#v, err=%v", current, expected, err)
			}
			routingCurrent, err := runtimeMemoryRoutingCurrentAt(transcriptcapture.RuntimeOpenClaw, desired.RuntimeWorkspace)
			if err != nil || !routingCurrent {
				t.Fatalf("recovered routing current=%t err=%v", routingCurrent, err)
			}
			if _, err := loadOpenClawTransactionJournal(fixture.stateDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal remains after recovery: %v (%#v)", err, journal)
			}
		})
	}
}

func TestOpenClawTransactionRecoversInterruptedUninstall(t *testing.T) {
	fixture := setupOpenClawIntegrationFixture(t)
	if err := os.MkdirAll(fixture.workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	previous := openClawTransactionTestConfig(t, fixture)
	if err := transcriptcapture.SaveConfig(previous); err != nil {
		t.Fatal(err)
	}
	if _, err := installRuntimeMemoryRoutingInstructionsAt(transcriptcapture.RuntimeOpenClaw, previous.RuntimeWorkspace); err != nil {
		t.Fatal(err)
	}
	writeOpenClawTransactionTestBinding(t, fixture, previous)
	if _, err := beginOpenClawTransaction(openClawTransactionUninstall, &previous, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixture.state); err != nil {
		t.Fatal(err)
	}

	if err := recoverOpenClawTransaction(fixture.stateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("integration config remains: %v", err)
	}
	routingCurrent, err := runtimeMemoryRoutingCurrentAt(transcriptcapture.RuntimeOpenClaw, previous.RuntimeWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if routingCurrent {
		t.Fatal("managed routing remains after recovered uninstall")
	}
	if _, err := loadOpenClawTransactionJournal(fixture.stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remains after recovered uninstall: %v", err)
	}
}

func TestOpenClawTransactionRecoveryUsesPersistedRootAfterSelectorDrift(t *testing.T) {
	for _, operation := range []string{openClawTransactionInstall, openClawTransactionUninstall} {
		t.Run(operation, func(t *testing.T) {
			fixture := setupOpenClawIntegrationFixture(t)
			if err := os.MkdirAll(fixture.workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := openClawTransactionTestConfig(t, fixture)
			if err := transcriptcapture.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			writeOpenClawTransactionTestBinding(t, fixture, cfg)
			if operation == openClawTransactionUninstall {
				if _, err := installRuntimeMemoryRoutingInstructionsAt(
					transcriptcapture.RuntimeOpenClaw,
					cfg.RuntimeWorkspace,
				); err != nil {
					t.Fatal(err)
				}
				if _, err := beginOpenClawTransaction(operation, &cfg, nil); err != nil {
					t.Fatal(err)
				}
			} else if _, err := beginOpenClawTransaction(operation, &cfg, &cfg); err != nil {
				t.Fatal(err)
			}

			driftRoot := filepath.Join(fixture.home, "drifted-openclaw-state")
			t.Setenv("OPENCLAW_STATE_DIR", driftRoot)
			t.Setenv("OPENCLAW_CONFIG_PATH", filepath.Join(driftRoot, "openclaw.json"))
			t.Setenv("OPENCLAW_PROFILE", "drifted")

			recoveryRoot, err := openClawOperationLockRoot()
			if err != nil {
				t.Fatal(err)
			}
			if recoveryRoot != fixture.stateDir {
				t.Fatalf("recovery root = %q, want persisted %q", recoveryRoot, fixture.stateDir)
			}
			release, err := acquireProviderIntegrationOperationLock(transcriptcapture.RuntimeOpenClaw)
			if err != nil {
				t.Fatal(err)
			}
			release()
			if _, err := os.Lstat(filepath.Join(fixture.stateDir, ".witself-openclaw-operation.lock")); err != nil {
				t.Fatalf("persisted-root operation lock is missing: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(driftRoot, ".witself-openclaw-operation.lock")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("selector-drift root received an operation lock: %v", err)
			}

			if err := recoverOpenClawTransaction(recoveryRoot); err != nil {
				t.Fatalf("recover %s after selector drift: %v", operation, err)
			}
			if _, err := os.Lstat(openClawTransactionPath(fixture.stateDir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("persisted-root journal remains after %s recovery: %v", operation, err)
			}
			_, configErr := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw)
			if operation == openClawTransactionInstall && configErr != nil {
				t.Fatalf("recovered install config is missing: %v", configErr)
			}
			if operation == openClawTransactionUninstall && !errors.Is(configErr, os.ErrNotExist) {
				t.Fatalf("recovered uninstall config remains: %v", configErr)
			}
		})
	}
}

func TestOpenClawInstallStopsAfterRecoveredUninstallChangesLockRoot(t *testing.T) {
	fixture := setupOpenClawIntegrationFixture(t)
	if err := os.MkdirAll(fixture.workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := openClawTransactionTestConfig(t, fixture)
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	writeOpenClawTransactionTestBinding(t, fixture, cfg)
	if _, err := installRuntimeMemoryRoutingInstructionsAt(
		transcriptcapture.RuntimeOpenClaw,
		cfg.RuntimeWorkspace,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := beginOpenClawTransaction(openClawTransactionUninstall, &cfg, nil); err != nil {
		t.Fatal(err)
	}

	driftRoot := filepath.Join(fixture.home, "drifted-openclaw-state")
	t.Setenv("OPENCLAW_STATE_DIR", driftRoot)
	t.Setenv("OPENCLAW_CONFIG_PATH", filepath.Join(driftRoot, "openclaw.json"))
	t.Setenv("OPENCLAW_PROFILE", "drifted")

	_, stderr, code := captureIntegrationsCLI(t, func() int {
		return installCmd([]string{transcriptcapture.RuntimeOpenClaw})
	})
	if code != 1 || !strings.Contains(stderr, "rerun install") {
		t.Fatalf("install after recovered uninstall code=%d stderr=%q", code, stderr)
	}
	if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeOpenClaw); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered uninstall config remains: %v", err)
	}
	if _, err := os.Lstat(openClawTransactionPath(fixture.stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered uninstall journal remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(driftRoot, ".witself-openclaw-operation.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("install mutated or locked the new selector root before rerun: %v", err)
	}
}

func TestOpenClawTransactionJournalRejectsTampering(t *testing.T) {
	for _, variant := range []string{"duplicate", "unknown", "workspace", "runtime agent"} {
		t.Run(variant, func(t *testing.T) {
			fixture := setupOpenClawIntegrationFixture(t)
			previous := openClawTransactionTestConfig(t, fixture)
			desired := previous
			writeOpenClawTransactionTestBinding(t, fixture, previous)
			if _, err := beginOpenClawTransaction(openClawTransactionInstall, &previous, &desired); err != nil {
				t.Fatal(err)
			}
			path := openClawTransactionPath(fixture.stateDir)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "duplicate":
				raw = bytes.Replace(raw, []byte(`"operation": "install",`), []byte(`"operation": "install", "operation": "uninstall",`), 1)
			case "unknown":
				raw = bytes.Replace(raw, []byte(`"operation": "install",`), []byte(`"operation": "install", "foreign": true,`), 1)
			default:
				var document map[string]any
				if err := json.Unmarshal(raw, &document); err != nil {
					t.Fatal(err)
				}
				desiredDocument := document["desired"].(map[string]any)
				if variant == "workspace" {
					desiredDocument["runtime_workspace"] = filepath.Join(fixture.home, "foreign-workspace")
				} else {
					desiredDocument["runtime_agent_id"] = "foreign-agent"
				}
				raw, err = json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = loadOpenClawTransactionJournal(fixture.stateDir)
			if err == nil {
				t.Fatalf("%s journal tampering was accepted", variant)
			}
			if variant == "workspace" && !strings.Contains(err.Error(), "owned workspace") {
				t.Fatalf("workspace tamper error = %v", err)
			}
			if variant == "runtime agent" && !strings.Contains(err.Error(), "runtime agent") {
				t.Fatalf("runtime agent tamper error = %v", err)
			}
		})
	}
}
