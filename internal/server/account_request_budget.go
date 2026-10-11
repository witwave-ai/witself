package server

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
}

const accountRequestBudgetEnvPrefix = "WITSELF_ACCOUNT_REQUEST_BUDGET_"

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
	return nil
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
	interval          [2]time.Duration
	tolerance         [2]time.Duration
	maxAccounts       int
	now               func() time.Time
	rejections        [2]uint64
	capacityEvictions uint64
}

// An entry, including its list element, is well under 256 bytes; 10,000 entries
// occupy under about 2.5 MB (excluding map and account-string allocation overhead).
type accountRequestBudgetEntry struct {
	accountID string
	tat       [2]time.Time
	element   *list.Element
}

func newAccountRequestBudget(cfg AccountRequestBudgetConfig, now func() time.Time) *accountRequestBudget {
	if now == nil {
		now = time.Now
	}
	b := &accountRequestBudget{
		entries:     make(map[string]*accountRequestBudgetEntry),
		maxAccounts: cfg.MaxTrackedAccounts,
		now:         now,
		interval:    [2]time.Duration{time.Minute / time.Duration(cfg.ReadPerMinute), time.Minute / time.Duration(cfg.WritePerMinute)},
	}
	b.tolerance = [2]time.Duration{time.Duration(cfg.ReadBurst-1) * b.interval[0], time.Duration(cfg.WriteBurst-1) * b.interval[1]}
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

// admitLocked only decides and updates state; callers write after unlocking mu.
func (b *accountRequestBudget) admitLocked(accountID, class string) (bool, int64) {
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
	index := 1
	if class == "read" {
		index = 0
	}
	tat := entry.tat[index]
	if tat.Before(now) {
		tat = now
	}
	if wait := tat.Sub(now) - b.tolerance[index]; wait > 0 {
		b.rejections[index]++
		return false, messageRateLimitRetryAfterSeconds(wait)
	}
	entry.tat[index] = tat.Add(b.interval[index])
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
	ticket.b.mu.Lock()
	admitted, retryAfter := ticket.b.admitLocked(accountID, ticket.class)
	ticket.debited = true
	ticket.b.mu.Unlock()
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
