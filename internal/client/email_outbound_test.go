package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAgentEmailOutboundClientRoutesHeadersAndOmitsFrom(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/email:send":
			if r.Header.Get("Idempotency-Key") != "send-1" {
				t.Errorf("send key = %q", r.Header.Get("Idempotency-Key"))
			}
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"from"`) || !strings.Contains(string(body), `"text":"hello"`) {
				t.Errorf("send body = %s", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"id": "esnd_aaaaaaaaaaaaaaaa", "from": "owner.realm@send.witmail.net", "to": "person@example.com", "state": "queued", "provider_state": ""}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/email/emsg_aaaaaaaaaaaaaaaa:reply":
			if r.Header.Get("Idempotency-Key") != "reply-1" {
				t.Errorf("reply key = %q", r.Header.Get("Idempotency-Key"))
			}
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"to"`) || strings.Contains(string(body), `"subject"`) || strings.Contains(string(body), `"from"`) {
				t.Errorf("reply body accepted routing fields = %s", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"id": "esnd_bbbbbbbbbbbbbbbb", "reply_to_inbound_message_id": "emsg_aaaaaaaaaaaaaaaa", "state": "queued", "provider_state": ""}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/email/sent":
			if r.URL.Query().Get("state") != "queued" || r.URL.Query().Get("limit") != "7" || r.URL.Query().Get("cursor") != "next" {
				t.Errorf("list query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]any{{"id": "esnd_aaaaaaaaaaaaaaaa", "state": "queued", "provider_state": ""}}, "next_cursor": "after"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/email/sent/esnd_aaaaaaaaaaaaaaaa":
			_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"id": "esnd_aaaaaaaaaaaaaaaa", "state": "queued", "provider_state": ""}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	sent, err := SendAgentEmail(ctx, server.URL, "agent-token", SendAgentEmailInput{
		To: "person@example.com", Subject: "Hello", Text: "hello", IdempotencyKey: "send-1",
	})
	if err != nil || sent.ID != "esnd_aaaaaaaaaaaaaaaa" || sent.From == "" {
		t.Fatalf("send = %#v, %v", sent, err)
	}
	reply, err := ReplyAgentEmail(ctx, server.URL, "agent-token", "emsg_aaaaaaaaaaaaaaaa", ReplyAgentEmailInput{Text: "reply", IdempotencyKey: "reply-1"})
	if err != nil || reply.ReplyToInboundMessageID != "emsg_aaaaaaaaaaaaaaaa" {
		t.Fatalf("reply = %#v, %v", reply, err)
	}
	page, err := ListSentAgentEmails(ctx, server.URL, "agent-token", AgentEmailOutboundListOptions{State: "queued", Limit: 7, Cursor: "next"})
	if err != nil || len(page.Messages) != 1 || page.NextCursor != "after" {
		t.Fatalf("list = %#v, %v", page, err)
	}
	shown, err := GetSentAgentEmail(ctx, server.URL, "agent-token", "esnd_aaaaaaaaaaaaaaaa")
	if err != nil || shown.ID != "esnd_aaaaaaaaaaaaaaaa" {
		t.Fatalf("show = %#v, %v", shown, err)
	}
	if len(paths) != 4 {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestAgentEmailOutboundClientPreservesRateLimitDetails(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		dimension  string
		key        string
		scope      string
		limit      int64
		used       int64
		window     int64
		retryAfter time.Duration
		retryable  bool
		source     string
	}{
		{
			name:      "platform account breaker",
			status:    http.StatusTooManyRequests,
			body:      `{"schema_version":"witself.v0","code":"rate_limited","error":"outbound agent-email rate limit reached","retryable":true,"retry_after":1,"details":{"limit_dimension":"agent_email_sent","limit_key":"agent_email_sent_per_account_minute","scope":"account","limit":1000,"used":1000,"attempted":1,"window_seconds":60,"retry_after":1,"source":"platform"}}`,
			dimension: "agent_email_sent", key: "agent_email_sent_per_account_minute",
			scope: "account", limit: 1000, used: 1000, retryAfter: time.Second,
			window: 60, retryable: true, source: "platform",
		},
		{
			name:      "platform daily account breaker",
			status:    http.StatusTooManyRequests,
			body:      `{"schema_version":"witself.v0","code":"rate_limited","error":"outbound agent-email rate limit reached","retryable":true,"retry_after":60,"details":{"limit_dimension":"agent_email_sent","limit_key":"agent_email_sent_per_account_day","scope":"account","limit":10000,"used":10000,"attempted":1,"window_seconds":86400,"retry_after":60,"source":"platform"}}`,
			dimension: "agent_email_sent", key: "agent_email_sent_per_account_day",
			scope: "account", limit: 10_000, used: 10_000, window: 86_400,
			retryAfter: time.Minute, retryable: true, source: "platform",
		},
		{
			name:      "platform daily recipient breaker",
			status:    http.StatusTooManyRequests,
			body:      `{"schema_version":"witself.v0","code":"rate_limited","error":"outbound agent-email rate limit reached","retryable":true,"retry_after":60,"details":{"limit_dimension":"agent_email_sent","limit_key":"agent_email_sent_per_recipient_day","scope":"recipient","limit":100,"used":100,"attempted":1,"window_seconds":86400,"retry_after":60,"source":"platform"}}`,
			dimension: "agent_email_sent", key: "agent_email_sent_per_recipient_day",
			scope: "recipient", limit: 100, used: 100, window: 86_400,
			retryAfter: time.Minute, retryable: true, source: "platform",
		},
		{
			name:      "transient agent limit",
			status:    http.StatusTooManyRequests,
			body:      `{"schema_version":"witself.v0","code":"rate_limited","error":"outbound agent-email rate limit reached","retryable":true,"retry_after":2,"details":{"limit_dimension":"agent_email_sent","limit_key":"agent_email_sent_per_agent_minute","scope":"agent","limit":4,"used":4,"attempted":1,"window_seconds":60,"retry_after":2,"source":"plan"}}`,
			dimension: "agent_email_sent", key: "agent_email_sent_per_agent_minute",
			scope: "agent", limit: 4, used: 4, retryAfter: 2 * time.Second,
			window: 60, retryable: true, source: "plan",
		},
		{
			name:      "hard realm limit",
			status:    http.StatusForbidden,
			body:      `{"schema_version":"witself.v0","code":"limit_exceeded","error":"outbound agent-email rate limit reached","retryable":false,"details":{"limit_dimension":"agent_email_sent","limit_key":"agent_email_sent_per_realm_minute","scope":"realm","limit":10,"used":10,"attempted":1,"window_seconds":60,"source":"account_override"}}`,
			dimension: "agent_email_sent", key: "agent_email_sent_per_realm_minute",
			scope: "realm", limit: 10, used: 10, window: 60, source: "account_override",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/email:send" {
					t.Fatalf("request = %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			_, err := SendAgentEmail(context.Background(), server.URL, "agent-token", SendAgentEmailInput{
				To: "person@example.com", Subject: "Hello", Text: "hello",
				IdempotencyKey: "send-rate-limit",
			})
			var rateErr *MessageRateLimitError
			if !errors.Is(err, ErrMessageRateLimited) || !errors.As(err, &rateErr) {
				t.Fatalf("error = %v, want MessageRateLimitError", err)
			}
			if rateErr.LimitDimension != test.dimension || rateErr.LimitKey != test.key ||
				rateErr.Scope != test.scope || rateErr.Limit != test.limit ||
				rateErr.Used != test.used || rateErr.Attempted != 1 ||
				rateErr.WindowSeconds != test.window || rateErr.RetryAfter != test.retryAfter ||
				rateErr.Retryable != test.retryable || rateErr.Source != test.source ||
				rateErr.Error() != "outbound agent-email rate limit reached" {
				t.Fatalf("rate error = %+v", rateErr)
			}
		})
	}
}
