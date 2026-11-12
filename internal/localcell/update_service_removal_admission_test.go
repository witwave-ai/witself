package localcell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func serviceRemovalHoldHost(t *testing.T, c *Cell) func() {
	t.Helper()
	file, err := openLockFile(c.path(".service-host.lock"), true)
	if err != nil {
		t.Fatal("Removal host contender open failed")
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		t.Fatal("Removal host contender acquisition failed")
	}
	return func() {
		if file.Close() != nil {
			t.Fatal("Removal host contender release failed")
		}
	}
}

func serviceRemovalStopped(t *testing.T, c *Cell, d ServiceDescriptor) {
	t.Helper()
	held, err := c.own(false)
	if err != nil {
		t.Fatal("Removal drain operation lock was unavailable")
	}
	defer func() { _ = held.Close() }()
	d.DesiredState = "stopped"
	unixServicePersistDescriptor(t, c, d)
}

func serviceRemovalLocksHeld(t *testing.T, c *Cell) {
	t.Helper()
	held, err := unixServiceContender(t, c).own(false)
	if held != nil {
		_ = held.Close()
	}
	if err != ErrLocked {
		t.Fatal("Removal effect escaped its operation lock")
	}
	file, err := openLockFile(c.path(".service-host.lock"), false)
	if err != nil {
		t.Fatal("Removal host contender could not open stable lock")
	}
	defer func() { _ = file.Close() }()
	if err := lockFile(file); !errors.Is(err, ErrLocked) {
		t.Fatal("Removal effect escaped its host lock")
	}
}

func serviceRemovalLocksFree(t *testing.T, c *Cell) {
	t.Helper()
	serviceMaintenanceReleased(t, c)
	info, err := os.Lstat(c.path(".service-host.lock"))
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Fatal("Removal changed the stable empty host lock")
	}
	release := serviceRemovalHoldHost(t, c)
	release()
}

type serviceRemovalFixture struct {
	t      *testing.T
	cell   *Cell
	d      ServiceDescriptor
	launch *launchdAgentAdapter
	lm     *launchdFixtureManager
	win    *windowsTaskFake
}

func newServiceRemovalFixture(t *testing.T, family string) *serviceRemovalFixture {
	t.Helper()
	f := &serviceRemovalFixture{t: t}
	if family == "Launchd" {
		f.launch, f.d, f.lm = launchdFixture(t)
		f.cell = f.lm.cell
		launchdInstallFixture(t, f.launch, f.d)
	} else {
		f.win = newWindowsTaskFake(t)
		f.cell, f.d = f.win.cell, f.win.d
		f.win.apply()
		// The portable Windows renderer needs Windows paths on Unix. Its existing
		// descriptor seam reads durable bytes from the real fixture root.
		f.win.a.readDescriptor = func(ServiceDescriptor) (ServiceDescriptor, error) {
			var d ServiceDescriptor
			err := readJSON(f.cell.path("service", "descriptor.json"), &d)
			return d, err
		}
	}
	serviceRemovalStopped(t, f.cell, f.d)
	f.setDrain(func(context.Context, ServiceDescriptor) error {
		var d ServiceDescriptor
		if readJSON(f.cell.path("service", "descriptor.json"), &d) != nil {
			t.Fatal("Removal drain descriptor read failed")
		}
		serviceRemovalStopped(t, f.cell, d)
		return nil
	})
	release := serviceRemovalHoldHost(t, f.cell)
	release()
	f.clearCalls()
	return f
}

