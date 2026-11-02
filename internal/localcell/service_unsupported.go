//go:build !linux && !darwin

package localcell

// NewServiceAdapter reports the platform capability without registering a
// service. The Windows native adapter belongs to LC-S7C2.
func NewServiceAdapter() ServiceAdapter { return unavailableServiceAdapter{} }
