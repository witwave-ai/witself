//go:build windows

package runtimeconfig

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PostgreSQL creates its own descendants. Its PGDATA boundary therefore has
// inheritable current-user/SYSTEM grants; ordinary local-cell control files
// retain their exact protected, non-inheritable descriptor. No existing object
// is repaired, and inheritance never permits another principal or weaker mask.
func windowsPostgresDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	descriptor, err := windowsPrivateDescriptor()
	if err != nil {
		return nil, err
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		return nil, windowsPrivateFailure(err)
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return nil, windowsPrivateFailure(err)
		}
		ace.Header.AceFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	return descriptor, nil
}

func windowsPostgresValidateDescriptor(descriptor *windows.SECURITY_DESCRIPTOR, directory, root bool) error {
	// Explicit private files are also safe inside PGDATA (e.g. our atomically
	// published configuration). Existing strict synthetic fixtures remain valid.
	if err := windowsPrivateValidateDescriptor(descriptor); err == nil {
		return nil
	}
	if descriptor == nil || !descriptor.IsValid() {
		return windowsPrivateFailure(nil)
	}
	control, _, err := descriptor.Control()
	if err != nil || (control&windows.SE_DACL_PROTECTED != 0) != root {
		return windowsPrivateFailure(err)
	}
	// Normalize a private copy solely for comparison with the strict policy.
	copyDescriptor, err := descriptor.ToAbsolute()
	if err != nil {
		return windowsPrivateFailure(err)
	}
	copyDescriptor, err = copyDescriptor.ToSelfRelative()
	if err != nil {
		return windowsPrivateFailure(err)
	}
	acl, _, err := copyDescriptor.DACL()
	if err != nil || acl == nil {
		return windowsPrivateFailure(err)
	}
	wantFlags := byte(windows.INHERITED_ACE)
	if directory {
		wantFlags |= windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	if root {
		if !directory {
			return windowsPrivateFailure(nil)
		}
		wantFlags &^= windows.INHERITED_ACE
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil || ace == nil || ace.Header.AceFlags != wantFlags {
			return windowsPrivateFailure(err)
		}
		ace.Header.AceFlags = 0
	}
	if err := copyDescriptor.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return windowsPrivateFailure(err)
	}
	return windowsPrivateValidateDescriptor(copyDescriptor)
}

func windowsPostgresValidateHandle(handle windows.Handle, directory, root bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return windowsPrivateFailure(err)
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	return windowsPostgresValidateDescriptor(descriptor, directory, root)
}

// WindowsPrivatePostgresMkdir secures PGDATA before initdb writes any bytes.
// A pre-existing directory, even an empty insecure one, is refused unchanged.
func WindowsPrivatePostgresMkdir(path string) error {
	guard, clean, err := windowsPrivateGuard(path)
	if err != nil {
		return err
	}
	defer guard.close()
	descriptor, err := windowsPostgresDescriptor()
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	pointer, err := windows.UTF16PtrFromString(clean)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	err = windows.CreateDirectory(pointer, &attributes)
	runtime.KeepAlive(descriptor)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	handle, err := windowsPrivateOpenMetadata(clean, windows.DELETE|windows.READ_CONTROL)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := guard.verify(clean, handle, true); err != nil {
		return windowsPrivateRollback(handle, err)
	}
	return windowsPostgresValidateHandle(handle, true, true)
}

// WindowsPrivatePostgresValidate checks the PGDATA boundary and every directory
// down to the requested object. Paths must stay inside that exact boundary.
func WindowsPrivatePostgresValidate(root, path string, directory bool) error {
	root, err := windowsPrivateCleanPath(root)
	if err != nil {
		return err
	}
	guard, clean, err := windowsPrivateGuard(path)
	if err != nil {
		return err
	}
	defer guard.close()
	relative, err := filepath.Rel(root, clean)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return windowsPrivateFailure(err)
	}
	var leaf windows.Handle
	for current := clean; ; current = filepath.Dir(current) {
		handle, err := windowsPrivateOpenMetadata(current, windows.READ_CONTROL)
		if err != nil {
			return windowsPrivateFailure(err)
		}
		atRoot := strings.EqualFold(current, root)
		err = windowsPostgresValidateHandle(handle, current != clean || directory, atRoot)
		if current == clean {
			leaf = handle
			defer func() { _ = windows.CloseHandle(leaf) }()
		} else {
			_ = windows.CloseHandle(handle)
		}
		if err != nil {
			return err
		}
		if atRoot {
			return guard.verify(clean, leaf, directory)
		}
	}
}

// WindowsPrivatePostgresRead reads an existing bounded PG-generated record
// after checking the descriptor of the opened object, never only its pathname.
func WindowsPrivatePostgresRead(root, path string) ([]byte, error) {
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
	defer func() { _ = file.Close() }()
	if err := windowsPostgresValidateHandle(handle, false, false); err != nil {
		return nil, err
	}
	if err := guard.verify(clean, handle, false); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(file, 8193))
}

// WindowsPrivatePostgresRename publishes a newly created strict-private file
// over a verified inherited-private PG file, using the same atomic rename fence.
func WindowsPrivatePostgresRename(root, oldPath, newPath string) error {
	validate := func(path string, directory bool) error {
		err := WindowsPrivatePostgresValidate(root, path, directory)
		if errors.Is(err, os.ErrNotExist) {
			if parentErr := WindowsPrivatePostgresValidate(root, filepath.Dir(path), true); parentErr != nil {
				return parentErr
			}
		}
		return err
	}
	return windowsPrivateRename(oldPath, newPath, validate, func(handle windows.Handle, directory bool) error {
		return windowsPostgresValidateHandle(handle, directory, false)
	})
}
