package store

import (
	"embed"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestExportImportColumnContract pins the seam between ExportAccount
// (jsonb_build_object queries) and ImportAccount (importColumns allowlist).
// A future schema change that adds a column to a table has three possible
// endings:
//
//  1. Exporter emits it, importer's allowlist accepts it — round-trip works.
//  2. Exporter emits it, importer's allowlist rejects it — every export
//     already in R2 becomes unimportable (400 "unknown column").
//  3. Exporter forgets it — old data silently disappears on restore.
//
// This test forces (2) and (3) to fail loudly and immediately. If it
// breaks, the fix is to update BOTH sides in the same commit.
func TestExportImportColumnContract(t *testing.T) {
	exportedByTable, err := parseExportedColumns()
	if err != nil {
		t.Fatalf("parse export.go queries: %v", err)
	}

	if len(exportedByTable) == 0 {
		t.Fatal("no jsonb_build_object queries found in export.go — parser is broken")
	}

	for table, exported := range exportedByTable {
		allowed, ok := importColumns[table]
		if !ok {
			t.Errorf("table %q is exported but has no importColumns allowlist entry", table)
			continue
		}
		// Every column the exporter emits must be in the importer's
		// allowlist. If not, an archive built from a fresh export will be
		// refused on restore.
		for col := range exported {
			if !allowed[col] {
				t.Errorf("table %q: column %q is emitted by ExportAccount but not in importColumns — restore would refuse this archive", table, col)
			}
		}
		// Every column in the allowlist must be one the exporter emits.
		// A stale extra entry doesn't break anything today, but it says
		// the two sides disagree about the shape — and disagreement is
		// exactly what this contract test exists to catch.
		for col := range allowed {
			if !exported[col] {
				t.Errorf("table %q: column %q is in importColumns but ExportAccount never emits it — stale allowlist entry, drift", table, col)
			}
		}
	}

	// A table in importColumns without an exporter query is either an
	// unimplemented export path or a stale allowlist entry. Either way,
	// something is inconsistent.
	for table := range importColumns {
		if _, ok := exportedByTable[table]; !ok {
			t.Errorf("importColumns names table %q but ExportAccount does not emit it", table)
		}
	}
}

// TestExportedColumnsCoverSchemaBase pins the FLOOR: every column present
// in the base migration for a table the arc restores must appear in the
// exporter (and therefore, via the contract test above, in the importer).
// Additive migrations that add new columns will make this test fail; the
// operator either adds the column to the exporter or, if the column is
// intentionally not restored, adds it to omittedColumns below with a
// justification. This is the "quiet regression" defense: a future
// migration cannot silently make a column drop out of restores.
func TestExportedColumnsCoverSchemaBase(t *testing.T) {
	schemaColumns, err := parseSchemaColumns()
	if err != nil {
		t.Fatalf("parse migrations: %v", err)
	}
	exportedByTable, err := parseExportedColumns()
	if err != nil {
		t.Fatalf("parse export.go: %v", err)
	}

	// Columns we deliberately do NOT export/restore, with the reason.
	// Anything added here needs an explicit call-out — it's a policy
	// decision, not an oversight.
	omittedColumns := map[string]map[string]string{
		"memory_versions": {
			"search_document": "generated full-text index is rebuilt from content on import",
		},
	}

	for table := range importColumns { // scope: tables the arc restores
		schema, ok := schemaColumns[table]
		if !ok {
			// Not in the base migrations — probably added by a later
			// migration whose CREATE TABLE this parser missed. Skip
			// rather than flag: the goal here is to catch missing
			// coverage of the CORE tables.
			continue
		}
		exported := exportedByTable[table]
		omitted := omittedColumns[table]
		for col := range schema {
			if exported[col] || omitted[col] != "" {
				continue
			}
			t.Errorf("table %q: schema column %q is not exported and has no omission justification — add it to jsonb_build_object or record it in omittedColumns with why", table, col)
		}
	}
}

func TestParseSchemaColumnsCoversMultiColumnAlter(t *testing.T) {
	schemaColumns, err := parseSchemaColumns()
	if err != nil {
		t.Fatalf("parse migrations: %v", err)
	}

	// Migration 0030 adds these four columns in one comma-separated ALTER TABLE
	// statement. Missing any column here would let the archive coverage test
	// silently overlook a current or future multi-column schema change.
	want := []string{
		"deleted_curation_run_count",
		"deleted_curation_action_count",
		"deleted_curation_input_count",
		"deleted_curation_mutation_count",
	}
	for _, column := range want {
		if !schemaColumns["memories"][column] {
			t.Errorf("migration 0030 column memories.%s was not parsed", column)
		}
	}
}

//go:embed export.go
var exportGoFS embed.FS

// parseExportedColumns walks the jsonb_build_object(...) calls in
// export.go and returns the set of column names emitted per table. It's
// deliberately regex-based (not a full Go parser) so it catches the same
// shape a human reviewer would look for, and stays robust as long as
// export.go keeps the same idiom.
func parseExportedColumns() (map[string]map[string]bool, error) {
	src, err := exportGoFS.ReadFile("export.go")
	if err != nil {
		return nil, err
	}
	text := string(src)

	// Split into per-table blocks by finding each querySource entry.
	// A block looks like:
	//   &querySource{tx: tx, table: "accounts", q: `
	//       SELECT jsonb_build_object(
	//         'id', id, 'is_default', is_default, ...
	//       ) FROM ...`, arg: accountID},
	tableRe := regexp.MustCompile(`table:\s*"([a-z_]+)"[\s\S]*?jsonb_build_object\(([\s\S]*?)\)`)
	quotedFieldRe := regexp.MustCompile(`'([a-z][a-z0-9_]*)'`)

	out := map[string]map[string]bool{}
	for _, m := range tableRe.FindAllStringSubmatch(text, -1) {
		table := m[1]
		body := m[2]
		cols := map[string]bool{}
		for _, mm := range quotedFieldRe.FindAllStringSubmatch(body, -1) {
			cols[mm[1]] = true
		}
		if len(cols) == 0 {
			return nil, notFoundError("no columns parsed for " + table)
		}
		out[table] = cols
	}
	return out, nil
}

//go:embed migrations
var migrationsForContract embed.FS

// parseSchemaColumns walks the embedded migrations and returns the union
// of columns per table across CREATE TABLE and ALTER TABLE ... ADD COLUMN
// statements. This is not a SQL parser — it deliberately handles the
// idioms the migrations happen to use, and would need updates if a
// migration introduces a fundamentally new shape.
func parseSchemaColumns() (map[string]map[string]bool, error) {
	entries, err := migrationsForContract.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	createTableRe := regexp.MustCompile(`(?is)create\s+table\s+(?:if\s+not\s+exists\s+)?([a-z_]+)\s*\(([^;]+?)\)\s*;`)
	alterTableRe := regexp.MustCompile(`(?is)alter\s+table\s+([a-z_]+)\s+([^;]+);`)
	addColumnRe := regexp.MustCompile(`(?i)\badd\s+column\s+(?:if\s+not\s+exists\s+)?([a-z_]+)\b`)
	// A column definition line inside CREATE TABLE begins with a name.
	// Filter out constraint lines (PRIMARY KEY, UNIQUE, CHECK, etc.).
	colHeadRe := regexp.MustCompile(`(?im)^\s*([a-z_]+)\s+[a-z]`)

	out := map[string]map[string]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := migrationsForContract.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		src := string(raw)

		for _, m := range createTableRe.FindAllStringSubmatch(src, -1) {
			table := m[1]
			body := m[2]
			if out[table] == nil {
				out[table] = map[string]bool{}
			}
			for _, line := range strings.Split(body, ",") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				head := colHeadRe.FindStringSubmatch(line + " ")
				if head == nil {
					continue
				}
				name := head[1]
				lower := strings.ToLower(name)
				// Skip SQL constraint keywords the regex will accidentally
				// pick up as "column names".
				if isSQLConstraintKeyword(lower) {
					continue
				}
				out[table][name] = true
			}
		}
		for _, alter := range alterTableRe.FindAllStringSubmatch(src, -1) {
			table := alter[1]
			for _, addition := range addColumnRe.FindAllStringSubmatch(alter[2], -1) {
				col := addition[1]
				if out[table] == nil {
					out[table] = map[string]bool{}
				}
				out[table][col] = true
			}
		}
	}
	return out, nil
}

func isSQLConstraintKeyword(s string) bool {
	switch s {
	case "primary", "unique", "check", "foreign", "constraint", "exclude", "not",
		"references", "on", "deferrable":
		return true
	}
	return false
}

type notFoundError string

func (e notFoundError) Error() string { return string(e) }
