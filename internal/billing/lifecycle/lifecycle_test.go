package lifecycle

import (
	"context"
	"errors"
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

type fitStub struct {
	mu         sync.Mutex
	violations []string
}

func (f *fitStub) set(v []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.violations = v
}

func (f *fitStub) Fit(context.Context, string, plans.Plan) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.violations, nil
}

// hookStore interposes on Put to deterministically interleave a competing
// writer inside another operation's read-decide-write window.
type hookStore struct {
	Store
	mu        sync.Mutex
	beforePut func(Record)
}

func (h *hookStore) Put(ctx context.Context, r Record) error {
	h.mu.Lock()
	f := h.beforePut
	h.beforePut = nil
	h.mu.Unlock()
	if f != nil {
		f(r)
	}
	return h.Store.Put(ctx, r)
}

type harness struct {
	m       *Manager
	fake    *fake.Fake
	store   *MemStore
	hooked  *hookStore
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
	hooked := &hookStore{Store: st}
	ap := &recApplier{}
	fit := &fitStub{}
	m, err := NewManager(Config{
		Catalog: catalog, Providers: map[string]billing.Provider{"fake": f}, Default: "fake",
		Store: hooked, Applier: ap, Fit: fit, Now: ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return &harness{m: m, fake: f, store: st, hooked: hooked, applier: ap, ck: ck, fit: fit}
}

func (h *harness) record(t *testing.T, accountID string) Record {
	t.Helper()
	r, ok, err := h.store.Get(context.Background(), accountID)
	if err != nil || !ok {
		t.Fatalf("record %s: ok=%v err=%v", accountID, ok, err)
	}
	return r
}

func TestStatusIsReadOnly(t *testing.T) {
	h := newHarness(t, false)
	r, err := h.m.Status(context.Background(), "acct_1", "s@example.com")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if r.Entitled != plans.Free || r.Applied != plans.Free || r.Pending != nil || r.CustomerID != "" {
		t.Fatalf("fresh record = %+v; want free/free, no pending, no customer", r)
	}
	// Probing an account id must not create phantom rows.
	if all, _ := h.store.List(context.Background()); len(all) != 0 {
		t.Fatalf("Status persisted %d record(s); reads must not write", len(all))
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
	h.ck.t = h.ck.t.Add(time.Minute)
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

	h.fit.set([]string{"agents 112 > 25", "3 realms > 1", "14 secrets in use"})
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

	rec := []billing.Event{{Type: billing.EventPaymentRecovered, CustomerID: r.CustomerID, At: h.ck.t.Add(2 * time.Hour)}}
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
	ck.t = ck.t.Add(time.Minute)
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

// TestCancelRacingActivation reproduces the review's HIGH finding: the payer
// completes checkout in exactly the window between CancelPending's read and
// its write. Without CAS the stale write landed last and the customer paid
// for a plan the record denied; with CAS the cancel loses, re-reads, and
// reports "nothing is pending" while the paid entitlement stands.
func TestCancelRacingActivation(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	// The race window under disarm-first ordering: the payer completes
	// checkout in the instant between CancelPending's read and its provider
	// disarm. Simulated with a provider hook (the store hook can no longer
	// reach this window — that is the point of the reordering).
	hooked := &hookProvider{Provider: h.fake, beforeCancel: func() {
		h.ck.t = h.ck.t.Add(time.Minute)
		events, err := h.fake.Complete(r.CustomerID)
		if err != nil {
			t.Errorf("Complete: %v", err)
		}
		if err := h.m.OnEvents(ctx, "fake", events); err != nil {
			t.Errorf("OnEvents: %v", err)
		}
	}}
	m2, err := NewManager(Config{
		Catalog: mustCatalog(t), Providers: map[string]billing.Provider{"fake": hooked}, Default: "fake",
		Store: h.store, Applier: h.applier, Now: h.ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	err = m2.CancelPending(ctx, "acct_1")
	if err == nil || !strings.Contains(err.Error(), "nothing is pending") {
		t.Fatalf("CancelPending racing activation = %v; want 'nothing is pending'", err)
	}
	final := h.record(t, "acct_1")
	if final.Entitled != "standard" || final.Applied != "standard" {
		t.Fatalf("record = %+v; the paid entitlement must survive the racing cancel", final)
	}
}

// hookProvider interposes on CancelPending to interleave work into the
// disarm window.
type hookProvider struct {
	billing.Provider
	beforeCancel func()
}

func (h *hookProvider) CancelPending(ctx context.Context, customerID string) error {
	if f := h.beforeCancel; f != nil {
		h.beforeCancel = nil
		f()
	}
	return h.Provider.CancelPending(ctx, customerID)
}

// TestStaleCancelDropped reproduces the review's redelivery finding: a
// cancellation whose partner timestamp predates the current entitlement (a
// redelivered or out-of-order webhook) must be dropped, not clobber a newer
// paid subscription.
func TestStaleCancelDropped(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	// Dunning cancels the subscription at T1...
	t1 := h.ck.t.Add(time.Hour)
	cancel := []billing.Event{{Type: billing.EventSubscriptionCanceled, CustomerID: r.CustomerID, At: t1}}
	if err := h.m.OnEvents(ctx, "fake", cancel); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	if r = h.record(t, "acct_1"); r.Entitled != plans.Free {
		t.Fatalf("entitled = %q after cancel; want free", r.Entitled)
	}

	// ...the customer re-subscribes at T2 > T1...
	h.ck.t = t1.Add(time.Hour)
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("re-upgrade: %v", err)
	}

	// ...and the T1 cancellation is REDELIVERED (ack was lost). It is stale
	// against the T2 entitlement and must be dropped.
	if err := h.m.OnEvents(ctx, "fake", cancel); err != nil {
		t.Fatalf("OnEvents redelivery: %v", err)
	}
	if r = h.record(t, "acct_1"); r.Entitled != "standard" || r.Applied != "standard" {
		t.Fatalf("record = %+v; a stale redelivered cancel clobbered a newer paid subscription", r)
	}
}

// TestApplyTimeFitCheck reproduces the review's missing-authoritative-check
// finding: usage grows past the target's caps during the 30-day wait, so the
// period-end downgrade must NOT be applied to the cell — the gap stays
// visible (Entitled != Applied, ApplyBlocked) until the account fits.
func TestApplyTimeFitCheck(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	periodEnd := h.ck.t.AddDate(0, 0, 30)
	if _, err := h.m.RequestDowngrade(ctx, "acct_1", "s@example.com", "free"); err != nil {
		t.Fatalf("RequestDowngrade: %v", err) // fits at request time
	}

	// Usage grows during the wait; at period end billing cancels regardless.
	h.fit.set([]string{"agents 112 > 25"})
	h.ck.t = periodEnd.Add(time.Hour)
	if err := h.m.OnEvents(ctx, "fake", h.fake.ApplyDue()); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	r := h.record(t, "acct_1")
	if r.Entitled != plans.Free {
		t.Fatalf("entitled = %q; billing did end the subscription", r.Entitled)
	}
	if r.Applied != "standard" || !strings.Contains(r.ApplyBlocked, "agents 112 > 25") {
		t.Fatalf("record = %+v; the cell must keep the old plan and the block must be visible", r)
	}
	if call := h.applier.last(t); call.plan != "standard" {
		t.Fatalf("a free snapshot was pushed despite the failed fit check: %+v", call)
	}

	// The account is pruned; the next sweep converges.
	h.fit.set(nil)
	if err := h.m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	r = h.record(t, "acct_1")
	if r.Applied != plans.Free || r.ApplyBlocked != "" {
		t.Fatalf("record = %+v; want applied free with the block cleared", r)
	}
}

// TestReplacingPendingCancelsProviderSide reproduces the review's finding
// that replacing a pending change must also cancel it at the provider — a
// "replaced" scheduled downgrade must not fire at period end.
func TestReplacingPendingCancelsProviderSide(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	periodEnd := h.ck.t.AddDate(0, 0, 30)
	if _, err := h.m.RequestDowngrade(ctx, "acct_1", "s@example.com", "free"); err != nil {
		t.Fatalf("RequestDowngrade: %v", err)
	}
	// The customer changes their mind toward enterprise: the lead replaces
	// the scheduled downgrade — INCLUDING at the provider.
	if out, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "enterprise"); err != nil || out.Kind != "contact" {
		t.Fatalf("RequestUpgrade enterprise = %+v, %v", out, err)
	}

	h.ck.t = periodEnd.Add(time.Hour)
	if events := h.fake.ApplyDue(); len(events) != 0 {
		t.Fatalf("the replaced downgrade still fired at the provider: %+v", events)
	}
	r := h.record(t, "acct_1")
	if r.Entitled != "standard" || r.Pending == nil || r.Pending.Kind != PendingContact {
		t.Fatalf("record = %+v; want standard entitlement with the enterprise lead pending", r)
	}
}

// TestUnknownPlanEventErrors: an activation for a plan the catalog does not
// know is deploy skew — it must fail the batch (provider redelivers until the
// catalog catches up) and must not corrupt entitlement meanwhile.
func TestUnknownPlanEventErrors(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	bogus := []billing.Event{{Type: billing.EventSubscriptionActivated, CustomerID: r.CustomerID, Plan: "mystery", At: h.ck.t.Add(time.Hour)}}
	if err := h.m.OnEvents(ctx, "fake", bogus); err == nil {
		t.Fatal("unknown-plan activation must fail the batch for redelivery")
	}
	if r = h.record(t, "acct_1"); r.Entitled != "standard" {
		t.Fatalf("entitled = %q; the unroutable event must not corrupt state", r.Entitled)
	}
}

// TestCancelClearsPastDue: terminal dunning ends with a cancellation — the
// account lands on free, not on free-and-eternally-past-due.
func TestCancelClearsPastDue(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	events := []billing.Event{
		{Type: billing.EventPaymentFailed, CustomerID: r.CustomerID, At: h.ck.t.Add(time.Hour)},
		{Type: billing.EventSubscriptionCanceled, CustomerID: r.CustomerID, At: h.ck.t.Add(2 * time.Hour)},
	}
	if err := h.m.OnEvents(ctx, "fake", events); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
	r = h.record(t, "acct_1")
	if r.Entitled != plans.Free || r.PastDueSince != nil {
		t.Fatalf("record = %+v; want free with PastDueSince cleared", r)
	}
}

// TestDunningFence reproduces the review's redelivered-dunning finding: a
// payment_failed folded AFTER its recovery (partial-batch redelivery) must
// not re-mark a paid-up account past due.
func TestDunningFence(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")

	t1, t2 := h.ck.t.Add(time.Hour), h.ck.t.Add(2*time.Hour)
	failed := []billing.Event{{Type: billing.EventPaymentFailed, CustomerID: r.CustomerID, At: t1}}
	recovered := []billing.Event{{Type: billing.EventPaymentRecovered, CustomerID: r.CustomerID, At: t2}}
	if err := h.m.OnEvents(ctx, "fake", failed); err != nil {
		t.Fatalf("OnEvents failed: %v", err)
	}
	if err := h.m.OnEvents(ctx, "fake", recovered); err != nil {
		t.Fatalf("OnEvents recovered: %v", err)
	}
	// The stale T1 failure redelivers (lost ACK / partial batch): fenced out.
	if err := h.m.OnEvents(ctx, "fake", failed); err != nil {
		t.Fatalf("OnEvents redelivered: %v", err)
	}
	if r = h.record(t, "acct_1"); r.PastDueSince != nil {
		t.Fatalf("redelivered stale payment_failed re-marked a recovered account: %+v", r)
	}
}

// TestUnroutableEventsFailTheBatch: unknown customers and unknown plans must
// error (so webhooks 500 and the provider redelivers) — a silent ACK would
// drop a possibly-paid event forever.
func TestUnroutableEventsFailTheBatch(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if err := h.m.OnEvents(ctx, "fake", []billing.Event{
		{Type: billing.EventSubscriptionActivated, CustomerID: "fake_cus_9999", Plan: "standard", At: h.ck.t},
	}); err == nil {
		t.Fatal("unknown-customer event must fail the batch, not be ACKed away")
	}
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	r := h.record(t, "acct_1")
	if err := h.m.OnEvents(ctx, "fake", []billing.Event{
		{Type: billing.EventSubscriptionActivated, CustomerID: r.CustomerID, Plan: "mystery", At: h.ck.t.Add(time.Hour)},
	}); err == nil {
		t.Fatal("unknown-plan activation must fail the batch (deploy skew), not vanish")
	}
}

// TestCancelDisarmsProviderFirst reproduces the review's stuck-state finding:
// if the provider's cancel fails, the record must still show the pending
// change (truthful + retryable) — not "nothing pending" with a schedule
// silently armed at the partner.
func TestCancelDisarmsProviderFirst(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	if _, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "standard"); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	if _, err := h.m.RequestDowngrade(ctx, "acct_1", "s@example.com", "free"); err != nil {
		t.Fatalf("RequestDowngrade: %v", err)
	}
	// Simulate the partner API failing the disarm: the fake errors on an
	// unknown customer, so point the record's customer at a vanished id by
	// building a manager whose provider has no such customer.
	other := fake.New(fake.Config{Prices: map[string]int64{"standard": 3000}})
	m2, err := NewManager(Config{
		Catalog: mustCatalog(t), Providers: map[string]billing.Provider{"fake": other}, Default: "fake",
		Store: h.store, Applier: h.applier, Now: h.ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m2.CancelPending(ctx, "acct_1"); err == nil {
		t.Fatal("provider disarm failure must surface as an error")
	}
	if r := h.record(t, "acct_1"); r.Pending == nil {
		t.Fatal("provider disarm failed but the local pending was cleared — the stuck state the review found")
	}
	// The original manager (working provider) retries successfully.
	if err := h.m.CancelPending(ctx, "acct_1"); err != nil {
		t.Fatalf("retry after provider recovery: %v", err)
	}
}

func mustCatalog(t *testing.T) *plans.Catalog {
	t.Helper()
	c, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	return c
}

// TestRefusalSentinel: user refusals carry ErrRefusal; infra errors don't.
func TestRefusalSentinel(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	_, err := h.m.RequestUpgrade(ctx, "acct_1", "s@example.com", "nope")
	if !errors.Is(err, ErrRefusal) {
		t.Fatalf("unknown plan = %v; want ErrRefusal", err)
	}
	if err := h.m.CancelPending(ctx, "acct_1"); !errors.Is(err, ErrRefusal) {
		t.Fatalf("nothing-pending = %v; want ErrRefusal", err)
	}
}
