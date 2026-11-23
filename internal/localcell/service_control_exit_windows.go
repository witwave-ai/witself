//go:build windows

package localcell

import (
	"context"
	"net"

	"golang.org/x/sys/windows"
)

// The activation is selected before retaining the host process. A single
// process can release its lease and start another listener.
type serviceHostExitBinding = serviceHostExitActivation

func validHostExitLease(d ServiceDescriptor, lease serviceLeaseRecord, boot string) bool {
	return validateServiceControlRecord(serviceEndpointForLease(lease), d, lease, boot) == nil
}

func hostExitBindingForLease(lease serviceLeaseRecord) serviceHostExitBinding {
	activation := serviceEndpointForLease(lease)
	return serviceHostExitBinding{Activation: &activation}
}

func (c *serviceControlHostState) hostExitBindingMatches(binding serviceHostExitBinding, lease serviceLeaseRecord, listener net.Listener) bool {
	if !binding.matches(serviceEndpointForLease(lease)) {
		return false
	}
	c.serviceControlMu.Lock()
	defer c.serviceControlMu.Unlock()
	return c.serviceControlListener != nil && (listener == nil || c.serviceControlListener == listener) &&
		c.serviceControlOwned != nil && binding.matches(*c.serviceControlOwned)
}

func (c *Cell) readHostExitEndpoint(binding serviceHostExitBinding) (serviceControlEndpointRecord, error) {
	record, err := c.readServiceControlEndpoint()
	if err != nil || !binding.matches(record) {
		return serviceControlEndpointRecord{}, hostExitRefused("envelope")
	}
	return record, nil
}

func (c *Cell) validateHostExitEndpoint(binding serviceHostExitBinding) error {
	_, err := c.readHostExitEndpoint(binding)
	return err
}

func (c *Cell) dialHostExitEndpoint(ctx context.Context, binding serviceHostExitBinding) (net.Conn, error) {
	record, err := c.readHostExitEndpoint(binding)
	if err != nil {
		return nil, err
	}
	return windowsDialPipe(ctx, record)
}

type windowsServiceExitHost struct{ handle windows.Handle }

func (o *windowsServiceExitHost) validateHost(d ServiceDescriptor) error {
	sid, err := windowsProcessTokenUser(o.handle)
	if err != nil || sid == nil || sid.String() != d.UID || windowsValidateProcessExecutable(o.handle, d.Payload.Host) != nil {
		return hostExitRefused("envelope")
	}
	return nil
}

func retainServiceExitHost(lease serviceLeaseRecord) (serviceExitHostObserver, error) {
	handle, alive, err := openVerifiedWindowsProcess(ProcessRecord{PID: lease.PID, StartTime: lease.StartIdentity}, 0)
	if err != nil || !alive {
		return nil, hostExitRefused("envelope")
	}
	sid, err := windowsProcessTokenUser(handle)
	owner, ownerErr := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || ownerErr != nil || sid == nil || owner == nil || owner.User.Sid == nil || !sid.Equals(owner.User.Sid) {
		_ = windows.CloseHandle(handle)
		return nil, hostExitRefused("envelope")
	}
	return &windowsServiceExitHost{handle: handle}, nil
}
func (o *windowsServiceExitHost) exited() (bool, error) {
	state, err := windows.WaitForSingleObject(o.handle, 0)
	if err != nil {
		return false, hostExitRefused("envelope")
	}
	switch state {
	case windows.WAIT_OBJECT_0:
		return true, nil
	case uint32(windows.WAIT_TIMEOUT):
		return false, nil
	default:
		return false, hostExitRefused("envelope")
	}
}
func (o *windowsServiceExitHost) close() { _ = windows.CloseHandle(o.handle) }
