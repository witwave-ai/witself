package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeleteAgentReturnsConflictForActiveVaultLifecycle(t *testing.T) {
	auth := func(context.Context, string) (string, string, string, bool, error) {
		return "opr_owner", "acc_test", "active", true, nil
	}
	deleted := func(context.Context, string, string, string) error { return ErrConflict }
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth, DeleteAgent: deleted}))
	defer srv.Close()
	req, err := http.NewRequest(http.MethodDelete,
		srv.URL+"/v1/realms/realm_abcdefghijklmnop/agents/agent_abcdefghijklmnop", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer operator")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, response)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.StatusCode)
	}
}
