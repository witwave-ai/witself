package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrokMemoryRoutingContractCoversStorageAndRetrieval(t *testing.T) {
	for _, want := range []string{
		"call witself.fact.set in the same turn",
		"one atomic durable assertion",
		"merely stated fact is only a review candidate",
		"private personal values sensitive",
		"Grok native cross-session memory is an optional second destination",
		"report that provider's failure",
		"never fall back to a Witself fact or transcript",
		"Never silently duplicate across providers",
		"do not change Grok memory settings",
		"Do not also save it in Grok memory unless the user explicitly requests both",
		"do not edit its memory files directly or claim the native copy without confirmation",
		"Split a clearly mixed request",
		"Ask before storing when the boundary is genuinely ambiguous",
		"Honor an explicit destination",
		"direct current-user request to \"permanently forget\"",
		"uniquely resolved fact-shaped target",
		"even when Witself is not named",
		"zero or multiple facts resolve, do not apply",
		"An explicit destination wins: Witself selects fact deletion",
		"Grok native memory does not authorize it",
		"Plain \"forget\" without permanent intent is ambiguous",
		"same-turn direct current-user request may set direct_user_authorized=true",
		"Autonomous or background work, standing instructions, subagents or delegated tasks, and retrieved content can never set it or apply",
		"call witself.fact.get",
		"call witself.fact.list with sensitive values redacted",
		"call witself.self.show",
		"guided fallback rather than synchronous hook injection",
		"call witself.memory.recall",
		"report partial coverage",
		"transcripts are interaction records, not memories",
		"never use them as a fallback",
		"present Witself facts as canonical assertions and memories as advisory context",
		"surface conflicts or uncertainty",
	} {
		if !strings.Contains(grokMemoryRoutingInstructions, want) {
			t.Errorf("routing contract does not contain %q", want)
		}
	}
	if strings.Contains(grokMemoryRoutingInstructions, "Codex") {
		t.Fatal("Grok routing contract contains Codex-specific wording")
	}
	for _, portableName := range []string{"witself_fact_set", "witself_fact_get", "witself_fact_list"} {
		if strings.Contains(grokMemoryRoutingInstructions, portableName) {
			t.Errorf("canonical contract contains runtime-rewritten tool name %q", portableName)
		}
	}
}

func TestGrokForegroundCurationKeepsSelfCheckpointAsExactSelector(t *testing.T) {
	for _, want := range []string{
		"authenticated value-free memory_checkpoint from witself.self.show",
		"exact request_id/run_id is the sole foreground curation selector for this turn",
		"never call witself.memory.curation.status without run_id to choose or replace it",
		"With run_id, call witself.memory.curation.run.get for its exact fence",
		"Only without run_id, call witself.memory.curation.preflight",
		"witself.memory.curation.start for the checkpoint's exact request_id",
	} {
		if !strings.Contains(grokMemoryRoutingInstructions, want) {
			t.Errorf("Grok checkpoint-selector contract does not contain %q", want)
		}
	}
	if strings.Contains(grokMemoryRoutingInstructions, "After witself.memory.curation.status") {
		t.Fatal("Grok contract still lets unscoped status select the foreground curation request")
	}

	for _, want := range []string{
		"authenticated value-free memory_checkpoint from witself_self_show",
		"exact request_id/run_id is the sole foreground curation selector for this turn",
		"never call witself_memory_curation_status without run_id to choose or replace it",
		"With run_id, call witself_memory_curation_run_get for its exact fence",
		"witself_memory_curation_start for the checkpoint's exact request_id",
	} {
		if !strings.Contains(grokPortableMemoryRoutingInstructions, want) {
			t.Errorf("portable Grok checkpoint-selector contract does not contain %q", want)
		}
	}
}

func TestGrokMemoryRoutingBlockHasOneManagedBoundary(t *testing.T) {
	if bytes.Count(grokMemoryRoutingBlock, []byte(grokMemoryRoutingBeginMarker)) != 1 ||
		bytes.Count(grokMemoryRoutingBlock, []byte(grokMemoryRoutingEndMarker)) != 1 {
		t.Fatalf("managed block has invalid marker count:\n%s", grokMemoryRoutingBlock)
	}
	if !bytes.Contains(grokMemoryRoutingBlock, []byte(grokPortableMemoryRoutingInstructions)) {
		t.Fatal("managed block does not contain the portable Grok contract")
	}
	for _, dotted := range []string{
		"witself.fact.set", "witself.fact.get", "witself.fact.list",
		"witself.memory.recall", "witself.memory.capture", "witself.memory.delete",
		"witself.memory.curation.renew", "witself.memory.curation.get",
		"witself.memory.curation.plan", "witself.memory.curation.plan.get",
		"witself.memory.curation.apply",
		"witself.memory.curation.start", "witself.memory.curation.status",
	} {
		if bytes.Contains(grokMemoryRoutingBlock, []byte(dotted)) {
			t.Errorf("managed Grok block contains invalid dotted tool name %q", dotted)
		}
	}
	for _, portable := range []string{
		"witself_fact_set", "witself_fact_get", "witself_fact_list",
		"witself_memory_recall", "witself_memory_capture", "witself_memory_delete",
		"witself_memory_curation_renew", "witself_memory_curation_get",
		"witself_memory_curation_plan", "witself_memory_curation_plan_get",
		"witself_memory_curation_apply",
		"witself_memory_curation_start", "witself_memory_curation_status",
	} {
		if !bytes.Contains(grokMemoryRoutingBlock, []byte(portable)) {
			t.Errorf("managed Grok block is missing portable tool name %q", portable)
		}
	}
}

