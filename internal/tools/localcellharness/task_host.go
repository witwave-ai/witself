package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/witwave-ai/witself/internal/localcell"
)

type taskHostFlags struct{ runtime, root, generation string }

type taskHostCommandError struct{ cause error }

func (*taskHostCommandError) Error() string {
	return "service_task_host_failed: step=task_host:entry code=0 reason=entry_failed"
}
func (e *taskHostCommandError) Unwrap() error { return e.cause }

func parseTaskHostFlags(args []string) (taskHostFlags, error) {
	var values taskHostFlags
	flags := flag.NewFlagSet("task-host", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&values.runtime, "runtime", "", "explicit local runtime")
	flags.StringVar(&values.root, "root", "", "existing private installation")
	flags.StringVar(&values.generation, "generation", "", "exact prepared generation")
	if flags.Parse(args) != nil || flags.NArg() != 0 || values.runtime != "local" ||
		!filepath.IsAbs(values.root) || filepath.Clean(values.root) != values.root ||
		values.generation == "" || strings.ContainsAny(values.generation, "\x00\r\n") {
		return values, errors.New("localcellharness: invalid task host arguments")
	}
	return values, nil
}

func runTaskHost(ctx context.Context, args []string) error {
	err := runTaskHostWith(ctx, args, func(cell *localcell.Cell, generation string) (localcell.ServiceDescriptor, error) {
		return cell.ValidateTaskHost(generation)
	}, func(cell *localcell.Cell) error {
		return cell.Run(ctx, localcell.SupervisorPolicy{})
	})
	if err != nil {
		// main prints command errors. Filesystem and descriptor failures may
		// contain private paths, so no underlying message crosses this boundary.
		return &taskHostCommandError{cause: err}
	}
	return nil
}

// The narrow injection points prove runtime-selection ordering without launching
// a host or registering a task in the hermetic harness tests.
func runTaskHostWith(ctx context.Context, args []string,
	validate func(*localcell.Cell, string) (localcell.ServiceDescriptor, error),
	host func(*localcell.Cell) error,
) error {
	values, err := parseTaskHostFlags(args)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cell, err := localcell.New(localcell.Config{Root: values.root})
	if err != nil {
		return err
	}
	descriptor, err := validate(cell, values.generation)
	if err != nil {
		return err
	}
	if len(descriptor.Payload.Args) == 0 || descriptor.Payload.Args[0] != "run" {
		return errors.New("localcellharness: invalid task host payload")
	}
	persisted, err := parseServiceFlags(descriptor.Payload.Args[1:])
	if err != nil || !sameInstallationRoot(persisted.root, descriptor.Root) || persisted.adapter != "" ||
		persisted.pgbin == "" || persisted.server == "" || persisted.worker == "" {
		return errors.New("localcellharness: invalid task host payload")
	}
	cell, err = localcell.New(persisted.config())
	if err != nil {
		return err
	}
	// Task Scheduler does not supply WITSELF_RUNTIME. The explicit command flag
	// can select it only after the persisted installation and executable checks.
	if err := os.Setenv("WITSELF_RUNTIME", "local"); err != nil {
		return errors.New("localcellharness: cannot select task host runtime")
	}
	return host(cell)
}
