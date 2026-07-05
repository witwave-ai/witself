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
package lifecycle

import (
	"context"
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
	Pending    *Pending
	// PastDueSince is set while the provider reports failed renewals. Grace
	// policy (when to suspend) is a control-plane decision layered on top.
	PastDueSince *time.Time
}

// Store persists Records. Implementations must be safe for concurrent use.
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

// Status returns the account's record, creating the default (free, applied)
// record on first sight — free is the zero value and needs no billing objects.
func (m *Manager) Status(ctx context.Context, accountID, email string) (Record, error) {
	r, ok, err := m.cfg.Store.Get(ctx, accountID)
	if err != nil {
		return Record{}, err
	}
	if !ok {
		r = Record{AccountID: accountID, Email: email, Entitled: plans.Free, Applied: plans.Free}
		if err := m.cfg.Store.Put(ctx, r); err != nil {
			return Record{}, err
		}
	}
	return r, nil
}

// RequestUpgrade starts an upgrade to plan. Outcomes: "done" (charged on file
// and applied), "action" (complete at URL), or "contact" (tier not self-serve;
// lead recorded). A new request replaces any pending change.
func (m *Manager) RequestUpgrade(ctx context.Context, accountID, email, planID string) (Outcome, error) {
	r, err := m.Status(ctx, accountID, email)
	if err != nil {
		return Outcome{}, err
	}
	target, ok := m.cfg.Catalog.Get(planID)
	if !ok {
		return Outcome{}, fmt.Errorf("unknown plan %q", planID)
	}
	current, _ := m.cfg.Catalog.Get(r.Entitled)
	if target.ID == r.Entitled {
		return Outcome{}, fmt.Errorf("already on the %s plan", target.ID)
	}
	if target.PriceCents() <= current.PriceCents() {
		return Outcome{}, fmt.Errorf("%s is not an upgrade from %s — use downgrade", target.ID, current.ID)
	}
	// Not self-serve yet: record the interest instead of erroring. The stored
	// desire IS the sales lead; the hoop is "talk to us" instead of "pay".
	if !target.Available {
		now := m.cfg.Now()
		r.Pending = &Pending{Kind: PendingContact, Plan: target.ID, Requested: now}
		if err := m.cfg.Store.Put(ctx, r); err != nil {
			return Outcome{}, err
		}
		return Outcome{Kind: "contact", Plan: target.ID}, nil
	}
	if !target.Purchasable() {
		return Outcome{}, fmt.Errorf("plan %q is not purchasable", target.ID)
	}

	name, provider, err := m.providerFor(r)
	if err != nil {
		return Outcome{}, err
	}
	if r.CustomerID == "" {
		id, err := provider.EnsureCustomer(ctx, accountID, email)
		if err != nil {
			return Outcome{}, err
		}
		// First billing objects: pin the provider for the life of the
		// relationship. A later Default switch must not reroute this account.
		r.CustomerID = id
		r.Provider = name
	}
	act, err := provider.Subscribe(ctx, r.CustomerID, target.ID)
	if err != nil {
		return Outcome{}, err
	}
	now := m.cfg.Now()
	if !act.Done {
		// The async hoop: park the desired change with a TTL and hand back the
		// URL. Entitlement arrives later via EventSubscriptionActivated.
		r.Pending = &Pending{
			Kind: PendingUpgrade, Plan: target.ID, URL: act.URL,
			Expires: now.Add(m.cfg.TTL), Requested: now,
		}
		if err := m.cfg.Store.Put(ctx, r); err != nil {
			return Outcome{}, err
		}
		return Outcome{Kind: "action", Plan: target.ID, URL: act.URL}, nil
	}
	// Charged headlessly: entitled now; apply immediately (best effort — if
	// the cell push fails, Reconcile retries until entitled == applied).
	r.Pending = nil
	r.Entitled = target.ID
	if err := m.cfg.Store.Put(ctx, r); err != nil {
		return Outcome{}, err
	}
	m.apply(ctx, &r)
	return Outcome{Kind: "done", Plan: target.ID}, nil
}

