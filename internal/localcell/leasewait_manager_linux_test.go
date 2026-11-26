//go:build linux

package localcell

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Install the lease at the selected admission or manager boundary. Normally
// cancellation follows the observed break. Disable cancels in the command
// itself, so the final ownership read must still make its first open attempt.
func adapterOperationLease(t *testing.T, kind string, mode int, atDisable bool) (context.Context, func(string), func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	holder := leaseHolder(t, kind, mode)
	leaseWait.ceiling = 2 * time.Second
	joined := make(chan struct{})
	var cutAt time.Time
	var armed bool
	hold := func(path string) {
		t.Helper()
		requireServiceLease(t, !armed, "manager fixture tried to lease its target twice")
		armed = true
		leaseTaken(t, holder, path)
		t.Cleanup(func() { <-joined })
		if atDisable {
			cancel()
		}
		go func() {
			at, broken := leaseContextBroken(holder)
			cancel()
			if kind == "released_after_cancel" { // A holder that lets go shortly after the cancellation.
				time.Sleep(50 * time.Millisecond)
				holder.release()
			}
			if !broken {
				at = time.Time{}
			}
			cutAt = at
			close(joined)
		}()
	}
	finish := func() {
		t.Helper()
		returned := time.Now()
		requireServiceLease(t, armed, "manager missed the selected read")
		<-joined
		holder.close()
		requireServiceLease(t, !cutAt.IsZero(), "manager never opened the leased file")
		requireServiceLease(t, returned.Sub(cutAt) < leaseWait.ceiling/2, "manager waited past half the lease ceiling after cancellation")
	}
	return ctx, hold, finish
}

func systemdLeaseEffects(m *systemdFixtureManager) []string {
	var effects []string
	for _, call := range m.calls {
		switch call[2] {
		case "enable", "disable", "daemon-reload":
			effects = append(effects, call[2])
		}
	}
	return effects
}

func requireSystemdLeaseCut(t *testing.T, err error, missing string, primaryRegistrationFailure bool) {
	t.Helper()
	requireServiceLease(t, serviceLeaseCut(err), missing)
	requireServiceLease(t, errors.Is(err, context.Canceled), "manager lost the cancellation cause")
	requireServiceLease(t, errors.Is(err, ErrServiceRegistrationFailed) == primaryRegistrationFailure, "unexpected registration failure on a lease cut")
	for _, refusal := range []error{ErrServiceOwnershipConflict, ErrServiceHostActive, ErrLocked, ErrServiceManagerUnavailable, ErrServiceRemovalFailed, errMaintenanceFenceActive, os.ErrNotExist, unix.EWOULDBLOCK} {
		requireServiceLease(t, !errors.Is(err, refusal), "manager classified the cut as an ordinary refusal")
	}
}

func requireSystemdUnit(t *testing.T, path string, original os.FileInfo) {
	t.Helper()
	current, err := os.Lstat(path)
	requireServiceLease(t, err == nil, "manager lost the unit")
	requireServiceLease(t, os.SameFile(original, current), "manager replaced the unit")
}

func TestLeaseWaitSystemdAdmission(t *testing.T) {
	for _, verb := range []string{"apply", "remove"} {
		for _, site := range []struct {
			name, file string
			mode       int
		}{
			{"owner", ".owner.lock", unix.F_RDLCK}, {"operation", "run/operation.lock", unix.F_RDLCK},
			{"descriptor", "service/descriptor.json", unix.F_WRLCK}, {"manifest", "manifest.json", unix.F_WRLCK},
			{"host", ".service-host.lock", unix.F_RDLCK}, {"maintenance", "journal/maintenance.json", unix.F_WRLCK},
		} {
			if verb == "apply" && site.name == "host" {
				continue // Only removal authorizes under host exclusion.
			}
			t.Run(verb+"/"+site.name, func(t *testing.T) {
				a, d, m := serviceRemovalSystemdFixture(t, true)
				m.active = false
				a.drain = func(context.Context, ServiceDescriptor) error { return nil } // Already stopped.
				serviceRemovalSystemdHostFile(t, m.cell)
				if site.name == "maintenance" {
					serviceMaintenanceFence(t, m.cell, "Valid")
				}
				before := serviceMaintenanceSnapshot(t, m.cell)
				path := a.unitPath(d)
				original, statErr := os.Lstat(path)
				requireServiceLease(t, statErr == nil, "cannot identify the admitted unit")
				kind := "kept"
				if verb == "apply" {
					kind = "retaken"
				}
				ctx, hold, finish := adapterOperationLease(t, kind, site.mode, false)
				hold(m.cell.path(site.file))
				perform := func(ctx context.Context) error {
					if verb == "remove" {
						return a.Remove(ctx, d)
					}
					return a.Apply(ctx, ServicePlan{Spec: d, RequiredPrivilege: "user"})
				}
				err := perform(ctx)
				finish()
				requireSystemdLeaseCut(t, err, "admission lost the lease cut", false)
				requireServiceLease(t, !m.active, "admission started the stopped host")
				requireServiceLease(t, !m.disabled, "admission disabled the registration")
				requireServiceLease(t, len(systemdLeaseEffects(m)) == 0, "admission mutated the manager")
				requireSystemdUnit(t, path, original)
				serviceMaintenanceUnchanged(t, m.cell, before)
				serviceRemovalLocksFree(t, m.cell)
				if site.name == "maintenance" {
					serviceMaintenanceCause(t, perform(t.Context()))
					serviceMaintenanceUnchanged(t, m.cell, before)
					return
				}
				requireServiceLease(t, perform(t.Context()) == nil, "unleased admission retry failed")
			})
		}
	}
}

