package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemStore is the in-memory Store: the dev/test registry until the control
// plane grows a database, and the reference for what a real Store must do —
// including the compare-and-swap contract on Record.Version.
type MemStore struct {
	mu               sync.Mutex
	byAcct           map[string]Record
	eventReceipts    map[string]EventReceipt
	pendingEventKeys map[string][]string
}

var _ Store = (*MemStore)(nil)
var _ EventReceiptStore = (*MemStore)(nil)

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{
		byAcct:           map[string]Record{},
		eventReceipts:    map[string]EventReceipt{},
		pendingEventKeys: map[string][]string{},
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
