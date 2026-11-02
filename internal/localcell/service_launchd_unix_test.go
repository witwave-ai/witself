//go:build !windows

package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLaunchdDirectoryOwnershipRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, step string
		sentinel   error
	}{
		{"symlink", "agent_dir_walk:2:type", ErrServiceOwnershipConflict},
		{"ancestor_symlink", "agent_dir_walk:1:type", ErrServiceOwnershipConflict},
		{"unowned", "agent_dir_walk:2:owner", ErrServiceOwnershipConflict},
		{"unwritable", "agent_dir_walk:2:access", ErrServiceManagerUnavailable},
		{"shared", "agent_dir_walk:2:mode", ErrServiceOwnershipConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			if err := os.MkdirAll(a.agentDir, 0o700); err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "symlink", "ancestor_symlink":
				target := a.agentDir
				if tc.name == "ancestor_symlink" {
					target = filepath.Dir(target)
				}
				if err := os.RemoveAll(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), target); err != nil {
					t.Fatal(err)
				}
			case "unowned":
				a.owned = func(path string, _ os.FileInfo) bool { return path != a.agentDir }
			case "unwritable":
				if err := os.Chmod(a.agentDir, 0o500); err != nil {
					t.Fatal(err)
				}
			case "shared":
				if err := os.Chmod(a.agentDir, 0o777); err != nil {
					t.Fatal(err)
				}
			}
			_, err := a.Plan(t.Context(), d)
			launchdRequireError(t, err, tc.sentinel, tc.step)
			launchdRequireNoMutation(t, m)
		})
	}
}

func TestLaunchdApplyOrderingIdempotenceAndExactRemoval(t *testing.T) {
	a, d, m := launchdFixture(t)
	if err := os.MkdirAll(filepath.Join(d.Root, "service"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(d.Root, "retained-data")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	launchdInstallFixture(t, a, d)
	for _, path := range []string{filepath.Dir(a.agentDir), a.agentDir} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatal("registration directory not private")
		}
	}
	info, err := os.Lstat(a.plistPath(d))
	if err != nil || info.Mode() != 0o600 {
		t.Fatal("plist not private")
	}
	var mutations []string
	for _, call := range m.calls {
		if call[1] != "print" {
			mutations = append(mutations, call[1])
		}
	}
	if !slices.Equal(mutations, []string{"enable", "bootstrap"}) {
		t.Fatal("enable did not precede exact bootstrap")
	}
	m.calls = nil
	launchdInstallFixture(t, a, d)
	launchdRequireNoMutation(t, m)
	if err := a.Remove(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(a.plistPath(d)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("plist not removed")
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "keep" {
		t.Fatal("removal changed cell data")
	}
	if err := a.Remove(t.Context(), d); err != nil {
		t.Fatal("absent removal not idempotent")
	}
}

func TestLaunchdForeignPlistRefused(t *testing.T) {
	for _, kind := range []string{"bytes", "symlink", "mode", "owner"} {
		t.Run(kind, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			if err := os.MkdirAll(a.agentDir, 0o700); err != nil {
				t.Fatal(err)
			}
			want, err := RenderLaunchdAgentPlist(d)
			if err != nil {
				t.Fatal(err)
			}
			step := "plist_file:content"
			switch kind {
			case "bytes":
				want = []byte("foreign")
			case "owner":
				a.owned = func(path string, _ os.FileInfo) bool { return path != a.plistPath(d) }
				step = "plist_file:owner"
			}
			if err := os.WriteFile(a.plistPath(d), want, 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Rename(a.plistPath(d), a.plistPath(d)+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(a.plistPath(d)+".target", a.plistPath(d)); err != nil {
					t.Fatal(err)
				}
				step = "plist_file:type"
			}
			if kind == "mode" {
				if err := os.Chmod(a.plistPath(d), 0o644); err != nil {
					t.Fatal(err)
				}
				step = "plist_file:mode"
			}
			plan := ServicePlan{Spec: d, RequiredPrivilege: "user"}
			launchdRequireError(t, a.Apply(t.Context(), plan), ErrServiceOwnershipConflict, step)
			launchdRequireError(t, a.Remove(t.Context(), d), ErrServiceOwnershipConflict, "remove:ownership")
			if _, err := os.Lstat(a.plistPath(d)); err != nil {
				t.Fatal("foreign file removed")
			}
			launchdRequireNoMutation(t, m)
		})
	}
}
