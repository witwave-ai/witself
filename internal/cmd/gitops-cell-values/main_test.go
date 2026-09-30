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
		{name: "no mode", want: "exactly one of --check, --write, --roll-cell, or --first-sync-check"},
		{name: "check and write", args: []string{"--check", "--write"}, want: "exactly one of --check, --write, --roll-cell, or --first-sync-check"},
		{name: "check and roll", args: []string{"--check", "--roll-cell", "cell"}, want: "exactly one of --check, --write, --roll-cell, or --first-sync-check"},
		{name: "write and roll", args: []string{"--write", "--roll-cell", "cell"}, want: "exactly one of --check, --write, --roll-cell, or --first-sync-check"},
		{name: "first sync and roll", args: []string{"--first-sync-check", "cell", "--roll-cell", "cell"}, want: "exactly one of --check, --write, --roll-cell, or --first-sync-check"},
		{name: "check and first sync", args: []string{"--check", "--first-sync-check", "cell"}, want: "exactly one of --check, --write, --roll-cell, or --first-sync-check"},
		{name: "first sync lacks version", args: []string{"--first-sync-check", "cell"}, want: "--first-sync-check requires --version"},
		{name: "first sync with digest", args: []string{"--first-sync-check", "cell", "--version", "0.0.1", "--image-digest", "invalid"}, want: "--image-digest requires --roll-cell"},
		{name: "first sync with backup", args: []string{"--first-sync-check", "cell", "--version", "0.0.1", "--backup-image-repository", "postgres", "--backup-image-tag", "0.0.1", "--backup-image-digest", "invalid"}, want: "backup image repository, tag, and digest require --roll-cell"},
		{name: "first sync with postgres", args: []string{"--first-sync-check", "cell", "--version", "0.0.1", "--postgres-image-registry", "ghcr.io", "--postgres-image-repository", "witwave-ai/images/postgresql", "--postgres-image-tag", "0.0.1-cell", "--postgres-image-digest", "invalid"}, want: "PostgreSQL image registry, repository, tag, and digest require --roll-cell"},
		{name: "roll lacks both pins", args: []string{"--roll-cell", "cell"}, want: "--roll-cell requires --version and --image-digest"},
		{name: "roll lacks digest", args: []string{"--roll-cell", "cell", "--version", "0.0.1"}, want: "--roll-cell requires --version and --image-digest"},
		{name: "roll lacks version", args: []string{"--roll-cell", "cell", "--image-digest", "invalid"}, want: "--roll-cell requires --version and --image-digest"},
		{name: "digest without roll", args: []string{"--write", "--image-digest", "invalid"}, want: "--image-digest requires --roll-cell"},
		{name: "version without roll", args: []string{"--check", "--version", "0.0.1"}, want: "--version requires --roll-cell or --first-sync-check"},
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

func TestFirstSyncCheckMode(t *testing.T) {
	const catalog = `schema: witself.gitops-cells.v1
gitops:
  repoURL: https://example.invalid/repo
  targetRevision: main
defaults:
  account_alias: sandbox
  role: dev
  domain_parent: cells.example.invalid
cells:
  civo-prod-use1-serving:
    cloud: civo
    account_alias: prod
    region: nyc1
    role: serving
    unprovisioned: true
`
	for _, tc := range []struct {
		name       string
		version    string
		unmarked   bool
		wantStatus int
		wantStdout string
		wantStderr string
	}{
		{name: "eligible", version: "1.2.3", wantStdout: "api.civo-prod-use1-serving.cells.example.invalid\n"},
		{name: "downgrade", version: "1.2.2", wantStatus: 1, wantStderr: "is lower than the pinned chartVersion 1.2.3"},
		{name: "unmarked", version: "1.2.3", unmarked: true, wantStatus: 1, wantStderr: "is not recorded as unprovisioned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cellDir := filepath.Join(root, ".gitops", "cells", "civo-prod-use1-serving")
			if err := os.MkdirAll(cellDir, 0o755); err != nil {
				t.Fatal(err)
			}
			body := catalog
			if tc.unmarked {
				body = strings.ReplaceAll(body, "    unprovisioned: true\n", "")
			}
			if err := os.WriteFile(filepath.Join(root, ".gitops", "cells", "catalog.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			values := "apps:\n  witselfServer:\n    chartVersion: 1.2.3\n    imageTag: 1.2.3\n"
			if err := os.WriteFile(filepath.Join(cellDir, "values.yaml"), []byte(values), 0o600); err != nil {
				t.Fatal(err)
			}
			stdoutPath := filepath.Join(root, "stdout")
			stdout, err := os.Create(stdoutPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stdout.Close() })
			stderrPath := filepath.Join(root, "stderr")
			stderr, err := os.Create(stderrPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stderr.Close() })
			previousStdout, previousStderr := os.Stdout, os.Stderr
			os.Stdout, os.Stderr = stdout, stderr
			t.Cleanup(func() { os.Stdout, os.Stderr = previousStdout, previousStderr })
			if status := run([]string{"--root", root, "--first-sync-check", "civo-prod-use1-serving", "--version", tc.version}); status != tc.wantStatus {
				t.Fatalf("first-sync check exited %d, want %d", status, tc.wantStatus)
			}
			out, err := os.ReadFile(stdoutPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.wantStdout {
				t.Fatalf("stdout = %q, want %q", out, tc.wantStdout)
			}
			errOut, err := os.ReadFile(stderrPath)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantStderr == "" {
				if len(errOut) != 0 {
					t.Fatalf("successful check wrote stderr: %s", errOut)
				}
			} else if !strings.Contains(string(errOut), tc.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", errOut, tc.wantStderr)
			}
		})
	}
}
