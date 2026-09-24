package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/clientinventory"
	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/local"
)

const accountFakeBearer = "synthetic-manager-private-canary"

func accountTestBackend(t *testing.T) (agentConnection, *dashboard.AccountManager, *atomic.Bool) {
	t.Helper()
	revoked := new(atomic.Bool)
	identity := consoleTestIdentity()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/self":
			if r.Header.Get("Authorization") != "Bearer "+consoleFakeToken {
				t.Error("agent credential isolation")
				w.WriteHeader(401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"identity": identity})
		case "/v1/whoami":
			if r.Header.Get("Authorization") != "Bearer "+accountFakeBearer {
				t.Error("operator credential isolation")
				w.WriteHeader(401)
				return
			}
			if revoked.Load() {
				w.WriteHeader(403)
				return
			}
			_, _ = fmt.Fprintf(w, `{"schema_version":"witself.v0","principal":{"kind":"operator","account_id":%q,"operator_id":"op_synthetic","account_role":"account_owner"}}`, identity.AccountID)
		default:
			t.Error("unexpected upstream route")
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	conn := agentConnection{Endpoint: server.URL, Token: consoleFakeToken, AccountID: identity.AccountID, AccountName: "selected"}
	manager := &dashboard.AccountManager{Endpoint: server.URL, BearerToken: accountFakeBearer, Identity: dashboard.AccountManagerIdentity{AccountID: identity.AccountID, OperatorID: "op_synthetic", Role: "account_owner"}}
	return conn, manager, revoked
}

func TestAccountConsoleResolutionOrdering(t *testing.T) {
	consoleTestHome(t)
	conn, expected, revoked := accountTestBackend(t)
	if err := local.Save("selected", local.Account{ID: conn.AccountID}, accountFakeBearer); err != nil {
		t.Fatal(err)
	}
	if err := local.Save("default", local.Account{ID: "acc_other"}, "synthetic-other-owner"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITSELF_ACCOUNT", "default")
	old := accountConsoleResolve
	t.Cleanup(func() { accountConsoleResolve = old })
	reads, located := 0, 0
	accountConsoleResolve = func(name string) (string, local.Account, string, error) {
		reads++
		if name != "selected" {
			t.Fatal("ambient owner fallback")
		}
		return local.Resolve(name)
	}
	locate := func(ctx context.Context, directory, id string) (string, string, error) {
		located++
		if directory != defaultControlPlane || id != conn.AccountID || reads != 0 {
			t.Fatal("directory did not precede bearer read")
		}
		return "", conn.Endpoint + "/", nil
	}
	if got := resolveAccountConsoleManager(context.Background(), conn, consoleTestIdentity(), false, locate); got == nil || *got != *expected || reads != 1 || located != 1 {
		t.Fatal("managed selection failed")
	}
	for _, tc := range []string{"explicit", "missing-original", "cross-account", "substituted", "directory-refusal", "metadata-changed", "missing-credential", "revoked"} {
		t.Run(tc, func(t *testing.T) {
			reads, located = 0, 0
			c := conn
			explicit := false
			locator := accountLocator(locate)
			switch tc {
			case "explicit":
				explicit = true
			case "missing-original":
				c.AccountName = ""
			case "cross-account":
				c.AccountID = "acc_other"
			case "substituted":
				locator = func(context.Context, string, string) (string, string, error) {
					return "", "https://substituted.invalid", nil
				}
			case "directory-refusal":
				locator = func(context.Context, string, string) (string, string, error) {
					return "", "", errors.New("private upstream detail")
				}
			case "metadata-changed":
				accountConsoleResolve = func(string) (string, local.Account, string, error) {
					reads++
					return "selected", local.Account{ID: "acc_other"}, accountFakeBearer, nil
				}
			case "missing-credential":
				accountConsoleResolve = func(string) (string, local.Account, string, error) {
					reads++
					return "", local.Account{}, "", errors.New("private path")
				}
			case "revoked":
				revoked.Store(true)
			}
			defer func() {
				revoked.Store(false)
				accountConsoleResolve = func(name string) (string, local.Account, string, error) { reads++; return local.Resolve(name) }
			}()
			if got := resolveAccountConsoleManager(context.Background(), c, consoleTestIdentity(), explicit, locator); got != nil {
				t.Fatal("failure enabled manager")
			}
			if tc != "metadata-changed" && tc != "missing-credential" && tc != "revoked" && reads != 0 {
				t.Fatal("bearer read before trusted binding")
			}
		})
	}
	for _, args := range [][]string{{"--endpoint="}, {"--token-file="}, {"--endpoint=https://example.invalid"}} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		factConnectionFlags(fs)
		if fs.Parse(args) != nil || !accountConsoleExplicit(fs) {
			t.Fatal("explicit flag presence lost")
		}
	}
}

