package testenv

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const disposableClusterUnavailable = "NOT RUN disposable_cluster_unavailable"

// DisposablePostgresCluster owns a database and six isolated login roles on an
// explicitly selected, exclusively owned test server. Connection strings and
// passwords are private fixture material and must never appear in diagnostics.
type DisposablePostgresCluster struct {
	database string
	dsn      string
	roles    map[string]string
	roleDSNs map[string]string
	created  []string
	admin    disposableConnection
	db       disposableConnection
	started  time.Time
	once     sync.Once
	err      error
}

type disposableCatalog interface {
	Identity(context.Context) (database string, superuser bool, err error)
	Databases(context.Context) ([]string, error)
}

// disposableConnection keeps catalog reads, allocation statements and cleanup on
// the same connection boundary. The allocator accepts only this boundary so its
// refusal wiring is exercised without connecting to a real PostgreSQL instance.
type disposableConnection interface {
	disposableCatalog
	Exec(context.Context, string) error
	Residue(context.Context, string, []string) (bool, error)
	Close() error
}

type disposableAllocator struct {
	open     func(string) (disposableConnection, error)
	suffix   func() (string, error)
	password func() (string, error)
}

type disposableSQLCatalog struct{ db *sql.DB }

func (c disposableSQLCatalog) Identity(ctx context.Context) (string, bool, error) {
	var database string
	var superuser bool
	err := c.db.QueryRowContext(ctx, `SELECT current_database(), current_setting('is_superuser') = 'on'`).Scan(&database, &superuser)
	return database, superuser, err
}

func (c disposableSQLCatalog) Databases(ctx context.Context) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT datname FROM pg_catalog.pg_database`)
	if err != nil {
		return nil, errors.New("disposable cluster catalog unavailable")
	}
	defer func() { _ = rows.Close() }()
	var databases []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, errors.New("disposable cluster catalog unavailable")
		}
		databases = append(databases, name)
	}
	return databases, rows.Err()
}

func (c disposableSQLCatalog) Exec(ctx context.Context, statement string) error {
	_, err := c.db.ExecContext(ctx, statement)
	return err
}

func (c disposableSQLCatalog) Residue(ctx context.Context, database string, roles []string) (bool, error) {
	var residue bool
	err := c.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_database WHERE datname=$1)
		OR EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = ANY($2))`, database, roles).Scan(&residue)
	return residue, err
}

func (c disposableSQLCatalog) Close() error { return c.db.Close() }

func openDisposableConnection(dsn string) (disposableConnection, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	return disposableSQLCatalog{db: db}, nil
}

