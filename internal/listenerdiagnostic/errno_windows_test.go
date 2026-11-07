package listenerdiagnostic

import (
	"fmt"
	"syscall"
	"testing"
)

func TestWindowsSocketErrorMapping(t *testing.T) {
	// Winsock uses WSAEADDRINUSE (10048), not the POSIX errno represented by
	// syscall.EADDRINUSE on Windows.
	if !IsAddressInUse(fmt.Errorf("bind: %w", syscall.Errno(10048))) {
		t.Fatal("wrapped WSAEADDRINUSE must be recognized")
	}
	if IsAddressInUse(syscall.EADDRINUSE) || IsAddressInUse(syscall.Errno(10013)) {
		t.Fatal("non-Winsock address-in-use and access-denied errors must remain other")
	}
}
