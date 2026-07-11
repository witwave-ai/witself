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
	ID        string           `json:"id"`
	AccountID string           `json:"account_id"`
	RealmID   string           `json:"realm_id"`
	From      MessageAgent     `json:"from"`
	To        MessageRecipient `json:"to"`
	Subject   string           `json:"subject,omitempty"`
	Kind      string           `json:"kind"`
	Body      string           `json:"body,omitempty"`
	Payload   json.RawMessage  `json:"payload,omitempty"`
	ThreadID  string           `json:"thread_id"`
	CreatedAt time.Time        `json:"created_at"`
	Delivery  MessageDelivery  `json:"delivery"`
	ReadState MessageReadState `json:"read_state"`
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
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
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
	To             string
	Subject        string
	Kind           string
	Body           string
	Payload        json.RawMessage
	ThreadID       string
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

// SendMessage sends one durable realm-local direct message.
func SendMessage(ctx context.Context, endpoint, token string, in SendMessageInput) (Message, error) {
	request := struct {
		To       map[string]string `json:"to"`
		Subject  string            `json:"subject,omitempty"`
		Kind     string            `json:"kind,omitempty"`
		Body     string            `json:"body"`
		Payload  json.RawMessage   `json:"payload,omitempty"`
		ThreadID string            `json:"thread_id,omitempty"`
	}{
		To:      map[string]string{"kind": "agent", "id": in.To},
		Subject: in.Subject, Kind: in.Kind, Body: in.Body,
		Payload: in.Payload, ThreadID: in.ThreadID,
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
	url := messagesURL(endpoint) + "/" + neturl.PathEscape(messageID) + ":" + action
	if err := doJSON(ctx, http.MethodPost, url, token, []byte(`{}`), &out); err != nil {
		return Message{}, err
	}
	return out.Message, nil
}

func messagesURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/messages"
}
