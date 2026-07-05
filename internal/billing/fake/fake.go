// Package fake is the in-memory billing.Provider used until a real partner is
// wired (and forever after in tests and self-contained dev). It exercises the
// COMPLETE plan lifecycle — checkout, entitlement, scheduled downgrades,
// invoices, payments, usage — with no network and no billing account.
//
// Two modes:
//   - headless (default): every Action returns Done immediately, so the whole
//     upgrade flow runs end-to-end in one call — what CI and dev want.
//   - interactive: first-time purchases return a fake checkout URL and park as
//     pending until Complete() is called — simulating the abandoned-checkout,
//     resume, and webhook paths so the control plane's state machine can be
//     tested against the same shapes a real provider produces.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
)

// Config tunes the fake. Zero value is usable: headless, 30-day periods,
// real clock, no known plans (Subscribe then fails with unknown plan).
type Config struct {
	// Prices maps plan id -> monthly price in cents. Plans absent from the map
	// cannot be subscribed to ("free" is deliberately absent everywhere: free
	// is the zero value of billing, not a subscription).
	Prices map[string]int64
	// Currency defaults to "usd".
	Currency string
	// Interactive makes first-time purchases and setup return a URL + pending
	// state (completed via Complete) instead of finishing headlessly.
	Interactive bool
	// Now injects a clock for tests. Defaults to time.Now.
	Now func() time.Time
	// PeriodDays is the billing period length. Defaults to 30.
	PeriodDays int
}

type pendingKind string

const (
	pendingCheckout  pendingKind = "checkout"  // awaiting payment (interactive)
	pendingSetup     pendingKind = "setup"     // awaiting card capture (interactive)
	pendingDowngrade pendingKind = "downgrade" // scheduled for period end
)

type pending struct {
	kind pendingKind
	plan string    // checkout/downgrade target
	url  string    // checkout/setup continue-URL
	at   time.Time // downgrade effective time
}

type customer struct {
	id        string
	accountID string
	email     string
	card      *billing.PaymentMethod
	plan      string // current paid plan; "" = none (free)
	periodEnd time.Time
	pending   *pending
	invoices  []billing.Invoice
	payments  []billing.Payment
	usage     map[string]int64 // metric -> total
	usageKeys map[string]bool  // idempotency keys seen
}

// Fake implements billing.Provider in memory. Safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	cfg     Config
	byAcct  map[string]*customer // accountID -> customer
	byID    map[string]*customer // customerID -> customer
	custSeq int
	invSeq  int
}

var _ billing.Provider = (*Fake)(nil)

