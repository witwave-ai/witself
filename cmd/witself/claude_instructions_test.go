package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeMemoryRoutingContractCoversStorageAndRetrieval(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"explicit remember/save/store request",
		"witself.fact.set",
		"same turn",
		"atomic durable assertion",
		"Claude Code auto memory",
		"Split mixed requests",
		"An explicit destination wins",
		"Use both only when explicitly requested",
		"private personal values are sensitive fact values",
		"merely stated without a save request is a review candidate",
		"current-repository and machine-local",
		"never substitute `CLAUDE.md`, project Markdown, a Witself fact, or a transcript",
		"Claim stored only after a confirmed native-memory write",
		"disabled, unavailable, or the write fails",
		"do not change memory settings",
		"witself.fact.get",
		"exact, intentional, authorized lookup",
		"including relevant topic files",
		"unavailable sources or partial coverage",
		"Witself facts are canonical; native memory is advisory",
		"explicitly requests transcript or conversation history",
		"If one source is explicitly named, use only it",
	} {
		if !strings.Contains(claudeMemoryRoutingInstructions, want) {
			t.Errorf("Claude routing contract does not contain %q", want)
		}
	}
	if strings.Contains(claudeMemoryRoutingInstructions, "Codex") {
		t.Fatal("Claude routing contract contains Codex-specific semantics")
	}
}

func TestClaudeMemoryRoutingContractFitsMCPInstructionLimit(t *testing.T) {
	t.Parallel()

	const maxClaudeMCPInstructionBytes = 2 * 1024
	if got := len([]byte(claudeMemoryRoutingInstructions)); got > maxClaudeMCPInstructionBytes {
		t.Fatalf("Claude routing contract is %d bytes, exceeds %d-byte MCP limit", got, maxClaudeMCPInstructionBytes)
	}

	synopsis := claudeMemoryRoutingInstructions
	if len(synopsis) > 512 {
		synopsis = synopsis[:512]
	}
	for _, want := range []string{
		"explicit remember/save/store request",
		"witself.fact.set",
		"atomic durable assertion",
		"Claude Code auto memory",
		"Split mixed requests",
		"explicit destination",
		"explicitly requested",
	} {
		if !strings.Contains(synopsis, want) {
			t.Errorf("first 512 Claude instruction bytes do not contain %q: %q", want, synopsis)
		}
	}
}

func TestClaudeMemoryRoutingPathUsesConfigDirectory(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "claude-home")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	got, err := claudeMemoryRoutingPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(configDir, "rules", claudeMemoryRoutingRuleFile)
	if got != want {
		t.Fatalf("claudeMemoryRoutingPath() = %q, want %q", got, want)
	}
}

func TestClaudeMemoryRoutingPathDefaultsToUserClaudeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "  ")

	got, err := claudeMemoryRoutingPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".claude", "rules", claudeMemoryRoutingRuleFile)
	if got != want {
		t.Fatalf("claudeMemoryRoutingPath() = %q, want %q", got, want)
	}
}

func TestClaudeMemoryRoutingManagedSpecUsesDedicatedRule(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "claude-home")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	spec, err := claudeManagedInstructionsSpec()
	if err != nil {
		t.Fatal(err)
	}
	if spec.path != filepath.Join(configDir, "rules", claudeMemoryRoutingRuleFile) ||
		spec.fileName != claudeMemoryRoutingRuleFile ||
		spec.beginMarker != claudeMemoryRoutingBeginMarker ||
		spec.endMarker != claudeMemoryRoutingEndMarker ||
		!bytes.Equal(spec.block, claudeMemoryRoutingBlock) || !spec.removeEmpty {
		t.Fatalf("unexpected managed instruction spec: %#v", spec)
	}
	if bytes.Count(claudeMemoryRoutingBlock, []byte(claudeMemoryRoutingBeginMarker)) != 1 ||
		bytes.Count(claudeMemoryRoutingBlock, []byte(claudeMemoryRoutingEndMarker)) != 1 ||
		!bytes.Contains(claudeMemoryRoutingBlock, []byte(runtimeNeutralMemoryRoutingInstructions)) {
		t.Fatalf("invalid managed Claude routing block:\n%s", claudeMemoryRoutingBlock)
	}
	if bytes.Contains(claudeMemoryRoutingBlock, []byte("Claude Code")) ||
		bytes.Contains(claudeMemoryRoutingBlock, []byte("Grok")) ||
		bytes.Contains(claudeMemoryRoutingBlock, []byte("witself.fact.")) {
		t.Fatalf("global Claude rule is not safe for compatibility loading:\n%s", claudeMemoryRoutingBlock)
	}
}
