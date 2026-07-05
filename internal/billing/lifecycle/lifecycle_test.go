package lifecycle

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/plans"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type applyCall struct {
	accountID, plan string
	limits          map[string]int64
	features        []string
}

// recApplier records Apply calls and can simulate cell-push failures.
type recApplier struct {
	mu    sync.Mutex
	fail  bool
	calls []applyCall
}

func (a *recApplier) Apply(_ context.Context, accountID, plan string, limits map[string]int64, features []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return context.DeadlineExceeded
	}
	a.calls = append(a.calls, applyCall{accountID, plan, limits, features})
	return nil
}

func (a *recApplier) last(t *testing.T) applyCall {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		t.Fatal("no Apply calls recorded")
	}
	return a.calls[len(a.calls)-1]
}

type fitStub struct{ violations []string }

func (f fitStub) Fit(context.Context, string, plans.Plan) ([]string, error) {
	return f.violations, nil
}

type harness struct {
	m       *Manager
	fake    *fake.Fake
	store   *MemStore
	applier *recApplier
	ck      *clock
	fit     *fitStub
}

func newHarness(t *testing.T, interactive bool) *harness {
	t.Helper()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	ck := &clock{t: time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)}
	f := fake.New(fake.Config{Prices: catalog.Prices(), Interactive: interactive, Now: ck.now})
	st := NewMemStore()
	ap := &recApplier{}
	fit := &fitStub{}
	m, err := NewManager(Config{
		Catalog: catalog, Providers: map[string]billing.Provider{"fake": f}, Default: "fake",
		Store: st, Applier: ap, Fit: fit, Now: ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return &harness{m: m, fake: f, store: st, applier: ap, ck: ck, fit: fit}
}

func (h *harness) record(t *testing.T, accountID string) Record {
	t.Helper()
	r, ok, err := h.store.Get(context.Background(), accountID)
	if err != nil || !ok {
		t.Fatalf("record %s: ok=%v err=%v", accountID, ok, err)
	}
	return r
}

func TestStatusDefaultsFree(t *testing.T) {
	h := newHarness(t, false)
	r, err := h.m.Status(context.Background(), "acct_1", "s@example.com")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if r.Entitled != plans.Free || r.Applied != plans.Free || r.Pending != nil || r.CustomerID != "" {
		t.Fatalf("fresh record = %+v; want free/free, no pending, no customer", r)
	}
}

func TestHeadlessUpgrade(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	out, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard")
	if err != nil || out.Kind != "done" {
		t.Fatalf("RequestUpgrade = %+v, %v; want done", out, err)
	}
	r := h.record(t, "acct_1")
	if r.Entitled != "standard" || r.Applied != "standard" || r.Pending != nil {
		t.Fatalf("record = %+v; want entitled+applied standard", r)
	}
	call := h.applier.last(t)
	if call.plan != "standard" || call.limits["agents"] != 250 || call.limits["realms"] != 10 {
		t.Fatalf("applied snapshot = %+v; want standard 250/10", call)
	}
	joined := strings.Join(call.features, ",")
	if !strings.Contains(joined, "secrets") || !strings.Contains(joined, "collaboration") {
		t.Fatalf("applied features = %v; want secrets+collaboration", call.features)
	}
}

func TestInteractiveUpgradeFlow(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	out, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard")
	if err != nil || out.Kind != "action" || out.URL == "" {
		t.Fatalf("RequestUpgrade = %+v, %v; want action+URL", out, err)
	}
	r := h.record(t, "acct_1")
	if r.Pending == nil || r.Pending.Kind != PendingUpgrade || r.Pending.Plan != "standard" {
		t.Fatalf("pending = %+v; want upgrade->standard", r.Pending)
	}
	if !r.Pending.Expires.Equal(h.ck.t.Add(DefaultPendingTTL)) {
		t.Fatalf("expires = %v; want request+TTL", r.Pending.Expires)
	}
	if r.Entitled != plans.Free {
		t.Fatalf("entitled = %q before payment; want free", r.Entitled)
	}

	// The payer completes checkout; the provider's events arrive (webhook).
	events, err := h.fake.Complete(r.CustomerID)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := h.m.OnEvents(ctx, "fake", events); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	r = h.record(t, "acct_1")
	if r.Entitled != "standard" || r.Applied != "standard" || r.Pending != nil {
		t.Fatalf("after events record = %+v; want entitled+applied standard, no pending", r)
	}
}

func TestPendingUpgradeExpires(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	h.ck.t = h.ck.t.Add(DefaultPendingTTL + time.Hour) // the payer never returns
	if err := h.m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if r = h.record(t, "acct_1"); r.Pending != nil {
		t.Fatalf("pending survived expiry: %+v", r.Pending)
	}
	// The provider-side checkout was cancelled too.
	if _, err := h.fake.Complete(r.CustomerID); err == nil {
		t.Fatal("provider checkout should have been cancelled at expiry")
	}
}

func TestUpgradeValidation(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "nope"); err == nil {
		t.Fatal("unknown plan should error")
	}
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "free"); err == nil ||
		!strings.Contains(err.Error(), "already on") {
		t.Fatal("upgrading free->free should read as already-on")
	}
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err == nil ||
		!strings.Contains(err.Error(), "already on") {
		t.Fatal("re-upgrading to the current plan should error")
	}
	// Direction enforcement: from a paid plan, a cheaper target is a downgrade.
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "free"); err == nil ||
		!strings.Contains(err.Error(), "not an upgrade") {
		t.Fatal("upgrading standard->free should point at downgrade")
	}
	if _, err := h.m.RequestDowngrade(ctx, "acct_1", "s@example.com", "team"); err == nil ||
		!strings.Contains(err.Error(), "not a downgrade") {
		t.Fatal("downgrading standard->team should point at upgrade")
	}
}

