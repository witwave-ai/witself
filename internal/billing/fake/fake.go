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
	"crypto/subtle"
	"encoding/json"
	"errors"
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
	// WebhookSecret, when set, makes HandleWebhook require the
	// X-Witself-Fake-Signature header to equal it — the fake's stand-in for
	// a real partner's signature verification. Without it, a PUBLICLY
	// mounted fake webhook route would accept forged entitlement events
	// from anonymous callers (customer ids are guessably sequential).
	WebhookSecret string
}

type pendingKind string

const (
	pendingCheckout  pendingKind = "checkout"  // awaiting payment (interactive)
	pendingSetup     pendingKind = "setup"     // awaiting card capture (interactive)
	pendingDowngrade pendingKind = "downgrade" // scheduled for period end
)

type pending struct {
	kind        pendingKind
	plan        string    // checkout/downgrade target
	url         string    // checkout/setup continue-URL
	at          time.Time // downgrade effective time
	operationID string    // checkout's durable Witself mutation identity
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

type subscribeReplay struct {
	customerID string
	plan       string
	action     billing.Action
}

type setupReplay struct {
	customerID string
	action     billing.Action
}

type downgradeReplay struct {
	customerID string
	plan       string
	effective  time.Time
}

type cancelReplay struct {
	customerID          string
	kind                billing.PendingCancellationKind
	providerObjectID    string
	originalOperationID string
}

// Fake implements billing.Provider in memory. Safe for concurrent use.
type Fake struct {
	mu     sync.Mutex
	cfg    Config
	byAcct map[string]*customer // accountID -> customer
	byID   map[string]*customer // customerID -> customer
	// Operation replay maps are provider-wide because an idempotency key names
	// one mutation, not one customer. Reusing a key with different parameters
	// is a caller error, matching Stripe's idempotency semantics.
	setupOps     map[string]setupReplay
	subscribeOps map[string]subscribeReplay
	downgradeOps map[string]downgradeReplay
	cancelOps    map[string]cancelReplay
	custSeq      int
	invSeq       int
}

var _ billing.Provider = (*Fake)(nil)
var _ billing.IdempotentSetupper = (*Fake)(nil)
var _ billing.IdempotentSubscriber = (*Fake)(nil)
var _ billing.IdempotentDowngrader = (*Fake)(nil)
var _ billing.ExactIdempotentDowngrader = (*Fake)(nil)
var _ billing.IdempotentPendingCanceller = (*Fake)(nil)
var _ billing.ExactPendingCanceller = (*Fake)(nil)

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
		cfg:          cfg,
		byAcct:       map[string]*customer{},
		byID:         map[string]*customer{},
		setupOps:     map[string]setupReplay{},
		subscribeOps: map[string]subscribeReplay{},
		downgradeOps: map[string]downgradeReplay{},
		cancelOps:    map[string]cancelReplay{},
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
	return f.setupLinkLocked(customerID)
}

