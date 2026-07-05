package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/witwave-ai/witself/internal/version"
)

// dashboardCmd is `witself-admin dashboard` — the fleet operator's
// fullscreen cockpit (issue #29). One binary, two faces: the CLI
// subcommands are the substrate (what AI agents drive), and the
// dashboard is a bubbletea front-end that shells out to its own
// executable for every operation, so the two can never skew.
//
// v1 is the support pane: a live ticket list fed by
// `<self> ticket watch --json`, drill-down thread view, inline reply
// composer, and keyboard state transitions. Credentials resolve
// exactly as the CLI resolves them (managed ~/.witself/tokens dir,
// env vars) because the CLI subprocesses do the resolving.
//
// The dashboard also keeps itself fresh: release builds check GitHub
// for a newer version occasionally, upgrade through whatever channel
// installed them (brew, or a checksum-verified direct download), and
// re-exec straight back into the view the operator was in — reply
// drafts included (state rides the --resume flag).
func dashboardCmd(args []string) int {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	interval := fs.String("interval", "30s", "live-watch poll cadence")
	resumeEnc := fs.String("resume", "", "internal: view snapshot from a self-upgrade re-exec")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cli, err := newAdminCLI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Live watch is best-effort: if the stream can't start, the
	// dashboard still works on manual refresh.
	watch, err := cli.watch(ctx, *interval)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: live watch unavailable (%v); manual refresh only\n", err)
		watch = nil
	}

	m := newModel(ctx, cli, watch).withSelfUpgrade(cli.bin, version.Version)
	if *resumeEnc != "" {
		if r, err := decodeResumeState(*resumeEnc); err == nil {
			m = m.withResume(&r)
		}
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}

	// Self-upgrade re-exec: bubbletea has restored the terminal; the
	// new binary takes over this process and this tty, resuming the
	// exact view (compose drafts included). Exec only returns on error.
	if fm, ok := final.(model); ok && fm.relaunch != nil {
		cancel() // stop the watch subprocess before handing off
		argv := []string{fm.binPath, "dashboard",
			"--interval", *interval,
			"--resume", fm.relaunch.encode(),
		}
		if err := syscall.Exec(fm.binPath, argv, os.Environ()); err != nil {
			fmt.Fprintf(os.Stderr, "witself-admin: upgraded to %s but relaunch failed: %v\n  run `witself-admin dashboard` again by hand\n",
				fm.relaunch.UpgradedTo, err)
			return 1
		}
	}
	return 0
}
