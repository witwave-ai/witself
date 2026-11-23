package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
)

const bindingConsumerFailure = "consumer changed its admission or exit proof"
const bindingExitFailure = "exit reached a different listener activation"

// Requests run through both transport passes, the real server's validation and
// its ACK/terminal handler. The command queue lets the test select a refusal at
// an exact boundary without running a child, manager or registration command.
func TestServiceBindingConsumer(t *testing.T) {
	for _, row := range []string{"remove_stop", "exit_before_ack", "exit_after_ack"} {
		t.Run(row, func(t *testing.T) {
			host := serviceCancellationReceiptCell(t)
			d := prepareSupervisorTest(t, host)
			lock, err := host.acquireServiceLease(d)
			if err != nil {
				t.Fatal(bindingConsumerFailure)
			}
			defer func() { _ = lock.Close() }()
			commands := make(chan serviceCommand)
			stop, err := host.serveServiceControl(commands)
			if err != nil {
				t.Fatal(bindingConsumerFailure)
			}
			defer stop()
			client, err := New(host.cfg)
			if err != nil {
				t.Fatal(bindingConsumerFailure)
			}
			writer, err := New(host.cfg)
			if err != nil {
				t.Fatal(bindingConsumerFailure)
			}
			ctx, cancel := context.WithTimeout(t.Context(), host.cfg.Timeout)
			defer cancel()
			before, err := os.ReadFile(host.path("manifest.json"))
			if err != nil {
				t.Fatal(bindingConsumerFailure)
			}
			var publicationErr error
			published := false
			if row == "exit_before_ack" {
				// Advance state after the original target is retained and
				// before the actual dial. Windows performs two additional
				// descriptor walks (retained executable proof and preflight).
				walks := 0
				selected := 2
				if runtime.GOOS == "windows" {
					selected = 4
				}
				client.observeRootWalk = func(before bool) {
					if !before {
						walks++
						if walks == selected {
							publicationErr = writer.SetServiceDesiredState(ctx, "maintenance")
							published = true
						}
					}
				}
			}
			done := make(chan struct{})
			var status ServiceStatus
			var callErr error
			go func() {
				defer close(done)
				if row == "exit_before_ack" {
					status, callErr = client.ExitServiceHost(ctx)
				} else {
					callErr = client.Remove(ctx, RemoveOptions{Purge: true})
				}
			}()
			defer func() { cancel(); <-done }()
			next := func(action string) serviceCommand {
				t.Helper()
				select {
				case command := <-commands:
					if command.action != action {
						t.Fatal(bindingConsumerFailure)
					}
					return command
				case <-done:
				case <-ctx.Done():
				}
				t.Fatal(bindingConsumerFailure)
				return serviceCommand{}
			}
			if row != "exit_before_ack" {
				command := next("stop")
				if row == "remove_stop" {
					// Publish ordinary maintenance while the live stop is busy.
					if writer.SetServiceDesiredState(ctx, "maintenance") != nil {
						t.Fatal(bindingConsumerFailure)
					}
					command.reply <- serviceReply{err: errServiceControlDeferred}
					command = next("stop")
				}
				if writer.SetServiceDesiredState(ctx, "stopped") != nil {
					t.Fatal(bindingConsumerFailure)
				}
				command.reply <- serviceReply{}
			}
			exit := next("exit-host")
			if row == "exit_after_ack" {
				exit.reply <- serviceReply{}
				select {
				case <-exit.delivered:
				case <-ctx.Done():
					t.Fatal(bindingConsumerFailure)
				}
				if writer.setServiceFaulted(ctx, true) != nil {
					t.Fatal(bindingConsumerFailure)
				}
				// While this exact process and lease remain held, receipt of the
				// accepted line can never authorize removal. The observer sees
				// the refused drain through its own fresh admission.
				observe := next("observe")
				observe.reply <- serviceReply{status: ServiceStatus{State: "faulted", HostExit: "refused"}}
				exit.completed <- "refused"
			} else {

				exit.reply <- serviceReply{err: hostExitRefused("drain")}
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal(bindingConsumerFailure)
			}
			after, err := os.ReadFile(host.path("manifest.json"))
			current, dErr := writer.ServiceDescriptor()
			if err != nil || dErr != nil || !bytes.Equal(before, after) || !errors.Is(callErr, ErrServiceControlRefused) ||
				status.HostExit != "" || current.Revision <= d.Revision || (row == "exit_before_ack" && (!published || publicationErr != nil)) || (row != "exit_before_ack" && current.DesiredState != "stopped") {
				t.Fatal(bindingConsumerFailure)
			}
		})
	}
	t.Run("manager_inspect", func(t *testing.T) {
		f := newWindowsTaskFake(t)
		f.apply()
		c, _ := descriptorTestCell(t)
		old, err := c.ServiceDescriptor()
		if err != nil {
			t.Fatal(bindingConsumerFailure)
		}
		// The fixture supplies a Windows-shaped immutable descriptor on all OSes.
		// Its operational state comes from the real publication at preflight.
		f.a.readDescriptor = func(ServiceDescriptor) (ServiceDescriptor, error) {
			d, err := c.ServiceDescriptor()
			current := f.d
			current.DesiredState, current.Revision = d.DesiredState, d.Revision
			return current, err
		}
		if c.SetServiceDesiredState(t.Context(), "maintenance") != nil {
			t.Fatal(bindingConsumerFailure)
		}
		current, err := c.ServiceDescriptor()
		if err != nil || current.Revision <= old.Revision {
			t.Fatal(bindingConsumerFailure)
		}
		queries := f.count("/Query")
		owned, err := f.a.Inspect(t.Context(), f.d)
		if err != nil || !owned.Owned || !owned.Registered || !owned.Available {
			t.Fatal(bindingConsumerFailure)
		}
		f.untyped = true
		unavailable, err := f.a.Inspect(t.Context(), f.d)
		if !errors.Is(err, ErrServiceManagerUnavailable) || unavailable.Available || unavailable.Owned || f.count("/Query") != queries+2 ||
			f.count("/Create") != 1 || f.count("/Delete") != 0 || f.count("/Run") != 0 {
			t.Fatal(bindingConsumerFailure)
		}
	})
	bindingOriginalExitActivation(t)
}
