// Command witself-admin is the Witself fleet-admin CLI. It always talks
// to the control plane (never a cell directly) so fleet-wide queries
// like "show me every open support ticket" fan out CP → all cells in
// parallel.
//
// Two credential surfaces live in this binary:
//
//   - Fleet token (WITSELF_FLEET_TOKEN / --fleet-token) authorizes the
//     admin registry: mint, list, revoke, delete admins. It is the same
//     shared secret used by every other fleet-level operation.
//
//   - Admin token (WITSELF_ADMIN_TOKEN / --token / --token-file)
//     authenticates a specific admin against the ticket routes. Minted
//     once by `witself-admin admin mint` and shown once.
//
// Split intentionally: an admin who lost their fleet-token access can
// still work tickets, and a compromised admin token doesn't grant the
// power to mint more.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/supportstates"
	"github.com/witwave-ai/witself/internal/version"
)

const defaultControlPlane = "https://self.witwave.ai"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stdout)
		return 0
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Println(version.String("witself-admin"))
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	case "whoami":
		return whoamiCmd(args[1:])
	case "admin":
		return adminCmd(args[1:])
	case "ticket":
		return ticketCmd(args[1:])
	case "account":
		return accountCmd(args[1:])
	case "cells":
		return cellsCmd(args[1:])
	case "events":
		return eventsCmd(args[1:])
	case "placement":
		return placementCmd(args[1:])
	case "dashboard", "tui":
		return dashboardCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself-admin: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func usage(w io.Writer) {
	usageLine(w, "witself-admin — the Witself fleet-admin CLI")
	usageLine(w)
	usageLine(w, "Usage:")
	usageLine(w, "  witself-admin whoami        Verify an admin token and print its identity")
	usageLine(w, "  witself-admin admin ...     Manage fleet-admin credentials (requires fleet token)")
	usageLine(w, "  witself-admin ticket ...    Read/reply/transition support tickets across the fleet")
	usageLine(w, "                                (list|watch|show|reply|state|resolve|close|states)")
	usageLine(w, "  witself-admin account ...   Read/set per-account fleet settings")
	usageLine(w, "                                (support-policy|transcript-retention|plan-override)")
	usageLine(w, "  witself-admin cells ...     Fleet cell registry with account counts (list)")
	usageLine(w, "  witself-admin events ...    Fleet-wide audit-event tail (list|watch)")
	usageLine(w, "  witself-admin placement ... Rescue archived accounts blocked by hard pins")
	usageLine(w, "                                (rescue)")
	usageLine(w, "  witself-admin dashboard     Fullscreen operator dashboard (cells · support ·")
	usageLine(w, "                                events; self-upgrades and resumes its view)")
	usageLine(w, "  witself-admin version       Print version information")
	usageLine(w)
	usageLine(w, "Tokens (managed dir first; env vars and flags override):")
	usageLine(w, "  admin: ~/.witself/tokens/admin.token, WITSELF_ADMIN_TOKEN, --token-file, --token")
	usageLine(w, "  fleet: ~/.witself/tokens/fleet.token, WITSELF_FLEET_TOKEN, --fleet-token")
	usageLine(w)
	usageLine(w, "Environment:")
	usageLine(w, "  WITSELF_CONTROL_PLANE   control-plane URL (default https://self.witwave.ai)")
	usageLine(w, "  WITSELF_HOME            managed dir root (default ~/.witself)")
}

func usageLine(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}

// cpEndpoint resolves the control-plane URL: --endpoint > env > default.
func cpEndpoint(flagValue string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("WITSELF_CONTROL_PLANE")); v != "" {
		return v
	}
	return defaultControlPlane
}

// managedTokenPath returns the token file's home in the managed
// directory — same WITSELF_HOME-overridable ~/.witself root the ws
// CLI uses, tokens live under tokens/. The zero-config daily-driver
// path: `witself-admin admin mint` defaults --out here, and every
// command reads it back without an env var or flag.
func managedTokenPath(name string) (string, error) {
	root := os.Getenv("WITSELF_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".witself")
	}
	return filepath.Join(root, "tokens", name), nil
}

// resolveAdminToken picks the admin token, in order: --token,
// --token-file, WITSELF_ADMIN_TOKEN, then the managed file
// (~/.witself/tokens/admin.token). Explicit sources override the
// managed fallback so scripts can pin a different credential.
func resolveAdminToken(tokenFlag, tokenFileFlag string) (string, error) {
	if t := strings.TrimSpace(tokenFlag); t != "" {
		return t, nil
	}
	if f := strings.TrimSpace(tokenFileFlag); f != "" {
		return readTokenFile(f)
	}
	if t := strings.TrimSpace(os.Getenv("WITSELF_ADMIN_TOKEN")); t != "" {
		return t, nil
	}
	if p, err := managedTokenPath("admin.token"); err == nil {
		if tok, err := readTokenFile(p); err == nil {
			return tok, nil
		}
	}
	return "", fmt.Errorf("no admin token — mint one with `witself-admin admin mint` (writes ~/.witself/tokens/admin.token), or set WITSELF_ADMIN_TOKEN / --token-file / --token")
}

