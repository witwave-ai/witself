package localcell

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/witwave-ai/witself/internal/store"
)

func selectedCredentialFixture(t *testing.T) (*Cell, *Manifest, map[credentialPurpose]string) {
	t.Helper()
	c, m := unitCell(t)
	m.PrincipalLayoutVersion = 1
	m.PrincipalRoles = append(store.SyncPrincipalRoleNames(), installerMigratorRole)
	if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
		t.Fatal("credential fixture could not publish selected manifest")
	}
	if err := privateDir(c.path("run", "credentials"), true); err != nil {
		t.Fatal("credential fixture could not create private directory")
	}
	values := make(map[credentialPurpose]string)
	for _, purpose := range append([]credentialPurpose{credentialBootstrap}, principalCredentialPurposes[:]...) {
		value := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{byte(purpose) + 13}, 32))
		values[purpose] = value
		path, err := c.credentialPath(purpose)
		if err != nil || writeAtomicBytes(path, []byte(value+"\n"), nil) != nil {
			t.Fatal("credential fixture could not publish private synthetic value")
		}
	}
	return c, m, values
}

func TestInstallerPrincipalsChildIsolation(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	c, m, values := selectedCredentialFixture(t)
	for _, variable := range []string{"WITSELF_LOCAL_CAPTURE_PROOF", "WITSELF_LOCAL_MIGRATION_PHASE", "WITSELF_LOCAL_SHUTDOWN_MODE", "WITSELF_LOCAL_MANAGED_PRINCIPALS", "WITSELF_LOCAL_SYNC_SCHEMA", "PGPASSWORD"} {
		t.Setenv(variable, "synthetic-ambient-selector")
	}
	for _, test := range []struct {
		component string
		purpose   credentialPurpose
	}{{"server", credentialServer}, {"worker", credentialWorker}} {
		t.Run(test.component, func(t *testing.T) {
			environment, err := c.childEnvironment(m, test.component)
			if err != nil {
				t.Fatal("child credential mapping unexpectedly refused selected fixture")
			}
			configuration := make(map[string]string)
			for _, entry := range environment {
				name, value, ok := strings.Cut(entry, "=")
				if !ok || configuration[name] != "" {
					t.Fatal("child credential mapping contains malformed or duplicate entry")
				}
				configuration[name] = value
			}
			dsn, err := url.Parse(configuration["WITSELF_DATABASE_URL"])
			if err != nil || dsn.User == nil {
				t.Fatal("child credential mapping did not carry a parseable private connection")
			}
			role, _ := credentialRole(test.purpose)
			password, supplied := dsn.User.Password()
			if dsn.User.Username() != role || !supplied || password != values[test.purpose] {
				t.Fatal("child credential mapping must contain only its own role and secret")
			}
			if dsn.Query().Get("options") != "" || configuration["WITSELF_LOCAL_MANAGED_PRINCIPALS"] != "1" || configuration["WITSELF_LOCAL_SYNC_SCHEMA"] != "1" {
				t.Fatal("child credential mapping selected unsafe or incomplete runtime options")
			}
			joined := strings.Join(environment, "\n")
			for purpose, secret := range values {
				path, _ := c.credentialPath(purpose)
				if strings.Contains(joined, path) || (purpose != test.purpose && strings.Contains(joined, secret)) {
					t.Fatal("child credential mapping disclosed sibling or privileged material")
				}
			}
			if strings.Contains(joined, "synthetic-ambient-selector") || configuration["WITSELF_LOCAL_MIGRATION_PHASE"] != "" {
				t.Fatal("child credential mapping inherited ambient selected configuration")
			}
			assertInstallerChildArguments(t, c, m, test.component)
		})
	}
	if values[credentialWorker] == values[credentialServer] {
		t.Fatal("child credential fixture requires distinguishable independent secrets")
	}
	if _, err := c.childEnvironment(m, "snapshot"); err == nil {
		t.Fatal("child credential mapping accepted an unwired component")
	}
	if err := c.startChildPrepared(t.Context(), m, "migration", nil); err == nil {
		t.Fatal("migration credential escaped into the daemon launcher")
	}
	for _, descriptor := range installerCredentialDescriptors(t, c, m) {
		assertNoPrincipalMaterial(t, descriptor, c, values)
	}
	for _, record := range []any{m, journalEntry{Operation: "install", Step: "credentials", State: "done"}, Status{Inspection: Inspection{Manifest: m}}} {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal("credential fixture could not encode value-free lifecycle metadata")
		}
		assertNoPrincipalMaterial(t, encoded, c, values)
	}
}

