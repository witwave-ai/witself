//go:build !windows

package localcell

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestServiceSystemdDirectoryWalkRechecksManagerEnablement(t *testing.T) {
	for _, linkKind := range []string{"absolute", "relative"} {
		t.Run(linkKind, func(t *testing.T) {
			a, d, m := systemdFixture(t)
			a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
				output, err := m.command(ctx, binary, args...)
				if err != nil || binary != a.systemctl || len(args) < 2 || args[1] != "enable" {
					return output, err
				}
				wants := filepath.Join(a.unitDir, "default.target.wants")
				if err := os.Mkdir(wants, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(wants, 0o755); err != nil {
					t.Fatal(err)
				}
				target := a.unitPath(d)
				if linkKind == "relative" {
					target = filepath.Join("..", serviceUnitLabel(d))
				}
				if err := os.Symlink(target, filepath.Join(wants, serviceUnitLabel(d))); err != nil {
					t.Fatal(err)
				}
				return output, nil
			}
			plan, err := a.Plan(t.Context(), d)
			if err != nil {
				t.Fatal(err)
			}
			// Apply exercises the real create walk while the fixture manager avoids
			// invoking systemd. Its new directories remain private.
			if err := a.Apply(t.Context(), plan); err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{filepath.Join(a.home, ".config"), filepath.Join(a.home, ".config", "systemd"), a.unitDir} {
				info, err := os.Lstat(dir)
				if err != nil || info.Mode().Perm() != 0o700 {
					t.Fatal("Apply did not create private registration directories")
				}
			}
			for _, create := range []bool{true, false} {
				if err := a.inspectDirectories(create); err != nil {
					t.Fatal("directory walk rejected manager-created enablement")
				}
			}
			if err := a.capability(t.Context()); err != nil {
				t.Fatal("capability rejected Apply's tree after manager enablement")
			}
			if err := a.inspectEnablement(d); err != nil {
				t.Fatal("ownership inspection rejected exact manager-created enablement")
			}
			if err := a.Remove(t.Context(), d); err != nil {
				t.Fatal("Remove rejected Apply's tree after manager enablement")
			}
		})
	}
}

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