func (f *serviceRemovalFixture) setDrain(fn func(context.Context, ServiceDescriptor) error) {
	if f.launch != nil {
		f.launch.drain = fn
		return
	}
	if fn == nil {
		f.win.a.drain = nil
		return
	}
	f.win.a.drain = func(ctx context.Context, d ServiceDescriptor) error {
		err := fn(ctx, d)
		if err == nil {
			f.win.drained = true
		}
		return err
	}
}
func (f *serviceRemovalFixture) remove() error {
	if f.launch != nil {
		return f.launch.Remove(f.t.Context(), f.d)
	}
	return f.win.a.Remove(f.t.Context(), f.d)
}
func (f *serviceRemovalFixture) clearCalls() {
	if f.launch != nil {
		f.lm.calls, f.lm.allCalls = nil, nil
	} else {
		f.win.calls = nil
	}
}
func (f *serviceRemovalFixture) absent() {
	if f.launch != nil {
		f.lm.loaded = false
	} else {
		f.win.xml = nil
	}
}
func (f *serviceRemovalFixture) registered() bool {
	if f.launch != nil {
		return f.lm.loaded
	}
	return f.win.xml != nil
}
func (f *serviceRemovalFixture) artifactDir() string {
	if f.launch != nil {
		return f.launch.agentDir
	}
	return f.win.a.directory(f.d)
}
func (f *serviceRemovalFixture) artifacts() map[string]serviceMaintenanceEntry {
	return unixServiceArtifactSnapshot(f.t, f.artifactDir())
}
func (f *serviceRemovalFixture) destructive() []string {
	var got []string
	if f.launch != nil {
		for _, call := range f.lm.allCalls {
			if call[1] != "print" && call[1] != "print-disabled" {
				got = append(got, call[1])
			}
		}
	} else {
		for _, call := range f.win.calls {
			if call[0] != "/Query" {
				got = append(got, call[0])
			}
		}
	}
	return got
}
func (f *serviceRemovalFixture) noDestruction() {
	f.t.Helper()
	if len(f.destructive()) != 0 {
		f.t.Fatal("Removal refusal issued a destructive manager command")
	}
}
func (f *serviceRemovalFixture) requireComplete(wasRegistered bool) {
	f.t.Helper()
	if f.registered() {
		f.t.Fatal("Removal retained manager registration")
	}
	want := []string{}
	if wasRegistered {
		if f.launch != nil {
			want = append(want, "bootout")
		} else {
			want = append(want, "/Delete")
		}
	}
	if !reflect.DeepEqual(append([]string{}, f.destructive()...), want) {
		f.t.Fatal("Removal destructive trace changed its exact baseline")
	}
	artifacts := f.artifacts()
	if len(artifacts) != 1 {
		f.t.Fatal("Removal retained registration artifacts")
	}
}

func TestServiceAdapterRemovalPostState(t *testing.T) {
	for _, family := range []string{"Launchd", "Windows"} {
		t.Run(family, func(t *testing.T) {
			for _, state := range []string{"registered", "absent_manager"} {
				t.Run(state, func(t *testing.T) {
					for _, kind := range append([]string{"Absent"}, unixServiceFenceKinds()...) {
						t.Run(kind, func(t *testing.T) {
							f := newServiceRemovalFixture(t, family)
							if state == "absent_manager" {
								f.absent()
							}
							serviceMaintenanceFence(t, f.cell, kind)
							before, artifacts, registered := serviceMaintenanceSnapshot(t, f.cell), f.artifacts(), f.registered()
							err := f.remove()
							if kind == "Absent" {
								if err != nil {
									t.Fatal("Unfenced removal did not complete")
								}
								f.requireComplete(registered)
							} else {
								serviceMaintenanceCause(t, err)
								f.noDestruction()
								if f.registered() != registered {
									t.Fatal("Fenced removal changed manager registration")
								}
								if !reflect.DeepEqual(artifacts, f.artifacts()) {
									t.Fatal("Fenced removal changed registration artifacts")
								}
								serviceMaintenanceUnchanged(t, f.cell, before)
							}
							serviceRemovalLocksFree(t, f.cell)
						})
					}
				})
			}
		})
	}
}

