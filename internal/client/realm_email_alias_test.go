package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestRealmEmailAliasCustomerRequests(t *testing.T) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer operator-token" {
			t.Fatalf("authorization = %q", got)
		}
		switch r.Method {
		case http.MethodPost:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["alias"] != "acme" || body["idempotency_key"] != "request-1" {
				t.Fatalf("body = %#v", body)
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": RealmEmailAliasSchemaVersion,
				"request": map[string]any{
					"id": "earq_aaaaaaaaaaaaaaaa", "alias": "acme", "account_id": "acc_1",
					"realm_id": "realm_aaaaaaaaaaaaaaaa", "status": "pending_review",
				},
			})
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": RealmEmailAliasSchemaVersion,
				"requests": []map[string]any{{
					"id": "earq_aaaaaaaaaaaaaaaa", "alias": "acme", "account_id": "acc_1",
					"realm_id": "realm_aaaaaaaaaaaaaaaa", "status": "pending_review",
				}},
			})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	got, err := RequestRealmEmailAlias(context.Background(), srv.URL,
		"operator-token", "acc_1", "realm_aaaaaaaaaaaaaaaa", " acme ", " request-1 ")
	if err != nil || got.Alias != "acme" || got.Status != "pending_review" {
		t.Fatalf("request = %#v, %v", got, err)
	}
	requests, err := ListRealmEmailAliasRequests(context.Background(), srv.URL,
		"operator-token", "acc_1", "realm_aaaaaaaaaaaaaaaa")
	if err != nil || len(requests) != 1 || requests[0].ID != "earq_aaaaaaaaaaaaaaaa" {
		t.Fatalf("list = %#v, %v", requests, err)
	}
	want := []string{
		"POST /v1/accounts/acc_1/realms/realm_aaaaaaaaaaaaaaaa/email-alias-requests",
		"GET /v1/accounts/acc_1/realms/realm_aaaaaaaaaaaaaaaa/email-alias-requests",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v; want %#v", calls, want)
	}
}

func TestAdminRealmEmailAliasReviewAndLifecycle(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer admin-token" {
			t.Fatalf("authorization = %q", got)
		}
		if r.Method != http.MethodGet {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["idempotency_key"] == nil {
				t.Fatalf("missing idempotency key: %#v", body)
			}
		}
		switch {
		case r.URL.Path == "/v1/admin/realm-email-alias-requests" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": RealmEmailAliasSchemaVersion,
				"requests":       []map[string]any{{"id": "earq_aaaaaaaaaaaaaaaa", "alias": "acme", "status": "pending_review"}},
			})
		case r.URL.Path == "/v1/admin/realm-email-alias-requests/earq_aaaaaaaaaaaaaaaa:approve":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": RealmEmailAliasSchemaVersion,
				"request":        map[string]any{"id": "earq_aaaaaaaaaaaaaaaa", "alias": "acme", "status": "approved"},
				"assignment": map[string]any{
					"claim_id": "era_bbbbbbbbbbbbbbbb", "alias": "acme",
					"domain": "agent-mail.witwave.ai", "status": "active",
				},
			})
		case r.URL.Path == "/v1/admin/realm-email-aliases/acme:suspend":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": RealmEmailAliasSchemaVersion,
				"assignment":     map[string]any{"alias": "acme", "status": "suspended"},
			})
		case r.URL.Path == "/v1/admin/realm-email-aliases/witself:abort-provisioning":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": RealmEmailAliasSchemaVersion,
				"assignment": map[string]any{
					"claim_id": "era_cccccccccccccccc", "alias": "witself",
					"status": "retired", "assignment_kind": "internal",
					"provisioning_failure": "admin_aborted",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	requests, err := ListAdminRealmEmailAliasRequests(context.Background(), srv.URL,
		"admin-token", AdminRealmEmailAliasRequestFilter{Status: "pending_review", AccountID: "acc_1"})
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests = %#v, %v", requests, err)
	}
	approved, err := ApproveAdminRealmEmailAliasRequest(context.Background(), srv.URL,
		"admin-token", "earq_aaaaaaaaaaaaaaaa", "approve-1", "reviewed")
	if err != nil || approved.Request.Status != "approved" ||
		approved.Assignment == nil || approved.Assignment.ClaimID != "era_bbbbbbbbbbbbbbbb" {
		t.Fatalf("approved = %#v, %v", approved, err)
	}
	suspended, err := SuspendAdminRealmEmailAlias(context.Background(), srv.URL,
		"admin-token", "acme", "suspend-1", "policy")
	if err != nil || suspended.Status != "suspended" {
		t.Fatalf("suspended = %#v, %v", suspended, err)
	}
	aborted, err := AbortAdminRealmEmailAliasProvisioning(
		context.Background(), srv.URL, "admin-token", "witself",
		"abort-1", "terminal recovery",
	)
	if err != nil || aborted.Status != "retired" ||
		aborted.ProvisioningFailure != "admin_aborted" {
		t.Fatalf("aborted = %#v, %v", aborted, err)
	}
	want := []string{
		"GET /v1/admin/realm-email-alias-requests?account_id=acc_1&status=pending_review",
		"POST /v1/admin/realm-email-alias-requests/earq_aaaaaaaaaaaaaaaa:approve",
		"POST /v1/admin/realm-email-aliases/acme:suspend",
		"POST /v1/admin/realm-email-aliases/witself:abort-provisioning",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v; want %#v", calls, want)
	}
}

