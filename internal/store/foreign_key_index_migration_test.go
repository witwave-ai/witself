package store

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/witwave-ai/witself/internal/testenv"
)

type foreignKeyMigrationIndex struct {
	name      string
	table     string
	columns   []string
	predicate string
}

func foreignKeyMigrationIndexes(t *testing.T, file string) []foreignKeyMigrationIndex {
	t.Helper()
	migration, err := migrationsFS.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(migration)
	if strings.Count(text, "-- +goose NO TRANSACTION") != 1 {
		t.Fatalf("%s must have exactly one NO TRANSACTION directive", file)
	}
	_, afterUp, ok := strings.Cut(text, "-- +goose Up")
	if !ok {
		t.Fatalf("%s has no Up section", file)
	}
	up, down, ok := strings.Cut(afterUp, "-- +goose Down")
	if !ok {
		t.Fatalf("%s has no Down section", file)
	}
	statements := func(section string) []string {
		section = regexp.MustCompile(`(?m)--[^\n]*`).ReplaceAllString(section, "")
		var result []string
		for _, statement := range strings.Split(section, ";") {
			if normalized := strings.Join(strings.Fields(statement), " "); normalized != "" {
				result = append(result, normalized)
			}
		}
		return result
	}
	upStatements := statements(up)
	if len(upStatements) == 0 || len(upStatements)%2 != 0 {
		t.Fatalf("%s Up must contain only paired concurrent index drops and creates", file)
	}
	createPattern := regexp.MustCompile(`^CREATE INDEX CONCURRENTLY ([a-z_]+) ON ([a-z_]+) \(([a-z_, ]+)\)(?: WHERE ([a-z_]+ IS NOT NULL))?$`)
	var indexes []foreignKeyMigrationIndex
	seen := make(map[string]bool)
	for i := 0; i < len(upStatements); i += 2 {
		match := createPattern.FindStringSubmatch(upStatements[i+1])
		if match == nil {
			t.Fatalf("%s Up statement %d must create an ordinary index CONCURRENTLY", file, i+2)
		}
		index := foreignKeyMigrationIndex{name: match[1], table: match[2], predicate: match[4]}
		for _, column := range strings.Split(match[3], ",") {
			index.columns = append(index.columns, strings.TrimSpace(column))
		}
		if upStatements[i] != "DROP INDEX CONCURRENTLY IF EXISTS "+index.name {
			t.Fatalf("index %s must be dropped CONCURRENTLY IF EXISTS immediately before its create", index.name)
		}
		if seen[index.name] {
			t.Fatalf("%s creates duplicate index %s", file, index.name)
		}
		seen[index.name] = true
		indexes = append(indexes, index)
	}
	downStatements := statements(down)
	if len(downStatements) != len(indexes) {
		t.Fatalf("%s Down has %d statements, want %d concurrent index drops", file, len(downStatements), len(indexes))
	}
	for _, index := range indexes {
		want := "DROP INDEX CONCURRENTLY IF EXISTS " + index.name
		if !slices.Contains(downStatements, want) {
			t.Fatalf("%s Down must drop %s CONCURRENTLY IF EXISTS", file, index.name)
		}
	}
	return indexes
}

const foreignKeyHotMigration = "0099_index_foreign_key_delete_paths_hot.sql"
const foreignKeyRemainingMigration = "0100_index_foreign_key_delete_paths.sql"

func TestMigration99And100ConcurrentIndexContract(t *testing.T) {
	hot := foreignKeyMigrationIndexes(t, foreignKeyHotMigration)
	remaining := foreignKeyMigrationIndexes(t, foreignKeyRemainingMigration)
	// Keep the hot definitions independent of parsing: moving any into 0100
	// would silently lose its checkpoint even if both files still migrate.
	required := []foreignKeyMigrationIndex{
		{"transcript_entries_by_reply_to", "transcript_entries", []string{"reply_to_entry_id"}, "reply_to_entry_id IS NOT NULL"},
		{"memory_curation_mutations_by_run_request", "memory_curation_mutations", []string{"run_id", "request_id"}, ""},
		{"memory_curation_mutations_by_request_scope", "memory_curation_mutations", []string{"request_id", "account_id", "realm_id", "owner_kind", "owner_id"}, ""},
	}
	if len(hot) != len(required) || len(remaining) != 35 {
		t.Fatalf("migration index counts = %d hot, %d remaining; want 3, 35", len(hot), len(remaining))
	}
	for _, want := range required {
		position := slices.IndexFunc(hot, func(got foreignKeyMigrationIndex) bool { return got.name == want.name })
		if position < 0 {
			t.Fatalf("migration 0099 omits required hot index %s", want.name)
		}
		got := hot[position]
		if got.table != want.table || !slices.Equal(got.columns, want.columns) || got.predicate != want.predicate {
			t.Fatalf("migration 0099 index %s has unexpected table, columns, or predicate", want.name)
		}
	}
	seen := make(map[string]bool)
	for _, index := range append(hot, remaining...) {
		if seen[index.name] || index.name == "memory_curation_run_inputs_by_run_scope" {
			t.Fatalf("migrations must not rebuild duplicate index %s", index.name)
		}
		seen[index.name] = true
	}
}

