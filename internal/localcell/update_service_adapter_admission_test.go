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

func windowsTaskMaintenanceKinds() []string {
	kinds := []string{"Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	return kinds
}

// The fake records live outside the cell. Inspect only its two existing path
// accessors and directory entries; cell snapshots use the shared safe helper.
func windowsTaskMaintenanceRecords(t *testing.T, f *windowsTaskFake) map[string]serviceMaintenanceEntry {
	t.Helper()
	entries, err := os.ReadDir(f.a.directory(f.d))
	if err != nil {
		t.Fatal("Task record directory cannot be inspected")
	}
	info, err := os.Lstat(f.a.directory(f.d))
	if err != nil {
		t.Fatal("Task record directory mode is unavailable")
	}
	result := map[string]serviceMaintenanceEntry{".": {mode: info.Mode()}}
	for _, entry := range entries {
		var path string
		switch entry.Name() {
		case filepath.Base(f.a.xmlPath(f.d)):
			path = f.a.xmlPath(f.d)
		case filepath.Base(f.a.receiptPath(f.d)):
			path = f.a.receiptPath(f.d)
		default:
			t.Fatal("Task record directory contains an unexpected object")
		}
		data, info, err := readWindowsTaskRecord(path)
		if err != nil {
			t.Fatal("Task private record cannot be inspected")
		}
		result[entry.Name()] = serviceMaintenanceEntry{mode: info.Mode(), data: string(data)}
	}
	return result
}

func windowsTaskMaintenanceRecordsUnchanged(t *testing.T, f *windowsTaskFake, before map[string]serviceMaintenanceEntry) {
	t.Helper()
	if !reflect.DeepEqual(before, windowsTaskMaintenanceRecords(t, f)) {
		t.Fatal("Task maintenance refusal changed private records")
	}
}

func windowsTaskMaintenanceQueriesOnly(t *testing.T, f *windowsTaskFake) {
	t.Helper()
	for _, call := range f.calls {
		if len(call) == 0 || call[0] != "/Query" {
			t.Fatal("Task maintenance refusal issued a manager mutation")
		}
	}
}

func windowsTaskMaintenanceDiagnostic(t *testing.T, got, kind error, step, reason string) {
	t.Helper()
	if !errors.Is(got, kind) {
		t.Fatal("Task refusal changed the expected error sentinel")
	}
	if !strings.Contains(got.Error(), ": "+step+":") {
		t.Fatal("Task refusal changed the expected diagnostic step")
	}
	if !strings.Contains(got.Error(), "reason="+reason) {
		t.Fatal("Task refusal changed the expected diagnostic reason")
	}
}

func TestWindowsTaskMaintenanceApplyAdmission(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, state := range []string{"fresh", "owned", "staged"} {
		for _, kind := range windowsTaskMaintenanceKinds() {
			t.Run(state+"/"+kind, func(t *testing.T) {
				f := newWindowsTaskFake(t)
				if state != "fresh" {
					f.apply()
					if state == "staged" {
						f.xml = nil
					}
				}
				plan, err := f.a.Plan(t.Context(), f.d)
				if err != nil {
					t.Fatal("Fenced Apply fixture could not construct a plan")
				}
				serviceMaintenanceFence(t, f.cell, kind)
				before := serviceMaintenanceSnapshot(t, f.cell)
				records := windowsTaskMaintenanceRecords(t, f)
				if state == "fresh" && len(records) != 1 {
					t.Fatal("Fresh Apply fixture already contains task records")
				}
				f.calls = nil
				serviceMaintenanceCause(t, f.a.Apply(t.Context(), plan))
				serviceMaintenanceUnchanged(t, f.cell, before)
				windowsTaskMaintenanceRecordsUnchanged(t, f, records)
				windowsTaskMaintenanceQueriesOnly(t, f)
				serviceMaintenanceReleased(t, f.cell)
			})
		}
	}
}

func TestWindowsTaskMaintenanceStartAdmission(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, kind := range windowsTaskMaintenanceKinds() {
		t.Run(kind, func(t *testing.T) {
			f := newWindowsTaskFake(t)
			f.apply()
			serviceMaintenanceFence(t, f.cell, kind)
			before := serviceMaintenanceSnapshot(t, f.cell)
			records := windowsTaskMaintenanceRecords(t, f)
			f.calls = nil
			serviceMaintenanceCause(t, f.a.Start(t.Context(), f.d))
			if f.count("/Run") != 0 {
				t.Fatal("Fenced Start issued a run command")
			}
			serviceMaintenanceUnchanged(t, f.cell, before)
			windowsTaskMaintenanceRecordsUnchanged(t, f, records)
			windowsTaskMaintenanceQueriesOnly(t, f)
			serviceMaintenanceReleased(t, f.cell)
		})
	}
}

func TestWindowsTaskMaintenanceAdmissionPrecedence(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, name := range []string{"non-user", "capability", "contender", "immutable-mismatch", "unreadable-descriptor", "mutable-drift", "desired-stopped", "faulted"} {
		t.Run("Apply/"+name, func(t *testing.T) {
			f := newWindowsTaskFake(t)
			plan, err := f.a.Plan(t.Context(), f.d)
			if err != nil {
				t.Fatal("Apply precedence fixture cannot construct its plan")
			}
			serviceMaintenanceFence(t, f.cell, "Valid")
			kind, step, reason := ErrServiceOwnershipConflict, "create", "stale_plan"
			var release func()
			switch name {
			case "non-user":
				plan.RequiredPrivilege = "system"
				reason = "not_user"
			case "capability":
				f.a.capability = nil
				kind, step, reason = ErrServiceManagerUnavailable, "capability:session", "interactive_token_unavailable"
			case "contender":
				other, err := New(f.cell.cfg)
				if err != nil {
					t.Fatal("Apply precedence contender cannot be constructed")
				}
				lock, err := other.own(false)
				if err != nil {
					t.Fatal("Apply precedence contender cannot acquire its lock")
				}
				release = func() {
					if lock.Close() != nil {
						t.Fatal("Apply precedence contender cannot release its lock")
					}
				}
				t.Cleanup(func() { _ = lock.Close() })
				// Exercise production lock error mapping with the native root.
				f.a.lock = func(ServiceDescriptor) (*Cell, func(), error) {
					return (&windowsLogonTaskAdapter{}).mutationLock(ServiceDescriptor{Root: f.cell.cfg.Root})
				}
				step, reason = "inspect:host_owner", "unavailable"
			case "immutable-mismatch":
				f.afterQuery = func() { f.current.Payload.Generation = "foreign-generation" }
			case "unreadable-descriptor":
				f.afterQuery = func() {
					f.a.readDescriptor = func(ServiceDescriptor) (ServiceDescriptor, error) {
						return ServiceDescriptor{}, errors.New("fixture descriptor unavailable")
					}
				}
			case "mutable-drift":
				f.current.Revision++
			case "desired-stopped":
				f.d.DesiredState, f.current.DesiredState = "stopped", "stopped"
				plan.Spec = f.d
			case "faulted":
				f.d.Faulted, f.current.Faulted = true, true
				plan.Spec = f.d
			}
			before := serviceMaintenanceSnapshot(t, f.cell)
			records := windowsTaskMaintenanceRecords(t, f)
			f.calls = nil
			err = f.a.Apply(t.Context(), plan)
			switch name {
			case "mutable-drift", "desired-stopped", "faulted":
				serviceMaintenanceCause(t, err)
			default:
				windowsTaskMaintenanceDiagnostic(t, err, kind, step, reason)
			}
			windowsTaskMaintenanceQueriesOnly(t, f)
			serviceMaintenanceUnchanged(t, f.cell, before)
			windowsTaskMaintenanceRecordsUnchanged(t, f, records)
			if release != nil {
				release()
			}
			serviceMaintenanceReleased(t, f.cell)
		})
	}
	for _, name := range []string{"desired-stopped", "faulted", "not-owned", "owned-running"} {
		t.Run("Start/"+name, func(t *testing.T) {
			f := newWindowsTaskFake(t)
			if name != "not-owned" {
				f.apply()
			}
			switch name {
			case "desired-stopped":
				f.current.DesiredState = "stopped"
			case "faulted":
				f.current.Faulted = true
			}
			serviceMaintenanceFence(t, f.cell, "Valid")
			before := serviceMaintenanceSnapshot(t, f.cell)
			records := windowsTaskMaintenanceRecords(t, f)
			f.calls = nil
			err := f.a.Start(t.Context(), f.d)
			switch name {
			case "desired-stopped", "faulted":
				windowsTaskMaintenanceDiagnostic(t, err, ErrServiceDesiredState, "inspect:host_owner", "stopped")
			case "not-owned":
				windowsTaskMaintenanceDiagnostic(t, err, ErrServiceOwnershipConflict, "inspect:host_owner", "not_owned")
			case "owned-running":
				serviceMaintenanceCause(t, err)
			}
			windowsTaskMaintenanceQueriesOnly(t, f)
			serviceMaintenanceUnchanged(t, f.cell, before)
			windowsTaskMaintenanceRecordsUnchanged(t, f, records)
			serviceMaintenanceReleased(t, f.cell)
		})
	}
	t.Run("Apply/drift-after-records", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		plan, err := f.a.Plan(t.Context(), f.d)
		if err != nil {
			t.Fatal("Staged drift fixture cannot construct its plan")
		}
		before := serviceMaintenanceSnapshot(t, f.cell)
		f.calls = nil
		drifted := false
		f.afterQuery = func() {
			records, err := f.a.records(f.d)
			if err != nil {
				t.Fatal("Staged drift fixture cannot inspect task records")
			}
			if records.exists {
				f.current.Revision++
				drifted = true
			}
		}
		err = f.a.Apply(t.Context(), plan)
		windowsTaskMaintenanceDiagnostic(t, err, ErrServiceOwnershipConflict, "create", "stale_plan")
		if !drifted {
			t.Fatal("Staged drift fixture never reached the durable record query")
		}
		if f.count("/Create") != 0 {
			t.Fatal("Staged descriptor drift allowed task creation")
		}
		windowsTaskMaintenanceQueriesOnly(t, f)
		serviceMaintenanceUnchanged(t, f.cell, before)
		if len(windowsTaskMaintenanceRecords(t, f)) != 3 {
			t.Fatal("Staged drift fixture did not retain both private records")
		}
		serviceMaintenanceReleased(t, f.cell)
	})
}

