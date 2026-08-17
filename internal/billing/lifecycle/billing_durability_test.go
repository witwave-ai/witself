package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/blob"
	"github.com/witwave-ai/witself/internal/plans"
)

type blockingResolverProvider struct {
	billing.Provider
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

type changingResolverProvider struct {
	billing.Provider
	calls atomic.Int64
}

type failOnceResolverProvider struct {
	billing.Provider
	calls atomic.Int64
}

func (p *failOnceResolverProvider) ResolveEvent(
	_ context.Context,
	event billing.Event,
) (*billing.Event, error) {
	if p.calls.Add(1) == 1 {
		return nil, errors.New("induced first resolution failure")
	}
	resolved := event
	return &resolved, nil
}

func (p *changingResolverProvider) ResolveEvent(
	_ context.Context,
	event billing.Event,
) (*billing.Event, error) {
	if p.calls.Add(1) != 1 {
		// Deliberately reverse the first applied outcome. A crash recovery that
		// consults the provider again would now ignore this delivery.
		return nil, nil
	}
	resolved := event
	return &resolved, nil
}

func (p *blockingResolverProvider) ResolveEvent(
	ctx context.Context,
	event billing.Event,
) (*billing.Event, error) {
	p.calls.Add(1)
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		resolved := event
		return &resolved, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type failCompleteStore struct {
	*MemStore
	mu        sync.Mutex
	failCount int
}

func (s *failCompleteStore) CompleteEvent(
	ctx context.Context,
	receipt EventReceipt,
	processedAt time.Time,
) error {
	s.mu.Lock()
	if s.failCount > 0 {
		s.failCount--
		s.mu.Unlock()
		return errors.New("induced receipt completion failure")
	}
	s.mu.Unlock()
	return s.MemStore.CompleteEvent(ctx, receipt, processedAt)
}

func durableManager(
	t *testing.T,
	store Store,
	provider billing.Provider,
	now func() time.Time,
) *Manager {
	t.Helper()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{
		Catalog: catalog,
		Providers: map[string]billing.Provider{
			"fake": provider,
		},
		Default: "fake",
		Store:   store,
		Applier: &recApplier{},
		Now:     now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestDurableEventReceiptSurvivesFoldCompletionCrash(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	provider := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	store := &failCompleteStore{MemStore: NewMemStore(), failCount: 1}
	manager := durableManager(t, store, provider, ck.now)

	if _, err := manager.RequestUpgrade(
		ctx, "acct_receipt", "owner@example.com", "standard"); err != nil {
		t.Fatal(err)
	}
	record, ok, err := store.Get(ctx, "acct_receipt")
	if err != nil || !ok {
		t.Fatalf("record = %+v, ok=%v, err=%v", record, ok, err)
	}
	event := durableTestEvent("evt_failed_1", record.CustomerID)
	if err := manager.OnEvents(ctx, "fake", []billing.Event{event}); err == nil {
		t.Fatal("induced completion failure was ACKed")
	}
	record, _, _ = store.Get(ctx, "acct_receipt")
	if record.PastDueSince == nil {
		t.Fatal("event was not folded before the completion failure")
	}
	pending, err := store.PendingEvents(ctx, "fake", record.CustomerID, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("receipt was not left pending: %+v, %v", pending, err)
	}

	// No provider redelivery is required: the account reconciler consumes its
	// bounded pending index and finishes the receipt.
	if err := manager.ReconcileAccount(ctx, "acct_receipt"); err != nil {
		t.Fatalf("ReconcileAccount: %v", err)
	}
	pending, err = store.PendingEvents(ctx, "fake", record.CustomerID, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after reconciliation = %+v, %v", pending, err)
	}
	beforeReplay, _, _ := store.Get(ctx, "acct_receipt")
	if err := manager.OnEvents(ctx, "fake", []billing.Event{event}); err != nil {
		t.Fatalf("processed redelivery: %v", err)
	}
	afterReplay, _, _ := store.Get(ctx, "acct_receipt")
	if afterReplay.Version != beforeReplay.Version {
		t.Fatalf("processed redelivery refolded the account: version %d -> %d",
			beforeReplay.Version, afterReplay.Version)
	}
}

func TestEventReceiptLeaseAllowsOnlyOneLiveFolder(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	underlying := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	provider := &blockingResolverProvider{
		Provider: underlying, entered: make(chan struct{}, 1), release: make(chan struct{}),
	}
	store := NewMemStore()
	manager := durableManager(t, store, provider, ck.now)
	putBillingRecordForEventTest(t, store, "acct_claimed", "cus_claimed")
	event := durableTestEvent("evt_claimed", "cus_claimed")

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- manager.OnEvents(ctx, "fake", []billing.Event{event})
	}()
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first event processor did not enter resolver")
	}
	if err := manager.OnEvents(ctx, "fake", []billing.Event{event}); !errors.Is(err, ErrEventReceiptInProgress) {
		t.Fatalf("concurrent event processing = %v; want ErrEventReceiptInProgress", err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("resolver calls while lease live = %d; want 1", got)
	}
	close(provider.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("lease owner processing: %v", err)
	}
	if err := manager.OnEvents(ctx, "fake", []billing.Event{event}); err != nil {
		t.Fatalf("processed replay: %v", err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("processed replay refolded event: resolver calls=%d", got)
	}
}

func TestExpiredEventReceiptClaimRetriesFoldAfterCrash(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 12, 45, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	provider := &changingResolverProvider{Provider: fake.New(fake.Config{
		Prices: catalog.Prices(), Now: ck.now,
	})}
	store := NewMemStore()
	manager := durableManager(t, store, provider, ck.now)
	putBillingRecordForEventTest(t, store, "acct_crashed_fold", "cus_crashed_fold")
	event := durableTestEvent("evt_crashed_fold", "cus_crashed_fold")
	receipt, err := newEventReceipt("fake", event, ck.now())
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.ReceiveEvent(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	claimed, acquired, err := store.ClaimEvent(
		ctx, stored, "ecl_crashed_owner", ck.now(),
		ck.now().Add(eventReceiptProcessingLease))
	if err != nil || !acquired {
		t.Fatalf("crashed claim = %+v, acquired=%v, err=%v", claimed, acquired, err)
	}
	resolution, err := manager.resolveEvent(ctx, "fake", event, true)
	if err != nil {
		t.Fatalf("resolve before crash: %v", err)
	}
	_, err = store.PinEventResolution(ctx, claimed, resolution)
	if err != nil {
		t.Fatalf("pin before crash: %v", err)
	}
	// Simulate a process dying after the pinned account fold but before receipt
	// completion or claim release.
	if _, err := manager.foldEventResolution(ctx, resolution); err != nil {
		t.Fatalf("first fold: %v", err)
	}
	if err := manager.OnEvents(ctx, "fake", []billing.Event{event}); !errors.Is(err, ErrEventReceiptInProgress) {
		t.Fatalf("live crashed lease = %v; want ErrEventReceiptInProgress", err)
	}
	ck.t = ck.t.Add(eventReceiptProcessingLease)
	if err := manager.OnEvents(ctx, "fake", []billing.Event{event}); err != nil {
		t.Fatalf("expired claim recovery: %v", err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("crash recovery re-resolved changed provider outcome: calls=%d", got)
	}
	got, _, _ := store.Get(ctx, "acct_crashed_fold")
	if got.PastDueSince == nil {
		t.Fatalf("recovered fold lost account state: %+v", got)
	}
	processed, created, err := store.ReceiveEvent(ctx, receipt)
	if err != nil || created || processed.Status != EventReceiptProcessed ||
		processed.Decision != "applied" || processed.Resolution == nil ||
		processed.Resolution.Decision != "applied" {
		t.Fatalf("recovered receipt = %+v, created=%v, err=%v", processed, created, err)
	}
}

func putBillingRecordForEventTest(
	t *testing.T,
	store *MemStore,
	accountID, customerID string,
) {
	t.Helper()
	if err := store.Put(context.Background(), Record{
		AccountID: accountID, Provider: "fake", CustomerID: customerID,
		Entitled: "standard", Applied: "standard",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDurableUnknownCustomerEventIsCompletedIgnored(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	provider := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	store := NewMemStore()
	manager := durableManager(t, store, provider, ck.now)
	event := durableTestEvent("evt_foreign_1", "cus_not_ours")

	if err := manager.OnEvents(ctx, "fake", []billing.Event{event}); err != nil {
		t.Fatalf("unrelated durable provider event was not ignored: %v", err)
	}
	receipt, err := newEventReceipt("fake", event, ck.now())
	if err != nil {
		t.Fatal(err)
	}
	stored, created, err := store.ReceiveEvent(ctx, receipt)
	if err != nil || created || stored.Status != EventReceiptProcessed ||
		stored.Decision != "ignored" {
		t.Fatalf("unknown-customer receipt = %+v, created=%v, err=%v", stored, created, err)
	}
	if pending, err := store.PendingEvents(ctx, "fake", event.CustomerID, 10); err != nil || len(pending) != 0 {
		t.Fatalf("unknown-customer receipt remains pending: %+v, %v", pending, err)
	}
}

func TestDelayedActivationDoesNotClearNewerUpgradeOperation(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 15, 0, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	provider := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	store := NewMemStore()
	manager := durableManager(t, store, provider, ck.now)
	const (
		accountID  = "acct_activation_fence"
		customerID = "cus_activation_fence"
		operationA = "bop_superseded_a"
		operationB = "bop_current_b"
	)
	if err := store.Put(ctx, Record{
		AccountID: accountID, Provider: "fake", CustomerID: customerID,
		Entitled: plans.Free, Applied: plans.Free,
		Pending: &Pending{
			Kind: PendingUpgrade, Plan: "standard", OperationID: operationB,
			Requested: ck.t, Expires: ck.t.Add(DefaultPendingTTL),
		},
	}); err != nil {
		t.Fatal(err)
	}
	activationA := durableTestEvent("evt_activation_a", customerID)
	activationA.Type = billing.EventSubscriptionActivated
	activationA.Plan = "standard"
	activationA.OperationID = operationA
	activationA.At = ck.t.Add(time.Minute)
	if err := manager.OnEvents(ctx, "fake", []billing.Event{activationA}); err != nil {
		t.Fatalf("delayed activation A: %v", err)
	}
	afterA, _, _ := store.Get(ctx, accountID)
	if afterA.Entitled != "standard" || afterA.Pending == nil ||
		afterA.Pending.OperationID != operationB {
		t.Fatalf("activation A cleared or changed pending B: %+v", afterA)
	}

	activationB := activationA
	activationB.ProviderEventID = "evt_activation_b"
	activationB.ProviderObjectID = "in_evt_activation_b"
	activationB.OperationID = operationB
	activationB.At = activationA.At.Add(time.Minute)
	if err := manager.OnEvents(ctx, "fake", []billing.Event{activationB}); err != nil {
		t.Fatalf("activation B: %v", err)
	}
	afterB, _, _ := store.Get(ctx, accountID)
	if afterB.Entitled != "standard" || afterB.Pending != nil {
		t.Fatalf("matching activation B did not settle its pending operation: %+v", afterB)
	}
}

func TestR2SaturatedPendingIndexStillProcessesRedelivery(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 16, 0, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	underlying := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	provider := &failOnceResolverProvider{Provider: underlying}
	store := newR2Store(t)
	manager := durableManager(t, store, provider, ck.now)
	const (
		accountID  = "acct_saturated_receipts"
		customerID = "cus_saturated_receipts"
	)
	if err := store.Put(ctx, Record{
		AccountID: accountID, Provider: "fake", CustomerID: customerID,
		Entitled: "standard", Applied: "standard",
	}); err != nil {
		t.Fatal(err)
	}
	eventIDs := make([]string, maxPendingEventReceiptsPerCustomer)
	for i := range eventIDs {
		eventIDs[i] = fmt.Sprintf("evt_existing_%03d", i)
	}
	index := r2PendingEventIndex{
		SchemaVersion: pendingEventIndexSchemaVersion,
		Provider:      "fake",
		CustomerID:    customerID,
		EventIDs:      eventIDs,
		Version:       1,
	}
	encoded, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.c.Put(
		ctx, store.pendingEventIndexKey("fake", customerID), encoded,
		blob.Cond{IfNoneMatchAny: true}); err != nil {
		t.Fatal(err)
	}

	event := durableTestEvent("evt_overflow", customerID)
	if err := manager.OnEvents(ctx, "fake", []billing.Event{event}); err == nil {
		t.Fatal("first induced resolution failure was acknowledged")
	}
	// The receipt object was created even though the bounded accelerator was
	// full. Provider redelivery must reach and synchronously process that same
	// object rather than failing forever while trying to add an index entry.
	if err := manager.OnEvents(ctx, "fake", []billing.Event{event}); err != nil {
		t.Fatalf("redelivery behind saturated index: %v", err)
	}
	receipt, err := newEventReceipt("fake", event, ck.now())
	if err != nil {
		t.Fatal(err)
	}
	stored, created, err := store.ReceiveEvent(ctx, receipt)
	if err != nil || created || stored.Status != EventReceiptProcessed {
		t.Fatalf("overflow receipt = %+v, created=%v, err=%v", stored, created, err)
	}
	record, _, _ := store.Get(ctx, accountID)
	if record.PastDueSince == nil {
		t.Fatalf("redelivered overflow receipt was not folded: %+v", record)
	}
}

func TestR2ReconcileContinuesPastMissingAndPoisonReceipts(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 17, 0, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	provider := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	store := newR2Store(t)
	manager := durableManager(t, store, provider, ck.now)
	const (
		accountID  = "acct_poison_receipt"
		customerID = "cus_poison_receipt"
	)
	if err := store.Put(ctx, Record{
		AccountID: accountID, Provider: "fake", CustomerID: customerID,
		Entitled: "standard", Applied: "standard",
	}); err != nil {
		t.Fatal(err)
	}
	poison := durableTestEvent("evt_poison", customerID)
	poison.Type = billing.EventSubscriptionActivated
	poison.Plan = "not-in-catalog"
	poison.At = ck.t.Add(time.Minute)
	healthy := durableTestEvent("evt_healthy", customerID)
	healthy.At = poison.At.Add(time.Minute)
	for _, event := range []billing.Event{poison, healthy} {
		receipt, err := newEventReceipt("fake", event, ck.now())
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ReceiveEvent(ctx, receipt); err != nil {
			t.Fatal(err)
		}
	}
	indexKey := store.pendingEventIndexKey("fake", customerID)
	data, etag, err := store.c.Get(ctx, indexKey)
	if err != nil {
		t.Fatal(err)
	}
	var index r2PendingEventIndex
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	index.EventIDs = append([]string{"evt_missing"}, index.EventIDs...)
	index.Version++
	data, err = json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.c.Put(ctx, indexKey, data, blob.Cond{IfMatch: etag}); err != nil {
		t.Fatal(err)
	}

	if err := manager.ReconcileAccount(ctx, accountID); err == nil {
		t.Fatal("reconcile hid missing/poison receipt errors")
	}
	record, _, _ := store.Get(ctx, accountID)
	if record.PastDueSince == nil {
		t.Fatalf("healthy receipt behind poison was starved: %+v", record)
	}
	healthyReceipt, err := newEventReceipt("fake", healthy, ck.now())
	if err != nil {
		t.Fatal(err)
	}
	processed, created, err := store.ReceiveEvent(ctx, healthyReceipt)
	if err != nil || created || processed.Status != EventReceiptProcessed {
		t.Fatalf("healthy receipt = %+v, created=%v, err=%v", processed, created, err)
	}
	poisonReceipt, err := newEventReceipt("fake", poison, ck.now())
	if err != nil {
		t.Fatal(err)
	}
	stillPending, created, err := store.ReceiveEvent(ctx, poisonReceipt)
	if err != nil || created || stillPending.Status != EventReceiptPending {
		t.Fatalf("poison receipt should remain retryable: %+v, created=%v, err=%v",
			stillPending, created, err)
	}
}

func TestProviderEventIdentityMapsToAtMostOneNormalizedEvent(t *testing.T) {
	ck := &clock{t: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	provider := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	store := NewMemStore()
	manager := durableManager(t, store, provider, ck.now)
	first := durableTestEvent("evt_ambiguous", "cus_1")
	second := first
	second.Type = billing.EventPaymentRecovered
	if err := manager.OnEvents(
		context.Background(), "fake", []billing.Event{first, second}); err == nil {
		t.Fatal("one provider event identity produced multiple normalized events")
	}
	if pending, err := store.PendingEvents(
		context.Background(), "fake", "cus_1", 10); err != nil || len(pending) != 0 {
		t.Fatalf("invalid batch had side effects: %+v, %v", pending, err)
	}
}

type providerWithoutIdempotentSubscribe struct {
	billing.Provider
}

func TestUpgradeFailsClosedWithoutIdempotentSubscriber(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	underlying := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	provider := &providerWithoutIdempotentSubscribe{Provider: underlying}
	store := NewMemStore()
	manager := durableManager(t, store, provider, ck.now)

	if _, err := manager.RequestUpgrade(
		ctx, "acct_unsafe_provider", "owner@example.com", "standard"); err == nil {
		t.Fatal("upgrade reached a provider without idempotent subscription support")
	}
	if records, err := store.List(ctx); err != nil || len(records) != 0 {
		t.Fatalf("fail-closed provider check parked local state: %+v, %v", records, err)
	}
}

type ambiguousSubscribeProvider struct {
	billing.Provider
	idempotent billing.IdempotentSubscriber
	mu         sync.Mutex
	keys       []string
	failFirst  bool
}

func (p *ambiguousSubscribeProvider) SubscribeIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.Action, error) {
	action, err := p.idempotent.SubscribeIdempotent(
		ctx, customerID, plan, operationID)
	p.mu.Lock()
	p.keys = append(p.keys, operationID)
	fail := p.failFirst
	p.failFirst = false
	p.mu.Unlock()
	if err != nil {
		return billing.Action{}, err
	}
	if fail {
		// The provider committed, but its response was lost.
		return billing.Action{}, context.DeadlineExceeded
	}
	return action, nil
}

func (p *ambiguousSubscribeProvider) callKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.keys...)
}

func TestUpgradeRetryReusesOperationAfterAmbiguousSubscribe(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	underlying := fake.New(fake.Config{
		Prices: catalog.Prices(), Interactive: true, Now: ck.now,
	})
	provider := &ambiguousSubscribeProvider{
		Provider: underlying, idempotent: underlying, failFirst: true,
	}
	store := NewMemStore()
	manager := durableManager(t, store, provider, ck.now)

	if _, err := manager.RequestUpgrade(
		ctx, "acct_ambiguous", "owner@example.com", "standard"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first RequestUpgrade = %v; want ambiguous transport error", err)
	}
	parked, ok, err := store.Get(ctx, "acct_ambiguous")
	if err != nil || !ok || parked.CustomerID == "" || parked.Provider != "fake" ||
		parked.Pending == nil || parked.Pending.OperationID == "" ||
		parked.Pending.CancelPrevious {
		t.Fatalf("ambiguous operation was not durably parked: %+v, ok=%v, err=%v",
			parked, ok, err)
	}
	if routed, ok, err := store.ByCustomer(ctx, "fake", parked.CustomerID); err != nil || !ok || routed.AccountID != parked.AccountID {
		t.Fatalf("customer route was not persisted before Checkout: %+v, %v, %v",
			routed, ok, err)
	}

	out, err := manager.RequestUpgrade(
		ctx, "acct_ambiguous", "owner@example.com", "standard")
	if err != nil || out.Kind != "action" || out.URL == "" {
		t.Fatalf("retry RequestUpgrade = %+v, %v", out, err)
	}
	keys := provider.callKeys()
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] ||
		keys[0] != parked.Pending.OperationID {
		t.Fatalf("Subscribe operation keys = %v; parked=%q", keys, parked.Pending.OperationID)
	}
	// Once the URL is folded, another client retry is answered entirely from
	// durable state and does not touch the provider.
	again, err := manager.RequestUpgrade(
		ctx, "acct_ambiguous", "owner@example.com", "standard")
	if err != nil || again.URL != out.URL || len(provider.callKeys()) != 2 {
		t.Fatalf("durable URL replay = %+v, %v; calls=%v", again, err, provider.callKeys())
	}
}

type cancelOnceProvider struct {
	billing.Provider
	idempotent billing.IdempotentSubscriber
	mu         sync.Mutex
	calls      int
	failures   int
}

func (p *cancelOnceProvider) SubscribeIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.Action, error) {
	return p.idempotent.SubscribeIdempotent(ctx, customerID, plan, operationID)
}

func (p *cancelOnceProvider) ScheduleDowngradeExactIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.ScheduledDowngrade, error) {
	return p.Provider.(billing.ExactIdempotentDowngrader).
		ScheduleDowngradeExactIdempotent(ctx, customerID, plan, operationID)
}

func (p *cancelOnceProvider) CancelPendingObjectIdempotent(
	ctx context.Context,
	customerID string,
	target billing.PendingCancellation,
	operationID string,
) error {
	p.mu.Lock()
	p.calls++
	fail := p.failures > 0
	if fail {
		p.failures--
	}
	p.mu.Unlock()
	if fail {
		return context.DeadlineExceeded
	}
	return p.Provider.(billing.ExactPendingCanceller).
		CancelPendingObjectIdempotent(
			ctx, customerID, target, operationID)
}

func (p *cancelOnceProvider) CancelPending(
	ctx context.Context,
	customerID string,
) error {
	p.mu.Lock()
	p.calls++
	fail := p.failures > 0
	if fail {
		p.failures--
	}
	p.mu.Unlock()
	if fail {
		return context.DeadlineExceeded
	}
	return p.Provider.CancelPending(ctx, customerID)
}

func TestReplacementCancelFailureRetainsPreSubscribePhase(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	underlying := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	provider := &cancelOnceProvider{
		Provider: underlying, idempotent: underlying, failures: 1,
	}
	store := NewMemStore()
	manager := durableManager(t, store, provider, ck.now)

	if _, err := manager.RequestUpgrade(
		ctx, "acct_cancel_phase", "owner@example.com", "standard"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RequestDowngrade(
		ctx, "acct_cancel_phase", "owner@example.com", "free"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RequestUpgrade(
		ctx, "acct_cancel_phase", "owner@example.com", "enterprise"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("replacement cancellation = %v; want transient failure", err)
	}
	parked, _, _ := store.Get(ctx, "acct_cancel_phase")
	if parked.Pending == nil || parked.Pending.Kind != PendingContact ||
		parked.Pending.OperationID == "" || !parked.Pending.CancelPrevious {
		t.Fatalf("pre-subscribe cancellation phase was lost: %+v", parked.Pending)
	}
	if out, err := manager.RequestUpgrade(
		ctx, "acct_cancel_phase", "owner@example.com", "enterprise"); err != nil || out.Kind != "contact" {
		t.Fatalf("replacement retry = %+v, %v", out, err)
	}
	parked, _, _ = store.Get(ctx, "acct_cancel_phase")
	if parked.Pending == nil || parked.Pending.CancelPrevious {
		t.Fatalf("successful cancellation did not clear phase: %+v", parked.Pending)
	}
	ck.t = ck.t.AddDate(0, 0, 31)
	if events := underlying.ApplyDue(); len(events) != 0 {
		t.Fatalf("provider-side downgrade remained armed: %+v", events)
	}
}

func TestReplacementSupersessionPreservesPendingCleanup(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	underlying := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	provider := &cancelOnceProvider{
		Provider: underlying, idempotent: underlying, failures: 2,
	}
	store := NewMemStore()
	manager := durableManager(t, store, provider, ck.now)

	if _, err := manager.RequestUpgrade(
		ctx, "acct_cancel_supersede", "owner@example.com", "standard"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RequestDowngrade(
		ctx, "acct_cancel_supersede", "owner@example.com", "free"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RequestUpgrade(
		ctx, "acct_cancel_supersede", "owner@example.com", "enterprise"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first cleanup = %v", err)
	}
	first, _, _ := store.Get(ctx, "acct_cancel_supersede")
	if first.Pending == nil || !first.Pending.CancelPrevious {
		t.Fatalf("first cleanup fence missing: %+v", first.Pending)
	}
	if _, err := manager.RequestUpgrade(
		ctx, "acct_cancel_supersede", "owner@example.com", "team"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("superseding cleanup = %v", err)
	}
	second, _, _ := store.Get(ctx, "acct_cancel_supersede")
	if second.Pending == nil || second.Pending.Plan != "team" ||
		!second.Pending.CancelPrevious ||
		second.Pending.OperationID == first.Pending.OperationID {
		t.Fatalf("superseding claim lost or reused cleanup identity: first=%+v second=%+v",
			first.Pending, second.Pending)
	}
	if out, err := manager.RequestUpgrade(
		ctx, "acct_cancel_supersede", "owner@example.com", "team"); err != nil || out.Kind != "contact" {
		t.Fatalf("final cleanup retry = %+v, %v", out, err)
	}
	ck.t = ck.t.AddDate(0, 0, 31)
	if events := underlying.ApplyDue(); len(events) != 0 {
		t.Fatalf("provider-side downgrade remained armed: %+v", events)
	}
}

func TestReconcileRetriesContactReplacementCleanup(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	underlying := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	provider := &cancelOnceProvider{
		Provider: underlying, idempotent: underlying, failures: 1,
	}
	store := NewMemStore()
	manager := durableManager(t, store, provider, ck.now)

	if _, err := manager.RequestUpgrade(
		ctx, "acct_reconcile_contact", "owner@example.com", "standard"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RequestDowngrade(
		ctx, "acct_reconcile_contact", "owner@example.com", "free"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RequestUpgrade(
		ctx, "acct_reconcile_contact", "owner@example.com", "enterprise"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("replacement cancellation = %v; want transient failure", err)
	}
	if err := manager.ReconcileAccount(ctx, "acct_reconcile_contact"); err != nil {
		t.Fatalf("ReconcileAccount cleanup: %v", err)
	}
	got, _, _ := store.Get(ctx, "acct_reconcile_contact")
	if got.Pending == nil || got.Pending.Kind != PendingContact || got.Pending.CancelPrevious {
		t.Fatalf("reconciled contact cleanup = %+v", got.Pending)
	}
	ck.t = ck.t.AddDate(0, 0, 31)
	if events := underlying.ApplyDue(); len(events) != 0 {
		t.Fatalf("reconcile left provider-side downgrade armed: %+v", events)
	}
}

func TestReconcileCleanupFailureRetainsContactFence(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	underlying := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	provider := &cancelOnceProvider{
		Provider: underlying, idempotent: underlying, failures: 2,
	}
	store := NewMemStore()
	manager := durableManager(t, store, provider, ck.now)

	if _, err := manager.RequestUpgrade(
		ctx, "acct_reconcile_failure", "owner@example.com", "standard"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RequestDowngrade(
		ctx, "acct_reconcile_failure", "owner@example.com", "free"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RequestUpgrade(
		ctx, "acct_reconcile_failure", "owner@example.com", "enterprise"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("replacement cancellation = %v; want transient failure", err)
	}
	if err := manager.ReconcileAccount(ctx, "acct_reconcile_failure"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failed reconcile cleanup = %v; want transient failure", err)
	}
	got, _, _ := store.Get(ctx, "acct_reconcile_failure")
	if got.Pending == nil || got.Pending.Kind != PendingContact ||
		!got.Pending.CancelPrevious {
		t.Fatalf("failed reconcile lost cleanup fence: %+v", got.Pending)
	}
	// The same durable marker remains sufficient for the next pass to finish.
	if err := manager.ReconcileAccount(ctx, "acct_reconcile_failure"); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	got, _, _ = store.Get(ctx, "acct_reconcile_failure")
	if got.Pending == nil || got.Pending.CancelPrevious {
		t.Fatalf("cleanup retry did not clear exact fence: %+v", got.Pending)
	}
	ck.t = ck.t.AddDate(0, 0, 31)
	if events := underlying.ApplyDue(); len(events) != 0 {
		t.Fatalf("cleanup retry left provider work armed: %+v", events)
	}
}

func TestCancelPendingHonorsContactReplacementCleanup(t *testing.T) {
	ctx := context.Background()
	ck := &clock{t: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	catalog, _ := plans.Load()
	underlying := fake.New(fake.Config{Prices: catalog.Prices(), Now: ck.now})
	provider := &cancelOnceProvider{
		Provider: underlying, idempotent: underlying, failures: 1,
	}
	store := NewMemStore()
	manager := durableManager(t, store, provider, ck.now)

	if _, err := manager.RequestUpgrade(
		ctx, "acct_cancel_contact", "owner@example.com", "standard"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RequestDowngrade(
		ctx, "acct_cancel_contact", "owner@example.com", "free"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RequestUpgrade(
		ctx, "acct_cancel_contact", "owner@example.com", "enterprise"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("replacement cancellation = %v; want transient failure", err)
	}
	if err := manager.CancelPending(ctx, "acct_cancel_contact"); err != nil {
		t.Fatalf("CancelPending cleanup: %v", err)
	}
	got, _, _ := store.Get(ctx, "acct_cancel_contact")
	if got.Pending != nil {
		t.Fatalf("canceled contact remains pending: %+v", got.Pending)
	}
	ck.t = ck.t.AddDate(0, 0, 31)
	if events := underlying.ApplyDue(); len(events) != 0 {
		t.Fatalf("cancel left provider-side downgrade armed: %+v", events)
	}
}
