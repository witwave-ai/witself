package stripe

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

const stripeLiveTestOptInEnv = "WITSELF_TEST_STRIPE_LIVE"

// stripeLiveTestOptedIn deliberately accepts one exact value. A broadly set
// Stripe test secret must never be enough to make an ordinary `go test` reach
// the network or create sandbox objects.
func stripeLiveTestOptedIn(value string) bool { return value == "1" }

func TestStripeLiveOptInIsExact(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "1", want: true},
		{value: "", want: false},
		{value: "0", want: false},
		{value: "true", want: false},
		{value: "TRUE", want: false},
		{value: " 1", want: false},
		{value: "1 ", want: false},
	} {
		t.Run(strconv.Quote(tc.value), func(t *testing.T) {
			t.Parallel()
			if got := stripeLiveTestOptedIn(tc.value); got != tc.want {
				t.Fatalf("stripeLiveTestOptedIn(%q) = %t, want %t", tc.value, got, tc.want)
			}
		})
	}
}

// TestStripeLive runs against the REAL Stripe sandbox when
// WITSELF_TEST_STRIPE_LIVE=1 AND WITSELF_TEST_STRIPE_SECRET_KEY are both set.
// The Professional catalog price must already be provisioned: this test never
// creates persistent Product/Price fixtures. It creates a uniquely marked
// customer plus subscription/setup Checkout sessions, exercises the read path,
// expires every still-open session, and deletes the customer. No payment is
// completed (that needs a browser); the session URL's existence is the
// contract.
func TestStripeLive(t *testing.T) {
	if !stripeLiveTestOptedIn(os.Getenv(stripeLiveTestOptInEnv)) {
		t.Skip("external Stripe calls disabled; set WITSELF_TEST_STRIPE_LIVE=1 together with WITSELF_TEST_STRIPE_SECRET_KEY to opt in")
	}
	key := os.Getenv("WITSELF_TEST_STRIPE_SECRET_KEY")
	if key == "" {
		t.Skip("WITSELF_TEST_STRIPE_LIVE=1 is set, but WITSELF_TEST_STRIPE_SECRET_KEY is absent")
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

	// Resolve, validate, and cache the pre-provisioned price without allowing
	// this test to leave permanent Product/Price objects behind.
	if err := cacheExistingLiveTestPrice(ctx, p, catalog, "standard"); err != nil {
		t.Fatalf("Professional sandbox price prerequisite: %v", err)
	}

	// Isolate and visibly mark every transient object. Reusing one account id
	// eventually exhausts deterministic idempotency generations because cleanup
	// deletes the customer while Stripe retains its response for about 24h.
	suffix := strconv.FormatInt(time.Now().UTC().UnixNano(), 36)
	testScope := "stripe_live_test_" + suffix
	accountID := "acct_" + testScope
	email := testScope + "@witself.example"
	createdCustomers := map[string]struct{}{}
	t.Cleanup(func() {
		cleanupStripeLiveTest(t, p, accountID, email, createdCustomers)
	})
	custID, err := p.EnsureCustomer(ctx, accountID, email)
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	if !strings.HasPrefix(custID, "cus_") {
		t.Fatalf("customer id = %q", custID)
	}
	createdCustomers[custID] = struct{}{}
	if err := p.call(ctx, "POST", "/v1/customers/"+custID, url.Values{
		"name":                              {"Witself Stripe live test " + suffix},
		"metadata[witself_test_scope]":      {testScope},
		"metadata[witself_test_disposable]": {"true"},
	}, "witself-live-test-mark-"+suffix, nil); err != nil {
		t.Fatalf("mark test customer: %v", err)
	}

	// Idempotency: same account id -> same customer (Stripe replays the
	// original response for the same Idempotency-Key).
	again, err := p.EnsureCustomer(ctx, accountID, email)
	if err != nil || again != custID {
		t.Fatalf("EnsureCustomer retry = %q, %v; want %q", again, err, custID)
	}

	// A real checkout session: the needs_action(url) the CLI would print.
	subscribeOperation := "bop_" + testScope + "_subscribe"
	act, err := p.SubscribeIdempotent(ctx, custID, "standard", subscribeOperation)
	if err != nil || act.Done || !strings.Contains(act.URL, "checkout.stripe.com") {
		t.Fatalf("Subscribe = %+v, %v; want a live checkout URL", act, err)
	}

	setupOperation := "bop_" + testScope + "_setup"
	if act, err := p.SetupLinkIdempotent(ctx, custID, setupOperation); err != nil || act.URL == "" {
		t.Fatalf("SetupLink = %+v, %v", act, err)
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

	// CancelPending expires the subscription-mode session. Cleanup independently
	// lists and expires every remaining open mode (including setup) before it
	// deletes the customer, so even an assertion failure leaves no usable link.
	if err := p.CancelPendingIdempotent(ctx, custID, "bop_"+testScope+"_cancel"); err != nil {
		t.Fatalf("CancelPending: %v", err)
	}
}

// cacheExistingLiveTestPrice validates the separately provisioned active
// catalog price and primes this Provider's private cache. That lets the live
// test exercise Checkout without creating persistent Stripe fixtures.
func cacheExistingLiveTestPrice(
	ctx context.Context,
	p *Provider,
	catalog *plans.Catalog,
	planID string,
) error {
	plan, ok := catalog.Get(planID)
	if !ok || !plan.Purchasable() {
		return fmt.Errorf("catalog plan %q is not purchasable", planID)
	}
	var list struct {
		Data []struct {
			ID         string `json:"id"`
			Active     bool   `json:"active"`
			UnitAmount int64  `json:"unit_amount"`
			Currency   string `json:"currency"`
			Product    string `json:"product"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	}
	q := url.Values{"active": {"true"}, "limit": {"10"}}
	q.Add("lookup_keys[]", lookupKey(planID))
	if err := p.call(ctx, "GET", "/v1/prices?"+q.Encode(), nil, "", &list); err != nil {
		return err
	}
	if list.HasMore || len(list.Data) != 1 {
		return fmt.Errorf("lookup key %q resolved %d active prices (has_more=%t); provision exactly one before running the live test",
			lookupKey(planID), len(list.Data), list.HasMore)
	}
	price := list.Data[0]
	if price.ID == "" || !price.Active || price.Product == "" ||
		price.UnitAmount != plan.PriceCents() ||
		!strings.EqualFold(price.Currency, catalog.Currency) {
		return fmt.Errorf("lookup key %q does not match the active catalog price", lookupKey(planID))
	}
	p.prices.put(lookupKey(planID), price.ID)
	return nil
}

func cleanupStripeLiveTest(
	t *testing.T,
	p *Provider,
	accountID, email string,
	createdCustomers map[string]struct{},
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, endpoint := range liveTestCustomerDiscoveryEndpoints(accountID, email) {
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool `json:"has_more"`
		}
		if err := p.call(ctx, "GET", endpoint, nil, "", &list); err != nil {
			t.Errorf("Stripe live-test cleanup: discover customer: %v", err)
			continue
		}
		if list.HasMore {
			t.Errorf("Stripe live-test cleanup: customer discovery was unexpectedly paginated")
		}
		for _, customer := range list.Data {
			if customer.ID != "" {
				createdCustomers[customer.ID] = struct{}{}
			}
		}
	}

	for customerID := range createdCustomers {
		canDeleteCustomer := true
		sessionIDs, err := openCheckoutSessionIDs(ctx, p, customerID)
		if err != nil {
			t.Errorf("Stripe live-test cleanup: list open Checkout sessions for %s: %v", customerID, err)
			canDeleteCustomer = false
		} else {
			for _, sessionID := range sessionIDs {
				if err := p.call(ctx, "POST", "/v1/checkout/sessions/"+sessionID+"/expire", url.Values{}, "", nil); err != nil {
					t.Errorf("Stripe live-test cleanup: expire Checkout session %s: %v", sessionID, err)
					canDeleteCustomer = false
				}
			}
		}
		if !canDeleteCustomer {
			t.Errorf("Stripe live-test cleanup: preserving customer %s because its Checkout sessions were not proven expired", customerID)
			continue
		}
		if err := p.call(ctx, "DELETE", "/v1/customers/"+customerID, nil, "", nil); err != nil {
			t.Errorf("Stripe live-test cleanup: delete customer %s: %v", customerID, err)
		}
	}
}

func liveTestCustomerDiscoveryEndpoints(accountID, email string) []string {
	byEmail := url.Values{"email": {email}, "limit": {"100"}}
	byMetadata := url.Values{
		"query": {fmt.Sprintf("metadata['witself_account']:'%s'", accountID)},
		"limit": {"100"},
	}
	return []string{
		"/v1/customers?" + byEmail.Encode(),
		"/v1/customers/search?" + byMetadata.Encode(),
	}
}

func openCheckoutSessionIDs(ctx context.Context, p *Provider, customerID string) ([]string, error) {
	var ids []string
	startingAfter := ""
	for page := 0; page < 10; page++ {
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool `json:"has_more"`
		}
		q := url.Values{"customer": {customerID}, "status": {"open"}, "limit": {"100"}}
		if startingAfter != "" {
			q.Set("starting_after", startingAfter)
		}
		if err := p.call(ctx, "GET", "/v1/checkout/sessions?"+q.Encode(), nil, "", &list); err != nil {
			return nil, err
		}
		for _, session := range list.Data {
			if session.ID == "" {
				return nil, fmt.Errorf("open Checkout session has no id")
			}
			ids = append(ids, session.ID)
		}
		if !list.HasMore {
			return ids, nil
		}
		if len(list.Data) == 0 {
			return nil, fmt.Errorf("Checkout session page has_more without data")
		}
		startingAfter = list.Data[len(list.Data)-1].ID
	}
	return nil, fmt.Errorf("open Checkout sessions exceed cleanup page bound")
}
