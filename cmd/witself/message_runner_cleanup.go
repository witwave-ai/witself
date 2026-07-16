package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/legacyrunnercleanup"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const messageRunnerRemovedUsage = "usage: witself message runner disable (--runtime RUNTIME|--all) [--force] [--json]"

var newLegacyRunnerCleaner = legacyrunnercleanup.Default

// retireLegacyMessageRunnersOnStartup is a one-time upgrade migration. It is
// called only by the real process entrypoint, never by run() in unit tests.
// That ensures replacing the CLI cannot leave a retired KeepAlive/Restart unit
// invoking a removed command indefinitely.
func retireLegacyMessageRunnersOnStartup() error {
	cleaner, err := newLegacyRunnerCleaner()
	if err != nil {
		return err
	}
	complete, err := cleaner.Completed()
	if err != nil || complete {
		return err
	}
	hasArtifacts, err := cleaner.HasArtifacts()
	if err != nil || !hasArtifacts {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cleaner.CleanupAll(ctx); err != nil {
		return err
	}
	return cleaner.MarkCompleted()
}

// legacyRunnerServeRuntime recognizes only the exact command previously placed
// in Witself-owned launchd and systemd definitions. It is not a public command;
// it is a tombstone that lets an already-running retired unit disable itself.
func legacyRunnerServeRuntime(args []string) (string, bool) {
	if len(args) != 5 || args[0] != "message" || args[1] != "runner" ||
		args[2] != "serve" || args[3] != "--runtime" {
		return "", false
	}
	runtimeName, err := transcriptcapture.NormalizeRuntime(args[4])
	return runtimeName, err == nil
}

func retireLegacyMessageRunnerServeTombstone(runtimeName string) error {
	cleaner, err := newLegacyRunnerCleaner()
	if err != nil {
		return err
	}
	cleaner.AllowLoadedWithoutDefinition = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = cleaner.Cleanup(ctx, runtimeName)
	return err
}

// messageRunnerCmd is intentionally only a migration-safe cleanup surface. The
// autonomous runner, provider adapter, status, notification ledger, and service
// entrypoint have been removed.
func messageRunnerCmd(args []string) int {
	if len(args) == 0 || args[0] != "disable" {
		fmt.Fprintln(os.Stderr, "witself: the autonomous message runner has been removed; only legacy cleanup is available")
		fmt.Fprintln(os.Stderr, messageRunnerRemovedUsage)
		return 2
	}
	fs := flag.NewFlagSet("message runner disable", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtimeFlag := fs.String("runtime", "", "retired installed runtime binding")
	all := fs.Bool("all", false, "remove legacy runner services and state for every former runtime")
	force := fs.Bool("force", false, "discard pending legacy local notification pointers after retiring services")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 || (*all == (strings.TrimSpace(*runtimeFlag) != "")) {
		fmt.Fprintln(os.Stderr, messageRunnerRemovedUsage)
		return 2
	}
	cleaner, err := newLegacyRunnerCleaner()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize legacy message runner cleanup: %v\n", err)
		return 1
	}
	cleaner.Force = *force
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var results []legacyrunnercleanup.Result
	if *all {
		results, err = cleaner.CleanupAll(ctx)
	} else {
		runtimeName, normalizeErr := transcriptcapture.NormalizeRuntime(*runtimeFlag)
		if normalizeErr != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", normalizeErr)
			return 2
		}
		var result legacyrunnercleanup.Result
		result, err = cleaner.Cleanup(ctx, runtimeName)
		results = []legacyrunnercleanup.Result{result}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: remove legacy message runner: %v\n", err)
		return 1
	}
	if *all {
		if err := cleaner.MarkCompleted(); err != nil {
			fmt.Fprintf(os.Stderr, "witself: record legacy message runner cleanup: %v\n", err)
			return 1
		}
	}
	if *jsonOut {
		return printJSON(map[string]any{
			"removed": true, "message": "autonomous message runner removed; legacy local services and state purged",
			"runtimes": results,
		})
	}
	for _, result := range results {
		fmt.Printf("removed\tlegacy-message-runner\truntime=%s\tservice=%t\tstate-purged=%t\n",
			result.Runtime, result.ServiceRemoved, result.StatePurged)
	}
	return 0
}
