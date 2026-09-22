package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/activity"
)

func TestActivityClientRequestIDs(t *testing.T) {
	var keys, passive []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get(activity.RequestIDHeader))
		passive = append(passive, r.Header.Get(activity.ObservationHeader))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	base := context.Background()
	stable := activity.WithRequestID(base, "0123456789abcdef")
	for _, ctx := range []context.Context{base, base, stable, stable, activity.WithObservation(stable), activity.WithDeliberate(activity.WithObservation(stable))} {
		if err := doJSON(ctx, "GET", srv.URL, "", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if keys[0] == keys[1] || !activity.ValidRequestID(keys[0]) || keys[2] != keys[3] || keys[4] != "" || passive[4] != "1" || keys[5] != keys[2] || passive[5] != "" {
		t.Fatal("read ID or intent propagation failed")
	}
	if err := doJSON(activity.WithRequestID(base, "private invalid key"), "GET", srv.URL, "", nil, nil); err == nil || strings.Contains(err.Error(), "private invalid key") {
		t.Fatal("invalid key accepted or echoed")
	}
}
func TestActivityClientTransparentPagesHaveDistinctIDs(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get(activity.RequestIDHeader))
		next := 0
		if r.URL.Query().Get("after_sequence") == "" {
			next = 1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"transcript": map[string]string{"id": "tr_1"}, "entries": []any{}, "next_after_sequence": next})
	}))
	defer srv.Close()
	stable := activity.WithRequestID(context.Background(), "0123456789abcdef")
	for range 2 {
		if _, err := GetTranscript(stable, srv.URL, "", "tr_1"); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 4 || keys[0] == keys[1] || keys[0] != keys[2] || keys[1] != keys[3] {
		t.Fatalf("page IDs must be distinct and stable on aggregate retry: %v", keys)
	}
	for range 2 {
		if _, err := GetTranscript(context.Background(), srv.URL, "", "tr_1"); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, key := range keys[4:] {
		if !activity.ValidRequestID(key) || seen[key] {
			t.Fatal("independent pages reused an ID")
		}
		seen[key] = true
	}
}
func TestActivityClientWire(t *testing.T) {
	start := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	base := ActivityReport{Schema: activity.Schema, Catalog: activity.CatalogVersion, AccountID: "acc_1", RealmID: "rlm_1", AgentID: "agt_1", Since: start, Until: start.Add(time.Hour), Bucket: "hour", Points: []UsagePoint{}, Totals: []UsageTotal{}}
	for _, tc := range []struct {
		name   string
		modify func(*ActivityReport)
		bad    bool
	}{
		{name: "untracked"},
		{name: "tracked partial", modify: func(r *ActivityReport) {
			v := start.Add(10 * time.Minute)
			r.TrackingSince = &v
			r.Points = []UsagePoint{{Dimension: "operation_read", Unit: "operation", BucketStart: start, Quantity: 2, EventCount: 2}}
			r.Totals = []UsageTotal{{Dimension: "operation_read", Unit: "operation", Quantity: 2, EventCount: 2}}
		}},
		{name: "schema", bad: true, modify: func(r *ActivityReport) { r.Schema = "unknown" }},
		{name: "truncated", bad: true, modify: func(r *ActivityReport) { r.Truncated = true }},
		{name: "null arrays", bad: true, modify: func(r *ActivityReport) { r.Points = nil }},
		{name: "unknown tracking with quantities", bad: true, modify: func(r *ActivityReport) {
			r.Totals = []UsageTotal{{Dimension: "operation_read", Unit: "operation", Quantity: 1, EventCount: 1}}
		}},
		{name: "bad unit", bad: true, modify: func(r *ActivityReport) {
			r.TrackingSince = &start
			r.Points = []UsagePoint{{Dimension: "operation_read", Unit: "byte", BucketStart: start, Quantity: 1, EventCount: 1}}
		}},
		{name: "unbounded window", bad: true, modify: func(r *ActivityReport) { r.Until = start.Add(32 * 24 * time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			if tc.modify != nil {
				tc.modify(&r)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get(activity.ObservationHeader) != "1" || req.Header.Get(activity.RequestIDHeader) != "" {
					t.Error("activity endpoint was not passive")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"activity": r})
			}))
			defer srv.Close()
			_, err := GetActivity(context.Background(), srv.URL, "", ActivityQuery{})
			if (err != nil) != tc.bad {
				t.Fatalf("error = %v, bad=%v", err, tc.bad)
			}
		})
	}
	for _, body := range []string{`{}`, `{"activity":{}}`, strings.Repeat(" ", 2<<20) + `{}`, `{"activity":null}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		_, err := GetActivity(context.Background(), srv.URL, "", ActivityQuery{})
		srv.Close()
		if err == nil {
			t.Fatal("malformed response accepted")
		}
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := GetActivity(context.Background(), srv.URL, "", ActivityQuery{}); !errors.Is(err, ErrNotFound) {
		t.Fatal("old server not distinguishable", err)
	}
}
