package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/activity"
	"github.com/witwave-ai/witself/internal/store"
)

func TestActivityReportAdapter(t *testing.T) {
	start := time.Date(2026, 9, 22, 1, 2, 3, 0, time.UTC)
	in := store.ActivityReport{Schema: activity.Schema, Catalog: activity.CatalogVersion, AccountID: "acc_1", RealmID: "rlm_1", AgentID: "agt_1", Since: start.Truncate(time.Hour), Until: start, Bucket: "hour", TrackingSince: &start, Points: []store.UsagePoint{{Dimension: "operation_read", Unit: "operation", Quantity: 1, EventCount: 1, BucketStart: start.Truncate(time.Hour)}}, Totals: []store.UsageTotal{{Dimension: "operation_read", Unit: "operation", Quantity: 1, EventCount: 1}}}
	out := toServerActivityReport(in)
	if out.Schema != in.Schema || out.Catalog != in.Catalog || out.AccountID != in.AccountID || out.RealmID != in.RealmID || out.AgentID != in.AgentID || !out.Since.Equal(in.Since) || !out.Until.Equal(in.Until) || out.Bucket != in.Bucket || !out.TrackingSince.Equal(start) || out.Truncated || len(out.Points) != 1 || len(out.Totals) != 1 {
		t.Fatal("activity report lost fields")
	}
	usage := toServerUsageReport(store.UsageReport{Points: in.Points, Totals: in.Totals})
	if !reflect.DeepEqual(out.Points, usage.Points) || !reflect.DeepEqual(out.Totals, usage.Totals) {
		t.Fatal("usage quantities changed")
	}
	empty := toServerActivityReport(store.ActivityReport{})
	if empty.TrackingSince != nil || empty.Points == nil || empty.Totals == nil {
		t.Fatal("empty contract changed")
	}
}
