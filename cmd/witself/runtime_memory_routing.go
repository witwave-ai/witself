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
	return installRuntimeMemoryRoutingInstructionsAt(runtimeName, "")
}

func installRuntimeMemoryRoutingInstructionsAt(runtimeName, runtimeWorkspace string) (runtimeMemoryRoutingSnapshot, error) {
	spec, displayName, managed, err := runtimeMemoryRoutingSpecAt(runtimeName, runtimeWorkspace)
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

func removeRuntimeMemoryRoutingInstructionsAt(runtimeName, runtimeWorkspace string) (runtimeMemoryRoutingSnapshot, error) {
	spec, displayName, managed, err := runtimeMemoryRoutingSpecAt(runtimeName, runtimeWorkspace)
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

func runtimeMemoryRoutingCurrentAt(runtimeName, runtimeWorkspace string) (bool, error) {
	spec, displayName, managed, err := runtimeMemoryRoutingSpecAt(runtimeName, runtimeWorkspace)
	if err != nil {
		return false, err
	}
	if !managed {
		return true, nil
	}
	spec, err = normalizeManagedInstructionsSpec(spec)
	if err != nil {
		return false, fmt.Errorf("inspect %s instructions: %w", displayName, err)
	}
	snapshot, err := readManagedInstructionsSnapshot(spec)
	if err != nil {
		return false, fmt.Errorf("inspect %s instructions: %w", displayName, err)
	}
	if !snapshot.existed {
		return false, nil
	}
	if spec.exclusive {
		if err := validateExclusiveManagedInstructionsContent(snapshot.data, spec, true); err != nil {
			return false, err
		}
	}
	_, changed, err := upsertManagedInstructionsBlock(snapshot.data, spec)
	return !changed, err
}

func runtimeMemoryRoutingSpecAt(runtimeName, runtimeWorkspace string) (managedInstructionsSpec, string, bool, error) {
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
		spec, err := cursorManagedInstructionsSpec()
		return spec, "Cursor", true, err
	case transcriptcapture.RuntimeOpenClaw:
		var spec managedInstructionsSpec
		var err error
		if runtimeWorkspace == "" {
			spec, err = openClawManagedInstructionsSpec()
		} else {
			spec, err = openClawManagedInstructionsSpecAt(runtimeWorkspace)
		}
		return spec, "OpenClaw", true, err
	case transcriptcapture.RuntimeAntigravity:
		// Antigravity loads the always-on rule and MCP definition from one
		// exact-owned plugin directory. Its adapter installs that ownership unit
		// atomically instead of editing an independent routing file here.
		return managedInstructionsSpec{}, "Antigravity", false, nil
	case transcriptcapture.RuntimeCopilot:
		spec, err := copilotManagedInstructionsSpec()
		return spec, "GitHub Copilot", true, err
	default:
		return managedInstructionsSpec{}, "", false, fmt.Errorf("unsupported runtime %q", runtimeName)
	}
}
