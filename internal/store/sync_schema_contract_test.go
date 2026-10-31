// Package store's installed-schema test projection is CLOSED at the relation
// set and table type, full format_type with typmod, domain name and underlying
// type, attnotnull, default-expression text, collation, identity, generated,
// attndims (declared array rank), and attnum order. Storage, compression,
// statistics targets, TOAST options, and comments do not change the logical
// schema or archive bytes and are out of scope; do not add them without a new
// owner ruling. Domain-over-array declarations are explicitly unsupported:
// their base rank requires typndims, which is outside this closed field set.
package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

var errSyncSchemaProjection = errors.New("sync installed schema differs from registry projection")

type syncSchemaRelation struct{ schema, name string }

type syncSchemaCatalogTable struct {
	oid uint32
	syncSchemaTable
}

type syncSchemaCatalog struct {
	schema string
	tables map[syncSchemaRelation]syncSchemaCatalogTable
}

type syncSchemaCatalogReader interface {
	Begin(context.Context) (pgx.Tx, error)
}

// Read the installed catalog, never the lexical migration inventory. The full
// user catalog catches newly created relations even outside the sync_ prefix or
// fixture schema. OIDs distinguish a dropped/recreated relation from its prior
// name. Materialized views are included but deliberately unsupported: unlike
// ordinary tables/views they have no information_schema projection to certify.
func readSyncSchemaCatalog(ctx context.Context, q syncSchemaCatalogReader) (syncSchemaCatalog, error) {
	catalog := syncSchemaCatalog{tables: make(map[syncSchemaRelation]syncSchemaCatalogTable)}
	// A transaction (or savepoint for a caller transaction) pins one connection.
	// Rendering under pg_catalog alone makes user type and expression names
	// qualified regardless of caller search_path. Rollback restores that path.
	tx, err := q.Begin(ctx)
	if err != nil {
		return catalog, errSyncSchemaProjection
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.QueryRow(ctx, `SELECT current_schema()`).Scan(&catalog.schema); err != nil {
		return catalog, errSyncSchemaProjection
	}
	if _, err := tx.Exec(ctx, `SET LOCAL search_path = pg_catalog`); err != nil {
		return catalog, errSyncSchemaProjection
	}
	rows, err := tx.Query(ctx, `SELECT n.nspname, c.relname, c.oid, coalesce(t.table_type, 'unsupported')
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		LEFT JOIN information_schema.tables t ON t.table_schema=n.nspname AND t.table_name=c.relname
		WHERE c.relkind IN ('r','p','v','m','f')
		AND n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'`)
	if err != nil {
		return catalog, errSyncSchemaProjection
	}
	defer rows.Close()
	for rows.Next() {
		var relation syncSchemaRelation
		var table syncSchemaCatalogTable
		if err := rows.Scan(&relation.schema, &relation.name, &table.oid, &table.tableType); err != nil {
			return catalog, errSyncSchemaProjection
		}
		catalog.tables[relation] = table
	}
	if rows.Err() != nil {
		return catalog, errSyncSchemaProjection
	}
	rows.Close()
	columns, err := tx.Query(ctx, `SELECT n.nspname, c.relname, a.attname, a.attnum, a.attndims,
		pg_catalog.format_type(a.atttypid, a.atttypmod), tn.nspname, typ.typname, a.atttypmod,
		typ.typtype='d', coalesce(pg_catalog.format_type(nullif(typ.typbasetype, 0), typ.typtypmod), ''),
		a.attnotnull, ad.oid IS NOT NULL, coalesce(pg_catalog.pg_get_expr(ad.adbin, ad.adrelid), ''),
		coalesce(cn.nspname, ''), coalesce(coll.collname, ''), a.attidentity::text, a.attgenerated::text,
		coalesce(i.ordinal_position, a.attnum), coalesce(i.data_type, 'unsupported'),
		coalesce(i.is_nullable='YES', false), i.column_default IS NOT NULL
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		JOIN pg_catalog.pg_type typ ON typ.oid=a.atttypid
		JOIN pg_catalog.pg_namespace tn ON tn.oid=typ.typnamespace
		LEFT JOIN pg_catalog.pg_attrdef ad ON ad.adrelid=a.attrelid AND ad.adnum=a.attnum
		LEFT JOIN pg_catalog.pg_collation coll ON coll.oid=a.attcollation
		LEFT JOIN pg_catalog.pg_namespace cn ON cn.oid=coll.collnamespace
		LEFT JOIN information_schema.columns i ON i.table_schema=n.nspname AND i.table_name=c.relname AND i.column_name=a.attname
		WHERE a.attnum > 0 AND NOT a.attisdropped AND c.relkind IN ('r','p','v','m','f')
		AND n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
		ORDER BY n.nspname, c.relname, a.attnum`)
	if err != nil {
		return catalog, errSyncSchemaProjection
	}
	defer columns.Close()
	for columns.Next() {
		var relation syncSchemaRelation
		var column syncSchemaColumn
		var typeSchema, typeName, collationSchema, collationName string
		var domain, infoHasDefault bool
		var infoOrdinal int
		if err := columns.Scan(&relation.schema, &relation.name, &column.name, &column.ordinal, &column.arrayDimensions,
			&column.fullType, &typeSchema, &typeName, &column.typeMod, &domain, &column.domainBase,
			&column.notNull, &column.hasDefault, &column.defaultExpr, &collationSchema, &collationName,
			&column.identity, &column.generated, &infoOrdinal, &column.dataType, &column.nullable, &infoHasDefault); err != nil {
			return catalog, errSyncSchemaProjection
		}
		// information_schema omits generated expressions from column_default;
		// domains can contribute NOT NULL independently of attnotnull.
		if column.dataType != "unsupported" && (infoOrdinal != column.ordinal || infoHasDefault != (column.hasDefault && column.generated == "") ||
			(!domain && column.nullable == column.notNull)) {
			return catalog, syncSchemaDifference(relation.name, column.name)
		}
		column.typeName = syncSchemaQualifiedName(catalog.schema, typeSchema, typeName)
		column.fullType = normalizeSyncSchemaType(catalog.schema, column.fullType)
		if domain {
			column.domainName = column.typeName
			column.domainBase = normalizeSyncSchemaType(catalog.schema, column.domainBase)
		}
		if collationName != "" && (collationSchema != "pg_catalog" || collationName != "default") {
			column.collation = syncSchemaQualifiedName(catalog.schema, collationSchema, collationName)
		}
		table, ok := catalog.tables[relation]
		if !ok {
			return catalog, errSyncSchemaProjection
		}
		table.columns = append(table.columns, column)
		catalog.tables[relation] = table
	}
	if columns.Err() != nil {
		return catalog, errSyncSchemaProjection
	}
	return catalog, nil
}

// Only a type's leading namespace component is normalized. Default expressions
// remain byte-exact: replacing schema-like text there could rewrite literals.
func normalizeSyncSchemaType(schema, fullType string) string {
	for _, prefix := range []string{pgx.Identifier{schema}.Sanitize() + ".", schema + "."} {
		if rest, ok := strings.CutPrefix(fullType, prefix); ok {
			return "<fixture>." + rest
		}
	}
	return fullType
}

func syncSchemaQualifiedName(fixture, schema, name string) string {
	if schema == fixture {
		return "<fixture>." + pgx.Identifier{name}.Sanitize()
	}
	return pgx.Identifier{schema, name}.Sanitize()
}

func syncSchemaDifference(relation, column string) error {
	if column == "" {
		return fmt.Errorf("%w: relation %q", errSyncSchemaProjection, relation)
	}
	return fmt.Errorf("%w: relation %q column %q", errSyncSchemaProjection, relation, column)
}

func compareSyncSchemaTable(name string, installed, declared syncSchemaTable) error {
	if installed.tableType != declared.tableType {
		return syncSchemaDifference(name, "")
	}
	for i, column := range installed.columns {
		if i >= len(declared.columns) || column != declared.columns[i] {
			return syncSchemaDifference(name, column.name)
		}
	}
	if len(installed.columns) < len(declared.columns) {
		return syncSchemaDifference(name, declared.columns[len(installed.columns)].name)
	}
	return nil
}

// This is the authoritative export-completeness contract. Include every sync
// relation and every relation created by the sync migration set, even when no
// lexical parser recognizes its DDL. Unrelated pre-existing relations are not
// export candidates. A sync migration may not silently change core relations.
func validateSyncInstalledSchema(before, after syncSchemaCatalog, declared map[string]syncSchemaTable) error {
	if before.schema == "" || after.schema != before.schema {
		return errSyncSchemaProjection
	}
	installed := make(map[string]syncSchemaTable)
	for _, relation := range sortedSyncSchemaRelations(after) {
		table := after.tables[relation]
		prior, existed := before.tables[relation]
		inNamespace := relation.schema == after.schema &&
			(strings.HasPrefix(relation.name, "sync_") || relation.name == syncSchemaVersionTable)
		if !inNamespace && existed && prior.oid == table.oid {
			if err := compareSyncSchemaTable(relation.name, table.syncSchemaTable, prior.syncSchemaTable); err != nil {
				return err
			}
			continue
		}
		if relation.schema != after.schema {
			return syncSchemaDifference(relation.name, "")
		}
		installed[relation.name] = table.syncSchemaTable
	}
	for _, relation := range sortedSyncSchemaRelations(before) {
		if _, present := after.tables[relation]; !present {
			return syncSchemaDifference(relation.name, "")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(installed)) {
		want, ok := declared[name]
		if !ok {
			return syncSchemaDifference(name, "")
		}
		// Only registered sync/new relations are subject to this supported-type
		// boundary; unchanged pre-existing core relations remain comparable.
		for _, column := range installed[name].columns {
			if column.domainName != "" && column.dataType == "ARRAY" {
				return syncSchemaDifference(name, column.name)
			}
		}
		if err := compareSyncSchemaTable(name, installed[name], want); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if _, ok := installed[name]; !ok {
			return syncSchemaDifference(name, "")
		}
	}
	return nil
}

func sortedSyncSchemaRelations(catalog syncSchemaCatalog) []syncSchemaRelation {
	return slices.SortedFunc(maps.Keys(catalog.tables), func(a, b syncSchemaRelation) int {
		if order := cmp.Compare(a.schema, b.schema); order != 0 {
			return order
		}
		return cmp.Compare(a.name, b.name)
	})
}

func requireSyncSchemaCatalog(t *testing.T, q syncSchemaCatalogReader) syncSchemaCatalog {
	t.Helper()
	catalog, err := readSyncSchemaCatalog(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func requireDeclaredSyncSchemaProjection(t *testing.T) map[string]syncSchemaTable {
	t.Helper()
	projection, err := declaredSyncSchemaProjection()
	if err != nil {
		t.Fatal(err)
	}
	return projection
}

func TestSyncArchiveSchemaCompletenessPostgres(t *testing.T) {
	st, _ := newSyncSchemaTestStore(t)
	before := requireSyncSchemaCatalog(t, st.pool)
	if err := st.MigrateSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := validateSyncInstalledSchema(before, requireSyncSchemaCatalog(t, st.pool), requireDeclaredSyncSchemaProjection(t)); err != nil {
		t.Fatal(err)
	}
}

// The injected provider executes copied migration bytes with PostgreSQL. It
// deliberately bypasses the lexical pre-check so that a lexical refusal cannot
// masquerade as a kill by the authoritative installed-schema contract.
func applySyncContractMigrations(t *testing.T, dsn string, source fs.FS) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("open contract migration database failed")
	}
	defer func() { _ = db.Close() }()
	fsys, err := fs.Sub(source, "sync_migrations")
	if err != nil {
		t.Fatal("open contract migration filesystem failed")
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys,
		goose.WithTableName(syncSchemaVersionTable), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal("configure contract migration provider failed")
	}
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatal("apply contract migrations failed")
	}
}

func TestSyncArchiveSchemaCompletenessMigrationRegressionsPostgres(t *testing.T) {
	for _, tc := range []struct{ name, ddl, column string }{
		{"M14_quoted_ADD", `ALTER TABLE "sync_incarnations"
    ADD COLUMN unregistered_evidence text NOT NULL DEFAULT 'review-evidence';`, "unregistered_evidence"},
		{"M17_mixed_ADD_DROP", `ALTER TABLE "sync_incarnations"
    ADD COLUMN review_phase_order_evidence text NOT NULL DEFAULT 'review-evidence',
    DROP COLUMN IF EXISTS review_phase_order_evidence;`, "review_phase_order_evidence"},
		{"opaque_procedural_ADD", `-- +goose StatementBegin
DO $$ BEGIN
    EXECUTE 'ALTER TABLE sync_incarnations ADD COLUMN procedural_evidence text';
END $$;
-- +goose StatementEnd`, "procedural_evidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, dsn := newSyncSchemaTestStore(t)
			before := requireSyncSchemaCatalog(t, st.pool)
			mutated := copiedSyncMigrationFS(t)
			file := mutated["sync_migrations/0001_sync_schema.sql"]
			const down = "-- +goose Down"
			if strings.Count(string(file.Data), down) != 1 {
				t.Fatal("contract mutation target missing")
			}
			file.Data = []byte(strings.Replace(string(file.Data), down, tc.ddl+"\n\n"+down, 1))
			applySyncContractMigrations(t, dsn, mutated)
			after := requireSyncSchemaCatalog(t, st.pool)
			table := after.tables[syncSchemaRelation{after.schema, "sync_incarnations"}]
			if !slices.ContainsFunc(table.columns, func(column syncSchemaColumn) bool { return column.name == tc.column }) {
				t.Fatal("PostgreSQL did not install the regression column")
			}
			if err := validateSyncInstalledSchema(before, after, requireDeclaredSyncSchemaProjection(t)); !errors.Is(err, errSyncSchemaProjection) || err.Error() != syncSchemaDifference("sync_incarnations", tc.column).Error() {
				t.Fatal("installed unregistered column escaped the fixed schema refusal")
			}
		})
	}
}

