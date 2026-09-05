package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageCommandShowsTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("allow_truncation"); got != "1" {
			t.Errorf("allow_truncation = %q, want 1", got)
		}
		_, _ = w.Write([]byte(`{"usage":{"points":[{"bucket_start":"2026-01-01T00:00:00Z","dimension":"message_sent","unit":"message","quantity":1,"event_count":1}],"totals":[{"dimension":"message_sent","unit":"message","quantity":1,"event_count":1}],"truncated":true}}`))
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_usage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"usage", "--endpoint", srv.URL, "--token-file", tokenFile, "--allow-truncation"}
	stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
	if code != 0 || !strings.Contains(stdout, "message_sent") || !strings.Contains(stderr, "WARNING: truncated: true") || !strings.Contains(stderr, "totals cover returned points only") {
		t.Fatalf("text usage = %d / %q / %q", code, stdout, stderr)
	}
	stdout, stderr, code = captureFactDeleteCLI(t, func() int { return run(append(args, "--json")) })
	var report struct {
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if code != 0 || !strings.Contains(stderr, "WARNING: truncated: true") || !strings.Contains(stderr, "totals cover returned points only") || !report.Truncated {
		t.Fatalf("JSON usage = %d / %q / %q", code, stdout, stderr)
	}
}

func TestUsageCommandFailsWithoutTruncationOptIn(t *testing.T) {
	const message = "usage query exceeds the 10000-row cap; narrow --since/--until, use a coarser --group-by, or opt in with --allow-truncation (allow_truncation=1)"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("allow_truncation") || r.URL.Query().Has("max_rows") {
			t.Errorf("request opted in to truncation without the flag: %s", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","code":"usage_query_too_large","error":"` + message + `","max_rows":10000}`))
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_usage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "default"},
		{name: "json", args: []string{"--json"}},
		{name: "explicit false", args: []string{"--allow-truncation=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"usage", "--endpoint", srv.URL, "--token-file", tokenFile}, tc.args...)
			stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
			if code != 1 || stdout != "" || stderr != "witself: "+message+"\n" {
				t.Fatalf("rejected usage = %d / %q / %q", code, stdout, stderr)
			}
		})
	}
}
