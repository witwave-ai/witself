package localcell

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestServiceSupervisorObserveUsesLastSnapshotWithoutHooks(t *testing.T) {
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	h, _, starts, stops, _ := supervisorTestHooks()
	healths, repairs, reconciles := 0, 0, 0
	h.health = func(context.Context) error { healths++; return nil }
	h.repair = func(context.Context) error { repairs++; return nil }
	h.reconcile = func() error { reconciles++; return nil }
	known := ProcessRecord{PID: 123, StartTime: "synthetic-start", Root: d.Root, Component: "server"}
	if err := writeAtomic(c.path("run", "server.json"), known, nil); err != nil {
		t.Fatal("cannot prepare known identity")
	}
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if err := s.load(d); err != nil {
		t.Fatal(err)
	}
	before := supervisorSnapshotInventory(t, c.cfg.Root)
	status, err := s.command(t.Context(), "observe")
	if err != nil || status.Healthy || status.DesiredState != "running" || status.Generation != d.Payload.Generation || status.LastStepOutcome != "not_run" || len(status.Processes) != 1 || status.Processes[0].StartTime != known.StartTime {
		t.Fatal("observe did not return the initial known snapshot")
	}
	if *starts != 0 || *stops != 0 || healths != 0 || repairs != 0 || reconciles != 0 || s.record.Attempts != 0 {
		t.Fatal("observe invoked a lifecycle hook or changed supervisor attempts")
	}
	if !reflect.DeepEqual(before, supervisorSnapshotInventory(t, c.cfg.Root)) {
		t.Fatal("observe changed the installation")
	}
	status.Processes[0].StartTime = "caller-modified"
	if s.status.Processes[0].StartTime != known.StartTime {
		t.Fatal("observe returned mutable supervisor identity storage")
	}
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.path("run", "server.json")); err != nil {
		t.Fatal("cannot remove synthetic process record")
	}
	before = supervisorSnapshotInventory(t, c.cfg.Root)
	status, err = s.command(t.Context(), "observe")
	if err != nil || !status.Healthy || status.LastStepOutcome != "ok" || len(status.Processes) != 1 || *starts != 1 || *stops != 0 || healths != 1 || repairs != 0 || reconciles != 0 {
		t.Fatal("observe refreshed or changed the last completed step")
	}
	if !reflect.DeepEqual(before, supervisorSnapshotInventory(t, c.cfg.Root)) {
		t.Fatal("completed-step observation changed the installation")
	}
	// An error in the next ordinary step replaces the successful health result.
	if err := os.Remove(c.path("service", "descriptor.json")); err != nil {
		t.Fatal("cannot remove synthetic descriptor")
	}
	if err := s.step(t.Context()); err == nil {
		t.Fatal("missing descriptor did not fail a normal step")
	}
	status, err = s.command(t.Context(), "observe")
	if err != nil || status.Healthy || status.LastStepOutcome != "failed" {
		t.Fatal("observe concealed the last failed step")
	}
}

func supervisorSnapshotInventory(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			files[path] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		if err == nil {
			files[path] = string(data)
		}
		return err
	})
	if err != nil {
		t.Fatal("cannot inventory synthetic supervisor files")
	}
	return files
}

func TestServiceSupervisorStatusAndHealthKeepStartingBehavior(t *testing.T) {
	for _, action := range []string{"status", "health"} {
		t.Run(action, func(t *testing.T) {
			c, _ := unitCell(t)
			d := prepareSupervisorTest(t, c)
			h, _, starts, _, _ := supervisorTestHooks()
			s := newSupervisor(c, SupervisorPolicy{hooks: &h})
			if err := s.load(d); err != nil {
				t.Fatal(err)
			}
			status, err := s.command(t.Context(), action)
			if err != nil || !status.Healthy || *starts != 1 || status.LastStepOutcome != "ok" {
				t.Fatal("existing status or health action stopped performing a step")
			}
		})
	}
}

