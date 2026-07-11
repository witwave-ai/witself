package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/placement"
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
		closeBody(t, resp)
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
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "good" {
			return "opr_x", "acc_y", "active", true, nil
		}
		return "", "", "", false, nil
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
	closeBody(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/v1/whoami with bad token = %d, want 401", resp.StatusCode)
	}

	resp, err = get("good")
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
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

func TestAuthWhoamiAlias(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "good" {
			return "opr_x", "acc_y", "active", true, nil
		}
		return "", "", "", false, nil
	}
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/auth/whoami", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/auth/whoami = %d, want 200", resp.StatusCode)
	}
}

func TestPlacementPolicyEndpoints(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "good" {
			return "opr_owner", "acc_owner", "active", true, nil
		}
		return "", "", "", false, nil
	}
	policy := placement.DefaultPolicy()
	var saved placement.Policy
	srv := httptest.NewServer(apiMux(Config{
		Authenticate: auth,
		GetPlacementPolicy: func(_ context.Context, accountID, operatorID string) (placement.Policy, error) {
			if accountID != "acc_owner" || operatorID != "opr_owner" {
				t.Fatalf("get principal = %s/%s", accountID, operatorID)
			}
			return policy, nil
		},
		SetPlacementPolicy: func(_ context.Context, accountID, operatorID string, next placement.Policy) (placement.Policy, error) {
			if accountID != "acc_owner" || operatorID != "opr_owner" {
				t.Fatalf("set principal = %s/%s", accountID, operatorID)
			}
			saved = next
			return next, nil
		},
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/account/placement-policy", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		PlacementPolicy placement.Policy `json:"placement_policy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusOK || got.PlacementPolicy.PreferredRegions[0] != "usw2" {
		t.Fatalf("GET placement = %d %#v", resp.StatusCode, got.PlacementPolicy)
	}

	req, _ = http.NewRequest(http.MethodPatch, srv.URL+"/v1/account/placement-policy", strings.NewReader(`{"preferred_clouds":["gcp"],"preferred_regions":["use1"],"preferred_channels":["stable"],"allowed_clouds":[],"allowed_regions":[],"allowed_channels":[],"rebalance_on":["cloud"]}`))
	req.Header.Set("Authorization", "Bearer good")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH placement = %d, want 200", resp.StatusCode)
	}
	if len(saved.PreferredClouds) != 1 || saved.PreferredClouds[0] != "gcp" {
		t.Fatalf("saved policy = %#v", saved)
	}
}

func TestPlacementPolicySystemEndpoint(t *testing.T) {
	provision := func(_ context.Context, _, _ string) (ProvisionedAccount, error) {
		return ProvisionedAccount{}, errors.New("unused")
	}
	var saved placement.Policy
	srv := httptest.NewServer(apiMux(Config{
		ProvisionToken:   "witself_prv_test",
		ProvisionAccount: provision,
		GetPlacementPolicySystem: func(_ context.Context, accountID string) (placement.Policy, error) {
			if accountID != "acc_1" {
				t.Fatalf("accountID = %q, want acc_1", accountID)
			}
			return placement.Policy{PreferredClouds: []string{"gcp"}}, nil
		},
		SetPlacementPolicySystem: func(_ context.Context, accountID string, policy placement.Policy) (placement.Policy, error) {
			if accountID != "acc_1" {
				t.Fatalf("accountID = %q, want acc_1", accountID)
			}
			saved = policy
			return policy, nil
		},
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/accounts/acc_1/placement-policy", nil)
	req.Header.Set("Authorization", "Bearer witself_prv_test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		AccountID       string           `json:"account_id"`
		PlacementPolicy placement.Policy `json:"placement_policy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET system placement policy = %d, want 200", resp.StatusCode)
	}
	if out.AccountID != "acc_1" || len(out.PlacementPolicy.PreferredClouds) != 1 || out.PlacementPolicy.PreferredClouds[0] != "gcp" {
		t.Fatalf("response = %#v", out)
	}

	req, _ = http.NewRequest(http.MethodPatch, srv.URL+"/v1/accounts/acc_1/placement-policy", strings.NewReader(`{"preferred_clouds":[],"preferred_regions":["usw2","use1"],"preferred_channels":["stable","edge","experimental"],"allowed_clouds":[],"allowed_regions":[],"allowed_channels":[],"rebalance_on":["cloud","channel"]}`))
	req.Header.Set("Authorization", "Bearer witself_prv_test")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH system placement policy = %d, want 200", resp.StatusCode)
	}
	if len(saved.AllowedRegions) != 0 || len(saved.PreferredRegions) != 2 || saved.PreferredRegions[0] != "usw2" {
		t.Fatalf("saved policy = %#v", saved)
	}
}

func TestRealmsCreateAndList(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "good" {
			return "opr_x", "acc_y", "active", true, nil
		}
		return "", "", "", false, nil
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
	closeBody(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("create without token = %d, want 401", resp.StatusCode)
	}

	resp = post("good", `{"name":"prod"}`)
	closeBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("create = %d, want 201", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/realms", nil)
	req.Header.Set("Authorization", "Bearer good")
	lresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, lresp)
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
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "good" {
			return "opr_x", "acc_y", "active", true, nil
		}
		return "", "", "", false, nil
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
	closeBody(t, r)
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", r.StatusCode)
	}
	r = post("missing", "good", `{"name":"a1"}`)
	closeBody(t, r)
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("missing realm = %d, want 404", r.StatusCode)
	}
	r = post("realm_1", "good", `{"name":"a1"}`)
	closeBody(t, r)
	if r.StatusCode != http.StatusCreated {
		t.Errorf("create = %d, want 201", r.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/realms/realm_1/agents", nil)
	req.Header.Set("Authorization", "Bearer good")
	lresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, lresp)
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
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "good" {
			return "opr_x", "acc_y", "active", true, nil
		}
		return "", "", "", false, nil
	}
	create := func(_ context.Context, accountID, actorOperatorID, agentID string) (string, string, string, error) {
		// Same shape as the sibling tests (create-operator, revoke-
		// token): assert the handler passes the AUTHENTICATED principal
		// through as the actor, so a future arg-swap that fires the
		// audit event with the wrong id is caught here.
		if accountID != "acc_y" {
			return "", "", "", fmt.Errorf("create accountID = %q, want acc_y", accountID)
		}
		if actorOperatorID != "opr_x" {
			return "", "", "", fmt.Errorf("create actorOperatorID = %q, want opr_x", actorOperatorID)
		}
		if agentID == "missing" {
			return "", "", "", ErrNotFound
		}
		return "witself_agt_minted", "tok_agent", "my-agent", nil
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
	closeBody(t, r)
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", r.StatusCode)
	}
	r = post("missing", "good")
	closeBody(t, r)
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("missing agent = %d, want 404", r.StatusCode)
	}
	r = post("agent_1", "good")
	defer closeBody(t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", r.StatusCode)
	}
	var out struct {
		AgentToken string `json:"agent_token"`
		TokenID    string `json:"token_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.AgentToken != "witself_agt_minted" || out.TokenID != "tok_agent" {
		t.Errorf("agent token response = %+v", out)
	}
}

func TestOperatorTokenCreate(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "good" {
			return "opr_x", "acc_y", "active", true, nil
		}
		return "", "", "", false, nil
	}
	create := func(_ context.Context, accountID, operatorID, displayName string, ttl *time.Duration) (string, string, *time.Time, error) {
		if accountID != "acc_y" || operatorID != "opr_x" {
			t.Fatalf("create principal = account %q operator %q", accountID, operatorID)
		}
		if displayName != "deploy bot" {
			t.Fatalf("displayName = %q, want deploy bot", displayName)
		}
		var expiresAt *time.Time
		if ttl != nil {
			tm := time.Date(2026, 7, 2, 1, 2, 3, 0, time.UTC)
			expiresAt = &tm
		}
		return "witself_opr_minted", "tok_operator", expiresAt, nil
	}
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth, CreateOperatorToken: create}))
	defer srv.Close()

	post := func(tok, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/operators/self/tokens", strings.NewReader(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	r := post("", `{}`)
	closeBody(t, r)
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", r.StatusCode)
	}
	r = post("good", `{"ttl":"0s"}`)
	closeBody(t, r)
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid ttl = %d, want 400", r.StatusCode)
	}
	r = post("good", `{"display_name":"deploy bot","ttl":"24h"}`)
	defer closeBody(t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", r.StatusCode)
	}
	var out struct {
		OperatorToken string `json:"operator_token"`
		OperatorID    string `json:"operator_id"`
		TokenID       string `json:"token_id"`
		DisplayName   string `json:"display_name"`
		ExpiresAt     string `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.OperatorToken != "witself_opr_minted" || out.OperatorID != "opr_x" || out.TokenID != "tok_operator" || out.DisplayName != "deploy bot" || out.ExpiresAt == "" {
		t.Errorf("operator token response = %+v", out)
	}
}

func TestOperatorTokenCreateMintsTokenThatCanAuthenticate(t *testing.T) {
	valid := map[string]bool{"parent": true}
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if valid[tok] {
			return "opr_x", "acc_y", "active", true, nil
		}
		return "", "", "", false, nil
	}
	create := func(_ context.Context, accountID, operatorID, _ string, _ *time.Duration) (string, string, *time.Time, error) {
		if accountID != "acc_y" || operatorID != "opr_x" {
			t.Fatalf("create principal = account %q operator %q", accountID, operatorID)
		}
		valid["child"] = true
		return "child", "tok_child", nil, nil
	}
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth, CreateOperatorToken: create}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/operators/self/tokens", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer parent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint token = %d, want 201", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/auth/whoami", nil)
	req.Header.Set("Authorization", "Bearer child")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("whoami with minted token = %d, want 200", resp.StatusCode)
	}
}

func TestOperatorTokenCreateRejectsNonOperatorCallers(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		switch tok {
		case "good":
			return "opr_x", "acc_y", "active", true, nil
		case "agent", "consumed", "expired", "invalid":
			return "", "", "", false, nil
		default:
			return "", "", "", false, nil
		}
	}
	create := func(context.Context, string, string, string, *time.Duration) (string, string, *time.Time, error) {
		t.Fatal("create should not be called")
		return "", "", nil, nil
	}
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth, CreateOperatorToken: create}))
	defer srv.Close()

	for _, tok := range []string{"agent", "consumed", "expired", "invalid"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/operators/self/tokens", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		closeBody(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s token create = %d, want 401", tok, resp.StatusCode)
		}
	}
}

func TestOperatorsListCreateAndDelete(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "good" {
			return "opr_root", "acc_y", "active", true, nil
		}
		return "", "", "", false, nil
	}
	now := time.Date(2026, 7, 2, 1, 2, 3, 0, time.UTC)
	operators := []Operator{{
		ID:          "opr_root",
		DisplayName: "owner",
		Role:        "account_owner",
		IsRoot:      true,
		CreatedAt:   now,
		UpdatedAt:   now,
		Tokens: []OperatorToken{{
			ID:          "tok_root",
			DisplayName: "laptop",
			CreatedAt:   now,
		}},
	}}
	list := func(_ context.Context, accountID string) ([]Operator, error) {
		if accountID != "acc_y" {
			t.Fatalf("list account = %q", accountID)
		}
		return operators, nil
	}
	create := func(_ context.Context, accountID, actorOperatorID, displayName, tokenDisplayName string, ttl *time.Duration) (Operator, string, *time.Time, error) {
		if actorOperatorID != "opr_root" {
			t.Fatalf("create actorOperatorID = %q, want opr_root", actorOperatorID)
		}
		if accountID != "acc_y" || displayName != "deploy bot" || tokenDisplayName != "deploy token" {
			t.Fatalf("create args account=%q display=%q tokenDisplay=%q", accountID, displayName, tokenDisplayName)
		}
		if ttl == nil || *ttl != 24*time.Hour {
			t.Fatalf("ttl = %v, want 24h", ttl)
		}
		op := Operator{
			ID:          "opr_deploy",
			DisplayName: displayName,
			Role:        "account_operator",
			CreatedAt:   now,
			UpdatedAt:   now,
			Tokens: []OperatorToken{{
				ID:          "tok_deploy",
				DisplayName: tokenDisplayName,
				CreatedAt:   now,
			}},
		}
		operators = append(operators, op)
		return op, "witself_opr_deploy", nil, nil
	}
	var deleted string
	deleteOperator := func(_ context.Context, accountID, actorOperatorID, targetOperatorID string) error {
		if accountID != "acc_y" || actorOperatorID != "opr_root" {
			t.Fatalf("delete principal account=%q actor=%q", accountID, actorOperatorID)
		}
		deleted = targetOperatorID
		return nil
	}
	srv := httptest.NewServer(apiMux(Config{
		Authenticate:   auth,
		ListOperators:  list,
		CreateOperator: create,
		DeleteOperator: deleteOperator,
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/operators", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list operators = %d, want 200", resp.StatusCode)
	}
	var listed struct {
		Operators []Operator `json:"operators"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Operators) != 1 || listed.Operators[0].Tokens[0].DisplayName != "laptop" {
		t.Fatalf("operators = %+v", listed.Operators)
	}

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/operators", strings.NewReader(`{"display_name":"deploy bot","token_display_name":"deploy token","ttl":"24h"}`))
	req.Header.Set("Authorization", "Bearer good")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create operator = %d, want 201", resp.StatusCode)
	}
	var created struct {
		Operator      Operator `json:"operator"`
		OperatorToken string   `json:"operator_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Operator.ID != "opr_deploy" || created.Operator.Tokens[0].ID != "tok_deploy" || created.OperatorToken != "witself_opr_deploy" {
		t.Fatalf("created = %+v", created)
	}

	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/v1/operators/opr_deploy", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete operator = %d, want 204", resp.StatusCode)
	}
	if deleted != "opr_deploy" {
		t.Fatalf("deleted = %q, want opr_deploy", deleted)
	}
}

func TestOperatorDeleteGuards(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "good" {
			return "opr_root", "acc_y", "active", true, nil
		}
		return "", "", "", false, nil
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"self", ErrCannotDeleteSelf, "cannot delete the authenticated operator"},
		{"root", ErrCannotDeleteRoot, "cannot delete the root operator"},
		{"last", ErrLastOperator, "cannot delete the last operator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deleteOperator := func(context.Context, string, string, string) error { return tc.err }
			srv := httptest.NewServer(apiMux(Config{Authenticate: auth, DeleteOperator: deleteOperator}))
			defer srv.Close()

			req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/operators/opr_x", nil)
			req.Header.Set("Authorization", "Bearer good")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(t, resp)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("delete operator = %d, want 409", resp.StatusCode)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Fatalf("body = %s, want %q", body, tc.want)
			}
		})
	}
}

func TestDeleteAndRevokeRoutes(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "good" {
			return "opr_x", "acc_y", "active", true, nil
		}
		return "", "", "", false, nil
	}
	var deletedRealm, deletedAgent, revokedToken string
	deleteRealm := func(_ context.Context, accountID, realmID string) error {
		if accountID != "acc_y" {
			t.Fatalf("realm account = %q", accountID)
		}
		deletedRealm = realmID
		return nil
	}
	deleteAgent := func(_ context.Context, accountID, realmID, agentID string) error {
		if accountID != "acc_y" || realmID != "realm_1" {
			t.Fatalf("agent delete account=%q realm=%q", accountID, realmID)
		}
		deletedAgent = agentID
		return nil
	}
	revoke := func(_ context.Context, accountID, actorOperatorID, tokenID string) error {
		if accountID != "acc_y" {
			t.Fatalf("revoke account = %q", accountID)
		}
		if actorOperatorID != "opr_x" {
			t.Fatalf("revoke actorOperatorID = %q, want opr_x", actorOperatorID)
		}
		revokedToken = tokenID
		return nil
	}
	srv := httptest.NewServer(apiMux(Config{
		Authenticate: auth,
		DeleteRealm:  deleteRealm,
		DeleteAgent:  deleteAgent,
		RevokeToken:  revoke,
	}))
	defer srv.Close()

	for _, req := range []*http.Request{
		mustRequest(t, http.MethodDelete, srv.URL+"/v1/realms/realm_1"),
		mustRequest(t, http.MethodDelete, srv.URL+"/v1/realms/realm_1/agents/agent_1"),
		mustRequest(t, http.MethodPost, srv.URL+"/v1/tokens/tok_1:revoke"),
	} {
		req.Header.Set("Authorization", "Bearer good")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		closeBody(t, resp)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s %s = %d, want 204", req.Method, req.URL.Path, resp.StatusCode)
		}
	}
	if deletedRealm != "realm_1" || deletedAgent != "agent_1" || revokedToken != "tok_1" {
		t.Fatalf("deletedRealm=%q deletedAgent=%q revokedToken=%q", deletedRealm, deletedAgent, revokedToken)
	}
}

func mustRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestProvisionAccount(t *testing.T) {
	provision := func(_ context.Context, email, _ string) (ProvisionedAccount, error) {
		if email == "taken@x.com" {
			return ProvisionedAccount{}, ErrConflict
		}
		return ProvisionedAccount{
			AccountID: "acc_new", OperatorID: "opr_new", Email: email,
			Status: "active", BootstrapToken: "witself_boot_x",
		}, nil
	}

	// Route absent entirely when no provision token is configured.
	bare := httptest.NewServer(apiMux(Config{ProvisionAccount: provision}))
	defer bare.Close()
	resp, err := http.Post(bare.URL+"/v1/accounts", "application/json", strings.NewReader(`{"email":"a@b.c"}`))
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unmounted provisioning = %d, want 404", resp.StatusCode)
	}

	srv := httptest.NewServer(apiMux(Config{ProvisionToken: "witself_prv_good", ProvisionAccount: provision}))
	defer srv.Close()
	post := func(tok, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/accounts", strings.NewReader(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	r := post("", `{"email":"a@b.c"}`)
	closeBody(t, r)
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", r.StatusCode)
	}
	r = post("witself_prv_bad", `{"email":"a@b.c"}`)
	closeBody(t, r)
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token = %d, want 401", r.StatusCode)
	}
	r = post("witself_prv_good", `{"email":"not-an-email"}`)
	closeBody(t, r)
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("bad email = %d, want 400", r.StatusCode)
	}
	r = post("witself_prv_good", `{"email":"taken@x.com"}`)
	closeBody(t, r)
	if r.StatusCode != http.StatusConflict {
		t.Errorf("duplicate email = %d, want 409", r.StatusCode)
	}
	r = post("witself_prv_good", `{"email":"Amy@Co.com"}`)
	defer closeBody(t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", r.StatusCode)
	}
	var out struct {
		Account ProvisionedAccount `json:"account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Account.AccountID != "acc_new" || out.Account.Email != "amy@co.com" || out.Account.BootstrapToken == "" {
		t.Errorf("account = %+v", out.Account)
	}
}

