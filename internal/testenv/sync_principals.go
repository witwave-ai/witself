package testenv

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type syncPrincipalExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// EnsureSyncPrincipalRoles provisions only absent reviewed fixture roles. The
// caller supplies store.SyncPrincipalRoleNames so testenv does not import store:
// store's own tests already depend on testenv. The administrator must have role
// creation authority. Existing roles are validated, never repaired or dropped;
// fixed names deliberately survive schema-fixture cleanup on a shared server.
func EnsureSyncPrincipalRoles(ctx context.Context, db syncPrincipalExecutor, roles ...string) error {
	refused := errors.New("sync_principal_fixture_roles_refused")
	if len(roles) != 6 {
		return refused
	}
	seen := make(map[string]bool, len(roles))
	for _, role := range roles {
		if role == "" || strings.Trim(role, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" || seen[role] {
			return refused
		}
		seen[role] = true
	}
	for index, role := range roles {
		login, nonOwnerChecks := "NOLOGIN", ""
		if index != 0 {
			login = "LOGIN"
			nonOwnerChecks = `
   WHEN NOT r.rolcanlogin THEN 'login'
   WHEN EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.member=r.oid) THEN 'membership'
   WHEN has_parameter_privilege(r.oid,'session_replication_role','SET') THEN 'replication_parameter'`
		}
		// The advisory lock serializes callers within one database. Roles are
		// cluster-wide, so another database can still race CREATE ROLE; catch
		// only duplicate-name races and independently validate the resulting row.
		// Each block is atomic, including validation of an already present role.
		// Match store.VerifySyncPrincipals: the owner may have either LOGIN
		// state, membership or parameter privileges; only the five nonowners
		// require LOGIN and prohibit those two indirect privilege routes.
		statement := `DO $$ DECLARE r pg_roles%ROWTYPE; invalid_attribute text; BEGIN
 PERFORM pg_advisory_xact_lock(hashtextextended('testenv.EnsureSyncPrincipalRoles:` + role + `',0));
 IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '` + role + `') THEN
  BEGIN
   CREATE ROLE ` + pgx.Identifier{role}.Sanitize() + ` ` + login + ` NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
  EXCEPTION WHEN duplicate_object OR unique_violation THEN
   NULL;
  END;
 END IF;
 SELECT * INTO r FROM pg_roles WHERE rolname = '` + role + `';
 IF NOT FOUND THEN
  invalid_attribute := 'missing';
 ELSE
  invalid_attribute := CASE
   WHEN r.rolsuper THEN 'superuser'
   WHEN r.rolcreaterole THEN 'createrole'
   WHEN r.rolcreatedb THEN 'createdb'
   WHEN r.rolbypassrls THEN 'bypassrls'
   WHEN r.rolinherit THEN 'inherit'
   WHEN r.rolreplication THEN 'replication'` + nonOwnerChecks + `
   ELSE NULL END;
 END IF;
 IF invalid_attribute IS NOT NULL THEN
  RAISE EXCEPTION USING MESSAGE = 'sync_principal_fixture_roles_refused', DETAIL = invalid_attribute;
 END IF;
END $$`
		if _, err := db.Exec(ctx, statement); err != nil {
			// Do not expose driver messages, role values or arbitrary server detail.
			attribute := "catalog"
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "P0001" && pgErr.Message == refused.Error() {
				switch pgErr.Detail {
				case "missing", "superuser", "createrole", "createdb", "bypassrls", "inherit", "replication", "login", "membership", "replication_parameter":
					attribute = pgErr.Detail
				}
			}
			return fmt.Errorf("%w: role_index=%d attribute=%s", refused, index, attribute)
		}
	}
	return nil
}
