package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Each fixture owns its entire root; cleanup never calls a fenced lifecycle
// owner or depends on stopping a child. unitCell also isolates both homes.
func serviceMaintenanceCell(t *testing.T, prepared bool) (*Cell, ServiceDescriptor) {
	t.Helper()
	c, _ := unitCell(t)
	t.Setenv("WITSELF_RUNTIME", "local")
	host, err := os.Executable()
	if err != nil {
		t.Fatal("maintenance fixture executable unavailable")
	}
	d := ServiceDescriptor{Adapter: "foreground", Payload: ServicePayload{Host: host, Args: []string{"run", "--root", c.cfg.Root}}}
	// Lock artifacts are permitted before admission. Seed them before snapshots
	// so later comparisons cover every filesystem object without exclusions.
	lock, err := c.own(false)
	if err != nil {
		t.Fatal("maintenance fixture lock unavailable")
	}
	if err := lock.Close(); err != nil {
		t.Fatal("maintenance fixture lock did not release")
	}
	if prepared {
		if err := c.PrepareService(t.Context(), d); err != nil {
			t.Fatal("maintenance fixture preparation failed")
		}
		d, err = c.ServiceDescriptor()
		if err != nil {
			t.Fatal("maintenance fixture descriptor unavailable")
		}
	}
	return c, d
}

type serviceMaintenanceEntry struct {
	mode os.FileMode
	data string
}

// Opaque fixture secret bytes stay in memory and are never formatted in a
// failure. Lstat/Readlink avoid following planted unsafe evidence.
func serviceMaintenanceSnapshot(t *testing.T, c *Cell) map[string]serviceMaintenanceEntry {
	t.Helper()
	entries := map[string]serviceMaintenanceEntry{}
	err := filepath.WalkDir(c.cfg.Root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(c.cfg.Root, path)
		if err != nil {
			return err
		}
		item := serviceMaintenanceEntry{mode: info.Mode()}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			item.data, err = os.Readlink(path)
		case info.Mode().IsRegular():
			// A zero-length file has no bytes to compare and Lstat already
			// reported its length, so it is never opened. The operation lock
			// files are always empty, and while a contender holds one Windows
			// refuses every read of its mandatorily locked byte range.
			if info.Size() != 0 {
				var data []byte
				data, err = os.ReadFile(path)
				item.data = string(data)
			}
		case entry.IsDir():
		default:
			return errors.New("maintenance snapshot encountered a non-fixture object")
		}
		entries[filepath.ToSlash(name)] = item
		return err
	})
	if err != nil {
		t.Fatal("cannot snapshot maintenance fixture")
	}
	return entries
}

func serviceMaintenanceUnchanged(t *testing.T, c *Cell, before map[string]serviceMaintenanceEntry) {
	t.Helper()
	if !reflect.DeepEqual(before, serviceMaintenanceSnapshot(t, c)) {
		t.Fatal("maintenance refusal changed fixture bytes modes or existence")
	}
}

func serviceMaintenanceFence(t *testing.T, c *Cell, kind string) {
	t.Helper()
	path := c.path("journal", "maintenance.json")
	switch kind {
	case "Absent":
		return
	case "Valid":
		updateTestFence(t, c) // Generation 1 intentionally matches positive-generation mutants.
	case "Corrupt":
		if testWriteFile(path, []byte(`{"generation":`), 0o600) != nil {
			t.Fatal("cannot plant malformed maintenance evidence")
		}
	case "Foreign":
		if testWriteFile(path, []byte(`{"generation":1,"operation_id":"fixture","since":"2026-11-10T00:00:00Z","foreign":"retained"}`), 0o600) != nil {
			t.Fatal("cannot plant foreign maintenance evidence")
		}
	case "Directory":
		if testMkdir(path, 0o700) != nil {
			t.Fatal("cannot plant directory maintenance evidence")
		}
	case "UnsafeMode":
		updateTestFence(t, c)
		if os.Chmod(path, 0o644) != nil {
			t.Fatal("cannot plant unsafe maintenance mode")
		}
	case "Symlink":
		target := filepath.Join(t.TempDir(), "evidence")
		if testWriteFile(target, []byte("retained external evidence"), 0o600) != nil || os.Symlink(target, path) != nil {
			t.Fatal("cannot plant symlink maintenance evidence")
		}
	default:
		t.Fatal("unknown maintenance evidence fixture")
	}
}

func serviceMaintenanceCause(t *testing.T, err error) {
	t.Helper()
	if err != errMaintenanceFenceActive || err.Error() != "local_maintenance_fence_active" {
		t.Fatal("eligible service owner did not return exact maintenance cause")
	}
}

func serviceMaintenanceNativeBoot(t *testing.T) string {
	t.Helper()
	boot, err := serviceBootID()
	if err != nil || !strings.HasPrefix(boot, runtime.GOOS+":") || len(boot) <= len(runtime.GOOS)+1 {
		t.Fatal("native service boot identity unavailable or malformed")
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		id := strings.TrimPrefix(boot, runtime.GOOS+":")
		if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
			t.Fatal("native boot identifier is not UUID-shaped")
		}
		for _, ch := range strings.ReplaceAll(id, "-", "") {
			if !strings.ContainsRune("0123456789abcdefABCDEF", ch) {
				t.Fatal("native boot identifier contains a non-hex digit")
			}
		}
	}
	if runtime.GOOS == "darwin" {
		parts := strings.Split(boot, ":")
		if len(parts) != 3 {
			t.Fatal("native Darwin boot identifier is not timeval-shaped")
		}
		seconds, secondsErr := strconv.ParseInt(parts[1], 10, 64)
		micros, microsErr := strconv.ParseInt(parts[2], 10, 64)
		if secondsErr != nil || microsErr != nil || seconds <= 0 || micros < 0 || micros >= 1000000 {
			t.Fatal("native Darwin boot identifier contains an invalid timeval")
		}
	}
	return boot
}