// resolveFleetToken picks the fleet-level shared secret, in order:
// --fleet-token, WITSELF_FLEET_TOKEN, then the managed file
// (~/.witself/tokens/fleet.token).
func resolveFleetToken(tokenFlag string) (string, error) {
	if t := strings.TrimSpace(tokenFlag); t != "" {
		return t, nil
	}
	if t := strings.TrimSpace(os.Getenv("WITSELF_FLEET_TOKEN")); t != "" {
		return t, nil
	}
	if p, err := managedTokenPath("fleet.token"); err == nil {
		if tok, err := readTokenFile(p); err == nil {
			return tok, nil
		}
	}
	return "", fmt.Errorf("no fleet token — expected ~/.witself/tokens/fleet.token, WITSELF_FLEET_TOKEN, or --fleet-token TOKEN")
}

func readTokenFile(path string) (string, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file %q: %w", path, err)
	}
	tok := strings.TrimSpace(string(buf))
	if tok == "" {
		return "", fmt.Errorf("token file %q is empty", path)
	}
	return tok, nil
}

// jsonFlag adds --json to a FlagSet — same convention as the witself CLI.
func jsonFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("json", false, "emit output as JSON instead of a table")
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	return 0
}

// tableWriter mirrors the witself CLI helper: elastic alignment on a TTY,
// header routed to stderr on a pipe so scripts don't consume it as data.
func tableWriter(header string) (io.Writer, func()) {
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		tw := tabwriter.NewWriter(os.Stdout, 6, 4, 3, ' ', 0)
		_, _ = fmt.Fprintln(tw, header)
		return tw, func() { _ = tw.Flush() }
	}
	fmt.Fprintln(os.Stderr, header)
	return os.Stdout, func() {}
}

// safeText strips C0 control characters, DEL, and the C1 range
// (U+0080–U+009F), matching the witself CLI helper. C1 matters:
// U+009B is a single-rune CSI executed like ESC-[ by C1-honoring
// terminals. Applied to any operator- or admin-supplied string before
// it hits a terminal, so a hostile ticket body can't hijack the screen.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return r
		}
		if r < 0x20 || (r >= 0x7F && r <= 0x9F) {
			return -1
		}
		return r
	}, s)
}

// tabSafe replaces embedded \t and \n with spaces for single-line
// table columns.
func tabSafe(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// whoamiCmd: witself-admin whoami
func whoamiCmd(args []string) int {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	token := fs.String("token", "", "admin token (prefer --token-file / WITSELF_ADMIN_TOKEN)")
	tokenFile := fs.String("token-file", "", "file containing the admin token")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tok, err := resolveAdminToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	me, err := client.GetAdminWhoami(context.Background(), cpEndpoint(*endpoint), tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(whoamiJSONMap(me))
	}
	fmt.Printf("%s\t%s\n", me.AdminID, me.Handle)
	return 0
}

// adminCmd handles `witself-admin admin ...`.
func adminCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself-admin admin mint|list|revoke|delete ...")
		return 2
	}
	switch args[0] {
	case "mint":
		return adminMint(args[1:])
	case "list":
		return adminList(args[1:])
	case "revoke":
		return adminRevoke(args[1:])
	case "delete":
		return adminDelete(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself-admin admin: unknown subcommand %q\n", args[0])
		return 2
	}
}

