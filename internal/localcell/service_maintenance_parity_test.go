package localcell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"testing"
	"time"
)

// This file intentionally uses only BASE APIs and fields. Its expectations run
// unchanged against the saved BASE production files and the candidate. Synthetic
// hooks replace component processes, never the scheduler or descriptor owners.
func TestServiceMaintenanceHostFenceBaselineProbe(t *testing.T) {
	kinds := []string{"Valid", "Corrupt", "Foreign", "Oversized", "Directory"}
	if runtime.GOOS != "windows" {
		kinds = append(kinds, "UnsafeMode", "Symlink")
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			c, d := serviceMaintenanceCell(t, true)
			h, _, starts, stops, _ := supervisorTestHooks()
			healths := 0
			h.health = func(context.Context) error { healths++; return nil }
			s := newSupervisor(c, SupervisorPolicy{hooks: &h})
			if s.load(d) != nil {
				t.Fatal("Cannot load baseline compatible fence probe")
			}
			serviceMaintenanceFence(t, c, kind)
			t.Cleanup(func() { _ = os.Remove(c.path("journal", "maintenance.json")) })
			before := serviceMaintenanceSnapshot(t, c)
			status, err := s.command(t.Context(), "status")
			if err != nil || s.record.Attempts != 0 || len(s.record.Failures) != 0 || !s.record.NextStart.IsZero() || *starts != 0 || *stops != 0 || healths != 0 {
				t.Fatal("Baseline fence probe scheduled enabling work or spent budget")
			}
			if status.State != "maintenance" || status.Reason != "local_maintenance_fence_active" || status.Healthy || status.LastStepOutcome != "ok" {
				t.Fatal("Baseline fence probe omitted completed maintenance projection")
			}
			serviceMaintenanceUnchanged(t, c, before)
		})
	}
}

func TestServiceMaintenanceHostAbsentParity(t *testing.T) {
	for _, row := range []string{
		"idle", "running", "host_exit_accepted", "host_exit_refused", "backoff_not_due", "backoff_due", "clock_rollback", "repair_revision",
		"stopped", "maintenance", "faulted", "drain_stopped", "drain_maintenance", "drain_faulted", "circuit",
		"running_unhealthy", "post_start_unhealthy", "start_failure", "start_busy", "running_health_busy", "post_start_health_busy", "cleanup_busy", "cleanup_failure",
		"status", "health", "observe", "initial_fault_observe", "initial_circuit_observe", "start", "stop", "stop_repair_revision", "stop_clock_rollback", "repair", "restart", "exit_envelope",
	} {
		t.Run(row, func(t *testing.T) { serviceMaintenanceAbsentRow(t, row) })
	}
	for _, row := range []string{"success", "start_failure", "stopped"} {
		t.Run("run_"+row, func(t *testing.T) { serviceMaintenanceRunParity(t, row) })
	}
	for _, row := range []string{"pre_canceled", "start_canceled", "running_health_canceled", "post_start_health_canceled"} {
		t.Run(row, func(t *testing.T) { serviceMaintenanceAbsentCancellation(t, row) })
	}
	for _, row := range []string{"missing", "valid", "repair_revision", "version", "attempts", "failures", "malformed", "save", "traversal"} {
		t.Run("record_"+row, func(t *testing.T) { serviceMaintenanceAbsentRecord(t, row) })
	}
	for _, row := range []string{"step", "status", "health", "start"} {
		t.Run("lifecycle_"+row, func(t *testing.T) { serviceMaintenanceAbsentLifecycle(t, row) })
	}
}