func serviceMaintenanceBootFixture(t *testing.T, kind string) (*Cell, ServiceDescriptor, string) {
	t.Helper()
	c, d := serviceMaintenanceCell(t, true)
	boot := serviceMaintenanceNativeBoot(t)
	if kind == "first" {
		return c, d, boot
	}
	old := runtime.GOOS + ":00000000-0000-0000-0000-000000000001"
	if old == boot {
		old = runtime.GOOS + ":00000000-0000-0000-0000-000000000002"
	}
	previous := old
	if kind == "same" {
		previous = boot
	}
	// Deliberately noncanonical spacing proves unconditional same-boot rewrite.
	data := []byte(fmt.Sprintf(" { \"installation_id\" : %q, \"boot_id\" : %q }\n", d.InstallationID, previous))
	if writeAtomicBytes(c.path("service", "boot.json"), data, nil) != nil {
		t.Fatal("cannot seed prior service boot")
	}
	if kind == "same" {
		return c, d, boot
	}
	if privateDir(c.path("pg"), true) != nil {
		t.Fatal("cannot prepare old boot PG directory")
	}
	for _, component := range []string{"postgres", "server", "worker"} {
		start := old + ":7"
		if runtime.GOOS == "windows" {
			start = "windows:0:1"
		}
		record := ProcessRecord{PID: os.Getpid(), StartTime: start, BootID: old, Root: c.cfg.Root, Component: component}
		if kind == "mixed" && component == "server" {
			record.Root += "-foreign"
		}
		if writeAtomic(c.path("run", component+".json"), record, nil) != nil {
			t.Fatal("cannot seed old boot component identity")
		}
	}
	pid := []byte(fmt.Sprintf("%d\n%s\n1\n", os.Getpid(), c.path("pg")))
	if writeAtomicBytes(c.path("pg", "postmaster.pid"), pid, nil) != nil {
		t.Fatal("cannot seed old boot postmaster identity")
	}
	return c, d, boot
}

func serviceMaintenanceReleased(t *testing.T, c *Cell) {
	t.Helper()
	other, err := New(c.cfg)
	if err != nil {
		t.Fatal("cannot construct post-return lock contender")
	}
	lock, err := other.own(false)
	if err != nil {
		t.Fatal("service owner retained operation lock after return")
	}
	if lock.Close() != nil {
		t.Fatal("post-return operation contender did not release")
	}
}

func TestServiceMaintenancePreparationAdmission(t *testing.T) {
	kinds := []string{"Absent", "Valid", "Corrupt", "Foreign", "Directory"}
	// POSIX mode and symlink fixtures are not Windows ACL/reparse fixtures.
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	for _, kind := range kinds {
		t.Run("new_publication/"+kind, func(t *testing.T) {
			c, d := serviceMaintenanceCell(t, false)
			serviceMaintenanceFence(t, c, kind)
			before := serviceMaintenanceSnapshot(t, c)
			var target string
			var targetBytes []byte
			if kind == "Symlink" {
				var err error
				target, err = os.Readlink(c.path("journal", "maintenance.json"))
				if err != nil {
					t.Fatal("cannot inspect planted evidence link")
				}
				targetBytes, err = os.ReadFile(target)
				if err != nil {
					t.Fatal("cannot read owned evidence target")
				}
			}
			err := c.PrepareService(t.Context(), d)
			if kind == "Absent" {
				if err != nil {
					t.Fatal("unfenced preparation did not reach publication")
				}
				got, err := c.ServiceDescriptor()
				if err != nil || got.Revision != 1 || got.RepairRevision != 0 || got.Faulted || got.Payload.Generation == "" {
					t.Fatal("unfenced preparation did not publish baseline descriptor")
				}
				if _, err := c.serviceControlSecret(); err != nil {
					t.Fatal("unfenced preparation did not publish private secret")
				}
				if _, err := os.Lstat(c.path("service", "transition.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unfenced preparation retained transition")
				}
				for _, path := range []string{"journal", "journal/operations.jsonl", "manifest.json"} {
					if serviceMaintenanceSnapshot(t, c)[path] != before[path] {
						t.Fatal("unfenced preparation changed operation journal or manifest")
					}
				}
				serviceMaintenanceReleased(t, c)
				return
			}
			serviceMaintenanceCause(t, err)
			serviceMaintenanceUnchanged(t, c, before)
			if _, err := os.Lstat(c.path("service")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("fenced preparation created service publication directory")
			}
			if target != "" {
				after, err := os.ReadFile(target)
				if err != nil || string(after) != string(targetBytes) {
					t.Fatal("fenced preparation changed symlink target evidence")
				}
			}
			serviceMaintenanceReleased(t, c)
		})
	}
	for _, kind := range []string{"Valid", "Corrupt"} {
		for _, branch := range []string{"Matching", "Conflict", "InvalidSecret"} {
			t.Run("existing/"+kind+"/"+branch, func(t *testing.T) {
				c, d := serviceMaintenanceCell(t, true)
				serviceMaintenanceFence(t, c, kind)
				if branch == "Conflict" {
					d.Payload.Generation += "-different"
				}
				if branch == "InvalidSecret" && testWriteFile(c.path("service", "control.secret"), []byte("invalid"), 0o600) != nil {
					t.Fatal("cannot seed invalid existing fixture secret")
				}
				before := serviceMaintenanceSnapshot(t, c)
				err := c.PrepareService(t.Context(), d)
				switch branch {
				case "Matching":
					if err != nil {
						t.Fatal("fence refused read-only matching preparation")
					}
				case "Conflict":
					if err != ErrServiceOwnershipConflict {
						t.Fatal("existing immutable conflict lost exact earlier cause")
					}
				case "InvalidSecret":
					if err == nil || err.Error() != "service_control_secret_invalid" {
						t.Fatal("existing secret refusal lost exact earlier cause")
					}
				}
				serviceMaintenanceUnchanged(t, c, before)
				serviceMaintenanceReleased(t, c)
			})
		}
	}
}

