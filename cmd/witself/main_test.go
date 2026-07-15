package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/placement"
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

func TestParseUsageStart(t *testing.T) {
	now := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	got, err := parseUsageStart("30d", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(-30 * 24 * time.Hour); !got.Equal(want) {
		t.Fatalf("30d = %s, want %s", got, want)
	}
	got, err = parseUsageStart("2026-07-01T00:00:00Z", now)
	if err != nil || !got.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("RFC3339 = %s / %v", got, err)
	}
	if _, err := parseUsageStart("0d", now); err == nil {
		t.Fatal("0d was accepted")
	}
}

func TestFactMetadataFlags(t *testing.T) {
	confidence, err := factConfidenceFlag(0)
	if err != nil || confidence == nil || *confidence != 0 {
		t.Fatalf("zero confidence = %#v / %v", confidence, err)
	}
	if _, err := factConfidenceFlag(1.1); err == nil {
		t.Fatal("confidence above one was accepted")
	}
	observed, err := factTimeFlag("2026-07-12T12:30:00-06:00")
	if err != nil || observed == nil || observed.Format(time.RFC3339) != "2026-07-12T18:30:00Z" {
		t.Fatalf("observed time = %#v / %v", observed, err)
	}
}

func TestCSVListFlagAcceptsRepeatedAndCommaSeparatedValues(t *testing.T) {
	var values csvListFlag
	if err := values.Set("transcript_created,transcript_entry_write"); err != nil {
		t.Fatal(err)
	}
	if err := values.Set("transcript_entry_read"); err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 || values[2] != "transcript_entry_read" {
		t.Fatalf("values = %#v", values)
	}
}

func TestUsageCommandUsesAgentTokenAndFilters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" || r.Header.Get("Authorization") != "Bearer witself_agt_usage" {
			t.Fatalf("request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if got := r.URL.Query()["dimension"]; len(got) != 2 || got[0] != "transcript_created" || got[1] != "transcript_entry_write" {
			t.Fatalf("dimensions = %#v", got)
		}
		if r.URL.Query().Get("group_by") != "day" || r.URL.Query().Get("since") != "2026-07-01T00:00:00Z" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"usage":{"account_id":"acc_1","realm_id":"rlm_1","agent_id":"agt_1","since":"2026-07-01T00:00:00Z","until":"2026-07-12T00:00:00Z","bucket":"day","points":[],"totals":[]}}`))
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_usage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := run([]string{
		"usage", "--endpoint", srv.URL, "--token-file", tokenFile,
		"--since", "2026-07-01T00:00:00Z", "--until", "2026-07-12T00:00:00Z",
		"--dimension", "transcript_created", "--dimension", "transcript_entry_write",
	})
	if code != 0 {
		t.Fatalf("run code = %d, want 0", code)
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

func TestTokenCreateCuratorWritesRestrictedExpiringToken(t *testing.T) {
	expiresAt := time.Date(2026, 7, 15, 8, 30, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/agents/agent_1/curator-tokens" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer witself_opr_parent" {
			t.Errorf("Authorization = %q", got)
		}
		var body struct {
			AccessProfile string `json:"access_profile"`
			DisplayName   string `json:"display_name"`
			TTL           string `json:"ttl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.AccessProfile != "curator-preview" || body.DisplayName != "nightly curator" || body.TTL != "30m" {
			t.Errorf("request body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "agent_token": "witself_agt_curator",
			"token_id": "tok_curator", "agent_id": "agent_1", "agent_name": "memory-agent",
			"access_profile": "curator-preview", "display_name": "nightly curator", "expires_at": expiresAt,
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.token")
	out := filepath.Join(dir, "curator.token")
	if err := os.WriteFile(parent, []byte("witself_opr_parent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := run([]string{
		"token", "create", "--endpoint", srv.URL, "--token-file", parent,
		"--agent", "agent_1", "--profile", "curator-preview",
		"--name", "nightly curator", "--ttl", "30m", "--out", out,
	})
	if code != 0 {
		t.Fatalf("run code = %d, want 0", code)
	}
	contents, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "witself_agt_curator\n" {
		t.Fatalf("token file = %q", contents)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %o", info.Mode().Perm())
	}
}

