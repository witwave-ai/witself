//go:build linux

package localcell

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func init() {
	writerNeverWaitRows = append(writerNeverWaitRows, writerPlantRow{"socket_node", func(name, _ string) (*os.File, int, error) {
		return nil, 0, unix.Mknod(name, unix.S_IFSOCK|0o600, 0)
	}})
}

// writerLease is a read lease taken through a second, read-only open (mode zero) or a write
// lease through a write-only one, maybe from the call's own goroutine; do runs fn on its descriptor while held.
type writerLease struct {
	mu   sync.Mutex
	file *os.File
	mode int
}

func (l *writerLease) take(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(name, l.mode, 0)
	if _, leaseErr := unix.FcntlInt(f.Fd(), unix.F_SETLEASE, map[int]int{os.O_RDONLY: unix.F_RDLCK, os.O_WRONLY: unix.F_WRLCK}[l.mode]); err == nil && leaseErr == nil {
		l.file = f
	} else {
		_ = f.Close()
	}
}

func (l *writerLease) do(fn func(fd uintptr)) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		fn(l.file.Fd())
	}
	return l.file != nil
}

func (l *writerLease) release() {
	l.do(func(fd uintptr) { _, _ = unix.FcntlInt(fd, unix.F_SETLEASE, unix.F_UNLCK) })
}

func (l *writerLease) close() { l.do(func(uintptr) { _ = l.file.Close() }) }

// A read lease held on the journal when a write open starts is refused at once
// with the site's open text, where HEAD waited (OQ1); nothing is appended or
// truncated, and the holder's lease reads F_UNLCK: the break has begun.
func TestWriterLeaseRefusedAtOnce(t *testing.T) {
	for _, row := range []struct {
		name, want string
		setup      func(t *testing.T, l *writerLease) (string, func() error)
	}{
		{"helper", errWriterFailed.Error(), func(t *testing.T, l *writerLease) (string, func() error) {
			name := filepath.Join(t.TempDir(), "journal")
			if os.WriteFile(name, []byte(readerJournalLine), 0o600) != nil {
				t.Fatal("cannot prepare the leased journal")
			}
			l.take(name)
			return name, func() error { return writerOpenRow(name) }
		}},
		{"journal_append", "cannot open local cell journal", func(t *testing.T, l *writerLease) (string, func() error) {
			c := readerCell(t)
			l.take(readerJournalPath(c))
			return readerJournalPath(c), func() error { return c.journal("stop", "operation", "started") }
		}},
		{"discard_tail", "cannot open torn local cell journal", func(t *testing.T, l *writerLease) (string, func() error) {
			c, progress := writerTornCell(t)
			l.take(readerJournalPath(c))
			return readerJournalPath(c), func() error { return c.discardJournalTail(progress) }
		}},
		{"retained_sync", "cannot open local cell journal", func(t *testing.T, l *writerLease) (string, func() error) {
			c := writerRetainedCleanup(t)
			c.cfg.Failpoint = func(op, step string) error {
				if op == "remove" && step == "cleanup_journal" {
					l.take(readerJournalPath(c))
				}
				return nil
			}
			return readerJournalPath(c), func() error { return c.Repair(t.Context()) }
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			l := &writerLease{}
			t.Cleanup(l.close)
			name, call := row.setup(t, l)
			size := writerSize(name)
			got, held := readerWatchdog(t, call, l.release), -1
			if !l.do(func(fd uintptr) { held, _ = unix.FcntlInt(fd, unix.F_GETLEASE, 0) }) {
				t.Fatal("this kernel or sandbox refused the read lease the writer row needs")
			}
			if got == nil || got.Error() != row.want || writerSize(name) != size || held != unix.F_UNLCK {
				t.Fatalf("the leased journal open gave its open text %t: size kept %t, break begun %t", got != nil && got.Error() == row.want, writerSize(name) == size, held == unix.F_UNLCK)
			}
		})
	}
}

