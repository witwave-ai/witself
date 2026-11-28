//go:build linux

package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// leaseSide: the shared reader under a write lease and the shared writer under a
// read lease, through their checked callers, both with the writer rows' texts.
var leaseSide = map[string]struct {
	lease int
	call  func(string) error
}{"reader": {unix.F_WRLCK, func(name string) error {
	f, err := openRegular(context.Background(), name, errWriterFailed, errWriterIrregular, errWriterFailed)
	_ = f.Close()
	return err
}}, "writer": {unix.F_RDLCK, writerOpenRow}}

// leaseTaken takes l on name, failing the row (never a skip) when refused.
func leaseTaken(t *testing.T, l *writerLease, name string) *writerLease {
	if l.take(name); !l.do(func(uintptr) {}) {
		t.Fatal("this kernel or sandbox refused the lease the row needs")
	}
	return l
}

// The shared opens wait out a lease on a regular file: a holder that lets go at
// the break costs a moment and the caller gets its handle; one that keeps the
// lease or takes it again at once is refused with the caller's failed text when
// the wait ends (ceiling 300 ms here).
func TestLeaseWaitHelper(t *testing.T) {
	for _, side := range []string{"reader", "writer"} {
		for _, holder := range []string{"lets_go", "kept", "retaken"} {
			t.Run(side+"/"+holder, func(t *testing.T) {
				name := filepath.Join(t.TempDir(), "leased")
				if os.WriteFile(name, []byte(readerJournalLine), 0o600) != nil {
					t.Fatal("cannot prepare the leased helper file")
				}
				l, started := leaseTaken(t, leaseHolder(t, holder, leaseSide[side].lease), name), time.Now()
				err := readerWatchdog(t, func() error { return leaseSide[side].call(name) }, l.release)
				elapsed := time.Since(started)
				if !l.broken() {
					t.Error("the holder never saw the shared open break its lease")
				}
				if holder == "lets_go" && err != nil {
					t.Error("the shared open did not wait out a lease whose holder let go at the break")
				}
				if holder != "lets_go" && err != errWriterFailed {
					t.Errorf("the shared open did not refuse a lease kept or taken again with the failed text: no error %t after %v", err == nil, elapsed.Round(10*time.Microsecond))
				}
				if holder != "lets_go" && elapsed < leaseKeptAfter {
					t.Error("the shared open returned on a lease kept or taken again sooner than leaseKeptAfter")
				}
			})
		}
	}
}

// A lease never let go is waited out only while a regular file stands at the
// name: 200 ms in, the leased file exchanged atomically (RENAME_EXCHANGE) with
// a FIFO (a reader held), a link or a directory planted beside it, the next
// attempt gives the irregular text within 150 ms of the exchange, nothing
// written to the FIFO, the target unchanged.
func TestLeaseWaitSwap(t *testing.T) {
	for _, side := range []string{"reader", "writer"} {
		for _, object := range []string{"fifo", "link", "directory"} {
			t.Run(side+"/"+object, func(t *testing.T) {
				dir, l := t.TempDir(), &writerLease{lease: leaseSide[side].lease}
				name, target, swapped, fifo := filepath.Join(dir, "leased"), filepath.Join(dir, "target"), make(chan time.Time, 1), (*os.File)(nil)
				t.Cleanup(l.close)
				if os.WriteFile(name, []byte(readerJournalLine), 0o600) != nil || os.WriteFile(target, []byte(readerJournalLine), 0o600) != nil {
					t.Fatal("cannot prepare the swap row files")
				}
				leaseTaken(t, l, name)
				planted := name + ".new"
				plant := map[string]func() error{"fifo": func() (err error) { fifo, _, err = writerFIFO(planted, false); return err },
					"link": func() error { return os.Symlink(target, planted) }, "directory": func() error { return os.Mkdir(planted, 0o700) }}[object]
				if plant() != nil {
					t.Fatal("cannot plant the object the swap row exchanges")
				}
				time.AfterFunc(200*time.Millisecond, func() {
					exchanged := unix.Renameat2(unix.AT_FDCWD, planted, unix.AT_FDCWD, name, unix.RENAME_EXCHANGE)
					swapped <- map[bool]time.Time{true: time.Now()}[exchanged == nil]
				})
				err := readerWatchdog(t, func() error { return leaseSide[side].call(name) }, l.release)
				returned, at := time.Now(), <-swapped
				if fifo != nil {
					defer func() { _ = fifo.Close() }()
				}
				data, readErr := os.ReadFile(target)
				if at.IsZero() || err != errWriterIrregular || returned.Sub(at) > 150*time.Millisecond || writerDrained(fifo) != 0 || readErr != nil || string(data) != readerJournalLine {
					t.Fatal("an object swapped in during a lease wait was not refused as irregular at the next attempt with nothing written")
				}
			})
		}
	}
}