func TestAdminRealmEmailReservedNameManagement(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet && r.URL.Path == "/v1/admin/realm-email-reserved-names" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": RealmEmailAliasSchemaVersion,
				"reserved_names": []map[string]any{{"name": "witself", "category": "platform_brand", "enabled": true}},
			})
			return
		}
		if r.Method != http.MethodGet {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["reason"] == nil || body["idempotency_key"] == nil {
				t.Fatalf("body = %#v", body)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": RealmEmailAliasSchemaVersion,
			"reserved_name": map[string]any{
				"name": "witself", "confusable_skeleton": "witself", "category": "platform_brand", "enabled": r.Method != http.MethodDelete,
			},
		})
	}))
	defer srv.Close()

	names, err := ListAdminRealmEmailReservedNames(context.Background(), srv.URL, "admin-token")
	if err != nil || len(names) != 1 || names[0].Name != "witself" {
		t.Fatalf("names = %#v, %v", names, err)
	}
	if _, err := CreateAdminRealmEmailReservedName(context.Background(), srv.URL,
		"admin-token", "witself", "platform_brand", "protect brand", "add-1", true); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := UpdateAdminRealmEmailReservedName(context.Background(), srv.URL,
		"admin-token", "witself", "platform_brand", "maintenance", "patch-1", &disabled, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireAdminRealmEmailReservedName(context.Background(), srv.URL,
		"admin-token", "witself", "retired", "delete-1"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /v1/admin/realm-email-reserved-names",
		"POST /v1/admin/realm-email-reserved-names",
		"PATCH /v1/admin/realm-email-reserved-names/witself",
		"DELETE /v1/admin/realm-email-reserved-names/witself",
	}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("methods = %#v; want %#v", methods, want)
	}
}

func TestListAdminRealmEmailAliasAudit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.RequestURI(); got != "/v1/admin/realm-email-alias-audit?action=alias.approved&limit=25" {
			t.Fatalf("request URI = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": RealmEmailAliasSchemaVersion,
			"events": []map[string]any{{
				"sequence": 7, "registry_revision": 7,
				"occurred_at": "2026-08-01T12:00:00Z",
				"actor_kind":  "platform_admin", "actor_id": "admin_1",
				"action": "alias.approved", "target": "acme",
				"metadata": map[string]any{"request_id": "earq_aaaaaaaaaaaaaaaa"},
			}},
		})
	}))
	defer srv.Close()

	events, err := ListAdminRealmEmailAliasAudit(
		context.Background(), srv.URL, "admin-token", "alias.approved", 25)
	if err != nil || len(events) != 1 || events[0].Sequence != 7 ||
		events[0].Target != "acme" {
		t.Fatalf("events = %#v, %v", events, err)
	}
}

