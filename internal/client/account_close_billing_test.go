package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
)

func TestCloseAccountBillingRequiresExactConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		ok         bool
	}{
		{"confirmed", `{"schema_version":"witself.v0","account_id":"acc_test","status":"billing_closed"}`, 200, true},
		{"wrong account", `{"schema_version":"witself.v0","account_id":"acc_other","status":"billing_closed"}`, 200, false},
		{"wrong schema", `{"schema_version":"witself.v1","account_id":"acc_test","status":"billing_closed"}`, 200, false},
		{"wrong status", `{"schema_version":"witself.v0","account_id":"acc_test","status":"active"}`, 200, false},
		{"accepted is not finished", `{}`, 202, false},
		{"busy", `{"error":"private provider detail"}`, 409, false},
		{"failed", `{"error":"private provider detail"}`, 500, false},
		{"malformed", `{`, 200, false},
		{"oversized", strings.Repeat(" ", 8193), 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/accounts/acc_test/billing:close" ||
					r.Header.Get("Authorization") != "Bearer owner" || r.Header.Get("X-Witself-Cell-Provision-Token") != "cell" {
					t.Error("incorrect close bridge request")
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != "{}" {
					t.Error("unexpected close request body")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			err := client.CloseAccountBilling(context.Background(), srv.URL+"/v1", "acc_test", "owner", "cell")
			if (err == nil) != tc.ok || (err != nil && !errors.Is(err, client.ErrAccountBillingClose)) {
				t.Fatalf("result = %v, want success %v", err, tc.ok)
			}
		})
	}
}

func TestCloseAccountBillingRefusesInvalidConfigurationAndRedirects(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	for _, args := range [][4]string{
		{"", "acc_test", "owner", "cell"},
		{" " + srv.URL, "acc_test", "owner", "cell"},
		{"http://public.example", "acc_test", "owner", "cell"},
		{srv.URL + "/%0a", "acc_test", "owner", "cell"},
		{srv.URL, "../other", "owner", "cell"},
		{srv.URL, "acc_test", "", "cell"},
		{srv.URL, "acc_test", "owner", ""},
		{srv.URL, "acc_test", "owner", "cell\n"},
	} {
		if err := client.CloseAccountBilling(context.Background(), args[0], args[1], args[2], args[3]); !errors.Is(err, client.ErrAccountBillingClose) {
			t.Fatal("invalid bridge configuration was accepted")
		}
	}
	if calls != 0 {
		t.Fatalf("invalid requests reached transport: %d", calls)
	}
	if err := client.CloseAccountBilling(context.Background(), srv.URL, "acc_test", "owner", "cell"); !errors.Is(err, client.ErrAccountBillingClose) || calls != 1 {
		t.Fatalf("redirect must fail without forwarding credentials: error %v, calls %d", err, calls)
	}
}

func TestGetAccountCloseEligibility(t *testing.T) {
	for _, role := range []string{"account_owner", "account_operator"} {
		t.Run(role, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/v1/account/close-eligibility" || r.Header.Get("Authorization") != "Bearer owner" || r.Header.Get("X-Witself-Cell-Provision-Token") != "" {
					t.Error("incorrect eligibility request")
				}
				_, _ = io.WriteString(w, `{"schema_version":"witself.v0","kind":"operator","account_id":"acc_test","operator_id":"opr_owner","account_role":"`+role+`"}`)
			}))
			defer srv.Close()
			result, err := client.GetAccountCloseEligibility(context.Background(), srv.URL, "owner")
			if role == "account_owner" {
				if err != nil || result.AccountID != "acc_test" || result.OperatorID != "opr_owner" {
					t.Fatalf("eligibility = %+v, %v", result, err)
				}
			} else if err == nil {
				t.Fatal("nonowner eligibility accepted")
			}
		})
	}
}

func TestGetAccountCloseEligibilityPreservesSafeAuthRefusals(t *testing.T) {
	for status, want := range map[int]error{401: client.ErrUnauthorized, 403: client.ErrForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "private refusal details")
		}))
		_, err := client.GetAccountCloseEligibility(context.Background(), srv.URL, "owner")
		srv.Close()
		if !errors.Is(err, want) || strings.Contains(err.Error(), "private") {
			t.Fatalf("auth status %d returned %v", status, err)
		}
	}
}

func TestCloseAccountControlPlaneConfirmationAndRedirect(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		ok         bool
	}{
		{"closed", `{"schema_version":"witself.v0","account_id":"acc_test","status":"closed"}`, 200, true},
		{"wrong account", `{"schema_version":"witself.v0","account_id":"acc_other","status":"closed"}`, 200, false},
		{"wrong schema", `{"schema_version":"witself.v1","account_id":"acc_test","status":"closed"}`, 200, false},
		{"billing only", `{"schema_version":"witself.v0","account_id":"acc_test","status":"billing_closed"}`, 200, false},
		{"missing receipt", ``, 200, false},
		{"redirect", ``, 307, false},
		{"oversized", strings.Repeat(" ", 8193), 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/v1/accounts/acc_test:close" || r.Header.Get("Authorization") != "Bearer owner" {
					t.Error("incorrect customer close request")
				}
				if tc.status == 307 {
					w.Header().Set("Location", "/redirected")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			err := client.CloseAccount(context.Background(), srv.URL+"/v1", "acc_test", "owner", "requested")
			if (err == nil) != tc.ok || calls != 1 {
				t.Fatalf("error %v, calls %d, want success %v and one request", err, calls, tc.ok)
			}
		})
	}
}