func TestUnavailableTierRecordsLead(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	out, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "enterprise")
	if err != nil || out.Kind != "contact" {
		t.Fatalf("RequestUpgrade enterprise = %+v, %v; want contact", out, err)
	}
	r := h.record(t, "acct_1")
	if r.Pending == nil || r.Pending.Kind != PendingContact || r.Pending.Plan != "enterprise" {
		t.Fatalf("pending = %+v; want a recorded enterprise lead", r.Pending)
	}
	if r.CustomerID != "" {
		t.Fatal("a sales lead must not create billing objects")
	}
	if err := h.m.CancelPending(ctx, "acct_1"); err != nil {
		t.Fatalf("CancelPending: %v", err)
	}
	if r = h.record(t, "acct_1"); r.Pending != nil {
		t.Fatalf("lead survived cancel: %+v", r.Pending)
	}
}

func TestDowngradeFitBlocked(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}

	h.fit.violations = []string{"agents 112 > 25", "3 realms > 1", "14 secrets in use"}
	_, err := h.m.RequestDowngrade(ctx, "acct_1", "s@example.com", "free")
	if err == nil || !strings.Contains(err.Error(), "agents 112 > 25") {
		t.Fatalf("blocked downgrade error = %v; want the violation report", err)
	}
	if r := h.record(t, "acct_1"); r.Pending != nil {
		t.Fatalf("blocked downgrade must not park a pending change: %+v", r.Pending)
	}
}

func TestScheduledDowngradeFlow(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	periodEnd := h.ck.t.AddDate(0, 0, 30)

	out, err := h.m.RequestDowngrade(ctx, "acct_1", "s@example.com", "free")
	if err != nil || out.Kind != "scheduled" || !out.Effective.Equal(periodEnd) {
		t.Fatalf("RequestDowngrade = %+v, %v; want scheduled at period end", out, err)
	}
	r := h.record(t, "acct_1")
	if r.Pending == nil || r.Pending.Kind != PendingDowngrade || r.Entitled != "standard" {
		t.Fatalf("record = %+v; want pending downgrade, still entitled standard", r)
	}

	// The period ends; the provider announces it (webhook-equivalent).
	h.ck.t = periodEnd.Add(time.Hour)
	if err := h.m.OnEvents(ctx, "fake", h.fake.ApplyDue()); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	r = h.record(t, "acct_1")
	if r.Entitled != plans.Free || r.Applied != plans.Free || r.Pending != nil {
		t.Fatalf("after period end record = %+v; want free/free, no pending", r)
	}
	if call := h.applier.last(t); call.plan != plans.Free || call.limits["agents"] != 25 {
		t.Fatalf("applied snapshot = %+v; want free 25/1", call)
	}
}

func TestApplierFailureThenReconcile(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	h.applier.fail = true // the cell push fails: paid but not applied
	out, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard")
	if err != nil || out.Kind != "done" {
		t.Fatalf("RequestUpgrade = %+v, %v", out, err)
	}
	r := h.record(t, "acct_1")
	if r.Entitled != "standard" || r.Applied != plans.Free {
		t.Fatalf("record = %+v; want entitled standard, applied still free", r)
	}

	h.applier.fail = false // the cell recovers; the sweep converges the gap
	if err := h.m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if r = h.record(t, "acct_1"); r.Applied != "standard" {
		t.Fatalf("Reconcile did not converge applied: %+v", r)
	}
}

