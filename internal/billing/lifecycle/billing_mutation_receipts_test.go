package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/blob"
)

func billingMutationTestReceipt(operationID string, now time.Time) BillingMutationReceipt {
	return BillingMutationReceipt{
		SchemaVersion:        billingMutationReceiptSchemaVersion,
		OperationID:          operationID,
		AccountID:            "acct_billing_mutation",
		ActorID:              "opr_billing_owner",
		ActorRole:            "account_owner",
		Operation:            BillingMutationUpgrade,
		AccountGeneration:    1,
		IdempotencyKeySHA256: strings.Repeat("a", 64),
		RequestSHA256:        strings.Repeat("b", 64),
		Reason:               "Upgrade the account for additional capacity",
		ConfirmedAt:          now,
		TargetPlan:           "standard",
		EmailSHA256:          strings.Repeat("c", 64),
		Status:               BillingMutationPending,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
}

// seedNewerBillingMutationLane models a lane produced by an older deployment
// that allowed a new account operation to overtake an expired pending receipt.
// Current stores deliberately cannot create this state through their public
// claim API; the state must nevertheless remain recoverable so the old receipt
// can be terminally superseded after rollout.
func seedNewerBillingMutationLane(
	t *testing.T,
	store BillingMutationStore,
	previous, newer BillingAccountMutationLease,
) {
	t.Helper()
	if err := validateBillingAccountMutationLease(newer); err != nil {
		t.Fatalf("invalid newer account lane fixture: %v", err)
	}
	if newer.AccountID != previous.AccountID ||
		newer.OperationID == previous.OperationID ||
		newer.OperationGeneration <= previous.OperationGeneration {
		t.Fatalf("newer account lane fixture does not supersede previous: old=%+v new=%+v",
			previous, newer)
	}
	replaceBillingMutationLane(t, store, previous, newer)
}

func replaceBillingMutationLane(
	t *testing.T,
	store BillingMutationStore,
	previous, replacement BillingAccountMutationLease,
) {
	t.Helper()
	switch typed := store.(type) {
	case *MemStore:
		typed.mu.Lock()
		current, ok := typed.billingMutationAccounts[previous.AccountID]
		if !ok || current.OperationID != previous.OperationID ||
			current.OperationGeneration != previous.OperationGeneration {
			typed.mu.Unlock()
			t.Fatalf("unexpected mem account lane before seed: %+v ok=%v", current, ok)
		}
		typed.billingMutationAccounts[previous.AccountID] =
			cloneBillingAccountMutationLease(replacement)
		typed.mu.Unlock()
	case *R2Store:
		ctx := context.Background()
		key := typed.billingMutationAccountKey(previous.AccountID)
		data, etag, err := typed.c.Get(ctx, key)
		if err != nil {
			t.Fatalf("read R2 account lane before seed: %v", err)
		}
		var current BillingAccountMutationLease
		if err := json.Unmarshal(data, &current); err != nil {
			t.Fatalf("decode R2 account lane before seed: %v", err)
		}
		if current.OperationID != previous.OperationID ||
			current.OperationGeneration != previous.OperationGeneration {
			t.Fatalf("unexpected R2 account lane before seed: %+v", current)
		}
		encoded, err := json.Marshal(replacement)
		if err != nil {
			t.Fatalf("encode newer R2 account lane: %v", err)
		}
		if _, err := typed.c.Put(ctx, key, encoded, blob.Cond{IfMatch: etag}); err != nil {
			t.Fatalf("seed newer R2 account lane: %v", err)
		}
	default:
		t.Fatalf("unsupported billing mutation store fixture %T", store)
	}
}

func readBillingMutationLane(
	t *testing.T,
	store BillingMutationStore,
	accountID string,
) BillingAccountMutationLease {
	t.Helper()
	switch typed := store.(type) {
	case *MemStore:
		typed.mu.Lock()
		defer typed.mu.Unlock()
		lease, ok := typed.billingMutationAccounts[accountID]
		if !ok {
			t.Fatalf("missing mem account lane %q", accountID)
		}
		return cloneBillingAccountMutationLease(lease)
	case *R2Store:
		data, _, err := typed.c.Get(
			context.Background(), typed.billingMutationAccountKey(accountID))
		if err != nil {
			t.Fatalf("read R2 account lane %q: %v", accountID, err)
		}
		var lease BillingAccountMutationLease
		if err := json.Unmarshal(data, &lease); err != nil {
			t.Fatalf("decode R2 account lane %q: %v", accountID, err)
		}
		return lease
	default:
		t.Fatalf("unsupported billing mutation store fixture %T", store)
		return BillingAccountMutationLease{}
	}
}

func replaceBillingMutationReceipt(
	t *testing.T,
	store BillingMutationStore,
	previous, replacement BillingMutationReceipt,
) {
	t.Helper()
	switch typed := store.(type) {
	case *MemStore:
		typed.mu.Lock()
		current, ok := typed.billingMutationReceipts[previous.OperationID]
		if !ok || current.OperationID != previous.OperationID {
			typed.mu.Unlock()
			t.Fatalf("unexpected mem receipt before replacement: %+v ok=%v", current, ok)
		}
		typed.billingMutationReceipts[previous.OperationID] =
			cloneBillingMutationReceipt(replacement)
		typed.mu.Unlock()
	case *R2Store:
		ctx := context.Background()
		key := typed.billingMutationReceiptKey(previous.OperationID)
		_, etag, err := typed.c.Get(ctx, key)
		if err != nil {
			t.Fatalf("read R2 receipt before replacement: %v", err)
		}
		encoded, err := json.Marshal(replacement)
		if err != nil {
			t.Fatalf("encode replacement R2 receipt: %v", err)
		}
		if _, err := typed.c.Put(ctx, key, encoded, blob.Cond{IfMatch: etag}); err != nil {
			t.Fatalf("replace R2 receipt: %v", err)
		}
	default:
		t.Fatalf("unsupported billing mutation store fixture %T", store)
	}
}

func testBillingMutationStoreContract(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	receipt := billingMutationTestReceipt("bop_contract_receipt", now)

	if _, ok, err := store.GetBillingMutation(
		ctx, receipt.OperationID); err != nil || ok {
		t.Fatalf("GetBillingMutation missing = ok=%v err=%v", ok, err)
	}
	stored, created, err := store.ReceiveBillingMutation(ctx, receipt)
	if err != nil || !created || stored.Version != 1 ||
		stored.Status != BillingMutationPending {
		t.Fatalf("ReceiveBillingMutation first = %+v created=%v err=%v",
			stored, created, err)
	}
	got, ok, err := store.GetBillingMutation(ctx, receipt.OperationID)
	if err != nil || !ok || !sameBillingMutationIdentity(got, stored) ||
		got.Version != stored.Version {
		t.Fatalf("GetBillingMutation = %+v ok=%v err=%v", got, ok, err)
	}

	// Receipt-construction timestamps belong to the first audit record and do
	// not turn a later exact retry into an idempotency conflict.
	replay := receipt
	replay.ConfirmedAt = now.Add(time.Hour)
	replay.CreatedAt = now.Add(time.Hour)
	replay.UpdatedAt = now.Add(time.Hour)
	again, created, err := store.ReceiveBillingMutation(ctx, replay)
	if err != nil || created || again.Version != stored.Version ||
		!again.ConfirmedAt.Equal(receipt.ConfirmedAt) {
		t.Fatalf("exact replay = %+v created=%v err=%v", again, created, err)
	}
	conflict := replay
	conflict.RequestSHA256 = strings.Repeat("d", 64)
	if _, _, err := store.ReceiveBillingMutation(
		ctx, conflict); !errors.Is(err, ErrBillingMutationConflict) {
		t.Fatalf("changed request hash = %v; want conflict", err)
	}
	conflict = replay
	conflict.Reason = "A different audit reason"
	if _, _, err := store.ReceiveBillingMutation(
		ctx, conflict); !errors.Is(err, ErrBillingMutationConflict) {
		t.Fatalf("changed reason = %v; want conflict", err)
	}
	conflict = replay
	conflict.AccountGeneration++
	if _, _, err := store.ReceiveBillingMutation(
		ctx, conflict); !errors.Is(err, ErrBillingMutationConflict) {
		t.Fatalf("changed account generation = %v; want conflict", err)
	}

	claimAt := now.Add(time.Minute)
	first, acquired, err := store.ClaimBillingMutation(
		ctx, stored, "bmc_first", claimAt, claimAt.Add(2*time.Minute))
	if err != nil || !acquired || first.ClaimGeneration != 1 ||
		first.ClaimToken != "bmc_first" || first.LeaseExpiresAt == nil {
		t.Fatalf("first claim = %+v acquired=%v err=%v", first, acquired, err)
	}
	firstReplay, acquired, err := store.ClaimBillingMutation(
		ctx, stored, "bmc_first", claimAt.Add(time.Second),
		claimAt.Add(2*time.Minute+time.Second))
	if err != nil || !acquired ||
		firstReplay.ClaimGeneration != first.ClaimGeneration {
		t.Fatalf("same-token live replay = %+v acquired=%v err=%v",
			firstReplay, acquired, err)
	}
	busy, acquired, err := store.ClaimBillingMutation(
		ctx, stored, "bmc_busy", claimAt.Add(time.Second),
		claimAt.Add(2*time.Minute+time.Second))
	if err != nil || acquired || busy.ClaimToken != first.ClaimToken {
		t.Fatalf("live contention = %+v acquired=%v err=%v", busy, acquired, err)
	}

	takeoverAt := *first.LeaseExpiresAt
	second, acquired, err := store.ClaimBillingMutation(
		ctx, stored, "bmc_second", takeoverAt,
		takeoverAt.Add(2*time.Minute))
	if err != nil || !acquired ||
		second.ClaimGeneration != first.ClaimGeneration+1 {
		t.Fatalf("expired takeover = %+v acquired=%v err=%v", second, acquired, err)
	}
	result := BillingMutationResult{
		Kind: BillingMutationResultAction,
		Plan: "standard",
		URL:  "https://billing.example.test/session/contract",
	}
	if _, err := store.CompleteBillingMutation(
		ctx, first, result, takeoverAt.Add(time.Second)); !errors.Is(err, ErrBillingMutationClaimLost) {
		t.Fatalf("stale completion = %v; want claim lost", err)
	}
	if err := store.ReleaseBillingMutation(
		ctx, first, takeoverAt.Add(time.Second)); !errors.Is(err, ErrBillingMutationClaimLost) {
		t.Fatalf("stale release = %v; want claim lost", err)
	}

	releasedAt := takeoverAt.Add(time.Second)
	if err := store.ReleaseBillingMutation(ctx, second, releasedAt); err != nil {
		t.Fatalf("release second claim: %v", err)
	}
	if err := store.ReleaseBillingMutation(ctx, second, releasedAt); err != nil {
		t.Fatalf("release acknowledgement replay: %v", err)
	}
	thirdAt := releasedAt.Add(time.Second)
	third, acquired, err := store.ClaimBillingMutation(
		ctx, stored, "bmc_third", thirdAt, thirdAt.Add(2*time.Minute))
	if err != nil || !acquired ||
		third.ClaimGeneration != second.ClaimGeneration+1 {
		t.Fatalf("claim after release = %+v acquired=%v err=%v", third, acquired, err)
	}
	completedAt := thirdAt.Add(time.Second)
	completed, err := store.CompleteBillingMutation(
		ctx, third, result, completedAt)
	if err != nil || completed.Status != BillingMutationCompleted ||
		completed.Result == nil ||
		!sameBillingMutationResult(*completed.Result, result) ||
		completed.CompletedAt == nil || completed.ClaimToken != "" ||
		completed.LeaseExpiresAt != nil {
		t.Fatalf("completion = %+v err=%v", completed, err)
	}
	completionVersion := completed.Version
	completedReplay, err := store.CompleteBillingMutation(
		ctx, third, result, completedAt.Add(time.Minute))
	if err != nil || completedReplay.Version != completionVersion {
		t.Fatalf("completion replay = %+v err=%v", completedReplay, err)
	}
	changedResult := BillingMutationResult{
		Kind: BillingMutationResultDone,
		Plan: "standard",
	}
	if _, err := store.CompleteBillingMutation(
		ctx, third, changedResult, completedAt.Add(time.Minute)); !errors.Is(err, ErrBillingMutationConflict) {
		t.Fatalf("changed terminal result = %v; want conflict", err)
	}
	if err := store.ReleaseBillingMutation(
		ctx, third, completedAt.Add(time.Minute)); err != nil {
		t.Fatalf("release after completion: %v", err)
	}
	terminal, acquired, err := store.ClaimBillingMutation(
		ctx, stored, "bmc_after_complete", completedAt.Add(time.Minute),
		completedAt.Add(3*time.Minute))
	if err != nil || acquired || terminal.Status != BillingMutationCompleted {
		t.Fatalf("claim completed = %+v acquired=%v err=%v", terminal, acquired, err)
	}
	terminalReplay, created, err := store.ReceiveBillingMutation(ctx, replay)
	if err != nil || created || terminalReplay.Status != BillingMutationCompleted ||
		terminalReplay.Result == nil {
		t.Fatalf("terminal Receive replay = %+v created=%v err=%v",
			terminalReplay, created, err)
	}

	// Returned pointers are defensive copies, including the terminal result.
	terminalReplay.Result.URL = "https://mutated.invalid/"
	readAgain, ok, err := store.GetBillingMutation(ctx, receipt.OperationID)
	if err != nil || !ok || readAgain.Result == nil ||
		readAgain.Result.URL != result.URL {
		t.Fatalf("receipt clone isolation = %+v ok=%v err=%v", readAgain, ok, err)
	}
}

func testBillingAccountMutationLaneContract(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 17, 0, 0, 0, time.UTC)
	accountID := "acct_lane_contract"

	first, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, "bop_lane_first", 0, "bcl_lane_first",
		now, now.Add(2*time.Minute))
	if err != nil || !acquired || first.OperationGeneration != 1 ||
		first.ClaimGeneration != 1 || first.Version != 1 ||
		first.SchemaVersion != billingAccountMutationLeaseSchemaVersion {
		t.Fatalf("first account claim = %+v acquired=%v err=%v",
			first, acquired, err)
	}
	replay, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, "bop_lane_first", 0, "bcl_lane_first",
		now.Add(time.Second), now.Add(2*time.Minute+time.Second))
	if err != nil || !acquired ||
		replay.OperationGeneration != first.OperationGeneration ||
		replay.ClaimGeneration != first.ClaimGeneration {
		t.Fatalf("account claim replay = %+v acquired=%v err=%v",
			replay, acquired, err)
	}
	busy, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, "bop_lane_second", 0, "bcl_lane_busy",
		now.Add(time.Second), now.Add(2*time.Minute+time.Second))
	if err != nil || acquired || busy.OperationID != first.OperationID {
		t.Fatalf("account lane contention = %+v acquired=%v err=%v",
			busy, acquired, err)
	}
	resumeBusy, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, first.OperationID, first.OperationGeneration,
		"bcl_resume_busy", now.Add(time.Second),
		now.Add(2*time.Minute+time.Second))
	if err != nil || acquired || resumeBusy.ClaimToken != first.ClaimToken {
		t.Fatalf("exact generation live contention = %+v acquired=%v err=%v",
			resumeBusy, acquired, err)
	}

	releasedAt := now.Add(time.Minute)
	if err := store.ReleaseBillingMutationAccount(ctx, first, releasedAt); err != nil {
		t.Fatalf("release first account claim: %v", err)
	}
	if err := store.ReleaseBillingMutationAccount(ctx, first, releasedAt); err != nil {
		t.Fatalf("replay first account release: %v", err)
	}
	// A crash after the account claim but before receipt creation leaves no
	// expected generation for the retry to read. The deterministic operation
	// id still resumes the same operation generation rather than manufacturing
	// a new one.
	gapAt := releasedAt.Add(time.Second)
	gapResume, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, first.OperationID, 0, "bcl_lane_gap_resume",
		gapAt, gapAt.Add(2*time.Minute))
	if err != nil || !acquired ||
		gapResume.OperationGeneration != first.OperationGeneration ||
		gapResume.ClaimGeneration != first.ClaimGeneration+1 {
		t.Fatalf("receipt-gap account resume = %+v acquired=%v err=%v",
			gapResume, acquired, err)
	}
	gapReleasedAt := gapAt.Add(time.Second)
	if err := store.ReleaseBillingMutationAccount(
		ctx, gapResume, gapReleasedAt); err != nil {
		t.Fatalf("release receipt-gap account claim: %v", err)
	}
	resumedAt := gapReleasedAt.Add(time.Second)
	resumed, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, first.OperationID, first.OperationGeneration,
		"bcl_lane_resume", resumedAt, resumedAt.Add(2*time.Minute))
	if err != nil || !acquired ||
		resumed.OperationGeneration != first.OperationGeneration ||
		resumed.ClaimGeneration != gapResume.ClaimGeneration+1 {
		t.Fatalf("resume exact account generation = %+v acquired=%v err=%v",
			resumed, acquired, err)
	}
	secondRelease := resumedAt.Add(time.Second)
	if err := store.ReleaseBillingMutationAccount(
		ctx, resumed, secondRelease); err != nil {
		t.Fatalf("release resumed account claim: %v", err)
	}
	priorReceipt := billingMutationTestReceipt(first.OperationID, now)
	priorReceipt.AccountID = accountID
	priorReceipt.AccountGeneration = first.OperationGeneration
	priorStored, _, err := store.ReceiveBillingMutation(ctx, priorReceipt)
	if err != nil {
		t.Fatal(err)
	}
	pendingAt := secondRelease.Add(time.Second)
	pendingBlocked, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, "bop_lane_second", 0, "bcl_lane_pending_blocked",
		pendingAt, pendingAt.Add(2*time.Minute))
	if err != nil || acquired || pendingBlocked.OperationID != first.OperationID ||
		pendingBlocked.OperationGeneration != first.OperationGeneration {
		t.Fatalf("new operation advanced past pending prior receipt = %+v acquired=%v err=%v",
			pendingBlocked, acquired, err)
	}
	priorClaimAt := pendingAt.Add(time.Second)
	priorClaimed, acquired, err := store.ClaimBillingMutation(
		ctx, priorStored, "bcl_lane_prior_receipt", priorClaimAt,
		priorClaimAt.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("claim prior receipt = %+v acquired=%v err=%v",
			priorClaimed, acquired, err)
	}
	priorResult := BillingMutationResult{
		Kind: BillingMutationResultAction, Plan: "standard",
		URL: "https://billing.example.test/session/lane-prior",
	}
	priorCompletedAt := priorClaimAt.Add(time.Second)
	if _, err := store.CompleteBillingMutation(
		ctx, priorClaimed, priorResult, priorCompletedAt); err != nil {
		t.Fatalf("complete prior receipt: %v", err)
	}
	if _, _, err := store.ClaimBillingMutationAccount(
		ctx, accountID, "bop_lane_second", 0, "bcl_lane_backdated",
		priorCompletedAt.Add(-time.Nanosecond),
		priorCompletedAt.Add(time.Minute),
	); err == nil {
		t.Fatal("new operation claim predating prior terminal receipt was accepted")
	}
	secondAt := priorCompletedAt.Add(time.Second)
	second, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, "bop_lane_second", 0, "bcl_lane_second",
		secondAt, secondAt.Add(2*time.Minute))
	if err != nil || !acquired ||
		second.OperationGeneration != first.OperationGeneration+1 {
		t.Fatalf("new account generation = %+v acquired=%v err=%v",
			second, acquired, err)
	}
	if _, _, err := store.ClaimBillingMutationAccount(
		ctx, accountID, first.OperationID, first.OperationGeneration,
		"bcl_stale_resume", secondAt.Add(time.Second),
		secondAt.Add(2*time.Minute+time.Second),
	); !errors.Is(err, ErrBillingMutationSuperseded) {
		t.Fatalf("stale account resume = %v; want superseded", err)
	}
	if err := store.ReleaseBillingMutationAccount(
		ctx, resumed, secondAt.Add(time.Second),
	); !errors.Is(err, ErrBillingMutationSuperseded) {
		t.Fatalf("stale account release = %v; want superseded", err)
	}

	// Expiry, like explicit release, must preserve an operation's generation.
	expiryAccount := accountID + "_expiry"
	expirySeed, acquired, err := store.ClaimBillingMutationAccount(
		ctx, expiryAccount, "bop_lane_expiry", 0, "bcl_lane_expiry_seed",
		now, now.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("seed expiry lane = %+v acquired=%v err=%v",
			expirySeed, acquired, err)
	}
	expiryResume, acquired, err := store.ClaimBillingMutationAccount(
		ctx, expiryAccount, expirySeed.OperationID, 0,
		"bcl_lane_expiry_resume", now.Add(time.Minute), now.Add(3*time.Minute))
	if err != nil || !acquired ||
		expiryResume.OperationGeneration != expirySeed.OperationGeneration ||
		expiryResume.ClaimGeneration != expirySeed.ClaimGeneration+1 {
		t.Fatalf("same operation expiry resume = %+v acquired=%v err=%v",
			expiryResume, acquired, err)
	}
}

