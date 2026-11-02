package store

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestSyncArchiveRegistryAlterColumnCompleteness(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{
			name: "quoted target review reproduction",
			sql: `ALTER TABLE "sync_incarnations"
    ADD COLUMN unregistered_evidence text NOT NULL DEFAULT 'review-evidence';`,
		},
		{"unquoted target", `ALTER TABLE sync_incarnations ADD COLUMN unregistered_evidence text;`},
		{"quoted column", `ALTER TABLE "sync_incarnations" ADD COLUMN "unregistered_evidence" text;`},
		{"same line", `SELECT 1; ALTER TABLE "sync_incarnations" ADD COLUMN unregistered_evidence text;`},
		{"mixed case and comments", `aLtEr/* target */tAbLe "sync_incarnations" aDd/* column */CoLuMn unregistered_evidence text;`},
		{"rename column", `ALTER TABLE "sync_incarnations" RENAME COLUMN publication_proof_ref TO unregistered_evidence;`},
		{"drop column", `ALTER TABLE "sync_incarnations" DROP COLUMN publication_proof_ref;`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := copiedSyncMigrationFS(t)
			file := mutated["sync_migrations/0001_sync_schema.sql"]
			const down = "-- +goose Down"
			if strings.Count(string(file.Data), down) != 1 {
				t.Fatal("migration Down directive is not unique")
			}
			file.Data = []byte(strings.Replace(string(file.Data), down, tc.sql+"\n\n"+down, 1))
			// This is the same precondition syncArchiveFixture checks before
			// opening its store or producing an archive completeness result.
			if err := validateSyncArchiveRegistryContract(mutated); err == nil || !strings.Contains(err.Error(), "column contract") {
				t.Fatalf("ALTER column mutation passed the archive fixture precondition: %v", err)
			}
		})
	}
}

func TestSyncArchiveRegistryAlterQualifiedTargetDoesNotAlias(t *testing.T) {
	for _, target := range []string{`public.sync_incarnations`, `"public"."sync_incarnations"`} {
		t.Run(target, func(t *testing.T) {
			mutated := copiedSyncMigrationFS(t)
			mutated[fmt.Sprintf("sync_migrations/%04d_qualified_alter.sql", SyncSchemaVersion()+1)] = &fstest.MapFile{
				Data: []byte(`ALTER TABLE ` + target + ` ADD COLUMN unregistered_evidence text;`),
			}
			if err := validateSyncArchiveRegistryContract(mutated); err != errSyncMigrationSQL {
				t.Fatalf("qualified target aliased the unqualified registry relation: %v", err)
			}
		})
	}
}

func TestSyncArchiveRegistryAlterSQLLiteralsAndCommentsAreOpaque(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"ordinary string", `SELECT 'ALTER TABLE sync_incarnations ADD COLUMN unregistered_evidence text;';`},
		{"escaped string", `SELECT E'ALTER TABLE sync_incarnations ADD COLUMN unregistered_evidence text;';`},
		{"dollar string", `SELECT $$ALTER TABLE sync_incarnations ADD COLUMN unregistered_evidence text;$$;`},
		{"tagged dollar string", `SELECT $body$ALTER TABLE sync_incarnations ADD COLUMN unregistered_evidence text;$$$body$;`},
		{"line comment", "SELECT 1; -- ALTER TABLE sync_incarnations ADD COLUMN unregistered_evidence text;\nSELECT 2;"},
		{"nested block comment", `/* ALTER TABLE sync_incarnations /* nested ; */ ADD COLUMN unregistered_evidence text; */ SELECT 1;`},
		{"quoted keywords", `SELECT "ALTER", "TABLE", "ALTER TABLE sync_incarnations ADD COLUMN unregistered_evidence text;";`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := copiedSyncMigrationFS(t)
			mutated[fmt.Sprintf("sync_migrations/%04d_opaque_alter.sql", SyncSchemaVersion()+1)] = &fstest.MapFile{Data: []byte(tc.sql)}
			if err := validateSyncArchiveRegistryContract(mutated); err != nil {
				t.Fatalf("opaque ALTER content changed the registry contract: %v", err)
			}
		})
	}
}

