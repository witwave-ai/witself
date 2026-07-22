package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopilotManagedInstructionsSpecUsesExactOwnedGlobalRule(t *testing.T) {
	root := filepath.Join(t.TempDir(), "copilot-home")
	t.Setenv("COPILOT_HOME", root)

	spec, err := copilotManagedInstructionsSpec()
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := cleanCopilotAbsolutePath("test COPILOT_HOME", root)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(canonicalRoot, "instructions", copilotMemoryRoutingRuleFile)
	if spec.path != wantPath || !spec.exclusive || !spec.removeEmpty {
		t.Fatalf("spec path=%q exclusive=%t removeEmpty=%t, want exact-owned rule at %s", spec.path, spec.exclusive, spec.removeEmpty, wantPath)
	}
	if !bytes.HasPrefix(spec.block, []byte("---\n# BEGIN WITSELF MANAGED MEMORY ROUTING\napplyTo: \"**\"\n---\n")) {
		t.Fatalf("Copilot rule has invalid frontmatter prefix: %q", spec.block[:min(len(spec.block), 128)])
	}
	for _, contract := range []string{
		copilotMemoryRoutingInstructions,
		foregroundMessagingRoutingInstructions,
		avatarRoutingInstructions,
		secretRoutingInstructions,
	} {
		if !bytes.Contains(spec.block, []byte(contract)) {
			t.Fatalf("Copilot rule omitted contract beginning %q", contract[:min(len(contract), 48)])
		}
	}
}

func TestCopilotManagedInstructionsLifecycleIsExclusive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "copilot-home")
	t.Setenv("COPILOT_HOME", root)
	spec, err := copilotManagedInstructionsSpec()
	if err != nil {
		t.Fatal(err)
	}

	installed, err := installManagedInstructions(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !installed.mutated {
		t.Fatal("first install did not report a mutation")
	}
	raw, err := os.ReadFile(spec.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, append(bytes.Clone(copilotMemoryRoutingBlock), '\n')) {
		t.Fatalf("installed Copilot rule differs from exact managed block")
	}
	if second, err := installManagedInstructions(spec); err != nil {
		t.Fatal(err)
	} else if second.mutated {
		t.Fatal("idempotent install reported a mutation")
	}

	removed, err := removeManagedInstructions(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !removed.mutated {
		t.Fatal("removal did not report a mutation")
	}
	if _, err := os.Stat(spec.path); !os.IsNotExist(err) {
		t.Fatalf("exact-owned Copilot rule still exists: %v", err)
	}
}

func TestCopilotManagedInstructionsRefuseForeignDedicatedFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "copilot-home")
	t.Setenv("COPILOT_HOME", root)
	spec, err := copilotManagedInstructionsSpec()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(spec.path), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("---\napplyTo: \"**\"\n---\n# User-owned instructions\n")
	if err := os.WriteFile(spec.path, foreign, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := installManagedInstructions(spec); err == nil || !strings.Contains(err.Error(), "not managed by Witself") {
		t.Fatalf("foreign exact-owned file error = %v", err)
	}
	raw, err := os.ReadFile(spec.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, foreign) {
		t.Fatal("foreign Copilot instructions were modified")
	}
}

func TestCurrentCopilotConfigRootHonorsExactHome(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real", "copilot")
	if err := os.MkdirAll(filepath.Dir(realRoot), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "linked")
	if err := os.Symlink(filepath.Join(base, "real"), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPILOT_HOME", filepath.Join(link, "copilot"))
	root, err := currentCopilotConfigRoot()
	if err != nil {
		t.Fatal(err)
	}
	canonicalRealRoot, err := cleanCopilotAbsolutePath("test root", realRoot)
	if err != nil {
		t.Fatal(err)
	}
	if root != canonicalRealRoot {
		t.Fatalf("root = %q, want canonical %q", root, canonicalRealRoot)
	}

	t.Setenv("COPILOT_HOME", " "+realRoot)
	if _, err := currentCopilotConfigRoot(); err == nil || !strings.Contains(err.Error(), "surrounding whitespace") {
		t.Fatalf("whitespace COPILOT_HOME error = %v", err)
	}
}
