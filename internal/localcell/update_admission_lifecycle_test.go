package localcell_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

// These expected bytes and traces are replayed against the saved pre-admission
// lifecycle source as well as the candidate. Only generated identity, creation
// time, allocated ports and the at field of journal records are substituted.
// This proves serialization and execution parity, not identical random draws.
func TestMaintenanceFenceAbsentLifecycleParity(t *testing.T) {
	binaries := buildLifecycleFixture(t)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, absence := range []string{"AbsentParent", "AbsentLeaf", "EmptyJournal"} {
		t.Run(absence, func(t *testing.T) {
			var trace []string
			var root string
			var removalJournal []byte
			cell, root := disposableCell(t, binaries, func(op, step string) error {
				trace = append(trace, op+":"+step)
				if op == "remove" && step == "cleanup_run" {
					removalJournal = admissionRead(t, root, "journal", "operations.jsonl")
				}
				return nil
			})
			if absence != "AbsentParent" {
				// A canceled version command leaves the production-created private
				// layout before manifest/journal publication, on every platform.
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := cell.Install(ctx, localcell.InstallOptions{}); err == nil || err.Error() != "cannot read PostgreSQL major/minor version" {
					t.Fatal("empty journal setup did not stop at the legacy version boundary")
				}
				admissionResetJournal(t, root, absence == "AbsentLeaf")
			}
			if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
				t.Fatal("absent install did not reach its successful legacy boundary")
			}
			wantInstall := []string{"manifest", "password", "initdb", "configure", "postgres", "database", "stop"}
			admissionAssertTrace(t, trace, admissionTrace("install", wantInstall))
			admissionAssertJournal(t, root, admissionJournal("install", wantInstall))
			manifest := admissionManifest(t, root)
			admissionAssertManifest(t, root, manifest, "install", false, true)
			assertFixtureEvents(t, root, "postgres-start\npostgres-stop\n")
			// Subsequent operations use fixed identity/time. Ports remain the
			// successful allocation already proved by the Install assertion.
			manifest.InstallationID = "lcmaintenanceparity"
			manifest.CreatedAt = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
			admissionWriteManifest(t, root, manifest)
			switch absence {
			case "AbsentParent":
				if err := os.RemoveAll(filepath.Join(root, "journal")); err != nil {
					t.Fatal("cannot prepare absent journal parent")
				}
			case "EmptyJournal":
				if err := os.Remove(filepath.Join(root, "journal", "operations.jsonl")); err != nil {
					t.Fatal("cannot prepare empty journal directory")
				}
			}
			admissionResetJournal(t, root, absence == "AbsentLeaf")
			trace = nil
			if err := lifecycleWriteFile(filepath.Join(root, "fixture-events"), nil); err != nil {
				t.Fatal("cannot reset process trace")
			}
			if err := cell.Start(t.Context()); err != nil {
				t.Fatal("absent start did not reach successful legacy boundary")
			}
			wantStart := []string{"postgres", "server", "server_ready", "account", "worker", "worker_ready"}
			admissionAssertTrace(t, trace, admissionTrace("start", wantStart))
			admissionAssertJournal(t, root, admissionJournal("start", wantStart))
			admissionAssertManifest(t, root, manifest, "start", true, true)
			assertFixtureEvents(t, root, "postgres-start\nserver-start\nworker-start\n")
			for _, component := range []string{"server", "worker"} {
				assertFixtureLaunches(t, root, component, 1)
			}
			// Already-running Start still writes its legacy successful journal,
			// but preserves manifest bytes and does not launch another process.
			before := admissionRead(t, root, "manifest.json")
			trace = nil
			admissionResetJournal(t, root, true)
			if err := cell.Start(t.Context()); err != nil {
				t.Fatal("already-running absent start changed its result")
			}
			admissionAssertTrace(t, trace, admissionTrace("start", wantStart))
			admissionAssertJournal(t, root, admissionJournal("start", wantStart))
			admissionAssertBytes(t, root, "manifest.json", before)
			assertFixtureEvents(t, root, "postgres-start\nserver-start\nworker-start\n")
			for _, component := range []string{"server", "worker"} {
				assertFixtureLaunches(t, root, component, 1)
			}
			trace = nil
			admissionResetJournal(t, root, true)
			if err := cell.Stop(t.Context()); err != nil {
				t.Fatal("legacy stop failed")
			}
			admissionAssertTrace(t, trace, []string{"stop:stop"})
			admissionAssertJournal(t, root, admissionJournal("stop", []string{"stop"}))
			admissionAssertManifest(t, root, manifest, "stop", true, true)
			assertFixtureEvents(t, root, "postgres-start\nserver-start\nworker-start\nworker-stop\nserver-stop\npostgres-stop\n")
			before = admissionRead(t, root, "manifest.json")
			journal := admissionRead(t, root, "journal", "operations.jsonl")
			trace = nil
			if err := cell.Repair(t.Context()); err != nil {
				t.Fatal("absent no-op repair failed")
			}
			admissionAssertTrace(t, trace, nil)
			admissionAssertBytes(t, root, "manifest.json", before)
			admissionAssertBytes(t, root, "journal/operations.jsonl", journal)
			trace = nil
			admissionResetJournal(t, root, true)
			if err := cell.Remove(t.Context(), localcell.RemoveOptions{}); err != nil {
				t.Fatal("absent retain removal failed")
			}
			admissionAssertTrace(t, trace, admissionTrace("remove", []string{"stop", "spool", "tmp", "logs", "cleanup_run", "cleanup_journal"}))
			admissionAssertRemovalJournal(t, removalJournal, false)
			admissionAssertManifest(t, root, manifest, "remove", true, true)
			admissionAssertRemoved(t, root, "run", "spool", "tmp", "logs", "journal")
			if data := admissionRead(t, root, "pg", "PG_VERSION"); string(data) != "16\n" {
				t.Fatal("retain removal changed PostgreSQL data")
			}
			trace = nil
			if err := cell.Remove(t.Context(), localcell.RemoveOptions{Purge: true}); err != nil {
				t.Fatal("absent purge removal failed")
			}
			admissionAssertTrace(t, trace, admissionTrace("remove", []string{"stop", "spool", "tmp", "logs", "purge", "cleanup_run", "cleanup_journal"}))
			admissionAssertRemovalJournal(t, removalJournal, true)
			admissionAssertRemoved(t, root, "manifest.json", "pg", "journal", "run")
		})
	}
	t.Run("InterruptedInstallRepair", func(t *testing.T) {
		var trace []string
		cell, root := disposableCell(t, binaries, func(op, step string) error {
			trace = append(trace, op+":"+step)
			if op == "install" && step == "stop" {
				return errors.New("fixture interruption")
			}
			return nil
		})
		if err := cell.Install(t.Context(), localcell.InstallOptions{}); err == nil || err.Error() != "injected local cell interruption; repair required" {
			t.Fatal("interrupted install changed exact legacy error")
		}
		admissionAssertTrace(t, trace, admissionTrace("install", []string{"manifest", "password", "initdb", "configure", "postgres", "database", "stop"}))
		manifest := admissionManifest(t, root)
		admissionAssertManifest(t, root, manifest, "install", false, false)
		want := strings.TrimSuffix(admissionJournal("install", []string{"manifest", "password", "initdb", "configure", "postgres", "database"}), admissionJournalLine("install", "operation", "done", ""))
		want += admissionJournalLine("install", "stop", "started", "")
		admissionAssertJournal(t, root, want)
		trace = nil
		if err := cell.Repair(t.Context()); err != nil {
			t.Fatal("absent repair did not complete durable interrupted install")
		}
		admissionAssertTrace(t, trace, []string{"install:revert_stop"})
		want += admissionJournalLine("install", "revert_stop", "started", "") + admissionJournalLine("install", "revert_stop", "done", "") + admissionJournalLine("install", "operation", "done", "")
		admissionAssertJournal(t, root, want)
		admissionAssertManifest(t, root, manifest, "repair", false, true)
		assertFixtureEvents(t, root, "postgres-start\npostgres-stop\n")
		assertStopped(t, cell)
	})
	t.Run("InterruptedStartRepair", func(t *testing.T) {
		var trace []string
		interrupt := false
		cell, root := disposableCell(t, binaries, func(op, step string) error {
			trace = append(trace, op+":"+step)
			if interrupt && op == "start" && step == "postgres" {
				return errors.New("fixture interruption")
			}
			return nil
		})
		if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
			t.Fatal("interrupted-start setup failed")
		}
		manifest := admissionManifest(t, root)
		admissionResetJournal(t, root, true)
		trace = nil
		interrupt = true
		if err := cell.Start(t.Context()); err == nil || err.Error() != "injected local cell interruption; repair required" {
			t.Fatal("interrupted start changed exact legacy error")
		}
		admissionAssertTrace(t, trace, []string{"start:postgres"})
		admissionAssertJournal(t, root, admissionJournalLine("start", "operation", "started", "")+admissionJournalLine("start", "postgres", "started", ""))
		trace = nil
		if err := cell.Repair(t.Context()); err != nil {
			t.Fatal("absent repair did not settle interrupted start")
		}
		admissionAssertTrace(t, trace, []string{"start:revert_stop"})
		admissionAssertJournal(t, root, admissionJournalLine("start", "operation", "started", "")+admissionJournalLine("start", "postgres", "started", "")+admissionJournalLine("start", "revert_stop", "started", "")+admissionJournalLine("start", "revert_stop", "done", "")+admissionJournalLine("start", "operation", "done", ""))
		admissionAssertManifest(t, root, manifest, "repair", false, true)
		assertFixtureEvents(t, root, "postgres-start\npostgres-stop\npostgres-start\npostgres-stop\n")
		assertStopped(t, cell)
	})
	t.Run("Rollback", func(t *testing.T) {
		cell, root := installedLostPortCell(t, binaries)
		manifest := admissionManifest(t, root)
		admissionResetJournal(t, root, true)
		writeLostPortMarker(t, root, "fixture-bind-other-server", "")
		err := cell.Start(t.Context())
		want := "local cell start: server_ready failed: local cell server exited before readiness (exit status 1): witself-server: listener bind failed: other; wait_observation=wait_completed recovered_alive=false"
		if err == nil || err.Error() != want {
			t.Fatal("absent rollback changed exact failure attribution")
		}
		journal := admissionJournalLine("start", "operation", "started", "")
		for _, step := range []string{"postgres", "server"} {
			journal += admissionJournalLine("start", step, "started", "") + admissionJournalLine("start", step, "done", "")
		}
		cause := strings.TrimPrefix(want, "local cell start: server_ready failed: ")
		journal += admissionJournalLine("start", "server_ready", "started", "") + admissionJournalLine("start", "server_ready", "failed", cause) + admissionJournalLine("start", "operation", "done", "")
		admissionAssertJournal(t, root, journal)
		admissionAssertManifest(t, root, manifest, "start", false, true)
		assertFixtureLaunches(t, root, "server", 1)
		assertFixtureEvents(t, root, "postgres-start\npostgres-stop\npostgres-start\npostgres-stop\n")
		assertStopped(t, cell)
	})
	t.Run("PortRelaunch", func(t *testing.T) {
		cell, root := installedLostPortCell(t, binaries)
		before := admissionManifest(t, root)
		admissionResetJournal(t, root, true)
		writeLostPortMarker(t, root, "fixture-steal-port-server", "once")
		var probes []string
		starter, err := localcell.New(localcell.Config{Root: root, Binaries: binaries, Timeout: time.Minute,
			Failpoint: func(operation, step string) error {
				if operation != "start" {
					return nil
				}
				if err := cell.Start(t.Context()); !errors.Is(err, localcell.ErrLocked) {
					t.Fatal("independent Start acquired the operation lock during readiness relaunch")
				}
				if step == "server_ready" {
					// This callback follows successful recovery, so its contention
					// witness covers the actual second launch and readiness result.
					assertFixtureLaunches(t, root, "server", 2)
				}
				probes = append(probes, operation+":"+step)
				return nil
			}})
		if err != nil {
			t.Fatal("cannot construct independent relaunch contender fixture")
		}
		if err := starter.Start(t.Context()); err != nil {
			t.Fatal("absent lost-port start failed")
		}
		admissionAssertTrace(t, probes, admissionTrace("start", []string{"postgres", "server", "server_ready", "account", "worker", "worker_ready"}))
		after := admissionManifest(t, root)
		assertOnlyComponentPortsChanged(t, before.Ports, after.Ports, "server")
		assertFixtureLaunches(t, root, "server", 2)
		assertFixtureLaunches(t, root, "worker", 1)
		assertPortRecoveryCauses(t, root, "server", true)
		wantJournal := admissionJournalLine("start", "operation", "started", "")
		for _, step := range []string{"postgres", "server", "server_ready", "account", "worker", "worker_ready"} {
			wantJournal += admissionJournalLine("start", step, "started", "")
			if step == "server_ready" {
				wantJournal += admissionJournalLine("start", "server", "started", "port_reallocated=server") + admissionJournalLine("start", "server_ready", "started", "port_conflict_relaunch=server")
			}
			wantJournal += admissionJournalLine("start", step, "done", "")
		}
		wantJournal += admissionJournalLine("start", "operation", "done", "")
		admissionAssertJournal(t, root, wantJournal)
		before.Ports = after.Ports
		admissionAssertManifest(t, root, before, "start", true, true)
		assertFixtureEvents(t, root, "postgres-start\npostgres-stop\npostgres-start\nserver-start\nworker-start\n")
		manifest := admissionRead(t, root, "manifest.json")
		journal := admissionRead(t, root, "journal", "operations.jsonl")
		if err := cell.Repair(t.Context()); err != nil {
			t.Fatal("relaunch retained the operation lock after return")
		}
		admissionAssertBytes(t, root, "manifest.json", manifest)
		admissionAssertBytes(t, root, "journal/operations.jsonl", journal)
	})
}

