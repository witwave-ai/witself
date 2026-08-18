package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/avatar"
	"github.com/witwave-ai/witself/internal/client"
)

var testIdentity = client.SelfIdentity{
	AccountID: "acc_1",
	AgentID:   "agt_dash",
	AgentName: "dash",
	RealmID:   "rlm_1",
	RealmName: "default",
}

const testBearer = "witself_agt_dash"

func writeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode fake response: %v", err)
	}
}

func testSelfDigest() client.SelfDigest {
	return client.SelfDigest{SchemaVersion: "witself.v0", Identity: testIdentity}
}

// newDashboard mounts Register onto an httptest server backed by the given
// cell handler and returns the server plus its Config.
func newDashboard(t *testing.T, backend http.HandlerFunc, mutate func(*Config)) (*httptest.Server, Config) {
	t.Helper()
	cell := httptest.NewServer(backend)
	t.Cleanup(cell.Close)
	cfg := Config{
		Endpoint:     cell.URL,
		BearerToken:  testBearer,
		AccessToken:  "0123456789abcdef0123456789abcdef",
		Identity:     testIdentity,
		Version:      "test",
		PollInterval: 20 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	mux := http.NewServeMux()
	if err := Register(mux, cfg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, cfg
}

func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func serverPort(srv *httptest.Server) string {
	return srv.URL[strings.LastIndex(srv.URL, ":")+1:]
}

// sessionCookie performs the one-time ?token= exchange and returns the
// per-port session cookie a browser would hold afterwards.
func sessionCookie(t *testing.T, srv *httptest.Server, cfg Config) *http.Cookie {
	t.Helper()
	resp, err := noRedirectClient().Get(srv.URL + "/?token=" + cfg.AccessToken)
	if err != nil {
		t.Fatalf("token exchange: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("token exchange: got %d, want 303", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if strings.HasPrefix(cookie.Name, accessCookiePrefix) {
			return cookie
		}
	}
	t.Fatal("token exchange set no session cookie")
	return nil
}

func authedRequest(t *testing.T, srv *httptest.Server, cfg Config, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(sessionCookie(t, srv, cfg))
	return req
}

func authedGet(t *testing.T, srv *httptest.Server, cfg Config, path string) *http.Response {
	t.Helper()
	resp, err := srv.Client().Do(authedRequest(t, srv, cfg, path))
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func selfBackend(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "GET /v1/self" {
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeTestJSON(t, w, testSelfDigest())
	}
}

func TestRegisterRequiresEndpointAndAccessToken(t *testing.T) {
	if err := Register(http.NewServeMux(), Config{Endpoint: "http://127.0.0.1:1"}); err == nil {
		t.Fatal("Register accepted an empty access token")
	}
	if err := Register(http.NewServeMux(), Config{AccessToken: "tok"}); err == nil {
		t.Fatal("Register accepted an empty endpoint")
	}
}

func TestHostHeaderPinnedToLoopbackListener(t *testing.T) {
	srv, cfg := newDashboard(t, selfBackend(t), nil)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]

	cases := []struct {
		name string
		host string
		want int
	}{
		{"rebound name", "evil.example:" + port, http.StatusForbidden},
		{"wrong port", "127.0.0.1:1", http.StatusForbidden},
		{"portless host", "127.0.0.1", http.StatusForbidden},
		{"loopback ip", "127.0.0.1:" + port, http.StatusOK},
		{"localhost", "localhost:" + port, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := authedRequest(t, srv, cfg, "/api/self")
			req.Host = tc.host
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Fatalf("Host %q: got %d, want %d", tc.host, resp.StatusCode, tc.want)
			}
		})
	}
}

func TestAccessTokenFlow(t *testing.T) {
	srv, cfg := newDashboard(t, selfBackend(t), nil)
	noRedirect := noRedirectClient()

	t.Run("bare request is unauthorized", func(t *testing.T) {
		resp, err := noRedirect.Get(srv.URL + "/api/self")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401", resp.StatusCode)
		}
	})

	t.Run("query token sets session cookie and redirects tokenless", func(t *testing.T) {
		resp, err := noRedirect.Get(srv.URL + "/api/self?token=" + cfg.AccessToken)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("got %d, want 303", resp.StatusCode)
		}
		if location := resp.Header.Get("Location"); location != "/api/self" {
			t.Fatalf("Location = %q, want /api/self", location)
		}
		var cookie *http.Cookie
		for _, c := range resp.Cookies() {
			if strings.HasPrefix(c.Name, accessCookiePrefix) {
				cookie = c
			}
		}
		if cookie == nil {
			t.Fatal("no access cookie set")
		}
		if want := accessCookiePrefix + serverPort(srv); cookie.Name != want {
			t.Fatalf("cookie name = %q, want port-scoped %q", cookie.Name, want)
		}
		if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
			t.Fatalf("cookie attributes not hardened: %+v", cookie)
		}
		if cookie.Value == "" || cookie.Value == cfg.AccessToken {
			t.Fatalf("cookie must hold a session value distinct from the URL token")
		}
	})

	t.Run("wrong query token is unauthorized", func(t *testing.T) {
		resp, err := noRedirect.Get(srv.URL + "/api/self?token=" + strings.Repeat("f", 32))
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401", resp.StatusCode)
		}
	})

	t.Run("valid session cookie is accepted", func(t *testing.T) {
		resp := authedGet(t, srv, cfg, "/api/self")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("got %d, want 200", resp.StatusCode)
		}
	})

	t.Run("wrong cookie is unauthorized", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/self", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.AddCookie(&http.Cookie{Name: accessCookiePrefix + serverPort(srv), Value: strings.Repeat("0", 32)})
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401", resp.StatusCode)
		}
	})

	t.Run("url token replayed as a cookie is rejected", func(t *testing.T) {
		// The printed ?token= credential must never be a valid cookie: a
		// hostile loopback listener that reads leaked cookies must not be
		// able to mint sessions from them.
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/self", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.AddCookie(&http.Cookie{Name: accessCookiePrefix + serverPort(srv), Value: cfg.AccessToken})
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401", resp.StatusCode)
		}
	})

	t.Run("session cookie minted for another port is rejected", func(t *testing.T) {
		cookie := sessionCookie(t, srv, cfg)
		cookie.Name = accessCookiePrefix + "1"
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/self", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.AddCookie(cookie)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401", resp.StatusCode)
		}
	})
}

// TestFetchMetadataRefusesCrossSiteRequests proves pages on other origins —
// including other loopback ports, which SameSite=Strict treats as same-site
// (port-blind, RFC 6265) and therefore sends the session cookie to — cannot
// issue credentialed requests: any browser-tagged cross-origin fetch is
// refused before the handler runs, so a hostile local page can neither hold
// SSE slots nor drive authenticated upstream polling.
func TestFetchMetadataRefusesCrossSiteRequests(t *testing.T) {
	srv, cfg := newDashboard(t, selfBackend(t), nil)

	cases := []struct {
		site string
		want int
	}{
		{"", http.StatusOK}, // non-browser clients send no fetch metadata
		{"same-origin", http.StatusOK},
		{"none", http.StatusOK}, // address-bar and bookmark navigations
		{"same-site", http.StatusForbidden},
		{"cross-site", http.StatusForbidden},
	}
	for _, tc := range cases {
		name := tc.site
		if name == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			req := authedRequest(t, srv, cfg, "/api/self")
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Fatalf("Sec-Fetch-Site %q: got %d, want %d", tc.site, resp.StatusCode, tc.want)
			}
		})
	}
}

// TestConcurrentDashboardsUseDistinctCookies proves two dashboards on
// different loopback ports can be used from one browser: RFC 6265 cookies
// are host-scoped only, so the browser sends both cookies everywhere, and
// each dashboard must pick out its own port-scoped session.
func TestConcurrentDashboardsUseDistinctCookies(t *testing.T) {
	srvA, cfgA := newDashboard(t, selfBackend(t), nil)
	srvB, cfgB := newDashboard(t, selfBackend(t), func(cfg *Config) {
		cfg.AccessToken = "fedcba9876543210fedcba9876543210"
	})
	cookieA := sessionCookie(t, srvA, cfgA)
	cookieB := sessionCookie(t, srvB, cfgB)
	if cookieA.Name == cookieB.Name {
		t.Fatalf("both dashboards used cookie name %q; sessions would clobber", cookieA.Name)
	}

	// A browser sends every 127.0.0.1 cookie to both servers.
	req, err := http.NewRequest(http.MethodGet, srvA.URL+"/api/self", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(cookieA)
	req.AddCookie(cookieB)
	resp, err := srvA.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard A with both cookies: got %d, want 200", resp.StatusCode)
	}

	// The other dashboard's session alone must not grant access.
	cross, err := http.NewRequest(http.MethodGet, srvA.URL+"/api/self", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	cross.AddCookie(cookieB)
	crossResp, err := srvA.Client().Do(cross)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = crossResp.Body.Close() }()
	if crossResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dashboard A with only B's cookie: got %d, want 401", crossResp.StatusCode)
	}
}

func TestStaticIndexServedWithSecurityHeaders(t *testing.T) {
	srv, cfg := newDashboard(t, selfBackend(t), nil)
	resp := authedGet(t, srv, cfg, "/")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	wantCSP := "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'"
	if got := resp.Header.Get("Content-Security-Policy"); got != wantCSP {
		t.Fatalf("CSP = %q, want %q", got, wantCSP)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q, want DENY", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "Witself Agent Console") {
		t.Fatal("index body missing title")
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}

	theme := authedGet(t, srv, cfg, "/static/themes/console.css")
	defer func() { _ = theme.Body.Close() }()
	if theme.StatusCode != http.StatusOK {
		t.Fatalf("theme css: got %d, want 200", theme.StatusCode)
	}
	if got := theme.Header.Get("Content-Security-Policy"); got != wantCSP {
		t.Fatalf("theme CSP = %q", got)
	}
}

func TestStaticEmailSurfaceIsMetadataOnlyAndCheckpointLinked(t *testing.T) {
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read app: %v", err)
	}
	for _, want := range []string{
		`href="#/email" data-nav="email"`,
	} {
		if !strings.Contains(string(index), want) {
			t.Errorf("index missing %q", want)
		}
	}
	for _, want := range []string{
		`self.email_checkpoint`, `href: "#/email"`, `fetchJSON("/api/email/address")`,
		`fetchJSON("/api/email/status")`, `fetchJSON("/api/email?"`,
		`fetchJSON("/api/email/sent?limit=100")`, `params.push("email_sent=true")`,
		`source.addEventListener("email_sent"`, `message.request_kind`,
		`params.push("email=true")`, `params.push("email_unread=true")`,
		`params.push("email_unacked=true")`, "raw MIME", "processing claims never enter this page",
		"account-wide attachment capacity", "attachment payload omitted because account-wide capacity is full",
		"newest 100 sent messages at most", "Bodies, message identifiers, and delivery actions never enter this page",
		`updateEmailAddressFromCheckpoint(self.email_checkpoint)`,
		`emailStateChanged && current.section === "email"`,
		`checkpoint.enabled === false`, `inbound email is not enabled on this account`,
		`state.emailStatus = body.status || null`,
		`event.data === state.lastEmailData`,
	} {
		if !strings.Contains(string(app), want) {
			t.Errorf("app missing %q", want)
		}
	}
	// Browser code has no action URL; all mutations remain available only to
	// active agents through the CLI/MCP surfaces, never this local viewer.
	if strings.Contains(string(app), `"/api/email:`) || strings.Contains(string(app), `"/api/email/" +`) {
		t.Fatal("email UI contains a per-message action URL")
	}
	for _, forbidden := range []string{`":send"`, `":reply"`, `":read"`, `":ack"`, `":claim"`, `":complete"`, `":retry"`, `":cancel"`} {
		if strings.Contains(string(app), forbidden) {
			t.Fatalf("email UI contains lifecycle action %s", forbidden)
		}
	}
}

func TestDashboardEmailLiveEnableTransition(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "testdata/email_transition_test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard email transition JavaScript test: %v\n%s", err, output)
	}
}

func TestDashboardAgentEmailStorageRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "testdata/email_capacity_test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard email capacity JavaScript test: %v\n%s", err, output)
	}
}

func TestDashboardAgentEmailSentRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "testdata/email_sent_test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard sent email JavaScript test: %v\n%s", err, output)
	}
}

func TestDashboardMemoryCapacityRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "testdata/memory_capacity_test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard memory capacity JavaScript test: %v\n%s", err, output)
	}
}

func TestDashboardFactCapacityRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "testdata/fact_capacity_test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard fact capacity JavaScript test: %v\n%s", err, output)
	}
}

func TestDashboardPlanEntitlementsRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "testdata/plan_entitlements_test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard plan entitlements JavaScript test: %v\n%s", err, output)
	}
}

