//go:build windows

package main

import (
	"path/filepath"
	"testing"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

func privateHarnessRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cell")
	if err := runtimeconfig.WindowsPrivateMkdir(root); err != nil {
		t.Fatal("cannot make harness test root private")
	}
	return root
}
