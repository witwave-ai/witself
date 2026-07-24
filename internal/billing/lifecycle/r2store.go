package lifecycle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

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

// r2LimitAuditKindPrefix makes account-limit audit metadata survive a rollback
// to a binary whose Record/AdminChange structs predate LimitOverrides. Such a
// binary still knows and preserves AdminChange.Kind, ActorID, ActorHandle,
// Reason, and At while dropping unknown JSON fields. Encoding the new audit
// fields into Kind therefore lets a later Phase A binary reconstruct the exact
// override state by replaying history, without a second non-atomic R2 object.
const r2LimitAuditKindPrefix = "witself.limit-override.v1:"
const r2MessagingPolicyAuditKindPrefix = "witself.messaging-policy-override.v1:"

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
	}
	r.LimitOverrides = replayR2LimitOverrides(r.LimitOverrides, replay)
	r.MessagingOverride, r.MessageRetentionOverride =
		replayR2MessagingPolicyOverrides(
			r.MessagingOverride, r.MessageRetentionOverride, policyReplay)
	return r, nil
}
