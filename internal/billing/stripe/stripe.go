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
	// point at the public site until the CP has a better destination.
	SuccessURL string
	CancelURL  string
	// PortalReturnURL is where the hosted portal's "back" goes.
	PortalReturnURL string
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

// New validates cfg and returns a Provider.
func New(cfg Config) (*Provider, error) {
	if cfg.SecretKey == "" {
		return nil, errors.New("stripe: SecretKey is required")
	}
	if cfg.Catalog == nil {
		return nil, errors.New("stripe: Catalog is required")
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
		cfg.SuccessURL = "https://witself.witwave.ai/billing/success"
	}
	if cfg.CancelURL == "" {
		cfg.CancelURL = "https://witself.witwave.ai/billing/cancelled"
	}
	if cfg.PortalReturnURL == "" {
		cfg.PortalReturnURL = "https://witself.witwave.ai"
	}
	return &Provider{cfg: cfg, prices: priceCache{ids: map[string]string{}}}, nil
}

// lookupKey names a plan's price in Stripe: "witself_standard".
func lookupKey(planID string) string { return "witself_" + planID }

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

// createPrice mints a monthly recurring price carrying the plan's
// lookup_key, transferring it from any prior price. The Idempotency-Key
// includes the amount, so a retried create replays but a changed catalog
// price creates fresh.
func (p *Provider) createPrice(ctx context.Context, productID, planID string, cents int64, key string) (string, error) {
	var price struct {
		ID string `json:"id"`
	}
	if err := p.call(ctx, "POST", "/v1/prices", url.Values{
		"product":             {productID},
		"unit_amount":         {strconv.FormatInt(cents, 10)},
		"currency":            {"usd"},
		"recurring[interval]": {"month"},
		"lookup_key":          {key},
		"transfer_lookup_key": {"true"},
	}, fmt.Sprintf("witself-price-%s-%d", planID, cents), &price); err != nil {
		return "", err
	}
	return price.ID, nil
}

// EnsureCustomer implements billing.Provider. The Idempotency-Key derived
// from the account id makes near-term retries return the same customer —
// BUT Stripe replays the original response even after the resource was
// deleted (live-test-discovered), so the replayed customer is verified and
// the create retried with a fresh key when it no longer exists.
func (p *Provider) EnsureCustomer(ctx context.Context, accountID, email string) (string, error) {
	params := url.Values{"metadata[witself_account]": {accountID}}
	if email != "" {
		params.Set("email", email)
	}
	// Walk deterministic key generations: the stable key first, then -g1,
	// -g2, … A generation goes stale when its 24h replay window returns a
	// customer that was since deleted, or 400s idempotency_error because
	// the params changed under it (email update). Deterministic generations
	// (rather than a time salt) keep EnsureCustomer stable across near-term
	// retries even when earlier generations are poisoned — retries walk the
	// same chain to the same live customer.
	for gen := 0; gen < 16; gen++ {
		key := "witself-ensure-" + accountID
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
			return cust.ID, nil
		}
	}
	// Pathological: 16 poisoned generations inside one 24h window. A
	// time-salted key is the last resort — unique, but not retry-stable.
	var cust struct {
		ID string `json:"id"`
	}
	salted := fmt.Sprintf("witself-ensure-%s-%d", accountID, p.cfg.Now().Unix())
	if err := p.call(ctx, "POST", "/v1/customers", params, salted, &cust); err != nil {
		return "", err
	}
	return cust.ID, nil
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
	priceID, err := p.priceID(ctx, plan)
	if err != nil {
		return billing.Action{}, err
	}
	var session struct {
		URL string `json:"url"`
	}
	err = p.call(ctx, "POST", "/v1/checkout/sessions", url.Values{
		"mode":                    {"subscription"},
		"customer":                {customerID},
		"line_items[0][price]":    {priceID},
		"line_items[0][quantity]": {"1"},
		"success_url":             {p.cfg.SuccessURL},
		"cancel_url":              {p.cfg.CancelURL},
		"metadata[witself_plan]":  {plan},
		"subscription_data[metadata][witself_plan]": {plan},
	}, "", &session)
	if err != nil {
		return billing.Action{}, err
	}
	return billing.Action{URL: session.URL}, nil
}

