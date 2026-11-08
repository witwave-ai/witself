package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

type serviceFlags struct {
	root, pgbin, server, worker, jobs string
	adapter                           string
	timeout                           time.Duration
}

func parseServiceFlags(args []string) (serviceFlags, error) {
	var values serviceFlags
	flags := flag.NewFlagSet("localcellharness service", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&values.root, "root", "", "absolute private cell directory")
	flags.StringVar(&values.pgbin, "pgbin", "", "absolute PostgreSQL binary directory")
	flags.StringVar(&values.server, "server", "", "absolute server executable")
	flags.StringVar(&values.worker, "worker", "", "absolute worker executable")
	flags.StringVar(&values.jobs, "jobs", "", "comma-separated local worker jobs")
	flags.StringVar(&values.adapter, "adapter", "", "explicit native adapter selection")
	flags.DurationVar(&values.timeout, "timeout", 45*time.Second, "bounded lifecycle operation timeout")
	if flags.Parse(args) != nil || flags.NArg() != 0 || !filepath.IsAbs(values.root) || values.timeout <= 0 {
		return values, errors.New("localcellharness: invalid service arguments")
	}
	return values, nil
}

func (v serviceFlags) config() localcell.Config {
	config := localcell.Config{Root: v.root, Binaries: localcell.Binaries{PostgresDir: v.pgbin, Server: v.server, Worker: v.worker}, Timeout: v.timeout}
	if v.jobs != "" {
		config.Jobs = strings.Split(v.jobs, ",")
	}
	return config
}

func (v serviceFlags) hostArgs() []string {
	args := []string{"run", "--root", v.root, "--pgbin", v.pgbin, "--server", v.server, "--worker", v.worker, "--timeout", v.timeout.String()}
	if v.jobs != "" {
		args = append(args, "--jobs", v.jobs)
	}
	return args
}

func runServiceCommand(ctx context.Context, args []string, out io.Writer) error {
	return runServiceCommandForGOOS(ctx, args, out, runtime.GOOS)
}

func serviceAdapterKind(goos string) (string, error) {
	switch goos {
	case "linux":
		return "systemd-user", nil
	case "darwin":
		return "launchd-agent", nil
	default:
		return "", fmt.Errorf("%w: adapter:platform: unsupported", localcell.ErrServiceManagerUnavailable)
	}
}

func runServiceCommandForGOOS(ctx context.Context, args []string, out io.Writer, goos string) (result error) {
	if os.Getenv("WITSELF_RUNTIME") != "local" {
		return errors.New("localcellharness: service commands require explicit local runtime selection")
	}
	action, flags := args[0], args[1:]
	if action == "service" {
		if len(flags) == 0 {
			return errors.New("localcellharness: service action is required")
		}
		action, flags = flags[0], flags[1:]
		if action != "install" && action != "observe" && action != "status" && action != "remove" && action != "start" && action != "stop" && action != "health" && action != "repair" {
			return errors.New("localcellharness: invalid service action")
		}
	}
	values, err := parseServiceFlags(flags)
	if err != nil {
		return err
	}
	if values.adapter != "" && (action != "install" || values.adapter != "windows-logon-task" || goos != "windows") {
		return errors.New("localcellharness: unsupported explicit service adapter")
	}
	selectedAdapter := "foreground"
	if action == "install" {
		if values.adapter != "" {
			selectedAdapter = values.adapter
		} else {
			selectedAdapter, err = serviceAdapterKind(goos)
			if err != nil {
				return err
			}
		}
	}
	site := "host_config"
	defer func() {
		if action != "install" && action != "remove" {
			localcell.WriteServiceHostFailure(os.Stderr, result, site)
		}
	}()
	cell, err := localcell.New(values.config())
	if err != nil {
		return err
	}
	site = "host_descriptor"
	descriptor, descriptorErr := cell.ServiceDescriptor()
	if descriptorErr != nil {
		if !errors.Is(descriptorErr, os.ErrNotExist) || (action != "run" && action != "install") {
			return descriptorErr
		}
		if values.pgbin == "" || values.server == "" || values.worker == "" {
			return errors.New("localcellharness: explicit component paths are required for service preparation")
		}
		host, hostErr := os.Executable()
		if hostErr != nil {
			return errors.New("localcellharness: cannot resolve service host")
		}
		host, hostErr = filepath.EvalSymlinks(host)
		if hostErr != nil || !filepath.IsAbs(host) {
			return errors.New("localcellharness: cannot resolve service host")
		}
		descriptor = localcell.ServiceDescriptor{Adapter: selectedAdapter, Scope: "user", Runtime: "local", DesiredState: "running", ResourceProfile: "unconfigured", Payload: localcell.ServicePayload{Host: host, Args: values.hostArgs()}}
		site = "host_prepare"
		if err := cell.PrepareService(ctx, descriptor); err != nil {
			return err
		}
		descriptor, err = cell.ServiceDescriptor()
		if err != nil {
			return err
		}
	}
	if action == "install" && descriptor.Adapter != selectedAdapter {
		return localcell.ErrServiceOwnershipConflict
	}
	// Every lifecycle operation reopens the installation's immutable payload.
	// Caller flags cannot silently replace selected components during restart.
	if len(descriptor.Payload.Args) == 0 || descriptor.Payload.Args[0] != "run" {
		return errors.New("localcellharness: invalid service payload")
	}
	persisted, parseErr := parseServiceFlags(descriptor.Payload.Args[1:])
	if parseErr != nil || persisted.adapter != "" || persisted.root != descriptor.Root {
		return errors.New("localcellharness: invalid service payload")
	}
	if (values.pgbin != "" && values.pgbin != persisted.pgbin) ||
		(values.server != "" && values.server != persisted.server) ||
		(values.worker != "" && values.worker != persisted.worker) ||
		(values.jobs != "" && values.jobs != persisted.jobs) {
		return errors.New("localcellharness: service payload change refused")
	}
	persisted.root = descriptor.Root
	site = "host_persisted_config"
	cell, err = localcell.New(persisted.config())
	if err != nil {
		return err
	}
	if action == "run" {
		site = "control_transport"
		return cell.Run(ctx, localcell.SupervisorPolicy{})
	}
	site = "control_transport"
	adapter := localcell.NewServiceAdapter()
	switch action {
	case "install":
		plan, planErr := adapter.Plan(ctx, descriptor)
		if planErr != nil {
			return planErr
		}
		err = adapter.Apply(ctx, plan)
	case "remove":
		// The durable stop fence precedes unregistering, so an interrupted
		// manager operation cannot leave an automatically restarting cell.
		if descriptor.Adapter == "foreground" {
			err = cell.Stop(ctx)
		} else if err = cell.SetServiceDesiredState(ctx, "stopped"); err == nil {
			err = adapter.Remove(ctx, descriptor)
		}
	case "repair":
		err = cell.RepairService(ctx)
	default:
		status, controlErr := cell.Control(ctx, action)
		if controlErr != nil {
			return controlErr
		}
		return json.NewEncoder(out).Encode(status)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Operation string `json:"operation"`
		State     string `json:"state"`
	}{"service " + action, "done"})
}
