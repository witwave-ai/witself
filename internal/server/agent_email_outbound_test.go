package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAgentEmailOutboundOwnerRoutesDeriveIdentityAndStayContentMinimal(t *testing.T) {
	principal := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_aaaaaaaaaaaaaaaa",
		AccountID: "acc_aaaaaaaaaaaaaaaa", RealmID: "realm_aaaaaaaaaaaaaaaa",
		AccountStatus: "active", AccessProfile: AccessProfileFull,
	}
	auth := func(context.Context, string) (DomainPrincipal, bool, error) { return principal, true, nil }
	created := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	message := AgentEmailOutboundMessage{
		ID: "esnd_aaaaaaaaaaaaaaaa", OwnerAgentID: principal.ID,
		From: "founder.aaaaaaaaaaaaaaaa@send.witmail.net", To: "person@example.com",
		Subject: "Hello", State: "queued", ProviderState: "", CreatedAt: created, UpdatedAt: created,
	}
	var queued SendAgentEmailRequest
	var queueKey, replyID, replyKey, replyText string
	cfg := Config{
		AuthenticatePrincipal: auth,
		RequireAgentEmailSendEntitlement: func(_ context.Context, got DomainPrincipal) error {
			if got != principal {
				t.Fatalf("entitlement principal = %#v", got)
			}
			return nil
		},
		QueueAgentEmail: func(_ context.Context, got DomainPrincipal, in SendAgentEmailRequest, key string) (AgentEmailOutboundMessage, error) {
			if got != principal {
				t.Fatalf("queue principal = %#v", got)
			}
			queued, queueKey = in, key
			return message, nil
		},
		ReplyAgentEmail: func(_ context.Context, got DomainPrincipal, inbound string, in ReplyAgentEmailRequest, key string) (AgentEmailOutboundMessage, error) {
			if got != principal {
				t.Fatalf("reply principal = %#v", got)
			}
			replyID, replyText, replyKey = inbound, in.Text, key
			replied := message
			replied.ID = "esnd_bbbbbbbbbbbbbbbb"
			replied.ReplyToInboundMessageID = inbound
			return replied, nil
		},
		ListAgentEmailOutbox: func(_ context.Context, got DomainPrincipal, opts AgentEmailOutboundListOptions) (AgentEmailOutboundPage, error) {
			if got != principal || opts.State != "queued" || opts.Limit != 7 || opts.Cursor != "next" {
				t.Fatalf("list args = %#v %#v", got, opts)
			}
			return AgentEmailOutboundPage{Messages: []AgentEmailOutboundMessage{message}, NextCursor: "after"}, nil
		},
		GetAgentEmailOutbound: func(_ context.Context, got DomainPrincipal, id string) (AgentEmailOutboundMessage, error) {
			if got != principal || id != message.ID {
				t.Fatalf("get args = %#v %q", got, id)
			}
			return message, nil
		},
	}
	handler := apiMux(cfg)

	response := performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email:send", "agent-token",
		`{"to":"person@example.com","subject":"Hello","text":"plain text"}`,
		map[string]string{"Idempotency-Key": "send-1"})
	if response.Code != http.StatusAccepted || queued.To != "person@example.com" || queued.Text != "plain text" ||
		queued.Subject != "Hello" || queueKey != "send-1" || strings.Contains(response.Body.String(), "plain text") {
		t.Fatalf("send = %d %s queued=%#v key=%q", response.Code, response.Body.String(), queued, queueKey)
	}

	response = performAgentEmailOwnerRequest(handler, http.MethodPost,
		"/v1/email/emsg_aaaaaaaaaaaaaaaa:reply", "agent-token", `{"text":"reply text"}`,
		map[string]string{"Idempotency-Key": "reply-1"})
	if response.Code != http.StatusAccepted || replyID != "emsg_aaaaaaaaaaaaaaaa" || replyText != "reply text" ||
		replyKey != "reply-1" || strings.Contains(response.Body.String(), "reply text") {
		t.Fatalf("reply = %d %s id=%q text=%q key=%q", response.Code, response.Body.String(), replyID, replyText, replyKey)
	}

	response = performAgentEmailOwnerRequest(handler, http.MethodGet,
		"/v1/email/sent?state=queued&limit=7&cursor=next", "agent-token", "", nil)
	if response.Code != http.StatusOK || !containsAll(response.Body.String(), `"next_cursor":"after"`, `"id":"esnd_aaaaaaaaaaaaaaaa"`) {
		t.Fatalf("list = %d %s", response.Code, response.Body.String())
	}
	response = performAgentEmailOwnerRequest(handler, http.MethodGet,
		"/v1/email/sent/esnd_aaaaaaaaaaaaaaaa", "agent-token", "", nil)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "plain text") {
		t.Fatalf("show = %d %s", response.Code, response.Body.String())
	}
}

