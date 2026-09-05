package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestAccountBackupValidationRollsBackPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	const (
		backupID       = "bkp_active_roundtrip"
		closedBackupID = "bkp_closed_roundtrip"
	)
	provisioned, err := st.ProvisionAccount(
		ctx,
		"active-backup-roundtrip@witwave.ai",
		"active backup roundtrip",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	accountID := provisioned.AccountID
	defer func() {
		_ = deleteAccountForIntegrationTest(ctx, st, accountID)
	}()
	if activated, err := st.ActivateAccount(ctx, accountID); err != nil ||
		!activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}

	var archive bytes.Buffer
	if err := st.ExportAccountBackup(
		ctx, accountID, backupID, "backup-source", "test", &archive,
	); err != nil {
		t.Fatal(err)
	}
	manifest, err := archiveexport.Read(
		ctx,
		bytes.NewReader(archive.Bytes()),
		archiveexport.ImportOptions{CurrentSchema: SchemaVersion()},
	)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Purpose != archiveexport.PurposeBackup ||
		manifest.BackupID != backupID ||
		manifest.AccountID != accountID ||
		manifest.Status != "active" ||
		manifest.EvacuationID != "" {
		t.Fatalf("backup manifest = %+v", manifest)
	}

	var status string
	var evacuationID *string
	if err := st.pool.QueryRow(ctx, `
		SELECT status, evacuation_id
		  FROM accounts
		 WHERE id=$1`, accountID).Scan(&status, &evacuationID); err != nil {
		t.Fatal(err)
	}
	if status != "active" || evacuationID != nil {
		t.Fatalf("source after backup status=%q evacuation_id=%v",
			status, evacuationID)
	}

	if _, err := st.ImportAccount(
		ctx, accountID, bytes.NewReader(archive.Bytes()),
	); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("generic import of backup = %v, want ErrArchiveContent", err)
	}

	if err := st.CloseAccount(
		ctx, accountID, provisioned.OperatorID, "closed backup validation",
	); err != nil {
		t.Fatal(err)
	}
	var closedArchive bytes.Buffer
	if err := st.ExportAccountBackup(
		ctx, accountID, closedBackupID,
		"backup-source", "test", &closedArchive,
	); err != nil {
		t.Fatal(err)
	}
	closedManifest, err := archiveexport.Read(
		ctx,
		bytes.NewReader(closedArchive.Bytes()),
		archiveexport.ImportOptions{CurrentSchema: SchemaVersion()},
	)
	if err != nil {
		t.Fatal(err)
	}
	if closedManifest.Purpose != archiveexport.PurposeBackup ||
		closedManifest.BackupID != closedBackupID ||
		closedManifest.AccountID != accountID ||
		closedManifest.Status != "closed" ||
		closedManifest.EvacuationID != "" {
		t.Fatalf("closed backup manifest = %+v", closedManifest)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT status, evacuation_id
		  FROM accounts
		 WHERE id=$1`, accountID).Scan(&status, &evacuationID); err != nil {
		t.Fatal(err)
	}
	if status != "closed" || evacuationID != nil {
		t.Fatalf("source after closed backup status=%q evacuation_id=%v",
			status, evacuationID)
	}

	if err := deleteAccountForIntegrationTest(ctx, st, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ValidateAccountBackup(
		ctx, accountID, "bkp_wrong", bytes.NewReader(archive.Bytes()),
	); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("mismatched backup validation = %v, want ErrArchiveContent", err)
	}
	var accountRows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE id=$1`,
		accountID,
	).Scan(&accountRows); err != nil {
		t.Fatal(err)
	}
	if accountRows != 0 {
		t.Fatalf("mismatched backup validation landed %d account rows",
			accountRows)
	}

	for _, backup := range []struct {
		id     string
		status string
		body   []byte
	}{
		{id: backupID, status: "active", body: archive.Bytes()},
		{id: closedBackupID, status: "closed", body: closedArchive.Bytes()},
	} {
		for attempt := 1; attempt <= 2; attempt++ {
			validated, err := st.ValidateAccountBackup(
				ctx, accountID, backup.id, bytes.NewReader(backup.body),
			)
			if err != nil {
				t.Fatalf("%s validation attempt %d: %v",
					backup.status, attempt, err)
			}
			if validated.BackupID != backup.id ||
				validated.Purpose != archiveexport.PurposeBackup ||
				validated.Status != backup.status {
				t.Fatalf("validated %s manifest attempt %d = %+v",
					backup.status, attempt, validated)
			}
			if err := st.pool.QueryRow(ctx,
				`SELECT count(*) FROM accounts WHERE id=$1`,
				accountID,
			).Scan(&accountRows); err != nil {
				t.Fatal(err)
			}
			if accountRows != 0 {
				t.Fatalf(
					"%s validation attempt %d committed %d account rows",
					backup.status, attempt, accountRows,
				)
			}
		}
	}
}
