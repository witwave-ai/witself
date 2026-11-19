//go:build linux

package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// leaseContextPlant is one of the four waiting opens on a regular file, with the
// lease its open conflicts with: write for the reader and the checkpoint opener,
// read for the writer and the lock open.
func leaseContextPlant(t *testing.T, side string) (string, int, func(context.Context) (*os.File, error)) {
	name := filepath.Join(t.TempDir(), "leased")
	if os.WriteFile(name, []byte(readerJournalLine), 0o600) != nil {
		t.Fatal("cannot prepare the leased context row file")
	}
	switch side {
	case "reader":
		return name, unix.F_WRLCK, func(ctx context.Context) (*os.File, error) { return openPrivateReader(ctx, name) }
	case "writer":
		return name, unix.F_RDLCK, func(ctx context.Context) (*os.File, error) {
			return openPrivateWriter(ctx, name, os.O_WRONLY|os.O_APPEND, 0o600)
		}
	case "lock":
		return name, unix.F_RDLCK, func(ctx context.Context) (*os.File, error) { return openLockFile(ctx, name, false) }
	}
	f, _ := checkpointFixture(t, "legacy", false)
	return f.cell.path("pg", "base", "1", "1259"), unix.F_WRLCK, func(ctx context.Context) (*os.File, error) {
		open, release, err := f.cell.openCheckpointSource(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		return open("base/1/1259")
	}
}

// leaseContextBroken waits up to 5 s for the holder to see the break.
func leaseContextBroken(l *writerLease) (time.Time, bool) {
	for deadline := time.Now().Add(5 * time.Second); !l.broken() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	return time.Now(), l.broken()
}

// A lease never let go (ceiling 2 s here) is waited out only until the context
// is done: cancelled 200 ms in, before the call (one attempt is made) or at a
// 200 ms deadline, each open returns a leaseWaitCut of the context's error soon
// after, never would-block or not-exist, nothing written; context.Background()
// with the kept holder (ceiling 300 ms) keeps today's refusal at the ceiling.
func TestLeaseWaitContext(t *testing.T) {
	for _, side := range []string{"reader", "writer", "lock", "checkpoint"} {
		for _, event := range []string{"cancel_during", "cancel_before", "deadline", "background"} {
			t.Run(side+"/"+event, func(t *testing.T) {
				name, lease, open := leaseContextPlant(t, side)
				before, readErr := os.ReadFile(name)
				l := leaseHolder(t, "kept", lease)
				if event != "background" {
					leaseWait.ceiling = 2 * time.Second
				}
				leaseTaken(t, l, name)
				ctx, cancel := context.WithCancel(context.Background())
				point, want := make(chan time.Time, 1), context.Canceled
				t.Cleanup(cancel)
				switch event {
				case "cancel_before":
					point <- time.Now()
					cancel()
				case "cancel_during":
					time.AfterFunc(200*time.Millisecond, func() { point <- time.Now(); cancel() })
				case "deadline":
					ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
					t.Cleanup(cancel)
					deadline, _ := ctx.Deadline()
					point <- deadline
					want = context.DeadlineExceeded
				default:
					ctx = context.Background()
				}
				var f *os.File
				started := time.Now()
				err := readerWatchdog(t, func() (err error) { f, err = open(ctx); return err }, l.release)
				returned, cut := time.Now(), (*leaseWaitCut)(nil)
				if f != nil {
					_ = f.Close()
				}
				if _, broken := leaseContextBroken(l); !broken {
					t.Fatal("the holder never saw the break of its lease")
				}
				l.close() // a plain read of a write-leased file would wait out the kernel's break
				if after, afterErr := os.ReadFile(name); readErr != nil || afterErr != nil || !bytes.Equal(before, after) {
					t.Fatal("the leased file changed during the context row")
				}
				if event == "background" {
					if !errors.Is(err, syscall.EWOULDBLOCK) || errors.As(err, &cut) || returned.Sub(started) < leaseKeptAfter {
						t.Fatal("the placeholder context did not keep the refusal at the ceiling")
					}
					return
				}
				if !errors.Is(err, want) {
					t.Fatal("the wait did not end with the context error")
				}
				if !errors.As(err, &cut) {
					t.Fatal("the wait ended by its context returned no leaseWaitCut")
				}
				if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, os.ErrNotExist) {
					t.Fatal("a cut wait reads as a lease refusal or an absent file")
				}
				if event == "cancel_before" && returned.Sub(started) > leaseWait.first+150*time.Millisecond {
					t.Fatal("a context done before the call did not end the wait after its first attempt")
				}
				if at := <-point; returned.Before(at) || returned.After(at.Add(leaseWait.longest+150*time.Millisecond)) {
					t.Fatal("the wait did not end soon after its context was done")
				}
			})
		}
	}
}

