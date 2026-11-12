package localcell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type serviceMaintenanceHostFixture struct {
	cell                            *Cell
	d                               ServiceDescriptor
	s                               *supervisor
	now                             *time.Time
	starts, stops, healths, repairs *int
}

func newServiceMaintenanceHostFixture(t *testing.T) serviceMaintenanceHostFixture {
	t.Helper()
	c, d := serviceMaintenanceCell(t, true)
	h, now, starts, stops, _ := supervisorTestHooks()
	healths, repairs := 0, 0
	h.health = func(context.Context) error { healths++; return nil }
	h.repair = func(context.Context) error { repairs++; return nil }
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if s.load(d) != nil {
		t.Fatal("Host fixture could not load supervisor")
	}
	return serviceMaintenanceHostFixture{c, d, s, now, starts, stops, &healths, &repairs}
}

func serviceMaintenanceHostPlant(t *testing.T, c *Cell, kind string) {
	t.Helper()
	serviceMaintenanceFence(t, c, kind)
	if kind != "Absent" {
		t.Cleanup(func() { _ = os.Remove(c.path("journal", "maintenance.json")) })
	}
}

func serviceMaintenanceHostClear(t *testing.T, c *Cell) {
	t.Helper()
	if os.Remove(c.path("journal", "maintenance.json")) != nil {
		t.Fatal("Host fixture could not remove its planted fence")
	}
}

func serviceMaintenanceHostSeed(t *testing.T, f serviceMaintenanceHostFixture, state string) supervisorRecord {
	t.Helper()
	q := supervisorRecord{Version: 1, RepairRevision: f.d.RepairRevision}
	switch state {
	case "idle":
	case "backoff_not_due":
		q.Attempts, q.Failures, q.NextStart = 3, []time.Time{f.now.Add(-time.Millisecond)}, f.now.Add(10*time.Millisecond)
	case "backoff_due":
		q.Attempts, q.Failures, q.NextStart = 3, []time.Time{f.now.Add(-time.Millisecond)}, *f.now
	case "clock_rollback":
		q.Attempts, q.Failures, q.NextStart = 3, []time.Time{f.now.Add(-time.Millisecond)}, f.now.Add(time.Hour)
	case "repair_revision":
		q.Attempts, q.Failures, q.NextStart = 3, []time.Time{f.now.Add(-time.Millisecond)}, f.now.Add(10*time.Millisecond)
		f.d.RepairRevision++
		if f.cell.writeServiceDescriptor(f.d) != nil {
			t.Fatal("Host fixture could not advance repair revision")
		}
	case "running":
		q.Attempts = 3
		f.s.running, f.s.status.Healthy = true, true
	case "stopped", "faulted":
		q.Attempts = 3
		if state == "stopped" {
			if f.cell.SetServiceDesiredState(t.Context(), "stopped") != nil {
				t.Fatal("Host fixture could not seed stopped intent")
			}
		} else if f.cell.setServiceFaulted(t.Context(), true) != nil {
			t.Fatal("Host fixture could not seed faulted intent")
		}
	default:
		t.Fatal("Host fixture received an unknown state")
	}
	f.s.record = q
	if f.s.save() != nil {
		t.Fatal("Host fixture could not seed persisted record")
	}
	q.Failures = slices.Clone(q.Failures)
	return q
}

func serviceMaintenanceHostHeld(t *testing.T, s *supervisor) {
	t.Helper()
	status, err := s.command(t.Context(), "observe")
	if err != nil || status.State != "maintenance" || status.Reason != "local_maintenance_fence_active" || status.Healthy || status.LastStepOutcome != "ok" {
		t.Fatal("Host completed snapshot did not report maintenance hold")
	}
}

func TestServiceMaintenanceHostHoldBudget(t *testing.T) {
	kinds := []string{"Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	for _, state := range []string{"idle", "backoff_not_due", "backoff_due", "clock_rollback", "repair_revision"} {
		for _, kind := range kinds {
			t.Run(state+"/"+kind, func(t *testing.T) {
				f := newServiceMaintenanceHostFixture(t)
				want := serviceMaintenanceHostSeed(t, f, state)
				serviceMaintenanceHostPlant(t, f.cell, kind)
				before := serviceMaintenanceSnapshot(t, f.cell)
				if f.s.step(t.Context()) != nil {
					t.Fatal("Held budget step returned an error")
				}
				if !reflect.DeepEqual(f.s.record, want) {
					t.Fatal("Held budget step changed restart record")
				}
				if *f.starts != 0 || *f.stops != 0 || *f.healths != 0 || *f.repairs != 0 {
					t.Fatal("Held budget step invoked a lifecycle hook")
				}
				serviceMaintenanceUnchanged(t, f.cell, before)
				serviceMaintenanceHostHeld(t, f.s)
			})
		}
	}
}

func TestServiceMaintenanceHostRunningHold(t *testing.T) {
	kinds := []string{"Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			f := newServiceMaintenanceHostFixture(t)
			want := serviceMaintenanceHostSeed(t, f, "running")
			known := ProcessRecord{PID: 23, StartTime: "known_running_identity", Root: f.d.Root, Component: "server"}
			if writeAtomic(f.cell.path("run", "server.json"), known, nil) != nil {
				t.Fatal("Running hold could not seed known identity")
			}
			f.s.rememberProcesses(f.d)
			processes := slices.Clone(f.s.status.Processes)
			serviceMaintenanceHostPlant(t, f.cell, kind)
			before := serviceMaintenanceSnapshot(t, f.cell)
			if f.s.step(t.Context()) != nil || !f.s.running || !reflect.DeepEqual(want, f.s.record) {
				t.Fatal("Running hold changed the known running tree or budget")
			}
			if *f.healths != 0 || *f.starts != 0 || *f.stops != 0 {
				t.Fatal("Running hold performed health or lifecycle work")
			}
			status, err := f.s.command(t.Context(), "observe")
			if err != nil || !reflect.DeepEqual(status.Processes, processes) {
				t.Fatal("Running hold discarded known process identities")
			}
			serviceMaintenanceHostHeld(t, f.s)
			serviceMaintenanceUnchanged(t, f.cell, before)
		})
	}
}

func TestServiceMaintenanceHostRepeatedHold(t *testing.T) {
	kinds := []string{"Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	for _, state := range []string{"idle", "running"} {
		for _, kind := range kinds {
			t.Run(state+"/"+kind, func(t *testing.T) {
				f := newServiceMaintenanceHostFixture(t)
				want := serviceMaintenanceHostSeed(t, f, state)
				serviceMaintenanceHostPlant(t, f.cell, kind)
				if f.s.step(t.Context()) != nil {
					t.Fatal("Repeated hold could not complete initial classification")
				}
				before := serviceMaintenanceSnapshot(t, f.cell)
				for range 6 {
					*f.now = f.now.Add(time.Hour)
					if f.s.tick(t.Context()) != nil {
						t.Fatal("Repeated hold tick returned an error")
					}
					serviceMaintenanceHostHeld(t, f.s)
				}
				if !reflect.DeepEqual(f.s.record, want) || *f.starts != 0 || *f.stops != 0 || *f.healths != 0 {
					t.Fatal("Repeated hold spent budget or invoked lifecycle hooks")
				}
				if f.s.running != (state == "running") {
					t.Fatal("Repeated hold changed the raw running flag")
				}
				serviceMaintenanceUnchanged(t, f.cell, before)
			})
		}
	}
}

