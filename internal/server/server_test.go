package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthProbes(t *testing.T) {
	srv := httptest.NewServer(healthMux(nil))
	defer srv.Close()
	for _, p := range []string{"/livez", "/readyz", "/startupz", "/healthz"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestReadyzReflectsReadiness(t *testing.T) {
	// Failing readiness check -> 503, but liveness stays 200.
	down := httptest.NewServer(healthMux(func(context.Context) error { return errors.New("db down") }))
	defer down.Close()
	if code := getStatus(t, down.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz with failing check = %d, want 503", code)
	}
	if code := getStatus(t, down.URL+"/livez"); code != http.StatusOK {
		t.Errorf("/livez = %d, want 200 (must not gate on readiness)", code)
	}

	// Passing readiness check -> 200.
	up := httptest.NewServer(healthMux(func(context.Context) error { return nil }))
	defer up.Close()
	if code := getStatus(t, up.URL+"/readyz"); code != http.StatusOK {
		t.Errorf("/readyz with passing check = %d, want 200", code)
	}
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestMetricsExposesUp(t *testing.T) {
	srv := httptest.NewServer(metricsMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "witself_up 1") {
		t.Errorf("metrics missing witself_up gauge:\n%s", body)
	}
}

func TestVersionEndpointIsBare(t *testing.T) {
	srv := httptest.NewServer(apiMux("", nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	// Bare shape: schema_version + version/commit/date at the top level.
	for _, k := range []string{"schema_version", "version", "commit", "date"} {
		if _, ok := m[k]; !ok {
			t.Errorf("version missing %q; got %v", k, m)
		}
	}
	if _, enveloped := m["data"]; enveloped {
		t.Errorf("version should be bare, found envelope data field: %v", m)
	}
}

func TestCapabilitiesShape(t *testing.T) {
	t.Setenv("WITSELF_BACKEND_KIND", "self-hosted")
	srv := httptest.NewServer(apiMux("", nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var c capabilities
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.SchemaVersion != "witself.v0" {
		t.Errorf("schema_version = %q", c.SchemaVersion)
	}
	if c.Backend.Kind != "self-hosted" || c.Backend.APIVersion != "v1" {
		t.Errorf("backend = %+v", c.Backend)
	}
	if f, ok := c.Features["memories"]; !ok || f.Supported || f.Reason != "not_implemented" {
		t.Errorf("memories feature = %+v (ok=%v)", f, ok)
	}
	if c.Account != nil {
		t.Errorf("account should be omitted when no account id is set, got %+v", c.Account)
	}
}

func TestCapabilitiesIncludesAccount(t *testing.T) {
	srv := httptest.NewServer(apiMux("acc_test123", nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var c capabilities
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.Account == nil || c.Account.ID != "acc_test123" {
		t.Errorf("account = %+v, want id acc_test123", c.Account)
	}
}
