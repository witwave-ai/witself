package main

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/witwave-ai/witself/internal/localcell"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestManagedPrincipalsCommandSelection(t *testing.T) {
	for _, selector := range []string{"WITSELF_LOCAL_MANAGED_PRINCIPALS", "WITSELF_LOCAL_MIGRATION_PHASE"} {
		lookup := func(name string) (string, bool) { return "1", name == selector }
		if managedServerCommandAllowed([]string{"agent-email", "backfill"}, lookup) {
			t.Fatal("managed server selector allowed operational command")
		}
	}
}

type managedServerFixture struct {
	core, sync int
	identity   error
	topology   error
	role       store.SyncPrincipalRole
	events     []string
	fail       string
	cancel     context.CancelFunc
}

func (s *managedServerFixture) AppliedSchemaVersion(context.Context) (int, error) {
	s.events = append(s.events, "core_version")
	return s.core, nil
}
func (s *managedServerFixture) AppliedSyncSchemaVersion(context.Context) (int, error) {
	s.events = append(s.events, "sync_version")
	return s.sync, nil
}
func (s *managedServerFixture) VerifyManagedRuntimePrincipal(_ context.Context, role store.SyncPrincipalRole) error {
	s.role = role
	return s.topology
}
func (s *managedServerFixture) VerifyManagedRuntimeIdentity(_ context.Context, role store.SyncPrincipalRole) error {
	s.role = role
	return s.identity
}
func (s *managedServerFixture) VerifyManagedMigrationPrincipal(context.Context) error {
	s.events = append(s.events, "identity")
	if s.fail == "identity" || s.fail == "post_identity" && slices.Contains(s.events, "sync_version") {
		return errors.New("managed fixture refused identity")
	}
	return nil
}
func (s *managedServerFixture) Migrate() error {
	s.events = append(s.events, "unbounded")
	return errors.New("managed fixture forbids unbounded migration")
}
func (s *managedServerFixture) MigrateContext(ctx context.Context) error {
	s.events = append(s.events, "core")
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("managed fixture requires migration deadline")
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.fail == "core" {
		return errors.New("managed fixture refused core")
	}
	return nil
}
func (s *managedServerFixture) MigrateSync(context.Context) error {
	s.events = append(s.events, "sync")
	if s.fail == "sync" {
		return errors.New("managed fixture refused sync")
	}
	return nil
}
func (s *managedServerFixture) Close() { s.events = append(s.events, "close") }

func TestManagedPrincipalsStartup(t *testing.T) {
	for _, wrong := range []string{"", "core_older", "core_newer", "sync_older", "sync_newer", "identity", "identity_before_versions", "topology"} {
		t.Run("server_"+wrong, func(t *testing.T) {
			st := &managedServerFixture{core: store.SchemaVersion(), sync: store.SyncSchemaVersion()}
			switch wrong {
			case "core_older":
				st.core--
			case "core_newer":
				st.core++
			case "sync_older":
				st.sync--
			case "sync_newer":
				st.sync++
			case "identity":
				st.identity = store.ErrSyncPrincipalTopologyUnverified
			case "identity_before_versions":
				st.identity, st.core, st.sync = store.ErrSyncPrincipalTopologyUnverified, 0, 0
			case "topology":
				st.topology = store.ErrSyncPrincipalTopologyUnverified
			}
			migrations, listeners := 0, 0
			cfg := runtimeconfig.Config{Local: true, SyncSchema: true, ManagedPrincipals: true, MigrationWait: time.Second}
			err := serverMigrationStartup(context.Background(), cfg, st, func() startupMigrator { migrations++; return st })
			if err == nil {
				listeners++
			}
			if migrations != 0 {
				t.Fatal("managed server invoked ordinary migrator")
			}
			if (listeners == 1) != (wrong == "") {
				t.Fatal("managed server reached startup with refused preflight")
			}
			identityRefusal := wrong == "identity" || wrong == "identity_before_versions" || wrong == "topology"
			if identityRefusal && err != localcell.ErrLocalRuntimeLoginOwnsTables {
				t.Fatal("managed server lost fixed login refusal")
			}
			if wrong == "identity_before_versions" && len(st.events) != 0 {
				t.Fatal("managed server inspected versions before refusing identity")
			}
			if wrong != "" && !identityRefusal && err != localcell.ErrLocalMigrationPhaseRequired {
				t.Fatal("managed server lost fixed migration refusal")
			}
			if wrong == "" && st.role != store.SyncPrincipalServer {
				t.Fatal("managed server checked incorrect runtime identity")
			}
		})
	}
}

