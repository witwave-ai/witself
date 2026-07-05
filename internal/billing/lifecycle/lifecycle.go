// Package lifecycle is the plan state machine (issue #31): the control-plane
// logic that moves an account between plans through the decided flow —
// desired -> entitled -> applied — with the billing partner behind the
// billing.Provider seam.
//
// The three states are separate facts on the Record:
//
//	Pending  (desired)  — the change the user asked for, working through hoops
//	Entitled            — what billing has confirmed payment for
//	Applied             — what the cell is actually enforcing
//
// The Manager is deliberately storage- and transport-agnostic: Store persists
// records (the control plane backs it with its database; memstore backs tests
// and dev), Applier pushes the resolved plan snapshot to the account's cell,
// and FitChecker answers whether current usage fits a downgrade target. All
// decided rules live here: one pending change at a time, requests expire
// (checkout-session TTL), checks run twice (advisory at request, authoritative
// at apply), downgrades take effect at period end, entitled != applied never
// rests, and asking for an unavailable tier records a sales lead instead of
// erroring.
//
// The Manager holds NAMED providers, not one: each record pins the provider
// its billing objects live at (Record.Provider, set at first purchase), and
// only a first purchase uses the configured default. That makes a provider
// cutover a config change, not a migration event — flip Default to the new
// partner and new purchases land there while existing accounts keep routing
// to the provider that holds their subscription, card, and invoice history.
// Swapping the fake for the real partner at launch is just the first use of
// this path (Kill Bill's per-account plugin binding is the precedent).
//
// Concurrency and ordering (adversarial review, 2026-07-05): records are
// versioned and Store.Put is compare-and-swap, so concurrent HTTP handlers and
// webhook deliveries cannot silently lose each other's writes — every
// operation re-reads and re-decides on conflict instead of clobbering.
// Provider calls happen OUTSIDE the read-modify-write loop (a claim is parked
// first, the partner is called once, the outcome is folded into the freshest
// record), so retries never double-charge. Entitlement-changing events carry
// the partner's timestamp and are dropped when they predate the record's
// current entitlement (EntitledAt) — a redelivered or out-of-order
// cancellation cannot clobber a newer paid subscription.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/plans"
)

// PendingKind classifies the single in-flight change on a Record.
type PendingKind string

const (
	// PendingUpgrade awaits payment at URL (expires at Expires).
	PendingUpgrade PendingKind = "upgrade"
	// PendingDowngrade is scheduled to take effect at Effective (period end).
	PendingDowngrade PendingKind = "downgrade"
	// PendingContact is a recorded sales lead for a tier that is not
	// self-serve yet (available: false). No billing objects exist for it.
	PendingContact PendingKind = "contact"
)

// Pending is the one in-flight change (the "desired" state).
type Pending struct {
	Kind      PendingKind
	Plan      string
	URL       string    // upgrade: where the payer completes checkout
	Expires   time.Time // upgrade: when the request lapses
	Effective time.Time // downgrade: when it takes effect
	Requested time.Time
}

