package fake

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
)

// prices mirror the live plan catalog; "free" is deliberately absent — free is
// the zero value of billing, not a subscription.
var prices = map[string]int64{"standard": 3000, "team": 25000}

// clock is a controllable time source for period-end tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newFake(t *testing.T, interactive bool) (*Fake, *clock, string) {
	t.Helper()
	ck := &clock{t: time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)}
	f := New(Config{Prices: prices, Interactive: interactive, Now: ck.now})
	id, err := f.EnsureCustomer(context.Background(), "acct_1", "scott@example.com")
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	return f, ck, id
}

func TestEnsureCustomerIdempotent(t *testing.T) {
	f, _, id := newFake(t, false)
	again, err := f.EnsureCustomer(context.Background(), "acct_1", "scott@example.com")
	if err != nil {
		t.Fatalf("EnsureCustomer again: %v", err)
	}
	if again != id {
		t.Fatalf("EnsureCustomer not idempotent: %q then %q", id, again)
	}
	other, _ := f.EnsureCustomer(context.Background(), "acct_2", "x@example.com")
	if other == id {
		t.Fatalf("distinct accounts got the same customer %q", id)
	}
}

func TestHeadlessSubscribe(t *testing.T) {
	f, ck, id := newFake(t, false)
	ctx := context.Background()

	act, err := f.Subscribe(ctx, id, "standard")
	if err != nil || !act.Done {
		t.Fatalf("Subscribe = %+v, %v; want Done", act, err)
	}
	inv, _ := f.ListInvoices(ctx, id)
	if len(inv) != 1 || inv[0].Status != "paid" || inv[0].AmountCents != 3000 {
		t.Fatalf("invoices = %+v; want one paid $30.00", inv)
	}
	pay, _ := f.ListPayments(ctx, id)
	if len(pay) != 1 || pay[0].Status != "succeeded" {
		t.Fatalf("payments = %+v; want one succeeded", pay)
	}
	next, _ := f.NextCharge(ctx, id)
	if next == nil || next.AmountCents != 3000 || !next.Date.Equal(ck.t.AddDate(0, 0, 30)) {
		t.Fatalf("NextCharge = %+v; want $30.00 at period end", next)
	}
}

func TestInteractiveCheckoutThenHeadlessUpgrade(t *testing.T) {
	f, _, id := newFake(t, true)
	ctx := context.Background()

	// First purchase: no card on file -> browser hoop.
	act, err := f.Subscribe(ctx, id, "standard")
	if err != nil || act.Done || !strings.Contains(act.URL, "checkout") {
		t.Fatalf("first Subscribe = %+v, %v; want checkout URL", act, err)
	}
	if inv, _ := f.ListInvoices(ctx, id); len(inv) != 0 {
		t.Fatalf("abandoned checkout must not invoice; got %+v", inv)
	}

	// The payer returns from the browser.
	events, err := f.Complete(id)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(events) != 1 || events[0].Type != billing.EventSubscriptionActivated || events[0].Plan != "standard" {
		t.Fatalf("Complete events = %+v; want one activation for standard", events)
	}
	if pm, _ := f.PaymentMethodOnFile(ctx, id); pm == nil {
		t.Fatal("checkout should leave a payment method on file")
	}

	// Card now on file: the next change is headless even in interactive mode.
	act, err = f.Subscribe(ctx, id, "team")
	if err != nil || !act.Done {
		t.Fatalf("upgrade with card on file = %+v, %v; want Done", act, err)
	}
	if inv, _ := f.ListInvoices(ctx, id); len(inv) != 2 {
		t.Fatalf("want 2 invoices after 2 charges, got %d", len(inv))
	}
}

