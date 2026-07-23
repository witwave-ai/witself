package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func hydrateLegacyRuntimeHookOwnership(cfg *transcriptcapture.Config, executable string) error {
	if cfg == nil {
		return nil
	}
	if strings.TrimSpace(executable) == "" {
		executable = cfg.MCPCommand
	}
	switch cfg.HookMode {
	case transcriptcapture.HookModeUser:
		opts, err := userHooksOptionsFromConfig(*cfg, executable)
		if err != nil {
			return err
		}
		if cfg.HookConfigPath == "" {
			cfg.HookConfigPath = opts.ConfigPath
		}
		return nil
	case transcriptcapture.HookModeManaged:
		if !supportsManagedHooks(cfg.Runtime) {
			return fmt.Errorf("%s does not support administrator-managed hooks on this platform", cfg.Runtime)
		}
		if _, ok, err := managedHookOwnershipFromConfig(*cfg); err != nil || ok {
			return err
		}
		result, err := inspectLegacyManagedRuntimeHooksAtHome(
			cfg.Runtime,
			cfg.CaptureMode,
			executable,
			configAccount(*cfg),
			configRealm(*cfg),
			configAgent(*cfg),
			cfg.Location.Name,
			cfg.MCPEnvironment["WITSELF_HOME"],
		)
		if errors.Is(err, os.ErrNotExist) || result.Missing {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reconstruct legacy managed hook ownership: %w", err)
		}
		setManagedHookOwnership(cfg, result.Ownership)
	}
	return nil
}

// planRuntimeHooksOwned resolves the exact deletion capability before the
// provider transaction journal or staged integration config is written.
func planRuntimeHooksOwned(cfg *transcriptcapture.Config, previous *transcriptcapture.Config) error {
	if cfg == nil || !supportsTranscriptHooks(cfg.Runtime) {
		return nil
	}
	clearHookOwnership(cfg)
	switch cfg.HookMode {
	case transcriptcapture.HookModeUser:
		desired, err := userHooksOptionsFromConfig(*cfg, cfg.MCPCommand)
		if err != nil {
			return err
		}
		if previous != nil && previous.HookMode == transcriptcapture.HookModeUser {
			prior, err := userHooksOptionsFromConfig(*previous, previous.MCPCommand)
			if err != nil {
				return err
			}
			desired.ConfigPath = prior.ConfigPath
		}
		cfg.HookConfigPath = desired.ConfigPath
		return nil
	case transcriptcapture.HookModeManaged:
		var prior *transcriptcapture.ManagedHookOwnership
		if previous != nil && previous.HookMode == transcriptcapture.HookModeManaged {
			ownership, ok, err := managedHookOwnershipFromConfig(*previous)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("managed hook ownership is missing from the prior integration record")
			}
			prior = &ownership
		}
		result, err := planManagedRuntimeHooksOwnedAtHome(
			cfg.Runtime,
			cfg.CaptureMode,
			cfg.MCPCommand,
			configAccount(*cfg),
			configRealm(*cfg),
			configAgent(*cfg),
			cfg.Location.Name,
			cfg.MCPEnvironment["WITSELF_HOME"],
			prior,
		)
		if err != nil {
			return err
		}
		setManagedHookOwnership(cfg, result.Ownership)
		return nil
	default:
		return fmt.Errorf("unsupported transcript hook mode %q", cfg.HookMode)
	}
}