func TestPastDueTracking(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	fail := []billing.Event{{Type: billing.EventPaymentFailed, CustomerID: r.CustomerID, At: h.ck.t}}
	if err := h.m.OnEvents(ctx, "fake", fail); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	if r = h.record(t, "acct_1"); r.PastDueSince == nil {
		t.Fatal("payment failure should set PastDueSince")
	}
	// Redelivery must not move the original past-due timestamp.
	later := []billing.Event{{Type: billing.EventPaymentFailed, CustomerID: r.CustomerID, At: h.ck.t.Add(time.Hour)}}
	if err := h.m.OnEvents(ctx, "fake", later); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	if r2 := h.record(t, "acct_1"); !r2.PastDueSince.Equal(*r.PastDueSince) {
		t.Fatal("redelivered failure moved PastDueSince")
	}

	rec := []billing.Event{{Type: billing.EventPaymentRecovered, CustomerID: r.CustomerID, At: h.ck.t}}
	if err := h.m.OnEvents(ctx, "fake", rec); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	if r = h.record(t, "acct_1"); r.PastDueSince != nil {
		t.Fatal("recovery should clear PastDueSince")
	}
}

// TestProviderCutover proves the fake -> real migration path: switching the
// DEFAULT provider routes new purchases to the new partner while existing
// accounts keep routing to the provider that holds their subscription, card,
// and invoice history. The cutover is a config change, not a migration event.
func TestProviderCutover(t *testing.T) {
	ctx := context.Background()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	ck := &clock{t: time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)}
	oldP := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	st := NewMemStore()
	ap := &recApplier{}

	// Era 1: only the fake exists; an account buys standard on it.
	m1, err := NewManager(Config{
		Catalog: catalog, Providers: map[string]billing.Provider{"fake": oldP}, Default: "fake",
		Store: st, Applier: ap, Now: ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager era1: %v", err)
	}
	if _, err := m1.RequestUpgrade(ctx, "acct_old", "old@example.com", "standard"); err != nil {
		t.Fatalf("era1 upgrade: %v", err)
	}

	// Era 2: the real partner arrives; SAME store, default flips to "stripe".
	newP := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	m2, err := NewManager(Config{
		Catalog:   catalog,
		Providers: map[string]billing.Provider{"fake": oldP, "stripe": newP},
		Default:   "stripe",
		Store:     st, Applier: ap, Now: ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager era2: %v", err)
	}

	// New purchases land on the new default...
	if _, err := m2.RequestUpgrade(ctx, "acct_new", "new@example.com", "standard"); err != nil {
		t.Fatalf("era2 new-account upgrade: %v", err)
	}
	rNew, _, _ := st.Get(ctx, "acct_new")
	if rNew.Provider != "stripe" {
		t.Fatalf("new account pinned to %q; want stripe", rNew.Provider)
	}
	if inv, err := newP.ListInvoices(ctx, rNew.CustomerID); err != nil || len(inv) != 1 {
		t.Fatalf("new account's invoice should live at the new provider: %v, %v", inv, err)
	}

	// ...while the existing account still routes to its pinned provider.
	rOld, _, _ := st.Get(ctx, "acct_old")
	if rOld.Provider != "fake" {
		t.Fatalf("old account pinned to %q; want fake", rOld.Provider)
	}
	out, err := m2.RequestDowngrade(ctx, "acct_old", "old@example.com", "free")
	if err != nil || out.Kind != "scheduled" {
		t.Fatalf("era2 old-account downgrade = %+v, %v; want scheduled via the OLD provider", out, err)
	}
	if _, err := oldP.ListInvoices(ctx, rOld.CustomerID); err != nil {
		t.Fatalf("old account's history must stay readable at the old provider: %v", err)
	}

	// Both partners minted the SAME customer id for different accounts (each
	// fake numbers from 1) — exactly the cross-provider collision the scoped
	// lookup exists for. A stripe-scoped event for this id must touch only
	// the stripe-pinned account, never the fake-pinned one.
	if rOld.CustomerID != rNew.CustomerID {
		t.Fatalf("test premise: expected colliding customer ids, got %q vs %q", rOld.CustomerID, rNew.CustomerID)
	}
	ev := []billing.Event{{Type: billing.EventSubscriptionCanceled, CustomerID: rOld.CustomerID, At: ck.t}}
	if err := m2.OnEvents(ctx, "stripe", ev); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	if r, _, _ := st.Get(ctx, "acct_old"); r.Entitled != "standard" {
		t.Fatalf("a stripe-scoped event must not touch a fake-pinned record; entitled = %q", r.Entitled)
	}
	if r, _, _ := st.Get(ctx, "acct_new"); r.Entitled != plans.Free {
		t.Fatalf("the stripe-scoped event should have reached the stripe-pinned record; entitled = %q", r.Entitled)
	}
}
