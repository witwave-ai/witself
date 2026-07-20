package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func dashboardPreferencesTestConfig(t *testing.T, stored *DashboardPreferences) (Config, *PutDashboardPreferencesRequest) {
	t.Helper()
	var putInput PutDashboardPreferencesRequest
	cfg := Config{
		AuthenticatePrincipal: secretTestAuth,
		GetDashboardPreferences: func(_ context.Context, p DomainPrincipal) (*DashboardPreferences, error) {
			assertSecretTestPrincipal(t, p)
			return stored, nil
		},
		PutDashboardPreferences: func(_ context.Context, p DomainPrincipal, in PutDashboardPreferencesRequest) (DashboardPreferences, error) {
			assertSecretTestPrincipal(t, p)
			putInput = in
			return DashboardPreferences{
				AgentID:   p.ID,
				Prefs:     in.Prefs,
				UpdatedAt: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
			}, nil
		},
	}
	return cfg, &putInput
}

func TestDashboardPreferencesRoutes(t *testing.T) {
	row := &DashboardPreferences{
		AgentID:   "agent_1",
		Prefs:     json.RawMessage(`{"schema":"witself.dashboard-prefs.v1","theme":"amber"}`),
		UpdatedAt: time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC),
	}
	cfg, putInput := dashboardPreferencesTestConfig(t, row)
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	t.Run("get returns the stored row", func(t *testing.T) {
		resp := secretTestRequest(t, srv.URL, http.MethodGet, "/v1/self/dashboard-preferences", "agent-token", "", "")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var out struct {
			Preferences *DashboardPreferences `json:"preferences"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if out.Preferences == nil || out.Preferences.AgentID != "agent_1" ||
			string(out.Preferences.Prefs) != string(row.Prefs) {
			t.Fatalf("preferences = %#v", out.Preferences)
		}
	})

	t.Run("get rejects query parameters", func(t *testing.T) {
		resp := secretTestRequest(t, srv.URL, http.MethodGet, "/v1/self/dashboard-preferences?agent=other", "agent-token", "", "")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("put forwards the prefs document", func(t *testing.T) {
		body := `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"midnight"}}`
		resp := secretTestRequest(t, srv.URL, http.MethodPut, "/v1/self/dashboard-preferences", "agent-token", body, "")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if string(putInput.Prefs) != `{"schema":"witself.dashboard-prefs.v1","theme":"midnight"}` {
			t.Fatalf("forwarded prefs = %s", putInput.Prefs)
		}
	})

	t.Run("put strict 400s", func(t *testing.T) {
		cases := map[string]string{
			"not JSON":             `nonsense`,
			"unknown envelope key": `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"console"},"extra":1}`,
			"missing prefs":        `{}`,
			"trailing JSON":        `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"console"}}{}`,
			"oversized body": `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"console","pad":"` +
				strings.Repeat("a", int(maxDashboardPreferencesRequestBytes)) + `"}}`,
		}
		for name, body := range cases {
			t.Run(name, func(t *testing.T) {
				resp := secretTestRequest(t, srv.URL, http.MethodPut, "/v1/self/dashboard-preferences", "agent-token", body, "")
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400", resp.StatusCode)
				}
			})
		}
	})

	t.Run("put maps store validation to 400", func(t *testing.T) {
		invalidCfg, _ := dashboardPreferencesTestConfig(t, nil)
		invalidCfg.PutDashboardPreferences = func(context.Context, DomainPrincipal, PutDashboardPreferencesRequest) (DashboardPreferences, error) {
			return DashboardPreferences{}, fmt.Errorf("%w: unknown key", ErrBadInput)
		}
		invalidSrv := httptest.NewServer(apiMux(invalidCfg))
		defer invalidSrv.Close()
		resp := secretTestRequest(t, invalidSrv.URL, http.MethodPut, "/v1/self/dashboard-preferences",
			"agent-token", `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"console","x":1}}`, "")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestDashboardPreferencesRoutesAgentAuthOnly(t *testing.T) {
	cfg, _ := dashboardPreferencesTestConfig(t, nil)
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	body := `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"console"}}`
	cases := []struct {
		name   string
		method string
		token  string
		body   string
		want   int
	}{
		{"get without token", http.MethodGet, "", "", http.StatusUnauthorized},
		{"put without token", http.MethodPut, "", body, http.StatusUnauthorized},
		{"get with operator token", http.MethodGet, "operator-token", "", http.StatusForbidden},
		{"put with operator token", http.MethodPut, "operator-token", body, http.StatusForbidden},
		{"get with curator profile", http.MethodGet, "curator-token", "", http.StatusForbidden},
		{"put with curator profile", http.MethodPut, "curator-token", body, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := secretTestRequest(t, srv.URL, tc.method, "/v1/self/dashboard-preferences", tc.token, tc.body, "")
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestDashboardPreferencesGetDefaultsToNull(t *testing.T) {
	cfg, _ := dashboardPreferencesTestConfig(t, nil)
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	resp := secretTestRequest(t, srv.URL, http.MethodGet, "/v1/self/dashboard-preferences", "agent-token", "", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if string(out["preferences"]) != "null" {
		t.Fatalf("preferences = %s, want null", out["preferences"])
	}
}
