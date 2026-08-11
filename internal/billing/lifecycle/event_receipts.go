package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
)

const (
	eventReceiptSchemaVersion      = 2
	pendingEventIndexSchemaVersion = 1
	// maxPendingEventReceiptsPerCustomer is an intentional operational bound:
	// one broken customer cannot grow an unbounded retry index in the control
	// plane. Receipt objects are value-minimal and separate from this bounded
	// pending-work index.
	maxPendingEventReceiptsPerCustomer = 256
	maxEventReceiptsPerReconcile       = 64
	// eventReceiptProcessingLease bounds exclusive fold ownership. Normal
	// provider calls are themselves bounded well below this window; a crashed
	// owner can be replaced after it expires while a live owner uses the exact
	// claim token/generation as its completion fence.
	eventReceiptProcessingLease = 2 * time.Minute
)

// EventReceiptStatus is the durable processing state of a provider event.
type EventReceiptStatus string

const (
	// EventReceiptPending remains claimable until its pinned result is complete.
	EventReceiptPending EventReceiptStatus = "pending"
	// EventReceiptProcessed has a durable decision and needs no further fold.
	EventReceiptProcessed EventReceiptStatus = "processed"
)

// ErrEventReceiptConflict means one provider event identity was presented with
// different signed content. It is deliberately fail-closed: neither version is
// folded until an operator investigates the provider/security inconsistency.
var ErrEventReceiptConflict = errors.New("billing event receipt identity conflict")

// ErrEventReceiptInProgress means another worker holds the receipt's live
// processing lease. Webhook callers return a retryable failure rather than
// acknowledging work that has not yet completed; reconciliation tries again on
// its next pass.
var ErrEventReceiptInProgress = errors.New("billing event receipt is already being processed")

// ErrEventReceiptClaimLost means a worker tried to release or complete a claim
// after a newer generation acquired the receipt. The stale worker must never
// alter the newer owner's durable state.
var ErrEventReceiptClaimLost = errors.New("billing event receipt processing claim was lost")

// errEventReceiptPendingIndexFull is an internal backpressure signal. R2 may
// still synchronously process/redeliver the independently durable receipt when
// this bounded reconciliation accelerator is saturated.
var errEventReceiptPendingIndexFull = errors.New(
	"billing event receipt pending index is full for provider customer")

// EventReceipt is the durable, value-minimal record of one authenticated
// provider event. It stores only normalized allowlisted fields and a body hash;
// raw provider JSON, signatures, payment data, and customer content are never
// persisted here.
type EventReceipt struct {
	SchemaVersion int                `json:"schema_version"`
	Provider      string             `json:"provider"`
	Event         billing.Event      `json:"event"`
	Status        EventReceiptStatus `json:"status"`
	Decision      string             `json:"decision,omitempty"`
	ReceivedAt    time.Time          `json:"received_at"`
	ProcessedAt   *time.Time         `json:"processed_at,omitempty"`
	// ClaimToken and LeaseExpiresAt identify the one worker currently allowed
	// to fold this pending receipt. ClaimGeneration is monotonic and retained
	// after release/completion so an expired owner's fence can never recur.
	ClaimToken      string     `json:"claim_token,omitempty"`
	ClaimGeneration int64      `json:"claim_generation,omitempty"`
	LeaseExpiresAt  *time.Time `json:"lease_expires_at,omitempty"`
	// Resolution is pinned under the processing claim before any account fold.
	// A recovered worker reuses it verbatim, so provider/routing changes cannot
	// turn a crash retry from ignored into applied (or the reverse).
	Resolution *EventReceiptResolution `json:"resolution,omitempty"`
	// Version is the receipt CAS token, analogous to Record.Version.
	Version int64 `json:"version"`
}

// EventReceiptResolution is the immutable, allowlisted outcome of routing and
// provider resolution. Applied work carries the exact account and normalized
// event to fold; ignored work carries neither an event nor an account mutation.
type EventReceiptResolution struct {
	Decision     string         `json:"decision"`
	IgnoreReason string         `json:"ignore_reason,omitempty"`
	AccountID    string         `json:"account_id,omitempty"`
	Event        *billing.Event `json:"event,omitempty"`
	ResolvedAt   time.Time      `json:"resolved_at"`
}

// EventReceiptStore is an optional extension implemented by the production R2
// store and the in-memory reference store. Manager requires it whenever a
// provider supplies a durable ProviderEventID; legacy in-process events with no
// identity retain the pre-receipt path for compatibility.
type EventReceiptStore interface {
	ReceiveEvent(
		ctx context.Context,
		receipt EventReceipt,
	) (stored EventReceipt, created bool, err error)
	ClaimEvent(
		ctx context.Context,
		receipt EventReceipt,
		claimToken string,
		now, leaseExpiresAt time.Time,
	) (claimed EventReceipt, acquired bool, err error)
	PinEventResolution(
		ctx context.Context,
		receipt EventReceipt,
		resolution EventReceiptResolution,
	) (EventReceipt, error)
	ReleaseEvent(ctx context.Context, receipt EventReceipt) error
	CompleteEvent(
		ctx context.Context,
		receipt EventReceipt,
		processedAt time.Time,
	) error
	PendingEvents(
		ctx context.Context,
		provider, customerID string,
		limit int,
	) ([]EventReceipt, error)
}

