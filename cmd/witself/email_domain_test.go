package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEmailDomainCLIRequestAndList(t *testing.T) {
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
				"schema_version": "witself.agent-email-domain.v1",
				"request": map[string]any{
					"id": "aedr_aaaaaaaaaaaaaaaa", "account_id": "acc_1",
					"domain": "agents.example.com", "state": "pending_verification",
					"ownership_challenge": map[string]any{
						"record_name": "_witself-verification.agents.example.com",
						"record_type": "TXT", "record_value": "proof-1",
					},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.agent-email-domain.v1",
			"requests": []map[string]any{{
				"id": "aedr_aaaaaaaaaaaaaaaa", "account_id": "acc_1",
				"domain": "agents.example.com", "state": "pending_verification",
			}},
			"truncated": true, "next_cursor": "customer-page-3",
		})
	}))
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "operator.token")
	if err := os.WriteFile(tokenPath, []byte("operator-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connection := []string{
		"--endpoint", srv.URL, "--token-file", tokenPath,
		"--account-id", "acc_1", "--json",
	}
	if code := run(append([]string{
		"email-domain", "request", "--domain", "agents.example.com",
		"--idempotency-key", "request-1",
	}, connection...)); code != 0 {
		t.Fatalf("request exit code = %d", code)
	}
	if code := run(append([]string{
		"email-domain", "list", "--cursor", "customer-page-2",
	}, connection...)); code != 0 {
		t.Fatalf("list exit code = %d", code)
	}

	want := []string{
		"POST /v1/accounts/acc_1/email-domain-requests",
		"GET /v1/accounts/acc_1/email-domain-requests?cursor=customer-page-2",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v; want %#v", calls, want)
	}
}

func TestEmailDomainCLIInputValidation(t *testing.T) {
	if code := run([]string{"email-domain"}); code != 2 {
		t.Fatalf("missing action exit code = %d", code)
	}
	if code := emailDomainRequest(nil); code != 2 {
		t.Fatalf("missing domain exit code = %d", code)
	}
	if code := emailDomainList([]string{"--account-id", "acc_1"}); code != 1 {
		t.Fatalf("account id without token file exit code = %d", code)
	}
	if code := emailDomainCmd([]string{"activate"}); code != 2 {
		t.Fatalf("unsupported activation exit code = %d", code)
	}
}