func TestAccountConsoleStrictIdentity(t *testing.T) {
	consoleTestHome(t)
	id := consoleTestIdentity()
	if err := local.Save("selected", local.Account{ID: id.AccountID}, accountFakeBearer); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{}`, `{"principal":{"kind":"operator"}}`,
		`{"schema_version":"witself.v0","principal":{"kind":"agent","account_id":"acc_synthetic","operator_id":"op_synthetic","account_role":"account_owner"}}`,
		`{"schema_version":"witself.v0","principal":{"kind":"operator","account_id":"acc_other","operator_id":"op_synthetic","account_role":"account_owner"}}`,
		`{"schema_version":"witself.v0","principal":{"kind":"operator","account_id":"acc_synthetic","operator_id":"","account_role":"account_owner"}}`,
		`{"schema_version":"witself.v0","principal":{"kind":"operator","account_id":"acc_synthetic","operator_id":"op_synthetic","account_role":"account_member"}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		conn := agentConnection{Endpoint: server.URL, Token: consoleFakeToken, AccountName: "selected", AccountID: id.AccountID}
		got := resolveAccountConsoleManager(context.Background(), conn, id, false, func(context.Context, string, string) (string, string, error) { return "", server.URL, nil })
		server.Close()
		if got != nil {
			t.Fatal("unknown identity enabled manager")
		}
	}
}

func TestAccountConsoleScannerExplicitAndCancellation(t *testing.T) {
	consoleTestHome(t)
	conn, manager, _ := accountTestBackend(t)
	roots := accountConsoleLocalRoots()
	previous := accountConsoleScan
	defer func() { accountConsoleScan = previous }()
	calls := 0
	accountConsoleScan = func(ctx context.Context, options clientinventory.Options) (clientinventory.Report, error) {
		calls++
		if options != (clientinventory.Options{Home: roots.Home, WitselfHome: roots.WitselfHome, DSHHome: roots.DSHHome, AccountID: conn.AccountID}) {
			t.Error("scanner roots or account changed")
		}
		return clientinventory.Report{SchemaVersion: clientinventory.SchemaVersion, DeviceLabel: "This device", CheckedAt: time.Now(), ScanStatus: clientinventory.ScanPartial, Entries: []clientinventory.Entry{{Runtime: clientinventory.RuntimeCodex, ExecutableStatus: clientinventory.ExecutableUnchecked, ConfigurationStatus: clientinventory.ConfigurationUnsupported, ConfigurationScope: clientinventory.ScopeNone, EffectiveVerification: clientinventory.EffectiveNotRun}}}, nil
	}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	scanner := newAccountConsoleScanner(parent, roots, manager)
	reader, err := dashboard.NewReader(dashboard.Config{Endpoint: conn.Endpoint, BearerToken: conn.Token, Identity: consoleTestIdentity(), AccountManager: manager, AccountClientsScan: scanner})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := reader.Read(parent, dashboard.ReadRequest{Resource: dashboard.ResourceAccountClients}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatal("passive scan")
	}
	raw, err := reader.Read(parent, dashboard.ReadRequest{Resource: dashboard.ResourceAccountClientsScan})
	if err != nil || calls != 1 || !strings.Contains(string(raw), `"scan_status":"partial"`) || !strings.Contains(string(raw), `"configuration_scope":"none"`) {
		t.Fatal("explicit scan translation failed")
	}
	if _, err := scanner(parent, "acc_other"); err == nil || calls != 1 {
		t.Fatal("cross-account scan")
	}
	started := make(chan struct{})
	accountConsoleScan = func(ctx context.Context, _ clientinventory.Options) (clientinventory.Report, error) {
		close(started)
		<-ctx.Done()
		return clientinventory.Report{}, ctx.Err()
	}
	scanner = newAccountConsoleScanner(parent, roots, manager)
	done := make(chan error, 1)
	go func() { _, err := scanner(context.Background(), conn.AccountID); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation lost")
		}
	case <-time.After(time.Second):
		t.Fatal("parent did not cancel scan")
	}
}

