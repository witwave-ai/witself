package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessagePeekHTTPContract(t *testing.T) {
	p := DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_peer", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}
	calls := 0
	cfg := Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) { return p, true, nil },
		PeekMessage: func(_ context.Context, got DomainPrincipal, id string) (Message, error) {
			calls++
			if got != p || id != "msg_1" {
				t.Fatal("peek did not receive the exact authenticated principal and message id")
			}
			return testClaimedMessage(true), nil
		},
		ReadMessage: func(context.Context, DomainPrincipal, string) (Message, error) {
			t.Fatal("observational route dispatched the mutating read")
			return Message{}, nil
		},
	}
	handler := apiMux(cfg)
	request := httptest.NewRequest(http.MethodGet, "/v1/messages/msg_1:peek", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || calls != 1 {
		t.Fatalf("peek status/calls = %d/%d, want 200/1", response.Code, calls)
	}
	if response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("peek content is cacheable")
	}
	var out struct {
		Schema  string  `json:"schema_version"`
		Message Message `json:"message"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Schema != "witself.v0" || out.Message.ID != "msg_1" || out.Message.Body != "secret body" ||
		string(out.Message.Payload) != `{"task":42}` || out.Message.ReadState.State != "unread" ||
		out.Message.Processing.State != "claimed" || out.Message.Processing.Generation != 9 {
		t.Fatal("peek did not preserve recipient content and observational state")
	}
	if out.Message.Processing.ClaimID != "" || out.Message.Processing.LeaseExpiresAt != nil ||
		strings.Contains(response.Body.String(), "mcl_secret") || strings.Contains(response.Body.String(), "lease_expires_at") {
		t.Fatal("peek exposed a processing capability")
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodHead, "/v1/messages/msg_1:peek", http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/messages/msg_1:peek", http.StatusNotFound},
		{http.MethodPut, "/v1/messages/msg_1:peek", http.StatusMethodNotAllowed},
		{http.MethodPatch, "/v1/messages/msg_1:peek", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/v1/messages/msg_1:peek", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/messages/msg_1:read", http.StatusNotFound},
		{http.MethodGet, "/v1/messages/msg_1:ack", http.StatusNotFound},
		{http.MethodGet, "/v1/messages/msg_1:peek:read", http.StatusNotFound},
		{http.MethodGet, "/v1/messages/:peek", http.StatusNotFound},
		{http.MethodGet, "/v1/messages/msg_1", http.StatusNotFound},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Authorization", "Bearer token")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status || calls != 1 || w.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatalf("rejected route status/calls/cache = %d/%d/%q", w.Code, calls, w.Header().Get("Cache-Control"))
			}
		})
	}
	cfg.PeekMessage = nil
	missing := httptest.NewRecorder()
	apiMux(cfg).ServeHTTP(missing, request)
	if missing.Code != http.StatusMethodNotAllowed || calls != 1 {
		t.Fatalf("missing callback status/calls = %d/%d", missing.Code, calls)
	}
}

func TestMessagePeekHTTPRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, kind, status, profile string
		err                         error
		noToken, invalidToken       bool
		want, wantCalls             int
	}{
		{name: "missing token", noToken: true, want: 401},
		{name: "invalid token", invalidToken: true, want: 401},
		{name: "operator", kind: PrincipalKindOperator, want: 403},
		{name: "inactive account", status: "suspended", want: 403},
		{name: "restricted curator", profile: AccessProfileCuratorPreview, want: 403},
		{name: "bad input", err: ErrBadInput, want: 400, wantCalls: 1},
		{name: "not recipient", err: ErrNotFound, want: 404, wantCalls: 1},
		{name: "revoked scope", err: ErrForbidden, want: 403, wantCalls: 1},
		{name: "disabled messaging", err: &FeatureNotEnabledError{Feature: "messaging"}, want: 403, wantCalls: 1},
		{name: "internal failure", err: errors.New("private backend marker"), want: 500, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, status := tc.kind, tc.status
			if kind == "" {
				kind = PrincipalKindAgent
			}
			if status == "" {
				status = "active"
			}
			calls := 0
			handler := apiMux(Config{
				AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
					return DomainPrincipal{Kind: kind, AccountStatus: status, AccessProfile: tc.profile}, !tc.invalidToken, nil
				},
				PeekMessage: func(context.Context, DomainPrincipal, string) (Message, error) {
					calls++
					return testClaimedMessage(true), tc.err
				},
			})
			r := httptest.NewRequest(http.MethodGet, "/v1/messages/msg_1:peek", nil)
			if !tc.noToken {
				r.Header.Set("Authorization", "Bearer token")
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.want || calls != tc.wantCalls || w.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatalf("refusal status/calls/cache = %d/%d/%q", w.Code, calls, w.Header().Get("Cache-Control"))
			}
			for _, marker := range []string{"secret body", "mcl_secret", "private backend marker"} {
				if strings.Contains(w.Body.String(), marker) {
					t.Fatal("refusal leaked private content")
				}
			}
		})
	}
}

func TestMessagePeekMetricsUseBoundedRoute(t *testing.T) {
	metrics := newRuntimeMetrics()
	handler := metrics.instrument(apiMux(Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
			return DomainPrincipal{Kind: PrincipalKindAgent, AccountStatus: "active"}, true, nil
		},
		PeekMessage: func(context.Context, DomainPrincipal, string) (Message, error) { return testMessage(true), nil },
	}))
	for _, id := range []string{"msg_private_one", "msg_private_two"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/messages/"+id+":peek", nil)
		r.Header.Set("Authorization", "Bearer token")
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}
	var output bytes.Buffer
	metrics.writePrometheus(&output)
	want := `witself_http_requests_total{method="GET",route="/v1/messages/{action}",status_class="2xx",result="success"} 2`
	if !strings.Contains(output.String(), want) || strings.Contains(output.String(), "msg_private_") || strings.Contains(output.String(), "secret body") {
		t.Fatal("peek metrics did not retain a value-free bounded route")
	}
}