func TestServiceMaintenanceBootAdmission(t *testing.T) {
	for _, bootKind := range []string{"first", "same", "old"} {
		for _, fence := range []string{"Absent", "Valid", "Corrupt", "Foreign"} {
			t.Run(bootKind+"/"+fence, func(t *testing.T) {
				c, d, boot := serviceMaintenanceBootFixture(t, bootKind)
				serviceMaintenanceFence(t, c, fence)
				before := serviceMaintenanceSnapshot(t, c)
				identity, alive, err := processIdentity(os.Getpid())
				if err != nil || !alive {
					t.Fatal("boot admission bystander unavailable")
				}
				err = c.reconcileServiceBoot()
				if fence != "Absent" {
					serviceMaintenanceCause(t, err)
					serviceMaintenanceUnchanged(t, c, before)
				} else {
					if err != nil {
						t.Fatal("unfenced boot owner did not reach publication or cleanup")
					}
					var got serviceBootRecord
					if readJSON(c.path("service", "boot.json"), &got) != nil || got != (serviceBootRecord{d.InstallationID, boot}) {
						t.Fatal("unfenced boot owner did not publish current boot")
					}
					after := serviceMaintenanceSnapshot(t, c)
					if after["service/boot.json"] == before["service/boot.json"] {
						t.Fatal("unfenced boot owner did not rewrite boot bytes")
					}
					if bootKind == "old" {
						for _, name := range []string{"run/postgres.json", "run/server.json", "run/worker.json", "pg/postmaster.pid"} {
							if _, exists := after[name]; exists {
								t.Fatal("unfenced boot owner did not remove old boot target")
							}
						}
					}
					for _, name := range []string{"manifest.json", "service/descriptor.json", "service/control.secret", "service/transition.json", "journal/operations.jsonl", "journal/maintenance.json"} {
						if after[name] != before[name] {
							t.Fatal("unfenced boot owner changed unrelated durable evidence")
						}
					}
				}
				afterIdentity, alive, err := processIdentity(os.Getpid())
				if err != nil || !alive || afterIdentity != identity {
					t.Fatal("boot reconciliation signaled the live bystander")
				}
				serviceMaintenanceReleased(t, c)
			})
		}
	}
}

