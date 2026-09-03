package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// JSON escaping can expand a valid 256 KiB text value to roughly 1.5 MiB.
	// This is an envelope bound only; decoded text remains capped below.
	maximumAgentEmailOutboundRequestBytes = 2 * 1024 * 1024
	maximumAgentEmailOutboundTextBytes    = 256 * 1024
	maximumAgentEmailOutboundSubjectBytes = 4 * 1024
	maximumAgentEmailOutboundKeyBytes     = 512
)

// AgentEmailOutboundProviderEventSchema is the strict normalized provider
// event envelope accepted by the cell-local delivery lifecycle route.
const AgentEmailOutboundProviderEventSchema = "witself.agent-email-provider-event.v1"

const (
	minimumAgentEmailProviderEventTokenBytes = 32
	maximumAgentEmailProviderEventTokenBytes = 4096
)

// ErrAgentEmailOutboundRateLimited identifies a closed send-rate refusal.
var ErrAgentEmailOutboundRateLimited = errors.New("outbound agent-email rate limit reached")

// AgentEmailOutboundRateLimitError carries only closed, value-free rate
// state from the store. No account, realm, agent, or recipient identifier is
// included in an HTTP response or metric label.
type AgentEmailOutboundRateLimitError struct {
	Scope         string
	Limit         int64
	Used          int64
	WindowSeconds int64
	RetryAfter    time.Duration
	ResetAt       time.Time
	Source        string
	Retryable     bool
}

func (e *AgentEmailOutboundRateLimitError) Error() string {
	return ErrAgentEmailOutboundRateLimited.Error()
}
func (e *AgentEmailOutboundRateLimitError) Unwrap() error { return ErrAgentEmailOutboundRateLimited }

// AgentEmailOutboundMessage is the owner-visible, content-minimal projection
// of one durable outbound email. From is always server-derived. Text is never
// returned by list or show under the production outbound contract.
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

// SendAgentEmailRequest queues one single-recipient plain-text email. From is
// deliberately absent so no caller can choose or spoof a sender identity.
type SendAgentEmailRequest struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
}

// ReplyAgentEmailRequest queues a plain-text reply. The server/store derive
// recipient, subject/thread provenance, and From from the inbound message and
// authenticated owner rather than accepting those fields from the caller.
type ReplyAgentEmailRequest struct {
	Text string `json:"text"`
}

// AgentEmailOutboundListOptions selects a bounded owner-only sent-mail page.
type AgentEmailOutboundListOptions struct {
	State  string
	Limit  int
	Cursor string
}

// AgentEmailOutboundPage is one metadata-only cursor page.
type AgentEmailOutboundPage struct {
	Messages   []AgentEmailOutboundMessage
	NextCursor string
}

// AgentEmailSendControl is the value-free agent send-switch projection.
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

// AgentEmailRealmSendControl is the operator-visible realm send kill switch.
type AgentEmailRealmSendControl struct {
	AccountID  string     `json:"account_id"`
	RealmID    string     `json:"realm_id"`
	SendState  string     `json:"send_state"`
	AgentCount int64      `json:"agent_count"`
	RowVersion int64      `json:"row_version"`
	UpdatedAt  time.Time  `json:"updated_at"`
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
}

// SetAgentEmailSendControlRequest changes only the selected agent or realm
// layer; the effective state remains server-derived.
type SetAgentEmailSendControlRequest struct {
	SendState          string `json:"send_state"`
	ExpectedRowVersion int64  `json:"expected_row_version,omitempty"`
}

// AgentEmailOutboundProviderEvent is the content-free, provider-neutral
// delivery update accepted from the managed adapter. Provider-specific raw
// payloads, recipient data, diagnostic strings, and email content are never
// accepted at the cell boundary.
type AgentEmailOutboundProviderEvent struct {
	SchemaVersion     string    `json:"schema_version"`
	EventID           string    `json:"event_id"`
	ProviderMessageID string    `json:"provider_message_id"`
	EventClass        string    `json:"event_class"`
	OccurredAt        time.Time `json:"occurred_at"`
	BounceType        string    `json:"bounce_type,omitempty"`
}

