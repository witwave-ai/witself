package localcell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"time"
)

var (
	// ErrServiceHostActive refuses a second lifetime owner for the same root.
	ErrServiceHostActive = errors.New("localcell: service_host_already_active")
	// ErrServiceControlRefused is the fixed authentication and privacy refusal.
	ErrServiceControlRefused = errors.New("localcell: service_control_refused")
	errServiceUnhealthy      = errors.New("localcell: service_unhealthy")
)

// ServiceStatus deliberately contains no process arguments, paths or credentials.
type ServiceStatus struct {
	DesiredState    string                   `json:"desired_state"`
	State           string                   `json:"state"`
	Reason          string                   `json:"reason,omitempty"`
	Healthy         bool                     `json:"healthy"`
	Restarts        int                      `json:"restarts"`
	Generation      string                   `json:"generation"`
	HostExit        string                   `json:"host_exit,omitempty"`
	LastStepOutcome string                   `json:"last_step_outcome"`
	Processes       []ServiceProcessIdentity `json:"processes,omitempty"`
}

// ServiceProcessIdentity is a known ownership fence, not a fresh liveness probe.
type ServiceProcessIdentity struct {
	Component string `json:"component"`
	PID       int    `json:"pid"`
	StartTime string `json:"start_time"`
}

// SupervisorPolicy bounds graceful host shutdown. The restart policy is fixed:
// one to sixty seconds, and five failures within ten minutes require repair.
type SupervisorPolicy struct {
	ShutdownTimeout time.Duration
	hooks           *supervisorHooks
}

type supervisorHooks struct {
	start, stop, repair, health               func(context.Context) error
	reconcile                                 func() error
	boot                                      func() (string, error)
	now                                       func() time.Time
	shutdownContext                           func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	ticks                                     <-chan time.Time
	initialBackoff, maxBackoff, failureWindow time.Duration
	windowsShutdownPolicy                     bool
}

type supervisorRecord struct {
	Version        int         `json:"version"`
	RepairRevision uint64      `json:"repair_revision"`
	Failures       []time.Time `json:"failures"`
	NextStart      time.Time   `json:"next_start"`
	Attempts       int         `json:"attempts"`
}

type serviceCommand struct {
	ctx       context.Context
	action    string
	reply     chan serviceReply
	exit      *serviceHostExitEnvelope
	delivered <-chan struct{}
	completed chan string
	finalized <-chan struct{}
}

type serviceReply struct {
	status ServiceStatus
	err    error
}

type supervisor struct {
	cell                 *Cell
	hooks                supervisorHooks
	record               supervisorRecord
	status               ServiceStatus
	running              bool
	phase, operation     string
	healthProgress       serviceHealthProgress
	exitDrained          bool
	exitDrainResult      error
	shutdownBlocked      bool
	shutdownFaultPending bool
	maintenanceHeld      bool
	maintenanceDisplay   bool
	maintenanceBusy      bool
	pendingStartup       serviceStartupPhase
	startupFaulted       bool
	completingStop       bool
}

