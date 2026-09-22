package dashboard

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"
)

const (
	summaryTTL              = 30 * time.Second
	summaryTimeout          = 8 * time.Second
	summaryMaxInteger int64 = 1<<53 - 1
)

// A collector belongs to one Reader or one Register invocation. Flights are
// demand-driven, with one shared deadline and at most three upstream requests.
// The last departing caller cancels the flight; a successor waits for that
// canceled flight to drain before starting another one.
type summaryCollector struct {
	cfg    Config
	now    func() time.Time
	mu     sync.Mutex
	cached *Summary
	flight *summaryFlight
}

type summaryFlight struct {
	done      chan struct{}
	cancel    context.CancelFunc
	waiters   int
	abandoned bool
	result    Summary
}

func newSummaryCollector(cfg Config) *summaryCollector {
	return &summaryCollector{cfg: cfg, now: time.Now}
}

func summaryHandler(c *summaryCollector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s, err := c.get(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusGatewayTimeout, "summary unavailable")
			return
		}
		writeJSON(w, struct {
			Summary Summary `json:"summary"`
		}{s})
	})
}

func (c *summaryCollector) get(ctx context.Context) (Summary, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Summary{}, err
		}
		c.mu.Lock()
		now := c.now().UTC()
		if s := c.cached; s != nil && !now.Before(s.GeneratedAt) && now.Sub(s.GeneratedAt) < summaryTTL && now.Truncate(time.Hour).Equal(s.GeneratedAt.Truncate(time.Hour)) {
			out := cloneSummary(*s)
			c.mu.Unlock()
			return out, nil
		}
		f := c.flight
		if f != nil && f.abandoned {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return Summary{}, ctx.Err()
			case <-f.done:
				continue
			}
		}
		if f == nil {
			collectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), summaryTimeout)
			f = &summaryFlight{done: make(chan struct{}), cancel: cancel}
			c.flight = f
			go func() {
				result := collectSummary(collectCtx, c.cfg, now)
				cancel()
				c.mu.Lock()
				f.result = result
				if !f.abandoned {
					c.cached = &result
				}
				c.flight = nil
				close(f.done)
				c.mu.Unlock()
			}()
		}
		f.waiters++
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			c.mu.Lock()
			f.waiters--
			if f.waiters == 0 && c.flight == f {
				f.abandoned = true
				f.cancel()
			}
			c.mu.Unlock()
			return Summary{}, ctx.Err()
		case <-f.done:
			if err := ctx.Err(); err != nil {
				return Summary{}, err
			}
			return cloneSummary(f.result), nil
		}
	}
}

func cloneSummary(s Summary) Summary {
	s.Categories = slices.Clone(s.Categories)
	for i := range s.Categories {
		c := &s.Categories[i]
		c.Activity.Bins = slices.Clone(c.Activity.Bins)
		for j, v := range c.Activity.Bins {
			if v != nil {
				n := *v
				c.Activity.Bins[j] = &n
			}
		}
		c.Activity.Breakdown = slices.Clone(c.Activity.Breakdown)
		for j, v := range c.Activity.Breakdown {
			if v.Total != nil {
				n := *v.Total
				c.Activity.Breakdown[j].Total = &n
			}
		}
		if c.Activity.Coverage != nil {
			v := *c.Activity.Coverage
			c.Activity.Coverage = &v
			if v.TrackingSince != nil {
				at := *v.TrackingSince
				c.Activity.Coverage.TrackingSince = &at
			}
		}
		if c.Inventory.Count != nil {
			n := *c.Inventory.Count
			c.Inventory.Count = &n
		}
		if c.Activity.Total != nil {
			n := *c.Activity.Total
			c.Activity.Total = &n
		}
	}
	s.Recent = slices.Clone(s.Recent)
	s.Checkpoints = slices.Clone(s.Checkpoints)
	return s
}

func emptySummary(now time.Time) Summary {
	now = now.UTC()
	s := Summary{
		Schema: "witself.agent-summary.v2", GeneratedAt: now, RefreshAfterSeconds: 30,
		// GetUsage serializes RFC3339, so the echoed effective interval has
		// second precision. Keep generation/cache time at its original precision.
		Window:     SummaryWindow{Since: now.Truncate(time.Hour).Add(-23 * time.Hour), Until: now.Truncate(time.Second), Bucket: "hour", Timezone: "UTC", PartialCurrentBucket: true},
		Categories: []SummaryCategory{}, Recent: []SummaryUpdate{}, Checkpoints: []SummaryCheckpoint{},
	}
	for _, d := range summaryDefinitions() {
		s.Categories = append(s.Categories, SummaryCategory{Key: d.key, Label: d.label, Code: d.code,
			Inventory: SummaryInventory{Status: "unavailable", Label: d.inventory},
			Activity:  SummaryActivity{Status: "unavailable", Bins: []*int64{}, Unit: d.unitLabel, Dimension: d.dimension},
		})
	}
	s.Categories[0].Inventory.Status = "not_applicable"
	s.Categories[0].Activity.Dimension = "operation"
	s.Categories[3].Activity.Dimension = "memory_change"
	for _, d := range []struct{ key, label string }{{"memory", "Memory curation"}, {"message", "Messaging"}, {"email", "Email"}, {"avatar", "Avatar"}} {
		s.Checkpoints = append(s.Checkpoints, SummaryCheckpoint{Key: d.key, Label: d.label, Status: "unavailable"})
	}
	return s
}

type summaryDefinition struct{ key, label, code, inventory, dimension, unit, unitLabel, action string }

func summaryDefinitions() []summaryDefinition {
	return []summaryDefinition{
		{key: "transactions", label: "Operations", code: "OPS", inventory: "activity only", unitLabel: "recorded operations"},
		{"transcripts", "Transcripts", "TRN", "recent records", "transcript_entry_write", "entry", "entries recorded", "transcript updated"},
		{"facts", "Facts", "FCT", "facts", "fact_returned", "fact", "recorded deliveries", ""},
		{key: "memories", label: "Memories", code: "MEM", inventory: "active memories", unitLabel: "memory changes", action: "memory updated"},
		{"secrets", "Secrets", "SEC", "recent records", "secret_read", "access", "recorded accesses", "secret updated"},
		{"email", "Email", "EML", "recent records", "email_sent", "email", "accepted sends", "email received"},
		{"messages", "Messages", "MSG", "recent records", "message_sent", "message", "messages sent", "message received"},
	}
}

func summaryNumber(n int64) bool { return n >= 0 && n <= summaryMaxInteger }