func installerCredentialDescriptors(t *testing.T, c *Cell, m *Manifest) [][]byte {
	t.Helper()
	host, err := os.Executable()
	if err != nil {
		t.Fatal("credential descriptor fixture could not resolve test executable")
	}
	t.Setenv("WITSELF_RUNTIME", "local")
	if c.PrepareService(t.Context(), ServiceDescriptor{Adapter: "foreground", Payload: ServicePayload{Host: host, Args: []string{"run", "--root", c.cfg.Root}}}) != nil {
		t.Fatal("credential descriptor fixture could not prepare selected service intent")
	}
	d, err := c.ServiceDescriptor()
	if err != nil || d.InstallationID != m.InstallationID {
		t.Fatal("credential descriptor fixture lost its selected installation identity")
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		t.Fatal("credential descriptor fixture could not render foreground descriptor")
	}
	result := [][]byte{encoded}
	// Unix renderer fixtures use their numeric UID contract on every platform;
	// the persisted foreground descriptor above retains the actual native owner.
	d.UID = "1000"
	d.Adapter = "systemd-user"
	unit, err := RenderSystemdUserUnit(d)
	if err != nil {
		t.Fatal("credential descriptor fixture could not render systemd descriptor")
	}
	result = append(result, unit)
	d.Adapter = "launchd-agent"
	plist, err := RenderLaunchdAgentPlist(d)
	if err != nil {
		t.Fatal("credential descriptor fixture could not render launchd descriptor")
	}
	return append(result, plist)
}

func assertNoPrincipalMaterial(t *testing.T, data []byte, c *Cell, values map[credentialPurpose]string) {
	t.Helper()
	for purpose, secret := range values {
		path, _ := c.credentialPath(purpose)
		if bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte(path)) {
			t.Fatal("credential exclusion failed for lifecycle or service descriptor")
		}
	}
	if bytes.Contains(data, []byte("postgres://")) {
		t.Fatal("credential exclusion failed for connection URL in public metadata")
	}
}

