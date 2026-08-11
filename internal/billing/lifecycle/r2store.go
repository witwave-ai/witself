package lifecycle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/blob"
	"github.com/witwave-ai/witself/internal/plans"
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
var _ EventReceiptStore = (*R2Store)(nil)

type r2PendingEventIndex struct {
	SchemaVersion int      `json:"schema_version"`
	Provider      string   `json:"provider"`
	CustomerID    string   `json:"customer_id"`
	EventIDs      []string `json:"event_ids"`
	Version       int64    `json:"version"`
}

// r2LimitAuditKindPrefix makes account-limit audit metadata survive a rollback
// to a binary whose Record/AdminChange structs predate LimitOverrides. Such a
// binary still knows and preserves AdminChange.Kind, ActorID, ActorHandle,
// Reason, and At while dropping unknown JSON fields. Encoding the new audit
// fields into Kind therefore lets a later Phase A binary reconstruct the exact
// override state by replaying history, without a second non-atomic R2 object.
const r2LimitAuditKindPrefix = "witself.limit-override.v1:"
const r2MessagingPolicyAuditKindPrefix = "witself.messaging-policy-override.v1:"

// This deliberately uses a new prefix instead of extending the messaging
// envelope: v0.0.210 understands and safely preserves an unknown Kind string,
// so a rollback cannot erase email override state from the audit log.
const r2AgentEmailPolicyAuditKindPrefix = "witself.agent-email-policy-override.v1:"

type r2LimitAuditEnvelope struct {
	Kind       string             `json:"kind"`
	Dimension  string             `json:"dimension"`
	From       *AccountLimitValue `json:"from"`
	To         *AccountLimitValue `json:"to"`
	FromSource string             `json:"from_source"`
	ToSource   string             `json:"to_source"`
}

// r2MessagingPolicyAuditEnvelope preserves messaging override state across a
// rollback to a binary whose Record predates the fields. Older binaries retain
// the encoded Kind plus common audit attribution, allowing a newer binary to
// replay the exact override without a second non-atomic R2 object.
type r2MessagingPolicyAuditEnvelope struct {
	Kind          string `json:"kind"`
	MessagingFrom *bool  `json:"messaging_from,omitempty"`
	MessagingTo   *bool  `json:"messaging_to,omitempty"`
	RetentionFrom *int64 `json:"retention_from,omitempty"`
	RetentionTo   *int64 `json:"retention_to,omitempty"`
	FromSource    string `json:"from_source"`
	ToSource      string `json:"to_source"`
}

// r2AgentEmailPolicyAuditEnvelope preserves inbound-email override state
// across a rollback to a binary whose Record predates these fields.
type r2AgentEmailPolicyAuditEnvelope struct {
	Kind          string `json:"kind"`
	ReceiveFrom   *bool  `json:"receive_from,omitempty"`
	ReceiveTo     *bool  `json:"receive_to,omitempty"`
	RetentionFrom *int64 `json:"retention_from,omitempty"`
	RetentionTo   *int64 `json:"retention_to,omitempty"`
	FromSource    string `json:"from_source"`
	ToSource      string `json:"to_source"`
}

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

func (s *R2Store) eventReceiptKey(provider, eventID string) string {
	return s.prefix + "billing-events/receipts/" +
		eventReceiptIdentityKey(provider, eventID) + ".json"
}

