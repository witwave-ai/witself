package trust

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

func validChannelIdentity(identity string) bool {
	// Persisted receipts fail closed until ACL-backed state exists. Still bound
	// and validate caller SID declarations for the portable adapter surface.
	parts := strings.Split(identity, "-")
	if len(parts) < 4 || len(parts) > 18 || parts[0] != "S" || parts[1] != "1" {
		return false
	}
	for i, part := range parts[2:] {
		bits := 32
		if i == 0 {
			bits = 48
		}
		value, err := strconv.ParseUint(part, 10, bits)
		if err != nil || strconv.FormatUint(value, 10) != part {
			return false
		}
	}
	return true
}

// GetSecurityInfo queries the actual opened file handle, never a username or
// PATH guess. Its returned security descriptor must be released with LocalFree.
var channelGetSecurityInfo = syscall.NewLazyDLL("advapi32.dll").NewProc("GetSecurityInfo")

// os.Open supports directory handles as well as files. Bind the handle to the
// no-reparse observations and use the existing volume/file-index helper.
func channelFileIdentity(name string, info os.FileInfo) (HostIdentity, error) {
	file, err := os.Open(name)
	if err != nil {
		return HostIdentity{}, ReasonStateIO
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	after, afterErr := os.Lstat(name)
	if err != nil || afterErr != nil || !inventoryInfoSafe(opened) || !inventoryInfoSafe(after) || !os.SameFile(info, opened) || !os.SameFile(after, opened) {
		return HostIdentity{}, ReasonChannelReceiptInvalid
	}
	return hostFileIdentity(file, opened)
}

func channelIdentityMatches(name string, info os.FileInfo, identity string) bool {
	file, err := os.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	after, afterErr := os.Lstat(name)
	if err != nil || afterErr != nil || !inventoryInfoSafe(after) || !os.SameFile(info, opened) || !os.SameFile(after, opened) {
		return false
	}
	var owner *syscall.SID
	var descriptor uintptr
	result, _, _ := channelGetSecurityInfo.Call(file.Fd(), 1, 1, uintptr(unsafe.Pointer(&owner)), 0, 0, 0, uintptr(unsafe.Pointer(&descriptor)))
	if descriptor != 0 {
		defer func() { _, _ = syscall.LocalFree(syscall.Handle(descriptor)) }()
	}
	if result != 0 || owner == nil {
		return false
	}
	sid, err := owner.String()
	return err == nil && sid == identity
}
