package localcell

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
)

func endpointFixture(t *testing.T) (serviceControlEndpointRecord, ServiceDescriptor, serviceLeaseRecord) {
	t.Helper()
	name, err := newServiceControlPipeName("lc_fixture")
	if err != nil {
		t.Fatal("cannot construct endpoint fixture")
	}
	d := ServiceDescriptor{InstallationID: "lc_fixture", UID: "fixture-owner", Revision: 7, Payload: ServicePayload{Generation: "generation-1"}}
	l := serviceLeaseRecord{
		serviceLeaseIdentity: serviceLeaseIdentity{Version: 2, InstallationID: d.InstallationID, Owner: d.UID, PID: 123, StartIdentity: "windows:1:2", BootID: "windows:fixture-boot"},
		BindingRevision:      d.Revision, Generation: d.Payload.Generation, PipeName: name,
	}
	r := serviceControlEndpointRecord{Version: 2, InstallationID: d.InstallationID, Generation: d.Payload.Generation, BindingRevision: d.Revision,
		BootID: l.BootID, HostPID: l.PID, HostStartIdentity: l.StartIdentity, PipeName: name}
	return r, d, l
}

func TestServiceControlPipeNameEncodingAndBounds(t *testing.T) {
	seen := make(map[string]bool)
	for range 32 {
		name, err := newServiceControlPipeName(strings.Repeat("a", 128))
		if err != nil || !validServiceControlPipeName(name, strings.Repeat("a", 128)) || len(name) >= 256 || seen[name] {
			t.Fatal("pipe name lacks bounded installation and random identity")
		}
		seen[name] = true
		suffix := name[len(name)-26:]
		decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(suffix))
		if err != nil || len(decoded) != 16 || suffix != strings.ToLower(suffix) || strings.Contains(suffix, "=") {
			t.Fatal("pipe nonce is not canonical lowercase unpadded 128-bit base32")
		}
	}
	for _, id := range []string{"", strings.Repeat("a", 129), "bad/id", `bad\id`, "bad.id", "a\n", "日本", "a b"} {
		if _, err := newServiceControlPipeName(id); !errors.Is(err, ErrServiceControlRefused) {
			t.Fatal("unbounded or namespace-changing installation accepted")
		}
	}
	r, _, _ := endpointFixture(t)
	for _, name := range []string{r.PipeName + "a", strings.ToUpper(r.PipeName), strings.Replace(r.PipeName, "lc_fixture", "other", 1), r.PipeName[:len(r.PipeName)-1] + "z", r.PipeName[:len(r.PipeName)-1] + "="} {
		if validServiceControlPipeName(name, r.InstallationID) {
			t.Fatal("noncanonical pipe name accepted")
		}
	}
}

func TestServiceControlEndpointStrictDecode(t *testing.T) {
	r, _, _ := endpointFixture(t)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal("cannot encode endpoint fixture")
	}
	got, err := decodeServiceControlEndpoint(bytes.NewReader(b))
	if err != nil || got != r {
		t.Fatal("endpoint encoding did not round trip")
	}
	boundary := string(b) + strings.Repeat(" ", 4096-len(b))
	got, err = decodeServiceControlEndpoint(strings.NewReader(boundary))
	if err != nil || got != r {
		t.Fatal("endpoint at the byte limit did not round trip")
	}
	for _, input := range []string{"", "{", string(b) + "{}", strings.TrimSuffix(string(b), "}") + `,"unknown":1}`, strings.Repeat("x", 4097), boundary + " ", boundary + `{"unknown":1}`} {
		got, err := decodeServiceControlEndpoint(strings.NewReader(input))
		if !errors.Is(err, ErrServiceControlRefused) || got != (serviceControlEndpointRecord{}) || err.Error() != "localcell: service_control_refused: endpoint:record" {
			t.Fatal("malformed endpoint leaked partial data or lost fixed refusal")
		}
	}
}

