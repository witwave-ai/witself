package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAccountConsentMigrationDowngradePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}

	t.Run("empty downgrade and re-upgrade", func(t *testing.T) {
		ctx := context.Background()
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 94)

		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 93)

		migrationTestUpTo(t, dsn, 94)
		assertMigrationTestVersion(t, dsn, 94)
		created, err := st.ProvisionAccountExact(
			ctx, "prv_consent_reupgrade", "reupgrade@witwave.ai",
			"Consent Re-upgrade", "terms-2026.08", "privacy-2026.08",
			time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}
		account, err := st.GetAccount(ctx, created.AccountID)
		if err != nil {
			t.Fatal(err)
		}
		if account.ConsentTermsVersion == nil ||
			*account.ConsentTermsVersion != "terms-2026.08" ||
			account.ConsentPrivacyVersion == nil ||
			*account.ConsentPrivacyVersion != "privacy-2026.08" ||
			account.ConsentRecordedAt == nil {
			t.Fatalf("consent after re-upgrade = %#v", account)
		}
	})

	t.Run("recorded consent refuses downgrade", func(t *testing.T) {
		ctx := context.Background()
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 94)
		created, err := st.ProvisionAccountExact(
			ctx, "prv_consent_downgrade_guard", "guard@witwave.ai",
			"Consent Guard", "terms-2026.08", "privacy-2026.08",
			time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}

		downErr := migrationTestDown(t, dsn, true)
		if downErr == nil || !strings.Contains(
			downErr.Error(),
			"cannot downgrade schema 0094 while recorded account consent exists",
		) {
			t.Fatalf("schema-94 recorded-consent downgrade error = %v", downErr)
		}
		assertMigrationTestVersion(t, dsn, 94)
		account, err := st.GetAccount(ctx, created.AccountID)
		if err != nil {
			t.Fatal(err)
		}
		if account.ConsentTermsVersion == nil ||
			*account.ConsentTermsVersion != "terms-2026.08" ||
			account.ConsentPrivacyVersion == nil ||
			*account.ConsentPrivacyVersion != "privacy-2026.08" ||
			account.ConsentRecordedAt == nil {
			t.Fatalf("consent after refused downgrade = %#v", account)
		}
		var consentEvents int
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*)
			  FROM account_events
			 WHERE account_id = $1
			   AND verb = $2`,
			created.AccountID, VerbAccountConsentRecorded,
		).Scan(&consentEvents); err != nil {
			t.Fatal(err)
		}
		if consentEvents != 1 {
			t.Fatalf("consent events after refused downgrade = %d, want 1", consentEvents)
		}
	})
}
