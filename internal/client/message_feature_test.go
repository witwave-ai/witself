package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMessagingClientsPreserveFeatureNotEnabledRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","code":"feature_not_enabled","feature":"messaging","error":"Sorry, this feature is not enabled on this account.","retryable":false}`))
	}))
	defer srv.Close()

	calls := []struct {
		name string
		call func() error
	}{
		{
			name: "ordinary message",
			call: func() error {
				_, err := SendMessage(context.Background(), srv.URL, "token", SendMessageInput{
					To: "peer", Body: "hello",
				})
				return err
			},
		},
		{
			name: "message request",
			call: func() error {
				_, err := ListMessageRequests(context.Background(), srv.URL, "token", MessageRequestListOptions{})
				return err
			},
		},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			var featureErr *FeatureNotEnabledError
			if !errors.Is(err, ErrFeatureNotEnabled) || !errors.As(err, &featureErr) {
				t.Fatalf("error = %v, want FeatureNotEnabledError", err)
			}
			if featureErr.Feature != "messaging" || featureErr.Retryable ||
				featureErr.Error() != "Sorry, this feature is not enabled on this account." {
				t.Fatalf("feature error = %+v", featureErr)
			}
		})
	}
}

func TestMessagingClientsPreserveFlatRateLimitRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","code":"rate_limited","error":"message rate limit reached","retryable":true,"retry_after":3,"details":{"limit_dimension":"message_delivered","limit_key":"message_delivered_per_recipient_minute","scope":"recipient","limit":60,"used":60,"attempted":1,"window_seconds":60,"retry_after":3}}`))
	}))
	defer srv.Close()

	calls := []struct {
		name string
		call func() error
	}{
		{
			name: "ordinary message",
			call: func() error {
				_, err := SendMessage(context.Background(), srv.URL, "token", SendMessageInput{To: "peer", Body: "hello"})
				return err
			},
		},
		{
			name: "message request",
			call: func() error {
				_, err := CreateMessageRequest(context.Background(), srv.URL, "token", CreateMessageRequestInput{Body: "help"})
				return err
			},
		},
	}
	for _, test := range calls {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			var rateErr *MessageRateLimitError
			if !errors.Is(err, ErrMessageRateLimited) || !errors.As(err, &rateErr) {
				t.Fatalf("error = %v, want MessageRateLimitError", err)
			}
			if rateErr.LimitDimension != "message_delivered" ||
				rateErr.LimitKey != "message_delivered_per_recipient_minute" ||
				rateErr.Scope != "recipient" || rateErr.Limit != 60 || rateErr.Used != 60 ||
				rateErr.Attempted != 1 || rateErr.WindowSeconds != 60 ||
				rateErr.RetryAfter != 3*time.Second || !rateErr.Retryable ||
				rateErr.Error() != "message rate limit reached" {
				t.Fatalf("rate error = %+v", rateErr)
			}
		})
	}
}

