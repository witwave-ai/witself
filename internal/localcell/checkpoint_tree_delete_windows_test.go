//go:build windows

package localcell

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func blockCheckpointReceiptSweep(t *testing.T, dir string) func() {
	return drillRootLock(t, filepath.Join(dir, checkpointInventoryName))
}

func init() {
	heldIdentifierRow = func(t *testing.T) {
		// An identifier that is not a checkpoint's opens and removes nothing.
		t.Run("identifier", func(t *testing.T) {
			_, c := heldFixture(t)
			before := checkpointTreeState(t, c)
			recovery, checkpoint, err := c.holdCheckpoint(".")
			held, target, heldErr := c.holdCheckpoint(heldTarget)
			defer closeCheckpointRoots(held, target)
			if recovery != nil || checkpoint != nil || !errors.Is(err, errCheckpointIdentity) || heldErr != nil ||
				!errors.Is(c.removeCheckpointHeld(held, target, "."), errCheckpointIdentity) || !reflect.DeepEqual(before, checkpointTreeState(t, c)) {
				t.Fatal("a held root helper accepted an identifier that is not a checkpoint's")
			}
		})
	}
}