func TestMaintenanceFenceStopAndInspectionRemainUsable(t *testing.T) {
	binaries := buildLifecycleFixture(t)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	cell, root := installedLostPortCell(t, binaries)
	if err := cell.Start(t.Context()); err != nil {
		t.Fatal("unfenced observation setup failed")
	}
	before, err := cell.Inspect(t.Context())
	if err != nil {
		t.Fatal("unfenced inspection failed")
	}
	status, err := cell.Status(t.Context())
	if err != nil || !status.ServerReady || !status.WorkerReady {
		t.Fatal("unfenced status did not reach ready observation")
	}
	fence := []byte("{\"generation\":7,\"operation_id\":\"maintenance-parity\",\"since\":\"2026-11-10T00:00:00Z\"}\n")
	fencePath := filepath.Join(root, "journal", "maintenance.json")
	if err := lifecycleWriteFile(fencePath, fence); err != nil {
		t.Fatal("cannot plant private maintenance fixture")
	}
	// Fixture removal is test cleanup, never a production clearing procedure.
	t.Cleanup(func() { _ = os.Remove(fencePath) })
	manifest := admissionRead(t, root, "manifest.json")
	journal := admissionRead(t, root, "journal", "operations.jsonl")
	after, err := cell.Inspect(t.Context())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("maintenance fence changed inspection output")
	}
	fencedStatus, err := cell.Status(t.Context())
	if err != nil || !reflect.DeepEqual(status, fencedStatus) {
		t.Fatal("maintenance fence changed status output")
	}
	admissionAssertBytes(t, root, "manifest.json", manifest)
	admissionAssertBytes(t, root, "journal/operations.jsonl", journal)
	if err := lifecycleWriteFile(filepath.Join(root, "fixture-events"), nil); err != nil {
		t.Fatal("cannot reset stop trace")
	}
	admissionResetJournal(t, root, true)
	if err := cell.Stop(t.Context()); err != nil {
		t.Fatal("maintenance fence blocked safety stop")
	}
	assertFixtureEvents(t, root, "worker-stop\nserver-stop\npostgres-stop\n")
	admissionAssertJournal(t, root, admissionJournal("stop", []string{"stop"}))
	assertStopped(t, cell)
	admissionAssertBytes(t, root, "journal/maintenance.json", fence)
}

