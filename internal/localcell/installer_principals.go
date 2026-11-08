package localcell

import (
	"context"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/store"
)

// PrincipalLayoutVersion identifies only the explicitly selected fresh layout.
// LayoutVersion and the bootstrap Role retain their original meaning.
const PrincipalLayoutVersion = 1

const installerMigrator = "witself_sync_migrator"

var selectedInstallSteps = []string{"principal_credentials", "principals", "principal_migrations", "principal_grants", "principal_verified"}

func installerRoleNames() []string {
	return append(store.SyncPrincipalRoleNames(), installerMigrator)
}

func validatePrincipalLayout(m *Manifest) error {
	if m.PrincipalLayoutVersion == 0 && len(m.PrincipalRoles) == 0 {
		return nil
	}
	if m.PrincipalLayoutVersion != PrincipalLayoutVersion || !slices.Equal(m.PrincipalRoles, installerRoleNames()) {
		return errPrincipalLayout
	}
	return nil
}

// These operation seams bind the actual journaled installer sequence. Tests can
// substitute bounded in-memory operations without starting a PostgreSQL server.
type installerPrincipalOperations struct {
	credentials, provision, migrate, grants, verify func(context.Context, *Manifest) error
	resumePostgres                                  func(context.Context, *Manifest) error
}

func (c *Cell) principalOperations() installerPrincipalOperations {
	if c.installerPrincipals != nil {
		return *c.installerPrincipals
	}
	return installerPrincipalOperations{
		credentials:    func(_ context.Context, m *Manifest) error { return c.makePrincipalPasswords(m) },
		provision:      c.provisionInstallerPrincipals,
		migrate:        c.runPrincipalMigration,
		grants:         c.grantInstallerStartupAccess,
		verify:         c.verifyInstallerPrincipals,
		resumePostgres: c.startServicePostgres,
	}
}

func (c *Cell) installPrincipals(ctx context.Context, m *Manifest, progress journalProgress) error {
	if err := validatePrincipalLayout(m); err != nil || m.PrincipalLayoutVersion == 0 {
		return errPrincipalLayout
	}
	ops := c.principalOperations()
	fns := []func(context.Context, *Manifest) error{ops.credentials, ops.provision, ops.migrate, ops.grants, ops.verify}
	for i, step := range selectedInstallSteps {
		if progress.done[step] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errPrincipalRepair
		}
		if err := c.step("install", step, func() error { return fns[i](ctx, m) }); err != nil {
			return err
		}
	}
	return nil
}

// A privileged connection is accepted only from this owned, journaled, fresh
// installation. There is intentionally no caller-supplied DSN or admin handle.
func (c *Cell) installerAdmin(ctx context.Context, m *Manifest, fn func(*pgx.Conn) error) error {
	if m.Installed || m.PrincipalLayoutVersion != PrincipalLayoutVersion || validatePrincipalLayout(m) != nil || c.validateRoot(false) != nil {
		return errPrincipalProvision
	}
	durable, err := readManifest(c.cfg.Root)
	if err != nil || durable.InstallationID != m.InstallationID || durable.Installed || validatePrincipalLayout(durable) != nil {
		return errPrincipalProvision
	}
	progress, err := c.readJournal()
	if err != nil || progress.pending == nil || progress.pending.Operation != "install" || !progress.done["database"] {
		return errPrincipalProvision
	}
	state, err := c.processState("postgres")
	if err != nil || !state.Alive || !state.Matches {
		return errPrincipalProvision
	}
	dsn, err := c.principalDSN(m, credentialBootstrap)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return errPrincipalProvision
	}
	return withInstallerConnection(conn, func() error {
		var current, session, database string
		if conn.QueryRow(ctx, "SELECT current_user, session_user, current_database()").Scan(&current, &session, &database) != nil || current != m.Role || session != m.Role || database != localDatabaseName {
			return errPrincipalProvision
		}
		if err := fn(conn); err != nil {
			return errPrincipalProvision
		}
		return nil
	})
}

type installerConnectionCloser interface{ Close(context.Context) error }

func withInstallerConnection(conn installerConnectionCloser, action func() error) error {
	defer closeInstallerConnection(conn)
	return action()
}

