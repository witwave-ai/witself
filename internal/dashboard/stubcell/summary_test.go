package stubcell_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard/stubcell"
)

func TestSummaryUsageFixtureRoundTrip(t *testing.T) {
	s := httptest.NewServer(stubcell.New(stubcell.Config{}))
	defer s.Close()
	now := time.Date(2026, 9, 22, 16, 37, 0, 0, time.UTC)
	q := client.UsageQuery{Since: now.Truncate(time.Hour).Add(-23 * time.Hour), Until: now, Bucket: "hour"}
	r, err := client.GetUsage(context.Background(), s.URL, "", q)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Since.Equal(q.Since) || !r.Until.Equal(q.Until) || len(r.Points) != 120 || len(r.Totals) != 5 || r.AgentID != stubcell.Identity("acceptance").AgentID {
		t.Fatal("incomplete summary fixture")
	}
	for _, total := range r.Totals {
		var sum int64
		for _, p := range r.Points {
			if p.Dimension == total.Dimension {
				sum += p.Quantity
			}
		}
		if sum != total.Quantity {
			t.Fatal("fixture totals mismatch")
		}
	}
}
