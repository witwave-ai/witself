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
	"time"
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
	create := func(_ context.Context, _, agentID string) (string, string, error) {
		if agentID == "missing" {
			return "", "", ErrNotFound
		}
		return "witself_agt_minted", "tok_agent", nil
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
	create := func(_ context.Context, accountID, displayName, tokenDisplayName string, ttl *time.Duration) (Operator, string, *time.Time, error) {
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
	revoke := func(_ context.Context, accountID, tokenID string) error {
		if accountID != "acc_y" {
			t.Fatalf("revoke account = %q", accountID)
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