func TestAgentEmailOutboundRejectsSpoofedFromAndHasStablePolicyAndConflictErrors(t *testing.T) {
	principal := DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_aaaaaaaaaaaaaaaa", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active", AccessProfile: AccessProfileFull}
	auth := func(context.Context, string) (DomainPrincipal, bool, error) { return principal, true, nil }
	calls := 0
	cfg := Config{
		AuthenticatePrincipal: auth,
		RequireAgentEmailSendEntitlement: func(context.Context, DomainPrincipal) error {
			return &FeatureNotEnabledError{Feature: "agent_email_send"}
		},
		QueueAgentEmail: func(context.Context, DomainPrincipal, SendAgentEmailRequest, string) (AgentEmailOutboundMessage, error) {
			calls++
			return AgentEmailOutboundMessage{}, nil
		},
	}
	handler := apiMux(cfg)
	response := performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email:send", "agent-token",
		`{"to":"person@example.com","subject":"Hello","text":"body","from":"spoof@example.com"}`,
		map[string]string{"Idempotency-Key": "send-1"})
	if response.Code != http.StatusForbidden || calls != 0 ||
		!containsAll(response.Body.String(), `"code":"feature_not_enabled"`, `"feature":"agent_email_send"`, "Sorry, this feature is not enabled on this account.") {
		t.Fatalf("policy refusal = %d %s calls=%d", response.Code, response.Body.String(), calls)
	}

	cfg.RequireAgentEmailSendEntitlement = func(context.Context, DomainPrincipal) error { return nil }
	cfg.QueueAgentEmail = func(context.Context, DomainPrincipal, SendAgentEmailRequest, string) (AgentEmailOutboundMessage, error) {
		calls++
		return AgentEmailOutboundMessage{}, ErrIdempotencyConflict
	}
	handler = apiMux(cfg)
	response = performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email:send", "agent-token",
		`{"to":"person@example.com","subject":"Hello","text":"body","from":"spoof@example.com"}`,
		map[string]string{"Idempotency-Key": "send-1"})
	if response.Code != http.StatusBadRequest || calls != 0 {
		t.Fatalf("spoof rejection = %d %s calls=%d", response.Code, response.Body.String(), calls)
	}
	response = performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email:send", "agent-token",
		`{"to":"person@example.com","subject":"Hello","text":"body"}`,
		map[string]string{"Idempotency-Key": "send-1"})
	if response.Code != http.StatusConflict || calls != 1 ||
		!containsAll(response.Body.String(), `"code":"agent_email_idempotency_conflict"`, `"retryable":false`, "idempotency key was reused for a different agent email send") {
		t.Fatalf("idempotency conflict = %d %s calls=%d", response.Code, response.Body.String(), calls)
	}
	response = performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email:send", "agent-token",
		`{"to":"person@example.com","text":"body"}`,
		map[string]string{"Idempotency-Key": "send-2"})
	if response.Code != http.StatusBadRequest || calls != 1 {
		t.Fatalf("missing subject = %d %s calls=%d", response.Code, response.Body.String(), calls)
	}
	assertAgentEmailOutboundCodedError(t, response, http.StatusBadRequest, "invalid_request", false)
}

