//go:build !windows

package localcell

import (
	"os"
	"testing"
)

func testPrivateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func testMkdir(path string, mode os.FileMode) error { return os.Mkdir(path, mode) }

func testWriteFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}