func admissionRead(t *testing.T, root string, parts ...string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{root}, parts...)...))
	if err != nil {
		t.Fatal("cannot read parity fixture bytes")
	}
	return data
}

func admissionManifest(t *testing.T, root string) localcell.Manifest {
	t.Helper()
	var m localcell.Manifest
	if err := json.Unmarshal(admissionRead(t, root, "manifest.json"), &m); err != nil {
		t.Fatal("cannot decode parity manifest")
	}
	return m
}

func admissionWriteManifest(t *testing.T, root string, m localcell.Manifest) {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil || lifecycleWriteFile(filepath.Join(root, "manifest.json"), append(data, '\n')) != nil {
		t.Fatal("cannot write fixed parity manifest")
	}
}

func admissionAssertManifest(t *testing.T, root string, generated localcell.Manifest, operation string, verified, installed bool) {
	t.Helper()
	// Independent field list keeps an added serialized field observable.
	account := ""
	schema := 0
	if verified {
		account = ",\n  \"account_id\": \"acc_aaaaaaaaaaaaaaaa\""
		schema = 1
	}
	id, _ := json.Marshal(generated.InstallationID)
	created, _ := json.Marshal(generated.CreatedAt)
	want := fmt.Sprintf("{\n  \"layout_version\": 1,\n  \"installation_id\": %s,\n  \"created_at\": %s,\n  \"postgresql_version\": \"postgres (PostgreSQL) 16.1\",\n  \"server_version\": \"localcell synthetic lifecycle fixture\",\n  \"worker_version\": \"localcell synthetic lifecycle fixture\",\n  \"ports\": {\n    \"postgres\": %d,\n    \"server_api\": %d,\n    \"server_health\": %d,\n    \"server_metrics\": %d,\n    \"worker_health\": %d,\n    \"worker_metrics\": %d\n  },\n  \"database\": \"witself\",\n  \"role\": \"localcell\"%s,\n  \"last_verified_schema_version\": %d,\n  \"last_operation\": %q,\n  \"installed\": %t,\n  \"migration_wait\": 120000000000,\n  \"job_restart_policy\": \"never\"\n}\n", id, created, generated.Ports.Postgres, generated.Ports.ServerAPI, generated.Ports.ServerHealth, generated.Ports.ServerMetrics, generated.Ports.WorkerHealth, generated.Ports.WorkerMetrics, account, schema, operation, installed)
	if !bytes.Equal(admissionRead(t, root, "manifest.json"), []byte(want)) {
		t.Fatal("manifest serialization differs from saved BASE fixture")
	}
}