func adminMint(args []string) int {
	fs := flag.NewFlagSet("admin mint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	fleetToken := fs.String("fleet-token", "", "fleet shared secret (prefer WITSELF_FLEET_TOKEN)")
	handle := fs.String("handle", "", "admin handle, e.g. sarah (required)")
	note := fs.String("note", "", "optional bookkeeping note (max 200 chars)")
	jsonOut := jsonFlag(fs)
	out := fs.String("out", "", "also write the raw admin token to this file (0600)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*handle) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself-admin admin mint --handle NAME [--note TEXT] [--out FILE]")
		return 2
	}
	ft, err := resolveFleetToken(*fleetToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	res, err := client.MintAdmin(context.Background(), cpEndpoint(*endpoint), ft, client.MintAdminInput{
		Handle: *handle,
		Note:   *note,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	// Default destination is the managed dir — the same place
	// resolveAdminToken reads back from, so mint-then-use needs no
	// flags or env vars at all. --out overrides for a second admin's
	// token you're handing to someone else.
	dest := strings.TrimSpace(*out)
	if dest == "" {
		if p, err := managedTokenPath("admin.token"); err == nil {
			if _, statErr := os.Stat(p); statErr == nil {
				// Never silently clobber an existing credential —
				// the old token would keep authenticating until
				// revoked, orphaned with no local copy.
				fmt.Fprintf(os.Stderr, "witself-admin: %s already exists — not overwriting; pass --out FILE for this token\n", p)
			} else {
				dest = p
			}
		}
	}
	if dest != "" {
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "witself-admin: could not create token dir: %v\n", err)
		} else if err := os.WriteFile(dest, []byte(res.AdminToken+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "witself-admin: wrote admin but could not save token file: %v\n", err)
			// Don't lose the token — fall through and print it too.
		} else {
			fmt.Fprintf(os.Stderr, "wrote admin token to %s (mode 0600)\n", dest)
		}
	}
	if *jsonOut {
		return printJSON(res)
	}
	// Fixed-format single-shot: this is the ONLY chance to capture the
	// raw token; make it obvious.
	fmt.Fprintln(os.Stderr, "SAVE THIS TOKEN NOW — it is not recoverable.")
	fmt.Println(res.AdminToken)
	fmt.Fprintf(os.Stderr, "admin_id=%s handle=%s\n", res.Admin.AdminID, res.Admin.Handle)
	return 0
}

func adminList(args []string) int {
	fs := flag.NewFlagSet("admin list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	fleetToken := fs.String("fleet-token", "", "fleet shared secret")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ft, err := resolveFleetToken(*fleetToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	admins, err := client.ListAdmins(context.Background(), cpEndpoint(*endpoint), ft)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"admins": admins})
	}
	if len(admins) == 0 {
		fmt.Fprintln(os.Stderr, "no admins minted")
		return 0
	}
	w, flush := tableWriter("admin_id\thandle\tcreated_at\tdisabled\tnote")
	for _, a := range admins {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\n",
			a.AdminID, a.Handle,
			a.CreatedAt.UTC().Format(time.RFC3339),
			a.Disabled, tabSafe(safeText(a.Note)))
	}
	flush()
	return 0
}

func adminRevoke(args []string) int {
	fs := flag.NewFlagSet("admin revoke", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	fleetToken := fs.String("fleet-token", "", "fleet shared secret")
	adminID := fs.String("id", "", "admin id to revoke (required)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*adminID) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself-admin admin revoke --id ADMIN_ID")
		return 2
	}
	ft, err := resolveFleetToken(*fleetToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	admin, err := client.RevokeAdmin(context.Background(), cpEndpoint(*endpoint), ft, *adminID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"admin": admin})
	}
	fmt.Printf("revoked %s (%s)\n", admin.AdminID, admin.Handle)
	return 0
}

func adminDelete(args []string) int {
	fs := flag.NewFlagSet("admin delete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	fleetToken := fs.String("fleet-token", "", "fleet shared secret")
	adminID := fs.String("id", "", "admin id to delete (required; must be revoked first)")
	yes := fs.Bool("yes", false, "confirm the destructive delete")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*adminID) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself-admin admin delete --id ADMIN_ID --yes")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "witself-admin: delete is destructive; re-run with --yes to confirm.")
		return 2
	}
	ft, err := resolveFleetToken(*fleetToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	if err := client.DeleteAdmin(context.Background(), cpEndpoint(*endpoint), ft, *adminID); err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	fmt.Printf("deleted %s\n", *adminID)
	return 0
}

// ticketCmd handles `witself-admin ticket ...`.
func ticketCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself-admin ticket list|watch|show|reply|state|resolve|close|states ...")
		return 2
	}
	switch args[0] {
	case "list":
		return ticketList(args[1:])
	case "watch":
		return ticketWatch(args[1:])
	case "show":
		return ticketShow(args[1:])
	case "reply":
		return ticketReply(args[1:])
	case "state":
		return ticketState(args[1:], "")
	case "resolve":
		return ticketState(args[1:], "resolved")
	case "close":
		return ticketState(args[1:], "closed")
	case "states":
		return ticketStates(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself-admin ticket: unknown subcommand %q\n", args[0])
		return 2
	}
}

