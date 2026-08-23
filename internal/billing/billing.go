// Package billing is the provider-plugin seam between Witself Cloud's control
// plane and whatever billing partner runs underneath (issue #31). The control
// plane owns the plan state machine (desired -> entitled -> applied) and talks
// to the partner ONLY through the Provider interface, so the partner is
// swappable (fake today; Stripe, Metronome, or a self-hosted meter later)
// without touching cells, the CLI, or the state machine.
//
// Cells never import this package: a cell enforces the plan snapshot on its
// account records and stays billing-ignorant. Self-hosted deployments run with
// no Provider at all.
//
// The load-bearing design decision is the two-outcome Action contract: every
// money-moving operation either completed (Done) or requires the payer's
// browser at URL. That single escape hatch covers the first card capture
// (PCI/SCA make it browser-bound by regulation), and the occasional bank
// challenge on a saved card — so the CLI has exactly one code path: Done ->
// print, URL -> open browser and poll.
package billing

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// ErrPendingAlreadyResolved means an exact provider object can no longer be
// cancelled because its effect completed. Callers must not report success or
// clear local pending state unless a fresh authoritative fold independently
// proves that resolution.
var ErrPendingAlreadyResolved = errors.New("billing pending object already resolved")

// Action is the outcome of a billing operation that may need the payer's
// browser: either it completed (Done) or the payer must continue at URL — a
// checkout page, a card-update form, or a bank's 3DS challenge.
type Action struct {
	Done bool
	URL  string // set when !Done; where the payer completes the operation
	// ProviderObjectID is the exact hosted object backing URL (for Stripe, a
	// Checkout Session). It remains control-plane internal and lets retries or
	// cancellation target the original object rather than customer-wide state.
	ProviderObjectID string
	// ExpiresAt is the provider's own action expiry. A durable receipt may
	// replay URL only before this instant; afterward the caller must start a
	// fresh operation with a new idempotency key.
	ExpiresAt time.Time
}

// EventType classifies the normalized webhook events a Provider emits. These
// are the ONLY billing facts the control plane reacts to; provider-specific
// event zoos are collapsed into these four by each implementation.
type EventType string

const (
	// EventSubscriptionActivated reports a confirmed payment — the account is
	// entitled to Event.Plan. Fires on first purchase and on plan changes.
	EventSubscriptionActivated EventType = "subscription_activated"
	// EventPaymentFailed reports a failed renewal charge. The control plane
	// decides grace policy; the provider only reports.
	EventPaymentFailed EventType = "payment_failed"
	// EventPaymentRecovered reports that a previously failed charge succeeded.
	EventPaymentRecovered EventType = "payment_recovered"
	// EventSubscriptionCanceled reports the subscription ended (downgrade took
	// effect, cancellation, or terminal dunning). Entitlement reverts to free.
	EventSubscriptionCanceled EventType = "subscription_canceled"
)

// Event is a normalized billing fact tied to a provider customer. The control
// plane maps CustomerID back to an account via its registry; events carry no
// account IDs because the provider does not know them.
type Event struct {
	Type       EventType `json:"type"`
	CustomerID string    `json:"customer_id"`
	Plan       string    `json:"plan,omitempty"` // plan id, when the event carries one
	At         time.Time `json:"at"`

	// ProviderEventID is the provider's immutable delivery identity (for
	// Stripe, evt_...). PayloadSHA256 binds that identity to the exact signed
	// body. Together they let the control plane durably suppress exact
	// redelivery and fail closed if one identity is ever reused for different
	// content. Providers used only in-process may leave both empty.
	ProviderEventID string `json:"provider_event_id,omitempty"`
	PayloadSHA256   string `json:"payload_sha256,omitempty"`
	// ProviderObjectID identifies data.object inside the provider event.
	// SubscriptionID is populated when that object refers to a subscription
	// indirectly (for example a Checkout Session). These are operational
	// reconciliation handles, never cell policy or customer-facing identity.
	ProviderObjectID string `json:"provider_object_id,omitempty"`
	SubscriptionID   string `json:"subscription_id,omitempty"`
	// OperationID binds a provider callback to the durable Witself operation
	// that created it when the provider carries that metadata through.
	OperationID string `json:"operation_id,omitempty"`
}

// IdempotentSubscriber is the optional strong form of Provider.Subscribe.
// A lifecycle manager uses it when available so an ambiguous network retry of
// one durable upgrade operation cannot create a second Checkout Session or
// subscription. Implementations must replay the same result for the same key
// and reject reuse with different customer/plan parameters.
type IdempotentSubscriber interface {
	SubscribeIdempotent(
		ctx context.Context,
		customerID, plan, operationID string,
	) (Action, error)
}

// IdempotentSetupper is the optional strong form of Provider.SetupLink.
// Implementations replay the original Action for an exact operation retry and
// reject reuse of the operation identity for another customer.
type IdempotentSetupper interface {
	SetupLinkIdempotent(
		ctx context.Context,
		customerID, operationID string,
	) (Action, error)
}

