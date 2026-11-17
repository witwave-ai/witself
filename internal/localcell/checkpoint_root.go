package localcell

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// checkpointRootHere, as a path's last element, makes the element before it a
// directory component, which a pathname open and os.Root (O_DIRECTORY|
// O_NOFOLLOW; FILE_DIRECTORY_FILE on Windows) accept only as a directory, so a
// planted FIFO, device or file is refused at once, never opened or waited on.
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
// roots, each as a directory only; recovery is returned whenever it opened.
func (c *Cell) holdCheckpoint(checkpointID string) (recovery, checkpoint *os.Root, err error) {
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
// current ones from the names without following their last element.
func (c *Cell) checkpointRootsBound(recovery, checkpoint *os.Root, checkpointID string) bool {
	recoveryHeld, err := recovery.Stat(".")
	recoveryNow, nowErr := os.Lstat(c.recoveryDir())
	if err != nil || nowErr != nil || !os.SameFile(recoveryHeld, recoveryNow) {
		return false
	}
	checkpointHeld, err := checkpoint.Stat(".")
	checkpointNow, nowErr := recovery.Lstat(checkpointID)
	return err == nil && nowErr == nil && os.SameFile(checkpointHeld, checkpointNow)
}

// unsealCheckpointHeld removes the witness through the held checkpoint
// directory once both names are bound, then syncs that directory; an absent
// witness is already removed.
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
// held recovery/ is reached.
func (c *Cell) removeCheckpointHeld(recovery, checkpoint *os.Root, checkpointID string) error {
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
// witness, through held roots; a present witness is not a failed discard, and
// an absent directory is already discarded.
func (c *Cell) discardOwnCheckpoint(checkpointID string) error {
	if _, err := c.checkpointDir(checkpointID); err != nil {
		return err
	}
	recovery, checkpoint, err := c.holdCheckpoint(checkpointID)
	defer closeCheckpointRoots(recovery, checkpoint)
	if recovery != nil && errors.Is(err, fs.ErrNotExist) {
		return syncCheckpointRoot(recovery)
	} else if err != nil {
		return errCheckpointCopy
	}
	if _, err := checkpoint.Lstat(checkpointWitnessName); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return errCheckpointCopy
	}
	if c.removeCheckpointHeld(recovery, checkpoint, checkpointID) != nil {
		return errCheckpointCopy
	}
	return nil
}
