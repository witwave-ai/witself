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
type stubRequest struct {
	method         string
	path           string
	idempotencyKey string
}

type checkoutReplay struct {
	fingerprint string
	response    string
}

type stubStripe struct {
	t                   *testing.T
	url                 string            // the stub server's base URL
	prices              map[string]string // lookup_key -> price id
	priceCents          map[string]int64  // lookup_key -> unit_amount
	created             []string          // paths of POSTs received
	lastForm            map[string]string // last POST form (flattened)
	lastVersion         string            // Stripe-Version header seen last
	lastIdem            string            // Idempotency-Key header seen last
	requests            []stubRequest     // all requests, including read headers
	checkoutSeq         int               // setup/subscription Checkout objects minted
	checkoutOps         map[string]checkoutReplay
	failNext            int    // when >0, respond with this status once
	failCode            string // error code for failNext (default "boom")
	failPath            string // when set, failNext fires only on this path
	upcoming            bool   // whether an upcoming invoice exists
	subActive           bool   // whether a live subscription exists
	subArmed            bool   // cancel_at_period_end on the stub subscription
	subscriptionsJSON   string // optional exact list response override
	custDeleted         bool   // customer GETs report deleted:true
	customerCreateKeys  []string
	customerCreateForms []map[string]string
	customerUpdateKeys  []string
	customerUpdateForms []map[string]string
	openSessions        []string // open checkout session ids
	expired             []string // session ids expired via POST .../expire
	refunded            bool     // charge has one successful partial refund
	refundMore          bool     // refund list reports unsupported pagination
	refundAmount        int      // settled amount reported by the charge
	refundData          string   // optional refund-list JSON data override
}

func TestDowngradeTargetCapabilityIsFreeOnly(t *testing.T) {
	provider := &Provider{}
	if !provider.SupportsDowngradeTarget(plans.Free) {
		t.Fatal("free downgrade target must be supported")
	}
	for _, target := range []string{"standard", "team", "enterprise", ""} {
		if provider.SupportsDowngradeTarget(target) {
			t.Fatalf("downgrade target %q unexpectedly supported", target)
		}
	}
}

