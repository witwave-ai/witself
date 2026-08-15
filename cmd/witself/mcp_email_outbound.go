package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/witwave-ai/witself/internal/client"
)

const maxMCPAgentEmailOutboundTextBytes = 128 * 1024

// mcpAgentEmailOutboundBackend is deliberately independent from the inbound
// mailbox interface. A runtime can advertise outbound email without enabling
// receive, and plan changes take effect server-side without reinstalling MCP.
type mcpAgentEmailOutboundBackend interface {
	SendAgentEmail(context.Context, client.SendAgentEmailInput) (client.AgentEmailOutboundMessage, error)
	ReplyAgentEmail(context.Context, string, client.ReplyAgentEmailInput) (client.AgentEmailOutboundMessage, error)
	ListSentAgentEmails(context.Context, client.AgentEmailOutboundListOptions) (client.AgentEmailOutboundPage, error)
	GetSentAgentEmail(context.Context, string) (client.AgentEmailOutboundMessage, error)
}

func (b configuredMCPBackend) SendAgentEmail(ctx context.Context, in client.SendAgentEmailInput) (client.AgentEmailOutboundMessage, error) {
	conn, err := b.connect(ctx)
	if err != nil {
		return client.AgentEmailOutboundMessage{}, err
	}
	return client.SendAgentEmail(ctx, conn.Endpoint, conn.Token, in)
}

func (b configuredMCPBackend) ReplyAgentEmail(ctx context.Context, inboundID string, in client.ReplyAgentEmailInput) (client.AgentEmailOutboundMessage, error) {
	conn, err := b.connect(ctx)
	if err != nil {
		return client.AgentEmailOutboundMessage{}, err
	}
	return client.ReplyAgentEmail(ctx, conn.Endpoint, conn.Token, inboundID, in)
}

func (b configuredMCPBackend) ListSentAgentEmails(ctx context.Context, opts client.AgentEmailOutboundListOptions) (client.AgentEmailOutboundPage, error) {
	conn, err := b.connect(ctx)
	if err != nil {
		return client.AgentEmailOutboundPage{}, err
	}
	return client.ListSentAgentEmails(ctx, conn.Endpoint, conn.Token, opts)
}

func (b configuredMCPBackend) GetSentAgentEmail(ctx context.Context, messageID string) (client.AgentEmailOutboundMessage, error) {
	conn, err := b.connect(ctx)
	if err != nil {
		return client.AgentEmailOutboundMessage{}, err
	}
	return client.GetSentAgentEmail(ctx, conn.Endpoint, conn.Token, messageID)
}

type mcpAgentEmailSendInput struct {
	To             string `json:"to" jsonschema:"required single recipient email address"`
	Subject        string `json:"subject" jsonschema:"required email subject"`
	Text           string `json:"text" jsonschema:"required plain-text email body"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"required retry key for exactly one logical external send"`
}

type mcpAgentEmailReplyInput struct {
	InboundMessageID string `json:"inbound_message_id" jsonschema:"required inbound agent email id beginning with emsg_; recipient and subject are server-derived"`
	Text             string `json:"text" jsonschema:"required plain-text reply body"`
	IdempotencyKey   string `json:"idempotency_key" jsonschema:"required retry key for exactly one logical external reply"`
}

type mcpAgentEmailSentListInput struct {
	State  string `json:"state,omitempty" jsonschema:"optional outbound state filter"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum messages to return from 1 to 100; defaults to 50"`
	Cursor string `json:"cursor,omitempty" jsonschema:"opaque continuation cursor"`
}

type mcpAgentEmailSentShowInput struct {
	MessageID string `json:"message_id" jsonschema:"outbound agent email id beginning with esnd_"`
}

type mcpAgentEmailOutboundOutput struct {
	Message *client.AgentEmailOutboundMessage `json:"message,omitempty"`
	Error   *mcpAgentEmailOutboundError       `json:"error,omitempty"`
}

type mcpAgentEmailSentListOutput struct {
	Messages   *[]client.AgentEmailOutboundMessage `json:"messages,omitempty"`
	NextCursor string                              `json:"next_cursor,omitempty"`
	Error      *mcpAgentEmailOutboundError         `json:"error,omitempty"`
}

// mcpAgentEmailOutboundError is the value-free machine result returned for an
// outbound backend refusal. Returning it with CallToolResult.IsError preserves
// retry and limit guidance across MCP instead of collapsing typed HTTP client
// errors into one text block. This envelope is intentionally local to outbound
// agent email until other MCP surfaces adopt an independently reviewed common
// error contract.
type mcpAgentEmailOutboundError struct {
	Code       string                             `json:"code"`
	Message    string                             `json:"message"`
	Retryable  bool                               `json:"retryable"`
	Feature    string                             `json:"feature,omitempty"`
	RetryAfter int64                              `json:"retry_after,omitempty"`
	Details    *mcpAgentEmailOutboundErrorDetails `json:"details,omitempty"`
}

