//go:build linux

package localcell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// bindSite is a described cell whose operation reaches one of the three
// bindings after a child started (LC-LEASE-2E6): alive, Start's postgres stage
// with a PostgreSQL already live; launch, Start's real server launch; retained,
// Reinstall's real retained PostgreSQL. Its children are test-owned sleeps (a
// synthetic pg_ctl starts an unused one and stops it by its pidfile; alive's
// first is live from the start) or launched by the cell from a synthetic
// server script, all below the test's own temporary directory.
type bindSite struct {
	cell                       *Cell
	operation, step, component string
	run                        func(context.Context) error
	kids                       []int
	exited                     map[int]<-chan error
	used                       string
}

const bindPgCtl = `#!/bin/sh
case "$1" in
start) for pid in %[1]s; do [ -e '%[2]s/'$pid ] && continue; : > '%[2]s/'$pid; printf '%%s\n%%s\n1\n' $pid "$3" > "$3/postmaster.pid"; exit 0; done; exit 1 ;;
stop) pid=$(command -p head -n 1 "$3/postmaster.pid") && kill $pid && while kill -0 $pid 2>/dev/null; do command -p sleep 0.01; done && command -p rm "$3/postmaster.pid"%[3]s ;;
*) exit 1 ;;
esac
`

// newBindSite builds site; with swap, pg_ctl's first stop leaves the journal a
// directory.
func newBindSite(t *testing.T, site string, swap bool) *bindSite {
	t.Helper()
	t.Setenv("WITSELF_RUNTIME", "local") // the binding does nothing otherwise
	s := &bindSite{operation: "start", step: "postgres", component: "postgres", exited: map[int]<-chan error{}, used: t.TempDir()}
	for range map[string]int{"alive": 2, "retained": 2}[site] {
		child := exec.Command("sleep", "60")
		child.Env = []string{}
		requireServiceLease(t, child.Start() == nil, "cannot start a bounded child")
		s.kids = append(s.kids, child.Process.Pid)
		s.exited[child.Process.Pid] = StartJoinedTestRunForTest(t, func() { _ = child.Process.Kill() }, 10*time.Second, child.Wait)
	}
	var c *Cell
	if site == "retained" {
		f, r := reinstallFixture(t, "legacy", true)
		query := func(ctx context.Context, m *Manifest, q string) (string, error) {
			if err := ctx.Err(); err != nil { // as psql, which never starts on an ended context
				return "", err
			}
			return r.query(ctx, m, q)
		}
		c, s.operation, s.run = f.cell, "reinstall", func(ctx context.Context) error { return f.cell.Reinstall(ctx, reinstallOptions(f)) }
		c.reinstallTest = &reinstallOperations{postgres: c.startRetainedPostgres, query: query, stop: c.stopAll}
	} else {
		c, _ = descriptorTestCell(t)
		requireServiceLease(t, c.makePassword() == nil && privateDir(c.path("pg"), true) == nil, "cannot prepare the start fixture")
		ops, quiet := c.principalStartOperations(), func(context.Context, *Manifest) error { return nil }
		launch := ops.launch
		ops.verify, ops.account = quiet, quiet
		if site == "launch" || site == "worker" {
			ops.postgres = quiet
		}
		ops.ready = func(context.Context, *Manifest, string, bool) error { return nil }
		ops.launch = func(ctx context.Context, m *Manifest, component string, prepare func() error) error {
			if site != "worker" && (site != "launch" || component != "server") {
				return nil
			}
			return launch(ctx, m, component, prepare)
		}
		c.installerStart, s.run = &ops, c.Start
	}
	s.cell, c.cfg.Timeout, c.cfg.Binaries.PostgresDir = c, 10*time.Second, t.TempDir()
	obstacle := ""
	if swap {
		obstacle = fmt.Sprintf(" && { [ -e '%[1]s/swapped' ] || { : > '%[1]s/swapped' && command -p mv journal/operations.jsonl journal/saved && command -p mkdir journal/operations.jsonl; }; }", s.used)
	}
	script := fmt.Sprintf(bindPgCtl, strings.Trim(fmt.Sprint(s.kids), "[]"), s.used, obstacle)
	requireServiceLease(t, os.WriteFile(filepath.Join(c.cfg.Binaries.PostgresDir, "pg_ctl"), []byte(script), 0o700) == nil, "cannot prepare synthetic pg_ctl")
	switch site {
	case "alive":
		identity, alive, err := processIdentity(s.kids[0])
		requireServiceLease(t, err == nil && alive, "cannot identify the live PostgreSQL")
		requireServiceLease(t, writeAtomicBytes(c.path("pg", "postmaster.pid"), fmt.Appendf(nil, "%d\n%s\n1\n", s.kids[0], c.path("pg")), nil) == nil &&
			writeAtomic(c.path("run", "postgres.json"), ProcessRecord{PID: s.kids[0], StartTime: identity, Root: c.cfg.Root, Component: "postgres"}, nil) == nil &&
			os.WriteFile(filepath.Join(s.used, fmt.Sprint(s.kids[0])), nil, 0o600) == nil, "cannot publish the live PostgreSQL")
	case "launch", "worker": // worker: Start's real worker launch after its real server launch
		sleep, err := exec.LookPath("sleep")
		s.step, c.cfg.Binaries.Server = map[string]string{"launch": "server", "worker": "worker"}[site], filepath.Join(t.TempDir(), "server")
		s.component, c.cfg.Binaries.Worker = s.step, c.cfg.Binaries.Server
		requireServiceLease(t, err == nil && os.WriteFile(c.cfg.Binaries.Server, []byte("#!/bin/sh\nexec "+sleep+" 60\n"), 0o700) == nil, "cannot prepare the synthetic server")
		t.Cleanup(func() { // registered before any launch: the newest, should a row leave it
			for _, component := range []string{"server", "worker"} {
				var r ProcessRecord
				if readJSON(context.Background(), c.path("run", component+".json"), &r) == nil {
					_ = cleanupEnvironmentFixture(r, 5*time.Second, processIdentity, terminateEnvironmentFixture)
				}
			}
		})
	}
	if site != "retained" {
		m, err := readManifest(t.Context(), c.cfg.Root)
		for _, component := range []string{"postgres", "server", "worker"} {
			if err == nil {
				m.Ports, err = c.allocateAvailablePorts(m.Ports, component)
			}
		}
		requireServiceLease(t, err == nil && writeAtomic(c.path("manifest.json"), m, nil) == nil, "cannot allocate the start ports")
	}
	return s
}

