package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/witwave-ai/witself/internal/syncharness"
)

func syncPrincipalUnavailable(t *testing.T) {
	t.Helper()
	r, err := syncharness.PrincipalRuntimeNotRun(t.Name())
	if err != nil {
		t.Fatal("principal evidence unavailable binding failed")
	}
	t.Logf("principal evidence NOT_RUN %s capability=%s test=%s", r.Reason, r.Capability, t.Name())
	t.Skip("NOT RUN disposable_cluster_unavailable")
}

type syncPrincipalRefusalReporter interface {
	Helper()
	Fatalf(string, ...any)
}

func syncPrincipalSQLRefused(t syncPrincipalRefusalReporter, err error, state, message string) {
	t.Helper()
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != state {
		t.Fatalf("principal refusal expected SQLSTATE %s and reviewed message", state)
		return
	}
	matches := pgerr.Message == message
	if state == "42501" && (strings.HasPrefix(message, "must be owner of") || message == "permission denied to set role") {
		matches = strings.HasPrefix(pgerr.Message, message)
	}
	if !matches {
		// Driver errors may contain generated role names or connection data.
		// Diagnose only the reviewed expected class, never the raw error.
		t.Fatalf("principal refusal expected SQLSTATE %s and reviewed message", state)
	}
}

func syncPrincipalState(t *testing.T, st *Store, account string) string {
	t.Helper()
	var state string
	err := st.pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'domain',(SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY agent_id),'[]'::jsonb) FROM agent_dashboard_preferences t WHERE account_id=$1),
 'capture',(SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY account_id),'[]'::jsonb) FROM sync_capture t WHERE account_id=$1),
 'capabilities',(SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY principal,capability),'[]'::jsonb) FROM sync_writer_capabilities t WHERE account_id=$1),
 'admissions',(SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY tx_id),'[]'::jsonb) FROM sync_tx_admissions t WHERE account_id=$1),
 'touches',(SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY tx_id,touch_id),'[]'::jsonb) FROM sync_tx_touches t WHERE account_id=$1),
 'operations',(SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY operation_id),'[]'::jsonb) FROM sync_operations t WHERE account_id=$1),
 'payloads',(SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY operation_id),'[]'::jsonb) FROM sync_operation_payloads t WHERE account_id=$1),
 'bundles',(SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY bundle_id),'[]'::jsonb) FROM sync_bundles t WHERE account_id=$1),
 'members',(SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY bundle_id,member_order),'[]'::jsonb) FROM sync_bundle_operations t WHERE account_id=$1),
 'counter',(SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY capture_epoch),'[]'::jsonb) FROM sync_finalized_counter t WHERE account_id=$1)
 )::text`, account).Scan(&state)
	if err != nil {
		t.Fatal("principal refusal state snapshot failed")
	}
	return state
}

func syncPrincipalUnchanged(t *testing.T, st *Store, account, before string) {
	t.Helper()
	if syncPrincipalState(t, st, account) != before {
		t.Fatal("principal refusal changed domain or sync state")
	}
	if !syncCaptureCoverageValid(context.Background(), st.pool) {
		t.Fatal("principal refusal damaged installed trigger coverage")
	}
	var functions int
	if err := st.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_proc
 WHERE pronamespace=current_schema()::regnamespace
 AND proname=ANY(ARRAY['sync_admit','sync_touch','sync_seal','sync_finalize']) AND prosecdef`).Scan(&functions); err != nil || functions != 4 {
		t.Fatal("principal refusal damaged protected functions")
	}
}

