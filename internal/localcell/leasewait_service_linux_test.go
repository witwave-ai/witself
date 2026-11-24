//go:build linux

package localcell

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestServiceHostExitPreflight(t *testing.T) {
	for _, row := range []string{"plain_refusal", "leased_secret"} {
		t.Run(row, func(t *testing.T) {
			c, _ := descriptorTestCell(t) // Local runtime, control secret present, no socket.
			var holder *writerLease
			if row == "leased_secret" {
				holder = leaseTaken(t, leaseHolder(t, "kept", unix.F_WRLCK), c.path("service", "control.secret"))
			}

			session, err := c.startHostExitRequest(t.Context(), serviceHostExitEnvelope{})
			if session != nil {
				session.cancel()
				t.Fatal("missing control socket opened a host-exit session")
			}
			if holder != nil {
				var state int
				var leaseErr error
				holder.do(func(fd uintptr) { state, leaseErr = unix.FcntlInt(fd, unix.F_GETLEASE, 0) })
				if leaseErr != nil {
					t.Fatal("cannot inspect the control secret lease after host-exit preflight")
				}
				if state != unix.F_WRLCK {
					t.Fatal("host-exit preflight broke the control secret lease before refusing the endpoint")
				}
			}
			var refusal *serviceControlRefusal
			if !errors.As(err, &refusal) {
				t.Fatal("missing control socket lost the host-exit refusal")
			}
			if refusal.step != "host_exit:envelope" {
				t.Fatal("missing control socket changed the host-exit refusal step")
			}
			if err.Error() != "localcell: service_control_refused: host_exit:envelope" {
				t.Fatal("missing control socket changed the host-exit refusal text")
			}
		})
	}
}

func TestServiceReadLeaseContext(t *testing.T) {
	t.Run("old_boot", serviceOldBootLeases)
	for _, row := range []struct {
		name, file string
		read       func(context.Context, *Cell) error
	}{
		{"descriptor", "descriptor.json", func(ctx context.Context, c *Cell) error { _, err := c.serviceDescriptor(ctx); return err }},
		{"control_secret", "control.secret", func(ctx context.Context, c *Cell) error { _, err := c.readServiceControlSecret(ctx); return err }},
		{"endpoint", "control-endpoint.json", func(ctx context.Context, c *Cell) error {
			_, err := c.readServiceControlEndpointRecord(ctx)
			return err
		}},
	} {
		for _, holder := range []string{"kept", "retaken"} {
			t.Run(row.name+"/"+holder, func(t *testing.T) {
				c, initial := descriptorTestCell(t) // unitCell pins both homes.
				var published serviceControlEndpointRecord
				if row.name == "endpoint" {
					_, published = endpointTestActivation(t, c, initial, func() (string, error) { return "fixture-boot", nil })
				}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if row.read(ctx, c) != nil {
					t.Fatal("unleased service read refused its first attempt")
				}
				l := leaseTaken(t, leaseHolder(t, holder, unix.F_WRLCK), c.path("service", row.file))
				leaseWait.ceiling = 2 * time.Second
				started := time.Now()
				err := row.read(ctx, c)
				elapsed := time.Since(started)
				l.close()
				if cut := (*leaseWaitCut)(nil); !errors.As(err, &cut) {
					t.Fatal("service reader lost lease cut")
				}
				if !errors.Is(err, ctx.Err()) {
					t.Fatal("service reader lost context cause")
				}
				if errors.Is(err, ErrServiceControlRefused) {
					t.Fatal("service reader classified a cut as control refusal")
				}
				if errors.Is(err, ErrServiceOwnershipConflict) {
					t.Fatal("service reader classified a cut as an ownership conflict")
				}
				if errors.Is(err, ErrLocked) {
					t.Fatal("service reader classified a cut as a busy lock")
				}
				if errors.Is(err, unix.EWOULDBLOCK) {
					t.Fatal("service reader classified a cut as would-block")
				}
				if errors.Is(err, os.ErrNotExist) {
					t.Fatal("service reader treated a cut as absence")
				}
				if elapsed >= leaseWait.ceiling/2 {
					t.Fatal("service reader waited past half the lease ceiling")
				}
				if row.name == "endpoint" {
					var site *serviceFailure
					if !errors.As(err, &site) {
						t.Fatal("endpoint cut lost its site wrapper")
					}
					if site.site != "endpoint_read" {
						t.Fatal("endpoint cut lost its read site")
					}
					record, err := c.readServiceControlEndpointRecord(t.Context())
					if err != nil {
						t.Fatal("endpoint was unreadable after a cut")
					}
					if record != published {
						t.Fatal("endpoint read changed its published activation")
					}
				}
				after, err := c.ServiceDescriptor()
				if err != nil {
					t.Fatal("cannot re-read descriptor after cut")
				}
				if after.Revision != initial.Revision {
					t.Fatal("service reader advanced the descriptor revision")
				}
			})
		}
	}
}