func admissionJournalLine(operation, step, state, cause string) string {
	if cause != "" {
		return fmt.Sprintf("{\"operation\":%q,\"step\":%q,\"state\":%q,\"at\":\"AT\",\"cause\":%q}\n", operation, step, state, cause)
	}
	return fmt.Sprintf("{\"operation\":%q,\"step\":%q,\"state\":%q,\"at\":\"AT\"}\n", operation, step, state)
}

func admissionJournal(operation string, steps []string) string {
	want := admissionJournalLine(operation, "operation", "started", "")
	for _, step := range steps {
		want += admissionJournalLine(operation, step, "started", "") + admissionJournalLine(operation, step, "done", "")
	}
	return want + admissionJournalLine(operation, "operation", "done", "")
}

func admissionAssertJournal(t *testing.T, root, want string) {
	t.Helper()
	admissionAssertJournalBytes(t, admissionRead(t, root, "journal", "operations.jsonl"), want)
}

func admissionAssertRemovalJournal(t *testing.T, data []byte, purge bool) {
	t.Helper()
	steps := []string{"stop", "spool", "tmp", "logs"}
	if purge {
		steps = append(steps, "purge")
	}
	want := strings.TrimSuffix(admissionJournal("remove", steps), admissionJournalLine("remove", "operation", "done", ""))
	want += admissionJournalLine("remove", "runtime_cleanup", "started", "")
	admissionAssertJournalBytes(t, data, want)
}

