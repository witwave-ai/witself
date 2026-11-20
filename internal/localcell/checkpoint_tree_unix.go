//go:build linux || darwin

package localcell

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// syncCheckpointTreeParent syncs the parent of a directory the copy made, made
// its slash name below recovery/. Tests replace it; production runs sync.
var syncCheckpointTreeParent = func(_ string, sync func() error) error { return sync() }

// checkpointTree reaches each name below one checkpoint directory from the held
// recovery/ through the private beneath walk: never through a link or off the
// held device, and through each directory the call made only while it is the one
// recorded at its name; recovery/ must still name the held directory before each
// operation. Names are slash paths below the checkpoint directory, "." itself.
type checkpointTree struct {
	c        *Cell
	id       string
	recovery *os.Root
	made     map[string]privateBeneathID
}

func (c *Cell) checkpointTree(checkpointID string) *checkpointTree {
	return &checkpointTree{c: c, id: checkpointID, made: map[string]privateBeneathID{}}
}

// hold opens recovery/ (once: the copy holds it) and binds it.
func (t *checkpointTree) hold() (err error) {
	if t.recovery, err = t.c.holdRecovery(); err != nil {
		return err
	}
	return t.bound()
}

func (t *checkpointTree) close() {
	if t.recovery != nil {
		_ = t.recovery.Close()
	}
}

// bound refuses unless recovery/ was held and its name still names it.
func (t *checkpointTree) bound() error {
	if t.recovery == nil || !t.c.checkpointRootsBound(t.recovery, nil, t.id) {
		return errCheckpointRootMoved
	}
	return nil
}

// name is name's slash path below recovery/.
func (t *checkpointTree) name(name string) string { return path.Join(t.id, name) }

// want is the identity this call recorded at a slash prefix below recovery/.
func (t *checkpointTree) want(prefix string) (privateBeneathID, bool) {
	id, ok := t.made[prefix]
	return id, ok
}

func (t *checkpointTree) lstat(name string) (fs.FileInfo, error) {
	if err := t.bound(); err != nil {
		return nil, err
	}
	return privateBeneathLstat(t.recovery, t.name(name), t.want)
}

// mkdir makes name and records its identity, a refused one's too once its
// re-open succeeded (its privacy check or parent sync failed), so the discard
// can remove it. A directory whose re-open failed has no identity and is not
// recorded: at the checkpoint's own name the discard leaves it, incomplete.
func (t *checkpointTree) mkdir(name string) error {
	if err := t.bound(); err != nil {
		return err
	}
	id, err := privateBeneathMkdir(t.recovery, t.name(name), t.want, func(fd int) error {
		return syncCheckpointTreeParent(t.name(name), func() error { return privateBeneathSync(fd) })
	})
	if id != (privateBeneathID{}) {
		t.made[t.name(name)] = id
	}
	return err
}

// createTemp makes a private temporary in the directory dir; it returns the
// file, labelled by its logical pathname (never resolved), and its name.
func (t *checkpointTree) createTemp(dir string) (*os.File, string, error) {
	if err := t.bound(); err != nil {
		return nil, "", err
	}
	label := filepath.Join(t.c.recoveryDir(), filepath.FromSlash(t.name(dir)), ".pending-*")
	created, name, err := privateBeneathCreateTemp(t.recovery, t.name(dir), t.want, label)
	return created, path.Join(dir, name), err
}

// publish renames from to to in their one directory once nothing stands at to.
func (t *checkpointTree) publish(from, to string) error {
	if err := t.bound(); err != nil {
		return err
	} else if path.Dir(from) != path.Dir(to) {
		return unix.EINVAL
	}
	return privateBeneathPublish(t.recovery, t.name(path.Dir(to)), path.Base(from), path.Base(to), t.want)
}

func (t *checkpointTree) sync(name string) error {
	return syncCheckpointTreeDirectory(t.name(name), func() error {
		if err := t.bound(); err != nil {
			return err
		}
		return privateBeneathDirectory(t.recovery, t.name(name), t.want, func(fd int, _ privateBeneathID) error { return privateBeneathSync(fd) })
	})
}

// discard removes this call's directory only while it has no witness, only as
// the identity it recorded, through the held recovery/ while bound. A witness
// in it, still bound, is not a failed discard. Anything else at the name (a
// link, a file, a directory it did not record) is left: incomplete.
func (t *checkpointTree) discard() error {
	t.c.checkpointStage("hold")
	if t.bound() != nil {
		return errCheckpointCopy
	}
	recorded, made := t.made[t.id]
	switch info, err := privateBeneathLstat(t.recovery, t.id, nil); {
	case errors.Is(err, fs.ErrNotExist):
		// The no-follow fstatat of the one name in the bound recovery/: nothing is there.
		return syncCheckpointRoot(t.recovery)
	case err != nil || !made || !info.IsDir() || privateBeneathIdentity(info.Sys().(*unix.Stat_t)) != recorded:
		return errCheckpointCopy
	}
	// Not-exist is the witness's fstatat in the recorded directory, or that
	// directory gone: the sweep's own open then refuses.
	if _, err := privateBeneathLstat(t.recovery, t.name(checkpointWitnessName), t.want); err == nil && t.bound() == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return errCheckpointCopy
	}
	t.c.checkpointStage("sweep")
	if t.bound() != nil || privateBeneathDirectory(t.recovery, t.id, t.want, func(fd int, _ privateBeneathID) error { return privateBeneathRemoveBelow(fd) }) != nil {
		return errCheckpointCopy
	}
	t.c.checkpointStage("directory")
	if t.bound() != nil || checkpointTreeRemoveDirectory(t.recovery, t.id, recorded) != nil || syncCheckpointRoot(t.recovery) != nil {
		return errCheckpointCopy
	}
	// The no-follow fstatat of the one name in the bound recovery/ again.
	if _, err := privateBeneathLstat(t.recovery, t.id, nil); t.bound() != nil || !errors.Is(err, fs.ErrNotExist) {
		return errCheckpointCopy
	}
	return nil
}

// checkpointTreeRemoveDirectory removes the empty directory name only while it
// is the directory id; a file or link at the name is never removed. An empty
// directory of another identity put at the name between the fstatat and the
// unlinkat (one call apart) is removed instead.
func checkpointTreeRemoveDirectory(root *os.Root, name string, id privateBeneathID) error {
	return privateBeneathAt(root, name, nil, func(dir int, last string) error {
		var stat unix.Stat_t
		if err := unix.Fstatat(dir, last, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		} else if stat.Mode&unix.S_IFMT != unix.S_IFDIR || privateBeneathIdentity(&stat) != id {
			return errPrivateBeneathMoved
		}
		return unix.Unlinkat(dir, last, unix.AT_REMOVEDIR)
	})
}
