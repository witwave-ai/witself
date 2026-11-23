//go:build windows

package localcell

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Each publication boundary is synchronous. No probe waits for a parked
// publisher to resume before deciding whether discovery agreed with the pipe.
func testBindingLifetimePhases(t *testing.T) {
	for _, row := range []string{"pre_reserve", "issue_after_reserve", "reserve_failure", "no_publication", "closed_mapped", "removed"} {
		t.Run(row, func(t *testing.T) {
			c, d := descriptorTestCell(t)
			lease, err := c.acquireServiceLease(d)
			if err != nil {
				t.Fatal(bindingLifetimeFailure)
			}
			defer func() { _ = lease.Close() }()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var firstName string
			var reservations int
			var issueRevision uint64
			provisionalOnly := false
			create := windowsCreateNamedPipe
			defer func() { windowsCreateNamedPipe = create }()
			windowsCreateNamedPipe = func(name *uint16, access, mode, count, out, in, wait uint32, attr *windows.SecurityAttributes) (windows.Handle, error) {
				if firstName == "" {
					firstName = windows.UTF16PtrToString(name)
					var candidate serviceLeaseRecord
					_, endpointErr := os.Lstat(c.path("service", "control-endpoint.json"))
					provisionalOnly = errors.Is(endpointErr, os.ErrNotExist) && readServiceRecordJSON(ctx, c.path("service", "lease.json"), &candidate) == nil &&
						candidate.Version == 2 && candidate.PipeName == "" && candidate.Generation == "" && candidate.BindingRevision == 0 &&
						candidate.PID == os.Getpid() && candidate.Owner == d.UID && candidate.InstallationID == d.InstallationID && candidate.StartIdentity != "" && candidate.BootID != ""
				} else if row == "reserve_failure" {
					// Even an initial successful instance is insufficient when a
					// subsequent reservation fails.
					return windows.InvalidHandle, windows.ERROR_ACCESS_DENIED
				}
				handle, err := create(name, access, mode, count, out, in, wait, attr)
				reservations++
				if row == "issue_after_reserve" && err == nil && reservations == windowsPipeInstances {
					if c.SetServiceDesiredState(ctx, "stopped") == nil {
						if advanced, readErr := c.ServiceDescriptor(); readErr == nil {
							issueRevision = advanced.Revision
						}
					}
				}
				return handle, err
			}
			var completed bool
			if row == "no_publication" {
				hooks := serviceLeasePublishTestHooks
				defer func() { serviceLeasePublishTestHooks = hooks }()
				serviceLeasePublishTestHooks.write = func(path string, value any, before func() error) error {
					if err := writeAtomicOnce(path, value, before); err != nil {
						return err
					}
					var issued serviceLeaseRecord
					_, discoveryErr := c.readServiceControlEndpoint()
					completed = errors.Is(discoveryErr, ErrServiceControlRefused) && readServiceRecordJSON(ctx, path, &issued) == nil &&
						firstName != "" && issued.PipeName == firstName && issued.BindingRevision == d.Revision && issued.Generation == d.Payload.Generation
					// A private directory at the final name makes endpoint
					// publication fail after the lease became durable.
					return makePrivateDir(c.path("service", "control-endpoint.json"))
				}
			}
			listener, listenErr := c.listenServiceControl()
			if listener != nil {
				defer func() { _ = listener.Close() }()
			}
			if !provisionalOnly || !validServiceControlPipeName(firstName, d.InstallationID) {
				t.Fatal(bindingLifetimeFailure)
			}
			switch row {
			case "pre_reserve":
				got, err := c.readServiceControlEndpoint()
				if listenErr != nil || err != nil || got.PipeName != firstName || got.BindingRevision != d.Revision || got.Generation != d.Payload.Generation {
					t.Fatal(bindingLifetimeFailure)
				}
			case "issue_after_reserve":
				got, err := c.readServiceControlEndpoint()
				if listenErr != nil || err != nil || issueRevision <= d.Revision || got.BindingRevision != issueRevision {
					t.Fatal(bindingLifetimeFailure)
				}
			case "reserve_failure", "no_publication":
				if listener != nil || listenErr == nil || (row == "no_publication" && !completed) {
					t.Fatal(bindingLifetimeFailure)
				}
				if row == "reserve_failure" {
					var provisional serviceLeaseRecord
					if readServiceRecordJSON(ctx, c.path("service", "lease.json"), &provisional) != nil || provisional.PipeName != "" || provisional.BindingRevision != 0 {
						t.Fatal(bindingLifetimeFailure)
					}
				}
				if _, err := c.readServiceControlEndpoint(); !errors.Is(err, ErrServiceControlRefused) {
					t.Fatal(bindingLifetimeFailure)
				}
				conn, err := windowsDialPipe(ctx, serviceControlEndpointRecord{PipeName: firstName})
				if conn != nil {
					_ = conn.Close()
				}
				if err == nil || err.Error() != windowsControlRefusal("pipe:open").Error() {
					t.Fatal(bindingLifetimeFailure)
				}
			case "closed_mapped":
				if listenErr != nil || listener.Close() != nil {
					t.Fatal(bindingLifetimeFailure)
				}
				if _, err := c.readServiceControlEndpoint(); err != nil {
					t.Fatal(bindingLifetimeFailure)
				}
				conn, err := c.dialServiceControl(ctx)
				if conn != nil {
					_ = conn.Close()
				}
				if !errors.Is(err, ErrServiceControlRefused) {
					t.Fatal(bindingLifetimeFailure)
				}
			case "removed":
				if listenErr != nil || c.removeServiceControlEndpoint() != nil {
					t.Fatal(bindingLifetimeFailure)
				}
				if _, err := os.Lstat(c.path("service", "control-endpoint.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(bindingLifetimeFailure)
				}
				if _, err := c.readServiceControlEndpoint(); !errors.Is(err, ErrServiceControlRefused) {
					t.Fatal(bindingLifetimeFailure)
				}
				other, err := c.acquireServiceLease(d)
				if other != nil {
					_ = other.Close()
				}
				if !errors.Is(err, ErrServiceHostActive) {
					t.Fatal(bindingLifetimeFailure)
				}
			}
		})
	}
}

func testServiceBindingSuccessor(t *testing.T, c *Cell, _ ServiceDescriptor, _ func() (string, error)) {
	t.Helper()
	// Use the worker fixture's native timeout for both complete Run cycles.
	c.cfg.Timeout = 5 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), serviceControlBudget(c.cfg.Timeout, 5))
	defer cancel()
	first := serviceWorkerOnCell(t, c, "", false)
	first.ready(t)
	old, err := c.readServiceControlEndpoint()
	if err != nil {
		t.Fatal(bindingLiveFailure)
	}
	first.cancel()
	select {
	case err := <-first.done:
		if err != nil {
			t.Fatal(bindingLiveFailure)
		}
	case <-ctx.Done():
		t.Fatal(bindingLiveFailure)
	}

	next := serviceWorkerOnCell(t, c, "", false)
	next.ready(t)
	current, err := c.readServiceControlEndpoint()
	if err != nil || old.HostPID != current.HostPID || old.HostStartIdentity != current.HostStartIdentity || old.PipeName == current.PipeName {
		t.Fatal(bindingLiveFailure)
	}
	path := c.path("service", "control-endpoint.json")
	if writeAtomic(path, old, nil) != nil {
		t.Fatal(bindingLiveFailure)
	}
	before := next.recorded()
	client, err := New(c.cfg)
	if err != nil {
		t.Fatal(bindingLiveFailure)
	}
	if _, err := client.Control(ctx, "health"); !errors.Is(err, ErrServiceControlRefused) || len(next.recorded()) != len(before) {
		t.Fatal(bindingLiveFailure)
	}
	if writeAtomic(path, current, nil) != nil {
		t.Fatal(bindingLiveFailure)
	}
	status, err := client.Control(ctx, "health")
	after := next.recorded()
	if err != nil || !status.Healthy || len(after) != len(before)+1 || after[len(after)-1] != "health" {
		t.Fatal(bindingLiveFailure)
	}
}

