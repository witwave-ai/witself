package localcell

import (
	"context"
	"errors"
	"time"
)

func readinessPort(m *Manifest, component string) int {
	if component == "worker" {
		return m.Ports.WorkerHealth
	}
	return m.Ports.ServerHealth
}

// Config has no independent relaunch timeout. Reserve at least one tenth of
// the component's configured readiness budget for a useful second attempt.
// This scales with caller settings without making worker retries impossible
// by requiring an entire fresh Timeout after time has already elapsed.
func (c *Cell) hasRelaunchBudget(ctx context.Context, component string, m *Manifest) bool {
	deadline, ok := ctx.Deadline()
	return ok && ctx.Err() == nil && time.Until(deadline) >= c.readinessTimeout(component, m)/10
}

func (c *Cell) waitReadyWithPortRecovery(ctx context.Context, m *Manifest, component string, launched bool) error {
	err := c.waitReady(ctx, component, readinessPort(m, component))
	var exit *readinessExitError
	if !launched || !errors.As(err, &exit) || exit.component != component ||
		exit.diagnostic != "witself-"+component+": listener bind failed: address in use" ||
		!c.hasRelaunchBudget(ctx, component, m) {
		return err
	}
	// Keep every other component, including children launched by this Start.
	// Ownership is still checked by the normal rollback fences.
	keep := map[string]bool{"postgres": true, "server": true, "worker": true}
	keep[component] = false
	if stopErr := c.stopSelected(ctx, keep); stopErr != nil {
		return stopErr
	}
	if reallocateErr := c.prepareComponentPorts(m, component, false, true); reallocateErr != nil {
		return reallocateErr
	}
	// Reallocation and cleanup also spend the original readiness budget.
	if !c.hasRelaunchBudget(ctx, component, m) {
		return err
	}
	if journalErr := c.journalCause("start", component+"_ready", "started", "port_conflict_relaunch="+component); journalErr != nil {
		return journalErr
	}
	if launchErr := c.startChildContext(ctx, m, component); launchErr != nil {
		return launchErr
	}
	// Deliberately no recursion, loop, or new deadline: one relaunch, within
	// the original step budget. A second bind exit remains attributable.
	return c.waitReady(ctx, component, readinessPort(m, component))
}
