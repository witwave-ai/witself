package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestSyncRefusalErrorMappings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mapError func(error) error
		in, want error
	}{
		{"import", mapAccountImportError, store.ErrSyncArchiveRequired, server.ErrBadArchive},
		{"backup", mapBackupValidationError, store.ErrSyncArchiveRequired, server.ErrBadArchive},
		{"backup lease", mapBackupValidationLeaseError, store.ErrSyncArchiveRequired, server.ErrBadArchive},
		{"finalization", mapAccountFinalizationError, store.ErrSyncDestructionUnsupported, server.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, err := range []error{tc.in, fmt.Errorf("private sync diagnostic: %w", tc.in)} {
				if got := tc.mapError(err); got != tc.want {
					t.Fatal("sync refusal did not map to the exact public error")
				}
			}
		})
	}
}

func TestAccountFinalizationErrorMappingPreservesExistingCases(t *testing.T) {
	other := errors.New("unclassified finalization failure")
	for _, tc := range []struct {
		name     string
		in, want error
	}{
		{"nil", nil, nil},
		{"missing account", store.ErrAccountNotFound, server.ErrNotFound},
		{"default account", store.ErrCannotCloseDefault, server.ErrCannotCloseDefault},
		{"evacuation mismatch", store.ErrAccountEvacuationMismatch, server.ErrConflict},
		{"not exportable", store.ErrAccountNotExportable, server.ErrConflict},
		{"other", other, other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapAccountFinalizationError(tc.in); got != tc.want {
				t.Fatal("existing finalization mapping changed")
			}
			if tc.in != nil {
				wrapped := fmt.Errorf("finalization: %w", tc.in)
				want := tc.want
				if tc.in == other {
					want = wrapped
				}
				if got := mapAccountFinalizationError(wrapped); got != want {
					t.Fatal("existing wrapped finalization mapping changed")
				}
			}
		})
	}
}
