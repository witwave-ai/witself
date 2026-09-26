package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/activity"
)

func TestActivityRouteCatalog(t *testing.T) {
	routes := map[string]string{
		"GET /v1/facts?predicate=x": "facts.exact", "GET /v1/facts": "facts.list", "GET /v1/facts/fact_1/history": "facts.history", "GET /v1/fact-occurrences": "facts.upcoming",
		"POST /v1/facts": "facts.set", "POST /v1/fact-candidates": "facts.propose", "POST /v1/fact-candidates/fcand_1:confirm": "facts.confirm", "POST /v1/fact-candidates/fcand_1:reject": "facts.reject", "DELETE /v1/facts": "facts.delete",
		"GET /v1/memories/mem_1": "memories.get", "GET /v1/memories": "memories.list", "POST /v1/memories:recall": "memories.recall", "GET /v1/memories/mem_1/history": "memories.history",
		"POST /v1/memories": "memories.capture", "PATCH /v1/memories/mem_1": "memories.adjust", "POST /v1/memories/mem_1/supersede": "memories.supersede", "POST /v1/memories/mem_1:forget": "memories.forget", "POST /v1/memories/mem_1:restore": "memories.restore", "POST /v1/memories/mem_1:reactivate": "memories.reactivate", "DELETE /v1/memories/mem_1": "memories.delete", "POST /v1/memory-curation-runs/mrun_1/apply": "memories.curation_apply",
		"GET /v1/transcripts": "transcripts.list", "GET /v1/transcripts/tr_1": "transcripts.page", "POST /v1/transcripts": "transcripts.create", "POST /v1/transcripts/tr_1/entries:batch": "transcripts.append",
		"GET /v1/messages": "messages.list", "POST /v1/messages/msg_1:read": "messages.read", "GET /v1/messages/msg_1:peek": "messages.peek", "POST /v1/messages": "messages.send", "POST /v1/messages/msg_1:reply": "messages.reply",
		"GET /v1/email": "email.list", "POST /v1/email/eml_1:read": "email.read", "GET /v1/email/sent": "email.outbox_list", "GET /v1/email/sent/esnd_1": "email.outbox_detail", "POST /v1/email:send": "email.send", "POST /v1/email/eml_1:reply": "email.reply",
		"GET /v1/secrets": "secrets.inventory", "GET /v1/secrets/sec_1": "secrets.show", "POST /v1/secrets/sec_1/fields/fld_1:access": "secrets.access", "POST /v1/secrets": "secrets.create", "POST /v1/secrets/sec_1:archive": "secrets.archive", "POST /v1/secrets/sec_1:restore": "secrets.restore", "POST /v1/secrets/sec_1:delete": "secrets.delete",
	}
	seen := map[string]bool{}
	for route, want := range routes {
		method, path, _ := strings.Cut(route, " ")
		got := requestActivityOperation(httptest.NewRequest(method, path, nil))
		if got != want {
			t.Errorf("%s = %s, want %s", route, got, want)
		}
		seen[got] = true
	}
	for op := range activity.Catalog() {
		if !seen[op] {
			t.Errorf("catalog route missing: %s", op)
		}
	}
	for _, route := range []string{"GET /v1/self", "GET /v1/activity", "GET /v1/usage", "GET /v1/secrets:status", "GET /v1/memories:status", "GET /v1/fact-subjects", "GET /v1/fact-candidates", "POST /v1/messages:listen", "POST /v1/email:listen", "POST /v1/messages/msg_1:ack", "POST /v1/messages/msg_1:claim", "POST /v1/memory-curation-runs/mrun_1/rollback", "POST /v1/memory-curation-runs/mrun_1/renew", "GET /v1/message-requests", "POST /v1/internal/agent-email:ingest", "HEAD /v1/facts", "GET /v1/facts/fact_1/unknown"} {
		method, path, _ := strings.Cut(route, " ")
		if got := requestActivityOperation(httptest.NewRequest(method, path, nil)); got != "" {
			t.Errorf("excluded %s = %s", route, got)
		}
	}
}
func TestActivityHeaderAndObservationIndependence(t *testing.T) {
	for _, tc := range []struct {
		name, id, observation string
		bad, passive          bool
	}{
		{name: "legacy"}, {name: "deliberate", id: "0123456789abcdef"}, {name: "passive", id: "0123456789abcdef", observation: "1", passive: true},
		{name: "short", id: "secret", bad: true}, {name: "long", id: strings.Repeat("a", 97), bad: true}, {name: "unbounded text", id: "private value with spaces", bad: true}, {name: "bad passive", observation: "true", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			mux := activityContextMux(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				op, _ := activity.Operation(r.Context())
				if op != "facts.exact" || activity.IsObservation(r.Context()) != tc.passive || activity.RequestID(r.Context()) != func() string {
					if activity.ValidRequestID(tc.id) {
						return tc.id
					}
					return ""
				}() {
					t.Fatal("bad context")
				}
				w.WriteHeader(204)
			}))
			req := httptest.NewRequest("GET", "/v1/facts?predicate=private&observational=true", nil)
			if tc.id != "" {
				req.Header.Set(activity.RequestIDHeader, tc.id)
			}
			if tc.observation != "" {
				req.Header.Set(activity.ObservationHeader, tc.observation)
			}
			res := httptest.NewRecorder()
			mux.ServeHTTP(res, req)
			if res.Code != 204 || !called {
				t.Fatal("valid intent rejected")
			}
		})
	}
}
func TestActivityEndpointScopesAndWire(t *testing.T) {
	calls := 0
	mux := apiMux(Config{AuthenticatePrincipal: func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		if token == "agent" {
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agt_1", AccountID: "acc_1", RealmID: "rlm_1", AccountStatus: "active"}, true, nil
		}
		if token == "operator" {
			return DomainPrincipal{Kind: PrincipalKindOperator, ID: "opr_1", AccountID: "acc_1", AccountStatus: "active"}, true, nil
		}
		return DomainPrincipal{}, false, nil
	},
		GetActivity: func(ctx context.Context, p DomainPrincipal, q ActivityQuery) (ActivityReport, error) {
			calls++
			if !activity.IsObservation(ctx) || p.ID != "agt_1" || q.Bucket != "hour" {
				t.Fatal("bad activity scope")
			}
			return ActivityReport{Schema: activity.Schema, Catalog: activity.CatalogVersion, AccountID: p.AccountID, RealmID: p.RealmID, AgentID: p.ID, Since: time.Now().UTC().Truncate(time.Hour), Until: time.Now().UTC(), Bucket: "hour"}, nil
		}})
	for _, tc := range []struct {
		path, token string
		status      int
	}{{"/v1/activity", "agent", 200}, {"/v1/activity", "operator", 403}, {"/v1/activity", "", 401}, {"/v1/activity?group_by=day", "agent", 400}, {"/v1/activity?since=bad", "agent", 400}, {"/v1/activity?account_id=acc_other", "agent", 400}, {"/v1/activity?since=x&since=y", "agent", 400}, {"/v1/activity?since=%zz", "agent", 400}, {"/v1/activity?since=x;y", "agent", 400}} {
		req := httptest.NewRequest("GET", tc.path, nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != tc.status {
			t.Fatalf("%s/%s = %d", tc.path, tc.token, res.Code)
		}
		if tc.status == 200 {
			var out map[string]map[string]json.RawMessage
			if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			for k, want := range map[string]string{"tracking_since": "null", "points": "[]", "totals": "[]", "truncated": "false"} {
				if string(out["activity"][k]) != want {
					t.Fatalf("wire %s = %s", k, out["activity"][k])
				}
			}
		}
	}
	if calls != 1 {
		t.Fatal("unauthorized or invalid query reached store")
	}
}

