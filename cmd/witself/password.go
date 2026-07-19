package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/witwave-ai/witself/internal/sealed"
)

func passwordCmd(args []string) int {
	if len(args) == 0 || args[0] != "generate" {
		fmt.Fprintln(os.Stderr, "usage: witself password generate [--length N] [--lowercase=BOOL] [--uppercase=BOOL] [--digits=BOOL] [--symbols=BOOL] [--exclude-ambiguous]")
		return 2
	}
	return passwordGenerate(args[1:])
}

func passwordGenerate(args []string) int {
	defaults := sealed.DefaultPasswordPolicy()
	fs := flag.NewFlagSet("password generate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	length := fs.Int("length", defaults.Length, "password length")
	lowercase := fs.Bool("lowercase", defaults.Lowercase, "include lowercase letters")
	uppercase := fs.Bool("uppercase", defaults.Uppercase, "include uppercase letters")
	digits := fs.Bool("digits", defaults.Digits, "include digits")
	symbols := fs.Bool("symbols", defaults.Symbols, "include symbols")
	excludeAmbiguous := fs.Bool("exclude-ambiguous", defaults.ExcludeAmbiguous, "exclude visually ambiguous characters")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "witself: password generate accepts no positional arguments")
		return 2
	}

	password, err := sealed.GeneratePassword(sealed.PasswordPolicy{
		Length:           *length,
		Lowercase:        *lowercase,
		Uppercase:        *uppercase,
		Digits:           *digits,
		Symbols:          *symbols,
		ExcludeAmbiguous: *excludeAmbiguous,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "witself: invalid password policy")
		return 2
	}
	// This command is deliberately value-returning. The value is emitted only
	// on stdout so callers can pipe it directly without it entering logs or
	// diagnostic text.
	_, _ = fmt.Fprintln(os.Stdout, password)
	return 0
}
