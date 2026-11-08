//go:build windows

package localcell

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCanonicalRootWindowsShortName(t *testing.T) {
	long, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("cannot resolve long installation parent spelling")
	}
	root := filepath.Join(long, "cell")
	longRoot, err := CanonicalRoot(root)
	if err != nil || longRoot != root {
		t.Fatal("long spelling did not identify the canonical root")
	}
	again, err := CanonicalRoot(longRoot)
	if err != nil || again != longRoot {
		t.Fatal("long root canonicalization is not idempotent")
	}

	pointer, err := windows.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal("cannot encode installation parent spelling")
	}
	length, err := windows.GetShortPathName(pointer, nil, 0)
	if err != nil || length == 0 {
		t.Fatal("cannot size short installation parent spelling")
	}
	buffer := make([]uint16, length)
	written, err := windows.GetShortPathName(pointer, &buffer[0], uint32(len(buffer)))
	if err != nil || written == 0 || written >= uint32(len(buffer)) {
		t.Fatal("cannot resolve short installation parent spelling")
	}
	short := filepath.Clean(windows.UTF16ToString(buffer[:written]))
	if short != filepath.Clean(long) {
		t.Run("short_name", func(t *testing.T) {
			aliasRoot, err := CanonicalRoot(filepath.Join(short, "cell"))
			if err != nil || aliasRoot != longRoot {
				t.Fatal("short and long names did not identify the same root")
			}
		})
	}
}