// ticketStates renders the support-ticket state graph. Two audiences:
// a human running `witself-admin ticket states` on a terminal (table
// output), and a TUI or AI agent piping `--json` into a state machine
// or decision policy. Source of truth is internal/supportstates, so
// this render can never drift from what the store actually enforces.
func ticketStates(args []string) int {
	fs := flag.NewFlagSet("ticket states", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	states := supportstates.States()
	transitions := supportstates.LegalTransitions()
	terminal := supportstates.TerminalStates()

	if *jsonOut {
		return printJSON(map[string]any{
			"schema_version": "witself.v0",
			"states":         states,
			"transitions":    transitions,
			"terminal":       terminal,
		})
	}
	// Human render: aligned "from -> [targets]" per row, in the
	// natural lifecycle order so an operator reads it top-down.
	w, flush := tableWriter("state\tlegal transitions to")
	for _, s := range states {
		targets := transitions[s]
		if len(targets) == 0 {
			_, _ = fmt.Fprintf(w, "%s\t(terminal)\n", s)
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\n", s, strings.Join(targets, ", "))
	}
	flush()
	return 0
}

func ticketList(args []string) int {
	fs := flag.NewFlagSet("ticket list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	token := fs.String("token", "", "admin token")
	tokenFile := fs.String("token-file", "", "file containing the admin token")
	states := fs.String("state", "", "comma-separated ticket states (default: all non-closed)")
	sinceStr := fs.String("since", "", "only tickets with last_activity_at ≥ RFC3339")
	limit := fs.Int("limit", 100, "per-cell page size (1-500)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tok, err := resolveAdminToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	filter := client.AdminTicketFilter{Limit: *limit}
	if s := strings.TrimSpace(*states); s != "" {
		for _, part := range strings.Split(s, ",") {
			if p := strings.TrimSpace(part); p != "" {
				filter.States = append(filter.States, p)
			}
		}
	}
	if s := strings.TrimSpace(*sinceStr); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself-admin: --since must be RFC3339: %v\n", err)
			return 2
		}
		filter.Since = &t
	}
	res, err := client.ListAdminTickets(context.Background(), cpEndpoint(*endpoint), tok, filter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(res)
	}
	if len(res.Tickets) == 0 {
		fmt.Fprintln(os.Stderr, "no tickets across the fleet matching your filter")
	} else {
		w, flush := tableWriter("cell\tlast_activity\tstate\tpriority\taccount_id\tticket_id\tsubject")
		for _, t := range res.Tickets {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				t.Cell,
				t.LastActivityAt.UTC().Format(time.RFC3339),
				t.State, t.Priority, t.AccountID, t.ID,
				tabSafe(safeText(t.Subject)))
		}
		flush()
	}
	// Cell-status footer: silence hides degraded fleet state.
	degraded := 0
	for _, c := range res.Cells {
		if c.Status != "ok" {
			degraded++
		}
	}
	if degraded > 0 {
		fmt.Fprintf(os.Stderr, "\n%d of %d cells reported an error or timeout:\n",
			degraded, len(res.Cells))
		for _, c := range res.Cells {
			if c.Status != "ok" {
				fmt.Fprintf(os.Stderr, "  %s: %s (%s)\n", c.Name, c.Status, c.Error)
			}
		}
	}
	if res.AggregateCapped {
		fmt.Fprintln(os.Stderr, "\nresult was capped at 500 tickets — use --since or --state to narrow the query.")
	}
	return 0
}

// ticketWatch polls /v1/admin/tickets on --interval, tracks a
// server-authoritative high-water mark on last_activity_at, and emits
// only tickets whose activity is newer than the previous tick. Powers
// the support pane in the follow-up TUI (#29) and any AI-agent
// reactive workflow that needs to know "what just happened."
//
// First tick emits nothing — it baselines the high-water mark from
// the newest ticket the server currently has. Subsequent ticks emit
// as they happen. Ctrl-C or SIGTERM exits cleanly. --json emits ndjson
// (one object per line, flushed) so a subprocess consumer (TUI or
// agent) can parse it as a stream.
//
// Errors from a single tick log to stderr and continue — a transient
// network blip must not kill a long-running dashboard.
func ticketWatch(args []string) int {
	fs := flag.NewFlagSet("ticket watch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	token := fs.String("token", "", "admin token")
	tokenFile := fs.String("token-file", "", "file containing the admin token")
	interval := fs.Duration("interval", 30*time.Second, "poll cadence (minimum 5s)")
	states := fs.String("state", "", "comma-separated ticket states (default: all non-closed)")
	limit := fs.Int("limit", 100, "per-cell page size (1-500)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *interval < 5*time.Second {
		// Guard against a tight loop that would hammer the fan-out
		// across every cell every second. 5s is already aggressive.
		*interval = 5 * time.Second
	}
	tok, err := resolveAdminToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	filter := client.AdminTicketFilter{Limit: *limit}
	if s := strings.TrimSpace(*states); s != "" {
		for _, part := range strings.Split(s, ",") {
			if p := strings.TrimSpace(part); p != "" {
				filter.States = append(filter.States, p)
			}
		}
	}

	// Clean shutdown on Ctrl-C or SIGTERM. The current in-flight call
	// still gets to complete (context is passed through) — we don't
	// tear the connection out from under a mid-fetch state.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Human-readable header once, before the first tick's output.
	if !*jsonOut {
		fmt.Fprintf(os.Stderr, "watching every %s. ctrl-c to stop.\n", *interval)
	}

	ep := cpEndpoint(*endpoint)
	var hwm time.Time
	first := true

	tick := func() {
		f := filter
		if !first {
			since := hwm
			f.Since = &since
		}
		res, err := client.ListAdminTickets(ctx, ep, tok, f)
		if err != nil {
			// Don't die on a transient. Log to stderr and let the
			// next tick try again — TUI parsers can filter their
			// input to lines that parse as JSON.
			fmt.Fprintf(os.Stderr, "witself-admin: watch tick failed: %v\n", err)
			return
		}
		if first {
			hwm = newestActivity(res.Tickets, time.Now().UTC().Add(-1*time.Second))
			first = false
			return
		}
		for _, t := range res.Tickets {
			if !t.LastActivityAt.After(hwm) {
				continue
			}
			emitWatchedTicket(t, *jsonOut)
		}
		hwm = newestActivity(res.Tickets, hwm)
	}

	tick()
	if ctx.Err() != nil {
		return 0
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
			tick()
		}
	}
}

// newestActivity returns the newest LastActivityAt across tickets, or
// fallback if the slice is empty / all timestamps are before fallback.
// Extracted for testability — the polling loop's hwm advance is what
// the unit test pins.
func newestActivity(tickets []client.AdminTicket, fallback time.Time) time.Time {
	newest := fallback
	for _, t := range tickets {
		if t.LastActivityAt.After(newest) {
			newest = t.LastActivityAt
		}
	}
	return newest
}

// emitWatchedTicket prints one ticket to stdout. jsonMode emits a
// single JSON object per line (ndjson — trailing newline required
// so consumers can split on '\n'). Human mode prints a
// safeText/tabSafe single-line summary.
func emitWatchedTicket(t client.AdminTicket, jsonMode bool) {
	if jsonMode {
		buf, err := json.Marshal(t)
		if err != nil {
			return
		}
		_, _ = os.Stdout.Write(append(buf, '\n'))
		return
	}
	fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		t.LastActivityAt.UTC().Format(time.RFC3339),
		t.Cell, t.State, t.Priority, t.AccountID, t.ID,
		tabSafe(safeText(t.Subject)))
}

