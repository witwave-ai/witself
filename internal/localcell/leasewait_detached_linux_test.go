//go:build linux

package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// LC-LEASE-2C2 R3: under leaseWaitDetached a lease never let go on 2A's four
// plants (ceiling 500 ms) is waited out to the ceiling though the context is
// cancelled 100 ms in: today's refusal, never a cut, nothing written.
func init() {
	for _, side := range []string{"reader", "writer", "lock", "checkpoint"} {
		leaseWaitDetachedRows[side] = func(t *testing.T) {
			name, lease, open := leaseContextPlant(t, side)
			before, readErr := os.ReadFile(name)
			l := leaseTaken(t, leaseHolder(t, "kept", lease), name)
			leaseWait.ceiling = 500 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			time.AfterFunc(100*time.Millisecond, cancel)
			var f *os.File
			started := time.Now()
			err := readerWatchdog(t, func() (err error) { f, err = open(leaseWaitDetached(ctx)); return err }, l.release)
			waited, cut := time.Since(started), (*leaseWaitCut)(nil)
			if f != nil {
				_ = f.Close()
			}
			if _, broken := leaseContextBroken(l); !broken {
				t.Fatal("the holder never saw the break of the detached row lease")
			}
			l.close() // a plain read of a write-leased file would wait out the kernel's break
			if after, afterErr := os.ReadFile(name); readErr != nil || afterErr != nil || !bytes.Equal(before, after) {
				t.Fatal("the leased file changed during the detached row")
			}
			if errors.As(err, &cut) || errors.Is(err, context.Canceled) || !errors.Is(err, syscall.EWOULDBLOCK) {
				t.Fatal("a detached wait did not keep the lease refusal")
			}
			if waited < leaseWait.ceiling-leaseWait.longest {
				t.Fatal("a detached wait did not wait to the ceiling")
			}
		}
	}
}

// leaseRetaken takes a lease on name again at once each time it is broken, as
// the start-state table's holder: F_GETLEASE polled about every 20 µs on a
// locked OS thread, raw fcntl re-takes retried for at most 250 µs, so no open
// gets in. It returns whether it saw a break or lost a re-take, and its stop.
func leaseRetaken(t *testing.T, name string, lease int) (saw, lost *atomic.Bool, stop func()) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if _, leaseErr := unix.FcntlInt(uintptr(fd), unix.F_SETLEASE, lease); err != nil || leaseErr != nil {
		_ = unix.Close(fd)
		t.Fatal("this kernel or sandbox refused the lease the row needs")
	}
	saw, lost, stopped, done := new(atomic.Bool), new(atomic.Bool), make(chan struct{}), make(chan struct{})
	go func() {
		runtime.LockOSThread() // never unlocked: the thread and its timer slack end with the holder
		defer close(done)
		defer func() { _ = unix.Close(fd) }()
		_ = unix.Prctl(unix.PR_SET_TIMERSLACK, 1000, 0, 0, 0)
		for pause := (unix.Timespec{Nsec: 20000}); ; _ = unix.Nanosleep(&pause, nil) {
			select {
			case <-stopped:
				return
			default:
			}
			if held, err := unix.FcntlInt(uintptr(fd), unix.F_GETLEASE, 0); err != nil || held == lease {
				continue
			}
			saw.Store(true)
			_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETLEASE, unix.F_UNLCK)
			for deadline := time.Now().Add(250 * time.Microsecond); ; {
				if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETLEASE, lease); err == nil || time.Now().After(deadline) {
					lost.Store(lost.Load() || err != nil)
					break
				}
			}
		}
	}()
	stop = sync.OnceFunc(func() { close(stopped); <-done })
	t.Cleanup(stop)
	return saw, lost, stop
}