// bindCut runs the site's operation and pauses it inside the journal sync of
// its step's started record, after every earlier read of the binding's files.
// There it leases the binding's read: the descriptor, or the manifest or the
// process record behind a descriptor holder that lets go at its break. At the
// target's break the kind acts: kept (cancelled; kept to the operation's
// return), released (cancelled; let go 50 ms later), live (kept to the
// ceiling, never cancelled), deadline (kept past the step's own deadline to the
// ceiling), deadline_release (let go after the step's own deadline, before the
// ceiling, never cancelled), ordinary (the file replaced by one the read
// refuses), finish (as kept) or done (the record, see bindDone).
// none takes no lease. canceled holds none of the binding's files and cancels
// before its first read: alive at the break of a lease on the pidfile its
// stage reads first, let go at once; launch at the pause; retained at the
// record's publication. waited runs from the break to the step's failure
// record (deadline_release: to operation:done); -1 is a missed done window.
func bindCut(t *testing.T, s *bindSite, read, kind string) (time.Duration, error) {
	t.Helper()
	c := s.cell
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first, second := leaseHolder(t, "kept", unix.F_WRLCK), leaseHolder(t, "kept", unix.F_WRLCK)
	if strings.HasPrefix(kind, "deadline") {
		c.cfg.Timeout = time.Second
		leaseWait.ceiling = 4 * c.cfg.Timeout
	} else if kind != "live" {
		leaseWait.ceiling = 2 * time.Second
	}
	paused, resume, journal := make(chan struct{}), make(chan struct{}), readerJournalPath(c)
	var failed time.Time
	saved, armed, ended := syncFile, false, false
	t.Cleanup(func() { syncFile = saved })
	syncFile = func(f *os.File) error {
		err := saved(f)
		if f.Name() != journal {
			if armed && !ended && kind == "canceled" && s.operation == "reinstall" && filepath.Dir(f.Name()) == c.path("run") {
				ended = true
				cancel()
			}
			return err
		}
		records := leaseJournalRecords(c)
		switch last := records[len(records)-1]; {
		case last == s.step+":started" && !armed:
			armed = true
			if kind == "canceled" && s.step == "server" {
				cancel()
			} else if kind != "none" && (kind != "canceled" || s.operation == "start") {
				close(paused)
				<-resume
			}
		case last == s.step+":failed", kind == "deadline_release" && last == "operation:done":
			failed = time.Now()
		}
		return err
	}
	done := StartJoinedTestRunForTest(t, cancel, time.Minute, func() error { return s.run(ctx) })
	if kind == "none" || (kind == "canceled" && (s.step == "server" || s.operation == "reinstall")) {
		return 0, <-done
	}
	select {
	case <-paused:
	case <-done:
		t.Fatal("the operation ended before it reached its step")
	}
	if kind == "canceled" {
		leaseTaken(t, first, c.path("pg", "postmaster.pid"))
		close(resume)
		_, ok := leaseContextBroken(first)
		requireServiceLease(t, ok, "the stage never read its pidfile")
		cancel()
		first.release()
		return 0, <-done
	}
	descriptor := c.path("service", "descriptor.json")
	original, err := os.ReadFile(descriptor)
	requireServiceLease(t, err == nil, "cannot snapshot the descriptor")
	leaseTaken(t, first, descriptor)
	close(resume)
	if kind == "done" && !bindDone(first, second, c.path("run", s.component+".json"), cancel) {
		cancel()
		<-done
		return -1, nil
	}
	_, ok := leaseContextBroken(first)
	requireServiceLease(t, ok, "the binding never reached its descriptor")
	target, path := first, map[string]string{"descriptor": descriptor, "manifest": c.path("manifest.json"), "record": c.path("run", s.component+".json")}[read]
	if read != "descriptor" && kind != "ordinary" && kind != "done" {
		target = leaseTaken(t, second, path)
		first.close()
		_, ok = leaseContextBroken(second)
		requireServiceLease(t, ok, "the binding never reached its leased "+read)
	}
	broke := time.Now()
	switch kind {
	case "ordinary":
		bindRefused(t, c, read, path)
		first.close()
	case "kept", "finish":
		cancel()
	case "released":
		cancel()
		time.Sleep(50 * time.Millisecond)
		target.release()
	case "deadline_release":
		time.Sleep(c.cfg.Timeout + c.cfg.Timeout/4)
		target.release()
	}
	select {
	case err = <-done:
	case <-time.After(time.Minute):
		t.Fatal("the operation did not return")
	}
	first.close()
	second.close()
	requireServiceLease(t, writeAtomicBytes(descriptor, original, nil) == nil, "cannot restore the descriptor")
	if failed.IsZero() {
		return time.Hour, err // the step never failed
	}
	return failed.Sub(broke), err
}

