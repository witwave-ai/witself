package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

var errSyncWriterInventory = errors.New("sync writer inventory is incomplete")

func TestSyncWriterInventoryComplete(t *testing.T) {
	if err := validateSyncWriterInventory(migrationsFS, syncMigrationsFS, syncWriterTables); err != nil {
		t.Fatalf("sync inventory completeness: %v", err)
	}
}

func TestSyncWriterInventoryRejectsOmissions(t *testing.T) {
	for _, name := range syncWriterTables {
		t.Run(name, func(t *testing.T) {
			inventory := slices.DeleteFunc(slices.Clone(syncWriterTables), func(item string) bool { return item == name })
			if err := validateSyncWriterInventory(migrationsFS, syncMigrationsFS, inventory); !errors.Is(err, errSyncWriterInventory) {
				t.Fatalf("sync inventory omission survived: %v", err)
			}
		})
	}
	for _, namespace := range []string{"migrations", "sync_migrations"} {
		t.Run(namespace, func(t *testing.T) {
			source := migrationsFS
			if namespace == "sync_migrations" {
				source = syncMigrationsFS
			}
			mutated := fstest.MapFS{}
			entries, err := fs.ReadDir(source, namespace)
			if err != nil {
				t.Fatalf("sync inventory filesystem read: %v", err)
			}
			for _, entry := range entries {
				path := namespace + "/" + entry.Name()
				data, err := fs.ReadFile(source, path)
				if err != nil {
					t.Fatalf("sync inventory migration read: %v", err)
				}
				mutated[path] = &fstest.MapFile{Data: data}
			}
			mutated[namespace+"/9999_unreviewed.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE unreviewed_writer (\n account_id text\n);")}
			var got error
			if namespace == "migrations" {
				got = validateSyncWriterInventory(mutated, syncMigrationsFS, syncWriterTables)
			} else {
				got = validateSyncWriterInventory(migrationsFS, mutated, syncWriterTables)
			}
			if !errors.Is(got, errSyncWriterInventory) {
				t.Fatalf("sync inventory added table survived: %v", got)
			}
		})
	}
	t.Run("alter_adds_account_scope", func(t *testing.T) {
		mutated := fstest.MapFS{}
		entries, err := fs.ReadDir(migrationsFS, "migrations")
		if err != nil {
			t.Fatalf("sync inventory ALTER fixture list: %v", err)
		}
		for _, entry := range entries {
			path := "migrations/" + entry.Name()
			data, err := fs.ReadFile(migrationsFS, path)
			if err != nil {
				t.Fatalf("sync inventory ALTER fixture read: %v", err)
			}
			mutated[path] = &fstest.MapFile{Data: data}
		}
		mutated["migrations/9999_account_scope.sql"] = &fstest.MapFile{Data: []byte(`ALTER TABLE account_purge_sweep_state ADD COLUMN account_id text;`)}
		if err := validateSyncWriterInventory(mutated, syncMigrationsFS, syncWriterTables); !errors.Is(err, errSyncWriterInventory) {
			t.Fatalf("sync inventory ALTER scope survived: %v", err)
		}
	})
}

func TestSyncWriterInventoryGuardAndLockContract(t *testing.T) {
	raw, err := syncMigrationsFS.ReadFile("sync_migrations/0002_sync_capture_guards.sql")
	if err != nil {
		t.Fatalf("sync inventory guard migration read: %v", err)
	}
	sql := string(raw)
	lock := regexp.MustCompile(`(?s)LOCK TABLE (.*?) IN ACCESS EXCLUSIVE MODE;`).FindStringSubmatch(sql)
	if len(lock) != 2 {
		t.Fatal("sync inventory relation drain missing")
	}
	var locked []string
	for _, name := range strings.Split(lock[1], ",") {
		locked = append(locked, strings.TrimSpace(name))
	}
	if !slices.Equal(locked, syncWriterTables) {
		t.Fatal("sync inventory relation lock order differs")
	}
	for _, name := range syncWriterTables {
		for _, shape := range []string{
			"CREATE TRIGGER sync_writer_guard BEFORE INSERT OR UPDATE OR DELETE ON " + name + "\n FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();",
			"CREATE TRIGGER sync_writer_touch AFTER INSERT OR UPDATE OR DELETE ON " + name + "\n FOR EACH ROW EXECUTE FUNCTION witself_sync_writer_guard();",
			"CREATE TRIGGER sync_writer_truncate BEFORE TRUNCATE ON " + name + "\n FOR EACH STATEMENT EXECUTE FUNCTION witself_sync_writer_guard();",
		} {
			if strings.Count(sql, shape) != 1 {
				t.Errorf("sync inventory guard missing or duplicated: %s", name)
			}
		}
	}
	if !strings.Contains(sql, "CREATE CONSTRAINT TRIGGER sync_tx_coverage AFTER INSERT OR UPDATE ON sync_tx_admissions\n DEFERRABLE INITIALLY DEFERRED") {
		t.Fatal("sync inventory deferred coverage missing")
	}
	if strings.Contains(sql, "pg_advisory_xact_lock") {
		t.Fatal("sync inventory guard illegally acquires gate")
	}
	if SyncSchemaVersion() != 2 {
		t.Fatalf("sync inventory version coordinate: %d", SyncSchemaVersion())
	}
}

