package store

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestMigrationTestDSNWithSearchPathPreservesProviderOptions(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		dsn     string
		options string
	}{
		{
			name: "URL",
			dsn: "postgres://user:password@db.example:5432/witself" +
				"?sslmode=require&application_name=witself-test&options=-cstatement_timeout%3D5000",
			options: "-cstatement_timeout=5000",
		},
		{
			name: "keyword",
			dsn: "host=db.example port=5432 dbname=witself user=user password=password " +
				"sslmode=require application_name=witself-test options='-c statement_timeout=5000'",
			options: "-c statement_timeout=5000",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := migrationTestDSNWithSearchPath(testCase.dsn, "witself_migration_123")
			if err != nil {
				t.Fatal(err)
			}
			config, err := pgx.ParseConfig(got)
			if err != nil {
				t.Fatal(err)
			}
			if got := config.RuntimeParams["search_path"]; got != "witself_migration_123" {
				t.Fatalf("search_path = %q", got)
			}
			if got := config.RuntimeParams["options"]; got != testCase.options {
				t.Fatalf("options = %q, want %q", got, testCase.options)
			}
			if got := config.RuntimeParams["application_name"]; got != "witself-test" {
				t.Fatalf("application_name = %q", got)
			}
		})
	}
}

func TestValidateMigrationPreflight(t *testing.T) {
	tests := []struct {
		name    string
		state   migrationPreflightState
		wantErr error
	}{
		{
			name:  "fresh database may install schema 36",
			state: migrationPreflightState{TargetVersion: 36},
		},
		{
			name: "Goose initialized but no application migration may install schema 36",
			state: migrationPreflightState{
				TargetVersion: 36, VersionTableExists: true,
			},
		},
		{
			name: "interrupted empty install before compatibility schema may resume",
			state: migrationPreflightState{
				CurrentVersion: 12, TargetVersion: 36,
				VersionTableExists: true, AccountsTableExists: true,
			},
		},
		{
			name: "interrupted empty install at schema 26 may resume",
			state: migrationPreflightState{
				CurrentVersion: 26, TargetVersion: 36,
				VersionTableExists: true, AccountsTableExists: true,
			},
		},
		{
			name: "populated schema 1 cannot skip compatibility release",
			state: migrationPreflightState{
				CurrentVersion: 1, TargetVersion: 36,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
			wantErr: ErrMigrationCompatibilityRequired,
		},
		{
			name: "populated schema 26 cannot skip compatibility release",
			state: migrationPreflightState{
				CurrentVersion: 26, TargetVersion: 36,
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
			name: "populated schema 27 may activate through schema 36",
			state: migrationPreflightState{
				CurrentVersion: 27, TargetVersion: 36,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
		},
		{
			name: "schema 32 is an idempotent no-op",
			state: migrationPreflightState{
				CurrentVersion: 32, TargetVersion: 32,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
		},
		{
			name: "populated schema 32 may activate schema 33",
			state: migrationPreflightState{
				CurrentVersion: 32, TargetVersion: 33,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
		},
		{
			name: "schema 33 is an idempotent no-op",
			state: migrationPreflightState{
				CurrentVersion: 33, TargetVersion: 33,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
		},
		{
			name: "populated schema 33 may activate through schema 36",
			state: migrationPreflightState{
				CurrentVersion: 33, TargetVersion: 36,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
		},
		{
			name: "populated schema 34 may activate through schema 36",
			state: migrationPreflightState{
				CurrentVersion: 34, TargetVersion: 36,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
		},
		{
			name: "schema 35 is an idempotent no-op",
			state: migrationPreflightState{
				CurrentVersion: 35, TargetVersion: 35,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
		},
		{
			name: "populated schema 35 may activate schema 36",
			state: migrationPreflightState{
				CurrentVersion: 35, TargetVersion: 36,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
		},
		{
			name: "schema 36 is an idempotent no-op",
			state: migrationPreflightState{
				CurrentVersion: 36, TargetVersion: 36,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
		},
		{
			name: "database ahead of binary is refused",
			state: migrationPreflightState{
				CurrentVersion: 37, TargetVersion: 36,
				VersionTableExists: true, AccountsTableExists: true, AccountsPopulated: true,
			},
			wantErr: ErrMigrationSchemaAhead,
		},
		{
			name: "versioned database without Goose table is corrupt",
			state: migrationPreflightState{
				CurrentVersion: 27, TargetVersion: 36, AccountsTableExists: true,
			},
			wantErr: ErrMigrationStateInvalid,
		},
		{
			name: "versioned database without accounts table is corrupt",
			state: migrationPreflightState{
				CurrentVersion: 27, TargetVersion: 36, VersionTableExists: true,
			},
			wantErr: ErrMigrationStateInvalid,
		},
		{
			name: "unversioned application schema is corrupt even when empty",
			state: migrationPreflightState{
				TargetVersion: 36, AccountsTableExists: true,
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
