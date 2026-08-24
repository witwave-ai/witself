package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/placement"
	"github.com/witwave-ai/witself/internal/plans"
)

const (
	accountPurgeIntegrationMemoryContent     = "shared account-purge memory content"
	accountPurgeIntegrationFactValue         = `"shared account-purge fact value"`
	accountPurgeIntegrationTranscriptBody    = "shared account-purge transcript body"
	accountPurgeIntegrationSupportSubject    = "shared account-purge support subject"
	accountPurgeIntegrationSupportBody       = "shared account-purge support body"
	accountPurgeIntegrationTargetCloseReason = "private account-purge close reason"
)

func TestAccountPurgeErasesClosedPastGraceAndPreservesExcludedAccountsPostgres(
	t *testing.T,
) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	suffix := fmt.Sprint(time.Now().UnixNano())
	target := seedAccountPurgeIntegrationContent(ctx, t, st, "target", suffix)
	openDecoy := seedAccountPurgeIntegrationContent(ctx, t, st, "open", suffix)
	configureAccountPurgeIntegrationTombstoneSource(ctx, t, st, target)
	openReceiptFingerprint := strings.Repeat("b", 64)
	seedAccountPurgeIntegrationProvisionReceipt(
		ctx, t, st, openDecoy.account.AccountID, openReceiptFingerprint,
	)
	if err := st.CloseAccount(
		ctx,
		target.account.AccountID,
		target.account.OperatorID,
		accountPurgeIntegrationTargetCloseReason,
	); err != nil {
		t.Fatal(err)
	}

	grace := DefaultAccountPurgeWorkerConfig().Grace
	withinGraceRow := readAccountPurgeIntegrationAccountRow(
		ctx, t, st, target.account.AccountID,
	)
	withinGraceCounts := readAccountPurgeIntegrationCounts(
		ctx, t, st, target.account.AccountID,
	)
	withinGraceReceiptFingerprint := readAccountPurgeIntegrationReceiptFingerprint(
		ctx, t, st, target.account.AccountID,
	)
	previewWithinGrace, err := st.PreviewAccountPurgeBatch(ctx, 100, grace)
	if err != nil {
		t.Fatal(err)
	}
	assertAccountPurgeIntegrationNoopResult(t, previewWithinGrace, "within-grace preview")
	enforceWithinGrace, err := st.ProcessAccountPurgeBatch(ctx, 100, grace)
	if err != nil {
		t.Fatal(err)
	}
	assertAccountPurgeIntegrationNoopResult(t, enforceWithinGrace, "within-grace enforce")
	if got := readAccountPurgeIntegrationAccountRow(ctx, t, st, target.account.AccountID); !reflect.DeepEqual(got, withinGraceRow) {
		t.Fatalf("within-grace account mutated:\n got: %#v\nwant: %#v", got, withinGraceRow)
	}
	if got := readAccountPurgeIntegrationCounts(ctx, t, st, target.account.AccountID); !reflect.DeepEqual(got, withinGraceCounts) {
		t.Fatalf("within-grace content mutated:\n got: %#v\nwant: %#v", got, withinGraceCounts)
	}
	if got := readAccountPurgeIntegrationReceiptFingerprint(ctx, t, st, target.account.AccountID); got != withinGraceReceiptFingerprint {
		t.Fatalf("within-grace provision receipt fingerprint = %q, want %q", got, withinGraceReceiptFingerprint)
	}
	assertAccountPurgeIntegrationContent(ctx, t, st, target, true)

	// Give the active decoy an equally old closed_at. The exact preview result
	// below must therefore fail if the selection page loses status='closed', even
	// when the per-account revalidation still refuses to purge it.
	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts
		   SET closed_at=statement_timestamp() - interval '31 days'
		 WHERE id IN ($1,$2)`,
		target.account.AccountID,
		openDecoy.account.AccountID,
	); err != nil {
		t.Fatal(err)
	}

	defaultAccountID := "acc_purge_default_" + suffix
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO accounts(
		  id,is_default,display_name,status,closed_at,closed_reason
		) VALUES (
		  $1,TRUE,'account purge default decoy','closed',
		  statement_timestamp() - interval '31 days','default exclusion'
		)`, defaultAccountID); err != nil {
		t.Fatal(err)
	}

	fenced := provisionActiveAccountPurgeIntegrationAccount(
		ctx, t, st, "fenced", suffix,
	)
	if err := st.CloseAccount(
		ctx, fenced.AccountID, fenced.OperatorID, "evacuation exclusion",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts
		   SET closed_at=statement_timestamp() - interval '31 days'
		 WHERE id=$1`, fenced.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginAccountEvacuation(
		ctx,
		fenced.AccountID,
		"evac_purge_fenced_"+suffix,
		"purge selection exclusion",
	); err != nil {
		t.Fatal(err)
	}
	if err := st.LogEvent(ctx, EventInput{
		AccountID: target.account.AccountID,
		ActorKind: ActorSystem,
		Verb:      VerbAccountPurged,
		Metadata:  map[string]any{},
	}); !errors.Is(err, ErrBadEventMetadata) {
		t.Fatalf("pre-purge account.purged error = %v, want bad event metadata", err)
	}

	targetBefore := readAccountPurgeIntegrationAccountRow(
		ctx, t, st, target.account.AccountID,
	)
	openBefore := readAccountPurgeIntegrationAccountRow(
		ctx, t, st, openDecoy.account.AccountID,
	)
	defaultBefore := readAccountPurgeIntegrationAccountRow(
		ctx, t, st, defaultAccountID,
	)
	fencedBefore := readAccountPurgeIntegrationAccountRow(
		ctx, t, st, fenced.AccountID,
	)
	targetCountsBefore := readAccountPurgeIntegrationCounts(
		ctx, t, st, target.account.AccountID,
	)
	openCountsBefore := readAccountPurgeIntegrationCounts(
		ctx, t, st, openDecoy.account.AccountID,
	)
	openReceiptBefore := readAccountPurgeIntegrationReceiptFingerprint(
		ctx, t, st, openDecoy.account.AccountID,
	)
	for _, table := range []string{
		"memories", "facts", "transcript_conversations", "transcript_entries",
		"support_tickets", "support_ticket_messages",
	} {
		if targetCountsBefore[table] < 1 || openCountsBefore[table] < 1 {
			t.Fatalf(
				"required seeded table %s counts = target %d open %d",
				table, targetCountsBefore[table], openCountsBefore[table],
			)
		}
	}

	preview, err := st.PreviewAccountPurgeBatch(ctx, 100, grace)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Scanned != 1 || preview.SkippedLocked != 0 ||
		preview.Eligible != 1 || preview.PurgedAccounts != 0 ||
		preview.DeferredVaultLifecycle != 0 ||
		preview.AttachmentInvariantFailures != 0 ||
		preview.ProvisionReceiptScrubs != 1 {
		t.Fatalf("preview result = %+v, want one eligible and no mutation", preview)
	}
	if !reflect.DeepEqual(preview.DeletedByTable, targetCountsBefore) {
		t.Fatalf(
			"preview per-table counts:\n got: %#v\nwant: %#v",
			preview.DeletedByTable, targetCountsBefore,
		)
	}
	assertAccountPurgeIntegrationStateUnchanged(
		ctx, t, st, target.account.AccountID, targetBefore, targetCountsBefore,
		"preview target",
	)
	assertAccountPurgeIntegrationStateUnchanged(
		ctx, t, st, openDecoy.account.AccountID, openBefore, openCountsBefore,
		"preview open decoy",
	)
	assertAccountPurgeIntegrationContent(ctx, t, st, target, true)
	assertAccountPurgeIntegrationContent(ctx, t, st, openDecoy, true)
	if got := readAccountPurgeIntegrationReceiptFingerprint(ctx, t, st, target.account.AccountID); got != withinGraceReceiptFingerprint {
		t.Fatalf("preview provision receipt fingerprint = %q, want %q", got, withinGraceReceiptFingerprint)
	}
	if got := readAccountPurgeIntegrationReceiptFingerprint(ctx, t, st, openDecoy.account.AccountID); got != openReceiptBefore {
		t.Fatalf("preview scrubbed open-account receipt = %q, want %q", got, openReceiptBefore)
	}

	enforced, err := st.ProcessAccountPurgeBatch(ctx, 100, grace)
	if err != nil {
		t.Fatal(err)
	}
	if enforced.Scanned != 1 || enforced.SkippedLocked != 0 ||
		enforced.Eligible != 1 || enforced.PurgedAccounts != 1 ||
		enforced.DeferredVaultLifecycle != 0 ||
		enforced.AttachmentInvariantFailures != 0 ||
		enforced.ProvisionReceiptScrubs != 1 {
		t.Fatalf("enforce result = %+v, want one purged account", enforced)
	}
	if !reflect.DeepEqual(enforced.DeletedByTable, targetCountsBefore) {
		t.Fatalf(
			"enforce per-table counts:\n got: %#v\nwant: %#v",
			enforced.DeletedByTable, targetCountsBefore,
		)
	}

	targetAfter := readAccountPurgeIntegrationAccountRow(
		ctx, t, st, target.account.AccountID,
	)
	assertAccountPurgeIntegrationTombstone(t, targetBefore, targetAfter)
	assertAccountPurgeIntegrationContent(ctx, t, st, target, false)
	targetCountsAfter := readAccountPurgeIntegrationCounts(
		ctx, t, st, target.account.AccountID,
	)
	for table, count := range targetCountsAfter {
		want := int64(0)
		if table == "account_events" {
			want = 1
		}
		if count != want {
			t.Errorf("post-purge %s count = %d, want %d", table, count, want)
		}
	}
	assertAccountPurgeIntegrationAuditEvent(ctx, t, st, target.account.AccountID)
	assertAccountPurgeIntegrationRejectsPostPurgeMutations(
		ctx, t, st, target.account.AccountID, targetBefore, targetAfter,
	)
	if got := readAccountPurgeIntegrationReceiptFingerprint(ctx, t, st, target.account.AccountID); got != purgedProvisionRequestFingerprint {
		t.Fatalf("purged provision receipt fingerprint = %q, want scrub marker", got)
	}

	assertAccountPurgeIntegrationStateUnchanged(
		ctx, t, st, openDecoy.account.AccountID, openBefore, openCountsBefore,
		"enforce open decoy",
	)
	assertAccountPurgeIntegrationContent(ctx, t, st, openDecoy, true)
	if got := readAccountPurgeIntegrationReceiptFingerprint(ctx, t, st, openDecoy.account.AccountID); got != openReceiptBefore {
		t.Fatalf("enforce scrubbed open-account receipt = %q, want %q", got, openReceiptBefore)
	}
	if got := readAccountPurgeIntegrationAccountRow(ctx, t, st, defaultAccountID); !reflect.DeepEqual(got, defaultBefore) {
		t.Fatalf("default account was selected:\n got: %#v\nwant: %#v", got, defaultBefore)
	}
	if got := readAccountPurgeIntegrationAccountRow(ctx, t, st, fenced.AccountID); !reflect.DeepEqual(got, fencedBefore) {
		t.Fatalf("evacuation-fenced account was selected:\n got: %#v\nwant: %#v", got, fencedBefore)
	}

	retry, err := st.ProcessAccountPurgeBatch(ctx, 100, grace)
	if err != nil {
		t.Fatal(err)
	}
	assertAccountPurgeIntegrationNoopResult(t, retry, "post-purge retry")
	if got := readAccountPurgeIntegrationAccountRow(ctx, t, st, target.account.AccountID); !reflect.DeepEqual(got, targetAfter) {
		t.Fatalf("purge retry changed tombstone:\n got: %#v\nwant: %#v", got, targetAfter)
	}
	assertAccountPurgeIntegrationAuditEvent(ctx, t, st, target.account.AccountID)
}

func TestAccountPurgeDefersActiveVaultLifecyclePostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	suffix := fmt.Sprint(time.Now().UnixNano())
	account := provisionActiveAccountPurgeIntegrationAccount(
		ctx, t, st, "vault-deferred", suffix,
	)
	realm, err := st.CreateRealm(ctx, account.AccountID, "account-purge-vault")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(
		ctx, account.AccountID, realm.ID, "account-purge-vault",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CloseAccount(
		ctx, account.AccountID, account.OperatorID, "vault lifecycle deferral",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts
		   SET closed_at=statement_timestamp() - interval '31 days'
		 WHERE id=$1`, account.AccountID); err != nil {
		t.Fatal(err)
	}

	// Normal close refuses an active vault lifecycle. Seed an orphan pending
	// key after closure to exercise the purge's independent corruption/race
	// fence, which must leave the account available for a later sweep.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_vault_keys(
		       id,account_id,realm_id,owner_agent_id,key_version,
		       algorithm,fingerprint,lifecycle_state,row_version
		) VALUES (
		       'avk_pppppppppppppppp',$1,$2,$3,1,$4,
		       '9999999999999999999999999999999999999999999999999999999999999999',
		       'pending',1
		)`, account.AccountID, realm.ID, agent.ID, SecretAEADAlgorithm); err != nil {
		t.Fatal(err)
	}

	before := readAccountPurgeIntegrationAccountRow(ctx, t, st, account.AccountID)
	countsBefore := readAccountPurgeIntegrationCounts(ctx, t, st, account.AccountID)
	if countsBefore["agent_vault_keys"] != 1 {
		t.Fatalf("pending vault key count = %d, want 1", countsBefore["agent_vault_keys"])
	}
	grace := DefaultAccountPurgeWorkerConfig().Grace
	preview, err := st.PreviewAccountPurgeBatch(ctx, 100, grace)
	if err != nil {
		t.Fatal(err)
	}
	assertAccountPurgeIntegrationDeferredResult(t, preview, "vault preview")
	assertAccountPurgeIntegrationStateUnchanged(
		ctx, t, st, account.AccountID, before, countsBefore, "vault preview",
	)

	enforced, err := st.ProcessAccountPurgeBatch(ctx, 100, grace)
	if err != nil {
		t.Fatal(err)
	}
	assertAccountPurgeIntegrationDeferredResult(t, enforced, "vault enforce")
	assertAccountPurgeIntegrationStateUnchanged(
		ctx, t, st, account.AccountID, before, countsBefore, "vault enforce",
	)
	var purgeEvents int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		 WHERE account_id=$1 AND verb=$2`,
		account.AccountID, VerbAccountPurged,
	).Scan(&purgeEvents); err != nil {
		t.Fatal(err)
	}
	if purgeEvents != 0 {
		t.Fatalf("deferred account purge event count = %d, want 0", purgeEvents)
	}
}