// mcpAgentEmailOutboundErrorDetails carries only closed, value-free rate
// metadata. Identifiers, recipient addresses, subjects, and message content are
// deliberately absent.
type mcpAgentEmailOutboundErrorDetails struct {
	LimitDimension string `json:"limit_dimension"`
	LimitKey       string `json:"limit_key"`
	Scope          string `json:"scope"`
	Limit          int64  `json:"limit"`
	Used           int64  `json:"used"`
	Attempted      int64  `json:"attempted"`
	WindowSeconds  int64  `json:"window_seconds"`
	RetryAfter     int64  `json:"retry_after,omitempty"`
	ResetAt        string `json:"reset_at,omitempty"`
	Source         string `json:"source,omitempty"`
}

func registerAgentEmailOutboundMCPTools(server *mcp.Server, runtimeName string, backend mcpAgentEmailOutboundBackend) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        mcpToolName(runtimeName, "witself.email.send"),
		Description: "Queue one irreversible external plain-text email from this token-bound agent's server-owned address to one recipient. From cannot be supplied or spoofed. Use one new idempotency key per logical send and reuse that exact key for every retry of the same send. Account policy may return feature_not_enabled without requiring MCP reinstallation.",
		Annotations: mcpAgentEmailExternalWriteAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpAgentEmailSendInput) (*mcp.CallToolResult, mcpAgentEmailOutboundOutput, error) {
		if err := validateMCPAgentEmailOutboundText(in.Text); err != nil {
			return nil, mcpAgentEmailOutboundOutput{}, err
		}
		if strings.TrimSpace(in.To) == "" || strings.TrimSpace(in.Subject) == "" || strings.TrimSpace(in.IdempotencyKey) == "" {
			return nil, mcpAgentEmailOutboundOutput{}, fmt.Errorf("to, subject, text, and idempotency_key are required")
		}
		message, err := backend.SendAgentEmail(ctx, client.SendAgentEmailInput{
			To: strings.TrimSpace(in.To), Subject: strings.TrimSpace(in.Subject), Text: in.Text,
			IdempotencyKey: strings.TrimSpace(in.IdempotencyKey),
		})
		if err != nil {
			result, toolErr := mcpAgentEmailOutboundToolError(err)
			return result, mcpAgentEmailOutboundOutput{Error: toolErr}, nil
		}
		return nil, mcpAgentEmailOutboundOutput{Message: &message}, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        mcpToolName(runtimeName, "witself.email.reply"),
		Description: "Queue one irreversible external plain-text reply to an inbound email. The server derives recipient, subject/thread provenance, and From from the exact owner-visible inbound message; callers cannot override them. Use one new idempotency key per logical reply and reuse that exact key for every retry of the same reply.",
		Annotations: mcpAgentEmailExternalWriteAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpAgentEmailReplyInput) (*mcp.CallToolResult, mcpAgentEmailOutboundOutput, error) {
		messageID, err := normalizeMCPAgentEmailID(in.InboundMessageID)
		if err != nil {
			return nil, mcpAgentEmailOutboundOutput{}, err
		}
		if err := validateMCPAgentEmailOutboundText(in.Text); err != nil {
			return nil, mcpAgentEmailOutboundOutput{}, err
		}
		if strings.TrimSpace(in.IdempotencyKey) == "" {
			return nil, mcpAgentEmailOutboundOutput{}, fmt.Errorf("idempotency_key is required")
		}
		message, err := backend.ReplyAgentEmail(ctx, messageID, client.ReplyAgentEmailInput{
			Text: in.Text, IdempotencyKey: strings.TrimSpace(in.IdempotencyKey),
		})
		if err != nil {
			result, toolErr := mcpAgentEmailOutboundToolError(err)
			return result, mcpAgentEmailOutboundOutput{Error: toolErr}, nil
		}
		return nil, mcpAgentEmailOutboundOutput{Message: &message}, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        mcpToolName(runtimeName, "witself.email.sent.list"),
		Description: "List this token-bound agent's metadata-only outbound email history. Submitted body text and provider message identifiers are never returned.",
		Annotations: mcpReadOnlyClosedWorldAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpAgentEmailSentListInput) (*mcp.CallToolResult, mcpAgentEmailSentListOutput, error) {
		limit, err := normalizeMCPAgentEmailLimit(in.Limit)
		if err != nil {
			return nil, mcpAgentEmailSentListOutput{}, err
		}
		page, err := backend.ListSentAgentEmails(ctx, client.AgentEmailOutboundListOptions{
			State: strings.TrimSpace(in.State), Limit: limit, Cursor: in.Cursor,
		})
		if err != nil {
			result, toolErr := mcpAgentEmailOutboundToolError(err)
			return result, mcpAgentEmailSentListOutput{Error: toolErr}, nil
		}
		return nil, mcpAgentEmailSentListOutput{Messages: &page.Messages, NextCursor: page.NextCursor}, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        mcpToolName(runtimeName, "witself.email.sent.show"),
		Description: "Show one metadata-only outbound email owned by this token-bound agent. Submitted body text and provider message identifiers are never returned.",
		Annotations: mcpReadOnlyClosedWorldAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpAgentEmailSentShowInput) (*mcp.CallToolResult, mcpAgentEmailOutboundOutput, error) {
		messageID := strings.TrimSpace(in.MessageID)
		if !validMCPAgentEmailGeneratedID(messageID, "esnd") {
			return nil, mcpAgentEmailOutboundOutput{}, fmt.Errorf("message_id must be a valid esnd_ id")
		}
		message, err := backend.GetSentAgentEmail(ctx, messageID)
		if err != nil {
			result, toolErr := mcpAgentEmailOutboundToolError(err)
			return result, mcpAgentEmailOutboundOutput{Error: toolErr}, nil
		}
		return nil, mcpAgentEmailOutboundOutput{Message: &message}, nil
	})
}