func TestActivityNormalizedActionRoutes(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/v1/memories/mem_1:%20forget%20", "memories.forget"},
		{"/v1/memories/mem_1:%20restore%20", "memories.restore"},
		{"/v1/memories/mem_1:%20reactivate%20", "memories.reactivate"},
		{"/v1/secrets/sec_aaaaaaaaaaaaaaaa:archive%20", "secrets.archive"},
		{"/v1/secrets/sec_aaaaaaaaaaaaaaaa:restore%20", "secrets.restore"},
		{"/v1/secrets/sec_aaaaaaaaaaaaaaaa:delete%20", "secrets.delete"},
		{"/v1/secrets/sec_aaaaaaaaaaaaaaaa/fields/fld_aaaaaaaaaaaaaaaa:access%20", "secrets.access"},
		{"/v1/messages/msg_1:read%20", ""},
		{"/v1/email/eml_1:read%20", ""},
		{"/v1/fact-candidates/fcand_1:confirm%20", ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequest("POST", tc.path, nil)
			// Verify the existing parser accepts these spellings before asserting the descriptor.
			parts := strings.Split(r.URL.Path, "/")
			if strings.HasPrefix(tc.want, "memories.") {
				if _, _, ok := parseMemoryAction(parts[3]); !ok {
					t.Fatal("handler rejects fixture")
				}
			}
			if strings.HasPrefix(tc.want, "secrets.") {
				r.SetPathValue("action", parts[len(parts)-1])
				w := httptest.NewRecorder()
				if tc.want == "secrets.access" {
					if _, ok := secretFieldAccessPathID(w, r); !ok {
						t.Fatal("handler rejects fixture")
					}
				} else {
					if _, _, ok := secretLifecyclePath(w, r); !ok {
						t.Fatal("handler rejects fixture")
					}
				}
			}
			if got := requestActivityOperation(r); got != tc.want {
				t.Fatalf("descriptor=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestInvalidActivityHeadersDoNotBlockExcludedRoutes(t *testing.T) {
	for _, path := range []string{"/v1/self", "/v1/auth", "/v1/facts"} {
		mux := activityContextMux(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(activity.RequestIDHeader) != "" || r.Header.Get(activity.ObservationHeader) != "" || activity.RequestID(r.Context()) != "" {
				t.Error("invalid headers survived")
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Add(activity.RequestIDHeader, "0123456789abcdef")
		req.Header.Add(activity.RequestIDHeader, "fedcba9876543210")
		req.Header.Add(activity.ObservationHeader, "1")
		req.Header.Add(activity.ObservationHeader, "1")
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != 204 {
			t.Fatal("telemetry blocked route", res.Code)
		}
	}
}
func TestActivityMeteringFailureCounter(t *testing.T) {
	mux := metricsMuxFor(newRuntimeMetrics(), nil, nil, nil, nil, nil, nil, nil, func() uint64 { return 7 })
	out := httptest.NewRecorder()
	mux.ServeHTTP(out, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(out.Body.String(), "# TYPE witself_activity_metering_failures_total counter\nwitself_activity_metering_failures_total 7\n") {
		t.Fatal("counter missing")
	}
}
