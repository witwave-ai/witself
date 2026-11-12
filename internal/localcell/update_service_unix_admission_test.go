package localcell

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func unixServiceFixtureCell(t *testing.T, rootName string, d ServiceDescriptor) (*Cell, ServiceDescriptor) {
	t.Helper()
	base, manifest := unitCell(t)
	root := filepath.Join(base.cfg.Root, rootName)
	if privateDir(root, true) != nil {
		t.Fatal("Unix fixture root creation failed")
	}
	c, err := New(Config{Root: root, Timeout: time.Second})
	if err != nil || c.layout() != nil {
		t.Fatal("Unix fixture cell creation failed")
	}
	oldRoot := d.Root
	d.Root, d.Version, d.Revision, d.ResourceProfile = root, 1, 1, "unconfigured"
	d.Payload.Args = append([]string(nil), d.Payload.Args...)
	for i, arg := range d.Payload.Args {
		if arg == oldRoot {
			d.Payload.Args[i] = root
		}
	}
	manifest.InstallationID = d.InstallationID
	if writeAtomic(c.path("manifest.json"), manifest, nil) != nil {
		t.Fatal("Unix fixture manifest persistence failed")
	}
	unixServicePersistDescriptor(t, c, d)
	lock, err := c.own(false)
	if err != nil {
		t.Fatal("Unix fixture lock creation failed")
	}
	if lock.Close() != nil {
		t.Fatal("Unix fixture lock release failed")
	}
	return c, d
}

func unixServicePersistDescriptor(t *testing.T, c *Cell, d ServiceDescriptor) {
	t.Helper()
	if privateDir(c.path("service"), true) != nil || writeAtomic(c.path("service", "descriptor.json"), d, nil) != nil {
		t.Fatal("Unix fixture descriptor persistence failed")
	}
}

func unixServiceFenceKinds() []string {
	kinds := []string{"Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	return kinds
}

// Registration artifacts live outside the cell. Cell snapshots always use the
// existing helper, which never opens an empty mandatory Windows lock file.
func unixServiceArtifactSnapshot(t *testing.T, dir string) map[string]serviceMaintenanceEntry {
	t.Helper()
	result := map[string]serviceMaintenanceEntry{}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return result
	}
	err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		item := serviceMaintenanceEntry{mode: info.Mode()}
		if info.Mode()&os.ModeSymlink != 0 {
			item.data, err = os.Readlink(path)
		} else if info.Mode().IsRegular() {
			var data []byte
			data, err = os.ReadFile(path)
			item.data = string(data)
		}
		result[filepath.ToSlash(name)] = item
		return err
	})
	if err != nil {
		t.Fatal("Unix artifact snapshot failed")
	}
	return result
}

func unixServiceRequireError(t *testing.T, err, sentinel error, step, detail string) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatal("Unix admission refusal lost its sentinel")
	}
	if !strings.Contains(err.Error(), ": "+step+": ") {
		t.Fatal("Unix admission refusal lost its step")
	}
	if detail != "" && !strings.HasSuffix(err.Error(), ": "+detail) {
		t.Fatal("Unix admission refusal lost its detail")
	}
}

func unixServiceContender(t *testing.T, c *Cell) *Cell {
	t.Helper()
	other, err := New(c.cfg)
	if err != nil {
		t.Fatal("Unix contender construction failed")
	}
	return other
}

func unixLaunchdUnchanged(t *testing.T, a *launchdAgentAdapter, m *launchdFixtureManager, cellBefore, artifacts map[string]serviceMaintenanceEntry) {
	t.Helper()
	for _, call := range m.allCalls {
		if len(call) < 2 || call[1] != "print" {
			t.Fatal("Launchd refusal issued an unexpected command")
		}
	}
	if artifacts != nil && !reflect.DeepEqual(artifacts, unixServiceArtifactSnapshot(t, a.agentDir)) {
		t.Fatal("Launchd refusal changed registration artifacts")
	}
	serviceMaintenanceUnchanged(t, m.cell, cellBefore)
}