func ticketShow(args []string) int {
	fs := flag.NewFlagSet("ticket show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	token := fs.String("token", "", "admin token")
	tokenFile := fs.String("token-file", "", "file containing the admin token")
	account := fs.String("account", "", "account id (required)")
	ticket := fs.String("ticket", "", "ticket id (required)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*account) == "" || strings.TrimSpace(*ticket) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself-admin ticket show --account ACCOUNT_ID --ticket TKT_ID")
		return 2
	}
	tok, err := resolveAdminToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	res, err := client.GetAdminTicket(context.Background(), cpEndpoint(*endpoint), tok, *account, *ticket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(res)
	}
	fmt.Printf("ticket %s (%s / %s / %s)\n  account: %s\n  subject: %s\n  opened:  %s by %s:%s\n",
		res.Ticket.ID, res.Ticket.Category, res.Ticket.State, res.Ticket.Priority,
		res.Ticket.AccountID,
		tabSafe(safeText(res.Ticket.Subject)),
		res.Ticket.OpenedAt.UTC().Format(time.RFC3339),
		res.Ticket.OpenedByKind, safeText(res.Ticket.OpenedByID))
	fmt.Println()
	for _, m := range res.Messages {
		who := m.AuthorKind
		if m.AuthorID != "" {
			who = m.AuthorKind + ":" + safeText(m.AuthorID)
		}
		fmt.Printf("--- %s  %s ---\n%s\n\n",
			m.PostedAt.UTC().Format(time.RFC3339), who, safeText(m.Body))
	}
	return 0
}

