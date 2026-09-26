package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/activity"
	"github.com/witwave-ai/witself/internal/client"
)

func TestUsageActivityIsPassiveAndPinsIdentity(t *testing.T) {
	for _, scenario := range []string{"untracked", "recorded", "wrong_identity", "old_server", "default_window"} {
		t.Run(scenario, func(t *testing.T) {
			start := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
			identity := client.SelfIdentity{AccountID: "acc_test", RealmID: "rlm_test", AgentID: "agt_test", RealmName: "default", AgentName: "test"}
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer synthetic-activity-token" {
					t.Error("missing synthetic authentication")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/self" {
					_ = json.NewEncoder(w).Encode(client.SelfDigest{Identity: identity})
					return
				}
				calls++
				if r.URL.Path != "/v1/activity" || r.URL.Query().Get("group_by") != "hour" || r.Header.Get(activity.RequestIDHeader) != "" {
					t.Error("activity report used an unexpected route, bucket, or active request ID")
				}
				if scenario == "old_server" {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"error":"not found"}`))
					return
				}
				report := client.ActivityReport{Schema: activity.Schema, Catalog: activity.CatalogVersion, AccountID: identity.AccountID, RealmID: identity.RealmID, AgentID: identity.AgentID, Since: start, Until: start.Add(time.Hour), Bucket: "hour", Points: []client.UsagePoint{}, Totals: []client.UsageTotal{}}
				if scenario == "default_window" {
					if r.URL.Query().Has("since") {
						t.Error("CLI overrode the server's shared 24-bucket default")
					}
					report.Since = report.Until.Truncate(time.Hour).Add(-23 * time.Hour)
				}
				if scenario == "wrong_identity" {
					report.AgentID = "agt_another"
				}
				if scenario == "recorded" {
					report.TrackingSince = &start
					report.Points = []client.UsagePoint{{Dimension: "operation_read", Unit: "operation", Quantity: 2, EventCount: 2, BucketStart: start}}
					report.Totals = []client.UsageTotal{{Dimension: "operation_read", Unit: "operation", Quantity: 2, EventCount: 2}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"activity": report})
			}))
			defer srv.Close()
			token := filepath.Join(t.TempDir(), "synthetic.token")
			if err := os.WriteFile(token, []byte("synthetic-activity-token"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"usage", "--activity", "--endpoint", srv.URL, "--token-file", token, "--until", start.Add(time.Hour).Format(time.RFC3339), "--json"}
			if scenario != "default_window" {
				args = append(args, "--since", start.Format(time.RFC3339))
			}
			stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
			if calls != 1 {
				t.Fatalf("activity calls = %d", calls)
			}
			if scenario == "wrong_identity" || scenario == "old_server" {
				if code != 1 || stdout != "" || stderr == "" {
					t.Fatalf("unsafe activity refusal: %d / %q / %q", code, stdout, stderr)
				}
				return
			}
			var got client.ActivityReport
			if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &got) != nil || got.AgentID != identity.AgentID {
				t.Fatalf("activity output = %d / %q / %q", code, stdout, stderr)
			}
			if (got.TrackingSince == nil) != (scenario == "untracked" || scenario == "default_window") {
				t.Fatal("unknown history was confused with recorded activity")
			}
		})
	}
}

func TestUsageActivityRejectsUnsupportedFiltersBeforeConnecting(t *testing.T) {
	for _, extra := range [][]string{{"--dimension", "message_sent"}, {"--allow-truncation"}, {"--group-by", "day"}} {
		args := append([]string{"usage", "--activity"}, extra...)
		stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
		if code != 2 || stdout != "" || !strings.Contains(stderr, "--activity uses hourly buckets") {
			t.Fatalf("unsupported filter result = %d / %q / %q", code, stdout, stderr)
		}
	}
}

func TestDeliberateRecallAndHistoryTruncationCLI(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	recalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/self":
			_ = json.NewEncoder(w).Encode(client.SelfDigest{Identity: client.SelfIdentity{AccountID: "acc_test", RealmID: "rlm_test", AgentID: "agt_test", RealmName: "default", AgentName: "test"}})
		case "/v1/memories:recall":
			recalls++
			if r.Header.Get(activity.ObservationHeader) != "" || !activity.ValidRequestID(r.Header.Get(activity.RequestIDHeader)) {
				t.Error("deliberate recall lacked activity intent")
			}
			_, _ = w.Write([]byte(`{"hits":[],"retrieval_mode":"lexical"}`))
		case "/v1/facts/fact_test/history":
			_, _ = w.Write([]byte(`{"assertions":[],"truncated":true}`))
		default:
			t.Error("unexpected route")
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	credential := filepath.Join(t.TempDir(), "synthetic-credential")
	if err := os.WriteFile(credential, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	flags := []string{"--endpoint", srv.URL, "--token-file", credential}
	_, stderr, code := captureFactDeleteCLI(t, func() int {
		return run(append([]string{"memory", "recall", "--query", "decision", "--json"}, flags...))
	})
	if code != 0 || recalls != 1 {
		t.Fatal("deliberate recall failed", code, stderr)
	}
	for _, jsonOut := range []bool{false, true} {
		args := append([]string{"fact", "history"}, flags...)
		if jsonOut {
			args = append(args, "--json")
		}
		args = append(args, "fact_test")
		stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
		if code != 0 {
			t.Fatal("history failed", code, stderr)
		}
		if jsonOut {
			var history client.FactHistoryPage
			if json.Unmarshal([]byte(stdout), &history) != nil || !history.Truncated {
				t.Fatal("JSON lost truncation")
			}
		} else if !strings.Contains(stderr, "History truncated") {
			t.Fatal("text lost truncation")
		}
	}
}
