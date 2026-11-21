//go:build windows

package localcell

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// checkpointTree on Windows makes today's guarded pathname calls below the
// checkpoint directory. Delete stores both roots on the tree. Failed-create
// discard holds its roots inside discardOwnCheckpoint. Names are slash paths.
type checkpointTree struct {
	c                    *Cell
	id                   string
	recovery, checkpoint *os.Root
}

func (c *Cell) checkpointTree(checkpointID string) *checkpointTree {
	return &checkpointTree{c: c, id: checkpointID}
}

func (t *checkpointTree) hold() error { return nil }

func (t *checkpointTree) close() { closeCheckpointRoots(t.recovery, t.checkpoint) }

// target is delete's admission of the checkpoint at its exact name, by
// today's pathname calls.
func (t *checkpointTree) target() error {
	dir, err := t.c.checkpointDir(t.id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return errCheckpointIdentity
	} else if err != nil || privateDir(dir, false) != nil {
		return errCheckpointState
	}
	// A case-insensitive volume also resolves another spelling: only the exact
	// on-disk name is deleted.
	entries, err := os.ReadDir(t.c.recoveryDir())
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if err != nil || !recoveryEntryCanonical(names, t.id) {
		return errCheckpointIdentity
	}
	return nil
}

// adopt holds recovery/ and the checkpoint as roots.
func (t *checkpointTree) adopt() (err error) {
	t.recovery, t.checkpoint, err = t.c.holdCheckpoint(t.id)
	return err
}

func (t *checkpointTree) unseal() error {
	return t.c.unsealCheckpointHeld(t.recovery, t.checkpoint, t.id)
}

func (t *checkpointTree) sweep() error {
	return t.c.removeCheckpointHeld(t.recovery, t.checkpoint, t.id)
}

func (t *checkpointTree) listing() ([]string, []bool, error) {
	entries, err := os.ReadDir(t.c.recoveryDir())
	if err != nil {
		return nil, nil, err
	}
	names, directories := make([]string, len(entries)), make([]bool, len(entries))
	for i, entry := range entries {
		names[i], directories[i] = entry.Name(), entry.IsDir()
	}
	return names, directories, nil
}

func (t *checkpointTree) witness(ctx context.Context, id string) (checkpointWitness, bool, error) {
	dir, err := t.c.checkpointDir(id)
	if err != nil {
		return checkpointWitness{}, false, err
	}
	return checkpointComplete(ctx, dir, id)
}

func (t *checkpointTree) snapshot() error {
	return validatePrivateTreeSnapshot(t.c.recoveryDir(), filepath.WalkDir, validateRecoveryObject, os.Lstat)
}

// name is name's slash path below recovery/.
func (t *checkpointTree) name(name string) string { return path.Join(t.id, name) }

func (t *checkpointTree) lstat(name string) (fs.FileInfo, error) {
	dir, err := t.c.checkpointDir(t.id)
	if err != nil {
		return nil, err
	}
	return os.Lstat(filepath.Join(dir, filepath.FromSlash(name)))
}

func (t *checkpointTree) mkdir(name string) error {
	dir, err := t.c.checkpointDir(t.id)
	if err != nil {
		return err
	}
	return privateDir(filepath.Join(dir, filepath.FromSlash(name)), true)
}

func (t *checkpointTree) createTemp(name string) (*os.File, string, error) {
	dir, err := t.c.checkpointDir(t.id)
	if err != nil {
		return nil, "", err
	}
	created, err := createPrivateTemp(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		return nil, "", err
	}
	return created, path.Join(name, filepath.Base(created.Name())), nil
}

func (t *checkpointTree) publish(from, to string) error {
	dir, err := t.c.checkpointDir(t.id)
	if err != nil {
		return err
	}
	return publishPrivateFile(filepath.Join(dir, filepath.FromSlash(from)), filepath.Join(dir, filepath.FromSlash(to)))
}

func (t *checkpointTree) sync(name string) error {
	return syncCheckpointTreeDirectory(t.name(name), func() error {
		dir, err := t.c.checkpointDir(t.id)
		if err != nil {
			return err
		}
		return syncDirectory(filepath.Join(dir, filepath.FromSlash(name)))
	})
}

func (t *checkpointTree) open(ctx context.Context, name string) (*os.File, error) {
	dir, err := t.c.checkpointDir(t.id)
	if err != nil {
		return nil, err
	}
	return openPrivateReader(ctx, filepath.Join(dir, filepath.FromSlash(name)))
}

// check is the inventory's walk of the checkpoint directory by pathname.
func (t *checkpointTree) check(planned map[string]bool) (int, error) {
	dir, err := t.c.checkpointDir(t.id)
	if err != nil {
		return 0, err
	}
	seen := 0
	err = filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		rel, relErr := filepath.Rel(dir, name)
		if err != nil || relErr != nil || rel == "." {
			return errors.Join(err, relErr)
		}
		directory, known := planned[filepath.ToSlash(rel)]
		if !known || directory != entry.IsDir() || (!directory && !entry.Type().IsRegular()) {
			return errCheckpointWitness
		}
		seen++
		return nil
	})
	return seen, err
}

func (t *checkpointTree) record(name string, data []byte) error {
	dir, err := t.c.checkpointDir(t.id)
	if err != nil {
		return err
	}
	return writeAtomicBytes(filepath.Join(dir, filepath.FromSlash(name)), data, nil)
}

func (t *checkpointTree) discard() error { return t.c.discardOwnCheckpoint(t.id) }