// Run hosts exactly one owned tree. The lifetime lease is distinct from the
// operation lock, so independent Stop and maintenance transitions remain usable.
// Cancellation stops children while retaining desired state for the next login.
func (c *Cell) Run(ctx context.Context, policy SupervisorPolicy) (result error) {
	if os.Getenv("WITSELF_RUNTIME") != "local" {
		return errors.New("localcell: service_requires_local_runtime")
	}
	if err := serviceControlCapability(); err != nil {
		return err
	}
	d, err := c.ServiceDescriptor()
	if err != nil {
		return serviceFailureAt(err, "initial_descriptor")
	}
	if err := validateServiceHost(d); err != nil {
		return err
	}
	s := newSupervisor(c, policy)
	acquireLease := c.acquireServiceLease
	if policy.hooks != nil {
		acquireLease = func(d ServiceDescriptor) (*operationLock, error) {
			return c.acquireServiceLeaseWithBoot(d, s.hooks.boot)
		}
	}
	lease, err := acquireLease(d)
	if err != nil {
		return err
	}
	defer func() { _ = lease.Close() }()
	commands := make(chan serviceCommand)
	closeControl, err := c.serveServiceControl(commands)
	if err != nil {
		return serviceFailureFallback(err, "control_transport")
	}
	defer closeControl()
	if err := s.load(d); err != nil {
		return err
	}
	shutdown := policy.ShutdownTimeout
	maximumShutdown := 30 * time.Second
	if runtime.GOOS == "windows" {
		maximumShutdown = ServiceShutdownTimeout
	}
	if shutdown <= 0 || shutdown > maximumShutdown {
		shutdown = maximumShutdown
	}
	defer func() {
		if s.exitDrained || s.shutdownBlocked {
			return
		}
		if ctx.Err() != nil {
			progress, journalErr := c.readJournal()
			// Only fixed supervisor labels and journal shape cross stderr.
			if s.operation == "health" {
				fmt.Fprintf(os.Stderr, "service_shutdown phase=%s operation=health site=%s journal_pending=%t journal_read_ok=%t\n",
					s.phase, serviceHealthSite(s.healthProgress.site), progress.pending != nil || progress.tornTail, journalErr == nil)
			} else {
				fmt.Fprintf(os.Stderr, "service_shutdown phase=%s operation=%s journal_pending=%t journal_read_ok=%t\n",
					s.phase, s.operation, progress.pending != nil || progress.tornTail, journalErr == nil)
			}
			if errors.Is(result, ctx.Err()) {
				result = nil
			}
		}
		stopCtx, cancel := s.hooks.shutdownContext(context.Background(), shutdown)
		defer cancel()
		// A canceled lifecycle operation may have left a pending journal entry.
		// Settle it before Stop attempts to begin another operation, retaining
		// the lifetime lease and the same bounded, signal-independent context.
		repairErr := retryServiceContention(stopCtx, s.hooks.repair)
		stopErr := retryServiceContention(stopCtx, s.hooks.stop)
		if serviceMaintenanceOnly(repairErr) {
			repairErr = nil
		}
		result = errors.Join(result, serviceFailureAt(repairErr, "shutdown_repair"), serviceFailureAt(stopErr, "shutdown_stop"))
	}()
	s.phase = "startup"
	s.pendingStartup = serviceStartupReconcile
	if err := retryServiceContention(ctx, s.resumeServiceStartup); err != nil {
		return err
	}
	var ticker *time.Ticker
	ticks := s.hooks.ticks
	if ticks == nil {
		ticker = time.NewTicker(time.Second)
		defer ticker.Stop()
		ticks = ticker.C
	}
	if err := s.tick(ctx); err != nil {
		return err
	}
	for {
		runDone := ctx.Done()
		select {
		case <-runDone:
			return nil
		case <-ticks:
			if err := s.tick(ctx); err != nil {
				return err
			}
		case command := <-commands:
			if ctx.Err() != nil {
				return nil
			}
			s.phase = "control"
			commandCtx, cancel := context.WithCancel(command.ctx)
			commandCtx = context.WithValue(commandCtx, serviceControlHostContextKey{}, ctx)
			if command.exit != nil {
				commandCtx = context.WithValue(commandCtx, serviceHostExitKey{}, *command.exit)
			}
			go func() {
				select {
				case <-runDone:
					cancel()
				case <-commandCtx.Done():
				}
			}()
			status, err := s.command(commandCtx, command.action)
			cancel()
			WriteServiceHostFailure(os.Stderr, err, "supervisor_command")
			command.reply <- serviceReply{status: status, err: err}
			if command.action == "exit-host" && err == nil {
				exited := s.finishHostExitCommand(ctx, command)
				if command.completed != nil {
					disposition := "refused"
					if exited {
						disposition = "drained"
						if s.exitDrainResult != nil {
							disposition = "forced"
						}
					}
					command.completed <- disposition
					select {
					case <-command.finalized:
					case <-command.ctx.Done():
					}
				}
				cancel()
				if exited {
					return s.exitDrainResult
				}
			}
			if ctx.Err() == nil {
				s.phase, s.operation = "idle", "none"
			}
		}
	}
}

