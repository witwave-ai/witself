package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/witwave-ai/witself/internal/store"
)

func TestInstallerPrincipalsSelection(t *testing.T) {
	t.Run("ExistingDataWithoutManifest", func(t *testing.T) {
		cell, _ := unitCell(t)
		if os.Remove(cell.path("manifest.json")) != nil || selectedFixtureDataDirectory(cell) != nil {
			t.Fatal("existing data refusal fixture failed")
		}
		path := cell.path("pg", "retained-data")
		if os.WriteFile(path, []byte("retained"), 0o600) != nil {
			t.Fatal("existing data preservation fixture failed")
		}
		calls := 0
		cell.installerPrincipals = recordingPrincipalOperations(&calls)
		if cell.Install(t.Context(), InstallOptions{ManagedPrincipals: true}) == nil || calls != 0 {
			t.Fatal("selected install adopted existing unmarked data")
		}
		data, _ := os.ReadFile(path)
		if string(data) != "retained" {
			t.Fatal("selected refusal changed existing data bytes")
		}
		if _, err := os.Stat(cell.path("journal", "operations.jsonl")); !os.IsNotExist(err) {
			t.Fatal("existing data refusal wrote provisioning journal")
		}
	})
	c, m := unitCell(t)
	if err := c.makePassword(); err != nil {
		t.Fatal("selection fixture password failed")
	}
	before, _ := os.ReadFile(c.path("manifest.json"))
	legacyEnvironment, err := c.childEnvironment(m, "server")
	if err != nil {
		t.Fatal("legacy environment fixture failed")
	}
	t.Setenv("WITSELF_LOCAL_MANAGED_PRINCIPALS", "1")
	t.Setenv("WITSELF_LOCAL_MIGRATION_PHASE", "1")
	afterEnvironment, err := c.childEnvironment(m, "server")
	if err != nil || !slices.Equal(legacyEnvironment, afterEnvironment) {
		t.Fatal("legacy environment bytes changed by ambient selection")
	}
	if bytes.Contains(before, []byte("principal_")) || (InstallOptions{}).ManagedPrincipals {
		t.Fatal("legacy manifest gained selected metadata")
	}
	calls := 0
	c.installerPrincipals = recordingPrincipalOperations(&calls)
	if c.Install(t.Context(), InstallOptions{ManagedPrincipals: true}) == nil || calls != 0 {
		t.Fatal("legacy selected adoption reached provisioning")
	}
	after, _ := os.ReadFile(c.path("manifest.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("selected adoption changed legacy manifest bytes")
	}
	for _, mutation := range []func(*Manifest){
		func(m *Manifest) { m.PrincipalLayoutVersion = 99 },
		func(m *Manifest) { m.PrincipalRoles = installerRoleNames() },
		func(m *Manifest) { m.PrincipalLayoutVersion = 1 },
		func(m *Manifest) { m.PrincipalLayoutVersion = 1; m.PrincipalRoles = []string{"unreviewed"} },
	} {
		changed := *m
		mutation(&changed)
		if err := writeAtomic(c.path("manifest.json"), &changed, nil); err != nil {
			t.Fatal("invalid selection fixture failed")
		}
		invalidBytes, _ := os.ReadFile(c.path("manifest.json"))
		if _, err := readManifest(c.cfg.Root); err == nil || c.Start(t.Context()) == nil || c.Repair(t.Context()) == nil || calls != 0 {
			t.Fatal("invalid selected marker reached a lifecycle operation")
		}
		current, _ := os.ReadFile(c.path("manifest.json"))
		if !bytes.Equal(current, invalidBytes) {
			t.Fatal("invalid selected marker changed persistent bytes")
		}
		for _, path := range []string{c.path("journal", "operations.jsonl"), c.path("run", "server.json"), c.path("run", "worker.json")} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("invalid selected marker wrote provisioning or child records")
			}
		}
	}
	m.PrincipalLayoutVersion, m.PrincipalRoles = PrincipalLayoutVersion, installerRoleNames()
	if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
		t.Fatal("supported selected fixture failed")
	}
	got, err := readManifest(c.cfg.Root)
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatal("supported selected marker failed round trip")
	}
}

func recordingPrincipalOperations(calls *int) *installerPrincipalOperations {
	f := func(context.Context, *Manifest) error { *calls++; return nil }
	return &installerPrincipalOperations{f, f, f, f, f, f}
}

type installerBoolRow struct {
	value bool
	err   error
}

