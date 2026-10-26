package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// MigrationFiles exposes the immutable embedded SQL used by startup migrations
// to local development tools. Paths are rooted at migrations/.
func MigrationFiles() fs.FS { return migrationsFS }

const (
	factDeletionCompatibilitySchema = int64(27)
	factDeletionActivationSchema    = int64(28)

	// Every server replica takes the same database-derived session advisory
	// lock before inspecting or changing the Goose schema. The database name
	// scopes the otherwise cluster-wide application key while keeping it
	// stable across rolling-deployment pods.
	migrationAdvisoryTryLockSQL = `
		SELECT pg_try_advisory_lock(
			hashtextextended(current_database() || ':witself:store:migrate:v1', 0)
		)`
	migrationAdvisoryUnlockSQL = `
		SELECT pg_advisory_unlock(
			hashtextextended(current_database() || ':witself:store:migrate:v1', 0)
		)`
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

	// ErrMigrationWaitCanceled means a migration wait was canceled or expired.
	// MigrationWaitPhase identifies the phase without exposing database details.
	ErrMigrationWaitCanceled = errors.New("migration wait canceled")
)

type migrationWaitError struct {
	phase string
}

func (e *migrationWaitError) Error() string { return ErrMigrationWaitCanceled.Error() + ": " + e.phase }
func (e *migrationWaitError) Unwrap() error { return ErrMigrationWaitCanceled }

// MigrationWaitPhase returns the value-free phase of a canceled migration wait,
// or an empty string when err is not ErrMigrationWaitCanceled.
func MigrationWaitPhase(err error) string {
	var wait *migrationWaitError
	if errors.As(err, &wait) {
		return wait.phase
	}
	return ""
}

func migrationContextError(ctx context.Context, phase string) error {
	if ctx.Err() != nil {
		return &migrationWaitError{phase: phase}
	}
	return nil
}

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
	return s.MigrateContext(context.Background())
}

// MigrateContext applies pending migrations with cancellable waits. A migration
// file already being applied finishes before cancellation is reported; its SQL
// is never interrupted by this context. Previously completed files stay applied
// and the next startup resumes with the next pending file.
func (s *Store) MigrateContext(ctx context.Context) error {
	db, err := sql.Open("pgx", s.dsn)
	if err != nil {
		return fmt.Errorf("open for migrate: %w", err)
	}
	defer func() { _ = db.Close() }()

	current, err := migrateSchemaWithAdvisoryLock(ctx, db, int64(SchemaVersion()))
	if err != nil {
		return err
	}
	if current >= 51 {
		if _, err := s.finalizeAvatarLockedLayerDigestMigration(ctx); err != nil {
			if canceled := migrationContextError(ctx, "verify"); canceled != nil {
				return canceled
			}
			return fmt.Errorf("finalize avatar locked-layer digests: %w", err)
		}
	}
	return migrationContextError(ctx, "verify")
}

