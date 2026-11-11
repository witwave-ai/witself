package localcell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func serviceTransitionFenceKinds() []string {
	kinds := []string{"Absent", "Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	return kinds
}

func serviceTransitionFence(t *testing.T, c *Cell, kind string) {
	t.Helper()
	serviceMaintenanceFence(t, c, kind)
	if kind != "Absent" {
		t.Cleanup(func() { _ = os.RemoveAll(c.path("journal", "maintenance.json")) })
	}
}

func serviceTransitionSeed(t *testing.T, c *Cell, d ServiceDescriptor) {
	t.Helper()
	if testWriteFile(c.path("service", "descriptor.json"), serviceMaintenanceParityJSON(t, d), 0o600) != nil {
		t.Fatal("cannot seed transition descriptor")
	}
}

// Rebuild the Cell with its callback in Config; the callback neither acquires R
// nor starts a child. The empty child tree still exercises the real stop journal.
func serviceTransitionLateFence(t *testing.T, c *Cell, kind string, unreadable bool) (*Cell, *int) {
	t.Helper()
	planted := 0
	cfg := c.cfg
	cfg.Failpoint = func(operation, step string) error {
		if operation == "stop" && step == "stop" {
			planted++
			if planted != 1 {
				t.Fatal("late fence callback ran more than once")
			}
			serviceTransitionFence(t, c, kind)
			if unreadable && testWriteFile(c.path("service", "descriptor.json"), []byte("{"), 0o600) != nil {
				t.Fatal("cannot seed unreadable final descriptor")
			}
		}
		return nil
	}
	configured, err := New(cfg)
	if err != nil {
		t.Fatal("cannot construct late-fence fixture")
	}
	return configured, &planted
}

func TestServiceMaintenanceDesiredTransitions(t *testing.T) {
	for _, from := range []string{"running", "stopped", "maintenance"} {
		for _, to := range []string{"running", "stopped", "maintenance"} {
			for _, faulted := range []bool{false, true} {
				for _, pending := range []bool{false, true} {
					for _, kind := range serviceTransitionFenceKinds() {
						name := fmt.Sprintf("from=%s,to=%s,faulted=%t,pending=%t,fence=%s", from, to, faulted, pending, kind)
						t.Run(name, func(t *testing.T) {
							c, d := serviceMaintenanceCell(t, true)
							d.DesiredState, d.Faulted, d.Revision, d.RepairRevision = from, faulted, 7, 3
							serviceTransitionSeed(t, c, d)
							if pending && testWriteFile(c.path("service", "transition.json"), serviceMaintenanceParityJSON(t, d), 0o600) != nil {
								t.Fatal("cannot seed pending transition")
							}
							serviceTransitionFence(t, c, kind)
							before := serviceMaintenanceSnapshot(t, c)
							err := c.SetServiceDesiredState(t.Context(), to)
							legacy := to == "running" && (faulted || from == "maintenance" || pending)
							if legacy || to == "running" && kind != "Absent" {
								if legacy {
									serviceMaintenanceExactError(t, err, ErrServiceDesiredState, "")
								} else {
									serviceMaintenanceCause(t, err)
								}
								serviceMaintenanceUnchanged(t, c, before)
							} else {
								if err != nil {
									t.Fatal("admitted or protective desired-state transition failed")
								}
								d.DesiredState, d.Revision = to, d.Revision+1
								serviceMaintenanceDescriptorBytes(t, c, d)
								serviceMaintenanceParityUnchanged(t, c, before, "service/descriptor.json", "service/transition.json")
							}
							serviceMaintenanceReleased(t, c)
						})
					}
				}
			}
		}
	}
}

func TestServiceMaintenanceRepairTransitions(t *testing.T) {
	for _, kind := range serviceTransitionFenceKinds() {
		for _, faulted := range []bool{false, true} {
			for _, timing := range []string{"initial", "late"} {
				if kind == "Absent" && timing == "late" {
					continue
				}
				name := fmt.Sprintf("timing=%s,faulted=%t,fence=%s", timing, faulted, kind)
				t.Run(name, func(t *testing.T) {
					c, d := serviceMaintenanceCell(t, true)
					d.Faulted, d.Revision, d.RepairRevision = faulted, 7, 3
					serviceTransitionSeed(t, c, d)
					var planted *int
					if timing == "late" {
						c, planted = serviceTransitionLateFence(t, c, kind, false)
					} else {
						serviceTransitionFence(t, c, kind)
					}
					before := serviceMaintenanceSnapshot(t, c)
					err := c.RepairService(t.Context())
					d.DesiredState, d.Revision = "stopped", d.Revision+1
					if kind == "Absent" {
						if err != nil {
							t.Fatal("unfenced repair failed")
						}
						d.Faulted, d.Revision, d.RepairRevision = false, d.Revision+1, d.RepairRevision+1
					} else {
						serviceMaintenanceCause(t, err)
					}
					serviceMaintenanceDescriptorBytes(t, c, d)
					serviceMaintenanceReleased(t, c)
					if timing == "late" || kind == "Absent" {
						serviceMaintenanceStopJournal(t, c)
						m, readErr := readManifest(c.cfg.Root)
						if readErr != nil || m.LastOperation != "stop" {
							t.Fatal("repair lost the completed stop effect")
						}
					} else {
						serviceMaintenanceParityUnchanged(t, c, before, "service/descriptor.json")
					}
					if kind == "Absent" {
						return
					}
					if timing == "late" && (*planted != 1 || c.maintenanceFenceAllows(0) != errMaintenanceFenceActive) {
						t.Fatal("late repair did not retain exactly one planted fence")
					}
					secondBefore := serviceMaintenanceSnapshot(t, c)
					serviceMaintenanceCause(t, c.RepairService(t.Context()))
					d.Revision++
					serviceMaintenanceDescriptorBytes(t, c, d)
					serviceMaintenanceParityUnchanged(t, c, secondBefore, "service/descriptor.json")
					if timing == "late" && *planted != 1 {
						t.Fatal("repeated fenced repair reached the stop callback")
					}
					serviceMaintenanceReleased(t, c)
				})
			}
		}
	}
	for _, kind := range []string{"Absent", "Valid"} {
		t.Run("final_read,fence="+kind, func(t *testing.T) {
			c, _ := serviceMaintenanceCell(t, true)
			c, planted := serviceTransitionLateFence(t, c, kind, true)
			serviceMaintenanceExactError(t, c.RepairService(t.Context()), nil, "invalid local cell record")
			if *planted != 1 || serviceMaintenanceSnapshot(t, c)["service/descriptor.json"].data != "{" {
				t.Fatal("final read refusal replaced unreadable evidence")
			}
			serviceMaintenanceStopJournal(t, c)
			serviceMaintenanceReleased(t, c)
		})
	}
}

func TestServiceMaintenanceTransitionLegacyOrdering(t *testing.T) {
	cases := []string{"invalid", "cancelled", "locked", "descriptor_missing", "descriptor_corrupt", "manifest_missing", "manifest_corrupt", "faulted", "maintenance", "pending"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			c, d := serviceMaintenanceCell(t, true)
			ctx, state := t.Context(), "running"
			var cause error
			want := ""
			switch name {
			case "invalid":
				state, cause = "invalid", ErrServiceDesiredState
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				cause = context.Canceled
			case "locked":
				cause = ErrLocked
			case "descriptor_missing":
				if os.Remove(c.path("service", "descriptor.json")) != nil {
					t.Fatal("cannot remove descriptor fixture")
				}
				cause = os.ErrNotExist
			case "descriptor_corrupt":
				if testWriteFile(c.path("service", "descriptor.json"), []byte("{"), 0o600) != nil {
					t.Fatal("cannot corrupt descriptor fixture")
				}
				want = "invalid local cell record"
			case "manifest_missing":
				if os.Remove(c.path("manifest.json")) != nil {
					t.Fatal("cannot remove manifest fixture")
				}
			case "manifest_corrupt":
				if testWriteFile(c.path("manifest.json"), []byte("{"), 0o600) != nil {
					t.Fatal("cannot corrupt manifest fixture")
				}
				want = "invalid local cell record"
			case "faulted":
				d.Faulted, cause = true, ErrServiceDesiredState
				serviceTransitionSeed(t, c, d)
			case "maintenance":
				d.DesiredState, cause = "maintenance", ErrServiceDesiredState
				serviceTransitionSeed(t, c, d)
			case "pending":
				if testWriteFile(c.path("service", "transition.json"), serviceMaintenanceParityJSON(t, d), 0o600) != nil {
					t.Fatal("cannot seed pending transition fixture")
				}
				cause = ErrServiceDesiredState
			}
			var earlier error
			for _, kind := range []string{"Absent", "Valid"} {
				t.Run(kind, func(t *testing.T) {
					serviceTransitionFence(t, c, kind)
					before := serviceMaintenanceSnapshot(t, c)
					var held *operationLock
					if name == "locked" {
						other, err := New(c.cfg)
						if err != nil {
							t.Fatal("cannot construct legacy contender")
						}
						held, err = other.own(false)
						if err != nil {
							t.Fatal("cannot acquire legacy contender")
						}
						t.Cleanup(func() { _ = held.Close() })
					}
					err := c.SetServiceDesiredState(ctx, state)
					if held != nil && held.Close() != nil {
						t.Fatal("cannot release legacy contender")
					}
					if name == "manifest_missing" {
						if !errors.Is(err, os.ErrNotExist) {
							t.Fatal("missing manifest lost its read cause")
						}
					} else {
						serviceMaintenanceExactError(t, err, cause, want)
					}
					if kind == "Absent" {
						earlier = err
					} else if reflect.TypeOf(err) != reflect.TypeOf(earlier) || err.Error() != earlier.Error() {
						t.Fatal("fence changed the earlier error identity or text")
					}
					serviceMaintenanceUnchanged(t, c, before)
					serviceMaintenanceReleased(t, c)
				})
			}
		})
	}
}

