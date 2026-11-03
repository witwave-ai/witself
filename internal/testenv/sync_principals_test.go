package testenv_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/testenv"
)

type syncPrincipalRecordingExecutor struct {
	roles      []string
	present    map[string]bool
	statements []string
	creates    int
	unsafe     string
	attribute  string
	driverErr  error
	raceRole   string
	raceError  string
}

func (f *syncPrincipalRecordingExecutor) Exec(_ context.Context, statement string, _ ...any) (pgconn.CommandTag, error) {
	f.statements = append(f.statements, statement)
	for _, role := range f.roles {
		if !strings.Contains(statement, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" ") {
			continue
		}
		if role == f.unsafe && strings.Contains(statement, "RAISE EXCEPTION USING MESSAGE = 'sync_principal_fixture_roles_refused', DETAIL = invalid_attribute") {
			if f.driverErr != nil {
				return pgconn.CommandTag{}, f.driverErr
			}
			if strings.Contains(statement, "'"+f.attribute+"'") {
				return pgconn.CommandTag{}, &pgconn.PgError{Code: "P0001", Message: "sync_principal_fixture_roles_refused", Detail: f.attribute}
			}
		}
		if !f.present[role] && f.raceRole == role {
			// Model the catalog check missing the role, then another database creating
			// it before this block's CREATE. The fixed SQL must catch both server error
			// classes and validate the row installed by that concurrent creator.
			f.present[role] = true
			if !strings.Contains(statement, "EXCEPTION WHEN duplicate_object OR unique_violation THEN") ||
				!strings.Contains(statement, "SELECT * INTO r FROM pg_roles WHERE rolname") {
				return pgconn.CommandTag{}, &pgconn.PgError{Code: f.raceError, Message: "private fixture conflict"}
			}
			return pgconn.NewCommandTag("DO"), nil
		}
		if !f.present[role] || !strings.Contains(statement, "IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '"+role+"')") {
			f.present[role] = true
			f.creates++
		}
		return pgconn.NewCommandTag("DO"), nil
	}
	return pgconn.CommandTag{}, errors.New("unexpected fixture statement")
}

func TestEnsureSyncPrincipalRolesGuardedSQLAndIdempotence(t *testing.T) {
	roles := store.SyncPrincipalRoleNames()
	fake := &syncPrincipalRecordingExecutor{roles: roles, present: make(map[string]bool)}
	for range 2 {
		if err := testenv.EnsureSyncPrincipalRoles(context.Background(), fake, roles...); err != nil {
			t.Fatal("reviewed fixture role provisioning refused")
		}
	}
	if fake.creates != 6 || len(fake.statements) != 12 {
		t.Fatal("fixture role provisioning was not idempotent")
	}
	for index, role := range roles {
		statement := fake.statements[index]
		login := "NOLOGIN"
		if index != 0 {
			login = "LOGIN"
		}
		for _, required := range []string{
			"DO $$ DECLARE r pg_roles%ROWTYPE; invalid_attribute text; BEGIN",
			"pg_advisory_xact_lock(hashtextextended('testenv.EnsureSyncPrincipalRoles:" + role + "',0))",
			"IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '" + role + "') THEN",
			"CREATE ROLE " + pgx.Identifier{role}.Sanitize() + " " + login + " NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;",
			"EXCEPTION WHEN duplicate_object OR unique_violation THEN",
			"SELECT * INTO r FROM pg_roles WHERE rolname = '" + role + "'",
			"IF NOT FOUND THEN\n  invalid_attribute := 'missing';",
			"WHEN r.rolinherit THEN 'inherit'", "WHEN r.rolsuper THEN 'superuser'", "WHEN r.rolcreatedb THEN 'createdb'",
			"WHEN r.rolcreaterole THEN 'createrole'", "WHEN r.rolreplication THEN 'replication'", "WHEN r.rolbypassrls THEN 'bypassrls'",
			"IF invalid_attribute IS NOT NULL THEN",
			"RAISE EXCEPTION USING MESSAGE = 'sync_principal_fixture_roles_refused', DETAIL = invalid_attribute",
		} {
			if !strings.Contains(statement, required) {
				t.Fatal("fixture role guarded SQL contract incomplete")
			}
		}
		for _, nonOwnerCheck := range []string{
			"WHEN NOT r.rolcanlogin THEN 'login'",
			"WHEN EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.member=r.oid) THEN 'membership'",
			"WHEN has_parameter_privilege(r.oid,'session_replication_role','SET') THEN 'replication_parameter'",
		} {
			if strings.Contains(statement, nonOwnerCheck) != (index != 0) {
				t.Fatal("fixture role owner and nonowner validation differ from production")
			}
		}
		if strings.Contains(statement, "ALTER ") || strings.Contains(statement, "DROP ") || strings.Contains(statement, "GRANT ") ||
			strings.Contains(statement, "PASSWORD") ||
			strings.Count(statement, "CREATE ROLE ") != 1 || statement != fake.statements[index+6] ||
			strings.Index(statement, "pg_advisory_xact_lock") > strings.Index(statement, "IF NOT EXISTS") {
			t.Fatal("fixture role SQL can mutate an existing role or race creation")
		}
	}
}

