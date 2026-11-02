package localcell

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// serviceShutdownDeadlineContext expires synchronously when its embedded
// context is canceled, preserving a fixed deadline without a wall-clock sleep.
type serviceShutdownDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c *serviceShutdownDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func (c *serviceShutdownDeadlineContext) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

func TestServiceRunShutdownSharedDeadline(t *testing.T) {
	for _, tc := range []struct {
		name          string
		timeout       time.Duration
		exhaust       bool
		repairReturns bool
	}{
		{name: "remaining_budget"},
		{name: "exhausted_repair_returns_nil", timeout: 30 * time.Second, exhaust: true},
		{name: "exhausted_repair_returns_deadline", timeout: time.Minute, exhaust: true, repairReturns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, m := unitCell(t)
			if runtime.GOOS != "windows" {
				// Keep the control socket within the native Unix path limit.
				root, err := os.MkdirTemp("/tmp", "lc-deadline-")
				if err != nil {
					t.Fatal("cannot create short private fixture")
				}
				t.Cleanup(func() { _ = os.RemoveAll(root) })
				c, err = New(Config{Root: root, Timeout: time.Second})
				if err != nil || c.layout() != nil || writeAtomic(c.path("manifest.json"), m, nil) != nil {
					t.Fatal("cannot prepare short private fixture")
				}
			}
			prepareSupervisorTest(t, c)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h, _, _, _, _ := supervisorTestHooks()
			if runtime.GOOS != "windows" {
				h.boot = func() (string, error) { return "synthetic-boot", nil }
			}
			var budget *serviceShutdownDeadlineContext
			var expire context.CancelFunc
			factories, repairs, stops, canceled := 0, 0, 0, false
			deadline := time.Now().Add(30 * time.Second)
			h.shutdownContext = func(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
				factories++
				if factories != 1 || timeout != 30*time.Second || parent.Err() != nil || parent.Done() != nil || ctx.Err() == nil {
					t.Fatal("shutdown did not create one fresh signal-independent 30 second parent budget")
				}
				if _, bounded := parent.Deadline(); bounded {
					t.Fatal("shutdown parent inherited a deadline")
				}
				var controlled context.Context
				controlled, expire = context.WithCancel(parent)
				budget = &serviceShutdownDeadlineContext{Context: controlled, deadline: deadline}
				return budget, func() { canceled = true; expire() }
			}
			assertBudget := func(opCtx context.Context) {
				t.Helper()
				gotDeadline, bounded := opCtx.Deadline()
				if opCtx != budget || !bounded || gotDeadline != deadline || opCtx.Err() != nil {
					t.Fatal("shutdown repair and stop did not share the unchanged parent budget")
				}
			}
			h.health = func(context.Context) error { cancel(); return nil }
			h.repair = func(opCtx context.Context) error {
				if ctx.Err() == nil {
					return nil // Startup repair does not consume the shutdown budget.
				}
				repairs++
				assertBudget(opCtx)
				if tc.exhaust {
					expire() // Deterministically consume the whole shared deadline in repair.
					if tc.repairReturns {
						return opCtx.Err()
					}
				}
				return nil
			}
			h.stop = func(opCtx context.Context) error {
				if ctx.Err() == nil {
					return nil // Startup stop.
				}
				stops++
				assertBudget(opCtx)
				return nil
			}
			err := c.Run(ctx, SupervisorPolicy{hooks: &h, ShutdownTimeout: tc.timeout})
			if factories != 1 || repairs != 1 || !canceled {
				t.Fatal("shutdown budget was reset, skipped, or left uncanceled")
			}
			if tc.exhaust {
				if stops != 0 || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "context deadline exceeded") {
					t.Fatal("repair exhausted the shared deadline without a visible deadline failure or admitted stop afterward")
				}
			} else if err != nil || stops != 1 {
				t.Fatal("shutdown with remaining shared budget did not complete repair then stop")
			}
		})
	}
}
