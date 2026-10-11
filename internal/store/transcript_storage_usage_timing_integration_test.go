package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/testenv"
)

// This opt-in harness measures trigger work at an operator-selected scale. It
// checks correctness, but intentionally imposes no machine-dependent time bound.
func TestTranscriptStorageUsageBulkTimingPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	if os.Getenv("WITSELF_TEST_TRANSCRIPT_STORAGE_TIMING") != "1" {
		t.Skip("set WITSELF_TEST_TRANSCRIPT_STORAGE_TIMING=1")
	}
	n := 2000
	if raw := os.Getenv("WITSELF_TEST_TRANSCRIPT_STORAGE_TIMING_ENTRIES"); raw != "" {
		var err error
		n, err = strconv.Atoi(raw)
		if err != nil || n <= 0 {
			t.Fatal("WITSELF_TEST_TRANSCRIPT_STORAGE_TIMING_ENTRIES must be a positive integer")
		}
	}
	ctx := context.Background()
	st, migrationDSN := newMigrationTestStore(t, dsn)
	migrationTestUpTo(t, migrationDSN, 100)
	x := newTranscriptStorageTestPrincipal(t, st, "timing-x")
	y := newTranscriptStorageTestPrincipal(t, st, "timing-y")
	const conversations = 50
	const bodyBytes = 1400
	const insertStatements = 1000
	const maxDeleteSample = 2000
	const payloadEvery = 4
	for _, principal := range []Principal{x, y} {
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO transcript_conversations (id, account_id, realm_id, owner_agent_id)
			SELECT $1 || '-timing-conversation-' || g, $1, $2, $3
			FROM generate_series(1, $4::integer) AS g`,
			principal.AccountID, principal.RealmID, principal.ID, conversations); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO transcript_entries
				(id, account_id, transcript_id, realm_id, recorded_by_agent_id, sequence, role, body, payload)
			SELECT $1 || '-timing-seed-' || g, $1,
				$1 || '-timing-conversation-' || (((g - 1) % $4::integer) + 1),
				$2, $3, ((g - 1) / $4::integer) + 1, 'user', repeat('a', $5::integer),
				CASE WHEN (g - 1) % $7::integer = 0 THEN jsonb_build_object(
					'type', 'tool_result', 'tool', 'file_search',
					'result', jsonb_build_object('path', 'internal/store/transcripts.go',
						'excerpt', repeat('A representative transcript search result with surrounding context. ', 50)),
					'usage', jsonb_build_object('input_tokens', 600, 'output_tokens', 900))
				END
			FROM generate_series(1, $6::integer) AS g`,
			principal.AccountID, principal.RealmID, principal.ID, conversations, bodyBytes, n, payloadEvery); err != nil {
			t.Fatal(err)
		}
	}
	// Seed before the counter migration so its real backfill processes the same
	// payload-bearing rows as the subsequent recompute measurement.
	started := time.Now()
	migrationTestUpTo(t, migrationDSN, 101)
	backfill := time.Since(started)
	assertTranscriptStorageUsageMatchesRecount(t, st, x.AccountID, y.AccountID)
	var payloadRows, payloadMinBytes, payloadMaxBytes int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*), min(octet_length(payload::text)), max(octet_length(payload::text))
		FROM transcript_entries WHERE account_id=$1 AND payload IS NOT NULL`, x.AccountID).
		Scan(&payloadRows, &payloadMinBytes, &payloadMaxBytes); err != nil {
		t.Fatal(err)
	}
	if payloadRows != (n-1)/payloadEvery+1 || payloadMinBytes < 2048 || payloadMaxBytes > 8192 {
		t.Fatalf("unexpected payload fixture: rows=%d min_bytes=%d max_bytes=%d", payloadRows, payloadMinBytes, payloadMaxBytes)
	}

	disableCounterTriggers := func(tx pgx.Tx) {
		t.Helper()
		if _, err := tx.Exec(ctx, `
			ALTER TABLE transcript_entries DISABLE TRIGGER transcript_entries_storage_usage_insert;
			ALTER TABLE transcript_entries DISABLE TRIGGER transcript_entries_storage_usage_delete;
			ALTER TABLE transcript_entries DISABLE TRIGGER transcript_entries_storage_usage_update;`); err != nil {
			t.Fatal(err)
		}
	}

	// Measure deletes before either account has any rolled-back insertion
	// history. Both accounts have identical seed histories, and both deletes
	// use the same bounded statement and are rolled back after measurement.
	deleteSample := min(n, maxDeleteSample)
	measureDelete := func(principal Principal, disabled bool) time.Duration {
		t.Helper()
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if disabled {
			disableCounterTriggers(tx)
		}
		started := time.Now()
		deleted, err := tx.Exec(ctx, `
			DELETE FROM transcript_entries WHERE account_id=$1 AND id IN (
				SELECT id FROM transcript_entries WHERE account_id=$1 ORDER BY id LIMIT $2
			)`, principal.AccountID, deleteSample)
		elapsed := time.Since(started)
		if err != nil {
			t.Fatal(err)
		}
		if deleted.RowsAffected() != int64(deleteSample) {
			t.Fatalf("delete (counter disabled=%v) affected %d entries, want %d", disabled, deleted.RowsAffected(), deleteSample)
		}
		if !disabled {
			assertTranscriptStorageTimingRecount(t, tx, principal.AccountID)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		return elapsed
	}
	deleteEnabled := measureDelete(x, false)
	deleteDisabled := measureDelete(y, true)
	assertTranscriptStorageUsageMatchesRecount(t, st, x.AccountID, y.AccountID)

	// Give each account the same rolled-back insertion history too, while
	// retaining all unrelated user and foreign-key triggers in the baseline.
	measureInserts := func(principal Principal, disabled bool) time.Duration {
		t.Helper()
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if disabled {
			disableCounterTriggers(tx)
		}
		started := time.Now()
		for i := range insertStatements {
			if _, err := tx.Exec(ctx, `
				INSERT INTO transcript_entries
					(id, account_id, transcript_id, realm_id, recorded_by_agent_id, sequence, role, body)
				VALUES ($1, $2, $2 || '-timing-conversation-1', $3, $4, $5, 'user', repeat('a', $6::integer))`,
				fmt.Sprintf("%s-timing-sample-%d", principal.AccountID, i), principal.AccountID, principal.RealmID, principal.ID, n+i+1, bodyBytes); err != nil {
				t.Fatal(err)
			}
		}
		elapsed := time.Since(started)
		if !disabled {
			assertTranscriptStorageTimingRecount(t, tx, principal.AccountID)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		return elapsed
	}
	insertEnabled := measureInserts(x, false)
	insertDisabled := measureInserts(y, true)
	assertTranscriptStorageUsageMatchesRecount(t, st, x.AccountID, y.AccountID)

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	started = time.Now()
	if err := recomputeTranscriptStorageUsageTx(ctx, tx, x.AccountID); err != nil {
		t.Fatal(err)
	}
	recompute := time.Since(started)
	assertTranscriptStorageTimingRecount(t, tx, x.AccountID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertTranscriptStorageUsageMatchesRecount(t, st, x.AccountID)

	t.Logf("transcript storage timing: entries_per_account=%d conversations_per_account=%d body_bytes=%d payload_rows_per_account=%d payload_min_bytes=%d payload_max_bytes=%d migration_101_payload_backfill=%s insert_statements=%d insert_enabled=%s insert_disabled=%s insert_enabled_per_statement=%s insert_disabled_per_statement=%s payload_bearing_recompute=%s delete_enabled_rows=%d delete_enabled=%s delete_disabled_rows=%d delete_disabled=%s",
		n, conversations, bodyBytes, payloadRows, payloadMinBytes, payloadMaxBytes, backfill,
		insertStatements, insertEnabled, insertDisabled, insertEnabled/insertStatements, insertDisabled/insertStatements,
		recompute, deleteSample, deleteEnabled, deleteSample, deleteDisabled)
}

func assertTranscriptStorageTimingRecount(t *testing.T, tx pgx.Tx, accountID string) {
	t.Helper()
	var mismatches int64
	if err := tx.QueryRow(context.Background(), `
		WITH recount AS (
			SELECT witself_transcript_storage_shard(transcript_id) AS shard,
				sum(witself_transcript_entry_storage_bytes(external_id, role, body, payload, model, reply_to_entry_id, artifacts))::bigint AS retained_bytes
			FROM transcript_entries WHERE account_id=$1 GROUP BY 1
		), stored AS (
			SELECT shard, retained_bytes FROM transcript_storage_usage WHERE account_id=$1
		)
		SELECT count(*) FROM recount FULL OUTER JOIN stored USING (shard)
		WHERE COALESCE(recount.retained_bytes, 0) <> COALESCE(stored.retained_bytes, 0)`, accountID).Scan(&mismatches); err != nil {
		t.Fatal(err)
	}
	if mismatches != 0 {
		t.Fatalf("timing fixture has %d counter shards differing from recount", mismatches)
	}
}
