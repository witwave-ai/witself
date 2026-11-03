package testenv

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type fakeDisposableCatalog struct {
	database    string
	superuser   bool
	databases   []string
	identityErr error
	listErr     error
	listCalls   int
}

func (c *fakeDisposableCatalog) Identity(context.Context) (string, bool, error) {
	return c.database, c.superuser, c.identityErr
}

func (c *fakeDisposableCatalog) Databases(context.Context) ([]string, error) {
	c.listCalls++
	return c.databases, c.listErr
}

func TestDisposableClusterCatalogPositiveIdentification(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*fakeDisposableCatalog)
		accepted bool
		listed   bool
	}{
		{"exclusive_server", func(*fakeDisposableCatalog) {}, true, true},
		{"not_superuser", func(c *fakeDisposableCatalog) { c.superuser = false }, false, false},
		{"wrong_actual_database", func(c *fakeDisposableCatalog) { c.database = "postgres" }, false, false},
		{"identity_query_error", func(c *fakeDisposableCatalog) { c.identityErr = errors.New("private fixture marker") }, false, false},
		{"catalog_query_error", func(c *fakeDisposableCatalog) { c.listErr = errors.New("private fixture marker") }, false, true},
		{"shared_codex_b", func(c *fakeDisposableCatalog) { c.databases = append(c.databases, "codex_b") }, false, true},
		{"shared_other_codex", func(c *fakeDisposableCatalog) { c.databases = append(c.databases, "codex_future") }, false, true},
		{"shared_prefix_only", func(c *fakeDisposableCatalog) { c.databases = append(c.databases, "codex_") }, false, true},
		{"underscore_literal", func(c *fakeDisposableCatalog) { c.databases = append(c.databases, "codexsafe") }, true, true},
		{"missing_selected_database", func(c *fakeDisposableCatalog) { c.databases = []string{"template0"} }, false, true},
		{"empty_catalog", func(c *fakeDisposableCatalog) { c.databases = nil }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := &fakeDisposableCatalog{database: "witself", superuser: true, databases: []string{"witself", "template0", "template1"}}
			tc.mutate(catalog)
			if disposableCatalogAllowed(context.Background(), catalog) != tc.accepted {
				t.Fatal("disposable selector catalog decision mismatch")
			}
			if (catalog.listCalls == 1) != tc.listed {
				t.Fatal("disposable selector catalog boundary mismatch")
			}
		})
	}
}

func TestDisposableClusterConnectionPositiveIdentification(t *testing.T) {
	t.Setenv("WITSELF_SYNC_HARNESS_DB", "")
	const fixtureURL = "postgres://fixture:synthetic@127.0.0.1:5432/"
	for _, tc := range []struct {
		name, enabled, dsn string
		accepted           bool
	}{
		{"enabled", "1", fixtureURL + "witself?sslmode=disable", true},
		{"disabled", "", fixtureURL + "witself?sslmode=disable", false},
		{"nonliteral_opt_in", "true", fixtureURL + "witself?sslmode=disable", false},
		{"shared_target", "1", fixtureURL + "codex_b?sslmode=disable", false},
		{"postgres_target", "1", fixtureURL + "postgres?sslmode=disable", false},
		{"missing_target", "1", "", false},
		{"remote_target", "1", "postgres://fixture:synthetic@db.example/witself", false},
		{"database_override", "1", fixtureURL + "witself?dbname=codex_b", false},
		{"host_override", "1", fixtureURL + "witself?host=db.example", false},
		{"startup_override", "1", fixtureURL + "witself?options=-crole=owner", false},
		{"keyword_target", "1", "host=127.0.0.1 dbname=witself", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if disposableTarget(tc.enabled, tc.dsn) != tc.accepted {
				t.Fatal("disposable selector connection decision mismatch")
			}
		})
	}
	t.Setenv("WITSELF_SYNC_HARNESS_DB", "codex_b")
	if disposableTarget("1", fixtureURL+"witself?sslmode=disable") {
		t.Fatal("disposable selector ignored shared lane fence")
	}
}

func TestDisposableClusterUnavailableNeverConnects(t *testing.T) {
	t.Setenv("WITSELF_TEST_DISPOSABLE_CLUSTER", "")
	t.Setenv(databaseURLEnv, "invalid fixture marker")
	if DisposableCluster(t) != nil {
		t.Fatal("disposable selector accepted missing opt in")
	}
	opened := false
	fixture, err := allocateDisposableCluster(context.Background(), "1", "invalid fixture marker", disposableAllocator{
		open: func(string) (disposableConnection, error) {
			opened = true
			return nil, errors.New("unexpected connection")
		},
	})
	if fixture != nil || err == nil || err.Error() != disposableClusterUnavailable || opened {
		t.Fatal("disposable selector accepted invalid connection")
	}
}

