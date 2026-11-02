//go:build darwin

package localcell

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestLaunchdNativeConstructorAndCommandIsolation(t *testing.T) {
	const childMarker = "--launchd-command-child"
	if index := slices.Index(os.Args, childMarker); index >= 0 {
		// Compare in memory only: environment values must never enter test output.
		if index+1 >= len(os.Args) || len(os.Environ()) != 1 || os.Getenv("HOME") != os.Args[index+1] {
			os.Exit(2)
		}
		os.Exit(0)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("DSH_HOME", home)
	t.Setenv("LAUNCHD_CALLER_CANARY", "synthetic-private-value")
	a, ok := NewServiceAdapter().(*launchdAgentAdapter)
	if !ok || a.home != home || a.agentDir != filepath.Join(home, "Library", "LaunchAgents") {
		t.Fatal("constructor did not select exact temporary home")
	}
	if _, err := os.Lstat(a.agentDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("constructor created agent directory")
	}
	host, err := os.Executable()
	if err != nil {
		t.Fatal("cannot resolve fixture child")
	}
	output, err := a.command(t.Context(), host, "-test.run=^TestLaunchdNativeConstructorAndCommandIsolation$", "--", childMarker, home)
	if err != nil || len(output) != 0 {
		t.Fatal("native command inherited ambient environment or lost exact HOME")
	}
	_, err = a.command(t.Context(), "/bin/sh", "-c", "printf 'synthetic-stderr' >&2; exit 5")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || string(exitErr.Stderr) != "synthetic-stderr" {
		t.Fatal("native command did not preserve stderr separately on ExitError")
	}
	bounded, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = a.command(bounded, "/bin/sh", "-c", "exit 0")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("native command did not preserve caller cancellation")
	}
}
