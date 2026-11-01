//go:build !windows

package localcell_test

import (
	"os"
	"path/filepath" // FLAKE-3 fix 5 (W1)
	"testing"
)

func shortLifecycleRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "lc-")
	if err != nil {
		t.Fatal("cannot allocate disposable cell")
	}
	return root
}

func lifecycleWriteFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }
func TestFixturePostgresStopWaitsForExitAliasedRoot(t *testing.T) { // FLAKE-3 fix 5 (W1)
	binaries := buildLifecycleFixture(t)              // FLAKE-3 fix 5 (W1)
	t.Setenv("WITSELF_HOME", t.TempDir())             // FLAKE-3 fix 5 (W1)
	t.Setenv("DSH_HOME", t.TempDir())                 // FLAKE-3 fix 5 (W1)
	parent := t.TempDir()                             // FLAKE-3 fix 5 (W1)
	alias := filepath.Join(t.TempDir(), "alias")      // FLAKE-3 fix 5 (W1)
	if err := os.Symlink(parent, alias); err != nil { // FLAKE-3 fix 5 (W1)
		t.Fatal("stoplink-a01 aliased parent setup failed") // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
	t.Setenv("TMPDIR", alias)               // FLAKE-3 fix 5 (W1)
	t.Run("PGCtlStop", func(t *testing.T) { // FLAKE-3 fix 5 (W1)
		testFixturePostgresStopWaitsForExit(t, binaries, true, true) // FLAKE-3 fix 5 (W1)
	}) // FLAKE-3 fix 5 (W1)
} // FLAKE-3 fix 5 (W1)
