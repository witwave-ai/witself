package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

//go:embed sync_migrations/*.sql
var syncMigrationsFS embed.FS

const syncSchemaVersionTable = "witself_sync_schema_version"

var (
	// ErrSyncSchemaAhead refuses an older binary without modifying sync evidence.
	ErrSyncSchemaAhead = errors.New("sync schema is newer than this binary")
	// ErrSyncSchemaStateInvalid covers incomplete or inconsistent namespace state.
	ErrSyncSchemaStateInvalid = errors.New("invalid sync schema state")
)

// SyncMigrationFiles exposes only the independent embedded sync namespace.
func SyncMigrationFiles() fs.FS { return syncMigrationsFS }

// SyncSchemaVersion is independent of the hosted/core SchemaVersion coordinate.
func SyncSchemaVersion() int {
	entries, err := syncMigrationsFS.ReadDir("sync_migrations")
	if err != nil {
		return 0
	}
	highest := 0
	for _, entry := range entries {
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if version, err := strconv.Atoi(prefix); ok && err == nil && version > highest {
			highest = version
		}
	}
	return highest
}

type syncVersionReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// appliedSyncSchemaVersion never creates a version table. Namespace presence is
// durable custody state, independent of runtime configuration. A missing version
// coordinate alongside sync tables cannot be treated as an ordinary core store.
func appliedSyncSchemaVersion(ctx context.Context, q syncVersionReader) (int, error) {
	var exists, tables bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('witself_sync_schema_version') IS NOT NULL,
		EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=current_schema() AND c.relkind IN ('r','p') AND c.relname LIKE 'sync\_%' ESCAPE '\')`).Scan(&exists, &tables); err != nil {
		return 0, errors.New("read sync schema state failed")
	}
	if !exists {
		if tables {
			return 0, ErrSyncSchemaStateInvalid
		}
		return 0, nil
	}
	var version int
	if err := q.QueryRow(ctx, `SELECT coalesce(max(version_id), 0) FROM (
		SELECT DISTINCT ON (version_id) version_id, is_applied
		FROM witself_sync_schema_version ORDER BY version_id, id DESC
	) AS latest_version WHERE is_applied`).Scan(&version); err != nil {
		return 0, errors.New("read sync schema version failed")
	}
	if version < 0 || (version == 0 && tables) || (version > 0 && !tables) {
		return 0, ErrSyncSchemaStateInvalid
	}
	if version > 0 && version <= SyncSchemaVersion() {
		required := syncArchiveTableNamesForSchema(version)
		for name, exclusion := range syncLocalArchiveExclusions {
			if exclusion.introducedSyncSchema <= version {
				required = append(required, name)
			}
		}
		var missing bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM unnest($1::text[]) AS required(name)
			WHERE NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname=current_schema() AND c.relkind IN ('r','p') AND c.relname=required.name))`, required).Scan(&missing); err != nil {
			return 0, errors.New("read sync schema coverage failed")
		}
		if missing {
			return 0, ErrSyncSchemaStateInvalid
		}
	}
	return version, nil
}

// AppliedSyncSchemaVersion reads installed sync custody without changing it.
func (s *Store) AppliedSyncSchemaVersion(ctx context.Context) (int, error) {
	return appliedSyncSchemaVersion(ctx, s.pool)
}

// MigrateSync installs the explicitly selected local sync namespace. Callers
// must require both local runtime and sync selection; hosted Migrate never calls
// this method. It holds the same database-wide migration session lock as core
// migrations. Each migration file completes before cancellation is observed.
func (s *Store) MigrateSync(ctx context.Context) (retErr error) {
	return s.InstallSyncGuards(ctx)
}

// InstallSyncGuards installs the selected sync namespace under the migration
// session lock. Migration 0002 drains the entire reviewed relation inventory
// with ACCESS EXCLUSIVE locks in its DDL transaction before attaching guards.
// Installing guards does not enable a capability or activate any account.
func (s *Store) InstallSyncGuards(ctx context.Context) (retErr error) {
	db, err := sql.Open("pgx", s.dsn)
	if err != nil {
		return errors.New("open sync migration database failed")
	}
	defer func() { _ = db.Close() }()
	lockConn, err := db.Conn(ctx)
	if err != nil {
		if canceled := migrationContextError(ctx, "advisory lock"); canceled != nil {
			return canceled
		}
		return errors.New("open sync migration lock failed")
	}
	defer func() {
		// Always discard the physical lock-owning session, even on cancellation
		// or an uncertain unlock, rather than returning it to a connection pool.
		_ = lockConn.Raw(func(any) error { return driver.ErrBadConn })
		_ = lockConn.Close()
	}()
	if err := acquireMigrationAdvisoryLock(ctx, lockConn); err != nil {
		if canceled := migrationContextError(ctx, "advisory lock"); canceled != nil {
			return canceled
		}
		return errors.New("acquire sync migration lock failed")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := releaseMigrationAdvisoryLock(cleanup, lockConn); err != nil && retErr == nil {
			retErr = errors.New("release sync migration lock failed")
		}
	}()
	current, err := s.AppliedSyncSchemaVersion(ctx)
	if err != nil {
		if canceled := migrationContextError(ctx, "preflight"); canceled != nil {
			return canceled
		}
		return err
	}
	if current > SyncSchemaVersion() {
		return ErrSyncSchemaAhead
	}
	core, err := s.AppliedSchemaVersion(ctx)
	if canceled := migrationContextError(ctx, "preflight"); canceled != nil {
		return canceled
	}
	if err != nil || core != SchemaVersion() {
		return ErrSyncSchemaStateInvalid
	}
	fsys, err := fs.Sub(syncMigrationsFS, "sync_migrations")
	if err != nil {
		return ErrSyncSchemaStateInvalid
	}
	// No process-global Goose filesystem, table, dialect, or Go registry state.
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys,
		goose.WithTableName(syncSchemaVersionTable), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return errors.New("configure sync migration provider failed")
	}
	for {
		if err := migrationContextError(ctx, "verify"); err != nil {
			return err
		}
		_, err := provider.UpByOne(context.Background())
		if errors.Is(err, goose.ErrNoNextVersion) {
			break
		}
		if err != nil {
			return errors.New("apply sync migration failed")
		}
	}
	current, err = s.AppliedSyncSchemaVersion(ctx)
	if err != nil {
		if canceled := migrationContextError(ctx, "verify"); canceled != nil {
			return canceled
		}
		return err
	}
	if current != SyncSchemaVersion() {
		return ErrSyncSchemaStateInvalid
	}
	return migrationContextError(ctx, "verify")
}
