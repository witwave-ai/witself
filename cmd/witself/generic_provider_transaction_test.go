package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func genericTransactionConfig(fixture genericProviderTestFixture) transcriptcapture.Config {
	cfg := fixture.cfg
	cfg.SchemaVersion = transcriptcapture.SchemaVersion
	cfg.InstalledAt = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	return cfg
}

func TestGenericProviderTransactionRecoversInterruptedFirstInstall(t *testing.T) {
	for _, runtimeName := range genericProviderTestRuntimes {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := newGenericProviderTestFixture(t, runtimeName)
			before := fixture.seedNonTargetConfig(t)
			desired := genericTransactionConfig(fixture)
			preflight, err := prepareGenericMCPInstallSnapshot(fixture.cli, desired, nil)
			if err != nil {
				t.Fatal(err)
			}
			journal, err := beginGenericProviderInstallTransaction(nil, nil, desired, desired, preflight)
			if err != nil {
				t.Fatal(err)
			}
			if err := transcriptcapture.SaveConfig(desired); err != nil {
				t.Fatal(err)
			}

			// This is the exact SIGKILL window that used to leave a staged
			// integration record with no provider registration.
			if err := recoverGenericProviderTransaction(runtimeName); err != nil {
				t.Fatal(err)
			}
			expected, err := genericMCPBindingFromConfig(desired)
			if err != nil {
				t.Fatal(err)
			}
			current, exists, after, err := inspectGenericMCP(desired)
			if err != nil {
				t.Fatal(err)
			}
			if !exists || !equalGenericMCPBinding(current, expected) || after.nonTarget != before.nonTarget {
				t.Fatalf("recovered binding = %#v, exists=%t, non-target preserved=%t", current, exists, after.nonTarget == before.nonTarget)
			}
			if _, err := loadGenericProviderTransactionJournal(runtimeName); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("transaction journal remains after recovery of %s (%s): %v", runtimeName, journal.ID, err)
			}
		})
	}
}

func TestInstallStopsWhenFirstInstallRecoveryChangesGenericProviderLockRoot(t *testing.T) {
	fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeCodex)
	before := fixture.seedNonTargetConfig(t)
	desired := genericTransactionConfig(fixture)
	preflight, err := prepareGenericMCPInstallSnapshot(fixture.cli, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beginGenericProviderInstallTransaction(nil, nil, desired, desired, preflight); err != nil {
		t.Fatal(err)
	}

	// Model selector drift at the exact post-recovery boundary. The interrupted
	// first install has not saved its staged config, so recovery rolls it back;
	// the active invocation must not proceed after the root it locked changes.
	originalResolver := genericProviderOperationLockRootAfterRecovery
	t.Cleanup(func() { genericProviderOperationLockRootAfterRecovery = originalResolver })
	driftedRoot := fixture.selectorRoot + "-drifted"
	genericProviderOperationLockRootAfterRecovery = func(runtimeName string) (string, error) {
		if runtimeName != transcriptcapture.RuntimeCodex {
			return "", errors.New("unexpected runtime in post-recovery root check")
		}
		return driftedRoot, nil
	}

	if code := installCmd([]string{transcriptcapture.RuntimeCodex}); code != 1 {
		t.Fatalf("install after provider-root drift code = %d, want 1", code)
	}
	if _, err := loadGenericProviderTransactionJournal(desired.Runtime); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rolled-back first-install journal remains: %v", err)
	}
	if _, err := transcriptcapture.LoadConfig(desired.Runtime); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rolled-back first install retained integration config: %v", err)
	}
	_, exists, after, err := inspectGenericMCP(desired)
	if err != nil || exists || after.nonTarget != before.nonTarget {
		t.Fatalf("first-install recovery state: exists=%t non-target-preserved=%t err=%v", exists, after.nonTarget == before.nonTarget, err)
	}
	if _, err := os.Lstat(filepath.Join(driftedRoot, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root-drifted install mutated the unlocked provider root: %v", err)
	}
}