func serviceMaintenanceAbsentLifecycle(t *testing.T, action string) {
	t.Helper()
	f := newAdmissionFixture(t, "StartLegacy")
	c := f.cell
	d := prepareSupervisorTest(t, c)
	m, err := readManifest(c.cfg.Root)
	if err != nil {
		t.Fatal("Cannot read absent lifecycle manifest")
	}
	m.MigrationWait, m.JobRestartPolicy = time.Second, "never"
	if writeAtomic(c.path("manifest.json"), m, nil) != nil {
		t.Fatal("Cannot seed absent lifecycle runtime policy")
	}
	h, _, _, _, _ := supervisorTestHooks()
	var events []string
	h.start = func(ctx context.Context) error { events = append(events, "start"); return c.Start(ctx) }
	h.health = func(context.Context) error { events = append(events, "health"); return nil }
	h.stop = func(ctx context.Context) error { events = append(events, "stop"); return c.stopServiceChildren(ctx) }
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if s.load(d) != nil {
		t.Fatal("Cannot load absent lifecycle supervisor")
	}
	before := serviceMaintenanceSnapshot(t, c)
	var status ServiceStatus
	if action == "step" {
		err = s.step(t.Context())
		status = s.status
	} else {
		status, err = s.command(t.Context(), action)
	}
	if err != nil || !reflect.DeepEqual(events, []string{"start", "health"}) {
		t.Fatal("Absent lifecycle changed successful owner dispatch")
	}
	wantTrace := []string{"postgres", "start:postgres", "server", "start:server", "server_ready", "start:server_ready", "account", "start:account", "worker", "start:worker", "worker_ready", "start:worker_ready"}
	if !reflect.DeepEqual(*f.trace, wantTrace) {
		t.Fatal("Absent lifecycle changed exact component effect order")
	}
	wantStatus := ServiceStatus{DesiredState: "running", State: "running", Healthy: true, Generation: d.Payload.Generation, LastStepOutcome: "ok"}
	gotJSON, gotErr := json.Marshal(status)
	wantJSON, wantErr := json.Marshal(wantStatus)
	if gotErr != nil || wantErr != nil || string(gotJSON) != string(wantJSON) {
		t.Fatal("Absent lifecycle changed complete response bytes")
	}
	if action == "start" {
		d.Revision++
	}
	serviceMaintenanceDescriptorBytes(t, c, d)
	serviceMaintenanceParityFile(t, c, "service/supervisor.json", serviceMaintenanceParityJSON(t, supervisorRecord{Version: 1, Attempts: 1}))
	admissionAssertJournalBytes(t, c, "start", []string{"postgres", "server", "server_ready", "account", "worker", "worker_ready"})
	m.LastOperation = "start"
	serviceMaintenanceParityFile(t, c, "manifest.json", serviceMaintenanceParityJSON(t, m))
	config, configErr := readPostgresFile(c.path("pg", "postgresql.conf"))
	wantConfig := fmt.Sprintf("listen_addresses = '127.0.0.1'\nport = %d\nunix_socket_directories = ''\npassword_encryption = 'scram-sha-256'\nlogging_collector = off\nlog_destination = 'stderr'\n", m.Ports.Postgres)
	if configErr != nil || string(config) != wantConfig {
		t.Fatal("Absent lifecycle changed PostgreSQL configuration bytes")
	}
	serviceMaintenanceParityUnchanged(t, c, before, "service/descriptor.json", "service/supervisor.json", "manifest.json", "journal/operations.jsonl", "pg/postgresql.conf")
}

