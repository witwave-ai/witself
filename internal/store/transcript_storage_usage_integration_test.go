package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestTranscriptStorageUsageTracksEveryWritePathPostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := newTranscriptStorageTestPrincipal(t, st, "writes-a")
	b := newTranscriptStorageTestPrincipal(t, st, "writes-b")
	t1 := createTranscriptStorageTestConversation(t, st, a, "t1")
	// Ensure the deletion witness cannot accidentally use the same shard as
	// an incorrect account-based hash.
	t1 = transcriptStorageTestMoveToOtherShard(t, st, t1, a.AccountID)
	t2 := createTranscriptStorageTestConversation(t, st, a, "t2")
	t2 = transcriptStorageTestMoveToOtherShard(t, st, t2, t1.ID)
	t3 := createTranscriptStorageTestConversation(t, st, b, "t3")
	first := appendTranscriptStorageTestEntry(t, st, a, t1, AppendTranscriptEntryInput{
		ExternalID: "first", Role: TranscriptRoleUser, Body: "first entry",
	})
	assertTranscriptStorageUsageMatchesRecount(t, st, a.AccountID, b.AccountID)
	before := transcriptStorageRecount(t, st, a.AccountID)
	batch := []AppendTranscriptEntryInput{
		{ExternalID: "second-é界", Role: TranscriptRoleAssistant, Body: "a reply é界", Payload: json.RawMessage(`{"answer": 42, "text": "é"}`), Model: "fixture-model-é界", ReplyToEntryID: first.ID},
		{ExternalID: "third", Role: TranscriptRoleUser, Body: "follow up"},
	}
	entries, err := st.AppendTranscriptEntries(ctx, a.AccountID, a.RealmID, a.ID, t1.ID, batch)
	if err != nil {
		t.Fatal(err)
	}
	assertTranscriptStorageUsageMatchesRecount(t, st, a.AccountID, b.AccountID)
	after := transcriptStorageRecount(t, st, a.AccountID)
	logicalBytes := transcriptEntryLogicalBytes(first)
	for _, entry := range entries {
		logicalBytes += transcriptEntryLogicalBytes(entry)
	}
	// The SQL counter and recount share one function. Check the independent Go
	// byte count too, including multibyte body, external_id, and model values.
	if got := transcriptStorageTestRowsSum(transcriptStorageUsageRows(t, st, a.AccountID)); got != logicalBytes {
		t.Fatalf("counter retained %d bytes, logical entries contain %d", got, logicalBytes)
	}
	var quantity int64
	if err := st.pool.QueryRow(ctx, `SELECT quantity FROM usage_events
		WHERE account_id=$1 AND dimension='transcript_storage_byte' AND idempotency_key=$2`,
		a.AccountID, "transcript_entry_storage:"+usageBatchKey([]string{entries[0].ID, entries[1].ID})).Scan(&quantity); err != nil {
		t.Fatal(err)
	}
	if quantity != after-before {
		t.Fatalf("batch usage event = %d, counter delta = %d", quantity, after-before)
	}
	beforeReplay := transcriptStorageUsageRows(t, st, a.AccountID)
	replayed, err := st.AppendTranscriptEntries(ctx, a.AccountID, a.RealmID, a.ID, t1.ID, batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != len(entries) || replayed[0].ID != entries[0].ID || replayed[1].ID != entries[1].ID {
		t.Fatal("batch replay did not return the original entries")
	}
	if !reflect.DeepEqual(beforeReplay, transcriptStorageUsageRows(t, st, a.AccountID)) {
		t.Fatal("batch replay changed counter rows")
	}
	assertTranscriptStorageUsageMatchesRecount(t, st, a.AccountID, b.AccountID)
	if _, err := st.pool.Exec(ctx, `UPDATE transcript_entries SET reply_to_entry_id=$1 WHERE id=$2`, first.ID, entries[1].ID); err != nil {
		t.Fatal(err)
	}
	assertTranscriptStorageUsageMatchesRecount(t, st, a.AccountID, b.AccountID)
	if got := transcriptStorageRecount(t, st, a.AccountID); got != after+int64(len(first.ID)) {
		t.Fatalf("reply update added %d bytes, want %d", got-after, len(first.ID))
	}
	// A multi-row move exercises both signs of the grouped UPDATE delta, even
	// when the destination shard has no counter yet. Move back before retention.
	for _, move := range [][2]string{{t1.ID, t2.ID}, {t2.ID, t1.ID}} {
		moved, err := st.pool.Exec(ctx, `UPDATE transcript_entries SET transcript_id=$1
			WHERE account_id=$2 AND transcript_id=$3`, move[1], a.AccountID, move[0])
		if err != nil {
			t.Fatal(err)
		}
		if moved.RowsAffected() != int64(len(entries)+1) {
			t.Fatalf("grouped update moved %d entries, want %d", moved.RowsAffected(), len(entries)+1)
		}
		assertTranscriptStorageUsageMatchesRecount(t, st, a.AccountID, b.AccountID)
	}
	secondConversation := appendTranscriptStorageTestEntry(t, st, a, t2, AppendTranscriptEntryInput{ExternalID: "kept", Role: TranscriptRoleUser, Body: "retained"})
	otherAccount := appendTranscriptStorageTestEntry(t, st, b, t3, AppendTranscriptEntryInput{ExternalID: "other", Role: TranscriptRoleUser, Body: "other account"})
	assertTranscriptStorageUsageMatchesRecount(t, st, a.AccountID, b.AccountID)
	bBefore := transcriptStorageUsageRows(t, st, b.AccountID)

	// Exercise both read helpers inside one transaction, including two ids
	// from different accounts to prove the entry sum remains account-scoped.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	used, exact, err := transcriptStorageUsageTx(ctx, tx, a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if !exact {
		t.Fatal("new account counter is not exact")
	}
	if want := transcriptStorageRecount(t, st, a.AccountID); used != want {
		t.Fatalf("used helper = %d, recount = %d", used, want)
	}
	chosenIDs := []string{first.ID, secondConversation.ID, otherAccount.ID}
	chosen, err := transcriptEntriesStorageBytesTx(ctx, tx, a.AccountID, chosenIDs)
	if err != nil {
		t.Fatal(err)
	}
	var chosenWant int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(witself_transcript_entry_storage_bytes(external_id,role,body,payload,model,reply_to_entry_id,artifacts)),0)::bigint
		FROM transcript_entries WHERE account_id=$1 AND id=ANY($2)`, a.AccountID, []string{first.ID, secondConversation.ID}).Scan(&chosenWant); err != nil {
		t.Fatal(err)
	}
	if chosen != chosenWant {
		t.Fatalf("entry helper = %d, chosen recount = %d", chosen, chosenWant)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := st.SetAccountPlan(ctx, a.AccountID, 0, "", "free", map[string]int64{}, map[string]int64{TranscriptRetentionDaysPolicy: 30}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE transcript_conversations SET updated_at=statement_timestamp()-interval '31 days' WHERE id=$1`, t1.ID); err != nil {
		t.Fatal(err)
	}
	result, err := st.ProcessTranscriptRetentionBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 {
		t.Fatalf("retention deleted %d conversations, want 1", result.Deleted)
	}
	if _, _, err := st.GetTranscript(ctx, a, t1.ID); !errors.Is(err, ErrTranscriptNotFound) {
		t.Fatalf("expired conversation lookup error = %v", err)
	}
	assertTranscriptStorageUsageMatchesRecount(t, st, a.AccountID, b.AccountID)
	if got := transcriptStorageRecount(t, st, a.AccountID); got != transcriptEntryLogicalBytes(secondConversation) {
		t.Fatalf("retention left %d bytes, want %d", got, transcriptEntryLogicalBytes(secondConversation))
	}
	if !reflect.DeepEqual(bBefore, transcriptStorageUsageRows(t, st, b.AccountID)) {
		t.Fatal("retention changed the other account's counter")
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM transcript_entries WHERE account_id=$1`, a.AccountID); err != nil {
		t.Fatal(err)
	}
	assertTranscriptStorageUsageMatchesRecount(t, st, a.AccountID, b.AccountID)
	if got := transcriptStorageTestRowsSum(transcriptStorageUsageRows(t, st, a.AccountID)); got != 0 {
		t.Fatalf("account delete left %d counter bytes", got)
	}
}

func TestTranscriptStorageUsageImportMarksPendingPostgres(t *testing.T) {
	st, schemaDSN := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := newTranscriptStorageTestPrincipal(t, st, "evacuation")
	first := createTranscriptStorageTestConversation(t, st, p, "first")
	second := createTranscriptStorageTestConversation(t, st, p, "second")
	appendTranscriptStorageTestEntry(t, st, p, first, AppendTranscriptEntryInput{ExternalID: "one", Role: TranscriptRoleUser, Body: "first conversation"})
	appendTranscriptStorageTestEntry(t, st, p, second, AppendTranscriptEntryInput{ExternalID: "two", Role: TranscriptRoleAssistant, Body: "second conversation"})
	assertTranscriptStorageUsageMatchesRecount(t, st, p.AccountID)
	const evacuationID = "evac_transcript_storage_replay"
	if _, err := st.BeginAccountEvacuation(ctx, p.AccountID, evacuationID, "transcript storage test"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccountEvacuation(ctx, p.AccountID, evacuationID, "source-cell", "test", &archive); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AbortAccountEvacuation(ctx, p.AccountID, evacuationID); err != nil {
		t.Fatal(err)
	}
	if err := deleteAccountForIntegrationTest(ctx, st, p.AccountID); err != nil {
		t.Fatal(err)
	}
	if len(transcriptStorageUsageRows(t, st, p.AccountID)) == 0 {
		t.Fatal("account deletion unexpectedly removed orphan counter rows")
	}
	if strings.HasPrefix(schemaDSN, "postgres://") || strings.HasPrefix(schemaDSN, "postgresql://") {
		u, err := url.Parse(schemaDSN)
		if err != nil {
			t.Fatal(err)
		}
		query := u.Query()
		query.Set("pool_max_conns", "1")
		u.RawQuery = query.Encode()
		schemaDSN = u.String()
	} else {
		schemaDSN += " pool_max_conns=1"
	}
	destination, err := Open(ctx, schemaDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(destination.Close)
	_, disposition, err := destination.ImportAccountEvacuation(ctx, p.AccountID, evacuationID, bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if disposition.AlreadyImported {
		t.Fatal("first evacuation import reported a replay")
	}
	assertTranscriptStoragePending(t, destination, p.AccountID)
	imported := transcriptStorageStateRows(t, destination, p.AccountID)
	summary, err := destination.ReconcileTranscriptStorageUsageUntilDone(ctx, transcriptStorageFastConfig())
	if err != nil || summary.UnverifiedEligible != 0 || summary.UnverifiedDeferred != 16 || summary.Entries != 0 {
		t.Fatalf("deferred import reconcile: %+v, error %v", summary, err)
	}
	_, disposition, err = destination.ImportAccountEvacuation(ctx, p.AccountID, evacuationID, bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !disposition.AlreadyImported {
		t.Fatal("same evacuation replay was not marked AlreadyImported")
	}
	if !reflect.DeepEqual(imported, transcriptStorageStateRows(t, destination, p.AccountID)) {
		t.Fatal("already-imported replay changed counter rows")
	}
	// Import preserves the destination's evacuation write fence. Complete the
	// move before an ordinary delete; this does not change the transcript bulk
	// setting, so the same connection still detects a setting leaked by import.
	completed, err := destination.CompleteAccountEvacuation(ctx, p.AccountID, evacuationID)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Completed {
		t.Fatal("destination evacuation did not complete")
	}
	transcriptStorageReconcileAndVerify(t, destination, p.AccountID)
	var removed int64
	if err := destination.pool.QueryRow(ctx, `SELECT sum(witself_transcript_entry_storage_bytes(external_id,role,body,payload,model,reply_to_entry_id,artifacts))::bigint
		FROM transcript_entries WHERE account_id=$1 AND transcript_id=$2`, p.AccountID, first.ID).Scan(&removed); err != nil {
		t.Fatal(err)
	}
	before := transcriptStorageTestRowsSum(transcriptStorageUsageRows(t, destination, p.AccountID))
	if _, err := destination.pool.Exec(ctx, `DELETE FROM transcript_conversations WHERE account_id=$1 AND id=$2`, p.AccountID, first.ID); err != nil {
		t.Fatal(err)
	}
	assertTranscriptStorageUsageMatchesRecount(t, destination, p.AccountID)
	if got := transcriptStorageTestRowsSum(transcriptStorageUsageRows(t, destination, p.AccountID)); got != before-removed {
		t.Fatalf("post-import deletion retained %d bytes, want %d", got, before-removed)
	}
	// A second evacuation round trip must invalidate the prior incarnation, too.
	previous := transcriptStorageStateRows(t, destination, p.AccountID)
	if err := deleteAccountForIntegrationTest(ctx, destination, p.AccountID); err != nil {
		t.Fatal(err)
	}
	if len(transcriptStorageStateRows(t, destination, p.AccountID)) == 0 {
		t.Fatal("round trip lost orphan counter rows")
	}
	_, disposition, err = destination.ImportAccountEvacuation(ctx, p.AccountID, evacuationID, bytes.NewReader(archive.Bytes()))
	if err != nil || disposition.AlreadyImported {
		t.Fatalf("round-trip import replay=%t, error=%v", disposition.AlreadyImported, err)
	}
	assertTranscriptStoragePending(t, destination, p.AccountID)
	for shard, row := range transcriptStorageStateRows(t, destination, p.AccountID) {
		if previous[shard].Epoch == row.Epoch {
			t.Fatal("round-trip import reused an old epoch")
		}
	}
	if _, err := destination.CompleteAccountEvacuation(ctx, p.AccountID, evacuationID); err != nil {
		t.Fatal(err)
	}
	transcriptStorageReconcileAndVerify(t, destination, p.AccountID)
	// An older binary imports without the bulk bypass or pending reset. Its
	// ordinary inserts must create exact rows through the installed triggers.
	legacy := newTranscriptStorageTestPrincipal(t, destination, "legacy-import")
	legacyTranscript := createTranscriptStorageTestConversation(t, destination, legacy, "legacy-import")
	if _, err := destination.pool.Exec(ctx, `INSERT INTO transcript_entries
		(id,account_id,transcript_id,realm_id,recorded_by_agent_id,sequence,role,body)
		VALUES ('legacy-import-entry',$1,$2,$3,$4,1,'user','legacy imported bytes')`,
		legacy.AccountID, legacyTranscript.ID, legacy.RealmID, legacy.ID); err != nil {
		t.Fatal(err)
	}
	assertTranscriptStorageUsageMatchesRecount(t, destination, legacy.AccountID)
}

func TestTranscriptStorageUsageDriftSelfHealsPostgres(t *testing.T) {
	st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	migrationTestUpTo(t, dsn, 101)
	ctx := context.Background()
	for _, missing := range []bool{false, true} {
		p := newTranscriptStorageTestPrincipal(t, st, fmt.Sprintf("drift-%t", missing))
		tr := createTranscriptStorageTestConversation(t, st, p, "drift")
		appendTranscriptStorageTestEntry(t, st, p, tr, AppendTranscriptEntryInput{Role: TranscriptRoleUser, Body: "drifted entry"})
		statement := `UPDATE transcript_storage_usage SET retained_bytes=0 WHERE account_id=$1`
		if missing {
			statement = `DELETE FROM transcript_storage_usage WHERE account_id=$1`
		}
		if _, err := st.pool.Exec(ctx, statement, p.AccountID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetAccountPlan(ctx, p.AccountID, 0, "", "free", map[string]int64{}, map[string]int64{TranscriptRetentionDaysPolicy: 30}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `UPDATE transcript_conversations SET updated_at=statement_timestamp()-interval '31 days' WHERE id=$1`, tr.ID); err != nil {
			t.Fatal(err)
		}
		result, err := st.ProcessTranscriptRetentionBatch(ctx, 10)
		if err != nil || result.Deleted != 1 {
			t.Fatalf("drift retention: %+v, %v", result, err)
		}
		states := transcriptStorageStateRows(t, st, p.AccountID)
		if len(states) != 1 {
			t.Fatalf("drift row count=%d", len(states))
		}
		for _, row := range states {
			if row.Bytes >= 0 || row.Exact {
				t.Fatalf("drift did not create a negative, non-exact row: %+v", row)
			}
		}
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		used, exact, err := transcriptStorageUsageTx(ctx, tx, p.AccountID)
		_ = tx.Rollback(ctx)
		if err != nil || used != 0 || exact {
			t.Fatalf("drift read=%d,%t,%v", used, exact, err)
		}
		transcriptStorageReconcileAndVerify(t, st, p.AccountID)
	}
	// The correction can exceed the stored counter's bound while both the
	// measured total and corrected result remain valid.
	p := newTranscriptStorageTestPrincipal(t, st, "large-negative-drift")
	tr := createTranscriptStorageTestConversation(t, st, p, "large-negative-drift")
	appendTranscriptStorageTestEntry(t, st, p, tr, AppendTranscriptEntryInput{Role: TranscriptRoleUser, Body: "positive measured bytes"})
	if _, err := st.pool.Exec(ctx, `UPDATE transcript_storage_usage SET retained_bytes=$2,exact=false WHERE account_id=$1`, p.AccountID, -transcriptStorageReconcileLimit); err != nil {
		t.Fatal(err)
	}
	transcriptStorageReconcileAndVerify(t, st, p.AccountID)
	// A purge also succeeds after drift and leaves a tombstone whose remaining
	// negative rows can be reconciled to exact zero.
	appendTranscriptStorageTestEntry(t, st, p, tr, AppendTranscriptEntryInput{Role: TranscriptRoleUser, Body: "purge drift"})
	if _, err := st.pool.Exec(ctx, `UPDATE transcript_storage_usage SET retained_bytes=0 WHERE account_id=$1`, p.AccountID); err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := st.pool.QueryRow(ctx, `SELECT id FROM operators WHERE account_id=$1 AND role='account_owner' LIMIT 1`, p.AccountID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseAccount(ctx, p.AccountID, owner, "drift purge fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE accounts SET closed_at=statement_timestamp()-interval '31 days' WHERE id=$1`, p.AccountID); err != nil {
		t.Fatal(err)
	}
	purged, err := st.ProcessAccountPurgeBatch(ctx, 10, 30*24*time.Hour)
	if err != nil || purged.PurgedAccounts != 1 {
		t.Fatalf("drift purge: %+v, %v", purged, err)
	}
	purgedRows := transcriptStorageStateRows(t, st, p.AccountID)
	if len(purgedRows) == 0 {
		t.Fatal("purge removed every drifted counter row")
	}
	for _, row := range purgedRows {
		if row.Bytes >= 0 || row.Exact {
			t.Fatalf("purge did not retain negative non-exact drift: %+v", row)
		}
	}
	transcriptStorageReconcileAndVerify(t, st, p.AccountID)
}

