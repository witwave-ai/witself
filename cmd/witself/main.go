// Command ws is the Witself CLI. The full command surface is specified under
// docs/; this early build supports version, gen-bootstrap-token, and auth login.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/placement"
	"github.com/witwave-ai/witself/internal/token"
	"github.com/witwave-ai/witself/internal/version"
)

var cellNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

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
		fmt.Println(version.String("witself"))
		return 0
	case "gen-bootstrap-token":
		return genBootstrapToken(args[1:])
	case "auth":
		return authCmd(args[1:])
	case "account":
		return accountCmd(args[1:])
	case "realm":
		return realmCmd(args[1:])
	case "agent":
		return agentCmd(args[1:])
	case "plan":
		return planCmd(args[1:])
	case "operator":
		return operatorCmd(args[1:])
	case "token":
		return tokenCmd(args[1:])
	case "self":
		return selfCmd(args[1:])
	case "transcript":
		return transcriptCmd(args[1:])
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "witself: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

// genBootstrapToken mints an operator bootstrap token. This is the one token
// generated client-side (a pre-shared secret): it is inert until a server
// adopts it at init and binds it to the seeded operator.
func genBootstrapToken(args []string) int {
	fs := flag.NewFlagSet("gen-bootstrap-token", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cell := fs.String("cell", "", "cell name for the default output path (~/.witself/tokens/<cell>/bootstrap.token)")
	out := fs.String("out", "", "write the token to this file (0600) instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" && *cell != "" {
		p, err := defaultBootstrapTokenPath(*cell)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		*out = p
	}

	tok, err := token.New(token.KindBootstrap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}

	if *out == "" {
		fmt.Println(tok)
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if err := os.WriteFile(*out, []byte(tok+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "wrote operator bootstrap token to %s\n", *out)
	return 0
}

func defaultBootstrapTokenPath(cell string) (string, error) {
	if !cellNamePattern.MatchString(cell) {
		return "", fmt.Errorf("invalid cell name %q", cell)
	}
	root := os.Getenv("WITSELF_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".witself")
	}
	return filepath.Join(root, "tokens", cell, "bootstrap.token"), nil
}

func authCmd(args []string) int {
	if len(args) == 0 || args[0] != "login" {
		fmt.Fprintln(os.Stderr, "usage: witself auth login --endpoint URL --bootstrap-token-file FILE [--out FILE]")
		return 2
	}
	return authLogin(args[1:])
}

// authLogin exchanges a bootstrap token for an operator token at a witself-server
// endpoint — the CLI's first round-trip to a backend.
func authLogin(args []string) int {
	fs := flag.NewFlagSet("auth login", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("bootstrap-token-file", "", "file containing the bootstrap token")
	out := fs.String("out", "", "write the operator token to this file (0600) instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *endpoint == "" || *tokenFile == "" {
		fmt.Fprintln(os.Stderr, "witself: --endpoint and --bootstrap-token-file are required")
		return 2
	}

	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	res, err := client.BootstrapLogin(context.Background(), *endpoint, strings.TrimSpace(string(raw)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "logged in as operator %s\n", res.OperatorID)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(res.OperatorToken+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "wrote operator token to %s\n", *out)
		return 0
	}
	fmt.Println(res.OperatorToken)
	return 0
}

// connect resolves how to reach a cell as an operator. Explicit
// --endpoint/--token-file always wins (the self-hosted path). Otherwise a
// named local account (--account, WITSELF_ACCOUNT, or "default") supplies the
// operator token, and the control plane directory is asked — fresh, every
// time — which cell hosts the account. Accounts can move between cells; the
// CLI never caches an endpoint.
func connect(ctx context.Context, accountName, endpoint, tokenFile string) (string, string, error) {
	if tokenFile != "" {
		if endpoint == "" {
			return "", "", fmt.Errorf("--token-file needs --endpoint (or drop both to use a local account name)")
		}
		tok, err := readToken(tokenFile)
		if err != nil {
			return "", "", err
		}
		return endpoint, tok, nil
	}
	name, acct, tok, err := local.Resolve(accountName)
	if err != nil {
		return "", "", err
	}
	if endpoint != "" { // explicit endpoint, named account's token
		return endpoint, tok, nil
	}
	_, cellEndpoint, err := client.LookupAccount(ctx, defaultControlPlane, acct.ID)
	if err != nil {
		return "", "", fmt.Errorf("locate account %q (%s): %w", name, acct.ID, err)
	}
	return cellEndpoint, tok, nil
}

type accountLocator func(context.Context, string, string) (string, string, error)

type agentConnection struct {
	Endpoint    string
	Token       string
	AccountID   string
	AccountName string
	RealmName   string
	AgentName   string
}

// connectAgent resolves a local agent selector to a realm-scoped token file.
// The selector chooses local credential material only; the server derives the
// authenticated identity from that token and selfShow verifies the result.
func connectAgent(ctx context.Context, accountName, realmName, agentName, endpoint, tokenFile string) (agentConnection, error) {
	return connectAgentWithLocator(ctx, accountName, realmName, agentName, endpoint, tokenFile, client.LookupAccount)
}

func connectAgentWithLocator(ctx context.Context, accountName, realmName, agentName, endpoint, tokenFile string, locate accountLocator) (agentConnection, error) {
	realmName = strings.TrimSpace(realmName)
	if realmName == "" {
		realmName = strings.TrimSpace(os.Getenv("WITSELF_REALM"))
	}
	if realmName == "" {
		realmName = "default"
	}
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		agentName = strings.TrimSpace(os.Getenv("WITSELF_AGENT"))
	}

	if tokenFile != "" {
		if endpoint == "" {
			return agentConnection{}, fmt.Errorf("--token-file needs --endpoint (or use --account/--agent to resolve managed credentials)")
		}
		tok, err := readToken(tokenFile)
		if err != nil {
			return agentConnection{}, err
		}
		return agentConnection{Endpoint: endpoint, Token: tok, RealmName: realmName, AgentName: agentName}, nil
	}
	if agentName == "" {
		return agentConnection{}, fmt.Errorf("--agent is required when --token-file is not supplied")
	}

	name, acct, err := local.ResolveAccount(accountName)
	if err != nil {
		return agentConnection{}, err
	}
	tok, _, err := local.ReadAgentToken(name, realmName, agentName)
	if err != nil {
		return agentConnection{}, err
	}
	if endpoint == "" {
		_, endpoint, err = locate(ctx, defaultControlPlane, acct.ID)
		if err != nil {
			return agentConnection{}, fmt.Errorf("locate account %q (%s): %w", name, acct.ID, err)
		}
	}
	return agentConnection{
		Endpoint: endpoint, Token: tok, AccountID: acct.ID, AccountName: name,
		RealmName: realmName, AgentName: agentName,
	}, nil
}

// accountFlag registers the shared --account flag on a cell command.
func accountFlag(fs *flag.FlagSet) *string {
	return fs.String("account", "", `local account name (default: WITSELF_ACCOUNT or "default")`)
}

// jsonFlag registers the shared --json flag on read commands: self-describing
// output for agents and scripts, instead of positional TSV columns.
func jsonFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("json", false, "print JSON instead of columns")
}

// printJSON writes one compact JSON document to stdout.
func printJSON(v any) int {
	if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	return 0
}

// tableWriter picks where tabular rows go. On a terminal the columns are
// elastically aligned — kubectl's approach (text/tabwriter with its
// minwidth/tabwidth/padding tuning) — and the header joins the table. In a
// pipe, rows stay pure TSV on stdout with the header on stderr, so `cut -f`
// keeps working. Call flush after the last row.
func tableWriter(header string) (w io.Writer, flush func()) {
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		tw := tabwriter.NewWriter(os.Stdout, 6, 4, 3, ' ', 0)
		_, _ = fmt.Fprintln(tw, header)
		return tw, func() { _ = tw.Flush() }
	}
	fmt.Fprintln(os.Stderr, header)
	return os.Stdout, func() {}
}

func flagWasPassed(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func parseCSVList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func printPlacementPolicy(policy placement.Policy) {
	w, flush := tableWriter("field\tvalues")
	_, _ = fmt.Fprintf(w, "preferred clouds\t%s\n", joinPolicyList(policy.PreferredClouds))
	_, _ = fmt.Fprintf(w, "preferred regions\t%s\n", joinPolicyList(policy.PreferredRegions))
	_, _ = fmt.Fprintf(w, "preferred channels\t%s\n", joinPolicyList(policy.PreferredChannels))
	_, _ = fmt.Fprintf(w, "only clouds\t%s\n", joinPolicyList(policy.AllowedClouds))
	_, _ = fmt.Fprintf(w, "only regions\t%s\n", joinPolicyList(policy.AllowedRegions))
	_, _ = fmt.Fprintf(w, "only channels\t%s\n", joinPolicyList(policy.AllowedChannels))
	_, _ = fmt.Fprintf(w, "rebalance on\t%s\n", joinPolicyListOr(policy.RebalanceOn, "(none)"))
	flush()
}

func joinPolicyList(values []string) string {
	return joinPolicyListOr(values, "(any)")
}

func joinPolicyListOr(values []string, empty string) string {
	if len(values) == 0 {
		return empty
	}
	return strings.Join(values, ",")
}

func realmCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself realm create|list|delete [--account NAME]")
		return 2
	}
	switch args[0] {
	case "create":
		return realmCreate(args[1:])
	case "list":
		return realmList(args[1:])
	case "delete":
		return realmDelete(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself realm: unknown subcommand %q\n", args[0])
		return 2
	}
}

func realmCreate(args []string) int {
	fs := flag.NewFlagSet("realm create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	name := fs.Arg(0)
	if name == "" {
		fmt.Fprintln(os.Stderr, "usage: witself realm create [--account NAME] NAME")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	r, err := client.CreateRealm(ctx, ep, tok, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Printf("%s\t%s\n", r.ID, r.Name)
	return 0
}

func realmDelete(args []string) int {
	fs := flag.NewFlagSet("realm delete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	yes := fs.Bool("yes", false, "confirm realm deletion")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	realmID := fs.Arg(0)
	if realmID == "" {
		fmt.Fprintln(os.Stderr, "usage: witself realm delete [--account NAME] --yes REALM")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "witself: refusing to delete realm without --yes")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if err := client.DeleteRealm(ctx, ep, tok, realmID); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "deleted realm %s\n", realmID)
	return 0
}

func realmList(args []string) int {
	fs := flag.NewFlagSet("realm list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	jsonOut := jsonFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	realms, err := client.ListRealms(ctx, ep, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if realms == nil {
		realms = []client.Realm{}
	}
	if *jsonOut {
		return printJSON(map[string]any{"realms": realms})
	}
	w, flush := tableWriter("id\tname")
	for _, r := range realms {
		_, _ = fmt.Fprintf(w, "%s\t%s\n", r.ID, r.Name)
	}
	flush()
	return 0
}

func agentCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself agent create|list|delete [--account NAME] --realm REALM")
		return 2
	}
	switch args[0] {
	case "create":
		return agentCreate(args[1:])
	case "list":
		return agentList(args[1:])
	case "delete":
		return agentDelete(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself agent: unknown subcommand %q\n", args[0])
		return 2
	}
}

func agentCreate(args []string) int {
	fs := flag.NewFlagSet("agent create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	realm := fs.String("realm", "", "realm id the agent belongs to")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	name := fs.Arg(0)
	if *realm == "" || name == "" {
		fmt.Fprintln(os.Stderr, "usage: witself agent create [--account NAME] --realm REALM NAME")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	a, err := client.CreateAgent(ctx, ep, tok, *realm, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Printf("%s\t%s\n", a.ID, a.Name)
	return 0
}

func agentList(args []string) int {
	fs := flag.NewFlagSet("agent list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	jsonOut := jsonFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	realm := fs.String("realm", "", "realm id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *realm == "" {
		fmt.Fprintln(os.Stderr, "usage: witself agent list [--account NAME] --realm REALM")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	agents, err := client.ListAgents(ctx, ep, tok, *realm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if agents == nil {
		agents = []client.Agent{}
	}
	if *jsonOut {
		return printJSON(map[string]any{"agents": agents})
	}
	w, flush := tableWriter("id\tname")
	for _, a := range agents {
		_, _ = fmt.Fprintf(w, "%s\t%s\n", a.ID, a.Name)
	}
	flush()
	return 0
}

func agentDelete(args []string) int {
	fs := flag.NewFlagSet("agent delete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	realm := fs.String("realm", "", "realm id")
	yes := fs.Bool("yes", false, "confirm agent deletion and token revocation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	agentID := fs.Arg(0)
	if *realm == "" || agentID == "" {
		fmt.Fprintln(os.Stderr, "usage: witself agent delete [--account NAME] --realm REALM --yes AGENT")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "witself: refusing to delete agent without --yes")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if err := client.DeleteAgent(ctx, ep, tok, *realm, agentID); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "deleted agent %s\n", agentID)
	return 0
}

func operatorCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself operator list|create|delete [--account NAME]")
		return 2
	}
	switch args[0] {
	case "list":
		return operatorList(args[1:])
	case "create":
		return operatorCreate(args[1:])
	case "delete":
		return operatorDelete(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself operator: unknown subcommand %q\n", args[0])
		return 2
	}
}

func operatorList(args []string) int {
	fs := flag.NewFlagSet("operator list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	jsonOut := jsonFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	operators, err := client.ListOperators(ctx, ep, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if operators == nil {
		operators = []client.Operator{}
	}
	if *jsonOut {
		for i := range operators {
			if operators[i].Tokens == nil {
				operators[i].Tokens = []client.OperatorToken{}
			}
		}
		return printJSON(map[string]any{"operators": operators})
	}
	w, flush := tableWriter("id\tname\trole\troot\tcreated\tupdated\ttokens")
	for _, op := range operators {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%s\t%s\t%s\n",
			op.ID,
			tabSafe(op.DisplayName),
			op.Role,
			op.IsRoot,
			formatTime(op.CreatedAt),
			formatTime(op.UpdatedAt),
			operatorTokenSummary(op.Tokens),
		)
	}
	flush()
	return 0
}

func operatorCreate(args []string) int {
	fs := flag.NewFlagSet("operator create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	name := fs.String("name", "", "operator display name")
	tokenName := fs.String("token-name", "", "initial operator token display name")
	ttl := fs.String("ttl", "", "initial operator token lifetime, such as 24h or 30m")
	out := fs.String("out", "", "write the new operator token to this file (0600) instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" {
		fmt.Fprintln(os.Stderr, "usage: witself operator create [--account NAME] --name NAME [--token-name NAME] [--ttl DURATION] [--out FILE]")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	res, err := client.CreateOperator(ctx, ep, tok, *name, *tokenName, *ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	tokenID := "-"
	if len(res.Operator.Tokens) > 0 {
		tokenID = res.Operator.Tokens[0].ID
	}
	fmt.Fprintf(os.Stderr, "created operator %s (%s), token %s\n", res.Operator.ID, res.Operator.DisplayName, tokenID)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(res.OperatorToken+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "wrote operator token to %s\n", *out)
		return 0
	}
	fmt.Println(res.OperatorToken)
	return 0
}

func operatorDelete(args []string) int {
	fs := flag.NewFlagSet("operator delete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	yes := fs.Bool("yes", false, "confirm operator deletion and token revocation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	operatorID := fs.Arg(0)
	if operatorID == "" {
		fmt.Fprintln(os.Stderr, "usage: witself operator delete [--account NAME] --yes OPERATOR")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "witself: refusing to delete operator without --yes")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if err := client.DeleteOperator(ctx, ep, tok, operatorID); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "deleted operator %s\n", operatorID)
	return 0
}

func tokenCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself token create|revoke [--account NAME]")
		return 2
	}
	switch args[0] {
	case "create":
		return tokenCreate(args[1:])
	case "revoke":
		return tokenRevoke(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself token: unknown subcommand %q\n", args[0])
		return 2
	}
}

func tokenCreate(args []string) int {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	agent := fs.String("agent", "", "agent id to mint a token for")
	operator := fs.Bool("operator", false, "mint another token for the authenticated operator")
	name := fs.String("name", "", "display name for an operator token")
	ttl := fs.String("ttl", "", "operator token lifetime, such as 24h or 30m")
	out := fs.String("out", "", "write the new token to this file (0600) instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*agent == "" && !*operator) || (*agent != "" && *operator) {
		fmt.Fprintln(os.Stderr, "usage: witself token create [--account NAME] (--agent AGENT | --operator) [--name NAME] [--ttl DURATION] [--out FILE]")
		return 2
	}
	if *agent != "" && *ttl != "" {
		fmt.Fprintln(os.Stderr, "witself: --ttl is currently supported only with --operator")
		return 2
	}
	if *agent != "" && *name != "" {
		fmt.Fprintln(os.Stderr, "witself: --name is currently supported only with --operator")
		return 2
	}
	ctx := context.Background()
	// Resolve the local account name early: named operator tokens default to
	// its managed home (accounts/<name>/operators/<token-name>.token).
	managedAccount := ""
	if *tokenFile == "" {
		if n, _, _, err := local.Resolve(*account); err == nil {
			managedAccount = n
		}
	}
	ep, op, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *operator {
		dest := *out
		if dest == "" && *name != "" && managedAccount != "" {
			dest = managedOperatorTokenPath(managedAccount, *name)
			if dest == "" {
				fmt.Fprintf(os.Stderr, "witself: invalid token name %q for a file (use --out, or letters/digits/hyphens)\n", *name)
				return 2
			}
			if _, err := os.Stat(dest); err == nil {
				fmt.Fprintf(os.Stderr, "witself: %s already exists — revoke and remove it first, or pass --out\n", dest)
				return 1
			}
		}
		res, err := client.CreateOperatorToken(ctx, ep, op, *name, *ttl)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		if res.TokenID != "" {
			fmt.Fprintf(os.Stderr, "created operator token %s\n", res.TokenID)
		}
		if dest != "" && dest != "-" {
			werr := os.MkdirAll(filepath.Dir(dest), 0o700)
			if werr == nil {
				werr = os.WriteFile(dest, []byte(res.OperatorToken+"\n"), 0o600)
			}
			if werr != nil {
				// Never strand the only copy of a fresh credential.
				fmt.Fprintf(os.Stderr, "witself: writing %s: %v — printing the token once instead:\n", dest, werr)
				fmt.Println(res.OperatorToken)
				return 1
			}
			fmt.Fprintf(os.Stderr, "wrote operator token for %s to %s\n", res.OperatorID, dest)
			if *name != "" && managedAccount != "" && *out == "" {
				fmt.Fprintf(os.Stderr, "revoke it later by name: witself token revoke --operator --name %s --yes\n", *name)
			}
			return 0
		}
		fmt.Println(res.OperatorToken)
		return 0
	}

	agentTok, tokenID, agentName, err := client.CreateAgentToken(ctx, ep, op, *agent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if tokenID != "" {
		fmt.Fprintf(os.Stderr, "created agent token %s\n", tokenID)
	}
	dest := *out
	if dest == "" && managedAccount != "" {
		// Managed home: agents/<agent-name>.token, falling back to the agent
		// id when the name isn't filename-safe (or the cell predates the
		// field) — both charsets are collision-free.
		fileBase := agentName
		if !cellNamePattern.MatchString(fileBase) {
			fileBase = *agent
		}
		tp, perr := local.TokenPath(managedAccount)
		if perr == nil {
			dest = filepath.Join(filepath.Dir(tp), "agents", fileBase+".token")
		}
	}
	if dest != "" && dest != "-" {
		werr := os.MkdirAll(filepath.Dir(dest), 0o700)
		if werr == nil {
			if _, err := os.Stat(dest); err == nil && *out == "" {
				werr = fmt.Errorf("%s already exists", dest)
			} else {
				werr = os.WriteFile(dest, []byte(agentTok+"\n"), 0o600)
			}
		}
		if werr != nil {
			// Never strand the only copy of a fresh credential.
			fmt.Fprintf(os.Stderr, "witself: writing %s: %v — printing the token once instead:\n", dest, werr)
			fmt.Println(agentTok)
			return 1
		}
		fmt.Fprintf(os.Stderr, "wrote agent token to %s\n", dest)
		return 0
	}
	fmt.Println(agentTok)
	return 0
}

func tokenRevoke(args []string) int {
	fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	tokenID := fs.String("token", "", "token id to revoke")
	operator := fs.Bool("operator", false, "select one of your own tokens by --name instead of --token")
	name := fs.String("name", "", "display name of your token to revoke (with --operator)")
	yes := fs.Bool("yes", false, "confirm token revocation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	byName := *operator && *name != ""
	if (*tokenID == "") == !byName { // exactly one selector
		fmt.Fprintln(os.Stderr, "usage: witself token revoke [--account NAME] (--token TOKEN_ID | --operator --name NAME) --yes")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "witself: refusing to revoke token without --yes")
		return 2
	}
	ctx := context.Background()
	managedAccount := ""
	if *tokenFile == "" {
		if n, _, _, err := local.Resolve(*account); err == nil {
			managedAccount = n
		}
	}
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	target := *tokenID
	if byName {
		// Resolve the display name among the CALLER's own live tokens only —
		// names aren't unique, and revoking someone else's token by name
		// would be a surprise.
		opID, _, err := client.Whoami(ctx, ep, tok)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		ops, err := client.ListOperators(ctx, ep, tok)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		var matches []string
		for _, op := range ops {
			if op.ID != opID {
				continue
			}
			for _, t := range op.Tokens {
				if t.DisplayName == *name {
					matches = append(matches, t.ID)
				}
			}
		}
		switch len(matches) {
		case 0:
			fmt.Fprintf(os.Stderr, "witself: no live token named %q on your operator\n", *name)
			return 1
		case 1:
			target = matches[0]
		default:
			fmt.Fprintf(os.Stderr, "witself: %d live tokens named %q — disambiguate with --token (%s)\n", len(matches), *name, strings.Join(matches, ", "))
			return 1
		}
	}
	if err := client.RevokeToken(ctx, ep, tok, target); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "revoked token %s\n", target)
	if byName && managedAccount != "" {
		if p := managedOperatorTokenPath(managedAccount, *name); p != "" {
			if err := os.Remove(p); err == nil {
				fmt.Fprintf(os.Stderr, "removed %s\n", p)
			}
		}
	}
	return 0
}

// managedOperatorTokenPath is the home for a named resident operator token:
// accounts/<account>/operators/<token-name>.token. Empty when the name can't
// be a safe filename.
func managedOperatorTokenPath(accountName, tokenName string) string {
	if !cellNamePattern.MatchString(tokenName) {
		return ""
	}
	tp, err := local.TokenPath(accountName)
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(tp), "operators", tokenName+".token")
}

func readToken(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// connectDomain resolves the endpoint plus an operator-or-agent token used by
// domain commands. An explicit/env token can still use a local account solely
// for fresh cell routing; without an override, the account's operator token is
// used (read-only on transcript surfaces).
func connectDomain(ctx context.Context, accountName, endpoint, tokenFile string) (string, string, error) {
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("WITSELF_ENDPOINT"))
	}
	if tokenFile == "" {
		tokenFile = strings.TrimSpace(os.Getenv("WITSELF_TOKEN_FILE"))
	}
	var tok string
	var err error
	if tokenFile != "" {
		tok, err = readToken(tokenFile)
		if err != nil {
			return "", "", err
		}
	} else if envToken := strings.TrimSpace(os.Getenv("WITSELF_TOKEN")); envToken != "" {
		tok = envToken
	}
	if tok == "" {
		return connect(ctx, accountName, endpoint, "")
	}
	if endpoint != "" {
		return endpoint, tok, nil
	}
	name, acct, _, err := local.Resolve(accountName)
	if err != nil {
		return "", "", fmt.Errorf("an agent token without --endpoint needs a local account for routing: %w", err)
	}
	_, endpoint, err = client.LookupAccount(ctx, defaultControlPlane, acct.ID)
	if err != nil {
		return "", "", fmt.Errorf("locate account %q (%s): %w", name, acct.ID, err)
	}
	return endpoint, tok, nil
}

func operatorTokenSummary(tokens []client.OperatorToken) string {
	if len(tokens) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		label := tok.ID
		if tok.DisplayName != "" {
			label += ":" + tabSafe(tok.DisplayName)
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, ",")
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func tabSafe(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// safeText strips C0 control characters, DEL, and the C1 range
// (U+0080–U+009F) from operator- or admin-supplied strings before we
// print them, so a malicious ticket body or subject can't smuggle
// ANSI/OSC escapes into a reader's terminal (screen clear, window-title
// spoof, cursor jumps). C1 matters: U+009B is a single-rune CSI that
// C1-honoring terminals execute exactly like ESC-[. Keeps \t and \n so
// plain multi-line text and tab-indented content survive; use tabSafe
// on top for single-line contexts.
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

// defaultControlPlane is the Witself Cloud front door: the one address a Cloud
// user ever needs. Self-hosted deployments never contact it.
const defaultControlPlane = "https://self.witwave.ai"

// accountCmd handles `witself account ...`.
func accountCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself account create|adopt|list|status|placement|resend-verification|change-email|change-display-name|recover|close|events|support|forget ...")
		return 2
	}
	switch args[0] {
	case "create":
		return accountCreate(args[1:])
	case "adopt":
		return accountAdopt(args[1:])
	case "list":
		return accountList(args[1:])
	case "status":
		return accountStatus(args[1:])
	case "placement":
		return accountPlacementCmd(args[1:])
	case "resend-verification":
		return accountResendVerification(args[1:])
	case "change-email":
		return accountChangeEmail(args[1:])
	case "change-display-name":
		return accountChangeDisplayName(args[1:])
	case "suspend":
		return accountSuspend(args[1:])
	case "resume":
		return accountResume(args[1:])
	case "recover":
		return accountRecover(args[1:])
	case "close":
		return accountClose(args[1:])
	case "events":
		return accountEvents(args[1:])
	case "support":
		return accountSupportCmd(args[1:])
	case "forget":
		return accountForget(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself account: unknown subcommand %q\n", args[0])
		return 2
	}
}

func accountPlacementCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself account placement show|set|reset [--account NAME]")
		return 2
	}
	switch args[0] {
	case "show":
		return accountPlacementShow(args[1:])
	case "set":
		return accountPlacementSet(args[1:])
	case "reset":
		return accountPlacementReset(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself account placement: unknown subcommand %q\n", args[0])
		return 2
	}
}

func accountPlacementShow(args []string) int {
	fs := flag.NewFlagSet("account placement show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	jsonOut := jsonFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	policy, err := client.GetPlacementPolicy(ctx, ep, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"placement_policy": policy})
	}
	printPlacementPolicy(policy)
	return 0
}

func accountPlacementSet(args []string) int {
	fs := flag.NewFlagSet("account placement set", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	jsonOut := jsonFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	preferClouds := fs.String("prefer-clouds", "", "ranked cloud preference list (aws,gcp,azure); empty clears it")
	preferRegions := fs.String("prefer-regions", "", "ranked canonical region list (for example usw2,use1); empty clears it")
	preferChannels := fs.String("prefer-channels", "", "ranked channel list (stable,edge,experimental); empty clears it")
	onlyClouds := fs.String("only-clouds", "", "hard cloud pin list; empty allows every cloud")
	onlyRegions := fs.String("only-regions", "", "hard canonical region pin list; empty allows every region")
	onlyChannels := fs.String("only-channels", "", "hard channel pin list; empty allows every channel")
	rebalanceOn := fs.String("rebalance-on", "", "movement triggers (cloud,region,channel); empty disables live rebalance")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself account placement set [--account NAME] [--prefer-clouds CSV] [--prefer-regions CSV] [--prefer-channels CSV] [--only-clouds CSV] [--only-regions CSV] [--only-channels CSV] [--rebalance-on CSV]")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	policy, err := client.GetPlacementPolicy(ctx, ep, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	changed := false
	if flagWasPassed(fs, "prefer-clouds") {
		policy.PreferredClouds = parseCSVList(*preferClouds)
		changed = true
	}
	if flagWasPassed(fs, "prefer-regions") {
		policy.PreferredRegions = parseCSVList(*preferRegions)
		changed = true
	}
	if flagWasPassed(fs, "prefer-channels") {
		policy.PreferredChannels = parseCSVList(*preferChannels)
		changed = true
	}
	if flagWasPassed(fs, "only-clouds") {
		policy.AllowedClouds = parseCSVList(*onlyClouds)
		changed = true
	}
	if flagWasPassed(fs, "only-regions") {
		policy.AllowedRegions = parseCSVList(*onlyRegions)
		changed = true
	}
	if flagWasPassed(fs, "only-channels") {
		policy.AllowedChannels = parseCSVList(*onlyChannels)
		changed = true
	}
	if flagWasPassed(fs, "rebalance-on") {
		policy.RebalanceOn = parseCSVList(*rebalanceOn)
		changed = true
	}
	if !changed {
		fmt.Fprintln(os.Stderr, "usage: witself account placement set needs at least one policy flag")
		return 2
	}
	policy, err = placement.Normalize(policy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	policy, err = client.SetPlacementPolicy(ctx, ep, tok, policy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"placement_policy": policy})
	}
	fmt.Fprintln(os.Stderr, "placement policy updated")
	printPlacementPolicy(policy)
	return 0
}

func accountPlacementReset(args []string) int {
	fs := flag.NewFlagSet("account placement reset", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	jsonOut := jsonFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself account placement reset [--account NAME]")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	policy, err := client.SetPlacementPolicy(ctx, ep, tok, placement.DefaultPolicy())
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"placement_policy": policy})
	}
	fmt.Fprintln(os.Stderr, "placement policy reset")
	printPlacementPolicy(policy)
	return 0
}

// accountSupportCmd is the ws-side support-ticket entry point. Slice 1a
// covers the tenant half — an operator can open, list, show, reply,
// and close their own tickets. The admin side (list-across-accounts,
// answer-as-admin) is slice 1b.
func accountSupportCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself account support open|list|show|reply|close ...")
		return 2
	}
	switch args[0] {
	case "open":
		return accountSupportOpen(args[1:])
	case "list":
		return accountSupportList(args[1:])
	case "show":
		return accountSupportShow(args[1:])
	case "reply":
		return accountSupportReply(args[1:])
	case "close":
		return accountSupportClose(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself account support: unknown subcommand %q\n", args[0])
		return 2
	}
}

func accountSupportOpen(args []string) int {
	fs := flag.NewFlagSet("account support open", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	subject := fs.String("subject", "", "one-line ticket title (required)")
	category := fs.String("category", "", "technical|billing|security|other (default: other)")
	priority := fs.String("priority", "", "low|normal|high|urgent (default: normal)")
	// Body sources: --body inline, --body-file @path, or stdin when --stdin.
	body := fs.String("body", "", "ticket description (required unless --body-file or --stdin)")
	bodyFile := fs.String("body-file", "", "read description from FILE ('-' means stdin)")
	stdin := fs.Bool("stdin", false, "read description from stdin")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*subject) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself account support open --subject TEXT (--body TEXT|--body-file FILE|--stdin)")
		return 2
	}
	text, err := readBodyFromFlags(*body, *bodyFile, *stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(os.Stderr, "witself: description body is required (use --body, --body-file, or --stdin)")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	res, err := client.OpenSupportTicket(ctx, ep, tok, client.OpenTicketInput{
		Subject:  *subject,
		Category: *category,
		Priority: *priority,
		Body:     text,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(res)
	}
	fmt.Printf("opened ticket %s (%s / %s)\n  subject: %s\n",
		res.Ticket.ID, res.Ticket.Category, res.Ticket.State, tabSafe(safeText(res.Ticket.Subject)))
	return 0
}

func accountSupportList(args []string) int {
	fs := flag.NewFlagSet("account support list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	tickets, err := client.ListSupportTickets(ctx, ep, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"tickets": tickets})
	}
	if len(tickets) == 0 {
		fmt.Fprintln(os.Stderr, "no support tickets on this account")
		return 0
	}
	w, flush := tableWriter("last_activity\tstate\tpriority\tid\tsubject")
	for _, t := range tickets {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			t.LastActivityAt.UTC().Format(time.RFC3339),
			t.State, t.Priority, t.ID, tabSafe(safeText(t.Subject)))
	}
	flush()
	return 0
}

func accountSupportShow(args []string) int {
	fs := flag.NewFlagSet("account support show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	ticketID := fs.String("ticket", "", "ticket id (required)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*ticketID) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself account support show --ticket TKT_ID")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	res, err := client.GetSupportTicket(ctx, ep, tok, *ticketID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(res)
	}
	fmt.Printf("ticket %s (%s / %s / %s)\n  subject: %s\n  opened:  %s by %s:%s\n",
		res.Ticket.ID, res.Ticket.Category, res.Ticket.State, res.Ticket.Priority,
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

func accountSupportReply(args []string) int {
	fs := flag.NewFlagSet("account support reply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	ticketID := fs.String("ticket", "", "ticket id (required)")
	body := fs.String("body", "", "reply body (required unless --body-file or --stdin)")
	bodyFile := fs.String("body-file", "", "read reply from FILE ('-' means stdin)")
	stdin := fs.Bool("stdin", false, "read reply from stdin")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*ticketID) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself account support reply --ticket TKT_ID (--body TEXT|--body-file FILE|--stdin)")
		return 2
	}
	text, err := readBodyFromFlags(*body, *bodyFile, *stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(os.Stderr, "witself: reply body is required (use --body, --body-file, or --stdin)")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	msg, err := client.ReplyToSupportTicket(ctx, ep, tok, *ticketID, text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"message": msg})
	}
	fmt.Printf("posted reply on %s (message %s)\n", *ticketID, msg.ID)
	return 0
}

func accountSupportClose(args []string) int {
	fs := flag.NewFlagSet("account support close", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	ticketID := fs.String("ticket", "", "ticket id (required)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*ticketID) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself account support close --ticket TKT_ID")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	// Only 'resolved' → 'closed' is a legal customer-facing final step.
	// The store rejects other transitions with 409, so we surface the
	// server's message verbatim.
	ticket, err := client.ChangeSupportTicketState(ctx, ep, tok, *ticketID, "closed")
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"ticket": ticket})
	}
	fmt.Printf("closed ticket %s\n", ticket.ID)
	return 0
}

// readBodyFromFlags resolves the description text from the three
// mutually-exclusive body sources shared by open + reply. Only one
// source may be set.
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

// accountList prints this machine's local account bindings — purely local,
// zero network, works when every token is dead. Its main job is finding an
// account id for recovery.
func accountList(args []string) int {
	fs := flag.NewFlagSet("account list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := local.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	names := make([]string, 0, len(cfg.Accounts))
	for name := range cfg.Accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	if *jsonOut {
		type row struct {
			Name  string `json:"name"`
			ID    string `json:"id"`
			Email string `json:"email,omitempty"`
		}
		rows := make([]row, 0, len(names))
		for _, name := range names {
			a := cfg.Accounts[name]
			rows = append(rows, row{Name: name, ID: a.ID, Email: a.Email})
		}
		return printJSON(map[string]any{"accounts": rows})
	}
	w, flush := tableWriter("name\tid\temail")
	for _, name := range names {
		a := cfg.Accounts[name]
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", name, a.ID, tabSafe(a.Email))
	}
	flush()
	return 0
}

// accountChangeEmail moves the account's contact email: a confirmation code
// proves the NEW inbox can receive, a notice warns the old one, and only the
// owner may commit. Routine and authenticated — unlike recovery, nothing is
// rotated.
func accountChangeEmail(args []string) int {
	fs := flag.NewFlagSet("account change-email", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	newEmail := fs.String("new-email", "", "the address to move the account to")
	code := fs.String("code", "", "confirmation code from the new address (second step)")
	endpoint := fs.String("endpoint", defaultControlPlane, "control plane URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *newEmail == "" {
		fmt.Fprintln(os.Stderr, "usage: witself account change-email --new-email EMAIL [--code CODE] [--account NAME]")
		return 2
	}
	name, acct, tok, err := local.Resolve(*account)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	ctx := context.Background()
	if *code == "" {
		if err := client.RequestEmailChange(ctx, *endpoint, acct.ID, tok, *newEmail); err != nil {
			// The request refuses if the alarm can't be delivered — the
			// old-address notice is the counter-move channel, so a mail
			// outage there fails the request rather than proceeding
			// silently.
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "a confirmation code was emailed to %s (valid ~15 minutes); a warning notice went to the current address so you'd see it if this wasn't you.\nnext: witself account change-email --new-email %s --code CODE\n", *newEmail, *newEmail)
		return 0
	}
	committed, err := client.RedeemEmailChange(ctx, *endpoint, acct.ID, tok, *newEmail, *code)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if err := local.SetEmail(name, committed); err != nil {
		fmt.Fprintf(os.Stderr, "witself: email changed to %s, but updating the local binding failed: %v\n", committed, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "email changed to %s for account %q (%s)\n", committed, name, acct.ID)
	return 0
}

// accountChangeDisplayName changes the account's server-side display name —
// cosmetic, owner-only, no email ceremony (unlike change-email, it isn't a
// security anchor). Local names are a different thing and live per-machine.
func accountChangeDisplayName(args []string) int {
	fs := flag.NewFlagSet("account change-display-name", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	displayName := strings.TrimSpace(fs.Arg(0))
	if displayName == "" {
		fmt.Fprintln(os.Stderr, `usage: witself account change-display-name [--account NAME] "New Display Name"`)
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if err := client.RenameAccount(ctx, ep, tok, displayName); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "display name changed to %q\n", displayName)
	return 0
}

// accountSuspend freezes an active account at the owner's request. Every
// domain endpoint refuses with 403 until resume; reads and status still
// work.
func accountSuspend(args []string) int {
	fs := flag.NewFlagSet("account suspend", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	reason := fs.String("reason", "", "optional free-text note for your own bookkeeping")
	yes := fs.Bool("yes", false, "confirm suspension")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "witself: suspend freezes every write on the account until you resume. Re-run with --yes to confirm.")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if err := client.SuspendAccount(ctx, ep, tok, *reason); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "account suspended. Resume with `witself account resume`.")
	return 0
}

// accountResume un-suspends an owner-initiated suspension. The store refuses
// to resume anything the owner did not suspend themselves.
func accountResume(args []string) int {
	fs := flag.NewFlagSet("account resume", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if err := client.ResumeAccount(ctx, ep, tok); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "account resumed.")
	return 0
}

// accountRecover is lost-token recovery: request a code to the account's
// email, then redeem it for a fresh owner credential. Requesting changes
// nothing; redeeming rotates every live root token.
func accountRecover(args []string) int {
	fs := flag.NewFlagSet("account recover", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := fs.String("account", "", `local account name (default: WITSELF_ACCOUNT or "default"; its binding supplies the id)`)
	id := fs.String("id", "", "raw account id (acc_...) when no local binding exists")
	code := fs.String("code", "", "recovery code from the email (second step)")
	name := fs.String("name", "", "local name to save the recovered credential under (only with --id and no existing binding)")
	endpoint := fs.String("endpoint", defaultControlPlane, "control plane URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *account != "" && *id != "" {
		fmt.Fprintln(os.Stderr, "usage: witself account recover [--account NAME | --id acc_ID] [--code CODE] [--name NEWNAME]")
		return 2
	}

	cfg, err := local.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	var accountID, targetName string
	if *id == "" {
		// Standard reference resolution: flag, then env, then "default".
		lookup := *account
		if lookup == "" {
			lookup = strings.TrimSpace(os.Getenv("WITSELF_ACCOUNT"))
		}
		if lookup == "" {
			lookup = "default"
		}
		a, ok := cfg.Accounts[lookup]
		if !ok {
			fmt.Fprintf(os.Stderr, "witself: no local account named %q (try `witself account list`, or --id acc_ID)\n", lookup)
			return 1
		}
		accountID, targetName = a.ID, lookup
	} else {
		if !strings.HasPrefix(*id, "acc_") {
			fmt.Fprintln(os.Stderr, "witself: --id takes a raw account id (acc_...)")
			return 2
		}
		accountID = *id
		for n, a := range cfg.Accounts {
			if a.ID == accountID {
				targetName = n // an existing binding refreshes in place
				break
			}
		}
		if targetName == "" && *name != "" {
			// New local name for this id — refuse a name that already binds
			// a DIFFERENT account, exactly like `witself account create --name`.
			if a, taken := cfg.Accounts[*name]; taken && a.ID != accountID {
				fmt.Fprintf(os.Stderr, "witself: local name %q already binds %s — pick another --name\n", *name, a.ID)
				return 1
			}
			targetName = *name
		}
	}

	ctx := context.Background()
	if *code == "" {
		if err := client.RequestRecovery(ctx, *endpoint, accountID); err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "if %s exists and is active, a recovery code was emailed to its address (valid ~15 minutes).\nnext: re-run with --code CODE\n", accountID)
		return 0
	}

	acct, err := client.RedeemRecovery(ctx, *endpoint, accountID, *code)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	res, err := client.BootstrapLogin(ctx, acct.Cell.Endpoint, acct.BootstrapToken)
	if err != nil {
		// The recovery code is spent server-side, so we can't try again
		// with it. Surface the bootstrap token so the user can finish the
		// exchange by hand instead of burning another quota slot.
		fmt.Fprintf(os.Stderr, "witself: recovered but login failed: %v\n"+
			"    finish by hand with `witself auth login --endpoint %s --bootstrap-token-file FILE`\n"+
			"    bootstrap token (expires soon, shown once):\n", err, acct.Cell.Endpoint)
		fmt.Println(acct.BootstrapToken)
		return 1
	}
	_, hasBinding := cfg.Accounts[targetName]
	switch {
	case targetName != "" && hasBinding:
		if err := local.RefreshToken(targetName, res.OperatorToken); err != nil {
			fmt.Fprintf(os.Stderr, "witself: saving recovered token: %v\n", err)
			fmt.Println(res.OperatorToken) // never strand the only credential
			return 1
		}
	case targetName != "":
		if err := local.Save(targetName, local.Account{ID: acct.AccountID, Email: acct.Email}, res.OperatorToken); err != nil {
			fmt.Fprintf(os.Stderr, "witself: saving recovered token: %v\n", err)
			fmt.Println(res.OperatorToken)
			return 1
		}
	default:
		fmt.Fprintln(os.Stderr, "witself: no local binding for this id — pass --name to save the credential; printing it once:")
		fmt.Println(res.OperatorToken)
		return 0
	}
	fmt.Fprintf(os.Stderr, "recovered — new owner token saved as %q; the old owner tokens are revoked\n", targetName)
	return 0
}

// accountResendVerification emails a fresh verification link for a
// still-pending account. Proof of ownership is the operator token the CLI
// already holds from signup; the control plane checks with the cell and only
// a still-pending answer sends.
func accountResendVerification(args []string) int {
	fs := flag.NewFlagSet("account resend-verification", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", defaultControlPlane, "control plane URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	name, acct, tok, err := local.Resolve(*account)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	email, err := client.ResendVerification(context.Background(), *endpoint, acct.ID, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		if strings.Contains(err.Error(), "unknown account") {
			fmt.Fprintf(os.Stderr, "    (it may have been closed for missing the verification window — `witself account forget --account %s --yes` removes the local name)\n", name)
		}
		return 1
	}
	fmt.Fprintf(os.Stderr, "verification email re-sent to %s for account %q (%s)\n", email, name, acct.ID)
	return 0
}

// accountStatus reads the account's lifecycle record from its cell. It works
// at any status — its main job is watching a pending account for activation.
func accountStatus(args []string) int {
	fs := flag.NewFlagSet("account status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	jsonOut := jsonFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		var arch *client.ErrAccountArchived
		if errors.As(err, &arch) {
			// The account exists in the fleet's memory but is not placed on
			// a live cell — no round trip possible. Report from the
			// directory's own answer.
			_, acct, _, rerr := local.Resolve(*account)
			id := ""
			if rerr == nil {
				id = acct.ID
			}
			if *jsonOut {
				return printJSON(map[string]any{"account": map[string]any{
					"id":              id,
					"status":          "archived",
					"archived_from":   arch.Cell,
					"archived_region": arch.Region,
					"archived_object": arch.Object,
					"exported_at":     arch.ExportedAt,
				}})
			}
			printStatusPairs([][2]string{
				{"account", id},
				{"status", "archived"},
				{"archived from", tabSafe(safeText(arch.Cell))},
				{"exported at", arch.ExportedAt.UTC().Format(time.RFC3339)},
			})
			fmt.Fprintln(os.Stderr, "\narchived — awaiting placement on a new cell")
			return 0
		}
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	rec, err := client.GetAccount(ctx, ep, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	// Support summary is a second call, only made when the account's
	// support_policy actually enables it. Free-tier / enterprise-off
	// accounts skip the roundtrip entirely.
	var supportLine string
	if rec.SupportPolicy == "enabled" && rec.Status == "active" {
		tickets, terr := client.ListSupportTickets(ctx, ep, tok)
		if terr == nil {
			supportLine = summarizeSupport(tickets)
		}
		// Silent failure is deliberate here — a broken support listing
		// must not hide the account status the operator asked for.
	}
	if *jsonOut {
		out := map[string]any{"account": rec}
		if supportLine != "" {
			out["support_summary"] = supportLine
		}
		return printJSON(out)
	}
	pairs := [][2]string{
		{"account", rec.ID},
		{"status", rec.Status},
	}
	if rec.DisplayName != "" {
		pairs = append(pairs, [2]string{"name", tabSafe(safeText(rec.DisplayName))})
	}
	if rec.Email != "" {
		pairs = append(pairs, [2]string{"email", tabSafe(safeText(rec.Email))})
	}
	if supportLine != "" {
		pairs = append(pairs, [2]string{"support", supportLine})
	}
	printStatusPairs(pairs)
	if rec.Status == "pending" {
		fmt.Fprintln(os.Stderr, "\npending: activation required before the account can be used")
	}
	if rec.Status == "suspended" && rec.SuspendedAt != nil {
		fmt.Fprintf(os.Stderr, "\nsuspended %s (%s)%s\n",
			rec.SuspendedAt.UTC().Format(time.RFC3339),
			suspendedForLabel(rec.SuspendedFor),
			closedReasonSuffix(rec.SuspendedReason))
		if rec.SuspendedFor == "owner_request" {
			fmt.Fprintln(os.Stderr, "resume with: witself account resume")
		}
	}
	if rec.ClosedAt != nil {
		fmt.Fprintf(os.Stderr, "\nclosed %s%s\n", rec.ClosedAt.UTC().Format(time.RFC3339), closedReasonSuffix(rec.ClosedReason))
	}
	return 0
}

// printStatusPairs renders a describe-style key/value list to stdout.
// Column-aligned with two spaces after the colon; keeps single-record
// output readable without pretending to be a table (a single row
// through tableWriter looks like an accident).
func printStatusPairs(pairs [][2]string) {
	width := 0
	for _, p := range pairs {
		if n := len(p[0]); n > width {
			width = n
		}
	}
	for _, p := range pairs {
		fmt.Printf("%-*s  %s\n", width+1, p[0]+":", p[1])
	}
}

// summarizeSupport returns the "N open (K awaiting your reply)" line
// the status command shows when the account's support_policy is
// enabled. K counts tickets awaiting_customer — that's the "ball is
// in your court" bucket — so the operator knows at a glance whether
// support is actually waiting on them.
func summarizeSupport(tickets []client.SupportTicket) string {
	open, awaitingCustomer := 0, 0
	for _, t := range tickets {
		if t.State == "closed" {
			continue
		}
		open++
		if t.State == "awaiting_customer" {
			awaitingCustomer++
		}
	}
	if open == 0 {
		return "no open tickets"
	}
	if awaitingCustomer == 0 {
		if open == 1 {
			return "1 open ticket"
		}
		return fmt.Sprintf("%d open tickets", open)
	}
	if open == 1 {
		return "1 open ticket (awaiting your reply)"
	}
	return fmt.Sprintf("%d open tickets (%d awaiting your reply)", open, awaitingCustomer)
}

func suspendedForLabel(s string) string {
	if s == "" {
		return "unknown"
	}
	return strings.ReplaceAll(s, "_", " ")
}

func closedReasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}

// accountClose permanently closes an account. The account's data remains as a
// tombstone forever; routing and every credential die now. Owner-only, and it
// demands --yes because there is no undo. On success the local name (if one
// was used) is retired too.
func accountClose(args []string) int {
	fs := flag.NewFlagSet("account close", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := fs.String("account", "", "local account name (default \"default\"), or a raw acc_ id with --token-file")
	tokenFile := fs.String("token-file", "", "file containing the account owner's operator token (with a raw acc_ id)")
	endpoint := fs.String("endpoint", defaultControlPlane, "control plane URL")
	reason := fs.String("reason", "", "optional close reason, recorded on the account")
	yes := fs.Bool("yes", false, "confirm: closing is permanent")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var accountID, tok, localName string
	if *tokenFile != "" {
		// Raw path: explicit id + token file, no local state involved.
		if !strings.HasPrefix(*account, "acc_") {
			fmt.Fprintln(os.Stderr, "usage: witself account close --account acc_ID --token-file FILE [--reason TEXT] --yes")
			return 2
		}
		t, err := readToken(*tokenFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		accountID, tok = *account, t
	} else {
		if strings.HasPrefix(*account, "acc_") {
			fmt.Fprintln(os.Stderr, "witself: a raw account id needs --token-file; local names never start with acc_")
			return 2
		}
		name, acct, t, err := local.Resolve(*account)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		accountID, tok, localName = acct.ID, t, name
	}

	if !*yes {
		fmt.Fprintf(os.Stderr, "witself: closing %s is permanent — its credentials will be revoked and the account will be retired.\n    Nothing has changed yet; re-run with --yes to confirm.\n", accountID)
		return 2
	}
	if err := client.CloseAccount(context.Background(), *endpoint, accountID, tok, *reason); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "account %s closed. Its operator token is now dead.\n", accountID)
	if localName != "" {
		if err := local.Delete(localName); err != nil {
			fmt.Fprintf(os.Stderr, "witself: retiring local name %q: %v\n", localName, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "local account %q removed.\n", localName)
	}
	return 0
}

// accountForget removes a LOCAL account binding — the config.json entry and
// accountEvents prints the owner's audit trail — every security-relevant
// mutation on the account, filterable and paginated. Owner-only: a
// non-owner operator token is refused with 403.
func accountEvents(args []string) int {
	fs := flag.NewFlagSet("account events", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	since := fs.String("since", "", "only events at or after this RFC3339 timestamp (e.g. 2026-01-02T00:00:00Z)")
	until := fs.String("until", "", "only events at or before this RFC3339 timestamp")
	verb := fs.String("verb", "", "filter by exact verb (e.g. account.email.changed)")
	limit := fs.Int("limit", 50, "max events per page (1-500)")
	after := fs.String("after", "", "opaque cursor from a previous page's next_cursor")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	q := client.EventsQuery{Verb: *verb, Limit: *limit, Cursor: *after}
	if *since != "" {
		t, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: --since must be RFC3339: %v\n", err)
			return 2
		}
		q.Since = &t
	}
	if *until != "" {
		t, err := time.Parse(time.RFC3339, *until)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: --until must be RFC3339: %v\n", err)
			return 2
		}
		q.Until = &t
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	page, err := client.ListAccountEvents(ctx, ep, tok, q)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(page)
	}
	if len(page.Events) == 0 {
		fmt.Fprintln(os.Stderr, "no events matching your filter")
		return 0
	}
	// Human-readable table: time / verb / actor / metadata as one line.
	// Sorted newest-first (server returns in that order).
	fmt.Println("occurred_at\tverb\tactor\tmetadata")
	for _, e := range page.Events {
		actor := e.ActorKind
		if e.ActorID != "" {
			actor = e.ActorKind + ":" + e.ActorID
		}
		fmt.Printf("%s\t%s\t%s\t%s\n",
			e.OccurredAt.UTC().Format(time.RFC3339),
			e.Verb, actor, string(e.Metadata))
	}
	if page.NextCursor != "" {
		fmt.Fprintf(os.Stderr, "\n%d events shown; more available. Continue with:\n  witself account events --after %s\n",
			len(page.Events), page.NextCursor)
	}
	return 0
}

// the token file — without ever contacting a server. It exists for stranded
// names: when the control plane closes an account the CLI didn't see close
// (the pending-account reaper, a torn-down cell), the credentials are already
// dead, so `witself account close` can't clean up the local half. Closing a live
// account is `witself account close`, which retires the local name itself.
func accountForget(args []string) int {
	fs := flag.NewFlagSet("account forget", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := fs.String("account", "", "local account name to forget (required)")
	yes := fs.Bool("yes", false, "confirm forgetting the local binding")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// No WITSELF_ACCOUNT or "default" fallback: forgetting always names its
	// target explicitly, so a stray invocation can't drop a live credential.
	if *account == "" {
		fmt.Fprintln(os.Stderr, "usage: witself account forget --account NAME --yes")
		return 2
	}
	// Available answering "taken" is exactly what forget needs: the name is
	// bound locally (config entry or token file), even if only half survives.
	switch err := local.Available(*account); {
	case err == nil:
		fmt.Fprintf(os.Stderr, "witself: no local account named %q\n", *account)
		return 1
	case !errors.Is(err, local.ErrNameTaken):
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if !*yes {
		fmt.Fprintf(os.Stderr, "witself: forgetting %q removes this machine's binding and token only — it does NOT close the account server-side (that is `witself account close`).\n    Nothing has changed yet; re-run with --yes to confirm.\n", *account)
		return 2
	}
	if err := local.Delete(*account); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "local account %q forgotten. The account itself, if it still exists, is untouched server-side.\n", *account)
	return 0
}

// accountCreate is Witself Cloud signup: one command from nothing to a working
// operator token. The control plane places the account on a cell; the CLI then
// claims it with the ordinary bootstrap exchange — the same path a self-hosted
// bootstrap uses — and remembers it under a local name so later commands are
// just `witself realm create --account NAME ...`.
func accountCreate(args []string) int {
	fs := flag.NewFlagSet("account create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	email := fs.String("email", "", "account owner email")
	invite := fs.String("invite", "", "invite code")
	name := fs.String("name", "", `local name for the new account (default "default")`)
	displayName := fs.String("display-name", "", "account display name (default: the email)")
	endpoint := fs.String("endpoint", defaultControlPlane, "control plane URL")
	out := fs.String("out", "", "also write the operator token to this file (0600)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *email == "" || *invite == "" {
		fmt.Fprintln(os.Stderr, "usage: witself account create --email EMAIL --invite CODE [--name LOCALNAME] [--display-name NAME] [--endpoint URL] [--out FILE]")
		return 2
	}
	localName := *name
	if localName == "" {
		localName = "default"
	}
	// Claim the local name BEFORE creating anything remote: a taken name must
	// not strand a freshly provisioned account's only credential.
	if err := local.Available(localName); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}

	ctx := context.Background()
	acct, err := client.CreateAccount(ctx, *endpoint, *email, *invite, *displayName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "account %s created on cell %s (%s)\n", acct.AccountID, acct.Cell.Name, acct.Cell.Endpoint)

	// Claim it: the same exchange a self-hosted bootstrap uses.
	res, err := client.BootstrapLogin(ctx, acct.Cell.Endpoint, acct.BootstrapToken)
	if err != nil {
		// Never strand the only credential: the bootstrap token is valid for
		// about an hour — surface it with the finish-by-hand recipe instead
		// of abandoning a freshly provisioned account to the reaper.
		fmt.Fprintf(os.Stderr, "witself: account created but login failed: %v\n"+
			"    finish by hand: (1) save the token below to a file, then\n"+
			"    (2) witself auth login --endpoint %s --bootstrap-token-file FILE --out op.token\n"+
			"    (3) witself account adopt --id %s --token-file op.token --name %s\n"+
			"    bootstrap token (expires in ~1 hour, shown once):\n",
			err, acct.Cell.Endpoint, acct.AccountID, localName)
		fmt.Println(acct.BootstrapToken)
		return 1
	}
	fmt.Fprintf(os.Stderr, "logged in as operator %s\n", res.OperatorID)

	if err := local.Save(localName, local.Account{ID: acct.AccountID, Email: acct.Email}, res.OperatorToken); err != nil {
		// Never strand the only credential: surface the token if we can't store it.
		fmt.Fprintf(os.Stderr, "witself: saving local account: %v\n", err)
		fmt.Println(res.OperatorToken)
		return 1
	}
	fmt.Fprintf(os.Stderr, "saved locally as %q\n", localName)

	if *out != "" {
		if err := os.WriteFile(*out, []byte(res.OperatorToken+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "witself: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "wrote operator token to %s\n", *out)
	}
	accountRef := ""
	if localName != "default" {
		accountRef = " --account " + localName
	}
	switch {
	case acct.Status == "active":
		fmt.Fprintf(os.Stderr, "next: witself realm create%s NAME\n", accountRef)
	case acct.EmailSent:
		fmt.Fprintf(os.Stderr, "account is %s — a verification link was emailed to %s\nafter clicking it: witself account status%s\n", acct.Status, acct.Email, accountRef)
	default:
		fmt.Fprintf(os.Stderr, "account is %s — activation required before use\nnext: witself account status%s\n", acct.Status, accountRef)
	}
	return 0
}

// accountAdopt binds an EXISTING account — an id plus an operator token the
// user already holds — under a local name. This is how a credential that
// arrived from elsewhere (a teammate-minted operator token, a pre-local-names
// --out file, a second machine for the same account) escapes permanent
// --endpoint/--token-file ceremony. The token is verified against the
// account's cell — it must authenticate AND belong to the given account —
// before anything is written.
func accountAdopt(args []string) int {
	fs := flag.NewFlagSet("account adopt", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	id := fs.String("id", "", "account id (acc_...)")
	tokenFile := fs.String("token-file", "", "file containing the account's operator token")
	name := fs.String("name", "", "local name for the account (required)")
	endpoint := fs.String("endpoint", defaultControlPlane, "control plane URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// No WITSELF_ACCOUNT or "default" fallback: adopting always names its
	// target explicitly.
	if !strings.HasPrefix(*id, "acc_") || *tokenFile == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "usage: witself account adopt --id acc_ID --token-file FILE --name NAME")
		return 2
	}
	// Claim the local name before any round trip, so a taken name fails fast.
	if err := local.Available(*name); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if tok == "" {
		fmt.Fprintf(os.Stderr, "witself: token file %s is empty\n", *tokenFile)
		return 1
	}

	// Verify before binding: the directory says which cell hosts the account,
	// the cell says whether the token authenticates — and to which account.
	ctx := context.Background()
	_, cellEndpoint, err := client.LookupAccount(ctx, *endpoint, *id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: locate account %s: %v\n", *id, err)
		return 1
	}
	rec, err := client.GetAccount(ctx, cellEndpoint, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: verify token against %s: %v\n", cellEndpoint, err)
		return 1
	}
	if rec.ID != *id {
		fmt.Fprintf(os.Stderr, "witself: token belongs to account %s, not %s\n", rec.ID, *id)
		return 1
	}

	// Bind what the cell reported. Email may be empty (the seeded default
	// account has none) — the binding stores it only when present.
	if err := local.Save(*name, local.Account{ID: rec.ID, Email: rec.Email}, tok); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "adopted account %s (%s) as %q\n", rec.ID, rec.Status, *name)
	return 0
}

func selfCmd(args []string) int {
	if len(args) == 0 || args[0] != "show" {
		fmt.Fprintln(os.Stderr, "usage: witself self show [--account NAME] [--realm NAME] (--agent NAME | --endpoint URL --token-file FILE)")
		return 2
	}
	return selfShow(args[1:])
}

func selfShow(args []string) int {
	fs := flag.NewFlagSet("self show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	realm := fs.String("realm", "", `local realm name (default: WITSELF_REALM or "default")`)
	agent := fs.String("agent", "", "local agent name (default: WITSELF_AGENT)")
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an agent token")
	noFacts := fs.Bool("no-facts", false, "omit primary facts")
	noSalient := fs.Bool("no-salient", false, "omit salient memories")
	salientLimit := fs.Int("salient-limit", 10, "maximum salient memories")
	maxBytes := fs.Int("max-bytes", 8192, "maximum digest size")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself self show [--account NAME] [--realm NAME] (--agent NAME | --endpoint URL --token-file FILE)")
		return 2
	}
	if *salientLimit < 1 || *salientLimit > 100 {
		fmt.Fprintln(os.Stderr, "witself: --salient-limit must be between 1 and 100")
		return 2
	}
	if *maxBytes < 1024 || *maxBytes > 65536 {
		fmt.Fprintln(os.Stderr, "witself: --max-bytes must be between 1024 and 65536")
		return 2
	}

	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	digest, err := client.GetSelf(ctx, conn.Endpoint, conn.Token, client.SelfOptions{
		IncludeFacts:    !*noFacts,
		IncludeSalient:  !*noSalient,
		SalientLimit:    *salientLimit,
		MaximumByteSize: *maxBytes,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if conn.AccountID != "" && digest.Identity.AccountID != conn.AccountID {
		fmt.Fprintf(os.Stderr, "witself: agent token belongs to account %s, not local account %s (%s)\n",
			digest.Identity.AccountID, conn.AccountName, conn.AccountID)
		return 1
	}
	if conn.RealmName != "" && digest.Identity.RealmName != conn.RealmName {
		fmt.Fprintf(os.Stderr, "witself: agent token belongs to realm %q, not %q\n", digest.Identity.RealmName, conn.RealmName)
		return 1
	}
	if conn.AgentName != "" && digest.Identity.AgentName != conn.AgentName {
		fmt.Fprintf(os.Stderr, "witself: agent token belongs to agent %q, not %q\n", digest.Identity.AgentName, conn.AgentName)
		return 1
	}
	if *jsonOut {
		return printJSON(digest)
	}
	fmt.Printf("agent:\t%s (%s)\n", safeText(digest.Identity.AgentName), digest.Identity.AgentID)
	fmt.Printf("realm:\t%s (%s)\n", safeText(digest.Identity.RealmName), digest.Identity.RealmID)
	fmt.Printf("account:\t%s\n", digest.Identity.AccountID)
	fmt.Printf("facts:\t%d\n", digest.Index.Counts["facts"])
	fmt.Printf("memories:\t%d\n", digest.Index.Counts["memories"])
	if digest.Elided {
		fmt.Println("elided:\ttrue")
	}
	return 0
}

func transcriptCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself transcript create|append|list|show ...")
		return 2
	}
	switch args[0] {
	case "create":
		return transcriptCreate(args[1:])
	case "append":
		return transcriptAppend(args[1:])
	case "list":
		return transcriptList(args[1:])
	case "show":
		return transcriptShow(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself transcript: unknown subcommand %q\n", args[0])
		return 2
	}
}

func transcriptCreate(args []string) int {
	fs := flag.NewFlagSet("transcript create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an agent token")
	title := fs.String("title", "", "short transcript title")
	externalID := fs.String("external-id", "", "runtime/vendor conversation id")
	metadataFile := fs.String("metadata-file", "", "file containing a small JSON object")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	metadata, err := readJSONFile(*metadataFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connectDomain(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	tr, err := client.CreateTranscript(ctx, ep, tok, client.CreateTranscriptInput{
		ExternalID: *externalID,
		Title:      *title,
		Metadata:   metadata,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"transcript": tr})
	}
	fmt.Printf("%s\t%s\n", tr.ID, tabSafe(safeText(tr.Title)))
	return 0
}

func transcriptAppend(args []string) int {
	transcriptID := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		transcriptID = strings.TrimSpace(args[0])
		args = args[1:]
	}
	fs := flag.NewFlagSet("transcript append", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an agent token")
	role := fs.String("role", "", "user|assistant|system|tool (required)")
	body := fs.String("body", "", "visible entry text")
	bodyFile := fs.String("body-file", "", "read visible text from FILE ('-' means stdin)")
	stdin := fs.Bool("stdin", false, "read visible text from stdin")
	payloadFile := fs.String("payload-file", "", "file containing a small JSON object")
	externalID := fs.String("external-id", "", "runtime/vendor message id for idempotency")
	model := fs.String("model", "", "optional model/version label")
	replyTo := fs.String("reply-to", "", "earlier entry id in this transcript")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if transcriptID == "" {
		transcriptID = strings.TrimSpace(fs.Arg(0))
	}
	if transcriptID == "" || strings.TrimSpace(*role) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself transcript append TRANSCRIPT_ID --role user|assistant|system|tool (--body TEXT|--body-file FILE|--stdin|--payload-file FILE)")
		return 2
	}
	text, err := readBodyFromFlags(*body, *bodyFile, *stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	payload, err := readJSONFile(*payloadFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if text == "" && len(payload) == 0 {
		fmt.Fprintln(os.Stderr, "witself: --body/--body-file/--stdin or --payload-file is required")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connectDomain(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	entry, err := client.AppendTranscriptEntry(ctx, ep, tok, transcriptID, client.AppendTranscriptEntryInput{
		ExternalID:     *externalID,
		Role:           *role,
		Body:           text,
		Payload:        payload,
		Model:          *model,
		ReplyToEntryID: *replyTo,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"entry": entry})
	}
	fmt.Printf("%d\t%s\t%s\n", entry.Sequence, entry.Role, entry.ID)
	return 0
}

func transcriptList(args []string) int {
	fs := flag.NewFlagSet("transcript list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an agent or operator token")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connectDomain(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	transcripts, err := client.ListTranscripts(ctx, ep, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"transcripts": transcripts})
	}
	if len(transcripts) == 0 {
		fmt.Fprintln(os.Stderr, "no transcripts")
		return 0
	}
	w, flush := tableWriter("updated\tid\tagent\ttitle")
	for _, tr := range transcripts {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			formatTime(tr.UpdatedAt), tr.ID, tr.OwnerAgentID, tabSafe(safeText(tr.Title)))
	}
	flush()
	return 0
}

func transcriptShow(args []string) int {
	transcriptID := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		transcriptID = strings.TrimSpace(args[0])
		args = args[1:]
	}
	fs := flag.NewFlagSet("transcript show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an agent or operator token")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if transcriptID == "" {
		transcriptID = strings.TrimSpace(fs.Arg(0))
	}
	if transcriptID == "" {
		fmt.Fprintln(os.Stderr, "usage: witself transcript show TRANSCRIPT_ID")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connectDomain(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	detail, err := client.GetTranscript(ctx, ep, tok, transcriptID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(detail)
	}
	fmt.Printf("transcript %s", detail.Transcript.ID)
	if detail.Transcript.Title != "" {
		fmt.Printf(" - %s", safeText(detail.Transcript.Title))
	}
	fmt.Printf("\nagent: %s\ncreated: %s\n\n", detail.Transcript.OwnerAgentID, formatTime(detail.Transcript.CreatedAt))
	for _, entry := range detail.Entries {
		fmt.Printf("--- %d  %s  %s  %s ---\n", entry.Sequence, entry.Role, entry.ID, formatTime(entry.CreatedAt))
		if entry.Body != "" {
			fmt.Printf("%s\n", safeText(entry.Body))
		}
		if len(entry.Payload) > 0 && string(entry.Payload) != "null" {
			fmt.Printf("payload: %s\n", safeText(string(entry.Payload)))
		}
		fmt.Println()
	}
	return 0
}

func readJSONFile(path string) (json.RawMessage, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read JSON file: %w", err)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%s does not contain valid JSON", path)
	}
	return json.RawMessage(raw), nil
}

func usage(w io.Writer) {
	usageLine(w, "witself — the Witself CLI (alias: ws)")
	usageLine(w)
	usageLine(w, "Usage:")
	usageLine(w, "  witself version              Print version information")
	usageLine(w, "  witself gen-bootstrap-token  Generate an operator bootstrap token")
	usageLine(w, "  witself auth login           Exchange a bootstrap token for an operator token")
	usageLine(w, "  witself account create       Create a Witself Cloud account (invite required)")
	usageLine(w, "  witself account adopt        Bind an existing account (id + token) to a local name")
	usageLine(w, "  witself account list         List this machine's local account names")
	usageLine(w, "  witself account status       Show an account's lifecycle status")
	usageLine(w, "  witself account placement    Show or change account cell-placement policy")
	usageLine(w, "  witself account recover      Email a recovery code, redeem it for a fresh owner token")
	usageLine(w, "  witself account change-email Move the account to a new address (code-confirmed)")
	usageLine(w, "  witself account change-display-name  Rename the account (owner only)")
	usageLine(w, "  witself account suspend      Freeze every write on the account (owner only)")
	usageLine(w, "  witself account resume       Un-freeze an owner-suspended account")
	usageLine(w, "  witself account resend-verification  Email a fresh verification link")
	usageLine(w, "  witself account close        Permanently close an account (owner only)")
	usageLine(w, "  witself account forget       Remove a local account binding (server untouched)")
	usageLine(w, "  witself realm create|list|delete")
	usageLine(w, "  witself agent create|list|delete")
	usageLine(w, "  witself operator list|create|delete")
	usageLine(w, "  witself token create|revoke  Mint or revoke agent/operator tokens")
	usageLine(w, "  witself self show            Show the token-bound agent identity and self digest")
	usageLine(w, "  witself transcript create|append|list|show  Record visible AI interactions")
	usageLine(w, "  witself help                 Show this help")
	usageLine(w)
	usageLine(w, "Cloud commands take --account NAME (a local account name; when omitted,")
	usageLine(w, `WITSELF_ACCOUNT or "default"). Self-hosted: --endpoint URL --token-file FILE.`)
}

func usageLine(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}
