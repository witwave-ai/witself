package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	output := os.Getenv("WITSELF_TEST_WINDOWS_HOOK_ARGV_OUTPUT")
	if output == "" {
		fmt.Fprintln(os.Stderr, "WITSELF_TEST_WINDOWS_HOOK_ARGV_OUTPUT is required")
		os.Exit(2)
	}
	raw, err := json.Marshal(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(output, raw, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