func TestLeaseWaitSystemdRegistration(t *testing.T) {
	for _, row := range []string{"observe", "apply_owned", "remove_owned", "post_reload", "post_reload_released", "cleanup", "registered"} {
		t.Run(row, func(t *testing.T) {
			a, d, m := systemdFixture(t)
			path := a.unitPath(d)
			plan := ServicePlan{Spec: d, RequiredPrivilege: "user"}
			ctx, hold, finish := adapterOperationLease(t, "kept", unix.F_WRLCK, false)
			var original os.FileInfo
			holdUnit := func() {
				var err error
				original, err = os.Lstat(path)
				requireServiceLease(t, err == nil, "cannot identify the unit at the lease boundary")
				hold(path)
			}
			var effects []string
			switch row {
			case "observe", "apply_owned", "remove_owned":
				if row == "remove_owned" {
					d.DesiredState = "stopped"
					unixServicePersistDescriptor(t, m.cell, d)
					a.drain = func(context.Context, ServiceDescriptor) error { return nil }
				}
				unixSystemdSeed(t, a, d)
				m.loaded = true
				// Target the ownership read under admission, after the earlier
				// optimistic Inspect has succeeded.
				reads := 0
				a.owned = func(name string, _ os.FileInfo) bool {
					if name == path {
						reads++
						if reads == 2 || row == "observe" && reads == 1 {
							holdUnit()
						}
					}
					return true
				}
			case "post_reload", "post_reload_released", "cleanup", "registered":
				m.failReload = row == "cleanup"
				after := "daemon-reload"
				effects = []string{"daemon-reload"}
				if row == "registered" {
					after = "enable"
					effects = append(effects, after)
				}
				inspections := 0
				a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
					if row == "post_reload_released" && original != nil && args[1] == "show" && args[2] == "--property=Version" {
						inspections++
						if inspections == 2 { // Any compensation after the cut would find the lease released.
							finish()
						}
					}
					reply, err := m.command(ctx, binary, args...)
					if args[1] == after && original == nil {
						holdUnit()
					}
					return reply, err
				}
			}
			serviceRemovalSystemdHostFile(t, m.cell)
			before := serviceMaintenanceSnapshot(t, m.cell)
			var err error
			var obs ServiceObservation
			switch row {
			case "observe":
				obs, err = a.Inspect(ctx, d)
			case "remove_owned":
				err = a.Remove(ctx, d)
			default:
				err = a.Apply(ctx, plan)
			}
			finish()
			if row == "observe" {
				requireServiceLease(t, obs == (ServiceObservation{}), "manager cut classified an observation")
			}
			requireSystemdLeaseCut(t, err, "manager lost lease cut", row == "cleanup")
			if row == "cleanup" {
				var joined interface{ Unwrap() []error }
				requireServiceLease(t, errors.As(err, &joined), "cleanup lost its independent manager failure")
				pair := joined.Unwrap()
				requireServiceLease(t, len(pair) == 2, "unexpected number of cleanup errors")
				requireServiceLease(t, errors.Is(pair[0], ErrServiceRegistrationFailed), "cleanup moved the manager failure")
				requireServiceLease(t, strings.Contains(pair[0].Error(), "daemon_reload"), "cleanup lost the manager failure site")
				requireServiceLease(t, serviceLeaseCut(pair[1]), "cleanup moved its lease cut")
			}
			requireSystemdUnit(t, path, original)
			requireServiceLease(t, slices.Equal(systemdLeaseEffects(m), effects), "manager cut changed the effect trace")
			requireServiceLease(t, m.active == (row == "registered"), "manager cut changed active state")
			requireServiceLease(t, m.loaded == (row != "cleanup"), "manager cut changed loaded state")
			requireServiceLease(t, !m.disabled, "manager cut disabled the unit")
			serviceMaintenanceUnchanged(t, m.cell, before)
			a.command, m.failReload = m.command, false
			a.owned = func(string, os.FileInfo) bool { return true }
			switch row {
			case "observe":
				obs, err := a.Inspect(t.Context(), d)
				requireServiceLease(t, err == nil, "inspection retry failed")
				requireServiceLease(t, obs == (ServiceObservation{Available: true, Owned: true, Registered: true}), "inspection retry lost its registration")
			case "remove_owned":
				requireServiceLease(t, a.Remove(t.Context(), d) == nil, "removal retry failed")
				_, err := os.Lstat(path)
				requireServiceLease(t, errors.Is(err, os.ErrNotExist), "removal retry retained the unit")
				return
			default:
				requireServiceLease(t, a.Apply(t.Context(), plan) == nil, "registration retry failed")
				requireServiceLease(t, m.active, "registration retry left the unit inactive")
			}
			requireSystemdUnit(t, path, original)
		})
	}
}

