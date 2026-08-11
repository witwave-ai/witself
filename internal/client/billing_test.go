package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

func TestBillingClientReadsProviderNeutralSurfaces(t *testing.T) {
	now := time.Date(2026, 8, 10, 20, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer owner" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/accounts/acct_1/billing":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0", "account_id": "acct_1",
				"billing_available": true, "configured": true,
				"subscription_status": "active", "billing_plan": "standard",
				"billing_plan_name": "Professional", "effective_plan": "enterprise",
				"effective_plan_name": "Enterprise", "applied_plan": "enterprise",
				"entitled_at": now, "payment_method": map[string]string{"label": "visa ****4242"},
				"next_charge": map[string]any{"date": now.AddDate(0, 1, 0), "amount_cents": 2500, "currency": "usd"},
			})
		case "GET /v1/accounts/acct_1/billing/invoices":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0", "account_id": "acct_1",
				"invoices": []map[string]any{{
					"number": "INV-1", "date": now, "amount_cents": 2500,
					"currency": "usd", "status": "paid",
					"pdf_url":    "https://billing.example/INV-1.pdf",
					"hosted_url": "https://billing.example/INV-1",
				}},
			})
		case "GET /v1/accounts/acct_1/billing/payments":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0", "account_id": "acct_1",
				"payments": []map[string]any{{
					"date": now, "amount_cents": 2500, "currency": "usd",
					"method": "visa ****4242", "status": "succeeded",
					"receipt_url": "https://billing.example/receipt/1",
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()

	// An advertised endpoint may already be the /v1 API base.
	summary, err := client.GetBillingSummary(ctx, srv.URL+"/v1/", "acct_1", "owner")
	if err != nil || !summary.Configured || summary.BillingPlan != "standard" ||
		summary.EffectivePlan != "enterprise" || summary.PaymentMethod == nil ||
		summary.PaymentMethod.Label != "visa ****4242" || summary.NextCharge == nil ||
		summary.NextCharge.AmountCents != 2500 {
		t.Fatalf("summary = %+v, %v", summary, err)
	}
	invoices, err := client.GetBillingInvoices(ctx, srv.URL, "acct_1", "owner")
	if err != nil || len(invoices.Invoices) != 1 || invoices.Invoices[0].Number != "INV-1" {
		t.Fatalf("invoices = %+v, %v", invoices, err)
	}
	payments, err := client.GetBillingPayments(ctx, srv.URL, "acct_1", "owner")
	if err != nil || len(payments.Payments) != 1 || payments.Payments[0].Status != "succeeded" {
		t.Fatalf("payments = %+v, %v", payments, err)
	}
}

func TestBillingCapabilityIsAccountBoundAndEndpointSafe(t *testing.T) {
	var accountID = "acct_1"
	var tokenAccountID = "acct_1"
	var backendKind = "self-hosted"
	var billing = map[string]any{
		"supported": true,
		"endpoint":  "https://billing.example/control-plane/",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/whoami":
			if r.Header.Get("Authorization") != "Bearer operator-private" {
				t.Fatalf("whoami auth=%q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"principal": map[string]string{
					"operator_id": "opr_1", "account_id": tokenAccountID,
				},
			})
		case "/v1/capabilities":
			if r.Header.Get("Authorization") != "" {
				t.Fatalf("capability auth=%q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"backend":        map[string]string{"kind": backendKind},
				"account":        map[string]string{"id": accountID},
				"billing":        billing,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	capability, err := client.GetBillingCapability(
		context.Background(), srv.URL+"/v1/", "acct_1", "operator-private")
	if err != nil || !capability.Supported ||
		capability.Endpoint != "https://billing.example/control-plane" {
		t.Fatalf("capability = %+v, %v", capability, err)
	}

	accountID = "acct_other"
	if _, err := client.GetBillingCapability(
		context.Background(), srv.URL, "acct_1", "operator-private"); err == nil {
		t.Fatal("mismatched capability account was accepted")
	}
	// Managed cells omit the deployment account block in production. Even if
	// an older cell emits its seeded default account, authenticated whoami is
	// the tenant fence and lets any correctly placed tenant proceed.
	backendKind = " MANAGED "
	capability, err = client.GetBillingCapability(
		context.Background(), srv.URL, "acct_1", "operator-private")
	if err != nil || !capability.Supported {
		t.Fatalf("managed multi-account capability = %+v, %v", capability, err)
	}
	backendKind = "self-hosted"
	accountID = "acct_1"
	tokenAccountID = "acct_other"
	if _, err := client.GetBillingCapability(
		context.Background(), srv.URL, "acct_1", "operator-private"); err == nil {
		t.Fatal("wrong authenticated account was accepted")
	}
	tokenAccountID = "acct_1"
	billing = map[string]any{
		"supported": true,
		"endpoint":  "http://billing.example/control-plane",
	}
	if _, err := client.GetBillingCapability(
		context.Background(), srv.URL, "acct_1", "operator-private"); err == nil {
		t.Fatal("insecure non-loopback billing endpoint was accepted")
	}
	billing = map[string]any{"supported": false, "reason": "self_hosted"}
	capability, err = client.GetBillingCapability(
		context.Background(), srv.URL, "acct_1", "operator-private")
	if err != nil || capability.Supported || capability.Reason != "self_hosted" {
		t.Fatalf("disabled capability = %+v, %v", capability, err)
	}
}

func TestBillingClientActionsAndEmailHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer owner" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/accounts/acct_1/billing:setup":
			if got := r.Header.Get("X-Witself-Email"); got != "billing@example.com" {
				t.Fatalf("email hint = %q", got)
			}
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","kind":"done"}`))
		case "/v1/accounts/acct_1/billing:portal":
			if got := r.Header.Get("X-Witself-Email"); got != "" {
				t.Fatalf("portal email hint = %q", got)
			}
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","kind":"action","url":"https://billing.example/portal"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()

	setup, err := client.CreateBillingSetup(ctx, srv.URL, "acct_1", "owner", "billing@example.com")
	if err != nil || setup.Kind != "done" || setup.URL != "" {
		t.Fatalf("setup = %+v, %v", setup, err)
	}
	portal, err := client.CreateBillingPortal(ctx, srv.URL, "acct_1", "owner")
	if err != nil || portal.Kind != "action" || portal.URL != "https://billing.example/portal" {
		t.Fatalf("portal = %+v, %v", portal, err)
	}
}

func TestBillingClientRejectsMalformedContracts(t *testing.T) {
	tests := []struct {
		name string
		body string
		call func(context.Context, string) error
	}{
		{
			name: "wrong account",
			body: `{"schema_version":"witself.v0","account_id":"acct_other","billing_plan":"free","effective_plan":"free"}`,
			call: func(ctx context.Context, endpoint string) error {
				_, err := client.GetBillingSummary(ctx, endpoint, "acct_1", "owner")
				return err
			},
		},
		{
			name: "action missing url",
			body: `{"schema_version":"witself.v0","kind":"action"}`,
			call: func(ctx context.Context, endpoint string) error {
				_, err := client.CreateBillingPortal(ctx, endpoint, "acct_1", "owner")
				return err
			},
		},
		{
			name: "done with url",
			body: `{"schema_version":"witself.v0","kind":"done","url":"https://billing.example/unexpected"}`,
			call: func(ctx context.Context, endpoint string) error {
				_, err := client.CreateBillingSetup(ctx, endpoint, "acct_1", "owner", "")
				return err
			},
		},
		{
			name: "action with encoded control",
			body: `{"schema_version":"witself.v0","kind":"action","url":"https://billing.example/%0aescape"}`,
			call: func(ctx context.Context, endpoint string) error {
				_, err := client.CreateBillingSetup(ctx, endpoint, "acct_1", "owner", "")
				return err
			},
		},
		{
			name: "unsafe invoice link",
			body: `{"schema_version":"witself.v0","account_id":"acct_1","invoices":[{"pdf_url":"https://billing.example/%E2%80%AEspoof"}]}`,
			call: func(ctx context.Context, endpoint string) error {
				_, err := client.GetBillingInvoices(ctx, endpoint, "acct_1", "owner")
				return err
			},
		},
		{
			name: "unsafe pending link",
			body: `{"schema_version":"witself.v0","account_id":"acct_1","billing_plan":"free","effective_plan":"free","pending":{"url":"https://billing.example/%5cevil"}}`,
			call: func(ctx context.Context, endpoint string) error {
				_, err := client.GetBillingSummary(ctx, endpoint, "acct_1", "owner")
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			if err := tc.call(context.Background(), srv.URL); err == nil || !strings.Contains(err.Error(), "control plane") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
