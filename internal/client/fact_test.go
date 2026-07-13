package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFactRetrievalClients(t *testing.T) {
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/facts" && r.URL.Query().Get("predicate") != "":
			seen["exact"]++
			if r.URL.Query().Get("subject") != "self" || r.URL.Query().Get("predicate") != "identity/name" {
				t.Errorf("exact query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"fact":{"id":"fact_1","subject":"self","predicate":"identity/name","value":"Scott"}}`))
		case r.URL.Path == "/v1/facts":
			seen["search"]++
			if r.URL.Query().Get("predicate_prefix") != "identity/" || r.URL.Query().Get("sort") != "usage" || r.URL.Query().Get("unused") != "true" {
				t.Errorf("search query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"facts":[{"id":"fact_1","subject":"self","predicate":"identity/name","value":"Scott","usage_count":2}]}`))
		case r.URL.Path == "/v1/fact-occurrences":
			seen["temporal"]++
			if r.URL.Query().Get("timezone") != "America/Denver" || r.URL.Query().Get("include_sensitive") != "true" {
				t.Errorf("temporal query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"occurrences":[{"fact":{"id":"fact_2","subject":"self","predicate":"schedule/appointment","value_type":"datetime","value":"2026-07-13T18:00:00Z"},"occurs_at":"2026-07-13T18:00:00Z"}]}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	if fact, err := GetFact(ctx, srv.URL, "agent-token", "self", "identity/name"); err != nil || fact.ID != "fact_1" {
		t.Fatalf("exact fact = %#v / %v", fact, err)
	}
	facts, err := ListFacts(ctx, srv.URL, "agent-token", FactListOptions{
		PredicatePrefix: "identity/", OrderByUsage: true, UnusedOnly: true,
	})
	if err != nil || len(facts) != 1 || facts[0].UsageCount != 2 {
		t.Fatalf("search facts = %#v / %v", facts, err)
	}
	from := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	occurrences, err := UpcomingFactsWithOptions(ctx, srv.URL, "agent-token", FactUpcomingOptions{
		From: from, Until: from.Add(48 * time.Hour), Timezone: "America/Denver", IncludeSensitive: true,
	})
	if err != nil || len(occurrences) != 1 || occurrences[0].Fact.ID != "fact_2" {
		t.Fatalf("temporal facts = %#v / %v", occurrences, err)
	}
	for _, mode := range []string{"exact", "search", "temporal"} {
		if seen[mode] != 1 {
			t.Errorf("%s calls = %d", mode, seen[mode])
		}
	}
}

func TestSetFactAnnualRecurrenceClientContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in SetFactInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		if in.ValueType != "date" || in.Recurrence != "annual" {
			t.Fatalf("set recurrence input = %#v", in)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/fact-candidates" {
			_, _ = w.Write([]byte(`{"candidate":{"id":"fcand_birthday","subject":"self","predicate":"identity/birth-date","value_type":"date","value":"1990-07-13","recurrence":"annual","status":"pending"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"fact":{"id":"fact_birthday","subject":"self","predicate":"identity/birth-date","value_type":"date","value":"1990-07-13","recurrence":"annual"}}`))
	}))
	defer srv.Close()

	fact, err := SetFact(context.Background(), srv.URL, "agent-token", SetFactInput{
		Predicate: "identity/birth-date", ValueType: "date",
		Value: json.RawMessage(`"1990-07-13"`), Recurrence: "annual",
	})
	if err != nil {
		t.Fatal(err)
	}
	if fact.Recurrence != "annual" {
		t.Fatalf("response recurrence = %q", fact.Recurrence)
	}
	candidate, err := ProposeFact(context.Background(), srv.URL, "agent-token", ProposeFactInput{SetFactInput: SetFactInput{
		Predicate: "identity/birth-date", ValueType: "date",
		Value: json.RawMessage(`"1990-07-13"`), Recurrence: "annual",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Recurrence != "annual" {
		t.Fatalf("candidate recurrence = %q", candidate.Recurrence)
	}
}

func TestFactCandidateClientLifecycle(t *testing.T) {
	now := time.Date(2026, 7, 12, 18, 30, 0, 0, time.UTC)
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/fact-candidates":
			seen["propose"]++
			var in ProposeFactInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode proposal: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if in.Subject != "self" || in.Predicate != "preferences/editor" || in.ValueType != "string" || string(in.Value) != `"helix"` {
				t.Errorf("proposal = %#v", in)
			}
			if in.Confidence == nil || *in.Confidence != 0.7 || in.Reason != "possible preference" || in.SourceRef != "transcript_1/entry_7" || !in.ObservedAt.Equal(now.Add(-time.Hour)) || in.ValidFrom == nil || !in.ValidFrom.Equal(now) {
				t.Errorf("proposal evidence = %#v", in)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","candidate":{"id":"fcand_1","subject":"self","predicate":"preferences/editor","value_type":"string","value":"helix","cardinality":"one","confidence":0.7,"observed_at":"2026-07-12T17:30:00Z","valid_from":"2026-07-12T18:30:00Z","reason":"possible preference","status":"conflict","observed_assertion_id":"fas_1","proposed_at":"2026-07-12T18:30:00Z"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/fact-candidates":
			seen["list"]++
			if got := r.URL.Query().Get("status"); got != "conflict" {
				t.Errorf("status query = %q", got)
			}
			if got := r.URL.Query().Get("limit"); got != "25" {
				t.Errorf("limit query = %q", got)
			}
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","candidates":[{"id":"fcand_1","subject":"self","predicate":"preferences/editor","value_type":"string","value":"helix","status":"conflict","proposed_at":"2026-07-12T18:30:00Z"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/fact-candidates/fcand_sensitive":
			seen["detail"]++
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","candidate":{"id":"fcand_sensitive","subject":"self","predicate":"identity/wedding-date","value_type":"date","value":"2010-06-12","recurrence":"annual","sensitive":true,"status":"conflict","observed_assertion_id":"fas_date_1","proposed_at":"2026-07-12T18:30:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/fact-candidates/fcand_1:confirm":
			seen["confirm"]++
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","fact":{"id":"fact_1","subject":"self","predicate":"preferences/editor","value_type":"string","value":"helix"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/fact-candidates/fcand_2:reject":
			seen["reject"]++
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","candidate":{"id":"fcand_2","subject":"self","predicate":"preferences/editor","value_type":"string","value":"vim","status":"rejected","proposed_at":"2026-07-12T18:30:00Z","decided_at":"2026-07-12T18:35:00Z"}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	confidence := 0.7
	proposal, err := ProposeFact(context.Background(), srv.URL+"/", "agent-token", ProposeFactInput{
		SetFactInput: SetFactInput{
			Subject: "self", Predicate: "preferences/editor", ValueType: "string",
			Value: json.RawMessage(`"helix"`), SourceRef: "transcript_1/entry_7",
			ObservedAt: now.Add(-time.Hour), ValidFrom: &now, Confidence: &confidence,
		},
		Reason: "possible preference",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.ID != "fcand_1" || proposal.Status != "conflict" || proposal.ObservedAssertionID != "fas_1" || string(proposal.Value) != `"helix"` || !proposal.ObservedAt.Equal(now.Add(-time.Hour)) || proposal.ValidFrom == nil || !proposal.ValidFrom.Equal(now) {
		t.Fatalf("proposal response = %#v", proposal)
	}

	candidates, err := ListFactCandidatesWithOptions(context.Background(), srv.URL, "agent-token", FactCandidateListOptions{Status: "conflict", Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != "fcand_1" || candidates[0].Status != "conflict" {
		t.Fatalf("candidate list = %#v", candidates)
	}

	detail, err := GetFactCandidate(context.Background(), srv.URL, "agent-token", "fcand_sensitive")
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Sensitive || string(detail.Value) != `"2010-06-12"` || detail.Recurrence != "annual" || detail.ObservedAssertionID != "fas_date_1" {
		t.Fatalf("candidate detail = %#v", detail)
	}

	fact, err := ConfirmFactCandidate(context.Background(), srv.URL, "agent-token", "fcand_1")
	if err != nil {
		t.Fatal(err)
	}
	if fact.ID != "fact_1" || string(fact.Value) != `"helix"` {
		t.Fatalf("confirmed fact = %#v", fact)
	}

	rejected, err := RejectFactCandidate(context.Background(), srv.URL, "agent-token", "fcand_2")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.ID != "fcand_2" || rejected.Status != "rejected" || rejected.DecidedAt == nil {
		t.Fatalf("rejected candidate = %#v", rejected)
	}

	for _, operation := range []string{"propose", "list", "detail", "confirm", "reject"} {
		if seen[operation] != 1 {
			t.Errorf("%s requests = %d, want 1", operation, seen[operation])
		}
	}
}