func TestCloseAccount(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		switch tok {
		case "owner":
			return "opr_owner", "acc_y", "active", true, nil
		case "member":
			return "opr_member", "acc_y", "active", true, nil
		case "root":
			return "opr_root", "acc_default", "active", true, nil
		}
		return "", "", "", false, nil
	}
	closeFn := func(_ context.Context, accountID, operatorID, _ string) error {
		if accountID == "acc_default" {
			return ErrCannotCloseDefault
		}
		if operatorID != "opr_owner" {
			return ErrNotAccountOwner
		}
		return nil
	}
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth, CloseAccount: closeFn}))
	defer srv.Close()

	post := func(tok string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/account:close", strings.NewReader(`{"reason":"test"}`))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	r := post("")
	closeBody(t, r)
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", r.StatusCode)
	}
	r = post("member")
	closeBody(t, r)
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("non-owner = %d, want 403", r.StatusCode)
	}
	r = post("root")
	closeBody(t, r)
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("default account = %d, want 403", r.StatusCode)
	}
	r = post("owner")
	defer closeBody(t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("owner close = %d, want 200", r.StatusCode)
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "closed" {
		t.Errorf("status = %q, want closed", out.Status)
	}
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	closeBody(t, resp)
	return resp.StatusCode
}

func TestMetricsExposesUp(t *testing.T) {
	srv := httptest.NewServer(metricsMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
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
	defer closeBody(t, resp)
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
	defer closeBody(t, resp)
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
	defer closeBody(t, resp)
	var c capabilities
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.Account == nil || c.Account.ID != "acc_test123" {
		t.Errorf("account = %+v, want id acc_test123", c.Account)
	}
}

func TestCapabilitiesReportsTranscriptSupport(t *testing.T) {
	principalAuth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{}, false, nil
	}
	cfg := Config{
		AuthenticatePrincipal: principalAuth,
		CreateTranscript: func(context.Context, DomainPrincipal, CreateTranscriptRequest) (Transcript, error) {
			return Transcript{}, nil
		},
		AppendTranscriptEntry: func(context.Context, DomainPrincipal, string, AppendTranscriptEntryRequest) (TranscriptEntry, error) {
			return TranscriptEntry{}, nil
		},
		ListTranscripts: func(context.Context, DomainPrincipal) ([]Transcript, error) {
			return nil, nil
		},
		GetTranscript: func(context.Context, DomainPrincipal, string) (Transcript, []TranscriptEntry, error) {
			return Transcript{}, nil, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
	var c capabilities
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if f := c.Features["transcripts"]; !f.Supported || f.Reason != "" {
		t.Errorf("transcripts feature = %+v, want supported", f)
	}
	if f := c.Features["self_digest"]; !f.Supported || f.Reason != "" {
		t.Errorf("self_digest feature = %+v, want supported", f)
	}
}

func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
}

// TestPendingAccountIsGated proves the pending lifecycle contract: everything
// is refused (403) — including whoami — except checking the account's own
// status and closing it. An unactivated account can be watched or abandoned,
// nothing else.
func TestPendingAccountIsGated(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "pending-token" {
			return "opr_p", "acc_p", "pending", true, nil
		}
		return "", "", "", false, nil
	}
	created := false
	create := func(_ context.Context, _, name string) (Realm, error) {
		created = true
		return Realm{ID: "realm_x", Name: name}, nil
	}
	closed := false
	closeFn := func(_ context.Context, _, _, _ string) error {
		closed = true
		return nil
	}
	get := func(_ context.Context, accountID string) (AccountRecord, error) {
		return AccountRecord{ID: accountID, Status: "pending", Email: "p@example.com"}, nil
	}
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth, CreateRealm: create, CloseAccount: closeFn, GetAccount: get}))
	defer srv.Close()

	do := func(method, path, body string) *http.Response {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rdr)
		req.Header.Set("Authorization", "Bearer pending-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := do(http.MethodPost, "/v1/realms", `{"name":"prod"}`)
	closeBody(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("realm create while pending = %d, want 403", resp.StatusCode)
	}
	if created {
		t.Error("realm create ran despite pending account")
	}

	resp = do(http.MethodGet, "/v1/whoami", "")
	closeBody(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("whoami while pending = %d, want 403 (only status and close are allowed)", resp.StatusCode)
	}

	resp = do(http.MethodGet, "/v1/account", "")
	var out struct {
		Account AccountRecord `json:"account"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusOK || out.Account.Status != "pending" {
		t.Errorf("account status while pending = %d %+v, want 200/pending", resp.StatusCode, out.Account)
	}

	resp = do(http.MethodPost, "/v1/account:close", `{"reason":"abandoning"}`)
	closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("close while pending = %d, want 200", resp.StatusCode)
	}
	if !closed {
		t.Error("close did not run for pending account")
	}
}

// TestReapAccount proves the reap contract: provision-token only, 200 for a
// reaped (or already-closed) account, 409 when the account activated first,
// 404 for unknown ids — and the route only exists alongside provisioning.
func TestReapAccount(t *testing.T) {
	reap := func(_ context.Context, accountID string) (bool, error) {
		switch accountID {
		case "acc_pending":
			return true, nil
		case "acc_closed":
			return false, nil
		case "acc_active":
			return false, ErrConflict
		default:
			return false, ErrNotFound
		}
	}
	provision := func(_ context.Context, _, _ string) (ProvisionedAccount, error) {
		return ProvisionedAccount{}, errors.New("unused")
	}
	srv := httptest.NewServer(apiMux(Config{
		ProvisionToken:   "witself_prv_test",
		ProvisionAccount: provision,
		ReapAccount:      reap,
	}))
	defer srv.Close()

	do := func(id, token string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/accounts/"+id+":reap", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	for _, tc := range []struct {
		id, token string
		want      int
	}{
		{"acc_pending", "wrong-token", http.StatusUnauthorized},
		{"acc_pending", "", http.StatusUnauthorized},
		{"acc_pending", "witself_prv_test", http.StatusOK},
		{"acc_closed", "witself_prv_test", http.StatusOK},
		{"acc_active", "witself_prv_test", http.StatusConflict},
		{"acc_missing", "witself_prv_test", http.StatusNotFound},
	} {
		resp := do(tc.id, tc.token)
		closeBody(t, resp)
		if resp.StatusCode != tc.want {
			t.Errorf("reap %s (token %q) = %d, want %d", tc.id, tc.token, resp.StatusCode, tc.want)
		}
	}

	// Without the provisioning pair the route must not exist at all.
	bare := httptest.NewServer(apiMux(Config{ReapAccount: reap}))
	defer bare.Close()
	req, _ := http.NewRequest(http.MethodPost, bare.URL+"/v1/accounts/acc_pending:reap", nil)
	req.Header.Set("Authorization", "Bearer witself_prv_test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("reap on self-hosted config = %d, want 404 (route unmounted)", resp.StatusCode)
	}
}

// TestActivateAccount proves the activation contract: provision-token only,
// 200 for a freshly activated or already-active account (idempotent second
// click), 409 for a closed/ineligible account, 404 for unknown ids — and
// both lifecycle verbs coexist on the shared route.
func TestActivateAccount(t *testing.T) {
	activate := func(_ context.Context, accountID string) (bool, error) {
		switch accountID {
		case "acc_pending":
			return true, nil
		case "acc_active":
			return false, nil
		case "acc_closed":
			return false, ErrConflict
		default:
			return false, ErrNotFound
		}
	}
	reap := func(_ context.Context, _ string) (bool, error) { return true, nil }
	provision := func(_ context.Context, _, _ string) (ProvisionedAccount, error) {
		return ProvisionedAccount{}, errors.New("unused")
	}
	srv := httptest.NewServer(apiMux(Config{
		ProvisionToken:   "witself_prv_test",
		ProvisionAccount: provision,
		ReapAccount:      reap,
		ActivateAccount:  activate,
	}))
	defer srv.Close()

	do := func(path, token string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{"/v1/accounts/acc_pending:activate", "wrong", http.StatusUnauthorized},
		{"/v1/accounts/acc_pending:activate", "witself_prv_test", http.StatusOK},
		{"/v1/accounts/acc_active:activate", "witself_prv_test", http.StatusOK},
		{"/v1/accounts/acc_closed:activate", "witself_prv_test", http.StatusConflict},
		{"/v1/accounts/acc_missing:activate", "witself_prv_test", http.StatusNotFound},
		{"/v1/accounts/acc_pending:reap", "witself_prv_test", http.StatusOK}, // dispatcher still reaps
		{"/v1/accounts/acc_pending:frobnicate", "witself_prv_test", http.StatusNotFound},
	} {
		resp := do(tc.path, tc.token)
		closeBody(t, resp)
		if resp.StatusCode != tc.want {
			t.Errorf("POST %s (token %q) = %d, want %d", tc.path, tc.token, resp.StatusCode, tc.want)
		}
	}

	// activated=true vs false must be distinguishable in the body.
	resp := do("/v1/accounts/acc_active:activate", "witself_prv_test")
	var out struct {
		Activated bool   `json:"activated"`
		Status    string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if out.Activated || out.Status != "active" {
		t.Errorf("already-active response = %+v, want activated=false status=active", out)
	}
}

// TestUpdateAccountEmail proves the email-change verb's contract: provision
// token only, owner-only refusals pass through as 403, non-active as 409,
// and the committed address echoes back normalized.
func TestUpdateAccountEmail(t *testing.T) {
	update := func(_ context.Context, accountID, operatorID, _ string) error {
		switch {
		case accountID == "acc_missing":
			return ErrNotFound
		case operatorID == "opr_member":
			return ErrNotAccountOwner
		case accountID == "acc_pending":
			return ErrConflict
		}
		return nil
	}
	provision := func(_ context.Context, _, _ string) (ProvisionedAccount, error) {
		return ProvisionedAccount{}, errors.New("unused")
	}
	srv := httptest.NewServer(apiMux(Config{
		ProvisionToken:     "witself_prv_test",
		ProvisionAccount:   provision,
		UpdateAccountEmail: update,
	}))
	defer srv.Close()

	do := func(id, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/accounts/"+id+":update-email", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer witself_prv_test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	for _, tc := range []struct {
		id, body string
		want     int
	}{
		{"acc_x", `{"operator_id":"opr_owner","new_email":"NEW@Example.com"}`, http.StatusOK},
		{"acc_x", `{"operator_id":"opr_member","new_email":"n@e.c"}`, http.StatusForbidden},
		{"acc_pending", `{"operator_id":"opr_owner","new_email":"n@e.c"}`, http.StatusConflict},
		{"acc_missing", `{"operator_id":"opr_owner","new_email":"n@e.c"}`, http.StatusNotFound},
		{"acc_x", `{"operator_id":"opr_owner","new_email":"not-an-email"}`, http.StatusBadRequest},
		{"acc_x", `{"new_email":"n@e.c"}`, http.StatusBadRequest},
	} {
		resp := do(tc.id, tc.body)
		if tc.want == http.StatusOK {
			var out struct {
				Email string `json:"email"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
			if out.Email != "new@example.com" {
				t.Errorf("committed email = %q, want normalized new@example.com", out.Email)
			}
		}
		closeBody(t, resp)
		if resp.StatusCode != tc.want {
			t.Errorf("update-email %s %s = %d, want %d", tc.id, tc.body, resp.StatusCode, tc.want)
		}
	}
}