func TestWindowsTaskMaintenanceRealLockLifetime(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, point := range []string{"Apply_FirstQuery", "Apply_StagedQuery", "Apply_Create", "Start_Run"} {
		t.Run(point, func(t *testing.T) {
			f := newWindowsTaskFake(t)
			if point == "Start_Run" {
				f.apply()
			}
			plan, err := f.a.Plan(t.Context(), f.d)
			if err != nil {
				t.Fatal("Lock lifetime fixture cannot construct its plan")
			}
			other, err := New(f.cell.cfg)
			if err != nil {
				t.Fatal("Lock lifetime contender cannot be constructed")
			}
			reached, queries := 0, 0
			command := f.a.command
			f.a.command = func(ctx context.Context, args ...string) (windowsTaskCommandResult, error) {
				if args[0] == "/Query" {
					queries++
				}
				check := point == "Apply_FirstQuery" && args[0] == "/Query" && queries == 1 ||
					point == "Apply_StagedQuery" && args[0] == "/Query" && queries == 2 ||
					point == "Apply_Create" && args[0] == "/Create" || point == "Start_Run" && args[0] == "/Run"
				if check {
					if point == "Apply_StagedQuery" {
						records, err := f.a.records(f.d)
						if err != nil || !records.exists {
							t.Fatal("Staged lock proof did not observe both private records")
						}
					}
					reached++
					lock, err := other.own(false)
					if lock != nil {
						_ = lock.Close()
					}
					if err != ErrLocked {
						t.Fatal("Manager command did not retain the real operation lock")
					}
				}
				return command(ctx, args...)
			}
			if point == "Start_Run" {
				err = f.a.Start(t.Context(), f.d)
			} else {
				err = f.a.Apply(t.Context(), plan)
			}
			if err != nil {
				t.Fatal("Lock lifetime operation unexpectedly failed")
			}
			if reached != 1 {
				switch point {
				case "Apply_FirstQuery":
					t.Fatal("Apply first query lock proof was not reached exactly once")
				case "Apply_StagedQuery":
					t.Fatal("Apply staged query lock proof was not reached exactly once")
				case "Apply_Create":
					t.Fatal("Apply create lock proof was not reached exactly once")
				case "Start_Run":
					t.Fatal("Start run lock proof was not reached exactly once")
				}
			}
			lock, err := other.own(false)
			if err != nil {
				t.Fatal("Lock lifetime contender could not acquire after return")
			}
			if lock.Close() != nil {
				t.Fatal("Lock lifetime contender could not release after return")
			}
		})
	}
}

