package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestImportAccountAuditCrossCheckPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	t.Run("consent event contradicts null account state", func(t *testing.T) {
		accountID, manifest, rows := buildImportAuditArchive(
			ctx, t, st, "consent-contradiction", false,
		)
		assertImportAuditAccountArchiveState(t, rows, true, true, false)
		appendImportAuditEvent(t, rows, manifest, accountID, VerbAccountConsentRecorded)

		archive := writeAvatarArchiveRows(t, manifest, manifest.Tables, rows)
		_, err := st.ImportAccount(ctx, accountID, bytes.NewReader(archive))
		if !errors.Is(err, ErrImportAuditContradiction) {
			t.Fatalf(
				"ImportAccount error = %v, want ErrImportAuditContradiction",
				err,
			)
		}
		assertImportAuditAccountAbsent(ctx, t, st, accountID)
	})

	t.Run("purge event contradicts null purged state", func(t *testing.T) {
		accountID, manifest, rows := buildImportAuditArchive(
			ctx, t, st, "purge-contradiction", false,
		)
		assertImportAuditAccountArchiveState(t, rows, true, true, false)
		appendImportAuditEvent(t, rows, manifest, accountID, VerbAccountPurged)

		archive := writeAvatarArchiveRows(t, manifest, manifest.Tables, rows)
		_, err := st.ImportAccount(ctx, accountID, bytes.NewReader(archive))
		if !errors.Is(err, ErrImportAuditContradiction) {
			t.Fatalf(
				"ImportAccount error = %v, want ErrImportAuditContradiction",
				err,
			)
		}
		assertImportAuditAccountAbsent(ctx, t, st, accountID)
	})

	t.Run("tombstone with fabricated consent event refused", func(t *testing.T) {
		// The purge worker deletes the portable audit stream in the same
		// transaction that scrubs the consent columns, so a tombstone archive
		// carrying a consent event can only be lossy or fabricated. The
		// purged-archive invariant (exactly one value-free purge event row)
		// fires first and wraps ErrArchiveContent.
		accountID, manifest, rows := buildImportAuditArchive(
			ctx, t, st, "purged-consent-history", true,
		)
		assertImportAuditAccountArchiveState(t, rows, true, false, false)
		appendImportAuditEvent(t, rows, manifest, accountID, VerbAccountConsentRecorded)

		archive := writeAvatarArchiveRows(t, manifest, manifest.Tables, rows)
		_, err := st.ImportAccount(ctx, accountID, bytes.NewReader(archive))
		if !errors.Is(err, ErrArchiveContent) {
			t.Fatalf(
				"ImportAccount error = %v, want ErrArchiveContent",
				err,
			)
		}
		assertImportAuditAccountAbsent(ctx, t, st, accountID)
	})

	t.Run("faithful purged tombstone imports cleanly", func(t *testing.T) {
		accountID, manifest, rows := buildImportAuditArchive(
			ctx, t, st, "purged-faithful", true,
		)
		assertImportAuditAccountArchiveState(t, rows, true, false, false)

		archive := writeAvatarArchiveRows(t, manifest, manifest.Tables, rows)
		if _, err := st.ImportAccount(
			ctx, accountID, bytes.NewReader(archive),
		); err != nil {
			t.Fatalf("ImportAccount faithful purged tombstone: %v", err)
		}
		assertImportedAuditAccountState(ctx, t, st, accountID, true)
	})

	t.Run("pre-consent archive with null consent state", func(t *testing.T) {
		accountID, manifest, rows := buildImportAuditArchive(
			ctx, t, st, "pre-consent", false,
		)
		assertImportAuditAccountArchiveState(t, rows, true, true, false)

		archive := writeAvatarArchiveRows(t, manifest, manifest.Tables, rows)
		if _, err := st.ImportAccount(
			ctx, accountID, bytes.NewReader(archive),
		); err != nil {
			t.Fatalf("ImportAccount pre-consent archive: %v", err)
		}
		assertImportedAuditAccountState(ctx, t, st, accountID, false)
	})
}