func (r installerBoolRow) Scan(values ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(values) != 1 {
		return errors.New("unexpected catalog projection")
	}
	value, ok := values[0].(*bool)
	if !ok {
		return errors.New("unexpected catalog type")
	}
	*value = r.value
	return nil
}

type installerCatalogFixture struct {
	owner, migrator, drained bool
	calls                    int
	roles                    []string
}

func (f *installerCatalogFixture) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	f.calls++
	switch {
	case strings.Contains(query, "pg_stat_activity"):
		if len(args) != 1 {
			return installerBoolRow{err: errors.New("unexpected drain arguments")}
		}
		f.roles, _ = args[0].([]string)
		return installerBoolRow{value: f.drained}
	case strings.Contains(query, "witself_sync_migrator"):
		return installerBoolRow{value: f.migrator}
	default:
		return installerBoolRow{value: f.owner}
	}
}

func TestInstallerPrincipalsCatalog(t *testing.T) {
	wire, st := newInstallerCatalogWire(t)
	attributes := map[string]int{"superuser": 0, "create_role": 1, "create_database": 2, "bypass_rls": 3, "inherit": 4, "replication": 5, "login": 6, "membership": 7, "replication_parameter": 8}
	for _, role := range store.SyncPrincipalRoleNames() {
		for _, predicate := range []string{"missing", "superuser", "create_role", "create_database", "bypass_rls", "inherit", "replication", "membership", "replication_parameter", "login"} {
			t.Run(role+"/"+predicate, func(t *testing.T) {
				wire.reset()
				if st.VerifySyncPrincipals(t.Context()) != nil {
					t.Fatal("installer catalog wire baseline did not verify actual role rows")
				}
				wire.mu.Lock()
				if predicate == "missing" {
					delete(wire.roles, role)
				} else {
					row := wire.roles[role]
					index := attributes[predicate]
					row[index] = !row[index]
					wire.roles[role] = row
				}
				wire.mu.Unlock()
				q := &installerCatalogFixture{owner: true, migrator: true}
				ownerExemption := role == string(store.SyncPrincipalOwner) && (predicate == "login" || predicate == "membership" || predicate == "replication_parameter")
				if ownerExemption {
					// C2a deliberately excludes these owner-only predicates. The
					// installer imposes them independently after the real oracle.
					if st.VerifySyncPrincipals(t.Context()) != nil {
						t.Fatal("installer owner exemption no longer matches C2a")
					}
					q.owner = false
				}
				if validateInstallerCatalog(t.Context(), q, st.VerifySyncPrincipals) == nil {
					t.Fatal("installer accepted independently corrupted catalog role")
				}
				if (!ownerExemption && q.calls != 0) || (ownerExemption && q.calls != 1) {
					t.Fatal("installer catalog corruption crossed its expected refusal boundary")
				}
			})
		}
	}
	wire.reset()
	for _, invalid := range []string{"owner_login", "migrator_membership"} {
		q := &installerCatalogFixture{owner: invalid != "owner_login", migrator: invalid != "migrator_membership"}
		if validateInstallerCatalog(t.Context(), q, st.VerifySyncPrincipals) == nil {
			t.Fatal("installer accepted unsafe additional principal custody")
		}
	}
	q := &installerCatalogFixture{owner: true, migrator: true}
	before := wire.selectCount()
	for range 2 {
		if validateInstallerCatalog(t.Context(), q, st.VerifySyncPrincipals) != nil {
			t.Fatal("valid installer catalog repeat failed")
		}
	}
	if wire.selectCount()-before != 12 {
		t.Fatal("installer catalog repeat did not read every role independently")
	}
}

// Local-cell causes use fixed vocabulary. Keep diagnostics useful without
// admitting a path, connection string, or a wrapped error into failure output.
func installerPrincipalFailureSuffix(err error) string {
	if err != nil && !strings.ContainsAny(err.Error(), `/\:`) {
		return ": " + err.Error()
	}
	return ""
}

func selectedJournalFixture(t *testing.T) (*Cell, *Manifest) {
	t.Helper()
	c, m := unitCell(t)
	m.Installed = false
	m.PrincipalLayoutVersion, m.PrincipalRoles = PrincipalLayoutVersion, installerRoleNames()
	if writeAtomic(c.path("manifest.json"), m, nil) != nil || c.begin("install") != nil {
		t.Fatal("selected journal fixture failed")
	}
	for _, name := range []string{"manifest", "password", "initdb", "configure", "postgres", "database"} {
		if c.step("install", name, func() error { return nil }) != nil {
			t.Fatal("selected journal prefix failed")
		}
	}
	return c, m
}

