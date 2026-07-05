package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/witwave-ai/witself/internal/blob"
)

// R2Store implements Store on an S3-compatible bucket (Cloudflare R2) — the
// control plane's no-database registry. One JSON object per account under
// <prefix>accounts/, with the storage layer's conditional writes enforcing
// the CAS contract: a stale writer's If-Match PUT gets 412, which surfaces as
// ErrStale exactly like MemStore's version check.
//
// Webhook lookups use small index objects under <prefix>customers/
// (<provider>/<customerID> -> accountID). The index is written BEFORE the
// record that references it, so a crash between the two writes can only leave
// a dangling index — never a record whose customer is unfindable. ByCustomer
// verifies the pointed-at record actually carries the (provider, customerID)
// pair, so dangles read as not-found and are harmless until reused.
type R2Store struct {
	c      *blob.Client
	prefix string
}

var _ Store = (*R2Store)(nil)

// NewR2Store returns an R2Store on c, namespacing every key under prefix
// (e.g. "registry/"). prefix may be empty.
func NewR2Store(c *blob.Client, prefix string) *R2Store {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &R2Store{c: c, prefix: prefix}
}

func (s *R2Store) accountKey(accountID string) string {
	return s.prefix + "accounts/" + accountID + ".json"
}

func (s *R2Store) customerKey(provider, customerID string) string {
	return s.prefix + "customers/" + provider + "/" + customerID
}

// Get implements Store.
func (s *R2Store) Get(ctx context.Context, accountID string) (Record, bool, error) {
	r, _, ok, err := s.get(ctx, accountID)
	return r, ok, err
}

func (s *R2Store) get(ctx context.Context, accountID string) (Record, string, bool, error) {
	data, etag, err := s.c.Get(ctx, s.accountKey(accountID))
	if errors.Is(err, blob.ErrNotFound) {
		return Record{}, "", false, nil
	}
	if err != nil {
		return Record{}, "", false, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, "", false, fmt.Errorf("r2store: decode record %s: %w", accountID, err)
	}
	return r, etag, true, nil
}

// ByCustomer implements Store: index lookup + verification against the
// pointed-at record, so a dangling index (crash between index and record
// writes, or a superseded pin) reads as not-found instead of misrouting a
// webhook event.
func (s *R2Store) ByCustomer(ctx context.Context, provider, customerID string) (Record, bool, error) {
	if provider == "" || customerID == "" {
		return Record{}, false, nil
	}
	ptr, _, err := s.c.Get(ctx, s.customerKey(provider, customerID))
	if errors.Is(err, blob.ErrNotFound) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	r, _, ok, err := s.get(ctx, strings.TrimSpace(string(ptr)))
	if err != nil || !ok {
		return Record{}, false, err
	}
	if r.Provider != provider || r.CustomerID != customerID {
		return Record{}, false, nil // dangling or superseded index
	}
	return r, true, nil
}

// Put implements Store with the storage layer enforcing CAS: Version zero is
// a create-only PUT (If-None-Match: *), any other Version re-reads the object
// and PUTs with If-Match on its ETag — the window between that read and the
// PUT is closed by the condition itself, so a concurrent writer surfaces as
// ErrStale, never a lost update.
func (s *R2Store) Put(ctx context.Context, r Record) error {
	// The customer index is written first (create-only; an existing index for
	// the same pair is fine). See the type comment for the ordering rationale.
	if r.Provider != "" && r.CustomerID != "" {
		_, err := s.c.Put(ctx, s.customerKey(r.Provider, r.CustomerID), []byte(r.AccountID), blob.Cond{IfNoneMatchAny: true})
		if err != nil && !errors.Is(err, blob.ErrPrecondition) {
			return err
		}
	}

	if r.Version == 0 {
		next := r
		next.Version = 1
		data, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("r2store: encode record: %w", err)
		}
		_, err = s.c.Put(ctx, s.accountKey(r.AccountID), data, blob.Cond{IfNoneMatchAny: true})
		if errors.Is(err, blob.ErrPrecondition) {
			return ErrStale
		}
		return err
	}

	current, etag, ok, err := s.get(ctx, r.AccountID)
	if err != nil {
		return err
	}
	if !ok || current.Version != r.Version {
		return ErrStale
	}
	next := r
	next.Version = r.Version + 1
	data, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("r2store: encode record: %w", err)
	}
	_, err = s.c.Put(ctx, s.accountKey(r.AccountID), data, blob.Cond{IfMatch: etag})
	if errors.Is(err, blob.ErrPrecondition) {
		return ErrStale
	}
	return err
}

// List implements Store: every account record under the prefix. N+1 reads,
// fine at control-plane scale (accounts, not agents); Reconcile is the only
// caller and it sweeps periodically, not per-request.
func (s *R2Store) List(ctx context.Context) ([]Record, error) {
	keys, err := s.c.List(ctx, s.prefix+"accounts/")
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(keys))
	for _, k := range keys {
		data, _, err := s.c.Get(ctx, k)
		if errors.Is(err, blob.ErrNotFound) {
			continue // deleted between list and read
		}
		if err != nil {
			return nil, err
		}
		var r Record
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("r2store: decode record %s: %w", k, err)
		}
		out = append(out, r)
	}
	return out, nil
}