// New returns a Fake with cfg defaults applied.
func New(cfg Config) *Fake {
	if cfg.Currency == "" {
		cfg.Currency = "usd"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PeriodDays == 0 {
		cfg.PeriodDays = 30
	}
	return &Fake{
		cfg:    cfg,
		byAcct: map[string]*customer{},
		byID:   map[string]*customer{},
	}
}

// EnsureCustomer implements billing.Provider: it returns the existing
// customer for accountID or creates one. Idempotent per accountID.
func (f *Fake) EnsureCustomer(_ context.Context, accountID, email string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.byAcct[accountID]; ok {
		return c.id, nil
	}
	f.custSeq++
	c := &customer{
		id:        fmt.Sprintf("fake_cus_%04d", f.custSeq),
		accountID: accountID,
		email:     email,
		usage:     map[string]int64{},
		usageKeys: map[string]bool{},
	}
	f.byAcct[accountID] = c
	f.byID[c.id] = c
	return c.id, nil
}

// SetupLink implements billing.Provider. Headless mode puts a card on file
// immediately; interactive mode returns a fake URL completed via Complete.
func (f *Fake) SetupLink(_ context.Context, customerID string) (billing.Action, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.cust(customerID)
	if err != nil {
		return billing.Action{}, err
	}
	if c.card != nil {
		return billing.Action{Done: true}, nil
	}
	if f.cfg.Interactive {
		c.pending = &pending{kind: pendingSetup, url: fakeURL("setup", c.id)}
		return billing.Action{URL: c.pending.url}, nil
	}
	c.card = &billing.PaymentMethod{Label: "visa ****4242"}
	return billing.Action{Done: true}, nil
}

// PortalLink implements billing.Provider with a fake portal URL.
func (f *Fake) PortalLink(_ context.Context, customerID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.cust(customerID); err != nil {
		return "", err
	}
	return fakeURL("portal", customerID), nil
}

// Subscribe implements billing.Provider. With a card on file (or in headless
// mode) it charges immediately; otherwise it parks a pending checkout whose
// URL the payer would visit, finished via Complete.
func (f *Fake) Subscribe(_ context.Context, customerID, plan string) (billing.Action, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.cust(customerID)
	if err != nil {
		return billing.Action{}, err
	}
	if _, ok := f.cfg.Prices[plan]; !ok {
		return billing.Action{}, fmt.Errorf("unknown plan %q", plan)
	}
	// A new request replaces whatever was pending (one change at a time).
	c.pending = nil
	if f.cfg.Interactive && c.card == nil {
		c.pending = &pending{kind: pendingCheckout, plan: plan, url: fakeURL("checkout", c.id)}
		return billing.Action{URL: c.pending.url}, nil
	}
	f.charge(c, plan)
	return billing.Action{Done: true}, nil
}

// ScheduleDowngrade implements billing.Provider: the change is recorded and
// takes effect at the current period end (applied by ApplyDue).
func (f *Fake) ScheduleDowngrade(_ context.Context, customerID, plan string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.cust(customerID)
	if err != nil {
		return time.Time{}, err
	}
	if c.plan == "" {
		return time.Time{}, fmt.Errorf("customer %s has no subscription to downgrade", customerID)
	}
	// Validate the target like Subscribe does — otherwise a typo'd plan id
	// would silently be treated as free and cancel a paid subscription at
	// period end. Exactly one unpriced target is legitimate: "free", the
	// catalog's zero-value plan. Priced targets must actually be downgrades.
	price, priced := f.cfg.Prices[plan]
	switch {
	case plan == "free":
		// The subscription ends at period end.
	case !priced:
		return time.Time{}, fmt.Errorf("unknown plan %q", plan)
	case price >= f.cfg.Prices[c.plan]:
		return time.Time{}, fmt.Errorf("plan %q is not a downgrade from %q", plan, c.plan)
	}
	c.pending = &pending{kind: pendingDowngrade, plan: plan, at: c.periodEnd}
	return c.periodEnd, nil
}

// CancelPending implements billing.Provider: it abandons any pending
// checkout, setup, or scheduled downgrade.
func (f *Fake) CancelPending(_ context.Context, customerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.cust(customerID)
	if err != nil {
		return err
	}
	c.pending = nil
	return nil
}

// HandleWebhook parses the fake's callback shape — a JSON body
// {"customer_id": "...", "type": "...", "plan": "..."} — so the control
// plane's webhook route can be exercised end-to-end without a real provider.
func (f *Fake) HandleWebhook(r *http.Request) ([]billing.Event, error) {
	var body struct {
		CustomerID string `json:"customer_id"`
		Type       string `json:"type"`
		Plan       string `json:"plan"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("fake webhook: %w", err)
	}
	if body.CustomerID == "" || body.Type == "" {
		return nil, fmt.Errorf("fake webhook: customer_id and type are required")
	}
	// Enforce the normalized-events contract: the four EventType constants are
	// the only billing facts a Provider may emit. Rejecting unknown types here
	// surfaces mis-mapped events as errors instead of letting them silently
	// fall through the control plane's switch.
	switch billing.EventType(body.Type) {
	case billing.EventSubscriptionActivated, billing.EventPaymentFailed,
		billing.EventPaymentRecovered, billing.EventSubscriptionCanceled:
	default:
		return nil, fmt.Errorf("fake webhook: unknown event type %q", body.Type)
	}
	return []billing.Event{{
		Type:       billing.EventType(body.Type),
		CustomerID: body.CustomerID,
		Plan:       body.Plan,
		At:         f.cfg.Now(),
	}}, nil
}

// RecordUsage implements billing.Provider. Deliveries repeating an
// idempotency key are dropped so retries cannot double-bill.
func (f *Fake) RecordUsage(_ context.Context, customerID, metric string, quantity int64, idempotencyKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.cust(customerID)
	if err != nil {
		return err
	}
	if idempotencyKey != "" && c.usageKeys[idempotencyKey] {
		return nil // duplicate delivery: already recorded, not an error
	}
	if idempotencyKey != "" {
		c.usageKeys[idempotencyKey] = true
	}
	c.usage[metric] += quantity
	return nil
}

// PaymentMethodOnFile implements billing.Provider; nil when no card is on file.
func (f *Fake) PaymentMethodOnFile(_ context.Context, customerID string) (*billing.PaymentMethod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.cust(customerID)
	if err != nil {
		return nil, err
	}
	if c.card == nil {
		return nil, nil
	}
	pm := *c.card
	return &pm, nil
}

// ListInvoices implements billing.Provider, newest first.
func (f *Fake) ListInvoices(_ context.Context, customerID string) ([]billing.Invoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.cust(customerID)
	if err != nil {
		return nil, err
	}
	out := append([]billing.Invoice(nil), c.invoices...)
	sort.Slice(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out, nil
}

// ListPayments implements billing.Provider, newest first.
func (f *Fake) ListPayments(_ context.Context, customerID string) ([]billing.Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.cust(customerID)
	if err != nil {
		return nil, err
	}
	out := append([]billing.Payment(nil), c.payments...)
	sort.Slice(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out, nil
}

// NextCharge implements billing.Provider: the upcoming renewal, accounting
// for a scheduled downgrade (nil when nothing will be charged).
func (f *Fake) NextCharge(_ context.Context, customerID string) (*billing.UpcomingCharge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.cust(customerID)
	if err != nil {
		return nil, err
	}
	if c.plan == "" {
		return nil, nil
	}
	// The renewal charges the current plan — unless a downgrade lands first,
	// in which case the next charge is the target plan's (or nothing, when the
	// target isn't a paid plan).
	plan := c.plan
	if p := c.pending; p != nil && p.kind == pendingDowngrade {
		plan = p.plan
	}
	price, ok := f.cfg.Prices[plan]
	if !ok {
		return nil, nil // downgrade to free (or any unpriced plan): nothing upcoming
	}
	return &billing.UpcomingCharge{Date: c.periodEnd, AmountCents: price, Currency: f.cfg.Currency}, nil
}

// Complete finishes the customer's pending interactive action — the payer
// "returning from the browser". It applies the change and returns the events a
// real provider would deliver by webhook. Errors when nothing is pending.
func (f *Fake) Complete(customerID string) ([]billing.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.cust(customerID)
	if err != nil {
		return nil, err
	}
	p := c.pending
	if p == nil {
		return nil, fmt.Errorf("customer %s has nothing pending", customerID)
	}
	switch p.kind {
	case pendingSetup:
		c.card = &billing.PaymentMethod{Label: "visa ****4242"}
		c.pending = nil
		return nil, nil
	case pendingCheckout:
		c.card = &billing.PaymentMethod{Label: "visa ****4242"} // checkout captures the card too
		plan := p.plan
		c.pending = nil
		f.charge(c, plan)
		return []billing.Event{{
			Type: billing.EventSubscriptionActivated, CustomerID: c.id, Plan: plan, At: f.cfg.Now(),
		}}, nil
	default:
		return nil, fmt.Errorf("pending %s is not completable — it applies at %s", p.kind, p.at.Format(time.RFC3339))
	}
}

// ApplyDue applies every scheduled downgrade whose effective time has passed
// (the period ended), returning the events a real provider would deliver by
// webhook. The control plane's reconciler is the intended caller.
func (f *Fake) ApplyDue() []billing.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.cfg.Now()
	var events []billing.Event
	for _, c := range f.byID {
		p := c.pending
		if p == nil || p.kind != pendingDowngrade || now.Before(p.at) {
			continue
		}
		c.pending = nil
		if _, paid := f.cfg.Prices[p.plan]; paid {
			f.charge(c, p.plan)
			events = append(events, billing.Event{
				Type: billing.EventSubscriptionActivated, CustomerID: c.id, Plan: p.plan, At: now,
			})
			continue
		}
		// Downgrade to an unpriced plan (free): the subscription ends.
		c.plan = ""
		c.periodEnd = time.Time{}
		events = append(events, billing.Event{
			Type: billing.EventSubscriptionCanceled, CustomerID: c.id, At: now,
		})
	}
	return events
}

// UsageTotal reports recorded usage for assertions in tests.
func (f *Fake) UsageTotal(customerID, metric string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.byID[customerID]
	if !ok {
		return 0
	}
	return c.usage[metric]
}

// charge starts (or switches) the subscription: records a paid invoice and a
// succeeded payment, sets the plan, and opens a fresh period. Callers hold mu.
func (f *Fake) charge(c *customer, plan string) {
	price := f.cfg.Prices[plan]
	now := f.cfg.Now()
	f.invSeq++
	number := fmt.Sprintf("%04d", f.invSeq)
	c.invoices = append(c.invoices, billing.Invoice{
		Number:      number,
		Date:        now,
		AmountCents: price,
		Currency:    f.cfg.Currency,
		Status:      "paid",
		PDFURL:      fakeURL("invoice", number+".pdf"),
		HostedURL:   fakeURL("invoice", number),
	})
	method := "none"
	if c.card != nil {
		method = c.card.Label
	}
	c.payments = append(c.payments, billing.Payment{
		Date:        now,
		AmountCents: price,
		Currency:    f.cfg.Currency,
		Method:      method,
		Status:      "succeeded",
		ReceiptURL:  fakeURL("receipt", number),
	})
	c.plan = plan
	c.periodEnd = now.AddDate(0, 0, f.cfg.PeriodDays)
}

func (f *Fake) cust(customerID string) (*customer, error) {
	c, ok := f.byID[customerID]
	if !ok {
		return nil, fmt.Errorf("no such customer %q", customerID)
	}
	return c, nil
}

func fakeURL(kind, ref string) string {
	return "https://billing.fake.invalid/" + kind + "/" + ref
}