func TestServiceMaintenanceHostReleaseParity(t *testing.T) {
	for _, state := range []string{"idle", "backoff_not_due", "backoff_due", "running", "stopped", "faulted", "clock_rollback", "repair_revision"} {
		t.Run(state, func(t *testing.T) {
			f := newServiceMaintenanceHostFixture(t)
			want := serviceMaintenanceHostSeed(t, f, state)
			serviceMaintenanceHostPlant(t, f.cell, "Valid")
			before := serviceMaintenanceSnapshot(t, f.cell)
			if f.s.step(t.Context()) != nil || !reflect.DeepEqual(want, f.s.record) {
				t.Fatal("Release fixture changed record before resumption")
			}
			serviceMaintenanceUnchanged(t, f.cell, before)
			serviceMaintenanceHostClear(t, f.cell)
			if f.s.tick(t.Context()) != nil {
				t.Fatal("Release tick failed to resume supervision")
			}
			wantStarts, wantHealths := 0, 0
			switch state {
			case "idle", "backoff_due":
				want.Attempts++
				wantStarts, wantHealths = 1, 1
			case "running":
				wantHealths = 1
			case "clock_rollback":
				want.NextStart = f.now.Add(f.s.hooks.maxBackoff)
			case "repair_revision":
				want = supervisorRecord{Version: 1, RepairRevision: f.d.RepairRevision + 1, Attempts: 1}
				wantStarts, wantHealths = 1, 1
			}
			if !reflect.DeepEqual(want, f.s.record) || *f.starts != wantStarts || *f.healths != wantHealths || *f.stops != 0 {
				t.Fatal("Release tick changed legacy record or hook effects")
			}
			serviceMaintenanceParityFile(t, f.cell, "service/supervisor.json", serviceMaintenanceParityJSON(t, want))
			status, err := f.s.command(t.Context(), "observe")
			if err != nil || status.State == "maintenance" || status.Reason == "local_maintenance_fence_active" {
				t.Fatal("Release tick retained a stale maintenance projection")
			}
		})
	}
}

func TestServiceMaintenanceHostLateStartRestore(t *testing.T) {
	kinds := []string{"Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	for _, shape := range []string{"zero_nil", "zero_empty", "seeded_nil", "seeded_nonempty"} {
		for _, kind := range kinds {
			t.Run(shape+"/"+kind, func(t *testing.T) {
				f := newServiceMaintenanceHostFixture(t)
				want := supervisorRecord{Version: 1, RepairRevision: f.d.RepairRevision}
				if shape == "zero_empty" {
					want.Failures = []time.Time{}
				}
				if shape == "seeded_nil" || shape == "seeded_nonempty" {
					want.Attempts, want.NextStart = 4, f.now.Add(-time.Second)
				}
				if shape == "seeded_nonempty" {
					want.Failures = []time.Time{f.now.Add(-time.Minute)}
				}
				f.s.record = want
				if f.s.save() != nil {
					t.Fatal("Late start could not seed prior record")
				}
				before := serviceMaintenanceSnapshot(t, f.cell)
				starts := 0
				f.s.hooks.start = func(ctx context.Context) error {
					starts++
					provisional := want
					provisional.Attempts++
					serviceMaintenanceParityFile(t, f.cell, "service/supervisor.json", serviceMaintenanceParityJSON(t, provisional))
					serviceMaintenanceHostPlant(t, f.cell, kind)
					return f.cell.Start(ctx)
				}
				if f.s.step(t.Context()) != nil || starts != 1 {
					t.Fatal("Late start refusal did not complete exactly one owner call")
				}
				if !reflect.DeepEqual(f.s.record, want) {
					t.Fatal("Late start refusal did not restore the whole prior record")
				}
				if *f.stops != 0 || *f.healths != 0 {
					t.Fatal("Late start refusal invoked cleanup or provisioning health")
				}
				serviceMaintenanceHostHeld(t, f.s)
				serviceMaintenanceHostClear(t, f.cell)
				serviceMaintenanceUnchanged(t, f.cell, before)
			})
		}
	}
	t.Run("restore_save_error", func(t *testing.T) {
		f := newServiceMaintenanceHostFixture(t)
		f.s.hooks.start = func(ctx context.Context) error {
			serviceMaintenanceHostPlant(t, f.cell, "Valid")
			if os.Remove(f.cell.path("service", "supervisor.json")) != nil || testMkdir(f.cell.path("service", "supervisor.json"), 0o700) != nil {
				t.Fatal("Restore error could not obstruct the second save")
			}
			return f.cell.Start(ctx)
		}
		err := f.s.step(t.Context())
		var located *serviceFailure
		if err == nil || !errors.As(err, &located) || located.site != "supervisor_save" {
			t.Fatal("Restore save failure was swallowed as a maintenance hold")
		}
		status, _ := f.s.command(t.Context(), "observe")
		if status.State == "maintenance" || status.LastStepOutcome != "failed" || *f.stops != 0 {
			t.Fatal("Restore save failure published a successful hold or cleanup")
		}
	})
}

func TestServiceMaintenanceHostRollbackCrashWindow(t *testing.T) {
	for _, image := range []string{"first_save", "restored"} {
		t.Run(image, func(t *testing.T) {
			f := newServiceMaintenanceHostFixture(t)
			want := supervisorRecord{Version: 1, RepairRevision: f.d.RepairRevision, Attempts: 4, Failures: []time.Time{f.now.Add(-time.Minute)}, NextStart: f.now.Add(time.Second)}
			if image == "first_save" {
				want.Attempts++
			}
			if writeAtomic(f.cell.path("service", "supervisor.json"), want, nil) != nil {
				t.Fatal("Crash image could not persist a supervisor record")
			}
			before := serviceMaintenanceSnapshot(t, f.cell)
			if f.s.load(f.d) != nil || !reflect.DeepEqual(f.s.record, want) {
				t.Fatal("Crash image invented attempt recovery or failure bookkeeping")
			}
			serviceMaintenanceUnchanged(t, f.cell, before)
		})
	}
}

// This fixture records this test process only for read-only ServiceHealth.
// It never enters a signaling or lifecycle cleanup path. TempDir removes files.
func serviceMaintenanceHostHealthCell(t *testing.T) (*Cell, ServiceDescriptor) {
	t.Helper()
	c, d := serviceMaintenanceCell(t, true)
	identity, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		t.Fatal("Health fixture could not read its current process identity")
	}
	boot := serviceMaintenanceNativeBoot(t)
	for _, component := range []string{"postgres", "server", "worker"} {
		record := ProcessRecord{PID: os.Getpid(), StartTime: identity, BootID: boot, Root: c.cfg.Root, Component: component}
		if writeAtomic(c.path("run", component+".json"), record, nil) != nil {
			t.Fatal("Health fixture could not seed matching component identity")
		}
	}
	if privateDir(c.path("pg"), true) != nil || writeAtomicBytes(c.path("pg", "postmaster.pid"), []byte(fmt.Sprintf("%d\n%s\n1\n", os.Getpid(), c.path("pg"))), nil) != nil {
		t.Fatal("Health fixture could not seed matching postmaster identity")
	}
	m, err := readManifest(c.cfg.Root)
	if err != nil {
		t.Fatal("Health fixture could not read its owned manifest")
	}
	m.AccountID = "health_fixture_account"
	if writeAtomic(c.path("manifest.json"), m, nil) != nil || c.serviceLiveTree() != nil {
		t.Fatal("Health fixture did not establish an eligible live tree")
	}
	return c, d
}

