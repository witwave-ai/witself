package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestTranscriptStorageUsageSchema101BackfillPostgres(t *testing.T) {
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
	accountA := newTranscriptStorageTestPrincipal(t, st, "backfill-a")
	accountB := newTranscriptStorageTestPrincipal(t, st, "backfill-b")
	accountC := newTranscriptStorageTestPrincipal(t, st, "backfill-empty")
	var conversationA Transcript
	for _, fixture := range []struct {
		principal     Principal
		conversations int
	}{
		{accountA, 3},
		{accountB, 1},
	} {
		for i := range fixture.conversations {
			conversation := createTranscriptStorageTestConversation(t, st, fixture.principal, fmt.Sprintf("backfill-%d", i))
			first := appendTranscriptStorageTestEntry(t, st, fixture.principal, conversation, AppendTranscriptEntryInput{
				ExternalID: "first", Role: TranscriptRoleUser, Body: "backfill body",
			})
			appendTranscriptStorageTestEntry(t, st, fixture.principal, conversation, AppendTranscriptEntryInput{
				ExternalID: "reply", Role: TranscriptRoleAssistant, Body: "a reply with non-ASCII bytes: é",
				Payload: json.RawMessage(`{"nested":{"value":"payload"},"count":2}`),
				Model:   "backfill-model", ReplyToEntryID: first.ID,
			})
			if fixture.principal.AccountID == accountA.AccountID {
				conversationA = conversation
			}
		}
	}

	migrationTestUpTo(t, dsn, 101)
	assertMigrationTestVersion(t, dsn, 101)
	assertIndexesPreserved()
	assertTranscriptStorageUsageMatchesRecount(t, st, accountA.AccountID, accountB.AccountID, accountC.AccountID)
	if rows := transcriptStorageUsageRows(t, st, accountC.AccountID); len(rows) != 0 {
		t.Fatalf("empty account has %d counter rows, want none", len(rows))
	}
	for _, principal := range []Principal{accountA, accountB} {
		var used int64
		for _, value := range transcriptStorageUsageRows(t, st, principal.AccountID) {
			used += value
		}
		if want := transcriptStorageRecount(t, st, principal.AccountID); used != want || used == 0 {
			t.Fatalf("backfilled account sum = %d, want positive recount %d", used, want)
		}
	}
	appendTranscriptStorageTestEntry(t, st, accountA, conversationA, AppendTranscriptEntryInput{
		ExternalID: "after-up", Role: TranscriptRoleUser, Body: "insert trigger runs after backfill",
	})
	assertTranscriptStorageUsageMatchesRecount(t, st, accountA.AccountID, accountB.AccountID, accountC.AccountID)

	migrationTestDownTo(t, dsn, 100)
	assertMigrationTestVersion(t, dsn, 100)
	assertIndexesPreserved()
	var tableAbsent, shardFunctionAbsent bool
	if err := st.pool.QueryRow(ctx, `SELECT to_regclass('transcript_storage_usage') IS NULL,
		to_regprocedure('witself_transcript_storage_shard(text)') IS NULL`).Scan(&tableAbsent, &shardFunctionAbsent); err != nil {
		t.Fatal(err)
	}
	if !tableAbsent || !shardFunctionAbsent {
		t.Fatalf("schema 0100 cleanup: table absent = %v, shard function absent = %v", tableAbsent, shardFunctionAbsent)
	}
	appendTranscriptStorageTestEntry(t, st, accountA, conversationA, AppendTranscriptEntryInput{
		ExternalID: "after-down", Role: TranscriptRoleUser, Body: "append remains available after downgrade",
	})

	migrationTestUpTo(t, dsn, 101)
	assertMigrationTestVersion(t, dsn, 101)
	assertIndexesPreserved()
	assertTranscriptStorageUsageMatchesRecount(t, st, accountA.AccountID, accountB.AccountID, accountC.AccountID)
}
