package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSyncArchiveSchemaTypeNamespaceNormalization(t *testing.T) {
	for _, tc := range []struct{ name, schema, fullType, want string }{
		{"unquoted", "local_schema", "local_schema.domain_type", "<fixture>.domain_type"},
		{"quoted", "local_schema", `"local_schema".domain_type`, "<fixture>.domain_type"},
		{"array", "local_schema", "local_schema.domain_type[]", "<fixture>.domain_type[]"},
		{"external", "local_schema", "external_schema.domain_type", "external_schema.domain_type"},
		{"shared_prefix", "local_schema", "local_schema_extra.domain_type", "local_schema_extra.domain_type"},
		{"quoted_component", "local.schema", `"local.schema".domain_type`, "<fixture>.domain_type"},
		{"real_marker_schema", "local_schema", `"<fixture>".domain_type`, `"<fixture>".domain_type`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if normalizeSyncSchemaType(tc.schema, tc.fullType) != tc.want {
				t.Fatal("type namespace normalization changed identity")
			}
		})
	}
	local := syncSchemaQualifiedName("local_schema", "local_schema", "domain_type")
	for _, schema := range []string{"fixture", "<fixture>", "external_schema"} {
		if local == syncSchemaQualifiedName("local_schema", schema, "domain_type") {
			t.Fatal("fixture type identity aliases an external namespace")
		}
	}
}

