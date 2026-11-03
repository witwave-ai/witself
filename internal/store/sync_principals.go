package store

import (
	"context"
	"errors"
	"io/fs"

	"github.com/jackc/pgx/v5"
)

// SyncPrincipalRole names a reviewed database identity, never a configuration
// value or an application-supplied identity.
type SyncPrincipalRole string

const (
	// SyncPrincipalOwner owns the reviewed local sync and covered domain objects.
	SyncPrincipalOwner SyncPrincipalRole = "witself_sync_owner"
	// SyncPrincipalServer is the admitted server login.
	SyncPrincipalServer SyncPrincipalRole = "witself_sync_server"
	// SyncPrincipalWorker is the admitted worker login.
	SyncPrincipalWorker SyncPrincipalRole = "witself_sync_worker"
	// SyncPrincipalMaintenance is the admitted maintenance login.
	SyncPrincipalMaintenance SyncPrincipalRole = "witself_sync_maintenance"
	// SyncPrincipalSnapshot has read-only access.
	SyncPrincipalSnapshot SyncPrincipalRole = "witself_sync_snapshot"
	// SyncPrincipalLegacy has domain DML but no protected function access.
	SyncPrincipalLegacy SyncPrincipalRole = "witself_sync_legacy"
)

var (
	// ErrSyncPrincipalsMissing refuses absent or unsafe role declarations.
	ErrSyncPrincipalsMissing = errors.New("sync_principals_missing")
	// ErrSyncPrincipalTopologyUnverified refuses activation without custody proof.
	ErrSyncPrincipalTopologyUnverified = errors.New("sync_principal_topology_unverified")
	// ErrSyncCapabilityRevoked refuses admission after a drained revocation.
	ErrSyncCapabilityRevoked = errors.New("capability_revoked")
)

// No production caller constructs this unexported seam. The schema-only shared
// test fixture does not provision cluster roles; its SQL projection and activation
// bypass are explicitly weaker evidence than the disposable-cluster tests.
type syncPrincipalTestOptions struct {
	migrations     fs.FS
	roles          []string
	singleLogin    bool
	functions      bool
	topologyBypass syncPrincipalTopologyPredicate
	roleCatalog    syncVersionReader
}

// SyncPrincipalRoleNames returns a fresh copy of the reviewed role inventory.
func SyncPrincipalRoleNames() []string {
	return syncPrincipalRoleNames()
}

func syncPrincipalRoleNames() []string {
	return []string{string(SyncPrincipalOwner), string(SyncPrincipalServer), string(SyncPrincipalWorker),
		string(SyncPrincipalMaintenance), string(SyncPrincipalSnapshot), string(SyncPrincipalLegacy)}
}

func (s *Store) syncPrincipalNames() []string {
	if s.syncPrincipalTest != nil && len(s.syncPrincipalTest.roles) == 6 {
		return s.syncPrincipalTest.roles
	}
	return syncPrincipalRoleNames()
}

// VerifySyncPrincipals performs only catalog reads. Role provisioning belongs
// to an explicit administrative provisioner or test fixture.
func (s *Store) VerifySyncPrincipals(ctx context.Context) error {
	var catalog syncVersionReader = s.pool
	if s.syncPrincipalTest != nil && s.syncPrincipalTest.roleCatalog != nil {
		catalog = s.syncPrincipalTest.roleCatalog
	}
	return verifySyncPrincipals(ctx, catalog, s.syncPrincipalNames())
}

// syncPrincipalRoleCatalog is deliberately row-shaped: SQL retrieves facts and
// Go validates each reviewed identity independently. A missing identity cannot be
// hidden behind another missing row or an aggregate SQL boolean.
type syncPrincipalRoleCatalog struct {
	name                                                                       string
	superuser, createRole, createDB, bypassRLS, inherit, replication, canLogin bool
	membership, replicationParameter                                           bool
}

const syncPrincipalRoleCatalogSQL = `SELECT r.rolname,r.rolsuper,r.rolcreaterole,r.rolcreatedb,
 r.rolbypassrls,r.rolinherit,r.rolreplication,r.rolcanlogin,
 EXISTS(SELECT 1 FROM pg_auth_members m WHERE m.member=r.oid),
 has_parameter_privilege(r.oid,'session_replication_role','SET')
 FROM pg_roles r WHERE r.rolname=$1`