func TestGenericProviderTransactionRecoversExactHooksAcrossCommitBoundaries(t *testing.T) {
	testCases := []struct {
		name     string
		runtime  string
		hookMode string
	}{
		{"codex-user", transcriptcapture.RuntimeCodex, transcriptcapture.HookModeUser},
		{"claude-user", transcriptcapture.RuntimeClaudeCode, transcriptcapture.HookModeUser},
		{"grok-user", transcriptcapture.RuntimeGrokBuild, transcriptcapture.HookModeUser},
		{"cursor-user", transcriptcapture.RuntimeCursor, transcriptcapture.HookModeUser},
		{"codex-managed", transcriptcapture.RuntimeCodex, transcriptcapture.HookModeManaged},
		{"claude-managed", transcriptcapture.RuntimeClaudeCode, transcriptcapture.HookModeManaged},
	}
	for _, tc := range testCases {
		for _, saveFinal := range []bool{false, true} {
			boundary := "hooks-committed-before-final-config"
			if saveFinal {
				boundary = "enriched-final-config-before-journal-clear"
			}
			t.Run(tc.name+"/"+boundary, func(t *testing.T) {
				if tc.hookMode == transcriptcapture.HookModeManaged && !supportsManagedHooks(tc.runtime) {
					t.Skip("managed hooks are not supported on this platform")
				}
				fixture := newGenericProviderTestFixture(t, tc.runtime)
				before := fixture.seedNonTargetConfig(t)
				desired := genericTransactionConfig(fixture)
				desired.HookMode = tc.hookMode
				if err := planRuntimeHooksOwned(&desired, nil); err != nil {
					t.Fatal(err)
				}
				planned := desired
				preflight, err := prepareGenericMCPInstallSnapshot(fixture.cli, desired, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := beginGenericProviderInstallTransaction(nil, nil, desired, desired, preflight); err != nil {
					t.Fatal(err)
				}
				if err := transcriptcapture.SaveConfig(desired); err != nil {
					t.Fatal(err)
				}
				if _, err := installRuntimeMemoryRoutingInstructionsAt(tc.runtime, desired.RuntimeWorkspace); err != nil {
					t.Fatal(err)
				}
				if err := registerGenericMCP(fixture.cli, desired, nil); err != nil {
					t.Fatal(err)
				}
				if _, _, err := installRuntimeHooksOwned(&desired, nil); err != nil {
					t.Fatal(err)
				}
				if saveFinal {
					if err := transcriptcapture.SaveConfig(desired); err != nil {
						t.Fatal(err)
					}
				}

				if err := recoverGenericProviderTransaction(tc.runtime); err != nil {
					t.Fatal(err)
				}
				stored, err := transcriptcapture.LoadConfig(tc.runtime)
				if err != nil {
					t.Fatal(err)
				}
				if stored.HookConfigPath != planned.HookConfigPath ||
					stored.HookManagedDir != planned.HookManagedDir ||
					stored.HookRunnerPath != planned.HookRunnerPath ||
					stored.HookRunnerDigest != planned.HookRunnerDigest ||
					stored.HookPolicyDigest != planned.HookPolicyDigest {
					t.Fatalf("recovered hook ownership = %#v, planned %#v", stored, planned)
				}
				if err := verifyRuntimeHooksOwned(stored); err != nil {
					t.Fatalf("verify recovered hooks: %v", err)
				}
				expected, err := genericMCPBindingFromConfig(stored)
				if err != nil {
					t.Fatal(err)
				}
				current, exists, after, err := inspectGenericMCP(stored)
				if err != nil || !exists || !equalGenericMCPBinding(current, expected) ||
					after.nonTarget != before.nonTarget {
					t.Fatalf("recovered provider = %#v exists=%t non-target-preserved=%t err=%v", current, exists, after.nonTarget == before.nonTarget, err)
				}
				if _, err := loadGenericProviderTransactionJournal(tc.runtime); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("transaction journal remains: %v", err)
				}
			})
		}
	}
}