func TestServiceAdapterRemovalDrainOutsideLock(t *testing.T) {
	for _, family := range []string{"Launchd", "Windows"} {
		t.Run(family, func(t *testing.T) {
			f := newServiceRemovalFixture(t, family)
			reached := false
			f.setDrain(func(context.Context, ServiceDescriptor) error {
				held, err := unixServiceContender(t, f.cell).own(false)
				if err != nil {
					t.Fatal("Removal held the operation lock across its drain")
				}
				defer func() { _ = held.Close() }()
				release := serviceRemovalHoldHost(t, f.cell)
				release()
				reached = true
				return nil
			})
			if f.remove() != nil || !reached {
				t.Fatal("Removal failed the unlocked drain callback")
			}
		})
	}
}

func TestServiceAdapterRemovalFreshIdentity(t *testing.T) {
	for _, family := range []string{"Launchd", "Windows"} {
		t.Run(family, func(t *testing.T) {
			for _, change := range []string{"installation", "generation", "running"} {
				t.Run(change, func(t *testing.T) {
					f := newServiceRemovalFixture(t, family)
					artifacts := f.artifacts()
					f.setDrain(func(context.Context, ServiceDescriptor) error {
						fresh := f.d
						fresh.DesiredState = "stopped"
						switch change {
						case "installation":
							fresh.InstallationID += "other"
							manifest, err := readManifest(f.cell.cfg.Root)
							if err != nil {
								t.Fatal("Removal identity manifest read failed")
							}
							manifest.InstallationID = fresh.InstallationID
							if writeAtomic(f.cell.path("manifest.json"), manifest, nil) != nil {
								t.Fatal("Removal identity manifest write failed")
							}
						case "generation":
							fresh.Payload.Generation += "other"
						case "running":
							fresh.DesiredState = "running"
						}
						held, err := f.cell.own(false)
						if err != nil {
							t.Fatal("Removal identity drain lock failed")
						}
						unixServicePersistDescriptor(t, f.cell, fresh)
						_ = held.Close()
						return nil
					})
					if !errors.Is(f.remove(), ErrServiceOwnershipConflict) {
						t.Fatal("Removal trusted stale identity or running intent")
					}
					f.noDestruction()
					if !reflect.DeepEqual(artifacts, f.artifacts()) {
						t.Fatal("Fresh identity refusal changed registration artifacts")
					}
				})
			}
		})
	}
}

