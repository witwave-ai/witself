//go:build !windows

package runtimeconfig

// WindowsPrivateFailureClass has no private Windows classification here.
func WindowsPrivateFailureClass(error) string { return "none" }

// WindowsPrivateTransient never retries a non-Windows filesystem failure.
func WindowsPrivateTransient(error) bool { return false }
