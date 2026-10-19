package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestTranscriptStorageUsageSchema101PendingRowsPostgres(t *testing.T) {
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	migrationTestUpTo(t, dsn, 100)
	// Preserve every index introduced by the two preceding migrations, including
	// the reply-to index needed to keep entry deletion bounded.
	indexes := append(foreignKeyMigrationIndexes(t, foreignKeyHotMigration),
		foreignKeyMigrationIndexes(t, foreignKeyRemainingMigration)...)
	indexDefinitions := make(map[string]string, len(indexes))
	for _, index := range indexes {
		var definition string
		if err := st.pool.QueryRow(ctx, `SELECT pg_get_indexdef(to_regclass($1))`, index.name).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		indexDefinitions[index.name] = definition
	}
	assertIndexesPreserved := func() {
		t.Helper()
		for _, index := range indexes {
			var definition string
			var valid, ready bool
			if err := st.pool.QueryRow(ctx, `SELECT pg_get_indexdef(indexrelid), indisvalid, indisready
				FROM pg_index WHERE indexrelid=to_regclass($1)`, index.name).Scan(&definition, &valid, &ready); err != nil {
				t.Fatalf("inspect preserved index %s: %v", index.name, err)
			}
			if definition != indexDefinitions[index.name] || !valid || !ready {
				t.Fatalf("migration 101 changed index %s or its validity/readiness", index.name)
			}
		}
	}
	accountA := newTranscriptStorageTestPrincipal(t, st, "pending-a")
	accountB := newTranscriptStorageTestPrincipal(t, st, "pending-b")
	accountC := newTranscriptStorageTestPrincipal(t, st, "pending-empty")
	var conversationA Transcript
	for _, fixture := range []struct {
		principal     Principal
		conversations int
	}{
		{accountA, 6},
		{accountB, 1},
	} {
		for i := range fixture.conversations {
			conversation := createTranscriptStorageTestConversation(t, st, fixture.principal, fmt.Sprintf("pending-%d", i))
			if fixture.principal.AccountID == accountA.AccountID {
				for salt := 0; ; salt++ {
					id := fmt.Sprintf("pending-shard-%d-%d", i, salt)
					digest := sha256.Sum256([]byte(id))
					if int(digest[0]%16) != i%3 {
						continue
					}
					if _, err := st.pool.Exec(ctx, `UPDATE transcript_conversations SET id=$1 WHERE id=$2`, id, conversation.ID); err != nil {
						t.Fatal(err)
					}
					conversation.ID = id
					break
				}
			}
			first := appendTranscriptStorageTestEntry(t, st, fixture.principal, conversation, AppendTranscriptEntryInput{
				ExternalID: "first", Role: TranscriptRoleUser, Body: "pending body",
			})
			appendTranscriptStorageTestEntry(t, st, fixture.principal, conversation, AppendTranscriptEntryInput{
				ExternalID: "reply", Role: TranscriptRoleAssistant, Body: "a reply with non-ASCII bytes: é",
				Payload: json.RawMessage(`{"nested":{"value":"payload"},"count":2}`),
				Model:   "pending-model", ReplyToEntryID: first.ID,
			})
			if fixture.principal.AccountID == accountA.AccountID {
				conversationA = conversation
			}
		}
	}

	migrationTestUpTo(t, dsn, 101)
	assertMigrationTestVersion(t, dsn, 101)
	assertIndexesPreserved()
	assertPending := func() {
		t.Helper()
		for _, principal := range []Principal{accountA, accountB} {
			var rows, zeroPending int
			if err := st.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE retained_bytes=0 AND NOT exact)
				FROM transcript_storage_usage WHERE account_id=$1`, principal.AccountID).Scan(&rows, &zeroPending); err != nil {
				t.Fatal(err)
			}
			if rows != 16 || zeroPending != 16 {
				t.Fatalf("T0 rows=%d zero/non-exact=%d, want 16 each", rows, zeroPending)
			}
		}
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for _, principal := range []Principal{accountA, accountB, accountC} {
			used, exact, err := transcriptStorageUsageTx(ctx, tx, principal.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if used != 0 || exact != (principal.AccountID == accountC.AccountID) {
				t.Fatalf("T0 read used=%d exact=%v", used, exact)
			}
		}
		if rows := transcriptStorageUsageRows(t, st, accountC.AccountID); len(rows) != 0 {
			t.Fatalf("empty account has %d counter rows, want none", len(rows))
		}
	}
	assertVerified := func(store *Store) {
		t.Helper()
		assertTranscriptStorageUsageMatchesRecount(t, store, accountA.AccountID, accountB.AccountID, accountC.AccountID)
		for _, principal := range []Principal{accountA, accountB, accountC} {
			verification, err := store.VerifyTranscriptStorageUsage(ctx, principal.AccountID)
			if err != nil || verification.Mismatches != 0 || !verification.Exact {
				t.Fatalf("verify: %+v, error: %v", verification, err)
			}
		}
	}
	assertPending()
	for _, principal := range []Principal{accountA, accountB, accountC} {
		verification, err := st.VerifyTranscriptStorageUsage(ctx, principal.AccountID)
		if err != nil {
			t.Fatal(err)
		}
		if principal.AccountID == accountC.AccountID {
			if verification.Mismatches != 0 || !verification.Exact {
				t.Fatalf("empty account verify: %+v, want zero mismatches and exact", verification)
			}
		} else if verification.Mismatches <= 0 || verification.Exact {
			t.Fatalf("pending account verify: %+v, want mismatches and non-exact", verification)
		}
	}
	cfg := DefaultTranscriptStorageReconcileConfig()
	cfg.PageEntries = 1
	if _, err := st.ReconcileTranscriptStorageUsageUntilDone(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	assertVerified(st)
	second, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, principal := range []Principal{accountA, accountB} {
		transcriptStorageMarkPending(t, second, principal.AccountID)
	}
	if _, err := second.ReconcileTranscriptStorageUsageUntilDone(ctx, DefaultTranscriptStorageReconcileConfig()); err != nil {
		t.Fatal(err)
	}
	assertVerified(second)
	appendTranscriptStorageTestEntry(t, st, accountA, conversationA, AppendTranscriptEntryInput{
		ExternalID: "after-up", Role: TranscriptRoleUser, Body: "insert trigger runs after pending",
	})
	assertTranscriptStorageUsageMatchesRecount(t, st, accountA.AccountID, accountB.AccountID, accountC.AccountID)

	migrationTestDownTo(t, dsn, 100)
	assertMigrationTestVersion(t, dsn, 100)
	assertIndexesPreserved()
	var tableAbsent, shardFunctionAbsent, sequenceAbsent bool
	if err := st.pool.QueryRow(ctx, `SELECT to_regclass('transcript_storage_usage') IS NULL,
		to_regprocedure('witself_transcript_storage_shard(text)') IS NULL,
		to_regclass('transcript_storage_usage_epoch_seq') IS NULL`).Scan(&tableAbsent, &shardFunctionAbsent, &sequenceAbsent); err != nil {
		t.Fatal(err)
	}
	if !tableAbsent || !shardFunctionAbsent || !sequenceAbsent {
		t.Fatalf("schema 0100 cleanup: table absent = %v, shard function absent = %v", tableAbsent, shardFunctionAbsent)
	}
	appendTranscriptStorageTestEntry(t, st, accountA, conversationA, AppendTranscriptEntryInput{
		ExternalID: "after-down", Role: TranscriptRoleUser, Body: "append remains available after downgrade",
	})

	migrationTestUpTo(t, dsn, 101)
	assertMigrationTestVersion(t, dsn, 101)
	assertIndexesPreserved()
	assertPending()
	if _, err := st.ReconcileTranscriptStorageUsageUntilDone(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	assertVerified(st)
}
