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
		{name: "backup without roll", args: []string{"--write", "--backup-image-repository", "postgres"}, want: "backup image repository, tag, and digest require --roll-cell"},
		{name: "backup lacks tag and digest", args: []string{"--roll-cell", "cell", "--version", "0.0.1", "--image-digest", "invalid", "--backup-image-repository", "postgres"}, want: "must all be supplied"},
		{name: "backup lacks repository", args: []string{"--roll-cell", "cell", "--version", "0.0.1", "--image-digest", "invalid", "--backup-image-tag", "0.0.1", "--backup-image-digest", "invalid"}, want: "must all be supplied"},
		{name: "postgres without roll", args: []string{"--write", "--postgres-image-registry", "ghcr.io"}, want: "PostgreSQL image registry, repository, tag, and digest require --roll-cell"},
		{name: "postgres lacks repository tag and digest", args: []string{"--roll-cell", "cell", "--version", "0.0.1", "--image-digest", "invalid", "--postgres-image-registry", "ghcr.io"}, want: "must all be supplied"},
		{name: "postgres lacks registry", args: []string{"--roll-cell", "cell", "--version", "0.0.1", "--image-digest", "invalid", "--postgres-image-repository", "witwave-ai/images/postgresql", "--postgres-image-tag", "0.0.1-cell", "--postgres-image-digest", "invalid"}, want: "must all be supplied"},
		{name: "postgres lacks tag", args: []string{"--roll-cell", "cell", "--version", "0.0.1", "--image-digest", "invalid", "--postgres-image-registry", "ghcr.io", "--postgres-image-repository", "witwave-ai/images/postgresql", "--postgres-image-digest", "invalid"}, want: "must all be supplied"},
		{name: "postgres lacks digest", args: []string{"--roll-cell", "cell", "--version", "0.0.1", "--image-digest", "invalid", "--postgres-image-registry", "ghcr.io", "--postgres-image-repository", "witwave-ai/images/postgresql", "--postgres-image-tag", "0.0.1-cell"}, want: "must all be supplied"},
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