func TestAgentEmailOutboundOwnerFailuresAlwaysUseCodedEnvelope(t *testing.T) {
	base := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_aaaaaaaaaaaaaaaa",
		AccountID: "acc_aaaaaaaaaaaaaaaa", RealmID: "realm_aaaaaaaaaaaaaaaa",
		AccountStatus: "active", AccessProfile: AccessProfileFull,
	}
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		principal := base
		switch token {
		case "agent-token":
			return principal, true, nil
		case "operator-token":
			principal.Kind = PrincipalKindOperator
			return principal, true, nil
		case "restricted-token":
			principal.AccessProfile = AccessProfileCuratorPreview
			return principal, true, nil
		case "suspended-token":
			principal.AccountStatus = "suspended"
			return principal, true, nil
		case "auth-error":
			return DomainPrincipal{}, false, errors.New("private authentication failure")
		default:
			return DomainPrincipal{}, false, nil
		}
	}
	handler := apiMux(Config{
		AuthenticatePrincipal: auth,
		QueueAgentEmail: func(context.Context, DomainPrincipal, SendAgentEmailRequest, string) (AgentEmailOutboundMessage, error) {
			return AgentEmailOutboundMessage{}, nil
		},
		ReplyAgentEmail: func(context.Context, DomainPrincipal, string, ReplyAgentEmailRequest, string) (AgentEmailOutboundMessage, error) {
			return AgentEmailOutboundMessage{}, nil
		},
		ListAgentEmailOutbox: func(context.Context, DomainPrincipal, AgentEmailOutboundListOptions) (AgentEmailOutboundPage, error) {
			return AgentEmailOutboundPage{}, nil
		},
		GetAgentEmailOutbound: func(context.Context, DomainPrincipal, string) (AgentEmailOutboundMessage, error) {
			return AgentEmailOutboundMessage{}, nil
		},
	})
	validSend := `{"to":"person@example.com","subject":"Hello","text":"body"}`
	tests := []struct {
		name, method, target, token, body, code string
		headers                                 map[string]string
		status                                  int
		retryable                               bool
	}{
		{name: "missing auth", method: http.MethodPost, target: "/v1/email:send", body: validSend, headers: map[string]string{"Idempotency-Key": "send-1"}, status: http.StatusUnauthorized, code: "auth_failed"},
		{name: "invalid auth", method: http.MethodPost, target: "/v1/email:send", token: "bad-token", body: validSend, headers: map[string]string{"Idempotency-Key": "send-1"}, status: http.StatusUnauthorized, code: "auth_failed"},
		{name: "auth backend unavailable", method: http.MethodPost, target: "/v1/email:send", token: "auth-error", body: validSend, headers: map[string]string{"Idempotency-Key": "send-1"}, status: http.StatusInternalServerError, code: "backend_unavailable", retryable: true},
		{name: "operator principal", method: http.MethodPost, target: "/v1/email:send", token: "operator-token", body: validSend, headers: map[string]string{"Idempotency-Key": "send-1"}, status: http.StatusForbidden, code: "forbidden"},
		{name: "restricted principal", method: http.MethodPost, target: "/v1/email:send", token: "restricted-token", body: validSend, headers: map[string]string{"Idempotency-Key": "send-1"}, status: http.StatusForbidden, code: "forbidden"},
		{name: "inactive account", method: http.MethodPost, target: "/v1/email:send", token: "suspended-token", body: validSend, headers: map[string]string{"Idempotency-Key": "send-1"}, status: http.StatusForbidden, code: "forbidden"},
		{name: "send query", method: http.MethodPost, target: "/v1/email:send?extra=1", token: "agent-token", body: validSend, headers: map[string]string{"Idempotency-Key": "send-1"}, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "send malformed json", method: http.MethodPost, target: "/v1/email:send", token: "agent-token", body: `{`, headers: map[string]string{"Idempotency-Key": "send-1"}, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "send invalid fields", method: http.MethodPost, target: "/v1/email:send", token: "agent-token", body: `{"to":"person@example.com","text":"body"}`, headers: map[string]string{"Idempotency-Key": "send-1"}, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "reply query", method: http.MethodPost, target: "/v1/email/emsg_aaaaaaaaaaaaaaaa:reply?extra=1", token: "agent-token", body: `{"text":"body"}`, headers: map[string]string{"Idempotency-Key": "reply-1"}, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "reply invalid fields", method: http.MethodPost, target: "/v1/email/emsg_aaaaaaaaaaaaaaaa:reply", token: "agent-token", body: `{"text":""}`, headers: map[string]string{"Idempotency-Key": "reply-1"}, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "list unsupported query", method: http.MethodGet, target: "/v1/email/sent?extra=1", token: "agent-token", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "list invalid limit", method: http.MethodGet, target: "/v1/email/sent?limit=nope", token: "agent-token", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "show query", method: http.MethodGet, target: "/v1/email/sent/esnd_aaaaaaaaaaaaaaaa?extra=1", token: "agent-token", status: http.StatusBadRequest, code: "invalid_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performAgentEmailOwnerRequest(
				handler, test.method, test.target, test.token, test.body, test.headers,
			)
			assertAgentEmailOutboundCodedError(t, response, test.status, test.code, test.retryable)
		})
	}
}