// leaseReleasedDetached are LC-LEASE-2C2's TestLeaseWaitContextReleased rows,
// each HEAD's behaviour (OQ2): a lease never let go (taken again at once on
// PG_VERSION) is waited out to the ceiling though the context is cancelled after
// the first break: the parent text and class, never a cut; nothing journaled or
// listed, checkpoints kept.
func leaseReleasedDetached(t *testing.T) {
	for _, row := range []struct {
		name, file, text, class string
		lease                   int
		ceiling, cancel         time.Duration
	}{
		{"inspect_owner_lock", ".owner.lock", "localcell: open owner lock: resource temporarily unavailable", "local_checkpoint_operation_failed", unix.F_RDLCK, 500 * time.Millisecond, 100 * time.Millisecond},
		{"create_fence", "journal/maintenance.json", "local_maintenance_fence_active", "local_maintenance_fence_active", unix.F_WRLCK, 2 * time.Second, 200 * time.Millisecond},
		{"create_pg_version", "pg/PG_VERSION", "local_checkpoint_source_unsupported", "local_checkpoint_source_unsupported", unix.F_WRLCK, 2 * time.Second, 200 * time.Millisecond},
		{"inspect_witness", "", "cannot read local cell checkpoint witness", "local_checkpoint_operation_failed", unix.F_WRLCK, 2 * time.Second, 200 * time.Millisecond},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, _ := checkpointFixture(t, "legacy", false)
			c, saved, leased := f.cell, leaseWait, f.cell.path(filepath.FromSlash(row.file))
			t.Cleanup(func() { leaseWait = saved })
			if row.name == "inspect_witness" && (c.Checkpoint(t.Context(), checkpointOptions(f)) != nil || len(checkpointIDs(t, c)) != 1) {
				t.Fatal("cannot prepare the checkpoint the witness row lists")
			} else if row.name == "inspect_witness" {
				leased = filepath.Join(c.recoveryDir(), checkpointIDs(t, c)[0], checkpointWitnessName)
			} else if row.name == "create_fence" {
				updateTestFence(t, c)
			}
			journal, kept := retainedRead(t, c, "journal/operations.jsonl"), checkpointIDs(t, c)
			var broken func() bool
			var release func()
			lost := func() bool { return false }
			if row.name == "create_pg_version" {
				saw, gone, stop := leaseRetaken(t, leased, row.lease)
				broken, lost, release = saw.Load, gone.Load, stop
			} else {
				l := leaseTaken(t, leaseHolder(t, "kept", row.lease), leased)
				broken, release = l.broken, l.release
			}
			leaseWait.ceiling = row.ceiling
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			seen := make(chan time.Time, 1)
			go func() {
				for deadline := time.Now().Add(5 * time.Second); !broken() && time.Now().Before(deadline); {
					time.Sleep(time.Millisecond)
				}
				seen <- time.Now()
				time.Sleep(row.cancel)
				cancel()
			}()
			var inspection Inspection
			started := time.Now()
			err := readerWatchdog(t, func() (err error) {
				if strings.HasPrefix(row.name, "inspect") {
					inspection, err = c.Inspect(ctx)
					return err
				}
				return c.Checkpoint(ctx, checkpointOptions(f))
			}, release)
			returned, waited, cut := time.Now(), <-seen, (*leaseWaitCut)(nil)
			if !broken() || lost() {
				t.Fatal("the released row never met its lease, or its holder lost it")
			}
			if err == nil || err.Error() != row.text || ClassifyCheckpointFailure(err).Class != row.class {
				t.Fatal("the released row did not end with its parent text and class")
			}
			if errors.As(err, &cut) || errors.Is(err, context.Canceled) {
				t.Fatal("the released row saw its context end the wait")
			}
			if returned.Sub(started) < leaseWait.ceiling-leaseWait.longest || returned.Sub(waited) < leaseWait.ceiling-2*leaseWait.longest {
				t.Fatal("the released row did not wait to the ceiling")
			}
			if !bytes.Equal(journal, retainedRead(t, c, "journal/operations.jsonl")) || !slices.Equal(checkpointIDs(t, c), kept) || len(inspection.Checkpoints) != 0 {
				t.Fatal("the released row journaled, changed the checkpoints or listed one it could not read")
			}
		})
	}
}

// A first signal ends pg_controldata's wait on a leased global/pg_control (S3):
// create cancelled 200 ms in ends soon after, well before the cell's Timeout
// (2 s here), nothing journaled or changed, as at HEAD (WithoutCancel: 2 s).
func TestLeaseWaitDetachedControl(t *testing.T) {
	t.Run("first_signal", func(t *testing.T) {
		f, r := checkpointFixture(t, "legacy", false)
		c := f.cell
		r.production, c.cfg.Binaries.PostgresDir, c.cfg.Timeout = true, testPrivateRoot(t), 2*time.Second
		standIn := "#!/bin/sh\n: <\"$2/global/pg_control\" || exit 1\nprintf '%s' '" + controlReading(checkpointSyntheticControl) + "'\n"
		if testWriteFile(filepath.Join(c.cfg.Binaries.PostgresDir, "pg_controldata"), []byte(standIn), 0o700) != nil {
			t.Fatal("cannot write the blocking stand-in control program")
		}
		before, lease := checkpointTreeState(t, c), checkpointLease(t, c, "pg/global/pg_control")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		point := make(chan time.Time, 1)
		time.AfterFunc(200*time.Millisecond, func() { point <- time.Now(); cancel() })
		failure := ClassifyCheckpointFailure(c.Checkpoint(ctx, checkpointOptions(f)))
		returned, at := time.Now(), <-point
		progress, journalErr := c.readJournal(context.Background())
		if returned.Before(at) || returned.After(at.Add(c.cfg.Timeout/2)) {
			t.Fatal("a first signal did not end the leased control program's wait")
		}
		if failure.Class != "local_checkpoint_operation_failed" || failure.CleanupIncomplete || r.calls["control"] != 1 {
			t.Fatal("a first signal did not fail create as an ended control run")
		}
		if journalErr != nil || progress.pending != nil || progress.tornTail || !reflect.DeepEqual(checkpointLeasedTree(t, c, lease), before) {
			t.Fatal("a first signal left create journaled or its tree changed")
		}
	})
}

