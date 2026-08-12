package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	witself "github.com/witwave-ai/witself"
	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/billing/stripe"
	"github.com/witwave-ai/witself/internal/plans"
)

func newBillingMutationManager(
	t *testing.T,
	interactive bool,
) (*Manager, *MemStore, *clock) {
	t.Helper()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	clock := &clock{t: time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)}
	provider := fake.New(fake.Config{
		Prices: catalog.Prices(), Interactive: interactive, Now: clock.now,
	})
	store := NewMemStore()
	manager, err := NewManager(Config{
		Catalog: catalog,
		Providers: map[string]billing.Provider{
			"fake": provider,
		},
		Default: "fake", Store: store, Applier: &recApplier{}, Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, store, clock
}

func TestBillingMutationPreviewIsWriteFree(t *testing.T) {
	manager, store, _ := newBillingMutationManager(t, true)
	preview, err := manager.PreviewBillingMutation(
		context.Background(), "acct_preview", "owner@example.com",
		BillingMutationCommand{
			Operation: BillingMutationSetup,
			Reason:    "Add a payment method before upgrading",
		})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Allowed || !preview.ConfirmationRequired ||
		len(preview.Effects) == 0 || len(preview.Violations) != 0 {
		t.Fatalf("preview = %+v", preview)
	}
	if records, err := store.List(context.Background()); err != nil || len(records) != 0 {
		t.Fatalf("preview records = %d, %v; want none", len(records), err)
	}
	operationID := billingOperationIDForTest("acct_preview", strings.Repeat("k", 16))
	if _, ok, err := store.GetBillingMutation(context.Background(), operationID); err != nil || ok {
		t.Fatalf("preview receipt exists=%t err=%v; want none", ok, err)
	}
}

func TestBillingMutationSetupExactReplayAndConflict(t *testing.T) {
	manager, store, clock := newBillingMutationManager(t, true)
	ctx := context.Background()
	command := BillingMutationCommand{
		Operation:      BillingMutationSetup,
		Reason:         "Configure the account payment method",
		Confirmed:      true,
		IdempotencyKey: "setup-2026-08-11-0001",
	}
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}

	first, err := manager.ExecuteBillingMutation(
		ctx, "acct_setup", "owner@example.com", actor, command)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Outcome.Kind != "action" ||
		first.OperationID == "" || first.Actor != actor || !first.Confirmed {
		t.Fatalf("first = %+v", first)
	}
	clock.t = clock.t.Add(time.Hour)
	second, err := manager.ExecuteBillingMutation(
		ctx, "acct_setup", "owner@example.com", actor, command)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.OperationID != first.OperationID ||
		second.Outcome.URL != first.Outcome.URL {
		t.Fatalf("replay = %+v; first = %+v", second, first)
	}

	receipt, ok, err := store.GetBillingMutation(ctx, first.OperationID)
	if err != nil || !ok || receipt.Status != BillingMutationCompleted ||
		receipt.ClaimToken != "" || receipt.Result == nil {
		t.Fatalf("receipt = %+v ok=%t err=%v", receipt, ok, err)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), command.IdempotencyKey) ||
		strings.Contains(string(encoded), "owner@example.com") {
		t.Fatalf("receipt leaked raw retry key or email: %s", encoded)
	}

	changed := command
	changed.Reason = "A changed reason must not reuse the same key"
	if _, err := manager.ExecuteBillingMutation(
		ctx, "acct_setup", "owner@example.com", actor, changed,
	); !errors.Is(err, ErrBillingMutationConflict) {
		t.Fatalf("changed replay error = %v; want conflict", err)
	}
}

func TestBillingMutationRequiresReasonConfirmationAndRetryKey(t *testing.T) {
	manager, _, _ := newBillingMutationManager(t, false)
	base := BillingMutationCommand{
		Operation:      BillingMutationSetup,
		Reason:         "Set up billing",
		Confirmed:      true,
		IdempotencyKey: "setup-2026-08-11-0002",
	}
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	tests := []struct {
		name string
		edit func(*BillingMutationCommand)
	}{
		{"reason", func(c *BillingMutationCommand) { c.Reason = "" }},
		{"confirmation", func(c *BillingMutationCommand) { c.Confirmed = false }},
		{"retry key", func(c *BillingMutationCommand) { c.IdempotencyKey = "" }},
		{"multiline reason", func(c *BillingMutationCommand) { c.Reason = "line one\nline two" }},
		{"bidi reason", func(c *BillingMutationCommand) { c.Reason = "reason \u202espoof" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := base
			tt.edit(&command)
			_, err := manager.ExecuteBillingMutation(
				context.Background(), "acct_input", "", actor, command)
			if !errors.Is(err, ErrBillingMutationInput) {
				t.Fatalf("error = %v; want ErrBillingMutationInput", err)
			}
		})
	}
}

func TestBillingMutationUpgradeDowngradeAndCancel(t *testing.T) {
	ctx := context.Background()
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}

	// Headless purchase followed by an idempotently scheduled downgrade.
	manager, _, _ := newBillingMutationManager(t, false)
	upgrade, err := manager.ExecuteBillingMutation(ctx, "acct_plan", "", actor,
		BillingMutationCommand{
			Operation: BillingMutationUpgrade, Plan: "standard",
			Reason: "Move to Professional", Confirmed: true,
			IdempotencyKey: "upgrade-2026-08-11-0001",
		})
	if err != nil || upgrade.Outcome.Kind != "done" || upgrade.Outcome.Plan != "standard" {
		t.Fatalf("upgrade = %+v, %v", upgrade, err)
	}
	downgrade, err := manager.ExecuteBillingMutation(ctx, "acct_plan", "", actor,
		BillingMutationCommand{
			Operation: BillingMutationDowngrade, Plan: "free",
			Reason: "Return to Personal at period end", Confirmed: true,
			IdempotencyKey: "downgrade-2026-08-11-0001",
		})
	if err != nil || downgrade.Outcome.Kind != "scheduled" ||
		downgrade.Outcome.Plan != "free" || downgrade.Outcome.Effective.IsZero() {
		t.Fatalf("downgrade = %+v, %v", downgrade, err)
	}

	// Interactive checkout remains pending and can be cancelled under its own
	// durable operation identity.
	interactive, _, _ := newBillingMutationManager(t, true)
	pending, err := interactive.ExecuteBillingMutation(ctx, "acct_cancel", "", actor,
		BillingMutationCommand{
			Operation: BillingMutationUpgrade, Plan: "standard",
			Reason: "Start a Professional checkout", Confirmed: true,
			IdempotencyKey: "upgrade-2026-08-11-0002",
		})
	if err != nil || pending.Outcome.Kind != "action" {
		t.Fatalf("pending upgrade = %+v, %v", pending, err)
	}
	cancelled, err := interactive.ExecuteBillingMutation(ctx, "acct_cancel", "", actor,
		BillingMutationCommand{
			Operation: BillingMutationCancel,
			Reason:    "Cancel the unfinished checkout", Confirmed: true,
			IdempotencyKey: "cancel-2026-08-11-00001",
		})
	if err != nil || cancelled.Outcome.Kind != "cancelled" {
		t.Fatalf("cancel = %+v, %v", cancelled, err)
	}
}

