package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
)

// The CLI tests cover route selection; this uses the existing real API mux to
// prove its operator-only whoami contract and the console's read-time checks.
func TestAccountConsoleLocalRealWhoami(t *testing.T) {
	const operatorBearer = "synthetic-local-console-operator"
	const agentBearer = "synthetic-local-console-agent"
	want := client.AccountConsoleIdentity{AccountID: "acc_console", OperatorID: "opr_console", Role: "account_owner"}
	var revoked atomic.Bool
	var whoamiCalls atomic.Int64
	cell := httptest.NewServer(apiMux(Config{
		Authenticate: func(_ context.Context, bearer string) (string, string, string, bool, error) {
			whoamiCalls.Add(1)
			if bearer == operatorBearer && !revoked.Load() {
				return want.OperatorID, want.AccountID, "active", true, nil
			}
			return "", "", "", false, nil
		},
		GetOperatorAccountRole: func(_ context.Context, accountID, operatorID string) (string, error) {
			if accountID != want.AccountID || operatorID != want.OperatorID {
				t.Error("role lookup did not preserve the authenticated operator")
			}
			return want.Role, nil
		},
		AuthenticatePrincipal: func(_ context.Context, bearer string) (DomainPrincipal, bool, error) {
			if bearer == agentBearer {
				return DomainPrincipal{
					Kind: PrincipalKindAgent, ID: "agent_console", AgentName: "console",
					AccountID: want.AccountID, RealmID: "realm_console", RealmName: "default",
					AccountStatus: "active",
				}, true, nil
			}
			return DomainPrincipal{}, false, nil
		},
	}))
	t.Cleanup(cell.Close)

	// Obtain a real agent identity so the negative whoami assertion cannot pass
	// merely because the cell does not recognize the agent bearer at all.
	resp := selfRequest(t, cell.URL+"/v1/self", agentBearer)
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatal("real self handler refused the synthetic agent")
	}
	var self client.SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&self); err != nil {
		t.Fatal("real self handler returned an invalid digest")
	}
	if self.Identity.AccountID != want.AccountID || self.Identity.AgentID != "agent_console" {
		t.Fatal("real self handler did not preserve the agent identity")
	}
	origin, err := client.AccountConsoleOrigin(cell.URL + "/")
	if err != nil || origin != cell.URL {
		t.Fatal("local console origin did not normalize")
	}
	principal, err := client.VerifyAccountConsoleManager(t.Context(), origin, operatorBearer, want.AccountID)
	if err != nil || principal != want || whoamiCalls.Load() != 1 {
		t.Fatal("real operator whoami did not produce the exact console binding")
	}
	manager := client.AccountConsoleManager{Endpoint: origin, BearerToken: operatorBearer, Identity: principal}
	reader, err := dashboard.NewReader(dashboard.Config{
		Endpoint: origin, BearerToken: agentBearer, Identity: self.Identity, AccountManager: &manager,
	})
	if err != nil {
		t.Fatal("console reader rejected the verified local binding")
	}
	readContext := func(t *testing.T, available bool) {
		t.Helper()
		raw, err := reader.Read(t.Context(), dashboard.ReadRequest{Resource: dashboard.ResourceAccountContext})
		if err != nil {
			t.Fatal("console context read failed")
		}
		var out struct {
			Available  bool   `json:"available"`
			AccountID  string `json:"account_id"`
			OperatorID string `json:"operator_id"`
			Role       string `json:"role"`
		}
		if json.Unmarshal(raw, &out) != nil || out.Available != available {
			t.Fatal("console context did not reflect current real API authority")
		}
		if available && (out.AccountID != want.AccountID || out.OperatorID != want.OperatorID || out.Role != want.Role) {
			t.Fatal("console context changed the real operator principal")
		}
		if !available && (out.AccountID != "" || out.OperatorID != "" || out.Role != "") {
			t.Fatal("unavailable console context retained its operator principal")
		}
	}

	t.Run("operator context revalidates on every read", func(t *testing.T) {
		before := whoamiCalls.Load()
		readContext(t, true)
		readContext(t, true)
		if whoamiCalls.Load() != before+2 {
			t.Fatal("console context reused authority without real whoami revalidation")
		}
	})
	t.Run("valid agent cannot become manager", func(t *testing.T) {
		resp := selfRequest(t, origin+"/v1/whoami", agentBearer)
		defer closeBody(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatal("real operator-only whoami accepted an agent bearer")
		}
		got, err := client.VerifyAccountConsoleManager(t.Context(), origin, agentBearer, want.AccountID)
		if !errors.Is(err, client.ErrAccountConsoleForbidden) || got != (client.AccountConsoleIdentity{}) {
			t.Fatal("agent bearer produced a console manager identity")
		}
	})
	t.Run("revoked operator loses context", func(t *testing.T) {
		revoked.Store(true)
		readContext(t, false)
		revoked.Store(false)
		readContext(t, true)
	})
	t.Run("stopped local cell loses context", func(t *testing.T) {
		cell.Close()
		if _, err := client.RevalidateAccountConsoleManager(t.Context(), manager); !errors.Is(err, client.ErrAccountConsoleUnavailable) {
			t.Fatal("stopped cell did not invalidate manager revalidation")
		}
		readContext(t, false)
	})
}
