package server

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSweepStaleArchiveSpools(t *testing.T) {
	dir := t.TempDir()
	write := func(path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("spool"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	removed := []string{"witself-backup-validate-1.tar.gz", "witself-account-import-1234567890.tar.gz", "witself-self-export-42.tar.gz"}
	survive := []string{
		"witself-backup-validate-123.tar.gz.keep", "witself-backup-validate-abc.tar.gz",
		"witself-backup-validate-.tar.gz", "xwitself-account-import-1.tar.gz",
		"witself-account-import-1.tar", "witself-other-1.tar.gz",
		"witself-account-import-12345678901.tar.gz",
	}
	for _, name := range append(removed, survive...) {
		write(filepath.Join(dir, name))
	}
	for _, name := range []string{"witself-self-export-9.tar.gz", "sub"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "sub", "witself-account-import-5.tar.gz"))
	target := filepath.Join(t.TempDir(), "target")
	write(target)
	if err := os.Symlink(target, filepath.Join(dir, "witself-account-import-7.tar.gz")); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	SweepStaleArchiveSpools(dir, &log)
	for _, name := range removed {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("matching spool survived: %s", name)
		}
	}
	for _, name := range append(survive, "witself-self-export-9.tar.gz", "witself-account-import-7.tar.gz", "sub/witself-account-import-5.tar.gz") {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("non-spool removed: %s", name)
		}
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "spool" {
		t.Fatal("symlink target changed")
	}
	want := fmt.Sprintf("witself-server: stale archive spool sweep dir=%q removed=3 bytes=15 failed=0\n", dir)
	if log.String() != want {
		t.Fatalf("sweep log = %q, want %q", log.String(), want)
	}
}

func TestSweepStaleArchiveSpoolsUnreadableDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	var log bytes.Buffer
	SweepStaleArchiveSpools(dir, &log)
	want := fmt.Sprintf("witself-server: stale archive spool sweep dir=%q removed=0 bytes=0 failed=1\n", dir)
	if log.String() != want {
		t.Fatalf("sweep log = %q, want %q", log.String(), want)
	}
}
