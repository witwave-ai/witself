package dashboard

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

// Metering is stored usage, not an audit-complete action history. Validation is
// all-or-nothing: only a complete matching report makes absent hour bins zero.
func projectSummaryUsage(s *Summary, want client.SelfIdentity, report client.UsageReport) {
	if !summaryIdentityMatches(want, report.AccountID, report.RealmID, report.AgentID) || report.Truncated || report.Bucket != "hour" ||
		!report.Since.Equal(s.Window.Since) || !report.Until.Equal(s.Window.Until) || report.Points == nil || report.Totals == nil || len(report.Points) > 120 || len(report.Totals) > 5 {
		return
	}
	definitions := summaryDefinitions()
	dimensions := map[string]int{}
	for i, d := range definitions {
		if d.dimension != "" {
			dimensions[d.dimension] = i
		}
	}
	type aggregate struct {
		quantity, events int64
		bins             [24]int64
		seen             [24]bool
		present          bool
	}
	sums := make([]aggregate, len(definitions))
	for _, p := range report.Points {
		i, ok := dimensions[p.Dimension]
		if !ok || p.Unit != definitions[i].unit || !summaryNumber(p.Quantity) || !summaryNumber(p.EventCount) ||
			!p.BucketStart.Equal(p.BucketStart.UTC().Truncate(time.Hour)) || p.BucketStart.Before(s.Window.Since) || p.BucketStart.After(s.Window.Until) {
			return
		}
		bin := int(p.BucketStart.Sub(s.Window.Since) / time.Hour)
		if bin < 0 || bin >= 24 {
			return
		}
		a := &sums[i]
		if a.seen[bin] || p.Quantity > summaryMaxInteger-a.quantity || p.EventCount > summaryMaxInteger-a.events {
			return
		}
		a.seen[bin], a.present = true, true
		a.bins[bin] = p.Quantity
		a.quantity += p.Quantity
		a.events += p.EventCount
	}
	seen := map[string]bool{}
	for _, total := range report.Totals {
		i, ok := dimensions[total.Dimension]
		if !ok || seen[total.Dimension] || total.Unit != definitions[i].unit || !summaryNumber(total.Quantity) || !summaryNumber(total.EventCount) {
			return
		}
		seen[total.Dimension] = true
		a := sums[i]
		if !a.present || total.Quantity != a.quantity || total.EventCount != a.events {
			return
		}
	}
	for dimension, i := range dimensions {
		if sums[i].present != seen[dimension] {
			return
		}
	}
	for _, i := range dimensions {
		a := &s.Categories[i].Activity
		a.Status = "available"
		a.Bins = make([]*int64, 24)
		for j, v := range sums[i].bins {
			n := v
			a.Bins[j] = &n
		}
		n := sums[i].quantity
		a.Total = &n
	}
}

// The ordinary usage client intentionally exposes value types. This display
// must also distinguish absent/null quantities from measured zeros, so validate
// wire presence inside the summary boundary before using the typed projector.
// It uses the same existing API, with a bounded response and no redirects.
func readSummaryUsage(ctx context.Context, cfg Config, window SummaryWindow) (client.UsageReport, bool) {
	q := url.Values{"since": {window.Since.Format(time.RFC3339)}, "until": {window.Until.Format(time.RFC3339)}, "group_by": {"hour"}}
	for _, d := range summaryDefinitions() {
		if d.dimension != "" {
			q.Add("dimension", d.dimension)
		}
	}
	f, status := summaryGET(ctx, cfg, "/v1/usage?"+q.Encode())
	if status != "available" {
		return client.UsageReport{}, false
	}
	raw := f["usage"]
	u := summaryObject(raw)
	truncated, valid := summaryBool(u["truncated"])
	if !valid || truncated {
		return client.UsageReport{}, false
	}
	for key, bound := range map[string]int{"points": 120, "totals": 5} {
		var rows []map[string]json.RawMessage
		if json.Unmarshal(u[key], &rows) != nil || rows == nil || len(rows) > bound {
			return client.UsageReport{}, false
		}
		for _, row := range rows {
			for _, field := range []string{"quantity", "event_count"} {
				var value *int64
				if json.Unmarshal(row[field], &value) != nil || value == nil || !summaryNumber(*value) {
					return client.UsageReport{}, false
				}
			}
		}
	}
	var report client.UsageReport
	if json.Unmarshal(raw, &report) != nil {
		return client.UsageReport{}, false
	}
	return report, true
}