func ticketReply(args []string) int {
	fs := flag.NewFlagSet("ticket reply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	token := fs.String("token", "", "admin token")
	tokenFile := fs.String("token-file", "", "file containing the admin token")
	account := fs.String("account", "", "account id (required)")
	ticket := fs.String("ticket", "", "ticket id (required)")
	body := fs.String("body", "", "reply body (required unless --body-file or --stdin)")
	bodyFile := fs.String("body-file", "", "read reply from FILE ('-' means stdin)")
	stdin := fs.Bool("stdin", false, "read reply from stdin")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*account) == "" || strings.TrimSpace(*ticket) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself-admin ticket reply --account ACCOUNT_ID --ticket TKT_ID (--body TEXT|--body-file FILE|--stdin)")
		return 2
	}
	text, err := readBodyFromFlags(*body, *bodyFile, *stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(os.Stderr, "witself-admin: reply body is required")
		return 2
	}
	tok, err := resolveAdminToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	msg, err := client.ReplyAdminTicket(context.Background(), cpEndpoint(*endpoint), tok, *account, *ticket, text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"message": msg})
	}
	fmt.Printf("posted reply on %s (message %s)\n", *ticket, msg.ID)
	return 0
}

// ticketState is shared by `ticket state`, `ticket resolve`, `ticket close`.
// When forceState is empty, --state is required; otherwise forceState wins.
func ticketState(args []string, forceState string) int {
	name := "ticket state"
	if forceState != "" {
		name = "ticket " + forceState
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	token := fs.String("token", "", "admin token")
	tokenFile := fs.String("token-file", "", "file containing the admin token")
	account := fs.String("account", "", "account id (required)")
	ticket := fs.String("ticket", "", "ticket id (required)")
	stateFlag := fs.String("state", "", "target state (open|awaiting_admin|awaiting_customer|resolved|closed)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	target := forceState
	if target == "" {
		target = strings.TrimSpace(*stateFlag)
	}
	if strings.TrimSpace(*account) == "" || strings.TrimSpace(*ticket) == "" || target == "" {
		fmt.Fprintf(os.Stderr, "usage: witself-admin %s --account ACCOUNT_ID --ticket TKT_ID%s\n",
			name,
			func() string {
				if forceState == "" {
					return " --state STATE"
				}
				return ""
			}(),
		)
		return 2
	}
	tok, err := resolveAdminToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	t, err := client.ChangeAdminTicketState(context.Background(), cpEndpoint(*endpoint), tok, *account, *ticket, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"ticket": t})
	}
	// The server returns the POST-transition ticket — the prior state
	// isn't on the wire, so print only where it landed (an earlier
	// version printed "resolved → resolved").
	fmt.Printf("%s is now %s\n", t.ID, t.State)
	return 0
}

// readBodyFromFlags resolves the reply text from the three mutually-
// exclusive body sources — same rule the witself CLI uses.
func readBodyFromFlags(inline, file string, stdin bool) (string, error) {
	sources := 0
	if strings.TrimSpace(inline) != "" {
		sources++
	}
	if strings.TrimSpace(file) != "" {
		sources++
	}
	if stdin {
		sources++
	}
	if sources > 1 {
		return "", fmt.Errorf("only one of --body, --body-file, or --stdin may be set")
	}
	switch {
	case stdin, file == "-":
		buf, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(buf), nil
	case file != "":
		buf, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read --body-file: %w", err)
		}
		return string(buf), nil
	default:
		return inline, nil
	}
}

// JSON envelope builders. Every --json output the witself-admin CLI
// emits for a single- or multi-resource response uses one of these,
// so the wrapped-envelope convention (documented in #27) is enforced
// in exactly one place. Extracting these tiny helpers lets unit tests
// pin the shape without needing to spin up the full CLI.
//
// Rule: single-resource responses go under a named envelope key that
// names the KIND of resource (not just its fields). A TUI or agent
// can route on the key alone without shape-sniffing.

func whoamiJSONMap(me *client.AdminWhoami) map[string]any {
	return map[string]any{"admin": me}
}

func supportPolicyReadJSONMap(res *client.SupportPolicyRead) map[string]any {
	return map[string]any{"support_policy": res}
}

func supportPolicyChangeJSONMap(res *client.SupportPolicyChange) map[string]any {
	return map[string]any{"support_policy_change": res}
}

func accountPolicyJSONMap(res *client.AdminAccountPolicy) map[string]any {
	return map[string]any{"account_policy": res}
}

// accountCmd handles `witself-admin account ...`. Slice 1b.iv seeds
// it with support-policy; future admin-only per-account settings hang
// off the same tree.
func accountCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself-admin account (support-policy|transcript-retention|plan-override) ...")
		return 2
	}
	switch args[0] {
	case "support-policy":
		return accountSupportPolicy(args[1:])
	case "transcript-retention":
		return accountTranscriptRetention(args[1:])
	case "plan-override":
		return accountPlanOverride(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself-admin account: unknown subcommand %q\n", args[0])
		return 2
	}
}