func TestLeaseServiceRepairAcknowledgement(t *testing.T) {
	c := serviceCancellationReceiptCell(t) // Short socket path and two private homes.
	initial := prepareSupervisorTest(t, c)
	if c.setServiceFaulted(t.Context(), true) != nil {
		t.Fatal("cannot persist the initial fault")
	}
	l := leaseHolder(t, "kept", unix.F_WRLCK)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.cfg.Failpoint = func(op, step string) error {
		if op == "stop" && step == "stop" {
			leaseTaken(t, l, c.path("service", "descriptor.json"))
			cancel()
		}
		return nil
	}
	err := c.RepairService(ctx)
	l.close()
	c.cfg.Failpoint = nil
	if cut := (*leaseWaitCut)(nil); !errors.As(err, &cut) {
		t.Fatal("acknowledgement lost its lease cut")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("acknowledgement lost the cancellation cause")
	}
	check := func(c *Cell, faulted bool, repairRevision uint64) {
		t.Helper()
		d, err := c.ServiceDescriptor()
		if err != nil {
			t.Fatal("cannot read the durable acknowledgement state")
		}
		if d.DesiredState != "stopped" {
			t.Fatal("acknowledgement lost stopped intent")
		}
		if d.Faulted != faulted {
			t.Fatal("acknowledgement changed the wrong fault state")
		}
		if d.RepairRevision != repairRevision {
			t.Fatal("acknowledgement has the wrong repair revision")
		}
	}
	check(c, true, initial.RepairRevision)
	if !slices.Equal(leaseJournalRecords(c), []string{"operation:started", "stop:started", "stop:done", "operation:done"}) {
		t.Fatal("stop journal did not commit all four records")
	}
	assertStoppedTickJournal(t, c)
	fresh := publicationClient(t, c)
	if fresh.Repair(t.Context()) != nil {
		t.Fatal("ordinary repair failed after the interrupted acknowledgement")
	}
	check(fresh, true, initial.RepairRevision)
	host := serviceWorkerOnCell(t, fresh, "", false)
	bound, stop := context.WithTimeout(t.Context(), serviceControlBudget(fresh.cfg.Timeout, 3))
	defer stop()
	for fresh.validateServiceControlEndpoint(t.Context()) != nil {
		select {
		case <-host.done:
			t.Fatal("host exited before publishing discovery")
		case <-bound.Done():
			t.Fatal("host did not publish discovery within its startup budget")
		case <-time.After(time.Millisecond):
		}
	}
	status := serviceWorkerSettled(bound, t, c.Control)
	if status.State != "faulted" {
		t.Fatal("next host start lost the circuit fault")
	}
	if status.Reason != "service_circuit_open" {
		t.Fatal("next host start lost the circuit reason")
	}
	if slices.Contains(host.recorded(), "start") {
		t.Fatal("next host start launched a child behind the held fault")
	}
	host.cancel()
	if <-host.done != nil {
		t.Fatal("faulted host did not retire cleanly")
	}
	check(c, true, initial.RepairRevision)
	if c.RepairService(t.Context()) != nil {
		t.Fatal("explicit repair retry failed")
	}
	check(c, false, initial.RepairRevision+1)
}

func requireServiceLease(t *testing.T, ok bool, message string) {
	t.Helper()
	if !ok {
		t.Fatal(message)
	}
}