func TestTokenCreateAgentWritesManagedCanonicalPathFromSelf(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	if err := local.Save("default", local.Account{ID: "acc_1"}, "witself_opr_owner"); err != nil {
		t.Fatal(err)
	}

	const mintedToken = "witself_agt_new"
	selfCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/agent_1/tokens":
			if got := r.Header.Get("Authorization"); got != "Bearer witself_opr_owner" {
				t.Errorf("mint Authorization = %q", got)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","agent_token":"` + mintedToken + `","token_id":"tok_1","agent_id":"agent_1","agent_name":"response-name"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/self":
			selfCalls++
			if got := r.Header.Get("Authorization"); got != "Bearer "+mintedToken {
				t.Errorf("self Authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","realm_id":"realm_1","realm_name":"engineering","agent_id":"agent_1","agent_name":"self-name"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"token", "create", "--account", "default", "--endpoint", srv.URL, "--agent", "agent_1"})
	})
	if code != 0 {
		t.Fatalf("run code = %d, want 0; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout exposed token: %q", stdout)
	}
	if selfCalls != 1 {
		t.Fatalf("self calls = %d, want 1", selfCalls)
	}

	canonical, err := local.AgentTokenPath("default", "engineering", "self-name")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != mintedToken+"\n" {
		t.Fatalf("canonical token contents = %q", contents)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("canonical token mode = %o", info.Mode().Perm())
	}
	legacy := filepath.Join(home, "tokens", "accounts", "default", "agents", "response-name.token")
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy token path exists or returned unexpected error: %v", err)
	}
}

func TestTokenCreateAgentPrintsOneTimeTokenWhenManagedPathCannotBeResolved(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	if err := local.Save("default", local.Account{ID: "acc_1"}, "witself_opr_owner"); err != nil {
		t.Fatal(err)
	}

	const mintedToken = "witself_agt_only_copy"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/agent_3/tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"agent_token":"` + mintedToken + `","token_id":"tok_3","agent_id":"agent_3","agent_name":"unplaced-bot"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/self":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"identity lookup unavailable"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"token", "create", "--account", "default", "--endpoint", srv.URL, "--agent", "agent_3"})
	})
	if code != 1 {
		t.Fatalf("run code = %d, want 1", code)
	}
	if stdout != mintedToken+"\n" {
		t.Fatalf("one-time token output = %q", stdout)
	}
	if !strings.Contains(stderr, "printing the token once instead") {
		t.Fatalf("stderr = %q, want no-stranding explanation", stderr)
	}
	if strings.Contains(stderr, mintedToken) {
		t.Fatalf("stderr exposed token: %q", stderr)
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
	if code := run([]string{"token", "create", "--endpoint", "http://example.test", "--token-file", "token", "--agent", "agent_1", "--profile", "curator-preview", "--name", "curator"}); code != 2 {
		t.Fatalf("curator without ttl code = %d, want 2", code)
	}
	if code := run([]string{"token", "create", "--endpoint", "http://example.test", "--token-file", "token", "--agent", "agent_1", "--profile", "admin", "--name", "curator", "--ttl", "30m"}); code != 2 {
		t.Fatalf("unknown profile code = %d, want 2", code)
	}
	if code := run([]string{"token", "create", "--endpoint", "http://example.test", "--token-file", "token", "--operator", "--profile", "curator-apply"}); code != 2 {
		t.Fatalf("operator curator profile code = %d, want 2", code)
	}
}

func TestSelfShowUsesAgentToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/self" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer witself_agt_scott" {
			t.Errorf("Authorization = %q", got)
		}
		q := r.URL.Query()
		if q.Get("include_facts") != "true" || q.Get("include_salient") != "true" || q.Get("salient_limit") != "10" || q.Get("max_bytes") != "8192" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","agent_id":"agent_1","agent_name":"scott","realm_id":"realm_1","realm_name":"default"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{"facts":0,"memories":0}},"elided":false}`))
	}))
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_scott\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := run([]string{
		"self", "show",
		"--endpoint", srv.URL,
		"--token-file", tokenFile,
		"--realm", "default",
		"--agent", "scott",
		"--json",
	})
	if code != 0 {
		t.Fatalf("run code = %d, want 0", code)
	}
}

