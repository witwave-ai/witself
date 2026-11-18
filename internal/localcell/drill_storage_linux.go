//go:build linux

package localcell

import (
	"os"

	"golang.org/x/sys/unix"
)

// drillPlatformVolume reads the held directory's volume by fstatfs alone and
// writes nothing: ext4 only, its unit at least 4 KiB, and the bytes the volume
// leaves to an unprivileged user (f_bavail).
func drillPlatformVolume(f *os.File) (drillVolume, error) {
	var stat unix.Statfs_t
	if unix.Fstatfs(int(f.Fd()), &stat) != nil {
		return drillVolume{}, drillStorageError{reason: "boundary_refused"}
	}
	if !drillLinuxSupported(uint64(stat.Type), uint64(stat.Frsize), uint64(stat.Bsize)) {
		return drillVolume{}, drillStorageError{reason: "unsupported_fs"}
	}
	return drillVolume{unit: max(uint64(stat.Frsize), uint64(stat.Bsize), checkpointBlock), free: drillStorageMul(stat.Bavail, uint64(stat.Frsize))}, nil
}
