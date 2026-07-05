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
	return r
}
