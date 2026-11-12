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

func unixSystemdReadTrace(t *testing.T, a *systemdUserAdapter, d ServiceDescriptor, m *systemdFixtureManager) [][]string {
	t.Helper()
	m.calls = nil
	_, _ = a.Inspect(t.Context(), d)
	calls := m.calls
	m.calls = nil
	return calls
}

func unixSystemdUnchanged(t *testing.T, a *systemdUserAdapter, m *systemdFixtureManager, cell, artifact map[string]serviceMaintenanceEntry, calls [][]string) {
	t.Helper()
	serviceMaintenanceUnchanged(t, m.cell, cell)
	if artifact != nil && !reflect.DeepEqual(artifact, unixServiceArtifactSnapshot(t, a.unitDir)) {
		t.Fatal("Systemd refusal changed registration tree")
	}
	if !reflect.DeepEqual(calls, m.calls) {
		t.Fatal("Systemd refusal changed inspection trace")
	}
}

func unixSystemdSeed(t *testing.T, a *systemdUserAdapter, d ServiceDescriptor) {
	t.Helper()
	if a.inspectDirectories(true) != nil {
		t.Fatal("Systemd seed directory creation failed")
	}
	data, err := RenderSystemdUserUnit(d)
	if err != nil || os.WriteFile(a.unitPath(d), data, 0o600) != nil {
		t.Fatal("Systemd seed unit creation failed")
	}
}

func TestUnixServiceMaintenanceApplyAdmissionSystemd(t *testing.T) {
	for _, state := range []string{"fresh", "reused"} {
		t.Run(state, func(t *testing.T) {
			for _, kind := range unixServiceFenceKinds() {
				t.Run(kind, func(t *testing.T) {
					a, d, m := systemdFixture(t)
					if state == "reused" {
						unixSystemdSeed(t, a, d)
					}
					serviceMaintenanceFence(t, m.cell, kind)
					calls := unixSystemdReadTrace(t, a, d, m)
					cell, artifact := serviceMaintenanceSnapshot(t, m.cell), unixServiceArtifactSnapshot(t, a.unitDir)
					serviceMaintenanceCause(t, a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"}))
					unixSystemdUnchanged(t, a, m, cell, artifact, calls)
					serviceMaintenanceReleased(t, m.cell)
				})
			}
		})
	}
}

func TestUnixServiceMaintenanceApplyPrecedenceSystemd(t *testing.T) {
	cases := []string{"non_user", "manager_unavailable", "uid", "confinement", "contender", "descriptor_missing", "descriptor_corrupt", "immutable_installation", "immutable_host", "revision_drift_under_fence", "revision_drift_without_fence", "stopped"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			a, d, m := systemdFixture(t)
			plan := ServicePlan{Spec: d, RequiredPrivilege: "user"}
			if name != "revision_drift_without_fence" && name != "stopped" {
				serviceMaintenanceFence(t, m.cell, "Valid")
			}
			var held *operationLock
			var expected error
			step, detail := "apply:descriptor", "unavailable"
			switch name {
			case "non_user":
				plan.RequiredPrivilege = "root"
				expected = ErrServiceOwnershipConflict
				step, detail = "apply:privilege", "not_user"
			case "manager_unavailable":
				m.unavailable = true
				expected = ErrServiceManagerUnavailable
			case "uid":
				plan.Spec.UID = "1002"
				expected = ErrServiceOwnershipConflict
				step, detail = "inspect:uid", "not_current_uid"
			case "confinement":
				a.unitDir = filepath.Join(d.Root, "units")
				a.unitBase = d.Root
				expected = ErrServiceOwnershipConflict
				step, detail = "apply:confinement", "overlap"
			case "contender":
				other, err := New(m.cell.cfg)
				if err != nil {
					t.Fatal("Systemd contender construction failed")
				}
				held, err = other.own(false)
				if err != nil {
					t.Fatal("Systemd contender acquisition failed")
				}
				defer func() { _ = held.Close() }()
				expected = ErrLocked
			case "descriptor_missing":
				if os.Remove(m.cell.path("service", "descriptor.json")) != nil {
					t.Fatal("Systemd missing descriptor setup failed")
				}
				expected = ErrServiceOwnershipConflict
			case "descriptor_corrupt":
				if os.WriteFile(m.cell.path("service", "descriptor.json"), []byte("{"), 0o600) != nil {
					t.Fatal("Systemd corrupt descriptor setup failed")
				}
				expected = ErrServiceOwnershipConflict
			case "immutable_installation":
				plan.Spec.InstallationID += "-other"
				expected = ErrServiceOwnershipConflict
				detail = "stale_identity"
			case "immutable_host":
				plan.Spec.Payload.Host += "-other"
				expected = ErrServiceOwnershipConflict
				detail = "stale_identity"
			case "revision_drift_under_fence":
				plan.Spec.Revision++
			case "revision_drift_without_fence":
				plan.Spec.Revision++
				expected = ErrServiceOwnershipConflict
				detail = "stale_plan"
			case "stopped":
				d.DesiredState = "stopped"
				plan.Spec = d
				unixServicePersistDescriptor(t, m.cell, d)
			}
			calls := unixSystemdReadTrace(t, a, plan.Spec, m)
			if name == "non_user" {
				calls = nil
			}
			var prefixError error
			if name == "manager_unavailable" {
				_, prefixError = a.Inspect(t.Context(), plan.Spec)
				m.calls = nil
			}
			cell := serviceMaintenanceSnapshot(t, m.cell)
			var artifact map[string]serviceMaintenanceEntry
			if name != "confinement" {
				artifact = unixServiceArtifactSnapshot(t, a.unitDir)
			}
			err := a.Apply(t.Context(), plan)
			switch name {
			case "stopped":
				if err != nil {
					t.Fatal("Systemd stopped descriptor did not preserve admission")
				}
			case "revision_drift_under_fence":
				serviceMaintenanceCause(t, err)
			case "contender":
				if err != ErrLocked {
					t.Fatal("Systemd contender lost exact lock error")
				}
			case "manager_unavailable":
				if !errors.Is(err, expected) || err.Error() != prefixError.Error() {
					t.Fatal("Systemd prefix refusal changed its error")
				}
			default:
				unixServiceRequireError(t, err, expected, step, detail)
			}
			if name != "stopped" {
				unixSystemdUnchanged(t, a, m, cell, artifact, calls)
			}
			if held != nil {
				if held.Close() != nil {
					t.Fatal("Systemd held contender release failed")
				}
				held = nil
			}
			serviceMaintenanceReleased(t, m.cell)
		})
	}
}

