package server

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AccountRequestBudgetConfig enables independent read and write request budgets
// per account. Disabled configurations are inert, including their numeric values.
type AccountRequestBudgetConfig struct {
	Enabled                                                                  bool
	ReadPerMinute, ReadBurst, WritePerMinute, WriteBurst, MaxTrackedAccounts int
	Plans                                                                    map[string]AccountRequestBudgetTier
}

// AccountRequestBudgetTier supplies the read and write budgets for one plan id.
type AccountRequestBudgetTier struct {
	ReadPerMinute, ReadBurst, WritePerMinute, WriteBurst int
}

const (
	accountRequestBudgetEnvPrefix         = "WITSELF_ACCOUNT_REQUEST_BUDGET_"
	accountRequestBudgetPlanTTL           = time.Minute
	accountRequestBudgetPlanRetryTTL      = 5 * time.Second
	accountRequestBudgetPlanLookupTimeout = 2 * time.Second
)

var (
	accountRequestBudgetPlanIDPattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	accountRequestBudgetIntegerPattern = regexp.MustCompile(`^[0-9]+$`)
)

// ParseAccountRequestBudgetConfig only reads numeric settings when enabled.
// Errors contain configuration names and ranges, never supplied values.
func ParseAccountRequestBudgetConfig(lookup func(string) (string, bool)) (AccountRequestBudgetConfig, error) {
	var cfg AccountRequestBudgetConfig
	raw, present := lookup(accountRequestBudgetEnvPrefix + "ENABLED")
	if !present {
		return cfg, nil
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return cfg, fmt.Errorf("WITSELF_ACCOUNT_REQUEST_BUDGET_ENABLED must be a boolean")
	}
	if !enabled {
		return cfg, nil
	}
	cfg.Enabled = true
	for _, setting := range accountRequestBudgetSettings(&cfg) {
		raw, present := lookup(setting.name)
		if !present {
			if setting.min == 100 {
				*setting.value = 10000
				continue
			}
			return AccountRequestBudgetConfig{}, fmt.Errorf("%s is required when WITSELF_ACCOUNT_REQUEST_BUDGET_ENABLED is true", setting.name)
		}
		value, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || value < setting.min || value > 1000000 {
			return AccountRequestBudgetConfig{}, setting.rangeError()
		}
		*setting.value = value
	}
	return cfg, nil
}

// ParseAccountRequestBudgetPlans reads only the optional plan map. Callers must
// invoke it only when the account request budget is enabled.
func ParseAccountRequestBudgetPlans(lookup func(string) (string, bool)) (map[string]AccountRequestBudgetTier, error) {
	raw, present := lookup(accountRequestBudgetEnvPrefix + "PLANS")
	if !present {
		return nil, nil
	}
	var plans map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &plans); err != nil || plans == nil {
		return nil, fmt.Errorf("WITSELF_ACCOUNT_REQUEST_BUDGET_PLANS must be a JSON object of plan ids to read and write budgets")
	}
	if err := validateAccountRequestBudgetPlanCount(len(plans)); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(plans))
	for id := range plans {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parsed := make(map[string]AccountRequestBudgetTier, len(plans))
	for _, id := range ids {
		if err := validateAccountRequestBudgetPlanID(id); err != nil {
			return nil, err
		}
		var values map[string]json.RawMessage
		var tier AccountRequestBudgetTier
		settings := accountRequestBudgetTierSettings(&tier)
		if err := json.Unmarshal(plans[id], &values); err != nil || len(values) != len(settings) {
			return nil, accountRequestBudgetPlanKeysError(id)
		}
		for _, setting := range settings {
			if _, ok := values[setting.name]; !ok {
				return nil, accountRequestBudgetPlanKeysError(id)
			}
		}
		for _, setting := range settings {
			token := strings.TrimSpace(string(values[setting.name]))
			value, err := strconv.Atoi(token)
			if !accountRequestBudgetIntegerPattern.MatchString(token) || err != nil || value < 1 || value > 1000000 {
				return nil, accountRequestBudgetPlanRangeError(id, setting.name)
			}
			*setting.value = value
		}
		parsed[id] = tier
	}
	return parsed, nil
}

func validateAccountRequestBudgetPlanCount(count int) error {
	if count < 1 || count > 16 {
		return fmt.Errorf("WITSELF_ACCOUNT_REQUEST_BUDGET_PLANS must name 1 to 16 plans")
	}
	return nil
}