func TestSyncArchiveSchemaCompletenessRegisteredMigrationPostgres(t *testing.T) {
	st, dsn := newSyncSchemaTestStore(t)
	before := requireSyncSchemaCatalog(t, st.pool)
	projection := requireDeclaredSyncSchemaProjection(t)
	portable := slices.Clone(syncArchiveTables)
	const tableName = "sync_incarnations"
	const columnName = "registered_evidence"
	for i := range portable {
		if portable[i].name == tableName {
			portable[i].columns = append(slices.Clone(portable[i].columns), columnName)
		}
	}
	table := projection[tableName]
	table.columns = append(table.columns, syncSchemaColumn{name: columnName, ordinal: len(table.columns) + 1,
		dataType: "text", nullable: false, hasDefault: true, fullType: "text", typeName: `"pg_catalog"."text"`, typeMod: -1,
		domainName: "", domainBase: "", arrayDimensions: 0, notNull: true, defaultExpr: "'registered'::text", collation: "", identity: "", generated: ""})
	projection[tableName] = table
	if err := validateSyncSchemaProjection(projection, portable, syncLocalArchiveExclusions); err != nil {
		t.Fatal(err)
	}
	mutated := copiedSyncMigrationFS(t)
	mutated["sync_migrations/0002_registered_evidence.sql"] = &fstest.MapFile{Data: []byte(`-- +goose Up
ALTER TABLE sync_incarnations ADD COLUMN registered_evidence text NOT NULL DEFAULT 'registered';
-- +goose Down
SELECT 1;
`)}
	applySyncContractMigrations(t, dsn, mutated)
	if err := validateSyncInstalledSchema(before, requireSyncSchemaCatalog(t, st.pool), projection); err != nil {
		t.Fatal("registered migration column did not satisfy installed schema contract")
	}
}

