package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/local"
)

func routeCLIConfig(t *testing.T) (string, []byte) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("DSH_HOME", t.TempDir())
	p := filepath.Join(home, "config.json")
	before := []byte("{\n  \"accounts\": {\n    \"local-test\": {\n      \"id\": \"acc_synthetic\",\n      \"email\": \"private@example.invalid\"\n    }\n  }\n}\n")
	if err := os.WriteFile(p, before, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, before
}

func TestAccountRouteCLISetShowClear(t *testing.T) {
	p, before := routeCLIConfig(t)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/version" || r.Header.Get("Authorization") != "" {
			t.Error("probe used wrong path, method, or credentials")
		}
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0"}`))
	}))
	defer srv.Close()
	_, stderr, code := capturePlanCLI(t, func() int { return accountCmd([]string{"route", "set", "--local-endpoint", srv.URL, "local-test"}) })
	if code != 0 || stderr != "" || requests != 1 {
		t.Fatalf("set code=%d stderr=%q requests=%d", code, stderr, requests)
	}
	cfg, err := local.Load()
	if err != nil || cfg.Accounts["local-test"].Route == nil || cfg.Accounts["local-test"].Route.Endpoint != srv.URL {
		t.Fatal("set did not persist the route")
	}
	stdout, stderr, code := capturePlanCLI(t, func() int { return accountCmd([]string{"route", "show", "local-test"}) })
	var route local.Route
	if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &route) != nil || route.Kind != "local" || route.Endpoint != srv.URL {
		t.Fatal("show did not return the route")
	}
	if strings.Contains(stdout, "acc_synthetic") || strings.Contains(stdout, "private@example.invalid") {
		t.Fatal("show exposed account metadata")
	}
	_, stderr, code = capturePlanCLI(t, func() int { return accountCmd([]string{"route", "clear", "local-test"}) })
	after, err := os.ReadFile(p)
	if code != 0 || stderr != "" || err != nil || !bytes.Equal(before, after) {
		t.Fatal("clear did not restore the route-free config bytes")
	}
	stdout, _, code = capturePlanCLI(t, func() int { return accountCmd([]string{"route", "show", "local-test"}) })
	if code != 0 || strings.TrimSpace(stdout) != "null" || requests != 1 {
		t.Fatal("show/clear should not probe and an absent route must be null")
	}
}

func TestAccountRouteCLIRefusesProbeFailuresWithoutWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"wrong-schema", `{"schema_version":"other"}`, http.StatusOK},
		{"missing-schema", `{}`, http.StatusOK},
		{"invalid-json", `not-json-private-response`, http.StatusOK},
		{"trailing-json", `{"schema_version":"witself.v0"}{}`, http.StatusOK},
		{"oversized", `{"schema_version":"witself.v0","padding":"` + strings.Repeat("x", 64<<10) + `"}`, http.StatusOK},
		{"not-ok", `{"schema_version":"witself.v0"}`, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, before := routeCLIConfig(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, stderr, code := capturePlanCLI(t, func() int { return accountRouteCmd([]string{"set", "--local-endpoint", srv.URL, "local-test"}) })
			after, err := os.ReadFile(p)
			if code != 1 || err != nil || !bytes.Equal(before, after) || !strings.HasPrefix(stderr, "witself: local connection route version probe") {
				t.Fatal("failed probe must refuse without changing config")
			}
			if strings.Contains(stderr, srv.URL) || strings.Contains(stderr, tc.body) {
				t.Fatal("probe error exposed endpoint or response")
			}
		})
	}
}

func TestAccountRouteCLIRefusesDeadInvalidAndRedirectEndpoints(t *testing.T) {
	p, before := routeCLIConfig(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls++
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0"}`))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirect.Close()
	for _, endpoint := range []string{deadURL, "http://192.0.2.1", "https://127.0.0.1", "http://localhost", "http://127.0.0.2", "http://[::ffff:127.0.0.1]", "http://user:private@127.0.0.1", redirect.URL} {
		_, _, code := capturePlanCLI(t, func() int { return accountRouteCmd([]string{"set", "--local-endpoint", endpoint, "local-test"}) })
		after, err := os.ReadFile(p)
		if code != 1 || err != nil || !bytes.Equal(before, after) {
			t.Fatal("dead, invalid, or redirect endpoint changed config")
		}
	}
	if targetCalls != 0 {
		t.Fatal("probe followed a redirect")
	}
}

func TestAccountRouteCLIRefusesUnknownStoredKind(t *testing.T) {
	p, _ := routeCLIConfig(t)
	before := []byte(`{"accounts":{"local-test":{"id":"acc_synthetic","route":{"kind":"unknown-private-kind","endpoint":"http://127.0.0.1"}}}}`)
	if err := os.WriteFile(p, before, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"show", "local-test"}, {"set", "--local-endpoint", "http://127.0.0.1", "local-test"}} {
		_, stderr, code := capturePlanCLI(t, func() int { return accountRouteCmd(args) })
		after, err := os.ReadFile(p)
		if code != 1 || err != nil || !bytes.Equal(before, after) || strings.Contains(stderr, "unknown-private-kind") {
			t.Fatal("unknown route kind must fail closed without disclosing its value")
		}
	}
}

func TestAccountRouteCLIRequiresNonemptyName(t *testing.T) {
	p, _ := routeCLIConfig(t)
	before := []byte(`{"accounts":{"default":{"id":"acc_synthetic","route":{"kind":"local","endpoint":"http://127.0.0.1"}}}}`)
	if err := os.WriteFile(p, before, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"show", ""}, {"clear", ""}, {"set", "--local-endpoint", "http://127.0.0.1", ""}} {
		_, _, code := capturePlanCLI(t, func() int { return accountRouteCmd(args) })
		after, err := os.ReadFile(p)
		if code != 2 || err != nil || !bytes.Equal(before, after) {
			t.Fatal("an empty route name selected the default account")
		}
	}
}