func TestInstallerPrincipalCredentialPublication(t *testing.T) {
	t.Run("private-temporaries-and-repeat", func(t *testing.T) {
		c, m := unitCell(t)
		m.PrincipalLayoutVersion = 1
		m.PrincipalRoles = append(store.SyncPrincipalRoleNames(), installerMigratorRole)
		opened := 0
		before := beforePasswordWrite
		beforePasswordWrite = func(f *os.File) {
			opened++
			info, err := f.Stat()
			if err != nil || info.Size() != 0 || !privatePassword(f.Name(), info) {
				t.Fatal("credential temporary was not private before its first write")
			}
		}
		t.Cleanup(func() { beforePasswordWrite = before })
		if c.makePrincipalPasswords(m) != nil || opened != len(principalCredentialPurposes) {
			t.Fatal("credential publisher did not observe every opened role temporary")
		}
		first := make(map[credentialPurpose]string)
		for _, purpose := range principalCredentialPurposes {
			value, err := c.credentialPassword(m, purpose)
			if err != nil {
				t.Fatal("credential publisher created an unreadable role value")
			}
			first[purpose] = value
		}
		if c.makePrincipalPasswords(m) != nil || opened != len(principalCredentialPurposes) {
			t.Fatal("credential publisher rotated an already complete credential set")
		}
		for purpose, value := range first {
			again, err := c.credentialPassword(m, purpose)
			if err != nil || again != value {
				t.Fatal("credential publisher changed a durable credential pair on retry")
			}
		}
		if _, err := os.Lstat(c.path("run", "credentials", string(store.SyncPrincipalOwner))); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("credential publisher created a password for the non-login owner")
		}
	})
	defects := []string{"missing", "directory", "symlink", "encoding", "unknown-layout", "unknown-purpose", "duplicate-secret", "owner-file"}
	if runtime.GOOS != "windows" {
		defects = append(defects, "mode", "hardlink")
	}
	for _, defect := range defects {
		t.Run(defect, func(t *testing.T) {
			c, m, values := selectedCredentialFixture(t)
			path, _ := c.credentialPath(credentialServer)
			foreign := filepath.Join(t.TempDir(), "foreign")
			if err := os.WriteFile(foreign, []byte("unchanged-foreign-object"), 0o600); err != nil {
				t.Fatal("credential refusal fixture could not create foreign sentinel")
			}
			switch defect {
			case "missing", "directory", "symlink", "hardlink":
				if removePrivateFile(path) != nil {
					t.Fatal("credential refusal fixture could not remove selected file")
				}
			}
			switch defect {
			case "mode":
				if os.Chmod(path, 0o644) != nil {
					t.Fatal("credential refusal fixture could not change Unix file mode")
				}
			case "directory":
				if makePrivateDir(path) != nil {
					t.Fatal("credential refusal fixture could not replace file with directory")
				}
			case "symlink":
				if os.Symlink(foreign, path) != nil {
					if runtime.GOOS == "windows" {
						t.Skip("LC-S2-PW3: Windows symlink fixture privileges unavailable")
					}
					t.Fatal("credential refusal fixture could not install a symlink")
				}
			case "hardlink":
				if os.Link(foreign, path) != nil {
					t.Fatal("credential refusal fixture could not install hardlink")
				}
			case "encoding":
				if writeAtomicBytes(path, []byte("invalid-value\n"), nil) != nil {
					t.Fatal("credential refusal fixture could not install malformed value")
				}
			case "unknown-layout":
				m.PrincipalLayoutVersion++
			case "duplicate-secret":
				if writeAtomicBytes(path, []byte(values[credentialWorker]+"\n"), nil) != nil {
					t.Fatal("credential refusal fixture could not install repeated value")
				}
			case "owner-file":
				if writeAtomicBytes(c.path("run", "credentials", string(store.SyncPrincipalOwner)), []byte(values[credentialServer]+"\n"), nil) != nil {
					t.Fatal("credential refusal fixture could not install unexpected owner file")
				}
			}
			var err error
			switch defect {
			case "unknown-purpose":
				_, err = c.credentialPassword(m, credentialPurpose(250))
			case "duplicate-secret", "owner-file":
				err = c.makePrincipalPasswords(m)
			default:
				_, err = c.childEnvironment(m, "server")
			}
			if err == nil {
				t.Fatal("credential refusal failed or fell back to bootstrap material")
			}
			assertNoPrincipalMaterial(t, []byte(err.Error()), c, values)
			got, err := os.ReadFile(foreign)
			if err != nil || string(got) != "unchanged-foreign-object" {
				t.Fatal("credential refusal changed unrelated foreign bytes")
			}
		})
	}
	t.Run("interrupted-publication-remains-fenced", func(t *testing.T) {
		c, m := unitCell(t)
		m.PrincipalLayoutVersion = 1
		m.PrincipalRoles = append(store.SyncPrincipalRoleNames(), installerMigratorRole)
		oldSync := syncFile
		calls := 0
		syncFile = func(f *os.File) error {
			calls++
			if calls == 2 {
				return errors.New("synthetic interrupted publication")
			}
			return f.Sync()
		}
		t.Cleanup(func() { syncFile = oldSync })
		if c.makePrincipalPasswords(m) == nil {
			t.Fatal("credential publication ignored the interrupted atomic write")
		}
		syncFile = oldSync
		before, err := c.credentialPassword(m, credentialServer)
		if err != nil {
			t.Fatal("credential publication lost the first durable credential")
		}
		if c.makePrincipalPasswords(m) == nil {
			t.Fatal("credential publication silently filled an ambiguous partial set")
		}
		if _, err := c.childEnvironment(m, "server"); err == nil {
			t.Fatal("credential publication allowed a partial set to reach a runtime child")
		}
		after, err := c.credentialPassword(m, credentialServer)
		if err != nil || before != after {
			t.Fatal("credential publication rotated the surviving database/file pair")
		}
	})
	ownerCase := "wrong-owner"
	if runtime.GOOS == "windows" {
		ownerCase = "windows-private-dacl-owner"
	}
	t.Run(ownerCase, func(t *testing.T) {
		c, _, _ := selectedCredentialFixture(t)
		path, _ := c.credentialPath(credentialServer)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal("credential owner fixture could not inspect selected file")
		}
		if runtime.GOOS == "windows" {
			if !privatePrincipalCredential(path, info) || !privatePassword(path, info) {
				t.Fatal("credential Windows DACL and owner policy rejected private publication")
			}
			return
		}
		if privatePrincipalCredential(path, principalCredentialOwnerInfo{FileInfo: info, uid: uint64(os.Geteuid()) + 1}) {
			t.Fatal("credential owner predicate accepted a foreign file owner")
		}
	})
}