// runPrincipalStartup does not permit a runtime launch until all bounded
// verification connections have closed and the privileged-session proof passed.
func runPrincipalStartup(verify, launch func() error) error {
	if err := verify(); err != nil {
		return err
	}
	return launch()
}

func closeInstallerConnection(conn installerConnectionCloser) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

type installerCatalog interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// C2a validates all six role attributes. The installer independently imposes
// NOLOGIN on the owner and the single, non-admin owner membership on migrator.
func validateInstallerCatalog(ctx context.Context, q installerCatalog, verify func(context.Context) error) error {
	if err := verify(ctx); err != nil {
		return errPrincipalProvision
	}
	var valid bool
	if q.QueryRow(ctx, `SELECT NOT rolcanlogin AND NOT EXISTS
 (SELECT 1 FROM pg_auth_members WHERE member=r.oid) AND NOT
 has_parameter_privilege(r.oid,'session_replication_role','SET')
 FROM pg_roles r WHERE rolname='witself_sync_owner'`).Scan(&valid) != nil || !valid {
		return errPrincipalProvision
	}
	if q.QueryRow(ctx, `SELECT rolcanlogin AND NOT (rolsuper OR rolinherit OR rolcreaterole
 OR rolcreatedb OR rolreplication OR rolbypassrls)
 AND NOT has_parameter_privilege(r.oid,'session_replication_role','SET')
 AND (SELECT count(*)=1 AND bool_and(roleid='witself_sync_owner'::regrole AND NOT admin_option)
 FROM pg_auth_members WHERE member=r.oid)
 FROM pg_roles r WHERE rolname='witself_sync_migrator'`).Scan(&valid) != nil || !valid {
		return errPrincipalProvision
	}
	return nil
}

type installerCredentialExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Passwords in CREATE ROLE cannot be bind parameters. Suppress PostgreSQL's
// statement, duration, parameter and error logging before executing that SQL,
// including cancellation/error paths. SET LOCAL confines every change to the
// bounded bootstrap transaction; it changes neither cluster nor runtime policy.
func withInstallerCredentialLogging(ctx context.Context, executor installerCredentialExecutor, action func() error) error {
	for _, setting := range []string{
		"SET LOCAL log_statement = 'none'",
		"SET LOCAL log_min_error_statement = 'panic'",
		"SET LOCAL log_min_messages = 'panic'",
		"SET LOCAL log_duration = off",
		"SET LOCAL log_min_duration_statement = -1",
		"SET LOCAL log_min_duration_sample = -1",
		"SET LOCAL log_transaction_sample_rate = 0",
		"SET LOCAL log_parameter_max_length = 0",
		"SET LOCAL log_parameter_max_length_on_error = 0",
	} {
		if ctx.Err() != nil {
			return errPrincipalProvision
		}
		if _, err := executor.Exec(ctx, setting); err != nil {
			return errPrincipalProvision
		}
	}
	if ctx.Err() != nil {
		return errPrincipalProvision
	}
	return action()
}

