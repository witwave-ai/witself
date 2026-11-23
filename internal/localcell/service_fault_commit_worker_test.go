package localcell

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceFaultCommit(t *testing.T) {
	runServiceFaultRealPaths(t)
	runServiceFaultGuardPaths(t)
}

func runServiceFaultRealPaths(t *testing.T) {
	for _, row := range []string{"running_health", "failed_start", "poststart_health", "delivered_exit"} {
		t.Run(row, func(t *testing.T) { serviceFaultCommitWorker(t, row) })
	}
	t.Run("run_contended_durable_cancel", runContendedDurableCancel)
}

func runContendedDurableCancel(t *testing.T) {
	const failure = "host cancellation after a contended durable refusal failed"
	c := serviceCancellationReceiptCell(t)
	initial := prepareSupervisorTest(t, c)
	client := publicationClient(t, c)
	w := &serviceWorkerHost{cell: c, ticks: make(chan time.Time), commands: make(chan serviceCommand)}
	h, _, _, _, _ := supervisorTestHooks()
	h.ticks, h.commands, h.windowsShutdownPolicy = w.ticks, w.commands, true
	var unhealthy atomic.Bool
	h.health = func(context.Context) error {
		if unhealthy.Load() {
			return errServiceUnhealthy
		}
		return nil
	}
	h.stop = func(context.Context) error {
		if unhealthy.Load() {
			return ErrServiceControlRefused
		}
		return nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, resume := make(chan struct{}, 1), make(chan struct{}, 1)
	gate := &serviceWorkerTickGate{Context: ctx, hold: func() {
		entered <- struct{}{}
		select {
		case <-resume:
		case <-ctx.Done():
		}
	}}
	h.beforeFaultWrite = func() { gate.arm.Store(true) }
	w.done = StartJoinedTestRunForTest(t, cancel, c.cfg.Timeout, func() error {
		return c.Run(gate, SupervisorPolicy{hooks: &h})
	})
	w.ready(t)
	discovery := publicationDiscoveryAt(t, client, failure)

	// The first command-marker read after the latch returns precedes the
	// follow-up. Hold the operation lock until the host publishes busy.
	unhealthy.Store(true)
	w.within(t, nil, w.ticks, failure)
	w.within(t, entered, nil, failure)
	d, err := client.ServiceDescriptor()
	if err != nil || !d.Faulted || d.Revision != initial.Revision+1 {
		t.Fatal(failure)
	}
	lock, err := client.own(t.Context(), false)
	if err != nil {
		t.Fatal(failure)
	}
	release := sync.OnceFunc(func() { _ = lock.Close() })
	defer release()
	resume <- struct{}{}
	probe, end := context.WithTimeout(t.Context(), c.cfg.Timeout)
	defer end()
	if v := serviceWorkerSettled(probe, t, client.Control); v.State != "busy" || v.Reason != "service_operation_in_progress" ||
		!discovery.unchanged(publicationDiscoveryAt(t, client, failure)) {
		t.Fatal(failure)
	}
	release()

	// Cancel inside the next tick's maintenance observation, before the
	// blocked host can recheck the fault or return to the idle select.
	gate.arm.Store(true)
	w.within(t, nil, w.ticks, failure)
	w.within(t, entered, nil, failure)
	cancel()
	if serviceWorkerReceive(t, w, w.done, failure) != nil {
		t.Fatal(failure)
	}
	final, err := client.ServiceDescriptor()
	if err != nil || !final.Faulted || final.Revision != d.Revision {
		t.Fatal(failure)
	}
}

func serviceFaultCommitWorker(t *testing.T, row string) {
	t.Helper()
	const failure = "same refusal committed a second fault"
	c := serviceCancellationReceiptCell(t)
	initial := prepareSupervisorTest(t, c)
	w := &serviceWorkerHost{cell: c, ticks: make(chan time.Time), commands: make(chan serviceCommand)}
	h, _, _, _, _ := supervisorTestHooks()
	h.ticks, h.commands, h.windowsShutdownPolicy = w.ticks, w.commands, true
	var armed atomic.Bool
	var stops atomic.Int32
	h.start = func(context.Context) error {
		if armed.Load() && row == "failed_start" {
			return errors.New("synthetic start failure")
		}
		return nil
	}
	h.health = func(context.Context) error {
		if armed.Load() && (row == "running_health" || row == "poststart_health") {
			return errors.New("synthetic unhealthy tree")
		}
		return nil
	}
	h.stop = func(context.Context) error {
		if armed.Load() {
			stops.Add(1)
			return errors.New("synthetic stop refusal")
		}
		return nil
	}
	latched, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	h.beforeFaultWrite = sync.OnceFunc(func() { close(latched); <-release })

	var previous ServiceDescriptor
	receipts, runErr := serviceMaintenanceCaptureReceipt(t, func() (result error) {
		ctx, cancel := context.WithTimeout(t.Context(), serviceControlBudget(c.cfg.Timeout, 3))
		done := StartJoinedTestRunForTest(t, cancel, c.cfg.Timeout, func() error {
			return c.Run(ctx, SupervisorPolicy{hooks: &h})
		})
		defer func() {
			unblock()
			cancel()
			result = serviceWorkerReceive(t, w, done, failure)
		}()
		w.ready(t)

		// A restart command writes running before its failed start/health.
		// The delivered exit follows the separate stopped-intent command.
		if row != "running_health" {
			stopped, err := c.Control(ctx, "stop")
			if err != nil || stopped.State != "stopped" {
				t.Fatal(failure)
			}
		}
		armed.Store(true)
		var start <-chan serviceReply
		var exit *serviceHostExitSession
		var finishExit func()
		wantRevision, wantDesired := initial.Revision, "running"
		switch row {
		case "running_health":
			w.within(t, nil, w.ticks, failure)
		case "failed_start", "poststart_health":
			start = w.boundaryCommand(t, "start")
			wantRevision += 2 // stopped, running
		case "delivered_exit":
			envelope, _, err := c.newHostExitEnvelope()
			if err != nil {
				t.Fatal(failure)
			}
			exit, err = c.startHostExitRequest(ctx, envelope)
			if err != nil {
				t.Fatal(failure)
			}
			finishExit = sync.OnceFunc(exit.finish)
			defer func() {
				exit.cancel()
				finishExit()
			}()
			wantRevision++
			wantDesired = "stopped"
		}

		w.within(t, latched, nil, failure)
		live := serviceWorkerReceive(t, w, w.boundaryCommand(t, "observe"), failure)
		var err error
		previous, err = c.ServiceDescriptor()
		if err != nil || previous.Faulted || previous.Revision != wantRevision || previous.DesiredState != wantDesired ||
			live.err != nil || !live.status.WorkInFlight || live.status.Healthy || live.status.State != "faulted" || live.status.Reason != "service_stop_refused" {
			t.Fatal(failure)
		}
		unblock()
		if start != nil {
			reply := serviceWorkerReceive(t, w, start, failure)
			if reply.err != nil || reply.status.State != "faulted" || reply.status.Reason != "service_stop_refused" {
				t.Fatal(failure)
			}
		}
		wantExit := ""
		if exit != nil {
			finishExit()
			if exit.disposition != "refused" || !errors.Is(exit.err, ErrServiceControlRefused) {
				t.Fatal(failure)
			}
			wantExit = "refused"
		}
		settled := serviceWorkerSettled(ctx, t, c.Control)
		if settled.State != "faulted" || settled.Reason != "service_stop_refused" || settled.Healthy || settled.HostExit != wantExit ||
			settled.DesiredState != wantDesired || settled.LastStepOutcome != "ok" {
			t.Fatal(failure)
		}
		return nil
	})
	wantReceipt := "native service host site=supervisor_stop\n"
	switch row {
	case "running_health", "poststart_health":
		wantReceipt = "native service host site=supervisor_health\n" + wantReceipt
	case "failed_start":
		wantReceipt = "native service host site=supervisor_start\n" + wantReceipt
	}
	if runErr != nil || string(receipts) != wantReceipt || stops.Load() != 1 {
		t.Fatal(failure)
	}
	final, err := c.ServiceDescriptor()
	if err != nil || !final.Faulted || final.DesiredState != previous.DesiredState || final.Revision != previous.Revision+1 {
		t.Fatal(failure)
	}
}
