package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/testenv"
)

func exportStreamingSources(t *testing.T) map[string]*querySource {
	t.Helper()
	source, err := exportGoFS.ReadFile("export.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`&querySource\{tx:\s*tx,\s*table:\s*"([a-z_]+)",\s*q:\s*` + "`" + `([\s\S]*?)` + "`" + `,\s*arg:\s*accountID(?:,\s*keyset:\s*\[\]string\{([^}]*)\})?\}`)
	out := map[string]*querySource{}
	for _, m := range re.FindAllStringSubmatch(string(source), -1) {
		src := &querySource{table: m[1], q: m[2]}
		for _, key := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[3], -1) {
			src.keyset = append(src.keyset, key[1])
		}
		out[m[1]] = src
	}
	if len(out) != len(importColumns) {
		t.Fatalf("parsed %d queries, expected %d", len(out), len(importColumns))
	}
	return out
}

func TestMigration97And98ExportIndexesPostgres(t *testing.T) {
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 98)
	for _, filename := range []string{
		"migrations/0097_index_account_export_order_hot.sql",
		"migrations/0098_index_account_export_order.sql",
	} {
		migration, err := migrationsFS.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`CREATE INDEX CONCURRENTLY (\w+) ON (\w+)`).FindAllStringSubmatch(string(migration), -1) {
			var valid, ready bool
			if err := st.pool.QueryRow(ctx, `SELECT indisvalid,indisready FROM pg_index WHERE indexrelid=to_regclass($1) AND indrelid=to_regclass($2)`, m[1], m[2]).Scan(&valid, &ready); err != nil {
				t.Fatal(err)
			}
			if !valid || !ready {
				t.Fatalf("index %s is not valid and ready", m[1])
			}
		}
	}
	// Empty tables favor seq scans; disabling them checks that each direct
	// query has an indexed path without a full sort, not that the planner always
	// chooses it. Only these seven tables may incrementally sort an index suffix.
	incrementalSortAllowed := map[string]bool{
		"account_events":          true,
		"agent_email_deliveries":  true,
		"agent_email_mailboxes":   true,
		"secret_deks":             true,
		"support_tickets":         true,
		"support_ticket_messages": true,
		"usage_rollups":           true,
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan=off; SET LOCAL enable_bitmapscan=off`); err != nil {
		t.Fatal(err)
	}
	for table, src := range exportStreamingSources(t) {
		q := src.q
		if strings.Contains(q, "ORDER BY") && len(src.keyset) == 0 {
			t.Errorf("%s has no bounded pagination", table)
		}
		if strings.Contains(q, "WITH RECURSIVE") || strings.Contains(q, "JOIN") || !strings.Contains(q, "ORDER BY") {
			continue
		}
		var plan string
		if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+q, "acc_streaming").Scan(&plan); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if strings.Contains(plan, `"Node Type": "Sort"`) || (!incrementalSortAllowed[table] && strings.Contains(plan, `"Node Type": "Incremental Sort"`)) {
			t.Errorf("%s requires sorting: %s", table, plan)
		}
	}
	// The historical proof ordering can tie across mailboxes. Exercise the real
	// source SQL over two full pages plus a tail so the tie-breaker cannot drift.
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE agent_email_retry_canary_arms
   (LIKE agent_email_retry_canary_arms INCLUDING DEFAULTS) ON COMMIT DROP;
   INSERT INTO agent_email_retry_canary_arms
    (account_id,realm_id,mailbox_id,owner_agent_id,challenge_sha256,state,expires_at,accepted_at)
   SELECT 'acc_streaming','realm','mailbox_'||lpad(n::text,4,'0'),'agent',repeat('a',64),
    'accepted','2026-01-02'::timestamptz,'2026-01-01'::timestamptz FROM generate_series(1,513) n;`); err != nil {
		t.Fatal(err)
	}
	proof := exportStreamingSources(t)["agent_email_retry_canary_arms"]
	proof.tx, proof.arg = tx, "acc_streaming"
	for n := 1; n <= 513; n++ {
		row, err := proof.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Mailbox string `json:"mailbox_id"`
		}
		if err := json.Unmarshal(row, &got); err != nil {
			t.Fatal(err)
		}
		if got.Mailbox != fmt.Sprintf("mailbox_%04d", n) {
			t.Fatalf("proof ordering at row %d", n)
		}
	}
	if row, err := proof.Next(ctx); err != nil || row != nil {
		t.Fatalf("proof tail: bytes=%d error=%v", len(row), err)
	}

}

type countedExportSource struct {
	*querySource
	produced int
}

func (s *countedExportSource) Next(ctx context.Context) ([]byte, error) {
	row, err := s.querySource.Next(ctx)
	if row != nil {
		s.produced++
	}
	return row, err
}

