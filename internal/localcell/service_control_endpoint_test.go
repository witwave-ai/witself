package localcell

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"errors"
	"os"
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
	l := serviceLeaseRecord{Version: 1, InstallationID: d.InstallationID, Owner: d.UID, PID: 123, StartIdentity: "windows:1:2", BootID: "windows:fixture-boot"}
	r := serviceControlEndpointRecord{Version: 1, InstallationID: d.InstallationID, Generation: d.Payload.Generation, DescriptorRevision: d.Revision,
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
		{"revision", func(r *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, _ *string) {
			r.DescriptorRevision++
		}},
		{"boot", func(_ *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, b *string) {
			*b = "other"
		}},
		{"empty_boot", func(_ *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, b *string) { *b = "" }},
		{"pipe", func(r *serviceControlEndpointRecord, _ *ServiceDescriptor, _ *serviceLeaseRecord, _ *string) {
			r.PipeName += "a"
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

func TestServiceControlEndpointRevisionRefresh(t *testing.T) {
	c, d := descriptorTestCell(t)
	r, _, _ := endpointFixture(t)
	r.InstallationID, r.Generation, r.DescriptorRevision = d.InstallationID, d.Payload.Generation, d.Revision
	c.serviceControlOwned = &r
	if err := writeAtomic(c.path("service", "control-endpoint.json"), r, nil); err != nil {
		t.Fatal("cannot prepare host endpoint")
	}
	if err := c.SetServiceDesiredState(t.Context(), "stopped"); err != nil {
		t.Fatal("host command failed to advance descriptor and endpoint")
	}
	fresh, err := c.readServiceControlEndpointRecord()
	if err != nil || fresh.DescriptorRevision != d.Revision+1 || !sameServiceControlHost(fresh, r) {
		t.Fatal("host command left stale endpoint revision")
	}
	replacement := fresh
	replacement.HostPID++
	if writeAtomic(c.path("service", "control-endpoint.json"), replacement, nil) != nil {
		t.Fatal("cannot prepare replacement")
	}
	if err := c.refreshOwnedServiceControlEndpoint(d); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("revision refresh adopted replacement host")
	}
	got, err := c.readServiceControlEndpointRecord()
	if err != nil || got != replacement {
		t.Fatal("revision refresh changed replacement endpoint")
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

func TestServiceControlEndpointPublicationAndLiveRead(t *testing.T) {
	c, d := descriptorTestCell(t)
	name, err := newServiceControlPipeName(d.InstallationID)
	if err != nil {
		t.Fatal("cannot construct publication fixture")
	}
	// The transport owns binding; this tests only its portable publication seam.
	if _, err := c.publishServiceControlEndpoint(name); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("endpoint published without host lease")
	}
	if _, err := c.readServiceControlEndpoint(); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("missing endpoint accepted by live reader")
	}
	boot := func() (string, error) { return "windows:synthetic-boot", nil }
	lease, err := c.acquireServiceLeaseWithBoot(d, boot)
	if err != nil {
		t.Fatal("cannot acquire publication fixture lease")
	}
	defer func() { _ = lease.Close() }()
	record, err := c.publishServiceControlEndpointWithBoot(name, boot)
	if err != nil {
		t.Fatal("valid leased endpoint could not publish")
	}
	fresh, err := c.readServiceControlEndpointWithBoot(boot)
	if err != nil || fresh != record {
		t.Fatal("published endpoint lost live descriptor/lease/boot binding")
	}
	for _, mutation := range []string{"boot", "revision", "start", "lease"} {
		changed := record
		switch mutation {
		case "boot":
			changed.BootID += "-stale"
		case "revision":
			changed.DescriptorRevision++
		case "start":
			changed.HostStartIdentity += "-stale"
		case "lease":
			if removePrivateFile(c.path("service", "lease.json")) != nil {
				t.Fatal("cannot remove fixture lease record")
			}
		}
		if writeAtomic(c.path("service", "control-endpoint.json"), changed, nil) != nil {
			t.Fatal("cannot publish refused fixture")
		}
		if _, err := c.readServiceControlEndpointWithBoot(boot); !errors.Is(err, ErrServiceControlRefused) {
			t.Fatal("live endpoint reader accepted stale authority")
		}
	}
}
