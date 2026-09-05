package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
)

type supportLimitTransport struct{ handler http.Handler }

func (t supportLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, req)
	return w.Result(), nil
}

func TestOpenSupportTicketRateLimitHTTPAndClient(t *testing.T) {
	mux := apiMux(Config{
		Authenticate: func(context.Context, string) (string, string, string, bool, error) {
			return "operator", "account", "active", true, nil
		},
		OpenSupportTicket: func(context.Context, OpenTicketRequest) (SupportTicket, SupportTicketMessage, error) {
			return SupportTicket{}, SupportTicketMessage{}, fmt.Errorf("wrapped: %w", &SupportRateLimitError{Limit: 10, WindowSeconds: 60, RetryAfterSeconds: 60})
		},
	})
	req := httptest.NewRequest(http.MethodPost, "http://support.test/v1/support/tickets", strings.NewReader(`{"subject":"private subject","body":"private body"}`))
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("status/headers=%d/%v", w.Code, w.Header())
	}
	var body struct {
		Code       string `json:"code"`
		Retryable  bool   `json:"retryable"`
		RetryAfter int    `json:"retry_after"`
		Details    struct {
			Scope         string `json:"scope"`
			Limit         int    `json:"limit"`
			WindowSeconds int    `json:"window_seconds"`
			RetryAfter    int    `json:"retry_after"`
		} `json:"details"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "rate_limited" || !body.Retryable || body.RetryAfter != 60 || body.Details.Scope != "account" || body.Details.Limit != 10 || body.Details.WindowSeconds != 60 || body.Details.RetryAfter != 60 {
		t.Fatalf("body=%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "operator") {
		t.Fatalf("private refusal=%s", w.Body.String())
	}
	// Exercise the actual CLI client decoder without binding a test listener.
	previous := http.DefaultTransport
	http.DefaultTransport = supportLimitTransport{handler: mux}
	t.Cleanup(func() { http.DefaultTransport = previous })
	_, err := client.OpenSupportTicket(context.Background(), "http://support.test", "token", client.OpenTicketInput{Subject: "subject", Body: "body"})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "rate_limited" || !apiErr.Retryable {
		t.Fatalf("client error=%T %v", err, err)
	}
}
