package localcell

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
)

const faultPersistenceLost = "stop refusal lost required persistence"

type faultCommitGuardFixture struct {
	serviceMaintenanceHostFixture
	ctx    context.Context
	cancel context.CancelFunc
}

func newFaultCommitGuardFixture(t *testing.T) *faultCommitGuardFixture {
	t.Helper()
	f := newServiceMaintenanceHostFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	f.s.running = true
	f.s.hooks.windowsShutdownPolicy = true
	f.s.hooks.health = func(context.Context) error { return errServiceUnhealthy }
	f.s.hooks.stop = func(context.Context) error {
		*f.stops++
		return ErrServiceControlRefused
	}
	return &faultCommitGuardFixture{f, ctx, cancel}
}

func (f *faultCommitGuardFixture) descriptor(t *testing.T) ServiceDescriptor {
	t.Helper()
	d, err := f.cell.ServiceDescriptor()
	if err != nil {
		t.Fatal(faultPersistenceLost)
	}
	return d
}

// Run the interposition at the first command-marker read after call(stop)
// returns from its detached latch write. Everything runs on this test goroutine.
func (f *faultCommitGuardFixture) step(t *testing.T, before, between func()) error {
	t.Helper()
	crossed := false
	gate := &serviceWorkerTickGate{Context: f.ctx, hold: func() {
		crossed = true
		between()
	}}
	f.s.hooks.beforeFaultWrite = func() {
		f.s.hooks.beforeFaultWrite = nil
		if before != nil {
			before()
		}
		gate.arm.Store(true)
	}
	err := f.s.step(gate)
	if !crossed || *f.stops != 1 || !f.s.shutdownBlocked {
		t.Fatal(faultPersistenceLost)
	}
	return err
}

// Also run the next real tick. It must keep the same revision and must not
// repeat the child stop, including after a failed first write was retried.
func (f *faultCommitGuardFixture) settled(t *testing.T, revision uint64) {
	t.Helper()
	d := f.descriptor(t)
	_, journalErr := os.Lstat(f.cell.path("service", "transition.json"))
	if d.Revision != revision || !d.Faulted || !errors.Is(journalErr, os.ErrNotExist) || f.s.shutdownFaultPending {
		t.Fatal(faultPersistenceLost)
	}
	if f.s.step(t.Context()) != nil || f.descriptor(t).Revision != revision || *f.stops != 1 ||
		f.s.status.State != "faulted" || f.s.status.Reason != "service_stop_refused" || f.s.status.Healthy {
		t.Fatal(faultPersistenceLost)
	}
}

func (f *faultCommitGuardFixture) interruptRetirement(t *testing.T) {
	t.Helper()
	f.cell.observeServiceDescriptor = func(stage string, d ServiceDescriptor) {
		if stage == "descriptor" && d.Faulted {
			f.cell.observeServiceDescriptor = nil
			if removePrivateFile(f.cell.path("service", "transition.json")) != nil {
				t.Fatal(faultPersistenceLost)
			}
		}
	}
}

