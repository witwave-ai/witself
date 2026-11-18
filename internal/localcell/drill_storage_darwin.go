//go:build darwin

package localcell

import (
	"os"

	"golang.org/x/sys/unix"
)

// drillPlatformVolume refuses every macOS volume once fstatfs has read it, and
// writes nothing: on APFS a write to blocks a snapshot or clone shares goes
// elsewhere, so no preallocation is proved (LC-S7D2G4-SPEC1); HFS+ has no
// native evidence row.
func drillPlatformVolume(f *os.File) (drillVolume, error) {
	var stat unix.Statfs_t
	if unix.Fstatfs(int(f.Fd()), &stat) != nil {
		return drillVolume{}, drillStorageError{reason: "boundary_refused"}
	}
	return drillVolume{}, drillStorageError{reason: "unsupported_fs"}
}
