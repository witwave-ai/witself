package testenv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresDatabaseName(t *testing.T) {
	const identity = "github.com/witwave-ai/witself/internal/store"
	for _, test := range []struct{ name, base string }{
		{"short", "codex_b"},
		{"long", strings.Repeat("base", 40)},
		{"awkward", "Upper-case / punctuation.☃"},
		{"empty", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := PostgresDatabaseName(test.base, identity)
			if len(name) > 63 || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
				t.Fatal("package database name violates identifier contract")
			}
			if name != PostgresDatabaseName(test.base, identity) {
				t.Fatal("package database name is not stable")
			}
			if name == PostgresDatabaseName(test.base, "github.com/witwave-ai/witself/cmd/witself-server") {
				t.Fatal("package database name ignores package identity")
			}
		})
	}
	if !strings.HasPrefix(PostgresDatabaseName("codex_b", identity), "codex_b_") {
		t.Fatal("short base database prefix was lost")
	}
	if PostgresDatabaseName("Mixed-Case", identity) == PostgresDatabaseName("mixed_case", identity) {
		t.Fatal("normalized base aliases collide")
	}
	if PostgresDatabaseName(strings.Repeat("a", 70)+"b", identity) == PostgresDatabaseName(strings.Repeat("a", 70)+"c", identity) {
		t.Fatal("truncated base aliases collide")
	}
}

func TestPostgresPackageIdentity(t *testing.T) {
	if postgresTestPackageIdentity() != "github.com/witwave-ai/witself/internal/testenv" {
		t.Fatal("test binary build information did not identify its package")
	}
	t.Chdir(t.TempDir())
	if postgresTestPackageIdentity() != "github.com/witwave-ai/witself/internal/testenv" {
		t.Fatal("test package identity changed with working directory")
	}
}

func TestPostgresDSNWithDatabase(t *testing.T) {
	for _, test := range []struct{ name, input, expected string }{
		{"url_options", "postgres://fixture:private@invalid.example/base?sslmode=disable&application_name=literal+bytes&options=-cstatement_timeout%3D5000", "postgres://fixture:private@invalid.example/package_db?sslmode=disable&application_name=literal+bytes&options=-cstatement_timeout%3D5000"},
		{"url_encoded_override", "postgresql://fixture:private@invalid.example/base?%64bname=override&application_name=literal%20bytes&database=other&sslmode=disable", "postgresql://fixture:private@invalid.example/package_db?%64bname=package_db&application_name=literal%20bytes&database=package_db&sslmode=disable"},
		{"url_spaced_override", "postgres://fixture:private@invalid.example/base? dbname =override&application_name=x&sslmode=disable", "postgres://fixture:private@invalid.example/package_db? dbname =package_db&application_name=x&sslmode=disable"},
		{"url_multihost", "postgresql://fixture:private@[::1]:5432,invalid.example:5433/base?sslmode=disable&target_session_attrs=any", "postgresql://fixture:private@[::1]:5432,invalid.example:5433/package_db?sslmode=disable&target_session_attrs=any"},
		{"url_hash", "postgres://fixture:private@invalid.example/base#literal?application_name=literal#bytes&sslmode=disable", "postgres://fixture:private@invalid.example/package_db?application_name=literal#bytes&sslmode=disable"},
		{"url_userinfo_question", "postgres://fixture:pri?vate@invalid.example/base?sslmode=disable", "postgres://fixture:pri?vate@invalid.example/package_db?sslmode=disable"},
		{"url_no_path", "postgres://fixture:private@invalid.example?sslmode=disable", "postgres://fixture:private@invalid.example/package_db?sslmode=disable"},
		{"url_empty_query", "postgres://fixture:private@invalid.example/base?", "postgres://fixture:private@invalid.example/package_db?"},
		{"url_trailing_ampersand", "postgres://fixture:private@invalid.example/base?sslmode=disable&", "postgres://fixture:private@invalid.example/package_db?sslmode=disable&"},
		{"keyword_options", "host=invalid.example dbname=base user=fixture password='synthetic private' sslmode=disable options='-cstatement_timeout=5000'", "host=invalid.example dbname=package_db user=fixture password='synthetic private' sslmode=disable options='-cstatement_timeout=5000'"},
		{"keyword_quoted", "  host='invalid.example'\t dbname = 'old\\'db'  user=fixture password=private sslmode=disable\n", "  host='invalid.example'\t dbname = 'package_db'  user=fixture password=private sslmode=disable\n"},
		{"keyword_repeated", "dbname=one database='two' user=fixture password=private host=invalid.example sslmode=disable", "dbname=package_db database='package_db' user=fixture password=private host=invalid.example sslmode=disable"},
		{"keyword_final_empty", "user=fixture password=private host=invalid.example sslmode=disable application_name=", "dbname=package_db user=fixture password=private host=invalid.example sslmode=disable application_name="},
		{"keyword_final_escape", "user=fixture password=private host=invalid.example sslmode=disable application_name=literal\\", "dbname=package_db user=fixture password=private host=invalid.example sslmode=disable application_name=literal\\"},
		{"keyword_final_empty_database", "user=fixture password=private host=invalid.example sslmode=disable dbname= ", "user=fixture password=private host=invalid.example sslmode=disable dbname= package_db"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := PostgresDSNWithDatabase(test.input, "package_db")
			if err != nil || got != test.expected {
				t.Fatal("database rewrite changed bytes outside database components")
			}
			before, beforeErr := pgx.ParseConfig(test.input)
			after, afterErr := pgx.ParseConfig(got)
			if beforeErr != nil || afterErr != nil || after.Database != "package_db" || before.Host != after.Host || before.Port != after.Port || before.User != after.User || before.Password != after.Password {
				t.Fatal("database rewrite changed effective connection configuration")
			}
			if fmt.Sprint(before.RuntimeParams) != fmt.Sprint(after.RuntimeParams) || len(before.Fallbacks) != len(after.Fallbacks) {
				t.Fatal("database rewrite lost options or fallbacks")
			}
		})
	}
}

