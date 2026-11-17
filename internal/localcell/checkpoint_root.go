package localcell

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// checkpointRootHere, as a path's last element, makes the element before it a
// directory component. os.OpenRoot sets no directory flag: Unix resolution
// refuses a FIFO, device or file at recovery/ unopened, Windows opens a file and
// refuses it; a root opens the name O_DIRECTORY|O_NOFOLLOW (FILE_DIRECTORY_FILE,
// no reparse). Nothing waits. Links are followed, at the name only inside
// recovery/, and the binds refuse them.
const checkpointRootHere = string(filepath.Separator) + "."

// errCheckpointRootMoved: recovery/ or the checkpoint's name no longer names
// the held directory. A step returning it fails with the step's fixed text.
var errCheckpointRootMoved = errors.New("local cell checkpoint directory moved")

// syncCheckpointRoot syncs a held directory. Tests replace it; production
// always uses syncRootDirectory.
var syncCheckpointRoot = syncRootDirectory

// syncRootDirectory syncs a held directory through the root. Windows has no
// portable directory sync (as syncDirectory).
func syncRootDirectory(held *os.Root) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	handle, err := held.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	return handle.Sync()
}

// checkpointStage reports a held-root stage to the test hook.
func (c *Cell) checkpointStage(stage string) {
	if c.checkpointTest != nil && c.checkpointTest.beforeStep != nil {
		c.checkpointTest.beforeStep(stage, "")
	}
}

// holdCheckpoint opens recovery/ and then the checkpoint directory below it as
// roots, each as a directory only; recovery is returned whenever it opened. An
// identifier checkpointDir refuses opens nothing.
func (c *Cell) holdCheckpoint(checkpointID string) (recovery, checkpoint *os.Root, err error) {
	if _, err = c.checkpointDir(checkpointID); err != nil {
		return nil, nil, err
	}
	c.checkpointStage("hold")
	recovery, err = os.OpenRoot(c.recoveryDir() + checkpointRootHere)
	if err != nil {
		return nil, nil, err
	}
	checkpoint, err = recovery.OpenRoot(checkpointID + checkpointRootHere)
	return recovery, checkpoint, err
}

// closeCheckpointRoots closes each root holdCheckpoint opened.
func closeCheckpointRoots(recovery, checkpoint *os.Root) {
	if checkpoint != nil {
		_ = checkpoint.Close()
	}
	if recovery != nil {
		_ = recovery.Close()
	}
}

// checkpointRootsBound reports whether recovery/ and the checkpoint's name
// still name the held directories: held identities come from the handles,
// current ones from the names without following their last element. A nil
// checkpoint binds recovery/ alone.
func (c *Cell) checkpointRootsBound(recovery, checkpoint *os.Root, checkpointID string) bool {
	recoveryHeld, err := recovery.Stat(".")
	recoveryNow, nowErr := os.Lstat(c.recoveryDir())
	if err != nil || nowErr != nil || !os.SameFile(recoveryHeld, recoveryNow) {
		return false
	} else if checkpoint == nil {
		return true
	}
	checkpointHeld, err := checkpoint.Stat(".")
	checkpointNow, nowErr := recovery.Lstat(checkpointID)
	return err == nil && nowErr == nil && os.SameFile(checkpointHeld, checkpointNow)
}

// unsealCheckpointHeld removes the witness through the held checkpoint
// directory once both names are bound, then syncs that directory; an absent
// witness is already removed. On Windows a witness that is an empty directory,
// is read-only or changed after admission is now removed, not refused as before.
func (c *Cell) unsealCheckpointHeld(recovery, checkpoint *os.Root, checkpointID string) error {
	c.checkpointStage("unseal")
	if !c.checkpointRootsBound(recovery, checkpoint, checkpointID) {
		return errCheckpointRootMoved
	}
	if err := checkpoint.Remove(checkpointWitnessName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncCheckpointRoot(checkpoint)
}

// removeCheckpointHeld removes every entry of the held checkpoint directory
// through it, then the directory itself, non-recursively, through the held
// recovery/, and syncs recovery/; each batch first re-checks that both names
// still name the held directories, and the first failure is returned. The last
// re-check and the remove are two calls: an object another process puts at the
// exact name between them is removed instead if it is a file, a link or an
// empty directory; a directory with content is refused; nothing outside the
// held recovery/ is reached. An identifier checkpointDir refuses removes nothing.
func (c *Cell) removeCheckpointHeld(recovery, checkpoint *os.Root, checkpointID string) error {
	if _, err := c.checkpointDir(checkpointID); err != nil {
		return err
	}
	c.checkpointStage("sweep")
	if !c.checkpointRootsBound(recovery, checkpoint, checkpointID) {
		return errCheckpointRootMoved
	}
	listing, err := checkpoint.Open(".")
	if err != nil {
		return err
	}
	names, err := listing.Readdirnames(-1)
	if closed := listing.Close(); err == nil {
		err = closed
	}
	if err != nil {
		return err
	}
	for index := range names {
		if err := checkpoint.RemoveAll(names[index]); err != nil {
			return err
		}
	}
	c.checkpointStage("directory")
	if !c.checkpointRootsBound(recovery, checkpoint, checkpointID) {
		return errCheckpointRootMoved
	}
	if err := recovery.Remove(checkpointID); err != nil {
		return err
	}
	return syncCheckpointRoot(recovery)
}

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
