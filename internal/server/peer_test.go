package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSelfPeersUsesTokenScopeFiltersSelfAndPreservesUnknownActivity(t *testing.T) {
	observed := time.Date(2026, 7, 15, 20, 30, 0, 0, time.UTC)
	wantPrincipal := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_scott", AgentName: "scott",
		AccountID: "acc_1", RealmID: "realm_default", RealmName: "default",
		AccountStatus: "active",
	}
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		if token != "agent-token" {
			return DomainPrincipal{}, false, nil
		}
		return wantPrincipal, true, nil
	}
	called := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		ListSelfPeers: func(_ context.Context, p DomainPrincipal) ([]SelfPeer, error) {
			called++
			if p != wantPrincipal {
				t.Fatalf("principal = %#v, want %#v", p, wantPrincipal)
			}
			return []SelfPeer{
				{ID: "agent_scott", Name: "scott", LastActivityAt: &observed},
				{ID: "agent_bob", Name: "bob", LastActivityAt: &observed, LastRuntime: "claude-code", LastLocation: "home", LastEvent: "Stop"},
				{ID: "agent_dormant", Name: "codex-test-bot"},
			}, nil
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self/peers", "agent-token")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var page SelfPeers
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if page.SchemaVersion != "witself.v0" || len(page.Peers) != 2 || called != 1 {
		t.Fatalf("page = %#v, calls = %d", page, called)
	}
	if got := page.Peers[0]; got.ID != "agent_bob" || got.Name != "bob" ||
		got.LastActivityAt == nil || !got.LastActivityAt.Equal(observed) ||
		got.LastRuntime != "claude-code" || got.LastLocation != "home" || got.LastEvent != "Stop" {
		t.Fatalf("active peer = %#v", got)
	}
	if got := page.Peers[1]; got.ID != "agent_dormant" || got.LastActivityAt != nil ||
		got.LastRuntime != "" || got.LastLocation != "" || got.LastEvent != "" {
		t.Fatalf("unknown-activity peer = %#v", got)
	}
}

func TestSelfPeersAuthorizationAndRejectsScopeSelectors(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		switch token {
		case "agent-token":
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, true, nil
		case "operator-token":
			return DomainPrincipal{Kind: PrincipalKindOperator, ID: "opr_1", AccountID: "acc_1", AccountStatus: "active"}, true, nil
		case "curator-token":
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active", AccessProfile: AccessProfileCuratorPreview}, true, nil
		default:
			return DomainPrincipal{}, false, nil
		}
	}
	calls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		ListSelfPeers: func(context.Context, DomainPrincipal) ([]SelfPeer, error) {
			calls++
			return nil, nil
		},
	}))
	defer srv.Close()

	tests := []struct {
		name  string
		token string
		path  string
		want  int
	}{
		{name: "missing token", path: "/v1/self/peers", want: http.StatusUnauthorized},
		{name: "invalid token", token: "invalid", path: "/v1/self/peers", want: http.StatusUnauthorized},
		{name: "operator", token: "operator-token", path: "/v1/self/peers", want: http.StatusForbidden},
		{name: "restricted curator", token: "curator-token", path: "/v1/self/peers", want: http.StatusForbidden},
		{name: "realm selector", token: "agent-token", path: "/v1/self/peers?realm=realm_other", want: http.StatusBadRequest},
		{name: "agent selector", token: "agent-token", path: "/v1/self/peers?agent=agent_other", want: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := selfRequest(t, srv.URL+tc.path, tc.token)
			defer closeBody(t, resp)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("list callback called %d times for refused requests", calls)
	}
}

func TestSelfPeersEncodesEmptyArrayAndOmitsInternalActivityFields(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, true, nil
	}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		ListSelfPeers: func(context.Context, DomainPrincipal) ([]SelfPeer, error) {
			return nil, nil
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self/peers", "token")
	defer closeBody(t, resp)
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw["peers"])); got != "[]" {
		t.Fatalf("peers JSON = %s, want []", got)
	}
	encoded, _ := json.Marshal(raw)
	for _, forbidden := range []string{"event_id", "event_occurred_at", "location_id", "availability"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("response exposed %q: %s", forbidden, encoded)
		}
	}
}
