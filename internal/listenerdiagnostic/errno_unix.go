//go:build !windows

package listenerdiagnostic

import "syscall"

// ErrAddressInUse is the native socket error, also used by hermetic fixtures.
const ErrAddressInUse = syscall.EADDRINUSE
