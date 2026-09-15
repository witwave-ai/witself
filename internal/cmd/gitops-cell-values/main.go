// Command gitops-cell-values generates per-cell GitOps values overlays and
// either writes them or checks the committed files for drift.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/witwave-ai/witself/internal/gitopsvalues"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("gitops-cell-values", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	check := fs.Bool("check", false, "exit non-zero with a unified diff when generated files differ")
	write := fs.Bool("write", false, "rewrite generated files that differ")
	root := fs.String("root", ".", "repository root containing .gitops/cells")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "usage: gitops-cell-values --check|--write [--root PATH]\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	if *check == *write {
		fs.Usage()
		_, _ = fmt.Fprintln(os.Stderr, "error: exactly one of --check or --write is required")
		return 2
	}
	var err error
	if *check {
		err = gitopsvalues.Check(*root, os.Stdout)
	} else {
		err = gitopsvalues.Write(*root, os.Stdout)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		if *check {
			return 1
		}
		return 2
	}
	return 0
}