func TestAgentEmailOutboundProviderEventRequiresDedicatedTrustAndStrictShape(t *testing.T) {
	var applied AgentEmailOutboundProviderEvent
	const providerToken = "provider-secret-0123456789-abcdef"
	handler := apiMux(Config{
		AgentEmailProviderEventToken: providerToken,
		ApplyAgentEmailOutboundProviderEvent: func(_ context.Context, event AgentEmailOutboundProviderEvent) error {
			applied = event
			return nil
		},
	})
	body := `{"schema_version":"witself.agent-email-provider-event.v1","event_id":"evt_1","provider_message_id":"provider-1","event_class":"delivered","occurred_at":"2026-08-14T12:00:00Z"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/internal/agent-email-send:provider-event", strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertAgentEmailOutboundCodedError(t, response, http.StatusUnauthorized, "auth_failed", false)
	request = httptest.NewRequest(http.MethodPost, "/v1/internal/agent-email-send:provider-event", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+providerToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || applied.EventID != "evt_1" || applied.ProviderMessageID != "provider-1" || applied.EventClass != "delivered" {
		t.Fatalf("accepted = %d %s event=%#v", response.Code, response.Body.String(), applied)
	}
	for _, eventClass := range []string{"deferred", "bounced", "failed", "rejected", "complained"} {
		bounce := ""
		if eventClass == "bounced" {
			bounce = `,"bounce_type":"permanent"`
		}
		eventBody := `{"schema_version":"witself.agent-email-provider-event.v1","event_id":"evt_` + eventClass + `","provider_message_id":"provider-1","event_class":"` + eventClass + `","occurred_at":"2026-08-14T12:00:00Z"` + bounce + `}`
		request = httptest.NewRequest(http.MethodPost, "/v1/internal/agent-email-send:provider-event", strings.NewReader(eventBody))
		request.Header.Set("Authorization", "Bearer "+providerToken)
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent || applied.EventClass != eventClass {
			t.Fatalf("%s = %d %s event=%#v", eventClass, response.Code, response.Body.String(), applied)
		}
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/internal/agent-email-send:provider-event", strings.NewReader(`{"schema_version":"witself.agent-email-provider-event.v1","event_id":"evt_soft","provider_message_id":"provider-1","event_class":"bounced","occurred_at":"2026-08-14T12:00:00Z","bounce_type":"soft"}`))
	request.Header.Set("Authorization", "Bearer "+providerToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertAgentEmailOutboundCodedError(t, response, http.StatusBadRequest, "invalid_request", false)
	request = httptest.NewRequest(http.MethodPost, "/v1/internal/agent-email-send:provider-event", strings.NewReader(strings.TrimSuffix(body, "}")+`,"raw_provider_payload":{}}`))
	request.Header.Set("Authorization", "Bearer "+providerToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertAgentEmailOutboundCodedError(t, response, http.StatusBadRequest, "invalid_request", false)

	encoded, err := json.Marshal(applied)
	if err != nil || strings.Contains(string(encoded), "raw_provider") {
		t.Fatalf("normalized event = %s err=%v", encoded, err)
	}
	shortTokenHandler := apiMux(Config{
		AgentEmailProviderEventToken:         "too-short",
		ApplyAgentEmailOutboundProviderEvent: func(context.Context, AgentEmailOutboundProviderEvent) error { return nil },
	})
	request = httptest.NewRequest(http.MethodPost, "/v1/internal/agent-email-send:provider-event", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer too-short")
	response = httptest.NewRecorder()
	shortTokenHandler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("short-token route status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestAgentEmailOutboundProviderEventStorageCapacityIsExplicitAndRetryable(t *testing.T) {
	const providerToken = "provider-secret-0123456789-abcdef"
	handler := apiMux(Config{
		AgentEmailProviderEventToken: providerToken,
		ApplyAgentEmailOutboundProviderEvent: func(context.Context, AgentEmailOutboundProviderEvent) error {
			return ErrAgentEmailDatabaseCapacity
		},
	})
	body := `{"schema_version":"witself.agent-email-provider-event.v1","event_id":"evt_capacity","provider_message_id":"provider-1","event_class":"delivered","occurred_at":"2026-08-14T12:00:00Z"}`
	request := httptest.NewRequest(
		http.MethodPost, "/v1/internal/agent-email-send:provider-event",
		strings.NewReader(body),
	)
	request.Header.Set("Authorization", "Bearer "+providerToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertAgentEmailOutboundCodedError(
		t, response, http.StatusInsufficientStorage,
		"agent_email_storage_full", true,
	)
}

func TestAgentEmailOutboundProviderEventStandaloneHandlerUsesProductionRoute(t *testing.T) {
	token := "provider-event-token-0123456789-abcd"
	var applied int
	handler, err := AgentEmailOutboundProviderEventHTTPHandler(
		token,
		func(context.Context, AgentEmailOutboundProviderEvent) error {
			applied++
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"schema_version":"witself.agent-email-provider-event.v1","event_id":"evt_standalone","provider_message_id":"provider-1","event_class":"delivered","occurred_at":"2026-08-14T12:00:00Z"}`
	request := httptest.NewRequest(
		http.MethodPost, "/v1/internal/agent-email-send:provider-event",
		strings.NewReader(body),
	)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || applied != 1 {
		t.Fatalf("standalone provider event = %d / %d", response.Code, applied)
	}

	wrongMethod := httptest.NewRequest(
		http.MethodGet, "/v1/internal/agent-email-send:provider-event", nil,
	)
	wrongMethodResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongMethodResponse, wrongMethod)
	if wrongMethodResponse.Code != http.StatusMethodNotAllowed || applied != 1 {
		t.Fatalf("wrong method = %d / %d", wrongMethodResponse.Code, applied)
	}
	if _, err := AgentEmailOutboundProviderEventHTTPHandler("short", func(context.Context, AgentEmailOutboundProviderEvent) error { return nil }); err == nil {
		t.Fatal("short provider-event token was accepted")
	}
}

