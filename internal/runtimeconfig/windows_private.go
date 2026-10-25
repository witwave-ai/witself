//go:build windows

package runtimeconfig

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// FILE_ALL_ACCESS includes SYNCHRONIZE; unlike GENERIC_ALL this is already
// mapped to file-specific rights. The two are equivalent for a file DACL.
const windowsPrivateFullAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

var (
	errWindowsPrivateObject     = errors.New("private Windows filesystem object required")
	errWindowsPrivateOwnerAdmin = errors.New("private Windows owner-is-admin-group")
	errWindowsPrivateOwner      = errors.New("private Windows owner-mismatch")
	errWindowsPrivateACECount   = errors.New("private Windows ace-count")
	errWindowsPrivateACEMask    = errors.New("private Windows ace-mask")
	errWindowsPrivateACEFlags   = errors.New("private Windows ace-flags")
	errWindowsPrivateProtected  = errors.New("private Windows protected-missing")
	errWindowsPrivateSystem     = errors.New("private Windows system-missing")
	errWindowsPrivateSharing    = errors.New("private Windows sharing-violation")
	errWindowsPrivateAccess     = errors.New("private Windows access-denied")
)

// Preserve errors.Is for callers' missing/existing-object handling without
// exposing paths, SIDs, or descriptor contents in an error string.
type windowsPrivateError struct {
	cause  error
	reason error
}

func (e windowsPrivateError) Error() string { return e.reason.Error() }
func (e windowsPrivateError) Unwrap() error { return e.cause }

func windowsPrivateFailure(err error) error {
	var private windowsPrivateError
	if errors.As(err, &private) {
		return private
	}
	reason := errWindowsPrivateObject
	switch {
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		reason = errWindowsPrivateSharing
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		reason = errWindowsPrivateAccess
	}
	return windowsPrivateError{cause: err, reason: reason}
}

func windowsPrivatePolicyFailure(reason error) error {
	return windowsPrivateError{cause: reason, reason: reason}
}

func windowsPrivatePrincipals() (*windows.SID, *windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return nil, nil, windowsPrivateFailure(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, nil, windowsPrivateFailure(err)
	}
	return user.User.Sid, system, nil
}

func windowsPrivateDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	user, system, err := windowsPrivatePrincipals()
	if err != nil {
		return nil, err
	}
	// Choose TokenUser rather than TokenOwner: an elevated token's default
	// owner can be Administrators. SetOwner(false) explicitly overrides that
	// default before CreateDirectory/CreateFile receive the descriptor.
	sddl := "D:P(A;;FA;;;" + user.String() + ")"
	if !user.Equals(system) {
		sddl += "(A;;FA;;;SY)"
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, windowsPrivateFailure(err)
	}
	absolute, err := descriptor.ToAbsolute()
	if err != nil {
		return nil, windowsPrivateFailure(err)
	}
	if err := absolute.SetOwner(user, false); err != nil {
		return nil, windowsPrivateFailure(err)
	}
	// Return one self-relative allocation owning the SID and ACL bytes; the
	// absolute descriptor references (rather than copies) its owner SID.
	descriptor, err = absolute.ToSelfRelative()
	runtime.KeepAlive(user)
	if err != nil {
		return nil, windowsPrivateFailure(err)
	}
	return descriptor, nil
}

