package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRollFlagsRequireAnExclusiveCompleteMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "no mode", want: "exactly one of --check, --write, or --roll-cell"},
		{name: "check and write", args: []string{"--check", "--write"}, want: "exactly one of --check, --write, or --roll-cell"},
		{name: "check and roll", args: []string{"--check", "--roll-cell", "cell"}, want: "exactly one of --check, --write, or --roll-cell"},
		{name: "write and roll", args: []string{"--write", "--roll-cell", "cell"}, want: "exactly one of --check, --write, or --roll-cell"},
		{name: "roll lacks both pins", args: []string{"--roll-cell", "cell"}, want: "--roll-cell requires --version and --image-digest"},
		{name: "roll lacks digest", args: []string{"--roll-cell", "cell", "--version", "0.0.1"}, want: "--roll-cell requires --version and --image-digest"},
		{name: "roll lacks version", args: []string{"--roll-cell", "cell", "--image-digest", "invalid"}, want: "--roll-cell requires --version and --image-digest"},
		{name: "digest without roll", args: []string{"--write", "--image-digest", "invalid"}, want: "--version and --image-digest require --roll-cell"},
		{name: "version without roll", args: []string{"--check", "--version", "0.0.1"}, want: "--version and --image-digest require --roll-cell"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stderr")
			capture, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			previousStderr := os.Stderr
			os.Stderr = capture
			t.Cleanup(func() {
				os.Stderr = previousStderr
				_ = capture.Close()
			})
			if status := run(tc.args); status != 2 {
				t.Fatalf("invalid flag combination exited %d, want 2", status)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Fatalf("invalid flags were not rejected before generator dispatch: %s", body)
			}
		})
	}
}
