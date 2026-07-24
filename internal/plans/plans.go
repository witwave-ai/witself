// Package plans parses and validates the witself.plans.v0 plan catalog — the
// document embedded at the repo root (witself.PlansJSON) and served publicly
// by the witself-plans Cloudflare Worker. The catalog is the single source of
// truth for what plans exist, what they cost, their limits and features, and
// which are purchasable (available). The control plane loads it to drive the
// plan state machine; cells never need it (they enforce the resolved snapshot
// pushed onto their account records).
package plans

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	witself "github.com/witwave-ai/witself"
)

// SchemaVersion is the catalog schema this package understands.
const SchemaVersion = "witself.plans.v0"

// Free is the id of the zero-value plan: the plan an account resolves to when
// no billing relationship exists. It is priced 0 and never purchasable
// through a billing provider.
const Free = "free"

const (
	// RealmLimit caps live realms account-wide.
	RealmLimit = "realms"
	// AgentLimit caps live agents account-wide.
	AgentLimit = "agents"
	// StoredSecretLimit caps retained top-level secrets independently for
	// each owner agent. Active and archived secrets count; deleted secrets do
	// not. A missing key means unlimited.
	StoredSecretLimit = "stored_secret"
	// MaxPlanLimit is the largest exact integer shared by Go and JavaScript's
	// JSON number representation. Unlimited is represented by a missing key,
	// never by an oversized sentinel.
	MaxPlanLimit int64 = 9_007_199_254_740_991

	// TranscriptRetentionDaysPolicy is the resolved behavioral-policy key
	// cells enforce. Its absence means indefinite retention; zero is never a
	// synonym for indefinite because an accidental zero must fail closed at
	// the control-plane boundary instead of immediately deleting transcripts.
	TranscriptRetentionDaysPolicy = "transcript_retention_days"
	// MaxTranscriptRetentionDays is a defensive representation bound, not a
	// product-tier cap. Enterprise indefinite retention is represented by a
	// missing key.
	MaxTranscriptRetentionDays int64 = 36500
)

// Plan is one catalog entry.
type Plan struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// PriceMonthly is whole dollars per month (display units, matching the
	// public document). Use PriceCents for billing math.
	PriceMonthly *int64 `json:"price_monthly"`
	// PriceMonthlyMin is whole dollars per month for minimum-commitment tiers.
	// Set instead of PriceMonthly.
	PriceMonthlyMin *int64           `json:"price_monthly_min"`
	Available       bool             `json:"available"`
	UsageBilled     bool             `json:"usage_billed"`
	Limits          map[string]int64 `json:"limits"` // nil = not yet defined (TBD tiers)
	// Policies are non-cap behavioral entitlements resolved by the control
	// plane and pushed to cells. A missing policy means no restriction.
	Policies map[string]int64 `json:"policies"`
	Features []string         `json:"features"`
	Summary  string           `json:"summary"`
}

// PriceCents returns the monthly price in cents (minimum commitment for
// tiers priced with price_monthly_min).
func (p Plan) PriceCents() int64 {
	switch {
	case p.PriceMonthly != nil:
		return *p.PriceMonthly * 100
	case p.PriceMonthlyMin != nil:
		return *p.PriceMonthlyMin * 100
	default:
		return 0
	}
}

// Paid reports whether the plan costs money.
func (p Plan) Paid() bool { return p.PriceCents() > 0 }

// Purchasable reports whether a billing provider can sell this plan today:
// available in the catalog and actually priced. Free is available but not
// purchasable (it is the absence of a subscription); unavailable and
// custom-priced tiers are also absent.
func (p Plan) Purchasable() bool { return p.Available && p.Paid() }

// HasFeature reports whether the plan includes the named feature.
func (p Plan) HasFeature(name string) bool { return slices.Contains(p.Features, name) }

// Catalog is the parsed, validated plan catalog. Plans preserves document
// order (the pricing-page order: cheapest first).
type Catalog struct {
	Updated  string
	Currency string
	Plans    []Plan

	byID map[string]Plan
}

// Get returns the plan by id.
func (c *Catalog) Get(id string) (Plan, bool) {
	p, ok := c.byID[id]
	return p, ok
}

// Prices returns purchasable plan prices in cents — the shape billing
// providers (including the fake) are configured with. Free and the
// not-yet-available tiers are deliberately absent.
func (c *Catalog) Prices() map[string]int64 {
	out := map[string]int64{}
	for _, p := range c.Plans {
		if p.Purchasable() {
			out[p.ID] = p.PriceCents()
		}
	}
	return out
}

