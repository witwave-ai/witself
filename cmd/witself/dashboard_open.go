package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/witwave-ai/witself/internal/agenttui"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
)

// Opening is a deliberate action against an existing session. Resolve the
// caller's authority independently and prove both live contexts, just as TUI
// reuse does; status remains local and value-free for manager sessions.
func dashboardOpen(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("dashboard open", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	printURL := fs.Bool("print-url", false, "deliberately reveal the verified session opening URL on stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, dashboardOpenUsage)
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, consoleStartupTimeout)
	defer cancel()
	fail := func() int {
		fmt.Fprintln(os.Stderr, "witself: dashboard opening authority could not be verified")
		return 1
	}
	conn, err := accountConsoleConnect(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return fail()
	}
	self, err := client.GetSelf(ctx, conn.Endpoint, conn.Token, client.SelfOptions{})
	if err != nil || verifySelfCardConnection(conn, self.Identity) != nil {
		return fail()
	}
	manager := resolveAccountConsoleManager(ctx, conn, self.Identity, accountConsoleExplicit(fs), accountConsoleLocate)
	controller := newTUIConsole(ctx, conn, self.Identity, time.Second, accountConsoleOptions{Manager: manager})
	defer func() { _ = controller.Close() }()
	status, entry, err := controller.discover(ctx)
	if err != nil || status.State != agenttui.ConsoleRunning {
		return fail()
	}
	matched, err := dashboard.WithRegistryInstance(ctx, entry, func() error { return ctx.Err() })
	if err != nil || !matched {
		return fail()
	}
	if *printURL {
		if _, err := fmt.Fprintln(os.Stdout, entry.AccessURL); err != nil {
			fmt.Fprintln(os.Stderr, "witself: dashboard opening URL could not be written")
			return 1
		}
		return 0
	}
	if err := launchBrowser(entry.AccessURL); err != nil {
		fmt.Fprintln(os.Stderr, "witself dashboard: browser could not open; repeat this command with --print-url to deliberately reveal the opening URL")
		return 1
	}
	return 0
}