func TestInstallerPrincipalsMigrationOrder(t *testing.T) {
	t.Run("LaunchAfterDrainAndObserverClose", func(t *testing.T) {
		for _, drained := range []bool{false, true} {
			observer := &installerCloseFixture{open: true}
			catalog := &installerCatalogFixture{drained: drained}
			launches := 0
			err := runPrincipalStartup(func() error {
				return withInstallerConnection(observer, func() error { return requirePrivilegedSessionDrain(t.Context(), catalog, "localcell") })
			}, func() error {
				launches++
				if observer.open || catalog.calls == 0 || !drained {
					t.Fatal("runtime launch preceded privileged drain or observer close")
				}
				return nil
			})
			if (err == nil) != drained || (launches == 1) != drained {
				t.Fatal("runtime launch crossed refused privileged-session proof")
			}
		}
	})
	t.Run("PrivilegedCloseBeforeNextPhase", func(t *testing.T) {
		for _, actionErr := range []error{nil, errPrincipalProvision, context.Canceled} {
			connection := &installerCloseFixture{open: true}
			err := withInstallerConnection(connection, func() error {
				if !connection.open {
					t.Fatal("privileged connection closed before its bounded operation")
				}
				return actionErr
			})
			if !errors.Is(err, actionErr) || connection.open || connection.closes != 1 || !connection.liveContext {
				t.Fatal("privileged connection was not closed before subsequent phase")
			}
		}
	})
	for fail := -1; fail < len(selectedInstallSteps); fail++ {
		c, m := selectedJournalFixture(t)
		var observed []string
		ops := &installerPrincipalOperations{}
		fields := []*func(context.Context, *Manifest) error{&ops.credentials, &ops.provision, &ops.migrate, &ops.grants, &ops.verify}
		for i, field := range fields {
			*field = func(context.Context, *Manifest) error {
				observed = append(observed, selectedInstallSteps[i])
				if i == fail {
					return errPrincipalProvision
				}
				return nil
			}
		}
		c.installerPrincipals = ops
		err := c.installPrincipals(t.Context(), m, journalProgress{})
		want := selectedInstallSteps
		if fail >= 0 {
			want = want[:fail+1]
		}
		if !slices.Equal(observed, want) || (err != nil) != (fail >= 0) {
			t.Fatal("selected install crossed an unsuccessful phase")
		}
		read, readErr := readManifest(c.cfg.Root)
		if readErr != nil || read.Installed {
			t.Fatal("selected phase prematurely marked installation complete")
		}
	}
	t.Run("HeldMigratorSession", func(t *testing.T) {
		q := &installerCatalogFixture{}
		if requirePrivilegedSessionDrain(t.Context(), q, "localcell") == nil {
			t.Fatal("held privileged session did not refuse runtime launch")
		}
		if !slices.Equal(q.roles, []string{"localcell", installerMigrator}) {
			t.Fatal("drain did not observe both authenticated privileged logins")
		}
		q.drained = true
		if requirePrivilegedSessionDrain(t.Context(), q, "localcell") != nil {
			t.Fatal("closed privileged sessions failed drain observation")
		}
	})
	t.Run("Cancellation", func(t *testing.T) {
		c, m := selectedJournalFixture(t)
		calls := 0
		c.installerPrincipals = recordingPrincipalOperations(&calls)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if c.installPrincipals(ctx, m, journalProgress{}) == nil || calls != 0 {
			t.Fatal("cancelled selected install crossed first phase")
		}
	})
}

