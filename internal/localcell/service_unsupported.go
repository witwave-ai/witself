//go:build !linux

package localcell

// NewServiceAdapter reports the platform capability without registering a
// service. macOS and Windows native adapters belong to LC-S7C.
func NewServiceAdapter() ServiceAdapter { return unavailableServiceAdapter{} }