// bindDone (m2) takes second, retried until held, on the record the launch
// helper has just published, so that the launch's last ownership poll waits on
// it (on context.Background()) while the binding's descriptor (first) is still
// unread; then it cancels and lets the record go: the poll returns into the
// binding on a done context. False: the window was missed.
func bindDone(first, second *writerLease, record string, cancel context.CancelFunc) bool {
	for end := time.Now().Add(10 * time.Second); !second.do(func(uintptr) {}) && !first.broken() && time.Now().Before(end); runtime.Gosched() {
		second.take(record)
	}
	if !second.do(func(uintptr) {}) || first.broken() {
		return false
	}
	if _, ok := leaseContextBroken(second); !ok || first.broken() {
		return false
	}
	cancel()
	second.release()
	return true
}

// bindRefused replaces path by one the binding's read refuses for an ordinary
// reason: an empty descriptor, a foreign installation, a foreign boot.
func bindRefused(t *testing.T, c *Cell, read, path string) {
	t.Helper()
	switch read {
	case "descriptor":
		requireServiceLease(t, writeAtomicBytes(path, []byte("{}"), nil) == nil, "cannot replace the descriptor")
	case "manifest":
		m, err := readManifest(t.Context(), c.cfg.Root)
		requireServiceLease(t, err == nil, "cannot read the manifest")
		m.InstallationID = "lc_foreign"
		requireServiceLease(t, writeAtomic(path, m, nil) == nil, "cannot replace the manifest")
	default:
		var r ProcessRecord
		requireServiceLease(t, readJSON(t.Context(), path, &r) == nil, "cannot read the process record")
		r.BootID = "linux:foreign-boot"
		requireServiceLease(t, writeAtomic(path, r, nil) == nil, "cannot replace the process record")
	}
}