// Record is one account's billing state. Plan facts only — identity stays in
// the control plane's registry, and the account -> cell mapping stays in the
// directory.
type Record struct {
	AccountID string
	Email     string
	// Provider names the configured provider this account's billing objects
	// live at. Pinned when the customer is first created and kept for the
	// life of those objects (subscription, card, invoice history) — a default
	// switch never reroutes an existing relationship. Empty for accounts with
	// no billing objects (free forever, or leads).
	Provider   string
	CustomerID string // provider customer id, once one exists
	Entitled   string // plan billing has confirmed; plans.Free when none
	Applied    string // plan the cell is enforcing; plans.Free until pushed
	// EntitledAt is when Entitled last changed — the staleness fence for
	// entitlement events: an activation/cancellation whose partner timestamp
	// predates it is stale (redelivered or out of order) and is dropped.
	EntitledAt time.Time
	Pending    *Pending
	// PastDueSince is set while the provider reports failed renewals. Grace
	// policy (when to suspend) is a control-plane decision layered on top.
	PastDueSince *time.Time
	// DunningAt is the staleness fence for dunning events, the analogue of
	// EntitledAt: a payment_failed/recovered whose partner timestamp predates
	// it is a redelivered stale event and is dropped — so a redelivered
	// "failed" can never re-mark an account that has since recovered.
	DunningAt time.Time
	// ApplyBlocked carries the violation report when the authoritative
	// apply-time fit check refused to push a downgraded snapshot to the cell.
	// The cell keeps enforcing the old plan (the gap stays visible as
	// Entitled != Applied) until the account is pruned and Reconcile clears
	// this — never a silent degrade.
	ApplyBlocked string
	// Version is the optimistic-concurrency token. Zero means "not yet
	// stored"; Store.Put refuses stale versions (ErrStale) and increments on
	// success. Callers never touch it.
	Version int64
}

// ErrStale is returned by Store.Put when the record changed since it was
// read. The Manager re-reads and re-decides; Store implementations return it
// verbatim.
var ErrStale = errors.New("lifecycle: stale record version")

// ErrRefusal is the sentinel every user-addressed refusal wraps: "already on
// standard", "not an upgrade from ...", the fit-check violations report,
// "nothing is pending". Everything NOT wrapped in it is infrastructure or
// provider failure (Store I/O, provider API errors, CAS exhaustion) — the
// HTTP layer maps refusals to 409 with the message verbatim and everything
// else to a generic 500, so an R2 outage never reads as a policy conflict
// and backend detail never reaches the CLI.
var ErrRefusal = errors.New("plan change refused")

// refuse builds a user-addressed refusal.
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrRefusal}, args...)...)
}

// Store persists Records with optimistic concurrency. Implementations must be
// safe for concurrent use, and Put MUST be compare-and-swap on Record.Version:
// reject a Put whose Version differs from the stored one (ErrStale), treat
// Version zero as create-only, and increment the stored Version on success.
type Store interface {
	Get(ctx context.Context, accountID string) (Record, bool, error)
	// ByCustomer resolves (provider, customer id) to its record — how webhook
	// events (which carry only customer ids) find their account. Scoped by
	// provider because two partners' customer-id namespaces can collide.
	ByCustomer(ctx context.Context, provider, customerID string) (Record, bool, error)
	Put(ctx context.Context, r Record) error
	// List returns every record; Reconcile sweeps it. Fine at control-plane
	// scale (accounts, not agents).
	List(ctx context.Context) ([]Record, error)
}

// Applier pushes the resolved plan snapshot to the account's cell (the cell
// system endpoint). The control plane implements it with its cell client; the
// cell then enforces autonomously.
type Applier interface {
	Apply(ctx context.Context, accountID, plan string, limits map[string]int64, features []string) error
}

// FitChecker reports why an account does NOT fit a downgrade target (agents
// over the cap, features in use the target lacks, ...). Empty means it fits.
// It runs twice by design: advisory at request time, authoritative at apply
// time — usage keeps moving while hoops are jumped.
type FitChecker interface {
	Fit(ctx context.Context, accountID string, target plans.Plan) (violations []string, err error)
}

// AlwaysFits is the FitChecker used until cells expose usage queries.
type AlwaysFits struct{}

// Fit implements FitChecker: no violations.
func (AlwaysFits) Fit(context.Context, string, plans.Plan) ([]string, error) { return nil, nil }

// DefaultPendingTTL matches hosted checkout-session expiry (~24h): an
// abandoned upgrade lapses instead of lingering as a zombie intent.
const DefaultPendingTTL = 24 * time.Hour

// casAttempts bounds the re-read/re-decide loop under contention. Conflicts
// are rare (per-account writes); exhausting this means something is spinning.
const casAttempts = 5

