//go:build !windows

package main

import (
	"context"
	"io"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/agenttui"
)

func openTUIConsoleBrowser(ctx context.Context, accessURL string) error {
	if ctx.Err() != nil {
		return errConsoleCanceled
	}
	var opener string
	switch runtime.GOOS {
	case "darwin":
		opener = "open"
	case "linux":
		opener = "xdg-open"
	default:
		return errConsoleOpen
	}
	return launchTUIConsoleBrowser(ctx, exec.Command(opener, accessURL), time.Second)
}

// Native argv only, with a separate process group and an unconditional reaper.
// Cancellation bounds observation, never the lifetime of an already launched browser.
func launchTUIConsoleBrowser(ctx context.Context, command *exec.Cmd, wait time.Duration) error {
	if ctx.Err() != nil {
		return errConsoleCanceled
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return errConsoleOpen
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return errConsoleOpen
		}
		return nil
	case <-timer.C:
		return agenttui.ErrBrowserOutcomeUnknown
	case <-ctx.Done():
		return agenttui.ErrBrowserOutcomeUnknown
	}
}

// The native browser helper is only used on Windows.
func runTUIConsoleBrowserChild(_ io.Reader) int { return 1 }
