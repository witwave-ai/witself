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
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
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
		fmt.Println(version.String("ws"))
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
	case "operator":
		return operatorCmd(args[1:])
	case "token":
		return tokenCmd(args[1:])
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "ws: unknown command %q\n\n", args[0])
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
			fmt.Fprintf(os.Stderr, "ws: %v\n", err)
			return 1
		}
		*out = p
	}

	tok, err := token.New(token.KindBootstrap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}

	if *out == "" {
		fmt.Println(tok)
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if err := os.WriteFile(*out, []byte(tok+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
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
		fmt.Fprintln(os.Stderr, "usage: ws auth login --endpoint URL --bootstrap-token-file FILE [--out FILE]")
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
		fmt.Fprintln(os.Stderr, "ws: --endpoint and --bootstrap-token-file are required")
		return 2
	}

	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	res, err := client.BootstrapLogin(context.Background(), *endpoint, strings.TrimSpace(string(raw)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "logged in as operator %s\n", res.OperatorID)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(res.OperatorToken+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "ws: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	return 0
}

func realmCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ws realm create|list|delete [--account NAME]")
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
		fmt.Fprintf(os.Stderr, "ws realm: unknown subcommand %q\n", args[0])
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
		fmt.Fprintln(os.Stderr, "usage: ws realm create [--account NAME] NAME")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	r, err := client.CreateRealm(ctx, ep, tok, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
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
		fmt.Fprintln(os.Stderr, "usage: ws realm delete [--account NAME] --yes REALM")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "ws: refusing to delete realm without --yes")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if err := client.DeleteRealm(ctx, ep, tok, realmID); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	realms, err := client.ListRealms(ctx, ep, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if realms == nil {
		realms = []client.Realm{}
	}
	if *jsonOut {
		return printJSON(map[string]any{"realms": realms})
	}
	fmt.Fprintln(os.Stderr, "id\tname")
	for _, r := range realms {
		fmt.Printf("%s\t%s\n", r.ID, r.Name)
	}
	return 0
}

func agentCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ws agent create|list|delete [--account NAME] --realm REALM")
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
		fmt.Fprintf(os.Stderr, "ws agent: unknown subcommand %q\n", args[0])
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
		fmt.Fprintln(os.Stderr, "usage: ws agent create [--account NAME] --realm REALM NAME")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	a, err := client.CreateAgent(ctx, ep, tok, *realm, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
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
		fmt.Fprintln(os.Stderr, "usage: ws agent list [--account NAME] --realm REALM")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	agents, err := client.ListAgents(ctx, ep, tok, *realm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if agents == nil {
		agents = []client.Agent{}
	}
	if *jsonOut {
		return printJSON(map[string]any{"agents": agents})
	}
	fmt.Fprintln(os.Stderr, "id\tname")
	for _, a := range agents {
		fmt.Printf("%s\t%s\n", a.ID, a.Name)
	}
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
		fmt.Fprintln(os.Stderr, "usage: ws agent delete [--account NAME] --realm REALM --yes AGENT")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "ws: refusing to delete agent without --yes")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if err := client.DeleteAgent(ctx, ep, tok, *realm, agentID); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "deleted agent %s\n", agentID)
	return 0
}

func operatorCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ws operator list|create|delete [--account NAME]")
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
		fmt.Fprintf(os.Stderr, "ws operator: unknown subcommand %q\n", args[0])
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
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	operators, err := client.ListOperators(ctx, ep, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
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
	fmt.Fprintln(os.Stderr, "id\tname\trole\troot\tcreated\tupdated\ttokens")
	for _, op := range operators {
		fmt.Printf("%s\t%s\t%s\t%t\t%s\t%s\t%s\n",
			op.ID,
			tabSafe(op.DisplayName),
			op.Role,
			op.IsRoot,
			formatTime(op.CreatedAt),
			formatTime(op.UpdatedAt),
			operatorTokenSummary(op.Tokens),
		)
	}
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
		fmt.Fprintln(os.Stderr, "usage: ws operator create [--account NAME] --name NAME [--token-name NAME] [--ttl DURATION] [--out FILE]")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	res, err := client.CreateOperator(ctx, ep, tok, *name, *tokenName, *ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	tokenID := "-"
	if len(res.Operator.Tokens) > 0 {
		tokenID = res.Operator.Tokens[0].ID
	}
	fmt.Fprintf(os.Stderr, "created operator %s (%s), token %s\n", res.Operator.ID, res.Operator.DisplayName, tokenID)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(res.OperatorToken+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "ws: %v\n", err)
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
		fmt.Fprintln(os.Stderr, "usage: ws operator delete [--account NAME] --yes OPERATOR")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "ws: refusing to delete operator without --yes")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if err := client.DeleteOperator(ctx, ep, tok, operatorID); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "deleted operator %s\n", operatorID)
	return 0
}

func tokenCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ws token create|revoke [--account NAME]")
		return 2
	}
	switch args[0] {
	case "create":
		return tokenCreate(args[1:])
	case "revoke":
		return tokenRevoke(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ws token: unknown subcommand %q\n", args[0])
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
		fmt.Fprintln(os.Stderr, "usage: ws token create [--account NAME] (--agent AGENT | --operator) [--name NAME] [--ttl DURATION] [--out FILE]")
		return 2
	}
	if *agent != "" && *ttl != "" {
		fmt.Fprintln(os.Stderr, "ws: --ttl is currently supported only with --operator")
		return 2
	}
	if *agent != "" && *name != "" {
		fmt.Fprintln(os.Stderr, "ws: --name is currently supported only with --operator")
		return 2
	}
	ctx := context.Background()
	ep, op, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if *operator {
		res, err := client.CreateOperatorToken(ctx, ep, op, *name, *ttl)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ws: %v\n", err)
			return 1
		}
		if res.TokenID != "" {
			fmt.Fprintf(os.Stderr, "created operator token %s\n", res.TokenID)
		}
		if *out != "" {
			if err := os.WriteFile(*out, []byte(res.OperatorToken+"\n"), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "ws: %v\n", err)
				return 1
			}
			fmt.Fprintf(os.Stderr, "wrote operator token for %s to %s\n", res.OperatorID, *out)
			return 0
		}
		fmt.Println(res.OperatorToken)
		return 0
	}

	agentTok, tokenID, err := client.CreateAgentToken(ctx, ep, op, *agent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if tokenID != "" {
		fmt.Fprintf(os.Stderr, "created agent token %s\n", tokenID)
	}
	if *out != "" {
		if err := os.WriteFile(*out, []byte(agentTok+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "ws: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "wrote agent token to %s\n", *out)
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
	yes := fs.Bool("yes", false, "confirm token revocation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tokenID == "" {
		fmt.Fprintln(os.Stderr, "usage: ws token revoke [--account NAME] --token TOKEN_ID --yes")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "ws: refusing to revoke token without --yes")
		return 2
	}
	ctx := context.Background()
	ep, tok, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if err := client.RevokeToken(ctx, ep, tok, *tokenID); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "revoked token %s\n", *tokenID)
	return 0
}

func readToken(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
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

// defaultControlPlane is the Witself Cloud front door: the one address a Cloud
// user ever needs. Self-hosted deployments never contact it.
const defaultControlPlane = "https://self.witwave.ai"

// accountCmd handles `ws account ...`.
func accountCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ws account create|adopt|status|resend-verification|close|forget ...")
		return 2
	}
	switch args[0] {
	case "create":
		return accountCreate(args[1:])
	case "adopt":
		return accountAdopt(args[1:])
	case "status":
		return accountStatus(args[1:])
	case "resend-verification":
		return accountResendVerification(args[1:])
	case "close":
		return accountClose(args[1:])
	case "forget":
		return accountForget(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ws account: unknown subcommand %q\n", args[0])
		return 2
	}
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
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	email, err := client.ResendVerification(context.Background(), *endpoint, acct.ID, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		if strings.Contains(err.Error(), "unknown account") {
			fmt.Fprintf(os.Stderr, "    (it may have been closed for missing the verification window — `ws account forget --account %s --yes` removes the local name)\n", name)
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
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	rec, err := client.GetAccount(ctx, ep, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"account": rec})
	}
	fmt.Fprintln(os.Stderr, "id\tstatus\temail")
	fmt.Printf("%s\t%s\t%s\n", rec.ID, rec.Status, tabSafe(rec.Email))
	if rec.Status == "pending" {
		fmt.Fprintln(os.Stderr, "pending: activation required before the account can be used")
	}
	if rec.ClosedAt != nil {
		fmt.Fprintf(os.Stderr, "closed %s%s\n", rec.ClosedAt.UTC().Format(time.RFC3339), closedReasonSuffix(rec.ClosedReason))
	}
	return 0
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
			fmt.Fprintln(os.Stderr, "usage: ws account close --account acc_ID --token-file FILE [--reason TEXT] --yes")
			return 2
		}
		t, err := readToken(*tokenFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ws: %v\n", err)
			return 1
		}
		accountID, tok = *account, t
	} else {
		if strings.HasPrefix(*account, "acc_") {
			fmt.Fprintln(os.Stderr, "ws: a raw account id needs --token-file; local names never start with acc_")
			return 2
		}
		name, acct, t, err := local.Resolve(*account)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ws: %v\n", err)
			return 1
		}
		accountID, tok, localName = acct.ID, t, name
	}

	if !*yes {
		fmt.Fprintf(os.Stderr, "ws: closing %s is permanent — its credentials will be revoked and the account will be retired.\n    Nothing has changed yet; re-run with --yes to confirm.\n", accountID)
		return 2
	}
	if err := client.CloseAccount(context.Background(), *endpoint, accountID, tok, *reason); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "account %s closed. Its operator token is now dead.\n", accountID)
	if localName != "" {
		if err := local.Delete(localName); err != nil {
			fmt.Fprintf(os.Stderr, "ws: retiring local name %q: %v\n", localName, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "local account %q removed.\n", localName)
	}
	return 0
}

