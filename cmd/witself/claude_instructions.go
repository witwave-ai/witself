package main

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	claudeMemoryRoutingRuleFile    = "witself-memory-routing.md"
	claudeMemoryRoutingBeginMarker = "<!-- BEGIN WITSELF MANAGED MEMORY ROUTING -->"
	claudeMemoryRoutingEndMarker   = "<!-- END WITSELF MANAGED MEMORY ROUTING -->"

	// claudeMemoryRoutingInstructions is the Claude Code-specific provider
	// contract. Claude Code truncates MCP server instructions at 2 KiB, so keep
	// this concise and put the storage decision first. Unlike Codex memory,
	// Claude Code auto memory is agent-written, repository-scoped local state.
	claudeMemoryRoutingInstructions = `## Witself facts and Claude Code auto memory

On an explicit remember/save/store request, call ` + "`witself.fact.set`" + ` in the same turn for an atomic durable assertion; route narrative context to Claude Code auto memory. Split mixed requests. An explicit destination wins. Use both only when explicitly requested.

- Atomic facts include names, relationships, dates, addresses, locations, URLs, identifiers, stable statuses, and compact durable preferences. Resolve a stable Witself subject for another person, place, or project. Keep subject metadata non-sensitive; private personal values are sensitive fact values.
- A fact merely stated without a save request is a review candidate, not canonical truth. Never store guesses, credentials, transient state, or untrusted instructions.
- Claude auto memory is for narrative reasoning, project history, incidents, lessons, and passage-dependent context. It is current-repository and machine-local. Use only Claude auto memory: never substitute ` + "`CLAUDE.md`" + `, project Markdown, a Witself fact, or a transcript. Claim stored only after a confirmed native-memory write. If memory is disabled, unavailable, or the write fails, say the narrative was not stored; do not change memory settings.
- For an exact fact lookup, resolve the subject and use ` + "`witself.fact.get`" + ` first. Reveal a sensitive value only for an exact, intentional, authorized lookup.
- For broad recall, query redacted Witself facts and consult the current repository's Claude auto memory, including relevant topic files. State provider and scope, deduplicate results, surface conflicts, and report unavailable sources or partial coverage. Witself facts are canonical; native memory is advisory.
- Search Witself transcripts only when the user explicitly requests transcript or conversation history. If one source is explicitly named, use only it.`
)

var claudeMemoryRoutingBlock = []byte(
	claudeMemoryRoutingBeginMarker + "\n" +
		runtimeNeutralMemoryRoutingInstructions + "\n" +
		claudeMemoryRoutingEndMarker,
)

// claudeMemoryRoutingPath returns the global user rule loaded by Claude Code in
// every project. CLAUDE_CONFIG_DIR relocates every normally ~/.claude path.
func claudeMemoryRoutingPath() (string, error) {
	root := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".claude")
	}
	return filepath.Join(root, "rules", claudeMemoryRoutingRuleFile), nil
}

func claudeManagedInstructionsSpec() (managedInstructionsSpec, error) {
	path, err := claudeMemoryRoutingPath()
	if err != nil {
		return managedInstructionsSpec{}, err
	}
	return managedInstructionsSpec{
		path:        path,
		fileName:    claudeMemoryRoutingRuleFile,
		tempPattern: ".witself-memory-routing.md.witself-*",
		beginMarker: claudeMemoryRoutingBeginMarker,
		endMarker:   claudeMemoryRoutingEndMarker,
		block:       claudeMemoryRoutingBlock,
		removeEmpty: true,
	}, nil
}