func mcpAgentEmailOutboundToolError(err error) (*mcp.CallToolResult, *mcpAgentEmailOutboundError) {
	toolErr := &mcpAgentEmailOutboundError{
		Code: "backend_unavailable", Message: strings.TrimSpace(err.Error()), Retryable: true,
	}
	if toolErr.Message == "" {
		toolErr.Message = "outbound agent email is unavailable"
	}

	var featureErr *client.FeatureNotEnabledError
	var rateErr *client.MessageRateLimitError
	var apiErr *client.APIError
	switch {
	case errors.As(err, &featureErr):
		toolErr.Code = "feature_not_enabled"
		toolErr.Retryable = featureErr.Retryable
		toolErr.Feature = featureErr.Feature
	case errors.As(err, &rateErr):
		toolErr.Code = "limit_exceeded"
		toolErr.Retryable = rateErr.Retryable
		toolErr.Details = &mcpAgentEmailOutboundErrorDetails{
			LimitDimension: rateErr.LimitDimension,
			LimitKey:       rateErr.LimitKey,
			Scope:          rateErr.Scope,
			Limit:          rateErr.Limit,
			Used:           rateErr.Used,
			Attempted:      rateErr.Attempted,
			WindowSeconds:  rateErr.WindowSeconds,
			Source:         rateErr.Source,
		}
		if rateErr.Retryable {
			toolErr.Code = "rate_limited"
			toolErr.RetryAfter = mcpAgentEmailOutboundRetryAfterSeconds(rateErr.RetryAfter)
			toolErr.Details.RetryAfter = toolErr.RetryAfter
		}
		if !rateErr.ResetAt.IsZero() {
			toolErr.Details.ResetAt = rateErr.ResetAt.UTC().Format(time.RFC3339Nano)
		}
	case errors.As(err, &apiErr):
		if code := strings.TrimSpace(apiErr.Code); code != "" {
			toolErr.Code = code
		}
		toolErr.Retryable = apiErr.Retryable
	case errors.Is(err, client.ErrUnauthorized):
		toolErr.Code, toolErr.Retryable = "auth_failed", false
	case errors.Is(err, client.ErrForbidden):
		toolErr.Code, toolErr.Retryable = "forbidden", false
	case errors.Is(err, client.ErrNotFound):
		toolErr.Code, toolErr.Retryable = "not_found", false
	case errors.Is(err, client.ErrConflict):
		toolErr.Code, toolErr.Retryable = "conflict", false
	case errors.Is(err, client.ErrBadRequest):
		toolErr.Code, toolErr.Retryable = "invalid_request", false
	}
	return &mcp.CallToolResult{IsError: true}, toolErr
}

func mcpAgentEmailOutboundRetryAfterSeconds(retryAfter time.Duration) int64 {
	if retryAfter <= 0 {
		return 1
	}
	seconds := int64(retryAfter / time.Second)
	if retryAfter%time.Second != 0 {
		seconds++
	}
	return seconds
}

func validateMCPAgentEmailOutboundText(text string) error {
	if text == "" {
		return fmt.Errorf("text is required")
	}
	if !utf8.ValidString(text) {
		return fmt.Errorf("text must be valid UTF-8")
	}
	if len(text) > maxMCPAgentEmailOutboundTextBytes {
		return fmt.Errorf("text exceeds %d bytes", maxMCPAgentEmailOutboundTextBytes)
	}
	return nil
}

func mcpAgentEmailExternalWriteAnnotations() *mcp.ToolAnnotations {
	destructive := true
	openWorld := true
	return &mcp.ToolAnnotations{
		DestructiveHint: &destructive,
		IdempotentHint:  true,
		OpenWorldHint:   &openWorld,
	}
}
