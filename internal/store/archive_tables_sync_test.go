package store

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func TestSyncArchiveRegistryMatchesMigrationColumnsAndOrder(t *testing.T) {
	if err := validateSyncArchiveRegistryContract(syncMigrationsFS); err != nil {
		t.Fatal(err)
	}
}

func TestSyncArchiveRegistryRejectsUnregisteredMigrationTable(t *testing.T) {
	for _, tc := range []struct{ name, sql, want string }{
		{"unregistered table", "CREATE TABLE sync_unregistered (account_id text REFERENCES accounts(id));\n", "unregistered sync table"},
		{"quoted table", `CREATE TABLE "sync_unregistered" (account_id text REFERENCES accounts(id));`, "unregistered sync table"},
		{"same line without prefix", `SELECT 1; CREATE TABLE evidence_extra (account_id text REFERENCES accounts(id));`, "unregistered sync table"},
		{"quoted same line", `SELECT 1; CREATE TABLE "evidence_extra" (account_id text REFERENCES accounts(id));`, "unregistered sync table"},
		{"qualified same line", `SELECT 1; CREATE TABLE public.evidence_extra (account_id text REFERENCES accounts(id));`, "unregistered sync table"},
		{"qualified quoted", `SELECT 1; CREATE TABLE "public"."evidence_extra" (account_id text REFERENCES accounts(id));`, "unregistered sync table"},
		{"escaped identifier", `CREATE TABLE "evidence"";extra" (account_id text);`, "unregistered sync table"},
		{"unlogged if absent", `SELECT 1; CREATE UNLOGGED TABLE IF NOT EXISTS evidence_extra (account_id text);`, "unregistered sync table"},
		{"temporary", `SELECT 1; CREATE TEMPORARY TABLE evidence_extra (account_id text);`, "unregistered sync table"},
		{"temp", `SELECT 1; CREATE TEMP TABLE evidence_extra (account_id text);`, "unregistered sync table"},
		{"comments between keywords", `SELECT 1; CREATE/* ; nested /* ignored */ */TABLE evidence_extra (account_id text);`, "unregistered sync table"},
		{"unregistered added column", "ALTER TABLE sync_incarnations ADD COLUMN unregistered_evidence text;", "column contract"},
		{"multiple added columns", "ALTER TABLE sync_incarnations ADD COLUMN unregistered_evidence text, ADD COLUMN another_evidence text;", errSyncMigrationSQL.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := copiedSyncMigrationFS(t)
			mutated["sync_migrations/0002_unregistered.sql"] = &fstest.MapFile{Data: []byte(tc.sql)}
			err := validateSyncArchiveRegistryContract(mutated)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unregistered schema mutation survived: %v; want %q", err, tc.want)
			}
		})
	}
}

func TestSyncArchiveRegistrySQLLiteralsAndCommentsAreOpaque(t *testing.T) {
	for _, sql := range []string{
		`SELECT $$ ; CREATE TABLE evidence_extra (account_id text); $$;`,
		`SELECT $body$ ; CREATE TABLE evidence_extra (account_id text); $$ $body$;`,
		`SELECT '; CREATE TABLE evidence_extra (account_id text); it''s a string';`,
		`SELECT E'escaped\'; CREATE TABLE evidence_extra (account_id text);';`,
		`SELECT "CREATE TABLE", "semi;colon";`,
		`SELECT 'INTO', "INTO";`,
		// Procedural bodies are classified as opaque, not certified harmless.
		// Their installed effects are checked by the PostgreSQL catalog gate.
		`DO $$ BEGIN EXECUTE 'CREATE TABLE evidence_extra (account_id text)'; END $$;`,
		"SELECT 1; -- CREATE TABLE evidence_extra (account_id text);\nSELECT 2;",
		`/* CREATE TABLE evidence_extra (account_id text); /* nested ; */ */ SELECT 1;`,
	} {
		mutated := copiedSyncMigrationFS(t)
		mutated["sync_migrations/0002_opaque.sql"] = &fstest.MapFile{Data: []byte(sql)}
		if err := validateSyncArchiveRegistryContract(mutated); err != nil {
			t.Fatalf("opaque SQL content changed the registry contract: %v", err)
		}
	}
	// A quoted existing name resolves to the same exact registry identifier.
	mutated := copiedSyncMigrationFS(t)
	file := mutated["sync_migrations/0001_sync_schema.sql"]
	file.Data = []byte(strings.Replace(string(file.Data), "CREATE TABLE sync_incarnations", `CREATE TABLE "sync_incarnations"`, 1))
	if err := validateSyncArchiveRegistryContract(mutated); err != nil {
		t.Fatalf("quoted registered table rejected: %v", err)
	}
}

