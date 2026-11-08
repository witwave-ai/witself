//go:build !linux && !darwin && !windows

package localcell

// NewServiceAdapter reports the platform capability without registering a
// service.
func NewServiceAdapter() ServiceAdapter { return unavailableServiceAdapter{} }
