package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/local"
)

func TestDefaultBootstrapTokenPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)

	got, err := defaultBootstrapTokenPath("aws-sandbox-usw2-dev")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "tokens", "aws-sandbox-usw2-dev", "bootstrap.token")
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
			DisplayName string `json:"display_name"`
			TTL         string `json:"ttl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.DisplayName != "deploy bot" {
			t.Errorf("display_name = %q, want deploy bot", body.DisplayName)
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
		"--name", "deploy bot",
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
	if code := run([]string{"token", "create", "--endpoint", "http://example.test", "--token-file", "token", "--agent", "agent_1", "--name", "deploy bot"}); code != 2 {
		t.Fatalf("agent name code = %d, want 2", code)
	}
}

func TestOperatorLifecycleCommands(t *testing.T) {
	var deletedOperator, revokedToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer witself_opr_parent" {
			t.Errorf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operators":
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","operators":[{"id":"opr_1","display_name":"owner","role":"account_owner","is_root":true,"created_at":"2026-07-02T00:00:00Z","updated_at":"2026-07-02T00:00:00Z","tokens":[{"id":"tok_1","display_name":"laptop","created_at":"2026-07-02T00:00:00Z"}]}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/operators":
			var body struct {
				DisplayName      string `json:"display_name"`
				TokenDisplayName string `json:"token_display_name"`
				TTL              string `json:"ttl"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.DisplayName != "deploy bot" || body.TokenDisplayName != "deploy token" || body.TTL != "24h" {
				t.Errorf("create body = %+v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","operator":{"id":"opr_2","display_name":"deploy bot","role":"account_operator","is_root":false,"created_at":"2026-07-02T00:00:00Z","updated_at":"2026-07-02T00:00:00Z","tokens":[{"id":"tok_2","display_name":"deploy token","created_at":"2026-07-02T00:00:00Z"}]},"operator_token":"witself_opr_new"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/operators/opr_2":
			deletedOperator = strings.TrimPrefix(r.URL.Path, "/v1/operators/")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tokens/tok_2:revoke":
			revokedToken = "tok_2"
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.token")
	out := filepath.Join(dir, "new.token")
	if err := os.WriteFile(parent, []byte("witself_opr_parent\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := run([]string{"operator", "list", "--endpoint", srv.URL, "--token-file", parent}); code != 0 {
		t.Fatalf("operator list code = %d, want 0", code)
	}
	if code := run([]string{
		"operator", "create",
		"--endpoint", srv.URL,
		"--token-file", parent,
		"--name", "deploy bot",
		"--token-name", "deploy token",
		"--ttl", "24h",
		"--out", out,
	}); code != 0 {
		t.Fatalf("operator create code = %d, want 0", code)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "witself_opr_new\n" {
		t.Fatalf("operator token file = %q", got)
	}
	if code := run([]string{"operator", "delete", "--endpoint", srv.URL, "--token-file", parent, "--yes", "opr_2"}); code != 0 {
		t.Fatalf("operator delete code = %d, want 0", code)
	}
	if deletedOperator != "opr_2" {
		t.Fatalf("deletedOperator = %q, want opr_2", deletedOperator)
	}
	if code := run([]string{"token", "revoke", "--endpoint", srv.URL, "--token-file", parent, "--token", "tok_2", "--yes"}); code != 0 {
		t.Fatalf("token revoke code = %d, want 0", code)
	}
	if revokedToken != "tok_2" {
		t.Fatalf("revokedToken = %q, want tok_2", revokedToken)
	}
}

func TestDestructiveCommandsRequireYes(t *testing.T) {
	if code := run([]string{"operator", "delete", "--endpoint", "http://example.test", "--token-file", "token", "opr_1"}); code != 2 {
		t.Fatalf("operator delete without yes code = %d, want 2", code)
	}
	if code := run([]string{"realm", "delete", "--endpoint", "http://example.test", "--token-file", "token", "realm_1"}); code != 2 {
		t.Fatalf("realm delete without yes code = %d, want 2", code)
	}
	if code := run([]string{"agent", "delete", "--endpoint", "http://example.test", "--token-file", "token", "--realm", "realm_1", "agent_1"}); code != 2 {
		t.Fatalf("agent delete without yes code = %d, want 2", code)
	}
	if code := run([]string{"token", "revoke", "--endpoint", "http://example.test", "--token-file", "token", "--token", "tok_1"}); code != 2 {
		t.Fatalf("token revoke without yes code = %d, want 2", code)
	}
}

func TestAccountForget(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	if err := local.Save("stale", local.Account{ID: "acc_1"}, "witself_opr_dead"); err != nil {
		t.Fatal(err)
	}

	if code := run([]string{"account", "forget", "--account", "stale"}); code != 2 {
		t.Fatalf("forget without --yes code = %d, want 2", code)
	}
	if _, _, _, err := local.Resolve("stale"); err != nil {
		t.Fatalf("binding gone after refused forget: %v", err)
	}

	if code := run([]string{"account", "forget", "--account", "stale", "--yes"}); code != 0 {
		t.Fatalf("forget code = %d, want 0", code)
	}
	cfg, err := local.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Accounts["stale"]; ok {
		t.Fatal("config still has the forgotten account")
	}
	tp, err := local.TokenPath("stale")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token file still present after forget: stat err = %v", err)
	}
}

func TestAccountForgetRequiresExplicitName(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	t.Setenv("WITSELF_ACCOUNT", "stale") // must never stand in for --account
	if code := run([]string{"account", "forget", "--yes"}); code != 2 {
		t.Fatalf("forget without --account code = %d, want 2", code)
	}
}

func TestAccountForgetUnknownName(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	if code := run([]string{"account", "forget", "--account", "nope", "--yes"}); code != 1 {
		t.Fatalf("forget unknown name code = %d, want 1", code)
	}
}
