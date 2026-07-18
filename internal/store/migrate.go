package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	factDeletionCompatibilitySchema = int64(27)
	factDeletionActivationSchema    = int64(28)
)

var (
	// ErrMigrationCompatibilityRequired prevents a populated pre-schema-27
	// deployment from skipping the writer-compatibility release. Goose itself
	// considers 26 -> 27 -> 28 in one Up call valid, but that would remove a
	// conflict target while old writers may still be serving.
	ErrMigrationCompatibilityRequired = errors.New("schema-27 compatibility release required")

	// ErrMigrationSchemaAhead means this binary is older than the database it
	// was asked to serve. Running it could make code-level assumptions that no
	// longer hold even though Goose has no pending migration to apply.
	ErrMigrationSchemaAhead = errors.New("database schema is newer than this binary")

	// ErrMigrationStateInvalid identifies a version/application-schema pairing
	// that a normal Witself migration or fresh-install retry cannot produce.
	ErrMigrationStateInvalid = errors.New("invalid database migration state")
)

type migrationPreflightState struct {
	CurrentVersion      int64
	TargetVersion       int64
	VersionTableExists  bool
	AccountsTableExists bool
	AccountsPopulated   bool
}

// Migrate applies all pending Goose migrations, embedded in the binary. It runs
// on serve startup, so a configured database must be reachable and migratable
// for the server to start (in Kubernetes it simply restarts until the DB is up).
func (s *Store) Migrate() error {
	db, err := sql.Open("pgx", s.dsn)
	if err != nil {
		return fmt.Errorf("open for migrate: %w", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	state, err := inspectMigrationPreflightState(db, int64(SchemaVersion()))
	if err != nil {
		return fmt.Errorf("inspect migration state: %w", err)
	}
	if err := validateMigrationPreflight(state); err != nil {
		return err
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	current, err := goose.GetDBVersion(db)
	if err != nil {
		return fmt.Errorf("verify migration version: %w", err)
	}
	if current != state.TargetVersion {
		return fmt.Errorf(
			"%w: migration finished at schema %d, compiled target is %d",
			ErrMigrationStateInvalid,
			current,
			state.TargetVersion,
		)
	}
	if current >= 51 {
		if err := s.finalizeAvatarLockedLayerDigestMigration(context.Background()); err != nil {
			return fmt.Errorf("finalize avatar locked-layer digests: %w", err)
		}
	}
	return nil
}

func inspectMigrationPreflightState(db *sql.DB, target int64) (migrationPreflightState, error) {
	state := migrationPreflightState{TargetVersion: target}
	if err := db.QueryRow(`SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&state.VersionTableExists); err != nil {
		return migrationPreflightState{}, fmt.Errorf("locate Goose version table: %w", err)
	}
	if state.VersionTableExists {
		current, err := goose.GetDBVersion(db)
		if err != nil {
			return migrationPreflightState{}, fmt.Errorf("read Goose version: %w", err)
		}
		state.CurrentVersion = current
	}
	if err := db.QueryRow(`SELECT to_regclass('accounts') IS NOT NULL`).Scan(&state.AccountsTableExists); err != nil {
		return migrationPreflightState{}, fmt.Errorf("locate accounts table: %w", err)
	}
	if state.AccountsTableExists {
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM accounts)`).Scan(&state.AccountsPopulated); err != nil {
			return migrationPreflightState{}, fmt.Errorf("inspect accounts table: %w", err)
		}
	}
	return state, nil
}

func validateMigrationPreflight(state migrationPreflightState) error {
	if state.CurrentVersion < 0 || state.TargetVersion < 1 {
		return fmt.Errorf(
			"%w: current schema %d, compiled target %d",
			ErrMigrationStateInvalid,
			state.CurrentVersion,
			state.TargetVersion,
		)
	}
	if state.CurrentVersion > state.TargetVersion {
		return fmt.Errorf(
			"%w: database is schema %d, compiled target is %d",
			ErrMigrationSchemaAhead,
			state.CurrentVersion,
			state.TargetVersion,
		)
	}
	if state.CurrentVersion == 0 {
		if state.AccountsTableExists {
			return fmt.Errorf(
				"%w: accounts exists while Goose reports schema 0",
				ErrMigrationStateInvalid,
			)
		}
		return nil
	}
	if !state.VersionTableExists || !state.AccountsTableExists {
		return fmt.Errorf(
			"%w: schema %d requires both Goose version and accounts tables",
			ErrMigrationStateInvalid,
			state.CurrentVersion,
		)
	}
	if state.TargetVersion >= factDeletionActivationSchema &&
		state.CurrentVersion < factDeletionCompatibilitySchema &&
		state.AccountsPopulated {
		return fmt.Errorf(
			"%w: populated database is schema %d; first deploy a binary whose compiled schema is %d and wait for every writer to converge before deploying schema %d",
			ErrMigrationCompatibilityRequired,
			state.CurrentVersion,
			factDeletionCompatibilitySchema,
			factDeletionActivationSchema,
		)
	}
	return nil
}
