//go:build windows

package runtimeconfig

import (
	"path/filepath"
	"testing"
)

func privateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	privateMkdir(t, dir)
	return dir
}

func privateMkdir(t *testing.T, path string) {
	t.Helper()
	if err := WindowsPrivateMkdir(path); err != nil {
		t.Fatal(err)
	}
}
