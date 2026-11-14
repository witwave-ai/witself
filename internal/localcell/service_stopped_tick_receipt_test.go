package localcell

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The harness source contract reads these literal pairs. Each row executes the
// real tick in the Windows foreground fixture's settled stopped state: clean
// journal, no running children, pending startup, or maintenance hold.
var stoppedTickReceiptCases = []struct {
	name, cancelAt, phase, operation string
}{
	{"before_tick", "before_tick", "idle", "none"},
	{"after_entry_guard", "after_entry_guard", "tick", "none"},
	{"after_observation", "after_observation", "tick", "none"},
	{"inside_now", "inside_now", "tick", "none"},
	{"before_idle_reset", "before_idle_reset", "tick", "none"},
	{"after_tick", "after_tick", "idle", "none"},
}

// Err is a synchronous scheduling seam. Reading the live result before the
// callback models cancellation immediately after a guard observed a live
// context. No goroutine, sleep, polling, or production seam is needed.
type stoppedTickReceiptContext struct {
	context.Context
	checks int
	before func(int)
	after  func(int)
}

func (c *stoppedTickReceiptContext) Err() error {
	c.checks++
	if c.before != nil {
		c.before(c.checks)
	}
	err := c.Context.Err()
	if c.after != nil {
		c.after(c.checks)
	}
	return err
}

func TestServiceStoppedTickCancellationReceipt(t *testing.T) {
	for _, test := range stoppedTickReceiptCases {
		t.Run(test.name, func(t *testing.T) {
			cell, _ := unitCell(t) // Both homes are private TempDir fixtures.
			prepareSupervisorTest(t, cell)
			if cell.SetServiceDesiredState(t.Context(), "stopped") != nil {
				t.Fatal("cannot prepare stopped tick receipt fixture")
			}
			descriptor, err := cell.ServiceDescriptor()
			if err != nil || descriptor.DesiredState != "stopped" || descriptor.Faulted {
				t.Fatal("stopped tick receipt fixture has the wrong durable intent")
			}
			hooks, now, _, _, _ := supervisorTestHooks()
			lifecycleCalls := 0
			forbidden := func(context.Context) error { lifecycleCalls++; return nil }
			hooks.start, hooks.stop, hooks.repair, hooks.health = forbidden, forbidden, forbidden, forbidden
			hooks.reconcile = func() error { lifecycleCalls++; return nil }
			s := newSupervisor(cell, SupervisorPolicy{hooks: &hooks})
			if s.load(descriptor) != nil || s.tick(t.Context()) != nil || lifecycleCalls != 0 ||
				s.status.State != "stopped" || s.running || s.pendingStartup != serviceStartupNone || s.maintenanceHeld ||
				s.phase != "idle" || s.operation != "none" {
				t.Fatal("stopped tick receipt fixture did not settle idle without lifecycle work")
			}
			assertStoppedTickJournal(t, cell)

			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &stoppedTickReceiptContext{Context: base}
			canceledAt := ""
			cancelAt := func(site string) {
				if canceledAt != "" {
					t.Fatal("stopped tick cancellation seam ran more than once")
				}
				canceledAt = site
				cancel()
			}
			clockCalls := 0
			s.hooks.now = func() time.Time {
				clockCalls++
				if test.cancelAt == "inside_now" {
					cancelAt("inside_now")
				}
				return *now
			}
			// With no control-command marker, the real path checks Err at tick
			// entry, maintenance-observation entry, after that observation, and
			// the final idle reset (service_supervisor.go:324,348;
			// service_maintenance.go:20,51). These gaps surround the only
			// reachable hook, now (service_supervisor.go:464). No pair of
			// lifecycle hooks is reachable in this state.
			ctx.after = func(check int) {
				if test.cancelAt == "after_entry_guard" && check == 1 {
					cancelAt("after_entry_guard")
				}
				if test.cancelAt == "after_observation" && check == 3 {
					cancelAt("after_observation")
				}
			}
			ctx.before = func(check int) {
				if test.cancelAt == "before_idle_reset" && check == 4 {
					cancelAt("before_idle_reset")
				}
			}
			if test.cancelAt == "before_tick" {
				cancelAt("before_tick")
			}
			err = s.tick(ctx)
			if test.cancelAt == "after_tick" {
				cancelAt("after_tick")
			}
			wantCanceledError := test.cancelAt == "before_tick" || test.cancelAt == "after_entry_guard"
			if (wantCanceledError && !errors.Is(err, context.Canceled)) || (!wantCanceledError && err != nil) ||
				canceledAt != test.cancelAt || base.Err() != context.Canceled {
				t.Fatal("stopped tick did not reach the requested cancellation boundary")
			}
			wantClockCalls := 1
			if wantCanceledError {
				wantClockCalls = 0
			}
			if clockCalls != wantClockCalls || lifecycleCalls != 0 || s.running || s.pendingStartup != serviceStartupNone || s.maintenanceHeld {
				t.Fatal("stopped tick reached an unexpected hook or changed its settled state")
			}
			if s.phase != test.phase || s.operation != test.operation {
				t.Fatal("stopped tick cancellation changed the exact shutdown receipt label pair")
			}
			assertStoppedTickJournal(t, cell)
		})
	}
}

func assertStoppedTickJournal(t *testing.T, cell *Cell) {
	t.Helper()
	progress, err := cell.readJournal()
	if err != nil || progress.pending != nil || progress.tornTail {
		t.Fatal("stopped tick receipt fixture lost its clean readable journal")
	}
}
