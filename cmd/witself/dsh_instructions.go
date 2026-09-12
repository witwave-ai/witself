package main

import (
	"errors"
	"path/filepath"
)

const (
	dshMemoryRoutingFile = "AGENTS.md"

	// dsh loads $DSH_HOME/AGENTS.md on a session's first request, ahead of the
	// project AGENTS.md/CLAUDE.md chain. The file is shared with anything else
	// the operator keeps there, so Witself owns only its fenced block.
	dshMemoryRoutingBeginMarker = "<!-- BEGIN WITSELF MANAGED DSH ROUTING -->"
	dshMemoryRoutingEndMarker   = "<!-- END WITSELF MANAGED DSH ROUTING -->"

	// dsh has no validated model-visible hook channel, so its filesystem rule
	// uses the portable runtime-neutral contract. Exact tool names also arrive
	// through the MCP server instructions.
	dshMemoryRoutingInstructions = runtimeNeutralMemoryRoutingInstructions
)

var dshMemoryRoutingBlock = []byte(
	dshMemoryRoutingBeginMarker + "\n" +
		dshMemoryRoutingInstructions + "\n\n" +
		foregroundMessagingRoutingInstructions + "\n\n" +
		avatarRoutingInstructions + "\n\n" +
		secretRoutingInstructions + "\n" +
		dshMemoryRoutingEndMarker,
)

func dshMemoryRoutingPathAt(configRoot string) (string, error) {
	root, err := cleanCopilotAbsolutePath("DeepSeek Harness routing config root", configRoot)
	if err != nil {
		return "", err
	}
	if root != configRoot {
		return "", errors.New("DeepSeek Harness routing config root must be canonical")
	}
	return filepath.Join(root, dshMemoryRoutingFile), nil
}

func dshManagedInstructionsSpecAt(configRoot string) (managedInstructionsSpec, error) {
	path, err := dshMemoryRoutingPathAt(configRoot)
	if err != nil {
		return managedInstructionsSpec{}, err
	}
	return managedInstructionsSpec{
		path:        path,
		fileName:    dshMemoryRoutingFile,
		tempPattern: ".AGENTS.md.witself-*",
		beginMarker: dshMemoryRoutingBeginMarker,
		endMarker:   dshMemoryRoutingEndMarker,
		block:       dshMemoryRoutingBlock,
	}, nil
}
