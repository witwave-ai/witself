package main

import (
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/store"
)

func TestSyntheticShapeValidation(t *testing.T) {
	var defaults seedOptions
	addSeedFlags(flag.NewFlagSet("seed", flag.ContinueOnError), &defaults)
	if defaults.Entries != 330304 || defaults.Transcripts != 2000 || defaults.Mutations != 174212 || defaults.Inputs != 30225 {
		t.Fatalf("unexpected default shape: %+v", defaults)
	}
	if err := validateSeedOptions(defaults); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*seedOptions)
	}{
		{"empty entries", func(o *seedOptions) { o.Entries = 0 }},
		{"missing tenant", func(o *seedOptions) { o.Transcripts = 2 }},
		{"orphan transcripts", func(o *seedOptions) { o.Entries = o.Transcripts - 1 }},
		{"empty mutations", func(o *seedOptions) { o.Mutations = 0 }},
		{"empty inputs", func(o *seedOptions) { o.Inputs = 0 }},
		{"too few supporting rows", func(o *seedOptions) { o.OtherRows = 2 }},
		{"too many supporting rows", func(o *seedOptions) { o.OtherRows = 10001 }},
		{"input budget", func(o *seedOptions) { o.Inputs = o.OtherRows*10000 + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := defaults
			tc.edit(&opts)
			if err := validateSeedOptions(opts); err == nil {
				t.Fatal("invalid synthetic shape accepted")
			}
		})
	}
}

func TestSyntheticCoversExportIndexMigrationTables(t *testing.T) {
	// These are the concrete migrations exercised by the real rehearsal. An
	// index over an empty lifecycle table is not representative, even when the
	// three largest tables have realistic byte counts.
	program := syntheticEntriesSQL + syntheticCurationSQL + syntheticMutationsSQL + syntheticSupportingSQL + syntheticGraphSQL + syntheticLifecycleSQL + syntheticEmailSQL
	// The parent/entry tables are inserted by seedSynthetic's bounded setup.
	seeded := map[string]bool{"operators": true, "transcript_conversations": true}
	for _, match := range regexp.MustCompile(`INSERT INTO ([a-z_]+)`).FindAllStringSubmatch(program, -1) {
		seeded[match[1]] = true
	}
	for _, filename := range []string{"0097_index_account_export_order_hot.sql", "0098_index_account_export_order.sql"} {
		migration, err := fs.ReadFile(store.MigrationFiles(), "migrations/"+filename)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range regexp.MustCompile(`CREATE INDEX CONCURRENTLY [a-z_]+ ON ([a-z_]+)`).FindAllStringSubmatch(string(migration), -1) {
			if !seeded[match[1]] {
				t.Errorf("migration %s indexes unseeded table %s", filename, match[1])
			}
		}
	}
}

func TestSyntheticMeasurementsContainOnlyNumericAggregates(t *testing.T) {
	encoded, err := json.Marshal(seedMeasurements{Entries: 2000, HeapBytes: 4096, PayloadRows: 1200})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	for key, value := range result {
		if _, ok := value.(float64); !ok {
			t.Fatalf("non-numeric measurement %s", key)
		}
	}
	if !strings.Contains(string(encoded), `"column_bytes":`) || !strings.Contains(string(encoded), `"all_column_bytes":`) {
		t.Fatal("nullable SQL-sum and null-aware sum must be independently reported")
	}
}

// The unit contract catches a narrowed key expression without Docker; the same
// invariant is checked against seeded database values by the opt-in smoke test.
func TestSyntheticIdentifiersUseProductionWidth(t *testing.T) {
	if !strings.Contains(syntheticIDSQL, "lpad(to_hex(n),16,'0')") {
		t.Fatal("synthetic identifier suffix must contain sixteen base32 characters")
	}
	for name, program := range map[string]string{
		"entries": syntheticEntriesSQL, "curation": syntheticCurationSQL,
		"mutations": syntheticMutationsSQL, "supporting": syntheticSupportingSQL,
		"graph": syntheticGraphSQL, "lifecycle": syntheticLifecycleSQL, "email": syntheticEmailSQL,
	} {
		if regexp.MustCompile(`'(?:sa|sr|sg|st|se)_[^']*'`).MatchString(program) {
			t.Errorf("%s retains a narrow synthetic identifier", name)
		}
	}
	for _, expression := range []string{
		"pg_temp.rehearsal_id('ent_',s.n)",
		"pg_temp.rehearsal_id('ent_',s.n-$3::bigint)",
		"pg_temp.rehearsal_id('trn_',s.t)",
	} {
		if !strings.Contains(syntheticEntriesSQL, expression) {
			t.Error("transcript identity no longer uses the production-width generator")
		}
	}
	for _, program := range []string{syntheticCurationSQL, syntheticMutationsSQL} {
		for _, prefix := range []string{"acc_", "realm_", "agent_"} {
			if !strings.Contains(program, "pg_temp.rehearsal_id('"+prefix+"',1)") {
				t.Error("curation scope no longer uses the production-width generator")
			}
		}
	}
}

