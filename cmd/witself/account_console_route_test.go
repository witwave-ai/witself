package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agenttui"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/local"
)

func accountConsoleRouteCell(t *testing.T, kind, accountID, role string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	requests := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/self" && r.Header.Get("Authorization") == "Bearer "+consoleFakeToken {
			_ = json.NewEncoder(w).Encode(map[string]any{"identity": consoleTestIdentity()})
			return
		}
		requests.Add(1)
		if r.URL.Path != "/v1/whoami" || r.Header.Get("Authorization") != "Bearer "+accountFakeBearer {
			t.Error("unexpected operator request or credential")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"principal":      map[string]string{"kind": kind, "account_id": accountID, "operator_id": "op_route_verified", "account_role": role},
		})
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func accountConsoleRouteSave(t *testing.T, name string, route *local.Route, bearer string) {
	t.Helper()
	if err := local.Save(name, local.Account{ID: consoleTestIdentity().AccountID, Route: route}, bearer); err != nil {
		t.Fatal("save synthetic account failed")
	}
	path, err := local.AgentTokenPath(name, "synthetic-realm", "synthetic")
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil || os.WriteFile(path, []byte(consoleFakeToken), 0o600) != nil {
		t.Fatal("save synthetic agent credential failed")
	}
}

func accountConsoleRouteBlackhole(calls *int) accountLocator {
	return func(context.Context, string, string) (string, string, error) {
		*calls++
		return "", "", errors.New("synthetic directory unavailable")
	}
}

func accountConsoleRouteCountReads(t *testing.T) *int {
	t.Helper()
	previous := accountConsoleResolve
	reads := new(int)
	accountConsoleResolve = func(name string) (string, local.Account, string, error) {
		*reads++
		return local.Resolve(name)
	}
	t.Cleanup(func() { accountConsoleResolve = previous })
	return reads
}

func TestAccountConsoleRouteBindingAndDefaultSelectors(t *testing.T) {
	for _, selection := range []struct {
		name, saved, explicit, ambient string
	}{
		{"named", "selected", "selected", "default"},
		{"default-explicit", "default", "default", "other"},
		{"default-implicit", "default", "", ""},
		{"default-environment", "default", "", "default"},
	} {
		for _, wrongOrigin := range []bool{false, true} {
			name := selection.name + "/bound"
			if wrongOrigin {
				name = selection.name + "/wrong-origin"
			}
			t.Run(name, func(t *testing.T) {
				consoleTestHome(t)
				t.Setenv("WITSELF_ACCOUNT", selection.ambient)
				server, requests := accountConsoleRouteCell(t, "operator", consoleTestIdentity().AccountID, "account_admin")
				accountConsoleRouteSave(t, selection.saved, &local.Route{Kind: "local", Endpoint: server.URL + "/"}, accountFakeBearer)
				reads := accountConsoleRouteCountReads(t)
				calls := 0
				locate := accountConsoleRouteBlackhole(&calls)
				conn, err := connectAgentWithLocator(context.Background(), selection.explicit, "synthetic-realm", "synthetic", "", "", locate)
				if err != nil || conn.AccountName != selection.saved || conn.AccountID != consoleTestIdentity().AccountID {
					t.Fatal("routed selector did not resolve its account")
				}
				if wrongOrigin {
					conn.Endpoint = "https://hosted.example.invalid"
				}
				manager := resolveAccountConsoleManager(context.Background(), conn, consoleTestIdentity(), false, locate)
				if calls != 0 {
					t.Fatalf("routed console consulted directory %d times", calls)
				}
				if wrongOrigin {
					if manager != nil || *reads != 0 || requests.Load() != 0 {
						t.Fatal("wrong-origin default or named account acquired manager authority")
					}
					return
				}
				want := client.AccountConsoleIdentity{AccountID: consoleTestIdentity().AccountID, OperatorID: "op_route_verified", Role: "account_admin"}
				if manager == nil || manager.Endpoint != server.URL || manager.BearerToken != accountFakeBearer || manager.Identity != want || *reads != 1 || requests.Load() != 1 {
					t.Fatal("routed console did not bind the normalized origin and verified operator")
				}
			})
		}
	}
}

