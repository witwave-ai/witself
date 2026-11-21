package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/signal"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLeaseWaitRecords(t *testing.T) {
	for _, name := range []string{"json", "manifest", "cleanup", "journal_absent", "journal_closed", "state", "state_record", "host_lock"} {
		t.Run(name, func(t *testing.T) {
			c, _ := unitCell(t)
			path := c.path("manifest.json")
			lease := unix.F_WRLCK
			switch name {
			case "journal_closed":
				if c.journal(t.Context(), "stop", "operation", "started") != nil || c.journal(t.Context(), "stop", "operation", "done") != nil {
					t.Fatal("cannot plant a closed journal")
				}
			case "state", "state_record":
				path = c.path("run", "server.json")
				checkpointRecord(t, c, "test")
			case "host_lock":
				path, lease = c.path(".service-host.lock"), unix.F_RDLCK
				if os.WriteFile(path, nil, 0o600) != nil {
					t.Fatal("cannot plant the private host lock")
				}
			}
			journal := readerJournalPath(c)
			before, readErr := os.ReadFile(journal)
			if name == "journal_closed" {
				if readErr != nil || len(before) == 0 {
					t.Fatal("closed journal unavailable before the cut")
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal("expected an absent journal before the cut")
			}

			holder := leaseHolder(t, "kept", lease)
			leaseWait.ceiling = 2 * time.Second
			leaseTaken(t, holder, path)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			start := time.Now()
			timer := time.AfterFunc(leaseWait.ceiling/8, cancel)
			defer timer.Stop()
			empty := false
			err := readerWatchdog(t, func() error {
				switch name {
				case "json":
					var m Manifest
					err := readJSON(ctx, path, &m)
					empty = reflect.DeepEqual(m, Manifest{})
					return err
				case "manifest":
					m, err := readManifest(ctx, c.cfg.Root)
					empty = m == nil
					return err
				case "cleanup":
					interruption, err := c.cleanupInterruption(ctx)
					empty = interruption == nil
					return err
				case "journal_absent", "journal_closed":
					progress, err := c.readJournal(ctx)
					// A closed journal retains the prefix already validated before the manifest open.
					if name == "journal_closed" {
						info, statErr := os.Stat(journal)
						if statErr != nil || !os.SameFile(progress.readFile, info) || progress.durableBytes != int64(len(before)) {
							return err
						}
						progress.readFile, progress.durableBytes = nil, 0
					}
					empty = reflect.DeepEqual(progress, journalProgress{})
					return err
				case "state":
					state, err := c.processState(ctx, "server")
					empty = state == (ProcessState{})
					return err
				case "state_record":
					state, record, err := c.processStateWithRecord(ctx, "server")
					empty = state == (ProcessState{}) && record == (ProcessRecord{})
					return err
				default: // host_lock
					release, err := serviceAdapterRemovalHostLock(ctx, c)
					empty = release == nil
					if release != nil {
						release()
					}
					return err
				}
			}, holder.release)
			if time.Since(start) >= leaseWait.ceiling/2 {
				t.Fatal("record reader waited past the cut bound")
			}
			if _, broken := leaseContextBroken(holder); !broken {
				t.Fatal("record lease holder never saw the break")
			}
			holder.close()
			var cut *leaseWaitCut
			if !errors.As(err, &cut) {
				t.Fatal("record reader hid the cut")
			}
			if ctx.Err() != context.Canceled || errors.Unwrap(cut) != context.Canceled {
				t.Fatal("record cut lost its exact cancellation")
			}
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, ErrLocked) {
				t.Fatal("record cut reported absence or contention")
			}
			if !empty {
				t.Fatal("record reader returned unexpected state beside the cut")
			}
			after, err := os.ReadFile(journal)
			if (readErr == nil) != (err == nil) || (err != nil && !errors.Is(err, os.ErrNotExist)) || !bytes.Equal(before, after) {
				t.Fatal("record cut changed the journal")
			}
			if name == "host_lock" {
				release, err := serviceAdapterRemovalHostLock(t.Context(), c)
				if err != nil || release == nil {
					t.Fatal("fresh removal host lock refused after release")
				}
				release()
			}
		})
	}
}

