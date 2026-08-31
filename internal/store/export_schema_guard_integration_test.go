package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestAccountExportsRejectDatabaseSchemaAheadPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(
		ctx,
		"export-schema-guard@witwave.ai",
		"export schema guard",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID)
	}()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate account = %v, %v", activated, err)
	}
	if err := st.SuspendAccountSystem(
		ctx,
		provisioned.AccountID,
		"evacuation",
		"export schema guard test",
	); err != nil {
		t.Fatal(err)
	}

	var fakeVersionRowID int64
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO goose_db_version (version_id, is_applied)
		VALUES ($1, true)
		RETURNING id`, int64(SchemaVersion())+1).Scan(&fakeVersionRowID); err != nil {
		t.Fatalf("insert future Goose version: %v", err)
	}
	fakeVersionPresent := true
	defer func() {
		if !fakeVersionPresent {
			return
		}
		if _, err := st.pool.Exec(
			context.Background(),
			`DELETE FROM goose_db_version WHERE id=$1`,
			fakeVersionRowID,
		); err != nil {
			t.Errorf("delete future Goose version: %v", err)
		}
	}()

	exportPaths := []struct {
		name string
		run  func(io.Writer) error
	}{
		{
			name: "account",
			run: func(w io.Writer) error {
				return st.ExportAccount(
					ctx, provisioned.AccountID, "schema-guard-source", "test", w,
				)
			},
		},
		{
			name: "evacuation",
			run: func(w io.Writer) error {
				return st.ExportAccountEvacuation(
					ctx,
					provisioned.AccountID,
					"evac_export_schema_guard",
					"schema-guard-source",
					"test",
					w,
				)
			},
		},
		{
			name: "backup",
			run: func(w io.Writer) error {
				return st.ExportAccountBackup(
					ctx,
					provisioned.AccountID,
					"backup_export_schema_guard",
					"schema-guard-source",
					"test",
					w,
				)
			},
		},
		{
			name: "self",
			run: func(w io.Writer) error {
				return st.ExportAccountSelf(
					ctx,
					provisioned.AccountID,
					"schema-guard-source",
					"test",
					w,
				)
			},
		},
	}
	for _, exportPath := range exportPaths {
		t.Run(exportPath.name, func(t *testing.T) {
			var archive bytes.Buffer
			err := exportPath.run(&archive)
			if !errors.Is(err, ErrExportSchemaAhead) {
				t.Fatalf("export error = %v, want ErrExportSchemaAhead", err)
			}
			if archive.Len() != 0 {
				t.Fatalf("refused export wrote %d archive bytes", archive.Len())
			}
		})
	}

	tag, err := st.pool.Exec(
		ctx,
		`DELETE FROM goose_db_version WHERE id=$1`,
		fakeVersionRowID,
	)
	if err != nil {
		t.Fatalf("delete future Goose version: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("deleted future Goose version rows = %d, want 1", tag.RowsAffected())
	}
	fakeVersionPresent = false

	var archive bytes.Buffer
	if err := st.ExportAccount(
		ctx,
		provisioned.AccountID,
		"schema-guard-source",
		"test",
		&archive,
	); err != nil {
		t.Fatalf("export after deleting future Goose version: %v", err)
	}
	if archive.Len() == 0 {
		t.Fatal("export after deleting future Goose version wrote no archive bytes")
	}
}
