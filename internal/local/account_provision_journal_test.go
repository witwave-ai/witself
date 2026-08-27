package local

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAccountProvisionJournalRejectsInvalidAcceptedVersions(t *testing.T) {
	tests := []struct {
		name    string
		terms   string
		privacy string
	}{
		{name: "one sided", terms: "terms-2026.08"},
		{name: "email shaped", terms: "owner@example.com", privacy: "privacy-2026.08"},
		{name: "over length", terms: strings.Repeat("a", 65), privacy: "privacy-2026.08"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("WITSELF_HOME", privateAccountProvisionTestHome(t))
			if _, _, err := BeginAccountProvisionJournal(
				"default", testAccountProvisionFingerprint,
			); err != nil {
				t.Fatal(err)
			}
			path, err := AccountProvisionJournalPath("default")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			record["accepted_terms_version"] = test.terms
			if test.privacy != "" {
				record["accepted_privacy_version"] = test.privacy
			}
			raw, err = json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadAccountProvisionJournal("default"); !errors.Is(err, ErrAccountProvisionJournalInvalid) {
				t.Fatalf("read invalid accepted versions = %v", err)
			}
		})
	}
}

const (
	testAccountProvisionFingerprint      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testOtherAccountProvisionFingerprint = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	testProvisionAccountID               = "acc_abcdefghijklmnop"
	testProvisionOperatorToken           = "witself_opr_accountProvisionRecovery"
)

func TestAccountProvisionJournalLifecycle(t *testing.T) {
	home := privateAccountProvisionTestHome(t)
	t.Setenv("WITSELF_HOME", home)

	first, created, err := BeginAccountProvisionJournal(
		"default", testAccountProvisionFingerprint,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !created || !strings.HasPrefix(first.ProvisionID, "prv_") {
		t.Fatalf("first journal = %+v, created = %v", first, created)
	}
	second, created, err := BeginAccountProvisionJournal(
		"default", testAccountProvisionFingerprint,
	)
	if err != nil {
		t.Fatal(err)
	}
	if created || second.ProvisionID != first.ProvisionID {
		t.Fatalf("second journal = %+v, created = %v; first = %+v", second, created, first)
	}
	if _, _, err := BeginAccountProvisionJournal(
		"default", testOtherAccountProvisionFingerprint,
	); !errors.Is(err, ErrAccountProvisionJournalConflict) {
		t.Fatalf("conflicting begin = %v", err)
	}

	path, err := AccountProvisionJournalPath("default")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("journal mode = %v", info.Mode())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"owner@example.com", "invite-private", "Display Name",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("journal contains private request value %q", forbidden)
		}
	}
	if strings.Contains(string(raw), "accepted_terms_version") ||
		strings.Contains(string(raw), "accepted_privacy_version") {
		t.Fatalf("legacy consentless journal gained optional fields: %s", raw)
	}

	if err := SaveAccountProvisionCredential(
		"default", testAccountProvisionFingerprint, first.ProvisionID,
		testProvisionAccountID, testProvisionOperatorToken,
	); err != nil {
		t.Fatal(err)
	}
	recovered, err := ReadAccountProvisionJournal("default")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.AccountID != testProvisionAccountID ||
		recovered.OperatorToken != testProvisionOperatorToken ||
		recovered.ProvisionID != first.ProvisionID {
		t.Fatalf("recovered journal = %+v", recovered)
	}
	if err := SaveAccountProvisionCredential(
		"default", testAccountProvisionFingerprint, first.ProvisionID,
		testProvisionAccountID, testProvisionOperatorToken,
	); err != nil {
		t.Fatalf("idempotent credential save: %v", err)
	}
	if err := SaveAccountProvisionCredential(
		"default", testAccountProvisionFingerprint, first.ProvisionID,
		"acc_ponmlkjihgfedcba", testProvisionOperatorToken,
	); !errors.Is(err, ErrAccountProvisionJournalConflict) {
		t.Fatalf("conflicting credential save = %v", err)
	}

	if err := DeleteAccountProvisionJournal(
		"default", testAccountProvisionFingerprint, first.ProvisionID,
		testProvisionAccountID, testProvisionOperatorToken,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAccountProvisionJournal("default"); !errors.Is(
		err, ErrAccountProvisionJournalUnavailable,
	) {
		t.Fatalf("read deleted journal = %v", err)
	}
}

