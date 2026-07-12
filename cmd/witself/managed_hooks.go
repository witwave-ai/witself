package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const managedHooksTestRootEnv = "WITSELF_TEST_MANAGED_HOOKS_ROOT"

func managedHooksCmd(args []string) int {
	if len(args) == 0 || (args[0] != "install" && args[0] != "remove") {
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
	if action == "install" {
		_, err = transcriptcapture.InstallManagedHooks(opts)
	} else {
		_, err = transcriptcapture.RemoveManagedHooks(opts)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %s managed hooks: %v\n", action, err)
		return 1
	}
	return 0
}

func installManagedRuntimeHooks(runtimeName, mode, executable, account, realm, agent, location string) (string, error) {
	return runManagedHooksWithElevation("install", runtimeName, mode, executable, account, realm, agent, location)
}

func removeManagedRuntimeHooks(runtimeName string) (string, error) {
	return runManagedHooksWithElevation("remove", runtimeName, transcriptcapture.ModeRaw, "", "", "", "", "")
}

func runManagedHooksWithElevation(action, runtimeName, mode, executable, account, realm, agent, location string) (string, error) {
	opts, err := managedHooksOptions(runtimeName, mode, executable, account, realm, agent, location)
	if err != nil {
		return "", err
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

func managedHooksOptions(runtimeName, mode, executable, account, realm, agent, location string) (transcriptcapture.ManagedHooksOptions, error) {
	opts, err := transcriptcapture.DefaultManagedHooksOptions(runtimeName, mode, executable, account, realm, agent, location)
	if err != nil {
		return transcriptcapture.ManagedHooksOptions{}, err
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