func verifySyncPrincipals(ctx context.Context, q syncVersionReader, roles []string) error {
	if len(roles) != 6 {
		return ErrSyncPrincipalsMissing
	}
	seen := make(map[string]bool, len(roles))
	for _, name := range roles {
		if name == "" || seen[name] {
			return ErrSyncPrincipalsMissing
		}
		seen[name] = true
	}
	for i, name := range roles {
		var role syncPrincipalRoleCatalog
		err := q.QueryRow(ctx, syncPrincipalRoleCatalogSQL, name).Scan(&role.name,
			&role.superuser, &role.createRole, &role.createDB, &role.bypassRLS,
			&role.inherit, &role.replication, &role.canLogin, &role.membership, &role.replicationParameter)
		if err != nil || role.name != name || role.superuser || role.createRole || role.createDB ||
			role.bypassRLS || role.inherit || role.replication ||
			(i != 0 && (!role.canLogin || role.membership || role.replicationParameter)) {
			return ErrSyncPrincipalsMissing
		}
	}
	return nil
}

type syncPrincipalTopologyPredicate uint8

const (
	syncPrincipalTopologyNone syncPrincipalTopologyPredicate = iota
	syncPrincipalTopologyRoles
	syncPrincipalTopologyOwnerIdentity
	syncPrincipalTopologyFunctionACL
	syncPrincipalTopologyOwnerDrain
)

var syncPrincipalTopologyPredicates = [...]syncPrincipalTopologyPredicate{
	syncPrincipalTopologyRoles, syncPrincipalTopologyOwnerIdentity,
	syncPrincipalTopologyFunctionACL, syncPrincipalTopologyOwnerDrain,
}

func (s *Store) verifySyncPrincipalTopology(ctx context.Context, q syncVersionReader) error {
	// The existing schema projection cannot provide custody evidence. The narrower
	// selector below is separate and skips exactly one predicate for regression
	// tests that exercise the remaining real catalog checks.
	if s.syncPrincipalTest != nil && s.syncPrincipalTest.singleLogin {
		return nil
	}
	for _, predicate := range syncPrincipalTopologyPredicates {
		if s.syncPrincipalTest != nil && s.syncPrincipalTest.topologyBypass == predicate {
			continue
		}
		if err := s.verifySyncPrincipalTopologyPredicate(ctx, q, predicate); err != nil {
			return err
		}
	}
	return nil
}

// Individual predicates remain callable from package tests so an unrelated
// role failure cannot mask ownership, ACL, or owner-drain regression coverage.
func (s *Store) verifySyncPrincipalTopologyPredicate(ctx context.Context, q syncVersionReader, predicate syncPrincipalTopologyPredicate) error {
	roles := s.syncPrincipalNames()
	if predicate == syncPrincipalTopologyRoles {
		if verifySyncPrincipals(ctx, q, roles) != nil {
			return ErrSyncPrincipalTopologyUnverified
		}
		return nil
	}
	var query string
	var args []any
	switch predicate {
	case syncPrincipalTopologyOwnerIdentity:
		query, args = syncPrincipalOwnerIdentitySQL, []any{SyncWriterInventory(), roles[0]}
	case syncPrincipalTopologyFunctionACL:
		query, args = syncPrincipalFunctionACLSQL, []any{SyncWriterInventory(), roles[0], roles}
	case syncPrincipalTopologyOwnerDrain:
		query, args = syncPrincipalOwnerDrainSQL, []any{SyncWriterInventory(), roles[0]}
	default:
		return ErrSyncPrincipalTopologyUnverified
	}
	var valid bool
	if err := q.QueryRow(ctx, query, args...).Scan(&valid); err != nil || !valid {
		return ErrSyncPrincipalTopologyUnverified
	}
	return nil
}

const syncPrincipalOwnerIdentitySQL = `SELECT
 EXISTS(SELECT 1 FROM pg_namespace n WHERE n.nspname=current_schema()
 AND n.nspowner=(SELECT oid FROM pg_roles WHERE rolname=$2))
 AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=current_schema() AND (c.relname=ANY($1::text[]) OR starts_with(c.relname,'sync_') OR c.relname='witself_sync_schema_version')
 AND (c.relowner=current_user::regrole OR c.relowner<>(SELECT oid FROM pg_roles WHERE rolname=$2)))
 AND NOT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname=current_schema() AND (starts_with(p.proname,'sync_') OR starts_with(p.proname,'witself_sync_') OR p.proname='capture_lock_key')
 AND (p.proowner=current_user::regrole OR p.proowner<>(SELECT oid FROM pg_roles WHERE rolname=$2)))`