// The real bound (OQ10): a read lease never let go is broken by the kernel
// after the lease-break-time it reads (45 seconds by default), within the 60
// second ceiling, and the journal append waiting on it then succeeds within 5
// seconds of that time; a time of 0 or less never breaks (fs/locks.c), so the
// append is refused at the ceiling with the open's text, nothing appended.
func TestLeaseWaitKernelBreak(t *testing.T) {
	data, err := os.ReadFile("/proc/sys/fs/lease-break-time")
	seconds, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || parseErr != nil || seconds > 55 {
		t.Fatal("the kernel lease break time is unreadable or above 55 seconds, so the ceiling cannot be proven against it")
	}
	c, l := readerCell(t), &writerLease{lease: unix.F_RDLCK}
	t.Cleanup(l.close)
	size, started := writerSize(readerJournalPath(c)), time.Now()
	leaseTaken(t, l, readerJournalPath(c))
	// No ten-second watchdog here: the ceiling itself bounds this call.
	err, elapsed := c.journal(context.Background(), "stop", "operation", "started"), time.Since(started)
	broken, low, high := seconds > 0, time.Duration(seconds-5)*time.Second, min(time.Duration(seconds+5)*time.Second, leaseWait.ceiling)
	if !broken {
		low, high = leaseWait.ceiling-leaseWait.longest, leaseWait.ceiling+5*time.Second
	}
	if (err == nil) != broken || !broken && err.Error() != "cannot open local cell journal" || elapsed < low || elapsed > high || (writerSize(readerJournalPath(c)) > size) != broken || !l.broken() {
		t.Fatal("the journal append did not end as the kernel break time it read requires, within the ceiling")
	}
}

// A read lease on a lock file is waited out too (OQ2): with a holder that lets
// go at the break, Inspect and then Stop each go on and succeed.
func TestLeaseWaitLockFile(t *testing.T) {
	for name, lock := range map[string]string{"owner": ".owner.lock", "operation": "run/operation.lock"} {
		t.Run(name, func(t *testing.T) {
			c, _ := unitCell(t)
			if c.Stop(t.Context()) != nil {
				t.Fatal("cannot prepare the lock files")
			}
			for _, call := range []func() error{func() error { _, err := c.Inspect(t.Context()); return err }, func() error { return c.Stop(t.Context()) }} {
				l := leaseTaken(t, leaseHolder(t, "lets_go", unix.F_RDLCK), c.path(filepath.FromSlash(lock)))
				if err := readerWatchdog(t, call, l.release); err != nil || !l.broken() {
					t.Fatal("a lock file lease whose holder let go at the break was not waited out")
				}
				l.close()
			}
			// G1: a write lease never let go (300 ms ceiling) gives the fixed lock refusal.
			t.Run("kept", func(t *testing.T) {
				l, started := leaseTaken(t, leaseHolder(t, "kept", unix.F_WRLCK), c.path(filepath.FromSlash(lock))), time.Now()
				err := readerWatchdog(t, func() error { _, err := c.Inspect(t.Context()); return err }, l.release)
				if err == nil || err.Error() != "localcell: open "+name+" lock: resource temporarily unavailable" || !l.broken() || time.Since(started) < leaseKeptAfter {
					t.Fatal("a lock file lease kept past the ceiling did not give the fixed lock refusal when the wait ended")
				}
			})
		})
	}
}

// leaseSupervisor loads a supervisor with the real start and stop hooks.
func leaseSupervisor(t *testing.T, c *Cell) *supervisor {
	d := prepareSupervisorTest(t, c)
	h, _, _, _, _ := supervisorTestHooks()
	h.start, h.stop = c.Start, c.stopServiceChildren
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if s.load(t.Context(), d) != nil {
		t.Fatal("cannot load the lease row supervisor")
	}
	return s
}

// leaseStep runs one step while a holder that lets go at the break leases name.
func leaseStep(t *testing.T, s *supervisor, name string, lease int) {
	l := leaseTaken(t, leaseHolder(t, "lets_go", lease), name)
	err := readerWatchdog(t, func() error { return s.step(t.Context()) }, l.release)
	d, descriptorErr := s.cell.ServiceDescriptor()
	if err != nil || !l.broken() || s.status.State != "running" || len(s.record.Failures) != 0 || descriptorErr != nil || d.Faulted {
		t.Fatal("a lease met by one supervisor step was not waited out to a running service with no failure")
	}
}