// Outcome is what a plan operation resolved to — the CLI renders it directly.
type Outcome struct {
	// Kind: "done" (applied), "action" (continue at URL), "scheduled"
	// (downgrade at Effective), or "contact" (sales lead recorded).
	Kind      string
	Plan      string
	URL       string
	Effective time.Time
}

// Config assembles a Manager.
type Config struct {
	Catalog *plans.Catalog
	// Providers are the configured billing partners by name (e.g. "fake",
	// "stripe"). Records pin the name their billing objects live at.
	Providers map[string]billing.Provider
	// Default names the provider FIRST purchases go to. Switching it is the
	// cutover mechanism: existing records keep their pinned provider.
	Default string
	Store   Store
	Applier Applier
	Fit     FitChecker       // nil -> AlwaysFits
	TTL     time.Duration    // nil-ish (0) -> DefaultPendingTTL
	Now     func() time.Time // nil -> time.Now
}

// Manager runs the plan state machine.
type Manager struct {
	cfg Config
}

// NewManager validates cfg and returns a Manager.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Catalog == nil || cfg.Store == nil || cfg.Applier == nil {
		return nil, fmt.Errorf("lifecycle: Catalog, Store, and Applier are all required")
	}
	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("lifecycle: at least one named provider is required")
	}
	if _, ok := cfg.Providers[cfg.Default]; !ok {
		return nil, fmt.Errorf("lifecycle: Default %q is not a configured provider", cfg.Default)
	}
	if cfg.Fit == nil {
		cfg.Fit = AlwaysFits{}
	}
	if cfg.TTL == 0 {
		cfg.TTL = DefaultPendingTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{cfg: cfg}, nil
}

// providerFor resolves the record's pinned provider, or the default when the
// record has no billing objects yet. A pinned name missing from Providers is
// a configuration error — a partner was removed while accounts still lived
// on it — and every operation on such a record fails loudly.
func (m *Manager) providerFor(r Record) (string, billing.Provider, error) {
	name := r.Provider
	if name == "" {
		name = m.cfg.Default
	}
	p, ok := m.cfg.Providers[name]
	if !ok {
		return "", nil, fmt.Errorf("account %s is pinned to provider %q, which is not configured", r.AccountID, name)
	}
	return name, p, nil
}

// load reads the record, synthesizing the free/free default (Version zero,
// unstored) when none exists. Reading NEVER writes — a record is only
// persisted by an operation that actually changes billing state, so probing
// an account id cannot create phantom rows.
func (m *Manager) load(ctx context.Context, accountID, email string) (Record, error) {
	r, ok, err := m.cfg.Store.Get(ctx, accountID)
	if err != nil {
		return Record{}, err
	}
	if !ok {
		r = Record{AccountID: accountID, Email: email, Entitled: plans.Free, Applied: plans.Free}
	}
	if email != "" {
		r.Email = email
	}
	return r, nil
}

// mutate runs a read-decide-write cycle under optimistic concurrency: load
// the freshest record, let fn decide (fn may return errSkipWrite to finish
// without writing), and Put with CAS — retrying the WHOLE cycle on ErrStale
// so every retry re-decides against reality instead of clobbering it. fn must
// be side-effect-free; provider calls belong outside (claim/fold pattern).
var errSkipWrite = errors.New("lifecycle: no write needed")

func (m *Manager) mutate(ctx context.Context, accountID, email string, fn func(*Record) error) (Record, error) {
	for range casAttempts {
		r, err := m.load(ctx, accountID, email)
		if err != nil {
			return Record{}, err
		}
		if err := fn(&r); err != nil {
			if errors.Is(err, errSkipWrite) {
				return r, nil
			}
			return Record{}, err
		}
		switch err := m.cfg.Store.Put(ctx, r); {
		case err == nil:
			return r, nil
		case errors.Is(err, ErrStale):
			continue
		default:
			return Record{}, err
		}
	}
	return Record{}, fmt.Errorf("lifecycle: account %s: too much contention", accountID)
}

