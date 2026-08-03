package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestAgentEmailDomainCustomerRequests(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer operator-token" {
			t.Fatalf("authorization = %q", got)
		}
		if r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["domain"] != "agents.example.com" ||
				body["idempotency_key"] != "request-1" {
				t.Fatalf("body = %#v", body)
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": AgentEmailDomainSchemaVersion,
				"request": map[string]any{
					"id":         "aedr_aaaaaaaaaaaaaaaa",
					"account_id": "acc_1", "domain": "agents.example.com",
					"state": "pending_verification",
					"ownership_challenge": map[string]any{
						"record_name": "_witself.agents.example.com",
						"record_type": "TXT", "record_value": "proof-1",
					},
					"plan_revision": 7, "plan_snapshot_hash": "plan-hash",
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": AgentEmailDomainSchemaVersion,
			"requests": []map[string]any{{
				"id": "aedr_aaaaaaaaaaaaaaaa", "account_id": "acc_1",
				"domain": "agents.example.com", "state": "pending_verification",
			}},
			"truncated": true, "next_cursor": "customer-page-3",
		})
	}))
	defer srv.Close()

	request, err := RequestAgentEmailDomain(context.Background(), srv.URL,
		"operator-token", " acc_1 ", " agents.example.com ", " request-1 ")
	if err != nil || request.RequestID != "aedr_aaaaaaaaaaaaaaaa" ||
		request.OwnershipChallenge == nil || request.OwnershipChallenge.RecordType != "TXT" ||
		request.PlanRevision != 7 || request.PlanSnapshotHash != "plan-hash" {
		t.Fatalf("request = %#v, %v", request, err)
	}
	page, err := ListAgentEmailDomainRequestsPage(context.Background(), srv.URL,
		"operator-token", "acc_1", " customer-page-2 ")
	if err != nil || len(page.Requests) != 1 || !page.Truncated ||
		page.NextCursor != "customer-page-3" {
		t.Fatalf("page = %#v, %v", page, err)
	}
	want := []string{
		"POST /v1/accounts/acc_1/email-domain-requests",
		"GET /v1/accounts/acc_1/email-domain-requests?cursor=customer-page-2",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v; want %#v", calls, want)
	}
}

