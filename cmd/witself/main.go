// Command witself is the Witself CLI. It also hosts the MCP stdio adapter via
// `witself mcp serve`. There is intentionally no `server` subcommand here; the
// backend API lives in cmd/witself-server.
//
// This is a scaffold: every command group is a stub that prints a clear
// not-yet-implemented message and exits nonzero. Real implementation (with
// cobra and the core service) replaces these stubs.
package main

import (
	"fmt"
	"os"

	"github.com/witwave-ai/witself/internal/version"
)

// commandGroups lists the documented CLI command groups. Each is a stub until
// the real implementation lands.
var commandGroups = []string{
	"memory", "fact", "secret", "totp", "policy", "group", "message",
	"password", "run", "self", "session", "recall", "account", "realm",
	"agent", "token", "audit", "billing", "support", "export", "import",
	"reference", "mcp", "config", "completion",
}

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
		fmt.Println(version.String())
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	case "mcp":
		return runMCP(args[1:])
	}

	for _, g := range commandGroups {
		if args[0] == g {
			fmt.Fprintf(os.Stderr, "witself %s: not yet implemented\n", g)
			return 1
		}
	}

	fmt.Fprintf(os.Stderr, "witself: unknown command %q\n", args[0])
	usage(os.Stderr)
	return 1
}

// runMCP handles `witself mcp ...`. `mcp serve` (the v0 MCP stdio adapter) is a
// stub until the real implementation lands.
func runMCP(args []string) int {
	if len(args) > 0 && args[0] == "serve" {
		fmt.Fprintln(os.Stderr, "witself mcp serve: not yet implemented")
		return 1
	}
	fmt.Fprintln(os.Stderr, "witself mcp: not yet implemented")
	return 1
}

func usage(w *os.File) {
	fmt.Fprintln(w, "witself - identity memory CLI (open plane: memories/facts; sealed plane: secrets/TOTP)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  witself <command> [args]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  version    Print version information")
	fmt.Fprintln(w, "  help       Show this help")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Command groups (stubs, not yet implemented):")
	for _, g := range commandGroups {
		fmt.Fprintf(w, "  %s\n", g)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Note: `witself mcp serve` runs the MCP stdio adapter. There is no")
	fmt.Fprintln(w, "`server` subcommand here; the backend API is the separate witself-server binary.")
}
