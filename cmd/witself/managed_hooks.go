package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const managedHooksTestRootEnv = "WITSELF_TEST_MANAGED_HOOKS_ROOT"

func managedHooksCmd(args []string) int {
	if len(args) == 0 ||
		(args[0] != "install" && args[0] != "remove" &&
			args[0] != "plan-owned" && args[0] != "install-owned" &&
			args[0] != "remove-owned" && args[0] != "inspect-legacy") {
		fmt.Fprintln(os.Stderr, "witself: invalid managed hooks helper invocation")
		return 2
	}
	action := args[0]
	fs := flag.NewFlagSet("_managed-hooks "+action, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtimeName := fs.String("runtime", "", "codex|claude-code")
	mode := fs.String("capture", transcriptcapture.ModeRaw, "messages|trace|raw")
	executable := fs.String("executable", "", "absolute Witself executable path")
	account := fs.String("account", "", "installed account name")
	realm := fs.String("realm", "", "installed realm name")
	agent := fs.String("agent", "", "installed agent name")
	location := fs.String("location", "", "optional installation location label")
	witselfHome := fs.String("witself-home", "", "installed Witself state root")
	ownershipJSON := fs.String("ownership-json", "", "exact prior managed hook ownership")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if os.Geteuid() != 0 && strings.TrimSpace(os.Getenv(managedHooksTestRootEnv)) == "" {
		fmt.Fprintln(os.Stderr, "witself: managed hooks helper requires administrator privileges")
		return 1
	}
	opts, err := managedHooksOptions(*runtimeName, *mode, *executable, *account, *realm, *agent, *location)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if strings.TrimSpace(*witselfHome) != "" {
		opts.WitselfHome, err = cleanCopilotAbsolutePath("WITSELF_HOME", *witselfHome)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
	}
	switch action {
	case "install":
		_, err = transcriptcapture.InstallManagedHooks(opts)
	case "remove":
		_, err = transcriptcapture.RemoveManagedHooks(opts)
	case "install-owned":
		var previous *transcriptcapture.ManagedHookOwnership
		if strings.TrimSpace(*ownershipJSON) != "" {
			var decoded transcriptcapture.ManagedHookOwnership
			if err = json.Unmarshal([]byte(*ownershipJSON), &decoded); err != nil {
				fmt.Fprintf(os.Stderr, "witself: parse managed hook ownership: %v\n", err)
				return 1
			}
			previous = &decoded
		}
		var ownership transcriptcapture.ManagedHookOwnership
		var touched bool
		ownership, touched, err = transcriptcapture.InstallManagedHooksOwned(opts, previous)
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(managedHooksHelperResult{
				Path: ownership.PolicyPath, Ownership: ownership, Touched: touched,
			})
		}
	case "plan-owned":
		var previous *transcriptcapture.ManagedHookOwnership
		if strings.TrimSpace(*ownershipJSON) != "" {
			var decoded transcriptcapture.ManagedHookOwnership
			if err = json.Unmarshal([]byte(*ownershipJSON), &decoded); err != nil {
				fmt.Fprintf(os.Stderr, "witself: parse managed hook ownership: %v\n", err)
				return 1
			}
			previous = &decoded
		}
		var ownership transcriptcapture.ManagedHookOwnership
		ownership, err = transcriptcapture.PlanManagedHooksOwned(opts, previous)
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(managedHooksHelperResult{
				Path: ownership.PolicyPath, Ownership: ownership,
			})
		}
	case "remove-owned":
		var ownership transcriptcapture.ManagedHookOwnership
		if err = json.Unmarshal([]byte(*ownershipJSON), &ownership); err != nil {
			fmt.Fprintf(os.Stderr, "witself: parse managed hook ownership: %v\n", err)
			return 1
		}
		var touched bool
		touched, err = transcriptcapture.RemoveManagedHooksOwned(*runtimeName, ownership)
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(managedHooksHelperResult{
				Path: ownership.PolicyPath, Ownership: ownership, Touched: touched,
			})
		}
	case "inspect-legacy":
		var ownership transcriptcapture.ManagedHookOwnership
		ownership, err = transcriptcapture.ReconstructLegacyManagedHookOwnership(opts)
		if errors.Is(err, os.ErrNotExist) {
			err = json.NewEncoder(os.Stdout).Encode(managedHooksHelperResult{Missing: true})
		} else if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(managedHooksHelperResult{
				Path: ownership.PolicyPath, Ownership: ownership,
			})
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %s managed hooks: %v\n", action, err)
		return 1
	}
	return 0
}