func TestServiceControlEndpointBindingRefusals(t *testing.T) {
	r, d, l := endpointFixture(t)
	if err := validateServiceControlRecord(r, d, l, r.BootID); err != nil {
		t.Fatal("valid endpoint binding refused")
	}
	for _, tc := range []struct {
		name   string
		change func(*serviceControlEndpointRecord, *ServiceDescriptor, *serviceLeaseRecord, *string)
	}{
		{"version", func(r *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, _ *string) {
			r.Version++
		}},
		{"installation", func(r *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, _ *string) {
			r.InstallationID = "other"
		}},
		{"generation", func(r *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, _ *string) {
			r.Generation = "other"
		}},
		{"descriptor_generation", func(_ *serviceControlEndpointRecord, d *ServiceDescriptor, _ *serviceLeaseRecord, _ *string) {
			d.Payload.Generation = "other"
		}},
		{"issue_revision", func(r *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, _ *string) {
			r.BindingRevision--
		}},
		{"issue_higher", func(_ *serviceControlEndpointRecord, _ *ServiceDescriptor, l *serviceLeaseRecord, _ *string) {
			l.BindingRevision--
		}},
		{"issue_zero", func(r *serviceControlEndpointRecord, _ *ServiceDescriptor, l *serviceLeaseRecord, _ *string) {
			r.BindingRevision, l.BindingRevision = 0, 0
		}},
		{"issue_future", func(_ *serviceControlEndpointRecord, d *ServiceDescriptor, _ *serviceLeaseRecord, _ *string) {
			d.Revision--
		}},
		{"lease_name", func(_ *serviceControlEndpointRecord, _ *ServiceDescriptor, l *serviceLeaseRecord, _ *string) {
			l.PipeName = ""
		}},
		{"lease_generation", func(_ *serviceControlEndpointRecord, _ *ServiceDescriptor, l *serviceLeaseRecord, _ *string) {
			l.Generation = "other"
		}},
		{"prior_activation", func(_ *serviceControlEndpointRecord, d *ServiceDescriptor, l *serviceLeaseRecord, _ *string) {
			l.PipeName, _ = newServiceControlPipeName(d.InstallationID)
		}},
		{"revision", func(r *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, _ *string) {
			r.BindingRevision++
		}},
		{"boot", func(_ *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, b *string) {
			*b = "other"
		}},
		{"empty_boot", func(_ *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, b *string) { *b = "" }},
		{"pipe", func(r *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, _ *string) {
			r.PipeName += "a"
		}},
		{"pipe_namespace", func(r *serviceControlEndpointRecord, d *ServiceDescriptor, l *serviceLeaseRecord, _ *string) {
			r.PipeName = strings.Replace(r.PipeName, d.InstallationID, "other", 1)
			l.PipeName = r.PipeName
		}},
		{"pid", func(r *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, _ *string) {
			r.HostPID++
		}},
		{"invalid_pid", func(r *serviceControlEndpointRecord, _ *ServiceDescriptor, l *serviceLeaseRecord, _ *string) {
			r.HostPID = 1
			l.PID = 1
		}},
		{"start", func(r *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, _ *string) {
			r.HostStartIdentity = "other"
		}},
		{"empty_start", func(r *serviceControlEndpointRecord, _ *ServiceDescriptor, l *serviceLeaseRecord, _ *string) {
			r.HostStartIdentity = ""
			l.StartIdentity = ""
		}},
		{"lease_version", func(_ *serviceControlEndpointRecord, _ *ServiceDescriptor, l *serviceLeaseRecord, _ *string) {
			l.Version++
		}},
		{"lease_version_one", func(_ *serviceControlEndpointRecord, _ *ServiceDescriptor, l *serviceLeaseRecord, _ *string) {
			l.Version = 1
		}},
		{"lease_boot", func(_ *serviceControlEndpointRecord, _ *ServiceDescriptor, l *serviceLeaseRecord, _ *string) {
			l.BootID = "other"
		}},
		{"lease_installation", func(_ *serviceControlEndpointRecord, _ *ServiceDescriptor, l *serviceLeaseRecord, _ *string) {
			l.InstallationID = "other"
		}},
		{"lease_owner", func(_ *serviceControlEndpointRecord, _ *ServiceDescriptor, l *serviceLeaseRecord, _ *string) {
			l.Owner = "other"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, descriptor, lease, boot := r, d, l, r.BootID
			tc.change(&changed, &descriptor, &lease, &boot)
			err := validateServiceControlRecord(changed, descriptor, lease, boot)
			if !errors.Is(err, ErrServiceControlRefused) {
				t.Fatal("unbound endpoint accepted")
			}
			if (tc.name == "pid" || tc.name == "start") && err.Error() != ErrServiceControlRefused.Error()+": endpoint:host_identity" {
				t.Fatal("host identity mismatch lost its fixed refusal detail")
			}
		})
	}
}