type blockingBillingProvider struct {
	*fake.Fake
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (p *blockingBillingProvider) SubscribeIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.Action, error) {
	blocked := false
	p.once.Do(func() {
		blocked = true
		close(p.entered)
	})
	if blocked {
		select {
		case <-p.release:
		case <-ctx.Done():
			return billing.Action{}, ctx.Err()
		}
	}
	return p.Fake.SubscribeIdempotent(ctx, customerID, plan, operationID)
}

type failFirstBillingProvider struct {
	*fake.Fake
	mu     sync.Mutex
	failed bool
	calls  int
}

func (p *failFirstBillingProvider) SubscribeIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.Action, error) {
	p.mu.Lock()
	p.calls++
	if !p.failed {
		p.failed = true
		p.mu.Unlock()
		return billing.Action{}, context.DeadlineExceeded
	}
	p.mu.Unlock()
	return p.Fake.SubscribeIdempotent(ctx, customerID, plan, operationID)
}

func (p *failFirstBillingProvider) subscribeCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type countingUpgradeBillingProvider struct {
	*fake.Fake
	mu             sync.Mutex
	ensureCalls    int
	subscribeCalls int
}

func (p *countingUpgradeBillingProvider) EnsureCustomer(
	ctx context.Context,
	accountID, email string,
) (string, error) {
	p.mu.Lock()
	p.ensureCalls++
	p.mu.Unlock()
	return p.Fake.EnsureCustomer(ctx, accountID, email)
}

func (p *countingUpgradeBillingProvider) SubscribeIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.Action, error) {
	p.mu.Lock()
	p.subscribeCalls++
	p.mu.Unlock()
	return p.Fake.SubscribeIdempotent(ctx, customerID, plan, operationID)
}

func (p *countingUpgradeBillingProvider) upgradeMutationCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ensureCalls + p.subscribeCalls
}

type mutableBillingFit struct {
	mu         sync.Mutex
	violations []string
}

func (f *mutableBillingFit) Fit(
	_ context.Context,
	_ string,
	_ plans.Plan,
) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.violations...), nil
}

func (f *mutableBillingFit) set(violations ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.violations = append([]string(nil), violations...)
}

// ambiguousDowngradeBillingProvider models the important failure boundary:
// the provider commits its idempotent mutation but the control plane loses the
// response. An exact retry must replay the provider operation without
// reclassifying it against mutable catalog or fit inputs.
type ambiguousDowngradeBillingProvider struct {
	*fake.Fake
	mu     sync.Mutex
	failed bool
	calls  int
}

func (p *ambiguousDowngradeBillingProvider) ScheduleDowngradeIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (time.Time, error) {
	effective, err := p.Fake.ScheduleDowngradeIdempotent(
		ctx, customerID, plan, operationID)
	if err != nil {
		return time.Time{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if !p.failed {
		p.failed = true
		return time.Time{}, context.DeadlineExceeded
	}
	return effective, nil
}

func (p *ambiguousDowngradeBillingProvider) downgradeCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// resolveDuringCancelBillingProvider simulates a webhook folding newer truth
// after the provider has been disarmed but before the cancel CAS runs.
type resolveDuringCancelBillingProvider struct {
	*fake.Fake
	store     *MemStore
	accountID string
	now       func() time.Time
	replace   *Pending
	mu        sync.Mutex
	calls     int
	once      sync.Once
	err       error
}

func (p *resolveDuringCancelBillingProvider) CancelPendingIdempotent(
	ctx context.Context,
	customerID, operationID string,
) error {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if err := p.Fake.CancelPendingIdempotent(ctx, customerID, operationID); err != nil {
		return err
	}
	p.once.Do(func() {
		record, ok, err := p.store.Get(ctx, p.accountID)
		if err != nil {
			p.err = err
			return
		}
		if !ok {
			p.err = errors.New("cancel race fixture: account record disappeared")
			return
		}
		if p.replace == nil {
			record.Pending = nil
		} else {
			replacement := *p.replace
			record.Pending = &replacement
		}
		record.Entitled = "standard"
		record.Applied = "standard"
		record.EntitledAt = p.now().UTC()
		p.err = p.store.Put(ctx, record)
	})
	return p.err
}

func (p *resolveDuringCancelBillingProvider) cancelCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

var errForcedBillingMutationCompletion = errors.New(
	"billing mutation completion response was lost")

type failNextBillingCompletionStore struct {
	*MemStore
	mu       sync.Mutex
	failNext bool
}

func (s *failNextBillingCompletionStore) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = true
}

func (s *failNextBillingCompletionStore) CompleteBillingMutation(
	ctx context.Context,
	receipt BillingMutationReceipt,
	result BillingMutationResult,
	completedAt time.Time,
) (BillingMutationReceipt, error) {
	s.mu.Lock()
	if s.failNext {
		s.failNext = false
		s.mu.Unlock()
		return BillingMutationReceipt{}, errForcedBillingMutationCompletion
	}
	s.mu.Unlock()
	return s.MemStore.CompleteBillingMutation(ctx, receipt, result, completedAt)
}

type delayedBillingReceiptStore struct {
	*MemStore
	operationID string
	once        sync.Once
	entered     chan struct{}
	release     chan struct{}
}

func (s *delayedBillingReceiptStore) ReceiveBillingMutation(
	ctx context.Context,
	receipt BillingMutationReceipt,
) (BillingMutationReceipt, bool, error) {
	if receipt.OperationID == s.operationID {
		blocked := false
		s.once.Do(func() {
			blocked = true
			close(s.entered)
		})
		if blocked {
			select {
			case <-s.release:
			case <-ctx.Done():
				return BillingMutationReceipt{}, false, ctx.Err()
			}
		}
	}
	return s.MemStore.ReceiveBillingMutation(ctx, receipt)
}

type recordingSetupBillingProvider struct {
	*fake.Fake
	mu           sync.Mutex
	operationIDs []string
}

func (p *recordingSetupBillingProvider) SetupLinkIdempotent(
	ctx context.Context,
	customerID, operationID string,
) (billing.Action, error) {
	p.mu.Lock()
	p.operationIDs = append(p.operationIDs, operationID)
	p.mu.Unlock()
	return p.Fake.SetupLinkIdempotent(ctx, customerID, operationID)
}

func (p *recordingSetupBillingProvider) setupOperationIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.operationIDs...)
}