func TestWindowsTaskMaintenanceAbsentParity(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Run("Absent", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		if os.Remove(f.cell.path("journal")) != nil {
			t.Fatal("Absent fence fixture cannot remove its empty journal directory")
		}
		serviceMaintenanceFence(t, f.cell, "Absent")
		f.apply()
		verbs := make([]string, 0, len(f.calls))
		for _, call := range f.calls {
			verbs = append(verbs, call[0])
		}
		if !reflect.DeepEqual(verbs, []string{"/Query", "/Query", "/Query", "/Create", "/Query"}) {
			t.Fatal("Absent fence changed the fresh registration command sequence")
		}
		records, err := f.a.records(f.d)
		if err != nil || !records.exists || f.xml == nil {
			t.Fatal("Absent fence registration did not establish owned private records")
		}
		before := windowsTaskMaintenanceRecords(t, f)
		f.calls = nil
		f.apply()
		if f.count("/Query") != 2 || len(f.calls) != 2 {
			t.Fatal("Absent fence changed the repeated owned registration no-op")
		}
		windowsTaskMaintenanceRecordsUnchanged(t, f, before)
		f.calls = nil
		if f.a.Start(t.Context(), f.d) != nil {
			t.Fatal("Absent fence refused the owned task start")
		}
		if f.count("/Run") != 1 || f.count("/Query") != 1 || len(f.calls) != 2 {
			t.Fatal("Absent fence changed the owned task start command sequence")
		}
		windowsTaskMaintenanceRecordsUnchanged(t, f, before)
		for _, path := range []string{f.cell.path("journal", "maintenance.json"), f.cell.path("journal")} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("Absent admission created maintenance evidence or its parent")
			}
		}
		serviceMaintenanceReleased(t, f.cell)
	})
}

