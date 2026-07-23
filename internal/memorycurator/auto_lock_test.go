package memorycurator

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAutoLockPlatformHelperPreservesExclusiveTryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto.lock")
	first, err := openAutoLockFileNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := openAutoLockFileNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if _, err := validateAutoLockFileIdentity(path, first); err != nil {
		t.Fatal(err)
	}
	if _, err := validateAutoLockFileIdentity(path, second); err != nil {
		t.Fatal(err)
	}
	acquired, err := tryLockAutoFile(first)
	if err != nil || !acquired {
		t.Fatalf("first try-lock = %t, %v", acquired, err)
	}
	acquired, err = tryLockAutoFile(second)
	if err != nil || acquired {
		t.Fatalf("contending try-lock = %t, %v", acquired, err)
	}
	if err := unlockAutoFile(first); err != nil {
		t.Fatal(err)
	}
	acquired, err = tryLockAutoFile(second)
	if err != nil || !acquired {
		t.Fatalf("try-lock after release = %t, %v", acquired, err)
	}
	if err := unlockAutoFile(second); err != nil {
		t.Fatal(err)
	}
}

func TestAutoLockPlatformHelperRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "linked.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	file, err := openAutoLockFileNoFollow(path)
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

func TestAutoLockIdentityRejectsPathReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "stable.lock")
	file, err := openAutoLockFileNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := validateAutoLockFileIdentity(path, file); err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(directory, "displaced.lock")
	if err := os.Rename(path, displaced); err != nil {
		if runtime.GOOS == "windows" {
			// Windows denies rename because the open handle intentionally omits
			// FILE_SHARE_DELETE, preserving the stable lock path in the kernel.
			return
		}
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateAutoLockFileIdentity(path, file); err == nil {
		t.Fatal("replacement lock path retained the opened file identity")
	}
}
