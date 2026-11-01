package localcell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPGCommandFixtureProcess(_ *testing.T) {
	if len(os.Args) < 5 || os.Args[2] != "--" || os.Args[3] != "--flake3-command" {
		return
	}
	if err := os.WriteFile("command-entered", nil, 0o600); err != nil {
		os.Exit(9)
	}
	fmt.Fprintln(os.Stderr, "synthetic-private-command-output")
	if os.Args[4] == "hold" {
		time.Sleep(10 * time.Second)
	}
	os.Exit(7)
}

func TestPGCommandFixedCausesReachJournal(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"exit", "PostgreSQL command exited with failure"},
		{"cancel", "PostgreSQL command cancelled"},
		{"deadline", "PostgreSQL command timed out"},
		{"missing", "PostgreSQL command could not start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := unitCell(t)
			c.cfg.Binaries.PostgresDir = t.TempDir()
			if tc.name != "missing" {
				self, err := os.Executable()
				if err != nil {
					t.Fatal("cannot locate fixed cause command fixture")
				}
				data, err := os.ReadFile(self)
				if err != nil || os.WriteFile(filepath.Join(c.cfg.Binaries.PostgresDir, executable("fixture")), data, 0o700) != nil {
					t.Fatal("cannot prepare fixed cause command fixture")
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			mode := "exit"
			done := make(chan struct{})
			if tc.name == "cancel" {
				mode = "hold"
				go func() {
					defer close(done)
					ticker := time.NewTicker(5 * time.Millisecond)
					defer ticker.Stop()
					for {
						if _, err := os.Stat(c.path("command-entered")); err == nil {
							cancel()
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
						}
					}
				}()
				defer func() { cancel(); <-done }()
			}
			if tc.name == "deadline" {
				mode = "hold"
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 250*time.Millisecond)
				defer deadlineCancel()
			}
			err := c.step("install", "initdb", func() error {
				return c.pgCommand(ctx, "fixture", "-test.run=^TestPGCommandFixtureProcess$", "--", "--flake3-command", mode)
			})
			assertFixedCauseJournal(t, c, err, tc.want)
			if tc.name == "cancel" {
				if _, err := os.Stat(c.path("command-entered")); err != nil {
					t.Fatal("command cancellation did not follow process entry")
				}
			}
		})
	}
}

func assertFixedCauseJournal(t *testing.T, c *Cell, err error, want string) {
	t.Helper()
	var cause fixedCause
	if err == nil || !errors.As(err, &cause) || cause.Error() != want || err.Error() != "local cell install: initdb failed: "+want {
		t.Fatal("fixed command cause did not reach returned error")
	}
	data, readErr := os.ReadFile(c.path("journal", "operations.jsonl"))
	if readErr != nil {
		t.Fatal("cannot read fixed cause journal fixture")
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var entry journalEntry
	if len(lines) != 2 || json.Unmarshal([]byte(lines[1]), &entry) != nil || entry.State != "failed" || entry.Cause != want {
		t.Fatal("fixed command cause did not reach journal field")
	}
	if strings.Contains(string(data), "synthetic-private") || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatal("fixed command cause exposed private diagnostic data")
	}
}

func TestStepFixedCauseDropsPrivateWrappers(t *testing.T) {
	c, _ := unitCell(t)
	trusted := fixedCause("PostgreSQL command exited with failure")
	err := c.step("install", "initdb", func() error {
		return fmt.Errorf("synthetic-private-wrapper: %w", trusted)
	})
	assertFixedCauseJournal(t, c, err, trusted.Error())
}

func TestStepUnknownCauseKeepsJournalEmpty(t *testing.T) {
	c, _ := unitCell(t)
	err := c.step("install", "initdb", func() error { return errors.New("synthetic-private-untrusted-error") })
	if err == nil || err.Error() != "local cell install: initdb failed" {
		t.Fatal("untrusted command error crossed the fixed cause boundary")
	}
	data, readErr := os.ReadFile(c.path("journal", "operations.jsonl"))
	if readErr != nil || strings.Contains(string(data), "cause") || strings.Contains(string(data), "synthetic-private") {
		t.Fatal("untrusted command error changed empty journal cause")
	}
}
