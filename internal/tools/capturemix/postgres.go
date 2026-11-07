package capturemix

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/testenv"
)

type cluster struct {
	admin      *pgx.Conn
	dsn        string
	disposable bool
	passwords  map[string]string
	created    []string
}

func openCluster(ctx context.Context, options Options) (*cluster, error) {
	dsn := options.OwnerDSN
	if options.DisposableCluster {
		dsn = options.SuperuserDSN
	}
	if testenv.ValidateHarnessPostgresDSN(dsn) != nil {
		return nil, errors.New("capturemix database target refused")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || !allowedClusterTarget(parsed, options.DisposableCluster) {
		return nil, errors.New("capturemix database target refused")
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, errors.New("capturemix database unavailable")
	}
	c := &cluster{admin: admin, dsn: dsn, disposable: options.DisposableCluster, passwords: map[string]string{}}
	if !options.DisposableCluster {
		return c, nil
	}
	var allowed bool
	if admin.QueryRow(ctx, `SELECT current_database()='witself' AND current_setting('is_superuser')='on'
	 AND NOT EXISTS(SELECT 1 FROM pg_database WHERE datname LIKE 'codex\_%' ESCAPE '\')
	 AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=ANY($1))`, store.SyncPrincipalRoleNames()).Scan(&allowed) != nil || !allowed {
		_ = admin.Close(ctx)
		return nil, errors.New("capturemix disposable cluster unidentified")
	}
	for _, role := range store.SyncPrincipalRoleNames() {
		password, err := randomToken()
		if err != nil {
			if c.close() != nil {
				return nil, errors.New("capturemix role allocation cleanup failed")
			}
			return nil, err
		}
		query := `CREATE ROLE ` + pgx.Identifier{role}.Sanitize() + ` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD '` + password + `'`
		if _, err := admin.Exec(ctx, query); err != nil {
			if c.close() != nil {
				return nil, errors.New("capturemix role allocation cleanup failed")
			}
			return nil, errors.New("capturemix role allocation failed")
		}
		c.created = append(c.created, role)
		c.passwords[role] = password
	}
	return c, nil
}

func allowedClusterTarget(target *url.URL, disposable bool) bool {
	if disposable {
		return target.Path == "/witself"
	}
	// The shared-instance rehearsal is limited to the closed local test lanes.
	// ValidateHarnessPostgresDSN has already rejected query target overrides.
	if target.Hostname() != "127.0.0.1" {
		return false
	}
	switch target.Port() {
	case "5599", "5600", "5601":
	default:
		return false
	}
	switch target.Path {
	case "/codex_a", "/codex_b", "/codex_c", "/codex_d":
		return true
	default:
		return false
	}
}

func (c *cluster) close() error {
	var failure error
	for i := len(c.created) - 1; i >= 0; i-- {
		if err := c.cleanupExec(`DROP ROLE `+pgx.Identifier{c.created[i]}.Sanitize(), "capturemix role cleanup failed"); err != nil {
			failure = errors.Join(failure, err)
		}
	}
	ctx, cancel := newCleanupContext()
	defer cancel()
	if c.admin != nil && c.admin.Close(ctx) != nil {
		failure = errors.Join(failure, errors.New("capturemix admin cleanup failed"))
	}
	return failure
}

// Keep cleanup proportional to the configured fixture intervals, preserving
// the previous half-minute allowance at the current one-minute base interval.
func newCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), proofWorkerInterval("MESSAGE_RATE_BUCKET_CLEANUP")/2)
}

// A failed observation or expired rollback/installer budget must not strand
// catalog objects. Each drop owns a new connection and budget; no operation
// depends on the fixture connection or the cluster's long-lived admin handle.
func (c *cluster) cleanupExec(statement, failureClass string) error {
	ctx, cancel := newCleanupContext()
	defer cancel()
	conn, err := pgx.Connect(ctx, c.dsn)
	if err != nil {
		return errors.Join(errors.New(failureClass), errors.New("capturemix cleanup connection failed"))
	}
	var failure error
	if _, err := conn.Exec(ctx, statement); err != nil {
		failure = errors.New(failureClass)
	}
	if conn.Close(ctx) != nil {
		failure = errors.Join(failure, errors.New("capturemix cleanup connection close failed"))
	}
	return failure
}

