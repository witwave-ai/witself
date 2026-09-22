package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
)

func TestSummaryUsageRequiresExplicitWireValues(t *testing.T) {
	for _, tc := range []struct{ section, field string }{{"points", "quantity"}, {"points", "event_count"}, {"totals", "quantity"}, {"totals", "event_count"}, {"report", "truncated"}} {
		for _, kind := range []string{"absent", "null", "wrong type", "explicit zero"} {
			t.Run(tc.section+"/"+tc.field+"/"+kind, func(t *testing.T) {
				s := emptySummary(summaryTestNow)
				report := summaryUsageFixture(summaryTestNow)
				report.Points = []client.UsagePoint{{Dimension: "fact_returned", Unit: "fact", BucketStart: s.Window.Since}}
				report.Totals = []client.UsageTotal{{Dimension: "fact_returned", Unit: "fact"}}
				raw, _ := json.Marshal(report)
				fields := summaryObject(raw)
				target := fields
				var rows []map[string]json.RawMessage
				if tc.section != "report" {
					if err := json.Unmarshal(fields[tc.section], &rows); err != nil {
						t.Fatal(err)
					}
					target = rows[0]
				}
				switch kind {
				case "absent":
					delete(target, tc.field)
				case "null":
					target[tc.field] = json.RawMessage(`null`)
				case "wrong type":
					target[tc.field] = json.RawMessage(`"0"`)
				}
				if tc.section != "report" {
					fields[tc.section], _ = json.Marshal(rows)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]any{"usage": fields}) }))
				defer server.Close()
				got, ok := readSummaryUsage(t.Context(), Config{Endpoint: server.URL}, s.Window)
				if ok != (kind == "explicit zero") {
					t.Fatalf("accepted=%v", ok)
				}
				if ok {
					projectSummaryUsage(&s, testIdentity, got)
					if s.Categories[2].Activity.Status != "available" || *s.Categories[2].Activity.Total != 0 {
						t.Fatal("explicit zero rejected")
					}
				}
			})
		}
	}
}
