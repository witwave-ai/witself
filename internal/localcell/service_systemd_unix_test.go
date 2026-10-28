//go:build !windows

package localcell

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServiceSystemdExternalConfigSymlinkAncestorRefused(t *testing.T) {
	home, config := t.TempDir(), t.TempDir()
	config, err := filepath.EvalSymlinks(config)
	if err != nil {
		t.Fatal("cannot canonicalize external config fixture")
	}
	if err := os.Mkdir(filepath.Join(config, "real"), 0o700); err != nil {
		t.Fatal("cannot prepare external config fixture")
	}
	if err := os.Symlink(filepath.Join(config, "real"), filepath.Join(config, "link")); err != nil {
		t.Fatal("cannot prepare config symlink fixture")
	}
	lookup := func() (string, error) { return home, nil }
	for _, path := range []string{filepath.Join(config, "link"), filepath.Join(config, "link", "missing-config")} {
		_, base, unit := systemdUserDirectories(lookup, lookup, path)
		if base != "" || unit != "" {
			t.Fatal("external config symlink selected a writable registration tree")
		}
	}
}
