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
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
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
	Kind PendingKind
	Plan string
	// OperationID is the durable identity of one provider mutation. Upgrade
	// retries retain it so Stripe receives the same idempotency key after an
	// ambiguous response or control-plane crash. Contact claims also carry an
	// operation identity so a failed cleanup of the provider state they
	// replaced can be resumed without losing the cleanup fence.
	OperationID string
	// CancelPrevious is a durable pre-subscribe phase. It remains true until
	// CancelPending succeeds and the result is folded, so a provider error or
	// process crash cannot strand the provider-side change that this claim
	// replaced. It is deliberately false before Subscribe: an ambiguous
	// Subscribe failure must be retried with OperationID, not canceled.
	CancelPrevious bool
	URL            string    // upgrade: where the payer completes checkout
	Expires        time.Time // upgrade: when the request lapses
	Effective      time.Time // downgrade: when it takes effect
	Requested      time.Time
}

func newBillingOperationID() (string, error) {
	var entropy [18]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("lifecycle: generate billing operation id: %w", err)
	}
	return "bop_" + base64.RawURLEncoding.EncodeToString(entropy[:]), nil
}

func validBillingOperationID(operationID string) bool {
	if len(operationID) < 8 || len(operationID) > 128 ||
		!strings.HasPrefix(operationID, "bop_") ||
		strings.TrimSpace(operationID) != operationID {
		return false
	}
	for i := 0; i < len(operationID); i++ {
		if operationID[i] < 0x21 || operationID[i] > 0x7e {
			return false
		}
	}
	return true
}

// AdminActor is the immutable administrator identity attached to a policy
// mutation. ID is the durable registry key; Handle is non-secret display
// metadata and must never be used as the audit identity by itself.
type AdminActor struct {
	ID     string `json:"id"`
	Handle string `json:"handle"`
}

// AccountPlanOverride classifies an account independently of its billing
// provider. It is the comp/backfill seam: billing Entitled remains truthful
// while the effective plan snapshot can be Enterprise (or another tier).
type AccountPlanOverride struct {
	Plan        string    `json:"plan"`
	ActorID     string    `json:"actor_id"`
	ActorHandle string    `json:"actor_handle"`
	Reason      string    `json:"reason"`
	SetAt       time.Time `json:"set_at"`
}

// TranscriptRetentionOverride is an account-specific exception to its plan
// default. Days=nil is an explicit indefinite override; absence of the whole
// struct means inherit the current plan default.
type TranscriptRetentionOverride struct {
	Days        *int64    `json:"days"`
	ActorID     string    `json:"actor_id"`
	ActorHandle string    `json:"actor_handle"`
	Reason      string    `json:"reason"`
	SetAt       time.Time `json:"set_at"`
}

// MessagingOverride is an account-specific exception to whether the durable
// messaging feature is enabled. Absence means inherit the current plan feature
// list; the explicit bool supports both enable and disable exceptions.
type MessagingOverride struct {
	Enabled     bool      `json:"enabled"`
	ActorID     string    `json:"actor_id"`
	ActorHandle string    `json:"actor_handle"`
	Reason      string    `json:"reason"`
	SetAt       time.Time `json:"set_at"`
}

// MessageRetentionOverride is an account-specific exception to its message
// retention default. Days=nil is an explicit indefinite override; absence of
// the whole struct means inherit the current plan default.
type MessageRetentionOverride struct {
	Days        *int64    `json:"days"`
	ActorID     string    `json:"actor_id"`
	ActorHandle string    `json:"actor_handle"`
	Reason      string    `json:"reason"`
	SetAt       time.Time `json:"set_at"`
}

// AgentEmailReceiveOverride is an account-specific exception to whether
// inbound agent email is enabled. Absence means inherit the plan feature.
type AgentEmailReceiveOverride struct {
	Enabled     bool      `json:"enabled"`
	ActorID     string    `json:"actor_id"`
	ActorHandle string    `json:"actor_handle"`
	Reason      string    `json:"reason"`
	SetAt       time.Time `json:"set_at"`
}

// AgentEmailSendOverride is an account-specific exception to whether
// outbound agent email is enabled. Absence means inherit the plan feature.
type AgentEmailSendOverride struct {
	Enabled     bool      `json:"enabled"`
	ActorID     string    `json:"actor_id"`
	ActorHandle string    `json:"actor_handle"`
	Reason      string    `json:"reason"`
	SetAt       time.Time `json:"set_at"`
}

// AgentEmailRetentionOverride is an account-specific exception to inbound
// agent-email retention. Days=nil is explicit indefinite retention; absence
// means inherit the current plan default.
type AgentEmailRetentionOverride struct {
	Days        *int64    `json:"days"`
	ActorID     string    `json:"actor_id"`
	ActorHandle string    `json:"actor_handle"`
	Reason      string    `json:"reason"`
	SetAt       time.Time `json:"set_at"`
}

// AccountLimitOverride is an account-specific hard-cap exception. Max=nil is
// an explicit unlimited override; absence of the dimension from
// Record.LimitOverrides means inherit the current plan default.
type AccountLimitOverride struct {
	Max         *int64    `json:"max"`
	ActorID     string    `json:"actor_id"`
	ActorHandle string    `json:"actor_handle"`
	Reason      string    `json:"reason"`
	SetAt       time.Time `json:"set_at"`
}

// AccountLimitValue preserves the difference between an absent audit field
// and an explicitly unlimited value (represented by {"max":null}).
type AccountLimitValue struct {
	Max *int64 `json:"max"`
}

// AdminChange is the append-only audit history embedded in the account's
// compare-and-swap record. Kind determines how nil values are interpreted.
type AdminChange struct {
	Kind                          string             `json:"kind"`
	ActorID                       string             `json:"actor_id"`
	ActorHandle                   string             `json:"actor_handle"`
	Reason                        string             `json:"reason"`
	At                            time.Time          `json:"at"`
	PlanFrom                      string             `json:"plan_from,omitempty"`
	PlanTo                        string             `json:"plan_to,omitempty"`
	RetentionFrom                 *int64             `json:"retention_from,omitempty"`
	RetentionTo                   *int64             `json:"retention_to,omitempty"`
	MessagingFrom                 *bool              `json:"messaging_from,omitempty"`
	MessagingTo                   *bool              `json:"messaging_to,omitempty"`
	MessagingFromSource           string             `json:"messaging_from_source,omitempty"`
	MessagingToSource             string             `json:"messaging_to_source,omitempty"`
	MessageRetentionFrom          *int64             `json:"message_retention_from,omitempty"`
	MessageRetentionTo            *int64             `json:"message_retention_to,omitempty"`
	MessageRetentionFromSource    string             `json:"message_retention_from_source,omitempty"`
	MessageRetentionToSource      string             `json:"message_retention_to_source,omitempty"`
	AgentEmailReceiveFrom         *bool              `json:"agent_email_receive_from,omitempty"`
	AgentEmailReceiveTo           *bool              `json:"agent_email_receive_to,omitempty"`
	AgentEmailReceiveFromSource   string             `json:"agent_email_receive_from_source,omitempty"`
	AgentEmailReceiveToSource     string             `json:"agent_email_receive_to_source,omitempty"`
	AgentEmailSendFrom            *bool              `json:"agent_email_send_from,omitempty"`
	AgentEmailSendTo              *bool              `json:"agent_email_send_to,omitempty"`
	AgentEmailSendFromSource      string             `json:"agent_email_send_from_source,omitempty"`
	AgentEmailSendToSource        string             `json:"agent_email_send_to_source,omitempty"`
	AgentEmailRetentionFrom       *int64             `json:"agent_email_retention_from,omitempty"`
	AgentEmailRetentionTo         *int64             `json:"agent_email_retention_to,omitempty"`
	AgentEmailRetentionFromSource string             `json:"agent_email_retention_from_source,omitempty"`
	AgentEmailRetentionToSource   string             `json:"agent_email_retention_to_source,omitempty"`
	LimitDimension                string             `json:"limit_dimension,omitempty"`
	LimitFrom                     *AccountLimitValue `json:"limit_from,omitempty"`
	LimitTo                       *AccountLimitValue `json:"limit_to,omitempty"`
	LimitFromSource               string             `json:"limit_from_source,omitempty"`
	LimitToSource                 string             `json:"limit_to_source,omitempty"`
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
	// PlanOverride, retention overrides, MessagingOverride, and LimitOverrides
	// are administrator-owned entitlement exceptions. They never create or
	// mutate provider billing objects. AdminHistory preserves every real
	// transition with attribution.
	PlanOverride                *AccountPlanOverride
	TranscriptRetentionOverride *TranscriptRetentionOverride
	MessagingOverride           *MessagingOverride
	MessageRetentionOverride    *MessageRetentionOverride
	AgentEmailReceiveOverride   *AgentEmailReceiveOverride
	AgentEmailSendOverride      *AgentEmailSendOverride
	AgentEmailRetentionOverride *AgentEmailRetentionOverride
	LimitOverrides              map[string]AccountLimitOverride
	AdminHistory                []AdminChange
	// PastDueSince is set while the provider reports failed renewals. Grace
	// policy (when to suspend) is a control-plane decision layered on top.
	PastDueSince *time.Time
	// DunningAt is the staleness fence for dunning events, the analogue of
	// EntitledAt: a payment_failed/recovered whose partner timestamp predates
	// it is a redelivered stale event and is dropped — so a redelivered
	// "failed" can never re-mark an account that has since recovered.
	DunningAt time.Time
	// LastBillingMutation* is the value-minimal account fold tombstone for a
	// durable mutation whose visible Pending state may disappear. It preserves
	// the exact terminal projection across the crash gap before receipt
	// completion without retaining provider payloads.
	LastBillingMutationOperationID string
	LastBillingMutationKind        BillingMutationOperation
	LastBillingMutationResultKind  BillingMutationResultKind
	LastBillingMutationPlan        string
	LastBillingMutationEffective   time.Time
	LastBillingMutationAt          time.Time
	// ApplyBlocked carries the violation report when the authoritative
	// apply-time fit check refused to push a downgraded snapshot to the cell.
	// The cell keeps enforcing the old plan (the gap stays visible as
	// Entitled != Applied) until the account is pruned and Reconcile clears
	// this — never a silent degrade.
	ApplyBlocked string
	// DesiredSnapshotHash/SnapshotRevision are the current control-plane
	// apply fence. AppliedSnapshotRevision/Hash are updated only after the cell
	// returns an exact acknowledgement for that revision. The cell rejects
	// lower revisions, fencing concurrent CP replicas and rolling restarts.
	DesiredSnapshotHash     string
	SnapshotRevision        int64
	AppliedSnapshotRevision int64
	AppliedSnapshotHash     string
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

// ErrBillingUnavailable means the control plane is running the account plan
// lifecycle without a payment provider. Status, plan defaults, administrator
// overrides, seeding, and cell enforcement remain available in this mode, but
// no customer billing mutation may fabricate a charge or subscription.
var ErrBillingUnavailable = errors.New("billing provider is not configured")

// ErrAdminInput identifies a malformed administrator override. It is separate
// from ErrRefusal because these endpoints are an operator surface, not a
// customer plan-change decision.
var ErrAdminInput = errors.New("invalid admin plan override")

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
	Apply(ctx context.Context, accountID string, request ApplyRequest) (ApplyAck, error)
}