// TestSuspendedAccountIsGated proves the suspended contract: every domain
// action is refused (403 with "account is suspended") while status, close,
// and RESUME still work. The store adjudicates suspend/resume authority; the
// dispatcher just lets the calls through.
func TestSuspendedAccountIsGated(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "suspended-token" {
			return "opr_s", "acc_s", "suspended", true, nil
		}
		return "", "", "", false, nil
	}
	realmed := false
	create := func(_ context.Context, _, name string) (Realm, error) {
		realmed = true
		return Realm{ID: "realm_x", Name: name}, nil
	}
	list := func(_ context.Context, _ string) ([]Realm, error) { return nil, nil }
	get := func(_ context.Context, accountID string) (AccountRecord, error) {
		return AccountRecord{ID: accountID, Status: "suspended", SuspendedFor: "owner_request"}, nil
	}
	resumed := false
	resume := func(_ context.Context, _, _ string) error { resumed = true; return nil }
	closed := false
	closeFn := func(_ context.Context, _, _, _ string) error { closed = true; return nil }
	srv := httptest.NewServer(apiMux(Config{
		Authenticate:       auth,
		CreateRealm:        create,
		ListRealms:         list,
		GetAccount:         get,
		ResumeAccountOwner: resume,
		CloseAccount:       closeFn,
	}))
	defer srv.Close()

	do := func(method, path, body string) *http.Response {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rdr)
		req.Header.Set("Authorization", "Bearer suspended-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Domain endpoints must all 403 with "account is suspended" — the whole
	// point of the freeze. Cover reads (list) AND writes (create) so any
	// future endpoint added without the requireOperator gate breaks CI.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/realms", `{"name":"prod"}`},
		{http.MethodGet, "/v1/realms", ""},
	} {
		resp := do(tc.method, tc.path, tc.body)
		body, _ := io.ReadAll(resp.Body)
		closeBody(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s while suspended = %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
		if !strings.Contains(string(body), "suspended") {
			t.Errorf("%s %s refusal did not name the suspended status: %s", tc.method, tc.path, body)
		}
	}
	if realmed {
		t.Error("realm create ran despite suspended account")
	}

	// Reads and status pass through.
	resp := do(http.MethodGet, "/v1/account", "")
	closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("account status while suspended = %d, want 200", resp.StatusCode)
	}

	// Resume passes through.
	resp = do(http.MethodPost, "/v1/account:resume", "")
	closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("resume while suspended = %d, want 200", resp.StatusCode)
	}
	if !resumed {
		t.Error("resume did not run")
	}

	// Close still works too — a suspended owner can decide to leave for good.
	resp = do(http.MethodPost, "/v1/account:close", `{"reason":"abandoning"}`)
	closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("close while suspended = %d, want 200", resp.StatusCode)
	}
	if !closed {
		t.Error("close did not run for suspended account")
	}
}