// A lease on the journal across one supervisor step with the real start and
// stop hooks (synthetic component operations) is waited out: the service runs,
// no failure is counted and the descriptor is not faulted.
func TestLeaseWaitSupervisorTick(t *testing.T) {
	for name, lease := range map[string]int{"read": unix.F_RDLCK, "write": unix.F_WRLCK} {
		t.Run(name, func(t *testing.T) {
			c := newAdmissionFixture(t, "StartLegacy").cell
			s := leaseSupervisor(t, c)
			if c.journal(context.Background(), "stop", "operation", "started") != nil || c.journal(context.Background(), "stop", "operation", "done") != nil {
				t.Fatal("cannot prepare the closed journal the supervisor row leases")
			}
			leaseStep(t, s, readerJournalPath(c), lease)
		})
	}
}

// A read lease on the server log, met by the first step's real Start of a
// hermetic cell (synthetic binaries), is waited out: the step runs the service.
func TestLeaseWaitSupervisorLog(t *testing.T) {
	dir := t.TempDir()
	build := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=false", "-o", filepath.Join(dir, "fixture"), "./testdata/lifecyclefixture")
	if build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local"); build.Run() != nil {
		t.Fatal("cannot build the synthetic lifecycle fixture")
	}
	for _, name := range []string{"postgres", "initdb", "pg_ctl", "createdb", "psql", "witself-server", "witself-worker"} {
		if os.Link(filepath.Join(dir, "fixture"), filepath.Join(dir, name)) != nil {
			t.Fatal("cannot prepare a synthetic lifecycle tool")
		}
	}
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	binaries := Binaries{PostgresDir: dir, Server: filepath.Join(dir, "witself-server"), Worker: filepath.Join(dir, "witself-worker")}
	c, err := New(Config{Root: testPrivateRoot(t), Binaries: binaries, Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal("cannot construct the hermetic lease row cell")
	}
	t.Cleanup(func() {
		if ctx := context.Background(); errors.Join(c.stopServiceChildren(ctx), c.Repair(ctx), c.Remove(ctx, RemoveOptions{Purge: true})) != nil {
			t.Error("cannot stop and remove the hermetic lease row cell")
		}
	})
	if c.Install(t.Context(), InstallOptions{}) != nil {
		t.Fatal("cannot install the hermetic lease row cell")
	}
	s := leaseSupervisor(t, c)
	if os.WriteFile(c.path("logs", "server.log"), nil, 0o600) != nil {
		t.Fatal("cannot prepare the leased server log")
	}
	leaseStep(t, s, c.path("logs", "server.log"), unix.F_RDLCK)
}

// While a command waits on a lease never let go (ceiling 1 s here), a second
// Inspect waits too only when it meets the same lease first (P1): on
// .owner.lock, waited before any lock is taken, or a write lease on the
// journal; on run/operation.lock (waited holding .owner.lock's lock) or a read
// lease on the journal it reports Locked at once, the first still waiting.
func TestLeaseWaitSecondCommand(t *testing.T) {
	for _, row := range []struct {
		name, file string
		lease      int
		waits      bool
	}{{"owner_lock", ".owner.lock", unix.F_RDLCK, true}, {"operation_lock", "run/operation.lock", unix.F_RDLCK, false},
		{"journal_write", "journal/operations.jsonl", unix.F_WRLCK, true}, {"journal_read", "journal/operations.jsonl", unix.F_RDLCK, false}} {
		t.Run(row.name, func(t *testing.T) {
			c, _ := unitCell(t)
			second, err := New(Config{Root: c.cfg.Root, Timeout: time.Second})
			saved, l, first := leaseWait, &writerLease{lease: row.lease}, make(chan error, 1)
			t.Cleanup(func() { leaseWait = saved; l.close() })
			if leaseWait.ceiling = time.Second; err != nil || c.Stop(t.Context()) != nil {
				t.Fatal("cannot prepare the lock files and the journal")
			}
			leaseTaken(t, l, c.path(filepath.FromSlash(row.file)))
			go func() { first <- c.Stop(t.Context()) }()
			for deadline := time.Now().Add(5 * time.Second); !l.broken() && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
			var inspection Inspection
			began := time.Now()
			err = readerWatchdog(t, func() (err error) { inspection, err = second.Inspect(t.Context()); return err }, l.release)
			took, waiting := time.Since(began), len(first) == 0
			if firstErr := readerWatchdog(t, func() error { return <-first }, l.release); !l.broken() || firstErr == nil ||
				row.waits && (err == nil || took < leaseKeptAfter) || !row.waits && (err != nil || !inspection.Locked || !waiting || took >= leaseKeptAfter) {
				t.Fatalf("the second command waited %t and reported the cell locked %t", took >= leaseKeptAfter, inspection.Locked)
			}
		})
	}
}