func TestSelfProxySendsObservationalAndDegradesOn501(t *testing.T) {
	t.Run("observational round trip", func(t *testing.T) {
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/self" {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			query := r.URL.Query()
			if query.Get("observational") != "true" {
				t.Errorf("observational = %q, want true", query.Get("observational"))
			}
			if query.Get("include_facts") != "false" || query.Get("include_salient") != "true" ||
				query.Get("include_counts") != "true" || query.Get("include_checkpoint") != "true" ||
				query.Get("include_message_checkpoint") != "true" || query.Get("include_email_checkpoint") != "true" ||
				query.Get("include_avatar_checkpoint") != "true" || query.Get("include_plan_entitlements") != "true" {
				t.Errorf("unexpected include flags: %s", r.URL.RawQuery)
			}
			if query.Get("include_sensitive") != "false" {
				t.Errorf("include_sensitive = %q, want false", query.Get("include_sensitive"))
			}
			writeTestJSON(t, w, testSelfDigest())
		}, nil)
		resp := authedGet(t, srv, cfg, "/api/self")
		defer func() { _ = resp.Body.Close() }()
		var envelope struct {
			Identity      client.SelfIdentity `json:"identity"`
			Observational bool                `json:"observational"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !envelope.Observational {
			t.Fatal("observational should be true")
		}
		if envelope.Identity != testIdentity {
			t.Fatalf("identity = %+v", envelope.Identity)
		}
	})

	t.Run("degrades once on 501", func(t *testing.T) {
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("observational") == "true" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotImplemented)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "observational fact hydration is unavailable"})
				return
			}
			writeTestJSON(t, w, testSelfDigest())
		}, nil)
		resp := authedGet(t, srv, cfg, "/api/self")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("got %d, want 200", resp.StatusCode)
		}
		var envelope struct {
			Observational bool `json:"observational"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if envelope.Observational {
			t.Fatal("observational should be false after the 501 degrade")
		}
	})
}

func TestSelfProxyPlanEntitlementsUsesClosedBrowserAllowList(t *testing.T) {
	t.Run("applied projection strips every forbidden upstream field", func(t *testing.T) {
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("include_plan_entitlements") != "true" {
				t.Errorf("include_plan_entitlements = %q", r.URL.Query().Get("include_plan_entitlements"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","identity":{"account_id":"acc_1","agent_id":"agt_dash","agent_name":"dash","realm_id":"rlm_1","realm_name":"default"},"primary_facts":[],"salient_memories":[],"plan_entitlements":{"schema_version":"witself.agent-entitlements.v1","state":"applied","source":"cell_applied_snapshot","enforced_plan_id":"standard","features":{"memory":true,"facts":true,"secrets":false,"messaging":true,"collaboration":false,"agent_email_receive":true,"agent_email_send":false,"support":true,"billing_admin":true},"retention_days":{"transcript_retention_days":90,"message_retention_days":30,"agent_email_retention_days":null,"billing_receipt_retention_days":3650},"subscription":{"status":"active"},"payment_method":"pm_secret","provider":"stripe","pending_transition":"team","portal_url":"https://example.test/secret","revision":99,"hash":"secret"},"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`)
		}, nil)
		resp := authedGet(t, srv, cfg, "/api/self")
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			"support", "billing_admin", "billing_receipt_retention_days", "subscription",
			"payment_method", "provider", "pending_transition", "portal_url", "revision", "hash", "pm_secret",
		} {
			if bytes.Contains(body, []byte(forbidden)) {
				t.Fatalf("browser response contains forbidden %q: %s", forbidden, body)
			}
		}
		var root map[string]json.RawMessage
		if err := json.Unmarshal(body, &root); err != nil {
			t.Fatal(err)
		}
		var block map[string]json.RawMessage
		if err := json.Unmarshal(root["plan_entitlements"], &block); err != nil {
			t.Fatal(err)
		}
		assertDashboardJSONKeys(t, block, "schema_version", "state", "source", "enforced_plan_id", "features", "retention_days")
		var features map[string]json.RawMessage
		if err := json.Unmarshal(block["features"], &features); err != nil {
			t.Fatal(err)
		}
		assertDashboardJSONKeys(t, features, "memory", "facts", "secrets", "messaging", "collaboration", "agent_email_receive", "agent_email_send")
		var retention map[string]json.RawMessage
		if err := json.Unmarshal(block["retention_days"], &retention); err != nil {
			t.Fatal(err)
		}
		assertDashboardJSONKeys(t, retention, "transcript_retention_days", "message_retention_days", "agent_email_retention_days")
	})

	t.Run("old-cell omission stays omitted", func(t *testing.T) {
		srv, cfg := newDashboard(t, selfBackend(t), nil)
		resp := authedGet(t, srv, cfg, "/api/self")
		defer func() { _ = resp.Body.Close() }()
		var root map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
			t.Fatal(err)
		}
		if _, exists := root["plan_entitlements"]; exists {
			t.Fatal("old-cell projection was invented at the browser boundary")
		}
	})

	t.Run("unsafe applied projection becomes value-free unavailable", func(t *testing.T) {
		bad := testSelfDigest()
		bad.PlanEntitlements = &client.SelfAgentEntitlements{
			SchemaVersion: "witself.agent-entitlements.v1", State: "applied", Source: "cell_applied_snapshot",
			EnforcedPlanID: `<img src=x onerror="alert(1)">`,
			Features:       &client.SelfAgentEntitlementFeatures{Memory: true},
			RetentionDays:  &client.SelfAgentRetentionDays{},
		}
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, _ *http.Request) { writeTestJSON(t, w, bad) }, nil)
		resp := authedGet(t, srv, cfg, "/api/self")
		defer func() { _ = resp.Body.Close() }()
		var envelope selfEnvelope
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		got := envelope.PlanEntitlements
		if got == nil || got.State != "unavailable" || got.EnforcedPlanID != "" ||
			got.Features != nil || got.RetentionDays != nil {
			t.Fatalf("unsafe projection = %#v", got)
		}
	})

	t.Run("incomplete closed maps become unavailable instead of implied false or indefinite", func(t *testing.T) {
		for _, body := range []string{
			`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","agent_id":"agt_dash","agent_name":"dash","realm_id":"rlm_1","realm_name":"default"},"primary_facts":[],"salient_memories":[],"plan_entitlements":{"schema_version":"witself.agent-entitlements.v1","state":"applied","source":"cell_applied_snapshot","enforced_plan_id":"standard","features":{"memory":true,"facts":true,"secrets":false,"messaging":true,"collaboration":false,"agent_email_receive":true},"retention_days":{"transcript_retention_days":90,"message_retention_days":30,"agent_email_retention_days":null}},"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`,
			`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","agent_id":"agt_dash","agent_name":"dash","realm_id":"rlm_1","realm_name":"default"},"primary_facts":[],"salient_memories":[],"plan_entitlements":{"schema_version":"witself.agent-entitlements.v1","state":"applied","source":"cell_applied_snapshot","enforced_plan_id":"standard","features":{"memory":true,"facts":true,"secrets":false,"messaging":true,"collaboration":false,"agent_email_receive":true,"agent_email_send":false},"retention_days":{"transcript_retention_days":90,"message_retention_days":30}},"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`,
			`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","agent_id":"agt_dash","agent_name":"dash","realm_id":"rlm_1","realm_name":"default"},"primary_facts":[],"salient_memories":[],"plan_entitlements":{"schema_version":"witself.agent-entitlements.v1","state":"applied","source":"cell_applied_snapshot","enforced_plan_id":"standard","features":{"memory":null,"facts":true,"secrets":false,"messaging":true,"collaboration":false,"agent_email_receive":true,"agent_email_send":false},"retention_days":{"transcript_retention_days":90,"message_retention_days":30,"agent_email_retention_days":null}},"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`,
			`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","agent_id":"agt_dash","agent_name":"dash","realm_id":"rlm_1","realm_name":"default"},"primary_facts":[],"salient_memories":[],"plan_entitlements":{"schema_version":"witself.agent-entitlements.v1","state":"applied","source":"cell_applied_snapshot","enforced_plan_id":"standard","features":{"memory":true,"facts":true,"secrets":false,"messaging":true,"collaboration":false,"agent_email_receive":true,"Agent_Email_Send":false},"retention_days":{"transcript_retention_days":90,"message_retention_days":30,"agent_email_retention_days":null}},"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`,
			`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","agent_id":"agt_dash","agent_name":"dash","realm_id":"rlm_1","realm_name":"default"},"primary_facts":[],"salient_memories":[],"plan_entitlements":{"schema_version":"witself.agent-entitlements.v1","state":"applied","source":"cell_applied_snapshot","enforced_plan_id":"standard","features":{"memory":true,"facts":true,"secrets":false,"messaging":true,"collaboration":false,"agent_email_receive":true,"agent_email_send":false},"retention_days":{"transcript_retention_days":90,"message_retention_days":"30","agent_email_retention_days":null}},"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`,
			`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","agent_id":"agt_dash","agent_name":"dash","realm_id":"rlm_1","realm_name":"default"},"primary_facts":[],"salient_memories":[],"plan_entitlements":{"schema_version":"witself.agent-entitlements.v1","state":"applied","source":"cell_applied_snapshot","enforced_plan_id":"standard","features":{"memory":true,"facts":true,"secrets":false,"messaging":true,"collaboration":false,"agent_email_receive":true,"agent_email_send":false},"retention_days":{"transcript_retention_days":90,"Message_Retention_Days":30,"agent_email_retention_days":null}},"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`,
		} {
			srv, cfg := newDashboard(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}, nil)
			resp := authedGet(t, srv, cfg, "/api/self")
			var envelope selfEnvelope
			if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
				_ = resp.Body.Close()
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if got := envelope.PlanEntitlements; got == nil || got.State != "unavailable" ||
				got.Features != nil || got.RetentionDays != nil {
				t.Fatalf("incomplete projection = %#v", got)
			}
		}
	})

	t.Run("exact fields win over case aliases and aliases never reach the browser", func(t *testing.T) {
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","identity":{"account_id":"acc_1","agent_id":"agt_dash","agent_name":"dash","realm_id":"rlm_1","realm_name":"default"},"primary_facts":[],"salient_memories":[],"plan_entitlements":{"schema_version":"witself.agent-entitlements.v1","state":"applied","source":"cell_applied_snapshot","enforced_plan_id":"standard","features":{"memory":false,"Memory":true,"facts":true,"secrets":false,"messaging":true,"collaboration":false,"agent_email_receive":true,"agent_email_send":false},"retention_days":{"transcript_retention_days":90,"message_retention_days":30,"Message_Retention_Days":999,"agent_email_retention_days":null},"State":"unmanaged","Source":"billing_provider","Enforced_Plan_ID":"team","Features":{"memory":true},"Retention_Days":{"message_retention_days":999}},"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`)
		}, nil)
		resp := authedGet(t, srv, cfg, "/api/self")
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, alias := range []string{`"Memory"`, `"Message_Retention_Days"`, `"State"`, `"Source"`, `"Enforced_Plan_ID"`, `"Features"`, `"Retention_Days"`} {
			if bytes.Contains(body, []byte(alias)) {
				t.Fatalf("case alias %s reached browser response: %s", alias, body)
			}
		}
		var envelope selfEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatal(err)
		}
		got := envelope.PlanEntitlements
		if got == nil || got.State != "applied" || got.EnforcedPlanID != "standard" ||
			got.Features == nil || got.Features.Memory ||
			got.RetentionDays == nil || got.RetentionDays.MessageRetentionDays == nil ||
			*got.RetentionDays.MessageRetentionDays != 30 {
			t.Fatalf("case aliases influenced exact projection: %#v", got)
		}
	})

	t.Run("case alias cannot invent the optional self block", func(t *testing.T) {
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","identity":{"account_id":"acc_1","agent_id":"agt_dash","agent_name":"dash","realm_id":"rlm_1","realm_name":"default"},"primary_facts":[],"salient_memories":[],"Plan_Entitlements":{"schema_version":"witself.agent-entitlements.v1","state":"applied","source":"cell_applied_snapshot","enforced_plan_id":"team","features":{"memory":true,"facts":true,"secrets":true,"messaging":true,"collaboration":true,"agent_email_receive":true,"agent_email_send":true},"retention_days":{"transcript_retention_days":90,"message_retention_days":90,"agent_email_retention_days":90}},"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`)
		}, nil)
		resp := authedGet(t, srv, cfg, "/api/self")
		defer func() { _ = resp.Body.Close() }()
		var root map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
			t.Fatal(err)
		}
		if _, exists := root["plan_entitlements"]; exists {
			t.Fatal("case-folded alias invented the optional browser entitlement block")
		}
	})
}

