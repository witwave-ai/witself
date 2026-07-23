//go:build windows

package transcriptcapture

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsFileDeleteChild windows.ACCESS_MASK = 0x00000040

var windowsUntrustedWriteMask = windows.ACCESS_MASK(windows.GENERIC_WRITE) |
	windows.ACCESS_MASK(windows.GENERIC_ALL) |
	windows.ACCESS_MASK(windows.DELETE) |
	windows.ACCESS_MASK(windows.WRITE_DAC) |
	windows.ACCESS_MASK(windows.WRITE_OWNER) |
	windows.ACCESS_MASK(windows.FILE_WRITE_DATA) |
	windows.ACCESS_MASK(windows.FILE_APPEND_DATA) |
	windows.ACCESS_MASK(windows.FILE_WRITE_EA) |
	windows.ACCESS_MASK(windows.FILE_WRITE_ATTRIBUTES) |
	windowsFileDeleteChild

// trustedPathIdentity maps the Unix owner-and-mode contract onto Windows file
// identity and ACLs. Windows FileMode permission bits are synthesized, so the
// path must instead have the current token user as owner and no effective
// write-capable allow ACE for an untrusted principal. SYSTEM and local
// Administrators are treated like Unix root. Any missing, malformed, or
// unfamiliar security information fails closed.
func trustedPathIdentity(path string, info os.FileInfo) bool {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return false
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return false
	}
	defer file.Close()
	var handleInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &handleInfo); err != nil ||
		handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false
	}
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return false
	}
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil || descriptor == nil || !descriptor.IsValid() {
		return false
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.IsValid() {
		return false
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() || !owner.Equals(user.User.Sid) {
		return false
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return false
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil || ace == nil {
			return false
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if ace.Mask&windowsUntrustedWriteMask == 0 {
				continue
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if sid == nil || !sid.IsValid() || !trustedWindowsWriter(sid, user.User.Sid) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func trustedWindowsWriter(sid, currentUser *windows.SID) bool {
	return sid.Equals(currentUser) ||
		sid.IsWellKnown(windows.WinLocalSystemSid) ||
		sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) ||
		sid.IsWellKnown(windows.WinCreatorOwnerSid) ||
		sid.IsWellKnown(windows.WinCreatorOwnerRightsSid)
}

// processRunning asks the kernel for a synchronizable process handle and then
// performs a zero-time wait. Access denial means the process exists but is
// protected. All other unexpected failures are unknown so stale-lock fallback
// remains conservative.
func processRunning(pid int) (running, known bool) {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return false, false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		switch {
		case errors.Is(err, windows.ERROR_INVALID_PARAMETER):
			return false, true
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			return true, true
		default:
			return false, false
		}
	}
	defer windows.CloseHandle(handle)
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
