package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const transcriptReleasePrivacyCaveat = "released turns were not sealed by the runtime; every tool result in them has been redacted to a placeholder; prompts and assistant messages are uploaded as captured"

func transcriptRelease(args []string) int {
	if len(args) == 1 && args[0] == transcriptReleaseUploaderCapabilityFlag {
		fmt.Fprintln(os.Stdout, transcriptReleaseUploaderCapability)
		return 0
	}
	fs := flag.NewFlagSet("transcript release", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtime := fs.String("runtime", "", "capture runtime")
	session := fs.String("session", "", "release residue for one local session id")
	all := fs.Bool("all", false, "release every eligible residue turn for the runtime")
	olderThan := fs.Duration("older-than", 24*time.Hour, "minimum age of the last queued event in a turn")
	dryRun := fs.Bool("dry-run", false, "list residue without changing it (default unless --yes; overrides --yes)")
	yes := fs.Bool("yes", false, "apply the release with tool result redaction")
	force := fs.Bool("force", false, "allow release of turns newer than --older-than")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sessionSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "session" {
			sessionSet = true
		}
	})
	*session = strings.TrimSpace(*session)
	apply := *yes && !*dryRun
	if fs.NArg() != 0 || *olderThan < 0 || (sessionSet && (*session == "" || *all)) || (apply && !sessionSet && !*all) {
		fmt.Fprintln(os.Stderr, "usage: witself transcript release --runtime RUNTIME [--session SESSION_ID | --all] [--older-than DURATION] [--dry-run] [--yes] [--force]")
		return 2
	}
	runtimeName, err := transcriptcapture.NormalizeRuntime(*runtime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself transcript release: %v\n", err)
		return 2
	}
	if !apply {
		if err := transcriptcapture.SweepOrphanSubmissions(runtimeName); err != nil {
			fmt.Fprintf(os.Stderr, "witself: clean orphaned capture submissions: %v\n", err)
		}
	}
	if apply {
		fmt.Fprintln(os.Stderr, transcriptReleasePrivacyCaveat)
	}
	residue, err := transcriptcapture.ListResidue(runtimeName, *session)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself transcript release: %v\n", err)
		return 1
	}
	table := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "SESSION\tQUEUED EVENTS\tFIRST EVENT (UTC)\tLAST EVENT (UTC)\tTOOL.RESULT PAYLOAD")
	for _, item := range residue {
		fmt.Fprintf(table, "%q\t%d\t%s\t%s\t%t\n", item.SessionID, item.EventCount,
			item.FirstEventAt.UTC().Format(time.RFC3339Nano), item.LastEventAt.UTC().Format(time.RFC3339Nano), item.HasToolResult)
	}
	if err := table.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "witself transcript release: print residue: %v\n", err)
		return 1
	}
	if !apply {
		return 0
	}
	if err := verifyTranscriptReleaseUploaders(); err != nil {
		fmt.Fprintf(os.Stderr, "witself transcript release: %v\n", err)
		return 1
	}
	released, err := transcriptcapture.ReleaseResidue(runtimeName, *session, *olderThan, *force)
	fmt.Fprintf(os.Stdout, "released %d turn(s)\n", released)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself transcript release: %v\n", err)
		return 1
	}
	return 0
}
