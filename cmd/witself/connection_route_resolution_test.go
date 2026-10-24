package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/witwave-ai/witself/internal/local"
)

const (
	connectionRouteAccount = "selected"
	connectionRouteID      = "acc_route_selected"
	connectionRouteOwner   = "synthetic-route-owner"
	connectionRouteAgent   = "synthetic-route-agent"
)

func connectionRouteTestHome(t *testing.T, route *local.Route) string {
	t.Helper()
	return connectionRouteTestNamedHome(t, connectionRouteAccount, route)
}

func connectionRouteTestNamedHome(t *testing.T, accountName string, route *local.Route) string {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, name := range []string{"WITSELF_ACCOUNT", "WITSELF_REALM", "WITSELF_AGENT", "WITSELF_ENDPOINT", "WITSELF_TOKEN_FILE", "WITSELF_TOKEN", "WITSELF_CONTROL_PLANE"} {
		t.Setenv(name, "")
	}
	if err := local.Save(accountName, local.Account{ID: connectionRouteID, Route: route}, connectionRouteOwner); err != nil {
		t.Fatal(err)
	}
	path, err := local.AgentTokenPath(accountName, "default", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(connectionRouteAgent+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func connectionRouteTestLocator(t *testing.T, locate accountLocator) {
	t.Helper()
	previous := connectionLocate
	connectionLocate = locate
	t.Cleanup(func() { connectionLocate = previous })
}

func TestConnectionRouteDirectoryBlackhole(t *testing.T) {
	for _, selection := range []struct {
		name        string
		accountName string
		explicit    string
		environment string
	}{
		{"named-explicit", connectionRouteAccount, connectionRouteAccount, ""},
		{"default-explicit", "default", "default", ""},
		{"default-implicit", "default", "", ""},
		{"default-environment", "default", "", "default"},
	} {
		for _, resolver := range []string{"operator", "agent", "domain-operator", "domain-token-file", "domain-token-env"} {
			for _, mode := range []string{"local", "hosted-blocked", "hosted"} {
				t.Run(selection.name+"/"+resolver+"/"+mode, func(t *testing.T) {
					var route *local.Route
					wantEndpoint := "https://hosted.example.invalid"
					if mode == "local" {
						wantEndpoint = "http://127.0.0.1:42123"
						route = &local.Route{Kind: "local", Endpoint: wantEndpoint}
					}
					agentPath := connectionRouteTestNamedHome(t, selection.accountName, route)
					t.Setenv("WITSELF_ACCOUNT", selection.environment)
					calls := 0
					blocked := errors.New("directory blackhole")
					connectionRouteTestLocator(t, func(_ context.Context, directory, accountID string) (string, string, error) {
						calls++
						if directory != defaultControlPlane || accountID != connectionRouteID {
							t.Error("directory received a different account or audience")
						}
						if mode != "hosted" {
							return "", "", blocked
						}
						return "hosted-cell", wantEndpoint, nil
					})
					var endpoint, bearer string
					var err error
					wantBearer := connectionRouteOwner
					switch resolver {
					case "operator":
						endpoint, bearer, err = connect(context.Background(), selection.explicit, "", "")
					case "agent":
						var conn agentConnection
						conn, err = connectAgent(context.Background(), selection.explicit, "default", "worker", "", "")
						endpoint, bearer = conn.Endpoint, conn.Token
						wantBearer = connectionRouteAgent
						if err == nil && (conn.AccountID != connectionRouteID || conn.AccountName != selection.accountName || conn.RealmName != "default" || conn.AgentName != "worker") {
							t.Error("agent connection changed its selected identity")
						}
					case "domain-operator":
						endpoint, bearer, err = connectDomain(context.Background(), selection.explicit, "", "")
					case "domain-token-file":
						endpoint, bearer, err = connectDomain(context.Background(), selection.explicit, "", agentPath)
						wantBearer = connectionRouteAgent
					case "domain-token-env":
						t.Setenv("WITSELF_TOKEN", connectionRouteAgent)
						endpoint, bearer, err = connectDomain(context.Background(), selection.explicit, "", "")
						wantBearer = connectionRouteAgent
					}
					wantCalls := 1
					if mode == "local" {
						wantCalls = 0
					}
					if calls != wantCalls {
						t.Fatalf("directory calls = %d, want %d", calls, wantCalls)
					}
					if mode == "hosted-blocked" {
						if !errors.Is(err, blocked) || endpoint != "" || bearer != "" {
							t.Fatal("hosted directory failure did not fail closed")
						}
						return
					}
					if err != nil || endpoint != wantEndpoint || bearer != wantBearer {
						t.Fatalf("connection endpoint or credential selection changed: %v", err)
					}
				})
			}
		}
	}
}

func TestConnectionRouteExplicitPrecedence(t *testing.T) {
	for _, routed := range []bool{false, true} {
		for _, resolver := range []string{"operator-named", "agent-named", "domain-named", "operator-token-file", "agent-token-file", "domain-token-file", "domain-env", "operator-missing-endpoint", "agent-missing-endpoint"} {
			t.Run(resolver+map[bool]string{false: "/hosted", true: "/local"}[routed], func(t *testing.T) {
				var route *local.Route
				if routed {
					route = &local.Route{Kind: "local", Endpoint: "http://127.0.0.1:42123"}
				}
				agentPath := connectionRouteTestHome(t, route)
				calls := 0
				connectionRouteTestLocator(t, func(context.Context, string, string) (string, string, error) {
					calls++
					return "", "", errors.New("directory blackhole")
				})
				const explicit = "https://explicit.example.invalid"
				var endpoint, bearer string
				var err error
				wantBearer := connectionRouteOwner
				switch resolver {
				case "operator-named":
					endpoint, bearer, err = connect(context.Background(), connectionRouteAccount, explicit, "")
				case "agent-named":
					var conn agentConnection
					conn, err = connectAgent(context.Background(), connectionRouteAccount, "default", "worker", explicit, "")
					endpoint, bearer = conn.Endpoint, conn.Token
					wantBearer = connectionRouteAgent
				case "domain-named":
					endpoint, bearer, err = connectDomain(context.Background(), connectionRouteAccount, explicit, "")
				case "operator-token-file":
					endpoint, bearer, err = connect(context.Background(), "missing", explicit, agentPath)
					wantBearer = connectionRouteAgent
				case "agent-token-file":
					var conn agentConnection
					conn, err = connectAgent(context.Background(), "missing", "default", "worker", explicit, agentPath)
					endpoint, bearer = conn.Endpoint, conn.Token
					wantBearer = connectionRouteAgent
				case "domain-token-file":
					endpoint, bearer, err = connectDomain(context.Background(), "missing", explicit, agentPath)
					wantBearer = connectionRouteAgent
				case "domain-env":
					t.Setenv("WITSELF_ENDPOINT", explicit)
					t.Setenv("WITSELF_TOKEN_FILE", agentPath)
					endpoint, bearer, err = connectDomain(context.Background(), "missing", "", "")
					wantBearer = connectionRouteAgent
				case "operator-missing-endpoint":
					endpoint, bearer, err = connect(context.Background(), connectionRouteAccount, "", agentPath)
				case "agent-missing-endpoint":
					var conn agentConnection
					conn, err = connectAgent(context.Background(), connectionRouteAccount, "default", "worker", "", agentPath)
					endpoint, bearer = conn.Endpoint, conn.Token
				}
				if calls != 0 {
					t.Fatal("explicit connection consulted the directory")
				}
				if resolver == "operator-missing-endpoint" || resolver == "agent-missing-endpoint" {
					if err == nil || endpoint != "" || bearer != "" {
						t.Fatal("explicit token file without endpoint was accepted")
					}
					return
				}
				if err != nil || endpoint != explicit || bearer != wantBearer {
					t.Fatalf("explicit endpoint or credential precedence changed: %v", err)
				}
			})
		}
	}
}

func TestConnectionRoutePlanAndBillingRefuseWithoutCredentialRead(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	connectionRouteTestHome(t, &local.Route{Kind: "local", Endpoint: server.URL})
	calls := 0
	connectionRouteTestLocator(t, func(context.Context, string, string) (string, string, error) {
		calls++
		return "", server.URL, nil
	})
	const refusal = "plan and billing commands are not available on a local connection route"
	for _, missingCredential := range []bool{false, true} {
		if missingCredential {
			path, err := local.TokenPath(connectionRouteAccount)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		for _, resolve := range []func(context.Context, string, string) (string, string, string, error){planContext, billingContext} {
			accountID, bearer, endpoint, err := resolve(context.Background(), connectionRouteAccount, "")
			if err == nil || err.Error() != refusal || accountID != "" || bearer != "" || endpoint != "" {
				t.Fatalf("local billing did not return its value-free refusal: %v", err)
			}
		}
		for _, command := range [][]string{{"plan", "status", "--account", connectionRouteAccount}, {"billing", "show", "--account", connectionRouteAccount}} {
			stdout, stderr, code := capturePlanCLI(t, func() int { return run(command) })
			prefix := "witself: "
			if command[0] == "billing" {
				prefix += "billing: "
			}
			if code != 1 || stdout != "" || stderr != prefix+refusal+"\n" {
				t.Fatal("local plan or billing CLI refusal changed or exposed values")
			}
		}
	}
	if calls != 0 || requests.Load() != 0 {
		t.Fatalf("local billing made directory calls=%d or HTTP requests=%d", calls, requests.Load())
	}
}

func TestConnectionRoutePlanHostedAndExplicitBehavior(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/v1/whoami":
			if r.Header.Get("Authorization") != "Bearer "+connectionRouteOwner {
				t.Error("billing operator credential changed")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"principal": map[string]string{"operator_id": "op_route", "account_id": connectionRouteID}})
		case "/v1/capabilities":
			if r.Header.Get("Authorization") != "" {
				t.Error("billing capability read received a credential")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0", "backend": map[string]string{"kind": "self-hosted"},
				"account": map[string]string{"id": connectionRouteID},
				"billing": map[string]any{"supported": true, "endpoint": "https://billing.example.invalid"},
			})
		default:
			t.Error("unexpected billing discovery path")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	for _, routed := range []bool{false, true} {
		t.Run(map[bool]string{false: "hosted", true: "local-explicit"}[routed], func(t *testing.T) {
			var route *local.Route
			endpoint := ""
			wantCalls := 1
			if routed {
				route = &local.Route{Kind: "local", Endpoint: "http://127.0.0.1:42123"}
				endpoint, wantCalls = server.URL, 0
			}
			connectionRouteTestHome(t, route)
			calls := 0
			connectionRouteTestLocator(t, func(_ context.Context, directory, accountID string) (string, string, error) {
				calls++
				if directory != defaultControlPlane || accountID != connectionRouteID {
					t.Error("hosted billing directory audience changed")
				}
				return "hosted-cell", server.URL, nil
			})
			before := requests.Load()
			accountID, bearer, billing, err := planContext(context.Background(), connectionRouteAccount, endpoint)
			if err != nil || accountID != connectionRouteID || bearer != connectionRouteOwner || billing != "https://billing.example.invalid" || calls != wantCalls || requests.Load()-before != 2 {
				t.Fatalf("hosted or explicit plan resolution changed: %v; directory calls=%d; requests=%d", err, calls, requests.Load()-before)
			}
		})
	}
}

func TestConnectionRouteConsoleUsesLocalTrust(t *testing.T) {
	consoleTestHome(t)
	conn, expected, _ := accountTestBackend(t)
	if err := local.Save("selected", local.Account{ID: conn.AccountID, Route: &local.Route{Kind: "local", Endpoint: conn.Endpoint}}, accountFakeBearer); err != nil {
		t.Fatal(err)
	}
	previous := accountConsoleResolve
	reads := 0
	accountConsoleResolve = func(name string) (string, local.Account, string, error) {
		reads++
		return local.Resolve(name)
	}
	t.Cleanup(func() { accountConsoleResolve = previous })
	calls := 0
	locate := func(context.Context, string, string) (string, string, error) {
		calls++
		return "", "", errors.New("directory blackhole")
	}
	if got := resolveAccountConsoleManager(context.Background(), conn, consoleTestIdentity(), false, locate); got == nil || *got != *expected || calls != 0 || reads != 1 {
		t.Fatal("local route did not establish independent console trust")
	}
	reads = 0
	if got := resolveAccountConsoleManager(context.Background(), conn, consoleTestIdentity(), true, locate); got != nil || calls != 0 || reads != 0 {
		t.Fatal("explicit endpoint was not declined before console lookup or credential read")
	}
}
