package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
)

type captureProofCatalog struct {
	database string
	exists   bool
	count    int64
	token    string
	future   bool
	err      error
}

func (c captureProofCatalog) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	return syncPrincipalCleanupRow(func(dest ...any) error {
		if c.err != nil {
			return c.err
		}
		if strings.Contains(query, "current_database()") {
			*dest[0].(*string), *dest[1].(*bool) = c.database, c.exists
		} else {
			*dest[0].(*int64), *dest[1].(*string), *dest[2].(*bool) = c.count, c.token, c.future
		}
		return nil
	})
}

func TestSyncCaptureProofDatabaseIdentification(t *testing.T) {
	good := captureProofCatalog{database: "witself_captureproof_fixture", exists: true, count: 1, token: "fixture-only", future: true}
	for _, tc := range []struct {
		name   string
		change func(*captureProofCatalog)
	}{
		{"shared_database", func(c *captureProofCatalog) { c.database = "codex_b" }},
		{"case_alias", func(c *captureProofCatalog) { c.database = "WITSELF_CAPTUREPROOF_fixture" }},
		{"missing_marker", func(c *captureProofCatalog) { c.exists = false }},
		{"empty_marker", func(c *captureProofCatalog) { c.count = 0 }},
		{"multiple_markers", func(c *captureProofCatalog) { c.count = 2 }},
		{"wrong_token", func(c *captureProofCatalog) { c.token = "another-fixture" }},
		{"expired_marker", func(c *captureProofCatalog) { c.future = false }},
		{"catalog_failure", func(c *captureProofCatalog) { c.err = errors.New("private detail") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := good
			tc.change(&catalog)
			if verifySyncCaptureProofDatabase(context.Background(), catalog, good.token) != ErrSyncCaptureProofDatabaseUnidentified {
				t.Fatal("capture proof identification refusal changed")
			}
		})
	}
	if verifySyncCaptureProofDatabase(context.Background(), good, good.token) != nil {
		t.Fatal("capture proof identification rejected live marker")
	}
	if verifySyncCaptureProofDatabase(context.Background(), good, "") != ErrSyncCaptureProofDatabaseUnidentified {
		t.Fatal("capture proof identification accepted absent token")
	}
}

func TestSyncCaptureProofMarkerArchiveAlterContract(t *testing.T) {
	// Parse proof DDL through the same CREATE/ALTER inventory used by ordinary
	// sync migrations, while keeping it outside the embedded migration source.
	tables, err := captureProofTableContracts(syncCaptureProofMarkerSQL)
	if err != nil || len(tables) != 1 || !slices.Equal(tables["sync_capture_proof_marker"].columns, []string{"token", "expires_at"}) {
		t.Fatal("capture proof marker column contract changed")
	}
	if SyncSchemaVersion() != 3 {
		t.Fatal("capture proof marker entered ordinary migrations")
	}
	ordinary, _, err := syncMigrationTableContracts(SyncMigrationFiles())
	if err != nil {
		t.Fatal("capture proof marker ordinary inventory unavailable")
	}
	if _, exists := ordinary["sync_capture_proof_marker"]; exists {
		t.Fatal("capture proof marker entered ordinary migrations")
	}
	if _, exists := syncArchiveTableByName("sync_capture_proof_marker"); exists || slices.Contains(SyncWriterInventory(), "sync_capture_proof_marker") {
		t.Fatal("capture proof marker entered archives or covered writes")
	}
	if !strings.Contains(syncCaptureProofMarkerSQL, "REVOKE ALL ON sync_capture_proof_marker FROM PUBLIC") ||
		!strings.Contains(syncCaptureProofMarkerSQL, "ALTER TABLE sync_capture_proof_marker OWNER TO witself_sync_owner") ||
		!strings.Contains(syncCaptureProofMarkerSQL, "GRANT SELECT ON sync_capture_proof_marker TO witself_sync_server, witself_sync_worker") {
		t.Fatal("capture proof marker read grants changed")
	}
	mutated := strings.Replace(syncCaptureProofMarkerSQL, "expires_at timestamptz NOT NULL", "expires_at timestamptz NOT NULL, unregistered text", 1)
	tables, err = captureProofTableContracts(mutated)
	if err != nil || slices.Equal(tables["sync_capture_proof_marker"].columns, []string{"token", "expires_at"}) {
		t.Fatal("capture proof marker mutation escaped column contract")
	}
}

func captureProofTableContracts(raw string) (map[string]syncMigrationTableContract, error) {
	statements, err := syncSQLStatements(raw)
	if err != nil {
		return nil, err
	}
	var columns strings.Builder
	for _, statement := range statements {
		text := syncSQLText(statement)
		switch strings.ToLower(text) {
		case "revoke all on sync_capture_proof_marker from public",
			"alter table sync_capture_proof_marker owner to witself_sync_owner",
			"grant select on sync_capture_proof_marker to witself_sync_server , witself_sync_worker":
			continue
		}
		columns.WriteString(text + ";\n")
	}
	source := fstest.MapFS{"sync_migrations/0004_capture_proof_marker.sql": &fstest.MapFile{Data: []byte(columns.String())}}
	tables, _, err := syncMigrationTableContracts(source)
	return tables, err
}
