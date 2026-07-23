package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSetupBillingRefusesFakeWithCell pins the review's HIGH secret-leak fix:
// mixing WITSELF_CP_BILLING_PROVIDER=fake with a real cell config would reuse
// the cell provision token as the fake's webhook signature, transmitting it
// in a request header on every legitimate webhook delivery — a fleet-wide
// plan-mint credential in any intermediary's access log. Refuse the combo.
func TestSetupBillingRefusesFakeWithCell(t *testing.T) {
	t.Setenv("WITSELF_CP_PLAN_LIFECYCLE_ENABLED", "true")
	t.Setenv("WITSELF_CP_BRIDGE_URL", "https://bridge.example.invalid")
	t.Setenv("WITSELF_CP_BRIDGE_TOKEN", "bridge-secret")
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
	t.Setenv("WITSELF_CP_PLAN_LIFECYCLE_ENABLED", "true")
	t.Setenv("WITSELF_CP_BRIDGE_URL", "https://bridge.example.invalid")
	t.Setenv("WITSELF_CP_BRIDGE_TOKEN", "bridge-secret")
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

func TestSetupBillingRequiresSecureBridgeURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "plaintext", url: "http://bridge.example.invalid"},
		{name: "userinfo", url: "https://user:secret@bridge.example.invalid"},
		{name: "query", url: "https://bridge.example.invalid?token=secret"},
		{name: "fragment", url: "https://bridge.example.invalid#fragment"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WITSELF_CP_PLAN_LIFECYCLE_ENABLED", "true")
			t.Setenv("WITSELF_CP_BRIDGE_URL", tc.url)
			t.Setenv("WITSELF_CP_BRIDGE_TOKEN", "bridge-secret")
			t.Setenv("WITSELF_CP_BILLING_PROVIDER", "")
			t.Setenv("WITSELF_CP_R2_ENDPOINT", "https://example.invalid")
			t.Setenv("WITSELF_CP_R2_BUCKET", "b")
			t.Setenv("WITSELF_CP_R2_ACCESS_KEY", "k")
			t.Setenv("WITSELF_CP_R2_SECRET_KEY", "s")

			err := setupBilling(context.Background(), http.NewServeMux())
			if err == nil {
				t.Fatalf("setup with bridge URL %q succeeded; want refusal", tc.url)
			}
			if !strings.Contains(err.Error(), "WITSELF_CP_BRIDGE_URL") ||
				!strings.Contains(err.Error(), "HTTPS") {
				t.Fatalf("err = %v; want a secure bridge URL message", err)
			}
		})
	}
}

func TestSetupBillingRoutesStayOffUnlessExplicitlyEnabled(t *testing.T) {
	t.Setenv("WITSELF_CP_BILLING_PROVIDER", "stripe")
	mux := http.NewServeMux()
	if err := setupBilling(context.Background(), mux); err != nil {
		t.Fatalf("disabled setup = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/plans", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled route status = %d; want 404", rec.Code)
	}
}

func TestExplicitPlanLifecycleBooleanRejectsTypos(t *testing.T) {
	t.Setenv("WITSELF_CP_PLAN_LIFECYCLE_ENABLED", "truthy")
	err := setupBilling(context.Background(), http.NewServeMux())
	if err == nil || !strings.Contains(err.Error(), "must be true or false") {
		t.Fatalf("setup error = %v; want strict boolean refusal", err)
	}
}

func TestBridgeAdminAuthenticatorRequiresVerifiedImmutableIdentity(t *testing.T) {
	auth := bridgeAdminAuthenticator("bridge-secret")
	tests := []struct {
		name, bearer, adminID, handle string
		want                          bool
	}{
		{
			name:   "verified",
			bearer: "bridge-secret", adminID: "adm_abcdefghijklmnopqrst",
			handle: "scott", want: true,
		},
		{
			name:   "wrong bridge token",
			bearer: "wrong", adminID: "adm_abcdefghijklmnopqrst",
			handle: "scott",
		},
		{name: "missing id", bearer: "bridge-secret", handle: "scott"},
		{
			name:   "malformed id",
			bearer: "bridge-secret", adminID: "adm_short", handle: "scott",
		},
		{
			name:   "missing handle",
			bearer: "bridge-secret", adminID: "adm_abcdefghijklmnopqrst",
		},
		{
			name:   "reserved handle",
			bearer: "bridge-secret", adminID: "adm_abcdefghijklmnopqrst",
			handle: "system",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actor, ok, err := auth(
				context.Background(), tc.bearer, tc.adminID, tc.handle)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.want {
				t.Fatalf("authorized = %t; want %t", ok, tc.want)
			}
			if tc.want &&
				(actor.ID != tc.adminID || actor.Handle != tc.handle) {
				t.Fatalf("actor = %+v", actor)
			}
		})
	}
}