// RequestDowngrade schedules a downgrade for period end after the fit check.
// A blocked downgrade returns an error listing every violation — the account
// must be pruned first; nothing is silently degraded.
func (m *Manager) RequestDowngrade(ctx context.Context, accountID, email, planID string) (Outcome, error) {
	r, err := m.Status(ctx, accountID, email)
	if err != nil {
		return Outcome{}, err
	}
	target, ok := m.cfg.Catalog.Get(planID)
	if !ok {
		return Outcome{}, fmt.Errorf("unknown plan %q", planID)
	}
	current, _ := m.cfg.Catalog.Get(r.Entitled)
	if target.ID == r.Entitled {
		return Outcome{}, fmt.Errorf("already on the %s plan", target.ID)
	}
	if target.PriceCents() >= current.PriceCents() {
		return Outcome{}, fmt.Errorf("%s is not a downgrade from %s — use upgrade", target.ID, current.ID)
	}
	violations, err := m.cfg.Fit.Fit(ctx, accountID, target)
	if err != nil {
		return Outcome{}, err
	}
	if len(violations) > 0 {
		return Outcome{}, fmt.Errorf("blocked — the account does not fit the %s plan:\n  %s",
			target.ID, strings.Join(violations, "\n  "))
	}
	_, provider, err := m.providerFor(r)
	if err != nil {
		return Outcome{}, err
	}
	effective, err := provider.ScheduleDowngrade(ctx, r.CustomerID, target.ID)
	if err != nil {
		return Outcome{}, err
	}
	r.Pending = &Pending{
		Kind: PendingDowngrade, Plan: target.ID,
		Effective: effective, Requested: m.cfg.Now(),
	}
	if err := m.cfg.Store.Put(ctx, r); err != nil {
		return Outcome{}, err
	}
	return Outcome{Kind: "scheduled", Plan: target.ID, Effective: effective}, nil
}

// CancelPending abandons the in-flight change (unfinished checkout, scheduled
// downgrade, or a recorded lead).
func (m *Manager) CancelPending(ctx context.Context, accountID string) error {
	r, ok, err := m.cfg.Store.Get(ctx, accountID)
	if err != nil {
		return err
	}
	if !ok || r.Pending == nil {
		return fmt.Errorf("nothing is pending")
	}
	// Leads have no provider-side state to cancel.
	if r.Pending.Kind != PendingContact && r.CustomerID != "" {
		_, provider, err := m.providerFor(r)
		if err != nil {
			return err
		}
		if err := provider.CancelPending(ctx, r.CustomerID); err != nil {
			return err
		}
	}
	r.Pending = nil
	return m.cfg.Store.Put(ctx, r)
}

// OnEvents folds normalized provider events into records. provider names the
// configured partner whose webhook delivered them — the control plane routes
// one webhook endpoint per partner, so the name is always known — and lookups
// are scoped to it so another partner's customer ids can never match. Events
// are processed idempotently: applying the same event twice converges on the
// same state, so redelivery is harmless.
func (m *Manager) OnEvents(ctx context.Context, provider string, events []billing.Event) error {
	for _, e := range events {
		r, ok, err := m.cfg.Store.ByCustomer(ctx, provider, e.CustomerID)
		if err != nil {
			return err
		}
		if !ok {
			// Unknown customer: not ours (or not yet registered). Skip rather
			// than fail the whole batch — providers redeliver on error.
			continue
		}
		switch e.Type {
		case billing.EventSubscriptionActivated:
			r.Entitled = e.Plan
			r.PastDueSince = nil
			if p := r.Pending; p != nil && (p.Kind == PendingUpgrade || p.Kind == PendingDowngrade) && p.Plan == e.Plan {
				r.Pending = nil
			}
		case billing.EventSubscriptionCanceled:
			r.Entitled = plans.Free
			if p := r.Pending; p != nil && p.Kind == PendingDowngrade {
				r.Pending = nil
			}
		case billing.EventPaymentFailed:
			if r.PastDueSince == nil {
				t := e.At
				r.PastDueSince = &t
			}
		case billing.EventPaymentRecovered:
			r.PastDueSince = nil
		}
		if err := m.cfg.Store.Put(ctx, r); err != nil {
			return err
		}
		m.apply(ctx, &r)
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
	for _, r := range records {
		if p := r.Pending; p != nil && p.Kind == PendingUpgrade && now.After(p.Expires) {
			if r.CustomerID != "" {
				_, provider, err := m.providerFor(r)
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
				} else if err := provider.CancelPending(ctx, r.CustomerID); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			r.Pending = nil
			if err := m.cfg.Store.Put(ctx, r); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if r.Entitled != r.Applied {
			m.apply(ctx, &r)
		}
	}
	return firstErr
}

// apply pushes the entitled plan's snapshot to the cell and records success.
// Failures are deliberately swallowed here: Applied stays behind Entitled and
// the next Reconcile retries — the one state that never rests.
func (m *Manager) apply(ctx context.Context, r *Record) {
	if r.Entitled == r.Applied {
		return
	}
	p, ok := m.cfg.Catalog.Get(r.Entitled)
	if !ok {
		return
	}
	features := append([]string(nil), p.Features...)
	sort.Strings(features)
	if err := m.cfg.Applier.Apply(ctx, r.AccountID, p.ID, p.Limits, features); err != nil {
		return
	}
	r.Applied = p.ID
	_ = m.cfg.Store.Put(ctx, *r)
}
