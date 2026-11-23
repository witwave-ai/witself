//go:build darwin || linux

package localcell

import (
	"errors"
	"os"
	"testing"
)

func TestServiceControlEndpointUnixPrivateRecord(t *testing.T) {
	c, _ := descriptorTestCell(t)
	r, _, _ := endpointFixture(t)
	path := c.path("service", "control-endpoint.json")
	if writeAtomic(path, r, nil) != nil || os.Chmod(path, 0o644) != nil {
		t.Fatal("cannot prepare nonprivate endpoint fixture")
	}
	if _, err := c.readServiceControlEndpointRecord(t.Context()); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("nonprivate endpoint record accepted")
	}
	if os.Chmod(path, 0o600) != nil {
		t.Fatal("cannot restore private endpoint fixture")
	}
	if got, err := c.readServiceControlEndpointRecord(t.Context()); err != nil || got != r {
		t.Fatal("private endpoint record refused")
	}
	target := c.path("service", "endpoint-target.json")
	if os.Rename(path, target) != nil || os.Symlink(target, path) != nil {
		t.Fatal("cannot prepare endpoint symlink fixture")
	}
	if _, err := c.readServiceControlEndpointRecord(t.Context()); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("endpoint record symlink accepted")
	}
	var retained serviceControlEndpointRecord
	if readJSON(t.Context(), target, &retained) != nil || retained != r {
		t.Fatal("endpoint refusal changed symlink target")
	}
}

func TestServiceHostExitUnixLeaseFormat(t *testing.T) {
	c, d := descriptorTestCell(t)
	lock, err := c.acquireServiceLease(d)
	if err != nil {
		t.Fatal("could not prepare version-one host lease")
	}
	defer func() { _ = lock.Close() }()
	_, original, err := c.serviceHostExitIdentity()
	if err != nil || original.Version != 1 {
		t.Fatal("Unix exit refused its original lease")
	}
	// Even a zero-valued Windows field was unknown to the Unix reader.
	legacy := struct {
		serviceLeaseIdentity
		BindingRevision uint64 `json:"binding_revision"`
	}{serviceLeaseIdentity: original.serviceLeaseIdentity}
	if writeAtomic(c.path("service", "lease.json"), legacy, nil) != nil {
		t.Fatal("could not publish mixed lease fixture")
	}
	if _, _, err := c.serviceHostExitIdentity(); err == nil {
		t.Fatal("Unix exit accepted a field outside its lease format")
	}
}
