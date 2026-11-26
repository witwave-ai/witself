//go:build linux

package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// adapterDrainFixture is a registered adapter whose drain cell's descriptor
// passes the plain descriptor reader, with no drain hook: Remove reaches the
// real drain (the Windows task through its removal-cell seam, on Linux).
type adapterDrainFixture struct {
	cell       *Cell
	remove     func(context.Context) error
	artifact   string
	registered func() bool
	effects    func() []string
}

func newAdapterDrainFixture(t *testing.T, family string) adapterDrainFixture {
	t.Helper()
	owner, err := serviceOwner()
	requireServiceLease(t, err == nil, "cannot identify the drain fixture owner")
	if family == "systemd" {
		a, d, m := serviceRemovalSystemdFixture(t, true)
		d.UID, a.uid, a.drain = owner, owner, nil
		m.descriptor = d
		unixServicePersistDescriptor(t, m.cell, d)
		unixSystemdSeed(t, a, d)
		serviceRemovalSystemdHostFile(t, m.cell)
		return adapterDrainFixture{m.cell, func(ctx context.Context) error { return a.Remove(ctx, d) }, a.unitPath(d),
			func() bool { return m.loaded }, func() []string { return systemdLeaseEffects(m) }}
	}
	if family == "windows" { // As TestServiceAdapterRemovalRealDrain.
		f := newServiceRemovalFixture(t, "Windows")
		c, _ := descriptorTestCell(t)
		serviceRemovalSystemdHostFile(t, c)
		f.win.a.removalCell = func(ServiceDescriptor) (*Cell, error) { return c, nil }
		f.setDrain(nil)
		f.win.drained = true
		return adapterDrainFixture{c, func(ctx context.Context) error { return f.win.a.Remove(ctx, f.d) }, f.win.a.xmlPath(f.d), f.registered, f.destructive}
	}
	if owner == "0" {
		t.Skip("launchd refuses the root GUI domain") // As TestServiceAdapterRemovalRealDrain.
	}
	f := newServiceRemovalFixture(t, "Launchd")
	f.d.UID, f.launch.uid = owner, owner
	f.lm.d = f.d
	unixServicePersistDescriptor(t, f.cell, f.d)
	f.setDrain(nil)
	return adapterDrainFixture{f.cell, func(ctx context.Context) error { return f.launch.Remove(ctx, f.d) }, f.launch.plistPath(f.d),
		f.registered, f.destructive}
}

// The removal drain's lease waits end with the remover's own context. Remove
// reaches the real drain with the stable host lock leased. Cancelled at the
// kernel's break, with a holder that keeps the lease (kept) or lets go 50 ms
// later (released), it returns the typed cut within half the lease ceiling;
// the parent returned the fixed removal refusal there. The state checks equal
// the parent's for both; the timing check is apart from them and is what fails
// a drain detached from the context (M34) with the kept holder. Live, a lease
// kept to the ceiling keeps the parent's fixed refusal; a holder that lets go
// at the break lets the removal complete. The live and lets_go leaves are
// regression pins: they pass on the parent's production code too.
func TestLeaseWaitAdapterRemovalDrain(t *testing.T) {
	for _, family := range []string{"systemd", "launchd", "windows"} {
		for _, leaf := range []string{"kept", "released", "live", "lets_go"} {
			t.Run(family+"/"+leaf, func(t *testing.T) {
				f := newAdapterDrainFixture(t, family)
				lock := f.cell.path(".service-host.lock")
				before := serviceMaintenanceSnapshot(t, f.cell)
				original, err := os.Lstat(f.artifact)
				requireServiceLease(t, err == nil, "cannot identify the registered artifact")
				content, err := os.ReadFile(f.artifact)
				requireServiceLease(t, err == nil, "cannot read the registered artifact")
				switch leaf {
				case "kept", "released":
					kind := map[string]string{"kept": "kept", "released": "released_after_cancel"}[leaf]
					ctx, hold, finish := adapterOperationLease(t, kind, unix.F_RDLCK, false)
					hold(lock)
					err = f.remove(ctx)
					finish()
					requireSystemdLeaseCut(t, err, "removal drain lost its lease cut", false)
				case "live":
					holder := leaseTaken(t, leaseHolder(t, "kept", unix.F_RDLCK), lock)
					started := time.Now()
					err = f.remove(t.Context())
					waited := time.Since(started)
					holder.close()
					if family == "windows" {
						windowsTaskMaintenanceDiagnostic(t, err, ErrServiceRemovalFailed, "remove:host_exit", "host_exit_failed")
					} else {
						unixServiceRequireError(t, err, ErrServiceRemovalFailed, "remove:host_exit", "host_exit_failed")
					}
					requireServiceLease(t, !serviceLeaseCut(err), "a live removal reported a lease cut")
					requireServiceLease(t, waited >= leaseKeptAfter, "a live removal drain returned before the lease ceiling")
				case "lets_go":
					holder := leaseTaken(t, leaseHolder(t, "lets_go", unix.F_RDLCK), lock)
					err = f.remove(t.Context())
					requireServiceLease(t, holder.broken(), "removal drain never opened the leased host lock")
					holder.close()
					requireServiceLease(t, err == nil, "removal drain did not complete after the holder let go")
					requireServiceLease(t, !f.registered(), "completed removal kept the registration")
					return
				}
				requireServiceLease(t, f.registered(), "drain refusal changed the registration")
				requireServiceLease(t, len(f.effects()) == 0, "drain refusal issued a manager effect")
				current, err := os.Lstat(f.artifact)
				requireServiceLease(t, err == nil && os.SameFile(original, current), "drain refusal replaced the registered artifact")
				after, err := os.ReadFile(f.artifact)
				requireServiceLease(t, err == nil && bytes.Equal(content, after), "drain refusal changed the registered artifact")
				serviceMaintenanceUnchanged(t, f.cell, before)
				serviceRemovalLocksFree(t, f.cell)
				requireServiceLease(t, f.remove(t.Context()) == nil, "removal retry after the drain refusal failed")
				requireServiceLease(t, !f.registered(), "removal retry kept the registration")
			})
		}
	}
}

