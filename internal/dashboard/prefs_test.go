package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const prefsTestDoc = `{"schema":"witself.dashboard-prefs.v1","theme":"amber"}`

// recordingBackend is a fake cell that answers minimal JSON to everything and
// records every upstream request the proxy makes, concurrency-safe so SSE
// poll ticks can be captured too.
type recordingBackend struct {
	mu    sync.Mutex
	calls []string
}

func (b *recordingBackend) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.calls = append(b.calls, r.Method+" "+r.URL.Path)
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/self/dashboard-preferences":
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","preferences":{"agent_id":"agt_dash","prefs":` + prefsTestDoc + `,"updated_at":"2026-07-19T12:00:00Z"}}`))
		case "PUT /v1/self/dashboard-preferences":
			raw, _ := io.ReadAll(r.Body)
			var in struct {
				Prefs json.RawMessage `json:"prefs"`
			}
			_ = json.Unmarshal(raw, &in)
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","preferences":{"agent_id":"agt_dash","prefs":` + string(in.Prefs) + `,"updated_at":"2026-07-19T12:00:00Z"}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}
}

func (b *recordingBackend) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

// prefsTestServer bundles a mounted dashboard with a ready session cookie so
// prefs tests can issue non-GET requests without re-running the exchange.
type prefsTestServer struct {
	srv    *httptest.Server
	cookie *http.Cookie
}

