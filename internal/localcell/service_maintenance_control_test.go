package localcell

import (
	"context"
	"errors"
	"go/ast"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func serviceMaintenanceControlSeed(t *testing.T, f serviceMaintenanceHostFixture, state string) ServiceDescriptor {
	t.Helper()
	base := "idle"
	if state == "running_healthy" || state == "desired_stopped_running" || state == "descriptor_faulted_running" || state == "circuit_open_unpersisted" {
		base = "running"
	}
	serviceMaintenanceHostSeed(t, f, base)
	d, err := f.cell.ServiceDescriptor()
	if err != nil {
		t.Fatal("Control seed could not read descriptor")
	}
	// Stable logical generation permits an exact status comparison across roots.
	d.Payload.Generation = "maintenance_control_generation"
	switch state {
	case "desired_stopped_running":
		d.DesiredState = "stopped"
	case "descriptor_faulted_running":
		d.Faulted = true
	case "circuit_open_unpersisted":
		for range 5 {
			f.s.record.Failures = append(f.s.record.Failures, *f.now)
		}
	case "shutdown_latch_pending":
		f.s.running, f.s.shutdownBlocked, f.s.shutdownFaultPending = true, true, true
	case "shutdown_latch_settled":
		// A persisted refusal leaves the latch beside a faulted descriptor; the
		// refusal must keep precedence over the circuit classification.
		d.Faulted = true
		f.s.running, f.s.shutdownBlocked = true, true
	case "pending_startup_clean", "pending_startup_faulted":
		f.s.pendingStartup = serviceStartupReconcile
		f.s.startupFaulted = state == "pending_startup_faulted"
	}
	if f.cell.writeServiceDescriptor(d) != nil || f.s.save() != nil {
		t.Fatal("Control seed could not persist initial state")
	}
	f.s.status.HostExit = "refused"
	// A successful observation must replace a previous tick's busy projection.
	f.s.maintenanceBusy = true
	return d
}

func TestServiceMaintenanceControlHoldMatrix(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	kinds := []string{"Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	states := []string{"idle", "running_healthy", "desired_stopped_running", "descriptor_faulted_running", "circuit_open_unpersisted", "shutdown_latch_pending", "shutdown_latch_settled", "pending_startup_clean", "pending_startup_faulted"}
	for _, kind := range kinds {
		for _, action := range []string{"status", "health"} {
			for _, state := range states {
				if kind != "Valid" && state != "idle" && state != "desired_stopped_running" {
					continue
				}
				t.Run(kind+"/"+action+"/"+state, func(t *testing.T) {
					f, twin := newServiceMaintenanceHostFixture(t), newServiceMaintenanceHostFixture(t)
					d := serviceMaintenanceControlSeed(t, f, state)
					serviceMaintenanceControlSeed(t, twin, state)
					reconciles := 0
					f.s.hooks.reconcile = func() error { reconciles++; return nil }
					serviceMaintenanceHostPlant(t, f.cell, kind)
					serviceMaintenanceHostPlant(t, twin.cell, kind)
					before := serviceMaintenanceSnapshot(t, f.cell)
					running, blocked, pending := f.s.running, f.s.shutdownBlocked, f.s.shutdownFaultPending
					startup, startupFaulted := f.s.pendingStartup, f.s.startupFaulted
					record := f.s.record
					record.Failures = slices.Clone(record.Failures)
					status, err := f.s.command(t.Context(), action)
					if action == "status" && err != nil || action == "health" && !errors.Is(err, errServiceUnhealthy) {
						t.Fatal("Held query returned an unexpected action error")
					}
					want := ServiceStatus{DesiredState: d.DesiredState, State: "maintenance", Reason: errMaintenanceFenceActive.Error(), Generation: d.Payload.Generation, Restarts: max(0, record.Attempts-1), HostExit: "refused", LastStepOutcome: "ok"}
					if !reflect.DeepEqual(status, want) {
						t.Fatal("Held query changed its complete projected response")
					}
					if *f.starts != 0 || *f.stops != 0 || *f.healths != 0 || *f.repairs != 0 || reconciles != 0 {
						t.Fatal("Held query performed a lifecycle effect")
					}
					serviceMaintenanceUnchanged(t, f.cell, before)
					if f.s.running != running || f.s.shutdownBlocked != blocked || f.s.shutdownFaultPending != pending || f.s.pendingStartup != startup || f.s.startupFaulted != startupFaulted || !reflect.DeepEqual(f.s.record, record) {
						t.Fatal("Held query changed an effect owning supervisor field")
					}
					if twin.s.step(t.Context()) != nil {
						t.Fatal("Held twin could not complete its protective effects")
					}
					if !reflect.DeepEqual(f.s.status, twin.s.status) {
						t.Fatal("Held query raw status differs from its effectful twin")
					}
					if !reflect.DeepEqual(f.s.projectedStatus(), twin.s.projectedStatus()) {
						t.Fatal("Held query projection differs from its effectful twin")
					}
					twinDescriptor, readErr := twin.cell.ServiceDescriptor()
					if readErr != nil {
						t.Fatal("Held twin descriptor could not be inspected")
					}
					obligation := false
					switch state {
					case "desired_stopped_running", "descriptor_faulted_running":
						obligation = true
						if *twin.stops != 1 || twin.s.running {
							t.Error("Held twin did not exercise the protective stop")
						}
					case "circuit_open_unpersisted":
						obligation = true
						if !twinDescriptor.Faulted || *twin.stops != 1 || twin.s.running {
							t.Error("Held twin did not persist and drain the circuit")
						}
					case "shutdown_latch_pending":
						obligation = true
						if !twinDescriptor.Faulted || twin.s.shutdownFaultPending {
							t.Error("Held twin did not settle the shutdown fault obligation")
						}
					}
					if obligation {
						if f.s.step(t.Context()) != nil {
							t.Fatal("Query successor tick could not complete its obligation")
						}
						got, descriptorErr := f.cell.ServiceDescriptor()
						if descriptorErr != nil {
							t.Fatal("Query successor tick descriptor could not be inspected")
						}
						switch state {
						case "desired_stopped_running", "descriptor_faulted_running":
							if *f.stops != 1 || f.s.running {
								t.Fatal("Query successor tick omitted its protective stop")
							}
						case "circuit_open_unpersisted":
							if !got.Faulted || *f.stops != 1 || f.s.running {
								t.Fatal("Query successor tick omitted its circuit fault or stop")
							}
						case "shutdown_latch_pending":
							if !got.Faulted || f.s.shutdownFaultPending {
								t.Fatal("Query successor tick omitted its shutdown fault persistence")
							}
						}
						if !reflect.DeepEqual(f.s.status, twin.s.status) || f.s.running != twin.s.running || f.s.shutdownFaultPending != twin.s.shutdownFaultPending || *f.stops != *twin.stops {
							t.Fatal("Query successor tick did not reach its effectful twin")
						}
						// Only per-installation identity differs between the two cells.
						twinDescriptor.Root, twinDescriptor.InstallationID = d.Root, d.InstallationID
						twinDescriptor.Payload.Args = d.Payload.Args
						if !reflect.DeepEqual(got, twinDescriptor) {
							t.Fatal("Query successor tick changed the protective descriptor result")
						}
					}
				})
			}
		}
	}
}

func TestServiceMaintenanceControlQueryFlagCleared(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, action := range []string{"status", "health"} {
		for _, outcome := range []string{"held", "not_held", "locked", "cancelled"} {
			t.Run(action+"/"+outcome, func(t *testing.T) {
				f := newServiceMaintenanceHostFixture(t)
				serviceMaintenanceHostSeed(t, f, "running")
				ctx := t.Context()
				if outcome == "held" {
					serviceMaintenanceHostPlant(t, f.cell, "Valid")
				}
				var release func() error
				if outcome == "locked" {
					lock, err := f.cell.own(false)
					if err != nil {
						t.Fatal("Query flag fixture could not acquire contention lock")
					}
					release = lock.Close
					t.Cleanup(func() { _ = lock.Close() })
				}
				if outcome == "cancelled" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				status, err := f.s.command(ctx, action)
				if release != nil && release() != nil {
					t.Fatal("Query flag fixture could not release contention lock")
				}
				if f.s.queryStep {
					t.Fatal("Completed query retained its temporary query flag")
				}
				switch outcome {
				case "held":
					if status.State != "maintenance" || action == "status" && err != nil || action == "health" && !errors.Is(err, errServiceUnhealthy) {
						t.Fatal("Held flag query lost its normal query result")
					}
					serviceMaintenanceHostClear(t, f.cell)
				case "not_held":
					if err != nil || !status.Healthy {
						t.Fatal("Absent flag query lost its healthy result")
					}
				case "locked":
					if status.State != "busy" || action == "status" && err != nil || action == "health" && !errors.Is(err, errServiceControlDeferred) {
						t.Fatal("Contended flag query lost its deferred busy result")
					}
				case "cancelled":
					if !errors.Is(err, context.Canceled) {
						t.Fatal("Canceled flag query lost caller cancellation")
					}
				}
				if f.cell.SetServiceDesiredState(t.Context(), "stopped") != nil {
					t.Fatal("Query flag fixture could not seed protective intent")
				}
				f.s.running = true
				serviceMaintenanceHostPlant(t, f.cell, "Valid")
				stops := *f.stops
				if f.s.step(t.Context()) != nil || *f.stops != stops+1 || f.s.running {
					t.Fatal("Query flag leaked into the subsequent protective tick")
				}
			})
		}
	}
	for _, action := range []string{"stop", "start", "repair"} {
		t.Run(action, func(t *testing.T) {
			f := newServiceMaintenanceHostFixture(t)
			observed := false
			check := func(context.Context) error { observed = observed || f.s.queryStep; return nil }
			f.s.hooks.start, f.s.hooks.stop, f.s.hooks.health, f.s.hooks.repair = check, check, check, check
			f.s.hooks.reconcile = func() error { observed = observed || f.s.queryStep; return nil }
			if _, err := f.s.command(t.Context(), action); err != nil {
				t.Fatal("Nonquery flag fixture command unexpectedly failed")
			}
			if observed || f.s.queryStep {
				t.Fatal("Nonquery command used or retained the query flag")
			}
		})
	}
}

func TestServiceMaintenanceControlLateFenceUnchanged(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, action := range []string{"status", "health"} {
		t.Run(action, func(t *testing.T) {
			f := newServiceMaintenanceHostFixture(t)
			seed := serviceMaintenanceHostSeed(t, f, "running")
			healths, stops := 0, 0
			f.s.hooks.health = func(context.Context) error {
				healths++
				serviceMaintenanceHostPlant(t, f.cell, "Valid")
				return errors.New("synthetic unhealthy admission")
			}
			f.s.hooks.stop = func(ctx context.Context) error { stops++; return f.cell.stopServiceChildren(ctx) }
			status, err := f.s.command(t.Context(), action)
			if action == "status" && err != nil || action == "health" && !errors.Is(err, errServiceUnhealthy) {
				t.Fatal("Late fence changed the completed query error")
			}
			if stops != 1 || healths != 1 || *f.starts != 0 || f.s.running {
				t.Fatal("Late fence suppressed the admitted protective cleanup")
			}
			if status.State != "maintenance" || status.Reason != errMaintenanceFenceActive.Error() || status.Healthy || status.LastStepOutcome != "ok" {
				t.Fatal("Late fence lost its stopped maintenance projection")
			}
			if !reflect.DeepEqual(f.s.record, seed) {
				t.Fatal("Late fence changed the admitted failure budget")
			}
			serviceMaintenanceStopJournal(t, f.cell)
		})
	}
}

func TestServiceMaintenanceControlHostExitObserved(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	f := newServiceMaintenanceHostFixture(t)
	serviceMaintenanceHostSeed(t, f, "running")
	serviceMaintenanceHostPlant(t, f.cell, "Valid")
	if f.s.step(t.Context()) != nil {
		t.Fatal("Host exit fixture could not enter maintenance hold")
	}
	ctx := serviceMaintenanceHostExitContext(t, f.cell, f.d)
	if status, err := f.s.command(ctx, "exit-host"); err != nil || status.HostExit != "accepted" {
		t.Fatal("Held host exit did not accept its authenticated envelope")
	}
	stops := 0
	f.s.hooks.stop = func(context.Context) error { stops++; return errors.New("synthetic drain refusal") }
	delivered := make(chan struct{})
	close(delivered)
	if f.s.finishHostExit(ctx, delivered) || stops != 1 {
		t.Fatal("Held host exit did not preserve its refused drain")
	}
	for _, action := range []string{"observe", "status"} {
		status, err := f.s.command(ctx, action)
		if err != nil || status.HostExit != "refused" || status.State != "maintenance" {
			t.Fatal("Held host exit projection concealed the refused drain")
		}
	}
	data, err := os.ReadFile("service_control_exit.go")
	if err != nil {
		t.Fatal("Host exit source assertion could not read its owner")
	}
	body := serviceAdmissionFunction(t, "service_control_exit.go", strings.ReplaceAll(string(data), "\r\n", "\n"), "waitServiceHostExit")
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		condition, ok := node.(*ast.IfStmt)
		if ok && strings.Contains(admissionNodeText(condition.Cond), `status.HostExit == "refused"`) {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("Host exit waiter lost its explicit refusal observation")
	}
}