// After a cut at the binding the step fails, and what follows is what follows
// any failure of that step at the parent: the rollback stops what the
// operation launched (launch: the server; retained: the proof's PostgreSQL)
// and never what was alive before it (alive: PostgreSQL), then the finish; the
// record stays unbound. A record lease kept through the rollback leaves it
// incomplete (its verifyAll waits the lease out), as at the parent, and Repair
// closes it. Then the next operation (Start, Reinstall) binds and Stop stops
// the tree. The kept leaves' state equals the parent's at the same lease (and
// the ordinary leaves', the parent's ordinary binding failure): there the
// timing check, apart from the state checks and proportional to the ceiling,
// is the proof that the binding took the operation's context. The released
// leaves also show that the parent's late write (the bound record) does not
// happen. launch/record/done (m2) cancels during the launch's last ownership
// poll: the binding meets a done context, makes its one attempt and cuts. The
// binding ends with the operation, never at the step's own deadline
// (LC-LEASE-2E6-F1, B1): with the operation live a lease kept past it to the
// ceiling gets the parent's refusal (deadline), and a holder that lets go
// between the two (deadline_release, at each site and the worker launch) lets
// the operation complete and bind, as at the parent; the timing checks apart.
// Regression pins, passing on the parent's production code too: the 31 none,
// canceled, live, ordinary, deadline and deadline_release leaves.
func TestBindCutAfterLaunch(t *testing.T) {
	for _, site := range []string{"alive", "launch", "retained", "worker"} {
		leaves := []string{"none", "canceled", "descriptor/deadline", "descriptor/deadline_release"}
		for _, read := range []string{"descriptor", "manifest", "record"} {
			for _, kind := range []string{"kept", "released", "live", "ordinary"} {
				leaves = append(leaves, read+"/"+kind)
			}
		}
		switch site {
		case "retained":
			leaves = append(leaves, "descriptor/finish")
		case "launch":
			leaves = append(leaves, "record/done")
		case "worker":
			leaves = leaves[3:4]
		}
		for _, leaf := range leaves {
			t.Run(site+"/"+leaf, func(t *testing.T) {
				read, kind, split := strings.Cut(leaf, "/")
				if !split {
					kind = read
				}
				s := newBindSite(t, site, kind == "finish")
				c := s.cell
				before, err := readManifest(t.Context(), c.cfg.Root)
				requireServiceLease(t, err == nil, "cannot snapshot the manifest")
				prior, _ := os.ReadFile(c.path("run", s.component+".json"))
				prefix := len(leaseJournalRecords(c))
				waited, err := bindCut(t, s, read, kind)
				for attempt := 1; waited == -1; attempt++ { // a missed done window: a fresh fixture, bounded
					requireServiceLease(t, attempt < 20, "the launch's last ownership poll never waited on its record")
					s = newBindSite(t, site, false)
					c, prefix = s.cell, len(leaseJournalRecords(s.cell))
					before, err = readManifest(t.Context(), c.cfg.Root)
					requireServiceLease(t, err == nil, "cannot snapshot the manifest")
					waited, err = bindCut(t, s, read, kind)
				}
				boot, bootErr := serviceBootID()
				requireServiceLease(t, bootErr == nil, "current boot unavailable")
				var record ProcessRecord
				recordErr := readJSON(t.Context(), c.path("run", s.component+".json"), &record)
				cut := kind == "kept" || kind == "released" || kind == "done" || kind == "finish"
				held := read == "record" && (kind == "kept" || kind == "live") // the rollback's verifyAll waits on it
				fixed := map[string]string{"start": "local cell start: " + s.step + " failed", "reinstall": "local cell reinstall: postgres failed"}[s.operation]
				if held {
					fixed = map[string]string{"start": "local cell startup failed; rollback incomplete, repair required", "reinstall": "local cell reinstall failed; rollback incomplete, repair required"}[s.operation]
				} else if kind == "finish" {
					fixed = "local cell reinstall failed; rollback completion failed, repair required"
				}
				switch {
				case kind == "none":
					requireServiceLease(t, err == nil, "an unleased live operation failed")
				case kind == "deadline_release":
					requireServiceLease(t, err == nil, "a binding that outlasted its step's deadline failed the operation")
				case kind == "canceled":
					want := map[string]string{"alive": "local cell start: server failed", "launch": "local cell start: server failed", "retained": "local cell reinstall: verified failed: local_reinstall_identity_mismatch"}[site]
					requireServiceLease(t, err != nil && err.Error() == want, "a cancelled unleased operation changed its refusal")
					requireServiceLease(t, !serviceLeaseCut(err), "a cancelled unleased operation reported a lease cut")
				case cut:
					cause := context.Canceled
					requireServiceLease(t, serviceLeaseCut(err), "the binding lost its lease cut")
					requireServiceLease(t, errors.Is(err, cause), "the binding's cut lost its cause")
					requireServiceLease(t, err.Error() == fixed+": local cell lease wait ended: "+cause.Error(), "the operation changed its fixed cut receipt")
					for _, other := range []error{ErrServiceOwnershipConflict, ErrLocked, errMaintenanceFenceActive, os.ErrNotExist, unix.EWOULDBLOCK, context.DeadlineExceeded} {
						requireServiceLease(t, other == nil || !errors.Is(err, other), "the binding's cut carried an ordinary refusal or another cause")
					}
				default:
					requireServiceLease(t, err != nil && err.Error() == fixed, "the binding changed the parent's refusal")
					requireServiceLease(t, !serviceLeaseCut(err), "a refusal that is not a cut reported a lease cut")
				}
				// The timing proofs, apart from the state checks, proportional to the ceiling.
				if cut {
					requireServiceLease(t, waited < leaseWait.ceiling/2, "the binding waited past half the lease ceiling after its cut")
				} else if kind == "live" {
					requireServiceLease(t, waited >= leaseKeptAfter, "a live binding returned before the lease ceiling")
				} else if kind == "deadline" {
					requireServiceLease(t, waited >= leaseWait.ceiling/2, "a binding ended at its step's own deadline")
				} else if kind == "deadline_release" {
					requireServiceLease(t, waited >= c.cfg.Timeout && waited < leaseWait.ceiling, "the holder did not let go between the step's deadline and the ceiling")
				}
				if kind == "finish" {
					requireServiceLease(t, os.Remove(readerJournalPath(c)) == nil && os.Rename(c.path("journal", "saved"), readerJournalPath(c)) == nil, "cannot restore the journal")
				}
				progress, journalErr := c.readJournal(t.Context())
				requireServiceLease(t, journalErr == nil && !progress.tornTail, "the operation left an unreadable journal")
				if records := leaseJournalRecords(c)[prefix:]; kind == "deadline_release" { // completed: nothing failed, the record bound, nothing stopped
					requireServiceLease(t, progress.pending == nil && records[len(records)-1] == "operation:done" && !slices.ContainsFunc(records, func(r string) bool { return strings.HasSuffix(r, ":failed") }),
						"a binding that outlasted its step's deadline failed a step")
					if state, stateErr := c.processState(t.Context(), s.component); site != "retained" {
						requireServiceLease(t, recordErr == nil && record.BootID == boot, "a binding that outlasted its step's deadline did not bind")
						requireServiceLease(t, stateErr == nil && state.Alive && state.Matches, "a completed operation stopped its child")
					}
				} else if kind != "none" && kind != "canceled" {
					steps := []string{"operation:started", "postgres:started", "postgres:failed"}
					if site == "launch" {
						steps = []string{"operation:started", "postgres:started", "postgres:done", "server:started", "server:failed"}
					}
					if open := held || kind == "finish"; !open {
						steps = append(steps, "operation:done")
						requireServiceLease(t, progress.pending == nil, "a completed rollback left the operation open")
					} else {
						requireServiceLease(t, progress.pending != nil && progress.pending.Step == s.step, "a failed rollback or finish did not leave its step open")
					}
					requireServiceLease(t, slices.Equal(leaseJournalRecords(c)[prefix:], steps), "the failed binding wrote unexpected journal records")
					requireServiceLease(t, recordErr == nil, "the failed binding lost the process record")
					if kind != "ordinary" || read != "record" {
						requireServiceLease(t, record.BootID == "", "a failed binding bound the process record")
					}
					if now, _ := os.ReadFile(c.path("run", "postgres.json")); site == "alive" && (kind != "ordinary" || read != "record") {
						requireServiceLease(t, bytes.Equal(now, prior), "a failed binding changed the live PostgreSQL's record")
					}
					state, stateErr := c.processState(t.Context(), s.component)
					requireServiceLease(t, stateErr == nil, "cannot inspect the bound child")
					requireServiceLease(t, state.Alive == (site == "alive" || held), "the rollback left the wrong child running")
					requireServiceLease(t, state.Alive == state.Matches, "the rollback left a child of the wrong ownership")
					after, err := readManifest(t.Context(), c.cfg.Root)
					requireServiceLease(t, err == nil && after.Ports == before.Ports, "the failed binding changed the ports")
					requireServiceLease(t, after.RemovalRetained == (site == "retained"), "the failed binding changed the retained marker")
				} else if (kind == "canceled" && site != "launch") || kind == "none" {
					requireServiceLease(t, recordErr == nil && record.BootID == boot, "a done context's first attempt did not bind")
				}
				if kind == "canceled" && site == "launch" {
					requireServiceLease(t, errors.Is(recordErr, os.ErrNotExist), "a cancelled launch started a child")
				}
				// The next operation closes it: Repair for an open one, then a bound start and a stop.
				var launched []ProcessRecord
				if recordErr == nil && (site == "launch" || site == "worker") {
					launched = append(launched, record)
				}
				if server := (ProcessRecord{}); site == "worker" && readJSON(t.Context(), c.path("run", "server.json"), &server) == nil {
					launched = append(launched, server)
				}
				if progress.pending != nil {
					requireServiceLease(t, c.Repair(t.Context()) == nil, "repair did not close the open operation")
				}
				if kind != "none" && kind != "deadline_release" && (kind != "ordinary" || read != "record" || site != "alive") {
					requireServiceLease(t, s.run(t.Context()) == nil, "the next operation did not succeed")
					requireServiceLease(t, readJSON(t.Context(), c.path("run", s.component+".json"), &record) == nil && record.BootID == boot, "the next operation did not bind")
					if site == "launch" {
						launched = append(launched, record)
					}
				}
				if kind == "canceled" && site == "launch" { // a done context's first attempt binds the launched record
					record.BootID = ""
					requireServiceLease(t, writeAtomic(c.path("run", "server.json"), record, nil) == nil, "cannot unbind the launched record")
					ended, end := context.WithCancel(t.Context())
					end()
					requireServiceLease(t, c.bindServiceProcessBoot(ended, "server") == nil, "a done context refused an unleased binding")
					requireServiceLease(t, readJSON(t.Context(), c.path("run", "server.json"), &record) == nil && record.BootID == boot, "a direct binding on a done context left the launched record unbound")
				}
				if site != "retained" {
					requireServiceLease(t, c.Stop(t.Context()) == nil, "stop did not stop the tree")
				}
				for _, r := range launched {
					identity, alive, err := processIdentity(r.PID)
					requireServiceLease(t, err == nil && (!alive || identity != r.StartTime), "a launched child was left running")
				}
				for _, pid := range s.kids {
					if _, err := os.Lstat(filepath.Join(s.used, fmt.Sprint(pid))); err == nil { // a child pg_ctl handed out
						select {
						case <-s.exited[pid]:
						case <-time.After(5 * time.Second):
							t.Fatal("a test-owned child was left running")
						}
					}
				}
			})
		}
	}
}

