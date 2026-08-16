package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/witwave-ai/witself/internal/client"
)

type fakeAgentEmailOutboundMCPBackend struct {
	*fakeMCPBackend
	send     client.SendAgentEmailInput
	replyID  string
	reply    client.ReplyAgentEmailInput
	list     client.AgentEmailOutboundListOptions
	shownID  string
	sendErr  error
	replyErr error
	listErr  error
	showErr  error
}

func (b *fakeAgentEmailOutboundMCPBackend) SendAgentEmail(_ context.Context, in client.SendAgentEmailInput) (client.AgentEmailOutboundMessage, error) {
	b.send = in
	if b.sendErr != nil {
		return client.AgentEmailOutboundMessage{}, b.sendErr
	}
	return client.AgentEmailOutboundMessage{ID: "esnd_aaaaaaaaaaaaaaaa", From: "owner.realm@send.witmail.net", To: in.To, Subject: in.Subject, State: "queued"}, nil
}

func (b *fakeAgentEmailOutboundMCPBackend) ReplyAgentEmail(_ context.Context, inboundID string, in client.ReplyAgentEmailInput) (client.AgentEmailOutboundMessage, error) {
	b.replyID, b.reply = inboundID, in
	if b.replyErr != nil {
		return client.AgentEmailOutboundMessage{}, b.replyErr
	}
	return client.AgentEmailOutboundMessage{ID: "esnd_bbbbbbbbbbbbbbbb", ReplyToInboundMessageID: inboundID, State: "queued"}, nil
}

func (b *fakeAgentEmailOutboundMCPBackend) ListSentAgentEmails(_ context.Context, opts client.AgentEmailOutboundListOptions) (client.AgentEmailOutboundPage, error) {
	b.list = opts
	if b.listErr != nil {
		return client.AgentEmailOutboundPage{}, b.listErr
	}
	return client.AgentEmailOutboundPage{Messages: []client.AgentEmailOutboundMessage{{ID: "esnd_aaaaaaaaaaaaaaaa", State: "queued"}}, NextCursor: "after"}, nil
}

func (b *fakeAgentEmailOutboundMCPBackend) GetSentAgentEmail(_ context.Context, id string) (client.AgentEmailOutboundMessage, error) {
	b.shownID = id
	if b.showErr != nil {
		return client.AgentEmailOutboundMessage{}, b.showErr
	}
	return client.AgentEmailOutboundMessage{ID: id, State: "accepted", ProviderState: "accepted"}, nil
}

func TestAgentEmailOutboundMCPToolsAreIndependentFromReceive(t *testing.T) {
	ctx := context.Background()
	backend := &fakeAgentEmailOutboundMCPBackend{fakeMCPBackend: &fakeMCPBackend{}}
	server := newWitselfMCPServer(backend)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	clientSession, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientSession.Close() }()

	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"witself.email.send": false, "witself.email.reply": false,
		"witself.email.sent.list": false, "witself.email.sent.show": false,
	}
	for _, tool := range listed.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
		if tool.Name == "witself.email.send" && (tool.Annotations == nil || tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint) {
			t.Fatalf("email.send annotations = %#v", tool.Annotations)
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("missing %s", name)
		}
	}
	call := func(name string, args map[string]any) {
		t.Helper()
		result, callErr := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if callErr != nil || result.IsError {
			t.Fatalf("%s = %#v, %v", name, result, callErr)
		}
	}
	call("witself.email.send", map[string]any{"to": "person@example.com", "subject": "Hello", "text": "body", "idempotency_key": "send-1"})
	call("witself.email.reply", map[string]any{"inbound_message_id": "emsg_aaaaaaaaaaaaaaaa", "text": "reply", "idempotency_key": "reply-1"})
	call("witself.email.sent.list", map[string]any{"state": "queued", "limit": 7, "cursor": "next"})
	call("witself.email.sent.show", map[string]any{"message_id": "esnd_aaaaaaaaaaaaaaaa"})
	if backend.send.To != "person@example.com" || backend.send.IdempotencyKey != "send-1" ||
		backend.replyID != "emsg_aaaaaaaaaaaaaaaa" || backend.reply.IdempotencyKey != "reply-1" ||
		backend.list.State != "queued" || backend.list.Limit != 7 || backend.list.Cursor != "next" ||
		backend.shownID != "esnd_aaaaaaaaaaaaaaaa" {
		t.Fatalf("backend calls = %#v", backend)
	}
}