func TestAgentEmailOutboundRateLimitIsStructuredAndValueFree(t *testing.T) {
	principal := DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_private", AccountID: "acc_private", RealmID: "realm_private", AccountStatus: "active", AccessProfile: AccessProfileFull}
	handler := apiMux(Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) { return principal, true, nil },
		QueueAgentEmail: func(context.Context, DomainPrincipal, SendAgentEmailRequest, string) (AgentEmailOutboundMessage, error) {
			return AgentEmailOutboundMessage{}, &AgentEmailOutboundRateLimitError{
				Scope: "agent", Limit: 4, Used: 4, WindowSeconds: 60,
				RetryAfter: 1500 * time.Millisecond, Source: "plan", Retryable: true,
			}
		},
	})
	response := performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email:send", "agent-token",
		`{"to":"person@example.com","subject":"Hello","text":"body"}`,
		map[string]string{"Idempotency-Key": "send-rate-1"})
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "2" ||
		!containsAll(response.Body.String(), `"code":"rate_limited"`, `"limit_key":"agent_email_sent_per_agent_minute"`, `"attempted":1`, `"retryable":true`) ||
		strings.Contains(response.Body.String(), "agent_private") || strings.Contains(response.Body.String(), "person@example.com") {
		t.Fatalf("rate response = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestAgentEmailOutboundDatabaseCapacityFailsClosedAndIsValueFree(t *testing.T) {
	principal := DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_private", AccountID: "acc_private", RealmID: "realm_private", AccountStatus: "active", AccessProfile: AccessProfileFull}
	handler := apiMux(Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) { return principal, true, nil },
		QueueAgentEmail: func(context.Context, DomainPrincipal, SendAgentEmailRequest, string) (AgentEmailOutboundMessage, error) {
			return AgentEmailOutboundMessage{}, ErrAgentEmailDatabaseCapacity
		},
	})
	response := performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email:send", "agent-token",
		`{"to":"person@example.com","subject":"Hello","text":"body"}`,
		map[string]string{"Idempotency-Key": "send-capacity-1"})
	if response.Code != http.StatusInsufficientStorage || response.Header().Get("Retry-After") != "" ||
		!containsAll(response.Body.String(), `"code":"agent_email_storage_full"`, `"retryable":false`) ||
		strings.Contains(response.Body.String(), "agent_private") || strings.Contains(response.Body.String(), "person@example.com") {
		t.Fatalf("capacity response = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestAgentEmailOutboundAccountBreakerHasStableWireShape(t *testing.T) {
	response := httptest.NewRecorder()
	writeAgentEmailOutboundRateLimitError(response, &AgentEmailOutboundRateLimitError{
		Scope: "account", Limit: 1_000, Used: 1_000, WindowSeconds: 60,
		RetryAfter: time.Second, Source: "platform", Retryable: true,
	})
	if response.Code != http.StatusTooManyRequests ||
		!containsAll(response.Body.String(),
			`"limit_dimension":"agent_email_sent"`,
			`"limit_key":"agent_email_sent_per_account_minute"`,
			`"scope":"account"`, `"source":"platform"`) {
		t.Fatalf("account breaker = %d %s", response.Code, response.Body.String())
	}
}

func TestAgentEmailOutboundDailyBreakersHaveStableWireShape(t *testing.T) {
	for _, test := range []struct {
		scope string
		key   string
	}{
		{scope: "account", key: "agent_email_sent_per_account_day"},
		{scope: "recipient", key: "agent_email_sent_per_recipient_day"},
	} {
		response := httptest.NewRecorder()
		writeAgentEmailOutboundRateLimitError(response, &AgentEmailOutboundRateLimitError{
			Scope: test.scope, Limit: 100, Used: 100, WindowSeconds: 86_400,
			RetryAfter: time.Minute, Source: "platform", Retryable: true,
		})
		body := response.Body.String()
		if response.Code != http.StatusTooManyRequests ||
			!containsAll(body,
				`"limit_dimension":"agent_email_sent"`,
				`"limit_key":"`+test.key+`"`,
				`"scope":"`+test.scope+`"`,
				`"source":"platform"`,
				`"window_seconds":86400`,
			) {
			t.Fatalf("daily %s breaker = %d %s", test.scope, response.Code, body)
		}
	}
}

func TestAgentEmailOutboundHTTPEnvelopeAllowsFullTextLimit(t *testing.T) {
	principal := DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_aaaaaaaaaaaaaaaa", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active", AccessProfile: AccessProfileFull}
	queueCalls := 0
	handler := apiMux(Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) { return principal, true, nil },
		QueueAgentEmail: func(_ context.Context, _ DomainPrincipal, in SendAgentEmailRequest, _ string) (AgentEmailOutboundMessage, error) {
			queueCalls++
			return AgentEmailOutboundMessage{ID: "esnd_aaaaaaaaaaaaaaaa", To: in.To, Subject: in.Subject, State: "queued"}, nil
		},
	})
	requestBody := func(text string) string {
		encoded, err := json.Marshal(SendAgentEmailRequest{To: "person@example.com", Subject: "boundary", Text: text})
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	response := performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email:send", "agent-token",
		requestBody(strings.Repeat("\"", maximumAgentEmailOutboundTextBytes)),
		map[string]string{"Idempotency-Key": "boundary-1"})
	if response.Code != http.StatusAccepted || queueCalls != 1 {
		t.Fatalf("exact text limit = %d body=%s calls=%d", response.Code, response.Body.String(), queueCalls)
	}
	response = performAgentEmailOwnerRequest(handler, http.MethodPost, "/v1/email:send", "agent-token",
		requestBody(strings.Repeat("x", maximumAgentEmailOutboundTextBytes+1)),
		map[string]string{"Idempotency-Key": "boundary-2"})
	if response.Code != http.StatusBadRequest || queueCalls != 1 {
		t.Fatalf("over text limit = %d body=%s calls=%d", response.Code, response.Body.String(), queueCalls)
	}
	assertAgentEmailOutboundCodedError(t, response, http.StatusBadRequest, "invalid_request", false)
}