// Load parses and validates the embedded canonical catalog. The validation
// runs in tests too, so an invalid catalog edit fails `make check` before it
// can ship anywhere.
func Load() (*Catalog, error) { return Parse(witself.PlansJSON) }

// Parse parses and validates a witself.plans.v0 document.
func Parse(raw []byte) (*Catalog, error) {
	var doc struct {
		SchemaVersion string `json:"schema_version"`
		Updated       string `json:"updated"`
		Currency      string `json:"currency"`
		Plans         []Plan `json:"plans"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("plan catalog: %w", err)
	}
	if doc.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("plan catalog: schema_version %q, want %q", doc.SchemaVersion, SchemaVersion)
	}
	if len(doc.Plans) == 0 {
		return nil, fmt.Errorf("plan catalog: no plans")
	}
	c := &Catalog{
		Updated:  doc.Updated,
		Currency: doc.Currency,
		Plans:    doc.Plans,
		byID:     make(map[string]Plan, len(doc.Plans)),
	}
	for _, p := range doc.Plans {
		if p.ID == "" {
			return nil, fmt.Errorf("plan catalog: plan with empty id")
		}
		if _, dup := c.byID[p.ID]; dup {
			return nil, fmt.Errorf("plan catalog: duplicate plan id %q", p.ID)
		}
		if p.PriceMonthly != nil && p.PriceMonthlyMin != nil {
			return nil, fmt.Errorf("plan catalog: plan %q sets both price_monthly and price_monthly_min", p.ID)
		}
		if err := ValidateLimits(p.Limits); err != nil {
			return nil, fmt.Errorf("plan catalog: plan %q: %w", p.ID, err)
		}
		if err := ValidatePolicies(p.Policies); err != nil {
			return nil, fmt.Errorf("plan catalog: plan %q: %w", p.ID, err)
		}
		c.byID[p.ID] = p
	}
	free, ok := c.byID[Free]
	switch {
	case !ok:
		return nil, fmt.Errorf("plan catalog: missing the %q plan (the zero-value plan every account defaults to)", Free)
	case free.Paid():
		return nil, fmt.Errorf("plan catalog: the %q plan must cost 0", Free)
	case !free.Available:
		return nil, fmt.Errorf("plan catalog: the %q plan must be available", Free)
	}
	return c, nil
}

// ValidateLimits validates one resolved hard-cap snapshot. Keys are closed so
// a misspelling cannot make a paid limit appear applied while every cell
// silently ignores it. Zero is a real cap; unlimited is represented by an
// omitted key.
func ValidateLimits(limits map[string]int64) error {
	for key, value := range limits {
		switch key {
		case RealmLimit, AgentLimit, StoredSecretLimit:
		default:
			return fmt.Errorf("unknown limit %q", key)
		}
		if value < 0 || value > MaxPlanLimit {
			return fmt.Errorf("%s must be between 0 and %d (omit it for unlimited)",
				key, MaxPlanLimit)
		}
	}
	return nil
}

// ValidatePolicies validates one resolved policy snapshot. Policy keys are
// deliberately closed while this contract has only one implemented member:
// silently accepting a misspelling would make a paid retention promise appear
// applied while the cell ignored it.
func ValidatePolicies(policies map[string]int64) error {
	for key, value := range policies {
		switch key {
		case TranscriptRetentionDaysPolicy:
			if value < 1 || value > MaxTranscriptRetentionDays {
				return fmt.Errorf("%s must be between 1 and %d days (omit it for indefinite retention)",
					TranscriptRetentionDaysPolicy, MaxTranscriptRetentionDays)
			}
		default:
			return fmt.Errorf("unknown policy %q", key)
		}
	}
	return nil
}

// SnapshotHash returns the canonical digest of one resolved account snapshot.
// Both the control plane and the cell use this exact function so the hash
// acknowledged by the cell proves that every behavioral field was understood
// and persisted.
func SnapshotHash(plan string, limits, policies map[string]int64, features []string) (string, error) {
	if limits == nil {
		limits = map[string]int64{}
	}
	if policies == nil {
		policies = map[string]int64{}
	}
	features = append([]string(nil), features...)
	if features == nil {
		features = []string{}
	}
	slices.Sort(features)
	raw, err := json.Marshal(struct {
		Plan     string           `json:"plan"`
		Limits   map[string]int64 `json:"limits"`
		Policies map[string]int64 `json:"policies"`
		Features []string         `json:"features"`
	}{
		Plan: plan, Limits: limits, Policies: policies, Features: features,
	})
	if err != nil {
		return "", fmt.Errorf("hash plan snapshot: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