func TestAdminAgentEmailDomainRequestLifecycleAndAudit(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer admin-token" {
			t.Fatalf("authorization = %q", got)
		}
		if r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["idempotency_key"] == nil || body["reason"] == nil {
				t.Fatalf("mutation body = %#v", body)
			}
		}
		switch r.URL.Path {
		case "/v1/admin/agent-email-domain-requests":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": AgentEmailDomainSchemaVersion,
				"requests": []map[string]any{{
					"id": "aedr_aaaaaaaaaaaaaaaa", "account_id": "acc_1",
					"domain": "agents.example.com", "state": "pending_verification",
				}},
				"truncated": true, "next_cursor": "request-page-3",
			})
		case "/v1/admin/agent-email-domain-requests/aedr_aaaaaaaaaaaaaaaa":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": AgentEmailDomainSchemaVersion,
				"request": map[string]any{
					"id": "aedr_aaaaaaaaaaaaaaaa", "account_id": "acc_1",
					"domain": "agents.example.com", "state": "pending_verification",
				},
			})
		case "/v1/admin/agent-email-domain-requests/aedr_aaaaaaaaaaaaaaaa:reject":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": AgentEmailDomainSchemaVersion,
				"request": map[string]any{
					"id": "aedr_aaaaaaaaaaaaaaaa", "account_id": "acc_1",
					"domain": "agents.example.com", "state": "rejected",
					"decision": map[string]any{"action": "rejected", "reason": "policy"},
				},
			})
		case "/v1/admin/agent-email-domain-requests/aedr_bbbbbbbbbbbbbbbb:retire":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": AgentEmailDomainSchemaVersion,
				"request": map[string]any{
					"id": "aedr_bbbbbbbbbbbbbbbb", "account_id": "acc_1",
					"domain": "old.example.com", "state": "retired", "reason": "closed",
				},
			})
		case "/v1/admin/agent-email-domain-audit":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": AgentEmailDomainSchemaVersion,
				"events": []map[string]any{{
					"sequence": 9, "registry_revision": 9,
					"occurred_at": "2026-08-03T12:00:00Z",
					"actor_kind":  "platform_admin", "actor_id": "admin_1",
					"action": "custom_domain.rejected", "target": "agents.example.com",
					"metadata": map[string]any{"account_id": "acc_1", "reason": "policy"},
				}},
				"truncated": true, "next_cursor": "audit-page-3",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	page, err := ListAdminAgentEmailDomainRequestsPage(context.Background(),
		srv.URL, "admin-token", AdminAgentEmailDomainRequestFilter{
			State: "pending_verification", AccountID: "acc_1",
			Domain: "agents.example.com", Cursor: "request-page-2",
		})
	if err != nil || len(page.Requests) != 1 || page.NextCursor != "request-page-3" {
		t.Fatalf("request page = %#v, %v", page, err)
	}
	shown, err := GetAdminAgentEmailDomainRequest(context.Background(), srv.URL,
		"admin-token", "aedr_aaaaaaaaaaaaaaaa")
	if err != nil || shown.Domain != "agents.example.com" {
		t.Fatalf("shown = %#v, %v", shown, err)
	}
	rejected, err := RejectAdminAgentEmailDomainRequest(context.Background(),
		srv.URL, "admin-token", "aedr_aaaaaaaaaaaaaaaa", "reject-1", "policy")
	if err != nil || rejected.State != "rejected" || rejected.Decision == nil ||
		rejected.Decision.Reason != "policy" {
		t.Fatalf("rejected = %#v, %v", rejected, err)
	}
	retired, err := RetireAdminAgentEmailDomainRequest(context.Background(),
		srv.URL, "admin-token", "aedr_bbbbbbbbbbbbbbbb", "retire-1", "closed")
	if err != nil || retired.State != "retired" {
		t.Fatalf("retired = %#v, %v", retired, err)
	}
	audit, err := ListAdminAgentEmailDomainAuditPage(context.Background(), srv.URL,
		"admin-token", AdminAgentEmailDomainAuditFilter{
			Action: "custom_domain.rejected", AccountID: "acc_1", Domain: "agents.example.com",
			Limit: 100, Cursor: "audit-page-2",
		})
	if err != nil || len(audit.Events) != 1 || audit.Events[0].Sequence != 9 ||
		len(audit.Events[0].Metadata) == 0 ||
		audit.NextCursor != "audit-page-3" {
		t.Fatalf("audit = %#v, %v", audit, err)
	}

	want := []string{
		"GET /v1/admin/agent-email-domain-requests?account_id=acc_1&cursor=request-page-2&domain=agents.example.com&state=pending_verification",
		"GET /v1/admin/agent-email-domain-requests/aedr_aaaaaaaaaaaaaaaa",
		"POST /v1/admin/agent-email-domain-requests/aedr_aaaaaaaaaaaaaaaa:reject",
		"POST /v1/admin/agent-email-domain-requests/aedr_bbbbbbbbbbbbbbbb:retire",
		"GET /v1/admin/agent-email-domain-audit?account_id=acc_1&action=custom_domain.rejected&cursor=audit-page-2&domain=agents.example.com&limit=100",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v; want %#v", calls, want)
	}
}

func TestAgentEmailDomainClientRejectsUnsafeIdentifiersAndEmptyMutations(t *testing.T) {
	if _, err := ListAgentEmailDomainRequests(context.Background(),
		"https://example.invalid", "operator-token", "bad/account"); err == nil {
		t.Fatal("unsafe account id accepted")
	}
	if _, err := GetAdminAgentEmailDomainRequest(context.Background(),
		"https://example.invalid", "admin-token", "aedr_bad"); err == nil {
		t.Fatal("invalid request id accepted")
	}
	if _, err := RejectAdminAgentEmailDomainRequest(context.Background(),
		"https://example.invalid", "admin-token", "aedr_aaaaaaaaaaaaaaaa",
		"", "reason"); err == nil {
		t.Fatal("empty idempotency key accepted")
	}
	for _, limit := range []int{-1, 101} {
		if _, err := ListAdminAgentEmailDomainAuditPage(
			context.Background(), "https://example.invalid", "admin-token",
			AdminAgentEmailDomainAuditFilter{Limit: limit},
		); err == nil {
			t.Fatalf("invalid audit limit %d accepted", limit)
		}
	}
}