func TestLeaseWaitCheckpointColdSecondRead(t *testing.T) {
	c, _ := unitCell(t)
	workerPath, serverPath := c.path("run", "worker.json"), c.path("run", "server.json")
	staged := serverPath + ".next"
	for _, path := range []string{workerPath, serverPath, staged} {
		if writeAtomic(path, ProcessRecord{PID: checkpointUnallocatedPID, StartTime: "stale"}, nil) != nil {
			t.Fatal("cannot plant stale process records for the cold check")
		}
	}
	worker := leaseHolder(t, "kept", unix.F_WRLCK)
	server := leaseHolder(t, "kept", unix.F_WRLCK)
	leaseWait.ceiling = 2 * time.Second
	leaseTaken(t, worker, workerPath)
	leaseTaken(t, server, staged)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	var result error
	go func() {
		result = c.checkpointCold(ctx)
		close(done)
	}()
	defer func() {
		worker.release()
		server.release()
		<-done
	}()

	// verifyAll reads server before worker. Keep it at worker until the
	// replacement is in place, then cancel only at the server re-read.
	if _, broken := leaseContextBroken(worker); !broken {
		t.Fatal("the first cold scan did not reach the worker record")
	}
	if os.Rename(staged, serverPath) != nil {
		t.Fatal("cannot replace the server record during the first cold scan")
	}
	worker.release()
	if _, broken := leaseContextBroken(server); !broken {
		t.Fatal("the cold check did not re-read the replacement server record")
	}
	cancel()
	<-done
	var cut *leaseWaitCut
	if !errors.As(result, &cut) || errors.Unwrap(cut) != context.Canceled {
		t.Fatal("the second cold read lost its cancellation cut")
	}
}

func TestLeaseWaitRecoverPostgresSecondRead(t *testing.T) {
	c, _ := unitCell(t)
	pidfile := c.path("pg", "postmaster.pid")
	staged := pidfile + ".next"
	if leaseRecordPlant(t, c, "postgres_record") != nil {
		t.Fatal("cannot plant stale postgres ownership records")
	}
	data, err := os.ReadFile(pidfile)
	if err != nil || os.WriteFile(staged, data, 0o600) != nil {
		t.Fatal("cannot stage the replacement pidfile")
	}
	recordPath := c.path("run", "postgres.json")
	before, err := os.Lstat(recordPath)
	if err != nil {
		t.Fatal("cannot inspect the original postgres record")
	}
	recordBytes := retainedRead(t, c, "run/postgres.json")
	record := leaseTaken(t, leaseHolder(t, "kept", unix.F_WRLCK), recordPath)
	replacement := leaseTaken(t, leaseHolder(t, "kept", unix.F_WRLCK), staged)
	leaseWait.ceiling = 2 * time.Second

	// SIGIO wakes the barrier. F_GETLEASE identifies the inode whose open
	// caused the break, even if signals coalesce or arrive from another file.
	breaks := make(chan os.Signal, 1)
	signal.Notify(breaks, unix.SIGIO)
	defer signal.Stop(breaks)
	waitBreak := func(l *writerLease, missing string) {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			var held int
			var err error
			if !l.do(func(fd uintptr) { held, err = unix.FcntlInt(fd, unix.F_GETLEASE, 0) }) || err != nil {
				t.Fatal("cannot inspect the held lease")
			}
			if held != unix.F_WRLCK {
				return
			}
			select {
			case <-breaks:
			case <-deadline.C:
				t.Fatal(missing)
			}
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var result error
	go func() {
		result = c.recoverPostgresRecord(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		record.release()
		replacement.release()
		<-done
	}()

	// The postgres record is opened after the first pidfile read. Keep it
	// blocked until the replacement is in place, then await the second open.
	waitBreak(record, "recovery did not reach the postgres record after the first pidfile read")
	if os.Rename(staged, pidfile) != nil {
		t.Fatal("cannot replace the pidfile between recovery reads")
	}
	record.release()
	waitBreak(replacement, "recovery did not re-read the replacement pidfile")
	cancel()
	<-done
	var cut *leaseWaitCut
	if !errors.As(result, &cut) || errors.Unwrap(cut) != context.Canceled {
		t.Fatal("the recovery pidfile re-read lost its cancellation cut")
	}
	after, err := os.Lstat(recordPath)
	if err != nil || !os.SameFile(before, after) || !bytes.Equal(recordBytes, retainedRead(t, c, "run/postgres.json")) {
		t.Fatal("the interrupted re-read changed the postgres record")
	}
}