func TestServiceControlEndpointPrivateReadAndOwnedRemoval(t *testing.T) {
	c, _ := descriptorTestCell(t)
	r, _, _ := endpointFixture(t)
	path := c.path("service", "control-endpoint.json")
	if _, err := c.readServiceControlEndpointRecord(); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("missing endpoint accepted")
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal("cannot prepare wrong-type endpoint")
	}
	if _, err := c.readServiceControlEndpointRecord(); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("non-file endpoint accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal("cannot clear fixture")
	}
	for _, field := range []string{"pid", "start", "pipe"} {
		replacement := r
		switch field {
		case "pid":
			replacement.HostPID++
		case "start":
			replacement.HostStartIdentity += "-other"
		case "pipe":
			replacement.PipeName += "-other"
		}
		if err := writeAtomic(path, replacement, nil); err != nil {
			t.Fatal("cannot prepare replacement")
		}
		if !errors.Is(c.removeOwnedServiceControlEndpoint(r), ErrServiceControlRefused) {
			t.Fatal("old host accepted replacement endpoint cleanup")
		}
		got, err := c.readServiceControlEndpointRecord()
		if err != nil || got != replacement {
			t.Fatal("old host changed replacement endpoint")
		}
	}
	if err := writeAtomic(path, r, nil); err != nil || c.removeOwnedServiceControlEndpoint(r) != nil {
		t.Fatal("owned endpoint cleanup failed")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned endpoint retained")
	}
}

func TestServiceControlHostedRefusesBeforeDiscovery(t *testing.T) {
	c, _ := unitCell(t)
	for _, selection := range []string{"", "hosted"} {
		t.Setenv("WITSELF_RUNTIME", selection)
		if _, err := c.Control(t.Context(), "status"); !errors.Is(err, ErrServiceControlRefused) {
			t.Fatal("unselected control reached capability or discovery")
		}
		if _, err := os.Lstat(c.path("service")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unselected control wrote service state")
		}
	}
}

const bindingLiveFailure = "live discovery lost its activation fence"

func endpointTestActivation(t *testing.T, c *Cell, d ServiceDescriptor, boot func() (string, error)) (*operationLock, serviceControlEndpointRecord) {
	t.Helper()
	lease, err := c.acquireServiceLeaseForPlatform(d, boot, "windows")
	if err != nil {
		t.Fatal(bindingLiveFailure)
	}
	t.Cleanup(func() { _ = lease.Close() })
	name, err := newServiceControlPipeName(d.InstallationID)
	if err != nil || c.completeServiceLease(d, name, boot) != nil {
		t.Fatal(bindingLiveFailure)
	}
	record, err := c.publishServiceControlEndpointWithBoot(name, boot)
	if err != nil {
		t.Fatal(bindingLiveFailure)
	}
	fresh, err := c.readServiceControlEndpointWithBoot(boot)
	if err != nil || fresh != record {
		t.Fatal(bindingLiveFailure)
	}
	return lease, record
}