func TestGenericProviderTransactionNeverAdoptsMarkerShapeWithoutExactPlan(t *testing.T) {
	t.Run("user", func(t *testing.T) {
		fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeClaudeCode)
		fixture.seedNonTargetConfig(t)
		desired := genericTransactionConfig(fixture)
		if err := planRuntimeHooksOwned(&desired, nil); err != nil {
			t.Fatal(err)
		}
		preflight, err := prepareGenericMCPInstallSnapshot(fixture.cli, desired, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := beginGenericProviderInstallTransaction(nil, nil, desired, desired, preflight); err != nil {
			t.Fatal(err)
		}
		if err := transcriptcapture.SaveConfig(desired); err != nil {
			t.Fatal(err)
		}
		if err := registerGenericMCP(fixture.cli, desired, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := transcriptcapture.InstallHooksWithWitselfHome(
			desired.Runtime,
			desired.CaptureMode,
			desired.MCPCommand,
			desired.Account,
			desired.Realm,
			"foreign-agent",
			desired.Location.Name,
			desired.MCPEnvironment["WITSELF_HOME"],
		); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(desired.HookConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := recoverGenericProviderTransaction(desired.Runtime); err == nil {
			t.Fatal("marker-shaped foreign user hooks were adopted during recovery")
		}
		after, err := os.ReadFile(desired.HookConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Fatal("refused user-hook recovery changed the foreign marker document")
		}
		if _, err := loadGenericProviderTransactionJournal(desired.Runtime); err != nil {
			t.Fatalf("refused recovery discarded its journal: %v", err)
		}
	})

	for _, runtimeName := range []string{transcriptcapture.RuntimeCodex, transcriptcapture.RuntimeClaudeCode} {
		t.Run(runtimeName+"-managed", func(t *testing.T) {
			if !supportsManagedHooks(runtimeName) {
				t.Skip("managed hooks are not supported on this platform")
			}
			fixture := newGenericProviderTestFixture(t, runtimeName)
			fixture.seedNonTargetConfig(t)
			desired := genericTransactionConfig(fixture)
			desired.HookMode = transcriptcapture.HookModeManaged
			if err := planRuntimeHooksOwned(&desired, nil); err != nil {
				t.Fatal(err)
			}
			preflight, err := prepareGenericMCPInstallSnapshot(fixture.cli, desired, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := beginGenericProviderInstallTransaction(nil, nil, desired, desired, preflight); err != nil {
				t.Fatal(err)
			}
			if err := transcriptcapture.SaveConfig(desired); err != nil {
				t.Fatal(err)
			}
			if err := registerGenericMCP(fixture.cli, desired, nil); err != nil {
				t.Fatal(err)
			}
			foreign, err := managedHooksOptions(
				runtimeName,
				desired.CaptureMode,
				desired.MCPCommand,
				desired.Account,
				desired.Realm,
				"foreign-agent",
				desired.Location.Name,
			)
			if err != nil {
				t.Fatal(err)
			}
			foreign.WitselfHome = desired.MCPEnvironment["WITSELF_HOME"]
			if _, err := transcriptcapture.InstallManagedHooks(foreign); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(desired.HookConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := recoverGenericProviderTransaction(runtimeName); err == nil {
				t.Fatal("marker-shaped foreign managed hooks were adopted during recovery")
			}
			after, err := os.ReadFile(desired.HookConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("refused managed-hook recovery changed the foreign marker policy")
			}
			if _, err := loadGenericProviderTransactionJournal(runtimeName); err != nil {
				t.Fatalf("refused recovery discarded its journal: %v", err)
			}
		})
	}
}

func TestGenericProviderTransactionRecoversInterruptedRebindAfterRemove(t *testing.T) {
	for _, runtimeName := range genericProviderTestRuntimes {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := newGenericProviderTestFixture(t, runtimeName)
			before := fixture.seedNonTargetConfig(t)
			previous := genericTransactionConfig(fixture)
			if err := registerGenericMCP(fixture.cli, previous, nil); err != nil {
				t.Fatal(err)
			}
			if err := transcriptcapture.SaveConfig(previous); err != nil {
				t.Fatal(err)
			}
			persistedPrevious, err := transcriptcapture.LoadConfig(runtimeName)
			if err != nil {
				t.Fatal(err)
			}
			desired := persistedPrevious
			desired.Agent = "provider-rebound-bot"
			desired.AgentID = "agent_provider_rebound"
			desired.AgentName = "provider-rebound-bot"
			desired.InstalledAt = desired.InstalledAt.Add(time.Minute)
			preflight, err := prepareGenericMCPInstallSnapshot(fixture.cli, desired, &persistedPrevious)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := beginGenericProviderInstallTransaction(
				&persistedPrevious,
				&persistedPrevious,
				desired,
				desired,
				preflight,
			); err != nil {
				t.Fatal(err)
			}
			if err := transcriptcapture.SaveConfig(desired); err != nil {
				t.Fatal(err)
			}
			if err := removeGenericMCPUnchecked(fixture.cli, desired); err != nil {
				t.Fatal(err)
			}

			// Simulate SIGKILL after the exact prior target was removed but before
			// the replacement add. Recovery must add only the journaled desired
			// binding and retain all non-target provider state.
			if err := recoverGenericProviderTransaction(runtimeName); err != nil {
				t.Fatal(err)
			}
			expected, err := genericMCPBindingFromConfig(desired)
			if err != nil {
				t.Fatal(err)
			}
			current, exists, after, err := inspectGenericMCP(desired)
			if err != nil {
				t.Fatal(err)
			}
			if !exists || !equalGenericMCPBinding(current, expected) || after.nonTarget != before.nonTarget {
				t.Fatalf("recovered rebind = %#v, exists=%t, non-target preserved=%t", current, exists, after.nonTarget == before.nonTarget)
			}
			stored, err := transcriptcapture.LoadConfig(runtimeName)
			if err != nil || stored.Agent != desired.Agent {
				t.Fatalf("recovered config = %#v, %v", stored, err)
			}
		})
	}
}

func TestGenericProviderUninstallTransactionRollsBackBeforeCommit(t *testing.T) {
	for _, runtimeName := range genericProviderTestRuntimes {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := newGenericProviderTestFixture(t, runtimeName)
			before := fixture.seedNonTargetConfig(t)
			cfg := genericTransactionConfig(fixture)
			if err := registerGenericMCP(fixture.cli, cfg, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := installRuntimeMemoryRoutingInstructionsAt(runtimeName, cfg.RuntimeWorkspace); err != nil {
				t.Fatal(err)
			}
			if _, _, err := installRuntimeHooksOwned(&cfg, nil); err != nil {
				t.Fatal(err)
			}
			if err := transcriptcapture.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			_, exists, providerBefore, err := inspectGenericMCP(cfg)
			if err != nil || !exists {
				t.Fatalf("inspect uninstall preimage: exists=%t err=%v", exists, err)
			}
			if _, err := beginGenericProviderUninstallTransaction(cfg, cfg, providerBefore); err != nil {
				t.Fatal(err)
			}
			if err := removeGenericMCPUnchecked(fixture.cli, cfg); err != nil {
				t.Fatal(err)
			}

			// With the integration record still present, it is the commit marker
			// for rollback. Recovery must restore the exact provider preimage.
			if err := recoverGenericProviderTransaction(runtimeName); err != nil {
				t.Fatal(err)
			}
			expected, err := genericMCPBindingFromConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			current, exists, after, err := inspectGenericMCP(cfg)
			if err != nil || !exists || !equalGenericMCPBinding(current, expected) || after.nonTarget != before.nonTarget {
				t.Fatalf("uninstall rollback = %#v exists=%t non-target-preserved=%t err=%v", current, exists, after.nonTarget == before.nonTarget, err)
			}
			if _, err := transcriptcapture.LoadConfig(runtimeName); err != nil {
				t.Fatalf("uninstall rollback lost integration record: %v", err)
			}
			if _, err := loadGenericProviderTransactionJournal(runtimeName); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("uninstall rollback journal remains: %v", err)
			}
		})
	}
}

