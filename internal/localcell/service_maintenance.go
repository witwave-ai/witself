package localcell

import (
	"context"
	"errors"
	"slices"
)

type serviceStartupPhase uint8

const (
	serviceStartupNone serviceStartupPhase = iota
	serviceStartupReconcile
	serviceStartupRepair
	serviceStartupStop
)

// This observation can suppress scheduling, never authorize a later effect.
// Each enabling owner must still admit the effect under its own continuous lock.
func (c *Cell) serviceMaintenanceObservation(ctx context.Context, observed *ServiceDescriptor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := c.own(false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	d, err := c.readServiceDescriptor()
	if err != nil {
		return err
	}
	if observed != nil {
		*observed = d
	}
	if err := c.maintenanceFenceAllows(0); err != nil {
		return err
	}
	return nil
}

func (s *supervisor) observeServiceMaintenance(ctx context.Context) (bool, error) {
	return s.observeServiceMaintenanceInto(ctx, nil)
}

func (s *supervisor) observeServiceMaintenanceInto(ctx context.Context, observed *ServiceDescriptor) (bool, error) {
	err := s.cell.serviceMaintenanceObservation(ctx, observed)
	if cancellation := serviceControlCancellation(ctx); cancellation != nil {
		return false, cancellation
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if errors.Is(err, errMaintenanceFenceActive) {
		s.maintenanceHeld = true
		return true, nil
	}
	if err == nil {
		s.maintenanceHeld = false
	}
	return false, err
}

func (s *supervisor) holdMaintenance() error {
	s.maintenanceHeld, s.maintenanceDisplay = true, true
	s.maintenanceBusy = false
	if s.pendingStartup != serviceStartupNone && s.startupFaulted {
		s.status.State, s.status.Reason = "faulted", "service_circuit_open"
	}
	s.status.LastStepOutcome = "ok"
	return nil
}

func (s *supervisor) projectedStatus() ServiceStatus {
	status := s.status
	status.Processes = slices.Clone(status.Processes)
	if s.maintenanceBusy {
		status.State, status.Reason, status.Healthy = "busy", "service_operation_in_progress", false
	} else if s.maintenanceDisplay {
		status.State, status.Reason, status.Healthy = "maintenance", errMaintenanceFenceActive.Error(), false
		status.Restarts = max(0, s.record.Attempts-1)
	}
	return status
}

func (s *supervisor) resumeServiceStartup(ctx context.Context) error {
	if s.pendingStartup == serviceStartupReconcile {
		if err := s.call(ctx, "reconcile", func(context.Context) error { return s.hooks.reconcile() }); err != nil {
			if ctx.Err() == nil && errors.Is(err, errMaintenanceFenceActive) {
				return s.holdMaintenance()
			}
			return serviceFailureAt(err, "startup_reconcile")
		}
		s.pendingStartup = serviceStartupRepair
	}
	if s.pendingStartup == serviceStartupRepair {
		if err := s.call(ctx, "repair", s.hooks.repair); err != nil {
			if ctx.Err() == nil && errors.Is(err, errMaintenanceFenceActive) {
				return s.holdMaintenance()
			}
			return serviceFailureAt(err, "startup_repair")
		}
		s.pendingStartup = serviceStartupStop
	}
	if s.pendingStartup == serviceStartupStop {
		if err := s.call(ctx, "stop", s.hooks.stop); err != nil {
			return serviceFailureAt(err, "startup_stop")
		}
		s.pendingStartup = serviceStartupNone
	}
	return nil
}

func (s *supervisor) failAfterMaintenanceObservation(ctx context.Context) error {
	held, err := s.observeServiceMaintenance(ctx)
	if errors.Is(err, ErrLocked) {
		return s.busy()
	}
	if err != nil {
		return err
	}
	if held {
		return s.holdMaintenance()
	}
	return s.failed(ctx)
}

// Suppress only the exact maintenance cause through our own site wrappers.
// A joined error retains every cause, even when one member is maintenance.
func serviceMaintenanceOnly(err error) bool {
	for {
		if err == errMaintenanceFenceActive {
			return true
		}
		failure, ok := err.(*serviceFailure)
		if !ok {
			return false
		}
		err = failure.cause
	}
}
