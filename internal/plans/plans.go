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
	"strings"

	witself "github.com/witwave-ai/witself"
)

// SchemaVersion is the catalog schema this package understands.
const SchemaVersion = "witself.plans.v0"

// Free is the id of the zero-value plan: the plan an account resolves to when
// no billing relationship exists. It is priced 0 and never purchasable
// through a billing provider.
const Free = "free"

const (
	// MessagingFeature enables the durable realm-local agent messaging
	// surface. Cells enforce this feature from the resolved account snapshot;
	// clients install the messaging tools once and do not reinstall when the
	// account's effective entitlement changes.
	MessagingFeature = "messaging"
	// AgentEmailReceiveFeature enables inbound agent email. The integration is
	// installed once; cells enforce the account's resolved feature snapshot.
	AgentEmailReceiveFeature = "agent_email_receive"
	// AgentEmailRealmAliasFeature allows a realm to activate memorable labels
	// in addition to its permanent ID-body email address. The global claim and
	// reserved-name policy, downgrade grace, and count enforcement remain
	// control-plane concerns. Cells trust the provision-token projection and
	// enforce its state plus the separate inbound-email entitlement.
	AgentEmailRealmAliasFeature = "agent_email_realm_alias"
	// AgentEmailCustomDomainFeature allows an account to activate inbound agent
	// email on organization-owned domains. The global claim, ownership proof,
	// routing lifecycle, and count enforcement remain control-plane concerns.
	// Cells trust only the applied route projection and continue to enforce the
	// separate inbound-email entitlement.
	AgentEmailCustomDomainFeature = "agent_email_custom_domain"

	// RealmLimit caps live realms account-wide.
	RealmLimit = "realms"
	// AgentLimit is the legacy account-wide live-agent cap. Existing snapshots
	// keep this meaning during the agents-per-realm rollout; catalogs must move
	// to AgentPerRealmLimit rather than silently reinterpreting this key.
	AgentLimit = "agents"
	// AgentPerRealmLimit caps live agents independently in each live realm.
	AgentPerRealmLimit = "agents_per_realm"
	// StoredSecretLimit caps retained top-level secrets independently for
	// each owner agent. Active and archived secrets count; deleted secrets do
	// not. A missing key means unlimited.
	StoredSecretLimit = "stored_secret"
	// StoredMemoryLimit caps active narrative-memory heads independently for
	// each owner agent. Historical, superseded, forgotten, and permanently
	// deleted memories do not consume active capacity. A missing key means
	// unlimited.
	StoredMemoryLimit = "stored_memory"
	// StoredFactLimit caps current resolved fact addresses independently for
	// each owner agent. Assertions, candidates, subjects, aliases, deleted
	// tombstones, and usage history do not consume current-fact capacity. A
	// missing key means unlimited.
	StoredFactLimit = "stored_fact"
	// AgentEmailMaxRawBytesLimit caps the raw MIME size of each inbound agent
	// email. The resolved value applies account-wide to each message; a
	// missing key means no plan cap, subject to the service's defensive
	// transport ceiling.
	AgentEmailMaxRawBytesLimit = "agent_email_max_raw_bytes"
	// AgentEmailAttachmentStorageBytesLimit caps retained attachment-bearing
	// MIME bytes across the account. The current inline-MIME implementation
	// charges the full raw-message size when a message contains attachments;
	// it does not model separately stored attachment blobs. A missing key
	// means unlimited.
	AgentEmailAttachmentStorageBytesLimit = "agent_email_attachment_storage_bytes"
	// AgentEmailRealmAliasesPerRealmLimit caps active memorable email labels in
	// each realm at the global control-plane authority. Zero prevents new alias
	// activation while preserving canonical ID-body address reservations and
	// alias tombstones; existing aliases follow the control-plane downgrade
	// grace. A missing key means unlimited, subject to the feature entitlement.
	AgentEmailRealmAliasesPerRealmLimit = "agent_email_realm_aliases_per_realm"
	// AgentEmailCustomDomainsPerAccountLimit caps active organization-owned
	// inbound email domains account-wide at the global control-plane authority.
	// Zero prevents activation; a missing key means unlimited, subject to the
	// feature entitlement. Enterprise therefore carries an explicit zero until
	// a contracted per-account override is applied.
	AgentEmailCustomDomainsPerAccountLimit = "agent_email_custom_domains_per_account"
	// MessageSentPerAgentMinuteLimit caps messages accepted from each sending
	// agent under a rolling one-minute rate budget. A missing key means no
	// commercial plan cap, but the service's defensive platform maximum still
	// applies.
	MessageSentPerAgentMinuteLimit = "message_sent_per_agent_minute"
	// MessageDeliveredPerRealmMinuteLimit caps aggregate recipient deliveries
	// within each realm under a rolling one-minute rate budget. Fan-out charges
	// one delivery per recipient. A missing key means no commercial plan cap,
	// but the service's defensive platform maximum still applies.
	MessageDeliveredPerRealmMinuteLimit = "message_delivered_per_realm_minute"
	// MessageDeliveredPerRecipientMinuteLimit caps deliveries to each recipient
	// under a rolling one-minute rate budget. A missing key means no commercial
	// plan cap, but the service's defensive platform maximum still applies.
	MessageDeliveredPerRecipientMinuteLimit = "message_delivered_per_recipient_minute"
	// AgentEmailReceivedPerSenderMinuteLimit caps accepted inbound email from
	// one unverified envelope-sender/recipient pair under a rolling one-minute
	// rate budget. A missing key means no commercial plan cap, but the service's
	// defensive platform maximum still applies.
	AgentEmailReceivedPerSenderMinuteLimit = "agent_email_received_per_sender_minute"
	// AgentEmailReceivedPerRecipientMinuteLimit caps accepted inbound email for
	// one receiving agent under a rolling one-minute rate budget.
	AgentEmailReceivedPerRecipientMinuteLimit = "agent_email_received_per_recipient_minute"
	// AgentEmailReceivedPerRealmMinuteLimit caps aggregate accepted inbound
	// email within one realm under a rolling one-minute rate budget.
	AgentEmailReceivedPerRealmMinuteLimit = "agent_email_received_per_realm_minute"
	// AgentEmailReceivedBytesPerSenderMinuteLimit is the byte-weighted companion
	// to AgentEmailReceivedPerSenderMinuteLimit. It meters signed raw-MIME bytes.
	AgentEmailReceivedBytesPerSenderMinuteLimit = "agent_email_received_bytes_per_sender_minute"
	// AgentEmailReceivedBytesPerRecipientMinuteLimit caps signed raw-MIME bytes
	// accepted for one receiving agent under a rolling one-minute rate budget.
	AgentEmailReceivedBytesPerRecipientMinuteLimit = "agent_email_received_bytes_per_recipient_minute"
	// AgentEmailReceivedBytesPerRealmMinuteLimit caps aggregate signed raw-MIME
	// bytes accepted within one realm under a rolling one-minute rate budget.
	AgentEmailReceivedBytesPerRealmMinuteLimit = "agent_email_received_bytes_per_realm_minute"
	// MaxAgentEmailRawBytes is the service/provider transport ceiling. Plan
	// defaults and per-account overrides may lower it but cannot promise a
	// larger message than the receive path can carry.
	MaxAgentEmailRawBytes int64 = 25 * 1024 * 1024
	// MaxMessageSentPerAgentMinute is the always-enforced platform maximum for
	// one sending agent under a rolling one-minute rate budget.
	MaxMessageSentPerAgentMinute int64 = 2_000
	// MaxMessageDeliveredPerRealmMinute is the always-enforced platform maximum
	// for aggregate recipient deliveries in one realm under a rolling one-minute
	// rate budget.
	MaxMessageDeliveredPerRealmMinute int64 = 100_000
	// MaxMessageDeliveredPerRecipientMinute is the always-enforced platform
	// maximum for one recipient under a rolling one-minute rate budget.
	MaxMessageDeliveredPerRecipientMinute int64 = 5_000
	// MaxAgentEmailReceivedPerSenderMinute is the always-enforced platform
	// breaker for one unverified envelope-sender/recipient pair.
	MaxAgentEmailReceivedPerSenderMinute int64 = 30
	// MaxAgentEmailReceivedPerRecipientMinute is the always-enforced platform
	// breaker for one receiving agent.
	MaxAgentEmailReceivedPerRecipientMinute int64 = 300
	// MaxAgentEmailReceivedPerRealmMinute is the always-enforced aggregate
	// platform breaker for one realm.
	MaxAgentEmailReceivedPerRealmMinute int64 = 5_000
	// MaxAgentEmailReceivedBytesPerSenderMinute bounds raw-MIME ingress from one
	// unverified envelope-sender/recipient pair to 64 MiB per rolling minute.
	MaxAgentEmailReceivedBytesPerSenderMinute int64 = 64 * 1024 * 1024
	// MaxAgentEmailReceivedBytesPerRecipientMinute bounds raw-MIME ingress for
	// one receiving agent to 512 MiB per rolling minute.
	MaxAgentEmailReceivedBytesPerRecipientMinute int64 = 512 * 1024 * 1024
	// MaxAgentEmailReceivedBytesPerRealmMinute bounds aggregate raw-MIME ingress
	// for one realm to 4 GiB per rolling minute.
	MaxAgentEmailReceivedBytesPerRealmMinute int64 = 4 * 1024 * 1024 * 1024
	// MaxPlanLimit is the largest exact integer shared by Go and JavaScript's
	// JSON number representation. Unlimited is represented by a missing key,
	// never by an oversized sentinel.
	MaxPlanLimit int64 = 9_007_199_254_740_991

	// TranscriptRetentionDaysPolicy is the resolved behavioral-policy key
	// cells enforce. Its absence means indefinite retention; zero is never a
	// synonym for indefinite because an accidental zero must fail closed at
	// the control-plane boundary instead of immediately deleting transcripts.
	TranscriptRetentionDaysPolicy = "transcript_retention_days"
	// MessageRetentionDaysPolicy is the maximum age of retained durable
	// message content. It remains meaningful while messaging is disabled so a
	// downgrade can make the mailbox inaccessible immediately and clean up on
	// the account's finite retention schedule. Its absence means indefinite
	// retention.
	MessageRetentionDaysPolicy = "message_retention_days"
	// MessagingEntitlementVersionPolicy is the rollout marker that makes the
	// explicit MessagingFeature authoritative. Legacy applied snapshots omit
	// it and preserve pre-entitlement messaging behavior. This marker remains
	// present even when message retention is explicitly indefinite.
	MessagingEntitlementVersionPolicy = "messaging_entitlement_version"
	// MessagingEntitlementVersion is the only marker version understood by
	// this release.
	MessagingEntitlementVersion int64 = 1
	// AgentEmailRetentionDaysPolicy is the maximum age of retained inbound
	// agent email. It remains meaningful while receipt is disabled so a
	// downgrade can clean up already-stored mail. Absence means indefinite.
	AgentEmailRetentionDaysPolicy = "agent_email_retention_days"
	// AgentEmailEntitlementVersionPolicy makes AgentEmailReceiveFeature
	// authoritative while preserving legacy snapshots that predate the gate.
	AgentEmailEntitlementVersionPolicy = "agent_email_entitlement_version"
	// AgentEmailEntitlementVersion is the only marker version understood by
	// this release.
	AgentEmailEntitlementVersion int64 = 1
	// MaxTranscriptRetentionDays is a defensive representation bound, not a
	// product-tier cap. Explicit indefinite transcript or message retention is
	// represented by a missing key.
	MaxTranscriptRetentionDays int64 = 36500
	// MaxMessageRetentionDays is the matching defensive bound for messages.
	MaxMessageRetentionDays int64 = 36500
	// MaxAgentEmailRetentionDays is the matching defensive bound for email.
	MaxAgentEmailRetentionDays int64 = 36500
)

