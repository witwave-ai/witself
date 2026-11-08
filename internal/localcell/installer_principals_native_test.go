package localcell_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/witwave-ai/witself/internal/localcell"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/syncharness"
)

func nativePrincipalManifest(t *testing.T, root string) localcell.Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	var m localcell.Manifest
	if err != nil || json.Unmarshal(data, &m) != nil {
		t.Fatal("native principal manifest unavailable")
	}
	return m
}

func nativePrincipalDSN(t *testing.T, root string, m localcell.Manifest, role string) string {
	t.Helper()
	path := filepath.Join(root, "run", "credentials", role)
	if role == "localcell" {
		path = filepath.Join(root, "run", "postgres.password")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("native principal credential unavailable")
	}
	u := url.URL{Scheme: "postgres", Host: "127.0.0.1:" + strconv.Itoa(m.Ports.Postgres), Path: "/" + m.Database, User: url.UserPassword(role, strings.TrimSpace(string(value))), RawQuery: "sslmode=disable"}
	if role == string(store.SyncPrincipalMigrator) {
		q := u.Query()
		q.Set("options", "-c role="+string(store.SyncPrincipalOwner))
		u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	}
	return u.String()
}

func nativePrincipalConnection(t *testing.T, root string, m localcell.Manifest, role string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(t.Context(), nativePrincipalDSN(t, root, m, role))
	if err != nil {
		t.Fatal("native principal connection failed")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = c.Close(ctx)
	})
	return c
}

func nativePrincipalCredentialMismatch(t *testing.T, cell *localcell.Cell, root string) {
	t.Helper()
	if cell.Stop(t.Context()) != nil || cell.SetServiceDesiredState(t.Context(), "running") != nil {
		t.Fatal("native credential mismatch fixture could not stop selected runtime")
	}
	path := filepath.Join(root, "run", "credentials", string(store.SyncPrincipalServer))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("native credential mismatch fixture could not retain original private bytes")
	}
	restored := false
	defer func() {
		if !restored && os.WriteFile(path, original, 0o600) != nil {
			t.Error("native credential mismatch fixture could not restore original private bytes")
		}
	}()
	// Rewriting the existing file preserves its private native DACL or Unix
	// owner/mode. Only authentication changes; format and filesystem checks pass.
	mismatch := []byte(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{237}, 32)) + "\n")
	if bytes.Equal(original, mismatch) || os.WriteFile(path, mismatch, 0o600) != nil {
		t.Fatal("native credential mismatch fixture could not install a distinct valid value")
	}
	if cell.Start(t.Context()) == nil {
		t.Fatal("native credential mismatch reached a runtime child")
	}
	inspection, err := cell.Inspect(t.Context())
	if err != nil || inspection.Processes["server"].Alive || inspection.Processes["worker"].Alive {
		t.Fatal("native credential mismatch did not fence both runtime children")
	}
	if os.WriteFile(path, original, 0o600) != nil {
		t.Fatal("native credential mismatch fixture could not restore the original credential pair")
	}
	restored = true
	if cell.Repair(t.Context()) != nil {
		t.Fatal("native credential mismatch left an unrecoverable pending start")
	}
}