func buildImportAuditArchive(
	ctx context.Context,
	t *testing.T,
	st *Store,
	label string,
	purge bool,
) (string, archiveexport.Manifest, map[string][][]byte) {
	t.Helper()
	suffix := time.Now().UnixNano()
	provisioned, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("import-audit-%s-%d@witwave.ai", label, suffix),
		fmt.Sprintf("import audit %s %d", label, suffix),
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CloseAccount(
		ctx, provisioned.AccountID, provisioned.OperatorID, "import audit fixture",
	); err != nil {
		t.Fatal(err)
	}
	if purge {
		if _, err := st.pool.Exec(ctx, `
			UPDATE accounts
			   SET closed_at=statement_timestamp() - interval '2 minutes'
			 WHERE id=$1`, provisioned.AccountID); err != nil {
			t.Fatal(err)
		}
		result, err := st.processAccountPurgeCandidate(
			ctx, provisioned.AccountID, time.Minute, true,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !result.eligible || !result.purged {
			t.Fatalf("purge fixture result = %+v, want eligible and purged", result)
		}
	}

	var archive bytes.Buffer
	if err := st.ExportAccount(
		ctx, provisioned.AccountID, "import-audit-source", "test", &archive,
	); err != nil {
		t.Fatal(err)
	}
	manifest, rows := readAvatarArchiveRows(t, archive.Bytes(), SchemaVersion())
	if err := deleteAccountForIntegrationTest(
		ctx, st, provisioned.AccountID,
	); err != nil {
		t.Fatal(err)
	}
	return provisioned.AccountID, manifest, rows
}

func appendImportAuditEvent(
	t *testing.T,
	rows map[string][][]byte,
	manifest archiveexport.Manifest,
	accountID, verb string,
) {
	t.Helper()
	actorKind := ActorControlPlane
	metadata := map[string]any{
		"terms_version":   "terms-import-audit",
		"privacy_version": "privacy-import-audit",
	}
	if verb == VerbAccountPurged {
		actorKind = ActorSystem
		metadata = map[string]any{}
	}
	event := map[string]any{
		"id":           fmt.Sprintf("evt_import_audit_%d", time.Now().UnixNano()),
		"account_id":   accountID,
		"occurred_at":  manifest.ExportedAt.UTC().Format(time.RFC3339Nano),
		"actor_kind":   actorKind,
		"actor_id":     nil,
		"verb":         verb,
		"metadata":     metadata,
		"retain_until": nil,
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	rows["account_events"] = append(rows["account_events"], raw)
}

func assertImportAuditAccountArchiveState(
	t *testing.T,
	rows map[string][][]byte,
	consentNull, purgedNull, closedNull bool,
) {
	t.Helper()
	if len(rows["accounts"]) != 1 {
		t.Fatalf("archived accounts rows = %d, want 1", len(rows["accounts"]))
	}
	var account map[string]any
	if err := json.Unmarshal(rows["accounts"][0], &account); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"consent_terms_version", "consent_privacy_version", "consent_recorded_at",
	} {
		if gotNull := account[field] == nil; gotNull != consentNull {
			t.Fatalf("archived accounts %s null = %t, want %t", field, gotNull, consentNull)
		}
	}
	if gotNull := account["purged_at"] == nil; gotNull != purgedNull {
		t.Fatalf("archived accounts purged_at null = %t, want %t", gotNull, purgedNull)
	}
	if gotNull := account["closed_at"] == nil; gotNull != closedNull {
		t.Fatalf("archived accounts closed_at null = %t, want %t", gotNull, closedNull)
	}
}

func assertImportAuditAccountAbsent(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
) {
	t.Helper()
	var exists bool
	if err := st.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, accountID,
	).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("rejected archive left an account row behind")
	}
}

func assertImportedAuditAccountState(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
	purged bool,
) {
	t.Helper()
	var consentNull, purgedNull, closedNull bool
	if err := st.pool.QueryRow(ctx, `
		SELECT consent_terms_version IS NULL
		       AND consent_privacy_version IS NULL
		       AND consent_recorded_at IS NULL,
		       purged_at IS NULL,
		       closed_at IS NULL
		  FROM accounts
		 WHERE id=$1`, accountID,
	).Scan(&consentNull, &purgedNull, &closedNull); err != nil {
		t.Fatal(err)
	}
	if !consentNull || purgedNull == purged || closedNull {
		t.Fatalf(
			"imported account state = consent_null:%t purged_null:%t closed_null:%t",
			consentNull, purgedNull, closedNull,
		)
	}
}

