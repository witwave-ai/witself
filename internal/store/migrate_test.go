package store

import (
	"errors"
	"testing"
)

func TestValidateMigrationPreflight(t *testing.T) {
	tests := []struct {
		name    string
		state   migrationPreflightState
		wantErr error
	}{
		{
			name:  "fresh database may install schema 28",
			state: migrationPreflightState{TargetVersion: 28},
		},
		{
			name: "Goose initialized but no application migration may install schema 28",
			state: migrationPreflightState{
				TargetVersion: 28, VersionTableExists: true,
			},
		},
		{
			name: "interrupted empty install before compatibility schema may resume",
			state: migrationPreflightState{
				CurrentVersion: 12, TargetVersion: 28,
				VersionTableExists: true, AccountsTableExists: true,
			},
		},
		{
			name: "interrupted empty install at schema 26 may resume",
			state: migrationPreflightState{
				CurrentVersion: 26, TargetVersion: 28,
				VersionTableExists: true, AccountsTableExists: true,
			},
		},
		{
			name: "populated schema 1 cannot skip compatibility release",
			state: migrationPreflightState{
				CurrentVersion: 1, TargetVersion: 28,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
			wantErr: ErrMigrationCompatibilityRequired,
		},
		{
			name: "populated schema 26 cannot skip compatibility release",
			state: migrationPreflightState{
				CurrentVersion: 26, TargetVersion: 28,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
			wantErr: ErrMigrationCompatibilityRequired,
		},
		{
			name: "phase A binary may move populated schema 26 to 27",
			state: migrationPreflightState{
				CurrentVersion: 26, TargetVersion: 27,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
		},
		{
			name: "populated schema 27 may activate schema 28",
			state: migrationPreflightState{
				CurrentVersion: 27, TargetVersion: 28,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
		},
		{
			name: "schema 28 is an idempotent no-op",
			state: migrationPreflightState{
				CurrentVersion: 28, TargetVersion: 28,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
		},
		{
			name: "database ahead of binary is refused",
			state: migrationPreflightState{
				CurrentVersion: 29, TargetVersion: 28,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
			wantErr: ErrMigrationSchemaAhead,
		},
		{
			name: "versioned database without Goose table is corrupt",
			state: migrationPreflightState{
				CurrentVersion: 27, TargetVersion: 28, AccountsTableExists: true,
			},
			wantErr: ErrMigrationStateInvalid,
		},
		{
			name: "versioned database without accounts table is corrupt",
			state: migrationPreflightState{
				CurrentVersion: 27, TargetVersion: 28, VersionTableExists: true,
			},
			wantErr: ErrMigrationStateInvalid,
		},
		{
			name: "unversioned application schema is corrupt even when empty",
			state: migrationPreflightState{
				TargetVersion: 28, AccountsTableExists: true,
			},
			wantErr: ErrMigrationStateInvalid,
		},
		{
			name:    "invalid compiled target is refused",
			state:   migrationPreflightState{},
			wantErr: ErrMigrationStateInvalid,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMigrationPreflight(tc.state)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("validateMigrationPreflight(%#v): %v", tc.state, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("validateMigrationPreflight(%#v) error = %v, want errors.Is(_, %v)", tc.state, err, tc.wantErr)
			}
		})
	}
}