// Each LC-LEASE-2C2 R5 declaration on a lease never let go (ceiling 2 s; a read
// lease on the owner lock) cancelled 200 ms in returns the cut soon after, never
// nil, an active fence or a pid alone; nothing written, admit never called.
// background (ceiling 300 ms): HEAD's refusal; the lockless probe defers (nil).
func TestLeaseWaitFence(t *testing.T) {
	for _, row := range []struct {
		name, prepare, leased, refusal string
		lease                          int
		call                           func(ctx context.Context, c *Cell) (bool, error)
	}{
		{"allows", "fence", "journal/maintenance.json", errMaintenanceFenceActive.Error(), unix.F_WRLCK, func(ctx context.Context, c *Cell) (bool, error) {
			return true, c.maintenanceFenceAllows(ctx, 0)
		}},
		{"postmaster", "record", "pg/postmaster.pid", "cannot read PostgreSQL process record", unix.F_WRLCK, func(ctx context.Context, c *Cell) (bool, error) {
			pid, err := c.postmaster(ctx)
			return pid == 0, err
		}},
		{"refuse_before_lock", "lockless", ".owner.lock", "", unix.F_RDLCK, func(ctx context.Context, c *Cell) (bool, error) {
			calls := 0
			err := c.refuseBeforeLockNames(ctx, func() error { calls++; return nil })
			_, run := os.Lstat(c.path("run", "operation.lock"))
			return calls == 0 && errors.Is(run, os.ErrNotExist), err
		}},
	} {
		for _, background := range []bool{true, false} {
			t.Run(row.name+map[bool]string{true: "/background"}[background], func(t *testing.T) {
				c, _ := unitCell(t)
				leased := c.path(filepath.FromSlash(row.leased))
				if leaseRecordPlant(t, c, row.prepare) != nil {
					t.Fatal("cannot prepare the fence row cell")
				}
				before, readErr := os.ReadFile(leased)
				l := leaseHolder(t, "kept", row.lease)
				if !background {
					leaseWait.ceiling = 2 * time.Second
				}
				leaseTaken(t, l, leased)
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				point := make(chan time.Time, 1)
				if background {
					ctx = context.Background()
				} else {
					time.AfterFunc(200*time.Millisecond, func() { point <- time.Now(); cancel() })
				}
				empty, started := false, time.Now()
				err := readerWatchdog(t, func() (err error) { empty, err = row.call(ctx, c); return err }, l.release)
				returned, cut := time.Now(), (*leaseWaitCut)(nil)
				if _, broken := leaseContextBroken(l); !broken {
					t.Fatal("the holder never saw the break of the fence row lease")
				}
				l.close() // a plain read of a write-leased file would wait out the kernel's break
				after, afterErr := os.ReadFile(leased)
				if _, record := os.Lstat(c.path("run", "postgres.json")); readErr != nil || afterErr != nil || !bytes.Equal(before, after) || !errors.Is(record, os.ErrNotExist) || !empty {
					t.Fatal("the fence row wrote, or gave a result beside its error")
				}
				if background {
					if errors.As(err, &cut) || (row.refusal == "") != (err == nil) || err != nil && err.Error() != row.refusal || returned.Sub(started) < leaseKeptAfter {
						t.Fatal("the placeholder context did not keep the fence row refusal at the ceiling")
					}
					return
				}
				if !errors.As(err, &cut) || !errors.Is(err, context.Canceled) {
					t.Fatal("the fence row declaration did not return the lease wait cut")
				}
				if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, os.ErrNotExist) || errors.Is(err, errMaintenanceFenceActive) {
					t.Fatal("the fence row cut reads as a refusal, an absent file or an active fence")
				}
				if at := <-point; returned.Before(at) || returned.After(at.Add(leaseWait.longest+150*time.Millisecond)) {
					t.Fatal("the fence row declaration did not return soon after its context was done")
				}
			})
		}
	}
}
