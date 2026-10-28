package localcell

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServiceAuthenticatedProbeRejectsBareSuccessAndForeignIdentity(t *testing.T) {
	credential := serviceHealthCredential{InstallationID: "lc_fixture", AccountID: "acc_fixture", OperatorID: "opr_fixture", Bearer: "fixture-only"}
	for _, tc := range []struct {
		name, body string
		status     int
		want       bool
	}{
		{"bare200", `{}`, 200, false},
		{"wrong-account", `{"principal":{"kind":"operator","account_id":"acc_other","operator_id":"opr_fixture"}}`, 200, false},
		{"wrong-operator", `{"principal":{"kind":"operator","account_id":"acc_fixture","operator_id":"opr_other"}}`, 200, false},
		{"refused", `{}`, 401, false},
		{"redirect", `{}`, 302, false},
		{"bound", `{"principal":{"kind":"operator","account_id":"acc_fixture","operator_id":"opr_fixture"}}`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if r.URL.Path != "/v1/auth/whoami" || r.URL.Query().Get("challenge") == "" || r.Header.Get("Authorization") != "Bearer "+credential.Bearer {
					t.Error("probe omitted authentication or challenge")
				}
				if tc.status == 302 {
					w.Header().Set("Location", "/unexpected")
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			client := localHTTPClient()
			defer client.CloseIdleConnections()
			err := serviceAuthenticatedProbe(t.Context(), client, srv.URL, credential)
			if !called || (err == nil) != tc.want {
				t.Fatal("authenticated probe accepted incorrect identity or status")
			}
		})
	}
}

func TestServiceHealthRequiresLiveProcessTree(t *testing.T) {
	c, _ := descriptorTestCell(t)
	if c.ServiceHealth(t.Context()) == nil {
		t.Fatal("descriptor alone counted as healthy")
	}
}
