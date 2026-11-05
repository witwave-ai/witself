package store

import (
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

var syncPrincipalFunctionSignatures = []string{
	"capture_lock_key(text)",
	"witself_sync_writer_guard()",
	"witself_sync_check_coverage()",
	"witself_sync_immutable_evidence()",
	"witself_sync_publication_transition()",
	"sync_admit(text,text,text)",
	"sync_touch(text,bigint,text,text,jsonb,jsonb,bytea,bytea,bytea)",
	"sync_seal(text,bigint,bytea)",
	"sync_finalize(text,bigint)",
}

// Keep the column/custody parser closed: only this reviewed ACL topology may be
// inventory-neutral. New roles, function signatures or DDL grants need review.
func syncPrincipalACLStatements() []string {
	const owner = "witself_sync_owner"
	const runtime = "witself_sync_server, witself_sync_worker, witself_sync_maintenance"
	const all = runtime + ", witself_sync_snapshot, witself_sync_legacy"
	var statements []string
	for _, table := range SyncWriterInventory() {
		statements = append(statements,
			"ALTER TABLE "+table+" OWNER TO "+owner,
			"REVOKE ALL ON TABLE "+table+" FROM PUBLIC, "+all,
			"GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLE "+table+" TO "+runtime+", witself_sync_legacy",
			"GRANT SELECT ON TABLE "+table+" TO witself_sync_snapshot")
	}
	var syncTables []string
	for name := range syncSchemaProjection {
		syncTables = append(syncTables, name)
	}
	slices.Sort(syncTables)
	for _, table := range syncTables {
		statements = append(statements,
			"ALTER TABLE "+table+" OWNER TO "+owner,
			"REVOKE ALL ON TABLE "+table+" FROM PUBLIC, "+all,
			"GRANT SELECT ON TABLE "+table+" TO "+all)
	}
	for _, signature := range syncPrincipalFunctionSignatures {
		statements = append(statements,
			"ALTER FUNCTION "+signature+" OWNER TO "+owner,
			"REVOKE EXECUTE ON FUNCTION "+signature+" FROM PUBLIC, "+all)
		if strings.HasPrefix(signature, "sync_") {
			statements = append(statements, "GRANT EXECUTE ON FUNCTION "+signature+" TO "+runtime)
		} else if strings.HasPrefix(signature, "capture_lock_key(") {
			statements = append(statements, "GRANT EXECUTE ON FUNCTION "+signature+" TO "+all)
		}
	}
	return append(statements, "ALTER FUNCTION witself_sync_check_coverage() SECURITY DEFINER")
}

var syncPrincipalACLKeys = func() map[string]bool {
	keys := make(map[string]bool)
	for _, expected := range syncPrincipalACLStatements() {
		parsed, err := syncSQLStatements(expected)
		if err == nil && len(parsed) == 1 {
			keys[strings.ToLower(syncSQLText(parsed[0]))] = true
		}
	}
	return keys
}()

func syncSQLPrincipalStatement(tokens []syncSQLToken) bool {
	if len(tokens) == 0 || !tokens[0].keyword("ALTER") && !tokens[0].keyword("GRANT") && !tokens[0].keyword("REVOKE") {
		return false
	}
	return syncPrincipalACLKeys[strings.ToLower(syncSQLText(tokens))]
}

func validateSyncPrincipalSQL(raw string) error {
	up, down, found := strings.Cut(raw, "-- +goose Down")
	if !found || !strings.Contains(down, "ERRCODE='42501'") || !strings.Contains(down, "ERRCODE='23514'") {
		return errors.New("principal SQL downgrade refusal missing")
	}
	statements, err := syncSQLStatements(up)
	if err != nil {
		return err
	}
	actual := make(map[string]int)
	for _, statement := range statements {
		actual[strings.ToLower(syncSQLText(statement))]++
		if len(statement) > 1 && statement[0].keyword("CREATE") &&
			(statement[1].keyword("ROLE") || statement[1].keyword("USER")) {
			return errors.New("principal SQL creates cluster object")
		}
		if (statement[0].keyword("GRANT") || statement[0].keyword("REVOKE")) && !syncSQLPrincipalStatement(statement) {
			return errors.New("principal SQL unreviewed privilege")
		}
	}
	for _, expected := range syncPrincipalACLStatements() {
		parsed, err := syncSQLStatements(expected)
		if err != nil || actual[strings.ToLower(syncSQLText(parsed[0]))] != 1 {
			return errors.New("principal SQL topology statement missing or duplicated")
		}
	}
	for _, function := range []string{"sync_admit", "sync_touch", "sync_seal", "sync_finalize"} {
		_, definition, found := strings.Cut(up, "CREATE FUNCTION "+function+"(")
		definition, _, ended := strings.Cut(definition, "-- +goose StatementEnd")
		if !found || !ended || !strings.Contains(definition, "LANGUAGE plpgsql SECURITY DEFINER SET search_path FROM CURRENT AS $$") {
			return errors.New("principal SQL protected definition missing")
		}
	}
	if strings.Count(up, "admission.database_principal<>session_user OR admission.backend_pid<>pg_backend_pid()") != 4 {
		return errors.New("principal SQL identity fence missing")
	}
	manifest := SyncCoverageManifestVersion + "\n" + strings.Join(SyncWriterInventory(), "\n") + "\n"
	if !strings.Contains(up, "convert_to($manifest$"+manifest+"$manifest$,'UTF8')") {
		return errors.New("principal SQL coverage commitment differs")
	}

	for _, fragment := range []string{
		"ALTER FUNCTION %s SET search_path = pg_catalog, %I, pg_temp",
		"signature,current_schema()",
		"ALTER SCHEMA %I OWNER TO witself_sync_owner",
		"REVOKE ALL ON SCHEMA %I FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy",
		"ALTER TABLE sync_tx_admissions ADD COLUMN database_principal text NOT NULL DEFAULT session_user;",
		"ALTER TABLE sync_tx_admissions ADD COLUMN backend_pid integer NOT NULL DEFAULT pg_backend_pid();",
		"admission.database_principal<>session_user OR admission.backend_pid<>pg_backend_pid()",
		"session_user<>current_user AND target_principal<>session_user",
		"RAISE EXCEPTION 'capability_revoked' USING ERRCODE='P0001'",
		"WHERE c.snapshot_id IS NOT NULL",
		"IF capture.snapshot_id IS NULL AND NOT EXISTS",
		"REVOKE ALL (%s) ON TABLE %I FROM PUBLIC, witself_sync_server, witself_sync_worker, witself_sync_maintenance, witself_sync_snapshot, witself_sync_legacy",
		"FOR unexpected IN",
		"a.privilege_type='EXECUTE' AND a.grantee<>p.proowner",
		"coalesce(r.rolname IN ('witself_sync_server','witself_sync_worker','witself_sync_maintenance'),false)",
		"REVOKE EXECUTE ON FUNCTION %s FROM PUBLIC CASCADE",
		"REVOKE EXECUTE ON FUNCTION %s FROM %I CASCADE",
	} {
		if !strings.Contains(up, fragment) {
			return errors.New("principal SQL security fence missing")
		}
	}
	for _, signature := range syncPrincipalFunctionSignatures {
		if !strings.Contains(up, "'"+signature+"'") {
			return errors.New("principal SQL pinned function missing")
		}
	}
	return nil
}

func syncPrincipalSQL(t *testing.T) string {
	t.Helper()
	raw, err := fs.ReadFile(syncMigrationsFS, "sync_migrations/0003_sync_principals.sql")
	if err != nil {
		t.Fatal("principal SQL migration read failed")
	}
	return string(raw)
}

func TestSyncPrincipalMigrationContract(t *testing.T) {
	if SyncSchemaVersion() != 3 {
		t.Fatal("principal SQL schema version differs")
	}
	if err := validateSyncPrincipalSQL(syncPrincipalSQL(t)); err != nil {
		t.Fatalf("principal SQL contract refused: %v", err)
	}
	if err := validateSyncArchiveRegistryContract(syncMigrationsFS); err != nil {
		t.Fatalf("principal SQL registry refused: %v", err)
	}
}

func TestSyncPrincipalMigrationRejectsTopologyMutations(t *testing.T) {
	source := syncPrincipalSQL(t)
	mutations := []struct{ name, old, replacement string }{
		{"public execution", "REVOKE EXECUTE ON FUNCTION sync_admit(text,text,text) FROM PUBLIC,", "REVOKE EXECUTE ON FUNCTION sync_admit(text,text,text) FROM"},
		{"runtime owner", "ALTER TABLE sync_capture OWNER TO witself_sync_owner;", "ALTER TABLE sync_capture OWNER TO witself_sync_worker;"},
		{"control dml", "GRANT SELECT ON TABLE sync_capture TO", "GRANT SELECT, UPDATE ON TABLE sync_capture TO"},
		{"missing definer", "RETURNS void LANGUAGE plpgsql SECURITY DEFINER", "RETURNS void LANGUAGE plpgsql SECURITY INVOKER"},
		{"temporary shadow", "SET search_path = pg_catalog, %I, pg_temp", "SET search_path = pg_catalog, %I"},
		{"login identity", "admission.database_principal<>session_user", "admission.database_principal<>current_user"},
		{"backend identity", "admission.backend_pid<>pg_backend_pid()", "false"},
		{"revocation guard", "IF capture.snapshot_id IS NULL AND NOT EXISTS", "IF NOT EXISTS"},
		{"downgrade permission", "ERRCODE='42501'", "ERRCODE='23514'"},
		{"unexpected function ACL", "REVOKE EXECUTE ON FUNCTION %s FROM %I CASCADE", "SELECT %s, %I"},
		{"unexpected PUBLIC ACL", "REVOKE EXECUTE ON FUNCTION %s FROM PUBLIC CASCADE", "SELECT %s"},
		{"unexpected ACL allowlist", "coalesce(r.rolname IN ('witself_sync_server','witself_sync_worker','witself_sync_maintenance'),false)", "true"},
	}
	for _, table := range SyncWriterInventory() {
		mutations = append(mutations, struct{ name, old, replacement string }{
			"covered owner " + table, "ALTER TABLE " + table + " OWNER TO witself_sync_owner;", "SELECT 1;"})
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(source, tc.old) {
				t.Fatal("principal SQL mutation target missing")
			}
			if err := validateSyncPrincipalSQL(strings.Replace(source, tc.old, tc.replacement, 1)); err == nil {
				t.Fatal("principal SQL mutation was accepted")
			}
		})
	}
}

