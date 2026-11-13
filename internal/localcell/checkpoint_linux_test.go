//go:build linux

package localcell

import (
	"os"
	"testing"
)

// A PGDATA name the inventory cannot spell refuses the copy plan before any
// write (XR1-04). Linux file systems accept the name; APFS and HFS+ refuse it,
// so on macOS none can reach a checkpoint, and TestCheckpointNameSupported
// decides the name on every OS.
func TestCheckpointSourceRefusesNonUTF8Name(t *testing.T) {
	f, r := checkpointFixture(t, "legacy", false)
	if os.WriteFile(f.cell.path("pg", "base", "1", "x\xff"), []byte("synthetic relation bytes"), 0o600) != nil {
		t.Fatal("this file system refused a non-UTF-8 name; the row needs one that accepts it, as Linux file systems do")
	}
	before := checkpointTreeState(t, f.cell)
	err := f.cell.Checkpoint(t.Context(), checkpointOptions(f))
	if checkpointUnchanged(t, f.cell, before); !checkpointSameError(err, "local_checkpoint_source_unsupported") || r.calls["control"] != 0 {
		t.Fatal("a non-UTF-8 PGDATA name was admitted to the copy plan")
	}
}