func TestPostgresDatabaseRejectsWithoutLeaking(t *testing.T) {
	for _, test := range []struct{ name, dsn, database string }{
		{"invalid_url", "postgres://private_user:private_password@private_host/%zz", "package_db"},
		{"invalid_keyword", "password='private_unterminated", "package_db"},
		{"invalid_identifier", "password=private_password dbname=base", "private_identifier;"},
		{"long_identifier", "password=private_password dbname=base", strings.Repeat("x", 64)},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := PostgresDSNWithDatabase(test.dsn, test.database)
			if output != "" || err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), test.dsn) {
				t.Fatal("database rewrite error leaked input or accepted invalid data")
			}
		})
	}
}

type fakePostgresDatabaseCatalog struct {
	exists                   []bool
	existsError              error
	createError              error
	queries, creates, closes int
	names                    []string
}

func (c *fakePostgresDatabaseCatalog) DatabaseExists(_ context.Context, name string) (bool, error) {
	c.names = append(c.names, name)
	c.queries++
	if c.existsError != nil {
		return false, c.existsError
	}
	if c.queries > len(c.exists) {
		return false, errors.New("private unexpected catalog read")
	}
	return c.exists[c.queries-1], nil
}

func (c *fakePostgresDatabaseCatalog) CreateDatabase(_ context.Context, name string) error {
	c.names = append(c.names, name)
	c.creates++
	return c.createError
}

func (c *fakePostgresDatabaseCatalog) Close(context.Context) error {
	c.closes++
	return nil
}

type postgresDatabaseFatalRecorder struct {
	mutex    sync.Mutex
	messages []string
}

func (*postgresDatabaseFatalRecorder) Helper() {}
func (r *postgresDatabaseFatalRecorder) Fatal(values ...any) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.messages = append(r.messages, fmt.Sprint(values...))
}

