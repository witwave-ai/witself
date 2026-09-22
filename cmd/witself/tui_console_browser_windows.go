//go:build windows

package main

import (
	"context"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

// Native association API, with a seam for tests that must never open a browser.
var consoleShellExecute = windows.ShellExecute

func openTUIConsoleBrowser(ctx context.Context, accessURL string) error {
	if ctx.Err() != nil {
		return errConsoleCanceled
	}
	executable, err := os.Executable()
	if err != nil {
		return errConsoleOpen
	}
	// Isolate the synchronous native call so a hung OS association handler
	// cannot outlive the bounded action. The URL travels only over private stdin.
	command := exec.CommandContext(ctx, executable, tuiConsoleBrowserArgument)
	command.Env = consoleChildEnvironment()
	command.Stdin = strings.NewReader(accessURL)
	if err := command.Run(); err != nil {
		return errConsoleOpen
	}
	return nil
}

func runTUIConsoleBrowserChild(input io.Reader) int {
	raw, err := io.ReadAll(io.LimitReader(input, 1025))
	if err != nil || len(raw) > 1024 {
		return 1
	}
	accessURL := string(raw)
	u, err := url.Parse(accessURL)
	if err != nil {
		return 1
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return 1
	}
	base := "http://127.0.0.1:" + strconv.Itoa(port) + "/?token="
	if !strings.HasPrefix(accessURL, base) {
		return 1
	}
	token := strings.TrimPrefix(accessURL, base)
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 16 || token != strings.ToLower(token) {
		return 1
	}
	operation, _ := windows.UTF16PtrFromString("open")
	target, err := windows.UTF16PtrFromString(accessURL)
	if err != nil {
		return 1
	}
	if err := consoleShellExecute(0, operation, target, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		return 1
	}
	return 0
}