func TestUnixServiceMaintenanceApplyContentionSystemd(t *testing.T) {
	t.Run("contender", func(t *testing.T) {
		a, d, m := systemdFixture(t)
		other, err := New(m.cell.cfg)
		if err != nil {
			t.Fatal("Systemd second cell construction failed")
		}
		lock, err := other.own(false)
		if err != nil {
			t.Fatal("Systemd second cell lock failed")
		}
		defer func() { _ = lock.Close() }()
		calls := unixSystemdReadTrace(t, a, d, m)
		cell, artifact := serviceMaintenanceSnapshot(t, m.cell), unixServiceArtifactSnapshot(t, a.unitDir)
		if a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"}) != ErrLocked {
			t.Fatal("Systemd held lock did not refuse exactly")
		}
		unixSystemdUnchanged(t, a, m, cell, artifact, calls)
		if lock.Close() != nil {
			t.Fatal("Systemd second cell release failed")
		}
		lock = nil
		if a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"}) != nil {
			t.Fatal("Systemd apply did not recover after contention")
		}
		serviceMaintenanceReleased(t, m.cell)
	})
	t.Run("continuous", func(t *testing.T) {
		a, d, m := systemdFixture(t)
		other, err := New(m.cell.cfg)
		if err != nil {
			t.Fatal("Systemd continuous contender construction failed")
		}
		seen := map[string]bool{}
		a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
			site := ""
			if binary == a.systemctl && len(args) > 1 {
				if args[1] == "daemon-reload" || args[1] == "enable" {
					site = args[1]
				}
				if args[1] == "show" && m.active {
					site = "post_enable"
				}
			}
			if site != "" {
				lock, lockErr := other.own(false)
				if lock != nil {
					_ = lock.Close()
				}
				if lockErr != ErrLocked {
					t.Fatal("Systemd manager effect escaped operation lock")
				}
				seen[site] = true
			}
			return m.command(ctx, binary, args...)
		}
		if a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"}) != nil {
			t.Fatal("Systemd continuous admission failed")
		}
		for _, site := range []string{"daemon-reload", "enable", "post_enable"} {
			if !seen[site] {
				t.Fatal("Systemd continuous lock probe was not reached")
			}
		}
		serviceMaintenanceReleased(t, m.cell)
	})
}

