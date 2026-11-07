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
	s.persistServiceShutdownFault(context.WithoutCancel(ctx))
}

func (s *supervisor) persistServiceShutdownFault(ctx context.Context) {
	if s.shutdownFaultPending && s.cell.setServiceFaulted(ctx, true) == nil {
		s.shutdownFaultPending = false
	}
	s.status.State, s.status.Reason, s.status.Healthy = "faulted", "service_stop_refused", false
}

func (s *supervisor) blockedServiceShutdown(ctx context.Context) bool {
	if !s.shutdownBlocked {
		return false
	}
	s.persistServiceShutdownFault(ctx)
	return true
}