// This models the cross-database race at the SQL boundary; it does not run
// simultaneous role DDL against any shared PostgreSQL database.
func TestEnsureSyncPrincipalRolesConcurrentCreateRace(t *testing.T) {
	for _, code := range []string{"42710", "23505"} {
		t.Run(code, func(t *testing.T) {
			roles := store.SyncPrincipalRoleNames()
			fake := &syncPrincipalRecordingExecutor{roles: roles, present: make(map[string]bool), raceRole: roles[2], raceError: code}
			for range 2 {
				if testenv.EnsureSyncPrincipalRoles(context.Background(), fake, roles...) != nil {
					t.Fatal("fixture role concurrent creation did not settle on reviewed existing row")
				}
			}
			if len(fake.present) != 6 || fake.creates != 5 || len(fake.statements) != 12 {
				t.Fatal("fixture role concurrent creation changed idempotence")
			}
		})
	}
}

func TestEnsureSyncPrincipalRolesExistingUnsafeRefusesWithoutRepair(t *testing.T) {
	roles := store.SyncPrincipalRoleNames()
	for index, role := range roles {
		t.Run(role, func(t *testing.T) {
			attributes := []string{"missing", "superuser", "createrole", "createdb", "bypassrls", "inherit", "replication"}
			if index != 0 {
				attributes = append(attributes, "login", "membership", "replication_parameter")
			}
			for _, attribute := range attributes {
				t.Run(attribute, func(t *testing.T) {
					present := make(map[string]bool, len(roles))
					for _, existing := range roles {
						present[existing] = true
					}
					fake := &syncPrincipalRecordingExecutor{roles: roles, present: present, unsafe: role, attribute: attribute}
					err := testenv.EnsureSyncPrincipalRoles(context.Background(), fake, roles...)
					want := fmt.Sprintf("sync_principal_fixture_roles_refused: role_index=%d attribute=%s", index, attribute)
					if err == nil || err.Error() != want || fake.creates != 0 || len(fake.statements) != index+1 {
						t.Fatal("unsafe existing fixture role was repaired or did not produce the value-free refusal detail")
					}
				})
			}
		})
	}
}

func TestEnsureSyncPrincipalRolesDriverErrorsAreValueFree(t *testing.T) {
	roles := store.SyncPrincipalRoleNames()
	for _, driverErr := range []error{
		errors.New("private driver detail"),
		&pgconn.PgError{Code: "P0001", Message: "sync_principal_fixture_roles_refused", Detail: "private server detail"},
		&pgconn.PgError{Code: "23505", Message: "sync_principal_fixture_roles_refused", Detail: "login"},
		&pgconn.PgError{Code: "P0001", Message: "private server message", Detail: "login"},
	} {
		fake := &syncPrincipalRecordingExecutor{roles: roles, present: make(map[string]bool), unsafe: roles[2], driverErr: driverErr}
		err := testenv.EnsureSyncPrincipalRoles(context.Background(), fake, roles...)
		if err == nil || err.Error() != "sync_principal_fixture_roles_refused: role_index=2 attribute=catalog" {
			t.Fatal("fixture role driver error escaped the value-free refusal boundary")
		}
	}
}

func TestEnsureSyncPrincipalRolesInvalidNamesIssueNoSQL(t *testing.T) {
	roles := store.SyncPrincipalRoleNames()
	for _, invalid := range [][]string{nil, roles[:5], append(append([]string{}, roles[:5]...), roles[0]),
		append(append([]string{}, roles[:5]...), "invalid'role")} {
		fake := &syncPrincipalRecordingExecutor{roles: roles, present: make(map[string]bool)}
		if err := testenv.EnsureSyncPrincipalRoles(context.Background(), fake, invalid...); err == nil || len(fake.statements) != 0 {
			t.Fatal("invalid fixture declarations reached SQL")
		}
	}
}

func TestEnsureSyncPrincipalRolesPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	if testenv.ValidateHarnessPostgresDSN(dsn) != nil {
		t.Fatal("sync principal fixture database selection refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("sync principal fixture administrator connection failed")
	}
	defer func() { _ = db.Close(context.Background()) }()
	roles := store.SyncPrincipalRoleNames()
	read := func() []uint32 {
		t.Helper()
		var identities []uint32
		for index, role := range roles {
			var oid uint32
			var login, unsafe, membership, replicationParameter bool
			err := db.QueryRow(ctx, `SELECT r.oid,r.rolcanlogin,
 r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolinherit OR r.rolreplication OR r.rolbypassrls,
 EXISTS(SELECT 1 FROM pg_auth_members m WHERE m.member=r.oid),
 has_parameter_privilege(r.oid,'session_replication_role','SET')
 FROM pg_roles r WHERE r.rolname=$1`, role).Scan(&oid, &login, &unsafe, &membership, &replicationParameter)
			if err != nil || unsafe || (index != 0 && (!login || membership || replicationParameter)) {
				t.Fatal("fixed sync principal fixture attributes differ from the reviewed declaration")
			}
			identities = append(identities, oid)
		}
		return identities
	}
	if testenv.EnsureSyncPrincipalRoles(ctx, db, roles...) != nil {
		t.Fatal("fixed sync principal fixture first provisioning failed")
	}
	before := read()
	if testenv.EnsureSyncPrincipalRoles(ctx, db, roles...) != nil || !reflect.DeepEqual(before, read()) {
		t.Fatal("fixed sync principal fixture second provisioning changed existing roles")
	}
}