func disposablePassword() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("disposable cluster random password failed")
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func disposableTarget(enabled, dsn string) bool {
	if enabled != "1" {
		return false
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Path != "/witself" || ValidateHarnessPostgresDSN(dsn) != nil {
		return false
	}
	return true
}

func disposableCatalogAllowed(ctx context.Context, catalog disposableCatalog) bool {
	database, superuser, err := catalog.Identity(ctx)
	if err != nil || database != "witself" || !superuser {
		return false
	}
	databases, err := catalog.Databases(ctx)
	if err != nil || len(databases) == 0 {
		return false
	}
	found := false
	for _, name := range databases {
		if strings.HasPrefix(name, "codex_") {
			return false
		}
		found = found || name == "witself"
	}
	return found
}

// DisposableCluster refuses before allocating anything unless explicit opt-in,
// a local witself database, superuser identity and absence of every codex_ database
// positively identify a disposable server. A nil result logs a fixed NOT RUN;
// callers bind it to their runtime result and skip their executable role proof.
func DisposableCluster(t testing.TB) *DisposablePostgresCluster {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture, err := allocateDisposableCluster(ctx, os.Getenv("WITSELF_TEST_DISPOSABLE_CLUSTER"), os.Getenv(databaseURLEnv), disposableAllocator{
		open: openDisposableConnection, suffix: disposableLowercaseSuffix, password: disposablePassword,
	})
	if err != nil {
		if err.Error() == disposableClusterUnavailable {
			t.Log(disposableClusterUnavailable)
		} else {
			t.Fatal(err.Error()) // Allocation returns only fixed, value-free errors.
		}
		return nil
	}
	t.Cleanup(func() {
		if fixture.Close() != nil {
			t.Error("disposable cluster cleanup failed")
		}
	})
	return fixture
}

func allocateDisposableCluster(ctx context.Context, enabled, dsn string, allocator disposableAllocator) (_ *DisposablePostgresCluster, resultErr error) {
	if !disposableTarget(enabled, dsn) {
		return nil, errors.New(disposableClusterUnavailable)
	}
	admin, err := allocator.open(dsn)
	if err != nil {
		return nil, errors.New(disposableClusterUnavailable)
	}
	if !disposableCatalogAllowed(ctx, admin) {
		_ = admin.Close()
		return nil, errors.New(disposableClusterUnavailable)
	}
	suffix, err := allocator.suffix()
	if err != nil {
		_ = admin.Close()
		return nil, errors.New("disposable cluster random identity failed")
	}
	fixture := &DisposablePostgresCluster{
		database: "witself_principals_" + suffix, roles: make(map[string]string), roleDSNs: make(map[string]string),
		admin: admin, started: time.Now(),
	}
	defer func() {
		if resultErr != nil && fixture.Close() != nil {
			resultErr = errors.New("disposable cluster allocation and cleanup failed")
		}
	}()
	// The kinds are fixture metadata, not a configurable production role map.
	// Store tests substitute only reviewed identifiers in their migration copy.
	for _, kind := range []string{"owner", "server", "worker", "maintenance", "snapshot", "legacy"} {
		base := "witself_sync_" + kind
		role := base + "_" + suffix
		password, err := allocator.password()
		if err != nil {
			return nil, errors.New("disposable cluster random password failed")
		}
		// Generated base64url contains no quote or SQL delimiter. Never print
		// this SQL or driver errors, which could include private material.
		if err := admin.Exec(ctx, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+
			` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD '`+password+`'`); err != nil {
			return nil, errors.New("disposable cluster create role failed")
		}
		fixture.created = append(fixture.created, role)
		fixture.roles[base] = role
		roleURL, _ := url.Parse(dsn) // disposableTarget has already validated the URL.
		roleURL.Path = "/" + fixture.database
		roleURL.User = url.UserPassword(role, password)
		fixture.roleDSNs[base] = roleURL.String()
	}
	if err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{fixture.database}.Sanitize()); err != nil {
		return nil, errors.New("disposable cluster create database failed")
	}
	databaseURL, _ := url.Parse(dsn)
	databaseURL.Path = "/" + fixture.database
	fixture.dsn = databaseURL.String()
	fixture.db, err = allocator.open(fixture.dsn)
	if err != nil {
		return nil, errors.New("disposable cluster open database failed")
	}
	return fixture, nil
}

func disposableLowercaseSuffix() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("disposable cluster random identity failed")
	}
	for i := range value {
		value[i] = 'a' + value[i]%26
	}
	return string(value), nil
}

// AdminDSN returns private fixture connection material; never log it.
func (f *DisposablePostgresCluster) AdminDSN() string { return f.dsn }

// Database returns only the randomly generated fixture database identifier.
func (f *DisposablePostgresCluster) Database() string { return f.database }

// Role returns the fixture name for one reviewed base role, or empty if unknown.
func (f *DisposablePostgresCluster) Role(base string) string { return f.roles[base] }

// RoleDSN returns private login material for a reviewed role, or empty if unknown.
func (f *DisposablePostgresCluster) RoleDSN(base string) string { return f.roleDSNs[base] }

// Close drops owned objects first, then the database, then the allocated roles.
// Callers must close their consumer pools before fixture cleanup runs.
func (f *DisposablePostgresCluster) Close() error {
	f.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), PostgresSchemaCleanupTimeout(time.Since(f.started)))
		defer cancel()
		if f.db != nil {
			for _, role := range f.created {
				if err := f.db.Exec(ctx, `DROP OWNED BY `+pgx.Identifier{role}.Sanitize()+` CASCADE`); err != nil {
					f.err = errors.Join(f.err, errors.New("disposable cluster drop owned failed"))
				}
			}
			if err := f.db.Close(); err != nil {
				f.err = errors.Join(f.err, errors.New("disposable cluster database close failed"))
			}
		}
		if f.dsn != "" {
			if err := f.admin.Exec(ctx, `DROP DATABASE `+pgx.Identifier{f.database}.Sanitize()+` WITH (FORCE)`); err != nil {
				f.err = errors.Join(f.err, errors.New("disposable cluster drop database failed"))
			}
		}
		for _, role := range f.created {
			if err := f.admin.Exec(ctx, `DROP ROLE `+pgx.Identifier{role}.Sanitize()); err != nil {
				f.err = errors.Join(f.err, errors.New("disposable cluster drop role failed"))
			}
		}
		if residue, err := f.admin.Residue(ctx, f.database, f.created); err != nil || residue {
			f.err = errors.Join(f.err, errors.New("disposable cluster absence not proven"))
		}
		if err := f.admin.Close(); err != nil {
			f.err = errors.Join(f.err, errors.New("disposable cluster administrator close failed"))
		}
	})
	return f.err
}