func TestSyncMigrationAlterTableInventory(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		columns      []string
		primaryKey   []string
	}{
		{"add column", `ADD COLUMN evidence text`, []string{"id", "value", "obsolete", "evidence"}, []string{"id"}},
		{"add without column keyword", `ADD evidence text`, []string{"id", "value", "obsolete", "evidence"}, []string{"id"}},
		{"add if absent", `ADD COLUMN IF NOT EXISTS evidence text`, []string{"id", "value", "obsolete", "evidence"}, []string{"id"}},
		{"add if absent without column keyword", `ADD IF NOT EXISTS evidence text`, []string{"id", "value", "obsolete", "evidence"}, []string{"id"}},
		{"existing add if absent", `ADD COLUMN IF NOT EXISTS value text`, []string{"id", "value", "obsolete"}, []string{"id"}},
		{"quoted added column", `ADD COLUMN "Evidence" text NOT NULL DEFAULT 'literal,with,commas'`, []string{"id", "value", "obsolete", "Evidence"}, []string{"id"}},
		{"type arguments contain comma", `ADD COLUMN evidence numeric(10,2)`, []string{"id", "value", "obsolete", "evidence"}, []string{"id"}},
		{"default function contains comma", `ADD COLUMN evidence text DEFAULT concat('review', 'evidence')`, []string{"id", "value", "obsolete", "evidence"}, []string{"id"}},
		{"drop column", `DROP COLUMN obsolete`, []string{"id", "value"}, []string{"id"}},
		{"drop without column keyword", `DROP obsolete`, []string{"id", "value"}, []string{"id"}},
		{"drop primary key column", `DROP COLUMN id`, []string{"value", "obsolete"}, nil},
		{"rename column", `RENAME COLUMN value TO evidence`, []string{"id", "evidence", "obsolete"}, []string{"id"}},
		{"rename without column keyword", `RENAME value TO evidence`, []string{"id", "evidence", "obsolete"}, []string{"id"}},
		{"rename primary key column", `RENAME COLUMN id TO "EvidenceID"`, []string{"EvidenceID", "value", "obsolete"}, []string{"EvidenceID"}},
		{"set default", `ALTER COLUMN value SET DEFAULT 'review,evidence'`, []string{"id", "value", "obsolete"}, []string{"id"}},
		{"drop default", `ALTER COLUMN value DROP DEFAULT`, []string{"id", "value", "obsolete"}, []string{"id"}},
		{"set not null", `ALTER COLUMN value SET NOT NULL`, []string{"id", "value", "obsolete"}, []string{"id"}},
		{"drop not null", `ALTER COLUMN value DROP NOT NULL`, []string{"id", "value", "obsolete"}, []string{"id"}},
		{"change type", `ALTER COLUMN value TYPE text`, []string{"id", "value", "obsolete"}, []string{"id"}},
		{"set data type", `ALTER COLUMN value SET DATA TYPE text`, []string{"id", "value", "obsolete"}, []string{"id"}},
		{"alter without column keyword", `ALTER value SET DEFAULT 'review-evidence'`, []string{"id", "value", "obsolete"}, []string{"id"}},
		{"add check constraint", `ADD CONSTRAINT evidence_check CHECK (value <> '')`, []string{"id", "value", "obsolete"}, []string{"id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := syncAlterInventory(t, `CREATE TABLE evidence (id text PRIMARY KEY, value text, obsolete text); ALTER TABLE "evidence" `+tc.action+`;`)
			table, ok := tables["evidence"]
			if !ok || len(tables) != 1 || table.version != 1 || !slices.Equal(table.columns, tc.columns) || !slices.Equal(table.primaryKey, tc.primaryKey) {
				t.Fatalf("ALTER inventory differs: table=%+v; want columns=%v primary key=%v", table, tc.columns, tc.primaryKey)
			}
		})
	}
}

func TestSyncMigrationAlterQualifiedInventory(t *testing.T) {
	for _, relation := range []string{`evidence`, `"evidence"`, `public.evidence`, `"public"."evidence"`} {
		t.Run(relation, func(t *testing.T) {
			tables := syncAlterInventory(t, `CREATE TABLE `+relation+` (id text); ALTER TABLE `+relation+` ADD COLUMN value text;`)
			name := strings.ReplaceAll(relation, `"`, "")
			if len(tables) != 1 || !slices.Equal(tables[name].columns, []string{"id", "value"}) {
				t.Fatalf("ALTER did not resolve the exact CREATE relation: %+v", tables)
			}
		})
	}
}

