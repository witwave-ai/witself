//go:build linux

package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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
			if ctx.Err() == nil {
				t.Fatal("the detached row context was not done when its wait ended")
			}
			if _, broken := leaseContextBroken(l); !broken {
				t.Fatal("the holder never saw the break of the detached row lease")
			}
			l.close() // a plain read of a write-leased file would wait out the kernel's break
			if after, afterErr := os.ReadFile(name); readErr != nil || afterErr != nil || !bytes.Equal(before, after) {
				t.Fatal("the leased file changed during the detached row")
			}
			if errors.As(err, &cut) {
				t.Fatal("a detached wait returned the lease wait cut")
			}
			if errors.Is(err, context.Canceled) {
				t.Fatal("a detached wait returned its context error")
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) {
				t.Fatal("a detached wait did not keep the lease refusal")
			}
			if waited < leaseWait.ceiling-leaseWait.longest {
				t.Fatal("a detached wait did not wait to the ceiling")
			}
		}
	}
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
		// Inspect probes through acquireLock directly. The released mutators go through own.
		{"inspect_lock_probe", ".owner.lock", "localcell: open owner lock: resource temporarily unavailable", "local_checkpoint_operation_failed", unix.F_RDLCK, 500 * time.Millisecond, 100 * time.Millisecond},
		{"create_owner_lock", ".owner.lock", "localcell: open owner lock: resource temporarily unavailable", "local_checkpoint_operation_failed", unix.F_RDLCK, 500 * time.Millisecond, 0},
		{"delete_operation_lock", "run/operation.lock", "localcell: open operation lock: resource temporarily unavailable", "local_checkpoint_operation_failed", unix.F_RDLCK, 500 * time.Millisecond, 0},
		{"create_fence", "journal/maintenance.json", "local_maintenance_fence_active", "local_maintenance_fence_active", unix.F_WRLCK, 2 * time.Second, 200 * time.Millisecond},
		{"create_pg_version", "pg/PG_VERSION", "local_checkpoint_source_unsupported", "local_checkpoint_source_unsupported", unix.F_WRLCK, 2 * time.Second, 200 * time.Millisecond},
		{"inspect_witness", "", "cannot read local cell checkpoint witness", "local_checkpoint_operation_failed", unix.F_WRLCK, 2 * time.Second, 200 * time.Millisecond},
		// LC-LEASE-2D: the listing's own wait, through the detached checkpointSummaries and listCheckpoints.
		{"inspect_listing", "", "cannot read local cell checkpoint witness", "local_checkpoint_operation_failed", unix.F_WRLCK, 500 * time.Millisecond, 100 * time.Millisecond},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, _ := checkpointFixture(t, "legacy", false)
			c, saved, leased := f.cell, leaseWait, f.cell.path(filepath.FromSlash(row.file))
			t.Cleanup(func() { leaseWait = saved })
			if row.file == "" || row.name == "delete_operation_lock" {
				if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil || len(checkpointIDs(t, c)) != 1 {
					t.Fatal("cannot prepare the released row's checkpoint")
				}
			}
			if row.file == "" {
				leased = filepath.Join(c.recoveryDir(), checkpointIDs(t, c)[0], checkpointWitnessName)
			} else if row.name == "create_owner_lock" {
				// With both names present the pre-admission probe defers to own.
				if leaseRecordPlant(t, c, "locks") != nil {
					t.Fatal("cannot prepare both lock files for checkpoint creation")
				}
			} else if row.name == "create_fence" {
				updateTestFence(t, c)
			}
			journal, kept := retainedRead(t, c, "journal/operations.jsonl"), checkpointIDs(t, c)
			l := leaseTaken(t, leaseHolder(t, map[bool]string{true: "retaken", false: "kept"}[row.name == "create_pg_version"], row.lease), leased)
			leaseWait.ceiling = row.ceiling
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			seen := make(chan time.Time, 1)
			go func() {
				for deadline := time.Now().Add(5 * time.Second); !l.broken() && time.Now().Before(deadline); {
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
				if row.name == "delete_operation_lock" {
					return c.DeleteCheckpoint(ctx, DeleteCheckpointOptions{CheckpointID: kept[0]})
				}
				return c.Checkpoint(ctx, checkpointOptions(f))
			}, l.release)
			returned, waited, cut := time.Now(), <-seen, (*leaseWaitCut)(nil)
			if ctx.Err() == nil {
				t.Fatal("the released row context was not done when its verb returned")
			}
			if !l.broken() {
				t.Fatal("the released row never met its lease")
			}
			if err == nil || err.Error() != row.text {
				t.Fatal("the released row did not end with its parent text")
			}
			if ClassifyCheckpointFailure(err).Class != row.class {
				t.Fatal("the released row did not end with its parent class")
			}
			if errors.As(err, &cut) {
				t.Fatal("the released row returned the lease wait cut")
			}
			if errors.Is(err, context.Canceled) {
				t.Fatal("the released row returned its context error")
			}
			if returned.Sub(started) < leaseWait.ceiling-leaseWait.longest {
				t.Fatal("the released row did not wait to the ceiling from its start")
			}
			if returned.Sub(waited) < leaseWait.ceiling-2*leaseWait.longest {
				t.Fatal("the released row did not wait to the ceiling after its lease broke")
			}
			if !bytes.Equal(journal, retainedRead(t, c, "journal/operations.jsonl")) {
				t.Fatal("the released row journaled")
			}
			if !slices.Equal(checkpointIDs(t, c), kept) {
				t.Fatal("the released row changed the checkpoints")
			}
			if len(inspection.Checkpoints) != 0 {
				t.Fatal("the released row listed a checkpoint it could not read")
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
		if returned.Before(at) {
			t.Fatal("the leased control program run ended before the first signal")
		}
		if returned.After(at.Add(c.cfg.Timeout / 2)) {
			t.Fatal("a first signal did not end the leased control program's wait")
		}
		if failure.Class != "local_checkpoint_operation_failed" {
			t.Fatal("a first signal did not fail create with the operation class")
		}
		if failure.CleanupIncomplete {
			t.Fatal("a first signal left the create cleanup incomplete")
		}
		if r.calls["control"] != 1 {
			t.Fatal("a first signal did not end create in its one control run")
		}
		if journalErr != nil || progress.pending != nil || progress.tornTail {
			t.Fatal("a first signal left create journaled")
		}
		if !reflect.DeepEqual(checkpointLeasedTree(t, c, lease), before) {
			t.Fatal("a first signal left the create tree changed")
		}
	})
}