func TestTranscriptStorageUsageBulkSettingSkipsTriggersPostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := newTranscriptStorageTestPrincipal(t, st, "bulk")
	tr := createTranscriptStorageTestConversation(t, st, p, "bulk")
	appendTranscriptStorageTestEntry(t, st, p, tr, AppendTranscriptEntryInput{Role: TranscriptRoleUser, Body: "baseline entry keeps a positive shard"})
	baseline := transcriptStorageUsageRows(t, st, p.AccountID)
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := enableTranscriptStorageUsageBulkTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		name string
		sql  string
		args []any
	}{
		{"insert", `INSERT INTO transcript_entries(id,account_id,transcript_id,realm_id,recorded_by_agent_id,sequence,role,body) VALUES('ent_storage_bulk',$1,$2,$3,$4,2,'user','temporary')`, []any{p.AccountID, tr.ID, p.RealmID, p.ID}},
		{"update", `UPDATE transcript_entries SET body='a larger temporary entry body' WHERE id='ent_storage_bulk'`, nil},
		{"delete", `DELETE FROM transcript_entries WHERE id='ent_storage_bulk'`, nil},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("bulk %s: %v", statement.name, err)
		}
		if !reflect.DeepEqual(baseline, transcriptStorageTestQueryRows(t, tx, p.AccountID)) {
			t.Fatalf("bulk %s changed counter rows", statement.name)
		}
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('witself.transcript_storage_usage_bulk','off',true)`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		before := transcriptStorageTestQueryRows(t, tx, p.AccountID)
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("ordinary %s: %v", statement.name, err)
		}
		if reflect.DeepEqual(before, transcriptStorageTestQueryRows(t, tx, p.AccountID)) {
			t.Fatalf("ordinary %s left counter unchanged", statement.name)
		}
		assertTranscriptStorageTestQueryMatchesRecount(t, tx, p.AccountID)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(baseline, transcriptStorageUsageRows(t, st, p.AccountID)) {
		t.Fatal("rolled-back trigger changes escaped transaction")
	}
}

func newTranscriptStorageTestPrincipal(t *testing.T, st *Store, name string) Principal {
	t.Helper()
	ctx := context.Background()
	account, err := st.ProvisionAccount(ctx, "transcript-storage-"+name+"@example.test", "transcript storage "+name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if active, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !active {
		t.Fatalf("activate account = %t, error = %v", active, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "recorder")
	if err != nil {
		t.Fatal(err)
	}
	return Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID, RealmID: realm.ID, AccountStatus: "active"}
}

func createTranscriptStorageTestConversation(t *testing.T, st *Store, p Principal, externalID string) Transcript {
	t.Helper()
	tr, err := st.CreateTranscript(context.Background(), p.AccountID, p.RealmID, p.ID, CreateTranscriptInput{ExternalID: externalID})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func appendTranscriptStorageTestEntry(t *testing.T, st *Store, p Principal, tr Transcript, in AppendTranscriptEntryInput) TranscriptEntry {
	t.Helper()
	entry, err := st.AppendTranscriptEntry(context.Background(), p.AccountID, p.RealmID, p.ID, tr.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// Pick a fixture id on a different shard before the conversation has entries.
// This makes wrong-key deletion mutations deterministic rather than dependent
// on randomly generated conversation ids avoiding one of sixteen buckets.
func transcriptStorageTestMoveToOtherShard(t *testing.T, st *Store, tr Transcript, avoid string) Transcript {
	t.Helper()
	ctx := context.Background()
	var candidate string
	if err := st.pool.QueryRow(ctx, `SELECT $1::text || '_' || n::text FROM generate_series(1,1000) n
		WHERE witself_transcript_storage_shard($1::text || '_' || n::text) <> witself_transcript_storage_shard($2)
		ORDER BY n LIMIT 1`, tr.ID, avoid).Scan(&candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE transcript_conversations SET id=$1 WHERE id=$2`, candidate, tr.ID); err != nil {
		t.Fatal(err)
	}
	tr.ID = candidate
	return tr
}

type transcriptStorageTestQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func transcriptStorageUsageRows(t *testing.T, st *Store, accountID string) map[int16]int64 {
	t.Helper()
	return transcriptStorageTestQueryRows(t, st.pool, accountID)
}

func transcriptStorageTestQueryRows(t *testing.T, q transcriptStorageTestQuerier, accountID string) map[int16]int64 {
	t.Helper()
	rows, err := q.Query(context.Background(), `SELECT shard,retained_bytes FROM transcript_storage_usage WHERE account_id=$1 ORDER BY shard`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := make(map[int16]int64)
	for rows.Next() {
		var shard int16
		var retained int64
		if err := rows.Scan(&shard, &retained); err != nil {
			t.Fatal(err)
		}
		out[shard] = retained
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func transcriptStorageRecount(t *testing.T, st *Store, accountID string) int64 {
	t.Helper()
	var used int64
	if err := st.pool.QueryRow(context.Background(), `SELECT COALESCE(sum(witself_transcript_entry_storage_bytes(external_id,role,body,payload,model,reply_to_entry_id,artifacts)),0)::bigint
		FROM transcript_entries WHERE account_id=$1`, accountID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	return used
}

func assertTranscriptStorageUsageMatchesRecount(t *testing.T, st *Store, accountIDs ...string) {
	t.Helper()
	for _, accountID := range accountIDs {
		assertTranscriptStorageTestQueryMatchesRecount(t, st.pool, accountID)
	}
}

func assertTranscriptStorageTestQueryMatchesRecount(t *testing.T, q transcriptStorageTestQuerier, accountID string) {
	t.Helper()
	var drift int
	if err := q.QueryRow(context.Background(), `WITH recount AS (
		SELECT account_id,witself_transcript_storage_shard(transcript_id) AS shard,
			sum(witself_transcript_entry_storage_bytes(external_id,role,body,payload,model,reply_to_entry_id,artifacts))::bigint AS retained_bytes
		FROM transcript_entries WHERE account_id=$1 GROUP BY 1,2
	), actual AS (SELECT account_id,shard,retained_bytes FROM transcript_storage_usage WHERE account_id=$1)
	SELECT count(*) FROM actual FULL OUTER JOIN recount USING(account_id,shard)
	WHERE COALESCE(actual.retained_bytes,0) <> COALESCE(recount.retained_bytes,0)`, accountID).Scan(&drift); err != nil {
		t.Fatal(err)
	}
	if drift != 0 {
		t.Fatalf("counter differs from recount in %d shards", drift)
	}
	var pending int
	if err := q.QueryRow(context.Background(), `SELECT count(*) FROM transcript_storage_usage WHERE account_id=$1 AND (NOT exact OR retained_bytes<0)`, accountID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("counter has %d non-exact or negative shards", pending)
	}
}

func transcriptStorageTestRowsSum(rows map[int16]int64) int64 {
	var total int64
	for _, n := range rows {
		total += n
	}
	return total
}
