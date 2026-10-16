package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

type fakeAccountCloseStore struct {
	events                 *[]string
	preflightErr, closeErr error
}

func (f fakeAccountCloseStore) CheckAccountClose(context.Context, string, string) error {
	*f.events = append(*f.events, "preflight")
	return f.preflightErr
}

func (f fakeAccountCloseStore) CloseAccount(context.Context, string, string, string) error {
	*f.events = append(*f.events, "close")
	return f.closeErr
}

func TestCloseAccountWithBillingOrdering(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		preflightErr, closeErr, wantErr error
		providerStatus                  int
		wantEvents                      []string
	}{
		{"success", nil, nil, nil, 200, []string{"preflight", "billing", "close"}},
		{"nonowner", store.ErrNotAccountOwner, nil, server.ErrNotAccountOwner, 200, []string{"preflight"}},
		{"default", store.ErrCannotCloseDefault, nil, server.ErrCannotCloseDefault, 200, []string{"preflight"}},
		{"vault preflight", store.ErrVaultLifecycleInProgress, nil, server.ErrConflict, 200, []string{"preflight"}},
		{"provider failure", nil, nil, server.ErrAccountBillingClosePending, 500, []string{"preflight", "billing"}},
		{"provider busy", nil, nil, server.ErrAccountBillingClosePending, 409, []string{"preflight", "billing"}},
		{"vault race recheck", nil, store.ErrVaultLifecycleInProgress, server.ErrConflict, 200, []string{"preflight", "billing", "close"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				events = append(events, "billing")
				if r.Header.Get("Authorization") != "Bearer owner" || r.Header.Get("X-Witself-Cell-Provision-Token") != "cell" {
					t.Error("missing bridge credentials")
				}
				w.WriteHeader(tc.providerStatus)
				_, _ = io.WriteString(w, `{"schema_version":"witself.v0","account_id":"acc_test","status":"billing_closed"}`)
			}))
			defer srv.Close()
			st := fakeAccountCloseStore{&events, tc.preflightErr, tc.closeErr}
			err := closeAccountWithBilling(context.Background(), st, srv.URL, "cell", "acc_test", "opr_owner", "requested", "owner")
			if !errors.Is(err, tc.wantErr) || !reflect.DeepEqual(events, tc.wantEvents) {
				t.Fatalf("error %v, events %v; want %v, %v", err, events, tc.wantErr, tc.wantEvents)
			}
		})
	}
}

func TestCloseAccountWithBillingConfiguredFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, token string
		wantErr               error
		wantEvents            []string
	}{
		{"selfhost", "", "", nil, []string{"close"}},
		{"invalid", " ", "cell", server.ErrAccountBillingClosePending, []string{"preflight"}},
		{"missing cell proof", "https://billing.example", "", server.ErrAccountBillingClosePending, []string{"preflight"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			st := fakeAccountCloseStore{events: &events}
			err := closeAccountWithBilling(context.Background(), st, tc.endpoint, tc.token, "acc_test", "opr_owner", "requested", "owner")
			if !errors.Is(err, tc.wantErr) || !reflect.DeepEqual(events, tc.wantEvents) {
				t.Fatalf("error %v, events %v; want %v, %v", err, events, tc.wantErr, tc.wantEvents)
			}
		})
	}
}
