//go:build windows

package runtimeconfig

import "errors"

// WindowsPrivateFailureClass returns only a value-free class for private
// filesystem failures. Foreign errors have no private failure classification.
func WindowsPrivateFailureClass(err error) string {
	var private windowsPrivateError
	if !errors.As(err, &private) {
		return "none"
	}
	switch private.reason {
	case errWindowsPrivateSharing:
		return "sharing_violation"
	case errWindowsPrivateAccess:
		return "access_denied"
	case errWindowsPrivateObject, errWindowsPrivateOwnerAdmin, errWindowsPrivateOwner,
		errWindowsPrivateACECount, errWindowsPrivateACEMask, errWindowsPrivateACEFlags,
		errWindowsPrivateProtected, errWindowsPrivateSystem:
		return "validation"
	default:
		return "other"
	}
}

// WindowsPrivateTransient identifies only private sharing and access refusals.
func WindowsPrivateTransient(err error) bool {
	class := WindowsPrivateFailureClass(err)
	return class == "sharing_violation" || class == "access_denied"
}
