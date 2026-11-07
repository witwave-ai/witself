//go:build !windows

package localcell_test

import "syscall"

// The same numeric errnos have unrelated meanings on other operating systems.
func fixturePublicationTransientErrno(syscall.Errno) bool { return false }

const fixturePublicationPlatformErrnoState = fixturePublicationFatal