type fixture struct {
	cluster                            *cluster
	options                            Options
	serverVersion, workerVersion       string
	name, dsn                          string
	db                                 *pgx.Conn
	st                                 *store.Store
	provider                           *goose.Provider
	migrationDB                        *sql.DB
	marker, activeToken, inactiveToken string
	processes                          []*childProcess
	server, worker                     *childProcess
	oldServer                          *childProcess
	held, prepared, late               *pgx.Conn
	observer                           *pgx.Conn
	drainRelation                      uint32
	heldTx, lateTx                     pgx.Tx
	install                            chan error
	installCancel                      context.CancelFunc
	installConsumed                    bool
	allocated                          bool
	started                            time.Time
	stub                               *stubServer
	jobEvidence                        []JobEvidence
}

func (c *cluster) fixture(ctx context.Context, options Options, server, worker string) (_ *fixture, resultErr error) {
	suffix, err := randomName()
	if err != nil {
		return nil, err
	}
	f := &fixture{cluster: c, options: options, serverVersion: server, workerVersion: worker, name: "capturemix_" + suffix, started: time.Now()}
	defer func() {
		if resultErr != nil {
			if err := f.close(); err != nil {
				resultErr = errors.Join(resultErr, errors.New("capturemix allocation cleanup failed"), err)
			}
		}
	}()
	u, _ := url.Parse(c.dsn)
	if c.disposable {
		f.name = "witself_captureproof_" + suffix
		if _, err := c.admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{f.name}.Sanitize()); err != nil {
			return nil, errors.New("capturemix database allocation failed")
		}
		u.Path = "/" + f.name
	} else {
		if _, err := c.admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{f.name}.Sanitize()); err != nil {
			return nil, errors.New("capturemix schema allocation failed")
		}
		isolateSchemaURL(u, f.name)
	}
	f.allocated = true
	f.dsn = u.String()
	f.db, err = pgx.Connect(ctx, f.dsn)
	if err != nil {
		return nil, errors.New("capturemix fixture connection failed")
	}
	expectedSchema := "public"
	if !c.disposable {
		expectedSchema = f.name
	}
	var actualSchema string
	var empty bool
	if f.db.QueryRow(ctx, `SELECT current_schema(),to_regclass('goose_db_version') IS NULL AND to_regclass('accounts') IS NULL`).Scan(&actualSchema, &empty) != nil || actualSchema != expectedSchema || !empty {
		return nil, errors.New("capturemix initial schema isolation failed")
	}
	f.st, err = store.Open(ctx, f.dsn)
	if err != nil {
		return nil, errors.New("capturemix fixture store failed")
	}
	if f.st.Migrate() != nil {
		return nil, errors.New("capturemix core migration failed")
	}
	var coreLocal bool
	if f.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relname='accounts' AND n.nspname=$1)`, expectedSchema).Scan(&coreLocal) != nil || !coreLocal {
		return nil, errors.New("capturemix migrated schema isolation failed")
	}
	if version, err := f.st.AppliedSchemaVersion(ctx); err != nil || version != store.SchemaVersion() {
		return nil, errors.New("capturemix core coordinate mismatch")
	}

	f.migrationDB, err = sql.Open("pgx", f.dsn)
	if err != nil {
		return nil, errors.New("capturemix migration connection failed")
	}
	source, err := fs.Sub(store.SyncMigrationFiles(), "sync_migrations")
	if err != nil {
		return nil, errors.New("capturemix migration source failed")
	}
	f.provider, err = goose.NewProvider(goose.DialectPostgres, f.migrationDB, source, goose.WithTableName("witself_sync_schema_version"), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return nil, errors.New("capturemix migration provider failed")
	}
	// The current provider is explicitly pinned to 0001. This avoids executing
	// startup code as a migration owner and is checked before either binary starts.
	if _, err := f.provider.UpTo(ctx, 1); err != nil {
		return nil, errors.New("capturemix version one migration failed")
	}
	if v, err := f.st.AppliedSyncSchemaVersion(ctx); err != nil || v != 1 {
		return nil, errors.New("capturemix initial schema mismatch")
	}
	if f.st.SelectSyncCapture(runtimeconfig.Config{Local: true, SyncSchema: true}) != nil {
		return nil, errors.New("capturemix sync selection failed")
	}
	if err := f.seed(ctx); err != nil {
		return nil, err
	}
	if c.disposable {
		// Production roles need existing core objects before migration 0003
		// narrows covered/control ACLs. No role owns covered objects or may alter them.
		for _, role := range store.SyncPrincipalRoleNames()[1:] {
			if _, err := f.db.Exec(ctx, `GRANT USAGE ON SCHEMA public TO `+pgx.Identifier{role}.Sanitize()+`; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO `+pgx.Identifier{role}.Sanitize()+`; GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA public TO `+pgx.Identifier{role}.Sanitize()); err != nil {
				return nil, errors.New("capturemix startup grant failed")
			}
		}
		f.marker, err = randomToken()
		if err != nil {
			return nil, err
		}
		if f.st.InstallSyncCaptureProofMarker(ctx, f.marker, time.Now().Add(30*time.Minute)) != nil {
			return nil, errors.New("capturemix marker installation failed")
		}

	}
	return f, nil
}

func (f *fixture) seed(ctx context.Context) error {
	for index, q := range []string{
		`INSERT INTO accounts(id,display_name,is_default,status) VALUES('mix_active','Capture fixture',true,'active'),('mix_inactive','Inactive fixture',false,'active')`,
		`INSERT INTO realms(id,account_id,name) VALUES('mix_active_realm','mix_active','default'),('mix_inactive_realm','mix_inactive','default')`,
		`INSERT INTO agents(id,realm_id,name) VALUES('mix_active_agent','mix_active_realm','agent'),('mix_inactive_agent','mix_inactive_realm','agent')`,
		`INSERT INTO operators(id,account_id,role,is_root,display_name) VALUES('mix_owner','mix_active','account_owner',true,'owner')`,
		`INSERT INTO sync_incarnations(account_id,lifecycle_epoch,restore_marker,allocation_id,transition_id,authority_installation,state) VALUES('mix_active',1,decode(repeat('01',16),'hex'),decode(repeat('02',16),'hex'),decode(repeat('03',16),'hex'),'local','installed')`,
		`INSERT INTO agent_dashboard_preferences(account_id,realm_id,agent_id,prefs) VALUES('mix_active','mix_active_realm','mix_active_agent','{"schema":"witself.dashboard-prefs.v1","theme":"amber"}'),('mix_inactive','mix_inactive_realm','mix_inactive_agent','{"schema":"witself.dashboard-prefs.v1","theme":"amber"}')`,
	} {
		if _, err := f.db.Exec(ctx, q); err != nil {
			return errors.New("capturemix fixture seed failed: " + []string{"accounts", "realms", "agents", "operator", "incarnation", "preferences"}[index] + " sqlstate=" + safeSQLState(err))
		}
	}
	for _, account := range []string{"mix_active", "mix_inactive"} {
		token, err := randomToken()
		if err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(token))
		if _, err := f.db.Exec(ctx, `INSERT INTO tokens(id,account_id,agent_id,kind,token_hash) VALUES($1,$2,$3,'agent',$4)`, account+"_token", account, account+"_agent", hex.EncodeToString(digest[:])); err != nil {
			return errors.New("capturemix fixture credential failed")
		}
		if account == "mix_active" {
			f.activeToken = token
		} else {
			f.inactiveToken = token
		}
	}
	return nil
}

func (f *fixture) roleDSN(role string) string {
	if !f.cluster.disposable {
		return f.dsn
	}
	u, _ := url.Parse(f.dsn)
	u.User = url.UserPassword(role, f.cluster.passwords[role])
	return u.String()
}

func (f *fixture) holdLegacy(ctx context.Context) error {
	if f.cluster.disposable || f.workerVersion == "old" {
		if err := f.seedWorkerCandidates(ctx); err != nil {
			return err
		}
	}
	var err error
	f.observer, err = pgx.Connect(ctx, f.dsn)
	if err != nil {
		return errors.New("capturemix installation observer connection failed")
	}
	// Resolve before the conflicting writer and installer queue their locks.
	// A regclass lookup in the observation itself can wait behind the DDL.
	if f.observer.QueryRow(ctx, `SELECT 'accounts'::regclass::oid`).Scan(&f.drainRelation) != nil {
		return errors.New("capturemix installation relation lookup failed")
	}
	f.held, err = pgx.Connect(ctx, f.roleDSN(string(store.SyncPrincipalLegacy)))
	if err != nil {
		return errors.New("capturemix held connection failed")
	}
	f.heldTx, err = f.held.Begin(ctx)
	if err != nil {
		return errors.New("capturemix held transaction failed")
	}
	if _, err = f.heldTx.Exec(ctx, `UPDATE accounts SET display_name='Before installation' WHERE id='mix_active'`); err != nil {
		return errors.New("capturemix held write failed")
	}
	f.prepared, err = pgx.Connect(ctx, f.roleDSN(string(store.SyncPrincipalLegacy)))
	if err != nil {
		return errors.New("capturemix prepared connection failed")
	}
	if _, err = f.prepared.Prepare(ctx, "mix_legacy_write", `UPDATE agent_dashboard_preferences SET prefs='{"schema":"witself.dashboard-prefs.v1","theme":"late"}' WHERE agent_id='mix_active_agent'`); err != nil {
		return errors.New("capturemix prepared statement failed")
	}
	f.late, err = pgx.Connect(ctx, f.roleDSN(string(store.SyncPrincipalLegacy)))
	if err != nil {
		return errors.New("capturemix late connection failed")
	}
	f.lateTx, err = f.late.Begin(ctx)
	if err != nil {
		return errors.New("capturemix late transaction failed")
	}
	if _, err = f.lateTx.Exec(ctx, `SELECT 1`); err != nil {
		return errors.New("capturemix late begin failed")
	}
	return nil
}

func (f *fixture) beginInstall(ctx context.Context) error {
	installCtx, cancel := context.WithCancel(ctx)
	f.installCancel = cancel
	f.install = make(chan error, 1)
	go func() { f.install <- f.st.InstallSyncGuards(installCtx) }()
	return nil
}
func (f *fixture) observeInstallWait(ctx context.Context) error {
	var observedFailure error
	err := poll(ctx, time.Since(f.started), func(ctx context.Context) (bool, error) {
		if err := f.installerBeforeWait(); err != nil {
			observedFailure = err
			return false, err
		}
		var observed bool
		err := f.observer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks w JOIN pg_locks h ON h.relation=w.relation AND h.database=w.database WHERE h.pid=$1 AND h.granted AND h.mode='RowExclusiveLock' AND NOT w.granted AND w.mode='AccessExclusiveLock' AND w.pid<>h.pid AND w.relation=$2::oid)`, f.held.PgConn().PID(), f.drainRelation).Scan(&observed)
		if err != nil {
			observedFailure = errors.New("capturemix installation observation query failed sqlstate=" + safeSQLState(err))
		}
		return observed, err
	})
	if observedFailure != nil {
		return observedFailure
	}
	return err
}

