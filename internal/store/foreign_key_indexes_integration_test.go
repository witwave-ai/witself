package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/testenv"
)

// A supporting index must be usable and cover every referencing column in its
// leading keys. Equality lookups may use those keys in any order. Included
// columns, expressions and predicates excluding possible references do not count.
const unsupportedForeignKeysSQL = `
 SELECT c.conname, t.relname, pg_get_constraintdef(c.oid)
 FROM pg_constraint c
 JOIN pg_class t ON t.oid = c.conrelid
 WHERE c.contype = 'f' AND c.connamespace = current_schema()::regnamespace
 AND NOT EXISTS (
   SELECT 1 FROM pg_index i JOIN pg_class idx ON idx.oid = i.indexrelid
   JOIN pg_am am ON am.oid = idx.relam
   WHERE i.indrelid = c.conrelid AND i.indisvalid AND i.indisready AND i.indislive
     AND am.amname = 'btree' AND i.indnkeyatts >= cardinality(c.conkey)
     AND ARRAY(SELECT k FROM unnest(i.indkey::smallint[]) WITH ORDINALITY x(k, n)
               WHERE n <= cardinality(c.conkey) ORDER BY k)
         = ARRAY(SELECT k FROM unnest(c.conkey) k ORDER BY k)
     AND (i.indpred IS NULL OR
          EXISTS (SELECT 1 FROM pg_attribute a
                  WHERE a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
                    AND pg_get_expr(i.indpred, i.indrelid) = format('(%I IS NOT NULL)', a.attname)))
 )
 ORDER BY t.relname, c.conname`

func TestForeignKeysHaveSupportingIndexes(t *testing.T) {
	checkForeignKeyIndexAllowlistCitations(t)
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	rows, err := st.pool.Query(context.Background(), unsupportedForeignKeysSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	allowlist := foreignKeyIndexAllowlist
	seen := make(map[string]bool)
	for rows.Next() {
		var constraint, table, definition string
		if err := rows.Scan(&constraint, &table, &definition); err != nil {
			t.Fatal(err)
		}
		key := table + "." + constraint
		if _, allowed := allowlist[key]; allowed {
			seen[key] = true
			continue
		}
		t.Errorf("foreign key %s on %s lacks a supporting index: add an index or an allowlist entry with a reason (%s)", constraint, table, definition)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for key := range allowlist {
		if !seen[key] {
			t.Errorf("allowlist %s is stale: remove it or review the changed constraint/index", key)
		}
	}
	var total int
	if err := st.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_constraint
 WHERE contype = 'f' AND connamespace = current_schema()::regnamespace`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("foreign key catalog guard inspected no constraints")
	}
	t.Logf("audited %d foreign keys with %d closed exceptions", total, len(allowlist))

}

// This is a proportionality check through the access path, not an elapsed-time
// threshold: an RI lookup must use the reply index even with another account's
// much larger history present. Leave the planner settings at their defaults.
func TestTranscriptEntryDeleteUsesReplyIndexPostgres(t *testing.T) {
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
 INSERT INTO accounts (id, display_name, status)
 VALUES ('acc_fk_a', 'FK delete A', 'active'), ('acc_fk_b', 'FK delete B', 'active');
 INSERT INTO realms (id, account_id, name)
 SELECT 'realm_fk_' || suffix, 'acc_fk_' || suffix, 'FK delete ' || suffix
 FROM (VALUES ('a'), ('b')) AS fixture(suffix);
 INSERT INTO agents (id, realm_id, name)
 SELECT 'agent_fk_' || suffix, 'realm_fk_' || suffix, 'FK delete ' || suffix
 FROM (VALUES ('a'), ('b')) AS fixture(suffix);
 INSERT INTO transcript_conversations (id, account_id, realm_id, owner_agent_id, next_sequence)
 SELECT 'transcript_fk_' || suffix, 'acc_fk_' || suffix, 'realm_fk_' || suffix,
        'agent_fk_' || suffix, entry_count + 1
 FROM (VALUES ('a', 2000), ('b', 20000)) AS fixture(suffix, entry_count);
 INSERT INTO transcript_entries
   (id, account_id, transcript_id, realm_id, recorded_by_agent_id, sequence, role, body, reply_to_entry_id)
 SELECT 'entry_fk_' || suffix || '_' || n, 'acc_fk_' || suffix, 'transcript_fk_' || suffix,
        'realm_fk_' || suffix, 'agent_fk_' || suffix, n, 'user', 'synthetic FK delete fixture',
        CASE WHEN n % 10 = 0 THEN 'entry_fk_' || suffix || '_' || (n - 1) END
 FROM (VALUES ('a', 2000), ('b', 20000)) AS fixture(suffix, entry_count)
 CROSS JOIN LATERAL generate_series(1, entry_count) AS entries(n);
 ANALYZE transcript_entries;`); err != nil {
		t.Fatal(err)
	}

	var rawPlan []byte
	if err := st.pool.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON)
 SELECT 1 FROM transcript_entries WHERE reply_to_entry_id = $1`, "entry_fk_a_9").Scan(&rawPlan); err != nil {
		t.Fatal(err)
	}
	type planNode struct {
		NodeType   string     `json:"Node Type"`
		IndexName  string     `json:"Index Name"`
		IndexCond  string     `json:"Index Cond"`
		ActualRows int        `json:"Actual Rows"`
		Plans      []planNode `json:"Plans"`
	}
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(rawPlan, &plans); err != nil {
		t.Fatal(err)
	}
	var usesReplyIndex func(planNode) bool
	usesReplyIndex = func(node planNode) bool {
		if node.IndexName == "transcript_entries_by_reply_to" &&
			strings.Contains(node.IndexCond, "reply_to_entry_id") &&
			(node.NodeType == "Index Scan" || node.NodeType == "Index Only Scan" || node.NodeType == "Bitmap Index Scan") {
			return true
		}
		for _, child := range node.Plans {
			if usesReplyIndex(child) {
				return true
			}
		}
		return false
	}
	if len(plans) != 1 || !usesReplyIndex(plans[0].Plan) {
		t.Fatal("RI-equivalent lookup must use transcript_entries_by_reply_to; add or restore the reply-to index")
	}
	if plans[0].Plan.ActualRows != 1 {
		t.Fatalf("RI lookup returned %d rows, want 1", plans[0].Plan.ActualRows)
	}

	tx, err := st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	deleted, err := tx.Exec(ctx, `DELETE FROM transcript_entries WHERE account_id = $1`, "acc_fk_a")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.RowsAffected() != 2000 {
		t.Fatalf("deleted %d entries, want 2000", deleted.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var remainingA, remainingB, repliesB int
	if err := st.pool.QueryRow(ctx, `SELECT
 count(*) FILTER (WHERE account_id = 'acc_fk_a'),
 count(*) FILTER (WHERE account_id = 'acc_fk_b'),
 count(*) FILTER (WHERE account_id = 'acc_fk_b' AND reply_to_entry_id IS NOT NULL)
 FROM transcript_entries`).Scan(&remainingA, &remainingB, &repliesB); err != nil {
		t.Fatal(err)
	}
	if remainingA != 0 || remainingB != 20000 || repliesB != 2000 {
		t.Fatalf("after account A delete: A=%d B=%d B replies=%d, want 0/20000/2000", remainingA, remainingB, repliesB)
	}
}
