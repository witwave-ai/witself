package cpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/plans"
)

func TestAccountBillingCloseRequiresBridgeAndOwnerWithoutPurchaseGate(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, bearer, bridge, role, body string
		missingAuth                      bool
		want                             int
	}{
		{name: "owner callback", bearer: "owner", bridge: "bridge", role: "account_owner", body: "{}", want: 200},
		{name: "missing bridge", bearer: "owner", role: "account_owner", body: "{}", want: 403},
		{name: "wrong bridge", bearer: "owner", bridge: "wrong", role: "account_owner", body: "{}", want: 403},
		{name: "missing bearer", bridge: "bridge", role: "account_owner", body: "{}", want: 401},
		{name: "billing role", bearer: "owner", bridge: "bridge", role: "account_billing", body: "{}", want: 403},
		{name: "unknown parameter", bearer: "owner", bridge: "bridge", role: "account_owner", body: `{"plan":"standard"}`, want: 400},
		{name: "duplicate body", bearer: "owner", bridge: "bridge", role: "account_owner", body: "{}{}", want: 400},
		{name: "missing close auth", bearer: "owner", bridge: "bridge", role: "account_owner", body: "{}", missingAuth: true, want: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := lifecycle.NewMemStore()
			manager, err := lifecycle.NewManager(lifecycle.Config{Catalog: catalog, Store: store, Applier: noopApplier{}})
			if err != nil {
				t.Fatal(err)
			}
			gateCalls := 0
			cfg := Config{
				Manager: manager, Catalog: catalog,
				Authenticate: func(context.Context, string, string, AccountPermission) (AccountAccess, bool, error) {
					t.Fatal("close used ordinary active-only billing authentication")
					return AccountAccess{}, false, nil
				},
				CloseAuthenticate: func(_ context.Context, accountID, bearer string, permission AccountPermission) (AccountAccess, bool, error) {
					if permission != AccountPermissionAccountClose {
						t.Fatalf("close permission = %s", permission)
					}
					return AccountAccess{ActorID: "operator", Role: tc.role, Permission: permission}, accountID == "acct_close" && bearer == "owner", nil
				},
				InternalAuthenticate: func(_ context.Context, bearer string) (bool, error) { return bearer == "bridge", nil },
				LifecycleObserver:    NewPlanLifecycleObserver(false),
				BillingMutationGate: func(context.Context, string) (bool, error) {
					gateCalls++
					return false, nil
				},
			}
			if tc.missingAuth {
				cfg.CloseAuthenticate = nil
			}
			mux := http.NewServeMux()
			if err := Register(mux, cfg); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/accounts/acct_close/billing:close", strings.NewReader(tc.body))
			if tc.bearer != "" {
				request.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			request.Header.Set(accountCloseBridgeHeader, tc.bridge)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != tc.want || gateCalls != 0 {
				t.Fatalf("status=%d want=%d purchase gate calls=%d body=%s", response.Code, tc.want, gateCalls, response.Body.String())
			}
			r, exists, err := store.Get(context.Background(), "acct_close")
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == 200 {
				var result map[string]string
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result["account_id"] != "acct_close" || result["status"] != "billing_closed" || result["schema_version"] != "witself.v0" || len(result) != 3 {
					t.Fatalf("invalid success acknowledgement: %s", response.Body.String())
				}
				if !exists || r.BillingClosedAt.IsZero() {
					t.Fatal("success before durable billing closure")
				}
			} else if exists {
				t.Fatal("refused closure mutated lifecycle state")
			}
		})
	}
}

func TestAccountClosePermissionIsOwnerOnly(t *testing.T) {
	for _, role := range []string{"account_owner", "account_billing", "account_admin", "account_operator", "unknown"} {
		if got := accountRoleAllows(role, AccountPermissionAccountClose); got != (role == "account_owner") {
			t.Fatalf("role=%s close allowed=%t", role, got)
		}
	}
}

func TestCellCloseAuthenticateUsesEligibilityInsteadOfWhoami(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/account/close-eligibility" || r.Header.Get("Authorization") != "Bearer owner" {
			t.Errorf("unexpected eligibility request: %s", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0", "kind": "operator", "account_id": "acct_close",
			"operator_id": "opr_owner", "account_role": "account_owner",
		})
	}))
	t.Cleanup(server.Close)
	authenticate := CellCloseAuthenticate(StaticCell(server.URL, "unused"))
	access, ok, err := authenticate(context.Background(), "acct_close", "owner", AccountPermissionAccountClose)
	if err != nil || !ok || access.ActorID != "opr_owner" || access.Role != "account_owner" {
		t.Fatalf("eligible owner: access=%+v ok=%t err=%v", access, ok, err)
	}
	if _, ok, err := authenticate(context.Background(), "another_account", "owner", AccountPermissionAccountClose); err != nil || ok {
		t.Fatalf("wrong account: ok=%t err=%v", ok, err)
	}
}