func TestAgentEmailOutboundMCPToolsPreserveMachineReadableErrors(t *testing.T) {
	resetAt := time.Date(2026, time.August, 14, 20, 1, 2, 345, time.UTC)
	tests := []struct {
		name      string
		tool      string
		arguments map[string]any
		configure func(*fakeAgentEmailOutboundMCPBackend)
		want      mcpAgentEmailOutboundError
	}{
		{
			name: "feature refusal",
			tool: "witself.email.send",
			arguments: map[string]any{
				"to": "person@example.com", "subject": "Hello", "text": "body",
				"idempotency_key": "send-feature",
			},
			configure: func(backend *fakeAgentEmailOutboundMCPBackend) {
				backend.sendErr = &client.FeatureNotEnabledError{
					Feature: "agent_email_send", Retryable: false,
					Message: "Sorry, this feature is not enabled on this account.",
				}
			},
			want: mcpAgentEmailOutboundError{
				Code: "feature_not_enabled", Feature: "agent_email_send",
				Message: "Sorry, this feature is not enabled on this account.",
			},
		},
		{
			name: "transient rate refusal",
			tool: "witself.email.reply",
			arguments: map[string]any{
				"inbound_message_id": "emsg_aaaaaaaaaaaaaaaa", "text": "reply",
				"idempotency_key": "reply-rate",
			},
			configure: func(backend *fakeAgentEmailOutboundMCPBackend) {
				backend.replyErr = &client.MessageRateLimitError{
					LimitDimension: "agent_email_sent",
					LimitKey:       "agent_email_sent_per_agent_minute",
					Scope:          "agent",
					Limit:          30,
					Used:           30,
					Attempted:      1,
					WindowSeconds:  60,
					RetryAfter:     1500 * time.Millisecond,
					ResetAt:        resetAt,
					Source:         "platform",
					Retryable:      true,
					Message:        "outbound agent-email rate limit reached",
				}
			},
			want: mcpAgentEmailOutboundError{
				Code: "rate_limited", Message: "outbound agent-email rate limit reached",
				Retryable: true, RetryAfter: 2,
				Details: &mcpAgentEmailOutboundErrorDetails{
					LimitDimension: "agent_email_sent",
					LimitKey:       "agent_email_sent_per_agent_minute",
					Scope:          "agent",
					Limit:          30,
					Used:           30,
					Attempted:      1,
					WindowSeconds:  60,
					RetryAfter:     2,
					ResetAt:        resetAt.Format(time.RFC3339Nano),
					Source:         "platform",
				},
			},
		},
		{
			name:      "hard plan limit",
			tool:      "witself.email.sent.list",
			arguments: map[string]any{"limit": 10},
			configure: func(backend *fakeAgentEmailOutboundMCPBackend) {
				backend.listErr = &client.MessageRateLimitError{
					LimitDimension: "agent_email_sent",
					LimitKey:       "agent_email_sent_per_realm_minute",
					Scope:          "realm",
					Limit:          0,
					Used:           0,
					Attempted:      1,
					WindowSeconds:  60,
					Source:         "account_override",
					Retryable:      false,
					Message:        "outbound agent-email rate limit reached",
				}
			},
			want: mcpAgentEmailOutboundError{
				Code: "limit_exceeded", Message: "outbound agent-email rate limit reached",
				Details: &mcpAgentEmailOutboundErrorDetails{
					LimitDimension: "agent_email_sent",
					LimitKey:       "agent_email_sent_per_realm_minute",
					Scope:          "realm",
					Attempted:      1,
					WindowSeconds:  60,
					Source:         "account_override",
				},
			},
		},
		{
			name: "daily recipient breaker",
			tool: "witself.email.send",
			arguments: map[string]any{
				"to": "person@example.com", "subject": "Hello", "text": "body",
				"idempotency_key": "send-daily-rate",
			},
			configure: func(backend *fakeAgentEmailOutboundMCPBackend) {
				backend.sendErr = &client.MessageRateLimitError{
					LimitDimension: "agent_email_sent",
					LimitKey:       "agent_email_sent_per_recipient_day",
					Scope:          "recipient",
					Limit:          100,
					Used:           100,
					Attempted:      1,
					WindowSeconds:  86_400,
					RetryAfter:     time.Minute,
					ResetAt:        resetAt,
					Source:         "platform",
					Retryable:      true,
					Message:        "outbound agent-email rate limit reached",
				}
			},
			want: mcpAgentEmailOutboundError{
				Code: "rate_limited", Message: "outbound agent-email rate limit reached",
				Retryable: true, RetryAfter: 60,
				Details: &mcpAgentEmailOutboundErrorDetails{
					LimitDimension: "agent_email_sent",
					LimitKey:       "agent_email_sent_per_recipient_day",
					Scope:          "recipient",
					Limit:          100,
					Used:           100,
					Attempted:      1,
					WindowSeconds:  86_400,
					RetryAfter:     60,
					ResetAt:        resetAt.Format(time.RFC3339Nano),
					Source:         "platform",
				},
			},
		},
		{
			name:      "specific conflict",
			tool:      "witself.email.sent.show",
			arguments: map[string]any{"message_id": "esnd_aaaaaaaaaaaaaaaa"},
			configure: func(backend *fakeAgentEmailOutboundMCPBackend) {
				backend.showErr = &client.APIError{
					Code: "agent_email_idempotency_conflict", Retryable: false,
					Message: "idempotency key was reused for a different agent email send",
				}
			},
			want: mcpAgentEmailOutboundError{
				Code:    "agent_email_idempotency_conflict",
				Message: "idempotency key was reused for a different agent email send",
			},
		},
		{
			name: "retryable processing conflict",
			tool: "witself.email.send",
			arguments: map[string]any{
				"to": "person@example.com", "subject": "Hello", "text": "body",
				"idempotency_key": "send-busy",
			},
			configure: func(backend *fakeAgentEmailOutboundMCPBackend) {
				backend.sendErr = &client.APIError{
					Code: "agent_email_processing_busy", Retryable: true,
					Message: "email send is already being processed",
				}
			},
			want: mcpAgentEmailOutboundError{
				Code: "agent_email_processing_busy", Retryable: true,
				Message: "email send is already being processed",
			},
		},
		{
			name:      "authentication failure",
			tool:      "witself.email.sent.list",
			arguments: map[string]any{"limit": 10},
			configure: func(backend *fakeAgentEmailOutboundMCPBackend) {
				backend.listErr = client.ErrUnauthorized
			},
			want: mcpAgentEmailOutboundError{
				Code: "auth_failed", Message: client.ErrUnauthorized.Error(),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			backend := &fakeAgentEmailOutboundMCPBackend{fakeMCPBackend: &fakeMCPBackend{}}
			test.configure(backend)
			server := newWitselfMCPServer(backend)
			clientTransport, serverTransport := mcp.NewInMemoryTransports()
			serverSession, err := server.Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = serverSession.Close() }()
			mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			clientSession, err := mcpClient.Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = clientSession.Close() }()

			result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
				Name: test.tool, Arguments: test.arguments,
			})
			if err != nil {
				t.Fatalf("CallTool returned protocol error: %v", err)
			}
			if !result.IsError {
				t.Fatalf("result = %#v, want IsError", result)
			}
			var envelope struct {
				Error *mcpAgentEmailOutboundError `json:"error"`
			}
			raw, err := json.Marshal(result.StructuredContent)
			if err != nil || json.Unmarshal(raw, &envelope) != nil || envelope.Error == nil {
				t.Fatalf("structured error = %s / %#v / %v", raw, result.StructuredContent, err)
			}
			wantJSON, _ := json.Marshal(test.want)
			gotJSON, _ := json.Marshal(envelope.Error)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("error = %s, want %s", gotJSON, wantJSON)
			}
			if len(result.Content) != 1 {
				t.Fatalf("content = %#v", result.Content)
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok || !json.Valid([]byte(text.Text)) {
				t.Fatalf("text content = %#v", result.Content[0])
			}
		})
	}
}
