package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
)

var dashboardBannerPattern = regexp.MustCompile(
	`witself dashboard: serving agent dash on (http://127\.0\.0\.1:(\d+)/\?token=[0-9a-f]{32})`)

func TestDashboardServeEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_ACCOUNT", "")
	t.Setenv("WITSELF_REALM", "")
	t.Setenv("WITSELF_AGENT", "")

	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_dash\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	identity := client.SelfIdentity{
		AccountID: "acc_1", AgentID: "agt_dash", AgentName: "dash",
		RealmID: "rlm_1", RealmName: "default",
	}
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer witself_agt_dash" {
			t.Errorf("Authorization = %q", got)
		}
		if r.Method+" "+r.URL.Path != "GET /v1/self" {
			t.Errorf("unexpected cell request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(client.SelfDigest{
			SchemaVersion: "witself.v0", Identity: identity,
		}); err != nil {
			t.Errorf("encode self digest: %v", err)
		}
	}))
	defer cell.Close()

	// Capture stderr through a pipe so the startup banner can be read while
	// the server is still serving (captureFactDeleteCLI only returns output
	// after the function under test finishes).
	oldStderr := os.Stderr
	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	os.Stderr = pipeWriter
	defer func() { os.Stderr = oldStderr }()
	lines := make(chan string, 64)
	scanErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(pipeReader)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		scanErr <- scanner.Err()
		close(lines)
	}()

	args := []string{"--endpoint", cell.URL, "--token-file", tokenFile, "--agent", "dash"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- dashboardServe(ctx, args) }()

	var serveURL string
	deadline := time.After(15 * time.Second)
	for serveURL == "" {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stderr closed before the serve banner appeared")
			}
			if match := dashboardBannerPattern.FindStringSubmatch(line); match != nil {
				serveURL = match[1]
			}
		case code := <-done:
			t.Fatalf("dashboard serve exited early with code %d", code)
		case <-deadline:
			t.Fatal("timed out waiting for the serve banner")
		}
	}

	registryPath := filepath.Join(home, "dashboards", "agt_dash.json")
	rawEntry, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatalf("registry entry missing while serving: %v", err)
	}
	var registered dashboard.RegistryEntry
	if err := json.Unmarshal(rawEntry, &registered); err != nil {
		t.Fatalf("decode registry entry: %v", err)
	}
	if registered.AccessURL != serveURL {
		t.Fatalf("registry access_url = %q, want the banner URL %q", registered.AccessURL, serveURL)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	browser := &http.Client{Jar: jar, Timeout: 10 * time.Second}

	index, err := browser.Get(serveURL) // ?token= URL: 303 into the cookie session
	if err != nil {
		t.Fatalf("GET %s: %v", serveURL, err)
	}
	if index.StatusCode != http.StatusOK {
		t.Fatalf("index after token redirect: got %d, want 200", index.StatusCode)
	}
	_ = index.Body.Close()

	parsed, err := url.Parse(serveURL)
	if err != nil {
		t.Fatalf("parse serve URL: %v", err)
	}
	selfResp, err := browser.Get("http://" + parsed.Host + "/api/self")
	if err != nil {
		t.Fatalf("GET /api/self: %v", err)
	}
	defer func() { _ = selfResp.Body.Close() }()
	if selfResp.StatusCode != http.StatusOK {
		t.Fatalf("/api/self: got %d, want 200", selfResp.StatusCode)
	}
	var envelope struct {
		Identity      client.SelfIdentity `json:"identity"`
		Observational bool                `json:"observational"`
	}
	if err := json.NewDecoder(selfResp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode /api/self: %v", err)
	}
	if envelope.Identity != identity {
		t.Fatalf("identity round trip mismatch: %+v", envelope.Identity)
	}
	if !envelope.Observational {
		t.Fatal("/api/self should report observational reads against this fake")
	}

	// A second serve for the same agent must refuse while the first is live.
	if code := dashboardServe(context.Background(), args); code != 1 {
		t.Fatalf("second serve for a live agent: got exit %d, want 1", code)
	}

	// Open a live SSE stream so shutdown is exercised with a connection that
	// never goes idle: the serve ctx must end the stream promptly instead of
	// stalling the full 5s Shutdown timeout.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	defer sseCancel()
	sseReq, err := http.NewRequestWithContext(sseCtx, http.MethodGet, "http://"+parsed.Host+"/api/events", nil)
	if err != nil {
		t.Fatalf("new events request: %v", err)
	}
	sseResp, err := browser.Do(sseReq)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer func() { _ = sseResp.Body.Close() }()
	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("/api/events: got %d, want 200", sseResp.StatusCode)
	}

	shutdownStart := time.Now()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("dashboard serve exited with code %d, want 0", code)
		}
		if elapsed := time.Since(shutdownStart); elapsed >= 4*time.Second {
			t.Fatalf("shutdown took %v with an open SSE stream; the stream is not wired to the serve ctx", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("dashboard serve did not shut down after ctx cancel")
	}
	if _, err := os.Stat(registryPath); !os.IsNotExist(err) {
		t.Fatalf("registry entry not removed on shutdown: %v", err)
	}
	_ = pipeWriter.Close()
	if err := <-scanErr; err != nil {
		t.Errorf("scan captured stderr: %v", err)
	}
}

