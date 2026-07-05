package cpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/plans"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type noopApplier struct{}

func (noopApplier) Apply(context.Context, string, string, map[string]int64, []string) error {
	return nil
}

type harness struct {
	srv  *httptest.Server
	fake *fake.Fake
	ck   *clock
}

// newHarness builds the full CP HTTP stack over MemStore + the interactive
// fake. Auth stub: token "good" may act on acct_1 only.
func newHarness(t *testing.T) *harness {
	t.Helper()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	ck := &clock{t: time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)}
	f := fake.New(fake.Config{Prices: catalog.Prices(), Interactive: true, Now: ck.now})
	providers := map[string]billing.Provider{"fake": f}
	m, err := lifecycle.NewManager(lifecycle.Config{
		Catalog: catalog, Providers: providers, Default: "fake",
		Store: lifecycle.NewMemStore(), Applier: noopApplier{}, Now: ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	mux := http.NewServeMux()
	err = Register(mux, Config{
		Manager: m, Catalog: catalog, Providers: providers,
		Authenticate: func(_ context.Context, accountID, bearer string) (bool, error) {
			return bearer == "good" && accountID == "acct_1", nil
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{srv: srv, fake: f, ck: ck}
}

// call makes a request and decodes the JSON document.
func (h *harness) call(t *testing.T, method, path, bearer, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("X-Witself-Email", "s@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode %s %s: %v", method, path, err)
	}
	return resp.StatusCode, doc
}

func TestCatalogEndpoint(t *testing.T) {
	h := newHarness(t)
	status, doc := h.call(t, "GET", "/v1/plans", "", "")
	if status != 200 || doc["schema_version"] != "witself.plans.v0" {
		t.Fatalf("catalog = %d %v", status, doc)
	}
	if plans := doc["plans"].([]any); len(plans) != 4 {
		t.Fatalf("catalog has %d plans; want 4", len(plans))
	}
}

func TestAuthGates(t *testing.T) {
	h := newHarness(t)
	if status, _ := h.call(t, "GET", "/v1/accounts/acct_1/plan", "", ""); status != 401 {
		t.Fatalf("no bearer = %d; want 401", status)
	}
	if status, _ := h.call(t, "GET", "/v1/accounts/acct_1/plan", "bad", ""); status != 403 {
		t.Fatalf("wrong bearer = %d; want 403", status)
	}
	if status, _ := h.call(t, "GET", "/v1/accounts/acct_2/plan", "good", ""); status != 403 {
		t.Fatalf("wrong account = %d; want 403", status)
	}
}

// TestFullLifecycleOverHTTP drives the decided CLI flow end to end through
// the HTTP layer: status(free) -> upgrade(action+url) -> pending visible ->
// the provider's webhook confirms payment -> status(standard) -> downgrade
// (scheduled) -> cancel -> status shows no pending.
func TestFullLifecycleOverHTTP(t *testing.T) {
	h := newHarness(t)

	// Fresh account: free/free.
	status, doc := h.call(t, "GET", "/v1/accounts/acct_1/plan", "good", "")
	if status != 200 || doc["plan"] != "free" || doc["applied"] != "free" {
		t.Fatalf("initial status = %d %v", status, doc)
	}

	// Upgrade: interactive fake -> action + checkout URL.
	status, doc = h.call(t, "POST", "/v1/accounts/acct_1/plan:upgrade", "good", `{"plan":"standard"}`)
	if status != 200 || doc["kind"] != "action" || doc["url"] == "" {
		t.Fatalf("upgrade = %d %v; want action+url", status, doc)
	}

	// Status shows the pending change truthfully.
	_, doc = h.call(t, "GET", "/v1/accounts/acct_1/plan", "good", "")
	pending, ok := doc["pending"].(map[string]any)
	if !ok || pending["kind"] != "upgrade" || pending["plan"] != "standard" {
		t.Fatalf("pending = %v; want the parked upgrade", doc["pending"])
	}

	// The payer completes checkout; the provider announces it via the
	// webhook ROUTE (the fake's callback shape), not an in-process call.
	h.ck.t = h.ck.t.Add(time.Minute)
	events, err := h.fake.Complete("fake_cus_0001")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 activation event, got %d", len(events))
	}
	status, doc = h.call(t, "POST", "/v1/billing/webhook/fake", "",
		`{"customer_id":"fake_cus_0001","type":"subscription_activated","plan":"standard"}`)
	if status != 200 || doc["received"].(float64) != 1 {
		t.Fatalf("webhook = %d %v", status, doc)
	}

	// Entitled + applied, no pending.
	_, doc = h.call(t, "GET", "/v1/accounts/acct_1/plan", "good", "")
	if doc["plan"] != "standard" || doc["applied"] != "standard" || doc["pending"] != nil {
		t.Fatalf("post-payment status = %v; want standard/standard, no pending", doc)
	}

	// Downgrade: scheduled for period end.
	status, doc = h.call(t, "POST", "/v1/accounts/acct_1/plan:downgrade", "good", `{"plan":"free"}`)
	if status != 200 || doc["kind"] != "scheduled" || doc["effective"] == nil {
		t.Fatalf("downgrade = %d %v; want scheduled+effective", status, doc)
	}

	// Cancel restores the status quo.
	if status, doc = h.call(t, "POST", "/v1/accounts/acct_1/plan:cancel", "good", ""); status != 200 || doc["cancelled"] != true {
		t.Fatalf("cancel = %d %v", status, doc)
	}
	_, doc = h.call(t, "GET", "/v1/accounts/acct_1/plan", "good", "")
	if doc["pending"] != nil || doc["plan"] != "standard" {
		t.Fatalf("post-cancel status = %v; want standard, no pending", doc)
	}
}

func TestVerbRefusalsSurfaceVerbatim(t *testing.T) {
	h := newHarness(t)
	status, doc := h.call(t, "POST", "/v1/accounts/acct_1/plan:upgrade", "good", `{"plan":"free"}`)
	if status != 409 || !strings.Contains(doc["error"].(string), "already on") {
		t.Fatalf("upgrade-to-free = %d %v; want the Manager's refusal verbatim", status, doc)
	}
	if status, _ := h.call(t, "POST", "/v1/accounts/acct_1/plan:upgrade", "good", `{}`); status != 400 {
		t.Fatalf("missing plan = %d; want 400", status)
	}
	if status, _ := h.call(t, "POST", "/v1/accounts/acct_1/plan:cancel", "good", ""); status != 409 {
		t.Fatalf("cancel with nothing pending = %d; want 409", status)
	}
}

func TestWebhookRoutes(t *testing.T) {
	h := newHarness(t)
	// Unknown provider: no route mounted.
	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/billing/webhook/stripe", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown provider webhook = %d; want 404", resp.StatusCode)
	}
	// Malformed body: 400 (redelivery of garbage cannot succeed).
	if status, _ := h.call(t, "POST", "/v1/billing/webhook/fake", "", `not json`); status != 400 {
		t.Fatalf("malformed webhook = %d; want 400", status)
	}
	// Unknown customer: could be the claim/fold race on a PAID event — never
	// ACK it away; 500 makes the provider redeliver until it routes.
	if status, _ := h.call(t, "POST", "/v1/billing/webhook/fake", "",
		`{"customer_id":"fake_cus_9999","type":"subscription_activated","plan":"standard"}`); status != 500 {
		t.Fatalf("unknown-customer webhook = %d; want 500 (retry-later)", status)
	}
}

