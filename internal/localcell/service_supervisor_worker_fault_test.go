package localcell

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hand the real scheduler the same capacity-one reply channel as its handler.
func (w *serviceWorkerHost) boundaryCommand(t *testing.T, action string) chan serviceReply {
	t.Helper()
	reply := make(chan serviceReply, 1)
	select {
	case w.commands <- serviceCommand{ctx: t.Context(), action: action, reply: reply}:
	case <-time.After(w.cell.cfg.Timeout):
		t.Fatal("host did not admit the boundary request")
	}
	return reply
}

// Hold a selected tick at its next command-marker read.
type serviceWorkerTickGate struct {
	context.Context
	arm, awaitingIdle atomic.Bool
	idle              chan struct{}
	hold              func()
}

// After the first fault write a blocked host asks for Done only when the
// scheduler returns to its idle select. No client reads the refusal here.
func (g *serviceWorkerTickGate) Done() <-chan struct{} {
	if g.awaitingIdle.CompareAndSwap(true, false) {
		close(g.idle)
	}
	return g.Context.Done()
}

func (g *serviceWorkerTickGate) Value(key any) any {
	if _, ok := key.(serviceControlCommandKey); ok && g.arm.CompareAndSwap(true, false) {
		g.hold()
	}
	return g.Context.Value(key)
}

func TestServiceWorkerFaultCancellation(t *testing.T) {
	for _, stage := range []string{"caused", "persistence_pending"} {
		t.Run(stage, func(t *testing.T) { serviceWorkerRefusal(t, stage) })
	}
	independent := errors.New("independent worker failure")
	for stage, workErr := range map[string]error{"joined": errors.Join(context.Canceled, independent), "unrelated": independent} {
		t.Run(stage, func(t *testing.T) {
			s := supervisor{shutdownBlocked: true}
			if s.workerExitError(workErr, context.Canceled, true) != workErr {
				t.Fatal("host shutdown discarded an independent cause")
			}
		})
	}
}

func TestServiceWorkerLatchedFault(t *testing.T) {
	for _, stage := range []string{"refusal_write", "refusal_write_observed", "after_write_observed", "blocked_tick", "after_repair", "refused_parked_exit", "endpoint_refresh"} {
		t.Run(stage, func(t *testing.T) { serviceWorkerRefusal(t, stage) })
	}
}

func serviceWorkerRefusal(t *testing.T, stage string) {
	t.Helper()
	c := serviceCancellationReceiptCell(t)
	prepareSupervisorTest(t, c)
	w := serviceWorkerHost{cell: c, ticks: make(chan time.Time), commands: make(chan serviceCommand)}
	h, _, _, _, _ := supervisorTestHooks()
	h.ticks, h.commands, h.windowsShutdownPolicy = w.ticks, w.commands, true
	entered, release := make(chan struct{}), make(chan struct{})
	free := sync.OnceFunc(func() { close(release) })
	hold := sync.OnceFunc(func() { close(entered); <-release })
	hostCtx, cancel := context.WithCancel(t.Context())
	gate := &serviceWorkerTickGate{Context: hostCtx, idle: make(chan struct{}), hold: hold}
	var unhealthy atomic.Bool
	h.health = func(context.Context) error {
		if unhealthy.Load() {
			return errors.New("synthetic unhealthy tree")
		}
		return nil
	}
	var refusals atomic.Int32
	h.stop = func(ctx context.Context) error {
		if unhealthy.Load() {
			refusals.Add(1)
			if stage == "caused" {
				hold()
				return ctx.Err()
			}
			return errors.New("synthetic stop refusal")
		}
		return nil
	}
	switch stage {
	case "blocked_tick", "after_repair":
		h.beforeFaultWrite = func() { gate.awaitingIdle.Store(true) }
	case "after_write_observed":
		h.beforeFaultWrite = func() { gate.arm.Store(true) }
	case "refusal_write", "refusal_write_observed", "persistence_pending", "refused_parked_exit":
		h.beforeFaultWrite = hold
	}
	w.done = StartJoinedTestRunForTest(t, cancel, c.cfg.Timeout, func() error {
		return c.Run(gate, SupervisorPolicy{hooks: &h, ShutdownTimeout: c.cfg.Timeout})
	})
	defer cancel()
	defer free()
	w.ready(t)
	if stage == "endpoint_refresh" {
		// Pause the actual writer between descriptor publication and endpoint refresh.
		c.serviceControlMu.Lock()
		defer c.serviceControlMu.Unlock()
	}
	unhealthy.Store(true)
	w.within(t, nil, w.ticks, "unhealthy tick was not admitted")
	ctx, end := context.WithTimeout(t.Context(), c.cfg.Timeout)
	defer end()
	if stage == "endpoint_refresh" {
		serviceWorkerObserveDuringRefresh(ctx, t, &w)
		return
	}
	if stage == "blocked_tick" || stage == "after_repair" {
		if stage == "blocked_tick" {
			w.within(t, gate.idle, nil, "faulted worker did not return to idle")
		} else {
			// This row is about the tick after the repair. It asks only once the refused stop's two
			// publications are complete; the endpoint_refresh row covers a request made between them.
			w.within(t, gate.idle, nil, "refused stop did not return to idle before the repair")
			if v := serviceWorkerSettled(ctx, t, c); v.Reason != "service_stop_refused" {
				t.Fatal("completed tick did not publish its stop refusal")
			}
			unhealthy.Store(false)
			if _, err := c.Control(ctx, "repair"); err != nil {
				t.Fatal("explicit repair did not clear the fault")
			}
			if _, err := c.Control(ctx, "start"); err != nil {
				t.Fatal("repaired host did not restart")
			}
		}
		gate.arm.Store(true)
		w.within(t, nil, w.ticks, "subsequent tick was not admitted")
	}
	w.within(t, entered, nil, "fault sequence did not reach its barrier")
	switch stage {
	case "after_repair":
		v, err := c.Control(ctx, "observe")
		serviceWorkerCheckControl(t, err, "could not observe the tick after repair")
		if !v.WorkInFlight {
			t.Fatal("tick after repair was not in flight")
		}
		if v.State != "running" {
			t.Fatal("tick after repair did not publish running")
		}
		if !v.Healthy {
			t.Fatal("tick after repair lost its healthy observation")
		}
		return
	case "refused_parked_exit":
		ex := w.boundaryCommand(t, "exit-host")
		free()
		select {
		case r := <-ex:
			var refused *serviceControlRefusal
			if !errors.As(r.err, &refused) || refused.step != "host_exit:drain" {
				t.Fatal("faulted host admitted a parked exit")
			}
		case <-ctx.Done():
			t.Fatal("parked exit missed its first refusal")
		}
		defer func() {
			cancel()
			if len(ex) != 0 {
				<-ex
			}
		}()
		v, err := c.Control(ctx, "observe")
		if err != nil || v.WorkInFlight {
			t.Fatal("refused parked exit ran again")
		}
		return
	case "after_write_observed":
		if d, err := c.ServiceDescriptor(); err != nil || !d.Faulted {
			t.Fatal("worker reached the postwrite barrier before the durable fault")
		}
		fallthrough
	case "refusal_write_observed":
		v, err := c.Control(ctx, "observe")
		if err != nil || !v.WorkInFlight || v.Reason != "service_stop_refused" || v.State != "faulted" {
			t.Fatal("live stop refusal was not observable before cancellation")
		}
	case "persistence_pending":
		lock, err := c.own(false)
		if err != nil {
			t.Fatal("could not hold the fixture operation lock")
		}
		defer func() { _ = lock.Close() }()
	}
	cancel()
	free()
	select {
	case err := <-w.done:
		if stage == "caused" || stage == "persistence_pending" {
			if !errors.Is(err, context.Canceled) {
				t.Fatal("unsettled stop refusal swallowed host cancellation")
			}
		} else if err != nil {
			t.Fatal("durable stop refusal lost clean shutdown")
		}
	case <-ctx.Done():
		t.Fatal("faulted host did not return within its bound")
	}
	d, err := c.ServiceDescriptor()
	if err != nil || d.Faulted != (stage != "persistence_pending") {
		t.Fatal("shutdown left the wrong durable fault state")
	}
	if refusals.Load() != 1 {
		t.Fatal("shutdown retried the refused protective stop")
	}
}