func TestPostgresDatabaseProvisioner(t *testing.T) {
	const base = "postgres://private_user:private_password@private_host/base?sslmode=disable&application_name=preserved"
	for _, test := range []struct {
		name              string
		catalog           fakePostgresDatabaseCatalog
		openError         error
		required, failure bool
		queries, creates  int
	}{
		{name: "absent_create", catalog: fakePostgresDatabaseCatalog{exists: []bool{false}}, queries: 1, creates: 1},
		{name: "present_reuse", catalog: fakePostgresDatabaseCatalog{exists: []bool{true}}, queries: 1},
		{name: "lost_create_race", catalog: fakePostgresDatabaseCatalog{exists: []bool{false, true}, createError: &pgconn.PgError{Code: "42P04", Message: "private duplicate"}}, queries: 2, creates: 1},
		{name: "lost_unique_race", catalog: fakePostgresDatabaseCatalog{exists: []bool{false, true}, createError: &pgconn.PgError{Code: "23505", Message: "private conflict"}}, queries: 2, creates: 1},
		{name: "race_without_database", catalog: fakePostgresDatabaseCatalog{exists: []bool{false, false}, createError: &pgconn.PgError{Code: "23505", Message: "private unrelated conflict"}}, queries: 2, creates: 1, failure: true},
		{name: "refused", catalog: fakePostgresDatabaseCatalog{exists: []bool{false}, createError: errors.New("private permission denied")}, queries: 1, creates: 1, failure: true},
		{name: "required_refused", catalog: fakePostgresDatabaseCatalog{exists: []bool{false}, createError: errors.New("private permission denied")}, queries: 1, creates: 1, failure: true, required: true},
		{name: "catalog_unavailable", catalog: fakePostgresDatabaseCatalog{existsError: errors.New("private catalog unreachable")}, queries: 1, failure: true},
		{name: "connection_unavailable", openError: errors.New("private connection refused"), failure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var state postgresDatabaseProvisioner
			var reporter postgresDatabaseFatalRecorder
			var output bytes.Buffer
			opens := 0
			open := func(ctx context.Context, dsn string) (postgresDatabaseCatalog, error) {
				opens++
				if _, ok := ctx.Deadline(); !ok || dsn != base {
					t.Fatal("provisioner omitted bounded context or changed administrator target")
				}
				return &test.catalog, test.openError
			}
			for range 3 {
				got := state.database(&reporter, base, "synthetic/package", test.required, &output, open)
				switch {
				case test.failure && test.required:
					if got != "" {
						t.Fatal("strict provisioning refusal returned a connection")
					}
				case test.failure:
					if got != base {
						t.Fatal("optional provisioning refusal did not retain base")
					}
				default:
					config, err := pgx.ParseConfig(got)
					if err != nil || config.Database != PostgresDatabaseName("base", "synthetic/package") {
						t.Fatal("provisioning selected the wrong database")
					}
				}
			}
			if opens != 1 || test.catalog.queries != test.queries || test.catalog.creates != test.creates {
				t.Fatal("provisioner did not execute expected once-only transitions")
			}
			if test.openError == nil && test.catalog.closes != 1 {
				t.Fatal("provisioner left administrator open")
			}
			if test.failure && !test.required {
				if output.String() != "NOT RUN package_database_unavailable\n" {
					t.Fatal("fallback diagnostic was not exact and once-only")
				}
			} else if output.Len() != 0 {
				t.Fatal("provisioner emitted unexpected diagnostics")
			}
			if test.required && test.failure {
				if len(reporter.messages) != 3 {
					t.Fatal("strict refusal did not fail each caller")
				}
				for _, message := range reporter.messages {
					if message != packageDatabaseUnavailable {
						t.Fatal("strict refusal message was not fixed and value-free")
					}
				}
			} else if len(reporter.messages) != 0 {
				t.Fatal("optional provisioning failure became fatal")
			}
			if strings.Contains(output.String(), "private") || strings.Contains(output.String(), base) {
				t.Fatal("provisioner leaked configuration or driver error")
			}
		})
	}
}

func TestPostgresDatabaseProvisionerParallelOnce(t *testing.T) {
	var state postgresDatabaseProvisioner
	var reporter postgresDatabaseFatalRecorder
	var output bytes.Buffer
	var opens atomic.Int32
	catalog := &fakePostgresDatabaseCatalog{exists: []bool{false}}
	open := func(context.Context, string) (postgresDatabaseCatalog, error) { opens.Add(1); return catalog, nil }
	var workers sync.WaitGroup
	for range 20 {
		workers.Go(func() {
			if state.database(&reporter, "dbname=base user=fixture password=private sslmode=disable", "synthetic/package", false, &output, open) == "" {
				t.Error("parallel provisioning returned an empty database")
			}
		})
	}
	workers.Wait()
	if opens.Load() != 1 || catalog.creates != 1 || catalog.closes != 1 || output.Len() != 0 || len(reporter.messages) != 0 {
		t.Fatal("parallel callers did not share one provisioning attempt")
	}
}