func TestServiceMaintenanceHostHealthAdmission(t *testing.T) {
	kinds := []string{"Absent", "Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	for _, kind := range kinds {
		t.Run("eligible/"+kind, func(t *testing.T) {
			c, _ := serviceMaintenanceHostHealthCell(t)
			serviceMaintenanceHostPlant(t, c, kind)
			before := serviceMaintenanceSnapshot(t, c)
			err := c.ServiceHealth(t.Context())
			if kind == "Absent" {
				if !errors.Is(err, errServiceHealth) || errors.Is(err, errMaintenanceFenceActive) {
					t.Fatal("Absent health did not reach the missing principal boundary")
				}
			} else if !errors.Is(err, errMaintenanceFenceActive) || errors.Is(err, errServiceHealth) {
				t.Fatal("Eligible health reached provisioning before maintenance admission")
			}
			var located *serviceFailure
			if !errors.As(err, &located) || located.site != "health_tree" {
				t.Fatal("Health admission changed its exact deferred failure site")
			}
			serviceMaintenanceUnchanged(t, c, before)
			serviceMaintenanceReleased(t, c)
		})
	}
	for _, earlier := range []string{"lock", "descriptor", "manifest", "intent", "fault", "transition", "tree", "account"} {
		t.Run("earlier/"+earlier, func(t *testing.T) {
			c, d := serviceMaintenanceHostHealthCell(t)
			switch earlier {
			case "lock":
				lock, err := c.own(false)
				if err != nil {
					t.Fatal("Health precedence could not acquire contender")
				}
				defer func() { _ = lock.Close() }()
			case "descriptor":
				if writeAtomicBytes(c.path("service", "descriptor.json"), []byte("broken"), nil) != nil {
					t.Fatal("Health precedence could not corrupt descriptor")
				}
			case "manifest":
				if writeAtomicBytes(c.path("manifest.json"), []byte("broken"), nil) != nil {
					t.Fatal("Health precedence could not corrupt manifest")
				}
			case "intent":
				d.DesiredState = "stopped"
				if c.writeServiceDescriptor(d) != nil {
					t.Fatal("Health precedence could not seed stopped intent")
				}
			case "fault":
				d.Faulted = true
				if c.writeServiceDescriptor(d) != nil {
					t.Fatal("Health precedence could not seed existing fault")
				}
			case "transition":
				if writeAtomic(c.path("service", "transition.json"), d, nil) != nil {
					t.Fatal("Health precedence could not seed pending transition")
				}
			case "tree":
				if os.Remove(c.path("run", "worker.json")) != nil {
					t.Fatal("Health precedence could not remove a component record")
				}
			case "account":
				m, err := readManifest(c.cfg.Root)
				if err != nil {
					t.Fatal("Health precedence could not read account fixture")
				}
				m.AccountID = ""
				if writeAtomic(c.path("manifest.json"), m, nil) != nil {
					t.Fatal("Health precedence could not remove account identity")
				}
			}
			serviceMaintenanceHostPlant(t, c, "Valid")
			err := c.ServiceHealth(t.Context())
			if err == nil || errors.Is(err, errMaintenanceFenceActive) {
				t.Fatal("Maintenance admission masked an earlier health refusal")
			}
			if earlier == "lock" && !errors.Is(err, ErrLocked) {
				t.Fatal("Health admission changed earlier lock contention")
			}
		})
	}
}

func TestServiceMaintenanceHostHealthRefusalHolds(t *testing.T) {
	kinds := []string{"Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	for _, branch := range []string{"running", "post_start"} {
		for _, kind := range kinds {
			t.Run(branch+"/"+kind, func(t *testing.T) {
				f := newServiceMaintenanceHostFixture(t)
				seed := serviceMaintenanceHostSeed(t, f, "idle")
				if branch == "running" {
					seed = serviceMaintenanceHostSeed(t, f, "running")
				}
				f.s.status.Healthy = true
				healths := 0
				f.s.hooks.health = func(ctx context.Context) error {
					healths++
					serviceMaintenanceHostPlant(t, f.cell, kind)
					return serviceFailureAt(f.cell.serviceMaintenanceObservation(ctx, nil), "health_tree")
				}
				status, err := f.s.command(t.Context(), "health")
				if !errors.Is(err, errServiceUnhealthy) || status.Healthy || status.State != "maintenance" {
					t.Fatal("Health refusal returned a stale healthy completion")
				}
				if branch == "post_start" {
					seed.Attempts++
				}
				if !reflect.DeepEqual(f.s.record, seed) || healths != 1 || *f.stops != 0 || !f.s.running {
					t.Fatal("Health refusal stopped the tree or changed failure budget")
				}
				serviceMaintenanceParityFile(t, f.cell, "service/supervisor.json", serviceMaintenanceParityJSON(t, seed))
			})
		}
	}
}