func TestServiceAdapterRemovalPrecedence(t *testing.T) {
	for _, family := range []string{"Launchd", "Windows"} {
		t.Run(family, func(t *testing.T) {
			for _, earlier := range []string{"preflight", "drain", "operation_lock", "identity", "running", "host", "fence"} {
				t.Run(earlier, func(t *testing.T) {
					f := newServiceRemovalFixture(t, family)
					serviceMaintenanceFence(t, f.cell, "Valid")
					sentinel := ErrServiceOwnershipConflict
					var release func()
					f.setDrain(func(context.Context, ServiceDescriptor) error { return nil })
					switch earlier {
					case "preflight":
						if f.launch != nil {
							f.lm.mismatch = "path"
						} else {
							f.win.queryCode = 0x80070005
							sentinel = ErrServiceManagerUnavailable
						}
						f.setDrain(func(context.Context, ServiceDescriptor) error {
							t.Fatal("Removal preflight refusal reached the drain")
							return nil
						})
					case "drain":
						f.setDrain(func(context.Context, ServiceDescriptor) error { return errors.New("fixture drain refusal") })
						sentinel = ErrServiceRemovalFailed
						held, err := unixServiceContender(t, f.cell).own(false)
						if err != nil {
							t.Fatal("Removal drain precedence contender failed")
						}
						release = func() { _ = held.Close() }
					case "operation_lock":
						f.setDrain(func(context.Context, ServiceDescriptor) error {
							fresh := f.d
							fresh.Payload.Generation += "other"
							serviceRemovalStopped(t, f.cell, fresh)
							held, err := unixServiceContender(t, f.cell).own(false)
							if err != nil {
								t.Fatal("Removal precedence operation contender failed")
							}
							release = func() { _ = held.Close() }
							return nil
						})
						if f.launch != nil {
							sentinel = ErrLocked
						}
					case "identity", "running":
						f.setDrain(func(context.Context, ServiceDescriptor) error {
							fresh := f.d
							if earlier == "identity" {
								fresh.Payload.Generation += "other"
							}
							held, err := f.cell.own(false)
							if err != nil {
								t.Fatal("Removal identity precedence drain lock failed")
							}
							unixServicePersistDescriptor(t, f.cell, fresh)
							_ = held.Close()
							release = serviceRemovalHoldHost(t, f.cell)
							return nil
						})
					case "host":
						release = serviceRemovalHoldHost(t, f.cell)
						sentinel = ErrServiceHostActive
					case "fence":
						sentinel = errMaintenanceFenceActive
					}
					err := f.remove()
					if release != nil {
						release()
					}
					if !errors.Is(err, sentinel) {
						t.Fatal("Removal precedence returned the later cause")
					}
					if f.launch != nil {
						switch earlier {
						case "operation_lock":
							if err != ErrLocked {
								t.Fatal("Launchd removal changed exact operation contention")
							}
						case "identity":
							unixServiceRequireError(t, err, ErrServiceOwnershipConflict, "remove:descriptor", "stale_identity")
						case "running":
							unixServiceRequireError(t, err, ErrServiceOwnershipConflict, "remove:descriptor", "not_stopped")
						case "host":
							unixServiceRequireError(t, err, ErrServiceHostActive, "remove:host_lease", "active")
						case "drain":
							unixServiceRequireError(t, err, ErrServiceRemovalFailed, "remove:host_exit", "host_exit_failed")
						}
					} else {
						switch earlier {
						case "operation_lock":
							windowsTaskMaintenanceDiagnostic(t, err, ErrServiceOwnershipConflict, "inspect:host_owner", "unavailable")
						case "identity", "running":
							windowsTaskMaintenanceDiagnostic(t, err, ErrServiceOwnershipConflict, "remove:ownership", "changed")
						case "drain":
							windowsTaskMaintenanceDiagnostic(t, err, ErrServiceRemovalFailed, "remove:host_exit", "host_exit_failed")
						}
					}
					f.noDestruction()
				})
			}
		})
	}
}

