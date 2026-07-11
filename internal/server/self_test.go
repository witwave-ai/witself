package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSelfDigestUsesAgentTokenIdentity(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		switch token {
		case "agent-token":
			return DomainPrincipal{
				Kind: PrincipalKindAgent, ID: "agent_1", AgentName: "scott",
				AccountID: "acc_1", RealmID: "realm_1", RealmName: "default",
				AccountStatus: "active",
			}, true, nil
		case "operator-token":
			return DomainPrincipal{Kind: PrincipalKindOperator, ID: "opr_1", AccountID: "acc_1", AccountStatus: "active"}, true, nil
		case "suspended-token":
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", AccountStatus: "suspended"}, true, nil
		default:
			return DomainPrincipal{}, false, nil
		}
	}
	srv := httptest.NewServer(apiMux(Config{AuthenticatePrincipal: auth}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self?include_facts=true&include_salient=true&salient_limit=10&max_bytes=8192", "agent-token")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent self = %d, want 200", resp.StatusCode)
	}
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if digest.SchemaVersion != "witself.v0" {
		t.Errorf("schema_version = %q", digest.SchemaVersion)
	}
	if digest.Identity != (SelfIdentity{AccountID: "acc_1", AgentID: "agent_1", AgentName: "scott", RealmID: "realm_1", RealmName: "default"}) {
		t.Errorf("identity = %+v", digest.Identity)
	}
	if digest.PrimaryFacts == nil || digest.SalientMemories == nil || digest.Index.Kinds == nil || digest.Index.Tags == nil {
		t.Fatalf("empty collections must be JSON arrays: %+v", digest)
	}
	if digest.Index.Counts["facts"] != 0 || digest.Index.Counts["memories"] != 0 || digest.Elided {
		t.Errorf("identity-only digest = %+v", digest)
	}
}

func TestSelfDigestAuthorizationAndBounds(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		switch token {
		case "agent-token":
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, true, nil
		case "operator-token":
			return DomainPrincipal{Kind: PrincipalKindOperator, ID: "opr_1", AccountID: "acc_1", AccountStatus: "active"}, true, nil
		case "suspended-token":
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", AccountStatus: "suspended"}, true, nil
		default:
			return DomainPrincipal{}, false, nil
		}
	}
	srv := httptest.NewServer(apiMux(Config{AuthenticatePrincipal: auth}))
	defer srv.Close()

	cases := []struct {
		name  string
		path  string
		token string
		want  int
	}{
		{name: "missing token", path: "/v1/self", want: http.StatusUnauthorized},
		{name: "invalid token", path: "/v1/self", token: "invalid", want: http.StatusUnauthorized},
		{name: "operator", path: "/v1/self", token: "operator-token", want: http.StatusForbidden},
		{name: "suspended account", path: "/v1/self", token: "suspended-token", want: http.StatusForbidden},
		{name: "bad boolean", path: "/v1/self?include_facts=perhaps", token: "agent-token", want: http.StatusBadRequest},
		{name: "bad salient limit", path: "/v1/self?salient_limit=101", token: "agent-token", want: http.StatusBadRequest},
		{name: "bad max bytes", path: "/v1/self?max_bytes=100", token: "agent-token", want: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := selfRequest(t, srv.URL+tc.path, tc.token)
			defer closeBody(t, resp)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func selfRequest(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