// accountForget removes a LOCAL account binding — the config.json entry and
// the token file — without ever contacting a server. It exists for stranded
// names: when the control plane closes an account the CLI didn't see close
// (the pending-account reaper, a torn-down cell), the credentials are already
// dead, so `ws account close` can't clean up the local half. Closing a live
// account is `ws account close`, which retires the local name itself.
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
		fmt.Fprintln(os.Stderr, "usage: ws account forget --account NAME --yes")
		return 2
	}
	// Available answering "taken" is exactly what forget needs: the name is
	// bound locally (config entry or token file), even if only half survives.
	switch err := local.Available(*account); {
	case err == nil:
		fmt.Fprintf(os.Stderr, "ws: no local account named %q\n", *account)
		return 1
	case !errors.Is(err, local.ErrNameTaken):
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if !*yes {
		fmt.Fprintf(os.Stderr, "ws: forgetting %q removes this machine's binding and token only — it does NOT close the account server-side (that is `ws account close`).\n    Nothing has changed yet; re-run with --yes to confirm.\n", *account)
		return 2
	}
	if err := local.Delete(*account); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "local account %q forgotten. The account itself, if it still exists, is untouched server-side.\n", *account)
	return 0
}

// accountCreate is Witself Cloud signup: one command from nothing to a working
// operator token. The control plane places the account on a cell; the CLI then
// claims it with the ordinary bootstrap exchange — the same path a self-hosted
// bootstrap uses — and remembers it under a local name so later commands are
// just `ws realm create --account NAME ...`.
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
		fmt.Fprintln(os.Stderr, "usage: ws account create --email EMAIL --invite CODE [--name LOCALNAME] [--display-name NAME] [--endpoint URL] [--out FILE]")
		return 2
	}
	localName := *name
	if localName == "" {
		localName = "default"
	}
	// Claim the local name BEFORE creating anything remote: a taken name must
	// not strand a freshly provisioned account's only credential.
	if err := local.Available(localName); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}

	ctx := context.Background()
	acct, err := client.CreateAccount(ctx, *endpoint, *email, *invite, *displayName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "account %s created on cell %s (%s)\n", acct.AccountID, acct.Cell.Name, acct.Cell.Endpoint)

	// Claim it: the same exchange a self-hosted bootstrap uses.
	res, err := client.BootstrapLogin(ctx, acct.Cell.Endpoint, acct.BootstrapToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: account created but login failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "logged in as operator %s\n", res.OperatorID)

	if err := local.Save(localName, local.Account{ID: acct.AccountID, Email: acct.Email}, res.OperatorToken); err != nil {
		// Never strand the only credential: surface the token if we can't store it.
		fmt.Fprintf(os.Stderr, "ws: saving local account: %v\n", err)
		fmt.Println(res.OperatorToken)
		return 1
	}
	fmt.Fprintf(os.Stderr, "saved locally as %q\n", localName)

	if *out != "" {
		if err := os.WriteFile(*out, []byte(res.OperatorToken+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "ws: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "next: ws realm create%s NAME\n", accountRef)
	case acct.EmailSent:
		fmt.Fprintf(os.Stderr, "account is %s — a verification link was emailed to %s\nafter clicking it: ws account status%s\n", acct.Status, acct.Email, accountRef)
	default:
		fmt.Fprintf(os.Stderr, "account is %s — activation required before use\nnext: ws account status%s\n", acct.Status, accountRef)
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
		fmt.Fprintln(os.Stderr, "usage: ws account adopt --id acc_ID --token-file FILE --name NAME")
		return 2
	}
	// Claim the local name before any round trip, so a taken name fails fast.
	if err := local.Available(*name); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if tok == "" {
		fmt.Fprintf(os.Stderr, "ws: token file %s is empty\n", *tokenFile)
		return 1
	}

	// Verify before binding: the directory says which cell hosts the account,
	// the cell says whether the token authenticates — and to which account.
	ctx := context.Background()
	_, cellEndpoint, err := client.LookupAccount(ctx, *endpoint, *id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: locate account %s: %v\n", *id, err)
		return 1
	}
	rec, err := client.GetAccount(ctx, cellEndpoint, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: verify token against %s: %v\n", cellEndpoint, err)
		return 1
	}
	if rec.ID != *id {
		fmt.Fprintf(os.Stderr, "ws: token belongs to account %s, not %s\n", rec.ID, *id)
		return 1
	}

	// Bind what the cell reported. Email may be empty (the seeded default
	// account has none) — the binding stores it only when present.
	if err := local.Save(*name, local.Account{ID: rec.ID, Email: rec.Email}, tok); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "adopted account %s (%s) as %q\n", rec.ID, rec.Status, *name)
	return 0
}

