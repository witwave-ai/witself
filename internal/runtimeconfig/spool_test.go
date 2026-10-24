package runtimeconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLocalSpoolRejectsSymlinkComponents(t *testing.T) {
	parent := privateDir(t)
	dir := filepath.Join(parent, "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(dir, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("LC-S2-PW3: symlink fixture creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{alias, filepath.Join(alias, "child")} {
		if ValidateSpoolDir(path) == nil {
			t.Error("symlink component accepted by spool validation")
		}
		root, err := OpenSpoolRoot(path)
		if root != nil {
			_ = root.Close()
		}
		if err == nil {
			t.Error("symlink component accepted by spool root opener")
		}
	}
	root, err := OpenSpoolRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
}
