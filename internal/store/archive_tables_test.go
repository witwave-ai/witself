package store

import (
	"errors"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestValidateArchiveManifestTablesIsSchemaAwareAndExact(t *testing.T) {
	current := canonicalArchiveTableNamesForSchema(SchemaVersion())
	if err := validateArchiveManifestTables(SchemaVersion(), current); err != nil {
		t.Fatalf("current canonical manifest = %v", err)
	}

	// Schema 13 predates events, support, transcripts, usage, facts, and
	// memories. Preserve the historical migration-order manifest rather than
	// imposing today's exporter order on a legitimate old archive.
	legacy := []string{"accounts", "operators", "tokens", "realms", "agents"}
	if err := validateArchiveManifestTables(13, legacy); err != nil {
		t.Fatalf("schema-13 canonical manifest = %v", err)
	}

	tests := []struct {
		name   string
		schema int
		tables []string
		want   string
	}{
		{
			name: "missing current empty memory stream", schema: SchemaVersion(),
			tables: withoutArchiveTable(current, "memory_evidence"),
			want:   "missing memory_evidence",
		},
		{
			name: "future stream added to old schema", schema: 28,
			tables: append(canonicalArchiveTableNamesForSchema(28), "memories"),
			want:   "unexpected memories",
		},
		{
			name: "unknown stream", schema: SchemaVersion(),
			tables: append(current, "shadow_memories"),
			want:   "unexpected shadow_memories",
		},
		{
			name: "duplicate stream", schema: SchemaVersion(),
			tables: append(current, "memories"),
			want:   `table "memories" more than once`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArchiveManifestTables(tc.schema, tc.tables)
			if err == nil || !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestCanonicalArchiveTablesMatchMigrationsExporterAndImporter(t *testing.T) {
	migrationIntroductions := archiveTableIntroductionsFromMigrations(t)
	registryIntroductions := make(map[string]int, len(canonicalArchiveTables))
	registryOrder := make([]string, 0, len(canonicalArchiveTables))
	for _, table := range canonicalArchiveTables {
		if previous, duplicate := registryIntroductions[table.name]; duplicate {
			t.Fatalf("canonical archive registry repeats %q (schemas %d and %d)",
				table.name, previous, table.introducedSchema)
		}
		registryIntroductions[table.name] = table.introducedSchema
		registryOrder = append(registryOrder, table.name)
	}
	if !reflect.DeepEqual(registryIntroductions, migrationIntroductions) {
		t.Fatalf(
			"canonical archive table introductions drifted from migrations\nregistry: %#v\nmigrations: %#v",
			registryIntroductions, migrationIntroductions,
		)
	}

	exportedByTable, err := parseExportedColumns()
	if err != nil {
		t.Fatalf("parse exporter: %v", err)
	}
	for table := range registryIntroductions {
		if _, ok := exportedByTable[table]; !ok {
			t.Errorf("canonical table %q has no ExportAccount stream", table)
		}
		if _, ok := importColumns[table]; !ok {
			t.Errorf("canonical table %q has no ImportAccount allowlist", table)
		}
	}
	for table := range exportedByTable {
		if _, ok := registryIntroductions[table]; !ok {
			t.Errorf("ExportAccount streams non-canonical table %q", table)
		}
	}
	for table := range importColumns {
		if _, ok := registryIntroductions[table]; !ok {
			t.Errorf("ImportAccount accepts non-canonical table %q", table)
		}
	}

	exporterOrder := exportedArchiveTableOrder(t)
	if !reflect.DeepEqual(exporterOrder, registryOrder) {
		t.Fatalf("ExportAccount table order drifted\nexporter: %v\nregistry: %v",
			exporterOrder, registryOrder)
	}
}

func withoutArchiveTable(tables []string, omitted string) []string {
	out := make([]string, 0, len(tables)-1)
	for _, table := range tables {
		if table != omitted {
			out = append(out, table)
		}
	}
	return out
}

func archiveTableIntroductionsFromMigrations(t *testing.T) map[string]int {
	t.Helper()
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	createTableRE := regexp.MustCompile(`(?im)^\s*create\s+table\s+(?:if\s+not\s+exists\s+)?([a-z_]+)\s*\(`)
	introductions := make(map[string]int)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			t.Fatalf("migration filename %q has no numeric prefix", entry.Name())
		}
		schema, err := strconv.Atoi(prefix)
		if err != nil {
			t.Fatalf("migration filename %q: %v", entry.Name(), err)
		}
		raw, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range createTableRE.FindAllStringSubmatch(string(raw), -1) {
			table := match[1]
			if previous, duplicate := introductions[table]; duplicate {
				t.Fatalf("table %q created in migrations %d and %d", table, previous, schema)
			}
			introductions[table] = schema
		}
	}
	return introductions
}

func exportedArchiveTableOrder(t *testing.T) []string {
	t.Helper()
	raw, err := exportGoFS.ReadFile("export.go")
	if err != nil {
		t.Fatal(err)
	}
	tableRE := regexp.MustCompile(`&querySource\{tx:\s*tx,\s*table:\s*"([a-z_]+)"`)
	matches := tableRE.FindAllStringSubmatch(string(raw), -1)
	order := make([]string, 0, len(matches))
	for _, match := range matches {
		order = append(order, match[1])
	}
	return order
}