func TestServiceMaintenanceTransitionAbsentParity(t *testing.T) {
	for _, parent := range []string{"leaf_absent", "parent_absent"} {
		for _, action := range []string{"running", "stopped", "maintenance", "repair"} {
			t.Run(action+","+parent, func(t *testing.T) {
				c, d := serviceMaintenanceCell(t, true)
				d.Revision, d.RepairRevision = 7, 3
				if action == "repair" {
					d.Faulted = true
				}
				serviceTransitionSeed(t, c, d)
				m, err := readManifest(c.cfg.Root)
				if err != nil {
					t.Fatal("cannot read parity manifest")
				}
				if parent == "parent_absent" && os.Remove(c.path("journal")) != nil {
					t.Fatal("cannot remove parity journal parent")
				}
				before := serviceMaintenanceSnapshot(t, c)
				exceptions := []string{"service/descriptor.json"}
				if action == "repair" {
					err = c.RepairService(t.Context())
					d.DesiredState, d.Faulted, d.Revision, d.RepairRevision = "stopped", false, d.Revision+2, d.RepairRevision+1
					m.LastOperation = "stop"
					serviceMaintenanceParityFile(t, c, "manifest.json", serviceMaintenanceParityJSON(t, m))
					serviceMaintenanceStopJournal(t, c)
					exceptions = append(exceptions, "manifest.json", "journal", "journal/operations.jsonl")
				} else {
					err = c.SetServiceDesiredState(t.Context(), action)
					d.DesiredState, d.Revision = action, d.Revision+1
				}
				if err != nil {
					t.Fatal("absent transition parity changed baseline success")
				}
				serviceMaintenanceDescriptorBytes(t, c, d)
				serviceMaintenanceParityFile(t, c, "service/descriptor.json", serviceMaintenanceParityJSON(t, d))
				serviceMaintenanceParityUnchanged(t, c, before, exceptions...)
				serviceMaintenanceReleased(t, c)
			})
		}
	}
}

