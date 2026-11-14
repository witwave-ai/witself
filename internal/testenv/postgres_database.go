package testenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	requirePackageDatabaseEnv  = "WITSELF_TEST_REQUIRE_PACKAGE_DATABASE"
	packageDatabaseUnavailable = "package database unavailable (WITSELF_TEST_REQUIRE_PACKAGE_DATABASE=1)"
	packageDatabaseTimeout     = 30 * time.Second
)

var (
	postgresPackageDatabase                                   = &postgresDatabaseProvisioner{}
	postgresPackageDatabaseOpen postgresDatabaseCatalogOpener = openPostgresDatabaseCatalog
)

// PostgresDatabaseName returns a reusable PostgreSQL identifier for a package.
// Including the original base in the digest keeps differently spelled bases
// distinct even when normalization or truncation makes their prefixes equal.
func PostgresDatabaseName(base, identity string) string {
	var prefix strings.Builder
	for _, character := range base {
		switch {
		case character >= 'A' && character <= 'Z':
			prefix.WriteByte(byte(character - 'A' + 'a'))
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9', character == '_':
			prefix.WriteByte(byte(character))
		default:
			prefix.WriteByte('_')
		}
	}
	name := prefix.String()
	if name == "" {
		name = "test"
	}
	if len(name) > 50 {
		name = name[:50]
	}
	digest := sha256.Sum256([]byte(base + "\x00" + identity))
	return name + "_" + hex.EncodeToString(digest[:6])
}

// PostgresDSNWithDatabase changes only database components. Unlike net/url,
// pgx accepts libpq multi-host URLs and literal '+' and '#' characters. Keep
// every other input byte, including keyword quoting and repeated options.
func PostgresDSNWithDatabase(baseDSN, database string) (string, error) {
	if database == "" || len(database) > 63 || strings.Trim(database, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
		return "", errors.New("invalid package database identifier")
	}
	if _, err := pgx.ParseConfig(baseDSN); err != nil {
		return "", errors.New("invalid package database configuration")
	}
	var rewritten string
	if strings.HasPrefix(baseDSN, "postgres://") || strings.HasPrefix(baseDSN, "postgresql://") {
		rewritten = postgresURLDSNWithDatabase(baseDSN, database)
	} else {
		rewritten = postgresKeywordDSNWithDatabase(baseDSN, database)
	}
	config, err := pgx.ParseConfig(rewritten)
	if err != nil || config.Database != database {
		return "", errors.New("invalid package database configuration")
	}
	return rewritten, nil
}

// The input has already been validated by pgx. Locate the path using the same
// authority boundaries as its libpq parser, rather than RFC URL boundaries.
func postgresURLDSNWithDatabase(dsn, database string) string {
	remaining := dsn[strings.Index(dsn, "://")+3:]
	if index := strings.IndexAny(remaining, "@/"); index >= 0 && remaining[index] == '@' {
		remaining = remaining[index+1:]
	}
	for {
		if strings.HasPrefix(remaining, "[") {
			remaining = remaining[strings.IndexByte(remaining, ']')+1:]
		} else {
			remaining = postgresDSNTailAtAny(remaining, ":/?,")
		}
		if strings.HasPrefix(remaining, ":") {
			remaining = postgresDSNTailAtAny(remaining[1:], "/?,")
		}
		if !strings.HasPrefix(remaining, ",") {
			break
		}
		remaining = remaining[1:]
	}
	prefix := dsn[:len(dsn)-len(remaining)] + "/" + database
	if strings.HasPrefix(remaining, "/") {
		remaining = postgresDSNTailAtAny(remaining, "?")
	}
	if remaining == "" {
		return prefix
	}
	pairs := strings.Split(remaining[1:], "&")
	for index, pair := range pairs {
		key, _, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		decoded, _ := url.PathUnescape(strings.Trim(key, " "))
		if decoded == "dbname" || decoded == "database" {
			pairs[index] = key + "=" + database
		}
	}
	return prefix + "?" + strings.Join(pairs, "&")
}

func postgresKeywordDSNWithDatabase(dsn, database string) string {
	const whitespace = " \t\n\r\v\f"
	var result strings.Builder
	remaining := dsn
	found := false
	for strings.TrimLeft(remaining, whitespace) != "" {
		equals := strings.IndexByte(remaining, '=')
		key := strings.Trim(remaining[:equals], whitespace)
		value := strings.TrimLeft(remaining[equals+1:], whitespace)
		start := len(remaining) - len(value)
		end := 0
		quoted := strings.HasPrefix(value, "'")
		if quoted {
			end++
		}
		for end < len(value) {
			if value[end] == '\\' {
				end++
				if end == len(value) {
					break
				}
			} else if quoted && value[end] == '\'' {
				end++
				break
			} else if !quoted && strings.ContainsRune(whitespace, rune(value[end])) {
				break
			}
			end++
		}
		result.WriteString(remaining[:start])
		if key == "dbname" || key == "database" {
			found = true
			if quoted {
				result.WriteByte('\'')
			}
			result.WriteString(database)
			if quoted {
				result.WriteByte('\'')
			}
		} else {
			result.WriteString(value[:end])
		}
		remaining = value[end:]
	}
	result.WriteString(remaining)
	if !found {
		// Prepending preserves final empty values and a final unquoted escape.
		return "dbname=" + database + " " + dsn
	}
	return result.String()
}

