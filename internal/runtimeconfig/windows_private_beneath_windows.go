//go:build windows

package runtimeconfig

import (
	"errors"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WindowsPrivateBeneath opens or creates the object that name, a
// slash-separated relative path, names below the held root, without resolving
// any pathname: each component is opened relative to its parent's
// handle with FILE_OPEN_REPARSE_POINT, so no reparse point is followed. Each
// component before the last must be a directory that is not a reparse point,
// on root's volume, and is held, denying delete sharing, until the call
// returns, so it can be neither renamed nor removed meanwhile. With create the
// last component is created exclusively (FILE_CREATE) with the exact protected
// DACL applied at creation; otherwise the existing object is opened for
// reading. The object is validated by handle before it is returned; a created
// object that fails is deleted through its own handle. A malformed name is
// refused before any system call. Errors carry no path.
func WindowsPrivateBeneath(root *os.Root, name string, create, directory bool) (*os.File, error) {
	return windowsPrivateBeneath(root, name, create, directory, nil)
}

// held is a native race-test seam after each component before the last is
// opened and checked; nil in production.
func windowsPrivateBeneath(root *os.Root, name string, create, directory bool, held func(int) error) (*os.File, error) {
	parts, ok := windowsPrivateBeneathParts(name)
	for _, part := range parts {
		clean, err := windowsPrivateCleanPath(`C:\` + part)
		ok = ok && err == nil && clean == `C:\`+part
	}
	if !ok || root == nil {
		return nil, windowsPrivateFailure(nil)
	}
	directoryHandle, err := root.Open(".")
	if err != nil {
		errno := windows.ERROR_INVALID_HANDLE // a closed root has no system error
		_ = errors.As(err, &errno)
		return nil, windowsPrivateFailure(errno)
	}
	defer func() { _ = directoryHandle.Close() }()
	parent := windows.Handle(directoryHandle.Fd())
	var rootInfo, info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(parent, &rootInfo); err != nil {
		return nil, windowsPrivateFailure(err)
	}
	for index, part := range parts[:len(parts)-1] {
		next, err := windowsPrivateAt(parent, part, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
		if err != nil {
			return nil, err
		}
		defer func() { _ = windows.CloseHandle(next) }()
		err = windows.GetFileInformationByHandle(next, &info)
		if err != nil || !windowsPrivateBeneathDirectory(info.FileAttributes, info.VolumeSerialNumber, rootInfo.VolumeSerialNumber) {
			return nil, windowsPrivateFailure(err)
		}
		if held != nil && held(index) != nil {
			return nil, windowsPrivateFailure(nil)
		}
		parent = next
	}
	access, disposition, options := uint32(windows.GENERIC_READ), uint32(windows.FILE_OPEN), uint32(windows.FILE_NON_DIRECTORY_FILE)
	if directory {
		access, options = windows.FILE_TRAVERSE, windows.FILE_DIRECTORY_FILE
	}
	var descriptor *windows.SECURITY_DESCRIPTOR
	if create {
		var err error
		if descriptor, err = windowsPrivateDescriptor(); err != nil {
			return nil, err
		}
		if access, disposition = access|windows.DELETE, windows.FILE_CREATE; !directory {
			access = windows.GENERIC_WRITE | windows.DELETE // DELETE: the rollback goes through this handle
		}
	}
	handle, err := windowsPrivateAt(parent, parts[len(parts)-1], access|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, disposition, options, descriptor)
	if err != nil {
		return nil, err
	}
	if err := windowsPrivateValidateHandle(handle, directory); err != nil {
		if create {
			err = windowsPrivateRollback(handle, err)
		}
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return os.NewFile(uintptr(handle), "private object"), nil
}

// windowsPrivateAt opens or creates one component relative to parent with
// FILE_OPEN_REPARSE_POINT: a reparse point is opened as itself, never
// followed, and FILE_CREATE never opens an existing object. An existing name
// is reported as ERROR_FILE_EXISTS, other failures as the system error alone.
func windowsPrivateAt(parent windows.Handle, name string, access, share, disposition, options uint32, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	object, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, windowsPrivateFailure(err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: object, Attributes: windows.OBJ_CASE_INSENSITIVE, SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	handle := windows.InvalidHandle
	err = windows.NtCreateFile(&handle, access|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, &attributes, &windows.IO_STATUS_BLOCK{}, nil,
		windows.FILE_ATTRIBUTE_NORMAL, share, disposition, options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	runtime.KeepAlive(descriptor)
	var status windows.NTStatus
	switch {
	case err == nil:
		return handle, nil
	case !errors.As(err, &status):
		return windows.InvalidHandle, windowsPrivateFailure(err)
	case status == windows.STATUS_OBJECT_NAME_COLLISION, status == windows.STATUS_FILE_IS_A_DIRECTORY && disposition == windows.FILE_CREATE:
		return windows.InvalidHandle, windowsPrivateFailure(windows.ERROR_FILE_EXISTS)
	}
	return windows.InvalidHandle, windowsPrivateFailure(status.Errno())
}