func prepareSupervisorTest(t *testing.T, c *Cell) ServiceDescriptor {
	t.Helper()
	t.Setenv("WITSELF_RUNTIME", "local")
	host, err := os.Executable()
	if err != nil {
		t.Fatal("fixture executable unavailable")
	}
	if err := c.PrepareService(t.Context(), ServiceDescriptor{Adapter: "foreground", Payload: ServicePayload{Host: host, Args: []string{"run", "--root", c.cfg.Root}}}); err != nil {
		t.Fatal(err)
	}
	d, err := c.ServiceDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func supervisorTestHooks() (supervisorHooks, *time.Time, *int, *int, *bool) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	starts, stops := 0, 0
	healthy := true
	h := supervisorHooks{
		start:  func(context.Context) error { starts++; return nil },
		stop:   func(context.Context) error { stops++; return nil },
		repair: func(context.Context) error { return nil }, reconcile: func() error { return nil },
		health: func(context.Context) error {
			if healthy {
				return nil
			}
			return errors.New("synthetic unhealthy")
		},
		now: func() time.Time { return now }, initialBackoff: time.Millisecond, maxBackoff: 60 * time.Millisecond, failureWindow: 600 * time.Millisecond,
	}
	return h, &now, &starts, &stops, &healthy
}

func TestServiceSupervisorBackoffCapAndDurableCircuit(t *testing.T) {
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	h, now, starts, _, healthy := supervisorTestHooks()
	*healthy = false
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if err := s.load(d); err != nil {
		t.Fatal(err)
	}
	for failure := 1; failure <= 5; failure++ {
		if err := s.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if *starts != failure {
			t.Fatal("one failure launched more than one owned tree")
		}
		delay := s.record.NextStart.Sub(*now)
		if delay != time.Millisecond*time.Duration(1<<(failure-1)) {
			t.Fatal("restart delay is not exponential")
		}
		if err := s.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if *starts != failure {
			t.Fatal("backoff did not suppress immediate restart")
		}
		*now = s.record.NextStart
	}
	d, err := c.ServiceDescriptor()
	if err != nil || !d.Faulted || s.status.State != "faulted" {
		t.Fatal("five failures did not durably open circuit")
	}
	*now = now.Add(time.Hour)
	restarted := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if err := restarted.load(d); err != nil {
		t.Fatal(err)
	}
	if err := restarted.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *starts != 5 {
		t.Fatal("host restart or elapsed failure window bypassed explicit repair")
	}
	for _, failures := range []int{7, 8, 63, 1000} {
		if supervisorBackoff(failures, time.Second, time.Minute) != time.Minute {
			t.Fatal("backoff exceeded sixty-second cap")
		}
	}
	if err := c.RepairService(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.SetServiceDesiredState(t.Context(), "running"); err != nil {
		t.Fatal(err)
	}
	*healthy = true
	if err := restarted.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !restarted.status.Healthy || len(restarted.record.Failures) != 0 {
		t.Fatal("explicit repair did not reset the durable failure budget")
	}
}

func TestServiceSupervisorStopAndMaintenanceSurviveHostCrash(t *testing.T) {
	for _, desired := range []string{"stopped", "maintenance"} {
		t.Run(desired, func(t *testing.T) {
			c, _ := unitCell(t)
			d := prepareSupervisorTest(t, c)
			h, _, starts, stops, _ := supervisorTestHooks()
			s := newSupervisor(c, SupervisorPolicy{hooks: &h})
			if err := s.load(d); err != nil {
				t.Fatal(err)
			}
			if err := s.step(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := c.SetServiceDesiredState(t.Context(), desired); err != nil {
				t.Fatal(err)
			}
			if err := s.step(t.Context()); err != nil {
				t.Fatal(err)
			}
			if *stops != 1 || *starts != 1 {
				t.Fatal("desired fence did not stop the complete owned tree once")
			}
			d, err := c.ServiceDescriptor()
			if err != nil {
				t.Fatal(err)
			}
			crashed := newSupervisor(c, SupervisorPolicy{hooks: &h})
			if err := crashed.load(d); err != nil {
				t.Fatal(err)
			}
			if err := crashed.step(t.Context()); err != nil {
				t.Fatal(err)
			}
			if *starts != 1 || crashed.status.Healthy || crashed.status.State != desired {
				t.Fatal("new host ignored durable desired state")
			}
		})
	}
}

func TestServiceSupervisorChildFailureRestartsOneTree(t *testing.T) {
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	h, now, starts, stops, healthy := supervisorTestHooks()
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if err := s.load(d); err != nil {
		t.Fatal(err)
	}
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	*healthy = false
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *starts != 1 || *stops != 1 || len(s.record.Failures) != 1 {
		t.Fatal("child failure did not stop one whole owned tree")
	}
	*healthy = true
	*now = s.record.NextStart
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *starts != 2 || *stops != 1 || !s.status.Healthy {
		t.Fatal("child failure created competing restarts")
	}
}

func TestServiceSupervisorFailureWindowAndRestartRecordValidation(t *testing.T) {
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	h, now, _, _, healthy := supervisorTestHooks()
	*healthy = false
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if err := s.load(d); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if err := s.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(h.failureWindow + time.Millisecond)
	}
	if len(s.record.Failures) != 1 {
		t.Fatal("expired failures incorrectly opened circuit")
	}
	if err := writeAtomic(c.path("service", "supervisor.json"), supervisorRecord{Version: 99}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.load(d); err == nil {
		t.Fatal("unknown durable restart record accepted")
	}
}

func TestServiceSupervisorCrashBeforeFaultDescriptorRemainsFenced(t *testing.T) {
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	h, now, starts, _, _ := supervisorTestHooks()
	record := supervisorRecord{Version: 1, RepairRevision: d.RepairRevision, Attempts: 5}
	for range 5 {
		record.Failures = append(record.Failures, *now)
	}
	if err := writeAtomic(c.path("service", "supervisor.json"), record, nil); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if err := s.load(d); err != nil {
		t.Fatal(err)
	}
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *starts != 0 || s.status.State != "faulted" {
		t.Fatal("interruption between failure receipt and descriptor bypassed circuit")
	}
}

func TestServiceSupervisorOperationContentionDoesNotTripCircuit(t *testing.T) {
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	h, _, starts, _, healthy := supervisorTestHooks()
	start := h.start
	h.start = func(context.Context) error { return ErrLocked }
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if err := s.load(d); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if err := s.step(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if s.record.Attempts != 0 || len(s.record.Failures) != 0 || *starts != 0 {
		t.Fatal("operation contention consumed the failure budget")
	}
	s.hooks.start = start
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	health := s.hooks.health
	s.hooks.health = func(context.Context) error { return ErrLocked }
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(s.record.Failures) != 0 || !s.running {
		t.Fatal("busy readiness was treated as a child failure")
	}
	s.hooks.health = health
	*healthy = false
	stop := s.hooks.stop
	s.hooks.stop = func(context.Context) error { return ErrLocked }
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(s.record.Failures) != 0 || !s.running {
		t.Fatal("busy stop was treated as a child failure")
	}
	s.hooks.stop = stop
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(s.record.Failures) != 1 || s.running {
		t.Fatal("released operation did not converge to one failure")
	}
}

func TestServiceSupervisorControlStopContentionRetriesOwnedTree(t *testing.T) {
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	h, _, _, stops, _ := supervisorTestHooks()
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if err := s.load(d); err != nil {
		t.Fatal(err)
	}
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	stop := s.hooks.stop
	s.hooks.stop = func(context.Context) error { return ErrLocked }
	if _, err := s.command(t.Context(), "stop"); !errors.Is(err, ErrLocked) {
		t.Fatal("contended control stop was acknowledged")
	}
	d, err := c.ServiceDescriptor()
	if err != nil || d.DesiredState != "stopped" || !s.running {
		t.Fatal("failed stop lost either durable fence or pending shutdown")
	}
	s.hooks.stop = stop
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *stops != 1 || s.running || s.status.Healthy {
		t.Fatal("next stopped-state tick did not retry owned-tree shutdown")
	}
}

func TestServiceSupervisorClockRollbackCapsPersistedRetry(t *testing.T) {
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	h, now, starts, _, _ := supervisorTestHooks()
	record := supervisorRecord{Version: 1, RepairRevision: d.RepairRevision, Attempts: 1, NextStart: now.Add(time.Hour)}
	if err := writeAtomic(c.path("service", "supervisor.json"), record, nil); err != nil {
		t.Fatal(err)
	}
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if err := s.load(d); err != nil {
		t.Fatal(err)
	}
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *starts != 0 || s.record.NextStart.Sub(*now) != h.maxBackoff {
		t.Fatal("wall-clock rollback made persisted retry exceed the cap")
	}
	var persisted supervisorRecord
	if err := readJSON(c.path("service", "supervisor.json"), &persisted); err != nil {
		t.Fatal(err)
	}
	if !persisted.NextStart.Equal(s.record.NextStart) {
		t.Fatal("capped retry deadline was not durable")
	}
	*now = s.record.NextStart
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *starts != 1 || !s.status.Healthy {
		t.Fatal("capped retry did not resume one tree")
	}
}
