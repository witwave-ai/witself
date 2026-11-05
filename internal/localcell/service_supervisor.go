package localcell

import (
	"context"
	"errors"
	"fmt"
	"os"
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
}

type supervisorRecord struct {
	Version        int         `json:"version"`
	RepairRevision uint64      `json:"repair_revision"`
	Failures       []time.Time `json:"failures"`
	NextStart      time.Time   `json:"next_start"`
	Attempts       int         `json:"attempts"`
}

type serviceCommand struct {
	ctx    context.Context
	action string
	reply  chan serviceReply
}

type serviceReply struct {
	status ServiceStatus
	err    error
}

type supervisor struct {
	cell             *Cell
	hooks            supervisorHooks
	record           supervisorRecord
	status           ServiceStatus
	running          bool
	phase, operation string
	healthProgress   serviceHealthProgress
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
	if shutdown <= 0 || shutdown > 30*time.Second {
		shutdown = 30 * time.Second
	}
	defer func() {
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
		result = errors.Join(result, serviceFailureAt(repairErr, "shutdown_repair"), serviceFailureAt(stopErr, "shutdown_stop"))
	}()
	s.phase = "startup"
	if err := retryServiceContention(ctx, func(ctx context.Context) error {
		return s.call(ctx, "reconcile", func(context.Context) error { return s.hooks.reconcile() })
	}); err != nil {
		return serviceFailureAt(err, "startup_reconcile")
	}
	if err := retryServiceContention(ctx, func(ctx context.Context) error { return s.call(ctx, "repair", s.hooks.repair) }); err != nil {
		return serviceFailureAt(err, "startup_repair")
	}
	if err := retryServiceContention(ctx, func(ctx context.Context) error { return s.call(ctx, "stop", s.hooks.stop) }); err != nil {
		return serviceFailureAt(err, "startup_stop")
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
	defer func() {
		if cancellation := serviceControlCancellation(ctx); cancellation != nil {
			s.status = previousStatus
			var located *serviceFailure
			if !errors.As(result, &located) || located.site != "control_cancelled" || !errors.Is(result, ctx.Err()) {
				result = cancellation
			}
			return
		}
		s.status.LastStepOutcome = "ok"
		if result != nil {
			s.status.LastStepOutcome, s.status.Healthy = "failed", false
		}
	}()
	d, err := s.cell.ServiceDescriptor()
	if cancellation := serviceControlCancellation(ctx); cancellation != nil {
		return cancellation
	}
	if err != nil {
		return serviceFailureAt(err, "supervisor_traversal")
	}
	defer s.rememberProcesses(d)
	if s.record.RepairRevision != d.RepairRevision {
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
	if s.record.NextStart.After(latest) {
		s.record.NextStart = latest
		if err := s.save(); err != nil {
			return err
		}
	}
	s.status = ServiceStatus{DesiredState: d.DesiredState, State: d.DesiredState, Restarts: max(0, s.record.Attempts-1), Generation: d.Payload.Generation}
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
		return nil
	}
	if s.running {
		if err := s.call(ctx, "health", s.hooks.health); err == nil {
			s.status.Healthy = true
			return nil
		} else if errors.Is(err, ErrLocked) {
			return s.busy()
		} else if serviceControlCancellation(ctx) != nil {
			return err
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
		return s.failed(ctx)
	}
	now := s.hooks.now()
	if cancellation := serviceControlCancellation(ctx); cancellation != nil {
		return cancellation
	}
	if now.Before(s.record.NextStart) {
		s.status.State = "backoff"
		return nil
	}
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
		if err := s.call(ctx, "stop", s.hooks.stop); err != nil {
			if errors.Is(err, ErrLocked) {
				s.running = true
				return s.busy()
			}
			return s.fault(ctx, "service_stop_refused")
		}
		return s.failed(ctx)
	}
	if err := s.call(ctx, "health", s.hooks.health); err != nil {
		if serviceControlCancellation(ctx) != nil {
			return err
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
		return s.failed(ctx)
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
		return s.status, err
	}
	var err error
	switch action {
	case "observe":
		// Return the last completed supervisor snapshot, including its health
		// result. Periodic supervision alone refreshes this observational view.
		status := s.status
		status.Processes = slices.Clone(s.status.Processes)
		return status, nil
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
			err = s.step(ctx)
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
				err = s.step(ctx)
			}
		}
	case "repair":
		err = s.cell.RepairService(ctx)
		if err == nil {
			s.running = false
			err = s.step(ctx)
		}
	default:
		err = ErrServiceControlRefused
	}
	if action == "health" && err == nil && !s.status.Healthy {
		if s.status.State == "busy" {
			// Contention prevented a decision; it is not a child failure.
			err = errServiceControlDeferred
		} else {
			err = errServiceUnhealthy
		}
	}
	return s.status, serviceFailureFallback(err, "supervisor_command")
}
