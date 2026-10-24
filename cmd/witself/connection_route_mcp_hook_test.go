package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/memoryhydration"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

// All credentials in these fixtures are synthetic and confined to temporary homes.
func setupRouteRuntime(t *testing.T) transcriptcapture.Config {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	if err := local.Save("route-test", local.Account{ID: "acc_route"}, "synthetic-operator-route"); err != nil {
		t.Fatal(err)
	}
	tokenPath, err := local.AgentTokenPath("route-test", "default", "route-agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("synthetic-agent-route\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return transcriptcapture.Config{
		Runtime: transcriptcapture.RuntimeClaudeCode, CaptureMode: transcriptcapture.ModeMessages, HookMode: transcriptcapture.HookModeUser,
		Account: "route-test", AccountID: "acc_route", Realm: "default", RealmID: "rlm_route",
		Agent: "route-agent", AgentID: "agt_route", AgentName: "route-agent",
		Location: transcriptcapture.Location{ID: "loc_route", Name: "test"},
	}
}

type routeRuntimeServer struct {
	*httptest.Server
	selfCalls       atomic.Int32
	transcriptCalls atomic.Int32
	mismatched      atomic.Bool
}

func newRouteRuntimeServer(t *testing.T) *routeRuntimeServer {
	t.Helper()
	server := &routeRuntimeServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-agent-route" {
			t.Error("request did not use the synthetic agent credential")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/self":
			server.selfCalls.Add(1)
			identity := client.SelfIdentity{AccountID: "acc_route", RealmID: "rlm_route", RealmName: "default", AgentID: "agt_route", AgentName: "route-agent"}
			if server.mismatched.Load() {
				identity.AgentID = "agt_other"
			}
			_ = json.NewEncoder(w).Encode(client.SelfDigest{
				SchemaVersion: "witself.v0", Identity: identity,
				PrimaryFacts:    []client.SelfFact{{ID: "fact_route", Name: "database", Value: "Postgres", Primary: true}},
				SalientMemories: []client.SelfMemory{}, Index: client.SelfIndex{Kinds: []string{}, Tags: []string{}, Counts: map[string]int{}},
			})
		case "/v1/transcripts":
			server.transcriptCalls.Add(1)
			_, _ = w.Write([]byte(`{"transcripts":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func blackholeRouteDirectory(t *testing.T) *atomic.Int32 {
	t.Helper()
	original := connectionLocate
	calls := &atomic.Int32{}
	connectionLocate = func(context.Context, string, string) (string, string, error) {
		calls.Add(1)
		return "", "", errors.New("directory blackholed for connection route test")
	}
	t.Cleanup(func() { connectionLocate = original })
	return calls
}

func startRouteMCP(t *testing.T, cfg transcriptcapture.Config) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- runConfiguredMCPServer(ctx, cfg, mcpServerOptions{}, serverTransport) }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "route-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("MCP serve shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("MCP serve did not stop")
		}
	})
	return session
}

func callRouteMCP(t *testing.T, session *mcp.ClientSession, tool string, wantFailure bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError != wantFailure {
		t.Fatalf("%s failure = %t, want %t", tool, result.IsError, wantFailure)
	}
}

func TestMCPConnectionRouteStartupSnapshot(t *testing.T) {
	for _, initialLocal := range []bool{true, false} {
		name := "hosted startup"
		if initialLocal {
			name = "local startup"
		}
		t.Run(name, func(t *testing.T) {
			cfg := setupRouteRuntime(t)
			first, second := newRouteRuntimeServer(t), newRouteRuntimeServer(t)
			directoryCalls := blackholeRouteDirectory(t)
			if initialLocal {
				if err := local.SetRoute(cfg.Account, &local.Route{Kind: "local", Endpoint: first.URL}); err != nil {
					t.Fatal(err)
				}
			} else {
				connectionLocate = func(context.Context, string, string) (string, string, error) {
					directoryCalls.Add(1)
					return "cell_test", first.URL, nil
				}
			}
			running := startRouteMCP(t, cfg)
			callRouteMCP(t, running, "witself.self.show", false)
			if first.selfCalls.Load() != 1 {
				t.Fatal("startup endpoint was not used")
			}
			if err := local.SetRoute(cfg.Account, &local.Route{Kind: "local", Endpoint: second.URL}); err != nil {
				t.Fatal(err)
			}
			callRouteMCP(t, running, "witself.self.show", false)
			if first.selfCalls.Load() != 2 || second.selfCalls.Load() != 0 {
				t.Fatal("running MCP adopted a route change without restarting")
			}
			restarted := startRouteMCP(t, cfg)
			callRouteMCP(t, restarted, "witself.self.show", false)
			if second.selfCalls.Load() != 1 {
				t.Fatal("restarted MCP did not adopt the changed route")
			}
			if err := local.SetRoute(cfg.Account, nil); err != nil {
				t.Fatal(err)
			}
			callRouteMCP(t, restarted, "witself.transcript.list", false)
			if second.selfCalls.Load() != 2 || second.transcriptCalls.Load() != 1 {
				t.Fatal("running MCP lost its route or skipped identity verification after route clear")
			}
			wantDirectoryCalls := int32(2)
			if initialLocal {
				wantDirectoryCalls = 0
			}
			if got := directoryCalls.Load(); got != wantDirectoryCalls {
				t.Fatalf("directory calls = %d, want %d", got, wantDirectoryCalls)
			}
		})
	}
}

func TestMCPConnectionRouteRejectsIdentityMismatch(t *testing.T) {
	cfg := setupRouteRuntime(t)
	server := newRouteRuntimeServer(t)
	calls := blackholeRouteDirectory(t)
	if err := local.SetRoute(cfg.Account, &local.Route{Kind: "local", Endpoint: server.URL}); err != nil {
		t.Fatal(err)
	}
	session := startRouteMCP(t, cfg)
	callRouteMCP(t, session, "witself.transcript.list", false)
	server.mismatched.Store(true)
	callRouteMCP(t, session, "witself.self.show", true)
	callRouteMCP(t, session, "witself.transcript.list", true)
	if server.selfCalls.Load() != 3 || server.transcriptCalls.Load() != 1 {
		t.Fatal("routed MCP did not verify identity before every operation")
	}
	if calls.Load() != 0 {
		t.Fatal("routed MCP consulted the directory")
	}
}

func TestHookConnectionRouteAdoptsEachEventAndRejectsIdentityMismatch(t *testing.T) {
	cfg := setupRouteRuntime(t)
	first, second := newRouteRuntimeServer(t), newRouteRuntimeServer(t)
	calls := blackholeRouteDirectory(t)
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	event := transcriptcapture.Event{Runtime: cfg.Runtime, HookEvent: memoryhydration.EventSessionStart}
	for _, server := range []*routeRuntimeServer{first, second} {
		if err := local.SetRoute(cfg.Account, &local.Route{Kind: "local", Endpoint: server.URL}); err != nil {
			t.Fatal(err)
		}
		output, err := automaticHydrationHook(context.Background(), event)
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(output) || !strings.Contains(string(output), "WITSELF_AUTOMATIC_CONTEXT_V1") {
			t.Fatal("routed hook did not hydrate")
		}
		if server.selfCalls.Load() != 1 {
			t.Fatal("hook did not adopt the current connection route")
		}
	}
	second.mismatched.Store(true)
	output, err := automaticHydrationHook(context.Background(), event)
	if err == nil || len(output) != 0 {
		t.Fatal("identity-mismatched routed hydration was not refused")
	}
	if calls.Load() != 0 {
		t.Fatal("routed hooks consulted the directory")
	}
	if err := local.SetRoute(cfg.Account, nil); err != nil {
		t.Fatal(err)
	}
	output, err = automaticHydrationHook(context.Background(), event)
	if err == nil || len(output) != 0 || calls.Load() != 1 {
		t.Fatal("cleared hook route did not return to hosted lookup")
	}
}