func (f *fixture) installerBeforeWait() error {
	select {
	case err := <-f.install:
		f.installConsumed = true
		if err == nil {
			return errors.New("capturemix installation completed before observed drain")
		}
		return errors.New("capturemix installation failed before observed drain: cause=" + installationFailureCause(err))
	default:
		return nil
	}
}

func installationFailureCause(err error) string {
	switch {
	case errors.Is(err, store.ErrSyncPrincipalsMissing):
		return "principals_missing"
	case errors.Is(err, store.ErrSyncSchemaAhead):
		return "schema_ahead"
	case errors.Is(err, store.ErrSyncSchemaStateInvalid):
		return "schema_invalid"
	case errors.Is(err, store.ErrMigrationWaitCanceled), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	}
	if err != nil && err.Error() == "apply sync migration failed" {
		return "migration_failed"
	}
	return "unclassified"
}
func (f *fixture) completeLegacy(ctx context.Context, completion string) error {
	var err error
	if completion == "commit" {
		err = f.heldTx.Commit(ctx)
	} else {
		err = f.heldTx.Rollback(ctx)
	}
	if err != nil {
		return errors.New("capturemix legacy completion failed")
	}
	f.heldTx = nil
	return nil
}
func (f *fixture) finishInstall(ctx context.Context) error {
	select {
	case err := <-f.install:
		f.installConsumed = true
		if err != nil {
			return errors.New("capturemix guard installation failed: cause=" + installationFailureCause(err))
		}
	case <-ctx.Done():
		return errors.New("capturemix guard installation timed out")
	}
	// The shared database cannot carry the positive profile marker. Current
	// ordinary-local processes necessarily start after their migrations here.
	if !f.cluster.disposable {
		if f.serverVersion == "new" {
			if err := f.startServer(ctx); err != nil {
				return err
			}
		}
		if f.workerVersion == "new" {
			if err := f.startWorker(ctx); err != nil {
				return err
			}
			if err := f.seedWorkerCandidates(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}
func (f *fixture) activate(ctx context.Context) error {
	if err := f.assertWorkerCandidateRows(ctx); err != nil {
		return err
	}
	if err := f.options.Activate(ctx, f.st, "mix_active"); err != nil {
		return err
	}
	if err := f.scheduleWorkerCandidateLanes(ctx); err != nil {
		return err
	}
	if err := f.schedulePurgeCandidate(ctx); err != nil {
		return err
	}
	if f.worker.paused {
		if resumeChild(f.worker.cmd.Process) != nil {
			return errors.New("capturemix worker resume failed")
		}
		f.worker.paused = false
	}
	return nil
}

func (f *fixture) assertWriters(ctx context.Context) error {
	if _, err := f.prepared.Exec(ctx, "mix_legacy_write"); !guardRefusal(err) {
		return errors.New("capturemix prepared writer refusal missing")
	}
	if _, err := f.lateTx.Exec(ctx, `UPDATE agent_dashboard_preferences SET prefs='{"schema":"witself.dashboard-prefs.v1","theme":"late"}' WHERE agent_id='mix_active_agent'`); !guardRefusal(err) {
		return errors.New("capturemix late writer refusal missing")
	}
	if err := f.lateTx.Rollback(ctx); err != nil {
		return errors.New("capturemix late transaction cleanup failed")
	}
	f.lateTx = nil
	if err := f.assertServer(ctx); err != nil {
		return err
	}
	return f.assertWorker(ctx)
}
func (f *fixture) assertDowngrade(ctx context.Context) error {
	if f.oldServer == nil || !f.oldServer.alive() {
		return errors.New("capturemix old server downgrade witness missing")
	}
	_, err := f.provider.DownTo(ctx, 1)
	if !downgradeRefusal(err) {
		return errors.New("capturemix principal downgrade refusal missing")
	}
	raw, err := fs.ReadFile(store.SyncMigrationFiles(), "sync_migrations/0002_sync_capture_guards.sql")
	if err != nil {
		return errors.New("capturemix guard downgrade source missing")
	}
	_, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		return errors.New("capturemix guard downgrade direction missing")
	}
	tx, err := f.db.Begin(ctx)
	if err != nil {
		return errors.New("capturemix guard downgrade transaction failed")
	}
	_, refused := tx.Exec(ctx, down)
	if tx.Rollback(ctx) != nil {
		return errors.New("capturemix guard downgrade rollback failed")
	}
	if !downgradeRefusal(refused) {
		return errors.New("capturemix guard downgrade refusal missing")
	}
	if v, e := f.st.AppliedSyncSchemaVersion(ctx); e != nil || v != 3 {
		return errors.New("capturemix downgrade changed schema")
	}
	if !f.oldServer.alive() {
		return errors.New("capturemix old server exited during downgrade")
	}
	return nil
}
func downgradeRefusal(err error) bool {
	var p *pgconn.PgError
	return errors.As(err, &p) && p.Code == "23514" && p.Message == "sync schema downgrade is forbidden"
}

func guardRefusal(err error) bool {
	var p *pgconn.PgError
	return errors.As(err, &p) && p.Code == "P0001" && p.Message == "sync writer not admitted"
}

func (f *fixture) close() error {
	var failure error
	if err := f.stopProcesses(); err != nil {
		failure = errors.Join(errors.New("capturemix process cleanup failed"), err)
	}
	// Child shutdown has its own bounded graceful/kill/join allowances. Give
	// transaction and database cleanup a fresh budget after all those attempts.
	ctx, cancel := newCleanupContext()
	defer cancel()
	for _, tx := range []pgx.Tx{f.heldTx, f.lateTx} {
		if tx != nil {
			if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				failure = errors.Join(failure, errors.New("capturemix transaction cleanup failed"))
			}
		}
	}
	if f.installCancel != nil {
		f.installCancel()
	}
	if f.install != nil && !f.installConsumed {
		select {
		case <-f.install:
			f.installConsumed = true
		case <-ctx.Done():
			failure = errors.Join(failure, errors.New("capturemix installer cleanup timed out"))
		}
	}
	for _, conn := range []*pgx.Conn{f.held, f.prepared, f.late, f.observer} {
		if conn != nil && conn.Close(ctx) != nil {
			failure = errors.Join(failure, errors.New("capturemix connection cleanup failed"))
		}
	}
	if f.st != nil {
		f.st.Close()
	}
	if f.migrationDB != nil && f.migrationDB.Close() != nil {
		failure = errors.Join(failure, errors.New("capturemix migration cleanup failed"))
	}
	if f.db != nil && f.db.Close(ctx) != nil {
		failure = errors.Join(failure, errors.New("capturemix database connection cleanup failed"))
	}
	if f.stub != nil {
		if f.options.ArtifactDir != "" {
			f.stub.mu.Lock()
			requests := append([]string(nil), f.stub.requests...)
			f.stub.mu.Unlock()
			if writePrivateJSON(filepath.Join(f.options.ArtifactDir, "stub-requests-"+f.name+".json"), requests) != nil {
				failure = errors.Join(failure, errors.New("capturemix stub artifact failed"))
			}
		}
		f.stub.close()
	}
	if !f.allocated {
		return failure
	}
	if f.cluster.disposable {
		if err := f.cluster.cleanupExec(`DROP DATABASE `+pgx.Identifier{f.name}.Sanitize(), "capturemix database cleanup failed"); err != nil {
			failure = errors.Join(failure, err)
		}
	} else {
		if err := f.cluster.cleanupExec(`DROP SCHEMA `+pgx.Identifier{f.name}.Sanitize()+` CASCADE`, "capturemix schema cleanup failed"); err != nil {
			failure = errors.Join(failure, err)
		}
	}
	return failure
}

func safeSQLState(err error) string {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) {
		switch pgerr.Code {
		case "23505", "23503", "23514", "42P01", "42703", "42501", "23502":
			return pgerr.Code
		}
	}
	return "other"
}