func authedDo(t *testing.T, srv *prefsTestServer, method, path, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.srv.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(srv.cookie)
	resp, err := srv.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestPrefsProxyGetAndPutRoundTrip(t *testing.T) {
	backend := &recordingBackend{}
	srv, cfg := newDashboard(t, backend.handler(), nil)
	bundle := &prefsTestServer{srv: srv, cookie: sessionCookie(t, srv, cfg)}

	resp := authedDo(t, bundle, http.MethodGet, "/api/prefs", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Preferences struct {
			AgentID string          `json:"agent_id"`
			Prefs   json.RawMessage `json:"prefs"`
		} `json:"preferences"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Preferences.AgentID != "agt_dash" || string(got.Preferences.Prefs) != prefsTestDoc {
		t.Fatalf("GET preferences = %#v", got.Preferences)
	}

	putResp := authedDo(t, bundle, http.MethodPut, "/api/prefs",
		`{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"midnight"}}`)
	defer func() { _ = putResp.Body.Close() }()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", putResp.StatusCode)
	}
	sawPut := false
	for _, call := range backend.snapshot() {
		if call == "PUT /v1/self/dashboard-preferences" {
			sawPut = true
		}
	}
	if !sawPut {
		t.Fatalf("proxy never forwarded the prefs PUT: %v", backend.snapshot())
	}
}

func TestPrefsPutForwardsOnlyValidatedShape(t *testing.T) {
	var forwarded []byte
	var mu sync.Mutex
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "PUT /v1/self/dashboard-preferences" {
			t.Errorf("unexpected backend request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		forwarded = raw
		mu.Unlock()
		writeTestJSON(t, w, map[string]any{"schema_version": "witself.v0", "preferences": map[string]any{
			"agent_id": "agt_dash", "prefs": json.RawMessage(prefsTestDoc), "updated_at": "2026-07-19T12:00:00Z",
		}})
	}, nil)
	bundle := &prefsTestServer{srv: srv, cookie: sessionCookie(t, srv, cfg)}

	resp := authedDo(t, bundle, http.MethodPut, "/api/prefs",
		"{\n  \"prefs\": {\"theme\": \"amber\", \"schema\": \"witself.dashboard-prefs.v1\"}\n}")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	// The proxy rebuilds the outgoing document from the validated fields:
	// canonical field order, no passthrough of the caller's raw bytes.
	if string(forwarded) != `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"amber"}}` {
		t.Fatalf("forwarded body = %s", forwarded)
	}
}

func TestPrefsPutRejectsBadBodiesLocally(t *testing.T) {
	backend := &recordingBackend{}
	srv, cfg := newDashboard(t, backend.handler(), nil)
	bundle := &prefsTestServer{srv: srv, cookie: sessionCookie(t, srv, cfg)}

	cases := map[string]string{
		"not JSON":         `nonsense`,
		"missing prefs":    `{}`,
		"unknown envelope": `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"amber"},"extra":1}`,
		"unknown pref key": `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"amber","font":"mono"}}`,
		"wrong schema":     `{"prefs":{"schema":"witself.dashboard-prefs.v2","theme":"amber"}}`,
		"empty theme":      `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":""}}`,
		"path theme":       `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"../evil"}}`,
		"oversized theme": `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"` +
			strings.Repeat("a", maxPrefsThemeBytes+1) + `"}}`,
		"oversized body": `{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"amber"},"pad":"` +
			strings.Repeat("x", maxPrefsRequestBytes) + `"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := authedDo(t, bundle, http.MethodPut, "/api/prefs", body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
	// Every rejection above must have been answered locally: the fake cell
	// saw no request at all.
	if calls := backend.snapshot(); len(calls) != 0 {
		t.Fatalf("invalid PUTs reached the cell: %v", calls)
	}
}

func TestPrefsRejectsEveryOtherMethod(t *testing.T) {
	backend := &recordingBackend{}
	srv, cfg := newDashboard(t, backend.handler(), nil)
	bundle := &prefsTestServer{srv: srv, cookie: sessionCookie(t, srv, cfg)}

	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch, http.MethodHead, http.MethodOptions} {
		resp := authedDo(t, bundle, method, "/api/prefs", "")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405", method, resp.StatusCode)
		}
	}
	if calls := backend.snapshot(); len(calls) != 0 {
		t.Fatalf("refused methods reached the cell: %v", calls)
	}
}

// TestProxyOnlyMutatingUpstreamRouteIsPrefsPut is the standing regression
// guard on the read-only contract: after driving every /api surface —
// including the SSE poll ticks and the one deliberate prefs write — the only
// non-GET request the cell ever saw is PUT /v1/self/dashboard-preferences.
func TestProxyOnlyMutatingUpstreamRouteIsPrefsPut(t *testing.T) {
	backend := &recordingBackend{}
	srv, cfg := newDashboard(t, backend.handler(), nil)
	bundle := &prefsTestServer{srv: srv, cookie: sessionCookie(t, srv, cfg)}

	paths := []string{
		"/api/self",
		"/api/themes",
		"/api/avatar.svg",
		"/api/transcripts",
		"/api/transcripts/tr_1",
		"/api/memories",
		"/api/memories/mem_1",
		"/api/memories/mem_1/history",
		"/api/messages",
		"/api/facts",
		"/api/facts/fact_1/history?subject=s&predicate=p",
		"/api/fact?subject=s&predicate=p",
		"/api/secrets",
		"/api/secrets/sec_abcdefghijklmnop",
		"/api/prefs",
	}
	for _, path := range paths {
		resp := authedDo(t, bundle, http.MethodGet, path, "")
		_ = resp.Body.Close()
	}
	putResp := authedDo(t, bundle, http.MethodPut, "/api/prefs",
		`{"prefs":{"schema":"witself.dashboard-prefs.v1","theme":"high-contrast"}}`)
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("prefs PUT status = %d, want 200", putResp.StatusCode)
	}

	// Hold one SSE stream with every tick enabled for a few poll intervals so
	// the poll loop's upstream reads are captured too.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/events?transcript=tr_1&messages=true&memories=true&facts=true&secrets=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(bundle.cookie)
	if resp, err := srv.Client().Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	calls := backend.snapshot()
	if len(calls) == 0 {
		t.Fatal("fake cell saw no requests at all")
	}
	var mutations []string
	for _, call := range calls {
		if !strings.HasPrefix(call, http.MethodGet+" ") {
			mutations = append(mutations, call)
		}
	}
	if len(mutations) != 1 || mutations[0] != "PUT /v1/self/dashboard-preferences" {
		t.Fatalf("upstream mutations = %v, want exactly one PUT /v1/self/dashboard-preferences", mutations)
	}
	if !bytes.Contains([]byte(strings.Join(calls, "\n")), []byte("GET /v1/self")) {
		t.Fatalf("expected read traffic in %v", calls)
	}
}