func TestAccountConsoleRouteCredentialDenials(t *testing.T) {
	for _, name := range []string{"missing", "agent-bearer", "agent-principal", "unknown-role"} {
		t.Run(name, func(t *testing.T) {
			consoleTestHome(t)
			kind, role, bearer, wantRequests := "operator", "account_owner", accountFakeBearer, int32(0)
			switch name {
			case "missing":
				bearer = ""
			case "agent-bearer":
				bearer = consoleFakeToken
			case "agent-principal":
				kind, wantRequests = "agent", 1
			case "unknown-role":
				role, wantRequests = "account_member", 1
			}
			server, requests := accountConsoleRouteCell(t, kind, consoleTestIdentity().AccountID, role)
			accountConsoleRouteSave(t, "selected", &local.Route{Kind: "local", Endpoint: server.URL}, bearer)
			reads := accountConsoleRouteCountReads(t)
			calls := 0
			conn := agentConnection{Endpoint: server.URL, Token: consoleFakeToken, AccountName: "selected", AccountID: consoleTestIdentity().AccountID}
			manager := resolveAccountConsoleManager(context.Background(), conn, consoleTestIdentity(), false, accountConsoleRouteBlackhole(&calls))
			if manager != nil || calls != 0 || *reads != 1 || requests.Load() != wantRequests {
				t.Fatal("credential refusal changed manager authority, directory use, or request count")
			}
		})
	}
}

func TestAccountConsoleRouteStaleBindingDenials(t *testing.T) {
	for _, name := range []string{"added", "cleared", "endpoint", "route-kind", "account", "resolved-name"} {
		t.Run(name, func(t *testing.T) {
			consoleTestHome(t)
			server, requests := accountConsoleRouteCell(t, "operator", consoleTestIdentity().AccountID, "account_owner")
			route := local.Route{Kind: "local", Endpoint: server.URL}
			initialRoute := &route
			if name == "added" {
				initialRoute = nil
			}
			accountConsoleRouteSave(t, "selected", initialRoute, accountFakeBearer)
			previous := accountConsoleResolve
			reads := 0
			accountConsoleResolve = func(selected string) (string, local.Account, string, error) {
				reads++
				resolved, account, bearer, err := local.Resolve(selected)
				if err != nil {
					t.Fatal("synthetic second account read failed")
				}
				// Only the credential-read result changes. A third configuration
				// read would see the original and must not hide this stale result.
				switch name {
				case "added":
					account.Route = &route
				case "cleared":
					account.Route = nil
				case "endpoint":
					account.Route.Endpoint += "/"
				case "route-kind":
					account.Route.Kind = "other"
				case "account":
					account.ID = "acc_other"
				case "resolved-name":
					resolved = "other"
				}
				return resolved, account, bearer, nil
			}
			t.Cleanup(func() { accountConsoleResolve = previous })
			calls := 0
			locate := accountConsoleRouteBlackhole(&calls)
			wantCalls := 0
			if name == "added" {
				wantCalls = 1
				locate = func(context.Context, string, string) (string, string, error) {
					calls++
					return "", server.URL, nil
				}
			}
			conn := agentConnection{Endpoint: server.URL, Token: consoleFakeToken, AccountName: "selected", AccountID: consoleTestIdentity().AccountID}
			if manager := resolveAccountConsoleManager(context.Background(), conn, consoleTestIdentity(), false, locate); manager != nil || calls != wantCalls || reads != 1 || requests.Load() != 0 {
				t.Fatal("stale credential-read binding acquired manager authority")
			}
		})
	}
}

