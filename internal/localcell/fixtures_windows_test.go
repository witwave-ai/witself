//go:build windows

package localcell

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

// TempDir inherits its parent's ACL; cell fixtures deliberately create a fresh
// protected child instead of weakening the production privacy boundary.
func testPrivateRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cell")
	if err := privateDir(root, true); err != nil {
		t.Fatal(err)
	}
	return root
}

func testMkdir(path string, _ os.FileMode) error { return privateDir(path, true) }

func testWriteFile(path string, data []byte, _ os.FileMode) error {
	f, err := runtimeconfig.WindowsPrivateOpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