type principalCredentialOwnerInfo struct {
	os.FileInfo
	uid uint64
}

func (i principalCredentialOwnerInfo) Sys() any {
	return &struct{ Uid, Nlink uint64 }{Uid: i.uid, Nlink: 1} //nolint:revive // Reflects the operating system Stat_t field names.
}

func TestInstallerPrincipalsMigrationCredential(t *testing.T) {
	c, m, values := selectedCredentialFixture(t)
	environment, err := c.childEnvironment(m, "migration")
	if err != nil {
		t.Fatal("migration credential mapping refused valid selected fixture")
	}
	var raw string
	for _, entry := range environment {
		if value, ok := strings.CutPrefix(entry, "WITSELF_DATABASE_URL="); ok {
			raw = value
		}
	}
	dsn, err := url.Parse(raw)
	if err != nil || dsn.User == nil {
		t.Fatal("migration credential mapping did not provide its private connection")
	}
	password, _ := dsn.User.Password()
	if dsn.User.Username() != installerMigratorRole || password != values[credentialMigrator] ||
		dsn.Query().Get("options") != "-c role="+string(store.SyncPrincipalOwner) ||
		!slices.Contains(environment, "WITSELF_LOCAL_MIGRATION_PHASE=1") {
		t.Fatal("migration credential mapping lost login identity or physical-connection owner selection")
	}
}

type installerHealthStore struct {
	ensure, issue, revoke int
	guarded               bool
	account               string
}

func (s *installerHealthStore) EnsureRootOperator(_ context.Context, account string) (string, error) {
	s.ensure++
	s.account = account
	if s.guarded {
		return "", errors.New("synthetic capture guard refusal")
	}
	return "opr_health_fixture", nil
}

func (s *installerHealthStore) CreateOperatorToken(_ context.Context, account, operator, name string, ttl *time.Duration) (string, string, *time.Time, error) {
	s.issue++
	if account != s.account || operator != "opr_health_fixture" || name != "local service health" || ttl != nil {
		return "", "", nil, errors.New("synthetic operator contract refusal")
	}
	return "synthetic-private-health-bearer", "tok_health_fixture", nil, nil
}