func TestDisposableClusterSuffixAndReviewedRoleLookup(t *testing.T) {
	first, err := disposableLowercaseSuffix()
	if err != nil || len(first) != 16 || strings.Trim(first, "abcdefghijklmnopqrstuvwxyz") != "" {
		t.Fatal("disposable suffix violated identifier contract")
	}
	second, err := disposableLowercaseSuffix()
	if err != nil || first == second {
		t.Fatal("disposable suffix reused fixture identity")
	}
	fixture := &DisposablePostgresCluster{roles: map[string]string{"witself_sync_worker": "fixture_worker"}, roleDSNs: map[string]string{"witself_sync_worker": "private fixture marker"}}
	if fixture.Role("witself_sync_worker") != "fixture_worker" || fixture.RoleDSN("witself_sync_worker") == "" ||
		fixture.Role("unknown") != "" || fixture.RoleDSN("unknown") != "" {
		t.Fatal("disposable lookup accepted unreviewed role")
	}
}

// This fake is injected into the allocation path used by DisposableCluster, not
// merely into its catalog predicate. It records every executable statement.
type recordingDisposableConnection struct {
	catalog    fakeDisposableCatalog
	statements []string
	events     []string
	closed     int
}

func (c *recordingDisposableConnection) Identity(ctx context.Context) (string, bool, error) {
	c.events = append(c.events, "identity")
	return c.catalog.Identity(ctx)
}

func (c *recordingDisposableConnection) Databases(ctx context.Context) ([]string, error) {
	c.events = append(c.events, "databases")
	return c.catalog.Databases(ctx)
}

func (c *recordingDisposableConnection) Exec(_ context.Context, statement string) error {
	c.events = append(c.events, "statement")
	c.statements = append(c.statements, statement)
	return nil
}

func (c *recordingDisposableConnection) Residue(context.Context, string, []string) (bool, error) {
	c.events = append(c.events, "residue")
	return false, nil
}

func (c *recordingDisposableConnection) Close() error {
	c.closed++
	return nil
}

func TestDisposableClusterAllocationRefusesSharedCatalogBeforeStatements(t *testing.T) {
	t.Setenv("WITSELF_SYNC_HARNESS_DB", "")
	admin := &recordingDisposableConnection{catalog: fakeDisposableCatalog{
		database: "witself", superuser: true, databases: []string{"witself", "template0", "template1", "codex_b"},
	}}
	opened, randomCalls := 0, 0
	fixture, err := allocateDisposableCluster(context.Background(), "1", "postgres://fixture:synthetic@127.0.0.1:5432/witself?sslmode=disable", disposableAllocator{
		open:     func(string) (disposableConnection, error) { opened++; return admin, nil },
		suffix:   func() (string, error) { randomCalls++; return "fixture", nil },
		password: func() (string, error) { randomCalls++; return "synthetic", nil },
	})
	if fixture != nil || err == nil || err.Error() != disposableClusterUnavailable {
		t.Fatal("disposable allocator failed fixed shared-server refusal")
	}
	if len(admin.statements) != 0 || opened != 1 || randomCalls != 0 || admin.closed != 1 ||
		!slices.Equal(admin.events, []string{"identity", "databases"}) {
		t.Fatal("disposable allocator crossed shared-server no-statement boundary")
	}
}

func TestDisposableClusterAllocationExecutesReviewedStatementsInOrder(t *testing.T) {
	t.Setenv("WITSELF_SYNC_HARNESS_DB", "")
	admin := &recordingDisposableConnection{catalog: fakeDisposableCatalog{
		database: "witself", superuser: true, databases: []string{"witself", "template0", "template1"},
	}}
	database := &recordingDisposableConnection{}
	opened, passwords := 0, 0
	fixture, err := allocateDisposableCluster(context.Background(), "1", "postgres://fixture:synthetic@127.0.0.1:5432/witself?sslmode=disable", disposableAllocator{
		open: func(string) (disposableConnection, error) {
			opened++
			if opened == 1 {
				return admin, nil
			}
			return database, nil
		},
		suffix:   func() (string, error) { return "fixture", nil },
		password: func() (string, error) { passwords++; return "synthetic", nil },
	})
	if err != nil || fixture == nil {
		t.Fatal("disposable allocator rejected exclusive fake server")
	}
	t.Cleanup(func() {
		if fixture.Close() != nil {
			t.Error("disposable allocator fake cleanup failed")
		}
	})
	var expected []string
	for _, role := range []string{"owner", "server", "worker", "maintenance", "snapshot", "legacy"} {
		expected = append(expected, `CREATE ROLE "witself_sync_`+role+`_fixture" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD 'synthetic'`)
	}
	expected = append(expected, `CREATE DATABASE "witself_principals_fixture"`)
	if opened != 2 || passwords != 6 || len(database.statements) != 0 || !slices.Equal(admin.statements, expected) ||
		!slices.Equal(admin.events, []string{"identity", "databases", "statement", "statement", "statement", "statement", "statement", "statement", "statement"}) {
		t.Fatal("disposable allocator statement order or reviewed role attributes changed")
	}
}