func TestServicePreparationLease(t *testing.T) {
	for _, row := range []string{"secret", "descriptor", "fault_receipt"} {
		t.Run(row, func(t *testing.T) {
			c, d := descriptorTestCell(t)
			descriptor := c.path("service", "descriptor.json")
			info, err := os.Stat(descriptor)
			requireServiceLease(t, err == nil, "cannot identify the original descriptor")
			secret, err := c.serviceControlSecret(t.Context())
			requireServiceLease(t, err == nil, "cannot read the original control secret")
			path := descriptor
			if row == "secret" {
				requireServiceLease(t, os.Remove(descriptor) == nil, "cannot remove the descriptor before provisioning")
				path = c.path("service", "control.secret")
			}
			holder := leaseTaken(t, leaseHolder(t, "kept", unix.F_WRLCK), path)
			leaseWait.ceiling = 2 * time.Second
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			var receipt serviceFaultCommit
			go func() {
				if row == "fault_receipt" {
					done <- c.commitServiceFault(ctx, true, &receipt)
					return
				}
				done <- c.PrepareService(ctx, d)
			}()
			_, broken := leaseContextBroken(holder)
			cancel()
			err = <-done
			holder.close()
			requireServiceLease(t, broken, "operation missed the leased service record")
			requireServiceLease(t, receipt == (serviceFaultCommit{}), "cut descriptor read issued a fault receipt")
			after, statErr := os.Stat(descriptor)
			if row == "secret" {
				requireServiceLease(t, errors.Is(statErr, os.ErrNotExist), "secret cut provisioned a descriptor")
			} else {
				requireServiceLease(t, statErr == nil, "descriptor disappeared behind a cut")
				requireServiceLease(t, os.SameFile(info, after), "descriptor was rewritten behind a cut")
			}
			current, readErr := c.serviceControlSecret(t.Context())
			requireServiceLease(t, readErr == nil, "control secret unreadable after cut")
			requireServiceLease(t, current == secret, "cut replaced the control secret")
			_, statErr = os.Lstat(c.path("service", "transition.json"))
			requireServiceLease(t, errors.Is(statErr, os.ErrNotExist), "cut left a pending descriptor transition")
			requireServiceLease(t, serviceLeaseCut(err), "operation hid its record read cut")
			requireServiceLease(t, errors.Is(err, context.Canceled), "record read cut lost its cancellation")
		})
	}
}

func TestServiceExitLease(t *testing.T) {
	for _, row := range []string{"poll", "session", "drain", "envelope"} {
		t.Run(row, func(t *testing.T) {
			c := serviceCancellationReceiptCell(t)
			lock, err := c.acquireServiceLease(prepareSupervisorTest(t, c))
			requireServiceLease(t, err == nil, "cannot acquire the host lease for exit")
			defer func() { _ = lock.Close() }()
			envelope, lease, err := c.newHostExitEnvelope()
			requireServiceLease(t, err == nil, "cannot identify the host before exit")
			if row == "envelope" {
				listener, err := c.listenServiceControl(t.Context())
				requireServiceLease(t, err == nil, "cannot prepare the exit endpoint")
				defer func() { _ = listener.Close() }()
			}
			holder := leaseTaken(t, leaseHolder(t, "kept", unix.F_WRLCK), c.path("service", "control.secret"))
			leaseWait.ceiling = 2 * time.Second
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				switch row {
				case "drain":
					done <- c.exitServiceHostForRemoval(ctx)
				case "envelope":
					_, err := c.startHostExitRequest(ctx, envelope)
					done <- err
				default:
					done <- c.waitServiceHostExit(ctx, lease, &unixServiceExitHost{lease: lease})
				}
			}()
			_, broken := leaseContextBroken(holder)
			if row == "poll" {
				time.Sleep(300 * time.Millisecond) // Longer than one observation, inside the lease ceiling.
				holder.release()
				time.Sleep(20 * time.Millisecond)
				requireServiceLease(t, lock.Close() == nil, "host did not release its exit fence")
			} else {
				cancel()
			}
			err = <-done
			requireServiceLease(t, broken, "host exit missed the leased control secret")
			if row == "poll" {
				requireServiceLease(t, err == nil, "one observation deadline aborted an accepted exit")
				return
			}
			requireServiceLease(t, serviceLeaseCut(err), "host exit replaced the secret cut")
			requireServiceLease(t, errors.Is(err, context.Canceled), "exit cut lost its cancellation")
			requireServiceLease(t, !errors.Is(err, ErrServiceControlRefused), "exit cut acquired a control refusal")
			if row == "session" {
				var exit *serviceHostExitError
				requireServiceLease(t, errors.As(err, &exit), "session cut lost its exit diagnostic")
				requireServiceLease(t, exit.phase == "observe", "session cut lost its observation phase")
				requireServiceLease(t, exit.accepted, "session cut lost its accepted disposition")
			}
		})
	}
}

