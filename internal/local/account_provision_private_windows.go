//go:build windows

package local

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These candidate primitives have no journal callers yet. They establish private
// leaf creation and handle identity, not ancestor safety or publication durability.
// Integration must pin and validate the application-owned directory chain first.

// FILE_ALL_ACCESS from WinNT.h; generic access masks are not accepted in an ACE.
const accountProvisionWindowsFullAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

func accountProvisionWindowsUser() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, ErrAccountProvisionJournalStorage
	}
	return user.User.Sid, nil
}

func accountProvisionWindowsPrincipals(user *windows.SID) ([]*windows.SID, error) {
	if user == nil || !user.IsValid() {
		return nil, ErrAccountProvisionJournalUnsafe
	}
	principals := []*windows.SID{user}
	for _, kind := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(kind)
		if err != nil {
			return nil, ErrAccountProvisionJournalStorage
		}
		duplicate := false
		for _, existing := range principals {
			duplicate = duplicate || existing.Equals(sid)
		}
		if !duplicate {
			principals = append(principals, sid)
		}
	}
	return principals, nil
}

func accountProvisionWindowsDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := accountProvisionWindowsUser()
	if err != nil {
		return nil, err
	}
	principals, err := accountProvisionWindowsPrincipals(user)
	if err != nil {
		return nil, err
	}
	var sddl strings.Builder
	sddl.WriteString("O:" + user.String() + "D:P")
	for _, sid := range principals {
		sddl.WriteString("(A;;FA;;;" + sid.String() + ")")
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl.String())
	if err != nil || !validAccountProvisionWindowsDescriptor(descriptor, user) {
		return nil, ErrAccountProvisionJournalStorage
	}
	return descriptor, nil
}

func validAccountProvisionWindowsDescriptor(descriptor *windows.SECURITY_DESCRIPTOR, user *windows.SID) bool {
	defer func() { runtime.KeepAlive(descriptor) }()
	if descriptor == nil || !descriptor.IsValid() {
		return false
	}
	principals, err := accountProvisionWindowsPrincipals(user)
	if err != nil {
		return false
	}
	control, revision, err := descriptor.Control()
	const required = windows.SE_DACL_PRESENT | windows.SE_DACL_PROTECTED
	const inherited = windows.SE_DACL_DEFAULTED | windows.SE_DACL_AUTO_INHERIT_REQ | windows.SE_DACL_AUTO_INHERITED
	if err != nil || revision != 1 || control&required != required || control&inherited != 0 {
		return false
	}
	owner, defaulted, err := descriptor.Owner()
	if err != nil || defaulted || owner == nil || !owner.IsValid() {
		return false
	}
	ownerTrusted := false
	for _, sid := range principals {
		ownerTrusted = ownerTrusted || owner.Equals(sid)
	}
	if !ownerTrusted {
		return false
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil || defaulted || dacl == nil || int(dacl.AceCount) != len(principals) {
		return false
	}
	seen := make([]bool, len(principals))
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil || ace == nil ||
			ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 ||
			ace.Header.AceSize < 20 || ace.Mask != accountProvisionWindowsFullAccess {
			return false
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || int(ace.Header.AceSize) != int(unsafe.Offsetof(ace.SidStart))+sid.Len() {
			return false
		}
		matched := false
		for j, allowed := range principals {
			if sid.Equals(allowed) {
				if seen[j] {
					return false
				}
				seen[j], matched = true, true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func validateAccountProvisionWindowsPrivateHandle(file *os.File, path string, expected os.FileInfo, directory bool) error {
	defer func() { runtime.KeepAlive(file) }()
	if file == nil || expected == nil {
		return ErrAccountProvisionJournalUnsafe
	}
	opened, openErr := file.Stat()
	linked, linkErr := os.Lstat(path)
	var information windows.ByHandleFileInformation
	if openErr != nil || linkErr != nil ||
		windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &information) != nil ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		(information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory ||
		opened.IsDir() != directory || linked.IsDir() != directory ||
		(!directory && (!opened.Mode().IsRegular() || !linked.Mode().IsRegular())) ||
		opened.Mode()&os.ModeSymlink != 0 || linked.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(expected, opened) || !os.SameFile(opened, linked) {
		return ErrAccountProvisionJournalUnsafe
	}
	user, err := accountProvisionWindowsUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || !validAccountProvisionWindowsDescriptor(descriptor, user) {
		return ErrAccountProvisionJournalUnsafe
	}
	final, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, final) || final.Mode()&os.ModeSymlink != 0 {
		return ErrAccountProvisionJournalUnsafe
	}
	return nil
}

// Creation is exclusive. Existing objects are never opened for repair or deleted
// on failure; ambiguous creation may leave an empty object for later inspection.
func createAccountProvisionWindowsPrivateFile(path string) (*os.File, error) {
	descriptor, err := accountProvisionWindowsDescriptor()
	if err != nil {
		return nil, err
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, ErrAccountProvisionJournalUnsafe
	}
	attributes := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, attributes, windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	runtime.KeepAlive(descriptor)
	if err != nil {
		return nil, accountProvisionWindowsCreationError(err)
	}
	return accountProvisionWindowsCreatedHandle(handle, path, false)
}

func createAccountProvisionWindowsPrivateDirectory(path string) (*os.File, error) {
	descriptor, err := accountProvisionWindowsDescriptor()
	if err != nil {
		return nil, err
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, ErrAccountProvisionJournalUnsafe
	}
	attributes := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	err = windows.CreateDirectory(pointer, attributes)
	runtime.KeepAlive(descriptor)
	if err != nil {
		return nil, accountProvisionWindowsCreationError(err)
	}
	// This handle pins the directory against deletion/replacement; it makes no
	// directory-flush or ancestor-pinning claim.
	// MS-FSA sharing checks exclude metadata-only handles. FILE_TRAVERSE
	// participates without requesting directory listing or mutation rights.
	handle, err := windows.CreateFile(pointer, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, ErrAccountProvisionJournalStorage
	}
	return accountProvisionWindowsCreatedHandle(handle, path, true)
}

func accountProvisionWindowsCreatedHandle(handle windows.Handle, path string, directory bool) (*os.File, error) {
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, ErrAccountProvisionJournalStorage
	}
	info, err := file.Stat()
	if err == nil {
		err = validateAccountProvisionWindowsPrivateHandle(file, path, info, directory)
	}
	if err != nil {
		_ = file.Close()
		return nil, ErrAccountProvisionJournalUnsafe
	}
	return file, nil
}

func accountProvisionWindowsCreationError(err error) error {
	if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return os.ErrExist
	}
	return ErrAccountProvisionJournalStorage
}
