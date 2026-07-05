package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/plans"
)

// stubStripe fakes the handful of Stripe endpoints the provider calls,
// recording requests for assertions. Response shapes follow the pinned
// apiVersion (Basil): current_period_end on subscription items.
type stubStripe struct {
	t            *testing.T
	url          string            // the stub server's base URL
	prices       map[string]string // lookup_key -> price id
	priceCents   map[string]int64  // lookup_key -> unit_amount
	created      []string          // paths of POSTs received
	lastForm     map[string]string // last POST form (flattened)
	lastVersion  string            // Stripe-Version header seen last
	failNext     int               // when >0, respond with this status once
	failCode     string            // error code for failNext (default "boom")
	failPath     string            // when set, failNext fires only on this path
	upcoming     bool              // whether an upcoming invoice exists
	subActive    bool              // whether a live subscription exists
	subArmed     bool              // cancel_at_period_end on the stub subscription
	custDeleted  bool              // customer GETs report deleted:true
	openSessions []string          // open checkout session ids
	expired      []string          // session ids expired via POST .../expire
}

func newStub(t *testing.T) (*stubStripe, *Provider) {
	t.Helper()
	s := &stubStripe{
		t: t, prices: map[string]string{}, priceCents: map[string]int64{},
		lastForm: map[string]string{}, subActive: true, subArmed: true, upcoming: true,
	}
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	p, err := New(Config{
		SecretKey: "sk_test_stub", WebhookSecret: "whsec_stub",
		Catalog: catalog, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, p
}

func (s *stubStripe) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer sk_test_stub" {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
		return
	}
	s.lastVersion = r.Header.Get("Stripe-Version")
	if s.failNext > 0 && (s.failPath == "" || s.failPath == r.URL.Path) {
		status := s.failNext
		s.failNext = 0
		code := s.failCode
		s.failCode, s.failPath = "", ""
		if code == "" {
			code = "boom"
		}
		http.Error(w, fmt.Sprintf(`{"error":{"code":%q,"message":"induced"}}`, code), status)
		return
	}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		s.created = append(s.created, r.URL.Path)
		s.lastForm = map[string]string{}
		for k, v := range r.PostForm {
			s.lastForm[k] = v[0]
		}
	}
	periodEnd := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC).Unix()
	switch {
	case r.URL.Path == "/v1/prices" && r.Method == http.MethodGet:
		key := r.URL.Query().Get("lookup_keys[]")
		if id, ok := s.prices[key]; ok {
			_, _ = fmt.Fprintf(w, `{"data":[{"id":%q,"unit_amount":%d,"currency":"usd","product":"prod_stub"}]}`, id, s.priceCents[key])
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	case r.URL.Path == "/v1/products":
		_, _ = fmt.Fprint(w, `{"id":"prod_stub"}`)
	case r.URL.Path == "/v1/prices":
		key := s.lastForm["lookup_key"]
		s.prices[key] = "price_" + key
		s.priceCents[key], _ = strconv.ParseInt(s.lastForm["unit_amount"], 10, 64)
		_, _ = fmt.Fprintf(w, `{"id":%q}`, "price_"+key)
	case r.URL.Path == "/v1/customers" && r.Method == http.MethodPost:
		_, _ = fmt.Fprint(w, `{"id":"cus_stub_1"}`)
	case strings.HasPrefix(r.URL.Path, "/v1/customers/") && r.Method == http.MethodGet:
		deleted := s.custDeleted
		s.custDeleted = false // one poisoned generation, then healthy
		_, _ = fmt.Fprintf(w, `{"id":"cus_stub_1","deleted":%t}`, deleted)
	case strings.HasSuffix(r.URL.Path, "/expire"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/checkout/sessions/"), "/expire")
		s.expired = append(s.expired, id)
		_, _ = fmt.Fprintf(w, `{"id":%q,"status":"expired"}`, id)
	case r.URL.Path == "/v1/checkout/sessions" && r.Method == http.MethodGet:
		items := make([]string, 0, len(s.openSessions))
		for _, id := range s.openSessions {
			items = append(items, fmt.Sprintf(`{"id":%q,"mode":"subscription"}`, id))
		}
		_, _ = fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(items, ","))
	case r.URL.Path == "/v1/checkout/sessions":
		_, _ = fmt.Fprint(w, `{"url":"https://checkout.stripe.com/c/pay/cs_test_stub"}`)
	case r.URL.Path == "/v1/billing_portal/sessions":
		_, _ = fmt.Fprint(w, `{"url":"https://billing.stripe.com/p/session_stub"}`)
	case r.URL.Path == "/v1/subscriptions" && r.Method == http.MethodGet:
		if s.subActive {
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"sub_stub","status":"active","cancel_at_period_end":%t,"items":{"data":[{"current_period_end":%d}]}}]}`, s.subArmed, periodEnd)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	case strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
		_, _ = fmt.Fprint(w, `{"id":"sub_stub"}`)
	case r.URL.Path == "/v1/invoices/create_preview":
		if !s.upcoming {
			http.Error(w, `{"error":{"code":"invoice_upcoming_none","message":"none"}}`, http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(w, `{"amount_due":3000,"currency":"usd","next_payment_attempt":%d}`, periodEnd)
	case r.URL.Path == "/v1/invoices":
		_, _ = fmt.Fprint(w, `{"data":[{"number":"INV-0001","created":1754265600,"total":3000,"currency":"usd","status":"paid","invoice_pdf":"https://files.stripe.com/inv.pdf","hosted_invoice_url":"https://invoice.stripe.com/i/x"}]}`)
	case r.URL.Path == "/v1/charges":
		_, _ = fmt.Fprint(w, `{"data":[{"created":1754265600,"amount":3000,"currency":"usd","status":"succeeded","receipt_url":"https://receipt.stripe.com/r/x","payment_method_details":{"card":{"brand":"visa","last4":"4242"}}}]}`)
	case r.URL.Path == "/v1/payment_methods":
		_, _ = fmt.Fprint(w, `{"data":[{"card":{"brand":"visa","last4":"4242"}}]}`)
	default:
		http.NotFound(w, r)
	}
}

func TestEnsurePricesBootstrapsMissing(t *testing.T) {
	s, p := newStub(t)
	if err := p.EnsurePrices(context.Background()); err != nil {
		t.Fatalf("EnsurePrices: %v", err)
	}
	// Only standard is purchasable today: one product + one price created.
	joined := strings.Join(s.created, ",")
	if !strings.Contains(joined, "/v1/products") || !strings.Contains(joined, "/v1/prices") {
		t.Fatalf("expected product+price creation, got %v", s.created)
	}
	if s.prices["witself_standard"] == "" {
		t.Fatalf("price for witself_standard not created: %v", s.prices)
	}
	// Second run: resolves from lookup (cache) — no new creations.
	before := len(s.created)
	if err := p.EnsurePrices(context.Background()); err != nil {
		t.Fatalf("EnsurePrices again: %v", err)
	}
	if len(s.created) != before {
		t.Fatalf("second EnsurePrices created more objects: %v", s.created[before:])
	}
	// A FRESH provider (new process) resolving against the same Stripe state
	// matches the existing price by lookup_key — still no new creations.
	catalog, _ := plans.Load()
	p2, err := New(Config{SecretKey: "sk_test_stub", Catalog: catalog, BaseURL: s.url})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p2.EnsurePrices(context.Background()); err != nil {
		t.Fatalf("EnsurePrices fresh provider: %v", err)
	}
	if len(s.created) != before {
		t.Fatalf("fresh provider recreated objects: %v", s.created[before:])
	}
}

// TestPriceChangePropagates pins the review's finding: a resolved lookup_key
// whose unit_amount no longer matches the catalog must mint a REPLACEMENT
// price (lookup_key transferred) — otherwise a plans.json price change
// silently keeps charging the old amount forever.
func TestPriceChangePropagates(t *testing.T) {
	s, p := newStub(t)
	s.prices["witself_standard"] = "price_old"
	s.priceCents["witself_standard"] = 999 // stale amount at Stripe

	id, err := p.priceID(context.Background(), "standard")
	if err != nil {
		t.Fatalf("priceID: %v", err)
	}
	if id == "price_old" {
		t.Fatal("stale price returned — catalog price change did not propagate")
	}
	if s.lastForm["unit_amount"] != "3000" || s.lastForm["transfer_lookup_key"] != "true" {
		t.Fatalf("replacement price form = %v; want unit_amount=3000 with transfer_lookup_key", s.lastForm)
	}
	// The product is reused, not duplicated.
	if strings.Contains(strings.Join(s.created, ","), "/v1/products") {
		t.Fatalf("price change created a new product: %v", s.created)
	}
}

// TestPinsAPIVersion pins the review's critical finding: the shapes read
// here are Basil-era (item-level current_period_end, create_preview), so
// every request must carry the pinned Stripe-Version rather than floating
// with the account's default.
func TestPinsAPIVersion(t *testing.T) {
	s, p := newStub(t)
	if _, err := p.priceID(context.Background(), "standard"); err != nil {
		t.Fatalf("priceID: %v", err)
	}
	if s.lastVersion != apiVersion {
		t.Fatalf("Stripe-Version = %q; want %q pinned on every request", s.lastVersion, apiVersion)
	}
}

// TestEnsureCustomerEscapesStaleIdempotency covers the two ways the stable
// Idempotency-Key's 24h replay window goes stale (both live-test- or
// review-discovered): the replayed customer was deleted, and the params
// changed (idempotency_error). Both must fall through to a salted create.
func TestEnsureCustomerEscapesStaleIdempotency(t *testing.T) {
	t.Run("deleted replay", func(t *testing.T) {
		s, p := newStub(t)
		s.custDeleted = true // the replayed customer no longer exists
		id, err := p.EnsureCustomer(context.Background(), "acct_1", "a@b.example")
		if err != nil || id != "cus_stub_1" {
			t.Fatalf("EnsureCustomer = %q, %v", id, err)
		}
		creates := 0
		for _, path := range s.created {
			if path == "/v1/customers" {
				creates++
			}
		}
		if creates != 2 {
			t.Fatalf("customer creates = %d; want 2 (stable-key replay, then generation g1)", creates)
		}
	})
	t.Run("idempotency_error on changed params", func(t *testing.T) {
		s, p := newStub(t)
		s.failNext, s.failCode = http.StatusBadRequest, "idempotency_error"
		id, err := p.EnsureCustomer(context.Background(), "acct_1", "new@b.example")
		if err != nil || id != "cus_stub_1" {
			t.Fatalf("EnsureCustomer = %q, %v", id, err)
		}
	})
}

func TestSubscribeBuildsCheckout(t *testing.T) {
	s, p := newStub(t)
	act, err := p.Subscribe(context.Background(), "cus_stub_1", "standard")
	if err != nil || act.Done || !strings.Contains(act.URL, "checkout.stripe.com") {
		t.Fatalf("Subscribe = %+v, %v; want a checkout URL", act, err)
	}
	if s.lastForm["mode"] != "subscription" || s.lastForm["customer"] != "cus_stub_1" {
		t.Fatalf("checkout form = %v", s.lastForm)
	}
	if s.lastForm["metadata[witself_plan]"] != "standard" {
		t.Fatalf("witself_plan metadata missing: %v — the activation webhook depends on it", s.lastForm)
	}
	if s.lastForm["line_items[0][price]"] != "price_witself_standard" {
		t.Fatalf("price resolved wrong: %v", s.lastForm)
	}
}

func TestScheduleDowngradeFreeOnly(t *testing.T) {
	s, p := newStub(t)
	eff, err := p.ScheduleDowngrade(context.Background(), "cus_stub_1", "free")
	if err != nil {
		t.Fatalf("ScheduleDowngrade: %v", err)
	}
	if eff.IsZero() {
		t.Fatal("effective time missing — current_period_end must be read from subscription ITEMS (Basil)")
	}
	if s.lastForm["cancel_at_period_end"] != "true" {
		t.Fatalf("expected cancel_at_period_end=true, got %v", s.lastForm)
	}
	// Paid-to-paid: refused clearly until subscription schedules land.
	if _, err := p.ScheduleDowngrade(context.Background(), "cus_stub_1", "standard"); err == nil {
		t.Fatal("paid-to-paid downgrade should be refused for now")
	}
}

func TestCancelPendingReleasesDowngrade(t *testing.T) {
	s, p := newStub(t)
	s.openSessions = []string{"cs_stale_tab"}
	if err := p.CancelPending(context.Background(), "cus_stub_1"); err != nil {
		t.Fatalf("CancelPending: %v", err)
	}
	if s.lastForm["cancel_at_period_end"] != "false" {
		t.Fatalf("expected cancel_at_period_end=false, got %v", s.lastForm)
	}
	// The open checkout session was expired — a replaced upgrade must not be
	// payable later from a stale tab (double-subscription defense).
	if len(s.expired) != 1 || s.expired[0] != "cs_stale_tab" {
		t.Fatalf("expired sessions = %v; want [cs_stale_tab]", s.expired)
	}
	// No subscription at all: nothing to release, no error.
	s.subActive = false
	s.openSessions = nil
	if err := p.CancelPending(context.Background(), "cus_stub_1"); err != nil {
		t.Fatalf("CancelPending without subscription: %v", err)
	}
}

// TestCancelPendingPropagatesErrors pins the review's critical finding: an
// API failure during CancelPending must PROPAGATE, not read as "nothing
// pending" — the Manager clears its local pending only after the provider
// disarm succeeds, and swallowing the error left downgrades armed at Stripe
// after the user was told the cancel took.
func TestCancelPendingPropagatesErrors(t *testing.T) {
	s, p := newStub(t)
	s.failNext = http.StatusInternalServerError
	if err := p.CancelPending(context.Background(), "cus_stub_1"); err == nil {
		t.Fatal("API failure swallowed — downgrade stays armed at Stripe while the Manager clears its pending")
	}
}

func TestReadPath(t *testing.T) {
	_, p := newStub(t)
	ctx := context.Background()

	pm, err := p.PaymentMethodOnFile(ctx, "cus_stub_1")
	if err != nil || pm == nil || pm.Label != "visa ****4242" {
		t.Fatalf("PaymentMethodOnFile = %+v, %v", pm, err)
	}
	inv, err := p.ListInvoices(ctx, "cus_stub_1")
	if err != nil || len(inv) != 1 || inv[0].AmountCents != 3000 || inv[0].PDFURL == "" {
		t.Fatalf("ListInvoices = %+v, %v", inv, err)
	}
	pay, err := p.ListPayments(ctx, "cus_stub_1")
	if err != nil || len(pay) != 1 || pay[0].Method != "visa ****4242" {
		t.Fatalf("ListPayments = %+v, %v", pay, err)
	}
	next, err := p.NextCharge(ctx, "cus_stub_1")
	if err != nil || next == nil || next.AmountCents != 3000 {
		t.Fatalf("NextCharge = %+v, %v", next, err)
	}
}

func TestNextChargeNoneIsNil(t *testing.T) {
	s, p := newStub(t)
	// No live subscription: nil without even previewing (create_preview
	// refuses a bare customer — live-verified).
	s.subActive = false
	next, err := p.NextCharge(context.Background(), "cus_stub_1")
	if err != nil || next != nil {
		t.Fatalf("NextCharge without subscription = %+v, %v; want nil, nil", next, err)
	}
	// Subscription ending at period end: invoice_upcoming_none -> nil.
	s.subActive = true
	s.upcoming = false
	next, err = p.NextCharge(context.Background(), "cus_stub_1")
	if err != nil || next != nil {
		t.Fatalf("NextCharge ending = %+v, %v; want nil, nil", next, err)
	}
	// A bare 404 from the preview (endpoint-shape regression) must stay an
	// ERROR — only the invoice_upcoming_none code means "no upcoming charge".
	s.failNext, s.failPath = http.StatusNotFound, "/v1/invoices/create_preview"
	if _, err := p.NextCharge(context.Background(), "cus_stub_1"); err == nil {
		t.Fatal("bare 404 masqueraded as no-upcoming-charge")
	}
}

// --- webhook signature + event normalization ---

// sign produces a valid Stripe-Signature header for payload at ts.
func sign(secret string, ts int64, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.", ts)
	mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// webhookStub builds a provider on a stub server (subscription.deleted
// verifies remaining subscriptions via the API) with a fixed clock.
func webhookStub(t *testing.T, now time.Time) (*stubStripe, *Provider) {
	t.Helper()
	s := &stubStripe{
		t: t, prices: map[string]string{}, priceCents: map[string]int64{},
		lastForm: map[string]string{}, subActive: true, subArmed: true, upcoming: true,
	}
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	p, err := New(Config{
		SecretKey: "sk_test_stub", WebhookSecret: "whsec_secret",
		Catalog: catalog, BaseURL: srv.URL, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, p
}

func deliver(t *testing.T, p *Provider, payload, sigHeader string) ([]billing.Event, error) {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/billing/webhook/stripe", strings.NewReader(payload))
	if sigHeader != "" {
		r.Header.Set("Stripe-Signature", sigHeader)
	}
	return p.HandleWebhook(r)
}

func TestWebhookSignatureVerification(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	_, p := webhookStub(t, now)
	payload := `{"type":"invoice.paid","created":1751716800,"data":{"object":{"customer":"cus_x"}}}`

	// Valid signature folds.
	events, err := deliver(t, p, payload, sign("whsec_secret", now.Unix(), []byte(payload)))
	if err != nil || len(events) != 1 || events[0].Type != billing.EventPaymentRecovered {
		t.Fatalf("valid signature = %+v, %v", events, err)
	}
	// Missing header refused.
	if _, err := deliver(t, p, payload, ""); err == nil {
		t.Fatal("missing signature accepted")
	}
	// Wrong secret refused.
	if _, err := deliver(t, p, payload, sign("whsec_WRONG", now.Unix(), []byte(payload))); err == nil {
		t.Fatal("forged signature accepted")
	}
	// Stale timestamp refused (replay defense).
	old := now.Add(-10 * time.Minute).Unix()
	if _, err := deliver(t, p, payload, sign("whsec_secret", old, []byte(payload))); err == nil {
		t.Fatal("stale signature accepted — replay window open")
	}
	// Tampered payload refused (signature covers the body).
	tampered := strings.Replace(payload, "cus_x", "cus_evil", 1)
	if _, err := deliver(t, p, tampered, sign("whsec_secret", now.Unix(), []byte(payload))); err == nil {
		t.Fatal("tampered payload accepted")
	}
}

func TestWebhookEventNormalization(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	s, p := webhookStub(t, now)
	sgn := func(payload string) string { return sign("whsec_secret", now.Unix(), []byte(payload)) }

	// checkout.session.completed (subscription mode, PAID) -> activated.
	payload := `{"type":"checkout.session.completed","created":1751716800,"data":{"object":{"customer":"cus_x","mode":"subscription","payment_status":"paid","metadata":{"witself_plan":"standard"}}}}`
	events, err := deliver(t, p, payload, sgn(payload))
	if err != nil || len(events) != 1 || events[0].Type != billing.EventSubscriptionActivated ||
		events[0].Plan != "standard" || events[0].CustomerID != "cus_x" {
		t.Fatalf("checkout completed = %+v, %v", events, err)
	}
	// UNPAID completion (delayed-notification method like ACH): the session
	// completed but the money has not moved — NOTHING is entitled yet.
	payload = `{"type":"checkout.session.completed","created":1751716800,"data":{"object":{"customer":"cus_x","mode":"subscription","payment_status":"unpaid","metadata":{"witself_plan":"standard"}}}}`
	if events, err = deliver(t, p, payload, sgn(payload)); err != nil || len(events) != 0 {
		t.Fatalf("unpaid completion = %+v, %v; want empty ACK (entitle on async_payment_succeeded)", events, err)
	}
	// The async success lands later and activates.
	payload = `{"type":"checkout.session.async_payment_succeeded","created":1751716800,"data":{"object":{"customer":"cus_x","mode":"subscription","payment_status":"paid","metadata":{"witself_plan":"standard"}}}}`
	if events, err = deliver(t, p, payload, sgn(payload)); err != nil || len(events) != 1 || events[0].Type != billing.EventSubscriptionActivated {
		t.Fatalf("async_payment_succeeded = %+v, %v; want activation", events, err)
	}
	// Setup-mode completion: card captured, no entitlement events.
	payload = `{"type":"checkout.session.completed","created":1751716800,"data":{"object":{"customer":"cus_x","mode":"setup","payment_status":"no_payment_required"}}}`
	if events, err = deliver(t, p, payload, sgn(payload)); err != nil || len(events) != 0 {
		t.Fatalf("setup completed = %+v, %v; want empty ACK", events, err)
	}
	// Subscription-mode completion MISSING the plan metadata must error (the
	// entitlement would be unroutable) — never silently ACKed.
	payload = `{"type":"checkout.session.completed","created":1751716800,"data":{"object":{"customer":"cus_x","mode":"subscription","payment_status":"paid"}}}`
	if _, err = deliver(t, p, payload, sgn(payload)); err == nil {
		t.Fatal("activation without witself_plan metadata was ACKed")
	}
	// payment_failed maps to its event.
	payload = `{"type":"invoice.payment_failed","created":1751716800,"data":{"object":{"customer":"cus_x"}}}`
	if events, _ = deliver(t, p, payload, sgn(payload)); len(events) != 1 || events[0].Type != billing.EventPaymentFailed {
		t.Fatalf("payment_failed = %+v", events)
	}
	// subscription.deleted while ANOTHER live subscription remains is
	// suppressed — a duplicate/stale subscription's deletion must not revoke
	// a live paid entitlement.
	payload = `{"type":"customer.subscription.deleted","created":1751716800,"data":{"object":{"customer":"cus_x"}}}`
	if events, err = deliver(t, p, payload, sgn(payload)); err != nil || len(events) != 0 {
		t.Fatalf("deleted with survivor = %+v, %v; want suppressed", events, err)
	}
	// With no live subscription left, the cancel folds.
	s.subActive = false
	if events, _ = deliver(t, p, payload, sgn(payload)); len(events) != 1 || events[0].Type != billing.EventSubscriptionCanceled {
		t.Fatalf("subscription.deleted = %+v", events)
	}
	// And an API failure during the survivor check must ERROR (Stripe
	// retries), never silently ACK or emit a possibly-wrong cancel.
	s.failNext = http.StatusInternalServerError
	if _, err = deliver(t, p, payload, sgn(payload)); err == nil {
		t.Fatal("survivor-check failure was ACKed")
	}
	// Unhandled types ACK with an empty batch.
	payload = `{"type":"charge.refunded","created":1751716800,"data":{"object":{"customer":"cus_x"}}}`
	if events, err = deliver(t, p, payload, sgn(payload)); err != nil || len(events) != 0 {
		t.Fatalf("unhandled type = %+v, %v; want empty ACK", events, err)
	}
}

func TestWebhookWithoutSecretRefusesAll(t *testing.T) {
	catalog, _ := plans.Load()
	p, err := New(Config{SecretKey: "sk_test_stub", Catalog: catalog})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := deliver(t, p, `{}`, "t=1,v1=x"); err == nil {
		t.Fatal("webhook without a configured secret must refuse everything")
	}
}
