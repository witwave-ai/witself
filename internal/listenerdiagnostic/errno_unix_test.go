//go:build !windows

package listenerdiagnostic

import (
	"fmt"
	"syscall"
	"testing"
)

func TestUnixSocketErrorMapping(t *testing.T) {
	if !IsAddressInUse(fmt.Errorf("bind: %w", syscall.EADDRINUSE)) {
		t.Fatal("wrapped EADDRINUSE must be recognized")
	}
	if IsAddressInUse(syscall.EACCES) {
		t.Fatal("access-denied errors must remain other")
	}
}