func TestMessagingClientsPreserveHardRateLimitRefusal(t *testing.T) {
	for _, refusal := range []struct {
		name                              string
		body                              string
		dimension, key, scope             string
		limit, used, attempted, windowSec int64
	}{
		{
			name:      "zero effective sender limit",
			body:      `{"schema_version":"witself.v0","code":"limit_exceeded","error":"message rate limit reached","retryable":false,"details":{"limit_dimension":"message_sent","limit_key":"message_sent_per_agent_minute","scope":"agent","limit":0,"used":0,"attempted":1,"window_seconds":60}}`,
			dimension: "message_sent", key: "message_sent_per_agent_minute", scope: "agent",
			limit: 0, used: 0, attempted: 1, windowSec: 60,
		},
		{
			name:      "fanout exceeds realm capacity",
			body:      `{"schema_version":"witself.v0","code":"limit_exceeded","error":"message rate limit reached","retryable":false,"details":{"limit_dimension":"message_delivered","limit_key":"message_delivered_per_realm_minute","scope":"realm","limit":1,"used":0,"attempted":2,"window_seconds":60}}`,
			dimension: "message_delivered", key: "message_delivered_per_realm_minute", scope: "realm",
			limit: 1, used: 0, attempted: 2, windowSec: 60,
		},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(refusal.body))
			}))
			defer srv.Close()

			calls := []struct {
				name string
				call func() error
			}{
				{
					name: "ordinary message",
					call: func() error {
						_, err := SendMessage(context.Background(), srv.URL, "token", SendMessageInput{To: "peer", Body: "hello"})
						return err
					},
				},
				{
					name: "message request",
					call: func() error {
						_, err := CreateMessageRequest(context.Background(), srv.URL, "token", CreateMessageRequestInput{Body: "help"})
						return err
					},
				},
			}
			for _, call := range calls {
				t.Run(call.name, func(t *testing.T) {
					err := call.call()
					var rateErr *MessageRateLimitError
					if !errors.Is(err, ErrMessageRateLimited) || !errors.As(err, &rateErr) {
						t.Fatalf("error = %v, want MessageRateLimitError", err)
					}
					if rateErr.LimitDimension != refusal.dimension || rateErr.LimitKey != refusal.key ||
						rateErr.Scope != refusal.scope || rateErr.Limit != refusal.limit ||
						rateErr.Used != refusal.used || rateErr.Attempted != refusal.attempted ||
						rateErr.WindowSeconds != refusal.windowSec || rateErr.Retryable ||
						rateErr.RetryAfter != 0 || !rateErr.ResetAt.IsZero() ||
						rateErr.Error() != "message rate limit reached" {
						t.Fatalf("rate error = %+v", rateErr)
					}
				})
			}
		})
	}
}

func TestMessagingClientToleratesNestedRateLimitEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "4")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","error":{"code":"rate_limited","message":"message rate limit reached","retryable":true,"details":{"limit_dimension":"message_sent","limit_key":"message_sent_per_agent_minute","scope":"agent","limit":30,"used":30,"attempted":1,"window_seconds":60}}}`))
	}))
	defer srv.Close()

	_, err := SendMessage(context.Background(), srv.URL, "token", SendMessageInput{To: "peer", Body: "hello"})
	var rateErr *MessageRateLimitError
	if !errors.Is(err, ErrMessageRateLimited) || !errors.As(err, &rateErr) {
		t.Fatalf("error = %v, want MessageRateLimitError", err)
	}
	if rateErr.LimitDimension != "message_sent" || rateErr.Scope != "agent" ||
		rateErr.RetryAfter != 4*time.Second || !rateErr.Retryable {
		t.Fatalf("rate error = %+v", rateErr)
	}
}

func TestClientDoesNotClassifyGenericLimitAsMessageRate(t *testing.T) {
	for _, response := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name: "hard plan limit", status: http.StatusForbidden,
			body: `{"schema_version":"witself.v0","code":"limit_exceeded","error":"plan limit reached","retryable":false,"details":{"limit_dimension":"agents_per_realm","limit_key":"agents_per_realm","scope":"realm","limit":10,"used":10,"attempted":1}}`,
		},
		{
			name: "generic throttle", status: http.StatusTooManyRequests,
			body: `{"schema_version":"witself.v0","code":"rate_limited","error":"request rate limited","retryable":true,"retry_after":2,"details":{"limit_dimension":"api_requests","limit_key":"api_requests_per_minute","scope":"account","limit":60,"used":60,"attempted":1,"retry_after":2}}`,
		},
	} {
		t.Run(response.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(response.status)
				_, _ = w.Write([]byte(response.body))
			}))
			defer srv.Close()

			_, err := SendMessage(context.Background(), srv.URL, "token", SendMessageInput{To: "peer", Body: "hello"})
			var rateErr *MessageRateLimitError
			if errors.Is(err, ErrMessageRateLimited) || errors.As(err, &rateErr) {
				t.Fatalf("generic limit error = %#v, must not be MessageRateLimitError", err)
			}
		})
	}
}
