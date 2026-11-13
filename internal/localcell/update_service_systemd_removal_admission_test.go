//go:build !windows

package localcell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func serviceRemovalSystemdFixture(t *testing.T, loaded bool) (*systemdUserAdapter, ServiceDescriptor, *systemdFixtureManager) {
	t.Helper()
	a, d, m := systemdFixture(t)
	d.DesiredState = "stopped"
	unixServicePersistDescriptor(t, m.cell, d)
	m.descriptor = d
	unixSystemdSeed(t, a, d)
	m.loaded, m.active = loaded, loaded
	return a, d, m
}

func serviceRemovalSystemdNoEffects(t *testing.T, m *systemdFixtureManager) {
	t.Helper()
	for _, call := range m.calls {
		if len(call) > 2 && (call[2] == "disable" || call[2] == "daemon-reload") {
			t.Fatal("Systemd refusal issued a destructive command")
		}
	}
}

func serviceRemovalSystemdHostFile(t *testing.T, c *Cell) {
	t.Helper()
	release := serviceRemovalHoldHost(t, c)
	release()
}

func TestServiceAdapterRemovalPostStateSystemd(t *testing.T) {
	for _, state := range []string{"registered", "absent_manager"} {
		t.Run(state, func(t *testing.T) {
			for _, kind := range append([]string{"Absent"}, unixServiceFenceKinds()...) {
				t.Run(kind, func(t *testing.T) {
					loaded := state == "registered"
					a, d, m := serviceRemovalSystemdFixture(t, loaded)
					serviceRemovalSystemdHostFile(t, m.cell)
					serviceMaintenanceFence(t, m.cell, kind)
					before := serviceMaintenanceSnapshot(t, m.cell)
					artifacts := unixServiceArtifactSnapshot(t, a.unitDir)
					err := a.Remove(t.Context(), d)
					if kind != "Absent" {
						serviceMaintenanceCause(t, err)
						serviceRemovalSystemdNoEffects(t, m)
						serviceMaintenanceUnchanged(t, m.cell, before)
						if !reflect.DeepEqual(artifacts, unixServiceArtifactSnapshot(t, a.unitDir)) {
							t.Fatal("Systemd fence changed registration artifacts")
						}
						if m.loaded != loaded || m.active != loaded || m.disabled {
							t.Fatal("Systemd fence changed manager registration")
						}
					} else {
						if err != nil {
							t.Fatal("Systemd absent fence refused removal")
						}
						want := unixSystemdBaseTrace(a, d, loaded)
						want = append(want, unixSystemdBaseTrace(a, d, loaded)...)
						want = append(want, []string{a.systemctl, "--user", "disable", "--now", serviceUnitLabel(d)}, []string{a.systemctl, "--user", "daemon-reload"})
						if !reflect.DeepEqual(m.calls, want) {
							t.Fatal("Systemd completed removal changed its command trace")
						}
						if _, err := os.Lstat(a.unitPath(d)); !errors.Is(err, os.ErrNotExist) || !m.disabled || m.loaded || m.active {
							t.Fatal("Systemd completed removal retained registration")
						}
						serviceMaintenanceUnchanged(t, m.cell, before)
					}
					serviceMaintenanceReleased(t, m.cell)
					serviceRemovalSystemdHostFile(t, m.cell)
				})
			}
		})
	}
}

func TestServiceAdapterRemovalDrainOutsideLockSystemd(t *testing.T) {
	a, d, m := serviceRemovalSystemdFixture(t, true)
	called := false
	a.drain = func(_ context.Context, value ServiceDescriptor) error {
		lock, err := unixServiceContender(t, m.cell).own(false)
		if err != nil {
			t.Fatal("Systemd drain could not acquire operation lock")
		}
		release := serviceRemovalHoldHost(t, m.cell)
		release()
		if lock.Close() != nil {
			t.Fatal("Systemd drain operation lock release failed")
		}
		serviceRemovalStopped(t, m.cell, value)
		called = true
		return nil
	}
	if a.Remove(t.Context(), d) != nil || !called {
		t.Fatal("Systemd drain did not complete before removal")
	}
}