// ApplyFenceReader is the optional read side of Applier. Production appliers
// implement it so a restored/rolled-back lifecycle store cannot reuse a
// revision the cell has already accepted. Only the fence is returned: the
// control plane never adopts plan payload from a cell.
type ApplyFenceReader interface {
	ReadApplyFence(ctx context.Context, accountID string) (ApplyFence, error)
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
	// (downgrade at Effective), "cancelled", "resolved" (the targeted pending
	// state changed before cancellation could fold), or "contact".
	Kind      string
	Plan      string
	URL       string
	Effective time.Time
}

// PlanSnapshot is the control plane's resolved account policy. DefaultLimits,
// DefaultPolicies, and DefaultFeatures are the effective plan's catalog
// defaults; Limits, Policies, and Features additionally reflect account-level
// overrides. The effective fields are the exact snapshot pushed to the cell.
type PlanSnapshot struct {
	Plan            string
	DefaultLimits   map[string]int64
	Limits          map[string]int64
	DefaultPolicies map[string]int64
	Policies        map[string]int64
	DefaultFeatures []string
	Features        []string
	Hash            string
}

// ApplyRequest is one monotonic cell snapshot write.
type ApplyRequest struct {
	Revision int64
	PlanSnapshot
}

// ApplyAck proves the exact revision/hash the cell durably accepted.
type ApplyAck struct {
	Revision int64
	Hash     string
}

// ApplyFence is the value-minimal current cell acknowledgement.
type ApplyFence struct {
	Revision int64
	Hash     string
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

	// applyMu serializes apply() PER ACCOUNT. The state machine's own
	// concurrency (CAS on Record.Version) prevents lost writes. The mutex
	// avoids redundant local pushes while the revision/hash fence on the
	// cell remains authoritative across control-plane replicas and rejects
	// stale or conflicting snapshots.
	applyMuMap sync.Mutex
	applyMu    map[string]*sync.Mutex
}

// NewManager validates cfg and returns a Manager.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Catalog == nil || cfg.Store == nil || cfg.Applier == nil {
		return nil, fmt.Errorf("lifecycle: Catalog, Store, and Applier are all required")
	}
	if len(cfg.Providers) == 0 {
		if cfg.Default != "" {
			return nil, fmt.Errorf("lifecycle: Default %q requires a configured provider", cfg.Default)
		}
		cfg.Providers = map[string]billing.Provider{}
	} else {
		if cfg.Default == "" {
			return nil, fmt.Errorf("lifecycle: Default is required when providers are configured")
		}
		if _, ok := cfg.Providers[cfg.Default]; !ok {
			return nil, fmt.Errorf("lifecycle: Default %q is not a configured provider", cfg.Default)
		}
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
	return &Manager{cfg: cfg, applyMu: map[string]*sync.Mutex{}}, nil
}

// BillingAvailable reports whether customer-initiated subscription mutations
// have a real configured provider. It does not affect administrator-owned
// plan/policy overrides.
func (m *Manager) BillingAvailable() bool {
	return len(m.cfg.Providers) > 0 && m.cfg.Default != ""
}

// perAccountApplyMu returns (creating on demand) the mutex serializing apply
// calls for accountID.
func (m *Manager) perAccountApplyMu(accountID string) *sync.Mutex {
	m.applyMuMap.Lock()
	defer m.applyMuMap.Unlock()
	mu, ok := m.applyMu[accountID]
	if !ok {
		mu = &sync.Mutex{}
		m.applyMu[accountID] = mu
	}
	return mu
}

// providerFor resolves the record's pinned provider, or the default when the
// record has no billing objects yet. A pinned name missing from Providers is
// a configuration error — a partner was removed while accounts still lived
// on it — and every operation on such a record fails loudly.
func (m *Manager) providerFor(r Record) (string, billing.Provider, error) {
	if !m.BillingAvailable() {
		return "", nil, ErrBillingUnavailable
	}
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

// EnsureAccount persists the Personal/free lifecycle baseline for one
// directory-authoritative account and converges its exact fenced snapshot onto
// the cell. It never creates provider objects. Existing lifecycle records are
// preserved and merely reconciled, making this safe for both initial backfill
// and recurring discovery of newly activated accounts.
//
// The returned created flag describes only whether this call won the
// create-only store race. applyPending remains true when the cell could not
// acknowledge the exact snapshot, so callers can expose aggregate retryable
// work without logging account identifiers.
func (m *Manager) EnsureAccount(ctx context.Context, accountID string) (created, applyPending bool, err error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return false, false, errors.New("lifecycle: account id is required")
	}
	for range casAttempts {
		_, ok, err := m.cfg.Store.Get(ctx, accountID)
		if err != nil {
			return false, false, err
		}
		if ok {
			if err := m.ReconcileAccount(ctx, accountID); err != nil {
				current, snapshot, readErr := m.ResolvedStatus(ctx, accountID, "")
				if readErr != nil {
					return false, true, errors.Join(err, readErr)
				}
				return false, SnapshotApplyPending(current, snapshot), err
			}
			current, snapshot, err := m.ResolvedStatus(ctx, accountID, "")
			if err != nil {
				return false, false, err
			}
			return false, SnapshotApplyPending(current, snapshot), nil
		}
		r := Record{
			AccountID: accountID,
			Entitled:  plans.Free,
			Applied:   plans.Free,
		}
		switch err := m.cfg.Store.Put(ctx, r); {
		case err == nil:
		case errors.Is(err, ErrStale):
			continue
		default:
			return false, false, err
		}
		if err := m.apply(ctx, accountID); err != nil {
			current, snapshot, readErr := m.ResolvedStatus(ctx, accountID, "")
			if readErr != nil {
				return true, true, errors.Join(err, readErr)
			}
			return true, SnapshotApplyPending(current, snapshot), err
		}
		current, snapshot, err := m.ResolvedStatus(ctx, accountID, "")
		if err != nil {
			return true, false, err
		}
		return true, SnapshotApplyPending(current, snapshot), nil
	}
	return false, false, fmt.Errorf("lifecycle: account %s: too much contention", accountID)
}

// ResolvedStatus returns billing state plus the exact effective snapshot. It
// is read-only: an unknown account still synthesizes Personal/free defaults
// without creating a registry object.
func (m *Manager) ResolvedStatus(ctx context.Context, accountID, email string) (Record, PlanSnapshot, error) {
	r, err := m.load(ctx, accountID, email)
	if err != nil {
		return Record{}, PlanSnapshot{}, err
	}
	snapshot, err := m.resolveSnapshot(r)
	if err != nil {
		return Record{}, PlanSnapshot{}, err
	}
	return r, snapshot, nil
}

func (m *Manager) resolveSnapshot(r Record) (PlanSnapshot, error) {
	planID := r.Entitled
	if r.PlanOverride != nil {
		planID = r.PlanOverride.Plan
	}
	p, ok := m.cfg.Catalog.Get(planID)
	if !ok {
		return PlanSnapshot{}, fmt.Errorf("effective plan %q is not in the catalog", planID)
	}
	snapshot := PlanSnapshot{
		Plan:            p.ID,
		DefaultLimits:   cloneInt64Map(p.Limits),
		Limits:          cloneInt64Map(p.Limits),
		DefaultPolicies: cloneInt64Map(p.Policies),
		Policies:        cloneInt64Map(p.Policies),
		DefaultFeatures: append([]string(nil), p.Features...),
		Features:        append([]string(nil), p.Features...),
	}
	if snapshot.DefaultLimits == nil {
		snapshot.DefaultLimits = map[string]int64{}
	}
	if snapshot.Limits == nil {
		snapshot.Limits = map[string]int64{}
	}
	if snapshot.DefaultPolicies == nil {
		snapshot.DefaultPolicies = map[string]int64{}
	}
	if snapshot.Policies == nil {
		snapshot.Policies = map[string]int64{}
	}
	if snapshot.DefaultFeatures == nil {
		snapshot.DefaultFeatures = []string{}
	}
	if snapshot.Features == nil {
		snapshot.Features = []string{}
	}
	if override := r.TranscriptRetentionOverride; override != nil {
		if override.Days == nil {
			delete(snapshot.Policies, plans.TranscriptRetentionDaysPolicy)
		} else {
			snapshot.Policies[plans.TranscriptRetentionDaysPolicy] = *override.Days
		}
	}
	if override := r.MessageRetentionOverride; override != nil {
		if override.Days == nil {
			delete(snapshot.Policies, plans.MessageRetentionDaysPolicy)
		} else {
			snapshot.Policies[plans.MessageRetentionDaysPolicy] = *override.Days
		}
	}
	if override := r.MessagingOverride; override != nil {
		snapshot.Features = setFeature(
			snapshot.Features, plans.MessagingFeature, override.Enabled)
	}
	if override := r.AgentEmailRetentionOverride; override != nil {
		if override.Days == nil {
			delete(snapshot.Policies, plans.AgentEmailRetentionDaysPolicy)
		} else {
			snapshot.Policies[plans.AgentEmailRetentionDaysPolicy] = *override.Days
		}
	}
	if override := r.AgentEmailReceiveOverride; override != nil {
		snapshot.Features = setFeature(
			snapshot.Features, plans.AgentEmailReceiveFeature, override.Enabled)
	}
	if override := r.AgentEmailSendOverride; override != nil {
		snapshot.Features = setFeature(
			snapshot.Features, plans.AgentEmailSendFeature, override.Enabled)
	}
	for dimension, override := range r.LimitOverrides {
		validationValue := int64(0)
		if override.Max != nil {
			validationValue = *override.Max
		}
		if err := plans.ValidateLimits(map[string]int64{dimension: validationValue}); err != nil {
			return PlanSnapshot{}, fmt.Errorf("resolve account %s limits: %w", r.AccountID, err)
		}
		if override.Max == nil {
			delete(snapshot.Limits, dimension)
		} else {
			snapshot.Limits[dimension] = *override.Max
		}
	}
	if err := plans.ValidateLimits(snapshot.Limits); err != nil {
		return PlanSnapshot{}, fmt.Errorf("resolve account %s limits: %w", r.AccountID, err)
	}
	if err := plans.ValidatePolicies(snapshot.Policies); err != nil {
		return PlanSnapshot{}, fmt.Errorf("resolve account %s policies: %w", r.AccountID, err)
	}
	sort.Strings(snapshot.DefaultFeatures)
	sort.Strings(snapshot.Features)
	hash, err := plans.SnapshotHash(
		snapshot.Plan, snapshot.Limits, snapshot.Policies, snapshot.Features)
	if err != nil {
		return PlanSnapshot{}, fmt.Errorf("hash account plan snapshot: %w", err)
	}
	snapshot.Hash = hash
	return snapshot, nil
}

