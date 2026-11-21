//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptrace"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

// Once the stopped host is idle, its next use of a context value is in the
// periodic tick. Hold it there while the control loop keeps taking requests.
type heldHarnessTick struct {
	context.Context
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *heldHarnessTick) Value(key any) any {
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		select {
		case <-g.release:
		case <-g.Done():
		}
	}
	return g.Context.Value(key)
}

func TestServiceStopWaitsForHeldTick(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	root, cell := prepareServiceHarnessInstallation(t, "", "", nil)
	host, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate the foreground host fixture")
	}
	args := serviceFlags{root: root, timeout: time.Second}.hostArgs()
	d := localcell.ServiceDescriptor{Adapter: "foreground", DesiredState: "stopped", Payload: localcell.ServicePayload{Host: host, Args: args}}
	if err := cell.PrepareService(t.Context(), d); err != nil {
		t.Fatal("cannot prepare the stopped host fixture")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	gate := &heldHarnessTick{Context: ctx, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- cell.Run(gate, localcell.SupervisorPolicy{ShutdownTimeout: time.Second}) }()
	t.Cleanup(func() {
		cancel()
		if <-done != nil {
			t.Error("foreground host failed at shutdown")
		}
	})
	for {
		status, err := cell.Control(ctx, "observe")
		if err == nil && !status.WorkInFlight && status.State == "stopped" && status.LastStepOutcome == "ok" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("foreground host did not publish its stopped state")
		case <-time.After(10 * time.Millisecond):
		}
	}
	gate.armed.Store(true)
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("the periodic supervision tick did not enter its hold")
	}

	// The server has already decided the first stop's response when its first
	// byte arrives. Releasing earlier could let a single attempt pass.
	var responses atomic.Int32
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotFirstResponseByte: func() {
		if responses.Add(1) == 1 {
			close(gate.release)
		}
	}})
	var output bytes.Buffer
	err = runServiceCommand(ctx, []string{"service", "stop", "--root", root}, &output)
	if err != nil && err.Error() == "localcell: service_control_deferred" {
		t.Fatal("harness stop returned the held tick's deferral")
	}
	var got localcell.ServiceStatus
	if err != nil || responses.Load() < 2 || json.Unmarshal(output.Bytes(), &got) != nil || got.State != "stopped" || got.DesiredState != "stopped" || got.WorkInFlight {
		t.Fatal("harness stop did not complete after the held tick")
	}
}