func TestInstallerPrincipalsRepairFence(t *testing.T) {
	for cut := 0; cut <= len(selectedInstallSteps); cut++ {
		c, m := selectedJournalFixture(t)
		prefix, err := c.readJournal()
		if err != nil || prefix.installComplete(m) {
			t.Fatal("old four-phase predicate completed selected installation")
		}
		calls := 0
		c.installerPrincipals = recordingPrincipalOperations(&calls)
		for _, step := range selectedInstallSteps[:cut] {
			if c.step("install", step, func() error { return nil }) != nil {
				t.Fatal("repair durable phase fixture failed")
			}
		}
		if selectedFixtureDataDirectory(c) != nil {
			t.Fatal("repair retained database fixture failed")
		}
		foreign := c.path("pg", "foreign-record")
		// Match production private-file ownership instead of the process token's
		// default owner, which can be Administrators on elevated Windows runners.
		if testWriteFile(foreign, []byte("retained"), 0o600) != nil {
			t.Fatal("repair preservation fixture failed")
		}
		if err := c.validateRoot(false); err != nil {
			t.Fatalf("repair fixture did not retain a private tree%s", installerPrincipalFailureSuffix(err))
		}
		if c.Start(t.Context()) == nil {
			t.Fatal("pending selected phases allowed start")
		}
		c.installerPrincipals.verify = func(context.Context, *Manifest) error { return errPrincipalProvision }
		if c.Repair(t.Context()) == nil {
			t.Fatal("failed selected verification allowed repair success")
		}
		read, _ := readManifest(c.cfg.Root)
		if read == nil || read.Installed {
			t.Fatal("old journal phases incorrectly completed selected install")
		}
		c.installerPrincipals.verify = func(context.Context, *Manifest) error { calls++; return nil }
		if err := c.Repair(t.Context()); err != nil {
			t.Fatalf("unambiguous selected repair failed%s", installerPrincipalFailureSuffix(err))
		}
		read, _ = readManifest(c.cfg.Root)
		if read == nil || !read.Installed || read.InstallationID != m.InstallationID {
			t.Fatal("selected repair did not finish exact installation")
		}
		before := calls
		if c.Repair(t.Context()) != nil || calls != before {
			t.Fatal("second selected repair repeated bounded work")
		}
		if c.Stop(t.Context()) != nil {
			t.Fatal("repair then stop failed")
		}
		data, _ := os.ReadFile(foreign)
		if string(data) != "retained" {
			t.Fatal("selected repair changed foreign bytes")
		}
		inspection, err := c.Inspect(t.Context())
		if err != nil || inspection.PrincipalTopology != "unverified" {
			t.Fatal("persisted selected metadata claimed verified topology")
		}
		encoded, _ := json.Marshal(inspection)
		if bytes.Contains(encoded, []byte("postgres://")) {
			t.Fatal("inspection exposed connection material")
		}
	}
}

type installerCloseFixture struct {
	open        bool
	closes      int
	liveContext bool
}

func selectedFixtureDataDirectory(c *Cell) error {
	if err := c.preparePostgresDirectory(); err != nil {
		return err
	}
	if _, err := os.Lstat(c.path("pg")); errors.Is(err, os.ErrNotExist) {
		return makePrivateDir(c.path("pg"))
	}
	return nil
}

func (c *installerCloseFixture) Close(ctx context.Context) error {
	c.liveContext = ctx.Err() == nil
	c.open = false
	c.closes++
	return nil
}

func TestInstallerPrincipalsRepairPublishedManifest(t *testing.T) {
	c, m := selectedJournalFixture(t)
	for _, step := range selectedInstallSteps {
		if c.step("install", step, func() error { return nil }) != nil {
			t.Fatal("published repair durable fixture failed")
		}
	}
	m.Installed = true
	if writeAtomic(c.path("manifest.json"), m, nil) != nil {
		t.Fatal("published repair manifest fixture failed")
	}
	calls := 0
	c.installerPrincipals = recordingPrincipalOperations(&calls)
	c.installerPrincipals.provision = func(context.Context, *Manifest) error {
		t.Fatal("published repair reopened provisioning authority")
		return nil
	}
	if c.Repair(t.Context()) != nil {
		t.Fatal("published manifest interruption failed repair")
	}
	progress, err := c.readJournal()
	if err != nil || progress.pending != nil {
		t.Fatal("published manifest repair did not close install journal")
	}
	if c.Repair(t.Context()) != nil {
		t.Fatal("published manifest repair was not idempotent")
	}
}

type installerPreMigrationACLFixture struct {
	calls     int
	rejectAt  int
	scanError bool
	seen      map[string]bool
}

func (f *installerPreMigrationACLFixture) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	f.calls++
	if len(args) != 1 {
		return installerBoolRow{err: errors.New("unexpected pre-migration arguments")}
	}
	roles, ok := args[0].([]string)
	if !ok || !slices.Equal(roles, []string{string(store.SyncPrincipalServer), string(store.SyncPrincipalWorker),
		string(store.SyncPrincipalMaintenance), string(store.SyncPrincipalSnapshot), string(store.SyncPrincipalLegacy)}) {
		return installerBoolRow{err: errors.New("unexpected pre-migration role inventory")}
	}
	if f.seen == nil {
		f.seen = make(map[string]bool)
	}
	if f.seen[query] {
		return installerBoolRow{err: errors.New("duplicate pre-migration predicate")}
	}
	f.seen[query] = true
	if f.calls == f.rejectAt {
		if f.scanError {
			return installerBoolRow{err: errors.New("pre-migration catalog unavailable")}
		}
		return installerBoolRow{value: false}
	}
	return installerBoolRow{value: true}
}

