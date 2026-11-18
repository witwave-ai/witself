//go:build linux

package localcell

import (
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
	f, err := openRegular(name, errWriterFailed, errWriterIrregular, errWriterFailed)
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
				if !l.broken() || holder == "lets_go" && err != nil || holder != "lets_go" && (err != errWriterFailed || time.Since(started) < leaseKeptAfter) {
					t.Fatal("the shared open did not wait out a lease that ends, or refuse one kept past the ceiling")
				}
			})
		}
	}
}

// A lease never let go is waited out only while a regular file stands at the
// name: 200 ms in, the leased file moved aside and a FIFO (a reader held), a
// link or a directory put there, the next attempt gives the irregular text
// within 150 ms of the swap, nothing written to the FIFO, the target unchanged.
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
				plant := map[string]func() error{"fifo": func() (err error) { fifo, _, err = writerFIFO(name, false); return err },
					"link": func() error { return os.Symlink(target, name) }, "directory": func() error { return os.Mkdir(name, 0o700) }}[object]
				time.AfterFunc(200*time.Millisecond, func() {
					moved := os.Rename(name, name+".aside")
					swapped <- map[bool]time.Time{true: time.Now()}[moved == nil && plant() == nil]
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
// after lease-break-time (45 seconds by default), within the 60 second
// ceiling, and the journal append waiting on it then succeeds.
func TestLeaseWaitKernelBreak(t *testing.T) {
	data, err := os.ReadFile("/proc/sys/fs/lease-break-time")
	if seconds, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); err != nil || parseErr != nil || seconds > 55 {
		t.Fatal("the kernel lease break time is unreadable or above 55 seconds, so the ceiling cannot be proven against it")
	}
	c, l := readerCell(t), &writerLease{lease: unix.F_RDLCK}
	t.Cleanup(l.close)
	size, started := writerSize(readerJournalPath(c)), time.Now()
	leaseTaken(t, l, readerJournalPath(c))
	// No ten-second watchdog here: the ceiling itself bounds this call.
	if err, elapsed := c.journal("stop", "operation", "started"), time.Since(started); err != nil || elapsed < 40*time.Second || elapsed > 60*time.Second || writerSize(readerJournalPath(c)) <= size || !l.broken() {
		t.Fatal("the journal append did not succeed when the kernel broke the lease within the ceiling")
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
		})
	}
}

// leaseSupervisor loads a supervisor with the real start and stop hooks.
func leaseSupervisor(t *testing.T, c *Cell) *supervisor {
	d := prepareSupervisorTest(t, c)
	h, _, _, _, _ := supervisorTestHooks()
	h.start, h.stop = c.Start, c.stopServiceChildren
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if s.load(d) != nil {
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
			if c.journal("stop", "operation", "started") != nil || c.journal("stop", "operation", "done") != nil {
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
