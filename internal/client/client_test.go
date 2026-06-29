package client

import (
	"context"
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
