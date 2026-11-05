package localcell

import "os"

// NativeServiceControlCapability bridges the explicit native foreground fixture
// to the private capability seam without adding a production API.
func NativeServiceControlCapability() error { return serviceControlCapability() }

// NativeServiceLeaseReleased checks the stable lock directly, preserving the
// lease JSON and the inode that supplies lifetime ownership authority.
func NativeServiceLeaseReleased(c *Cell) bool {
	f, err := openLockFile(c.path(".service-host.lock"), false)
	if err != nil {
		return false
	}
	lock := &operationLock{files: []*os.File{f}}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return false
	}
	return lock.Close() == nil
}

func NativeServiceEndpointBootMatches(boot string) bool {
	current, err := serviceBootID()
	return err == nil && boot != "" && current == boot
}
