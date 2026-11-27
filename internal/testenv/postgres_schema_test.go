package testenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// FLAKE-65: a drop refused because the cluster's lock table is full is repeated
// until it runs, at most lockTableAttempts times, with a wait that doubles up to
// two seconds; any other refusal, and the end of the cleanup's budget, return at
// once. Time is the test bubble's own, so every wait is exact. The fixture's one
// schema drop goes through the repeat.
func TestRetryLockTableFull(t *testing.T) {
	full := &pgconn.PgError{Code: "53200", Message: "out of shared memory", Hint: "You might need to increase max_locks_per_transaction."}
	synctest.Test(t, func(t *testing.T) {
		calls, began := 0, time.Now()
		err := retryLockTableFull(t.Context(), 300*time.Millisecond, func() error {
			if calls++; calls < 4 {
				return fmt.Errorf("wrapped: %w", full)
			}
			return nil
		})
		if err != nil {
			t.Fatal("a lock-table refusal that cleared did not end in success")
		}
		if calls != 4 {
			t.Fatal("the statement was not repeated once per lock-table refusal")
		}
		if time.Since(began) != 2100*time.Millisecond {
			t.Fatal("three refusals did not wait once, twice and four times the first wait")
		}
	})
	synctest.Test(t, func(t *testing.T) {
		var at []time.Duration
		began := time.Now()
		err := retryLockTableFull(t.Context(), 300*time.Millisecond, func() error {
			at = append(at, time.Since(began))
			if len(at) > 2*lockTableAttempts { // a repeat that should not happen ends by itself
				return nil
			}
			return full
		})
		if !errors.Is(err, full) {
			t.Fatal("a lock table that stayed full did not return its refusal")
		}
		want := []time.Duration{0, 300, 900, 2100, 4100, 6100, 8100, 10100, 12100, 14100}
		if len(at) != len(want) {
			t.Fatal("a lock table that stayed full was not given up after ten attempts")
		}
		for i := range at {
			if at[i] != want[i]*time.Millisecond {
				t.Fatal("the wait between attempts did not double up to two seconds")
			}
		}
	})
	for _, refusal := range []error{
		&pgconn.PgError{Code: "42P01"}, &pgconn.PgError{Code: "53300", Hint: full.Hint},
		&pgconn.PgError{Code: "53200", Message: "out of memory"}, errors.New("plain"), context.DeadlineExceeded,
	} {
		calls := 0
		err := retryLockTableFull(t.Context(), time.Microsecond, func() error {
			if calls++; calls > 3 { // a repeat that should not happen ends by itself
				return nil
			}
			return refusal
		})
		if !errors.Is(err, refusal) {
			t.Fatal("another refusal was not returned as it is")
		}
		if calls != 1 {
			t.Fatal("another refusal was repeated")
		}
	}
	synctest.Test(t, func(t *testing.T) {
		budget, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		calls, began := 0, time.Now()
		err := retryLockTableFull(budget, 300*time.Millisecond, func() error {
			if calls++; calls > 2*lockTableAttempts {
				return nil
			}
			return full
		})
		if !errors.Is(err, full) {
			t.Fatal("an ended cleanup budget did not return the lock-table refusal")
		}
		if calls != 3 {
			t.Fatal("an ended cleanup budget still repeated the statement")
		}
		if time.Since(began) != time.Second {
			t.Fatal("the repeat did not end with the cleanup budget")
		}
	})
	source, err := os.ReadFile("postgres_schema.go")
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	if err != nil || strings.Count(text, "DROP SCHEMA") != 1 {
		t.Fatal("the fixture no longer has exactly one schema drop")
	}
	if !strings.Contains(text, "retryLockTableFull(ctx, wait, func() error {\n\t\t\t_, err := s.admin.ExecContext(ctx, `DROP SCHEMA `+s.name+` CASCADE`)\n") {
		t.Fatal("the fixture's schema drop left the lock-table repeat")
	}
}

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
	for _, tc := range []struct{ name, label string }{
		{"application_name process tag", "witself-test-1-synthetic"},
		{"application_name other label", "other-label"},
	} {
		for _, guard := range []string{"", "codex_b"} {
			mode := "unguarded"
			if guard != "" {
				mode = "guarded"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				t.Setenv("WITSELF_SYNC_HARNESS_DB", guard)
				if err := ValidateHarnessPostgresDSN(base + "codex_b?sslmode=disable&application_name=" + tc.label); err != nil {
					t.Fatal("local test database with application_name refused")
				}
			})
		}
	}
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
		{"application_name host override", base + "codex_b?sslmode=disable&application_name=witself-test-1-synthetic&host=localhost", "codex_b"},
		{"database override", base + "codex_b?sslmode=disable&dbname=postgres", ""},
		{"startup override", base + "codex_b?sslmode=disable&options=-csearch_path%3Dpublic", ""},
		{"malformed query", base + "codex_b?sslmode=disable&host=db.example;", ""},
		{"malformed query escape", base + "codex_b?sslmode=disable&host=%", ""},
		{"multiple hosts", "postgres://fixture:synthetic@127.0.0.1:5599,db.example:5599/codex_b?sslmode=disable", ""},
		{"missing database", base + "?sslmode=disable", ""},
		{"fragment", base + "codex_b?sslmode=disable#fragment", ""},
		{"keyword", "host=127.0.0.1 port=5599 dbname=codex_b sslmode=disable", "codex_b"},
		{"application_name keyword", "host=127.0.0.1 port=5599 dbname=codex_b sslmode=disable application_name=witself-test-1-synthetic", "codex_b"},
		{"malformed", "postgres://fixture:synthetic@%", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WITSELF_SYNC_HARNESS_DB", tc.guard)
			err := ValidateHarnessPostgresDSN(tc.dsn)
			if err == nil {
				t.Fatal("unsafe fixture accepted")
			}
			if tc.name == "application_name host override" && err.Error() != "harness connection override is not allowed" {
				t.Fatal("connection override refusal changed")
			}
			if strings.Contains(err.Error(), "synthetic") || strings.Contains(err.Error(), tc.dsn) {
				t.Fatal("fixture error disclosed connection data")
			}
		})
	}
}

func TestValidateHarnessPostgresPackageDatabase(t *testing.T) {
	t.Setenv("WITSELF_SYNC_HARNESS_DB", "codex_b")
	identity := postgresTestPackageIdentity()
	if identity == "" {
		t.Fatal("test package identity unavailable")
	}
	base := "postgres://fixture:synthetic@127.0.0.1:5599/"
	if ValidateHarnessPostgresDSN(base+PostgresDatabaseName("codex_b", identity)) != nil {
		t.Fatal("exact package database refused by its base lane guard")
	}
	for _, name := range []string{
		PostgresDatabaseName("codex_a", identity),
		PostgresDatabaseName("codex_b", identity+"/another"),
		"codex_b_unrelated",
	} {
		if ValidateHarnessPostgresDSN(base+name) == nil {
			t.Fatal("lane guard accepted an unrelated database")
		}
	}
}
