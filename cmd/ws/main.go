// Command ws is the Witself CLI. The full command surface is specified under
// docs/; this early build supports version, gen-bootstrap-token, and auth login.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
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
	cell := fs.String("cell", "", "cell name for the default output path (~/.witself/bootstrap/<cell>/bootstrap-token)")
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
	return filepath.Join(root, "bootstrap", cell, "bootstrap-token"), nil
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

func realmCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ws realm create|list|delete --endpoint URL --token-file FILE")
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
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	name := fs.Arg(0)
	if *endpoint == "" || *tokenFile == "" || name == "" {
		fmt.Fprintln(os.Stderr, "usage: ws realm create --endpoint URL --token-file FILE NAME")
		return 2
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	r, err := client.CreateRealm(context.Background(), *endpoint, tok, name)
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
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	yes := fs.Bool("yes", false, "confirm realm deletion")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	realmID := fs.Arg(0)
	if *endpoint == "" || *tokenFile == "" || realmID == "" {
		fmt.Fprintln(os.Stderr, "usage: ws realm delete --endpoint URL --token-file FILE --yes REALM")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "ws: refusing to delete realm without --yes")
		return 2
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if err := client.DeleteRealm(context.Background(), *endpoint, tok, realmID); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "deleted realm %s\n", realmID)
	return 0
}

func realmList(args []string) int {
	fs := flag.NewFlagSet("realm list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *endpoint == "" || *tokenFile == "" {
		fmt.Fprintln(os.Stderr, "usage: ws realm list --endpoint URL --token-file FILE")
		return 2
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	realms, err := client.ListRealms(context.Background(), *endpoint, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	for _, r := range realms {
		fmt.Printf("%s\t%s\n", r.ID, r.Name)
	}
	return 0
}

func agentCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ws agent create|list|delete --endpoint URL --token-file FILE --realm REALM")
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
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	realm := fs.String("realm", "", "realm id the agent belongs to")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	name := fs.Arg(0)
	if *endpoint == "" || *tokenFile == "" || *realm == "" || name == "" {
		fmt.Fprintln(os.Stderr, "usage: ws agent create --endpoint URL --token-file FILE --realm REALM NAME")
		return 2
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	a, err := client.CreateAgent(context.Background(), *endpoint, tok, *realm, name)
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
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	realm := fs.String("realm", "", "realm id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *endpoint == "" || *tokenFile == "" || *realm == "" {
		fmt.Fprintln(os.Stderr, "usage: ws agent list --endpoint URL --token-file FILE --realm REALM")
		return 2
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	agents, err := client.ListAgents(context.Background(), *endpoint, tok, *realm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	for _, a := range agents {
		fmt.Printf("%s\t%s\n", a.ID, a.Name)
	}
	return 0
}

func agentDelete(args []string) int {
	fs := flag.NewFlagSet("agent delete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	realm := fs.String("realm", "", "realm id")
	yes := fs.Bool("yes", false, "confirm agent deletion and token revocation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	agentID := fs.Arg(0)
	if *endpoint == "" || *tokenFile == "" || *realm == "" || agentID == "" {
		fmt.Fprintln(os.Stderr, "usage: ws agent delete --endpoint URL --token-file FILE --realm REALM --yes AGENT")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "ws: refusing to delete agent without --yes")
		return 2
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if err := client.DeleteAgent(context.Background(), *endpoint, tok, *realm, agentID); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "deleted agent %s\n", agentID)
	return 0
}

func operatorCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ws operator list|create|delete --endpoint URL --token-file FILE")
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
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *endpoint == "" || *tokenFile == "" {
		fmt.Fprintln(os.Stderr, "usage: ws operator list --endpoint URL --token-file FILE")
		return 2
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	operators, err := client.ListOperators(context.Background(), *endpoint, tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
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
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	name := fs.String("name", "", "operator display name")
	tokenName := fs.String("token-name", "", "initial operator token display name")
	ttl := fs.String("ttl", "", "initial operator token lifetime, such as 24h or 30m")
	out := fs.String("out", "", "write the new operator token to this file (0600) instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *endpoint == "" || *tokenFile == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "usage: ws operator create --endpoint URL --token-file FILE --name NAME [--token-name NAME] [--ttl DURATION] [--out FILE]")
		return 2
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	res, err := client.CreateOperator(context.Background(), *endpoint, tok, *name, *tokenName, *ttl)
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
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	yes := fs.Bool("yes", false, "confirm operator deletion and token revocation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	operatorID := fs.Arg(0)
	if *endpoint == "" || *tokenFile == "" || operatorID == "" {
		fmt.Fprintln(os.Stderr, "usage: ws operator delete --endpoint URL --token-file FILE --yes OPERATOR")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "ws: refusing to delete operator without --yes")
		return 2
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if err := client.DeleteOperator(context.Background(), *endpoint, tok, operatorID); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "deleted operator %s\n", operatorID)
	return 0
}

func tokenCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ws token create|revoke --endpoint URL --token-file FILE")
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
	if *endpoint == "" || *tokenFile == "" || (*agent == "" && !*operator) || (*agent != "" && *operator) {
		fmt.Fprintln(os.Stderr, "usage: ws token create --endpoint URL --token-file FILE (--agent AGENT | --operator) [--name NAME] [--ttl DURATION] [--out FILE]")
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
	op, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if *operator {
		res, err := client.CreateOperatorToken(context.Background(), *endpoint, op, *name, *ttl)
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

	agentTok, tokenID, err := client.CreateAgentToken(context.Background(), *endpoint, op, *agent)
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
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing the operator token")
	tokenID := fs.String("token", "", "token id to revoke")
	yes := fs.Bool("yes", false, "confirm token revocation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *endpoint == "" || *tokenFile == "" || *tokenID == "" {
		fmt.Fprintln(os.Stderr, "usage: ws token revoke --endpoint URL --token-file FILE --token TOKEN_ID --yes")
		return 2
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "ws: refusing to revoke token without --yes")
		return 2
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	if err := client.RevokeToken(context.Background(), *endpoint, tok, *tokenID); err != nil {
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

func usage(w io.Writer) {
	usageLine(w, "ws — the Witself CLI")
	usageLine(w)
	usageLine(w, "Usage:")
	usageLine(w, "  ws version              Print version information")
	usageLine(w, "  ws gen-bootstrap-token  Generate an operator bootstrap token")
	usageLine(w, "  ws auth login           Exchange a bootstrap token for an operator token")
	usageLine(w, "  ws realm create|list|delete")
	usageLine(w, "  ws agent create|list|delete")
	usageLine(w, "  ws operator list|create|delete")
	usageLine(w, "  ws token create|revoke  Mint or revoke agent/operator tokens")
	usageLine(w, "  ws help                 Show this help")
}

func usageLine(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}
