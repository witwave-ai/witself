package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMessagingClientsPreserveFeatureNotEnabledRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","code":"feature_not_enabled","feature":"messaging","error":"Sorry, this feature is not enabled on this account.","retryable":false}`))
	}))
	defer srv.Close()

	calls := []struct {
		name string
		call func() error
	}{
		{
			name: "ordinary message",
			call: func() error {
				_, err := SendMessage(context.Background(), srv.URL, "token", SendMessageInput{
					To: "peer", Body: "hello",
				})
				return err
			},
		},
		{
			name: "message request",
			call: func() error {
				_, err := ListMessageRequests(context.Background(), srv.URL, "token", MessageRequestListOptions{})
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
			if featureErr.Feature != "messaging" || featureErr.Retryable ||
				featureErr.Error() != "Sorry, this feature is not enabled on this account." {
				t.Fatalf("feature error = %+v", featureErr)
			}
		})
	}
}