func TestServiceAdapterRemovalFreshIdentitySystemd(t *testing.T) {
	for _, change := range []string{"installation", "generation", "running"} {
		t.Run(change, func(t *testing.T) {
			a, d, m := serviceRemovalSystemdFixture(t, true)
			a.drain = func(_ context.Context, fresh ServiceDescriptor) error {
				lock, err := m.cell.own(false)
				if err != nil {
					t.Fatal("Systemd fresh identity lock acquisition failed")
				}
				defer func() { _ = lock.Close() }()
				switch change {
				case "installation":
					fresh.InstallationID += "-new"
					manifest, err := readManifest(d.Root)
					if err != nil {
						t.Fatal("Systemd changed installation manifest read failed")
					}
					manifest.InstallationID = fresh.InstallationID
					if writeAtomic(m.cell.path("manifest.json"), manifest, nil) != nil {
						t.Fatal("Systemd changed installation manifest write failed")
					}
				case "generation":
					fresh.Payload.Generation += "-new"
				case "running":
					fresh.DesiredState = "running"
				}
				fresh.Revision++
				unixServicePersistDescriptor(t, m.cell, fresh)
				return nil
			}
			detail := "stale_identity"
			if change == "running" {
				detail = "not_stopped"
			}
			unixServiceRequireError(t, a.Remove(t.Context(), d), ErrServiceOwnershipConflict, "remove:descriptor", detail)
			serviceRemovalSystemdNoEffects(t, m)
			if _, err := os.Lstat(a.unitPath(d)); err != nil {
				t.Fatal("Systemd fresh identity refusal lost unit evidence")
			}
		})
	}
}

func TestServiceAdapterRemovalPrecedenceSystemd(t *testing.T) {
	for _, pair := range []string{"preflight_drain", "drain_lock", "lock_identity", "identity_stopped", "stopped_host", "host_fence", "confinement_fence"} {
		t.Run(pair, func(t *testing.T) {
			a, d, m := serviceRemovalSystemdFixture(t, true)
			serviceMaintenanceFence(t, m.cell, "Valid")
			var release func()
			sentinel, step, detail := ErrServiceOwnershipConflict, "remove:descriptor", "stale_identity"
			switch pair {
			case "preflight_drain":
				m.mismatch = "arguments"
				a.drain = func(context.Context, ServiceDescriptor) error {
					t.Fatal("Systemd preflight refusal reached drain callback")
					return errors.New("fixture drain failure")
				}
				step, detail = "inspect:exec_start", "argv_length_mismatch"
			case "drain_lock", "lock_identity":
				lock, err := unixServiceContender(t, m.cell).own(false)
				if err != nil {
					t.Fatal("Systemd precedence lock fixture failed")
				}
				release = func() { _ = lock.Close() }
				if pair == "drain_lock" {
					a.drain = func(context.Context, ServiceDescriptor) error { return errors.New("fixture drain failure") }
					sentinel, step, detail = ErrServiceRemovalFailed, "remove:host_exit", "host_exit_failed"
				} else {
					a.drain = func(context.Context, ServiceDescriptor) error { return nil }
					fresh := d
					fresh.Payload.Generation += "-new"
					unixServicePersistDescriptor(t, m.cell, fresh)
					sentinel = ErrLocked
				}
			case "identity_stopped":
				a.drain = func(context.Context, ServiceDescriptor) error { return nil }
				fresh := d
				fresh.DesiredState, fresh.Payload.Generation = "running", "changed"
				unixServicePersistDescriptor(t, m.cell, fresh)
			case "stopped_host":
				a.drain = func(context.Context, ServiceDescriptor) error { return nil }
				fresh := d
				fresh.DesiredState = "running"
				unixServicePersistDescriptor(t, m.cell, fresh)
				release = serviceRemovalHoldHost(t, m.cell)
				detail = "not_stopped"
			case "host_fence":
				release = serviceRemovalHoldHost(t, m.cell)
				sentinel, step, detail = ErrServiceHostActive, "remove:host_lease", "active"
			case "confinement_fence":
				a.unitDir = filepath.Join(d.Root, "units")
				a.unitBase = d.Root
				unixSystemdSeed(t, a, d)
				step, detail = "remove:confinement", "overlap"
			}
			if release != nil {
				defer release()
			}
			err := a.Remove(t.Context(), d)
			if sentinel == ErrLocked {
				if err != ErrLocked {
					t.Fatal("Systemd lock precedence lost exact contention cause")
				}
			} else {
				unixServiceRequireError(t, err, sentinel, step, detail)
			}
			serviceRemovalSystemdNoEffects(t, m)
		})
	}
}

