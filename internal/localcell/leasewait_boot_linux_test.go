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
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Exercise the real stored reconciler. The later two hooks only record whether
// startup advanced past it. Each old process must validate before any removal.
func serviceOldBootLeases(t *testing.T) {
	for _, row := range []struct {
		name, file string
		lease      int
	}{
		{"unleased", "", 0},
		{"owner", ".owner.lock", unix.F_RDLCK},
		{"operation", "run/operation.lock", unix.F_RDLCK},
		{"descriptor", "service/descriptor.json", unix.F_WRLCK},
		{"manifest", "manifest.json", unix.F_WRLCK},
		{"boot_record", "service/boot.json", unix.F_WRLCK},
		{"maintenance", "journal/maintenance.json", unix.F_WRLCK},
		{"process", "run/server.json", unix.F_WRLCK},
		{"postmaster", "pg/postmaster.pid", unix.F_WRLCK},
	} {
		holders := []string{"kept"}
		if row.name == "process" {
			holders = append(holders, "retaken")
		}
		for _, holder := range holders {
			name := row.name
			if len(holders) > 1 {
				name += "/" + holder
			}
			t.Run(name, func(t *testing.T) {
				c, d := descriptorTestCell(t)
				const oldBoot = "linux:old-fixture"
				bootFile := c.path("service", "boot.json")
				requireServiceLease(t, writeAtomic(bootFile, serviceBootRecord{d.InstallationID, oldBoot}, nil) == nil, "cannot prepare the old boot")
				requireServiceLease(t, privateDir(c.path("pg"), true) == nil, "cannot prepare the postmaster directory")
				pidFile := c.path("pg", "postmaster.pid")
				pid := os.Getpid()
				requireServiceLease(t, writeAtomicBytes(pidFile, fmt.Appendf(nil, "%d\n%s\n1\n", pid, c.path("pg")), nil) == nil, "cannot prepare the old postmaster")
				stale := []string{pidFile}
				for _, component := range []string{"postgres", "server", "worker"} {
					r := ProcessRecord{PID: pid, StartTime: oldBoot + ":1", Root: c.cfg.Root, Component: component}
					path := c.path("run", component+".json")
					requireServiceLease(t, writeAtomic(path, r, nil) == nil, "cannot prepare an old process record")
					stale = append(stale, path)
				}
				if row.name == "maintenance" {
					updateTestFence(t, c)
				}
				s := newSupervisor(c, SupervisorPolicy{})
				s.pendingStartup = serviceStartupReconcile
				var after []string
				s.hooks.repair = func(context.Context) error { after = append(after, "repair"); return nil }
				s.hooks.stop = func(context.Context) error { after = append(after, "stop"); return nil }
				if row.file == "" {
					requireServiceLease(t, s.resumeServiceStartup(t.Context()) == nil, "unleased reconciliation refused valid old records")
					requireServiceLease(t, slices.Equal(after, []string{"repair", "stop"}), "unleased reconciliation changed startup ordering")
					var b serviceBootRecord
					requireServiceLease(t, readJSON(t.Context(), bootFile, &b) == nil, "cannot read the published boot")
					boot, err := serviceBootID()
					requireServiceLease(t, err == nil, "current boot unavailable")
					requireServiceLease(t, b.BootID == boot, "reconciliation did not publish the current boot")
					requireServiceLease(t, b.InstallationID == d.InstallationID, "reconciliation changed the boot installation")
					for _, path := range stale {
						_, err := os.Lstat(path)
						requireServiceLease(t, errors.Is(err, os.ErrNotExist), "unleased reconciliation left a stale record")
					}
					return
				}
				// Compare both the bytes and the objects (an identical rewrite is still a write).
				protected := append(stale, bootFile, c.path("service", "descriptor.json"), c.path("manifest.json"))
				if row.name == "maintenance" {
					protected = append(protected, c.path(row.file))
				}
				type snapshot struct {
					data []byte
					info os.FileInfo
				}
				before := make(map[string]snapshot)
				for _, path := range protected {
					data, err := os.ReadFile(path)
					requireServiceLease(t, err == nil, "cannot snapshot the old boot state")
					info, err := os.Lstat(path)
					requireServiceLease(t, err == nil, "cannot identify an old boot object")
					before[path] = snapshot{data, info}
				}
				l := leaseHolder(t, holder, row.lease)
				leaseWait.ceiling = 2 * time.Second
				leaseTaken(t, l, c.path(row.file))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cancelAtServiceLeaseBreak(t, l, cancel)
				started := time.Now()
				err := s.resumeServiceStartup(ctx)
				elapsed := time.Since(started)
				l.close()
				requireServiceLease(t, serviceLeaseCut(err), "service reader lost lease cut")
				requireServiceLease(t, errors.Is(err, context.Canceled), "old boot read lost the cancellation")
				for _, ordinary := range []error{ErrServiceOwnershipConflict, ErrLocked, errMaintenanceFenceActive, os.ErrNotExist, unix.EWOULDBLOCK} {
					requireServiceLease(t, !errors.Is(err, ordinary), "old boot read attached an ordinary refusal to the cut")
				}
				requireServiceLease(t, elapsed < leaseWait.ceiling/2, "old boot cut waited past half the lease ceiling")
				requireServiceLease(t, s.pendingStartup == serviceStartupReconcile, "a cut advanced the startup phase")
				requireServiceLease(t, len(after) == 0, "a cut reconciliation invoked a later startup hook")
				for path, was := range before {
					data, err := os.ReadFile(path)
					requireServiceLease(t, err == nil, "reconciliation removed an old boot object after a cut")
					requireServiceLease(t, bytes.Equal(data, was.data), "reconciliation changed old boot state after a cut")
					info, err := os.Lstat(path)
					requireServiceLease(t, err == nil, "an old boot object disappeared after a cut")
					requireServiceLease(t, os.SameFile(info, was.info), "reconciliation replaced an old boot object after a cut")
				}
			})
		}
	}
}