// SetupLink implements billing.Provider: a Checkout session in setup mode
// (card capture without charging).
func (p *Provider) SetupLink(ctx context.Context, customerID string) (billing.Action, error) {
	var session struct {
		URL string `json:"url"`
	}
	err := p.call(ctx, "POST", "/v1/checkout/sessions", url.Values{
		"mode":        {"setup"},
		"customer":    {customerID},
		"currency":    {"usd"}, // required in setup mode (no line items to infer it from)
		"success_url": {p.cfg.SuccessURL},
		"cancel_url":  {p.cfg.CancelURL},
	}, "", &session)
	if err != nil {
		return billing.Action{}, err
	}
	return billing.Action{URL: session.URL}, nil
}

// PortalLink implements billing.Provider: a hosted customer-portal session
// (the default portal configuration — activated in the dashboard).
func (p *Provider) PortalLink(ctx context.Context, customerID string) (string, error) {
	var session struct {
		URL string `json:"url"`
	}
	err := p.call(ctx, "POST", "/v1/billing_portal/sessions", url.Values{
		"customer":   {customerID},
		"return_url": {p.cfg.PortalReturnURL},
	}, "", &session)
	if err != nil {
		return "", err
	}
	return session.URL, nil
}

// stripeSubscription is the slice of the subscription object read here. As
// of API 2025-03-31.basil, current_period_end lives on subscription ITEMS —
// it was removed from the subscription top level.
type stripeSubscription struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	CancelAtPeriodEnd bool   `json:"cancel_at_period_end"`
	Items             struct {
		Data []struct {
			CurrentPeriodEnd int64 `json:"current_period_end"`
		} `json:"data"`
	} `json:"items"`
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

// liveSubscriptions lists the customer's subscriptions that still back an
// entitlement: active, trialing, past_due, or unpaid. past_due matters — a
// dunned customer must still be able to downgrade to free, and a
// status=active filter made them invisible. incomplete (a checkout
// mid-flight) and canceled do not count.
func (p *Provider) liveSubscriptions(ctx context.Context, customerID string) ([]stripeSubscription, error) {
	var list struct {
		Data []stripeSubscription `json:"data"`
	}
	q := url.Values{"customer": {customerID}, "limit": {"100"}}
	if err := p.call(ctx, "GET", "/v1/subscriptions?"+q.Encode(), nil, "", &list); err != nil {
		return nil, err
	}
	live := list.Data[:0]
	for _, s := range list.Data {
		switch s.Status {
		case "active", "trialing", "past_due", "unpaid":
			live = append(live, s)
		}
	}
	return live, nil
}

// ScheduleDowngrade implements billing.Provider. Today's only downgrade
// target is free (Standard is the sole purchasable paid plan):
// cancel_at_period_end ends the subscription at the period boundary and
// Stripe announces it with customer.subscription.deleted — the canceled
// event the Manager already folds. Downgrade-to-free ends EVERY live
// subscription, so if a duplicate ever slipped in (two paid checkout tabs)
// this is also the self-heal. Paid-to-paid needs subscription schedules;
// that lands with the Team tier.
func (p *Provider) ScheduleDowngrade(ctx context.Context, customerID, plan string) (time.Time, error) {
	if plan != plans.Free {
		return time.Time{}, fmt.Errorf("stripe: downgrade to %q not supported yet (only free; paid-to-paid lands with the Team tier)", plan)
	}
	subs, err := p.liveSubscriptions(ctx, customerID)
	if err != nil {
		return time.Time{}, err
	}
	if len(subs) == 0 {
		return time.Time{}, fmt.Errorf("stripe: customer %s has no live subscription", customerID)
	}
	var latest time.Time
	for _, sub := range subs {
		if err := p.call(ctx, "POST", "/v1/subscriptions/"+sub.ID, url.Values{
			"cancel_at_period_end": {"true"},
		}, "", nil); err != nil {
			return time.Time{}, err
		}
		if pe := sub.periodEnd(); pe.After(latest) {
			latest = pe
		}
	}
	return latest, nil
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
	var sessions struct {
		Data []struct {
			ID   string `json:"id"`
			Mode string `json:"mode"`
		} `json:"data"`
	}
	q := url.Values{"customer": {customerID}, "status": {"open"}, "limit": {"100"}}
	if err := p.call(ctx, "GET", "/v1/checkout/sessions?"+q.Encode(), nil, "", &sessions); err != nil {
		return err
	}
	for _, s := range sessions.Data {
		if s.Mode != "subscription" {
			continue // leave setup-mode (card capture) links alone
		}
		if err := p.call(ctx, "POST", "/v1/checkout/sessions/"+s.ID+"/expire", url.Values{}, "", nil); err != nil {
			return err
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
		if err := p.call(ctx, "POST", "/v1/subscriptions/"+sub.ID, url.Values{
			"cancel_at_period_end": {"false"},
		}, "", nil); err != nil {
			return err
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

	switch event.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		var s struct {
			Customer      string            `json:"customer"`
			Mode          string            `json:"mode"`
			PaymentStatus string            `json:"payment_status"`
			Metadata      map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(event.Data.Object, &s); err != nil {
			return nil, fmt.Errorf("stripe: decode session: %w", err)
		}
		if s.Mode != "subscription" {
			return []billing.Event{}, nil // setup-mode completion: card captured, no entitlement change
		}
		if s.PaymentStatus != "paid" {
			// A delayed-notification method (ACH debit, SEPA): the session
			// completed but the money has not moved. Entitle nothing yet —
			// checkout.session.async_payment_succeeded lands here again with
			// payment_status=paid when it clears, and a failure never
			// entitles at all (the Manager's pending TTL cleans up).
			return []billing.Event{}, nil
		}
		plan := s.Metadata["witself_plan"]
		if s.Customer == "" || plan == "" {
			return nil, fmt.Errorf("stripe: %s missing customer or witself_plan metadata", event.Type)
		}
		return []billing.Event{{
			Type: billing.EventSubscriptionActivated, CustomerID: s.Customer, Plan: plan, At: at,
		}}, nil

	case "invoice.payment_failed":
		cust, err := objectCustomer(event.Data.Object)
		if err != nil {
			return nil, err
		}
		return []billing.Event{{Type: billing.EventPaymentFailed, CustomerID: cust, At: at}}, nil

	case "invoice.paid":
		cust, err := objectCustomer(event.Data.Object)
		if err != nil {
			return nil, err
		}
		// Every paid invoice reads as "payments are healthy" — it clears
		// PastDueSince when set and is a fenced no-op otherwise.
		return []billing.Event{{Type: billing.EventPaymentRecovered, CustomerID: cust, At: at}}, nil

	case "customer.subscription.deleted":
		cust, err := objectCustomer(event.Data.Object)
		if err != nil {
			return nil, err
		}
		// The event names only the customer, not which subscription of
		// theirs died — so a duplicate/stale subscription's deletion must
		// not revoke a live paid entitlement. Emit the cancel only when NO
		// live subscription remains; an API error propagates so Stripe
		// retries the delivery.
		subs, err := p.liveSubscriptions(r.Context(), cust)
		if err != nil {
			return nil, fmt.Errorf("stripe: verify remaining subscriptions for %s: %w", cust, err)
		}
		if len(subs) > 0 {
			return []billing.Event{}, nil // another subscription still backs the entitlement
		}
		return []billing.Event{{Type: billing.EventSubscriptionCanceled, CustomerID: cust, At: at}}, nil

	default:
		return []billing.Event{}, nil // not ours to fold; ACK so Stripe stops redelivering
	}
}

// objectCustomer pulls the customer id out of an event payload object.
func objectCustomer(raw json.RawMessage) (string, error) {
	var o struct {
		Customer string `json:"customer"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return "", fmt.Errorf("stripe: decode event object: %w", err)
	}
	if o.Customer == "" {
		return "", errors.New("stripe: event object has no customer")
	}
	return o.Customer, nil
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

// ListPayments implements billing.Provider.
func (p *Provider) ListPayments(ctx context.Context, customerID string) ([]billing.Payment, error) {
	var list struct {
		Data []struct {
			Created              int64  `json:"created"`
			Amount               int64  `json:"amount"`
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
	subs, err := p.liveSubscriptions(ctx, customerID)
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return nil, nil // nothing to charge
	}
	var up struct {
		AmountDue          int64  `json:"amount_due"`
		Currency           string `json:"currency"`
		NextPaymentAttempt int64  `json:"next_payment_attempt"`
		PeriodEnd          int64  `json:"period_end"`
	}
	err = p.call(ctx, "POST", "/v1/invoices/create_preview", url.Values{
		"customer":     {customerID},
		"subscription": {subs[0].ID},
	}, "", &up)
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