func newBillingMutationManagerWithProvider(
	t *testing.T,
	catalog *plans.Catalog,
	store Store,
	provider billing.Provider,
	clock *clock,
) *Manager {
	t.Helper()
	manager, err := NewManager(Config{
		Catalog: catalog,
		Providers: map[string]billing.Provider{
			"billing": provider,
		},
		Default: "billing", Store: store,
		Applier: &recApplier{}, Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func seedPendingBillingMutationForTest(
	t *testing.T,
	store BillingMutationStore,
	schemaVersion int,
	accountID, email string,
	actor BillingActor,
	command BillingMutationCommand,
	approval billingMutationApproval,
	now time.Time,
) BillingMutationReceipt {
	t.Helper()
	ctx := context.Background()
	operationID := billingMutationOperationID(accountID, command.IdempotencyKey)
	claimToken := "bcl_seed_" + strings.TrimPrefix(operationID, "bop_")
	lease, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, operationID, 0, claimToken,
		now, now.Add(billingMutationClaimLease))
	if err != nil || !acquired {
		t.Fatalf("claim seed account lane = %+v acquired=%v err=%v",
			lease, acquired, err)
	}
	receipt, err := buildBillingMutationReceiptForSchema(
		schemaVersion, accountID, email, actor, command,
		lease.OperationGeneration, approval, now)
	if err != nil {
		t.Fatal(err)
	}
	stored, created, err := store.ReceiveBillingMutation(ctx, receipt)
	if err != nil || !created {
		t.Fatalf("receive seeded billing mutation = %+v created=%v err=%v",
			stored, created, err)
	}
	if err := store.ReleaseBillingMutationAccount(ctx, lease, now); err != nil {
		t.Fatalf("release seed account lane: %v", err)
	}
	return stored
}

func TestBillingMutationAccountLaneSerializesUpgradeAndCancel(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	clock := &clock{t: time.Date(2026, 8, 11, 14, 0, 0, 0, time.UTC)}
	base := fake.New(fake.Config{
		Prices: catalog.Prices(), Interactive: true, Now: clock.now,
	})
	provider := &blockingBillingProvider{
		Fake: base, entered: make(chan struct{}), release: make(chan struct{}),
	}
	store := NewMemStore()
	upgradeManager := newBillingMutationManagerWithProvider(
		t, catalog, store, provider, clock)
	cancelManager := newBillingMutationManagerWithProvider(
		t, catalog, store, provider, clock)
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	upgradeCommand := BillingMutationCommand{
		Operation: BillingMutationUpgrade, Plan: "standard",
		Reason: "Start the Professional checkout", Confirmed: true,
		IdempotencyKey: "lane-upgrade-2026-08-11-0001",
	}
	type result struct {
		execution BillingMutationExecution
		err       error
	}
	upgradeDone := make(chan result, 1)
	go func() {
		execution, executeErr := upgradeManager.ExecuteBillingMutation(
			context.Background(), "acct_lane", "", actor, upgradeCommand)
		upgradeDone <- result{execution: execution, err: executeErr}
	}()
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upgrade did not reach the blocked provider call")
	}

	cancelCommand := BillingMutationCommand{
		Operation: BillingMutationCancel,
		Reason:    "Cancel the pending checkout", Confirmed: true,
		IdempotencyKey: "lane-cancel-2026-08-11-00001",
	}
	if _, err := cancelManager.ExecuteBillingMutation(
		context.Background(), "acct_lane", "", actor, cancelCommand,
	); !errors.Is(err, ErrBillingMutationInProgress) {
		t.Fatalf("concurrent cancel error = %v; want in progress", err)
	}
	cancelOperationID := billingMutationOperationID(
		"acct_lane", cancelCommand.IdempotencyKey)
	if _, exists, err := store.GetBillingMutation(
		context.Background(), cancelOperationID); err != nil || exists {
		t.Fatalf("blocked cancel receipt exists=%t err=%v; want no receipt", exists, err)
	}

	close(provider.release)
	first := <-upgradeDone
	if first.err != nil || first.execution.Outcome.Kind != "action" {
		t.Fatalf("upgrade = %+v, %v", first.execution, first.err)
	}
	cancelled, err := cancelManager.ExecuteBillingMutation(
		context.Background(), "acct_lane", "", actor, cancelCommand)
	if err != nil || cancelled.Outcome.Kind != "cancelled" {
		t.Fatalf("serialized cancel = %+v, %v", cancelled, err)
	}
}

func TestBillingMutationLateReceiptRevalidatesLaneBeforeProvider(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	clock := &clock{t: time.Date(2026, 8, 11, 14, 30, 0, 0, time.UTC)}
	base := fake.New(fake.Config{
		Prices: catalog.Prices(), Interactive: true, Now: clock.now,
	})
	provider := &recordingSetupBillingProvider{Fake: base}
	accountID := "acct_late_receipt_handshake"
	ctx := context.Background()
	customerID, err := base.EnsureCustomer(ctx, accountID, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemStore()
	if err := mem.Put(ctx, Record{
		AccountID: accountID, Provider: "billing", CustomerID: customerID,
		Entitled: plans.Free, Applied: plans.Free,
	}); err != nil {
		t.Fatal(err)
	}
	first := BillingMutationCommand{
		Operation: BillingMutationSetup,
		Reason:    "Exercise a delayed first receipt", Confirmed: true,
		IdempotencyKey: "late-receipt-first-0001",
	}
	firstOperationID := billingMutationOperationID(accountID, first.IdempotencyKey)
	store := &delayedBillingReceiptStore{
		MemStore: mem, operationID: firstOperationID,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	manager := newBillingMutationManagerWithProvider(
		t, catalog, store, provider, clock)
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}

	firstResult := make(chan error, 1)
	go func() {
		_, err := manager.ExecuteBillingMutation(
			ctx, accountID, "owner@example.com", actor, first)
		firstResult <- err
	}()
	<-store.entered
	clock.t = clock.t.Add(billingMutationClaimLease + time.Minute)

	second := BillingMutationCommand{
		Operation: BillingMutationSetup,
		Reason:    "Take over an expired receipt-less lane", Confirmed: true,
		IdempotencyKey: "late-receipt-second-0002",
	}
	secondOperationID := billingMutationOperationID(accountID, second.IdempotencyKey)
	secondExecution, err := manager.ExecuteBillingMutation(
		ctx, accountID, "owner@example.com", actor, second)
	if err != nil || secondExecution.OperationID != secondOperationID ||
		secondExecution.Outcome.Kind != "action" {
		close(store.release)
		t.Fatalf("receipt-less successor = %+v err=%v", secondExecution, err)
	}
	close(store.release)
	if err := <-firstResult; !errors.Is(err, ErrBillingMutationSuperseded) {
		t.Fatalf("late first operation error = %v; want superseded", err)
	}

	if calls := provider.setupOperationIDs(); len(calls) != 1 || calls[0] != secondOperationID {
		t.Fatalf("provider setup operations = %v; want only %s",
			calls, secondOperationID)
	}
	late, ok, err := store.GetBillingMutation(ctx, firstOperationID)
	if err != nil || !ok || late.Status != BillingMutationSuperseded ||
		late.SupersededByOperationID != secondOperationID {
		t.Fatalf("late receipt = %+v ok=%v err=%v", late, ok, err)
	}
}

func TestBillingMutationPendingReceiptReservesAccountUntilExactRetry(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	clock := &clock{t: time.Date(2026, 8, 11, 15, 0, 0, 0, time.UTC)}
	base := fake.New(fake.Config{
		Prices: catalog.Prices(), Interactive: true, Now: clock.now,
	})
	provider := &failFirstBillingProvider{Fake: base}
	store := NewMemStore()
	manager := newBillingMutationManagerWithProvider(
		t, catalog, store, provider, clock)
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	upgradeCommand := BillingMutationCommand{
		Operation: BillingMutationUpgrade, Plan: "standard",
		Reason:         "Start a checkout with an ambiguous first attempt",
		Confirmed:      true,
		IdempotencyKey: "supersede-upgrade-2026-08-11-1",
	}
	if _, err := manager.ExecuteBillingMutation(
		context.Background(), "acct_supersede", "owner-old@example.com", actor, upgradeCommand,
	); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first upgrade error = %v; want deadline", err)
	}

	cancelCommand := BillingMutationCommand{
		Operation: BillingMutationCancel,
		Reason:    "Cancel the retried checkout intent", Confirmed: true,
		IdempotencyKey: "supersede-cancel-2026-08-11-01",
	}
	if _, err := manager.ExecuteBillingMutation(
		context.Background(), "acct_supersede", "", actor, cancelCommand,
	); !errors.Is(err, ErrBillingMutationInProgress) {
		t.Fatalf("cancel during ambiguous upgrade = %v; want in progress", err)
	}
	cancelOperationID := billingMutationOperationID(
		"acct_supersede", cancelCommand.IdempotencyKey)
	if _, exists, err := store.GetBillingMutation(
		context.Background(), cancelOperationID); err != nil || exists {
		t.Fatalf("blocked cancel receipt exists=%t err=%v; want no receipt", exists, err)
	}

	rotatedActor := BillingActor{ID: "usr_billing_2", Role: "billing"}
	upgrade, err := manager.ExecuteBillingMutation(
		context.Background(), "acct_supersede", "owner-new@example.com", rotatedActor, upgradeCommand,
	)
	if err != nil || upgrade.Outcome.Kind != "action" || !upgrade.Replayed {
		t.Fatalf("exact upgrade recovery = %+v, %v", upgrade, err)
	}
	if upgrade.Actor != actor {
		t.Fatalf("recovered actor = %+v; want immutable initiator %+v", upgrade.Actor, actor)
	}
	cancelled, err := manager.ExecuteBillingMutation(
		context.Background(), "acct_supersede", "", actor, cancelCommand)
	if err != nil || cancelled.Outcome.Kind != "cancelled" {
		t.Fatalf("cancel after upgrade completion = %+v, %v", cancelled, err)
	}
	replayed, err := manager.ExecuteBillingMutation(
		context.Background(), "acct_supersede", "", actor, upgradeCommand)
	if err != nil || !replayed.Replayed ||
		replayed.OperationID != upgrade.OperationID ||
		replayed.Outcome.Kind != "action" {
		t.Fatalf("completed upgrade replay = %+v, %v", replayed, err)
	}
	operationID := billingMutationOperationID(
		"acct_supersede", upgradeCommand.IdempotencyKey)
	receipt, ok, err := store.GetBillingMutation(
		context.Background(), operationID)
	if err != nil || !ok || receipt.Status != BillingMutationCompleted {
		t.Fatalf("completed receipt = %+v ok=%t err=%v", receipt, ok, err)
	}
}

func TestReconcileBillingMutationsAutonomouslyRetriesPendingProviderWork(
	t *testing.T,
) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	clock := &clock{t: time.Date(2026, 8, 11, 15, 30, 0, 0, time.UTC)}
	base := fake.New(fake.Config{
		Prices: catalog.Prices(), Interactive: true, Now: clock.now,
	})
	provider := &failFirstBillingProvider{Fake: base}
	store := NewMemStore()
	manager := newBillingMutationManagerWithProvider(
		t, catalog, store, provider, clock)
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	command := BillingMutationCommand{
		Operation: BillingMutationUpgrade, Plan: "standard",
		Reason:         "Recover an ambiguous Professional checkout without user input",
		Confirmed:      true,
		IdempotencyKey: "autonomous-reconcile-upgrade-0001",
	}
	ctx := context.Background()
	if _, err := manager.ExecuteBillingMutation(
		ctx, "acct_autonomous_reconcile", "owner@example.com", actor, command,
	); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first upgrade error = %v; want deadline", err)
	}
	operationID := billingMutationOperationID(
		"acct_autonomous_reconcile", command.IdempotencyKey)
	pending, ok, err := store.GetBillingMutation(ctx, operationID)
	if err != nil || !ok || pending.Status != BillingMutationPending {
		t.Fatalf("pending autonomous receipt = %+v ok=%v err=%v",
			pending, ok, err)
	}
	if provider.subscribeCalls() != 1 {
		t.Fatalf("provider calls before reconcile = %d; want 1",
			provider.subscribeCalls())
	}

	// The request path released both claims after the transient provider error.
	// The global reconciler needs no account-directory scan or raw idempotency key
	// to claim the indexed receipt and replay its exact provider operation.
	clock.t = clock.t.Add(time.Minute)
	summary, err := manager.ReconcileBillingMutations(ctx)
	if err != nil || summary.Scanned != 1 || summary.Attempted != 1 ||
		summary.Completed != 1 || summary.Superseded != 0 ||
		summary.Busy != 0 || summary.Failed != 0 ||
		summary.TerminalCleaned != 0 || summary.ScanCapped ||
		summary.OldestObservedPendingAt == nil ||
		!summary.OldestObservedPendingAt.Equal(pending.CreatedAt) {
		t.Fatalf("autonomous reconcile summary = %+v err=%v", summary, err)
	}
	if provider.subscribeCalls() != 2 {
		t.Fatalf("provider calls after reconcile = %d; want exact retry",
			provider.subscribeCalls())
	}
	completed, ok, err := store.GetBillingMutation(ctx, operationID)
	if err != nil || !ok || completed.Status != BillingMutationCompleted ||
		completed.Result == nil ||
		completed.Result.Kind != BillingMutationResultAction {
		t.Fatalf("autonomously completed receipt = %+v ok=%v err=%v",
			completed, ok, err)
	}
	empty, err := store.PendingBillingMutations(
		ctx, billingMutationPendingShardCount)
	if err != nil || empty.Scanned != 0 || len(empty.Receipts) != 0 {
		t.Fatalf("pending batch after autonomous reconcile = %+v err=%v",
			empty, err)
	}
}

