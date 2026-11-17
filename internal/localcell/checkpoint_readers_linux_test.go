//go:build linux

package localcell

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// checkpointLease takes a write lease on the cell's slash path name through a
// second, read-only open, held until the row ends; a refused lease fails it.
func checkpointLease(t *testing.T, c *Cell, name string) *os.File {
	t.Helper()
	lease, err := os.Open(c.path(filepath.FromSlash(name)))
	if err != nil {
		t.Fatal("cannot open the file the lease row takes a write lease on")
	}
	t.Cleanup(func() { _ = lease.Close() })
	switch _, err := unix.FcntlInt(lease.Fd(), unix.F_SETLEASE, unix.F_WRLCK); {
	case errors.Is(err, unix.EAGAIN):
		t.Fatal("this kernel refused the write lease with EAGAIN, another open of the file is held")
	case errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM):
		t.Fatal("this kernel refused the write lease with EACCES or EPERM, the file is not owned here")
	case err != nil:
		t.Fatal("this kernel or sandbox refused the write lease, leases are disabled or filtered here")
	}
	return lease
}

// checkpointLeasedTree is checkpointTreeState with the leased file read through
// the lease's own handle, which breaks nothing; another open of it would wait.
func checkpointLeasedTree(t *testing.T, c *Cell, lease *os.File) map[string]retainedEntry {
	t.Helper()
	tree := map[string]retainedEntry{}
	err := filepath.WalkDir(c.cfg.Root, func(name string, entry fs.DirEntry, err error) error {
		rel, relErr := filepath.Rel(c.cfg.Root, name)
		info, infoErr := entry.Info()
		if err = errors.Join(err, relErr, infoErr); err != nil {
			return err
		}
		value, data := retainedEntry{mode: info.Mode()}, []byte(nil)
		if entry.Type().IsRegular() && !strings.HasSuffix(rel, ".lock") {
			if name == lease.Name() {
				data, err = io.ReadAll(io.NewSectionReader(lease, 0, info.Size()))
			} else {
				data, err = os.ReadFile(name)
			}
			value.size, value.data = info.Size(), string(data)
		}
		tree[filepath.ToSlash(rel)] = value
		return err
	})
	if err != nil {
		t.Fatal("cannot snapshot the leased checkpoint fixture tree")
	}
	return tree
}

// A write lease that another open of a database file holds from before the
// call is refused at once, never waited out (LC-READERS-3R P1, accepted):
// PG_VERSION at admission as an unsupported source, before the control call
// and with the tree unchanged, any other file in the copy as a failed copy
// with the cleanup complete. The lease is held until the row ends, the refused
// open has started its break, and the watchdog's unblock releases it.
func TestCheckpointReadersLease(t *testing.T) {
	for _, row := range []struct{ name, target, want string }{
		{"pg_version", "pg/PG_VERSION", "local_checkpoint_source_unsupported"},
		{"source", "pg/base/1/1259", "local_checkpoint_copy_failed"},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			c := f.cell
			before := checkpointTreeState(t, c)
			lease := checkpointLease(t, c, row.target)
			err := readerWatchdog(t, func() error { return c.Checkpoint(t.Context(), checkpointOptions(f)) }, func() { _ = lease.Close() })
			failure := ClassifyCheckpointFailure(err)
			progress, journalErr := c.readJournal()
			closed := journalErr == nil && progress.pending == nil && !progress.tornTail
			leftover := slices.ContainsFunc(checkpointIDs(t, c), func(name string) bool { return strings.HasPrefix(name, "ckpt_") })
			if held, _ := unix.FcntlInt(lease.Fd(), unix.F_GETLEASE, 0); failure.Class != row.want || failure.CleanupIncomplete || !closed || leftover || held != unix.F_RDLCK {
				t.Fatal("a database file under a write lease was not refused at once with its class, the cleanup complete and the lease held")
			}
			if row.name == "pg_version" && (r.calls["control"] != 0 || !reflect.DeepEqual(checkpointLeasedTree(t, c, lease), before)) {
				t.Fatal("a leased PG_VERSION was refused after the control call or with the tree changed")
			}
		})
	}
}