func windowsPrivateValidateDescriptor(descriptor *windows.SECURITY_DESCRIPTOR) error {
	if descriptor == nil || !descriptor.IsValid() {
		return windowsPrivateFailure(nil)
	}
	user, system, err := windowsPrivatePrincipals()
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		admin, adminErr := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
		if adminErr == nil && owner != nil && owner.Equals(admin) {
			return windowsPrivatePolicyFailure(errWindowsPrivateOwnerAdmin)
		}
		return windowsPrivatePolicyFailure(errWindowsPrivateOwner)
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return windowsPrivatePolicyFailure(errWindowsPrivateProtected)
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil {
		return windowsPrivateFailure(err)
	}
	wantCount := uint16(2)
	if user.Equals(system) {
		wantCount = 1
	}
	if acl.AceCount != wantCount {
		// A sole user ACE specifically identifies a missing SYSTEM grant.
		if acl.AceCount == 1 && !user.Equals(system) {
			var only *windows.ACCESS_ALLOWED_ACE
			if windows.GetAce(acl, 0, &only) == nil && only != nil && only.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE {
				sid := (*windows.SID)(unsafe.Pointer(&only.SidStart))
				if sid.IsValid() && sid.Equals(user) {
					return windowsPrivatePolicyFailure(errWindowsPrivateSystem)
				}
			}
		}
		return windowsPrivatePolicyFailure(errWindowsPrivateACECount)
	}
	var foundUser, foundSystem bool
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil || ace == nil {
			return windowsPrivateFailure(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return windowsPrivateFailure(nil)
		}
		// Each child receives its own descriptor, so neither inheritance nor
		// inherit-only flags are needed. ACE ordering and auto-inheritance
		// descriptor control bits are not policy constraints.
		if ace.Header.AceFlags != 0 {
			return windowsPrivatePolicyFailure(errWindowsPrivateACEFlags)
		}
		mask := ace.Mask
		if mask&windows.GENERIC_ALL != 0 {
			mask = mask&^windows.GENERIC_ALL | windowsPrivateFullAccess
		}
		if mask != windowsPrivateFullAccess {
			return windowsPrivatePolicyFailure(errWindowsPrivateACEMask)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || uintptr(ace.Header.AceSize) != unsafe.Offsetof(ace.SidStart)+uintptr(sid.Len()) {
			return windowsPrivateFailure(nil)
		}
		switch {
		case sid.Equals(user):
			if foundUser {
				return windowsPrivateFailure(nil)
			}
			foundUser = true
		case sid.Equals(system):
			if foundSystem {
				return windowsPrivateFailure(nil)
			}
			foundSystem = true
		default:
			return windowsPrivateFailure(nil)
		}
	}
	if !foundUser || (!foundSystem && !user.Equals(system)) {
		return windowsPrivatePolicyFailure(errWindowsPrivateSystem)
	}
	return nil
}

func windowsPrivateCleanPath(path string) (string, error) {
	// Check the original components too: Win32 can interpret a device name
	// before Clean removes a trailing dot or dot-dot component.
	path = strings.ReplaceAll(path, "/", `\`)
	if len(path) < 3 || path[1] != ':' || path[2] != '\\' || ((path[0] < 'A' || path[0] > 'Z') && (path[0] < 'a' || path[0] > 'z')) {
		return "", windowsPrivateFailure(nil)
	}
	for _, component := range strings.Split(path[3:], `\`) {
		if component == "" || component == "." || component == ".." {
			continue
		}
		if strings.TrimRight(component, ". ") != component || strings.ContainsAny(component, `<>:"|?*`) {
			return "", windowsPrivateFailure(nil)
		}
		for _, character := range component {
			if character < 32 {
				return "", windowsPrivateFailure(nil)
			}
		}
		base, _, _ := strings.Cut(strings.ToUpper(component), ".")
		base = strings.TrimRight(base, " ")
		switch base {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$", "CLOCK$":
			return "", windowsPrivateFailure(nil)
		}
		if (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && strings.Contains("123456789¹²³", strings.TrimPrefix(strings.TrimPrefix(base, "COM"), "LPT")) {
			suffix := []rune(base[3:])
			if len(suffix) == 1 {
				return "", windowsPrivateFailure(nil)
			}
		}
	}
	clean := filepath.Clean(path)
	if len(utf16.Encode([]rune(clean))) >= 260 {
		return "", windowsPrivateFailure(nil)
	}
	return clean, nil
}

type windowsPrivateGuardEntry struct {
	handle   windows.Handle
	path     string
	identity windows.ByHandleFileInformation
}

type windowsPrivatePathGuard []windowsPrivateGuardEntry

func (guard windowsPrivatePathGuard) close() {
	for index := len(guard) - 1; index >= 0; index-- {
		_ = windows.CloseHandle(guard[index].handle)
	}
}

// Open every ancestor without following a reparse point. FILE_TRAVERSE makes
// the handles participate in sharing checks; attributes-only access does not.
// Denying delete sharing fences renames while a named operation follows.
// Write sharing is allowed: denying it cannot fence attribute-only reparse
// conversion and can conflict with unrelated ancestor writers. Every creation
// rechecks the retained identities and reparse attributes before exposing data.
// This is detection after creation plus rollback, not prevention; handle-relative
// creation remains the eventual fix.
// Ancestors need not have private ACLs: a user's profile/temp parent usually
// does not. Only the object owned by the caller has the private policy.
func windowsPrivateGuard(path string) (windowsPrivatePathGuard, string, error) {
	clean, err := windowsPrivateCleanPath(path)
	if err != nil {
		return nil, "", err
	}
	root := clean[:3]
	rootPointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, "", windowsPrivateFailure(err)
	}
	if drive := windows.GetDriveType(rootPointer); drive != windows.DRIVE_FIXED && drive != windows.DRIVE_REMOVABLE {
		return nil, "", windowsPrivateFailure(nil)
	}
	paths := []string{root}
	current := root
	for _, component := range strings.Split(clean[3:], `\`) {
		if component != "" {
			current = filepath.Join(current, component)
			paths = append(paths, current)
		}
	}
	var guard windowsPrivatePathGuard
	for index, current := range paths {
		pointer, err := windows.UTF16PtrFromString(current)
		if err != nil {
			guard.close()
			return nil, "", windowsPrivateFailure(err)
		}
		handle, err := windows.CreateFile(pointer, windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			if index == len(paths)-1 && (errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)) {
				return guard, clean, nil
			}
			guard.close()
			return nil, "", windowsPrivateFailure(err)
		}
		guard = append(guard, windowsPrivateGuardEntry{handle: handle, path: current})
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (index < len(paths)-1 && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0) {
			guard.close()
			return nil, "", windowsPrivateFailure(err)
		}
		guard[len(guard)-1].identity = info
		if index == 0 {
			var filesystem [32]uint16
			if err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, nil, &filesystem[0], uint32(len(filesystem))); err != nil || windows.UTF16ToString(filesystem[:]) != "NTFS" {
				guard.close()
				return nil, "", windowsPrivateFailure(err)
			}
		}
	}
	return guard, clean, nil
}

// WindowsPrivateValidatePath accepts only short absolute local NTFS paths with
// no reparse component. The final object may be absent; every parent must exist.
func WindowsPrivateValidatePath(path string) error {
	guard, _, err := windowsPrivateGuard(path)
	guard.close()
	return err
}

// WindowsPrivateOpenRoot retains the checked path components against rename
// until os.OpenRoot pins the directory, then rechecks the opened object and
// retained ancestor identities for in-place reparse conversion.
func WindowsPrivateOpenRoot(path string) (*os.Root, error) {
	guard, clean, err := windowsPrivateGuard(path)
	if err != nil {
		return nil, err
	}
	defer guard.close()
	if err := WindowsPrivateValidate(clean, true); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(clean)
	if err != nil {
		return nil, windowsPrivateFailure(err)
	}
	directory, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, windowsPrivateFailure(err)
	}
	defer func() { _ = directory.Close() }()
	if err := guard.verify(clean, windows.Handle(directory.Fd()), true); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

func windowsPrivateValidateHandle(handle windows.Handle, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return windowsPrivateFailure(err)
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	return windowsPrivateValidateDescriptor(descriptor)
}

// WindowsPrivateValidateFile checks the owner, protected exact DACL, type and
// reparse attribute of the already opened object, never a pathname substitute.
func WindowsPrivateValidateFile(file *os.File, directory bool) error {
	if file == nil {
		return windowsPrivateFailure(nil)
	}
	return windowsPrivateValidateHandle(windows.Handle(file.Fd()), directory)
}

// WindowsPrivateValidate checks path confinement and the object's security
// descriptor by opened handle. It does not repair pre-existing objects.
func WindowsPrivateValidate(path string, directory bool) error {
	guard, clean, err := windowsPrivateGuard(path)
	if err != nil {
		return err
	}
	defer guard.close()
	pointer, err := windows.UTF16PtrFromString(clean)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	handle, err := windows.CreateFile(pointer, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	return windowsPrivateValidateHandle(handle, directory)
}

// WindowsPrivateMkdir creates an explicitly secured directory, then verifies
// its opened security descriptor. An existing object is refused unchanged.
func WindowsPrivateMkdir(path string) error {
	return windowsPrivateMkdir(path, nil)
}

func windowsPrivateMkdir(path string, afterCreate func(string) error) error {
	return windowsPrivateMkdirWithHook(path, afterCreate, nil)
}

// beforeCreate is a native race-test seam after guard acquisition.
func windowsPrivateMkdirWithHook(path string, afterCreate func(string) error, beforeCreate func() error) error {
	guard, clean, err := windowsPrivateGuard(path)
	if err != nil {
		return err
	}
	defer guard.close()
	descriptor, err := windowsPrivateDescriptor()
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	pointer, err := windows.UTF16PtrFromString(clean)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	if beforeCreate != nil {
		if err := beforeCreate(); err != nil {
			return windowsPrivateFailure(err)
		}
	}
	err = windows.CreateDirectory(pointer, &attributes)
	runtime.KeepAlive(descriptor)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	// CreateDirectory has no returned handle. Pin the new object immediately;
	// later rollback uses this handle, never the potentially redirected path.
	handle, err := windowsPrivateOpenMetadata(clean, windows.DELETE|windows.READ_CONTROL)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if afterCreate != nil {
		if err := afterCreate(clean); err != nil {
			return windowsPrivateFailure(err)
		}
	}
	if err := guard.verify(clean, handle, true); err != nil {
		return windowsPrivateRollback(handle, err)
	}
	return windowsPrivateValidateHandle(handle, true)
}

// WindowsPrivateOpenFile supplies the exact protected DACL at creation and
// validates the opened object before truncation or the caller can write bytes.
// Existing files are validated unchanged. Handles are non-inheritable.
func WindowsPrivateOpenFile(path string, flag int) (*os.File, error) {
	return windowsPrivateOpenFile(path, flag, nil)
}

func windowsPrivateOpenFile(path string, flag int, afterCreate func(windows.Handle) error) (*os.File, error) {
	return windowsPrivateOpenFileWithHook(path, flag, afterCreate, nil)
}

func windowsPrivateOpenFileWithHook(path string, flag int, afterCreate func(windows.Handle) error, beforeCreate func() error) (*os.File, error) {
	if flag & ^(os.O_RDONLY|os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_EXCL|os.O_TRUNC|os.O_SYNC) != 0 {
		return nil, windowsPrivateFailure(nil)
	}
	access := uint32(windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES)
	switch flag & (os.O_WRONLY | os.O_RDWR) {
	case os.O_RDONLY:
		access |= windows.GENERIC_READ
		if flag&(os.O_APPEND|os.O_TRUNC) != 0 {
			return nil, windowsPrivateFailure(nil)
		}
	case os.O_WRONLY:
		access |= windows.GENERIC_WRITE
	case os.O_RDWR:
		access |= windows.GENERIC_READ | windows.GENERIC_WRITE
	default:
		return nil, windowsPrivateFailure(nil)
	}
	if flag&os.O_APPEND != 0 {
		// Match Go's native append rights, including the rights needed by
		// Sync, while retaining GENERIC_WRITE when truncation requires it.
		if flag&os.O_TRUNC == 0 {
			access &^= windows.GENERIC_WRITE
		}
		access |= windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.FILE_WRITE_EA | windows.STANDARD_RIGHTS_WRITE | windows.SYNCHRONIZE
	}
	guard, clean, err := windowsPrivateGuard(path)
	if err != nil {
		return nil, err
	}
	defer guard.close()
	descriptor, err := windowsPrivateDescriptor()
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	pointer, err := windows.UTF16PtrFromString(clean)
	if err != nil {
		return nil, windowsPrivateFailure(err)
	}
	flags := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if flag&os.O_SYNC != 0 {
		flags |= windows.FILE_FLAG_WRITE_THROUGH
	}
	disposition := uint32(windows.OPEN_EXISTING)
	if flag&os.O_CREATE != 0 {
		disposition = windows.CREATE_NEW
	}
	createAccess := access
	if disposition == windows.CREATE_NEW && (len(guard) == 0 || guard[len(guard)-1].path != clean) {
		createAccess |= windows.DELETE // Rollback is bound to the created object.
	}
	if beforeCreate != nil {
		if err := beforeCreate(); err != nil {
			return nil, windowsPrivateFailure(err)
		}
	}
	handle, err := windows.CreateFile(pointer, createAccess, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		&attributes, disposition, flags, 0)
	created := err == nil && disposition == windows.CREATE_NEW
	if flag&os.O_CREATE != 0 && flag&os.O_EXCL == 0 && (errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS)) {
		handle, err = windows.CreateFile(pointer, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.OPEN_EXISTING, flags, 0)
	}
	runtime.KeepAlive(descriptor)
	if err != nil {
		return nil, windowsPrivateFailure(err)
	}
	file := os.NewFile(uintptr(handle), clean)
	if created && afterCreate != nil {
		if err := afterCreate(handle); err != nil {
			_ = file.Close()
			return nil, windowsPrivateFailure(err)
		}
	}
	if err := guard.verify(clean, handle, false); err != nil {
		if created {
			err = windowsPrivateRollback(handle, err)
		}
		_ = file.Close()
		return nil, err
	}
	if err := WindowsPrivateValidateFile(file, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	if flag&os.O_TRUNC != 0 {
		if err := file.Truncate(0); err != nil {
			_ = file.Close()
			return nil, windowsPrivateFailure(err)
		}
	}
	if created {
		// A separate open drops DELETE sharing participation. DuplicateHandle
		// would retain the same file object's sharing state. Recheck identity
		// before closing the rollback handle or exposing the new writer.
		writer, err := windows.CreateFile(pointer, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.OPEN_EXISTING, flags, 0)
		if err == nil {
			var original, reopened windows.ByHandleFileInformation
			err = windows.GetFileInformationByHandle(handle, &original)
			if err == nil {
				err = windows.GetFileInformationByHandle(writer, &reopened)
			}
			if err == nil && !windowsPrivateSameIdentity(original, reopened) {
				err = errWindowsPrivatePathChanged
			}
			if err == nil {
				err = guard.verify(clean, writer, false)
			}
		}
		if err != nil {
			if writer != windows.InvalidHandle {
				_ = windows.CloseHandle(writer)
			}
			err = windowsPrivateRollback(handle, err)
			_ = file.Close()
			return nil, windowsPrivateFailure(err)
		}
		_ = file.Close()
		file = os.NewFile(uintptr(writer), clean)
	}
	return file, nil
}

// WindowsPrivateCreateTemp creates a private, empty .pending-* replacement
// file with exclusive creation. It never exposes bytes through a default DACL.
func WindowsPrivateCreateTemp(dir string) (*os.File, error) {
	for range 100 {
		var suffix [12]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, windowsPrivateFailure(err)
		}
		file, err := WindowsPrivateOpenFile(filepath.Join(dir, ".pending-"+hex.EncodeToString(suffix[:])), os.O_RDWR|os.O_CREATE|os.O_EXCL)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return file, err
	}
	return nil, windowsPrivateFailure(os.ErrExist)
}

// WindowsPrivateRename publishes a private file while retaining its parent
// directories against rename. Reparse conversion is detected after publication
// and the published object is rolled back by handle. Existing destinations are
// validated before replacement; sharing violations leave both files intact.
func WindowsPrivateRename(oldPath, newPath string) error {
	oldClean, err := windowsPrivateCleanPath(oldPath)
	if err != nil {
		return err
	}
	newClean, err := windowsPrivateCleanPath(newPath)
	if err != nil {
		return err
	}
	oldGuard, _, err := windowsPrivateGuard(filepath.Dir(oldClean))
	if err != nil {
		return err
	}
	defer oldGuard.close()
	newGuard, _, err := windowsPrivateGuard(filepath.Dir(newClean))
	if err != nil {
		return err
	}
	defer newGuard.close()
	if err := WindowsPrivateValidate(oldClean, false); err != nil {
		return err
	}
	if err := WindowsPrivateValidate(newClean, false); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// POSIX replacement permits existing target readers. First acquire DELETE
	// on that target so readers that deny delete sharing still refuse publication;
	// retain the handle so a new incompatible reader cannot enter afterward.
	target, err := windowsPrivateOpenMetadata(newClean, windows.DELETE|windows.READ_CONTROL)
	if err == nil {
		defer func() { _ = windows.CloseHandle(target) }()
		if err := windowsPrivateValidateHandle(target, false); err != nil {
			return err
		}
		if err := newGuard.verify(newClean, target, false); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return windowsPrivateFailure(err)
	}
	handle, err := windowsPrivateOpenMetadata(oldClean, windows.DELETE|windows.READ_CONTROL)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := oldGuard.verify(oldClean, handle, false); err != nil {
		return err
	}
	if err := windowsPrivateReplace(handle, newClean); err != nil {
		return windowsPrivateFailure(err)
	}
	if err := oldGuard.verify(newClean, handle, false); err != nil {
		return windowsPrivateRollback(handle, err)
	}
	if err := newGuard.verify(newClean, handle, false); err != nil {
		return windowsPrivateRollback(handle, err)
	}
	return windowsPrivateValidateHandle(handle, false)
}

// WindowsPrivateRemove deletes a private regular file while pinning its parent
// path, so best-effort temporary-file cleanup cannot follow a replaced reparse
// directory into an unrelated location.
func WindowsPrivateRemove(path string) error {
	clean, err := windowsPrivateCleanPath(path)
	if err != nil {
		return err
	}
	guard, _, err := windowsPrivateGuard(filepath.Dir(clean))
	if err != nil {
		return err
	}
	defer guard.close()
	if err := WindowsPrivateValidate(clean, false); err != nil {
		return err
	}
	pointer, err := windows.UTF16PtrFromString(clean)
	if err != nil {
		return windowsPrivateFailure(err)
	}
	if err := windows.DeleteFile(pointer); err != nil {
		return windowsPrivateFailure(err)
	}
	return nil
}
