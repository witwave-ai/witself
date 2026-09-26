//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

func consolePIDDead(pid int) bool {
	if pid <= 1 || uint64(pid) > uint64(^uint32(0)) {
		return false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	result, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && result == windows.WAIT_OBJECT_0
}
