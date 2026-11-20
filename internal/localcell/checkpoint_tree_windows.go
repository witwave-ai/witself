//go:build windows

package localcell

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// checkpointTree on Windows makes today's guarded pathname calls below the
// checkpoint directory and holds nothing. Names are slash paths below it.
type checkpointTree struct {
	c  *Cell
	id string
}

func (c *Cell) checkpointTree(checkpointID string) *checkpointTree {
	return &checkpointTree{c: c, id: checkpointID}
}

func (t *checkpointTree) hold() error { return nil }

func (t *checkpointTree) close() {}

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

func (t *checkpointTree) discard() error { return t.c.discardOwnCheckpoint(t.id) }
