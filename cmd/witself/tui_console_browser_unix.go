//go:build !windows

package main

import (
	"context"
	"io"
	"os/exec"
	"runtime"
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
	// Native argv delivery only; never a shell. CommandContext bounds the
	// launcher and reaps it; failures retain the running console for retry.
	if err := exec.CommandContext(ctx, opener, accessURL).Run(); err != nil {
		return errConsoleOpen
	}
	return nil
}

// The native browser helper is only used on Windows.
func runTUIConsoleBrowserChild(_ io.Reader) int { return 1 }
