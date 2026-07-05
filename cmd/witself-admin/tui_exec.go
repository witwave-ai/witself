package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/witwave-ai/witself/internal/client"
)

// adminCLI shells out for every dashboard operation — to THIS binary
// (os.Executable()), running its own CLI subcommands in subprocesses.
// The dashboard therefore exercises the exact substrate AI agents
// drive, with zero version skew possible between the two: they are
// the same file. Types are imported from internal/client because
// that's exactly what the CLI marshals; the subprocess boundary is
// the contract, the structs just name it.
type adminCLI struct {
	bin string // path to this very executable
}

// newAdminCLI resolves the running binary for self-exec.
func newAdminCLI() (*adminCLI, error) {
	p, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve own executable: %w", err)
	}
	return &adminCLI{bin: p}, nil
}

// run executes one witself-admin invocation and unmarshals its stdout
// envelope into out. stderr becomes the error text on failure — the
// CLI already writes operator-grade messages there.
func (a *adminCLI) run(ctx context.Context, out any, stdin string, args ...string) error {
	cmd := exec.CommandContext(ctx, a.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", strings.TrimPrefix(msg, "witself-admin: "))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return fmt.Errorf("unexpected witself-admin output: %v", err)
	}
	return nil
}

// listTickets fetches the fleet-wide snapshot the TUI seeds from.
func (a *adminCLI) listTickets(ctx context.Context) (client.AdminTicketList, error) {
	var res client.AdminTicketList
	err := a.run(ctx, &res, "", "ticket", "list", "--json")
	return res, err
}

// showTicket fetches one thread for the detail pane.
func (a *adminCLI) showTicket(ctx context.Context, accountID, ticketID string) (client.GetSupportTicketResult, error) {
	var res client.GetSupportTicketResult
	err := a.run(ctx, &res, "",
		"ticket", "show", "--account", accountID, "--ticket", ticketID, "--json")
	return res, err
}

// reply posts an admin reply; body travels via stdin so multi-line
// content needs no shell quoting.
func (a *adminCLI) reply(ctx context.Context, accountID, ticketID, body string) error {
	var res struct {
		Message client.SupportTicketMessage `json:"message"`
	}
	return a.run(ctx, &res, body,
		"ticket", "reply", "--account", accountID, "--ticket", ticketID, "--stdin", "--json")
}

// setState transitions a ticket (resolved / closed / awaiting_admin …).
func (a *adminCLI) setState(ctx context.Context, accountID, ticketID, state string) error {
	var res struct {
		Ticket client.SupportTicket `json:"ticket"`
	}
	return a.run(ctx, &res, "",
		"ticket", "state", "--account", accountID, "--ticket", ticketID, "--state", state, "--json")
}

// watch starts the long-running ndjson stream and forwards each updated
// ticket on the returned channel. The subprocess dies with the context;
// stream errors close the channel (the TUI shows a "watch stopped"
// status and keeps working on manual refresh).
func (a *adminCLI) watch(ctx context.Context, interval string) (<-chan client.AdminTicket, error) {
	cmd := exec.CommandContext(ctx, a.bin,
		"ticket", "watch", "--json", "--interval", interval)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := make(chan client.AdminTicket, 16)
	go func() {
		defer close(ch)
		defer func() { _ = cmd.Wait() }()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var t client.AdminTicket
			if err := json.Unmarshal(sc.Bytes(), &t); err != nil {
				continue // stderr noise or partial line — skip
			}
			select {
			case ch <- t:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
