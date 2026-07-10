package main

// Dashboard hot key: `a` on a cell in `error` state suspends the TUI
// and runs the cloud's login command in the foreground so the browser
// flow works normally (bubbletea's altscreen would otherwise fight
// stdin and paint the login banner over the frame). On return the
// dashboard refreshes and — if the login succeeded — the pin check
// flips from ✗ to ✓ without a manual `g`.

import (
	"context"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

// authCommand builds the interactive login command for a cell's
// cloud, or nil when we don't know how to auth this one. The command
// runs foreground (stdin/stdout/stderr → the operator's terminal), so
// browser prompts, "press enter to continue", and TOTP challenges
// work exactly like they would from a shell.
func authCommand(ctx context.Context, st cellState) (*exec.Cmd, string) {
	cloud := ""
	if st.entry.Cloud != nil {
		cloud = *st.entry.Cloud
	}
	sc := st.entry.SecurityContext
	switch cloud {
	case "aws":
		profile := ""
		if sc != nil && sc.AWS != nil && sc.AWS.Profile != nil {
			profile = *sc.AWS.Profile
		}
		if profile == "" {
			return nil, "no aws profile pinned — set security_context.aws.profile"
		}
		return exec.CommandContext(ctx, "aws", "sso", "login", "--profile", profile),
			"aws sso login --profile " + profile
	case "gcp":
		project := ""
		if sc != nil && sc.GCP != nil && sc.GCP.Project != nil {
			project = *sc.GCP.Project
		}
		args := []string{"auth", "application-default", "login"}
		if project != "" {
			args = append(args, "--project", project)
		}
		return exec.CommandContext(ctx, "gcloud", args...),
			"gcloud auth application-default login"
	case "azure":
		tenant := ""
		if sc != nil && sc.Azure != nil && sc.Azure.Tenant != nil {
			tenant = *sc.Azure.Tenant
		}
		args := []string{"login"}
		if tenant != "" {
			args = append(args, "--tenant", tenant)
		}
		return exec.CommandContext(ctx, "az", args...), "az login"
	}
	return nil, "unknown cloud: " + cloud
}

// runAuthCmd wires stdin/stdout/stderr for the login subprocess so
// the browser flow can print URLs and prompt normally.
func runAuthCmd(cmd *exec.Cmd) *exec.Cmd {
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

// authCompletedMsg lands in Update after tea.ExecProcess returns —
// carries the shell exit error, if any, so the status line can report
// success or failure without silently swallowing a bad login.
type authCompletedMsg struct {
	cell string
	desc string
	err  error
}

// startAuth is the `a`-key handler. Available on ANY selected cell
// while no op is running — not just error-state cells. Re-running a
// login flow is harmless, and gating on st.err missed real cases:
// ADC can expire between the last refresh and the next op, and an
// operator who knows a login is stale shouldn't have to wait for the
// dashboard to notice before they're allowed to fix it.
func (m dashboardModel) startAuth() (dashboardModel, tea.Cmd) {
	if m.op != nil {
		// ExecProcess suspends the TUI; the running op's output would
		// paint over the login banner and fight for the terminal.
		m.status = "an op is running — wait for it to finish before running auth"
		return m, nil
	}
	stp := m.selectedState()
	if stp == nil {
		if m.currentRow().kind == rowHeader {
			m.status = "select a cell (j/k) — auth applies to cells, not control planes"
		} else {
			m.status = "no cell selected"
		}
		return m, nil
	}
	st := *stp
	cmd, desc := authCommand(m.ctx, st)
	if cmd == nil {
		m.status = desc // already reads as an error explanation
		return m, nil
	}
	cell := st.name
	m.status = "running: " + desc + "…"
	return m, tea.ExecProcess(runAuthCmd(cmd), func(err error) tea.Msg {
		return authCompletedMsg{cell: cell, desc: desc, err: err}
	})
}
