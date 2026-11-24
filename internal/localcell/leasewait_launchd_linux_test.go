//go:build linux

package localcell

import (
	"context"
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// Run the portable adapter with a synthetic launchd manager on the platform
// with waiting opens. The plist itself still has its original opener.
func TestLeaseWaitLaunchdAdmission(t *testing.T) {
	for _, verb := range []string{"apply", "remove"} {
		for _, site := range []struct {
			name, file string
			mode       int
		}{
			{"owner", ".owner.lock", unix.F_RDLCK},
			{"descriptor", "service/descriptor.json", unix.F_WRLCK},
			{"host", ".service-host.lock", unix.F_RDLCK},
			{"maintenance", "journal/maintenance.json", unix.F_WRLCK},
		} {
			if verb == "apply" && site.name == "host" {
				continue
			}
			t.Run(verb+"/"+site.name, func(t *testing.T) {
				f := newServiceRemovalFixture(t, "Launchd")
				f.d.DesiredState = "stopped"
				f.setDrain(func(context.Context, ServiceDescriptor) error { return nil })
				if verb == "apply" {
					f.absent() // An interrupted bootstrap, eligible for explicit Apply.
				}
				if site.name == "maintenance" {
					serviceMaintenanceFence(t, f.cell, "Valid")
				}
				cellBefore, artifacts := serviceMaintenanceSnapshot(t, f.cell), f.artifacts()
				path := f.launch.plistPath(f.d)
				original, err := os.Lstat(path)
				requireServiceLease(t, err == nil, "cannot identify the original plist")
				perform := func(ctx context.Context) error {
					if verb == "remove" {
						return f.launch.Remove(ctx, f.d)
					}
					return f.launch.Apply(ctx, ServicePlan{Spec: f.d, RequiredPrivilege: "user"})
				}
				ctx, hold, finish := adapterOperationLease(t, "kept", site.mode, false)
				hold(f.cell.path(site.file))
				err = perform(ctx)
				finish()
				requireServiceLease(t, serviceLeaseCut(err), "launchd admission lost lease cut")
				requireServiceLease(t, errors.Is(err, ctx.Err()), "launchd admission lost cancellation")
				for _, refusal := range []error{ErrServiceOwnershipConflict, ErrServiceHostActive, ErrServiceRemovalFailed, ErrLocked, errMaintenanceFenceActive} {
					requireServiceLease(t, !errors.Is(err, refusal), "launchd admission misclassified a lease cut")
				}
				unixLaunchdUnchanged(t, f.launch, f.lm, cellBefore, artifacts)
				current, err := os.Lstat(path)
				requireServiceLease(t, err == nil, "launchd admission lost the plist")
				requireServiceLease(t, os.SameFile(original, current), "launchd admission replaced the plist")
				requireServiceLease(t, f.registered() == (verb == "remove"), "launchd admission changed the loaded registration")
				serviceRemovalLocksFree(t, f.cell)
				if site.name == "maintenance" {
					serviceMaintenanceCause(t, perform(t.Context()))
					return
				}
				requireServiceLease(t, perform(t.Context()) == nil, "launchd admission retry failed")
				requireServiceLease(t, f.registered() == (verb == "apply"), "launchd retry did not finish the requested operation")
			})
		}
	}
}