func TestAccountPurgeSkipsAttachmentInvariantFailureAndContinuesPostgres(
	t *testing.T,
) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	suffix := fmt.Sprint(time.Now().UnixNano())
	broken := provisionActiveAccountPurgeIntegrationAccount(
		ctx, t, st, "attachment-broken", suffix,
	)
	healthy := provisionActiveAccountPurgeIntegrationAccount(
		ctx, t, st, "attachment-healthy", suffix,
	)
	brokenReceiptFingerprint := strings.Repeat("c", 64)
	healthyReceiptFingerprint := strings.Repeat("d", 64)
	seedAccountPurgeIntegrationProvisionReceipt(
		ctx, t, st, broken.AccountID, brokenReceiptFingerprint,
	)
	seedAccountPurgeIntegrationProvisionReceipt(
		ctx, t, st, healthy.AccountID, healthyReceiptFingerprint,
	)
	seedAccountPurgeIntegrationAccountedEmail(
		ctx, t, st, broken, suffix,
	)
	for _, account := range []ProvisionedAccount{broken, healthy} {
		if err := st.CloseAccount(
			ctx, account.AccountID, account.OperatorID, "attachment invariant test",
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts
		   SET closed_at=CASE id
		       WHEN $1 THEN statement_timestamp() - interval '32 days'
		       ELSE statement_timestamp() - interval '31 days'
		       END,
		       retained_agent_email_attachment_bytes=CASE id
		       WHEN $1 THEN 3
		       ELSE retained_agent_email_attachment_bytes
		       END
		 WHERE id IN ($1,$2)`,
		broken.AccountID,
		healthy.AccountID,
	); err != nil {
		t.Fatal(err)
	}

	brokenBefore := readAccountPurgeIntegrationAccountRow(
		ctx, t, st, broken.AccountID,
	)
	brokenCountsBefore := readAccountPurgeIntegrationCounts(
		ctx, t, st, broken.AccountID,
	)
	if brokenBefore.retainedAttachmentBytes != 3 ||
		brokenCountsBefore["agent_email_messages"] != 1 {
		t.Fatalf(
			"broken attachment fixture = counter %d messages %d, want 3/1",
			brokenBefore.retainedAttachmentBytes,
			brokenCountsBefore["agent_email_messages"],
		)
	}
	healthyBefore := readAccountPurgeIntegrationAccountRow(
		ctx, t, st, healthy.AccountID,
	)
	healthyCountsBefore := readAccountPurgeIntegrationCounts(
		ctx, t, st, healthy.AccountID,
	)

	result, err := st.ProcessAccountPurgeBatch(
		ctx, 100, DefaultAccountPurgeWorkerConfig().Grace,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 2 || result.SkippedLocked != 0 ||
		result.Eligible != 2 || result.PurgedAccounts != 1 ||
		result.DeferredVaultLifecycle != 0 ||
		result.AttachmentInvariantFailures != 1 ||
		result.ProvisionReceiptScrubs != 1 {
		t.Fatalf("attachment-invariant result = %+v", result)
	}
	if !reflect.DeepEqual(result.DeletedByTable, healthyCountsBefore) {
		t.Fatalf(
			"committed counts after invariant skip:\n got: %#v\nwant: %#v",
			result.DeletedByTable,
			healthyCountsBefore,
		)
	}
	assertAccountPurgeIntegrationStateUnchanged(
		ctx, t, st, broken.AccountID, brokenBefore, brokenCountsBefore,
		"attachment-invariant account",
	)
	var brokenPurgeEvents int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		 WHERE account_id=$1 AND verb=$2`,
		broken.AccountID,
		VerbAccountPurged,
	).Scan(&brokenPurgeEvents); err != nil {
		t.Fatal(err)
	}
	if brokenPurgeEvents != 0 {
		t.Fatalf("attachment-invariant purge events = %d, want 0", brokenPurgeEvents)
	}
	if got := readAccountPurgeIntegrationReceiptFingerprint(ctx, t, st, broken.AccountID); got != brokenReceiptFingerprint {
		t.Fatalf("attachment-invariant receipt fingerprint = %q, want %q", got, brokenReceiptFingerprint)
	}
	healthyAfter := readAccountPurgeIntegrationAccountRow(
		ctx, t, st, healthy.AccountID,
	)
	assertAccountPurgeIntegrationTombstone(t, healthyBefore, healthyAfter)
	assertAccountPurgeIntegrationAuditEvent(ctx, t, st, healthy.AccountID)
	if got := readAccountPurgeIntegrationReceiptFingerprint(ctx, t, st, healthy.AccountID); got != purgedProvisionRequestFingerprint {
		t.Fatalf("healthy purged receipt fingerprint = %q, want scrub marker", got)
	}
}

