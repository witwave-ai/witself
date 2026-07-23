//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsIntegrationOperationLockUsesProtectedDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operation.lock")
	release, err := acquireIntegrationOperationLock(path, "test", "busy")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("integration operation lock DACL inherits untrusted parent entries")
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil || user == nil || user.User.Sid == nil || !owner.Equals(user.User.Sid) {
		t.Fatalf("integration operation lock owner = %v, want current user", owner)
	}
}

func TestWindowsIntegrationOperationLockRefusesReparsePoint(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "foreign")
	path := filepath.Join(root, "operation.lock")
	if err := os.WriteFile(target, []byte("foreign\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("Windows symlink creation unavailable: %v", err)
	}
	if release, err := acquireIntegrationOperationLock(path, "test", "busy"); err == nil {
		release()
		t.Fatal("Windows integration lock followed a reparse point")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "foreign\n" {
		t.Fatalf("reparse target changed to %q", raw)
	}
}
