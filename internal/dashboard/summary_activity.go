package dashboard

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

var activityDimensions = []struct{ dimension, unit string }{
	{"operation_read", "operation"}, {"operation_write", "operation"},
	{"operation_read_record", "record"}, {"operation_write_record", "record"},
	{"memory_created", "change"}, {"memory_revised", "change"}, {"memory_archived", "change"}, {"memory_restored", "change"}, {"memory_deleted", "change"},
}

func readSummaryActivity(ctx context.Context, cfg Config, w SummaryWindow) (map[string]json.RawMessage, string) {
	q := url.Values{"since": {w.Since.Format(time.RFC3339)}, "until": {w.Until.Format(time.RFC3339)}, "group_by": {"hour"}}
	return summaryGET(ctx, cfg, "/v1/activity?"+q.Encode())
}

// This local decoder deliberately has no dependency on the new backend/client
// API. Presence, bounds, identity and reconciliation precede every projection.
func projectSummaryActivity(s *Summary, want client.SelfIdentity, fields map[string]json.RawMessage, status string) {
	if status != "available" {
		if status != "disabled" && status != "server_update_needed" {
			status = "unavailable"
		}
		for _, i := range []int{0, 3} {
			s.Categories[i].Activity.Status = status
		}
		return
	}
	raw := fields["activity"]
	f := summaryObject(raw)
	var r struct {
		Schema        string     `json:"schema"`
		Catalog       string     `json:"catalog"`
		AccountID     string     `json:"account_id"`
		RealmID       string     `json:"realm_id"`
		AgentID       string     `json:"agent_id"`
		Since         time.Time  `json:"since"`
		Until         time.Time  `json:"until"`
		Bucket        string     `json:"bucket"`
		TrackingSince *time.Time `json:"tracking_since"`
		Points        []struct {
			Dimension string    `json:"dimension"`
			Unit      string    `json:"unit"`
			Quantity  *int64    `json:"quantity"`
			Events    *int64    `json:"event_count"`
			At        time.Time `json:"bucket_start"`
		} `json:"points"`
		Totals []struct {
			Dimension string `json:"dimension"`
			Unit      string `json:"unit"`
			Quantity  *int64 `json:"quantity"`
			Events    *int64 `json:"event_count"`
		} `json:"totals"`
	}
	truncated, valid := summaryBool(f["truncated"])
	if !valid || truncated || f["tracking_since"] == nil || json.Unmarshal(raw, &r) != nil ||
		r.Schema != "witself.agent-activity.v1" || r.Catalog != "core-records.v1" || r.Bucket != "hour" ||
		!summaryIdentityMatches(want, r.AccountID, r.RealmID, r.AgentID) || !r.Since.Equal(s.Window.Since) || !r.Until.Equal(s.Window.Until) ||
		r.Points == nil || r.Totals == nil || len(r.Points) > 24*9 || len(r.Totals) > 9 {
		return
	}
	tracking := r.TrackingSince
	if tracking != nil {
		at := tracking.UTC()
		tracking = &at
	}
	if tracking != nil && (tracking.IsZero() || tracking.Year() < 1 || tracking.After(r.Until)) {
		return
	}
	if tracking == nil && (len(r.Points) > 0 || len(r.Totals) > 0) {
		return
	}
	lookup := func(d, u string) int {
		for i, v := range activityDimensions {
			if v.dimension == d && v.unit == u {
				return i
			}
		}
		return -1
	}
	var quantities, events [9]int64
	var bins [9][24]int64
	var seen [9][24]bool
	var present, totals [9]bool
	for _, p := range r.Points {
		i := lookup(p.Dimension, p.Unit)
		if i < 0 || !validSummaryActivityQuantity(p.Unit, p.Quantity, p.Events) ||
			!p.At.Equal(p.At.UTC().Truncate(time.Hour)) || p.At.Before(r.Since) || !p.At.Before(r.Until) || tracking == nil || p.At.Before(tracking.UTC().Truncate(time.Hour)) {
			return
		}
		h := int(p.At.Sub(r.Since) / time.Hour)
		if h < 0 || h >= 24 || seen[i][h] || *p.Quantity > summaryMaxInteger-quantities[i] || *p.Events > summaryMaxInteger-events[i] {
			return
		}
		seen[i][h], present[i] = true, true
		bins[i][h] = *p.Quantity
		quantities[i] += *p.Quantity
		events[i] += *p.Events
	}
	for _, t := range r.Totals {
		i := lookup(t.Dimension, t.Unit)
		if i < 0 || totals[i] || !present[i] || !validSummaryActivityQuantity(t.Unit, t.Quantity, t.Events) || *t.Quantity != quantities[i] || *t.Events != events[i] {
			return
		}
		totals[i] = true
	}
	for i := range present {
		if present[i] != totals[i] {
			return
		}
	}
	// Aggregate only operations together, and only memory changes together.
	projected := make([]SummaryActivity, 2)
	for n, category := range []int{0, 3} {
		a := s.Categories[category].Activity
		a.Status = "not_tracked"
		a.Bins = make([]*int64, 24)
		a.Coverage = &SummaryCoverage{TrackingSince: tracking}
		if tracking != nil {
			a.Status = "available"
			a.Coverage.PartialFirstBucket = !tracking.Before(r.Since)
			start, end := 0, 2
			if category == 3 {
				start, end = 4, 9
			}
			var total int64
			for i := start; i < end; i++ {
				if quantities[i] > summaryMaxInteger-total {
					return
				}
				total += quantities[i]
			}
			a.Total = &total
			for h := 0; h < 24; h++ {
				if r.Since.Add(time.Duration(h) * time.Hour).Before(tracking.UTC().Truncate(time.Hour)) {
					continue
				}
				var v int64
				for i := start; i < end; i++ {
					if bins[i][h] > summaryMaxInteger-v {
						return
					}
					v += bins[i][h]
				}
				a.Bins[h] = &v
			}
			if category == 0 {
				end = 4
			}
			for i := start; i < end; i++ {
				v := quantities[i]
				a.Breakdown = append(a.Breakdown, SummaryMeasure{Dimension: activityDimensions[i].dimension, Unit: activityDimensions[i].unit, Total: &v})
			}
		}
		projected[n] = a
	}
	s.Categories[0].Activity = projected[0]
	s.Categories[3].Activity = projected[1]
}