func TestPostPublicationCut(t *testing.T) {
	for _, row := range []string{"released", "rollback", "finish", "deadline"} {
		t.Run(row, func(t *testing.T) {
			closed := row == "released" || row == "deadline" // the rollback closes the start
			c, _ := descriptorTestCell(t)
			requireServiceLease(t, c.makePassword() == nil, "cannot prepare the start fixture")
			requireServiceLease(t, privateDir(c.path("pg"), true) == nil, "cannot prepare the start directory")
			c.cfg.Binaries.PostgresDir = t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			descriptor := leaseHolder(t, "kept", unix.F_WRLCK)
			record := leaseHolder(t, "kept", unix.F_WRLCK)
			leaseWait.ceiling = 750 * time.Millisecond
			c.cfg.Timeout = 8 * leaseWait.ceiling
			if row == "deadline" {
				// The lease outlives the postgres step's own budget, which cuts the binding.
				c.cfg.Timeout = 2 * leaseWait.ceiling
				leaseWait.ceiling = 2 * c.cfg.Timeout
			}
			refused := filepath.Join(t.TempDir(), "refused")
			var exited <-chan error
			awaitExit := func() {
				t.Helper()
				select {
				case <-exited:
				case <-time.After(c.cfg.Timeout):
					t.Fatal("synthetic pg_ctl did not stop the child within the rollback budget")
				}
			}
			var published ProcessRecord
			var rollbackBegan time.Time
			ops := c.principalStartOperations()
			start := ops.postgres
			ops.postgres = func(step context.Context, m *Manifest) error {
				// pg_ctl stands in only for starting/stopping this bounded child. The
				// real Unix launcher reads the pidfile and publishes the process record.
				child := exec.Command("sleep", "60")
				child.Env = []string{}
				requireServiceLease(t, child.Start() == nil, "cannot start the bounded child")
				exited = StartJoinedTestRunForTest(t, func() { _ = child.Process.Kill() }, c.cfg.Timeout, child.Wait)
				const pgCtl = `#!/bin/sh
case "$1" in
start) printf '%%s\n%%s\n1\n' %d "$3" > "$3/postmaster.pid" ;;
stop) %skill %d && command -p rm "$3/postmaster.pid" %s ;;
*) exit 1 ;;
esac
`
				obstacle, refusal := "", ""
				if row == "rollback" {
					// The rollback's first stop runs on its own deadline and is refused.
					refusal = fmt.Sprintf("[ -e '%s' ] || { : > '%s'; exit 1; }; ", refused, refused)
				}
				if row == "finish" {
					obstacle = "&& command -p mv journal/operations.jsonl journal/saved && command -p mkdir journal/operations.jsonl"
				}
				script := fmt.Sprintf(pgCtl, child.Process.Pid, refusal, child.Process.Pid, obstacle)
				requireServiceLease(t, os.WriteFile(filepath.Join(c.cfg.Binaries.PostgresDir, "pg_ctl"), []byte(script), 0700) == nil, "cannot prepare synthetic pg_ctl")
				// Pause the actual post-launch binding at its descriptor, after the
				// process record has been published, and lease that exact record.
				leaseTaken(t, descriptor, c.path("service", "descriptor.json"))
				done := StartJoinedTestRunForTest(t, cancel, c.cfg.Timeout, func() error { return start(step, m) })
				_, ok := leaseContextBroken(descriptor)
				requireServiceLease(t, ok, "the shared binding never reached its descriptor")
				path := c.path("run", "postgres.json")
				requireServiceLease(t, readJSON(t.Context(), path, &published) == nil, "the binding preceded process publication")
				requireServiceLease(t, published.PID == child.Process.Pid, "the launcher published the wrong child")
				if !closed {
					leaseTaken(t, record, path)
					descriptor.close()
					_, ok = leaseContextBroken(record)
					requireServiceLease(t, ok, "the shared binding never reached its process record")
				}
				if row != "deadline" {
					cancel()
				}
				err := <-done
				descriptor.close()
				if row != "rollback" {
					record.close()
				}
				rollbackBegan = time.Now()
				if row == "rollback" {
					// The rollback's own read waits the lease out, then reaches its stop.
					time.AfterFunc(5*leaseWait.ceiling/8, record.close)
				}
				return err
			}
			c.installerStart = &ops
			err := c.Start(ctx)
			rollbackElapsed := time.Since(rollbackBegan)
			record.close()
			requireServiceLease(t, serviceLeaseCut(err), "start lost the cut")
			cause := context.Canceled
			if row == "deadline" {
				cause = context.DeadlineExceeded
				requireServiceLease(t, errors.Is(err, cause), "start lost the step deadline")
				requireServiceLease(t, !errors.Is(err, context.Canceled), "a step deadline cut carried a cancellation")
				requireServiceLease(t, ctx.Err() == nil, "the step's own deadline ended the operation")
				requireServiceLease(t, !errors.Is(err, ErrServiceOwnershipConflict), "a step deadline cut read as a stale or foreign binding")
				requireServiceLease(t, !errors.Is(err, os.ErrNotExist), "a step deadline cut read as an absent binding")
			} else {
				requireServiceLease(t, errors.Is(err, context.Canceled), "start lost the cancellation")
			}
			want := map[string]string{"released": "local cell start: postgres failed: ", "rollback": "local cell startup failed; rollback incomplete, repair required: ", "finish": "local cell startup failed; rollback completion failed, repair required: ", "deadline": "local cell start: postgres failed: "}
			requireServiceLease(t, err.Error() == want[row]+"local cell lease wait ended: "+cause.Error(), "start changed the fixed cut receipt")
			if row == "rollback" {
				requireServiceLease(t, rollbackElapsed >= leaseWait.ceiling/2, "rollback skipped its independent lease wait")
				requireServiceLease(t, rollbackElapsed < 2*leaseWait.ceiling, "rollback exceeded twice its lease ceiling")
				_, statErr := os.Lstat(refused)
				requireServiceLease(t, statErr == nil, "the rollback's stop did not run on the rollback's own deadline")
			}
			if row == "finish" {
				requireServiceLease(t, os.Remove(readerJournalPath(c)) == nil, "cannot clear the completion obstacle")
				requireServiceLease(t, os.Rename(c.path("journal", "saved"), readerJournalPath(c)) == nil, "cannot restore the original journal")
			}
			if row != "rollback" {
				awaitExit()
			}
			state, observed, err := c.processStateWithRecord(t.Context(), "postgres")
			requireServiceLease(t, err == nil, "cannot inspect the launched child")
			requireServiceLease(t, observed.BootID == "", "a cut authorized a boot binding")
			requireServiceLease(t, observed == published, "a cut changed the published process fence")
			requireServiceLease(t, state.Alive == (row == "rollback"), "rollback left the wrong child state")
			requireServiceLease(t, state.Matches == (row == "rollback"), "rollback left the wrong child ownership")
			m, err := readManifest(t.Context(), c.cfg.Root)
			requireServiceLease(t, err == nil, "cannot inspect the start manifest")
			requireServiceLease(t, (m.LastOperation == "start") == (row != "rollback"), "start manifest crossed the wrong completion boundary")
			progress, err := c.readJournal(t.Context())
			requireServiceLease(t, err == nil, "cannot inspect the start journal")
			requireServiceLease(t, !progress.tornTail, "start left a torn journal")
			steps := []string{"operation:started", "postgres:started", "postgres:failed"}
			if closed {
				steps = append(steps, "operation:done")
				requireServiceLease(t, progress.pending == nil, "successful rollback did not close the start")
			} else {
				requireServiceLease(t, progress.pending != nil, "secondary failure closed the start")
				requireServiceLease(t, progress.pending.Operation == "start", "secondary failure changed the open operation")
				requireServiceLease(t, progress.pending.Step == "postgres", "secondary failure changed the open step")
			}
			requireServiceLease(t, slices.Equal(leaseJournalRecords(c), steps), "start wrote unexpected journal records")
			if !closed {
				requireServiceLease(t, c.Repair(t.Context()) == nil, "repair did not recover the open start")
				if row == "rollback" {
					awaitExit()
				}
				state, err := c.processState(t.Context(), "postgres")
				requireServiceLease(t, err == nil, "cannot inspect the repaired child")
				requireServiceLease(t, !state.Alive, "repair left the child running")
				progress, err = c.readJournal(t.Context())
				requireServiceLease(t, err == nil, "cannot inspect the repaired journal")
				requireServiceLease(t, progress.pending == nil, "repair left the start operation open")
				steps = append(steps, "revert_stop:started", "revert_stop:done", "operation:done")
				requireServiceLease(t, slices.Equal(leaseJournalRecords(c), steps), "repair closed the interrupted start more than once or out of order")
			}
		})
	}
}