func TestSyncMigrationAlterTableRenameAndConstraints(t *testing.T) {
	for _, tc := range []struct{ name, relation, renamed string }{
		{"quoted dot is part of relation", `"a.b"`, "renamed"},
		{"qualified quoted dot keeps only real schema", `public."a.b"`, "public.renamed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := syncAlterInventory(t, `CREATE TABLE `+tc.relation+` (id text); ALTER TABLE `+tc.relation+` RENAME TO renamed;`)
			if len(tables) != 1 || !slices.Equal(tables[tc.renamed].columns, []string{"id"}) {
				t.Fatalf("RENAME TO mistook a quoted dot for a schema qualifier: %+v", tables)
			}
		})
	}
	t.Run("table rename preserves inventory and references", func(t *testing.T) {
		tables := syncAlterInventory(t, `
CREATE TABLE parent (id text PRIMARY KEY);
CREATE TABLE child (id text PRIMARY KEY, parent_id text REFERENCES parent(id));
ALTER TABLE "parent" RENAME TO "renamed_parent";
ALTER TABLE "renamed_parent" ADD COLUMN evidence text;
`)
		if _, stale := tables["parent"]; stale || len(tables) != 2 {
			t.Fatalf("RENAME TO retained the old relation: %+v", tables)
		}
		parent := tables["renamed_parent"]
		if parent.version != 1 || !slices.Equal(parent.columns, []string{"id", "evidence"}) || !slices.Equal(parent.primaryKey, []string{"id"}) {
			t.Fatalf("RENAME TO lost table inventory: %+v", parent)
		}
		if got := tables["child"].references; len(got) != 1 || got[0].table != "renamed_parent" {
			t.Fatalf("RENAME TO did not update relation dependencies: %+v", got)
		}
	})
	t.Run("add constraint records key and dependency", func(t *testing.T) {
		tables := syncAlterInventory(t, `
CREATE TABLE parent (id text PRIMARY KEY);
CREATE TABLE child (id text, parent_id text);
ALTER TABLE child ADD CONSTRAINT child_key PRIMARY KEY (id);
ALTER TABLE child ADD CONSTRAINT child_parent FOREIGN KEY (parent_id) REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED;
`)
		child := tables["child"]
		if !slices.Equal(child.columns, []string{"id", "parent_id"}) || !slices.Equal(child.primaryKey, []string{"id"}) ||
			len(child.references) != 1 || child.references[0].table != "parent" || !child.references[0].deferred {
			t.Fatalf("ADD CONSTRAINT lost inventory or dependency: %+v", child)
		}
	})
}

func TestSyncMigrationAlterMultipleActionsFailClosed(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"M17 review reproduction", `ALTER TABLE "sync_incarnations"
    ADD COLUMN review_phase_order_evidence text NOT NULL DEFAULT 'review-evidence',
    DROP COLUMN IF EXISTS review_phase_order_evidence;`},
		{"reverse action order", `ALTER TABLE sync_incarnations DROP COLUMN IF EXISTS review_phase_order_evidence, ADD COLUMN review_phase_order_evidence text;`},
		{"quoted normalized name", `ALTER TABLE "sync_incarnations" ADD COLUMN "review_phase_order_evidence" text, DROP COLUMN IF EXISTS REVIEW_PHASE_ORDER_EVIDENCE;`},
		{"without column keywords", `ALTER TABLE sync_incarnations ADD review_phase_order_evidence text, DROP IF EXISTS review_phase_order_evidence;`},
		{"both if variants", `ALTER TABLE sync_incarnations ADD COLUMN IF NOT EXISTS review_phase_order_evidence text, DROP COLUMN IF EXISTS review_phase_order_evidence;`},
		{"existing column", `ALTER TABLE sync_incarnations DROP COLUMN id, ADD COLUMN id text;`},
		{"existing column with both if variants", `ALTER TABLE sync_incarnations ADD IF NOT EXISTS id text, DROP IF EXISTS id;`},
		{"disjoint columns", `ALTER TABLE sync_incarnations ADD COLUMN evidence numeric(10,2), DROP COLUMN obsolete;`},
		{"two adds", `ALTER TABLE sync_incarnations ADD COLUMN first_evidence text, ADD COLUMN second_evidence text;`},
		{"no-op followed by add", `ALTER TABLE sync_incarnations ADD COLUMN IF NOT EXISTS id text, ADD COLUMN evidence text;`},
		{"default followed by add", `ALTER TABLE sync_incarnations ALTER COLUMN id SET DEFAULT 'review,evidence', ADD COLUMN evidence text;`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := syncAlterInventory(t, `CREATE TABLE sync_incarnations (id text PRIMARY KEY, obsolete text);`)
			before := tables["sync_incarnations"]
			before.columns = slices.Clone(before.columns)
			before.primaryKey = slices.Clone(before.primaryKey)
			before.references = slices.Clone(before.references)
			statements, err := syncSQLStatements(tc.sql)
			if err != nil || len(statements) != 1 || len(statements[0]) < 2 {
				t.Fatal("invalid multi-action test SQL")
			}
			if err := syncSQLAlterTable(statements[0][2:], tables); err != errSyncMigrationSQL {
				t.Fatalf("multi-action ALTER must return the exact fixed refusal: %v", err)
			}
			if !reflect.DeepEqual(tables["sync_incarnations"], before) {
				t.Fatal("refused multi-action ALTER changed the lexical inventory")
			}
		})
	}
}

