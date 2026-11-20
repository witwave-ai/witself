//go:build windows

package localcell

import (
	"errors"
	"io/fs"
)

// discardOwnCheckpoint removes this call's directory only while it has no
// witness, through held roots; a witness in it, bound to both names, is not a
// failed discard. A failed create runs it once and reuses the result. Discarded
// needs recovery/ bound and nothing left at the name: a link the hold found with
// nothing behind it is removed itself through the held recovery/ (window 1 of
// removeCheckpointHeld); anything else left is incomplete and never removed.
func (c *Cell) discardOwnCheckpoint(checkpointID string) error {
	if _, err := c.checkpointDir(checkpointID); err != nil {
		return err
	}
	recovery, checkpoint, err := c.holdCheckpoint(checkpointID)
	defer closeCheckpointRoots(recovery, checkpoint)
	held := err == nil
	if !held && (recovery == nil || !errors.Is(err, fs.ErrNotExist)) {
		return errCheckpointCopy
	} else if held {
		switch _, err := checkpoint.Lstat(checkpointWitnessName); {
		case err == nil && c.checkpointRootsBound(recovery, checkpoint, checkpointID):
			return nil
		case !errors.Is(err, fs.ErrNotExist) || c.removeCheckpointHeld(recovery, checkpoint, checkpointID) != nil:
			return errCheckpointCopy
		}
	}
	if !c.checkpointRootsBound(recovery, nil, checkpointID) {
		return errCheckpointCopy
	}
	switch left, err := recovery.Lstat(checkpointID); {
	case errors.Is(err, fs.ErrNotExist):
	case !held && err == nil && left.Mode()&fs.ModeSymlink != 0 && recovery.Remove(checkpointID) == nil:
	default:
		return errCheckpointCopy
	}
	if held {
		return nil
	}
	return syncCheckpointRoot(recovery)
}