func TestInstallerPrincipalsPreMigrationACL(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		q := &installerPreMigrationACLFixture{}
		currentCalls := 0
		err := validateInstallerPreMigrationACL(t.Context(), q,
			func(context.Context) (int, error) { return version, nil },
			func(context.Context) error { currentCalls++; return nil })
		if err != nil || q.calls != 4 || len(q.seen) != 4 || currentCalls != 0 {
			t.Fatal("valid intermediate schema did not use all read-only ACL predicates")
		}
	}
	for _, scanError := range []bool{false, true} {
		for rejectAt := 1; rejectAt <= 4; rejectAt++ {
			q := &installerPreMigrationACLFixture{rejectAt: rejectAt, scanError: scanError}
			currentCalls := 0
			err := validateInstallerPreMigrationACL(t.Context(), q,
				func(context.Context) (int, error) { return 2, nil },
				func(context.Context) error { currentCalls++; return nil })
			if !errors.Is(err, errPrincipalProvision) || q.calls != rejectAt || currentCalls != 0 {
				t.Fatal("unsafe pre-migration ACL or failed catalog read reached a later predicate")
			}
		}
	}
}

func TestInstallerPrincipalsPreMigrationCurrentTopology(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		q := &installerPreMigrationACLFixture{}
		currentCalls := 0
		err := validateInstallerPreMigrationACL(t.Context(), q,
			func(context.Context) (int, error) { return store.SyncSchemaVersion(), nil },
			func(context.Context) error {
				currentCalls++
				if refuse {
					return store.ErrSyncPrincipalTopologyUnverified
				}
				return nil
			})
		if (err != nil) != refuse || q.calls != 0 || currentCalls != 1 {
			t.Fatal("completed schema did not preserve the full current runtime topology verifier")
		}
	}
	for _, version := range []int{-1, store.SyncSchemaVersion() + 1} {
		q := &installerPreMigrationACLFixture{}
		currentCalls := 0
		err := validateInstallerPreMigrationACL(t.Context(), q,
			func(context.Context) (int, error) { return version, nil },
			func(context.Context) error { currentCalls++; return nil })
		if !errors.Is(err, errPrincipalProvision) || q.calls != 0 || currentCalls != 0 {
			t.Fatal("unsupported sync version reached pre-migration ACL acceptance")
		}
	}
}

func TestInstallerPrincipalsPreMigrationCancellation(t *testing.T) {
	for _, cancelBeforeRead := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if cancelBeforeRead {
			cancel()
		}
		q := &installerPreMigrationACLFixture{}
		reads, currentCalls := 0, 0
		err := validateInstallerPreMigrationACL(ctx, q,
			func(context.Context) (int, error) { reads++; cancel(); return store.SyncSchemaVersion(), nil },
			func(context.Context) error { currentCalls++; return nil })
		cancel()
		wantReads := 1
		if cancelBeforeRead {
			wantReads = 0
		}
		if !errors.Is(err, errPrincipalProvision) || reads != wantReads || q.calls != 0 || currentCalls != 0 {
			t.Fatal("cancelled pre-migration ACL observation reached later work")
		}
	}
	q := &installerPreMigrationACLFixture{}
	currentCalls := 0
	err := validateInstallerPreMigrationACL(t.Context(), q,
		func(context.Context) (int, error) { return 0, errors.New("sync version unavailable") },
		func(context.Context) error { currentCalls++; return nil })
	if !errors.Is(err, errPrincipalProvision) || q.calls != 0 || currentCalls != 0 {
		t.Fatal("failed sync version read reached pre-migration ACL acceptance")
	}
}

// installerCatalogWire feeds individual PostgreSQL catalog rows through the
// production Store.VerifySyncPrincipals scanner and predicates. It implements
// only the read-only wire exchange; no PostgreSQL process or cluster is used.
type installerCatalogWire struct {
	mu       sync.Mutex
	roles    map[string][9]bool
	selects  int
	rejected bool
}

func (w *installerCatalogWire) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.roles = make(map[string][9]bool)
	for index, role := range store.SyncPrincipalRoleNames() {
		var attributes [9]bool
		attributes[6] = index != 0
		w.roles[role] = attributes
	}
}

func (w *installerCatalogWire) selectCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.selects
}

