package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Keep unexpected traffic separate from signup attempts, but never ignore it.
// Only closed classes leave this fixture; request values and decode errors do not.
type consentlessSignupRequests struct {
	mu                         sync.Mutex
	manifestCalls, signupCalls int
	posted                     map[string]string
	unexpected                 string
}

func (s *consentlessSignupRequests) recordUnexpected(class string) {
	if s.unexpected == "" {
		s.unexpected = class
	}
}

func (s *consentlessSignupRequests) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path == "/legal/versions.json" {
		s.manifestCalls++
		if r.Method != http.MethodGet {
			s.recordUnexpected("foreign_method")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeSignupLegalTestManifest(w, "new-terms", "new-privacy")
		return
	}
	switch {
	case r.Method != http.MethodPost:
		s.recordUnexpected("foreign_method")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	case r.URL.Path != "/v1/accounts":
		s.recordUnexpected("foreign_path")
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.signupCalls++
	body, err := io.ReadAll(r.Body)
	switch {
	case err != nil:
		s.recordUnexpected("other")
	case len(body) == 0:
		s.recordUnexpected("empty_body_post")
	default:
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&s.posted); err != nil {
			s.recordUnexpected("other")
		}
	}
	w.WriteHeader(http.StatusBadRequest)
	_, _ = fmt.Fprint(w, `{"error":"stop before provisioning"}`)
}

type signupRequestReporter interface {
	Helper()
	Logf(string, ...any)
	Fatalf(string, ...any)
}

func (s *consentlessSignupRequests) assertCounts(t signupRequestReporter, code int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if code != 1 || s.manifestCalls != 0 || s.signupCalls != 1 || s.unexpected != "" {
		class := s.unexpected
		if class == "" {
			class = "other"
			if s.signupCalls > 1 {
				class = "duplicate_post"
			}
		}
		t.Logf("signup request class=%s", class)
		t.Fatalf("legacy journal was upgraded: exit=%d manifest=%d signup=%d", code, s.manifestCalls, s.signupCalls)
	}
}

type signupRequestReport struct{ lines []string }

func (*signupRequestReport) Helper() {}
func (r *signupRequestReport) Logf(format string, args ...any) {
	r.lines = append(r.lines, "log: "+fmt.Sprintf(format, args...))
}
func (r *signupRequestReport) Fatalf(format string, args ...any) {
	r.lines = append(r.lines, "fatal: "+fmt.Sprintf(format, args...))
}

// Exercise the same handler and exact-count assertion used by the command test.
// Injected traffic is deterministic evidence about the fixture, not proof of
// the source of a historical stray request.
func TestSignupLegalConsentlessRequestOracle(t *testing.T) {
	const validBody = `{"provision_id":"synthetic"}`
	for _, tc := range []struct {
		name, method, path, body, class string
		initialSignup                   bool
		code, signup, manifest, status  int
	}{
		{"one signup", "POST", "/v1/accounts", validBody, "", false, 1, 1, 0, 400},
		{"foreign method", "PRIVATE_METHOD", "/v1/accounts", "private-body", "foreign_method", true, 1, 1, 0, 405},
		{"foreign path", "POST", "/private-path?private=query", validBody, "foreign_path", true, 1, 1, 0, 404},
		{"foreign empty get", "GET", "/private-path", "", "foreign_method", true, 1, 1, 0, 405},
		{"empty body", "POST", "/v1/accounts", "", "empty_body_post", false, 1, 1, 0, 400},
		{"second empty post", "POST", "/v1/accounts", "", "empty_body_post", true, 1, 2, 0, 400},
		{"duplicate post", "POST", "/v1/accounts", validBody, "duplicate_post", true, 1, 2, 0, 400},
		{"invalid body", "POST", "/v1/accounts", "private-body", "other", false, 1, 1, 0, 400},
		{"whitespace body", "POST", "/v1/accounts", " ", "other", false, 1, 1, 0, 400},
		{"manifest", "GET", "/legal/versions.json", "", "other", true, 1, 1, 1, 200},
		{"wrong exit", "POST", "/v1/accounts", validBody, "other", false, 0, 1, 0, 400},
		{"missing signup", "", "", "", "other", false, 1, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := &consentlessSignupRequests{}
			if tc.initialSignup {
				requests.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/accounts", strings.NewReader(validBody)))
			}
			if tc.method != "" {
				response := httptest.NewRecorder()
				request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				request.Header.Set("X-Private-Fixture", "private-header")
				requests.ServeHTTP(response, request)
				if response.Code != tc.status {
					t.Fatal("request received the wrong fixture response")
				}
			}
			if requests.signupCalls != tc.signup || requests.manifestCalls != tc.manifest {
				t.Fatal("request counted in the wrong class")
			}
			report := &signupRequestReport{}
			requests.assertCounts(report, tc.code)
			if tc.class == "" {
				if len(report.lines) != 0 || requests.posted["provision_id"] != "synthetic" {
					t.Fatal("one valid signup did not preserve the decoded request")
				}
				return
			}
			if len(report.lines) != 2 || report.lines[0] != "log: signup request class="+tc.class ||
				report.lines[1] != fmt.Sprintf("fatal: legacy journal was upgraded: exit=%d manifest=%d signup=%d", tc.code, tc.manifest, tc.signup) {
				t.Fatal("request failure must emit one value-free class before the unchanged fatal")
			}
		})
	}
	// The earliest unexpected request is the evidence; a later one must not replace it.
	t.Run("first class wins", func(t *testing.T) {
		requests := &consentlessSignupRequests{}
		requests.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/private-path", strings.NewReader(validBody)))
		requests.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/accounts", strings.NewReader("")))
		report := &signupRequestReport{}
		requests.assertCounts(report, 1)
		if len(report.lines) != 2 || report.lines[0] != "log: signup request class=foreign_path" {
			t.Fatal("the first unexpected request must name the class")
		}
	})
}