func TestServiceMaintenanceHostUnhealthyStopThenHold(t *testing.T) {
	for _, origin := range []string{"running_health", "start", "post_start_health"} {
		for _, cleanup := range []string{"success", "locked", "failure", "cancellation"} {
			t.Run(origin+"/"+cleanup, func(t *testing.T) {
				f := newServiceMaintenanceHostFixture(t)
				seed := serviceMaintenanceHostSeed(t, f, "idle")
				if origin == "running_health" {
					seed = serviceMaintenanceHostSeed(t, f, "running")
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				fail := func(context.Context) error {
					serviceMaintenanceHostPlant(t, f.cell, "Valid")
					return errors.New("synthetic unhealthy admission")
				}
				if origin == "start" {
					f.s.hooks.start = fail
				} else {
					f.s.hooks.health = fail
				}
				stops := 0
				f.s.hooks.stop = func(opCtx context.Context) error {
					stops++
					switch cleanup {
					case "locked":
						return ErrLocked
					case "failure":
						return errors.New("synthetic cleanup refusal")
					case "cancellation":
						cancel()
						return opCtx.Err()
					default:
						return f.cell.stopServiceChildren(opCtx)
					}
				}
				status, err := f.s.command(ctx, "status")
				if stops != 1 {
					t.Fatal("Unhealthy cleanup did not call protective stop exactly once")
				}
				if origin != "running_health" {
					seed.Attempts++
				}
				if !reflect.DeepEqual(f.s.record, seed) {
					t.Fatal("Unhealthy cleanup charged failure budget during maintenance")
				}
				switch cleanup {
				case "success":
					if err != nil || status.State != "maintenance" || f.s.running {
						t.Fatal("Successful protective cleanup did not enter a stopped hold")
					}
					serviceMaintenanceStopJournal(t, f.cell)
				case "locked":
					if err != nil || status.State != "busy" {
						t.Fatal("Contended protective cleanup lost legacy busy completion")
					}
				case "failure":
					d, readErr := f.cell.ServiceDescriptor()
					if err != nil || readErr != nil || !d.Faulted || f.s.status.State != "faulted" {
						t.Fatal("Failed protective cleanup lost the durable safety fault")
					}
				case "cancellation":
					if !errors.Is(err, context.Canceled) {
						t.Fatal("Canceled protective cleanup lost caller cancellation")
					}
				}
			})
		}
	}
}

func serviceMaintenanceHostExitContext(t *testing.T, c *Cell, d ServiceDescriptor) context.Context {
	t.Helper()
	lease, err := c.acquireServiceLease(d)
	if err != nil {
		t.Fatal("Safety exit fixture could not acquire host lease")
	}
	t.Cleanup(func() { _ = lease.Close() })
	envelope, _, err := c.newHostExitEnvelope()
	if err != nil || c.validateHostExitEnvelope(envelope) != nil {
		t.Fatal("Safety exit fixture could not establish valid envelope")
	}
	return context.WithValue(t.Context(), serviceHostExitKey{}, envelope)
}

func TestServiceMaintenanceHostProtectiveDrain(t *testing.T) {
	for _, state := range []string{"stopped", "maintenance", "faulted"} {
		for _, branch := range []string{"drain", "retry"} {
			t.Run(state+"/"+branch, func(t *testing.T) {
				f := newServiceMaintenanceHostFixture(t)
				seed := serviceMaintenanceHostSeed(t, f, "running")
				if state == "faulted" {
					if f.cell.setServiceFaulted(t.Context(), true) != nil {
						t.Fatal("Protective drain could not seed durable fault")
					}
				} else if f.cell.SetServiceDesiredState(t.Context(), state) != nil {
					t.Fatal("Protective drain could not seed protective intent")
				}
				serviceMaintenanceHostPlant(t, f.cell, "Valid")
				stops := 0
				f.s.hooks.stop = func(ctx context.Context) error {
					stops++
					if branch == "retry" && stops == 1 {
						return ErrLocked
					}
					return f.cell.stopServiceChildren(ctx)
				}
				if f.s.step(t.Context()) != nil {
					t.Fatal("Protective drain step returned an error")
				}
				if branch == "retry" {
					status, _ := f.s.command(t.Context(), "observe")
					if status.State != "busy" || !f.s.running || stops != 1 {
						t.Fatal("Protective drain contention discarded pending tree")
					}
					if f.s.step(t.Context()) != nil {
						t.Fatal("Protective drain could not retry after contention")
					}
				}
				if f.s.running || !reflect.DeepEqual(f.s.record, seed) || *f.starts != 0 || *f.healths != 0 {
					t.Fatal("Protective hold stranded a tree or changed restart budget")
				}
				serviceMaintenanceHostHeld(t, f.s)
				serviceMaintenanceStopJournal(t, f.cell)
				if state == "faulted" {
					ctx := serviceMaintenanceHostExitContext(t, f.cell, f.d)
					_, err := f.s.command(ctx, "exit-host")
					if err == nil || f.s.status.State != "faulted" {
						t.Fatal("Maintenance projection concealed raw fault from host exit")
					}
				}
			})
		}
	}
	for _, seedState := range []string{"repair_revision", "clock_rollback"} {
		for _, kind := range []string{"Absent", "Valid"} {
			t.Run("explicit_stop/"+seedState+"/"+kind, func(t *testing.T) {
				f := newServiceMaintenanceHostFixture(t)
				seed := serviceMaintenanceHostSeed(t, f, seedState)
				serviceMaintenanceHostPlant(t, f.cell, kind)
				status, err := f.s.command(t.Context(), "stop")
				if err != nil || status.State != "stopped" || status.DesiredState != "stopped" || status.Healthy || *f.stops != 1 {
					t.Fatal("Explicit stop lost its legacy stopped completion")
				}
				if seedState == "repair_revision" {
					seed = supervisorRecord{Version: 1, RepairRevision: f.d.RepairRevision + 1}
				} else {
					seed.NextStart = f.now.Add(f.s.hooks.maxBackoff)
				}
				if !reflect.DeepEqual(seed, f.s.record) {
					t.Fatal("Explicit stop suppressed legacy record normalization")
				}
				serviceMaintenanceParityFile(t, f.cell, "service/supervisor.json", serviceMaintenanceParityJSON(t, seed))
			})
		}
	}
}

func TestServiceMaintenanceHostProtectiveCircuit(t *testing.T) {
	for _, stage := range []string{"crash_recovered", "initial_faulted"} {
		t.Run(stage, func(t *testing.T) {
			f := newServiceMaintenanceHostFixture(t)
			seed := supervisorRecord{Version: 1, Attempts: 5, RepairRevision: f.d.RepairRevision}
			if stage == "crash_recovered" {
				for range 5 {
					seed.Failures = append(seed.Failures, *f.now)
				}
			} else {
				if f.cell.setServiceFaulted(t.Context(), true) != nil {
					t.Fatal("Protective circuit could not seed initial durable fault")
				}
				f.d.Faulted = true
			}
			f.s.record = seed
			if f.s.save() != nil || f.s.load(f.d) != nil {
				t.Fatal("Protective circuit could not load seed record")
			}
			serviceMaintenanceHostPlant(t, f.cell, "Valid")
			if stage == "initial_faulted" {
				f.s.hooks.reconcile = f.cell.reconcileServiceBoot
				f.s.pendingStartup = serviceStartupReconcile
				if f.s.resumeServiceStartup(t.Context()) != nil {
					t.Fatal("Initial faulted startup could not enter maintenance hold")
				}
			} else if f.s.step(t.Context()) != nil {
				t.Fatal("Protective circuit could not complete durable fault")
			}
			d, err := f.cell.ServiceDescriptor()
			if err != nil || !d.Faulted || !reflect.DeepEqual(f.s.record, seed) || *f.starts != 0 || *f.healths != 0 {
				t.Fatal("Maintenance hold lost a real circuit fault or charged another failure")
			}
			if f.s.status.State != "faulted" {
				t.Fatal("Protective circuit did not retain raw faulted safety status")
			}
			serviceMaintenanceHostHeld(t, f.s)
			ctx := serviceMaintenanceHostExitContext(t, f.cell, d)
			if _, err := f.s.command(ctx, "exit-host"); err == nil {
				t.Fatal("Protective circuit maintenance projection admitted host exit")
			}
		})
	}
}

func TestServiceMaintenanceHostShutdownLatch(t *testing.T) {
	kinds := []string{"Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	for _, state := range []string{"pending", "completed"} {
		for _, kind := range kinds {
			t.Run(state+"/"+kind, func(t *testing.T) {
				f := newServiceMaintenanceHostFixture(t)
				f.s.hooks.windowsShutdownPolicy = true
				f.s.running, f.s.shutdownBlocked = true, true
				f.s.shutdownFaultPending = state == "pending"
				if state == "completed" && f.cell.setServiceFaulted(t.Context(), true) != nil {
					t.Fatal("Shutdown latch could not seed completed fault")
				}
				serviceMaintenanceHostPlant(t, f.cell, kind)
				if f.s.step(t.Context()) != nil || !f.s.shutdownBlocked || f.s.shutdownFaultPending {
					t.Fatal("Maintenance hold did not preserve and settle shutdown latch")
				}
				d, err := f.cell.ServiceDescriptor()
				if err != nil || !d.Faulted || f.s.status.State != "faulted" {
					t.Fatal("Shutdown latch lost its durable raw fault")
				}
				for _, action := range []string{"stop", "start", "exit-host", "repair"} {
					if _, err := f.s.command(t.Context(), action); err == nil {
						t.Fatal("Held shutdown latch admitted a refused command")
					}
				}
				if !f.s.shutdownBlocked || *f.starts != 0 || *f.stops != 0 || *f.healths != 0 {
					t.Fatal("Held shutdown latch reopened force or lifecycle work")
				}
				serviceMaintenanceHostClear(t, f.cell)
				if f.s.step(t.Context()) != nil || !f.s.shutdownBlocked || *f.starts != 0 || *f.stops != 0 || *f.healths != 0 {
					t.Fatal("Fence release reopened the shutdown latch")
				}
			})
		}
	}
}

func TestServiceMaintenanceHostClassificationContention(t *testing.T) {
	t.Run("held_busy_held", func(t *testing.T) {
		f := newServiceMaintenanceHostFixture(t)
		seed := serviceMaintenanceHostSeed(t, f, "idle")
		serviceMaintenanceHostPlant(t, f.cell, "Valid")
		if f.s.step(t.Context()) != nil {
			t.Fatal("Contention fixture could not enter initial hold")
		}
		lock, err := f.cell.own(false)
		if err != nil {
			t.Fatal("Contention fixture could not acquire operation lock")
		}
		defer func() { _ = lock.Close() }()
		before := serviceMaintenanceSnapshot(t, f.cell)
		status, err := f.s.command(t.Context(), "health")
		if !errors.Is(err, errServiceControlDeferred) || status.State != "busy" || status.Healthy || !reflect.DeepEqual(seed, f.s.record) {
			t.Fatal("Contended held classifier did not report deferred busy")
		}
		if !f.s.maintenanceHeld || *f.starts != 0 || *f.stops != 0 || *f.healths != 0 {
			t.Fatal("Contended classifier erased fence knowledge or ran hooks")
		}
		serviceMaintenanceUnchanged(t, f.cell, before)
		if lock.Close() != nil {
			t.Fatal("Contention fixture could not release operation lock")
		}
		status, err = f.s.command(t.Context(), "health")
		if !errors.Is(err, errServiceUnhealthy) || status.State != "maintenance" || status.Healthy {
			t.Fatal("Released classifier failed to restore held health completion")
		}
	})
	for _, outcome := range []string{"absent", "maintenance", "descriptor_error"} {
		t.Run("release/"+outcome, func(t *testing.T) {
			f := newServiceMaintenanceHostFixture(t)
			if outcome == "maintenance" {
				serviceMaintenanceHostPlant(t, f.cell, "Valid")
			}
			if outcome == "descriptor_error" && os.Remove(f.cell.path("service", "descriptor.json")) != nil {
				t.Fatal("Classifier release could not remove descriptor")
			}
			err := f.cell.serviceMaintenanceObservation(t.Context(), nil)
			if outcome == "absent" && err != nil || outcome == "maintenance" && !errors.Is(err, errMaintenanceFenceActive) || outcome == "descriptor_error" && (err == nil || errors.Is(err, errMaintenanceFenceActive)) {
				t.Fatal("Classifier release changed its legacy result identity")
			}
			serviceMaintenanceReleased(t, f.cell)
			if outcome == "descriptor_error" && f.cell.writeServiceDescriptor(f.d) != nil {
				t.Fatal("Classifier release could not restore descriptor")
			}
			if outcome != "maintenance" {
				serviceMaintenanceHostPlant(t, f.cell, "Valid")
			}
			if !errors.Is(f.cell.Start(t.Context()), errMaintenanceFenceActive) || !errors.Is(f.cell.Repair(t.Context()), errMaintenanceFenceActive) {
				t.Fatal("Classifier retained ownership across public lifecycle owners")
			}
		})
	}
}

// Value is first queried by the caller cancellation check after the observer
// has returned and released its operation lock. It cancels that exact boundary.
type serviceMaintenanceHostObservationContext struct {
	context.Context
	cancel context.CancelFunc
}

func (c *serviceMaintenanceHostObservationContext) Value(key any) any {
	if _, ok := key.(serviceControlCommandKey); ok {
		c.cancel()
		return true
	}
	return c.Context.Value(key)
}

func TestServiceMaintenanceHostCancellation(t *testing.T) {
	t.Run("after_observation", func(t *testing.T) {
		f := newServiceMaintenanceHostFixture(t)
		serviceMaintenanceHostPlant(t, f.cell, "Valid")
		if f.s.step(t.Context()) != nil {
			t.Fatal("Observation cancellation could not seed completed hold")
		}
		previous, _ := f.s.command(t.Context(), "observe")
		serviceMaintenanceHostClear(t, f.cell)
		parent, cancel := context.WithCancel(t.Context())
		defer cancel()
		ctx := &serviceMaintenanceHostObservationContext{Context: parent, cancel: cancel}
		held, err := f.s.observeServiceMaintenance(ctx)
		var abandoned *serviceControlClientCancellation
		if held || !errors.As(err, &abandoned) || !errors.Is(err, context.Canceled) || !f.s.maintenanceHeld {
			t.Fatal("Observation return committed absence after caller cancellation")
		}
		after, _ := f.s.command(t.Context(), "observe")
		if !reflect.DeepEqual(previous, after) {
			t.Fatal("Observation cancellation replaced completed projection")
		}
	})
	for _, point := range []string{"before_observation", "before_attempt", "after_start", "running_health", "post_start_health"} {
		t.Run(point, func(t *testing.T) {
			f := newServiceMaintenanceHostFixture(t)
			seed := serviceMaintenanceHostSeed(t, f, "idle")
			if point == "running_health" {
				seed = serviceMaintenanceHostSeed(t, f, "running")
			}
			serviceMaintenanceHostPlant(t, f.cell, "Valid")
			if f.s.step(t.Context()) != nil {
				t.Fatal("Cancellation fixture could not enter completed hold")
			}
			previous, _ := f.s.command(t.Context(), "observe")
			serviceMaintenanceHostClear(t, f.cell)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch point {
			case "before_observation":
				cancel()
			case "before_attempt":
				clocks := 0
				f.s.hooks.now = func() time.Time {
					clocks++
					if clocks == 2 {
						cancel()
					}
					return *f.now
				}
			case "after_start":
				f.s.hooks.start = func(context.Context) error {
					serviceMaintenanceHostPlant(t, f.cell, "Valid")
					cancel()
					return errMaintenanceFenceActive
				}
			default:
				f.s.hooks.health = func(context.Context) error {
					serviceMaintenanceHostPlant(t, f.cell, "Valid")
					cancel()
					return errMaintenanceFenceActive
				}
			}
			status, err := f.s.command(ctx, "health")
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(status, previous) {
				t.Fatal("Canceled classification replaced the previous completed projection")
			}
			if point == "after_start" || point == "post_start_health" {
				seed.Attempts++
			}
			if !reflect.DeepEqual(seed, f.s.record) || *f.stops != 0 {
				t.Fatal("Canceled maintenance refusal rolled back real scheduling or ran cleanup")
			}
		})
	}
}

func TestServiceMaintenanceHostObservationBytes(t *testing.T) {
	f := newServiceMaintenanceHostFixture(t)
	seed := serviceMaintenanceHostSeed(t, f, "running")
	f.s.status.HostExit = "refused"
	known := ProcessRecord{PID: 23, StartTime: "known_identity", Root: f.d.Root, Component: "server"}
	if writeAtomic(f.cell.path("run", "server.json"), known, nil) != nil {
		t.Fatal("Observation bytes could not seed known identity")
	}
	serviceMaintenanceHostPlant(t, f.cell, "Valid")
	before := serviceMaintenanceSnapshot(t, f.cell)
	if f.s.step(t.Context()) != nil {
		t.Fatal("Observation bytes could not complete held classification")
	}
	want := ServiceStatus{DesiredState: "running", State: "maintenance", Reason: "local_maintenance_fence_active", Healthy: false, Restarts: seed.Attempts - 1, Generation: f.d.Payload.Generation, HostExit: "refused", LastStepOutcome: "ok", Processes: []ServiceProcessIdentity{{Component: "server", PID: known.PID, StartTime: known.StartTime}}}
	status, err := f.s.command(t.Context(), "observe")
	gotJSON, gotErr := json.Marshal(status)
	wantJSON, wantErr := json.Marshal(want)
	if err != nil || gotErr != nil || wantErr != nil || string(gotJSON) != string(wantJSON) {
		t.Fatal("Observation bytes changed exact held fields or host exit refusal")
	}
	if f.s.status.State == "maintenance" {
		t.Fatal("Observation projection overwrote the raw safety state")
	}
	status.Processes[0].StartTime = "caller_change"
	if f.s.status.Processes[0].StartTime != known.StartTime {
		t.Fatal("Observation projection exposed mutable process cache storage")
	}
	serviceMaintenanceUnchanged(t, f.cell, before)
	serviceMaintenanceHostClear(t, f.cell)
	if os.Remove(f.cell.path("run", "server.json")) != nil {
		t.Fatal("Observation cache could not remove fixture identity")
	}
	cached, err := f.s.command(t.Context(), "observe")
	if err != nil || !reflect.DeepEqual(cached, want) || *f.healths != 0 || *f.starts != 0 || *f.stops != 0 {
		t.Fatal("Cached observation performed fresh reads or lifecycle work")
	}
}

// Each wait budget includes three independent local admission samples. Channels
// carry lifecycle progress; no sleep is used to order host and control work.
func serviceMaintenanceHostRunBound(t *testing.T, c *Cell) time.Duration {
	t.Helper()
	var measured time.Duration
	for range 3 {
		start := time.Now()
		if _, err := c.ServiceDescriptor(); err != nil {
			t.Fatal("Host bound could not sample descriptor admission")
		}
		if err := c.serviceMaintenanceObservation(t.Context(), nil); err != nil && !errors.Is(err, errMaintenanceFenceActive) {
			t.Fatal("Host bound could not sample maintenance observation")
		}
		measured = max(measured, time.Since(start))
	}
	c.cfg.Timeout = max(c.cfg.Timeout, 20*measured)
	return 8 * c.cfg.Timeout
}

func serviceMaintenanceHostStartup(t *testing.T, kind string, late bool) {
	t.Helper()
	c, d := serviceMaintenanceShortCell(t)
	if !late {
		serviceMaintenanceHostPlant(t, c, kind)
	}
	bound := serviceMaintenanceHostRunBound(t, c)
	before := serviceMaintenanceSnapshot(t, c)
	h, _, _, _, _ := supervisorTestHooks()
	h.ticks = make(chan time.Time)
	ctx, cancel := context.WithTimeout(t.Context(), bound)
	defer cancel()
	entered, done := make(chan struct{}), make(chan struct{})
	var enteredOnce sync.Once
	var reconciles, repairs, stops, starts, healths atomic.Int32
	h.reconcile = func() error {
		reconciles.Add(1)
		if !late {
			enteredOnce.Do(func() { close(entered) })
		}
		err := c.reconcileServiceBoot()
		if late && err == nil {
			serviceMaintenanceHostPlant(t, c, kind)
		}
		return err
	}
	h.repair = func(opCtx context.Context) error {
		repairs.Add(1)
		if late {
			enteredOnce.Do(func() { close(entered) })
		}
		return c.Repair(opCtx)
	}
	h.stop = func(context.Context) error { stops.Add(1); return nil }
	h.start = func(context.Context) error { starts.Add(1); return nil }
	h.health = func(context.Context) error { healths.Add(1); return nil }
	var runErr error
	go func() { runErr = c.Run(ctx, SupervisorPolicy{hooks: &h, ShutdownTimeout: c.cfg.Timeout}); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(bound):
			t.Error("Held host cleanup exceeded its sampled bound")
		}
	})
	select {
	case <-entered:
	case <-done:
		t.Fatal("Held startup exited before reaching its owner gate")
	case <-ctx.Done():
		t.Fatal("Held startup did not reach its owner gate within budget")
	}
	status, err := c.Control(t.Context(), "observe")
	if err != nil || status.State != "maintenance" || status.Healthy || status.LastStepOutcome != "ok" {
		t.Fatal("Held startup did not retain a responsive observation endpoint")
	}
	if lease, err := c.acquireServiceLease(d); err != ErrServiceHostActive {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatal("Held startup released its lifetime lease")
	}
	wantRepairs := int32(0)
	if late {
		wantRepairs = 1
	}
	if reconciles.Load() != 1 || repairs.Load() != wantRepairs || stops.Load() != 0 || starts.Load() != 0 || healths.Load() != 0 {
		t.Fatal("Held startup advanced or repeated an unfinished phase")
	}
	for _, path := range []string{"service/supervisor.json", "journal/operations.jsonl"} {
		if _, err := os.Lstat(c.path(path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("Held startup wrote supervisor budget or lifecycle journal")
		}
	}
	if late {
		boot := serviceMaintenanceNativeBoot(t)
		serviceMaintenanceParityFile(t, c, "service/boot.json", serviceMaintenanceParityJSON(t, serviceBootRecord{d.InstallationID, boot}))
	} else if _, err := os.Lstat(c.path("service", "boot.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Initial held startup published a boot record")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(bound):
		t.Fatal("Held startup cancellation failed to complete shutdown")
	}
	if runErr != nil || stops.Load() != 1 || repairs.Load() != wantRepairs+1 {
		t.Fatal("Held startup cancellation lost protective shutdown ordering")
	}
	serviceMaintenanceHostReleased(t, c)
	if late {
		serviceMaintenanceHostClear(t, c)
		serviceMaintenanceParityUnchanged(t, c, before, ".service-host.lock", "service/lease.json", "service/boot.json")
	} else {
		serviceMaintenanceParityUnchanged(t, c, before, ".service-host.lock", "service/lease.json")
	}
}

