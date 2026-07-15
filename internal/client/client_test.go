package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBootstrapLoginSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/auth/bootstrap" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","operator_token":"witself_opr_abc","operator_id":"opr_1"}`))
	}))
	defer srv.Close()

	res, err := BootstrapLogin(context.Background(), srv.URL, "witself_boot_x")
	if err != nil {
		t.Fatal(err)
	}
	if res.OperatorToken != "witself_opr_abc" || res.OperatorID != "opr_1" {
		t.Errorf("got %+v, want operator_token=witself_opr_abc operator_id=opr_1", res)
	}
}

func TestBootstrapLoginUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := BootstrapLogin(context.Background(), srv.URL, "bad"); err == nil {
		t.Error("want error on 401, got nil")
	}
}

func TestCreateOperatorToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/operators/self/tokens" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer witself_opr_parent" {
			t.Errorf("Authorization = %q", got)
		}
		var body struct {
			DisplayName string `json:"display_name"`
			TTL         string `json:"ttl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.DisplayName != "deploy bot" {
			t.Errorf("display_name = %q, want deploy bot", body.DisplayName)
		}
		if body.TTL != "24h" {
			t.Errorf("ttl = %q, want 24h", body.TTL)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","operator_token":"witself_opr_child","operator_id":"opr_1","token_id":"tok_1","display_name":"deploy bot","expires_at":"2026-07-03T00:00:00Z"}`))
	}))
	defer srv.Close()

	res, err := CreateOperatorToken(context.Background(), srv.URL, "witself_opr_parent", "deploy bot", "24h")
	if err != nil {
		t.Fatal(err)
	}
	if res.OperatorToken != "witself_opr_child" || res.OperatorID != "opr_1" || res.TokenID != "tok_1" || res.DisplayName != "deploy bot" || res.ExpiresAt == "" {
		t.Errorf("operator token result = %+v", res)
	}
}

func TestCreateCuratorToken(t *testing.T) {
	expiresAt := time.Date(2026, 7, 15, 8, 30, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/agents/agent_1/curator-tokens" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer witself_opr_parent" {
			t.Errorf("Authorization = %q", got)
		}
		var body struct {
			AccessProfile string `json:"access_profile"`
			DisplayName   string `json:"display_name"`
			TTL           string `json:"ttl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.AccessProfile != "curator-preview" || body.DisplayName != "nightly curator" || body.TTL != "30m" {
			t.Errorf("request body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "agent_token": "witself_agt_curator",
			"token_id": "tok_curator", "agent_id": "agent_1", "agent_name": "memory-agent",
			"access_profile": "curator-preview", "display_name": "nightly curator", "expires_at": expiresAt,
		})
	}))
	defer srv.Close()

	result, err := CreateCuratorToken(
		context.Background(), srv.URL, "witself_opr_parent", "agent_1",
		"curator-preview", "nightly curator", "30m",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.AgentToken != "witself_agt_curator" || result.TokenID != "tok_curator" ||
		result.AgentID != "agent_1" || result.AgentName != "memory-agent" ||
		result.AccessProfile != "curator-preview" || result.DisplayName != "nightly curator" ||
		!result.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("curator token result = %#v", result)
	}
}

func TestOperatorLifecycleClient(t *testing.T) {
	now := time.Date(2026, 7, 2, 1, 2, 3, 0, time.UTC).Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer witself_opr_parent" {
			t.Errorf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operators":
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","operators":[{"id":"opr_1","display_name":"owner","role":"account_owner","is_root":true,"created_at":"` + now + `","updated_at":"` + now + `","tokens":[{"id":"tok_1","display_name":"laptop","created_at":"` + now + `"}]}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/operators":
			var body struct {
				DisplayName      string `json:"display_name"`
				TokenDisplayName string `json:"token_display_name"`
				TTL              string `json:"ttl"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.DisplayName != "deploy bot" || body.TokenDisplayName != "deploy token" || body.TTL != "24h" {
				t.Errorf("create body = %+v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","operator":{"id":"opr_2","display_name":"deploy bot","role":"account_operator","is_root":false,"created_at":"` + now + `","updated_at":"` + now + `","tokens":[{"id":"tok_2","display_name":"deploy token","created_at":"` + now + `"}]},"operator_token":"witself_opr_new"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/operators/opr_2":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tokens/tok_2:revoke":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ops, err := ListOperators(context.Background(), srv.URL, "witself_opr_parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Tokens[0].DisplayName != "laptop" {
		t.Fatalf("operators = %+v", ops)
	}

	created, err := CreateOperator(context.Background(), srv.URL, "witself_opr_parent", "deploy bot", "deploy token", "24h")
	if err != nil {
		t.Fatal(err)
	}
	if created.Operator.ID != "opr_2" || created.OperatorToken != "witself_opr_new" || created.Operator.Tokens[0].ID != "tok_2" {
		t.Fatalf("created = %+v", created)
	}

	if err := DeleteOperator(context.Background(), srv.URL, "witself_opr_parent", "opr_2"); err != nil {
		t.Fatal(err)
	}
	if err := RevokeToken(context.Background(), srv.URL, "witself_opr_parent", "tok_2"); err != nil {
		t.Fatal(err)
	}
}
