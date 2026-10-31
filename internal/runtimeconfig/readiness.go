package runtimeconfig

import (
	"context"
	"errors"
)

// BootstrapStore is the database state required for local readiness.
type BootstrapStore interface {
	Ping(context.Context) error
	AppliedSchemaVersion(context.Context) (int, error)
	LocalBootstrapAccount(context.Context) (string, error)
}

// SyncSchemaStore reads the independent, explicitly installed sync namespace.
type SyncSchemaStore interface {
	AppliedSyncSchemaVersion(context.Context) (int, error)
}

// CheckSyncSchemaReady deliberately does not change core service readiness.
// Callers install this separate check only after explicit local sync selection.
func CheckSyncSchemaReady(ctx context.Context, st SyncSchemaStore, compiledVersion int) error {
	if st == nil {
		return errors.New("local sync schema is not ready")
	}
	applied, err := st.AppliedSyncSchemaVersion(ctx)
	if err != nil || compiledVersion <= 0 || applied != compiledVersion {
		return errors.New("local sync schema is not ready")
	}
	return nil
}

// ValidateAccount verifies the active bootstrap account and optional binding.
func ValidateAccount(ctx context.Context, st BootstrapStore, expectedAccountID string) error {
	id, err := st.LocalBootstrapAccount(ctx)
	if err != nil || id == "" {
		return errors.New("local bootstrap account must exist and be active")
	}
	if expectedAccountID != "" && id != expectedAccountID {
		return errors.New("WITSELF_LOCAL_ACCOUNT_ID does not match the local bootstrap account")
	}
	return nil
}

// CheckReady reads every dependency afresh; it never treats the compiled schema
// as evidence of the database's actual applied version.
func CheckReady(ctx context.Context, st BootstrapStore, compiledVersion int, expectedAccountID string) error {
	if st == nil {
		return errors.New("local database is required")
	}
	if err := st.Ping(ctx); err != nil {
		return errors.New("local database is unavailable")
	}
	applied, err := st.AppliedSchemaVersion(ctx)
	if err != nil || applied != compiledVersion {
		return errors.New("local database schema is not ready")
	}
	return ValidateAccount(ctx, st, expectedAccountID)
}