func TestExportTranscriptFirstChunkPostgres(t *testing.T) {
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	const total = 2300
	_, err := st.pool.Exec(ctx, `
 INSERT INTO accounts(id,display_name,status) VALUES ('acc_streaming','streaming test','active');
 INSERT INTO realms(id,account_id,name) VALUES ('realm_streaming','acc_streaming','streaming');
 INSERT INTO agents(id,realm_id,name) VALUES ('agent_streaming','realm_streaming','streaming');
 INSERT INTO transcript_conversations(id,account_id,realm_id,owner_agent_id,next_sequence)
 VALUES ('transcript_streaming','acc_streaming','realm_streaming','agent_streaming',2301);
 INSERT INTO transcript_entries(id,account_id,transcript_id,realm_id,recorded_by_agent_id,sequence,role,body)
 SELECT 'entry_'||n,'acc_streaming','transcript_streaming','realm_streaming','agent_streaming',n,'user',repeat('x',32768)
 FROM generate_series(1,2300) n;
 ANALYZE transcript_entries;`)
	if err != nil {
		t.Fatal(err)
	}
	source := exportStreamingSources(t)["transcript_entries"]
	source.arg = "acc_streaming"
	pageQuery, args := source.query()
	planRows, err := st.pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) "+pageQuery, args...)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for planRows.Next() {
		var line string
		if err := planRows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	planRows.Close()
	if err := planRows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("transcript query EXPLAIN (default planner):\n%s", plan.String())
	if strings.Contains(plan.String(), "Sort") || !strings.Contains(plan.String(), "Index Scan") {
		t.Fatalf("transcript plan is not streaming: %s", plan.String())
	}
	tx, err := st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	source.tx = tx
	src := &countedExportSource{querySource: source}
	var archive bytes.Buffer
	flushes := 0
	err = archiveexport.Write(ctx, &archive, archiveexport.Manifest{SchemaVersion: SchemaVersion()}, []archiveexport.RowSource{src}, archiveexport.WriteOptions{Flush: func() error {
		flushes++
		if flushes == 2 && (src.produced == 0 || src.produced >= total) {
			t.Fatalf("first chunk after %d/%d rows", src.produced, total)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if flushes < 3 {
		t.Fatalf("flushes=%d, want manifest and multiple chunks", flushes)
	}
	restored := 0
	_, err = archiveexport.Read(ctx, bytes.NewReader(archive.Bytes()), archiveexport.ImportOptions{CurrentSchema: SchemaVersion(), Row: func(_ string, row []byte) error {
		var entry struct {
			Sequence int `json:"sequence"`
		}
		if err := json.Unmarshal(row, &entry); err != nil {
			return err
		}
		restored++
		if entry.Sequence != restored {
			return fmt.Errorf("sequence=%d, want %d", entry.Sequence, restored)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if restored != total {
		t.Fatalf("restored=%d, want %d", restored, total)
	}
}

func TestExportKeysetBatchesKeepSnapshotAndOrderingPostgres(t *testing.T) {
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	// Synthetic rows isolate keyset boundary behavior from domain write APIs.
	if _, err := st.pool.Exec(ctx, `CREATE TABLE export_rows(account_id text,id integer,group_id integer,created_at timestamptz);
 INSERT INTO export_rows SELECT 'account', n, n/3, '2026-01-01'::timestamptz FROM generate_series(1,768) n;`); err != nil {
		t.Fatal(err)
	}
	for _, recursive := range []bool{false, true} {
		t.Run(fmt.Sprint("recursive=", recursive), func(t *testing.T) {
			tx, err := st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			q := `SELECT jsonb_build_object('id',id) FROM export_rows WHERE account_id=$1 ORDER BY group_id, created_at, id`
			if recursive {
				q = `WITH RECURSIVE source AS (SELECT * FROM export_rows WHERE account_id=$1) SELECT jsonb_build_object('id',id) FROM source ORDER BY group_id, created_at, id`
			}
			src := &querySource{tx: tx, table: "fixture", q: q, arg: "account", keyset: []string{"group_id", "created_at", "id"}}
			for n := 1; n <= 768; n++ {
				row, err := src.Next(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var got struct {
					ID int `json:"id"`
				}
				if err := json.Unmarshal(row, &got); err != nil {
					t.Fatal(err)
				}
				if got.ID != n {
					t.Fatalf("row=%d, want %d", got.ID, n)
				}
				if n == exportRowBatchSize {
					if _, err := st.pool.Exec(ctx, `INSERT INTO export_rows VALUES('account',769,999,$1)`, time.Now()); err != nil {
						t.Fatal(err)
					}
				}
			}
			if row, err := src.Next(ctx); err != nil || row != nil {
				t.Fatalf("snapshot tail: bytes=%d error=%v", len(row), err)
			}
			if _, err := st.pool.Exec(ctx, `DELETE FROM export_rows WHERE id=769`); err != nil {
				t.Fatal(err)
			}
			if row, err := src.Next(ctx); err != nil || row != nil {
				t.Fatal("exhausted source restarted")
			}
		})
	}
	// Canceled pages must return cancellation rather than silently ending rows.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	src := &querySource{tx: tx, table: "fixture", q: `SELECT jsonb_build_object('id',id) FROM export_rows WHERE account_id=$1 ORDER BY id`, arg: "account", keyset: []string{"id"}}
	if _, err := src.Next(canceled); err == nil || err == io.EOF {
		t.Fatal("canceled source succeeded")
	}
}
