package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// MemStore is the in-memory Store: the dev/test registry until the control
// plane grows a database, and the reference for what a real Store must do —
// including the compare-and-swap contract on Record.Version.
type MemStore struct {
	mu                      sync.Mutex
	byAcct                  map[string]Record
	eventReceipts           map[string]EventReceipt
	pendingEventKeys        map[string][]string
	billingMutationReceipts map[string]BillingMutationReceipt
	billingMutationAccounts map[string]BillingAccountMutationLease
	pendingBillingMutations map[int]memBillingMutationPendingShard
}

type memBillingMutationPendingShard struct {
	OperationIDs []string
	Cursor       int
}

var _ Store = (*MemStore)(nil)
var _ EventReceiptStore = (*MemStore)(nil)
var _ BillingMutationStore = (*MemStore)(nil)

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{
		byAcct:                  map[string]Record{},
		eventReceipts:           map[string]EventReceipt{},
		pendingEventKeys:        map[string][]string{},
		billingMutationReceipts: map[string]BillingMutationReceipt{},
		billingMutationAccounts: map[string]BillingAccountMutationLease{},
		pendingBillingMutations: map[int]memBillingMutationPendingShard{},
	}
}

// Get implements Store.
func (s *MemStore) Get(_ context.Context, accountID string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byAcct[accountID]
	return clone(r), ok, nil
}

// ByCustomer implements Store: lookups are scoped to the named provider so
// customer ids from different partners can never cross-match.
func (s *MemStore) ByCustomer(_ context.Context, provider, customerID string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if provider == "" || customerID == "" {
		return Record{}, false, nil
	}
	for _, r := range s.byAcct {
		if r.Provider == provider && r.CustomerID == customerID {
			return clone(r), true, nil
		}
	}
	return Record{}, false, nil
}

// Put implements Store with compare-and-swap on Version: a Put whose Version
// does not match the stored record's fails with ErrStale (Version zero is
// create-only), and a successful Put increments the stored Version — so a
// writer holding a stale read can never silently clobber a newer write.
func (s *MemStore) Put(_ context.Context, r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.byAcct[r.AccountID]
	switch {
	case !exists && r.Version != 0:
		return ErrStale
	case exists && current.Version != r.Version:
		return ErrStale
	}
	r.Version++
	s.byAcct[r.AccountID] = clone(r)
	return nil
}

// List implements Store.
func (s *MemStore) List(_ context.Context) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.byAcct))
	for _, r := range s.byAcct {
		out = append(out, clone(r))
	}
	return out, nil
}

