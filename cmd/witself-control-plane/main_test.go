package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestSetupBillingRefusesFakeWithCell pins the review's HIGH secret-leak fix:
// mixing WITSELF_CP_BILLING_PROVIDER=fake with a real cell config would reuse
// the cell provision token as the fake's webhook signature, transmitting it
// in a request header on every legitimate webhook delivery — a fleet-wide
// plan-mint credential in any intermediary's access log. Refuse the combo.
func TestSetupBillingRefusesFakeWithCell(t *testing.T) {
	t.Setenv("WITSELF_CP_BILLING_PROVIDER", "fake")
	t.Setenv("WITSELF_CP_R2_ENDPOINT", "https://example.invalid")
	t.Setenv("WITSELF_CP_R2_BUCKET", "b")
	t.Setenv("WITSELF_CP_R2_ACCESS_KEY", "k")
	t.Setenv("WITSELF_CP_R2_SECRET_KEY", "s")
	t.Setenv("WITSELF_CP_CELL_ENDPOINT", "https://cell.example.invalid")
	t.Setenv("WITSELF_CP_CELL_PROVISION_TOKEN", "witself_prv_secret")

	err := setupBilling(context.Background(), http.NewServeMux())
	if err == nil {
		t.Fatal("fake+cell must be refused — the cell provision token would leak through webhook signatures")
	}
	if !strings.Contains(err.Error(), "fake") || !strings.Contains(err.Error(), "cell") {
		t.Fatalf("err = %v; want a message naming both fake and cell", err)
	}
}

// TestSetupBillingRequiresStripeWebhookSecret pins the review's finding: a
// stripe deploy without WITSELF_CP_STRIPE_WEBHOOK_SECRET boots cleanly, mints
// checkout links, takes payments — and refuses every webhook delivery, so
// paid activations are silently lost after Stripe's ~3-day retry horizon.
// The refusal must fire before any Stripe API call (hermetic).
func TestSetupBillingRequiresStripeWebhookSecret(t *testing.T) {
	t.Setenv("WITSELF_CP_BILLING_PROVIDER", "stripe")
	t.Setenv("WITSELF_CP_STRIPE_SECRET_KEY", "sk_test_hermetic")
	t.Setenv("WITSELF_CP_STRIPE_WEBHOOK_SECRET", "")

	err := setupBilling(context.Background(), http.NewServeMux())
	if err == nil {
		t.Fatal("stripe without a webhook secret must be refused at boot")
	}
	if !strings.Contains(err.Error(), "WITSELF_CP_STRIPE_WEBHOOK_SECRET") {
		t.Fatalf("err = %v; want a message naming the missing env var", err)
	}
}
