package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"
)

// The Windows foreground fixture keeps its descriptor stopped. Interrupt its
// exact status bookkeeping seam rather than substituting a health operation.
func TestServiceStoppedStatusCancellationReceipt(t *testing.T) {
	cell, _ := unitCell(t)
	prepareSupervisorTest(t, cell)
	if err := cell.SetServiceDesiredState(t.Context(), "stopped"); err != nil {
		t.Fatal("cannot prepare stopped cancellation fixture")
	}
	descriptor, err := cell.ServiceDescriptor()
	hooks, now, _, _, _ := supervisorTestHooks()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clockCalls := 0
	hooks.now = func() time.Time {
		clockCalls++
		cancel()
		return *now
	}
	s := newSupervisor(cell, SupervisorPolicy{hooks: &hooks})
	if err != nil || s.load(descriptor) != nil {
		t.Fatal("cannot load stopped cancellation fixture")
	}
	before := s.status
	_, commandErr := s.command(ctx, "status")
	if clockCalls != 1 || !errors.Is(commandErr, context.Canceled) {
		t.Fatal("stopped status did not cancel during its bookkeeping command")
	}
	var output bytes.Buffer
	WriteServiceHostFailure(&output, commandErr, "supervisor_command")
	if output.Len() != 0 {
		if output.String() == "native service host site=control_cancelled\n" {
			t.Log("native service host site=control_cancelled")
		}
		t.Fatal("client-cancelled stopped status emitted a host failure receipt")
	}
	status, err := s.command(t.Context(), "observe")
	if err != nil || status.DesiredState != before.DesiredState || status.State != before.State || status.LastStepOutcome != before.LastStepOutcome {
		t.Fatal("cancelled stopped status changed the completed supervisor snapshot")
	}
}

// Run owns both emitting boundaries: call reports an interrupted hook and the
// command loop reports its returned error. Exercise both over real authenticated
// control transport, with a barrier inside the accepted status health hook.
func TestServiceControlCancellationReceiptActualHost(t *testing.T) {
	for _, origin := range []string{"client", "host"} {
		t.Run(origin, func(t *testing.T) {
			cell := serviceCancellationReceiptCell(t)
			prepareSupervisorTest(t, cell)
			hooks, _, _, _, _ := supervisorTestHooks()
			if runtime.GOOS != "windows" {
				hooks.boot = func() (string, error) { return "synthetic-boot", nil }
			}
			hooks.ticks = make(chan time.Time)
			ready, entered := make(chan struct{}), make(chan struct{})
			healthCalls := 0
			hooks.health = func(ctx context.Context) error {
				healthCalls++
				if healthCalls == 1 {
					close(ready)
					return nil
				}
				serviceHealthStep(ctx, "health_database")
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			}
			budget, budgetCancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer budgetCancel()
			hostCtx, hostCancel := context.WithCancel(budget)
			defer hostCancel()
			stderr, err := os.CreateTemp(t.TempDir(), "cancel-receipt")
			if err != nil {
				t.Fatal("cannot capture cancellation receipt")
			}
			previous := os.Stderr
			os.Stderr = stderr
			defer func() { os.Stderr = previous; _ = stderr.Close() }()
			done := make(chan struct{})
			var hostErr error
			go func() {
				hostErr = cell.Run(hostCtx, SupervisorPolicy{hooks: &hooks, ShutdownTimeout: time.Second})
				close(done)
			}()
			defer func() {
				hostCancel()
				<-done // Join every emitter before restoring stderr.
			}()
			select {
			case <-ready:
			case <-done:
				t.Fatal("foreground cancellation fixture exited before readiness")
			case <-budget.Done():
				t.Fatal("foreground cancellation fixture never became ready")
			}
			clientCtx, clientCancel := context.WithCancel(budget)
			defer clientCancel()
			controlDone := make(chan error, 1)
			go func() {
				_, err := cell.Control(clientCtx, "status")
				controlDone <- err
			}()
			select {
			case <-entered:
			case <-done:
				t.Fatal("foreground cancellation fixture exited before accepted status")
			case <-budget.Done():
				t.Fatal("status never reached the health cancellation barrier")
			}
			if origin == "client" {
				clientCancel()
			} else {
				hostCancel()
			}
			select {
			case err := <-controlDone:
				if err == nil {
					t.Fatal("cancelled control request returned successful status")
				}
			case <-budget.Done():
				t.Fatal("cancelled control request did not finish")
			}
			if origin == "client" {
				status, err := cell.Control(budget, "observe")
				if err != nil || status.State != "running" || !status.Healthy || status.LastStepOutcome != "ok" {
					t.Fatal("client cancellation stopped the host or changed its completed snapshot")
				}
				hostCancel()
			}
			select {
			case <-done:
			case <-budget.Done():
				t.Fatal("foreground cancellation fixture did not finish shutdown")
			}
			if hostErr != nil || healthCalls != 2 {
				t.Fatal("cancellation fixture lost its single accepted status or clean shutdown")
			}
			data, err := os.ReadFile(stderr.Name())
			if err != nil {
				t.Fatal("cannot read cancellation receipt")
			}
			want := "service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true\n"
			if origin == "host" {
				want = "native service host site=control_cancelled\n" +
					"service_shutdown phase=control operation=health site=health_database journal_pending=false journal_read_ok=true\n"
			}
			if string(data) != want {
				if bytes.Contains(data, []byte("native service host site=control_cancelled\n")) {
					t.Log("native service host site=control_cancelled")
				}
				t.Fatal("control cancellation emitted the wrong host failure or shutdown receipts")
			}
		})
	}
}

func serviceCancellationReceiptCell(t *testing.T) *Cell {
	t.Helper()
	cell, manifest := unitCell(t) // Both home variables are pinned to TempDir.
	if runtime.GOOS != "windows" {
		root, err := os.MkdirTemp("/tmp", "lc-cancel-")
		if err != nil {
			t.Fatal("cannot create short private cancellation fixture")
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		cell, err = New(Config{Root: root, Timeout: 5 * time.Second})
		if err != nil || cell.layout() != nil || writeAtomic(cell.path("manifest.json"), manifest, nil) != nil {
			t.Fatal("cannot prepare short private cancellation fixture")
		}
	}
	cell.cfg.Timeout = 5 * time.Second
	return cell
}
