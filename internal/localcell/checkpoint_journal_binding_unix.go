//go:build linux || darwin

package localcell

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// A name in the canonical cell and the stat of the object actually held.
type journalBinding struct {
	path string
	stat func() (os.FileInfo, error)
}

func (c *Cell) journalBindings(bindings []journalBinding) error {
	root, err := filepath.EvalSymlinks(c.cfg.Root)
	if err != nil || root != c.cfg.Root {
		return errCheckpointJournalIO
	}
	var device uint64
	for i, b := range bindings {
		current, err := os.Lstat(b.path)
		held, statErr := b.stat()
		sameBinding := err == nil && statErr == nil && os.SameFile(current, held)
		if !sameBinding {
			return errCheckpointJournalIO
		}
		s, ok := held.Sys().(*syscall.Stat_t)
		if !ok || s.Uid != uint32(os.Geteuid()) || i > 0 && uint64(s.Dev) != device {
			return errPrivateLayout
		}
		device = uint64(s.Dev)
		if held.IsDir() && held.Mode().Perm() == 0700 {
			continue
		}
		if held.Mode().IsRegular() && held.Mode().Perm()&0600 != 0600 {
			return errCheckpointJournalIO
		}
		if !held.Mode().IsRegular() || held.Mode().Perm() != 0600 {
			return errPrivateLayout
		}
		linkCount := s.Nlink
		if linkCount != 1 {
			return errPrivateLayout
		}
	}
	return nil
}

func (c *Cell) repairOwnedJournal(ctx context.Context, owner *os.File, root *os.Root) (CheckpointJournalStatus, error) {
	bound := []journalBinding{{c.cfg.Root, func() (os.FileInfo, error) { return root.Stat(".") }}, {c.path(".owner.lock"), owner.Stat}}
	// Only a positive Lstat absence below a still-bound parent qualifies.
	absent := func(parent *os.Root, name string) (CheckpointJournalStatus, error) {
		if err := errors.Join(c.journalBindings(bound), ctx.Err()); err != nil {
			return "", err
		}
		if _, err := parent.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			return "", errCheckpointJournalIO
		}
		return CheckpointJournalAbsent, nil
	}
	dir, err := root.OpenRoot("journal" + checkpointRootHere)
	if errors.Is(err, os.ErrNotExist) {
		return absent(root, "journal")
	}
	if err != nil {
		return "", errCheckpointJournalIO
	}
	defer func() { _ = dir.Close() }()
	bound = append(bound, journalBinding{c.path("journal"), func() (os.FileInfo, error) { return dir.Stat(".") }})
	const name = "operations.jsonl"
	info, err := dir.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return absent(dir, name)
	}
	if err != nil {
		return "", errCheckpointJournalIO
	}
	if !info.Mode().IsRegular() {
		return "", errPrivateLayout
	}
	f, err := openCheckpointJournalFile(ctx, dir, name, os.O_RDWR)
	if err != nil {
		return "", errors.Join(errCheckpointJournalIO, ctx.Err())
	}
	bound = append(bound, journalBinding{c.path("journal", name), f.Stat})
	var before unix.Stat_t
	if err := errors.Join(c.journalBindings(bound), unix.Fstat(int(f.Fd()), &before)); err != nil {
		_ = f.Close()
		return "", err
	}
	// The parser has consumed EOF on this descriptor. Its offset and the
	// native change time must still cover exactly the observed extent.
	revalidate := func() error {
		if err := c.journalBindings(bound); err != nil {
			return err
		}
		var now unix.Stat_t
		end, err := f.Seek(0, io.SeekCurrent)
		if err != nil || unix.Fstat(int(f.Fd()), &now) != nil || end != before.Size || now.Size != before.Size || now.Ctim != before.Ctim || now.Mtim != before.Mtim {
			return errCheckpointJournalIO
		}
		return nil
	}
	return c.settleCheckpointJournal(ctx, root, dir, f, before.Size, revalidate)
}

func openCheckpointJournalFile(ctx context.Context, dir *os.Root, name string, access int) (*os.File, error) {
	return waitOutLease(ctx, name, dir.Lstat, func() (*os.File, error) {
		return dir.OpenFile(name, access|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	})
}

// An admitted verb may follow a repair that truncated this file but failed
// to sync it. Settle the observed inode and both names before its first append.
func (c *Cell) syncCheckpointJournalPrefix(ctx context.Context, progress journalProgress) (err error) {
	if progress.absent {
		return nil // first publication owns its own durability
	}
	if progress.readFile == nil {
		return errCheckpointJournalIO
	}
	root, err := os.OpenRoot(c.cfg.Root + checkpointRootHere)
	if err != nil {
		return errCheckpointJournalIO
	}
	defer func() { _ = root.Close() }()
	dir, err := root.OpenRoot("journal" + checkpointRootHere)
	if err != nil {
		return errCheckpointJournalIO
	}
	defer func() { _ = dir.Close() }()
	const name = "operations.jsonl"
	f, err := openCheckpointJournalFile(ctx, dir, name, os.O_RDONLY)
	if cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {
		return cut
	}
	if err != nil {
		return errCheckpointJournalIO
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = errCheckpointJournalIO
		}
	}()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(progress.readFile, info) {
		return errCheckpointJournalIO
	}
	if !journalNamesBound([]journalBinding{
		{c.cfg.Root, func() (os.FileInfo, error) { return root.Stat(".") }},
		{c.path("journal"), func() (os.FileInfo, error) { return dir.Stat(".") }},
		{c.path("journal", name), f.Stat},
	}) {
		return errCheckpointJournalIO
	}
	return syncHeldCheckpointJournal(root, dir, f)
}

// Syncing the prefix only requires the names to still identify the held
// objects. Repair's stricter metadata checks protect its truncation.
func journalNamesBound(bindings []journalBinding) bool {
	for _, b := range bindings {
		current, err := os.Lstat(b.path)
		held, statErr := b.stat()
		if err != nil || statErr != nil || !os.SameFile(current, held) {
			return false
		}
	}
	return true
}
