//go:build windows

package localcell

import (
	"path/filepath"
	"testing"
)

// The strict Windows rule below recovery/: a widened DACL deep in a checkpoint
// and a junction are refused by RV, so Checkpoint, DeleteCheckpoint and
// Inspect refuse too.
func TestCheckpointRecoveryValidatorWindows(t *testing.T) {
	t.Run("widened-dacl-deep", func(t *testing.T) {
		f, _ := checkpointFixture(t, "legacy", false)
		dir := checkpointPlant(t, f.cell, "ckpt_aaaaaaaaaaaaaaaa", "ckpt_aaaaaaaaaaaaaaaa", "pg/base/1/1259")
		setTestDACL(t, filepath.Join(dir, "pg", "base", "1", "1259"), "(A;;FR;;;WD)", false, false)
		checkpointRefusedEverywhere(t, f)
	})
	t.Run("junction", func(t *testing.T) {
		f, _ := checkpointFixture(t, "legacy", false)
		dir := checkpointPlant(t, f.cell, "ckpt_aaaaaaaaaaaaaaaa", "ckpt_aaaaaaaaaaaaaaaa")
		createTestJunction(t, filepath.Join(dir, "junction"), testPrivateRoot(t))
		checkpointRefusedEverywhere(t, f)
	})
}
