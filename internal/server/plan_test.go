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
func planTestServer(
	t *testing.T,
	setPlan func(
		ctx context.Context,
		accountID string,
		revision int64,
		snapshotHash, plan string,
		limits, policies map[string]int64,
		features []string,
	) (PlanSnapshotRecord, error),
) *httptest.Server {
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
	var gotRevision int64
	var gotHash string
	var gotLimits map[string]int64
	var gotPolicies map[string]int64
	var gotFeatures []string
	srv := planTestServer(t, func(
		_ context.Context,
		accountID string,
		revision int64,
		snapshotHash, plan string,
		limits, policies map[string]int64,
		features []string,
	) (PlanSnapshotRecord, error) {
		if accountID == "acct_missing" {
			return PlanSnapshotRecord{}, ErrNotFound
		}
		if accountID == "acct_stale" {
			return PlanSnapshotRecord{}, ErrConflict
		}
		gotRevision, gotHash = revision, snapshotHash
		gotAccount, gotPlan, gotLimits, gotPolicies, gotFeatures = accountID, plan, limits, policies, features
		return PlanSnapshotRecord{
			AccountID: accountID, Revision: revision, SnapshotHash: snapshotHash,
			Plan: plan, Limits: limits, Policies: policies, Features: features,
		}, nil
	})

	// Happy path: the control plane applies a snapshot.
	hash := strings.Repeat("a", 64)
	resp := postPlan(t, srv.URL+"/v1/accounts/acct_1:plan", "witself_prv_test",
		`{"revision":7,"snapshot_hash":"`+hash+`","plan":"standard","limits":{"agents":250,"realms":10},"policies":{"transcript_retention_days":90},"features":["memory","facts","secrets","collaboration","support"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	if gotAccount != "acct_1" || gotPlan != "standard" || gotLimits["agents"] != 250 || gotLimits["realms"] != 10 {
		t.Fatalf("callback got (%q, %q, %v); want the applied snapshot", gotAccount, gotPlan, gotLimits)
	}
	if gotRevision != 7 || gotHash != hash {
		t.Fatalf("fence = %d/%q; want 7/%q", gotRevision, gotHash, hash)
	}
	if len(gotFeatures) != 5 || gotFeatures[2] != "secrets" {
		t.Fatalf("features = %v; want the 5 standard features", gotFeatures)
	}
	if gotPolicies["transcript_retention_days"] != 90 {
		t.Fatalf("policies = %v; want transcript retention 90", gotPolicies)
	}
	var body struct {
		Revision int64            `json:"revision"`
		Hash     string           `json:"snapshot_hash"`
		Plan     string           `json:"plan"`
		Limits   map[string]int64 `json:"limits"`
		Policies map[string]int64 `json:"policies"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil ||
		body.Revision != 7 || body.Hash != hash || body.Plan != "standard" ||
		body.Limits["agents"] != 250 || body.Policies["transcript_retention_days"] != 90 {
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
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_1:plan", "witself_prv_test",
		`{"plan":"free","policies":{"transcript_retention_days":0}}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("zero retention status = %d; want 400", resp.StatusCode)
	}
	// Unknown account -> 404.
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_missing:plan", "witself_prv_test", `{"plan":"free"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown account status = %d; want 404", resp.StatusCode)
	}
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_stale:plan", "witself_prv_test", `{"plan":"free"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale snapshot status = %d; want 409", resp.StatusCode)
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
	planInfo := func(context.Context) (string, map[string]int64, map[string]int64, []string, error) {
		return "standard", map[string]int64{"agents": 250, "realms": 10},
			map[string]int64{"transcript_retention_days": 90},
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
	policies := account["plan_policies"].(map[string]any)
	if policies["transcript_retention_days"] != float64(90) {
		t.Fatalf("plan_policies = %v; want transcript retention 90", policies)
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