// Repair binds its truncate and retained sync to the journal it read (P3):
// under a read lease never let go (ceiling 2 s here), 100 ms after the first
// break the journal is exchanged atomically for a regular file longer or
// shorter than its durable prefix, and each refuses with its open text at the
// next attempt: that file byte-identical and unsynced, the moved journal
// unchanged, the cleanup marker kept. Unleased and unswapped, Repair succeeds.
func TestLeaseWaitRepairSwap(t *testing.T) {
	for _, site := range []string{"torn", "retained"} {
		for _, size := range []string{"longer", "shorter", "plain"} {
			t.Run(site+"/"+size, func(t *testing.T) {
				saved, savedSync, l, syncs, swapped := leaseWait, syncFile, &writerLease{lease: unix.F_RDLCK}, 0, make(chan time.Time, 1)
				t.Cleanup(func() { leaseWait, syncFile = saved, savedSync; l.close() })
				var c *Cell
				if site == "torn" {
					if c, _ = unitCell(t); c.journal(context.Background(), "stop", "operation", "started") != nil || c.journal(context.Background(), "stop", "operation", "done") != nil {
						t.Fatal("cannot prepare the closed journal")
					}
				} else {
					c = writerRetainedCleanup(t)
				}
				name, want := readerJournalPath(c), map[string]string{"torn": "cannot open torn local cell journal", "retained": "cannot open local cell journal"}[site]
				durable, err := os.ReadFile(name)
				if site == "torn" && err == nil {
					err = testWriteFile(name, append(bytes.Clone(durable), `{"operation":"stop","st`...), 0o600)
				}
				before, readErr := os.ReadFile(name)
				bystander := bytes.Repeat([]byte("b"), map[string]int{"longer": len(before) + 64, "shorter": 3, "plain": 3}[size])
				if err != nil || readErr != nil || testWriteFile(name+".bystander", bystander, 0o600) != nil {
					t.Fatal("cannot prepare the journal and its bystander")
				}
				leaseWait.ceiling, syncFile = 2*time.Second, func(f *os.File) error {
					syncs += map[bool]int{true: 1}[filepath.Base(f.Name()) == "operations.jsonl"]
					return savedSync(f)
				}
				prefixSyncs := 0
				if site == "retained" {
					c.cfg.Failpoint = func(op, step string) error {
						if op == "remove" && step == "cleanup_journal" {
							prefixSyncs, syncs = syncs, 0
							if size != "plain" {
								l.take(name)
							}
						}
						return nil
					}
				}
				if size == "plain" {
					swapped <- time.Time{}
				} else if site == "torn" {
					leaseTaken(t, l, name)
				}
				if size != "plain" {
					go func() {
						for deadline := time.Now().Add(5 * time.Second); !l.broken() && time.Now().Before(deadline); {
							time.Sleep(time.Millisecond)
						}
						time.Sleep(100 * time.Millisecond)
						exchanged := unix.Renameat2(unix.AT_FDCWD, name+".bystander", unix.AT_FDCWD, name, unix.RENAME_EXCHANGE)
						swapped <- map[bool]time.Time{true: time.Now()}[exchanged == nil && l.broken()]
					}()
				}
				err = readerWatchdog(t, func() error { return c.Repair(t.Context()) }, l.release)
				returned, at := time.Now(), <-swapped
				m, manifestErr := readManifest(t.Context(), c.cfg.Root)
				if site == "retained" && checkpointJournalBarrierSupported && prefixSyncs != 1 {
					t.Fatal("retained Repair did not sync the prefix before the lease boundary")
				}
				if size == "plain" {
					if err != nil || syncs != 1 || manifestErr != nil || m.Cleanup != nil || site == "torn" && writerSize(name) != int64(len(durable)) {
						t.Fatal("repair without a lease or a swap did not succeed as before")
					}
					return
				}
				if !l.do(func(uintptr) {}) {
					t.Fatal("this kernel or sandbox refused the lease the row needs")
				}
				now, nowErr := os.ReadFile(name)
				moved, movedErr := os.ReadFile(name + ".bystander")
				kept := nowErr == nil && bytes.Equal(now, bystander) && movedErr == nil && bytes.Equal(moved, before) && syncs == 0
				if at.IsZero() || err == nil || err.Error() != want || returned.Sub(at) > time.Second || !kept || manifestErr != nil || site == "retained" && m.Cleanup == nil {
					t.Fatalf("the swapped journal site refused with its open text %t, files kept unsynced %t, marker kept %t", err != nil && err.Error() == want, kept, manifestErr == nil && m.Cleanup != nil)
				}
			})
		}
	}
}