// B1 under the service host (LC-LEASE-2E6-F1): the supervisor's step with the
// real Start, Stop and Repair, a descriptor lease at the server's or the
// worker's binding let go after the step's own deadline, before the ceiling:
// the tree runs, no failure is counted, no backoff, no receipt, as at the
// parent (the reviewed head counted one, backed off and wrote the start
// receipt). One round is enough: a round that counts nothing and leaves the
// tree running sends the next tick to health, never to a start, so five
// counted failures (the circuit) cannot accrue. Both leaves are regression pins.
func TestBindSupervisorOutlastsStepDeadline(t *testing.T) {
	for _, site := range []string{"launch", "worker"} {
		t.Run(site, func(t *testing.T) {
			s := newBindSite(t, site, false)
			c := s.cell
			hooks, _, _, _, _ := supervisorTestHooks()
			hooks.start, hooks.stop, hooks.repair = c.Start, c.stopServiceChildren, c.Repair
			host, previous := newSupervisor(c, SupervisorPolicy{hooks: &hooks}), os.Stderr
			d, err := c.ServiceDescriptor()
			requireServiceLease(t, err == nil && host.load(t.Context(), d) == nil, "cannot load the supervisor")
			capture, err := os.CreateTemp(t.TempDir(), "receipt")
			requireServiceLease(t, err == nil, "cannot capture the supervisor receipts")
			os.Stderr, s.run = capture, host.step
			defer func() { os.Stderr = previous; _ = capture.Close() }()
			waited, err := bindCut(t, s, "descriptor", "deadline_release")
			receipts, readErr := os.ReadFile(capture.Name())
			var saved supervisorRecord
			requireServiceLease(t, readErr == nil && readJSON(t.Context(), c.path("service", "supervisor.json"), &saved) == nil, "cannot inspect the service host")
			requireServiceLease(t, err == nil && host.running && host.status.State == "running", "the service host did not run the tree")
			requireServiceLease(t, saved.Attempts == 1 && len(saved.Failures) == 0, "the service host counted a failure")
			requireServiceLease(t, saved.NextStart.IsZero(), "the service host backed off")
			requireServiceLease(t, len(receipts) == 0, "the service host wrote a failure receipt")
			requireServiceLease(t, waited >= c.cfg.Timeout && waited < leaseWait.ceiling, "the holder did not let go between the step's deadline and the ceiling")
			var launched []ProcessRecord
			for _, component := range []string{"server", "worker"} {
				if r := (ProcessRecord{}); readJSON(t.Context(), c.path("run", component+".json"), &r) == nil {
					launched = append(launched, r)
				}
			}
			requireServiceLease(t, len(launched) == map[string]int{"launch": 1, "worker": 2}[site] && c.Stop(t.Context()) == nil, "stop did not stop the tree")
			for _, r := range launched {
				identity, alive, err := processIdentity(r.PID)
				requireServiceLease(t, err == nil && (!alive || identity != r.StartTime), "a launched child was left running")
			}
		})
	}
}

