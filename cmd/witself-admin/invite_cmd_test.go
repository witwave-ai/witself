package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestInviteAdminCLIRoundTrip(t *testing.T) {
	var calls []string
	var postBodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer fleet-token" {
			t.Fatalf("authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/invites":
			writeInviteAdminTestJSON(t, w, map[string]any{
				"schema_version": "witself.v0",
				"invites": []map[string]any{
					inviteAdminTestRecord("launch-code", true),
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/invites/launch-code":
			writeInviteAdminTestJSON(t, w, map[string]any{
				"schema_version": "witself.v0",
				"invite":         inviteAdminTestRecord("launch-code", true),
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/invites":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			postBodies = append(postBodies, body)
			code, _ := body["code"].(string)
			if code == "" {
				code = "made-by-server"
			}
			enabled := true
			if value, ok := body["enabled"].(bool); ok {
				enabled = value
			}
			writeInviteAdminTestJSON(t, w, map[string]any{
				"schema_version": "witself.v0",
				"invite":         inviteAdminTestRecord(code, enabled),
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/invites/launch-code":
			writeInviteAdminTestJSON(t, w, map[string]any{
				"schema_version": "witself.v0",
				"deleted":        true,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	common := []string{"--endpoint", srv.URL, "--fleet-token", "fleet-token", "--json"}
	commands := [][]string{
		append([]string{"invite", "list"}, common...),
		append([]string{"invite", "show", "launch-code"}, common...),
		append([]string{
			"invite", "create", "--code", "launch-code", "--max-uses", "7",
			"--not-before", "2026-08-28T12:00:00Z",
			"--expires", "2026-09-01T12:00:00Z",
			"--cell", "civo-nyc", "--region", "nyc1", "--note", "launch",
		}, common...),
		append([]string{"invite", "disable", "launch-code"}, common...),
		append([]string{"invite", "enable", "launch-code"}, common...),
		append([]string{"invite", "delete", "launch-code"}, common...),
	}
	for _, args := range commands {
		stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
		if code != 0 {
			t.Fatalf("run(%q) exit code = %d; stderr=%s", args[:2], code, stderr)
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatalf("run(%q) JSON output: %v\n%s", args[:2], err, stdout)
		}
		if envelope["schema_version"] != "witself.v0" {
			t.Fatalf("run(%q) envelope = %#v", args[:2], envelope)
		}
	}

	wantCalls := []string{
		"GET /v1/invites",
		"GET /v1/invites/launch-code",
		"POST /v1/invites",
		"POST /v1/invites",
		"POST /v1/invites",
		"DELETE /v1/invites/launch-code",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %#v; want %#v", calls, wantCalls)
	}
	wantBodies := []map[string]any{
		{
			"code": "launch-code", "max_uses": float64(7),
			"not_before": "2026-08-28T12:00:00Z",
			"expires_at": "2026-09-01T12:00:00Z",
			"cell":       "civo-nyc", "region": "nyc1", "note": "launch",
		},
		{"code": "launch-code", "enabled": false},
		{"code": "launch-code", "enabled": true},
	}
	if !reflect.DeepEqual(postBodies, wantBodies) {
		t.Fatalf("POST bodies = %#v; want %#v", postBodies, wantBodies)
	}
}

func TestInviteAdminCreateAllowsGeneratedCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, present := body["code"]; present {
			t.Fatalf("generated-code request included code: %#v", body)
		}
		writeInviteAdminTestJSON(t, w, map[string]any{
			"schema_version": "witself.v0",
			"invite":         inviteAdminTestRecord("made-by-server", true),
		})
	}))
	defer srv.Close()

	stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int {
		return run([]string{
			"invite", "create", "--note", "generated",
			"--endpoint", srv.URL, "--fleet-token", "fleet-token", "--json",
		})
	})
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, `"code": "made-by-server"`) {
		t.Fatalf("JSON output = %s", stdout)
	}
}

func TestInviteAdminTableOutput(t *testing.T) {
	record := inviteAdminTestRecord("launch-code", false)
	record["uses"] = 5
	record["max_uses"] = 5
	record["valid"] = false
	record["reason"] = "fully used"
	record["exhausted"] = true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := map[string]any{"schema_version": "witself.v0"}
		if r.URL.Path == "/v1/invites" {
			response["invites"] = []map[string]any{record}
		} else {
			response["invite"] = record
		}
		writeInviteAdminTestJSON(t, w, response)
	}))
	defer srv.Close()

	common := []string{"--endpoint", srv.URL, "--fleet-token", "fleet-token"}
	stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int {
		return run(append([]string{"invite", "list"}, common...))
	})
	if code != 0 {
		t.Fatalf("list exit code = %d; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "code\tenabled\tuses/max\twindow\tcell/region\tnote") {
		t.Fatalf("list header = %q", stderr)
	}
	if !strings.Contains(stdout,
		"launch-code\tfalse\t5/5\t2026-08-28T12:00:00Z..2026-09-01T12:00:00Z\tcivo-nyc/nyc1\tlaunch") {
		t.Fatalf("list row = %q", stdout)
	}

	stdout, stderr, code = captureEmailAliasAdminCLI(t, func() int {
		return run(append([]string{"invite", "show", "launch-code"}, common...))
	})
	if code != 0 {
		t.Fatalf("show exit code = %d; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "valid\texhausted\texpired\tnot-yet-valid\treason\tcreated-at\tnote") {
		t.Fatalf("show header = %q", stderr)
	}
	if !strings.Contains(stdout,
		"\tfalse\ttrue\tfalse\tfalse\tfully used\t2026-08-28T10:00:00Z\tlaunch") {
		t.Fatalf("show verdict row = %q", stdout)
	}
}

func TestInviteAdminDeleteMissingIsSuccessful(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeInviteAdminTestJSON(t, w, map[string]any{
			"schema_version": "witself.v0",
			"deleted":        false,
		})
	}))
	defer srv.Close()

	stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int {
		return run([]string{
			"invite", "delete", "missing-code", "--endpoint", srv.URL,
			"--fleet-token", "fleet-token",
		})
	})
	if code != 0 || stdout != "deleted\tfalse\n" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestInviteAdminRejectsInvalidArguments(t *testing.T) {
	tests := [][]string{
		{"invite"},
		{"invite", "unknown"},
		{"invite", "show"},
		{"invite", "show", "UPPER"},
		{"invite", "create", "--code", "UPPER"},
		{"invite", "create", "--max-uses", "0"},
		{"invite", "delete", "valid-code", "extra"},
	}
	for _, args := range tests {
		_, _, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
		if code != 2 {
			t.Errorf("run(%q) exit code = %d", args, code)
		}
	}
}