func usage(w io.Writer) {
	usageLine(w, "ws — the Witself CLI")
	usageLine(w)
	usageLine(w, "Usage:")
	usageLine(w, "  ws version              Print version information")
	usageLine(w, "  ws gen-bootstrap-token  Generate an operator bootstrap token")
	usageLine(w, "  ws auth login           Exchange a bootstrap token for an operator token")
	usageLine(w, "  ws account create       Create a Witself Cloud account (invite required)")
	usageLine(w, "  ws account adopt        Bind an existing account (id + token) to a local name")
	usageLine(w, "  ws account status       Show an account's lifecycle status")
	usageLine(w, "  ws account resend-verification  Email a fresh verification link")
	usageLine(w, "  ws account close        Permanently close an account (owner only)")
	usageLine(w, "  ws account forget       Remove a local account binding (server untouched)")
	usageLine(w, "  ws realm create|list|delete")
	usageLine(w, "  ws agent create|list|delete")
	usageLine(w, "  ws operator list|create|delete")
	usageLine(w, "  ws token create|revoke  Mint or revoke agent/operator tokens")
	usageLine(w, "  ws help                 Show this help")
	usageLine(w)
	usageLine(w, "Cloud commands take --account NAME (a local account name; when omitted,")
	usageLine(w, `WITSELF_ACCOUNT or "default"). Self-hosted: --endpoint URL --token-file FILE.`)
}

func usageLine(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}
