package stripe

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

// TestStripeLive runs against the REAL Stripe sandbox when
// WITSELF_TEST_STRIPE_SECRET_KEY is set (skipped otherwise): bootstraps the
// catalog prices by lookup_key, creates a customer, opens a real checkout
// session, exercises the read path, and cleans up the customer. No payment is
// completed (that needs a browser); the session URL's existence is the
// contract — the payment side is covered by Stripe's own test cards when a
// human (or stripe-cli) drives the browser step.
func TestStripeLive(t *testing.T) {
	key := os.Getenv("WITSELF_TEST_STRIPE_SECRET_KEY")
	if key == "" {
		t.Skip("set WITSELF_TEST_STRIPE_SECRET_KEY to run against the Stripe sandbox")
	}
	if !strings.HasPrefix(key, "sk_test_") {
		t.Fatalf("refusing to run: key is not a sandbox key (sk_test_...)")
	}
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	p, err := New(Config{SecretKey: key, Catalog: catalog})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	// Bootstrap: products/prices exist after this, by lookup_key.
	if err := p.EnsurePrices(ctx); err != nil {
		t.Fatalf("EnsurePrices against sandbox: %v", err)
	}
	// Idempotent: run again, resolves without creating.
	if err := p.EnsurePrices(ctx); err != nil {
		t.Fatalf("EnsurePrices second run: %v", err)
	}

	// A throwaway customer. Cleanup lists by email rather than holding the
	// one id, so a failure between create and return cannot leak — and any
	// litter from prior failed runs gets swept too.
	// Isolate every invocation. Reusing one account id eventually exhausts the
	// deterministic idempotency generations because cleanup deliberately
	// deletes each prior customer while Stripe retains its response for 24h.
	suffix := strconv.FormatInt(time.Now().UTC().UnixNano(), 36)
	accountID := "acct_live_test_" + suffix
	email := "live-test+" + suffix + "@witself.example"
	t.Cleanup(func() {
		cctx := context.Background()
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		q := url.Values{"email": {email}, "limit": {"100"}}
		if err := p.call(cctx, "GET", "/v1/customers?"+q.Encode(), nil, "", &list); err != nil {
			t.Logf("cleanup list: %v", err)
			return
		}
		for _, c := range list.Data {
			_ = p.call(cctx, "DELETE", "/v1/customers/"+c.ID, nil, "", nil)
		}
	})
	custID, err := p.EnsureCustomer(ctx, accountID, email)
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	if !strings.HasPrefix(custID, "cus_") {
		t.Fatalf("customer id = %q", custID)
	}

	// Idempotency: same account id -> same customer (Stripe replays the
	// original response for the same Idempotency-Key).
	again, err := p.EnsureCustomer(ctx, accountID, email)
	if err != nil || again != custID {
		t.Fatalf("EnsureCustomer retry = %q, %v; want %q", again, err, custID)
	}

	// A real checkout session: the needs_action(url) the CLI would print.
	act, err := p.Subscribe(ctx, custID, "standard")
	if err != nil || act.Done || !strings.Contains(act.URL, "checkout.stripe.com") {
		t.Fatalf("Subscribe = %+v, %v; want a live checkout URL", act, err)
	}

	// Setup + portal links mint too (portal was activated in the dashboard).
	if act, err := p.SetupLink(ctx, custID); err != nil || act.URL == "" {
		t.Fatalf("SetupLink = %+v, %v", act, err)
	}
	if url, err := p.PortalLink(ctx, custID); err != nil || !strings.Contains(url, "stripe.com") {
		t.Fatalf("PortalLink = %q, %v", url, err)
	}

	// Read path on a fresh customer: empty but well-formed. NextCharge
	// exercises POST /v1/invoices/create_preview against the pinned API
	// version — the invoice_upcoming_none -> nil mapping is live-verified.
	if inv, err := p.ListInvoices(ctx, custID); err != nil || len(inv) != 0 {
		t.Fatalf("ListInvoices fresh = %+v, %v; want empty", inv, err)
	}
	if pm, err := p.PaymentMethodOnFile(ctx, custID); err != nil || pm != nil {
		t.Fatalf("PaymentMethodOnFile fresh = %+v, %v; want nil", pm, err)
	}
	if next, err := p.NextCharge(ctx, custID); err != nil || next != nil {
		t.Fatalf("NextCharge fresh = %+v, %v; want nil", next, err)
	}

	// CancelPending expires the open subscription-mode checkout session from
	// Subscribe above — live-verifying the stale-tab defense (and tidying
	// the sandbox; the setup-mode session is deliberately left alone).
	if err := p.CancelPending(ctx, custID); err != nil {
		t.Fatalf("CancelPending: %v", err)
	}
}
