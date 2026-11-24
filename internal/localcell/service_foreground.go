package localcell

import (
	"context"
	"errors"
	"os"
	"runtime"
)

type serviceLeaseIdentity struct {
	Version        int    `json:"version"`
	BootID         string `json:"boot_id"`
	StartIdentity  string `json:"start_identity"`
	PID            int    `json:"pid"`
	Owner          string `json:"owner"`
	InstallationID string `json:"installation_id"`
}

type serviceLeaseRecord struct {
	serviceLeaseIdentity
	Generation      string `json:"generation,omitempty"`
	BindingRevision uint64 `json:"binding_revision,omitempty"`
	PipeName        string `json:"pipe_name,omitempty"`
}

// The Unix wire record predates Windows listener issuance. Its strict decoder
// still rejects every field outside the original identity, even an empty one.
func (r *serviceLeaseRecord) hostFields() any {
	if runtime.GOOS == "windows" {
		return r
	}
	return &r.serviceLeaseIdentity
}

// The stable file is never unlinked: unlinking a held lease permits two hosts
// to own different inodes. The record is diagnostic; the OS lock is the fence.
func (c *Cell) acquireServiceLease(ctx context.Context, d ServiceDescriptor) (*operationLock, error) {
	return c.acquireServiceLeaseWithBoot(ctx, d, serviceBootID)
}

func (c *Cell) acquireServiceLeaseWithBoot(ctx context.Context, d ServiceDescriptor, readBoot func() (string, error)) (*operationLock, error) {
	return c.acquireServiceLeaseForPlatform(ctx, d, readBoot, runtime.GOOS)
}

func (c *Cell) acquireServiceLeaseForPlatform(ctx context.Context, d ServiceDescriptor, readBoot func() (string, error), platform string) (*operationLock, error) {
	if err := privateDir(c.path("service"), false); err != nil {
		return nil, serviceFailureAt(err, "lease_directory")
	}
	if err := servicePrivateRecord(c.path(".service-host.lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, serviceFailureAt(err, "lease_record")
	}
	f, err := openLockFile(ctx, c.path(".service-host.lock"), true)
	if err != nil {
		return nil, serviceContextReadFailure(ctx, err, errors.New("localcell: service_lease_unavailable"))
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
	record := serviceLeaseRecord{
		serviceLeaseIdentity: serviceLeaseIdentity{Version: 1, BootID: boot, StartIdentity: start, PID: os.Getpid(), Owner: d.UID, InstallationID: d.InstallationID},
		PipeName:             "",
	}
	if platform == "windows" {
		record.Version = 2
	}
	if err := c.publishServiceLease(c.path("service", "lease.json"), record); err != nil {
		_ = lock.Close()
		return nil, serviceFailureAt(err, "lease_write")
	}
	return lock, nil
}