// Public fallback would let Goose see a different schema's applied version.
// An isolated rehearsal has exactly one search-path element, including when
// the caller's URL already carried a search_path query parameter.
func isolateSchemaURL(target *url.URL, schema string) {
	q := target.Query()
	q.Set("search_path", schema)
	target.RawQuery = q.Encode()
}

func randomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("capturemix random input failed")
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
func randomName() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("capturemix random identity failed")
	}
	for i := range value {
		value[i] = 'a' + value[i]%26
	}
	return string(value), nil
}
func poll(ctx context.Context, work time.Duration, predicate func(context.Context) (bool, error)) error {
	budget := max(10*time.Second, work*4)
	budget = min(budget, 2*time.Minute)
	return pollWithBudget(ctx, budget, predicate)
}

func pollWithBudget(ctx context.Context, budget time.Duration, predicate func(context.Context) (bool, error)) error {
	wait, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := predicate(wait)
		if err != nil {
			return errors.New("capturemix observation failed")
		}
		if ok {
			return nil
		}
		select {
		case <-wait.Done():
			return errors.New("capturemix observation timed out")
		case <-ticker.C:
		}
	}
}

// Activate performs the production activation and its same-connection snapshot
// handoff. The store test wraps this only to select its private local test knob.
func Activate(ctx context.Context, st *store.Store, account string) (resultErr error) {
	if st.EnableSyncWriter(ctx, account, store.SyncPreferencesPrincipal, store.SyncPreferencesCapability) != nil || st.EnableSyncWriter(ctx, account, string(store.SyncPrincipalServer), store.SyncPreferencesCapability) != nil {
		return errors.New("capturemix capability enable failed")
	}
	a, err := st.ActivateSyncCapture(ctx, account, store.SyncCaptureOptions{LifecycleEpoch: 1, RestoreMarker: bytes.Repeat([]byte{1}, 16), Capability: store.SyncPreferencesCapability})
	if err != nil {
		return errors.New("capturemix activation failed")
	}
	defer a.Close()
	pin, err := a.PinSnapshot(ctx)
	if err != nil {
		return errors.New("capturemix snapshot pin failed")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if pin.Close(cleanup) != nil {
			resultErr = errors.New("capturemix snapshot cleanup failed")
		}
	}()
	if a.Acknowledge(ctx, pin) != nil || !pin.Acknowledged() {
		return errors.New("capturemix snapshot acknowledgment failed")
	}
	return nil
}
