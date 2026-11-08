//go:build windows

package runtimeconfig

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateFailureClass(t *testing.T) {
	for _, test := range []struct {
		name      string
		err       error
		class     string
		transient bool
	}{
		{"sharing", windowsPrivateFailure(windows.ERROR_SHARING_VIOLATION), "sharing_violation", true},
		{"access", windowsPrivateFailure(windows.ERROR_ACCESS_DENIED), "access_denied", true},
		{"object", windowsPrivatePolicyFailure(errWindowsPrivateObject), "validation", false},
		{"owner_admin", windowsPrivatePolicyFailure(errWindowsPrivateOwnerAdmin), "validation", false},
		{"owner", windowsPrivatePolicyFailure(errWindowsPrivateOwner), "validation", false},
		{"ace_count", windowsPrivatePolicyFailure(errWindowsPrivateACECount), "validation", false},
		{"ace_mask", windowsPrivatePolicyFailure(errWindowsPrivateACEMask), "validation", false},
		{"ace_flags", windowsPrivatePolicyFailure(errWindowsPrivateACEFlags), "validation", false},
		{"protected", windowsPrivatePolicyFailure(errWindowsPrivateProtected), "validation", false},
		{"system", windowsPrivatePolicyFailure(errWindowsPrivateSystem), "validation", false},
		{"unknown", windowsPrivatePolicyFailure(errors.New("unknown private reason")), "other", false},
		{"nil", nil, "none", false},
		{"foreign", errors.New("foreign refusal"), "none", false},
		{"foreign_sharing", windows.ERROR_SHARING_VIOLATION, "none", false},
		{"foreign_access", windows.ERROR_ACCESS_DENIED, "none", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := func(err error) {
				t.Helper()
				if got := WindowsPrivateFailureClass(err); got != test.class {
					t.Fatalf("class = %q, want %q", got, test.class)
				}
				if got := WindowsPrivateTransient(err); got != test.transient {
					t.Fatalf("transient = %t, want %t", got, test.transient)
				}
			}
			check(test.err)
			if test.err != nil {
				check(fmt.Errorf("wrapped: %w", test.err))
			}
		})
	}
}