func TestAccountConsoleChildManagerValidation(t *testing.T) {
	consoleTestHome(t)
	conn, manager, revoked := accountTestBackend(t)
	good := tuiConsoleBootstrap{Connection: conn, Identity: consoleTestIdentity(), Manager: consoleManagerBootstrap(manager), ScannerRoots: accountConsoleLocalRoots()}
	raw, _ := json.Marshal(good)
	if !strings.Contains(string(raw), accountFakeBearer) {
		t.Fatal("pipe DTO lost bearer")
	}
	generic, _ := json.Marshal(manager)
	if strings.Contains(string(generic), accountFakeBearer) {
		t.Fatal("production manager serialized bearer")
	}
	for _, name := range []string{"valid", "operator", "role", "account", "origin", "roots", "roots-relative", "roots-unclean", "roots-control", "roots-oversized", "empty-bearer", "revoked"} {
		t.Run(name, func(t *testing.T) {
			b := good
			m := *good.Manager
			b.Manager = &m
			switch name {
			case "operator":
				m.Binding.OperatorID = "op_other"
			case "role":
				m.Binding.Role = "account_member"
			case "account":
				m.Binding.AccountID = "acc_other"
			case "origin":
				m.Endpoint = "https://substituted.invalid"
			case "roots":
				b.ScannerRoots = accountConsoleRoots{}
			case "roots-relative":
				b.ScannerRoots.DSHHome = "~/.dsh"
			case "roots-unclean":
				b.ScannerRoots.WitselfHome += "/"
			case "roots-control":
				b.ScannerRoots.Home += "\nprivate-root"
			case "roots-oversized":
				b.ScannerRoots.Home += strings.Repeat("x", 4096)
			case "empty-bearer":
				m.Bearer = ""
			case "revoked":
				revoked.Store(true)
				defer revoked.Store(false)
			}
			got, err := verifyConsoleChildManager(context.Background(), b)
			if name == "valid" {
				if err != nil || got == nil || *got != *manager {
					t.Fatal("valid private bootstrap rejected")
				}
			} else if err == nil || got != nil {
				t.Fatal("invalid claim downgraded or accepted")
			}
		})
	}
}

func TestAccountConsoleStatusStopRedaction(t *testing.T) {
	consoleTestHome(t)
	live, stale := seedDashboardRegistry(t)
	binding := &dashboard.ViewerBinding{AccountID: "acc_synthetic", OperatorID: "op_synthetic", Role: "account_owner"}
	for _, entry := range []dashboard.RegistryEntry{live, stale} {
		entry.Manager = binding
		entry.ViewerContract = dashboard.ViewerSchema
		if dashboard.WriteRegistryEntry(entry) != nil {
			t.Fatal("write registry")
		}
	}
	old := signalDashboard
	signalDashboard = func(dashboard.RegistryEntry) error { return errors.New("synthetic stop refusal") }
	defer func() { signalDashboard = old }()
	for _, args := range [][]string{{"status"}, {"status", "--json"}, {"stop", "--json"}} {
		out, errout, _ := captureFactDeleteCLI(t, func() int { return dashboardCmd(args) })
		for _, secret := range []string{live.AccessURL, stale.AccessURL, accountFakeBearer} {
			if secret != "" && strings.Contains(out+errout, secret) {
				t.Fatal("private opening URL disclosed")
			}
		}
	}
	current, err := dashboard.ReadRegistryEntry(live.AgentID)
	if err != nil || current.AccessURL != live.AccessURL {
		t.Fatal("output redaction changed private registry")
	}
}

