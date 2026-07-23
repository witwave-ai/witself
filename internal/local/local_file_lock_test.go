package local

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalLockFileOpenMaintainsStableIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stable.lock")
	first, created, err := openLocalLockFileNoFollow(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if !created {
		t.Fatal("first lock open did not report creation")
	}
	second, created, err := openLocalLockFileNoFollow(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if created {
		t.Fatal("existing lock open reported creation")
	}
	firstInfo, err := first.Stat()
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := second.Stat()
	if err != nil {
		t.Fatal(err)
	}
	linkedInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, secondInfo) || !os.SameFile(firstInfo, linkedInfo) {
		t.Fatal("lock opens did not preserve one stable file identity")
	}
	if err := lockLocalFile(first); err != nil {
		t.Fatal(err)
	}
	if err := unlockLocalFile(first); err != nil {
		t.Fatal(err)
	}
}

func TestLocalLockFileOpenRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "linked.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	file, _, err := openLocalLockFileNoFollow(path, false)
	if file != nil {
		_ = file.Close()
		t.Fatal("no-follow lock open returned a file for a symlink")
	}
	if err == nil {
		t.Fatal("no-follow lock open accepted a symlink")
	}
	raw, readErr := os.ReadFile(target)
	if readErr != nil || string(raw) != "unchanged" {
		t.Fatalf("symlink target changed: %q, %v", raw, readErr)
	}
}