func TestMigration99And100ForeignKeyIndexesUpDownRetryPostgres(t *testing.T) {
	hot := foreignKeyMigrationIndexes(t, foreignKeyHotMigration)
	remaining := foreignKeyMigrationIndexes(t, foreignKeyRemainingMigration)
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	migrationTestUpTo(t, dsn, 98)
	assertMigrationTestVersion(t, dsn, 98)
	if _, err := st.pool.Exec(ctx, `
INSERT INTO accounts(id,display_name,status) VALUES ('acc_fk_retry','index retry test','active');
INSERT INTO realms(id,account_id,name) VALUES ('realm_fk_retry','acc_fk_retry','index retry test');
INSERT INTO agents(id,realm_id,name) VALUES ('agent_fk_retry','realm_fk_retry','index retry test');
INSERT INTO transcript_conversations(id,account_id,realm_id,owner_agent_id,next_sequence)
VALUES ('transcript_fk_retry','acc_fk_retry','realm_fk_retry','agent_fk_retry',3);
INSERT INTO transcript_entries(id,account_id,transcript_id,realm_id,recorded_by_agent_id,sequence,role,body)
SELECT 'entry_fk_retry_'||n,'acc_fk_retry','transcript_fk_retry','realm_fk_retry','agent_fk_retry',n,'user','index retry test'
FROM generate_series(1,2) n;`); err != nil {
		t.Fatal(err)
	}
	// A failed concurrent unique build leaves a real invalid catalog entry,
	// reproducing the retry condition without manually changing pg_catalog.
	_, err := st.pool.Exec(ctx, `CREATE UNIQUE INDEX CONCURRENTLY transcript_entries_by_reply_to ON transcript_entries (account_id)`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatal("retry fixture must fail its concurrent unique index build with a uniqueness violation")
	}
	var valid bool
	if err := st.pool.QueryRow(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid=to_regclass('transcript_entries_by_reply_to')`).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Fatal("retry fixture did not leave an invalid concurrent index")
	}
	assertIndexes := func(indexes []foreignKeyMigrationIndex) {
		t.Helper()
		for _, index := range indexes {
			var valid, ready bool
			var columns []string
			var predicate, method string
			if err := st.pool.QueryRow(ctx, `
SELECT i.indisvalid,i.indisready,am.amname,
 ARRAY(SELECT pg_get_indexdef(i.indexrelid,n,true) FROM generate_series(1,i.indnkeyatts) n),
 COALESCE(pg_get_expr(i.indpred,i.indrelid),'')
FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am am ON am.oid=c.relam
WHERE i.indexrelid=to_regclass($1) AND i.indrelid=to_regclass($2)`, index.name, index.table).Scan(&valid, &ready, &method, &columns, &predicate); err != nil {
				t.Fatalf("inspect index %s: %v", index.name, err)
			}
			if !valid || !ready || method != "btree" || !slices.Equal(columns, index.columns) || strings.Trim(predicate, "()") != index.predicate {
				t.Errorf("index %s has unexpected validity, readiness, access method, columns, or predicate", index.name)
			}
		}
	}
	assertAbsent := func(indexes []foreignKeyMigrationIndex) {
		t.Helper()
		for _, index := range indexes {
			var exists bool
			if err := st.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, index.name).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists {
				t.Errorf("index %s exists outside its migration version", index.name)
			}
		}
	}
	migrationTestUpTo(t, dsn, 99)
	assertMigrationTestVersion(t, dsn, 99)
	assertIndexes(hot)
	assertAbsent(remaining)
	migrationTestUpTo(t, dsn, 100)
	assertMigrationTestVersion(t, dsn, 100)
	assertIndexes(hot)
	assertIndexes(remaining)
	migrationTestDownTo(t, dsn, 99)
	assertMigrationTestVersion(t, dsn, 99)
	assertIndexes(hot)
	assertAbsent(remaining)
	migrationTestDownTo(t, dsn, 98)
	assertMigrationTestVersion(t, dsn, 98)
	assertAbsent(hot)
	assertAbsent(remaining)
	migrationTestUpTo(t, dsn, 100)
	assertMigrationTestVersion(t, dsn, 100)
	assertIndexes(hot)
	assertIndexes(remaining)
}
