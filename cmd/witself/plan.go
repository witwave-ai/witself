package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
)

// planCmd dispatches `witself plan <verb>`: status, upgrade, downgrade, cancel.
// Talks to the CONTROL PLANE (plans are Cloud-side). Self-hosted deployments
// have no CP — the CLI errors gracefully then.
func planCmd(args []string) int {
	if len(args) == 0 {
		return planStatus(nil)
	}
	switch args[0] {
	case "status":
		return planStatus(args[1:])
	case "upgrade":
		return planChangeCLI("upgrade", args[1:])
	case "downgrade":
		return planChangeCLI("downgrade", args[1:])
	case "cancel":
		return planCancelCLI(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself plan: unknown subcommand %q\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: witself plan status|upgrade|downgrade|cancel [--account NAME]")
		return 2
	}
}

// planContext resolves the account name to (accountID, operatorToken, cpURL).
// The token is the same operator token used for cell verbs — the CP
// introspects it against the account's cell to authorize.
func planContext(accountName string) (accountID, token, controlPlane string, err error) {
	_, acct, tok, err := local.Resolve(accountName)
	if err != nil {
		return "", "", "", err
	}
	cp := defaultControlPlane
	if v := strings.TrimSpace(os.Getenv("WITSELF_CONTROL_PLANE")); v != "" {
		cp = v
	}
	return acct.ID, tok, cp, nil
}

func planStatus(args []string) int {
	fs := flag.NewFlagSet("plan status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	acctID, tok, cp, err := planContext(*account)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	status, err := client.GetPlan(context.Background(), cp, acctID, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	printPlanStatus(status)
	return 0
}

func planChangeCLI(verb string, args []string) int {
	fs := flag.NewFlagSet("plan "+verb, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	email := fs.String("email", "", "billing email (used on first purchase)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	target := fs.Arg(0)
	if target == "" {
		fmt.Fprintf(os.Stderr, "usage: witself plan %s [--account NAME] [--email E] TARGET_PLAN\n", verb)
		return 2
	}
	acctID, tok, cp, err := planContext(*account)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	ctx := context.Background()
	var out client.PlanOutcome
	switch verb {
	case "upgrade":
		out, err = client.UpgradePlan(ctx, cp, acctID, tok, target, *email)
	case "downgrade":
		out, err = client.DowngradePlan(ctx, cp, acctID, tok, target, *email)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	printPlanOutcome(out)
	return 0
}

func planCancelCLI(args []string) int {
	fs := flag.NewFlagSet("plan cancel", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	acctID, tok, cp, err := planContext(*account)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if err := client.CancelPlanChange(context.Background(), cp, acctID, tok); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Println("cancelled")
	return 0
}

// printPlanStatus renders the record — designed to say the truth: current
// plan, what's applied, and anything pending (a URL to resume, an effective
// date, an apply-blocked reason).
func printPlanStatus(s client.PlanStatus) {
	fmt.Printf("plan:     %s\n", s.Plan)
	if s.Applied != s.Plan {
		fmt.Printf("applied:  %s   (converging — retry or wait)\n", s.Applied)
	}
	if s.ApplyBlocked != "" {
		fmt.Printf("blocked:  %s\n", s.ApplyBlocked)
	}
	if s.PastDueSince != nil {
		fmt.Printf("past-due: since %s\n", s.PastDueSince.Format(time.RFC3339))
	}
	if p := s.Pending; p != nil {
		fmt.Println()
		switch p.Kind {
		case "upgrade":
			fmt.Printf("pending:  upgrade → %s (awaiting payment)\n", p.Plan)
			if p.URL != "" {
				fmt.Printf("  resume: %s\n", p.URL)
			}
			if p.Expires != nil {
				fmt.Printf("  expires: %s\n", p.Expires.Format(time.RFC3339))
			}
		case "downgrade":
			fmt.Printf("pending:  downgrade → %s (scheduled)\n", p.Plan)
			if p.Effective != nil {
				fmt.Printf("  effective: %s\n", p.Effective.Format(time.RFC3339))
			}
			fmt.Println("  cancel:  witself plan cancel")
		case "contact":
			fmt.Printf("pending:  interest in %s recorded — we'll be in touch\n", p.Plan)
		}
	}
}

// printPlanOutcome renders what a change resolved to. The one CLI path branches
// on Kind — the shape the `done | needs_action(url)` adapter guarantees.
func printPlanOutcome(out client.PlanOutcome) {
	switch out.Kind {
	case "done":
		fmt.Printf("upgraded to %s\n", out.Plan)
	case "action":
		fmt.Printf("complete your %s upgrade at:\n  %s\n", out.Plan, out.URL)
		fmt.Println("(this link expires; re-run to get a new one)")
	case "scheduled":
		fmt.Printf("downgrade to %s scheduled for %s\n", out.Plan, out.Effective.Format(time.RFC3339))
		fmt.Println("witself plan cancel to undo before then")
	case "contact":
		fmt.Printf("interest in %s recorded — we'll be in touch\n", out.Plan)
	default:
		fmt.Printf("%s (kind=%s)\n", out.Plan, out.Kind)
	}
}
