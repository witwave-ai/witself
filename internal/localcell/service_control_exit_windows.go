//go:build windows

package localcell

import "golang.org/x/sys/windows"

type windowsServiceExitHost struct{ handle windows.Handle }

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