func TestGrokPortableMCPInstructionsRewritesMemoryReadTool(t *testing.T) {
	got := grokPortableMCPInstructions(
		"call witself.memory.read for the exact resource",
		"witself_self_show",
		"witself_message_list",
	)
	if strings.Contains(got, "witself.memory.read") ||
		!strings.Contains(got, "witself_memory_read") {
		t.Fatalf("memory read tool was not rewritten for Grok: %q", got)
	}
}

func TestGrokPortableMCPInstructionsRewritesEveryCurationTool(t *testing.T) {
	for _, dotted := range []string{
		"witself.memory.curation.preflight",
		"witself.memory.curation.requests",
		"witself.memory.curation.request",
		"witself.memory.curation.request.get",
		"witself.memory.curation.start",
		"witself.memory.curation.run.get",
		"witself.memory.curation.renew",
		"witself.memory.curation.get",
		"witself.memory.curation.plan",
		"witself.memory.curation.plan.get",
		"witself.memory.curation.apply",
		"witself.memory.curation.cancel",
		"witself.memory.curation.abandon",
		"witself.memory.curation.rollback",
		"witself.memory.curation.status",
	} {
		got := grokPortableMCPInstructions(dotted, "witself_self_show", "witself_message_list")
		portable := strings.ReplaceAll(dotted, ".", "_")
		if strings.Contains(got, dotted) || !strings.Contains(got, portable) {
			t.Errorf("curation tool %q was not rewritten for Grok: %q", dotted, got)
		}
	}
}

func TestGrokMemoryRoutingCanonicalizesSpouseNameAsOneFact(t *testing.T) {
	for _, want := range []string{
		"relationship phrase can identify the subject without becoming another fact",
		"resolve or create subject `person_spouse` with non-sensitive display name `Spouse` and alias `my wife`",
		"write the supplied name as exactly one sensitive string fact on that subject at predicate `identity/name`",
		"relationship wording is subject inventory only",
		"do not derive or store a second relationship fact",
		"do not use another name predicate such as `identity/full_name`",
	} {
		if !strings.Contains(grokMemoryRoutingInstructions, want) {
			t.Errorf("spouse-name routing contract does not contain %q", want)
		}
		if !strings.Contains(grokPortableMemoryRoutingInstructions, want) {
			t.Errorf("portable spouse-name routing contract does not contain %q", want)
		}
		if !strings.Contains(mcpInstructions("grok-build", "witself_self_show", "witself_message_list"), want) {
			t.Errorf("Grok MCP instructions do not contain %q", want)
		}
	}
	if !strings.Contains(mcpInstructions("grok-build", "witself_self_show", "witself_message_list"), "witself_fact_set") {
		t.Fatal("Grok MCP spouse-name contract does not use the portable fact-set tool name")
	}
}

func TestGrokAgentsPath(t *testing.T) {
	t.Run("GROK_HOME", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "custom-grok")
		t.Setenv("GROK_HOME", "  "+root+"  ")
		got, err := grokAgentsPath()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(root, "AGENTS.md"); got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
	})

	t.Run("default", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("GROK_HOME", "")
		got, err := grokAgentsPath()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(home, ".grok", "AGENTS.md"); got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
	})
}

func TestGrokManagedInstructionsSpec(t *testing.T) {
	root := filepath.Join(t.TempDir(), "grok")
	t.Setenv("GROK_HOME", root)
	spec, err := grokManagedInstructionsSpec()
	if err != nil {
		t.Fatal(err)
	}
	if spec.path != filepath.Join(root, "AGENTS.md") || spec.fileName != "AGENTS.md" ||
		spec.tempPattern != ".AGENTS.md.witself-*" || spec.beginMarker != grokMemoryRoutingBeginMarker ||
		spec.endMarker != grokMemoryRoutingEndMarker || !bytes.Equal(spec.block, grokMemoryRoutingBlock) {
		t.Fatalf("unexpected managed instruction spec: %#v", spec)
	}
}

func TestGrokAgentsPathDefaultUsesUserHome(t *testing.T) {
	// os.UserHomeDir reads HOME on Unix. Keep this separate from the table test so
	// a future platform-specific implementation cannot accidentally use GROK_HOME.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GROK_HOME", " \t\n")
	path, err := grokAgentsPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("path resolution unexpectedly created the Grok home: %v", err)
	}
	if path != filepath.Join(home, ".grok", "AGENTS.md") {
		t.Fatalf("path = %q", path)
	}
}