func TestReconcileBillingMutationKeepsContactApprovalAfterAvailabilityFlip(
	t *testing.T,
) {
	initialCatalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	team, ok := initialCatalog.Get("team")
	if !ok || team.Available {
		t.Fatalf("initial Team fixture = %+v ok=%v; want unavailable", team, ok)
	}
	now := time.Date(2026, 8, 11, 15, 40, 0, 0, time.UTC)
	clock := &clock{t: now}
	store := NewMemStore()
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	command := BillingMutationCommand{
		Operation: BillingMutationUpgrade, Plan: "team",
		Reason:         "Ask about moving to Team",
		Confirmed:      true,
		IdempotencyKey: "contact-availability-flip-0001",
	}
	seeded := seedPendingBillingMutationForTest(
		t, store, billingMutationReceiptSchemaVersion,
		"acct_contact_flip", "owner@example.com", actor, command,
		billingMutationApproval{
			ExecutionClass: BillingMutationExecutionUpgradeContact,
		}, now)
	if seeded.ExecutionClass != BillingMutationExecutionUpgradeContact {
		t.Fatalf("seeded execution class = %q; want contact", seeded.ExecutionClass)
	}

	flippedCatalog := catalogWithPlanAvailability(t, "team", true)
	flippedTeam, ok := flippedCatalog.Get("team")
	if !ok || !flippedTeam.Purchasable() {
		t.Fatalf("flipped Team fixture = %+v ok=%v; want self-serve", flippedTeam, ok)
	}
	provider := &countingUpgradeBillingProvider{Fake: fake.New(fake.Config{
		Prices: flippedCatalog.Prices(), Interactive: true, Now: clock.now,
	})}
	manager := newBillingMutationManagerWithProvider(
		t, flippedCatalog, store, provider, clock)

	summary, err := manager.ReconcileBillingMutations(context.Background())
	if err != nil || summary.Attempted != 1 || summary.Completed != 1 ||
		summary.Failed != 0 {
		t.Fatalf("contact reconcile summary = %+v err=%v", summary, err)
	}
	if calls := provider.upgradeMutationCalls(); calls != 0 {
		t.Fatalf("provider upgrade mutation calls = %d; want zero", calls)
	}
	record, ok, err := store.Get(context.Background(), seeded.AccountID)
	if err != nil || !ok || record.Pending == nil ||
		record.Pending.Kind != PendingContact ||
		record.Pending.Plan != command.Plan ||
		record.Pending.OperationID != seeded.OperationID {
		t.Fatalf("contact account fold = %+v ok=%v err=%v", record, ok, err)
	}
	completed, ok, err := store.GetBillingMutation(
		context.Background(), seeded.OperationID)
	if err != nil || !ok || completed.Status != BillingMutationCompleted ||
		completed.ExecutionClass != BillingMutationExecutionUpgradeContact ||
		completed.Result == nil ||
		completed.Result.Kind != BillingMutationResultContact {
		t.Fatalf("completed contact receipt = %+v ok=%v err=%v",
			completed, ok, err)
	}
}

