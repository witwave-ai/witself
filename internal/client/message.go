package client

import (
	"context"
	"encoding/json"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

// Message is one durable direct message. Body and Payload are absent from list
// results and present after send/read.
type Message struct {
	ID               string            `json:"id"`
	AccountID        string            `json:"account_id"`
	RealmID          string            `json:"realm_id"`
	From             MessageAgent      `json:"from"`
	To               MessageRecipient  `json:"to"`
	Subject          string            `json:"subject,omitempty"`
	Kind             string            `json:"kind"`
	Body             string            `json:"body,omitempty"`
	Payload          json.RawMessage   `json:"payload,omitempty"`
	ThreadID         string            `json:"thread_id"`
	ReplyToMessageID string            `json:"reply_to_message_id,omitempty"`
	CausalDepth      int64             `json:"causal_depth"`
	CreatedAt        time.Time         `json:"created_at"`
	Delivery         MessageDelivery   `json:"delivery"`
	ReadState        MessageReadState  `json:"read_state"`
	Processing       MessageProcessing `json:"processing"`
}

// MessageAgent identifies a token-derived message sender.
type MessageAgent struct {
	Kind      string `json:"kind"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
}

// MessageRecipient identifies a resolved direct recipient.
type MessageRecipient struct {
	Kind      string `json:"kind"`
	AgentID   string `json:"agent_id,omitempty"`
	AgentName string `json:"agent_name,omitempty"`
	Count     int    `json:"count,omitempty"`
}

// MessageDelivery is the recipient delivery state.
type MessageDelivery struct {
	State       string     `json:"state"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

// MessageReadState is the recipient's unread/read/acked state.
type MessageReadState struct {
	State   string     `json:"state"`
	ReadAt  *time.Time `json:"read_at,omitempty"`
	AckedAt *time.Time `json:"acked_at,omitempty"`
}

// SendMessageInput carries one direct send and its optional retry key.
type SendMessageInput struct {
	AudienceKind   string
	To             string
	ToAgents       []string
	Subject        string
	Kind           string
	Body           string
	Payload        json.RawMessage
	ThreadID       string
	IdempotencyKey string
}

// ReplyMessageInput carries reply content and one retry key. The server derives
// the recipient, realm, thread, and causal parent from the URL parent message.
type ReplyMessageInput struct {
	Subject        string
	Kind           string
	Body           string
	Payload        json.RawMessage
	IdempotencyKey string
}

// MessageListOptions selects one inbox or outbox page.
type MessageListOptions struct {
	Direction string
	Unread    bool
	From      string
	ThreadID  string
	Kind      string
	Limit     int
	Cursor    string
}

// MessagePage is one metadata-only mailbox page.
type MessagePage struct {
	Messages   []Message `json:"messages"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// MessageListenOptions selects oldest unacknowledged inbound metadata and a
// bounded server-side wait. Listen is not a claim and changes no state.
type MessageListenOptions struct {
	WaitSeconds *int
	From        string
	ThreadID    string
	Kind        string
	Limit       int
}

// MessageListenResult is one non-consuming waitable mailbox result.
type MessageListenResult struct {
	Messages []Message `json:"messages"`
	TimedOut bool      `json:"timed_out"`
}

// MessageProcessing is the fenced processing state for one inbound message.
type MessageProcessing struct {
	State           string     `json:"state"`
	ClaimID         string     `json:"claim_id,omitempty"`
	Generation      int64      `json:"generation"`
	FailureCount    int64      `json:"failure_count"`
	LeaseExpiresAt  *time.Time `json:"lease_expires_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	ResultMessageID string     `json:"result_message_id,omitempty"`
}

// ClaimMessageInput requests one bounded processing lease.
type ClaimMessageInput struct {
	LeaseSeconds   int
	IdempotencyKey string
}

// MessageClaimInput identifies one exact claim generation.
type MessageClaimInput struct {
	ClaimID              string
	Generation           int64
	DeterministicFailure bool
}

// RenewMessageClaimInput extends one exact claim generation.
type RenewMessageClaimInput struct {
	ClaimID      string
	Generation   int64
	LeaseSeconds int
}

// CompleteMessageInput atomically finishes a claim and creates its result
// reply. The server derives the reply recipient, thread, and causal parent.
type CompleteMessageInput struct {
	ClaimID        string
	Generation     int64
	Subject        string
	Kind           string
	Body           string
	Payload        json.RawMessage
	IdempotencyKey string
}

// CompleteMessageResult is the completed processing state and durable reply.
type CompleteMessageResult struct {
	Processing MessageProcessing `json:"processing"`
	Message    Message           `json:"message"`
}

// SendMessage sends one durable realm-local direct message.
func SendMessage(ctx context.Context, endpoint, token string, in SendMessageInput) (Message, error) {
	audienceKind := strings.TrimSpace(in.AudienceKind)
	if audienceKind == "" {
		audienceKind = "agent"
	}
	request := struct {
		To       map[string]any  `json:"to"`
		Subject  string          `json:"subject,omitempty"`
		Kind     string          `json:"kind,omitempty"`
		Body     string          `json:"body"`
		Payload  json.RawMessage `json:"payload,omitempty"`
		ThreadID string          `json:"thread_id,omitempty"`
	}{
		To:      map[string]any{"kind": audienceKind},
		Subject: in.Subject, Kind: in.Kind, Body: in.Body,
		Payload: in.Payload, ThreadID: in.ThreadID,
	}
	switch audienceKind {
	case "agent":
		request.To["id"] = in.To
	case "agents":
		request.To["ids"] = in.ToAgents
	}
	body, err := json.Marshal(request)
	if err != nil {
		return Message{}, err
	}
	headers := map[string]string{}
	if in.IdempotencyKey != "" {
		headers["Idempotency-Key"] = in.IdempotencyKey
	}
	var out struct {
		Message Message `json:"message"`
	}
	if err := doJSONWithHeaders(ctx, http.MethodPost, messagesURL(endpoint), token, headers, body, &out); err != nil {
		return Message{}, err
	}
	return out.Message, nil
}

// ReplyMessage sends one recipient-only reply to an inbound parent message.
func ReplyMessage(ctx context.Context, endpoint, token, parentMessageID string, in ReplyMessageInput) (Message, error) {
	request := struct {
		Subject string          `json:"subject,omitempty"`
		Kind    string          `json:"kind,omitempty"`
		Body    string          `json:"body"`
		Payload json.RawMessage `json:"payload,omitempty"`
	}{Subject: in.Subject, Kind: in.Kind, Body: in.Body, Payload: in.Payload}
	body, err := json.Marshal(request)
	if err != nil {
		return Message{}, err
	}
	headers := map[string]string{}
	if in.IdempotencyKey != "" {
		headers["Idempotency-Key"] = in.IdempotencyKey
	}
	var out struct {
		Message Message `json:"message"`
	}
	url := messagesURL(endpoint) + "/" + neturl.PathEscape(parentMessageID) + ":reply"
	if err := doJSONWithHeaders(ctx, http.MethodPost, url, token, headers, body, &out); err != nil {
		return Message{}, err
	}
	return out.Message, nil
}

// ClaimMessage starts or idempotently resumes a processing claim.
func ClaimMessage(ctx context.Context, endpoint, token, messageID string, in ClaimMessageInput) (MessageProcessing, error) {
	request := struct {
		LeaseSeconds int `json:"lease_seconds"`
	}{LeaseSeconds: in.LeaseSeconds}
	body, err := json.Marshal(request)
	if err != nil {
		return MessageProcessing{}, err
	}
	headers := map[string]string{}
	if in.IdempotencyKey != "" {
		headers["Idempotency-Key"] = in.IdempotencyKey
	}
	var out struct {
		Processing MessageProcessing `json:"processing"`
	}
	if err := doJSONWithHeaders(ctx, http.MethodPost, messageActionURL(endpoint, messageID, "claim"), token, headers, body, &out); err != nil {
		return MessageProcessing{}, err
	}
	return out.Processing, nil
}

// RenewMessageClaim extends one exact processing claim generation.
func RenewMessageClaim(ctx context.Context, endpoint, token, messageID string, in RenewMessageClaimInput) (MessageProcessing, error) {
	request := struct {
		ClaimID      string `json:"claim_id"`
		Generation   int64  `json:"generation"`
		LeaseSeconds int    `json:"lease_seconds"`
	}{ClaimID: in.ClaimID, Generation: in.Generation, LeaseSeconds: in.LeaseSeconds}
	body, err := json.Marshal(request)
	if err != nil {
		return MessageProcessing{}, err
	}
	var out struct {
		Processing MessageProcessing `json:"processing"`
	}
	if err := doJSON(ctx, http.MethodPost, messageActionURL(endpoint, messageID, "renew"), token, body, &out); err != nil {
		return MessageProcessing{}, err
	}
	return out.Processing, nil
}

// ReleaseMessageClaim releases one exact processing claim generation.
func ReleaseMessageClaim(ctx context.Context, endpoint, token, messageID string, in MessageClaimInput) (MessageProcessing, error) {
	request := struct {
		ClaimID              string `json:"claim_id"`
		Generation           int64  `json:"generation"`
		DeterministicFailure bool   `json:"deterministic_failure,omitempty"`
	}{
		ClaimID: in.ClaimID, Generation: in.Generation,
		DeterministicFailure: in.DeterministicFailure,
	}
	body, err := json.Marshal(request)
	if err != nil {
		return MessageProcessing{}, err
	}
	var out struct {
		Processing MessageProcessing `json:"processing"`
	}
	if err := doJSON(ctx, http.MethodPost, messageActionURL(endpoint, messageID, "release"), token, body, &out); err != nil {
		return MessageProcessing{}, err
	}
	return out.Processing, nil
}

// CompleteMessage atomically completes one claim and creates its result reply.
func CompleteMessage(ctx context.Context, endpoint, token, messageID string, in CompleteMessageInput) (CompleteMessageResult, error) {
	request := struct {
		ClaimID    string          `json:"claim_id"`
		Generation int64           `json:"generation"`
		Subject    string          `json:"subject,omitempty"`
		Kind       string          `json:"kind,omitempty"`
		Body       string          `json:"body"`
		Payload    json.RawMessage `json:"payload,omitempty"`
	}{
		ClaimID: in.ClaimID, Generation: in.Generation,
		Subject: in.Subject, Kind: in.Kind, Body: in.Body, Payload: in.Payload,
	}
	body, err := json.Marshal(request)
	if err != nil {
		return CompleteMessageResult{}, err
	}
	headers := map[string]string{}
	if in.IdempotencyKey != "" {
		headers["Idempotency-Key"] = in.IdempotencyKey
	}
	var out CompleteMessageResult
	if err := doJSONWithHeaders(ctx, http.MethodPost, messageActionURL(endpoint, messageID, "complete"), token, headers, body, &out); err != nil {
		return CompleteMessageResult{}, err
	}
	return out, nil
}

// ListMessages returns one metadata-only mailbox page without marking read.
func ListMessages(ctx context.Context, endpoint, token string, opts MessageListOptions) (MessagePage, error) {
	params := neturl.Values{}
	if opts.Direction != "" {
		params.Set("direction", opts.Direction)
	}
	if opts.Unread {
		params.Set("unread", "true")
	}
	if opts.From != "" {
		params.Set("from", opts.From)
	}
	if opts.ThreadID != "" {
		params.Set("thread_id", opts.ThreadID)
	}
	if opts.Kind != "" {
		params.Set("kind", opts.Kind)
	}
	if opts.Limit != 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		params.Set("cursor", opts.Cursor)
	}
	url := messagesURL(endpoint)
	if len(params) != 0 {
		url += "?" + params.Encode()
	}
	var out MessagePage
	if err := doJSON(ctx, http.MethodGet, url, token, nil, &out); err != nil {
		return MessagePage{}, err
	}
	return out, nil
}

// ListenMessages waits for oldest unacknowledged inbound metadata. The client
// transport timeout includes headroom beyond the server-side wait while an
// earlier caller context deadline still wins.
func ListenMessages(ctx context.Context, endpoint, token string, opts MessageListenOptions) (MessageListenResult, error) {
	request := struct {
		WaitSeconds *int   `json:"wait_seconds,omitempty"`
		FromAgent   string `json:"from_agent,omitempty"`
		ThreadID    string `json:"thread_id,omitempty"`
		Kind        string `json:"kind,omitempty"`
		Limit       int    `json:"limit,omitempty"`
	}{
		WaitSeconds: opts.WaitSeconds, FromAgent: opts.From,
		ThreadID: opts.ThreadID, Kind: opts.Kind, Limit: opts.Limit,
	}
	body, err := json.Marshal(request)
	if err != nil {
		return MessageListenResult{}, err
	}
	var out MessageListenResult
	if err := doJSONWithHeadersTimeout(ctx, http.MethodPost, messagesURL(endpoint)+":listen", token, nil, body, &out, messageListenTransportTimeout(opts)); err != nil {
		return MessageListenResult{}, err
	}
	if out.Messages == nil {
		out.Messages = []Message{}
	}
	return out, nil
}

func messageListenTransportTimeout(opts MessageListenOptions) time.Duration {
	effectiveWaitSeconds := 20
	if opts.WaitSeconds != nil {
		effectiveWaitSeconds = *opts.WaitSeconds
	}
	timeout := 15 * time.Second
	if candidate := time.Duration(effectiveWaitSeconds+5) * time.Second; candidate > timeout {
		timeout = candidate
	}
	return timeout
}

// ReadMessage returns recipient-visible content and marks the message read.
func ReadMessage(ctx context.Context, endpoint, token, messageID string) (Message, error) {
	return messageAction(ctx, endpoint, token, messageID, "read")
}

// AckMessage marks a recipient message read and acknowledged.
func AckMessage(ctx context.Context, endpoint, token, messageID string) (Message, error) {
	return messageAction(ctx, endpoint, token, messageID, "ack")
}

func messageAction(ctx context.Context, endpoint, token, messageID, action string) (Message, error) {
	var out struct {
		Message Message `json:"message"`
	}
	url := messageActionURL(endpoint, messageID, action)
	if err := doJSON(ctx, http.MethodPost, url, token, []byte(`{}`), &out); err != nil {
		return Message{}, err
	}
	return out.Message, nil
}

func messageActionURL(endpoint, messageID, action string) string {
	return messagesURL(endpoint) + "/" + neturl.PathEscape(messageID) + ":" + action
}

func messagesURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/messages"
}