func TestManagedPrincipalsMigrationPhase(t *testing.T) {
	for _, failure := range []string{"", "open", "identity", "core", "sync", "cancel", "core_version", "sync_version"} {
		t.Run("phase_"+failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			st := &managedServerFixture{core: store.SchemaVersion(), sync: store.SyncSchemaVersion(), fail: failure}
			if failure == "cancel" {
				st.cancel = cancel
			}
			if failure == "core_version" {
				st.core--
			}
			if failure == "sync_version" {
				st.sync--
			}
			cfg := runtimeconfig.Config{Local: true, SyncSchema: true, ManagedPrincipals: true, MigrationPhase: true, MigrationWait: time.Second,
				DatabaseURL: "postgres://witself_sync_migrator:fixture-migration-secret@127.0.0.1:1234/witself?options=-c+role%3Dwitself_sync_owner"}
			err := runManagedMigrationPhase(ctx, cfg, func(context.Context, string) (managedMigrationStore, error) {
				if failure == "open" {
					return nil, errors.New("managed fixture refuses connection")
				}
				return st, nil
			})
			if (err == nil) != (failure == "") {
				t.Fatal("managed migration accepted failed edge")
			}
			if failure != "open" && (len(st.events) == 0 || st.events[len(st.events)-1] != "close") {
				t.Fatal("managed migration failed to close privileged pool")
			}
			if failure == "" && !reflect.DeepEqual(st.events, []string{"identity", "core", "sync", "core_version", "sync_version", "identity", "close"}) {
				t.Fatal("managed migration changed custody and schema ordering")
			}
			if failure == "cancel" && !reflect.DeepEqual(st.events, []string{"identity", "core", "close"}) {
				t.Fatal("managed migration continued after cancellation")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		fail   string
		cause  error
		events []string
	}{
		{"entry_exits_before_runtime", "", nil, []string{"identity", "core", "sync", "core_version", "sync_version", "identity", "close"}},
		{"entry_failure_open", "open", localcell.ErrLocalMigrationPhaseRequired, nil},
		{"entry_failure_identity", "identity", localcell.ErrLocalRuntimeLoginOwnsTables, []string{"identity", "close"}},
		{"entry_failure_core", "core", localcell.ErrLocalMigrationPhaseRequired, []string{"identity", "core", "close"}},
		{"entry_failure_sync", "sync", localcell.ErrLocalMigrationPhaseRequired, []string{"identity", "core", "sync", "close"}},
		{"entry_failure_cancel", "cancel", localcell.ErrLocalMigrationPhaseRequired, []string{"identity", "core", "close"}},
		{"entry_failure_core_version", "core_version", localcell.ErrLocalMigrationPhaseRequired, []string{"identity", "core", "sync", "core_version", "close"}},
		{"entry_failure_sync_version", "sync_version", localcell.ErrLocalMigrationPhaseRequired, []string{"identity", "core", "sync", "core_version", "sync_version", "close"}},
		{"entry_failure_post_identity", "post_identity", localcell.ErrLocalRuntimeLoginOwnsTables, []string{"identity", "core", "sync", "core_version", "sync_version", "identity", "close"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearServerRuntimeEnvironment(t)
			t.Setenv("WITSELF_HOME", t.TempDir())
			t.Setenv("DSH_HOME", t.TempDir())
			t.Setenv("WITSELF_RUNTIME", "local")
			t.Setenv("WITSELF_LOCAL_SYNC_SCHEMA", "1")
			t.Setenv("WITSELF_LOCAL_MANAGED_PRINCIPALS", "1")
			t.Setenv("WITSELF_LOCAL_MIGRATION_PHASE", "1")
			t.Setenv("WITSELF_DATABASE_URL", "postgres://witself_sync_migrator:fixture-migration-secret@127.0.0.1:1234/witself?options=-c+role%3Dwitself_sync_owner")
			spool, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal("managed migration spool canonicalization failed")
			}
			if os.Chmod(spool, 0o700) != nil {
				t.Fatal("managed migration private spool setup failed")
			}
			t.Setenv("WITSELF_LOCAL_SPOOL_DIR", spool)
			st := &managedServerFixture{core: store.SchemaVersion(), sync: store.SyncSchemaVersion(), fail: tc.fail}
			if tc.fail == "core_version" {
				st.core--
			}
			if tc.fail == "sync_version" {
				st.sync--
			}
			opens := 0
			listeners, migrators, runtimeSetup, shutdowns, bootstrapReads := 0, 0, 0, 0, 0
			status, output := captureMigrationStartup(t, serverStartup{
				migrationStore: func(context.Context, string) (managedMigrationStore, error) {
					opens++
					if tc.fail == "open" {
						return nil, errors.New("managed fixture refuses connection")
					}
					return st, nil
				},
				runtimeSetup:   func() { runtimeSetup++ },
				beforeShutdown: func() { shutdowns++ },
				bootstrapToken: func() (string, error) { bootstrapReads++; return "", nil },
				migrator:       func(*store.Store) startupMigrator { migrators++; return st },
				notifyContext: func(ctx context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
					ctx, cancel := context.WithCancel(ctx)
					if tc.fail == "cancel" {
						st.cancel = cancel
					}
					return ctx, cancel
				},
				run: func(context.Context, server.Config) error { listeners++; return nil },
			})
			// Prove the selected phase and close ran before checking its exit status:
			// an unrelated configuration refusal must not satisfy a failure case.
			if opens != 1 || !reflect.DeepEqual(st.events, tc.events) {
				t.Fatal("managed migration entry changed phase or pool closure ordering")
			}
			wantOutput := ""
			if tc.cause != nil {
				wantOutput = "witself-server: " + tc.cause.Error() + "\n"
			}
			if output != wantOutput {
				t.Fatal("managed migration entry lost exact fixed diagnostic")
			}
			if runtimeSetup != 0 {
				t.Fatal("managed migration reached normal runtime setup")
			}
			if shutdowns != 0 || bootstrapReads != 0 {
				t.Fatal("managed migration touched shutdown or bootstrap wiring")
			}
			if listeners != 0 || migrators != 0 {
				t.Fatal("managed migration entry fell through into runtime startup")
			}
			if tc.cause != nil && status != 1 {
				t.Fatal("managed migration failure did not return failure status")
			}
			if tc.cause == nil && status != 0 {
				t.Fatal("managed migration success did not return success status")
			}
		})
	}
}

// This fixture drives the ordinary production entry with a concrete Store. The
// wire peer supplies catalog rows, not preflight return values, and supports the
// remaining startup reads/writes so omitting the entry's preflight reaches run.
func TestManagedPrincipalsStartupEntry(t *testing.T) {
	cases := []string{"valid", "current_identity", "session_identity", "bootstrap_identity", "owned_tables", "owner_login"}
	cases = append(cases, "superuser", "create_role", "create_database", "bypass_rls", "inherit", "replication", "no_login", "membership", "replication_parameter")
	for _, wrong := range cases {
		t.Run(wrong, func(t *testing.T) {
			clearServerRuntimeEnvironment(t)
			t.Setenv("WITSELF_RUNTIME", "local")
			t.Setenv("WITSELF_LOCAL_SYNC_SCHEMA", "1")
			t.Setenv("WITSELF_LOCAL_MANAGED_PRINCIPALS", "1")
			wire, dsn := newManagedStartupWire(t, wrong)
			t.Setenv("WITSELF_DATABASE_URL", dsn)
			spool, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil || os.Chmod(spool, 0o700) != nil {
				t.Fatal("managed ordinary startup private spool setup failed")
			}
			t.Setenv("WITSELF_LOCAL_SPOOL_DIR", spool)

			// Independently establish valid schema coordinates even for identity
			// refusals, whose production ordering intentionally reads no versions.
			st, err := store.Open(context.Background(), dsn)
			if err != nil {
				t.Fatal("managed ordinary startup fixture store could not open")
			}
			core, coreErr := st.AppliedSchemaVersion(context.Background())
			syncVersion, syncErr := st.AppliedSyncSchemaVersion(context.Background())
			st.Close()
			if coreErr != nil || syncErr != nil || core != store.SchemaVersion() || syncVersion != store.SyncSchemaVersion() {
				t.Fatal("managed ordinary startup fixture versions were not independently valid")
			}

			listeners, migrators := 0, 0
			status, output := captureMigrationStartup(t, serverStartup{
				migrator: func(*store.Store) startupMigrator { migrators++; return &managedServerFixture{} },
				notifyContext: func(ctx context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
					return context.WithCancel(ctx)
				},
				bootstrapToken: func() (string, error) { return "", nil },
				run: func(ctx context.Context, cfg server.Config) error {
					listeners++
					if !cfg.LocalRuntime || cfg.AccountID != "acc_fixture" || cfg.Ready == nil || cfg.SyncSchemaReady == nil ||
						cfg.Ready(ctx) != nil || cfg.SyncSchemaReady(ctx) != nil {
						t.Error("managed ordinary startup callback received incomplete store configuration")
					}
					return nil
				},
			})
			if migrators != 0 {
				t.Fatal("managed ordinary startup invoked a migration callback")
			}
			wire.mu.Lock()
			unexpected, writes := wire.unexpected, wire.writes
			wire.mu.Unlock()
			if unexpected {
				t.Fatal("managed ordinary startup fixture received an unexpected query")
			}
			if wrong == "valid" {
				if status != 0 || listeners != 1 || writes != 1 {
					t.Fatal("managed ordinary startup valid control did not reach server callback")
				}
				return
			}
			if listeners != 0 || writes != 0 {
				t.Fatal("managed ordinary startup reached runtime work with refused catalog")
			}
			cause := "witself-server: " + localcell.ErrLocalRuntimeLoginOwnsTables.Error() + "\n"
			if status != 1 || strings.Count(output, cause) != 1 || !strings.HasSuffix(output, cause) {
				t.Fatal("managed ordinary startup lost fixed catalog refusal")
			}
		})
	}
}

type managedStartupWire struct {
	wrong      string
	mu         sync.Mutex
	unexpected bool
	writes     int
}

func newManagedStartupWire(t *testing.T, wrong string) (*managedStartupWire, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("managed startup wire listener could not open")
	}
	wire := &managedStartupWire{wrong: wrong}
	var connections sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer func() { _ = connection.Close() }()
				if connection.SetDeadline(time.Now().Add(30*time.Second)) != nil {
					wire.mu.Lock()
					wire.unexpected = true
					wire.mu.Unlock()
					return
				}
				wire.serve(connection)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
		connections.Wait()
	})
	connection := url.URL{Scheme: "postgres", User: url.User(string(store.SyncPrincipalServer)), Host: listener.Addr().String(), Path: "/fixture"}
	query := connection.Query()
	query.Set("sslmode", "disable")
	query.Set("default_query_exec_mode", "simple_protocol")
	connection.RawQuery = query.Encode()
	return wire, connection.String()
}