func TestReconcileBillingMutationSelfServePriceDriftFailsClosed(
	t *testing.T,
) {
	initialCatalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	standard, ok := initialCatalog.Get("standard")
	if !ok || !standard.Purchasable() {
		t.Fatalf("initial Professional fixture = %+v ok=%v", standard, ok)
	}
	now := time.Date(2026, 8, 11, 15, 45, 0, 0, time.UTC)
	clock := &clock{t: now}
	store := NewMemStore()
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	command := BillingMutationCommand{
		Operation: BillingMutationUpgrade, Plan: "standard",
		Reason:         "Move to Professional at the approved price",
		Confirmed:      true,
		IdempotencyKey: "self-serve-price-drift-0001",
	}
	seeded := seedPendingBillingMutationForTest(
		t, store, billingMutationReceiptSchemaVersion,
		"acct_price_drift", "owner@example.com", actor, command,
		billingMutationApproval{
			ExecutionClass:     BillingMutationExecutionUpgradeSelfServe,
			ApprovedPriceCents: standard.PriceCents(),
			ApprovedCurrency:   strings.ToLower(initialCatalog.Currency),
		}, now)

	driftedCatalog := catalogWithPlanMonthlyPrice(t, "standard", 31)
	driftedStandard, _ := driftedCatalog.Get("standard")
	if driftedStandard.PriceCents() == seeded.ApprovedPriceCents {
		t.Fatal("price-drift fixture retained the approved price")
	}
	provider := &countingUpgradeBillingProvider{Fake: fake.New(fake.Config{
		Prices: driftedCatalog.Prices(), Interactive: true, Now: clock.now,
	})}
	manager := newBillingMutationManagerWithProvider(
		t, driftedCatalog, store, provider, clock)

	summary, err := manager.ReconcileBillingMutations(context.Background())
	if !errors.Is(err, ErrBillingMutationApprovalDrift) ||
		summary.Attempted != 1 || summary.Completed != 0 || summary.Failed != 1 {
		t.Fatalf("price-drift reconcile summary = %+v err=%v", summary, err)
	}
	if calls := provider.upgradeMutationCalls(); calls != 0 {
		t.Fatalf("provider upgrade mutation calls = %d; want zero", calls)
	}
	pending, ok, err := store.GetBillingMutation(
		context.Background(), seeded.OperationID)
	if err != nil || !ok || pending.Status != BillingMutationPending ||
		pending.ExecutionClass != BillingMutationExecutionUpgradeSelfServe ||
		pending.ApprovedPriceCents != seeded.ApprovedPriceCents ||
		pending.ApprovedCurrency != seeded.ApprovedCurrency {
		t.Fatalf("pending price-pinned receipt = %+v ok=%v err=%v",
			pending, ok, err)
	}
}

func TestReconcileLegacyBillingMutationRequiresExactDurableEvidence(
	t *testing.T,
) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 11, 15, 50, 0, 0, time.UTC)
	clock := &clock{t: now}
	store := NewMemStore()
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	command := BillingMutationCommand{
		Operation: BillingMutationUpgrade, Plan: "standard",
		Reason:         "Resume a receipt written before approvals were pinned",
		Confirmed:      true,
		IdempotencyKey: "legacy-unpinned-upgrade-0001",
	}
	seeded := seedPendingBillingMutationForTest(
		t, store, billingMutationLegacyReceiptSchemaVersion,
		"acct_legacy_unpinned", "owner@example.com", actor, command,
		billingMutationApproval{}, now)
	provider := &countingUpgradeBillingProvider{Fake: fake.New(fake.Config{
		Prices: catalog.Prices(), Interactive: true, Now: clock.now,
	})}
	manager := newBillingMutationManagerWithProvider(
		t, catalog, store, provider, clock)

	first, err := manager.ReconcileBillingMutations(context.Background())
	if !errors.Is(err, ErrBillingMutationEffectUnpinned) ||
		first.Attempted != 1 || first.Completed != 0 || first.Failed != 1 {
		t.Fatalf("unpinned reconcile summary = %+v err=%v", first, err)
	}
	if calls := provider.upgradeMutationCalls(); calls != 0 {
		t.Fatalf("provider calls without durable evidence = %d; want zero", calls)
	}
	pending, ok, err := store.GetBillingMutation(
		context.Background(), seeded.OperationID)
	if err != nil || !ok || pending.Status != BillingMutationPending ||
		pending.SchemaVersion != billingMutationLegacyReceiptSchemaVersion ||
		pending.ExecutionClass != "" {
		t.Fatalf("legacy pending receipt = %+v ok=%v err=%v", pending, ok, err)
	}

	// A matching durable Pending fold proves the provider effect already
	// happened before a lost receipt-completion response. Recovery may project
	// that exact result, but still must not send the unpinned receipt to billing.
	clock.t = clock.t.Add(time.Minute)
	if err := store.Put(context.Background(), Record{
		AccountID: seeded.AccountID,
		Entitled:  plans.Free,
		Applied:   plans.Free,
		Pending: &Pending{
			Kind: PendingUpgrade, Plan: seeded.TargetPlan,
			OperationID: seeded.OperationID,
			URL:         "https://billing.example.test/recovered-session",
			Expires:     clock.now().Add(time.Hour),
			Requested:   now,
		},
	}); err != nil {
		t.Fatal(err)
	}
	second, err := manager.ReconcileBillingMutations(context.Background())
	if err != nil || second.Attempted != 1 || second.Completed != 1 ||
		second.Failed != 0 {
		t.Fatalf("evidence recovery summary = %+v err=%v", second, err)
	}
	if calls := provider.upgradeMutationCalls(); calls != 0 {
		t.Fatalf("provider calls with exact durable evidence = %d; want zero", calls)
	}
	completed, ok, err := store.GetBillingMutation(
		context.Background(), seeded.OperationID)
	if err != nil || !ok || completed.Status != BillingMutationCompleted ||
		completed.Result == nil ||
		completed.Result.Kind != BillingMutationResultAction ||
		completed.Result.URL != "https://billing.example.test/recovered-session" {
		t.Fatalf("legacy evidence-completed receipt = %+v ok=%v err=%v",
			completed, ok, err)
	}
}

