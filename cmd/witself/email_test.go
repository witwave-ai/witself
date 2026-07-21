package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestEmailCLIAddressListAndRead(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/email/address":
			_, _ = w.Write([]byte(`{"address":{"id":"eaddr_aaaaaaaaaaaaaaaa","address":"owner.realm@example.com","receive_state":"enabled"}}`))
		case "/v1/email":
			_, _ = w.Write([]byte(`{"messages":[{"id":"emsg_aaaaaaaaaaaaaaaa","header_from":"sender@example.com","subject":"code","sender_verification_state":"unverified","read_state":{"state":"unread"},"processing":{"state":"available"}}]}`))
		case "/v1/email/emsg_aaaaaaaaaaaaaaaa:read":
			_, _ = w.Write([]byte(`{"message":{"id":"emsg_aaaaaaaaaaaaaaaa","header_from":"sender@example.com","subject":"code","text":"123456","sender_verification_state":"unverified","read_state":{"state":"read"},"processing":{"state":"available"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"--endpoint", srv.URL, "--token-file", tokenFile, "--json"}
	for _, args := range [][]string{
		append([]string{"email", "address", "show"}, base...),
		append([]string{"email", "list", "--unread", "--limit", "5"}, base...),
		append([]string{"email", "read", "emsg_aaaaaaaaaaaaaaaa"}, base...),
	} {
		if code := run(args); code != 0 {
			t.Fatalf("run(%v) = %d", args, code)
		}
	}
	if len(paths) != 3 || paths[0] != "GET /v1/email/address" ||
		paths[1] != "GET /v1/email?limit=5&unread=true" ||
		paths[2] != "POST /v1/email/emsg_aaaaaaaaaaaaaaaa:read" {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestEmailCLIClaimSendsFenceInputs(t *testing.T) {
	var body map[string]any
	var key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"processing":{"state":"claimed","claim_id":"ecl_aaaaaaaaaaaaaaaa","generation":1,"failure_count":0}}`))
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{
		"email", "claim", "emsg_aaaaaaaaaaaaaaaa", "--endpoint", srv.URL,
		"--token-file", tokenFile, "--lease", "2m", "--idempotency-key", "claim-1", "--json",
	}); code != 0 {
		t.Fatalf("run = %d", code)
	}
	if key != "claim-1" || body["lease_seconds"] != float64(120) {
		t.Fatalf("request = key %q body %#v", key, body)
	}
}
