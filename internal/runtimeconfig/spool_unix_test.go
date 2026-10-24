//go:build !windows

package runtimeconfig

import (
	"os"
	"syscall"
	"testing"
)

type ownedInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (i ownedInfo) Sys() any { return &i.stat }

func TestLocalSpoolOwnershipAndModes(t *testing.T) {
	dir := privateDir(t)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	stat := *info.Sys().(*syscall.Stat_t)
	stat.Uid = uint32(os.Geteuid()) + 1
	if privateSpoolOwner(dir, ownedInfo{info, stat}) {
		t.Fatal("foreign owner accepted")
	}
	for _, mode := range []os.FileMode{0o710, 0o720, 0o740, 0o701, 0o702, 0o704, 0o755} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		if err := ValidateSpoolDir(dir); err == nil {
			t.Fatalf("spool mode %o accepted", mode)
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}