// seedDashboardRegistry writes one live and one stale registry entry under a
// fresh WITSELF_HOME: "scout" backed by a marker-header responder and this
// test's own PID, "drone" pointing at a PID far beyond any real pid table.
func seedDashboardRegistry(t *testing.T) (live, stale dashboard.RegistryEntry) {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())

	marker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(dashboard.MarkerHeader, dashboard.RegistrySchemaVersion)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(marker.Close)
	addr, ok := marker.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr %T is not *net.TCPAddr", marker.Listener.Addr())
	}

	started := time.Date(2026, 7, 18, 9, 30, 0, 0, time.UTC)
	live = dashboard.RegistryEntry{
		AgentID: "agt_live", AgentName: "scout", Account: "default", Realm: "default",
		Port: addr.Port, PID: os.Getpid(),
		URL:       fmt.Sprintf("http://127.0.0.1:%d/", addr.Port),
		AccessURL: fmt.Sprintf("http://127.0.0.1:%d/?token=%s", addr.Port, strings.Repeat("ab", 16)),
		StartedAt: started,
	}
	stale = dashboard.RegistryEntry{
		AgentID: "agt_stale", AgentName: "drone", Account: "default", Realm: "edge",
		Port: addr.Port, PID: 1 << 26,
		URL:       fmt.Sprintf("http://127.0.0.1:%d/", addr.Port),
		AccessURL: fmt.Sprintf("http://127.0.0.1:%d/?token=%s", addr.Port, strings.Repeat("cd", 16)),
		StartedAt: started,
	}
	for _, entry := range []dashboard.RegistryEntry{live, stale} {
		if err := dashboard.WriteRegistryEntry(entry); err != nil {
			t.Fatalf("write %s: %v", entry.AgentID, err)
		}
	}
	return live, stale
}