func TestSyncPrincipalACLParserRemainsBounded(t *testing.T) {
	for _, statement := range []string{
		"GRANT ALL ON TABLE sync_capture TO witself_sync_worker",
		"GRANT SELECT ON TABLE sync_capture TO PUBLIC",
		"ALTER TABLE accounts OWNER TO witself_sync_worker",
		"ALTER TABLE unreviewed_relation OWNER TO witself_sync_owner",
		"ALTER FUNCTION unreviewed_function() OWNER TO witself_sync_owner",
		"ALTER FUNCTION sync_admit(text,text,text) SECURITY INVOKER",
		"GRANT CREATE ON SCHEMA public TO witself_sync_worker",
	} {
		parsed, err := syncSQLStatements(statement)
		if err != nil || len(parsed) != 1 || syncSQLPrincipalStatement(parsed[0]) {
			t.Fatal("principal SQL ACL parser accepted mutation")
		}
	}
}

// Allowed execution never includes the authority to delegate execution. The
// catalog mutation test exercises the same check against PostgreSQL ACL rows.
func TestSyncPrincipalTopologyACLContract(t *testing.T) {
	const closedACL = "WHERE a.privilege_type='EXECUTE' AND a.grantee<>p.proowner\n AND (a.is_grantable OR NOT (p.runtime_execute AND coalesce(grantee.rolname=ANY(($3::text[])[2:4]),false))))"
	if !strings.Contains(syncPrincipalTopologySQL, closedACL) {
		t.Fatal("principal topology grants execution outside owner custody")
	}
}