func TestServiceAdapterRemovalHostExclusion(t *testing.T) {
	for _, family := range []string{"Launchd", "Windows"} {
		t.Run(family, func(t *testing.T) {
			t.Run("gap_winner", func(t *testing.T) {
				f := newServiceRemovalFixture(t, family)
				var release func()
				f.setDrain(func(context.Context, ServiceDescriptor) error {
					release = serviceRemovalHoldHost(t, f.cell)
					return nil
				})
				err := f.remove()
				if release != nil {
					release()
				}
				if !errors.Is(err, ErrServiceHostActive) {
					t.Fatal("Removal did not refuse the gap host winner")
				}
				if f.launch != nil {
					unixServiceRequireError(t, err, ErrServiceHostActive, "remove:host_lease", "active")
				} else {
					windowsTaskMaintenanceDiagnostic(t, err, ErrServiceHostActive, "remove:host_lease", "active")
				}
				for _, other := range []error{ErrServiceOwnershipConflict, ErrServiceManagerUnavailable, ErrServiceRegistrationFailed, ErrServiceRemovalFailed} {
					if errors.Is(err, other) {
						t.Fatal("Removal host refusal exposed another service sentinel")
					}
				}
				f.noDestruction()
				serviceRemovalLocksFree(t, f.cell)
			})
			t.Run("never_hosted_and_continuous", func(t *testing.T) {
				f := newServiceRemovalFixture(t, family)
				if os.Remove(f.cell.path(".service-host.lock")) != nil {
					t.Fatal("Removal never hosted lock setup failed")
				}
				if writeAtomicBytes(f.cell.path("service", "lease.json"), []byte("retained diagnostic lease"), nil) != nil {
					t.Fatal("Removal diagnostic lease fixture failed")
				}
				before := serviceMaintenanceSnapshot(t, f.cell)
				effects := 0
				if f.launch != nil {
					command := f.launch.command
					f.launch.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
						if args[0] == "bootout" {
							serviceRemovalLocksHeld(t, f.cell)
							effects++
						}
						return command(ctx, binary, args...)
					}
					f.launch.inspectFile = func(path string) (os.FileInfo, error) {
						if path == f.launch.plistPath(f.d) && !f.lm.loaded {
							serviceRemovalLocksHeld(t, f.cell)
							effects++
						}
						return os.Lstat(path)
					}
				} else {
					command := f.win.a.command
					f.win.a.command = func(ctx context.Context, args ...string) (windowsTaskCommandResult, error) {
						if args[0] == "/Delete" {
							serviceRemovalLocksHeld(t, f.cell)
							effects++
						}
						return command(ctx, args...)
					}
					directory := f.win.a.recordDirectory
					f.win.a.recordDirectory = func(d ServiceDescriptor) string {
						// This accessor is evaluated by both final unlink expressions.
						if f.win.count("/Delete") != 0 {
							serviceRemovalLocksHeld(t, f.cell)
							effects++
						}
						return directory(d)
					}
				}
				if f.remove() != nil {
					t.Fatal("Removal with continuous locks failed")
				}
				if effects < 2 {
					t.Fatal("Removal continuous lock observers did not run")
				}
				after := serviceMaintenanceSnapshot(t, f.cell)
				delete(after, ".service-host.lock")
				if !reflect.DeepEqual(before, after) {
					t.Fatal("Removal changed cell data or diagnostic lease")
				}
				serviceRemovalLocksFree(t, f.cell)
			})
		})
	}
}

func TestServiceAdapterRemovalRetry(t *testing.T) {
	for _, family := range []string{"Launchd", "Windows"} {
		t.Run(family, func(t *testing.T) {
			f := newServiceRemovalFixture(t, family)
			serviceMaintenanceFence(t, f.cell, "Valid")
			serviceMaintenanceCause(t, f.remove())
			if os.Remove(f.cell.path("journal", "maintenance.json")) != nil {
				t.Fatal("Removal retry fence cleanup failed")
			}
			f.clearCalls()
			if f.remove() != nil {
				t.Fatal("Removal did not finish after fence cleanup")
			}
			f.requireComplete(true)
		})
	}
	t.Run("Launchd_bootout_done_plist_retained", func(t *testing.T) {
		f := newServiceRemovalFixture(t, "Launchd")
		failed := false
		saved := filepath.Join(f.launch.agentDir, "saved-plist")
		path := f.launch.plistPath(f.d)
		f.launch.inspectFile = func(name string) (os.FileInfo, error) {
			info, err := os.Lstat(name)
			if name == path && !f.lm.loaded && !failed && err == nil {
				// Windows resolves file identity lazily. Capture it before the path changes.
				if !os.SameFile(info, info) {
					t.Fatal("Launchd unlink fixture identity capture failed")
				}
				failed = true
				if os.Rename(path, saved) != nil || os.Mkdir(path, 0o700) != nil || os.WriteFile(filepath.Join(path, "blocker"), []byte("retained"), 0o600) != nil {
					t.Fatal("Launchd unlink failure fixture setup failed")
				}
			}
			return info, err
		}
		if !errors.Is(f.remove(), ErrServiceRemovalFailed) || !failed || f.lm.loaded {
			t.Fatal("Launchd failed unlink did not retain unloaded recovery state")
		}
		if os.Remove(filepath.Join(path, "blocker")) != nil || os.Remove(path) != nil || os.Rename(saved, path) != nil {
			t.Fatal("Launchd unlink failure fixture restoration failed")
		}
		f.launch.inspectFile = nil
		f.clearCalls()
		if f.remove() != nil {
			t.Fatal("Launchd retained unloaded plist could not be removed")
		}
		f.requireComplete(false)
	})
	for _, state := range []string{"foreign", "domain_unavailable"} {
		t.Run("Launchd_"+state, func(t *testing.T) {
			f := newServiceRemovalFixture(t, "Launchd")
			f.absent()
			if state == "foreign" {
				if os.WriteFile(f.launch.plistPath(f.d), []byte("foreign retained plist"), 0o600) != nil {
					t.Fatal("Launchd foreign recovery fixture failed")
				}
			} else {
				f.lm.noGUI = true
			}
			_, baseline := f.launch.Inspect(t.Context(), f.d)
			err := f.remove()
			if baseline == nil || err == nil {
				t.Fatal("Launchd unsafe recovery state was accepted")
			}
			want := baseline.Error()
			if errors.Is(baseline, ErrServiceOwnershipConflict) || errors.Is(baseline, ErrServiceRegistrationFailed) {
				want = ErrServiceOwnershipConflict.Error() + ": remove:ownership: " + want
			}
			if err.Error() != want {
				t.Fatal("Launchd recovery changed its baseline refusal text")
			}
			f.noDestruction()
		})
	}
}

