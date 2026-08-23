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
		receipt.ClaimToken != "" || receipt.Result == nil ||
		receipt.Result.ProviderObjectID == "" ||
		receipt.Result.ActionExpiresAt == nil {
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

	clock.t = *receipt.Result.ActionExpiresAt
	if _, err := manager.ExecuteBillingMutation(
		ctx, "acct_setup", "owner@example.com", actor, command,
	); !errors.Is(err, ErrRefusal) || !strings.Contains(err.Error(), "new idempotency key") {
		t.Fatalf("expired exact replay error = %v", err)
	}
	fresh := command
	fresh.IdempotencyKey = "setup-2026-08-11-0002"
	renewed, err := manager.ExecuteBillingMutation(
		ctx, "acct_setup", "owner@example.com", actor, fresh)
	if err != nil || renewed.OperationID == first.OperationID ||
		renewed.Outcome.Kind != "action" {
		t.Fatalf("renewed hosted setup = %+v, %v", renewed, err)
	}
}

func TestBillingExecutionNeverReplaysExpiredHostedAction(t *testing.T) {
	t.Parallel()
	expires := time.Date(2026, 8, 17, 13, 0, 0, 0, time.UTC)
	receipt := BillingMutationReceipt{
		Status: BillingMutationCompleted,
		Result: &BillingMutationResult{
			Kind:             BillingMutationResultAction,
			URL:              "https://checkout.stripe.com/c/pay/cs_test_expiring",
			ProviderObjectID: "cs_test_expiring",
			ActionExpiresAt:  &expires,
		},
	}
	if execution, err := billingExecutionFromReceipt(
		receipt, true, expires.Add(-time.Nanosecond),
	); err != nil || execution.Outcome.URL != receipt.Result.URL {
		t.Fatalf("pre-expiry replay = %+v, %v", execution, err)
	}
	for _, now := range []time.Time{expires, expires.Add(time.Second)} {
		if _, err := billingExecutionFromReceipt(receipt, true, now); !errors.Is(err, ErrRefusal) ||
			!strings.Contains(err.Error(), "new idempotency key") {
			t.Fatalf("expired replay at %v error = %v", now, err)
		}
	}
}

func TestLegacyHostedActionGetsBoundedCreatedAtFallback(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	completed := created.Add(23 * time.Hour)
	receipt := BillingMutationReceipt{
		CreatedAt: created, CompletedAt: &completed,
		Status: BillingMutationCompleted,
		Result: &BillingMutationResult{
			Kind: BillingMutationResultAction,
			URL:  "https://checkout.stripe.com/c/pay/cs_test_legacy",
		},
	}
	boundary := created.Add(DefaultPendingTTL)
	if _, err := billingExecutionFromReceipt(
		receipt, true, boundary.Add(-time.Nanosecond),
	); err != nil {
		t.Fatalf("legacy pre-boundary replay: %v", err)
	}
	if _, err := billingExecutionFromReceipt(
		receipt, true, boundary,
	); !errors.Is(err, ErrRefusal) {
		t.Fatalf("legacy boundary replay error = %v", err)
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
	mu          sync.Mutex
	failed      bool
	calls       int
	firstAction billing.Action
	lastAction  billing.Action
}

// providerWithoutExactCancel deliberately exposes the historical broad
// cancellation interface but not ExactPendingCanceller. Preview must not
// approve work that execution would later refuse after owning the receipt.
type providerWithoutExactCancel struct {
	billing.Provider
	subscriber billing.IdempotentSubscriber
	canceller  billing.IdempotentPendingCanceller
}

func (p *providerWithoutExactCancel) SubscribeIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.Action, error) {
	return p.subscriber.SubscribeIdempotent(ctx, customerID, plan, operationID)
}

func (p *providerWithoutExactCancel) CancelPendingIdempotent(
	ctx context.Context,
	customerID, operationID string,
) error {
	return p.canceller.CancelPendingIdempotent(ctx, customerID, operationID)
}

func (p *failFirstBillingProvider) SubscribeIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.Action, error) {
	action, err := p.Fake.SubscribeIdempotent(
		ctx, customerID, plan, operationID)
	p.mu.Lock()
	p.calls++
	fail := !p.failed
	if fail {
		p.failed = true
		p.firstAction = action
	} else {
		p.lastAction = action
	}
	p.mu.Unlock()
	if err != nil {
		return billing.Action{}, err
	}
	if fail {
		// The provider committed Checkout, but its response was lost. The exact
		// retry must recover the same object rather than minting a second one.
		return billing.Action{}, context.DeadlineExceeded
	}
	return action, nil
}

func (p *failFirstBillingProvider) subscribeCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *failFirstBillingProvider) replayedExactAction() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls >= 2 && p.firstAction == p.lastAction &&
		p.firstAction.ProviderObjectID != "" && p.firstAction.URL != "" &&
		!p.firstAction.ExpiresAt.IsZero()
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
	_ PlanSnapshot,
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
	mu                sync.Mutex
	failed            bool
	calls             int
	prepareCalls      int
	driftAfterPrepare bool
	targets           []billing.ScheduledDowngrade
}

