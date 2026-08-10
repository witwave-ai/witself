package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
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

func TestEmailAliasAdminCLIHumanOutputSanitizesTerminalControlsAndPreservesJSON(t *testing.T) {
	requestID := "earq_\x1b[31mred"
	alias := "acme\tforged"
	accountID := "acc_1\nforged"
	realmID := "realm_\u009b31mred"
	status := "pending_review\nforged"
	nextCursor := "next\x1b[2J\tpage\nforged"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.realm-email-alias.v1",
			"requests": []map[string]any{{
				"id": requestID, "alias": alias, "account_id": accountID,
				"realm_id": realmID, "status": status,
			}},
			"truncated": true, "next_cursor": nextCursor,
		})
	}))
	defer srv.Close()
	base := []string{"list", "--endpoint", srv.URL, "--token", "admin-token"}

	stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int {
		return emailAliasAdminRequests(base)
	})
	if code != 0 {
		t.Fatalf("plain list = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	combined := stdout + stderr
	if strings.ContainsAny(combined, "\x1b\u009b") || strings.Contains(combined, "\nforged") {
		t.Fatalf("plain output retained terminal or row injection: %q", combined)
	}
	for _, want := range []string{
		"earq_[31mred", "acme forged", "acc_1 forged", "realm_31mred",
		"pending_review forged", "next cursor: next[2J page forged",
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("plain output omitted sanitized value %q: %q", want, combined)
		}
	}

	stdout, stderr, code = captureEmailAliasAdminCLI(t, func() int {
		return emailAliasAdminRequests(append(append([]string{}, base...), "--json"))
	})
	if code != 0 || stderr != "" {
		t.Fatalf("JSON list = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	var page struct {
		Requests []struct {
			ID        string `json:"id"`
			Alias     string `json:"alias"`
			AccountID string `json:"account_id"`
			RealmID   string `json:"realm_id"`
			Status    string `json:"status"`
		} `json:"requests"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(stdout), &page); err != nil {
		t.Fatalf("decode JSON output: %v\n%s", err, stdout)
	}
	if len(page.Requests) != 1 || page.Requests[0].ID != requestID ||
		page.Requests[0].Alias != alias || page.Requests[0].AccountID != accountID ||
		page.Requests[0].RealmID != realmID || page.Requests[0].Status != status ||
		page.NextCursor != nextCursor {
		t.Fatalf("JSON output changed remote values: %#v", page)
	}
}

func captureEmailAliasAdminCLI(t *testing.T, fn func() int) (stdout, stderr string, code int) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code = fn()
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()
	outBytes, outReadErr := io.ReadAll(outR)
	errBytes, errReadErr := io.ReadAll(errR)
	_ = outR.Close()
	_ = errR.Close()
	if outReadErr != nil || errReadErr != nil {
		t.Fatalf("read captured output: stdout=%v stderr=%v", outReadErr, errReadErr)
	}
	return string(outBytes), string(errBytes), code
}