type accountPurgeIntegrationContent struct {
	account           ProvisionedAccount
	realmID           string
	agentID           string
	memoryID          string
	factAssertionID   string
	transcriptEntryID string
	supportMessageID  string
}

func seedAccountPurgeIntegrationContent(
	ctx context.Context,
	t *testing.T,
	st *Store,
	label, suffix string,
) accountPurgeIntegrationContent {
	t.Helper()
	account := provisionActiveAccountPurgeIntegrationAccount(
		ctx, t, st, label, suffix,
	)
	realm, err := st.CreateRealm(ctx, account.AccountID, "account-purge-realm")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(
		ctx, account.AccountID, realm.ID, "account-purge-agent",
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
	}
	memoryResult, err := st.CaptureMemory(ctx, principal, CaptureMemoryInput{
		Content: accountPurgeIntegrationMemoryContent,
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidencePending,
			ExternalLocator: "codex://account-purge/integration",
		}},
		IdempotencyKey: "account-purge-memory",
	})
	if err != nil {
		t.Fatal(err)
	}
	fact, err := st.SetFact(ctx, principal, SetFactInput{
		Predicate:      "account/purge_test",
		Value:          json.RawMessage(accountPurgeIntegrationFactValue),
		SourceKind:     FactSourceAgent,
		IdempotencyKey: "account-purge-fact",
	})
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := st.CreateTranscript(
		ctx, account.AccountID, realm.ID, agent.ID,
		CreateTranscriptInput{ExternalID: "account-purge-transcript"},
	)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := st.AppendTranscriptEntry(
		ctx, account.AccountID, realm.ID, agent.ID, transcript.ID,
		AppendTranscriptEntryInput{
			ExternalID: "account-purge-entry",
			Role:       TranscriptRoleUser,
			Body:       accountPurgeIntegrationTranscriptBody,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, supportMessage, err := st.OpenTicket(ctx, OpenTicketInput{
		AccountID: account.AccountID, OperatorID: account.OperatorID,
		Subject: accountPurgeIntegrationSupportSubject,
		Body:    accountPurgeIntegrationSupportBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	return accountPurgeIntegrationContent{
		account: account, realmID: realm.ID, agentID: agent.ID,
		memoryID: memoryResult.Memory.ID, factAssertionID: fact.ResolvedAssertionID,
		transcriptEntryID: entry.ID, supportMessageID: supportMessage.ID,
	}
}

func provisionActiveAccountPurgeIntegrationAccount(
	ctx context.Context,
	t *testing.T,
	st *Store,
	label, suffix string,
) ProvisionedAccount {
	t.Helper()
	account, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("account-purge-%s-%s@witwave.ai", label, suffix),
		fmt.Sprintf("account purge %s %s", label, suffix),
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate %s account = %t / %v", label, activated, err)
	}
	return account
}

func configureAccountPurgeIntegrationTombstoneSource(
	ctx context.Context,
	t *testing.T,
	st *Store,
	target accountPurgeIntegrationContent,
) {
	t.Helper()
	limits := map[string]int64{
		plans.RealmLimit:         3,
		plans.AgentPerRealmLimit: 4,
		plans.StoredMemoryLimit:  5,
		plans.StoredFactLimit:    5,
	}
	policies := map[string]int64{plans.TranscriptRetentionDaysPolicy: 90}
	features := []string{
		plans.MemoryFeature, plans.FactsFeature, plans.SupportFeature,
	}
	const planLabel = "account-purge-plan"
	hash, err := plans.SnapshotHash(planLabel, limits, policies, features)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(
		ctx, target.account.AccountID, 7, hash, planLabel,
		limits, policies, features,
	); err != nil {
		t.Fatal(err)
	}
	customPlacement := placement.Policy{
		PreferredClouds:   []string{"gcp"},
		PreferredRegions:  []string{"use1"},
		PreferredChannels: []string{"edge"},
		AllowedClouds:     []string{"gcp"},
		AllowedRegions:    []string{"use1"},
		AllowedChannels:   []string{"edge"},
		RebalanceOn:       []string{"region"},
	}
	if _, err := st.SetPlacementPolicy(
		ctx, target.account.AccountID, target.account.OperatorID, customPlacement,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts
		   SET suspended_at=statement_timestamp() - interval '60 days',
		       suspended_for='owner_request',
		       suspended_reason='private suspension reason',
		       last_evacuation_id='evac_prior_account_purge',
		       last_evacuation_completed_at=statement_timestamp() - interval '45 days',
		       last_evacuation_outcome='completed'
		 WHERE id=$1`, target.account.AccountID); err != nil {
		t.Fatal(err)
	}
	seedAccountPurgeIntegrationProvisionReceipt(
		ctx, t, st, target.account.AccountID, strings.Repeat("a", 64),
	)
}

func seedAccountPurgeIntegrationProvisionReceipt(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
	fingerprint string,
) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO account_provision_receipts(
		       provision_id,account_id,request_fingerprint,bootstrap_token_id
		) VALUES ($1,$2,$3,$4)`,
		"provision_purge_"+accountID,
		accountID,
		fingerprint,
		"tok_provision_purge_"+accountID,
	); err != nil {
		t.Fatal(err)
	}
}

func seedAccountPurgeIntegrationAccountedEmail(
	ctx context.Context,
	t *testing.T,
	st *Store,
	account ProvisionedAccount,
	suffix string,
) {
	t.Helper()
	realm, err := st.CreateRealm(ctx, account.AccountID, "account-purge-email")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(
		ctx, account.AccountID, realm.ID, "account-purge-email",
	)
	if err != nil {
		t.Fatal(err)
	}
	// The legacy pilot scope requires 5-10 enrolled agents; enroll four
	// same-account fillers alongside the owner so the fixture passes the
	// production validator instead of a relaxed test-only shape.
	enrolledAgents := map[string]bool{agent.ID: true}
	for i := 0; i < 4; i++ {
		filler, err := st.CreateAgent(
			ctx, account.AccountID, realm.ID,
			fmt.Sprintf("account-purge-email-filler-%d", i),
		)
		if err != nil {
			t.Fatal(err)
		}
		enrolledAgents[filler.ID] = true
	}
	scope := AgentEmailPilotScope{
		Enabled:  true,
		Domain:   "agent-mail.witwave.ai",
		Audience: "account-purge-integration",
		RealmIDs: map[string]bool{realm.ID: true},
		AgentIDs: enrolledAgents,
	}
	address, err := st.EnsureAgentEmailMailbox(
		ctx, scope, account.AccountID, realm.ID, agent.ID, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("payload")
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_email_messages(
		       id,account_id,realm_id,mailbox_id,owner_agent_id,address_id,
		       provider,envelope_sender,envelope_recipient,agent_segment,realm_label,
		       raw_mime,raw_size_bytes,raw_sha256,parse_state,attachment_count,
		       attachment_storage_bytes,retained_attachment_storage_bytes,
		       payload_retention_state,attachment_storage_accounted,
		       spf_result,dkim_result,dmarc_result,spam_verdict,
		       sender_verification_state,duplicate_group_sha256,received_at
		) VALUES (
		       $1,$2,$3,$4,$5,$6,'cloudflare_email_routing',
		       'sender@example.com',$7,$8,$9,$10,$11,$12,'parsed',1,
		       $11,$11,'retained',TRUE,'unknown','unknown','unknown','unknown',
		       'unverified',$13,clock_timestamp()
		)`,
		"emsg_pppppppppppppppp",
		account.AccountID,
		realm.ID,
		address.MailboxID,
		agent.ID,
		address.ID,
		address.Address,
		address.AgentSegment,
		address.RealmLabel,
		raw,
		len(raw),
		strings.Repeat("e", 64),
		strings.Repeat("f", 64),
	); err != nil {
		t.Fatalf("seed accounted agent email %s: %v", suffix, err)
	}
}

func readAccountPurgeIntegrationReceiptFingerprint(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
) string {
	t.Helper()
	var fingerprint string
	if err := st.pool.QueryRow(ctx, `
		SELECT request_fingerprint
		  FROM account_provision_receipts
		 WHERE account_id=$1`,
		accountID,
	).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

type accountPurgeIntegrationAccountRow struct {
	email                     *string
	isDefault                 bool
	displayName               string
	status                    string
	createdAt                 time.Time
	closedAt                  *time.Time
	closedReason              string
	suspendedAt               *time.Time
	suspendedFor              *string
	suspendedReason           *string
	supportPolicy             string
	plan                      string
	planLimits                map[string]int64
	planPolicies              map[string]int64
	planFeatures              []string
	planAppliedAt             *time.Time
	planSnapshotRevision      int64
	planSnapshotHash          string
	placementPolicy           placement.Policy
	evacuationID              *string
	evacuationStartedAt       *time.Time
	evacuationRole            *string
	lastEvacuationID          *string
	lastEvacuationCompletedAt *time.Time
	lastEvacuationOutcome     *string
	retainedAttachmentBytes   int64
	purgedAt                  *time.Time
}

func readAccountPurgeIntegrationAccountRow(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
) accountPurgeIntegrationAccountRow {
	t.Helper()
	var out accountPurgeIntegrationAccountRow
	var planLimits, planPolicies, planFeatures, placementPolicy []byte
	if err := st.pool.QueryRow(ctx, `
		SELECT email,is_default,display_name,status,created_at,
		       closed_at,closed_reason,suspended_at,suspended_for,suspended_reason,
		       support_policy,plan,plan_limits,plan_policies,plan_features,
		       plan_applied_at,plan_snapshot_revision,plan_snapshot_hash,
		       placement_policy,evacuation_id,evacuation_started_at,evacuation_role,
		       last_evacuation_id,last_evacuation_completed_at,last_evacuation_outcome,
		       retained_agent_email_attachment_bytes,purged_at
		  FROM accounts
		 WHERE id=$1`, accountID).Scan(
		&out.email, &out.isDefault, &out.displayName, &out.status, &out.createdAt,
		&out.closedAt, &out.closedReason, &out.suspendedAt, &out.suspendedFor,
		&out.suspendedReason, &out.supportPolicy, &out.plan, &planLimits,
		&planPolicies, &planFeatures, &out.planAppliedAt,
		&out.planSnapshotRevision, &out.planSnapshotHash, &placementPolicy,
		&out.evacuationID, &out.evacuationStartedAt, &out.evacuationRole,
		&out.lastEvacuationID, &out.lastEvacuationCompletedAt,
		&out.lastEvacuationOutcome, &out.retainedAttachmentBytes, &out.purgedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planLimits, &out.planLimits); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planPolicies, &out.planPolicies); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planFeatures, &out.planFeatures); err != nil {
		t.Fatal(err)
	}
	var err error
	out.placementPolicy, err = placement.FromJSON(placementPolicy)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readAccountPurgeIntegrationCounts(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
) map[string]int64 {
	t.Helper()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	schemaVersion, err := accountPurgeSchemaVersionTx(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	counts, err := countPortableAccountRowsTx(ctx, tx, accountID, schemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	localCounts, err := countCellLocalAccountRowsTx(ctx, tx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	mergeAccountPurgeTableCounts(counts, localCounts)
	return counts
}

func assertAccountPurgeIntegrationNoopResult(
	t *testing.T,
	got AccountPurgeBatchResult,
	label string,
) {
	t.Helper()
	if got.Scanned != 0 || got.SkippedLocked != 0 || got.Eligible != 0 ||
		got.PurgedAccounts != 0 || got.DeferredVaultLifecycle != 0 ||
		got.AttachmentInvariantFailures != 0 || got.ProvisionReceiptScrubs != 0 ||
		len(got.DeletedByTable) != 0 {
		t.Fatalf("%s result = %+v, want counted no-op", label, got)
	}
}

func assertAccountPurgeIntegrationDeferredResult(
	t *testing.T,
	got AccountPurgeBatchResult,
	label string,
) {
	t.Helper()
	if got.Scanned != 1 || got.SkippedLocked != 0 || got.Eligible != 1 ||
		got.PurgedAccounts != 0 || got.DeferredVaultLifecycle != 1 ||
		got.AttachmentInvariantFailures != 0 || got.ProvisionReceiptScrubs != 0 ||
		len(got.DeletedByTable) != 0 {
		t.Fatalf("%s result = %+v, want one vault-lifecycle deferral", label, got)
	}
}

func assertAccountPurgeIntegrationStateUnchanged(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
	wantRow accountPurgeIntegrationAccountRow,
	wantCounts map[string]int64,
	label string,
) {
	t.Helper()
	if got := readAccountPurgeIntegrationAccountRow(ctx, t, st, accountID); !reflect.DeepEqual(got, wantRow) {
		t.Fatalf("%s account changed:\n got: %#v\nwant: %#v", label, got, wantRow)
	}
	if got := readAccountPurgeIntegrationCounts(ctx, t, st, accountID); !reflect.DeepEqual(got, wantCounts) {
		t.Fatalf("%s rows changed:\n got: %#v\nwant: %#v", label, got, wantCounts)
	}
}

func assertAccountPurgeIntegrationTombstone(
	t *testing.T,
	before, after accountPurgeIntegrationAccountRow,
) {
	t.Helper()
	if after.email != nil || after.displayName != "" || after.closedReason != "" ||
		after.suspendedFor != nil || after.suspendedReason != nil {
		t.Errorf("content-bearing tombstone fields were retained: %#v", after)
	}
	if after.purgedAt == nil || after.closedAt == nil ||
		!after.purgedAt.After(*after.closedAt) {
		t.Errorf("tombstone purge/closure times = purged %v closed %v", after.purgedAt, after.closedAt)
	}
	if after.isDefault || after.status != "closed" ||
		!after.createdAt.Equal(before.createdAt) ||
		!equalAccountPurgeIntegrationTimes(after.closedAt, before.closedAt) ||
		!equalAccountPurgeIntegrationTimes(after.suspendedAt, before.suspendedAt) {
		t.Errorf("tombstone lifecycle fields changed: before %#v after %#v", before, after)
	}
	if after.supportPolicy != before.supportPolicy || after.plan != before.plan ||
		!equalAccountPurgeIntegrationTimes(after.planAppliedAt, before.planAppliedAt) ||
		after.planSnapshotRevision != before.planSnapshotRevision ||
		after.planSnapshotHash != before.planSnapshotHash {
		t.Errorf("value-free plan authority changed: before %#v after %#v", before, after)
	}
	if len(after.planLimits) != 0 || len(after.planPolicies) != 0 ||
		len(after.planFeatures) != 0 {
		t.Errorf("plan payload was not anonymized: %#v", after)
	}
	if !reflect.DeepEqual(after.placementPolicy, placement.DefaultPolicy()) {
		t.Errorf("placement policy = %#v, want default %#v", after.placementPolicy, placement.DefaultPolicy())
	}
	if after.evacuationID != nil || after.evacuationStartedAt != nil ||
		after.evacuationRole != nil ||
		!reflect.DeepEqual(after.lastEvacuationID, before.lastEvacuationID) ||
		!equalAccountPurgeIntegrationTimes(
			after.lastEvacuationCompletedAt, before.lastEvacuationCompletedAt,
		) || !reflect.DeepEqual(after.lastEvacuationOutcome, before.lastEvacuationOutcome) {
		t.Errorf("evacuation tombstone fields changed: before %#v after %#v", before, after)
	}
	if after.retainedAttachmentBytes != 0 {
		t.Errorf("retained attachment counter = %d, want 0", after.retainedAttachmentBytes)
	}
}

func equalAccountPurgeIntegrationTimes(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func assertAccountPurgeIntegrationAuditEvent(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
) {
	t.Helper()
	var total int64
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM account_events WHERE account_id=$1`,
		accountID,
	).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("surviving account event count = %d, want 1", total)
	}
	var actorKind string
	var actorID *string
	var metadata []byte
	if err := st.pool.QueryRow(ctx, `
		SELECT actor_kind,actor_id,metadata
		  FROM account_events
		 WHERE account_id=$1 AND verb=$2`,
		accountID, VerbAccountPurged,
	).Scan(&actorKind, &actorID, &metadata); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(metadata, &decoded); err != nil {
		t.Fatal(err)
	}
	if actorKind != ActorSystem || actorID != nil || len(decoded) != 0 {
		t.Fatalf(
			"account.purged event = actor %q id %v metadata %s",
			actorKind, actorID, metadata,
		)
	}
}

func assertAccountPurgeIntegrationRejectsPostPurgeMutations(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
	before, tombstone accountPurgeIntegrationAccountRow,
) {
	t.Helper()
	for _, revision := range []int64{
		before.planSnapshotRevision,
		before.planSnapshotRevision + 1,
	} {
		if _, err := st.SetAccountPlan(
			ctx,
			accountID,
			revision,
			before.planSnapshotHash,
			before.plan,
			before.planLimits,
			before.planPolicies,
			before.planFeatures,
		); !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf(
				"post-purge plan revision %d error = %v, want account not found",
				revision,
				err,
			)
		}
	}

	nextSupportPolicy := SupportPolicyDisabled
	if before.supportPolicy == SupportPolicyDisabled {
		nextSupportPolicy = SupportPolicyEnabled
	}
	if _, err := st.SetSupportPolicyAdmin(ctx, SetSupportPolicyAdminInput{
		AccountID:   accountID,
		AdminHandle: "purge_guard",
		NewPolicy:   nextSupportPolicy,
	}); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("post-purge support-policy error = %v, want account not found", err)
	}

	if _, err := st.SetPlacementPolicySystem(ctx, accountID, placement.Policy{
		PreferredClouds: []string{"gcp"},
		AllowedClouds:   []string{"gcp"},
	}); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("post-purge placement error = %v, want account not found", err)
	}

	if err := st.LogEvent(ctx, EventInput{
		AccountID: accountID,
		ActorKind: ActorControlPlane,
		Verb:      VerbAccountEmailRecoverySent,
		Metadata:  map[string]any{"to_masked": "p***@e***.test"},
	}); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("post-purge event error = %v, want account not found", err)
	}
	if err := st.LogEvent(ctx, EventInput{
		AccountID: accountID,
		ActorKind: ActorSystem,
		Verb:      VerbAccountPurged,
		Metadata:  map[string]any{},
	}); !errors.Is(err, ErrBadEventMetadata) {
		t.Fatalf("duplicate account.purged error = %v, want bad event metadata", err)
	}

	if got := readAccountPurgeIntegrationAccountRow(
		ctx, t, st, accountID,
	); !reflect.DeepEqual(got, tombstone) {
		t.Fatalf(
			"post-purge mutation changed tombstone:\n got: %#v\nwant: %#v",
			got,
			tombstone,
		)
	}
	assertAccountPurgeIntegrationAuditEvent(ctx, t, st, accountID)
}