func TestUnixServiceMaintenanceApplyAdmissionLaunchd(t *testing.T) {
	for _, state := range []string{"fresh", "reused", "owned"} {
		t.Run(state, func(t *testing.T) {
			for _, kind := range unixServiceFenceKinds() {
				t.Run(kind, func(t *testing.T) {
					a, d, m := launchdFixture(t)
					defer serviceMaintenanceReleased(t, m.cell)
					if state != "fresh" {
						launchdDisabledSeedPlist(t, a, d)
						m.loaded = state == "owned"
					}
					serviceMaintenanceFence(t, m.cell, kind)
					before, artifacts := serviceMaintenanceSnapshot(t, m.cell), unixServiceArtifactSnapshot(t, a.agentDir)
					serviceMaintenanceCause(t, a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"}))
					unixLaunchdUnchanged(t, a, m, before, artifacts)
				})
			}
		})
	}
}

func TestUnixServiceMaintenanceApplyPrecedenceLaunchd(t *testing.T) {
	for _, name := range []string{"non_user", "manager_unavailable", "uid", "confinement", "contender", "missing_descriptor", "corrupt_descriptor", "immutable_installation", "immutable_payload", "revision_drift_under_fence", "revision_drift_without_fence", "stopped"} {
		t.Run(name, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			defer serviceMaintenanceReleased(t, m.cell)
			plan := ServicePlan{Spec: d, RequiredPrivilege: "user"}
			sentinel, step, detail := ErrServiceOwnershipConflict, "apply:descriptor", "unavailable"
			var held *operationLock
			var prefixErr error
			switch name {
			case "non_user":
				plan.RequiredPrivilege = "root"
				step, detail = "apply:privilege", "not_user"
			case "manager_unavailable":
				m.noGUI = true
				_, prefixErr = a.Inspect(t.Context(), d)
				sentinel, step, detail = ErrServiceManagerUnavailable, "capability:gui_domain", ""
			case "uid":
				plan.Spec.UID = "1002"
				step, detail = "inspect:uid", "not_current_uid"
			case "confinement":
				a.home = d.Root
				a.agentDir = filepath.Join(a.home, "Library", "LaunchAgents")
				step, detail = "apply:confinement", "overlap"
			case "contender":
				var err error
				held, err = unixServiceContender(t, m.cell).own(false)
				if err != nil {
					t.Fatal("Launchd precedence contender failed")
				}
			case "missing_descriptor":
				if os.Remove(m.cell.path("service", "descriptor.json")) != nil {
					t.Fatal("Launchd descriptor removal failed")
				}
			case "corrupt_descriptor":
				if writeAtomicBytes(m.cell.path("service", "descriptor.json"), []byte("{"), nil) != nil {
					t.Fatal("Launchd descriptor corruption failed")
				}
			case "immutable_installation":
				fresh := d
				fresh.InstallationID += "-other"
				unixServicePersistDescriptor(t, m.cell, fresh)
				manifest, err := readManifest(m.cell.cfg.Root)
				if err != nil {
					t.Fatal("Launchd manifest read failed")
				}
				manifest.InstallationID = fresh.InstallationID
				if writeAtomic(m.cell.path("manifest.json"), manifest, nil) != nil {
					t.Fatal("Launchd manifest rewrite failed")
				}
				detail = "stale_identity"
			case "immutable_payload":
				fresh := d
				fresh.Payload.Host += "-other"
				unixServicePersistDescriptor(t, m.cell, fresh)
				detail = "stale_identity"
			case "revision_drift_under_fence", "revision_drift_without_fence":
				fresh := d
				fresh.Revision++
				unixServicePersistDescriptor(t, m.cell, fresh)
				detail = "stale_plan"
			case "stopped":
				plan.Spec.DesiredState = "stopped"
				unixServicePersistDescriptor(t, m.cell, plan.Spec)
			}
			if name != "revision_drift_without_fence" && name != "stopped" {
				serviceMaintenanceFence(t, m.cell, "Valid")
			}
			before := serviceMaintenanceSnapshot(t, m.cell)
			var artifacts map[string]serviceMaintenanceEntry
			if name != "confinement" {
				artifacts = unixServiceArtifactSnapshot(t, a.agentDir)
			}
			m.calls, m.allCalls = nil, nil
			err := a.Apply(t.Context(), plan)
			if held != nil {
				if held.Close() != nil {
					t.Fatal("Launchd precedence lock release failed")
				}
			}
			switch name {
			case "contender":
				if err != ErrLocked {
					t.Fatal("Launchd contention lost exact lock identity")
				}
			case "revision_drift_under_fence":
				serviceMaintenanceCause(t, err)
			case "stopped":
				if err != nil || !m.loaded {
					t.Fatal("Launchd stopped descriptor lost baseline admission")
				}
				return
			default:
				unixServiceRequireError(t, err, sentinel, step, detail)
			}
			if prefixErr != nil && err.Error() != prefixErr.Error() {
				t.Fatal("Launchd manager prefix refusal changed")
			}
			unixLaunchdUnchanged(t, a, m, before, artifacts)
		})
	}
}

