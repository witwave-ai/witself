package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

// mcpRegistrationAlreadyMissing recognizes only the runtime CLIs' explicit
// response for an absent named MCP server. Other remove failures remain fatal so
// uninstall cannot silently leave an ambiguous or partially modified binding.
func mcpRegistrationAlreadyMissing(output []byte) bool {
	message := strings.ToLower(strings.TrimSpace(string(output)))
	return strings.Contains(message, "no mcp server named") && strings.Contains(message, "witself")
}

// preflightRuntimeMemoryRoutingRemovalAt validates that the managed routing
// block can be parsed without changing it. Uninstall uses this before probing
// the runtime CLI so malformed local state wins deterministically and no
// external command runs until local teardown is known to be safe.
func preflightRuntimeMemoryRoutingRemovalAt(runtimeName, runtimeWorkspace string) error {
	spec, displayName, managed, err := runtimeMemoryRoutingSpecAt(runtimeName, runtimeWorkspace)
	if err != nil {
		return fmt.Errorf("resolve %s memory routing instructions: %w", displayName, err)
	}
	if !managed {
		return nil
	}
	spec, err = normalizeManagedInstructionsSpec(spec)
	if err != nil {
		return fmt.Errorf("remove %s memory routing instructions: %w", displayName, err)
	}
	snapshot, err := readManagedInstructionsSnapshot(spec)
	if err != nil {
		return fmt.Errorf("remove %s memory routing instructions: %w", displayName, err)
	}
	if !snapshot.existed {
		return nil
	}
	if spec.exclusive {
		if err := validateExclusiveManagedInstructionsContent(snapshot.data, spec, false); err != nil {
			return fmt.Errorf("remove %s memory routing instructions: %w", displayName, err)
		}
	}
	if _, _, err := removeManagedInstructionsBlock(snapshot.data, spec); err != nil {
		return fmt.Errorf("remove %s memory routing instructions: %w", displayName, err)
	}
	return nil
}

func restoreRuntimeMCPBinding(runtimeName, runtimeCLI, executable string, previous, attempted *transcriptcapture.Config) error {
	if runtimeName == transcriptcapture.RuntimeAntigravity {
		return restoreAntigravityPlugin(previous, attempted)
	}
	if runtimeName == transcriptcapture.RuntimeOpenClaw {
		if attempted == nil {
			return errors.New("attempted OpenClaw integration binding is required for safe MCP rollback")
		}
		attemptedBinding, err := openClawMCPBindingFromConfig(executable, *attempted)
		if err != nil {
			return err
		}
		current, exists, err := inspectOpenClawMCPWithEnvironment(runtimeCLI, attemptedBinding.Env)
		if err != nil {
			return err
		}
		var previousBinding openClawMCPBinding
		if previous != nil {
			previousBinding, err = openClawMCPBindingFromConfig(executable, *previous)
			if err != nil {
				return err
			}
			if exists && equalOpenClawMCPBinding(current, previousBinding) {
				return nil
			}
		}
		if exists {
			if !equalOpenClawMCPBinding(current, attemptedBinding) {
				return errors.New("openclaw-managed mcp.servers.witself changed during rollback; refusing to modify it")
			}
			if err := unregisterOpenClawMCP(runtimeCLI, &attemptedBinding); err != nil {
				return err
			}
		}
		if previous == nil {
			return nil
		}
		return registerOpenClawMCPBinding(runtimeCLI, previousBinding)
	}
	if runtimeName == transcriptcapture.RuntimeCopilot {
		if attempted == nil {
			return errors.New("attempted GitHub Copilot integration binding is required for safe MCP rollback")
		}
		attemptedName, attemptedBinding, err := copilotMCPBindingFromConfig(executable, *attempted)
		if err != nil {
			return err
		}
		current, exists, err := inspectCopilotMCP(runtimeCLI, *attempted)
		if err != nil {
			return err
		}

		previousName := ""
		var previousBinding copilotMCPBinding
		if previous != nil {
			previousName, previousBinding, err = copilotMCPBindingFromConfig(previous.MCPCommand, *previous)
			if err != nil {
				return err
			}
			if exists && attemptedName == previousName && equalCopilotMCPBinding(current, previousBinding) {
				return nil
			}
		}

		if exists {
			if !equalCopilotMCPBinding(current, attemptedBinding) {
				return fmt.Errorf("GitHub Copilot MCP server %s changed during rollback; refusing to modify it", attemptedName)
			}
			if err := unregisterCopilotMCP(runtimeCLI, attempted); err != nil {
				return err
			}
		}
		if previous == nil {
			return nil
		}

		currentPrevious, previousExists, err := inspectCopilotMCP(runtimeCLI, *previous)
		if err != nil {
			return err
		}
		if previousExists {
			if equalCopilotMCPBinding(currentPrevious, previousBinding) {
				return nil
			}
			return fmt.Errorf("GitHub Copilot MCP server %s changed during rollback; refusing to replace it", previousName)
		}
		return registerCopilotMCP(runtimeCLI, *previous)
	}
	if previous == nil {
		return unregisterMCP(runtimeName, runtimeCLI)
	}
	account := strings.TrimSpace(previous.Account)
	if account == "" {
		account = "default"
	}
	realm := strings.TrimSpace(previous.Realm)
	if realm == "" {
		realm = "default"
	}
	agent := strings.TrimSpace(previous.Agent)
	if agent == "" {
		agent = strings.TrimSpace(previous.AgentName)
	}
	if agent == "" {
		return errors.New("previous integration has no agent name")
	}
	if runtimeName == transcriptcapture.RuntimeCursor && runtimeCLI == "" {
		serveArgs := []string{
			executable, "mcp", "serve", "--runtime", runtimeName,
			"--account", account, "--realm", realm, "--agent", agent,
		}
		if previous.Location.Name != "" {
			serveArgs = append(serveArgs, "--location", previous.Location.Name)
		}
		if err := registerCursorMCP(serveArgs); err != nil {
			return err
		}
		return errors.New("restored Cursor MCP configuration but could not re-enable it because the Cursor CLI is unavailable")
	}
	return registerMCP(runtimeName, runtimeCLI, executable, account, realm, agent, previous.Location.Name)
}