func assertAccountPurgeIntegrationContent(
	ctx context.Context,
	t *testing.T,
	st *Store,
	content accountPurgeIntegrationContent,
	wantPresent bool,
) {
	t.Helper()
	assertAccountPurgeIntegrationTextRow(ctx, t, st, `
		SELECT version.content
		  FROM memory_versions version
		 WHERE version.account_id=$1 AND version.memory_id=$2 AND version.version=1`,
		content.account.AccountID, content.memoryID,
		accountPurgeIntegrationMemoryContent, wantPresent, "memory",
	)
	assertAccountPurgeIntegrationTextRow(ctx, t, st, `
		SELECT assertion.value::text
		  FROM fact_assertions assertion
		 WHERE assertion.account_id=$1 AND assertion.id=$2`,
		content.account.AccountID, content.factAssertionID,
		accountPurgeIntegrationFactValue, wantPresent, "fact",
	)
	assertAccountPurgeIntegrationTextRow(ctx, t, st, `
		SELECT entry.body
		  FROM transcript_entries entry
		 WHERE entry.account_id=$1 AND entry.id=$2`,
		content.account.AccountID, content.transcriptEntryID,
		accountPurgeIntegrationTranscriptBody, wantPresent, "transcript",
	)
	assertAccountPurgeIntegrationTextRow(ctx, t, st, `
		SELECT message.body
		  FROM support_ticket_messages message
		 WHERE message.account_id=$1 AND message.id=$2`,
		content.account.AccountID, content.supportMessageID,
		accountPurgeIntegrationSupportBody, wantPresent, "support",
	)
}

func assertAccountPurgeIntegrationTextRow(
	ctx context.Context,
	t *testing.T,
	st *Store,
	query, accountID, rowID, want string,
	wantPresent bool,
	label string,
) {
	t.Helper()
	var got string
	err := st.pool.QueryRow(ctx, query, accountID, rowID).Scan(&got)
	if !wantPresent {
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("purged %s row = %q / %v, want absent", label, got, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("read surviving %s row: %v", label, err)
	}
	if got != want {
		t.Fatalf("surviving %s content = %q, want %q", label, got, want)
	}
}