func testBillingMutationReceiptlessLaneExpiry(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 17, 20, 0, 0, time.UTC)
	accountID := "acct_receiptless_lane_expiry"
	expires := now.Add(time.Minute)
	prior, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, "bop_receiptless_prior", 0,
		"bcl_receiptless_prior", now, expires)
	if err != nil || !acquired {
		t.Fatalf("seed receipt-less lane = %+v acquired=%v err=%v",
			prior, acquired, err)
	}
	blocked, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, "bop_receiptless_next", 0,
		"bcl_receiptless_early", expires.Add(-time.Nanosecond),
		expires.Add(time.Minute))
	if err != nil || acquired || blocked.OperationID != prior.OperationID {
		t.Fatalf("live receipt-less lane = %+v acquired=%v err=%v",
			blocked, acquired, err)
	}

	takeover, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, "bop_receiptless_next", 0,
		"bcl_receiptless_next", expires, expires.Add(2*time.Minute))
	if err != nil || !acquired ||
		takeover.OperationGeneration != prior.OperationGeneration+1 {
		t.Fatalf("expired receipt-less takeover = %+v acquired=%v err=%v",
			takeover, acquired, err)
	}

	lateReceipt := billingMutationTestReceipt(prior.OperationID, now)
	lateReceipt.AccountID = accountID
	lateReceipt.AccountGeneration = prior.OperationGeneration
	lateStored, created, err := store.ReceiveBillingMutation(ctx, lateReceipt)
	if err != nil || !created {
		t.Fatalf("late prior receipt = %+v created=%v err=%v",
			lateStored, created, err)
	}
	if _, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, prior.OperationID, prior.OperationGeneration,
		prior.ClaimToken, expires.Add(time.Second), expires.Add(3*time.Minute),
	); !errors.Is(err, ErrBillingMutationSuperseded) || acquired {
		t.Fatalf("late prior handshake acquired=%v err=%v; want superseded",
			acquired, err)
	}
	superseded, err := store.SupersedeBillingMutation(
		ctx, lateStored, expires.Add(time.Second))
	if err != nil || superseded.Status != BillingMutationSuperseded ||
		superseded.SupersededByOperationID != takeover.OperationID {
		t.Fatalf("late prior supersede = %+v err=%v", superseded, err)
	}
}