func agentEmailOutboundProviderEventHandler(
	token string,
	apply func(context.Context, AgentEmailOutboundProviderEvent) error,
) http.HandlerFunc {
	return agentEmailNoStore(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			writeAgentEmailOutboundCodedError(w, http.StatusUnauthorized, "auth_failed", "invalid agent email provider token", false)
			return
		}
		if len(r.URL.Query()) != 0 {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "provider event does not accept query parameters", false)
			return
		}
		var event AgentEmailOutboundProviderEvent
		if decodeStrictAgentEmailJSON(w, r, &event, 32*1024) != nil || !validAgentEmailOutboundProviderEvent(event) {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "invalid agent email provider event", false)
			return
		}
		if err := apply(r.Context(), event); err != nil {
			switch {
			case errors.Is(err, ErrBadInput):
				writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "invalid agent email provider event", false)
			case errors.Is(err, ErrNotFound):
				writeAgentEmailOutboundCodedError(w, http.StatusNotFound, "not_found", "outbound email not found", false)
			case errors.Is(err, ErrConflict), errors.Is(err, ErrIdempotencyConflict):
				writeAgentEmailOutboundCodedError(w, http.StatusConflict, "agent_email_state_conflict", "agent email provider event conflicts with existing state", false)
			case errors.Is(err, ErrAgentEmailDatabaseCapacity):
				writeAgentEmailOutboundCodedError(w, http.StatusInsufficientStorage, "agent_email_storage_full", "agent email storage capacity has been reached", true)
			default:
				writeAgentEmailOutboundCodedError(w, http.StatusInternalServerError, "backend_unavailable", "could not apply agent email provider event", true)
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// AgentEmailOutboundProviderEventHTTPHandler returns the same exact-method,
// exact-path handler mounted by the production API mux. It exists so bounded
// operator probes can exercise the real bearer-authenticated HTTP boundary on
// localhost without exposing unrelated listeners or routes.
func AgentEmailOutboundProviderEventHTTPHandler(
	token string,
	apply func(context.Context, AgentEmailOutboundProviderEvent) error,
) (http.Handler, error) {
	if !validAgentEmailProviderEventToken(token) || apply == nil {
		return nil, errors.New("invalid agent email provider-event handler configuration")
	}
	mux := http.NewServeMux()
	registerAgentEmailOutboundProviderEventRoute(mux, token, apply)
	return securityResponseHeaders(mux), nil
}

func registerAgentEmailOutboundProviderEventRoute(
	mux *http.ServeMux,
	token string,
	apply func(context.Context, AgentEmailOutboundProviderEvent) error,
) {
	mux.HandleFunc("POST /v1/internal/agent-email-send:provider-event",
		agentEmailOutboundProviderEventHandler(token, apply))
}

func validAgentEmailOutboundProviderEvent(event AgentEmailOutboundProviderEvent) bool {
	if event.SchemaVersion != AgentEmailOutboundProviderEventSchema ||
		event.EventID != strings.TrimSpace(event.EventID) || event.EventID == "" || len(event.EventID) > 512 ||
		event.ProviderMessageID != strings.TrimSpace(event.ProviderMessageID) || event.ProviderMessageID == "" || len(event.ProviderMessageID) > 512 ||
		event.OccurredAt.IsZero() || !event.OccurredAt.Equal(event.OccurredAt.UTC()) {
		return false
	}
	switch event.EventClass {
	case "delivered", "deferred", "bounced", "failed", "rejected", "complained":
	default:
		return false
	}
	if event.EventClass != "bounced" {
		return event.BounceType == ""
	}
	// The edge adapter must discard soft/transient bounces and emit bounced
	// only for a permanent/hard outcome. The normalized hint is optional;
	// when present, provider-specific names such as "hard" are not accepted.
	return event.BounceType == "" || event.BounceType == "permanent"
}

func validAgentEmailProviderEventToken(token string) bool {
	return len(token) >= minimumAgentEmailProviderEventTokenBytes &&
		len(token) <= maximumAgentEmailProviderEventTokenBytes &&
		token == strings.TrimSpace(token)
}

func sendAgentEmailHandler(
	auth PrincipalAuthFunc,
	requireEnabled func(context.Context, DomainPrincipal) error,
	queue func(context.Context, DomainPrincipal, SendAgentEmailRequest, string) (AgentEmailOutboundMessage, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailSendPrincipal(auth, requireEnabled, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if len(r.URL.Query()) != 0 {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "email send does not accept query parameters", false)
			return
		}
		var req SendAgentEmailRequest
		if decodeStrictAgentEmailJSON(w, r, &req, maximumAgentEmailOutboundRequestBytes) != nil {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body", false)
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if strings.TrimSpace(req.To) == "" || len(req.To) > 320 ||
			strings.TrimSpace(req.Subject) == "" ||
			!validAgentEmailOutboundSubject(req.Subject) || !validAgentEmailOutboundText(req.Text) ||
			key == "" || len(key) > maximumAgentEmailOutboundKeyBytes {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "invalid outbound email request", false)
			return
		}
		message, err := queue(r.Context(), p, req, key)
		if writeAgentEmailOutboundError(w, err, "could not queue agent email") {
			return
		}
		writeAgentEmailOutboundMessage(w, http.StatusAccepted, message)
	}))
}

