package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/syncharness"
	"github.com/witwave-ai/witself/internal/testenv"
)

// The shared schema fixture executes the exact protected function bodies, with
// their schema pinning, but cannot claim role/ACL evidence. Only this _test.go
// projection removes the cluster-role-dependent ownership and grant block.
func syncPrincipalLocalMigrationFS() fs.FS {
	return syncPrincipalLocalMigrationSource(syncMigrationsFS)
}

func syncPrincipalLocalMigrationSource(source fs.FS) fs.FS {
	files := fstest.MapFS{}
	entries, _ := fs.ReadDir(source, "sync_migrations")
	for _, entry := range entries {
		name := "sync_migrations/" + entry.Name()
		raw, _ := fs.ReadFile(source, name)
		if strings.HasPrefix(entry.Name(), "0003_") {
			before, rest, ok := strings.Cut(string(raw), "-- BEGIN SYNC PRINCIPAL TOPOLOGY")
			_, after, end := strings.Cut(rest, "-- END SYNC PRINCIPAL TOPOLOGY")
			if !ok || !end {
				panic("principal fixture topology boundary missing")
			}
			raw = []byte(before + after)
		}
		files[name] = &fstest.MapFile{Data: raw}
	}
	return files
}

func syncPrincipalNamedMigrationFS(roles []string) fs.FS {
	files := fstest.MapFS{}
	pairs := []string{}
	for i, name := range syncPrincipalRoleNames() {
		pairs = append(pairs, name, roles[i])
	}
	replace := strings.NewReplacer(pairs...)
	entries, _ := fs.ReadDir(syncMigrationsFS, "sync_migrations")
	for _, entry := range entries {
		name := "sync_migrations/" + entry.Name()
		raw, _ := fs.ReadFile(syncMigrationsFS, name)
		files[name] = &fstest.MapFile{Data: []byte(replace.Replace(string(raw)))}
	}
	return files
}

func newSyncPrincipalCluster(t *testing.T) (*Store, *testenv.DisposablePostgresCluster, Principal) {
	t.Helper()
	fixture := testenv.DisposableCluster(t)
	if fixture == nil {
		return nil, nil, Principal{}
	}
	st, err := Open(context.Background(), fixture.AdminDSN())
	if err != nil {
		t.Fatal("principal check cluster store open failed")
	}
	t.Cleanup(st.Close)
	roles := syncPrincipalRoleNames()
	for i, role := range roles {
		roles[i] = fixture.Role(role)
	}
	st.syncPrincipalTest = &syncPrincipalTestOptions{roles: roles, migrations: syncPrincipalNamedMigrationFS(roles)}
	ctx := context.Background()
	if st.Migrate() != nil || st.VerifySyncPrincipals(ctx) != nil || st.MigrateSync(ctx) != nil ||
		st.SelectSyncCapture(runtimeconfig.Config{Local: true, SyncSchema: true}) != nil {
		t.Fatal("principal check cluster schema setup failed")
	}
	p := seedSyncPrincipalAccount(t, st)
	return st, fixture, p
}

func seedSyncPrincipalAccount(t *testing.T, st *Store) Principal {
	t.Helper()
	p := Principal{Kind: PrincipalAgent, AccountID: "capture_account", RealmID: "capture_realm", ID: "capture_agent", AccountStatus: "active"}
	for _, query := range []string{
		`INSERT INTO accounts(id,display_name,is_default,status) VALUES('capture_account','Capture fixture',false,'active')`,
		`INSERT INTO realms(id,account_id,name) VALUES('capture_realm','capture_account','default')`,
		`INSERT INTO agents(id,realm_id,name) VALUES('capture_agent','capture_realm','first'),('capture_second','capture_realm','second')`,
		`INSERT INTO sync_incarnations(account_id,lifecycle_epoch,restore_marker,allocation_id,transition_id,authority_installation,state) VALUES('capture_account',1,decode(repeat('01',16),'hex'),decode(repeat('02',16),'hex'),decode(repeat('03',16),'hex'),'local','installed')`,
	} {
		if _, err := st.pool.Exec(context.Background(), query); err != nil {
			t.Fatalf("principal check cluster seed SQLSTATE %s", syncTestSQLState(err))
		}
	}
	return p
}