func newStub(t *testing.T) (*stubStripe, *Provider) {
	t.Helper()
	s := &stubStripe{
		t: t, prices: map[string]string{}, priceCents: map[string]int64{},
		lastForm: map[string]string{}, checkoutOps: map[string]checkoutReplay{},
		subActive: true, subArmed: true, upcoming: true,
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
		Now: func() time.Time {
			return time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		},
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
	s.lastIdem = r.Header.Get("Idempotency-Key")
	s.requests = append(s.requests, stubRequest{
		method:         r.Method,
		path:           r.URL.Path,
		idempotencyKey: s.lastIdem,
	})
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
	checkoutExpires := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC).Unix()
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
		s.customerCreateKeys = append(s.customerCreateKeys, s.lastIdem)
		s.customerCreateForms = append(s.customerCreateForms, s.lastForm)
		_, _ = fmt.Fprint(w, `{"id":"cus_stub_1"}`)
	case strings.HasPrefix(r.URL.Path, "/v1/customers/") && r.Method == http.MethodGet:
		deleted := s.custDeleted
		s.custDeleted = false // one poisoned generation, then healthy
		_, _ = fmt.Fprintf(w, `{"id":"cus_stub_1","deleted":%t}`, deleted)
	case strings.HasPrefix(r.URL.Path, "/v1/customers/") && r.Method == http.MethodPost:
		s.customerUpdateKeys = append(s.customerUpdateKeys, s.lastIdem)
		s.customerUpdateForms = append(s.customerUpdateForms, s.lastForm)
		_, _ = fmt.Fprint(w, `{"id":"cus_stub_1"}`)
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
		fingerprint := r.URL.Path + "\n" + r.PostForm.Encode()
		if s.lastIdem != "" {
			if replay, ok := s.checkoutOps[s.lastIdem]; ok {
				if replay.fingerprint != fingerprint {
					http.Error(w, `{"error":{"code":"idempotency_error","message":"key reused with different parameters"}}`, http.StatusBadRequest)
					return
				}
				_, _ = fmt.Fprint(w, replay.response)
				return
			}
		}
		s.checkoutSeq++
		response := fmt.Sprintf(
			`{"id":"cs_test_stub_%d","url":"https://checkout.stripe.com/c/pay/cs_test_stub_%d","expires_at":%d}`,
			s.checkoutSeq, s.checkoutSeq, checkoutExpires,
		)
		if s.lastIdem != "" {
			s.checkoutOps[s.lastIdem] = checkoutReplay{
				fingerprint: fingerprint,
				response:    response,
			}
		}
		_, _ = fmt.Fprint(w, response)
	case r.URL.Path == "/v1/billing_portal/sessions":
		_, _ = fmt.Fprint(w, `{"url":"https://billing.stripe.com/p/session_stub"}`)
	case r.URL.Path == "/v1/subscriptions" && r.Method == http.MethodGet:
		if s.subscriptionsJSON != "" {
			_, _ = fmt.Fprint(w, s.subscriptionsJSON)
			return
		}
		if s.subActive {
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"sub_stub","status":"active","cancel_at_period_end":%t,"metadata":{"witself_plan":"standard"},"items":{"data":[{"current_period_end":%d,"price":{"id":"price_standard","lookup_key":"witself_standard"}}]}}],"has_more":false}`, s.subArmed, periodEnd)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[],"has_more":false}`)
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
		amountRefunded := 0
		if s.refunded {
			amountRefunded = s.refundAmount
			if amountRefunded == 0 {
				amountRefunded = 700
			}
		}
		_, _ = fmt.Fprintf(w, `{"data":[{"id":"ch_stub","created":1754265600,"amount":3000,"amount_refunded":%d,"currency":"usd","status":"succeeded","receipt_url":"https://receipt.stripe.com/r/x","payment_method_details":{"card":{"brand":"visa","last4":"4242"}}}]}`, amountRefunded)
	case r.URL.Path == "/v1/refunds":
		if got := r.URL.Query().Get("charge"); got != "ch_stub" {
			http.Error(w, `{"error":{"message":"wrong charge"}}`, http.StatusBadRequest)
			return
		}
		data := s.refundData
		if data == "" {
			data = `[{"created":1754269200,"amount":700,"status":"succeeded"}]`
		}
		_, _ = fmt.Fprintf(w, `{"data":%s,"has_more":%t}`, data, s.refundMore)
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

// TestEnsureCustomerEscapesStaleIdempotency covers stale create generations:
// a replayed customer was deleted (live-test-discovered), or a prior deployed
// create shape left a changed-parameter idempotency conflict. Both advance to a
// deterministic generation.
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
	t.Run("legacy create shape idempotency_error", func(t *testing.T) {
		s, p := newStub(t)
		s.failNext, s.failCode = http.StatusBadRequest, "idempotency_error"
		id, err := p.EnsureCustomer(context.Background(), "acct_1", "new@b.example")
		if err != nil || id != "cus_stub_1" {
			t.Fatalf("EnsureCustomer = %q, %v", id, err)
		}
	})
}

func TestEnsureCustomerSeparatesStableCreateFromMutableEmail(t *testing.T) {
	s, p := newStub(t)
	ctx := context.Background()

	firstEmail := "first@example.test"
	firstID, err := p.EnsureCustomer(ctx, "acct_1", firstEmail)
	if err != nil || firstID != "cus_stub_1" {
		t.Fatalf("first EnsureCustomer = %q, %v", firstID, err)
	}
	secondEmail := "rotated@example.test"
	secondID, err := p.EnsureCustomer(ctx, "acct_1", secondEmail)
	if err != nil || secondID != firstID {
		t.Fatalf("rotated EnsureCustomer = %q, %v; want customer %q", secondID, err, firstID)
	}
	if len(s.customerCreateKeys) != 2 ||
		s.customerCreateKeys[0] != "witself-ensure-acct_1" ||
		s.customerCreateKeys[1] != s.customerCreateKeys[0] {
		t.Fatalf("customer create keys = %v; want the same stable account key", s.customerCreateKeys)
	}
	for i, form := range s.customerCreateForms {
		if form["metadata[witself_account]"] != "acct_1" || form["email"] != "" {
			t.Fatalf("customer create form %d = %v; mutable email leaked into create", i, form)
		}
	}
	if len(s.customerUpdateKeys) != 2 || len(s.customerUpdateForms) != 2 {
		t.Fatalf("customer email updates = keys %v forms %v; want two", s.customerUpdateKeys, s.customerUpdateForms)
	}
	if s.customerUpdateForms[0]["email"] != firstEmail ||
		s.customerUpdateForms[1]["email"] != secondEmail {
		t.Fatalf("customer email update forms = %v", s.customerUpdateForms)
	}
	if s.customerUpdateKeys[0] == "" ||
		s.customerUpdateKeys[0] == s.customerUpdateKeys[1] {
		t.Fatalf("customer email update keys = %v; want distinct non-empty keys", s.customerUpdateKeys)
	}
	for _, key := range s.customerUpdateKeys {
		if strings.Contains(key, firstEmail) || strings.Contains(key, secondEmail) {
			t.Fatalf("customer email leaked into idempotency key %q", key)
		}
	}

	_, err = p.EnsureCustomer(ctx, "acct_1", secondEmail)
	if err != nil {
		t.Fatalf("exact rotated-email replay: %v", err)
	}
	if got := s.customerUpdateKeys[2]; got != s.customerUpdateKeys[1] {
		t.Fatalf("exact email update replay key = %q; want %q", got, s.customerUpdateKeys[1])
	}
}