// Reinstall keeps only the step's own cut through a failed rollback or finish,
// as Start does: the postgres seam returns a cut of the step's context
// (cancelled, or at the step's own deadline), as the real wait does, an
// ordinary failure, or a cut that is not the step's own (a foreign deadline or
// cancellation with the operation live, a foreign deadline after the operation
// ended). The open operation is then closed by Repair, still removed, and a
// following Reinstall readmits. Every leaf but own_cancel and own_deadline is
// a regression pin: it passes on the parent's production code too.
func TestReinstallRollbackKeepsOwnCut(t *testing.T) {
	for _, after := range []string{"rollback", "finish"} {
		for _, leaf := range []string{"own_cancel", "own_deadline", "ordinary", "foreign_live", "foreign_canceled", "foreign_ended"} {
			t.Run(after+"/"+leaf, func(t *testing.T) {
				f, r := reinstallFixture(t, "legacy", false)
				c := f.cell
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if leaf == "own_deadline" {
					c.cfg.Timeout = 50 * time.Millisecond
				}
				c.reinstallTest.postgres = func(step context.Context, _ *Manifest) error {
					switch leaf {
					case "own_cancel", "own_deadline":
						if leaf == "own_cancel" {
							cancel()
						}
						<-step.Done()
						return &leaseWaitCut{cause: step.Err()}
					case "ordinary":
						return fixedCause("PostgreSQL command exited with failure")
					case "foreign_canceled":
						return &leaseWaitCut{cause: context.Canceled}
					case "foreign_ended":
						cancel()
					}
					return &leaseWaitCut{cause: context.DeadlineExceeded}
				}
				journal := readerJournalPath(c)
				if after == "rollback" {
					r.fail["stop"] = errors.New("synthetic stop failure")
				} else {
					c.reinstallTest.stop = func(context.Context) error {
						return errors.Join(os.Rename(journal, c.path("journal", "saved")), os.Mkdir(journal, 0o700))
					}
				}
				err := c.Reinstall(ctx, reinstallOptions(f))
				if after == "finish" {
					requireServiceLease(t, os.Remove(journal) == nil && os.Rename(c.path("journal", "saved"), journal) == nil, "cannot restore the journal")
				}
				parent := map[string]string{"rollback": "local cell reinstall failed; rollback incomplete, repair required", "finish": "local cell journal must be a regular file"}[after]
				if leaf == "own_cancel" || leaf == "own_deadline" {
					cause := map[bool]error{true: context.Canceled, false: context.DeadlineExceeded}[leaf == "own_cancel"]
					fixed := map[string]string{"rollback": "local cell reinstall failed; rollback incomplete, repair required", "finish": "local cell reinstall failed; rollback completion failed, repair required"}[after]
					requireServiceLease(t, serviceLeaseCut(err), "reinstall lost the step's own cut")
					requireServiceLease(t, errors.Is(err, cause), "reinstall lost the cut's cause")
					requireServiceLease(t, err.Error() == fixed+": local cell lease wait ended: "+cause.Error(), "reinstall changed the secondary refusal beside the cut")
				} else {
					requireServiceLease(t, err != nil && err.Error() == parent, "reinstall changed the parent's refusal")
					requireServiceLease(t, !serviceLeaseCut(err), "reinstall kept a cut that was not the step's own")
				}
				progress, journalErr := c.readJournal(t.Context())
				requireServiceLease(t, journalErr == nil && progress.pending != nil && progress.pending.Step == "postgres", "the failed rollback or finish did not leave the proof open")
				delete(r.fail, "stop")
				c.cfg.Timeout = time.Second
				*c.reinstallTest = reinstallOperations{postgres: r.postgres, query: r.query, stop: r.stop}
				calls := len(r.calls)
				requireServiceLease(t, c.Repair(t.Context()) == nil && len(r.calls) == calls, "repair did not settle the open reinstall")
				reinstallAssertRemoved(t, f)
				requireServiceLease(t, c.Reinstall(t.Context(), reinstallOptions(f)) == nil, "a following reinstall did not readmit")
			})
		}
	}
}
