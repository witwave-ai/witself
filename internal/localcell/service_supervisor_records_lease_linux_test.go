//go:build linux

package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// supervisorRecordsFixture is the host fixture with a supervisor record, three
// recorded children and the host's own lease record on disk, loaded once.
func supervisorRecordsFixture(t *testing.T) serviceMaintenanceHostFixture {
	t.Helper()
	f := newServiceMaintenanceHostFixture(t)
	record := supervisorRecord{Version: 1, RepairRevision: f.d.RepairRevision, Attempts: 3}
	requireServiceLease(t, writeAtomic(f.cell.path("service", "supervisor.json"), record, nil) == nil, "cannot seed the supervisor record")
	for _, name := range []string{"postgres", "server", "worker"} {
		child := ProcessRecord{PID: 23, StartTime: "recorded", Root: f.d.Root, Component: name}
		requireServiceLease(t, writeAtomic(f.cell.path("run", name+".json"), child, nil) == nil, "cannot seed a recorded child")
	}
	lock, err := f.cell.acquireServiceLease(t.Context(), f.d)
	requireServiceLease(t, err == nil && lock.Close() == nil, "cannot seed the recorded host")
	requireServiceLease(t, f.s.load(t.Context(), f.d) == nil && len(f.s.status.Processes) == 4, "cannot load the recorded snapshot")
	return f
}

func withoutComponent(processes []ServiceProcessIdentity, component string) []ServiceProcessIdentity {
	return slices.DeleteFunc(slices.Clone(processes), func(p ServiceProcessIdentity) bool { return p.Component == component })
}

// Each read of load and of the snapshot (LC-LEASE-2E4) holds a kept lease, none
// opened earlier in the call. Cancelled at the kernel's break, the read returns
// its typed cut with nothing installed or published; a live read keeps the
// parent's handling at the ceiling (the record's refusal, or the unreadable
// component left out); a released lease completes the read.
func TestSupervisorRecordsLease(t *testing.T) {
	for _, via := range []string{"load", "remember"} {
		for _, target := range []struct{ name, file, component string }{{"record", "service/supervisor.json", ""}, {"postgres", "run/postgres.json", "postgres"},
			{"server", "run/server.json", "server"}, {"worker", "run/worker.json", "worker"}, {"host", "service/lease.json", "service-host"}} {
			if via == "remember" && target.component == "" {
				continue
			}
			t.Run(via+"/"+target.name, func(t *testing.T) {
				f := supervisorRecordsFixture(t)
				run := func(ctx context.Context) error { return f.s.rememberProcesses(ctx, f.d) }
				if via == "load" {
					run = func(ctx context.Context) error { return f.s.load(ctx, f.d) }
				}
				full, prior := slices.Clone(f.s.status.Processes), supervisorRecord{Version: 1, RepairRevision: f.d.RepairRevision, Attempts: 9}
				before := ServiceStatus{State: "prior", LastStepOutcome: "prior", Processes: []ServiceProcessIdentity{{Component: "server", PID: 22, StartTime: "prior"}}}
				f.s.record, f.s.status, f.s.startupFaulted = prior, before, true
				holder := leaseHolder(t, "kept", unix.F_WRLCK)
				leaseWait.ceiling = 2 * time.Second
				leaseTaken(t, holder, f.cell.path(target.file))
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- run(ctx) }()
				at, broken := leaseContextBroken(holder)
				cancel()
				err := <-done
				returned := time.Since(at)
				holder.close()
				requireServiceLease(t, broken, "supervisor records never broke the selected lease")
				requireExitLeaseCut(t, err, context.Canceled)
				requireServiceLease(t, returned < leaseWait.ceiling/2, "supervisor records waited to the lease ceiling after cancellation")
				requireServiceLease(t, slices.Equal(f.s.status.Processes, before.Processes), "cut record read published a process list")
				requireServiceLease(t, reflect.DeepEqual(f.s.status, before), "cut record read changed the status")
				requireServiceLease(t, reflect.DeepEqual(f.s.record, prior), "cut record read installed a supervisor record")
				requireServiceLease(t, f.s.startupFaulted, "cut record read changed the startup fault")

				leaseWait.ceiling = 200 * time.Millisecond
				leaseTaken(t, holder, f.cell.path(target.file))
				started := time.Now()
				err = run(t.Context())
				elapsed := time.Since(started)
				holder.close()
				requireServiceLease(t, !serviceLeaseCut(err), "live record read reported a cut")
				requireServiceLease(t, elapsed >= leaseWait.ceiling/2, "live record read refused before its lease budget")
				if located := (*serviceFailure)(nil); target.component == "" {
					requireServiceLease(t, errors.As(err, &located) && located.site == "supervisor_load", "live record read lost the parent's ceiling refusal")
					requireServiceLease(t, reflect.DeepEqual(f.s.record, prior), "live record refusal installed a supervisor record")
				} else {
					requireServiceLease(t, err == nil, "live ceiling read refused the snapshot")
					requireServiceLease(t, slices.Equal(f.s.status.Processes, withoutComponent(full, target.component)), "live ceiling read did not leave out the unreadable component")
				}
				requireServiceLease(t, run(t.Context()) == nil, "released record read did not complete")
				requireServiceLease(t, slices.Equal(f.s.status.Processes, full), "released record read lost the snapshot")
			})
		}
	}
}

