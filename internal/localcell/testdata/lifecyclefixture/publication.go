package main

import (
	"errors"
	"runtime"
	"syscall"
	"time"
)

const fixturePublicationTimeout = 5 * time.Second

// A reader can briefly prevent a Windows rename. Keep that retry budget inside
// one tenth of the fixture's publication gate timeout.
func fixtureRenamePublication(oldpath, newpath string, rename func(string, string) error, transient func(error) bool, now func() time.Time, sleep func(time.Duration)) error {
	deadline := now().Add(fixturePublicationTimeout / 10)
	original := rename(oldpath, newpath)
	if original == nil || !transient(original) {
		return original
	}
	for {
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return original
		}
		sleep(min(10*time.Millisecond, remaining))
		if !now().Before(deadline) {
			return original
		}
		err := rename(oldpath, newpath)
		if err == nil || !transient(err) {
			return err
		}
	}
}

// The numeric codes below are unrelated errors on other operating systems.
func fixturePublicationTransient(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	// Win32 System Error Codes (0-499): ERROR_ACCESS_DENIED (5),
	// ERROR_SHARING_VIOLATION (32), and ERROR_LOCK_VIOLATION (33).
	// https://learn.microsoft.com/en-us/windows/win32/debug/system-error-codes--0-499-
	switch errno {
	case 5, 32, 33:
		return true
	default:
		return false
	}
}