func TestServiceControlEndpointPublicationAndLiveRead(t *testing.T) {
	for _, row := range []string{"issue_revision", "state_advanced", "legacy_mixed", "successor", "boot", "start", "lease", "provisional", "legacy_bootstrap", "missing"} {
		t.Run(row, func(t *testing.T) {
			c, d := descriptorTestCell(t)
			boot := func() (string, error) { return "windows:synthetic-boot", nil }
			name, err := newServiceControlPipeName(d.InstallationID)
			if err != nil {
				t.Fatal(bindingLiveFailure)
			}
			if row == "missing" {
				if _, err := c.publishServiceControlEndpoint(name); !errors.Is(err, ErrServiceControlRefused) {
					t.Fatal(bindingLiveFailure)
				}
				if _, err := c.readServiceControlEndpointWithBoot(boot); !errors.Is(err, ErrServiceControlRefused) {
					t.Fatal(bindingLiveFailure)
				}
				return
			}
			if row == "provisional" || row == "legacy_bootstrap" {
				platform := "windows"
				if row == "legacy_bootstrap" {
					platform = "linux"
				}
				lock, err := c.acquireServiceLeaseForPlatform(d, boot, platform)
				if err != nil {
					t.Fatal(bindingLiveFailure)
				}
				defer func() { _ = lock.Close() }()
				if row == "legacy_bootstrap" && !errors.Is(c.completeServiceLease(d, name, boot), ErrServiceControlRefused) {
					t.Fatal(bindingLiveFailure)
				}
				if _, err := c.publishServiceControlEndpointWithBoot(name, boot); !errors.Is(err, ErrServiceControlRefused) {
					t.Fatal(bindingLiveFailure)
				}
				return
			}
			if row == "successor" {
				testServiceBindingSuccessor(t, c, d, boot)
				return
			}
			_, record := endpointTestActivation(t, c, d, boot)
			changed := record
			switch row {
			case "issue_revision":
				// Keep the claimed revision positive and within the live state
				// range. Only its disagreement with the issued lease refuses.
				if c.SetServiceDesiredState(t.Context(), "stopped") != nil {
					t.Fatal(bindingLiveFailure)
				}
				advanced, err := c.ServiceDescriptor()
				if err != nil || advanced.Revision != record.BindingRevision+1 {
					t.Fatal(bindingLiveFailure)
				}
				changed.BindingRevision = advanced.Revision
			case "state_advanced":
				if c.SetServiceDesiredState(t.Context(), "stopped") != nil {
					t.Fatal(bindingLiveFailure)
				}
				advanced, err := c.ServiceDescriptor()
				if err != nil || advanced.Revision <= record.BindingRevision {
					t.Fatal(bindingLiveFailure)
				}
				got, err := c.readServiceControlEndpointWithBoot(boot)
				if err != nil || got != record {
					t.Fatal(bindingLiveFailure)
				}
				return
			case "legacy_mixed":
				// Neither direction may use the other version as authority.
				var live serviceLeaseRecord
				leasePath := c.path("service", "lease.json")
				if readServiceRecordJSON(t.Context(), leasePath, &live) != nil {
					t.Fatal(bindingLiveFailure)
				}
				old := live.serviceLeaseIdentity
				old.Version = 1
				if writeAtomic(leasePath, old, nil) != nil {
					t.Fatal(bindingLiveFailure)
				}
				if _, err := c.readServiceControlEndpointWithBoot(boot); !errors.Is(err, ErrServiceControlRefused) {
					t.Fatal(bindingLiveFailure)
				}
				if writeAtomic(leasePath, live, nil) != nil {
					t.Fatal(bindingLiveFailure)
				}
				changed.Version = 1
			case "boot":
				changed.BootID += "-stale"
			case "start":
				changed.HostStartIdentity += "-stale"
			case "lease":
				if removePrivateFile(c.path("service", "lease.json")) != nil {
					t.Fatal(bindingLiveFailure)
				}
			}
			if writeAtomic(c.path("service", "control-endpoint.json"), changed, nil) != nil {
				t.Fatal(bindingLiveFailure)
			}
			if _, err := c.readServiceControlEndpointWithBoot(boot); !errors.Is(err, ErrServiceControlRefused) {
				t.Fatal(bindingLiveFailure)
			}
		})
	}
}

const bindingLifetimeFailure = "listener lifetime disagreed with discovery"

func TestServiceBindingLifetime(t *testing.T) {
	testBindingLifetimePhases(t)
	t.Run("successor_preserved", func(t *testing.T) {
		c, d := descriptorTestCell(t)
		boot := func() (string, error) { return "fixture-boot", nil }
		_, record := endpointTestActivation(t, c, d, boot)
		successor := record
		var err error
		successor.PipeName, err = newServiceControlPipeName(d.InstallationID)
		if err != nil {
			t.Fatal(bindingLifetimeFailure)
		}
		published := false
		err = c.removeOwnedServiceControlEndpointBy(record, func() {
			published = writeAtomic(c.path("service", "control-endpoint.json"), successor, nil) == nil
		})
		survivor, readErr := c.readServiceControlEndpointRecord()
		if !published || !errors.Is(err, ErrServiceControlRefused) || readErr != nil || survivor != successor {
			t.Fatal(bindingLifetimeFailure)
		}
		if c.removeOwnedServiceControlEndpoint(successor) != nil {
			t.Fatal(bindingLifetimeFailure)
		}
	})
}

