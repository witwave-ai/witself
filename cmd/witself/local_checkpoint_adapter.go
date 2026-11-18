package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/witwave-ai/witself/internal/localcell"
)

// The adapter connects the checkpoint handler to the local cell API; run's
// "local" case reaches it only through localCheckpointNamespace.
//
// Trust boundary: the callbacks are adapter-owned and cooperative. They always
// pass the handler's context, never panic on any local cell result, and reach
// only Inspect, Checkpoint and DeleteCheckpoint of a Cell that
// localCheckpointOpen constructs once per callback invocation, after the
// cold-v1 guard. --timeout budgets that context; Inspect and DeleteCheckpoint
// ignore it, so it is no wall-clock bound, and an absolute binary path proves
// no authenticity. A first signal cancels the context; a killed process leaves
// its operation open for the next admitted create or delete to close; a torn
// journal tail still needs Repair, which this command never runs. A stdout
// failure mid-receipt can leave a truncated line, after which the handler
// prints only its fixed stderr line.
type localCheckpointMode string

// localCheckpointColdV1 is the only mode that constructs a Cell.
const localCheckpointColdV1 localCheckpointMode = "cold-v1"

// localCheckpointCell is the whole local cell surface the adapter can reach.
type localCheckpointCell interface {
	Inspect(context.Context) (localcell.Inspection, error)
	Checkpoint(context.Context, localcell.CheckpointOptions) error
	DeleteCheckpoint(context.Context, localcell.DeleteCheckpointOptions) error
}

// localCheckpointOpen refuses before construction unless the mode is cold-v1
// and neither path holds NUL; argv cannot, so the bound protects direct callers.
func localCheckpointOpen(mode localCheckpointMode, r localCheckpointRequest) (localCheckpointCell, error) {
	if mode != localCheckpointColdV1 || strings.ContainsRune(r.root, 0) || strings.ContainsRune(r.pgbin, 0) {
		return nil, errors.New("local checkpoint construction refused")
	}
	cell, err := localcell.New(localcell.Config{Root: r.root, Binaries: localcell.Binaries{PostgresDir: r.pgbin}, Timeout: r.timeout})
	if err != nil {
		return nil, err
	}
	return cell, nil
}

// localCheckpointFailed keeps the error for the handler's success test and
// takes class, reason and cleanup only from the closed classifier.
func localCheckpointFailed(err error) localCheckpointFailure {
	failure := localcell.ClassifyCheckpointFailure(err)
	return localCheckpointFailure{err, failure.Class, failure.Reason, failure.CleanupIncomplete}
}

// localCheckpointCallbacksFor returns three non-nil callbacks. Each invocation
// opens one Cell with mode and makes one API call. Summaries map 1:1, in
// order and without a maximum; a repeated identifier fails the read closed.
func localCheckpointCallbacksFor(mode localCheckpointMode, open func(localCheckpointMode, localCheckpointRequest) (localCheckpointCell, error)) localCheckpointCallbacks {
	read := func(ctx context.Context, r localCheckpointRequest) (localCheckpointObservation, localCheckpointFailure) {
		cell, err := open(mode, r)
		if err != nil {
			return localCheckpointObservation{}, localCheckpointFailed(err)
		}
		inspection, err := cell.Inspect(ctx)
		o, seen := localCheckpointObservation{status: inspection.CheckpointListing, unknown: inspection.CheckpointUnknown}, map[string]bool{}
		if inspection.Manifest != nil {
			o.installationID, o.installationPresent = inspection.Manifest.InstallationID, true
		}
		for _, s := range inspection.Checkpoints {
			if seen[s.ID] && err == nil {
				err = errors.New("local checkpoint listing repeats an identifier")
			}
			seen[s.ID] = true
			o.entries = append(o.entries, localCheckpointSummary{s.ID, s.InstallationID, s.CreatedAt, s.Bytes, s.Complete})
		}
		if err != nil {
			return localCheckpointObservation{}, localCheckpointFailed(err)
		}
		return o, localCheckpointFailure{}
	}
	create := func(ctx context.Context, r localCheckpointRequest) localCheckpointFailure {
		cell, err := open(mode, r)
		if err == nil {
			err = cell.Checkpoint(ctx, localcell.CheckpointOptions{InstallationID: r.installationID})
		}
		return localCheckpointFailed(err)
	}
	remove := func(ctx context.Context, r localCheckpointRequest) localCheckpointFailure {
		cell, err := open(mode, r)
		if err == nil {
			err = cell.DeleteCheckpoint(ctx, localcell.DeleteCheckpointOptions{CheckpointID: r.checkpointID})
		}
		return localCheckpointFailed(err)
	}
	return localCheckpointCallbacks{create, read, remove}
}

// localCheckpointRun selects cold-v1 only when the handler's own parser admits
// the request and never parses flags itself; the handler decides the rest.
func localCheckpointRun(ctx context.Context, args []string, open func(localCheckpointMode, localCheckpointRequest) (localCheckpointCell, error), stdout, stderr io.Writer) int {
	var mode localCheckpointMode
	if _, refusal := localCheckpointParse(args); refusal == "" {
		mode = localCheckpointColdV1
	}
	return localCheckpointCommand(ctx, args, localCheckpointCallbacksFor(mode, open), stdout, stderr)
}

// localCheckpointSignals makes the command's context; localCheckpointNamespace
// alone calls it. The first SIGINT, SIGTERM or SIGHUP cancels it (on Windows,
// Ctrl-C and Ctrl-Break arrive as os.Interrupt; closing the console, logoff
// and shutdown as SIGTERM). AfterFunc then runs stop, which restores each
// signal's handling before the call, so a further signal acts as before; one
// that arrives before stop has finished is absorbed (a window of time).
func localCheckpointSignals() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}

// localCheckpointNamespace is the released route: anything but the checkpoint
// word is the fixed usage line, exit 2, before any signal is caught; otherwise
// the signal context is made before the handler parses and stopped on return.
func localCheckpointNamespace(args []string) int {
	if len(args) == 0 || args[0] != "checkpoint" {
		_, _ = io.WriteString(os.Stderr, "usage: witself local checkpoint create|inspect|delete --checkpoint-mode=cold-v1 --root=ABSOLUTE_PATH\n")
		return 2
	}
	ctx, stop := localCheckpointSignals()
	defer stop()
	return localCheckpointRun(ctx, args[1:], localCheckpointOpen, os.Stdout, os.Stderr)
}