// IdempotentDowngrader is the optional strong form of
// Provider.ScheduleDowngrade. Implementations replay the original effective
// time for an exact operation retry and reject reuse of the operation identity
// with different customer or plan parameters.
type IdempotentDowngrader interface {
	ScheduleDowngradeIdempotent(
		ctx context.Context,
		customerID, plan, operationID string,
	) (effective time.Time, err error)
}

// ScheduledDowngrade is the exact provider effect armed for a period-end
// plan change. ProviderObjectID identifies the subscription whose period-end
// flag was mutated; it must be persisted before cancellation can be exact.
type ScheduledDowngrade struct {
	Effective        time.Time
	ProviderObjectID string
}

// ExactIdempotentDowngrader is the strong form used by durable managed
// billing. It returns both the effective boundary and the exact provider
// object that was armed.
type ExactIdempotentDowngrader interface {
	ScheduleDowngradeExactIdempotent(
		ctx context.Context,
		customerID, plan, operationID string,
	) (ScheduledDowngrade, error)
}

// PreparedIdempotentDowngrader splits a period-end downgrade into a read-only
// target selection and an exact mutation. Managed lifecycle persists the
// prepared target before crossing the provider mutation boundary, so a lost
// response cannot make a retry rediscover and arm a different subscription.
type PreparedIdempotentDowngrader interface {
	PrepareDowngrade(
		ctx context.Context,
		customerID, plan string,
	) (ScheduledDowngrade, error)
	SchedulePreparedDowngradeIdempotent(
		ctx context.Context,
		customerID, plan, operationID string,
		prepared ScheduledDowngrade,
	) (ScheduledDowngrade, error)
}

// DowngradeTargetChecker is the optional target-aware capability side of an
// IdempotentDowngrader. Providers whose downgrade support is narrower than the
// plan catalog implement it so a write-free preview can refuse an unsupported
// target before a durable mutation receipt reserves the account billing lane.
// Providers that omit this interface retain the broad IdempotentDowngrader
// contract for compatibility.
type DowngradeTargetChecker interface {
	SupportsDowngradeTarget(plan string) bool
}

// UpgradeTransitionChecker lets a provider declare which self-serve upgrade
// transitions it can actually execute, so the lifecycle refuses the ones it
// cannot rather than issuing a purchase that collides with a live
// subscription. Providers that omit this interface are treated as able to
// execute every upgrade, preserving the broad Subscribe contract.
type UpgradeTransitionChecker interface {
	SupportsUpgradeTransition(current, target string) bool
}

// IdempotentPendingCanceller is the optional strong form of
// Provider.CancelPending. Implementations replay successful completion for an
// exact operation retry and reject reuse of the operation identity for another
// customer.
type IdempotentPendingCanceller interface {
	CancelPendingIdempotent(
		ctx context.Context,
		customerID, operationID string,
	) error
}

// PendingCancellationKind identifies the exact provider object whose pending
// effect must be disarmed. These names describe provider-neutral effects, not
// Stripe resource types.
type PendingCancellationKind string

const (
	// PendingCancellationHostedAction is an unfinished hosted purchase action
	// such as a Stripe Checkout Session.
	PendingCancellationHostedAction PendingCancellationKind = "hosted_action"
	// PendingCancellationPeriodEnd is a subscription already armed to end at
	// its current billing period boundary.
	PendingCancellationPeriodEnd PendingCancellationKind = "period_end"
)

// PendingCancellation is the durable, value-minimal identity of one provider
// effect. OriginalOperationID lets a provider verify that a hosted object was
// created by the exact Witself operation now being replaced.
type PendingCancellation struct {
	Kind                PendingCancellationKind
	ProviderObjectID    string
	OriginalOperationID string
}

// ExactPendingCanceller is the strong, object-scoped form of pending
// cancellation. Implementations must inspect and mutate only target; they must
// never discover and cancel every open object belonging to customerID.
type ExactPendingCanceller interface {
	CancelPendingObjectIdempotent(
		ctx context.Context,
		customerID string,
		target PendingCancellation,
		operationID string,
	) error
}