func TestSyncArchiveSchemaCompletenessCatalogDriftPostgres(t *testing.T) {
	st, _ := newSyncSchemaTestStore(t)
	before := requireSyncSchemaCatalog(t, st.pool)
	if err := st.MigrateSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	projection := requireDeclaredSyncSchemaProjection(t)
	if err := validateSyncInstalledSchema(before, requireSyncSchemaCatalog(t, st.pool), projection); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, ddl string }{
		{"prefixed_table", `CREATE TABLE sync_unregistered (evidence text)`},
		{"unprefixed_table", `CREATE TABLE unregistered_evidence (evidence text)`},
		{"other_schema_table", "CREATE SCHEMA " + pgx.Identifier{before.schema + "_extra"}.Sanitize() +
			"; CREATE TABLE " + pgx.Identifier{before.schema + "_extra", "unregistered_evidence"}.Sanitize() + " (evidence text)"},
		{"unprefixed_view", `CREATE VIEW unregistered_view AS SELECT 1 AS evidence`},
		{"materialized_view", `CREATE MATERIALIZED VIEW unregistered_materialized AS SELECT 1 AS evidence`},
		{"missing_table", `DROP TABLE sync_payload_retirements`},
		{"missing_column", `ALTER TABLE sync_incarnations DROP COLUMN publication_proof_ref`},
		{"column_name", `ALTER TABLE sync_incarnations RENAME COLUMN publication_proof_ref TO renamed_evidence`},
		{"column_type", `ALTER TABLE sync_incarnations ALTER COLUMN publication_proof_ref TYPE varchar(100)`},
		{"nullability", `ALTER TABLE sync_incarnations ALTER COLUMN publication_proof_ref SET NOT NULL`},
		{"default_added", `ALTER TABLE sync_incarnations ALTER COLUMN publication_proof_ref SET DEFAULT 'unexpected'`},
		{"default_removed", `ALTER TABLE sync_resource_budgets ALTER COLUMN pending_operations DROP DEFAULT`},
		{"default_expression", `ALTER TABLE sync_resource_budgets ALTER COLUMN pending_operations SET DEFAULT 1`},
		{"text_default_expression", `ALTER TABLE sync_publication_outbox ALTER COLUMN state SET DEFAULT 'pinned'`},
		{"ordinal_gap", `ALTER TABLE sync_incarnations DROP COLUMN publication_proof_ref;
ALTER TABLE sync_incarnations ADD COLUMN publication_proof_ref text`},
		{"excluded_column", `ALTER TABLE sync_capture ADD COLUMN unregistered_evidence text`},
		{"bookkeeping_column", `ALTER TABLE witself_sync_schema_version ADD COLUMN unregistered_evidence text`},
		{"core_column", `ALTER TABLE accounts ADD COLUMN unregistered_evidence text`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := st.pool.Begin(context.Background())
			if err != nil {
				t.Fatal("begin catalog drift transaction failed")
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(context.Background(), tc.ddl); err != nil {
				t.Fatal("apply catalog drift failed")
			}
			if err := validateSyncInstalledSchema(before, requireSyncSchemaCatalog(t, tx), projection); !errors.Is(err, errSyncSchemaProjection) {
				t.Fatal("catalog drift escaped the fixed schema refusal")
			}
		})
	}
}
