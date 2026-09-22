package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/witwave-ai/witself/internal/activity"
	"github.com/witwave-ai/witself/internal/client"
)

// Opening either console must not turn its inventory refreshes into activity.
// Check the real shared client's outgoing headers, not just context helpers.
func TestConsoleInventoriesDoNotRequestActivity(t *testing.T) {
	cases := []struct {
		resource Resource
		path     string
	}{
		{ResourceSelf, "/api/self"},
		{ResourceTranscripts, "/api/transcripts"},
		{ResourceMemories, "/api/memories"},
		{ResourceFacts, "/api/facts"},
		{ResourceMessages, "/api/messages"},
		{ResourceEmailReceived, "/api/email"},
		{ResourceEmailSent, "/api/email/sent"},
		{ResourceSecrets, "/api/secrets"},
		{ResourceSummary, "/api/summary"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			var calls atomic.Int32
			fixture := readerFixtureBackend(t)
			backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get(activity.RequestIDHeader) != "" {
					t.Error("passive inventory requested an operation count")
				}
				fixture.ServeHTTP(w, r)
			})
			r, _ := newTestReader(t, backend)
			if _, err := r.Read(t.Context(), ReadRequest{Resource: tc.resource}); err != nil {
				t.Fatal(err)
			}
			before := calls.Load()
			if before == 0 {
				t.Fatal("reader never contacted the synthetic backend")
			}
			srv, cfg := newDashboard(t, backend, nil)
			resp := authedGet(t, srv, cfg, tc.path)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || calls.Load() <= before {
				t.Fatalf("web inventory did not complete: status %d", resp.StatusCode)
			}
		})
	}
}

// A deliberate reveal is counted independently of the old observational
// parameter, which must keep suppressing fact ranking and delivery usage.
func TestConsoleExplicitReadsRetainActivityAndObservationSemantics(t *testing.T) {
	for _, web := range []bool{false, true} {
		for _, fact := range []bool{false, true} {
			name := "tui"
			if web {
				name = "web"
			}
			if fact {
				name += "/fact"
			} else {
				name += "/message"
			}
			t.Run(name, func(t *testing.T) {
				var calls atomic.Int32
				backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if !activity.ValidRequestID(r.Header.Get(activity.RequestIDHeader)) || r.Header.Get(activity.ObservationHeader) != "" {
						t.Error("explicit read lost its deliberate activity scope")
					}
					if fact {
						if r.URL.Path != "/v1/facts" || r.URL.Query().Get("observational") != "true" {
							t.Error("fact reveal changed the existing observational contract")
						}
						writeTestJSON(t, w, map[string]any{"fact": client.Fact{ID: "fact_synthetic", Subject: "self", Predicate: "test/value", Value: json.RawMessage(`"synthetic-value"`)}})
					} else {
						if r.URL.Path != "/v1/messages/msg_1:peek" {
							t.Error("preview changed the non-acknowledging peek route")
						}
						writeTestJSON(t, w, map[string]any{"message": map[string]any{"id": "msg_1", "body": "synthetic preview"}})
					}
				})
				if web {
					srv, cfg := newDashboard(t, backend, nil)
					path := "/api/messages/msg_1/body"
					if fact {
						path = "/api/fact?subject=self&predicate=test%2Fvalue"
					}
					resp := authedGet(t, srv, cfg, path)
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("explicit read status %d", resp.StatusCode)
					}
				} else {
					r, _ := newTestReader(t, backend)
					ctx := activity.WithObservation(context.Background())
					var err error
					if fact {
						_, err = r.RevealFact(ctx, "self", "test/value")
					} else {
						_, err = r.PreviewMessage(ctx, "msg_1")
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if calls.Load() != 1 {
					t.Fatalf("explicit read made %d upstream requests", calls.Load())
				}
			})
		}
	}
}