func TestUnixServiceMaintenanceApplyContentionLaunchd(t *testing.T) {
	t.Run("contender", func(t *testing.T) {
		a, d, m := launchdFixture(t)
		defer serviceMaintenanceReleased(t, m.cell)
		held, err := unixServiceContender(t, m.cell).own(false)
		if err != nil {
			t.Fatal("Launchd contention fixture failed")
		}
		before, artifacts := serviceMaintenanceSnapshot(t, m.cell), unixServiceArtifactSnapshot(t, a.agentDir)
		err = a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"})
		if held.Close() != nil {
			t.Fatal("Launchd held contender did not release")
		}
		if err != ErrLocked {
			t.Fatal("Launchd Apply did not preserve exact lock error")
		}
		unixLaunchdUnchanged(t, a, m, before, artifacts)
		if a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"}) != nil {
			t.Fatal("Launchd Apply failed after contention released")
		}
	})
	t.Run("continuous", func(t *testing.T) {
		a, d, m := launchdFixture(t)
		defer serviceMaintenanceReleased(t, m.cell)
		other := unixServiceContender(t, m.cell)
		seen := map[string]bool{}
		a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
			if args[0] == "enable" || args[0] == "bootstrap" || (args[0] == "print" && m.loaded) {
				held, err := other.own(false)
				if held != nil {
					_ = held.Close()
				}
				if err != ErrLocked {
					t.Fatal("Launchd effect escaped the continuous operation lock")
				}
				seen[args[0]] = true
			}
			return m.command(ctx, binary, args...)
		}
		if a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"}) != nil {
			t.Fatal("Launchd continuous lock Apply failed")
		}
		if !seen["enable"] || !seen["bootstrap"] || !seen["print"] {
			t.Fatal("Launchd continuous lock missed an effect boundary")
		}
	})
}

func TestUnixServiceMaintenanceApplyAbsentParityLaunchd(t *testing.T) {
	for _, name := range []string{"seeded", "lockless"} {
		t.Run(name, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			defer serviceMaintenanceReleased(t, m.cell)
			if os.Remove(m.cell.path("journal")) != nil {
				t.Fatal("Launchd absent journal preparation failed")
			}
			if name == "lockless" {
				for _, path := range []string{m.cell.path(".owner.lock"), m.cell.path("run", "operation.lock")} {
					if os.Remove(path) != nil {
						t.Fatal("Launchd lockless preparation failed")
					}
				}
			}
			serviceMaintenanceFence(t, m.cell, "Absent")
			prefix := [][]string{{a.launchctl, "print", a.domain()}, {a.launchctl, "print", a.target(d)}}
			want := append(append([][]string{}, prefix...), []string{a.launchctl, "print-disabled", a.domain()}, []string{a.launchctl, "enable", a.target(d)}, []string{a.launchctl, "bootstrap", a.domain(), a.plistPath(d)})
			want = append(want, prefix...)
			plan := ServicePlan{Spec: d, RequiredPrivilege: "user"}
			if a.Apply(t.Context(), plan) != nil {
				t.Fatal("Launchd absent fence Apply failed")
			}
			if !reflect.DeepEqual(m.allCalls, want) {
				t.Fatal("Launchd fresh absent trace changed")
			}
			m.allCalls = nil
			if a.Apply(t.Context(), plan) != nil {
				t.Fatal("Launchd repeat absent Apply failed")
			}
			if !reflect.DeepEqual(m.allCalls, prefix) {
				t.Fatal("Launchd repeat absent trace changed")
			}
			if _, err := os.Lstat(m.cell.path("journal")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("Launchd absent fence created journal parent")
			}
			if _, err := os.Lstat(m.cell.path("journal", "maintenance.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("Launchd absent fence created evidence")
			}
		})
	}
}