func newInstallerCatalogWire(t *testing.T) (*installerCatalogWire, *store.Store) {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("installer catalog wire listener unavailable")
	}
	wire := &installerCatalogWire{}
	wire.reset()
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		if connection.SetDeadline(time.Now().Add(30*time.Second)) != nil {
			return
		}
		wire.serve(connection)
	}()
	u := url.URL{Scheme: "postgres", Host: listener.Addr().String(), Path: "/catalog_fixture",
		User:     url.UserPassword("installer_fixture", "fixture-only-catalog-password"),
		RawQuery: "sslmode=disable&default_query_exec_mode=simple_protocol&pool_max_conns=1&connect_timeout=2"}
	st, err := store.Open(t.Context(), u.String())
	if err != nil {
		_ = listener.Close()
		t.Fatal("installer catalog wire store unavailable")
	}
	t.Cleanup(func() {
		st.Close()
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("installer catalog wire cleanup did not complete")
		}
		wire.mu.Lock()
		defer wire.mu.Unlock()
		if wire.rejected || wire.selects == 0 {
			t.Error("installer catalog wire observed noncatalog SQL or incomplete setup")
		}
	})
	return wire, st
}

func (w *installerCatalogWire) serve(connection net.Conn) {
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
	for {
		message, err := backend.Receive()
		if err != nil {
			return
		}
		switch message := message.(type) {
		case *pgproto3.Terminate:
			return
		case *pgproto3.Query:
			if message.String == "" {
				backend.Send(&pgproto3.EmptyQueryResponse{})
			} else {
				w.sendCatalogRow(backend, message.String)
			}
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if backend.Flush() != nil {
				return
			}
		default:
			w.mu.Lock()
			w.rejected = true
			w.mu.Unlock()
			return
		}
	}
}

func (w *installerCatalogWire) sendCatalogRow(backend *pgproto3.Backend, query string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, roleLiteral, found := strings.Cut(query, "FROM pg_roles r WHERE r.rolname=")
	roleLiteral = strings.TrimSpace(roleLiteral)
	role := strings.Trim(roleLiteral, "'")
	if !found || roleLiteral != "'"+role+"'" || !slices.Contains(store.SyncPrincipalRoleNames(), role) ||
		!strings.HasPrefix(query, "SELECT r.rolname,r.rolsuper,r.rolcreaterole,r.rolcreatedb,") ||
		!strings.Contains(query, "r.rolbypassrls,r.rolinherit,r.rolreplication,r.rolcanlogin") ||
		!strings.Contains(query, "EXISTS(SELECT 1 FROM pg_auth_members m WHERE m.member=r.oid)") ||
		!strings.Contains(query, "has_parameter_privilege(r.oid,'session_replication_role','SET')") {
		w.rejected = true
		backend.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "42601", Message: "catalog fixture refused unexpected query"})
		return
	}
	w.selects++
	fields := []pgproto3.FieldDescription{{Name: []byte("rolname"), DataTypeOID: 25, DataTypeSize: -1, TypeModifier: -1}}
	for _, name := range []string{"rolsuper", "rolcreaterole", "rolcreatedb", "rolbypassrls", "rolinherit", "rolreplication", "rolcanlogin", "membership", "replication_parameter"} {
		fields = append(fields, pgproto3.FieldDescription{Name: []byte(name), DataTypeOID: 16, DataTypeSize: 1, TypeModifier: -1})
	}
	backend.Send(&pgproto3.RowDescription{Fields: fields})
	if attributes, exists := w.roles[role]; exists {
		values := [][]byte{[]byte(role)}
		for _, enabled := range attributes {
			value := []byte("f")
			if enabled {
				value = []byte("t")
			}
			values = append(values, value)
		}
		backend.Send(&pgproto3.DataRow{Values: values})
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
	} else {
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 0")})
	}
}

// This fixture keeps authenticated sessions independently of SET ROLE, so the
// production pg_stat_activity usesysid query can distinguish login from role.
type installerMaintenanceWire struct {
	mu                    sync.Mutex
	sessions              map[net.Conn]string
	current               map[net.Conn]string
	observerReads         int
	cancelHeldObservation context.CancelFunc
	rejected              bool
	endpoint              string
}

