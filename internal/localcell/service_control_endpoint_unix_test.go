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
	if _, err := c.readServiceControlEndpointRecord(); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("nonprivate endpoint record accepted")
	}
	if os.Chmod(path, 0o600) != nil {
		t.Fatal("cannot restore private endpoint fixture")
	}
	if got, err := c.readServiceControlEndpointRecord(); err != nil || got != r {
		t.Fatal("private endpoint record refused")
	}
	target := c.path("service", "endpoint-target.json")
	if os.Rename(path, target) != nil || os.Symlink(target, path) != nil {
		t.Fatal("cannot prepare endpoint symlink fixture")
	}
	if _, err := c.readServiceControlEndpointRecord(); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("endpoint record symlink accepted")
	}
	var retained serviceControlEndpointRecord
	if readJSON(target, &retained) != nil || retained != r {
		t.Fatal("endpoint refusal changed symlink target")
	}
}