func TestUnixServiceMaintenanceApplyCleanupLaunchd(t *testing.T) {
	for _, name := range []string{"persist_failure", "persist_replacement", "enable_failure", "bootstrap_failure", "pre_bootstrap_identity"} {
		t.Run(name, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			defer serviceMaintenanceReleased(t, m.cell)
			step := "plist_file:persist"
			switch name {
			case "persist_failure":
				a.writePlist = func(*os.File, []byte) error { return errors.New("fixture persist refusal") }
			case "persist_replacement":
				if a.inspectDirectories(true) != nil {
					t.Fatal("Launchd replacement directory preparation failed")
				}
				a.syncDir = func(string) error {
					data, err := os.ReadFile(a.plistPath(d))
					if err != nil || os.Rename(a.plistPath(d), a.plistPath(d)+".original") != nil || os.WriteFile(a.plistPath(d), data, 0o600) != nil {
						t.Fatal("Launchd persist replacement creation failed")
					}
					return errors.New("fixture persist refusal")
				}
			case "enable_failure":
				m.failEnable = true
				step = "enable"
			case "bootstrap_failure":
				delete(m.enabled, a.target(d))
				m.failBootstrap = true
				step = "bootstrap"
			case "pre_bootstrap_identity":
				step = "pre_bootstrap"
				a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
					out, err := m.command(ctx, binary, args...)
					if args[0] == "enable" {
						data, readErr := os.ReadFile(a.plistPath(d))
						if readErr != nil || os.Rename(a.plistPath(d), a.plistPath(d)+".original") != nil || os.WriteFile(a.plistPath(d), data, 0o600) != nil {
							t.Fatal("Launchd identity replacement failed")
						}
					}
					return out, err
				}
			}
			err := a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"})
			unixServiceRequireError(t, err, ErrServiceRegistrationFailed, step, "")
			for _, call := range m.allCalls {
				if call[1] == "bootout" {
					t.Fatal("Launchd Apply cleanup stopped a host")
				}
			}
			if name == "persist_failure" {
				if _, err := os.Lstat(a.plistPath(d)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("Launchd persist failure retained created inode")
				}
				return
			}
			before, err := os.Lstat(a.plistPath(d))
			if err != nil || !os.SameFile(before, before) {
				t.Fatal("Launchd enabling failure lost recovery evidence")
			}
			a.command, a.writePlist, a.syncDir, m.failEnable, m.failBootstrap = m.command, nil, nil, false, false
			if a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"}) != nil {
				t.Fatal("Launchd retained plist recovery failed")
			}
			after, err := os.Lstat(a.plistPath(d))
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("Launchd recovery replaced retained evidence")
			}
		})
	}
}

func TestUnixServiceArtifactConfinement(t *testing.T) {
	for _, name := range []string{"equal", "under_root", "root_under", "symlinked_parent", "case_variant", "sibling", "missing_outside"} {
		t.Run(name, func(t *testing.T) {
			if name == "symlinked_parent" && runtime.GOOS == "windows" {
				t.Skip("LC-S2-PW3: Windows symlink fixture privileges unavailable")
			}
			_, d, m := launchdFixture(t)
			for _, path := range []string{m.cell.path(".owner.lock"), m.cell.path("run", "operation.lock")} {
				if os.Remove(path) != nil {
					t.Fatal("Confinement lockless preparation failed")
				}
			}
			dir := d.Root
			overlap := true
			switch name {
			case "under_root":
				dir = filepath.Join(d.Root, "units")
			case "root_under":
				dir = filepath.Dir(d.Root)
			case "symlinked_parent":
				link := filepath.Join(filepath.Dir(d.Root), "alias")
				if os.Symlink(d.Root, link) != nil {
					t.Fatal("Confinement symlink setup failed")
				}
				dir = filepath.Join(link, "missing", "units")
			case "case_variant":
				resolved, err := filepath.EvalSymlinks(d.Root)
				if err != nil || strings.ToUpper(resolved) == resolved {
					t.Fatal("Confinement case fixture setup failed")
				}
				// A case-folding platform must treat another spelling of the
				// root as the root; a case-preserving one keeps them apart.
				dir = filepath.Join(strings.ToUpper(resolved), "units")
				overlap = runtime.GOOS == "darwin" || runtime.GOOS == "windows"
			case "sibling":
				dir = d.Root + "-units"
				overlap = false
			case "missing_outside":
				dir = filepath.Join(t.TempDir(), "missing", "units")
				overlap = false
			}
			before := serviceMaintenanceSnapshot(t, m.cell)
			c, fresh, unlock, err := serviceAdapterAdmission("1001", d, dir)
			if overlap {
				unixServiceRequireError(t, err, ErrServiceOwnershipConflict, "apply:confinement", "overlap")
				if c != nil || unlock != nil || !reflect.DeepEqual(fresh, ServiceDescriptor{}) {
					t.Fatal("Confinement refusal returned live admission state")
				}
				serviceMaintenanceUnchanged(t, m.cell, before)
			} else {
				if err != nil || c == nil || unlock == nil || !reflect.DeepEqual(fresh, d) {
					t.Fatal("Disjoint registration directory was refused")
				}
				unlock()
			}
			serviceMaintenanceReleased(t, m.cell)
		})
	}
}

