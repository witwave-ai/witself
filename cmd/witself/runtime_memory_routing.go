package main

import (
	"fmt"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type runtimeMemoryRoutingSnapshot struct {
	runtimeName string
	displayName string
	path        string
	snapshot    managedInstructionsSnapshot
	managed     bool
}

func installRuntimeMemoryRoutingInstructions(runtimeName string) (runtimeMemoryRoutingSnapshot, error) {
	spec, displayName, managed, err := runtimeMemoryRoutingSpec(runtimeName)
	if err != nil {
		return runtimeMemoryRoutingSnapshot{}, fmt.Errorf("resolve %s memory routing instructions: %w", displayName, err)
	}
	if !managed {
		return runtimeMemoryRoutingSnapshot{}, nil
	}
	if runtimeName == transcriptcapture.RuntimeCodex {
		if err := validateCodexAgentsFileIsActive(spec.path); err != nil {
			return runtimeMemoryRoutingSnapshot{}, fmt.Errorf("resolve %s memory routing instructions: %w", displayName, err)
		}
	}
	snapshot, err := installManagedInstructions(spec)
	if err != nil {
		return runtimeMemoryRoutingSnapshot{}, fmt.Errorf("install %s memory routing instructions: %w", displayName, err)
	}
	return runtimeMemoryRoutingSnapshot{
		runtimeName: runtimeName,
		displayName: displayName,
		path:        spec.path,
		snapshot:    snapshot,
		managed:     true,
	}, nil
}

func removeRuntimeMemoryRoutingInstructions(runtimeName string) (runtimeMemoryRoutingSnapshot, error) {
	spec, displayName, managed, err := runtimeMemoryRoutingSpec(runtimeName)
	if err != nil {
		return runtimeMemoryRoutingSnapshot{}, fmt.Errorf("resolve %s memory routing instructions: %w", displayName, err)
	}
	if !managed {
		return runtimeMemoryRoutingSnapshot{}, nil
	}
	snapshot, err := removeManagedInstructions(spec)
	if err != nil {
		return runtimeMemoryRoutingSnapshot{}, fmt.Errorf("remove %s memory routing instructions: %w", displayName, err)
	}
	return runtimeMemoryRoutingSnapshot{
		runtimeName: runtimeName,
		displayName: displayName,
		path:        spec.path,
		snapshot:    snapshot,
		managed:     true,
	}, nil
}

func (state runtimeMemoryRoutingSnapshot) restore() error {
	if !state.managed {
		return nil
	}
	return state.snapshot.restore()
}

func runtimeMemoryRoutingSpec(runtimeName string) (managedInstructionsSpec, string, bool, error) {
	switch runtimeName {
	case transcriptcapture.RuntimeCodex:
		path, err := codexAgentsPath()
		if err != nil {
			return managedInstructionsSpec{}, "Codex", true, err
		}
		return codexManagedInstructionsSpec(path), "Codex", true, nil
	case transcriptcapture.RuntimeClaudeCode:
		spec, err := claudeManagedInstructionsSpec()
		return spec, "Claude Code", true, err
	case transcriptcapture.RuntimeGrokBuild:
		spec, err := grokManagedInstructionsSpec()
		return spec, "Grok Build", true, err
	case transcriptcapture.RuntimeCursor:
		return managedInstructionsSpec{}, "", false, nil
	default:
		return managedInstructionsSpec{}, "", false, fmt.Errorf("unsupported runtime %q", runtimeName)
	}
}
