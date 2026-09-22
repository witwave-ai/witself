package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/dashboard/stubcell"
)

func activityFixture() map[string]any {
	s := emptySummary(summaryTestNow)
	return stubcell.SummaryActivity(testIdentity, s.Window.Since, s.Window.Until)
}
func projectActivityFixture(r map[string]any) Summary {
	s := emptySummary(summaryTestNow)
	raw, _ := json.Marshal(map[string]any{"activity": r})
	projectSummaryActivity(&s, testIdentity, summaryObject(raw), "available")
	return s
}
func TestSummaryActivityCountsCoverageAndClone(t *testing.T) {
	s := projectActivityFixture(activityFixture())
	for _, tc := range []struct {
		i        int
		total    int64
		measures int
	}{{0, 3, 4}, {3, 35, 5}} {
		a := s.Categories[tc.i].Activity
		if a.Status != "available" || *a.Total != tc.total || len(a.Breakdown) != tc.measures || !a.Coverage.PartialFirstBucket || !ValidSummaryActivity(a, s.Categories[tc.i].Key, s.Window) {
			t.Fatalf("bad projection: %+v", a)
		}
		for h, v := range a.Bins {
			if h < 6 {
				if v != nil {
					t.Fatal("invented history")
				}
				continue
			}
			if v == nil || h == 6 && *v != tc.total || h > 6 && *v != 0 {
				t.Fatalf("bin %d: %v", h, v)
			}
		}
	}
	if s.Categories[0].Inventory.Status != "not_applicable" {
		t.Fatal("operations inventory")
	}
	for i, b := range s.Categories[0].Activity.Breakdown {
		if *b.Total != int64(i+1) {
			t.Fatal("operation/record quantities")
		}
	}
	for i, b := range s.Categories[3].Activity.Breakdown {
		if *b.Total != int64(i+5) {
			t.Fatal("memory changes")
		}
	}
	cloned := cloneSummary(s)
	*cloned.Categories[0].Activity.Bins[6] = 999
	*cloned.Categories[0].Activity.Breakdown[0].Total = 999
	*cloned.Categories[0].Activity.Coverage.TrackingSince = summaryTestNow
	if *s.Categories[0].Activity.Bins[6] != 3 || *s.Categories[0].Activity.Breakdown[0].Total != 1 || s.Categories[0].Activity.Coverage.TrackingSince.Equal(summaryTestNow) {
		t.Fatal("clone aliases")
	}
}
func TestSummaryActivityEmptyCoverage(t *testing.T) {
	for _, when := range []string{"none", "before window", "exact hour", "inside hour"} {
		t.Run(when, func(t *testing.T) {
			r := activityFixture()
			r["points"] = []any{}
			r["totals"] = []any{}
			s := emptySummary(summaryTestNow)
			switch when {
			case "none":
				r["tracking_since"] = nil
			case "before window":
				r["tracking_since"] = s.Window.Since.Add(-time.Hour)
			case "exact hour":
				r["tracking_since"] = s.Window.Since.Add(6 * time.Hour)
			}
			s = projectActivityFixture(r)
			for _, i := range []int{0, 3} {
				a := s.Categories[i].Activity
				if !ValidSummaryActivity(a, s.Categories[i].Key, s.Window) {
					t.Fatal("consumer rejected")
				}
				if when == "none" {
					if a.Status != "not_tracked" || a.Total != nil {
						t.Fatal("no marker")
					}
					for _, v := range a.Bins {
						if v != nil {
							t.Fatal("unknown became zero")
						}
					}
				} else if a.Status != "available" || *a.Total != 0 {
					t.Fatal("valid measured zero")
				}
			}
		})
	}
}
func TestSummaryActivityRejectsMalformed(t *testing.T) {
	tests := map[string]func(map[string]any){
		"schema": func(r map[string]any) { r["schema"] = "private" }, "catalog": func(r map[string]any) { r["catalog"] = "private" },
		"agent": func(r map[string]any) { r["agent_id"] = "other" }, "account": func(r map[string]any) { r["account_id"] = "other" }, "realm": func(r map[string]any) { r["realm_id"] = "other" },
		"since": func(r map[string]any) { r["since"] = summaryTestNow }, "until": func(r map[string]any) { r["until"] = summaryTestNow.Add(time.Hour) }, "bucket": func(r map[string]any) { r["bucket"] = "day" },
		"truncated": func(r map[string]any) { r["truncated"] = true }, "missing truncated": func(r map[string]any) { delete(r, "truncated") },
		"missing marker": func(r map[string]any) { delete(r, "tracking_since") }, "null marker with events": func(r map[string]any) { r["tracking_since"] = nil },
		"future marker": func(r map[string]any) { r["tracking_since"] = summaryTestNow.Add(time.Hour) }, "bad marker": func(r map[string]any) { r["tracking_since"] = "2026-02-30T00:00:00Z" },
		"null points": func(r map[string]any) { r["points"] = nil }, "null totals": func(r map[string]any) { r["totals"] = nil },
		"unmatched":         func(r map[string]any) { r["totals"] = []any{} },
		"duplicate point":   func(r map[string]any) { p := r["points"].([]map[string]any); r["points"] = append(p, p[0]) },
		"duplicate total":   func(r map[string]any) { p := r["totals"].([]map[string]any); r["totals"] = append(p[:8], p[0]) },
		"point cardinality": func(r map[string]any) { r["points"] = make([]any, 217) }, "total cardinality": func(r map[string]any) { r["totals"] = make([]any, 10) },
		"before marker": func(r map[string]any) {
			r["points"].([]map[string]any)[0]["bucket_start"] = emptySummary(summaryTestNow).Window.Since
		},
		"unaligned": func(r map[string]any) { r["points"].([]map[string]any)[0]["bucket_start"] = summaryTestNow },
	}
	for _, section := range []string{"points", "totals"} {
		for _, field := range []string{"quantity", "event_count", "dimension", "unit"} {
			for _, kind := range []string{"missing", "null", "wrong", "overflow", "fraction"} {
				tests[section+field+kind] = func(r map[string]any) {
					row := r[section].([]map[string]any)[0]
					switch kind {
					case "missing":
						delete(row, field)
					case "null":
						row[field] = nil
					case "wrong":
						row[field] = "PRIVATE"
					case "overflow":
						row[field] = summaryMaxInteger + 1
					case "fraction":
						row[field] = 0.5
					}
				}
			}
		}
	}
	tests["aggregate overflow"] = func(r map[string]any) {
		for _, section := range []string{"points", "totals"} {
			p := r[section].([]map[string]any)
			p[0]["quantity"] = summaryMaxInteger
			p[1]["quantity"] = summaryMaxInteger
			p[0]["event_count"] = summaryMaxInteger
			p[1]["event_count"] = summaryMaxInteger
		}
	}
	tests["dimension overflow"] = func(r map[string]any) {
		p := r["points"].([]map[string]any)
		p[0]["quantity"] = summaryMaxInteger
		p[0]["event_count"] = summaryMaxInteger
		p = append(p, map[string]any{"dimension": "operation_read", "unit": "operation", "quantity": 1, "event_count": 1, "bucket_start": summaryTestNow.Truncate(time.Hour)})
		r["points"] = p
	}
	tests["wrong unit"] = func(r map[string]any) { r["points"].([]map[string]any)[0]["unit"] = "record" }
	for _, name := range []string{"zero operation", "zero events", "operation count mismatch", "change count mismatch", "records below events"} {
		tests[name] = func(r map[string]any) {
			for _, section := range []string{"points", "totals"} {
				rows := r[section].([]map[string]any)
				switch name {
				case "zero operation":
					rows[0]["quantity"], rows[0]["event_count"] = 0, 0
				case "zero events":
					rows[0]["event_count"] = 0
				case "operation count mismatch":
					rows[0]["event_count"] = 2
				case "change count mismatch":
					rows[4]["event_count"] = 1
				case "records below events":
					rows[2]["event_count"] = 4
				}
			}
		}
	}
	tests["mismatch total"] = func(r map[string]any) { r["totals"].([]map[string]any)[0]["quantity"] = 2 }
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			r := activityFixture()
			mutate(r)
			s := projectActivityFixture(r)
			for _, i := range []int{0, 3} {
				a := s.Categories[i].Activity
				if a.Status != "unavailable" || a.Total != nil || len(a.Bins) != 0 {
					t.Fatalf("accepted %+v", a)
				}
			}
		})
	}
}

