//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServicePayloadRootSymlinkSpelling(t *testing.T) {
	realParent := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realParent, link); err != nil {
		t.Fatal("cannot create installation root symlink fixture")
	}
	realRoot, otherRoot := filepath.Join(realParent, "cell"), filepath.Join(realParent, "other")
	for _, root := range []string{realRoot, otherRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal("cannot create installation root fixture")
		}
	}
	aliasRoot := filepath.Join(link, "cell")
	if !sameInstallationRoot(aliasRoot, realRoot) || !sameInstallationRoot(realRoot, aliasRoot) {
		t.Fatal("symlink spelling changed installation root identity")
	}
	assertTaskHostRootIdentity(t, aliasRoot, realRoot, otherRoot)
}
