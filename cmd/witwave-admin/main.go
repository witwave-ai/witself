// Command witwave-admin is the Witwave fleet-admin CLI. It always talks
// to the control plane (never a cell directly) so fleet-wide queries
// like "show me every open support ticket" fan out CP → all cells in
// parallel.
//
// Two credential surfaces live in this binary:
//
//   - Fleet token (WITWAVE_FLEET_TOKEN / --fleet-token) authorizes the
//     admin registry: mint, list, revoke, delete admins. It is the same
//     shared secret used by every other fleet-level operation.
//
//   - Admin token (WITWAVE_ADMIN_TOKEN / --token / --token-file)
//     authenticates a specific admin against the ticket routes. Minted
//     once by `witwave-admin admin mint` and shown once.
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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/witwave-ai/witself/internal/client"
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
		fmt.Println(version.String("witwave-admin"))
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
	default:
		fmt.Fprintf(os.Stderr, "witwave-admin: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "witwave-admin — the Witwave fleet-admin CLI")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  witwave-admin whoami        Verify an admin token and print its identity")
	fmt.Fprintln(w, "  witwave-admin admin ...     Manage fleet-admin credentials (requires fleet token)")
	fmt.Fprintln(w, "  witwave-admin ticket ...    Read/reply/transition support tickets across the fleet")
	fmt.Fprintln(w, "  witwave-admin version       Print version information")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Environment:")
	fmt.Fprintln(w, "  WITSELF_CONTROL_PLANE   control-plane URL (default https://self.witwave.ai)")
	fmt.Fprintln(w, "  WITWAVE_ADMIN_TOKEN     admin bearer token for `whoami` and `ticket` commands")
	fmt.Fprintln(w, "  WITWAVE_FLEET_TOKEN     fleet-level shared secret for `admin` commands")
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

// resolveAdminToken picks the admin token, in order: --token, --token-file,
// WITWAVE_ADMIN_TOKEN. Returns a friendly error when none is set.
func resolveAdminToken(tokenFlag, tokenFileFlag string) (string, error) {
	if t := strings.TrimSpace(tokenFlag); t != "" {
		return t, nil
	}
	if f := strings.TrimSpace(tokenFileFlag); f != "" {
		return readTokenFile(f)
	}
	if t := strings.TrimSpace(os.Getenv("WITWAVE_ADMIN_TOKEN")); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("no admin token — set WITWAVE_ADMIN_TOKEN, --token-file FILE, or --token TOKEN")
}

// resolveFleetToken picks the fleet-level shared secret, in order:
// --fleet-token, WITWAVE_FLEET_TOKEN.
func resolveFleetToken(tokenFlag string) (string, error) {
	if t := strings.TrimSpace(tokenFlag); t != "" {
		return t, nil
	}
	if t := strings.TrimSpace(os.Getenv("WITWAVE_FLEET_TOKEN")); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("no fleet token — set WITWAVE_FLEET_TOKEN or --fleet-token TOKEN")
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

// jsonFlag adds --json to a FlagSet — same convention as the ws CLI.
func jsonFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("json", false, "emit output as JSON instead of a table")
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 1
	}
	return 0
}

// tableWriter mirrors the ws CLI helper: elastic alignment on a TTY,
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

// safeText strips ASCII/C0 control characters + DEL, matching the ws
// CLI helper. Applied to any operator- or admin-supplied string before
// it hits a terminal, so a hostile ticket body can't hijack the screen.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return r
		}
		if r < 0x20 || r == 0x7F {
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

// whoamiCmd: witwave-admin whoami
func whoamiCmd(args []string) int {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	token := fs.String("token", "", "admin token (prefer --token-file / WITWAVE_ADMIN_TOKEN)")
	tokenFile := fs.String("token-file", "", "file containing the admin token")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tok, err := resolveAdminToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 2
	}
	me, err := client.GetAdminWhoami(context.Background(), cpEndpoint(*endpoint), tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(me)
	}
	fmt.Printf("%s\t%s\n", me.AdminID, me.Handle)
	return 0
}

// adminCmd handles `witwave-admin admin ...`.
func adminCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witwave-admin admin mint|list|revoke|delete ...")
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
		fmt.Fprintf(os.Stderr, "witwave-admin admin: unknown subcommand %q\n", args[0])
		return 2
	}
}

