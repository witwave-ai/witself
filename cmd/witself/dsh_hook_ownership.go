package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func dshUsesDedicatedHooks(cfg transcriptcapture.Config) bool {
	return cfg.Runtime == transcriptcapture.RuntimeDSH && cfg.HookMode == transcriptcapture.HookModeUser &&
		cfg.RuntimeConfigRoot != "" && cfg.HookConfigPath == filepath.Join(cfg.RuntimeConfigRoot, transcriptcapture.DSHDedicatedHooksFilename)
}

func planDSHHooksOwned(cfg *transcriptcapture.Config, previous *transcriptcapture.Config) error {
	clearHookOwnership(cfg)
	if cfg.HookMode == transcriptcapture.HookModeNone {
		return nil
	}
	if cfg.HookMode != transcriptcapture.HookModeUser {
		return errors.New("unsupported DeepSeek Harness hook mode")
	}
	path, err := dshHookConfigPathAt(cfg.RuntimeConfigRoot)
	if err != nil {
		return err
	}
	cfg.HookConfigPath = path
	if previous == nil || previous.HookMode != transcriptcapture.HookModeUser {
		return nil
	}
	if err := hydrateLegacyRuntimeHookOwnership(previous, previous.MCPCommand); err != nil {
		return err
	}
	if dshUsesDedicatedHooks(*previous) {
		cfg.DSHLegacyHookBridge = previous.DSHLegacyHookBridge
		return nil
	}
	prior, err := userHooksOptionsFromConfig(*previous, previous.MCPCommand)
	if err != nil {
		return err
	}
	cfg.DSHLegacyHookBridge, err = transcriptcapture.InspectDSHLegacyHooks(filepath.Join(cfg.RuntimeConfigRoot, dshHookConfigFileName), &prior)
	return err
}

// Inspect before granting a new journal authority over a dedicated file. An
// existing file cannot become owned merely because it equals a proposed binding.
func preflightDSHHooksTransaction(desired, previous *transcriptcapture.Config) error {
	if desired == nil || !dshUsesDedicatedHooks(*desired) {
		return nil
	}
	if _, err := dshManagedHookRowPath(*desired); err != nil {
		return err
	}
	if _, err := os.Lstat(desired.HookConfigPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errors.New("inspect dedicated DeepSeek Harness hooks failed")
	}
	if previous == nil || !dshUsesDedicatedHooks(*previous) {
		return errors.New("dedicated DeepSeek Harness hook file exists without prior durable ownership")
	}
	return verifyRuntimeHooksOwned(*previous)
}

type dshHookSelectionChangedError struct{ err error }

func (e *dshHookSelectionChangedError) Error() string { return e.err.Error() }
func (e *dshHookSelectionChangedError) Unwrap() error { return e.err }

// Called only for a legacy->dedicated migration. Rebinds carry the saved choice
// without consulting unrelated live commands. The inspector returns no content.
func checkDSHLegacyBridgeSelection(desired transcriptcapture.Config, previous *transcriptcapture.Config) (*transcriptcapture.UserHooksOptions, error) {
	if !dshUsesDedicatedHooks(desired) || previous == nil || previous.HookMode != transcriptcapture.HookModeUser || dshUsesDedicatedHooks(*previous) {
		return nil, nil
	}
	prior, err := userHooksOptionsFromConfig(*previous, previous.MCPCommand)
	if err != nil {
		return nil, err
	}
	selected, err := transcriptcapture.InspectDSHLegacyHooks(filepath.Join(desired.RuntimeConfigRoot, dshHookConfigFileName), &prior)
	if err != nil {
		return nil, &dshHookSelectionChangedError{err}
	}
	if selected != desired.DSHLegacyHookBridge {
		return nil, &dshHookSelectionChangedError{errors.New("legacy DeepSeek Harness hooks changed the planned compatibility bridge selection")}
	}
	return &prior, nil
}

// A narrow cut used by synthetic tests to exercise failures after either write.
var dshAfterHookMutationForTest func(string) error