// installRuntimeHooksOwned converges cfg.HookMode from the exact prior binding.
// It mutates cfg only after a durable hook mutation has returned its ownership
// capability so final config persistence cannot lose the deletion target.
func installRuntimeHooksOwned(cfg *transcriptcapture.Config, previous *transcriptcapture.Config) (string, bool, error) {
	if cfg == nil || !supportsTranscriptHooks(cfg.Runtime) {
		return "", false, nil
	}
	if previous != nil {
		if err := hydrateLegacyRuntimeHookOwnership(previous, previous.MCPCommand); err != nil {
			return "", false, err
		}
	}
	plannedConfigPath := cfg.HookConfigPath
	var plannedManaged transcriptcapture.ManagedHookOwnership
	plannedManagedSet := false
	if cfg.HookMode == transcriptcapture.HookModeManaged {
		var plannedManagedErr error
		plannedManaged, plannedManagedSet, plannedManagedErr = managedHookOwnershipFromConfig(*cfg)
		if plannedManagedErr != nil {
			return "", false, plannedManagedErr
		}
	}
	clearHookOwnership(cfg)
	var path string
	touched := false
	switch cfg.HookMode {
	case transcriptcapture.HookModeUser:
		desired, err := userHooksOptionsFromConfig(*cfg, cfg.MCPCommand)
		if err != nil {
			return "", false, err
		}
		var prior *transcriptcapture.UserHooksOptions
		if previous != nil && previous.HookMode == transcriptcapture.HookModeUser {
			previousOpts, err := userHooksOptionsFromConfig(*previous, previous.MCPCommand)
			if err != nil {
				return "", false, err
			}
			desired.ConfigPath = previousOpts.ConfigPath
			prior = &previousOpts
		}
		mutation, err := transcriptcapture.InstallOwnedHooks(desired, prior)
		touched = mutation.Touched
		if err != nil {
			return mutation.Path, touched, err
		}
		cfg.HookConfigPath = mutation.Path
		if plannedConfigPath != "" && mutation.Path != plannedConfigPath {
			return mutation.Path, touched, errors.New("installed user hook path differs from the durable transaction plan")
		}
		path = mutation.Path
		if previous != nil && previous.HookMode == transcriptcapture.HookModeManaged {
			removed, err := removeManagedHooksFromConfig(*previous)
			touched = touched || removed.Touched
			if err != nil {
				return path, touched, err
			}
		}
	case transcriptcapture.HookModeManaged:
		var prior *transcriptcapture.ManagedHookOwnership
		if previous != nil && previous.HookMode == transcriptcapture.HookModeManaged {
			ownership, ok, err := managedHookOwnershipFromConfig(*previous)
			if err != nil {
				return "", false, err
			}
			if ok {
				prior = &ownership
			}
		}
		result, err := installManagedRuntimeHooksOwnedAtHome(
			cfg.Runtime,
			cfg.CaptureMode,
			cfg.MCPCommand,
			configAccount(*cfg),
			configRealm(*cfg),
			configAgent(*cfg),
			cfg.Location.Name,
			cfg.MCPEnvironment["WITSELF_HOME"],
			prior,
		)
		touched = result.Touched
		if result.Ownership.PolicyPath != "" {
			setManagedHookOwnership(cfg, result.Ownership)
		}
		if err != nil {
			return result.Path, touched, err
		}
		if plannedManagedSet && result.Ownership != plannedManaged {
			return result.Path, touched, errors.New("installed managed hook ownership differs from the durable transaction plan")
		}
		path = result.Path
		if previous != nil && previous.HookMode == transcriptcapture.HookModeUser {
			removed, err := removeUserHooksFromConfig(*previous)
			touched = touched || removed.Touched
			if err != nil {
				return path, touched, err
			}
		}
	default:
		return "", false, fmt.Errorf("unsupported transcript hook mode %q", cfg.HookMode)
	}
	return path, touched, nil
}

func recoverRuntimeHooksOwned(desired *transcriptcapture.Config, previous *transcriptcapture.Config) error {
	if desired == nil || !supportsTranscriptHooks(desired.Runtime) {
		return nil
	}
	// The desired binding itself is durable recovery authority. Accept it only
	// when every exact handler or managed digest already verifies.
	if err := verifyRuntimeHooksOwned(*desired); err == nil {
		return nil
	}
	_, _, err := installRuntimeHooksOwned(desired, previous)
	return err
}

