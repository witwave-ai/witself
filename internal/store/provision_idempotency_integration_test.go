package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProvisionAccountExactCommittedReplayRotatesBootstrapPostgres(
	t *testing.T,
) {
	ctx, st := openAccountEvacuationTestStore(t)
	provisionID := fmt.Sprintf("prv_replay_%d", time.Now().UnixNano())
	var accountIDs []string
	registerProvisionCleanup(
		t, st, &accountIDs, []string{provisionID},
	)

	first, err := st.ProvisionAccountExact(
		ctx, provisionID, " Replay@Witwave.AI ", " Replay Account ",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	accountIDs = append(accountIDs, first.AccountID)
	if first.ProvisionID != provisionID || first.Replayed ||
		first.Email != "replay@witwave.ai" ||
		first.Status != "pending" ||
		first.BootstrapToken == "" {
		t.Fatalf("first provision = %#v", first)
	}

	replay, err := st.ProvisionAccountExact(
		ctx, provisionID, "replay@witwave.ai", "Replay Account",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.AccountID != first.AccountID ||
		replay.OperatorID != first.OperatorID ||
		replay.BootstrapToken == "" ||
		replay.BootstrapToken == first.BootstrapToken {
		t.Fatalf("replayed provision = %#v; first = %#v", replay, first)
	}

	if _, _, err := st.ExchangeBootstrap(
		ctx, first.BootstrapToken,
	); !errors.Is(err, ErrInvalidBootstrap) {
		t.Fatalf("superseded bootstrap exchange = %v, want %v",
			err, ErrInvalidBootstrap)
	}
	_, operatorID, err := st.ExchangeBootstrap(
		ctx, replay.BootstrapToken,
	)
	if err != nil {
		t.Fatal(err)
	}
	if operatorID != first.OperatorID {
		t.Fatalf("replayed bootstrap operator = %q, want %q",
			operatorID, first.OperatorID)
	}
	if _, err := st.ProvisionAccountExact(
		ctx, provisionID, "replay@witwave.ai", "Replay Account",
		time.Hour,
	); !errors.Is(err, ErrProvisionReplayUnsafe) {
		t.Fatalf("claimed receipt replay = %v, want %v",
			err, ErrProvisionReplayUnsafe)
	}

	var issueCount, provisionEvents int
	if err := st.pool.QueryRow(ctx, `
		SELECT issue_count
		  FROM account_provision_receipts
		 WHERE provision_id = $1`,
		provisionID,
	).Scan(&issueCount); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM account_events
		 WHERE account_id = $1
		   AND verb = $2`,
		first.AccountID, VerbAccountProvisioned,
	).Scan(&provisionEvents); err != nil {
		t.Fatal(err)
	}
	if issueCount != 2 || provisionEvents != 1 {
		t.Fatalf(
			"receipt issues/events = %d/%d, want 2/1",
			issueCount, provisionEvents,
		)
	}
}

func TestProvisionAccountExactConflictPrivacyAndDuplicateEmailPostgres(
	t *testing.T,
) {
	ctx, st := openAccountEvacuationTestStore(t)
	suffix := time.Now().UnixNano()
	firstID := fmt.Sprintf("prv_conflict_%d", suffix)
	secondID := fmt.Sprintf("prv_duplicate_email_%d", suffix)
	var accountIDs []string
	registerProvisionCleanup(
		t, st, &accountIDs, []string{firstID, secondID},
	)

	first, err := st.ProvisionAccountExact(
		ctx, firstID, "same@witwave.ai", "Same Account", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	accountIDs = append(accountIDs, first.AccountID)
	for _, request := range []struct {
		email, displayName string
	}{
		{email: "different@witwave.ai", displayName: "Same Account"},
		{email: "same@witwave.ai", displayName: "Different Account"},
	} {
		if _, err := st.ProvisionAccountExact(
			ctx, firstID, request.email, request.displayName, time.Hour,
		); !errors.Is(err, ErrProvisionRequestConflict) {
			t.Fatalf(
				"conflicting reuse %q/%q = %v, want %v",
				request.email, request.displayName, err,
				ErrProvisionRequestConflict,
			)
		}
	}
	second, err := st.ProvisionAccountExact(
		ctx, secondID, "same@witwave.ai", "Same Account", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	accountIDs = append(accountIDs, second.AccountID)
	if second.AccountID == first.AccountID {
		t.Fatal("distinct provision ids returned the same account")
	}
	var sameEmailAccounts int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM accounts
		 WHERE email = 'same@witwave.ai'`,
	).Scan(&sameEmailAccounts); err != nil {
		t.Fatal(err)
	}
	if sameEmailAccounts != 2 {
		t.Fatalf("duplicate-email accounts = %d, want 2",
			sameEmailAccounts)
	}

	var fingerprint string
	if err := st.pool.QueryRow(ctx, `
		SELECT request_fingerprint
		  FROM account_provision_receipts
		 WHERE provision_id = $1`,
		firstID,
	).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	decoded, err := hex.DecodeString(fingerprint)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("request fingerprint = %q / %v", fingerprint, err)
	}
	var piiColumnCount int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'account_provision_receipts'
		   AND column_name IN (
		       'email', 'display_name',
		       'normalized_email', 'normalized_display_name'
		   )`,
	).Scan(&piiColumnCount); err != nil {
		t.Fatal(err)
	}
	if piiColumnCount != 0 {
		t.Fatalf("provision receipt PII columns = %d, want 0",
			piiColumnCount)
	}
}

func TestProvisionAccountExactConcurrentSameIDPostgres(t *testing.T) {
	ctx, st := openAccountEvacuationTestStore(t)
	provisionID := fmt.Sprintf(
		"prv_concurrent_%d", time.Now().UnixNano(),
	)
	var accountIDs []string
	registerProvisionCleanup(
		t, st, &accountIDs, []string{provisionID},
	)

	type result struct {
		account ProvisionedAccount
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			account, err := st.ProvisionAccountExact(
				ctx, provisionID,
				"concurrent@witwave.ai", "Concurrent",
				time.Hour,
			)
			results <- result{account: account, err: err}
		}()
	}
	close(start)
	firstResult, secondResult := <-results, <-results
	for index, current := range []result{firstResult, secondResult} {
		if current.err != nil {
			t.Fatalf("concurrent call %d: %v", index+1, current.err)
		}
	}
	first, second := firstResult.account, secondResult.account
	accountIDs = append(accountIDs, first.AccountID)
	if first.AccountID != second.AccountID ||
		first.OperatorID != second.OperatorID {
		t.Fatalf("concurrent accounts = %#v / %#v", first, second)
	}
	if first.Replayed == second.Replayed {
		t.Fatalf(
			"concurrent replay flags = %t/%t, want one initial and one replay",
			first.Replayed, second.Replayed,
		)
	}
	initial, replay := first, second
	if initial.Replayed {
		initial, replay = replay, initial
	}
	if _, _, err := st.ExchangeBootstrap(
		ctx, initial.BootstrapToken,
	); !errors.Is(err, ErrInvalidBootstrap) {
		t.Fatalf("concurrent superseded bootstrap = %v, want %v",
			err, ErrInvalidBootstrap)
	}
	if _, operatorID, err := st.ExchangeBootstrap(
		ctx, replay.BootstrapToken,
	); err != nil || operatorID != replay.OperatorID {
		t.Fatalf("concurrent replay bootstrap = operator %q / %v",
			operatorID, err)
	}
	var accountCount, receiptCount int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE id = $1`,
		first.AccountID,
	).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM account_provision_receipts
		  WHERE provision_id = $1`,
		provisionID,
	).Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if accountCount != 1 || receiptCount != 1 {
		t.Fatalf("concurrent rows = account %d receipt %d, want 1/1",
			accountCount, receiptCount)
	}
}

func TestProvisionReceiptSurvivesFinalizationAndIsNotPortablePostgres(
	t *testing.T,
) {
	ctx, st := openAccountEvacuationTestStore(t)
	suffix := time.Now().UnixNano()
	provisionID := fmt.Sprintf("prv_finalize_%d", suffix)
	evacuationID := fmt.Sprintf("evac_provision_%d", suffix)
	var accountIDs []string
	registerProvisionCleanup(
		t, st, &accountIDs, []string{provisionID},
	)

	source, err := st.ProvisionAccountExact(
		ctx, provisionID,
		"finalize-provision@witwave.ai", "Finalize Provision",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	accountIDs = append(accountIDs, source.AccountID)
	if activated, err := st.ActivateAccount(
		ctx, source.AccountID,
	); err != nil || !activated {
		t.Fatalf("activate source = %t / %v", activated, err)
	}
	if _, err := st.ProvisionAccountExact(
		ctx, provisionID,
		"finalize-provision@witwave.ai", "Finalize Provision",
		time.Hour,
	); !errors.Is(err, ErrProvisionReplayUnsafe) {
		t.Fatalf("active provision replay = %v, want %v",
			err, ErrProvisionReplayUnsafe)
	}
	if _, err := st.BeginAccountEvacuation(
		ctx, source.AccountID, evacuationID, "provision receipt export",
	); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccountEvacuation(
		ctx, source.AccountID, evacuationID,
		"source-cell", "test", &archive,
	); err != nil {
		t.Fatal(err)
	}
	manifest, archivedRows := readAvatarArchiveRows(
		t, archive.Bytes(), SchemaVersion(),
	)
	for _, table := range manifest.Tables {
		if table == "account_provision_receipts" {
			t.Fatal("manifest contains cell-local provision receipts")
		}
	}
	if _, exists := archivedRows["account_provision_receipts"]; exists {
		t.Fatal("archive contains cell-local provision receipt rows")
	}

	if _, err := st.FinalizeAccountEvacuationSource(
		ctx, source.AccountID, evacuationID,
	); err != nil {
		t.Fatal(err)
	}
	var receiptCount, accountCount int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM account_provision_receipts
		 WHERE provision_id = $1`,
		provisionID,
	).Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE id = $1`,
		source.AccountID,
	).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if receiptCount != 1 || accountCount != 0 {
		t.Fatalf(
			"post-finalization rows = receipt %d account %d, want 1/0",
			receiptCount, accountCount,
		)
	}
	if _, err := st.ProvisionAccountExact(
		ctx, provisionID,
		"finalize-provision@witwave.ai", "Finalize Provision",
		time.Hour,
	); !errors.Is(err, ErrProvisionReplayUnsafe) {
		t.Fatalf("finalized provision replay = %v, want %v",
			err, ErrProvisionReplayUnsafe)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE id = $1`,
		source.AccountID,
	).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if accountCount != 0 {
		t.Fatalf("stale provision replay recreated %d accounts", accountCount)
	}

	if _, _, err := st.ImportAccountEvacuation(
		ctx, source.AccountID, evacuationID,
		bytes.NewReader(archive.Bytes()),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProvisionAccountExact(
		ctx, provisionID,
		"finalize-provision@witwave.ai", "Finalize Provision",
		time.Hour,
	); !errors.Is(err, ErrProvisionReplayUnsafe) {
		t.Fatalf("restored-target provision replay = %v, want %v",
			err, ErrProvisionReplayUnsafe)
	}
	if _, err := st.CompleteAccountEvacuation(
		ctx, source.AccountID, evacuationID,
	); err != nil {
		t.Fatal(err)
	}
}

func TestProvisionReceiptMigrationDowngradePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}

	t.Run("empty table can downgrade", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		migrationTestDownTo(t, dsn, 72)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 71)
		assertMigrationTestTable(
			t, st, "account_provision_receipts", false,
		)
	})

	t.Run("committed receipt refuses downgrade", func(t *testing.T) {
		ctx := context.Background()
		st, dsn := newMigrationTestStore(t, baseDSN)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		provisionID := "prv_downgrade_receipt"
		created, err := st.ProvisionAccountExact(
			ctx, provisionID,
			"downgrade-receipt@witwave.ai", "Downgrade Receipt",
			time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}

		migrationTestDownTo(t, dsn, 72)
		downErr := migrationTestDown(t, dsn, true)
		if downErr == nil || !strings.Contains(
			downErr.Error(), "provision receipts exist",
		) {
			t.Fatalf("downgrade error = %v", downErr)
		}
		assertMigrationTestVersion(t, dsn, 72)
		assertMigrationTestTable(
			t, st, "account_provision_receipts", true,
		)

		var receiptCount, accountCount int
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FROM account_provision_receipts
			 WHERE provision_id = $1`,
			provisionID,
		).Scan(&receiptCount); err != nil {
			t.Fatal(err)
		}
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FROM accounts WHERE id = $1`,
			created.AccountID,
		).Scan(&accountCount); err != nil {
			t.Fatal(err)
		}
		if receiptCount != 1 || accountCount != 1 {
			t.Fatalf(
				"rows after refused downgrade = receipt %d account %d",
				receiptCount, accountCount,
			)
		}

		replayed, err := st.ProvisionAccountExact(
			ctx, provisionID,
			"downgrade-receipt@witwave.ai", "Downgrade Receipt",
			time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !replayed.Replayed ||
			replayed.AccountID != created.AccountID {
			t.Fatalf(
				"post-refusal replay = %#v, first = %#v",
				replayed, created,
			)
		}
	})
}

func registerProvisionCleanup(
	t *testing.T,
	st *Store,
	accountIDs *[]string,
	provisionIDs []string,
) {
	t.Helper()
	t.Cleanup(func() {
		for _, accountID := range *accountIDs {
			cleanupAccountEvacuationTestAccount(
				context.Background(), t, st, accountID,
			)
		}
		for _, provisionID := range provisionIDs {
			if _, err := st.pool.Exec(
				context.Background(),
				`DELETE FROM account_provision_receipts
				  WHERE provision_id = $1`,
				provisionID,
			); err != nil {
				t.Errorf(
					"delete provision receipt %q: %v",
					provisionID, err,
				)
			}
		}
	})
}