func (c *Cell) provisionInstallerPrincipals(ctx context.Context, m *Manifest) error {
	return c.installerAdmin(ctx, m, func(conn *pgx.Conn) error {
		var count int
		if conn.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolname=ANY($1::text[])", installerRoleNames()).Scan(&count) != nil || (count != 0 && count != len(installerRoleNames())) {
			return errPrincipalProvision
		}
		if count == 0 {
			tx, err := conn.Begin(ctx)
			if err != nil {
				return errPrincipalProvision
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := withInstallerCredentialLogging(ctx, tx, func() error {
				for _, name := range installerRoleNames() {
					login := " NOLOGIN"
					if name != string(store.SyncPrincipalOwner) {
						purpose, err := installerPurposeForRole(name)
						if err != nil {
							return err
						}
						password, err := c.credentialPassword(m, purpose)
						if err != nil {
							return err
						}
						login = " LOGIN PASSWORD '" + strings.ReplaceAll(password, "'", "''") + "'"
					}
					if _, err := tx.Exec(ctx, "CREATE ROLE "+pgx.Identifier{name}.Sanitize()+login+" NOINHERIT NOSUPERUSER NOCREATEROLE NOCREATEDB NOREPLICATION NOBYPASSRLS"); err != nil {
						return errPrincipalProvision
					}
				}
				return nil
			}); err != nil {
				return err
			}
			for _, query := range []string{
				"GRANT witself_sync_owner TO witself_sync_migrator",
				"ALTER DATABASE witself OWNER TO witself_sync_owner",
				"ALTER SCHEMA public OWNER TO witself_sync_owner",
				"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
			} {
				if _, err := tx.Exec(ctx, query); err != nil {
					return errPrincipalProvision
				}
			}
			if tx.Commit(ctx) != nil {
				return errPrincipalProvision
			}
		}
		dsn, err := c.principalDSN(m, credentialBootstrap)
		if err != nil {
			return err
		}
		st, err := store.Open(ctx, dsn)
		if err != nil {
			return errPrincipalProvision
		}
		defer st.Close()
		if err := validateInstallerCatalog(ctx, conn, st.VerifySyncPrincipals); err != nil {
			return err
		}
		var owned bool
		if conn.QueryRow(ctx, `SELECT d.datdba='witself_sync_owner'::regrole AND n.nspowner='witself_sync_owner'::regrole
 FROM pg_database d,pg_namespace n WHERE d.datname=current_database() AND n.nspname='public'`).Scan(&owned) != nil || !owned {
			return errPrincipalProvision
		}
		if err := validateInstallerPreMigrationACL(ctx, conn, st.AppliedSyncSchemaVersion, func(ctx context.Context) error {
			maintenance, err := c.openMaintenance(ctx, m)
			if err != nil {
				return err
			}
			defer maintenance.Close()
			return maintenance.VerifyManagedRuntimePrincipal(ctx, store.SyncPrincipalMaintenance)
		}); err != nil {
			return err
		}
		return c.verifyPrincipalCredentialPairs(ctx, m)
	})
}

// Before migration three, the five runtime roles have no application-object
// grants. Core and sync migrations may have committed separately, so absent
// relations and ordinary PUBLIC function defaults are valid intermediate state.
// Read effective table/column rights (including PUBLIC), but inspect explicit
// runtime function ACLs: pre-three trigger functions normally retain PostgreSQL's
// default PUBLIC EXECUTE. Never repair a preexisting grant by running migration
// three's REVOKEs. Once three is installed, use C2a's complete runtime topology
// instead; its approved domain and admission-function grants must be retained.
// PostgreSQL rejects zero-dimensional empty arrays in aclexplode. The explicit
// ACL expansions below map NULL/empty arrays to NULL (no rows), preserving every
// populated ACL entry, including grant options; malformed populated arrays fail.
const installerPreMigrationSchemaSQL = `SELECT NOT EXISTS (
 SELECT 1 FROM pg_roles r WHERE r.rolname=ANY($1::text[])
 AND has_schema_privilege(r.oid,current_schema(),'CREATE'))`

const installerPreMigrationOwnershipSQL = `SELECT NOT EXISTS (
 SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 JOIN pg_roles r ON r.oid=c.relowner
 WHERE n.nspname=current_schema() AND r.rolname=ANY($1::text[]))
 AND NOT EXISTS (
 SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 JOIN pg_roles r ON r.oid=p.proowner
 WHERE n.nspname=current_schema() AND r.rolname=ANY($1::text[]))`

const installerPreMigrationTableACLSQL = `SELECT NOT EXISTS (
 SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 CROSS JOIN pg_roles r WHERE n.nspname=current_schema()
 AND c.relkind IN ('r','p','v','m','f') AND r.rolname=ANY($1::text[])
 AND (has_table_privilege(r.oid,c.oid,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
 OR has_any_column_privilege(r.oid,c.oid,'SELECT,INSERT,UPDATE,REFERENCES')))
 AND NOT EXISTS (
 SELECT 1 FROM pg_default_acl d
 CROSS JOIN LATERAL aclexplode(CASE WHEN cardinality(d.defaclacl)>0 THEN d.defaclacl ELSE NULL::aclitem[] END) a
 LEFT JOIN pg_roles r ON r.oid=a.grantee
 WHERE (d.defaclnamespace=0 OR d.defaclnamespace=to_regnamespace(current_schema()))
 AND d.defaclobjtype IN ('r','S') AND (a.grantee=0 OR r.rolname=ANY($1::text[])))`

