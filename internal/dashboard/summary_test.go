package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

var summaryTestNow = time.Date(2026, 9, 22, 16, 25, 7, 123456789, time.UTC)

func summaryUsageFixture(now time.Time) client.UsageReport {
	s := emptySummary(now)
	return client.UsageReport{AccountID: testIdentity.AccountID, RealmID: testIdentity.RealmID, AgentID: testIdentity.AgentID, Since: s.Window.Since, Until: s.Window.Until, Bucket: "hour", Points: []client.UsagePoint{}, Totals: []client.UsageTotal{}}
}

func summaryTestBackend(t *testing.T, now time.Time) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("non-passive request %s %s", r.Method, r.URL.Path)
			http.Error(w, "forbidden", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/v1/self":
			q := r.URL.Query()
			for _, key := range []string{"include_counts", "include_checkpoint", "include_message_checkpoint", "include_email_checkpoint", "include_avatar_checkpoint", "observational"} {
				if q.Get(key) != "true" {
					t.Errorf("self missing %s", key)
				}
			}
			for _, key := range []string{"include_facts", "include_salient", "include_sensitive"} {
				if q.Get(key) != "false" {
					t.Errorf("self requests content: %s", key)
				}
			}
			if q.Get("max_bytes") != "16384" || r.Header.Get("X-Witself-Hydration") != "" {
				t.Error("unbounded or hook self request")
			}
			writeJSON(w, map[string]any{"identity": testIdentity, "index": map[string]any{"counts": map[string]any{"facts": 321, "memories": 207}}, "memory_checkpoint": map[string]any{"pending": true, "request_id": "POISON_FENCE"}, "message_checkpoint": map[string]any{"pending": false, "enabled": true}, "email_checkpoint": map[string]any{"pending": true, "enabled": false}, "avatar_checkpoint": map[string]any{"pending": true, "unavailable": true, "reason": "POISON_REASON"}, "salient_memories": []any{"POISON_CONTENT"}, "primary_facts": []any{"POISON_VALUE"}})
		case "/v1/usage":
			q := r.URL.Query()
			expected := []string{"transcript_entry_write", "fact_returned", "secret_read", "email_sent", "message_sent"}
			if !reflect.DeepEqual(q["dimension"], expected) || q.Get("group_by") != "hour" || q.Has("allow_truncation") {
				t.Errorf("usage query: %v", q)
			}
			report := summaryUsageFixture(now)
			if q.Get("since") != report.Since.Format(time.RFC3339) || q.Get("until") != report.Until.Format(time.RFC3339) {
				t.Errorf("wrong interval: %v", q)
			}
			report.AgentName = "POISON_NAME"
			report.Points = []client.UsagePoint{{Dimension: "fact_returned", Unit: "fact", BucketStart: now.Truncate(time.Hour), Quantity: 2, EventCount: 1}}
			report.Totals = []client.UsageTotal{{Dimension: "fact_returned", Unit: "fact", Quantity: 2, EventCount: 1}}
			writeJSON(w, map[string]any{"usage": report, "extra": "POISON_CONTENT"})
		case "/v1/transcripts", "/v1/memories", "/v1/secrets", "/v1/messages", "/v1/email":
			key := strings.TrimPrefix(r.URL.Path, "/v1/")
			if key != "transcripts" && r.URL.Query().Get("limit") != "100" {
				t.Error("unbounded metadata query")
			}
			if key == "messages" && r.URL.Query().Get("direction") != "inbox" {
				t.Error("non-inbox query")
			}
			if key == "secrets" && r.URL.Query().Get("include_fields") != "false" {
				t.Error("secret fields requested")
			}
			if key == "email" {
				key = "messages"
			}
			if key == "memories" || key == "secrets" {
				key = "items"
			}
			records := []map[string]any{}
			for i := 0; i < 14; i++ {
				records = append(records, map[string]any{"id": fmt.Sprintf("POISON_ID_%d", i), "name": "POISON_NAME", "title": "POISON_TITLE", "subject": "POISON_SUBJECT", "body": "POISON_BODY", "value": "POISON_VALUE", "ciphertext": "POISON_CIPHER", "email": "POISON_EMAIL@example.invalid", "updated_at": now.Add(-time.Duration(i+1) * time.Minute), "created_at": now.Add(-time.Hour), "received_at": now.Add(-time.Duration(i+1) * time.Minute), "processing": map[string]any{"claim_id": "POISON_FENCE"}})
			}
			// Use the existing client wire types for the two items envelopes.
			data, _ := json.Marshal(records)
			switch r.URL.Path {
			case "/v1/memories":
				var items []client.Memory
				if err := json.Unmarshal(data, &items); err != nil {
					t.Error(err)
				}
				writeJSON(w, client.MemoryPage{Items: items, NextCursor: "POISON_CURSOR"})
			case "/v1/secrets":
				var items []client.Secret
				if err := json.Unmarshal(data, &items); err != nil {
					t.Error(err)
				}
				writeJSON(w, client.SecretPage{Items: items, NextCursor: "POISON_CURSOR"})
			default:
				writeJSON(w, map[string]any{key: records, "next_cursor": "POISON_CURSOR"})
			}
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func TestSummaryProjectionPassiveAndContentFree(t *testing.T) {
	var calls atomic.Int32
	backend := summaryTestBackend(t, summaryTestNow)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); backend(w, r) }))
	defer server.Close()
	reader, err := NewReader(Config{Endpoint: server.URL, Identity: testIdentity})
	if err != nil {
		t.Fatal(err)
	}
	reader.summary.now = func() time.Time { return summaryTestNow }
	raw, err := reader.Read(t.Context(), ReadRequest{Resource: ResourceSummary})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "POISON") || strings.Contains(string(raw), testIdentity.AgentID) {
		t.Fatalf("content leak: %s", raw)
	}
	var envelope struct {
		Summary Summary `json:"summary"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	s := envelope.Summary
	if s.Schema != "witself.agent-summary.v1" || s.RefreshAfterSeconds != 30 || !s.GeneratedAt.Equal(summaryTestNow) || len(s.Categories) != 7 || len(s.Recent) != 12 || len(s.Checkpoints) != 4 {
		t.Fatalf("shape: %+v", s)
	}
	for i, code := range []string{"OPS", "TRN", "FCT", "MEM", "SEC", "EML", "MSG"} {
		if s.Categories[i].Code != code {
			t.Fatal("order")
		}
	}
	for i, n := range map[int]int64{1: 14, 2: 321, 3: 207, 4: 14, 6: 14} {
		c := s.Categories[i]
		if c.Inventory.Count == nil || *c.Inventory.Count != n || c.Inventory.Exact != (i == 2 || i == 3) {
			t.Fatalf("inventory %+v", c)
		}
	}
	if s.Categories[0].Inventory.Status != "unavailable" || s.Categories[3].Activity.Status != "unavailable" || s.Categories[5].Inventory.Status != "disabled" || s.Categories[5].Inventory.Count != nil {
		t.Fatal("unknown/disabled semantics")
	}
	if *s.Categories[2].Activity.Total != 2 || s.Categories[2].Activity.Bins[23] != 2 || *s.Categories[1].Activity.Total != 0 || len(s.Categories[1].Activity.Bins) != 24 {
		t.Fatal("activity bins")
	}
	for i, status := range []string{"pending", "clear", "disabled", "unavailable"} {
		if s.Checkpoints[i].Status != status {
			t.Fatal(s.Checkpoints)
		}
	}
	for i, u := range s.Recent {
		if u.Key == "facts" || u.Key == "email" || u.At.After(summaryTestNow) || (i > 0 && u.At.After(s.Recent[i-1].At)) {
			t.Fatal("recent projection")
		}
	}
	if calls.Load() != 7 {
		t.Fatalf("calls %d", calls.Load())
	}
	second, err := reader.Read(t.Context(), ReadRequest{Resource: ResourceSummary})
	if err != nil || string(second) != string(raw) || calls.Load() != 7 {
		t.Fatal("cache reuse", err)
	}
	for _, request := range []ReadRequest{{Resource: ResourceSummary, ID: "id"}, {Resource: ResourceSummary, Query: url.Values{"limit": {"1"}}}} {
		if _, err := reader.Read(t.Context(), request); err == nil {
			t.Fatal("invalid summary input accepted")
		}
	}
}

func TestSummaryCheckpointSemantics(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`null`, "unavailable"}, {`{}`, "unavailable"}, {`{"pending":null}`, "unavailable"}, {`{"pending":"false"}`, "unavailable"}, {`{"Pending":false}`, "unavailable"},
		{`{"pending":true}`, "pending"}, {`{"pending":false}`, "clear"}, {`{"pending":true,"enabled":false}`, "disabled"}, {`{"enabled":false,"pending":"invalid","unavailable":true}`, "disabled"},
		{`{"pending":false,"unavailable":true}`, "unavailable"}, {`{"pending":false,"unavailable":"false"}`, "unavailable"}, {`{"pending":false,"enabled":null}`, "unavailable"}, {`{"pending":false,"enabled":true,"reason":"poison"}`, "clear"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := summaryCheckpoint([]byte(tc.raw)); got != tc.want {
				t.Fatalf("%s want %s", got, tc.want)
			}
		})
	}
}

func TestSummarySelfCountsAndCanonicalIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, counts string
		mutate       func(map[string]json.RawMessage)
		fact, memory string
	}{
		{name: "exact zero", counts: `{"facts":0,"memories":0}`, fact: "available", memory: "available"},
		{name: "missing", counts: `{}`, fact: "unavailable", memory: "unavailable"},
		{name: "null", counts: `{"facts":null,"memories":4}`, fact: "unavailable", memory: "available"},
		{name: "negative", counts: `{"facts":-1,"memories":4}`, fact: "unavailable", memory: "available"},
		{name: "unsafe", counts: `{"facts":9007199254740992,"memories":4}`, fact: "unavailable", memory: "available"},
		{name: "fraction", counts: `{"facts":1.5,"memories":4}`, fact: "unavailable", memory: "available"},
		{name: "max", counts: `{"facts":9007199254740991,"memories":4}`, fact: "available", memory: "available"},
		{name: "account mismatch", counts: `{"facts":1,"memories":4}`, mutate: func(f map[string]json.RawMessage) { f["account_id"] = json.RawMessage(`"wrong"`) }, fact: "unavailable", memory: "unavailable"},
		{name: "realm mismatch", counts: `{"facts":1,"memories":4}`, mutate: func(f map[string]json.RawMessage) { f["realm_id"] = json.RawMessage(`"wrong"`) }, fact: "unavailable", memory: "unavailable"},
		{name: "agent mismatch", counts: `{"facts":1,"memories":4}`, mutate: func(f map[string]json.RawMessage) { delete(f, "agent_id") }, fact: "unavailable", memory: "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := emptySummary(summaryTestNow)
			id, _ := json.Marshal(testIdentity)
			identity := summaryObject(id)
			if tc.mutate != nil {
				tc.mutate(identity)
			}
			id, _ = json.Marshal(identity)
			projectSummarySelf(&s, testIdentity, map[string]json.RawMessage{"identity": id, "index": json.RawMessage(`{"counts":` + tc.counts + `}`)})
			if s.Categories[2].Inventory.Status != tc.fact || s.Categories[3].Inventory.Status != tc.memory {
				t.Fatal(s.Categories)
			}
		})
	}
}

func TestSummaryUsageValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*client.UsageReport)
	}{
		{"account", func(r *client.UsageReport) { r.AccountID = "wrong" }}, {"realm", func(r *client.UsageReport) { r.RealmID = "wrong" }}, {"agent", func(r *client.UsageReport) { r.AgentID = "wrong" }},
		{"since", func(r *client.UsageReport) { r.Since = r.Since.Add(time.Hour) }}, {"until", func(r *client.UsageReport) { r.Until = r.Until.Add(time.Second) }}, {"bucket", func(r *client.UsageReport) { r.Bucket = "day" }},
		{"truncated", func(r *client.UsageReport) { r.Truncated = true }}, {"missing points", func(r *client.UsageReport) { r.Points = nil }}, {"missing totals", func(r *client.UsageReport) { r.Totals = nil }},
		{"unit", func(r *client.UsageReport) { r.Points[0].Unit = "access" }}, {"total unit", func(r *client.UsageReport) { r.Totals[0].Unit = "access" }}, {"dimension", func(r *client.UsageReport) { r.Points[0].Dimension = "email_received" }},
		{"delivered", func(r *client.UsageReport) { r.Points[0].Dimension = "message_delivered" }},
		{"unaligned", func(r *client.UsageReport) { r.Points[0].BucketStart = r.Since.Add(time.Minute) }}, {"before", func(r *client.UsageReport) { r.Points[0].BucketStart = r.Since.Add(-time.Hour) }}, {"future", func(r *client.UsageReport) { r.Points[0].BucketStart = r.Since.Add(24 * time.Hour) }},
		{"negative quantity", func(r *client.UsageReport) { r.Points[0].Quantity = -1 }}, {"negative events", func(r *client.UsageReport) { r.Points[0].EventCount = -1 }}, {"unsafe", func(r *client.UsageReport) { r.Points[0].Quantity = summaryMaxInteger + 1 }},
		{"unsafe events", func(r *client.UsageReport) { r.Points[0].EventCount = summaryMaxInteger + 1 }}, {"duplicate point", func(r *client.UsageReport) { r.Points = append(r.Points, r.Points[0]) }}, {"duplicate total", func(r *client.UsageReport) { r.Totals = append(r.Totals, r.Totals[0]) }},
		{"quantity mismatch", func(r *client.UsageReport) { r.Totals[0].Quantity++ }}, {"event mismatch", func(r *client.UsageReport) { r.Totals[0].EventCount++ }}, {"missing dimension total", func(r *client.UsageReport) { r.Totals = []client.UsageTotal{} }},
		{"extra total", func(r *client.UsageReport) {
			r.Totals = append(r.Totals, client.UsageTotal{Dimension: "email_sent", Unit: "email"})
		}},
		{"sum overflow", func(r *client.UsageReport) {
			r.Points[0].Quantity = summaryMaxInteger
			r.Points = append(r.Points, client.UsagePoint{Dimension: "fact_returned", Unit: "fact", BucketStart: r.Since.Add(time.Hour), Quantity: 1})
		}},
		{"event sum overflow", func(r *client.UsageReport) {
			r.Points[0].EventCount = summaryMaxInteger
			r.Points = append(r.Points, client.UsagePoint{Dimension: "fact_returned", Unit: "fact", BucketStart: r.Since.Add(time.Hour), EventCount: 1})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := emptySummary(summaryTestNow)
			r := summaryUsageFixture(summaryTestNow)
			r.Points = []client.UsagePoint{{Dimension: "fact_returned", Unit: "fact", BucketStart: r.Since, Quantity: 2, EventCount: 1}}
			r.Totals = []client.UsageTotal{{Dimension: "fact_returned", Unit: "fact", Quantity: 2, EventCount: 1}}
			tc.mutate(&r)
			projectSummaryUsage(&s, testIdentity, r)
			for _, c := range s.Categories {
				if c.Activity.Status != "unavailable" || c.Activity.Total != nil || len(c.Activity.Bins) != 0 {
					t.Fatal("partial invalid report projected")
				}
			}
		})
	}
	t.Run("complete empty", func(t *testing.T) {
		s := emptySummary(summaryTestNow)
		projectSummaryUsage(&s, testIdentity, summaryUsageFixture(summaryTestNow))
		for _, i := range []int{1, 2, 4, 5, 6} {
			a := s.Categories[i].Activity
			if a.Status != "available" || len(a.Bins) != 24 || *a.Total != 0 {
				t.Fatal(a)
			}
		}
	})
	t.Run("max integer and edge bins", func(t *testing.T) {
		s := emptySummary(summaryTestNow)
		r := summaryUsageFixture(summaryTestNow)
		r.Points = []client.UsagePoint{{Dimension: "secret_read", Unit: "access", BucketStart: r.Since, Quantity: summaryMaxInteger}, {Dimension: "secret_read", Unit: "access", BucketStart: r.Since.Add(23 * time.Hour), Quantity: 0}}
		r.Totals = []client.UsageTotal{{Dimension: "secret_read", Unit: "access", Quantity: summaryMaxInteger}}
		projectSummaryUsage(&s, testIdentity, r)
		if s.Categories[4].Activity.Status != "available" || *s.Categories[4].Activity.Total != summaryMaxInteger {
			t.Fatal(s)
		}
	})
}

func TestSummaryRecentInvalidAndBounded(t *testing.T) {
	s := emptySummary(summaryTestNow)
	raw := `[{"id":"poison","created_at":"2026-09-21T10:00:00Z","updated_at":"bad"},{"created_at":"2026-09-22T16:00:00Z","updated_at":"2099-01-01T00:00:00Z"},{"updated_at":"2099-01-01T00:00:00Z"},{"updated_at":"0001-01-01T00:00:00Z"}]`
	projectSummaryRecords(&s, 1, []byte(raw))
	if len(s.Recent) != 2 || *s.Categories[1].Inventory.Count != 4 {
		t.Fatal(s)
	}
	records := make([]map[string]string, 150)
	for i := range records {
		records[i] = map[string]string{"updated_at": summaryTestNow.Add(-time.Minute).Format(time.RFC3339)}
	}
	data, _ := json.Marshal(records)
	s = emptySummary(summaryTestNow)
	projectSummaryRecords(&s, 1, data)
	if *s.Categories[1].Inventory.Count != 100 || len(s.Recent) != 100 {
		t.Fatal("unbounded page")
	}
	for _, raw := range []string{`null`, `{}`, `[null]`, ``} {
		s = emptySummary(summaryTestNow)
		projectSummaryRecords(&s, 1, []byte(raw))
		if s.Categories[1].Inventory.Status != "unavailable" {
			t.Fatal("missing became zero")
		}
	}
	s = emptySummary(summaryTestNow)
	projectSummaryRecords(&s, 1, []byte(`[]`))
	if *s.Categories[1].Inventory.Count != 0 {
		t.Fatal("empty page")
	}
}

func TestSummaryIndependentFailuresAndMalformedUsage(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		status      int
	}{
		{"missing", `{}`, 200}, {"malformed", `{"usage":`, 200}, {"wrong numeric type", `{"usage":{"points":[{"quantity":"3"}]}}`, 200}, {"integer overflow", `{"usage":{"points":[{"quantity":9223372036854775808}]}}`, 200}, {"truncated", `{"usage":{"truncated":true}}`, 200}, {"disabled", `{"code":"feature_not_enabled","error":"POISON"}`, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/usage":
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.usage)
				case "/v1/transcripts":
					_, _ = io.WriteString(w, `{"transcripts":[]}`)
				case "/v1/email":
					w.WriteHeader(403)
					_, _ = io.WriteString(w, `{"code":"feature_not_enabled","error":"POISON"}`)
				case "/v1/messages":
					w.WriteHeader(403)
					_, _ = io.WriteString(w, `{"error":"feature_not_enabled POISON"}`)
				default:
					http.Error(w, "POISON", http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			s := collectSummary(t.Context(), Config{Endpoint: server.URL, Identity: testIdentity}, summaryTestNow)
			if s.Categories[1].Inventory.Status != "available" || *s.Categories[1].Inventory.Count != 0 || s.Categories[5].Inventory.Status != "disabled" || s.Categories[6].Inventory.Status != "unavailable" || s.Categories[2].Inventory.Status != "unavailable" {
				t.Fatal(s)
			}
			for _, c := range s.Categories {
				if c.Activity.Status != "unavailable" {
					t.Fatal("invalid usage accepted")
				}
			}
			b, _ := json.Marshal(s)
			if strings.Contains(string(b), "POISON") {
				t.Fatal("error leak")
			}
		})
	}
}

func TestSummaryCacheTTLHourRolloverAndCopies(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/v1/transcripts" {
			_, _ = io.WriteString(w, `{"transcripts":[]}`)
		} else {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	c := newSummaryCollector(Config{Endpoint: server.URL, Identity: testIdentity})
	now := time.Date(2026, 9, 22, 16, 59, 45, 0, time.UTC)
	c.now = func() time.Time { return now }
	first, err := c.get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first.Categories[1].Inventory.Status = "poison"
	*first.Categories[1].Inventory.Count = 999
	first.Checkpoints[0].Status = "poison"
	now = now.Add(14 * time.Second)
	second, err := c.get(t.Context())
	if err != nil || calls.Load() != 7 || second.Categories[1].Inventory.Status != "available" || *second.Categories[1].Inventory.Count != 0 || second.Checkpoints[0].Status == "poison" {
		t.Fatal("cache copy", err)
	}
	now = now.Add(time.Second)
	third, err := c.get(t.Context())
	if err != nil || calls.Load() != 14 || !third.GeneratedAt.Equal(now) {
		t.Fatal("hour rollover", err)
	}
	now = now.Add(29 * time.Second)
	_, _ = c.get(t.Context())
	if calls.Load() != 14 {
		t.Fatal("early expiry")
	}
	now = now.Add(time.Second)
	_, _ = c.get(t.Context())
	if calls.Load() != 21 {
		t.Fatal("TTL expiry")
	}
	clone := emptySummary(now)
	projectSummaryUsage(&clone, testIdentity, summaryUsageFixture(now))
	clone.Recent = append(clone.Recent, SummaryUpdate{Key: "transcripts", At: now, Action: "transcript updated"})
	c.mu.Lock()
	c.cached = &clone
	c.mu.Unlock()
	out, _ := c.get(t.Context())
	out.Categories[1].Activity.Bins[0] = 9
	*out.Categories[1].Activity.Total = 9
	out.Recent[0].Action = "poison"
	out, _ = c.get(t.Context())
	if out.Categories[1].Activity.Bins[0] != 0 || *out.Categories[1].Activity.Total != 0 || out.Recent[0].Action == "poison" {
		t.Fatal("shared cached slice or pointer")
	}
}

func TestSummaryRegisterGuardsAndReaderSameProjection(t *testing.T) {
	var calls atomic.Int32
	backend := func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		now := summaryTestNow
		if r.URL.Path == "/v1/usage" {
			now, _ = time.Parse(time.RFC3339, r.URL.Query().Get("until"))
		}
		summaryTestBackend(t, now)(w, r)
	}
	srv, cfg := newDashboard(t, backend, nil)
	cookie := sessionCookie(t, srv, cfg)
	for _, tc := range []struct {
		method, host, site string
		auth               bool
		status             int
	}{{"GET", "", "", false, 401}, {"GET", "evil.invalid", "", true, 403}, {"GET", "", "cross-site", true, 403}, {"POST", "", "", true, 404}} {
		req, _ := http.NewRequest(tc.method, srv.URL+"/api/summary", nil)
		if tc.host != "" {
			req.Host = tc.host
		}
		if tc.site != "" {
			req.Header.Set("Sec-Fetch-Site", tc.site)
		}
		if tc.auth {
			req.AddCookie(cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("guard %v status %d", tc, resp.StatusCode)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("refused route contacted upstream")
	}
	// Exercise the route with unavailable data: both collectors preserve the
	// same shape independently. Compare actual data with the same fake clock
	// through the common handler used by Register.
	reader, err := NewReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reader.summary.now = func() time.Time { return summaryTestNow }
	raw, err := reader.Read(t.Context(), ReadRequest{Resource: ResourceSummary})
	if err != nil {
		t.Fatal(err)
	}
	collector := newSummaryCollector(cfg)
	collector.now = func() time.Time { return summaryTestNow }
	w := httptest.NewRecorder()
	summaryHandler(collector).ServeHTTP(w, httptest.NewRequest("GET", "/api/summary", nil))
	if strings.TrimSpace(w.Body.String()) != strings.TrimSpace(string(raw)) {
		t.Fatal("different projections")
	}
	if calls.Load() != 14 {
		t.Fatal("collectors shared process state")
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/summary", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Fatal("route not guarded")
	}
}

// Channel synchronization controls the fake upstream. Deadlines only bound a
// failing test; correctness does not depend on arbitrary sleep durations.
func TestSummaryCoalescingCancellationAndConcurrency(t *testing.T) {
	var active, peak, calls atomic.Int32
	entered := make(chan struct{}, 20)
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p; p = peak.Load() {
			if peak.CompareAndSwap(p, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
			_, _ = io.WriteString(w, `{}`)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer releaseOnce.Do(func() { close(release) })
	c := newSummaryCollector(Config{Endpoint: server.URL, Identity: testIdentity})
	c.now = func() time.Time { return summaryTestNow }
	ctx, cancel := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() { _, err := c.get(ctx); first <- err }()
	for range 3 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	second := make(chan error, 1)
	go func() { _, err := c.get(t.Context()); second <- err }()
	// Observe waiter registration before canceling the first caller.
	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		waiters := c.flight.waiters
		c.mu.Unlock()
		if waiters == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("second caller not coalesced")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-first:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation blocked")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("survivor blocked")
	}
	if calls.Load() != 7 || peak.Load() > 3 {
		t.Fatalf("calls %d peak %d", calls.Load(), peak.Load())
	}
}

func TestSummaryLastCallerCancellationDrainsAndRecovers(t *testing.T) {
	entered := make(chan struct{}, 10)
	exited := make(chan struct{}, 10)
	var blocking atomic.Bool
	blocking.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blocking.Load() {
			entered <- struct{}{}
			<-r.Context().Done()
			exited <- struct{}{}
		} else {
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer server.Close()
	c := newSummaryCollector(Config{Endpoint: server.URL, Identity: testIdentity})
	c.now = func() time.Time { return summaryTestNow }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := c.get(ctx); done <- err }()
	for range 3 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("not started")
		}
	}
	c.mu.Lock()
	flight := c.flight
	c.mu.Unlock()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("caller stranded")
	}
	for range 3 {
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Fatal("request stranded")
		}
	}
	select {
	case <-flight.done:
	case <-time.After(5 * time.Second):
		t.Fatal("flight stranded")
	}
	c.mu.Lock()
	cached := c.cached
	c.mu.Unlock()
	if cached != nil {
		t.Fatal("canceled result cached")
	}
	blocking.Store(false)
	if _, err := c.get(t.Context()); err != nil {
		t.Fatal("recovery", err)
	}
}

func TestSummaryPartialCacheRecoveryAndSourceIndependence(t *testing.T) {
	var healthy atomic.Bool
	var calls atomic.Int32
	now := summaryTestNow
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/v1/usage":
			// Valid recorded activity survives an independently failing self source.
			until, _ := time.Parse(time.RFC3339, r.URL.Query().Get("until"))
			report := summaryUsageFixture(until)
			for _, d := range summaryDefinitions() {
				if d.dimension == "" {
					continue
				}
				report.Points = append(report.Points, client.UsagePoint{Dimension: d.dimension, Unit: d.unit, BucketStart: report.Since, Quantity: 3, EventCount: 1})
				report.Totals = append(report.Totals, client.UsageTotal{Dimension: d.dimension, Unit: d.unit, Quantity: 3, EventCount: 1})
			}
			writeJSON(w, struct {
				Usage client.UsageReport `json:"usage"`
			}{report})
		case "/v1/self":
			if !healthy.Load() {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, map[string]any{"identity": testIdentity, "index": map[string]any{"counts": map[string]int{"facts": 11, "memories": 12}}, "email_checkpoint": map[string]any{"enabled": false, "pending": true}})
		case "/v1/transcripts":
			writeJSON(w, map[string]any{"transcripts": []any{map[string]any{"updated_at": summaryTestNow.Add(-time.Minute)}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := newSummaryCollector(Config{Endpoint: server.URL, Identity: testIdentity})
	c.now = func() time.Time { return now }
	first, err := c.get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first.Categories[2].Inventory.Status != "unavailable" || first.Categories[3].Inventory.Status != "unavailable" || len(first.Recent) != 1 {
		t.Fatal("partial inventory lost", first)
	}
	for _, i := range []int{1, 2, 4, 5, 6} {
		if first.Categories[i].Activity.Status != "available" || *first.Categories[i].Activity.Total != 3 {
			t.Fatal("independent usage lost", first)
		}
	}
	healthy.Store(true)
	now = now.Add(29 * time.Second)
	cached, err := c.get(t.Context())
	if err != nil || calls.Load() != 7 || cached.Categories[2].Inventory.Status != "unavailable" {
		t.Fatal("partial TTL", err)
	}
	now = now.Add(time.Second)
	recovered, err := c.get(t.Context())
	if err != nil || calls.Load() != 14 || *recovered.Categories[2].Inventory.Count != 11 || *recovered.Categories[3].Inventory.Count != 12 {
		t.Fatal("recovery", err)
	}
	if recovered.Categories[5].Inventory.Status != "disabled" || recovered.Categories[5].Activity.Status != "available" || len(recovered.Recent) != 1 {
		t.Fatal("disabled inbox erased independent section", recovered)
	}
}

func TestSummaryMetadataRedirectAndByteBound(t *testing.T) {
	var followed atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { followed.Add(1) }))
	defer destination.Close()
	for _, kind := range []string{"redirect", "oversize", "trailing json"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "redirect":
					http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
				case "oversize":
					_, _ = io.WriteString(w, `{"transcripts":[],"poison":"`+strings.Repeat("x", summaryResponseLimit)+`"}`)
				case "trailing json":
					_, _ = io.WriteString(w, `{"transcripts":[]} {"transcripts":[]}`)
				}
			}))
			defer server.Close()
			fields, status := summaryGET(t.Context(), Config{Endpoint: server.URL, BearerToken: "synthetic-test-token"}, "/v1/transcripts")
			if fields != nil || status != "unavailable" || followed.Load() != 0 {
				t.Fatal("unsafe metadata accepted or redirect followed")
			}
		})
	}
}