// ValidateOperationID enforces the portable identity accepted by durable
// provider mutations. IDs are deliberately restricted to a small ASCII set so
// they can be copied safely into provider metadata and idempotency headers.
func ValidateOperationID(operationID string) error {
	if len(operationID) < 1 || len(operationID) > 128 {
		return errors.New("billing operation id must be 1-128 characters")
	}
	for i := 0; i < len(operationID); i++ {
		b := operationID[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '.' || b == '_' || b == ':' || b == '-' {
			continue
		}
		return errors.New("billing operation id contains unsupported characters")
	}
	return nil
}

// ValidateProviderObjectID enforces the small ASCII identity admitted into
// durable billing state. It intentionally accepts the same portable alphabet
// as operation IDs while allowing the longer provider resource identifiers.
func ValidateProviderObjectID(objectID string) error {
	if len(objectID) < 1 || len(objectID) > 255 {
		return errors.New("billing provider object id must be 1-255 characters")
	}
	for i := 0; i < len(objectID); i++ {
		b := objectID[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '.' || b == '_' || b == ':' || b == '-' {
			continue
		}
		return errors.New("billing provider object id contains unsupported characters")
	}
	return nil
}

// EventResolver performs provider reads that are unsafe to do before a
// verified event has a durable receipt. Returning nil means the event is a
// durable no-op (for example one deleted duplicate subscription while another
// live subscription still backs the customer). The returned event must retain
// the input's delivery identity, payload hash, and customer.
type EventResolver interface {
	ResolveEvent(ctx context.Context, event Event) (*Event, error)
}

// PaymentMethod is the payer's stored instrument, described for display only
// (e.g. "visa ****4242"). Providers never expose raw card data.
type PaymentMethod struct {
	Label string
}

// Invoice is a normalized invoice. Amounts are integer cents to keep money
// exact. PDFURL points at the provider-rendered document — Witself never
// renders invoices itself.
type Invoice struct {
	Number      string
	Date        time.Time
	AmountCents int64
	Currency    string
	Status      string // draft | open | paid | void | uncollectible
	PDFURL      string
	HostedURL   string
}

// Payment is a normalized charge or refund. Charges use a positive amount;
// refunds use a negative amount so callers can reconstruct the net movement
// without guessing from a display status.
type Payment struct {
	Date        time.Time
	AmountCents int64
	Currency    string
	Method      string // display label, e.g. "visa ****4242"
	Status      string // succeeded | failed | pending | refunded
	ReceiptURL  string
}

// UpcomingCharge previews the next renewal, for `witself billing`.
type UpcomingCharge struct {
	Date        time.Time
	AmountCents int64
	Currency    string
}

// Provider is the billing-partner plugin. Implementations must be safe for
// concurrent use; the control plane calls them from HTTP handlers.
//
// Money-moving methods return Action (done | needs_action(url)). Read methods
// are plain queries. All identifiers are the provider's own customer IDs; the
// control plane's registry maps account <-> customer.
type Provider interface {
	// EnsureCustomer returns the provider customer for an account, creating it
	// on first use. Idempotent per accountID.
	EnsureCustomer(ctx context.Context, accountID, email string) (customerID string, err error)

	// SetupLink starts payment-method capture (`witself billing setup`) — the
	// once-per-payer browser hoop. Done means an instrument is already on file
	// or the provider captured one without interaction.
	SetupLink(ctx context.Context, customerID string) (Action, error)

	// PortalLink returns the provider's hosted self-serve portal (card
	// updates, invoice history, cancellation). Always a URL by nature.
	PortalLink(ctx context.Context, customerID string) (string, error)

	// Subscribe purchases or switches to plan. With an instrument on file this
	// completes headlessly (Done); otherwise — or when the bank demands a
	// challenge — it returns the URL to continue at. Entitlement is confirmed
	// by Done or by a later EventSubscriptionActivated, never assumed.
	Subscribe(ctx context.Context, customerID, plan string) (Action, error)

	// ScheduleDowngrade arranges the switch to a cheaper plan at period end
	// (the decided downgrade policy) and returns when it takes effect.
	ScheduleDowngrade(ctx context.Context, customerID, plan string) (effective time.Time, err error)

	// CancelPending abandons the in-flight change: an unfinished checkout or a
	// scheduled downgrade. No-op when nothing is pending.
	CancelPending(ctx context.Context, customerID string) error

	// HandleWebhook verifies and parses a provider callback into normalized
	// events. Implementations authenticate the request (signatures); callers
	// must treat redelivery as normal and process events idempotently. One
	// provider callback identity may produce zero or one normalized Event,
	// never multiple Events carrying the same ProviderEventID.
	HandleWebhook(r *http.Request) ([]Event, error)

	// RecordUsage reports metered usage (phase 1 — gates the Team tier).
	// idempotencyKey deduplicates retries so re-sent events cannot double-bill.
	RecordUsage(ctx context.Context, customerID, metric string, quantity int64, idempotencyKey string) error

	// PaymentMethodOnFile returns the stored instrument, or nil when none.
	PaymentMethodOnFile(ctx context.Context, customerID string) (*PaymentMethod, error)

	// ListInvoices returns invoices, newest first.
	ListInvoices(ctx context.Context, customerID string) ([]Invoice, error)

	// ListPayments returns charges and refunds, newest first. Refund amounts
	// are negative and successful refunds use status "refunded".
	ListPayments(ctx context.Context, customerID string) ([]Payment, error)

	// NextCharge previews the next renewal, or nil when none is coming (no
	// subscription, or a downgrade to free is scheduled).
	NextCharge(ctx context.Context, customerID string) (*UpcomingCharge, error)
}
