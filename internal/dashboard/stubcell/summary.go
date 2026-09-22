package stubcell

import (
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

// SummaryUsage supplies only synthetic recorded quantities for browser acceptance.
// It echoes the requested interval so acceptance exercises the real projection.
func SummaryUsage(id client.SelfIdentity, since, until time.Time) client.UsageReport {
	r := client.UsageReport{AccountID: id.AccountID, RealmID: id.RealmID, AgentID: id.AgentID,
		Since: since, Until: until, Bucket: "hour", Points: []client.UsagePoint{}, Totals: []client.UsageTotal{}}
	for i, d := range []struct{ dimension, unit string }{
		{"transcript_entry_write", "entry"}, {"fact_returned", "fact"}, {"secret_read", "access"}, {"email_sent", "email"}, {"message_sent", "message"},
	} {
		var total int64
		var events int64
		for at := since.UTC().Truncate(time.Hour); at.Before(until) && events < 24; at = at.Add(time.Hour) {
			n := int64((at.Hour() + i) % 11)
			r.Points = append(r.Points, client.UsagePoint{Dimension: d.dimension, Unit: d.unit, BucketStart: at, Quantity: n, EventCount: 1})
			total += n
			events++
		}
		if events > 0 {
			r.Totals = append(r.Totals, client.UsageTotal{Dimension: d.dimension, Unit: d.unit, Quantity: total, EventCount: events})
		}
	}
	return r
}

// SummaryActivity is a synthetic partial-coverage report for both consoles.
// It never uses accounts, registry, credentials or a database.
func SummaryActivity(id client.SelfIdentity, since, until time.Time) map[string]any {
	tracking := since.Add(6*time.Hour + 17*time.Minute)
	points, totals := []map[string]any{}, []map[string]any{}
	for i, d := range []struct{ dimension, unit string }{
		{"operation_read", "operation"}, {"operation_write", "operation"}, {"operation_read_record", "record"}, {"operation_write_record", "record"},
		{"memory_created", "change"}, {"memory_revised", "change"}, {"memory_archived", "change"}, {"memory_restored", "change"}, {"memory_deleted", "change"},
	} {
		n := int64(i + 1)
		events := n
		if d.unit == "record" {
			events = 1
		}
		points = append(points, map[string]any{"dimension": d.dimension, "unit": d.unit, "quantity": n, "event_count": events, "bucket_start": tracking.Truncate(time.Hour)})
		totals = append(totals, map[string]any{"dimension": d.dimension, "unit": d.unit, "quantity": n, "event_count": events})
	}
	return map[string]any{"schema": "witself.agent-activity.v1", "catalog": "core-records.v1", "account_id": id.AccountID, "realm_id": id.RealmID, "agent_id": id.AgentID, "since": since, "until": until, "bucket": "hour", "tracking_since": tracking, "points": points, "totals": totals, "truncated": false}
}