func TestSyncPrincipalSeparation(t *testing.T) {
	admin, fixture, p := newSyncPrincipalCluster(t)
	if fixture == nil {
		syncPrincipalUnavailable(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if admin.VerifySyncPrincipals(ctx) != nil {
		t.Fatal("principal topology preflight rejected isolated roles")
	}
	if admin.EnableSyncWriter(ctx, p.AccountID, fixture.Role(string(SyncPrincipalWorker)), SyncPreferencesCapability) != nil {
		t.Fatal("principal topology worker capability setup failed")
	}
	ownerStore := openSyncPrincipalRole(t, fixture, SyncPrincipalOwner)
	ownerTx, err := ownerStore.pool.Begin(ctx)
	if err != nil {
		t.Fatal("principal topology owner drain transaction failed")
	}
	defer func() { _ = ownerTx.Rollback(ctx) }()
	var ownerPID int
	if ownerTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&ownerPID) != nil {
		t.Fatal("principal topology owner backend observation failed")
	}
	if _, err := ownerTx.Exec(ctx, `SELECT 1 FROM agent_dashboard_preferences FOR SHARE`); err != nil {
		t.Fatal("principal topology owner relation lock setup failed")
	}
	capturePoll(t, admin, `SELECT EXISTS(SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
 WHERE l.pid=$1 AND l.relation='agent_dashboard_preferences'::regclass AND l.mode='RowShareLock' AND l.granted
 AND a.usesysid=(SELECT oid FROM pg_roles WHERE rolname=$2))`, ownerPID, fixture.Role(string(SyncPrincipalOwner)))
	activation, err := admin.ActivateSyncCapture(ctx, p.AccountID, SyncCaptureOptions{
		LifecycleEpoch: 1, RestoreMarker: bytes.Repeat([]byte{1}, 16), Capability: SyncPreferencesCapability,
	})
	if activation != nil {
		activation.Close()
	}
	if !errors.Is(err, ErrSyncPrincipalTopologyUnverified) {
		t.Fatal("principal topology undrained owner activation accepted")
	}
	if ownerTx.Rollback(ctx) != nil {
		t.Fatal("principal topology owner relation drain failed")
	}
	// Prove the named owner is rejected for ownership identity alone after all
	// independent custody predicates pass on its actual login connection.
	ownerStore.syncPrincipalTest = &syncPrincipalTestOptions{roles: admin.syncPrincipalNames()}
	for _, predicate := range []syncPrincipalTopologyPredicate{
		syncPrincipalTopologyRoles, syncPrincipalTopologyFunctionACL, syncPrincipalTopologyOwnerDrain,
	} {
		if ownerStore.verifySyncPrincipalTopologyPredicate(ctx, ownerStore.pool, predicate) != nil {
			t.Fatal("principal topology named owner isolation predicate failed")
		}
	}
	ownerBefore := syncPrincipalState(t, admin, p.AccountID)
	ownerActivation, ownerErr := ownerStore.ActivateSyncCapture(ctx, p.AccountID, SyncCaptureOptions{
		LifecycleEpoch: 1, RestoreMarker: bytes.Repeat([]byte{1}, 16), Capability: SyncPreferencesCapability,
	})
	if ownerActivation != nil {
		ownerActivation.Close()
	}
	if ownerErr != ErrSyncPrincipalTopologyUnverified {
		t.Fatal("principal topology named owner activation accepted")
	}
	syncPrincipalUnchanged(t, admin, p.AccountID, ownerBefore)
	activateSyncFixture(t, admin, p.AccountID)
	completed := 0
	for _, role := range []SyncPrincipalRole{SyncPrincipalOwner, SyncPrincipalServer, SyncPrincipalWorker, SyncPrincipalMaintenance, SyncPrincipalSnapshot, SyncPrincipalLegacy} {
		t.Run(string(role), func(t *testing.T) {
			st := openSyncPrincipalRole(t, fixture, role)
			var login string
			if st.pool.QueryRow(ctx, `SELECT session_user`).Scan(&login) != nil || login != fixture.Role(string(role)) {
				t.Fatal("principal topology connection identity differs")
			}
			owner, writer := role == SyncPrincipalOwner, role != SyncPrincipalSnapshot
			executor := owner || role == SyncPrincipalServer || role == SyncPrincipalWorker || role == SyncPrincipalMaintenance
			var valid bool
			err := admin.pool.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1 AND rolcanlogin AND NOT rolsuper AND NOT rolcreaterole
 AND NOT rolcreatedb AND NOT rolbypassrls AND NOT rolinherit)
 AND ($3 OR NOT pg_has_role($1,$2,'MEMBER'))
 AND has_schema_privilege($1,current_schema(),'CREATE')=$3
 AND NOT EXISTS(SELECT 1 FROM pg_class c WHERE c.relnamespace=current_schema()::regnamespace
 AND (c.relname=ANY($6::text[]) OR starts_with(c.relname,'sync_') OR c.relname='witself_sync_schema_version')
 AND ((c.relowner=(SELECT oid FROM pg_roles WHERE rolname=$1))<>$3))
 AND NOT EXISTS(SELECT 1 FROM pg_class c WHERE c.relnamespace=current_schema()::regnamespace AND c.relname=ANY($6::text[])
 AND (NOT has_table_privilege($1,c.oid,'SELECT') OR has_table_privilege($1,c.oid,'INSERT')<>$4
 OR has_table_privilege($1,c.oid,'UPDATE')<>$4 OR has_table_privilege($1,c.oid,'DELETE')<>$4
 OR has_table_privilege($1,c.oid,'TRUNCATE')<>$4 OR has_table_privilege($1,c.oid,'TRIGGER')<>$3))
 AND NOT EXISTS(SELECT 1 FROM pg_class c WHERE c.relnamespace=current_schema()::regnamespace AND c.relkind='r'
 AND (starts_with(c.relname,'sync_') OR c.relname='witself_sync_schema_version')
 AND (NOT has_table_privilege($1,c.oid,'SELECT') OR has_table_privilege($1,c.oid,'INSERT')<>$3
 OR has_table_privilege($1,c.oid,'UPDATE')<>$3 OR has_table_privilege($1,c.oid,'DELETE')<>$3
 OR has_table_privilege($1,c.oid,'TRUNCATE')<>$3))
 AND (SELECT count(*) FROM pg_proc f WHERE f.pronamespace=current_schema()::regnamespace
 AND f.proname=ANY(ARRAY['sync_admit','sync_touch','sync_seal','sync_finalize'])
 AND f.proowner=(SELECT oid FROM pg_roles WHERE rolname=$2) AND f.prosecdef
 AND has_function_privilege($1,f.oid,'EXECUTE')=$5
 AND NOT EXISTS(SELECT 1 FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner))) a
 WHERE a.grantee=0 AND a.privilege_type='EXECUTE'))=4`,
				login, fixture.Role(string(SyncPrincipalOwner)), owner, writer, executor, SyncWriterInventory()).Scan(&valid)
			if err != nil || !valid {
				t.Fatal("principal topology ownership grant or attribute mismatch")
			}
			if role == SyncPrincipalWorker {
				if _, err := st.PutDashboardPreferences(ctx, p, syncPreferenceDocument("amber")); err != nil {
					t.Fatal("principal topology genuine worker writer failed")
				}
				var captured bool
				if err := admin.pool.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(a.database_principal=$2 AND a.sealed)
 FROM sync_tx_admissions a JOIN sync_bundles b USING(account_id,tx_id) WHERE a.account_id=$1`, p.AccountID, login).Scan(&captured); err != nil || !captured {
					t.Fatal("principal topology genuine writer lacked bound bundle")
				}
				before := syncPrincipalState(t, admin, p.AccountID)
				tx, err := st.pool.Begin(ctx)
				if err != nil {
					t.Fatal("principal topology temporary shadow transaction failed")
				}
				defer func() { _ = tx.Rollback(ctx) }()
				if _, err := tx.Exec(ctx, `CREATE TEMP TABLE sync_capture(account_id text PRIMARY KEY) ON COMMIT DROP`); err != nil {
					t.Fatal("principal topology temporary control shadow setup failed")
				}
				var admittedID int64
				if err := tx.QueryRow(ctx, `SELECT tx_id FROM sync_admit($1,$2,$3)`, p.AccountID, login, SyncPreferencesCapability).Scan(&admittedID); err != nil || admittedID <= 0 {
					t.Fatal("principal topology protected function followed temporary shadow")
				}
				if tx.Rollback(ctx) != nil {
					t.Fatal("principal topology temporary shadow rollback failed")
				}
				syncPrincipalUnchanged(t, admin, p.AccountID, before)
			}
			completed++
		})
	}
	principalEvidence(t, completed, 6)
}