func TestServiceAdapterRemovalThenApply(t *testing.T) {
	for _, family := range []string{"Launchd", "Windows"} {
		t.Run(family, func(t *testing.T) {
			f := newServiceRemovalFixture(t, family)
			if f.remove() != nil {
				t.Fatal("Removal before reinstall failed")
			}
			var fresh ServiceDescriptor
			if readJSON(f.cell.path("service", "descriptor.json"), &fresh) != nil {
				t.Fatal("Removal reinstall descriptor read failed")
			}
			if fresh.DesiredState != "stopped" {
				t.Fatal("Removal lost persisted stopped intent")
			}
			if f.launch != nil {
				plan, err := f.launch.Plan(t.Context(), fresh)
				if err != nil || f.launch.Apply(t.Context(), plan) != nil || !f.lm.loaded {
					t.Fatal("Launchd reinstall changed its stopped intent baseline")
				}
			} else {
				plan, err := f.win.a.Plan(t.Context(), fresh)
				if err != nil {
					t.Fatal("Windows stopped reinstall planning failed")
				}
				err = f.win.a.Apply(t.Context(), plan)
				if !errors.Is(err, ErrServiceOwnershipConflict) || f.win.xml != nil {
					t.Fatal("Windows reinstall changed its stopped intent refusal")
				}
			}
		})
	}
}

