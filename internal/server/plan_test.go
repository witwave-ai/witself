package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// planTestServer builds a test server whose lifecycle route is enabled (the
// provisioning pair) with a recording SetAccountPlan.
func planTestServer(t *testing.T, setPlan func(ctx context.Context, accountID, plan string, limits map[string]int64, features []string) error) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(apiMux(Config{
		ProvisionToken: "witself_prv_test",
		ProvisionAccount: func(context.Context, string, string) (ProvisionedAccount, error) {
			return ProvisionedAccount{}, nil
		},
		SetAccountPlan: setPlan,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func postPlan(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestSetAccountPlanEndpoint(t *testing.T) {
	var gotAccount, gotPlan string
	var gotLimits map[string]int64
	var gotFeatures []string
	srv := planTestServer(t, func(_ context.Context, accountID, plan string, limits map[string]int64, features []string) error {
		if accountID == "acct_missing" {
			return ErrNotFound
		}
		gotAccount, gotPlan, gotLimits, gotFeatures = accountID, plan, limits, features
		return nil
	})

	// Happy path: the control plane applies a snapshot.
	resp := postPlan(t, srv.URL+"/v1/accounts/acct_1:plan", "witself_prv_test",
		`{"plan":"standard","limits":{"agents":250,"realms":10},"features":["memory","facts","secrets","collaboration","support"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	if gotAccount != "acct_1" || gotPlan != "standard" || gotLimits["agents"] != 250 || gotLimits["realms"] != 10 {
		t.Fatalf("callback got (%q, %q, %v); want the applied snapshot", gotAccount, gotPlan, gotLimits)
	}
	if len(gotFeatures) != 5 || gotFeatures[2] != "secrets" {
		t.Fatalf("features = %v; want the 5 standard features", gotFeatures)
	}
	var body struct {
		Plan   string           `json:"plan"`
		Limits map[string]int64 `json:"limits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Plan != "standard" || body.Limits["agents"] != 250 {
		t.Fatalf("response body = %+v, %v; want the snapshot echoed", body, err)
	}

	// Wrong token -> 401 (constant-time provision check).
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_1:plan", "witself_prv_WRONG", `{"plan":"standard"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d; want 401", resp.StatusCode)
	}
	// Missing plan -> 400.
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_1:plan", "witself_prv_test", `{"limits":{"agents":1}}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing plan status = %d; want 400", resp.StatusCode)
	}
	// Unknown account -> 404.
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_missing:plan", "witself_prv_test", `{"plan":"free"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown account status = %d; want 404", resp.StatusCode)
	}
}

func TestCreateRealmPlanLimit(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "good" {
			return "opr_x", "acc_y", "active", true, nil
		}
		return "", "", "", false, nil
	}
	create := func(context.Context, string, string) (Realm, error) {
		return Realm{}, fmt.Errorf("%w: realms 1/1 on the free plan", ErrPlanLimit)
	}
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth, CreateRealm: create}))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("POST", srv.URL+"/v1/realms", strings.NewReader(`{"name":"second"}`))
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || !strings.Contains(body.Error, "realms 1/1 on the free plan") {
		t.Fatalf("error body = %+v, %v; the refusal must explain itself", body, err)
	}
}

func TestCreateAgentPlanLimit(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		return "opr_x", "acc_y", "active", tok == "good", nil
	}
	create := func(context.Context, string, string, string) (Agent, error) {
		return Agent{}, fmt.Errorf("%w: agents 25/25 on the free plan", ErrPlanLimit)
	}
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth, CreateAgent: create}))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("POST", srv.URL+"/v1/realms/realm_1/agents", strings.NewReader(`{"name":"a26"}`))
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || !strings.Contains(body.Error, "agents 25/25") {
		t.Fatalf("error body = %+v, %v; the refusal must explain itself", body, err)
	}
}

func TestCapabilitiesPlanAndBilling(t *testing.T) {
	planInfo := func(context.Context) (string, map[string]int64, []string, error) {
		return "standard", map[string]int64{"agents": 250, "realms": 10},
			[]string{"memory", "facts", "secrets", "collaboration", "support"}, nil
	}
	srv := httptest.NewServer(apiMux(Config{AccountID: "acct_default", PlanInfo: planInfo}))
	t.Cleanup(srv.Close)

	get := func() map[string]any {
		t.Helper()
		resp, err := http.Get(srv.URL + "/v1/capabilities")
		if err != nil {
			t.Fatalf("GET capabilities: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var doc map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return doc
	}

	// Default: plan surfaced, billing not configured -> self_hosted.
	doc := get()
	account := doc["account"].(map[string]any)
	if account["plan"] != "standard" {
		t.Fatalf("account.plan = %v; want standard", account["plan"])
	}
	limits := doc["limits"].(map[string]any)
	if limits["agents"].(float64) != 250 || limits["realms"].(float64) != 10 {
		t.Fatalf("limits = %v; want the snapshot", limits)
	}
	features := account["plan_features"].([]any)
	if len(features) != 5 {
		t.Fatalf("plan_features = %v; want 5 entries", features)
	}
	billing := doc["billing"].(map[string]any)
	if billing["supported"].(bool) || billing["reason"] != "self_hosted" {
		t.Fatalf("billing = %v; want unsupported/self_hosted by default", billing)
	}

	// With the deployment config set, capabilities advertises the endpoint.
	t.Setenv("WITSELF_BILLING_ENDPOINT", "https://cp.example/v1")
	billing = get()["billing"].(map[string]any)
	if !billing["supported"].(bool) || billing["endpoint"] != "https://cp.example/v1" {
		t.Fatalf("billing = %v; want supported with the configured endpoint", billing)
	}
}