// Operation contention is not a child failure. The lifetime lease remains
// held while the bounded operation lock belongs to a separate lifecycle call.
func retryServiceContention(ctx context.Context, operation func(context.Context) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := operation(ctx); !errors.Is(err, ErrLocked) {
			return err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func newSupervisor(c *Cell, policy SupervisorPolicy) *supervisor {
	h := supervisorHooks{start: c.Start, stop: c.stopServiceChildren, repair: c.Repair, health: c.ServiceHealth,
		reconcile: c.reconcileServiceBoot, boot: serviceBootID, now: time.Now, initialBackoff: time.Second, maxBackoff: time.Minute, failureWindow: 10 * time.Minute}
	if policy.hooks != nil {
		h = *policy.hooks
		if h.boot == nil {
			h.boot = serviceBootID
		}
	}
	if h.shutdownContext == nil {
		h.shutdownContext = context.WithTimeout
	}
	h.windowsShutdownPolicy = h.windowsShutdownPolicy || runtime.GOOS == "windows"
	return &supervisor{cell: c, hooks: h, phase: "startup", operation: "none"}
}

func (s *supervisor) load(d ServiceDescriptor) (result error) {
	defer func() { result = serviceFailureFallback(result, "supervisor_load") }()
	if err := servicePrivateRecord(s.cell.path("service", "supervisor.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := readJSON(s.cell.path("service", "supervisor.json"), &s.record); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if s.record.Version != 0 && s.record.Version != 1 {
		return errors.New("localcell: invalid_supervisor_record")
	}
	if s.record.Attempts < 0 || len(s.record.Failures) > 5 {
		return errors.New("localcell: invalid_supervisor_record")
	}
	if s.record.RepairRevision != d.RepairRevision {
		s.record = supervisorRecord{RepairRevision: d.RepairRevision}
	}
	s.record.Version = 1
	s.status = ServiceStatus{DesiredState: d.DesiredState, State: d.DesiredState, Generation: d.Payload.Generation, LastStepOutcome: "not_run"}
	s.startupFaulted = d.Faulted || len(s.record.Failures) >= 5
	s.rememberProcesses(d)
	return nil
}

func (s *supervisor) save() error {
	return serviceFailureAt(writeAtomic(s.cell.path("service", "supervisor.json"), s.record, nil), "supervisor_save")
}

// tick checks cancellation before any descriptor reads, bookkeeping or hooks.
// A ready ticker must not win a select against cancellation and begin new work.
func (s *supervisor) tick(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.phase = "tick"
	if s.pendingStartup != serviceStartupNone {
		held, err := s.observeServiceMaintenance(ctx)
		if errors.Is(err, ErrLocked) {
			s.maintenanceDisplay = false
			return s.busy()
		}
		if err != nil {
			return err
		}
		if !held || s.pendingStartup == serviceStartupStop {
			if err := s.resumeServiceStartup(ctx); err != nil {
				if errors.Is(err, ErrLocked) {
					s.maintenanceDisplay = false
					return s.busy()
				}
				return err
			}
		}
	}
	err := s.step(ctx)
	if ctx.Err() == nil {
		s.phase, s.operation = "idle", "none"
	}
	return err
}

// call also fences transitions between hooks within a tick. Retaining the
// active label on cancellation identifies the interrupted phase at shutdown.
func (s *supervisor) call(ctx context.Context, operation string, hook func(context.Context) error) (result error) {
	// No operation receipt may claim a hook which never ran.
	if err := ctx.Err(); err != nil {
		if cancellation := serviceControlCancellation(ctx); cancellation != nil {
			return cancellation
		}
		return err
	}
	defer func() {
		result = serviceFailureOperation(result, operation)
		// Some failed operations enter backoff instead of exiting the host.
		// Preserve their fixed site before that error is reduced to status.
		WriteServiceHostFailure(os.Stderr, result, "unknown")
	}()
	s.operation = operation
	hookCtx := ctx
	if operation == "health" {
		s.healthProgress.site = "health_lock"
		var cancel context.CancelFunc
		hookCtx, cancel = context.WithTimeout(ctx, s.cell.cfg.Timeout)
		defer cancel()
		hookCtx = withServiceHealthProgress(hookCtx, &s.healthProgress)
	}
	err := hook(hookCtx)
	s.latchServiceShutdownRefusal(ctx, operation, err)
	if cancellation := serviceControlCancellation(ctx); cancellation != nil {
		return cancellation
	}
	if ctx.Err() != nil {
		if operation == "health" {
			return serviceFailureFallback(ctx.Err(), serviceHealthSite(s.healthProgress.site))
		}
		return ctx.Err()
	}
	if hookCtx.Err() != nil {
		err = serviceFailureFallback(hookCtx.Err(), serviceHealthSite(s.healthProgress.site))
	}
	s.operation = "none"
	return err
}

func (s *supervisor) step(ctx context.Context) (result error) {
	previousStatus := s.status
	previousHeld, previousDisplay := s.maintenanceHeld, s.maintenanceDisplay
	previousBusy := s.maintenanceBusy
	s.maintenanceDisplay = false
	s.maintenanceBusy = false
	defer func() {
		if cancellation := serviceControlCancellation(ctx); cancellation != nil {
			s.status = previousStatus
			s.maintenanceHeld, s.maintenanceDisplay = previousHeld, previousDisplay
			s.maintenanceBusy = previousBusy
			var located *serviceFailure
			if !errors.As(result, &located) || located.site != "control_cancelled" || !errors.Is(result, ctx.Err()) {
				result = cancellation
			}
			return
		}
		s.status.LastStepOutcome = "ok"
		if result != nil {
			s.status.LastStepOutcome, s.status.Healthy = "failed", false
			s.maintenanceDisplay = false
		}
		if s.maintenanceDisplay {
			s.status.HostExit = previousStatus.HostExit
		}
	}()
	var d ServiceDescriptor
	var err error
	held := false
	if s.completingStop {
		d, err = s.cell.ServiceDescriptor()
		if cancellation := serviceControlCancellation(ctx); cancellation != nil {
			return cancellation
		}
		if err != nil {
			return serviceFailureAt(err, "supervisor_traversal")
		}
	} else {
		held, err = s.observeServiceMaintenanceInto(ctx, &d)
		if cancellation := serviceControlCancellation(ctx); cancellation != nil {
			return cancellation
		}
		if errors.Is(err, ErrLocked) {
			d, err = s.cell.ServiceDescriptor()
			if cancellation := serviceControlCancellation(ctx); cancellation != nil {
				return cancellation
			}
			if err != nil {
				return serviceFailureAt(err, "supervisor_traversal")
			}
			defer s.rememberProcesses(d)
			return s.busy()
		}
		if err != nil {
			return serviceFailureAt(err, "supervisor_traversal")
		}
	}
	defer s.rememberProcesses(d)
	frozen := held || s.pendingStartup != serviceStartupNone
	if !frozen && s.record.RepairRevision != d.RepairRevision {
		s.record = supervisorRecord{Version: 1, RepairRevision: d.RepairRevision}
		if err := s.save(); err != nil {
			return err
		}
	}
	// Wall time may move backward across a host restart. Retain the retry
	// fence, while limiting any single wait to the configured sixty-second cap.
	latest := s.hooks.now().Add(s.hooks.maxBackoff)
	if cancellation := serviceControlCancellation(ctx); cancellation != nil {
		return cancellation
	}
	if !frozen && s.record.NextStart.After(latest) {
		s.record.NextStart = latest
		if err := s.save(); err != nil {
			return err
		}
	}
	s.status = ServiceStatus{DesiredState: d.DesiredState, State: d.DesiredState, Restarts: max(0, s.record.Attempts-1), Generation: d.Payload.Generation}
	if held {
		defer func() {
			if result == nil && s.status.State != "busy" && !s.maintenanceBusy {
				_ = s.holdMaintenance()
			}
		}()
	}
	if s.blockedServiceShutdown(ctx) {
		return nil
	}
	// A crash can occur after the fifth failure is synced but before the
	// descriptor is marked faulted. Preserve that circuit decision on restart.
	if len(s.record.Failures) >= 5 && !d.Faulted {
		if err := s.fault(ctx, "service_circuit_open"); err != nil {
			return err
		}
		d.Faulted = true
	}
	if d.Faulted {
		s.status.State, s.status.Reason = "faulted", "service_circuit_open"
	}
	if d.DesiredState != "running" || d.Faulted {
		if s.running {
			if err := s.call(ctx, "stop", s.hooks.stop); err != nil {
				if errors.Is(err, ErrLocked) {
					return s.busy()
				}
				return err
			}
			s.running = false
		}
		if !held && !s.completingStop && s.pendingStartup != serviceStartupNone {
			return s.busy()
		}
		return nil
	}
	if held {
		return s.holdMaintenance()
	}
	if s.pendingStartup != serviceStartupNone {
		return s.busy()
	}
	if s.running {
		if err := s.call(ctx, "health", s.hooks.health); err == nil {
			s.status.Healthy = true
			return nil
		} else if errors.Is(err, ErrLocked) {
			return s.busy()
		} else if serviceControlCancellation(ctx) != nil {
			return err
		} else if errors.Is(err, errMaintenanceFenceActive) {
			return s.holdMaintenance()
		}
		if err := s.call(ctx, "stop", s.hooks.stop); err != nil {
			if serviceControlCancellation(ctx) != nil {
				return err
			}
			if errors.Is(err, ErrLocked) {
				return s.busy()
			}
			return s.fault(ctx, "service_stop_refused")
		}
		s.running = false
		return s.failAfterMaintenanceObservation(ctx)
	}
	now := s.hooks.now()
	if cancellation := serviceControlCancellation(ctx); cancellation != nil {
		return cancellation
	}
	if now.Before(s.record.NextStart) {
		s.status.State = "backoff"
		return nil
	}
	previousRecord := s.record
	previousRecord.Failures = slices.Clone(s.record.Failures)
	s.record.Attempts++
	if err := s.save(); err != nil {
		return err
	}
	if err := s.call(ctx, "start", s.hooks.start); err != nil {
		if serviceControlCancellation(ctx) != nil {
			return err
		}
		if errors.Is(err, ErrLocked) {
			s.record.Attempts--
			if err := s.save(); err != nil {
				return err
			}
			return s.busy()
		}
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errMaintenanceFenceActive) {
			// Completed-call compensation only. A crash before this save can
			// retain the provisional attempt, but never adds a failure.
			s.record = previousRecord
			if err := s.save(); err != nil {
				return err
			}
			return s.holdMaintenance()
		}
		if err := s.call(ctx, "stop", s.hooks.stop); err != nil {
			if errors.Is(err, ErrLocked) {
				s.running = true
				return s.busy()
			}
			return s.fault(ctx, "service_stop_refused")
		}
		return s.failAfterMaintenanceObservation(ctx)
	}
	if err := s.call(ctx, "health", s.hooks.health); err != nil {
		if serviceControlCancellation(ctx) != nil {
			return err
		}
		if errors.Is(err, errMaintenanceFenceActive) {
			s.running = true
			return s.holdMaintenance()
		}
		if errors.Is(err, ErrLocked) {
			s.running = true
			return s.busy()
		}
		if err := s.call(ctx, "stop", s.hooks.stop); err != nil {
			if errors.Is(err, ErrLocked) {
				s.running = true
				return s.busy()
			}
			return s.fault(ctx, "service_stop_refused")
		}
		return s.failAfterMaintenanceObservation(ctx)
	}
	s.running, s.status.Healthy = true, true
	s.status.State, s.status.Restarts = "running", max(0, s.record.Attempts-1)
	return nil
}

// Cache only identities already recorded by the owned lifecycle. In particular,
// observe never reads files, probes a process, or invokes a lifecycle hook.
func (s *supervisor) rememberProcesses(d ServiceDescriptor) {
	s.status.Processes = nil
	for _, component := range []string{"postgres", "server", "worker"} {
		var record ProcessRecord
		if readJSON(s.cell.path("run", component+".json"), &record) == nil && record.PID > 1 && record.StartTime != "" && record.Root == d.Root && record.Component == component {
			s.status.Processes = append(s.status.Processes, ServiceProcessIdentity{Component: component, PID: record.PID, StartTime: record.StartTime})
		}
	}
	var lease serviceLeaseRecord
	if readJSON(s.cell.path("service", "lease.json"), &lease) == nil && lease.PID > 1 && lease.StartIdentity != "" && lease.InstallationID == d.InstallationID && lease.Owner == d.UID {
		s.status.Processes = append(s.status.Processes, ServiceProcessIdentity{Component: "service-host", PID: lease.PID, StartTime: lease.StartIdentity})
	}
}

func (s *supervisor) failed(ctx context.Context) error {
	if err := serviceControlCancellation(ctx); err != nil {
		return err
	}
	now := s.hooks.now()
	if err := serviceControlCancellation(ctx); err != nil {
		return err
	}
	recent := s.record.Failures[:0]
	for _, failure := range s.record.Failures {
		if now.Sub(failure) < s.hooks.failureWindow {
			recent = append(recent, failure)
		}
	}
	s.record.Failures = append(recent, now)
	s.record.NextStart = now.Add(supervisorBackoff(len(s.record.Failures), s.hooks.initialBackoff, s.hooks.maxBackoff))
	if err := s.save(); err != nil {
		return err
	}
	if len(s.record.Failures) >= 5 {
		return s.fault(ctx, "service_circuit_open")
	}
	s.status.State, s.status.Reason, s.status.Healthy = "backoff", "service_child_unhealthy", false
	return nil
}

func supervisorBackoff(failures int, initial, ceiling time.Duration) time.Duration {
	delay := initial
	for i := 1; i < failures && delay < ceiling; i++ {
		if delay > ceiling/2 {
			return ceiling
		}
		delay *= 2
	}
	return min(delay, ceiling)
}

func (s *supervisor) busy() error {
	if s.status.State == "faulted" && (s.maintenanceHeld || s.pendingStartup != serviceStartupNone) {
		s.maintenanceBusy = true
		return nil
	}
	s.status.State, s.status.Reason, s.status.Healthy = "busy", "service_operation_in_progress", false
	return nil
}

func (s *supervisor) fault(ctx context.Context, reason string) error {
	if err := serviceControlCancellation(ctx); err != nil {
		return err
	}
	s.status.State, s.status.Reason, s.status.Healthy = "faulted", reason, false
	err := s.cell.setServiceFaulted(ctx, true)
	if errors.Is(err, ErrLocked) {
		return s.busy()
	}
	return err
}

func (s *supervisor) command(ctx context.Context, action string) (ServiceStatus, error) {
	// Only the host's health bound can produce an unhealthy verdict. A caller
	// asking to wait less cancels the command instead, preserving the previous
	// completed status and leaving subsequent decisions to the periodic tick.
	if bound := serviceControlHealthBound(ctx, s.cell.cfg.Timeout); bound < s.cell.cfg.Timeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, bound)
		defer cancel()
	}
	ctx = context.WithValue(ctx, serviceControlCommandKey{}, true)
	if err := serviceControlCancellation(ctx); err != nil {
		return s.projectedStatus(), err
	}
	if s.shutdownBlocked && (action == "stop" || action == "start" || action == "exit-host") {
		return s.status, hostExitRefused("drain")
	}
	var err error
	switch action {
	case "exit-host":
		return s.acceptHostExit(ctx)
	case "observe":
		// Return the last completed supervisor snapshot, including its health
		// result. Periodic supervision alone refreshes this observational view.
		return s.projectedStatus(), nil
	case "status", "health":
		err = s.step(ctx)
	case "stop":
		err = s.cell.SetServiceDesiredState(ctx, "stopped")
		if err == nil {
			err = s.call(ctx, "stop", s.hooks.stop)
			if err == nil {
				s.running = false
			}
		}
		if err == nil {
			s.completingStop = true
			err = s.step(ctx)
			s.completingStop = false
		}
	case "start":
		d, readErr := s.cell.ServiceDescriptor()
		if readErr != nil {
			err = readErr
		} else if d.Faulted || d.DesiredState == "maintenance" {
			err = ErrServiceControlRefused
		} else {
			err = s.cell.SetServiceDesiredState(ctx, "running")
			if err == nil {
				if s.pendingStartup != serviceStartupNone {
					err = errServiceControlDeferred
				} else {
					err = s.step(ctx)
				}
			}
		}
	case "repair":
		if s.pendingStartup != serviceStartupNone {
			err = s.cell.SetServiceDesiredState(ctx, "stopped")
			if err == nil {
				var held bool
				held, err = s.observeServiceMaintenance(ctx)
				if err == nil {
					err = errServiceControlDeferred
					if held {
						err = errMaintenanceFenceActive
					}
				}
			}
			break
		}
		err = s.cell.RepairService(ctx)
		if err == nil {
			s.shutdownBlocked, s.shutdownFaultPending = false, false
			s.running = false
			err = s.step(ctx)
		}
	default:
		err = ErrServiceControlRefused
	}
	status := s.projectedStatus()
	if action == "health" && err == nil && !status.Healthy {
		if status.State == "busy" {
			// Contention prevented a decision; it is not a child failure.
			err = errServiceControlDeferred
		} else {
			err = errServiceUnhealthy
		}
	}
	return status, serviceFailureFallback(err, "supervisor_command")
}