func TestAccountConsoleSubstitutedEndpointGetsNoOperatorRequest(t *testing.T) {
	consoleTestHome(t)
	conn, _, _ := accountTestBackend(t)
	if err := local.Save("selected", local.Account{ID: conn.AccountID}, accountFakeBearer); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	substituted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer substituted.Close()
	trusted := conn.Endpoint
	conn.Endpoint = substituted.URL
	old := accountConsoleResolve
	defer func() { accountConsoleResolve = old }()
	accountConsoleResolve = func(string) (string, local.Account, string, error) {
		t.Fatal("bearer lookup before origin equality")
		return "", local.Account{}, "", nil
	}
	manager := resolveAccountConsoleManager(context.Background(), conn, consoleTestIdentity(), false, func(context.Context, string, string) (string, string, error) { return "", trusted, nil })
	if manager != nil || requests.Load() != 0 {
		t.Fatal("substituted endpoint received operator authority")
	}
}

func TestAccountConsoleRootsFailClosed(t *testing.T) {
	for _, name := range []string{"missing-home", "home-control", "witself-control", "dsh-control", "oversized", "unresolvable-relative"} {
		t.Run(name, func(t *testing.T) {
			consoleTestHome(t)
			switch name {
			case "missing-home":
				t.Setenv("HOME", "")
				t.Setenv("USERPROFILE", "")
			case "home-control":
				t.Setenv("HOME", "/synthetic\ninvalid")
				t.Setenv("USERPROFILE", "/synthetic\ninvalid")
			case "witself-control":
				t.Setenv("WITSELF_HOME", "/synthetic\rinvalid")
			case "dsh-control":
				t.Setenv("DSH_HOME", "/synthetic\ninvalid")
			case "oversized":
				t.Setenv("WITSELF_HOME", "/"+strings.Repeat("x", 4096))
			case "unresolvable-relative":
				cwd := t.TempDir()
				t.Chdir(cwd)
				t.Setenv("PWD", "")
				if err := os.Remove(cwd); err != nil {
					t.Fatal("cannot remove synthetic cwd")
				}
				if _, err := os.Getwd(); err == nil {
					t.Skip("platform still resolves a removed cwd; no unresolved-root condition")
				}
				t.Setenv("WITSELF_HOME", "relative-state")
			}
			roots := accountConsoleLocalRoots()
			manager := &dashboard.AccountManager{Identity: dashboard.AccountManagerIdentity{AccountID: "acc_synthetic"}}
			if roots.valid() || newAccountConsoleScanner(context.Background(), roots, manager) != nil {
				t.Fatal("invalid roots acquired a fallback scanner")
			}
		})
	}
	if accountConsoleAbsoluteRoot("/synthetic\x00invalid") != "" {
		t.Fatal("NUL root accepted")
	}
}

func TestAccountConsoleRootsStayLexical(t *testing.T) {
	consoleTestHome(t)
	base := t.TempDir()
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(base, "missing-target"), link); err != nil {
		t.Fatal("cannot create synthetic symlink")
	}
	t.Setenv("HOME", link)
	t.Setenv("USERPROFILE", link)
	t.Setenv("WITSELF_HOME", "")
	t.Setenv("DSH_HOME", "~/.dsh/")
	want := accountConsoleRoots{link, filepath.Join(link, ".witself"), filepath.Join(link, ".dsh")}
	if got := accountConsoleLocalRoots(); got != want || !got.valid() {
		t.Fatal("root capture resolved or required a filesystem target")
	}
}
