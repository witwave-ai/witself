//go:build windows

package runtimeconfig

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// WindowsPrivateOpenSnapshot opens an existing private regular file for reading.
// The containing directory must also be private. The returned handle can finish
// reading its original object across a private publisher's atomic replacement.
func WindowsPrivateOpenSnapshot(path string) (*os.File, error) {
	return windowsPrivateSnapshot(path, true, nil)
}

// WindowsPrivateValidateSnapshot checks an ordinary file reported by a private
// tree walk. The walker checks each directory separately. This form needs only
// the leaf's original guard and security-query rights, without data access.
func WindowsPrivateValidateSnapshot(path string) error {
	file, err := windowsPrivateSnapshot(path, false, nil)
	if err != nil {
		return err
	}
	_ = file.Close()
	return nil
}

// Starting at the volume root, open each single component relative to the
// actual held parent, with FILE_OPEN_REPARSE_POINT. An ancestor's name cannot
// be replaced while its traverse handle denies delete sharing. Its reparse
// attributes can change in place, so check the held chain again after opening
// the leaf. Only this last handle permits replacement. No name lookup can
// substitute a different object for its security check or its content.
func windowsPrivateSnapshot(path string, content bool, opened func()) (*os.File, error) {
	clean, err := windowsPrivateCleanPath(path)
	if err != nil {
		return nil, err
	}
	if len(clean) == 3 { // a drive root is a directory
		return nil, windowsPrivateFailure(nil)
	}
	parts := strings.Split(clean[3:], `\`)
	var guard windowsPrivatePathGuard
	defer func() { guard.close() }()
	var parent windows.Handle
	var volume uint32
	for i := range parts {
		access := uint32(windows.FILE_TRAVERSE)
		privateParent := content && i == len(parts)-1
		if privateParent {
			access |= windows.READ_CONTROL
		}
		if i == 0 {
			parent, err = windowsPrivateSnapshotVolume(clean[:3], access)
		} else {
			parent, err = windowsPrivateSnapshotAt(parent, parts[i-1], access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, true)
		}
		if err != nil {
			return nil, err
		}
		guard = append(guard, windowsPrivateGuardEntry{handle: parent})
		info := &guard[i].identity
		if err := windows.GetFileInformationByHandle(parent, info); err != nil {
			return nil, windowsPrivateFailure(err)
		}
		if i == 0 {
			volume = info.VolumeSerialNumber
		}
		if !windowsPrivateBeneathDirectory(info.FileAttributes, info.VolumeSerialNumber, volume) {
			return nil, windowsPrivateFailure(nil)
		}
		if privateParent {
			if err := windowsPrivateValidateHandle(parent, true); err != nil {
				return nil, err
			}
		}
	}

	access := uint32(windows.FILE_EXECUTE | windows.READ_CONTROL)
	if content {
		access |= windows.GENERIC_READ
	}
	handle, err := windowsPrivateSnapshotAt(parent, parts[len(parts)-1], access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, false)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), "private object")
	if opened != nil {
		opened()
	}
	kind, err := windows.GetFileType(handle)
	if err != nil || kind != windows.FILE_TYPE_DISK {
		_ = file.Close()
		return nil, windowsPrivateFailure(err)
	}
	if err := windowsPrivateValidateHandle(handle, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	for _, entry := range guard {
		var current windows.ByHandleFileInformation
		err := windows.GetFileInformationByHandle(entry.handle, &current)
		if err != nil || current.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || !windowsPrivateSameIdentity(entry.identity, current) {
			_ = file.Close()
			return nil, windowsPrivateFailure(err)
		}
	}
	return file, nil
}

// The same drive and NTFS restrictions as the pathname guard. Request
// READ_CONTROL on the drive root only when it is the direct private parent.
func windowsPrivateSnapshotVolume(root string, access uint32) (windows.Handle, error) {
	pointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return windows.InvalidHandle, windowsPrivateFailure(err)
	}
	if drive := windows.GetDriveType(pointer); drive != windows.DRIVE_FIXED && drive != windows.DRIVE_REMOVABLE {
		return windows.InvalidHandle, windowsPrivateFailure(nil)
	}
	handle, err := windows.CreateFile(pointer, access|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return windows.InvalidHandle, windowsPrivateFailure(err)
	}
	var filesystem [32]uint16
	if err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, nil, &filesystem[0], uint32(len(filesystem))); err != nil || windows.UTF16ToString(filesystem[:]) != "NTFS" {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, windowsPrivateFailure(err)
	}
	return handle, nil
}

// A reparse object could deny READ_CONTROL. Preserve the guard's earlier
// reparse refusal in that case. This diagnostic open cannot authorize a read.
func windowsPrivateSnapshotAt(parent windows.Handle, name string, access, share uint32, ancestor bool) (windows.Handle, error) {
	handle, err := windowsPrivateAt(parent, name, access, share, windows.FILE_OPEN, 0, nil)
	if access&windows.READ_CONTROL != 0 && errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		guard, guardErr := windowsPrivateAt(parent, name, windows.FILE_TRAVERSE, share, windows.FILE_OPEN, 0, nil)
		if guardErr == nil {
			var info windows.ByHandleFileInformation
			guardErr = windows.GetFileInformationByHandle(guard, &info)
			_ = windows.CloseHandle(guard)
			if guardErr == nil && (info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || ancestor && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0) {
				return windows.InvalidHandle, windowsPrivateFailure(nil)
			}
		}
	}
	return handle, err
}
