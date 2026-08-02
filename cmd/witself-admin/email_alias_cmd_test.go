package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestEmailAliasAdminCLIReservedAndApproval(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer admin-token" {
			t.Fatal("missing admin token")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["idempotency_key"] == nil || body["reason"] == nil {
			t.Fatalf("mutation body = %#v", body)
		}
		switch r.URL.Path {
		case "/v1/admin/realm-email-reserved-names":
			if body["name"] != "witself" || body["category"] != "platform_brand" ||
				body["internal_assignable"] != true {
				t.Fatalf("reserved body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.realm-email-alias.v1",
				"reserved_name": map[string]any{
					"name": "witself", "category": "platform_brand", "enabled": true,
				},
			})
		case "/v1/admin/realm-email-alias-requests/earq_aaaaaaaaaaaaaaaa:approve":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.realm-email-alias.v1",
				"request": map[string]any{
					"id": "earq_aaaaaaaaaaaaaaaa", "alias": "acme", "status": "approved",
				},
				"assignment": map[string]any{
					"claim_id": "era_bbbbbbbbbbbbbbbb", "alias": "acme", "status": "active",
				},
			})
		case "/v1/admin/realm-email-aliases/witself:abort-provisioning":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.realm-email-alias.v1",
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

	if code := emailAliasAdminReserved([]string{
		"add", "--endpoint", srv.URL, "--token", "admin-token",
		"--name", "witself", "--category", "platform_brand",
		"--internal-assignable", "true",
		"--reason", "protect product name", "--idempotency-key", "reserved-1",
		"--json",
	}); code != 0 {
		t.Fatalf("reserved add exit code = %d", code)
	}
	if code := emailAliasAdminRequests([]string{
		"approve", "--endpoint", srv.URL, "--token", "admin-token",
		"--request", "earq_aaaaaaaaaaaaaaaa", "--reason", "reviewed",
		"--idempotency-key", "approve-1", "--json",
	}); code != 0 {
		t.Fatalf("approve exit code = %d", code)
	}
	if code := emailAliasAdminAssignments([]string{
		"abort-provisioning", "--endpoint", srv.URL,
		"--token", "admin-token", "--alias", "witself",
		"--reason", "terminal recovery", "--idempotency-key", "abort-1",
		"--json",
	}); code != 0 {
		t.Fatalf("abort provisioning exit code = %d", code)
	}
	want := []string{
		"POST /v1/admin/realm-email-reserved-names",
		"POST /v1/admin/realm-email-alias-requests/earq_aaaaaaaaaaaaaaaa:approve",
		"POST /v1/admin/realm-email-aliases/witself:abort-provisioning",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v; want %#v", calls, want)
	}
}

func TestEmailAliasAdminCLIRejectsCustomerOverrideShape(t *testing.T) {
	if code := emailAliasAdminReserved([]string{
		"add", "--name", "mail", "--category", "infrastructure",
	}); code != 2 {
		t.Fatalf("missing audit reason exit code = %d", code)
	}
	if code := emailAliasAdminAssignments([]string{
		"suspend", "--alias", "acme", "--account", "acc_1",
		"--reason", "policy",
	}); code != 2 {
		t.Fatalf("account-scoped suspend exit code = %d", code)
	}
}

func TestEmailAliasAdminCLIAudit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/v1/admin/realm-email-alias-audit?action=alias.suspend&limit=10" {
			t.Fatalf("request URI = %q", r.URL.RequestURI())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.realm-email-alias.v1",
			"events": []map[string]any{{
				"sequence": 3, "registry_revision": 3,
				"occurred_at": "2026-08-01T12:00:00Z",
				"actor_kind":  "platform_admin", "actor_id": "admin_1",
				"action": "alias.suspend", "target": "acme",
			}},
		})
	}))
	defer srv.Close()

	if code := emailAliasAdminAudit([]string{
		"--endpoint", srv.URL, "--token", "admin-token",
		"--action", "alias.suspend", "--limit", "10", "--json",
	}); code != 0 {
		t.Fatalf("audit exit code = %d", code)
	}
	if code := emailAliasAdminAudit([]string{"--limit", "501"}); code != 2 {
		t.Fatalf("invalid audit limit exit code = %d", code)
	}
}

func TestEmailAliasAdminCLIListPaginationAndFilters(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.RequestURI())
		response := map[string]any{
			"schema_version": "witself.realm-email-alias.v1",
			"truncated":      true,
			"next_cursor":    "next-page",
		}
		switch r.URL.Path {
		case "/v1/admin/realm-email-alias-requests":
			response["requests"] = []any{}
		case "/v1/admin/realm-email-aliases":
			response["aliases"] = []any{}
		case "/v1/admin/realm-email-reserved-names":
			response["reserved_names"] = []any{}
		case "/v1/admin/realm-email-alias-audit":
			response["events"] = []any{}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer srv.Close()

	common := []string{"--endpoint", srv.URL, "--token", "admin-token", "--json"}
	if code := emailAliasAdminRequests(append([]string{"list",
		"--status", "pending_review", "--cursor", "request-page-2"}, common...)); code != 0 {
		t.Fatalf("request list exit code = %d", code)
	}
	if code := emailAliasAdminAssignments(append([]string{"list",
		"--account", "acc_1", "--cursor", "alias-page-2"}, common...)); code != 0 {
		t.Fatalf("assignment list exit code = %d", code)
	}
	if code := emailAliasAdminReserved(append([]string{"list",
		"--category", "infrastructure", "--enabled", "false",
		"--cursor", "reserved-page-2"}, common...)); code != 0 {
		t.Fatalf("reserved list exit code = %d", code)
	}
	if code := emailAliasAdminAudit(append([]string{
		"--action", "alias.approved", "--limit", "500", "--cursor", "audit-page-2",
	}, common...)); code != 0 {
		t.Fatalf("audit exit code = %d", code)
	}

	want := []string{
		"/v1/admin/realm-email-alias-requests?cursor=request-page-2&status=pending_review",
		"/v1/admin/realm-email-aliases?account_id=acc_1&cursor=alias-page-2",
		"/v1/admin/realm-email-reserved-names?category=infrastructure&cursor=reserved-page-2&enabled=false",
		"/v1/admin/realm-email-alias-audit?action=alias.approved&cursor=audit-page-2&limit=500",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v; want %#v", calls, want)
	}
}