func validateAccountRequestBudgetPlanID(id string) error {
	if !accountRequestBudgetPlanIDPattern.MatchString(id) {
		return fmt.Errorf("WITSELF_ACCOUNT_REQUEST_BUDGET_PLANS has a plan id that is not 1-32 lowercase letters, digits or underscores starting with a letter")
	}
	return nil
}

func accountRequestBudgetPlanKeysError(id string) error {
	return fmt.Errorf("WITSELF_ACCOUNT_REQUEST_BUDGET_PLANS plan %q must set exactly read_per_minute, read_burst, write_per_minute and write_burst", id)
}

func accountRequestBudgetPlanRangeError(id, key string) error {
	return fmt.Errorf("WITSELF_ACCOUNT_REQUEST_BUDGET_PLANS plan %q %s must be an integer from 1 to 1000000", id, key)
}

func accountRequestBudgetTierSettings(tier *AccountRequestBudgetTier) []accountRequestBudgetSetting {
	return []accountRequestBudgetSetting{
		{"read_per_minute", &tier.ReadPerMinute, 1},
		{"read_burst", &tier.ReadBurst, 1},
		{"write_per_minute", &tier.WritePerMinute, 1},
		{"write_burst", &tier.WriteBurst, 1},
	}
}

type accountRequestBudgetSetting struct {
	name  string
	value *int
	min   int
}

func accountRequestBudgetSettings(c *AccountRequestBudgetConfig) []accountRequestBudgetSetting {
	return []accountRequestBudgetSetting{
		{accountRequestBudgetEnvPrefix + "READ_PER_MINUTE", &c.ReadPerMinute, 1},
		{accountRequestBudgetEnvPrefix + "READ_BURST", &c.ReadBurst, 1},
		{accountRequestBudgetEnvPrefix + "WRITE_PER_MINUTE", &c.WritePerMinute, 1},
		{accountRequestBudgetEnvPrefix + "WRITE_BURST", &c.WriteBurst, 1},
		{accountRequestBudgetEnvPrefix + "MAX_ACCOUNTS", &c.MaxTrackedAccounts, 100},
	}
}

func (s accountRequestBudgetSetting) rangeError() error {
	return fmt.Errorf("%s must be an integer from %d to 1000000", s.name, s.min)
}

// Validate accepts all disabled configurations and validates each enabled bound.
func (c AccountRequestBudgetConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	for _, setting := range accountRequestBudgetSettings(&c) {
		if *setting.value < setting.min || *setting.value > 1000000 {
			return setting.rangeError()
		}
	}
	if c.Plans != nil {
		if err := validateAccountRequestBudgetPlanCount(len(c.Plans)); err != nil {
			return err
		}
		for _, id := range accountRequestBudgetPlanIDs(c.Plans) {
			if err := validateAccountRequestBudgetPlanID(id); err != nil {
				return err
			}
			tier := c.Plans[id]
			for _, setting := range accountRequestBudgetTierSettings(&tier) {
				if *setting.value < 1 || *setting.value > 1000000 {
					return accountRequestBudgetPlanRangeError(id, setting.name)
				}
			}
		}
	}
	return nil
}

