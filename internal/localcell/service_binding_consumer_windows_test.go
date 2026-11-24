//go:build windows

package localcell

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Retire a normal Run only after the exact-target request has finished its
// initial admission. Start the successor in the same process before actual dial.
func bindingOriginalExitActivation(t *testing.T) {
	t.Run("original_exit_activation", func(t *testing.T) {
		cell := serviceCancellationReceiptCell(t)
		prepareSupervisorTest(t, cell)
		ctx, cancel := context.WithTimeout(t.Context(), 4*cell.cfg.Timeout)
		defer cancel()
		start := func() (context.CancelFunc, <-chan error) {
			t.Helper()
			hooks, _, _, _, _ := supervisorTestHooks()
			hooks.ticks = make(chan time.Time)
			ready := make(chan struct{})
			healthy := sync.OnceFunc(func() { close(ready) })
			hooks.health = func(context.Context) error { healthy(); return nil }
			runCtx, stop := context.WithCancel(ctx)
			done := StartJoinedTestRunForTest(t, stop, cell.cfg.Timeout, func() error { return cell.Run(runCtx, SupervisorPolicy{hooks: &hooks}) })
			select {
			case <-ready:
			case <-done:
				t.Fatal(bindingExitFailure)
			case <-ctx.Done():
				t.Fatal(bindingExitFailure)
			}
			if serviceHealthyAfterStartup(ctx, cell) != nil {
				t.Fatal(bindingExitFailure)
			}
			return stop, done
		}
		stopFirst, firstDone := start()
		cell.serviceControlMu.Lock()
		firstListener := cell.serviceControlListener
		cell.serviceControlMu.Unlock()
		client, err := New(cell.cfg)
		if err != nil {
			t.Fatal(bindingExitFailure)
		}
		original, lease, err := client.newHostExitEnvelope(t.Context())
		if err != nil {
			t.Fatal(bindingExitFailure)
		}
		retained, err := client.retainServiceExitHost(t.Context(), lease)
		if err != nil {
			t.Fatal(bindingExitFailure)
		}
		defer retained.close()
		var successorDone <-chan error
		passes := 0
		client.observeRootWalk = func(before bool) {
			if before {
				return
			}
			passes++
			// 1: exit preflight. 2: authentication tree. 3: actual dial.
			if passes != 2 {
				return
			}
			stopFirst()
			select {
			case err := <-firstDone:
				if err != nil {
					t.Fatal(bindingExitFailure)
				}
			case <-ctx.Done():
				t.Fatal(bindingExitFailure)
			}
			_, successorDone = start()
		}
		session, err := client.startHostExitRequest(ctx, original)
		if session != nil {
			session.cancel()
			session.finish()
			t.Fatal(bindingExitFailure)
		}
		var phase *serviceHostExitError
		var cause *serviceControlRefusal
		if !errors.As(err, &phase) || phase.phase != "admission" || phase.accepted || !errors.As(err, &cause) || cause.step != "host_exit:envelope" || passes != 3 || successorDone == nil {
			t.Fatal(bindingExitFailure)
		}
		client.observeRootWalk = nil
		fresh, _, err := client.newHostExitEnvelope(t.Context())
		cell.serviceControlMu.Lock()
		currentListener := cell.serviceControlListener
		cell.serviceControlMu.Unlock()
		if err != nil || currentListener == nil || firstListener == nil || firstListener == currentListener ||
			cell.validateHostExitEnvelope(t.Context(), original, currentListener) == nil || cell.validateHostExitEnvelope(t.Context(), fresh, firstListener) == nil ||
			cell.validateHostExitEnvelope(t.Context(), fresh, currentListener) != nil {
			t.Fatal(bindingExitFailure)
		}
		session, err = client.startHostExitRequest(ctx, fresh)
		if err != nil || session == nil {
			t.Fatal(bindingExitFailure)
		}
		defer session.cancel()
		session.finish()
		select {
		case runErr := <-successorDone:
			if runErr != nil || session.err != nil {
				t.Fatal(bindingExitFailure)
			}
		case <-ctx.Done():
			t.Fatal(bindingExitFailure)
		}
	})
}