func TestGenericProviderUninstallTransactionRollsForwardAfterCommit(t *testing.T) {
	for _, runtimeName := range genericProviderTestRuntimes {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := newGenericProviderTestFixture(t, runtimeName)
			before := fixture.seedNonTargetConfig(t)
			cfg := genericTransactionConfig(fixture)
			if err := registerGenericMCP(fixture.cli, cfg, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := installRuntimeMemoryRoutingInstructionsAt(runtimeName, cfg.RuntimeWorkspace); err != nil {
				t.Fatal(err)
			}
			if _, _, err := installRuntimeHooksOwned(&cfg, nil); err != nil {
				t.Fatal(err)
			}
			if err := transcriptcapture.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			_, exists, providerBefore, err := inspectGenericMCP(cfg)
			if err != nil || !exists {
				t.Fatalf("inspect uninstall preimage: exists=%t err=%v", exists, err)
			}
			if _, err := beginGenericProviderUninstallTransaction(cfg, cfg, providerBefore); err != nil {
				t.Fatal(err)
			}
			if err := removeGenericMCPUnchecked(fixture.cli, cfg); err != nil {
				t.Fatal(err)
			}
			if err := transcriptcapture.RemoveConfig(runtimeName); err != nil {
				t.Fatal(err)
			}

			// Once the integration record is absent, recovery must finish the
			// uninstall rather than resurrecting credentials or provider state.
			if err := recoverGenericProviderTransaction(runtimeName); err != nil {
				t.Fatal(err)
			}
			_, exists, after, err := inspectGenericMCP(cfg)
			if err != nil || exists || after.nonTarget != before.nonTarget {
				t.Fatalf("uninstall roll-forward exists=%t non-target-preserved=%t err=%v", exists, after.nonTarget == before.nonTarget, err)
			}
			if _, err := transcriptcapture.LoadConfig(runtimeName); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("uninstall roll-forward restored integration record: %v", err)
			}
			if _, err := loadGenericProviderTransactionJournal(runtimeName); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("uninstall roll-forward journal remains: %v", err)
			}
		})
	}
}