func replyAgentEmailHandler(
	auth PrincipalAuthFunc,
	requireEnabled func(context.Context, DomainPrincipal) error,
	reply func(context.Context, DomainPrincipal, string, ReplyAgentEmailRequest, string) (AgentEmailOutboundMessage, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailSendPrincipal(auth, requireEnabled, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if len(r.URL.Query()) != 0 {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "email reply does not accept query parameters", false)
			return
		}
		inboundID := strings.TrimSpace(r.PathValue("inbound"))
		if inboundID == "" {
			candidate, operation, ok := strings.Cut(r.PathValue("action"), ":")
			if ok && operation == "reply" {
				inboundID = strings.TrimSpace(candidate)
			}
		}
		var req ReplyAgentEmailRequest
		if inboundID == "" || decodeStrictAgentEmailJSON(w, r, &req, maximumAgentEmailOutboundRequestBytes) != nil {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "invalid email reply", false)
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if !validAgentEmailOutboundText(req.Text) || key == "" || len(key) > maximumAgentEmailOutboundKeyBytes {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "invalid outbound email reply", false)
			return
		}
		message, err := reply(r.Context(), p, inboundID, req, key)
		if writeAgentEmailOutboundError(w, err, "could not queue agent email reply") {
			return
		}
		writeAgentEmailOutboundMessage(w, http.StatusAccepted, message)
	}))
}

func validAgentEmailOutboundText(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" &&
		len(value) <= maximumAgentEmailOutboundTextBytes && strings.IndexByte(value, 0) < 0
}

func validAgentEmailOutboundSubject(value string) bool {
	if !utf8.ValidString(value) || len(value) > maximumAgentEmailOutboundSubjectBytes || strings.ContainsAny(value, "\r\n") {
		return false
	}
	for _, r := range value {
		if r < 0x20 && r != '\t' || r == 0x7f {
			return false
		}
	}
	return true
}

// agentEmailActionDispatchHandler shares the ServeMux-compatible
// /v1/email/{action} route between legacy inbound actions and outbound reply.
// Go wildcards cannot have a literal suffix, so "{inbound}:reply" must be
// parsed inside the handler. Each selected handler retains its own auth and
// entitlement gate; outbound reply never inherits inbound receive scope.
func agentEmailActionDispatchHandler(receive, reply http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, operation, ok := strings.Cut(r.PathValue("action"), ":")
		if ok && operation == "reply" && reply != nil {
			reply(w, r)
			return
		}
		if receive != nil {
			receive(w, r)
			return
		}
		writeAgentEmailOutboundCodedError(w, http.StatusNotFound, "not_found", "email action not found", false)
	}
}

