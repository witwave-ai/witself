package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","operator_token":"witself_opr_child","operator_id":"opr_1","display_name":"deploy bot","expires_at":"2026-07-03T00:00:00Z"}`))
	}))
	defer srv.Close()

	res, err := CreateOperatorToken(context.Background(), srv.URL, "witself_opr_parent", "deploy bot", "24h")
	if err != nil {
		t.Fatal(err)
	}
	if res.OperatorToken != "witself_opr_child" || res.OperatorID != "opr_1" || res.DisplayName != "deploy bot" || res.ExpiresAt == "" {
		t.Errorf("operator token result = %+v", res)
	}
}
