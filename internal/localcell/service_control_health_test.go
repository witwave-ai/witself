package localcell

import (
	"context"
	"errors"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestServiceSupervisorControlHealthCancellation(t *testing.T) {
	for _, action := range []string{"health", "status"} {
		t.Run(action, func(t *testing.T) {
			c, _ := unitCell(t)
			d := prepareSupervisorTest(t, c)
			h, _, _, stops, _ := supervisorTestHooks()
			s := newSupervisor(c, SupervisorPolicy{hooks: &h})
			if err := s.load(d); err != nil || s.step(t.Context()) != nil {
				t.Fatal("cannot prepare running supervisor")
			}
			beforeStatus, beforeRecord := s.status, s.record
			beforeDescriptor, err := os.ReadFile(c.path("service", "descriptor.json"))
			if err != nil {
				t.Fatal("cannot snapshot synthetic descriptor")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered := make(chan struct{})
			s.hooks.health = func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			}
			stderr, err := os.CreateTemp(t.TempDir(), "cancel-receipt")
			if err != nil {
				t.Fatal("cannot capture cancellation receipts")
			}
			previous := os.Stderr
			os.Stderr = stderr
			t.Cleanup(func() { os.Stderr = previous; _ = stderr.Close() })
			done := StartJoinedTestRunForTest(t, cancel, serviceControlBudget(c.cfg.Timeout, 4), func() error {
				_, err := s.command(ctx, action)
				return err
			})
			<-entered
			cancel()
			err = <-done
			WriteServiceHostFailure(stderr, err, "supervisor_command")
			if !errors.Is(err, context.Canceled) {
				t.Error("control cancellation lost cancellation error identity")
			}
			var located *serviceFailure
			if !errors.As(err, &located) || located.site != "control_cancelled" {
				t.Error("control cancellation lost its fixed cancellation site")
			}
			if *stops != 0 {
				t.Error("control cancellation invoked stop hook")
			}
			if !reflect.DeepEqual(s.status, beforeStatus) {
				t.Error("control cancellation changed completed service status")
			}
			if !reflect.DeepEqual(s.record.Failures, beforeRecord.Failures) || s.record.NextStart != beforeRecord.NextStart || s.record.Attempts != beforeRecord.Attempts {
				t.Error("control cancellation changed failure or restart record")
			}
			afterDescriptor, readErr := os.ReadFile(c.path("service", "descriptor.json"))
			if readErr != nil || string(afterDescriptor) != string(beforeDescriptor) {
				t.Error("control cancellation changed or faulted durable descriptor")
			}
			data, readErr := os.ReadFile(stderr.Name())
			if readErr != nil || strings.Contains(string(data), "site=supervisor_stop") {
				t.Error("control cancellation emitted a stop receipt for an uninvoked hook")
			}
			if len(data) != 0 {
				t.Error("client control cancellation emitted a host failure receipt")
			}
			t.Logf("captured host failure bytes after client cancellation: %d", len(data))
		})
	}
}

func TestServiceSupervisorPreCanceledControlDoesNoWork(t *testing.T) {
	for _, action := range []string{"observe", "status", "health", "start", "stop", "repair"} {
		t.Run(action, func(t *testing.T) {
			c, _ := unitCell(t)
			d := prepareSupervisorTest(t, c)
			h, _, starts, stops, _ := supervisorTestHooks()
			s := newSupervisor(c, SupervisorPolicy{hooks: &h})
			if s.load(d) != nil {
				t.Fatal("cannot load synthetic supervisor")
			}
			before := supervisorSnapshotInventory(t, c.cfg.Root)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := s.command(ctx, action)
			if !errors.Is(err, context.Canceled) || *starts != 0 || *stops != 0 || !reflect.DeepEqual(before, supervisorSnapshotInventory(t, c.cfg.Root)) {
				t.Fatal("pre-canceled control command performed work")
			}
		})
	}
}

func TestServiceControlHealthBudget(t *testing.T) {
	for _, bound := range []time.Duration{time.Nanosecond, time.Millisecond, time.Second, time.Minute} {
		if serviceControlBudget(bound, 4) != 4*bound || serviceControlBudget(bound, 3) != 3*bound {
			t.Fatal("control budget lost proportional queue and response margin")
		}
	}
	const maximum = time.Duration(1<<63 - 1)
	if serviceControlBudget(maximum, 4) != maximum {
		t.Fatal("control budget overflowed")
	}
	ctx := context.WithValue(t.Context(), serviceControlHealthBoundKey{}, time.Second)
	if serviceControlHealthBound(ctx, 2*time.Second) != time.Second || serviceControlHealthBound(ctx, time.Millisecond) != time.Millisecond {
		t.Fatal("caller health bound extended the host bound or was ignored")
	}
}

func TestServiceSupervisorControlCancellationBeforeBookkeeping(t *testing.T) {
	for _, stage := range []string{"clamp", "start", "failure"} {
		t.Run(stage, func(t *testing.T) {
			c, _ := unitCell(t)
			d := prepareSupervisorTest(t, c)
			h, _, starts, stops, _ := supervisorTestHooks()
			s := newSupervisor(c, SupervisorPolicy{hooks: &h})
			if s.load(d) != nil {
				t.Fatal("cannot load cancellation bookkeeping fixture")
			}
			if stage == "clamp" {
				s.record.NextStart = time.Now().Add(time.Hour)
			}
			before := s.record
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			clocks := 0
			s.hooks.now = func() time.Time {
				clocks++
				if stage != "start" || clocks == 2 {
					cancel()
				}
				return time.Now()
			}
			var err error
			if stage == "failure" {
				err = s.failed(context.WithValue(ctx, serviceControlCommandKey{}, true))
			} else {
				_, err = s.command(ctx, "health")
			}
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(s.record, before) || *starts != 0 || *stops != 0 {
				t.Fatal("cancelled control command changed scheduling bookkeeping")
			}
		})
	}
}

func TestServiceSupervisorHealthBoundIsNotClientCancellation(t *testing.T) {
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	h, _, _, stops, _ := supervisorTestHooks()
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if s.load(d) != nil || s.step(t.Context()) != nil {
		t.Fatal("cannot start health bound fixture")
	}
	s.hooks.health = func(context.Context) error { return nil }
	start := time.Now()
	if _, err := s.command(t.Context(), "health"); err != nil {
		t.Fatal("cannot measure direct health command fixture")
	}
	measured := time.Since(start)
	hostBound := max(time.Millisecond, 20*measured)
	t.Logf("direct health calibration: measured=%s H=%s", measured, hostBound)
	s.hooks.health = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	// The host's own bound expires while the caller still has time. That is
	// a completed unhealthy verdict, not cancellation of the command.
	c.cfg.Timeout = hostBound
	ctx := context.WithValue(t.Context(), serviceControlHealthBoundKey{}, 1000*hostBound)
	_, err := s.command(ctx, "health")
	if ctx.Err() != nil || !errors.Is(err, errServiceUnhealthy) || *stops != 1 || len(s.record.Failures) != 1 || s.status.State != "backoff" {
		t.Fatal("bounded health failure was confused with client cancellation")
	}
}

// The request queues behind the first tick, then runs a second bounded hook.
// Both hooks consume three quarters of their own bound. An equal client budget
// expires before the second verdict even though each hook is within its bound.
func TestServiceControlSlowHealthVerdictActualHost(t *testing.T) {
	hostBound := serviceHealthMeasuredBound(t, time.Second)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	cell, manifest := unitCell(t)
	if runtime.GOOS != "windows" {
		root, err := os.MkdirTemp("/tmp", "lc-budget-")
		if err != nil {
			t.Fatal("cannot create short health fixture")
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		cell, err = New(Config{Root: root, Timeout: hostBound})
		if err != nil || cell.layout() != nil || writeAtomic(cell.path("manifest.json"), manifest, nil) != nil {
			t.Fatal("cannot prepare short health fixture")
		}
	}
	cell.cfg.Timeout = hostBound
	prepareSupervisorTest(t, cell)
	hooks, _, _, _, _ := supervisorTestHooks()
	if runtime.GOOS != "windows" {
		hooks.boot = func() (string, error) { return "synthetic-boot", nil }
	}
	hooks.ticks = make(chan time.Time)
	entered := make(chan struct{})
	var measured []time.Duration
	hooks.health = func(ctx context.Context) error {
		if len(measured) == 0 {
			close(entered)
		}
		start := time.Now()
		timer := time.NewTimer(3 * cell.cfg.Timeout / 4)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
		measured = append(measured, time.Since(start))
		if len(measured) == 1 {
			return nil
		}
		return errServiceHealth
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*cell.cfg.Timeout)
	defer cancel()
	done := StartJoinedTestRunForTest(t, cancel, serviceControlBudget(hostBound, 8), func() error {
		return cell.Run(ctx, SupervisorPolicy{hooks: &hooks, ShutdownTimeout: hostBound})
	})
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("health fixture did not enter first tick")
	}
	_, verdict := cell.Control(ctx, "health")
	cancel()
	select {
	case hostErr := <-done:
		if hostErr != nil {
			t.Error("health fixture host shutdown failed")
		}
	case <-time.After(8 * cell.cfg.Timeout):
		t.Fatal("health fixture host did not finish")
	}
	if !errors.Is(verdict, errServiceUnhealthy) || verdict.Error() != "localcell: service_unhealthy" {
		t.Fatal("bounded health verdict was hidden by control transport deadline")
	}
	if len(measured) != 2 {
		t.Fatal("health verdict did not follow both measured bounded hooks")
	}
	for _, elapsed := range measured {
		if elapsed < 3*cell.cfg.Timeout/4 || elapsed >= cell.cfg.Timeout {
			t.Fatal("slow health fixture did not finish within its own bound")
		}
	}
}

// Serve the real authenticated transport while keeping supervisor execution
// explicit, so assertions can inspect state before shutdown invokes its stop.
func serviceHealthControlFixture(t *testing.T, bound time.Duration) (*Cell, chan serviceCommand) {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	cell, manifest := unitCell(t)
	if runtime.GOOS != "windows" {
		root, err := os.MkdirTemp("/tmp", "lc-yield-")
		if err != nil {
			t.Fatal("cannot create short control health fixture")
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		cell, err = New(Config{Root: root, Timeout: bound})
		if err != nil || cell.layout() != nil || writeAtomic(cell.path("manifest.json"), manifest, nil) != nil {
			t.Fatal("cannot prepare control health fixture")
		}
	}
	cell.cfg.Timeout = bound
	d := prepareSupervisorTest(t, cell)
	boot := serviceBootID
	if runtime.GOOS != "windows" {
		boot = func() (string, error) { return "synthetic-boot", nil }
	}
	lease, err := cell.acquireServiceLeaseWithBoot(d, boot)
	if err != nil {
		t.Fatal("cannot hold control fixture lease")
	}
	t.Cleanup(func() { _ = lease.Close() })
	commands := make(chan serviceCommand)
	closeControl, err := cell.serveServiceControl(commands)
	if err != nil {
		t.Fatal("cannot serve control health fixture")
	}
	t.Cleanup(closeControl)
	return cell, commands
}

// Calibrate each timing fixture with a complete authenticated Control request
// and the same supervisor health hook seam used by its measured scenario. The
// calibration cell remains separate so no active server observes a timeout
// mutation; its declared floor is only the bootstrap bound for this no-op.
func serviceHealthMeasuredBound(t *testing.T, floor time.Duration) time.Duration {
	t.Helper()
	cell, commands := serviceHealthControlFixture(t, floor)
	d, err := cell.ServiceDescriptor()
	h, _, _, _, _ := supervisorTestHooks()
	h.health = func(context.Context) error { return nil }
	s := newSupervisor(cell, SupervisorPolicy{hooks: &h})
	if err != nil || s.load(d) != nil || s.step(t.Context()) != nil {
		t.Fatal("cannot prepare health round trip calibration")
	}
	healthCalls := 0
	s.hooks.health = func(context.Context) error { healthCalls++; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := StartJoinedTestRunForTest(t, cancel, serviceControlBudget(floor, 4), func() error {
		select {
		case command := <-commands:
			status, commandErr := s.command(command.ctx, command.action)
			command.reply <- serviceReply{status: status, err: commandErr}
		case <-ctx.Done():
		}
		return nil
	})
	start := time.Now()
	_, err = cell.Control(ctx, "health")
	measured := time.Since(start)
	cancel()
	<-done
	if err != nil || healthCalls != 1 {
		t.Fatal("cannot measure no-op health round trip")
	}
	bound := max(floor, 20*measured)
	t.Logf("health round trip calibration: measured=%s H=%s", measured, bound)
	return bound
}

func TestServiceControlCallerBoundYieldsActualHost(t *testing.T) {
	for _, tighter := range []bool{true, false} {
		name := "host_bound_verdict"
		if tighter {
			name = "caller_bound_yields"
		}
		t.Run(name, func(t *testing.T) {
			floor := serviceHealthVerdictFloor
			if tighter {
				floor = 2 * time.Second
			}
			hostBound := serviceHealthMeasuredBound(t, floor)
			if hostBound < serviceHealthVerdictFloor {
				t.Fatalf("host bound below floor host_bound_ms=%d floor_ms=%d", hostBound.Milliseconds(), serviceHealthVerdictFloor.Milliseconds())
			}
			callerBound := 10 * hostBound
			if tighter {
				callerBound = hostBound / 10
			}
			cell, commands := serviceHealthControlFixture(t, hostBound)
			client := &Cell{cfg: cell.cfg}
			client.cfg.Timeout = callerBound
			d, err := cell.ServiceDescriptor()
			h, _, _, stops, _ := supervisorTestHooks()
			s := newSupervisor(cell, SupervisorPolicy{hooks: &h})
			if err != nil || s.load(d) != nil || s.step(t.Context()) != nil {
				t.Fatal("cannot start caller bound fixture")
			}
			beforeStatus, beforeRecord := s.status, s.record
			beforeDescriptor, err := os.ReadFile(cell.path("service", "descriptor.json"))
			if err != nil {
				t.Fatal("cannot snapshot caller bound descriptor")
			}
			beforeRecordBytes, err := os.ReadFile(cell.path("service", "supervisor.json"))
			if err != nil {
				t.Fatal("cannot snapshot caller bound record")
			}
			var elapsed time.Duration
			s.hooks.health = func(ctx context.Context) error {
				serviceHealthStep(ctx, "health_database")
				start := time.Now()
				if tighter {
					// A healthy, context-unaware substep finishes after the caller
					// gives up but comfortably inside the host's tolerance.
					time.Sleep(2 * callerBound)
					elapsed = time.Since(start)
					return nil
				}
				<-ctx.Done()
				elapsed = time.Since(start)
				return ctx.Err()
			}
			stderr, err := os.CreateTemp(t.TempDir(), "bound-receipt")
			if err != nil {
				t.Fatal("cannot capture bound receipt")
			}
			previous := os.Stderr
			os.Stderr = stderr
			t.Cleanup(func() { os.Stderr = previous; _ = stderr.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 8*max(hostBound, callerBound))
			defer cancel()
			done := StartJoinedTestRunForTest(t, cancel, serviceControlBudget(max(hostBound, callerBound), 8), func() error {
				select {
				case command := <-commands:
					status, commandErr := s.command(command.ctx, command.action)
					WriteServiceHostFailure(stderr, commandErr, "supervisor_command")
					command.reply <- serviceReply{status: status, err: commandErr}
				case <-ctx.Done():
				}
				return nil
			})
			_, verdict := client.Control(ctx, "health")
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("caller bound fixture did not finish its command")
			}
			if tighter {
				if !errors.Is(verdict, errServiceControlDeferred) || verdict.Error() != "localcell: service_control_deferred" {
					t.Error("caller bound expiry became an unhealthy verdict instead of deferred")
				}
				if *stops != 0 || !s.running || !reflect.DeepEqual(s.status, beforeStatus) || !reflect.DeepEqual(s.record, beforeRecord) {
					t.Error("caller bound expiry stopped or changed the healthy supervisor")
				}
				afterDescriptor, descriptorErr := os.ReadFile(cell.path("service", "descriptor.json"))
				afterRecord, recordErr := os.ReadFile(cell.path("service", "supervisor.json"))
				if descriptorErr != nil || recordErr != nil || string(afterDescriptor) != string(beforeDescriptor) || string(afterRecord) != string(beforeRecordBytes) {
					t.Error("caller bound expiry changed durable descriptor or failure record")
				}
				data, readErr := os.ReadFile(stderr.Name())
				if readErr != nil || len(data) != 0 {
					t.Error("caller bound yield emitted a host failure receipt")
				}
				if elapsed <= callerBound || elapsed >= hostBound {
					t.Error("healthy hook did not complete between caller and host bounds")
				}
			} else {
				observed := serviceHealthVerdictObservation{
					verdict: verdict, stops: *stops, running: s.running,
					failures: len(s.record.Failures), nextStartZero: s.record.NextStart.IsZero(),
					state: s.status.State, elapsed: elapsed, hostBound: hostBound,
				}
				fields := serviceHealthVerdictFields(observed)
				if len(fields) > 0 {
					t.Error(serviceHealthVerdictReport(fields, observed))
				} else {
					// A wrong-state copy must exercise the same collector and report.
					// The valid host result alone cannot detect a missing state check.
					observed.state = ""
					if fields := serviceHealthVerdictFields(observed); !reflect.DeepEqual(fields, []string{"state"}) || !strings.HasPrefix(serviceHealthVerdictReport(fields, observed), "host bound expiry failed fields=state ") {
						t.Error("host bound diagnostic lost state check")
					}
				}
			}
		})
	}
}

func TestServiceControlNoVerdictClassification(t *testing.T) {
	for _, stage := range []string{"queue", "decision", "cancelled_reply", "unhealthy", "other_failure"} {
		t.Run(stage, func(t *testing.T) {
			hostBound := serviceHealthMeasuredBound(t, time.Second)
			cell, commands := serviceHealthControlFixture(t, hostBound)
			client := &Cell{cfg: cell.cfg}
			client.cfg.Timeout = hostBound / 4
			ctx, cancel := context.WithTimeout(t.Context(), 8*hostBound)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				if stage == "queue" {
					<-ctx.Done()
					return
				}
				select {
				case command := <-commands:
					var err error
					switch stage {
					case "decision":
						<-command.ctx.Done()
						err = command.ctx.Err()
					case "cancelled_reply":
						err = serviceFailureAt(context.Canceled, "control_cancelled")
					case "unhealthy":
						err = errServiceUnhealthy
					case "other_failure":
						err = ErrServiceControlRefused
					}
					command.reply <- serviceReply{err: err}
				case <-ctx.Done():
				}
			}()
			_, err := client.Control(ctx, "health")
			cancel()
			<-done
			switch stage {
			case "unhealthy":
				if !errors.Is(err, errServiceUnhealthy) {
					t.Fatal("computed unhealthy verdict lost its classification")
				}
			case "other_failure":
				if err == nil || err.Error() != "localcell: service_control_action_failed" {
					t.Fatal("non-verdict failure was classified as unhealthy")
				}
			default:
				if !errors.Is(err, errServiceControlDeferred) || err.Error() != "localcell: service_control_deferred" {
					t.Fatal("no-verdict expiry was not classified as deferred")
				}
			}
		})
	}
}

func TestServiceControlBusyHealthIsDeferred(t *testing.T) {
	hostBound := serviceHealthMeasuredBound(t, time.Second)
	cell, commands := serviceHealthControlFixture(t, hostBound)
	d, err := cell.ServiceDescriptor()
	h, _, _, stops, _ := supervisorTestHooks()
	s := newSupervisor(cell, SupervisorPolicy{hooks: &h})
	if err != nil || s.load(d) != nil || s.step(t.Context()) != nil {
		t.Fatal("cannot start contention fixture")
	}
	beforeRecord := s.record
	s.hooks.health = func(context.Context) error { return ErrLocked }
	ctx, cancel := context.WithTimeout(t.Context(), 8*hostBound)
	defer cancel()
	done := StartJoinedTestRunForTest(t, cancel, serviceControlBudget(hostBound, 4), func() error {
		select {
		case command := <-commands:
			status, commandErr := s.command(command.ctx, command.action)
			command.reply <- serviceReply{status: status, err: commandErr}
		case <-ctx.Done():
		}
		return nil
	})
	_, verdict := cell.Control(ctx, "health")
	cancel()
	<-done
	if !errors.Is(verdict, errServiceControlDeferred) || *stops != 0 || !s.running || !reflect.DeepEqual(s.record, beforeRecord) || s.status.State != "busy" {
		t.Fatal("health contention without a verdict was not deferred")
	}
}
