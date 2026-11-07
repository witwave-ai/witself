//go:build !windows

package localcell

import (
	"context"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

// InstallLocalShutdown preserves unselected startup and refuses unavailable transports.
func InstallLocalShutdown(_ context.Context, _ context.CancelFunc, cfg runtimeconfig.LocalShutdown, _ string) (LocalShutdownChannel, error) {
	if cfg == (runtimeconfig.LocalShutdown{}) {
		return nil, nil
	}
	return nil, shutdownRefused("shutdown:config")
}

func (c *Cell) stopChildOutcome(ctx context.Context, component string, state ProcessState) serviceStopOutcome {
	if err := c.terminateChild(ctx, component, state); err != nil {
		return serviceStopOutcome{Kind: serviceStopRefused, Component: component, Detail: "shutdown:incarnation"}
	}
	for {
		current, err := c.processState(component)
		if err != nil {
			return serviceStopOutcome{Kind: serviceStopRefused, Component: component, Detail: "shutdown:incarnation"}
		}
		if !current.Alive || !current.Matches {
			return serviceStopOutcome{Kind: serviceStopGraceful, Component: component}
		}
		select {
		case <-ctx.Done():
			return serviceStopOutcome{Kind: serviceStopUnknown, Component: component, Detail: "shutdown:deadline"}
		case <-time.After(25 * time.Millisecond):
		}
	}
}
