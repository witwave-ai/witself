//go:build !windows

package main

import (
	"os"
	"testing"
)

func privateHarnessRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal("cannot make harness test root private")
	}
	return root
}
