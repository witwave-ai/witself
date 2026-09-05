package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func transcriptFence(args []string) int {
	fs := flag.NewFlagSet("transcript fence", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtime := fs.String("runtime", "", "capture runtime (codex)")
	session := fs.String("session", "", "local Codex session id")
	run := fs.String("run", "", "capture run id pinned when the delegated job starts")
	turn := fs.String("turn", "", "capture turn id pinned when the delegated job starts")
	reason := fs.String("reason", "job-completed", "reason for the synthetic terminal event")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *runtime != transcriptcapture.RuntimeCodex || strings.TrimSpace(*session) == "" || strings.TrimSpace(*run) == "" || strings.TrimSpace(*turn) == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself transcript fence --runtime codex --session <session_id> --run <run_id> --turn <turn_id> [--reason job-completed]")
		return 2
	}
	if _, _, err := transcriptcapture.EnqueueFence(*runtime, *session, *run, *turn, *reason); err != nil {
		fmt.Fprintf(os.Stderr, "witself transcript fence: %v\n", err)
		return 1
	}
	if os.Getenv("WITSELF_CAPTURE_NO_FLUSH") == "" {
		if err := startBackgroundFlush(*runtime); err != nil {
			fmt.Fprintf(os.Stderr, "witself capture: queued locally; background flush did not start: %v\n", err)
		}
	}
	return 0
}