func TestServiceMaintenanceLegacyOrdering(t *testing.T) {
	prepareCases := []string{"runtime", "context", "own", "own_directory", "service_directory", "existing_missing_secret", "manifest_missing", "manifest_malformed", "manifest_uninstalled", "descriptor_invalid", "existing_malformed", "existing_owner", "existing_secret", "later_secret"}
	for _, name := range prepareCases {
		for _, fence := range []string{"Absent", "Valid"} {
			t.Run("Prepare/"+name+"/"+fence, func(t *testing.T) {
				c, d := serviceMaintenanceCell(t, strings.HasPrefix(name, "existing_"))
				ctx := t.Context()
				want := ""
				var cause error
				var held *operationLock
				switch name {
				case "runtime":
					t.Setenv("WITSELF_RUNTIME", "hosted")
					want = "service_local_runtime_required"
				case "context":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
					cause = context.Canceled
				case "own":
					other, err := New(c.cfg)
					if err != nil {
						t.Fatal("cannot construct independent lock owner")
					}
					lock, err := other.own(false)
					if err != nil {
						t.Fatal("cannot hold independent operation lock")
					}
					t.Cleanup(func() { _ = lock.Close() })
					held = lock
					cause = ErrLocked
				case "own_directory":
					if os.Remove(c.path("run", "operation.lock")) != nil || os.Remove(c.path("run")) != nil || testWriteFile(c.path("run"), []byte("retained layout evidence"), 0o600) != nil {
						t.Fatal("cannot seed invalid operation directory")
					}
					want = "local cell directory is missing or not a real directory"
				case "service_directory":
					if testWriteFile(c.path("service"), []byte("retained service evidence"), 0o600) != nil {
						t.Fatal("cannot seed invalid service directory")
					}
					want = "local cell directory is missing or not a real directory"
				case "existing_missing_secret":
					if os.Remove(c.path("service", "control.secret")) != nil {
						t.Fatal("cannot remove existing fixture secret")
					}
					cause = os.ErrNotExist
				case "manifest_missing":
					if os.Remove(c.path("manifest.json")) != nil {
						t.Fatal("cannot remove fixture manifest")
					}
					want = "service_installation_required"
				case "manifest_malformed":
					if testWriteFile(c.path("manifest.json"), []byte("{"), 0o600) != nil {
						t.Fatal("cannot corrupt fixture manifest")
					}
					want = "service_installation_required"
				case "manifest_uninstalled":
					m, err := readManifest(c.cfg.Root)
					if err != nil {
						t.Fatal("cannot read fixture manifest")
					}
					m.Installed = false
					if writeAtomic(c.path("manifest.json"), m, nil) != nil {
						t.Fatal("cannot mark fixture uninstalled")
					}
					want = "service_installation_required"
				case "descriptor_invalid":
					d.Adapter = "unrecognized"
					cause = ErrServiceOwnershipConflict
				case "existing_malformed":
					if testWriteFile(c.path("service", "descriptor.json"), []byte("{"), 0o600) != nil {
						t.Fatal("cannot corrupt fixture descriptor")
					}
					want = "invalid local cell record"
				case "existing_owner":
					foreign := d
					foreign.InstallationID = "foreign"
					if writeAtomic(c.path("service", "descriptor.json"), foreign, nil) != nil {
						t.Fatal("cannot seed foreign descriptor")
					}
					cause = ErrServiceOwnershipConflict
				case "existing_secret", "later_secret":
					if privateDir(c.path("service"), true) != nil || testWriteFile(c.path("service", "control.secret"), []byte("invalid"), 0o600) != nil {
						t.Fatal("cannot seed invalid fixture secret")
					}
					want = "service_control_secret_invalid"
					if name == "later_secret" && fence == "Valid" {
						cause = errMaintenanceFenceActive
					}
				}
				serviceMaintenanceFence(t, c, fence)
				before := serviceMaintenanceSnapshot(t, c)
				err := c.PrepareService(ctx, d)
				serviceMaintenanceExactError(t, err, cause, want)
				serviceMaintenanceUnchanged(t, c, before)
				if held != nil && held.Close() != nil {
					t.Fatal("cannot release earlier operation contender")
				}
				if name == "own_directory" {
					if os.Remove(c.path("run")) != nil || privateDir(c.path("run"), true) != nil {
						t.Fatal("cannot restore fixture directory for release probe")
					}
				}
				serviceMaintenanceReleased(t, c)
			})
		}
	}
	bootCases := []string{"own", "own_directory", "service_directory", "descriptor_missing", "descriptor_malformed", "descriptor_owner", "boot_malformed", "boot_owner", "boot_empty", "old_record", "postmaster"}
	for _, name := range bootCases {
		for _, fence := range []string{"Absent", "Valid"} {
			t.Run("Boot/"+name+"/"+fence, func(t *testing.T) {
				kind := "same"
				if name == "old_record" || name == "postmaster" {
					kind = "mixed"
				}
				c, d, boot := serviceMaintenanceBootFixture(t, kind)
				var cause error
				var held *operationLock
				want := ""
				switch name {
				case "own":
					other, err := New(c.cfg)
					if err != nil {
						t.Fatal("cannot construct independent lock owner")
					}
					lock, err := other.own(false)
					if err != nil {
						t.Fatal("cannot hold independent operation lock")
					}
					t.Cleanup(func() { _ = lock.Close() })
					held = lock
					cause = ErrLocked
				case "own_directory":
					if os.Remove(c.path("run", "operation.lock")) != nil || os.Remove(c.path("run")) != nil || testWriteFile(c.path("run"), []byte("retained layout evidence"), 0o600) != nil {
						t.Fatal("cannot seed invalid operation directory")
					}
					want = "local cell directory is missing or not a real directory"
				case "service_directory":
					if os.RemoveAll(c.path("service")) != nil || testWriteFile(c.path("service"), []byte("retained service evidence"), 0o600) != nil {
						t.Fatal("cannot seed invalid service directory")
					}
					want = "local cell directory is missing or not a real directory"
				case "descriptor_missing":
					if os.Remove(c.path("service", "descriptor.json")) != nil {
						t.Fatal("cannot remove fixture descriptor")
					}
					cause = os.ErrNotExist
				case "descriptor_malformed":
					if testWriteFile(c.path("service", "descriptor.json"), []byte("{"), 0o600) != nil {
						t.Fatal("cannot corrupt fixture descriptor")
					}
					want = "invalid local cell record"
				case "descriptor_owner":
					d.InstallationID = "foreign"
					if writeAtomic(c.path("service", "descriptor.json"), d, nil) != nil {
						t.Fatal("cannot seed foreign descriptor")
					}
					cause = ErrServiceOwnershipConflict
				case "boot_malformed":
					if testWriteFile(c.path("service", "boot.json"), []byte("{"), 0o600) != nil {
						t.Fatal("cannot corrupt fixture boot")
					}
					want = "invalid local cell record"
				case "boot_owner", "boot_empty":
					record := serviceBootRecord{d.InstallationID, boot}
					if name == "boot_owner" {
						record.InstallationID = "foreign"
					} else {
						record.BootID = ""
					}
					if writeAtomic(c.path("service", "boot.json"), record, nil) != nil {
						t.Fatal("cannot seed conflicting boot record")
					}
					cause = ErrServiceOwnershipConflict
				case "old_record":
					cause = ErrServiceOwnershipConflict
					if fence == "Valid" {
						cause = errMaintenanceFenceActive
					}
				case "postmaster":
					// Remove the mixed component so the absent control reaches the
					// postmaster parser, past the complete record validation loop.
					if os.Remove(c.path("run", "server.json")) != nil || testWriteFile(c.path("pg", "postmaster.pid"), []byte("invalid\nforeign\n1\n"), 0o600) != nil {
						t.Fatal("cannot seed postmaster refusal")
					}
					want = "PostgreSQL data directory identity mismatch"
					if fence == "Valid" {
						cause = errMaintenanceFenceActive
					}
				}
				serviceMaintenanceFence(t, c, fence)
				before := serviceMaintenanceSnapshot(t, c)
				serviceMaintenanceExactError(t, c.reconcileServiceBoot(), cause, want)
				serviceMaintenanceUnchanged(t, c, before)
				if held != nil && held.Close() != nil {
					t.Fatal("cannot release earlier operation contender")
				}
				if name == "own_directory" {
					if os.Remove(c.path("run")) != nil || privateDir(c.path("run"), true) != nil {
						t.Fatal("cannot restore fixture directory for release probe")
					}
				}
				serviceMaintenanceReleased(t, c)
			})
		}
	}
}

func serviceMaintenanceExactError(t *testing.T, got, cause error, text string) {
	t.Helper()
	if cause != nil {
		if got != cause || got.Error() != cause.Error() {
			t.Fatal("service owner changed exact legacy refusal identity or text")
		}
		return
	}
	if got == nil || got.Error() != text {
		t.Fatal("service owner changed exact legacy refusal text")
	}
}

