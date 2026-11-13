//go:build windows

package runtimeconfig

import (
	"os"

	"golang.org/x/sys/windows"
)

// WindowsPrivatePostgresOpen is WindowsPrivatePostgresRead without the read:
// the same boundary, share flags, reparse refusal and handle and ancestor
// checks; the caller owns the returned handle and bounds what it reads.
func WindowsPrivatePostgresOpen(root, path string) (*os.File, error) {
	if err := WindowsPrivatePostgresValidate(root, path, false); err != nil {
		return nil, err
	}
	guard, clean, err := windowsPrivateGuard(path)
	if err != nil {
		return nil, err
	}
	defer guard.close()
	pointer, err := windows.UTF16PtrFromString(clean)
	if err != nil {
		return nil, windowsPrivateFailure(err)
	}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, windowsPrivateFailure(err)
	}
	file := os.NewFile(uintptr(handle), clean)
	if err := windowsPostgresValidateHandle(handle, false, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := guard.verify(clean, handle, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