func TestScheduledDowngrade(t *testing.T) {
	f, ck, id := newFake(t, false)
	ctx := context.Background()
	if _, err := f.Subscribe(ctx, id, "team"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	periodEnd := ck.t.AddDate(0, 0, 30)

	// Downgrade to a cheaper paid plan: next charge previews the target.
	eff, err := f.ScheduleDowngrade(ctx, id, "standard")
	if err != nil || !eff.Equal(periodEnd) {
		t.Fatalf("ScheduleDowngrade = %v, %v; want period end %v", eff, err, periodEnd)
	}
	next, _ := f.NextCharge(ctx, id)
	if next == nil || next.AmountCents != 3000 {
		t.Fatalf("NextCharge during paid downgrade = %+v; want target price", next)
	}

	// Retarget to free: nothing upcoming.
	if _, err := f.ScheduleDowngrade(ctx, id, "free"); err != nil {
		t.Fatalf("ScheduleDowngrade free: %v", err)
	}
	if next, _ := f.NextCharge(ctx, id); next != nil {
		t.Fatalf("NextCharge with free downgrade = %+v; want nil", next)
	}

	// Cancel restores the status quo.
	if err := f.CancelPending(ctx, id); err != nil {
		t.Fatalf("CancelPending: %v", err)
	}
	if next, _ := f.NextCharge(ctx, id); next == nil || next.AmountCents != 25000 {
		t.Fatalf("NextCharge after cancel = %+v; want current plan's renewal", next)
	}
}

func TestApplyDue(t *testing.T) {
	f, ck, id := newFake(t, false)
	ctx := context.Background()
	if _, err := f.Subscribe(ctx, id, "standard"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := f.ScheduleDowngrade(ctx, id, "free"); err != nil {
		t.Fatalf("ScheduleDowngrade: %v", err)
	}

	if ev := f.ApplyDue(); len(ev) != 0 {
		t.Fatalf("ApplyDue before period end = %+v; want none", ev)
	}

	ck.t = ck.t.AddDate(0, 0, 31) // the period ends
	ev := f.ApplyDue()
	if len(ev) != 1 || ev[0].Type != billing.EventSubscriptionCanceled {
		t.Fatalf("ApplyDue = %+v; want one cancellation", ev)
	}
	if next, _ := f.NextCharge(ctx, id); next != nil {
		t.Fatalf("NextCharge after downgrade to free = %+v; want nil", next)
	}
	if ev := f.ApplyDue(); len(ev) != 0 {
		t.Fatalf("ApplyDue must be one-shot; got %+v", ev)
	}
}

func TestRecordUsageIdempotency(t *testing.T) {
	f, _, id := newFake(t, false)
	ctx := context.Background()
	for range 3 {
		if err := f.RecordUsage(ctx, id, "tokens", 500, "cursor-42"); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}
	if got := f.UsageTotal(id, "tokens"); got != 500 {
		t.Fatalf("usage after 3 deliveries of one key = %d; want 500", got)
	}
	if err := f.RecordUsage(ctx, id, "tokens", 500, "cursor-43"); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if got := f.UsageTotal(id, "tokens"); got != 1000 {
		t.Fatalf("usage after a new key = %d; want 1000", got)
	}
}

func TestHandleWebhook(t *testing.T) {
	f, _, id := newFake(t, false)

	r := httptest.NewRequest("POST", "/v1/billing/webhook",
		strings.NewReader(`{"customer_id":"`+id+`","type":"payment_failed"}`))
	events, err := f.HandleWebhook(r)
	if err != nil || len(events) != 1 || events[0].Type != billing.EventPaymentFailed {
		t.Fatalf("HandleWebhook = %+v, %v; want one payment_failed", events, err)
	}

	r = httptest.NewRequest("POST", "/v1/billing/webhook", strings.NewReader(`{"type":"x"}`))
	if _, err := f.HandleWebhook(r); err == nil {
		t.Fatal("HandleWebhook without customer_id should error")
	}

	// Un-normalized types must be rejected, not passed through: the four
	// EventType constants are the only facts a Provider may emit.
	r = httptest.NewRequest("POST", "/v1/billing/webhook",
		strings.NewReader(`{"customer_id":"`+id+`","type":"invoice.paid"}`))
	if _, err := f.HandleWebhook(r); err == nil {
		t.Fatal("HandleWebhook should reject event types outside the normalized four")
	}
}

func TestErrors(t *testing.T) {
	f, _, id := newFake(t, false)
	ctx := context.Background()

	if _, err := f.Subscribe(ctx, id, "free"); err == nil {
		t.Fatal("subscribing to an unpriced plan should error")
	}
	if _, err := f.Subscribe(ctx, "fake_cus_9999", "standard"); err == nil {
		t.Fatal("unknown customer should error")
	}
	if _, err := f.ScheduleDowngrade(ctx, id, "free"); err == nil {
		t.Fatal("downgrade without a subscription should error")
	}
	if _, err := f.Complete(id); err == nil {
		t.Fatal("Complete with nothing pending should error")
	}

	// A typo'd downgrade target must error, not silently become free — that
	// would cancel a paid subscription at period end. And a pricier target is
	// not a downgrade.
	if _, err := f.Subscribe(ctx, id, "standard"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := f.ScheduleDowngrade(ctx, id, "standrad"); err == nil {
		t.Fatal("downgrade to an unknown plan should error, not silently mean free")
	}
	if _, err := f.ScheduleDowngrade(ctx, id, "team"); err == nil {
		t.Fatal("downgrade to a pricier plan should error")
	}
}

// TestWebhookSecret: with a secret configured, unsigned webhook deliveries
// are rejected — the fake's stand-in for real signature verification, so a
// publicly mounted route cannot accept forged entitlement events.
func TestWebhookSecret(t *testing.T) {
	f := New(Config{Prices: prices, WebhookSecret: "s3cret"})
	id, _ := f.EnsureCustomer(context.Background(), "acct_1", "s@example.com")

	body := `{"customer_id":"` + id + `","type":"payment_failed"}`
	r := httptest.NewRequest("POST", "/v1/billing/webhook", strings.NewReader(body))
	if _, err := f.HandleWebhook(r); err == nil {
		t.Fatal("unsigned webhook must be rejected when a secret is set")
	}
	r = httptest.NewRequest("POST", "/v1/billing/webhook", strings.NewReader(body))
	r.Header.Set("X-Witself-Fake-Signature", "s3cret")
	if _, err := f.HandleWebhook(r); err != nil {
		t.Fatalf("signed webhook rejected: %v", err)
	}
}