func TestSummaryActivityRejectsBucketAtExclusiveUntil(t *testing.T) {
	s := emptySummary(summaryTestNow.Truncate(time.Hour))
	r := stubcell.SummaryActivity(testIdentity, s.Window.Since, s.Window.Until)
	r["points"].([]map[string]any)[0]["bucket_start"] = s.Window.Until
	raw, err := json.Marshal(map[string]any{"activity": r})
	if err != nil {
		t.Fatal(err)
	}
	projectSummaryActivity(&s, testIdentity, summaryObject(raw), "available")
	for _, i := range []int{0, 3} {
		if s.Categories[i].Activity.Status != "unavailable" {
			t.Fatal("accepted a point outside the exclusive reporting window")
		}
	}
}

func TestSummaryActivityIndependentLoopback(t *testing.T) {
	for _, mode := range []string{"valid", "old", "disabled", "forbidden", "malformed", "partial", "oversize", "nonfinite"} {
		t.Run(mode, func(t *testing.T) {
			backend := summaryTestBackend(t, summaryTestNow)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/activity" {
					backend(w, r)
					return
				}
				q := r.URL.Query()
				window := emptySummary(summaryTestNow).Window
				if r.Method != "GET" || len(q) != 3 || q.Get("group_by") != "hour" || q.Get("since") != window.Since.Format(time.RFC3339) || q.Get("until") != window.Until.Format(time.RFC3339) {
					t.Error("activity fetch scope")
				}
				switch mode {
				case "old":
					http.NotFound(w, r)
				case "disabled":
					w.WriteHeader(403)
					_, _ = w.Write([]byte(`{"code":"feature_not_enabled"}`))
				case "forbidden":
					w.WriteHeader(403)
				case "malformed":
					_, _ = w.Write([]byte(`{"activity":{}}`))
				case "partial":
					w.Header().Set("Content-Length", "5000")
					_, _ = w.Write([]byte(`{"activity":`))
				case "oversize":
					_, _ = w.Write([]byte(strings.Repeat(" ", summaryResponseLimit+1)))
				case "nonfinite":
					_, _ = w.Write([]byte(`{"activity":{"quantity":NaN}}`))
				default:
					writeJSON(w, map[string]any{"activity": activityFixture(), "private": "PRIVATE"})
				}
			}))
			defer server.Close()
			s := collectSummary(t.Context(), Config{Endpoint: server.URL, Identity: testIdentity}, summaryTestNow)
			want := "unavailable"
			switch mode {
			case "valid":
				want = "available"
			case "old":
				want = "server_update_needed"
			case "disabled":
				want = "disabled"
			}
			for _, i := range []int{0, 3} {
				if s.Categories[i].Activity.Status != want {
					t.Fatalf("%s: %+v", mode, s.Categories[i].Activity)
				}
			}
			for _, i := range []int{1, 2, 4, 5, 6} {
				if s.Categories[i].Activity.Status != "available" {
					t.Fatal("legacy usage lost")
				}
			}
			if *s.Categories[3].Inventory.Count != 207 {
				t.Fatal("active inventory lost")
			}
			raw, _ := json.Marshal(s)
			if strings.Contains(string(raw), "PRIVATE") {
				t.Fatal("content retained")
			}
		})
	}
}