func TestServiceMaintenanceLockLifetime(t *testing.T) {
	for _, owner := range []string{"Prepare", "Boot"} {
		for _, fence := range []string{"Absent", "Valid"} {
			t.Run(owner+"/"+fence, func(t *testing.T) {
				c, d := serviceMaintenanceCell(t, owner == "Boot")
				serviceMaintenanceFence(t, c, fence)
				call := func() error { return c.PrepareService(t.Context(), d) }
				if owner == "Boot" {
					call = c.reconcileServiceBoot
				}
				other, err := New(c.cfg)
				if err != nil {
					t.Fatal("cannot construct independent contender")
				}
				lock, err := other.own(false)
				if err != nil {
					t.Fatal("independent contender cannot hold operation lock")
				}
				t.Cleanup(func() { _ = lock.Close() })
				before := serviceMaintenanceSnapshot(t, c)
				if err := call(); err != ErrLocked {
					t.Fatal("service owner did not preserve prior operation contention")
				}
				serviceMaintenanceUnchanged(t, c, before)
				if lock.Close() != nil {
					t.Fatal("independent contender did not release")
				}
				err = call()
				if fence == "Valid" {
					serviceMaintenanceCause(t, err)
					serviceMaintenanceUnchanged(t, c, before)
				} else if err != nil {
					t.Fatal("service owner did not proceed after contender released")
				}
				after, err := other.own(false)
				if err != nil {
					t.Fatal("service owner retained operation lock after return")
				}
				if after.Close() != nil {
					t.Fatal("post-return contender did not release")
				}
			})
		}
	}
	t.Run("host_lease_independent", func(t *testing.T) {
		c, d := serviceMaintenanceCell(t, true)
		lease, err := c.acquireServiceLeaseWithBoot(d, func() (string, error) { return serviceMaintenanceNativeBoot(t), nil })
		if err != nil {
			t.Fatal("cannot hold diagnostic host lifetime lease")
		}
		defer func() { _ = lease.Close() }()
		if err := c.reconcileServiceBoot(); err != nil {
			t.Fatal("diagnostic host lease blocked operation owner")
		}
		serviceMaintenanceFence(t, c, "Valid")
		serviceMaintenanceCause(t, c.reconcileServiceBoot())
		second, err := c.acquireServiceLeaseWithBoot(d, func() (string, error) { return serviceMaintenanceNativeBoot(t), nil })
		if second != nil || err != ErrServiceHostActive {
			t.Fatal("operation owner released independent host lifetime lease")
		}
	})
}

func TestServiceMaintenanceProtectivePathsRemainUsable(t *testing.T) {
	for _, fence := range []string{"Absent", "Valid", "Corrupt"} {
		t.Run(fence, func(t *testing.T) {
			c, d := serviceMaintenanceCell(t, true)
			serviceMaintenanceFence(t, c, fence)
			before := serviceMaintenanceSnapshot(t, c)
			if c.PrepareService(t.Context(), d) != nil {
				t.Fatal("protective read-only preparation was fenced")
			}
			inspection, err := c.Inspect(t.Context())
			if err != nil || inspection.Manifest == nil || !inspection.Manifest.Installed || inspection.Locked || inspection.Interrupted != nil {
				t.Fatal("maintenance evidence changed legacy Inspect result")
			}
			status, err := c.Status(t.Context())
			if err != nil || !reflect.DeepEqual(status.Inspection, inspection) || status.ServerReady || status.WorkerReady {
				t.Fatal("maintenance evidence changed legacy Status result")
			}
			serviceMaintenanceUnchanged(t, c, before)
			for _, desired := range []string{"stopped", "maintenance"} {
				if c.SetServiceDesiredState(t.Context(), desired) != nil {
					t.Fatal("protective desired-state write was fenced")
				}
				d.DesiredState, d.Revision = desired, d.Revision+1
				serviceMaintenanceDescriptorBytes(t, c, d)
			}
			trace := []string{}
			c.cfg.Failpoint = func(operation, step string) error { trace = append(trace, operation+":"+step); return nil }
			if c.Stop(t.Context()) != nil {
				t.Fatal("protective Stop was fenced")
			}
			d.DesiredState, d.Revision = "stopped", d.Revision+1
			serviceMaintenanceDescriptorBytes(t, c, d)
			if !reflect.DeepEqual(trace, []string{"stop:stop"}) {
				t.Fatal("protective Stop changed its guarded step order")
			}
			m, err := readManifest(c.cfg.Root)
			if err != nil || m.LastOperation != "stop" {
				t.Fatal("protective Stop lost its completed manifest")
			}
			serviceMaintenanceStopJournal(t, c)
			after := serviceMaintenanceSnapshot(t, c)
			for _, name := range []string{"journal/maintenance.json", "service/control.secret", "service/transition.json"} {
				if after[name] != before[name] {
					t.Fatal("protective path changed retained evidence or secret")
				}
			}
		})
		t.Run(fence+"/status_steps", func(t *testing.T) {
			c, d := serviceMaintenanceCell(t, true)
			serviceMaintenanceFence(t, c, fence)
			h, _, starts, stops, _ := supervisorTestHooks()
			trace := []string{}
			h.start = func(context.Context) error { *starts++; trace = append(trace, "start"); return nil }
			h.health = func(context.Context) error { trace = append(trace, "health"); return nil }
			s := newSupervisor(c, SupervisorPolicy{hooks: &h})
			if s.load(d) != nil {
				t.Fatal("cannot load healthy synthetic supervisor")
			}
			before := serviceMaintenanceSnapshot(t, c)
			status, err := s.command(t.Context(), "status")
			if err != nil || !status.Healthy || *starts != 1 || *stops != 0 || s.record.Attempts != 1 || len(s.record.Failures) != 0 || !s.record.NextStart.IsZero() || !reflect.DeepEqual(trace, []string{"start", "health"}) {
				t.Fatal("maintenance evidence changed status starting or healthy bookkeeping")
			}
			if serviceMaintenanceSnapshot(t, c)["journal/maintenance.json"] != before["journal/maintenance.json"] {
				t.Fatal("status changed maintenance evidence")
			}
		})
		t.Run(fence+"/pending_stop", func(t *testing.T) {
			c, d := serviceMaintenanceCell(t, true)
			serviceMaintenanceFence(t, c, fence)
			if c.journal("start", "operation", "started") != nil {
				t.Fatal("cannot seed pending operation")
			}
			before := serviceMaintenanceSnapshot(t, c)
			err := c.Stop(t.Context())
			if err == nil || err.Error() != "local cell operation interrupted; run repair first" {
				t.Fatal("pending journal lost legacy Stop refusal")
			}
			d.DesiredState, d.Revision = "stopped", d.Revision+1
			serviceMaintenanceDescriptorBytes(t, c, d)
			after := serviceMaintenanceSnapshot(t, c)
			delete(before, "service/descriptor.json")
			delete(after, "service/descriptor.json")
			if !reflect.DeepEqual(before, after) {
				t.Fatal("pending Stop changed evidence beyond protective stopped intent")
			}
		})
	}
}