func accountRequestBudgetPlanIDs(plans map[string]AccountRequestBudgetTier) []string {
	ids := make([]string, 0, len(plans))
	for id := range plans {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// accountRequestBudget is a bounded, per-account GCRA: each replica keeps its own
// buckets. With N replicas, an account's cell-wide allowance lies between 1× (all
// of its traffic reaches one replica, for example over one keep-alive connection)
// and N× (traffic spread evenly). A restart or rollout refills every bucket on
// that replica, so an account can get at most one extra burst per replica per restart.
type accountRequestBudget struct {
	mu                sync.Mutex
	entries           map[string]*accountRequestBudgetEntry
	lru               list.List
	tiers             []tierLimits
	planIndex         map[string]int
	tierCache         map[string]*accountRequestBudgetTierEntry
	tierLRU           list.List
	tierSeq           uint64
	planLookupTimeout time.Duration
	getAccountPlan    func(context.Context, string) (PlanSnapshotRecord, error)
	maxAccounts       int
	now               func() time.Time
	rejections        [2]uint64
	capacityEvictions uint64
}

// An entry, including its list element, is well under 256 bytes; 10,000 entries
// occupy under about 2.5 MB (excluding map and account-string allocation overhead).
type accountRequestBudgetEntry struct {
	accountID string
	tier      int
	tat       [2]time.Time
	element   *list.Element
}

type tierLimits struct {
	interval  [2]time.Duration
	tolerance [2]time.Duration
}

type accountRequestBudgetTierEntry struct {
	accountID string
	tier      int
	resolved  bool
	expires   time.Time
	seq       uint64
	element   *list.Element
}

func newAccountRequestBudgetTierLimits(tier AccountRequestBudgetTier) tierLimits {
	limits := tierLimits{
		interval: [2]time.Duration{time.Minute / time.Duration(tier.ReadPerMinute), time.Minute / time.Duration(tier.WritePerMinute)},
	}
	limits.tolerance = [2]time.Duration{time.Duration(tier.ReadBurst-1) * limits.interval[0], time.Duration(tier.WriteBurst-1) * limits.interval[1]}
	return limits
}

func newAccountRequestBudget(cfg AccountRequestBudgetConfig, now func() time.Time) *accountRequestBudget {
	if now == nil {
		now = time.Now
	}
	b := &accountRequestBudget{
		entries:           make(map[string]*accountRequestBudgetEntry),
		maxAccounts:       cfg.MaxTrackedAccounts,
		now:               now,
		planIndex:         make(map[string]int, len(cfg.Plans)),
		tierCache:         make(map[string]*accountRequestBudgetTierEntry),
		planLookupTimeout: accountRequestBudgetPlanLookupTimeout,
		tiers: []tierLimits{newAccountRequestBudgetTierLimits(AccountRequestBudgetTier{
			ReadPerMinute: cfg.ReadPerMinute, ReadBurst: cfg.ReadBurst,
			WritePerMinute: cfg.WritePerMinute, WriteBurst: cfg.WriteBurst,
		})},
	}
	for _, id := range accountRequestBudgetPlanIDs(cfg.Plans) {
		b.planIndex[id] = len(b.tiers)
		b.tiers = append(b.tiers, newAccountRequestBudgetTierLimits(cfg.Plans[id]))
	}
	return b
}

func (b *accountRequestBudget) remove(entry *accountRequestBudgetEntry) {
	delete(b.entries, entry.accountID)
	b.lru.Remove(entry.element)
}

func (b *accountRequestBudget) admit(accountID, class string) (bool, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.admitLocked(accountID, class)
}

// admitForRequest resolves a plan outside the mutex, then selects and debits its
// tier under one lock. The separate tier LRU survives bucket idle sweeps.
func (b *accountRequestBudget) admitForRequest(r *http.Request, accountID, class string) (bool, int64) {
	b.mu.Lock()
	if len(b.planIndex) == 0 {
		admitted, retryAfter := b.admitLocked(accountID, class)
		b.mu.Unlock()
		return admitted, retryAfter
	}
	now := b.now()
	if entry := b.tierCache[accountID]; entry != nil && now.Before(entry.expires) {
		b.tierLRU.MoveToFront(entry.element)
		admitted, retryAfter := b.admitTierLocked(accountID, class, entry.tier)
		b.mu.Unlock()
		return admitted, retryAfter
	}
	b.tierSeq++
	seq := b.tierSeq
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), b.planLookupTimeout)
	record, err := b.getAccountPlan(ctx, accountID)
	cancel()

	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.tierCache[accountID]
	tier := 0
	if err == nil {
		tier = b.planIndex[record.Plan]
		if cur == nil || seq > cur.seq {
			cur = b.cacheTierLocked(accountID, tier, true, now.Add(accountRequestBudgetPlanTTL), seq)
		} else {
			tier = cur.tier
		}
	} else if cur != nil && cur.resolved {
		// A failed refresh preserves a successful resolution, even when the
		// refresh was started before that successful lookup completed.
		tier = cur.tier
		if seq > cur.seq {
			cur.expires = now.Add(accountRequestBudgetPlanRetryTTL)
			cur.seq = seq
		}
	} else if cur == nil || seq > cur.seq {
		cur = b.cacheTierLocked(accountID, 0, false, now.Add(accountRequestBudgetPlanRetryTTL), seq)
	}
	b.tierLRU.MoveToFront(cur.element)
	return b.admitTierLocked(accountID, class, tier)
}

func (b *accountRequestBudget) cacheTierLocked(accountID string, tier int, resolved bool, expires time.Time, seq uint64) *accountRequestBudgetTierEntry {
	entry := b.tierCache[accountID]
	if entry == nil {
		if len(b.tierCache) >= b.maxAccounts {
			oldest := b.tierLRU.Back()
			delete(b.tierCache, oldest.Value.(*accountRequestBudgetTierEntry).accountID)
			b.tierLRU.Remove(oldest)
		}
		entry = &accountRequestBudgetTierEntry{accountID: accountID}
		entry.element = b.tierLRU.PushFront(entry)
		b.tierCache[accountID] = entry
	}
	entry.tier = tier
	entry.resolved = resolved
	entry.expires = expires
	entry.seq = seq
	return entry
}

