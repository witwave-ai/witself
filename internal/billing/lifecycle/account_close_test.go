package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/plans"
)

type accountCloseProvider struct {
	*fake.Fake
	cancel func(context.Context, string, string, string) error
}

func (p *accountCloseProvider) CancelForAccountClosure(ctx context.Context, customerID, accountID, operationID string) error {
	return p.cancel(ctx, customerID, accountID, operationID)
}

func accountCloseManager(t *testing.T, store Store, cancel func(context.Context, string, string, string) error) (*Manager, *recApplier) {
	t.Helper()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	provider := &accountCloseProvider{
		Fake: fake.New(fake.Config{Prices: catalog.Prices(), Interactive: true}), cancel: cancel,
	}
	applier := &recApplier{}
	manager, err := NewManager(Config{
		Catalog: catalog, Store: store, Providers: map[string]billing.Provider{"fake": provider},
		Default: "fake", Applier: applier,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, applier
}

func TestAccountCloseBillingRetryAndPermanentFence(t *testing.T) {
	for _, kind := range []string{"memory", "r2"} {
		t.Run(kind, func(t *testing.T) {
			var store Store = NewMemStore()
			if kind == "r2" {
				store = newR2Store(t)
			}
			ctx := context.Background()
			calls := 0
			var operation string
			cancel := func(_ context.Context, customerID, accountID, operationID string) error {
				if customerID != "cus_close" || accountID != "acct_close" {
					t.Fatal("cancellation routed to wrong account")
				}
				if operation != "" && operation != operationID {
					t.Fatal("retry changed provider operation identity")
				}
				operation = operationID
				calls++
				if calls == 1 {
					return context.DeadlineExceeded
				}
				return nil
			}
			manager, applier := accountCloseManager(t, store, cancel)
			if err := store.Put(ctx, Record{AccountID: "acct_close", Provider: "fake", CustomerID: "cus_close", Entitled: "standard", Applied: "standard"}); err != nil {
				t.Fatal(err)
			}
			if err := manager.CloseAccountBilling(ctx, "acct_close"); !errors.Is(err, ErrProviderRequest) {
				t.Fatalf("ambiguous close = %v", err)
			}
			r, _, err := store.Get(ctx, "acct_close")
			if err != nil || r.BillingClosureOperationID == "" || r.BillingClosureStartedAt.IsZero() || !r.BillingClosedAt.IsZero() {
				t.Fatalf("missing retry fence or premature completion: %v", err)
			}
			if _, err := manager.RequestUpgrade(ctx, "acct_close", "owner@example.com", "professional"); !errors.Is(err, ErrAccountBillingClosed) {
				t.Fatalf("upgrade after closure began = %v", err)
			}
			if _, err := manager.CreateBillingSetup(ctx, "acct_close", "owner@example.com"); !errors.Is(err, ErrAccountBillingClosed) {
				t.Fatalf("setup after closure began = %v", err)
			}
			if _, err := manager.CreateBillingPortal(ctx, "acct_close", "owner@example.com"); !errors.Is(err, ErrAccountBillingClosed) {
				t.Fatalf("portal after closure began = %v", err)
			}
			if err := manager.CancelPending(ctx, "acct_close"); !errors.Is(err, ErrAccountBillingClosed) {
				t.Fatalf("cancel after closure began = %v", err)
			}
			if err := manager.ReconcileAccount(ctx, "acct_close"); err != nil {
				t.Fatal(err)
			}
			if err := manager.OnEvents(ctx, "fake", []billing.Event{{Type: billing.EventSubscriptionActivated, CustomerID: "cus_close", Plan: "standard", At: time.Now()}}); err != nil {
				t.Fatal(err)
			}
			if len(applier.calls) != 0 {
				t.Fatal("closing account applied a cell plan")
			}
			// A new manager proves recovery depends on storage, not local memory.
			manager, applier = accountCloseManager(t, store, cancel)
			if err := manager.CloseAccountBilling(ctx, "acct_close"); err != nil {
				t.Fatal(err)
			}
			if err := manager.CloseAccountBilling(ctx, "acct_close"); err != nil {
				t.Fatal(err)
			}
			r, _, err = store.Get(ctx, "acct_close")
			if err != nil || r.BillingClosedAt.IsZero() || r.Entitled != plans.Free || r.Pending != nil || calls != 2 || len(applier.calls) != 0 {
				t.Fatalf("close completion/replay failed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestAccountCloseBillingDoesNotOvertakeUnresolvedPurchase(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newBillingMutationManager(t, true)
	command := BillingMutationCommand{Operation: BillingMutationPlanUpgrade, Plan: "standard", Reason: "purchase", Confirmed: true, IdempotencyKey: "close-pending-operation-0001"}
	op := billingMutationOperationID("acct_pending", command.IdempotencyKey)
	now := manager.cfg.Now().UTC()
	lane, ok, err := store.ClaimBillingMutationAccount(ctx, "acct_pending", op, 0, "claim_close_test", now, now.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("claim = %t, %v", ok, err)
	}
	receipt, err := buildBillingMutationReceipt("acct_pending", "", BillingActor{ID: "owner", Role: "account_owner"}, command, lane.OperationGeneration,
		billingMutationApproval{ExecutionClass: BillingMutationExecutionUpgradeSelfServe, ApprovedPriceCents: 100, ApprovedCurrency: "usd"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReceiveBillingMutation(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseBillingMutationAccount(ctx, lane, now); err != nil {
		t.Fatal(err)
	}
	if err := manager.CloseAccountBilling(ctx, "acct_pending"); !errors.Is(err, ErrBillingMutationInProgress) {
		t.Fatalf("unresolved purchase close = %v", err)
	}
	if _, exists, err := store.Get(ctx, "acct_pending"); err != nil || exists {
		t.Fatalf("busy close persisted a closure marker: exists=%t err=%v", exists, err)
	}
}

func TestAccountCloseBillingBlocksCompletedCheckoutReplay(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	manager, _ := accountCloseManager(t, store, func(context.Context, string, string, string) error { return nil })
	command := BillingMutationCommand{Operation: BillingMutationSetup, Reason: "set up payment", Confirmed: true, IdempotencyKey: "closure-checkout-replay-0001"}
	actor := BillingActor{ID: "owner", Role: "account_owner"}
	if _, err := manager.ExecuteBillingMutation(ctx, "acct_replay", "owner@example.com", actor, command); err != nil {
		t.Fatal(err)
	}
	now := manager.cfg.Now().UTC()
	lane, acquired, err := store.ClaimBillingMutationAccount(ctx, "acct_replay", accountClosureOperationID("acct_replay"), 0, "close_before_record", now, now.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("reserve close before Record write: acquired=%t err=%v", acquired, err)
	}
	if _, err := manager.ExecuteBillingMutation(ctx, "acct_replay", "owner@example.com", actor, command); !errors.Is(err, ErrAccountBillingClosed) {
		t.Fatalf("hosted action replay during pre-Record close crash = %v", err)
	}
	if err := store.ReleaseBillingMutationAccount(ctx, lane, manager.cfg.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := manager.CloseAccountBilling(ctx, "acct_replay"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ExecuteBillingMutation(ctx, "acct_replay", "owner@example.com", actor, command); !errors.Is(err, ErrAccountBillingClosed) {
		t.Fatalf("old hosted action replay = %v", err)
	}
}

func TestAccountCloseBillingSerializesConcurrentClose(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	manager, _ := accountCloseManager(t, store, func(context.Context, string, string, string) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	})
	if err := store.Put(ctx, Record{AccountID: "acct_close", CustomerID: "cus_close", Provider: "fake", Entitled: plans.Free, Applied: plans.Free}); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- manager.CloseAccountBilling(ctx, "acct_close") }()
	<-entered
	if err := manager.CloseAccountBilling(ctx, "acct_close"); !errors.Is(err, ErrBillingMutationInProgress) {
		t.Errorf("concurrent close = %v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestAccountCloseBillingProviderlessAndInconsistentState(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	manager, err := NewManager(Config{Catalog: catalog, Store: store, Applier: &recApplier{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := manager.CloseAccountBilling(ctx, "acct_free"); err != nil {
		t.Fatalf("providerless free closure = %v", err)
	}
	if err := store.Put(ctx, Record{AccountID: "acct_ambiguous", Entitled: "standard", Applied: "standard"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.CloseAccountBilling(ctx, "acct_ambiguous"); !errors.Is(err, ErrRefusal) {
		t.Fatalf("missing paid customer closure = %v", err)
	}
}

func TestAccountCloseBillingReservationCannotBeOvertaken(t *testing.T) {
	for _, kind := range []string{"memory", "r2"} {
		t.Run(kind, func(t *testing.T) {
			var store BillingMutationStore = NewMemStore()
			if kind == "r2" {
				store = newR2Store(t)
			}
			ctx := context.Background()
			now := time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC)
			closeOperation := accountClosureOperationID("acct_reservation")
			lease, acquired, err := store.ClaimBillingMutationAccount(ctx, "acct_reservation", closeOperation, 0, "close_claim", now, now.Add(time.Minute))
			if err != nil || !acquired {
				t.Fatalf("close reservation = %t %v", acquired, err)
			}
			// Simulate a crash before the account Record exists. No receipt is
			// present, but expiry must never let a purchase overtake closure.
			later := now.Add(2 * time.Minute)
			if _, acquired, err := store.ClaimBillingMutationAccount(ctx, "acct_reservation", "bop_new_purchase", 0, "purchase_claim", later, later.Add(time.Minute)); !errors.Is(err, ErrAccountBillingClosed) || acquired {
				t.Fatalf("expired close reservation overtaken: acquired=%t err=%v", acquired, err)
			}
			if err := store.ReleaseBillingMutationAccount(ctx, lease, later); err != nil {
				t.Fatal(err)
			}
			if _, acquired, err := store.ClaimBillingMutationAccount(ctx, "acct_reservation", "bop_new_purchase", 0, "purchase_claim", later, later.Add(time.Minute)); !errors.Is(err, ErrAccountBillingClosed) || acquired {
				t.Fatalf("released close reservation overtaken: acquired=%t err=%v", acquired, err)
			}
			if _, acquired, err := store.ClaimBillingMutationAccount(ctx, "acct_reservation", closeOperation, 0, "close_retry", later, later.Add(time.Minute)); err != nil || !acquired {
				t.Fatalf("exact close retry = %t %v", acquired, err)
			}
			manager, _ := accountCloseManager(t, store.(Store), func(context.Context, string, string, string) error {
				t.Fatal("free closure unexpectedly called provider")
				return nil
			})
			manager.cfg.Now = func() time.Time { return later.Add(2 * time.Minute) }
			if err := manager.CloseAccountBilling(ctx, "acct_reservation"); err != nil {
				t.Fatalf("recover closure after expired reservation: %v", err)
			}
			if err := store.(Store).Put(ctx, Record{AccountID: "acct_reservation", Entitled: "standard"}); !errors.Is(err, ErrStale) {
				t.Fatalf("stale pre-closure record overwrote fence: %v", err)
			}
		})
	}
}

func TestAccountCloseBillingRetiresLateUnexecutedPurchaseReceipt(t *testing.T) {
	ctx := context.Background()
	manager, store, clock := newBillingMutationManager(t, true)
	now := clock.now()
	command := BillingMutationCommand{Operation: BillingMutationSetup, Reason: "payment setup", Confirmed: true, IdempotencyKey: "late-after-closure-receipt"}
	operation := billingMutationOperationID("acct_late", command.IdempotencyKey)
	lane, acquired, err := store.ClaimBillingMutationAccount(ctx, "acct_late", operation, 0, "stale_worker", now, now.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("purchase reservation = %t %v", acquired, err)
	}
	clock.t = now.Add(2 * time.Minute)
	if err := manager.CloseAccountBilling(ctx, "acct_late"); err != nil {
		t.Fatal(err)
	}
	// A worker delayed before receipt creation resumes after closure. The
	// generation handshake fences its provider work; recovery retires the row.
	receipt, err := buildBillingMutationReceipt("acct_late", "", BillingActor{ID: "owner", Role: "account_owner"}, command, lane.OperationGeneration,
		billingMutationApproval{ExecutionClass: BillingMutationExecutionSetup}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReceiveBillingMutation(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	summary, err := manager.ReconcileBillingMutations(ctx)
	if err != nil || summary.Superseded != 1 || summary.Completed != 0 {
		t.Fatalf("late receipt recovery = %+v err=%v", summary, err)
	}
	stored, found, err := store.GetBillingMutation(ctx, operation)
	if err != nil || !found || stored.Status != BillingMutationSuperseded {
		t.Fatalf("late receipt was not durably retired: found=%t err=%v", found, err)
	}
}