// Standalone transport fixtures have no Run to supply the validated launch
// descriptor. Unix opens the same socket even in fixtures without a descriptor.
func (c *Cell) serveServiceControl(commands chan<- serviceCommand) (func(), error) {
	var initial ServiceDescriptor
	if runtime.GOOS == "windows" {
		var err error
		initial, err = c.ServiceDescriptor()
		if err != nil {
			return nil, err
		}
	}
	return c.serveServiceControlForPlatform(context.Background(), commands, runtime.GOOS, initial)
}

func TestServiceBindingLeaseCompletionRefusals(t *testing.T) {
	for _, row := range []string{"already_completed", "foreign_pid", "different_boot"} {
		t.Run(row, func(t *testing.T) {
			c, d := descriptorTestCell(t)
			boot := func() (string, error) { return "fixture-boot", nil }
			lock, err := c.acquireServiceLeaseForPlatform(d, boot, "windows")
			if err != nil {
				t.Fatal("could not acquire provisional lease")
			}
			defer func() { _ = lock.Close() }()
			path := c.path("service", "lease.json")
			var lease serviceLeaseRecord
			if readServiceRecordJSON(t.Context(), path, &lease) != nil {
				t.Fatal("could not read provisional lease")
			}
			name, err := newServiceControlPipeName(d.InstallationID)
			if err != nil {
				t.Fatal("could not construct candidate listener name")
			}
			requireRefusal := func() {
				t.Helper()
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal("could not read lease before completion")
				}
				if !errors.Is(c.completeServiceLease(d, name, boot), ErrServiceControlRefused) {
					t.Fatal("invalid lease completion accepted")
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("refused completion changed the lease")
				}
				if _, err := os.Lstat(c.path("service", "control-endpoint.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("refused completion published an endpoint")
				}
			}
			switch row {
			case "already_completed":
				oldName, err := newServiceControlPipeName(d.InstallationID)
				if err != nil || c.completeServiceLease(d, oldName, boot) != nil {
					t.Fatal("could not complete prior activation")
				}
				requireRefusal()
				// The prior name alone must also prevent reissue, independent
				// of the generation and revision on a fully completed lease.
				lease.PipeName = oldName
			case "foreign_pid":
				lease.PID++
			case "different_boot":
				lease.BootID += "-other"
			}
			if writeAtomic(path, lease, nil) != nil {
				t.Fatal("could not prepare lease for completion")
			}
			requireRefusal()
		})
	}
}

func TestServiceBindingIssueDescriptor(t *testing.T) {
	for _, row := range []string{"generation_changed", "host_changed", "state_advanced"} {
		t.Run(row, func(t *testing.T) {
			c, initial := descriptorTestCell(t)
			lock, err := c.acquireServiceLeaseForPlatform(initial, func() (string, error) { return "fixture-boot", nil }, "windows")
			if err != nil {
				t.Fatal(bindingLifetimeFailure)
			}
			defer func() { _ = lock.Close() }()
			if row == "state_advanced" {
				if c.SetServiceDesiredState(t.Context(), "stopped") != nil {
					t.Fatal(bindingLifetimeFailure)
				}
				d, err := c.serviceListenerDescriptor(initial)
				if err != nil || d.Revision <= initial.Revision || d.DesiredState != "stopped" {
					t.Fatal(bindingLifetimeFailure)
				}
				name, err := newServiceControlPipeName(initial.InstallationID)
				boot := func() (string, error) { return "fixture-boot", nil }
				var issued serviceLeaseRecord
				if err != nil || c.completeServiceLease(initial, name, boot) != nil ||
					readServiceRecordJSON(t.Context(), c.path("service", "lease.json"), &issued) != nil || issued.BindingRevision != d.Revision {
					t.Fatal(bindingLifetimeFailure)
				}
				return
			}
			current := initial
			if row == "generation_changed" {
				current.Payload.Generation += "-changed"
			} else {
				current.Payload.Host += "-other"
			}
			if writeAtomic(c.path("service", "descriptor.json"), current, nil) != nil {
				t.Fatal(bindingLifetimeFailure)
			}
			if _, err := c.serviceListenerDescriptor(initial); !errors.Is(err, ErrServiceControlRefused) {
				t.Fatal(bindingLifetimeFailure)
			}
		})
	}
}
