//go:build windows

package runtimeconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WindowsObjectIdentity is what one handle reports about its object: the
// volume serial number and file index that identify it, its attributes, its
// link count and pending deletion, its end of file, and its change and
// last-write times (FILE_BASIC_INFO).
type WindowsObjectIdentity struct {
	Volume                uint32
	Index                 uint64
	Attributes, Links     uint32
	DeletePending         bool
	Size                  int64
	ChangeTime, WriteTime int64
}

// windowsBasicInfo is FILE_BASIC_INFO and windowsStandardInfo FILE_STANDARD_INFO
// (winbase.h), 40 and 24 bytes.
type windowsBasicInfo struct {
	creation, access, write, change int64
	attributes, _                   uint32
}

type windowsStandardInfo struct {
	allocation, end          int64
	links                    uint32
	deletePending, directory uint8 // BOOLEAN: a byte, read as a number and never as a Go bool
}

// WindowsPrivateHoldPath holds the existing directory at path. The path guard
// opens every component without following a reparse point and requires a local
// NTFS volume; the directory is then opened relative to its held parent with
// FILE_TRAVERSE|READ_CONTROL, sharing read and write but not delete, so that
// neither it nor, while it is open, any ancestor can be renamed or removed, and
// the guard verifies that the path still names it. It does not validate the
// object's descriptor: the caller does, by handle, before trusting it.
func WindowsPrivateHoldPath(path string) (*os.File, error) {
	guard, clean, err := windowsPrivateGuard(path)
	if err != nil {
		return nil, err
	}
	defer guard.close()
	if len(guard) < 2 || guard[len(guard)-1].path != clean {
		return nil, windowsPrivateFailure(os.ErrNotExist)
	}
	handle, err := windowsPrivateAt(guard[len(guard)-2].handle, filepath.Base(clean), windows.FILE_TRAVERSE|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
	if err != nil {
		return nil, err
	}
	if err := guard.verify(clean, handle, true); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return os.NewFile(uintptr(handle), clean), nil
}

// WindowsPrivateHold opens the existing object name, one component, relative
// to the held directory parent, without following a reparse point (one is
// opened as itself) and never creating. A directory is held FILE_TRAVERSE|
// READ_CONTROL sharing read and write; any other form is held GENERIC_READ|
// GENERIC_WRITE|READ_CONTROL sharing read only, so it fails while another
// handle may write, append or delete and, while it is open, nothing else can.
// Neither shares delete. A missing name unwraps to ERROR_FILE_NOT_FOUND and a
// sharing refusal to ERROR_SHARING_VIOLATION. Like WindowsPrivateHoldPath it
// does not validate: the caller refuses a reparse point, the wrong type or a
// descriptor that is not private, by handle.
func WindowsPrivateHold(parent *os.File, name string, directory bool) (*os.File, error) {
	parts, ok := windowsPrivateBeneathParts(name)
	clean, err := windowsPrivateCleanPath(`C:\` + name)
	if !ok || len(parts) != 1 || err != nil || clean != `C:\`+name || parent == nil {
		return nil, windowsPrivateFailure(nil)
	}
	access, share, options := uint32(windows.GENERIC_READ|windows.GENERIC_WRITE), uint32(windows.FILE_SHARE_READ), uint32(0)
	if directory {
		access, share, options = windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_DIRECTORY_FILE
	}
	handle, err := windowsPrivateAt(windows.Handle(parent.Fd()), name, access|windows.READ_CONTROL, share, windows.FILE_OPEN, options, nil)
	runtime.KeepAlive(parent)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), "private hold"), nil
}

// WindowsPrivateIdentity reads the identity of an opened object by handle.
func WindowsPrivateIdentity(file *os.File) (WindowsObjectIdentity, error) {
	if file == nil {
		return WindowsObjectIdentity{}, windowsPrivateFailure(nil)
	}
	identity, err := windowsPrivateHandleIdentity(windows.Handle(file.Fd()))
	runtime.KeepAlive(file)
	return identity, err
}

// WindowsPrivateNameIdentity reads the identity of what path names now through
// an attribute-only handle that shares everything, takes no part in sharing
// checks and opens a reparse point at the last element as itself. The handle
// is closed before it returns.
func WindowsPrivateNameIdentity(path string) (WindowsObjectIdentity, error) {
	clean, err := windowsPrivateCleanPath(path)
	if err != nil {
		return WindowsObjectIdentity{}, err
	}
	handle, err := windowsPrivateOpenMetadata(clean, 0)
	if err != nil {
		return WindowsObjectIdentity{}, windowsPrivateFailure(err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	return windowsPrivateHandleIdentity(handle)
}

func windowsPrivateHandleIdentity(handle windows.Handle) (WindowsObjectIdentity, error) {
	var info windows.ByHandleFileInformation
	var basic windowsBasicInfo
	var standard windowsStandardInfo
	err := windows.GetFileInformationByHandle(handle, &info)
	if err == nil {
		err = windows.GetFileInformationByHandleEx(handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)))
	}
	if err == nil {
		err = windows.GetFileInformationByHandleEx(handle, windows.FileStandardInfo, (*byte)(unsafe.Pointer(&standard)), uint32(unsafe.Sizeof(standard)))
	}
	if err != nil {
		return WindowsObjectIdentity{}, windowsPrivateFailure(err)
	}
	return WindowsObjectIdentity{Volume: info.VolumeSerialNumber, Index: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
		Attributes: info.FileAttributes, Links: standard.links, DeletePending: standard.deletePending != 0, Size: standard.end,
		ChangeTime: basic.change, WriteTime: basic.write}, nil
}