func admissionAssertJournalBytes(t *testing.T, data []byte, want string) {
	t.Helper()
	var normalized strings.Builder
	for _, line := range bytes.SplitAfter(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var fields map[string]json.RawMessage
		var at time.Time
		if json.Unmarshal(line, &fields) != nil || json.Unmarshal(fields["at"], &at) != nil || at.IsZero() {
			t.Fatal("parity journal lacks a valid generated timestamp")
		}
		needle := append([]byte(`"at":`), fields["at"]...)
		if bytes.Count(line, needle) != 1 {
			t.Fatal("parity timestamp is not one exact known journal field")
		}
		normalized.Write(bytes.Replace(line, needle, []byte(`"at":"AT"`), 1))
	}
	if normalized.String() != want {
		t.Fatal("journal serialization or step order differs from saved BASE fixture")
	}
}

func admissionResetJournal(t *testing.T, root string, exists bool) {
	t.Helper()
	if exists && lifecycleWriteFile(filepath.Join(root, "journal", "operations.jsonl"), nil) != nil {
		t.Fatal("cannot reset completed parity journal")
	}
}

func admissionTrace(operation string, steps []string) []string {
	out := make([]string, len(steps))
	for i, step := range steps {
		out[i] = operation + ":" + step
	}
	return out
}

func admissionAssertTrace(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatal("operation callback order differs from saved BASE fixture")
	}
}

func admissionAssertBytes(t *testing.T, root, name string, before []byte) {
	t.Helper()
	if !bytes.Equal(admissionRead(t, root, name), before) {
		t.Fatal("read-only or no-op path changed durable fixture bytes")
	}
}

func admissionAssertRemoved(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("legacy removal did not remove its expected target")
		}
	}
}