func serviceMaintenanceDescriptorBytes(t *testing.T, c *Cell, expected ServiceDescriptor) {
	t.Helper()
	data, err := json.MarshalIndent(expected, "", "  ")
	if err != nil {
		t.Fatal("cannot encode expected fixture descriptor")
	}
	actual, err := os.ReadFile(c.path("service", "descriptor.json"))
	if err != nil || string(actual) != string(append(data, '\n')) {
		t.Fatal("protective desired-state descriptor bytes differ from baseline")
	}
	if _, err := os.Lstat(c.path("service", "transition.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("protective descriptor transition was not completed")
	}
}

func serviceMaintenanceStopJournal(t *testing.T, c *Cell) {
	t.Helper()
	data, err := os.ReadFile(c.path("journal", "operations.jsonl"))
	if err != nil {
		t.Fatal("protective Stop journal unavailable")
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatal("protective Stop changed journal step count")
	}
	for index, step := range []string{"operation:started", "stop:started", "stop:done", "operation:done"} {
		var entry journalEntry
		if json.Unmarshal([]byte(lines[index]), &entry) != nil || entry.Operation != "stop" || entry.Step+":"+entry.State != step || entry.At.IsZero() || entry.Cause != "" {
			t.Fatal("protective Stop changed journal effect order")
		}
	}
}

// The parity expectations below are also executed unchanged with saved BASE
// production bytes. Only generated secrets and the explicit fixture identity
// fields are variable; descriptor revisions, bytes, hook order and file presence
// are compared without normalization.
func serviceMaintenanceParityJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal("cannot encode parity expectation")
	}
	return append(data, '\n')
}

func serviceMaintenanceParityFile(t *testing.T, c *Cell, name string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(c.path(filepath.FromSlash(name)))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("absent parity bytes changed: %s", name)
	}
	if servicePrivateRecord(c.path(filepath.FromSlash(name))) != nil {
		t.Fatalf("absent parity privacy changed: %s", name)
	}
}

func serviceMaintenanceParityUnchanged(t *testing.T, c *Cell, before map[string]serviceMaintenanceEntry, exceptions ...string) {
	t.Helper()
	after := serviceMaintenanceSnapshot(t, c)
	for _, name := range exceptions {
		delete(before, name)
		delete(after, name)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("absent parity changed unrelated bytes, modes or existence")
	}
}

func TestServiceMaintenanceAbsentParity(t *testing.T) {
	for _, parent := range []string{"journal_leaf", "journal_parent"} {
		for _, kind := range []string{"new", "existing", "conflict", "invalid_secret"} {
			t.Run("prepare_"+kind+"/"+parent, func(t *testing.T) {
				c, d := serviceMaintenanceCell(t, kind == "existing" || kind == "conflict")
				if parent == "journal_parent" && os.Remove(c.path("journal")) != nil {
					t.Fatal("cannot remove absent parity parent")
				}
				if kind == "conflict" {
					d.Payload.Generation += "-different"
				}
				if kind == "invalid_secret" {
					if privateDir(c.path("service"), true) != nil || testWriteFile(c.path("service", "control.secret"), []byte("invalid"), 0o600) != nil {
						t.Fatal("cannot seed parity secret refusal")
					}
				}
				before := serviceMaintenanceSnapshot(t, c)
				err := c.PrepareService(t.Context(), d)
				switch kind {
				case "conflict":
					if err != ErrServiceOwnershipConflict {
						t.Fatal("absent preparation conflict changed")
					}
				case "invalid_secret":
					if err == nil || err.Error() != "service_control_secret_invalid" {
						t.Fatal("absent preparation secret refusal changed")
					}
				default:
					if err != nil {
						t.Fatal("absent preparation did not retain baseline success")
					}
				}
				if kind != "new" {
					serviceMaintenanceParityUnchanged(t, c, before)
					return
				}
				m, readErr := readManifest(c.cfg.Root)
				uid, ownerErr := serviceOwner()
				if readErr != nil || ownerErr != nil {
					t.Fatal("parity fixture identity unavailable")
				}
				want := ServiceDescriptor{Version: 1, InstallationID: m.InstallationID, UID: uid, Root: c.cfg.Root,
					Runtime: "local", Scope: "user", Adapter: d.Adapter, DesiredState: "running", ResourceProfile: "unconfigured",
					Revision: 1, Payload: d.Payload}
				if want.Payload.Generation == "" {
					want.Payload.Generation = m.InstallationID + "-1"
				}
				serviceMaintenanceParityFile(t, c, "service/descriptor.json", serviceMaintenanceParityJSON(t, want))
				secret, secretErr := c.serviceControlSecret()
				if secretErr != nil || len(secret) != 43 {
					t.Fatal("absent preparation generated secret shape changed")
				}
				// The only opaque generated field is the validated control secret.
				serviceMaintenanceParityUnchanged(t, c, before, "service", "service/descriptor.json", "service/control.secret")
			})
		}
		for _, kind := range []string{"first", "same", "old", "mixed"} {
			t.Run("boot_"+kind+"/"+parent, func(t *testing.T) {
				c, d, boot := serviceMaintenanceBootFixture(t, kind)
				if parent == "journal_parent" && os.Remove(c.path("journal")) != nil {
					t.Fatal("cannot remove absent parity parent")
				}
				before := serviceMaintenanceSnapshot(t, c)
				err := c.reconcileServiceBoot()
				if kind == "mixed" {
					if err != ErrServiceOwnershipConflict {
						t.Fatal("absent mixed boot refusal changed")
					}
					serviceMaintenanceParityUnchanged(t, c, before)
					return
				}
				if err != nil {
					t.Fatal("absent boot did not retain baseline success")
				}
				serviceMaintenanceParityFile(t, c, "service/boot.json", serviceMaintenanceParityJSON(t, serviceBootRecord{d.InstallationID, boot}))
				exceptions := []string{"service/boot.json"}
				if kind == "old" {
					for _, name := range []string{"pg/postmaster.pid", "run/postgres.json", "run/server.json", "run/worker.json"} {
						if _, statErr := os.Lstat(c.path(filepath.FromSlash(name))); !errors.Is(statErr, os.ErrNotExist) {
							t.Fatal("absent old boot retained stale evidence")
						}
						exceptions = append(exceptions, name)
					}
				}
				serviceMaintenanceParityUnchanged(t, c, before, exceptions...)
			})
		}
	}
	for _, kind := range []string{"success", "start_failure", "stopped"} {
		t.Run("run_"+kind, func(t *testing.T) { serviceMaintenanceRunParity(t, kind) })
	}
}