func TestSyncArchiveSchemaCompletenessTypeMigrationsPostgres(t *testing.T) {
	for _, tc := range []struct {
		name, ddl, relation, column, observation string
	}{
		{"M21_named_domain", `-- +goose StatementBegin
DO $review$ BEGIN
    EXECUTE 'CREATE DOMAIN r4_evidence_domain AS text CHECK (VALUE <> ''r4_forbidden_evidence'')';
    EXECUTE 'ALTER TABLE sync_incarnations ALTER COLUMN publication_proof_ref TYPE r4_evidence_domain';
END $review$;
-- +goose StatementEnd`, "sync_incarnations", "publication_proof_ref", `SELECT
			i.data_type='text' AND i.domain_schema=current_schema() AND i.domain_name='r4_evidence_domain'
			AND t.typtype='d' AND t.typname='r4_evidence_domain' AND n.nspname=current_schema()
			AND pg_catalog.format_type(t.typbasetype, t.typtypmod)='text'
			FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type t ON t.oid=a.atttypid
			JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
			JOIN information_schema.columns i ON i.table_schema=current_schema()
			AND i.table_name='sync_incarnations' AND i.column_name=a.attname
			WHERE a.attrelid='sync_incarnations'::regclass AND a.attname='publication_proof_ref'`},
		{"M22_timestamp_precision", `ALTER TABLE sync_receipts ALTER COLUMN settled_at TYPE timestamptz(0);`,
			"sync_receipts", "settled_at", `SELECT i.data_type='timestamp with time zone' AND i.datetime_precision=0
			AND a.atttypmod=0 AND pg_catalog.format_type(a.atttypid,a.atttypmod)='timestamp(0) with time zone'
			FROM pg_catalog.pg_attribute a JOIN information_schema.columns i
			ON i.table_schema=current_schema() AND i.table_name='sync_receipts' AND i.column_name=a.attname
			WHERE a.attrelid='sync_receipts'::regclass AND a.attname='settled_at'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, dsn := newSyncSchemaTestStore(t)
			before := requireSyncSchemaCatalog(t, st.pool)
			mutated := copiedSyncMigrationFS(t)
			file := mutated["sync_migrations/0001_sync_schema.sql"]
			const down = "-- +goose Down"
			if strings.Count(string(file.Data), down) != 1 {
				t.Fatal("type regression mutation target missing")
			}
			file.Data = []byte(strings.Replace(string(file.Data), down, tc.ddl+"\n\n"+down, 1))
			if err := validateSyncArchiveRegistryContract(mutated); err != nil {
				t.Fatal("type regression no longer reaches the installed catalog contract")
			}
			applySyncContractMigrations(t, dsn, mutated)
			var observed bool
			if err := st.pool.QueryRow(context.Background(), tc.observation).Scan(&observed); err != nil || !observed {
				t.Fatal("PostgreSQL did not install the independently observed type regression")
			}
			if tc.name == "M21_named_domain" {
				_, err := st.pool.Exec(context.Background(), `SELECT 'r4_forbidden_evidence'::r4_evidence_domain`)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
					t.Fatal("domain did not enforce its distinct constraint")
				}
			}
			err := validateSyncInstalledSchema(before, requireSyncSchemaCatalog(t, st.pool), requireDeclaredSyncSchemaProjection(t))
			if !errors.Is(err, errSyncSchemaProjection) || err.Error() != syncSchemaDifference(tc.relation, tc.column).Error() {
				t.Fatal("installed type regression escaped the fixed names-only refusal")
			}
		})
	}
}

// Richer SQL types are accepted only when declared explicitly, including the
// typmod of an array element and a domain's underlying type. All expectations
// are independent of the catalog reader and the executed migration text.
func TestSyncArchiveSchemaCompletenessRegisteredTypesPostgres(t *testing.T) {
	st, dsn := newSyncSchemaTestStore(t)
	before := requireSyncSchemaCatalog(t, st.pool)
	projection := requireDeclaredSyncSchemaProjection(t)
	portable := slices.Clone(syncArchiveTables)
	const tableName = "sync_incarnations"
	columns := []syncSchemaColumn{
		{name: "registered_varchar", ordinal: 11, dataType: "character varying", nullable: true, hasDefault: false,
			fullType: "character varying(32)", typeName: `"pg_catalog"."varchar"`, typeMod: 36, domainName: "", domainBase: "",
			arrayDimensions: 0, notNull: false, defaultExpr: "", collation: "", identity: "", generated: ""},
		{name: "registered_numeric", ordinal: 12, dataType: "numeric", nullable: true, hasDefault: false,
			fullType: "numeric(10,2)", typeName: `"pg_catalog"."numeric"`, typeMod: 655366, domainName: "", domainBase: "",
			arrayDimensions: 0, notNull: false, defaultExpr: "", collation: "", identity: "", generated: ""},
		{name: "registered_precision", ordinal: 13, dataType: "timestamp with time zone", nullable: true, hasDefault: false,
			fullType: "timestamp(0) with time zone", typeName: `"pg_catalog"."timestamptz"`, typeMod: 0, domainName: "", domainBase: "",
			arrayDimensions: 0, notNull: false, defaultExpr: "", collation: "", identity: "", generated: ""},
		{name: "registered_domain", ordinal: 14, dataType: "character varying", nullable: true, hasDefault: false,
			fullType: "<fixture>.registered_contract_domain", typeName: `<fixture>."registered_contract_domain"`, typeMod: -1,
			domainName: `<fixture>."registered_contract_domain"`, domainBase: "character varying(32)",
			arrayDimensions: 0, notNull: false, defaultExpr: "", collation: "", identity: "", generated: ""},
		{name: "registered_array", ordinal: 15, dataType: "ARRAY", nullable: true, hasDefault: false,
			fullType: "character varying(32)[]", typeName: `"pg_catalog"."_varchar"`, typeMod: 36, domainName: "", domainBase: "",
			arrayDimensions: 1, notNull: false, defaultExpr: "", collation: "", identity: "", generated: ""},
		{name: "registered_identity", ordinal: 16, dataType: "bigint", nullable: false, hasDefault: false,
			fullType: "bigint", typeName: `"pg_catalog"."int8"`, typeMod: -1, domainName: "", domainBase: "",
			arrayDimensions: 0, notNull: true, defaultExpr: "", collation: "", identity: "a", generated: ""},
		{name: "registered_generated", ordinal: 17, dataType: "bigint", nullable: true, hasDefault: true,
			fullType: "bigint", typeName: `"pg_catalog"."int8"`, typeMod: -1, domainName: "", domainBase: "",
			arrayDimensions: 0, notNull: false, defaultExpr: "1", collation: "", identity: "", generated: "s"},
		{name: "registered_collated", ordinal: 18, dataType: "text", nullable: true, hasDefault: false,
			fullType: "text", typeName: `"pg_catalog"."text"`, typeMod: -1, domainName: "", domainBase: "",
			arrayDimensions: 0, notNull: false, defaultExpr: "", collation: `"pg_catalog"."C"`, identity: "", generated: ""},
	}
	for i := range portable {
		if portable[i].name == tableName {
			portable[i].columns = slices.Clone(portable[i].columns)
			for _, column := range columns {
				portable[i].columns = append(portable[i].columns, column.name)
			}
		}
	}
	table := projection[tableName]
	table.columns = append(table.columns, columns...)
	projection[tableName] = table
	if err := validateSyncSchemaProjection(projection, portable, syncLocalArchiveExclusions); err != nil {
		t.Fatal("registered type projection does not match archive registry")
	}
	mutated := copiedSyncMigrationFS(t)
	mutated["sync_migrations/0002_registered_types.sql"] = &fstest.MapFile{Data: []byte(`-- +goose Up
CREATE DOMAIN registered_contract_domain AS varchar(32);
ALTER TABLE sync_incarnations ADD COLUMN registered_varchar varchar(32);
ALTER TABLE sync_incarnations ADD COLUMN registered_numeric numeric(10,2);
ALTER TABLE sync_incarnations ADD COLUMN registered_precision timestamptz(0);
ALTER TABLE sync_incarnations ADD COLUMN registered_domain registered_contract_domain;
ALTER TABLE sync_incarnations ADD COLUMN registered_array varchar(32)[];
ALTER TABLE sync_incarnations ADD COLUMN registered_identity bigint GENERATED ALWAYS AS IDENTITY;
ALTER TABLE sync_incarnations ADD COLUMN registered_generated bigint GENERATED ALWAYS AS (1) STORED;
ALTER TABLE sync_incarnations ADD COLUMN registered_collated text COLLATE "C";
-- +goose Down
SELECT 1;
`)}
	applySyncContractMigrations(t, dsn, mutated)
	after := requireSyncSchemaCatalog(t, st.pool)
	if err := validateSyncInstalledSchema(before, after, projection); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, column, ddl string }{
		{"varchar_typmod", "registered_varchar", `ALTER TABLE sync_incarnations ALTER COLUMN registered_varchar TYPE varchar(31)`},
		{"numeric_typmod", "registered_numeric", `ALTER TABLE sync_incarnations ALTER COLUMN registered_numeric TYPE numeric(10,3)`},
		{"array_typmod", "registered_array", `ALTER TABLE sync_incarnations ALTER COLUMN registered_array TYPE varchar(31)[]`},
		{"domain_identity", "registered_domain", `CREATE DOMAIN alternate_contract_domain AS varchar(32);
ALTER TABLE sync_incarnations ALTER COLUMN registered_domain TYPE alternate_contract_domain`},
		{"identity_flag", "registered_identity", `ALTER TABLE sync_incarnations ALTER COLUMN registered_identity SET GENERATED BY DEFAULT`},
		{"generated_flag", "registered_generated", `ALTER TABLE sync_incarnations ALTER COLUMN registered_generated DROP EXPRESSION;
ALTER TABLE sync_incarnations ALTER COLUMN registered_generated SET DEFAULT 1`},
		{"collation", "registered_collated", `ALTER TABLE sync_incarnations ALTER COLUMN registered_collated TYPE text COLLATE "POSIX"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := st.pool.Begin(context.Background())
			if err != nil {
				t.Fatal("begin rich type drift failed")
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(context.Background(), tc.ddl); err != nil {
				t.Fatal("apply rich type drift failed")
			}
			err = validateSyncInstalledSchema(before, requireSyncSchemaCatalog(t, tx), projection)
			if !errors.Is(err, errSyncSchemaProjection) || err.Error() != syncSchemaDifference(tableName, tc.column).Error() {
				t.Fatal("rich type drift escaped the fixed names-only refusal")
			}
		})
	}
	t.Run("array_declared_rank", func(t *testing.T) {
		const observation = `SELECT a.attndims=$1 AND a.atttypmod=36
			AND pg_catalog.format_type(a.atttypid,a.atttypmod)='character varying(32)[]'
			AND a.atttypid='pg_catalog.varchar[]'::regtype
			FROM pg_catalog.pg_attribute a
			WHERE a.attrelid='sync_incarnations'::regclass AND a.attname='registered_array'`
		var observed bool
		if err := st.pool.QueryRow(context.Background(), observation, 1).Scan(&observed); err != nil || !observed {
			t.Fatal("registered array control did not have declared rank one")
		}
		tx, err := st.pool.Begin(context.Background())
		if err != nil {
			t.Fatal("begin array rank drift failed")
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(context.Background(), `ALTER TABLE sync_incarnations ALTER COLUMN registered_array TYPE varchar(32)[][]`); err != nil {
			t.Fatal("apply array rank drift failed")
		}
		if err := tx.QueryRow(context.Background(), observation, 2).Scan(&observed); err != nil || !observed {
			t.Fatal("array rank drift did not preserve formatted type, type identity, and typmod")
		}
		drifted := requireSyncSchemaCatalog(t, tx)
		relation := syncSchemaRelation{after.schema, tableName}
		control := after.tables[relation].columns[14]
		changed := drifted.tables[relation].columns[14]
		if control.arrayDimensions != 1 || changed.arrayDimensions != 2 {
			t.Fatal("catalog projection omitted the independently observed array ranks")
		}
		changed.arrayDimensions = control.arrayDimensions
		if changed != control {
			t.Fatal("array rank drift changed another projected column field")
		}
		err = validateSyncInstalledSchema(before, drifted, projection)
		if !errors.Is(err, errSyncSchemaProjection) || err.Error() != syncSchemaDifference(tableName, "registered_array").Error() {
			t.Fatal("declared array rank drift escaped the fixed names-only refusal")
		}
	})
	// A domain over an array stores its base rank in typndims, not attndims.
	// Refuse both ranks explicitly instead of silently accepting an incomplete
	// declaration or extending the owner's closed catalog field set.
	for _, tc := range []struct {
		name, base string
		rank       int
	}{
		{"domain_array_rank_one_unsupported", "varchar(32)[]", 1},
		{"domain_array_rank_two_unsupported", "varchar(32)[][]", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := st.pool.Begin(context.Background())
			if err != nil {
				t.Fatal("begin unsupported domain array failed")
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(context.Background(), `CREATE DOMAIN registered_array_domain AS `+tc.base+`;
ALTER TABLE sync_incarnations ALTER COLUMN registered_array TYPE registered_array_domain`); err != nil {
				t.Fatal("apply unsupported domain array failed")
			}
			var observed bool
			if err := tx.QueryRow(context.Background(), `SELECT a.attndims=0 AND typ.typtype='d' AND typ.typndims=$1
				AND pg_catalog.format_type(typ.typbasetype,typ.typtypmod)='character varying(32)[]'
				FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type typ ON typ.oid=a.atttypid
				WHERE a.attrelid='sync_incarnations'::regclass AND a.attname='registered_array'`, tc.rank).Scan(&observed); err != nil || !observed {
				t.Fatal("domain array did not exhibit the independently observed unsupported base rank")
			}
			unsupported := requireDeclaredSyncSchemaProjection(t)
			domainTable := table
			domainTable.columns = slices.Clone(table.columns)
			domainTable.columns[14] = syncSchemaColumn{name: "registered_array", ordinal: 15, dataType: "ARRAY", nullable: true, hasDefault: false,
				fullType: "<fixture>.registered_array_domain", typeName: `<fixture>."registered_array_domain"`, typeMod: -1,
				domainName: `<fixture>."registered_array_domain"`, domainBase: "character varying(32)[]",
				arrayDimensions: 0, notNull: false, defaultExpr: "", collation: "", identity: "", generated: ""}
			unsupported[tableName] = domainTable
			installed := requireSyncSchemaCatalog(t, tx)
			if installed.tables[syncSchemaRelation{after.schema, tableName}].columns[14] != domainTable.columns[14] {
				t.Fatal("unsupported domain array differs from its explicit closed-field declaration")
			}
			for _, err := range []error{
				validateSyncSchemaProjection(unsupported, portable, syncLocalArchiveExclusions),
				validateSyncInstalledSchema(before, installed, unsupported),
			} {
				if !errors.Is(err, errSyncSchemaProjection) || err.Error() != syncSchemaDifference(tableName, "registered_array").Error() {
					t.Fatal("unsupported domain array escaped the fixed names-only refusal")
				}
			}
		})
	}
	// Changing only the declared domain base tests this field independently of
	// its name, its column ordinal, and its information_schema base-type label.
	t.Run("domain_base_declaration", func(t *testing.T) {
		table.columns = slices.Clone(table.columns)
		table.columns[13].domainBase = "character varying(31)"
		projection[tableName] = table
		err := validateSyncInstalledSchema(before, after, projection)
		if !errors.Is(err, errSyncSchemaProjection) || err.Error() != syncSchemaDifference(tableName, "registered_domain").Error() {
			t.Fatal("domain base modifier escaped its explicit declaration")
		}
	})
}