func TestServiceMaintenanceHostStartupHold(t *testing.T) {
	kinds := []string{"Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) { serviceMaintenanceHostStartup(t, kind, false) })
	}
}

func TestServiceMaintenanceHostStartupLateRepairHold(t *testing.T) {
	kinds := []string{"Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) { serviceMaintenanceHostStartup(t, kind, true) })
	}
}

func TestServiceMaintenanceHostStartupReleaseOrder(t *testing.T) {
	t.Run("pending_stop_contention", func(t *testing.T) {
		f := newServiceMaintenanceHostFixture(t)
		f.s.pendingStartup = serviceStartupStop
		serviceMaintenanceHostPlant(t, f.cell, "Valid")
		stops := 0
		f.s.hooks.stop = func(ctx context.Context) error {
			stops++
			if stops == 1 {
				return ErrLocked
			}
			return f.cell.stopServiceChildren(ctx)
		}
		if f.s.tick(t.Context()) != nil || f.s.pendingStartup != serviceStartupStop || stops != 1 {
			t.Fatal("Startup stop contention discarded its pending protective phase")
		}
		status, _ := f.s.command(t.Context(), "observe")
		if status.State != "busy" {
			t.Fatal("Startup stop contention did not publish a busy completion")
		}
		if f.s.tick(t.Context()) != nil || f.s.pendingStartup != serviceStartupNone || stops != 2 || *f.starts != 0 || *f.healths != 0 {
			t.Fatal("Startup protective stop did not retry during maintenance hold")
		}
		serviceMaintenanceHostHeld(t, f.s)
	})
	kinds := []string{"Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	phases := []struct {
		name  string
		phase serviceStartupPhase
	}{
		{"reconcile", serviceStartupReconcile},
		{"repair", serviceStartupRepair},
		{"stop", serviceStartupStop},
	}
	for _, phase := range phases {
		t.Run("replay/"+phase.name, func(t *testing.T) {
			f := newServiceMaintenanceHostFixture(t)
			if f.cell.SetServiceDesiredState(t.Context(), "stopped") != nil {
				t.Fatal("Startup replay could not seed stopped intent")
			}
			f.s.pendingStartup = phase.phase
			var events []string
			f.s.hooks.reconcile = func() error { events = append(events, "reconcile"); return f.cell.reconcileServiceBoot() }
			f.s.hooks.repair = func(ctx context.Context) error { events = append(events, "repair"); return f.cell.Repair(ctx) }
			f.s.hooks.stop = func(ctx context.Context) error {
				events = append(events, "stop")
				return f.cell.stopServiceChildren(ctx)
			}
			serviceMaintenanceHostPlant(t, f.cell, "Valid")
			if f.s.step(t.Context()) != nil || len(events) != 0 {
				t.Fatal("Ordinary startup step replayed a pending phase")
			}
			serviceMaintenanceHostClear(t, f.cell)
			if f.s.tick(t.Context()) != nil || f.s.pendingStartup != serviceStartupNone {
				t.Fatal("Released startup tick failed to complete unfinished phases")
			}
			want := []string{"reconcile", "repair", "stop"}
			if phase.name == "repair" {
				want = want[1:]
			}
			if phase.name == "stop" {
				want = want[2:]
			}
			if !reflect.DeepEqual(events, want) || *f.starts != 0 || *f.healths != 0 {
				t.Fatal("Released startup replay changed exact unfinished phase order")
			}
			d, err := f.cell.ServiceDescriptor()
			if err != nil || d.DesiredState != "stopped" {
				t.Fatal("Released startup replay lost persisted stopped intent")
			}
		})
		for _, action := range []string{"observe", "status", "health", "start", "repair", "stop", "exit-host", "restart"} {
			for _, fence := range []string{"cleared", "reappeared"} {
				t.Run("command_gap/"+phase.name+"/"+action+"/"+fence, func(t *testing.T) {
					f := newServiceMaintenanceHostFixture(t)
					f.s.pendingStartup = phase.phase
					serviceMaintenanceHostPlant(t, f.cell, "Valid")
					if f.s.step(t.Context()) != nil {
						t.Fatal("Startup command gap could not establish completed hold")
					}
					serviceMaintenanceHostClear(t, f.cell)
					if fence == "reappeared" {
						serviceMaintenanceHostPlant(t, f.cell, "Valid")
					}
					ctx := t.Context()
					if action == "exit-host" {
						ctx = serviceMaintenanceHostExitContext(t, f.cell, f.d)
					}
					status, err := f.s.command(ctx, action)
					if *f.starts != 0 || *f.healths != 0 || *f.repairs != 0 || f.s.pendingStartup != phase.phase {
						t.Fatal("Startup command gap ran enabling work or completed replay")
					}
					switch action {
					case "observe":
						if err != nil || status.State != "maintenance" {
							t.Fatal("Startup observe gap refreshed the completed hold")
						}
					case "status", "health":
						wantState := "busy"
						if fence == "reappeared" {
							wantState = "maintenance"
						}
						if status.State != wantState {
							t.Fatal("Startup status gap returned an enabling completion")
						}
						if action == "status" && err != nil {
							t.Fatal("Startup status gap returned an unexpected error")
						}
						if action == "health" && fence == "cleared" && !errors.Is(err, errServiceControlDeferred) {
							t.Fatal("Startup health gap did not defer pending work")
						}
						if action == "health" && fence == "reappeared" && !errors.Is(err, errServiceUnhealthy) {
							t.Fatal("Startup held health gap did not reject readiness")
						}
					case "start", "repair":
						if fence == "cleared" && !errors.Is(err, errServiceControlDeferred) {
							t.Fatal("Startup enabling command did not defer before replay")
						}
						if fence == "reappeared" && !errors.Is(err, errMaintenanceFenceActive) {
							t.Fatal("Startup enabling command lost owner maintenance refusal")
						}
					case "stop":
						if err != nil || status.State != "stopped" || *f.stops != 1 {
							t.Fatal("Startup stop gap lost protective stopped completion")
						}
					case "exit-host":
						if err != nil || status.HostExit != "accepted" {
							t.Fatal("Startup exit gap lost legacy drain admission")
						}
					case "restart":
						if !errors.Is(err, ErrServiceControlRefused) {
							t.Fatal("Startup restart gap gained an unsupported action")
						}
					}
					if action != "stop" && *f.stops != 0 {
						t.Fatal("Startup command gap dispatched an unrequested drain")
					}
				})
			}
		}
	}
	for _, phase := range []string{"reconcile", "repair", "stop"} {
		for _, kind := range kinds {
			t.Run("late_fence/"+phase+"/"+kind, func(t *testing.T) {
				f := newServiceMaintenanceHostFixture(t)
				if f.cell.SetServiceDesiredState(t.Context(), "stopped") != nil {
					t.Fatal("Late startup fence could not seed stopped intent")
				}
				f.s.pendingStartup = serviceStartupReconcile
				var events []string
				f.s.hooks.reconcile = func() error {
					events = append(events, "reconcile")
					if phase == "reconcile" {
						serviceMaintenanceHostPlant(t, f.cell, kind)
					}
					return f.cell.reconcileServiceBoot()
				}
				f.s.hooks.repair = func(ctx context.Context) error {
					events = append(events, "repair")
					if phase == "repair" {
						serviceMaintenanceHostPlant(t, f.cell, kind)
					}
					err := f.cell.Repair(ctx)
					if phase == "stop" && err == nil {
						serviceMaintenanceHostPlant(t, f.cell, kind)
					}
					return err
				}
				f.s.hooks.stop = func(ctx context.Context) error {
					events = append(events, "stop")
					return f.cell.stopServiceChildren(ctx)
				}
				if f.s.tick(t.Context()) != nil {
					t.Fatal("Late startup fence did not park the unfinished phase")
				}
				want, pending := []string{"reconcile"}, serviceStartupReconcile
				if phase == "repair" {
					want, pending = []string{"reconcile", "repair"}, serviceStartupRepair
				}
				if phase == "stop" {
					want, pending = []string{"reconcile", "repair", "stop"}, serviceStartupNone
				}
				if !reflect.DeepEqual(events, want) || f.s.pendingStartup != pending {
					t.Fatal("Late startup fence skipped or repeated a phase obligation")
				}
			})
		}
	}
}