// migrateSchemaWithAdvisoryLock serializes startup migration preflight, Goose
// application, and final version verification across every replica connected
// to the database. The dedicated connection is intentional: PostgreSQL
// session advisory locks are owned by a connection, not a transaction.
func migrateSchemaWithAdvisoryLock(ctx context.Context, db *sql.DB, target int64) (current int64, retErr error) {
	lockConn, err := db.Conn(ctx)
	if err != nil {
		if canceled := migrationContextError(ctx, "advisory lock"); canceled != nil {
			return 0, canceled
		}
		return 0, fmt.Errorf("open migration lock connection: %w", err)
	}
	defer func() {
		// Close alone returns a connection to the pool. Discard the physical
		// session too, so even an uncertain acquisition or failed unlock cannot
		// retain a session advisory lock in that pool.
		if ctx.Done() != nil {
			_ = lockConn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = lockConn.Close()
	}()

	if err := acquireMigrationAdvisoryLock(ctx, lockConn); err != nil {
		if canceled := migrationContextError(ctx, "advisory lock"); canceled != nil {
			return 0, canceled
		}
		return 0, fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		cleanupCtx := context.Background()
		if ctx.Done() != nil {
			var cancel context.CancelFunc
			cleanupCtx, cancel = context.WithTimeout(cleanupCtx, time.Second)
			defer cancel()
		}
		if err := releaseMigrationAdvisoryLock(cleanupCtx, lockConn); err != nil && retErr == nil {
			retErr = fmt.Errorf("release migration advisory lock: %w", err)
		}
	}()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return 0, fmt.Errorf("goose dialect: %w", err)
	}
	state, err := inspectMigrationPreflightState(ctx, db, target)
	if err != nil {
		if canceled := migrationContextError(ctx, "preflight"); canceled != nil {
			return 0, canceled
		}
		return 0, fmt.Errorf("inspect migration state: %w", err)
	}
	if err := migrationContextError(ctx, "preflight"); err != nil {
		return 0, err
	}
	if err := validateMigrationPreflight(state); err != nil {
		return 0, err
	}
	if err := applyMigrationsContext(ctx, db); err != nil {
		if errors.Is(err, ErrMigrationWaitCanceled) {
			return 0, err
		}
		return 0, fmt.Errorf("apply migrations: %w", err)
	}
	current, err = goose.GetDBVersionContext(ctx, db)
	if err != nil {
		if canceled := migrationContextError(ctx, "verify"); canceled != nil {
			return 0, canceled
		}
		return 0, fmt.Errorf("verify migration version: %w", err)
	}
	if current != state.TargetVersion {
		return 0, fmt.Errorf(
			"%w: migration finished at schema %d, compiled target is %d",
			ErrMigrationStateInvalid,
			current,
			state.TargetVersion,
		)
	}
	return current, migrationContextError(ctx, "verify")
}

func applyMigrationsContext(ctx context.Context, db *sql.DB) error {
	if ctx.Done() == nil {
		// Preserve hosted Goose behavior, including its logging and errors.
		return goose.Up(db, "migrations")
	}
	for {
		if err := migrationContextError(ctx, "verify"); err != nil {
			return err
		}
		// UpByOne uses an uncancelled context. This protects both transactional
		// files and NO TRANSACTION files from cancellation partway through SQL.
		err := goose.UpByOne(db, "migrations")
		if errors.Is(err, goose.ErrNoNextVersion) {
			return migrationContextError(ctx, "verify")
		}
		if err != nil {
			return err
		}
	}
}

func acquireMigrationAdvisoryLock(ctx context.Context, conn *sql.Conn) error {
	const retryInterval = 100 * time.Millisecond
	for {
		var acquired bool
		// Poll instead of blocking inside pg_advisory_lock. A blocked SELECT
		// keeps an autocommit transaction's virtual XID alive, which can in
		// turn block CREATE INDEX CONCURRENTLY run by the lock holder.
		if err := conn.QueryRowContext(ctx, migrationAdvisoryTryLockSQL).Scan(&acquired); err != nil {
			return err
		}
		if acquired {
			return nil
		}

		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func releaseMigrationAdvisoryLock(ctx context.Context, conn *sql.Conn) error {
	var released bool
	if err := conn.QueryRowContext(ctx, migrationAdvisoryUnlockSQL).Scan(&released); err != nil {
		return err
	}
	if !released {
		return errors.New("lock was not held")
	}
	return nil
}

func inspectMigrationPreflightState(ctx context.Context, db *sql.DB, target int64) (migrationPreflightState, error) {
	state := migrationPreflightState{TargetVersion: target}
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&state.VersionTableExists); err != nil {
		return migrationPreflightState{}, fmt.Errorf("locate Goose version table: %w", err)
	}
	if state.VersionTableExists {
		current, err := goose.GetDBVersionContext(ctx, db)
		if err != nil {
			return migrationPreflightState{}, fmt.Errorf("read Goose version: %w", err)
		}
		state.CurrentVersion = current
	}
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('accounts') IS NOT NULL`).Scan(&state.AccountsTableExists); err != nil {
		return migrationPreflightState{}, fmt.Errorf("locate accounts table: %w", err)
	}
	if state.AccountsTableExists {
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM accounts)`).Scan(&state.AccountsPopulated); err != nil {
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