func serviceMaintenanceAbsentRow(t *testing.T, row string) {
	t.Helper()
	c, d := serviceMaintenanceCell(t, true)
	h, now, _, _, _ := supervisorTestHooks()
	seed := supervisorRecord{Version: 1, RepairRevision: d.RepairRevision, Attempts: 2, Failures: []time.Time{now.Add(-20 * time.Millisecond)}}
	want := seed
	want.Failures = append([]time.Time(nil), seed.Failures...)
	running, wantRunning := false, false
	wantStatus := ServiceStatus{DesiredState: d.DesiredState, State: "running", Healthy: true, Restarts: 2, Generation: d.Payload.Generation, LastStepOutcome: "ok"}
	wantEvents := []string{"start", "health"}
	action := ""
	var wantError error
	startError, healthError, stopError := error(nil), error(nil), error(nil)
	synthetic := errors.New("synthetic absent parity failure")
	switch row {
	case "running", "running_unhealthy", "running_health_busy", "cleanup_busy", "cleanup_failure":
		running, wantRunning = true, true
		wantEvents = []string{"health"}
		wantStatus.Restarts = 1
	case "backoff_not_due":
		seed.NextStart = now.Add(10 * time.Millisecond)
		want.NextStart = seed.NextStart
		wantStatus.State, wantStatus.Healthy, wantStatus.Restarts = "backoff", false, 1
		wantEvents = nil
	case "backoff_due":
		seed.NextStart = *now
		want.NextStart = seed.NextStart
	case "clock_rollback", "stop_clock_rollback":
		seed.NextStart = now.Add(time.Hour)
		want.NextStart = now.Add(h.maxBackoff)
		wantStatus.State, wantStatus.Healthy, wantStatus.Restarts = "backoff", false, 1
		wantEvents = nil
	case "repair_revision", "stop_repair_revision":
		seed.RepairRevision = d.RepairRevision + 1
		want = supervisorRecord{Version: 1, RepairRevision: d.RepairRevision}
		wantStatus.Restarts = 0
	case "stopped", "maintenance", "drain_stopped", "drain_maintenance":
		d.DesiredState = "stopped"
		if row == "maintenance" || row == "drain_maintenance" {
			d.DesiredState = "maintenance"
		}
		wantStatus.DesiredState, wantStatus.State = d.DesiredState, d.DesiredState
		wantStatus.Healthy, wantStatus.Restarts = false, 1
		wantEvents = nil
		if row == "drain_stopped" || row == "drain_maintenance" {
			running = true
			wantEvents = []string{"stop"}
		}
	case "faulted", "drain_faulted", "circuit":
		d.Faulted = row != "circuit"
		wantStatus.State, wantStatus.Reason, wantStatus.Healthy, wantStatus.Restarts = "faulted", "service_circuit_open", false, 1
		wantEvents = nil
		if row == "drain_faulted" {
			running = true
			wantEvents = []string{"stop"}
		}
		if row == "circuit" {
			seed.Failures = []time.Time{now.Add(-5 * time.Millisecond), now.Add(-4 * time.Millisecond), now.Add(-3 * time.Millisecond), now.Add(-2 * time.Millisecond), now.Add(-time.Millisecond)}
			want.Failures = append([]time.Time(nil), seed.Failures...)
		}
	case "status", "health", "start", "stop", "repair", "observe", "restart":
		action = row
	case "initial_fault_observe", "initial_circuit_observe":
		action = "observe"
		d.Faulted = row == "initial_fault_observe"
		if row == "initial_circuit_observe" {
			seed.Failures = []time.Time{now.Add(-5 * time.Millisecond), now.Add(-4 * time.Millisecond), now.Add(-3 * time.Millisecond), now.Add(-2 * time.Millisecond), now.Add(-time.Millisecond)}
			want.Failures = append([]time.Time(nil), seed.Failures...)
		}
	case "exit_envelope":
		action = "exit-host"
	}
	if c.writeServiceDescriptor(d) != nil {
		t.Fatal("Cannot seed absent parity descriptor")
	}
	wantDescriptor := d
	if row == "circuit" {
		wantDescriptor.Faulted, wantDescriptor.Revision = true, d.Revision+1
	}
	if row == "stop_repair_revision" || row == "stop_clock_rollback" {
		action = "stop"
	}
	if action == "start" {
		wantDescriptor.Revision++
	}
	if action == "stop" {
		wantDescriptor.DesiredState, wantDescriptor.Revision = "stopped", d.Revision+1
		wantStatus.DesiredState, wantStatus.State, wantStatus.Healthy = "stopped", "stopped", false
		wantStatus.Restarts = max(0, want.Attempts-1)
		wantEvents = []string{"stop"}
	}
	if action == "repair" {
		wantDescriptor.DesiredState, wantDescriptor.Revision, wantDescriptor.RepairRevision = "stopped", d.Revision+2, d.RepairRevision+1
		want = supervisorRecord{Version: 1, RepairRevision: wantDescriptor.RepairRevision}
		wantStatus.DesiredState, wantStatus.State, wantStatus.Healthy, wantStatus.Restarts = "stopped", "stopped", false, 0
		wantEvents = nil
	}
	if action == "observe" || action == "restart" || action == "exit-host" {
		wantStatus = ServiceStatus{DesiredState: d.DesiredState, State: d.DesiredState, Generation: d.Payload.Generation, LastStepOutcome: "not_run"}
		wantEvents = nil
		if action == "restart" {
			wantError = ErrServiceControlRefused
		}
		if action == "exit-host" {
			wantError = hostExitRefused("envelope")
		}
	}
	if row == "running_unhealthy" || row == "post_start_unhealthy" || row == "cleanup_busy" || row == "cleanup_failure" {
		healthError = synthetic
		wantEvents = []string{"health", "stop"}
		if row == "post_start_unhealthy" {
			wantEvents = []string{"start", "health", "stop"}
		}
	}
	if row == "start_failure" {
		startError = synthetic
		wantEvents = []string{"start", "stop"}
	}
	if row == "running_unhealthy" || row == "post_start_unhealthy" || row == "start_failure" {
		want.Failures = append(want.Failures, *now)
		want.NextStart = now.Add(2 * h.initialBackoff)
		wantStatus.State, wantStatus.Reason, wantStatus.Healthy = "backoff", "service_child_unhealthy", false
		wantStatus.Restarts = 1 // The failure branch retains its entry snapshot.
		wantRunning = false
	}
	if row == "start_busy" {
		startError = ErrLocked
		wantEvents = []string{"start"}
	}
	if row == "running_health_busy" || row == "post_start_health_busy" {
		healthError = ErrLocked
		wantRunning = true
	}
	if row == "cleanup_busy" {
		stopError = ErrLocked
	}
	if row == "start_busy" || row == "running_health_busy" || row == "post_start_health_busy" || row == "cleanup_busy" {
		wantStatus.State, wantStatus.Reason, wantStatus.Healthy, wantStatus.Restarts = "busy", "service_operation_in_progress", false, 1
	}
	if row == "cleanup_failure" {
		stopError = synthetic
		wantStatus.State, wantStatus.Reason, wantStatus.Healthy = "faulted", "service_stop_refused", false
		wantDescriptor.Faulted, wantDescriptor.Revision = true, d.Revision+1
		if runtime.GOOS == "windows" {
			wantDescriptor.Revision++ // The Windows latch persists before fault.
		}
	}
	for _, event := range wantEvents {
		if event == "start" && row != "start_busy" {
			want.Attempts++
			if wantStatus.Healthy {
				wantRunning = true
			}
		}
	}
	var events []string
	h.start = func(context.Context) error { events = append(events, "start"); return startError }
	h.health = func(context.Context) error { events = append(events, "health"); return healthError }
	h.stop = func(context.Context) error { events = append(events, "stop"); return stopError }
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if row == "initial_fault_observe" || row == "initial_circuit_observe" {
		if writeAtomic(c.path("service", "supervisor.json"), seed, nil) != nil {
			t.Fatal("Cannot seed initial absent safety observation")
		}
	}
	if s.load(d) != nil {
		t.Fatal("Cannot load absent parity supervisor")
	}
	if row == "host_exit_accepted" {
		s.status.HostExit = "accepted"
	}
	if row == "host_exit_refused" {
		s.status.HostExit = "refused"
	}
	s.record, s.running = seed, running
	if s.save() != nil {
		t.Fatal("Cannot persist absent parity seed")
	}
	before := serviceMaintenanceSnapshot(t, c)
	var gotStatus ServiceStatus
	var err error
	if action == "" {
		err = s.step(t.Context())
		gotStatus = s.status
	} else {
		gotStatus, err = s.command(t.Context(), action)
	}
	if (err == nil) != (wantError == nil) || err != nil && err.Error() != wantError.Error() {
		t.Fatal("Absent command error identity or bytes changed")
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatal("Absent command hook sequence changed")
	}
	if s.running != wantRunning {
		t.Fatal("Absent command running custody changed")
	}
	if !reflect.DeepEqual(s.record, want) {
		t.Fatal("Absent command in memory record changed")
	}
	gotJSON, gotJSONErr := json.Marshal(gotStatus)
	wantJSON, wantJSONErr := json.Marshal(wantStatus)
	if gotJSONErr != nil || wantJSONErr != nil || string(gotJSON) != string(wantJSON) {
		t.Fatal("Absent command complete status bytes changed")
	}
	serviceMaintenanceParityFile(t, c, "service/supervisor.json", serviceMaintenanceParityJSON(t, want))
	serviceMaintenanceDescriptorBytes(t, c, wantDescriptor)
	exceptions := []string{"service/supervisor.json", "service/descriptor.json"}
	if action == "repair" {
		serviceMaintenanceStopJournal(t, c)
		m, manifestErr := readManifest(c.cfg.Root)
		if manifestErr != nil {
			t.Fatal("Cannot read absent repair manifest")
		}
		var original Manifest
		if json.Unmarshal([]byte(before["manifest.json"].data), &original) != nil {
			t.Fatal("Cannot decode absent repair manifest baseline")
		}
		original.LastOperation = "stop"
		if !reflect.DeepEqual(*m, original) {
			t.Fatal("Absent repair changed manifest beyond stop completion")
		}
		serviceMaintenanceParityFile(t, c, "manifest.json", serviceMaintenanceParityJSON(t, original))
		exceptions = append(exceptions, "manifest.json", "journal/operations.jsonl")
	}
	serviceMaintenanceParityUnchanged(t, c, before, exceptions...)
}

