//go:build windows

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestTUIConsoleWindowsNativeBrowserBoundary(t *testing.T) {
	old := consoleShellExecute
	t.Cleanup(func() { consoleShellExecute = old })
	target := "http://127.0.0.1:51234/?token=" + strings.Repeat("ab", 16)
	calls := 0
	consoleShellExecute = func(_ windows.Handle, operation, file, parameters, directory *uint16, show int32) error {
		calls++
		if windows.UTF16PtrToString(operation) != "open" || windows.UTF16PtrToString(file) != target || parameters != nil || directory != nil || show != windows.SW_SHOWNORMAL {
			t.Error("unexpected native browser parameters")
		}
		return nil
	}
	if code := runTUIConsoleBrowserChild(strings.NewReader(target)); code != 0 || calls != 1 {
		t.Fatalf("native call: code=%d calls=%d", code, calls)
	}
	for _, bad := range []string{"https://example.invalid/", target + "&next=x", target + "#fragment", strings.Repeat("x", 1025)} {
		if code := runTUIConsoleBrowserChild(strings.NewReader(bad)); code == 0 {
			t.Error("unsafe native target accepted")
		}
	}
	if calls != 1 {
		t.Fatal("unsafe target called native API")
	}
	consoleShellExecute = func(windows.Handle, *uint16, *uint16, *uint16, *uint16, int32) error {
		return errors.New("private-url-error")
	}
	if code := runTUIConsoleBrowserChild(strings.NewReader(target)); code != 1 {
		t.Fatal("native failure not propagated as fixed exit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := openTUIConsoleBrowser(ctx, target); err != errConsoleCanceled {
		t.Fatalf("canceled open: %v", err)
	}
}