func (s *installerHealthStore) RevokeToken(_ context.Context, account, operator, token string) error {
	if account != s.account || operator != "opr_health_fixture" || token != "tok_health_fixture" {
		return errors.New("synthetic operator revocation refusal")
	}
	s.revoke++
	return nil
}

func TestInstallerPrincipalsServiceHealthIdentity(t *testing.T) {
	t.Run("maintenance-and-legacy-custody", func(t *testing.T) {
		c, m, values := selectedCredentialFixture(t)
		raw, err := c.healthDSN(m)
		if err != nil {
			t.Fatal("health identity selector refused selected maintenance fixture")
		}
		dsn, err := url.Parse(raw)
		if err != nil || dsn.User == nil {
			t.Fatal("health identity selector did not supply a private connection")
		}
		password, _ := dsn.User.Password()
		if dsn.User.Username() != string(store.SyncPrincipalMaintenance) || password != values[credentialMaintenance] || dsn.Query().Get("options") != "" {
			t.Fatal("health identity selector must open maintenance only")
		}
		path, _ := c.credentialPath(credentialMaintenance)
		if removePrivateFile(path) != nil {
			t.Fatal("health identity fixture could not remove maintenance credential")
		}
		if _, err := c.healthDSN(m); err == nil {
			t.Fatal("health identity selector retried a missing maintenance credential with bootstrap")
		}
		m.PrincipalLayoutVersion, m.PrincipalRoles = 0, nil
		raw, err = c.healthDSN(m)
		if err != nil {
			t.Fatal("health identity selector changed legacy bootstrap behavior")
		}
		dsn, err = url.Parse(raw)
		if err != nil || dsn.User == nil || dsn.User.Username() != m.Role {
			t.Fatal("health identity selector changed the legacy role")
		}
	})
	for _, mode := range []string{"create-reuse-authenticate", "guarded-creation", "private-publication-failure"} {
		t.Run(mode, func(t *testing.T) {
			c, m, _ := selectedCredentialFixture(t)
			m.AccountID = "acc_health_fixture"
			if privateDir(c.path("service"), true) != nil {
				t.Fatal("health credential fixture could not create private service directory")
			}
			st := &installerHealthStore{guarded: mode == "guarded-creation"}
			if mode == "private-publication-failure" {
				before := syncFile
				syncFile = func(*os.File) error { return errors.New("synthetic publication interruption") }
				t.Cleanup(func() { syncFile = before })
			}
			credential, err := c.serviceAPICredential(t.Context(), st, m)
			switch mode {
			case "guarded-creation":
				if err == nil || st.ensure != 1 || st.issue != 0 || st.revoke != 0 {
					t.Fatal("guarded health credential creation retried elevated writes")
				}
			case "private-publication-failure":
				if err == nil || st.ensure != 1 || st.issue != 1 || st.revoke != 1 || credential.Bearer != "" {
					t.Fatal("health credential publication failure did not revoke the issued bearer")
				}
			case "create-reuse-authenticate":
				if err != nil || st.ensure != 1 || st.issue != 1 || st.revoke != 0 || credential.OperatorID != "opr_health_fixture" {
					t.Fatal("health credential creation changed the existing operator bearer contract")
				}
				second, err := c.serviceAPICredential(t.Context(), st, m)
				if err != nil || second != credential || st.ensure != 1 || st.issue != 1 {
					t.Fatal("health credential reuse performed new guarded writes")
				}
				requests := 0
				api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/v1/auth/whoami" || r.URL.Query().Get("challenge") == "" || r.Header.Get("Authorization") != "Bearer "+credential.Bearer || r.Header.Get("Cache-Control") != "no-store" {
						t.Error("health authenticated probe omitted existing identity verification contract")
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					requests++
					_, _ = w.Write([]byte(`{"principal":{"kind":"operator","account_id":"acc_health_fixture","operator_id":"opr_health_fixture"}}`))
				}))
				t.Cleanup(api.Close)
				if serviceAuthenticatedProbe(t.Context(), api.Client(), api.URL, credential) != nil || requests != 1 {
					t.Fatal("health authenticated probe refused a matching operator identity")
				}
			}
		})
	}
}