func TestSyncMigrationAlterSeparateStatementsRemainSupported(t *testing.T) {
	tables := syncAlterInventory(t, `CREATE TABLE evidence (id text PRIMARY KEY, obsolete text);
ALTER TABLE evidence ADD COLUMN review_phase_order_evidence text;
ALTER TABLE evidence DROP COLUMN review_phase_order_evidence;
ALTER TABLE evidence DROP COLUMN obsolete;
ALTER TABLE evidence ADD COLUMN registered_evidence text;`)
	if !slices.Equal(tables["evidence"].columns, []string{"id", "registered_evidence"}) {
		t.Fatal("separate single-action ALTER statements lost lexical inventory")
	}
}

func TestSyncMigrationAlterUnsupportedFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"inherit", `ALTER TABLE evidence INHERIT parent;`},
		{"unknown unquoted target", `ALTER TABLE absent ADD COLUMN value text;`},
		{"unknown quoted target", `ALTER TABLE "absent" ADD COLUMN value text;`},
		{"qualified target must not alias", `ALTER TABLE public.evidence ADD COLUMN value text;`},
		{"missing target", `ALTER TABLE;`},
		{"literal target", `ALTER TABLE 'evidence' ADD COLUMN value text;`},
		{"malformed qualified target", `ALTER TABLE evidence. ADD COLUMN value text;`},
		{"unsupported target modifier", `ALTER TABLE ONLY evidence ADD COLUMN value text;`},
		{"missing action", `ALTER TABLE evidence;`},
		{"missing column type", `ALTER TABLE evidence ADD COLUMN value;`},
		{"malformed if not exists", `ALTER TABLE evidence ADD COLUMN IF EXISTS value text;`},
		{"missing drop column", `ALTER TABLE evidence DROP COLUMN;`},
		{"missing alter action", `ALTER TABLE evidence ALTER COLUMN id;`},
		{"unknown alter action", `ALTER TABLE evidence ALTER COLUMN id SET STATISTICS 100;`},
		{"missing default", `ALTER TABLE evidence ALTER COLUMN id SET DEFAULT;`},
		{"missing type", `ALTER TABLE evidence ALTER COLUMN id TYPE;`},
		{"missing rename target", `ALTER TABLE evidence RENAME COLUMN id TO;`},
		{"missing constraint body", `ALTER TABLE evidence ADD CONSTRAINT evidence_key;`},
		{"unsupported after add", `ALTER TABLE evidence ADD COLUMN value text UNSUPPORTED;`},
		{"unsupported after drop", `ALTER TABLE evidence DROP COLUMN id UNSUPPORTED;`},
		{"unsupported after alter", `ALTER TABLE evidence ALTER COLUMN id DROP DEFAULT UNSUPPORTED;`},
		{"unsupported after column rename", `ALTER TABLE evidence RENAME COLUMN id TO new_id UNSUPPORTED;`},
		{"unsupported after table rename", `ALTER TABLE evidence RENAME TO new_evidence UNSUPPORTED;`},
		{"unsupported after constraint", `ALTER TABLE evidence ADD CONSTRAINT evidence_key UNIQUE (id) UNSUPPORTED;`},
		{"trailing comma", `ALTER TABLE evidence ADD COLUMN value text,;`},
		{"unknown later action", `ALTER TABLE evidence ADD COLUMN value text, INHERIT parent;`},
		{"quoted alter action is not keyword", `ALTER TABLE evidence "ADD" COLUMN value text;`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := fstest.MapFS{"sync_migrations/0001_alter.sql": &fstest.MapFile{
				Data: []byte(`CREATE TABLE evidence (id text); ` + tc.sql),
			}}
			if _, _, err := syncMigrationTableContracts(source); err != errSyncMigrationSQL {
				t.Fatalf("unsupported ALTER must return the exact fixed refusal: %v", err)
			}
		})
	}
}

func syncAlterInventory(t *testing.T, sql string) map[string]syncMigrationTableContract {
	t.Helper()
	source := fstest.MapFS{"sync_migrations/0001_alter.sql": &fstest.MapFile{Data: []byte(sql)}}
	tables, _, err := syncMigrationTableContracts(source)
	if err != nil {
		t.Fatalf("supported ALTER inventory refused: %v", err)
	}
	return tables
}
