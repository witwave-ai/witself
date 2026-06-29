// Command ws is the Witself CLI. This first slice (v0.0.1) supports only the
// version command; the full command surface is specified under docs/.
package main

import (
	"fmt"
	"io"
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
		fmt.Println(version.String("ws"))
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "ws: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "ws — the Witself CLI")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ws version    Print version information")
	fmt.Fprintln(w, "  ws help       Show this help")
}