func removeRuntimeHooksOwned(cfg transcriptcapture.Config) (bool, error) {
	if err := hydrateLegacyRuntimeHookOwnership(&cfg, cfg.MCPCommand); err != nil {
		return false, err
	}
	switch cfg.HookMode {
	case transcriptcapture.HookModeUser:
		result, err := removeUserHooksFromConfig(cfg)
		return result.Touched, err
	case transcriptcapture.HookModeManaged:
		result, err := removeManagedHooksFromConfig(cfg)
		return result.Touched, err
	case transcriptcapture.HookModeNone:
		return false, nil
	default:
		return false, fmt.Errorf("unsupported transcript hook mode %q", cfg.HookMode)
	}
}

func restoreRuntimeHooksOwned(attempted, previous *transcriptcapture.Config) error {
	var errs []error
	if attempted != nil {
		if _, err := removeRuntimeHooksOwned(*attempted); err != nil {
			errs = append(errs, fmt.Errorf("remove attempted runtime hooks: %w", err))
		}
	}
	if previous == nil {
		return errors.Join(errs...)
	}
	if err := hydrateLegacyRuntimeHookOwnership(previous, previous.MCPCommand); err != nil {
		errs = append(errs, err)
		return errors.Join(errs...)
	}
	switch previous.HookMode {
	case transcriptcapture.HookModeUser:
		opts, err := userHooksOptionsFromConfig(*previous, previous.MCPCommand)
		if err != nil {
			errs = append(errs, err)
			break
		}
		if err := transcriptcapture.VerifyOwnedHooks(opts); err == nil {
			break
		}
		if _, err := transcriptcapture.InstallOwnedHooks(opts, nil); err != nil {
			errs = append(errs, fmt.Errorf("restore prior user hooks: %w", err))
		}
	case transcriptcapture.HookModeManaged:
		ownership, ok, err := managedHookOwnershipFromConfig(*previous)
		if err != nil {
			errs = append(errs, err)
			break
		}
		if ok {
			if err := transcriptcapture.VerifyManagedHooksOwned(previous.Runtime, ownership); err == nil {
				break
			}
		}
		result, err := installManagedRuntimeHooksOwnedAtHome(
			previous.Runtime,
			previous.CaptureMode,
			previous.MCPCommand,
			configAccount(*previous),
			configRealm(*previous),
			configAgent(*previous),
			previous.Location.Name,
			previous.MCPEnvironment["WITSELF_HOME"],
			nil,
		)
		if err != nil {
			errs = append(errs, fmt.Errorf("restore prior managed hooks: %w", err))
		} else {
			setManagedHookOwnership(previous, result.Ownership)
		}
	}
	return errors.Join(errs...)
}

func verifyRuntimeHooksOwned(cfg transcriptcapture.Config) error {
	if cfg.HookMode == transcriptcapture.HookModeNone {
		return nil
	}
	if !supportsTranscriptHooks(cfg.Runtime) {
		return fmt.Errorf("%s does not support transcript hooks on this platform", cfg.Runtime)
	}
	if err := hydrateLegacyRuntimeHookOwnership(&cfg, cfg.MCPCommand); err != nil {
		return err
	}
	switch cfg.HookMode {
	case transcriptcapture.HookModeUser:
		opts, err := userHooksOptionsFromConfig(cfg, cfg.MCPCommand)
		if err != nil {
			return err
		}
		return transcriptcapture.VerifyOwnedHooks(opts)
	case transcriptcapture.HookModeManaged:
		ownership, ok, err := managedHookOwnershipFromConfig(cfg)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("managed hook ownership is missing from the integration record")
		}
		return transcriptcapture.VerifyManagedHooksOwned(cfg.Runtime, ownership)
	default:
		return fmt.Errorf("unsupported transcript hook mode %q", cfg.HookMode)
	}
}

func userHooksOptionsFromConfig(cfg transcriptcapture.Config, executable string) (transcriptcapture.UserHooksOptions, error) {
	if strings.TrimSpace(executable) == "" {
		executable = cfg.MCPCommand
	}
	opts, err := transcriptcapture.DefaultUserHooksOptions(
		cfg.Runtime,
		cfg.CaptureMode,
		executable,
		configAccount(cfg),
		configRealm(cfg),
		configAgent(cfg),
		cfg.Location.Name,
		cfg.MCPEnvironment["WITSELF_HOME"],
	)
	if err != nil {
		return transcriptcapture.UserHooksOptions{}, err
	}
	if cfg.HookConfigPath != "" {
		opts.ConfigPath = cfg.HookConfigPath
	}
	return opts, nil
}