// accountSupportPolicy reads or writes an account's support_policy.
// Without --set, reads and prints the current value. With --set,
// PATCHes to the new value and prints the transition. Idempotent
// server-side (no-op audit event) so re-running is safe.
func accountSupportPolicy(args []string) int {
	fs := flag.NewFlagSet("account support-policy", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	token := fs.String("token", "", "admin token")
	tokenFile := fs.String("token-file", "", "file containing the admin token")
	account := fs.String("account", "", "account id (required)")
	set := fs.String("set", "", "flip policy to this value (enabled|disabled)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*account) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself-admin account support-policy --account ACCOUNT_ID [--set enabled|disabled]")
		return 2
	}
	tok, err := resolveAdminToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	ep := cpEndpoint(*endpoint)

	if strings.TrimSpace(*set) == "" {
		res, err := client.GetAdminSupportPolicy(context.Background(), ep, tok, *account)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
			return 1
		}
		if *jsonOut {
			return printJSON(supportPolicyReadJSONMap(res))
		}
		fmt.Printf("%s: %s\n", res.AccountID, res.SupportPolicy)
		return 0
	}
	res, err := client.SetAdminSupportPolicy(context.Background(), ep, tok, *account, *set)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(supportPolicyChangeJSONMap(res))
	}
	if res.PolicyFrom == res.PolicyTo {
		fmt.Printf("%s: already %s (no change)\n", res.AccountID, res.PolicyTo)
	} else {
		fmt.Printf("%s: %s → %s\n", res.AccountID, res.PolicyFrom, res.PolicyTo)
	}
	return 0
}

func resolveAdminAccountAction(endpoint, token, tokenFile string) (string, string, error) {
	tok, err := resolveAdminToken(token, tokenFile)
	if err != nil {
		return "", "", err
	}
	return cpEndpoint(endpoint), tok, nil
}

func printAdminAccountPolicy(res *client.AdminAccountPolicy) int {
	retention := "indefinite"
	if res.TranscriptRetention.EffectiveDays != nil {
		retention = fmt.Sprintf("%d days", *res.TranscriptRetention.EffectiveDays)
	}
	override := "none"
	if res.PlanOverride != nil {
		override = res.PlanOverride.Plan
	}
	applyState := "applied"
	if res.ApplyPending {
		applyState = "pending"
	}
	fmt.Printf("%s: plan=%s billing_plan=%s retention=%s retention_override=%t plan_override=%s apply=%s desired_revision=%d applied_revision=%d\n",
		safeText(res.AccountID), safeText(res.Plan), safeText(res.BillingPlan),
		retention, res.TranscriptRetention.Overridden, safeText(override),
		applyState, res.DesiredRevision, res.AppliedRevision)
	return reportAdminAccountPolicyPending(res)
}

func printAdminAccountPolicyJSON(res *client.AdminAccountPolicy) int {
	if code := printJSON(accountPolicyJSONMap(res)); code != 0 {
		return code
	}
	return reportAdminAccountPolicyPending(res)
}

func reportAdminAccountPolicyPending(res *client.AdminAccountPolicy) int {
	if res.ApplyPending {
		fmt.Fprintln(os.Stderr,
			"witself-admin: policy is saved but still pending cell application; verify convergence before relying on it")
		return 1
	}
	return 0
}

