//go:build !windows

package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agenttui"
)

func TestTUIConsoleBrowserLaunchObservation(t *testing.T) {
	consoleTestHome(t)
	missing := exec.Command(filepath.Join(t.TempDir(), "missing-opener"))
	if err := launchTUIConsoleBrowser(context.Background(), missing, time.Second); err != errConsoleOpen {
		t.Fatal("missing opener accepted")
	}
	for _, mode := range []string{"console-test-fail", "console-test-wait"} {
		command, err := consoleTestCommand(mode)()
		if err != nil {
			t.Fatal(err)
		}
		want := error(nil)
		if mode == "console-test-fail" {
			want = errConsoleOpen
		}
		if err := launchTUIConsoleBrowser(context.Background(), command, 5*time.Second); err != want {
			t.Fatal("early exit was not observed")
		}
	}
	command := exec.Command("sleep", "30")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := launchTUIConsoleBrowser(ctx, command, 30*time.Millisecond)
	if !errors.Is(err, agenttui.ErrBrowserOutcomeUnknown) {
		t.Fatal("running opener reported failure")
	}
	defer func() { _ = command.Process.Kill() }()
	cancel()
	if err := command.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("launch cancellation killed browser")
	}
	group, err := syscall.Getpgid(command.Process.Pid)
	if err != nil || group != command.Process.Pid {
		t.Fatal("browser did not get a separate process group")
	}
}