func TestAccountProvisionJournalPersistsAcceptedLegalVersions(t *testing.T) {
	t.Setenv("WITSELF_HOME", privateAccountProvisionTestHome(t))

	first, created, err := BeginAccountProvisionJournalWithConsent(
		"default", testAccountProvisionFingerprint,
		"terms-2026.08", "privacy-2026.08",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !created || first.AcceptedTermsVersion != "terms-2026.08" ||
		first.AcceptedPrivacyVersion != "privacy-2026.08" {
		t.Fatalf("created consent journal = %+v, created = %v", first, created)
	}

	recovered, err := ReadAccountProvisionJournal("default")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.AcceptedTermsVersion != first.AcceptedTermsVersion ||
		recovered.AcceptedPrivacyVersion != first.AcceptedPrivacyVersion {
		t.Fatalf("recovered consent journal = %+v; first = %+v", recovered, first)
	}

	resumed, created, err := BeginAccountProvisionJournalWithConsent(
		"default", testAccountProvisionFingerprint,
		first.AcceptedTermsVersion, first.AcceptedPrivacyVersion,
	)
	if err != nil {
		t.Fatal(err)
	}
	if created || resumed.ProvisionID != first.ProvisionID {
		t.Fatalf("resumed consent journal = %+v, created = %v", resumed, created)
	}
}

func TestAccountProvisionJournalBackfillsLegacyConsentVersions(t *testing.T) {
	t.Setenv("WITSELF_HOME", privateAccountProvisionTestHome(t))

	legacy, created, err := BeginAccountProvisionJournal(
		"default", testAccountProvisionFingerprint,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !created || legacy.AcceptedTermsVersion != "" ||
		legacy.AcceptedPrivacyVersion != "" {
		t.Fatalf("legacy journal = %+v, created = %v", legacy, created)
	}

	backfilled, created, err := BeginAccountProvisionJournalWithConsent(
		"default", testAccountProvisionFingerprint,
		"terms-2026.08", "privacy-2026.08",
	)
	if err != nil {
		t.Fatal(err)
	}
	if created || backfilled.ProvisionID != legacy.ProvisionID ||
		backfilled.AcceptedTermsVersion != "terms-2026.08" ||
		backfilled.AcceptedPrivacyVersion != "privacy-2026.08" {
		t.Fatalf("backfilled journal = %+v, created = %v", backfilled, created)
	}
	recovered, err := ReadAccountProvisionJournal("default")
	if err != nil {
		t.Fatal(err)
	}
	if recovered != backfilled {
		t.Fatalf("recovered backfill = %+v; want %+v", recovered, backfilled)
	}

	if _, _, err := BeginAccountProvisionJournal(
		"default", testAccountProvisionFingerprint,
	); !errors.Is(err, ErrAccountProvisionJournalConflict) {
		t.Fatalf("consentless reuse after backfill = %v", err)
	}
	if _, _, err := BeginAccountProvisionJournalWithConsent(
		"default", testAccountProvisionFingerprint,
		"terms-2026.09", "privacy-2026.09",
	); !errors.Is(err, ErrAccountProvisionJournalConflict) {
		t.Fatalf("mismatched consent reuse after backfill = %v", err)
	}
}

func TestAccountProvisionJournalCreatesMissingPrivateHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "missing", ".witself")
	t.Setenv("WITSELF_HOME", home)
	if _, _, err := BeginAccountProvisionJournal(
		"default", testAccountProvisionFingerprint,
	); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		home,
		filepath.Join(home, "journal"),
		filepath.Join(home, "journal", "account-provision"),
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v", path, info.Mode())
		}
	}
}

func TestAccountProvisionJournalConcurrentBeginUsesOneID(t *testing.T) {
	t.Setenv("WITSELF_HOME", privateAccountProvisionTestHome(t))
	const workers = 24
	var wait sync.WaitGroup
	wait.Add(workers)
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	for range workers {
		go func() {
			defer wait.Done()
			record, _, err := BeginAccountProvisionJournal(
				"default", testAccountProvisionFingerprint,
			)
			if err != nil {
				errs <- err
				return
			}
			ids <- record.ProvisionID
		}()
	}
	wait.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent begin: %v", err)
	}
	unique := map[string]bool{}
	for provisionID := range ids {
		unique[provisionID] = true
	}
	if len(unique) != 1 {
		t.Fatalf("concurrent provision ids = %#v", unique)
	}
}