func TestAccountConsoleRouteWrongOriginDenials(t *testing.T) {
	for _, name := range []string{"http-non-loopback", "http-hostname", "https-non-loopback", "https-loopback", "route-path", "selected-other-loopback", "selected-hosted", "other-principal-account"} {
		t.Run(name, func(t *testing.T) {
			consoleTestHome(t)
			accountID := consoleTestIdentity().AccountID
			if name == "other-principal-account" {
				accountID = "acc_other"
			}
			server, requests := accountConsoleRouteCell(t, "operator", accountID, "account_owner")
			route := &local.Route{Kind: "local", Endpoint: server.URL}
			accountConsoleRouteSave(t, "selected", route, accountFakeBearer)
			conn := agentConnection{Endpoint: server.URL, Token: consoleFakeToken, AccountName: "selected", AccountID: consoleTestIdentity().AccountID}
			switch name {
			case "http-non-loopback":
				route.Endpoint = "http://192.0.2.1:1"
			case "http-hostname":
				route.Endpoint = strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
			case "https-non-loopback":
				route.Endpoint = "https://hosted.example.invalid"
			case "https-loopback":
				route.Endpoint = strings.Replace(server.URL, "http://", "https://", 1)
			case "route-path":
				route.Endpoint += "/untrusted"
			case "selected-other-loopback":
				conn.Endpoint = "http://127.0.0.1:1"
			case "selected-hosted":
				conn.Endpoint = "https://hosted.example.invalid"
			}
			if strings.HasPrefix(name, "http") || name == "route-path" {
				// Write the configuration directly: Save refuses the malformed
				// origins here, and route storage accepts a base path that the
				// console's stricter origin rule must still reject.
				conn.Endpoint = route.Endpoint
				raw, err := json.Marshal(local.Config{Accounts: map[string]local.Account{"selected": {ID: conn.AccountID, Route: route}}})
				home, homeErr := local.Home()
				if err != nil || homeErr != nil || os.WriteFile(filepath.Join(home, "config.json"), raw, 0o600) != nil {
					t.Fatal("write synthetic non-secret configuration failed")
				}
			}
			reads := accountConsoleRouteCountReads(t)
			if name == "https-non-loopback" {
				// Fail at the credential boundary even under a validation mutant,
				// so this external-looking fixture can never send a request.
				accountConsoleResolve = func(string) (string, local.Account, string, error) {
					*reads++
					return "", local.Account{}, "", errors.New("untrusted origin reached credential read")
				}
			}
			calls := 0
			manager := resolveAccountConsoleManager(context.Background(), conn, consoleTestIdentity(), false, accountConsoleRouteBlackhole(&calls))
			wantRequests, wantReads := int32(0), 0
			if name == "other-principal-account" {
				wantRequests, wantReads = 1, 1
			}
			if manager != nil || calls != 0 || *reads != wantReads || requests.Load() != wantRequests {
				t.Fatal("wrong origin or account acquired manager authority or crossed the credential-read boundary")
			}
		})
	}
}

func TestAccountConsoleRouteExplicitFlagsDenyBeforeCredentialRead(t *testing.T) {
	for _, args := range [][]string{{"--endpoint="}, {"--endpoint=http://127.0.0.1:1"}, {"--token-file="}, {"--token-file=synthetic-only"}} {
		t.Run(strings.Join(args, ""), func(t *testing.T) {
			consoleTestHome(t)
			server, requests := accountConsoleRouteCell(t, "operator", consoleTestIdentity().AccountID, "account_owner")
			accountConsoleRouteSave(t, "selected", &local.Route{Kind: "local", Endpoint: server.URL}, accountFakeBearer)
			fs := flag.NewFlagSet("synthetic", flag.ContinueOnError)
			factConnectionFlags(fs)
			if fs.Parse(args) != nil {
				t.Fatal("synthetic flags failed")
			}
			reads := accountConsoleRouteCountReads(t)
			calls := 0
			conn := agentConnection{Endpoint: server.URL, Token: consoleFakeToken, AccountName: "selected", AccountID: consoleTestIdentity().AccountID}
			if manager := resolveAccountConsoleManager(context.Background(), conn, consoleTestIdentity(), accountConsoleExplicit(fs), accountConsoleRouteBlackhole(&calls)); manager != nil || calls != 0 || *reads != 0 || requests.Load() != 0 {
				t.Fatal("explicit invocation acquired routed manager authority")
			}
		})
	}
}