func TestFactSetAndListUseAgentToken(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer witself_agt_scott" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/facts":
			if got := r.Header.Get("Idempotency-Key"); got != "set-birthday-1" {
				t.Errorf("Idempotency-Key = %q", got)
			}
			var body client.SetFactInput
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Subject != "self" || body.Predicate != "identity/birth-date" || string(body.Value) != `"1980-02-29"` || body.ValueType != "date" || body.Recurrence != "annual" {
				t.Errorf("body = %#v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"fact":{"id":"fact_1","subject":"self","predicate":"identity/birth-date","value_type":"date","value":"1980-02-29","recurrence":"annual"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/facts":
			if r.URL.Query().Get("predicate_prefix") != "preferences" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"facts":[{"id":"fact_1","subject":"self","predicate":"preferences/editor","value_type":"string","value":"vim","updated_at":"2026-07-12T00:00:00Z"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_scott\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"fact", "set", "--endpoint", srv.URL, "--token-file", tokenFile, "--type", "date", "--recurrence", "annual", "--idempotency-key", "set-birthday-1", "identity/birth-date", "1980-02-29"}); code != 0 {
		t.Fatalf("fact set code = %d", code)
	}
	if code := run([]string{"fact", "list", "--endpoint", srv.URL, "--token-file", tokenFile, "--category", "preferences"}); code != 0 {
		t.Fatalf("fact list code = %d", code)
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestFactProposalAndDecisionCLIForwardIdempotencyKeys(t *testing.T) {
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/fact-candidates":
			seen["propose"] = r.Header.Get("Idempotency-Key")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"candidate":{"id":"fcand_1","subject":"self","predicate":"preferences/editor","value":"zed","status":"pending"}}`))
		case "/v1/fact-candidates/fcand_1:confirm":
			seen["confirm"] = r.Header.Get("Idempotency-Key")
			_, _ = w.Write([]byte(`{"fact":{"id":"fact_1","subject":"self","predicate":"preferences/editor","value":"zed"}}`))
		case "/v1/fact-candidates/fcand_2:reject":
			seen["reject"] = r.Header.Get("Idempotency-Key")
			_, _ = w.Write([]byte(`{"candidate":{"id":"fcand_2","subject":"self","predicate":"preferences/theme","value":"dark","status":"rejected"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_scott\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connection := []string{"--endpoint", srv.URL, "--token-file", tokenFile}
	if code := run(append([]string{"fact", "propose"}, append(connection, "--idempotency-key", "proposal-1", "preferences/editor", "zed")...)); code != 0 {
		t.Fatalf("fact propose code = %d", code)
	}
	if code := run(append([]string{"fact", "confirm"}, append(connection, "--idempotency-key", "confirm-1", "fcand_1")...)); code != 0 {
		t.Fatalf("fact confirm code = %d", code)
	}
	if code := run(append([]string{"fact", "reject"}, append(connection, "--idempotency-key", "reject-1", "fcand_2")...)); code != 0 {
		t.Fatalf("fact reject code = %d", code)
	}
	if seen["propose"] != "proposal-1" || seen["confirm"] != "confirm-1" || seen["reject"] != "reject-1" {
		t.Fatalf("idempotency headers = %#v", seen)
	}
}

func TestFactCandidateReviewAndDetailUseAgentToken(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer witself_agt_scott" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/fact-candidates":
			if r.URL.Query().Get("status") != "open" || r.URL.Query().Get("limit") != "25" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"candidates":[{"id":"fcand_1","subject":"person_spouse","predicate":"identity/name","value_type":"string","value":null,"sensitive":true,"status":"pending","observed_at":"2026-07-12T00:00:00Z","proposed_at":"2026-07-12T00:00:00Z"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/fact-candidates/fcand_1":
			_, _ = w.Write([]byte(`{"candidate":{"id":"fcand_1","subject":"person_spouse","predicate":"identity/name","value_type":"string","value":"Arina Pavlova Habich","sensitive":true,"status":"pending","observed_at":"2026-07-12T00:00:00Z","proposed_at":"2026-07-12T00:00:00Z"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_scott\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"fact", "review", "--endpoint", srv.URL, "--token-file", tokenFile, "--limit", "25"}); code != 0 {
		t.Fatalf("fact review code = %d", code)
	}
	if code := run([]string{"fact", "candidate", "--endpoint", srv.URL, "--token-file", tokenFile, "fcand_1"}); code != 0 {
		t.Fatalf("fact candidate code = %d", code)
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestFactUpcomingCanIncludeSensitiveDates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/fact-occurrences" || r.URL.Query().Get("include_sensitive") != "true" {
			t.Fatalf("request = %s", r.URL.RequestURI())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer witself_agt_scott" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"occurrences":[]}`))
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_scott\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"fact", "upcoming", "--endpoint", srv.URL, "--token-file", tokenFile, "--days", "14", "--include-sensitive"}); code != 0 {
		t.Fatalf("fact upcoming code = %d", code)
	}
}

func TestFactSubjectCommandsUseAgentToken(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer witself_agt_scott" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/fact-subjects/person_spouse":
			var body client.UpsertFactSubjectInput
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.DisplayName != "Spouse" {
				t.Errorf("subject body = %#v", body)
			}
			_, _ = w.Write([]byte(`{"subject":{"id":"sub_1","canonical_key":"person_spouse","display_name":"Spouse","aliases":[]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/fact-subjects/person_spouse/aliases":
			var body client.AddFactSubjectAliasInput
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Alias != "my wife" {
				t.Errorf("alias body = %#v", body)
			}
			_, _ = w.Write([]byte(`{"subject":{"id":"sub_1","canonical_key":"person_spouse","display_name":"Spouse","aliases":["my wife"]}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/fact-subjects":
			_, _ = w.Write([]byte(`{"subjects":[{"id":"sub_1","canonical_key":"person_spouse","display_name":"Spouse","aliases":["my wife"]}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_scott\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"fact", "subject", "set", "--endpoint", srv.URL, "--token-file", tokenFile, "--display-name", "Spouse", "person_spouse"},
		{"fact", "subject", "alias", "--endpoint", srv.URL, "--token-file", tokenFile, "person_spouse", "my wife"},
		{"fact", "subject", "list", "--endpoint", srv.URL, "--token-file", tokenFile},
	} {
		if code := run(args); code != 0 {
			t.Fatalf("%v code = %d", args, code)
		}
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
}

func TestConnectAgentResolvesManagedAccountAndCanonicalToken(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("WITSELF_AGENT", "scott")
	if err := local.Save("default", local.Account{ID: "acc_1"}, "witself_opr_owner"); err != nil {
		t.Fatal(err)
	}
	path, err := local.AgentTokenPath("default", "default", "scott")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("witself_agt_scott\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	conn, err := connectAgentWithLocator(context.Background(), "", "", "", "", "", func(_ context.Context, controlPlane, accountID string) (string, string, error) {
		if controlPlane != defaultControlPlane || accountID != "acc_1" {
			t.Fatalf("locator = %q / %q", controlPlane, accountID)
		}
		return "gcp-sandbox-use1-dev", "https://cell.example.test", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if conn.Endpoint != "https://cell.example.test" || conn.Token != "witself_agt_scott" || conn.AccountID != "acc_1" || conn.AccountName != "default" || conn.RealmName != "default" || conn.AgentName != "scott" {
		t.Fatalf("connection = %+v", conn)
	}
}

func TestTranscriptAppendAcceptsIDBeforeFlags(t *testing.T) {
	var gotPath string
	var got struct {
		ExternalID     string `json:"external_id"`
		Role           string `json:"role"`
		Body           string `json:"body"`
		ReplyToEntryID string `json:"reply_to_entry_id"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer witself_agt_recorder" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"entry":{"id":"ent_2","transcript_id":"trn_1","sequence":2,"role":"assistant","artifacts":[],"created_at":"2026-07-10T00:00:00Z"}}`))
	}))
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_recorder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := run([]string{
		"transcript", "append", "trn_1",
		"--endpoint", srv.URL,
		"--token-file", tokenFile,
		"--external-id", "vendor-message-2",
		"--role", "assistant",
		"--body", "done",
		"--reply-to", "ent_1",
	})
	if code != 0 {
		t.Fatalf("run code = %d, want 0", code)
	}
	if gotPath != "/v1/transcripts/trn_1/entries" {
		t.Fatalf("path = %q", gotPath)
	}
	if got.ExternalID != "vendor-message-2" || got.Role != "assistant" || got.Body != "done" || got.ReplyToEntryID != "ent_1" {
		t.Fatalf("request = %+v", got)
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

func TestAccountPlacementSetMergesCurrentPolicy(t *testing.T) {
	policy := placement.DefaultPolicy()
	var patched placement.Policy
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer witself_opr_parent" {
			t.Errorf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/account/placement-policy":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version":   "witself.v0",
				"placement_policy": policy,
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/account/placement-policy":
			if err := json.NewDecoder(r.Body).Decode(&patched); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version":   "witself.v0",
				"placement_policy": patched,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.token")
	if err := os.WriteFile(parent, []byte("witself_opr_parent\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code := run([]string{
		"account", "placement", "set",
		"--endpoint", srv.URL,
		"--token-file", parent,
		"--prefer-clouds", "gcp,aws",
		"--prefer-regions", "use1",
		"--only-channels", "stable,edge",
		"--rebalance-on", "cloud,channel",
	})
	if code != 0 {
		t.Fatalf("run code = %d, want 0", code)
	}
	if got := patched.PreferredClouds; len(got) != 2 || got[0] != "gcp" || got[1] != "aws" {
		t.Fatalf("preferred_clouds = %#v", got)
	}
	if got := patched.PreferredRegions; len(got) != 1 || got[0] != "use1" {
		t.Fatalf("preferred_regions = %#v", got)
	}
	if got := patched.PreferredChannels; len(got) != 3 || got[0] != "stable" || got[2] != "experimental" {
		t.Fatalf("preferred_channels was not preserved: %#v", got)
	}
	if got := patched.AllowedChannels; len(got) != 2 || got[0] != "stable" || got[1] != "edge" {
		t.Fatalf("allowed_channels = %#v", got)
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