// The checkpoint verbs refuse at once a read lease held on the journal when
// they start (HEAD waited): nothing is journaled, create leaves recovery/
// itself absent or listing no checkpoint (R9: with the root a directory,
// not-exist is absence on Linux), and a delete's checkpoint stays complete.
func TestWriterLeaseCheckpointVerbs(t *testing.T) {
	for _, verb := range []string{"create", "delete"} {
		for _, layout := range []string{"legacy", "selected"} {
			t.Run(verb+"/"+layout, func(t *testing.T) {
				f, _ := checkpointFixture(t, layout, false)
				c, ids := f.cell, []string(nil)
				if verb == "delete" && (c.Checkpoint(t.Context(), checkpointOptions(f)) != nil || len(checkpointIDs(t, c)) != 1) {
					t.Fatal("cannot prepare the one checkpoint to delete")
				}
				ids = checkpointIDs(t, c)
				before, err := os.ReadFile(readerJournalPath(c))
				l := &writerLease{}
				t.Cleanup(l.close)
				l.take(readerJournalPath(c))
				if err != nil || !l.do(func(uintptr) {}) {
					t.Fatal("this kernel or sandbox refused the read lease the writer row needs")
				}
				err = readerWatchdog(t, func() error {
					if verb == "create" {
						return c.Checkpoint(t.Context(), checkpointOptions(f))
					}
					return c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: ids[0]})
				}, l.release)
				l.close()
				after, readErr := os.ReadFile(readerJournalPath(c))
				if failure := ClassifyCheckpointFailure(err); failure.Class != "local_checkpoint_operation_failed" || err.Error() != "cannot open local cell journal" || readErr != nil || !bytes.Equal(before, after) {
					t.Fatalf("the leased checkpoint %s ended as class %q, journal unchanged %t", verb, failure.Class, readErr == nil && bytes.Equal(before, after))
				}
				root, rootErr := os.Lstat(c.cfg.Root)
				recovery, recoveryErr := os.Lstat(c.recoveryDir())
				entries, listErr := os.ReadDir(c.recoveryDir())
				none := rootErr == nil && root.IsDir() && (errors.Is(recoveryErr, os.ErrNotExist) || recoveryErr == nil && recovery.IsDir() && listErr == nil &&
					!slices.ContainsFunc(entries, func(entry os.DirEntry) bool { return strings.HasPrefix(entry.Name(), "ckpt_") }))
				if verb == "create" && !none || verb == "delete" && !checkpointSummaryOf(t, c, ids[0]).Complete {
					t.Fatal("the leased verb made a checkpoint or changed the one it named")
				}
			})
		}
	}
}

// The journal lease of docs/cli-command-surface.md, run as the command runs it (Inspect, then
// the verb when Inspect lists): a write lease stops it at Inspect, a read lease at the verb's
// journal write, at once, with the operation failure class and no reason; nothing is journaled,
// create makes no checkpoint and delete keeps its own. Once the holder lets go, both succeed.
func TestWriterLeaseCheckpointRetry(t *testing.T) {
	for _, lease := range []string{"read", "write"} {
		for _, verb := range []string{"create", "delete"} {
			for _, layout := range []string{"legacy", "selected"} {
				t.Run(lease+"/"+verb+"/"+layout, func(t *testing.T) {
					f, _ := checkpointFixture(t, layout, false)
					c, ids, l := f.cell, []string(nil), &writerLease{mode: map[string]int{"read": os.O_RDONLY, "write": os.O_WRONLY}[lease]}
					call := func() error {
						if verb == "create" {
							return c.Checkpoint(t.Context(), checkpointOptions(f))
						}
						return c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: ids[0]})
					}
					if verb == "delete" && (c.Checkpoint(t.Context(), checkpointOptions(f)) != nil || len(checkpointIDs(t, c)) != 1) {
						t.Fatal("cannot prepare the one checkpoint to delete")
					}
					ids = checkpointIDs(t, c)
					before, err := os.ReadFile(readerJournalPath(c))
					t.Cleanup(l.close)
					l.take(readerJournalPath(c))
					if err != nil || !l.do(func(uintptr) {}) {
						t.Fatal("this kernel or sandbox refused the lease the writer row needs")
					}
					var observed, called error
					_ = readerWatchdog(t, func() error {
						if _, observed = c.Inspect(t.Context()); observed == nil {
							called = call()
						}
						return nil
					}, l.release)
					l.close()
					after, readErr := os.ReadFile(readerJournalPath(c))
					got, want := errors.Join(observed, called), map[string]string{"read": "cannot open local cell journal", "write": "cannot read local cell journal"}[lease]
					stopped := (observed == nil) == (lease == "read") && got != nil && got.Error() == want
					kept := readErr == nil && bytes.Equal(before, after) && slices.Equal(checkpointIDs(t, c), ids) && (verb == "create" || checkpointSummaryOf(t, c, ids[0]).Complete)
					if !stopped || ClassifyCheckpointFailure(got) != (CheckpointFailure{Class: "local_checkpoint_operation_failed"}) || !kept {
						t.Fatalf("the leased checkpoint %s stopped at its step %t, journal and checkpoints kept %t", verb, stopped, kept)
					}
					if _, err := c.Inspect(t.Context()); err != nil || call() != nil || len(checkpointIDs(t, c)) != 1-len(ids) {
						t.Fatal("the checkpoint verb did not succeed once the holder released the lease")
					}
				})
			}
		}
	}
}