func newEventReceipt(provider string, event billing.Event, receivedAt time.Time) (EventReceipt, error) {
	receipt := EventReceipt{
		SchemaVersion: eventReceiptSchemaVersion,
		Provider:      provider,
		Event:         event,
		Status:        EventReceiptPending,
		ReceivedAt:    receivedAt.UTC(),
	}
	if err := validateEventReceipt(receipt); err != nil {
		return EventReceipt{}, err
	}
	return receipt, nil
}

func validateEventReceipt(receipt EventReceipt) error {
	e := receipt.Event
	switch {
	case receipt.SchemaVersion != eventReceiptSchemaVersion:
		return fmt.Errorf("event receipt: unsupported schema version %d", receipt.SchemaVersion)
	case receipt.Provider == "" || strings.TrimSpace(receipt.Provider) != receipt.Provider || len(receipt.Provider) > 64:
		return errors.New("event receipt: provider must be 1-64 normalized characters")
	case e.ProviderEventID == "" || strings.TrimSpace(e.ProviderEventID) != e.ProviderEventID || len(e.ProviderEventID) > 255:
		return errors.New("event receipt: provider event id must be 1-255 normalized characters")
	case e.CustomerID == "" || strings.TrimSpace(e.CustomerID) != e.CustomerID || len(e.CustomerID) > 255:
		return errors.New("event receipt: customer id must be 1-255 normalized characters")
	case len(e.PayloadSHA256) != 64:
		return errors.New("event receipt: payload sha256 must be 64 hexadecimal characters")
	case e.At.IsZero():
		return errors.New("event receipt: provider event timestamp is required")
	case receipt.ReceivedAt.IsZero():
		return errors.New("event receipt: received_at is required")
	case len(e.ProviderObjectID) > 255 || len(e.SubscriptionID) > 255 || len(e.OperationID) > 255:
		return errors.New("event receipt: provider object identifiers must not exceed 255 characters")
	case len(e.Plan) > 128:
		return errors.New("event receipt: plan must not exceed 128 characters")
	}
	if _, err := hex.DecodeString(e.PayloadSHA256); err != nil || strings.ToLower(e.PayloadSHA256) != e.PayloadSHA256 {
		return errors.New("event receipt: payload sha256 must be lowercase hexadecimal")
	}
	switch e.Type {
	case billing.EventSubscriptionActivated, billing.EventPaymentFailed,
		billing.EventPaymentRecovered, billing.EventSubscriptionCanceled:
	default:
		return fmt.Errorf("event receipt: unsupported event type %q", e.Type)
	}
	switch receipt.Status {
	case EventReceiptPending:
		if receipt.Decision != "" || receipt.ProcessedAt != nil {
			return errors.New("event receipt: pending receipt cannot have a decision or processed_at")
		}
		if (receipt.ClaimToken == "") != (receipt.LeaseExpiresAt == nil) {
			return errors.New("event receipt: processing claim requires both token and lease expiry")
		}
		if receipt.ClaimToken != "" {
			if strings.TrimSpace(receipt.ClaimToken) != receipt.ClaimToken ||
				len(receipt.ClaimToken) > 128 || receipt.ClaimGeneration < 1 ||
				receipt.LeaseExpiresAt.IsZero() {
				return errors.New("event receipt: invalid processing claim")
			}
		}
	case EventReceiptProcessed:
		if receipt.Decision == "" || receipt.ProcessedAt == nil || receipt.ProcessedAt.IsZero() {
			return errors.New("event receipt: processed receipt requires decision and processed_at")
		}
		if receipt.ClaimToken != "" || receipt.LeaseExpiresAt != nil {
			return errors.New("event receipt: processed receipt cannot retain a processing claim")
		}
	default:
		return fmt.Errorf("event receipt: unsupported status %q", receipt.Status)
	}
	if receipt.Version < 0 || receipt.ClaimGeneration < 0 {
		return errors.New("event receipt: versions cannot be negative")
	}
	if receipt.Resolution != nil {
		if err := validateEventReceiptResolution(e, *receipt.Resolution); err != nil {
			return err
		}
	}
	if receipt.Status == EventReceiptProcessed {
		if receipt.Resolution == nil || receipt.Decision != receipt.Resolution.Decision {
			return errors.New("event receipt: processed decision requires its pinned resolution")
		}
	}
	return nil
}

