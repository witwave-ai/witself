//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopilotOperationLockSerializesMutations(t *testing.T) {
	root := filepath.Join(t.TempDir(), "copilot-home")
	t.Setenv("COPILOT_HOME", root)

	firstRelease, err := acquireCopilotOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	if release, err := acquireCopilotOperationLock(); err == nil {
		release()
		t.Fatal("second GitHub Copilot operation acquired the live lock")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second lock error = %v", err)
	}
	firstRelease()
	firstRelease()

	thirdRelease, err := acquireCopilotOperationLock()
	if err != nil {
		t.Fatalf("lock remained held after release: %v", err)
	}
	thirdRelease()

	path := filepath.Join(root, copilotOperationLockFile)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("operation lock mode = %v", info.Mode())
	}
}

func TestCopilotOperationLockRefusesSymlink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "copilot-home")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPILOT_HOME", root)

	target := filepath.Join(t.TempDir(), "foreign-lock")
	if err := os.WriteFile(target, []byte("foreign\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, copilotOperationLockFile)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if release, err := acquireCopilotOperationLock(); err == nil {
		release()
		t.Fatal("GitHub Copilot operation lock followed a symlink")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "foreign\n" {
		t.Fatalf("symlink target changed to %q", raw)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target mode changed to %04o", info.Mode().Perm())
	}
}

func TestCopilotOperationLockRepairsFileMode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "copilot-home")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPILOT_HOME", root)
	path := filepath.Join(root, copilotOperationLockFile)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	release, err := acquireCopilotOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	release()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("operation lock mode = %v", info.Mode())
	}
}

func TestCopilotCommandsRespectOperationLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "copilot-home")
	t.Setenv("COPILOT_HOME", root)

	release, err := acquireCopilotOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	for _, test := range []struct {
		name    string
		command func([]string) int
	}{
		{name: "install", command: installCmd},
		{name: "uninstall", command: uninstallCmd},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, stderr, code := captureIntegrationsCLI(t, func() int {
				return test.command([]string{"copilot"})
			})
			if code != 1 || !strings.Contains(stderr, "another GitHub Copilot install, uninstall, or routing refresh is already running") {
				t.Fatalf("code = %d stderr = %q", code, stderr)
			}
		})
	}
}