func validSummaryActivityQuantity(unit string, quantity, events *int64) bool {
	if quantity == nil || events == nil || *quantity <= 0 || *events <= 0 || !summaryNumber(*quantity) || !summaryNumber(*events) {
		return false
	}
	if unit == "record" {
		return *quantity >= *events
	}
	return *quantity == *events
}

// ValidSummaryActivity is the shared strict consumer contract. Labels never
// come from the wire. JS mirrors these checks for the web projection.
func ValidSummaryActivity(a SummaryActivity, key string, w SummaryWindow) bool {
	modern := key == "transactions" || key == "memories"
	dimension, unit := "", ""
	for _, d := range summaryDefinitions() {
		if d.key == key {
			dimension, unit = d.dimension, d.unitLabel
		}
	}
	if key == "transactions" {
		dimension = "operation"
	}
	if key == "memories" {
		dimension = "memory_change"
	}
	if a.Dimension != dimension || a.Unit != unit {
		return false
	}
	switch a.Status {
	case "unavailable", "disabled", "server_update_needed":
		return (modern || a.Status != "server_update_needed") && a.Total == nil && len(a.Bins) == 0 && a.Coverage == nil && len(a.Breakdown) == 0
	case "not_tracked":
		if !modern || a.Total != nil || len(a.Bins) != 24 || a.Coverage == nil || a.Coverage.TrackingSince != nil || a.Coverage.PartialFirstBucket || len(a.Breakdown) != 0 {
			return false
		}
		for _, v := range a.Bins {
			if v != nil {
				return false
			}
		}
		return true
	case "available":
	default:
		return false
	}
	if len(a.Bins) != 24 || a.Total == nil || !summaryNumber(*a.Total) {
		return false
	}
	var tracking *time.Time
	if modern {
		if a.Coverage == nil || a.Coverage.TrackingSince == nil {
			return false
		}
		tracking = a.Coverage.TrackingSince
		if tracking.IsZero() || tracking.Year() < 1 || tracking.After(w.Until) || a.Coverage.PartialFirstBucket != !tracking.Before(w.Since) {
			return false
		}
	} else if a.Coverage != nil || len(a.Breakdown) != 0 {
		return false
	}
	var sum int64
	for h, v := range a.Bins {
		unknown := tracking != nil && w.Since.Add(time.Duration(h)*time.Hour).Before(tracking.UTC().Truncate(time.Hour))
		if unknown {
			if v != nil {
				return false
			}
			continue
		}
		if v == nil || !summaryNumber(*v) || *v > summaryMaxInteger-sum {
			return false
		}
		sum += *v
	}
	if sum != *a.Total {
		return false
	}
	if modern {
		start, end := 0, 4
		if key == "memories" {
			start, end = 4, 9
		}
		if len(a.Breakdown) != end-start {
			return false
		}
		sum = 0
		for i, b := range a.Breakdown {
			d := activityDimensions[start+i]
			if b.Dimension != d.dimension || b.Unit != d.unit || b.Total == nil || !summaryNumber(*b.Total) {
				return false
			}
			if key == "memories" || i < 2 {
				if *b.Total > summaryMaxInteger-sum {
					return false
				}
				sum += *b.Total
			}
		}
		if sum != *a.Total {
			return false
		}
	}
	return true
}