func (w *managedStartupWire) serve(connection net.Conn) {
	backend := pgproto3.NewBackend(connection, connection)
	message, err := backend.ReceiveStartupMessage()
	if err != nil {
		return
	}
	if _, ok := message.(*pgproto3.StartupMessage); !ok {
		return
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "16.0"})
	backend.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
	backend.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if backend.Flush() != nil {
		return
	}
	transaction := byte('I')
	for {
		message, err := backend.Receive()
		if err != nil {
			return
		}
		switch message := message.(type) {
		case *pgproto3.Terminate:
			return
		case *pgproto3.Query:
			query := strings.TrimSpace(message.String)
			switch strings.ToLower(query) {
			case "", "-- ping":
				backend.Send(&pgproto3.EmptyQueryResponse{})
			case "begin":
				transaction = 'T'
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")})
			case "commit", "rollback":
				transaction = 'I'
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(strings.ToUpper(query))})
			default:
				w.sendRow(backend, query)
			}
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: transaction})
			if backend.Flush() != nil {
				return
			}
		default:
			w.mu.Lock()
			w.unexpected = true
			w.mu.Unlock()
			return
		}
	}
}

func (w *managedStartupWire) sendRow(backend *pgproto3.Backend, query string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var values []any
	switch {
	case query == "SELECT current_user,session_user":
		current, session := string(store.SyncPrincipalServer), string(store.SyncPrincipalServer)
		switch w.wrong {
		case "current_identity":
			current = string(store.SyncPrincipalOwner)
		case "session_identity":
			session = string(store.SyncPrincipalMigrator)
		case "bootstrap_identity":
			current, session = "localcell", "localcell"
		}
		values = []any{current, session}
	case strings.HasPrefix(query, "SELECT r.rolname,r.rolsuper,r.rolcreaterole,r.rolcreatedb,"):
		_, literal, found := strings.Cut(query, "FROM pg_roles r WHERE r.rolname=")
		role := strings.Trim(strings.TrimSpace(literal), "'")
		if !found || !slices.Contains(store.SyncPrincipalRoleNames(), role) {
			w.reject(backend)
			return
		}
		attributes := []bool{false, false, false, false, false, false, role != string(store.SyncPrincipalOwner), false, false}
		if role == string(store.SyncPrincipalServer) {
			for index, name := range []string{"superuser", "create_role", "create_database", "bypass_rls", "inherit", "replication", "no_login", "membership", "replication_parameter"} {
				if name == w.wrong {
					attributes[index] = !attributes[index]
				}
			}
		}
		values = []any{role}
		for _, attribute := range attributes {
			values = append(values, attribute)
		}
	case strings.HasPrefix(query, "SELECT rolcanlogin FROM pg_roles WHERE rolname="):
		values = []any{w.wrong == "owner_login"}
	case strings.Contains(query, "FROM goose_db_version ORDER BY version_id, id DESC"):
		values = []any{store.SchemaVersion()}
	case strings.HasPrefix(query, "SELECT to_regclass('witself_sync_schema_version') IS NOT NULL,"):
		values = []any{true, true}
	case strings.Contains(query, "FROM witself_sync_schema_version ORDER BY version_id, id DESC"):
		values = []any{store.SyncSchemaVersion()}
	case strings.HasPrefix(query, "SELECT EXISTS (SELECT 1 FROM unnest("):
		values = []any{false}
	case strings.Contains(query, "c.relowner=current_user::regrole"):
		values = []any{w.wrong != "owned_tables"}
	case strings.HasPrefix(query, "WITH expected_functions(signature,runtime_execute) AS (VALUES"):
		values = []any{true}
	case strings.HasPrefix(query, "UPDATE agent_email_cell_storage_capacity"):
		w.writes++
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("UPDATE 1")})
		return
	case query == "SELECT id FROM accounts WHERE is_default", query == "SELECT id FROM accounts WHERE is_default AND status = 'active'":
		values = []any{"acc_fixture"}
	case strings.HasPrefix(query, "SELECT id FROM operators WHERE account_id = ") && strings.HasSuffix(query, " AND is_root"):
		values = []any{"opr_fixture"}
	default:
		w.reject(backend)
		return
	}
	fields := make([]pgproto3.FieldDescription, len(values))
	row := make([][]byte, len(values))
	for index, value := range values {
		fields[index] = pgproto3.FieldDescription{Name: []byte("fixture"), TypeModifier: -1, DataTypeSize: -1}
		switch value := value.(type) {
		case bool:
			fields[index].DataTypeOID = 16
			row[index] = []byte(strconv.FormatBool(value))
		case int:
			fields[index].DataTypeOID = 20
			row[index] = []byte(strconv.Itoa(value))
		case string:
			fields[index].DataTypeOID = 25
			row[index] = []byte(value)
		default:
			w.reject(backend)
			return
		}
	}
	backend.Send(&pgproto3.RowDescription{Fields: fields})
	backend.Send(&pgproto3.DataRow{Values: row})
	backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
}

func (w *managedStartupWire) reject(backend *pgproto3.Backend) {
	w.unexpected = true
	backend.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "42601", Message: "managed startup wire refused unexpected query"})
}
