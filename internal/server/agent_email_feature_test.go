package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAgentEmailFeatureRefusalIsStructuredAcrossOwnerSurfaces(t *testing.T) {
	pilot, _ := testAgentEmailPilotConfig(t)
	principal := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_aaaaaaaaaaaaaaaa",
		AccountID: "acc_1", RealmID: "realm_aaaaaaaaaaaaaaaa",
		AccountStatus: "active", AccessProfile: AccessProfileFull,
	}
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return principal, true, nil
	}
	refusal := func() error {
		return &FeatureNotEnabledError{Feature: "agent_email_receive"}
	}
	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		headers map[string]string
		config  Config
	}{
		{
			name: "address", method: http.MethodGet, path: "/v1/email/address",
			config: Config{
				GetAgentEmailAddress: func(context.Context, DomainPrincipal) (AgentEmailAddress, error) {
					return AgentEmailAddress{}, refusal()
				},
			},
		},
		{
			name: "mailbox list", method: http.MethodGet, path: "/v1/email",
			config: Config{
				ListAgentEmails: func(context.Context, DomainPrincipal, AgentEmailListOptions) (AgentEmailPage, error) {
					return AgentEmailPage{}, refusal()
				},
			},
		},
		{
			name: "read", method: http.MethodPost,
			path: "/v1/email/emsg_aaaaaaaaaaaaaaaa:read",
			config: Config{
				ReadAgentEmail: func(context.Context, DomainPrincipal, string) (AgentEmailMessage, error) {
					return AgentEmailMessage{}, refusal()
				},
			},
		},
		{
			name: "claim", method: http.MethodPost,
			path: "/v1/email/emsg_aaaaaaaaaaaaaaaa:claim",
			body: `{"lease_seconds":90}`,
			headers: map[string]string{
				"Idempotency-Key": "claim-disabled-email",
			},
			config: Config{
				ClaimAgentEmail: func(context.Context, DomainPrincipal, string, ClaimAgentEmailRequest) (AgentEmailProcessing, error) {
					return AgentEmailProcessing{}, refusal()
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.config.AuthenticatePrincipal = auth
			tc.config.AgentEmailPilot = pilot
			handler := apiMux(tc.config)
			response := performAgentEmailOwnerRequest(
				handler, tc.method, tc.path, "token", tc.body, tc.headers,
			)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d body=%s, want 403", response.Code, response.Body.String())
			}
			if got := response.Header().Get("Cache-Control"); got != "private, no-store" {
				t.Fatalf("cache control = %q", got)
			}
			var body struct {
				Code      string `json:"code"`
				Feature   string `json:"feature"`
				Error     string `json:"error"`
				Retryable bool   `json:"retryable"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != "feature_not_enabled" ||
				body.Feature != "agent_email_receive" ||
				body.Error != "Sorry, this feature is not enabled on this account." ||
				body.Retryable {
				t.Fatalf("error body = %+v", body)
			}
		})
	}
}

func TestAgentEmailEntitlementPrecedesPilotEnrollment(t *testing.T) {
	pilot, _ := testAgentEmailPilotConfig(t)
	principal := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_zzzzzzzzzzzzzzzz",
		AccountID: "acc_personal", RealmID: "realm_aaaaaaaaaaaaaaaa",
		AccountStatus: "active", AccessProfile: AccessProfileFull,
	}
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return principal, true, nil
	}
	addressCalls := 0
	cfg := Config{
		AuthenticatePrincipal: auth,
		AgentEmailPilot:       pilot,
		RequireAgentEmailEntitlement: func(context.Context, DomainPrincipal) error {
			return &FeatureNotEnabledError{Feature: "agent_email_receive"}
		},
		GetAgentEmailAddress: func(context.Context, DomainPrincipal) (AgentEmailAddress, error) {
			addressCalls++
			return AgentEmailAddress{}, nil
		},
	}
	response := performAgentEmailOwnerRequest(
		apiMux(cfg), http.MethodGet, "/v1/email/address", "token", "", nil,
	)
	if response.Code != http.StatusForbidden ||
		!json.Valid(response.Body.Bytes()) ||
		!containsAll(response.Body.String(), `"code":"feature_not_enabled"`, `"feature":"agent_email_receive"`) ||
		addressCalls != 0 {
		t.Fatalf("disabled non-enrolled response = %d %s calls=%d", response.Code, response.Body.String(), addressCalls)
	}

	cfg.RequireAgentEmailEntitlement = func(context.Context, DomainPrincipal) error { return nil }
	response = performAgentEmailOwnerRequest(
		apiMux(cfg), http.MethodGet, "/v1/email/address", "token", "", nil,
	)
	if response.Code != http.StatusForbidden ||
		!containsAll(response.Body.String(), "agent is not enrolled in the email pilot") ||
		addressCalls != 0 {
		t.Fatalf("enabled non-enrolled response = %d %s calls=%d", response.Code, response.Body.String(), addressCalls)
	}
}

func containsAll(value string, values ...string) bool {
	for _, candidate := range values {
		if !strings.Contains(value, candidate) {
			return false
		}
	}
	return true
}
