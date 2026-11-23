//go:build linux

package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func init() {
	writerNeverWaitRows = append(writerNeverWaitRows, writerPlantRow{"socket_node", func(name, _ string) (*os.File, int, error) {
		return nil, 0, unix.Mknod(name, unix.S_IFSOCK|0o600, 0)
	}})
}

// writerLease is a lease taken through a second, read-only open, maybe from the call's own goroutine;
// do runs fn on its descriptor while held. Its holder polls F_GETLEASE (hold) and at a break records
// it and lets go, lets go and takes the lease again (retaken), or keeps it.
type writerLease struct {
	holders            sync.WaitGroup
	mu                 sync.Mutex
	file               *os.File
	lease              int
	letGo, retake, saw bool
}

// leaseHolder is the row's holder by kind (lets_go, kept, retaken); kept and retaken shorten the
// ceiling to 300 ms for the row (never in parallel): the wait sleeps no interval that would end
// past it, so their refusal comes at least leaseKeptAfter in.
const leaseKeptAfter = 200 * time.Millisecond

func leaseHolder(t *testing.T, kind string, lease int) *writerLease {
	if saved := leaseWait; kind != "lets_go" {
		leaseWait.ceiling = 300 * time.Millisecond
		t.Cleanup(func() { leaseWait = saved })
	}
	l := &writerLease{lease: lease, letGo: kind == "lets_go", retake: kind == "retaken"}
	t.Cleanup(l.close)
	return l
}

func (l *writerLease) take(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(name)
	if _, leaseErr := unix.FcntlInt(f.Fd(), unix.F_SETLEASE, l.lease); err == nil && leaseErr == nil {
		l.file = f
		l.holders.Add(1)
		go l.hold(f, l.retake, leaseWait.first, time.Now())
	} else {
		_ = f.Close()
	}
}

// hold polls every 100 µs until the file is closed. A Go sleep under 1 ms lasts about 1 ms (the
// runtime's poller waits whole milliseconds), the opener's first pause, so a retaking holder let go
// just as the opener tried again (FLAKE-64): it polls about every first/50 by nanosleep, with a 1 µs
// timer slack, on a thread locked for good, which ends with the holder. fresh is when it last saw
// its lease held and unbroken (taken, at first).
func (l *writerLease) hold(f *os.File, retaking bool, first time.Duration, fresh time.Time) {
	defer l.holders.Done()
	nap, lost := func() { time.Sleep(100 * time.Microsecond) }, false
	if retaking {
		runtime.LockOSThread()
		_ = unix.Prctl(unix.PR_SET_TIMERSLACK, uintptr(time.Microsecond), 0, 0, 0)
		pause := unix.NsecToTimespec(int64(first / 50))
		nap = func() { _ = unix.Nanosleep(&pause, nil) }
	}
	for open := true; open; l.mu.Unlock() {
		nap()
		l.mu.Lock()
		polled := time.Now()
		if held, err := unix.FcntlInt(f.Fd(), unix.F_GETLEASE, 0); l.file == f && err == nil && held != l.lease {
			l.saw = true
			if l.letGo {
				_, _ = unix.FcntlInt(f.Fd(), unix.F_SETLEASE, unix.F_UNLCK)
			}
			if l.retake {
				fresh, lost = l.retaken(f.Fd(), fresh, lost, first)
			}
		} else if err == nil {
			fresh = polled
		}
		open = l.file == f
	}
}