// brokenStore fails every operation — the R2-outage stand-in.
type brokenStore struct{}

func (brokenStore) Get(context.Context, string) (lifecycle.Record, bool, error) {
	return lifecycle.Record{}, false, errStoreDown
}
func (brokenStore) ByCustomer(context.Context, string, string) (lifecycle.Record, bool, error) {
	return lifecycle.Record{}, false, errStoreDown
}
func (brokenStore) Put(context.Context, lifecycle.Record) error { return errStoreDown }
func (brokenStore) List(context.Context) ([]lifecycle.Record, error) {
	return nil, errStoreDown
}

var errStoreDown = errors.New("blob: get registry/accounts/x.json: 503 backend down at https://internal-endpoint")

// TestInfraErrorsAreNot409s: an R2 outage must surface as a generic 500 —
// not a 409 "refusal" carrying raw backend detail the CLI would show a
// customer as a policy decision.
func TestInfraErrorsAreNot409s(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	f := fake.New(fake.Config{Prices: catalog.Prices()})
	providers := map[string]billing.Provider{"fake": f}
	m, err := lifecycle.NewManager(lifecycle.Config{
		Catalog: catalog, Providers: providers, Default: "fake",
		Store: brokenStore{}, Applier: noopApplier{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	mux := http.NewServeMux()
	if err := Register(mux, Config{
		Manager: m, Catalog: catalog, Providers: providers,
		Authenticate: func(context.Context, string, string) (bool, error) { return true, nil },
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := &harness{srv: srv}

	status, doc := h.call(t, "POST", "/v1/accounts/acct_1/plan:upgrade", "any", `{"plan":"standard"}`)
	if status != 500 {
		t.Fatalf("store outage = %d %v; want 500, never a 409 refusal", status, doc)
	}
	if msg := doc["error"].(string); strings.Contains(msg, "internal-endpoint") || strings.Contains(msg, "blob:") {
		t.Fatalf("error body leaks backend detail: %q", msg)
	}
	// A genuine refusal still reads as 409 with its message.
	// (Covered by TestVerbRefusalsSurfaceVerbatim against the real store.)
}
