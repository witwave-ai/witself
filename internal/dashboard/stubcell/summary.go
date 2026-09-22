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
