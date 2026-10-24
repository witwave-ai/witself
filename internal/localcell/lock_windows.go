//go:build windows

package localcell

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func openLockFile(path string, create bool) (*os.File, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	file, err := openPrivateFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	if err := validateLockFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func lockFile(file *os.File) error {
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrLocked
	}
	return err
}

func unlockFile(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
}
