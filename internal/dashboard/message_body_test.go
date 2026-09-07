package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMessageBodyPreviewUsesOnlyPeekAndProjectsText(t *testing.T) {
	for _, body := range []string{"", "Hello\n  <img src=x onerror=alert(1)> & café", strings.Repeat("é", 32*1024)} {
		t.Run(stringSizeName(body), func(t *testing.T) {
			var calls atomic.Int32
			srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v1/messages/msg_1:peek" || r.URL.RawQuery != "" {
					t.Errorf("preview used unexpected route: %s %s", r.Method, r.URL)
					http.Error(w, "unexpected route", http.StatusBadRequest)
					return
				}
				if r.Header.Get("Authorization") != "Bearer "+testBearer {
					t.Error("preview did not use the configured agent token")
				}
				requestBody, err := io.ReadAll(r.Body)
				if err != nil || len(requestBody) != 0 {
					t.Error("observational peek must have no request body")
				}
				message := map[string]any{"id": "msg_1", "account_id": "private-account", "realm_id": "private-realm", "payload": map[string]any{"private": "private-payload"}, "processing": map[string]any{"claim_id": "private-fence"}}
				// Production omits empty bodies; both shapes must project a string.
				if body != "" {
					message["body"] = body
				}
				writeTestJSON(t, w, map[string]any{"message": message})
			}, nil)
			resp := authedGet(t, srv, cfg, "/api/messages/msg_1/body")
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			var got map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got["body"] != body {
				t.Fatal("preview was not the exact body-only projection")
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want exactly one peek", calls.Load())
			}
			assertMessagePreviewHeaders(t, resp)
		})
	}
}

func stringSizeName(s string) string {
	if s == "" {
		return "omitted_empty"
	}
	if len(s) == 64*1024 {
		return "exact_byte_limit"
	}
	return "literal_multiline_text"
}

func assertMessagePreviewHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	if !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") || !strings.Contains(resp.Header.Get("Cache-Control"), "private") || resp.Header.Get("Content-Security-Policy") == "" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("preview lost the local private-response protections")
	}
}

func TestMessageBodyPreviewRefusesInvalidUpstreamWithoutFallback(t *testing.T) {
	const privateMarker = "private-upstream-detail"
	cases := []struct {
		name   string
		status int
		raw    string
		want   int
	}{
		{"wrong_id", 200, `{"message":{"id":"another","body":"private-upstream-detail"}}`, 502},
		{"missing_message", 200, `{}`, 502},
		{"null_message", 200, `{"message":null}`, 502},
		{"bad_json", 200, `{"message":`, 502},
		{"numeric_body", 200, `{"message":{"id":"msg_1","body":123}}`, 502},
		{"object_body", 200, `{"message":{"id":"msg_1","body":{"private":"private-upstream-detail"}}}`, 502},
		{"over_byte_limit", 200, `{"message":{"id":"msg_1","body":"` + strings.Repeat("é", 32*1024+1) + `"}}`, 502},
	}
	for _, status := range []int{400, 401, 403, 404, 405, 409, 429, 500, 501, 503} {
		want := http.StatusBadGateway
		if status == 403 || status == 404 {
			want = status
		}
		cases = append(cases, struct {
			name   string
			status int
			raw    string
			want   int
		}{
			http.StatusText(status), status, `{"error":{"code":"feature_not_enabled","message":"private-upstream-detail"}}`, want,
		})
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method+" "+r.URL.Path != "GET /v1/messages/msg_1:peek" {
					t.Errorf("unexpected fallback route %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.raw)
			}, nil)
			resp := authedGet(t, srv, cfg, "/api/messages/msg_1/body")
			raw, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tt.want || string(raw) != "{\"error\":\"message body is unavailable\"}\n" || strings.Contains(string(raw), privateMarker) {
				t.Fatalf("unsafe preview refusal: status %d, response %q", resp.StatusCode, raw)
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d", calls.Load())
			}
			assertMessagePreviewHeaders(t, resp)
		})
	}
}

func TestMessageBodyPreviewValidatesMethodPathAndSessionBeforePeek(t *testing.T) {
	var calls atomic.Int32
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}, nil)
	cookie := sessionCookie(t, srv, cfg)
	for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, srv.URL+"/api/messages/msg_1/body", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(cookie)
			resp, err := noRedirectClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET" {
				t.Fatalf("method refusal = %d, Allow %q", resp.StatusCode, resp.Header.Get("Allow"))
			}
			assertMessagePreviewHeaders(t, resp)
		})
	}
	for _, path := range []string{
		"/api/messages/msg_1/body?limit=1", "/api/messages/msg_1/body?",
		"/api/messages/%2F/body", "/api/messages/%2E/body", "/api/messages/msg%3Aread/body",
		"/api/messages/msg%252Fother/body", "/api/messages/-msg/body", "/api/messages/" + strings.Repeat("m", 129) + "/body",
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(cookie)
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		want := http.StatusBadRequest
		// The mux rejects an encoded slash before the preview pattern matches.
		if path == "/api/messages/%2F/body" {
			want = http.StatusNotFound
		}
		if resp.StatusCode != want {
			t.Errorf("%s status = %d, want %d", path, resp.StatusCode, want)
		}
		assertMessagePreviewHeaders(t, resp)
	}
	for _, boundary := range []string{"no_session", "foreign_host", "cross_site"} {
		t.Run(boundary, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/messages/msg_1/body", nil)
			if err != nil {
				t.Fatal(err)
			}
			if boundary != "no_session" {
				req.AddCookie(cookie)
			}
			if boundary == "foreign_host" {
				req.Host = "foreign.example:" + serverPort(srv)
			}
			if boundary == "cross_site" {
				req.Header.Set("Sec-Fetch-Site", "cross-site")
			}
			resp, err := noRedirectClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode < 400 || resp.StatusCode >= 500 {
				t.Fatalf("security refusal = %d", resp.StatusCode)
			}
			assertMessagePreviewHeaders(t, resp)
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("refused requests reached upstream %d times", calls.Load())
	}
}

func TestMessageBodyPreviewDoesNotFollowRedirect(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	srv, cfg := newDashboard(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}, nil)
	resp := authedGet(t, srv, cfg, "/api/messages/msg_1/body")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway || redirected.Load() != 0 {
		t.Fatalf("redirect refusal = %d; redirected requests = %d", resp.StatusCode, redirected.Load())
	}
}