// Status returns the account's billing state. Read-only: unknown accounts get
// the synthesized free/free default without creating a record.
func (m *Manager) Status(ctx context.Context, accountID, email string) (Record, error) {
	return m.load(ctx, accountID, email)
}

// cancelReplacedPending cancels the provider-side state of a pending change
// that is being replaced (one pending at a time — but the partner must agree,
// or a "replaced" scheduled downgrade would still fire at period end and a
// replaced checkout could still complete). Leads have no provider-side state.
func (m *Manager) cancelReplacedPending(ctx context.Context, r Record) error {
	if r.Pending == nil || r.Pending.Kind == PendingContact || r.CustomerID == "" {
		return nil
	}
	_, provider, err := m.providerFor(r)
	if err != nil {
		return err
	}
	return provider.CancelPending(ctx, r.CustomerID)
}

// RequestUpgrade starts an upgrade to plan. Outcomes: "done" (charged on file
// and applied), "action" (complete at URL), or "contact" (tier not self-serve;
// lead recorded). A new request replaces any pending change — including its
// provider-side state.
func (m *Manager) RequestUpgrade(ctx context.Context, accountID, email, planID string) (Outcome, error) {
	target, ok := m.cfg.Catalog.Get(planID)
	if !ok {
		return Outcome{}, refuse("unknown plan %q", planID)
	}
	now := m.cfg.Now()

	// Claim: validate against the freshest record and park the desired change
	// BEFORE any provider call, so a concurrent request cannot double-charge —
	// it either sees our claim (and replaces it, failing our fold) or we see
	// its state. The claim also captures what pending change it replaces.
	var replaced Record
	claim, err := m.mutate(ctx, accountID, email, func(r *Record) error {
		current, _ := m.cfg.Catalog.Get(r.Entitled)
		if target.ID == r.Entitled {
			return refuse("already on the %s plan", target.ID)
		}
		if target.PriceCents() <= current.PriceCents() {
			return refuse("%s is not an upgrade from %s — use downgrade", target.ID, current.ID)
		}
		if !target.Available {
			// Not self-serve yet: the stored desire IS the sales lead; the
			// hoop is "talk to us" instead of "pay".
			replaced = *r
			r.Pending = &Pending{Kind: PendingContact, Plan: target.ID, Requested: now}
			return nil
		}
		if !target.Purchasable() {
			return refuse("plan %q is not purchasable", target.ID)
		}
		replaced = *r
		r.Pending = &Pending{
			Kind: PendingUpgrade, Plan: target.ID,
			Expires: now.Add(m.cfg.TTL), Requested: now,
		}
		return nil
	})
	if err != nil {
		return Outcome{}, err
	}
	if err := m.cancelReplacedPending(ctx, replaced); err != nil {
		return Outcome{}, err
	}
	if claim.Pending.Kind == PendingContact {
		return Outcome{Kind: "contact", Plan: target.ID}, nil
	}

	// Provider calls, exactly once, outside any retry loop.
	name, provider, err := m.providerFor(claim)
	if err != nil {
		return Outcome{}, err
	}
	customerID := claim.CustomerID
	if customerID == "" {
		if customerID, err = provider.EnsureCustomer(ctx, accountID, email); err != nil {
			m.releaseClaim(ctx, accountID, claim)
			return Outcome{}, err
		}
	}
	act, err := provider.Subscribe(ctx, customerID, target.ID)
	if err != nil {
		m.releaseClaim(ctx, accountID, claim)
		return Outcome{}, err
	}

	// Fold: land the provider outcome on the freshest record — but only if
	// our claim is still the pending change (a concurrent request may have
	// replaced it, in which case its provider state superseded ours).
	folded, err := m.mutate(ctx, accountID, email, func(r *Record) error {
		p := r.Pending
		if p == nil || p.Kind != PendingUpgrade || p.Plan != target.ID || !p.Requested.Equal(now) {
			return refuse("upgrade to %s was superseded by another request", target.ID)
		}
		if r.CustomerID == "" {
			// First billing objects: pin the provider for the life of the
			// relationship. A Default switch must not reroute this account.
			r.CustomerID = customerID
			r.Provider = name
		}
		if act.Done {
			r.Pending = nil
			r.Entitled = target.ID
			r.EntitledAt = now
		} else {
			r.Pending.URL = act.URL
		}
		return nil
	})
	if err != nil {
		return Outcome{}, err
	}
	if act.Done {
		m.apply(ctx, folded.AccountID)
		return Outcome{Kind: "done", Plan: target.ID}, nil
	}
	return Outcome{Kind: "action", Plan: target.ID, URL: act.URL}, nil
}