// supervisorRefreshOutcome is what a step leaves, apart from the cell's own
// generation and installation (each fixture is its own cell).
type supervisorRefreshOutcome struct {
	err                     error
	status                  ServiceStatus
	record                  supervisorRecord
	running                 bool
	starts, stops, healths  int
	coherent, fresh, before []ServiceProcessIdentity
}

// supervisorRefreshStep runs one step of the row on a fresh fixture whose
// server record changed since the snapshot; with lease, the refresh's last read
// (the host record, after the three children) holds a kept lease and the
// step's context ends at the break.
func supervisorRefreshStep(t *testing.T, row string, lease bool) supervisorRefreshOutcome {
	f := supervisorRecordsFixture(t)
	out := supervisorRefreshOutcome{coherent: slices.Clone(f.s.status.Processes)}
	moved := ProcessRecord{PID: 24, StartTime: "moved", Root: f.d.Root, Component: "server"}
	requireServiceLease(t, writeAtomic(f.cell.path("run", "server.json"), moved, nil) == nil, "cannot move a recorded child")
	out.fresh = slices.Clone(out.coherent)
	out.fresh[1] = ServiceProcessIdentity{Component: "server", PID: 24, StartTime: "moved"}
	f.s.running = true
	switch row {
	case "failure":
		f.s.hooks.health = func(context.Context) error { return errors.New("synthetic unhealthy") }
		path := f.cell.path("service", "supervisor.json")
		requireServiceLease(t, os.Remove(path) == nil && os.Mkdir(path, 0o700) == nil, "cannot refuse the supervisor save")
	case "busy":
		lock, err := f.cell.own(t.Context(), false)
		requireServiceLease(t, err == nil, "cannot hold the operation lock")
		defer func() { _ = lock.Close() }()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var holder *writerLease
	broke := make(chan time.Time, 1)
	if lease {
		holder = leaseTaken(t, leaseHolder(t, "kept", unix.F_WRLCK), f.cell.path("service", "lease.json"))
		leaseWait.ceiling = 2 * time.Second
		go func() { at, _ := leaseContextBroken(holder); cancel(); broke <- at }()
	}
	if row == "status" {
		work := f.s.serveCommand(t.Context(), serviceCommand{ctx: ctx, action: "status"})
		out.err, out.status = work.err, work.status
	} else {
		out.err, out.status = f.s.step(ctx), f.s.status
	}
	if returned := time.Now(); lease {
		at := <-broke
		holder.close()
		requireServiceLease(t, holder.broken(), "the step's refresh never broke the selected lease")
		requireServiceLease(t, returned.Sub(at) < leaseWait.ceiling/2, "the step's refresh waited to the lease ceiling after cancellation")
	}
	out.before, out.status.Generation = out.status.Processes, ""
	out.status.Processes, out.record, out.running = nil, f.s.record, f.s.running
	out.starts, out.stops, out.healths = *f.starts, *f.stops, *f.healths
	return out
}

// The step's refresh is bookkeeping (LC-LEASE-2E4). Cut at the kernel's break,
// it leaves the step's own result, success or an independent failure, and the
// status, failure accounting and hook calls of the same step run without a
// lease, with the last coherent snapshot in place of the fresh one; it never
// triggers the step's deferred restore. A status command whose client ends
// during the refresh is restored by that cancellation, as at the parent: the
// state that row asserts is the parent's, and its timing check is the one
// assertion that tells a refresh on the command's context from a detached one.
func TestSupervisorRefreshLease(t *testing.T) {
	for _, row := range []string{"success", "failure", "busy", "status"} {
		t.Run(row, func(t *testing.T) {
			cut := supervisorRefreshStep(t, row, true)
			requireServiceLease(t, slices.Equal(cut.before, cut.coherent), "a cut refresh replaced the last coherent snapshot")
			requireServiceLease(t, !serviceLeaseCut(cut.err), "a cut refresh became the step's result")
			if row == "status" {
				requireServiceLease(t, errors.As(cut.err, new(*serviceControlClientCancellation)), "an abandoned status lost its client cancellation")
				requireServiceLease(t, errors.Is(cut.err, context.Canceled), "an abandoned status lost its cancellation cause")
				requireServiceLease(t, cut.status.LastStepOutcome == "not_run", "an abandoned status kept the step it ended")
				return
			}
			plain := supervisorRefreshStep(t, row, false)
			requireServiceLease(t, slices.Equal(plain.before, plain.fresh), "the reference step did not refresh its snapshot")
			requireServiceLease(t, (cut.err == nil) == (plain.err == nil), "a cut refresh changed whether the step failed")
			if plain.err != nil {
				var site, reference *serviceFailure
				requireServiceLease(t, errors.As(plain.err, &reference) && reference.site == "supervisor_save", "the failure fixture did not fail at the supervisor save")
				requireServiceLease(t, errors.As(cut.err, &site) && site.site == reference.site, "a cut refresh changed the failure's site")
				requireServiceLease(t, cut.err.Error() == plain.err.Error(), "a cut refresh changed the step's failure")
			}
			requireServiceLease(t, cut.status.LastStepOutcome != "not_run", "a cut refresh triggered the step's deferred restore")
			requireServiceLease(t, reflect.DeepEqual(cut.status, plain.status), "a cut refresh changed the step's status")
			cut.record.RepairRevision, plain.record.RepairRevision = 0, 0
			requireServiceLease(t, reflect.DeepEqual(cut.record, plain.record), "a cut refresh changed the failure accounting")
			requireServiceLease(t, cut.running == plain.running, "a cut refresh changed the running tree")
			requireServiceLease(t, [3]int{cut.starts, cut.stops, cut.healths} == [3]int{plain.starts, plain.stops, plain.healths}, "a cut refresh changed the step's hook calls")
		})
	}
}

// Completed reads keep the parent's handling at load and in the step's refresh;
// an absent record keeps what the supervisor held, as at the parent. A refused
// record leaves the supervisor's record as it was, failures included (the parent
// left a partly decoded one behind its error; only a direct caller sees it, as
// Run returns that error and discards its supervisor).
func TestSupervisorRecordsCompletedReads(t *testing.T) {
	for _, row := range []struct{ name, file, data, text string }{
		{"absent", "service/supervisor.json", "", ""},
		{"undecodable", "service/supervisor.json", `{"version":1,"attempts":7,"failures":["2026-10-27T00:00:00Z"],"unknown":1}`, "invalid local cell record"},
		{"version", "service/supervisor.json", `{"version":2,"attempts":7}`, "localcell: invalid_supervisor_record"},
		{"failures", "service/supervisor.json", `{"version":1,"failures":["2026-10-27T00:00:00Z","2026-10-27T00:00:00Z","2026-10-27T00:00:00Z","2026-10-27T00:00:00Z","2026-10-27T00:00:00Z","2026-10-27T00:00:00Z"]}`, "localcell: invalid_supervisor_record"},
		{"not_private", "service/supervisor.json", "0644", "service_record_not_private"},
		{"load/component_absent", "run/server.json", "", ""}, {"load/component_undecodable", "run/server.json", "{", ""},
		{"load/component_root", "run/server.json", `{"pid":23,"start_time":"recorded","root":"/elsewhere","component":"server"}`, ""},
		{"refresh/component_absent", "run/server.json", "", ""}, {"refresh/component_undecodable", "run/server.json", "{", ""},
		{"refresh/component_root", "run/server.json", `{"pid":23,"start_time":"recorded","root":"/elsewhere","component":"server"}`, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := supervisorRecordsFixture(t)
			full, held := slices.Clone(f.s.status.Processes), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			prior := supervisorRecord{Version: 1, RepairRevision: f.d.RepairRevision, Attempts: 9, Failures: []time.Time{held}}
			path := f.cell.path(row.file)
			switch row.data {
			case "":
				requireServiceLease(t, os.Remove(path) == nil, "cannot remove the fixture record")
			case "0644":
				requireServiceLease(t, os.Chmod(path, 0o644) == nil, "cannot expose the fixture record")
			default:
				requireServiceLease(t, os.WriteFile(path, []byte(row.data), 0o600) == nil, "cannot plant the fixture record")
			}
			f.s.record = prior
			f.s.record.Failures = []time.Time{held}
			var err error
			if strings.HasPrefix(row.name, "refresh/") {
				err = f.s.step(t.Context())
			} else {
				err = f.s.load(t.Context(), f.d)
			}
			if row.text != "" {
				var located *serviceFailure
				requireServiceLease(t, err != nil && err.Error() == row.text, "a refused record lost the parent's refusal")
				requireServiceLease(t, errors.As(err, &located) && located.site == "supervisor_load", "a refused record lost its load site")
				requireServiceLease(t, reflect.DeepEqual(f.s.record, prior), "a refused record was installed")
				requireServiceLease(t, slices.Equal(f.s.status.Processes, full), "a refused record changed the snapshot")
				return
			}
			requireServiceLease(t, err == nil, "a completed read refused the supervisor")
			if row.name == "absent" {
				requireServiceLease(t, reflect.DeepEqual(f.s.record, prior), "an absent record changed what the supervisor held")
			}
			requireServiceLease(t, slices.Equal(f.s.status.Processes, withoutComponent(full, strings.TrimSuffix(strings.TrimPrefix(row.file, "run/"), ".json"))), "an unusable component was not left out of the snapshot")
		})
	}
}