func testConcurrentBillingMutationReceiptlessLaneRace(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 8, 11, 17, 25, 0, 0, time.UTC)
	for i := range 24 {
		now := base.Add(time.Duration(i) * time.Hour)
		expires := now.Add(time.Minute)
		accountID := fmt.Sprintf("acct_receiptless_race_%02d", i)
		priorOperationID := fmt.Sprintf("bop_receiptless_race_prior_%02d", i)
		nextOperationID := fmt.Sprintf("bop_receiptless_race_next_%02d", i)
		priorToken := fmt.Sprintf("bcl_receiptless_race_prior_%02d", i)
		prior, acquired, err := store.ClaimBillingMutationAccount(
			ctx, accountID, priorOperationID, 0, priorToken, now, expires)
		if err != nil || !acquired {
			t.Fatalf("race %d seed = %+v acquired=%v err=%v",
				i, prior, acquired, err)
		}
		receipt := billingMutationTestReceipt(priorOperationID, now)
		receipt.AccountID = accountID
		receipt.AccountGeneration = prior.OperationGeneration

		start := make(chan struct{})
		priorResult := make(chan struct {
			lease    BillingAccountMutationLease
			acquired bool
			err      error
		}, 1)
		takeoverResult := make(chan struct {
			lease    BillingAccountMutationLease
			acquired bool
			err      error
		}, 1)
		go func() {
			<-start
			if _, _, err := store.ReceiveBillingMutation(ctx, receipt); err != nil {
				priorResult <- struct {
					lease    BillingAccountMutationLease
					acquired bool
					err      error
				}{err: err}
				return
			}
			lease, acquired, err := store.ClaimBillingMutationAccount(
				ctx, accountID, priorOperationID, prior.OperationGeneration,
				priorToken, expires.Add(time.Second), expires.Add(3*time.Minute))
			priorResult <- struct {
				lease    BillingAccountMutationLease
				acquired bool
				err      error
			}{lease: lease, acquired: acquired, err: err}
		}()
		go func() {
			<-start
			lease, acquired, err := store.ClaimBillingMutationAccount(
				ctx, accountID, nextOperationID, 0,
				fmt.Sprintf("bcl_receiptless_race_next_%02d", i),
				expires, expires.Add(2*time.Minute))
			takeoverResult <- struct {
				lease    BillingAccountMutationLease
				acquired bool
				err      error
			}{lease: lease, acquired: acquired, err: err}
		}()
		close(start)
		old := <-priorResult
		takeover := <-takeoverResult
		if takeover.err != nil ||
			(old.err != nil && !errors.Is(old.err, ErrBillingMutationSuperseded)) {
			t.Fatalf("race %d results: old=%+v takeover=%+v", i, old, takeover)
		}

		if takeover.acquired {
			if takeover.lease.OperationGeneration != prior.OperationGeneration+1 ||
				!errors.Is(old.err, ErrBillingMutationSuperseded) || old.acquired {
				t.Fatalf("race %d takeover won: next=%+v old=%+v acquired=%v err=%v",
					i, takeover.lease, old.lease, old.acquired, old.err)
			}
			continue
		}
		if takeover.lease.OperationID != priorOperationID || old.err != nil ||
			!old.acquired || old.lease.OperationGeneration != prior.OperationGeneration {
			t.Fatalf("race %d receipt won: next=%+v old=%+v acquired=%v err=%v",
				i, takeover.lease, old.lease, old.acquired, old.err)
		}
	}
}