// retaken lets go and takes the lease again by raw fcntl calls, which the Go scheduler cannot run
// between, only within first/2 of fresh: the break came later, and the opener tries again at least
// first after it, so the gap closes before. Seen later (the holder held up), the broken lease is
// kept; one lost is taken again at once. The kernel refuses the re-take (EAGAIN) while it still
// counts the opener's failed open, so a refusal is retried for at most first/4.
func (l *writerLease) retaken(fd uintptr, fresh time.Time, lost bool, first time.Duration) (time.Time, bool) {
	if !lost && time.Since(fresh) > first/2 {
		return fresh, false
	}
	_, _, _ = unix.RawSyscall(unix.SYS_FCNTL, fd, unix.F_SETLEASE, unix.F_UNLCK)
	_, _, errno := unix.RawSyscall(unix.SYS_FCNTL, fd, unix.F_SETLEASE, uintptr(l.lease))
	for end := time.Now().Add(first / 4); errno == unix.EAGAIN && time.Now().Before(end); {
		_, _, errno = unix.RawSyscall(unix.SYS_FCNTL, fd, unix.F_SETLEASE, uintptr(l.lease))
	}
	return time.Now(), errno != 0
}

func (l *writerLease) do(fn func(fd uintptr)) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		fn(l.file.Fd())
	}
	return l.file != nil
}

func (l *writerLease) broken() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.saw }

func (l *writerLease) release() {
	l.do(func(fd uintptr) { l.retake = false; _, _ = unix.FcntlInt(fd, unix.F_SETLEASE, unix.F_UNLCK) })
}

func (l *writerLease) close() {
	l.do(func(uintptr) { _ = l.file.Close(); l.file = nil })
	l.holders.Wait()
}

// A read lease held on the journal when a write open starts is waited out: a holder that lets
// go at the break costs a moment; one kept past the ceiling (300 ms here) gives the site's open
// text when the wait ends, nothing appended or truncated, and its lease reads F_UNLCK.
func TestWriterLeaseWaitedOut(t *testing.T) {
	for index, row := range slices.Repeat([]struct {
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
			return readerJournalPath(c), func() error { return c.journal(context.Background(), "stop", "operation", "started") }
		}},
		{"discard_tail", "cannot open torn local cell journal", func(t *testing.T, l *writerLease) (string, func() error) {
			c, progress := writerTornCell(t)
			l.take(readerJournalPath(c))
			return readerJournalPath(c), func() error { return c.discardJournalTail(context.Background(), progress) }
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
	}, 2) {
		holder := []string{"lets_go", "kept"}[index/4]
		t.Run(row.name+"/"+holder, func(t *testing.T) {
			l := leaseHolder(t, holder, unix.F_RDLCK)
			name, call := row.setup(t, l)
			size, started := writerSize(name), time.Now()
			got, held := readerWatchdog(t, call, l.release), -1
			if !l.do(func(fd uintptr) { held, _ = unix.FcntlInt(fd, unix.F_GETLEASE, 0) }) {
				t.Fatal("this kernel or sandbox refused the read lease the writer row needs")
			}
			if holder == "lets_go" && (got != nil || !l.broken()) || holder == "kept" && (got == nil || got.Error() != row.want || writerSize(name) != size || held != unix.F_UNLCK || time.Since(started) < leaseKeptAfter) {
				t.Fatalf("the leased journal open gave its open text %t: size kept %t, break begun %t", got != nil && got.Error() == row.want, writerSize(name) == size, held == unix.F_UNLCK)
			}
		})
	}
}

