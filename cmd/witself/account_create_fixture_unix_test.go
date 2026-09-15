//go:build !windows

package main

import (
	"os"
	"testing"
)

func newAccountCreateTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func assertAccountCreatePrivateTestFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode = %v", path, info.Mode())
	}
}

func writeAccountCreatePrivateTestFile(path string, raw []byte) error {
	return os.WriteFile(path, raw, 0o600)
}