func TestServiceMaintenanceHostShutdownRepairRefusal(t *testing.T) {
	for _, result := range []string{"pure", "wrapped", "mixed", "nonmaintenance", "stop_error", "deadline"} {
		t.Run(result, func(t *testing.T) {
			c, _ := serviceMaintenanceShortCell(t)
			bound := serviceMaintenanceHostRunBound(t, c)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h, _, _, _, _ := supervisorTestHooks()
			h.ticks = make(chan time.Time)
			h.health = func(context.Context) error { cancel(); return nil }
			other := errors.New("synthetic shutdown repair refusal")
			stopError := errors.New("synthetic shutdown drain refusal")
			factories, repairs, stops := 0, 0, 0
			var shutdownCtx context.Context
			var expire context.CancelFunc
			h.shutdownContext = func(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
				factories++
				if parent.Err() != nil || parent.Done() != nil || timeout != c.cfg.Timeout {
					t.Fatal("Held shutdown created an invalid independent budget")
				}
				var controlled context.Context
				controlled, expire = context.WithCancel(parent)
				shutdownCtx = &serviceShutdownDeadlineContext{Context: controlled, deadline: time.Now().Add(timeout)}
				return shutdownCtx, expire
			}
			h.repair = func(opCtx context.Context) error {
				if ctx.Err() == nil {
					return nil
				}
				repairs++
				if opCtx != shutdownCtx {
					t.Fatal("Shutdown repair did not receive the shared budget")
				}
				switch result {
				case "wrapped":
					return serviceFailureAt(serviceFailureAt(errMaintenanceFenceActive, "supervisor_repair"), "startup_repair")
				case "mixed":
					return errors.Join(errMaintenanceFenceActive, other)
				case "nonmaintenance":
					return other
				case "deadline":
					expire()
					return errMaintenanceFenceActive
				default:
					return errMaintenanceFenceActive
				}
			}
			h.stop = func(opCtx context.Context) error {
				if ctx.Err() == nil {
					return nil
				}
				stops++
				if opCtx != shutdownCtx {
					t.Fatal("Shutdown stop renewed or replaced its shared budget")
				}
				if result == "stop_error" {
					return stopError
				}
				return nil
			}
			parent, stopParent := context.WithTimeout(ctx, bound)
			defer stopParent()
			err := c.Run(parent, SupervisorPolicy{hooks: &h, ShutdownTimeout: c.cfg.Timeout})
			if factories != 1 || repairs != 1 {
				t.Fatal("Shutdown refusal bypassed or duplicated budget and repair")
			}
			if result == "deadline" {
				if stops != 0 || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("Exhausted shared shutdown budget admitted a fresh drain")
				}
			} else if stops != 1 {
				t.Fatal("Shutdown maintenance refusal suppressed protective stop")
			}
			switch result {
			case "pure", "wrapped":
				if err != nil {
					t.Fatal("Sole maintenance shutdown repair refusal remained fatal")
				}
			case "mixed":
				if !errors.Is(err, other) || !errors.Is(err, errMaintenanceFenceActive) {
					t.Fatal("Mixed shutdown repair failure was swallowed")
				}
			case "nonmaintenance":
				if !errors.Is(err, other) {
					t.Fatal("Ordinary shutdown repair failure was swallowed")
				}
			case "stop_error":
				if !errors.Is(err, stopError) {
					t.Fatal("Protective shutdown stop failure was swallowed")
				}
			}
		})
	}
}