func TestDashboardStatusText(t *testing.T) {
	live, stale := seedDashboardRegistry(t)

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return dashboardCmd([]string{"status"})
	})
	if code != 0 {
		t.Fatalf("status exit = %d, stderr %q", code, stderr)
	}
	rows := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(rows) != 2 {
		t.Fatalf("status rows = %q, want 2", rows)
	}
	drone, scout := strings.Split(rows[0], "\t"), strings.Split(rows[1], "\t")
	if len(drone) != 7 || len(scout) != 7 {
		t.Fatalf("column count: drone=%d scout=%d, want 7", len(drone), len(scout))
	}
	if drone[0] != "drone" || drone[1] != "edge" || drone[5] != "stale" || drone[6] != "-" {
		t.Fatalf("stale row = %q", rows[0])
	}
	if scout[0] != "scout" || scout[1] != "default" || scout[5] != "live" || scout[6] != live.AccessURL {
		t.Fatalf("live row = %q", rows[1])
	}
	if scout[2] != strconv.Itoa(live.Port) || scout[3] != strconv.Itoa(os.Getpid()) || scout[4] != "2026-07-18T09:30:00Z" {
		t.Fatalf("live row fields = %q", rows[1])
	}
	if strings.Contains(stdout, stale.AccessURL) {
		t.Fatalf("stale access URL printed: %q", stdout)
	}

	stdout, stderr, code = captureFactDeleteCLI(t, func() int {
		return dashboardCmd([]string{"status", "--agent", "scout"})
	})
	if code != 0 {
		t.Fatalf("filtered status exit = %d, stderr %q", code, stderr)
	}
	if rows := strings.Split(strings.TrimSpace(stdout), "\n"); len(rows) != 1 || !strings.HasPrefix(rows[0], "scout\t") {
		t.Fatalf("--agent scout rows = %q, want just scout", rows)
	}

	stdout, stderr, code = captureFactDeleteCLI(t, func() int {
		return dashboardCmd([]string{"status", "--realm", "elsewhere"})
	})
	if code != 0 || strings.TrimSpace(stdout) != "" {
		t.Fatalf("no-match filter: exit=%d stdout=%q stderr=%q, want empty success", code, stdout, stderr)
	}

	if _, _, code := captureFactDeleteCLI(t, func() int {
		return dashboardCmd([]string{"status", "extra"})
	}); code != 2 {
		t.Fatalf("positional arg exit = %d, want 2", code)
	}
	if _, _, code := captureFactDeleteCLI(t, func() int {
		return dashboardCmd([]string{"bogus"})
	}); code != 2 {
		t.Fatalf("unknown subcommand exit = %d, want 2", code)
	}
}

func TestDashboardStatusJSON(t *testing.T) {
	live, stale := seedDashboardRegistry(t)

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return dashboardCmd([]string{"status", "--json"})
	})
	if code != 0 {
		t.Fatalf("status --json exit = %d, stderr %q", code, stderr)
	}
	var payload struct {
		Dashboards []struct {
			SchemaVersion string    `json:"schema_version"`
			AgentID       string    `json:"agent_id"`
			AgentName     string    `json:"agent_name"`
			Account       string    `json:"account"`
			Realm         string    `json:"realm"`
			Port          int       `json:"port"`
			PID           int       `json:"pid"`
			URL           string    `json:"url"`
			AccessURL     string    `json:"access_url"`
			StartedAt     time.Time `json:"started_at"`
			Live          bool      `json:"live"`
		} `json:"dashboards"`
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		t.Fatalf("decode status JSON (shape drifted?): %v\n%s", err, stdout)
	}
	if len(payload.Dashboards) != 2 {
		t.Fatalf("dashboards = %+v, want 2 entries", payload.Dashboards)
	}
	drone, scout := payload.Dashboards[0], payload.Dashboards[1]
	if drone.AgentName != "drone" || drone.Live || drone.AccessURL != stale.AccessURL {
		t.Fatalf("stale entry = %+v", drone)
	}
	if scout.AgentName != "scout" || !scout.Live || scout.AccessURL != live.AccessURL {
		t.Fatalf("live entry = %+v", scout)
	}
	if scout.SchemaVersion != dashboard.RegistrySchemaVersion || scout.Port != live.Port ||
		scout.PID != os.Getpid() || !scout.StartedAt.Equal(live.StartedAt) {
		t.Fatalf("live entry fields = %+v", scout)
	}

	stdout, _, code = captureFactDeleteCLI(t, func() int {
		return dashboardCmd([]string{"status", "--agent", "nobody", "--json"})
	})
	if code != 0 || strings.TrimSpace(stdout) != `{"dashboards":[]}` {
		t.Fatalf("empty filter JSON: exit=%d stdout=%q, want {\"dashboards\":[]}", code, stdout)
	}
}
