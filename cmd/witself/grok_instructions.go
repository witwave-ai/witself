package main

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	grokMemoryRoutingBeginMarker = "<!-- BEGIN WITSELF MANAGED MEMORY ROUTING -->"
	grokMemoryRoutingEndMarker   = "<!-- END WITSELF MANAGED MEMORY ROUTING -->"

	// grokMemoryRoutingInstructions is the canonical fact-versus-native-memory
	// contract for Grok Build. MCP guidance starts from this dotted-name form;
	// the runtime adapter rewrites tool names for Grok's portable MCP namespace.
	grokMemoryRoutingInstructions = `## Witself facts and Grok memory

On an explicit remember/save/store request, call witself.fact.set in the same turn for one atomic durable assertion. A merely stated fact is only a review candidate, not authority for canonical truth. Mark private personal values sensitive and keep them out of subject metadata. Route narrative requests to Grok native cross-session memory only when it is enabled and available. If native memory is unavailable, say the narrative was not stored; never fall back to a Witself fact or transcript. Never silently duplicate across providers, and do not change Grok memory settings.

- Atomic facts include names or relationships, dates, addresses or locations, URLs, identifiers, stable statuses, and compact durable preferences. Resolve a stable Witself subject for another person, place, or project. Do not also save it in Grok memory unless the user explicitly requests both.
- Narrative context includes reasoning, project history, multi-step incidents, lessons, and material whose meaning depends on a passage. Do not reduce it to a fact merely because the user said "remember." Use only Grok's supported memory facility; do not edit its memory files directly or claim success without confirmation.
- Split a clearly mixed request. Ask before storing when the boundary is genuinely ambiguous. Honor an explicit destination such as Witself, Grok memory, or both.
- Do not enable, disable, or otherwise change Grok memory settings as part of storage or retrieval.
- For an exact fact lookup, resolve the subject and call witself.fact.get. Reveal a sensitive value only for an exact, intentional, authorized lookup.
- For broad recall, call witself.fact.list with sensitive values redacted and consult Grok native memory only when it is enabled and available. If a requested provider cannot be consulted, name it and report partial coverage rather than claiming a complete search.
- Witself transcripts are interaction records, not memories. Search them only when the user explicitly requests transcript or conversation history; never use them as a fallback for disabled or unavailable native memory.
- Honor an explicitly named source. Merge results without silently duplicating them, preserve provenance, present Witself facts as canonical assertions and memories as advisory context, and surface conflicts or uncertainty.`
)

var grokPortableMemoryRoutingInstructions = grokPortableMCPInstructions(
	grokMemoryRoutingInstructions,
	"witself_self_show",
	"witself_message_list",
)

var grokMemoryRoutingBlock = []byte(
	grokMemoryRoutingBeginMarker + "\n" +
		grokPortableMemoryRoutingInstructions + "\n" +
		grokMemoryRoutingEndMarker,
)

func grokPortableMCPInstructions(instructions, selfTool, messageListTool string) string {
	pairs := []string{
		"witself.self.show", selfTool,
		"witself.message.list", messageListTool,
	}
	for _, name := range []string{
		"witself.fact.propose_from_transcript",
		"witself.fact.candidate.get",
		"witself.fact.subject.alias",
		"witself.fact.subject.list",
		"witself.fact.subject.set",
		"witself.fact.upcoming",
		"witself.transcript.list",
		"witself.transcript.get",
		"witself.transcript.tail",
		"witself.fact.propose",
		"witself.fact.confirm",
		"witself.fact.review",
		"witself.fact.reject",
		"witself.message.send",
		"witself.message.read",
		"witself.fact.list",
		"witself.fact.get",
		"witself.fact.set",
	} {
		pairs = append(pairs, name, strings.ReplaceAll(name, ".", "_"))
	}
	return strings.NewReplacer(pairs...).Replace(instructions)
}

func grokAgentsPath() (string, error) {
	root := strings.TrimSpace(os.Getenv("GROK_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".grok")
	}
	return filepath.Join(root, "AGENTS.md"), nil
}

func grokManagedInstructionsSpec() (managedInstructionsSpec, error) {
	path, err := grokAgentsPath()
	if err != nil {
		return managedInstructionsSpec{}, err
	}
	return managedInstructionsSpec{
		path:        path,
		fileName:    "AGENTS.md",
		tempPattern: ".AGENTS.md.witself-*",
		beginMarker: grokMemoryRoutingBeginMarker,
		endMarker:   grokMemoryRoutingEndMarker,
		block:       grokMemoryRoutingBlock,
	}, nil
}