// Only the operation's own cut leaves the drain typed. A failure that is not a
// cut, a foreign cut with the operation live or ended, a plain cancellation and
// a joined error whose sibling alone carries the cause keep each adapter's fixed
// removal refusal with its class and site; in every case nothing after the
// drain runs (the registration and its artifacts are unchanged). Every leaf but
// own is a regression pin: it passes on the parent's production code too.
func TestServiceAdapterRemovalDrainCut(t *testing.T) {
	foreign := &leaseWaitCut{cause: context.DeadlineExceeded}
	for _, family := range []string{"systemd", "Launchd", "Windows"} {
		for _, leaf := range []string{"own", "refused", "foreign_live", "foreign_ended", "canceled", "joined"} {
			t.Run(family+"/"+leaf, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				returned := map[string]error{"own": &leaseWaitCut{cause: context.Canceled}, "refused": errors.New("fixture drain failure"),
					"foreign_live": foreign, "foreign_ended": foreign, "canceled": context.Canceled, "joined": errors.Join(foreign, context.Canceled)}[leaf]
				var remove func() error
				var registered func() bool
				var artifacts func() map[string]serviceMaintenanceEntry
				drain := func(context.Context, ServiceDescriptor) error {
					if leaf != "refused" && leaf != "foreign_live" {
						cancel() // The operation ends during its drain.
					}
					return returned
				}
				if family == "systemd" {
					a, d, m := serviceRemovalSystemdFixture(t, true)
					a.drain = drain
					remove, registered = func() error { return a.Remove(ctx, d) }, func() bool { return m.loaded && len(systemdLeaseEffects(m)) == 0 }
					artifacts = func() map[string]serviceMaintenanceEntry { return unixServiceArtifactSnapshot(t, a.unitDir) }
				} else {
					f := newServiceRemovalFixture(t, family)
					f.setDrain(drain)
					remove, artifacts = func() error {
						if f.launch != nil {
							return f.launch.Remove(ctx, f.d)
						}
						return f.win.a.Remove(ctx, f.d)
					}, f.artifacts
					registered = func() bool { return f.registered() && len(f.destructive()) == 0 }
				}
				before := artifacts()
				err := remove()
				if leaf == "own" {
					requireServiceLease(t, err == returned, "the operation's own drain cut was not returned as it is")
				} else if family == "Windows" {
					windowsTaskMaintenanceDiagnostic(t, err, ErrServiceRemovalFailed, "remove:host_exit", "host_exit_failed")
				} else {
					unixServiceRequireError(t, err, ErrServiceRemovalFailed, "remove:host_exit", "host_exit_failed")
				}
				requireServiceLease(t, leaf == "own" || !serviceLeaseCut(err), "a drain failure that is not the operation's cut stayed typed")
				requireServiceLease(t, registered(), "a drain failure changed the registration")
				requireServiceLease(t, reflect.DeepEqual(before, artifacts()), "a drain failure changed the registration artifacts")
			})
		}
	}
}