func serviceWorkerObserveDuringRefresh(ctx context.Context, t *testing.T, w *serviceWorkerHost) {
	t.Helper()
	client, err := New(w.cell.cfg)
	if err != nil {
		t.Fatal("could not prepare an independent control client")
	}
	// The faulted descriptor is visible and its journal has been removed.
	// The host cannot advance the endpoint until the caller releases the mutex.
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		var d ServiceDescriptor
		if readJSON(t.Context(), client.path("service", "descriptor.json"), &d) == nil && d.Faulted {
			_, err := os.Lstat(client.path("service", "transition.json"))
			if errors.Is(err, os.ErrNotExist) {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("fault publication did not reach endpoint refresh")
		case <-poll.C:
		}
	}
	reply := w.boundaryCommand(t, "observe")
	select {
	case r := <-reply:
		serviceWorkerCheckControl(t, r.err, "scheduler refused observation during endpoint refresh")
		if r.status.Reason != "service_stop_refused" {
			t.Fatal("scheduler did not publish the stop refusal during endpoint refresh")
		}
	case <-ctx.Done():
		t.Fatal("scheduler did not answer during endpoint refresh")
	}
	v, err := client.Control(ctx, "observe")
	if runtime.GOOS == "windows" {
		// Known gap, the same on main and pinned here until the service host's descriptor and endpoint are
		// published as one (follow-up LC-HOST-ENDPOINT-1): a Windows client compares the two records'
		// revisions before it opens the pipe, so between the two publications it is refused although the
		// host, as shown above, would answer. The slice that closes the gap replaces this expectation.
		var refusal *serviceControlRefusal
		if err == nil {
			t.Fatal("independent Windows client was answered during endpoint refresh; the pinned gap has closed")
		}
		if !errors.As(err, &refusal) || refusal.step != "endpoint:record" {
			serviceWorkerCheckControl(t, err, "independent Windows client failed outside the endpoint record during endpoint refresh")
		}
		return
	}
	serviceWorkerCheckControl(t, err, "independent client could not observe during endpoint refresh")
	if !v.WorkInFlight {
		t.Fatal("independent client missed work in flight during endpoint refresh")
	}
	if v.State != "faulted" {
		t.Fatal("independent client missed the faulted state during endpoint refresh")
	}
	if v.Reason != "service_stop_refused" {
		t.Fatal("independent client missed the stop refusal during endpoint refresh")
	}
	if v.Healthy {
		t.Fatal("independent client saw a healthy fault during endpoint refresh")
	}
}
