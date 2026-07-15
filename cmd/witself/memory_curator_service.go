package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/memorycurator"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const memoryCurateAutoServiceUsage = "usage: witself memory curate auto service install|status|start|uninstall --runtime RUNTIME"

type automaticCuratorServiceLifecycle interface {
	Install(context.Context, string) (memorycurator.CuratorServiceStatus, error)
	Status(context.Context, string) (memorycurator.CuratorServiceStatus, error)
	Start(context.Context, string) (memorycurator.CuratorServiceStatus, error)
	Uninstall(context.Context, string) (memorycurator.CuratorServiceStatus, error)
}

var automaticCuratorServiceFactory = func(executable string) (automaticCuratorServiceLifecycle, error) {
	manager, err := memorycurator.DefaultCuratorServiceManager(executable)
	if err != nil {
		return nil, err
	}
	return manager, nil
}

func memoryCurateAutoService(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, memoryCurateAutoServiceUsage)
		return 2
	}
	command := strings.TrimSpace(args[0])
	switch command {
	case "install", "status", "start", "uninstall":
	default:
		fmt.Fprintf(os.Stderr, "witself memory curate auto service: unknown subcommand %q\n", args[0])
		return 2
	}
	parsed, code := parseMemoryCurateAutoRuntime("service "+command, args[1:], false)
	if code != 0 {
		return code
	}
	if command == "install" || command == "start" {
		if err := validateAutomaticCuratorServiceBinding(parsed.Runtime); err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
	}
	executable, err := currentExecutablePath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: resolve persistent executable: %v\n", err)
		return 1
	}
	lifecycle, err := automaticCuratorServiceFactory(executable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize automatic curator service: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var status memorycurator.CuratorServiceStatus
	switch command {
	case "install":
		status, err = lifecycle.Install(ctx, parsed.Runtime)
	case "status":
		status, err = lifecycle.Status(ctx, parsed.Runtime)
	case "start":
		status, err = lifecycle.Start(ctx, parsed.Runtime)
	case "uninstall":
		status, err = lifecycle.Uninstall(ctx, parsed.Runtime)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: automatic curator service %s: %v\n", command, err)
		return 1
	}
	if parsed.JSON {
		return printJSON(status)
	}
	state := "uninstalled"
	if status.Installed {
		state = "installed"
	}
	fmt.Printf("%s\tplatform=%s\truntime=%s\tenabled=%t\tactive=%t\n",
		state, status.Platform, status.Runtime, status.Enabled, status.Active)
	return 0
}

func validateAutomaticCuratorServiceBinding(runtimeName string) error {
	binding, err := transcriptcapture.LoadConfig(runtimeName)
	if err != nil {
		return fmt.Errorf("load %s integration: %w", runtimeName, err)
	}
	store, err := memorycurator.DefaultAutoStore(binding.AgentID)
	if err != nil {
		return fmt.Errorf("initialize automatic curator: %w", err)
	}
	inspection, err := store.Inspect()
	if err != nil {
		return fmt.Errorf("inspect automatic curator: %w", err)
	}
	if !inspection.Configured || !inspection.Config.Enabled {
		return errors.New("enable automatic curation for this runtime before installing or starting its service")
	}
	if inspection.Config.AccountID != binding.AccountID || inspection.Config.RealmID != binding.RealmID ||
		inspection.Config.AgentID != binding.AgentID {
		return errors.New("automatic curator configuration does not match the installed runtime binding")
	}
	return nil
}
