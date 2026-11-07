//go:build !windows

package localcell

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
