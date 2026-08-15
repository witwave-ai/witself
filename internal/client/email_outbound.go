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

// AgentEmailOutboundMessage is a content-minimal sent-mail projection. From
// is server-derived, and neither list nor show returns the submitted text.
type AgentEmailOutboundMessage struct {
	ID                      string     `json:"id"`
	AccountID               string     `json:"account_id,omitempty"`
	RealmID                 string     `json:"realm_id,omitempty"`
	OwnerAgentID            string     `json:"owner_agent_id"`
	From                    string     `json:"from"`
	ReplyTo                 string     `json:"reply_to,omitempty"`
	To                      string     `json:"to"`
	Subject                 string     `json:"subject,omitempty"`
	State                   string     `json:"state"`
	ProviderState           string     `json:"provider_state,omitempty"`
	Provider                string     `json:"provider,omitempty"`
	ErrorCode               string     `json:"error_code,omitempty"`
	RequestKind             string     `json:"request_kind,omitempty"`
	ReplyToInboundMessageID string     `json:"reply_to_inbound_message_id,omitempty"`
	ThreadKey               string     `json:"thread_key,omitempty"`
	AttemptCount            int64      `json:"attempt_count"`
	QueuedAt                time.Time  `json:"queued_at"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
	ProviderStartedAt       *time.Time `json:"provider_started_at,omitempty"`
	AcceptedAt              *time.Time `json:"accepted_at,omitempty"`
	DeliveredAt             *time.Time `json:"delivered_at,omitempty"`
	DeferredAt              *time.Time `json:"deferred_at,omitempty"`
	FailedAt                *time.Time `json:"failed_at,omitempty"`
	AmbiguousAt             *time.Time `json:"ambiguous_at,omitempty"`
	CanceledAt              *time.Time `json:"canceled_at,omitempty"`
}

// SendAgentEmailInput queues a single-recipient plain-text message. A From
// field intentionally does not exist.
type SendAgentEmailInput struct {
	To             string
	Subject        string
	Text           string
	IdempotencyKey string
}

// ReplyAgentEmailInput queues a server-addressed plain-text reply.
type ReplyAgentEmailInput struct {
	Text           string
	IdempotencyKey string
}

// AgentEmailOutboundListOptions selects one bounded sent-mail page.
type AgentEmailOutboundListOptions struct {
	State  string
	Limit  int
	Cursor string
}

// AgentEmailOutboundPage is one metadata-only sent-mail page.
type AgentEmailOutboundPage struct {
	Messages   []AgentEmailOutboundMessage `json:"messages"`
	NextCursor string                      `json:"next_cursor,omitempty"`
}

// AgentEmailSendControl is the value-free per-agent send switch.
type AgentEmailSendControl struct {
	AccountID       string     `json:"account_id"`
	RealmID         string     `json:"realm_id"`
	AgentID         string     `json:"agent_id"`
	SendState       string     `json:"send_state"`
	AgentSendState  string     `json:"agent_send_state"`
	RealmSendState  string     `json:"realm_send_state"`
	RowVersion      int64      `json:"row_version"`
	RealmRowVersion int64      `json:"realm_row_version"`
	UpdatedAt       time.Time  `json:"updated_at"`
	DisabledAt      *time.Time `json:"disabled_at,omitempty"`
	RealmDisabledAt *time.Time `json:"realm_disabled_at,omitempty"`
}

// AgentEmailRealmSendControl is the realm-level send kill switch.
type AgentEmailRealmSendControl struct {
	AccountID  string     `json:"account_id"`
	RealmID    string     `json:"realm_id"`
	SendState  string     `json:"send_state"`
	AgentCount int64      `json:"agent_count"`
	RowVersion int64      `json:"row_version"`
	UpdatedAt  time.Time  `json:"updated_at"`
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
}

// SendAgentEmail queues one new outbound email.
func SendAgentEmail(ctx context.Context, endpoint, token string, in SendAgentEmailInput) (AgentEmailOutboundMessage, error) {
	body, err := json.Marshal(struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
		Text    string `json:"text"`
	}{To: in.To, Subject: in.Subject, Text: in.Text})
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	return agentEmailOutboundMutation(ctx, token, agentEmailURL(endpoint)+":send", in.IdempotencyKey, body)
}

// ReplyAgentEmail queues a reply whose recipient and thread provenance are
// derived by the server from the inbound message.
func ReplyAgentEmail(ctx context.Context, endpoint, token, inboundMessageID string, in ReplyAgentEmailInput) (AgentEmailOutboundMessage, error) {
	body, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: in.Text})
	if err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	url := agentEmailURL(endpoint) + "/" + neturl.PathEscape(inboundMessageID) + ":reply"
	return agentEmailOutboundMutation(ctx, token, url, in.IdempotencyKey, body)
}

func agentEmailOutboundMutation(ctx context.Context, token, url, idempotencyKey string, body []byte) (AgentEmailOutboundMessage, error) {
	var out struct {
		Message AgentEmailOutboundMessage `json:"message"`
	}
	headers := map[string]string{}
	if idempotencyKey != "" {
		headers["Idempotency-Key"] = idempotencyKey
	}
	if err := doJSONWithHeaders(ctx, http.MethodPost, url, token, headers, body, &out); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	return out.Message, nil
}

// ListSentAgentEmails returns a metadata-only owner outbox page.
func ListSentAgentEmails(ctx context.Context, endpoint, token string, opts AgentEmailOutboundListOptions) (AgentEmailOutboundPage, error) {
	params := neturl.Values{}
	if opts.State != "" {
		params.Set("state", opts.State)
	}
	if opts.Limit != 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		params.Set("cursor", opts.Cursor)
	}
	url := agentEmailURL(endpoint) + "/sent"
	if len(params) != 0 {
		url += "?" + params.Encode()
	}
	var out AgentEmailOutboundPage
	if err := doJSON(ctx, http.MethodGet, url, token, nil, &out); err != nil {
		return AgentEmailOutboundPage{}, err
	}
	if out.Messages == nil {
		out.Messages = []AgentEmailOutboundMessage{}
	}
	return out, nil
}

// GetSentAgentEmail returns one content-minimal owner outbox record.
func GetSentAgentEmail(ctx context.Context, endpoint, token, messageID string) (AgentEmailOutboundMessage, error) {
	var out struct {
		Message AgentEmailOutboundMessage `json:"message"`
	}
	url := agentEmailURL(endpoint) + "/sent/" + neturl.PathEscape(messageID)
	if err := doJSON(ctx, http.MethodGet, url, token, nil, &out); err != nil {
		return AgentEmailOutboundMessage{}, err
	}
	return out.Message, nil
}

// GetAgentEmailSendControl returns one account agent's send switch.
func GetAgentEmailSendControl(ctx context.Context, endpoint, token, agentID string) (AgentEmailSendControl, error) {
	var out struct {
		Control AgentEmailSendControl `json:"control"`
	}
	if err := doJSON(ctx, http.MethodGet, agentEmailAgentSendURL(endpoint, agentID), token, nil, &out); err != nil {
		return AgentEmailSendControl{}, err
	}
	return out.Control, nil
}

// SetAgentEmailSendControl changes one agent's send layer.
func SetAgentEmailSendControl(ctx context.Context, endpoint, token, agentID, sendState string) (AgentEmailSendControl, error) {
	return SetAgentEmailSendControlExact(ctx, endpoint, token, agentID, sendState, 0)
}

// SetAgentEmailSendControlExact optionally fences the write to an observed
// row version. Zero requests the documented unconditional operator write.
func SetAgentEmailSendControlExact(ctx context.Context, endpoint, token, agentID, sendState string, expectedRowVersion int64) (AgentEmailSendControl, error) {
	body, err := json.Marshal(struct {
		SendState          string `json:"send_state"`
		ExpectedRowVersion int64  `json:"expected_row_version,omitempty"`
	}{SendState: sendState, ExpectedRowVersion: expectedRowVersion})
	if err != nil {
		return AgentEmailSendControl{}, err
	}
	var out struct {
		Control AgentEmailSendControl `json:"control"`
	}
	if err := doJSON(ctx, http.MethodPatch, agentEmailAgentSendURL(endpoint, agentID), token, body, &out); err != nil {
		return AgentEmailSendControl{}, err
	}
	return out.Control, nil
}

// GetRealmAgentEmailSendControl returns one realm's send kill switch.
func GetRealmAgentEmailSendControl(ctx context.Context, endpoint, token, realmID string) (AgentEmailRealmSendControl, error) {
	var out struct {
		Control AgentEmailRealmSendControl `json:"control"`
	}
	if err := doJSON(ctx, http.MethodGet, agentEmailRealmSendURL(endpoint, realmID), token, nil, &out); err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	return out.Control, nil
}

// SetRealmAgentEmailSendControl changes one realm's send layer.
func SetRealmAgentEmailSendControl(ctx context.Context, endpoint, token, realmID, sendState string) (AgentEmailRealmSendControl, error) {
	return SetRealmAgentEmailSendControlExact(ctx, endpoint, token, realmID, sendState, 0)
}

// SetRealmAgentEmailSendControlExact optionally fences the write to an
// observed realm row version. Zero is an unconditional operator write.
func SetRealmAgentEmailSendControlExact(ctx context.Context, endpoint, token, realmID, sendState string, expectedRowVersion int64) (AgentEmailRealmSendControl, error) {
	body, err := json.Marshal(struct {
		SendState          string `json:"send_state"`
		ExpectedRowVersion int64  `json:"expected_row_version,omitempty"`
	}{SendState: sendState, ExpectedRowVersion: expectedRowVersion})
	if err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	var out struct {
		Control AgentEmailRealmSendControl `json:"control"`
	}
	if err := doJSON(ctx, http.MethodPatch, agentEmailRealmSendURL(endpoint, realmID), token, body, &out); err != nil {
		return AgentEmailRealmSendControl{}, err
	}
	return out.Control, nil
}

func agentEmailAgentSendURL(endpoint, agentID string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/agents/" + neturl.PathEscape(agentID) + "/email-send"
}

func agentEmailRealmSendURL(endpoint, realmID string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/realms/" + neturl.PathEscape(realmID) + "/email-send"
}
