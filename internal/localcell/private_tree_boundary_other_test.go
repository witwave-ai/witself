//go:build !windows

package localcell

import (
	"os"
	"testing"
)

// recoveryBoundaryLink creates a symbolic link fixture at path.
func recoveryBoundaryLink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal("cannot create recovery boundary link fixture")
	}
}

// recoveryBoundaryNonPrivate makes an existing fixture object readable by
// every user, which the private-object rules refuse.
func recoveryBoundaryNonPrivate(t *testing.T, path string, directory bool) {
	t.Helper()
	mode := os.FileMode(0o644)
	if directory {
		mode = 0o755
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal("cannot weaken recovery boundary fixture permissions")
	}
}