// A lease cut owns the supervisor operation only if that operation ended.
// Refusal by the secondary stop still commits its fault and both receipts.
func TestSupervisorStartLeaseCut(t *testing.T) {
	for _, row := range []string{"operation", "step_deadline", "step_deadline_then_stop"} {
		t.Run(row, func(t *testing.T) {
			c, d := descriptorTestCell(t)
			hooks, _, _, _, _ := supervisorTestHooks()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			l := leaseHolder(t, "kept", unix.F_WRLCK)
			leaseWait.ceiling = 2 * time.Second
			hooks.start = func(ctx context.Context) error {
				leaseTaken(t, l, c.path("service", "descriptor.json"))
				if row == "operation" {
					cancelAtServiceLeaseBreak(t, l, cancel)
				} else {
					var end context.CancelFunc
					ctx, end = context.WithTimeout(ctx, leaseWait.ceiling/8)
					defer end()
				}
				_, err := c.serviceDescriptor(ctx)
				requireServiceLease(t, serviceLeaseCut(err), "the start hook did not encounter a lease cut")
				l.close()
				if row == "step_deadline_then_stop" {
					// The hook's own deadline cut the read; the operation ends after.
					requireServiceLease(t, errors.Is(err, context.DeadlineExceeded), "the hook's own deadline did not cut its read")
					cancel()
				}
				return err
			}
			stops := 0
			hooks.stop = func(context.Context) error { stops++; return errors.New("synthetic stop refusal") }
			s := newSupervisor(c, SupervisorPolicy{hooks: &hooks})
			requireServiceLease(t, s.load(d) == nil, "cannot load the supervisor cut fixture")
			capture, err := os.CreateTemp(t.TempDir(), "receipt")
			requireServiceLease(t, err == nil, "cannot capture the supervisor receipts")
			previous := os.Stderr
			os.Stderr = capture
			defer func() { os.Stderr = previous; _ = capture.Close() }()
			err = s.step(ctx)
			after, readErr := c.ServiceDescriptor()
			requireServiceLease(t, readErr == nil, "cannot reread the supervisor cut descriptor")
			receipts, readErr := os.ReadFile(capture.Name())
			requireServiceLease(t, readErr == nil, "cannot read the supervisor receipts")
			const start = "native service host site=supervisor_start\n"
			switch row {
			case "operation":
				requireServiceLease(t, serviceLeaseCut(err), "supervisor lost the operation's start cut")
				requireServiceLease(t, errors.Is(err, context.Canceled), "supervisor lost the operation cancellation")
				requireServiceLease(t, s.status.LastStepOutcome == "not_run", "an interrupted start replaced the last completed snapshot")
				requireServiceLease(t, stops == 0, "an interrupted start invoked the secondary stop")
				requireServiceLease(t, !after.Faulted, "an interrupted start recorded a fault")
				requireServiceLease(t, after.Revision == d.Revision, "an interrupted start changed the descriptor revision")
				requireServiceLease(t, string(receipts) == start, "an interrupted start changed its host receipt")
			case "step_deadline_then_stop":
				requireServiceLease(t, err == nil, "a cut that was not the operation's own ended the stopped operation")
				requireServiceLease(t, stops == 0, "a stopped operation invoked the secondary stop")
				requireServiceLease(t, !after.Faulted, "a stopped operation recorded a fault")
				requireServiceLease(t, string(receipts) == start, "a stopped operation changed its host receipt")
			default:
				requireServiceLease(t, err == nil, "a step deadline terminated the supervisor operation")
				requireServiceLease(t, s.status.Reason == "service_stop_refused", "the step deadline lost the secondary stop reason")
				requireServiceLease(t, stops == 1, "a step deadline skipped the secondary stop")
				requireServiceLease(t, after.Faulted, "a secondary stop refusal lost its fault")
				requireServiceLease(t, after.Revision == d.Revision+1, "a secondary stop refusal changed the wrong revision")
				requireServiceLease(t, string(receipts) == start+"native service host site=supervisor_stop\n", "a secondary stop refusal lost its host receipt")
			}
			var saved supervisorRecord
			requireServiceLease(t, readJSON(t.Context(), c.path("service", "supervisor.json"), &saved) == nil, "cannot inspect the start attempt")
			requireServiceLease(t, saved.Attempts == 1, "the admitted start lost its attempt")
			requireServiceLease(t, len(saved.Failures) == 0, "the start cut recorded a backoff failure")
			requireServiceLease(t, saved.NextStart.IsZero(), "the start cut scheduled a retry")
		})
	}
}

