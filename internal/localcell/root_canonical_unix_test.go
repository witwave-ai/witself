//go:build !windows

package localcell

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalRootSymlinkParent(t *testing.T) {
	realParent := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realParent, link); err != nil {
		t.Fatal(err)
	}
	linkRoot, err := CanonicalRoot(filepath.Join(link, "cell"))
	if err != nil {
		t.Fatal(err)
	}
	realRoot, err := CanonicalRoot(filepath.Join(realParent, "cell"))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := filepath.EvalSymlinks(realParent)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(parent, "cell"); linkRoot != realRoot || linkRoot != want {
		t.Fatalf("parent aliases did not identify the same root: link=%q real=%q want=%q", linkRoot, realRoot, want)
	}
}
