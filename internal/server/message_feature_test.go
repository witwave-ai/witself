package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessagingFeatureRefusalIsStructuredAcrossMessageSurfaces(t *testing.T) {
	principal := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_sender", AccountID: "acc_1",
		RealmID: "realm_1", AgentName: "sender", AccountStatus: "active",
	}
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return principal, true, nil
	}
	refusal := func() error {
		return &FeatureNotEnabledError{Feature: "messaging"}
	}
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		config Config
	}{
		{
			name: "ordinary send", method: http.MethodPost, path: "/v1/messages",
			body: `{"to":{"kind":"agent","id":"peer"},"body":"never stored"}`,
			config: Config{
				AuthenticatePrincipal: auth,
				SendMessage: func(context.Context, DomainPrincipal, SendMessageRequest) (Message, error) {
					return Message{}, refusal()
				},
			},
		},
		{
			name: "mailbox list", method: http.MethodGet, path: "/v1/messages",
			config: Config{
				AuthenticatePrincipal: auth,
				ListMessages: func(context.Context, DomainPrincipal, MessageListOptions) (MessagePage, error) {
					return MessagePage{}, refusal()
				},
			},
		},
		{
			name: "request open", method: http.MethodPost, path: "/v1/message-requests",
			body: `{"body":"never stored"}`,
			config: Config{
				AuthenticatePrincipal: auth,
				CreateMessageRequest: func(context.Context, DomainPrincipal, CreateMessageRequestRequest) (CreateMessageRequestResult, error) {
					return CreateMessageRequestResult{}, refusal()
				},
			},
		},
		{
			name: "request list", method: http.MethodGet, path: "/v1/message-requests",
			config: Config{
				AuthenticatePrincipal: auth,
				ListMessageRequests: func(context.Context, DomainPrincipal, MessageRequestListOptions) (MessageRequestPage, error) {
					return MessageRequestPage{}, refusal()
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(apiMux(tc.config))
			defer srv.Close()
			req, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := resp.Body.Close(); err != nil {
					t.Errorf("close response body: %v", err)
				}
			}()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
			if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
				t.Fatalf("cache control = %q", got)
			}
			var body struct {
				Code      string `json:"code"`
				Feature   string `json:"feature"`
				Error     string `json:"error"`
				Retryable bool   `json:"retryable"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Code != "feature_not_enabled" || body.Feature != "messaging" ||
				body.Error != "Sorry, this feature is not enabled on this account." ||
				body.Retryable {
				t.Fatalf("error body = %+v", body)
			}
		})
	}
}