func TestAccountConsoleRouteHostedDirectoryAndLocalRevalidation(t *testing.T) {
	for _, routed := range []bool{false, true} {
		name := "hosted"
		if routed {
			name = "local"
		}
		t.Run(name, func(t *testing.T) {
			consoleTestHome(t)
			server, requests := accountConsoleRouteCell(t, "operator", consoleTestIdentity().AccountID, "account_owner")
			var route *local.Route
			if routed {
				route = &local.Route{Kind: "local", Endpoint: server.URL}
			}
			accountConsoleRouteSave(t, "selected", route, accountFakeBearer)
			reads := accountConsoleRouteCountReads(t)
			calls := 0
			locate := func(_ context.Context, directory, accountID string) (string, string, error) {
				calls++
				if directory != defaultControlPlane || accountID != consoleTestIdentity().AccountID || *reads != 0 {
					t.Error("hosted directory lookup changed account or credential ordering")
				}
				return "", server.URL + "/", nil
			}
			conn := agentConnection{Endpoint: server.URL, Token: consoleFakeToken, AccountName: "selected", AccountID: consoleTestIdentity().AccountID}
			manager := resolveAccountConsoleManager(context.Background(), conn, consoleTestIdentity(), false, locate)
			wantCalls := 1
			if routed {
				wantCalls = 0
			}
			if manager == nil || calls != wantCalls || *reads != 1 || requests.Load() != 1 {
				t.Fatal("binding changed hosted directory behavior or routed isolation")
			}
			if principal, err := client.RevalidateAccountConsoleManager(context.Background(), *manager); err != nil || principal != manager.Identity || requests.Load() != 2 {
				t.Fatal("manager did not revalidate against its bound cell")
			}
			server.Close()
			if _, err := client.RevalidateAccountConsoleManager(context.Background(), *manager); !errors.Is(err, client.ErrAccountConsoleUnavailable) {
				t.Fatal("stopped cell did not become unavailable")
			}
			if calls != wantCalls || *reads != 1 {
				t.Fatal("revalidation reopened directory or credential resolution")
			}
		})
	}
}

func TestAccountConsoleRouteTUIConstructor(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "managed"
		if explicit {
			name = "explicit-agent-only"
		}
		t.Run(name, func(t *testing.T) {
			isolateAgentTUI(t)
			server, requests := accountConsoleRouteCell(t, "operator", consoleTestIdentity().AccountID, "account_owner")
			accountConsoleRouteSave(t, "selected", &local.Route{Kind: "local", Endpoint: server.URL}, accountFakeBearer)
			previousLocate, previousConnectionLocate, previousConnect := accountConsoleLocate, connectionLocate, accountConsoleConnect
			calls := 0
			accountConsoleLocate, connectionLocate = accountConsoleRouteBlackhole(&calls), accountConsoleRouteBlackhole(&calls)
			accountConsoleConnect = connectAgent
			t.Cleanup(func() {
				accountConsoleLocate, connectionLocate, accountConsoleConnect = previousLocate, previousConnectionLocate, previousConnect
			})
			constructed, ran := false, false
			createTUIConsole = func(_ context.Context, _ agentConnection, _ client.SelfIdentity, _ time.Duration, options accountConsoleOptions) agenttui.ConsoleController {
				constructed = true
				if (options.Manager == nil) != explicit || (options.Manager != nil && options.Manager.Endpoint != server.URL) {
					t.Fatal("TUI constructor lost routed authority boundary")
				}
				return &testTUIConsoleCleanup{}
			}
			runAgentTUI = func(ctx context.Context, source agenttui.Source, _ agenttui.Options) error {
				ran = true
				raw, err := source.Read(ctx, dashboard.ReadRequest{Resource: dashboard.ResourceAccountContext})
				want := `"available":true`
				if explicit {
					want = `"available":false`
				}
				if err != nil || !strings.Contains(string(raw), want) {
					t.Fatal("TUI reader lost routed authority boundary")
				}
				return nil
			}
			args := []string{"--account", "selected", "--realm", "synthetic-realm", "--agent", "synthetic"}
			if explicit {
				args = append(args, "--endpoint", server.URL)
			}
			if agentTUI(context.Background(), args) != 0 || !constructed || !ran || calls != 0 || (!explicit && requests.Load() < 2) || (explicit && requests.Load() != 0) {
				t.Fatal("routed TUI consulted the directory or failed to preserve agent-only explicit mode")
			}
		})
	}
}