// admitLocked only decides and updates state; callers write after unlocking mu.
func (b *accountRequestBudget) admitLocked(accountID, class string) (bool, int64) {
	return b.admitTierLocked(accountID, class, 0)
}

func (b *accountRequestBudget) admitTierLocked(accountID, class string, tier int) (bool, int64) {
	now := b.now()
	back := b.lru.Back()
	for examined := 0; examined < 2 && back != nil; examined++ {
		entry := back.Value.(*accountRequestBudgetEntry)
		back = back.Prev()
		if entry.accountID != accountID && !entry.tat[0].After(now) && !entry.tat[1].After(now) {
			b.remove(entry)
		}
	}
	entry := b.entries[accountID]
	if entry == nil {
		if len(b.entries) >= b.maxAccounts {
			// An evicted account starts with full buckets. At the default cap,
			// eviction requires over 10,000 distinct active accounts per replica.
			b.remove(b.lru.Back().Value.(*accountRequestBudgetEntry))
			b.capacityEvictions++
		}
		entry = &accountRequestBudgetEntry{accountID: accountID}
		entry.element = b.lru.PushFront(entry)
		b.entries[accountID] = entry
	} else {
		// Refusals are activity too: keep a continuously calling account hot.
		b.lru.MoveToFront(entry.element)
	}
	if entry.tier != tier {
		entry.tat = [2]time.Time{}
		entry.tier = tier
	}
	limits := b.tiers[tier]
	index := 1
	if class == "read" {
		index = 0
	}
	tat := entry.tat[index]
	if tat.Before(now) {
		tat = now
	}
	if wait := tat.Sub(now) - limits.tolerance[index]; wait > 0 {
		b.rejections[index]++
		return false, messageRateLimitRetryAfterSeconds(wait)
	}
	entry.tat[index] = tat.Add(limits.interval[index])
	return true, 0
}

// classifyAccountRequest classifies MCP tool calls by the HTTP requests the CLI
// sends for them. A mutating tool (memory capture, POST /v1/memories) debits write;
// memory recall debits read. A tool sending several requests debits once per
// request. Some read-only POSTs (secret field :access, retry-canary:status) debit
// write on purpose; see FU123-10. Unknown or mis-cased methods debit write.
func classifyAccountRequest(r *http.Request) string {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return "read"
	}
	if r.Method == http.MethodPost {
		switch r.URL.Path {
		case "/v1/memories:recall", "/v1/messages:listen", "/v1/email:listen":
			return "read"
		}
	}
	return "write"
}

type accountRequestBudgetTicketKey struct{}

type accountRequestBudgetTicket struct {
	b       *accountRequestBudget
	class   string
	debited bool
}

func accountRequestBudgetMux(b *accountRequestBudget, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), accountRequestBudgetTicketKey{}, &accountRequestBudgetTicket{b: b, class: classifyAccountRequest(r)})
		// Preserve the request's identity so outer instrumentation sees r.Pattern.
		*r = *r.WithContext(ctx)
		next.ServeHTTP(w, r)
	})
}

func admitAccountRequest(w http.ResponseWriter, r *http.Request, accountID string) bool {
	ticket, ok := r.Context().Value(accountRequestBudgetTicketKey{}).(*accountRequestBudgetTicket)
	if !ok || ticket.debited || strings.TrimSpace(accountID) == "" {
		return true
	}
	admitted, retryAfter := ticket.b.admitForRequest(r, accountID, ticket.class)
	ticket.debited = true
	if !admitted {
		writeAccountRequestBudgetError(w, ticket.class, retryAfter)
	}
	return admitted
}

func writeAccountRequestBudgetError(w http.ResponseWriter, class string, retryAfter int64) {
	dimension := "account_request_" + class
	w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema_version": "witself.v0",
		"code":           "rate_limited",
		"error":          "account request rate limit reached",
		"retryable":      true,
		"retry_after":    retryAfter,
		"details": map[string]any{
			"limit_dimension": dimension,
			"limit_key":       dimension + "_per_account_minute",
			"scope":           "account",
			"retry_after":     retryAfter,
		},
	})
}
