package localcell

import (
	"context"
	"testing"
	"time"
)

// NativeServiceReadinessBudgetForTest is exported only by the test build so the
// external native proof and internal transport probe exercise one calculation.
// The first readiness measurement remains an input for call-site compatibility,
// but cannot raise either readiness wait above the path-derived four host bounds.
func NativeServiceReadinessBudgetForTest(bound, _ time.Duration) time.Duration {
	return serviceControlBudget(max(bound, 0), 4)
}

// The reviewer reproduction scales the native host bound to one second and its
// exact four-host-bound readiness parent by 1/60, yielding a four-second parent.
// A real request queues behind a 5H/6 tick health hook, then its own health hook
// takes 5H/6. Both finish within H, so the readiness parent must allow the reply.
func TestNativeServiceReadinessQueuedBoundedHooks(t *testing.T) {
	bound := serviceHealthMeasuredBound(t, time.Second)
	cell, commands := serviceHealthControlFixture(t, bound)
	descriptor, err := cell.ServiceDescriptor()
	hooks, _, _, _, _ := supervisorTestHooks()
	supervisor := newSupervisor(cell, SupervisorPolicy{hooks: &hooks})
	if err != nil || supervisor.load(descriptor) != nil || supervisor.step(t.Context()) != nil {
		t.Fatal("cannot start queued readiness fixture")
	}
	entered := make(chan struct{})
	var measured []time.Duration
	supervisor.hooks.health = func(context.Context) error {
		if len(measured) == 0 {
			close(entered)
		}
		started := time.Now()
		// Deliberately context-unaware: even if the parent expires, record both
		// complete bounded hooks before assessing whether delivery succeeded.
		time.Sleep(5 * bound / 6)
		measured = append(measured, time.Since(started))
		return nil
	}
	bailout, stop := context.WithTimeout(t.Context(), 8*bound)
	defer stop()
	done := make(chan struct{})
	var tickErr error
	go func() {
		defer close(done)
		tickErr = supervisor.step(bailout)
		select {
		case command := <-commands:
			status, commandErr := supervisor.command(command.ctx, command.action)
			command.reply <- serviceReply{status: status, err: commandErr}
		case <-bailout.Done():
		}
	}()
	defer func() { stop(); <-done }()
	select {
	case <-entered:
	case <-bailout.Done():
		t.Fatal("queued readiness fixture did not enter its tick")
	}
	const nativeScale = 60
	budget := NativeServiceReadinessBudgetForTest(nativeScale*bound, 0) / nativeScale
	parent, cancel := context.WithTimeout(t.Context(), budget)
	status, verdict := cell.Control(parent, "health")
	cancel()
	select {
	case <-done:
	case <-bailout.Done():
		t.Fatal("queued readiness fixture did not finish both hooks")
	}
	if tickErr != nil || len(measured) != 2 {
		t.Fatal("queued readiness fixture did not execute both bounded hooks")
	}
	for _, elapsed := range measured {
		if elapsed < 5*bound/6 || elapsed >= bound {
			t.Fatal("queued readiness hook did not finish within its host bound")
		}
	}
	if verdict != nil || !status.Healthy {
		t.Fatal("bounded restart health verdict was hidden by readiness parent deadline")
	}
}
