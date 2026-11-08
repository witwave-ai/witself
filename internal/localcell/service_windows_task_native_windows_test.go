//go:build windows

package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsTaskNativeCapabilityRemainsUntyped(t *testing.T) {
	// This fixture makes no manager calls and cannot register anything. A
	// non-interactive/elevated runner also refuses before the unproven codes.
	err := WindowsLogonTaskCapability(t.Context())
	if !errors.Is(err, ErrServiceManagerUnavailable) {
		t.Fatal("unproved native query codes enabled task registration")
	}
	for _, fixed := range []string{"query_result_untyped", "owner_token_refused", "interactive_token_unavailable", "system_directory_unavailable", "system_command_unavailable"} {
		if strings.Contains(err.Error(), fixed) {
			return
		}
	}
	t.Fatal("native capability did not return a fixed refusal")
}

func TestWindowsTaskNativeCommandUsesSystemDirectory(t *testing.T) {
	path, root, err := windowsTaskSystemCommand()
	if err != nil {
		t.Fatal("trusted task manager command unavailable")
	}
	system, err := windows.GetSystemDirectory()
	if err != nil || path != filepath.Join(system, "schtasks.exe") || !filepath.IsAbs(path) || !filepath.IsAbs(root) {
		t.Fatal("task manager command was resolved outside system directory")
	}
}

func TestWindowsTaskRetainedExecutableAndOwner(t *testing.T) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(os.Getpid()))
	if err != nil {
		t.Fatal("cannot retain fixture process")
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	host, err := os.Executable()
	if err != nil {
		t.Fatal("cannot resolve fixture executable")
	}
	uid, err := serviceOwner()
	if err != nil {
		t.Fatal("cannot resolve fixture owner")
	}
	observer := &windowsServiceExitHost{handle: handle}
	d := ServiceDescriptor{UID: uid, Payload: ServicePayload{Host: host}}
	if observer.validateHost(d) != nil {
		t.Fatal("retained host executable and owner did not match")
	}
	d.UID = "S-1-5-18"
	if observer.validateHost(d) == nil {
		t.Fatal("retained host accepted foreign owner SID")
	}
	d.UID = uid
	d.Payload.Host = filepath.Join(t.TempDir(), "different.exe")
	if os.WriteFile(d.Payload.Host, []byte("not the retained image"), 0o600) != nil {
		t.Fatal("cannot write executable identity fixture")
	}
	if observer.validateHost(d) == nil {
		t.Fatal("retained host accepted different executable identity")
	}
	for _, invalid := range []windows.Handle{0, windows.InvalidHandle} {
		if windowsValidateProcessExecutable(invalid, host) == nil {
			t.Fatal("invalid retained handle supplied executable identity")
		}
	}
}

func TestWindowsTaskOwnerTokenIsExplicit(t *testing.T) {
	uid, err := serviceOwner()
	if err != nil {
		t.Fatal("cannot resolve fixture owner")
	}
	if validateTaskHostOwner(uid+"-1") == nil {
		t.Fatal("task host accepted a foreign descriptor SID")
	}
	elevation, err := windowsTaskTokenUint32(windows.GetCurrentProcessToken(), windows.TokenElevation)
	if err != nil {
		t.Fatal("cannot query fixture token elevation")
	}
	if (validateTaskHostOwner(uid) == nil) != validWindowsTaskOwner(uid, elevation != 0) {
		t.Fatal("task host token elevation policy was bypassed")
	}
}
