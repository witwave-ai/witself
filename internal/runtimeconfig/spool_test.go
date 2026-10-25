package runtimeconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLocalSpoolRejectsReplacementDuringValidation(t *testing.T) {
	dir := privateDir(t)
	root, err := openSpoolRoot(dir, func(path string) (os.FileInfo, error) {
		before, err := validatedSpoolDirectory(path)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(path, path+"-retired"); err != nil {
			t.Fatal(err)
		}
		privateMkdir(t, path)
		return before, nil
	})
	if root != nil {
		_ = root.Close()
		t.Fatal("replacement directory escaped the identity check")
	}
	if err != errSpoolIdentity {
		t.Fatal("replacement must produce the fixed value-free refusal")
	}
}

func TestLocalSpoolRejectsSymlinkComponents(t *testing.T) {
	parent := privateDir(t)
	dir := filepath.Join(parent, "private")
	privateMkdir(t, dir)
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(dir, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("LC-S2-PW3: symlink fixture creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
	privateMkdir(t, filepath.Join(dir, "child"))
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