// Effective ACL checks include column grants and PUBLIC. Role preflight
// disallows nonowner membership, so SET ROLE cannot recover an indirect grant.
const syncPrincipalFunctionACLSQL = `WITH expected_functions(signature,runtime_execute) AS (VALUES
 ('sync_admit(text,text,text)',true),
 ('sync_touch(text,bigint,text,text,jsonb,jsonb,bytea,bytea,bytea)',true),
 ('sync_seal(text,bigint,bytea)',true),
 ('sync_finalize(text,bigint)',true),
 ('witself_sync_writer_guard()',false),
 ('witself_sync_check_coverage()',false)
), protected_functions AS (
 SELECT e.runtime_execute,p.* FROM expected_functions e
 LEFT JOIN pg_proc p ON p.oid=to_regprocedure(format('%I.%s',current_schema(),e.signature))
) SELECT cardinality($1::text[])>0 AND
 NOT EXISTS(SELECT 1 FROM pg_roles r WHERE r.rolname=ANY(($3::text[])[2:6])
 AND has_schema_privilege(r.oid,current_schema(),'CREATE'))
 AND NOT EXISTS(SELECT 1 FROM protected_functions p WHERE p.oid IS NULL OR NOT p.prosecdef
 OR p.proowner<>(SELECT oid FROM pg_roles WHERE rolname=$2)
 OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog, '||quote_ident(current_schema())||', pg_temp']
 OR EXISTS(SELECT 1 FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a
 LEFT JOIN pg_roles grantee ON grantee.oid=a.grantee
 WHERE a.privilege_type='EXECUTE' AND a.grantee<>p.proowner
 AND (a.is_grantable OR NOT (p.runtime_execute AND coalesce(grantee.rolname=ANY(($3::text[])[2:4]),false))))
 OR EXISTS(SELECT 1 FROM pg_roles r WHERE r.rolname=ANY(($3::text[])[2:6])
 AND has_function_privilege(r.oid,p.oid,'EXECUTE') IS DISTINCT FROM
 (p.runtime_execute AND r.rolname=ANY(($3::text[])[2:4]))))
 AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 CROSS JOIN pg_roles r WHERE n.nspname=current_schema() AND c.relkind IN ('r','p')
 AND (starts_with(c.relname,'sync_') OR c.relname='witself_sync_schema_version')
 AND r.rolname=ANY(($3::text[])[2:6])
 AND (has_table_privilege(r.oid,c.oid,'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
 OR has_any_column_privilege(r.oid,c.oid,'INSERT,UPDATE,REFERENCES')))`

const syncPrincipalOwnerDrainSQL = `SELECT NOT EXISTS(SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
 JOIN pg_class c ON c.oid=l.relation JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE a.usesysid=(SELECT oid FROM pg_roles WHERE rolname=$2) AND a.pid<>pg_backend_pid()
 AND l.database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND n.nspname=current_schema()
 AND c.relname=ANY($1::text[]))`

// Kept as the reviewed complete topology text for the independent SQL contract.
const syncPrincipalTopologySQL = syncPrincipalOwnerIdentitySQL + syncPrincipalFunctionACLSQL + syncPrincipalOwnerDrainSQL

// syncFunctionAdmission selects the protected path for every nonowner. An
// absent function is never permission to fall back to direct control-table DML.
func syncFunctionAdmission(ctx context.Context, tx pgx.Tx, force bool) (bool, string, error) {
	var owner, functions bool
	var principal string
	err := tx.QueryRow(ctx, `SELECT c.relowner=current_user::regrole,
 to_regprocedure('sync_admit(text,text,text)') IS NOT NULL,session_user
 FROM pg_class c WHERE c.oid='sync_capture'::regclass`).Scan(&owner, &functions, &principal)
	if err != nil || ((!owner || force) && !functions) {
		return false, "", ErrSyncWriterNotAdmitted
	}
	if !owner {
		return true, principal, nil
	}
	return force, "", nil
}
