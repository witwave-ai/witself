package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func dshHookUpgradeConfig(t *testing.T, previous transcriptcapture.Config) transcriptcapture.Config {
	t.Helper()
	desired := previous
	desired.HookMode = transcriptcapture.HookModeUser
	if err := planRuntimeHooksOwned(&desired, &previous); err != nil {
		t.Fatal(err)
	}
	return desired
}

func TestDSHRefusedHookWritePreservesRollbackOwnership(t *testing.T) {
	previous := installedDSHTestBinding(t, "")
	t.Setenv("DSH_HOME", previous.RuntimeConfigRoot)
	desired := dshHookUpgradeConfig(t, previous)
	journal, err := beginDSHTransaction(dshTransactionInstall, &previous, &desired)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := prepareDSHPatchInstallPlan(desired.RuntimeCLICommand, desired, &previous)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installDSHPatchBlockWithPlan(plan); err != nil {
		t.Fatal(err)
	}
	// A foreign directory is a deterministic refused write with no mutation.
	if err := os.Mkdir(desired.HookConfigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	plannedPath := desired.HookConfigPath
	if _, touched, err := installRuntimeHooksOwned(&desired, &previous); err == nil || touched {
		t.Fatalf("directory hook target = touched %t, error %v", touched, err)
	}
	if desired.HookConfigPath != plannedPath {
		t.Error("refused hook write discarded durable rollback ownership")
	}
	if err := restoreRuntimeMCPBinding(transcriptcapture.RuntimeDSH, desired.RuntimeCLICommand, desired.MCPCommand, &previous, &desired); err != nil {
		t.Fatalf("restore phase-one MCP binding after refused hooks: %v", err)
	}
	if err := clearDSHTransaction(previous.RuntimeConfigRoot, journal); err != nil {
		t.Fatal(err)
	}
	if err := validateDSHServeTopology(previous); err != nil {
		t.Fatalf("phase-one MCP serve remains usable: %v", err)
	}
}

func TestDSHHookPlanUsesCanonicalSymlinkHome(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "dsh-link")
	if err := os.Symlink(cfg.RuntimeConfigRoot, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSH_HOME", alias)
	if err := configureDSHBinding(&cfg, cfg.RuntimeCLICommand, cfg.MCPCommand); err != nil {
		t.Fatal(err)
	}
	desired := dshHookUpgradeConfig(t, cfg)
	if _, err := dshManagedPatchBlock(desired); err != nil {
		t.Fatalf("canonical binding must render through a symlinked DSH_HOME: %v", err)
	}
	if _, _, err := installRuntimeHooksOwned(&desired, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := verifyRuntimeHooksOwned(desired); err != nil {
		t.Fatal(err)
	}
}

func TestDSHRefusedFirstHookWriteClearsRollbackJournal(t *testing.T) {
	desired := configuredDSHTestConfig(t)
	desired.HookMode = transcriptcapture.HookModeUser
	if err := planRuntimeHooksOwned(&desired, nil); err != nil {
		t.Fatal(err)
	}
	stubDSHDumpConfig(t, dshComposedFixture(), nil)
	journal, err := beginDSHTransaction(dshTransactionInstall, nil, &desired)
	if err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(desired); err != nil {
		t.Fatal(err)
	}
	if _, err := installDSHPatchBlock(desired); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(desired.HookConfigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, touched, err := installRuntimeHooksOwned(&desired, nil); err == nil || touched {
		t.Fatalf("directory hook target = touched %t, error %v", touched, err)
	}
	if err := restoreRuntimeMCPBinding(transcriptcapture.RuntimeDSH, desired.RuntimeCLICommand, desired.MCPCommand, nil, &desired); err != nil {
		t.Fatalf("remove first-install MCP binding after refused hooks: %v", err)
	}
	if err := transcriptcapture.RemoveConfig(transcriptcapture.RuntimeDSH); err != nil {
		t.Fatal(err)
	}
	if err := clearDSHTransaction(desired.RuntimeConfigRoot, journal); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(desired.HookConfigPath); err != nil || !info.IsDir() {
		t.Fatalf("rollback must preserve the foreign hook target: %v", err)
	}
}

func TestDSHHookRecoveryUsesJournaledHome(t *testing.T) {
	previous := installedDSHTestBinding(t, "")
	t.Setenv("DSH_HOME", previous.RuntimeConfigRoot)
	desired := dshHookUpgradeConfig(t, previous)
	if _, err := beginDSHTransaction(dshTransactionInstall, &previous, &desired); err != nil {
		t.Fatal(err)
	}
	ambientRoot := t.TempDir()
	t.Setenv("DSH_HOME", ambientRoot)
	root, err := dshOperationLockRoot()
	if err != nil || root != previous.RuntimeConfigRoot {
		t.Fatalf("recovery lock must retain the installed root: %v", err)
	}
	recoveryErr := recoverDSHTransaction(root)
	if _, err := os.Lstat(filepath.Join(ambientRoot, dshHookConfigFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("recovery wrote orphan hooks in the ambient home: %v", err)
	}
	if recoveryErr == nil || !strings.Contains(recoveryErr.Error(), "DSH_HOME changed") {
		t.Fatalf("recovery must retain the existing selector guard: %v", recoveryErr)
	}
	if err := verifyRuntimeHooksOwned(desired); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSH_HOME", previous.RuntimeConfigRoot)
	stubDSHDumpConfig(t, dshComposedFixture()+"  - id: "+dshHooksPatchRowID+"\n    name: '"+dshHooksBridgePluginName+"'\n", nil)
	if err := recoverDSHTransaction(root); err != nil {
		t.Fatalf("recovery must finish when the installed selector is restored: %v", err)
	}
}

func TestDSHPhaseOneVerificationRequestsHookUpgrade(t *testing.T) {
	cfg := installedDSHTestBinding(t, "")
	status := verifyInstalledIntegration(
		transcriptcapture.RuntimeDSH,
		integrationPlatformStatus{SupportedOnPlatform: true, SupportLevel: integrationPlatformNative},
		cfg,
	)
	if status.State != integrationVerificationIncomplete || !strings.Contains(status.Message, "reinstall") {
		t.Fatalf("phase-one binding must advertise missing capture: state=%s message=%q", status.State, status.Message)
	}
	// Verification's upgrade nudge must not make the existing MCP mount fail.
	if err := validateDSHServeTopology(cfg); err != nil {
		t.Fatalf("phase-one MCP serve must remain usable: %v", err)
	}
}
