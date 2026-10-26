package testenv

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPostgresSchemaCleanupTimeout(t *testing.T) {
	for _, elapsed := range []time.Duration{0, time.Second, 20 * time.Second, 20*time.Second + time.Nanosecond, 2 * time.Minute} {
		if got, want := PostgresSchemaCleanupTimeout(elapsed), max(time.Minute, 3*elapsed); got != want {
			t.Fatalf("cleanup timeout = %s, want %s", got, want)
		}
	}
}

func TestPostgresSchemaDSNPreservesOptionsAndOverridesSearchPath(t *testing.T) {
	for _, dsn := range []string{
		"postgres://fixture:synthetic@localhost/example?sslmode=disable&search_path=old&options=-cstatement_timeout%3D5000",
		"host=localhost dbname=example user=fixture password=synthetic sslmode=disable options='-cstatement_timeout=5000' search_path=old",
	} {
		scoped, err := PostgresDSNWithSearchPath(dsn, "fixture_1")
		if err != nil {
			t.Fatal("scope failed")
		}
		parsed, err := pgx.ParseConfig(scoped)
		if err != nil {
			t.Fatal("parse scoped fixture failed")
		}
		if parsed.RuntimeParams["search_path"] != "fixture_1" || parsed.RuntimeParams["options"] != "-cstatement_timeout=5000" {
			t.Fatal("scoping lost options or failed to override search path")
		}
	}
	for _, schema := range []string{"", "bad'quote", "a,b", "a;DROP", "a b"} {
		if _, err := PostgresDSNWithSearchPath("host=localhost", schema); err == nil {
			t.Fatal("unsafe schema accepted")
		}
	}
}

func TestValidateHarnessPostgresDSN(t *testing.T) {
	const base = "postgres://postgres:test@127.0.0.1:5599/"
	for _, database := range []string{"gate_a", "witself", "codex_b", "fixture_without_allowlist"} {
		t.Run("unguarded/"+database, func(t *testing.T) {
			t.Setenv("WITSELF_SYNC_HARNESS_DB", "")
			if err := os.Unsetenv("WITSELF_SYNC_HARNESS_DB"); err != nil {
				t.Fatal("unset fixture guard failed")
			}
			if err := ValidateHarnessPostgresDSN(base + database + "?sslmode=disable"); err != nil {
				t.Fatal("local test database refused without a lane guard")
			}
		})
		t.Run("guarded/"+database, func(t *testing.T) {
			t.Setenv("WITSELF_SYNC_HARNESS_DB", database)
			if err := ValidateHarnessPostgresDSN(base + database + "?sslmode=disable"); err != nil {
				t.Fatal("local test database refused with a matching lane guard")
			}
		})
	}
	for _, tc := range []struct{ name, dsn string }{
		{"localhost", "postgres://fixture:synthetic@localhost:5432/witself?sslmode=disable"},
		{"ipv6", "postgres://fixture:synthetic@[::1]:5432/witself?sslmode=disable"},
		{"default port", "postgresql://fixture:synthetic@127.0.0.1/witself?sslmode=disable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WITSELF_SYNC_HARNESS_DB", "")
			if err := ValidateHarnessPostgresDSN(tc.dsn); err != nil {
				t.Fatal("local test database refused")
			}
		})
	}
	for _, tc := range []struct{ name, dsn, guard string }{
		{"guard mismatch", base + "codex_a?sslmode=disable", "codex_b"},
		{"remote", "postgres://fixture:synthetic@db.example:5599/codex_b?sslmode=disable", ""},
		{"host override", base + "codex_b?sslmode=disable&host=db.example", ""},
		{"database override", base + "codex_b?sslmode=disable&dbname=postgres", ""},
		{"startup override", base + "codex_b?sslmode=disable&options=-csearch_path%3Dpublic", ""},
		{"malformed query", base + "codex_b?sslmode=disable&host=db.example;", ""},
		{"malformed query escape", base + "codex_b?sslmode=disable&host=%", ""},
		{"multiple hosts", "postgres://fixture:synthetic@127.0.0.1:5599,db.example:5599/codex_b?sslmode=disable", ""},
		{"missing database", base + "?sslmode=disable", ""},
		{"fragment", base + "codex_b?sslmode=disable#fragment", ""},
		{"keyword", "host=127.0.0.1 port=5599 dbname=codex_b sslmode=disable", "codex_b"},
		{"malformed", "postgres://fixture:synthetic@%", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WITSELF_SYNC_HARNESS_DB", tc.guard)
			err := ValidateHarnessPostgresDSN(tc.dsn)
			if err == nil {
				t.Fatal("unsafe fixture accepted")
			}
			if strings.Contains(err.Error(), "synthetic") || strings.Contains(err.Error(), tc.dsn) {
				t.Fatal("fixture error disclosed connection data")
			}
		})
	}
}