// accountTranscriptRetention manages an account exception independently from
// plan and price:
//
//	witself-admin account transcript-retention get --account ACCOUNT
//	witself-admin account transcript-retention set --account ACCOUNT --days 60 --reason "..."
//	witself-admin account transcript-retention set --account ACCOUNT --indefinite --reason "..."
//	witself-admin account transcript-retention clear --account ACCOUNT --reason "..."
func accountTranscriptRetention(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself-admin account transcript-retention (get|set|clear) ...")
		return 2
	}
	action := args[0]
	if action != "get" && action != "set" && action != "clear" {
		fmt.Fprintf(os.Stderr, "witself-admin account transcript-retention: unknown action %q\n", action)
		return 2
	}

	fs := flag.NewFlagSet("account transcript-retention "+action, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	token := fs.String("token", "", "admin token")
	tokenFile := fs.String("token-file", "", "file containing the admin token")
	account := fs.String("account", "", "account id (required)")
	days := fs.Int64("days", 0, "finite retention window (1-36500)")
	indefinite := fs.Bool("indefinite", false, "retain transcripts indefinitely")
	reason := fs.String("reason", "", "required audit reason for set/clear")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*account) == "" {
		fmt.Fprintf(os.Stderr, "usage: witself-admin account transcript-retention %s --account ACCOUNT_ID", action)
		switch action {
		case "set":
			fmt.Fprint(os.Stderr, " (--days N|--indefinite) --reason REASON")
		case "clear":
			fmt.Fprint(os.Stderr, " --reason REASON")
		}
		fmt.Fprintln(os.Stderr)
		return 2
	}
	switch action {
	case "get":
		if *days != 0 || *indefinite || strings.TrimSpace(*reason) != "" {
			fmt.Fprintln(os.Stderr, "witself-admin: get does not accept --days, --indefinite, or --reason")
			return 2
		}
	case "set":
		if (*days == 0) == !*indefinite {
			fmt.Fprintln(os.Stderr, "witself-admin: set exactly one of --days N or --indefinite")
			return 2
		}
		if *days < 0 || *days > client.MaxAdminTranscriptRetentionDays {
			fmt.Fprintf(os.Stderr, "witself-admin: --days must be between 1 and %d\n", client.MaxAdminTranscriptRetentionDays)
			return 2
		}
		if strings.TrimSpace(*reason) == "" {
			fmt.Fprintln(os.Stderr, "witself-admin: --reason is required for set")
			return 2
		}
	case "clear":
		if *days != 0 || *indefinite {
			fmt.Fprintln(os.Stderr, "witself-admin: clear does not accept --days or --indefinite")
			return 2
		}
		if strings.TrimSpace(*reason) == "" {
			fmt.Fprintln(os.Stderr, "witself-admin: --reason is required for clear")
			return 2
		}
	}

	ep, tok, err := resolveAdminAccountAction(*endpoint, *token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	var res *client.AdminAccountPolicy
	switch action {
	case "get":
		res, err = client.GetAdminTranscriptRetention(context.Background(), ep, tok, *account)
	case "set":
		var finite *int64
		if !*indefinite {
			finite = days
		}
		res, err = client.SetAdminTranscriptRetention(context.Background(), ep, tok, *account,
			client.AdminTranscriptRetentionInput{Days: finite, Indefinite: *indefinite, Reason: *reason})
	case "clear":
		res, err = client.ClearAdminTranscriptRetention(context.Background(), ep, tok, *account, *reason)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printAdminAccountPolicyJSON(res)
	}
	return printAdminAccountPolicy(res)
}

// accountPlanOverride manages an effective classification exception without
// creating or changing a provider subscription.
func accountPlanOverride(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself-admin account plan-override (get|set|clear) ...")
		return 2
	}
	action := args[0]
	if action != "get" && action != "set" && action != "clear" {
		fmt.Fprintf(os.Stderr, "witself-admin account plan-override: unknown action %q\n", action)
		return 2
	}

	fs := flag.NewFlagSet("account plan-override "+action, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	token := fs.String("token", "", "admin token")
	tokenFile := fs.String("token-file", "", "file containing the admin token")
	account := fs.String("account", "", "account id (required)")
	plan := fs.String("plan", "", "effective plan id")
	reason := fs.String("reason", "", "required audit reason for set/clear")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*account) == "" {
		fmt.Fprintf(os.Stderr, "usage: witself-admin account plan-override %s --account ACCOUNT_ID", action)
		switch action {
		case "set":
			fmt.Fprint(os.Stderr, " --plan PLAN_ID --reason REASON")
		case "clear":
			fmt.Fprint(os.Stderr, " --reason REASON")
		}
		fmt.Fprintln(os.Stderr)
		return 2
	}

	switch action {
	case "get":
		if strings.TrimSpace(*plan) != "" || strings.TrimSpace(*reason) != "" {
			fmt.Fprintln(os.Stderr, "witself-admin: get does not accept --plan or --reason")
			return 2
		}
	case "set":
		if strings.TrimSpace(*plan) == "" || strings.TrimSpace(*reason) == "" {
			fmt.Fprintln(os.Stderr, "witself-admin: --plan and --reason are required for set")
			return 2
		}
	case "clear":
		if strings.TrimSpace(*plan) != "" {
			fmt.Fprintln(os.Stderr, "witself-admin: clear does not accept --plan")
			return 2
		}
		if strings.TrimSpace(*reason) == "" {
			fmt.Fprintln(os.Stderr, "witself-admin: --reason is required for clear")
			return 2
		}
	}

	ep, tok, err := resolveAdminAccountAction(*endpoint, *token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	var res *client.AdminAccountPolicy
	switch action {
	case "get":
		res, err = client.GetAdminPlanOverride(context.Background(), ep, tok, *account)
	case "set":
		res, err = client.SetAdminPlanOverride(context.Background(), ep, tok, *account, *plan, *reason)
	case "clear":
		res, err = client.ClearAdminPlanOverride(context.Background(), ep, tok, *account, *reason)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printAdminAccountPolicyJSON(res)
	}
	return printAdminAccountPolicy(res)
}