func TestServiceMaintenanceTransitionInterimControl(t *testing.T) {
	for _, kind := range serviceTransitionFenceKinds()[1:] {
		for _, action := range []string{"start", "repair"} {
			t.Run(action+",fence="+kind, func(t *testing.T) {
				c, d := serviceMaintenanceCell(t, true)
				d.DesiredState, d.RepairRevision = "stopped", 3
				d.Faulted = action == "repair"
				serviceTransitionSeed(t, c, d)
				var planted *int
				if action == "repair" {
					c, planted = serviceTransitionLateFence(t, c, kind, false)
				} else {
					serviceTransitionFence(t, c, kind)
				}
				h, now, starts, stops, _ := supervisorTestHooks()
				healths, repairs, reconciles := 0, 0, 0
				h.health = func(context.Context) error { healths++; return nil }
				h.repair = func(context.Context) error { repairs++; return nil }
				h.reconcile = func() error { reconciles++; return nil }
				seed := supervisorRecord{Version: 1, RepairRevision: d.RepairRevision, Attempts: 3,
					Failures: []time.Time{now.Add(-time.Millisecond)}, NextStart: now.Add(h.initialBackoff)}
				if testWriteFile(c.path("service", "supervisor.json"), serviceMaintenanceParityJSON(t, seed), 0o600) != nil {
					t.Fatal("cannot seed supervisor transition circuit")
				}
				s := newSupervisor(c, SupervisorPolicy{hooks: &h})
				if s.load(d) != nil {
					t.Fatal("cannot load child-free transition supervisor")
				}
				if action == "repair" {
					s.shutdownBlocked, s.shutdownFaultPending, s.running = true, true, true
				}
				statusBefore, before := s.status, serviceMaintenanceSnapshot(t, c)
				_, err := s.command(t.Context(), action)
				if !errors.Is(err, errMaintenanceFenceActive) {
					t.Fatal("control command lost the maintenance cause")
				}
				if *starts != 0 || *stops != 0 || healths != 0 || repairs != 0 || reconciles != 0 || !reflect.DeepEqual(seed, s.record) || !reflect.DeepEqual(statusBefore, s.status) {
					t.Fatal("refused command consumed a supervisor step or circuit record")
				}
				if action == "start" {
					serviceMaintenanceUnchanged(t, c, before)
					status, err := s.command(t.Context(), "status")
					if err != nil || status.DesiredState != "stopped" || status.LastStepOutcome != "ok" || !reflect.DeepEqual(seed, s.record) || *starts != 0 || *stops != 0 || healths != 0 {
						t.Fatal("refused start prevented the following status response")
					}
					serviceMaintenanceUnchanged(t, c, before)
				} else {
					if !s.shutdownBlocked || !s.shutdownFaultPending || !s.running || *planted != 1 {
						t.Fatal("late-fenced repair reopened the shutdown latch")
					}
					d.Revision++
					serviceMaintenanceDescriptorBytes(t, c, d)
					serviceMaintenanceStopJournal(t, c)
					if serviceMaintenanceSnapshot(t, c)["service/supervisor.json"] != before["service/supervisor.json"] {
						t.Fatal("late-fenced repair rewrote the circuit record")
					}
				}
				serviceMaintenanceReleased(t, c)
			})
		}
	}
}