type managedHooksHelperResult struct {
	Path      string                                 `json:"path"`
	Ownership transcriptcapture.ManagedHookOwnership `json:"ownership"`
	Touched   bool                                   `json:"touched"`
	Missing   bool                                   `json:"missing,omitempty"`
}

func installManagedRuntimeHooks(runtimeName, mode, executable, account, realm, agent, location string) (string, error) {
	return installManagedRuntimeHooksAtHome(runtimeName, mode, executable, account, realm, agent, location, "")
}

func installManagedRuntimeHooksAtHome(runtimeName, mode, executable, account, realm, agent, location, witselfHome string) (string, error) {
	return runManagedHooksWithElevationAtHome("install", runtimeName, mode, executable, account, realm, agent, location, witselfHome)
}

func removeManagedRuntimeHooks(runtimeName string) (string, error) {
	return runManagedHooksWithElevation("remove", runtimeName, transcriptcapture.ModeRaw, "", "", "", "", "")
}

func installManagedRuntimeHooksOwnedAtHome(
	runtimeName, mode, executable, account, realm, agent, location, witselfHome string,
	previous *transcriptcapture.ManagedHookOwnership,
) (managedHooksHelperResult, error) {
	return runOwnedManagedHooksWithElevation(
		"install-owned", runtimeName, mode, executable, account, realm, agent, location, witselfHome, previous,
	)
}

func planManagedRuntimeHooksOwnedAtHome(
	runtimeName, mode, executable, account, realm, agent, location, witselfHome string,
	previous *transcriptcapture.ManagedHookOwnership,
) (managedHooksHelperResult, error) {
	return runOwnedManagedHooksWithElevation(
		"plan-owned", runtimeName, mode, executable, account, realm, agent, location, witselfHome, previous,
	)
}

func removeManagedRuntimeHooksOwned(runtimeName string, ownership transcriptcapture.ManagedHookOwnership) (managedHooksHelperResult, error) {
	return runOwnedManagedHooksWithElevation(
		"remove-owned", runtimeName, transcriptcapture.ModeRaw, "", "", "", "", "", "", &ownership,
	)
}

func inspectLegacyManagedRuntimeHooksAtHome(
	runtimeName, mode, executable, account, realm, agent, location, witselfHome string,
) (managedHooksHelperResult, error) {
	return runOwnedManagedHooksWithElevation(
		"inspect-legacy", runtimeName, mode, executable, account, realm, agent, location, witselfHome, nil,
	)
}

func runManagedHooksWithElevation(action, runtimeName, mode, executable, account, realm, agent, location string) (string, error) {
	return runManagedHooksWithElevationAtHome(action, runtimeName, mode, executable, account, realm, agent, location, "")
}