func TestSyntheticEmailRealmLabelsMatchScope(t *testing.T) {
	// Email realms occupy a disjoint range from the supporting graph. The
	// schema requires each canonical label to be the body of that exact ID.
	if strings.Contains(syntheticEmailSQL, "pg_temp.rehearsal_id('',n)") ||
		!strings.Contains(syntheticEmailSQL, "pg_temp.rehearsal_id('realm_',n+$1::bigint)") ||
		!strings.Contains(syntheticEmailSQL, "pg_temp.rehearsal_id('',n+$1::bigint)") {
		t.Fatal("canonical email labels and realm scope must use the same disjoint ID range")
	}
}

func syntheticIdentifierWidthVerification(t *testing.T) string {
	t.Helper()
	// Each identifier is compared inside PostgreSQL. Only the boolean result
	// leaves the disposable database, including when verification fails.
	var queries []string
	for _, table := range []struct {
		name    string
		columns map[string]string
	}{
		{"accounts", map[string]string{"id": "acc"}},
		{"realms", map[string]string{"id": "realm", "account_id": "acc"}},
		{"agents", map[string]string{"id": "agent", "realm_id": "realm"}},
		{"transcript_conversations", map[string]string{"id": "trn", "account_id": "acc", "realm_id": "realm", "owner_agent_id": "agent"}},
		{"transcript_entries", map[string]string{"id": "ent", "reply_to_entry_id": "ent", "account_id": "acc", "transcript_id": "trn", "realm_id": "realm", "recorded_by_agent_id": "agent"}},
		{"memory_curation_requests", map[string]string{"id": "mcrq", "account_id": "acc", "realm_id": "realm", "owner_id": "agent", "actor_id": "agent"}},
		{"memory_curation_runs", map[string]string{"id": "mrun", "request_id": "mcrq", "account_id": "acc", "realm_id": "realm", "owner_id": "agent", "actor_id": "agent"}},
		{"memory_curation_run_inputs", map[string]string{"run_id": "mrun", "transcript_id": "trn", "account_id": "acc", "realm_id": "realm", "owner_id": "agent"}},
		{"memory_curation_actions", map[string]string{"id": "mact", "run_id": "mrun", "account_id": "acc", "realm_id": "realm", "owner_id": "agent"}},
		{"memory_curation_mutations", map[string]string{"id": "mcmu", "request_id": "mcrq", "account_id": "acc", "realm_id": "realm", "owner_id": "agent", "actor_id": "agent"}},
		{"fact_assertions", map[string]string{"fact_id": "fact", "account_id": "acc", "realm_id": "realm", "asserted_by_agent_id": "agent"}},
		{"memory_evidence", map[string]string{"memory_id": "mem", "account_id": "acc", "realm_id": "realm", "owner_id": "agent"}},
		{"agent_message_deliveries", map[string]string{"message_id": "msg", "account_id": "acc", "realm_id": "realm", "recipient_agent_id": "agent"}},
		{"agent_email_deliveries", map[string]string{"message_id": "emsg", "account_id": "acc", "realm_id": "realm", "mailbox_id": "emb", "owner_agent_id": "agent"}},
	} {
		for column, prefix := range table.columns {
			queries = append(queries, "SELECT "+column+" AS value,'"+prefix+"' AS prefix FROM "+table.name+" WHERE "+column+" IS NOT NULL")
		}
	}
	query := "WITH identifiers AS (" + strings.Join(queries, " UNION ALL ") + ") SELECT count(*)>0 AND bool_and(value ~ ('^'||prefix||'_[a-z2-7]{16}$')) FROM identifiers"
	path := filepath.Join(t.TempDir(), "verify-identifier-widths.sql")
	if err := os.WriteFile(path, []byte(query), 0600); err != nil {
		t.Fatal("write identifier-width verification")
	}
	return path
}
