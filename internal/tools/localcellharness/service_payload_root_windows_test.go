//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestServicePayloadRootShortNameSpelling(t *testing.T) {
	long, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("cannot resolve long installation parent spelling")
	}
	realRoot, otherRoot := filepath.Join(long, "cell"), filepath.Join(long, "other")
	for _, root := range []string{realRoot, otherRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal("cannot create installation root fixture")
		}
	}
	if !sameInstallationRoot(realRoot, realRoot) {
		t.Fatal("identical spelling changed installation root identity")
	}

	name, err := windows.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal("cannot encode installation parent spelling")
	}
	size, err := windows.GetShortPathName(name, nil, 0)
	if err != nil || size == 0 {
		t.Fatal("cannot size short installation parent spelling")
	}
	buffer := make([]uint16, size)
	n, err := windows.GetShortPathName(name, &buffer[0], uint32(len(buffer)))
	if err != nil || n == 0 || n >= uint32(len(buffer)) {
		t.Fatal("cannot resolve short installation parent spelling")
	}
	short := filepath.Clean(windows.UTF16ToString(buffer[:n]))
	if short != filepath.Clean(long) {
		t.Run("short_name", func(t *testing.T) {
			aliasRoot := filepath.Join(short, "cell")
			if !sameInstallationRoot(aliasRoot, realRoot) || !sameInstallationRoot(realRoot, aliasRoot) {
				t.Fatal("alias spelling changed installation root identity")
			}
			assertTaskHostRootIdentity(t, aliasRoot, realRoot, otherRoot)
		})
	} else {
		t.Run("same_name", func(t *testing.T) {
			assertTaskHostRootIdentity(t, realRoot, realRoot, otherRoot)
		})
	}
}