type runtimeHooksSnapshot struct {
	userPresent    bool
	managedPresent bool
}

func snapshotRuntimeHooks(runtimeName string) (runtimeHooksSnapshot, error) {
	if !supportsTranscriptHooks(runtimeName) {
		return runtimeHooksSnapshot{}, nil
	}
	userPresent, err := transcriptcapture.HooksInstalled(runtimeName)
	if err != nil {
		return runtimeHooksSnapshot{}, fmt.Errorf("inspect user hooks: %w", err)
	}
	snapshot := runtimeHooksSnapshot{userPresent: userPresent}
	if !supportsManagedHooks(runtimeName) {
		return snapshot, nil
	}
	opts, err := managedHooksOptions(runtimeName, transcriptcapture.ModeRaw, "", "", "", "", "")
	if err != nil {
		return runtimeHooksSnapshot{}, fmt.Errorf("resolve managed hooks: %w", err)
	}
	snapshot.managedPresent, err = transcriptcapture.ManagedHooksInstalled(opts)
	if err != nil {
		return runtimeHooksSnapshot{}, fmt.Errorf("inspect managed hooks: %w", err)
	}
	return snapshot, nil
}

func restoreRuntimeHooksBinding(runtimeName, executable string, previous *transcriptcapture.Config) error {
	snapshot := runtimeHooksSnapshot{}
	if previous != nil {
		hookMode, err := transcriptcapture.NormalizeHookMode(previous.HookMode)
		if err != nil {
			return err
		}
		snapshot.userPresent = hookMode == transcriptcapture.HookModeUser
		snapshot.managedPresent = hookMode == transcriptcapture.HookModeManaged
	}
	return restoreRuntimeHooksSnapshot(runtimeName, executable, previous, snapshot)
}

func restoreRuntimeHooksSnapshot(runtimeName, executable string, previous *transcriptcapture.Config, snapshot runtimeHooksSnapshot) error {
	if !supportsTranscriptHooks(runtimeName) {
		if snapshot.userPresent || snapshot.managedPresent {
			return fmt.Errorf("%s does not support transcript hooks", runtimeName)
		}
		return nil
	}
	if previous == nil && (snapshot.userPresent || snapshot.managedPresent) {
		return errors.New("cannot reconstruct pre-existing hooks without an integration binding")
	}
	var rollbackErrs []error
	if previous == nil || !snapshot.userPresent {
		if _, err := transcriptcapture.RemoveHooks(runtimeName); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("remove user hooks: %w", err))
		}
	} else if err := installUserRuntimeHooks(runtimeName, executable, previous); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	}

	if supportsManagedHooks(runtimeName) {
		if previous == nil || !snapshot.managedPresent {
			if _, err := removeManagedRuntimeHooks(runtimeName); err != nil {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("remove managed hooks: %w", err))
			}
		} else if err := installManagedRuntimeHooksBinding(runtimeName, executable, previous); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
	}
	return errors.Join(rollbackErrs...)
}

func installUserRuntimeHooks(runtimeName, executable string, previous *transcriptcapture.Config) error {
	account := strings.TrimSpace(previous.Account)
	if account == "" {
		account = "default"
	}
	realm := strings.TrimSpace(previous.Realm)
	if realm == "" {
		realm = "default"
	}
	agent := strings.TrimSpace(previous.Agent)
	if agent == "" {
		agent = strings.TrimSpace(previous.AgentName)
	}
	if agent == "" {
		return errors.New("previous integration has no agent name")
	}
	if _, err := transcriptcapture.InstallHooks(
		runtimeName,
		previous.CaptureMode,
		executable,
		account,
		realm,
		agent,
		previous.Location.Name,
	); err != nil {
		return fmt.Errorf("restore user hooks: %w", err)
	}
	return nil
}

func installManagedRuntimeHooksBinding(runtimeName, executable string, previous *transcriptcapture.Config) error {
	account := strings.TrimSpace(previous.Account)
	if account == "" {
		account = "default"
	}
	realm := strings.TrimSpace(previous.Realm)
	if realm == "" {
		realm = "default"
	}
	agent := strings.TrimSpace(previous.Agent)
	if agent == "" {
		agent = strings.TrimSpace(previous.AgentName)
	}
	if agent == "" {
		return errors.New("previous integration has no agent name")
	}
	if _, err := installManagedRuntimeHooks(
		runtimeName,
		previous.CaptureMode,
		executable,
		account,
		realm,
		agent,
		previous.Location.Name,
	); err != nil {
		return fmt.Errorf("restore managed hooks: %w", err)
	}
	return nil
}