func newInstallerMaintenanceWire(t *testing.T) *installerMaintenanceWire {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("maintenance wire listener unavailable")
	}
	w := &installerMaintenanceWire{sessions: map[net.Conn]string{}, current: map[net.Conn]string{}, endpoint: listener.Addr().String()}
	var clients sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			clients.Add(1)
			go func() { defer clients.Done(); w.serve(conn) }()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
		w.mu.Lock()
		for conn := range w.sessions {
			_ = conn.Close()
		}
		w.mu.Unlock()
		clients.Wait()
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.rejected {
			t.Error("maintenance wire refused unexpected query")
		}
	})
	return w
}

func (w *installerMaintenanceWire) dsn(role string) string {
	u := url.URL{Scheme: "postgres", Host: w.endpoint, Path: "/maintenance_fixture", User: url.User(role), RawQuery: "sslmode=disable&default_query_exec_mode=simple_protocol&pool_max_conns=1&connect_timeout=2"}
	return u.String()
}

func (w *installerMaintenanceWire) serve(conn net.Conn) {
	defer func() {
		w.mu.Lock()
		delete(w.sessions, conn)
		delete(w.current, conn)
		w.mu.Unlock()
		_ = conn.Close()
	}()
	backend := pgproto3.NewBackend(conn, conn)
	message, err := backend.ReceiveStartupMessage()
	if err != nil {
		return
	}
	startup, ok := message.(*pgproto3.StartupMessage)
	if !ok {
		return
	}
	w.mu.Lock()
	w.sessions[conn] = startup.Parameters["user"]
	w.current[conn] = startup.Parameters["user"]
	w.mu.Unlock()
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "16.0"})
	backend.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
	backend.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if backend.Flush() != nil {
		return
	}
	for {
		message, err = backend.Receive()
		if err != nil {
			return
		}
		switch query := message.(type) {
		case *pgproto3.Terminate:
			return
		case *pgproto3.Query:
			w.respond(backend, conn, query.String)
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if backend.Flush() != nil {
				return
			}
		default:
			w.mu.Lock()
			w.rejected = true
			w.mu.Unlock()
			return
		}
	}
}

func (w *installerMaintenanceWire) respond(backend *pgproto3.Backend, conn net.Conn, query string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	row := func(values ...string) {
		fields := make([]pgproto3.FieldDescription, len(values))
		data := make([][]byte, len(values))
		for i, value := range values {
			oid := uint32(25)
			size := int16(-1)
			if value == "t" || value == "f" {
				oid = 16
				size = 1
			} else if _, err := strconv.Atoi(value); err == nil {
				oid = 23
				size = 4
			}
			fields[i] = pgproto3.FieldDescription{Name: []byte("value"), DataTypeOID: oid, DataTypeSize: size, TypeModifier: -1}
			data[i] = []byte(value)
		}
		backend.Send(&pgproto3.RowDescription{Fields: fields})
		backend.Send(&pgproto3.DataRow{Values: data})
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
	}
	switch {
	case query == "":
		backend.Send(&pgproto3.EmptyQueryResponse{})
	case query == "SET ROLE witself_sync_owner":
		w.current[conn] = string(store.SyncPrincipalOwner)
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SET")})
	case query == "SELECT current_user,session_user":
		row(w.current[conn], w.sessions[conn])
	case strings.Contains(query, "pg_stat_activity"):
		w.observerReads++
		if !strings.Contains(query, "usesysid IN") || !strings.Contains(query, "localcell") || !strings.Contains(query, installerMigrator) {
			w.rejected = true
		}
		drained := true
		for _, login := range w.sessions {
			if login == "localcell" || login == installerMigrator {
				drained = false
			}
		}
		if drained {
			row("t")
		} else {
			row("f")
			if w.cancelHeldObservation != nil {
				w.cancelHeldObservation()
			}
		}
	case strings.HasPrefix(query, "SELECT r.rolname,r.rolsuper"):
		_, name, _ := strings.Cut(query, "FROM pg_roles r WHERE r.rolname=")
		role := strings.Trim(strings.TrimSpace(name), "'")
		login := "t"
		if role == string(store.SyncPrincipalOwner) {
			login = "f"
		}
		row(role, "f", "f", "f", "f", "f", "f", login, "f", "f")
	case strings.HasPrefix(query, "SELECT rolcanlogin FROM pg_roles"):
		row("f")
	case strings.Contains(query, "SELECT to_regclass('witself_sync_schema_version')"):
		row("t", "t")
	case strings.Contains(query, "SELECT DISTINCT ON (version_id)"):
		if strings.Contains(query, "FROM witself_sync_schema_version") {
			row(strconv.Itoa(store.SyncSchemaVersion()))
		} else {
			row(strconv.Itoa(store.SchemaVersion()))
		}
	case strings.Contains(query, "AS required(name)"):
		row("f")
	case strings.HasPrefix(query, "SELECT\n EXISTS(SELECT 1 FROM pg_namespace"), strings.HasPrefix(query, "WITH expected_functions"), strings.HasPrefix(query, "SELECT NOT rolcanlogin"), strings.HasPrefix(query, "SELECT rolcanlogin AND NOT"), strings.HasPrefix(query, "SELECT NOT EXISTS(SELECT 1 FROM pg_roles r"):
		row("t")
	default:
		w.rejected = true
		backend.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "42601", Message: "maintenance fixture refused unexpected query"})
	}
}