// Plan is one catalog entry.
type Plan struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Recommended is presentation metadata for the one plan the product wants
	// to emphasize. It never changes entitlement resolution or purchasability.
	Recommended bool `json:"recommended,omitempty"`
	// Badge is the bounded customer-facing label paired with Recommended.
	Badge string `json:"badge,omitempty"`
	// PriceMonthly is whole dollars per month (display units, matching the
	// public document). Use PriceCents for billing math.
	PriceMonthly *int64 `json:"price_monthly,omitempty"`
	// PriceMonthlyMin is whole dollars per month for minimum-commitment tiers.
	// Set instead of PriceMonthly.
	PriceMonthlyMin *int64           `json:"price_monthly_min,omitempty"`
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
	recommendedPlan := ""
	for i, p := range doc.Plans {
		p.ID = strings.TrimSpace(p.ID)
		p.Name = strings.TrimSpace(p.Name)
		p.Badge = strings.TrimSpace(p.Badge)
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
		if len(p.Badge) > 64 {
			return nil, fmt.Errorf("plan catalog: plan %q badge exceeds 64 bytes", p.ID)
		}
		if p.Recommended {
			if recommendedPlan != "" {
				return nil, fmt.Errorf("plan catalog: plans %q and %q are both recommended",
					recommendedPlan, p.ID)
			}
			if !p.Purchasable() {
				return nil, fmt.Errorf("plan catalog: recommended plan %q must be available and priced", p.ID)
			}
			if p.Badge == "" {
				return nil, fmt.Errorf("plan catalog: recommended plan %q requires a badge", p.ID)
			}
			recommendedPlan = p.ID
		} else if p.Badge != "" {
			return nil, fmt.Errorf("plan catalog: plan %q has a badge but is not recommended", p.ID)
		}
		c.Plans[i] = p
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
		case RealmLimit, AgentLimit, AgentPerRealmLimit, StoredSecretLimit,
			StoredMemoryLimit, StoredFactLimit, AgentEmailMaxRawBytesLimit,
			AgentEmailAttachmentStorageBytesLimit,
			AgentEmailRealmAliasesPerRealmLimit,
			AgentEmailCustomDomainsPerAccountLimit,
			MessageSentPerAgentMinuteLimit,
			MessageDeliveredPerRealmMinuteLimit,
			MessageDeliveredPerRecipientMinuteLimit,
			AgentEmailReceivedPerSenderMinuteLimit,
			AgentEmailReceivedPerRecipientMinuteLimit,
			AgentEmailReceivedPerRealmMinuteLimit,
			AgentEmailReceivedBytesPerSenderMinuteLimit,
			AgentEmailReceivedBytesPerRecipientMinuteLimit,
			AgentEmailReceivedBytesPerRealmMinuteLimit:
		default:
			return fmt.Errorf("unknown limit %q", key)
		}
		if value < 0 || value > MaxPlanLimit {
			return fmt.Errorf("%s must be between 0 and %d (omit it for unlimited)",
				key, MaxPlanLimit)
		}
		if key == AgentEmailMaxRawBytesLimit && value > MaxAgentEmailRawBytes {
			return fmt.Errorf("%s must be between 0 and %d",
				key, MaxAgentEmailRawBytes)
		}
		switch key {
		case MessageSentPerAgentMinuteLimit:
			if value > MaxMessageSentPerAgentMinute {
				return fmt.Errorf("%s must be between 0 and %d",
					key, MaxMessageSentPerAgentMinute)
			}
		case MessageDeliveredPerRealmMinuteLimit:
			if value > MaxMessageDeliveredPerRealmMinute {
				return fmt.Errorf("%s must be between 0 and %d",
					key, MaxMessageDeliveredPerRealmMinute)
			}
		case MessageDeliveredPerRecipientMinuteLimit:
			if value > MaxMessageDeliveredPerRecipientMinute {
				return fmt.Errorf("%s must be between 0 and %d",
					key, MaxMessageDeliveredPerRecipientMinute)
			}
		case AgentEmailReceivedPerSenderMinuteLimit:
			if value > MaxAgentEmailReceivedPerSenderMinute {
				return fmt.Errorf("%s must be between 0 and %d",
					key, MaxAgentEmailReceivedPerSenderMinute)
			}
		case AgentEmailReceivedPerRecipientMinuteLimit:
			if value > MaxAgentEmailReceivedPerRecipientMinute {
				return fmt.Errorf("%s must be between 0 and %d",
					key, MaxAgentEmailReceivedPerRecipientMinute)
			}
		case AgentEmailReceivedPerRealmMinuteLimit:
			if value > MaxAgentEmailReceivedPerRealmMinute {
				return fmt.Errorf("%s must be between 0 and %d",
					key, MaxAgentEmailReceivedPerRealmMinute)
			}
		case AgentEmailReceivedBytesPerSenderMinuteLimit:
			if value > MaxAgentEmailReceivedBytesPerSenderMinute {
				return fmt.Errorf("%s must be between 0 and %d",
					key, MaxAgentEmailReceivedBytesPerSenderMinute)
			}
		case AgentEmailReceivedBytesPerRecipientMinuteLimit:
			if value > MaxAgentEmailReceivedBytesPerRecipientMinute {
				return fmt.Errorf("%s must be between 0 and %d",
					key, MaxAgentEmailReceivedBytesPerRecipientMinute)
			}
		case AgentEmailReceivedBytesPerRealmMinuteLimit:
			if value > MaxAgentEmailReceivedBytesPerRealmMinute {
				return fmt.Errorf("%s must be between 0 and %d",
					key, MaxAgentEmailReceivedBytesPerRealmMinute)
			}
		}
	}
	return nil
}