const installerPreMigrationFunctionACLSQL = `SELECT NOT EXISTS (
 SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 CROSS JOIN LATERAL aclexplode(CASE WHEN cardinality(p.proacl)>0 THEN p.proacl ELSE NULL::aclitem[] END) a
 JOIN pg_roles r ON r.oid=a.grantee
 WHERE n.nspname=current_schema() AND r.rolname=ANY($1::text[])
 AND (starts_with(p.proname,'sync_') OR starts_with(p.proname,'witself_sync_')
 OR p.proname='capture_lock_key'))
 AND NOT EXISTS (
 SELECT 1 FROM pg_default_acl d
 CROSS JOIN LATERAL aclexplode(CASE WHEN cardinality(d.defaclacl)>0 THEN d.defaclacl ELSE NULL::aclitem[] END) a
 JOIN pg_roles r ON r.oid=a.grantee
 WHERE (d.defaclnamespace=0 OR d.defaclnamespace=to_regnamespace(current_schema()))
 AND d.defaclobjtype='f' AND r.rolname=ANY($1::text[]))`

func validateInstallerPreMigrationACL(ctx context.Context, q installerCatalog,
	readVersion func(context.Context) (int, error), verifyCurrent func(context.Context) error,
) error {
	if ctx.Err() != nil {
		return errPrincipalProvision
	}
	version, err := readVersion(ctx)
	if err != nil || ctx.Err() != nil || version < 0 || version > store.SyncSchemaVersion() {
		return errPrincipalProvision
	}
	if version == store.SyncSchemaVersion() {
		if verifyCurrent(ctx) != nil || ctx.Err() != nil {
			return errPrincipalProvision
		}
		return nil
	}
	roles := store.SyncPrincipalRoleNames()[1:]
	for _, query := range []string{installerPreMigrationSchemaSQL, installerPreMigrationOwnershipSQL,
		installerPreMigrationTableACLSQL, installerPreMigrationFunctionACLSQL} {
		var valid bool
		if ctx.Err() != nil || q.QueryRow(ctx, query, roles).Scan(&valid) != nil || !valid {
			return errPrincipalProvision
		}
	}
	return nil
}

func installerPurposeForRole(role string) (credentialPurpose, error) {
	for _, purpose := range []credentialPurpose{credentialServer, credentialWorker, credentialMaintenance, credentialSnapshot, credentialLegacy, credentialMigrator} {
		name, err := credentialRole(purpose)
		if err == nil && name == role {
			return purpose, nil
		}
	}
	return 0, errPrincipalLayout
}

func (c *Cell) verifyPrincipalCredentialPairs(ctx context.Context, m *Manifest) error {
	return c.verifyCredentialPurposes(ctx, m, principalCredentialPurposes[:])
}

func (c *Cell) verifyCredentialPurposes(ctx context.Context, m *Manifest, purposes []credentialPurpose) error {
	for _, purpose := range purposes {
		dsn, err := c.principalDSN(m, purpose)
		if err != nil {
			return err
		}
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return ErrLocalPrincipalCredentialsRefused
		}
		var session, current string
		err = conn.QueryRow(ctx, "SELECT session_user,current_user").Scan(&session, &current)
		closeInstallerConnection(conn)
		role, roleErr := credentialRole(purpose)
		wantCurrent := role
		if purpose == credentialMigrator {
			wantCurrent = string(store.SyncPrincipalOwner)
		}
		if err != nil || roleErr != nil || session != role || current != wantCurrent {
			return ErrLocalPrincipalCredentialsRefused
		}
	}
	return nil
}

func (c *Cell) verifyRuntimePrincipalStart(ctx context.Context, m *Manifest) error {
	if err := c.validatePrincipalPasswords(m); err != nil {
		return err
	}
	if err := c.verifyCredentialPurposes(ctx, m, []credentialPurpose{credentialServer, credentialWorker, credentialMaintenance}); err != nil {
		return err
	}
	return c.verifyMaintenanceTopology(ctx, m)
}

func (c *Cell) runPrincipalMigration(ctx context.Context, m *Manifest) error {
	environment, err := c.childEnvironment(m, "migration")
	if err != nil {
		return err
	}
	cmd, err := c.command(ctx, c.cfg.Binaries.Server, "serve")
	if err != nil {
		return err
	}
	cmd.Env = environment
	// Migration libraries may log SQL. No subprocess output is retained.
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = time.Second
	if cmd.Run() != nil {
		return ErrLocalMigrationPhaseRequired
	}
	return nil
}

