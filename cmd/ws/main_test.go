package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultBootstrapTokenPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)

	got, err := defaultBootstrapTokenPath("aws-sandbox-usw2-dev")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "bootstrap", "aws-sandbox-usw2-dev", "bootstrap-token")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestDefaultBootstrapTokenPathRejectsTraversal(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	if _, err := defaultBootstrapTokenPath("../aws-sandbox-usw2-dev"); err == nil {
		t.Fatal("path traversal cell name = nil error, want error")
	}
}

func TestTokenCreateOperatorWritesOutFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/operators/self/tokens" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer witself_opr_parent" {
			t.Errorf("Authorization = %q", got)
		}
		var body struct {
			TTL string `json:"ttl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.TTL != "24h" {
			t.Errorf("ttl = %q, want 24h", body.TTL)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","operator_token":"witself_opr_child","operator_id":"opr_1"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.token")
	out := filepath.Join(dir, "child.token")
	if err := os.WriteFile(parent, []byte("witself_opr_parent\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code := run([]string{
		"token", "create",
		"--endpoint", srv.URL,
		"--token-file", parent,
		"--operator",
		"--ttl", "24h",
		"--out", out,
	})
	if code != 0 {
		t.Fatalf("run code = %d, want 0", code)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "witself_opr_child\n" {
		t.Fatalf("token file = %q", got)
	}
}

func TestTokenCreateRequiresOneSubject(t *testing.T) {
	if code := run([]string{"token", "create", "--endpoint", "http://example.test", "--token-file", "token"}); code != 2 {
		t.Fatalf("missing token subject code = %d, want 2", code)
	}
	if code := run([]string{"token", "create", "--endpoint", "http://example.test", "--token-file", "token", "--agent", "agent_1", "--operator"}); code != 2 {
		t.Fatalf("two token subjects code = %d, want 2", code)
	}
	if code := run([]string{"token", "create", "--endpoint", "http://example.test", "--token-file", "token", "--agent", "agent_1", "--ttl", "24h"}); code != 2 {
		t.Fatalf("agent ttl code = %d, want 2", code)
	}
}
