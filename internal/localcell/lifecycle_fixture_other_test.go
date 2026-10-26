//go:build !windows

package localcell_test

import (
	"os"
	"testing"
)

func shortLifecycleRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "lc-")
	if err != nil {
		t.Fatal("cannot allocate disposable cell")
	}
	return root
}

func lifecycleWriteFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }
