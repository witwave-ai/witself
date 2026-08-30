// Package stripe implements billing.Provider against the Stripe API — the
// real partner behind the seam the fake has stood in for. It is a minimal
// hand-rolled REST client (form-encoded requests, stdlib only), the same
// discipline as internal/blob: the lean root module never gains a vendor SDK.
//
// Shape notes, mapped to the decided design (issue #31):
//   - Subscribe always returns needs_action(url): a Stripe Checkout session in
//     subscription mode. Checkout shows saved cards to returning customers, so
//     the flow is one-click for them; a headless charge-on-file path is a
//     future optimization the Action contract already permits.
//   - Prices resolve by lookup_key ("witself_" + plan id), never hardcoded
//     ids. EnsurePrices bootstraps missing products/prices from the catalog,
//     so a new plan in plans.json self-provisions — no dashboard clicks.
//   - Customers are created with an Idempotency-Key derived from the account
//     id, so Manager retries within Stripe's 24h idempotency window cannot
//     double-create. (The Search API was deliberately avoided: its indexing
//     lag would make EnsureCustomer racy.)
//   - Webhooks verify the Stripe-Signature header (HMAC-SHA256 over
//     "t.payload", constant-time compare, 5-minute tolerance) and collapse
//     Stripe's event zoo into the four normalized EventTypes. Unhandled event
//     types return an empty batch (ACK) — Stripe sends many types we do not
//     subscribe to, and re-delivering them helps nobody.
//   - ScheduleDowngrade supports the downgrade-to-free path via
//     cancel_at_period_end (the only downgrade today: Standard is the sole
//     purchasable paid plan). Paid-to-paid downgrades (Team -> Standard) need
//     subscription schedules and land with the Team tier.
//   - CancelPending disarms a scheduled downgrade (cancel_at_period_end =
//     false) AND expires any open subscription-mode Checkout sessions, so a
//     replaced upgrade cannot be paid later from a stale tab and mint a
//     duplicate subscription.
//   - Every request pins Stripe-Version: the shapes read here (item-level
//     current_period_end, /v1/invoices/create_preview) are Basil-era and must
//     not float with the account's default API version.
package stripe

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/plans"
)

const apiBase = "https://api.stripe.com"

// apiVersion pins every request. The response shapes read here are
// Basil-era: current_period_end lives on subscription items (removed from
// the subscription top level in 2025-03-31), and invoice previews come from
// POST /v1/invoices/create_preview (GET /v1/invoices/upcoming is gone).
// Floating with the account's default version would silently break both.
const apiVersion = "2025-03-31.basil"

// minimumDowngradeScheduleLead is longer than lifecycle's four-minute
// provider execution budget. A downgrade too close to renewal is refused
// before mutation so cancel_at_period_end cannot silently bind to the newly
// renewed period while Witself reports the old boundary.
const minimumDowngradeScheduleLead = 5 * time.Minute

// Config assembles the provider.
type Config struct {
	// SecretKey is the sk_test_/sk_live_ API key.
	SecretKey string
	// WebhookSecret is the whsec_ signing secret for HandleWebhook. Empty is
	// allowed only until webhooks are wired (stripe listen / CP deploy) —
	// HandleWebhook refuses everything without it.
	WebhookSecret string
	// Catalog maps plan ids to prices for bootstrap and lookup_key naming.
	Catalog *plans.Catalog
	// SuccessURL / CancelURL are where Checkout returns the payer. Defaults
	// point at the owned, value-free control-plane return pages.
	SuccessURL string
	CancelURL  string
	// PortalReturnURL is where the hosted portal's "back" goes.
	PortalReturnURL string
	// PortalConfigurationID pins a reviewed portal configuration whose
	// subscription update and cancellation features are disabled. An empty id
	// disables portal creation rather than falling back to Stripe's mutable
	// account-wide default configuration.
	PortalConfigurationID string
	// AutomaticTax turns on Stripe Tax for the purchase Checkout and for the
	// renewal preview, so the amount Witself quotes is the amount Stripe will
	// charge. Stripe needs a customer address to determine jurisdiction, so
	// enabling it also makes Checkout collect a billing address and save it
	// back to the existing customer. It is off by default: switching it on
	// requires an activated Stripe Tax registration set, and calculating tax
	// without one fails the purchase rather than silently undercharging.
	AutomaticTax bool
	// TestClockID attaches newly created customers to one Stripe test clock so
	// sandbox acceptance can advance renewal and period-end downgrades without
	// waiting a month. It is rejected with non-test keys and is never inferred.
	TestClockID string
	// HTTPClient defaults to a 30s-timeout client.
	HTTPClient *http.Client
	// Now injects a clock for signature-tolerance tests.
	Now func() time.Time
	// BaseURL overrides the API host (tests). Defaults to api.stripe.com.
	BaseURL string
}

// Provider implements billing.Provider on Stripe. Safe for concurrent use.
type Provider struct {
	cfg Config
	// prices caches lookup_key -> price id after EnsurePrices/first resolve.
	prices priceCache
}

var _ billing.Provider = (*Provider)(nil)
var _ billing.IdempotentSetupper = (*Provider)(nil)
var _ billing.IdempotentSubscriber = (*Provider)(nil)
var _ billing.IdempotentDowngrader = (*Provider)(nil)
var _ billing.ExactIdempotentDowngrader = (*Provider)(nil)
var _ billing.PreparedIdempotentDowngrader = (*Provider)(nil)
var _ billing.DowngradeTargetChecker = (*Provider)(nil)
var _ billing.IdempotentPendingCanceller = (*Provider)(nil)
var _ billing.ExactPendingCanceller = (*Provider)(nil)
var _ billing.EventResolver = (*Provider)(nil)

// New validates cfg and returns a Provider.
func New(cfg Config) (*Provider, error) {
	if cfg.SecretKey == "" {
		return nil, errors.New("stripe: SecretKey is required")
	}
	if cfg.Catalog == nil {
		return nil, errors.New("stripe: Catalog is required")
	}
	if cfg.TestClockID != "" {
		if !strings.HasPrefix(cfg.SecretKey, "sk_test_") {
			return nil, errors.New("stripe: TestClockID requires an sk_test_ key")
		}
		if err := validateStripeResourceID(cfg.TestClockID, "clock_"); err != nil {
			return nil, fmt.Errorf("stripe: invalid TestClockID: %w", err)
		}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = apiBase
	}
	if cfg.SuccessURL == "" {
		cfg.SuccessURL = "https://self.witwave.ai/billing/success"
	}
	if cfg.CancelURL == "" {
		cfg.CancelURL = "https://self.witwave.ai/billing/cancelled"
	}
	if cfg.PortalReturnURL == "" {
		cfg.PortalReturnURL = "https://self.witwave.ai/billing/portal-return"
	}
	return &Provider{cfg: cfg, prices: priceCache{ids: map[string]string{}}}, nil
}

// lookupKey names a plan's price in Stripe: "witself_standard".
func lookupKey(planID string) string { return "witself_" + planID }

// childIdempotencyKey derives one bounded Stripe key for a mutation inside a
// larger durable operation. The provider object id is hashed rather than
// copied into the header, keeping the key bounded and avoiding raw identifiers
// in request metadata. Legacy, non-strong calls pass no operation id and
// intentionally receive no key.
func childIdempotencyKey(operationID, action, objectID string) string {
	if operationID == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(action + "\x00" + objectID))
	return "witself-" + action + "-" + operationID + "-" +
		hex.EncodeToString(digest[:])
}

func validateStripeResourceID(objectID, prefix string) error {
	if err := billing.ValidateProviderObjectID(objectID); err != nil {
		return err
	}
	if !strings.HasPrefix(objectID, prefix) || len(objectID) == len(prefix) {
		return fmt.Errorf("expected Stripe %s resource id", prefix)
	}
	return nil
}