// ValidatePolicies validates one resolved policy snapshot. Policy keys are
// deliberately closed: silently accepting a misspelling would make a paid
// retention promise appear applied while the cell ignored it.
func ValidatePolicies(policies map[string]int64) error {
	for key, value := range policies {
		switch key {
		case TranscriptRetentionDaysPolicy:
			if value < 1 || value > MaxTranscriptRetentionDays {
				return fmt.Errorf("%s must be between 1 and %d days (omit it for indefinite retention)",
					TranscriptRetentionDaysPolicy, MaxTranscriptRetentionDays)
			}
		case MessageRetentionDaysPolicy:
			if value < 1 || value > MaxMessageRetentionDays {
				return fmt.Errorf("%s must be between 1 and %d days (omit it for indefinite retention)",
					MessageRetentionDaysPolicy, MaxMessageRetentionDays)
			}
		case MessagingEntitlementVersionPolicy:
			if value != MessagingEntitlementVersion {
				return fmt.Errorf("%s must be %d",
					MessagingEntitlementVersionPolicy,
					MessagingEntitlementVersion)
			}
		case AgentEmailRetentionDaysPolicy:
			if value < 1 || value > MaxAgentEmailRetentionDays {
				return fmt.Errorf("%s must be between 1 and %d days (omit it for indefinite retention)",
					AgentEmailRetentionDaysPolicy, MaxAgentEmailRetentionDays)
			}
		case AgentEmailEntitlementVersionPolicy:
			if value != AgentEmailEntitlementVersion {
				return fmt.Errorf("%s must be %d",
					AgentEmailEntitlementVersionPolicy,
					AgentEmailEntitlementVersion)
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