func serviceMaintenanceShortCell(t *testing.T) (*Cell, ServiceDescriptor) {
	t.Helper()
	c, m := unitCell(t)
	if runtime.GOOS != "windows" {
		root, err := os.MkdirTemp("/tmp", "lc-svc-admit-")
		if err != nil {
			t.Fatal("cannot create short owned service fixture")
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		c, err = New(Config{Root: root, Timeout: time.Second})
		if err != nil || c.layout() != nil || writeAtomic(c.path("manifest.json"), m, nil) != nil {
			t.Fatal("cannot initialize short owned service fixture")
		}
	}
	d := prepareSupervisorTest(t, c)
	return c, d
}

type serviceMaintenanceIdleContext struct {
	context.Context
	cancel context.CancelFunc
	ready  atomic.Bool
}

func (c *serviceMaintenanceIdleContext) Done() <-chan struct{} {
	if c.ready.Load() {
		c.cancel()
	}
	return c.Context.Done()
}

func serviceMaintenanceCaptureReceipt(t *testing.T, run func() error) ([]byte, error) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "service-receipt")
	if err != nil {
		t.Fatal("cannot capture service receipt")
	}
	original := os.Stderr
	defer func() { os.Stderr = original; _ = file.Close() }()
	os.Stderr = file
	result := run()
	os.Stderr = original
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal("cannot read captured service receipt")
	}
	return data, result
}

func serviceMaintenancePublishedHost(t *testing.T, c *Cell, d ServiceDescriptor) []byte {
	t.Helper()
	boot, bootErr := serviceBootID()
	identity, alive, identityErr := processIdentity(os.Getpid())
	if bootErr != nil || identityErr != nil || !alive {
		t.Fatal("native fixture host identity unavailable")
	}
	want := serviceLeaseRecord{Version: 1, BootID: boot, StartIdentity: identity, PID: os.Getpid(), Owner: d.UID, InstallationID: d.InstallationID}
	data := serviceMaintenanceParityJSON(t, want)
	serviceMaintenanceParityFile(t, c, "service/lease.json", data)
	if c.validateServiceControlEndpoint() != nil {
		t.Fatal("Run did not publish control before reconcile")
	}
	if lease, err := c.acquireServiceLease(d); err != ErrServiceHostActive {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatal("Run did not retain independent host lease")
	}
	lock, err := c.own(false)
	if err != nil {
		t.Fatal("host lease unexpectedly owns operation lock")
	}
	if lock.Close() != nil {
		t.Fatal("cannot release independent operation probe")
	}
	return data
}

