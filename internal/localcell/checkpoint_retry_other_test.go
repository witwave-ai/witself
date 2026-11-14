//go:build !windows

package localcell

import (
	"io/fs"
	"os"
	"testing"
)

// The hermetic cell still supplies its control/free fixture boundaries; only
// publish replaces a retry operation, leaving the production predicate intact.
func TestCheckpointRetryOtherProductionPredicate(t *testing.T) {
	f, _ := checkpointFixture(t, "legacy", false)
	attempts := 0
	f.cell.checkpointTest.publish = func(oldPath, newPath string) error {
		attempts++
		if attempts == 1 {
			return &fs.PathError{Op: "rename", Err: os.ErrPermission}
		}
		return publishPrivateFile(oldPath, newPath)
	}
	if err := f.cell.Checkpoint(t.Context(), checkpointOptions(f)); !checkpointSameError(err, "local_checkpoint_copy_failed") || attempts != 1 {
		t.Fatal("off-Windows publication did not preserve one-attempt refusal")
	}
}