func TestRealmEmailAliasListPagesPreserveContinuation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got == "" {
			t.Fatal("missing authorization")
		}
		var next string
		switch got := r.URL.RequestURI(); got {
		case "/v1/accounts/acc_1/realms/realm_aaaaaaaaaaaaaaaa/email-alias-requests?cursor=customer-page-2":
			next = "customer-page-3"
		case "/v1/admin/realm-email-alias-requests?account_id=acc_1&cursor=request-page-2&realm_id=realm_aaaaaaaaaaaaaaaa&status=pending_review":
			next = "request-page-3"
		case "/v1/admin/realm-email-aliases?account_id=acc_1&cursor=alias-page-2&realm_id=realm_aaaaaaaaaaaaaaaa&status=suspended":
			next = "alias-page-3"
		case "/v1/admin/realm-email-reserved-names?category=infrastructure&cursor=reserved-page-2&enabled=false":
			next = "reserved-page-3"
		case "/v1/admin/realm-email-alias-audit?action=alias.approved&cursor=audit-page-2&limit=500":
			next = "audit-page-3"
		default:
			t.Fatalf("unexpected request URI %q", got)
		}
		response := map[string]any{
			"schema_version": RealmEmailAliasSchemaVersion,
			"truncated":      true,
			"next_cursor":    next,
		}
		switch r.URL.Path {
		case "/v1/admin/realm-email-aliases":
			response["aliases"] = []any{}
		case "/v1/admin/realm-email-reserved-names":
			response["reserved_names"] = []any{}
			response["reserved_policy_version"] = 42
		case "/v1/admin/realm-email-alias-audit":
			response["events"] = []any{}
		default:
			response["requests"] = []any{}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer srv.Close()

	customerPage, err := ListRealmEmailAliasRequestsPage(context.Background(),
		srv.URL, "operator-token", "acc_1", "realm_aaaaaaaaaaaaaaaa",
		"customer-page-2")
	if err != nil || customerPage.NextCursor != "customer-page-3" ||
		!customerPage.Truncated || len(customerPage.Requests) != 0 {
		t.Fatalf("customer page = %#v, %v", customerPage, err)
	}

	requestPage, err := ListAdminRealmEmailAliasRequestsPage(context.Background(),
		srv.URL, "admin-token", AdminRealmEmailAliasRequestFilter{
			Status: "pending_review", AccountID: "acc_1",
			RealmID: "realm_aaaaaaaaaaaaaaaa", Cursor: "request-page-2",
		})
	if err != nil || requestPage.NextCursor != "request-page-3" ||
		!requestPage.Truncated || len(requestPage.Requests) != 0 {
		t.Fatalf("request page = %#v, %v", requestPage, err)
	}

	aliasPage, err := ListAdminRealmEmailAliasesPage(context.Background(),
		srv.URL, "admin-token", AdminRealmEmailAliasFilter{
			Status: "suspended", AccountID: "acc_1",
			RealmID: "realm_aaaaaaaaaaaaaaaa", Cursor: "alias-page-2",
		})
	if err != nil || aliasPage.NextCursor != "alias-page-3" ||
		!aliasPage.Truncated || len(aliasPage.Aliases) != 0 {
		t.Fatalf("alias page = %#v, %v", aliasPage, err)
	}

	enabled := false
	reservedPage, err := ListAdminRealmEmailReservedNamesPage(context.Background(),
		srv.URL, "admin-token", AdminRealmEmailReservedNameFilter{
			Category: "infrastructure", Enabled: &enabled, Cursor: "reserved-page-2",
		})
	if err != nil || reservedPage.NextCursor != "reserved-page-3" ||
		!reservedPage.Truncated || len(reservedPage.ReservedNames) != 0 ||
		reservedPage.ReservedPolicyVersion != 42 {
		t.Fatalf("reserved page = %#v, %v", reservedPage, err)
	}

	auditPage, err := ListAdminRealmEmailAliasAuditPage(context.Background(),
		srv.URL, "admin-token", AdminRealmEmailAliasAuditFilter{
			Action: "alias.approved", Limit: 500, Cursor: "audit-page-2",
		})
	if err != nil || auditPage.NextCursor != "audit-page-3" ||
		!auditPage.Truncated || len(auditPage.Events) != 0 {
		t.Fatalf("audit page = %#v, %v", auditPage, err)
	}
}
