package localcell

import (
	"context"
	"errors"
)

// Once a Windows stop is unknown or refused, a fresh periodic/deferred context
// must not turn that refusal into a later force authorization. Explicit repair
// is the only in-process transition that reopens supervision.
func (s *supervisor) latchServiceShutdownRefusal(ctx context.Context, operation string, err error) {
	if !s.hooks.windowsShutdownPolicy || operation != "stop" || errors.Is(err, ErrLocked) {
		return
	}
	if ctx.Err() == nil && serviceStopPermitsHostExit(err) {
		return
	}
	s.shutdownBlocked, s.shutdownFaultPending = true, true
	s.shutdownFaultCommit = serviceFaultCommit{}
	if s.publishStopRefusal != nil {
		s.publishStopRefusal()
	}
	s.persistServiceShutdownFault(context.WithoutCancel(ctx))
}

func (s *supervisor) persistServiceShutdownFault(ctx context.Context) {
	if s.shutdownFaultPending {
		if s.hooks.beforeFaultWrite != nil {
			s.hooks.beforeFaultWrite()
		}
		_ = s.commitShutdownFault(ctx)
	}
	s.status.State, s.status.Reason, s.status.Healthy = "faulted", "service_stop_refused", false
}

func (s *supervisor) commitShutdownFault(ctx context.Context) error {
	// Cancellation before the operation leaves the detached latch result intact.
	if err := ctx.Err(); err != nil {
		return err
	}
	err := s.cell.commitServiceFault(true, &s.shutdownFaultCommit)
	s.shutdownFaultPending = err != nil
	return err
}

func (s *supervisor) retireShutdownFault() {
	s.shutdownFaultCommit = serviceFaultCommit{}
}

func (s *supervisor) blockedServiceShutdown(ctx context.Context) bool {
	if !s.shutdownBlocked {
		return false
	}
	s.persistServiceShutdownFault(ctx)
	return true
}
