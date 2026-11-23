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

func TestServiceReadLeaseContext(t *testing.T) {
	for _, row := range []struct {
		name, file string
		read       func(context.Context, *Cell) error
	}{
		{"descriptor", "descriptor.json", func(ctx context.Context, c *Cell) error { _, err := c.serviceDescriptor(ctx); return err }},
		{"control_secret", "control.secret", func(ctx context.Context, c *Cell) error { _, err := c.readServiceControlSecret(ctx); return err }},
	} {
		for _, holder := range []string{"kept", "retaken"} {
			t.Run(row.name+"/"+holder, func(t *testing.T) {
				c, initial := descriptorTestCell(t) // unitCell pins both homes.
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
	for fresh.validateServiceControlEndpoint() != nil {
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