func TestPlanEntitlementsAddNoBrowserRouteOrAction(t *testing.T) {
	backendCalls := 0
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		selfBackend(t)(w, r)
	}, nil)
	for _, path := range []string{"/api/plan", "/api/entitlements", "/api/billing", "/api/subscription"} {
		resp := authedGet(t, srv, cfg, path)
		if resp.StatusCode != http.StatusNotFound {
			_ = resp.Body.Close()
			t.Fatalf("GET %s = %d, want 404", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if backendCalls != 0 {
		t.Fatalf("unknown plan/action routes reached cell %d times", backendCalls)
	}
}

func assertDashboardJSONKeys(t *testing.T, object map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(object) != len(want) {
		t.Fatalf("keys = %v, want exactly %v", object, want)
	}
	for _, key := range want {
		if _, ok := object[key]; !ok {
			t.Fatalf("keys = %v, missing %q", object, key)
		}
	}
}

func TestAvatarServesOnlyCanonicalHashVerifiedSVG(t *testing.T) {
	canonical, err := avatar.GeneratePlaceholderSVG(testIdentity.AgentID, testIdentity.AgentName)
	if err != nil {
		t.Fatalf("placeholder: %v", err)
	}
	sum := sha256.Sum256(canonical)
	goodHash := hex.EncodeToString(sum[:])

	serveAvatar := func(view *client.AvatarView) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method+" "+r.URL.Path != "GET /v1/self/avatar" {
				t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			}
			writeTestJSON(t, w, map[string]any{"avatar": view})
		}
	}
	version := func(svg, hash string) *client.AvatarView {
		return &client.AvatarView{Active: &client.AvatarVersion{SVG: svg, SVGSHA256: hash}}
	}

	t.Run("canonical payload served", func(t *testing.T) {
		srv, cfg := newDashboard(t, serveAvatar(version(string(canonical), goodHash)), nil)
		resp := authedGet(t, srv, cfg, "/api/avatar.svg")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("got %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); got != "image/svg+xml" {
			t.Fatalf("Content-Type = %q", got)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(body) != string(canonical) {
			t.Fatal("served bytes are not the sanitized canonical payload")
		}
	})

	t.Run("non-canonical payload rejected", func(t *testing.T) {
		mutated := strings.Replace(string(canonical), "<svg", "<!-- injected --><svg", 1)
		mutatedSum := sha256.Sum256([]byte(mutated))
		srv, cfg := newDashboard(t, serveAvatar(version(mutated, hex.EncodeToString(mutatedSum[:]))), nil)
		resp := authedGet(t, srv, cfg, "/api/avatar.svg")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("got %d, want 502", resp.StatusCode)
		}
	})

	t.Run("unsafe payload rejected", func(t *testing.T) {
		unsafe := `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`
		unsafeSum := sha256.Sum256([]byte(unsafe))
		srv, cfg := newDashboard(t, serveAvatar(version(unsafe, hex.EncodeToString(unsafeSum[:]))), nil)
		resp := authedGet(t, srv, cfg, "/api/avatar.svg")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("got %d, want 502", resp.StatusCode)
		}
	})

	t.Run("hash mismatch rejected", func(t *testing.T) {
		wrong := sha256.Sum256([]byte("something else"))
		srv, cfg := newDashboard(t, serveAvatar(version(string(canonical), hex.EncodeToString(wrong[:]))), nil)
		resp := authedGet(t, srv, cfg, "/api/avatar.svg")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("got %d, want 502", resp.StatusCode)
		}
	})

	t.Run("missing active payload is 404", func(t *testing.T) {
		srv, cfg := newDashboard(t, serveAvatar(&client.AvatarView{}), nil)
		resp := authedGet(t, srv, cfg, "/api/avatar.svg")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("got %d, want 404", resp.StatusCode)
		}
	})
}

func TestTranscriptProxyUsesObservationalReads(t *testing.T) {
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/transcripts" && r.Method == http.MethodGet:
			writeTestJSON(t, w, map[string]any{"transcripts": []client.Transcript{{ID: "tr_1"}}})
		case r.URL.Path == "/v1/transcripts/tr_1" && r.Method == http.MethodGet:
			query := r.URL.Query()
			if query.Get("observational") != "true" {
				t.Errorf("observational = %q, want true", query.Get("observational"))
			}
			if query.Get("after_sequence") != "5" || query.Get("limit") != "10" {
				t.Errorf("unexpected query %s", r.URL.RawQuery)
			}
			writeTestJSON(t, w, client.TranscriptDetail{Transcript: client.Transcript{ID: "tr_1"}})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}, nil)

	list := authedGet(t, srv, cfg, "/api/transcripts")
	defer func() { _ = list.Body.Close() }()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list: got %d", list.StatusCode)
	}

	page := authedGet(t, srv, cfg, "/api/transcripts/tr_1?after_sequence=5&limit=10")
	defer func() { _ = page.Body.Close() }()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("page: got %d", page.StatusCode)
	}

	bad := authedGet(t, srv, cfg, "/api/transcripts/tr_1?after_sequence=nope")
	defer func() { _ = bad.Body.Close() }()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid after_sequence: got %d, want 400", bad.StatusCode)
	}
}

func TestMemoriesProxyNeverRequestsSensitiveValues(t *testing.T) {
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
		}
		if r.URL.Query().Has("include_sensitive") {
			t.Errorf("include_sensitive must never be sent, got query %s", r.URL.RawQuery)
		}
		switch r.URL.Path {
		case "/v1/memories":
			query := r.URL.Query()
			if query.Get("state") != "active" || query.Get("kind") != "note" ||
				query.Get("limit") != "5" || query.Get("cursor") != "c7" {
				t.Errorf("unexpected query %s", r.URL.RawQuery)
			}
			if tags := query["tag"]; len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
				t.Errorf("tags = %v", query["tag"])
			}
			writeTestJSON(t, w, client.MemoryPage{Items: []client.Memory{{ID: "mem_1"}}})
		case "/v1/memories/mem_1":
			writeTestJSON(t, w, map[string]any{"memory": client.Memory{ID: "mem_1"}})
		case "/v1/memories/mem_1/history":
			writeTestJSON(t, w, client.MemoryHistoryPage{})
		default:
			t.Errorf("unexpected backend path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}, nil)

	for _, path := range []string{
		"/api/memories?limit=5&state=active&kind=note&tag=a&tag=b&cursor=c7",
		"/api/memories/mem_1",
		"/api/memories/mem_1/history?limit=50",
	} {
		resp := authedGet(t, srv, cfg, path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: got %d, want 200", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

func TestMessagesProxyOnlyTouchesPassiveList(t *testing.T) {
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ":") {
			t.Errorf("dashboard touched mutating message action %s %s (never :read/:listen/:claim)", r.Method, r.URL.Path)
		}
		if r.Method+" "+r.URL.Path != "GET /v1/messages" {
			t.Errorf("dashboard must only use GET /v1/messages, got %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if query := r.URL.Query(); query.Get("direction") != "inbox" || query.Get("limit") != "3" {
			t.Errorf("unexpected query %s", r.URL.RawQuery)
		}
		// A misbehaving cell that leaks bodies from the passive list: the
		// proxy must strip them rather than trust server-side redaction.
		writeTestJSON(t, w, client.MessagePage{Messages: []client.Message{{
			ID:      "msg_1",
			Subject: "greetings",
			Body:    "leaked-body-text",
			Payload: json.RawMessage(`{"leaked":"payload"}`),
		}}})
	}, nil)
	resp := authedGet(t, srv, cfg, "/api/messages?direction=inbox&limit=3")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(raw), "msg_1") || !strings.Contains(string(raw), "greetings") {
		t.Fatalf("metadata missing from proxied page: %s", raw)
	}
	if strings.Contains(string(raw), "leaked") {
		t.Fatalf("message body/payload reached the browser: %s", raw)
	}
}

// TestAgentEmailProxyUsesOnlyPassiveGETsAndAllowListsMetadata proves the
// dashboard's browser boundary is stricter than the public list wire shape:
// even a misbehaving cell cannot leak content, raw MIME/header data,
// attachment detail, row identifiers, or a processing claim fence.
func TestAgentEmailProxyUsesOnlyPassiveGETsAndAllowListsMetadata(t *testing.T) {
	now := time.Date(2026, 7, 21, 20, 1, 2, 0, time.UTC)
	lease := now.Add(time.Minute)
	backend := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet ||
			(strings.Contains(r.URL.Path, ":") && r.URL.Path != "/v1/email:status") {
			t.Errorf("email dashboard touched non-passive route %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/v1/email/address":
			writeTestJSON(t, w, map[string]any{"address": client.AgentEmailAddress{
				ID: "private-address-id", MailboxID: "private-mailbox-id", OwnerAgentID: "private-owner-id",
				Address: "dash@agents.example", Domain: "private-domain", LocalPart: "private-local-part",
				AgentSegment: "private-agent-segment", RealmLabel: "private-realm-label",
				ProvisioningKind: "pilot", ReceiveState: "disabled",
				AgentReceiveState: "enabled", RealmReceiveState: "disabled",
				CreatedAt: now, UpdatedAt: now,
			}})
		case "/v1/email:status":
			if r.URL.RawQuery != "" {
				t.Errorf("unexpected email status query %s", r.URL.RawQuery)
			}
			writeTestJSON(t, w, map[string]any{
				"schema_version":    "witself.v0",
				"maximum_raw_bytes": 25 * 1024 * 1024,
				"attachment_capacity": map[string]any{
					"used": 4096, "max": 8192, "remaining": 4096,
					"unlimited": false, "near_limit": false,
					"at_limit": false, "over_limit": false,
					"private_account_id": "leaked-status-account",
				},
				"private_policy_revision": "leaked-policy-revision",
			})
		case "/v1/email":
			query := r.URL.Query()
			if query.Get("unread") != "true" || query.Get("unacked") != "true" ||
				query.Get("limit") != "7" || query.Get("cursor") != "" {
				t.Errorf("unexpected email query %s", r.URL.RawQuery)
			}
			writeTestJSON(t, w, client.AgentEmailPage{
				Messages: []client.AgentEmailMessage{{
					ID: "claimable-message-id", MailboxID: "private-mailbox-id", OwnerAgentID: "private-owner-id",
					AddressID: "private-address-id", Provider: "cloudflare", EnvelopeSender: "sender@example.net",
					EnvelopeRecipient: "private-recipient", AgentSegment: "private-agent-segment",
					RealmLabel: "private-realm-label", SubaddressTag: "private-subaddress",
					RawSizeBytes: 2048, ParseState: "parsed", HeaderFrom: "leaked-header-from",
					HeaderTo: "leaked-header-to", Subject: "safe subject", MIMEMessageID: "leaked-mime-id",
					MessageDate: &now, AttachmentCount: 2,
					AttachmentStorageBytes: 1536, RetainedAttachmentStorageBytes: 0,
					PayloadRetentionState: "omitted_capacity",
					SPFResult:             "pass", DKIMResult: "pass",
					DMARCResult: "pass", SpamVerdict: "none", SenderVerificationState: "unverified",
					PossibleDuplicate: true, PossibleDuplicateOfMessage: "leaked-duplicate-id",
					ReceivedAt: now, Folder: "inbox", DeliveredAt: now,
					ReadState: client.AgentEmailReadState{State: "unread"},
					Processing: client.AgentEmailProcessing{
						State: "claimed", Generation: 9, FailureCount: 2,
						ClaimID: "leaked-claim-id", LeaseExpiresAt: &lease,
					},
					Text: "leaked decoded body", TextKind: "plain",
				}},
				NextCursor: "cursor-2",
			})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	addressResp := authedGet(t, srv, cfg, "/api/email/address")
	addressRaw, err := io.ReadAll(addressResp.Body)
	_ = addressResp.Body.Close()
	if err != nil {
		t.Fatalf("read address: %v", err)
	}
	if addressResp.StatusCode != http.StatusOK || !strings.Contains(string(addressRaw), "dash@agents.example") ||
		!strings.Contains(string(addressRaw), `"receive_state":"disabled"`) ||
		!strings.Contains(string(addressRaw), `"agent_receive_state":"enabled"`) ||
		!strings.Contains(string(addressRaw), `"realm_receive_state":"disabled"`) {
		t.Fatalf("address projection = %d %s", addressResp.StatusCode, addressRaw)
	}
	for _, forbidden := range []string{"private-address-id", "private-mailbox-id", "private-owner-id", "private-domain", "private-local-part", "private-agent-segment", "private-realm-label"} {
		if strings.Contains(string(addressRaw), forbidden) {
			t.Fatalf("address projection leaked %q: %s", forbidden, addressRaw)
		}
	}

	statusResp := authedGet(t, srv, cfg, "/api/email/status")
	statusRaw, err := io.ReadAll(statusResp.Body)
	_ = statusResp.Body.Close()
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status got %d: %s", statusResp.StatusCode, statusRaw)
	}
	for _, want := range []string{
		`"maximum_raw_bytes":26214400`, `"used":4096`, `"max":8192`,
		`"remaining":4096`, `"unlimited":false`, `"near_limit":false`,
		`"at_limit":false`, `"over_limit":false`,
	} {
		if !strings.Contains(string(statusRaw), want) {
			t.Errorf("safe status %q missing: %s", want, statusRaw)
		}
	}
	for _, forbidden := range []string{
		"schema_version", "leaked-status-account", "private_account_id",
		"leaked-policy-revision", "private_policy_revision",
	} {
		if strings.Contains(string(statusRaw), forbidden) {
			t.Fatalf("email status projection leaked %q: %s", forbidden, statusRaw)
		}
	}

	listResp := authedGet(t, srv, cfg, "/api/email?unread=true&unacked=true&limit=7")
	listRaw, err := io.ReadAll(listResp.Body)
	_ = listResp.Body.Close()
	if err != nil {
		t.Fatalf("read list: %v", err)
	}
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list got %d: %s", listResp.StatusCode, listRaw)
	}
	for _, want := range []string{
		"safe subject", "sender@example.net", `"attachment_count":2`,
		`"attachment_storage_bytes":1536`, `"retained_attachment_storage_bytes":0`,
		`"payload_retention_state":"omitted_capacity"`,
		`"possible_duplicate":true`, `"failure_count":2`,
	} {
		if !strings.Contains(string(listRaw), want) {
			t.Errorf("safe metadata %q missing: %s", want, listRaw)
		}
	}
	for _, forbidden := range []string{
		"claimable-message-id", "private-mailbox-id", "private-owner-id", "private-address-id",
		"private-recipient", "private-agent-segment", "private-realm-label", "private-subaddress",
		"leaked-header-from", "leaked-header-to", "leaked-mime-id", "leaked-duplicate-id",
		"leaked-claim-id", "leaked decoded body", `"generation"`, `"lease_expires_at"`, `"text_kind"`,
		"cursor-2", `"next_cursor"`,
	} {
		if strings.Contains(string(listRaw), forbidden) {
			t.Fatalf("email projection leaked %q: %s", forbidden, listRaw)
		}
	}
}