// Run-level rows (the control socket, so not on a lane that refuses it). load:
// Run cancelled at the break of a kept lease on its record returns nil within
// half the ceiling, with no hook called and the record unwritten. tick: a tick
// whose refresh is cut by Run's own stop returns nil after the tick's health
// and the shutdown's repair and stop. status: a delivered status whose client
// ends during the refresh, then an observe, show the last coherent snapshot.
// The result and the state of tick and status are the parent's; their timing
// checks are what fails when the refresh waits on a detached context.
func TestSupervisorRecordsRun(t *testing.T) {
	for _, row := range []string{"load", "tick", "status"} {
		t.Run(row, func(t *testing.T) {
			holder := leaseHolder(t, "kept", unix.F_WRLCK)
			var record []byte
			w := serviceWorkerConfigured(t, "reconcile", false, func(c *Cell, d ServiceDescriptor) {
				leaseWait.ceiling = 2 * time.Second
				child := ProcessRecord{PID: 23, StartTime: "recorded", Root: d.Root, Component: "worker"}
				requireServiceLease(t, writeAtomic(c.path("run", "worker.json"), child, nil) == nil, "cannot seed the host's recorded child")
				requireServiceLease(t, writeAtomic(c.path("service", "supervisor.json"), supervisorRecord{Version: 1, RepairRevision: d.RepairRevision}, nil) == nil, "cannot seed the host's record")
				record, _ = os.ReadFile(c.path("service", "supervisor.json"))
				if row == "load" {
					leaseTaken(t, holder, c.path("service", "supervisor.json"))
				}
			})
			deliver := func(command serviceCommand, text string) serviceReply {
				reply := make(chan serviceReply, 1)
				command.reply = reply
				select {
				case w.commands <- command:
				case <-time.After(w.cell.cfg.Timeout):
					t.Fatal(text)
				}
				select {
				case got := <-reply:
					return got
				case <-time.After(w.cell.cfg.Timeout):
					t.Fatal(text)
				}
				return serviceReply{}
			}
			var settled ServiceStatus
			events := 0
			if row != "load" {
				settled, events = w.ready(t), len(w.recorded())
				leaseTaken(t, holder, w.cell.path("run", "worker.json"))
			}
			switch row {
			case "tick":
				w.within(t, nil, w.ticks, "the host did not take the refresh tick")
			case "status":
				client, cancel := context.WithCancel(t.Context())
				broke := make(chan time.Time, 1)
				go func() { at, _ := leaseContextBroken(holder); cancel(); broke <- at }()
				got := deliver(serviceCommand{ctx: client, action: "status"}, "the host did not answer the delivered status")
				returned, broken := time.Now(), holder.broken()
				holder.close()
				requireServiceLease(t, broken, "the delivered status never broke the refresh lease")
				requireServiceLease(t, returned.Sub(<-broke) < leaseWait.ceiling/2, "the delivered status waited to the lease ceiling after its client ended")
				requireServiceLease(t, errors.As(got.err, new(*serviceControlClientCancellation)), "the abandoned status lost its client cancellation")
				requireServiceLease(t, slices.Equal(got.status.Processes, settled.Processes), "the abandoned status replied with a new process list")
				observed := deliver(serviceCommand{ctx: t.Context(), action: "observe"}, "the host did not answer the observation")
				requireServiceLease(t, slices.Equal(observed.status.Processes, settled.Processes), "the host lost its last coherent snapshot")
				return
			}
			at, broken := leaseContextBroken(holder)
			w.cancel()
			err := <-w.done
			returned := time.Since(at)
			holder.close()
			requireServiceLease(t, broken, "Run never broke the selected lease")
			requireServiceLease(t, err == nil, "Run's own cut of its records failed the host")
			requireServiceLease(t, returned < leaseWait.ceiling/2, "Run waited to the lease ceiling after cancellation")
			after, readErr := os.ReadFile(w.cell.path("service", "supervisor.json"))
			requireServiceLease(t, readErr == nil && (row != "load" || bytes.Equal(after, record)), "a host cut at its record load wrote its record")
			want := map[string][]string{"load": nil, "tick": {"health", "repair", "stop"}}[row]
			requireServiceLease(t, slices.Equal(w.recorded()[events:], want), "a cut record read changed the host's hook calls")
		})
	}
}

