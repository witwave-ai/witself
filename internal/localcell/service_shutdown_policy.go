package localcell

import (
	"context"
	"errors"
	"time"
)

const (
	shutdownPhaseBarrier  = "barrier"
	shutdownPhaseStarting = "initializing"
	shutdownPhaseReady    = "runtime_ready"
)

type serviceShutdownResponse struct {
	RequestID string `json:"request_id"`
	Nonce     string `json:"nonce"`
	Accepted  bool   `json:"accepted"`
	Phase     string `json:"phase"`
}

var errShutdownAckLost = errors.New("service_shutdown_ack_lost")

// These operations all belong to one retained, previously verified process.
// The Windows adapter closes over its retained handle; the policy has no reopen
// operation and never chooses another incarnation while retrying a request.
type serviceShutdownProcess struct {
	verify      func(ProcessRecord) (bool, error)
	send        func(context.Context, serviceShutdownRequest) (serviceShutdownResponse, error)
	wait        func(context.Context, time.Duration) bool
	force       func() error
	beforeForce func(*ProcessRecord)
}

// runServiceShutdownPolicy is the shared portable ACK/retry/exit policy. A lost
// ACK never establishes acceptance: only an accepted reply or observed exit of
// this retained process can advance the tree drain.
func runServiceShutdownPolicy(ctx context.Context, record ProcessRecord, request serviceShutdownRequest, forceRights bool, appBudget time.Duration, process serviceShutdownProcess) serviceStopOutcome {
	result := func(kind serviceStopKind, detail string) serviceStopOutcome {
		return serviceStopOutcome{Kind: kind, Component: record.Component, Detail: detail}
	}
	appCtx, cancel := context.WithTimeout(ctx, appBudget)
	defer cancel()
	accepted := false
	phase := ""
	for {
		alive, err := process.verify(record)
		if err != nil {
			return result(serviceStopUnknown, "shutdown:incarnation")
		}
		if !alive {
			if accepted {
				return result(serviceStopGraceful, "")
			}
			return result(serviceStopAlreadyExited, "")
		}
		if appCtx.Err() != nil {
			break
		}
		if !accepted {
			response, requestErr := process.send(appCtx, request)
			if requestErr == nil {
				accepted = true
				phase = response.Phase
			} else if !errors.Is(requestErr, errShutdownAckLost) {
				if appCtx.Err() != nil {
					break
				}
				detail := "shutdown:pipe"
				var refusal *localShutdownRefusal
				if errors.As(requestErr, &refusal) {
					detail = refusal.step
				}
				return result(serviceStopRefused, detail)
			}
		}
		if process.wait(appCtx, 25*time.Millisecond) {
			if accepted {
				return result(serviceStopGraceful, "")
			}
			return result(serviceStopAlreadyExited, "")
		}
	}
	if ctx.Err() != nil {
		return result(serviceStopUnknown, "shutdown:deadline")
	}
	if !accepted {
		return result(serviceStopUnknown, "shutdown:ack")
	}
	if phase != shutdownPhaseReady {
		return result(serviceStopRefused, "shutdown:migration")
	}
	if !forceRights {
		return result(serviceStopRefused, "shutdown:force_rights")
	}
	if process.beforeForce != nil {
		process.beforeForce(&record)
	}
	alive, err := process.verify(record)
	if err != nil {
		return result(serviceStopRefused, "shutdown:force")
	}
	if !alive {
		return result(serviceStopGraceful, "")
	}
	if ctx.Err() != nil {
		return result(serviceStopUnknown, "shutdown:deadline")
	}
	if process.force() != nil {
		return result(serviceStopUnknown, "shutdown:force")
	}
	if !process.wait(ctx, 5*time.Second) {
		return result(serviceStopUnknown, "shutdown:force")
	}
	return result(serviceStopForced, "service_shutdown_forced")
}