func adminMint(args []string) int {
	fs := flag.NewFlagSet("admin mint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	fleetToken := fs.String("fleet-token", "", "fleet shared secret (prefer WITWAVE_FLEET_TOKEN)")
	handle := fs.String("handle", "", "admin handle, e.g. sarah (required)")
	note := fs.String("note", "", "optional bookkeeping note (max 200 chars)")
	jsonOut := jsonFlag(fs)
	out := fs.String("out", "", "also write the raw admin token to this file (0600)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*handle) == "" {
		fmt.Fprintln(os.Stderr, "usage: witwave-admin admin mint --handle NAME [--note TEXT] [--out FILE]")
		return 2
	}
	ft, err := resolveFleetToken(*fleetToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 2
	}
	res, err := client.MintAdmin(context.Background(), cpEndpoint(*endpoint), ft, client.MintAdminInput{
		Handle: *handle,
		Note:   *note,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 1
	}
	if *out != "" {
		if err := os.WriteFile(*out, []byte(res.AdminToken+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "witwave-admin: wrote admin but could not save token file: %v\n", err)
			// Don't lose the token — fall through and print it too.
		} else {
			fmt.Fprintf(os.Stderr, "wrote admin token to %s (mode 0600)\n", *out)
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
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 2
	}
	admins, err := client.ListAdmins(context.Background(), cpEndpoint(*endpoint), ft)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
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
		fmt.Fprintln(os.Stderr, "usage: witwave-admin admin revoke --id ADMIN_ID")
		return 2
	}
	ft, err := resolveFleetToken(*fleetToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 2
	}
	admin, err := client.RevokeAdmin(context.Background(), cpEndpoint(*endpoint), ft, *adminID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
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
		fmt.Fprintln(os.Stderr, "usage: witwave-admin admin delete --id ADMIN_ID --yes")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "witwave-admin: delete is destructive; re-run with --yes to confirm.")
		return 2
	}
	ft, err := resolveFleetToken(*fleetToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 2
	}
	if err := client.DeleteAdmin(context.Background(), cpEndpoint(*endpoint), ft, *adminID); err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 1
	}
	fmt.Printf("deleted %s\n", *adminID)
	return 0
}

// ticketCmd handles `witwave-admin ticket ...`.
func ticketCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witwave-admin ticket list|show|reply|state|resolve|close ...")
		return 2
	}
	switch args[0] {
	case "list":
		return ticketList(args[1:])
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
	default:
		fmt.Fprintf(os.Stderr, "witwave-admin ticket: unknown subcommand %q\n", args[0])
		return 2
	}
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
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "witwave-admin: --since must be RFC3339: %v\n", err)
			return 2
		}
		filter.Since = &t
	}
	res, err := client.ListAdminTickets(context.Background(), cpEndpoint(*endpoint), tok, filter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
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
		fmt.Fprintln(os.Stderr, "usage: witwave-admin ticket show --account ACCOUNT_ID --ticket TKT_ID")
		return 2
	}
	tok, err := resolveAdminToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 2
	}
	res, err := client.GetAdminTicket(context.Background(), cpEndpoint(*endpoint), tok, *account, *ticket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
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
		fmt.Fprintln(os.Stderr, "usage: witwave-admin ticket reply --account ACCOUNT_ID --ticket TKT_ID (--body TEXT|--body-file FILE|--stdin)")
		return 2
	}
	text, err := readBodyFromFlags(*body, *bodyFile, *stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 2
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(os.Stderr, "witwave-admin: reply body is required")
		return 2
	}
	tok, err := resolveAdminToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 2
	}
	msg, err := client.ReplyAdminTicket(context.Background(), cpEndpoint(*endpoint), tok, *account, *ticket, text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "usage: witwave-admin %s --account ACCOUNT_ID --ticket TKT_ID%s\n",
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
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 2
	}
	t, err := client.ChangeAdminTicketState(context.Background(), cpEndpoint(*endpoint), tok, *account, *ticket, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witwave-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"ticket": t})
	}
	fmt.Printf("%s: %s → %s\n", t.ID, t.State, target)
	return 0
}

// readBodyFromFlags resolves the reply text from the three mutually-
// exclusive body sources — same rule the ws CLI uses.
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