func TestSubscribeBuildsCheckout(t *testing.T) {
	s, p := newStub(t)
	act, err := p.Subscribe(context.Background(), "cus_stub_1", "standard")
	if err != nil || act.Done || !strings.Contains(act.URL, "checkout.stripe.com") {
		t.Fatalf("Subscribe = %+v, %v; want a checkout URL", act, err)
	}
	if act.ProviderObjectID == "" || !act.ExpiresAt.After(p.cfg.Now()) {
		t.Fatalf("Subscribe hosted action lacks exact object/expiry: %+v", act)
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

func TestSubscribeIdempotentBindsCheckoutOperation(t *testing.T) {
	s, p := newStub(t)
	act, err := p.SubscribeIdempotent(
		context.Background(), "cus_stub_1", "standard", "bop_checkout_1")
	if err != nil || act.Done || !strings.Contains(act.URL, "checkout.stripe.com") {
		t.Fatalf("SubscribeIdempotent = %+v, %v; want a checkout URL", act, err)
	}
	if s.lastIdem != "witself-subscribe-bop_checkout_1" {
		t.Fatalf("Idempotency-Key = %q", s.lastIdem)
	}
	if s.lastForm["metadata[witself_operation_id]"] != "bop_checkout_1" ||
		s.lastForm["subscription_data[metadata][witself_operation_id]"] != "bop_checkout_1" {
		t.Fatalf("operation metadata missing from session/subscription: %v", s.lastForm)
	}
	if _, err := p.SubscribeIdempotent(
		context.Background(), "cus_stub_1", "standard", ""); err == nil {
		t.Fatal("empty subscription operation id accepted")
	}
}

func TestSetupLinkIdempotentBindsReplaysAndRejectsConflict(t *testing.T) {
	s, p := newStub(t)
	ctx := context.Background()

	first, err := p.SetupLinkIdempotent(ctx, "cus_stub_1", "bop_setup_1")
	if err != nil || first.Done || !strings.Contains(first.URL, "checkout.stripe.com") {
		t.Fatalf("first SetupLinkIdempotent = %+v, %v", first, err)
	}
	if first.ProviderObjectID == "" || !first.ExpiresAt.After(p.cfg.Now()) {
		t.Fatalf("setup hosted action lacks exact object/expiry: %+v", first)
	}
	if s.lastIdem != "witself-setup-bop_setup_1" {
		t.Fatalf("Idempotency-Key = %q", s.lastIdem)
	}
	if s.lastForm["metadata[witself_operation_id]"] != "bop_setup_1" {
		t.Fatalf("operation metadata missing from setup session: %v", s.lastForm)
	}
	second, err := p.SetupLinkIdempotent(ctx, "cus_stub_1", "bop_setup_1")
	if err != nil || second != first {
		t.Fatalf("replayed SetupLinkIdempotent = %+v, %v; want %+v", second, err, first)
	}
	if s.checkoutSeq != 1 {
		t.Fatalf("exact setup replay minted %d Checkout objects; want 1", s.checkoutSeq)
	}
	if _, err := p.SetupLinkIdempotent(ctx, "cus_other", "bop_setup_1"); err == nil {
		t.Fatal("setup operation identity reused for another customer")
	}
	if s.checkoutSeq != 1 {
		t.Fatalf("conflicting setup minted another Checkout object: %d", s.checkoutSeq)
	}
}

func TestStripeStrongOperationIDShape(t *testing.T) {
	_, p := newStub(t)
	for _, operationID := range []string{
		"",
		" leading",
		"trailing ",
		"slash/value",
		"unicode-é",
		strings.Repeat("x", 129),
	} {
		if _, err := p.SetupLinkIdempotent(
			context.Background(), "cus_stub_1", operationID,
		); err == nil {
			t.Fatalf("SetupLinkIdempotent accepted operation id %q", operationID)
		}
	}
}

func TestScheduleDowngradeIdempotentKeysMutationsOnly(t *testing.T) {
	s, p := newStub(t)
	ctx := context.Background()

	first, err := p.ScheduleDowngradeIdempotent(
		ctx, "cus_stub_1", "free", "bop_down_1",
	)
	if err != nil || first.IsZero() {
		t.Fatalf("first ScheduleDowngradeIdempotent = %v, %v", first, err)
	}
	wantKey := childIdempotencyKey("bop_down_1", "downgrade", "sub_stub")
	if wantKey == "" || len(wantKey) > 255 {
		t.Fatalf("derived downgrade key has invalid length %d: %q", len(wantKey), wantKey)
	}
	if len(s.requests) != 2 {
		t.Fatalf("downgrade requests = %+v; want one read and one mutation", s.requests)
	}
	if got := s.requests[0]; got.method != http.MethodGet || got.idempotencyKey != "" {
		t.Fatalf("downgrade discovery read carried an idempotency key: %+v", got)
	}
	if got := s.requests[1]; got.method != http.MethodPost || got.idempotencyKey != wantKey {
		t.Fatalf("downgrade mutation request = %+v; want key %q", got, wantKey)
	}

	start := len(s.requests)
	second, err := p.ScheduleDowngradeIdempotent(
		ctx, "cus_stub_1", "free", "bop_down_1",
	)
	if err != nil || !second.Equal(first) {
		t.Fatalf("replayed ScheduleDowngradeIdempotent = %v, %v; want %v", second, err, first)
	}
	if got := s.requests[start+1].idempotencyKey; got != wantKey {
		t.Fatalf("downgrade replay key = %q; want %q", got, wantKey)
	}

	bounded := childIdempotencyKey(
		strings.Repeat("x", 128), "cancel-subscription", strings.Repeat("object", 100),
	)
	if len(bounded) > 255 {
		t.Fatalf("maximal child Idempotency-Key is %d bytes; Stripe allows 255", len(bounded))
	}
}

func TestCancelPendingIdempotentKeysEveryMutationAndNoRead(t *testing.T) {
	s, p := newStub(t)
	s.openSessions = []string{"cs_stale_a", "cs_stale_b"}
	if err := p.CancelPendingIdempotent(
		context.Background(), "cus_stub_1", "bop_cancel_1",
	); err != nil {
		t.Fatalf("CancelPendingIdempotent: %v", err)
	}

	wantKeys := map[string]string{
		"/v1/checkout/sessions/cs_stale_a/expire": childIdempotencyKey(
			"bop_cancel_1", "cancel-checkout", "cs_stale_a"),
		"/v1/checkout/sessions/cs_stale_b/expire": childIdempotencyKey(
			"bop_cancel_1", "cancel-checkout", "cs_stale_b"),
		"/v1/subscriptions/sub_stub": childIdempotencyKey(
			"bop_cancel_1", "cancel-subscription", "sub_stub"),
	}
	seen := map[string]bool{}
	for _, request := range s.requests {
		switch request.method {
		case http.MethodGet:
			if request.idempotencyKey != "" {
				t.Fatalf("cancel discovery read carried key %q: %+v", request.idempotencyKey, request)
			}
		case http.MethodPost:
			want, ok := wantKeys[request.path]
			if !ok {
				t.Fatalf("unexpected cancellation mutation: %+v", request)
			}
			if request.idempotencyKey != want {
				t.Fatalf("cancellation mutation %s key = %q; want %q", request.path, request.idempotencyKey, want)
			}
			if seen[request.idempotencyKey] {
				t.Fatalf("two child mutations collided on key %q", request.idempotencyKey)
			}
			seen[request.idempotencyKey] = true
		}
	}
	if len(seen) != len(wantKeys) {
		t.Fatalf("keyed cancellation mutations = %v; want %v", seen, wantKeys)
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

func TestScheduleDowngradeValidatesProjectionBeforeMutation(t *testing.T) {
	s, p := newStub(t)
	s.subscriptionsJSON = `{"data":[{"id":"sub_bad","status":"active","metadata":{"witself_plan":"standard"},"items":{"data":[{"price":{"id":"price_standard","lookup_key":"witself_standard"}}]}}],"has_more":false}`
	if _, err := p.ScheduleDowngradeIdempotent(
		context.Background(), "cus_stub_1", plans.Free, "bop_bad_period",
	); err == nil || !strings.Contains(err.Error(), "future current period end") {
		t.Fatalf("ScheduleDowngradeIdempotent error = %v; want invalid period refusal", err)
	}
	for _, request := range s.requests {
		if request.method == http.MethodPost &&
			strings.HasPrefix(request.path, "/v1/subscriptions/") {
			t.Fatalf("invalid subscription was mutated before validation: %+v", request)
		}
	}
}

func TestManagedSubscriptionProjectionFailsClosed(t *testing.T) {
	t.Parallel()
	one := `{"id":"sub_a","status":"active","metadata":{"witself_plan":"standard"},"items":{"data":[{"current_period_end":1783123200,"price":{"id":"price_a","lookup_key":"witself_standard"}}]}}`
	tests := []struct {
		name     string
		response string
		want     string
	}{
		{name: "pagination", response: `{"data":[],"has_more":true}`, want: "more than 100"},
		{name: "multiple", response: `{"data":[` + one + `,` + strings.ReplaceAll(one, "sub_a", "sub_b") + `],"has_more":false}`, want: "2 live subscriptions"},
		{name: "missing metadata", response: `{"data":[{"id":"sub_a","status":"active","items":{"data":[{"current_period_end":1783123200,"price":{"id":"price_a","lookup_key":"witself_standard"}}]}}],"has_more":false}`, want: "witself_plan"},
		{name: "price mismatch", response: `{"data":[{"id":"sub_a","status":"active","metadata":{"witself_plan":"standard"},"items":{"data":[{"current_period_end":1783123200,"price":{"id":"price_a","lookup_key":"witself_team"}}]}}],"has_more":false}`, want: "does not match"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, p := newStub(t)
			s.subscriptionsJSON = tc.response
			if _, _, err := p.managedLiveSubscription(
				context.Background(), "cus_stub_1",
			); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("managedLiveSubscription error = %v; want %q", err, tc.want)
			}
		})
	}
}

func TestResolveEventUsesExactManagedSubscription(t *testing.T) {
	_, p := newStub(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 1, 0, 0, 0, time.UTC)

	unrelated, err := p.ResolveEvent(ctx, billing.Event{
		Type: billing.EventPaymentFailed, CustomerID: "cus_stub_1", At: at,
		SubscriptionID: "sub_unrelated",
	})
	if err != nil || unrelated != nil {
		t.Fatalf("unrelated invoice resolved = %+v, %v; want ignored", unrelated, err)
	}

	managed, err := p.ResolveEvent(ctx, billing.Event{
		Type: billing.EventPaymentFailed, CustomerID: "cus_stub_1", At: at,
		SubscriptionID: "sub_stub",
	})
	if err != nil || managed == nil || managed.SubscriptionID != "sub_stub" ||
		managed.Plan != "standard" {
		t.Fatalf("managed invoice resolved = %+v, %v", managed, err)
	}

	staleCheckout, err := p.ResolveEvent(ctx, billing.Event{
		Type: billing.EventSubscriptionActivated, CustomerID: "cus_stub_1",
		Plan: "team", At: at, SubscriptionID: "sub_old",
		OperationID: "bop_old",
	})
	if err != nil || staleCheckout == nil ||
		staleCheckout.Type != billing.EventSubscriptionActivated ||
		staleCheckout.Plan != "standard" ||
		staleCheckout.SubscriptionID != "sub_stub" ||
		staleCheckout.OperationID != "" {
		t.Fatalf("stale checkout resolved = %+v, %v; want current provider projection", staleCheckout, err)
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

func TestListPaymentsIncludesRefundsAsNegativeMovements(t *testing.T) {
	s, p := newStub(t)
	s.refunded = true

	payments, err := p.ListPayments(context.Background(), "cus_stub_1")
	if err != nil {
		t.Fatalf("ListPayments: %v", err)
	}
	if len(payments) != 2 {
		t.Fatalf("payments = %+v; want charge and refund", payments)
	}
	if payments[0].AmountCents != -700 || payments[0].Status != "refunded" ||
		payments[0].Method != "visa ****4242" {
		t.Fatalf("refund = %+v", payments[0])
	}
	if payments[1].AmountCents != 3000 || payments[1].Status != "succeeded" ||
		payments[1].ReceiptURL == "" {
		t.Fatalf("charge = %+v", payments[1])
	}
}

func TestListPaymentsFailsClosedWhenRefundHistoryExceedsPage(t *testing.T) {
	s, p := newStub(t)
	s.refunded = true
	s.refundMore = true
	if _, err := p.ListPayments(context.Background(), "cus_stub_1"); err == nil {
		t.Fatal("paginated refund history was silently truncated")
	}
}

func TestListPaymentsOmitsUnsettledRefundAttempts(t *testing.T) {
	s, p := newStub(t)
	s.refunded = true
	s.refundData = `[
		{"created":1754269200,"amount":700,"status":"succeeded"},
		{"created":1754269300,"amount":200,"status":"pending"},
		{"created":1754269400,"amount":300,"status":"requires_action"},
		{"created":1754269500,"amount":400,"status":"failed"},
		{"created":1754269600,"amount":500,"status":"canceled"}
	]`

	payments, err := p.ListPayments(context.Background(), "cus_stub_1")
	if err != nil {
		t.Fatalf("ListPayments: %v", err)
	}
	if len(payments) != 2 || payments[0].AmountCents != -700 ||
		payments[0].Status != "refunded" {
		t.Fatalf("payments = %+v; want one settled refund and the charge", payments)
	}
}

func TestListPaymentsRejectsSettledRefundTotalMismatch(t *testing.T) {
	s, p := newStub(t)
	s.refunded = true
	s.refundAmount = 800
	if _, err := p.ListPayments(context.Background(), "cus_stub_1"); err == nil {
		t.Fatal("charge/refund settled-total mismatch was accepted")
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
		lastForm: map[string]string{}, checkoutOps: map[string]checkoutReplay{},
		subActive: true, subArmed: true, upcoming: true,
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
	payload := `{"id":"evt_paid_1","type":"invoice.paid","created":1751716800,"data":{"object":{"id":"in_paid_1","customer":"cus_x","subscription":"sub_x"}}}`

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
	payload := `{"id":"evt_checkout_1","type":"checkout.session.completed","created":1751716800,"data":{"object":{"id":"cs_1","customer":"cus_x","subscription":"sub_x","mode":"subscription","payment_status":"paid","metadata":{"witself_plan":"standard","witself_operation_id":"bop_1"}}}}`
	events, err := deliver(t, p, payload, sgn(payload))
	if err != nil || len(events) != 1 || events[0].Type != billing.EventSubscriptionActivated ||
		events[0].Plan != "standard" || events[0].CustomerID != "cus_x" ||
		events[0].ProviderEventID != "evt_checkout_1" ||
		events[0].ProviderObjectID != "cs_1" ||
		events[0].SubscriptionID != "sub_x" || events[0].OperationID != "bop_1" ||
		len(events[0].PayloadSHA256) != 64 {
		t.Fatalf("checkout completed = %+v, %v", events, err)
	}
	// UNPAID completion (delayed-notification method like ACH): the session
	// completed but the money has not moved — NOTHING is entitled yet.
	payload = `{"id":"evt_checkout_2","type":"checkout.session.completed","created":1751716800,"data":{"object":{"id":"cs_2","customer":"cus_x","mode":"subscription","payment_status":"unpaid","metadata":{"witself_plan":"standard"}}}}`
	if events, err = deliver(t, p, payload, sgn(payload)); err != nil || len(events) != 0 {
		t.Fatalf("unpaid completion = %+v, %v; want empty ACK (entitle on async_payment_succeeded)", events, err)
	}
	// The async success lands later and activates.
	payload = `{"id":"evt_checkout_3","type":"checkout.session.async_payment_succeeded","created":1751716800,"data":{"object":{"id":"cs_3","customer":"cus_x","subscription":"sub_x","mode":"subscription","payment_status":"paid","metadata":{"witself_plan":"standard"}}}}`
	if events, err = deliver(t, p, payload, sgn(payload)); err != nil || len(events) != 1 || events[0].Type != billing.EventSubscriptionActivated {
		t.Fatalf("async_payment_succeeded = %+v, %v; want activation", events, err)
	}
	// Setup-mode completion: card captured, no entitlement events.
	payload = `{"id":"evt_setup_1","type":"checkout.session.completed","created":1751716800,"data":{"object":{"id":"cs_setup_1","customer":"cus_x","mode":"setup","payment_status":"no_payment_required"}}}`
	if events, err = deliver(t, p, payload, sgn(payload)); err != nil || len(events) != 0 {
		t.Fatalf("setup completed = %+v, %v; want empty ACK", events, err)
	}
	// Subscription-mode completion MISSING the plan metadata must error (the
	// entitlement would be unroutable) — never silently ACKed.
	payload = `{"id":"evt_checkout_4","type":"checkout.session.completed","created":1751716800,"data":{"object":{"id":"cs_4","customer":"cus_x","mode":"subscription","payment_status":"paid"}}}`
	if _, err = deliver(t, p, payload, sgn(payload)); err == nil {
		t.Fatal("activation without witself_plan metadata was ACKed")
	}
	// A handled event without Stripe's ordering timestamp must fail rather than
	// become an epoch-dated event that is silently treated as stale.
	payload = `{"id":"evt_checkout_no_time","type":"checkout.session.completed","data":{"object":{"id":"cs_no_time","customer":"cus_x","mode":"subscription","payment_status":"paid","metadata":{"witself_plan":"standard"}}}}`
	if _, err = deliver(t, p, payload, sgn(payload)); err == nil {
		t.Fatal("handled event without provider timestamp was ACKed")
	}
	// payment_failed maps to its event.
	payload = `{"id":"evt_invoice_failed_1","type":"invoice.payment_failed","created":1751716800,"data":{"object":{"id":"in_failed_1","customer":"cus_x","subscription":"sub_x"}}}`
	if events, _ = deliver(t, p, payload, sgn(payload)); len(events) != 1 ||
		events[0].Type != billing.EventPaymentFailed ||
		events[0].ProviderObjectID != "in_failed_1" ||
		events[0].SubscriptionID != "sub_x" {
		t.Fatalf("payment_failed = %+v", events)
	}
	// subscription.deleted while ANOTHER live subscription remains is
	// normalized first, then projected back to that exact managed survivor by
	// the post-receipt resolver. A duplicate/stale subscription's deletion must
	// not revoke a live paid entitlement, and no Stripe read happens in
	// HandleWebhook.
	payload = `{"id":"evt_subscription_deleted_1","type":"customer.subscription.deleted","created":1751716800,"data":{"object":{"id":"sub_deleted","customer":"cus_x"}}}`
	if events, err = deliver(t, p, payload, sgn(payload)); err != nil || len(events) != 1 {
		t.Fatalf("normalize deleted subscription = %+v, %v", events, err)
	}
	resolved, err := p.ResolveEvent(context.Background(), events[0])
	if err != nil || resolved == nil ||
		resolved.Type != billing.EventSubscriptionActivated ||
		resolved.Plan != "standard" || resolved.SubscriptionID != "sub_stub" {
		t.Fatalf("deleted with survivor resolved = %+v, %v; want survivor projection", resolved, err)
	}
	// With no live subscription left, the cancel folds.
	s.subActive = false
	resolved, err = p.ResolveEvent(context.Background(), events[0])
	if err != nil || resolved == nil ||
		resolved.Type != billing.EventSubscriptionCanceled ||
		resolved.SubscriptionID != "" {
		t.Fatalf("subscription.deleted resolved = %+v, %v", resolved, err)
	}
	// And an API failure during the post-receipt survivor check must ERROR and
	// leave the durable receipt pending, never emit a possibly-wrong cancel.
	s.failNext = http.StatusInternalServerError
	if _, err = p.ResolveEvent(context.Background(), events[0]); err == nil {
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