// Every row is mandatory, even when the disposable runtime lane is unavailable.
// This reviewed list is independent of the executable matrix below.
const syncPrincipalMandatoryBypassRows = `insert_without_admission update_without_admission delete_without_admission
set_role_owner disable_trigger drop_trigger drop_guard_function alter_guard_owner truncate copy_from_stdin
forged_admission_principal forged_seal_principal other_session_setting update_capture update_capabilities delete_touches
legacy_admit legacy_touch legacy_seal legacy_finalize`

type syncPrincipalBypassCase struct {
	name, state, message string
	legacy               bool
	run                  func(context.Context, pgx.Tx) error
}

func syncPrincipalBypassCases(p Principal, role func(SyncPrincipalRole) string, foreignTx, otherSessionID int64) []syncPrincipalBypassCase {
	exec := func(sql string, args ...any) func(context.Context, pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, args...); return err }
	}
	return []syncPrincipalBypassCase{
		{"insert_without_admission", "P0001", "sync writer not admitted", false,
			exec(`INSERT INTO agent_dashboard_preferences(agent_id,account_id,realm_id,prefs) VALUES($1,$2,$3,'{}')`, p.ID+"_missing", p.AccountID, p.RealmID)},
		{"update_without_admission", "P0001", "sync writer not admitted", false,
			exec(`UPDATE agent_dashboard_preferences SET prefs='{}' WHERE agent_id=$1`, p.ID)},
		{"delete_without_admission", "P0001", "sync writer not admitted", false,
			exec(`DELETE FROM agent_dashboard_preferences WHERE agent_id=$1`, p.ID)},
		{"set_role_owner", "42501", "permission denied to set role", false,
			exec(`SET LOCAL ROLE ` + pgx.Identifier{role(SyncPrincipalOwner)}.Sanitize())},
		{"disable_trigger", "42501", "must be owner of table", false,
			exec(`ALTER TABLE agent_dashboard_preferences DISABLE TRIGGER sync_writer_guard`)},
		{"drop_trigger", "42501", "must be owner of", false,
			exec(`DROP TRIGGER sync_writer_guard ON agent_dashboard_preferences`)},
		{"drop_guard_function", "42501", "must be owner of function", false,
			exec(`DROP FUNCTION witself_sync_writer_guard() CASCADE`)},
		{"alter_guard_owner", "42501", "must be owner of function", false,
			exec(`ALTER FUNCTION witself_sync_writer_guard() OWNER TO ` + pgx.Identifier{role(SyncPrincipalWorker)}.Sanitize())},
		{"truncate", "P0001", "sync writer not admitted", false, exec(`TRUNCATE agent_dashboard_preferences`)},
		{"copy_from_stdin", "P0001", "sync writer not admitted", false, func(ctx context.Context, tx pgx.Tx) error {
			// PostgreSQL 16, COPY: COPY FROM invokes destination triggers and CHECK constraints.
			// https://www.postgresql.org/docs/16/sql-copy.html
			_, err := tx.CopyFrom(ctx, pgx.Identifier{"agent_dashboard_preferences"}, []string{"agent_id", "account_id", "realm_id", "prefs"},
				pgx.CopyFromRows([][]any{{p.ID + "_missing", p.AccountID, p.RealmID, "{}"}}))
			return err
		}},
		{"forged_admission_principal", "P0001", "sync writer not admitted", false,
			exec(`SELECT * FROM sync_admit($1,$2,$3)`, p.AccountID, role(SyncPrincipalServer), SyncPreferencesCapability)},
		{"forged_seal_principal", "P0001", "admission principal mismatch", false, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('witself.sync_tx',$1::bigint::text,true)`, foreignTx); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `SELECT sync_seal($1,$2,decode(repeat('01',16),'hex'))`, p.AccountID, foreignTx)
			return err
		}},
		{"other_session_setting", "P0001", "sync writer not admitted", false, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('witself.sync_tx',$1::bigint::text,true)`, otherSessionID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE agent_dashboard_preferences SET prefs='{}' WHERE agent_id=$1`, p.ID)
			return err
		}},
		{"update_capture", "42501", "permission denied for table sync_capture", false, exec(`UPDATE sync_capture SET capture_epoch=capture_epoch+1 WHERE account_id=$1`, p.AccountID)},
		{"update_capabilities", "42501", "permission denied for table sync_writer_capabilities", false, exec(`UPDATE sync_writer_capabilities SET enabled=false WHERE account_id=$1`, p.AccountID)},
		{"delete_touches", "42501", "permission denied for table sync_tx_touches", false, exec(`DELETE FROM sync_tx_touches WHERE account_id=$1`, p.AccountID)},
		{"legacy_admit", "42501", "permission denied for function sync_admit", true, exec(`SELECT * FROM sync_admit($1,$2,$3)`, p.AccountID, role(SyncPrincipalLegacy), SyncPreferencesCapability)},
		{"legacy_touch", "42501", "permission denied for function sync_touch", true, exec(`SELECT sync_touch($1,1,'preferences.put','preferences','{}','{}',decode(repeat('01',16),'hex'),'','')`, p.AccountID)},
		{"legacy_seal", "42501", "permission denied for function sync_seal", true, exec(`SELECT sync_seal($1,1,decode(repeat('01',16),'hex'))`, p.AccountID)},
		{"legacy_finalize", "42501", "permission denied for function sync_finalize", true, exec(`SELECT sync_finalize($1,1)`, p.AccountID)},
	}
}

// The executable and hermetic lanes share this runner. The observer records
// completed assertions; the hermetic reporter also verifies that successful SQL
// reaches the real refusal helper and fails for every reviewed row.
func runSyncPrincipalBypassCase(t syncPrincipalRefusalReporter, tc syncPrincipalBypassCase, execute func(syncPrincipalBypassCase) error, observe func(string)) {
	err := execute(tc)
	syncPrincipalSQLRefused(t, err, tc.state, tc.message)
	if observe != nil {
		observe(tc.name)
	}
}

type syncPrincipalRefusalRecorder struct{ failures int }

func (*syncPrincipalRefusalRecorder) Helper()                 {}
func (r *syncPrincipalRefusalRecorder) Fatalf(string, ...any) { r.failures++ }

func TestSyncPrincipalBypassMatrixContract(t *testing.T) {
	cases := syncPrincipalBypassCases(Principal{ID: "fixture_agent", AccountID: "fixture_account", RealmID: "fixture_realm"},
		func(role SyncPrincipalRole) string { return string(role) }, 1, 2)
	mandatory := strings.Fields(syncPrincipalMandatoryBypassRows)
	if len(cases) != len(mandatory) {
		t.Fatal("principal bypass mandatory matrix size changed")
	}
	rows := make(map[string]syncPrincipalBypassCase, len(cases))
	for _, tc := range cases {
		if _, duplicate := rows[tc.name]; duplicate || tc.run == nil {
			t.Fatal("principal bypass matrix contains duplicate or inert row")
		}
		rows[tc.name] = tc
	}
	for _, name := range mandatory {
		t.Run(name, func(t *testing.T) {
			tc, ok := rows[name]
			if !ok {
				t.Fatal("principal bypass mandatory row missing")
			}
			for _, refused := range []bool{false, true} {
				recorder := &syncPrincipalRefusalRecorder{}
				var events []string
				runSyncPrincipalBypassCase(recorder, tc, func(row syncPrincipalBypassCase) error {
					events = append(events, "execute:"+row.name)
					if refused {
						return &pgconn.PgError{Code: row.state, Message: row.message}
					}
					return nil // A successful bypass must fail the real refusal assertion.
				}, func(name string) { events = append(events, "refusal:"+name) })
				expectedFailures := 1
				if refused {
					expectedFailures = 0
				}
				if recorder.failures != expectedFailures || len(events) != 2 ||
					events[0] != "execute:"+name || events[1] != "refusal:"+name {
					t.Fatal("principal bypass mandatory refusal assertion omitted or accepted successful SQL")
				}
			}
		})
	}
}

func TestSyncPrincipalBypassDenied(t *testing.T) {
	admin, fixture, p := newSyncPrincipalCluster(t)
	if fixture == nil {
		syncPrincipalUnavailable(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, role := range []SyncPrincipalRole{SyncPrincipalWorker, SyncPrincipalServer} {
		if admin.EnableSyncWriter(ctx, p.AccountID, fixture.Role(string(role)), SyncPreferencesCapability) != nil {
			t.Fatal("principal bypass capability setup failed")
		}
	}
	activateSyncFixture(t, admin, p.AccountID)
	worker := openSyncPrincipalRole(t, fixture, SyncPrincipalWorker)
	server := openSyncPrincipalRole(t, fixture, SyncPrincipalServer)
	legacy := openSyncPrincipalRole(t, fixture, SyncPrincipalLegacy)
	if _, err := server.PutDashboardPreferences(ctx, p, syncPreferenceDocument("amber")); err != nil {
		t.Fatal("principal bypass committed foreign admission setup failed")
	}
	var foreignTx int64
	if err := admin.pool.QueryRow(ctx, `SELECT tx_id FROM sync_tx_admissions WHERE account_id=$1 AND database_principal=$2`,
		p.AccountID, fixture.Role(string(SyncPrincipalServer))).Scan(&foreignTx); err != nil {
		t.Fatal("principal bypass foreign admission observation failed")
	}
	otherSession, err := worker.AdmitSyncTx(ctx, p.AccountID, fixture.Role(string(SyncPrincipalWorker)), SyncPreferencesCapability)
	if err != nil {
		t.Fatal("principal bypass other session admission failed")
	}
	defer func() { _ = otherSession.Rollback(ctx) }()
	var uncoveredEdges int
	if err := admin.pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint c
 JOIN pg_class child ON child.oid=c.conrelid JOIN pg_class parent ON parent.oid=c.confrelid
 WHERE c.contype='f' AND c.confdeltype='c' AND child.relnamespace=current_schema()::regnamespace
 AND child.relname=ANY($1::text[]) AND NOT parent.relname=ANY($1::text[])`, SyncWriterInventory()).Scan(&uncoveredEdges); err != nil || uncoveredEdges != 0 {
		t.Fatal("principal bypass uncovered cascade inventory requires review")
	}
	t.Log("principal bypass NOT_RUN uncovered_parent_cascade_absent: no real uncovered-parent cascade into covered inventory")
	cases := syncPrincipalBypassCases(p, func(role SyncPrincipalRole) string {
		return fixture.Role(string(role))
	}, foreignTx, otherSession.id)
	completed := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := syncPrincipalState(t, admin, p.AccountID)
			st := worker
			if tc.legacy {
				st = legacy
			}
			tx, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal("principal bypass transaction setup failed")
			}
			defer func() { _ = tx.Rollback(ctx) }()
			runSyncPrincipalBypassCase(t, tc, func(tc syncPrincipalBypassCase) error {
				err := tc.run(ctx, tx)
				_ = tx.Rollback(ctx)
				return err
			}, nil)
			syncPrincipalUnchanged(t, admin, p.AccountID, before)
			completed++
		})
	}
	principalEvidence(t, completed, len(cases))
}

