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
	"time"

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
// call is waited out (LC-LEASE-1): a holder that lets go at the break costs a
// moment and create makes one complete checkpoint, as before the reader slices.
// One kept past the ceiling (300 ms here) is refused when the wait ends:
// PG_VERSION at admission as an unsupported source, before the control call
// and with the tree unchanged, any other file in the copy as a failed copy
// with the cleanup complete, the break begun.
func TestCheckpointReadersLeaseWaitedOut(t *testing.T) {
	for _, row := range []struct{ name, target, want, holder string }{
		{"pg_version", "pg/PG_VERSION", "local_checkpoint_source_unsupported", "lets_go"}, {"pg_version", "pg/PG_VERSION", "local_checkpoint_source_unsupported", "kept"},
		{"source", "pg/base/1/1259", "local_checkpoint_copy_failed", "lets_go"}, {"source", "pg/base/1/1259", "local_checkpoint_copy_failed", "kept"},
	} {
		t.Run(row.name+"/"+row.holder, func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			c := f.cell
			before := checkpointTreeState(t, c)
			lease := leaseTaken(t, leaseHolder(t, row.holder, unix.F_WRLCK), c.path(filepath.FromSlash(row.target)))
			started := time.Now()
			err := readerWatchdog(t, func() error { return c.Checkpoint(t.Context(), checkpointOptions(f)) }, lease.release)
			failure, ids := ClassifyCheckpointFailure(err), checkpointIDs(t, c)
			progress, journalErr := c.readJournal()
			closed := journalErr == nil && progress.pending == nil && !progress.tornTail
			if row.holder == "lets_go" {
				if err != nil || !lease.broken() || !closed || len(ids) != 1 || !checkpointSummaryOf(t, c, ids[0]).Complete {
					t.Fatal("a database file under a write lease whose holder let go at the break was not waited out")
				}
				return
			}
			leftover := slices.ContainsFunc(ids, func(name string) bool { return strings.HasPrefix(name, "ckpt_") })
			if held, _ := unix.FcntlInt(lease.file.Fd(), unix.F_GETLEASE, 0); failure.Class != row.want || failure.CleanupIncomplete || !closed || leftover || held != unix.F_RDLCK || time.Since(started) < leaseKeptAfter {
				t.Fatal("a database file under a kept write lease was not refused when the wait ended with its class, the cleanup complete and the lease held")
			}
			if row.name == "pg_version" && (r.calls["control"] != 0 || !reflect.DeepEqual(checkpointLeasedTree(t, c, lease.file), before)) {
				t.Fatal("a leased PG_VERSION was refused after the control call or with the tree changed")
			}
		})
	}
}