func TestServiceMaintenanceDescriptorContention(t *testing.T) {
	t.Run("remember_current", func(t *testing.T) {
		f := newServiceMaintenanceHostFixture(t)
		known := ProcessRecord{PID: 23, StartTime: "current_descriptor_process", Root: f.d.Root, Component: "server"}
		if writeAtomic(f.cell.path("run", "server.json"), known, nil) != nil {
			t.Fatal("Contended descriptor could not seed current process")
		}
		f.s.status.Processes = []ServiceProcessIdentity{{Component: "worker", PID: 24, StartTime: "stale_process"}}
		lock, err := f.cell.own(false)
		if err != nil {
			t.Fatal("Contended descriptor could not acquire operation lock")
		}
		defer func() { _ = lock.Close() }()
		if f.s.step(t.Context()) != nil || f.s.status.State != "busy" || f.s.status.Reason != "service_operation_in_progress" {
			t.Fatal("Contended descriptor did not preserve busy completion")
		}
		want := []ServiceProcessIdentity{{Component: "server", PID: known.PID, StartTime: known.StartTime}}
		if !reflect.DeepEqual(f.s.status.Processes, want) {
			t.Fatal("Contended descriptor did not remember current processes")
		}
		if *f.starts != 0 || *f.stops != 0 || *f.healths != 0 {
			t.Fatal("Contended descriptor invoked a lifecycle hook")
		}
	})
	t.Run("missing_wins", func(t *testing.T) {
		f := newServiceMaintenanceHostFixture(t)
		if os.Remove(f.cell.path("service", "descriptor.json")) != nil {
			t.Fatal("Contended missing descriptor could not remove record")
		}
		lock, err := f.cell.own(false)
		if err != nil {
			t.Fatal("Contended missing descriptor could not acquire operation lock")
		}
		defer func() { _ = lock.Close() }()
		err = f.s.step(t.Context())
		var located *serviceFailure
		if !errors.As(err, &located) || located.site != "supervisor_traversal" || !errors.Is(err, os.ErrNotExist) {
			t.Fatal("Contended missing descriptor lost traversal failure and cause")
		}
		if f.s.status.State == "busy" || f.s.status.LastStepOutcome != "failed" {
			t.Fatal("Contended missing descriptor reported busy completion")
		}
	})
}