type installerCredentialLoggingFixture struct {
	queries  []string
	failAt   int
	cancelAt int
	cancel   context.CancelFunc
}

func (f *installerCredentialLoggingFixture) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if len(args) != 0 {
		return pgconn.CommandTag{}, errors.New("unexpected credential logging arguments")
	}
	f.queries = append(f.queries, query)
	if len(f.queries) == f.cancelAt && f.cancel != nil {
		f.cancel()
	}
	if len(f.queries) == f.failAt {
		return pgconn.CommandTag{}, errors.New("credential logging setting refused")
	}
	return pgconn.NewCommandTag("SET"), nil
}

func TestInstallerPrincipalCredentialLogging(t *testing.T) {
	// Independent required PostgreSQL policy. No credential value is needed to
	// prove that every sensitive action is behind the transaction-local fence.
	settings := []string{
		"SET LOCAL log_statement = 'none'",
		"SET LOCAL log_min_error_statement = 'panic'",
		"SET LOCAL log_min_messages = 'panic'",
		"SET LOCAL log_duration = off",
		"SET LOCAL log_min_duration_statement = -1",
		"SET LOCAL log_min_duration_sample = -1",
		"SET LOCAL log_transaction_sample_rate = 0",
		"SET LOCAL log_parameter_max_length = 0",
		"SET LOCAL log_parameter_max_length_on_error = 0",
	}
	t.Run("EverySettingBeforeAction", func(t *testing.T) {
		for range 2 {
			tx := &installerCredentialLoggingFixture{}
			actions := 0
			err := withInstallerCredentialLogging(t.Context(), tx, func() error {
				actions++
				if !slices.Equal(tx.queries, settings) {
					t.Fatal("credential statement action ran before all logging suppression")
				}
				return nil
			})
			if err != nil || actions != 1 || !slices.Equal(tx.queries, settings) {
				t.Fatal("credential logging transaction did not establish complete local policy")
			}
		}
	})
	t.Run("SettingFailure", func(t *testing.T) {
		for failAt := 1; failAt <= len(settings); failAt++ {
			tx := &installerCredentialLoggingFixture{failAt: failAt}
			actions := 0
			err := withInstallerCredentialLogging(t.Context(), tx, func() error { actions++; return nil })
			if !errors.Is(err, errPrincipalProvision) || actions != 0 || !slices.Equal(tx.queries, settings[:failAt]) {
				t.Fatal("failed credential logging setting reached a sensitive action")
			}
		}
	})
	t.Run("Cancellation", func(t *testing.T) {
		for cancelAt := 0; cancelAt <= len(settings); cancelAt++ {
			ctx, cancel := context.WithCancel(t.Context())
			tx := &installerCredentialLoggingFixture{cancelAt: cancelAt, cancel: cancel}
			if cancelAt == 0 {
				cancel()
			}
			actions := 0
			err := withInstallerCredentialLogging(ctx, tx, func() error { actions++; return nil })
			cancel()
			if !errors.Is(err, errPrincipalProvision) || actions != 0 || !slices.Equal(tx.queries, settings[:cancelAt]) {
				t.Fatal("cancelled credential logging fence reached a sensitive action")
			}
		}
	})
	t.Run("ActionFailure", func(t *testing.T) {
		tx := &installerCredentialLoggingFixture{}
		err := withInstallerCredentialLogging(t.Context(), tx, func() error { return errPrincipalProvision })
		if !errors.Is(err, errPrincipalProvision) || !slices.Equal(tx.queries, settings) {
			t.Fatal("credential action error escaped the complete logging policy")
		}
	})
}
