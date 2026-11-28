//go:build linux

package localcell

import (
	"testing"

	"golang.org/x/sys/unix"
)

// The rebound rows hold the package's kept write lease (300 ms ceiling, raised by the row).
func init() {
	barrierLease = func(t *testing.T) (func(string) bool, func() bool, func()) {
		l := leaseHolder(t, "kept", unix.F_WRLCK)
		leaseWait.ceiling = barrierLeaseCeiling
		return func(name string) bool { l.take(name); return l.do(func(uintptr) {}) }, l.broken, l.release
	}
}
