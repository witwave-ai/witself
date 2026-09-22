package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"

	"github.com/witwave-ai/witself/internal/agenttui"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/version"
)

const agentTUIUsage = "usage: witself tui [--account NAME] [--realm NAME] [--agent NAME] [--endpoint URL --token-file FILE] [--theme NAME] [--poll DURATION] [--demo]"

var agentTUITerminal = func() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

var runAgentTUI = func(ctx context.Context, source agenttui.Source, opts agenttui.Options) error {
	model := agenttui.New(ctx, source, opts)
	defer model.Close()
	_, err := tea.NewProgram(model, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}

func tuiCmd(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return agentTUI(ctx, args)
}

// agentTUI resolves the same agent connection as the web console. Demo mode
// never resolves credentials, contacts a cell, or starts a local server.
func agentTUI(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	demo := fs.Bool("demo", false, "explore a synthetic agent workspace without an account or network")
	theme := fs.String("theme", "", "console, paper, midnight, amber, high-contrast, or auto (default: saved preference)")
	poll := fs.Duration("poll", 2*time.Second, "active-view refresh interval (1s to 1m)")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, agentTUIUsage); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || (*theme != "" && !agenttui.ValidTheme(*theme)) || *poll < time.Second || *poll > time.Minute {
		fmt.Fprintln(os.Stderr, agentTUIUsage)
		fmt.Fprintln(os.Stderr, "witself: use a supported theme and a poll interval between 1s and 1m")
		return 2
	}
	if *demo && (*account != "" || *realm != "" || *agent != "" || *endpoint != "" || *tokenFile != "") {
		fmt.Fprintln(os.Stderr, "witself: --demo cannot be combined with account or connection flags")
		return 2
	}
	// Check before touching credentials: a redirected invocation must not
	// accidentally authenticate or serialize a private dashboard into a log.
	if !agentTUITerminal() {
		fmt.Fprintln(os.Stderr, "witself: tui needs an interactive input and output terminal; use the ordinary --json commands for scripts")
		return 2
	}
	opts := agenttui.Options{Version: version.Version, Theme: *theme, PollInterval: *poll, Demo: *demo}
	var source agenttui.Source
	if *demo {
		opts.Agent, opts.Realm = "Atlas", "studio"
		source = agenttui.NewDemoSource()
	} else {
		startup, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		conn, err := connectAgent(startup, *account, *realm, *agent, *endpoint, *tokenFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "witself: could not resolve the agent connection; check your account, realm, agent, and connection flags")
			return 1
		}
		self, err := client.GetSelf(startup, conn.Endpoint, conn.Token, client.SelfOptions{Observational: true})
		if err != nil {
			fmt.Fprintln(os.Stderr, "witself: could not verify the agent identity; check the endpoint and agent authorization")
			return 1
		}
		if err := verifySelfCardConnection(conn, self.Identity); err != nil || self.Identity.AgentID == "" {
			fmt.Fprintln(os.Stderr, "witself: the authenticated identity does not match the selected agent connection")
			return 1
		}
		source, err = dashboard.NewReader(dashboard.Config{
			Endpoint: conn.Endpoint, BearerToken: conn.Token, Identity: self.Identity,
			Version: version.Version, PollInterval: *poll,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "witself: could not initialize the agent console reader")
			return 1
		}
		opts.Agent, opts.Realm = self.Identity.AgentName, self.Identity.RealmName
		// Explicit token-file connections do not retain the local alias in
		// connectAgent. Preserve the user's selected account for vault custody.
		if conn.AccountName == "" {
			conn.AccountName, _, _ = secretLocalSelectors(*account, self.Identity.RealmName, self.Identity.AgentName)
		}
		source = &tuiSecretSource{Source: source, connection: conn, identity: self.Identity}
	}
	clipboard := newTUIClipboard()
	defer clipboard.Close()
	if clipboard.write != nil {
		opts.Copy = clipboard.Copy
	}
	if err := runAgentTUI(ctx, source, opts); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "witself: the terminal workspace stopped unexpectedly")
		return 1
	}
	return 0
}