func validateEventReceiptResolution(
	received billing.Event,
	resolution EventReceiptResolution,
) error {
	if resolution.ResolvedAt.IsZero() {
		return errors.New("event receipt: resolution timestamp is required")
	}
	switch resolution.Decision {
	case "applied":
		if resolution.IgnoreReason != "" || resolution.AccountID == "" ||
			strings.TrimSpace(resolution.AccountID) != resolution.AccountID ||
			len(resolution.AccountID) > 255 || resolution.Event == nil {
			return errors.New("event receipt: applied resolution requires an account and event")
		}
		resolved := *resolution.Event
		if resolved.ProviderEventID != received.ProviderEventID ||
			resolved.PayloadSHA256 != received.PayloadSHA256 ||
			resolved.CustomerID != received.CustomerID {
			return errors.New("event receipt: resolved event changed delivery identity or customer")
		}
		if resolved.At.IsZero() || len(resolved.Plan) > 128 ||
			len(resolved.ProviderObjectID) > 255 ||
			len(resolved.SubscriptionID) > 255 || len(resolved.OperationID) > 255 {
			return errors.New("event receipt: invalid resolved event")
		}
		switch resolved.Type {
		case billing.EventSubscriptionActivated, billing.EventPaymentFailed,
			billing.EventPaymentRecovered, billing.EventSubscriptionCanceled:
		default:
			return fmt.Errorf("event receipt: unsupported resolved event type %q", resolved.Type)
		}
	case "ignored":
		if resolution.IgnoreReason == "" ||
			strings.TrimSpace(resolution.IgnoreReason) != resolution.IgnoreReason ||
			len(resolution.IgnoreReason) > 64 || resolution.Event != nil ||
			len(resolution.AccountID) > 255 ||
			strings.TrimSpace(resolution.AccountID) != resolution.AccountID {
			return errors.New("event receipt: ignored resolution is invalid")
		}
	default:
		return fmt.Errorf("event receipt: unsupported resolution decision %q", resolution.Decision)
	}
	return nil
}

func sameEventReceiptIdentity(a, b EventReceipt) bool {
	return a.Provider == b.Provider &&
		a.Event.Type == b.Event.Type &&
		a.Event.ProviderEventID == b.Event.ProviderEventID &&
		a.Event.CustomerID == b.Event.CustomerID &&
		a.Event.PayloadSHA256 == b.Event.PayloadSHA256 &&
		a.Event.Plan == b.Event.Plan &&
		a.Event.At.Equal(b.Event.At) &&
		a.Event.ProviderObjectID == b.Event.ProviderObjectID &&
		a.Event.SubscriptionID == b.Event.SubscriptionID &&
		a.Event.OperationID == b.Event.OperationID
}

func cloneEventReceipt(receipt EventReceipt) EventReceipt {
	if receipt.ProcessedAt != nil {
		processedAt := *receipt.ProcessedAt
		receipt.ProcessedAt = &processedAt
	}
	if receipt.LeaseExpiresAt != nil {
		leaseExpiresAt := *receipt.LeaseExpiresAt
		receipt.LeaseExpiresAt = &leaseExpiresAt
	}
	if receipt.Resolution != nil {
		resolution := *receipt.Resolution
		if resolution.Event != nil {
			event := *resolution.Event
			resolution.Event = &event
		}
		receipt.Resolution = &resolution
	}
	return receipt
}

func sameEventReceiptResolution(a, b EventReceiptResolution) bool {
	if a.Decision != b.Decision || a.IgnoreReason != b.IgnoreReason ||
		a.AccountID != b.AccountID || !a.ResolvedAt.Equal(b.ResolvedAt) ||
		(a.Event == nil) != (b.Event == nil) {
		return false
	}
	if a.Event == nil {
		return true
	}
	return a.Event.Type == b.Event.Type &&
		a.Event.CustomerID == b.Event.CustomerID &&
		a.Event.Plan == b.Event.Plan &&
		a.Event.At.Equal(b.Event.At) &&
		a.Event.ProviderEventID == b.Event.ProviderEventID &&
		a.Event.PayloadSHA256 == b.Event.PayloadSHA256 &&
		a.Event.ProviderObjectID == b.Event.ProviderObjectID &&
		a.Event.SubscriptionID == b.Event.SubscriptionID &&
		a.Event.OperationID == b.Event.OperationID
}

func newEventReceiptClaimToken() (string, error) {
	var entropy [18]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("event receipt: generate processing claim: %w", err)
	}
	return "ecl_" + base64.RawURLEncoding.EncodeToString(entropy[:]), nil
}

func validateEventClaimRequest(
	claimToken string,
	now, leaseExpiresAt time.Time,
) error {
	if claimToken == "" || strings.TrimSpace(claimToken) != claimToken ||
		len(claimToken) > 128 {
		return errors.New("event receipt: claim token must be 1-128 normalized characters")
	}
	if now.IsZero() || leaseExpiresAt.IsZero() || !leaseExpiresAt.After(now) {
		return errors.New("event receipt: claim lease must end after a non-zero claim time")
	}
	return nil
}

func eventReceiptClaimMatches(current, claimed EventReceipt) bool {
	return current.ClaimToken != "" &&
		current.ClaimToken == claimed.ClaimToken &&
		current.ClaimGeneration == claimed.ClaimGeneration
}

func encodeEventReceiptComponent(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func eventReceiptIdentityKey(provider, eventID string) string {
	return encodeEventReceiptComponent(provider) + "/" +
		encodeEventReceiptComponent(eventID)
}

func eventReceiptCustomerKey(provider, customerID string) string {
	return encodeEventReceiptComponent(provider) + "/" +
		encodeEventReceiptComponent(customerID)
}
