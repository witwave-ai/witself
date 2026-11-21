//go:build linux || darwin

package localcell

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

// syncCheckpointTreeParent syncs the parent of a directory the copy made, made
// its slash name below recovery/. Tests replace it; production runs sync.
var syncCheckpointTreeParent = func(_ string, sync func() error) error { return sync() }

// checkpointTreeListed observes a snapshot directory after enumeration.
var checkpointTreeListed func(name string)

// checkpointTree reaches names from the held recovery/ through the private
// beneath walk. A copy uses slash paths relative to its checkpoint; a reader
// has an empty id and uses names relative to recovery/. made records directory
// identities for the walk, including the directories typed by a reader.
type checkpointTree struct {
	c        *Cell
	id       string
	recovery *os.Root
	made     map[string]privateBeneathID
	listed   []int // keep listed checkpoints' inodes alive until the witness reads finish
}

func (c *Cell) checkpointTree(checkpointID string) *checkpointTree {
	return &checkpointTree{c: c, id: checkpointID, made: map[string]privateBeneathID{}}
}

// hold opens recovery/ and verifies that its name still identifies it.
func (t *checkpointTree) hold() (err error) {
	if t.recovery, err = t.c.holdRecovery(); err != nil {
		return err
	}
	return t.bound()
}

func (t *checkpointTree) close() {
	for _, fd := range t.listed {
		_ = unix.Close(fd)
	}
	t.listed = nil
	if t.recovery != nil {
		_ = t.recovery.Close()
	}
}

func (t *checkpointTree) listing() ([]string, []bool, error) {
	if t.recovery == nil {
		if err := t.hold(); err != nil {
			// Only this open of the boundary may return not-exist to the caller.
			return nil, nil, err
		}
	} else if err := t.bound(); err != nil {
		return nil, nil, err
	}
	dir, err := t.recovery.Open(".")
	if err != nil {
		return nil, nil, errPrivateLayout
	}
	defer func() { _ = dir.Close() }()
	names, err := dir.Readdirnames(-1)
	var root unix.Stat_t
	if err != nil || unix.Fstat(int(dir.Fd()), &root) != nil {
		return nil, nil, errPrivateLayout
	}
	slices.Sort(names)
	directories := make([]bool, len(names))
	for i, name := range names {
		info, err := privateBeneathLstat(t.recovery, name, nil)
		if err != nil {
			return nil, nil, errPrivateLayout
		}
		stat := info.Sys().(*unix.Stat_t)
		if stat.Dev != root.Dev {
			return nil, nil, errCheckpointState
		}
		directories[i] = info.IsDir()
		if directories[i] && checkpointIDPattern.MatchString(name) {
			t.made[name] = privateBeneathIdentity(stat)
			err = privateBeneathDirectory(t.recovery, name, t.want, func(fd int, _ privateBeneathID) error {
				dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
				if err == nil {
					t.listed = append(t.listed, dup)
				}
				return err
			})
			if err != nil {
				return nil, nil, errPrivateLayout
			}
		}
	}
	return names, directories, nil
}

func (t *checkpointTree) witness(ctx context.Context, id string) (checkpointWitness, bool, error) {
	f, err := t.open(ctx, path.Join(id, checkpointWitnessName))
	if cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {
		return checkpointWitness{}, false, cut
	}
	if err != nil {
		if t.bound() != nil {
			return checkpointWitness{}, false, errCheckpointWitnessUnread
		}
		// Only the leaf lookup inside the listed directory can prove absence.
		// A failed walk to its parent (including ENOENT) remains unreadable.
		err = privateBeneathDirectory(t.recovery, id, t.want, func(fd int, parent privateBeneathID) error {
			var stat unix.Stat_t
			statErr := unix.Fstatat(fd, checkpointWitnessName, &stat, unix.AT_SYMLINK_NOFOLLOW)
			if errors.Is(statErr, unix.ENOENT) || statErr == nil && uint64(stat.Dev) == parent.dev && stat.Mode&unix.S_IFMT != unix.S_IFREG {
				return nil
			}
			return errCheckpointWitnessUnread
		})
		if err != nil {
			return checkpointWitness{}, false, errCheckpointWitnessUnread
		}
		return checkpointWitness{}, false, nil
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return checkpointWitness{}, false, errCheckpointWitnessUnread
	}
	if !info.Mode().IsRegular() {
		return checkpointWitness{}, false, nil
	}
	return checkpointWitnessRead(f, id)
}

