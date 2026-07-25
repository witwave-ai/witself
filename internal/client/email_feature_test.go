package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentEmailClientsPreserveFeatureNotEnabledRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","code":"feature_not_enabled","feature":"agent_email_receive","error":"Sorry, this feature is not enabled on this account.","retryable":false}`))
	}))
	defer srv.Close()

	calls := []struct {
		name string
		call func() error
	}{
		{
			name: "address",
			call: func() error {
				_, err := ShowAgentEmailAddress(context.Background(), srv.URL, "token")
				return err
			},
		},
		{
			name: "mailbox list",
			call: func() error {
				_, err := ListAgentEmails(
					context.Background(), srv.URL, "token", AgentEmailListOptions{},
				)
				return err
			},
		},
		{
			name: "read",
			call: func() error {
				_, err := ReadAgentEmail(
					context.Background(), srv.URL, "token", "emsg_aaaaaaaaaaaaaaaa",
				)
				return err
			},
		},
		{
			name: "claim",
			call: func() error {
				_, err := ClaimAgentEmail(
					context.Background(), srv.URL, "token", "emsg_aaaaaaaaaaaaaaaa",
					ClaimAgentEmailInput{LeaseSeconds: 90, IdempotencyKey: "disabled-email"},
				)
				return err
			},
		},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			var featureErr *FeatureNotEnabledError
			if !errors.Is(err, ErrFeatureNotEnabled) || !errors.As(err, &featureErr) {
				t.Fatalf("error = %v, want FeatureNotEnabledError", err)
			}
			if featureErr.Feature != "agent_email_receive" || featureErr.Retryable ||
				featureErr.Error() != "Sorry, this feature is not enabled on this account." {
				t.Fatalf("feature error = %+v", featureErr)
			}
		})
	}
}