// releaseClaim clears the claim we parked if it is still ours — best effort;
// an unreleasable claim simply expires at its TTL.
func (m *Manager) releaseClaim(ctx context.Context, accountID string, claim Record) {
	_, _ = m.mutate(ctx, accountID, claim.Email, func(r *Record) error {
		p, q := r.Pending, claim.Pending
		if p == nil || q == nil || p.Kind != q.Kind || p.Plan != q.Plan || !p.Requested.Equal(q.Requested) {
			return errSkipWrite
		}
		r.Pending = nil
		return nil
	})
}

// RequestDowngrade schedules a downgrade for period end after the advisory
// fit check. A blocked downgrade returns an error listing every violation —
// the account must be pruned first; nothing is silently degraded. The check
// runs again, authoritatively, before the snapshot is applied.
func (m *Manager) RequestDowngrade(ctx context.Context, accountID, email, planID string) (Outcome, error) {
	target, ok := m.cfg.Catalog.Get(planID)
	if !ok {
		return Outcome{}, refuse("unknown plan %q", planID)
	}
	now := m.cfg.Now()

	var replaced Record
	claim, err := m.mutate(ctx, accountID, email, func(r *Record) error {
		current, _ := m.cfg.Catalog.Get(r.Entitled)
		if target.ID == r.Entitled {
			return refuse("already on the %s plan", target.ID)
		}
		if target.PriceCents() >= current.PriceCents() {
			return refuse("%s is not a downgrade from %s — use upgrade", target.ID, current.ID)
		}
		violations, err := m.cfg.Fit.Fit(ctx, accountID, target)
		if err != nil {
			return err
		}
		if len(violations) > 0 {
			return refuse("blocked — the account does not fit the %s plan:\n  %s",
				target.ID, strings.Join(violations, "\n  "))
		}
		replaced = *r
		r.Pending = &Pending{Kind: PendingDowngrade, Plan: target.ID, Requested: now}
		return nil
	})
	if err != nil {
		return Outcome{}, err
	}
	if err := m.cancelReplacedPending(ctx, replaced); err != nil {
		return Outcome{}, err
	}

	_, provider, err := m.providerFor(claim)
	if err != nil {
		return Outcome{}, err
	}
	effective, err := provider.ScheduleDowngrade(ctx, claim.CustomerID, target.ID)
	if err != nil {
		m.releaseClaim(ctx, accountID, claim)
		return Outcome{}, err
	}
	if _, err := m.mutate(ctx, accountID, email, func(r *Record) error {
		p := r.Pending
		if p == nil || p.Kind != PendingDowngrade || p.Plan != target.ID || !p.Requested.Equal(now) {
			return refuse("downgrade to %s was superseded by another request", target.ID)
		}
		r.Pending.Effective = effective
		return nil
	}); err != nil {
		return Outcome{}, err
	}
	return Outcome{Kind: "scheduled", Plan: target.ID, Effective: effective}, nil
}

