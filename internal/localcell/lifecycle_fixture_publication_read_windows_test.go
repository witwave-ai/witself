//go:build windows

package localcell_test

import "syscall"

// Win32 System Error Codes (0-499): ERROR_ACCESS_DENIED (5),
// ERROR_SHARING_VIOLATION (32), ERROR_LOCK_VIOLATION (33).
// These may be temporary while the fixture creates or renames its record.
// https://learn.microsoft.com/en-us/windows/win32/debug/system-error-codes--0-499-
func fixturePublicationTransientErrno(errno syscall.Errno) bool {
	switch errno {
	case 5, 32, 33:
		return true
	default:
		return false
	}
}

const fixturePublicationPlatformErrnoState = fixturePublicationTransient