// Each LC-LEASE-2C2 R5 declaration on a lease never let go (ceiling 2 s; a read
// lease on the owner lock) cancelled 200 ms in returns the cut well before the
// ceiling (within half of it after the cancel), never nil, an active fence or a
// pid alone; nothing written, admit never called.
// background (ceiling 300 ms): HEAD's refusal; the lockless probe defers (nil).
func TestLeaseWaitFence(t *testing.T) {
	const ownerStage, operationStage = "localcell: open owner lock: ", "localcell: open operation lock: "
	noLock := func(lock *operationLock, err error) (bool, error) {
		if lock != nil {
			_ = lock.Close()
		}
		return lock == nil, err
	}
	own := func(ctx context.Context, c *Cell) (bool, error) { return noLock(c.own(ctx, false)) }
	existing := func(ctx context.Context, c *Cell) (bool, error) { return noLock(c.ownExisting(ctx)) }
	verify := func(ctx context.Context, c *Cell) (bool, error) { return true, c.verifyAll(ctx) }
	recoverRecord := func(ctx context.Context, c *Cell) (bool, error) { return true, c.recoverPostgresRecord(ctx) }
	for _, row := range []struct {
		name, prepare, leased, refusal string
		lease                          int
		call                           func(ctx context.Context, c *Cell) (bool, error)
	}{
		{"own_owner", "locks", ".owner.lock", ownerStage + syscall.EWOULDBLOCK.Error(), unix.F_RDLCK, own},
		{"own_operation", "locks", "run/operation.lock", operationStage + syscall.EWOULDBLOCK.Error(), unix.F_RDLCK, own},
		{"existing_owner", "locks", ".owner.lock", ownerStage + syscall.EWOULDBLOCK.Error(), unix.F_RDLCK, existing},
		{"existing_operation", "locks", "run/operation.lock", operationStage + syscall.EWOULDBLOCK.Error(), unix.F_RDLCK, existing},
		{"existing_fallback", "fallback", "run/operation.lock", operationStage + syscall.EWOULDBLOCK.Error(), unix.F_RDLCK, existing},
		{"verify_first_read", "record", "pg/postmaster.pid", "cannot read PostgreSQL process record", unix.F_WRLCK, verify},
		{"cold_first_read", "record", "pg/postmaster.pid", "cannot read PostgreSQL process record", unix.F_WRLCK, func(ctx context.Context, c *Cell) (bool, error) {
			return true, c.checkpointCold(ctx)
		}},
		{"recover_first_read", "record", "pg/postmaster.pid", "cannot read PostgreSQL process record", unix.F_WRLCK, recoverRecord},
		{"verify_state", "server_record", "run/server.json", "cannot open local cell record", unix.F_WRLCK, verify},
		{"recover_state", "postgres_record", "run/postgres.json", "cannot open local cell record", unix.F_WRLCK, recoverRecord},
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
			return calls == 0 && lockDirectory(c.path("run")) == nil && errors.Is(run, os.ErrNotExist), err
		}},
	} {
		for _, background := range []bool{true, false} {
			t.Run(row.name+map[bool]string{true: "/background"}[background], func(t *testing.T) {
				c, _ := unitCell(t)
				leased := c.path(filepath.FromSlash(row.leased))
				if leaseRecordPlant(t, c, row.prepare) != nil {
					t.Fatal("cannot prepare the fence row cell")
				}
				if lockDirectory(c.cfg.Root) != nil {
					t.Fatal("the fence row root is not a real directory")
				}
				if lockDirectory(c.path("run")) != nil {
					t.Fatal("the fence row process record parent is not a real directory")
				}
				postgresRecord := c.path("run", "postgres.json")
				original, statErr := os.Lstat(postgresRecord)
				if row.prepare == "postgres_record" {
					if statErr != nil {
						t.Fatal("the recovery row requires its planted process record")
					}
				} else if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("the fence row requires an absent PostgreSQL process record")
				}
				if row.prepare == "fallback" {
					if _, err := os.Lstat(c.path(".owner.lock")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("the fallback row requires an absent stable lock")
					}
				}
				tree := checkpointTreeState(t, c)
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
					time.AfterFunc(leaseWait.ceiling/10, func() { point <- time.Now(); cancel() })
				}
				empty, started := false, time.Now()
				err := readerWatchdog(t, func() (err error) { empty, err = row.call(ctx, c); return err }, l.release)
				returned, cut := time.Now(), (*leaseWaitCut)(nil)
				if _, broken := leaseContextBroken(l); !broken {
					t.Fatal("the holder never saw the break of the fence row lease")
				}
				l.close() // a plain read of a write-leased file would wait out the kernel's break
				after, afterErr := os.ReadFile(leased)
				if readErr != nil || afterErr != nil || !bytes.Equal(before, after) {
					t.Fatal("the fence row changed its leased file")
				}
				if original != nil {
					current, err := os.Lstat(postgresRecord)
					if err != nil {
						t.Fatal("the recovery row lost its planted process record")
					}
					if !os.SameFile(original, current) {
						t.Fatal("the recovery row replaced its planted process record")
					}
				}
				afterTree := checkpointTreeState(t, c)
				if row.prepare == "fallback" {
					delete(afterTree, ".owner.lock") // the next read-only acquisition must reuse this new leaf
				}
				if !reflect.DeepEqual(tree, afterTree) {
					t.Fatal("the fence row changed the cell tree")
				}
				lockRow := row.prepare == "locks" || row.prepare == "fallback"
				if lockRow {
					held, err := acquireLock(t.Context(), c.cfg.Root, false)
					if err != nil {
						t.Fatal("ownership reacquisition failed after the holder closed")
					}
					if held == nil {
						t.Fatal("ownership reacquisition found no stable lock")
					}
					if len(held.files) != 2 {
						t.Fatal("ownership reacquisition did not hold both retained locks")
					}
					if held.Close() != nil {
						t.Fatal("cannot release reacquired ownership")
					}
				}
				if !empty {
					t.Fatal("the fence row gave a result beside its error")
				}
				if background {
					if errors.As(err, &cut) {
						t.Fatal("the placeholder context cut the fence row wait")
					}
					if (row.refusal == "") != (err == nil) || err != nil && err.Error() != row.refusal {
						t.Fatal("the placeholder context did not keep the fence row refusal")
					}
					if returned.Sub(started) < leaseWait.ceiling-leaseWait.longest {
						t.Fatal("the placeholder context did not keep the fence row wait to the ceiling")
					}
					return
				}
				if !errors.As(err, &cut) {
					t.Fatal("the fence row declaration did not return the lease wait cut")
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatal("the fence row cut does not carry its context error")
				}
				if errors.Is(err, syscall.EWOULDBLOCK) {
					t.Fatal("the fence row cut reads as a lease refusal")
				}
				if errors.Is(err, os.ErrNotExist) {
					t.Fatal("the fence row cut reads as an absent file")
				}
				if errors.Is(err, ErrLocked) {
					t.Fatal("the fence row cut reads as ownership contention")
				}
				if lockRow && !strings.HasPrefix(err.Error(), strings.TrimSuffix(row.refusal, syscall.EWOULDBLOCK.Error())) {
					t.Fatal("the ownership cut lost its lock stage")
				}
				if errors.Is(err, errMaintenanceFenceActive) {
					t.Fatal("the fence row cut reads as an active fence")
				}
				at := <-point
				if returned.Before(at) {
					t.Fatal("the fence row declaration returned before its context was done")
				}
				if returned.After(at.Add(leaseWait.ceiling / 2)) {
					t.Fatal("the fence row declaration did not return well before the ceiling once its context was done")
				}
			})
		}
	}
}