func TestPostgresDatabaseMissingIdentity(t *testing.T) {
	called := false
	_, err := provisionPostgresDatabaseWithCatalog(context.Background(), "dbname=base password=private", "", func(context.Context, string) (postgresDatabaseCatalog, error) { called = true; return nil, nil })
	if err == nil || called || err.Error() != "package database unavailable" {
		t.Fatal("missing package identity did not fail closed before opening the catalog")
	}
}

const postgresDatabaseHelperMode = "WITSELF_TESTENV_PACKAGE_DATABASE_MODE"

func TestPostgresDatabaseURL(t *testing.T) {
	for _, test := range []struct {
		name, required string
		fatal          bool
	}{
		{name: "optional"},
		{name: "zero_knob", required: "0"},
		{name: "strict", required: "1", fatal: true},
		{name: "non_exact_knob", required: "true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal("locate package database helper failed")
			}
			command := exec.Command(executable, "-test.run=^TestPostgresDatabaseURLHelperProcess$", "-test.v")
			command.Env = append(os.Environ(),
				postgresDatabaseHelperMode+"=1",
				databaseURLEnv+"=postgres://private_user:private_password@private_host/base?sslmode=disable",
				requirePackageDatabaseEnv+"="+test.required,
			)
			captured, err := command.CombinedOutput()
			output := string(captured)
			if (err != nil) != test.fatal {
				t.Fatal("package database accessor did not honor the exact strict knob")
			}
			if strings.Contains(output, "private_") || strings.Contains(output, "postgres://") {
				t.Fatal("package database accessor leaked input or catalog error")
			}
			if test.fatal {
				if strings.Count(output, packageDatabaseUnavailable) != 1 || strings.Contains(output, "NOT RUN package_database_unavailable") {
					t.Fatal("strict accessor did not emit only its fixed fatal diagnostic")
				}
			} else if strings.Count(output, "NOT RUN package_database_unavailable\n") != 1 {
				t.Fatal("optional accessor did not emit its fixed diagnostic exactly once")
			}
		})
	}
}

func TestPostgresDatabaseURLHelperProcess(t *testing.T) {
	if os.Getenv(postgresDatabaseHelperMode) != "1" {
		return
	}
	postgresPackageDatabase = &postgresDatabaseProvisioner{}
	postgresPackageDatabaseOpen = func(context.Context, string) (postgresDatabaseCatalog, error) {
		return nil, errors.New("private_catalog_error")
	}
	for range 3 {
		if PostgresDatabaseURL(t) != os.Getenv(databaseURLEnv) {
			t.Fatal("optional accessor did not return its unchanged base")
		}
	}
}

func TestPostgresDatabaseURLUnset(t *testing.T) {
	t.Setenv(databaseURLEnv, "")
	t.Setenv(requirePackageDatabaseEnv, "1")
	if PostgresDatabaseURL(t) != "" {
		t.Fatal("unset database accessor returned a connection")
	}
}

func TestPostgresDatabaseURLOnce(t *testing.T) {
	previousState, previousOpen := postgresPackageDatabase, postgresPackageDatabaseOpen
	t.Cleanup(func() {
		postgresPackageDatabase, postgresPackageDatabaseOpen = previousState, previousOpen
	})
	postgresPackageDatabase = &postgresDatabaseProvisioner{}
	catalog := &fakePostgresDatabaseCatalog{exists: []bool{true}}
	opens := 0
	postgresPackageDatabaseOpen = func(context.Context, string) (postgresDatabaseCatalog, error) {
		opens++
		return catalog, nil
	}
	t.Setenv(databaseURLEnv, "dbname=first user=fixture password=private sslmode=disable")
	t.Setenv(requirePackageDatabaseEnv, "1")
	first := PostgresDatabaseURL(t)
	t.Chdir(t.TempDir())
	t.Setenv(databaseURLEnv, "dbname=second user=fixture password=private sslmode=disable")
	second := PostgresDatabaseURL(t)
	parsed, err := pgx.ParseConfig(first)
	if err != nil || parsed.Database != PostgresDatabaseName("first", "github.com/witwave-ai/witself/internal/testenv") || first != second || opens != 1 {
		t.Fatal("public accessor did not retain its once-per-process package database")
	}
}
