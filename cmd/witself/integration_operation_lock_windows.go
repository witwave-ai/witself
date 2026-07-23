//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openIntegrationOperationLockFile(path string) (*os.File, error) {
	securityDescriptor, currentUser, err := integrationOperationLockSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	attributes := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: securityDescriptor,
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		attributes,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	runtime.KeepAlive(securityDescriptor)
	runtime.KeepAlive(currentUser)
	if err != nil {
		return nil, err
	}

	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if information.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("integration lock path is a reparse point or directory")
	}
	if information.NumberOfLinks != 1 {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("integration lock file must have exactly one hard link")
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("integration lock file handle is unavailable")
	}
	return file, nil
}

func validateIntegrationOperationLockIdentity(file *os.File) error {
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &information); err != nil {
		return err
	}
	if information.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		return errors.New("integration lock path is a reparse point or directory")
	}
	if information.NumberOfLinks != 1 {
		return errors.New("integration lock file must have exactly one hard link")
	}
	return nil
}

func secureIntegrationOperationLockFile(file *os.File) error {
	securityDescriptor, currentUser, err := integrationOperationLockSecurityDescriptor()
	if err != nil {
		return err
	}
	existing, err := windows.GetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	owner, _, err := existing.Owner()
	if err != nil || owner == nil || !owner.IsValid() {
		return errors.New("integration lock file owner is unavailable")
	}
	if !owner.Equals(currentUser) &&
		!owner.IsWellKnown(windows.WinLocalSystemSid) &&
		!owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		return fmt.Errorf("integration lock file has an untrusted owner %s", owner.String())
	}
	dacl, _, err := securityDescriptor.DACL()
	if err != nil || dacl == nil {
		return errors.New("build protected integration lock DACL")
	}
	return windows.SetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
}

func integrationOperationLockSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, *windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, nil, errors.New("current Windows user SID is unavailable")
	}
	sid := user.User.Sid
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf(
		"O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)",
		sid.String(),
		sid.String(),
	))
	if err != nil {
		return nil, nil, err
	}
	return descriptor, sid, nil
}

func tryIntegrationOperationLock(file *os.File) (bool, error) {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&overlapped,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func unlockIntegrationOperationLock(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}
