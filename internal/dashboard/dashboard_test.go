package dashboard

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	if !strings.Contains(string(body), "witself dashboard") {
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
				query.Get("include_message_checkpoint") != "true" || query.Get("include_avatar_checkpoint") != "true" {
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

func TestEventsStreamRejectsInvalidMessagesFlag(t *testing.T) {
	srv, cfg := newDashboard(t, selfBackend(t), nil)
	for _, path := range []string{"/api/events?messages=nope", "/api/events?memories=nope", "/api/events?facts=nope", "/api/events?secrets=nope"} {
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
	if len(body.Themes) != 2 || body.Themes[0] != "console" || body.Themes[1] != "paper" {
		t.Fatalf("themes = %v, want [console paper]", body.Themes)
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