func TestServiceAdapterDescriptorOwnerContract(t *testing.T) {
	t.Run("source", unixServiceDescriptorOwnerSourceContract)
	for _, name := range []string{"accepted", "other_uid", "missing_service", "non_private", "unknown_field", "revision_zero", "manifest_id", "not_installed", "root_mismatch", "public_reader"} {
		t.Run(name, func(t *testing.T) {
			_, d, m := launchdFixture(t)
			defer serviceMaintenanceReleased(t, m.cell)
			path := m.cell.path("service", "descriptor.json")
			switch name {
			case "other_uid":
				d.UID = "1002"
				unixServicePersistDescriptor(t, m.cell, d)
			case "missing_service":
				if os.RemoveAll(m.cell.path("service")) != nil {
					t.Fatal("Owner reader service removal failed")
				}
			case "non_private":
				data, err := os.ReadFile(path)
				// Ordinary creation deliberately omits the private record writer.
				// Unix exposes group read while Windows inherits an unprotected ACL.
				if err != nil || os.Remove(path) != nil || os.WriteFile(path, data, 0o644) != nil {
					t.Fatal("Owner reader unsafe record setup failed")
				}
				if servicePrivateRecord(path) == nil {
					t.Fatal("Owner reader unsafe fixture remained private")
				}
			case "unknown_field":
				value := struct {
					ServiceDescriptor
					Extra string `json:"extra"`
				}{d, "unexpected"}
				if writeAtomic(path, value, nil) != nil {
					t.Fatal("Owner reader unknown field setup failed")
				}
			case "revision_zero":
				d.Revision = 0
				unixServicePersistDescriptor(t, m.cell, d)
			case "manifest_id", "not_installed":
				manifest, err := readManifest(m.cell.cfg.Root)
				if err != nil {
					t.Fatal("Owner reader manifest read failed")
				}
				if name == "manifest_id" {
					manifest.InstallationID += "-other"
				} else {
					manifest.Installed = false
				}
				if writeAtomic(m.cell.path("manifest.json"), manifest, nil) != nil {
					t.Fatal("Owner reader manifest setup failed")
				}
			case "root_mismatch":
				d.Root += "-other"
				unixServicePersistDescriptor(t, m.cell, d)
			case "public_reader":
				uid, _ := serviceOwner()
				if uid == d.UID {
					d.UID = "1002"
					unixServicePersistDescriptor(t, m.cell, d)
				}
			}
			var fresh ServiceDescriptor
			var err error
			if name == "public_reader" {
				fresh, err = m.cell.ServiceDescriptor()
			} else {
				fresh, err = m.cell.serviceDescriptorForOwner("1001")
			}
			switch name {
			case "accepted":
				if err != nil || !reflect.DeepEqual(fresh, d) {
					t.Fatal("Owner reader refused its modeled owner")
				}
			case "missing_service":
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("Owner reader changed missing service identity")
				}
			case "other_uid", "revision_zero", "manifest_id", "not_installed", "root_mismatch":
				if !errors.Is(err, ErrServiceOwnershipConflict) {
					t.Fatal("Owner reader lost its ownership refusal")
				}
			default:
				if err == nil {
					t.Fatal("Owner reader admitted invalid persisted evidence")
				}
			}
		})
	}
}