func TestServiceMaintenanceDescriptorFailures(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		f := newServiceMaintenanceHostFixture(t)
		if os.Remove(f.cell.path("service", "descriptor.json")) != nil {
			t.Fatal("Missing descriptor fixture could not remove record")
		}
		err := f.s.step(t.Context())
		var located *serviceFailure
		if !errors.As(err, &located) || located.site != "supervisor_traversal" || !errors.Is(err, os.ErrNotExist) {
			t.Fatal("Missing descriptor changed traversal site or cause")
		}
	})
	t.Run("ownership_conflict", func(t *testing.T) {
		f := newServiceMaintenanceHostFixture(t)
		f.d.InstallationID = "foreign_installation"
		if writeAtomic(f.cell.path("service", "descriptor.json"), f.d, nil) != nil {
			t.Fatal("Foreign descriptor fixture could not replace record")
		}
		err := f.s.step(t.Context())
		var located *serviceFailure
		if !errors.As(err, &located) || located.site != "supervisor_traversal" || !errors.Is(err, ErrServiceOwnershipConflict) {
			t.Fatal("Foreign descriptor changed traversal site or cause")
		}
	})
	t.Run("non_private_record", func(t *testing.T) {
		f := newServiceMaintenanceHostFixture(t)
		path := f.cell.path("service", "descriptor.json")
		if os.Remove(path) != nil {
			t.Fatal("Nonprivate descriptor fixture could not remove record")
		}
		if testMkdir(path, 0o700) != nil {
			t.Fatal("Nonprivate descriptor fixture could not plant directory")
		}
		err := f.s.step(t.Context())
		var located *serviceFailure
		if !errors.As(err, &located) || located.site != "supervisor_traversal" || err.Error() != "service_record_not_private" {
			t.Fatal("Nonprivate descriptor changed traversal site or cause")
		}
	})
}

func TestServiceMaintenanceDescriptorHeldProjection(t *testing.T) {
	f := newServiceMaintenanceHostFixture(t)
	serviceMaintenanceHostPlant(t, f.cell, "Valid")
	if f.s.step(t.Context()) != nil {
		t.Fatal("Held descriptor could not complete initial step")
	}
	initial := f.s.projectedStatus()
	if initial.State != "maintenance" || initial.DesiredState != f.d.DesiredState || initial.Generation != f.d.Payload.Generation {
		t.Fatal("Held descriptor lost initial desired state or generation")
	}
	f.d.DesiredState, f.d.Payload.Generation = "stopped", "replacement_generation"
	f.d.Revision++
	if f.cell.writeServiceDescriptor(f.d) != nil {
		t.Fatal("Held descriptor could not publish replacement")
	}
	if f.s.step(t.Context()) != nil {
		t.Fatal("Held descriptor could not complete replacement step")
	}
	after := f.s.projectedStatus()
	if after.State != "maintenance" || after.DesiredState != "stopped" || after.Generation != "replacement_generation" || after.Reason != "local_maintenance_fence_active" {
		t.Fatal("Held descriptor did not follow replacement state and generation")
	}
	if *f.starts != 0 || *f.stops != 0 || *f.healths != 0 {
		t.Fatal("Held descriptor replacement invoked a lifecycle hook")
	}
}

func TestServiceMaintenanceDescriptorCompletingStop(t *testing.T) {
	f := newServiceMaintenanceHostFixture(t)
	f.d.DesiredState = "stopped"
	if f.cell.writeServiceDescriptor(f.d) != nil {
		t.Fatal("Completing stop could not publish stopped descriptor")
	}
	known := ProcessRecord{PID: 23, StartTime: "completing_stop_process", Root: f.d.Root, Component: "server"}
	if writeAtomic(f.cell.path("run", "server.json"), known, nil) != nil {
		t.Fatal("Completing stop could not seed current process")
	}
	f.s.running, f.s.completingStop = true, true
	lock, err := f.cell.own(false)
	if err != nil {
		t.Fatal("Completing stop could not acquire operation lock")
	}
	defer func() { _ = lock.Close() }()
	if f.s.step(t.Context()) != nil || f.s.status.State != "stopped" || f.s.status.LastStepOutcome != "ok" || f.s.running {
		t.Fatal("Completing stop changed stopped completion under contention")
	}
	want := []ServiceProcessIdentity{{Component: "server", PID: known.PID, StartTime: known.StartTime}}
	if !reflect.DeepEqual(f.s.status.Processes, want) {
		t.Fatal("Completing stop did not remember current processes")
	}
	if *f.stops != 1 || *f.starts != 0 || *f.healths != 0 {
		t.Fatal("Completing stop changed expected lifecycle hook calls")
	}
}
