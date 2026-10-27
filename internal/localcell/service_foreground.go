package localcell

import (
	"errors"
	"os"
)

type serviceLeaseRecord struct {
	Version        int    `json:"version"`
	BootID         string `json:"boot_id"`
	StartIdentity  string `json:"start_identity"`
	PID            int    `json:"pid"`
	Owner          string `json:"owner"`
	InstallationID string `json:"installation_id"`
}

// The stable file is never unlinked: unlinking a held lease permits two hosts
// to own different inodes. The record is diagnostic; the OS lock is the fence.
func (c *Cell) acquireServiceLease(d ServiceDescriptor) (*operationLock, error) {
	return c.acquireServiceLeaseWithBoot(d, serviceBootID)
}

func (c *Cell) acquireServiceLeaseWithBoot(d ServiceDescriptor, readBoot func() (string, error)) (*operationLock, error) {
	if err := privateDir(c.path("service"), false); err != nil {
		return nil, err
	}
	if err := servicePrivateRecord(c.path(".service-host.lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := openLockFile(c.path(".service-host.lock"), true)
	if err != nil {
		return nil, errors.New("localcell: service_lease_unavailable")
	}
	lock := &operationLock{files: []*os.File{f}}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, ErrLocked) {
			return nil, ErrServiceHostActive
		}
		return nil, errors.New("localcell: service_lease_unavailable")
	}
	start, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		_ = lock.Close()
		return nil, errors.New("localcell: service_host_identity_unavailable")
	}
	boot, err := readBoot()
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	record := serviceLeaseRecord{Version: 1, BootID: boot, StartIdentity: start, PID: os.Getpid(), Owner: d.UID, InstallationID: d.InstallationID}
	if err := writeAtomic(c.path("service", "lease.json"), record, nil); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}
