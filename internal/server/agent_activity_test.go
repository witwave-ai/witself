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

func TestTouchAgentActivityUsesAgentIdentityAndPublicProjection(t *testing.T) {
	observedAt := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)
	var gotPrincipal DomainPrincipal
	var gotInput AgentActivityRequest
	server := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: agentActivityTestAuth,
		TouchAgentActivity: func(_ context.Context, p DomainPrincipal, in AgentActivityRequest) (AgentActivity, error) {
			gotPrincipal, gotInput = p, in
			return AgentActivity{
				LastActivityAt: observedAt, LastRuntime: in.Runtime,
				LastLocation: in.Location, LastEvent: in.Event,
			}, nil
		},
	}))
	defer server.Close()

	body := `{"runtime":"cursor","location_id":"loc_1","location":"home","event":"AgentResponse","event_id":"evt_1","event_occurred_at":"2026-07-15T19:59:59Z"}`
	resp := postAgentActivity(t, server.URL, "agent-token", body)
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotPrincipal.ID != "agent_1" || gotPrincipal.RealmID != "realm_1" ||
		gotInput.EventID != "evt_1" || gotInput.EventOccurredAt.IsZero() {
		t.Fatalf("callback principal/input = %#v / %#v", gotPrincipal, gotInput)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"event_id", "event_occurred_at", "availability", "session_id"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("public response exposed %q: %s", forbidden, encoded)
		}
	}
	activity, ok := raw["activity"].(map[string]any)
	if !ok || activity["last_activity_at"] != observedAt.Format(time.RFC3339) ||
		activity["last_runtime"] != "cursor" || activity["last_location"] != "home" ||
		activity["last_event"] != "AgentResponse" {
		t.Fatalf("activity response = %#v", raw)
	}
}

func TestTouchAgentActivityAuthorizationAndStrictInput(t *testing.T) {
	calls := 0
	server := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: agentActivityTestAuth,
		TouchAgentActivity: func(_ context.Context, _ DomainPrincipal, in AgentActivityRequest) (AgentActivity, error) {
			calls++
			return AgentActivity{LastActivityAt: time.Now().UTC(), LastRuntime: in.Runtime, LastLocation: in.Location, LastEvent: in.Event}, nil
		},
	}))
	defer server.Close()

	valid := `{"runtime":"codex","location_id":"loc_1","location":"home","event":"SessionStart","event_id":"evt_1","event_occurred_at":"2026-07-15T19:59:59Z"}`
	queryResp := postAgentActivityURL(t, server.URL+"/v1/self/activity?account=other&realm=other&agent=other", "agent-token", valid)
	defer closeBody(t, queryResp)
	if queryResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("identity-selector query status = %d, want 400", queryResp.StatusCode)
	}
	tests := []struct {
		name, token, body string
		want              int
	}{
		{name: "missing token", body: valid, want: http.StatusUnauthorized},
		{name: "operator", token: "operator-token", body: valid, want: http.StatusForbidden},
		{name: "restricted profile", token: "curator-token", body: valid, want: http.StatusForbidden},
		{name: "suspended", token: "suspended-token", body: valid, want: http.StatusForbidden},
		{name: "future runtime", token: "agent-token", body: strings.Replace(valid, "codex", "gemini", 1), want: http.StatusOK},
		{name: "optional location", token: "agent-token", body: strings.Replace(valid, `"location":"home"`, `"location":""`, 1), want: http.StatusOK},
		{name: "control in event", token: "agent-token", body: strings.Replace(valid, "SessionStart", `Session\u000aStart`, 1), want: http.StatusBadRequest},
		{name: "missing runtime", token: "agent-token", body: strings.Replace(valid, `"runtime":"codex"`, `"runtime":""`, 1), want: http.StatusBadRequest},
		{name: "timestamp injection", token: "agent-token", body: strings.TrimSuffix(valid, "}") + `,"last_activity_at":"2027-01-01T00:00:00Z"}`, want: http.StatusBadRequest},
		{name: "availability injection", token: "agent-token", body: strings.TrimSuffix(valid, "}") + `,"availability":"online"}`, want: http.StatusBadRequest},
		{name: "missing ordering time", token: "agent-token", body: strings.Replace(valid, `"event_occurred_at":"2026-07-15T19:59:59Z"`, `"event_occurred_at":"0001-01-01T00:00:00Z"`, 1), want: http.StatusBadRequest},
		{name: "valid", token: "agent-token", body: valid, want: http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp := postAgentActivity(t, server.URL, test.token, test.body)
			defer closeBody(t, resp)
			if resp.StatusCode != test.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, test.want)
			}
		})
	}
	if calls != 3 {
		t.Fatalf("touch callback calls = %d, want 3", calls)
	}
}

func agentActivityTestAuth(_ context.Context, token string) (DomainPrincipal, bool, error) {
	switch token {
	case "agent-token":
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, true, nil
	case "operator-token":
		return DomainPrincipal{Kind: PrincipalKindOperator, ID: "operator_1", AccountID: "acc_1", AccountStatus: "active"}, true, nil
	case "curator-token":
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active", AccessProfile: AccessProfileCuratorApply}, true, nil
	case "suspended-token":
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "suspended"}, true, nil
	default:
		return DomainPrincipal{}, false, nil
	}
}

func postAgentActivity(t *testing.T, endpoint, token, body string) *http.Response {
	t.Helper()
	return postAgentActivityURL(t, endpoint+"/v1/self/activity", token, body)
}

func postAgentActivityURL(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
