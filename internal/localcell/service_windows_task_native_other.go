//go:build !windows

package localcell

import "context"

// NewWindowsLogonTaskAdapter is an explicit, per-user Windows-only selection.
func NewWindowsLogonTaskAdapter() ServiceAdapter { return unavailableServiceAdapter{} }

// WindowsLogonTaskCapability is a read-only capability probe.
func WindowsLogonTaskCapability(context.Context) error { return ErrServiceManagerUnavailable }

func validateTaskHostOwner(string) error { return nil }

func validateServiceHostIdentity(string) error { return nil }
