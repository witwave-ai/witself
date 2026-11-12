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

// Keep foreign traffic out of expected-request counters without hiding it.
type httpAdmission struct {
	mu    sync.Mutex
	first string
}

func (a *httpAdmission) admit(w http.ResponseWriter, r *http.Request, routes ...string) bool {
	method, route := false, false
	for _, allowed := range routes {
		method = method || strings.HasPrefix(allowed, r.Method+" ")
		route = route || allowed == r.Method+" "+r.URL.Path
	}
	class := ""
	switch {
	case !method:
		class = "foreign_method"
	case !route:
		class = "foreign_path"
	case r.Method == http.MethodPost:
		body, err := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		switch {
		case err != nil:
			class = "other"
		case len(body) == 0:
			class = "empty_body_post"
		case !json.Valid(body):
			class = "other"
		}
	}
	if class == "" {
		return true
	}
	a.mu.Lock()
	if a.first == "" {
		a.first = class
	}
	a.mu.Unlock()
	w.WriteHeader(http.StatusBadRequest)
	return false
}

func (a *httpAdmission) check(t interface {
	Helper()
	Errorf(string, ...any)
}) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.first != "" {
		t.Errorf("unexpected HTTP request class=%s", a.first)
	}
}

type admissionReport struct{ lines []string }

func (*admissionReport) Helper() {}
func (r *admissionReport) Errorf(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func TestHTTPAdmissionOracle(t *testing.T) {
	for _, tc := range []struct{ name, method, path, body, class string }{
		{"none", "", "", "", ""},
		{"admitted post", "POST", "/expected", `{"ok":true}`, ""},
		{"admitted get", "GET", "/manifest", "", ""},
		{"method", "PRIVATE", "/expected", "private", "foreign_method"},
		{"path", "POST", "/private?private=value", `{}`, "foreign_path"},
		{"empty", "POST", "/expected", "", "empty_body_post"},
		{"invalid", "POST", "/expected", "private", "other"},
		{"first wins", "POST", "/private", `{}`, "foreign_path"},
		{"after call", "POST", "/expected", "", "empty_body_post"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &httpAdmission{}
			admit := func(method, path, body string) bool {
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				ok := a.admit(httptest.NewRecorder(), r, "POST /expected", "GET /manifest")
				if ok {
					got, err := io.ReadAll(r.Body)
					if err != nil || string(got) != body {
						t.Fatal("admitted body changed")
					}
				}
				return ok
			}
			if tc.name == "after call" && !admit("POST", "/expected", `{}`) {
				t.Fatal("expected call rejected")
			}
			if tc.method != "" && admit(tc.method, tc.path, tc.body) != (tc.class == "") {
				t.Fatal("request admission mismatch")
			}
			if tc.name == "first wins" {
				admit("PRIVATE", "/expected", "")
			}
			report := &admissionReport{}
			a.check(report)
			want := ""
			if tc.class != "" {
				want = "unexpected HTTP request class=" + tc.class
			}
			if strings.Join(report.lines, "\n") != want {
				t.Fatal("foreign request reporting mismatch")
			}
		})
	}
}