func (c *Cell) grantInstallerStartupAccess(ctx context.Context, m *Manifest) error {
	return c.installerAdmin(ctx, m, func(conn *pgx.Conn) error {
		// These are the only uncovered startup tables. The capacity singleton is
		// read and updated by server startup; it is never inserted or deleted.
		_, err := conn.Exec(ctx, `GRANT SELECT ON goose_db_version TO witself_sync_server,witself_sync_worker,witself_sync_maintenance;
GRANT SELECT ON agent_email_cell_storage_capacity TO witself_sync_server;
GRANT UPDATE (admission_bytes,admission_root_rows,hard_bytes,hard_counted_rows,updated_at) ON agent_email_cell_storage_capacity TO witself_sync_server`)
		return err
	})
}

func (c *Cell) verifyInstallerPrincipals(ctx context.Context, m *Manifest) error {
	if err := c.verifyPrincipalCredentialPairs(ctx, m); err != nil {
		return err
	}
	return c.verifyMaintenanceTopology(ctx, m)
}

func (c *Cell) verifyMaintenanceTopology(ctx context.Context, m *Manifest) error {
	st, err := c.openMaintenance(ctx, m)
	if err != nil {
		return err
	}
	if err := st.VerifyManagedRuntimePrincipal(ctx, store.SyncPrincipalMaintenance); err != nil {
		st.Close()
		return ErrLocalRuntimeLoginOwnsTables
	}
	core, err := st.AppliedSchemaVersion(ctx)
	if err != nil || core != store.SchemaVersion() {
		st.Close()
		return ErrLocalMigrationPhaseRequired
	}
	if m.AccountID != "" && runtimeconfig.CheckReady(ctx, st, store.SchemaVersion(), m.AccountID) != nil {
		st.Close()
		return ErrLocalMigrationPhaseRequired
	}
	version, err := st.AppliedSyncSchemaVersion(ctx)
	st.Close()
	if err != nil || version != store.SyncSchemaVersion() {
		return ErrLocalMigrationPhaseRequired
	}
	if err := c.verifyInstallerStartupCatalog(ctx, m); err != nil {
		return err
	}
	return c.observePrivilegedSessions(ctx, m)
}

func (c *Cell) verifyInstallerStartupCatalog(ctx context.Context, m *Manifest) error {
	dsn, err := c.maintenanceDSN(m)
	if err != nil {
		return err
	}
	conn, err := c.connectMaintenance(ctx, dsn)
	if err != nil {
		return errPrincipalProvision
	}
	defer closeInstallerConnection(conn)
	// The six-role C2a predicate has just succeeded through maintenance. Check
	// the independently bounded owner/migrator additions and uncovered ACLs.
	if err := validateInstallerCatalog(ctx, conn, func(context.Context) error { return nil }); err != nil {
		return err
	}
	return verifyInstallerStartupGrants(ctx, conn)
}

