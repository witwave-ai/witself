package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/avatar"
)

func TestAvatarCLIProposalReadsSeparateFilesAndSendsHeaderOnlyKey(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "agent.token")
	svgFile := filepath.Join(dir, "avatar.svg")
	specFile := filepath.Join(dir, "visual-spec.json")
	if err := os.WriteFile(tokenFile, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svgFile, []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specFile, []byte(`{"expression":"curious","experience":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/self/avatar/proposals" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "avatar-proposal-1" {
			t.Fatalf("Idempotency-Key = %q", got)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, leaked := body["idempotency_key"]; leaked {
			t.Fatalf("body leaked idempotency key: %s", mustAvatarCLIJSON(t, body))
		}
		for _, forbidden := range []string{"account_id", "realm_id", "agent_id"} {
			if _, leaked := body[forbidden]; leaked {
				t.Fatalf("self proposal leaked %s: %s", forbidden, mustAvatarCLIJSON(t, body))
			}
		}
		if string(body["expected_profile_revision"]) != "4" || string(body["parent_version"]) != "3" ||
			string(body["style_pack_id"]) != `"witself-flat-portrait"` || string(body["style_pack_version"]) != "1" ||
			string(body["subject_form"]) != `"animal"` || string(body["description"]) != `"A curious fox"` ||
			string(body["visual_spec"]) != `{"experience":1,"expression":"curious"}` {
			t.Fatalf("proposal body = %s", mustAvatarCLIJSON(t, body))
		}
		var svg string
		if err := json.Unmarshal(body["svg"], &svg); err != nil || svg != `<svg xmlns="http://www.w3.org/2000/svg"></svg>` {
			t.Fatalf("svg = %q / %v", svg, err)
		}
		writeAvatarCLIJSON(t, w, map[string]any{
			"avatar":  map[string]any{"profile": map[string]any{"agent_id": "agent_1", "profile_revision": 5}},
			"receipt": map[string]any{"operation": "propose", "result_revision": 5, "result_version": 4},
		})
	}))
	defer srv.Close()

	code := run([]string{
		"avatar", "propose", "--endpoint", srv.URL, "--token-file", tokenFile,
		"--expected-profile-revision", "4", "--parent-version", "3",
		"--style-pack-id", avatar.DefaultStylePackID, "--style-pack-version", "1",
		"--subject-form", "animal", "--description", "A curious fox",
		"--spec-file", specFile, "--svg-file", svgFile, "--runtime", "codex",
		"--idempotency-key", "avatar-proposal-1",
	})
	if code != 0 || calls != 1 {
		t.Fatalf("run code = %d, calls = %d", code, calls)
	}
}

func TestAvatarCLIRejectsShellValueAndAmbiguousStdinBeforeHTTP(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{
		"avatar", "propose", "--endpoint", srv.URL, "--token-file", tokenFile,
		"--expected-profile-revision", "1", "--style-pack-id", avatar.DefaultStylePackID,
		"--style-pack-version", "1", "--subject-form", "human", "--description", "Portrait",
		"--idempotency-key", "proposal-1",
	}
	if code := run(append(append([]string{}, base...), "--spec-file", "spec.json", "--svg", "<svg></svg>")); code != 2 {
		t.Fatalf("shell SVG flag code = %d, want 2", code)
	}
	if code := run(append(append([]string{}, base...), "--spec-stdin", "--svg-stdin")); code != 2 {
		t.Fatalf("dual stdin code = %d, want 2", code)
	}
	if code := run([]string{"avatar", "show", "--endpoint", srv.URL, "--token-file", tokenFile, "--agent-id", "agent_other"}); code != 2 {
		t.Fatalf("self target selector code = %d, want 2", code)
	}
	if code := run([]string{"avatar", "version", "--version", "0", "--endpoint", srv.URL, "--token-file", tokenFile}); code != 2 {
		t.Fatalf("non-positive version code = %d, want 2", code)
	}
	if code := run([]string{"avatar", "history", "--limit", "101", "--endpoint", srv.URL, "--token-file", tokenFile}); code != 2 {
		t.Fatalf("over-limit history code = %d, want 2", code)
	}
	if code := run([]string{"avatar", "reset", "--expected-profile-revision", "1", "--reason-code", "free form", "--idempotency-key", "reset-1", "--endpoint", srv.URL, "--token-file", tokenFile}); code != 2 {
		t.Fatalf("invalid reset reason code = %d, want 2", code)
	}
	if calls != 0 {
		t.Fatalf("invalid CLI inputs made %d HTTP calls", calls)
	}
}

func TestReadAvatarPayloadSupportsFileOrStdin(t *testing.T) {
	file := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(file, []byte(`{"file":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readAvatarPayload("test", file, false, 100, strings.NewReader("ignored"))
	if err != nil || string(got) != `{"file":true}` {
		t.Fatalf("file payload = %q / %v", got, err)
	}
	got, err = readAvatarPayload("test", "", true, 100, strings.NewReader(`{"stdin":true}`))
	if err != nil || string(got) != `{"stdin":true}` {
		t.Fatalf("stdin payload = %q / %v", got, err)
	}
	got, err = readAvatarPayload("test", "-", false, 100, strings.NewReader(`{"dash":true}`))
	if err != nil || string(got) != `{"dash":true}` {
		t.Fatalf("dash payload = %q / %v", got, err)
	}
	if _, err := readAvatarPayload("test", file, true, 100, strings.NewReader("x")); err == nil {
		t.Fatal("file plus stdin was accepted")
	}
	if _, err := readAvatarPayload("test", "-", true, 100, strings.NewReader("x")); err == nil {
		t.Fatal("duplicate stdin selectors were accepted")
	}
	if _, err := readAvatarPayload("test", "", false, 100, strings.NewReader("x")); err == nil {
		t.Fatal("missing source was accepted")
	}
}

func TestAvatarCLIOperatorSurfacesUseExplicitTargetSelectors(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "operator.token")
	svgFile := filepath.Join(dir, "avatar.svg")
	specFile := filepath.Join(dir, "spec.json")
	styleFile := filepath.Join(dir, "style.json")
	if err := os.WriteFile(tokenFile, []byte("operator-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svgFile, []byte("<svg></svg>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specFile, []byte(`{"expression":"calm"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	styleRaw, err := json.Marshal(avatar.BuiltInFlatVectorStylePack())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(styleFile, styleRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	wantKeys := map[string]string{
		"POST /v1/agents/agent_9/avatar/proposals":      "op-propose",
		"POST /v1/agents/agent_9/avatar:activate":       "op-activate",
		"POST /v1/agents/agent_9/avatar:reject":         "op-reject",
		"POST /v1/agents/agent_9/avatar:rollback":       "op-rollback",
		"POST /v1/agents/agent_9/avatar:reset":          "op-reset",
		"PATCH /v1/agents/agent_9/avatar-policy":        "op-policy",
		"POST /v1/realms/realm_9/avatar-style/versions": "op-style",
	}
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + r.URL.Path
		calls[route]++
		if got := r.Header.Get("Authorization"); got != "Bearer operator-token" {
			t.Fatalf("%s Authorization = %q", route, got)
		}
		if want := wantKeys[route]; want != "" && r.Header.Get("Idempotency-Key") != want {
			t.Fatalf("%s Idempotency-Key = %q, want %q", route, r.Header.Get("Idempotency-Key"), want)
		}
		switch route {
		case "GET /v1/agents/agent_9/avatar":
			writeAvatarCLIJSON(t, w, map[string]any{"avatar": map[string]any{"profile": map[string]any{"agent_id": "agent_9"}}})
		case "GET /v1/agents/agent_9/avatar/history":
			writeAvatarCLIJSON(t, w, map[string]any{"schema_version": "witself.v0", "versions": []any{}})
		case "GET /v1/agents/agent_9/avatar/versions/2":
			writeAvatarCLIJSON(t, w, map[string]any{
				"schema_version": "witself.v0",
				"version": map[string]any{
					"version": 2, "description": "Exact portrait", "visual_spec": map[string]any{"expression": "calm"},
					"svg": `<svg xmlns="http://www.w3.org/2000/svg"></svg>`,
				},
			})
		case "GET /v1/realms/realm_9/avatar-style":
			writeAvatarCLIJSON(t, w, map[string]any{"style": map[string]any{"realm_id": "realm_9"}})
		case "POST /v1/realms/realm_9/avatar-style/versions":
			writeAvatarCLIJSON(t, w, map[string]any{"style": map[string]any{"realm_id": "realm_9"}, "receipt": map[string]any{}})
		default:
			if wantKeys[route] == "" {
				t.Fatalf("unexpected route %s", route)
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if _, leaked := body["agent_id"]; leaked {
				t.Fatalf("%s leaked target into body", route)
			}
			writeAvatarCLIJSON(t, w, map[string]any{"avatar": map[string]any{"profile": map[string]any{"agent_id": "agent_9"}}, "receipt": map[string]any{}})
		}
	}))
	defer srv.Close()
	connection := []string{"--endpoint", srv.URL, "--token-file", tokenFile}
	runOK := func(args ...string) {
		t.Helper()
		if code := run(append(args, connection...)); code != 0 {
			t.Fatalf("run(%q) code = %d", args, code)
		}
	}
	runOK("avatar", "operator", "show", "--agent-id", "agent_9")
	runOK("avatar", "operator", "history", "--agent-id", "agent_9")
	runOK("avatar", "operator", "version", "--agent-id", "agent_9", "--version", "2")
	runOK("avatar", "operator", "propose", "--agent-id", "agent_9",
		"--expected-profile-revision", "1", "--style-pack-id", avatar.DefaultStylePackID,
		"--style-pack-version", "1", "--subject-form", "human", "--description", "Calm portrait",
		"--spec-file", specFile, "--svg-file", svgFile, "--idempotency-key", "op-propose")
	runOK("avatar", "operator", "activate", "--agent-id", "agent_9", "--version", "2", "--expected-profile-revision", "2", "--idempotency-key", "op-activate")
	runOK("avatar", "operator", "reject", "--agent-id", "agent_9", "--version", "2", "--expected-profile-revision", "3", "--reason-code", "off_style", "--idempotency-key", "op-reject")
	runOK("avatar", "operator", "rollback", "--agent-id", "agent_9", "--version", "1", "--expected-profile-revision", "4", "--idempotency-key", "op-rollback")
	runOK("avatar", "operator", "reset", "--agent-id", "agent_9", "--expected-profile-revision", "5", "--reason-code", "new_direction", "--idempotency-key", "op-reset")
	runOK("avatar", "operator", "policy", "--agent-id", "agent_9", "--policy", "agent_proposes", "--expected-profile-revision", "5", "--idempotency-key", "op-policy")
	runOK("avatar", "operator", "style", "show", "--realm-id", "realm_9")
	runOK("avatar", "operator", "style", "version", "--realm-id", "realm_9", "--expected-style-revision", "1", "--style-file", styleFile, "--idempotency-key", "op-style")

	for route := range wantKeys {
		if calls[route] != 1 {
			t.Errorf("%s calls = %d, want 1", route, calls[route])
		}
	}
	for _, route := range []string{
		"GET /v1/agents/agent_9/avatar", "GET /v1/agents/agent_9/avatar/history",
		"GET /v1/agents/agent_9/avatar/versions/2",
		"GET /v1/realms/realm_9/avatar-style",
	} {
		if calls[route] != 1 {
			t.Errorf("%s calls = %d, want 1", route, calls[route])
		}
	}
}

func TestAvatarCLIResetUsesSelfRouteAndHeaderOnlyKey(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "agent.token")
	if err := os.WriteFile(tokenFile, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/self/avatar:reset" ||
			r.Header.Get("Authorization") != "Bearer agent-token" || r.Header.Get("Idempotency-Key") != "reset-1" {
			t.Fatalf("request = %s %s auth=%q key=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"))
		}
		body := map[string]json.RawMessage{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["expected_profile_revision"]) != "7" || string(body["reason_code"]) != `"user_requested"` {
			t.Fatalf("reset body = %s", mustAvatarCLIJSON(t, body))
		}
		for _, forbidden := range []string{"idempotency_key", "agent_id", "account_id", "realm_id"} {
			if _, ok := body[forbidden]; ok {
				t.Fatalf("reset body leaked %s: %s", forbidden, mustAvatarCLIJSON(t, body))
			}
		}
		writeAvatarCLIJSON(t, w, map[string]any{
			"avatar":  map[string]any{"profile": map[string]any{"agent_id": "agent_1", "profile_revision": 8, "status": "generation_due"}},
			"receipt": map[string]any{"operation": "reset", "result_revision": 8},
		})
	}))
	defer srv.Close()

	if code := run([]string{
		"avatar", "reset", "--endpoint", srv.URL, "--token-file", tokenFile,
		"--expected-profile-revision", "7", "--reason-code", "user_requested", "--idempotency-key", "reset-1",
	}); code != 0 || calls != 1 {
		t.Fatalf("run code = %d, calls = %d", code, calls)
	}
}

func TestAvatarCLIHistoryDisplaysLifecycleProjection(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "operator.token")
	if err := os.WriteFile(tokenFile, []byte("operator-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/agents/agent_9/avatar/history" ||
			r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("before_version") != "4" ||
			r.Header.Get("Authorization") != "Bearer operator-token" {
			t.Fatalf("request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		writeAvatarCLIJSON(t, w, map[string]any{
			"schema_version": "witself.v0",
			"versions": []any{map[string]any{
				"version": 2, "is_active": false, "is_proposed": false,
				"was_activated": true, "rollback_eligible": true, "rejected": false,
				"last_activated_at": "2026-07-17T20:02:00Z",
				"description":       "must not survive summary decoding", "svg": "<svg></svg>",
				"visual_spec": map[string]any{"must_not": "survive"},
			}},
		})
	}))
	defer srv.Close()

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{
			"avatar", "operator", "history", "--agent-id", "agent_9",
			"--limit", "2", "--before-version", "4",
			"--endpoint", srv.URL, "--token-file", tokenFile,
		})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("history code=%d stderr=%q", code, stderr)
	}
	var page struct {
		Versions []struct {
			WasActivated     bool   `json:"was_activated"`
			RollbackEligible bool   `json:"rollback_eligible"`
			LastActivatedAt  string `json:"last_activated_at"`
		} `json:"versions"`
	}
	if err := json.Unmarshal([]byte(stdout), &page); err != nil {
		t.Fatalf("decode CLI history: %v; output=%q", err, stdout)
	}
	if len(page.Versions) != 1 || !page.Versions[0].WasActivated || !page.Versions[0].RollbackEligible || page.Versions[0].LastActivatedAt == "" {
		t.Fatalf("CLI history projection = %+v", page.Versions)
	}
	for _, forbidden := range []string{"must not survive", "<svg", "visual_spec"} {
		if strings.Contains(stdout, forbidden) {
			t.Fatalf("CLI history leaked creative payload %q: %s", forbidden, stdout)
		}
	}
}

func TestAvatarCLIVersionDisplaysExactCreativePayload(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/self/avatar/versions/2" ||
			r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Fatalf("request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		writeAvatarCLIJSON(t, w, map[string]any{
			"schema_version": "witself.v0",
			"version": map[string]any{
				"version": 2, "description": "Exact portrait",
				"visual_spec": map[string]any{"expression": "calm"},
				"svg":         `<svg xmlns="http://www.w3.org/2000/svg"></svg>`,
			},
		})
	}))
	defer srv.Close()
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{
			"avatar", "version", "--version", "2", "--endpoint", srv.URL, "--token-file", tokenFile,
		})
	})
	if code != 0 || stderr != "" || !strings.Contains(stdout, "Exact portrait") ||
		!strings.Contains(stdout, "visual_spec") || !strings.Contains(stdout, "svg") {
		t.Fatalf("exact version code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestAvatarGenerationFailureReasonIsBoundedBeforeHTTP(t *testing.T) {
	for _, invalid := range []string{"", "UpperCase", strings.Repeat("a", maxAvatarReasonCodeBytes+1), "contains space"} {
		if validAvatarReasonCode(invalid, false) {
			t.Errorf("invalid reason %q was accepted", invalid)
		}
	}
	for _, valid := range []string{
		"model_unavailable", "validation-failed", "model.render_unavailable", "timeout1",
		"a." + strings.Repeat("b", maxAvatarReasonCodeBytes-2),
	} {
		if !validAvatarReasonCode(valid, false) {
			t.Errorf("valid reason %q was rejected", valid)
		}
	}
	if !validAvatarReasonCode("", true) {
		t.Fatal("optional empty reason was rejected")
	}
}

func writeAvatarCLIJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func mustAvatarCLIJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
