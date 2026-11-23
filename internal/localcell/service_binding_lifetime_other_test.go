//go:build !windows

package localcell

import (
	"errors"
	"testing"
)

// Exercise the Windows publication format on a held native host lock. The
// first-instance and closed-handle phases need a native Windows listener.
func testBindingLifetimePhases(t *testing.T) {
	for _, row := range []string{"pre_reserve", "no_publication", "removed"} {
		t.Run(row, func(t *testing.T) {
			c, d := descriptorTestCell(t)
			boot := func() (string, error) { return "fixture-boot", nil }
			if row == "removed" {
				_, record := endpointTestActivation(t, c, d, boot)
				if c.removeOwnedServiceControlEndpoint(t.Context(), record) != nil {
					t.Fatal(bindingLifetimeFailure)
				}
				if _, err := c.readServiceControlEndpointWithBoot(t.Context(), boot); !errors.Is(err, ErrServiceControlRefused) {
					t.Fatal(bindingLifetimeFailure)
				}
				return
			}
			lease, err := c.acquireServiceLeaseForPlatform(d, boot, "windows")
			if err != nil {
				t.Fatal(bindingLifetimeFailure)
			}
			defer func() { _ = lease.Close() }()
			var provisional serviceLeaseRecord
			if readServiceRecordJSON(t.Context(), c.path("service", "lease.json"), &provisional) != nil ||
				provisional.PipeName != "" || provisional.BindingRevision != 0 || provisional.Generation != "" || provisional.Version != 2 {
				t.Fatal(bindingLifetimeFailure)
			}
			if row == "no_publication" {
				name, err := newServiceControlPipeName(d.InstallationID)
				if err != nil || c.completeServiceLease(t.Context(), d, name, boot) != nil {
					t.Fatal(bindingLifetimeFailure)
				}
			}
			if _, err := c.readServiceControlEndpointWithBoot(t.Context(), boot); !errors.Is(err, ErrServiceControlRefused) {
				t.Fatal(bindingLifetimeFailure)
			}
		})
	}
}

func testServiceBindingSuccessor(t *testing.T, c *Cell, d ServiceDescriptor, boot func() (string, error)) {
	t.Helper()
	lock, old := endpointTestActivation(t, c, d, boot)
	if lock.Close() != nil {
		t.Fatal(bindingLiveFailure)
	}
	_, next := endpointTestActivation(t, c, d, boot)
	if next.PipeName == old.PipeName || next.HostPID != old.HostPID || next.HostStartIdentity != old.HostStartIdentity {
		t.Fatal(bindingLiveFailure)
	}
	if writeAtomic(c.path("service", "control-endpoint.json"), old, nil) != nil {
		t.Fatal(bindingLiveFailure)
	}
	if _, err := c.readServiceControlEndpointWithBoot(t.Context(), boot); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal(bindingLiveFailure)
	}
}
