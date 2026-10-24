//go:build !windows

package localcell

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestUnixChildrenReceiveExactPrivateEnvironmentValues(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("PATH", filepath.Join(parent, "synthetic-parent-path"))
	t.Setenv("HOME", filepath.Join(parent, "synthetic-parent-home"))
	t.Setenv("TMPDIR", parent)
	for _, component := range []string{"server", "worker"} {
		t.Run(component, func(t *testing.T) {
			c, m := unitCell(t)
			c.cfg.Binaries.PostgresDir = t.TempDir()
			if err := c.makePassword(); err != nil {
				t.Fatal(err)
			}
			environment, err := c.childEnvironment(m, component)
			if err != nil {
				t.Fatal(err)
			}
			// Only shell builtins are used: the poisoned parent PATH cannot
			// affect fixture lookup, and no environment values are printed.
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", `
test "$PATH" = "$1" || exit 10
test "$HOME" = "$2" || exit 11
test "$TMPDIR" = "$3" || exit 12
`, "environment-values", c.cfg.Binaries.PostgresDir, c.path("home"), c.path("tmp"))
			cmd.Env, cmd.Dir = environment, c.cfg.Root
			if err := cmd.Run(); err != nil {
				t.Fatal("child PATH, HOME or TMPDIR differs from its exact private path")
			}
		})
	}
}

func terminateEnvironmentFixture(record ProcessRecord) error {
	process, err := os.FindProcess(record.PID)
	if err != nil {
		return err
	}
	defer func() { _ = process.Release() }()
	return process.Kill()
}