func cloneInt64Map(in map[string]int64) map[string]int64 {
	if in == nil {
		return nil
	}
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func featureEnabled(features []string, feature string) bool {
	for _, candidate := range features {
		if candidate == feature {
			return true
		}
	}
	return false
}

func setFeature(features []string, feature string, enabled bool) []string {
	out := make([]string, 0, len(features)+1)
	found := false
	for _, candidate := range features {
		if candidate == feature {
			if !found && enabled {
				out = append(out, candidate)
			}
			found = true
			continue
		}
		out = append(out, candidate)
	}
	if enabled && !found {
		out = append(out, feature)
	}
	return out
}

// planRank uses the catalog's documented cheapest-to-richest display order.
// Price cannot establish this ordering for custom-priced Enterprise because
// an intentionally absent price is represented as zero.
func (m *Manager) planRank(planID string) (int, bool) {
	for i, plan := range m.cfg.Catalog.Plans {
		if plan.ID == planID {
			return i, true
		}
	}
	return 0, false
}

func retentionDays(policies map[string]int64) *int64 {
	return policyDays(policies, plans.TranscriptRetentionDaysPolicy)
}

func messageRetentionDays(policies map[string]int64) *int64 {
	return policyDays(policies, plans.MessageRetentionDaysPolicy)
}

func agentEmailRetentionDays(policies map[string]int64) *int64 {
	return policyDays(policies, plans.AgentEmailRetentionDaysPolicy)
}

func policyDays(policies map[string]int64, key string) *int64 {
	value, ok := policies[key]
	if !ok {
		return nil
	}
	valueCopy := value
	return &valueCopy
}

func boolValue(value bool) *bool {
	valueCopy := value
	return &valueCopy
}

func sameOptionalDays(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func limitValue(limits map[string]int64, dimension string) *int64 {
	value, ok := limits[dimension]
	if !ok {
		return nil
	}
	valueCopy := value
	return &valueCopy
}

func accountLimitValue(limitMax *int64) *AccountLimitValue {
	value := &AccountLimitValue{}
	if limitMax != nil {
		maxCopy := *limitMax
		value.Max = &maxCopy
	}
	return value
}

func validateAccountLimit(dimension string, limitMax *int64) (string, error) {
	dimension = strings.TrimSpace(dimension)
	value := int64(0)
	if limitMax != nil {
		value = *limitMax
	}
	if err := plans.ValidateLimits(map[string]int64{dimension: value}); err != nil {
		return "", fmt.Errorf("%w: %v", ErrAdminInput, err)
	}
	return dimension, nil
}

func validateAdminChange(actor AdminActor, reason string) (AdminActor, string, error) {
	actor.ID = strings.TrimSpace(actor.ID)
	actor.Handle = strings.TrimSpace(actor.Handle)
	reason = strings.TrimSpace(reason)
	switch {
	case actor.ID == "" || len(actor.ID) > 128:
		return AdminActor{}, "", fmt.Errorf("%w: actor id must be 1-128 characters", ErrAdminInput)
	case actor.Handle == "" || len(actor.Handle) > 128:
		return AdminActor{}, "", fmt.Errorf("%w: actor handle must be 1-128 characters", ErrAdminInput)
	case reason == "" || len(reason) > 512:
		return AdminActor{}, "", fmt.Errorf("%w: reason must be 1-512 characters", ErrAdminInput)
	default:
		return actor, reason, nil
	}
}

// SetAccountLimitOverride sets one account hard-cap exception independently
// from its plan and provider billing relationship. max=nil explicitly makes
// the dimension unlimited; clearing is the only way to resume inheritance.
func (m *Manager) SetAccountLimitOverride(
	ctx context.Context,
	accountID, dimension string,
	limitMax *int64,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	dimension, err = validateAccountLimit(dimension, limitMax)
	if err != nil {
		return Record{}, err
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		current, overridden := r.LimitOverrides[dimension]
		if overridden &&
			sameOptionalDays(current.Max, limitMax) {
			return errSkipWrite
		}
		fromSource := "inherited"
		if overridden {
			fromSource = "override"
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		if r.LimitOverrides == nil {
			r.LimitOverrides = map[string]AccountLimitOverride{}
		}
		stored := AccountLimitOverride{
			ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, SetAt: now,
		}
		if limitMax != nil {
			value := *limitMax
			stored.Max = &value
		}
		r.LimitOverrides[dimension] = stored
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind: "limit_override_set", ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now, LimitDimension: dimension,
			LimitFrom:       accountLimitValue(limitValue(before.Limits, dimension)),
			LimitTo:         accountLimitValue(stored.Max),
			LimitFromSource: fromSource,
			LimitToSource:   "override",
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	// The override is already durable. A failed cell push is intentionally
	// represented as apply_pending and retried by reconciliation.
	_ = m.apply(ctx, accountID)
	return r, nil
}

// ClearAccountLimitOverride removes one account exception so the dimension
// inherits the current effective plan default. Clearing an absent override is
// an idempotent no-op.
func (m *Manager) ClearAccountLimitOverride(
	ctx context.Context,
	accountID, dimension string,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	dimension, err = validateAccountLimit(dimension, nil)
	if err != nil {
		return Record{}, err
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if _, ok := r.LimitOverrides[dimension]; !ok {
			return errSkipWrite
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		delete(r.LimitOverrides, dimension)
		if len(r.LimitOverrides) == 0 {
			r.LimitOverrides = nil
		}
		after, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind: "limit_override_cleared", ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now, LimitDimension: dimension,
			LimitFrom:       accountLimitValue(limitValue(before.Limits, dimension)),
			LimitTo:         accountLimitValue(limitValue(after.Limits, dimension)),
			LimitFromSource: "override",
			LimitToSource:   "inherited",
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	// The override is already durable. A failed cell push is intentionally
	// represented as apply_pending and retried by reconciliation.
	_ = m.apply(ctx, accountID)
	return r, nil
}

// SetAccountPlanOverride changes the account's effective plan classification
// without changing Entitled, Provider, CustomerID, invoices, or subscriptions.
// This is the safe backfill/comp path for an existing Personal account that
// should be treated as Enterprise.
func (m *Manager) SetAccountPlanOverride(
	ctx context.Context,
	accountID, planID string,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	target, ok := m.cfg.Catalog.Get(strings.TrimSpace(planID))
	if !ok {
		return Record{}, fmt.Errorf("%w: unknown plan %q", ErrAdminInput, planID)
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		currentPlan := r.Entitled
		if r.PlanOverride != nil {
			currentPlan = r.PlanOverride.Plan
			if currentPlan == target.ID {
				return errSkipWrite
			}
		}
		r.PlanOverride = &AccountPlanOverride{
			Plan: target.ID, ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, SetAt: now,
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind: "plan_override_set", ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			PlanFrom: currentPlan, PlanTo: target.ID,
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	// The override is already durable. A failed cell push is intentionally
	// represented as apply_pending and retried by reconciliation.
	_ = m.apply(ctx, accountID)
	return r, nil
}

// ClearAccountPlanOverride restores the provider-backed entitlement. Clearing
// an absent override is an idempotent no-op.
func (m *Manager) ClearAccountPlanOverride(
	ctx context.Context,
	accountID string,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if r.PlanOverride == nil {
			return errSkipWrite
		}
		from := r.PlanOverride.Plan
		r.PlanOverride = nil
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind: "plan_override_cleared", ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			PlanFrom: from, PlanTo: r.Entitled,
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	// The override is already durable. A failed cell push is intentionally
	// represented as apply_pending and retried by reconciliation.
	_ = m.apply(ctx, accountID)
	return r, nil
}

// SetTranscriptRetentionOverride sets an account policy exception independent
// of its plan. days=nil is an explicit indefinite exception. The caller must
// use ClearTranscriptRetentionOverride to resume inheritance.
func (m *Manager) SetTranscriptRetentionOverride(
	ctx context.Context,
	accountID string,
	days *int64,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	if days != nil {
		if err := plans.ValidatePolicies(map[string]int64{
			plans.TranscriptRetentionDaysPolicy: *days,
		}); err != nil {
			return Record{}, fmt.Errorf("%w: %v", ErrAdminInput, err)
		}
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if current := r.TranscriptRetentionOverride; current != nil &&
			sameOptionalDays(current.Days, days) {
			return errSkipWrite
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		var storedDays *int64
		if days != nil {
			value := *days
			storedDays = &value
		}
		r.TranscriptRetentionOverride = &TranscriptRetentionOverride{
			Days: storedDays, ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, SetAt: now,
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind:    "transcript_retention_override_set",
			ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			RetentionFrom: retentionDays(before.Policies), RetentionTo: storedDays,
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	// The override is already durable. A failed cell push is intentionally
	// represented as apply_pending and retried by reconciliation.
	_ = m.apply(ctx, accountID)
	return r, nil
}

// ClearTranscriptRetentionOverride restores the current effective plan
// default. A later plan-default change therefore flows through automatically.
func (m *Manager) ClearTranscriptRetentionOverride(
	ctx context.Context,
	accountID string,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if r.TranscriptRetentionOverride == nil {
			return errSkipWrite
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		r.TranscriptRetentionOverride = nil
		after, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind:    "transcript_retention_override_cleared",
			ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			RetentionFrom: retentionDays(before.Policies),
			RetentionTo:   retentionDays(after.Policies),
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	// The override is already durable. A failed cell push is intentionally
	// represented as apply_pending and retried by reconciliation.
	_ = m.apply(ctx, accountID)
	return r, nil
}

// SetMessagingOverride enables or disables durable messaging for one account
// independently of its plan and billing relationship. The client integration
// remains installed; the resolved snapshot changes what the cell permits.
func (m *Manager) SetMessagingOverride(
	ctx context.Context,
	accountID string,
	enabled bool,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if current := r.MessagingOverride; current != nil &&
			current.Enabled == enabled {
			return errSkipWrite
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		fromSource := "inherited"
		if r.MessagingOverride != nil {
			fromSource = "override"
		}
		r.MessagingOverride = &MessagingOverride{
			Enabled: enabled, ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, SetAt: now,
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind:    "messaging_override_set",
			ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			MessagingFrom:       boolValue(featureEnabled(before.Features, plans.MessagingFeature)),
			MessagingTo:         boolValue(enabled),
			MessagingFromSource: fromSource,
			MessagingToSource:   "override",
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	_ = m.apply(ctx, accountID)
	return r, nil
}

// ClearMessagingOverride restores the messaging feature state inherited from
// the account's current effective plan.
func (m *Manager) ClearMessagingOverride(
	ctx context.Context,
	accountID string,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if r.MessagingOverride == nil {
			return errSkipWrite
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		r.MessagingOverride = nil
		after, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind:    "messaging_override_cleared",
			ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			MessagingFrom:       boolValue(featureEnabled(before.Features, plans.MessagingFeature)),
			MessagingTo:         boolValue(featureEnabled(after.Features, plans.MessagingFeature)),
			MessagingFromSource: "override",
			MessagingToSource:   "inherited",
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	_ = m.apply(ctx, accountID)
	return r, nil
}

// SetMessageRetentionOverride sets a finite or explicit indefinite account
// exception independently of messaging availability and billing. Keeping the
// controls independent lets an administrator safely configure retention
// before enabling messaging.
func (m *Manager) SetMessageRetentionOverride(
	ctx context.Context,
	accountID string,
	days *int64,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	if days != nil {
		if err := plans.ValidatePolicies(map[string]int64{
			plans.MessageRetentionDaysPolicy: *days,
		}); err != nil {
			return Record{}, fmt.Errorf("%w: %v", ErrAdminInput, err)
		}
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if current := r.MessageRetentionOverride; current != nil &&
			sameOptionalDays(current.Days, days) {
			return errSkipWrite
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		fromSource := "inherited"
		if r.MessageRetentionOverride != nil {
			fromSource = "override"
		}
		var storedDays *int64
		if days != nil {
			value := *days
			storedDays = &value
		}
		r.MessageRetentionOverride = &MessageRetentionOverride{
			Days: storedDays, ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, SetAt: now,
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind:    "message_retention_override_set",
			ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			MessageRetentionFrom:       messageRetentionDays(before.Policies),
			MessageRetentionTo:         storedDays,
			MessageRetentionFromSource: fromSource,
			MessageRetentionToSource:   "override",
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	_ = m.apply(ctx, accountID)
	return r, nil
}

// ClearMessageRetentionOverride restores the current plan default.
func (m *Manager) ClearMessageRetentionOverride(
	ctx context.Context,
	accountID string,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if r.MessageRetentionOverride == nil {
			return errSkipWrite
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		r.MessageRetentionOverride = nil
		after, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind:    "message_retention_override_cleared",
			ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			MessageRetentionFrom:       messageRetentionDays(before.Policies),
			MessageRetentionTo:         messageRetentionDays(after.Policies),
			MessageRetentionFromSource: "override",
			MessageRetentionToSource:   "inherited",
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	_ = m.apply(ctx, accountID)
	return r, nil
}

// SetAgentEmailReceiveOverride enables or disables inbound agent email for one
// account without changing its plan or requiring a client reinstall.
func (m *Manager) SetAgentEmailReceiveOverride(
	ctx context.Context,
	accountID string,
	enabled bool,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if current := r.AgentEmailReceiveOverride; current != nil &&
			current.Enabled == enabled {
			return errSkipWrite
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		fromSource := "inherited"
		if r.AgentEmailReceiveOverride != nil {
			fromSource = "override"
		}
		r.AgentEmailReceiveOverride = &AgentEmailReceiveOverride{
			Enabled: enabled, ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, SetAt: now,
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind:    "agent_email_receive_override_set",
			ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			AgentEmailReceiveFrom: boolValue(featureEnabled(
				before.Features, plans.AgentEmailReceiveFeature)),
			AgentEmailReceiveTo:         boolValue(enabled),
			AgentEmailReceiveFromSource: fromSource,
			AgentEmailReceiveToSource:   "override",
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	_ = m.apply(ctx, accountID)
	return r, nil
}

// ClearAgentEmailReceiveOverride restores the inbound-email feature state
// inherited from the account's effective plan.
func (m *Manager) ClearAgentEmailReceiveOverride(
	ctx context.Context,
	accountID string,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if r.AgentEmailReceiveOverride == nil {
			return errSkipWrite
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		r.AgentEmailReceiveOverride = nil
		after, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind:    "agent_email_receive_override_cleared",
			ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			AgentEmailReceiveFrom: boolValue(featureEnabled(
				before.Features, plans.AgentEmailReceiveFeature)),
			AgentEmailReceiveTo: boolValue(featureEnabled(
				after.Features, plans.AgentEmailReceiveFeature)),
			AgentEmailReceiveFromSource: "override",
			AgentEmailReceiveToSource:   "inherited",
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	_ = m.apply(ctx, accountID)
	return r, nil
}

// SetAgentEmailSendOverride enables or disables outbound agent email for one
// account without changing its plan or requiring a client reinstall.
func (m *Manager) SetAgentEmailSendOverride(
	ctx context.Context,
	accountID string,
	enabled bool,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if current := r.AgentEmailSendOverride; current != nil &&
			current.Enabled == enabled {
			return errSkipWrite
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		fromSource := "inherited"
		if r.AgentEmailSendOverride != nil {
			fromSource = "override"
		}
		r.AgentEmailSendOverride = &AgentEmailSendOverride{
			Enabled: enabled, ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, SetAt: now,
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind:    "agent_email_send_override_set",
			ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			AgentEmailSendFrom: boolValue(featureEnabled(
				before.Features, plans.AgentEmailSendFeature)),
			AgentEmailSendTo:         boolValue(enabled),
			AgentEmailSendFromSource: fromSource,
			AgentEmailSendToSource:   "override",
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	_ = m.apply(ctx, accountID)
	return r, nil
}

// ClearAgentEmailSendOverride restores the outbound-email feature state
// inherited from the account's effective plan.
func (m *Manager) ClearAgentEmailSendOverride(
	ctx context.Context,
	accountID string,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if r.AgentEmailSendOverride == nil {
			return errSkipWrite
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		r.AgentEmailSendOverride = nil
		after, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind:    "agent_email_send_override_cleared",
			ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			AgentEmailSendFrom: boolValue(featureEnabled(
				before.Features, plans.AgentEmailSendFeature)),
			AgentEmailSendTo: boolValue(featureEnabled(
				after.Features, plans.AgentEmailSendFeature)),
			AgentEmailSendFromSource: "override",
			AgentEmailSendToSource:   "inherited",
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	_ = m.apply(ctx, accountID)
	return r, nil
}

// SetAgentEmailRetentionOverride sets a finite or explicit indefinite agent
// email-retention exception independently of receive and send availability.
func (m *Manager) SetAgentEmailRetentionOverride(
	ctx context.Context,
	accountID string,
	days *int64,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	if days != nil {
		if err := plans.ValidatePolicies(map[string]int64{
			plans.AgentEmailRetentionDaysPolicy: *days,
		}); err != nil {
			return Record{}, fmt.Errorf("%w: %v", ErrAdminInput, err)
		}
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if current := r.AgentEmailRetentionOverride; current != nil &&
			sameOptionalDays(current.Days, days) {
			return errSkipWrite
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		fromSource := "inherited"
		if r.AgentEmailRetentionOverride != nil {
			fromSource = "override"
		}
		var storedDays *int64
		if days != nil {
			value := *days
			storedDays = &value
		}
		r.AgentEmailRetentionOverride = &AgentEmailRetentionOverride{
			Days: storedDays, ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, SetAt: now,
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind:    "agent_email_retention_override_set",
			ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			AgentEmailRetentionFrom:       agentEmailRetentionDays(before.Policies),
			AgentEmailRetentionTo:         storedDays,
			AgentEmailRetentionFromSource: fromSource,
			AgentEmailRetentionToSource:   "override",
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	_ = m.apply(ctx, accountID)
	return r, nil
}

// ClearAgentEmailRetentionOverride restores the current plan default.
func (m *Manager) ClearAgentEmailRetentionOverride(
	ctx context.Context,
	accountID string,
	actor AdminActor,
	reason string,
) (Record, error) {
	actor, reason, err := validateAdminChange(actor, reason)
	if err != nil {
		return Record{}, err
	}
	now := m.cfg.Now()
	r, err := m.mutate(ctx, accountID, "", func(r *Record) error {
		if r.AgentEmailRetentionOverride == nil {
			return errSkipWrite
		}
		before, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		r.AgentEmailRetentionOverride = nil
		after, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		r.AdminHistory = append(r.AdminHistory, AdminChange{
			Kind:    "agent_email_retention_override_cleared",
			ActorID: actor.ID, ActorHandle: actor.Handle,
			Reason: reason, At: now,
			AgentEmailRetentionFrom:       agentEmailRetentionDays(before.Policies),
			AgentEmailRetentionTo:         agentEmailRetentionDays(after.Policies),
			AgentEmailRetentionFromSource: "override",
			AgentEmailRetentionToSource:   "inherited",
		})
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	_ = m.apply(ctx, accountID)
	return r, nil
}

// cancelReplacedPending cancels the provider-side state of a pending change
// that is being replaced (one pending at a time — but the partner must agree,
// or a "replaced" scheduled downgrade would still fire at period end and a
// replaced checkout could still complete). Leads have no provider-side state.
func (m *Manager) cancelReplacedPending(ctx context.Context, r Record) error {
	if r.Pending == nil || r.Pending.Kind == PendingContact || r.CustomerID == "" {
		return nil
	}
	return m.cancelProviderPending(ctx, r)
}

// cancelProviderPending disarms provider state for a durable replacement
// phase. Unlike cancelReplacedPending, the record's current Pending describes
// the NEW claim and may itself be a contact lead; CancelPrevious is the proof
// that an older provider-side operation still needs cancellation.
func (m *Manager) cancelProviderPending(ctx context.Context, r Record) error {
	if r.CustomerID == "" {
		return errors.New("lifecycle: pending provider cleanup has no customer id")
	}
	_, provider, err := m.providerFor(r)
	if err != nil {
		return err
	}
	return provider.CancelPending(ctx, r.CustomerID)
}

// cancelProviderPendingIdempotent is the customer-mutation path used by the
// durable billing envelope. A provider that cannot bind cancellation to the
// operation identity is refused before it can make an ambiguous mutation.
func (m *Manager) cancelProviderPendingIdempotent(
	ctx context.Context,
	r Record,
	operationID string,
) error {
	if r.CustomerID == "" {
		return errors.New("lifecycle: pending provider cleanup has no customer id")
	}
	name, provider, err := m.providerFor(r)
	if err != nil {
		return err
	}
	idempotent, ok := provider.(billing.IdempotentPendingCanceller)
	if !ok {
		return fmt.Errorf(
			"billing provider %q does not support idempotent pending cancellation",
			name)
	}
	return idempotent.CancelPendingIdempotent(ctx, r.CustomerID, operationID)
}

// cancelCurrentPending honors the durable replacement-cleanup phase even when
// the current claim is a contact lead. A plain contact has no provider state;
// a contact with CancelPrevious is the only remaining fence for the provider
// operation it replaced and must not be skipped.
func (m *Manager) cancelCurrentPending(ctx context.Context, r Record) error {
	if r.Pending != nil && r.Pending.CancelPrevious {
		return m.cancelProviderPending(ctx, r)
	}
	return m.cancelReplacedPending(ctx, r)
}

func (m *Manager) cancelCurrentPendingIdempotent(
	ctx context.Context,
	r Record,
	operationID string,
) error {
	if r.Pending == nil ||
		(r.Pending.Kind == PendingContact && !r.Pending.CancelPrevious) {
		return nil
	}
	return m.cancelProviderPendingIdempotent(ctx, r, operationID)
}

// RequestUpgrade starts an upgrade to plan. Outcomes: "done" (charged on file
// and applied), "action" (complete at URL), or "contact" (tier not self-serve;
// lead recorded). A new request replaces any pending change — including its
// provider-side state.
func (m *Manager) RequestUpgrade(ctx context.Context, accountID, email, planID string) (Outcome, error) {
	operationID, err := newBillingOperationID()
	if err != nil {
		return Outcome{}, err
	}
	return m.requestUpgrade(
		ctx, accountID, email, planID, operationID, false,
		billingMutationApproval{})
}

func (m *Manager) requestUpgrade(
	ctx context.Context,
	accountID, email, planID, operationID string,
	strictOperation bool,
	approval billingMutationApproval,
) (Outcome, error) {
	if !m.BillingAvailable() {
		return Outcome{}, ErrBillingUnavailable
	}
	target, ok := m.cfg.Catalog.Get(planID)
	if !ok {
		return Outcome{}, refuse("unknown plan %q", planID)
	}
	approvedContact := strictOperation &&
		approval.ExecutionClass == BillingMutationExecutionUpgradeContact
	approvedSelfServe := strictOperation &&
		approval.ExecutionClass == BillingMutationExecutionUpgradeSelfServe
	if validBillingMutationExecutionClass(approval.ExecutionClass) {
		switch {
		case !approvedContact && !approvedSelfServe:
			return Outcome{}, ErrBillingMutationApprovalDrift
		case approvedContact &&
			(approval.ApprovedPriceCents != 0 || approval.ApprovedCurrency != ""):
			return Outcome{}, ErrBillingMutationApprovalDrift
		case approvedSelfServe &&
			(!target.Purchasable() ||
				target.PriceCents() != approval.ApprovedPriceCents ||
				strings.ToLower(m.cfg.Catalog.Currency) != approval.ApprovedCurrency):
			return Outcome{}, ErrBillingMutationApprovalDrift
		}
	}
	now := m.cfg.Now()

	// Claim: validate against the freshest record and park the desired change
	// BEFORE any provider call, so a concurrent request cannot double-charge —
	// it either sees our claim (and replaces it, failing our fold) or we see
	// its state. The claim also captures what pending change it replaces.
	reused := false
	pinnedUpgrade := approvedSelfServe
	claim, err := m.mutate(ctx, accountID, email, func(r *Record) error {
		pinnedUpgrade = approvedSelfServe
		current, _ := m.cfg.Catalog.Get(r.Entitled)
		if target.ID == r.Entitled {
			return refuse("already on the %s plan", target.ID)
		}
		if strictOperation && r.Pending != nil &&
			r.Pending.OperationID == operationID &&
			r.Pending.Plan == target.ID {
			switch r.Pending.Kind {
			case PendingContact:
				if approvedSelfServe {
					return ErrBillingMutationConflict
				}
				reused = true
				return errSkipWrite
			case PendingUpgrade:
				if approvedContact {
					return ErrBillingMutationConflict
				}
				// Pending.Kind is the durable execution classification approved
				// before the provider call. Resume it even if a later catalog
				// deployment changes availability or pricing semantics.
				pinnedUpgrade = true
				if !r.Pending.Expires.IsZero() && now.Before(r.Pending.Expires) {
					reused = true
					return errSkipWrite
				}
			}
		}
		if approvedContact || !target.Available && !pinnedUpgrade {
			// Not self-serve yet: the stored desire IS the sales lead; the
			// hoop is "talk to us" instead of "pay". This check precedes
			// price ordering because custom-priced Enterprise intentionally
			// has no catalog amount.
			if pending := r.Pending; pending != nil &&
				pending.Kind == PendingContact && pending.Plan == target.ID &&
				pending.OperationID != "" {
				if strictOperation && pending.OperationID != operationID {
					return refuse("another request is already pending for plan %s", target.ID)
				}
				reused = true
				return errSkipWrite
			}
			cancelPrevious := r.Pending != nil &&
				(r.Pending.CancelPrevious ||
					(r.Pending.Kind != PendingContact && r.CustomerID != ""))
			r.Pending = &Pending{
				Kind: PendingContact, Plan: target.ID, OperationID: operationID,
				CancelPrevious: cancelPrevious,
				Requested:      now,
			}
			return nil
		}
		if !pinnedUpgrade && target.PriceCents() <= current.PriceCents() {
			return refuse("%s is not an upgrade from %s — use downgrade", target.ID, current.ID)
		}
		if !pinnedUpgrade && !target.Purchasable() {
			return refuse("plan %q is not purchasable", target.ID)
		}
		providerName, provider, err := m.providerFor(*r)
		if err != nil {
			return err
		}
		if _, ok := provider.(billing.IdempotentSubscriber); !ok {
			return fmt.Errorf(
				"billing provider %q does not support idempotent subscription operations",
				providerName)
		}
		if pending := r.Pending; pending != nil &&
			pending.Kind == PendingUpgrade && pending.Plan == target.ID &&
			pending.OperationID != "" && !pending.Expires.IsZero() &&
			now.Before(pending.Expires) {
			if strictOperation && pending.OperationID != operationID {
				return refuse("another request is already pending for plan %s", target.ID)
			}
			// This is a retry/resume of the same unfinished purchase. Reuse the
			// durable operation rather than canceling it and manufacturing a
			// second Checkout Session/idempotency identity.
			reused = true
			return errSkipWrite
		}
		cancelPrevious := r.Pending != nil &&
			(r.Pending.CancelPrevious ||
				(r.Pending.Kind != PendingContact && r.CustomerID != ""))
		r.Pending = &Pending{
			Kind: PendingUpgrade, Plan: target.ID, OperationID: operationID,
			CancelPrevious: cancelPrevious,
			Expires:        now.Add(m.cfg.TTL), Requested: now,
		}
		return nil
	})
	if err != nil {
		return Outcome{}, err
	}
	if claim.Pending.CancelPrevious {
		// This phase is durable. On failure (or a crash before the CAS below),
		// the same claim retries cancellation. Once the flag is cleared, later
		// ambiguous Subscribe errors retain the operation and never return to
		// this destructive phase.
		var cancelErr error
		if strictOperation {
			cancelErr = m.cancelProviderPendingIdempotent(
				ctx, claim, claim.Pending.OperationID+"-previous")
		} else {
			cancelErr = m.cancelProviderPending(ctx, claim)
		}
		if cancelErr != nil {
			return Outcome{}, cancelErr
		}
		claim, err = m.mutate(ctx, accountID, email, func(r *Record) error {
			p := r.Pending
			if p == nil || p.OperationID != claim.Pending.OperationID ||
				p.Kind != claim.Pending.Kind || p.Plan != claim.Pending.Plan {
				return refuse("upgrade to %s was superseded by another request", target.ID)
			}
			if !p.CancelPrevious {
				return errSkipWrite
			}
			p.CancelPrevious = false
			return nil
		})
		if err != nil {
			return Outcome{}, err
		}
	}
	if claim.Pending.Kind == PendingContact {
		return Outcome{Kind: "contact", Plan: target.ID}, nil
	}
	if reused && claim.Pending.URL != "" {
		return Outcome{
			Kind: "action", Plan: target.ID, URL: claim.Pending.URL,
		}, nil
	}

	// Provider calls remain outside every CAS retry loop. The durable operation
	// identity makes a whole-request retry safe when Subscribe's response is
	// ambiguous; providers implementing IdempotentSubscriber replay it.
	name, provider, err := m.providerFor(claim)
	if err != nil {
		return Outcome{}, err
	}
	customerID := claim.CustomerID
	if customerID == "" {
		if customerID, err = provider.EnsureCustomer(ctx, accountID, email); err != nil {
			return Outcome{}, err
		}
		// Persist the provider/customer routing index BEFORE Checkout can emit
		// a paid callback. A callback racing the HTTP response can now route on
		// its first delivery instead of depending on the provider retry horizon.
		claim, err = m.mutate(ctx, accountID, email, func(r *Record) error {
			p := r.Pending
			if p == nil || p.Kind != PendingUpgrade ||
				p.OperationID != claim.Pending.OperationID {
				return refuse("upgrade to %s was superseded by another request", target.ID)
			}
			if r.Provider != "" && r.Provider != name {
				return fmt.Errorf(
					"account %s was concurrently pinned to provider %q",
					accountID, r.Provider)
			}
			if r.CustomerID != "" {
				return errSkipWrite // a concurrent setup/customer fold won
			}
			r.Provider = name
			r.CustomerID = customerID
			return nil
		})
		if err != nil {
			return Outcome{}, err
		}
		name, provider, err = m.providerFor(claim)
		if err != nil {
			return Outcome{}, err
		}
		customerID = claim.CustomerID
	}
	idempotent, ok := provider.(billing.IdempotentSubscriber)
	if !ok {
		// The claim-time check should make this unreachable unless provider
		// routing/configuration changed inside one request. Keep it fail-closed.
		return Outcome{}, fmt.Errorf(
			"billing provider %q lost idempotent subscription support", name)
	}
	act, err := idempotent.SubscribeIdempotent(
		ctx, customerID, target.ID, claim.Pending.OperationID)
	if err != nil {
		// Keep the durable operation parked. A transport error can arrive after
		// the provider committed Checkout; clearing it here would discard the
		// only idempotency identity and make the user's retry a second purchase.
		return Outcome{}, err
	}
	validActionURL := act.URL != "" && validBillingMutationURL(act.URL)
	if (act.Done && act.URL != "") || (!act.Done && !validActionURL) {
		return Outcome{}, invalidProviderAction("subscription")
	}

	// Fold: land the provider outcome on the freshest record — but only if
	// our claim is still the pending change (a concurrent request may have
	// replaced it, in which case its provider state superseded ours).
	folded, err := m.mutate(ctx, accountID, email, func(r *Record) error {
		p := r.Pending
		if p == nil || p.Kind != PendingUpgrade ||
			p.OperationID != claim.Pending.OperationID {
			return refuse("upgrade to %s was superseded by another request", target.ID)
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
		// Billing is already durable. apply records its own pending/error
		// fence for reconciliation, so a transient cell failure must not
		// make the caller retry the provider mutation.
		_ = m.apply(ctx, folded.AccountID)
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
	operationID, err := newBillingOperationID()
	if err != nil {
		return Outcome{}, err
	}
	return m.requestDowngrade(
		ctx, accountID, email, planID, operationID, false,
		billingMutationApproval{})
}

func (m *Manager) requestDowngrade(
	ctx context.Context,
	accountID, email, planID, operationID string,
	strictOperation bool,
	approval billingMutationApproval,
) (Outcome, error) {
	if !m.BillingAvailable() {
		return Outcome{}, ErrBillingUnavailable
	}
	target, ok := m.cfg.Catalog.Get(planID)
	if !ok {
		return Outcome{}, refuse("unknown plan %q", planID)
	}
	approvedDowngrade := strictOperation &&
		approval.ExecutionClass == BillingMutationExecutionDowngrade
	if validBillingMutationExecutionClass(approval.ExecutionClass) &&
		(!approvedDowngrade ||
			target.PriceCents() != approval.ApprovedPriceCents ||
			strings.ToLower(m.cfg.Catalog.Currency) != approval.ApprovedCurrency) {
		return Outcome{}, ErrBillingMutationApprovalDrift
	}
	now := m.cfg.Now()

	reused := false
	claim, err := m.mutate(ctx, accountID, email, func(r *Record) error {
		if strictOperation && r.Pending != nil &&
			r.Pending.Kind == PendingDowngrade &&
			r.Pending.Plan == target.ID &&
			r.Pending.OperationID == operationID {
			// The durable Pending classification proves this operation already
			// passed direction and advisory-fit checks before its provider call.
			// A recovery retry must replay that exact operation even if mutable
			// usage or catalog pricing changed after an ambiguous provider result.
			reused = true
			return errSkipWrite
		}
		if !approvedDowngrade {
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
		}
		cancelPrevious := r.Pending != nil &&
			(r.Pending.CancelPrevious ||
				(r.Pending.Kind != PendingContact && r.CustomerID != ""))
		r.Pending = &Pending{
			Kind: PendingDowngrade, Plan: target.ID,
			OperationID: operationID, CancelPrevious: cancelPrevious,
			Requested: now,
		}
		return nil
	})
	if err != nil {
		return Outcome{}, err
	}
	if claim.Pending.CancelPrevious {
		var cancelErr error
		if strictOperation {
			cancelErr = m.cancelProviderPendingIdempotent(
				ctx, claim, claim.Pending.OperationID+"-previous")
		} else {
			cancelErr = m.cancelProviderPending(ctx, claim)
		}
		if cancelErr != nil {
			return Outcome{}, cancelErr
		}
		claim, err = m.mutate(ctx, accountID, email, func(r *Record) error {
			p := r.Pending
			if p == nil || p.OperationID != claim.Pending.OperationID ||
				p.Kind != PendingDowngrade || p.Plan != target.ID {
				return refuse("downgrade to %s was superseded by another request", target.ID)
			}
			if !p.CancelPrevious {
				return errSkipWrite
			}
			p.CancelPrevious = false
			return nil
		})
		if err != nil {
			return Outcome{}, err
		}
	}
	if reused && !claim.Pending.Effective.IsZero() {
		return Outcome{
			Kind: "scheduled", Plan: target.ID,
			Effective: claim.Pending.Effective,
		}, nil
	}

	name, provider, err := m.providerFor(claim)
	if err != nil {
		return Outcome{}, err
	}
	var effective time.Time
	if strictOperation {
		idempotent, ok := provider.(billing.IdempotentDowngrader)
		if !ok {
			return Outcome{}, fmt.Errorf(
				"billing provider %q does not support idempotent downgrade operations",
				name)
		}
		effective, err = idempotent.ScheduleDowngradeIdempotent(
			ctx, claim.CustomerID, target.ID, operationID)
	} else {
		effective, err = provider.ScheduleDowngrade(ctx, claim.CustomerID, target.ID)
	}
	if err != nil {
		// A durable-envelope retry must retain the operation identity after an
		// ambiguous provider response. Legacy direct calls keep their historical
		// best-effort release behavior.
		if !strictOperation {
			m.releaseClaim(ctx, accountID, claim)
		}
		return Outcome{}, err
	}
	if effective.IsZero() {
		return Outcome{}, invalidProviderAction("downgrade")
	}
	if _, err := m.mutate(ctx, accountID, email, func(r *Record) error {
		p := r.Pending
		if p == nil || p.Kind != PendingDowngrade || p.Plan != target.ID ||
			p.OperationID != operationID {
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
	_, err := m.cancelPending(ctx, accountID, "", false)
	return err
}

// cancelPendingMutation cancels an in-flight change for an already-claimed
// schema-2 receipt. It is intentionally private so the operation identity
// cannot be used without the receipt's immutable approval and retry guards.
func (m *Manager) cancelPendingMutation(
	ctx context.Context,
	accountID, operationID string,
) (resolved bool, err error) {
	if !validBillingOperationID(operationID) {
		return false, errors.New("lifecycle: invalid billing operation id")
	}
	return m.cancelPending(ctx, accountID, operationID, true)
}

func (m *Manager) cancelPending(
	ctx context.Context,
	accountID, operationID string,
	strictOperation bool,
) (resolved bool, err error) {
	if !m.BillingAvailable() {
		return false, ErrBillingUnavailable
	}
	// Disarm the provider FIRST: if its cancel fails, the record still shows
	// the pending change (truthful, retryable). The old order cleared local
	// state first, so a provider blip left a schedule armed at the partner
	// with nothing visible locally — a downgrade would fire "spontaneously"
	// at period end with no API path left to disarm it.
	r, err := m.load(ctx, accountID, "")
	if err != nil {
		return false, err
	}
	if r.Pending == nil {
		if strictOperation {
			resolved = true
			_, err := m.mutate(ctx, accountID, "", func(r *Record) error {
				if r.LastBillingMutationOperationID == operationID &&
					r.LastBillingMutationKind == BillingMutationPlanCancel {
					switch r.LastBillingMutationResultKind {
					case BillingMutationResultCancelled, "":
						resolved = false
					case BillingMutationResultResolved:
						resolved = true
					default:
						return ErrBillingMutationConflict
					}
					return errSkipWrite
				}
				setBillingMutationTombstone(
					r, operationID, BillingMutationPlanCancel,
					BillingMutationResultResolved, "", time.Time{}, m.cfg.Now().UTC())
				return nil
			})
			return resolved, err
		}
		return false, refuse("nothing is pending")
	}
	expected := *r.Pending
	var cancelErr error
	if strictOperation {
		cancelErr = m.cancelCurrentPendingIdempotent(ctx, r, operationID)
	} else {
		cancelErr = m.cancelCurrentPending(ctx, r)
	}
	if cancelErr != nil {
		return false, cancelErr
	}
	_, err = m.mutate(ctx, accountID, "", func(r *Record) error {
		if r.Pending == nil {
			// The change resolved while we were disarming (e.g. the payer
			// completed checkout): nothing to clear, entitlement stands.
			if strictOperation {
				resolved = true
				setBillingMutationTombstone(
					r, operationID, BillingMutationPlanCancel,
					BillingMutationResultResolved, "", time.Time{}, m.cfg.Now().UTC())
				return nil
			}
			return refuse("nothing is pending")
		}
		if r.Pending.OperationID != expected.OperationID ||
			r.Pending.Kind != expected.Kind || r.Pending.Plan != expected.Plan ||
			!r.Pending.Requested.Equal(expected.Requested) {
			if strictOperation {
				resolved = true
				setBillingMutationTombstone(
					r, operationID, BillingMutationPlanCancel,
					BillingMutationResultResolved, "", time.Time{}, m.cfg.Now().UTC())
				return nil
			}
			return refuse("pending change was superseded; retry cancel")
		}
		if strictOperation {
			resolved = false
			setBillingMutationTombstone(
				r, operationID, BillingMutationPlanCancel,
				BillingMutationResultCancelled, "", time.Time{}, m.cfg.Now().UTC())
		}
		r.Pending = nil
		return nil
	})
	return resolved, err
}

func setBillingMutationTombstone(
	r *Record,
	operationID string,
	operation BillingMutationOperation,
	resultKind BillingMutationResultKind,
	plan string,
	effective, at time.Time,
) {
	r.LastBillingMutationOperationID = operationID
	r.LastBillingMutationKind = operation
	r.LastBillingMutationResultKind = resultKind
	r.LastBillingMutationPlan = plan
	r.LastBillingMutationEffective = effective.UTC()
	r.LastBillingMutationAt = at.UTC()
}

// OnEvents durably receives and then folds normalized provider events into
// records. provider names the configured partner whose webhook delivered
// them; lookups are scoped to it so customer ids from different partners can
// never cross-match.
//
// Providers may emit at most one normalized Event for one ProviderEventID.
// Stripe's mapping is deliberately zero-or-one. This invariant keeps the
// durable receipt identity unambiguous and is enforced before any batch side
// effects. Legacy in-process events may omit both ProviderEventID and
// PayloadSHA256; externally delivered events must supply both.
func (m *Manager) OnEvents(ctx context.Context, provider string, events []billing.Event) error {
	seen := make(map[string]struct{}, len(events))
	for _, event := range events {
		hasID := event.ProviderEventID != ""
		hasHash := event.PayloadSHA256 != ""
		if hasID != hasHash {
			return errors.New("billing event identity requires both provider event id and payload sha256")
		}
		if !hasID {
			continue
		}
		if _, duplicate := seen[event.ProviderEventID]; duplicate {
			return fmt.Errorf(
				"provider %s emitted more than one normalized event for provider event %s",
				provider, event.ProviderEventID)
		}
		seen[event.ProviderEventID] = struct{}{}
	}

	for _, event := range events {
		if event.ProviderEventID == "" {
			if _, err := m.processEvent(ctx, provider, event, false); err != nil {
				return err
			}
			continue
		}
		store, ok := m.cfg.Store.(EventReceiptStore)
		if !ok {
			return errors.New(
				"billing event has durable identity but lifecycle store has no event receipt support")
		}
		receipt, err := newEventReceipt(provider, event, m.cfg.Now())
		if err != nil {
			return err
		}
		stored, _, err := store.ReceiveEvent(ctx, receipt)
		if err != nil {
			return err
		}
		if err := m.processReceipt(ctx, store, stored); err != nil {
			return err
		}
	}
	return nil
}

// processReceipt completes a durable receipt only after its normalized event
// is safely folded or deliberately ignored. A processed receipt may remain in
// an R2 pending index when the completion write succeeded but index cleanup
// failed; repeating CompleteEvent repairs only that index and never refolds.
func (m *Manager) processReceipt(
	ctx context.Context,
	store EventReceiptStore,
	receipt EventReceipt,
) error {
	if err := validateEventReceipt(receipt); err != nil {
		return fmt.Errorf("billing event store returned an invalid receipt: %w", err)
	}
	if receipt.Status == EventReceiptProcessed {
		return store.CompleteEvent(
			ctx, receipt, *receipt.ProcessedAt)
	}
	claimToken, err := newEventReceiptClaimToken()
	if err != nil {
		return err
	}
	now := m.cfg.Now()
	claimed, acquired, err := store.ClaimEvent(
		ctx, receipt, claimToken, now, now.Add(eventReceiptProcessingLease))
	if err != nil {
		return err
	}
	// Another worker may have completed between ReceiveEvent/PendingEvents and
	// this claim attempt. Repair only its pending-index cleanup; never refold.
	if claimed.Status == EventReceiptProcessed {
		return store.CompleteEvent(
			ctx, claimed, *claimed.ProcessedAt)
	}
	if !acquired {
		return ErrEventReceiptInProgress
	}
	// Bound all resolution, fold, and completion work to the exact persisted
	// lease rather than granting a fresh window after claim acquisition latency.
	remaining := claimed.LeaseExpiresAt.Sub(m.cfg.Now())
	if remaining <= 0 {
		return releaseEventReceiptAfterError(ctx, store, claimed, errors.New(
			"event receipt: processing lease expired before resolution"))
	}
	processCtx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	if claimed.Resolution == nil {
		resolution, err := m.resolveEvent(
			processCtx, claimed.Provider, claimed.Event, true)
		if err != nil {
			return releaseEventReceiptAfterError(ctx, store, claimed, err)
		}
		claimed, err = store.PinEventResolution(
			processCtx, claimed, resolution)
		if err != nil {
			return releaseEventReceiptAfterError(ctx, store, claimed, err)
		}
	}
	if _, err := m.foldEventResolution(processCtx, *claimed.Resolution); err != nil {
		return releaseEventReceiptAfterError(ctx, store, claimed, err)
	}
	if err := store.CompleteEvent(processCtx, claimed, m.cfg.Now()); err != nil {
		return releaseEventReceiptAfterError(ctx, store, claimed, err)
	}
	return nil
}

func releaseEventReceiptAfterError(
	ctx context.Context,
	store EventReceiptStore,
	claimed EventReceipt,
	processErr error,
) error {
	if releaseErr := store.ReleaseEvent(ctx, claimed); releaseErr != nil {
		return errors.Join(processErr, fmt.Errorf(
			"event receipt: release processing claim: %w", releaseErr))
	}
	return processErr
}

// resolveEvent performs all provider, routing, and catalog reads before an
// account mutation. Durable callers pin its exact result to their receipt;
// recovery then folds that saved account/event/decision without resolving it
// again against provider or routing state that may have changed.
func (m *Manager) resolveEvent(
	ctx context.Context,
	provider string,
	event billing.Event,
	durable bool,
) (EventReceiptResolution, error) {
	resolvedAt := m.cfg.Now().UTC()
	configured, ok := m.cfg.Providers[provider]
	if !ok {
		return EventReceiptResolution{}, fmt.Errorf(
			"billing event provider %q is not configured", provider)
	}
	seed, ok, err := m.cfg.Store.ByCustomer(ctx, provider, event.CustomerID)
	if err != nil {
		return EventReceiptResolution{}, err
	}
	if !ok {
		if durable {
			return EventReceiptResolution{
				Decision: "ignored", IgnoreReason: "unknown_customer",
				ResolvedAt: resolvedAt,
			}, nil
		}
		return EventReceiptResolution{}, fmt.Errorf(
			"event %s for unknown %s customer — will resolve on redelivery",
			event.Type, provider)
	}

	if resolver, ok := configured.(billing.EventResolver); ok {
		resolved, err := resolver.ResolveEvent(ctx, event)
		if err != nil {
			return EventReceiptResolution{}, err
		}
		if resolved == nil {
			return EventReceiptResolution{
				Decision: "ignored", IgnoreReason: "provider_noop",
				AccountID: seed.AccountID, ResolvedAt: resolvedAt,
			}, nil
		}
		if resolved.ProviderEventID != event.ProviderEventID ||
			resolved.PayloadSHA256 != event.PayloadSHA256 ||
			resolved.CustomerID != event.CustomerID {
			return EventReceiptResolution{}, errors.New(
				"billing event resolver changed durable event identity or customer")
		}
		event = *resolved
	}

	// An activation for a plan this catalog does not know is deploy skew (the
	// partner sells something this binary has not heard of). Keep its receipt
	// pending until the catalog catches up rather than losing a paid event.
	if event.Type == billing.EventSubscriptionActivated {
		if _, ok := m.cfg.Catalog.Get(event.Plan); !ok {
			return EventReceiptResolution{}, fmt.Errorf(
				"activation for plan %q not in this catalog — will resolve after deploy",
				event.Plan)
		}
	}
	switch event.Type {
	case billing.EventSubscriptionActivated,
		billing.EventSubscriptionCanceled,
		billing.EventPaymentFailed,
		billing.EventPaymentRecovered:
	default:
		return EventReceiptResolution{
			Decision: "ignored", IgnoreReason: "unsupported_event",
			AccountID: seed.AccountID, ResolvedAt: resolvedAt,
		}, nil
	}
	resolved := event
	return EventReceiptResolution{
		Decision: "applied", AccountID: seed.AccountID,
		Event: &resolved, ResolvedAt: resolvedAt,
	}, nil
}

// foldEventResolution applies only a previously selected resolution. It does
// no provider or routing reads, which makes crash recovery deterministic.
func (m *Manager) foldEventResolution(
	ctx context.Context,
	resolution EventReceiptResolution,
) (string, error) {
	if resolution.Decision == "ignored" {
		return "ignored", nil
	}
	if resolution.Decision != "applied" || resolution.AccountID == "" ||
		resolution.Event == nil {
		return "", errors.New("billing event has invalid pinned resolution")
	}
	event := *resolution.Event
	r, err := m.mutate(ctx, resolution.AccountID, "", func(r *Record) error {
		switch event.Type {
		case billing.EventSubscriptionActivated:
			if !event.At.IsZero() && event.At.Before(r.EntitledAt) {
				return errSkipWrite // stale: predates the current entitlement
			}
			r.Entitled = event.Plan
			r.EntitledAt = event.At
			r.PastDueSince = nil
			if p := r.Pending; p != nil && p.Plan == event.Plan {
				switch p.Kind {
				case PendingUpgrade:
					// A checkout callback belongs to one durable operation. A
					// delayed callback from superseded operation A may confirm
					// entitlement, but it must not erase newer pending operation B
					// just because both targeted the same plan.
					if event.OperationID != "" &&
						p.OperationID == event.OperationID {
						r.Pending = nil
					}
				case PendingDowngrade:
					// Persist the exact result before clearing Pending. This closes
					// the crash gap where the provider committed a schedule, its
					// response was lost, and the period-end webhook arrives before
					// the durable receipt can be completed.
					effective := p.Effective
					if effective.IsZero() {
						effective = event.At
					}
					if effective.IsZero() {
						effective = resolution.ResolvedAt
					}
					if validBillingOperationID(p.OperationID) && !effective.IsZero() {
						setBillingMutationTombstone(
							r, p.OperationID, BillingMutationPlanDowngrade,
							BillingMutationResultScheduled, p.Plan, effective,
							m.cfg.Now().UTC())
					}
					r.Pending = nil
				}
			}
		case billing.EventSubscriptionCanceled:
			if !event.At.IsZero() && event.At.Before(r.EntitledAt) {
				return errSkipWrite // stale cancel of an older subscription
			}
			r.Entitled = plans.Free
			r.EntitledAt = event.At
			// Terminal states clear dunning: there is no longer a failing
			// renewal to be past due on.
			r.PastDueSince = nil
			if p := r.Pending; p != nil && p.Kind == PendingDowngrade {
				if p.Plan == plans.Free {
					effective := p.Effective
					if effective.IsZero() {
						effective = event.At
					}
					if effective.IsZero() {
						effective = resolution.ResolvedAt
					}
					if validBillingOperationID(p.OperationID) && !effective.IsZero() {
						setBillingMutationTombstone(
							r, p.OperationID, BillingMutationPlanDowngrade,
							BillingMutationResultScheduled, p.Plan, effective,
							m.cfg.Now().UTC())
					}
				}
				r.Pending = nil
			}
		case billing.EventPaymentFailed:
			if !event.At.IsZero() && event.At.Before(r.DunningAt) {
				return errSkipWrite // stale: predates newer dunning state
			}
			if r.PastDueSince == nil {
				t := event.At
				r.PastDueSince = &t
			}
			r.DunningAt = event.At
		case billing.EventPaymentRecovered:
			if !event.At.IsZero() && event.At.Before(r.DunningAt) {
				return errSkipWrite
			}
			r.PastDueSince = nil
			r.DunningAt = event.At
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	// The provider event is already folded. Keep webhook processing successful
	// and let the durable apply fence reconcile any transient cell failure.
	_ = m.apply(ctx, r.AccountID)
	return "applied", nil
}

// processEvent retains the legacy identity-less path. Durable receipt callers
// invoke resolveEvent, pin the result, and only then foldEventResolution.
func (m *Manager) processEvent(
	ctx context.Context,
	provider string,
	event billing.Event,
	durable bool,
) (string, error) {
	resolution, err := m.resolveEvent(ctx, provider, event, durable)
	if err != nil {
		return "", err
	}
	return m.foldEventResolution(ctx, resolution)
}

// ReconcileAccount expires a lapsed upgrade request and retries one account's
// exact cell snapshot. Directory-driven callers use this bounded form so a
// control-plane tick performs no all-account R2 scan.
func (m *Manager) ReconcileAccount(ctx context.Context, accountID string) error {
	return m.reconcileAccount(ctx, accountID, m.cfg.Now())
}

func (m *Manager) reconcileAccount(ctx context.Context, accountID string, now time.Time) error {
	r, err := m.load(ctx, accountID, "")
	if err != nil {
		return err
	}
	var firstErr error
	// Replay only this account's bounded provider/customer receipt index. No
	// bucket-wide receipt scan is needed. A malformed/missing indexed receipt
	// or a receipt whose fold fails is retained as the first error, but it does
	// not starve the other bounded receipts for this customer.
	if receiptStore, ok := m.cfg.Store.(EventReceiptStore); ok &&
		r.Provider != "" && r.CustomerID != "" {
		receipts, receiptErr := receiptStore.PendingEvents(
			ctx, r.Provider, r.CustomerID, maxEventReceiptsPerReconcile)
		if receiptErr != nil {
			firstErr = receiptErr
		}
		for _, receipt := range receipts {
			if receiptErr := m.processReceipt(ctx, receiptStore, receipt); receiptErr != nil &&
				firstErr == nil {
				firstErr = receiptErr
			}
		}
		// A receipt may have changed entitlement or dunning state.
		r, err = m.load(ctx, accountID, "")
		if err != nil {
			return err
		}
	}
	if p := r.Pending; p != nil && p.CancelPrevious {
		// Replacement cleanup is its own durable phase. Retry it for every
		// pending kind, including contact leads, then clear only the exact
		// operation fence observed before the provider call.
		expected := *p
		if cleanupErr := m.cancelProviderPending(ctx, r); cleanupErr != nil {
			if firstErr == nil {
				firstErr = cleanupErr
			}
		} else {
			r, err = m.mutate(ctx, accountID, "", func(r *Record) error {
				current := r.Pending
				if current == nil ||
					current.OperationID != expected.OperationID ||
					current.Kind != expected.Kind || current.Plan != expected.Plan ||
					!current.Requested.Equal(expected.Requested) ||
					!current.CancelPrevious {
					return errSkipWrite
				}
				current.CancelPrevious = false
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	if p := r.Pending; p != nil &&
		p.Kind == PendingUpgrade && !p.CancelPrevious && now.After(p.Expires) {
		// Provider state is disarmed BEFORE the local pending marker is
		// cleared. In providerless recovery mode a pinned legacy record
		// therefore remains truthful and retryable until its provider is
		// configured again.
		if err := m.cancelReplacedPending(ctx, r); err != nil {
			firstErr = err
		} else {
			expected := *p
			r, err = m.mutate(ctx, accountID, "", func(r *Record) error {
				current := r.Pending
				if current == nil ||
					current.Kind != expected.Kind ||
					current.Plan != expected.Plan ||
					!current.Requested.Equal(expected.Requested) ||
					!now.After(current.Expires) {
					return errSkipWrite
				}
				r.Pending = nil
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	if err := m.apply(ctx, r.AccountID); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Reconcile is the compatibility full sweep used by standalone callers. The
// hosted directory reconciler uses ReconcileAccount through EnsureAccount so
// each tick remains bounded to one directory page.
func (m *Manager) Reconcile(ctx context.Context) error {
	records, err := m.cfg.Store.List(ctx)
	if err != nil {
		return err
	}
	now := m.cfg.Now()
	var firstErr error
	for _, snapshot := range records {
		if err := m.reconcileAccount(ctx, snapshot.AccountID, now); err != nil &&
			firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// apply converges the cell onto the effective plan. For downgrades it runs the
// AUTHORITATIVE fit check first — the advisory one ran at request time, but
// usage kept moving while the hoops were jumped — and refuses to push a
// snapshot the account no longer fits: the cell keeps enforcing the old plan
// (in the customer's favor), the gap stays visible as Entitled != Applied plus
// ApplyBlocked, and every Reconcile retries until the account is pruned.
// Push failures are likewise left pending for Reconcile — the one state that
// never rests — and are returned to the caller for aggregate observability.
func (m *Manager) apply(ctx context.Context, accountID string) error {
	// Serialize per account. See applyMuMap on Manager.
	mu := m.perAccountApplyMu(accountID)
	mu.Lock()
	defer mu.Unlock()
	r, err := m.load(ctx, accountID, "")
	if err != nil {
		return err
	}
	if r.Version == 0 {
		return fmt.Errorf("lifecycle: account %s has no stored plan record", accountID)
	}
	// Read only the cell's acknowledgement fence. A restored lifecycle store
	// can be behind the cell, but the cell payload is not entitlement
	// authority and is never adopted. Instead we advance above the observed
	// revision and reapply our own resolved snapshot.
	observed := ApplyFence{
		Revision: r.AppliedSnapshotRevision,
		Hash:     r.AppliedSnapshotHash,
	}
	if reader, ok := m.cfg.Applier.(ApplyFenceReader); ok {
		observed, err = reader.ReadApplyFence(ctx, accountID)
		if err != nil {
			return fmt.Errorf("read cell plan fence: %w", err)
		}
		if observed.Revision < 0 ||
			(observed.Revision == 0 && observed.Hash != "") ||
			(observed.Revision > 0 && observed.Hash == "") {
			return errors.New("cell returned an invalid plan snapshot fence")
		}
	}
	// Mint a monotonic apply revision whenever the resolved snapshot changes
	// OR the cell fence differs from the record's acknowledged state. The
	// latter is the R2 rollback/cell rollback recovery path.
	r, err = m.mutate(ctx, accountID, "", func(r *Record) error {
		current, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		cellMatchesRecord := observed.Revision == r.AppliedSnapshotRevision &&
			observed.Hash == r.AppliedSnapshotHash
		if !SnapshotApplyPending(*r, current) && cellMatchesRecord {
			return errSkipWrite
		}
		// A previously minted desired revision that is already above the
		// observed cell fence remains safe to retry verbatim. An exact observed
		// desired fence is also replayed verbatim: the cell may have accepted the
		// write while the bridge lost its post-cell policy completion or response.
		// Replaying the cell's idempotent revision/hash finishes those hooks
		// without burning a new R2 version/revision.
		cellAcceptedDesiredFence :=
			r.DesiredSnapshotHash == current.Hash &&
				r.SnapshotRevision == observed.Revision &&
				observed.Hash == current.Hash
		if r.DesiredSnapshotHash == current.Hash &&
			(r.SnapshotRevision > observed.Revision ||
				cellAcceptedDesiredFence) {
			return errSkipWrite
		}
		maxRevision := r.SnapshotRevision
		if observed.Revision > maxRevision {
			maxRevision = observed.Revision
		}
		if maxRevision == int64(^uint64(0)>>1) {
			return errors.New("lifecycle: plan snapshot revision exhausted")
		}
		r.SnapshotRevision = maxRevision + 1
		r.DesiredSnapshotHash = current.Hash
		return nil
	})
	if err != nil {
		return err
	}
	snapshot, err := m.resolveSnapshot(r)
	if err != nil {
		return err
	}
	cellMatchesRecord := observed.Revision == r.AppliedSnapshotRevision &&
		observed.Hash == r.AppliedSnapshotHash
	if !SnapshotApplyPending(r, snapshot) && cellMatchesRecord {
		return nil
	}
	target, ok := m.cfg.Catalog.Get(snapshot.Plan)
	if !ok {
		return fmt.Errorf("effective plan %q is not in the catalog", snapshot.Plan)
	}
	targetRank, targetRankOK := m.planRank(target.ID)
	appliedRank, appliedRankOK := m.planRank(r.Applied)
	if !targetRankOK || !appliedRankOK {
		return fmt.Errorf("cannot order applied plan %q and target plan %q", r.Applied, target.ID)
	}
	cellAcceptedDesiredFence :=
		r.DesiredSnapshotHash == snapshot.Hash &&
			r.SnapshotRevision == observed.Revision &&
			observed.Hash == snapshot.Hash
	// Do not let a fresh fit violation wedge completion after the cell already
	// accepted this exact downgrade. The exact replay changes no cell policy; it
	// only gives the bridge another chance to finish its fenced projections.
	if targetRank < appliedRank && !cellAcceptedDesiredFence {
		violations, err := m.cfg.Fit.Fit(ctx, accountID, target)
		if err != nil {
			return err
		}
		if len(violations) > 0 {
			_, _ = m.mutate(ctx, accountID, "", func(r *Record) error {
				report := strings.Join(violations, "; ")
				current, err := m.resolveSnapshot(*r)
				if err != nil || current.Plan != target.ID || r.ApplyBlocked == report {
					return errSkipWrite
				}
				r.ApplyBlocked = report
				return nil
			})
			return fmt.Errorf("plan apply blocked: %s", strings.Join(violations, "; "))
		}
	}
	request := ApplyRequest{Revision: r.SnapshotRevision, PlanSnapshot: snapshot}
	ack, err := m.cfg.Applier.Apply(ctx, accountID, request)
	if err != nil {
		return err
	}
	if ack.Revision != request.Revision || ack.Hash != request.Hash {
		return errors.New("cell returned a mismatched plan snapshot acknowledgement")
	}
	_, err = m.mutate(ctx, accountID, "", func(r *Record) error {
		current, err := m.resolveSnapshot(*r)
		if err != nil {
			return err
		}
		if current.Hash != request.Hash ||
			r.DesiredSnapshotHash != request.Hash ||
			r.SnapshotRevision != request.Revision {
			return errSkipWrite // entitlement/override moved while we pushed
		}
		r.Applied = request.Plan
		r.AppliedSnapshotRevision = request.Revision
		r.AppliedSnapshotHash = request.Hash
		r.ApplyBlocked = ""
		return nil
	})
	return err
}

// SnapshotApplyPending reports whether the cell has acknowledged the exact
// currently resolved snapshot, not merely the same plan label.
func SnapshotApplyPending(r Record, snapshot PlanSnapshot) bool {
	return r.SnapshotRevision == 0 ||
		r.DesiredSnapshotHash != snapshot.Hash ||
		r.AppliedSnapshotRevision != r.SnapshotRevision ||
		r.AppliedSnapshotHash != snapshot.Hash
}