func openSyncPrincipalRole(t *testing.T, fixture *testenv.DisposablePostgresCluster, role SyncPrincipalRole) *Store {
	t.Helper()
	st, err := Open(context.Background(), fixture.RoleDSN(string(role)))
	if err != nil {
		t.Fatal("principal check role store open failed")
	}
	t.Cleanup(st.Close)
	if st.SelectSyncCapture(runtimeconfig.Config{Local: true, SyncSchema: true}) != nil {
		t.Fatal("principal check role capture selection failed")
	}
	return st
}

func principalEvidence(t *testing.T, checks, required int) {
	t.Helper()
	if t.Failed() || checks != required {
		t.Log("principal check evidence withheld for partial execution")
		return
	}
	artifact, err := syncharness.VerifyPrincipalRuntimeArtifact(t.Name(), checks)
	if err != nil {
		t.Fatal("principal check evidence binding failed")
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal("principal check evidence serialization failed")
	}
	t.Logf("principal evidence %s", raw)
}

func TestSyncPrincipalActivationTopology(t *testing.T) {
	st, p := newSyncCaptureFixture(t, false)
	ctx := context.Background()
	st.syncPrincipalTest.singleLogin = false
	before := syncPrincipalState(t, st, p.AccountID)
	// Reach the actual installed ownership predicates independently of role
	// preflight. The local login owns every covered relation and function; using
	// it as the expected owner isolates the forbidden current-user comparisons.
	roles := syncPrincipalRoleNames()
	if st.pool.QueryRow(ctx, `SELECT current_user`).Scan(&roles[0]) != nil {
		t.Fatal("principal topology local owner lookup failed")
	}
	st.syncPrincipalTest.roles = roles
	t.Run("owner_identity", func(t *testing.T) {
		if st.verifySyncPrincipalTopologyPredicate(ctx, st.pool, syncPrincipalTopologyOwnerIdentity) != ErrSyncPrincipalTopologyUnverified {
			t.Fatal("principal topology owner identity predicate accepted current login")
		}
		syncPrincipalUnchanged(t, st, p.AccountID, before)
	})
	t.Run("owner_drain", func(t *testing.T) {
		if st.verifySyncPrincipalTopologyPredicate(ctx, st.pool, syncPrincipalTopologyOwnerDrain) != nil {
			t.Fatal("principal topology drained owner predicate refused")
		}
		ownerTx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal("principal topology local owner transaction unavailable")
		}
		defer func() { _ = ownerTx.Rollback(ctx) }()
		if _, err := ownerTx.Exec(ctx, `SELECT 1 FROM agent_dashboard_preferences FOR SHARE`); err != nil {
			t.Fatal("principal topology local owner lock failed")
		}
		if st.verifySyncPrincipalTopologyPredicate(ctx, st.pool, syncPrincipalTopologyOwnerDrain) != ErrSyncPrincipalTopologyUnverified {
			t.Fatal("principal topology undrained owner predicate accepted")
		}
		if ownerTx.Rollback(ctx) != nil || st.verifySyncPrincipalTopologyPredicate(ctx, st.pool, syncPrincipalTopologyOwnerDrain) != nil {
			t.Fatal("principal topology owner drain did not restore predicate")
		}
		syncPrincipalUnchanged(t, st, p.AccountID, before)
	})
	t.Run("function_attributes_and_acl", func(t *testing.T) {
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal("principal topology function transaction unavailable")
		}
		defer func() { _ = tx.Rollback(ctx) }()
		// This predicate checks installed function attributes independently of
		// role existence and ownership identity. Use schema-specific absent
		// runtime identities so fixed shared fixture roles cannot change reach.
		localRoles := append([]string(nil), roles...)
		var schema string
		if tx.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema) != nil {
			t.Fatal("principal topology schema lookup failed")
		}
		for i := 1; i < len(localRoles); i++ {
			localRoles[i] = schema + "_absent_" + strconv.Itoa(i)
		}
		probe := &Store{syncPrincipalTest: &syncPrincipalTestOptions{roles: localRoles}}
		for _, signature := range []string{"sync_admit(text,text,text)", "sync_touch(text,bigint,text,text,jsonb,jsonb,bytea,bytea,bytea)",
			"sync_seal(text,bigint,bytea)", "sync_finalize(text,bigint)", "witself_sync_writer_guard()", "witself_sync_check_coverage()"} {
			if _, err := tx.Exec(ctx, `REVOKE EXECUTE ON FUNCTION `+signature+` FROM PUBLIC`); err != nil {
				t.Fatal("principal topology local function ACL setup failed")
			}
		}
		if probe.verifySyncPrincipalTopologyPredicate(ctx, tx, syncPrincipalTopologyFunctionACL) != nil {
			t.Fatal("principal topology isolated function predicate refused baseline")
		}
		for _, query := range []string{
			`ALTER FUNCTION sync_seal(text,bigint,bytea) SET search_path=public`,
			`GRANT EXECUTE ON FUNCTION sync_seal(text,bigint,bytea) TO PUBLIC`,
		} {
			if _, err := tx.Exec(ctx, `SAVEPOINT function_predicate`); err != nil {
				t.Fatal("principal topology function savepoint failed")
			}
			if _, err := tx.Exec(ctx, query); err != nil {
				t.Fatal("principal topology function deviation failed")
			}
			if probe.verifySyncPrincipalTopologyPredicate(ctx, tx, syncPrincipalTopologyFunctionACL) != ErrSyncPrincipalTopologyUnverified {
				t.Fatal("principal topology isolated function deviation accepted")
			}
			if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT function_predicate`); err != nil {
				t.Fatal("principal topology function deviation rollback failed")
			}
		}
		_ = tx.Rollback(ctx)
		syncPrincipalUnchanged(t, st, p.AccountID, before)
	})
	options := SyncCaptureOptions{LifecycleEpoch: 1, RestoreMarker: bytes.Repeat([]byte{1}, 16), Capability: SyncPreferencesCapability}
	activation, err := st.ActivateSyncCapture(ctx, p.AccountID, options)
	if activation != nil {
		activation.Close()
	}
	if !errors.Is(err, ErrSyncPrincipalTopologyUnverified) {
		t.Fatal("principal check owner topology did not refuse")
	}
	var count int
	if st.pool.QueryRow(ctx, `SELECT count(*) FROM sync_capture`).Scan(&count) != nil || count != 0 {
		t.Fatal("principal check topology refusal changed control state")
	}
	st.syncPrincipalTest.singleLogin = true
	activateSyncFixture(t, st, p.AccountID)
	principalEvidence(t, 2, 2)
}

func TestSyncPrincipalMissingMigrationRefuses(t *testing.T) {
	for _, missing := range syncPrincipalRoleNames() {
		t.Run(missing, func(t *testing.T) {
			st, _ := newSyncSchemaTestStore(t)
			reader := newPrincipalCatalogReader()
			delete(reader.roles, missing)
			st.syncPrincipalTest = &syncPrincipalTestOptions{roleCatalog: reader}
			ctx := context.Background()
			if err := st.VerifySyncPrincipals(ctx); !errors.Is(err, ErrSyncPrincipalsMissing) {
				t.Fatal("principal check single missing role preflight did not refuse")
			}
			if err := st.MigrateSync(ctx); !errors.Is(err, ErrSyncPrincipalsMissing) {
				t.Fatal("principal check single missing role migration did not refuse")
			}
			if version, err := st.AppliedSyncSchemaVersion(ctx); err != nil || version != 2 {
				t.Fatal("principal check refused migration advanced principal schema")
			}
			var absent bool
			if st.pool.QueryRow(ctx, `SELECT
 to_regprocedure('sync_admit(text,text,text)') IS NULL
 AND to_regprocedure('sync_touch(text,bigint,text,text,jsonb,jsonb,bytea,bytea,bytea)') IS NULL
 AND to_regprocedure('sync_seal(text,bigint,bytea)') IS NULL
 AND to_regprocedure('sync_finalize(text,bigint)') IS NULL
 AND NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='sync_tx_admissions'::regclass
 AND attname IN ('database_principal','backend_pid') AND NOT attisdropped)`).Scan(&absent) != nil || !absent {
				t.Fatal("principal check refused migration left 0003 objects")
			}
		})
	}
}

func TestSyncPrincipalCatalogRollback(t *testing.T) {
	st, dsn := newSyncSchemaTestStore(t)
	ctx := context.Background()
	var superuser, createRole bool
	if st.pool.QueryRow(ctx, `SELECT rolsuper,rolcreaterole FROM pg_roles WHERE rolname=current_user`).Scan(&superuser, &createRole) != nil {
		t.Fatal("principal check role allocation preflight failed")
	}
	if !superuser && !createRole {
		t.Skip("NOT RUN createrole_unavailable")
	}
	db := migrationTestSQLDB(t, dsn)
	defer func() { _ = db.Close() }()
	source, err := fs.Sub(syncMigrationsFS, "sync_migrations")
	if err != nil {
		t.Fatal("principal check migration source unavailable")
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, source,
		goose.WithTableName(syncSchemaVersionTable), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal("principal check migration provider unavailable")
	}
	if _, err := provider.UpTo(ctx, 2); err != nil {
		t.Fatal("principal check predecessor schema setup failed")
	}
	withSyncPrincipalCatalogRoles(ctx, t, st.pool, func(tx pgx.Tx, roles []string) {
		if verifySyncPrincipals(ctx, tx, roles) != nil {
			t.Fatal("principal check transactional role preflight failed")
		}
		for _, attribute := range []string{"SUPERUSER", "CREATEDB", "CREATEROLE", "BYPASSRLS", "INHERIT", "REPLICATION", "NOLOGIN"} {
			t.Run(attribute, func(t *testing.T) {
				if !superuser && (attribute == "SUPERUSER" || attribute == "BYPASSRLS" || attribute == "REPLICATION") {
					t.Skip("NOT RUN superuser_attribute_unavailable")
				}
				if _, err := tx.Exec(ctx, `SAVEPOINT principal_attribute`); err != nil {
					t.Fatal("principal check attribute savepoint failed")
				}
				if _, err := tx.Exec(ctx, `ALTER ROLE `+pgx.Identifier{roles[2]}.Sanitize()+` `+attribute); err != nil {
					t.Fatal("principal check attribute mutation failed")
				}
				if verifySyncPrincipals(ctx, tx, roles) != ErrSyncPrincipalsMissing {
					t.Fatal("principal check forbidden attribute survived preflight")
				}
				if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT principal_attribute`); err != nil {
					t.Fatal("principal check attribute rollback failed")
				}
			})
		}
		raw, _ := fs.ReadFile(syncPrincipalNamedMigrationFS(roles), "sync_migrations/0003_sync_principals.sql")
		up, _, ok := strings.Cut(string(raw), "-- +goose Down")
		if !ok {
			t.Fatal("principal check migration direction boundary missing")
		}
		// An unrelated default grantee must not survive creation and owner transfer
		// of the protected functions. This catalog change rolls back with the roles.
		if _, err := tx.Exec(ctx, `ALTER DEFAULT PRIVILEGES GRANT EXECUTE ON FUNCTIONS TO pg_monitor`); err != nil {
			t.Fatal("principal check default function grant fixture failed")
		}
		if _, err := tx.Exec(ctx, up); err != nil {
			t.Fatalf("principal check transactional migration SQLSTATE %s", syncTestSQLState(err))
		}
		st.syncPrincipalTest = &syncPrincipalTestOptions{roles: roles}
		if st.verifySyncPrincipalTopology(ctx, tx) != nil {
			t.Fatal("principal check transactional catalog topology failed")
		}
		for _, mutation := range []struct{ name, sql string }{
			{"column_control_grant", `GRANT UPDATE(coverage_state) ON sync_capture TO ` + pgx.Identifier{roles[2]}.Sanitize()},
			{"public_execute", `GRANT EXECUTE ON FUNCTION sync_admit(text,text,text) TO PUBLIC`},
			{"unreviewed_execute", `GRANT EXECUTE ON FUNCTION sync_admit(text,text,text) TO CURRENT_USER`},
			{"runtime_execute_grant_option", `GRANT EXECUTE ON FUNCTION sync_admit(text,text,text) TO ` + pgx.Identifier{roles[2]}.Sanitize() + ` WITH GRANT OPTION`},
			{"invoker_function", `ALTER FUNCTION sync_admit(text,text,text) SECURITY INVOKER`},
			{"untrusted_search_path", `ALTER FUNCTION sync_admit(text,text,text) SET search_path=public`},
			{"missing_signature_with_overload", `DROP FUNCTION sync_finalize(text,bigint); CREATE FUNCTION sync_touch(text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$BEGIN RETURN; END;$$`},
			{"control_table_grant", `GRANT DELETE ON sync_tx_touches TO ` + pgx.Identifier{roles[2]}.Sanitize()},
		} {
			t.Run(mutation.name, func(t *testing.T) {
				if _, err := tx.Exec(ctx, `SAVEPOINT principal_topology`); err != nil {
					t.Fatal("principal check topology savepoint failed")
				}
				if _, err := tx.Exec(ctx, mutation.sql); err != nil {
					t.Fatal("principal check topology mutation failed")
				}
				if st.verifySyncPrincipalTopology(ctx, tx) != ErrSyncPrincipalTopologyUnverified {
					t.Fatal("principal check topology mutation survived refusal")
				}
				if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT principal_topology`); err != nil {
					t.Fatal("principal check topology rollback failed")
				}
			})
		}
		if _, err := tx.Exec(ctx, `GRANT `+pgx.Identifier{roles[0]}.Sanitize()+` TO `+pgx.Identifier{roles[2]}.Sanitize()); err != nil {
			t.Fatal("principal check transactional membership mutation failed")
		}
		if verifySyncPrincipals(ctx, tx, roles) != ErrSyncPrincipalsMissing {
			t.Fatal("principal check owner membership mutation survived")
		}
		if tx.Rollback(ctx) != nil {
			t.Fatal("principal check catalog rollback failed")
		}
		var residue bool
		if st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=ANY($1))`, roles).Scan(&residue) != nil || residue {
			t.Fatal("principal check transactional role residue found")
		}
		var protected bool
		if st.pool.QueryRow(ctx, `SELECT to_regprocedure('sync_admit(text,text,text)') IS NOT NULL`).Scan(&protected) != nil || protected {
			t.Fatal("principal check transactional function residue found")
		}
		t.Log("principal catalog dry-run ROLLED BACK role_residue=0; not a disposable-cluster runtime receipt")
	})
}

