package agenttui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
)

// Real closed Reader and core projections, backed solely by a synthetic HTTP
// loopback fixture. No local inventory, real credentials, or providers.
func TestAccountActualReaderEnvelopes(t *testing.T) {
	demo := NewDemoSource()
	payloads := map[string]object{}
	for resource, path := range map[dashboard.Resource]string{dashboard.ResourceAccountOverview: "/v1/account", dashboard.ResourceAccountPlan: "/v1/accounts/acct_studio_demo/plan", dashboard.ResourceAccountSupport: "/v1/support/tickets", dashboard.ResourceAccountSupportTicket: "/v1/support/tickets/tkt_demo_guide"} {
		raw, err := demo.Read(context.Background(), dashboard.ReadRequest{Resource: resource, ID: "tkt_demo_guide"})
		if err != nil {
			t.Fatal(err)
		}
		var o object
		if err = json.Unmarshal(raw, &o); err != nil {
			t.Fatal(err)
		}
		o["schema_version"] = "witself.v0"
		switch resource {
		case dashboard.ResourceAccountPlan:
			o["apply_blocked"] = ""
			o["account_id"] = "acct_studio_demo"
		case dashboard.ResourceAccountSupport:
			for _, ticket := range list(o, "tickets") {
				ticket["account_id"] = "acct_studio_demo"
			}
		case dashboard.ResourceAccountSupportTicket:
			obj(o["ticket"])["account_id"] = "acct_studio_demo"
			for _, message := range list(o, "messages") {
				message["account_id"] = "acct_studio_demo"
				message["ticket_id"] = "tkt_demo_guide"
			}
		}
		payloads[path] = o
	}
	payloads["/v1/accounts/acct_studio_demo/billing"] = object{"schema_version": "witself.v0", "account_id": "acct_studio_demo", "billing_plan": "studio", "effective_plan": "studio", "next_charge": object{"amount_cents": json.Number("9007199254740993"), "currency": "JPY"}}
	payloads["/v1/accounts/acct_studio_demo/billing/invoices"] = object{"schema_version": "witself.v0", "account_id": "acct_studio_demo", "invoices": []any{object{"number": "core-1", "amount_cents": 0, "currency": "USD"}}}
	payloads["/v1/accounts/acct_studio_demo/billing/payments"] = object{"schema_version": "witself.v0", "account_id": "acct_studio_demo", "payments": []any{object{"amount_cents": nil, "currency": "EUR"}}}
	var scans, details atomic.Int32
	fixtureURL := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected synthetic request %s %s", r.Method, r.URL.Host)
		}
		path := r.URL.Path
		want := "Bearer synthetic-manager"
		if path == "/v1/capabilities" {
			want = ""
		}
		if r.Header.Get("Authorization") != want {
			t.Errorf("wrong principal on %s", path)
		}
		var body object
		switch path {
		case "/v1/whoami":
			body = object{"schema_version": "witself.v0", "principal": object{"kind": "operator", "account_id": "acct_studio_demo", "operator_id": "op_manager_demo", "account_role": "account_owner"}}
		case "/v1/capabilities":
			body = object{"schema_version": "witself.v0", "backend": object{"kind": "managed"}, "billing": object{"supported": true, "endpoint": fixtureURL}}
		default:
			body = payloads[path]
			if body == nil {
				t.Errorf("unexpected resource %s", path)
			}
			if strings.HasSuffix(path, "/tkt_demo_guide") {
				details.Add(1)
			}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	fixtureURL = server.URL
	reader, err := dashboard.NewReader(dashboard.Config{Endpoint: fixtureURL, BearerToken: "synthetic-agent", Identity: client.SelfIdentity{AccountID: "acct_studio_demo", AgentID: "agent_atlas_demo"}, AccountManager: &dashboard.AccountManager{Endpoint: fixtureURL, BearerToken: "synthetic-manager", Identity: dashboard.AccountManagerIdentity{AccountID: "acct_studio_demo", OperatorID: "op_manager_demo", Role: "account_owner"}}, AccountClientsScan: func(ctx context.Context, id string) (dashboard.AccountClientsReport, error) {
		scans.Add(1)
		if id != "acct_studio_demo" {
			t.Error("wrong scan account")
		}
		return dashboard.AccountClientsReport{SchemaVersion: dashboard.AccountClientsSchema, DeviceLabel: "This device", CheckedAt: demoTime, ScanStatus: "complete", Entries: []dashboard.AccountClientEntry{{Runtime: "codex", RecordedVersion: "0.1.0", ExecutableStatus: "present", ConfigurationStatus: "match", ConfigurationScope: "mcp_registration", EffectiveVerification: "not_run"}}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	m := testModel(t, reader)
	authorizeAccount(t, m)
	accountPress(t, m, "8")
	for _, section := range accountSections {
		selectAccount(t, m, section)
		if m.account.status != "ready" {
			t.Fatalf("actual %s envelope rejected: %s", section, m.account.status)
		}
		if section == "clients" {
			if scans.Load() != 0 || str(m.account.data, "status") != "not_checked" {
				t.Fatal("passive core scan")
			}
			accountPress(t, m, "x")
			accountPress(t, m, "r")
			if scans.Load() != 1 || str(m.account.data, "status") != "checked" {
				t.Fatal("core cached scan seam broken")
			}
		}
		if section == "billing" {
			if got := accountAmount(obj(obj(m.account.data["summary"])["next_charge"])); got != "9007199254740993 cents · JPY" {
				t.Fatal(got)
			}
		}
		if section == "support" {
			if details.Load() != 0 {
				t.Fatal("passive core body read")
			}
			accountPress(t, m, "enter")
			m.account.ticket.Lock()
			status := m.account.ticket.status
			m.account.ticket.Unlock()
			if status != "ready" || details.Load() != 1 {
				t.Fatal("core selected thread failed")
			}
		}
	}
}