func listAgentEmailOutboundHandler(
	auth PrincipalAuthFunc,
	requireEnabled func(context.Context, DomainPrincipal) error,
	list func(context.Context, DomainPrincipal, AgentEmailOutboundListOptions) (AgentEmailOutboundPage, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailSendPrincipal(auth, requireEnabled, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		q := r.URL.Query()
		opts := AgentEmailOutboundListOptions{State: strings.TrimSpace(q.Get("state")), Cursor: q.Get("cursor")}
		for key := range q {
			if key != "state" && key != "cursor" && key != "limit" {
				writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "unsupported sent-email query parameter", false)
				return
			}
		}
		if raw := q.Get("limit"); raw != "" {
			limit, err := strconv.Atoi(raw)
			if err != nil {
				writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "limit must be an integer", false)
				return
			}
			opts.Limit = limit
		}
		page, err := list(r.Context(), p, opts)
		if writeAgentEmailOutboundError(w, err, "could not list sent agent email") {
			return
		}
		if page.Messages == nil {
			page.Messages = []AgentEmailOutboundMessage{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "messages": page.Messages, "next_cursor": page.NextCursor,
		})
	}))
}

func getAgentEmailOutboundHandler(
	auth PrincipalAuthFunc,
	requireEnabled func(context.Context, DomainPrincipal) error,
	get func(context.Context, DomainPrincipal, string) (AgentEmailOutboundMessage, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailSendPrincipal(auth, requireEnabled, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if len(r.URL.Query()) != 0 {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "sent email does not accept query parameters", false)
			return
		}
		messageID := strings.TrimSpace(r.PathValue("id"))
		if messageID == "" {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "sent email id is required", false)
			return
		}
		message, err := get(r.Context(), p, messageID)
		if writeAgentEmailOutboundError(w, err, "could not get sent agent email") {
			return
		}
		writeAgentEmailOutboundMessage(w, http.StatusOK, message)
	}))
}

func writeAgentEmailOutboundMessage(w http.ResponseWriter, status int, message AgentEmailOutboundMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "message": message})
}

func requireAgentEmailSendPrincipal(
	auth PrincipalAuthFunc,
	requireEnabled func(context.Context, DomainPrincipal) error,
	h func(http.ResponseWriter, *http.Request, DomainPrincipal),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setAuthenticatedNoStoreDefault(w)
		token, ok := bearerToken(r)
		if !ok {
			writeAgentEmailOutboundCodedError(w, http.StatusUnauthorized, "auth_failed", "missing bearer token", false)
			return
		}
		p, ok, err := auth(r.Context(), token)
		if err != nil {
			writeAgentEmailOutboundCodedError(w, http.StatusInternalServerError, "backend_unavailable", "could not authenticate agent email send", true)
			return
		}
		if !ok {
			writeAgentEmailOutboundCodedError(w, http.StatusUnauthorized, "auth_failed", "invalid token", false)
			return
		}
		if p.AccountStatus != "active" {
			writeAgentEmailOutboundCodedError(w, http.StatusForbidden, "forbidden", "agent email send requires an active account", false)
			return
		}
		if profile := effectiveAccessProfile(p); profile != AccessProfileFull {
			writeAgentEmailOutboundCodedError(w, http.StatusForbidden, "forbidden", "credential profile is not authorized for this route", false)
			return
		}
		if p.Kind != PrincipalKindAgent {
			writeAgentEmailOutboundCodedError(w, http.StatusForbidden, "forbidden", "only an agent token may send email", false)
			return
		}
		if requireEnabled != nil {
			if err := requireEnabled(r.Context(), p); writeAgentEmailOutboundError(w, err, "could not check agent-email send entitlement") {
				return
			}
		}
		h(w, r, p)
	}
}

