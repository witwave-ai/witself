//go:build darwin || linux

package localcell

import (
	"os"
	"syscall"
	"testing"
)

type testOwnedDirectoryInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (info testOwnedDirectoryInfo) Sys() any { return &info.stat }

func TestPrivateOwnedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !privateOwnedDir(root, info) {
		t.Fatalf("private directory owned by the current user was rejected: mode=%o owner=%d euid=%d", info.Mode().Perm(), info.Sys().(*syscall.Stat_t).Uid, os.Geteuid())
	}
	stat := *info.Sys().(*syscall.Stat_t)
	stat.Uid++
	if privateOwnedDir(root, testOwnedDirectoryInfo{info, stat}) {
		t.Fatal("directory owned by another user was accepted")
	}
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if privateOwnedDir(root, info) {
		t.Fatal("directory readable by another user was accepted")
	}
}
