package main

import (
	"context"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/plans"
)

func setValidStripeControlPlaneEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WITSELF_CP_STRIPE_MODE", "test")
	t.Setenv("WITSELF_CP_STRIPE_SECRET_KEY", "sk_test_hermetic")
	t.Setenv("WITSELF_CP_STRIPE_WEBHOOK_SECRET", "whsec_hermetic")
	t.Setenv("WITSELF_CP_STRIPE_SUCCESS_URL", "https://console.example.invalid/billing/success")
	t.Setenv("WITSELF_CP_STRIPE_CANCEL_URL", "https://console.example.invalid/billing/cancelled")
	t.Setenv("WITSELF_CP_STRIPE_PORTAL_RETURN_URL", "https://console.example.invalid/billing")
	t.Setenv("WITSELF_CP_STRIPE_PORTAL_CONFIGURATION_ID", "bpc_hermetic123")
	t.Setenv("WITSELF_CP_STRIPE_TEST_CLOCK_ID", "")
	t.Setenv("WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST", "acct_founder,acct_sandbox")
}

func testCatalog(t *testing.T) *plans.Catalog {
	t.Helper()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestStripeModeRequiresExactlyMatchingSecretKeyPrefix(t *testing.T) {
	tests := []struct {
		name string
		mode string
		key  string
	}{
		{name: "test mode rejects live key", mode: "test", key: "sk_live_hermetic"},
		{name: "live mode rejects test key", mode: "live", key: "sk_test_hermetic"},
		{name: "mode is case sensitive", mode: "TEST", key: "sk_test_hermetic"},
		{name: "key prefix is case sensitive", mode: "test", key: "SK_TEST_hermetic"},
		{name: "prefix alone is not a key", mode: "test", key: "sk_test_"},
		{name: "leading whitespace is not canonical", mode: "test", key: " sk_test_hermetic"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateStripeModeAndKey(tc.mode, tc.key); err == nil {
				t.Fatalf("validateStripeModeAndKey(%q, %q) succeeded", tc.mode, tc.key)
			}
		})
	}
	for _, tc := range []struct {
		mode string
		key  string
	}{
		{mode: "test", key: "sk_test_hermetic"},
		{mode: "live", key: "sk_live_hermetic"},
	} {
		if err := validateStripeModeAndKey(tc.mode, tc.key); err != nil {
			t.Fatalf("validateStripeModeAndKey(%q, %q) = %v", tc.mode, tc.key, err)
		}
	}
}

func TestBillingAccountAllowlistEmptyFailsClosed(t *testing.T) {
	gate, err := billingAccountAllowlistGate("")
	if err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []string{"acct_founder", "", "acct_sandbox"} {
		enabled, err := gate(context.Background(), accountID)
		if err != nil {
			t.Fatal(err)
		}
		if enabled {
			t.Fatalf("empty allowlist enabled %q", accountID)
		}
	}
}

func TestBillingAccountAllowlistIsStrictAndExact(t *testing.T) {
	for _, raw := range []string{
		" acct_founder",
		"acct_founder ",
		"acct_founder, acct_sandbox",
		"acct_founder,",
		",acct_founder",
		"acct_founder,,acct_sandbox",
		"acct/founder",
		"acct_founder,acct_founder",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := billingAccountAllowlistGate(raw); err == nil {
				t.Fatalf("billingAccountAllowlistGate(%q) succeeded", raw)
			}
		})
	}

	gate, err := billingAccountAllowlistGate("acct_founder,Acct_Team-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		accountID string
		want      bool
	}{
		{accountID: "acct_founder", want: true},
		{accountID: "Acct_Team-1", want: true},
		{accountID: "acct_found", want: false},
		{accountID: "acct_founder_extra", want: false},
		{accountID: "ACCT_FOUNDER", want: false},
		{accountID: " Acct_Team-1", want: false},
	} {
		enabled, err := gate(context.Background(), tc.accountID)
		if err != nil {
			t.Fatal(err)
		}
		if enabled != tc.want {
			t.Fatalf("gate(%q) = %t; want %t", tc.accountID, enabled, tc.want)
		}
	}
}

func TestCanonicalStripeReturnURLs(t *testing.T) {
	const envName = "WITSELF_TEST_STRIPE_RETURN_URL"
	for _, rawURL := range []string{
		"",
		"http://console.example.invalid/billing",
		"https://user:secret@console.example.invalid/billing",
		"https://console.example.invalid/billing?account=acct_founder",
		"https://console.example.invalid/billing#done",
		"https://console.example.invalid/billing?",
		" https://console.example.invalid/billing",
		"https://console.example.invalid/billing\nnext",
		"https://console.example.invalid/billing\u0085next",
		"https:///billing",
	} {
		t.Run(strings.ReplaceAll(rawURL, "/", "_"), func(t *testing.T) {
			t.Setenv(envName, rawURL)
			if _, err := canonicalHTTPSURLFromEnv(envName); err == nil {
				t.Fatalf("canonicalHTTPSURLFromEnv accepted %q", rawURL)
			}
		})
	}

	const valid = "https://console.example.invalid/billing/success"
	t.Setenv(envName, valid)
	got, err := canonicalHTTPSURLFromEnv(envName)
	if err != nil {
		t.Fatal(err)
	}
	if got != valid {
		t.Fatalf("URL = %q; want %q", got, valid)
	}
}