func TestAgentEmailProxyValidatesFilters(t *testing.T) {
	srv, cfg := newDashboard(t, selfBackend(t), nil)
	for _, path := range []string{
		"/api/email?unread=nope",
		"/api/email?unacked=nope",
		"/api/email?limit=-1",
		"/api/email?limit=0",
		"/api/email?limit=101",
		"/api/email?cursor=1700000000000000000:emsg_aaaaaaaaaaaaaaaa",
	} {
		resp := authedGet(t, srv, cfg, path)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

func TestAgentEmailProjectionIsHardBounded(t *testing.T) {
	messages := make([]client.AgentEmailMessage, sseEmailPageLimit+1)
	for i := range messages {
		messages[i].Subject = strconv.Itoa(i)
	}
	got := sanitizeAgentEmails(messages)
	if len(got) != sseEmailPageLimit {
		t.Fatalf("sanitized messages = %d, want %d", len(got), sseEmailPageLimit)
	}
	if got[0].Subject != "0" || got[len(got)-1].Subject != strconv.Itoa(sseEmailPageLimit-1) {
		t.Fatalf("sanitizer did not retain the newest bounded prefix: first=%q last=%q", got[0].Subject, got[len(got)-1].Subject)
	}
}

func TestAgentEmailProxyRendersAvailabilityStates(t *testing.T) {
	t.Run("cell unavailable", func(t *testing.T) {
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}, nil)
		for _, path := range []string{"/api/email/address", "/api/email/status", "/api/email"} {
			resp := authedGet(t, srv, cfg, path)
			if resp.StatusCode != http.StatusNotImplemented {
				t.Errorf("%s: got %d, want 501", path, resp.StatusCode)
			}
			_ = resp.Body.Close()
		}
	})

	t.Run("agent not enrolled", func(t *testing.T) {
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSONError(w, http.StatusForbidden, "agent email is not enabled for this agent")
		}, nil)
		for _, path := range []string{"/api/email/address", "/api/email/status", "/api/email"} {
			resp := authedGet(t, srv, cfg, path)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s: got %d, want 403", path, resp.StatusCode)
			}
			_ = resp.Body.Close()
		}
	})

	t.Run("legacy agent enrollment wording", func(t *testing.T) {
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSONError(w, http.StatusForbidden, "agent is not enrolled in the email pilot")
		}, nil)
		resp := authedGet(t, srv, cfg, "/api/email/status")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("legacy wording: got %d, want 403", resp.StatusCode)
		}
		_ = resp.Body.Close()
	})

	t.Run("account feature disabled", func(t *testing.T) {
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"code":           "feature_not_enabled",
				"feature":        "agent_email_receive",
				"error":          "Sorry, this feature is not enabled on this account.",
				"retryable":      false,
			})
		}, nil)
		for _, path := range []string{"/api/email/address", "/api/email/status", "/api/email"} {
			resp := authedGet(t, srv, cfg, path)
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusForbidden ||
				!strings.Contains(string(body), "inbound email is not enabled on this account") {
				t.Errorf("%s: got %d %s", path, resp.StatusCode, body)
			}
		}
	})

	t.Run("upstream errors are content free", func(t *testing.T) {
		const leaked = "private subject and emsg_aaaaaaaaaaaaaaaa"
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSONError(w, http.StatusInternalServerError, leaked)
		}, nil)
		for _, path := range []string{"/api/email/address", "/api/email/status", "/api/email"} {
			resp := authedGet(t, srv, cfg, path)
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), agentEmailUpstreamMessage) {
				t.Errorf("%s: got %d %s", path, resp.StatusCode, body)
			}
			if strings.Contains(string(body), leaked) || strings.Contains(string(body), "emsg_") {
				t.Fatalf("%s leaked upstream email error: %s", path, body)
			}
		}
	})
}

func TestAgentEmailSentProxyUsesOnlyPassiveGETAndAllowListsMetadata(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 15, 16, 0, time.UTC)
	providerStarted := now.Add(time.Second)
	accepted := now.Add(2 * time.Second)
	delivered := now.Add(3 * time.Second)
	backend := func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "GET /v1/email/sent" {
			t.Errorf("sent-email dashboard touched non-list route %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query()
		if query.Get("limit") != "7" || query.Get("cursor") != "" || query.Get("state") != "" || len(query) != 1 {
			t.Errorf("unexpected sent-email query %s", r.URL.RawQuery)
		}
		writeTestJSON(t, w, map[string]any{
			"messages": []map[string]any{{
				"id": "leaked-sent-message-id", "account_id": "leaked-account-id",
				"realm_id": "leaked-realm-id", "owner_agent_id": "leaked-owner-id",
				"from": "dash@witmail.net", "reply_to": "reply@witmail.net",
				"to": "person@example.net", "subject": "safe sent subject",
				"state": "delivered", "provider_state": "delivered",
				"provider": "leaked-provider-name", "provider_message_id": "leaked-provider-message-id",
				"error_code": "provider_timeout", "request_kind": "send", "attempt_count": 2,
				"reply_to_inbound_message_id": "leaked-inbound-reply-target",
				"thread_key":                  "leaked-thread-key", "text": "leaked submitted body",
				"future_private_field": "leaked-future-field",
				"queued_at":            now, "created_at": now, "updated_at": delivered,
				"provider_started_at": providerStarted, "accepted_at": accepted, "delivered_at": delivered,
			}},
			"next_cursor": "leaked-sent-cursor",
		})
	}
	srv, cfg := newDashboard(t, backend, nil)

	resp := authedGet(t, srv, cfg, "/api/email/sent?limit=7")
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read sent list: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sent list got %d: %s", resp.StatusCode, raw)
	}
	for _, want := range []string{
		`"available":true`, `"from":"dash@witmail.net"`, `"reply_to":"reply@witmail.net"`,
		`"to":"person@example.net"`, `"subject":"safe sent subject"`, `"state":"delivered"`,
		`"provider_state":"delivered"`, `"error_code":"provider_timeout"`,
		`"request_kind":"send"`, `"attempt_count":2`,
		`"provider_started_at":"2026-08-17T14:15:17Z"`, `"accepted_at":"2026-08-17T14:15:18Z"`,
		`"delivered_at":"2026-08-17T14:15:19Z"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("safe sent metadata %q missing: %s", want, raw)
		}
	}
	for _, forbidden := range []string{
		"leaked-sent-message-id", "leaked-account-id", "leaked-realm-id", "leaked-owner-id",
		"leaked-provider-name", "leaked-provider-message-id", "leaked-inbound-reply-target",
		"leaked-thread-key", "leaked submitted body", "leaked-future-field", "leaked-sent-cursor",
		`"id"`, `"account_id"`, `"realm_id"`, `"owner_agent_id"`, `"provider"`,
		`"reply_to_inbound_message_id"`, `"thread_key"`, `"text"`, `"next_cursor"`,
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("sent-email projection leaked %q: %s", forbidden, raw)
		}
	}
}

func TestAgentEmailSentProxyValidatesAndBoundsQuery(t *testing.T) {
	backendCalls := 0
	backend := func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		if r.Method+" "+r.URL.Path != "GET /v1/email/sent" || r.URL.Query().Get("limit") != "100" || len(r.URL.Query()) != 1 {
			t.Errorf("default sent-email query = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		writeTestJSON(t, w, client.AgentEmailOutboundPage{})
	}
	srv, cfg := newDashboard(t, backend, nil)
	for _, path := range []string{
		"/api/email/sent?limit=nope", "/api/email/sent?limit=-1", "/api/email/sent?limit=0",
		"/api/email/sent?limit=101", "/api/email/sent?cursor=1700000000000000000:esnd_leak",
		"/api/email/sent?state=queued", "/api/email/sent?extra=1",
	} {
		resp := authedGet(t, srv, cfg, path)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if backendCalls != 0 {
		t.Fatalf("invalid sent-email queries reached backend %d times", backendCalls)
	}
	resp := authedGet(t, srv, cfg, "/api/email/sent")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || backendCalls != 1 {
		t.Fatalf("default sent-email page = %d calls=%d", resp.StatusCode, backendCalls)
	}
}

func TestAgentEmailSentProjectionIsHardBounded(t *testing.T) {
	messages := make([]client.AgentEmailOutboundMessage, sseEmailSentPageLimit+1)
	for i := range messages {
		messages[i].Subject = strconv.Itoa(i)
	}
	got := sanitizeAgentEmailSentMessages(messages)
	if len(got) != sseEmailSentPageLimit {
		t.Fatalf("sanitized sent messages = %d, want %d", len(got), sseEmailSentPageLimit)
	}
	if got[0].Subject != "0" || got[len(got)-1].Subject != strconv.Itoa(sseEmailSentPageLimit-1) {
		t.Fatalf("sent sanitizer did not retain bounded prefix: first=%q last=%q", got[0].Subject, got[len(got)-1].Subject)
	}
}

func TestAgentEmailSentProxyRendersAvailabilityStates(t *testing.T) {
	t.Run("pre-feature cell", func(t *testing.T) {
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, nil)
		resp := authedGet(t, srv, cfg, "/api/email/sent")
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), "cell does not serve outbound agent email") {
			t.Fatalf("pre-feature sent email = %d %s", resp.StatusCode, body)
		}
	})

	t.Run("account feature disabled", func(t *testing.T) {
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0", "code": "feature_not_enabled",
				"feature": "agent_email_send", "error": "Sorry, this feature is not enabled on this account.",
				"retryable": false,
			})
		}, nil)
		resp := authedGet(t, srv, cfg, "/api/email/sent")
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "outbound email is not enabled on this account") {
			t.Fatalf("disabled sent email = %d %s", resp.StatusCode, body)
		}
	})

	t.Run("upstream errors are content free", func(t *testing.T) {
		const leaked = "private recipient, subject, and esnd_aaaaaaaaaaaaaaaa"
		srv, cfg := newDashboard(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSONError(w, http.StatusInternalServerError, leaked)
		}, nil)
		resp := authedGet(t, srv, cfg, "/api/email/sent")
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), agentEmailSentUpstreamMessage) {
			t.Fatalf("sent upstream error = %d %s", resp.StatusCode, body)
		}
		if strings.Contains(string(body), leaked) || strings.Contains(string(body), "esnd_") {
			t.Fatalf("sent-email upstream error leaked: %s", body)
		}
	})
}

func TestEventsStreamEmitsSelfAndTranscript(t *testing.T) {
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "/v1/transcripts/tr_1":
			if r.URL.Query().Get("observational") != "true" {
				t.Errorf("transcript poll must be observational, got %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("after_sequence") == "" {
				writeTestJSON(t, w, client.TranscriptDetail{
					Transcript: client.Transcript{ID: "tr_1"},
					Entries: []client.TranscriptEntry{
						{Sequence: 1, Role: "user", Body: "hello"},
						{Sequence: 2, Role: "assistant", Body: "hi"},
					},
				})
				return
			}
			writeTestJSON(t, w, client.TranscriptDetail{Transcript: client.Transcript{ID: "tr_1"}})
		default:
			t.Errorf("unexpected backend path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?transcript=tr_1").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}

	sawSelf, sawTranscript := false, false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && (!sawSelf || !sawTranscript) {
		line := scanner.Text()
		if line == "event: self" {
			sawSelf = true
		}
		if line == "event: transcript" {
			sawTranscript = true
		}
	}
	if !sawSelf || !sawTranscript {
		t.Fatalf("stream ended early: self=%v transcript=%v (%v)", sawSelf, sawTranscript, scanner.Err())
	}
	cancel() // disconnect; srv.Close in cleanup hangs if the handler leaks
}

func TestEventsStreamRefreshesCellAppliedPlanEntitlements(t *testing.T) {
	var mu sync.Mutex
	reads := 0
	backend := func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "GET /v1/self" {
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("include_plan_entitlements") != "true" {
			t.Errorf("self poll omitted plan entitlement opt-in: %s", r.URL.RawQuery)
		}
		mu.Lock()
		reads++
		plan := "standard"
		if reads > 1 {
			plan = "team"
		}
		mu.Unlock()
		transcriptDays := int64(90)
		digest := testSelfDigest()
		digest.PlanEntitlements = &client.SelfAgentEntitlements{
			SchemaVersion: "witself.agent-entitlements.v1", State: "applied", Source: "cell_applied_snapshot",
			EnforcedPlanID: plan,
			Features:       &client.SelfAgentEntitlementFeatures{Memory: true, Facts: true},
			RetentionDays: &client.SelfAgentRetentionDays{
				TranscriptRetentionDays: &transcriptDays,
			},
		}
		writeTestJSON(t, w, digest)
	}
	srv, cfg := newDashboard(t, backend, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	seen := map[string]bool{}
	event := ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && len(seen) < 2 {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
			continue
		}
		if event != "self" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		var envelope selfEnvelope
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &envelope); err != nil {
			t.Fatalf("decode self event: %v", err)
		}
		if envelope.PlanEntitlements == nil || envelope.PlanEntitlements.State != "applied" ||
			envelope.PlanEntitlements.Source != "cell_applied_snapshot" {
			t.Fatalf("self entitlement event = %#v", envelope.PlanEntitlements)
		}
		seen[envelope.PlanEntitlements.EnforcedPlanID] = true
		event = ""
	}
	cancel()
	if !seen["standard"] || !seen["team"] {
		t.Fatalf("SSE plan transitions = %v, scanner error = %v", seen, scanner.Err())
	}
}

// TestEventsStreamEmitsMessagesFromPassiveList proves the opt-in messages
// tick polls only the passive metadata-only mailbox list — one GET
// /v1/messages page per direction, never :read, :listen, or claim — and
// emits both pages as one "messages" event.
func TestEventsStreamEmitsMessagesFromPassiveList(t *testing.T) {
	var mu sync.Mutex
	directions := map[string]bool{}
	backend := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ":") {
			t.Errorf("messages tick touched mutating action %s %s (never :read/:listen/:claim)", r.Method, r.URL.Path)
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "GET /v1/messages":
			query := r.URL.Query()
			direction := query.Get("direction")
			if direction != "inbox" && direction != "outbox" {
				t.Errorf("direction = %q, want inbox or outbox", direction)
			}
			if query.Get("limit") != strconv.Itoa(sseMessagePageLimit) {
				t.Errorf("limit = %q, want %d", query.Get("limit"), sseMessagePageLimit)
			}
			mu.Lock()
			directions[direction] = true
			mu.Unlock()
			if direction == "inbox" {
				writeTestJSON(t, w, client.MessagePage{Messages: []client.Message{{
					ID:        "msg_in",
					From:      client.MessageAgent{Kind: "agent", AgentID: "agt_peer", AgentName: "peer"},
					To:        client.MessageRecipient{Kind: "agent", AgentID: testIdentity.AgentID},
					Body:      "leaked-body-text",
					ReadState: client.MessageReadState{State: "unread"},
				}}})
				return
			}
			writeTestJSON(t, w, client.MessagePage{Messages: []client.Message{{ID: "msg_out"}}})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?messages=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	sawEvent, sawInbox, sawOutbox := false, false, false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && (!sawEvent || !sawInbox || !sawOutbox) {
		line := scanner.Text()
		if line == "event: messages" {
			sawEvent = true
		}
		if strings.Contains(line, "leaked-body-text") {
			t.Fatalf("message body reached the SSE stream: %q", line)
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "msg_in") {
			sawInbox = true
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "msg_out") {
			sawOutbox = true
		}
	}
	if !sawEvent || !sawInbox || !sawOutbox {
		t.Fatalf("stream ended early: event=%v inbox=%v outbox=%v (%v)", sawEvent, sawInbox, sawOutbox, scanner.Err())
	}
	mu.Lock()
	defer mu.Unlock()
	if !directions["inbox"] || !directions["outbox"] {
		t.Fatalf("messages tick polled directions %v, want both inbox and outbox", directions)
	}
	cancel()
}

// TestEventsStreamEmitsAgentEmailFromPassiveList proves live email refreshes
// use only the filtered GET list and apply the same browser allow-list as the
// direct proxy. In particular, the stream never uses :listen or a lifecycle
// action and never carries an id that could target one.
func TestEventsStreamEmitsAgentEmailFromPassiveList(t *testing.T) {
	now := time.Date(2026, 7, 21, 20, 2, 3, 0, time.UTC)
	lease := now.Add(time.Minute)
	var mu sync.Mutex
	emailCalls := 0
	statusCalls := 0
	backend := func(w http.ResponseWriter, r *http.Request) {
		if (strings.Contains(r.URL.Path, ":") && r.URL.Path != "/v1/email:status") ||
			r.Method != http.MethodGet {
			t.Errorf("email tick touched non-passive route %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "/v1/email":
			query := r.URL.Query()
			if query.Get("unread") != "true" || query.Get("unacked") != "true" ||
				query.Get("limit") != strconv.Itoa(sseEmailPageLimit) {
				t.Errorf("unexpected email tick query %s", r.URL.RawQuery)
			}
			mu.Lock()
			emailCalls++
			mu.Unlock()
			writeTestJSON(t, w, client.AgentEmailPage{Messages: []client.AgentEmailMessage{{
				ID: "leaked-message-id", EnvelopeSender: "sender@example.net", Subject: "live subject",
				AttachmentCount: 1, SenderVerificationState: "unverified", ReceivedAt: now, DeliveredAt: now,
				ReadState: client.AgentEmailReadState{State: "unread"},
				Processing: client.AgentEmailProcessing{
					State: "claimed", Generation: 4, ClaimID: "leaked-claim-id", LeaseExpiresAt: &lease,
				},
				Text: "leaked live body", MIMEMessageID: "leaked-mime-id",
			}}})
		case "/v1/email:status":
			mu.Lock()
			statusCalls++
			mu.Unlock()
			maximum := int64(8192)
			remaining := int64(4096)
			writeTestJSON(t, w, client.AgentEmailStorageStatus{
				SchemaVersion:   "witself.v0",
				MaximumRawBytes: 25 * 1024 * 1024,
				AttachmentCapacity: client.MemoryLimitStatus{
					Used: 4096, Max: &maximum, Remaining: &remaining,
				},
			})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?email=true&email_unread=true&email_unacked=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	sawEvent, sawMetadata, sawCapacity := false, false, false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && (!sawEvent || !sawMetadata || !sawCapacity) {
		line := scanner.Text()
		if line == "event: email" {
			sawEvent = true
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "live subject") {
			sawMetadata = true
			sawCapacity = strings.Contains(line, `"status":{"maximum_raw_bytes":26214400`) &&
				strings.Contains(line, `"attachment_capacity":{"used":4096,"max":8192`)
			for _, forbidden := range []string{"leaked-message-id", "leaked-claim-id", "leaked live body", "leaked-mime-id", `"generation"`, `"lease_expires_at"`} {
				if strings.Contains(line, forbidden) {
					t.Fatalf("email SSE leaked %q: %s", forbidden, line)
				}
			}
		}
	}
	if !sawEvent || !sawMetadata || !sawCapacity {
		t.Fatalf("stream ended early: event=%v metadata=%v capacity=%v (%v)",
			sawEvent, sawMetadata, sawCapacity, scanner.Err())
	}
	mu.Lock()
	if emailCalls == 0 {
		t.Error("email list was never polled")
	}
	if statusCalls == 0 {
		t.Error("email storage status was never polled")
	}
	mu.Unlock()
	cancel()
}

func TestEventsStreamRefreshesAgentEmailCapacityWhenOnlyPolicyChanges(t *testing.T) {
	var mu sync.Mutex
	statusCalls := 0
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "/v1/email":
			writeTestJSON(t, w, client.AgentEmailPage{Messages: []client.AgentEmailMessage{}})
		case "/v1/email:status":
			mu.Lock()
			statusCalls++
			call := statusCalls
			mu.Unlock()
			maximum := int64(4096)
			if call > 1 {
				maximum = 8192
			}
			remaining := maximum - 1024
			writeTestJSON(t, w, client.AgentEmailStorageStatus{
				SchemaVersion:   "witself.v0",
				MaximumRawBytes: 25 * 1024 * 1024,
				AttachmentCapacity: client.MemoryLimitStatus{
					Used: 1024, Max: &maximum, Remaining: &remaining,
				},
			})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?email=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var maxima []int64
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() && len(maxima) < 2 {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"status"`) {
			continue
		}
		var frame struct {
			Status struct {
				AttachmentCapacity struct {
					Max *int64 `json:"max"`
				} `json:"attachment_capacity"`
			} `json:"status"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatalf("decode email frame: %v", err)
		}
		if frame.Status.AttachmentCapacity.Max == nil {
			t.Fatalf("email frame omitted finite maximum: %s", line)
		}
		maxima = append(maxima, *frame.Status.AttachmentCapacity.Max)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan events: %v", err)
	}
	if !reflect.DeepEqual(maxima, []int64{4096, 8192}) {
		t.Fatalf("capacity maxima = %v, want policy update without a message change", maxima)
	}
	cancel()
}

func TestEventsStreamEmitsSettledEmailUnavailableState(t *testing.T) {
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "/v1/email", "/v1/email:status":
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?email=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	sawEmail, sawUnavailable, sawEmailDegraded := false, false, false
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() && !sawUnavailable {
		line := scanner.Text()
		if line == "event: email" {
			sawEmail = true
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"source":"email"`) {
			sawEmailDegraded = true
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"available":false`) &&
			strings.Contains(line, `"reason":"pre_feature"`) {
			sawUnavailable = true
		}
	}
	if !sawEmail || !sawUnavailable || sawEmailDegraded {
		t.Fatalf("settled availability event: email=%v unavailable=%v degraded=%v (%v)",
			sawEmail, sawUnavailable, sawEmailDegraded, scanner.Err())
	}
	cancel()
}

func TestEventsStreamRedactsAgentEmailUpstreamError(t *testing.T) {
	const leaked = "private subject and emsg_aaaaaaaaaaaaaaaa"
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "/v1/email":
			writeJSONError(w, http.StatusInternalServerError, leaked)
		case "/v1/email:status":
			writeTestJSON(t, w, client.AgentEmailStorageStatus{
				SchemaVersion: "witself.v0", AttachmentCapacity: client.MemoryLimitStatus{Unlimited: true},
			})
		default:
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?email=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, leaked) || strings.Contains(line, "emsg_") {
			t.Fatalf("email SSE leaked upstream error: %s", line)
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"source":"email"`) {
			if !strings.Contains(line, agentEmailUpstreamMessage) {
				t.Fatalf("email SSE did not use fixed error: %s", line)
			}
			cancel()
			return
		}
	}
	t.Fatalf("email upstream event not observed: %v", scanner.Err())
}