func TestInviteAdminLocalErrorsDoNotEchoCodes(t *testing.T) {
	const codeValue = "private-launch-code"
	_, stderr, exitCode := captureEmailAliasAdminCLI(t, func() int {
		return run([]string{"invite", codeValue})
	})
	if exitCode != 2 {
		t.Fatalf("exit code = %d; stderr=%q", exitCode, stderr)
	}
	if strings.Contains(stderr, codeValue) {
		t.Fatalf("local error exposed invite code: %q", stderr)
	}
}

func TestInviteAdminCodeParserAcceptsCodeFirstAndFlagsFirst(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "code first",
			args: []string{
				"launch-code", "--endpoint", "https://cp.example",
				"--fleet-token", "fleet-token", "--json",
			},
		},
		{
			name: "flags first",
			args: []string{
				"--endpoint", "https://cp.example", "--fleet-token", "fleet-token",
				"--json", "launch-code",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, common, ok := parseInviteAdminCodeArgs("show", tc.args)
			if !ok || code != "launch-code" {
				t.Fatalf("code = %q, ok = %t", code, ok)
			}
			if *common.endpoint != "https://cp.example" ||
				*common.fleetToken != "fleet-token" || !*common.json {
				t.Fatalf("parsed common flags incorrectly")
			}
		})
	}
}

func TestInviteAdminTransportErrorsRedactCode(t *testing.T) {
	const codeValue = "private-launch-code"
	_, stderr, exitCode := captureEmailAliasAdminCLI(t, func() int {
		return run([]string{
			"invite", "show", codeValue, "--endpoint", "://broken-endpoint",
			"--fleet-token", "fleet-token",
		})
	})
	if exitCode != 1 {
		t.Fatalf("exit code = %d; stderr=%q", exitCode, stderr)
	}
	if strings.Contains(stderr, codeValue) || !strings.Contains(stderr, "[invite]") {
		t.Fatalf("transport error did not redact invite code: %q", stderr)
	}
}

func TestInviteAdminErrorsRedactCode(t *testing.T) {
	const codeValue = "private-launch-code"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		writeInviteAdminTestJSON(t, w, map[string]any{
			"error": "could not read invite " + codeValue,
		})
	}))
	defer srv.Close()

	_, stderr, code := captureEmailAliasAdminCLI(t, func() int {
		return run([]string{
			"invite", "show", codeValue, "--endpoint", srv.URL,
			"--fleet-token", "fleet-token",
		})
	})
	if code != 1 {
		t.Fatalf("exit code = %d; stderr=%q", code, stderr)
	}
	if strings.Contains(stderr, codeValue) || !strings.Contains(stderr, "[invite]") {
		t.Fatalf("error did not redact invite code: %q", stderr)
	}
}

func inviteAdminTestRecord(code string, enabled bool) map[string]any {
	return map[string]any{
		"code": code, "enabled": enabled,
		"not_before": "2026-08-28T12:00:00Z",
		"expires_at": "2026-09-01T12:00:00Z",
		"max_uses":   7, "cell": "civo-nyc", "region": "nyc1",
		"uses": 2, "note": "launch", "created_at": "2026-08-28T10:00:00Z",
		"valid": enabled, "reason": "", "exhausted": false,
		"expired": false, "not_yet_valid": false,
	}
}

func writeInviteAdminTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
