package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testFactCandidateRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

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
			if got := r.Header.Get("Idempotency-Key"); got != "proposal-birthday-1" {
				t.Errorf("proposal Idempotency-Key = %q", got)
			}
			_, _ = w.Write([]byte(`{"candidate":{"id":"fcand_birthday","subject":"self","predicate":"identity/birth-date","value_type":"date","value":"1990-07-13","recurrence":"annual","status":"pending"}}`))
			return
		}
		if got := r.Header.Get("Idempotency-Key"); got != "set-birthday-1" {
			t.Errorf("set Idempotency-Key = %q", got)
		}
		_, _ = w.Write([]byte(`{"fact":{"id":"fact_birthday","subject":"self","predicate":"identity/birth-date","value_type":"date","value":"1990-07-13","recurrence":"annual"}}`))
	}))
	defer srv.Close()

	fact, err := SetFact(context.Background(), srv.URL, "agent-token", SetFactInput{
		Predicate: "identity/birth-date", ValueType: "date",
		Value: json.RawMessage(`"1990-07-13"`), Recurrence: "annual", IdempotencyKey: "set-birthday-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if fact.Recurrence != "annual" {
		t.Fatalf("response recurrence = %q", fact.Recurrence)
	}
	candidate, err := ProposeFact(context.Background(), srv.URL, "agent-token", ProposeFactInput{SetFactInput: SetFactInput{
		Predicate: "identity/birth-date", ValueType: "date",
		Value: json.RawMessage(`"1990-07-13"`), Recurrence: "annual", IdempotencyKey: "proposal-birthday-1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Recurrence != "annual" {
		t.Fatalf("candidate recurrence = %q", candidate.Recurrence)
	}
}

func TestFactDeletionClientContract(t *testing.T) {
	deletedAt := "2026-07-13T20:30:00Z"
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/facts/fact_sensitive" {
			t.Errorf("request = %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		if r.URL.Query().Get("dry_run") == "true" {
			seen["preview"]++
			if got := r.Header.Get("Idempotency-Key"); got != "" {
				t.Errorf("preview Idempotency-Key = %q", got)
			}
			if got := r.URL.Query().Get("expected_resolved_assertion_id"); got != "" {
				t.Errorf("preview expected assertion = %q", got)
			}
			if got := r.URL.Query().Get("expected_candidate_revision"); got != "" {
				t.Errorf("preview expected candidate revision = %q", got)
			}
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","deletion":{"fact_id":"fact_sensitive","subject_id":"fsub_1","subject":"spouse","predicate":"identity/name","sensitive":true,"assertion_count":2,"candidate_count":1,"candidate_revision":"` + testFactCandidateRevision + `","usage_count":7,"resolved_assertion_id":"fas_current","deletion_state":"active","applied":false}}`))
			return
		}
		seen["apply"]++
		if got := r.Header.Get("Idempotency-Key"); got != "delete-fact-1" {
			t.Errorf("apply Idempotency-Key = %q", got)
		}
		if got := r.URL.Query().Get("expected_resolved_assertion_id"); got != "fas_current" {
			t.Errorf("apply expected assertion = %q", got)
		}
		if got := r.URL.Query().Get("expected_candidate_revision"); got != testFactCandidateRevision {
			t.Errorf("apply expected candidate revision = %q", got)
		}
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","deletion":{"fact_id":"fact_sensitive","receipt_id":"fdel_1","subject_id":"fsub_1","subject":"spouse","predicate":"identity/name","sensitive":true,"assertion_count":2,"candidate_count":1,"candidate_revision":"` + testFactCandidateRevision + `","usage_count":7,"resolved_assertion_id":"fas_current","deletion_state":"deleted","deleted_at":"` + deletedAt + `","applied":true}}`))
	}))
	defer srv.Close()

	preview, err := PreviewDeleteFact(context.Background(), srv.URL+"/", "agent-token", "fact_sensitive")
	if err != nil {
		t.Fatal(err)
	}
	if preview.FactID != "fact_sensitive" || preview.Subject != "spouse" || !preview.Sensitive || preview.ResolvedAssertionID != "fas_current" || preview.DeletionState != "active" || preview.Applied {
		t.Fatalf("preview = %#v", preview)
	}
	deleted, err := DeleteFact(context.Background(), srv.URL, "agent-token", DeleteFactInput{
		FactID: "fact_sensitive", ExpectedResolvedAssertionID: preview.ResolvedAssertionID,
		ExpectedCandidateRevision: preview.CandidateRevision, IdempotencyKey: "delete-fact-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !deleted.Applied || deleted.DeletionState != "deleted" || deleted.ReceiptID != "fdel_1" || deleted.DeletedAt == nil || deleted.DeletedAt.Format(time.RFC3339) != deletedAt {
		t.Fatalf("delete = %#v", deleted)
	}
	if seen["preview"] != 1 || seen["apply"] != 1 {
		t.Fatalf("calls = %#v", seen)
	}
}

func TestFactDeletionAddressPreviewIsValueFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/facts" {
			t.Fatalf("request = %s %s", r.Method, r.URL.RequestURI())
		}
		if got := r.URL.Query().Get("dry_run"); got != "true" {
			t.Errorf("dry_run = %q", got)
		}
		if got := r.URL.Query().Get("subject"); got != "person_spouse" {
			t.Errorf("subject = %q", got)
		}
		if got := r.URL.Query().Get("predicate"); got != "identity/name" {
			t.Errorf("predicate = %q", got)
		}
		if r.Header.Get("Idempotency-Key") != "" {
			t.Errorf("preview sent an idempotency key")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deletion":{"fact_id":"fact_spouse","subject_id":"sub_spouse","subject":"person_spouse","predicate":"identity/name","sensitive":true,"assertion_count":1,"candidate_count":0,"candidate_revision":"` + testFactCandidateRevision + `","usage_count":3,"resolved_assertion_id":"fas_current","deletion_state":"active","applied":false}}`))
	}))
	defer srv.Close()

	preview, err := PreviewDeleteFactByAddress(context.Background(), srv.URL, "token", "person_spouse", "identity/name")
	if err != nil {
		t.Fatal(err)
	}
	if preview.FactID != "fact_spouse" || preview.Subject != "person_spouse" || preview.Predicate != "identity/name" || !preview.Sensitive || preview.Applied {
		t.Fatalf("preview = %#v", preview)
	}
}

func TestFactDeletionClientErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/facts/deleted":
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"error":"fact already deleted"}`))
		case "/v1/facts/stale":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"fact changed since deletion preview"}`))
		case "/v1/facts/idem":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"idempotency key was reused for a different fact deletion"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	if _, err := PreviewDeleteFact(context.Background(), srv.URL, "token", "deleted"); err == nil || !strings.Contains(err.Error(), "fact already deleted") {
		t.Fatalf("deleted error = %v", err)
	}
	for _, tc := range []struct {
		factID string
		want   string
	}{
		{factID: "stale", want: "fact changed since deletion preview"},
		{factID: "idem", want: "idempotency key was reused for a different fact deletion"},
	} {
		_, err := DeleteFact(context.Background(), srv.URL, "token", DeleteFactInput{FactID: tc.factID, ExpectedResolvedAssertionID: "fas_1", ExpectedCandidateRevision: testFactCandidateRevision, IdempotencyKey: "delete-1"})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s error = %v", tc.factID, err)
		}
	}
}

func TestSetFactRecreateDeletedClientContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		if in["recreate_deleted"] != true {
			t.Errorf("recreate_deleted = %#v", in["recreate_deleted"])
		}
		if _, exists := in["idempotency_key"]; exists {
			t.Errorf("body leaked idempotency_key: %#v", in)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "recreate-1" {
			t.Errorf("Idempotency-Key = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"fact":{"id":"fact_new","predicate":"preferences/editor","value":"zed"}}`))
	}))
	defer srv.Close()
	if _, err := SetFact(context.Background(), srv.URL, "token", SetFactInput{
		Predicate: "preferences/editor", Value: json.RawMessage(`"zed"`),
		RecreateDeleted: true, IdempotencyKey: "recreate-1",
	}); err != nil {
		t.Fatal(err)
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
			if got := r.Header.Get("Idempotency-Key"); got != "proposal-editor-1" {
				t.Errorf("proposal Idempotency-Key = %q", got)
			}
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
			if got := r.Header.Get("Idempotency-Key"); got != "confirm-editor-1" {
				t.Errorf("confirm Idempotency-Key = %q", got)
			}
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","fact":{"id":"fact_1","subject":"self","predicate":"preferences/editor","value_type":"string","value":"helix"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/fact-candidates/fcand_2:reject":
			seen["reject"]++
			if got := r.Header.Get("Idempotency-Key"); got != "reject-editor-1" {
				t.Errorf("reject Idempotency-Key = %q", got)
			}
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
			IdempotencyKey: "proposal-editor-1",
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

	fact, err := ConfirmFactCandidateWithIdempotency(context.Background(), srv.URL, "agent-token", "fcand_1", "confirm-editor-1")
	if err != nil {
		t.Fatal(err)
	}
	if fact.ID != "fact_1" || string(fact.Value) != `"helix"` {
		t.Fatalf("confirmed fact = %#v", fact)
	}

	rejected, err := RejectFactCandidateWithIdempotency(context.Background(), srv.URL, "agent-token", "fcand_2", "reject-editor-1")
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
