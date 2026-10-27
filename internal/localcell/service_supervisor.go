package localcell

import (
	"context"
	"errors"
	"os"
	"time"
)

var (
	// ErrServiceHostActive refuses a second lifetime owner for the same root.
	ErrServiceHostActive = errors.New("localcell: service_host_already_active")
	// ErrServiceControlRefused is the fixed authentication and privacy refusal.
	ErrServiceControlRefused = errors.New("localcell: service_control_refused")
)

// ServiceStatus deliberately contains no process arguments, paths or credentials.
type ServiceStatus struct {
	DesiredState string `json:"desired_state"`
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	Healthy      bool   `json:"healthy"`
	Restarts     int    `json:"restarts"`
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
	cell    *Cell
	hooks   supervisorHooks
	record  supervisorRecord
	status  ServiceStatus
	running bool
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
		return err
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
		return err
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
		stopCtx, cancel := context.WithTimeout(context.Background(), shutdown)
		defer cancel()
		result = errors.Join(result, retryServiceContention(stopCtx, s.hooks.stop))
	}()
	if err := retryServiceContention(ctx, func(context.Context) error { return s.hooks.reconcile() }); err != nil {
		return err
	}
	if err := retryServiceContention(ctx, s.hooks.repair); err != nil {
		return err
	}
	if err := retryServiceContention(ctx, s.hooks.stop); err != nil {
		return err
	}
	var ticker *time.Ticker
	ticks := s.hooks.ticks
	if ticks == nil {
		ticker = time.NewTicker(time.Second)
		defer ticker.Stop()
		ticks = ticker.C
	}
	if err := s.step(ctx); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticks:
			if err := s.step(ctx); err != nil {
				return err
			}
		case command := <-commands:
			status, err := s.command(command.ctx, command.action)
			command.reply <- serviceReply{status: status, err: err}
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
	return &supervisor{cell: c, hooks: h}
}

func (s *supervisor) load(d ServiceDescriptor) error {
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
	return nil
}

func (s *supervisor) save() error {
	return writeAtomic(s.cell.path("service", "supervisor.json"), s.record, nil)
}

func (s *supervisor) step(ctx context.Context) error {
	d, err := s.cell.ServiceDescriptor()
	if err != nil {
		return err
	}
	if s.record.RepairRevision != d.RepairRevision {
		s.record = supervisorRecord{Version: 1, RepairRevision: d.RepairRevision}
		if err := s.save(); err != nil {
			return err
		}
	}
	// Wall time may move backward across a host restart. Retain the retry
	// fence, while limiting any single wait to the configured sixty-second cap.
	if latest := s.hooks.now().Add(s.hooks.maxBackoff); s.record.NextStart.After(latest) {
		s.record.NextStart = latest
		if err := s.save(); err != nil {
			return err
		}
	}
	s.status = ServiceStatus{DesiredState: d.DesiredState, State: d.DesiredState, Restarts: max(0, s.record.Attempts-1)}
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
			if err := s.hooks.stop(ctx); err != nil {
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
		if err := s.hooks.health(ctx); err == nil {
			s.status.Healthy = true
			return nil
		} else if errors.Is(err, ErrLocked) {
			return s.busy()
		}
		if err := s.hooks.stop(ctx); err != nil {
			if errors.Is(err, ErrLocked) {
				return s.busy()
			}
			return s.fault(ctx, "service_stop_refused")
		}
		s.running = false
		return s.failed(ctx)
	}
	if s.hooks.now().Before(s.record.NextStart) {
		s.status.State = "backoff"
		return nil
	}
	s.record.Attempts++
	if err := s.save(); err != nil {
		return err
	}
	if err := s.hooks.start(ctx); err != nil {
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
		if err := s.hooks.stop(ctx); err != nil {
			if errors.Is(err, ErrLocked) {
				s.running = true
				return s.busy()
			}
			return s.fault(ctx, "service_stop_refused")
		}
		return s.failed(ctx)
	}
	if err := s.hooks.health(ctx); err != nil {
		if errors.Is(err, ErrLocked) {
			s.running = true
			return s.busy()
		}
		if err := s.hooks.stop(ctx); err != nil {
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

func (s *supervisor) failed(ctx context.Context) error {
	now := s.hooks.now()
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
	s.status.State, s.status.Reason, s.status.Healthy = "faulted", reason, false
	err := s.cell.setServiceFaulted(ctx, true)
	if errors.Is(err, ErrLocked) {
		return s.busy()
	}
	return err
}

func (s *supervisor) command(ctx context.Context, action string) (ServiceStatus, error) {
	var err error
	switch action {
	case "status", "health":
		err = s.step(ctx)
	case "stop":
		err = s.cell.SetServiceDesiredState(ctx, "stopped")
		if err == nil {
			err = s.hooks.stop(ctx)
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
		err = errors.New("localcell: service_unhealthy")
	}
	return s.status, err
}
