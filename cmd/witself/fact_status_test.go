package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
)

func TestFactStatusCommandPrintsFiniteAndJSONCapacity(t *testing.T) {
	maximum, remaining := int64(1000), int64(100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/facts:status" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer witself_agt_fact_status" {
			t.Errorf("Authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"fact_capacity": client.FactLimitStatus{
				Used: 900, Max: &maximum, Remaining: &remaining, NearLimit: true,
			},
		})
	}))
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_fact_status\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connection := []string{"--endpoint", srv.URL, "--token-file", tokenFile}

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run(append([]string{"fact", "status"}, connection...))
	})
	if code != 0 || stderr != "" {
		t.Fatalf("fact status = %d, stderr %q", code, stderr)
	}
	for _, want := range []string{
		"used:\t900",
		"max:\t1000",
		"remaining:\t100",
		"near limit:\ttrue",
		"at limit:\tfalse",
		"over limit:\tfalse",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("fact status output lacks %q:\n%s", want, stdout)
		}
	}

	stdout, stderr, code = captureFactDeleteCLI(t, func() int {
		return run(append([]string{"fact", "status"}, append(connection, "--json")...))
	})
	if code != 0 || stderr != "" {
		t.Fatalf("fact status --json = %d, stderr %q", code, stderr)
	}
	var status client.FactLimitStatus
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatalf("decode JSON output: %v\n%s", err, stdout)
	}
	if status.Used != 900 || status.Max == nil || *status.Max != 1000 ||
		status.Remaining == nil || *status.Remaining != 100 || !status.NearLimit {
		t.Fatalf("JSON fact status = %#v", status)
	}
}