func writeAgentEmailOutboundError(w http.ResponseWriter, err error, internalMessage string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrFeatureNotEnabled):
		writeFeatureNotEnabledError(w, err)
	case errors.Is(err, ErrAgentEmailOutboundRateLimited):
		writeAgentEmailOutboundRateLimitError(w, err)
	case errors.Is(err, ErrAgentEmailDatabaseCapacity):
		writeAgentEmailOutboundCodedError(
			w, http.StatusInsufficientStorage, "agent_email_storage_full",
			"agent email storage capacity has been reached", false,
		)
	case errors.Is(err, ErrBadInput):
		writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", err.Error(), false)
	case errors.Is(err, ErrNotFound):
		writeAgentEmailOutboundCodedError(w, http.StatusNotFound, "not_found", "email not found", false)
	case errors.Is(err, ErrForbidden):
		writeAgentEmailOutboundCodedError(w, http.StatusForbidden, "forbidden", "agent email send is forbidden", false)
	case errors.Is(err, ErrBusy):
		writeAgentEmailOutboundCodedError(w, http.StatusConflict, "agent_email_processing_busy", "email send is already being processed", true)
	case errors.Is(err, ErrIdempotencyConflict):
		writeAgentEmailOutboundCodedError(w, http.StatusConflict, "agent_email_idempotency_conflict", "idempotency key was reused for a different agent email send", false)
	case errors.Is(err, ErrConflict):
		writeAgentEmailOutboundCodedError(w, http.StatusConflict, "agent_email_state_conflict", "email send conflicts with existing state", false)
	default:
		writeAgentEmailOutboundCodedError(w, http.StatusInternalServerError, "backend_unavailable", internalMessage, true)
	}
	return true
}

func writeAgentEmailOutboundCodedError(
	w http.ResponseWriter,
	status int,
	code, message string,
	retryable bool,
) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema_version": "witself.v0",
		"code":           code,
		"error":          message,
		"retryable":      retryable,
	})
}

func writeAgentEmailOutboundRateLimitError(w http.ResponseWriter, err error) {
	detail := &AgentEmailOutboundRateLimitError{Retryable: true, WindowSeconds: 60}
	var typed *AgentEmailOutboundRateLimitError
	if errors.As(err, &typed) && typed != nil {
		detail = typed
	}
	scope := "unknown"
	limitKey := ""
	switch detail.Scope {
	case "account":
		scope = "account"
		switch detail.WindowSeconds {
		case 60:
			limitKey = "agent_email_sent_per_account_minute"
		case 86_400:
			limitKey = "agent_email_sent_per_account_day"
		}
	case "agent":
		scope = "agent"
		if detail.WindowSeconds == 60 {
			limitKey = "agent_email_sent_per_agent_minute"
		}
	case "realm":
		scope = "realm"
		if detail.WindowSeconds == 60 {
			limitKey = "agent_email_sent_per_realm_minute"
		}
	case "recipient":
		scope = "recipient"
		if detail.WindowSeconds == 86_400 {
			limitKey = "agent_email_sent_per_recipient_day"
		}
	}
	window := detail.WindowSeconds
	if window < 1 {
		window = 60
	}
	details := map[string]any{
		"limit_dimension": "agent_email_sent",
		"scope":           scope, "limit": max(detail.Limit, 0), "used": max(detail.Used, 0),
		"attempted":      int64(1),
		"window_seconds": window,
	}
	if limitKey != "" {
		details["limit_key"] = limitKey
	}
	if detail.Source == "plan" || detail.Source == "platform" {
		details["source"] = detail.Source
	}
	status := http.StatusForbidden
	payload := map[string]any{
		"schema_version": "witself.v0", "code": "limit_exceeded",
		"error": ErrAgentEmailOutboundRateLimited.Error(), "retryable": false,
		"details": details,
	}
	if detail.Retryable {
		retryAfter := messageRateLimitRetryAfterSeconds(detail.RetryAfter)
		status = http.StatusTooManyRequests
		payload["code"], payload["retryable"], payload["retry_after"] = "rate_limited", true, retryAfter
		details["retry_after"] = retryAfter
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func getAgentEmailSendControlHandler(
	auth AuthFunc,
	get func(context.Context, string, string, string) (AgentEmailSendControl, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailSendOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		if !allowAgentEmailSendControlStatus(w, p.accountStatus, "") {
			return
		}
		if len(r.URL.Query()) != 0 {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "email send control does not accept query parameters", false)
			return
		}
		agentID := strings.TrimSpace(r.PathValue("agent"))
		if agentID == "" {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "agent id is required", false)
			return
		}
		control, err := get(r.Context(), p.accountID, p.operatorID, agentID)
		if writeAgentEmailOutboundError(w, err, "could not get agent email send control") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "control": control})
	}))
}