func TestAgentEmailOutboundControlFailuresAlwaysUseCodedEnvelope(t *testing.T) {
	auth := func(_ context.Context, token string) (string, string, string, bool, error) {
		switch token {
		case "operator-token":
			return "op_1", "acc_1", "active", true, nil
		case "suspended-token":
			return "op_1", "acc_1", "suspended", true, nil
		case "pending-token":
			return "op_1", "acc_1", "pending", true, nil
		case "auth-error":
			return "", "", "", false, errors.New("private authentication failure")
		default:
			return "", "", "", false, nil
		}
	}
	handler := apiMux(Config{
		Authenticate: auth,
		GetAgentEmailSendControl: func(context.Context, string, string, string) (AgentEmailSendControl, error) {
			return AgentEmailSendControl{}, nil
		},
		SetAgentEmailSendControl: func(context.Context, string, string, string, string, int64) (AgentEmailSendControl, error) {
			return AgentEmailSendControl{}, nil
		},
	})
	tests := []struct {
		name, method, target, token, body, code string
		status                                  int
		retryable                               bool
	}{
		{name: "missing auth", method: http.MethodGet, target: "/v1/agents/agent_1/email-send", status: http.StatusUnauthorized, code: "auth_failed"},
		{name: "invalid auth", method: http.MethodGet, target: "/v1/agents/agent_1/email-send", token: "bad-token", status: http.StatusUnauthorized, code: "auth_failed"},
		{name: "auth backend unavailable", method: http.MethodGet, target: "/v1/agents/agent_1/email-send", token: "auth-error", status: http.StatusInternalServerError, code: "backend_unavailable", retryable: true},
		{name: "invalid query", method: http.MethodGet, target: "/v1/agents/agent_1/email-send?extra=1", token: "operator-token", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "inactive account", method: http.MethodGet, target: "/v1/agents/agent_1/email-send", token: "pending-token", status: http.StatusForbidden, code: "forbidden"},
		{name: "suspended cannot enable", method: http.MethodPatch, target: "/v1/agents/agent_1/email-send", token: "suspended-token", body: `{"send_state":"enabled"}`, status: http.StatusForbidden, code: "forbidden"},
		{name: "invalid control body", method: http.MethodPatch, target: "/v1/agents/agent_1/email-send", token: "operator-token", body: `{"send_state":"maybe"}`, status: http.StatusBadRequest, code: "invalid_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performAgentEmailOwnerRequest(
				handler, test.method, test.target, test.token, test.body, nil,
			)
			assertAgentEmailOutboundCodedError(t, response, test.status, test.code, test.retryable)
		})
	}
}

func assertAgentEmailOutboundCodedError(
	t *testing.T,
	response *httptest.ResponseRecorder,
	status int,
	code string,
	retryable bool,
) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d body=%s, want %d", response.Code, response.Body.String(), status)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode coded error %q: %v", response.Body.String(), err)
	}
	errorMessage, hasErrorMessage := body["error"].(string)
	if body["schema_version"] != "witself.v0" || body["code"] != code ||
		body["retryable"] != retryable || !hasErrorMessage || strings.TrimSpace(errorMessage) == "" {
		t.Fatalf("coded error = %#v, want code=%q retryable=%t", body, code, retryable)
	}
	if response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("cache control = %q", response.Header().Get("Cache-Control"))
	}
}