func removeUserHooksFromConfig(cfg transcriptcapture.Config) (transcriptcapture.HookMutation, error) {
	opts, err := userHooksOptionsFromConfig(cfg, cfg.MCPCommand)
	if err != nil {
		return transcriptcapture.HookMutation{}, err
	}
	return transcriptcapture.RemoveOwnedHooks(opts)
}

func removeManagedHooksFromConfig(cfg transcriptcapture.Config) (managedHooksHelperResult, error) {
	ownership, ok, err := managedHookOwnershipFromConfig(cfg)
	if err != nil {
		return managedHooksHelperResult{}, err
	}
	if !ok {
		return managedHooksHelperResult{}, errors.New("managed hook ownership is missing from the integration record")
	}
	return removeManagedRuntimeHooksOwned(cfg.Runtime, ownership)
}

func managedHookOwnershipFromConfig(cfg transcriptcapture.Config) (transcriptcapture.ManagedHookOwnership, bool, error) {
	fields := []string{
		cfg.HookConfigPath,
		cfg.HookManagedDir,
		cfg.HookRunnerPath,
		cfg.HookRunnerDigest,
		cfg.HookPolicyDigest,
	}
	allEmpty := true
	allSet := true
	for _, field := range fields {
		allEmpty = allEmpty && strings.TrimSpace(field) == ""
		allSet = allSet && strings.TrimSpace(field) != ""
	}
	if allEmpty {
		return transcriptcapture.ManagedHookOwnership{}, false, nil
	}
	if !allSet {
		return transcriptcapture.ManagedHookOwnership{}, false, errors.New("managed hook ownership in the integration record is incomplete")
	}
	ownership := transcriptcapture.ManagedHookOwnership{
		PolicyPath: cfg.HookConfigPath, ManagedDir: cfg.HookManagedDir,
		RunnerPath: cfg.HookRunnerPath, RunnerDigest: cfg.HookRunnerDigest,
		PolicyDigest: cfg.HookPolicyDigest,
	}
	return ownership, true, nil
}

func setManagedHookOwnership(cfg *transcriptcapture.Config, ownership transcriptcapture.ManagedHookOwnership) {
	cfg.HookConfigPath = ownership.PolicyPath
	cfg.HookManagedDir = ownership.ManagedDir
	cfg.HookRunnerPath = ownership.RunnerPath
	cfg.HookRunnerDigest = ownership.RunnerDigest
	cfg.HookPolicyDigest = ownership.PolicyDigest
}

func copyHookOwnership(dst *transcriptcapture.Config, src transcriptcapture.Config) {
	dst.HookConfigPath = src.HookConfigPath
	dst.HookManagedDir = src.HookManagedDir
	dst.HookRunnerPath = src.HookRunnerPath
	dst.HookRunnerDigest = src.HookRunnerDigest
	dst.HookPolicyDigest = src.HookPolicyDigest
}

func clearHookOwnership(cfg *transcriptcapture.Config) {
	cfg.HookConfigPath = ""
	cfg.HookManagedDir = ""
	cfg.HookRunnerPath = ""
	cfg.HookRunnerDigest = ""
	cfg.HookPolicyDigest = ""
}

func configAccount(cfg transcriptcapture.Config) string {
	if value := strings.TrimSpace(cfg.Account); value != "" {
		return value
	}
	return "default"
}

func configRealm(cfg transcriptcapture.Config) string {
	if value := strings.TrimSpace(cfg.Realm); value != "" {
		return value
	}
	return "default"
}

func configAgent(cfg transcriptcapture.Config) string {
	if value := strings.TrimSpace(cfg.Agent); value != "" {
		return value
	}
	return strings.TrimSpace(cfg.AgentName)
}