func testBillingMutationAccountBindingContract(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 17, 30, 0, 0, time.UTC)
	accountID := "acct_lane_account_binding"
	expires := now.Add(time.Minute)
	lane, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, "bop_lane_account_binding", 0,
		"bcl_lane_account_binding", now, expires)
	if err != nil || !acquired {
		t.Fatalf("seed account binding lane = %+v acquired=%v err=%v",
			lane, acquired, err)
	}
	mismatched := cloneBillingAccountMutationLease(lane)
	mismatched.AccountID = "acct_lane_account_binding_corrupt"
	replaceBillingMutationLane(t, store, lane, mismatched)
	if _, _, err := store.ClaimBillingMutationAccount(
		ctx, accountID, lane.OperationID, lane.OperationGeneration,
		"bcl_lane_account_binding_next", expires, expires.Add(time.Minute),
	); !errors.Is(err, ErrBillingMutationConflict) {
		t.Fatalf("claim against mismatched account lane = %v; want conflict", err)
	}
	if err := store.ReleaseBillingMutationAccount(
		ctx, lane, expires); !errors.Is(err, ErrBillingMutationConflict) {
		t.Fatalf("release against mismatched account lane = %v; want conflict", err)
	}
}

func testBillingMutationSupersedeAuthorizationFailures(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 17, 45, 0, 0, time.UTC)

	missingLaneReceipt := billingMutationTestReceipt(
		"bop_supersede_missing_lane", now)
	missingLaneReceipt.AccountID = "acct_supersede_missing_lane"
	missingStored, _, err := store.ReceiveBillingMutation(ctx, missingLaneReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SupersedeBillingMutation(
		ctx, missingStored, now.Add(time.Second),
	); !errors.Is(err, ErrBillingMutationConflict) {
		t.Fatalf("missing-lane supersede = %v; want conflict", err)
	}

	accountID := "acct_supersede_account_binding"
	laneExpiry := now.Add(time.Minute)
	lane, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, "bop_supersede_account_binding", 0,
		"bcl_supersede_account_binding", now, laneExpiry)
	if err != nil || !acquired {
		t.Fatalf("seed supersede binding lane = %+v acquired=%v err=%v",
			lane, acquired, err)
	}
	receipt := billingMutationTestReceipt(lane.OperationID, now)
	receipt.AccountID = accountID
	receipt.AccountGeneration = lane.OperationGeneration
	stored, _, err := store.ReceiveBillingMutation(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	newerAt := laneExpiry
	newerExpiry := newerAt.Add(2 * time.Minute)
	mismatched := BillingAccountMutationLease{
		SchemaVersion:       billingAccountMutationLeaseSchemaVersion,
		AccountID:           "acct_supersede_account_binding_corrupt",
		OperationID:         "bop_supersede_account_binding_newer",
		OperationGeneration: lane.OperationGeneration + 1,
		ClaimToken:          "bcl_supersede_account_binding_newer",
		ClaimGeneration:     lane.ClaimGeneration + 1,
		LeaseExpiresAt:      &newerExpiry,
		UpdatedAt:           newerAt,
		Version:             lane.Version + 1,
	}
	replaceBillingMutationLane(t, store, lane, mismatched)
	if _, err := store.SupersedeBillingMutation(
		ctx, stored, newerAt); !errors.Is(err, ErrBillingMutationConflict) {
		t.Fatalf("mismatched-lane supersede = %v; want conflict", err)
	}
}

func testBillingMutationSupersessionContract(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC)
	accountID := "acct_supersession_contract"
	lane, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, "bop_superseded_old", 0, "bcl_account_old",
		now, now.Add(2*time.Minute))
	if err != nil || !acquired {
		t.Fatalf("claim old account lane = %+v acquired=%v err=%v", lane, acquired, err)
	}
	receipt := billingMutationTestReceipt(lane.OperationID, now)
	receipt.AccountID = accountID
	receipt.AccountGeneration = lane.OperationGeneration
	stored, _, err := store.ReceiveBillingMutation(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	claimAt := now.Add(time.Minute)
	claimed, acquired, err := store.ClaimBillingMutation(
		ctx, stored, "bcl_receipt_old", claimAt, now.Add(3*time.Minute))
	if err != nil || !acquired {
		t.Fatalf("claim old receipt = %+v acquired=%v err=%v", claimed, acquired, err)
	}
	if _, err := store.SupersedeBillingMutation(
		ctx, claimed, now.Add(90*time.Second)); !errors.Is(err, ErrBillingMutationConflict) {
		t.Fatalf("unauthorized receipt supersede = %v; want conflict", err)
	}

	takeoverAt := now.Add(2 * time.Minute)
	newerExpiry := takeoverAt.Add(3 * time.Minute)
	newer := BillingAccountMutationLease{
		SchemaVersion:       billingAccountMutationLeaseSchemaVersion,
		AccountID:           accountID,
		OperationID:         "bop_superseded_new",
		OperationGeneration: lane.OperationGeneration + 1,
		ClaimToken:          "bcl_account_new",
		ClaimGeneration:     lane.ClaimGeneration + 1,
		LeaseExpiresAt:      &newerExpiry,
		UpdatedAt:           takeoverAt,
		Version:             lane.Version + 1,
	}
	seedNewerBillingMutationLane(t, store, lane, newer)
	if _, err := store.SupersedeBillingMutation(
		ctx, claimed, takeoverAt.Add(-time.Nanosecond)); err == nil {
		t.Fatal("supersede predating its authorizing account lane was accepted")
	}
	if _, err := store.SupersedeBillingMutation(
		ctx, claimed, takeoverAt); !errors.Is(err, ErrBillingMutationClaimActive) {
		t.Fatalf("authorized live receipt supersede = %v; want active claim", err)
	}

	supersededAt := now.Add(3 * time.Minute)
	superseded, err := store.SupersedeBillingMutation(ctx, claimed, supersededAt)
	if err != nil || superseded.Status != BillingMutationSuperseded ||
		superseded.Result != nil || superseded.CompletedAt == nil ||
		superseded.ClaimToken != "" || superseded.LeaseExpiresAt != nil ||
		superseded.SupersededByOperationID != newer.OperationID {
		t.Fatalf("superseded receipt = %+v err=%v", superseded, err)
	}
	supersededVersion := superseded.Version
	replay, err := store.SupersedeBillingMutation(
		ctx, claimed, supersededAt.Add(time.Minute))
	if err != nil || replay.Version != supersededVersion {
		t.Fatalf("supersede replay = %+v err=%v", replay, err)
	}
	result := BillingMutationResult{
		Kind: BillingMutationResultAction, Plan: "standard",
		URL: "https://billing.example.test/session/stale",
	}
	if _, err := store.CompleteBillingMutation(
		ctx, claimed, result, supersededAt.Add(time.Second)); !errors.Is(err, ErrBillingMutationSuperseded) {
		t.Fatalf("stale completion after supersede = %v; want superseded", err)
	}
	if _, _, err := store.ClaimBillingMutation(
		ctx, stored, "bcl_stale_receipt", supersededAt.Add(time.Second),
		supersededAt.Add(2*time.Minute)); !errors.Is(err, ErrBillingMutationSuperseded) {
		t.Fatalf("stale receipt claim = %v; want superseded", err)
	}
	if err := store.ReleaseBillingMutation(
		ctx, claimed, supersededAt.Add(time.Second)); err != nil {
		t.Fatalf("release superseded receipt: %v", err)
	}
	terminal, created, err := store.ReceiveBillingMutation(ctx, receipt)
	if err != nil || created || terminal.Status != BillingMutationSuperseded {
		t.Fatalf("superseded receipt replay = %+v created=%v err=%v",
			terminal, created, err)
	}
	if err := store.ReleaseBillingMutationAccount(
		ctx, lane, supersededAt.Add(time.Second)); !errors.Is(err, ErrBillingMutationSuperseded) {
		t.Fatalf("old lane release = %v; want superseded", err)
	}
}