func TestServiceEndpointLeaseWait(t *testing.T) {
	for _, row := range []struct{ operation, file, site string }{
		{"complete", "descriptor.json", "endpoint_descriptor"}, {"complete", "lease.json", "endpoint_lease"},
		{"publish", "descriptor.json", "endpoint_descriptor"}, {"publish", "lease.json", "endpoint_lease"},
		{"read", "descriptor.json", "endpoint_descriptor"}, {"read", "lease.json", "endpoint_lease"},
		{"read", "control-endpoint.json", "endpoint_read"}, {"retire", "control-endpoint.json", "endpoint_read"},
	} {
		t.Run(row.operation+"/"+row.file, func(t *testing.T) {
			c, d := descriptorTestCell(t)
			boot := func() (string, error) { return "fixture-boot", nil }
			lock, err := c.acquireServiceLeaseForPlatform(d, boot, "windows")
			requireServiceLease(t, err == nil, "cannot acquire provisional fixture lease")
			defer func() { _ = lock.Close() }()
			name, err := newServiceControlPipeName(d.InstallationID)
			requireServiceLease(t, err == nil, "cannot name fixture listener")
			if row.operation != "complete" {
				requireServiceLease(t, c.completeServiceLease(t.Context(), d, name, boot) == nil, "cannot complete fixture lease")
			}
			var owned serviceControlEndpointRecord
			published := row.operation == "read" || row.operation == "retire"
			if published {
				owned, err = c.publishServiceControlEndpointWithBoot(t.Context(), name, boot)
				requireServiceLease(t, err == nil, "cannot publish fixture endpoint")
			}
			leasePath := c.path("service", "lease.json")
			var before, after serviceLeaseRecord
			requireServiceLease(t, readServiceRecordJSON(t.Context(), leasePath, &before) == nil, "cannot read original host lease")
			call := func(ctx context.Context) error {
				switch row.operation {
				case "complete":
					return c.completeServiceLease(ctx, d, name, boot)
				case "publish":
					_, err := c.publishServiceControlEndpointWithBoot(ctx, name, boot)
					return err
				case "retire":
					return c.removeOwnedServiceControlEndpoint(ctx, owned)
				default:
					_, err := c.readServiceControlEndpointWithBoot(ctx, boot)
					return err
				}
			}
			holder := leaseTaken(t, leaseHolder(t, "kept", unix.F_WRLCK), c.path("service", row.file))
			leaseWait.ceiling = 2 * time.Second
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- call(ctx) }()
			_, broken := leaseContextBroken(holder)
			cancel()
			err = <-done
			holder.close()
			requireServiceLease(t, broken, "endpoint operation never reached the leased file")
			requireServiceLease(t, serviceLeaseCut(err), "endpoint operation lost lease cut")
			requireServiceLease(t, errors.Is(err, context.Canceled), "endpoint operation lost cancellation")
			requireServiceLease(t, !errors.Is(err, ErrServiceControlRefused), "endpoint operation classified the cut as a refusal")
			var failure *serviceFailure
			requireServiceLease(t, errors.As(err, &failure), "endpoint cut lost its site")
			requireServiceLease(t, failure.site == row.site, "endpoint cut received the wrong site")
			requireServiceLease(t, readServiceRecordJSON(t.Context(), leasePath, &after) == nil, "cannot read host lease after cut")
			requireServiceLease(t, after == before, "interrupted validation replaced the lease")
			if published {
				got, err := c.readServiceControlEndpointWithBoot(t.Context(), boot)
				requireServiceLease(t, err == nil, "interrupted validation removed discovery")
				requireServiceLease(t, got == owned, "interrupted validation replaced discovery")
			} else {
				_, err := os.Lstat(c.path("service", "control-endpoint.json"))
				requireServiceLease(t, errors.Is(err, os.ErrNotExist), "interrupted validation published discovery")
			}
			requireServiceLease(t, call(ctx) == nil, "unleased retry failed at its first attempt")
		})
	}
}