func TestWindowsServiceControlRunRetainsInitialPayload(t *testing.T) {
	c, initial := descriptorTestCell(t)
	originalHooks := serviceLeasePublishTestHooks
	defer func() { serviceLeasePublishTestHooks = originalHooks }()
	changed := false
	serviceLeasePublishTestHooks.write = func(path string, value any, before func() error) error {
		if err := writeAtomicOnce(path, value, before); err != nil {
			return err
		}
		current := initial
		current.Payload.Generation += "-changed"
		if err := writeAtomic(c.path("service", "descriptor.json"), current, nil); err != nil {
			return err
		}
		changed = true
		return nil
	}
	reservations := 0
	create := windowsCreateNamedPipe
	defer func() { windowsCreateNamedPipe = create }()
	windowsCreateNamedPipe = func(*uint16, uint32, uint32, uint32, uint32, uint32, uint32, *windows.SecurityAttributes) (windows.Handle, error) {
		reservations++
		return windows.InvalidHandle, windows.ERROR_ACCESS_DENIED
	}
	hooks, _, _, _, _ := supervisorTestHooks()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := c.Run(ctx, SupervisorPolicy{hooks: &hooks})
	if !changed || reservations != 0 || !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal(bindingLifetimeFailure)
	}
	if _, err := os.Lstat(c.path("service", "control-endpoint.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(bindingLifetimeFailure)
	}
}