// SetupLinkIdempotent implements billing.IdempotentSetupper. An exact replay
// returns the original action without replacing an interactive setup session;
// reusing the operation identity for another customer fails closed.
func (f *Fake) SetupLinkIdempotent(
	_ context.Context,
	customerID, operationID string,
) (billing.Action, error) {
	if err := billing.ValidateOperationID(operationID); err != nil {
		return billing.Action{}, fmt.Errorf("fake billing: setup: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if replay, ok := f.setupOps[operationID]; ok {
		if replay.customerID != customerID {
			return billing.Action{}, fmt.Errorf(
				"fake billing: setup operation was reused with different parameters")
		}
		return replay.action, nil
	}
	action, err := f.setupLinkLocked(customerID)
	if err != nil {
		return billing.Action{}, err
	}
	f.setupOps[operationID] = setupReplay{customerID: customerID, action: action}
	return action, nil
}

// setupLinkLocked performs the non-replayed mutation. Callers hold f.mu.
func (f *Fake) setupLinkLocked(customerID string) (billing.Action, error) {
	c, err := f.cust(customerID)
	if err != nil {
		return billing.Action{}, err
	}
	if c.card != nil {
		return billing.Action{Done: true}, nil
	}
	if f.cfg.Interactive {
		c.pending = &pending{kind: pendingSetup, url: fakeURL("setup", c.id)}
		return billing.Action{
			URL: c.pending.url, ProviderObjectID: "fake_setup_" + c.id,
			ExpiresAt: f.cfg.Now().UTC().Add(24 * time.Hour),
		}, nil
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
	return f.subscribeLocked(customerID, plan, "")
}

// SubscribeIdempotent implements billing.IdempotentSubscriber. An exact
// replay returns the original action without charging again or replacing the
// interactive checkout; reusing the operation identity with different
// parameters fails closed.
func (f *Fake) SubscribeIdempotent(
	_ context.Context,
	customerID, plan, operationID string,
) (billing.Action, error) {
	if err := billing.ValidateOperationID(operationID); err != nil {
		return billing.Action{}, fmt.Errorf("fake billing: subscription: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if replay, ok := f.subscribeOps[operationID]; ok {
		if replay.customerID != customerID || replay.plan != plan {
			return billing.Action{}, fmt.Errorf(
				"fake billing: subscription operation was reused with different parameters")
		}
		return replay.action, nil
	}
	action, err := f.subscribeLocked(customerID, plan, operationID)
	if err != nil {
		return billing.Action{}, err
	}
	f.subscribeOps[operationID] = subscribeReplay{
		customerID: customerID,
		plan:       plan,
		action:     action,
	}
	return action, nil
}

// subscribeLocked performs the non-replayed mutation. Callers hold f.mu.
func (f *Fake) subscribeLocked(customerID, plan, operationID string) (billing.Action, error) {
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
		c.pending = &pending{
			kind: pendingCheckout, plan: plan, url: fakeURL("checkout", c.id),
			operationID: operationID,
		}
		return billing.Action{
			URL: c.pending.url, ProviderObjectID: "fake_checkout_" + c.id,
			ExpiresAt: f.cfg.Now().UTC().Add(24 * time.Hour),
		}, nil
	}
	f.charge(c, plan)
	return billing.Action{Done: true}, nil
}

// ScheduleDowngrade implements billing.Provider: the change is recorded and
// takes effect at the current period end (applied by ApplyDue).
func (f *Fake) ScheduleDowngrade(_ context.Context, customerID, plan string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scheduleDowngradeLocked(customerID, plan)
}

// ScheduleDowngradeIdempotent implements billing.IdempotentDowngrader. An
// exact retry returns the originally selected effective time without
// retargeting pending state; a parameter mismatch fails closed.
func (f *Fake) ScheduleDowngradeIdempotent(
	_ context.Context,
	customerID, plan, operationID string,
) (time.Time, error) {
	if err := billing.ValidateOperationID(operationID); err != nil {
		return time.Time{}, fmt.Errorf("fake billing: downgrade: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if replay, ok := f.downgradeOps[operationID]; ok {
		if replay.customerID != customerID || replay.plan != plan {
			return time.Time{}, fmt.Errorf(
				"fake billing: downgrade operation was reused with different parameters")
		}
		return replay.effective, nil
	}
	effective, err := f.scheduleDowngradeLocked(customerID, plan)
	if err != nil {
		return time.Time{}, err
	}
	if c, ok := f.byID[customerID]; ok && c.pending != nil &&
		c.pending.kind == pendingDowngrade {
		c.pending.operationID = operationID
	}
	f.downgradeOps[operationID] = downgradeReplay{
		customerID: customerID,
		plan:       plan,
		effective:  effective,
	}
	return effective, nil
}

// ScheduleDowngradeExactIdempotent returns the fake subscription identity
// alongside the stable period boundary so managed lifecycle cancellation can
// target only the schedule created by this operation.
func (f *Fake) ScheduleDowngradeExactIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.ScheduledDowngrade, error) {
	effective, err := f.ScheduleDowngradeIdempotent(
		ctx, customerID, plan, operationID)
	if err != nil {
		return billing.ScheduledDowngrade{}, err
	}
	return billing.ScheduledDowngrade{
		Effective: effective, ProviderObjectID: "fake_subscription_" + customerID,
	}, nil
}

// scheduleDowngradeLocked performs the non-replayed mutation. Callers hold
// f.mu.
func (f *Fake) scheduleDowngradeLocked(customerID, plan string) (time.Time, error) {
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
	return f.cancelPendingLocked(customerID)
}

// CancelPendingIdempotent implements billing.IdempotentPendingCanceller. An
// exact retry is a no-op even if newer pending state now exists; reusing the
// operation identity for another customer fails closed.
func (f *Fake) CancelPendingIdempotent(
	_ context.Context,
	customerID, operationID string,
) error {
	if err := billing.ValidateOperationID(operationID); err != nil {
		return fmt.Errorf("fake billing: cancel pending: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if replay, ok := f.cancelOps[operationID]; ok {
		if replay.customerID != customerID {
			return fmt.Errorf(
				"fake billing: cancel-pending operation was reused with different parameters")
		}
		return nil
	}
	if err := f.cancelPendingLocked(customerID); err != nil {
		return err
	}
	f.cancelOps[operationID] = cancelReplay{customerID: customerID}
	return nil
}

// CancelPendingObjectIdempotent disarms only the exact fake object recorded by
// the lifecycle state. A completed action is not reported as cancelled.
func (f *Fake) CancelPendingObjectIdempotent(
	_ context.Context,
	customerID string,
	target billing.PendingCancellation,
	operationID string,
) error {
	if err := billing.ValidateOperationID(operationID); err != nil {
		return fmt.Errorf("fake billing: exact cancel: %w", err)
	}
	if err := billing.ValidateProviderObjectID(target.ProviderObjectID); err != nil {
		return fmt.Errorf("fake billing: exact cancel: %w", err)
	}
	if err := billing.ValidateOperationID(target.OriginalOperationID); err != nil {
		return fmt.Errorf("fake billing: exact cancel original operation: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if replay, ok := f.cancelOps[operationID]; ok {
		if replay.customerID != customerID || replay.kind != target.Kind ||
			replay.providerObjectID != target.ProviderObjectID ||
			replay.originalOperationID != target.OriginalOperationID {
			return errors.New(
				"fake billing: exact cancel operation was reused with different parameters")
		}
		return nil
	}
	c, err := f.cust(customerID)
	if err != nil {
		return err
	}
	if c.pending == nil {
		return fmt.Errorf("fake billing: %w", billing.ErrPendingAlreadyResolved)
	}
	switch target.Kind {
	case billing.PendingCancellationHostedAction:
		if c.pending.kind != pendingCheckout ||
			target.ProviderObjectID != "fake_checkout_"+customerID ||
			c.pending.operationID != target.OriginalOperationID {
			return errors.New("fake billing: hosted cancellation target mismatch")
		}
	case billing.PendingCancellationPeriodEnd:
		if c.pending.kind != pendingDowngrade ||
			target.ProviderObjectID != "fake_subscription_"+customerID ||
			c.pending.operationID != target.OriginalOperationID {
			return errors.New("fake billing: period-end cancellation target mismatch")
		}
	default:
		return errors.New("fake billing: unsupported exact cancellation target")
	}
	c.pending = nil
	f.cancelOps[operationID] = cancelReplay{
		customerID: customerID, kind: target.Kind,
		providerObjectID:    target.ProviderObjectID,
		originalOperationID: target.OriginalOperationID,
	}
	return nil
}

// cancelPendingLocked performs the non-replayed mutation. Callers hold f.mu.
func (f *Fake) cancelPendingLocked(customerID string) error {
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
	if f.cfg.WebhookSecret != "" {
		sig := r.Header.Get("X-Witself-Fake-Signature")
		if subtle.ConstantTimeCompare([]byte(sig), []byte(f.cfg.WebhookSecret)) != 1 {
			return nil, fmt.Errorf("fake webhook: bad signature")
		}
	}
	var body struct {
		CustomerID  string `json:"customer_id"`
		Type        string `json:"type"`
		Plan        string `json:"plan"`
		OperationID string `json:"operation_id"`
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
		Type:        billing.EventType(body.Type),
		CustomerID:  body.CustomerID,
		Plan:        body.Plan,
		OperationID: body.OperationID,
		At:          f.cfg.Now(),
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
			Type: billing.EventSubscriptionActivated, CustomerID: c.id, Plan: plan,
			At: f.cfg.Now(), OperationID: p.operationID,
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
