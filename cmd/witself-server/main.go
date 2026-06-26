// Command witself-server is the Witself backend API server. It supports the
// serve, serve --dev, and migrate subcommands.
//
// The server exposes three separate ports (documented here, wired up by the
// real implementation):
//
//	:8080  API      (/v1, plural resources, colon actions)
//	:8081  health   (Kubernetes liveness/readiness probes)
//	:9090  metrics  (Prometheus)
//
// Configuration is read from the WITSELF_ environment prefix. This is a
// scaffold: each subcommand is a stub that prints a clear not-yet-implemented
// message and exits nonzero. Real implementation replaces these stubs.
package main

import (
	"fmt"
	"os"

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
		fmt.Println(version.String())
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	case "serve":
		return runServe(args[1:])
	case "migrate":
		fmt.Fprintln(os.Stderr, "witself-server migrate: not yet implemented")
		return 1
	}

	fmt.Fprintf(os.Stderr, "witself-server: unknown command %q\n", args[0])
	usage(os.Stderr)
	return 1
}

// runServe handles `serve` and `serve --dev`. Both are stubs until the real
// implementation lands.
func runServe(args []string) int {
	dev := false
	for _, a := range args {
		if a == "--dev" {
			dev = true
		}
	}
	if dev {
		fmt.Fprintln(os.Stderr, "witself-server serve --dev: not yet implemented")
	} else {
		fmt.Fprintln(os.Stderr, "witself-server serve: not yet implemented")
	}
	return 1
}

func usage(w *os.File) {
	fmt.Fprintln(w, "witself-server - Witself backend API server")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  witself-server <command> [args]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  serve         Run the API server")
	fmt.Fprintln(w, "  serve --dev   Run the API server in development mode")
	fmt.Fprintln(w, "  migrate       Run database migrations")
	fmt.Fprintln(w, "  version       Print version information")
	fmt.Fprintln(w, "  help          Show this help")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Ports: :8080 (api) :8081 (health) :9090 (metrics). Config via WITSELF_ env prefix.")
}
