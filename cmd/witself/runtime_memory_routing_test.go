package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestRuntimeMemoryRoutingSpecsSelectProviderFiles(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex")
	claudeHome := filepath.Join(home, "claude")
	grokHome := filepath.Join(home, "grok")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("GROK_HOME", grokHome)

	for _, tc := range []struct {
		runtimeName string
		displayName string
		path        string
		managed     bool
	}{
		{transcriptcapture.RuntimeCodex, "Codex", filepath.Join(codexHome, "AGENTS.md"), true},
		{transcriptcapture.RuntimeClaudeCode, "Claude Code", filepath.Join(claudeHome, "rules", claudeMemoryRoutingRuleFile), true},
		{transcriptcapture.RuntimeGrokBuild, "Grok Build", filepath.Join(grokHome, "AGENTS.md"), true},
		{transcriptcapture.RuntimeCursor, "", "", false},
	} {
		t.Run(tc.runtimeName, func(t *testing.T) {
			spec, displayName, managed, err := runtimeMemoryRoutingSpec(tc.runtimeName)
			if err != nil {
				t.Fatal(err)
			}
			if displayName != tc.displayName || managed != tc.managed || spec.path != tc.path {
				t.Fatalf("spec=%#v display=%q managed=%t", spec, displayName, managed)
			}
		})
	}
	if _, _, _, err := runtimeMemoryRoutingSpec("unknown-runtime"); err == nil {
		t.Fatal("unsupported runtime did not fail")
	}
}

func TestRuntimeMemoryRoutingLifecycleUsesProviderContract(t *testing.T) {
	for _, tc := range []struct {
		name         string
		runtimeName  string
		envName      string
		path         func(string) string
		instructions string
		original     []byte
		removed      bool
	}{
		{
			name: "codex", runtimeName: transcriptcapture.RuntimeCodex, envName: "CODEX_HOME",
			path:         func(root string) string { return filepath.Join(root, "AGENTS.md") },
			instructions: codexMemoryRoutingInstructions, original: []byte("# Existing Codex rules\n"),
		},
		{
			name: "claude", runtimeName: transcriptcapture.RuntimeClaudeCode, envName: "CLAUDE_CONFIG_DIR",
			path:         func(root string) string { return filepath.Join(root, "rules", claudeMemoryRoutingRuleFile) },
			instructions: runtimeNeutralMemoryRoutingInstructions, removed: true,
		},
		{
			name: "grok", runtimeName: transcriptcapture.RuntimeGrokBuild, envName: "GROK_HOME",
			path:         func(root string) string { return filepath.Join(root, "AGENTS.md") },
			instructions: grokPortableMemoryRoutingInstructions, original: []byte("# Existing Grok rules\n"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), tc.name)
			t.Setenv(tc.envName, root)
			path := tc.path(root)
			if len(tc.original) > 0 {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, tc.original, 0o640); err != nil {
					t.Fatal(err)
				}
			}

			installed, err := installRuntimeMemoryRoutingInstructions(tc.runtimeName)
			if err != nil {
				t.Fatal(err)
			}
			if !installed.managed || installed.path != path {
				t.Fatalf("installed state = %#v", installed)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), tc.instructions) ||
				(len(tc.original) > 0 && !bytes.HasSuffix(raw, tc.original)) {
				t.Fatalf("installed routing = %q", raw)
			}
			if _, err := installRuntimeMemoryRoutingInstructions(tc.runtimeName); err != nil {
				t.Fatal(err)
			}
			reinstalled, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(reinstalled, raw) {
				t.Fatal("idempotent runtime install changed the file")
			}

			removedState, err := removeRuntimeMemoryRoutingInstructions(tc.runtimeName)
			if err != nil {
				t.Fatal(err)
			}
			if !removedState.managed {
				t.Fatal("runtime removal was not marked managed")
			}
			if tc.removed {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("dedicated rule still exists: %v", err)
				}
				return
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.original) {
				t.Fatalf("removed routing = %q, want %q", got, tc.original)
			}
		})
	}
}

func TestCodexRuntimeRoutingRemovalIgnoresLaterOverride(t *testing.T) {
	root := filepath.Join(t.TempDir(), "codex")
	t.Setenv("CODEX_HOME", root)
	path := filepath.Join(root, "AGENTS.md")
	original := []byte("# Existing Codex rules\n")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeCodex); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.override.md"), []byte("# New override\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := removeRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeCodex); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("removed routing = %q, want %q", got, original)
	}
}
