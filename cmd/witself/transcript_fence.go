package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const transcriptFenceUsage = "usage: witself transcript fence --runtime RUNTIME --session <session_id> " +
	"(--latest | --run <run_id> --turn <turn_id>) [--reason job-completed]"

func transcriptFence(args []string) int {
	fs := flag.NewFlagSet("transcript fence", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtime := fs.String("runtime", "", "capture runtime (codex|claude-code|grok-build|cursor|openclaw|antigravity|copilot)")
	session := fs.String("session", "", "local capture session id")
	run := fs.String("run", "", "capture run id pinned when the delegated job starts")
	turn := fs.String("turn", "", "capture turn id pinned when the delegated job starts")
	latest := fs.Bool("latest", false, "derive the bound run and every still-held turn from local capture state")
	reason := fs.String("reason", "job-completed", "reason for the synthetic terminal event")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	runtimeName, err := transcriptcapture.NormalizeRuntime(*runtime)
	pinned := strings.TrimSpace(*run) != "" && strings.TrimSpace(*turn) != ""
	partiallyPinned := !pinned && (strings.TrimSpace(*run) != "" || strings.TrimSpace(*turn) != "")
	if err != nil || strings.TrimSpace(*session) == "" || fs.NArg() != 0 ||
		partiallyPinned || *latest == pinned {
		fmt.Fprintln(os.Stderr, transcriptFenceUsage)
		return 2
	}
	if *latest {
		events, err := transcriptcapture.EnqueueLatestFence(runtimeName, *session, *reason)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself transcript fence: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "fenced %d held %s turn(s)\n", len(events), runtimeName)
	} else if _, _, err := transcriptcapture.EnqueueFence(runtimeName, *session, *run, *turn, *reason); err != nil {
		fmt.Fprintf(os.Stderr, "witself transcript fence: %v\n", err)
		return 1
	}
	if os.Getenv("WITSELF_CAPTURE_NO_FLUSH") == "" {
		if err := startBackgroundFlush(runtimeName); err != nil {
			fmt.Fprintf(os.Stderr, "witself capture: queued locally; background flush did not start: %v\n", err)
		}
	}
	return 0
}