func TestSaveProvisionedAccountDurableResumesPartialCommit(t *testing.T) {
	home := privateAccountProvisionTestHome(t)
	t.Setenv("WITSELF_HOME", home)
	record, _, err := BeginAccountProvisionJournal(
		"default", testAccountProvisionFingerprint,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveAccountProvisionCredential(
		"default", testAccountProvisionFingerprint, record.ProvisionID,
		testProvisionAccountID, testProvisionOperatorToken,
	); err != nil {
		t.Fatal(err)
	}
	account := Account{ID: testProvisionAccountID, Email: "owner@example.com"}
	if err := SaveProvisionedAccountDurable(
		"default", account, testProvisionOperatorToken,
	); err != nil {
		t.Fatal(err)
	}
	_, resolved, resolvedToken, err := Resolve("default")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != account || resolvedToken != testProvisionOperatorToken {
		t.Fatalf("resolved = %+v, token = %q", resolved, resolvedToken)
	}
	for _, path := range []string{
		filepath.Join(home, "config.json"),
		filepath.Join(home, "tokens", "accounts", "default", "owner.token"),
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v", path, info.Mode())
		}
	}
	if _, err := ReadAccountProvisionJournal("default"); err != nil {
		t.Fatalf("durable save removed journal early: %v", err)
	}

	// Model a crash after the token publication but before config publication.
	if err := os.Remove(filepath.Join(home, "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := SaveProvisionedAccountDurable(
		"default", account, testProvisionOperatorToken,
	); err != nil {
		t.Fatalf("resume token-only partial commit: %v", err)
	}
	_, resolved, resolvedToken, err = Resolve("default")
	if err != nil || resolved != account || resolvedToken != testProvisionOperatorToken {
		t.Fatalf("resolved after resume = %+v, %q, %v", resolved, resolvedToken, err)
	}

	if err := DeleteAccountProvisionJournal(
		"default", testAccountProvisionFingerprint, record.ProvisionID,
		testProvisionAccountID, testProvisionOperatorToken,
	); err != nil {
		t.Fatal(err)
	}
}

func TestAccountProvisionJournalRejectsUnsafeFiles(t *testing.T) {
	t.Run("journal permissions", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", privateAccountProvisionTestHome(t))
		if _, _, err := BeginAccountProvisionJournal(
			"default", testAccountProvisionFingerprint,
		); err != nil {
			t.Fatal(err)
		}
		path, _ := AccountProvisionJournalPath("default")
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAccountProvisionJournal("default"); !errors.Is(
			err, ErrAccountProvisionJournalUnsafe,
		) {
			t.Fatalf("read non-private journal = %v", err)
		}
	})

	t.Run("journal symlink", func(t *testing.T) {
		home := privateAccountProvisionTestHome(t)
		t.Setenv("WITSELF_HOME", home)
		if _, _, err := BeginAccountProvisionJournal(
			"default", testAccountProvisionFingerprint,
		); err != nil {
			t.Fatal(err)
		}
		path, _ := AccountProvisionJournalPath("default")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(home, "target")
		if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, _, err := BeginAccountProvisionJournal(
			"default", testAccountProvisionFingerprint,
		); !errors.Is(err, ErrAccountProvisionJournalUnsafe) {
			t.Fatalf("begin through symlink = %v", err)
		}
		raw, err := os.ReadFile(target)
		if err != nil || string(raw) != "unchanged" {
			t.Fatalf("symlink target = %q, %v", raw, err)
		}
	})

	t.Run("lock permissions", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", privateAccountProvisionTestHome(t))
		if _, _, err := BeginAccountProvisionJournal(
			"default", testAccountProvisionFingerprint,
		); err != nil {
			t.Fatal(err)
		}
		path, _ := AccountProvisionJournalPath("default")
		lockPath := filepath.Join(filepath.Dir(path), accountProvisionJournalLockFile)
		if err := os.Chmod(lockPath, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAccountProvisionJournal("default"); !errors.Is(
			err, ErrAccountProvisionJournalUnsafe,
		) {
			t.Fatalf("read with non-private lock = %v", err)
		}
	})

	t.Run("trailing json", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", privateAccountProvisionTestHome(t))
		if _, _, err := BeginAccountProvisionJournal(
			"default", testAccountProvisionFingerprint,
		); err != nil {
			t.Fatal(err)
		}
		path, _ := AccountProvisionJournalPath("default")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, []byte("{}\n")...)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAccountProvisionJournal("default"); !errors.Is(
			err, ErrAccountProvisionJournalInvalid,
		) {
			t.Fatalf("read trailing JSON = %v", err)
		}
	})

	t.Run("conflicting token", func(t *testing.T) {
		home := privateAccountProvisionTestHome(t)
		t.Setenv("WITSELF_HOME", home)
		record, _, err := BeginAccountProvisionJournal(
			"default", testAccountProvisionFingerprint,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := SaveAccountProvisionCredential(
			"default", testAccountProvisionFingerprint, record.ProvisionID,
			testProvisionAccountID, testProvisionOperatorToken,
		); err != nil {
			t.Fatal(err)
		}
		tokenPath, _ := TokenPath("default")
		if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tokenPath, []byte("witself_opr_different\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := SaveProvisionedAccountDurable(
			"default", Account{ID: testProvisionAccountID},
			testProvisionOperatorToken,
		); !errors.Is(err, ErrNameTaken) {
			t.Fatalf("save over conflicting token = %v", err)
		}
	})
}

func privateAccountProvisionTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}