func (s *R2Store) pendingEventIndexKey(provider, customerID string) string {
	return s.prefix + "billing-events/pending/" +
		eventReceiptCustomerKey(provider, customerID) + ".json"
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
	r, err := unmarshalR2Record(data)
	if err != nil {
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
		data, err := marshalR2Record(next)
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
	data, err := marshalR2Record(next)
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
		r, err := unmarshalR2Record(data)
		if err != nil {
			return nil, fmt.Errorf("r2store: decode record %s: %w", k, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// ReceiveEvent implements EventReceiptStore. The receipt is create-only by
// provider event identity; its separate per-customer pending index is a bounded
// CAS object used by ReconcileAccount. A crash after receipt creation but before
// indexing returns no acknowledgement, so provider redelivery repairs the
// missing index without duplicating the receipt.
func (s *R2Store) ReceiveEvent(
	ctx context.Context,
	receipt EventReceipt,
) (EventReceipt, bool, error) {
	if err := validateEventReceipt(receipt); err != nil {
		return EventReceipt{}, false, err
	}
	if receipt.Version != 0 || receipt.Status != EventReceiptPending {
		return EventReceipt{}, false, errors.New(
			"event receipt: new receipt must be pending at version zero")
	}
	key := s.eventReceiptKey(
		receipt.Provider, receipt.Event.ProviderEventID)
	created := false
	stored := receipt
	stored.Version = 1
	data, err := json.Marshal(stored)
	if err != nil {
		return EventReceipt{}, false, fmt.Errorf("event receipt: encode: %w", err)
	}
	if _, err = s.c.Put(ctx, key, data, blob.Cond{IfNoneMatchAny: true}); err == nil {
		created = true
	} else if errors.Is(err, blob.ErrPrecondition) {
		data, _, err = s.c.Get(ctx, key)
		if err != nil {
			return EventReceipt{}, false, err
		}
		if err := json.Unmarshal(data, &stored); err != nil {
			return EventReceipt{}, false, fmt.Errorf(
				"event receipt: decode existing: %w", err)
		}
		if err := validateEventReceipt(stored); err != nil {
			return EventReceipt{}, false, fmt.Errorf(
				"event receipt: invalid existing receipt: %w", err)
		}
		if !sameEventReceiptIdentity(stored, receipt) {
			return EventReceipt{}, false, fmt.Errorf(
				"%w: provider=%s event=%s",
				ErrEventReceiptConflict, receipt.Provider,
				receipt.Event.ProviderEventID)
		}
	} else {
		return EventReceipt{}, false, err
	}
	if stored.Status == EventReceiptPending {
		if err := s.ensurePendingEventIndex(ctx, stored); err != nil {
			if errors.Is(err, errEventReceiptPendingIndexFull) {
				// The receipt object is already durable. Index saturation must
				// not prevent this webhook from processing it synchronously, and
				// redelivery can retry the same object even while the bounded
				// reconciliation index remains full.
				return cloneEventReceipt(stored), created, nil
			}
			return EventReceipt{}, false, err
		}
	}
	return cloneEventReceipt(stored), created, nil
}

// ClaimEvent implements EventReceiptStore with a receipt-object CAS. A live
// claim is exclusive; an expired claim advances ClaimGeneration before a new
// worker may fold, fencing every completion/release from the former owner.
func (s *R2Store) ClaimEvent(
	ctx context.Context,
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
	key := s.eventReceiptKey(
		receipt.Provider, receipt.Event.ProviderEventID)
	for range casAttempts {
		data, etag, err := s.c.Get(ctx, key)
		if err != nil {
			return EventReceipt{}, false, err
		}
		var current EventReceipt
		if err := json.Unmarshal(data, &current); err != nil {
			return EventReceipt{}, false, fmt.Errorf(
				"event receipt: decode for claim: %w", err)
		}
		if err := validateEventReceipt(current); err != nil {
			return EventReceipt{}, false, fmt.Errorf(
				"event receipt: invalid receipt for claim: %w", err)
		}
		if !sameEventReceiptIdentity(current, receipt) {
			return EventReceipt{}, false, ErrEventReceiptConflict
		}
		if current.Status == EventReceiptProcessed {
			return cloneEventReceipt(current), false, nil
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
		encoded, err := json.Marshal(current)
		if err != nil {
			return EventReceipt{}, false, fmt.Errorf(
				"event receipt: encode claim: %w", err)
		}
		_, err = s.c.Put(ctx, key, encoded, blob.Cond{IfMatch: etag})
		if errors.Is(err, blob.ErrPrecondition) {
			continue
		}
		if err != nil {
			return EventReceipt{}, false, err
		}
		return cloneEventReceipt(current), true, nil
	}
	return EventReceipt{}, false, errors.New(
		"event receipt: processing claim has too much contention")
}

// PinEventResolution stores one immutable routing result under the exact claim
// fence. An expired-lease successor can reuse it, but neither worker can change
// the account, event, or decision selected before the first fold.
func (s *R2Store) PinEventResolution(
	ctx context.Context,
	receipt EventReceipt,
	resolution EventReceiptResolution,
) (EventReceipt, error) {
	if err := validateEventReceipt(receipt); err != nil {
		return EventReceipt{}, err
	}
	if err := validateEventReceiptResolution(receipt.Event, resolution); err != nil {
		return EventReceipt{}, err
	}
	key := s.eventReceiptKey(
		receipt.Provider, receipt.Event.ProviderEventID)
	for range casAttempts {
		data, etag, err := s.c.Get(ctx, key)
		if err != nil {
			return EventReceipt{}, err
		}
		var current EventReceipt
		if err := json.Unmarshal(data, &current); err != nil {
			return EventReceipt{}, fmt.Errorf(
				"event receipt: decode for resolution pin: %w", err)
		}
		if err := validateEventReceipt(current); err != nil {
			return EventReceipt{}, fmt.Errorf(
				"event receipt: invalid receipt for resolution pin: %w", err)
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
		encoded, err := json.Marshal(current)
		if err != nil {
			return EventReceipt{}, fmt.Errorf(
				"event receipt: encode resolution pin: %w", err)
		}
		_, err = s.c.Put(ctx, key, encoded, blob.Cond{IfMatch: etag})
		if errors.Is(err, blob.ErrPrecondition) {
			continue
		}
		if err != nil {
			return EventReceipt{}, err
		}
		return cloneEventReceipt(current), nil
	}
	return EventReceipt{}, errors.New(
		"event receipt: resolution pin has too much contention")
}

// ReleaseEvent relinquishes one exact receipt claim while keeping its pending
// index entry available for immediate redelivery or reconciliation.
func (s *R2Store) ReleaseEvent(
	ctx context.Context,
	receipt EventReceipt,
) error {
	if err := validateEventReceipt(receipt); err != nil {
		return err
	}
	if receipt.ClaimToken == "" || receipt.ClaimGeneration < 1 {
		return errors.New("event receipt: release requires a processing claim")
	}
	key := s.eventReceiptKey(
		receipt.Provider, receipt.Event.ProviderEventID)
	for range casAttempts {
		data, etag, err := s.c.Get(ctx, key)
		if err != nil {
			return err
		}
		var current EventReceipt
		if err := json.Unmarshal(data, &current); err != nil {
			return fmt.Errorf("event receipt: decode for release: %w", err)
		}
		if err := validateEventReceipt(current); err != nil {
			return fmt.Errorf("event receipt: invalid receipt for release: %w", err)
		}
		if !sameEventReceiptIdentity(current, receipt) {
			return ErrEventReceiptConflict
		}
		if current.Status == EventReceiptProcessed {
			return nil
		}
		if current.ClaimToken == "" &&
			current.ClaimGeneration == receipt.ClaimGeneration {
			return s.ensurePendingEventIndex(ctx, current)
		}
		if !eventReceiptClaimMatches(current, receipt) {
			return ErrEventReceiptClaimLost
		}
		current.ClaimToken = ""
		current.LeaseExpiresAt = nil
		current.Version++
		encoded, err := json.Marshal(current)
		if err != nil {
			return fmt.Errorf("event receipt: encode release: %w", err)
		}
		_, err = s.c.Put(ctx, key, encoded, blob.Cond{IfMatch: etag})
		if errors.Is(err, blob.ErrPrecondition) {
			continue
		}
		if err != nil {
			return err
		}
		return s.ensurePendingEventIndex(ctx, current)
	}
	return errors.New("event receipt: processing release has too much contention")
}

func (s *R2Store) ensurePendingEventIndex(
	ctx context.Context,
	receipt EventReceipt,
) error {
	key := s.pendingEventIndexKey(
		receipt.Provider, receipt.Event.CustomerID)
	for range casAttempts {
		data, etag, err := s.c.Get(ctx, key)
		if errors.Is(err, blob.ErrNotFound) {
			index := r2PendingEventIndex{
				SchemaVersion: pendingEventIndexSchemaVersion,
				Provider:      receipt.Provider,
				CustomerID:    receipt.Event.CustomerID,
				EventIDs:      []string{receipt.Event.ProviderEventID},
				Version:       1,
			}
			encoded, encodeErr := json.Marshal(index)
			if encodeErr != nil {
				return fmt.Errorf("event receipt: encode pending index: %w", encodeErr)
			}
			_, err = s.c.Put(ctx, key, encoded, blob.Cond{IfNoneMatchAny: true})
			if errors.Is(err, blob.ErrPrecondition) {
				continue
			}
			return err
		}
		if err != nil {
			return err
		}
		index, err := decodePendingEventIndex(
			data, receipt.Provider, receipt.Event.CustomerID)
		if err != nil {
			return err
		}
		for _, eventID := range index.EventIDs {
			if eventID == receipt.Event.ProviderEventID {
				return nil
			}
		}
		if len(index.EventIDs) >= maxPendingEventReceiptsPerCustomer {
			return errEventReceiptPendingIndexFull
		}
		index.EventIDs = append(index.EventIDs, receipt.Event.ProviderEventID)
		index.Version++
		encoded, err := json.Marshal(index)
		if err != nil {
			return fmt.Errorf("event receipt: encode pending index: %w", err)
		}
		_, err = s.c.Put(ctx, key, encoded, blob.Cond{IfMatch: etag})
		if errors.Is(err, blob.ErrPrecondition) {
			continue
		}
		return err
	}
	return errors.New("event receipt: pending index has too much contention")
}

func decodePendingEventIndex(
	data []byte,
	provider, customerID string,
) (r2PendingEventIndex, error) {
	var index r2PendingEventIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return r2PendingEventIndex{}, fmt.Errorf(
			"event receipt: decode pending index: %w", err)
	}
	if index.SchemaVersion != pendingEventIndexSchemaVersion ||
		index.Provider != provider || index.CustomerID != customerID ||
		index.Version < 1 ||
		len(index.EventIDs) > maxPendingEventReceiptsPerCustomer {
		return r2PendingEventIndex{}, errors.New(
			"event receipt: invalid pending index")
	}
	seen := make(map[string]struct{}, len(index.EventIDs))
	for _, eventID := range index.EventIDs {
		if eventID == "" || len(eventID) > 255 {
			return r2PendingEventIndex{}, errors.New(
				"event receipt: invalid event id in pending index")
		}
		if _, duplicate := seen[eventID]; duplicate {
			return r2PendingEventIndex{}, errors.New(
				"event receipt: duplicate event id in pending index")
		}
		seen[eventID] = struct{}{}
	}
	return index, nil
}

// CompleteEvent implements EventReceiptStore. Receipt completion precedes
// pending-index removal. If the second write fails, retry sees the processed
// receipt and idempotently finishes only the index cleanup.
func (s *R2Store) CompleteEvent(
	ctx context.Context,
	receipt EventReceipt,
	processedAt time.Time,
) error {
	if err := validateEventReceipt(receipt); err != nil {
		return err
	}
	if processedAt.IsZero() {
		return errors.New("event receipt: processed_at is required")
	}
	key := s.eventReceiptKey(
		receipt.Provider, receipt.Event.ProviderEventID)
	var current EventReceipt
	for range casAttempts {
		data, etag, err := s.c.Get(ctx, key)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &current); err != nil {
			return fmt.Errorf("event receipt: decode for completion: %w", err)
		}
		if err := validateEventReceipt(current); err != nil {
			return fmt.Errorf("event receipt: invalid receipt for completion: %w", err)
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
			break
		}
		if !eventReceiptClaimMatches(current, receipt) {
			return ErrEventReceiptClaimLost
		}
		if current.Resolution == nil || receipt.Resolution == nil ||
			!sameEventReceiptResolution(*current.Resolution, *receipt.Resolution) {
			return fmt.Errorf(
				"%w: completion requires the pinned resolution",
				ErrEventReceiptConflict)
		}
		at := processedAt.UTC()
		current.Status = EventReceiptProcessed
		current.Decision = current.Resolution.Decision
		current.ProcessedAt = &at
		current.ClaimToken = ""
		current.LeaseExpiresAt = nil
		current.Version++
		encoded, err := json.Marshal(current)
		if err != nil {
			return fmt.Errorf("event receipt: encode completion: %w", err)
		}
		_, err = s.c.Put(ctx, key, encoded, blob.Cond{IfMatch: etag})
		if errors.Is(err, blob.ErrPrecondition) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	if current.Status != EventReceiptProcessed {
		return errors.New("event receipt: completion has too much contention")
	}
	return s.removePendingEventIndex(ctx, current)
}

func (s *R2Store) removePendingEventIndex(
	ctx context.Context,
	receipt EventReceipt,
) error {
	key := s.pendingEventIndexKey(
		receipt.Provider, receipt.Event.CustomerID)
	for range casAttempts {
		data, etag, err := s.c.Get(ctx, key)
		if errors.Is(err, blob.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		index, err := decodePendingEventIndex(
			data, receipt.Provider, receipt.Event.CustomerID)
		if err != nil {
			return err
		}
		kept := index.EventIDs[:0]
		found := false
		for _, eventID := range index.EventIDs {
			if eventID == receipt.Event.ProviderEventID {
				found = true
				continue
			}
			kept = append(kept, eventID)
		}
		if !found {
			return nil
		}
		index.EventIDs = kept
		index.Version++
		encoded, err := json.Marshal(index)
		if err != nil {
			return fmt.Errorf("event receipt: encode pending cleanup: %w", err)
		}
		_, err = s.c.Put(ctx, key, encoded, blob.Cond{IfMatch: etag})
		if errors.Is(err, blob.ErrPrecondition) {
			continue
		}
		return err
	}
	return errors.New("event receipt: pending cleanup has too much contention")
}

// PendingEvents implements EventReceiptStore. It reads one bounded index and
// at most its hard maximum of receipt objects; it never performs a bucket-wide
// prefix list.
func (s *R2Store) PendingEvents(
	ctx context.Context,
	provider, customerID string,
	limit int,
) ([]EventReceipt, error) {
	if limit < 1 || limit > maxPendingEventReceiptsPerCustomer {
		return nil, fmt.Errorf("event receipt: pending limit must be 1-%d",
			maxPendingEventReceiptsPerCustomer)
	}
	key := s.pendingEventIndexKey(provider, customerID)
	data, _, err := s.c.Get(ctx, key)
	if errors.Is(err, blob.ErrNotFound) {
		return []EventReceipt{}, nil
	}
	if err != nil {
		return nil, err
	}
	index, err := decodePendingEventIndex(data, provider, customerID)
	if err != nil {
		return nil, err
	}
	out := make([]EventReceipt, 0, min(limit, len(index.EventIDs)))
	var pendingErr error
	for _, eventID := range index.EventIDs {
		data, _, err := s.c.Get(ctx, s.eventReceiptKey(provider, eventID))
		if err != nil {
			pendingErr = errors.Join(pendingErr, fmt.Errorf(
				"event receipt: pending receipt %s unavailable: %w", eventID, err))
			continue
		}
		var receipt EventReceipt
		if err := json.Unmarshal(data, &receipt); err != nil {
			pendingErr = errors.Join(pendingErr, fmt.Errorf(
				"event receipt: decode pending receipt %s: %w", eventID, err))
			continue
		}
		if err := validateEventReceipt(receipt); err != nil {
			pendingErr = errors.Join(pendingErr, fmt.Errorf(
				"event receipt: invalid pending receipt %s: %w", eventID, err))
			continue
		}
		if receipt.Provider != provider || receipt.Event.CustomerID != customerID ||
			receipt.Event.ProviderEventID != eventID {
			pendingErr = errors.Join(pendingErr, fmt.Errorf(
				"event receipt: pending index points at mismatched receipt %s", eventID))
			continue
		}
		out = append(out, cloneEventReceipt(receipt))
		if len(out) == limit {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Event.At.Equal(out[j].Event.At) {
			return out[i].Event.ProviderEventID < out[j].Event.ProviderEventID
		}
		return out[i].Event.At.Before(out[j].Event.At)
	})
	return out, pendingErr
}

func cloneAccountLimitValue(value *AccountLimitValue) *AccountLimitValue {
	if value == nil {
		return nil
	}
	out := &AccountLimitValue{}
	if value.Max != nil {
		maxValue := *value.Max
		out.Max = &maxValue
	}
	return out
}

func normalizeR2LimitAudit(change AdminChange) (r2LimitAuditEnvelope, error) {
	if change.Kind != "limit_override_set" &&
		change.Kind != "limit_override_cleared" {
		return r2LimitAuditEnvelope{}, fmt.Errorf("unsupported normalized kind %q", change.Kind)
	}
	dimension, err := validateAccountLimit(change.LimitDimension, nil)
	if err != nil {
		return r2LimitAuditEnvelope{}, fmt.Errorf("invalid dimension: %w", err)
	}
	if dimension != change.LimitDimension {
		return r2LimitAuditEnvelope{}, errors.New("dimension is not normalized")
	}
	if change.LimitFrom == nil || change.LimitTo == nil {
		return r2LimitAuditEnvelope{}, errors.New("from and to value wrappers are required")
	}
	if _, err := validateAccountLimit(dimension, change.LimitFrom.Max); err != nil {
		return r2LimitAuditEnvelope{}, fmt.Errorf("invalid from value: %w", err)
	}
	if _, err := validateAccountLimit(dimension, change.LimitTo.Max); err != nil {
		return r2LimitAuditEnvelope{}, fmt.Errorf("invalid to value: %w", err)
	}
	switch change.Kind {
	case "limit_override_set":
		if change.LimitToSource != "override" ||
			(change.LimitFromSource != "inherited" &&
				change.LimitFromSource != "override") {
			return r2LimitAuditEnvelope{}, errors.New("invalid set sources")
		}
	case "limit_override_cleared":
		if change.LimitFromSource != "override" ||
			change.LimitToSource != "inherited" {
			return r2LimitAuditEnvelope{}, errors.New("invalid clear sources")
		}
	}
	return r2LimitAuditEnvelope{
		Kind:       change.Kind,
		Dimension:  dimension,
		From:       cloneAccountLimitValue(change.LimitFrom),
		To:         cloneAccountLimitValue(change.LimitTo),
		FromSource: change.LimitFromSource,
		ToSource:   change.LimitToSource,
	}, nil
}

func encodeR2LimitAuditKind(change AdminChange) (string, error) {
	envelope, err := normalizeR2LimitAudit(change)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return r2LimitAuditKindPrefix +
		base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeR2LimitAuditKind(kind string) (r2LimitAuditEnvelope, error) {
	encoded := strings.TrimPrefix(kind, r2LimitAuditKindPrefix)
	if encoded == kind || encoded == "" {
		return r2LimitAuditEnvelope{}, errors.New("missing limit-audit envelope")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return r2LimitAuditEnvelope{}, fmt.Errorf("decode base64url: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var envelope r2LimitAuditEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return r2LimitAuditEnvelope{}, fmt.Errorf("decode JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return r2LimitAuditEnvelope{}, errors.New("multiple JSON values")
		}
		return r2LimitAuditEnvelope{}, fmt.Errorf("decode trailing JSON: %w", err)
	}
	change := AdminChange{
		Kind:            envelope.Kind,
		LimitDimension:  envelope.Dimension,
		LimitFrom:       envelope.From,
		LimitTo:         envelope.To,
		LimitFromSource: envelope.FromSource,
		LimitToSource:   envelope.ToSource,
	}
	normalized, err := normalizeR2LimitAudit(change)
	if err != nil {
		return r2LimitAuditEnvelope{}, err
	}
	return normalized, nil
}

func restoreR2LimitAudit(change *AdminChange, envelope r2LimitAuditEnvelope) {
	change.Kind = envelope.Kind
	change.LimitDimension = envelope.Dimension
	change.LimitFrom = cloneAccountLimitValue(envelope.From)
	change.LimitTo = cloneAccountLimitValue(envelope.To)
	change.LimitFromSource = envelope.FromSource
	change.LimitToSource = envelope.ToSource
}

func replayR2LimitOverrides(
	current map[string]AccountLimitOverride,
	changes []AdminChange,
) map[string]AccountLimitOverride {
	var overrides map[string]AccountLimitOverride
	if current != nil {
		overrides = make(map[string]AccountLimitOverride, len(current))
		for dimension, override := range current {
			if override.Max != nil {
				maxValue := *override.Max
				override.Max = &maxValue
			}
			overrides[dimension] = override
		}
	}
	for _, change := range changes {
		switch change.Kind {
		case "limit_override_set":
			if overrides == nil {
				overrides = map[string]AccountLimitOverride{}
			}
			override := AccountLimitOverride{
				ActorID: change.ActorID, ActorHandle: change.ActorHandle,
				Reason: change.Reason, SetAt: change.At,
			}
			if change.LimitTo.Max != nil {
				maxValue := *change.LimitTo.Max
				override.Max = &maxValue
			}
			overrides[change.LimitDimension] = override
		case "limit_override_cleared":
			delete(overrides, change.LimitDimension)
		}
	}
	if len(overrides) == 0 {
		return nil
	}
	return overrides
}

func normalizeR2MessagingPolicyAudit(
	change AdminChange,
) (r2MessagingPolicyAuditEnvelope, error) {
	switch change.Kind {
	case "messaging_override_set":
		if change.MessagingFrom == nil || change.MessagingTo == nil ||
			change.MessagingToSource != "override" ||
			(change.MessagingFromSource != "inherited" &&
				change.MessagingFromSource != "override") {
			return r2MessagingPolicyAuditEnvelope{}, errors.New("invalid messaging set audit")
		}
		return r2MessagingPolicyAuditEnvelope{
			Kind:          change.Kind,
			MessagingFrom: change.MessagingFrom,
			MessagingTo:   change.MessagingTo,
			FromSource:    change.MessagingFromSource,
			ToSource:      change.MessagingToSource,
		}, nil
	case "messaging_override_cleared":
		if change.MessagingFrom == nil || change.MessagingTo == nil ||
			change.MessagingFromSource != "override" ||
			change.MessagingToSource != "inherited" {
			return r2MessagingPolicyAuditEnvelope{}, errors.New("invalid messaging clear audit")
		}
		return r2MessagingPolicyAuditEnvelope{
			Kind:          change.Kind,
			MessagingFrom: change.MessagingFrom,
			MessagingTo:   change.MessagingTo,
			FromSource:    change.MessagingFromSource,
			ToSource:      change.MessagingToSource,
		}, nil
	case "message_retention_override_set":
		if change.MessageRetentionToSource != "override" ||
			(change.MessageRetentionFromSource != "inherited" &&
				change.MessageRetentionFromSource != "override") {
			return r2MessagingPolicyAuditEnvelope{}, errors.New("invalid message retention set audit")
		}
		if err := validateOptionalMessageRetention(change.MessageRetentionFrom); err != nil {
			return r2MessagingPolicyAuditEnvelope{}, fmt.Errorf("invalid from retention: %w", err)
		}
		if err := validateOptionalMessageRetention(change.MessageRetentionTo); err != nil {
			return r2MessagingPolicyAuditEnvelope{}, fmt.Errorf("invalid to retention: %w", err)
		}
		return r2MessagingPolicyAuditEnvelope{
			Kind:          change.Kind,
			RetentionFrom: change.MessageRetentionFrom,
			RetentionTo:   change.MessageRetentionTo,
			FromSource:    change.MessageRetentionFromSource,
			ToSource:      change.MessageRetentionToSource,
		}, nil
	case "message_retention_override_cleared":
		if change.MessageRetentionFromSource != "override" ||
			change.MessageRetentionToSource != "inherited" {
			return r2MessagingPolicyAuditEnvelope{}, errors.New("invalid message retention clear audit")
		}
		if err := validateOptionalMessageRetention(change.MessageRetentionFrom); err != nil {
			return r2MessagingPolicyAuditEnvelope{}, fmt.Errorf("invalid from retention: %w", err)
		}
		if err := validateOptionalMessageRetention(change.MessageRetentionTo); err != nil {
			return r2MessagingPolicyAuditEnvelope{}, fmt.Errorf("invalid to retention: %w", err)
		}
		return r2MessagingPolicyAuditEnvelope{
			Kind:          change.Kind,
			RetentionFrom: change.MessageRetentionFrom,
			RetentionTo:   change.MessageRetentionTo,
			FromSource:    change.MessageRetentionFromSource,
			ToSource:      change.MessageRetentionToSource,
		}, nil
	default:
		return r2MessagingPolicyAuditEnvelope{}, fmt.Errorf(
			"unsupported normalized kind %q", change.Kind)
	}
}

func validateOptionalMessageRetention(days *int64) error {
	if days == nil {
		return nil
	}
	return plans.ValidatePolicies(map[string]int64{
		plans.MessageRetentionDaysPolicy: *days,
	})
}

func encodeR2MessagingPolicyAuditKind(change AdminChange) (string, error) {
	envelope, err := normalizeR2MessagingPolicyAudit(change)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return r2MessagingPolicyAuditKindPrefix +
		base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeR2MessagingPolicyAuditKind(
	kind string,
) (r2MessagingPolicyAuditEnvelope, error) {
	encoded := strings.TrimPrefix(kind, r2MessagingPolicyAuditKindPrefix)
	if encoded == kind || encoded == "" {
		return r2MessagingPolicyAuditEnvelope{}, errors.New(
			"missing messaging-policy audit envelope")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return r2MessagingPolicyAuditEnvelope{}, fmt.Errorf("decode base64url: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var envelope r2MessagingPolicyAuditEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return r2MessagingPolicyAuditEnvelope{}, fmt.Errorf("decode JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return r2MessagingPolicyAuditEnvelope{}, errors.New("multiple JSON values")
		}
		return r2MessagingPolicyAuditEnvelope{}, fmt.Errorf("decode trailing JSON: %w", err)
	}
	change := AdminChange{Kind: envelope.Kind}
	switch envelope.Kind {
	case "messaging_override_set", "messaging_override_cleared":
		change.MessagingFrom = envelope.MessagingFrom
		change.MessagingTo = envelope.MessagingTo
		change.MessagingFromSource = envelope.FromSource
		change.MessagingToSource = envelope.ToSource
	case "message_retention_override_set", "message_retention_override_cleared":
		change.MessageRetentionFrom = envelope.RetentionFrom
		change.MessageRetentionTo = envelope.RetentionTo
		change.MessageRetentionFromSource = envelope.FromSource
		change.MessageRetentionToSource = envelope.ToSource
	}
	return normalizeR2MessagingPolicyAudit(change)
}

func restoreR2MessagingPolicyAudit(
	change *AdminChange,
	envelope r2MessagingPolicyAuditEnvelope,
) {
	change.Kind = envelope.Kind
	switch envelope.Kind {
	case "messaging_override_set", "messaging_override_cleared":
		change.MessagingFrom = envelope.MessagingFrom
		change.MessagingTo = envelope.MessagingTo
		change.MessagingFromSource = envelope.FromSource
		change.MessagingToSource = envelope.ToSource
	case "message_retention_override_set", "message_retention_override_cleared":
		change.MessageRetentionFrom = envelope.RetentionFrom
		change.MessageRetentionTo = envelope.RetentionTo
		change.MessageRetentionFromSource = envelope.FromSource
		change.MessageRetentionToSource = envelope.ToSource
	}
}

func replayR2MessagingPolicyOverrides(
	currentMessaging *MessagingOverride,
	currentRetention *MessageRetentionOverride,
	changes []AdminChange,
) (*MessagingOverride, *MessageRetentionOverride) {
	var messaging *MessagingOverride
	if currentMessaging != nil {
		value := *currentMessaging
		messaging = &value
	}
	var retention *MessageRetentionOverride
	if currentRetention != nil {
		value := *currentRetention
		if value.Days != nil {
			days := *value.Days
			value.Days = &days
		}
		retention = &value
	}
	for _, change := range changes {
		switch change.Kind {
		case "messaging_override_set":
			messaging = &MessagingOverride{
				Enabled: *change.MessagingTo,
				ActorID: change.ActorID, ActorHandle: change.ActorHandle,
				Reason: change.Reason, SetAt: change.At,
			}
		case "messaging_override_cleared":
			messaging = nil
		case "message_retention_override_set":
			retention = &MessageRetentionOverride{
				ActorID: change.ActorID, ActorHandle: change.ActorHandle,
				Reason: change.Reason, SetAt: change.At,
			}
			if change.MessageRetentionTo != nil {
				days := *change.MessageRetentionTo
				retention.Days = &days
			}
		case "message_retention_override_cleared":
			retention = nil
		}
	}
	return messaging, retention
}

func normalizeR2AgentEmailPolicyAudit(
	change AdminChange,
) (r2AgentEmailPolicyAuditEnvelope, error) {
	switch change.Kind {
	case "agent_email_receive_override_set":
		if change.AgentEmailReceiveFrom == nil ||
			change.AgentEmailReceiveTo == nil ||
			change.AgentEmailReceiveToSource != "override" ||
			(change.AgentEmailReceiveFromSource != "inherited" &&
				change.AgentEmailReceiveFromSource != "override") {
			return r2AgentEmailPolicyAuditEnvelope{}, errors.New(
				"invalid agent email receive set audit")
		}
		return r2AgentEmailPolicyAuditEnvelope{
			Kind: change.Kind, ReceiveFrom: change.AgentEmailReceiveFrom,
			ReceiveTo:  change.AgentEmailReceiveTo,
			FromSource: change.AgentEmailReceiveFromSource,
			ToSource:   change.AgentEmailReceiveToSource,
		}, nil
	case "agent_email_receive_override_cleared":
		if change.AgentEmailReceiveFrom == nil ||
			change.AgentEmailReceiveTo == nil ||
			change.AgentEmailReceiveFromSource != "override" ||
			change.AgentEmailReceiveToSource != "inherited" {
			return r2AgentEmailPolicyAuditEnvelope{}, errors.New(
				"invalid agent email receive clear audit")
		}
		return r2AgentEmailPolicyAuditEnvelope{
			Kind: change.Kind, ReceiveFrom: change.AgentEmailReceiveFrom,
			ReceiveTo:  change.AgentEmailReceiveTo,
			FromSource: change.AgentEmailReceiveFromSource,
			ToSource:   change.AgentEmailReceiveToSource,
		}, nil
	case "agent_email_retention_override_set":
		if change.AgentEmailRetentionToSource != "override" ||
			(change.AgentEmailRetentionFromSource != "inherited" &&
				change.AgentEmailRetentionFromSource != "override") {
			return r2AgentEmailPolicyAuditEnvelope{}, errors.New(
				"invalid agent email retention set audit")
		}
		if err := validateOptionalAgentEmailRetention(
			change.AgentEmailRetentionFrom); err != nil {
			return r2AgentEmailPolicyAuditEnvelope{}, fmt.Errorf(
				"invalid from retention: %w", err)
		}
		if err := validateOptionalAgentEmailRetention(
			change.AgentEmailRetentionTo); err != nil {
			return r2AgentEmailPolicyAuditEnvelope{}, fmt.Errorf(
				"invalid to retention: %w", err)
		}
		return r2AgentEmailPolicyAuditEnvelope{
			Kind:          change.Kind,
			RetentionFrom: change.AgentEmailRetentionFrom,
			RetentionTo:   change.AgentEmailRetentionTo,
			FromSource:    change.AgentEmailRetentionFromSource,
			ToSource:      change.AgentEmailRetentionToSource,
		}, nil
	case "agent_email_retention_override_cleared":
		if change.AgentEmailRetentionFromSource != "override" ||
			change.AgentEmailRetentionToSource != "inherited" {
			return r2AgentEmailPolicyAuditEnvelope{}, errors.New(
				"invalid agent email retention clear audit")
		}
		if err := validateOptionalAgentEmailRetention(
			change.AgentEmailRetentionFrom); err != nil {
			return r2AgentEmailPolicyAuditEnvelope{}, fmt.Errorf(
				"invalid from retention: %w", err)
		}
		if err := validateOptionalAgentEmailRetention(
			change.AgentEmailRetentionTo); err != nil {
			return r2AgentEmailPolicyAuditEnvelope{}, fmt.Errorf(
				"invalid to retention: %w", err)
		}
		return r2AgentEmailPolicyAuditEnvelope{
			Kind:          change.Kind,
			RetentionFrom: change.AgentEmailRetentionFrom,
			RetentionTo:   change.AgentEmailRetentionTo,
			FromSource:    change.AgentEmailRetentionFromSource,
			ToSource:      change.AgentEmailRetentionToSource,
		}, nil
	default:
		return r2AgentEmailPolicyAuditEnvelope{}, fmt.Errorf(
			"unsupported normalized kind %q", change.Kind)
	}
}

func validateOptionalAgentEmailRetention(days *int64) error {
	if days == nil {
		return nil
	}
	return plans.ValidatePolicies(map[string]int64{
		plans.AgentEmailRetentionDaysPolicy: *days,
	})
}

func encodeR2AgentEmailPolicyAuditKind(change AdminChange) (string, error) {
	envelope, err := normalizeR2AgentEmailPolicyAudit(change)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return r2AgentEmailPolicyAuditKindPrefix +
		base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeR2AgentEmailPolicyAuditKind(
	kind string,
) (r2AgentEmailPolicyAuditEnvelope, error) {
	encoded := strings.TrimPrefix(kind, r2AgentEmailPolicyAuditKindPrefix)
	if encoded == kind || encoded == "" {
		return r2AgentEmailPolicyAuditEnvelope{}, errors.New(
			"missing agent-email-policy audit envelope")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return r2AgentEmailPolicyAuditEnvelope{}, fmt.Errorf(
			"decode base64url: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var envelope r2AgentEmailPolicyAuditEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return r2AgentEmailPolicyAuditEnvelope{}, fmt.Errorf(
			"decode JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return r2AgentEmailPolicyAuditEnvelope{}, errors.New(
				"multiple JSON values")
		}
		return r2AgentEmailPolicyAuditEnvelope{}, fmt.Errorf(
			"decode trailing JSON: %w", err)
	}
	change := AdminChange{Kind: envelope.Kind}
	switch envelope.Kind {
	case "agent_email_receive_override_set",
		"agent_email_receive_override_cleared":
		change.AgentEmailReceiveFrom = envelope.ReceiveFrom
		change.AgentEmailReceiveTo = envelope.ReceiveTo
		change.AgentEmailReceiveFromSource = envelope.FromSource
		change.AgentEmailReceiveToSource = envelope.ToSource
	case "agent_email_retention_override_set",
		"agent_email_retention_override_cleared":
		change.AgentEmailRetentionFrom = envelope.RetentionFrom
		change.AgentEmailRetentionTo = envelope.RetentionTo
		change.AgentEmailRetentionFromSource = envelope.FromSource
		change.AgentEmailRetentionToSource = envelope.ToSource
	}
	return normalizeR2AgentEmailPolicyAudit(change)
}

func restoreR2AgentEmailPolicyAudit(
	change *AdminChange,
	envelope r2AgentEmailPolicyAuditEnvelope,
) {
	change.Kind = envelope.Kind
	switch envelope.Kind {
	case "agent_email_receive_override_set",
		"agent_email_receive_override_cleared":
		change.AgentEmailReceiveFrom = envelope.ReceiveFrom
		change.AgentEmailReceiveTo = envelope.ReceiveTo
		change.AgentEmailReceiveFromSource = envelope.FromSource
		change.AgentEmailReceiveToSource = envelope.ToSource
	case "agent_email_retention_override_set",
		"agent_email_retention_override_cleared":
		change.AgentEmailRetentionFrom = envelope.RetentionFrom
		change.AgentEmailRetentionTo = envelope.RetentionTo
		change.AgentEmailRetentionFromSource = envelope.FromSource
		change.AgentEmailRetentionToSource = envelope.ToSource
	}
}

func replayR2AgentEmailPolicyOverrides(
	currentReceive *AgentEmailReceiveOverride,
	currentRetention *AgentEmailRetentionOverride,
	changes []AdminChange,
) (*AgentEmailReceiveOverride, *AgentEmailRetentionOverride) {
	var receive *AgentEmailReceiveOverride
	if currentReceive != nil {
		value := *currentReceive
		receive = &value
	}
	var retention *AgentEmailRetentionOverride
	if currentRetention != nil {
		value := *currentRetention
		if value.Days != nil {
			days := *value.Days
			value.Days = &days
		}
		retention = &value
	}
	for _, change := range changes {
		switch change.Kind {
		case "agent_email_receive_override_set":
			receive = &AgentEmailReceiveOverride{
				Enabled: *change.AgentEmailReceiveTo,
				ActorID: change.ActorID, ActorHandle: change.ActorHandle,
				Reason: change.Reason, SetAt: change.At,
			}
		case "agent_email_receive_override_cleared":
			receive = nil
		case "agent_email_retention_override_set":
			retention = &AgentEmailRetentionOverride{
				ActorID: change.ActorID, ActorHandle: change.ActorHandle,
				Reason: change.Reason, SetAt: change.At,
			}
			if change.AgentEmailRetentionTo != nil {
				days := *change.AgentEmailRetentionTo
				retention.Days = &days
			}
		case "agent_email_retention_override_cleared":
			retention = nil
		}
	}
	return receive, retention
}

func marshalR2Record(r Record) ([]byte, error) {
	stored := clone(r)
	for i := range stored.AdminHistory {
		change := &stored.AdminHistory[i]
		if strings.HasPrefix(change.Kind, r2LimitAuditKindPrefix) {
			envelope, err := decodeR2LimitAuditKind(change.Kind)
			if err != nil {
				return nil, fmt.Errorf("admin history %d: malformed reserved kind: %w", i, err)
			}
			restoreR2LimitAudit(change, envelope)
		}
		if strings.HasPrefix(change.Kind, r2MessagingPolicyAuditKindPrefix) {
			envelope, err := decodeR2MessagingPolicyAuditKind(change.Kind)
			if err != nil {
				return nil, fmt.Errorf(
					"admin history %d: malformed reserved kind: %w", i, err)
			}
			restoreR2MessagingPolicyAudit(change, envelope)
		}
		if strings.HasPrefix(change.Kind, r2AgentEmailPolicyAuditKindPrefix) {
			envelope, err := decodeR2AgentEmailPolicyAuditKind(change.Kind)
			if err != nil {
				return nil, fmt.Errorf(
					"admin history %d: malformed reserved kind: %w", i, err)
			}
			restoreR2AgentEmailPolicyAudit(change, envelope)
		}
		switch change.Kind {
		case "messaging_override_set", "messaging_override_cleared",
			"message_retention_override_set", "message_retention_override_cleared":
			kind, err := encodeR2MessagingPolicyAuditKind(*change)
			if err != nil {
				return nil, fmt.Errorf(
					"admin history %d: encode messaging policy audit: %w", i, err)
			}
			change.Kind = kind
			continue
		}
		switch change.Kind {
		case "agent_email_receive_override_set",
			"agent_email_receive_override_cleared",
			"agent_email_retention_override_set",
			"agent_email_retention_override_cleared":
			kind, err := encodeR2AgentEmailPolicyAuditKind(*change)
			if err != nil {
				return nil, fmt.Errorf(
					"admin history %d: encode agent email policy audit: %w",
					i, err)
			}
			change.Kind = kind
			continue
		}
		if change.Kind != "limit_override_set" &&
			change.Kind != "limit_override_cleared" {
			continue
		}
		kind, err := encodeR2LimitAuditKind(*change)
		if err != nil {
			return nil, fmt.Errorf("admin history %d: encode limit audit: %w", i, err)
		}
		change.Kind = kind
	}
	return json.Marshal(stored)
}

func unmarshalR2Record(data []byte) (Record, error) {
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, err
	}
	replay := make([]AdminChange, 0)
	policyReplay := make([]AdminChange, 0)
	emailPolicyReplay := make([]AdminChange, 0)
	for i := range r.AdminHistory {
		change := &r.AdminHistory[i]
		if strings.HasPrefix(change.Kind, r2LimitAuditKindPrefix) {
			envelope, err := decodeR2LimitAuditKind(change.Kind)
			if err != nil {
				return Record{}, fmt.Errorf(
					"admin history %d: malformed reserved kind: %w", i, err)
			}
			restoreR2LimitAudit(change, envelope)
			replay = append(replay, *change)
			continue
		}
		if strings.HasPrefix(change.Kind, r2MessagingPolicyAuditKindPrefix) {
			envelope, err := decodeR2MessagingPolicyAuditKind(change.Kind)
			if err != nil {
				return Record{}, fmt.Errorf(
					"admin history %d: malformed reserved kind: %w", i, err)
			}
			restoreR2MessagingPolicyAudit(change, envelope)
			policyReplay = append(policyReplay, *change)
			continue
		}
		if strings.HasPrefix(change.Kind, r2AgentEmailPolicyAuditKindPrefix) {
			envelope, err := decodeR2AgentEmailPolicyAuditKind(change.Kind)
			if err != nil {
				return Record{}, fmt.Errorf(
					"admin history %d: malformed reserved kind: %w", i, err)
			}
			restoreR2AgentEmailPolicyAudit(change, envelope)
			emailPolicyReplay = append(emailPolicyReplay, *change)
			continue
		}
		// Before this envelope existed, development builds could write the
		// normal kind plus the new fields directly. Replay those when they are
		// complete, but leave unrelated/legacy history untouched.
		if change.Kind == "limit_override_set" ||
			change.Kind == "limit_override_cleared" {
			if _, err := normalizeR2LimitAudit(*change); err == nil {
				replay = append(replay, *change)
			}
		}
		switch change.Kind {
		case "messaging_override_set", "messaging_override_cleared",
			"message_retention_override_set", "message_retention_override_cleared":
			if _, err := normalizeR2MessagingPolicyAudit(*change); err == nil {
				policyReplay = append(policyReplay, *change)
			}
		}
		switch change.Kind {
		case "agent_email_receive_override_set",
			"agent_email_receive_override_cleared",
			"agent_email_retention_override_set",
			"agent_email_retention_override_cleared":
			if _, err := normalizeR2AgentEmailPolicyAudit(*change); err == nil {
				emailPolicyReplay = append(emailPolicyReplay, *change)
			}
		}
	}
	r.LimitOverrides = replayR2LimitOverrides(r.LimitOverrides, replay)
	r.MessagingOverride, r.MessageRetentionOverride =
		replayR2MessagingPolicyOverrides(
			r.MessagingOverride, r.MessageRetentionOverride, policyReplay)
	r.AgentEmailReceiveOverride, r.AgentEmailRetentionOverride =
		replayR2AgentEmailPolicyOverrides(
			r.AgentEmailReceiveOverride, r.AgentEmailRetentionOverride,
			emailPolicyReplay)
	return r, nil
}