// openRegularBy returns a cut as it is, before any Lstat (R4): a write-leased
// file whose wait is cancelled 200 ms in gives the leaseWaitCut itself, never
// the open text. In removed the name is renamed aside once the opener has
// returned the cut and before openRegularBy decides, so a classifying check
// would read it as absent.
func TestLeaseWaitContextRegularBy(t *testing.T) {
	for _, row := range []string{"cancel", "removed"} {
		t.Run(row, func(t *testing.T) {
			name, lease, _ := leaseContextPlant(t, "reader")
			l := leaseHolder(t, "kept", lease)
			leaseWait.ceiling = 2 * time.Second
			leaseTaken(t, l, name)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			time.AfterFunc(200*time.Millisecond, cancel)
			opener, renamed := openPrivateReader, error(nil)
			if row == "removed" {
				opener = func(ctx context.Context, path string) (*os.File, error) {
					f, err := openPrivateReader(ctx, path)
					renamed = os.Rename(path, path+".aside")
					return f, err
				}
			}
			inspect, irregular, open := errors.New("regular by row inspect"), errors.New("regular by row irregular"), errors.New("regular by row open")
			var f *os.File
			err := readerWatchdog(t, func() (err error) {
				f, err = openRegularBy(ctx, opener, nil, name, inspect, irregular, open)
				return err
			}, l.release)
			if f != nil {
				_ = f.Close()
			}
			if renamed != nil {
				t.Fatal("cannot rename the leased name aside")
			}
			if _, isCut := err.(*leaseWaitCut); !isCut || !errors.Is(err, context.Canceled) {
				t.Fatal("openRegularBy did not return the cut itself")
			}
			if err == open || err == inspect || err == irregular || errors.Is(err, os.ErrNotExist) {
				t.Fatal("openRegularBy classified a cut wait")
			}
		})
	}
}

// The released verbs keep their documented wait (OQ2): their context is detached
// (LC-LEASE-2C2), so a command context cancelled 100 ms into a lease wait never
// let go (ceiling 500 ms here) ends no wait, and each verb ends as HEAD does:
// the journal rows with the open text, nothing journaled and the checkpoint
// kept; the copy row as a failed copy, its partial copy removed and its
// operation closed.
func TestLeaseWaitContextReleased(t *testing.T) {
	for _, row := range []struct {
		name, file, text, class string
		lease                   int
	}{
		{"create_journal", "journal/operations.jsonl", "cannot open local cell journal", "local_checkpoint_operation_failed", unix.F_RDLCK},
		{"delete_journal", "journal/operations.jsonl", "cannot open local cell journal", "local_checkpoint_operation_failed", unix.F_RDLCK},
		{"create_copy", "pg/base/1/1259", "", "local_checkpoint_copy_failed", unix.F_WRLCK},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, _ := checkpointFixture(t, "legacy", false)
			c := f.cell
			call := func(ctx context.Context) error { return c.Checkpoint(ctx, checkpointOptions(f)) }
			if row.name == "delete_journal" {
				if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil || len(checkpointIDs(t, c)) != 1 {
					t.Fatal("cannot prepare the checkpoint the delete row deletes")
				}
				options := DeleteCheckpointOptions{CheckpointID: checkpointIDs(t, c)[0]}
				call = func(ctx context.Context) error { return c.DeleteCheckpoint(ctx, options) }
			}
			journal, kept := retainedRead(t, c, "journal/operations.jsonl"), checkpointIDs(t, c)
			l := leaseHolder(t, "kept", row.lease)
			leaseWait.ceiling = 500 * time.Millisecond
			leaseTaken(t, l, c.path(filepath.FromSlash(row.file)))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			seen := make(chan time.Time, 1)
			go func() {
				at, _ := leaseContextBroken(l)
				seen <- at
				time.Sleep(100 * time.Millisecond)
				cancel()
			}()
			started := time.Now()
			err := readerWatchdog(t, func() error { return call(ctx) }, l.release)
			returned, waited, cut := time.Now(), <-seen, (*leaseWaitCut)(nil)
			progress, journalErr := c.readJournal(context.Background())
			if !l.broken() {
				t.Fatal("the released verb never met the lease")
			}
			if err == nil || ClassifyCheckpointFailure(err).Class != row.class || row.text != "" && err.Error() != row.text {
				t.Fatal("the released verb did not end with its parent text and class")
			}
			if errors.As(err, &cut) || errors.Is(err, context.Canceled) {
				t.Fatal("the released verb saw its context end the wait")
			}
			if returned.Sub(started) < leaseWait.ceiling-leaseWait.longest || returned.Sub(waited) < leaseWait.ceiling-2*leaseWait.longest {
				t.Fatal("the released verb did not wait to the ceiling")
			}
			if row.name != "create_copy" && !bytes.Equal(journal, retainedRead(t, c, "journal/operations.jsonl")) {
				t.Fatal("the released verb journaled under a lease it could not wait out")
			}
			if ids := checkpointIDs(t, c); len(ids) != len(kept) || len(kept) == 1 && ids[0] != kept[0] {
				t.Fatal("the released verb changed the checkpoints it refused")
			}
			if journalErr != nil || progress.pending != nil {
				t.Fatal("the released verb left its operation open")
			}
		})
	}
	leaseReleasedDetached(t) // LC-LEASE-2C2's rows (leasewait_detached_linux_test.go)
}
