package localcell

import (
	"errors"
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

// holdRecovery opens recovery/ as a root, as a directory only: the one opener
// of the held recovery/ for holdCheckpoint and the copy's tree.
func (c *Cell) holdRecovery() (*os.Root, error) {
	return os.OpenRoot(c.recoveryDir() + checkpointRootHere)
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
