//go:build !windows

package localcell

import (
	"context"
	"net"
)

// Unix keeps its existing wire envelope, socket admission and version-one lease.
type serviceHostExitBinding struct{}

func validHostExitLease(_ ServiceDescriptor, lease serviceLeaseRecord, _ string) bool {
	return lease.Version == 1
}
func hostExitBindingForLease(serviceLeaseRecord) serviceHostExitBinding {
	return serviceHostExitBinding{}
}
func (*serviceControlHostState) hostExitBindingMatches(serviceHostExitBinding, serviceLeaseRecord, net.Listener) bool {
	return true
}
func (c *Cell) validateHostExitEndpoint(ctx context.Context, _ serviceHostExitBinding) error {
	return c.validateServiceControlEndpoint(ctx)
}
func (c *Cell) dialHostExitEndpoint(ctx context.Context, _ serviceHostExitBinding) (net.Conn, error) {
	return c.dialServiceControl(ctx)
}

// Unix uses the stable lifetime lease as the host-exit fence. Rechecking the
// creation identity prevents replacement from being mistaken for that host.
type unixServiceExitHost struct{ lease serviceLeaseRecord }

func retainServiceExitHost(lease serviceLeaseRecord) (serviceExitHostObserver, error) {
	identity, alive, err := processIdentity(lease.PID)
	if err != nil || !alive || identity != lease.StartIdentity {
		return nil, hostExitRefused("envelope")
	}
	return &unixServiceExitHost{lease: lease}, nil
}
func (o *unixServiceExitHost) exited() (bool, error) {
	identity, alive, err := processIdentity(o.lease.PID)
	if err != nil || (alive && identity != o.lease.StartIdentity) {
		return false, hostExitRefused("envelope")
	}
	// Run can be hosted in the caller's process (embedding and hermetic tests).
	// The lease, checked by the caller in the same iteration, fences Run's exit.
	return true, nil
}
func (*unixServiceExitHost) close() {}