func (p *ambiguousDowngradeBillingProvider) ScheduleDowngradeExactIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
) (billing.ScheduledDowngrade, error) {
	prepared, err := p.PrepareDowngrade(ctx, customerID, plan)
	if err != nil {
		return billing.ScheduledDowngrade{}, err
	}
	return p.SchedulePreparedDowngradeIdempotent(
		ctx, customerID, plan, operationID, prepared)
}

func (p *ambiguousDowngradeBillingProvider) PrepareDowngrade(
	ctx context.Context,
	customerID, plan string,
) (billing.ScheduledDowngrade, error) {
	prepared, err := p.Fake.PrepareDowngrade(ctx, customerID, plan)
	if err != nil {
		return billing.ScheduledDowngrade{}, err
	}
	p.mu.Lock()
	p.prepareCalls++
	call := p.prepareCalls
	drift := p.driftAfterPrepare
	p.mu.Unlock()
	if drift && call > 1 {
		prepared.ProviderObjectID = "fake_subscription_replacement"
	}
	return prepared, nil
}

func (p *ambiguousDowngradeBillingProvider) SchedulePreparedDowngradeIdempotent(
	ctx context.Context,
	customerID, plan, operationID string,
	prepared billing.ScheduledDowngrade,
) (billing.ScheduledDowngrade, error) {
	scheduled, err := p.Fake.SchedulePreparedDowngradeIdempotent(
		ctx, customerID, plan, operationID, prepared)
	if err != nil {
		return billing.ScheduledDowngrade{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.targets = append(p.targets, prepared)
	if !p.failed {
		p.failed = true
		return billing.ScheduledDowngrade{}, context.DeadlineExceeded
	}
	return scheduled, nil
}

func (p *ambiguousDowngradeBillingProvider) downgradeCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *ambiguousDowngradeBillingProvider) preparedCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prepareCalls
}

func (p *ambiguousDowngradeBillingProvider) downgradeTargets() []billing.ScheduledDowngrade {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]billing.ScheduledDowngrade(nil), p.targets...)
}

// alreadyResolvedExactCancelProvider models Stripe observing that period end
// already won before its subscription-deleted webhook reached lifecycle state.
// The local pending fence must remain visible until that webhook is folded.
type alreadyResolvedExactCancelProvider struct {
	*fake.Fake
	mu    sync.Mutex
	calls int
}

func (p *alreadyResolvedExactCancelProvider) CancelPendingObjectIdempotent(
	_ context.Context,
	_ string,
	_ billing.PendingCancellation,
	_ string,
) error {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return billing.ErrPendingAlreadyResolved
}

