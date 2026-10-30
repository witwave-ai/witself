package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

type serviceFlags struct {
	root, pgbin, server, worker, jobs string
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
	cell, err := localcell.New(values.config())
	if err != nil {
		return err
	}
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
		adapter := "foreground"
		if action == "install" {
			adapter = "systemd-user"
		}
		descriptor = localcell.ServiceDescriptor{Adapter: adapter, Scope: "user", Runtime: "local", DesiredState: "running", ResourceProfile: "unconfigured", Payload: localcell.ServicePayload{Host: host, Args: values.hostArgs()}}
		if err := cell.PrepareService(ctx, descriptor); err != nil {
			return err
		}
		descriptor, err = cell.ServiceDescriptor()
		if err != nil {
			return err
		}
	}
	// Every lifecycle operation reopens the installation's immutable payload.
	// Caller flags cannot silently replace selected components during restart.
	if len(descriptor.Payload.Args) == 0 || descriptor.Payload.Args[0] != "run" {
		return errors.New("localcellharness: invalid service payload")
	}
	persisted, parseErr := parseServiceFlags(descriptor.Payload.Args[1:])
	if parseErr != nil {
		return errors.New("localcellharness: invalid service payload")
	}
	if (values.pgbin != "" && values.pgbin != persisted.pgbin) ||
		(values.server != "" && values.server != persisted.server) ||
		(values.worker != "" && values.worker != persisted.worker) ||
		(values.jobs != "" && values.jobs != persisted.jobs) {
		return errors.New("localcellharness: service payload change refused")
	}
	persisted.root = descriptor.Root
	cell, err = localcell.New(persisted.config())
	if err != nil {
		return err
	}
	if action == "run" {
		return cell.Run(ctx, localcell.SupervisorPolicy{})
	}
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
