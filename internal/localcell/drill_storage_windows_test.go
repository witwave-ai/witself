package localcell

import (
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func init() {
	drillStorageVolumeRows = append(drillStorageVolumeRows, func(t *testing.T) {
		t.Run("ntfs_supported", func(t *testing.T) {
			volume, err := drillStorageOpened(t, t.TempDir())
			drillAssert(t, err == nil && volume.unit >= 4096 && volume.free > 0, "the runner volume is not a supported NTFS volume")
		})
		// FSCTL_SET_COMPRESSION with COMPRESSION_FORMAT_DEFAULT (1) on a backup-semantics handle.
		t.Run("compressed_refused", func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "compressed")
			name, err := windows.UTF16PtrFromString(dir)
			drillAssert(t, testMkdir(dir, 0o700) == nil && err == nil, "cannot prepare the compressed directory")
			handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
				windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
			drillAssert(t, err == nil, "cannot open the compressed directory")
			format, returned := uint16(1), uint32(0)
			err = windows.DeviceIoControl(handle, windows.FSCTL_SET_COMPRESSION, (*byte)(unsafe.Pointer(&format)), 2, nil, 0, &returned, nil)
			_ = windows.CloseHandle(handle)
			drillAssert(t, err == nil, "cannot compress the drill storage directory")
			_, err = drillStorageOpened(t, dir)
			drillStorageReason(t, err, "unsupported_fs", "drill storage admitted a compressed directory")
		})
	})
}