func serviceMaintenanceAbsentCancellation(t *testing.T, row string) {
	t.Helper()
	c, d := serviceMaintenanceCell(t, true)
	h, _, _, _, _ := supervisorTestHooks()
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if s.load(d) != nil {
		t.Fatal("Cannot load absent cancellation supervisor")
	}
	if row == "running_health_canceled" {
		if s.step(t.Context()) != nil {
			t.Fatal("Cannot prepare absent running cancellation")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var events []string
	s.hooks.start = func(context.Context) error {
		events = append(events, "start")
		if row == "start_canceled" {
			cancel()
		}
		return nil
	}
	s.hooks.health = func(context.Context) error { events = append(events, "health"); cancel(); return nil }
	s.hooks.stop = func(context.Context) error { events = append(events, "stop"); return nil }
	if row == "pre_canceled" {
		cancel()
	}
	previousStatus, wantRecord := s.status, s.record
	before := serviceMaintenanceSnapshot(t, c)
	status, err := s.command(ctx, "health")
	var located *serviceFailure
	if !errors.Is(err, context.Canceled) || !errors.As(err, &located) || located.site != "control_cancelled" {
		t.Fatal("Absent cancellation cause or fixed site changed")
	}
	wantEvents := []string(nil)
	if row == "start_canceled" {
		wantEvents = []string{"start"}
		wantRecord.Attempts++
	}
	if row == "post_start_health_canceled" {
		wantEvents = []string{"start", "health"}
		wantRecord.Attempts++
	}
	if row == "running_health_canceled" {
		wantEvents = []string{"health"}
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatal("Absent cancellation hook order changed")
	}
	if !reflect.DeepEqual(status, previousStatus) || !reflect.DeepEqual(s.status, previousStatus) {
		t.Fatal("Absent cancellation changed completed status")
	}
	if !reflect.DeepEqual(s.record, wantRecord) {
		t.Fatal("Absent cancellation changed attempt or failure bookkeeping")
	}
	if row == "pre_canceled" || row == "running_health_canceled" {
		serviceMaintenanceParityUnchanged(t, c, before)
	} else {
		serviceMaintenanceParityFile(t, c, "service/supervisor.json", serviceMaintenanceParityJSON(t, wantRecord))
		serviceMaintenanceParityUnchanged(t, c, before, "service/supervisor.json")
	}
}

func serviceMaintenanceAbsentRecord(t *testing.T, row string) {
	t.Helper()
	c, d := serviceMaintenanceCell(t, true)
	h, now, starts, stops, _ := supervisorTestHooks()
	seed := supervisorRecord{Version: 1, RepairRevision: d.RepairRevision, Attempts: 3, Failures: []time.Time{*now}, NextStart: now.Add(h.initialBackoff)}
	want := seed
	if row == "version" {
		seed.Version = 2
	}
	if row == "attempts" {
		seed.Attempts = -1
	}
	if row == "failures" {
		seed.Failures = []time.Time{*now, *now, *now, *now, *now, *now}
	}
	if row == "repair_revision" {
		seed.RepairRevision++
		want = supervisorRecord{Version: 1, RepairRevision: d.RepairRevision}
	}
	if row != "missing" && writeAtomic(c.path("service", "supervisor.json"), seed, nil) != nil {
		t.Fatal("Cannot seed absent record fixture")
	}
	if row == "malformed" && testWriteFile(c.path("service", "supervisor.json"), []byte("{"), 0o600) != nil {
		t.Fatal("Cannot seed malformed absent record")
	}
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	before := serviceMaintenanceSnapshot(t, c)
	err := s.load(d)
	if row == "version" || row == "attempts" || row == "failures" || row == "malformed" {
		var located *serviceFailure
		if err == nil || !errors.As(err, &located) || located.site != "supervisor_load" {
			t.Fatal("Absent invalid record lost load error site")
		}
		if row != "malformed" && err.Error() != "localcell: invalid_supervisor_record" {
			t.Fatal("Absent invalid record changed fixed error bytes")
		}
		serviceMaintenanceParityUnchanged(t, c, before)
		return
	}
	if err != nil {
		t.Fatal("Absent accepted record failed load")
	}
	if row == "missing" {
		want = supervisorRecord{Version: 1}
	}
	if !reflect.DeepEqual(s.record, want) {
		t.Fatal("Absent load changed accepted record fields")
	}
	serviceMaintenanceParityUnchanged(t, c, before)
	if row != "save" && row != "traversal" {
		return
	}
	if row == "save" {
		if os.Remove(c.path("service", "supervisor.json")) != nil || testMkdir(c.path("service", "supervisor.json"), 0o700) != nil {
			t.Fatal("Cannot block absent record save")
		}
		s.record.NextStart = time.Time{}
	} else if os.Remove(c.path("service", "descriptor.json")) != nil {
		t.Fatal("Cannot block absent descriptor traversal")
	}
	before = serviceMaintenanceSnapshot(t, c)
	err = s.step(t.Context())
	var located *serviceFailure
	wantSite := "supervisor_save"
	if row == "traversal" {
		wantSite = "supervisor_traversal"
	}
	if err == nil || !errors.As(err, &located) || located.site != wantSite || s.status.LastStepOutcome != "failed" || s.status.Healthy {
		t.Fatal("Absent persistence failure site or completion changed")
	}
	if *starts != 0 || *stops != 0 {
		t.Fatal("Absent persistence failure invoked lifecycle hooks")
	}
	serviceMaintenanceParityUnchanged(t, c, before)
}