func TestServiceAdapterRemovalHostExclusionSystemd(t *testing.T) {
	t.Run("gap_winner", func(t *testing.T) {
		a, d, m := serviceRemovalSystemdFixture(t, true)
		var release func()
		a.drain = func(_ context.Context, value ServiceDescriptor) error {
			serviceRemovalStopped(t, m.cell, value)
			release = serviceRemovalHoldHost(t, m.cell)
			return nil
		}
		err := a.Remove(t.Context(), d)
		if release != nil {
			release()
		}
		unixServiceRequireError(t, err, ErrServiceHostActive, "remove:host_lease", "active")
		serviceRemovalSystemdNoEffects(t, m)
		serviceMaintenanceReleased(t, m.cell)
	})
	t.Run("never_hosted_continuous", func(t *testing.T) {
		a, d, m := serviceRemovalSystemdFixture(t, true)
		if _, err := os.Lstat(m.cell.path(".service-host.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("Systemd never hosted fixture already has host lock")
		}
		lease := []byte("retained diagnostic lease")
		if os.WriteFile(m.cell.path("service", "lease.json"), lease, 0o600) != nil {
			t.Fatal("Systemd retained lease fixture creation failed")
		}
		before := serviceMaintenanceSnapshot(t, m.cell)
		seen := map[string]bool{}
		var order []string
		removing := false
		a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
			if binary == a.systemctl && len(args) > 1 && (args[1] == "disable" || args[1] == "daemon-reload") {
				serviceRemovalLocksHeld(t, m.cell)
				seen[args[1]] = true
				order = append(order, args[1])
				removing = args[1] == "disable"
			}
			return m.command(ctx, binary, args...)
		}
		a.inspectFile = func(path string) (os.FileInfo, error) {
			if removing && path == a.unitPath(d) {
				serviceRemovalLocksHeld(t, m.cell)
				seen["owned_unit_check"] = true
				order = append(order, "owned_unit_check")
			}
			return os.Lstat(path)
		}
		a.removeFile = func(path string) error {
			serviceRemovalLocksHeld(t, m.cell)
			seen["final_unlink"] = true
			order = append(order, "final_unlink")
			return os.Remove(path)
		}
		if a.Remove(t.Context(), d) != nil {
			t.Fatal("Systemd continuous lock removal failed")
		}
		for _, site := range []string{"disable", "owned_unit_check", "final_unlink", "daemon-reload"} {
			if !seen[site] {
				t.Fatal("Systemd continuous lock callback was not reached")
			}
		}
		if !reflect.DeepEqual(order, []string{"disable", "owned_unit_check", "final_unlink", "daemon-reload"}) {
			t.Fatal("Systemd removal callbacks changed order")
		}
		after := serviceMaintenanceSnapshot(t, m.cell)
		delete(after, ".service-host.lock")
		if !reflect.DeepEqual(before, after) {
			t.Fatal("Systemd lock acquisition changed retained cell data")
		}
		info, err := os.Lstat(m.cell.path(".service-host.lock"))
		if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
			t.Fatal("Systemd removal lost stable empty host lock")
		}
		serviceMaintenanceReleased(t, m.cell)
		serviceRemovalSystemdHostFile(t, m.cell)
	})
}

func TestServiceAdapterRemovalRetrySystemd(t *testing.T) {
	a, d, m := serviceRemovalSystemdFixture(t, true)
	serviceMaintenanceFence(t, m.cell, "Valid")
	serviceMaintenanceCause(t, a.Remove(t.Context(), d))
	serviceRemovalSystemdNoEffects(t, m)
	if os.Remove(m.cell.path("journal", "maintenance.json")) != nil {
		t.Fatal("Systemd retry fixture fence removal failed")
	}
	if a.Remove(t.Context(), d) != nil {
		t.Fatal("Systemd removal did not recover after fence removal")
	}
	if a.Remove(t.Context(), d) != nil {
		t.Fatal("Systemd completed removal was not idempotent")
	}
}

