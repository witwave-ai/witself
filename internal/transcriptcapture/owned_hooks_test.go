package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnedUserHooksRejectDifferentIdentityHomeExecutableAndMixedMarkers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	opts, err := DefaultUserHooksOptions(
		RuntimeClaudeCode, ModeRaw, "/usr/local/bin/witself",
		"default", "default", "scott", "home", filepath.Join(home, ".witself"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstallOwnedHooks(opts, nil); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*UserHooksOptions){
		"identity":   func(value *UserHooksOptions) { value.Agent = "other-agent" },
		"home":       func(value *UserHooksOptions) { value.WitselfHome = filepath.Join(home, "other-home") },
		"executable": func(value *UserHooksOptions) { value.Executable = "/opt/other/witself" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			different := opts
			mutate(&different)
			if _, err := RemoveOwnedHooks(different); err == nil ||
				!strings.Contains(err.Error(), "differs from the persisted") {
				t.Fatalf("remove error = %v", err)
			}
			got, err := os.ReadFile(opts.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) {
				t.Fatal("differently bound removal changed the owned hook document")
			}
		})
	}

	var root map[string]any
	if err := json.Unmarshal(original, &root); err != nil {
		t.Fatal(err)
	}
	hooks := root["hooks"].(map[string]any)
	groups := hooks["Stop"].([]any)
	groups = append(groups, map[string]any{
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": "'/opt/foreign/witself' transcript hook --runtime claude-code --account 'default' --realm 'default' --agent 'foreign'",
			"timeout": 10,
		}},
	})
	hooks["Stop"] = groups
	mixed, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mixed = append(mixed, '\n')
	if err := os.WriteFile(opts.ConfigPath, mixed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveOwnedHooks(opts); err == nil || !strings.Contains(err.Error(), "marker-shaped") {
		t.Fatalf("mixed marker removal error = %v", err)
	}
	got, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, mixed) {
		t.Fatal("mixed marker hook document changed")
	}
}