// This child uses installer-produced identities only. No parallel role recipe
// or shared test database can manufacture a native installer receipt.
func testInstallerPrincipalsNative(t *testing.T, binaries localcell.Binaries) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	checks := 0
	completed := false
	// Registered before cells and connections: their cleanup and process-witness
	// assertions run first. A later cleanup failure must suppress this receipt.
	t.Cleanup(func() {
		if t.Failed() || !completed {
			return
		}
		artifact, err := syncharness.VerifyInstallerPrincipalRuntimeArtifact(t.Name(), checks)
		if err != nil {
			t.Fatal("native installer receipt refused completed groups")
		}
		data, err := json.Marshal(artifact)
		if err != nil {
			t.Fatal("native installer receipt encoding failed")
		}
		t.Log("installer principal receipt: " + string(data))
	})
	var held *pgx.Conn
	var root string
	cell, created := disposableCell(t, binaries, func(operation, step string) error {
		if operation == "install" && step == "principal_grants" {
			m := nativePrincipalManifest(t, root)
			held = nativePrincipalConnection(t, root, m, string(store.SyncPrincipalMigrator))
		}
		return nil
	})
	root = created
	installErr := cell.Install(t.Context(), localcell.InstallOptions{ManagedPrincipals: true, LeaveRunning: true})
	if held == nil {
		if installErr != nil && !strings.ContainsAny(installErr.Error(), `/\:`) {
			t.Fatalf("native install failed before the held-migrator fence: %s", installErr)
		}
		t.Fatal("native install failed before the held-migrator fence")
	}
	if installErr == nil {
		t.Fatal("native held migrator did not fence installation")
	}
	if !strings.HasSuffix(installErr.Error(), ": local_privileged_sessions_present") {
		t.Fatal("native held migrator failed before session observation")
	}
	m := nativePrincipalManifest(t, root)
	if m.Installed {
		t.Fatal("native undrained install was marked complete")
	}
	_ = held.Close(t.Context())
	for range 2 {
		if cell.Repair(t.Context()) != nil {
			t.Fatal("native fresh selected repair failed")
		}
	}
	checks++ // Held-session refusal and exact fresh repair.
	if cell.Start(t.Context()) != nil {
		t.Fatal("native selected start failed")
	}
	m = *assertReady(t, cell)
	if m.PrincipalLayoutVersion != 1 || len(m.PrincipalRoles) != 7 || m.Role != "localcell" {
		t.Fatal("native selected marker or principal inventory invalid")
	}
	checks++ // Selected install/start and retained bootstrap semantics.
	maintenance := nativePrincipalConnection(t, root, m, string(store.SyncPrincipalMaintenance))
	var runtimes int
	if maintenance.QueryRow(t.Context(), `SELECT count(DISTINCT usesysid) FROM pg_stat_activity WHERE usesysid IN ('witself_sync_server'::regrole,'witself_sync_worker'::regrole)`).Scan(&runtimes) != nil || runtimes != 2 {
		t.Fatal("native server and worker sessions not independently restricted")
	}
	checks++
	var owned bool
	if maintenance.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM pg_class WHERE relnamespace='public'::regnamespace AND relname=ANY($1::text[]) AND relowner<>'witself_sync_owner'::regrole) AND (SELECT nspowner='witself_sync_owner'::regrole FROM pg_namespace WHERE nspname='public')`, store.SyncWriterInventory()).Scan(&owned) != nil || !owned {
		t.Fatal("native independent covered ownership proof failed")
	}
	checks++
	st, err := store.Open(t.Context(), nativePrincipalDSN(t, root, m, string(store.SyncPrincipalMaintenance)))
	if err != nil {
		t.Fatal("native maintenance oracle open failed")
	}
	if st.VerifySyncPrincipals(t.Context()) != nil || st.VerifyManagedRuntimePrincipal(t.Context(), store.SyncPrincipalMaintenance) != nil {
		st.Close()
		t.Fatal("native C2a role and topology oracle failed")
	}
	st.Close()
	checks++
	var noLogin bool
	if maintenance.QueryRow(t.Context(), "SELECT NOT rolcanlogin FROM pg_roles WHERE rolname='witself_sync_owner'").Scan(&noLogin) != nil || !noLogin {
		t.Fatal("native owner was allowed to log in")
	}
	if _, err := os.Stat(filepath.Join(root, "run", "credentials", string(store.SyncPrincipalOwner))); !os.IsNotExist(err) {
		t.Fatal("native owner credential unexpectedly exists")
	}
	checks++
	for _, role := range []store.SyncPrincipalRole{store.SyncPrincipalServer, store.SyncPrincipalWorker, store.SyncPrincipalMaintenance} {
		conn := nativePrincipalConnection(t, root, m, string(role))
		for _, query := range []string{"ALTER TABLE accounts ADD COLUMN installer_denied boolean", "DELETE FROM sync_capture WHERE false", "SET session_replication_role=replica", "SET ROLE witself_sync_owner"} {
			_, err := conn.Exec(t.Context(), query)
			var denied *pgconn.PgError
			if !errors.As(err, &denied) || denied.Code != "42501" {
				t.Fatal("native runtime privilege denial failed")
			}
		}
		var version int
		if conn.QueryRow(t.Context(), "SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY id DESC LIMIT 1").Scan(&version) != nil || version != store.SchemaVersion() {
			t.Fatal("native runtime version grant failed")
		}
		_ = conn.Close(t.Context())
	}
	var capacity bool
	if maintenance.QueryRow(t.Context(), `SELECT has_table_privilege('witself_sync_server','agent_email_cell_storage_capacity','SELECT') AND has_column_privilege('witself_sync_server','agent_email_cell_storage_capacity','admission_bytes','UPDATE') AND NOT has_table_privilege('witself_sync_worker','agent_email_cell_storage_capacity','UPDATE')`).Scan(&capacity) != nil || !capacity {
		t.Fatal("native bounded capacity grant failed")
	}
	checks++
	pool, err := pgxpool.New(t.Context(), nativePrincipalDSN(t, root, m, string(store.SyncPrincipalMigrator)))
	if err != nil {
		t.Fatal("native migration pool open failed")
	}
	first, err := pool.Acquire(t.Context())
	if err != nil {
		pool.Close()
		t.Fatal("native migration first connection failed")
	}
	second, err := pool.Acquire(t.Context())
	if err != nil {
		first.Release()
		pool.Close()
		t.Fatal("native migration reopened connection failed")
	}
	for _, conn := range []*pgxpool.Conn{first, second} {
		var current, session string
		if conn.QueryRow(t.Context(), "SELECT current_user,session_user").Scan(&current, &session) != nil || current != string(store.SyncPrincipalOwner) || session != string(store.SyncPrincipalMigrator) {
			second.Release()
			first.Release()
			pool.Close()
			t.Fatal("native migration reopened connection lost owner custody")
		}
	}
	second.Release()
	first.Release()
	pool.Close()
	checks++
	host, err := os.Executable()
	if err != nil {
		t.Fatal("native service descriptor host unavailable")
	}
	if cell.PrepareService(t.Context(), localcell.ServiceDescriptor{Adapter: "foreground", Payload: localcell.ServicePayload{Host: host, Args: []string{"run", "--root", root}}}) != nil || cell.SetServiceDesiredState(t.Context(), "running") != nil || cell.ServiceHealth(t.Context()) != nil || cell.ServiceHealth(t.Context()) != nil {
		t.Fatal("native maintenance authenticated health failed")
	}
	checks++
	var drained bool
	if maintenance.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usesysid IN ('localcell'::regrole,'witself_sync_migrator'::regrole))`).Scan(&drained) != nil || !drained {
		t.Fatal("native privileged session drain failed")
	}
	_ = maintenance.Close(t.Context())
	checks++
	d, err := cell.ServiceDescriptor()
	if err != nil {
		t.Fatal("native descriptor unavailable")
	}
	descriptor, _ := json.Marshal(d)
	descriptors := [][]byte{descriptor}
	// Render both existing manager formats without registering or querying a
	// service. Their UID grammar is numeric, including when this runs on Windows.
	d.UID = "1000"
	d.Adapter = "systemd-user"
	unit, err := localcell.RenderSystemdUserUnit(d)
	if err != nil {
		t.Fatal("native principal systemd descriptor rendering failed")
	}
	descriptors = append(descriptors, unit)
	d.Adapter = "launchd-agent"
	plist, err := localcell.RenderLaunchdAgentPlist(d)
	if err != nil {
		t.Fatal("native principal launchd descriptor rendering failed")
	}
	descriptors = append(descriptors, plist)
	for _, role := range append(store.SyncPrincipalRoleNames()[1:], string(store.SyncPrincipalMigrator), "localcell") {
		parsed, _ := url.Parse(nativePrincipalDSN(t, root, m, role))
		secret, _ := parsed.User.Password()
		for _, data := range descriptors {
			if bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte(filepath.Join(root, "run", "credentials"))) || bytes.Contains(data, []byte("postgres.password")) {
				t.Fatal("native descriptor exposed principal material")
			}
		}
	}
	checks++
	nativePrincipalCredentialMismatch(t, cell, root)
	if cell.SetServiceDesiredState(t.Context(), "running") != nil || cell.Start(t.Context()) != nil {
		t.Fatal("native selected restart failed")
	}
	other, otherRoot := disposableCell(t, binaries, nil)
	if other.Install(t.Context(), localcell.InstallOptions{ManagedPrincipals: true}) != nil || other.Start(t.Context()) != nil {
		t.Fatal("native independent selected installation failed")
	}
	secondManifest := nativePrincipalManifest(t, otherRoot)
	if secondManifest.InstallationID == m.InstallationID || secondManifest.Ports.Postgres == m.Ports.Postgres || secondManifest.AccountID == m.AccountID {
		t.Fatal("native selected installations shared identity")
	}
	for _, entry := range []struct {
		cell *localcell.Cell
		root string
	}{{cell, root}, {other, otherRoot}} {
		if entry.cell.Stop(t.Context()) != nil || entry.cell.Remove(t.Context(), localcell.RemoveOptions{Purge: true}) != nil {
			t.Fatal("native selected cleanup failed")
		}
		assertStopped(t, entry.cell)
		if _, err := os.Stat(filepath.Join(entry.root, "pg")); !os.IsNotExist(err) {
			t.Fatal("native selected cleanup retained database")
		}
	}
	checks++
	completed = true
}