func (p *alreadyResolvedExactCancelProvider) cancelCalls() int {
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

func (p *resolveDuringCancelBillingProvider) CancelPendingObjectIdempotent(
	ctx context.Context,
	customerID string,
	target billing.PendingCancellation,
	operationID string,
) error {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if err := p.Fake.CancelPendingObjectIdempotent(
		ctx, customerID, target, operationID,
	); err != nil {
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
	); !errors.Is(err, ErrRefusal) ||
		!strings.Contains(err.Error(), "operator reconciliation") {
		t.Fatalf("concurrent targetless cancel error = %v; want safe refusal", err)
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
	); !errors.Is(err, ErrRefusal) ||
		!strings.Contains(err.Error(), "operator reconciliation") {
		t.Fatalf("cancel during ambiguous upgrade = %v; want safe refusal", err)
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
	if !provider.replayedExactAction() ||
		upgrade.Outcome.ProviderObjectID != provider.firstAction.ProviderObjectID {
		t.Fatalf("ambiguous retry did not recover one exact Checkout: %+v", upgrade)
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
	if !provider.replayedExactAction() {
		t.Fatal("autonomous retry did not recover the exact created Checkout")
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

func TestReconcileBillingMutationsDefersAccountsOutsideMutationCohort(
	t *testing.T,
) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	standard, ok := catalog.Get("standard")
	if !ok || !standard.Purchasable() {
		t.Fatalf("Professional fixture = %+v ok=%v", standard, ok)
	}
	now := time.Date(2026, 8, 17, 18, 0, 0, 0, time.UTC)
	clock := &clock{t: now}
	store := NewMemStore()
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	command := BillingMutationCommand{
		Operation: BillingMutationUpgrade, Plan: "standard",
		Reason:         "Exercise cohort-fenced recovery",
		Confirmed:      true,
		IdempotencyKey: "cohort-fenced-recovery-0001",
	}
	seeded := seedPendingBillingMutationForTest(
		t, store, billingMutationReceiptSchemaVersion,
		"acct_cohort_recovery", "owner@example.com", actor, command,
		billingMutationApproval{
			ExecutionClass:     BillingMutationExecutionUpgradeSelfServe,
			ApprovedPriceCents: standard.PriceCents(),
			ApprovedCurrency:   strings.ToLower(catalog.Currency),
		}, now)
	provider := &countingUpgradeBillingProvider{Fake: fake.New(fake.Config{
		Prices: catalog.Prices(), Interactive: true, Now: clock.now,
	})}
	var gateMu sync.Mutex
	enabled := false
	gatedAccount := ""
	manager, err := NewManager(Config{
		Catalog: catalog,
		Providers: map[string]billing.Provider{
			"billing": provider,
		},
		Default: "billing", Store: store,
		Applier: &recApplier{}, Now: clock.now,
		BillingMutationGate: func(_ context.Context, accountID string) (bool, error) {
			gateMu.Lock()
			defer gateMu.Unlock()
			gatedAccount = accountID
			return enabled, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	deferred, err := manager.ReconcileBillingMutations(context.Background())
	if err != nil || deferred.Scanned != 1 || deferred.Attempted != 0 ||
		deferred.Completed != 0 || deferred.Failed != 0 {
		t.Fatalf("cohort-deferred summary = %+v err=%v", deferred, err)
	}
	if calls := provider.upgradeMutationCalls(); calls != 0 {
		t.Fatalf("provider calls outside cohort = %d; want zero", calls)
	}
	gateMu.Lock()
	gotGatedAccount := gatedAccount
	enabled = true
	gateMu.Unlock()
	if gotGatedAccount != seeded.AccountID {
		t.Fatalf("recovery gate account = %q; want %q",
			gotGatedAccount, seeded.AccountID)
	}
	pending, ok, err := store.GetBillingMutation(
		context.Background(), seeded.OperationID)
	if err != nil || !ok || pending.Status != BillingMutationPending {
		t.Fatalf("deferred receipt = %+v ok=%v err=%v", pending, ok, err)
	}

	resumed, err := manager.ReconcileBillingMutations(context.Background())
	if err != nil || resumed.Scanned != 1 || resumed.Attempted != 1 ||
		resumed.Completed != 1 || resumed.Failed != 0 {
		t.Fatalf("cohort-resumed summary = %+v err=%v", resumed, err)
	}
	if calls := provider.upgradeMutationCalls(); calls != 2 {
		t.Fatalf("provider calls after cohort enable = %d; want customer+checkout", calls)
	}
	completed, ok, err := store.GetBillingMutation(
		context.Background(), seeded.OperationID)
	if err != nil || !ok || completed.Status != BillingMutationCompleted {
		t.Fatalf("resumed receipt = %+v ok=%v err=%v", completed, ok, err)
	}
}

func TestReconcileBillingMutationsFailsClosedWhenMutationGateErrors(
	t *testing.T,
) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	standard, _ := catalog.Get("standard")
	now := time.Date(2026, 8, 17, 18, 15, 0, 0, time.UTC)
	clock := &clock{t: now}
	store := NewMemStore()
	seeded := seedPendingBillingMutationForTest(
		t, store, billingMutationReceiptSchemaVersion,
		"acct_cohort_gate_error", "owner@example.com",
		BillingActor{ID: "usr_owner_1", Role: "owner"},
		BillingMutationCommand{
			Operation: BillingMutationUpgrade, Plan: "standard",
			Reason:         "Exercise recovery gate failure",
			Confirmed:      true,
			IdempotencyKey: "cohort-gate-error-0001",
		}, billingMutationApproval{
			ExecutionClass:     BillingMutationExecutionUpgradeSelfServe,
			ApprovedPriceCents: standard.PriceCents(),
			ApprovedCurrency:   strings.ToLower(catalog.Currency),
		}, now)
	provider := &countingUpgradeBillingProvider{Fake: fake.New(fake.Config{
		Prices: catalog.Prices(), Interactive: true, Now: clock.now,
	})}
	gateErr := errors.New("cohort source unavailable")
	manager, err := NewManager(Config{
		Catalog: catalog,
		Providers: map[string]billing.Provider{
			"billing": provider,
		},
		Default: "billing", Store: store,
		Applier: &recApplier{}, Now: clock.now,
		BillingMutationGate: func(context.Context, string) (bool, error) {
			return false, gateErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := manager.ReconcileBillingMutations(context.Background())
	if !errors.Is(err, gateErr) || summary.Scanned != 1 ||
		summary.Attempted != 1 || summary.Failed != 1 || summary.Completed != 0 {
		t.Fatalf("gate-error summary = %+v err=%v", summary, err)
	}
	if calls := provider.upgradeMutationCalls(); calls != 0 {
		t.Fatalf("provider calls after gate error = %d; want zero", calls)
	}
	pending, ok, getErr := store.GetBillingMutation(
		context.Background(), seeded.OperationID)
	if getErr != nil || !ok || pending.Status != BillingMutationPending {
		t.Fatalf("gate-error receipt = %+v ok=%v err=%v", pending, ok, getErr)
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

func TestAmbiguousHostedPurchaseStaysQuarantinedAfterRetryHorizon(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	clock := &clock{t: time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)}
	base := fake.New(fake.Config{
		Prices: catalog.Prices(), Interactive: true, Now: clock.now,
	})
	provider := &failFirstBillingProvider{Fake: base}
	store := NewMemStore()
	manager := newBillingMutationManagerWithProvider(
		t, catalog, store, provider, clock)
	command := BillingMutationCommand{
		Operation: BillingMutationUpgrade, Plan: "standard",
		Reason:    "Exercise a lost Checkout response beyond the automatic retry window",
		Confirmed: true, IdempotencyKey: "ambiguous-horizon-2026-08-11",
	}
	ctx := context.Background()
	if _, err := manager.ExecuteBillingMutation(
		ctx, "acct_ambiguous_horizon", "owner@example.com",
		BillingActor{ID: "usr_owner_1", Role: "owner"}, command,
	); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first ambiguous upgrade = %v", err)
	}
	clock.t = clock.t.Add(25 * time.Hour)
	if err := manager.ReconcileAccount(ctx, "acct_ambiguous_horizon"); err == nil || !strings.Contains(err.Error(), "operator reconciliation") {
		t.Fatalf("expired ambiguous reconcile = %v; want quarantine", err)
	}
	record, ok, err := store.Get(ctx, "acct_ambiguous_horizon")
	if err != nil || !ok || record.Pending == nil ||
		record.Pending.Kind != PendingUpgrade ||
		record.Pending.ProviderObjectID != "" {
		t.Fatalf("ambiguous hosted purchase was cleared: %+v ok=%t err=%v",
			record, ok, err)
	}
	operationID := billingMutationOperationID(
		"acct_ambiguous_horizon", command.IdempotencyKey)
	receipt, ok, err := store.GetBillingMutation(ctx, operationID)
	if err != nil || !ok || receipt.Status != BillingMutationPending {
		t.Fatalf("ambiguous receipt was terminalized: %+v ok=%t err=%v",
			receipt, ok, err)
	}
}

// A paid-to-paid UPGRADE is the dangerous direction: Subscribe starts a new
// hosted Checkout Session, so without a guard an account that already pays
// would end up with two live subscriptions and two invoices. It must take the
// contact path instead, while free-to-paid stays self-serve.
// guardedUpgradeBillingProvider mirrors the Stripe adapter's capability surface
// after the paid-to-paid guard: it can only start a subscription for an account
// that does not already have one.
type guardedUpgradeBillingProvider struct {
	*countingUpgradeBillingProvider
}

func (*guardedUpgradeBillingProvider) SupportsUpgradeTransition(current, _ string) bool {
	return current == "" || current == plans.Free
}

// A preview approval describes the account as it was when it was minted. If the
// account becomes paid before the receipt executes — a webhook fold landing, an
// exact retry, or the reconciler resuming unattended — replaying that approved
// self-serve class would buy a SECOND subscription. Execution must re-assert the
// provider capability against the freshest record and refuse.
func TestBillingMutationPaidUpgradeIsRefusedAtExecutionNotOnlyPreview(t *testing.T) {
	var document map[string]any
	if err := json.Unmarshal(witself.PlansJSON, &document); err != nil {
		t.Fatal(err)
	}
	for _, entry := range document["plans"].([]any) {
		plan := entry.(map[string]any)
		if plan["id"] == "team" {
			plan["available"] = true
		}
	}
	flipped, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := plans.Parse(flipped)
	if err != nil {
		t.Fatal(err)
	}
	team, ok := catalog.Get("team")
	if !ok || !team.Purchasable() {
		t.Fatalf("test catalog Team = %+v ok=%v; want purchasable", team, ok)
	}

	now := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
	clock := &clock{t: now}
	store := NewMemStore()
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	command := BillingMutationCommand{
		Operation: BillingMutationUpgrade, Plan: "team",
		Reason:         "Move up to Team",
		Confirmed:      true,
		IdempotencyKey: "paid-upgrade-stale-approval-0001",
	}
	// The approval was legitimately minted while the account was still free.
	seedPendingBillingMutationForTest(
		t, store, billingMutationReceiptSchemaVersion,
		"acct_stale_upgrade", "owner@example.com", actor, command,
		billingMutationApproval{
			ExecutionClass:     BillingMutationExecutionUpgradeSelfServe,
			ApprovedPriceCents: team.PriceCents(),
			ApprovedCurrency:   strings.ToLower(catalog.Currency),
		}, now)
	// Meanwhile the account actually became paid.
	if err := store.Put(context.Background(), Record{
		AccountID: "acct_stale_upgrade", Provider: "fake",
		CustomerID:            "cus_stale_upgrade",
		Entitled:              "standard",
		Applied:               "standard",
		EntitledAt:            now,
		ManagedSubscriptionID: "sub_live_standard",
	}); err != nil {
		t.Fatal(err)
	}

	provider := &guardedUpgradeBillingProvider{
		countingUpgradeBillingProvider: &countingUpgradeBillingProvider{
			Fake: fake.New(fake.Config{
				Prices: catalog.Prices(), Interactive: true, Now: clock.now,
			}),
		},
	}
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

	_, execErr := manager.ExecuteBillingMutation(
		context.Background(), "acct_stale_upgrade", "owner@example.com", actor, command)
	if !errors.Is(execErr, ErrBillingMutationApprovalDrift) {
		t.Fatalf("execute of a stale self-serve approval = %v, want approval drift", execErr)
	}
	provider.mu.Lock()
	subscribes := provider.subscribeCalls
	provider.mu.Unlock()
	if subscribes != 0 {
		t.Fatalf("subscribe calls = %d, want 0; a purchase here is a second live subscription", subscribes)
	}

	// The receipt stays pending for operator resolution rather than being
	// terminalized on a state the approval never described.
	stored, ok, err := store.GetBillingMutation(
		context.Background(), billingMutationOperationID("acct_stale_upgrade", command.IdempotencyKey))
	if err != nil || !ok {
		t.Fatalf("receipt lookup ok=%v err=%v", ok, err)
	}
	if stored.Status != BillingMutationPending {
		t.Fatalf("receipt status = %q, want pending for operator resolution", stored.Status)
	}
}

func TestBillingMutationStripeRoutesPaidUpgradeToContactBeforeReceipt(t *testing.T) {
	// Team is not purchasable in the shipped catalog yet, so a shipped-catalog
	// assertion here would pass through the unavailable-plan branch and prove
	// nothing. Parse a catalog with Team flipped available — exactly the state
	// this guard exists to make safe — so the guard itself is what is tested.
	var document map[string]any
	if err := json.Unmarshal(witself.PlansJSON, &document); err != nil {
		t.Fatal(err)
	}
	for _, entry := range document["plans"].([]any) {
		plan := entry.(map[string]any)
		if plan["id"] == "team" {
			plan["available"] = true
		}
	}
	flipped, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := plans.Parse(flipped)
	if err != nil {
		t.Fatal(err)
	}
	team, ok := catalog.Get("team")
	if !ok || !team.Purchasable() {
		t.Fatalf("test catalog Team = %+v ok=%v; want purchasable", team, ok)
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
		AccountID: "acct_paid_upgrade", Provider: "stripe",
		CustomerID: "cus_preview_only", Entitled: "standard", Applied: "standard",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), Record{
		AccountID: "acct_free_upgrade", Provider: "stripe",
		CustomerID: "cus_preview_free", Entitled: plans.Free, Applied: plans.Free,
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

	paid, err := manager.PreviewBillingMutation(
		ctx, "acct_paid_upgrade", "", BillingMutationCommand{
			Operation: BillingMutationUpgrade, Plan: "team",
			Reason: "Move from Professional to Team",
		})
	if err != nil {
		t.Fatal(err)
	}
	if paid.approval.ExecutionClass != BillingMutationExecutionUpgradeContact {
		t.Fatalf("paid-to-paid upgrade class = %q, want %q (a purchase would double-bill)",
			paid.approval.ExecutionClass, BillingMutationExecutionUpgradeContact)
	}
	if strings.Contains(strings.Join(paid.Effects, "\n"), "purchase") {
		t.Fatalf("paid-to-paid upgrade effects = %+v; want no purchase", paid.Effects)
	}

	free, err := manager.PreviewBillingMutation(
		ctx, "acct_free_upgrade", "", BillingMutationCommand{
			Operation: BillingMutationUpgrade, Plan: "standard",
			Reason: "Move from Personal to Professional",
		})
	if err != nil {
		t.Fatal(err)
	}
	if free.approval.ExecutionClass != BillingMutationExecutionUpgradeSelfServe {
		t.Fatalf("free-to-paid upgrade class = %q, want %q (the launch path must stay self-serve)",
			free.approval.ExecutionClass, BillingMutationExecutionUpgradeSelfServe)
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

func TestBillingMutationPreviewRequiresExactPendingCancellationTargetAndCapability(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	clock := &clock{t: time.Date(2026, 8, 11, 16, 30, 0, 0, time.UTC)}
	base := fake.New(fake.Config{
		Prices: catalog.Prices(), Interactive: true, Now: clock.now,
	})
	provider := &providerWithoutExactCancel{
		Provider: base, subscriber: base, canceller: base,
	}
	store := NewMemStore()
	put := func(accountID, objectID string) {
		t.Helper()
		if err := store.Put(context.Background(), Record{
			AccountID: accountID, Provider: "billing", CustomerID: "cus_" + accountID,
			Entitled: plans.Free, Applied: plans.Free,
			Pending: &Pending{
				Kind: PendingUpgrade, Plan: "standard",
				OperationID:      "bop_existing_" + accountID,
				ProviderObjectID: objectID,
				Requested:        clock.now(), Expires: clock.now().Add(time.Hour),
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("acct_no_exact", "cs_existing_exact")
	put("acct_no_target", "")
	manager, err := NewManager(Config{
		Catalog: catalog, Providers: map[string]billing.Provider{"billing": provider},
		Default: "billing", Store: store, Applier: &recApplier{}, Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	command := BillingMutationCommand{
		Operation: BillingMutationCancel,
		Reason:    "Cancel the unfinished purchase",
	}

	preview, err := manager.PreviewBillingMutation(
		context.Background(), "acct_no_exact", "", command)
	if err != nil || preview.Allowed || !strings.Contains(
		strings.Join(preview.Violations, "\n"), "exact pending cancellation",
	) {
		t.Fatalf("legacy-only cancellation preview = %+v, %v", preview, err)
	}
	preview, err = manager.PreviewBillingMutation(
		context.Background(), "acct_no_target", "", command)
	if err != nil || preview.Allowed || !strings.Contains(
		strings.Join(preview.Violations, "\n"), "operator reconciliation",
	) {
		t.Fatalf("targetless cancellation preview = %+v, %v", preview, err)
	}

	command.Confirmed = true
	command.IdempotencyKey = "targetless-cancel-2026-08-11"
	if _, err := manager.ExecuteBillingMutation(
		context.Background(), "acct_no_target", "",
		BillingActor{ID: "usr_owner_1", Role: "owner"}, command,
	); !errors.Is(err, ErrRefusal) {
		t.Fatalf("targetless cancellation apply = %v; want refusal", err)
	}
	operationID := billingMutationOperationID(
		"acct_no_target", command.IdempotencyKey)
	if _, ok, err := store.GetBillingMutation(
		context.Background(), operationID,
	); err != nil || ok {
		t.Fatalf("targetless refusal receipt exists=%t err=%v", ok, err)
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
	provider := &ambiguousDowngradeBillingProvider{
		Fake: base, driftAfterPrepare: true,
	}
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
		record.Pending.OperationID != operationID || !record.Pending.Effective.IsZero() ||
		record.Pending.PreparedEffective.IsZero() ||
		record.Pending.ProviderObjectID == "" ||
		record.Pending.ProviderPhase != pendingProviderPrepared ||
		!isPreparedDowngradeFence(record.Pending) {
		t.Fatalf("ambiguous account state = %+v ok=%t err=%v", record, ok, err)
	}
	preparedEffective := record.Pending.PreparedEffective
	if err := validatePendingCancellationTarget(
		*record.Pending.CancelPreviousTarget); err == nil {
		t.Fatal("prepared downgrade fence became a provider cancellation target")
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
		recovered.Outcome.Plan != "standard" ||
		!recovered.Outcome.Effective.Equal(preparedEffective) ||
		!recovered.Replayed {
		t.Fatalf("exact recovery = %+v, %v", recovered, err)
	}
	if provider.preparedCalls() != 1 {
		t.Fatalf("provider prepare calls = %d; want one durable target selection",
			provider.preparedCalls())
	}
	targets := provider.downgradeTargets()
	if provider.downgradeCalls() != 2 || len(targets) != 2 ||
		targets[0] != targets[1] ||
		targets[0].ProviderObjectID != record.Pending.ProviderObjectID ||
		!targets[0].Effective.Equal(preparedEffective) {
		t.Fatalf("provider retry targets = %+v calls=%d; want exact prepared replay",
			targets, provider.downgradeCalls())
	}
	record, ok, err = store.Get(ctx, "acct_drift")
	if err != nil || !ok || record.Pending == nil ||
		record.Pending.ProviderPhase != pendingProviderApplied ||
		!providerEffectApplied(record.Pending) ||
		record.Pending.ProviderObjectID != targets[0].ProviderObjectID ||
		!record.Pending.Effective.Equal(preparedEffective) ||
		!record.Pending.PreparedEffective.IsZero() ||
		record.Pending.CancelPrevious || record.Pending.CancelPreviousTarget != nil {
		t.Fatalf("confirmed prepared state = %+v ok=%t err=%v", record, ok, err)
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
	preparedRecord, ok, err := store.Get(ctx, "acct_webhook_recovery")
	if err != nil || !ok || preparedRecord.Pending == nil ||
		preparedRecord.Pending.ProviderPhase != pendingProviderPrepared ||
		preparedRecord.Pending.ProviderObjectID == "" ||
		preparedRecord.Pending.PreparedEffective.IsZero() ||
		!preparedRecord.Pending.Effective.IsZero() ||
		!isPreparedDowngradeFence(preparedRecord.Pending) {
		t.Fatalf("prepared account state = %+v ok=%t err=%v",
			preparedRecord, ok, err)
	}
	preparedEffective := preparedRecord.Pending.PreparedEffective

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
		!record.LastBillingMutationEffective.Equal(preparedEffective) {
		t.Fatalf("webhook tombstone = %+v ok=%t err=%v", record, ok, err)
	}

	recovered, err := manager.ExecuteBillingMutation(
		ctx, "acct_webhook_recovery", "owner@example.com", actor, command)
	if err != nil || recovered.Outcome.Kind != "scheduled" ||
		recovered.Outcome.Plan != plans.Free ||
		!recovered.Outcome.Effective.Equal(preparedEffective) || !recovered.Replayed {
		t.Fatalf("webhook recovery = %+v, %v", recovered, err)
	}
	if provider.downgradeCalls() != 1 {
		t.Fatalf("provider downgrade calls = %d; want one", provider.downgradeCalls())
	}
	receipt, ok, err := store.GetBillingMutation(ctx, operationID)
	if err != nil || !ok || receipt.Status != BillingMutationCompleted ||
		receipt.Result == nil || receipt.Result.Kind != BillingMutationResultScheduled ||
		receipt.Result.Plan != plans.Free || receipt.Result.Effective == nil ||
		!receipt.Result.Effective.Equal(preparedEffective) {
		t.Fatalf("completed receipt = %+v ok=%t err=%v", receipt, ok, err)
	}
}

func TestBillingMutationCancelWaitsForTerminalDowngradeWebhook(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	clock := &clock{t: time.Date(2026, 8, 11, 17, 45, 0, 0, time.UTC)}
	base := fake.New(fake.Config{Prices: catalog.Prices(), Now: clock.now})
	provider := &alreadyResolvedExactCancelProvider{Fake: base}
	ctx := context.Background()
	accountID := "acct_terminal_downgrade_cancel"
	customerID, err := base.EnsureCustomer(ctx, accountID, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.SubscribeIdempotent(
		ctx, customerID, "standard", "bop_seed_terminal_cancel",
	); err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	if err := store.Put(ctx, Record{
		AccountID: accountID, Provider: "billing", CustomerID: customerID,
		Entitled: "standard", Applied: "standard", EntitledAt: clock.now(),
	}); err != nil {
		t.Fatal(err)
	}
	manager := newBillingMutationManagerWithProvider(
		t, catalog, store, provider, clock)
	actor := BillingActor{ID: "usr_owner_1", Role: "owner"}
	downgrade := BillingMutationCommand{
		Operation: BillingMutationDowngrade, Plan: plans.Free,
		Reason: "Return to Personal at period end", Confirmed: true,
		IdempotencyKey: "terminal-downgrade-2026-08-11",
	}
	scheduled, err := manager.ExecuteBillingMutation(
		ctx, accountID, "owner@example.com", actor, downgrade)
	if err != nil || scheduled.Outcome.Kind != "scheduled" {
		t.Fatalf("schedule downgrade = %+v, %v", scheduled, err)
	}
	downgradeOperationID := billingMutationOperationID(
		accountID, downgrade.IdempotencyKey)
	before, ok, err := store.Get(ctx, accountID)
	if err != nil || !ok || before.Pending == nil ||
		before.Pending.OperationID != downgradeOperationID ||
		before.Pending.ProviderPhase != pendingProviderApplied ||
		!providerEffectApplied(before.Pending) {
		t.Fatalf("scheduled pending = %+v ok=%t err=%v", before, ok, err)
	}
	clock.t = before.Pending.Effective.Add(-time.Minute)

	cancel := BillingMutationCommand{
		Operation: BillingMutationCancel,
		Reason:    "Cancel period-end downgrade", Confirmed: true,
		IdempotencyKey: "terminal-cancel-2026-08-11",
	}
	if _, err := manager.ExecuteBillingMutation(
		ctx, accountID, "owner@example.com", actor, cancel,
	); !errors.Is(err, billing.ErrPendingAlreadyResolved) {
		t.Fatalf("terminal cancel = %v; want pending already resolved", err)
	}
	after, ok, err := store.Get(ctx, accountID)
	if err != nil || !ok || after.Pending == nil ||
		after.Pending.OperationID != before.Pending.OperationID ||
		after.Pending.ProviderObjectID != before.Pending.ProviderObjectID ||
		!after.Pending.Effective.Equal(before.Pending.Effective) ||
		after.Pending.ProviderPhase != pendingProviderApplied {
		t.Fatalf("terminal cancel changed pending = %+v ok=%t err=%v", after, ok, err)
	}
	cancelOperationID := billingMutationOperationID(
		accountID, cancel.IdempotencyKey)
	receipt, ok, err := store.GetBillingMutation(ctx, cancelOperationID)
	if err != nil || !ok || receipt.Status != BillingMutationPending {
		t.Fatalf("terminal cancel receipt = %+v ok=%t err=%v", receipt, ok, err)
	}
	if provider.cancelCalls() != 1 {
		t.Fatalf("terminal provider cancel calls = %d; want one", provider.cancelCalls())
	}

	clock.t = before.Pending.Effective.Add(time.Minute)
	events := base.ApplyDue()
	if len(events) != 1 || events[0].Type != billing.EventSubscriptionCanceled {
		t.Fatalf("terminal period-end events = %+v", events)
	}
	if err := manager.OnEvents(ctx, "billing", events); err != nil {
		t.Fatal(err)
	}
	replayed, err := manager.ExecuteBillingMutation(
		ctx, accountID, "owner@example.com", actor, cancel)
	if err != nil || !replayed.Replayed || replayed.Outcome.Kind != "resolved" {
		t.Fatalf("terminal cancel recovery = %+v, %v", replayed, err)
	}
	if provider.cancelCalls() != 1 {
		t.Fatalf("terminal cancel retried provider after webhook: %d", provider.cancelCalls())
	}
	receipt, ok, err = store.GetBillingMutation(ctx, cancelOperationID)
	if err != nil || !ok || receipt.Status != BillingMutationCompleted ||
		receipt.Result == nil || receipt.Result.Kind != BillingMutationResultResolved ||
		receipt.Result.Cancelled {
		t.Fatalf("terminal cancel completed receipt = %+v ok=%t err=%v",
			receipt, ok, err)
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

func TestR2StorePreservesPreparedDowngradePhase(t *testing.T) {
	store := newR2Store(t)
	now := time.Date(2026, 8, 11, 18, 30, 0, 0, time.UTC)
	effective := now.AddDate(0, 1, 0)
	operationID := "bop_r2_prepared_downgrade"
	providerObjectID := "sub_r2_prepared"
	record := Record{
		AccountID: "acct_r2_prepared_downgrade", Provider: "billing",
		CustomerID: "cus_r2_prepared", Entitled: "standard", Applied: "standard",
		Pending: &Pending{
			Kind: PendingDowngrade, Plan: plans.Free,
			OperationID:       operationID,
			ProviderObjectID:  providerObjectID,
			PreparedEffective: effective,
			ProviderPhase:     pendingProviderPrepared,
			CancelPrevious:    true,
			CancelPreviousTarget: &billing.PendingCancellation{
				Kind:                preparedDowngradeFenceKind,
				ProviderObjectID:    providerObjectID,
				OriginalOperationID: operationID,
			},
			Requested: now,
		},
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(context.Background(), record.AccountID)
	if err != nil || !ok || got.Pending == nil ||
		got.Pending.Kind != PendingDowngrade ||
		got.Pending.OperationID != record.Pending.OperationID ||
		got.Pending.ProviderObjectID != record.Pending.ProviderObjectID ||
		got.Pending.ProviderPhase != pendingProviderPrepared ||
		!got.Pending.PreparedEffective.Equal(effective) ||
		!got.Pending.Effective.IsZero() || !got.Pending.CancelPrevious ||
		got.Pending.CancelPreviousTarget == nil ||
		*got.Pending.CancelPreviousTarget != *record.Pending.CancelPreviousTarget ||
		!got.Pending.Requested.Equal(now) {
		t.Fatalf("R2 prepared downgrade = %+v ok=%t err=%v", got.Pending, ok, err)
	}
	if !isPreparedDowngradeFence(got.Pending) {
		t.Fatalf("R2 prepared downgrade fence is incoherent: %+v", got.Pending)
	}
}

func TestProviderEffectAppliedRequiresCoherentState(t *testing.T) {
	effective := time.Date(2026, 9, 11, 18, 30, 0, 0, time.UTC)
	target := billing.PendingCancellation{
		Kind:             billing.PendingCancellationPeriodEnd,
		ProviderObjectID: "sub_coherent", OriginalOperationID: "bop_coherent",
	}
	tests := []struct {
		name    string
		pending *Pending
		want    bool
	}{
		{
			name: "applied",
			pending: &Pending{
				Kind: PendingDowngrade, ProviderObjectID: "sub_coherent",
				ProviderPhase: pendingProviderApplied, Effective: effective,
			},
			want: true,
		},
		{
			name: "legacy applied",
			pending: &Pending{
				Kind: PendingDowngrade, ProviderObjectID: "sub_legacy",
				Effective: effective,
			},
			want: true,
		},
		{
			name: "wrong kind",
			pending: &Pending{
				Kind: PendingUpgrade, ProviderObjectID: "sub_coherent",
				ProviderPhase: pendingProviderApplied, Effective: effective,
			},
		},
		{
			name: "invalid provider object",
			pending: &Pending{
				Kind: PendingDowngrade, ProviderObjectID: "sub invalid",
				ProviderPhase: pendingProviderApplied, Effective: effective,
			},
		},
		{
			name: "zero effective",
			pending: &Pending{
				Kind: PendingDowngrade, ProviderObjectID: "sub_coherent",
				ProviderPhase: pendingProviderApplied,
			},
		},
		{
			name: "prepared effective remains",
			pending: &Pending{
				Kind: PendingDowngrade, ProviderObjectID: "sub_coherent",
				ProviderPhase: pendingProviderApplied, Effective: effective,
				PreparedEffective: effective,
			},
		},
		{
			name: "cancel flag remains",
			pending: &Pending{
				Kind: PendingDowngrade, ProviderObjectID: "sub_coherent",
				ProviderPhase: pendingProviderApplied, Effective: effective,
				CancelPrevious: true,
			},
		},
		{
			name: "cancel target remains",
			pending: &Pending{
				Kind: PendingDowngrade, ProviderObjectID: "sub_coherent",
				ProviderPhase: pendingProviderApplied, Effective: effective,
				CancelPreviousTarget: &target,
			},
		},
		{
			name: "prepared phase",
			pending: &Pending{
				Kind: PendingDowngrade, ProviderObjectID: "sub_coherent",
				ProviderPhase: pendingProviderPrepared, Effective: effective,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := providerEffectApplied(test.pending); got != test.want {
				t.Fatalf("providerEffectApplied(%+v) = %t; want %t", test.pending, got, test.want)
			}
		})
	}
}

func TestLegacyAppliedDowngradeRemainsCancellable(t *testing.T) {
	effective := time.Date(2026, 9, 11, 18, 30, 0, 0, time.UTC)
	record := Record{
		AccountID: "acct_legacy_applied_downgrade", CustomerID: "cus_legacy",
		Pending: &Pending{
			Kind: PendingDowngrade, Plan: plans.Free,
			OperationID:      "bop_legacy_applied_downgrade",
			ProviderObjectID: "sub_legacy_applied", Effective: effective,
			// ProviderPhase is deliberately absent, matching pre-field data.
		},
	}
	if !providerEffectApplied(record.Pending) {
		t.Fatal("legacy exact applied downgrade was quarantined")
	}
	target, err := pendingCancellationTarget(record)
	if err != nil || target == nil ||
		target.Kind != billing.PendingCancellationPeriodEnd ||
		target.ProviderObjectID != record.Pending.ProviderObjectID ||
		target.OriginalOperationID != record.Pending.OperationID {
		t.Fatalf("legacy cancellation target = %+v, %v", target, err)
	}

	ambiguous := record
	ambiguous.Pending = &Pending{
		Kind: PendingDowngrade, Plan: plans.Free,
		OperationID: "bop_legacy_ambiguous_downgrade",
	}
	if providerEffectApplied(ambiguous.Pending) {
		t.Fatal("targetless legacy downgrade was treated as provider-applied")
	}
	if _, err := pendingCancellationTarget(ambiguous); err == nil {
		t.Fatal("targetless legacy downgrade did not fail closed")
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
