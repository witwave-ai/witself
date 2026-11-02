package runtimeconfig

import (
	"context"
	"errors"
)

// SyncStatus describes capture only. It is independent of service readiness and
// installed schema readiness; an acknowledged pin does not make sync ready.
type SyncStatus string

// Capture status deliberately has no ready state before snapshot materialization.
const (
	SyncDisabled      SyncStatus = "disabled"
	SyncBootstrapping SyncStatus = "bootstrapping"
	SyncSuspended     SyncStatus = "suspended"
)

// ErrSyncStatusUnavailable deliberately contains no database or account values.
var ErrSyncStatusUnavailable = errors.New("local sync status is unavailable")

// SyncCaptureSelected requires both explicit selectors. Schema installation
// alone cannot select capture for an ordinary local or hosted caller.
func (c Config) SyncCaptureSelected() bool { return c.Local && c.SyncSchema }

// SyncStatusStore reads durable account capture state, not a compiled version.
type SyncStatusStore interface {
	SyncCaptureStatus(context.Context, string) (SyncStatus, error)
}

// ReadSyncStatus never consults the store when sync is unselected. Unavailable or
// unsupported durable state suspends the reported status without changing core
// readiness, installing a schema, activating capture, or attempting recovery.
func ReadSyncStatus(ctx context.Context, cfg Config, st SyncStatusStore, account string) (SyncStatus, error) {
	if !cfg.SyncCaptureSelected() {
		return SyncDisabled, nil
	}
	if st == nil || account == "" {
		return SyncSuspended, ErrSyncStatusUnavailable
	}
	status, err := st.SyncCaptureStatus(ctx, account)
	if err != nil {
		return SyncSuspended, ErrSyncStatusUnavailable
	}
	switch status {
	case SyncDisabled, SyncBootstrapping, SyncSuspended:
		return status, nil
	default:
		return SyncSuspended, ErrSyncStatusUnavailable
	}
}
