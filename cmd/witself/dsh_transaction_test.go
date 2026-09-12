package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

// installedDSHTestBinding saves the binding, installs both owned artifacts, and
// returns the persisted config so recovery assertions compare exact bytes.
func installedDSHTestBinding(t *testing.T, foreignPatch string) transcriptcapture.Config {
	t.Helper()
	cfg := configuredDSHTestConfig(t)
	stubDSHDumpConfig(t, dshComposedFixture(), nil)
	if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if foreignPatch != "" {
		if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(foreignPatch), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	persisted, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeDSH)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installDSHPatchBlock(persisted); err != nil {
		t.Fatal(err)
	}
	if _, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeDSH); err != nil {
		t.Fatal(err)
	}
	return persisted
}

func TestDSHTransactionRecoversInterruptedRebindAndFinalize(t *testing.T) {
	foreign := "# operator notes\n- id: existing-row\n  config:\n    retries: 3\n"
	for _, phase := range []string{"before patch write", "after patch write", "after config finalize"} {
		t.Run(phase, func(t *testing.T) {
			previous := installedDSHTestBinding(t, foreign)
			desired := previous
			desired.AgentID = "agt_rebound"
			desired.Agent = "rebound-agent"
			desired.AgentName = "rebound-agent"
			desiredBlock, err := dshManagedPatchBlock(desired)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := beginDSHTransaction(dshTransactionInstall, &previous, &desired); err != nil {
				t.Fatal(err)
			}
			if phase != "before patch write" {
				plan, _, err := prepareDSHPatchInstallPlan(desired.RuntimeCLICommand, desired, &previous)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := installDSHPatchBlockWithPlan(plan); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "after config finalize" {
				if err := transcriptcapture.SaveConfig(desired); err != nil {
					t.Fatal(err)
				}
			}

			if err := recoverDSHTransaction(desired.RuntimeConfigRoot); err != nil {
				t.Fatal(err)
			}
			installed, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeDSH)
			if err != nil {
				t.Fatal(err)
			}
			if !equalDSHTransactionConfig(installed, desired) {
				t.Fatalf("recovered config = %#v, want desired %#v", installed, desired)
			}
			snapshot, err := readDSHPatchSnapshot(desired.RuntimeMCPConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(snapshot.block()) != string(desiredBlock) {
				t.Fatalf("recovered block = %q", snapshot.block())
			}
			if !strings.HasPrefix(string(snapshot.raw), foreign) {
				t.Fatalf("recovery disturbed foreign rows: %q", snapshot.raw)
			}
			if err := validateDSHPersistedTopology(desired); err != nil {
				t.Fatalf("recovered topology: %v", err)
			}
			if _, err := os.Lstat(dshTransactionPath(desired.RuntimeConfigRoot)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("transaction journal remained after recovery: %v", err)
			}
		})
	}
}

// A FIRST install has no prior binding to fall back on. Interrupted after the
// patch write or after the config save, recovery must claim the block it
// wrote and converge instead of refusing its own fence.
func TestDSHTransactionRecoversInterruptedFirstInstall(t *testing.T) {
	foreign := "# operator notes\n- id: existing-row\n  config:\n    retries: 3\n"
	for _, phase := range []string{"after patch write", "after config save"} {
		t.Run(phase, func(t *testing.T) {
			cfg := configuredDSHTestConfig(t)
			stubDSHDumpConfig(t, dshComposedFixture(), nil)
			if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(foreign), 0o600); err != nil {
				t.Fatal(err)
			}
			desired := cfg
			if _, err := beginDSHTransaction(dshTransactionInstall, nil, &desired); err != nil {
				t.Fatal(err)
			}
			plan, _, err := prepareDSHPatchInstallPlan(desired.RuntimeCLICommand, desired, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := installDSHPatchBlockWithPlan(plan); err != nil {
				t.Fatal(err)
			}
			if phase == "after config save" {
				if err := transcriptcapture.SaveConfig(desired); err != nil {
					t.Fatal(err)
				}
			}
			if err := recoverDSHTransaction(desired.RuntimeConfigRoot); err != nil {
				t.Fatalf("recover interrupted first install (%s): %v", phase, err)
			}
			installed, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeDSH)
			if err != nil || !equalDSHTransactionConfig(installed, desired) {
				t.Fatalf("recovered config = %#v, %v", installed, err)
			}
			if err := validateDSHPersistedTopology(installed); err != nil {
				t.Fatalf("recovered topology: %v", err)
			}
			if !strings.HasPrefix(readDSHPatchFile(t, cfg.RuntimeMCPConfigPath), foreign) {
				t.Fatal("recovery disturbed foreign rows")
			}
			if _, err := os.Lstat(dshTransactionPath(desired.RuntimeConfigRoot)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("transaction journal remained after recovery: %v", err)
			}
		})
	}
}

