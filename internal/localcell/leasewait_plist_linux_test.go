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

// Every launchd plist read, run with a synthetic manager on the platform with
// waiting opens, the lease taken at the target read (the target-th ownership
// check of the plist, just before its open). kept and released cancel at the
// kernel's break (released lets go 50 ms later): the cut is returned within
// half the lease ceiling with the state as the parent left it for a cancelled
// manager command, never an observation, absence or plist_changed. live keeps
// the lease to the ceiling and owner refuses the plist's owner at that read:
// both are regression pins of the parent's refusal and its state, and pass at
// the parent. Every leaf then retries the operation, and an Apply a Remove.
func TestLeaseWaitLaunchdRegistration(t *testing.T) {
	for _, leaf := range []struct {
		name, op, setup, step string
		target                int
		loaded, cleans        bool
		effects               []string
	}{
		{"observe", "inspect", "registered", "", 1, true, false, nil},
		{"apply_identity", "apply", "unloaded", "", 3, false, false, nil},
		{"pre_disabled", "apply", "fresh", "pre_disabled", 1, false, true, nil},
		{"post_disabled", "apply", "fresh", "post_disabled", 3, false, true, nil},
		{"pre_bootstrap", "apply", "fresh", "pre_bootstrap", 5, false, false, []string{"enable"}},
		{"registered", "apply", "fresh", "post_bootstrap", 7, true, false, []string{"enable", "bootstrap"}},
		{"cleanup", "apply", "fresh", "inspect_disabled", 3, false, true, nil},
		{"remove_owned", "remove", "registered", "", 5, true, false, nil},
		{"remove_recovery", "remove", "unloaded", "", 5, false, false, nil},
	} {
		for _, holder := range []string{"kept", "released", "live", "owner"} {
			t.Run(leaf.name+"/"+holder, func(t *testing.T) {
				f := newServiceRemovalFixture(t, "Launchd")
				f.d.DesiredState = "stopped"
				f.setDrain(func(context.Context, ServiceDescriptor) error { return nil })
				path, want := f.launch.plistPath(f.d), launchdPlistBytes(t, f.d)
				if leaf.setup != "registered" {
					f.absent()
				}
				if leaf.setup == "fresh" {
					requireServiceLease(t, os.Remove(path) == nil, "cannot clear the fixture plist")
					f.lm.enabled[f.launch.target(f.d)] = false // A disabled override: Apply enables before bootstrap.
				}
				before := serviceMaintenanceSnapshot(t, f.cell)
				ctx, live := t.Context(), (*writerLease)(nil)
				var hold func(string)
				finish := func() {}
				switch holder {
				case "kept", "released":
					ctx, hold, finish = adapterOperationLease(t, map[string]string{"kept": "kept", "released": "released_after_cancel"}[holder], unix.F_WRLCK, false)
				case "live":
					live = leaseHolder(t, "kept", unix.F_WRLCK)
				}
				var identity os.FileInfo
				reads := 0
				f.launch.owned = func(name string, _ os.FileInfo) bool {
					if name != path {
						return true
					}
					if reads++; reads != leaf.target {
						return true
					}
					var err error
					identity, err = os.Lstat(path)
					requireServiceLease(t, err == nil, "cannot identify the plist at the target read")
					switch holder {
					case "owner":
						return false
					case "live":
						leaseTaken(t, live, path)
					default:
						hold(path)
					}
					return true
				}
				failDisabled := leaf.name == "cleanup"
				f.launch.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
					if failDisabled && len(args) > 0 && args[0] == "print-disabled" {
						return nil, errors.New("fixture print-disabled failure")
					}
					return f.lm.command(ctx, binary, args...)
				}
				var obs ServiceObservation
				var err error
				started := time.Now()
				switch leaf.op {
				case "inspect":
					obs, err = f.launch.Inspect(ctx, f.d)
				case "apply":
					err = f.launch.Apply(ctx, ServicePlan{Spec: f.d, RequiredPrivilege: "user"})
				default:
					err = f.launch.Remove(ctx, f.d)
				}
				waited := time.Since(started)
				finish()
				if live != nil {
					live.close()
				}
				requireServiceLease(t, reads >= leaf.target, "the operation missed the target plist read")
				site := map[string]string{"live": "plist_file:open", "owner": "plist_file:owner"}[holder]
				if site == "" {
					requireSystemdLeaseCut(t, err, "plist read lost its lease cut", leaf.step != "")
					if leaf.op == "inspect" {
						requireServiceLease(t, obs == (ServiceObservation{}), "manager cut classified an observation")
					}
				} else {
					requireServiceLease(t, !serviceLeaseCut(err), "an ordinary plist refusal reported a lease cut")
					requireServiceLease(t, errors.Is(err, ErrServiceOwnershipConflict) || leaf.name == "cleanup", "an ordinary plist refusal lost its conflict class")
					switch {
					case leaf.op == "remove":
						unixServiceRequireError(t, err, ErrServiceOwnershipConflict, "remove:ownership", "plist_changed")
					case leaf.name != "cleanup":
						requireServiceLease(t, strings.Contains(err.Error(), site+":"), "an ordinary plist refusal lost its site")
					}
					if leaf.op == "inspect" {
						requireServiceLease(t, obs == ServiceObservation{Available: true, Registered: true, Reason: "service_ownership_conflict"}, "an ordinary plist refusal lost its conflict observation")
					}
				}
				if leaf.step != "" {
					requireServiceLease(t, errors.Is(err, ErrServiceRegistrationFailed) && strings.Contains(err.Error(), ": "+leaf.step+": "), "plist read lost the registration refusal of its step")
				}
				if leaf.name == "cleanup" && site == "" { // errors.Join(registration failure, cleanup cut), in that order.
					joined, ok := err.(interface{ Unwrap() []error })
					requireServiceLease(t, ok && len(joined.Unwrap()) == 2 && joined.Unwrap()[0] != ErrServiceRegistrationFailed, "the cleanup cut was not kept beside the registration failure")
					requireServiceLease(t, strings.Contains(joined.Unwrap()[0].Error(), ": inspect_disabled: "), "cleanup moved the registration failure")
					requireServiceLease(t, serviceLeaseCut(joined.Unwrap()[1]), "cleanup moved its lease cut")
				}
				if holder == "live" {
					requireServiceLease(t, waited >= leaseKeptAfter, "a live plist read returned before the lease ceiling")
				}
				requireServiceLease(t, f.registered() == leaf.loaded, "plist read changed the loaded registration")
				requireServiceLease(t, slices.Equal(f.destructive(), leaf.effects), "plist read changed the manager effect trace")
				current, statErr := os.Lstat(path)
				if leaf.cleans && site != "" {
					requireServiceLease(t, errors.Is(statErr, os.ErrNotExist), "an ordinary refusal kept the plist its Apply created")
				} else {
					requireServiceLease(t, statErr == nil && os.SameFile(identity, current), "plist read removed or replaced the plist")
					data, readErr := os.ReadFile(path)
					requireServiceLease(t, readErr == nil && string(data) == string(want), "plist read changed the plist")
				}
				serviceMaintenanceUnchanged(t, f.cell, before)
				serviceRemovalLocksFree(t, f.cell)
				f.launch.owned, failDisabled = func(string, os.FileInfo) bool { return true }, false
				switch leaf.op {
				case "inspect":
					obs, err = f.launch.Inspect(t.Context(), f.d)
					requireServiceLease(t, err == nil && obs == ServiceObservation{Available: true, Registered: true, Owned: true}, "inspection retry lost its registration")
				case "apply":
					requireServiceLease(t, f.launch.Apply(t.Context(), ServicePlan{Spec: f.d, RequiredPrivilege: "user"}) == nil, "registration retry failed")
					requireServiceLease(t, f.registered(), "registration retry left the agent unloaded")
				}
				requireServiceLease(t, f.launch.Remove(t.Context(), f.d) == nil, "removal after the plist read failed")
				_, statErr = os.Lstat(path)
				requireServiceLease(t, !f.registered() && errors.Is(statErr, os.ErrNotExist), "removal after the plist read kept the registration")
			})
		}
	}
}

func launchdPlistBytes(t *testing.T, d ServiceDescriptor) []byte {
	t.Helper()
	want, err := RenderLaunchdAgentPlist(d)
	requireServiceLease(t, err == nil, "cannot render the fixture plist")
	return want
}
