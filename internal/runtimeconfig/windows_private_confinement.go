//go:build windows

package runtimeconfig

import (
	"errors"

	"golang.org/x/sys/windows"
)

var (
	errWindowsPrivatePathChanged = errors.New("private Windows parent identity changed")
	errWindowsPrivateReparse     = errors.New("private Windows reparse detected after creation")
	errWindowsPrivateRollback    = errors.New("private Windows creation rollback failed")
)

func windowsPrivateSameIdentity(left, right windows.ByHandleFileInformation) bool {
	return left.VolumeSerialNumber == right.VolumeSerialNumber &&
		left.FileIndexHigh == right.FileIndexHigh && left.FileIndexLow == right.FileIndexLow
}

func windowsPrivateOpenMetadata(path string, access uint32) (windows.Handle, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, windowsPrivateFailure(err)
	}
	return windows.CreateFile(pointer, access|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

// verify detects pathname redirection after creation. Reopen the created leaf
// without following its final reparse point, compare it to the creation handle,
// and compare each retained ancestor to both its original and current identity.
// This does not prevent creation through a concurrently installed junction.
func (guard windowsPrivatePathGuard) verify(path string, created windows.Handle, directory bool) error {
	leaf, err := windowsPrivateOpenMetadata(path, 0)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	defer func() { _ = windows.CloseHandle(leaf) }()
	var actual, reopened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(created, &actual); err != nil {
		return windowsPrivateFailure(err)
	}
	if err := windows.GetFileInformationByHandle(leaf, &reopened); err != nil {
		return windowsPrivateFailure(err)
	}
	if actual.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || reopened.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errWindowsPrivateReparse
	}
	if !windowsPrivateSameIdentity(actual, reopened) || (actual.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return errWindowsPrivatePathChanged
	}
	for _, entry := range guard {
		var retained windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(entry.handle, &retained); err != nil {
			return windowsPrivateFailure(err)
		}
		if retained.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errWindowsPrivateReparse
		}
		current, err := windowsPrivateOpenMetadata(entry.path, 0)
		if err != nil {
			return windowsPrivateFailure(err)
		}
		var info windows.ByHandleFileInformation
		err = windows.GetFileInformationByHandle(current, &info)
		_ = windows.CloseHandle(current)
		if err != nil {
			return windowsPrivateFailure(err)
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errWindowsPrivateReparse
		}
		if !windowsPrivateSameIdentity(entry.identity, retained) || !windowsPrivateSameIdentity(retained, info) {
			return errWindowsPrivatePathChanged
		}
	}
	return nil
}

// A failed path check must never choose a deletion target by that same path.
// FILE_DISPOSITION_INFO contains one BOOLEAN byte and requires DELETE access.
// Closing the caller's handle completes deletion of this exact created object.
func windowsPrivateRollback(handle windows.Handle, cause error) error {
	remove := byte(1)
	if err := windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo, &remove, 1); err != nil {
		return errWindowsPrivateRollback
	}
	return cause
}