// CancelPending abandons the in-flight change (unfinished checkout, scheduled
// downgrade, or a recorded lead). If the change already resolved — say the
// payer completed checkout while this call was in flight — the fresh re-read
// sees it and this returns "nothing is pending" instead of clobbering the new
// entitlement.
func (m *Manager) CancelPending(ctx context.Context, accountID string) error {
	// Disarm the provider FIRST: if its cancel fails, the record still shows
	// the pending change (truthful, retryable). The old order cleared local
	// state first, so a provider blip left a schedule armed at the partner
	// with nothing visible locally — a downgrade would fire "spontaneously"
	// at period end with no API path left to disarm it.
	r, err := m.load(ctx, accountID, "")
	if err != nil {
		return err
	}
	if r.Pending == nil {
		return refuse("nothing is pending")
	}
	if err := m.cancelReplacedPending(ctx, r); err != nil {
		return err
	}
	_, err = m.mutate(ctx, accountID, "", func(r *Record) error {
		if r.Pending == nil {
			// The change resolved while we were disarming (e.g. the payer
			// completed checkout): nothing to clear, entitlement stands.
			return refuse("nothing is pending")
		}
		r.Pending = nil
		return nil
	})
	return err
}

// OnEvents folds normalized provider events into records. provider names the
// configured partner whose webhook delivered them — the control plane routes
// one webhook endpoint per partner, so the name is always known — and lookups
// are scoped to it so another partner's customer ids can never match.
//
// Redelivery and ordering: entitlement events (activated/canceled) do not
// commute with intervening changes, so any such event whose partner timestamp
// predates the record's EntitledAt is stale and dropped; dunning events fence
// on DunningAt the same way. Within those fences, folding the same event
// twice converges on the same state. Events that cannot be routed (unknown
// customer, unknown plan) FAIL the batch — a 2xx would ACK a possibly-paid
// event forever; redelivery lets the claim/fold race and catalog deploys
// resolve.
func (m *Manager) OnEvents(ctx context.Context, provider string, events []billing.Event) error {
	for _, e := range events {
		seed, ok, err := m.cfg.Store.ByCustomer(ctx, provider, e.CustomerID)
		if err != nil {
			return err
		}
		if !ok {
			// Unknown customer: either NOT YET ours (the claim/fold window —
			// the index lands moments later) or genuinely foreign. We cannot
			// tell the difference, and a silent ACK on the former loses a
			// paid event forever. Fail the batch so the provider redelivers;
			// convergence resolves the race, and truly-foreign deliveries age
			// out at the provider's retry horizon.
			return fmt.Errorf("event %s for unknown %s customer — will resolve on redelivery", e.Type, provider)
		}
		// An activation for a plan this catalog does not know is deploy skew
		// (the partner sells something this binary has not heard of). ACKing
		// would lose a PAID event forever; fail the batch so the provider
		// redelivers until the catalog catches up.
		if e.Type == billing.EventSubscriptionActivated {
			if _, ok := m.cfg.Catalog.Get(e.Plan); !ok {
				return fmt.Errorf("activation for plan %q not in this catalog — will resolve on redelivery after deploy", e.Plan)
			}
		}
		r, err := m.mutate(ctx, seed.AccountID, "", func(r *Record) error {
			switch e.Type {
			case billing.EventSubscriptionActivated:
				if !e.At.IsZero() && e.At.Before(r.EntitledAt) {
					return errSkipWrite // stale: predates the current entitlement
				}
				r.Entitled = e.Plan
				r.EntitledAt = e.At
				r.PastDueSince = nil
				if p := r.Pending; p != nil && (p.Kind == PendingUpgrade || p.Kind == PendingDowngrade) && p.Plan == e.Plan {
					r.Pending = nil
				}
			case billing.EventSubscriptionCanceled:
				if !e.At.IsZero() && e.At.Before(r.EntitledAt) {
					return errSkipWrite // stale cancel of an older subscription
				}
				r.Entitled = plans.Free
				r.EntitledAt = e.At
				// Terminal states clear dunning: there is no longer a failing
				// renewal to be past due on.
				r.PastDueSince = nil
				if p := r.Pending; p != nil && p.Kind == PendingDowngrade {
					r.Pending = nil
				}
			case billing.EventPaymentFailed:
				if !e.At.IsZero() && e.At.Before(r.DunningAt) {
					return errSkipWrite // stale: predates newer dunning state
				}
				if r.PastDueSince == nil {
					t := e.At
					r.PastDueSince = &t
				}
				r.DunningAt = e.At
			case billing.EventPaymentRecovered:
				if !e.At.IsZero() && e.At.Before(r.DunningAt) {
					return errSkipWrite
				}
				r.PastDueSince = nil
				r.DunningAt = e.At
			default:
				return errSkipWrite
			}
			return nil
		})
		if err != nil {
			return err
		}
		m.apply(ctx, r.AccountID)
	}
	return nil
}