func TestDSHTransactionRecoversInterruptedUninstall(t *testing.T) {
	foreign := "# operator notes\n- id: existing-row\n  config:\n    retries: 3\n"
	for _, phase := range []string{"before patch removal", "after patch removal"} {
		t.Run(phase, func(t *testing.T) {
			cfg := installedDSHTestBinding(t, foreign)
			if _, err := beginDSHTransaction(dshTransactionUninstall, &cfg, nil); err != nil {
				t.Fatal(err)
			}
			if phase == "after patch removal" {
				if _, err := removeDSHPatchBlock(&cfg); err != nil {
					t.Fatal(err)
				}
			}

			if err := recoverDSHTransaction(cfg.RuntimeConfigRoot); err != nil {
				t.Fatal(err)
			}
			if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeDSH); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("integration config remained after uninstall recovery: %v", err)
			}
			if got := readDSHPatchFile(t, cfg.RuntimeMCPConfigPath); got != foreign {
				t.Fatalf("uninstall recovery did not restore the foreign rows exactly: %q", got)
			}
			spec, _, _, err := runtimeMemoryRoutingSpecAt(transcriptcapture.RuntimeDSH, "")
			if err != nil {
				t.Fatal(err)
			}
			// AGENTS.md is shared with the operator, so uninstall removes only
			// the fenced block and keeps the file even when nothing remains.
			routing := readDSHPatchFile(t, spec.path)
			if strings.Contains(routing, dshMemoryRoutingBeginMarker) {
				t.Fatalf("uninstall recovery left the routing block behind: %q", routing)
			}
			if _, err := os.Lstat(dshTransactionPath(cfg.RuntimeConfigRoot)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("uninstall transaction journal remained: %v", err)
			}
		})
	}
}

func TestDSHTransactionRefusesForeignPatchDriftAfterJournaling(t *testing.T) {
	cfg := installedDSHTestBinding(t, "# operator notes\n")
	journal, err := beginDSHTransaction(dshTransactionUninstall, &cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	current := readDSHPatchFile(t, cfg.RuntimeMCPConfigPath)
	if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(current+"- id: appeared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateDSHTransactionProviderBefore(journal); err == nil ||
		!strings.Contains(err.Error(), "changed after transaction journaling") {
		t.Fatalf("provider-before validation = %v", err)
	}
	if err := recoverDSHTransaction(cfg.RuntimeConfigRoot); err == nil ||
		!strings.Contains(err.Error(), "foreign DeepSeek Harness patch rows changed") {
		t.Fatalf("recovery over foreign drift = %v", err)
	}
	if _, err := os.Lstat(dshTransactionPath(cfg.RuntimeConfigRoot)); err != nil {
		t.Fatalf("refused recovery discarded the journal: %v", err)
	}
}

func TestDSHTransactionRefusesASecondConcurrentJournal(t *testing.T) {
	cfg := installedDSHTestBinding(t, "")
	if _, err := beginDSHTransaction(dshTransactionUninstall, &cfg, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := beginDSHTransaction(dshTransactionUninstall, &cfg, nil); err == nil ||
		!strings.Contains(err.Error(), "requires recovery") {
		t.Fatalf("second journal error = %v", err)
	}
}

func TestDSHTransactionJournalRejectsSelectorChangesAndForeignRuntimes(t *testing.T) {
	cfg := installedDSHTestBinding(t, "")
	rebound := cfg
	rebound.RuntimeConfigRoot = cfg.RuntimeConfigRoot + "-other"
	if _, err := beginDSHTransaction(dshTransactionInstall, &cfg, &rebound); err == nil {
		t.Fatal("transaction accepted a config-root change")
	}
	foreign := cfg
	foreign.Runtime = transcriptcapture.RuntimeCopilot
	if err := validateDSHTransactionConfig(foreign); err == nil ||
		!strings.Contains(err.Error(), "non-dsh integration") {
		t.Fatalf("foreign runtime validation = %v", err)
	}
	if _, err := beginDSHTransaction("resync", &cfg, nil); err == nil ||
		!strings.Contains(err.Error(), "unknown DeepSeek Harness transaction operation") {
		t.Fatalf("unknown operation error = %v", err)
	}
}
