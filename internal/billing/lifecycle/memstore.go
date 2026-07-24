package lifecycle

import (
	"context"
	"sync"
)

// MemStore is the in-memory Store: the dev/test registry until the control
// plane grows a database, and the reference for what a real Store must do —
// including the compare-and-swap contract on Record.Version.
type MemStore struct {
	mu     sync.Mutex
	byAcct map[string]Record
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{byAcct: map[string]Record{}}
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