// snapshot retries the entire pass only when an entry disappears between
// listing its parent and examining its name in that same open directory.
func (t *checkpointTree) snapshot() error {
	for range 3 {
		directories := []string{"."}
		vanished := false
		for len(directories) != 0 && !vanished {
			name := directories[0]
			directories = directories[1:]
			if t.bound() != nil {
				return errPrivateLayout
			}
			scan := func(dir *os.File) error {
				fd := int(dir.Fd())
				var self, stat unix.Stat_t
				if unix.Fstat(fd, &self) != nil {
					return errPrivateLayout
				}
				uid := uint32(os.Geteuid())
				if self.Uid != uid || self.Mode&0o777 != 0o700 {
					return errCheckpointState
				}
				names, err := dir.Readdirnames(-1)
				if err != nil {
					return errPrivateLayout
				}
				if checkpointTreeListed != nil {
					checkpointTreeListed(name)
				}
				for _, entry := range names {
					if err := unix.Fstatat(fd, entry, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
						vanished = true
						return nil
					} else if err != nil || stat.Dev != self.Dev || stat.Uid != uid {
						return errCheckpointState
					}
					if stat.Mode&unix.S_IFMT == unix.S_IFDIR && stat.Mode&0o777 == 0o700 {
						child := path.Join(name, entry)
						t.made[child] = privateBeneathIdentity(&stat)
						directories = append(directories, child)
					} else if stat.Mode != unix.S_IFREG|0o600 {
						return errCheckpointState
					}
				}
				return nil
			}
			if name == "." {
				dir, err := t.recovery.Open(".")
				if err != nil {
					return errPrivateLayout
				}
				err = scan(dir)
				_ = dir.Close()
				if err != nil {
					return err
				}
			} else if err := privateBeneathDirectory(t.recovery, name, t.want, func(fd int, _ privateBeneathID) error {
				return privateBeneathDup(fd, scan)
			}); err != nil {
				if errors.Is(err, errCheckpointState) || errors.Is(err, unix.EXDEV) || errors.Is(err, errPrivateBeneathMoved) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
					return errCheckpointState
				}
				return errPrivateLayout
			}
		}
		if !vanished {
			return nil
		}
	}
	return errPrivateLayout
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

// open opens name read-only through the walk, a Linux lease waited out as
// openPrivateReader waits (its lstat a no-follow stat through the walk); it
// never follows a link, leaves the device or waits on what stands there, and
// returns any type: the caller decides by Stat before a read. label is the
// logical pathname, never resolved.
func (t *checkpointTree) open(ctx context.Context, name string) (*os.File, error) {
	if err := t.bound(); err != nil {
		return nil, err
	}
	label := filepath.Join(t.c.recoveryDir(), filepath.FromSlash(t.name(name)))
	return waitOutLease(ctx, label, func(string) (fs.FileInfo, error) { return privateBeneathLstat(t.recovery, t.name(name), t.want) },
		func() (*os.File, error) { return privateBeneathOpen(t.recovery, t.name(name), t.want, label) })
}

// check lists the checkpoint directory and each planned directory below it
// through the walk, every directory this call made reached as the one it
// recorded, and counts their entries; one not planned, or not of its planned
// kind (a directory, else a regular file), is errCheckpointWitness, and a
// listing that fails (an entry gone before its stat too) refuses.
func (t *checkpointTree) check(planned map[string]bool) (int, error) {
	seen, directories := 0, []string{"."}
	for len(directories) > 0 {
		directory := directories[0]
		directories = directories[1:]
		if err := t.bound(); err != nil {
			return 0, err
		}
		entries, err := privateBeneathList(t.recovery, t.name(directory), t.want)
		if err != nil {
			return 0, err
		}
		for _, entry := range entries {
			name := path.Join(directory, entry.name)
			if kind, known := planned[name]; !known || kind != entry.directory || !kind && !entry.regular {
				return 0, errCheckpointWitness
			} else if seen++; kind {
				directories = append(directories, name)
			}
		}
	}
	return seen, nil
}

// record publishes data at name, a regular file or nothing there, in the
// directory the walk reaches: a private temporary made there, written, synced
// and closed, renamed over name in that one directory (a link put at name is
// replaced, never followed), then the directory synced. A temporary a failure
// left is removed through the walk. It binds first, before the rename and after
// it: the walk starts from the held descriptor and never re-proves that
// recovery/'s name still names it, so only that last bind refuses a recovery/
// moved in the instant after the rename.
func (t *checkpointTree) record(name string, data []byte) error {
	if err := t.bound(); err != nil {
		return err
	} else if info, err := privateBeneathLstat(t.recovery, t.name(name), t.want); err == nil && !info.Mode().IsRegular() {
		return errors.New("local cell record must be a regular file")
	}
	// Not-exist is the no-follow fstatat of name in the reached directory: nothing there. A walk refusal recurs below.
	temporary, pending, err := t.createTemp(path.Dir(name))
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = privateBeneathRemove(t.recovery, t.name(pending), t.want)
		}
	}()
	if _, err = temporary.Write(data); err == nil {
		err = syncFile(temporary)
	}
	if closeErr := temporary.Close(); err != nil || closeErr != nil {
		return errors.New("cannot persist local cell record")
	}
	if err := t.bound(); err != nil {
		return err
	} else if err := privateBeneathReplace(t.recovery, t.name(path.Dir(name)), path.Base(pending), path.Base(name), t.want); err != nil {
		return err
	}
	published = true
	if err := t.bound(); err != nil {
		return err
	}
	return privateBeneathDirectory(t.recovery, t.name(path.Dir(name)), t.want, func(fd int, _ privateBeneathID) error { return privateBeneathSync(fd) })
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