func unixSystemdBaseTrace(a *systemdUserAdapter, d ServiceDescriptor, loaded bool) [][]string {
	calls := [][]string{
		{a.systemctl, "--user", "show", "--property=Version", "--value"},
		{a.busctl, "--user", "--json=short", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "Version"},
		{a.systemctl, "--user", "show", serviceUnitLabel(d), "--property=Id,LoadState,FragmentPath,DropInPaths", "--no-pager"},
	}
	if loaded {
		calls = append(calls, []string{a.busctl, "--user", "--json=short", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "LoadUnit", "s", serviceUnitLabel(d)}, []string{a.busctl, "--user", "--json=short", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/fixture", "org.freedesktop.systemd1.Service", "ExecStart"})
	}
	return calls
}

func TestUnixServiceMaintenanceApplyAbsentParitySystemd(t *testing.T) {
	for _, name := range []string{"seeded", "lockless"} {
		t.Run(name, func(t *testing.T) {
			a, d, m := systemdFixture(t)
			if os.RemoveAll(m.cell.path("journal")) != nil {
				t.Fatal("Systemd absent journal preparation failed")
			}
			if name == "lockless" {
				for _, path := range []string{m.cell.path(".owner.lock"), m.cell.path("run", "operation.lock")} {
					if os.Remove(path) != nil {
						t.Fatal("Systemd lockless preparation failed")
					}
				}
			}
			for iteration := 0; iteration < 2; iteration++ {
				m.calls = nil
				if a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"}) != nil {
					t.Fatal("Systemd absent fence apply failed")
				}
				want := unixSystemdBaseTrace(a, d, iteration > 0)
				want = append(want, []string{a.systemctl, "--user", "daemon-reload"})
				want = append(want, unixSystemdBaseTrace(a, d, true)...)
				want = append(want, []string{a.systemctl, "--user", "enable", "--now", serviceUnitLabel(d)})
				want = append(want, unixSystemdBaseTrace(a, d, true)...)
				if !reflect.DeepEqual(want, m.calls) {
					t.Fatal("Systemd absent fence changed base manager trace")
				}
				if _, err := os.Lstat(m.cell.path("journal")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("Systemd absent fence created journal parent")
				}
				if _, err := os.Lstat(m.cell.path("journal", "maintenance.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("Systemd absent fence created evidence")
				}
				serviceMaintenanceReleased(t, m.cell)
			}
		})
	}
}

func TestUnixServiceMaintenanceApplyCleanupSystemd(t *testing.T) {
	for _, name := range []string{"write_failure", "directory_sync_failure", "reload_failure", "post_reload_failure", "replacement", "enable_failure", "post_enable_failure"} {
		t.Run(name, func(t *testing.T) {
			a, d, m := systemdFixture(t)
			created := 0
			var identity os.FileInfo
			a.writeUnit = func(file *os.File, data []byte) error {
				created++
				var err error
				identity, err = file.Stat()
				if err != nil {
					t.Fatal("Systemd created identity inspection failed")
				}
				if _, err := file.Write(data); err != nil {
					t.Fatal("Systemd cleanup fixture persistence failed")
				}
				if name == "write_failure" {
					return errors.New("fixture write failure")
				}
				if name == "directory_sync_failure" {
					if os.Chmod(a.unitDir, 0o300) != nil {
						t.Fatal("Systemd sync failure preparation failed")
					}
				}
				return file.Sync()
			}
			sentinel, step, detail := ErrServiceRegistrationFailed, "unit_file:write", "fixture write failure"
			switch name {
			case "directory_sync_failure":
				step, detail = "unit_dir:sync_created", "cannot open local cell directory for sync"
			case "reload_failure", "replacement":
				m.failReload = true
				step, detail = "daemon_reload", "fixture reload denied"
			case "post_reload_failure":
				m.failExecBus = true
				step, detail = "post_reload", "fixture bus unavailable"
			case "enable_failure":
				m.failEnable = true
				step, detail = "enable_now", "fixture registration failure"
			case "post_enable_failure":
				m.mismatchAfterEnable = "drop-in"
				sentinel = ErrServiceOwnershipConflict
				step, detail = "post_enable", "mismatch"
			}
			replaced := false
			a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
				if name == "replacement" && !replaced && binary == a.systemctl && len(args) > 1 && args[1] == "daemon-reload" {
					path := a.unitPath(d)
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal("Systemd replacement source read failed")
					}
					if os.Rename(path, path+".original") != nil || os.WriteFile(path, data, 0o600) != nil {
						t.Fatal("Systemd replacement publication failed")
					}
					replaced = true
				}
				return m.command(ctx, binary, args...)
			}
			err := a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"})
			if name == "directory_sync_failure" {
				if os.Chmod(a.unitDir, 0o700) != nil {
					t.Fatal("Systemd sync fixture mode restoration failed")
				}
			}
			unixServiceRequireError(t, err, sentinel, step, detail)
			if name == "post_reload_failure" && !errors.Is(err, ErrServiceManagerUnavailable) {
				t.Fatal("Systemd cleanup lost loaded unit bus cause")
			}
			info, statErr := os.Stat(a.unitPath(d))
			retained := name == "post_reload_failure" || name == "replacement" || name == "enable_failure" || name == "post_enable_failure"
			if retained {
				if statErr != nil {
					t.Fatal("Systemd enabling failure removed recovery evidence")
				}
				if name == "replacement" {
					if os.SameFile(identity, info) {
						t.Fatal("Systemd replacement fixture reused original inode")
					}
					original, err := os.Stat(a.unitPath(d) + ".original")
					if err != nil || !os.SameFile(identity, original) {
						t.Fatal("Systemd replacement lost original created inode")
					}
				} else if !os.SameFile(identity, info) {
					t.Fatal("Systemd cleanup changed retained registration inode")
				}
				want, renderErr := RenderSystemdUserUnit(d)
				data, readErr := os.ReadFile(a.unitPath(d))
				if renderErr != nil || readErr != nil || string(data) != string(want) {
					t.Fatal("Systemd cleanup changed retained registration bytes")
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("Systemd pre enable failure retained created inode")
			}
			wantCalls := unixSystemdBaseTrace(a, d, false)
			switch name {
			case "reload_failure", "replacement":
				wantCalls = append(wantCalls, []string{a.systemctl, "--user", "daemon-reload"})
				wantCalls = append(wantCalls, unixSystemdBaseTrace(a, d, false)...)
				wantCalls = append(wantCalls, []string{a.systemctl, "--user", "daemon-reload"})
			case "post_reload_failure":
				wantCalls = append(wantCalls, []string{a.systemctl, "--user", "daemon-reload"})
				failedRead := unixSystemdBaseTrace(a, d, true)
				failedRead = failedRead[:len(failedRead)-1]
				wantCalls = append(wantCalls, failedRead...)
				wantCalls = append(wantCalls, failedRead...)
			case "enable_failure", "post_enable_failure":
				wantCalls = append(wantCalls, []string{a.systemctl, "--user", "daemon-reload"})
				wantCalls = append(wantCalls, unixSystemdBaseTrace(a, d, true)...)
				wantCalls = append(wantCalls, []string{a.systemctl, "--user", "enable", "--now", serviceUnitLabel(d)})
				if name == "post_enable_failure" {
					wantCalls = append(wantCalls, unixSystemdBaseTrace(a, d, false)...)
				}
			}
			if !reflect.DeepEqual(wantCalls, m.calls) {
				t.Fatal("Systemd cleanup changed expected manager trace")
			}
			reloads, enables := 0, 0
			for _, call := range m.calls {
				if call[0] == a.systemctl && len(call) > 2 {
					switch call[2] {
					case "disable":
						t.Fatal("Systemd cleanup issued stop under operation lock")
					case "daemon-reload":
						reloads++
					case "enable":
						enables++
					}
				}
			}
			if (name == "reload_failure" || name == "replacement") && reloads != 2 {
				t.Fatal("Systemd reload failure omitted best effort reload")
			}
			if (name == "enable_failure" || name == "post_enable_failure") && (reloads != 1 || enables != 1) {
				t.Fatal("Systemd enabling failure issued extra manager mutation")
			}
			serviceMaintenanceReleased(t, m.cell)
			if name == "post_reload_failure" || name == "enable_failure" || name == "post_enable_failure" {
				m.failExecBus = false
				m.failEnable = false
				m.mismatch = ""
				m.mismatchAfterEnable = ""
				if a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"}) != nil {
					t.Fatal("Systemd retained registration did not recover")
				}
				after, err := os.Stat(a.unitPath(d))
				if err != nil || created != 1 || !os.SameFile(info, after) {
					t.Fatal("Systemd recovery created another registration inode")
				}
				serviceMaintenanceReleased(t, m.cell)
			}
		})
	}
}
