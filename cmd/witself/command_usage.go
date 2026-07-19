package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// commandHelpRequested recognizes help at command-group boundaries, before a
// FlagSet exists to process it. Both spellings are accepted for ordinary CLI
// discoverability.
func commandHelpRequested(args []string) bool {
	return len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help")
}

// configureCommandUsage gives FlagSet-based subcommands their documented
// positional syntax instead of the package default's implementation-oriented
// "Usage of ..." heading.
func configureCommandUsage(fs *flag.FlagSet, usage string) {
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), usage)
		fs.PrintDefaults()
	}
}

// parseCommandFlags makes an explicit help request a successful, side-effect-
// free command. flag.FlagSet reports flag.ErrHelp through the same channel as a
// parse error, so callers must distinguish the two.
func parseCommandFlags(fs *flag.FlagSet, args []string) (parsed bool, exitCode int) {
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return false, 0
	}
	if err != nil {
		return false, 2
	}
	return true, 0
}

func printCommandGroupHelp(w io.Writer, usage string, commands ...string) {
	_, _ = fmt.Fprintln(w, usage)
	if len(commands) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Commands:")
	for _, command := range commands {
		_, _ = fmt.Fprintf(w, "  %s\n", command)
	}
}
