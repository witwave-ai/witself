package localcell

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"
)

// ServiceShutdownTimeout bounds the complete service tree: fifteen seconds per
// app, thirty seconds for PostgreSQL, and fifteen seconds for protocol/exit slack.
// Per-app waits always inherit this deadline; they cannot extend it.
const ServiceShutdownTimeout = 75 * time.Second

const serviceAppShutdownTimeout = 15 * time.Second
const servicePostgresShutdownTimeout = 30 * time.Second

type serviceStopKind string

const (
	serviceStopGraceful      serviceStopKind = "graceful"
	serviceStopAlreadyExited serviceStopKind = "already_exited"
	serviceStopForced        serviceStopKind = "forced"
	serviceStopRefused       serviceStopKind = "refused"
	serviceStopUnknown       serviceStopKind = "unknown"
)

// ErrServiceShutdownForced means all forced app exits were positively observed
// and the rest of the tree drained. It never grants a graceful receipt.
var ErrServiceShutdownForced = errors.New("localcell: service_shutdown_forced")

type serviceStopOutcome struct {
	Kind      serviceStopKind
	Component string
	Detail    string
}

func (o serviceStopOutcome) permitsDrain() bool {
	return o.Kind == serviceStopGraceful || o.Kind == serviceStopAlreadyExited || o.Kind == serviceStopForced
}

func (o serviceStopOutcome) err() error {
	switch o.Kind {
	case serviceStopGraceful, serviceStopAlreadyExited:
		return nil
	case serviceStopForced:
		return ErrServiceShutdownForced
	default:
		return fmt.Errorf("localcell: %w", shutdownRefused(serviceShutdownDetail(o.Detail)))
	}
}

// Never include a path, nonce, SID, token or a raw platform error in an outcome.
func serviceShutdownDetail(detail string) string {
	switch detail {
	case "shutdown:config", "shutdown:descriptor", "shutdown:incarnation", "shutdown:pipe", "shutdown:ack",
		"shutdown:deadline", "shutdown:force_rights", "shutdown:force", "shutdown:migration":
		return detail
	default:
		return "shutdown:incarnation"
	}
}

func serviceStopPermitsHostExit(err error) bool {
	// Equality is deliberate: a joined forced + unknown/PG failure is not proof
	// that the whole tree exited, even though errors.Is would match it.
	for {
		located, ok := err.(*serviceFailure)
		if !ok {
			break
		}
		err = located.cause
	}
	return err == nil || err == ErrServiceShutdownForced
}

func (c *Cell) serviceShutdownDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if runtime.GOOS == "windows" {
		return context.WithTimeout(ctx, ServiceShutdownTimeout)
	}
	return c.deadline(ctx)
}

// drainServiceTree is shared by the real lifecycle and hermetic policy tests.
// Unknown or refused outcomes stop before the next app or PostgreSQL. Confirmed
// forced exits permit the remainder to drain, but retain a non-graceful result.
func drainServiceTree(ctx context.Context, keep map[string]bool, stopApp func(context.Context, string) serviceStopOutcome, stopPG func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, ServiceShutdownTimeout)
	defer cancel()
	forced := false
	for _, component := range []string{"worker", "server"} {
		if keep[component] {
			continue
		}
		if ctx.Err() != nil {
			return (serviceStopOutcome{Kind: serviceStopUnknown, Component: component, Detail: "shutdown:deadline"}).err()
		}
		outcome := stopApp(ctx, component)
		if !outcome.permitsDrain() {
			return outcome.err()
		}
		forced = forced || outcome.Kind == serviceStopForced
	}
	if !keep["postgres"] {
		if ctx.Err() != nil {
			return (serviceStopOutcome{Kind: serviceStopUnknown, Component: "postgres", Detail: "shutdown:deadline"}).err()
		}
		pgCtx, stopPGDeadline := context.WithTimeout(ctx, servicePostgresShutdownTimeout)
		defer stopPGDeadline()
		if err := stopPG(pgCtx); err != nil {
			return err
		}
	}
	if forced {
		return ErrServiceShutdownForced
	}
	return nil
}
