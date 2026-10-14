package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestTerminalSnapshotUsesRealDemoMemory(t *testing.T) {
	view, err := terminalSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plain := ansi.Strip(view)
	for _, want := range []string{"Atlas", "studio", "Memories", "Wayfinding should feel calm", "DEMO"} {
		if !strings.Contains(plain, want) {
			t.Errorf("snapshot missing %q", want)
		}
	}
	if !strings.Contains(view, "\x1b[38;2;") {
		t.Error("snapshot has no true-color output")
	}
	lines := strings.Split(plain, "\n")
	if len(lines) != 40 {
		t.Errorf("snapshot has %d rows, want 40", len(lines))
	}
	for i, line := range lines {
		if width := ansi.StringWidth(line); width > 120 {
			t.Errorf("row %d exceeds 120 columns: %d", i, width)
		}
	}
}

func TestDemoCellFailsClosed(t *testing.T) {
	handler := demoCell("synthetic-test-token")
	for _, tc := range []struct {
		method, path, token string
		status              int
	}{
		{"GET", "/v1/self", "", http.StatusUnauthorized},
		{"POST", "/v1/memories", "synthetic-test-token", http.StatusMethodNotAllowed},
		{"PUT", "/v1/self/dashboard-preferences", "synthetic-test-token", http.StatusMethodNotAllowed},
		{"GET", "/v1/memories/missing/history", "synthetic-test-token", http.StatusNotFound},
		{"GET", "/v1/memories/mem_wayfinding/history/extra", "synthetic-test-token", http.StatusNotFound},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.status {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, response.Code, tc.status)
		}
	}
}

func TestRealDashboardReadsDemoMemoryThroughClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	accessURL, closeDemo, err := startDemo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDemo()
	parsed, err := url.Parse(accessURL)
	if err != nil || parsed.Hostname() != "127.0.0.1" {
		t.Fatal("demo must announce only a loopback listener")
	}
	base := parsed.Scheme + "://" + parsed.Host
	jar, _ := cookiejar.New(nil)
	httpClient := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	get := func(target string) (int, []byte) {
		t.Helper()
		response, err := httpClient.Get(target)
		if err != nil {
			t.Fatal("demo HTTP read failed")
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal("demo response read failed")
		}
		return response.StatusCode, body
	}
	if status, _ := get(base + "/api/self"); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dashboard returned %d", status)
	}
	if status, body := get(accessURL); status != http.StatusOK || !bytes.Contains(body, []byte("/static/app.js")) {
		t.Fatal("real dashboard did not serve its embedded application")
	}
	for _, tc := range []struct{ path, want string }{
		{"/api/self", `"agent_name":"Atlas"`},
		{"/api/memories", `"id":"mem_wayfinding"`},
		{"/api/memories/mem_wayfinding", "Wayfinding should feel calm"},
		{"/api/memories/mem_wayfinding/history", `"version":2`},
		{"/api/account/context", `"available":false`},
	} {
		if status, body := get(base + tc.path); status != http.StatusOK || !bytes.Contains(body, []byte(tc.want)) {
			t.Errorf("real dashboard projection failed for %s (status %d)", tc.path, status)
		}
	}
}

func TestStdinEOFStopsBothServers(t *testing.T) {
	var stdout bytes.Buffer
	err := run(context.Background(), []string{"--ansi-out", filepath.Join(t.TempDir(), "tui.ansi")}, strings.NewReader(""), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	var announcement struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(stdout.Bytes(), &announcement) != nil || announcement.URL == "" {
		t.Fatal("missing single process announcement")
	}
	httpClient := &http.Client{Timeout: time.Second}
	response, err := httpClient.Get(announcement.URL)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("console still serves after stdin EOF")
	}
}