func TestSyncCaptureMigrationConstraintRemovalIsBounded(t *testing.T) {
	for _, sql := range []string{
		`ALTER TABLE sync_writer_capabilities DROP CONSTRAINT sync_writer_capabilities_pkey;`,
		`ALTER TABLE sync_capture DROP CONSTRAINT sync_capture_pkey;`,
		`ALTER TABLE sync_writer_capabilities DROP CONSTRAINT sync_writer_capabilities_enabled_check CASCADE;`,
	} {
		mutated := copiedSyncMigrationFS(t)
		mutated[fmt.Sprintf("sync_migrations/%04d_unreviewed_constraint.sql", SyncSchemaVersion()+1)] = &fstest.MapFile{Data: []byte(sql)}
		if err := validateSyncArchiveRegistryContract(mutated); !errors.Is(err, errSyncMigrationSQL) {
			t.Fatalf("sync inventory constraint removal survived: %v", err)
		}
	}
}

func TestSyncWriterInventoryInstalled(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncSchemaTestStore(t)
	if err := st.MigrateSync(ctx); err != nil {
		t.Fatal("sync inventory installed migration failed")
	}
	if err := validateInstalledSyncWriterInventory(ctx, st); err != nil {
		t.Fatalf("sync inventory catalog completeness: %v", err)
	}
	// A table can acquire account ownership after CREATE. Catalog validation
	// must reject that change even when its old exclusion still exists.
	if _, err := st.pool.Exec(ctx, `ALTER TABLE account_purge_sweep_state ADD COLUMN account_id text`); err != nil {
		t.Fatalf("sync inventory ALTER mutation SQLSTATE: %s", syncTestSQLState(err))
	}
	if err := validateInstalledSyncWriterInventory(ctx, st); !errors.Is(err, errSyncWriterInventory) {
		t.Fatalf("sync inventory ALTER mutation survived: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `ALTER TABLE account_purge_sweep_state DROP COLUMN account_id`); err != nil {
		t.Fatalf("sync inventory ALTER cleanup SQLSTATE: %s", syncTestSQLState(err))
	}
}

func validateInstalledSyncWriterInventory(ctx context.Context, st *Store) error {
	expected := map[string]bool{}
	for _, table := range canonicalArchiveTables {
		expected[table.name] = true
	}
	for table := range cellLocalArchiveExclusions {
		expected[table] = true
	}
	for _, table := range syncArchiveTables {
		expected[table.name] = true
	}
	for table := range syncLocalArchiveExclusions {
		expected[table] = true
	}
	rows, err := st.pool.Query(ctx, `SELECT c.relname,
		EXISTS(SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attname='account_id' AND NOT a.attisdropped),
		(SELECT count(*) FROM pg_trigger t WHERE t.tgrelid=c.oid AND NOT t.tgisinternal AND t.tgenabled='O'
		 AND t.tgname IN ('sync_writer_guard','sync_writer_touch','sync_writer_truncate'))
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=current_schema() AND c.relkind IN ('r','p')
		AND c.relname NOT IN ('goose_db_version','witself_sync_schema_version')`)
	if err != nil {
		return errSyncWriterInventory
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var account bool
		var triggers int
		if rows.Scan(&name, &account, &triggers) != nil || !expected[name] {
			return errSyncWriterInventory
		}
		delete(expected, name)
		covered := slices.Contains(syncWriterTables, name)
		if covered && triggers != 3 {
			return errSyncWriterInventory
		}
		if !covered && (syncWriterExclusions[name] == "" || (account && !strings.HasPrefix(name, "sync_"))) {
			return errSyncWriterInventory
		}
	}
	if rows.Err() != nil || len(expected) != 0 {
		return errSyncWriterInventory
	}
	return nil
}

func validateSyncWriterInventory(core, sync fs.FS, inventory []string) error {
	if !slices.IsSorted(inventory) {
		return errSyncWriterInventory
	}
	covered := map[string]bool{}
	for _, name := range inventory {
		if covered[name] || syncWriterExclusions[name] != "" {
			return errSyncWriterInventory
		}
		covered[name] = true
	}
	registered := map[string]bool{}
	for _, table := range canonicalArchiveTables {
		registered[table.name] = true
	}
	for name := range cellLocalArchiveExclusions {
		registered[name] = true
	}
	for _, table := range syncArchiveTables {
		registered[table.name] = true
	}
	for name := range syncLocalArchiveExclusions {
		registered[name] = true
	}
	seen := map[string]bool{}
	for _, source := range []struct {
		fs  fs.FS
		dir string
	}{{core, "migrations"}, {sync, "sync_migrations"}} {
		entries, err := fs.ReadDir(source.fs, source.dir)
		if err != nil {
			return errSyncWriterInventory
		}
		for _, entry := range entries {
			raw, err := fs.ReadFile(source.fs, source.dir+"/"+entry.Name())
			if err != nil {
				return errSyncWriterInventory
			}
			sql := string(raw)
			if source.dir == "migrations" {
				// Core DDL predates the sync namespace's stricter literal rules.
				// Reconcile its reviewed CREATE forms independently; installed
				// PostgreSQL columns are checked by the integration contract.
				creates := regexp.MustCompile(`(?is)CREATE\s+(?:(?:UNLOGGED|TEMPORARY|TEMP)\s+)?TABLE(?:\s+IF\s+NOT\s+EXISTS)?\s+((?:"(?:[^"]|"")+"|\w+)(?:\s*\.\s*(?:"(?:[^"]|"")+"|\w+))?)\s*\((.*?)\);`)
				for _, alter := range regexp.MustCompile(`(?is)ALTER\s+TABLE\s+"?(\w+)"?\s+([^;]+);`).FindAllStringSubmatch(strings.Split(sql, "-- +goose Down")[0], -1) {
					if !covered[alter[1]] && regexp.MustCompile(`(?i)\bADD\s+(?:COLUMN\s+)?"?account_id"?\s+`).MatchString(alter[2]) {
						return fmt.Errorf("%w: ALTER adds account ownership", errSyncWriterInventory)
					}
				}
				up := strings.Split(sql, "-- +goose Down")[0]
				matches := creates.FindAllStringSubmatch(up, -1)
				if len(matches) != len(regexp.MustCompile(`(?i)CREATE\s+(?:(?:UNLOGGED|TEMPORARY|TEMP)\s+)?TABLE\b`).FindAllString(up, -1)) {
					return fmt.Errorf("%w: unparsed core table", errSyncWriterInventory)
				}
				for _, match := range matches {
					identifiers, err := syncSQLStatements(match[1])
					if err != nil || len(identifiers) != 1 {
						return errSyncWriterInventory
					}
					name, _, err := syncSQLRelation(identifiers[0])
					if err != nil {
						return errSyncWriterInventory
					}
					if seen[name] || !registered[name] || (!covered[name] && syncWriterExclusions[name] == "") {
						return fmt.Errorf("%w: core table registration", errSyncWriterInventory)
					}
					seen[name] = true
					if !covered[name] && regexp.MustCompile(`(?i)\baccount_id\s+text\b`).MatchString(match[2]) {
						return fmt.Errorf("%w: account table exclusion", errSyncWriterInventory)
					}
				}
				continue
			}
			statements, err := syncSQLStatements(sql)
			if err != nil {
				return fmt.Errorf("%w: SQL tokenization", errSyncWriterInventory)
			}
			for _, statement := range statements {
				if len(statement) < 2 || !statement[0].keyword("CREATE") {
					continue
				}
				tableStart := statement[1].keyword("TABLE") || statement[1].keyword("UNLOGGED") || statement[1].keyword("TEMP") || statement[1].keyword("TEMPORARY")
				if !tableStart {
					continue
				}
				name, body, isTable, err := syncSQLCreateTable(statement[1:])
				if err != nil || !isTable || seen[name] || !registered[name] {
					return fmt.Errorf("%w: table registration", errSyncWriterInventory)
				}
				seen[name] = true
				if !covered[name] && syncWriterExclusions[name] == "" {
					return fmt.Errorf("%w: missing table %s", errSyncWriterInventory, name)
				}
				if !covered[name] && source.dir == "migrations" {
					for _, token := range body {
						if token.keyword("account_id") {
							return fmt.Errorf("%w: account table exclusion", errSyncWriterInventory)
						}
					}
				}
			}
		}
	}
	for name := range registered {
		if !seen[name] {
			return fmt.Errorf("%w: registry relation missing", errSyncWriterInventory)
		}
	}
	for name := range covered {
		if !seen[name] {
			return errSyncWriterInventory
		}
	}
	for name, reason := range syncWriterExclusions {
		if !seen[name] || reason == "" {
			return errSyncWriterInventory
		}
	}
	return nil
}