func TestBillingMutationExactRetryRepairsIndexAndKeepsStoredApproval(
	t *testing.T,
) {
	initialCatalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	standard, ok := initialCatalog.Get("standard")
	if !ok {
		t.Fatal("Professional plan missing from fixture")
	}
	now := time.Date(2026, 8, 11, 15, 55, 0, 0, time.UTC)
	clock := &clock{t: now.Add(time.Minute)}
	store := NewMemStore()
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	command := BillingMutationCommand{
		Operation: BillingMutationUpgrade, Plan: "standard",
		Reason:         "Retry the exact approved Professional purchase",
		Confirmed:      true,
		IdempotencyKey: "exact-retry-index-approval-0001",
	}
	seeded := seedPendingBillingMutationForTest(
		t, store, billingMutationReceiptSchemaVersion,
		"acct_exact_retry_approval", "owner-old@example.com", actor, command,
		billingMutationApproval{
			ExecutionClass:     BillingMutationExecutionUpgradeSelfServe,
			ApprovedPriceCents: standard.PriceCents(),
			ApprovedCurrency:   strings.ToLower(initialCatalog.Currency),
		}, now)
	removePendingBillingMutationIndexFixture(t, store, seeded.OperationID)
	if pendingBillingMutationIndexContains(t, store, seeded.OperationID) {
		t.Fatal("missing-index fixture still contains the operation")
	}

	driftedCatalog := catalogWithPlanMonthlyPrice(t, "standard", 31)
	provider := &countingUpgradeBillingProvider{Fake: fake.New(fake.Config{
		Prices: driftedCatalog.Prices(), Interactive: true, Now: clock.now,
	})}
	manager := newBillingMutationManagerWithProvider(
		t, driftedCatalog, store, provider, clock)
	rotatedActor := BillingActor{ID: "usr_billing_2", Role: "billing"}
	_, err = manager.ExecuteBillingMutation(
		context.Background(), seeded.AccountID, "owner-new@example.com",
		rotatedActor, command)
	if !errors.Is(err, ErrBillingMutationApprovalDrift) {
		t.Fatalf("exact retry error = %v; want approval drift", err)
	}
	if calls := provider.upgradeMutationCalls(); calls != 0 {
		t.Fatalf("provider calls for drifted exact retry = %d; want zero", calls)
	}
	if occurrences := pendingBillingMutationIndexOccurrences(
		t, store, seeded.OperationID); occurrences != 1 {
		t.Fatalf("exact retry indexed operation %d times; want one", occurrences)
	}
	stored, ok, err := store.GetBillingMutation(
		context.Background(), seeded.OperationID)
	if err != nil || !ok || stored.Status != BillingMutationPending ||
		stored.ExecutionClass != seeded.ExecutionClass ||
		stored.ApprovedPriceCents != seeded.ApprovedPriceCents ||
		stored.ApprovedCurrency != seeded.ApprovedCurrency ||
		stored.ActorID != seeded.ActorID || stored.ActorRole != seeded.ActorRole {
		t.Fatalf("stored approval after exact retry = %+v ok=%v err=%v",
			stored, ok, err)
	}
}

