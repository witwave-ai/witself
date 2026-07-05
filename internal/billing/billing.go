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
	"net/http"
	"time"
)

// Action is the outcome of a billing operation that may need the payer's
// browser: either it completed (Done) or the payer must continue at URL — a
// checkout page, a card-update form, or a bank's 3DS challenge.
type Action struct {
	Done bool
	URL  string // set when !Done; where the payer completes the operation
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
	Type       EventType
	CustomerID string
	Plan       string // plan id, when the event carries one
	At         time.Time
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

// Payment is a normalized charge with its receipt.
type Payment struct {
	Date        time.Time
	AmountCents int64
	Currency    string
	Method      string // display label, e.g. "visa ****4242"
	Status      string // succeeded | failed | refunded
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
	// must treat redelivery as normal and process events idempotently.
	HandleWebhook(r *http.Request) ([]Event, error)

	// RecordUsage reports metered usage (phase 1 — gates the Team tier).
	// idempotencyKey deduplicates retries so re-sent events cannot double-bill.
	RecordUsage(ctx context.Context, customerID, metric string, quantity int64, idempotencyKey string) error

	// PaymentMethodOnFile returns the stored instrument, or nil when none.
	PaymentMethodOnFile(ctx context.Context, customerID string) (*PaymentMethod, error)

	// ListInvoices returns invoices, newest first.
	ListInvoices(ctx context.Context, customerID string) ([]Invoice, error)

	// ListPayments returns charges, newest first.
	ListPayments(ctx context.Context, customerID string) ([]Payment, error)

	// NextCharge previews the next renewal, or nil when none is coming (no
	// subscription, or a downgrade to free is scheduled).
	NextCharge(ctx context.Context, customerID string) (*UpcomingCharge, error)
}
