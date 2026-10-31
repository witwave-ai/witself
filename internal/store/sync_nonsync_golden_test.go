package store

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

// This test intentionally uses only the core migration and pre-sync movement
// APIs. Running it with HEAD's finalize_evacuation.go/account_purge.go overlaid
// must produce the same literal golden as the candidate. Clock values are
// projected to presence and exact retry equality; generated IDs are excluded.
func TestSyncUnselectedCoreFinalizationAndPurgeGoldenPostgres(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncSchemaTestStore(t)
	const finalID = "acc_core_golden_finalize"
	const purgeID = "acc_core_golden_purge"
	const decoyID = "acc_core_golden_decoy"
	const evacuationID = "evac_core_golden"
	for _, accountID := range []string{finalID, purgeID, decoyID} {
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO accounts(id,display_name,email,is_default)
			VALUES($1,'core golden','golden@example.invalid',false)`, accountID); err != nil {
			t.Fatal("seed core golden account failed")
		}
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO realms(id,account_id,name) VALUES($1,$2,'core golden')`,
			"rlm_"+accountID, accountID); err != nil {
			t.Fatal("seed core golden realm failed")
		}
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts SET status='closed',closed_at='2000-01-01'::timestamptz,
		closed_reason='core golden close' WHERE id=$1`, purgeID); err != nil {
		t.Fatal("close core golden purge account failed")
	}
	if _, err := st.BeginAccountEvacuation(ctx, finalID, evacuationID, "core golden move"); err != nil {
		t.Fatal("begin core golden evacuation failed")
	}
	first, err := st.FinalizeAccountEvacuationSource(ctx, finalID, evacuationID)
	if err != nil {
		t.Fatal("core golden finalization failed")
	}
	retry, err := st.FinalizeAccountEvacuationSource(ctx, finalID, evacuationID)
	if err != nil {
		t.Fatal("core golden finalization retry failed")
	}
	var finalAccounts, finalRealms, finalEvents, finalReceipts int64
	if err := st.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM accounts WHERE id=$1),
		(SELECT count(*) FROM realms WHERE account_id=$1),
		(SELECT count(*) FROM account_events WHERE account_id=$1),
		(SELECT count(*) FROM account_evacuation_finalizations WHERE account_id=$1 AND evacuation_id=$2)`,
		finalID, evacuationID,
	).Scan(&finalAccounts, &finalRealms, &finalEvents, &finalReceipts); err != nil {
		t.Fatal("read core golden finalization state failed")
	}
	grace := 30 * 24 * time.Hour
	preview, err := st.processAccountPurgeCandidate(ctx, purgeID, grace, false)
	if err != nil {
		t.Fatal("core golden purge preview failed")
	}
	var previewAccounts, previewRealms int64
	var previewUnchanged bool
	if err := st.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM accounts WHERE id=$1),
		(SELECT count(*) FROM realms WHERE account_id=$1),
		EXISTS(SELECT 1 FROM accounts WHERE id=$1 AND display_name='core golden'
		AND email='golden@example.invalid' AND closed_reason='core golden close' AND purged_at IS NULL)`,
		purgeID,
	).Scan(&previewAccounts, &previewRealms, &previewUnchanged); err != nil {
		t.Fatal("read core golden preview state failed")
	}
	enforced, err := st.processAccountPurgeCandidate(ctx, purgeID, grace, true)
	if err != nil {
		t.Fatal("core golden purge failed")
	}
	purgeRetry, err := st.processAccountPurgeCandidate(ctx, purgeID, grace, true)
	if err != nil {
		t.Fatal("core golden purge retry failed")
	}
	var purgeStatus, purgeDisplayName, purgeClosedReason string
	var purgeEmailNull, purgeTimestampPresent bool
	if err := st.pool.QueryRow(ctx, `
		SELECT status,display_name,closed_reason,email IS NULL,purged_at IS NOT NULL
		FROM accounts WHERE id=$1`, purgeID,
	).Scan(&purgeStatus, &purgeDisplayName, &purgeClosedReason, &purgeEmailNull, &purgeTimestampPresent); err != nil {
		t.Fatal("read core golden tombstone failed")
	}
	var purgeAccounts, purgeRealms, purgeEvents, purgeErasureEvents, decoyAccounts, decoyRealms int64
	var decoyUnchanged bool
	if err := st.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM accounts WHERE id=$1),
		(SELECT count(*) FROM realms WHERE account_id=$1),
		(SELECT count(*) FROM account_events WHERE account_id=$1),
		(SELECT count(*) FROM account_events WHERE account_id=$1 AND verb='account.purged'),
		(SELECT count(*) FROM accounts WHERE id=$2),
		(SELECT count(*) FROM realms WHERE account_id=$2),
		EXISTS(SELECT 1 FROM accounts WHERE id=$2 AND status='active' AND display_name='core golden'
		AND email='golden@example.invalid' AND purged_at IS NULL)`,
		purgeID, decoyID,
	).Scan(&purgeAccounts, &purgeRealms, &purgeEvents, &purgeErasureEvents,
		&decoyAccounts, &decoyRealms, &decoyUnchanged); err != nil {
		t.Fatal("read core golden purge/decoy state failed")
	}
	version, err := st.AppliedSyncSchemaVersion(ctx)
	if err != nil {
		t.Fatal("read core golden sync absence failed")
	}
	projection := map[string]any{
		"sync_version": version,
		"finalization": map[string]any{
			"account_id": first.AccountID, "evacuation_id": first.EvacuationID,
			"source_status": first.SourceStatus, "finalized": first.Finalized,
			"already_finalized": first.AlreadyFinalized, "timestamp_present": !first.FinalizedAt.IsZero(),
			"retry_finalized": retry.Finalized, "retry_already_finalized": retry.AlreadyFinalized,
			"retry_identity_stable":  retry.AccountID == first.AccountID && retry.EvacuationID == first.EvacuationID && retry.SourceStatus == first.SourceStatus,
			"retry_timestamp_stable": retry.FinalizedAt.Equal(first.FinalizedAt),
			"account_rows":           finalAccounts, "realm_rows": finalRealms,
			"audit_rows": finalEvents, "receipt_rows": finalReceipts,
		},
		"purge": map[string]any{
			"preview":              coreMovementGoldenPurgeResult(preview),
			"preview_account_rows": previewAccounts, "preview_realm_rows": previewRealms,
			"preview_unchanged": previewUnchanged,
			"enforced":          coreMovementGoldenPurgeResult(enforced),
			"retry":             coreMovementGoldenPurgeResult(purgeRetry),
			"account_rows":      purgeAccounts, "realm_rows": purgeRealms,
			"audit_rows": purgeEvents, "erasure_events": purgeErasureEvents,
			"status": purgeStatus, "display_name": purgeDisplayName, "closed_reason": purgeClosedReason,
			"email_null": purgeEmailNull, "timestamp_present": purgeTimestampPresent,
		},
		"decoy": map[string]any{"account_rows": decoyAccounts, "realm_rows": decoyRealms, "unchanged": decoyUnchanged},
	}
	const golden = `{
		"sync_version": 0,
		"finalization": {
			"account_id": "acc_core_golden_finalize", "evacuation_id": "evac_core_golden",
			"source_status": "suspended", "finalized": true, "already_finalized": false,
			"timestamp_present": true, "retry_finalized": true, "retry_already_finalized": true,
			"retry_identity_stable": true, "retry_timestamp_stable": true,
			"account_rows": 0, "realm_rows": 0, "audit_rows": 0, "receipt_rows": 1
		},
		"purge": {
			"preview": {"eligible": true, "purged": false, "deferred_vault": false,
				"attachment_invariant_failure": false, "provision_receipt_scrubs": 0, "nonzero_deleted": {"realms": 1}},
			"preview_account_rows": 1, "preview_realm_rows": 1, "preview_unchanged": true,
			"enforced": {"eligible": true, "purged": true, "deferred_vault": false,
				"attachment_invariant_failure": false, "provision_receipt_scrubs": 0, "nonzero_deleted": {"realms": 1}},
			"retry": {"eligible": false, "purged": false, "deferred_vault": false,
				"attachment_invariant_failure": false, "provision_receipt_scrubs": 0, "nonzero_deleted": {}},
			"account_rows": 1, "realm_rows": 0, "audit_rows": 1, "erasure_events": 1,
			"status": "closed", "display_name": "", "closed_reason": "", "email_null": true, "timestamp_present": true
		},
		"decoy": {"account_rows": 1, "realm_rows": 1, "unchanged": true}
	}`
	var expected any
	if err := json.Unmarshal([]byte(golden), &expected); err != nil {
		t.Fatal("invalid core movement golden")
	}
	want, err := json.Marshal(expected)
	if err != nil {
		t.Fatal("encode core movement golden failed")
	}
	got, err := json.Marshal(projection)
	if err != nil {
		t.Fatal("encode core movement projection failed")
	}
	if !bytes.Equal(got, want) {
		// The projection contains only fixed fixture IDs, counts and booleans;
		// it deliberately excludes raw errors, timestamps and fingerprints.
		t.Fatalf("core movement golden differs: %s", got)
	}
}

func coreMovementGoldenPurgeResult(result accountPurgeCandidateResult) map[string]any {
	nonzero := make(map[string]int64)
	for table, count := range result.deletedByTable {
		if count != 0 {
			nonzero[table] = count
		}
	}
	return map[string]any{
		"eligible": result.eligible, "purged": result.purged,
		"deferred_vault":               result.deferredVaultLifecycle,
		"attachment_invariant_failure": result.attachmentInvariantFailure,
		"provision_receipt_scrubs":     result.provisionReceiptScrubs,
		"nonzero_deleted":              nonzero,
	}
}