func TestServiceAdapterRemovalRealDrain(t *testing.T) {
	for _, family := range []string{"Launchd", "Windows"} {
		owner := ""
		if family == "Launchd" {
			// Windows ownership uses SIDs rather than launchd numeric user IDs.
			if runtime.GOOS == "windows" {
				continue
			}
			var err error
			owner, err = serviceOwner()
			if err != nil {
				t.Fatal("Launchd real owner lookup failed")
			}
			if runtime.GOOS == "linux" && owner == "0" {
				continue
			}
		}
		t.Run(family, func(t *testing.T) {
			states := []string{"empty", "unknown", "live"}
			if family == "Windows" {
				states = append(states, "absent_unknown", "absent_live")
			}
			for _, state := range states {
				t.Run(state, func(t *testing.T) {
					f := newServiceRemovalFixture(t, family)
					c := f.cell
					if strings.HasPrefix(state, "absent_") {
						f.absent()
					}
					if family == "Launchd" {
						f.d.UID, f.launch.uid = owner, owner
						f.lm.d = f.d
						unixServicePersistDescriptor(t, c, f.d)
					} else {
						c, _ = descriptorTestCell(t)
						f.win.a.removalCell = func(ServiceDescriptor) (*Cell, error) { return c, nil }
					}
					switch state {
					case "unknown", "absent_unknown":
						if writeAtomicBytes(c.path("run", "worker.json"), []byte("{"), nil) != nil {
							t.Fatal("Removal unknown child fixture failed")
						}
					case "live", "absent_live":
						windowsTaskRecordedLiveChild(t, c, "worker")
					}
					f.setDrain(nil)
					if family == "Windows" {
						f.win.drained = true
					}
					artifacts := f.artifacts()
					childBefore := serviceMaintenanceSnapshot(t, c)["run/worker.json"]
					err := f.remove()
					if state == "empty" {
						if err != nil {
							t.Fatal("Removal production drain refused an empty child tree")
						}
					} else {
						if !errors.Is(err, ErrServiceRemovalFailed) || !strings.Contains(err.Error(), "host_exit_failed") {
							t.Fatal("Removal production drain accepted retained child evidence")
						}
						f.noDestruction()
						if !reflect.DeepEqual(artifacts, f.artifacts()) {
							t.Fatal("Removal production drain refusal changed registration")
						}
						if childBefore != serviceMaintenanceSnapshot(t, c)["run/worker.json"] {
							t.Fatal("Removal production drain refusal changed retained child evidence")
						}
					}
				})
			}
		})
	}
}

func TestServiceAdapterRemovalRetainsCellData(t *testing.T) {
	for _, family := range []string{"Launchd", "Windows"} {
		t.Run(family, func(t *testing.T) {
			f := newServiceRemovalFixture(t, family)
			if privateDir(f.cell.path("bystander"), true) != nil || writeAtomicBytes(f.cell.path("bystander", "retained"), []byte("retained cell data"), nil) != nil {
				t.Fatal("Removal retained tree fixture failed")
			}
			sibling := filepath.Join(f.artifactDir(), "sibling.registration")
			if os.WriteFile(sibling, []byte("sibling registration"), 0o600) != nil {
				t.Fatal("Removal sibling fixture failed")
			}
			before := serviceMaintenanceSnapshot(t, f.cell)
			if f.remove() != nil {
				t.Fatal("Removal retained data fixture could not finish")
			}
			serviceMaintenanceUnchanged(t, f.cell, before)
			raw, err := os.ReadFile(sibling)
			if err != nil || string(raw) != "sibling registration" {
				t.Fatal("Removal changed a sibling registration")
			}
		})
	}
}

func TestServiceAdapterRemovalConfinementLaunchd(t *testing.T) {
	f := newServiceRemovalFixture(t, "Launchd")
	raw, err := os.ReadFile(f.launch.plistPath(f.d))
	if err != nil {
		t.Fatal("Launchd confinement artifact read failed")
	}
	f.launch.home = f.cell.cfg.Root
	f.launch.agentDir = filepath.Join(f.launch.home, "Library", "LaunchAgents")
	if os.MkdirAll(f.launch.agentDir, 0o700) != nil || os.WriteFile(f.launch.plistPath(f.d), raw, 0o600) != nil {
		t.Fatal("Launchd confinement registration setup failed")
	}
	// The overlapping artifact directory was created ordinarily inside the
	// cell root, which Windows treats as a non-private tree. The fixture has
	// already persisted stopped intent, and confinement is refused before the
	// operation lock, so the drain must not take that lock here.
	f.setDrain(func(context.Context, ServiceDescriptor) error { return nil })
	before := serviceMaintenanceSnapshot(t, f.cell)
	err = f.remove()
	unixServiceRequireError(t, err, ErrServiceOwnershipConflict, "remove:confinement", "overlap")
	f.noDestruction()
	serviceMaintenanceUnchanged(t, f.cell, before)
}