func TestServiceAdapterRemovalThenApplySystemd(t *testing.T) {
	a, d, m := serviceRemovalSystemdFixture(t, true)
	if a.Remove(t.Context(), d) != nil {
		t.Fatal("Systemd reinstall fixture removal failed")
	}
	fresh, err := m.cell.serviceDescriptorForOwner(a.uid)
	if err != nil || fresh.DesiredState != "stopped" {
		t.Fatal("Systemd completed removal lost stopped intent")
	}
	m.calls = nil
	if a.Apply(t.Context(), ServicePlan{Spec: fresh, RequiredPrivilege: "user"}) != nil {
		t.Fatal("Systemd stopped intent changed apply admission")
	}
	if !m.active || !m.loaded {
		t.Fatal("Systemd stopped intent changed manager enable policy")
	}
}

func TestServiceAdapterRemovalRealDrainSystemd(t *testing.T) {
	for _, tree := range []string{"empty", "unknown", "live"} {
		t.Run(tree, func(t *testing.T) {
			a, d, m := serviceRemovalSystemdFixture(t, true)
			owner, err := serviceOwner()
			if err != nil {
				t.Fatal("Systemd real drain fixture owner unavailable")
			}
			d.UID, a.uid = owner, owner
			m.descriptor = d
			unixServicePersistDescriptor(t, m.cell, d)
			unixSystemdSeed(t, a, d)
			a.drain = nil
			switch tree {
			case "unknown":
				if os.WriteFile(m.cell.path("run", "worker.json"), []byte("{"), 0o600) != nil {
					t.Fatal("Systemd real drain unknown child fixture failed")
				}
			case "live":
				windowsTaskRecordedLiveChild(t, m.cell, "worker")
			}
			artifacts := unixServiceArtifactSnapshot(t, a.unitDir)
			childBefore := serviceMaintenanceSnapshot(t, m.cell)["run/worker.json"]
			err = a.Remove(t.Context(), d)
			if tree == "empty" {
				if err != nil {
					t.Fatal("Systemd real drain refused empty child tree")
				}
			} else {
				unixServiceRequireError(t, err, ErrServiceRemovalFailed, "remove:host_exit", "host_exit_failed")
				serviceRemovalSystemdNoEffects(t, m)
				if !reflect.DeepEqual(artifacts, unixServiceArtifactSnapshot(t, a.unitDir)) {
					t.Fatal("Systemd real drain refusal changed registration")
				}
				childAfter, retained := serviceMaintenanceSnapshot(t, m.cell)["run/worker.json"]
				if !retained || childAfter != childBefore {
					t.Fatal("Systemd real drain refusal changed child evidence")
				}
			}
		})
	}
}

func TestServiceAdapterRemovalRetainsCellDataSystemd(t *testing.T) {
	a, d, m := serviceRemovalSystemdFixture(t, true)
	serviceRemovalSystemdHostFile(t, m.cell)
	if os.MkdirAll(m.cell.path("bystander", "nested"), 0o700) != nil || os.WriteFile(m.cell.path("bystander", "nested", "value"), []byte("retained cell data"), 0o600) != nil {
		t.Fatal("Systemd retained cell tree fixture failed")
	}
	sibling := filepath.Join(a.unitDir, "bystander.service")
	if os.WriteFile(sibling, []byte("retained sibling"), 0o600) != nil {
		t.Fatal("Systemd retained registration sibling fixture failed")
	}
	before := serviceMaintenanceSnapshot(t, m.cell)
	if a.Remove(t.Context(), d) != nil {
		t.Fatal("Systemd retained data removal failed")
	}
	serviceMaintenanceUnchanged(t, m.cell, before)
	data, err := os.ReadFile(sibling)
	if err != nil || string(data) != "retained sibling" {
		t.Fatal("Systemd removal changed registration sibling")
	}
}