func TestEventsStreamEmitsAgentEmailSentIndependently(t *testing.T) {
	now := time.Date(2026, 8, 17, 16, 17, 18, 0, time.UTC)
	var mu sync.Mutex
	receiveCalls, sentCalls := 0, 0
	backend := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("email stream touched mutating route %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "/v1/email":
			mu.Lock()
			receiveCalls++
			mu.Unlock()
			writeTestJSON(t, w, client.AgentEmailPage{Messages: []client.AgentEmailMessage{{
				EnvelopeSender: "sender@example.net", Subject: "inbound live subject",
				SenderVerificationState: "unverified", ReceivedAt: now, DeliveredAt: now,
			}}})
		case "/v1/email:status":
			writeTestJSON(t, w, client.AgentEmailStorageStatus{
				SchemaVersion: "witself.v0", MaximumRawBytes: 25 * 1024 * 1024,
				AttachmentCapacity: client.MemoryLimitStatus{Unlimited: true},
			})
		case "/v1/email/sent":
			if r.URL.Query().Get("limit") != strconv.Itoa(sseEmailSentPageLimit) || len(r.URL.Query()) != 1 {
				t.Errorf("sent-email tick query = %s", r.URL.RawQuery)
			}
			mu.Lock()
			sentCalls++
			mu.Unlock()
			writeTestJSON(t, w, map[string]any{"messages": []map[string]any{{
				"id": "leaked-live-sent-id", "account_id": "leaked-live-account",
				"provider": "leaked-live-provider", "reply_to_inbound_message_id": "leaked-live-reply-target",
				"thread_key": "leaked-live-thread", "future_body": "leaked-live-body",
				"from": "dash@witmail.net", "reply_to": "reply@witmail.net",
				"to": "person@example.net", "subject": "outbound live subject",
				"state": "queued", "request_kind": "send", "attempt_count": 0,
				"queued_at": now, "created_at": now, "updated_at": now,
			}}})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?email=true&email_sent=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open email events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	sawReceive, sawSent := false, false
	lastEvent := ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && (!sawReceive || !sawSent) {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			lastEvent = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		switch lastEvent {
		case "email":
			if strings.Contains(line, "inbound live subject") {
				sawReceive = true
			}
		case "email_sent":
			if strings.Contains(line, "outbound live subject") {
				sawSent = true
				for _, forbidden := range []string{
					"leaked-live-sent-id", "leaked-live-account", "leaked-live-provider",
					"leaked-live-reply-target", "leaked-live-thread", "leaked-live-body",
				} {
					if strings.Contains(line, forbidden) {
						t.Fatalf("sent-email SSE leaked %q: %s", forbidden, line)
					}
				}
			}
		}
	}
	if !sawReceive || !sawSent {
		t.Fatalf("independent email events: receive=%v sent=%v (%v)", sawReceive, sawSent, scanner.Err())
	}
	mu.Lock()
	if receiveCalls == 0 || sentCalls == 0 {
		t.Errorf("email polls: receive=%d sent=%d", receiveCalls, sentCalls)
	}
	mu.Unlock()
	cancel()
}

func TestEventsStreamAgentEmailFetchesAreConcurrentAndBudgeted(t *testing.T) {
	started := make(chan string, 4)
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "/v1/email", "/v1/email:status":
			select {
			case started <- r.URL.Path:
			default:
			}
			<-r.Context().Done()
		case "/v1/email/sent":
			writeTestJSON(t, w, client.AgentEmailOutboundPage{Messages: []client.AgentEmailOutboundMessage{{
				Subject: "sent is not blocked by receive", State: "queued",
			}}})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, func(cfg *Config) {
		cfg.PollInterval = 150 * time.Millisecond
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?email=true&email_sent=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open email events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	startedPaths := map[string]bool{}
	for len(startedPaths) != 2 {
		select {
		case path := <-started:
			startedPaths[path] = true
		case <-time.After(time.Second):
			t.Fatalf("receive list and status did not start concurrently: %v", startedPaths)
		}
	}

	sawSent, sawReceiveBudget := false, false
	lastEvent := ""
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() && !sawReceiveBudget {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			lastEvent = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if lastEvent == "email_sent" && strings.Contains(line, "sent is not blocked by receive") {
			sawSent = true
		}
		if lastEvent == "upstream" && strings.Contains(line, `"source":"email"`) {
			if !sawSent {
				t.Fatalf("blocked receive withheld the ready sent frame: %s", line)
			}
			if !strings.Contains(line, agentEmailUpstreamMessage) || strings.Contains(line, "deadline exceeded") {
				t.Fatalf("receive budget error was not fixed and redacted: %s", line)
			}
			sawReceiveBudget = true
		}
	}
	if !sawSent || !sawReceiveBudget {
		t.Fatalf("bounded independent email tick: sent=%v receive_budget=%v (%v)",
			sawSent, sawReceiveBudget, scanner.Err())
	}
	cancel()
}

func TestEventsStreamAgentEmailDirectionsFailIndependently(t *testing.T) {
	const leaked = "private email subject and actionable-id"
	tests := []struct {
		name          string
		failedPath    string
		failedSource  string
		successEvent  string
		successMarker string
		fixedMessage  string
	}{
		{
			name: "receive failure does not suppress sent", failedPath: "/v1/email",
			failedSource: "email", successEvent: "email_sent", successMarker: "sent survives",
			fixedMessage: agentEmailUpstreamMessage,
		},
		{
			name: "sent failure does not suppress receive", failedPath: "/v1/email/sent",
			failedSource: "email_sent", successEvent: "email", successMarker: "receive survives",
			fixedMessage: agentEmailSentUpstreamMessage,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend := func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/self":
					writeTestJSON(t, w, testSelfDigest())
				case "/v1/email":
					if tc.failedPath == r.URL.Path {
						writeJSONError(w, http.StatusInternalServerError, leaked)
						return
					}
					writeTestJSON(t, w, client.AgentEmailPage{Messages: []client.AgentEmailMessage{{Subject: "receive survives"}}})
				case "/v1/email:status":
					writeTestJSON(t, w, client.AgentEmailStorageStatus{
						SchemaVersion: "witself.v0", AttachmentCapacity: client.MemoryLimitStatus{Unlimited: true},
					})
				case "/v1/email/sent":
					if tc.failedPath == r.URL.Path {
						writeJSONError(w, http.StatusInternalServerError, leaked)
						return
					}
					writeTestJSON(t, w, client.AgentEmailOutboundPage{Messages: []client.AgentEmailOutboundMessage{{Subject: "sent survives"}}})
				default:
					http.NotFound(w, r)
				}
			}
			srv, cfg := newDashboard(t, backend, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			req := authedRequest(t, srv, cfg, "/api/events?email=true&email_sent=true").WithContext(ctx)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("open email events: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			sawSuccess, sawFailure := false, false
			lastEvent := ""
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() && (!sawSuccess || !sawFailure) {
				line := scanner.Text()
				if strings.Contains(line, leaked) || strings.Contains(line, "actionable-id") {
					t.Fatalf("email direction error leaked: %s", line)
				}
				if strings.HasPrefix(line, "event: ") {
					lastEvent = strings.TrimPrefix(line, "event: ")
					continue
				}
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				if lastEvent == tc.successEvent && strings.Contains(line, tc.successMarker) {
					sawSuccess = true
				}
				if lastEvent == "upstream" && strings.Contains(line, `"source":"`+tc.failedSource+`"`) {
					if !strings.Contains(line, tc.fixedMessage) {
						t.Fatalf("email direction error was not fixed: %s", line)
					}
					sawFailure = true
				}
			}
			if !sawSuccess || !sawFailure {
				t.Fatalf("email direction isolation: success=%v failure=%v (%v)", sawSuccess, sawFailure, scanner.Err())
			}
			cancel()
		})
	}
}

func TestEventsStreamEmitsSettledAgentEmailSentAvailability(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		reply  func(http.ResponseWriter)
	}{
		{
			name: "feature disabled", reason: "feature_disabled",
			reply: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": "feature_not_enabled", "feature": "agent_email_send",
					"error": "Sorry, this feature is not enabled on this account.", "retryable": false,
				})
			},
		},
		{name: "pre-feature cell", reason: "pre_feature", reply: func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend := func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/self":
					writeTestJSON(t, w, testSelfDigest())
				case "/v1/email/sent":
					tc.reply(w)
				default:
					http.NotFound(w, r)
				}
			}
			srv, cfg := newDashboard(t, backend, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			req := authedRequest(t, srv, cfg, "/api/events?email_sent=true").WithContext(ctx)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("open sent-email events: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			sawEvent, sawUnavailable, sawDegraded := false, false, false
			lastEvent := ""
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() && !sawUnavailable {
				line := scanner.Text()
				if strings.HasPrefix(line, "event: ") {
					lastEvent = strings.TrimPrefix(line, "event: ")
					if lastEvent == "email_sent" {
						sawEvent = true
					}
					continue
				}
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				if lastEvent == "upstream" && strings.Contains(line, `"source":"email_sent"`) {
					sawDegraded = true
				}
				if lastEvent == "email_sent" && strings.Contains(line, `"available":false`) &&
					strings.Contains(line, `"reason":"`+tc.reason+`"`) {
					sawUnavailable = true
				}
			}
			if !sawEvent || !sawUnavailable || sawDegraded {
				t.Fatalf("sent availability: event=%v unavailable=%v degraded=%v (%v)",
					sawEvent, sawUnavailable, sawDegraded, scanner.Err())
			}
			cancel()
		})
	}
}