func installDSHHooksOwned(cfg *transcriptcapture.Config, previous *transcriptcapture.Config, recovery bool) (string, bool, error) {
	if previous != nil {
		if err := hydrateLegacyRuntimeHookOwnership(previous, previous.MCPCommand); err != nil {
			return "", false, err
		}
	}
	if cfg.HookMode == transcriptcapture.HookModeNone {
		if previous == nil {
			return "", false, nil
		}
		touched, err := removeRuntimeHooksOwned(*previous)
		return "", touched, err
	}
	if _, err := dshManagedHookRowPath(*cfg); err != nil {
		return "", false, err
	}
	legacy, err := checkDSHLegacyBridgeSelection(*cfg, previous)
	if err != nil {
		return cfg.HookConfigPath, false, err
	}
	desired, err := userHooksOptionsFromConfig(*cfg, cfg.MCPCommand)
	if err != nil {
		return "", false, err
	}
	var prior *transcriptcapture.UserHooksOptions
	if previous != nil && previous.HookMode == transcriptcapture.HookModeUser && previous.HookConfigPath == desired.ConfigPath {
		opts, err := userHooksOptionsFromConfig(*previous, previous.MCPCommand)
		if err != nil {
			return "", false, err
		}
		prior = &opts
	}
	touched := false
	// Only a durable recovery journal can recognize a desired file written before
	// a crash. Do not take this shortcut on first install or ordinary rebind.
	if !recovery || transcriptcapture.VerifyOwnedHooks(desired) != nil {
		mutation, err := transcriptcapture.InstallOwnedHooks(desired, prior)
		touched = mutation.Touched
		if err != nil {
			return desired.ConfigPath, touched, err
		}
		if mutation.Touched && dshAfterHookMutationForTest != nil {
			if err := dshAfterHookMutationForTest("desired"); err != nil {
				return desired.ConfigPath, touched, err
			}
		}
	}
	if legacy != nil {
		// Repeat after the dedicated write and after subtraction, closing both cuts.
		if _, err := checkDSHLegacyBridgeSelection(*cfg, previous); err != nil {
			return desired.ConfigPath, touched, err
		}
		removed, err := transcriptcapture.RemoveOwnedHooks(*legacy)
		touched = touched || removed.Touched
		if err != nil {
			return desired.ConfigPath, touched, err
		}
		if removed.Touched && dshAfterHookMutationForTest != nil {
			if err := dshAfterHookMutationForTest("legacy"); err != nil {
				return desired.ConfigPath, touched, err
			}
		}
		if _, err := checkDSHLegacyBridgeSelection(*cfg, previous); err != nil {
			return desired.ConfigPath, touched, err
		}
	}
	cfg.HookConfigPath = desired.ConfigPath
	return desired.ConfigPath, touched, nil
}

// Schedule a synthetic operator edit after the early guard and prior verification.
var dshBeforeHookRestoreForTest func()

// Restore only the two reconstructible sets. Same-path rebinds use the generic
// exact replacement primitive; cross-path migrations reconcile independently.
func restoreDSHHooksOwned(attempted, previous *transcriptcapture.Config) error {
	if attempted == nil {
		empty := *previous
		empty.HookMode = transcriptcapture.HookModeNone
		clearHookOwnership(&empty)
		attempted = &empty
	}
	if previous != nil {
		if err := hydrateLegacyRuntimeHookOwnership(previous, previous.MCPCommand); err != nil {
			return err
		}
	}
	samePath := previous != nil && previous.HookMode == transcriptcapture.HookModeUser && attempted.HookMode == transcriptcapture.HookModeUser && previous.HookConfigPath == attempted.HookConfigPath
	if previous != nil && previous.HookMode == transcriptcapture.HookModeUser && !dshUsesDedicatedHooks(*previous) {
		authority := *previous
		if samePath {
			if err := verifyRuntimeHooksOwned(*previous); err == nil {
				return nil
			}
			authority = *attempted
		}
		if err := validateDSHLegacyRestoreDocument(authority); err != nil {
			return err
		}
	}
	if !samePath {
		if _, err := removeRuntimeHooksOwned(*attempted); err != nil {
			return err
		}
	}
	if previous == nil || previous.HookMode == transcriptcapture.HookModeNone {
		return nil
	}
	prior, err := userHooksOptionsFromConfig(*previous, previous.MCPCommand)
	if err != nil {
		return err
	}
	if err := transcriptcapture.VerifyOwnedHooks(prior); err == nil {
		return nil
	}
	authority := prior
	if samePath {
		authority, err = userHooksOptionsFromConfig(*attempted, attempted.MCPCommand)
		if err != nil {
			return err
		}
	}
	if dshBeforeHookRestoreForTest != nil {
		dshBeforeHookRestoreForTest()
	}
	_, err = transcriptcapture.InstallOwnedHooks(prior, &authority)
	return err
}

// The old bridge selects root.hooks when present, otherwise the bare event map.
// Adding a wrapped prior set over bare operator hooks would stop their execution.
// This early guard avoids removing attempted hooks when refusal is already known.
// InstallOwnedHooks must still validate its own snapshot before wrapping via CAS.
func validateDSHLegacyRestoreDocument(previous transcriptcapture.Config) error {
	prior, err := userHooksOptionsFromConfig(previous, previous.MCPCommand)
	if err != nil {
		return err
	}
	if _, err := transcriptcapture.InspectDSHLegacyHooks(prior.ConfigPath, &prior); err != nil {
		return err
	}
	// Reuse the bounded, identity-checked reader. A wider-mode foreign file is
	// also a safe rollback stop, not authority to rewrite operator content.
	snapshot, err := loadIntegrationTransactionJournalFile(prior.ConfigPath, "legacy DeepSeek Harness rollback document")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("unable to verify legacy DeepSeek Harness rollback document")
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(snapshot.raw, &root) != nil {
		return errors.New("invalid legacy DeepSeek Harness rollback document")
	}
	if _, wrapped := root["hooks"]; wrapped {
		return nil
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "SubagentStart", "SubagentStop"} {
		var rows []json.RawMessage
		if value := root[event]; value != nil {
			if json.Unmarshal(value, &rows) != nil || len(rows) > 0 {
				return errors.New("restoring legacy capture would mask bare operator hooks; preserving recovery state")
			}
		}
	}
	return nil
}
