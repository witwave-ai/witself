package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The plan-entitlement refusal is a 403 — an upgrade boundary, not a
// retryable conflict — and must not be conflated with the operator
// kill-switch's 409.
func TestOpenTicketNotIncludedMapsToForbidden(t *testing.T) {
	server := httptest.NewServer(apiMux(Config{
		Authenticate: func(context.Context, string) (string, string, string, bool, error) {
			return "op_1", "acct_1", "active", true, nil
		},
		OpenSupportTicket: func(context.Context, OpenTicketRequest) (SupportTicket, SupportTicketMessage, error) {
			return SupportTicket{}, SupportTicketMessage{}, ErrSupportNotIncluded
		},
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/support/tickets",
		strings.NewReader(`{"subject":"s","body":"b"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden ||
		!strings.Contains(string(body), "not included in this plan") {
		t.Fatalf("status=%d body=%q; want 403 naming the plan boundary",
			resp.StatusCode, body)
	}
}