func TestEventsStreamRejectsInvalidMessagesFlag(t *testing.T) {
	srv, cfg := newDashboard(t, selfBackend(t), nil)
	for _, path := range []string{
		"/api/events?messages=nope", "/api/events?memories=nope", "/api/events?facts=nope", "/api/events?secrets=nope",
		"/api/events?email=nope", "/api/events?email_sent=nope", "/api/events?email=true&email_unread=nope", "/api/events?email=true&email_unacked=nope",
	} {
		resp := authedGet(t, srv, cfg, path)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// TestEventsStreamEmitsMemoriesFromRedactedList proves the opt-in memories
// tick polls the redacted-by-default broad list (never include_sensitive)
// and emits it as a "memories" event, so the memories surface live-updates
// like the others.
func TestEventsStreamEmitsMemoriesFromRedactedList(t *testing.T) {
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "GET /v1/memories":
			query := r.URL.Query()
			if query.Has("include_sensitive") {
				t.Errorf("include_sensitive must never be sent, got query %s", r.URL.RawQuery)
			}
			if query.Get("limit") != strconv.Itoa(sseMemoryPageLimit) {
				t.Errorf("limit = %q, want %d", query.Get("limit"), sseMemoryPageLimit)
			}
			writeTestJSON(t, w, client.MemoryPage{Items: []client.Memory{{ID: "mem_live"}}})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?memories=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	sawEvent, sawItem := false, false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && (!sawEvent || !sawItem) {
		line := scanner.Text()
		if line == "event: memories" {
			sawEvent = true
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "mem_live") {
			sawItem = true
		}
	}
	if !sawEvent || !sawItem {
		t.Fatalf("stream ended early: event=%v item=%v (%v)", sawEvent, sawItem, scanner.Err())
	}
	cancel()
}

// TestEventsStreamEmitsUpstreamStateChangesOnly proves a failing poll source
// surfaces as exactly one "upstream" error event when it starts failing and
// exactly one clearing event when it recovers — steady failure across ticks
// never spams — and that no frame leaks the bearer token.
func TestEventsStreamEmitsUpstreamStateChangesOnly(t *testing.T) {
	var mu sync.Mutex
	memoryPolls := 0
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "GET /v1/memories":
			mu.Lock()
			memoryPolls++
			failing := memoryPolls <= 3
			mu.Unlock()
			if failing {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"memories exploded"}`))
				return
			}
			writeTestJSON(t, w, client.MemoryPage{Items: []client.Memory{{ID: "mem_back"}}})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?memories=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	type upstreamFrame struct {
		Source  string `json:"source"`
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	var frames []upstreamFrame
	recovered, ticksAfterRecovery := false, 0
	lastEvent := ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && ticksAfterRecovery < 3 {
		line := scanner.Text()
		if strings.Contains(line, testBearer) {
			t.Fatalf("bearer token leaked into the SSE stream: %q", line)
		}
		if strings.HasPrefix(line, "event: ") {
			lastEvent = strings.TrimPrefix(line, "event: ")
			// Count post-recovery ticks by their self frames to prove the
			// steady recovered state stays silent too.
			if recovered && lastEvent == "self" {
				ticksAfterRecovery++
			}
			continue
		}
		if lastEvent != "upstream" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		var frame upstreamFrame
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatalf("decode upstream frame %q: %v", line, err)
		}
		frames = append(frames, frame)
		if frame.OK {
			recovered = true
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		t.Fatalf("scan events stream: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("upstream frames = %+v, want exactly one error and one recovery", frames)
	}
	if frames[0].Source != "memories" || frames[0].OK || frames[0].Message != "memories exploded" {
		t.Fatalf("error frame = %+v", frames[0])
	}
	if frames[1].Source != "memories" || !frames[1].OK || frames[1].Message != "" {
		t.Fatalf("recovery frame = %+v", frames[1])
	}
	mu.Lock()
	defer mu.Unlock()
	if memoryPolls < 4 {
		t.Fatalf("memory polls = %d, want at least 4 (three failing ticks, then recovery)", memoryPolls)
	}
	cancel()
}

// answerFactReadProbe replies to the one-time observational capability probe
// (client.ProbeObservationalFactReads) the way every cell that parses the
// parameter does — 400 before any read — and reports whether it handled the
// request. The probe is recognizable by its deliberately unparseable
// observational value.
func answerFactReadProbe(w http.ResponseWriter, r *http.Request) bool {
	raw := r.URL.Query().Get("observational")
	if raw == "" {
		return false
	}
	if _, err := strconv.ParseBool(raw); err == nil {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "observational must be true or false"})
	return true
}

// TestFactsProxyListsObservationalAndRedacts proves the facts list proxy is
// an observational read that never requests sensitive values, forwards the
// browser's filters, and — defense in depth, mirroring stripMessageBodies —
// zeroes any sensitive value a misbehaving cell leaks into the list.
func TestFactsProxyListsObservationalAndRedacts(t *testing.T) {
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "GET /v1/facts" {
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if answerFactReadProbe(w, r) {
			return
		}
		query := r.URL.Query()
		if query.Get("observational") != "true" {
			t.Errorf("observational = %q, want true", query.Get("observational"))
		}
		if query.Has("include_sensitive") {
			t.Errorf("include_sensitive must never be sent, got query %s", r.URL.RawQuery)
		}
		if query.Get("subject") != "self" || query.Get("predicate_prefix") != "identity" || query.Get("limit") != "25" {
			t.Errorf("unexpected query %s", r.URL.RawQuery)
		}
		// A misbehaving cell that leaks a sensitive value from the broad
		// list: the proxy must strip it rather than trust upstream redaction.
		writeTestJSON(t, w, map[string]any{"facts": []client.Fact{
			{ID: "fact_1", Subject: "self", Predicate: "identity/name", Value: json.RawMessage(`"Scott"`)},
			{ID: "fact_2", Subject: "self", Predicate: "identity/ssn", Sensitive: true,
				Value: json.RawMessage(`"leaked-fact-value"`), SourceRef: "leaked-source-ref"},
		}})
	}, nil)

	resp := authedGet(t, srv, cfg, "/api/facts?subject=self&predicate_prefix=identity&limit=25")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(raw), "fact_1") || !strings.Contains(string(raw), "Scott") {
		t.Fatalf("non-sensitive fact missing from proxied list: %s", raw)
	}
	if strings.Contains(string(raw), "leaked") {
		t.Fatalf("sensitive fact value/source ref reached the browser: %s", raw)
	}

	bad := authedGet(t, srv, cfg, "/api/facts?limit=nope")
	defer func() { _ = bad.Body.Close() }()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid limit: got %d, want 400", bad.StatusCode)
	}
}

// TestFactsProxyDoesNotDegradeOn501 proves the list proxy never falls back to
// the plain fact list on a cell that knows the observational parameter but
// has no observational hooks wired: unlike the self digest, the plain list
// records ranking-eligible search usage, so the 501 surfaces as a clear 501
// the UI renders instead of a silently perturbing degrade.
func TestFactsProxyDoesNotDegradeOn501(t *testing.T) {
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		if answerFactReadProbe(w, r) {
			return
		}
		if r.URL.Query().Get("observational") != "true" {
			t.Errorf("proxy fell back to the plain usage-recording list: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "observational fact reads are unavailable"})
	}, nil)
	resp := authedGet(t, srv, cfg, "/api/facts")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("got %d, want 501", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "cell does not support observational fact reads" {
		t.Fatalf("error = %q", body.Error)
	}
}

// TestFactsSurfacesRefuseCellsThatIgnoreObservational proves the capability
// probe closes the released-cell gap (v0.0.152-v0.0.168): those cells ignore
// the observational parameter entirely and silently run the plain
// usage-recording read path instead of answering 501, so the broad list, the
// SSE facts tick, and the history sensitivity probe must all refuse to read
// rather than perturb usage ranking on every render. The probe itself pairs
// an unparseable observational value with an unparseable limit, which such a
// cell rejects before any read, and its answer is memoized.
func TestFactsSurfacesRefuseCellsThatIgnoreObservational(t *testing.T) {
	var mu sync.Mutex
	var plainReads, probes int
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "GET /v1/facts/fact_1/history":
			writeTestJSON(t, w, map[string]any{"assertions": []client.FactAssertion{
				{ID: "fas_1", FactID: "fact_1", Value: json.RawMessage(`"history-value"`), SourceRef: "history-ref"},
			}})
		case "GET /v1/facts":
			// The v0.0.168 factsReadHandler: the observational parameter did
			// not exist, so it is ignored and only an invalid limit stops
			// the plain read.
			if raw := r.URL.Query().Get("limit"); raw != "" {
				if _, err := strconv.Atoi(raw); err != nil {
					mu.Lock()
					probes++
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "limit must be an integer"})
					return
				}
			}
			mu.Lock()
			plainReads++
			mu.Unlock()
			writeTestJSON(t, w, map[string]any{"facts": []client.Fact{{ID: "fact_1"}}})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	for range 2 {
		resp := authedGet(t, srv, cfg, "/api/facts")
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("list on an old cell: got %d, want 501", resp.StatusCode)
		}
		var body struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		_ = resp.Body.Close()
		if body.Error != "cell does not support observational fact reads" {
			t.Fatalf("error = %q", body.Error)
		}
	}

	hist := authedGet(t, srv, cfg, "/api/facts/fact_1/history?subject=self&predicate=identity%2Fname")
	defer func() { _ = hist.Body.Close() }()
	if hist.StatusCode != http.StatusOK {
		t.Fatalf("history on an old cell: got %d, want 200", hist.StatusCode)
	}
	raw, err := io.ReadAll(hist.Body)
	if err != nil {
		t.Fatalf("read history body: %v", err)
	}
	if !strings.Contains(string(raw), "fas_1") {
		t.Fatalf("assertion metadata missing: %s", raw)
	}
	if strings.Contains(string(raw), "history-value") || strings.Contains(string(raw), "history-ref") {
		t.Fatalf("history values must stay locked without a usage-free sensitivity probe: %s", raw)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?facts=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	selfFrames := 0
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && selfFrames < 3 {
		line := scanner.Text()
		if line == "event: facts" {
			t.Fatalf("old cell must get no facts tick, saw %q", line)
		}
		if line == "event: self" {
			selfFrames++
		}
	}
	if selfFrames < 3 {
		t.Fatalf("stream ended early after %d self frames (%v)", selfFrames, scanner.Err())
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if plainReads != 0 {
		t.Fatalf("plain usage-recording reads = %d, want 0", plainReads)
	}
	if probes != 1 {
		t.Fatalf("capability probes = %d, want 1 (memoized)", probes)
	}
}

// TestFactRevealUsesObservationalExactRead proves the user-initiated reveal
// endpoint is one exact observational read (skipping usage recording) that
// returns the sensitive value. The reveal is query-addressed like the
// upstream exact read — a /api/facts/{subject}/{predicate} path shape would
// let the literal /api/facts/{id}/history pattern shadow any fact whose
// predicate is the single segment "history", which the server's predicate
// grammar permits — so a "history" predicate stays revealable.
func TestFactRevealUsesObservationalExactRead(t *testing.T) {
	for _, predicate := range []string{"identity/ssn", "history"} {
		t.Run(predicate, func(t *testing.T) {
			srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method+" "+r.URL.Path != "GET /v1/facts" {
					t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				query := r.URL.Query()
				if query.Get("observational") != "true" {
					t.Errorf("observational = %q, want true", query.Get("observational"))
				}
				if query.Get("subject") != "self" || query.Get("predicate") != predicate {
					t.Errorf("unexpected exact read query %s", r.URL.RawQuery)
				}
				writeTestJSON(t, w, map[string]any{"fact": client.Fact{
					ID: "fact_2", Subject: "self", Predicate: predicate, Sensitive: true,
					Value: json.RawMessage(`"s3cret-value"`),
				}})
			}, nil)
			resp := authedGet(t, srv, cfg, "/api/fact?subject=self&predicate="+url.QueryEscape(predicate))
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("got %d, want 200", resp.StatusCode)
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !strings.Contains(string(raw), "s3cret-value") {
				t.Fatalf("reveal response must carry the value: %s", raw)
			}

			missing := authedGet(t, srv, cfg, "/api/fact?subject=self")
			defer func() { _ = missing.Body.Close() }()
			if missing.StatusCode != http.StatusBadRequest {
				t.Fatalf("missing predicate: got %d, want 400", missing.StatusCode)
			}
		})
	}
}

// TestFactRevealFallsBackToPlainReadOn501 proves the reveal — and only the
// reveal — degrades to the plain exact read on a cell without observational
// fact reads: a user-initiated reveal is an intentional exact lookup, so
// recording one legitimate delivery usage there is acceptable.
func TestFactRevealFallsBackToPlainReadOn501(t *testing.T) {
	var mu sync.Mutex
	var observational, plain bool
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("predicate") != "identity/ssn" {
			t.Errorf("unexpected backend request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if query.Get("observational") == "true" {
			mu.Lock()
			observational = true
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotImplemented)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "observational fact reads are unavailable"})
			return
		}
		mu.Lock()
		plain = true
		mu.Unlock()
		writeTestJSON(t, w, map[string]any{"fact": client.Fact{
			ID: "fact_2", Subject: "self", Predicate: "identity/ssn", Sensitive: true,
			Value: json.RawMessage(`"s3cret-value"`),
		}})
	}, nil)
	resp := authedGet(t, srv, cfg, "/api/fact?subject=self&predicate=identity%2Fssn")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(raw), "s3cret-value") {
		t.Fatalf("fallback reveal must carry the value: %s", raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if !observational || !plain {
		t.Fatalf("observational=%v plain=%v, want the observational attempt then the plain fallback", observational, plain)
	}
}

// TestFactHistoryProxyLocksValuesUnlessProvenNonSensitive proves the drill-in
// history forwards assertion values only when an exact observational read
// proves the fact non-sensitive; sensitive, unproven, or mismatched facts get
// value-free history (no per-assertion reveal in v1).
func TestFactHistoryProxyLocksValuesUnlessProvenNonSensitive(t *testing.T) {
	backend := func(sensitive bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/facts/fact_1/history":
				writeTestJSON(t, w, map[string]any{"assertions": []client.FactAssertion{
					{ID: "fas_1", FactID: "fact_1", Value: json.RawMessage(`"history-value"`), SourceRef: "history-ref"},
				}})
			case "/v1/facts":
				if answerFactReadProbe(w, r) {
					return
				}
				if r.URL.Query().Get("observational") != "true" {
					t.Errorf("sensitivity probe must be observational: %s", r.URL.RawQuery)
				}
				writeTestJSON(t, w, map[string]any{"fact": client.Fact{
					ID: "fact_1", Subject: "self", Predicate: "identity/name", Sensitive: sensitive,
				}})
			default:
				t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}
		}
	}

	cases := []struct {
		name      string
		sensitive bool
		path      string
		wantValue bool
	}{
		{"non-sensitive proven", false, "/api/facts/fact_1/history?subject=self&predicate=identity%2Fname", true},
		{"sensitive fact locked", true, "/api/facts/fact_1/history?subject=self&predicate=identity%2Fname", false},
		{"missing address locked", false, "/api/facts/fact_1/history", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, cfg := newDashboard(t, backend(tc.sensitive), nil)
			resp := authedGet(t, srv, cfg, tc.path)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("got %d, want 200", resp.StatusCode)
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !strings.Contains(string(raw), "fas_1") {
				t.Fatalf("assertion metadata missing: %s", raw)
			}
			if got := strings.Contains(string(raw), "history-value"); got != tc.wantValue {
				t.Fatalf("history value present = %v, want %v: %s", got, tc.wantValue, raw)
			}
			if !tc.wantValue && strings.Contains(string(raw), "history-ref") {
				t.Fatalf("locked history leaked source ref: %s", raw)
			}
		})
	}
}

// TestEventsStreamEmitsFactsFromRedactedList proves the opt-in facts tick
// polls the observational list (never include_sensitive, never the plain
// usage-recording list) and that a leaked sensitive value never reaches an
// SSE frame.
func TestEventsStreamEmitsFactsFromRedactedList(t *testing.T) {
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "GET /v1/facts":
			if answerFactReadProbe(w, r) {
				return
			}
			query := r.URL.Query()
			if query.Get("observational") != "true" {
				t.Errorf("facts tick must be observational, got %s", r.URL.RawQuery)
			}
			if query.Has("include_sensitive") {
				t.Errorf("include_sensitive must never be sent, got query %s", r.URL.RawQuery)
			}
			if query.Get("limit") != strconv.Itoa(sseFactPageLimit) {
				t.Errorf("limit = %q, want %d", query.Get("limit"), sseFactPageLimit)
			}
			writeTestJSON(t, w, map[string]any{"facts": []client.Fact{
				{ID: "fact_live", Subject: "self", Predicate: "identity/name", Value: json.RawMessage(`"Scott"`)},
				{ID: "fact_hot", Subject: "self", Predicate: "identity/ssn", Sensitive: true,
					Value: json.RawMessage(`"leaked-fact-value"`)},
			}})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?facts=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	sawEvent, sawItem := false, false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && (!sawEvent || !sawItem) {
		line := scanner.Text()
		if line == "event: facts" {
			sawEvent = true
		}
		if strings.Contains(line, "leaked-fact-value") {
			t.Fatalf("sensitive fact value reached the SSE stream: %q", line)
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "fact_live") {
			sawItem = true
		}
	}
	if !sawEvent || !sawItem {
		t.Fatalf("stream ended early: event=%v item=%v (%v)", sawEvent, sawItem, scanner.Err())
	}
	cancel()
}

// rejectSecretMutations fails the test if the dashboard reaches the sealed
// plane through anything but a plain GET: no POST, no lifecycle :action,
// and never the field :access route, which delivers encrypted material and
// records audit and usage.
func rejectSecretMutations(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodGet {
		t.Errorf("dashboard sent non-GET secrets request %s %s", r.Method, r.URL.Path)
	}
	if strings.Contains(r.URL.Path, ":") {
		t.Errorf("dashboard touched secret action route %s %s (never :archive/:restore/:access)", r.Method, r.URL.Path)
	}
}

// leakySecretJSON is a cell response that misbehaves in every way the proxy
// must survive: cryptographic material and plaintext-like values embedded in
// list and get payloads, including a public value on a field flagged
// sensitive. None of these strings may reach the browser.
func leakySecretJSON() map[string]any {
	return map[string]any{
		"id": "sec_1", "name": "prod-db", "template": "credential",
		"tags": []string{"prod"}, "lifecycle": "active",
		"sensitive_field_count": 1,
		"created_at":            "2026-07-01T00:00:00Z",
		"updated_at":            "2026-07-02T00:00:00Z",
		"ciphertext":            "leaked-ciphertext",
		"plaintext":             "leaked-plaintext",
		"wrapped_dek":           "leaked-wrapped-dek",
		"fields": []map[string]any{
			{
				"id": "fld_1", "name": "password", "kind": "password", "sensitive": true,
				"public_value": "leaked-public-value",
				"sealed": map[string]any{
					"ciphertext": "leaked-ciphertext",
					"aad":        "leaked-aad",
					"nonce":      "leaked-nonce",
					"dek":        map[string]any{"wrapped_dek": "leaked-wrapped-dek", "key_material": "leaked-key-material"},
				},
			},
			{"id": "fld_2", "name": "username", "kind": "text", "sensitive": false,
				"public_value": "leaked-public-value"},
		},
	}
}

// TestSecretsProxyListsMetadataOnly proves the secrets list proxy touches
// only GET /v1/secrets, forwards safe list options, and — defense in depth,
// mirroring stripMessageBodies — rebuilds every secret through the
// allow-list projection so no ciphertext, wrapped DEK, plaintext-like
// value, or even explicitly public field value a misbehaving cell embeds
// can reach the browser.
func TestSecretsProxyListsMetadataOnly(t *testing.T) {
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		rejectSecretMutations(t, r)
		if r.URL.Path != "/v1/secrets" {
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query()
		if query.Get("limit") != "25" || query.Get("include_fields") != "true" {
			t.Errorf("unexpected query %s", r.URL.RawQuery)
		}
		writeTestJSON(t, w, map[string]any{"items": []map[string]any{leakySecretJSON()}})
	}, nil)

	resp := authedGet(t, srv, cfg, "/api/secrets?limit=25")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, want := range []string{"sec_1", "prod-db", "password", `"sensitive_field_count":1`, `"field_count":2`, "active"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("metadata %q missing from proxied list: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "leaked") {
		t.Fatalf("secret material reached the browser: %s", raw)
	}

	bad := authedGet(t, srv, cfg, "/api/secrets?limit=nope")
	defer func() { _ = bad.Body.Close() }()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid limit: got %d, want 400", bad.StatusCode)
	}
}

// TestSecretProxyDetailMetadataOnly proves the detail proxy touches only
// GET /v1/secrets/{id} plus the read-only vault-key binding GET, forwards
// binding identifiers (never key material), and strips embedded
// cryptographic material exactly like the list.
func TestSecretProxyDetailMetadataOnly(t *testing.T) {
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		rejectSecretMutations(t, r)
		switch r.URL.Path {
		case "/v1/secrets/sec_1":
			writeTestJSON(t, w, map[string]any{"secret": leakySecretJSON()})
		case "/v1/vault/key-epochs/current":
			writeTestJSON(t, w, map[string]any{"key_epoch": map[string]any{
				"id": "avk_1", "key_version": 3, "algorithm": "age-x25519",
				"fingerprint": "fp-abc", "lifecycle_state": "current",
				"private_key": "leaked-private-key",
			}})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}, nil)

	resp := authedGet(t, srv, cfg, "/api/secrets/sec_1")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, want := range []string{"sec_1", "password", "username", "avk_1", `"key_version":3`, "fp-abc"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("metadata %q missing from proxied detail: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "leaked") {
		t.Fatalf("secret material reached the browser: %s", raw)
	}
}

// TestSecretsProxySurfacesPreSealedCellAsUnavailable proves a cell released
// before the sealed plane — no /v1/secrets routes at all, so every read
// 404s — surfaces as the distinguishable 501 state the UI renders as
// "sealed plane not available on this cell", on both the list and the
// detail route (where the capability probe disambiguates a missing route
// from a missing secret).
func TestSecretsProxySurfacesPreSealedCellAsUnavailable(t *testing.T) {
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/secrets") {
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
		}
		http.NotFound(w, r)
	}, nil)

	for _, path := range []string{"/api/secrets/sec_1", "/api/secrets"} {
		resp := authedGet(t, srv, cfg, path)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("%s: got %d, want 501", path, resp.StatusCode)
		}
		var body struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		_ = resp.Body.Close()
		if body.Error != "cell does not serve the sealed secrets plane" {
			t.Fatalf("%s: error = %q", path, body.Error)
		}
	}
}

// TestSecretDetailMissingSecretStays404OnSupportingCell proves the
// disambiguation cuts the other way too: on a cell that serves the sealed
// plane, a genuinely missing secret is a plain 404, never the
// unavailable-cell state.
func TestSecretDetailMissingSecretStays404OnSupportingCell(t *testing.T) {
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		rejectSecretMutations(t, r)
		switch r.URL.Path {
		case "/v1/secrets":
			writeTestJSON(t, w, map[string]any{"items": []map[string]any{}})
		case "/v1/secrets/sec_missing":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "secret resource not found"})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}, nil)

	resp := authedGet(t, srv, cfg, "/api/secrets/sec_missing")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", resp.StatusCode)
	}
}

// TestEventsStreamEmitsSecretsFromMetadataList proves the opt-in secrets
// tick polls only the side-effect-free metadata list (the sealed plane's
// list and get reads write no audit or usage rows; only the field :access
// POST does, and the dashboard never calls it) and that embedded secret
// material never reaches an SSE frame.
func TestEventsStreamEmitsSecretsFromMetadataList(t *testing.T) {
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "GET /v1/secrets":
			rejectSecretMutations(t, r)
			query := r.URL.Query()
			if query.Get("limit") != strconv.Itoa(sseSecretPageLimit) {
				t.Errorf("limit = %q, want %d", query.Get("limit"), sseSecretPageLimit)
			}
			if query.Get("include_fields") != "true" {
				t.Errorf("include_fields = %q, want true", query.Get("include_fields"))
			}
			leaky := leakySecretJSON()
			leaky["id"] = "sec_live"
			writeTestJSON(t, w, map[string]any{"items": []map[string]any{leaky}})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?secrets=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	sawEvent, sawItem := false, false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && (!sawEvent || !sawItem) {
		line := scanner.Text()
		if line == "event: secrets" {
			sawEvent = true
		}
		if strings.Contains(line, "leaked") {
			t.Fatalf("secret material reached the SSE stream: %q", line)
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "sec_live") {
			sawItem = true
		}
	}
	if !sawEvent || !sawItem {
		t.Fatalf("stream ended early: event=%v item=%v (%v)", sawEvent, sawItem, scanner.Err())
	}
	cancel()
}

// TestEventsStreamSkipsSecretsTickOnPreSealedCell proves the tick stops
// after one 404 from a cell without the sealed plane: the negative answer
// is memoized for the recheck window, no "secrets" event is ever emitted,
// and the missing routes are not re-polled every tick.
func TestEventsStreamSkipsSecretsTickOnPreSealedCell(t *testing.T) {
	var mu sync.Mutex
	secretPolls := 0
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "GET /v1/secrets":
			mu.Lock()
			secretPolls++
			mu.Unlock()
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?secrets=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	selfFrames := 0
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && selfFrames < 3 {
		line := scanner.Text()
		if line == "event: secrets" {
			t.Fatalf("pre-sealed cell must get no secrets tick, saw %q", line)
		}
		if line == "event: self" {
			selfFrames++
		}
	}
	if selfFrames < 3 {
		t.Fatalf("stream ended early after %d self frames (%v)", selfFrames, scanner.Err())
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if secretPolls != 1 {
		t.Fatalf("secret polls = %d, want 1 (memoized negative)", secretPolls)
	}
}

// TestEventsStreamRecoversSecretsAfterTransientNotFound proves one 404 —
// which the client cannot distinguish from a pre-sealed-plane cell when an
// intermediary mints it mid-deploy — disables the tick only for the recheck
// window, not the process lifetime: once upstream serves the list again the
// stream resumes secrets frames without a restart.
func TestEventsStreamRecoversSecretsAfterTransientNotFound(t *testing.T) {
	previous := secretsUnavailableRecheck
	secretsUnavailableRecheck = 30 * time.Millisecond
	defer func() { secretsUnavailableRecheck = previous }()

	var mu sync.Mutex
	secretPolls := 0
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "GET /v1/secrets":
			rejectSecretMutations(t, r)
			mu.Lock()
			secretPolls++
			first := secretPolls == 1
			mu.Unlock()
			if first {
				http.NotFound(w, r)
				return
			}
			writeTestJSON(t, w, map[string]any{"items": []map[string]any{{
				"id": "sec_back", "name": "prod-db", "lifecycle": "active",
				"created_at": "2026-07-01T00:00:00Z", "updated_at": "2026-07-02T00:00:00Z",
			}}})
		default:
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?secrets=true").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	sawItem := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && !sawItem {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "sec_back") {
			sawItem = true
		}
	}
	if !sawItem {
		t.Fatalf("stream never resumed secrets frames after the transient 404 (%v)", scanner.Err())
	}
	cancel()
}

// TestSecretsProbeDoesNotBlockMemoizedReads proves the capability probe
// releases the mutex during its upstream read: while one probe is in flight
// (up to the full client timeout), the SSE tick's knownUnavailable check and
// the note calls on every secrets response must not queue behind it.
func TestSecretsProbeDoesNotBlockMemoizedReads(t *testing.T) {
	probeStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case probeStarted <- struct{}{}:
		default:
		}
		<-release
		http.NotFound(w, r)
	}))
	t.Cleanup(cell.Close)

	secrets := &secretsCapability{}
	cfg := Config{Endpoint: cell.URL, BearerToken: testBearer}
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		_, _ = secrets.available(t.Context(), cfg)
	}()
	<-probeStarted

	answered := make(chan bool, 1)
	go func() { answered <- secrets.knownUnavailable() }()
	select {
	case got := <-answered:
		if got {
			t.Error("knownUnavailable = true while the probe is unresolved")
		}
	case <-time.After(5 * time.Second):
		t.Error("knownUnavailable blocked behind the in-flight probe")
	}
	close(release)
	<-probeDone
}

// TestThemesEndpointListsEmbeddedPacks proves the theme picker's source of
// truth is the embedded theme directory: dropping a CSS file into
// static/themes is the whole change (ADR 0004).
func TestThemesEndpointListsEmbeddedPacks(t *testing.T) {
	srv, cfg := newDashboard(t, selfBackend(t), nil)
	resp := authedGet(t, srv, cfg, "/api/themes")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	var body struct {
		Themes []string `json:"themes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"amber", "console", "high-contrast", "midnight", "paper"}
	if len(body.Themes) != len(want) {
		t.Fatalf("themes = %v, want %v", body.Themes, want)
	}
	for i, name := range want {
		if body.Themes[i] != name {
			t.Fatalf("themes = %v, want %v", body.Themes, want)
		}
	}
	for _, name := range body.Themes {
		if name == "auto" {
			t.Fatal("\"auto\" is a client-side picker entry and must never be an embedded pack")
		}
	}
}

// TestEventsStreamSeedsCursorAndDrainsPages proves /api/events starts the
// transcript cursor at the client-supplied after_sequence (no full replay)
// and drains more than one upstream page inside a single poll tick.
func TestEventsStreamSeedsCursorAndDrainsPages(t *testing.T) {
	entriesFrom := func(first, count int) []client.TranscriptEntry {
		entries := make([]client.TranscriptEntry, count)
		for i := range entries {
			entries[i] = client.TranscriptEntry{
				Sequence: int64(first + i),
				Role:     "assistant",
				Body:     "b-" + strconv.Itoa(first+i),
			}
		}
		return entries
	}
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "/v1/transcripts/tr_1":
			query := r.URL.Query()
			if query.Get("limit") != strconv.Itoa(sseTranscriptPageLimit) {
				t.Errorf("limit = %q, want %d", query.Get("limit"), sseTranscriptPageLimit)
			}
			after, err := strconv.ParseInt(query.Get("after_sequence"), 10, 64)
			if err != nil || after < 7 {
				// The stream must never restart from zero: the browser
				// seeded after_sequence=7 and the cursor only advances.
				t.Errorf("after_sequence = %q, want >= 7", query.Get("after_sequence"))
				writeTestJSON(t, w, client.TranscriptDetail{Transcript: client.Transcript{ID: "tr_1"}})
				return
			}
			detail := client.TranscriptDetail{Transcript: client.Transcript{ID: "tr_1"}}
			switch after {
			case 7:
				// One full page: the handler must keep draining this tick.
				detail.Entries = entriesFrom(8, sseTranscriptPageLimit)
				detail.NextAfterSequence = 7 + int64(sseTranscriptPageLimit)
			case 7 + int64(sseTranscriptPageLimit):
				detail.Entries = entriesFrom(8+sseTranscriptPageLimit, 2)
			}
			writeTestJSON(t, w, detail)
		default:
			t.Errorf("unexpected backend path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?transcript=tr_1&after_sequence=7").WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	wantLast := "b-" + strconv.Itoa(7+sseTranscriptPageLimit+2)
	wantID := "id: " + strconv.Itoa(7+sseTranscriptPageLimit+2)
	sawLast, sawID := false, false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && !sawLast {
		line := scanner.Text()
		if line == wantID {
			sawID = true
		}
		if strings.Contains(line, wantLast) {
			sawLast = true
		}
	}
	if !sawLast {
		t.Fatalf("stream ended before draining to %s (%v)", wantLast, scanner.Err())
	}
	if !sawID {
		t.Fatalf("transcript events carry no %q line for reconnect resumption", wantID)
	}
	cancel()
}

// TestEventsStreamHonorsLastEventID proves an EventSource auto-reconnect
// (browser sends Last-Event-ID) resumes from the last delivered cursor, not
// from the originally seeded after_sequence.
func TestEventsStreamHonorsLastEventID(t *testing.T) {
	polled := make(chan string, 8)
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/self":
			writeTestJSON(t, w, testSelfDigest())
		case "/v1/transcripts/tr_1":
			select {
			case polled <- r.URL.Query().Get("after_sequence"):
			default:
			}
			writeTestJSON(t, w, client.TranscriptDetail{Transcript: client.Transcript{ID: "tr_1"}})
		default:
			t.Errorf("unexpected backend path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
	srv, cfg := newDashboard(t, backend, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req := authedRequest(t, srv, cfg, "/api/events?transcript=tr_1&after_sequence=3").WithContext(ctx)
	req.Header.Set("Last-Event-ID", "9")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	select {
	case after := <-polled:
		if after != "9" {
			t.Fatalf("first poll after_sequence = %q, want 9 (Last-Event-ID wins over the stale seed)", after)
		}
	case <-ctx.Done():
		t.Fatal("no transcript poll observed")
	}
	cancel()
}

func TestEventsStreamRejectsInvalidAfterSequence(t *testing.T) {
	srv, cfg := newDashboard(t, selfBackend(t), nil)
	for _, raw := range []string{"nope", "-3"} {
		resp := authedGet(t, srv, cfg, "/api/events?transcript=tr_1&after_sequence="+raw)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("after_sequence=%s: got %d, want 400", raw, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// TestUpstreamBadRequestSurfacesAsBadRequest proves browser-supplied params
// the proxy forwards but the cell rejects come back as a client error, not a
// bad gateway.
func TestUpstreamBadRequestSurfacesAsBadRequest(t *testing.T) {
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "limit must be between 1 and 500"})
	}, nil)
	resp := authedGet(t, srv, cfg, "/api/transcripts/tr_1?limit=501")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body.Error, "limit must be between 1 and 500") {
		t.Fatalf("error = %q, want the upstream validation text", body.Error)
	}
}

func TestEventsStreamCapsConcurrentConnections(t *testing.T) {
	previous := maxSSEConnections
	maxSSEConnections = 1
	defer func() { maxSSEConnections = previous }()

	srv, cfg := newDashboard(t, selfBackend(t), nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first, err := srv.Client().Do(authedRequest(t, srv, cfg, "/api/events").WithContext(ctx))
	if err != nil {
		t.Fatalf("open first stream: %v", err)
	}
	defer func() { _ = first.Body.Close() }()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first stream: got %d", first.StatusCode)
	}
	// Ensure the handler is running (headers already flushed on Do return).
	second := authedGet(t, srv, cfg, "/api/events")
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second stream: got %d, want 429", second.StatusCode)
	}
}
