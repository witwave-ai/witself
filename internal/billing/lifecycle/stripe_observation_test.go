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

func TestStripeWebhookReplayObservationExcludesOtherProvidersAndRecovery(t *testing.T) {
	ctx := context.Background()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	provider := fake.New(fake.Config{Prices: catalog.Prices()})
	manager, err := NewManager(Config{
		Catalog: catalog, Providers: map[string]billing.Provider{"stripe": provider, "fake": provider},
		Default: "stripe", Store: store, Applier: &recApplier{},
	})
	if err != nil {
		t.Fatal(err)
	}
	stripeEvent := durableTestEvent("evt_private_stripe", "cus_private_stripe")
	fakeEvent := durableTestEvent("evt_private_fake", "cus_private_fake")
	for _, name := range []string{"stripe", "fake"} {
		event := stripeEvent
		if name == "fake" {
			event = fakeEvent
		}
		for range 2 {
			if err := manager.OnEvents(ctx, name, []billing.Event{event}); err != nil {
				t.Fatal("fixture receipt processing failed")
			}
		}
	}
	if got := manager.StripeWebhookReplays(); got != 1 {
		t.Fatalf("replay count = %d; want one Stripe replay only", got)
	}
	receipt, err := newEventReceipt("stripe", stripeEvent, time.Now())
	if err != nil {
		t.Fatal("fixture receipt was invalid")
	}
	processed, _, err := store.ReceiveEvent(ctx, receipt)
	if err != nil || processed.Status != EventReceiptProcessed {
		t.Fatal("fixture receipt was not already processed")
	}
	if err := manager.processReceipt(ctx, store, processed, false); err != nil {
		t.Fatal("background receipt-index cleanup failed")
	}
	if got := manager.StripeWebhookReplays(); got != 1 {
		t.Fatalf("background cleanup counted as webhook replay: %d", got)
	}
	const concurrentReplays = 64
	var wg sync.WaitGroup
	for range concurrentReplays {
		wg.Go(func() {
			if err := manager.OnEvents(ctx, "stripe", []billing.Event{stripeEvent}); err != nil {
				t.Error("concurrent replay changed processing result")
			}
			_ = manager.StripeWebhookReplays()
		})
	}
	wg.Wait()
	if got := manager.StripeWebhookReplays(); got != concurrentReplays+1 {
		t.Fatalf("concurrent replay increments lost: %d", got)
	}
}

type stripeReplayClaimStore struct {
	EventReceiptStore
	processed   EventReceipt
	completeErr error
	completed   int
}

func (s *stripeReplayClaimStore) ClaimEvent(context.Context, EventReceipt, string, time.Time, time.Time) (EventReceipt, bool, error) {
	return s.processed, false, nil
}

func (s *stripeReplayClaimStore) CompleteEvent(context.Context, EventReceipt, time.Time) error {
	s.completed++
	return s.completeErr
}

func TestStripeWebhookReplayObservationClaimRacePreservesCleanupResult(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	pending, err := newEventReceipt("stripe", durableTestEvent("evt_private_race", "cus_private_race"), now)
	if err != nil {
		t.Fatal("fixture receipt was invalid")
	}
	processed := pending
	processed.Status = EventReceiptProcessed
	processed.Decision = "ignored"
	processed.ProcessedAt = &now
	processed.Resolution = &EventReceiptResolution{Decision: "ignored", IgnoreReason: "unknown_customer", ResolvedAt: now}
	for _, fails := range []bool{false, true} {
		for _, webhookDelivery := range []bool{false, true} {
			var completeErr error
			if fails {
				completeErr = errors.New("fixture cleanup failed")
			}
			store := &stripeReplayClaimStore{processed: processed, completeErr: completeErr}
			manager := &Manager{cfg: Config{Now: func() time.Time { return now }}}
			if err := manager.processReceipt(ctx, store, pending, webhookDelivery); !errors.Is(err, completeErr) {
				t.Fatal("observing replay changed the existing cleanup result")
			}
			want := uint64(0)
			if webhookDelivery {
				want = 1
			}
			if manager.StripeWebhookReplays() != want || store.completed != 1 {
				t.Fatalf("claim-race replay=%d cleanup calls=%d", manager.StripeWebhookReplays(), store.completed)
			}
		}
	}
}