func TestLeaseWaitSystemdRemovalRecoveryOwner(t *testing.T) {
	a, d, m := systemdFixture(t)
	owner, err := serviceOwner()
	requireServiceLease(t, err == nil, "cannot identify the fixture service owner")
	a.uid, d.UID, d.DesiredState = owner, owner, "stopped"
	m.descriptor, m.active, m.loaded = d, true, true
	unixServicePersistDescriptor(t, m.cell, d)
	unixSystemdSeed(t, a, d)
	serviceRemovalSystemdHostFile(t, m.cell)
	a.drain = func(context.Context, ServiceDescriptor) error { return nil }
	before := serviceMaintenanceSnapshot(t, m.cell)
	path := a.unitPath(d)
	original, err := os.Lstat(path)
	requireServiceLease(t, err == nil, "cannot identify the removable unit")
	ctx, hold, finish := adapterOperationLease(t, "kept", unix.F_WRLCK, true)
	a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
		reply, err := m.command(ctx, binary, args...)
		if args[1] == "disable" && err == nil {
			hold(path)
		}
		return reply, err
	}
	err = a.Remove(ctx, d)
	finish()
	requireSystemdLeaseCut(t, err, "post-disable file read lost lease cut", false)
	retained := func() {
		t.Helper()
		requireSystemdUnit(t, path, original)
		requireServiceLease(t, !m.active, "retained registration became active")
		requireServiceLease(t, m.disabled, "retained registration became enabled")
		requireServiceLease(t, m.loaded, "disabled unit was unloaded")
		requireServiceLease(t, slices.Equal(systemdLeaseEffects(m), []string{"disable"}), "retained registration gained a manager effect")
		progress, err := m.cell.readJournal(t.Context())
		requireServiceLease(t, err == nil, "disabled registration journal unreadable")
		requireServiceLease(t, progress.pending == nil, "disabled registration left a pending operation")
		serviceMaintenanceUnchanged(t, m.cell, before)
	}
	retained()
	requireServiceLease(t, m.cell.Repair(t.Context()) == nil, "ordinary repair failed")
	retained()
	requireServiceLease(t, errors.Is(m.cell.Start(t.Context()), ErrServiceDesiredState), "start lost stopped service intent")
	retained()
	a.command = m.command
	a.removeFile = func(name string) error {
		requireSystemdUnit(t, name, original)
		return os.Remove(name)
	}
	requireServiceLease(t, a.Remove(t.Context(), d) == nil, "owner could not retry removal")
	_, err = os.Lstat(path)
	requireServiceLease(t, errors.Is(err, os.ErrNotExist), "owner retry retained the disabled unit")
	requireServiceLease(t, !m.loaded, "owner retry left the manager loaded")
	requireServiceLease(t, a.Remove(t.Context(), d) == nil, "completed removal refused a second retry")
	requireServiceLease(t, slices.Equal(systemdLeaseEffects(m), []string{"disable", "disable", "daemon-reload"}), "owner recovery changed the effect trace")
	serviceMaintenanceUnchanged(t, m.cell, before)
}

func TestServiceAdapterForeignLeaseCut(t *testing.T) {
	refusal := errors.New("fixture refusal")
	foreign := &leaseWaitCut{cause: context.DeadlineExceeded}
	for _, phase := range []string{"live", "ended"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if phase == "ended" {
				cancel()
			}
			for _, err := range []error{foreign, context.Canceled, errors.Join(foreign, context.Canceled)} {
				got := serviceAdapterReadFailure(ctx, err, refusal)
				requireServiceLease(t, got == refusal, "adapter classified a foreign cut or plain cancellation as its operation ending")
			}
		})
	}
}
