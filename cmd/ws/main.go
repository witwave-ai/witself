// Command ws is the Witself CLI. The full command surface is specified under
// docs/; this early build supports version, gen-bootstrap-token, and auth login.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/token"
	"github.com/witwave-ai/witself/internal/version"
)

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
	out := fs.String("out", "", "write the token to this file (0600) instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
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
	if err := os.WriteFile(*out, []byte(tok+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "ws: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "wrote operator bootstrap token to %s\n", *out)
	return 0
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

func usage(w io.Writer) {
	fmt.Fprintln(w, "ws — the Witself CLI")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ws version              Print version information")
	fmt.Fprintln(w, "  ws gen-bootstrap-token  Generate an operator bootstrap token")
	fmt.Fprintln(w, "  ws auth login           Exchange a bootstrap token for an operator token")
	fmt.Fprintln(w, "  ws help                 Show this help")
}