func TestOwnedGrokHooksRequireDurableOrExactLegacyBinding(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	if _, err := InstallHooksWithWitselfHome(
		RuntimeGrokBuild, ModeRaw, "/usr/local/bin/witself",
		"default", "default", "scott", "home", filepath.Join(home, ".witself"),
	); err != nil {
		t.Fatal(err)
	}
	opts, err := DefaultUserHooksOptions(
		RuntimeGrokBuild, ModeRaw, "/usr/local/bin/witself",
		"default", "default", "scott", "home", filepath.Join(home, ".witself"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstallOwnedHooks(opts, nil); err == nil ||
		!strings.Contains(err.Error(), "without a durable Witself ownership record") {
		t.Fatalf("unowned Grok adoption error = %v", err)
	}
	if mutation, err := InstallOwnedHooks(opts, &opts); err != nil {
		t.Fatal(err)
	} else if mutation.Touched {
		t.Fatal("exact legacy Grok adoption rewrote an already exact document")
	}
	if err := VerifyOwnedHooks(opts); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedUserHookCASRejectsEditImmediatelyBeforeRemoval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	opts, err := DefaultUserHooksOptions(
		RuntimeClaudeCode, ModeRaw, "/usr/local/bin/witself",
		"default", "default", "scott", "home", filepath.Join(home, ".witself"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstallOwnedHooks(opts, nil); err != nil {
		t.Fatal(err)
	}
	later := []byte(`{"foreign":"concurrent"}` + "\n")
	ownedHookBeforeMutationForTest = func(path string) {
		ownedHookBeforeMutationForTest = nil
		if err := os.WriteFile(path, later, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { ownedHookBeforeMutationForTest = nil })
	if _, err := RemoveOwnedHooks(opts); err == nil || !strings.Contains(err.Error(), "changed concurrently") {
		t.Fatalf("remove CAS error = %v", err)
	}
	got, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, later) {
		t.Fatalf("concurrent hook edit changed: got %q want %q", got, later)
	}
}

func TestOwnedManagedHooksPinDirectoryRunnerAndDigests(t *testing.T) {
	opts := managedHooksTestOptions(t, RuntimeCodex, ModeRaw)
	enterpriseDir := filepath.Join(t.TempDir(), "enterprise-hooks")
	initial := "[features]\nhooks = true\n\n[hooks]\nmanaged_dir = " + tomlTestQuote(enterpriseDir) + "\n"
	if err := os.MkdirAll(filepath.Dir(opts.CodexRequirementsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opts.CodexRequirementsPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	ownership, touched, err := InstallManagedHooksOwned(opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !touched || ownership.ManagedDir != enterpriseDir ||
		!strings.HasPrefix(filepath.Base(ownership.RunnerPath), managedOwnedRunnerPrefix) ||
		len(ownership.RunnerDigest) != 64 || len(ownership.PolicyDigest) != 64 {
		t.Fatalf("ownership = %#v touched=%t", ownership, touched)
	}
	if err := VerifyManagedHooksOwned(RuntimeCodex, ownership); err != nil {
		t.Fatal(err)
	}

	foreignDir := filepath.Join(t.TempDir(), "foreign-hooks")
	raw, err := os.ReadFile(ownership.PolicyPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(enterpriseDir), []byte(foreignDir), 1)
	if err := os.WriteFile(ownership.PolicyPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	foreignRunner := filepath.Join(foreignDir, managedRunnerName)
	if err := os.MkdirAll(foreignDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreignBytes := []byte("#!/bin/sh\nexit 42\n")
	if err := os.WriteFile(foreignRunner, foreignBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	if touched, err := RemoveManagedHooksOwned(RuntimeCodex, ownership); err != nil || !touched {
		t.Fatalf("remove touched=%t error=%v", touched, err)
	}
	got, err := os.ReadFile(foreignRunner)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, foreignBytes) {
		t.Fatal("changed managed_dir caused removal of the foreign runner")
	}
	if _, err := os.Stat(ownership.RunnerPath); !os.IsNotExist(err) {
		t.Fatalf("pinned owned runner remains: %v", err)
	}
}

func TestOwnedManagedHooksRefuseForeignRunnerAndDropIn(t *testing.T) {
	t.Run("runner", func(t *testing.T) {
		opts := managedHooksTestOptions(t, RuntimeCodex, ModeRaw)
		runnerRaw := managedRunnerBytes(opts.Executable)
		runnerPath := ownedManagedRunnerPath(opts, opts.CodexManagedDir, runnerRaw)
		if err := os.MkdirAll(opts.CodexManagedDir, 0o755); err != nil {
			t.Fatal(err)
		}
		foreign := []byte("#!/bin/sh\nexit 9\n")
		if err := os.WriteFile(runnerPath, foreign, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, _, err := InstallManagedHooksOwned(opts, nil); err == nil ||
			!strings.Contains(err.Error(), "without a durable ownership record") {
			t.Fatalf("foreign runner error = %v", err)
		}
		got, err := os.ReadFile(runnerPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, foreign) {
			t.Fatal("foreign runner changed")
		}
	})
	t.Run("drop-in", func(t *testing.T) {
		opts := managedHooksTestOptions(t, RuntimeClaudeCode, ModeRaw)
		if err := os.MkdirAll(filepath.Dir(opts.ClaudeSettingsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		foreign := []byte(`{"hooks":{},"foreign":true}` + "\n")
		if err := os.WriteFile(opts.ClaudeSettingsPath, foreign, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := InstallManagedHooksOwned(opts, nil); err == nil ||
			!strings.Contains(err.Error(), "without a durable ownership record") {
			t.Fatalf("foreign drop-in error = %v", err)
		}
		got, err := os.ReadFile(opts.ClaudeSettingsPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, foreign) {
			t.Fatal("foreign managed drop-in changed")
		}
	})
}

func TestOwnedManagedHookCASRejectsEditImmediatelyBeforeRemoval(t *testing.T) {
	opts := managedHooksTestOptions(t, RuntimeClaudeCode, ModeRaw)
	ownership, _, err := InstallManagedHooksOwned(opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	later := []byte(`{"foreign":"concurrent"}` + "\n")
	ownedManagedHookBeforeMutationForTest = func(path string) {
		if path != ownership.PolicyPath {
			return
		}
		ownedManagedHookBeforeMutationForTest = nil
		if err := os.WriteFile(path, later, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { ownedManagedHookBeforeMutationForTest = nil })
	if _, err := RemoveManagedHooksOwned(RuntimeClaudeCode, ownership); err == nil ||
		!strings.Contains(err.Error(), "changed concurrently") {
		t.Fatalf("managed remove CAS error = %v", err)
	}
	got, err := os.ReadFile(ownership.PolicyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, later) {
		t.Fatalf("concurrent managed policy changed: got %q want %q", got, later)
	}
	if _, err := os.Stat(ownership.RunnerPath); err != nil {
		t.Fatalf("owned runner was removed after policy CAS failure: %v", err)
	}
}

func TestOwnedManagedHookInstallCASRejectsPolicyAndRunnerEditsBeforeCommit(t *testing.T) {
	t.Run("policy", func(t *testing.T) {
		opts := managedHooksTestOptions(t, RuntimeClaudeCode, ModeRaw)
		runnerPath := ownedManagedRunnerPath(opts, opts.ClaudeManagedDir, managedRunnerBytes(opts.Executable))
		later := []byte(`{"foreign":"concurrent-policy"}` + "\n")
		ownedManagedHookBeforeMutationForTest = func(path string) {
			if path != opts.ClaudeSettingsPath {
				return
			}
			ownedManagedHookBeforeMutationForTest = nil
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, later, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { ownedManagedHookBeforeMutationForTest = nil })
		if _, touched, err := InstallManagedHooksOwned(opts, nil); err == nil ||
			!strings.Contains(err.Error(), "changed concurrently") || touched {
			t.Fatalf("install touched=%t error=%v", touched, err)
		}
		got, err := os.ReadFile(opts.ClaudeSettingsPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, later) {
			t.Fatalf("concurrent managed policy changed: got %q want %q", got, later)
		}
		if _, err := os.Stat(runnerPath); !os.IsNotExist(err) {
			t.Fatalf("staged managed runner remains after clean rollback: %v", err)
		}
	})

	t.Run("runner", func(t *testing.T) {
		opts := managedHooksTestOptions(t, RuntimeClaudeCode, ModeRaw)
		runnerPath := ownedManagedRunnerPath(opts, opts.ClaudeManagedDir, managedRunnerBytes(opts.Executable))
		later := []byte("#!/bin/sh\nexit 73\n")
		ownedManagedHookBeforeMutationForTest = func(path string) {
			if path != opts.ClaudeSettingsPath {
				return
			}
			ownedManagedHookBeforeMutationForTest = nil
			if err := os.WriteFile(runnerPath, later, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { ownedManagedHookBeforeMutationForTest = nil })
		ownership, touched, err := InstallManagedHooksOwned(opts, nil)
		if err == nil || !strings.Contains(err.Error(), "runner changed before policy commit") || !touched {
			t.Fatalf("install ownership=%#v touched=%t error=%v", ownership, touched, err)
		}
		if ownership.RunnerPath != runnerPath {
			t.Fatalf("reported runner path = %q, want %q", ownership.RunnerPath, runnerPath)
		}
		got, readErr := os.ReadFile(runnerPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(got, later) {
			t.Fatalf("concurrent managed runner changed: got %q want %q", got, later)
		}
		if _, statErr := os.Stat(opts.ClaudeSettingsPath); !os.IsNotExist(statErr) {
			t.Fatalf("managed policy committed after runner drift: %v", statErr)
		}
	})
}

func TestOwnedManagedHooksRefuseOversizePolicyAndRunnerSnapshots(t *testing.T) {
	t.Run("policy", func(t *testing.T) {
		opts := managedHooksTestOptions(t, RuntimeCodex, ModeRaw)
		if err := os.MkdirAll(filepath.Dir(opts.CodexRequirementsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(opts.CodexRequirementsPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(managedHookFileReadLimit + 1); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := PlanManagedHooksOwned(opts, nil); err == nil ||
			!strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversize policy error = %v", err)
		}
	})

	t.Run("runner", func(t *testing.T) {
		opts := managedHooksTestOptions(t, RuntimeClaudeCode, ModeRaw)
		ownership, _, err := InstallManagedHooksOwned(opts, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(ownership.RunnerPath, managedHookFileReadLimit+1); err != nil {
			t.Fatal(err)
		}
		if err := VerifyManagedHooksOwned(RuntimeClaudeCode, ownership); err == nil ||
			!strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversize runner verify error = %v", err)
		}
		policyBefore, err := os.ReadFile(ownership.PolicyPath)
		if err != nil {
			t.Fatal(err)
		}
		if touched, err := RemoveManagedHooksOwned(RuntimeClaudeCode, ownership); err == nil ||
			!strings.Contains(err.Error(), "exceeds") || touched {
			t.Fatalf("oversize runner removal touched=%t error=%v", touched, err)
		}
		policyAfter, err := os.ReadFile(ownership.PolicyPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(policyAfter, policyBefore) {
			t.Fatal("oversize runner refusal changed managed policy")
		}
	})
}