// The nil that Run's own cut earns at its record load reaches no other error:
// a record Run refuses fails the host at supervisor_load with its fixed text,
// with no hook called and the record's bytes as planted, as at the parent.
func TestSupervisorRecordsRunRefusal(t *testing.T) {
	const planted = `{"version":2}`
	w := serviceWorkerConfigured(t, "reconcile", false, func(c *Cell, _ ServiceDescriptor) {
		requireServiceLease(t, os.WriteFile(c.path("service", "supervisor.json"), []byte(planted), 0o600) == nil, "cannot plant the refused record")
	})
	var err error
	select {
	case err = <-w.done:
	case <-time.After(w.cell.cfg.Timeout):
		t.Fatal("the host did not return its record refusal")
	}
	var located *serviceFailure
	requireServiceLease(t, errors.As(err, &located) && located.site == "supervisor_load", "Run lost the site of its record refusal")
	requireServiceLease(t, err != nil && err.Error() == "localcell: invalid_supervisor_record", "Run lost its record refusal")
	requireServiceLease(t, !serviceLeaseCut(err), "Run reported its record refusal as a cut")
	after, readErr := os.ReadFile(w.cell.path("service", "supervisor.json"))
	requireServiceLease(t, readErr == nil && string(after) == planted, "a host refused at its record load wrote its record")
	requireServiceLease(t, len(w.recorded()) == 0, "a host refused at its record load called a hook")
}