func setAgentEmailSendControlHandler(
	auth AuthFunc,
	set func(context.Context, string, string, string, string, int64) (AgentEmailSendControl, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailSendOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		if len(r.URL.Query()) != 0 {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "email send control does not accept query parameters", false)
			return
		}
		agentID := strings.TrimSpace(r.PathValue("agent"))
		var req SetAgentEmailSendControlRequest
		if agentID == "" || decodeStrictAgentEmailJSON(w, r, &req, 16*1024) != nil ||
			(req.SendState != "enabled" && req.SendState != "disabled") || req.ExpectedRowVersion < 0 {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "send_state must be enabled or disabled", false)
			return
		}
		if !allowAgentEmailSendControlStatus(w, p.accountStatus, req.SendState) {
			return
		}
		control, err := set(r.Context(), p.accountID, p.operatorID, agentID, req.SendState, req.ExpectedRowVersion)
		if writeAgentEmailOutboundError(w, err, "could not set agent email send control") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "control": control})
	}))
}

func getRealmAgentEmailSendControlHandler(
	auth AuthFunc,
	get func(context.Context, string, string, string) (AgentEmailRealmSendControl, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailSendOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		if !allowAgentEmailSendControlStatus(w, p.accountStatus, "") {
			return
		}
		if len(r.URL.Query()) != 0 {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "email send control does not accept query parameters", false)
			return
		}
		realmID := strings.TrimSpace(r.PathValue("realm"))
		if realmID == "" {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "realm id is required", false)
			return
		}
		control, err := get(r.Context(), p.accountID, p.operatorID, realmID)
		if writeAgentEmailOutboundError(w, err, "could not get realm email send control") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "control": control})
	}))
}

func setRealmAgentEmailSendControlHandler(
	auth AuthFunc,
	set func(context.Context, string, string, string, string, int64) (AgentEmailRealmSendControl, error),
) http.HandlerFunc {
	return agentEmailNoStore(requireAgentEmailSendOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		if len(r.URL.Query()) != 0 {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "email send control does not accept query parameters", false)
			return
		}
		realmID := strings.TrimSpace(r.PathValue("realm"))
		var req SetAgentEmailSendControlRequest
		if realmID == "" || decodeStrictAgentEmailJSON(w, r, &req, 16*1024) != nil ||
			(req.SendState != "enabled" && req.SendState != "disabled") || req.ExpectedRowVersion < 0 {
			writeAgentEmailOutboundCodedError(w, http.StatusBadRequest, "invalid_request", "send_state must be enabled or disabled", false)
			return
		}
		if !allowAgentEmailSendControlStatus(w, p.accountStatus, req.SendState) {
			return
		}
		control, err := set(r.Context(), p.accountID, p.operatorID, realmID, req.SendState, req.ExpectedRowVersion)
		if writeAgentEmailOutboundError(w, err, "could not set realm email send control") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "control": control})
	}))
}

func allowAgentEmailSendControlStatus(w http.ResponseWriter, accountStatus, desiredState string) bool {
	if accountStatus == "active" || accountStatus == "suspended" && (desiredState == "" || desiredState == "disabled") {
		return true
	}
	if desiredState == "enabled" {
		writeAgentEmailOutboundCodedError(w, http.StatusForbidden, "forbidden", "enabling email send requires an active account", false)
		return false
	}
	writeAgentEmailOutboundCodedError(w, http.StatusForbidden, "forbidden", "email send control requires an active or suspended account", false)
	return false
}

func requireAgentEmailSendOperatorAnyStatus(
	auth AuthFunc,
	h func(http.ResponseWriter, *http.Request, principal),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setAuthenticatedNoStoreDefault(w)
		token, ok := bearerToken(r)
		if !ok {
			writeAgentEmailOutboundCodedError(w, http.StatusUnauthorized, "auth_failed", "missing bearer token", false)
			return
		}
		operatorID, accountID, accountStatus, ok, err := auth(r.Context(), token)
		if err != nil {
			writeAgentEmailOutboundCodedError(w, http.StatusInternalServerError, "backend_unavailable", "could not authenticate agent email send control", true)
			return
		}
		if !ok {
			writeAgentEmailOutboundCodedError(w, http.StatusUnauthorized, "auth_failed", "invalid token", false)
			return
		}
		h(w, r, principal{operatorID: operatorID, accountID: accountID, accountStatus: accountStatus})
	}
}