func TestBillingMutationStripeRejectsUnsupportedPaidDowngradeBeforeReceipt(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	provider, err := stripe.New(stripe.Config{
		SecretKey: "sk_test_preview_only",
		Catalog:   catalog,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock := &clock{t: time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)}
	store := NewMemStore()
	if err := store.Put(context.Background(), Record{
		AccountID: "acct_stripe_target", Provider: "stripe",
		CustomerID: "cus_preview_only", Entitled: "team", Applied: "team",
	}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{
		Catalog: catalog,
		Providers: map[string]billing.Provider{
			"stripe": provider,
		},
		Default: "stripe", Store: store, Applier: &recApplier{}, Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	paid := BillingMutationCommand{
		Operation: BillingMutationDowngrade, Plan: "standard",
		Reason: "Move from Team to Professional",
	}
	preview, err := manager.PreviewBillingMutation(
		ctx, "acct_stripe_target", "", paid)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Allowed || !strings.Contains(
		strings.Join(preview.Violations, "\n"), "downgrade to standard",
	) {
		t.Fatalf("paid-to-paid preview = %+v; want provider-capability refusal", preview)
	}

	apply := paid
	apply.Confirmed = true
	apply.IdempotencyKey = "stripe-paid-downgrade-0001"
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	if _, err := manager.ExecuteBillingMutation(
		ctx, "acct_stripe_target", "", actor, apply,
	); !errors.Is(err, ErrRefusal) {
		t.Fatalf("paid-to-paid apply error = %v; want refusal", err)
	}
	operationID := billingMutationOperationID(
		"acct_stripe_target", apply.IdempotencyKey)
	if _, exists, err := store.GetBillingMutation(ctx, operationID); err != nil || exists {
		t.Fatalf("refused receipt exists=%t err=%v; want none", exists, err)
	}
	if len(store.billingMutationAccounts) != 0 {
		t.Fatalf("refused downgrade created account lane: %+v", store.billingMutationAccounts)
	}

	free := paid
	free.Plan = plans.Free
	free.Reason = "Return to Personal"
	preview, err = manager.PreviewBillingMutation(
		ctx, "acct_stripe_target", "", free)
	if err != nil || !preview.Allowed {
		t.Fatalf("free downgrade preview = %+v, %v; want allowed", preview, err)
	}
}

func TestBillingMutationExactDowngradeRetryIgnoresFitAndCatalogDrift(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	clock := &clock{t: time.Date(2026, 8, 11, 17, 0, 0, 0, time.UTC)}
	prices := catalog.Prices()
	prices["team"] = 25_000
	base := fake.New(fake.Config{Prices: prices, Now: clock.now})
	provider := &ambiguousDowngradeBillingProvider{Fake: base}
	ctx := context.Background()
	customerID, err := base.EnsureCustomer(ctx, "acct_drift", "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.SubscribeIdempotent(
		ctx, customerID, "team", "bop_seed_team_subscription",
	); err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	if err := store.Put(ctx, Record{
		AccountID: "acct_drift", Provider: "billing", CustomerID: customerID,
		Entitled: "team", Applied: "team", EntitledAt: clock.now(),
	}); err != nil {
		t.Fatal(err)
	}
	fit := &mutableBillingFit{}
	newManager := func(catalog *plans.Catalog) *Manager {
		manager, err := NewManager(Config{
			Catalog: catalog,
			Providers: map[string]billing.Provider{
				"billing": provider,
			},
			Default: "billing", Store: store, Applier: &recApplier{},
			Fit: fit, Now: clock.now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	command := BillingMutationCommand{
		Operation: BillingMutationDowngrade, Plan: "standard",
		Reason: "Move from Team to Professional", Confirmed: true,
		IdempotencyKey: "drift-downgrade-2026-08-11",
	}
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	if _, err := newManager(catalog).ExecuteBillingMutation(
		ctx, "acct_drift", "owner@example.com", actor, command,
	); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ambiguous downgrade error = %v; want deadline", err)
	}

	operationID := billingMutationOperationID("acct_drift", command.IdempotencyKey)
	receipt, ok, err := store.GetBillingMutation(ctx, operationID)
	if err != nil || !ok || receipt.Status != BillingMutationPending {
		t.Fatalf("pending receipt = %+v ok=%t err=%v", receipt, ok, err)
	}
	record, ok, err := store.Get(ctx, "acct_drift")
	if err != nil || !ok || record.Pending == nil ||
		record.Pending.OperationID != operationID || !record.Pending.Effective.IsZero() {
		t.Fatalf("ambiguous account state = %+v ok=%t err=%v", record, ok, err)
	}

	fit.set("current usage now exceeds the Professional cap")
	driftedCatalog := catalogWithTeamMonthlyPrice(t, 20)
	team, _ := driftedCatalog.Get("team")
	standard, _ := driftedCatalog.Get("standard")
	if standard.PriceCents() <= team.PriceCents() {
		t.Fatalf("drift fixture did not reverse direction: standard=%d team=%d",
			standard.PriceCents(), team.PriceCents())
	}
	recovered, err := newManager(driftedCatalog).ExecuteBillingMutation(
		ctx, "acct_drift", "owner@example.com", actor, command)
	if err != nil || recovered.Outcome.Kind != "scheduled" ||
		recovered.Outcome.Plan != "standard" || recovered.Outcome.Effective.IsZero() ||
		!recovered.Replayed {
		t.Fatalf("exact recovery = %+v, %v", recovered, err)
	}
}

func TestBillingMutationDowngradeWebhookTombstoneRecoversLostProviderResponse(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	clock := &clock{t: time.Date(2026, 8, 11, 17, 30, 0, 0, time.UTC)}
	prices := catalog.Prices()
	prices["team"] = 25_000
	base := fake.New(fake.Config{Prices: prices, Now: clock.now})
	provider := &ambiguousDowngradeBillingProvider{Fake: base}
	ctx := context.Background()
	customerID, err := base.EnsureCustomer(
		ctx, "acct_webhook_recovery", "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.SubscribeIdempotent(
		ctx, customerID, "team", "bop_seed_webhook_team",
	); err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	if err := store.Put(ctx, Record{
		AccountID: "acct_webhook_recovery", Provider: "billing",
		CustomerID: customerID, Entitled: "team", Applied: "team",
		EntitledAt: clock.now(),
	}); err != nil {
		t.Fatal(err)
	}
	manager := newBillingMutationManagerWithProvider(
		t, catalog, store, provider, clock)
	command := BillingMutationCommand{
		Operation: BillingMutationDowngrade, Plan: plans.Free,
		Reason: "Return from Team to Personal", Confirmed: true,
		IdempotencyKey: "webhook-downgrade-2026-08-11",
	}
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	if _, err := manager.ExecuteBillingMutation(
		ctx, "acct_webhook_recovery", "owner@example.com", actor, command,
	); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ambiguous downgrade error = %v; want deadline", err)
	}
	operationID := billingMutationOperationID(
		"acct_webhook_recovery", command.IdempotencyKey)

	clock.t = clock.t.AddDate(0, 0, 31)
	events := base.ApplyDue()
	if len(events) != 1 || events[0].Type != billing.EventSubscriptionCanceled {
		t.Fatalf("period-end events = %+v", events)
	}
	if err := manager.OnEvents(ctx, "billing", events); err != nil {
		t.Fatal(err)
	}
	record, ok, err := store.Get(ctx, "acct_webhook_recovery")
	if err != nil || !ok || record.Pending != nil || record.Entitled != plans.Free ||
		record.LastBillingMutationOperationID != operationID ||
		record.LastBillingMutationKind != BillingMutationPlanDowngrade ||
		record.LastBillingMutationResultKind != BillingMutationResultScheduled ||
		record.LastBillingMutationPlan != plans.Free ||
		!record.LastBillingMutationEffective.Equal(events[0].At) {
		t.Fatalf("webhook tombstone = %+v ok=%t err=%v", record, ok, err)
	}

	recovered, err := manager.ExecuteBillingMutation(
		ctx, "acct_webhook_recovery", "owner@example.com", actor, command)
	if err != nil || recovered.Outcome.Kind != "scheduled" ||
		recovered.Outcome.Plan != plans.Free ||
		!recovered.Outcome.Effective.Equal(events[0].At) || !recovered.Replayed {
		t.Fatalf("webhook recovery = %+v, %v", recovered, err)
	}
	if provider.downgradeCalls() != 1 {
		t.Fatalf("provider downgrade calls = %d; want one", provider.downgradeCalls())
	}
	receipt, ok, err := store.GetBillingMutation(ctx, operationID)
	if err != nil || !ok || receipt.Status != BillingMutationCompleted ||
		receipt.Result == nil || receipt.Result.Kind != BillingMutationResultScheduled ||
		receipt.Result.Plan != plans.Free || receipt.Result.Effective == nil ||
		!receipt.Result.Effective.Equal(events[0].At) {
		t.Fatalf("completed receipt = %+v ok=%t err=%v", receipt, ok, err)
	}
}

func TestBillingMutationCancelTombstoneRecoversExactTerminalKind(t *testing.T) {
	for _, tc := range []struct {
		name        string
		race        string
		wantKind    BillingMutationResultKind
		wantPending string
	}{
		{name: "successful cancellation", wantKind: BillingMutationResultCancelled},
		{name: "pending disappeared", race: "disappear", wantKind: BillingMutationResultResolved},
		{
			name: "pending was replaced", race: "replace",
			wantKind: BillingMutationResultResolved, wantPending: "bop_replacement_pending",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := plans.Load()
			if err != nil {
				t.Fatal(err)
			}
			clock := &clock{t: time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC)}
			base := fake.New(fake.Config{
				Prices: catalog.Prices(), Interactive: true, Now: clock.now,
			})
			store := &failNextBillingCompletionStore{MemStore: NewMemStore()}
			manager := newBillingMutationManagerWithProvider(
				t, catalog, store, base, clock)
			ctx := context.Background()
			actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
			accountID := "acct_cancel_" + strings.ReplaceAll(tc.race+tc.name, " ", "_")
			pending, err := manager.ExecuteBillingMutation(
				ctx, accountID, "owner@example.com", actor,
				BillingMutationCommand{
					Operation: BillingMutationUpgrade, Plan: "standard",
					Reason: "Start Professional checkout", Confirmed: true,
					IdempotencyKey: "cancel-race-upgrade-0001",
				})
			if err != nil || pending.Outcome.Kind != "action" {
				t.Fatalf("pending upgrade = %+v, %v", pending, err)
			}

			var racingProvider *resolveDuringCancelBillingProvider
			var cancelProvider billing.Provider = base
			if tc.race != "" {
				racingProvider = &resolveDuringCancelBillingProvider{
					Fake: base, store: store.MemStore, accountID: accountID, now: clock.now,
				}
				if tc.race == "replace" {
					racingProvider.replace = &Pending{
						Kind: PendingDowngrade, Plan: plans.Free,
						OperationID: tc.wantPending, Requested: clock.now().Add(time.Minute),
					}
				}
				cancelProvider = racingProvider
			}
			manager = newBillingMutationManagerWithProvider(
				t, catalog, store, cancelProvider, clock)
			cancelCommand := BillingMutationCommand{
				Operation: BillingMutationCancel,
				Reason:    "Cancel the pending checkout", Confirmed: true,
				IdempotencyKey: "cancel-race-cancel-00001",
			}
			store.arm()
			if _, err := manager.ExecuteBillingMutation(
				ctx, accountID, "owner@example.com", actor, cancelCommand,
			); !errors.Is(err, errForcedBillingMutationCompletion) {
				t.Fatalf("lost completion error = %v", err)
			}

			operationID := billingMutationOperationID(
				accountID, cancelCommand.IdempotencyKey)
			receipt, ok, err := store.GetBillingMutation(ctx, operationID)
			if err != nil || !ok || receipt.Status != BillingMutationPending {
				t.Fatalf("pending receipt = %+v ok=%t err=%v", receipt, ok, err)
			}
			record, ok, err := store.Get(ctx, accountID)
			if err != nil || !ok ||
				record.LastBillingMutationOperationID != operationID ||
				record.LastBillingMutationKind != BillingMutationPlanCancel ||
				record.LastBillingMutationResultKind != tc.wantKind ||
				record.LastBillingMutationPlan != "" ||
				!record.LastBillingMutationEffective.IsZero() {
				t.Fatalf("cancel tombstone = %+v ok=%t err=%v", record, ok, err)
			}
			if tc.wantPending == "" {
				if record.Pending != nil {
					t.Fatalf("unexpected pending state after cancel: %+v", record.Pending)
				}
			} else if record.Pending == nil || record.Pending.OperationID != tc.wantPending {
				t.Fatalf("replacement pending was changed: %+v", record.Pending)
			}

			clock.t = clock.t.Add(billingMutationClaimLease + time.Minute)
			replayed, err := manager.ExecuteBillingMutation(
				ctx, accountID, "owner@example.com", actor, cancelCommand)
			if err != nil || replayed.Outcome.Kind != string(tc.wantKind) ||
				!replayed.Replayed || replayed.OperationID != operationID ||
				replayed.Outcome.Plan != "" || replayed.Outcome.URL != "" ||
				!replayed.Outcome.Effective.IsZero() {
				t.Fatalf("recovered cancel = %+v, %v", replayed, err)
			}
			if racingProvider != nil && racingProvider.cancelCalls() != 1 {
				t.Fatalf("provider cancel calls = %d; want one", racingProvider.cancelCalls())
			}
			receipt, ok, err = store.GetBillingMutation(ctx, operationID)
			if err != nil || !ok || receipt.Status != BillingMutationCompleted ||
				receipt.Result == nil || receipt.Result.Kind != tc.wantKind ||
				receipt.Result.Cancelled != (tc.wantKind == BillingMutationResultCancelled) {
				t.Fatalf("completed receipt = %+v ok=%t err=%v", receipt, ok, err)
			}
		})
	}
}

func TestR2StorePreservesBillingMutationTombstoneProjection(t *testing.T) {
	store := newR2Store(t)
	effective := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	record := Record{
		AccountID: "acct_r2_billing_tombstone", Entitled: plans.Free,
		Applied:                        plans.Free,
		LastBillingMutationOperationID: "bop_r2_downgrade_tombstone",
		LastBillingMutationKind:        BillingMutationPlanDowngrade,
		LastBillingMutationResultKind:  BillingMutationResultScheduled,
		LastBillingMutationPlan:        plans.Free,
		LastBillingMutationEffective:   effective,
		LastBillingMutationAt:          effective.Add(time.Minute),
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(context.Background(), record.AccountID)
	if err != nil || !ok ||
		got.LastBillingMutationOperationID != record.LastBillingMutationOperationID ||
		got.LastBillingMutationKind != record.LastBillingMutationKind ||
		got.LastBillingMutationResultKind != record.LastBillingMutationResultKind ||
		got.LastBillingMutationPlan != record.LastBillingMutationPlan ||
		!got.LastBillingMutationEffective.Equal(record.LastBillingMutationEffective) ||
		!got.LastBillingMutationAt.Equal(record.LastBillingMutationAt) {
		t.Fatalf("R2 tombstone = %+v ok=%t err=%v", got, ok, err)
	}
}

func catalogWithTeamMonthlyPrice(t *testing.T, monthly int64) *plans.Catalog {
	t.Helper()
	var doc struct {
		SchemaVersion string       `json:"schema_version"`
		Updated       string       `json:"updated"`
		Currency      string       `json:"currency"`
		Plans         []plans.Plan `json:"plans"`
	}
	if err := json.Unmarshal(witself.PlansJSON, &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range doc.Plans {
		if doc.Plans[i].ID == "team" {
			doc.Plans[i].PriceMonthly = &monthly
			doc.Plans[i].PriceMonthlyMin = nil
			found = true
			break
		}
	}
	if !found {
		t.Fatal("team plan missing from drift fixture")
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := plans.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func catalogWithPlanAvailability(
	t *testing.T,
	planID string,
	available bool,
) *plans.Catalog {
	t.Helper()
	return catalogWithPlanEdit(t, planID, func(plan *plans.Plan) {
		plan.Available = available
	})
}

func catalogWithPlanMonthlyPrice(
	t *testing.T,
	planID string,
	monthly int64,
) *plans.Catalog {
	t.Helper()
	return catalogWithPlanEdit(t, planID, func(plan *plans.Plan) {
		plan.PriceMonthly = &monthly
		plan.PriceMonthlyMin = nil
	})
}

func catalogWithPlanEdit(
	t *testing.T,
	planID string,
	edit func(*plans.Plan),
) *plans.Catalog {
	t.Helper()
	var doc struct {
		SchemaVersion string       `json:"schema_version"`
		Updated       string       `json:"updated"`
		Currency      string       `json:"currency"`
		Plans         []plans.Plan `json:"plans"`
	}
	if err := json.Unmarshal(witself.PlansJSON, &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range doc.Plans {
		if doc.Plans[i].ID == planID {
			edit(&doc.Plans[i])
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("plan %q missing from catalog fixture", planID)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := plans.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func billingOperationIDForTest(accountID, key string) string {
	command := BillingMutationCommand{
		Operation: BillingMutationSetup,
		Reason:    "test preview identity", Confirmed: true,
		IdempotencyKey: key,
	}
	receipt, err := buildBillingMutationReceipt(
		accountID, "", BillingActor{ID: "test_actor", Role: "owner"},
		command, 1, billingMutationApproval{
			ExecutionClass: BillingMutationExecutionSetup,
		}, time.Now().UTC())
	if err != nil {
		panic(err)
	}
	return receipt.OperationID
}