// A fresh Install and a selected Repair reach the shared binding after a real
// process publication; neither classifies the operation's cut.
func TestPostLaunchCallerCut(t *testing.T) {
	for _, row := range []string{"install", "repair"} {
		t.Run(row, func(t *testing.T) {
			c, _ := descriptorTestCell(t)
			l := leaseHolder(t, "kept", unix.F_WRLCK)
			leaseWait.ceiling = 750 * time.Millisecond
			c.cfg.Timeout = 8 * leaseWait.ceiling
			dir := t.TempDir()
			c.cfg.Binaries = Binaries{PostgresDir: dir, Server: filepath.Join(dir, "server"), Worker: filepath.Join(dir, "worker")}
			child := exec.Command("sleep", "60")
			child.Env = []string{}
			requireServiceLease(t, child.Start() == nil, "cannot start the bounded child")
			exited := StartJoinedTestRunForTest(t, func() { _ = child.Process.Kill() }, c.cfg.Timeout, child.Wait)
			for name, body := range map[string]string{"postgres": "echo 'postgres (PostgreSQL) 17.4'", "server": "echo v0", "worker": "echo v0",
				"initdb": `command -p mkdir -m 700 "$2"`, "pg_ctl": fmt.Sprintf(`case "$1" in
start) printf '%%s\n%%s\n1\n' %d "$3" > "$3/postmaster.pid" ;;
stop) kill %d && command -p rm "$3/postmaster.pid" ;;
*) exit 1 ;;
esac`, child.Process.Pid, child.Process.Pid)} {
				requireServiceLease(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o700) == nil, "cannot prepare a synthetic tool")
			}
			later := 0
			var journal []byte
			if row == "install" {
				requireServiceLease(t, os.Remove(c.path("manifest.json")) == nil, "cannot prepare the fresh install")
			} else {
				m, err := readManifest(t.Context(), c.cfg.Root)
				requireServiceLease(t, err == nil, "cannot read the selected manifest")
				m.Installed, m.PrincipalLayoutVersion, m.PrincipalRoles = false, PrincipalLayoutVersion, installerRoleNames()
				requireServiceLease(t, writeAtomic(c.path("manifest.json"), m, nil) == nil && c.begin(t.Context(), "install") == nil, "cannot open the selected install")
				for _, step := range append([]string{"manifest", "password", "initdb", "configure", "postgres", "database"}, selectedInstallSteps...) {
					requireServiceLease(t, c.step(t.Context(), "install", step, func() error { return nil }) == nil, "cannot record a durable install phase")
				}
				requireServiceLease(t, c.makePassword() == nil && privateDir(c.path("pg"), true) == nil, "cannot prepare the selected data directory")
				phase := func(context.Context, *Manifest) error { later++; return nil }
				c.installerPrincipals = &installerPrincipalOperations{credentials: phase, provision: phase, migrate: phase, grants: phase, verify: phase, resumePostgres: c.startServicePostgres}
				journal, err = os.ReadFile(readerJournalPath(c))
				requireServiceLease(t, err == nil, "cannot snapshot the open install journal")
			}
			leaseTaken(t, l, c.path("service", "descriptor.json"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cancelAtServiceLeaseBreak(t, l, cancel)
			var err error
			if row == "install" {
				err = c.Install(ctx, InstallOptions{})
			} else {
				err = c.Repair(ctx)
			}
			requireServiceLease(t, serviceLeaseCut(err), "the caller lost the binding's cut")
			requireServiceLease(t, errors.Is(err, context.Canceled), "the caller lost the cancellation")
			for _, ordinary := range []error{ErrServiceOwnershipConflict, ErrLocked, errMaintenanceFenceActive, os.ErrNotExist} {
				requireServiceLease(t, !errors.Is(err, ordinary), "the caller classified the binding's cut")
			}
			const cause = "local cell lease wait ended: context canceled"
			progress, readErr := c.readJournal(t.Context())
			requireServiceLease(t, readErr == nil && progress.pending != nil, "the caller closed the interrupted install")
			var record ProcessRecord
			requireServiceLease(t, readJSON(t.Context(), c.path("run", "postgres.json"), &record) == nil, "the caller lost the published process record")
			requireServiceLease(t, record.PID == child.Process.Pid, "the cut preceded the launcher's publication")
			requireServiceLease(t, record.BootID == "", "a cut authorized a boot binding")
			if row == "install" {
				requireServiceLease(t, err.Error() == "local cell install: postgres failed: "+cause, "install changed its step error")
				requireServiceLease(t, progress.pending.Step == "postgres", "install moved its failed step")
				select {
				case <-exited:
				case <-time.After(c.cfg.Timeout):
					t.Fatal("install did not roll back the launched child")
				}
				return
			}
			requireServiceLease(t, err.Error() == cause, "repair changed the first resume error")
			after, readErr := os.ReadFile(readerJournalPath(c))
			requireServiceLease(t, readErr == nil && bytes.Equal(after, journal), "repair changed the open install's journal")
			requireServiceLease(t, later == 0, "repair ran a later phase after the cut")
			state, readErr := c.processState(t.Context(), "postgres")
			requireServiceLease(t, readErr == nil && state.Alive, "repair stopped the child after the cut")
		})
	}
}