// The checkpoint verbs wait out a read lease held on the journal when they start: a holder that
// lets go at the break costs a moment and the verb succeeds; one kept past the ceiling (300 ms
// here) gives today's refusal when the wait ends: nothing is journaled, create leaves recovery/
// itself absent or listing no checkpoint (R9: with the root a directory, not-exist is absence on
// Linux), and a delete's checkpoint stays complete.
func TestWriterLeaseCheckpointVerbsWaitedOut(t *testing.T) {
	for _, verb := range []string{"create", "delete"} {
		for _, run := range []string{"legacy/lets_go", "legacy/kept", "selected/lets_go", "selected/kept"} {
			t.Run(verb+"/"+run, func(t *testing.T) {
				layout, holder, _ := strings.Cut(run, "/")
				f, _ := checkpointFixture(t, layout, false)
				c, ids := f.cell, []string(nil)
				if verb == "delete" && (c.Checkpoint(t.Context(), checkpointOptions(f)) != nil || len(checkpointIDs(t, c)) != 1) {
					t.Fatal("cannot prepare the one checkpoint to delete")
				}
				ids = checkpointIDs(t, c)
				before, err := os.ReadFile(readerJournalPath(c))
				l := leaseHolder(t, holder, unix.F_RDLCK)
				l.take(readerJournalPath(c))
				if err != nil || !l.do(func(uintptr) {}) {
					t.Fatal("this kernel or sandbox refused the read lease the writer row needs")
				}
				started := time.Now()
				err = readerWatchdog(t, func() error {
					if verb == "create" {
						return c.Checkpoint(t.Context(), checkpointOptions(f))
					}
					return c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: ids[0]})
				}, l.release)
				if l.close(); holder == "lets_go" {
					if err != nil || !l.broken() || len(checkpointIDs(t, c)) != 1-len(ids) {
						t.Fatal("the checkpoint verb did not wait out a journal lease whose holder let go at the break")
					}
					return
				}
				after, readErr := os.ReadFile(readerJournalPath(c))
				if failure := ClassifyCheckpointFailure(err); failure.Class != "local_checkpoint_operation_failed" || err.Error() != "cannot open local cell journal" || readErr != nil || !bytes.Equal(before, after) || time.Since(started) < leaseKeptAfter {
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
// the verb when Inspect lists). A holder that lets go at the break costs a moment and both
// succeed; one kept past the ceiling (300 ms here) stops a write lease at Inspect and a read lease
// at the verb's journal write, when the wait ends, with the operation failure class and no reason;
// nothing is journaled, create makes no checkpoint and delete keeps its own. Once it lets go, both succeed.
func TestWriterLeaseCheckpointCommandWaitedOut(t *testing.T) {
	for _, lease := range []string{"read", "write"} {
		for _, verb := range []string{"create", "delete"} {
			for _, run := range []string{"legacy/lets_go", "legacy/kept", "selected/lets_go", "selected/kept"} {
				t.Run(lease+"/"+verb+"/"+run, func(t *testing.T) {
					layout, holder, _ := strings.Cut(run, "/")
					f, _ := checkpointFixture(t, layout, false)
					c, ids, l := f.cell, []string(nil), leaseHolder(t, holder, map[string]int{"read": unix.F_RDLCK, "write": unix.F_WRLCK}[lease])
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
					l.take(readerJournalPath(c))
					if err != nil || !l.do(func(uintptr) {}) {
						t.Fatal("this kernel or sandbox refused the lease the writer row needs")
					}
					var observed, called error
					started := time.Now()
					_ = readerWatchdog(t, func() error {
						if _, observed = c.Inspect(t.Context()); observed == nil {
							called = call()
						}
						return nil
					}, l.release)
					if l.close(); holder == "lets_go" {
						if observed != nil || called != nil || !l.broken() || len(checkpointIDs(t, c)) != 1-len(ids) {
							t.Fatal("the checkpoint command did not wait out a journal lease whose holder let go at the break")
						}
						return
					}
					after, readErr := os.ReadFile(readerJournalPath(c))
					got, want := errors.Join(observed, called), map[string]string{"read": "cannot open local cell journal", "write": "cannot read local cell journal"}[lease]
					stopped := (observed == nil) == (lease == "read") && got != nil && got.Error() == want
					kept := readErr == nil && bytes.Equal(before, after) && slices.Equal(checkpointIDs(t, c), ids) && (verb == "create" || checkpointSummaryOf(t, c, ids[0]).Complete)
					if !stopped || ClassifyCheckpointFailure(got) != (CheckpointFailure{Class: "local_checkpoint_operation_failed"}) || !kept || time.Since(started) < leaseKeptAfter {
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
