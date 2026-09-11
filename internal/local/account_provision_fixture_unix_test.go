//go:build !windows

package local

import (
	"os"
	"path/filepath"
	"testing"
)

func privateAccountProvisionTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func missingAccountProvisionTestHome(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "missing", ".witself")
}

func assertAccountProvisionPrivateTestPath(t *testing.T, path string, directory bool) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if directory {
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v", path, info.Mode())
		}
	} else if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode = %v", path, info.Mode())
	}
}

func makeAccountProvisionTestPathReadable(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
}

func writeAccountProvisionPrivateTestFile(path string, raw []byte) error {
	return os.WriteFile(path, raw, 0o600)
}

func createAccountProvisionPrivateTestTokenDirs(t *testing.T, home, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "tokens", "accounts", name), 0o700); err != nil {
		t.Fatal(err)
	}
}
