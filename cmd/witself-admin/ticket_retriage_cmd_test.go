package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type ticketRetriageRoundTripFunc func(*http.Request) (*http.Response, error)

func (f ticketRetriageRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestTicketRetriageCommand(t *testing.T) {
	requests := 0
	var got map[string]any
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	http.DefaultTransport = ticketRetriageRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodPatch ||
			r.URL.Path != "/v1/admin/accounts/acct_1/tickets/tkt_1/retriage" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer admin-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"ticket": {
					"id": "tkt_1",
					"account_id": "acct_1",
					"category": "security",
					"priority": "urgent"
				}
			}`)),
			Request: r,
		}, nil
	})

	if code := ticketCmd([]string{
		"retriage", "--endpoint", "https://cp.example", "--token", "admin-token",
		"--account", "acct_1", "--ticket", "tkt_1",
		"--category", "security", "--priority", "urgent", "--json",
	}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if requests != 1 || got["category"] != "security" || got["priority"] != "urgent" {
		t.Fatalf("requests = %d, body = %#v", requests, got)
	}

	if code := ticketCmd([]string{
		"retriage", "--endpoint", "https://cp.example", "--token", "admin-token",
		"--account", "acct_1", "--ticket", "tkt_1",
	}); code != 2 {
		t.Fatalf("missing changes exit code = %d, want 2", code)
	}
	if requests != 1 {
		t.Fatalf("invalid invocation made %d requests, want 1", requests)
	}
}