// TestValidateImportedAccountAuditConsistencyTxPostgres pins the transaction
// backstop's contradiction branches directly: the integration fixtures above
// always trip the archive-scan check first (the replayed row comes verbatim
// from the archive the scan already compared), so without this test a refactor
// dropping the Tx re-check would leave the suite green.
func TestValidateImportedAccountAuditConsistencyTxPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("import-audit-txcheck-%d@witwave.ai", time.Now().UnixNano()),
		"import audit tx check",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = deleteAccountForIntegrationTest(
			context.Background(), st, provisioned.AccountID,
		)
	})

	check := func(t *testing.T, hasConsentEvent bool, purgedEvents int64, wantContradiction bool) {
		t.Helper()
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		err = validateImportedAccountAuditConsistencyTx(
			ctx, tx, provisioned.AccountID, hasConsentEvent, purgedEvents,
		)
		if got := errors.Is(err, ErrImportAuditContradiction); got != wantContradiction {
			t.Fatalf(
				"contradiction = %t, error = %v, want %t",
				got, err, wantContradiction,
			)
		}
	}

	// Freshly provisioned: consent and purge columns are both NULL.
	t.Run("consent event contradicts null column", func(t *testing.T) {
		check(t, true, 0, true)
	})
	t.Run("purge event contradicts null column", func(t *testing.T) {
		check(t, false, 1, true)
	})
	t.Run("no events pass null columns", func(t *testing.T) {
		check(t, false, 0, false)
	})

	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts
		   SET consent_terms_version='terms-txcheck',
		       consent_privacy_version='privacy-txcheck',
		       consent_recorded_at=statement_timestamp()
		 WHERE id=$1`, provisioned.AccountID); err != nil {
		t.Fatal(err)
	}
	t.Run("consent event passes recorded column", func(t *testing.T) {
		check(t, true, 0, false)
	})

	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts
		   SET status='closed',
		       closed_at=statement_timestamp(),
		       purged_at=statement_timestamp()
		 WHERE id=$1`, provisioned.AccountID); err != nil {
		t.Fatal(err)
	}
	t.Run("purge event passes purged column", func(t *testing.T) {
		check(t, false, 1, false)
	})
}

// TestImportAccountEvacuationRetryAuditCrossCheckPostgres pins the archive-scan
// check on its sole-enforcement path: an evacuation retry with
// disposition.AlreadyImported validates rows without replaying them and skips
// the Tx backstop by design, so only the unconditional archive-scan call
// refuses a contradictory retry archive. Moving that call under
// !AlreadyImported would fail exactly this test.
func TestImportAccountEvacuationRetryAuditCrossCheckPostgres(t *testing.T) {
	ctx, st := openAccountEvacuationTestStore(t)
	provisioned := provisionActiveEvacuationTestAccount(ctx, t, st, "audit-retry")
	const evacuationID = "evac_audit_retry_crosscheck"

	if _, err := st.BeginAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID, "audit cross-check retry test",
	); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccountEvacuation(
		ctx,
		provisioned.AccountID,
		evacuationID,
		"source-cell",
		"test",
		&archive,
	); err != nil {
		t.Fatal(err)
	}
	archiveBytes := bytes.Clone(archive.Bytes())
	if _, err := st.AbortAccountEvacuation(
		ctx, provisioned.AccountID, evacuationID,
	); err != nil {
		t.Fatal(err)
	}
	if err := deleteAccountForIntegrationTest(
		ctx, st, provisioned.AccountID,
	); err != nil {
		t.Fatal(err)
	}

	if _, _, err := st.ImportAccountEvacuation(
		ctx,
		provisioned.AccountID,
		evacuationID,
		bytes.NewReader(archiveBytes),
	); err != nil {
		t.Fatalf("first evacuation import: %v", err)
	}

	manifest, rows := readAvatarArchiveRows(t, archiveBytes, SchemaVersion())
	appendImportAuditEvent(
		t, rows, manifest, provisioned.AccountID, VerbAccountConsentRecorded,
	)
	mutated := writeAvatarArchiveRows(t, manifest, manifest.Tables, rows)

	_, _, err := st.ImportAccountEvacuation(
		ctx,
		provisioned.AccountID,
		evacuationID,
		bytes.NewReader(mutated),
	)
	if !errors.Is(err, ErrImportAuditContradiction) {
		t.Fatalf(
			"contradictory retry error = %v, want ErrImportAuditContradiction",
			err,
		)
	}
}
