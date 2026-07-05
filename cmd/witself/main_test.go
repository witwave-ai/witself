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

	"github.com/witwave-ai/witself/internal/client"
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

// adoptServer plays both the control plane directory and the cell: the
// directory entry for accountID points back at the server itself, and
// /v1/account answers with servedID for the expected token.
func adoptServer(t *testing.T, accountID, servedID, email string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/directory/"+accountID:
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","cell":{"cell":"test-cell","endpoint":"` + srv.URL + `"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/account":
			if got := r.Header.Get("Authorization"); got != "Bearer witself_opr_teammate" {
				t.Errorf("Authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","account":{"id":"` + servedID + `","email":"` + email + `","status":"active","created_at":"2026-07-02T00:00:00Z"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv
}

func TestAccountAdopt(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	srv := adoptServer(t, "acc_1", "acc_1", "teammate@witwave.ai")
	defer srv.Close()

	tf := filepath.Join(t.TempDir(), "teammate.token")
	if err := os.WriteFile(tf, []byte("witself_opr_teammate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := run([]string{
		"account", "adopt",
		"--id", "acc_1",
		"--token-file", tf,
		"--name", "team",
		"--endpoint", srv.URL,
	})
	if code != 0 {
		t.Fatalf("adopt code = %d, want 0", code)
	}
	name, acct, tok, err := local.Resolve("team")
	if err != nil {
		t.Fatal(err)
	}
	if name != "team" || acct.ID != "acc_1" || acct.Email != "teammate@witwave.ai" {
		t.Fatalf("binding = %q %+v", name, acct)
	}
	if tok != "witself_opr_teammate" {
		t.Fatalf("token = %q", tok)
	}
}

func TestAccountAdoptRefusesForeignToken(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	// The cell authenticates the token but as a DIFFERENT account.
	srv := adoptServer(t, "acc_1", "acc_2", "")
	defer srv.Close()

	tf := filepath.Join(t.TempDir(), "teammate.token")
	if err := os.WriteFile(tf, []byte("witself_opr_teammate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := run([]string{
		"account", "adopt",
		"--id", "acc_1",
		"--token-file", tf,
		"--name", "team",
		"--endpoint", srv.URL,
	})
	if code != 1 {
		t.Fatalf("adopt with foreign token code = %d, want 1", code)
	}
	if err := local.Available("team"); err != nil {
		t.Fatalf("name bound despite refused adopt: %v", err)
	}
}

func TestAccountAdoptRequiresFlags(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	// No --name default fallback, and the id must be a raw acc_ id.
	if code := run([]string{"account", "adopt", "--id", "acc_1", "--token-file", "token"}); code != 2 {
		t.Fatalf("adopt without --name code = %d, want 2", code)
	}
	if code := run([]string{"account", "adopt", "--id", "team", "--token-file", "token", "--name", "team"}); code != 2 {
		t.Fatalf("adopt with non-acc_ id code = %d, want 2", code)
	}
	if code := run([]string{"account", "adopt", "--id", "acc_1", "--name", "team"}); code != 2 {
		t.Fatalf("adopt without --token-file code = %d, want 2", code)
	}
}

func TestAccountAdoptRefusesTakenName(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	if err := local.Save("team", local.Account{ID: "acc_9"}, "witself_opr_existing"); err != nil {
		t.Fatal(err)
	}
	tf := filepath.Join(t.TempDir(), "teammate.token")
	if err := os.WriteFile(tf, []byte("witself_opr_teammate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No server: the taken name must fail before any network round trip.
	code := run([]string{
		"account", "adopt",
		"--id", "acc_1",
		"--token-file", tf,
		"--name", "team",
		"--endpoint", "http://127.0.0.1:0",
	})
	if code != 1 {
		t.Fatalf("adopt onto taken name code = %d, want 1", code)
	}
	if _, _, tok, err := local.Resolve("team"); err != nil || tok != "witself_opr_existing" {
		t.Fatalf("existing binding disturbed: tok=%q err=%v", tok, err)
	}
}

// TestSafeTextStripsTerminalEscapes pins the sanitizer the support-
// ticket renderers wrap every operator- or admin-supplied string in.
// Without it, a malicious ticket body containing an ANSI/OSC sequence
// would hijack the reader operators screen the moment they run
// `ws account support show` — clearing the screen, spoofing the
// window title, or moving the cursor so subsequent lines land
// wherever the attacker chose.
func TestSafeTextStripsTerminalEscapes(t *testing.T) {
	tests := map[string]string{
		"\x1b[2J\x1b[Hyou have been pwned":     "[2J[Hyou have been pwned",
		"\x1b]0;URGENT: account suspended\x07": "]0;URGENT: account suspended",
		"before\x08\x08\x08\x08after":          "beforeafter",
		"\x7fDEL":                              "DEL",
		"plain ASCII stays":                    "plain ASCII stays",
		"tabs\tand\nnewlines\tare kept":        "tabs\tand\nnewlines\tare kept",
		"unicode π survives":                   "unicode π survives",
		// C1 controls: U+009B is a single-rune CSI (ESC-[ equivalent),
		// U+009D a single-rune OSC — C1-honoring terminals execute them.
		"\u009b0mreset smuggled":    "0mreset smuggled",
		"\u009d0;title spoof\u009c": "0;title spoof",
	}
	for in, want := range tests {
		if got := safeText(in); got != want {
			t.Errorf("safeText(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSummarizeSupport pins the exact wording shown in `ws account
// status` when support is enabled. The line is a UX contract with
// operators — scripts and docs quote it — so any drift is worth
// flagging.
func TestSummarizeSupport(t *testing.T) {
	tk := func(state string) client.SupportTicket {
		return client.SupportTicket{State: state}
	}
	tests := []struct {
		name string
		in   []client.SupportTicket
		want string
	}{
		{"no tickets", nil, "no open tickets"},
		{"closed only",
			[]client.SupportTicket{tk("closed"), tk("closed")},
			"no open tickets"},
		{"one awaiting_admin",
			[]client.SupportTicket{tk("awaiting_admin")},
			"1 open ticket"},
		{"three awaiting_admin",
			[]client.SupportTicket{tk("awaiting_admin"), tk("awaiting_admin"), tk("resolved")},
			"3 open tickets"},
		{"one awaiting_customer",
			[]client.SupportTicket{tk("awaiting_customer")},
			"1 open ticket (awaiting your reply)"},
		{"mix — 2 open, 1 awaiting_customer",
			[]client.SupportTicket{tk("awaiting_customer"), tk("awaiting_admin")},
			"2 open tickets (1 awaiting your reply)"},
		{"closed doesn't count as open",
			[]client.SupportTicket{tk("awaiting_customer"), tk("closed")},
			"1 open ticket (awaiting your reply)"},
	}
	for _, tc := range tests {
		if got := summarizeSupport(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
