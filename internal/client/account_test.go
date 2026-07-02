package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/accounts" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Email  string `json:"email"`
			Invite string `json:"invite"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch body.Invite {
		case "good-code":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","account_id":"acc_1","operator_id":"opr_1",
				"email":"` + body.Email + `","status":"active","bootstrap_token":"witself_boot_x",
				"cell":{"name":"aws-prod-usw2-1","endpoint":"https://api.example.com"}}`))
		case "spent-code":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","error":"invalid invite: fully used"}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","error":"no capacity: no accepting cells"}`))
		}
	}))
	defer srv.Close()

	acct, err := CreateAccount(context.Background(), srv.URL, "amy@co.com", "good-code", "Amy")
	if err != nil {
		t.Fatal(err)
	}
	if acct.AccountID != "acc_1" || acct.Cell.Endpoint != "https://api.example.com" || acct.BootstrapToken == "" {
		t.Errorf("account = %+v", acct)
	}

	_, err = CreateAccount(context.Background(), srv.URL, "amy@co.com", "spent-code", "")
	if err == nil || !strings.Contains(err.Error(), "fully used") {
		t.Errorf("spent invite error = %v, want server message surfaced", err)
	}

	_, err = CreateAccount(context.Background(), srv.URL, "amy@co.com", "any-other", "")
	if err == nil || !strings.Contains(err.Error(), "no capacity") {
		t.Errorf("no-capacity error = %v, want server message surfaced", err)
	}
}
