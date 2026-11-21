package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
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
