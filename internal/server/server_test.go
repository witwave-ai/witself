package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthProbes(t *testing.T) {
	srv := httptest.NewServer(healthMux(nil))
	defer srv.Close()
	for _, p := range []string{"/livez", "/readyz", "/startupz", "/healthz"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestReadyzReflectsReadiness(t *testing.T) {
	// Failing readiness check -> 503, but liveness stays 200.
	down := httptest.NewServer(healthMux(func(context.Context) error { return errors.New("db down") }))
	defer down.Close()
	if code := getStatus(t, down.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz with failing check = %d, want 503", code)
	}
	if code := getStatus(t, down.URL+"/livez"); code != http.StatusOK {
		t.Errorf("/livez = %d, want 200 (must not gate on readiness)", code)
	}

	// Passing readiness check -> 200.
	up := httptest.NewServer(healthMux(func(context.Context) error { return nil }))
	defer up.Close()
	if code := getStatus(t, up.URL+"/readyz"); code != http.StatusOK {
		t.Errorf("/readyz with passing check = %d, want 200", code)
	}
}

func TestWhoamiAuth(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, bool, error) {
		if tok == "good" {
			return "opr_x", "acc_y", true, nil
		}
		return "", "", false, nil
	}
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth}))
	defer srv.Close()

	if code := getStatus(t, srv.URL+"/v1/whoami"); code != http.StatusUnauthorized {
		t.Errorf("/v1/whoami with no token = %d, want 401", code)
	}

	get := func(tok string) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		return http.DefaultClient.Do(req)
	}

	resp, err := get("bad")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/v1/whoami with bad token = %d, want 401", resp.StatusCode)
	}

	resp, err = get("good")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/whoami with good token = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Principal struct {
			Kind       string `json:"kind"`
			OperatorID string `json:"operator_id"`
			AccountID  string `json:"account_id"`
		} `json:"principal"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Principal.Kind != "operator" || out.Principal.OperatorID != "opr_x" || out.Principal.AccountID != "acc_y" {
		t.Errorf("principal = %+v", out.Principal)
	}
}

func TestRealmsCreateAndList(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, bool, error) {
		if tok == "good" {
			return "opr_x", "acc_y", true, nil
		}
		return "", "", false, nil
	}
	var created []Realm
	create := func(_ context.Context, _, name string) (Realm, error) {
		r := Realm{ID: "realm_" + name, Name: name}
		created = append(created, r)
		return r, nil
	}
	list := func(_ context.Context, _ string) ([]Realm, error) { return created, nil }
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth, CreateRealm: create, ListRealms: list}))
	defer srv.Close()

	post := func(tok, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/realms", strings.NewReader(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := post("", `{"name":"prod"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("create without token = %d, want 401", resp.StatusCode)
	}

	resp = post("good", `{"name":"prod"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("create = %d, want 201", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/realms", nil)
	req.Header.Set("Authorization", "Bearer good")
	lresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer lresp.Body.Close()
	var out struct {
		Realms []Realm `json:"realms"`
	}
	if err := json.NewDecoder(lresp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Realms) != 1 || out.Realms[0].Name != "prod" {
		t.Errorf("realms = %+v", out.Realms)
	}
}

func TestAgentsCreateAndList(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, bool, error) {
		if tok == "good" {
			return "opr_x", "acc_y", true, nil
		}
		return "", "", false, nil
	}
	var created []Agent
	create := func(_ context.Context, _, realmID, name string) (Agent, error) {
		if realmID == "missing" {
			return Agent{}, ErrNotFound
		}
		a := Agent{ID: "agent_" + name, Name: name}
		created = append(created, a)
		return a, nil
	}
	list := func(_ context.Context, _, _ string) ([]Agent, error) { return created, nil }
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth, CreateAgent: create, ListAgents: list}))
	defer srv.Close()

	post := func(realm, tok, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/realms/"+realm+"/agents", strings.NewReader(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	r := post("realm_1", "", `{"name":"a1"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", r.StatusCode)
	}
	r = post("missing", "good", `{"name":"a1"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("missing realm = %d, want 404", r.StatusCode)
	}
	r = post("realm_1", "good", `{"name":"a1"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Errorf("create = %d, want 201", r.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/realms/realm_1/agents", nil)
	req.Header.Set("Authorization", "Bearer good")
	lresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer lresp.Body.Close()
	var out struct {
		Agents []Agent `json:"agents"`
	}
	if err := json.NewDecoder(lresp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Agents) != 1 || out.Agents[0].Name != "a1" {
		t.Errorf("agents = %+v", out.Agents)
	}
}

func TestAgentTokenCreate(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, bool, error) {
		if tok == "good" {
			return "opr_x", "acc_y", true, nil
		}
		return "", "", false, nil
	}
	create := func(_ context.Context, _, agentID string) (string, error) {
		if agentID == "missing" {
			return "", ErrNotFound
		}
		return "witself_agt_minted", nil
	}
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth, CreateAgentToken: create}))
	defer srv.Close()

	post := func(agent, tok string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/agents/"+agent+"/tokens", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	r := post("agent_1", "")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", r.StatusCode)
	}
	r = post("missing", "good")
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("missing agent = %d, want 404", r.StatusCode)
	}
	r = post("agent_1", "good")
	defer r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", r.StatusCode)
	}
	var out struct {
		AgentToken string `json:"agent_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.AgentToken != "witself_agt_minted" {
		t.Errorf("agent_token = %q", out.AgentToken)
	}
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestMetricsExposesUp(t *testing.T) {
	srv := httptest.NewServer(metricsMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "witself_up 1") {
		t.Errorf("metrics missing witself_up gauge:\n%s", body)
	}
}

func TestVersionEndpointIsBare(t *testing.T) {
	srv := httptest.NewServer(apiMux(Config{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	// Bare shape: schema_version + version/commit/date at the top level.
	for _, k := range []string{"schema_version", "version", "commit", "date"} {
		if _, ok := m[k]; !ok {
			t.Errorf("version missing %q; got %v", k, m)
		}
	}
	if _, enveloped := m["data"]; enveloped {
		t.Errorf("version should be bare, found envelope data field: %v", m)
	}
}

func TestCapabilitiesShape(t *testing.T) {
	t.Setenv("WITSELF_BACKEND_KIND", "self-hosted")
	srv := httptest.NewServer(apiMux(Config{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var c capabilities
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.SchemaVersion != "witself.v0" {
		t.Errorf("schema_version = %q", c.SchemaVersion)
	}
	if c.Backend.Kind != "self-hosted" || c.Backend.APIVersion != "v1" {
		t.Errorf("backend = %+v", c.Backend)
	}
	if f, ok := c.Features["memories"]; !ok || f.Supported || f.Reason != "not_implemented" {
		t.Errorf("memories feature = %+v (ok=%v)", f, ok)
	}
	if c.Account != nil {
		t.Errorf("account should be omitted when no account id is set, got %+v", c.Account)
	}
}

func TestCapabilitiesIncludesAccount(t *testing.T) {
	srv := httptest.NewServer(apiMux(Config{AccountID: "acc_test123"}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var c capabilities
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.Account == nil || c.Account.ID != "acc_test123" {
		t.Errorf("account = %+v, want id acc_test123", c.Account)
	}
}