func runServiceFaultGuardPaths(t *testing.T) {
	t.Run("plain_reuse", func(t *testing.T) {
		const failure = "same refusal committed a second fault"
		f := newFaultCommitGuardFixture(t)
		if f.s.step(f.ctx) != nil {
			t.Fatal(failure)
		}
		d := f.descriptor(t)
		if d.Revision != f.d.Revision+1 || !d.Faulted || *f.stops != 1 || !f.s.shutdownBlocked || f.s.shutdownFaultPending {
			t.Fatal(failure)
		}
	})
	t.Run("outside_repair_after_settle", func(t *testing.T) {
		const failure = "settled refusal overwrote an outside repair"
		f := newFaultCommitGuardFixture(t)
		if f.s.step(f.ctx) != nil {
			t.Fatal(failure)
		}
		committed := f.descriptor(t)
		if !committed.Faulted || committed.Revision != f.d.Revision+1 || f.s.shutdownFaultPending {
			t.Fatal(failure)
		}
		if publicationClient(t, f.cell).RepairService(t.Context()) != nil {
			t.Fatal(failure)
		}
		repaired := f.descriptor(t)
		if repaired.Faulted || repaired.Revision <= committed.Revision || f.s.step(f.ctx) != nil {
			t.Fatal(failure)
		}
		d := f.descriptor(t)
		if d.Revision != repaired.Revision || d.Faulted || *f.stops != 1 ||
			!f.s.shutdownBlocked || f.s.status.Reason != "service_stop_refused" {
			t.Fatal(failure)
		}
	})
	t.Run("contended_durable_cancel", func(t *testing.T) {
		const failure = "durable refusal turned host cancellation into a failure"
		f := newFaultCommitGuardFixture(t)
		var release func()
		err := f.step(t, nil, func() { release = f.holdOperation(t, failure) })
		d := f.descriptor(t)
		_, journalErr := os.Lstat(f.cell.path("service", "transition.json"))
		if err != nil || f.s.status.State != "busy" || !d.Faulted || d.Revision != f.d.Revision+1 ||
			!errors.Is(journalErr, os.ErrNotExist) {
			t.Fatal(failure)
		}
		release()
		f.cancel()
		tickErr := f.s.step(f.ctx)
		if !errors.Is(tickErr, context.Canceled) || f.s.workerExitError(tickErr, f.ctx.Err(), true) != nil {
			t.Fatal(failure)
		}
	})
	t.Run("pending_then_cancelled", func(t *testing.T) {
		const failure = "cancelled follow-up lost the pending refusal"
		f := newFaultCommitGuardFixture(t)
		var release func()
		err := f.step(t, func() { release = f.holdOperation(t, failure) }, func() {
			d := f.descriptor(t)
			if d.Faulted || d.Revision != f.d.Revision || !f.s.shutdownFaultPending {
				t.Fatal(failure)
			}
			release()
			f.cancel()
		})
		d := f.descriptor(t)
		if !errors.Is(err, context.Canceled) || d.Faulted || d.Revision != f.d.Revision ||
			!f.s.shutdownFaultPending || f.s.workerExitError(err, f.ctx.Err(), true) != err {
			t.Fatal(failure)
		}
	})
	for _, row := range []string{"new_refusal", "independent_fault"} {
		t.Run(row, func(t *testing.T) {
			f := newFaultCommitGuardFixture(t)
			refusals := 0
			f.s.publishStopRefusal = func() { refusals++ }
			f.s.latchServiceShutdownRefusal(t.Context(), "stop", ErrServiceControlRefused)
			first := f.descriptor(t)
			if !first.Faulted || first.Revision != f.d.Revision+1 || refusals != 1 || f.s.shutdownFaultPending {
				t.Fatal(faultPersistenceLost)
			}

			// Both events start with exactly the descriptor committed above.
			// Neither event is the follow-up write for the first refused stop.
			wantReason, wantRefusals := "service_circuit_open", 1
			if row == "new_refusal" {
				f.s.latchServiceShutdownRefusal(t.Context(), "stop", ErrServiceControlRefused)
				wantReason, wantRefusals = "service_stop_refused", 2
			} else if err := f.s.fault(t.Context(), wantReason); err != nil {
				t.Fatal(faultPersistenceLost)
			}
			d := f.descriptor(t)
			if !d.Faulted || d.Revision != first.Revision+1 || f.s.status.Reason != wantReason ||
				refusals != wantRefusals || *f.stops != 0 || !f.s.shutdownBlocked || f.s.shutdownFaultPending {
				t.Fatal(faultPersistenceLost)
			}
		})
	}
	for _, row := range []string{"pending_first", "pending_retry_locked"} {
		t.Run(row, func(t *testing.T) {
			f := newFaultCommitGuardFixture(t)
			var release func()
			err := f.step(t, func() {
				lock, err := f.cell.own(t.Context(), false)
				if err != nil {
					t.Fatal(faultPersistenceLost)
				}
				release = sync.OnceFunc(func() { _ = lock.Close() })
				t.Cleanup(release)
			}, func() {
				d := f.descriptor(t)
				if d.Faulted || d.Revision != f.d.Revision || !f.s.shutdownFaultPending {
					t.Fatal(faultPersistenceLost)
				}
				if row == "pending_first" {
					release()
				}
			})
			if err != nil {
				t.Fatal(faultPersistenceLost)
			}
			if row == "pending_retry_locked" {
				d := f.descriptor(t)
				if d.Faulted || d.Revision != f.d.Revision || !f.s.shutdownFaultPending {
					t.Fatal(faultPersistenceLost)
				}
				release()
				if f.s.step(t.Context()) != nil {
					t.Fatal(faultPersistenceLost)
				}
			}
			f.settled(t, f.d.Revision+1)
		})
	}
	t.Run("interrupted_first", func(t *testing.T) {
		f := newFaultCommitGuardFixture(t)
		f.interruptRetirement(t)
		err := f.step(t, nil, func() {
			d := f.descriptor(t)
			if !d.Faulted || d.Revision != f.d.Revision+1 || !f.s.shutdownFaultPending {
				t.Fatal(faultPersistenceLost)
			}
		})
		if err != nil {
			t.Fatal(faultPersistenceLost)
		}
		f.settled(t, f.d.Revision+2)
	})
	t.Run("contended_repair", func(t *testing.T) {
		f := newFaultCommitGuardFixture(t)
		writer := publicationClient(t, f.cell)
		var release func()
		var repaired ServiceDescriptor
		err := f.step(t, nil, func() {
			if writer.RepairService(t.Context()) != nil {
				t.Fatal(faultPersistenceLost)
			}
			repaired = f.descriptor(t)
			if repaired.Faulted || repaired.DesiredState != "stopped" || repaired.Revision != f.d.Revision+3 {
				t.Fatal(faultPersistenceLost)
			}
			lock, err := writer.own(t.Context(), false)
			if err != nil {
				t.Fatal(faultPersistenceLost)
			}
			release = sync.OnceFunc(func() { _ = lock.Close() })
			t.Cleanup(release)
		})
		if err != nil || f.s.status.State != "busy" || !f.s.shutdownFaultPending || f.descriptor(t).Faulted {
			t.Fatal(faultPersistenceLost)
		}
		release()
		if f.s.step(t.Context()) != nil {
			t.Fatal(faultPersistenceLost)
		}
		f.settled(t, repaired.Revision+1)
	})
	for _, row := range []string{"repaired_between", "revision_changed", "generation_changed", "installation_changed", "fault_cleared", "transition_pending"} {
		t.Run(row, func(t *testing.T) {
			f := newFaultCommitGuardFixture(t)
			writer := publicationClient(t, f.cell)
			var beforeRetry ServiceDescriptor
			err := f.step(t, nil, func() {
				d := f.descriptor(t)
				if !d.Faulted || d.Revision != f.d.Revision+1 || f.s.shutdownFaultPending {
					t.Fatal(faultPersistenceLost)
				}
				switch row {
				case "repaired_between":
					if writer.RepairService(t.Context()) != nil {
						t.Fatal(faultPersistenceLost)
					}
				case "revision_changed":
					if writer.setServiceFaulted(t.Context(), true) != nil {
						t.Fatal(faultPersistenceLost)
					}
				case "generation_changed", "installation_changed", "fault_cleared":
					// Keep the original durable revision. Change just the claimed
					// evidence field in these synthetic objects below the fixture root.
					switch row {
					case "generation_changed":
						d.Payload.Generation += "-next"
					case "installation_changed":
						m, err := readManifest(t.Context(), writer.cfg.Root)
						if err != nil {
							t.Fatal(faultPersistenceLost)
						}
						m.InstallationID += "-next"
						d.InstallationID = m.InstallationID
						if writeAtomic(writer.path("manifest.json"), m, nil) != nil {
							t.Fatal(faultPersistenceLost)
						}
					case "fault_cleared":
						d.Faulted = false
					}
					if writer.writeServiceDescriptor(d) != nil {
						t.Fatal(faultPersistenceLost)
					}
				case "transition_pending":
					d.Revision++
					d.DesiredState = "stopped"
					if writeAtomic(writer.path("service", "transition.json"), d, nil) != nil {
						t.Fatal(faultPersistenceLost)
					}
				}
				beforeRetry = f.descriptor(t)
				if row == "repaired_between" && (beforeRetry.Faulted || beforeRetry.RepairRevision != f.d.RepairRevision+1 ||
					beforeRetry.DesiredState != "stopped" || beforeRetry.Revision != d.Revision+2) {
					t.Fatal(faultPersistenceLost)
				}
			})
			if err != nil {
				t.Fatal(faultPersistenceLost)
			}
			f.settled(t, beforeRetry.Revision+1)
			d := f.descriptor(t)
			if d.DesiredState != beforeRetry.DesiredState || d.RepairRevision != beforeRetry.RepairRevision ||
				d.Payload.Generation != beforeRetry.Payload.Generation || d.InstallationID != beforeRetry.InstallationID {
				t.Fatal(faultPersistenceLost)
			}
		})
	}
	t.Run("independent_error", func(t *testing.T) {
		f := newFaultCommitGuardFixture(t)
		err := f.step(t, nil, func() {
			// Invalidate the first publication; fail retirement of the required
			// second publication after its descriptor actually reached disk.
			if f.cell.SetServiceDesiredState(t.Context(), "stopped") != nil {
				t.Fatal(faultPersistenceLost)
			}
			f.interruptRetirement(t)
		})
		var failure *serviceFailure
		if !errors.As(err, &failure) || failure.site != "descriptor_write" {
			t.Fatal(faultPersistenceLost)
		}
		f.cancel()
		joined := errors.Join(f.ctx.Err(), err)
		if f.s.workerExitError(err, f.ctx.Err(), true) != err || f.s.workerExitError(joined, f.ctx.Err(), true) != joined {
			t.Fatal(faultPersistenceLost)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		f := newFaultCommitGuardFixture(t)
		err := f.step(t, nil, f.cancel)
		if f.s.workerExitError(err, f.ctx.Err(), true) != nil {
			t.Fatal(faultPersistenceLost)
		}
		f.settled(t, f.d.Revision+1)
	})
}

func (f *faultCommitGuardFixture) holdOperation(t *testing.T, failure string) func() {
	t.Helper()
	lock, err := publicationClient(t, f.cell).own(t.Context(), false)
	if err != nil {
		t.Fatal(failure)
	}
	release := sync.OnceFunc(func() { _ = lock.Close() })
	t.Cleanup(release)
	return release
}