func validateStripeCheckoutAction(
	objectID, rawURL string,
	expiresAt time.Time,
	now time.Time,
) error {
	if err := validateStripeResourceID(objectID, "cs_"); err != nil {
		return err
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" ||
		!strings.EqualFold(parsed.Hostname(), "checkout.stripe.com") ||
		parsed.User != nil || strings.TrimSpace(rawURL) != rawURL {
		return errors.New("invalid Stripe Checkout URL")
	}
	if !expiresAt.After(now.UTC()) {
		return errors.New("stripe Checkout action is already expired")
	}
	return nil
}

// EnsurePrices bootstraps the Stripe side of the catalog: for every
// purchasable plan, resolve its price by lookup_key, creating the product and
// price when missing. Idempotent; the control plane calls it at startup so a
// catalog change self-provisions.
func (p *Provider) EnsurePrices(ctx context.Context) error {
	for _, plan := range p.cfg.Catalog.Plans {
		if !plan.Purchasable() {
			continue
		}
		if _, err := p.priceID(ctx, plan.ID); err != nil {
			return err
		}
	}
	return nil
}

// priceID resolves (and caches) the Stripe price for a plan, creating the
// product+price on first miss. A resolved price whose amount no longer
// matches the catalog gets a REPLACEMENT price (lookup_key transferred), so
// a plans.json price change actually propagates to new checkouts — existing
// subscriptions stay grandfathered on the price they signed up at.
func (p *Provider) priceID(ctx context.Context, planID string) (string, error) {
	key := lookupKey(planID)
	if id, ok := p.prices.get(key); ok {
		return id, nil
	}
	plan, ok := p.cfg.Catalog.Get(planID)
	if !ok || !plan.Purchasable() {
		return "", fmt.Errorf("stripe: plan %q is not purchasable", planID)
	}
	cents := plan.PriceCents()
	// Resolve by lookup key.
	var list struct {
		Data []struct {
			ID         string `json:"id"`
			UnitAmount int64  `json:"unit_amount"`
			Currency   string `json:"currency"`
			Product    string `json:"product"`
		} `json:"data"`
	}
	q := url.Values{}
	q.Add("lookup_keys[]", key)
	if err := p.call(ctx, "GET", "/v1/prices?"+q.Encode(), nil, "", &list); err != nil {
		return "", err
	}
	if len(list.Data) > 0 {
		existing := list.Data[0]
		if err := p.reconcileProduct(ctx, existing.Product, plan); err != nil {
			return "", err
		}
		if existing.UnitAmount == cents && existing.Currency == "usd" {
			p.prices.put(key, existing.ID)
			return existing.ID, nil
		}
		id, err := p.createPrice(ctx, existing.Product, planID, cents, key)
		if err != nil {
			return "", err
		}
		p.prices.put(key, id)
		return id, nil
	}
	// First sighting: create product + price from the catalog.
	var product struct {
		ID string `json:"id"`
	}
	if err := p.call(ctx, "POST", "/v1/products", url.Values{
		"name":                   {"Witself " + plan.Name},
		"metadata[witself_plan]": {plan.ID},
	}, "witself-product-"+plan.ID, &product); err != nil {
		return "", err
	}
	id, err := p.createPrice(ctx, product.ID, planID, cents, key)
	if err != nil {
		return "", err
	}
	p.prices.put(key, id)
	return id, nil
}

func (p *Provider) reconcileProduct(
	ctx context.Context,
	productID string,
	plan plans.Plan,
) error {
	if err := validateStripeResourceID(productID, "prod_"); err != nil {
		return fmt.Errorf("stripe: invalid product id for plan %s: %w", plan.ID, err)
	}
	var product struct {
		ID       string            `json:"id"`
		Name     string            `json:"name"`
		Metadata map[string]string `json:"metadata"`
	}
	path := "/v1/products/" + url.PathEscape(productID)
	if err := p.call(ctx, "GET", path, nil, "", &product); err != nil {
		return err
	}
	wantName := "Witself " + plan.Name
	if product.ID != productID {
		return errors.New("stripe: product lookup returned a different object")
	}
	if product.Name == wantName && product.Metadata["witself_plan"] == plan.ID {
		return nil
	}
	digest := sha256.Sum256([]byte(wantName + "\x00" + plan.ID))
	var updated struct {
		ID       string            `json:"id"`
		Name     string            `json:"name"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := p.call(ctx, "POST", path, url.Values{
		"name":                   {wantName},
		"metadata[witself_plan]": {plan.ID},
	}, fmt.Sprintf("witself-product-sync-%s-%x", plan.ID, digest[:8]), &updated); err != nil {
		return err
	}
	if updated.ID != productID || updated.Name != wantName ||
		updated.Metadata["witself_plan"] != plan.ID {
		return errors.New("stripe: product update did not confirm catalog identity")
	}
	return nil
}

// createPrice mints a monthly recurring price carrying the plan's
// lookup_key, transferring it from any prior price. The Idempotency-Key
// includes the amount, so a retried create replays but a changed catalog
// price creates fresh.
func (p *Provider) createPrice(ctx context.Context, productID, planID string, cents int64, key string) (string, error) {
	var price struct {
		ID string `json:"id"`
	}
	if err := p.call(ctx, "POST", "/v1/prices", url.Values{
		"product":                {productID},
		"unit_amount":            {strconv.FormatInt(cents, 10)},
		"currency":               {"usd"},
		"recurring[interval]":    {"month"},
		"lookup_key":             {key},
		"transfer_lookup_key":    {"true"},
		"metadata[witself_plan]": {planID},
	}, fmt.Sprintf("witself-price-%s-%d", planID, cents), &price); err != nil {
		return "", err
	}
	return price.ID, nil
}

// EnsureCustomer implements billing.Provider. Customer creation is bound only
// to stable account metadata; mutable email is applied afterward with its own
// value-derived idempotency key. That separation matters when an exact billing
// retry arrives after the account email changed: Stripe must replay the same
// customer create instead of rejecting changed create parameters and advancing
// to a second customer generation.
//
// Stripe replays the original create response even after the resource was
// deleted (live-test-discovered), so the replayed customer is verified and the
// create retried with a fresh key when it no longer exists.
func (p *Provider) EnsureCustomer(ctx context.Context, accountID, email string) (string, error) {
	params := url.Values{"metadata[witself_account]": {accountID}}
	keyBase := "witself-ensure-" + accountID
	if p.cfg.TestClockID != "" {
		params.Set("test_clock", p.cfg.TestClockID)
		digest := sha256.Sum256([]byte(p.cfg.TestClockID))
		keyBase += "-clock-" + hex.EncodeToString(digest[:8])
	}
	// Walk deterministic key generations: the stable key first, then -g1,
	// -g2, … A generation goes stale when its 24h replay window returns a
	// customer that was since deleted. A 400 idempotency_error can also remain
	// during rollout from an older create shape. Deterministic generations (rather
	// than a time salt) keep EnsureCustomer stable across near-term retries even
	// when earlier generations are poisoned — retries walk the same chain to the
	// same live customer.
	for gen := 0; gen < 16; gen++ {
		key := keyBase
		if gen > 0 {
			key = fmt.Sprintf("%s-g%d", key, gen)
		}
		var cust struct {
			ID string `json:"id"`
		}
		if err := p.call(ctx, "POST", "/v1/customers", params, key, &cust); err != nil {
			var se *apiError
			if errors.As(err, &se) && se.code == "idempotency_error" {
				continue
			}
			return "", err
		}
		alive, err := p.customerAlive(ctx, cust.ID)
		if err != nil {
			return "", err
		}
		if alive {
			if err := p.updateCustomerEmail(ctx, cust.ID, email); err != nil {
				return "", err
			}
			return cust.ID, nil
		}
	}
	// Pathological: 16 poisoned generations inside one 24h window. A
	// time-salted key is the last resort — unique, but not retry-stable.
	var cust struct {
		ID string `json:"id"`
	}
	salted := fmt.Sprintf("%s-%d", keyBase, p.cfg.Now().Unix())
	if err := p.call(ctx, "POST", "/v1/customers", params, salted, &cust); err != nil {
		return "", err
	}
	if err := p.updateCustomerEmail(ctx, cust.ID, email); err != nil {
		return "", err
	}
	return cust.ID, nil
}

// updateCustomerEmail applies mutable customer contact state separately from
// the stable create. The raw email never appears in the idempotency key: a hash
// of the customer/value pair gives exact retries one key while allowing a later
// rotation to perform a distinct update.
func (p *Provider) updateCustomerEmail(
	ctx context.Context,
	customerID, email string,
) error {
	if email == "" {
		return nil
	}
	digest := sha256.Sum256([]byte(customerID + "\x00" + email))
	idempotencyKey := "witself-customer-email-" + hex.EncodeToString(digest[:])
	return p.call(ctx, "POST", "/v1/customers/"+customerID, url.Values{
		"email": {email},
	}, idempotencyKey, nil)
}

// customerAlive reports whether the customer exists and is not deleted.
func (p *Provider) customerAlive(ctx context.Context, customerID string) (bool, error) {
	var cust struct {
		Deleted bool `json:"deleted"`
	}
	err := p.call(ctx, "GET", "/v1/customers/"+customerID, nil, "", &cust)
	if err != nil {
		var se *apiError
		if errors.As(err, &se) && se.status == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	return !cust.Deleted, nil
}

// Subscribe implements billing.Provider: a Checkout session in subscription
// mode. Always needs_action(url) — see the package comment.
func (p *Provider) Subscribe(ctx context.Context, customerID, plan string) (billing.Action, error) {
	return p.subscribe(ctx, customerID, plan, "")
}

// SubscribeIdempotent creates or replays the Checkout Session for one durable
// Witself upgrade operation. The operation id is also copied into provider
// metadata so its later callback can be correlated with the originating claim.
func (p *Provider) SubscribeIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.Action, error) {
	if err := billing.ValidateOperationID(operationID); err != nil {
		return billing.Action{}, fmt.Errorf("stripe: subscription: %w", err)
	}
	return p.subscribe(ctx, customerID, plan, operationID)
}

func (p *Provider) subscribe(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.Action, error) {
	priceID, err := p.priceID(ctx, plan)
	if err != nil {
		return billing.Action{}, err
	}
	var session struct {
		ID        string `json:"id"`
		URL       string `json:"url"`
		ExpiresAt int64  `json:"expires_at"`
	}
	params := url.Values{
		"mode": {"subscription"},
		// Pin the purchase flow to synchronous cards. Leaving this absent lets
		// mutable Dashboard settings add delayed-notification methods; a
		// completed-but-unpaid Checkout cannot be safely reported cancelled.
		"payment_method_types[]":  {"card"},
		"customer":                {customerID},
		"line_items[0][price]":    {priceID},
		"line_items[0][quantity]": {"1"},
		// Let Checkout accept operator-minted promotion codes (launch
		// proofs, future discounts). Entering no code keeps full price;
		// code redemption limits and expiry stay dashboard-governed.
		"allow_promotion_codes":  {"true"},
		"success_url":            {p.cfg.SuccessURL},
		"cancel_url":             {p.cfg.CancelURL},
		"metadata[witself_plan]": {plan},
		"subscription_data[metadata][witself_plan]": {plan},
	}
	if p.cfg.AutomaticTax {
		params.Set("automatic_tax[enabled]", "true")
		// Stripe cannot calculate tax for an existing customer without a
		// usable address, and it will not persist the one Checkout collects
		// unless the session is explicitly allowed to update the customer.
		params.Set("billing_address_collection", "required")
		params.Set("customer_update[address]", "auto")
		params.Set("customer_update[name]", "auto")
	}
	idempotencyKey := ""
	if operationID != "" {
		params.Set("metadata[witself_operation_id]", operationID)
		params.Set("subscription_data[metadata][witself_operation_id]", operationID)
		idempotencyKey = "witself-subscribe-" + operationID
	}
	err = p.call(ctx, "POST", "/v1/checkout/sessions", params, idempotencyKey, &session)
	if err != nil {
		return billing.Action{}, err
	}
	expiresAt := time.Unix(session.ExpiresAt, 0).UTC()
	if err := validateStripeCheckoutAction(
		session.ID, session.URL, expiresAt, p.cfg.Now(),
	); err != nil {
		return billing.Action{}, fmt.Errorf(
			"stripe: invalid subscription checkout response: %w", err)
	}
	return billing.Action{
		URL: session.URL, ProviderObjectID: session.ID,
		ExpiresAt: expiresAt,
	}, nil
}

// SetupLink implements billing.Provider: a Checkout session in setup mode
// (card capture without charging).
func (p *Provider) SetupLink(ctx context.Context, customerID string) (billing.Action, error) {
	return p.setupLink(ctx, customerID, "")
}

// SetupLinkIdempotent creates or replays the Checkout Session for one durable
// setup operation. The operation identity is copied into session metadata so
// Stripe-side diagnostics can correlate the resulting object without copying
// customer content into the idempotency key.
func (p *Provider) SetupLinkIdempotent(
	ctx context.Context,
	customerID, operationID string,
) (billing.Action, error) {
	if err := billing.ValidateOperationID(operationID); err != nil {
		return billing.Action{}, fmt.Errorf("stripe: setup: %w", err)
	}
	return p.setupLink(ctx, customerID, operationID)
}

func (p *Provider) setupLink(
	ctx context.Context,
	customerID, operationID string,
) (billing.Action, error) {
	var session struct {
		ID        string `json:"id"`
		URL       string `json:"url"`
		ExpiresAt int64  `json:"expires_at"`
	}
	params := url.Values{
		"mode":                   {"setup"},
		"payment_method_types[]": {"card"},
		"customer":               {customerID},
		"currency":               {"usd"}, // required in setup mode (no line items to infer it from)
		"success_url":            {p.cfg.SuccessURL},
		"cancel_url":             {p.cfg.CancelURL},
	}
	idempotencyKey := ""
	if operationID != "" {
		params.Set("metadata[witself_operation_id]", operationID)
		idempotencyKey = "witself-setup-" + operationID
	}
	err := p.call(ctx, "POST", "/v1/checkout/sessions", params, idempotencyKey, &session)
	if err != nil {
		return billing.Action{}, err
	}
	expiresAt := time.Unix(session.ExpiresAt, 0).UTC()
	if err := validateStripeCheckoutAction(
		session.ID, session.URL, expiresAt, p.cfg.Now(),
	); err != nil {
		return billing.Action{}, fmt.Errorf(
			"stripe: invalid setup checkout response: %w", err)
	}
	return billing.Action{
		URL: session.URL, ProviderObjectID: session.ID,
		ExpiresAt: expiresAt,
	}, nil
}

// PortalLink implements billing.Provider using one explicitly reviewed portal
// configuration. The mutable account-wide default is never used.
func (p *Provider) PortalLink(ctx context.Context, customerID string) (string, error) {
	if err := validateStripeResourceID(p.cfg.PortalConfigurationID, "bpc_"); err != nil {
		return "", fmt.Errorf(
			"stripe: safe billing portal configuration is not configured: %w", err)
	}
	var session struct {
		URL string `json:"url"`
	}
	err := p.call(ctx, "POST", "/v1/billing_portal/sessions", url.Values{
		"customer":      {customerID},
		"return_url":    {p.cfg.PortalReturnURL},
		"configuration": {p.cfg.PortalConfigurationID},
	}, "", &session)
	if err != nil {
		return "", err
	}
	return session.URL, nil
}

// stripeProductRef is a price's product reference. Stripe serializes it as a
// bare "prod_..." ID string unless the read expanded the product; only an
// expanded read carries metadata. Both shapes must decode: expansion depth is
// capped at 4 levels, so list reads (whose paths start at "data.") can never
// reach the product object and always see the string form.
type stripeProductRef struct {
	ID       string            `json:"id"`
	Metadata map[string]string `json:"metadata"`
}

func (p *stripeProductRef) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		return json.Unmarshal(trimmed, &p.ID)
	}
	type bare stripeProductRef
	return json.Unmarshal(trimmed, (*bare)(p))
}

// stripeSubscription is the slice of the subscription object read here. As
// of API 2025-03-31.basil, current_period_end lives on subscription ITEMS —
// it was removed from the subscription top level.
type stripeSubscription struct {
	ID                string            `json:"id"`
	Customer          string            `json:"customer"`
	Status            string            `json:"status"`
	CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
	Metadata          map[string]string `json:"metadata"`
	Items             struct {
		Data []struct {
			CurrentPeriodEnd int64 `json:"current_period_end"`
			Price            struct {
				ID        string           `json:"id"`
				LookupKey string           `json:"lookup_key"`
				Product   stripeProductRef `json:"product"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

func liveStripeSubscriptionStatus(status string) bool {
	switch status {
	case "active", "trialing", "past_due", "unpaid":
		return true
	default:
		return false
	}
}

// periodEnd is the subscription's latest item period end.
func (s stripeSubscription) periodEnd() time.Time {
	var latest int64
	for _, it := range s.Items.Data {
		if it.CurrentPeriodEnd > latest {
			latest = it.CurrentPeriodEnd
		}
	}
	return time.Unix(latest, 0).UTC()
}

func (p *Provider) subscriptionPlan(s stripeSubscription) (string, error) {
	if err := validateStripeResourceID(s.ID, "sub_"); err != nil {
		return "", fmt.Errorf("stripe: invalid managed subscription id: %w", err)
	}
	planID := strings.TrimSpace(s.Metadata["witself_plan"])
	plan, ok := p.cfg.Catalog.Get(planID)
	if !ok || !plan.Paid() {
		return "", fmt.Errorf(
			"stripe: subscription %s has unknown or non-paid witself_plan %q",
			s.ID, planID)
	}
	if len(s.Items.Data) != 1 {
		return "", fmt.Errorf(
			"stripe: subscription %s has %d items; want exactly one",
			s.ID, len(s.Items.Data))
	}
	price := s.Items.Data[0].Price
	productPlan := strings.TrimSpace(price.Product.Metadata["witself_plan"])
	if err := validateStripeResourceID(price.ID, "price_"); err != nil {
		return "", fmt.Errorf(
			"stripe: subscription %s has invalid price id: %w", s.ID, err)
	}
	lookupMatches := price.LookupKey == lookupKey(planID)
	productMatches := productPlan == planID
	if (price.LookupKey != "" && !lookupMatches) ||
		(productPlan != "" && !productMatches) ||
		(!lookupMatches && !productMatches) {
		return "", fmt.Errorf(
			"stripe: subscription %s price does not match plan %q",
			s.ID, planID)
	}
	if productMatches {
		if err := validateStripeResourceID(price.Product.ID, "prod_"); err != nil {
			return "", fmt.Errorf(
				"stripe: subscription %s has invalid product id: %w", s.ID, err)
		}
	}
	return planID, nil
}

// liveSubscriptions lists the customer's subscriptions that still back an
// entitlement: active, trialing, past_due, or unpaid. past_due matters — a
// dunned customer must still be able to downgrade to free, and a
// status=active filter made them invisible. incomplete (a checkout
// mid-flight) and canceled do not count.
func (p *Provider) liveSubscriptions(ctx context.Context, customerID string) ([]stripeSubscription, error) {
	var list struct {
		Data    []stripeSubscription `json:"data"`
		HasMore bool                 `json:"has_more"`
	}
	// No product expansion here: list paths start at "data.", so reaching
	// the product object would need 5 expansion levels and Stripe caps
	// expansion at 4 (property_expansion_max_depth). The one chosen
	// subscription is re-read with items.data.price.product expanded in
	// managedLiveSubscription before any plan validation.
	q := url.Values{"customer": {customerID}, "limit": {"100"}}
	if err := p.call(ctx, "GET", "/v1/subscriptions?"+q.Encode(), nil, "", &list); err != nil {
		return nil, err
	}
	if list.HasMore {
		return nil, fmt.Errorf(
			"stripe: customer %s has more than 100 subscriptions; refusing a partial projection",
			customerID)
	}
	live := list.Data[:0]
	for _, s := range list.Data {
		if s.Customer != customerID {
			return nil, fmt.Errorf(
				"stripe: subscription %s belongs to unexpected customer %q",
				s.ID, s.Customer)
		}
		if liveStripeSubscriptionStatus(s.Status) {
			live = append(live, s)
		}
	}
	return live, nil
}

// managedLiveSubscription returns the one exact Witself subscription backing
// the account. A dedicated Stripe customer must never have multiple or
// unrelated live subscriptions: choosing one would make entitlement,
// dunning, and cancellation arrival-order dependent. Ambiguity remains
// pending for operator reconciliation instead of being guessed.
func (p *Provider) managedLiveSubscription(
	ctx context.Context,
	customerID string,
) (*stripeSubscription, string, error) {
	subs, err := p.liveSubscriptions(ctx, customerID)
	if err != nil {
		return nil, "", err
	}
	if len(subs) == 0 {
		return nil, "", nil
	}
	if len(subs) != 1 {
		return nil, "", fmt.Errorf(
			"stripe: customer %s has %d live subscriptions; refusing ambiguous billing state",
			customerID, len(subs))
	}
	// Re-read the one live subscription with its product expanded (a legal
	// 4-level path on a single retrieve) so subscriptionPlan can check
	// price.product metadata as well as the lookup key; the list response
	// only carries the product id.
	if err := validateStripeResourceID(subs[0].ID, "sub_"); err != nil {
		return nil, "", fmt.Errorf("stripe: invalid live subscription id: %w", err)
	}
	var sub stripeSubscription
	path := "/v1/subscriptions/" + url.PathEscape(subs[0].ID)
	if err := p.call(
		ctx, "GET", path+"?expand[]=items.data.price.product", nil, "", &sub,
	); err != nil {
		return nil, "", err
	}
	if sub.ID != subs[0].ID || sub.Customer != customerID ||
		!liveStripeSubscriptionStatus(sub.Status) {
		return nil, "", fmt.Errorf(
			"stripe: subscription %s changed between list and read; refusing stale billing state",
			subs[0].ID)
	}
	planID, err := p.subscriptionPlan(sub)
	if err != nil {
		return nil, "", err
	}
	return &sub, planID, nil
}

// ScheduleDowngrade implements billing.Provider. Today's only downgrade
// target is free (Standard is the sole purchasable paid plan):
// cancel_at_period_end ends the subscription at the period boundary and
// Stripe announces it with customer.subscription.deleted — the canceled
// event the Manager already folds. More than one live subscription is an
// ambiguous provider state and fails closed for operator reconciliation;
// paid-to-paid needs subscription schedules and lands with the Team tier.
func (p *Provider) ScheduleDowngrade(ctx context.Context, customerID, plan string) (time.Time, error) {
	prepared, err := p.prepareDowngrade(ctx, customerID, plan, "")
	if err != nil {
		return time.Time{}, err
	}
	scheduled, err := p.SchedulePreparedDowngradeIdempotent(
		ctx, customerID, plan, "", prepared)
	return scheduled.Effective, err
}

// ScheduleDowngradeIdempotent applies one durable downgrade operation. The
// exact managed subscription mutation receives a deterministic child key, so
// an ambiguous retry replays that effect instead of discovering a new target.
func (p *Provider) ScheduleDowngradeIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (time.Time, error) {
	if err := billing.ValidateOperationID(operationID); err != nil {
		return time.Time{}, fmt.Errorf("stripe: downgrade: %w", err)
	}
	prepared, err := p.prepareDowngrade(ctx, customerID, plan, operationID)
	if err != nil {
		return time.Time{}, err
	}
	scheduled, err := p.SchedulePreparedDowngradeIdempotent(
		ctx, customerID, plan, operationID, prepared)
	return scheduled.Effective, err
}

// ScheduleDowngradeExactIdempotent returns the exact subscription armed for
// period-end cancellation in addition to the effective boundary.
func (p *Provider) ScheduleDowngradeExactIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.ScheduledDowngrade, error) {
	if err := billing.ValidateOperationID(operationID); err != nil {
		return billing.ScheduledDowngrade{}, fmt.Errorf("stripe: downgrade: %w", err)
	}
	prepared, err := p.prepareDowngrade(ctx, customerID, plan, operationID)
	if err != nil {
		return billing.ScheduledDowngrade{}, err
	}
	return p.SchedulePreparedDowngradeIdempotent(
		ctx, customerID, plan, operationID, prepared)
}

// SupportsDowngradeTarget reports the exact target set this adapter can
// schedule today. Paid-to-paid transitions require subscription schedules and
// remain unavailable until the Team billing slice lands.
func (*Provider) SupportsDowngradeTarget(plan string) bool {
	return plan == plans.Free
}

var _ billing.UpgradeTransitionChecker = (*Provider)(nil)

// SupportsUpgradeTransition reports the exact self-serve upgrades this adapter
// can execute today. Subscribe starts a NEW subscription through a hosted
// Checkout Session, so an account that already pays for a plan would end up
// with two live subscriptions and two invoices. Paid-to-paid therefore stays
// unavailable — the same subscription-schedule work that gates paid-to-paid
// downgrades — and the lifecycle routes it to a contact request instead.
func (*Provider) SupportsUpgradeTransition(current, _ string) bool {
	return current == "" || current == plans.Free
}

// PrepareDowngrade selects the one exact live subscription and its current
// period boundary without mutating Stripe.
func (p *Provider) PrepareDowngrade(
	ctx context.Context,
	customerID, plan string,
) (billing.ScheduledDowngrade, error) {
	return p.prepareDowngrade(ctx, customerID, plan, "")
}

// prepareDowngrade permits an already armed subscription only when the
// caller supplies the exact Witself operation recorded on that subscription.
// The public read-only preparation API has no operation identity, so it must
// refuse every pre-armed target rather than silently adopt an external
// period-end cancellation. Strong wrappers pass their validated operation id
// so an exact retry can recover without another provider mutation.
func (p *Provider) prepareDowngrade(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.ScheduledDowngrade, error) {
	if plan != plans.Free {
		return billing.ScheduledDowngrade{}, fmt.Errorf("stripe: downgrade to %q not supported yet (only free; paid-to-paid lands with the Team tier)", plan)
	}
	sub, _, err := p.managedLiveSubscription(ctx, customerID)
	if err != nil {
		return billing.ScheduledDowngrade{}, err
	}
	if sub == nil {
		return billing.ScheduledDowngrade{}, fmt.Errorf("stripe: customer %s has no live subscription", customerID)
	}
	if sub.CancelAtPeriodEnd &&
		(operationID == "" ||
			sub.Metadata["witself_pending_downgrade_operation_id"] != operationID) {
		return billing.ScheduledDowngrade{}, errors.New(
			"stripe: subscription is already scheduled for period-end cancellation outside this exact Witself downgrade operation")
	}
	if err := validateStripeResourceID(sub.ID, "sub_"); err != nil {
		return billing.ScheduledDowngrade{}, fmt.Errorf(
			"stripe: invalid managed subscription id: %w", err)
	}
	effective := sub.periodEnd()
	if effective.IsZero() || !effective.After(p.cfg.Now().UTC()) {
		return billing.ScheduledDowngrade{}, fmt.Errorf(
			"stripe: subscription %s has no future current period end", sub.ID)
	}
	if !effective.After(p.cfg.Now().UTC().Add(minimumDowngradeScheduleLead)) {
		return billing.ScheduledDowngrade{}, errors.New(
			"stripe: subscription is too close to renewal to schedule a bounded downgrade safely; retry after provider reconciliation")
	}
	return billing.ScheduledDowngrade{
		Effective: effective, ProviderObjectID: sub.ID,
	}, nil
}

// SchedulePreparedDowngradeIdempotent arms only the target durably prepared
// by lifecycle state. Retry uses an operation-only Stripe idempotency key, so
// even an accidental target drift cannot create a second mutation.
func (p *Provider) SchedulePreparedDowngradeIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
	prepared billing.ScheduledDowngrade,
) (billing.ScheduledDowngrade, error) {
	if plan != plans.Free {
		return billing.ScheduledDowngrade{}, fmt.Errorf(
			"stripe: downgrade to %q not supported yet (only free; paid-to-paid lands with the Team tier)", plan)
	}
	if operationID != "" {
		if err := billing.ValidateOperationID(operationID); err != nil {
			return billing.ScheduledDowngrade{}, fmt.Errorf(
				"stripe: downgrade: %w", err)
		}
	}
	if err := validateStripeResourceID(
		prepared.ProviderObjectID, "sub_"); err != nil {
		return billing.ScheduledDowngrade{}, fmt.Errorf(
			"stripe: invalid prepared subscription id: %w", err)
	}
	if prepared.Effective.IsZero() {
		return billing.ScheduledDowngrade{}, errors.New(
			"stripe: prepared downgrade has no period boundary")
	}
	if !prepared.Effective.After(
		p.cfg.Now().UTC().Add(minimumDowngradeScheduleLead)) {
		return billing.ScheduledDowngrade{}, errors.New(
			"stripe: prepared downgrade is too close to renewal; reconcile before retrying")
	}
	path := "/v1/subscriptions/" + url.PathEscape(prepared.ProviderObjectID)
	var sub stripeSubscription
	if err := p.call(ctx, "GET", path+"?expand[]=items.data.price.product", nil, "", &sub); err != nil {
		return billing.ScheduledDowngrade{}, err
	}
	if sub.ID != prepared.ProviderObjectID || sub.Customer != customerID ||
		!liveStripeSubscriptionStatus(sub.Status) {
		return billing.ScheduledDowngrade{}, errors.New(
			"stripe: prepared downgrade target is no longer the exact live subscription")
	}
	currentPlan, err := p.subscriptionPlan(sub)
	if err != nil {
		return billing.ScheduledDowngrade{}, err
	}
	if currentPlan == plan || !sub.periodEnd().Equal(prepared.Effective) {
		return billing.ScheduledDowngrade{}, errors.New(
			"stripe: prepared downgrade target changed before mutation")
	}
	if sub.CancelAtPeriodEnd {
		if operationID == "" ||
			sub.Metadata["witself_pending_downgrade_operation_id"] != operationID {
			return billing.ScheduledDowngrade{}, errors.New(
				"stripe: prepared subscription is already scheduled for period-end cancellation outside this exact Witself downgrade operation")
		}
		return prepared, nil
	}
	if !prepared.Effective.After(
		p.cfg.Now().UTC().Add(minimumDowngradeScheduleLead)) {
		return billing.ScheduledDowngrade{}, errors.New(
			"stripe: prepared downgrade crossed the safe renewal margin")
	}
	params := url.Values{"cancel_at_period_end": {"true"}}
	if operationID != "" {
		params.Set("metadata[witself_pending_downgrade_operation_id]", operationID)
	}
	var armed stripeSubscription
	if err := p.call(ctx, "POST", path, params,
		childIdempotencyKey(operationID, "downgrade", "prepared"), &armed); err != nil {
		return billing.ScheduledDowngrade{}, err
	}
	if armed.ID != prepared.ProviderObjectID || armed.Customer != customerID ||
		!liveStripeSubscriptionStatus(armed.Status) ||
		!armed.CancelAtPeriodEnd ||
		!armed.periodEnd().Equal(prepared.Effective) ||
		(operationID != "" &&
			armed.Metadata["witself_pending_downgrade_operation_id"] != operationID) {
		return billing.ScheduledDowngrade{}, errors.New(
			"stripe: subscription response did not confirm exact period-end schedule")
	}
	return billing.ScheduledDowngrade{
		Effective:        prepared.Effective,
		ProviderObjectID: prepared.ProviderObjectID,
	}, nil
}

// CancelPending implements billing.Provider: undo whatever the pending
// change armed at Stripe. Open subscription-mode Checkout sessions are
// expired (a replaced upgrade must not be payable later from a stale tab —
// that minted a duplicate subscription), and scheduled downgrades are
// disarmed (cancel_at_period_end = false). Errors PROPAGATE: the Manager
// clears its local pending only after this succeeds (disarm-first
// invariant); swallowing an API failure here left downgrades armed at Stripe
// after the user was told the cancel took.
func (p *Provider) CancelPending(ctx context.Context, customerID string) error {
	return p.cancelPending(ctx, customerID, "")
}

// CancelPendingIdempotent applies one durable cancellation operation. Each
// expired Checkout Session and disarmed subscription receives its own stable
// child key; the discovery reads deliberately carry no idempotency header.
func (p *Provider) CancelPendingIdempotent(
	ctx context.Context,
	customerID, operationID string,
) error {
	if err := billing.ValidateOperationID(operationID); err != nil {
		return fmt.Errorf("stripe: cancel pending: %w", err)
	}
	return p.cancelPending(ctx, customerID, operationID)
}

// CancelPendingObjectIdempotent inspects and disarms one exact object. A
// completed Checkout is deliberately an error: reporting it as cancelled
// would let the lifecycle clear local pending state before its activation
// webhook arrives.
func (p *Provider) CancelPendingObjectIdempotent(
	ctx context.Context,
	customerID string,
	target billing.PendingCancellation,
	operationID string,
) error {
	if err := billing.ValidateOperationID(operationID); err != nil {
		return fmt.Errorf("stripe: exact cancel: %w", err)
	}
	if err := billing.ValidateOperationID(target.OriginalOperationID); err != nil {
		return fmt.Errorf("stripe: exact cancel original operation: %w", err)
	}
	switch target.Kind {
	case billing.PendingCancellationHostedAction:
		if err := validateStripeResourceID(target.ProviderObjectID, "cs_"); err != nil {
			return fmt.Errorf("stripe: exact checkout cancel: %w", err)
		}
		var session struct {
			ID       string            `json:"id"`
			Customer string            `json:"customer"`
			Mode     string            `json:"mode"`
			Status   string            `json:"status"`
			Metadata map[string]string `json:"metadata"`
		}
		path := "/v1/checkout/sessions/" + url.PathEscape(target.ProviderObjectID)
		if err := p.call(ctx, "GET", path, nil, "", &session); err != nil {
			return err
		}
		if session.ID != target.ProviderObjectID || session.Customer != customerID ||
			session.Mode != "subscription" ||
			session.Metadata["witself_operation_id"] != target.OriginalOperationID {
			return errors.New("stripe: exact checkout cancellation target mismatch")
		}
		switch session.Status {
		case "expired":
			return nil
		case "open":
			var expired struct {
				ID       string `json:"id"`
				Customer string `json:"customer"`
				Status   string `json:"status"`
			}
			if err := p.call(
				ctx, "POST", path+"/expire", url.Values{},
				childIdempotencyKey(
					operationID, "cancel-checkout", target.ProviderObjectID),
				&expired); err != nil {
				return err
			}
			if expired.ID != target.ProviderObjectID ||
				expired.Customer != customerID || expired.Status != "expired" {
				return errors.New(
					"stripe: checkout expiry response did not confirm exact cancellation")
			}
			return nil
		case "complete":
			return fmt.Errorf(
				"stripe: checkout already completed; awaiting provider reconciliation: %w",
				billing.ErrPendingAlreadyResolved)
		default:
			return fmt.Errorf(
				"stripe: unsupported checkout status %q during cancellation",
				session.Status)
		}
	case billing.PendingCancellationPeriodEnd:
		if err := validateStripeResourceID(target.ProviderObjectID, "sub_"); err != nil {
			return fmt.Errorf("stripe: exact period-end cancel: %w", err)
		}
		var sub struct {
			ID                string            `json:"id"`
			Customer          string            `json:"customer"`
			Status            string            `json:"status"`
			CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
			Metadata          map[string]string `json:"metadata"`
		}
		path := "/v1/subscriptions/" + url.PathEscape(target.ProviderObjectID)
		if err := p.call(ctx, "GET", path, nil, "", &sub); err != nil {
			return err
		}
		if sub.ID != target.ProviderObjectID || sub.Customer != customerID {
			return errors.New("stripe: exact period-end cancellation target mismatch")
		}
		switch sub.Status {
		case "canceled", "incomplete_expired":
			return fmt.Errorf(
				"stripe: subscription already reached a terminal state; awaiting provider reconciliation: %w",
				billing.ErrPendingAlreadyResolved)
		default:
			if !liveStripeSubscriptionStatus(sub.Status) {
				return fmt.Errorf(
					"stripe: unsupported subscription status %q during cancellation",
					sub.Status)
			}
		}
		owner, hasOwner := sub.Metadata["witself_pending_downgrade_operation_id"]
		if !sub.CancelAtPeriodEnd {
			if !hasOwner {
				return nil
			}
			if owner != target.OriginalOperationID {
				return errors.New("stripe: exact period-end cancellation target mismatch")
			}
		} else if !hasOwner || owner != target.OriginalOperationID {
			return errors.New("stripe: exact period-end cancellation target mismatch")
		}
		var disarmed struct {
			ID                string            `json:"id"`
			Customer          string            `json:"customer"`
			Status            string            `json:"status"`
			CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
			Metadata          map[string]string `json:"metadata"`
		}
		if err := p.call(ctx, "POST", path, url.Values{
			"cancel_at_period_end":                             {"false"},
			"metadata[witself_pending_downgrade_operation_id]": {""},
		}, childIdempotencyKey(
			operationID, "cancel-subscription", target.ProviderObjectID), &disarmed); err != nil {
			return err
		}
		_, hasOwner = disarmed.Metadata["witself_pending_downgrade_operation_id"]
		if disarmed.ID != target.ProviderObjectID ||
			disarmed.Customer != customerID || disarmed.CancelAtPeriodEnd ||
			!liveStripeSubscriptionStatus(disarmed.Status) ||
			hasOwner {
			return errors.New(
				"stripe: subscription response did not confirm exact period-end cancellation")
		}
		return nil
	default:
		return errors.New("stripe: unsupported exact cancellation target")
	}
}

func (p *Provider) cancelPending(
	ctx context.Context,
	customerID, operationID string,
) error {
	var sessions struct {
		Data []struct {
			ID   string `json:"id"`
			Mode string `json:"mode"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	}
	q := url.Values{"customer": {customerID}, "status": {"open"}, "limit": {"100"}}
	if err := p.call(ctx, "GET", "/v1/checkout/sessions?"+q.Encode(), nil, "", &sessions); err != nil {
		return err
	}
	if sessions.HasMore {
		return errors.New(
			"stripe: more than 100 open Checkout Sessions; refusing partial cancellation")
	}
	for _, s := range sessions.Data {
		if s.Mode != "subscription" {
			continue // leave setup-mode (card capture) links alone
		}
		if err := validateStripeResourceID(s.ID, "cs_"); err != nil {
			return fmt.Errorf("stripe: invalid legacy Checkout Session id: %w", err)
		}
		var expired struct {
			ID       string `json:"id"`
			Customer string `json:"customer"`
			Status   string `json:"status"`
		}
		if err := p.call(
			ctx,
			"POST",
			"/v1/checkout/sessions/"+url.PathEscape(s.ID)+"/expire",
			url.Values{},
			childIdempotencyKey(operationID, "cancel-checkout", s.ID),
			&expired,
		); err != nil {
			return err
		}
		if expired.ID != s.ID || expired.Customer != customerID ||
			expired.Status != "expired" {
			return errors.New(
				"stripe: legacy Checkout response did not confirm cancellation")
		}
	}
	subs, err := p.liveSubscriptions(ctx, customerID)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if !sub.CancelAtPeriodEnd {
			continue
		}
		if err := validateStripeResourceID(sub.ID, "sub_"); err != nil {
			return fmt.Errorf("stripe: invalid legacy subscription id: %w", err)
		}
		var disarmed struct {
			ID                string            `json:"id"`
			Customer          string            `json:"customer"`
			CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
			Metadata          map[string]string `json:"metadata"`
		}
		if err := p.call(ctx, "POST", "/v1/subscriptions/"+url.PathEscape(sub.ID), url.Values{
			"cancel_at_period_end":                             {"false"},
			"metadata[witself_pending_downgrade_operation_id]": {""},
		}, childIdempotencyKey(
			operationID, "cancel-subscription", sub.ID), &disarmed); err != nil {
			return err
		}
		_, hasOwner := disarmed.Metadata["witself_pending_downgrade_operation_id"]
		if disarmed.ID != sub.ID || disarmed.Customer != customerID ||
			disarmed.CancelAtPeriodEnd || hasOwner {
			return errors.New(
				"stripe: legacy subscription response did not confirm cancellation")
		}
	}
	return nil
}

// HandleWebhook implements billing.Provider: verify the Stripe-Signature,
// then collapse Stripe's event types into the normalized four. Unhandled
// types return an empty batch (ACK).
func (p *Provider) HandleWebhook(r *http.Request) ([]billing.Event, error) {
	if p.cfg.WebhookSecret == "" {
		return nil, errors.New("stripe: webhook secret not configured")
	}
	payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("stripe: read webhook body: %w", err)
	}
	if err := verifySignature(r.Header.Get("Stripe-Signature"), payload, p.cfg.WebhookSecret, p.cfg.Now()); err != nil {
		return nil, err
	}

	var event struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Created int64  `json:"created"`
		Data    struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("stripe: decode event: %w", err)
	}
	at := time.Unix(event.Created, 0).UTC()
	payloadDigest := sha256.Sum256(payload)
	payloadSHA256 := hex.EncodeToString(payloadDigest[:])
	identity := func(objectID string) billing.Event {
		return billing.Event{
			ProviderEventID:  event.ID,
			ProviderObjectID: objectID,
			PayloadSHA256:    payloadSHA256,
			At:               at,
		}
	}
	requireIdentity := func(objectID string) error {
		if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(objectID) == "" {
			return fmt.Errorf("stripe: %s missing event or object id", event.Type)
		}
		if event.Created <= 0 {
			return fmt.Errorf("stripe: %s missing provider timestamp", event.Type)
		}
		return nil
	}

	switch event.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		var s struct {
			ID            string            `json:"id"`
			Customer      string            `json:"customer"`
			Subscription  string            `json:"subscription"`
			Mode          string            `json:"mode"`
			PaymentStatus string            `json:"payment_status"`
			Metadata      map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(event.Data.Object, &s); err != nil {
			return nil, fmt.Errorf("stripe: decode session: %w", err)
		}
		if err := requireIdentity(s.ID); err != nil {
			return nil, err
		}
		if s.Mode != "subscription" {
			return []billing.Event{}, nil // setup-mode completion: card captured, no entitlement change
		}
		if s.PaymentStatus != "paid" {
			// New Witself Checkout sessions explicitly allow only synchronous
			// cards, so this is defensive compatibility for a pre-change or
			// externally-created delayed-notification session. Entitle nothing;
			// async success may later activate it, while an ambiguous failure
			// remains fenced for operator reconciliation rather than being
			// reported as a successful cancellation.
			return []billing.Event{}, nil
		}
		plan := s.Metadata["witself_plan"]
		if s.Customer == "" || plan == "" {
			return nil, fmt.Errorf("stripe: %s missing customer or witself_plan metadata", event.Type)
		}
		e := identity(s.ID)
		e.Type = billing.EventSubscriptionActivated
		e.CustomerID = s.Customer
		e.Plan = plan
		e.SubscriptionID = s.Subscription
		e.OperationID = s.Metadata["witself_operation_id"]
		return []billing.Event{e}, nil

	case "invoice.payment_failed":
		ref, err := invoiceReferenceFrom(event.Data.Object)
		if err != nil {
			return nil, err
		}
		if err := requireIdentity(ref.ID); err != nil {
			return nil, err
		}
		e := identity(ref.ID)
		e.Type = billing.EventPaymentFailed
		e.CustomerID = ref.Customer
		e.SubscriptionID = ref.Subscription
		return []billing.Event{e}, nil

	case "invoice.paid":
		ref, err := invoiceReferenceFrom(event.Data.Object)
		if err != nil {
			return nil, err
		}
		if err := requireIdentity(ref.ID); err != nil {
			return nil, err
		}
		// Every paid invoice reads as "payments are healthy" — it clears
		// PastDueSince when set and is a fenced no-op otherwise.
		e := identity(ref.ID)
		e.Type = billing.EventPaymentRecovered
		e.CustomerID = ref.Customer
		e.SubscriptionID = ref.Subscription
		return []billing.Event{e}, nil

	case "customer.subscription.deleted":
		ref, err := objectReferenceFrom(event.Data.Object)
		if err != nil {
			return nil, err
		}
		if err := requireIdentity(ref.ID); err != nil {
			return nil, err
		}
		// Do not query Stripe here. The signed event must be durably received
		// before any provider read; ResolveEvent performs the survivor check.
		e := identity(ref.ID)
		e.Type = billing.EventSubscriptionCanceled
		e.CustomerID = ref.Customer
		e.SubscriptionID = ref.ID
		return []billing.Event{e}, nil

	default:
		return []billing.Event{}, nil // not ours to fold; ACK so Stripe stops redelivering
	}
}

type stripeObjectReference struct {
	ID           string `json:"id"`
	Customer     string `json:"customer"`
	Subscription string `json:"subscription"`
}

// objectReferenceFrom pulls the reconciliation handles out of a provider
// event object without retaining the raw object.
func objectReferenceFrom(raw json.RawMessage) (stripeObjectReference, error) {
	var ref stripeObjectReference
	if err := json.Unmarshal(raw, &ref); err != nil {
		return stripeObjectReference{}, fmt.Errorf("stripe: decode event object: %w", err)
	}
	if ref.Customer == "" {
		return stripeObjectReference{}, errors.New("stripe: event object has no customer")
	}
	return ref, nil
}

// invoiceReferenceFrom supports both the Basil invoice shape and historical
// webhook endpoints. Basil removed invoice.subscription in favor of
// parent.subscription_details.subscription. A dual-shaped event must agree;
// otherwise using it as a dunning fence would be arrival-order dependent.
func invoiceReferenceFrom(raw json.RawMessage) (stripeObjectReference, error) {
	var invoice struct {
		ID           string `json:"id"`
		Customer     string `json:"customer"`
		Subscription string `json:"subscription"`
		Parent       *struct {
			Type                string `json:"type"`
			SubscriptionDetails *struct {
				Subscription string `json:"subscription"`
			} `json:"subscription_details"`
		} `json:"parent"`
	}
	if err := json.Unmarshal(raw, &invoice); err != nil {
		return stripeObjectReference{}, fmt.Errorf(
			"stripe: decode invoice event object: %w", err)
	}
	if invoice.Customer == "" {
		return stripeObjectReference{}, errors.New(
			"stripe: invoice event object has no customer")
	}
	nested := ""
	if invoice.Parent != nil {
		switch invoice.Parent.Type {
		case "subscription_details":
			if invoice.Parent.SubscriptionDetails == nil ||
				invoice.Parent.SubscriptionDetails.Subscription == "" {
				return stripeObjectReference{}, errors.New(
					"stripe: subscription invoice parent has no subscription")
			}
			nested = invoice.Parent.SubscriptionDetails.Subscription
		default:
			if invoice.Parent.SubscriptionDetails != nil &&
				invoice.Parent.SubscriptionDetails.Subscription != "" {
				return stripeObjectReference{}, errors.New(
					"stripe: invoice parent type conflicts with subscription details")
			}
		}
	}
	if invoice.Subscription != "" && nested != "" &&
		invoice.Subscription != nested {
		return stripeObjectReference{}, errors.New(
			"stripe: invoice subscription references disagree")
	}
	if nested != "" {
		invoice.Subscription = nested
	}
	return stripeObjectReference{
		ID: invoice.ID, Customer: invoice.Customer,
		Subscription: invoice.Subscription,
	}, nil
}

// ResolveEvent implements billing.EventResolver. Every entitlement or dunning
// event is collapsed to the provider's current one-subscription projection
// only after the exact signed callback is durable. This prevents a stale
// Checkout callback, a deleted duplicate, or an unrelated invoice from
// changing local account state. Provider ambiguity remains pending and
// retryable instead of being guessed.
func (p *Provider) ResolveEvent(
	ctx context.Context,
	event billing.Event,
) (*billing.Event, error) {
	switch event.Type {
	case billing.EventSubscriptionActivated,
		billing.EventSubscriptionCanceled,
		billing.EventPaymentFailed,
		billing.EventPaymentRecovered:
	default:
		resolved := event
		return &resolved, nil
	}
	sub, planID, err := p.managedLiveSubscription(ctx, event.CustomerID)
	if err != nil {
		return nil, fmt.Errorf(
			"stripe: resolve managed subscription for %s: %w",
			event.CustomerID, err)
	}
	if sub == nil {
		switch event.Type {
		case billing.EventPaymentFailed, billing.EventPaymentRecovered:
			return nil, nil
		default:
			resolved := event
			resolved.Type = billing.EventSubscriptionCanceled
			resolved.Plan = ""
			// Empty means the provider authoritatively observed no survivor; it
			// intentionally bypasses the local stale-subscription mismatch fence.
			resolved.SubscriptionID = ""
			resolved.OperationID = ""
			return &resolved, nil
		}
	}

	if event.Type == billing.EventPaymentFailed ||
		event.Type == billing.EventPaymentRecovered {
		if event.SubscriptionID == "" || event.SubscriptionID != sub.ID {
			return nil, nil
		}
		resolved := event
		resolved.Plan = planID
		resolved.SubscriptionID = sub.ID
		return &resolved, nil
	}

	resolved := event
	resolved.Type = billing.EventSubscriptionActivated
	resolved.Plan = planID
	if event.SubscriptionID != "" && event.SubscriptionID != sub.ID {
		// The provider projection is authoritative, but a callback for a
		// different subscription cannot complete the current Witself operation.
		resolved.OperationID = ""
	}
	resolved.SubscriptionID = sub.ID
	if resolved.SubscriptionID == "" {
		return nil, nil
	}
	return &resolved, nil
}

// verifySignature checks the Stripe-Signature header: HMAC-SHA256 over
// "t.payload" with the whsec secret, constant-time compare, and a 5-minute
// timestamp tolerance so captured deliveries cannot be replayed later.
func verifySignature(header string, payload []byte, secret string, now time.Time) error {
	if header == "" {
		return errors.New("stripe: missing Stripe-Signature header")
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == "" || len(sigs) == 0 {
		return errors.New("stripe: malformed Stripe-Signature header")
	}
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("stripe: malformed signature timestamp")
	}
	if d := now.Sub(time.Unix(tsInt, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return errors.New("stripe: signature timestamp outside tolerance")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	for _, sig := range sigs {
		if hmac.Equal([]byte(sig), []byte(want)) {
			return nil
		}
	}
	return errors.New("stripe: signature mismatch")
}

// RecordUsage implements billing.Provider. Usage-based billing gates the
// Team tier (phase 1) and is not wired yet.
func (p *Provider) RecordUsage(context.Context, string, string, int64, string) error {
	return errors.New("stripe: usage recording not implemented (phase 1 — gates the Team tier)")
}

// PaymentMethodOnFile implements billing.Provider.
func (p *Provider) PaymentMethodOnFile(ctx context.Context, customerID string) (*billing.PaymentMethod, error) {
	var list struct {
		Data []struct {
			Card struct {
				Brand string `json:"brand"`
				Last4 string `json:"last4"`
			} `json:"card"`
		} `json:"data"`
	}
	q := url.Values{"customer": {customerID}, "type": {"card"}, "limit": {"1"}}
	if err := p.call(ctx, "GET", "/v1/payment_methods?"+q.Encode(), nil, "", &list); err != nil {
		return nil, err
	}
	if len(list.Data) == 0 {
		return nil, nil
	}
	c := list.Data[0].Card
	return &billing.PaymentMethod{Label: c.Brand + " ****" + c.Last4}, nil
}

// ListInvoices implements billing.Provider.
func (p *Provider) ListInvoices(ctx context.Context, customerID string) ([]billing.Invoice, error) {
	var list struct {
		Data []struct {
			Number           string `json:"number"`
			Created          int64  `json:"created"`
			Total            int64  `json:"total"`
			Currency         string `json:"currency"`
			Status           string `json:"status"`
			InvoicePDF       string `json:"invoice_pdf"`
			HostedInvoiceURL string `json:"hosted_invoice_url"`
		} `json:"data"`
	}
	// 100 is Stripe's max page. The CLI shows recent history; deeper
	// pagination lands when a customer actually exceeds it.
	q := url.Values{"customer": {customerID}, "limit": {"100"}}
	if err := p.call(ctx, "GET", "/v1/invoices?"+q.Encode(), nil, "", &list); err != nil {
		return nil, err
	}
	out := make([]billing.Invoice, 0, len(list.Data))
	for _, in := range list.Data {
		out = append(out, billing.Invoice{
			Number: in.Number, Date: time.Unix(in.Created, 0).UTC(),
			AmountCents: in.Total, Currency: in.Currency, Status: in.Status,
			PDFURL: in.InvoicePDF, HostedURL: in.HostedInvoiceURL,
		})
	}
	return out, nil
}

// ListPayments implements billing.Provider for Stripe's bounded recent charge
// page. Refunds attached to those charges are expanded into negative ledger
// movements; the hosted portal remains authoritative for refunds against older
// charges outside this deliberately bounded read surface.
func (p *Provider) ListPayments(ctx context.Context, customerID string) ([]billing.Payment, error) {
	var list struct {
		Data []struct {
			ID                   string `json:"id"`
			Created              int64  `json:"created"`
			Amount               int64  `json:"amount"`
			AmountRefunded       int64  `json:"amount_refunded"`
			Currency             string `json:"currency"`
			Status               string `json:"status"`
			ReceiptURL           string `json:"receipt_url"`
			PaymentMethodDetails struct {
				Card struct {
					Brand string `json:"brand"`
					Last4 string `json:"last4"`
				} `json:"card"`
			} `json:"payment_method_details"`
		} `json:"data"`
	}
	q := url.Values{"customer": {customerID}, "limit": {"100"}} // Stripe's max page; see ListInvoices
	if err := p.call(ctx, "GET", "/v1/charges?"+q.Encode(), nil, "", &list); err != nil {
		return nil, err
	}
	out := make([]billing.Payment, 0, len(list.Data))
	for _, c := range list.Data {
		method := "card"
		if c.PaymentMethodDetails.Card.Last4 != "" {
			method = c.PaymentMethodDetails.Card.Brand + " ****" + c.PaymentMethodDetails.Card.Last4
		}
		out = append(out, billing.Payment{
			Date: time.Unix(c.Created, 0).UTC(), AmountCents: c.Amount,
			Currency: c.Currency, Method: method, Status: c.Status, ReceiptURL: c.ReceiptURL,
		})
		if c.AmountRefunded <= 0 || c.ID == "" {
			continue
		}
		refunds, err := p.listChargeRefunds(
			ctx, c.ID, c.Currency, method, c.AmountRefunded,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, refunds...)
	}
	slices.SortStableFunc(out, func(a, b billing.Payment) int {
		return b.Date.Compare(a.Date)
	})
	if len(out) > 100 {
		out = out[:100]
	}
	return out, nil
}

// listChargeRefunds reads the complete recent refund list for one refunded
// charge. Stripe embeds only a bounded sub-list on a charge, so relying on the
// charge object alone can silently omit partial refunds. Refunds are rare, and
// this extra request happens only when amount_refunded is non-zero.
func (p *Provider) listChargeRefunds(
	ctx context.Context,
	chargeID, currency, method string,
	expectedSettledAmount int64,
) ([]billing.Payment, error) {
	var list struct {
		Data []struct {
			Created int64  `json:"created"`
			Amount  int64  `json:"amount"`
			Status  string `json:"status"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	}
	q := url.Values{"charge": {chargeID}, "limit": {"100"}}
	if err := p.call(ctx, "GET", "/v1/refunds?"+q.Encode(), nil, "", &list); err != nil {
		return nil, err
	}
	if list.HasMore {
		return nil, errors.New("stripe: refund history exceeds the supported page")
	}
	out := make([]billing.Payment, 0, len(list.Data))
	var settledAmount int64
	for _, refund := range list.Data {
		if refund.Amount <= 0 {
			return nil, errors.New("stripe: invalid refund amount")
		}
		switch refund.Status {
		case "succeeded":
			if settledAmount > expectedSettledAmount ||
				refund.Amount > expectedSettledAmount-settledAmount {
				return nil, errors.New(
					"stripe: settled refunds exceed charge amount_refunded")
			}
			settledAmount += refund.Amount
			out = append(out, billing.Payment{
				Date:        time.Unix(refund.Created, 0).UTC(),
				AmountCents: -refund.Amount,
				Currency:    currency,
				Method:      method,
				Status:      "refunded",
			})
		case "pending", "requires_action", "failed", "canceled":
			// These are attempts, not completed negative money movement. The
			// charge's amount_refunded is the settled aggregate; omit them from
			// the customer ledger instead of overstating the refund.
		default:
			return nil, fmt.Errorf(
				"stripe: unsupported refund status %q", refund.Status)
		}
	}
	if settledAmount != expectedSettledAmount {
		return nil, fmt.Errorf(
			"stripe: settled refund total %d does not match charge amount_refunded %d",
			settledAmount, expectedSettledAmount,
		)
	}
	return out, nil
}

// NextCharge implements billing.Provider via an invoice preview
// (POST /v1/invoices/create_preview — Basil's replacement for the removed
// GET /v1/invoices/upcoming). The preview requires a subscription (a bare
// customer 400s, live-verified), so: no live subscription -> nil, otherwise
// preview the subscription's next invoice. Only the specific
// invoice_upcoming_none error code maps to nil (the subscription is ending)
// — anything else stays an error, so an endpoint-shape regression cannot
// masquerade as "no upcoming charge" forever.
func (p *Provider) NextCharge(ctx context.Context, customerID string) (*billing.UpcomingCharge, error) {
	sub, _, err := p.managedLiveSubscription(ctx, customerID)
	if err != nil {
		return nil, err
	}
	if sub == nil {
		return nil, nil // nothing to charge
	}
	var up struct {
		AmountDue          int64  `json:"amount_due"`
		Currency           string `json:"currency"`
		NextPaymentAttempt int64  `json:"next_payment_attempt"`
		PeriodEnd          int64  `json:"period_end"`
	}
	previewParams := url.Values{
		"customer":     {customerID},
		"subscription": {sub.ID},
	}
	if p.cfg.AutomaticTax {
		// Without this the preview quotes the untaxed amount while the renewal
		// charges the taxed one, so the customer-facing next-charge figure
		// would be wrong by exactly the tax.
		previewParams.Set("automatic_tax[enabled]", "true")
	}
	err = p.call(ctx, "POST", "/v1/invoices/create_preview", previewParams, "", &up)
	if err != nil {
		var se *apiError
		if errors.As(err, &se) && se.code == "invoice_upcoming_none" {
			return nil, nil // ending at period end: no further charge coming
		}
		return nil, err
	}
	at := up.NextPaymentAttempt
	if at == 0 {
		at = up.PeriodEnd
	}
	return &billing.UpcomingCharge{
		Date: time.Unix(at, 0).UTC(), AmountCents: up.AmountDue, Currency: up.Currency,
	}, nil
}

// apiError is a non-2xx Stripe response.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("stripe: %d %s: %s", e.status, e.code, e.message)
}

// call performs one form-encoded Stripe API request. idempotencyKey, when
// non-empty, makes Stripe replay the original response for retries.
func (p *Provider) call(ctx context.Context, method, path string, params url.Values, idempotencyKey string, out any) error {
	var body io.Reader
	if params != nil {
		body = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, p.cfg.BaseURL+path, body)
	if err != nil {
		return fmt.Errorf("stripe: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.SecretKey)
	req.Header.Set("Stripe-Version", apiVersion)
	if params != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("stripe: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("stripe: read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return &apiError{status: resp.StatusCode, code: e.Error.Code, message: e.Error.Message}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("stripe: decode response: %w", err)
		}
	}
	return nil
}

// priceCache is a tiny concurrent map.
type priceCache struct {
	mu  sync.Mutex
	ids map[string]string
}

func (c *priceCache) get(k string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.ids[k]
	return v, ok
}

func (c *priceCache) put(k, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids[k] = v
}