func verifyInstallerStartupGrants(ctx context.Context, q installerCatalog) error {
	var valid bool
	if q.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_roles r
 WHERE r.rolname=ANY($1::text[]) AND (
 has_table_privilege(r.oid,'goose_db_version','SELECT') IS DISTINCT FROM
 (r.rolname=ANY(ARRAY['witself_sync_server','witself_sync_worker','witself_sync_maintenance']))
 OR has_any_column_privilege(r.oid,'goose_db_version','SELECT') IS DISTINCT FROM
 (r.rolname=ANY(ARRAY['witself_sync_server','witself_sync_worker','witself_sync_maintenance']))
 OR has_table_privilege(r.oid,'goose_db_version','INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
 OR has_any_column_privilege(r.oid,'goose_db_version','INSERT,UPDATE,REFERENCES')
 OR has_table_privilege(r.oid,'agent_email_cell_storage_capacity','SELECT') IS DISTINCT FROM (r.rolname='witself_sync_server')
 OR has_any_column_privilege(r.oid,'agent_email_cell_storage_capacity','SELECT') IS DISTINCT FROM (r.rolname='witself_sync_server')
 OR has_table_privilege(r.oid,'agent_email_cell_storage_capacity','INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
 OR has_any_column_privilege(r.oid,'agent_email_cell_storage_capacity','INSERT,REFERENCES')
 OR EXISTS(SELECT 1 FROM pg_class c CROSS JOIN LATERAL aclexplode(CASE WHEN cardinality(c.relacl)>0 THEN c.relacl ELSE NULL::aclitem[] END) a
 WHERE c.oid IN ('goose_db_version'::regclass,'agent_email_cell_storage_capacity'::regclass)
 AND a.grantee IN (0,r.oid) AND a.is_grantable)
 OR EXISTS(SELECT 1 FROM pg_attribute a CROSS JOIN LATERAL aclexplode(CASE WHEN cardinality(a.attacl)>0 THEN a.attacl ELSE NULL::aclitem[] END) acl
 WHERE a.attrelid IN ('goose_db_version'::regclass,'agent_email_cell_storage_capacity'::regclass)
 AND acl.grantee IN (0,r.oid) AND acl.is_grantable)
 OR EXISTS(SELECT 1 FROM pg_attribute a WHERE a.attrelid='agent_email_cell_storage_capacity'::regclass
 AND a.attnum>0 AND NOT a.attisdropped AND
 has_column_privilege(r.oid,a.attrelid,a.attnum,'UPDATE') IS DISTINCT FROM
 (r.rolname='witself_sync_server' AND a.attname=ANY(ARRAY['admission_bytes','admission_root_rows','hard_bytes','hard_counted_rows','updated_at'])))))`, store.SyncPrincipalRoleNames()[1:]).Scan(&valid) != nil || !valid {
		return errPrincipalProvision
	}
	return nil
}

func (c *Cell) openMaintenance(ctx context.Context, m *Manifest) (*store.Store, error) {
	dsn, err := c.maintenanceDSN(m)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return nil, ErrLocalRuntimeLoginOwnsTables
	}
	return st, nil
}

// usesysid identifies the authenticated login even when current_user is owner
// through SET ROLE. The observer must close before the caller launches a child.
func (c *Cell) observePrivilegedSessions(ctx context.Context, m *Manifest) error {
	dsn, err := c.maintenanceDSN(m)
	if err != nil {
		return err
	}
	conn, err := c.connectMaintenance(ctx, dsn)
	if err != nil {
		return errPrincipalSessions
	}
	defer closeInstallerConnection(conn)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		if requirePrivilegedSessionDrain(ctx, conn, m.Role) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errPrincipalSessions
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func requirePrivilegedSessionDrain(ctx context.Context, q installerCatalog, bootstrap string) error {
	var drained bool
	if q.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM pg_stat_activity
 WHERE usesysid IN (SELECT oid FROM pg_roles WHERE rolname=ANY($1::text[])))`, []string{bootstrap, installerMigrator}).Scan(&drained) != nil || !drained {
		return errPrincipalSessions
	}
	return nil
}

func (c *Cell) prepareSelectedHealthCredential(ctx context.Context, m *Manifest) error {
	st, err := c.healthStore(ctx, m)
	if err != nil {
		return err
	}
	defer st.Close()
	if runtimeconfig.CheckReady(ctx, st, store.SchemaVersion(), m.AccountID) != nil {
		return errServiceHealth
	}
	if err := privateDir(c.path("service"), true); err != nil {
		return errServiceHealth
	}
	_, err = c.serviceAPICredential(ctx, st, m)
	return err
}

// maintenanceDSN preserves production credential selection. A package-local
// fixture can substitute its read-only wire endpoint without a native cluster.
func (c *Cell) maintenanceDSN(m *Manifest) (string, error) {
	if c.maintenanceTest != nil {
		return c.maintenanceTest.dsn(), nil
	}
	return c.principalDSN(m, credentialMaintenance)
}

// installerMaintenanceOperations exposes only read-only connection boundaries.
// It is nil on every production Cell.
type installerMaintenanceOperations struct {
	dsn     func() string
	connect func(context.Context, string) (*pgx.Conn, error)
}

func (c *Cell) connectMaintenance(ctx context.Context, dsn string) (*pgx.Conn, error) {
	if c.maintenanceTest != nil {
		return c.maintenanceTest.connect(ctx, dsn)
	}
	return pgx.Connect(ctx, dsn)
}
