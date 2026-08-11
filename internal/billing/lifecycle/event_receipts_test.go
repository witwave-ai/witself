package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
)

func durableTestEvent(eventID, customerID string) billing.Event {
	return billing.Event{
		Type:             billing.EventPaymentFailed,
		CustomerID:       customerID,
		At:               time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
		ProviderEventID:  eventID,
		PayloadSHA256:    strings.Repeat("a", 64),
		ProviderObjectID: "in_" + eventID,
		SubscriptionID:   "sub_1",
	}
}

func testEventReceiptStoreContract(
	t *testing.T,
	store EventReceiptStore,
) {
	t.Helper()
	ctx := context.Background()
	receipt, err := newEventReceipt(
		"stripe", durableTestEvent("evt_1", "cus_1"),
		time.Date(2026, 8, 10, 12, 0, 1, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	stored, created, err := store.ReceiveEvent(ctx, receipt)
	if err != nil || !created || stored.Version != 1 ||
		stored.Status != EventReceiptPending {
		t.Fatalf("ReceiveEvent first = %+v, created=%v, err=%v", stored, created, err)
	}
	again, created, err := store.ReceiveEvent(ctx, receipt)
	if err != nil || created || !sameEventReceiptIdentity(stored, again) {
		t.Fatalf("ReceiveEvent replay = %+v, created=%v, err=%v", again, created, err)
	}

	conflict := receipt
	conflict.Event.PayloadSHA256 = strings.Repeat("b", 64)
	if _, _, err := store.ReceiveEvent(ctx, conflict); !errors.Is(err, ErrEventReceiptConflict) {
		t.Fatalf("conflicting event identity = %v; want ErrEventReceiptConflict", err)
	}
	conflict = receipt
	conflict.Event.SubscriptionID = "sub_changed_normalization"
	if _, _, err := store.ReceiveEvent(ctx, conflict); !errors.Is(err, ErrEventReceiptConflict) {
		t.Fatalf("conflicting normalized event = %v; want ErrEventReceiptConflict", err)
	}

	pending, err := store.PendingEvents(ctx, "stripe", "cus_1", 10)
	if err != nil || len(pending) != 1 ||
		pending[0].Event.ProviderEventID != "evt_1" {
		t.Fatalf("PendingEvents = %+v, %v", pending, err)
	}
	claimAt := time.Date(2026, 8, 10, 12, 0, 1, 0, time.UTC)
	claimed, acquired, err := store.ClaimEvent(
		ctx, stored, "ecl_contract_owner", claimAt,
		claimAt.Add(eventReceiptProcessingLease))
	if err != nil || !acquired || claimed.ClaimToken != "ecl_contract_owner" ||
		claimed.ClaimGeneration != 1 || claimed.LeaseExpiresAt == nil {
		t.Fatalf("ClaimEvent first = %+v, acquired=%v, err=%v", claimed, acquired, err)
	}
	busy, acquired, err := store.ClaimEvent(
		ctx, stored, "ecl_other_owner", claimAt.Add(time.Second),
		claimAt.Add(eventReceiptProcessingLease+time.Second))
	if err != nil || acquired || busy.ClaimToken != claimed.ClaimToken ||
		busy.ClaimGeneration != claimed.ClaimGeneration {
		t.Fatalf("ClaimEvent live contention = %+v, acquired=%v, err=%v", busy, acquired, err)
	}
	resolvedEvent := claimed.Event
	resolution := EventReceiptResolution{
		Decision: "applied", AccountID: "acct_1", Event: &resolvedEvent,
		ResolvedAt: claimAt,
	}
	claimed, err = store.PinEventResolution(ctx, claimed, resolution)
	if err != nil || claimed.Resolution == nil ||
		!sameEventReceiptResolution(*claimed.Resolution, resolution) {
		t.Fatalf("PinEventResolution = %+v, err=%v", claimed, err)
	}
	if replay, err := store.PinEventResolution(ctx, claimed, resolution); err != nil ||
		replay.Resolution == nil ||
		!sameEventReceiptResolution(*replay.Resolution, resolution) {
		t.Fatalf("PinEventResolution replay = %+v, err=%v", replay, err)
	}
	conflictingResolution := resolution
	conflictingResolution.AccountID = "acct_other"
	if _, err := store.PinEventResolution(
		ctx, claimed, conflictingResolution); !errors.Is(err, ErrEventReceiptConflict) {
		t.Fatalf("PinEventResolution mismatch = %v; want ErrEventReceiptConflict", err)
	}
	processedAt := time.Date(2026, 8, 10, 12, 0, 2, 0, time.UTC)
	if err := store.CompleteEvent(ctx, claimed, processedAt); err != nil {
		t.Fatalf("CompleteEvent: %v", err)
	}
	// Completion and its pending-index cleanup are independently retryable.
	if err := store.CompleteEvent(ctx, claimed, processedAt); err != nil {
		t.Fatalf("CompleteEvent replay: %v", err)
	}
	conflictingClaim := claimed
	conflictingClaim.Resolution = &conflictingResolution
	if err := store.CompleteEvent(
		ctx, conflictingClaim, processedAt); !errors.Is(err, ErrEventReceiptConflict) {
		t.Fatalf("CompleteEvent resolution mismatch = %v; want ErrEventReceiptConflict", err)
	}
	pending, err = store.PendingEvents(ctx, "stripe", "cus_1", 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("PendingEvents after completion = %+v, %v", pending, err)
	}
	processed, created, err := store.ReceiveEvent(ctx, receipt)
	if err != nil || created || processed.Status != EventReceiptProcessed ||
		processed.Decision != "applied" || processed.ProcessedAt == nil {
		t.Fatalf("processed replay = %+v, created=%v, err=%v", processed, created, err)
	}
}

func testEventReceiptLeaseRecovery(
	t *testing.T,
	store EventReceiptStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 13, 0, 0, 0, time.UTC)
	receipt, err := newEventReceipt(
		"stripe", durableTestEvent("evt_lease", "cus_lease"), now)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.ReceiveEvent(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	first, acquired, err := store.ClaimEvent(
		ctx, stored, "ecl_first", now, now.Add(eventReceiptProcessingLease))
	if err != nil || !acquired {
		t.Fatalf("first claim = %+v, acquired=%v, err=%v", first, acquired, err)
	}
	// Release makes a provider/fold failure immediately retryable without
	// waiting for the crash-recovery lease.
	if err := store.ReleaseEvent(ctx, first); err != nil {
		t.Fatalf("release first claim: %v", err)
	}
	second, acquired, err := store.ClaimEvent(
		ctx, stored, "ecl_second", now.Add(time.Second),
		now.Add(time.Second).Add(eventReceiptProcessingLease))
	if err != nil || !acquired || second.ClaimGeneration != first.ClaimGeneration+1 {
		t.Fatalf("claim after release = %+v, acquired=%v, err=%v", second, acquired, err)
	}
	resolvedEvent := second.Event
	resolution := EventReceiptResolution{
		Decision: "applied", AccountID: "acct_lease", Event: &resolvedEvent,
		ResolvedAt: now.Add(time.Second),
	}
	second, err = store.PinEventResolution(ctx, second, resolution)
	if err != nil {
		t.Fatalf("pin before crash: %v", err)
	}
	// A crashed owner is recoverable after expiry. The generation increment
	// fences both completion and release from the expired owner.
	recoveredAt := *second.LeaseExpiresAt
	recovered, acquired, err := store.ClaimEvent(
		ctx, stored, "ecl_recovered", recoveredAt,
		recoveredAt.Add(eventReceiptProcessingLease))
	if err != nil || !acquired || recovered.ClaimGeneration != second.ClaimGeneration+1 {
		t.Fatalf("expired recovery = %+v, acquired=%v, err=%v", recovered, acquired, err)
	}
	if recovered.Resolution == nil ||
		!sameEventReceiptResolution(*recovered.Resolution, resolution) {
		t.Fatalf("expired recovery lost pinned resolution: %+v", recovered.Resolution)
	}
	if err := store.ReleaseEvent(ctx, second); !errors.Is(err, ErrEventReceiptClaimLost) {
		t.Fatalf("stale release = %v; want ErrEventReceiptClaimLost", err)
	}
	if err := store.CompleteEvent(ctx, second, recoveredAt); !errors.Is(err, ErrEventReceiptClaimLost) {
		t.Fatalf("stale completion = %v; want ErrEventReceiptClaimLost", err)
	}
	if err := store.CompleteEvent(ctx, recovered, recoveredAt); err != nil {
		t.Fatalf("recovered completion: %v", err)
	}
}

func testEventReceiptSameTokenExpiryAndReleaseReplay(
	t *testing.T,
	store EventReceiptStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 13, 30, 0, 0, time.UTC)
	receipt, err := newEventReceipt(
		"stripe", durableTestEvent("evt_same_token", "cus_same_token"), now)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.ReceiveEvent(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	first, acquired, err := store.ClaimEvent(
		ctx, stored, "ecl_same", now, now.Add(eventReceiptProcessingLease))
	if err != nil || !acquired {
		t.Fatalf("first same-token claim = %+v, acquired=%v, err=%v", first, acquired, err)
	}
	expiredAt := *first.LeaseExpiresAt
	renewed, acquired, err := store.ClaimEvent(
		ctx, stored, "ecl_same", expiredAt,
		expiredAt.Add(eventReceiptProcessingLease))
	if err != nil || !acquired ||
		renewed.ClaimGeneration != first.ClaimGeneration+1 {
		t.Fatalf("expired same-token claim = %+v, acquired=%v, err=%v", renewed, acquired, err)
	}
	if err := store.ReleaseEvent(ctx, first); !errors.Is(err, ErrEventReceiptClaimLost) {
		t.Fatalf("expired same-token release = %v; want ErrEventReceiptClaimLost", err)
	}
	if err := store.ReleaseEvent(ctx, renewed); err != nil {
		t.Fatalf("release renewed claim: %v", err)
	}
	if err := store.ReleaseEvent(ctx, renewed); err != nil {
		t.Fatalf("release acknowledgement replay: %v", err)
	}
}

func TestMemStoreEventReceiptContract(t *testing.T) {
	testEventReceiptStoreContract(t, NewMemStore())
	testEventReceiptLeaseRecovery(t, NewMemStore())
	testEventReceiptSameTokenExpiryAndReleaseReplay(t, NewMemStore())
}

func TestR2StoreEventReceiptContract(t *testing.T) {
	testEventReceiptStoreContract(t, newR2Store(t))
	testEventReceiptLeaseRecovery(t, newR2Store(t))
	testEventReceiptSameTokenExpiryAndReleaseReplay(t, newR2Store(t))
}

func TestR2StoreConcurrentEventReceiptCreate(t *testing.T) {
	store := newR2Store(t)
	receipt, err := newEventReceipt(
		"stripe", durableTestEvent("evt_race", "cus_race"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	const writers = 12
	var created atomic.Int64
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, won, err := store.ReceiveEvent(context.Background(), receipt)
			if won {
				created.Add(1)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("ReceiveEvent concurrently: %v", err)
	}
	if got := created.Load(); got != 1 {
		t.Fatalf("create winners = %d; want exactly 1", got)
	}
	pending, err := store.PendingEvents(
		context.Background(), "stripe", "cus_race", 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending after concurrent receive = %+v, %v", pending, err)
	}
}

func testConcurrentEventReceiptSemanticConflict(
	t *testing.T,
	store EventReceiptStore,
) {
	t.Helper()
	now := time.Date(2026, 8, 10, 13, 45, 0, 0, time.UTC)
	first, err := newEventReceipt(
		"stripe", durableTestEvent("evt_semantic_race", "cus_semantic_race"), now)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Event.SubscriptionID = "sub_conflicting_normalization"
	start := make(chan struct{})
	results := make(chan error, 2)
	var created atomic.Int64
	var wg sync.WaitGroup
	for _, receipt := range []EventReceipt{first, second} {
		receipt := receipt
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, won, err := store.ReceiveEvent(context.Background(), receipt)
			if won {
				created.Add(1)
			}
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	conflicts := 0
	for err := range results {
		if errors.Is(err, ErrEventReceiptConflict) {
			conflicts++
		} else if err != nil {
			t.Fatalf("concurrent semantic receive: %v", err)
		}
	}
	if created.Load() != 1 || conflicts != 1 {
		t.Fatalf("semantic race created=%d conflicts=%d; want 1/1",
			created.Load(), conflicts)
	}
}

func TestMemStoreConcurrentEventReceiptSemanticConflict(t *testing.T) {
	testConcurrentEventReceiptSemanticConflict(t, NewMemStore())
}

func TestR2StoreConcurrentEventReceiptSemanticConflict(t *testing.T) {
	testConcurrentEventReceiptSemanticConflict(t, newR2Store(t))
}

func testConcurrentEventReceiptClaim(t *testing.T, store EventReceiptStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 14, 0, 0, 0, time.UTC)
	receipt, err := newEventReceipt(
		"stripe", durableTestEvent("evt_claim_race", "cus_claim_race"), now)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.ReceiveEvent(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	const writers = 12
	type result struct {
		receipt EventReceipt
		won     bool
		err     error
	}
	results := make(chan result, writers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claimed, won, err := store.ClaimEvent(
				context.Background(), stored,
				fmt.Sprintf("ecl_racer_%02d", i), now,
				now.Add(eventReceiptProcessingLease))
			results <- result{receipt: claimed, won: won, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var winner EventReceipt
	wins := 0
	for result := range results {
		if result.err != nil {
			t.Errorf("concurrent ClaimEvent: %v", result.err)
		}
		if result.won {
			winner = result.receipt
			wins++
		}
	}
	if wins != 1 || winner.ClaimGeneration != 1 || winner.ClaimToken == "" {
		t.Fatalf("claim winners = %d; winner=%+v", wins, winner)
	}
	winner, err = store.PinEventResolution(ctx, winner, EventReceiptResolution{
		Decision: "ignored", IgnoreReason: "concurrent_winner", ResolvedAt: now,
	})
	if err != nil {
		t.Fatalf("pin concurrent winner: %v", err)
	}
	if err := store.CompleteEvent(ctx, winner, now.Add(time.Second)); err != nil {
		t.Fatalf("complete concurrent winner: %v", err)
	}
}

func TestMemStoreConcurrentEventReceiptClaim(t *testing.T) {
	testConcurrentEventReceiptClaim(t, NewMemStore())
}

func TestR2StoreConcurrentEventReceiptClaim(t *testing.T) {
	testConcurrentEventReceiptClaim(t, newR2Store(t))
}

func TestR2PendingOperationPhaseRoundTrip(t *testing.T) {
	store := newR2Store(t)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	record := Record{
		AccountID: "acct_pending_operation",
		Entitled:  "free",
		Applied:   "free",
		Pending: &Pending{
			Kind: PendingUpgrade, Plan: "standard",
			OperationID: "bop_round_trip", CancelPrevious: true,
			Requested: now, Expires: now.Add(time.Hour),
		},
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(context.Background(), record.AccountID)
	if err != nil || !ok || got.Pending == nil ||
		got.Pending.OperationID != "bop_round_trip" ||
		!got.Pending.CancelPrevious {
		t.Fatalf("pending operation round trip = %+v, ok=%v, err=%v", got.Pending, ok, err)
	}
}