func TestInstallStopsAfterRecoveringCommittedGenericProviderUninstall(t *testing.T) {
	fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeCodex)
	before := fixture.seedNonTargetConfig(t)
	cfg := genericTransactionConfig(fixture)
	if err := planRuntimeHooksOwned(&cfg, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := installRuntimeMemoryRoutingInstructionsAt(cfg.Runtime, cfg.RuntimeWorkspace); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installRuntimeHooksOwned(&cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err := registerGenericMCP(fixture.cli, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	_, exists, providerBefore, err := inspectGenericMCP(cfg)
	if err != nil || !exists {
		t.Fatalf("inspect uninstall preimage: exists=%t err=%v", exists, err)
	}
	if _, err := beginGenericProviderUninstallTransaction(cfg, cfg, providerBefore); err != nil {
		t.Fatal(err)
	}
	if err := removeGenericMCPUnchecked(fixture.cli, cfg); err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.RemoveConfig(cfg.Runtime); err != nil {
		t.Fatal(err)
	}

	// The interrupted uninstall was committed under the old selector. The next
	// install invocation acquires its provider lock before recovery; once
	// recovery removes the old durable binding, it must stop and require a rerun
	// rather than continue under a lock derived from state that no longer exists.
	if code := installCmd([]string{transcriptcapture.RuntimeCodex}); code != 1 {
		t.Fatalf("install after committed uninstall recovery code = %d, want 1", code)
	}
	if _, err := transcriptcapture.LoadConfig(cfg.Runtime); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery recreated integration config: %v", err)
	}
	if _, err := loadGenericProviderTransactionJournal(cfg.Runtime); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered uninstall journal remains: %v", err)
	}
	_, exists, after, err := inspectGenericMCP(cfg)
	if err != nil || exists || after.nonTarget != before.nonTarget {
		t.Fatalf("recovered old provider state: exists=%t non-target-preserved=%t err=%v", exists, after.nonTarget == before.nonTarget, err)
	}
	newRoot := fixture.selectorRoot + "-new"
	t.Setenv("CODEX_HOME", newRoot)
	if _, err := os.Lstat(filepath.Join(newRoot, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("install mutated the newly selected provider root before rerun: %v", err)
	}
}

func TestGenericProviderTransactionRefusesForeignTargetDuringRecovery(t *testing.T) {
	fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeCodex)
	fixture.seedNonTargetConfig(t)
	desired := genericTransactionConfig(fixture)
	preflight, err := prepareGenericMCPInstallSnapshot(fixture.cli, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beginGenericProviderInstallTransaction(nil, nil, desired, desired, preflight); err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(desired); err != nil {
		t.Fatal(err)
	}
	foreign := fixture.installForeignTarget(t)
	if err := recoverGenericProviderTransaction(fixture.runtime); err == nil {
		t.Fatal("foreign target was overwritten during transaction recovery")
	}
	after, err := os.ReadFile(desired.RuntimeMCPConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, foreign) {
		t.Fatal("foreign provider bytes changed during refused recovery")
	}
	if _, err := loadGenericProviderTransactionJournal(fixture.runtime); err != nil {
		t.Fatalf("recovery journal was not preserved after refusal: %v", err)
	}
}

func TestGenericProviderTransactionJournalRejectsDuplicateAndUnknownFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func([]byte) []byte
		want   string
	}{
		{
			name: "duplicate key",
			inject: func(raw []byte) []byte {
				return bytes.Replace(raw, []byte("{\n"), []byte("{\n  \"schema_version\": \"foreign\",\n"), 1)
			},
			want: "duplicate JSON object key",
		},
		{
			name: "unknown field",
			inject: func(raw []byte) []byte {
				return bytes.Replace(raw, []byte("{\n"), []byte("{\n  \"foreign_field\": true,\n"), 1)
			},
			want: "unknown field",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeCodex)
			fixture.seedNonTargetConfig(t)
			desired := genericTransactionConfig(fixture)
			preflight, err := prepareGenericMCPInstallSnapshot(fixture.cli, desired, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := beginGenericProviderInstallTransaction(nil, nil, desired, desired, preflight); err != nil {
				t.Fatal(err)
			}
			path, err := genericProviderTransactionPath(fixture.runtime)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.inject(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadGenericProviderTransactionJournal(fixture.runtime); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("malformed journal error = %v, want %q", err, tc.want)
			}
			if _, exists, _, err := inspectGenericMCP(desired); err != nil || exists {
				t.Fatalf("malformed journal changed provider state: exists=%t err=%v", exists, err)
			}
		})
	}
}

func TestIntegrationsVerifyReportsPendingGenericProviderTransactionReadOnly(t *testing.T) {
	fixture := newGenericProviderTestFixture(t, transcriptcapture.RuntimeCodex)
	fixture.seedNonTargetConfig(t)
	desired := genericTransactionConfig(fixture)
	preflight, err := prepareGenericMCPInstallSnapshot(fixture.cli, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beginGenericProviderInstallTransaction(nil, nil, desired, desired, preflight); err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(desired); err != nil {
		t.Fatal(err)
	}
	status := inspectIntegrationRuntime(transcriptcapture.RuntimeCodex, true)
	if status.Verification == nil || status.Verification.State != integrationVerificationIncomplete ||
		!strings.Contains(status.Verification.Message, "interrupted install") ||
		!strings.Contains(status.Verification.Message, "witself install codex") {
		t.Fatalf("verification with pending transaction = %#v", status)
	}
	if _, err := loadGenericProviderTransactionJournal(fixture.runtime); err != nil {
		t.Fatalf("read-only verify changed the pending journal: %v", err)
	}
	if _, exists, _, err := inspectGenericMCP(desired); err != nil || exists {
		t.Fatalf("read-only verify changed provider state: exists=%t err=%v", exists, err)
	}
}
