//go:build windows

package dashboard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lockRegistryClaim takes a blocking exclusive byte-range lock. CreateFile
// opens the final path component itself rather than following a reparse point,
// and omitting FILE_SHARE_DELETE keeps that identity stable for the lifetime of
// the handle. Other dashboard processes may still open the file and block in
// LockFileEx, preserving the Unix flock serialization contract.
func lockRegistryClaim(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}

	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("dashboard: inspect registry claim lock %s: %w", path, err)
	}
	if information.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("dashboard: registry claim lock %s is a reparse point or directory", path)
	}

	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("dashboard: adopt registry claim lock %s", path)
	}
	overlapped := &windows.Overlapped{}
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, overlapped); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("dashboard: lock registry claim %s: %w", path, err)
	}
	if err := validateLockedRegistryFile(path, file); err != nil {
		_ = windows.UnlockFileEx(handle, 0, 1, 0, overlapped)
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(handle, 0, 1, 0, overlapped)
		_ = file.Close()
	}, nil
}

// pidRunning checks a process handle without sending a signal. A zero-timeout
// wait distinguishes a live process from an exited one; access denied still
// proves existence for protected processes, while unexpected probe failures are
// left unknown rather than being misreported as dead.
func pidRunning(pid int) (running, known bool) {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return false, false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		switch {
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			return true, true
		case errors.Is(err, windows.ERROR_INVALID_PARAMETER):
			return false, true
		default:
			return false, false
		}
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	result, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, false
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		return false, true
	case uint32(windows.WAIT_TIMEOUT):
		return true, true
	default:
		return false, false
	}
}