// ClaimBillingMutationAccount serializes provider work for one account. A
// zero expected generation starts a new operation only after the prior lane is
// idle; a positive generation can resume only that exact operation.
func (s *MemStore) ClaimBillingMutationAccount(
	_ context.Context,
	accountID, operationID string,
	expectedOperationGeneration int64,
	claimToken string,
	now, leaseExpiresAt time.Time,
) (BillingAccountMutationLease, bool, error) {
	if err := validateBillingAccountMutationClaimRequest(
		accountID, operationID, expectedOperationGeneration,
		claimToken, now, leaseExpiresAt); err != nil {
		return BillingAccountMutationLease{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.billingMutationAccounts[accountID]
	if exists {
		if err := validateBillingAccountMutationLease(current); err != nil {
			return BillingAccountMutationLease{}, false, err
		}
		if current.AccountID != accountID {
			return BillingAccountMutationLease{}, false,
				ErrBillingMutationConflict
		}
	}
	if expectedOperationGeneration > 0 {
		if !exists || current.OperationID != operationID ||
			current.OperationGeneration != expectedOperationGeneration {
			return BillingAccountMutationLease{}, false, ErrBillingMutationSuperseded
		}
	} else if exists && billingAccountMutationLeaseLive(current, now) {
		if current.OperationID == operationID && current.ClaimToken == claimToken {
			return cloneBillingAccountMutationLease(current), true, nil
		}
		return cloneBillingAccountMutationLease(current), false, nil
	}
	if exists && billingAccountMutationLeaseLive(current, now) {
		if current.ClaimToken != claimToken {
			return cloneBillingAccountMutationLease(current), false, nil
		}
		if expectedOperationGeneration == 0 {
			return cloneBillingAccountMutationLease(current), true, nil
		}
		// An exact-generation, same-token call is the post-receipt handshake.
		// Advance its claim generation under this mutex even while the original
		// lease is live, fencing any contender that observed the earlier gap.
	}
	if exists && expectedOperationGeneration == 0 &&
		current.OperationID != operationID {
		prior, ok := s.billingMutationReceipts[current.OperationID]
		if ok {
			if err := validateBillingMutationReceipt(prior); err != nil {
				return BillingAccountMutationLease{}, false, err
			}
			if prior.OperationID != current.OperationID ||
				prior.AccountID != accountID ||
				prior.AccountGeneration != current.OperationGeneration {
				return BillingAccountMutationLease{}, false,
					ErrBillingMutationConflict
			}
			if !billingMutationReceiptTerminal(prior) {
				return cloneBillingAccountMutationLease(current), false, nil
			}
			if now.Before(prior.UpdatedAt) {
				return BillingAccountMutationLease{}, false, errors.New(
					"billing account mutation lease: claim time predates prior terminal receipt")
			}
		}
		// A missing receipt means the prior worker stopped between reserving
		// this lane and durably receiving its operation. Once that reservation
		// is idle, a different operation may advance. A late prior worker must
		// revalidate this exact generation after receiving its receipt, so it
		// cannot reach the provider after this transition wins.
	}
	if exists && now.Before(current.UpdatedAt) {
		return BillingAccountMutationLease{}, false, errors.New(
			"billing account mutation lease: claim time predates current state")
	}
	expires := leaseExpiresAt.UTC()
	if current.ClaimGeneration == math.MaxInt64 || current.Version == math.MaxInt64 {
		return BillingAccountMutationLease{}, false, errors.New(
			"billing account mutation lease: counter overflow")
	}
	if expectedOperationGeneration == 0 {
		current.AccountID = accountID
		if current.OperationID != operationID {
			if current.OperationGeneration == math.MaxInt64 {
				return BillingAccountMutationLease{}, false, errors.New(
					"billing account mutation lease: operation generation overflow")
			}
			current.OperationID = operationID
			current.OperationGeneration++
		}
	}
	if !exists {
		current.SchemaVersion = billingAccountMutationLeaseSchemaVersion
	}
	current.ClaimToken = claimToken
	current.ClaimGeneration++
	current.LeaseExpiresAt = &expires
	current.UpdatedAt = now.UTC()
	current.Version++
	if err := validateBillingAccountMutationLease(current); err != nil {
		return BillingAccountMutationLease{}, false, err
	}
	s.billingMutationAccounts[accountID] =
		cloneBillingAccountMutationLease(current)
	return cloneBillingAccountMutationLease(current), true, nil
}

// ReleaseBillingMutationAccount clears only the exact account-lane claim.
// A newer operation generation can never be released by an older worker.
func (s *MemStore) ReleaseBillingMutationAccount(
	_ context.Context,
	lease BillingAccountMutationLease,
	releasedAt time.Time,
) error {
	if err := validateBillingAccountMutationLease(lease); err != nil {
		return err
	}
	if lease.ClaimToken == "" || releasedAt.IsZero() {
		return errors.New(
			"billing account mutation lease: release requires a claim and timestamp")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.billingMutationAccounts[lease.AccountID]
	if !exists {
		return ErrBillingMutationSuperseded
	}
	if err := validateBillingAccountMutationLease(current); err != nil {
		return err
	}
	if current.AccountID != lease.AccountID {
		return ErrBillingMutationConflict
	}
	if current.OperationID != lease.OperationID ||
		current.OperationGeneration != lease.OperationGeneration {
		return ErrBillingMutationSuperseded
	}
	if current.ClaimToken == "" &&
		current.ClaimGeneration == lease.ClaimGeneration {
		return nil
	}
	if !billingAccountMutationClaimMatches(current, lease) {
		return ErrBillingMutationClaimLost
	}
	if releasedAt.Before(current.UpdatedAt) {
		return errors.New(
			"billing account mutation lease: release time predates current state")
	}
	if current.Version == math.MaxInt64 {
		return errors.New("billing account mutation lease: version overflow")
	}
	current.ClaimToken = ""
	current.LeaseExpiresAt = nil
	current.UpdatedAt = releasedAt.UTC()
	current.Version++
	if err := validateBillingAccountMutationLease(current); err != nil {
		return err
	}
	s.billingMutationAccounts[lease.AccountID] =
		cloneBillingAccountMutationLease(current)
	return nil
}

// GetBillingMutation reads one receipt directly by its globally unique
// operation id without changing replay or claim state.
func (s *MemStore) GetBillingMutation(
	_ context.Context,
	operationID string,
) (BillingMutationReceipt, bool, error) {
	if !validBillingMutationOperationID(operationID) {
		return BillingMutationReceipt{}, false, errors.New(
			"billing mutation receipt: invalid operation id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, ok := s.billingMutationReceipts[operationID]
	if !ok {
		return BillingMutationReceipt{}, false, nil
	}
	if err := validateBillingMutationReceipt(receipt); err != nil {
		return BillingMutationReceipt{}, false, err
	}
	if receipt.OperationID != operationID {
		return BillingMutationReceipt{}, false, ErrBillingMutationConflict
	}
	return cloneBillingMutationReceipt(receipt), ok, nil
}

// ReceiveBillingMutation implements create-only operation identity with exact
// replay. A reused operation id carrying different immutable request semantics
// fails closed before any caller can reach a billing provider.
func (s *MemStore) ReceiveBillingMutation(
	_ context.Context,
	receipt BillingMutationReceipt,
) (BillingMutationReceipt, bool, error) {
	if err := validateBillingMutationReceipt(receipt); err != nil {
		return BillingMutationReceipt{}, false, err
	}
	if receipt.Version != 0 || receipt.Status != BillingMutationPending ||
		receipt.Result != nil || receipt.ClaimToken != "" ||
		receipt.ClaimGeneration != 0 {
		return BillingMutationReceipt{}, false, errors.New(
			"billing mutation receipt: new receipt must be unclaimed pending at version zero")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.billingMutationReceipts[receipt.OperationID]; ok {
		if err := validateBillingMutationReceipt(existing); err != nil {
			return BillingMutationReceipt{}, false, err
		}
		if !sameBillingMutationIdentity(existing, receipt) {
			return BillingMutationReceipt{}, false, fmt.Errorf(
				"%w: operation=%s", ErrBillingMutationConflict,
				receipt.OperationID)
		}
		if existing.Status == BillingMutationPending {
			if err := s.ensurePendingBillingMutationLocked(existing); err != nil {
				return BillingMutationReceipt{}, false, err
			}
		}
		return cloneBillingMutationReceipt(existing), false, nil
	}
	receipt.Version = 1
	s.billingMutationReceipts[receipt.OperationID] =
		cloneBillingMutationReceipt(receipt)
	if err := s.ensurePendingBillingMutationLocked(receipt); err != nil {
		return cloneBillingMutationReceipt(receipt), true, err
	}
	return cloneBillingMutationReceipt(receipt), true, nil
}

func (s *MemStore) ensurePendingBillingMutationLocked(
	receipt BillingMutationReceipt,
) error {
	shardID := pendingBillingMutationShard(receipt.OperationID)
	shard := s.pendingBillingMutations[shardID]
	for _, operationID := range shard.OperationIDs {
		if operationID == receipt.OperationID {
			return nil
		}
	}
	if len(shard.OperationIDs) >= maxPendingBillingMutationsPerShard {
		return errBillingMutationPendingIndexFull
	}
	shard.OperationIDs = append(shard.OperationIDs, receipt.OperationID)
	s.pendingBillingMutations[shardID] = shard
	return nil
}

func (s *MemStore) removePendingBillingMutationLocked(
	operationID string,
) {
	shardID := pendingBillingMutationShard(operationID)
	shard := s.pendingBillingMutations[shardID]
	kept := shard.OperationIDs[:0]
	removedBeforeCursor := 0
	for index, candidate := range shard.OperationIDs {
		if candidate != operationID {
			kept = append(kept, candidate)
		} else if index < shard.Cursor {
			removedBeforeCursor++
		}
	}
	if len(kept) == 0 {
		delete(s.pendingBillingMutations, shardID)
		return
	}
	shard.Cursor -= removedBeforeCursor
	shard.OperationIDs = kept
	shard.Cursor %= len(kept)
	s.pendingBillingMutations[shardID] = shard
}

// ClaimBillingMutation atomically grants one live worker the mutation lease.
// An expired or explicitly released receipt advances ClaimGeneration before a
// successor can write, fencing stale completion and release attempts.
func (s *MemStore) ClaimBillingMutation(
	_ context.Context,
	receipt BillingMutationReceipt,
	claimToken string,
	now, leaseExpiresAt time.Time,
) (BillingMutationReceipt, bool, error) {
	if err := validateBillingMutationReceipt(receipt); err != nil {
		return BillingMutationReceipt{}, false, err
	}
	if err := validateBillingMutationClaimRequest(
		claimToken, now, leaseExpiresAt); err != nil {
		return BillingMutationReceipt{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.billingMutationReceipts[receipt.OperationID]
	if !ok {
		return BillingMutationReceipt{}, false, errors.New(
			"billing mutation receipt: receipt disappeared before claim")
	}
	if err := validateBillingMutationReceipt(current); err != nil {
		return BillingMutationReceipt{}, false, err
	}
	if !sameBillingMutationIdentity(current, receipt) {
		return BillingMutationReceipt{}, false, ErrBillingMutationConflict
	}
	if current.Status == BillingMutationCompleted {
		return cloneBillingMutationReceipt(current), false, nil
	}
	if current.Status == BillingMutationSuperseded {
		return BillingMutationReceipt{}, false, ErrBillingMutationSuperseded
	}
	if current.ClaimToken == claimToken && current.LeaseExpiresAt != nil &&
		now.Before(*current.LeaseExpiresAt) {
		return cloneBillingMutationReceipt(current), true, nil
	}
	if current.ClaimToken != "" && current.LeaseExpiresAt != nil &&
		now.Before(*current.LeaseExpiresAt) {
		return cloneBillingMutationReceipt(current), false, nil
	}
	if now.Before(current.UpdatedAt) {
		return BillingMutationReceipt{}, false, errors.New(
			"billing mutation receipt: claim time predates current state")
	}
	if current.ClaimGeneration == math.MaxInt64 || current.Version == math.MaxInt64 {
		return BillingMutationReceipt{}, false, errors.New(
			"billing mutation receipt: counter overflow")
	}
	expires := leaseExpiresAt.UTC()
	current.ClaimToken = claimToken
	current.ClaimGeneration++
	current.LeaseExpiresAt = &expires
	current.UpdatedAt = now.UTC()
	current.Version++
	s.billingMutationReceipts[receipt.OperationID] =
		cloneBillingMutationReceipt(current)
	return cloneBillingMutationReceipt(current), true, nil
}

// CompleteBillingMutation atomically pins the first allowlisted result and
// makes the receipt terminal. Exact completion replay is idempotent; a changed
// result under the same operation identity is a conflict.
func (s *MemStore) CompleteBillingMutation(
	_ context.Context,
	receipt BillingMutationReceipt,
	result BillingMutationResult,
	completedAt time.Time,
) (BillingMutationReceipt, error) {
	if err := validateBillingMutationReceipt(receipt); err != nil {
		return BillingMutationReceipt{}, err
	}
	if err := validateBillingMutationResultForOperation(
		receipt.Operation, receipt.ExecutionClass,
		receipt.TargetPlan, result); err != nil {
		return BillingMutationReceipt{}, err
	}
	if completedAt.IsZero() {
		return BillingMutationReceipt{}, errors.New(
			"billing mutation receipt: completed_at is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.billingMutationReceipts[receipt.OperationID]
	if !ok {
		return BillingMutationReceipt{}, errors.New(
			"billing mutation receipt: receipt disappeared before completion")
	}
	if err := validateBillingMutationReceipt(current); err != nil {
		return BillingMutationReceipt{}, err
	}
	if !sameBillingMutationIdentity(current, receipt) {
		return BillingMutationReceipt{}, ErrBillingMutationConflict
	}
	if current.Status == BillingMutationCompleted {
		if current.Result == nil ||
			!sameBillingMutationResult(*current.Result, result) {
			return BillingMutationReceipt{}, fmt.Errorf(
				"%w: terminal result mismatch", ErrBillingMutationConflict)
		}
		s.removePendingBillingMutationLocked(current.OperationID)
		return cloneBillingMutationReceipt(current), nil
	}
	if current.Status == BillingMutationSuperseded {
		return BillingMutationReceipt{}, ErrBillingMutationSuperseded
	}
	if !billingMutationClaimMatches(current, receipt) {
		return BillingMutationReceipt{}, ErrBillingMutationClaimLost
	}
	if completedAt.Before(current.UpdatedAt) {
		return BillingMutationReceipt{}, errors.New(
			"billing mutation receipt: completion time predates current state")
	}
	if current.Version == math.MaxInt64 {
		return BillingMutationReceipt{}, errors.New(
			"billing mutation receipt: version overflow")
	}
	at := completedAt.UTC()
	resultCopy := cloneBillingMutationResult(result)
	current.Status = BillingMutationCompleted
	current.Result = &resultCopy
	current.ClaimToken = ""
	current.LeaseExpiresAt = nil
	current.UpdatedAt = at
	current.CompletedAt = &at
	current.Version++
	if err := validateBillingMutationReceipt(current); err != nil {
		return BillingMutationReceipt{}, err
	}
	s.billingMutationReceipts[receipt.OperationID] =
		cloneBillingMutationReceipt(current)
	s.removePendingBillingMutationLocked(current.OperationID)
	return cloneBillingMutationReceipt(current), nil
}

// SupersedeBillingMutation terminally retires an older pending receipt after
// its processing claim is absent or expired. The cleared claim plus terminal
// status fences every stale completion attempt.
func (s *MemStore) SupersedeBillingMutation(
	_ context.Context,
	receipt BillingMutationReceipt,
	supersededAt time.Time,
) (BillingMutationReceipt, error) {
	if err := validateBillingMutationReceipt(receipt); err != nil {
		return BillingMutationReceipt{}, err
	}
	if supersededAt.IsZero() {
		return BillingMutationReceipt{}, errors.New(
			"billing mutation receipt: superseded_at is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.billingMutationReceipts[receipt.OperationID]
	if !ok {
		return BillingMutationReceipt{}, errors.New(
			"billing mutation receipt: receipt disappeared before supersede")
	}
	if err := validateBillingMutationReceipt(current); err != nil {
		return BillingMutationReceipt{}, err
	}
	if !sameBillingMutationIdentity(current, receipt) {
		return BillingMutationReceipt{}, ErrBillingMutationConflict
	}
	switch current.Status {
	case BillingMutationSuperseded:
		s.removePendingBillingMutationLocked(current.OperationID)
		return cloneBillingMutationReceipt(current), nil
	case BillingMutationCompleted:
		return BillingMutationReceipt{}, ErrBillingMutationConflict
	}
	lane, laneExists := s.billingMutationAccounts[current.AccountID]
	if !laneExists {
		return BillingMutationReceipt{}, ErrBillingMutationConflict
	}
	if err := validateBillingAccountMutationLease(lane); err != nil {
		return BillingMutationReceipt{}, err
	}
	if lane.AccountID != current.AccountID ||
		lane.OperationGeneration <= current.AccountGeneration ||
		lane.OperationID == current.OperationID {
		return BillingMutationReceipt{}, ErrBillingMutationConflict
	}
	if supersededAt.Before(lane.UpdatedAt) {
		return BillingMutationReceipt{}, errors.New(
			"billing mutation receipt: supersede time predates superseding account lane")
	}
	if current.ClaimToken != "" && current.LeaseExpiresAt != nil &&
		supersededAt.Before(*current.LeaseExpiresAt) {
		return BillingMutationReceipt{}, ErrBillingMutationClaimActive
	}
	if supersededAt.Before(current.UpdatedAt) {
		return BillingMutationReceipt{}, errors.New(
			"billing mutation receipt: supersede time predates current state")
	}
	if current.Version == math.MaxInt64 {
		return BillingMutationReceipt{}, errors.New(
			"billing mutation receipt: version overflow")
	}
	at := supersededAt.UTC()
	current.Status = BillingMutationSuperseded
	current.Result = nil
	current.ClaimToken = ""
	current.LeaseExpiresAt = nil
	current.UpdatedAt = at
	current.CompletedAt = &at
	current.SupersededByOperationID = lane.OperationID
	current.Version++
	if err := validateBillingMutationReceipt(current); err != nil {
		return BillingMutationReceipt{}, err
	}
	s.billingMutationReceipts[receipt.OperationID] =
		cloneBillingMutationReceipt(current)
	s.removePendingBillingMutationLocked(current.OperationID)
	return cloneBillingMutationReceipt(current), nil
}

// ReleaseBillingMutation clears exactly one live claim. Repeating an
// ambiguous successful release is safe; an older generation cannot release a
// successor's claim.
func (s *MemStore) ReleaseBillingMutation(
	_ context.Context,
	receipt BillingMutationReceipt,
	releasedAt time.Time,
) error {
	if err := validateBillingMutationReceipt(receipt); err != nil {
		return err
	}
	if receipt.ClaimToken == "" || receipt.ClaimGeneration < 1 ||
		releasedAt.IsZero() {
		return errors.New("billing mutation receipt: release requires a claim and timestamp")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.billingMutationReceipts[receipt.OperationID]
	if !ok {
		return errors.New("billing mutation receipt: receipt disappeared before release")
	}
	if err := validateBillingMutationReceipt(current); err != nil {
		return err
	}
	if !sameBillingMutationIdentity(current, receipt) {
		return ErrBillingMutationConflict
	}
	if current.Status == BillingMutationCompleted {
		s.removePendingBillingMutationLocked(current.OperationID)
		return nil
	}
	if current.Status == BillingMutationSuperseded {
		s.removePendingBillingMutationLocked(current.OperationID)
		return nil
	}
	if current.ClaimToken == "" &&
		current.ClaimGeneration == receipt.ClaimGeneration {
		if err := s.ensurePendingBillingMutationLocked(current); err != nil {
			return err
		}
		return nil
	}
	if !billingMutationClaimMatches(current, receipt) {
		return ErrBillingMutationClaimLost
	}
	if releasedAt.Before(current.UpdatedAt) {
		return errors.New(
			"billing mutation receipt: release time predates current state")
	}
	if current.Version == math.MaxInt64 {
		return errors.New("billing mutation receipt: version overflow")
	}
	current.ClaimToken = ""
	current.LeaseExpiresAt = nil
	current.UpdatedAt = releasedAt.UTC()
	current.Version++
	s.billingMutationReceipts[receipt.OperationID] =
		cloneBillingMutationReceipt(current)
	if err := s.ensurePendingBillingMutationLocked(current); err != nil {
		return err
	}
	return nil
}

// PendingBillingMutations rotates a bounded window through every global shard.
// Poison references remain visible and return an error; only a validated
// terminal receipt is safe to remove automatically.
func (s *MemStore) PendingBillingMutations(
	_ context.Context,
	limit int,
) (BillingMutationPendingBatch, error) {
	if limit < billingMutationPendingShardCount ||
		limit > billingMutationPendingShardCount*maxPendingBillingMutationsPerShard {
		return BillingMutationPendingBatch{}, fmt.Errorf(
			"billing mutation receipt: pending limit must be %d-%d",
			billingMutationPendingShardCount,
			billingMutationPendingShardCount*maxPendingBillingMutationsPerShard)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	batch := BillingMutationPendingBatch{
		Receipts: make([]BillingMutationReceipt, 0, limit),
	}
	selected := make([]billingMutationPendingReference, 0, limit)
	baseQuota := limit / billingMutationPendingShardCount
	remainder := limit % billingMutationPendingShardCount
	for shardID := 0; shardID < billingMutationPendingShardCount; shardID++ {
		quota := baseQuota
		if shardID < remainder {
			quota++
		}
		shard, ok := s.pendingBillingMutations[shardID]
		if !ok || len(shard.OperationIDs) == 0 {
			continue
		}
		if shard.Cursor < 0 || shard.Cursor >= len(shard.OperationIDs) {
			return BillingMutationPendingBatch{}, fmt.Errorf(
				"billing mutation receipt: shard %d has invalid cursor", shardID)
		}
		count := min(quota, len(shard.OperationIDs))
		batch.ScanCapped = batch.ScanCapped || len(shard.OperationIDs) > count
		for offset := 0; offset < count; offset++ {
			index := (shard.Cursor + offset) % len(shard.OperationIDs)
			selected = append(selected, billingMutationPendingReference{
				operationID: shard.OperationIDs[index],
				shardID:     shardID,
			})
		}
		shard.Cursor = (shard.Cursor + count) % len(shard.OperationIDs)
		s.pendingBillingMutations[shardID] = shard
	}
	batch.Scanned = len(selected)
	var firstErr error
	seen := make(map[string]struct{}, len(selected))
	for _, reference := range selected {
		operationID := reference.operationID
		if !validBillingMutationOperationID(operationID) ||
			pendingBillingMutationShard(operationID) != reference.shardID {
			firstErr = errors.Join(firstErr, fmt.Errorf(
				"billing mutation receipt: pending shard %d contains an invalid operation id",
				reference.shardID))
			continue
		}
		if _, duplicate := seen[operationID]; duplicate {
			firstErr = errors.Join(firstErr, fmt.Errorf(
				"billing mutation receipt: pending shard %d contains a duplicate operation id",
				reference.shardID))
			continue
		}
		seen[operationID] = struct{}{}
		receipt, ok := s.billingMutationReceipts[operationID]
		if !ok {
			firstErr = errors.Join(firstErr, fmt.Errorf(
				"billing mutation receipt: pending receipt %s is unavailable",
				operationID))
			continue
		}
		if err := validateBillingMutationReceipt(receipt); err != nil {
			firstErr = errors.Join(firstErr, fmt.Errorf(
				"billing mutation receipt: invalid pending receipt %s: %w",
				operationID, err))
			continue
		}
		if receipt.OperationID != operationID ||
			pendingBillingMutationShard(receipt.OperationID) !=
				pendingBillingMutationShard(operationID) {
			firstErr = errors.Join(firstErr, fmt.Errorf(
				"billing mutation receipt: pending index points at mismatched receipt %s",
				operationID))
			continue
		}
		if receipt.Status != BillingMutationPending {
			s.removePendingBillingMutationLocked(operationID)
			batch.TerminalCleanup++
			continue
		}
		if batch.OldestObservedPendingAt == nil ||
			receipt.CreatedAt.Before(*batch.OldestObservedPendingAt) {
			createdAt := receipt.CreatedAt
			batch.OldestObservedPendingAt = &createdAt
		}
		batch.Receipts = append(batch.Receipts,
			cloneBillingMutationReceipt(receipt))
	}
	sort.Slice(batch.Receipts, func(i, j int) bool {
		if batch.Receipts[i].CreatedAt.Equal(batch.Receipts[j].CreatedAt) {
			return batch.Receipts[i].OperationID < batch.Receipts[j].OperationID
		}
		return batch.Receipts[i].CreatedAt.Before(batch.Receipts[j].CreatedAt)
	})
	return batch, firstErr
}

// ReceiveEvent implements EventReceiptStore with create-only event identity
// semantics. The pending index is bounded per provider customer.
func (s *MemStore) ReceiveEvent(
	_ context.Context,
	receipt EventReceipt,
) (EventReceipt, bool, error) {
	if err := validateEventReceipt(receipt); err != nil {
		return EventReceipt{}, false, err
	}
	if receipt.Version != 0 || receipt.Status != EventReceiptPending {
		return EventReceipt{}, false, fmt.Errorf("event receipt: new receipt must be pending at version zero")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	identityKey := eventReceiptIdentityKey(
		receipt.Provider, receipt.Event.ProviderEventID)
	if existing, ok := s.eventReceipts[identityKey]; ok {
		if !sameEventReceiptIdentity(existing, receipt) {
			return EventReceipt{}, false, fmt.Errorf(
				"%w: provider=%s event=%s",
				ErrEventReceiptConflict, receipt.Provider,
				receipt.Event.ProviderEventID)
		}
		if existing.Status == EventReceiptPending {
			if err := s.ensurePendingEventLocked(existing, identityKey); err != nil {
				return EventReceipt{}, false, err
			}
		}
		return cloneEventReceipt(existing), false, nil
	}
	if err := s.ensurePendingEventLocked(receipt, identityKey); err != nil {
		return EventReceipt{}, false, err
	}
	receipt.Version = 1
	s.eventReceipts[identityKey] = cloneEventReceipt(receipt)
	return cloneEventReceipt(receipt), true, nil
}

func (s *MemStore) ensurePendingEventLocked(receipt EventReceipt, identityKey string) error {
	customerKey := eventReceiptCustomerKey(
		receipt.Provider, receipt.Event.CustomerID)
	keys := s.pendingEventKeys[customerKey]
	for _, key := range keys {
		if key == identityKey {
			return nil
		}
	}
	if len(keys) >= maxPendingEventReceiptsPerCustomer {
		return fmt.Errorf(
			"event receipt: pending index is full for provider customer")
	}
	s.pendingEventKeys[customerKey] = append(keys, identityKey)
	return nil
}

// ClaimEvent implements EventReceiptStore. Claim acquisition and expired-lease
// takeover are atomic with respect to every other in-memory receipt operation.
func (s *MemStore) ClaimEvent(
	_ context.Context,
	receipt EventReceipt,
	claimToken string,
	now, leaseExpiresAt time.Time,
) (EventReceipt, bool, error) {
	if err := validateEventReceipt(receipt); err != nil {
		return EventReceipt{}, false, err
	}
	if err := validateEventClaimRequest(claimToken, now, leaseExpiresAt); err != nil {
		return EventReceipt{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	identityKey := eventReceiptIdentityKey(
		receipt.Provider, receipt.Event.ProviderEventID)
	current, ok := s.eventReceipts[identityKey]
	if !ok {
		return EventReceipt{}, false, errors.New(
			"event receipt: receipt disappeared before claim")
	}
	if !sameEventReceiptIdentity(current, receipt) {
		return EventReceipt{}, false, ErrEventReceiptConflict
	}
	if current.Status == EventReceiptProcessed {
		return cloneEventReceipt(current), false, nil
	}
	if err := s.ensurePendingEventLocked(current, identityKey); err != nil {
		return EventReceipt{}, false, err
	}
	if current.ClaimToken == claimToken && current.LeaseExpiresAt != nil &&
		now.Before(*current.LeaseExpiresAt) {
		return cloneEventReceipt(current), true, nil
	}
	if current.ClaimToken != "" && current.LeaseExpiresAt != nil &&
		now.Before(*current.LeaseExpiresAt) {
		return cloneEventReceipt(current), false, nil
	}
	expires := leaseExpiresAt.UTC()
	current.ClaimToken = claimToken
	current.ClaimGeneration++
	current.LeaseExpiresAt = &expires
	current.Version++
	s.eventReceipts[identityKey] = cloneEventReceipt(current)
	return cloneEventReceipt(current), true, nil
}

// PinEventResolution stores the exact routing result before the claim owner
// mutates account state. The first fenced write wins permanently; recovery
// workers may only reuse that same resolution.
func (s *MemStore) PinEventResolution(
	_ context.Context,
	receipt EventReceipt,
	resolution EventReceiptResolution,
) (EventReceipt, error) {
	if err := validateEventReceipt(receipt); err != nil {
		return EventReceipt{}, err
	}
	if err := validateEventReceiptResolution(receipt.Event, resolution); err != nil {
		return EventReceipt{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	identityKey := eventReceiptIdentityKey(
		receipt.Provider, receipt.Event.ProviderEventID)
	current, ok := s.eventReceipts[identityKey]
	if !ok {
		return EventReceipt{}, errors.New(
			"event receipt: receipt disappeared before resolution pin")
	}
	if !sameEventReceiptIdentity(current, receipt) {
		return EventReceipt{}, ErrEventReceiptConflict
	}
	if current.Status == EventReceiptProcessed {
		if current.Resolution == nil ||
			!sameEventReceiptResolution(*current.Resolution, resolution) {
			return EventReceipt{}, fmt.Errorf(
				"%w: pinned resolution mismatch", ErrEventReceiptConflict)
		}
		return cloneEventReceipt(current), nil
	}
	if !eventReceiptClaimMatches(current, receipt) {
		return EventReceipt{}, ErrEventReceiptClaimLost
	}
	if current.Resolution != nil {
		if !sameEventReceiptResolution(*current.Resolution, resolution) {
			return EventReceipt{}, fmt.Errorf(
				"%w: pinned resolution mismatch", ErrEventReceiptConflict)
		}
		return cloneEventReceipt(current), nil
	}
	resolutionCopy := resolution
	if resolution.Event != nil {
		event := *resolution.Event
		resolutionCopy.Event = &event
	}
	current.Resolution = &resolutionCopy
	current.Version++
	s.eventReceipts[identityKey] = cloneEventReceipt(current)
	return cloneEventReceipt(current), nil
}

// ReleaseEvent relinquishes only the exact claim generation supplied by its
// owner. A stale worker cannot clear a replacement worker's lease.
func (s *MemStore) ReleaseEvent(
	_ context.Context,
	receipt EventReceipt,
) error {
	if err := validateEventReceipt(receipt); err != nil {
		return err
	}
	if receipt.ClaimToken == "" || receipt.ClaimGeneration < 1 {
		return errors.New("event receipt: release requires a processing claim")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	identityKey := eventReceiptIdentityKey(
		receipt.Provider, receipt.Event.ProviderEventID)
	current, ok := s.eventReceipts[identityKey]
	if !ok {
		return errors.New("event receipt: receipt disappeared before release")
	}
	if !sameEventReceiptIdentity(current, receipt) {
		return ErrEventReceiptConflict
	}
	if current.Status == EventReceiptProcessed {
		return nil
	}
	if current.ClaimToken == "" &&
		current.ClaimGeneration == receipt.ClaimGeneration {
		return s.ensurePendingEventLocked(current, identityKey)
	}
	if !eventReceiptClaimMatches(current, receipt) {
		return ErrEventReceiptClaimLost
	}
	current.ClaimToken = ""
	current.LeaseExpiresAt = nil
	current.Version++
	s.eventReceipts[identityKey] = cloneEventReceipt(current)
	return s.ensurePendingEventLocked(current, identityKey)
}

// CompleteEvent implements EventReceiptStore. Marking the receipt and removing
// its pending-index entry are atomic under the in-memory store lock.
func (s *MemStore) CompleteEvent(
	_ context.Context,
	receipt EventReceipt,
	processedAt time.Time,
) error {
	if err := validateEventReceipt(receipt); err != nil {
		return err
	}
	if processedAt.IsZero() {
		return fmt.Errorf("event receipt: processed_at is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	identityKey := eventReceiptIdentityKey(
		receipt.Provider, receipt.Event.ProviderEventID)
	current, ok := s.eventReceipts[identityKey]
	if !ok {
		return fmt.Errorf("event receipt: receipt disappeared before completion")
	}
	if !sameEventReceiptIdentity(current, receipt) {
		return ErrEventReceiptConflict
	}
	if current.Status == EventReceiptProcessed {
		if receipt.Resolution == nil || current.Resolution == nil ||
			!sameEventReceiptResolution(*current.Resolution, *receipt.Resolution) {
			return fmt.Errorf(
				"%w: pinned resolution mismatch", ErrEventReceiptConflict)
		}
	}
	if current.Status == EventReceiptPending {
		if !eventReceiptClaimMatches(current, receipt) {
			return ErrEventReceiptClaimLost
		}
		if current.Resolution == nil || receipt.Resolution == nil ||
			!sameEventReceiptResolution(*current.Resolution, *receipt.Resolution) {
			return fmt.Errorf(
				"%w: completion requires the pinned resolution",
				ErrEventReceiptConflict)
		}
		processedAt = processedAt.UTC()
		current.Status = EventReceiptProcessed
		current.Decision = current.Resolution.Decision
		current.ProcessedAt = &processedAt
		current.ClaimToken = ""
		current.LeaseExpiresAt = nil
		current.Version++
		s.eventReceipts[identityKey] = cloneEventReceipt(current)
	}
	customerKey := eventReceiptCustomerKey(
		current.Provider, current.Event.CustomerID)
	keys := s.pendingEventKeys[customerKey]
	kept := keys[:0]
	for _, key := range keys {
		if key != identityKey {
			kept = append(kept, key)
		}
	}
	if len(kept) == 0 {
		delete(s.pendingEventKeys, customerKey)
	} else {
		s.pendingEventKeys[customerKey] = kept
	}
	return nil
}

// PendingEvents implements EventReceiptStore with a bounded, chronological
// view of one provider customer's retry work.
func (s *MemStore) PendingEvents(
	_ context.Context,
	provider, customerID string,
	limit int,
) ([]EventReceipt, error) {
	if limit < 1 || limit > maxPendingEventReceiptsPerCustomer {
		return nil, fmt.Errorf("event receipt: pending limit must be 1-%d",
			maxPendingEventReceiptsPerCustomer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	customerKey := eventReceiptCustomerKey(provider, customerID)
	keys := s.pendingEventKeys[customerKey]
	out := make([]EventReceipt, 0, min(limit, len(keys)))
	kept := keys[:0]
	for _, key := range keys {
		receipt, ok := s.eventReceipts[key]
		if !ok || receipt.Status != EventReceiptPending {
			continue
		}
		kept = append(kept, key)
		if len(out) < limit {
			out = append(out, cloneEventReceipt(receipt))
		}
	}
	if len(kept) == 0 {
		delete(s.pendingEventKeys, customerKey)
	} else {
		s.pendingEventKeys[customerKey] = kept
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Event.At.Equal(out[j].Event.At) {
			return out[i].Event.ProviderEventID < out[j].Event.ProviderEventID
		}
		return out[i].Event.At.Before(out[j].Event.At)
	})
	return out, nil
}

// clone deep-copies a Record so callers never alias the store's pointers.
func clone(r Record) Record {
	if r.Pending != nil {
		p := *r.Pending
		r.Pending = &p
	}
	if r.PastDueSince != nil {
		t := *r.PastDueSince
		r.PastDueSince = &t
	}
	if r.PlanOverride != nil {
		override := *r.PlanOverride
		r.PlanOverride = &override
	}
	if r.TranscriptRetentionOverride != nil {
		override := *r.TranscriptRetentionOverride
		if override.Days != nil {
			days := *override.Days
			override.Days = &days
		}
		r.TranscriptRetentionOverride = &override
	}
	if r.MessagingOverride != nil {
		override := *r.MessagingOverride
		r.MessagingOverride = &override
	}
	if r.MessageRetentionOverride != nil {
		override := *r.MessageRetentionOverride
		if override.Days != nil {
			days := *override.Days
			override.Days = &days
		}
		r.MessageRetentionOverride = &override
	}
	if r.AgentEmailReceiveOverride != nil {
		override := *r.AgentEmailReceiveOverride
		r.AgentEmailReceiveOverride = &override
	}
	if r.AgentEmailRetentionOverride != nil {
		override := *r.AgentEmailRetentionOverride
		if override.Days != nil {
			days := *override.Days
			override.Days = &days
		}
		r.AgentEmailRetentionOverride = &override
	}
	if r.LimitOverrides != nil {
		overrides := make(map[string]AccountLimitOverride, len(r.LimitOverrides))
		for dimension, override := range r.LimitOverrides {
			if override.Max != nil {
				maxValue := *override.Max
				override.Max = &maxValue
			}
			overrides[dimension] = override
		}
		r.LimitOverrides = overrides
	}
	if r.AdminHistory != nil {
		r.AdminHistory = append([]AdminChange(nil), r.AdminHistory...)
		for i := range r.AdminHistory {
			if r.AdminHistory[i].RetentionFrom != nil {
				value := *r.AdminHistory[i].RetentionFrom
				r.AdminHistory[i].RetentionFrom = &value
			}
			if r.AdminHistory[i].RetentionTo != nil {
				value := *r.AdminHistory[i].RetentionTo
				r.AdminHistory[i].RetentionTo = &value
			}
			if r.AdminHistory[i].MessagingFrom != nil {
				value := *r.AdminHistory[i].MessagingFrom
				r.AdminHistory[i].MessagingFrom = &value
			}
			if r.AdminHistory[i].MessagingTo != nil {
				value := *r.AdminHistory[i].MessagingTo
				r.AdminHistory[i].MessagingTo = &value
			}
			if r.AdminHistory[i].MessageRetentionFrom != nil {
				value := *r.AdminHistory[i].MessageRetentionFrom
				r.AdminHistory[i].MessageRetentionFrom = &value
			}
			if r.AdminHistory[i].MessageRetentionTo != nil {
				value := *r.AdminHistory[i].MessageRetentionTo
				r.AdminHistory[i].MessageRetentionTo = &value
			}
			if r.AdminHistory[i].AgentEmailReceiveFrom != nil {
				value := *r.AdminHistory[i].AgentEmailReceiveFrom
				r.AdminHistory[i].AgentEmailReceiveFrom = &value
			}
			if r.AdminHistory[i].AgentEmailReceiveTo != nil {
				value := *r.AdminHistory[i].AgentEmailReceiveTo
				r.AdminHistory[i].AgentEmailReceiveTo = &value
			}
			if r.AdminHistory[i].AgentEmailRetentionFrom != nil {
				value := *r.AdminHistory[i].AgentEmailRetentionFrom
				r.AdminHistory[i].AgentEmailRetentionFrom = &value
			}
			if r.AdminHistory[i].AgentEmailRetentionTo != nil {
				value := *r.AdminHistory[i].AgentEmailRetentionTo
				r.AdminHistory[i].AgentEmailRetentionTo = &value
			}
			if r.AdminHistory[i].LimitFrom != nil {
				value := *r.AdminHistory[i].LimitFrom
				if value.Max != nil {
					maxValue := *value.Max
					value.Max = &maxValue
				}
				r.AdminHistory[i].LimitFrom = &value
			}
			if r.AdminHistory[i].LimitTo != nil {
				value := *r.AdminHistory[i].LimitTo
				if value.Max != nil {
					maxValue := *value.Max
					value.Max = &maxValue
				}
				r.AdminHistory[i].LimitTo = &value
			}
		}
	}
	return r
}
