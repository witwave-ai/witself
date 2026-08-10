package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRealmEmailAliasCLIRequestAndList(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer operator-token" {
			t.Fatal("missing operator token")
		}
		if r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["alias"] != "acme" || body["idempotency_key"] != "retry-1" {
				t.Fatalf("body = %#v", body)
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.realm-email-alias.v1",
				"request": map[string]any{
					"id": "earq_bbbbbbbbbbbbbbbb", "alias": "acme", "status": "pending_review",
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.realm-email-alias.v1",
			"requests":       []any{},
			"truncated":      true,
			"next_cursor":    "customer-page-3",
		})
	}))
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "operator.token")
	if err := os.WriteFile(tokenPath, []byte("operator-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := []string{
		"--endpoint", srv.URL, "--token-file", tokenPath,
		"--account-id", "acc_1", "--realm", "realm_aaaaaaaaaaaaaaaa", "--json",
	}
	requestArgs := append([]string{}, common...)
	requestArgs = append(requestArgs, "--alias", "acme", "--idempotency-key", "retry-1")
	if code := realmEmailAliasRequest(requestArgs); code != 0 {
		t.Fatalf("request exit code = %d", code)
	}
	listArgs := append([]string{}, common...)
	listArgs = append(listArgs, "--cursor", "customer-page-2")
	if code := realmEmailAliasList(listArgs); code != 0 {
		t.Fatalf("list exit code = %d", code)
	}
	want := []string{
		"POST /v1/accounts/acc_1/realms/realm_aaaaaaaaaaaaaaaa/email-alias-requests",
		"GET /v1/accounts/acc_1/realms/realm_aaaaaaaaaaaaaaaa/email-alias-requests?cursor=customer-page-2",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v; want %#v", calls, want)
	}
}

func TestRealmEmailAliasCLIInputValidation(t *testing.T) {
	if code := realmEmailAliasRequest([]string{"--realm", "realm_aaaaaaaaaaaaaaaa"}); code != 2 {
		t.Fatalf("missing alias exit code = %d", code)
	}
	if code := realmEmailAliasList([]string{"--realm", "realm_aaaaaaaaaaaaaaaa", "--account-id", "acc_1"}); code != 1 {
		t.Fatalf("account id without token file exit code = %d", code)
	}
}

func TestRealmEmailAliasCLIHumanOutputSanitizesTerminalControls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.realm-email-alias.v1",
			"requests": []map[string]any{{
				"id": "earq_\x1b[31mred", "alias": "acme\tforged",
				"status": "pending_review\nforged",
			}},
			"truncated": true, "next_cursor": "next\u009b31m\nforged",
		})
	}))
	defer srv.Close()
	tokenPath := filepath.Join(t.TempDir(), "operator.token")
	if err := os.WriteFile(tokenPath, []byte("operator-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return realmEmailAliasList([]string{
			"--endpoint", srv.URL, "--token-file", tokenPath,
			"--account-id", "acc_1", "--realm", "realm_aaaaaaaaaaaaaaaa",
		})
	})
	if code != 0 {
		t.Fatalf("list = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	combined := stdout + stderr
	if strings.ContainsAny(combined, "\x1b\u009b") || strings.Contains(combined, "\nforged") {
		t.Fatalf("output retained terminal or row injection: %q", combined)
	}
	for _, want := range []string{
		"earq_[31mred", "acme forged", "pending_review forged",
		"next cursor: next31m forged",
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("output omitted sanitized value %q: %q", want, combined)
		}
	}
}