func TestWindowsTaskMaintenanceLockBinding(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, kind := range windowsTaskMaintenanceKinds() {
		t.Run("target/"+kind, func(t *testing.T) {
			target, _ := serviceMaintenanceCell(t, false)
			decoy, _ := serviceMaintenanceCell(t, false)
			serviceMaintenanceFence(t, target, kind)
			a := &windowsLogonTaskAdapter{}
			cell, unlock, err := a.mutationLock(ServiceDescriptor{Root: target.cfg.Root})
			if err != nil || cell == nil || unlock == nil {
				t.Fatal("Production mutation lock did not return its cell and release")
			}
			defer unlock()
			other, err := New(target.cfg)
			if err != nil {
				t.Fatal("Target binding contender cannot be constructed")
			}
			lock, err := other.own(false)
			if lock != nil {
				_ = lock.Close()
			}
			if err != ErrLocked {
				t.Fatal("Production mutation lock did not exclude the target contender")
			}
			decoyLock, err := decoy.own(false)
			if err != nil {
				t.Fatal("Production mutation lock interfered with the decoy root")
			}
			if decoyLock.Close() != nil {
				t.Fatal("Decoy root lock did not release")
			}
			serviceMaintenanceCause(t, cell.maintenanceFenceAllows(0))
			unlock()
			serviceMaintenanceReleased(t, target)
		})
	}
	t.Run("decoy_only", func(t *testing.T) {
		target, _ := serviceMaintenanceCell(t, false)
		decoy, _ := serviceMaintenanceCell(t, false)
		serviceMaintenanceFence(t, decoy, "Valid")
		cell, unlock, err := (&windowsLogonTaskAdapter{}).mutationLock(ServiceDescriptor{Root: target.cfg.Root})
		if err != nil || cell == nil || unlock == nil {
			t.Fatal("Decoy fence fixture could not acquire the target lock")
		}
		defer unlock()
		if cell.maintenanceFenceAllows(0) != nil {
			t.Fatal("Production mutation lock consulted decoy maintenance evidence")
		}
		unlock()
		serviceMaintenanceReleased(t, target)
	})
	t.Run("contender", func(t *testing.T) {
		target, _ := serviceMaintenanceCell(t, false)
		lock, err := target.own(false)
		if err != nil {
			t.Fatal("Binding contention fixture cannot acquire the target lock")
		}
		defer func() { _ = lock.Close() }()
		cell, unlock, err := (&windowsLogonTaskAdapter{}).mutationLock(ServiceDescriptor{Root: target.cfg.Root})
		windowsTaskMaintenanceDiagnostic(t, err, ErrServiceOwnershipConflict, "inspect:host_owner", "unavailable")
		if cell != nil || unlock != nil {
			t.Fatal("Contended mutation lock returned a cell or release function")
		}
		if lock.Close() != nil {
			t.Fatal("Binding contention fixture cannot release the target lock")
		}
		serviceMaintenanceReleased(t, target)
	})
	t.Run("missing_root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "absent")
		cell, unlock, err := (&windowsLogonTaskAdapter{}).mutationLock(ServiceDescriptor{Root: root})
		windowsTaskMaintenanceDiagnostic(t, err, ErrServiceOwnershipConflict, "inspect:host_owner", "unavailable")
		if cell != nil || unlock != nil {
			t.Fatal("Missing root mutation lock returned a cell or release function")
		}
		if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("Missing root mutation lock created its target directory")
		}
	})
}