func testConcurrentBillingMutationAccountResume(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 20, 0, 0, 0, time.UTC)
	seed, acquired, err := store.ClaimBillingMutationAccount(
		ctx, "acct_concurrent_resume", "bop_concurrent_resume", 0,
		"bcl_resume_seed", now, now.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("seed resume lane = %+v acquired=%v err=%v", seed, acquired, err)
	}
	if err := store.ReleaseBillingMutationAccount(
		ctx, seed, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	const workers = 24
	start := make(chan struct{})
	results := make(chan struct {
		lease    BillingAccountMutationLease
		acquired bool
		err      error
	}, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lease, acquired, err := store.ClaimBillingMutationAccount(
				ctx, seed.AccountID, seed.OperationID, seed.OperationGeneration,
				fmt.Sprintf("bcl_resume_racer_%02d", i),
				now.Add(2*time.Second), now.Add(2*time.Minute))
			results <- struct {
				lease    BillingAccountMutationLease
				acquired bool
				err      error
			}{lease: lease, acquired: acquired, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for result := range results {
		if result.err != nil {
			t.Errorf("concurrent exact-generation resume: %v", result.err)
		}
		if result.acquired {
			winners++
			if result.lease.OperationGeneration != seed.OperationGeneration {
				t.Errorf("resume changed operation generation: %+v", result.lease)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("exact-generation resume winners = %d; want 1", winners)
	}
}

func testConcurrentBillingMutationAccountReleaseTakeover(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 20, 30, 0, 0, time.UTC)
	expiry := now.Add(time.Minute)
	seed, acquired, err := store.ClaimBillingMutationAccount(
		ctx, "acct_release_takeover", "bop_release_takeover", 0,
		"bcl_release_takeover_seed", now, expiry)
	if err != nil || !acquired {
		t.Fatalf("seed release/takeover lane = %+v acquired=%v err=%v",
			seed, acquired, err)
	}

	start := make(chan struct{})
	releaseResult := make(chan error, 1)
	takeoverResult := make(chan struct {
		lease    BillingAccountMutationLease
		acquired bool
		err      error
	}, 1)
	go func() {
		<-start
		releaseResult <- store.ReleaseBillingMutationAccount(ctx, seed, expiry)
	}()
	go func() {
		<-start
		lease, acquired, err := store.ClaimBillingMutationAccount(
			ctx, seed.AccountID, seed.OperationID, seed.OperationGeneration,
			"bcl_release_takeover_next", expiry, expiry.Add(2*time.Minute))
		takeoverResult <- struct {
			lease    BillingAccountMutationLease
			acquired bool
			err      error
		}{lease: lease, acquired: acquired, err: err}
	}()
	close(start)
	releaseErr := <-releaseResult
	takeover := <-takeoverResult
	if takeover.err != nil || !takeover.acquired ||
		takeover.lease.OperationGeneration != seed.OperationGeneration ||
		takeover.lease.ClaimGeneration != seed.ClaimGeneration+1 {
		t.Fatalf("release/takeover successor = %+v acquired=%v err=%v",
			takeover.lease, takeover.acquired, takeover.err)
	}
	if releaseErr != nil && !errors.Is(releaseErr, ErrBillingMutationClaimLost) {
		t.Fatalf("release/takeover release = %v; want nil or claim lost", releaseErr)
	}
	if err := store.ReleaseBillingMutationAccount(
		ctx, seed, expiry.Add(time.Second)); !errors.Is(err, ErrBillingMutationClaimLost) {
		t.Fatalf("stale release after takeover = %v; want claim lost", err)
	}
}

func testConcurrentBillingMutationClaimSupersede(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 20, 45, 0, 0, time.UTC)
	oldExpiry := now.Add(time.Minute)
	lane, acquired, err := store.ClaimBillingMutationAccount(
		ctx, "acct_claim_supersede", "bop_claim_supersede_old", 0,
		"bcl_claim_supersede_lane", now, oldExpiry)
	if err != nil || !acquired {
		t.Fatalf("claim/supersede old lane = %+v acquired=%v err=%v",
			lane, acquired, err)
	}
	receipt := billingMutationTestReceipt(lane.OperationID, now)
	receipt.AccountID = lane.AccountID
	receipt.AccountGeneration = lane.OperationGeneration
	stored, _, err := store.ReceiveBillingMutation(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	newerAt := oldExpiry
	newerExpiry := newerAt.Add(3 * time.Minute)
	newer := BillingAccountMutationLease{
		SchemaVersion:       billingAccountMutationLeaseSchemaVersion,
		AccountID:           lane.AccountID,
		OperationID:         "bop_claim_supersede_newer",
		OperationGeneration: lane.OperationGeneration + 1,
		ClaimToken:          "bcl_claim_supersede_newer",
		ClaimGeneration:     lane.ClaimGeneration + 1,
		LeaseExpiresAt:      &newerExpiry,
		UpdatedAt:           newerAt,
		Version:             lane.Version + 1,
	}
	seedNewerBillingMutationLane(t, store, lane, newer)

	raceAt := newerAt.Add(time.Second)
	claimExpiry := raceAt.Add(time.Minute)
	start := make(chan struct{})
	claimResult := make(chan struct {
		receipt  BillingMutationReceipt
		acquired bool
		err      error
	}, 1)
	supersedeResult := make(chan error, 1)
	go func() {
		<-start
		claimed, acquired, err := store.ClaimBillingMutation(
			ctx, stored, "bcl_claim_supersede_receipt", raceAt, claimExpiry)
		claimResult <- struct {
			receipt  BillingMutationReceipt
			acquired bool
			err      error
		}{receipt: claimed, acquired: acquired, err: err}
	}()
	go func() {
		<-start
		_, err := store.SupersedeBillingMutation(ctx, stored, raceAt)
		supersedeResult <- err
	}()
	close(start)
	claim := <-claimResult
	supersedeErr := <-supersedeResult
	current, ok, err := store.GetBillingMutation(ctx, stored.OperationID)
	if err != nil || !ok {
		t.Fatalf("read claim/supersede race = %+v ok=%v err=%v", current, ok, err)
	}
	switch current.Status {
	case BillingMutationPending:
		if claim.err != nil || !claim.acquired ||
			!errors.Is(supersedeErr, ErrBillingMutationClaimActive) ||
			current.ClaimToken != claim.receipt.ClaimToken {
			t.Fatalf("claim won: claim=%+v acquired=%v err=%v supersede=%v current=%+v",
				claim.receipt, claim.acquired, claim.err, supersedeErr, current)
		}
		if _, err := store.SupersedeBillingMutation(
			ctx, current, claimExpiry); err != nil {
			t.Fatalf("supersede after winning claim expired: %v", err)
		}
	case BillingMutationSuperseded:
		if supersedeErr != nil || claim.acquired ||
			!errors.Is(claim.err, ErrBillingMutationSuperseded) {
			t.Fatalf("supersede won: claim=%+v acquired=%v err=%v supersede=%v current=%+v",
				claim.receipt, claim.acquired, claim.err, supersedeErr, current)
		}
	default:
		t.Fatalf("claim/supersede race produced invalid state: %+v", current)
	}
}

func testConcurrentBillingMutationTerminalTransition(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 21, 0, 0, 0, time.UTC)
	lane, acquired, err := store.ClaimBillingMutationAccount(
		ctx, "acct_billing_mutation", "bop_terminal_race", 0,
		"bcl_terminal_lane", now, now.Add(90*time.Second))
	if err != nil || !acquired {
		t.Fatalf("terminal race lane = %+v acquired=%v err=%v", lane, acquired, err)
	}
	receipt := billingMutationTestReceipt(lane.OperationID, now)
	receipt.AccountID = lane.AccountID
	receipt.AccountGeneration = lane.OperationGeneration
	stored, _, err := store.ReceiveBillingMutation(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	terminalAt := now.Add(2 * time.Minute)
	claimed, acquired, err := store.ClaimBillingMutation(
		ctx, stored, "bcl_terminal_race", now.Add(time.Minute), terminalAt)
	if err != nil || !acquired {
		t.Fatalf("terminal race claim = %+v acquired=%v err=%v", claimed, acquired, err)
	}
	newerAt := now.Add(90 * time.Second)
	newerExpiry := newerAt.Add(3 * time.Minute)
	newer := BillingAccountMutationLease{
		SchemaVersion:       billingAccountMutationLeaseSchemaVersion,
		AccountID:           lane.AccountID,
		OperationID:         "bop_terminal_race_newer",
		OperationGeneration: lane.OperationGeneration + 1,
		ClaimToken:          "bcl_terminal_lane_newer",
		ClaimGeneration:     lane.ClaimGeneration + 1,
		LeaseExpiresAt:      &newerExpiry,
		UpdatedAt:           newerAt,
		Version:             lane.Version + 1,
	}
	seedNewerBillingMutationLane(t, store, lane, newer)
	result := BillingMutationResult{
		Kind: BillingMutationResultAction, Plan: "standard",
		URL: "https://billing.example.test/session/terminal-race",
	}
	start := make(chan struct{})
	completeResult := make(chan error, 1)
	supersedeResult := make(chan error, 1)
	go func() {
		<-start
		_, err := store.CompleteBillingMutation(ctx, claimed, result, terminalAt)
		completeResult <- err
	}()
	go func() {
		<-start
		_, err := store.SupersedeBillingMutation(ctx, claimed, terminalAt)
		supersedeResult <- err
	}()
	close(start)
	completeErr := <-completeResult
	supersedeErr := <-supersedeResult
	terminal, ok, err := store.GetBillingMutation(ctx, receipt.OperationID)
	if err != nil || !ok {
		t.Fatalf("read terminal race = %+v ok=%v err=%v", terminal, ok, err)
	}
	switch terminal.Status {
	case BillingMutationCompleted:
		if completeErr != nil ||
			!errors.Is(supersedeErr, ErrBillingMutationConflict) {
			t.Fatalf("completion won: complete=%v supersede=%v terminal=%+v",
				completeErr, supersedeErr, terminal)
		}
	case BillingMutationSuperseded:
		if supersedeErr != nil ||
			!errors.Is(completeErr, ErrBillingMutationSuperseded) {
			t.Fatalf("supersede won: complete=%v supersede=%v terminal=%+v",
				completeErr, supersedeErr, terminal)
		}
	default:
		t.Fatalf("terminal race left non-terminal receipt: %+v", terminal)
	}
}

func testConcurrentBillingMutationAccountClaims(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 19, 0, 0, 0, time.UTC)
	const workers = 24
	start := make(chan struct{})
	results := make(chan struct {
		acquired bool
		err      error
	}, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, acquired, err := store.ClaimBillingMutationAccount(
				ctx, "acct_concurrent_lane",
				fmt.Sprintf("bop_lane_racer_%02d", i), 0,
				fmt.Sprintf("bcl_lane_racer_%02d", i),
				now, now.Add(2*time.Minute))
			results <- struct {
				acquired bool
				err      error
			}{acquired: acquired, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for result := range results {
		if result.err != nil {
			t.Errorf("concurrent account claim: %v", result.err)
		}
		if result.acquired {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("account lane winners = %d; want 1", winners)
	}
}

func testConcurrentBillingMutationAdvanceAfterTerminal(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 19, 30, 0, 0, time.UTC)
	lane, acquired, err := store.ClaimBillingMutationAccount(
		ctx, "acct_concurrent_terminal_advance", "bop_terminal_advance_prior", 0,
		"bcl_terminal_advance_prior", now, now.Add(2*time.Minute))
	if err != nil || !acquired {
		t.Fatalf("seed terminal advance lane = %+v acquired=%v err=%v",
			lane, acquired, err)
	}
	receipt := billingMutationTestReceipt(lane.OperationID, now)
	receipt.AccountID = lane.AccountID
	receipt.AccountGeneration = lane.OperationGeneration
	stored, _, err := store.ReceiveBillingMutation(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	claimAt := now.Add(time.Second)
	claimed, acquired, err := store.ClaimBillingMutation(
		ctx, stored, "bcl_terminal_advance_receipt", claimAt,
		claimAt.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("claim terminal advance receipt = %+v acquired=%v err=%v",
			claimed, acquired, err)
	}
	completedAt := claimAt.Add(time.Second)
	if _, err := store.CompleteBillingMutation(ctx, claimed, BillingMutationResult{
		Kind: BillingMutationResultAction, Plan: "standard",
		URL: "https://billing.example.test/session/terminal-advance",
	}, completedAt); err != nil {
		t.Fatalf("complete terminal advance receipt: %v", err)
	}
	if err := store.ReleaseBillingMutationAccount(
		ctx, lane, completedAt); err != nil {
		t.Fatalf("release terminal advance lane: %v", err)
	}

	const workers = 24
	start := make(chan struct{})
	results := make(chan struct {
		lease    BillingAccountMutationLease
		acquired bool
		err      error
	}, workers)
	for i := range workers {
		go func() {
			<-start
			lease, acquired, err := store.ClaimBillingMutationAccount(
				ctx, lane.AccountID, fmt.Sprintf("bop_terminal_advance_%02d", i), 0,
				fmt.Sprintf("bcl_terminal_advance_%02d", i),
				completedAt.Add(time.Second), completedAt.Add(2*time.Minute))
			results <- struct {
				lease    BillingAccountMutationLease
				acquired bool
				err      error
			}{lease: lease, acquired: acquired, err: err}
		}()
	}
	close(start)
	winners := 0
	for range workers {
		result := <-results
		if result.err != nil {
			t.Errorf("concurrent terminal advance: %v", result.err)
		}
		if result.acquired {
			winners++
			if result.lease.OperationGeneration != lane.OperationGeneration+1 {
				t.Errorf("terminal advance generation = %+v", result.lease)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("terminal account advance winners = %d; want 1", winners)
	}
}

func testConcurrentBillingMutationReceive(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 14, 0, 0, 0, time.UTC)
	receipt := billingMutationTestReceipt("bop_concurrent_receive", now)
	const workers = 24
	start := make(chan struct{})
	results := make(chan struct {
		created bool
		err     error
	}, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, created, err := store.ReceiveBillingMutation(ctx, receipt)
			results <- struct {
				created bool
				err     error
			}{created: created, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	creates := 0
	for result := range results {
		if result.err != nil {
			t.Errorf("concurrent ReceiveBillingMutation: %v", result.err)
		}
		if result.created {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("create winners = %d; want 1", creates)
	}
}

func testConcurrentBillingMutationClaim(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 15, 0, 0, 0, time.UTC)
	receipt := billingMutationTestReceipt("bop_concurrent_claim", now)
	stored, _, err := store.ReceiveBillingMutation(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 24
	start := make(chan struct{})
	results := make(chan struct {
		acquired bool
		err      error
	}, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, acquired, err := store.ClaimBillingMutation(
				ctx, stored, fmt.Sprintf("bmc_racer_%02d", i),
				now.Add(time.Second), now.Add(2*time.Minute))
			results <- struct {
				acquired bool
				err      error
			}{acquired: acquired, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for result := range results {
		if result.err != nil {
			t.Errorf("concurrent ClaimBillingMutation: %v", result.err)
		}
		if result.acquired {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners = %d; want 1", winners)
	}
}

func testBillingMutationCounterOverflowContract(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 8, 11, 22, 0, 0, 0, time.UTC)

	seedIdleLane := func(accountID, operationID string, now time.Time) BillingAccountMutationLease {
		lane, acquired, err := store.ClaimBillingMutationAccount(
			ctx, accountID, operationID, 0, "bcl_overflow_seed",
			now, now.Add(time.Minute))
		if err != nil || !acquired {
			t.Fatalf("seed overflow lane %q = %+v acquired=%v err=%v",
				accountID, lane, acquired, err)
		}
		if err := store.ReleaseBillingMutationAccount(
			ctx, lane, now.Add(time.Second)); err != nil {
			t.Fatalf("release overflow lane %q: %v", accountID, err)
		}
		return readBillingMutationLane(t, store, accountID)
	}

	claimGenerationLane := seedIdleLane(
		"acct_overflow_lane_claim", "bop_overflow_lane_claim", base)
	claimGenerationMax := claimGenerationLane
	claimGenerationMax.ClaimGeneration = math.MaxInt64
	replaceBillingMutationLane(
		t, store, claimGenerationLane, claimGenerationMax)
	if _, acquired, err := store.ClaimBillingMutationAccount(
		ctx, claimGenerationMax.AccountID, claimGenerationMax.OperationID,
		claimGenerationMax.OperationGeneration, "bcl_overflow_claim_next",
		base.Add(2*time.Second), base.Add(time.Minute),
	); err == nil || acquired {
		t.Fatalf("max account claim generation advanced: acquired=%v err=%v",
			acquired, err)
	}
	claimGenerationAfter := readBillingMutationLane(
		t, store, claimGenerationMax.AccountID)
	if claimGenerationAfter.ClaimGeneration != math.MaxInt64 ||
		claimGenerationAfter.Version != claimGenerationMax.Version {
		t.Fatalf("account claim overflow changed lane: %+v", claimGenerationAfter)
	}

	versionBase := base.Add(10 * time.Minute)
	versionLane := seedIdleLane(
		"acct_overflow_lane_version", "bop_overflow_lane_version", versionBase)
	versionMax := versionLane
	versionMax.Version = math.MaxInt64
	replaceBillingMutationLane(t, store, versionLane, versionMax)
	if _, acquired, err := store.ClaimBillingMutationAccount(
		ctx, versionMax.AccountID, versionMax.OperationID,
		versionMax.OperationGeneration, "bcl_overflow_version_next",
		versionBase.Add(2*time.Second), versionBase.Add(time.Minute),
	); err == nil || acquired {
		t.Fatalf("max account lane version advanced: acquired=%v err=%v",
			acquired, err)
	}
	versionAfter := readBillingMutationLane(t, store, versionMax.AccountID)
	if versionAfter.Version != math.MaxInt64 ||
		versionAfter.ClaimGeneration != versionMax.ClaimGeneration {
		t.Fatalf("account version overflow changed lane: %+v", versionAfter)
	}

	operationBase := base.Add(20 * time.Minute)
	operationLane := seedIdleLane(
		"acct_overflow_lane_operation", "bop_overflow_lane_operation_seed",
		operationBase)
	operationMax := operationLane
	operationMax.OperationID = "bop_overflow_lane_operation_prior"
	operationMax.OperationGeneration = math.MaxInt64
	replaceBillingMutationLane(t, store, operationLane, operationMax)
	priorReceipt := billingMutationTestReceipt(operationMax.OperationID, operationMax.UpdatedAt)
	priorReceipt.AccountID = operationMax.AccountID
	priorReceipt.AccountGeneration = operationMax.OperationGeneration
	priorStored, _, err := store.ReceiveBillingMutation(ctx, priorReceipt)
	if err != nil {
		t.Fatal(err)
	}
	priorClaimAt := operationMax.UpdatedAt.Add(time.Second)
	priorClaimed, acquired, err := store.ClaimBillingMutation(
		ctx, priorStored, "bcl_overflow_operation_receipt", priorClaimAt,
		priorClaimAt.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("claim operation-overflow prior receipt = %+v acquired=%v err=%v",
			priorClaimed, acquired, err)
	}
	priorCompletedAt := priorClaimAt.Add(time.Second)
	if _, err := store.CompleteBillingMutation(
		ctx, priorClaimed, BillingMutationResult{
			Kind: BillingMutationResultAction, Plan: "standard",
			URL: "https://billing.example.test/session/overflow-operation",
		}, priorCompletedAt); err != nil {
		t.Fatalf("complete operation-overflow prior receipt: %v", err)
	}
	if _, acquired, err := store.ClaimBillingMutationAccount(
		ctx, operationMax.AccountID, "bop_overflow_lane_operation_next", 0,
		"bcl_overflow_operation_next", priorCompletedAt.Add(time.Second),
		priorCompletedAt.Add(time.Minute),
	); err == nil || acquired {
		t.Fatalf("max account operation generation advanced: acquired=%v err=%v",
			acquired, err)
	}
	operationAfter := readBillingMutationLane(t, store, operationMax.AccountID)
	if operationAfter.OperationGeneration != math.MaxInt64 ||
		operationAfter.OperationID != operationMax.OperationID {
		t.Fatalf("account operation overflow changed lane: %+v", operationAfter)
	}

	receiptClaimBase := base.Add(30 * time.Minute)
	receiptClaim := billingMutationTestReceipt(
		"bop_overflow_receipt_claim", receiptClaimBase)
	receiptClaimStored, _, err := store.ReceiveBillingMutation(ctx, receiptClaim)
	if err != nil {
		t.Fatal(err)
	}
	receiptClaimMax := receiptClaimStored
	receiptClaimMax.ClaimGeneration = math.MaxInt64
	replaceBillingMutationReceipt(
		t, store, receiptClaimStored, receiptClaimMax)
	if _, acquired, err := store.ClaimBillingMutation(
		ctx, receiptClaimMax, "bcl_overflow_receipt_claim_next",
		receiptClaimBase.Add(time.Second), receiptClaimBase.Add(time.Minute),
	); err == nil || acquired {
		t.Fatalf("max receipt claim generation advanced: acquired=%v err=%v",
			acquired, err)
	}
	receiptClaimAfter, ok, err := store.GetBillingMutation(
		ctx, receiptClaimMax.OperationID)
	if err != nil || !ok || receiptClaimAfter.ClaimGeneration != math.MaxInt64 ||
		receiptClaimAfter.Version != receiptClaimMax.Version {
		t.Fatalf("receipt claim overflow changed state: %+v ok=%v err=%v",
			receiptClaimAfter, ok, err)
	}

	receiptVersionBase := base.Add(40 * time.Minute)
	receiptVersion := billingMutationTestReceipt(
		"bop_overflow_receipt_version", receiptVersionBase)
	receiptVersionStored, _, err := store.ReceiveBillingMutation(ctx, receiptVersion)
	if err != nil {
		t.Fatal(err)
	}
	receiptVersionMax := receiptVersionStored
	receiptVersionMax.Version = math.MaxInt64
	replaceBillingMutationReceipt(
		t, store, receiptVersionStored, receiptVersionMax)
	if _, acquired, err := store.ClaimBillingMutation(
		ctx, receiptVersionMax, "bcl_overflow_receipt_version_next",
		receiptVersionBase.Add(time.Second), receiptVersionBase.Add(time.Minute),
	); err == nil || acquired {
		t.Fatalf("max receipt version advanced: acquired=%v err=%v", acquired, err)
	}
	receiptVersionAfter, ok, err := store.GetBillingMutation(
		ctx, receiptVersionMax.OperationID)
	if err != nil || !ok || receiptVersionAfter.Version != math.MaxInt64 ||
		receiptVersionAfter.ClaimGeneration != receiptVersionMax.ClaimGeneration {
		t.Fatalf("receipt version overflow changed state: %+v ok=%v err=%v",
			receiptVersionAfter, ok, err)
	}
}

func testBillingMutationStoredSchemaContract(
	t *testing.T,
	store BillingMutationStore,
) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 23, 0, 0, 0, time.UTC)
	lane, acquired, err := store.ClaimBillingMutationAccount(
		ctx, "acct_invalid_lane_schema", "bop_invalid_lane_schema", 0,
		"bcl_invalid_lane_schema", now, now.Add(time.Minute))
	if err != nil || !acquired {
		t.Fatalf("seed invalid-schema lane = %+v acquired=%v err=%v",
			lane, acquired, err)
	}
	invalidLane := lane
	invalidLane.SchemaVersion++
	replaceBillingMutationLane(t, store, lane, invalidLane)
	if _, _, err := store.ClaimBillingMutationAccount(
		ctx, lane.AccountID, lane.OperationID, lane.OperationGeneration,
		"bcl_invalid_lane_schema_next", now.Add(time.Minute), now.Add(2*time.Minute),
	); err == nil {
		t.Fatal("stored account lane with unknown schema was accepted")
	}

	receipt := billingMutationTestReceipt("bop_invalid_receipt_schema", now)
	stored, _, err := store.ReceiveBillingMutation(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	invalidReceipt := stored
	invalidReceipt.SchemaVersion++
	replaceBillingMutationReceipt(t, store, stored, invalidReceipt)
	if _, ok, err := store.GetBillingMutation(
		ctx, stored.OperationID); err == nil || ok {
		t.Fatalf("stored receipt with unknown schema = ok=%v err=%v; want rejection",
			ok, err)
	}

	mismatchedReceipt := billingMutationTestReceipt(
		"bop_mismatched_receipt_key", now.Add(time.Minute))
	mismatchedStored, _, err := store.ReceiveBillingMutation(ctx, mismatchedReceipt)
	if err != nil {
		t.Fatal(err)
	}
	mismatchedValue := mismatchedStored
	mismatchedValue.OperationID = "bop_mismatched_receipt_value"
	replaceBillingMutationReceipt(
		t, store, mismatchedStored, mismatchedValue)
	if _, ok, err := store.GetBillingMutation(
		ctx, mismatchedStored.OperationID,
	); !errors.Is(err, ErrBillingMutationConflict) || ok {
		t.Fatalf("stored receipt under mismatched key = ok=%v err=%v; want conflict",
			ok, err)
	}
}

func TestMemStoreBillingMutationReceiptContract(t *testing.T) {
	testBillingMutationStoreContract(t, NewMemStore())
	testConcurrentBillingMutationReceive(t, NewMemStore())
	testConcurrentBillingMutationClaim(t, NewMemStore())
	testBillingAccountMutationLaneContract(t, NewMemStore())
	testBillingMutationReceiptlessLaneExpiry(t, NewMemStore())
	testConcurrentBillingMutationReceiptlessLaneRace(t, NewMemStore())
	testBillingMutationAccountBindingContract(t, NewMemStore())
	testBillingMutationSupersedeAuthorizationFailures(t, NewMemStore())
	testBillingMutationSupersessionContract(t, NewMemStore())
	testConcurrentBillingMutationAccountClaims(t, NewMemStore())
	testConcurrentBillingMutationAdvanceAfterTerminal(t, NewMemStore())
	testConcurrentBillingMutationAccountResume(t, NewMemStore())
	testConcurrentBillingMutationAccountReleaseTakeover(t, NewMemStore())
	testConcurrentBillingMutationClaimSupersede(t, NewMemStore())
	testConcurrentBillingMutationTerminalTransition(t, NewMemStore())
	testBillingMutationCounterOverflowContract(t, NewMemStore())
	testBillingMutationStoredSchemaContract(t, NewMemStore())
}

func TestR2StoreBillingMutationReceiptContract(t *testing.T) {
	testBillingMutationStoreContract(t, newR2Store(t))
	testConcurrentBillingMutationReceive(t, newR2Store(t))
	testConcurrentBillingMutationClaim(t, newR2Store(t))
	testBillingAccountMutationLaneContract(t, newR2Store(t))
	testBillingMutationReceiptlessLaneExpiry(t, newR2Store(t))
	testConcurrentBillingMutationReceiptlessLaneRace(t, newR2Store(t))
	testBillingMutationAccountBindingContract(t, newR2Store(t))
	testBillingMutationSupersedeAuthorizationFailures(t, newR2Store(t))
	testBillingMutationSupersessionContract(t, newR2Store(t))
	testConcurrentBillingMutationAccountClaims(t, newR2Store(t))
	testConcurrentBillingMutationAdvanceAfterTerminal(t, newR2Store(t))
	testConcurrentBillingMutationAccountResume(t, newR2Store(t))
	testConcurrentBillingMutationAccountReleaseTakeover(t, newR2Store(t))
	testConcurrentBillingMutationClaimSupersede(t, newR2Store(t))
	testConcurrentBillingMutationTerminalTransition(t, newR2Store(t))
	testBillingMutationCounterOverflowContract(t, newR2Store(t))
	testBillingMutationStoredSchemaContract(t, newR2Store(t))
}

func TestR2BillingMutationAccountKeyHashesAccountID(t *testing.T) {
	store := newR2Store(t)
	accountID := "acct_private_lane_key"
	key := store.billingMutationAccountKey(accountID)
	if strings.Contains(key, accountID) ||
		!strings.HasPrefix(key, "registry/billing-mutations/accounts/") ||
		!strings.HasSuffix(key, ".json") {
		t.Fatalf("unsafe billing account lane key %q", key)
	}
}

func TestBillingMutationReceiptValidation(t *testing.T) {
	now := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	valid := billingMutationTestReceipt("bop_validation", now)
	valid.Reason = strings.Repeat("r", maxBillingReasonBytes)
	if err := validateBillingMutationReceipt(valid); err != nil {
		t.Fatalf("maximum-length reason rejected: %v", err)
	}
	for name, mutate := range map[string]func(*BillingMutationReceipt){
		"uppercase hash": func(r *BillingMutationReceipt) {
			r.RequestSHA256 = strings.Repeat("A", 64)
		},
		"raw email shape": func(r *BillingMutationReceipt) {
			r.EmailSHA256 = "owner@example.com"
		},
		"multiline reason": func(r *BillingMutationReceipt) {
			r.Reason = "first line\nsecond line"
		},
		"reason too long": func(r *BillingMutationReceipt) {
			r.Reason = strings.Repeat("r", maxBillingReasonBytes+1)
		},
		"bidi reason": func(r *BillingMutationReceipt) {
			r.Reason = "approve \u202ereversed"
		},
		"unknown operation": func(r *BillingMutationReceipt) {
			r.Operation = "refund"
		},
		"unknown schema": func(r *BillingMutationReceipt) {
			r.SchemaVersion++
		},
		"missing upgrade target": func(r *BillingMutationReceipt) {
			r.TargetPlan = ""
		},
		"missing account generation": func(r *BillingMutationReceipt) {
			r.AccountGeneration = 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			receipt := valid
			mutate(&receipt)
			if err := validateBillingMutationReceipt(receipt); err == nil {
				t.Fatalf("invalid receipt accepted: %+v", receipt)
			}
		})
	}
	if err := validateBillingMutationResultForOperation(
		BillingMutationSetup, "", BillingMutationResult{
			Kind: BillingMutationResultAction,
			URL:  "http://billing.example.test/insecure",
		}); err == nil {
		t.Fatal("insecure hosted URL was accepted")
	}
	if err := validateBillingMutationResultForOperation(
		BillingMutationCancel, "", BillingMutationResult{
			Kind: BillingMutationResultDone,
		}); err == nil {
		t.Fatal("result from another operation was accepted")
	}
	for _, raw := range []string{
		"http://billing.example.test/session",
		"https:billing.example.test/opaque",
		"https://user:secret@billing.example.test/session",
		"https://billing.example.test/session\nhttps://forged.example.test",
		"https://billing.example.test/%0ahttps://forged.example.test",
		"https://billing.example.test/%00hidden",
		"https://billing.example.test/%E2%80%AEspoof",
		"https://billing.example.test/\\@forged.example.test",
		"https://billing.example.test/%5c@forged.example.test",
		" https://billing.example.test/session ",
	} {
		if err := validateBillingMutationResultForOperation(
			BillingMutationUpgrade, "standard", BillingMutationResult{
				Kind: BillingMutationResultAction, Plan: "standard", URL: raw,
			}); err == nil {
			t.Errorf("unsafe hosted URL was accepted: %q", raw)
		}
	}
	for _, raw := range []string{
		"https://billing.example.test/session?public=value#done",
		"HTTPS://billing.example.test/session",
	} {
		if err := validateBillingMutationResultForOperation(
			BillingMutationUpgrade, "standard", BillingMutationResult{
				Kind: BillingMutationResultAction, Plan: "standard", URL: raw,
			}); err != nil {
			t.Errorf("safe hosted URL rejected: %q: %v", raw, err)
		}
	}
}

func TestBillingAccountMutationLeaseValidation(t *testing.T) {
	now := time.Date(2026, 8, 11, 16, 30, 0, 0, time.UTC)
	expires := now.Add(time.Minute)
	valid := BillingAccountMutationLease{
		SchemaVersion:       billingAccountMutationLeaseSchemaVersion,
		AccountID:           "acct_lease_validation",
		OperationID:         "bop_lease_validation",
		OperationGeneration: 1,
		ClaimToken:          "bcl_lease_validation",
		ClaimGeneration:     1,
		LeaseExpiresAt:      &expires,
		UpdatedAt:           now,
		Version:             1,
	}
	if err := validateBillingAccountMutationLease(valid); err != nil {
		t.Fatalf("valid account lane rejected: %v", err)
	}
	for name, mutate := range map[string]func(*BillingAccountMutationLease){
		"unknown schema": func(lease *BillingAccountMutationLease) {
			lease.SchemaVersion++
		},
		"zero operation generation": func(lease *BillingAccountMutationLease) {
			lease.OperationGeneration = 0
		},
		"zero claim generation": func(lease *BillingAccountMutationLease) {
			lease.ClaimGeneration = 0
		},
		"expired at update": func(lease *BillingAccountMutationLease) {
			at := lease.UpdatedAt
			lease.LeaseExpiresAt = &at
		},
	} {
		t.Run(name, func(t *testing.T) {
			lease := cloneBillingAccountMutationLease(valid)
			mutate(&lease)
			if err := validateBillingAccountMutationLease(lease); err == nil {
				t.Fatalf("invalid account lane accepted: %+v", lease)
			}
		})
	}
}

func TestBillingMutationClaimTokenValidationMatchesGenerator(t *testing.T) {
	for range 100 {
		token, err := newBillingMutationClaimToken()
		if err != nil {
			t.Fatal(err)
		}
		if !validBillingMutationClaimToken(token) {
			t.Fatalf("generated claim token was rejected: %q", token)
		}
	}
	if validBillingMutationClaimToken("bcl_with spaces") {
		t.Fatal("claim token containing spaces was accepted")
	}
}