func TestInstallerPrincipalsStartVerification(t *testing.T) {
	for _, login := range []string{"localcell", installerMigrator} {
		for _, changedRole := range []bool{false, true} {
			name := login + "/login-role"
			if changedRole {
				name = login + "/owner-role"
			}
			t.Run(name, func(t *testing.T) {
				c, m, _ := selectedCredentialFixture(t)
				m.Installed = true
				if selectedFixtureDataDirectory(c) != nil {
					t.Fatal("selected startup database fixture failed")
				}
				if writeAtomic(c.path("manifest.json"), m, nil) != nil {
					t.Fatal("selected startup fixture publication failed")
				}
				w := newInstallerMaintenanceWire(t)
				maintenanceDSN := w.dsn(string(store.SyncPrincipalMaintenance))
				var observers []*pgx.Conn
				c.maintenanceTest = &installerMaintenanceOperations{
					dsn: func() string { return maintenanceDSN },
					connect: func(ctx context.Context, dsn string) (*pgx.Conn, error) {
						conn, err := pgx.Connect(ctx, dsn)
						if err == nil {
							observers = append(observers, conn)
						}
						return conn, err
					},
				}
				held, err := pgx.Connect(t.Context(), w.dsn(login))
				if err != nil {
					t.Fatal("held authenticated session fixture failed")
				}
				defer closeInstallerConnection(held)
				if changedRole {
					if _, err := held.Exec(t.Context(), "SET ROLE witself_sync_owner"); err != nil {
						t.Fatal("held session role change failed")
					}
				}
				var current, session string
				if held.QueryRow(t.Context(), "SELECT current_user,session_user").Scan(&current, &session) != nil || session != login || (changedRole && current != string(store.SyncPrincipalOwner)) {
					t.Fatal("held session authentication and current role were not independent")
				}
				launches := 0
				c.installerStart = &installerStartOperations{
					postgres: func(context.Context, *Manifest) error { return nil },
					verify: func(ctx context.Context, m *Manifest) error {
						ctx, cancel := context.WithCancel(ctx)
						defer cancel()
						w.mu.Lock()
						w.cancelHeldObservation = cancel
						w.mu.Unlock()
						return c.verifyMaintenanceTopology(ctx, m)
					},
					launch: func(context.Context, *Manifest, string, func() error) error {
						launches++
						w.mu.Lock()
						reads := w.observerReads
						w.mu.Unlock()
						if reads == 0 || len(observers) < 2 {
							t.Fatal("selected Start launched before production drain observation")
						}
						for _, observer := range observers {
							if !observer.IsClosed() {
								t.Fatal("selected Start launched before maintenance observer closed")
							}
						}
						return nil
					},
					ready:   func(context.Context, *Manifest, string, bool) error { return nil },
					account: func(context.Context, *Manifest) error { return nil },
				}
				if err := c.Start(t.Context()); !errors.Is(err, errPrincipalSessions) || launches != 0 {
					t.Fatal("selected Start did not refuse held authenticated privileged session before launch")
				}
				closeInstallerConnection(held)
				// Wait only for fixture socket teardown, not for production verification.
				deadline := time.Now().Add(time.Second)
				for {
					w.mu.Lock()
					count := 0
					for _, role := range w.sessions {
						if role == login {
							count++
						}
					}
					w.mu.Unlock()
					if count == 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("held session fixture failed to close")
					}
					time.Sleep(time.Millisecond)
				}
				for attempt := 0; attempt < 2; attempt++ {
					observers = nil
					w.mu.Lock()
					w.observerReads = 0
					w.mu.Unlock()
					if c.Start(t.Context()) != nil || launches != (attempt+1)*2 {
						t.Fatal("selected Start or restart failed after authenticated sessions drained")
					}
					if c.Stop(t.Context()) != nil {
						t.Fatal("selected restart stop boundary failed")
					}
				}
			})
		}
	}
}
