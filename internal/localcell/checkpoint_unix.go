//go:build linux || darwin

package localcell

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
)

// checkpointFreeSpace returns the bytes available to this user and the volume
// size: Linux counts fragments (blocks when zero); Darwin has no fragment field.
func checkpointFreeSpace(path string) (free, total uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, errCheckpointState
	}
	unit := uint64(stat.Bsize)
	if fragment := reflect.ValueOf(stat).FieldByName("Frsize"); fragment.IsValid() && !fragment.IsZero() {
		unit = fragment.Convert(reflect.TypeFor[uint64]()).Uint()
	}
	return uint64(stat.Bavail) * unit, uint64(stat.Blocks) * unit, nil
}

// openCheckpointSource confines reads to pg/ with one os.Root, which follows
// only links that stay inside it; admission's walk refuses every link. pg/
// opens as a directory only, as recovery/ does: a FIFO, file or device at the
// name is refused unopened, so nothing waits; a link at pg/ is followed as
// before (admission refused one present then). The returned opener never waits
// on what stands at a file's name (a FIFO opens at once) but waits out a lease
// on a regular file there (waitOutLease), and every caller decides the opened
// handle before a byte is read.
func (c *Cell) openCheckpointSource() (func(string) (*os.File, error), func(), error) {
	root, err := os.OpenRoot(c.path("pg") + checkpointRootHere)
	if err != nil {
		return nil, nil, err
	}
	return func(rel string) (*os.File, error) {
		name := filepath.FromSlash(rel)
		return waitOutLease(name, root.Lstat, func() (*os.File, error) { return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0) })
	}, func() { _ = root.Close() }, nil
}

// validateRecoveryObject decides one object by Lstat and never opens it: a
// private directory or a regular 0600 file owned by this user. A vanished
// object returns its Lstat error unchanged so the snapshot retry applies.
func validateRecoveryObject(path string, directory bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err != nil || info.IsDir() != directory || (directory && !privateOwnedDir(path, info)) {
		return errCheckpointState
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !directory && (!ok || info.Mode() != 0o600 || stat.Uid != uint32(os.Geteuid())) {
		return errCheckpointState
	}
	return nil
}