func TestServiceMaintenanceTransitionLockLifetime(t *testing.T) {
	for _, owner := range []string{"running", "repair"} {
		for _, kind := range serviceTransitionFenceKinds() {
			t.Run(owner+",fence="+kind, func(t *testing.T) {
				c, d := serviceMaintenanceCell(t, true)
				serviceTransitionFence(t, c, kind)
				call := func() error { return c.SetServiceDesiredState(t.Context(), "running") }
				if owner == "repair" {
					call = func() error { return c.RepairService(t.Context()) }
				}
				before := serviceMaintenanceSnapshot(t, c)
				other, err := New(c.cfg)
				if err != nil {
					t.Fatal("cannot construct transition contender")
				}
				lock, err := other.own(false)
				if err != nil {
					t.Fatal("cannot acquire transition contender")
				}
				t.Cleanup(func() { _ = lock.Close() })
				got := call()
				// No reads, including snapshots, while the contender holds R.
				if lock.Close() != nil {
					t.Fatal("cannot release transition contender")
				}
				serviceMaintenanceExactError(t, got, ErrLocked, "")
				serviceMaintenanceUnchanged(t, c, before)
				serviceMaintenanceReleased(t, c)
				err = call()
				if kind != "Absent" {
					serviceMaintenanceCause(t, err)
				} else if err != nil {
					t.Fatal("transition did not proceed after contention")
				}
				if owner == "repair" {
					d.DesiredState, d.Revision = "stopped", d.Revision+1
					if kind == "Absent" {
						d.Revision, d.RepairRevision = d.Revision+1, d.RepairRevision+1
					}
				} else if kind == "Absent" {
					d.Revision++
				}
				serviceMaintenanceDescriptorBytes(t, c, d)
				serviceMaintenanceReleased(t, c)
			})
		}
	}
}
