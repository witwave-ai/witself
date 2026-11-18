//go:build windows

package localcell

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// drillPlatformVolume reads, by the held directory handle alone and writing
// nothing, the file system name, the directory's attributes and, by ntdll's
// NtQueryVolumeInformationFile (no x/sys wrapper), FILE_REMOTE_DEVICE (0x10) of
// class 4 FileFsDeviceInformation and the units of class 7 FileFsFullSizeInformation.
func drillPlatformVolume(f *os.File) (drillVolume, error) {
	handle, refused := windows.Handle(f.Fd()), drillStorageError{reason: "boundary_refused"}
	query := windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQueryVolumeInformationFile")
	var name [32]uint16
	var info windows.ByHandleFileInformation
	if windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, nil, &name[0], uint32(len(name))) != nil ||
		windows.GetFileInformationByHandle(handle, &info) != nil || query.Find() != nil {
		return drillVolume{}, refused
	}
	var status windows.IO_STATUS_BLOCK
	var device struct{ deviceType, characteristics uint32 }
	var size struct {
		total, caller, actual int64
		sectors, bytes        uint32
	}
	if r, _, _ := query.Call(uintptr(handle), uintptr(unsafe.Pointer(&status)), uintptr(unsafe.Pointer(&device)), unsafe.Sizeof(device), 4); r != 0 {
		return drillVolume{}, refused
	}
	if r, _, _ := query.Call(uintptr(handle), uintptr(unsafe.Pointer(&status)), uintptr(unsafe.Pointer(&size)), unsafe.Sizeof(size), 7); r != 0 {
		return drillVolume{}, refused
	}
	if !drillWindowsSupported(windows.UTF16ToString(name[:]), device.characteristics&0x10 != 0, info.FileAttributes&windows.FILE_ATTRIBUTE_COMPRESSED != 0) {
		return drillVolume{}, drillStorageError{reason: "unsupported_fs"}
	}
	unit := uint64(size.sectors) * uint64(size.bytes)
	if size.caller < 0 || unit == 0 {
		return drillVolume{}, refused
	}
	return drillVolume{unit: max(unit, checkpointBlock), free: drillStorageMul(uint64(size.caller), unit)}, nil
}