// Reconcile is the periodic sweep: it expires lapsed upgrade requests (the
// abandoned-checkout TTL) and retries every record whose cell snapshot lags
// its entitlement — entitled != applied never rests.
func (m *Manager) Reconcile(ctx context.Context) error {
	records, err := m.cfg.Store.List(ctx)
	if err != nil {
		return err
	}
	now := m.cfg.Now()
	var firstErr error
	for _, snapshot := range records {
		var expired Record
		r, err := m.mutate(ctx, snapshot.AccountID, "", func(r *Record) error {
			p := r.Pending
			if p == nil || p.Kind != PendingUpgrade || !now.After(p.Expires) {
				return errSkipWrite
			}
			expired = *r
			r.Pending = nil
			return nil
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if expired.Pending != nil {
			if err := m.cancelReplacedPending(ctx, expired); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if r.Entitled != r.Applied {
			m.apply(ctx, r.AccountID)
		}
	}
	return firstErr
}

// apply converges the cell onto the entitled plan. For downgrades it runs the
// AUTHORITATIVE fit check first — the advisory one ran at request time, but
// usage kept moving while the hoops were jumped — and refuses to push a
// snapshot the account no longer fits: the cell keeps enforcing the old plan
// (in the customer's favor), the gap stays visible as Entitled != Applied plus
// ApplyBlocked, and every Reconcile retries until the account is pruned.
// Push failures are likewise left for Reconcile — the one state that never
// rests. Errors are deliberately not returned: convergence is eventual.
func (m *Manager) apply(ctx context.Context, accountID string) {
	r, err := m.load(ctx, accountID, "")
	if err != nil || r.Version == 0 || r.Entitled == r.Applied {
		return
	}
	target, ok := m.cfg.Catalog.Get(r.Entitled)
	if !ok {
		return
	}
	applied, _ := m.cfg.Catalog.Get(r.Applied)
	if target.PriceCents() < applied.PriceCents() {
		violations, err := m.cfg.Fit.Fit(ctx, accountID, target)
		if err != nil {
			return
		}
		if len(violations) > 0 {
			_, _ = m.mutate(ctx, accountID, "", func(r *Record) error {
				report := strings.Join(violations, "; ")
				if r.Entitled != target.ID || r.ApplyBlocked == report {
					return errSkipWrite
				}
				r.ApplyBlocked = report
				return nil
			})
			return
		}
	}
	features := append([]string(nil), target.Features...)
	sort.Strings(features)
	if err := m.cfg.Applier.Apply(ctx, accountID, target.ID, target.Limits, features); err != nil {
		return
	}
	_, _ = m.mutate(ctx, accountID, "", func(r *Record) error {
		if r.Entitled != target.ID {
			return errSkipWrite // entitlement moved again while we pushed
		}
		r.Applied = target.ID
		r.ApplyBlocked = ""
		return nil
	})
}