func TestSyncPrincipalDowngradeDenied(t *testing.T) {
	admin, fixture, p := newSyncPrincipalCluster(t)
	if fixture == nil {
		syncPrincipalUnavailable(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if admin.EnableSyncWriter(ctx, p.AccountID, fixture.Role(string(SyncPrincipalWorker)), SyncPreferencesCapability) != nil {
		t.Fatal("principal downgrade capability setup failed")
	}
	activateSyncFixture(t, admin, p.AccountID)
	completed := 0
	for _, role := range []SyncPrincipalRole{SyncPrincipalWorker, SyncPrincipalOwner} {
		for _, migration := range []string{"0002_sync_capture_guards.sql", "0003_sync_principals.sql"} {
			t.Run(string(role)+"/"+migration, func(t *testing.T) {
				data, err := syncMigrationsFS.ReadFile("sync_migrations/" + migration)
				if err != nil {
					t.Fatal("principal downgrade migration source unavailable")
				}
				_, down, found := strings.Cut(string(data), "-- +goose Down")
				if !found || down == "" {
					t.Fatal("principal downgrade migration refusal absent")
				}
				st := openSyncPrincipalRole(t, fixture, role)
				before := syncPrincipalState(t, admin, p.AccountID)
				tx, err := st.pool.Begin(ctx)
				if err != nil {
					t.Fatal("principal downgrade transaction setup failed")
				}
				defer func() { _ = tx.Rollback(ctx) }()
				_, err = tx.Exec(ctx, down)
				_ = tx.Rollback(ctx)
				state, message := "42501", "sync schema downgrade permission denied"
				if role == SyncPrincipalOwner {
					state, message = "23514", "sync schema downgrade is forbidden"
				}
				syncPrincipalSQLRefused(t, err, state, message)
				syncPrincipalUnchanged(t, admin, p.AccountID, before)
				completed++
			})
		}
	}
	principalEvidence(t, completed, 4)
}