// PostgresDatabaseURL returns the process's package database without adding
// transaction attribution. An unset DSN remains empty for direct opt-in readers.
// Provisioning and optional diagnostics happen once, even with parallel tests.
func PostgresDatabaseURL(t testing.TB) string {
	t.Helper()
	base := os.Getenv(databaseURLEnv)
	if base == "" {
		return ""
	}
	return postgresPackageDatabase.database(t, base, postgresTestPackageIdentity(), os.Getenv(requirePackageDatabaseEnv) == "1", os.Stderr, postgresPackageDatabaseOpen)
}

func postgresTestPackageIdentity() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Path == "" || info.Path == "command-line-arguments" || info.Path == "command-line-arguments.test" {
		return ""
	}
	return strings.TrimSuffix(info.Path, ".test")
}

type postgresDatabaseReporter interface {
	Helper()
	Fatal(...any)
}

type postgresDatabaseProvisioner struct {
	once    sync.Once
	warning sync.Once
	dsn     string
	err     error
}

func (p *postgresDatabaseProvisioner) database(t postgresDatabaseReporter, base, identity string, required bool, output io.Writer, open postgresDatabaseCatalogOpener) string {
	t.Helper()
	p.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), packageDatabaseTimeout)
		defer cancel()
		p.dsn, p.err = provisionPostgresDatabaseWithCatalog(ctx, base, identity, open)
		if p.err != nil {
			p.dsn = base
		}
	})
	if p.err != nil {
		if required {
			t.Fatal(packageDatabaseUnavailable)
			return ""
		}
		p.warning.Do(func() { _, _ = fmt.Fprintln(output, "NOT RUN package_database_unavailable") })
	}
	return p.dsn
}

type postgresDatabaseCatalog interface {
	DatabaseExists(context.Context, string) (bool, error)
	CreateDatabase(context.Context, string) error
	Close(context.Context) error
}

type postgresDatabaseCatalogOpener func(context.Context, string) (postgresDatabaseCatalog, error)

func provisionPostgresDatabase(ctx context.Context, base, identity string) (string, error) {
	return provisionPostgresDatabaseWithCatalog(ctx, base, identity, openPostgresDatabaseCatalog)
}

func provisionPostgresDatabaseWithCatalog(ctx context.Context, base, identity string, open postgresDatabaseCatalogOpener) (string, error) {
	failure := errors.New("package database unavailable")
	if identity == "" {
		return "", failure
	}
	config, err := pgx.ParseConfig(base)
	if err != nil || config.Database == "" {
		return "", failure
	}
	name := PostgresDatabaseName(config.Database, identity)
	dsn, err := PostgresDSNWithDatabase(base, name)
	if err != nil {
		return "", failure
	}
	catalog, err := open(ctx, base)
	if err != nil {
		return "", failure
	}
	defer func() { _ = catalog.Close(ctx) }()
	exists, err := catalog.DatabaseExists(ctx, name)
	if err != nil {
		return "", failure
	}
	if !exists {
		if err := catalog.CreateDatabase(ctx, name); err != nil {
			var postgresError *pgconn.PgError
			if !errors.As(err, &postgresError) || (postgresError.Code != "42P04" && postgresError.Code != "23505") {
				return "", failure
			}
			// A unique violation is success only after proving this database
			// exists, including a race with another instance of this package.
			if exists, err := catalog.DatabaseExists(ctx, name); err != nil || !exists {
				return "", failure
			}
		}
	}
	return dsn, nil
}

type pgxPostgresDatabaseCatalog struct{ connection *pgx.Conn }

func openPostgresDatabaseCatalog(ctx context.Context, dsn string) (postgresDatabaseCatalog, error) {
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &pgxPostgresDatabaseCatalog{connection: connection}, nil
}

func (c *pgxPostgresDatabaseCatalog) DatabaseExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := c.connection.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, name).Scan(&exists)
	return exists, err
}

func (c *pgxPostgresDatabaseCatalog) CreateDatabase(ctx context.Context, name string) error {
	_, err := c.connection.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()+` TEMPLATE template0`)
	return err
}

func (c *pgxPostgresDatabaseCatalog) Close(ctx context.Context) error {
	return c.connection.Close(ctx)
}