func TestStripeControlPlaneConfigRequiresAndPassesSafePortalConfig(t *testing.T) {
	setValidStripeControlPlaneEnv(t)
	catalog := testCatalog(t)

	cfg, gate, err := stripeControlPlaneConfig(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SecretKey != "sk_test_hermetic" ||
		cfg.SuccessURL != "https://console.example.invalid/billing/success" ||
		cfg.CancelURL != "https://console.example.invalid/billing/cancelled" ||
		cfg.PortalReturnURL != "https://console.example.invalid/billing" ||
		cfg.PortalConfigurationID != "bpc_hermetic123" || cfg.Catalog != catalog {
		t.Fatalf("Stripe config did not preserve validated settings: %+v", cfg)
	}
	for accountID, want := range map[string]bool{
		"acct_founder": true,
		"acct_sandbox": true,
		"acct_other":   false,
	} {
		got, gateErr := gate(context.Background(), accountID)
		if gateErr != nil || got != want {
			t.Fatalf("gate(%q) = %t, %v; want %t", accountID, got, gateErr, want)
		}
	}

	for _, portalConfigurationID := range []string{
		"", "bpc_", " bpc_hermetic123", "bpc_bad-id", "pc_hermetic123",
	} {
		t.Run("invalid_"+portalConfigurationID, func(t *testing.T) {
			setValidStripeControlPlaneEnv(t)
			t.Setenv("WITSELF_CP_STRIPE_PORTAL_CONFIGURATION_ID", portalConfigurationID)
			_, _, err := stripeControlPlaneConfig(catalog)
			if err == nil || !strings.Contains(err.Error(), "WITSELF_CP_STRIPE_PORTAL_CONFIGURATION_ID") {
				t.Fatalf("error = %v; want portal configuration refusal", err)
			}
		})
	}
}

func TestStripeControlPlaneConfigRejectsMalformedWebhookSecret(t *testing.T) {
	catalog := testCatalog(t)
	for _, webhookSecret := range []string{
		"", "whsec_", " whsec_hermetic", "whsec_hermetic ",
		"secret_hermetic", "WHSEC_hermetic", "whsec_bad-value",
	} {
		t.Run(webhookSecret, func(t *testing.T) {
			setValidStripeControlPlaneEnv(t)
			t.Setenv("WITSELF_CP_STRIPE_WEBHOOK_SECRET", webhookSecret)
			_, _, err := stripeControlPlaneConfig(catalog)
			if err == nil ||
				!strings.Contains(err.Error(), "WITSELF_CP_STRIPE_WEBHOOK_SECRET") {
				t.Fatalf("error = %v; want webhook-secret refusal", err)
			}
		})
	}
}

func TestStripeControlPlaneConfigGatesTestClockToTestMode(t *testing.T) {
	catalog := testCatalog(t)
	setValidStripeControlPlaneEnv(t)
	t.Setenv("WITSELF_CP_STRIPE_TEST_CLOCK_ID", "clock_hermetic123")
	t.Setenv("WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST", "acct_sandbox")
	cfg, _, err := stripeControlPlaneConfig(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TestClockID != "clock_hermetic123" {
		t.Fatalf("TestClockID = %q", cfg.TestClockID)
	}

	for _, test := range []struct {
		name, mode, key, clock string
	}{
		{name: "live mode", mode: "live", key: "sk_live_hermetic", clock: "clock_hermetic123"},
		{name: "prefix only", mode: "test", key: "sk_test_hermetic", clock: "clock_"},
		{name: "whitespace", mode: "test", key: "sk_test_hermetic", clock: " clock_hermetic123"},
		{name: "wrong resource", mode: "test", key: "sk_test_hermetic", clock: "test_clock_hermetic123"},
	} {
		t.Run(test.name, func(t *testing.T) {
			setValidStripeControlPlaneEnv(t)
			t.Setenv("WITSELF_CP_STRIPE_MODE", test.mode)
			t.Setenv("WITSELF_CP_STRIPE_SECRET_KEY", test.key)
			t.Setenv("WITSELF_CP_STRIPE_TEST_CLOCK_ID", test.clock)
			if _, _, err := stripeControlPlaneConfig(catalog); err == nil ||
				!strings.Contains(err.Error(), "WITSELF_CP_STRIPE_TEST_CLOCK_ID") {
				t.Fatalf("error = %v; want test-clock refusal", err)
			}
		})
	}
}

func TestStripeControlPlaneConfigBoundsTestClockCohort(t *testing.T) {
	catalog := testCatalog(t)
	for _, test := range []struct {
		name      string
		allowlist string
		wantError bool
	}{
		{name: "dark", allowlist: ""},
		{name: "one disposable account", allowlist: "acct_sandbox"},
		{name: "broader cohort", allowlist: "acct_sandbox,acct_other", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			setValidStripeControlPlaneEnv(t)
			t.Setenv("WITSELF_CP_STRIPE_TEST_CLOCK_ID", "clock_hermetic123")
			t.Setenv("WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST", test.allowlist)
			_, _, err := stripeControlPlaneConfig(catalog)
			if test.wantError {
				if err == nil ||
					!strings.Contains(err.Error(), "at most one account") {
					t.Fatalf("error = %v; want bounded test-clock cohort refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStripeControlPlaneConfigRejectsEachUnsafeReturnURL(t *testing.T) {
	catalog := testCatalog(t)
	for _, envName := range []string{
		"WITSELF_CP_STRIPE_SUCCESS_URL",
		"WITSELF_CP_STRIPE_CANCEL_URL",
		"WITSELF_CP_STRIPE_PORTAL_RETURN_URL",
	} {
		t.Run(envName, func(t *testing.T) {
			setValidStripeControlPlaneEnv(t)
			t.Setenv(envName, "https://console.example.invalid/billing?unsafe=true")
			_, _, err := stripeControlPlaneConfig(catalog)
			if err == nil || !strings.Contains(err.Error(), envName) {
				t.Fatalf("error = %v; want %s refusal", err, envName)
			}
		})
	}
}
