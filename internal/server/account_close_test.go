package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccountCloseEligibilityStatusAndAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		checkErr     error
		want         int
	}{
		{"active owner", "active", nil, 200},
		{"pending owner", "pending", nil, 200},
		{"suspended owner", "suspended", nil, 200},
		{"nonowner", "active", ErrNotAccountOwner, 403},
		{"default", "active", ErrCannotCloseDefault, 403},
		{"vault busy", "active", ErrConflict, 409},
		{"store failed", "active", errors.New("private database details"), 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := apiMux(Config{
				Authenticate: func(_ context.Context, token string) (string, string, string, bool, error) {
					return "opr_owner", "acc_test", tc.status, token == "owner", nil
				},
				CheckAccountClose: func(_ context.Context, accountID, operatorID string) error {
					calls++
					if accountID != "acc_test" || operatorID != "opr_owner" {
						t.Error("incorrect principal")
					}
					return tc.checkErr
				},
			})
			req := httptest.NewRequest(http.MethodGet, "/v1/account/close-eligibility", nil)
			req.Header.Set("Authorization", "Bearer owner")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != tc.want || calls != 1 || strings.Contains(rec.Body.String(), "private") {
				t.Fatalf("eligibility status %d calls %d", rec.Code, calls)
			}
			if tc.want == 200 {
				var result map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result["schema_version"] != "witself.v0" || result["kind"] != "operator" || result["account_role"] != "account_owner" || result["account_id"] != "acc_test" || result["operator_id"] != "opr_owner" {
					t.Fatalf("incorrect eligibility response: %v", result)
				}
			}
			req.Header.Del("Authorization")
			rec = httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != 401 || calls != 1 {
				t.Fatal("unauthenticated preflight ran")
			}
		})
	}
}

func TestDirectAccountClosePassesOwnerBearerAndFailsClosedOnBilling(t *testing.T) {
	called := false
	srv := apiMux(Config{
		Authenticate: func(_ context.Context, token string) (string, string, string, bool, error) {
			return "opr_owner", "acc_test", "suspended", token == "owner", nil
		},
		CloseAccount: func(_ context.Context, accountID, operatorID, reason, ownerBearer string) error {
			called = true
			if accountID != "acc_test" || operatorID != "opr_owner" || reason != "requested" || ownerBearer != "owner" {
				t.Error("close callback lost authenticated request context")
			}
			return ErrAccountBillingClosePending
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/account:close", strings.NewReader(`{"reason":"requested"}`))
	req.Header.Set("Authorization", "Bearer owner")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if !called || rec.Code != 503 || !strings.Contains(rec.Body.String(), "account remains open") || strings.Contains(rec.Body.String(), `"status":"closed"`) {
		t.Fatalf("direct close did not fail safely: %d %s", rec.Code, rec.Body.String())
	}
}
