package localcell

import (
	"errors"
	"os"
	"testing"
)

func TestServiceControlEndpointRetirement(t *testing.T) {
	c, d := descriptorTestCell(t)
	boot := func() (string, error) { return "fixture-boot", nil }
	lock, retired := endpointTestActivation(t, c, d, boot)
	path := c.path("service", "control-endpoint.json")
	reader, err := openAdmissionRecord(t.Context(), path, serviceEndpointRefused("record"))
	if err != nil {
		t.Fatal("cannot hold the client's admission reader")
	}
	defer func() { _ = reader.Close() }()

	if c.removeOwnedServiceControlEndpoint(retired) != nil {
		t.Fatal("owner could not remove its endpoint")
	}
	_, statErr := os.Lstat(path)
	if lock.Close() != nil {
		t.Fatal("cannot release the retired host lease")
	}
	next, err := c.acquireServiceLeaseForPlatform(d, boot, "windows")
	if err != nil {
		t.Fatal("cannot acquire the successor host lease")
	}
	defer func() { _ = next.Close() }()
	name, err := newServiceControlPipeName(d.InstallationID)
	if err != nil || name == retired.PipeName || c.completeServiceLease(d, name, boot) != nil {
		t.Fatal("cannot complete the successor host lease")
	}
	published, publishErr := c.publishServiceControlEndpointWithBoot(name, boot)
	fresh, freshErr := c.readServiceControlEndpointWithBoot(boot)
	held, heldErr := decodeServiceControlEndpoint(reader)

	for _, row := range []struct {
		name, success, failure, message string
		ok                              bool
	}{
		{"name_absent", "name_absent", "name_not_released", "retired endpoint name did not disappear immediately", errors.Is(statErr, os.ErrNotExist)},
		{"successor_published", "successor_published", "successor_unavailable", "admission reader blocked successor endpoint publication", publishErr == nil && published.PipeName == name && freshErr == nil && fresh == published},
		{"retired_reader", "retired_record", "retired_record_lost", "held admission reader did not return the retired endpoint", heldErr == nil && held == retired},
	} {
		t.Run(row.name, func(t *testing.T) {
			class := row.success
			if !row.ok {
				class = row.failure
			}
			t.Logf("endpoint retirement class=%s", class)
			if !row.ok {
				t.Fatal(row.message)
			}
		})
	}
}