func TestSyncArchiveRegistrySQLUnsupportedFailsClosed(t *testing.T) {
	for _, sql := range []string{
		`SELECT 'unterminated; CREATE TABLE evidence_extra (account_id text);`,
		`SELECT $$unterminated;`,
		`SELECT $tag$unterminated$$;`,
		`SELECT "unterminated;`,
		`/* unterminated /* nested */`,
		`SELECT E'escape\`,
		`SELECT 'setting-dependent\escape';`,
		`CREATE TABLE (account_id text);`,
		`CREATE TABLE evidence_extra (account_id text;`,
		`CREATE TABLE evidence_extra AS SELECT 1;`,
		`CREATE TABLE IF EXISTS evidence_extra (account_id text);`,
		`CREATE GLOBAL TEMP TABLE evidence_extra (account_id text);`,
		`CREATE TABLE U&"evidence_extra" (account_id text);`,
		`CREATE TABLE evidence_extra (account_id text) INHERITS (accounts);`,
		`DROP TABLE sync_incarnations;`,
		`TRUNCATE TABLE sync_incarnations;`,
		`ALTER SEQUENCE evidence_sequence RESTART;`,
		`SELECT 1 INTO unregistered_evidence;`,
		`SELECT 1 INTO TEMP TABLE unregistered_evidence;`,
		`WITH evidence AS (SELECT 1) SELECT * INTO unregistered_evidence FROM evidence;`,
		`CALL unreviewed_procedure();`,
		`DO LANGUAGE plpgsql $$ BEGIN NULL; END $$;`,
		`DO $$ BEGIN NULL; END $$ UNSUPPORTED;`,
		`UNREVIEWED STATEMENT;`,
	} {
		mutated := copiedSyncMigrationFS(t)
		mutated["sync_migrations/0002_unsupported.sql"] = &fstest.MapFile{Data: []byte(sql)}
		if err := validateSyncArchiveRegistryContract(mutated); !errors.Is(err, errSyncMigrationSQL) || err.Error() != errSyncMigrationSQL.Error() {
			t.Fatalf("unsupported SQL must return the fixed refusal: %v", err)
		}
	}
}

func TestSyncArchiveRegistryRejectsMissingColumnAndFence(t *testing.T) {
	for _, tc := range []struct {
		name, old, replacement, want string
	}{
		{
			name: "unregistered column", old: "publication_proof_ref text,",
			replacement: "publication_proof_ref text,\n    unregistered_evidence text,", want: "column contract",
		},
		{
			name:        "missing evacuation guard",
			old:         "CREATE TRIGGER account_evacuation_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_incarnations",
			replacement: "CREATE TRIGGER missing_fence BEFORE INSERT OR UPDATE OR DELETE ON sync_incarnations",
			want:        "evacuation fence",
		},
		{
			name:        "portable history depends on live admission",
			old:         "CREATE TABLE sync_bundles (",
			replacement: "CREATE TABLE sync_bundles (\n    FOREIGN KEY(account_id,tx_id) REFERENCES sync_tx_admissions(account_id,tx_id),",
			want:        "excluded local state",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := copiedSyncMigrationFS(t)
			file := mutated["sync_migrations/0001_sync_schema.sql"]
			if !strings.Contains(string(file.Data), tc.old) {
				t.Fatal("mutation target missing")
			}
			file.Data = []byte(strings.Replace(string(file.Data), tc.old, tc.replacement, 1))
			err := validateSyncArchiveRegistryContract(mutated)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("registry mutation survived: %v; want %q", err, tc.want)
			}
		})
	}
}

func TestSyncArchiveRegistryExcludesOnlyLocalControlState(t *testing.T) {
	want := []string{"sync_capture", "sync_resource_reservations", "sync_tx_admissions", "sync_tx_touches", "sync_writer_capabilities"}
	got := make([]string, 0, len(syncLocalArchiveExclusions))
	for table, exclusion := range syncLocalArchiveExclusions {
		got = append(got, table)
		if exclusion.reason == "" || exclusion.introducedSyncSchema != 1 {
			t.Errorf("exclusion %q lacks version or reason", table)
		}
		if exclusion.movement != excludedSyncMovementPolicy() {
			t.Errorf("exclusion %q lacks its five movement policies", table)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("local exclusions changed without reviewed policy: %v", got)
	}
	for _, table := range syncArchiveTables {
		if table.movement != portableSyncMovementPolicy() {
			t.Errorf("portable evidence %q lacks preservation/refusal policies", table.name)
		}
		if _, excluded := syncLocalArchiveExclusions[table.name]; excluded {
			t.Errorf("portable evidence %q is also excluded", table.name)
		}
		for _, core := range canonicalArchiveTables {
			if table.name == core.name {
				t.Errorf("sync evidence %q entered core blanket deletion registry", table.name)
			}
		}
	}
}

func TestSyncArchiveManifestRequiresEveryStreamAndCanonicalOrder(t *testing.T) {
	current := syncArchiveTableNamesForSchema(SyncSchemaVersion())
	if err := validateSyncArchiveManifestTables(SyncSchemaVersion(), current); err != nil {
		t.Fatal(err)
	}
	if len(syncArchiveTableNamesForSchema(0)) != 0 {
		t.Fatal("uninstalled sync schema has portable streams")
	}
	for _, table := range current {
		t.Run(table, func(t *testing.T) {
			if err := validateSyncArchiveManifestTables(SyncSchemaVersion(), withoutArchiveTable(current, table)); !errors.Is(err, ErrArchiveContent) {
				t.Fatalf("omitted stream accepted: %v", err)
			}
		})
	}
	for _, tables := range [][]string{
		append(slices.Clone(current), "untrusted_stream_value"),
		append(slices.Clone(current), current[0]),
		nil,
	} {
		err := validateSyncArchiveManifestTables(SyncSchemaVersion(), tables)
		if !errors.Is(err, ErrArchiveContent) || strings.Contains(err.Error(), "untrusted_stream_value") {
			t.Fatalf("invalid stream list error is absent or value-bearing: %v", err)
		}
	}
	reordered := slices.Clone(current)
	reordered[0], reordered[1] = reordered[1], reordered[0]
	if err := validateSyncArchiveManifestTables(SyncSchemaVersion(), reordered); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("noncanonical insertion order accepted: %v", err)
	}
	for _, version := range []int{-1, 0, SyncSchemaVersion() + 1} {
		if err := validateSyncArchiveManifestTables(version, current); !errors.Is(err, ErrArchiveContent) {
			t.Fatalf("unsupported sync version accepted: %v", err)
		}
	}
}

func TestSyncArchiveRowAllowlistRequiresExactScopedColumns(t *testing.T) {
	for _, spec := range syncArchiveTables {
		t.Run(spec.name, func(t *testing.T) {
			row := make(map[string]any, len(spec.columns))
			for _, column := range spec.columns {
				row[column] = nil
			}
			row["account_id"] = "account_expected"
			if err := validateSyncArchiveRow(spec, row, "account_expected"); err != nil {
				t.Fatalf("complete scoped column set refused: %v", err)
			}
			for _, column := range spec.columns {
				saved := row[column]
				delete(row, column)
				if err := validateSyncArchiveRow(spec, row, "account_expected"); !errors.Is(err, ErrArchiveContent) {
					t.Fatalf("omitted %q accepted: %v", column, err)
				}
				row[column] = saved
			}
			row["untrusted_column_value"] = "untrusted_payload_value"
			if err := validateSyncArchiveRow(spec, row, "account_expected"); !errors.Is(err, ErrArchiveContent) || strings.Contains(err.Error(), "untrusted_") {
				t.Fatalf("unknown column error is absent or value-bearing: %v", err)
			}
			delete(row, "untrusted_column_value")
			row["account_id"] = "account_foreign"
			if err := validateSyncArchiveRow(spec, row, "account_expected"); !errors.Is(err, ErrArchiveContent) {
				t.Fatalf("cross-account row accepted: %v", err)
			}
		})
	}
}

type syncMigrationTableContract struct {
	version    int
	columns    []string
	primaryKey []string
	references []syncMigrationReference
}

type syncMigrationReference struct {
	table    string
	deferred bool
}

// validateSyncArchiveRegistryContract accepts an injected filesystem so the
// same contract that gates checked-in DDL can be exercised against mutations.
func validateSyncArchiveRegistryContract(source fs.FS) error {
	tables, ddl, err := syncMigrationTableContracts(source)
	if err != nil {
		return err
	}
	registered := make(map[string]int, len(syncArchiveTables))
	for index, table := range syncArchiveTables {
		if _, duplicate := registered[table.name]; duplicate {
			return fmt.Errorf("duplicate sync archive table %q", table.name)
		}
		registered[table.name] = index
	}
	for name, table := range tables {
		if index, ok := registered[name]; ok {
			spec := syncArchiveTables[index]
			if _, excluded := syncLocalArchiveExclusions[name]; excluded {
				return fmt.Errorf("portable sync evidence %q is also excluded", name)
			}
			if table.version != spec.introducedSyncSchema {
				return fmt.Errorf("sync table %q introduction version differs", name)
			}
			if !reflect.DeepEqual(table.columns, spec.columns) {
				return fmt.Errorf("sync table %q column contract differs: DDL %v registry %v", name, table.columns, spec.columns)
			}
			if !reflect.DeepEqual(table.primaryKey, spec.orderBy) {
				return fmt.Errorf("sync table %q ordering does not cover its primary key", name)
			}
			if err := checkSyncRegistryExportColumns(spec); err != nil {
				return err
			}
			for _, dependency := range table.references {
				if dependency.table == "accounts" || dependency.table == name {
					continue
				}
				if _, excluded := syncLocalArchiveExclusions[dependency.table]; excluded {
					return fmt.Errorf("portable sync table %q depends on excluded local state %q", name, dependency.table)
				}
				if prior, ok := registered[dependency.table]; !ok || (prior >= index && !dependency.deferred) {
					return fmt.Errorf("sync table %q has invalid import dependency order for %q", name, dependency.table)
				}
			}
			if spec.movement != portableSyncMovementPolicy() {
				return fmt.Errorf("sync table %q movement policy is incomplete", name)
			}
		} else if exclusion, ok := syncLocalArchiveExclusions[name]; ok {
			if exclusion.introducedSyncSchema != table.version || exclusion.reason == "" || exclusion.movement != excludedSyncMovementPolicy() {
				return fmt.Errorf("sync exclusion %q lacks version/reason/movement policy", name)
			}
		} else {
			return fmt.Errorf("unregistered sync table %q", name)
		}
		fence := regexp.MustCompile(`(?is)CREATE\s+TRIGGER\s+account_evacuation_fence\s+BEFORE\s+INSERT\s+OR\s+UPDATE\s+OR\s+DELETE\s+ON\s+` + regexp.QuoteMeta(name) + `\s+FOR\s+EACH\s+ROW\s+EXECUTE\s+FUNCTION\s+witself_tenant_evacuation_fence\s*\(\s*\)`)
		if !fence.MatchString(ddl) {
			return fmt.Errorf("sync table %q lacks its evacuation fence", name)
		}
	}
	for name := range registered {
		if _, ok := tables[name]; !ok {
			return fmt.Errorf("sync archive table %q is not created by a migration", name)
		}
	}
	for name := range syncLocalArchiveExclusions {
		if _, ok := tables[name]; !ok {
			return fmt.Errorf("sync exclusion %q is not created by a migration", name)
		}
	}
	return nil
}

func checkSyncRegistryExportColumns(spec syncArchiveTable) error {
	arguments, err := sqlFunctionArguments(spec.exportQuery(), "jsonb_build_object")
	if err != nil {
		return err
	}
	if len(arguments) != 2*len(spec.columns) {
		return fmt.Errorf("sync exporter %q has incomplete columns", spec.name)
	}
	for index, column := range spec.columns {
		if arguments[2*index] != "'"+column+"'" || arguments[2*index+1] != column {
			return fmt.Errorf("sync exporter %q column mapping differs", spec.name)
		}
	}
	wantSuffix := " FROM " + spec.name + " WHERE account_id = $1 ORDER BY " + strings.Join(spec.orderBy, ", ")
	if !strings.HasSuffix(spec.exportQuery(), wantSuffix) {
		return fmt.Errorf("sync exporter %q scope or ordering differs", spec.name)
	}
	return nil
}

func syncMigrationTableContracts(source fs.FS) (map[string]syncMigrationTableContract, string, error) {
	entries, err := fs.ReadDir(source, "sync_migrations")
	if err != nil {
		return nil, "", err
	}
	primaryKey := regexp.MustCompile(`(?is)^PRIMARY\s+KEY\s*\(([^)]*)\)`)
	reference := regexp.MustCompile(`(?i)\bREFERENCES\s+([a-z_][a-z_0-9]*)\s*\(`)
	definitionHead := regexp.MustCompile(`^[a-zA-Z_][a-zA-Z_0-9]*`)
	tables := make(map[string]syncMigrationTableContract)
	var ddl strings.Builder
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return nil, "", fmt.Errorf("sync migration lacks version: %s", entry.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, "", err
		}
		raw, err := fs.ReadFile(source, "sync_migrations/"+entry.Name())
		if err != nil {
			return nil, "", err
		}
		statements, err := syncSQLStatements(string(raw))
		if err != nil {
			return nil, "", err
		}
		for _, statement := range statements {
			sql := syncSQLText(statement) + ";"
			ddl.WriteString(sql)
			ddl.WriteByte('\n')
			// Classify only the statement head. Unrecognized statement families
			// cannot silently pass as inventory-neutral SQL. Accepted procedural
			// bodies remain opaque; PostgreSQL's catalog certifies their effects.
			switch {
			case statement[0].keyword("ALTER"):
				if len(statement) < 2 || !statement[1].keyword("TABLE") {
					return nil, "", errSyncMigrationSQL
				}
				if err := syncSQLAlterTable(statement[2:], tables); err != nil {
					return nil, "", err
				}
				continue
			case statement[0].keyword("SELECT"):
				for _, token := range statement[1:] {
					if token.keyword("INTO") {
						return nil, "", errSyncMigrationSQL
					}
				}
				continue
			case statement[0].keyword("DO"):
				if len(statement) != 2 || statement[1].kind != 's' {
					return nil, "", errSyncMigrationSQL
				}
				continue
			case statement[0].keyword("CREATE"):
				// The bounded CREATE classifier below rejects unfamiliar forms.
			default:
				return nil, "", errSyncMigrationSQL
			}
			name, body, isTable, err := syncSQLCreateTable(statement[1:])
			if err != nil {
				return nil, "", err
			}
			if !isTable {
				continue
			}
			if _, duplicate := tables[name]; duplicate {
				return nil, "", fmt.Errorf("sync migration creates %q twice", name)
			}
			// Reuse the balanced SQL argument parser: table definitions have
			// the same nested-parenthesis and quoted-string boundaries.
			definitions, err := sqlFunctionArguments("columns("+syncSQLText(body)+")", "columns")
			if err != nil {
				return nil, "", err
			}
			table := syncMigrationTableContract{version: version}
			for _, definition := range definitions {
				head := definitionHead.FindString(definition)
				if head == "" {
					return nil, "", fmt.Errorf("empty sync table definition")
				}
				if match := primaryKey.FindStringSubmatch(definition); match != nil {
					for _, column := range strings.Split(match[1], ",") {
						table.primaryKey = append(table.primaryKey, strings.TrimSpace(column))
					}
				}
				for _, match := range reference.FindAllStringSubmatch(definition, -1) {
					table.references = append(table.references, syncMigrationReference{
						table: match[1], deferred: strings.Contains(strings.ToUpper(definition), "DEFERRABLE INITIALLY DEFERRED"),
					})
				}
				switch strings.ToUpper(head) {
				case "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "CONSTRAINT":
					continue
				}
				table.columns = append(table.columns, head)
				if strings.Contains(strings.ToUpper(definition), "PRIMARY KEY") {
					table.primaryKey = append(table.primaryKey, head)
				}
			}
			tables[name] = table
		}
	}
	return tables, ddl.String(), nil
}

func copiedSyncMigrationFS(t *testing.T) fstest.MapFS {
	t.Helper()
	copyFS := make(fstest.MapFS)
	err := fs.WalkDir(syncMigrationsFS, "sync_migrations", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		raw, err := fs.ReadFile(syncMigrationsFS, path)
		if err != nil {
			return err
		}
		copyFS[path] = &fstest.MapFile{Data: raw}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return copyFS
}