func serviceMaintenanceHostReleased(t *testing.T, c *Cell) {
	t.Helper()
	for _, name := range []string{"service/control.sock", "service/control-endpoint.json"} {
		if _, err := os.Lstat(c.path(filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("Run retained control endpoint after exit")
		}
	}
	file, err := openLockFile(c.path(".service-host.lock"), false)
	if err != nil {
		t.Fatal("Run removed stable host lease inode")
	}
	lease := &operationLock{files: []*os.File{file}}
	defer func() { _ = lease.Close() }()
	if lockFileErr := lockFile(file); lockFileErr != nil {
		t.Fatal("Run did not release host lease")
	}
}

func serviceMaintenanceRunParity(t *testing.T, kind string) {
	t.Helper()
	c, d := serviceMaintenanceShortCell(t)
	if kind == "stopped" {
		if c.SetServiceDesiredState(t.Context(), "stopped") != nil {
			t.Fatal("cannot seed stopped parity descriptor")
		}
		var err error
		d, err = c.ServiceDescriptor()
		if err != nil {
			t.Fatal("cannot read stopped parity descriptor")
		}
	}
	h, now, _, _, _ := supervisorTestHooks()
	seed := supervisorRecord{Version: 1, RepairRevision: d.RepairRevision, Attempts: 2, Failures: []time.Time{now.Add(-time.Minute)}}
	if writeAtomic(c.path("service", "supervisor.json"), seed, nil) != nil {
		t.Fatal("cannot seed parity supervisor")
	}
	before := serviceMaintenanceSnapshot(t, c)
	parent, cancel := context.WithTimeout(t.Context(), 8*c.cfg.Timeout)
	defer cancel()
	ctx := &serviceMaintenanceIdleContext{Context: parent, cancel: cancel}
	h.ticks = make(chan time.Time)
	var events []string
	var leaseBytes []byte
	starts, stops := 0, 0
	h.reconcile = func() error {
		events = append(events, "reconcile")
		leaseBytes = serviceMaintenancePublishedHost(t, c, d)
		return c.reconcileServiceBoot()
	}
	h.repair = func(opCtx context.Context) error { events = append(events, "repair"); return c.Repair(opCtx) }
	h.stop = func(context.Context) error {
		events = append(events, "stop")
		stops++
		if kind == "stopped" || kind == "start_failure" && starts == 1 {
			ctx.ready.Store(true)
		}
		return nil
	}
	h.start = func(context.Context) error {
		events = append(events, "start")
		starts++
		if kind == "start_failure" {
			return errors.New("synthetic service parity start failure")
		}
		return nil
	}
	h.health = func(context.Context) error { events = append(events, "health"); ctx.ready.Store(true); return nil }
	receipt, err := serviceMaintenanceCaptureReceipt(t, func() error { return c.Run(ctx, SupervisorPolicy{hooks: &h, ShutdownTimeout: c.cfg.Timeout}) })
	if err != nil || errors.Is(parent.Err(), context.DeadlineExceeded) {
		t.Fatal("absent Run result changed")
	}
	wantEvents := []string{"reconcile", "repair", "stop", "start", "health", "repair", "stop"}
	want := seed
	want.Attempts++
	wantStarts, wantStops := 1, 2
	wantReceipt := "service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true\n"
	if kind == "success" {
		// Releasing the health child context observes Done after the hook
		// arms cancellation. BASE therefore records tick, before the idle
		// transition; keep that exact transcript rather than normalizing it.
		wantReceipt = "service_shutdown phase=tick operation=none journal_pending=false journal_read_ok=true\n"
	}
	if kind == "start_failure" {
		wantEvents = []string{"reconcile", "repair", "stop", "start", "stop", "repair", "stop"}
		want.Failures = []time.Time{*now}
		want.NextStart = now.Add(h.initialBackoff)
		wantStops = 3
		wantReceipt = "native service host site=supervisor_start\n" + wantReceipt
	}
	if kind == "stopped" {
		wantEvents = []string{"reconcile", "repair", "stop", "repair", "stop"}
		want = seed
		wantStarts = 0
	}
	if !reflect.DeepEqual(events, wantEvents) || starts != wantStarts || stops != wantStops {
		t.Fatal("absent Run hook order or counters changed")
	}
	if string(receipt) != wantReceipt {
		t.Fatal("absent Run fixed receipt changed")
	}
	serviceMaintenanceParityFile(t, c, "service/supervisor.json", serviceMaintenanceParityJSON(t, want))
	boot, bootErr := serviceBootID()
	if bootErr != nil {
		t.Fatal("native parity boot unavailable")
	}
	serviceMaintenanceParityFile(t, c, "service/boot.json", serviceMaintenanceParityJSON(t, serviceBootRecord{d.InstallationID, boot}))
	serviceMaintenanceParityFile(t, c, "service/lease.json", leaseBytes)
	serviceMaintenanceParityFile(t, c, ".service-host.lock", nil)
	serviceMaintenanceHostReleased(t, c)
	serviceMaintenanceParityUnchanged(t, c, before, ".service-host.lock", "service/boot.json", "service/lease.json", "service/supervisor.json")
}

func TestServiceMaintenanceRunStartupBoundary(t *testing.T) {
	for _, kind := range []string{"Valid", "Corrupt"} {
		t.Run(kind, func(t *testing.T) {
			c, d := serviceMaintenanceShortCell(t)
			serviceMaintenanceFence(t, c, kind)
			before := serviceMaintenanceSnapshot(t, c)
			h, _, _, _, _ := supervisorTestHooks()
			var events []string
			var leaseBytes []byte
			h.reconcile = func() error {
				events = append(events, "reconcile")
				leaseBytes = serviceMaintenancePublishedHost(t, c, d)
				return c.reconcileServiceBoot()
			}
			h.repair = func(ctx context.Context) error { events = append(events, "repair"); return c.Repair(ctx) }
			h.stop = func(context.Context) error { events = append(events, "stop"); return nil }
			h.start = func(context.Context) error { events = append(events, "start"); return nil }
			h.health = func(context.Context) error { events = append(events, "health"); return nil }
			ctx, cancel := context.WithTimeout(t.Context(), 8*c.cfg.Timeout)
			defer cancel()
			receipt, err := serviceMaintenanceCaptureReceipt(t, func() error { return c.Run(ctx, SupervisorPolicy{hooks: &h, ShutdownTimeout: c.cfg.Timeout}) })
			if !errors.Is(err, errMaintenanceFenceActive) || err.Error() != errMaintenanceFenceActive.Error()+"\n"+errMaintenanceFenceActive.Error() {
				t.Fatal("fenced Run did not retain reconcile and deferred Repair causes")
			}
			if !reflect.DeepEqual(events, []string{"reconcile", "repair", "stop"}) {
				t.Fatal("fenced Run changed startup refusal or protective shutdown order")
			}
			if string(receipt) != "native service host site=supervisor_reconcile\n" {
				t.Fatal("fenced Run did not report reconcile failure site")
			}
			var located *serviceFailure
			if !errors.As(err, &located) || located.site != "startup_reconcile" {
				t.Fatal("fenced Run did not retain startup_reconcile boundary")
			}
			serviceMaintenanceParityFile(t, c, "service/lease.json", leaseBytes)
			serviceMaintenanceParityFile(t, c, ".service-host.lock", nil)
			serviceMaintenanceHostReleased(t, c)
			serviceMaintenanceParityUnchanged(t, c, before, ".service-host.lock", "service/lease.json")
		})
	}
}
