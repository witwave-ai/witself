package testenv

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // register the test fixture database driver
)

// PostgresSchemaReporter is the small testing surface used by schema fixtures.
// It also admits value-free reporters in migration certification tests.
type PostgresSchemaReporter interface {
	Helper()
	Cleanup(func())
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
}

var postgresSchemaSequence atomic.Uint64

// PostgresSchema owns one disposable schema, never a database or shared role.
// Its DSN is private fixture material: callers must never include it in logs.
// Close consumers before Close; registering their cleanup after this helper
// naturally gives that ordering through testing's last-in-first-out cleanup.
type PostgresSchema struct {
	name    string
	dsn     string
	admin   *sql.DB
	started time.Time
	once    sync.Once
	err     error
}

// NewPostgresSchema allocates a unique schema and registers bounded cleanup
// before any consumer can open it. It does not apply migrations.
func NewPostgresSchema(t PostgresSchemaReporter, baseDSN string) *PostgresSchema {
	t.Helper()
	fixture := &PostgresSchema{
		name: fmt.Sprintf("witself_migration_%d_%d", os.Getpid(), postgresSchemaSequence.Add(1)),
	}
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal("open schema fixture administrator failed")
		return nil
	}
	fixture.admin = admin
	fixture.started = time.Now()
	if _, err := admin.Exec(`CREATE SCHEMA ` + fixture.name); err != nil {
		_ = admin.Close()
		t.Fatal("create schema fixture failed")
		return nil
	}
	t.Cleanup(func() {
		if err := fixture.Close(); err != nil {
			t.Errorf("schema fixture cleanup: %v", err)
		}
	})
	fixture.dsn, err = PostgresDSNWithSearchPath(baseDSN, fixture.name)
	if err != nil {
		t.Fatal("scope schema fixture connection failed")
		return nil
	}
	return fixture
}

// Name is the generated, SQL-safe schema identifier, not an account identifier.
func (s *PostgresSchema) Name() string { return s.name }

// DSN returns the private scoped connection string; it must never be logged.
func (s *PostgresSchema) DSN() string { return s.dsn }

// Stats exposes only value-free administrator pool counts for cleanup assertions.
func (s *PostgresSchema) Stats() sql.DBStats { return s.admin.Stats() }

// Close drops exactly the allocated schema, proves absence, and closes its pool.
// Repeated calls return the first result without touching any other schema.
func (s *PostgresSchema) Close() error {
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), PostgresSchemaCleanupTimeout(time.Since(s.started)))
		defer cancel()
		if _, err := s.admin.ExecContext(ctx, `DROP SCHEMA `+s.name+` CASCADE`); err != nil {
			s.err = errors.New("drop allocated schema failed")
		} else {
			var exists bool
			if err := s.admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, s.name).Scan(&exists); err != nil || exists {
				s.err = errors.New("allocated schema absence not proven")
			}
		}
		if err := s.admin.Close(); err != nil {
			s.err = errors.Join(s.err, errors.New("close schema administrator failed"))
		}
		if stats := s.admin.Stats(); stats.OpenConnections != 0 || stats.InUse != 0 {
			s.err = errors.Join(s.err, errors.New("schema administrator connections remain"))
		}
	})
	return s.err
}

// PostgresSchemaCleanupTimeout includes the full fixture lifetime, preserving
// the migration fixture's three-times-elapsed budget and one-minute floor.
func PostgresSchemaCleanupTimeout(elapsed time.Duration) time.Duration {
	return max(3*elapsed, time.Minute)
}

// PostgresDSNWithSearchPath supports URL and keyword DSNs and preserves options.
// The schema is controlled fixture input, never arbitrary caller SQL.
func PostgresDSNWithSearchPath(baseDSN, schema string) (string, error) {
	if schema == "" || strings.Trim(schema, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
		return "", errors.New("invalid fixture schema identifier")
	}
	if strings.HasPrefix(baseDSN, "postgres://") || strings.HasPrefix(baseDSN, "postgresql://") {
		u, err := url.Parse(baseDSN)
		if err != nil {
			return "", errors.New("invalid PostgreSQL fixture URL")
		}
		query := u.Query()
		query.Set("search_path", schema)
		u.RawQuery = query.Encode()
		return u.String(), nil
	}
	return baseDSN + " search_path='" + schema + "'", nil
}

// ValidateHarnessPostgresDSN rejects non-local targets and connection overrides
// before allocation. WITSELF_SYNC_HARNESS_DB is an optional lane guard: when set,
// it must match the database selected by WITSELF_TEST_DATABASE_URL.
func ValidateHarnessPostgresDSN(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || !harnessLocalHost(u.Hostname()) || u.Path == "" || u.Path == "/" || u.Fragment != "" {
		return errors.New("harness requires a local test database")
	}
	database := strings.TrimPrefix(u.Path, "/")
	if guard := os.Getenv("WITSELF_SYNC_HARNESS_DB"); guard != "" && guard != database {
		return errors.New("harness database lane guard does not match")
	}
	// Query overrides and multi-host fallbacks must not redirect an apparently
	// local URL. Require the parsed driver's effective target as well.
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return errors.New("harness connection query is not allowed")
	}
	for key := range query {
		if key != "sslmode" {
			return errors.New("harness connection override is not allowed")
		}
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil || !harnessLocalHost(config.Host) || config.Database != database {
		return errors.New("harness effective database target is not allowed")
	}
	// Default SSL negotiation can add a fallback to the same local server.
	for _, fallback := range config.Fallbacks {
		if !harnessLocalHost(fallback.Host) {
			return errors.New("harness effective database target is not allowed")
		}
	}
	return nil
}

func harnessLocalHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