func TestSyncPrincipalCapabilityRevocation(t *testing.T) {
	completed := 0
	for _, commit := range []bool{false, true} {
		name := "rollback"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			st, p := newSyncCaptureFixture(t, true)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			writer, err := st.AdmitSyncTx(ctx, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability)
			if err != nil {
				t.Fatal("principal check revoke writer admission failed")
			}
			defer func() { _ = writer.Rollback(context.Background()) }()
			if _, err := putSyncDashboardPreferencesSQL(ctx, writer.Tx(), p, syncPreferenceDocument("amber")); err != nil ||
				writer.Touch(ctx, SyncPreferencesCapability, "preferences", syncPreferenceDocument("amber"), json.RawMessage(`{}`)) != nil || writer.Seal(ctx) != nil {
				t.Fatal("principal check revoke writer seal failed")
			}
			result := make(chan error, 1)
			go func() { result <- st.RevokeSyncCapability(ctx, p.AccountID, SyncPreferencesPrincipal) }()
			capturePoll(t, st, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND mode='ShareLock' AND granted)
 AND EXISTS(SELECT 1 FROM pg_stat_activity WHERE datid=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND pid<>$1 AND pid<>pg_backend_pid() AND query LIKE '%pg_try_advisory_lock(capture_lock_key%')`, writer.Tx().Conn().PgConn().PID())
			select {
			case <-result:
				t.Fatal("principal check revoke passed an admitted writer")
			default:
			}
			if commit {
				err = writer.Commit(ctx)
			} else {
				err = writer.Rollback(ctx)
			}
			if err != nil {
				t.Fatal("principal check writer completion failed")
			}
			select {
			case err = <-result:
				if err != nil {
					t.Fatal("principal check revoke after drain failed")
				}
			case <-ctx.Done():
				t.Fatal("principal check revoke drain timed out")
			}
			var revoked bool
			if st.pool.QueryRow(ctx, `SELECT NOT enabled AND revoked_generation=1 AND capture_epoch=2 FROM sync_writer_capabilities WHERE account_id=$1 AND principal=$2`, p.AccountID, SyncPreferencesPrincipal).Scan(&revoked) != nil || !revoked {
				t.Fatal("principal check revoke generation or epoch differs")
			}
			for _, functions := range []bool{false, true} {
				st.syncPrincipalTest.functions = functions
				requireSyncPrincipalRevokedAdmission(t, func() (*SyncTx, error) {
					return st.AdmitSyncTx(ctx, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability)
				})
			}
			_, err = st.pool.Exec(ctx, `UPDATE agents SET name='forbidden' WHERE id=$1`, p.ID)
			captureRefusedSQL(t, err)
			t.Log("principal revoke observed granted writer ShareLock and pending exclusive poll before writer completion")
			completed++
		})
	}
	principalEvidence(t, completed, 2)
}

func TestSyncPrincipalDualPath(t *testing.T) {
	type projection struct {
		Preferences, Effects, Dependencies, Kind string
		Touches, Bundles, Operations, Finalized  int
	}
	var baseline *projection
	for _, functions := range []bool{false, true} {
		name := "owner_sql"
		if functions {
			name = "protected_functions"
		}
		t.Run(name, func(t *testing.T) {
			st, p := newSyncCaptureFixture(t, true)
			st.syncPrincipalTest.functions = functions
			ctx := context.Background()
			second := p
			second.ID = "capture_second"
			for _, tc := range []struct {
				name  string
				fault syncPreferencesFault
			}{
				{"omit_touch", syncPreferencesFault{omitTouch: true}},
				{"omit_seal", syncPreferencesFault{omitSeal: true}},
				{"empty_seal", syncPreferencesFault{emptySeal: true}},
				{"touch_one_of_two", syncPreferencesFault{second: &second}},
				{"same_row_twice", syncPreferencesFault{repeatWrite: true}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if _, err := st.putDashboardPreferencesSync(ctx, p, syncPreferenceDocument("amber"), tc.fault); !errors.Is(err, ErrSyncWriterNotAdmitted) {
						t.Fatalf("principal check omission refusal SQLSTATE %s", syncTestSQLState(err))
					}
					var empty bool
					if st.pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM agent_dashboard_preferences)
 AND NOT EXISTS(SELECT 1 FROM sync_tx_admissions) AND NOT EXISTS(SELECT 1 FROM sync_tx_touches)
 AND NOT EXISTS(SELECT 1 FROM sync_operations) AND NOT EXISTS(SELECT 1 FROM sync_operation_payloads)
 AND NOT EXISTS(SELECT 1 FROM sync_bundles) AND NOT EXISTS(SELECT 1 FROM sync_bundle_operations)
 AND (SELECT last_finalized FROM sync_finalized_counter WHERE account_id=$1)=0`, p.AccountID).Scan(&empty) != nil || !empty {
						t.Fatal("principal check omission left durable state")
					}
				})
			}
			for _, guard := range []string{"foreign_session_guc", "backend_writer", "backend_seal"} {
				t.Run(guard, func(t *testing.T) {
					before := syncPrincipalState(t, st, p.AccountID)
					writer, err := st.AdmitSyncTx(ctx, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability)
					if err != nil {
						t.Fatal("principal backend admission setup failed")
					}
					defer func() { _ = writer.Rollback(ctx) }()
					foreign, err := st.pool.Begin(ctx)
					if err != nil {
						t.Fatal("principal backend second session setup failed")
					}
					defer func() { _ = foreign.Rollback(ctx) }()
					foreignPID := foreign.Conn().PgConn().PID()
					if foreignPID == writer.Tx().Conn().PgConn().PID() {
						t.Fatal("principal backend fixture reused physical session")
					}
					if guard == "foreign_session_guc" {
						if _, err := foreign.Exec(ctx, `SELECT set_config('witself.sync_tx',$1,true)`, strconv.FormatInt(writer.id, 10)); err != nil {
							t.Fatal("principal backend foreign GUC setup failed")
						}
						_, err = foreign.Exec(ctx, `INSERT INTO agent_dashboard_preferences(agent_id,account_id,realm_id,prefs)
 VALUES($1,$2,$3,$4)`, p.ID, p.AccountID, p.RealmID, syncPreferenceDocument("blue"))
					} else {
						// The admission remains in its own transaction and retains its
						// principal, epoch and GUC. Only the recorded backend differs.
						if guard == "backend_seal" {
							if _, err := putSyncDashboardPreferencesSQL(ctx, writer.Tx(), p, syncPreferenceDocument("blue")); err != nil ||
								writer.Touch(ctx, SyncPreferencesCapability, "preferences", syncPreferenceDocument("blue"), json.RawMessage(`{}`)) != nil {
								t.Fatal("principal backend seal setup failed")
							}
						}
						if _, err := writer.Tx().Exec(ctx, `UPDATE sync_tx_admissions SET backend_pid=$2 WHERE tx_id=$1`, writer.id, foreignPID); err != nil {
							t.Fatal("principal backend identity fixture failed")
						}
						var isolated bool
						if writer.Tx().QueryRow(ctx, `SELECT tx_id=txid_current() AND database_principal=session_user
 AND current_setting('witself.sync_tx')=tx_id::text AND backend_pid=$2 AND backend_pid<>pg_backend_pid()
 FROM sync_tx_admissions WHERE tx_id=$1`, writer.id, foreignPID).Scan(&isolated) != nil || !isolated {
							t.Fatal("principal backend fixture changed another identity guard")
						}
						if guard == "backend_seal" {
							_, err = writer.Tx().Exec(ctx, `SELECT sync_seal($1,$2,$3)`, p.AccountID, writer.id, bytes.Repeat([]byte{1}, 16))
						} else {
							_, err = writer.Tx().Exec(ctx, `INSERT INTO agent_dashboard_preferences(agent_id,account_id,realm_id,prefs)
 VALUES($1,$2,$3,$4)`, p.ID, p.AccountID, p.RealmID, syncPreferenceDocument("blue"))
						}
					}
					message := ErrSyncWriterNotAdmitted.Error()
					if guard == "backend_seal" {
						message = "admission principal mismatch"
					}
					syncPrincipalSQLRefused(t, err, "P0001", message)
					_ = foreign.Rollback(ctx)
					_ = writer.Rollback(ctx)
					syncPrincipalUnchanged(t, st, p.AccountID, before)
				})
			}
			if _, err := st.PutDashboardPreferences(ctx, p, syncPreferenceDocument("amber")); err != nil {
				t.Fatalf("principal check dual writer SQLSTATE %s", syncTestSQLState(err))
			}
			var got projection
			if st.pool.QueryRow(ctx, `SELECT p.prefs::text,b.semantic_effects::text,b.dependencies::text,o.operation_kind,
 (SELECT count(*) FROM sync_tx_touches),(SELECT count(*) FROM sync_bundles),(SELECT count(*) FROM sync_operations),f.last_finalized
 FROM agent_dashboard_preferences p CROSS JOIN sync_bundles b CROSS JOIN sync_operations o CROSS JOIN sync_finalized_counter f`).Scan(
				&got.Preferences, &got.Effects, &got.Dependencies, &got.Kind, &got.Touches, &got.Bundles, &got.Operations, &got.Finalized) != nil {
				t.Fatal("principal check dual projection query failed")
			}
			if got.Touches != 1 || got.Bundles != 1 || got.Operations != 1 || got.Finalized != 1 || got.Kind != SyncPreferencesCapability {
				t.Fatal("principal check dual projection counts differ")
			}
			if baseline == nil {
				baseline = &got
			} else if !reflect.DeepEqual(*baseline, got) {
				t.Fatal("principal check protected and owner projections differ")
			}
			writer, err := st.AdmitSyncTx(ctx, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability)
			if err != nil {
				t.Fatal("principal check savepoint admission failed")
			}
			defer func() { _ = writer.Rollback(ctx) }()
			cut := errors.New("fixture savepoint cut")
			err = writer.Savepoint(ctx, func(child *SyncTx) error {
				if _, err := putSyncDashboardPreferencesSQL(ctx, child.Tx(), p, syncPreferenceDocument("blue")); err != nil {
					return err
				}
				if err := child.Touch(ctx, SyncPreferencesCapability, "preferences", syncPreferenceDocument("blue"), json.RawMessage(`{}`)); err != nil {
					return err
				}
				if err := child.Seal(ctx); err != nil {
					return err
				}
				return cut
			})
			if !errors.Is(err, cut) || writer.sealed {
				t.Fatal("principal check savepoint did not restore handle")
			}
			var touches int
			if writer.Tx().QueryRow(ctx, `SELECT count(*) FROM sync_tx_touches WHERE tx_id=$1`, writer.id).Scan(&touches) != nil || touches != 0 {
				t.Fatal("principal check savepoint did not restore touches")
			}
		})
	}
}
