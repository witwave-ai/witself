package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestEmailDomainAdminCLIDispatchAndLifecycle(t *testing.T) {
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
				"schema_version": "witself.agent-email-domain.v1",
				"requests": []map[string]any{{
					"id": "aedr_aaaaaaaaaaaaaaaa", "account_id": "acc_1",
					"domain": "agents.example.com", "state": "pending_verification",
				}},
				"truncated": true, "next_cursor": "request-page-3",
			})
		case "/v1/admin/agent-email-domain-requests/aedr_aaaaaaaaaaaaaaaa":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.agent-email-domain.v1",
				"request": map[string]any{
					"id": "aedr_aaaaaaaaaaaaaaaa", "account_id": "acc_1",
					"domain": "agents.example.com", "state": "pending_verification",
					"ownership_challenge": map[string]any{
						"record_name": "_witself.agents.example.com",
						"record_type": "TXT", "record_value": "proof-1",
					},
				},
			})
		case "/v1/admin/agent-email-domain-requests/aedr_aaaaaaaaaaaaaaaa:reject":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.agent-email-domain.v1",
				"request": map[string]any{
					"id": "aedr_aaaaaaaaaaaaaaaa", "account_id": "acc_1",
					"domain": "agents.example.com", "state": "rejected",
				},
			})
		case "/v1/admin/agent-email-domain-requests/aedr_bbbbbbbbbbbbbbbb:retire":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.agent-email-domain.v1",
				"request": map[string]any{
					"id": "aedr_bbbbbbbbbbbbbbbb", "account_id": "acc_1",
					"domain": "old.example.com", "state": "retired",
				},
			})
		case "/v1/admin/agent-email-domain-audit":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.agent-email-domain.v1",
				"events": []map[string]any{{
					"sequence": 3, "occurred_at": "2026-08-03T12:00:00Z",
					"action": "custom_domain.rejected", "target": "agents.example.com",
					"metadata":   map[string]any{"account_id": "acc_1", "reason": "policy"},
					"actor_kind": "platform_admin", "actor_id": "admin_1",
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	common := []string{"--endpoint", srv.URL, "--token", "admin-token", "--json"}
	if code := run(append([]string{
		"email-domain", "requests", "list", "--state", "pending_verification",
		"--account", "acc_1", "--domain", "agents.example.com",
		"--cursor", "request-page-2",
	}, common...)); code != 0 {
		t.Fatalf("list exit code = %d", code)
	}
	if code := run(append([]string{
		"email-domain", "requests", "show", "--request", "aedr_aaaaaaaaaaaaaaaa",
	}, common...)); code != 0 {
		t.Fatalf("show exit code = %d", code)
	}
	if code := run(append([]string{
		"email-domain", "requests", "reject", "--request", "aedr_aaaaaaaaaaaaaaaa",
		"--reason", "policy", "--idempotency-key", "reject-1",
	}, common...)); code != 0 {
		t.Fatalf("reject exit code = %d", code)
	}
	if code := run(append([]string{
		"email-domain", "requests", "retire", "--request", "aedr_bbbbbbbbbbbbbbbb",
		"--reason", "closed", "--idempotency-key", "retire-1",
	}, common...)); code != 0 {
		t.Fatalf("retire exit code = %d", code)
	}
	if code := run(append([]string{
		"email-domain", "audit", "--action", "custom_domain.rejected",
		"--account", "acc_1", "--domain", "agents.example.com",
		"--cursor", "audit-page-2", "--limit", "100",
	}, common...)); code != 0 {
		t.Fatalf("audit exit code = %d", code)
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

func TestEmailDomainAdminCLIRejectsUnsafeArguments(t *testing.T) {
	if code := run([]string{"email-domain"}); code != 2 {
		t.Fatalf("missing subcommand exit code = %d", code)
	}
	if code := emailDomainAdminRequests([]string{
		"reject", "--request", "aedr_aaaaaaaaaaaaaaaa",
	}); code != 2 {
		t.Fatalf("missing reason exit code = %d", code)
	}
	if code := emailDomainAdminRequests([]string{"show"}); code != 2 {
		t.Fatalf("missing request exit code = %d", code)
	}
	if code := emailDomainAdminAudit([]string{"--limit", "101"}); code != 2 {
		t.Fatalf("invalid audit limit exit code = %d", code)
	}
}