func runManagedHooksWithElevationAtHome(action, runtimeName, mode, executable, account, realm, agent, location, witselfHome string) (string, error) {
	opts, err := managedHooksOptions(runtimeName, mode, executable, account, realm, agent, location)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(witselfHome) != "" {
		opts.WitselfHome, err = cleanCopilotAbsolutePath("WITSELF_HOME", witselfHome)
		if err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(os.Getenv(managedHooksTestRootEnv)) != "" || os.Geteuid() == 0 {
		if action == "install" {
			return transcriptcapture.InstallManagedHooks(opts)
		}
		return transcriptcapture.RemoveManagedHooks(opts)
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return "", fmt.Errorf("managed hook installation is not supported on %s", runtime.GOOS)
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return "", errorsWithAdminHint(err)
	}
	helperExecutable, err := currentExecutablePath()
	if err != nil {
		return "", err
	}
	helperArgs := []string{"--", helperExecutable, "_managed-hooks", action, "--runtime", runtimeName}
	if action == "install" {
		helperArgs = append(helperArgs, "--capture", mode, "--executable", executable, "--account", account, "--realm", realm, "--agent", agent)
		if location != "" {
			helperArgs = append(helperArgs, "--location", location)
		}
		if opts.WitselfHome != "" {
			helperArgs = append(helperArgs, "--witself-home", opts.WitselfHome)
		}
	}
	cmd := exec.Command(sudo, helperArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("administrator-managed hook %s failed: %w", action, err)
	}
	return opts.PolicyPath(), nil
}

func runOwnedManagedHooksWithElevation(
	action, runtimeName, mode, executable, account, realm, agent, location, witselfHome string,
	ownership *transcriptcapture.ManagedHookOwnership,
) (managedHooksHelperResult, error) {
	opts, err := managedHooksOptions(runtimeName, mode, executable, account, realm, agent, location)
	if err != nil {
		return managedHooksHelperResult{}, err
	}
	if strings.TrimSpace(witselfHome) != "" {
		opts.WitselfHome, err = cleanCopilotAbsolutePath("WITSELF_HOME", witselfHome)
		if err != nil {
			return managedHooksHelperResult{}, err
		}
	}
	if strings.TrimSpace(os.Getenv(managedHooksTestRootEnv)) != "" || os.Geteuid() == 0 {
		switch action {
		case "plan-owned":
			planned, err := transcriptcapture.PlanManagedHooksOwned(opts, ownership)
			return managedHooksHelperResult{Path: planned.PolicyPath, Ownership: planned}, err
		case "install-owned":
			installed, touched, err := transcriptcapture.InstallManagedHooksOwned(opts, ownership)
			return managedHooksHelperResult{Path: installed.PolicyPath, Ownership: installed, Touched: touched}, err
		case "remove-owned":
			if ownership == nil {
				return managedHooksHelperResult{}, fmt.Errorf("managed hook ownership is required for removal")
			}
			touched, err := transcriptcapture.RemoveManagedHooksOwned(runtimeName, *ownership)
			return managedHooksHelperResult{Path: ownership.PolicyPath, Ownership: *ownership, Touched: touched}, err
		case "inspect-legacy":
			installed, err := transcriptcapture.ReconstructLegacyManagedHookOwnership(opts)
			if errors.Is(err, os.ErrNotExist) {
				return managedHooksHelperResult{Missing: true}, nil
			}
			return managedHooksHelperResult{Path: installed.PolicyPath, Ownership: installed}, err
		default:
			return managedHooksHelperResult{}, fmt.Errorf("unsupported owned managed hook action %q", action)
		}
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return managedHooksHelperResult{}, fmt.Errorf("managed hook installation is not supported on %s", runtime.GOOS)
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return managedHooksHelperResult{}, errorsWithAdminHint(err)
	}
	helperExecutable, err := currentExecutablePath()
	if err != nil {
		return managedHooksHelperResult{}, err
	}
	helperArgs := []string{"--", helperExecutable, "_managed-hooks", action, "--runtime", runtimeName}
	if action == "plan-owned" || action == "install-owned" || action == "inspect-legacy" {
		helperArgs = append(helperArgs,
			"--capture", mode,
			"--executable", executable,
			"--account", account,
			"--realm", realm,
			"--agent", agent,
		)
		if location != "" {
			helperArgs = append(helperArgs, "--location", location)
		}
		if opts.WitselfHome != "" {
			helperArgs = append(helperArgs, "--witself-home", opts.WitselfHome)
		}
	}
	if ownership != nil {
		raw, marshalErr := json.Marshal(ownership)
		if marshalErr != nil {
			return managedHooksHelperResult{}, marshalErr
		}
		helperArgs = append(helperArgs, "--ownership-json", string(raw))
	}
	cmd := exec.Command(sudo, helperArgs...)
	cmd.Stdin = os.Stdin
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return managedHooksHelperResult{}, fmt.Errorf("administrator-managed hook %s failed: %w", action, err)
	}
	var result managedHooksHelperResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return managedHooksHelperResult{}, fmt.Errorf("parse administrator-managed hook result: %w", err)
	}
	return result, nil
}

func managedHooksOptions(runtimeName, mode, executable, account, realm, agent, location string) (transcriptcapture.ManagedHooksOptions, error) {
	opts, err := transcriptcapture.DefaultManagedHooksOptions(runtimeName, mode, executable, account, realm, agent, location)
	if err != nil {
		return transcriptcapture.ManagedHooksOptions{}, err
	}
	witselfHome, err := local.Home()
	if err != nil {
		return transcriptcapture.ManagedHooksOptions{}, fmt.Errorf("resolve WITSELF_HOME: %w", err)
	}
	if strings.TrimSpace(witselfHome) != "" {
		opts.WitselfHome, err = cleanCopilotAbsolutePath("WITSELF_HOME", witselfHome)
		if err != nil {
			return transcriptcapture.ManagedHooksOptions{}, err
		}
	}
	root := strings.TrimSpace(os.Getenv(managedHooksTestRootEnv))
	if root == "" {
		return opts, nil
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return transcriptcapture.ManagedHooksOptions{}, err
	}
	opts.CodexRequirementsPath = filepath.Join(root, "codex", "requirements.toml")
	opts.CodexManagedDir = filepath.Join(root, "codex", "witself-hooks")
	opts.ClaudeSettingsPath = filepath.Join(root, "claude-code", "managed-settings.d", "50-witself.json")
	opts.ClaudeManagedDir = filepath.Join(root, "claude-code", "witself-hooks")
	return opts, nil
}

func errorsWithAdminHint(err error) error {
	return fmt.Errorf("administrator-managed hooks require sudo: %w", err)
}