// Regression pins of behaviour the parent already had: Run's descriptor read
// and its control secret read (ahead of the control socket) keep Run's own cut
// a clean stop. Cancelled at the kernel's break, Run returns nil within half
// the ceiling and calls no tree hook; live, the lease kept to the ceiling keeps
// the parent's refusal and site. No read below either guard joins errors or
// derives a deadline, so no reachable error tells the selected-cut test both
// guards now call (LC-LEASE-2E4, part 3) from the parent's whole-error test:
// these rows pass at the parent too, and the guards' new form is held by the
// source pins alone. The joined leaves show the helper itself on a joined error
// whose sibling carries Run's cause, live and ended; they do not run a guard.
func TestRunEarlyGuardsLease(t *testing.T) {
	for _, row := range []struct{ name, file, site string }{{"descriptor", "service/descriptor.json", "initial_descriptor"}, {"control", "service/control.secret", "control_secret"}} {
		for _, mode := range []string{"cancelled", "live"} {
			t.Run(row.name+"/"+mode, func(t *testing.T) {
				c, _ := descriptorTestCell(t)
				h, _, starts, stops, _ := supervisorTestHooks()
				boots, repairs := 0, 0
				h.boot = func() (string, error) { boots++; return serviceBootID() }
				h.repair = func(context.Context) error { repairs++; return nil }
				h.reconcile = h.repair
				holder := leaseTaken(t, leaseHolder(t, "kept", unix.F_WRLCK), c.path(row.file))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				broken := make(chan time.Time, 1)
				if mode == "cancelled" {
					leaseWait.ceiling = 2 * time.Second
					go func() { at, _ := leaseContextBroken(holder); cancel(); broken <- at }()
				}
				err := c.Run(ctx, SupervisorPolicy{hooks: &h})
				returned := time.Now()
				requireServiceLease(t, holder.broken(), "Run never opened its leased early read")
				holder.close()
				if located := (*serviceFailure)(nil); mode == "cancelled" {
					requireServiceLease(t, err == nil, "Run did not stop cleanly at its own early cut")
					requireServiceLease(t, returned.Sub(<-broken) < leaseWait.ceiling/2, "Run waited past half the lease ceiling after its stop")
				} else {
					requireServiceLease(t, errors.As(err, &located) && located.site == row.site, "live Run lost its early refusal")
					requireServiceLease(t, !serviceLeaseCut(err), "live Run reported a cut")
				}
				requireServiceLease(t, boots == map[string]int{"descriptor": 0, "control": 1}[row.name], "Run's early cut changed its boot read")
				requireServiceLease(t, repairs+*starts+*stops == 0, "Run's early cut called a tree hook")
			})
		}
	}
	for _, phase := range []string{"live", "ended"} {
		t.Run("joined/"+phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if phase == "ended" {
				cancel()
			}
			foreign := errors.Join(&leaseWaitCut{cause: context.DeadlineExceeded}, context.Canceled)
			requireServiceLease(t, !serviceContextLeaseCut(ctx, foreign), "Run's early guards took another context's cut for their own")
			own := errors.Join(&leaseWaitCut{cause: context.Canceled}, errors.New("synthetic sibling"))
			requireServiceLease(t, serviceContextLeaseCut(ctx, own) == (phase == "ended"), "Run's early guards lost their own cut")
		})
	}
}