func TestWindowsServiceControlCompletionFailure(t *testing.T) {
	c, _ := descriptorTestCell(t)
	c.cfg.Timeout = 5 * time.Second
	injected := errors.New("injected completed lease publication failure")
	var reserved serviceControlEndpointRecord
	var firstInstanceOccupied bool
	hooks := serviceLeasePublishTestHooks
	defer func() { serviceLeasePublishTestHooks = hooks }()
	serviceLeasePublishTestHooks.write = func(path string, value any, before func() error) error {
		lease, ok := value.(serviceLeaseRecord)
		if !ok || lease.PipeName == "" {
			return writeAtomicOnce(path, value, before)
		}
		// Observe the native reservation at the exact failed publication.
		reserved = serviceEndpointForLease(lease)
		probe, err := windowsListenPipe(reserved.PipeName)
		if probe != nil {
			_ = probe.Close()
		}
		firstInstanceOccupied = err != nil && err.Error() == "localcell: service_control_unavailable: pipe:first_instance"
		return injected
	}
	policy, _, _, _, _ := supervisorTestHooks()
	ctx, cancel := context.WithTimeout(t.Context(), c.cfg.Timeout)
	defer cancel()
	err := c.Run(ctx, SupervisorPolicy{hooks: &policy})
	serviceLeasePublishTestHooks = hooks
	if !errors.Is(err, injected) || !firstInstanceOccupied {
		t.Fatal("did not fail lease completion after a successful first-instance reservation")
	}

	t.Run("listener_closed", func(t *testing.T) {
		// A new first instance at the exact retired name requires every
		// server instance of the failed listener to have been closed.
		probe, err := windowsListenPipe(reserved.PipeName)
		if err != nil {
			t.Fatal("failed lease completion left the listener reserved")
		}
		if probe.Close() != nil {
			t.Fatal("could not release the first-instance probe")
		}
	})
	t.Run("endpoint_absent", func(t *testing.T) {
		if _, err := os.Lstat(c.path("service", "control-endpoint.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed lease completion left an endpoint record")
		}
	})
	t.Run("dial_refused", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), c.cfg.Timeout)
		defer cancel()
		conn, err := windowsDialPipe(ctx, reserved)
		if conn != nil {
			_ = conn.Close()
		}
		if err == nil || err.Error() != windowsControlRefusal("pipe:open").Error() {
			t.Fatal("dial of the failed reservation was not refused at open")
		}
	})
	t.Run("next_start", func(t *testing.T) {
		next := serviceWorkerOnCell(t, c, "", false)
		next.ready(t)
		fresh, err := c.readServiceControlEndpoint()
		if err != nil || fresh.PipeName == reserved.PipeName {
			t.Fatal("next start did not publish a fresh listener")
		}
	})
}