// TestImportAccountArchive proves the restore verb's contract: provision
// token only, the body streams through untouched, and each refusal maps to
// its own status — 409 exists, 409 too-new, 400 corrupt.
func TestImportAccountArchive(t *testing.T) {
	var gotBody []byte
	imp := func(_ context.Context, accountID string, body io.Reader) (ImportSummary, error) {
		b, _ := io.ReadAll(body)
		gotBody = b
		switch accountID {
		case "acc_exists":
			return ImportSummary{}, ErrConflict
		case "acc_future":
			return ImportSummary{}, ErrArchiveTooNew
		case "acc_garbled":
			return ImportSummary{}, ErrBadArchive
		default:
			return ImportSummary{AccountID: accountID, Status: "suspended", SchemaVersion: 13}, nil
		}
	}
	provision := func(_ context.Context, _, _ string) (ProvisionedAccount, error) {
		return ProvisionedAccount{}, errors.New("unused")
	}
	srv := httptest.NewServer(apiMux(Config{
		ProvisionToken:       "witself_prv_test",
		ProvisionAccount:     provision,
		ImportAccountArchive: imp,
	}))
	defer srv.Close()

	do := func(path, token, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{"/v1/accounts/acc_ok:import", "wrong", http.StatusUnauthorized},
		{"/v1/accounts/acc_ok:import", "witself_prv_test", http.StatusOK},
		{"/v1/accounts/acc_exists:import", "witself_prv_test", http.StatusConflict},
		{"/v1/accounts/acc_future:import", "witself_prv_test", http.StatusConflict},
		{"/v1/accounts/acc_garbled:import", "witself_prv_test", http.StatusBadRequest},
	} {
		resp := do(tc.path, tc.token, "tar-bytes")
		closeBody(t, resp)
		if resp.StatusCode != tc.want {
			t.Errorf("POST %s (token %q) = %d, want %d", tc.path, tc.token, resp.StatusCode, tc.want)
		}
	}
	if string(gotBody) != "tar-bytes" {
		t.Errorf("body reaching the import func = %q, want the raw stream", gotBody)
	}

	// The success body carries the archive's coordinates.
	resp := do("/v1/accounts/acc_ok:import", "witself_prv_test", "tar-bytes")
	var out struct {
		AccountID     string `json:"account_id"`
		Status        string `json:"status"`
		SchemaVersion int    `json:"archive_schema_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if out.AccountID != "acc_ok" || out.Status != "suspended" || out.SchemaVersion != 13 {
		t.Errorf("import response = %+v", out)
	}
}

// TestLogAccountEvent proves the :events verb's contract: provision token
// only, verb + actor_kind required, ErrBadInput → 400 for
// registry-refused shapes, ErrNotFound → 404 for missing accounts.
func TestLogAccountEvent(t *testing.T) {
	log := func(_ context.Context, accountID, verb, actorKind string, _ map[string]any) error {
		switch accountID {
		case "acc_missing":
			return ErrNotFound
		case "acc_baddata":
			return fmt.Errorf("%w: verb %s: unknown verb", ErrBadInput, verb)
		default:
			_ = actorKind
			return nil
		}
	}
	provision := func(_ context.Context, _, _ string) (ProvisionedAccount, error) {
		return ProvisionedAccount{}, errors.New("unused")
	}
	srv := httptest.NewServer(apiMux(Config{
		ProvisionToken:   "witself_prv_test",
		ProvisionAccount: provision,
		LogAccountEvent:  log,
	}))
	defer srv.Close()

	do := func(path, token, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	for _, tc := range []struct {
		path, token, body string
		want              int
	}{
		{"/v1/accounts/acc_x:events", "wrong", `{"verb":"recovery.requested","actor_kind":"control_plane","metadata":{"email_masked":"s***@w***.ai"}}`, http.StatusUnauthorized},
		{"/v1/accounts/acc_x:events", "witself_prv_test", `{"verb":"recovery.requested","actor_kind":"control_plane","metadata":{"email_masked":"s***@w***.ai"}}`, http.StatusOK},
		{"/v1/accounts/acc_x:events", "witself_prv_test", `{}`, http.StatusBadRequest},                            // missing verb + actor_kind
		{"/v1/accounts/acc_x:events", "witself_prv_test", `{"verb":"recovery.requested"}`, http.StatusBadRequest}, // missing actor_kind
		{"/v1/accounts/acc_x:events", "witself_prv_test", ``, http.StatusBadRequest},                              // invalid JSON
		{"/v1/accounts/acc_missing:events", "witself_prv_test", `{"verb":"recovery.requested","actor_kind":"control_plane","metadata":{}}`, http.StatusNotFound},
		{"/v1/accounts/acc_baddata:events", "witself_prv_test", `{"verb":"sneaky.action","actor_kind":"control_plane","metadata":{}}`, http.StatusBadRequest},
	} {
		resp := do(tc.path, tc.token, tc.body)
		closeBody(t, resp)
		if resp.StatusCode != tc.want {
			t.Errorf("POST %s body %q = %d, want %d", tc.path, tc.body, resp.StatusCode, tc.want)
		}
	}
}

// TestResumeAccountSystem proves the machine-resume verb: provision token
// only, a category is required, and the authority-scoping refusals map to
// 409s the control plane can tell apart from success.
func TestResumeAccountSystem(t *testing.T) {
	resume := func(_ context.Context, accountID, _ string) error {
		switch accountID {
		case "acc_owner_susp":
			return ErrResumeWrongCategory
		case "acc_active":
			return nil // idempotent
		case "acc_closed":
			return ErrAccountNotSuspended
		case "acc_missing":
			return ErrNotFound
		default:
			return nil
		}
	}
	provision := func(_ context.Context, _, _ string) (ProvisionedAccount, error) {
		return ProvisionedAccount{}, errors.New("unused")
	}
	srv := httptest.NewServer(apiMux(Config{
		ProvisionToken:      "witself_prv_test",
		ProvisionAccount:    provision,
		ResumeAccountSystem: resume,
	}))
	defer srv.Close()

	do := func(path, token, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	for _, tc := range []struct {
		path, token, body string
		want              int
	}{
		{"/v1/accounts/acc_evac:resume", "wrong", `{"for":"evacuation"}`, http.StatusUnauthorized},
		{"/v1/accounts/acc_evac:resume", "witself_prv_test", `{"for":"evacuation"}`, http.StatusOK},
		{"/v1/accounts/acc_evac:resume", "witself_prv_test", `{}`, http.StatusBadRequest},
		{"/v1/accounts/acc_evac:resume", "witself_prv_test", ``, http.StatusBadRequest},
		{"/v1/accounts/acc_owner_susp:resume", "witself_prv_test", `{"for":"evacuation"}`, http.StatusConflict},
		{"/v1/accounts/acc_closed:resume", "witself_prv_test", `{"for":"evacuation"}`, http.StatusConflict},
		{"/v1/accounts/acc_active:resume", "witself_prv_test", `{"for":"evacuation"}`, http.StatusOK},
		{"/v1/accounts/acc_missing:resume", "witself_prv_test", `{"for":"evacuation"}`, http.StatusNotFound},
	} {
		resp := do(tc.path, tc.token, tc.body)
		closeBody(t, resp)
		if resp.StatusCode != tc.want {
			t.Errorf("POST %s body %q = %d, want %d", tc.path, tc.body, resp.StatusCode, tc.want)
		}
	}
}

// TestValidateAdminHandle pins the shape guard the /v1/accounts/{id}/admin:*
// handlers apply before delegating to the store. The same regex lives in
// the Cloudflare Worker (ADMIN_HANDLE) and in the store (adminHandleRE);
// all three must agree, so any drift here shows up as an integration bug.
func TestValidateAdminHandle(t *testing.T) {
	tests := map[string]bool{
		"sarah":                true,
		"s2":                   true,
		"sarah_jones":          true,
		"a-b":                  true,
		"abcdefghijklmnopqrst": true,
		"":                     false,
		"a":                    false,
		"S":                    false,
		"1abc":                 false,
		"-abc":                 false,
		"has space":            false,
		"has.dot":              false,
		"has/slash":            false,
		"UPPER":                false,
		"way_too_long_way_too_long_beyond_32_chars": false,
	}
	for h, wantOK := range tests {
		err := validateAdminHandle(h)
		gotOK := err == nil
		if gotOK != wantOK {
			t.Errorf("validateAdminHandle(%q): ok=%v want %v (err=%v)", h, gotOK, wantOK, err)
		}
	}
}
