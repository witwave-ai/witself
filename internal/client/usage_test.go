package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUsageDecodesTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Errorf("request path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("allow_truncation"); got != "1" {
			t.Errorf("allow_truncation = %q, want 1", got)
		}
		_, _ = w.Write([]byte(`{"usage":{"points":[],"totals":[],"truncated":true}}`))
	}))
	defer srv.Close()
	report, err := GetUsage(context.Background(), srv.URL, "agent-token", UsageQuery{AllowTruncation: true})
	if err != nil || !report.Truncated {
		t.Fatalf("decoded truncation = %v / %v", report.Truncated, err)
	}
}

func TestUsageDoesNotOptInByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("allow_truncation") || r.URL.Query().Has("max_rows") {
			t.Errorf("default request opted in to truncation: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"usage":{"points":[],"totals":[],"truncated":false}}`))
	}))
	defer srv.Close()
	report, err := GetUsage(context.Background(), srv.URL, "agent-token", UsageQuery{})
	if err != nil || report.Truncated {
		t.Fatalf("complete report = %#v / %v", report, err)
	}
}

func TestUsageQueryTooLargePreservesServerMessage(t *testing.T) {
	const message = "usage query exceeds the 10000-row cap; narrow --since/--until, use a coarser --group-by, or opt in with --allow-truncation (allow_truncation=1)"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("allow_truncation") || r.URL.Query().Has("max_rows") {
			t.Errorf("default request opted in to truncation: %s", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","code":"usage_query_too_large","error":"` + message + `","max_rows":10000}`))
	}))
	defer srv.Close()
	report, err := GetUsage(context.Background(), srv.URL, "agent-token", UsageQuery{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "usage_query_too_large" || err.Error() != message {
		t.Fatalf("query too large error = %#v / %v", apiErr, err)
	}
	if len(report.Points) != 0 || len(report.Totals) != 0 {
		t.Fatalf("query too large returned partial report: %#v", report)
	}
}

func TestUsageRejectsUnnegotiatedTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"points":[{"dimension":"message_sent","quantity":1}],"totals":[{"dimension":"message_sent","quantity":1}],"truncated":true}}`))
	}))
	defer srv.Close()
	report, err := GetUsage(context.Background(), srv.URL, "agent-token", UsageQuery{})
	if err == nil || !strings.Contains(err.Error(), "usage report is truncated") {
		t.Fatalf("unnegotiated truncation error = %v", err)
	}
	if len(report.Points) != 0 || len(report.Totals) != 0 {
		t.Fatalf("unnegotiated truncation returned partial report: %#v", report)
	}
}
