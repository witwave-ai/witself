//go:build !windows

package capturemix

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

func TestProcessDirectoriesMeetProductionSpoolContract(t *testing.T) {
	directory, err := newProcessDirectory()
	if err != nil {
		t.Fatal("capturemix private process directory setup failed")
	}
	t.Cleanup(func() {
		if os.RemoveAll(directory) != nil {
			t.Error("capturemix private process directory cleanup failed")
		}
	})
	t.Setenv("TMPDIR", filepath.Join(directory, "tmp"))
	if runtimeconfig.ValidateSpoolDir(filepath.Join(directory, "spool")) != nil || runtimeconfig.ValidateSpoolDir(filepath.Join(directory, "tmp")) == nil {
		t.Fatal("capturemix spool aliases the child temporary directory or fails production validation")
	}
	if canonical, err := filepath.EvalSymlinks(directory); err != nil || canonical != directory {
		t.Fatal("capturemix child directory retained a noncanonical ancestor")
	}
}
