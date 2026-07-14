package server

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

func TestFactRoutes(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token", nil
	}
	now := time.Now().UTC()
	fact := Fact{ID: "fact_1", Subject: "self", Predicate: "preferences/editor", ValueType: "string", Value: json.RawMessage(`"vim"`), CreatedAt: now, UpdatedAt: now}
	listCalls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		SetFact: func(_ context.Context, _ DomainPrincipal, in SetFactRequest) (Fact, error) {
			if in.Predicate != "preferences/editor" || string(in.Value) != `"vim"` {
				t.Fatalf("set input = %#v", in)
			}
			return fact, nil
		},
		GetFact: func(_ context.Context, _ DomainPrincipal, subject, predicate string) (Fact, error) {
			if subject != "self" || predicate != "preferences/editor" {
				t.Fatalf("get = %q / %q", subject, predicate)
			}
			return fact, nil
		},
		ListFacts: func(_ context.Context, _ DomainPrincipal, opts FactListOptions) ([]Fact, error) {
			listCalls++
			if opts.PredicatePrefix != "preferences/" || !opts.OrderByUsage || !opts.UnusedOnly || opts.RetrievalMode != FactRetrievalModeSearch {
				t.Fatalf("list options = %#v", opts)
			}
			return []Fact{fact}, nil
		},
		GetFactHistory: func(_ context.Context, _ DomainPrincipal, factID string) ([]FactAssertion, error) {
			return []FactAssertion{{ID: "fas_1", FactID: factID, Value: json.RawMessage(`"vim"`)}}, nil
		},
	}))
	defer srv.Close()

	resp := factRequest(t, srv.URL, http.MethodPost, "/v1/facts", `{"subject":"self","predicate":"preferences/editor","value":"vim"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("set status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	resp = factRequest(t, srv.URL, http.MethodGet, "/v1/facts?subject=self&predicate=preferences%2Feditor", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	resp = factRequest(t, srv.URL, http.MethodGet, "/v1/facts?predicate_prefix=preferences%2F&sort=usage&unused=true", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if listCalls != 1 {
		t.Fatalf("list calls = %d", listCalls)
	}
	resp = factRequest(t, srv.URL, http.MethodGet, "/v1/facts/fact_1/history", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestFactAnnualRecurrenceRouteContract(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token", nil
	}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		SetFact: func(_ context.Context, _ DomainPrincipal, in SetFactRequest) (Fact, error) {
			if in.ValueType != "date" || in.Recurrence != "annual" {
				t.Fatalf("set recurrence input = %#v", in)
			}
			return Fact{ID: "fact_birthday", Subject: "self", Predicate: in.Predicate, ValueType: in.ValueType, Value: in.Value, Recurrence: in.Recurrence}, nil
		},
		ProposeFact: func(_ context.Context, _ DomainPrincipal, in ProposeFactRequest) (FactCandidate, error) {
			if in.ValueType != "date" || in.Recurrence != "annual" {
				t.Fatalf("proposal recurrence input = %#v", in)
			}
			return FactCandidate{ID: "fcand_birthday", Subject: "self", Predicate: in.Predicate, ValueType: in.ValueType, Value: in.Value, Recurrence: in.Recurrence, Status: "pending"}, nil
		},
	}))
	defer srv.Close()

	resp := factRequest(t, srv.URL, http.MethodPost, "/v1/facts", `{"predicate":"identity/birth-date","value_type":"date","value":"1990-07-13","recurrence":"annual"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("set status = %d", resp.StatusCode)
	}
	var body struct {
		Fact Fact `json:"fact"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if body.Fact.Recurrence != "annual" {
		t.Fatalf("response recurrence = %q", body.Fact.Recurrence)
	}

	resp = factRequest(t, srv.URL, http.MethodPost, "/v1/fact-candidates", `{"predicate":"identity/birth-date","value_type":"date","value":"1990-07-13","recurrence":"annual"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("proposal status = %d", resp.StatusCode)
	}
	var proposal struct {
		Candidate FactCandidate `json:"candidate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&proposal); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if proposal.Candidate.Recurrence != "annual" {
		t.Fatalf("proposal response recurrence = %q", proposal.Candidate.Recurrence)
	}
}

func TestFactCandidateRoutes(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token", nil
	}
	now := time.Date(2026, 7, 12, 18, 30, 0, 0, time.UTC)
	candidate := FactCandidate{
		ID:                  "fcand_1",
		Subject:             "self",
		Predicate:           "preferences/editor",
		ValueType:           "string",
		Value:               json.RawMessage(`"helix"`),
		Cardinality:         "one",
		SourceRef:           "transcript_1/entry_7",
		Confidence:          0.7,
		ObservedAt:          now.Add(-time.Hour),
		ValidFrom:           &now,
		Reason:              "mentioned as a possible preference",
		Status:              "conflict",
		ObservedAssertionID: "fas_1",
		ProposedAt:          now,
	}
	detailCandidate := FactCandidate{
		ID:                  "fcand_sensitive",
		Subject:             "self",
		Predicate:           "identity/wedding-date",
		ValueType:           "date",
		Value:               json.RawMessage(`"2010-06-12"`),
		Recurrence:          "annual",
		Cardinality:         "one",
		Sensitive:           true,
		Status:              "conflict",
		ObservedAssertionID: "fas_date_1",
		ProposedAt:          now,
	}
	confirmed := Fact{
		ID: "fact_1", Subject: "self", Predicate: "preferences/editor",
		ValueType: "string", Value: json.RawMessage(`"helix"`),
		ResolvedAssertionID: "fas_2", CreatedAt: now, UpdatedAt: now,
	}

	listCalls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		ProposeFact: func(_ context.Context, p DomainPrincipal, in ProposeFactRequest) (FactCandidate, error) {
			if p.ID != "agent_1" {
				t.Errorf("proposal principal = %#v", p)
			}
			if in.Subject != "self" || in.Predicate != "preferences/editor" || string(in.Value) != `"helix"` || in.ValueType != "string" {
				t.Errorf("proposal input = %#v", in)
			}
			if in.SourceRef != "transcript_1/entry_7" || in.Confidence == nil || *in.Confidence != 0.7 || in.Reason != "mentioned as a possible preference" || !in.ObservedAt.Equal(now.Add(-time.Hour)) || in.ValidFrom == nil || !in.ValidFrom.Equal(now) {
				t.Errorf("proposal evidence = %#v", in)
			}
			return candidate, nil
		},
		GetFactCandidate: func(_ context.Context, p DomainPrincipal, id string) (FactCandidate, error) {
			if p.ID != "agent_1" || id != "fcand_sensitive" {
				t.Errorf("get principal/id = %#v / %q", p, id)
			}
			return detailCandidate, nil
		},
		ListFactCandidates: func(_ context.Context, p DomainPrincipal, opts FactCandidateListOptions) ([]FactCandidate, error) {
			listCalls++
			if p.ID != "agent_1" || opts.Status != "conflict" || opts.Limit != 25 {
				t.Errorf("list principal/options = %#v / %#v", p, opts)
			}
			return []FactCandidate{candidate}, nil
		},
		ConfirmFactCandidate: func(_ context.Context, p DomainPrincipal, id, _ string) (Fact, error) {
			if id == "fcand_stale" {
				return Fact{}, ErrConflict
			}
			if p.ID != "agent_1" || id != "fcand_1" {
				t.Errorf("confirm principal/id = %#v / %q", p, id)
			}
			return confirmed, nil
		},
		RejectFactCandidate: func(_ context.Context, p DomainPrincipal, id, _ string) (FactCandidate, error) {
			if p.ID != "agent_1" || id != "fcand_2" {
				t.Errorf("reject principal/id = %#v / %q", p, id)
			}
			rejected := candidate
			rejected.ID = id
			rejected.Status = "rejected"
			rejected.DecidedAt = &now
			return rejected, nil
		},
	}))
	defer srv.Close()

	resp := factRequest(t, srv.URL, http.MethodPost, "/v1/fact-candidates", `{"subject":"self","predicate":"preferences/editor","value_type":"string","value":"helix","source_ref":"transcript_1/entry_7","confidence":0.7,"observed_at":"2026-07-12T17:30:00Z","valid_from":"2026-07-12T18:30:00Z","reason":"mentioned as a possible preference"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("propose status = %d", resp.StatusCode)
	}
	var proposal struct {
		Candidate FactCandidate `json:"candidate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&proposal); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if proposal.Candidate.ID != "fcand_1" || proposal.Candidate.Status != "conflict" ||
		proposal.Candidate.ObservedAssertionID != "fas_1" ||
		!proposal.Candidate.ObservedAt.Equal(now.Add(-time.Hour)) || proposal.Candidate.ValidFrom == nil || !proposal.Candidate.ValidFrom.Equal(now) {
		t.Fatalf("proposal response = %#v", proposal.Candidate)
	}

	resp = factRequest(t, srv.URL, http.MethodGet, "/v1/fact-candidates?status=conflict&limit=25", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	var review struct {
		Candidates []FactCandidate `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&review); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(review.Candidates) != 1 || review.Candidates[0].ID != "fcand_1" {
		t.Fatalf("review response = %#v", review.Candidates)
	}

	resp = factRequest(t, srv.URL, http.MethodGet, "/v1/fact-candidates/fcand_sensitive", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("detail Cache-Control = %q", got)
	}
	var detail struct {
		Candidate FactCandidate `json:"candidate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !detail.Candidate.Sensitive || string(detail.Candidate.Value) != `"2010-06-12"` ||
		detail.Candidate.Recurrence != "annual" || detail.Candidate.ObservedAssertionID != "fas_date_1" {
		t.Fatalf("detail response = %#v", detail.Candidate)
	}

	resp = factRequest(t, srv.URL, http.MethodGet, "/v1/fact-candidates?limit=501", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized list status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if listCalls != 1 {
		t.Fatalf("list calls = %d, want 1", listCalls)
	}

	resp = factRequest(t, srv.URL, http.MethodPost, "/v1/fact-candidates/fcand_1:confirm", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm status = %d", resp.StatusCode)
	}
	var confirmation struct {
		Fact Fact `json:"fact"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&confirmation); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if confirmation.Fact.ID != "fact_1" || string(confirmation.Fact.Value) != `"helix"` {
		t.Fatalf("confirmation response = %#v", confirmation.Fact)
	}

	resp = factRequest(t, srv.URL, http.MethodPost, "/v1/fact-candidates/fcand_stale:confirm", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale confirm status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = factRequest(t, srv.URL, http.MethodPost, "/v1/fact-candidates/fcand_2:reject", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reject status = %d", resp.StatusCode)
	}
	var rejection struct {
		Candidate FactCandidate `json:"candidate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rejection); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if rejection.Candidate.ID != "fcand_2" || rejection.Candidate.Status != "rejected" || rejection.Candidate.DecidedAt == nil {
		t.Fatalf("rejection response = %#v", rejection.Candidate)
	}
}

func TestFactCandidateDetailAndDecisionRequireAgent(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindOperator, ID: "operator_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token", nil
	}
	called := false
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetFactCandidate: func(context.Context, DomainPrincipal, string) (FactCandidate, error) {
			called = true
			return FactCandidate{}, nil
		},
		ConfirmFactCandidate: func(context.Context, DomainPrincipal, string, string) (Fact, error) {
			called = true
			return Fact{}, nil
		},
		RejectFactCandidate: func(context.Context, DomainPrincipal, string, string) (FactCandidate, error) {
			called = true
			return FactCandidate{}, nil
		},
		UpcomingFacts: func(context.Context, DomainPrincipal, UpcomingFactOptions) ([]FactOccurrence, error) {
			called = true
			return nil, nil
		},
	}))
	defer srv.Close()

	resp := factRequest(t, srv.URL, http.MethodGet, "/v1/fact-candidates/fcand_sensitive", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("detail status = %d", resp.StatusCode)
	}
	if called {
		t.Fatal("detail callback was called for non-agent principal")
	}
	_ = resp.Body.Close()

	resp = factRequest(t, srv.URL, http.MethodPost, "/v1/fact-candidates/fcand_sensitive:confirm", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("decision status = %d", resp.StatusCode)
	}
	if called {
		t.Fatal("decision callback was called for non-agent principal")
	}
	_ = resp.Body.Close()

	resp = factRequest(t, srv.URL, http.MethodGet, "/v1/fact-occurrences", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("upcoming status = %d", resp.StatusCode)
	}
	if called {
		t.Fatal("upcoming callback was called for non-agent principal")
	}
}

func TestUpcomingFactsIncludeSensitiveQuery(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token", nil
	}
	calls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		UpcomingFacts: func(_ context.Context, _ DomainPrincipal, opts UpcomingFactOptions) ([]FactOccurrence, error) {
			calls++
			if !opts.IncludeSensitive || opts.Timezone != "America/Denver" {
				t.Errorf("upcoming options = %#v", opts)
			}
			return []FactOccurrence{}, nil
		},
	}))
	defer srv.Close()

	resp := factRequest(t, srv.URL, http.MethodGet, "/v1/fact-occurrences?timezone=America%2FDenver&include_sensitive=true", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls != 1 {
		t.Fatalf("upcoming status/calls = %d/%d", resp.StatusCode, calls)
	}
	resp = factRequest(t, srv.URL, http.MethodGet, "/v1/fact-occurrences?include_sensitive=maybe", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest || calls != 1 {
		t.Fatalf("invalid upcoming status/calls = %d/%d", resp.StatusCode, calls)
	}
}

func TestFactMutationIdempotencyHeaders(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token", nil
	}
	seen := map[string]string{}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		SetFact: func(_ context.Context, _ DomainPrincipal, in SetFactRequest) (Fact, error) {
			seen["set"] = in.IdempotencyKey
			if in.IdempotencyKey == "conflict" {
				return Fact{}, ErrIdempotencyConflict
			}
			return Fact{ID: "fact_1", Subject: "self", Predicate: in.Predicate, Value: in.Value}, nil
		},
		ProposeFact: func(_ context.Context, _ DomainPrincipal, in ProposeFactRequest) (FactCandidate, error) {
			seen["propose"] = in.IdempotencyKey
			return FactCandidate{ID: "fcand_1", Subject: "self", Predicate: in.Predicate, Value: in.Value, Status: "pending"}, nil
		},
		ConfirmFactCandidate: func(_ context.Context, _ DomainPrincipal, _ string, key string) (Fact, error) {
			seen["confirm"] = key
			return Fact{ID: "fact_1"}, nil
		},
		RejectFactCandidate: func(_ context.Context, _ DomainPrincipal, _ string, key string) (FactCandidate, error) {
			seen["reject"] = key
			return FactCandidate{ID: "fcand_2", Status: "rejected"}, nil
		},
	}))
	defer srv.Close()

	tests := []struct {
		name string
		path string
		body string
		key  string
		want int
	}{
		{name: "set", path: "/v1/facts", body: `{"predicate":"preferences/editor","value":"zed"}`, key: "set-1", want: http.StatusCreated},
		{name: "propose", path: "/v1/fact-candidates", body: `{"predicate":"preferences/editor","value":"zed"}`, key: "proposal-1", want: http.StatusCreated},
		{name: "confirm", path: "/v1/fact-candidates/fcand_1:confirm", key: "confirm-1", want: http.StatusOK},
		{name: "reject", path: "/v1/fact-candidates/fcand_2:reject", key: "reject-1", want: http.StatusOK},
	}
	for _, tt := range tests {
		resp := factRequestWithIdempotency(t, srv.URL, http.MethodPost, tt.path, tt.body, tt.key)
		_ = resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Fatalf("%s status = %d", tt.name, resp.StatusCode)
		}
	}
	if seen["set"] != "set-1" || seen["propose"] != "proposal-1" || seen["confirm"] != "confirm-1" || seen["reject"] != "reject-1" {
		t.Fatalf("idempotency headers = %#v", seen)
	}

	resp := factRequestWithIdempotency(t, srv.URL, http.MethodPost, "/v1/facts", `{"predicate":"preferences/editor","value":"zed"}`, "conflict")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status = %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "idempotency key was reused for a different fact mutation" {
		t.Fatalf("conflict body = %#v", body)
	}
}

func TestDeleteFactPreviewAndApplyContract(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token", nil
	}
	deletedAt := time.Date(2026, 7, 13, 20, 30, 0, 0, time.UTC)
	var calls []DeleteFactRequest
	mutations := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		DeleteFact: func(_ context.Context, _ DomainPrincipal, in DeleteFactRequest) (FactDeletionReceipt, error) {
			calls = append(calls, in)
			receipt := FactDeletionReceipt{
				FactID: "fact_sensitive", SubjectID: "fsub_1", Subject: "spouse",
				Predicate: "identity/name", Sensitive: true, AssertionCount: 2,
				CandidateCount: 1, CandidateRevision: testFactCandidateRevision, UsageCount: 7, ResolvedAssertionID: "fas_current",
				DeletionState: "active",
			}
			if in.Apply {
				mutations++
				receipt.ReceiptID = "fdel_1"
				receipt.Applied = true
				receipt.DeletionState = "deleted"
				receipt.DeletedAt = &deletedAt
			}
			return receipt, nil
		},
	}))
	defer srv.Close()

	resp := factRequest(t, srv.URL, http.MethodDelete, "/v1/facts/fact_sensitive?dry_run=true", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("preview Cache-Control = %q", got)
	}
	var preview map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(calls) != 1 || calls[0].Apply || calls[0].FactID != "fact_sensitive" || calls[0].IdempotencyKey != "" || calls[0].ExpectedResolvedAssertionID != "" || calls[0].ExpectedCandidateRevision != "" {
		t.Fatalf("preview callback = %#v", calls)
	}
	if mutations != 0 {
		t.Fatalf("preview mutations = %d, want zero", mutations)
	}
	deletion, ok := preview["deletion"].(map[string]any)
	if !ok {
		t.Fatalf("preview envelope = %#v", preview)
	}
	for _, forbidden := range []string{"value", "source", "source_ref", "evidence", "delete_key", "idempotency_key", "request_fingerprint"} {
		if _, exists := deletion[forbidden]; exists {
			t.Fatalf("preview leaked forbidden field %q: %#v", forbidden, deletion)
		}
	}
	if deletion["sensitive"] != true || deletion["resolved_assertion_id"] != "fas_current" || deletion["candidate_revision"] != testFactCandidateRevision || deletion["deletion_state"] != "active" || deletion["applied"] != false {
		t.Fatalf("preview receipt = %#v", deletion)
	}

	resp = factRequestWithIdempotency(t, srv.URL, http.MethodDelete, "/v1/facts/fact_sensitive?expected_resolved_assertion_id=fas_current&expected_candidate_revision="+testFactCandidateRevision, "", "delete-fact-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("apply Cache-Control = %q", got)
	}
	var apply struct {
		Deletion FactDeletionReceipt `json:"deletion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apply); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(calls) != 2 || !calls[1].Apply || calls[1].IdempotencyKey != "delete-fact-1" || calls[1].ExpectedResolvedAssertionID != "fas_current" || calls[1].ExpectedCandidateRevision != testFactCandidateRevision {
		t.Fatalf("apply callback = %#v", calls)
	}
	if mutations != 1 || !apply.Deletion.Applied || apply.Deletion.ReceiptID != "fdel_1" || apply.Deletion.DeletionState != "deleted" || apply.Deletion.DeletedAt == nil || !apply.Deletion.DeletedAt.Equal(deletedAt) {
		t.Fatalf("apply receipt/mutations = %#v / %d", apply.Deletion, mutations)
	}
}

func TestDeleteFactAddressPreviewContract(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token", nil
	}
	mutations := 0
	var got DeleteFactRequest
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		DeleteFact: func(_ context.Context, _ DomainPrincipal, in DeleteFactRequest) (FactDeletionReceipt, error) {
			got = in
			if in.Apply {
				mutations++
			}
			return FactDeletionReceipt{
				FactID: "fact_spouse", SubjectID: "sub_spouse", Subject: "person_spouse",
				Predicate: "identity/name", Sensitive: true, AssertionCount: 1,
				CandidateRevision: testFactCandidateRevision, UsageCount: 3, ResolvedAssertionID: "fas_current", DeletionState: "active",
			}, nil
		},
	}))
	defer srv.Close()

	resp := factRequest(t, srv.URL, http.MethodDelete, "/v1/facts?dry_run=true&subject=person_spouse&predicate=identity%2Fname", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got.FactID != "" || got.Subject != "person_spouse" || got.Predicate != "identity/name" || got.Apply || got.IdempotencyKey != "" || got.ExpectedResolvedAssertionID != "" || got.ExpectedCandidateRevision != "" {
		t.Fatalf("callback = %#v", got)
	}
	if mutations != 0 {
		t.Fatalf("preview mutations = %d", mutations)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	deletion := body["deletion"].(map[string]any)
	for _, forbidden := range []string{"value", "source_ref", "evidence", "idempotency_key"} {
		if _, exists := deletion[forbidden]; exists {
			t.Fatalf("preview leaked %q: %#v", forbidden, deletion)
		}
	}
}

func TestDeleteFactGuardsAndErrors(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		kind := PrincipalKindAgent
		if token == "operator-token" {
			kind = PrincipalKindOperator
		}
		return DomainPrincipal{Kind: kind, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token" || token == "operator-token", nil
	}
	calls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		DeleteFact: func(_ context.Context, _ DomainPrincipal, in DeleteFactRequest) (FactDeletionReceipt, error) {
			calls++
			switch in.FactID {
			case "bad":
				return FactDeletionReceipt{}, ErrBadInput
			case "forbidden":
				return FactDeletionReceipt{}, ErrForbidden
			case "missing":
				return FactDeletionReceipt{}, ErrNotFound
			case "stale":
				return FactDeletionReceipt{}, ErrConflict
			case "deleted":
				return FactDeletionReceipt{}, ErrFactDeleted
			case "idem":
				return FactDeletionReceipt{}, ErrIdempotencyConflict
			default:
				return FactDeletionReceipt{FactID: in.FactID}, nil
			}
		},
	}))
	defer srv.Close()

	for _, path := range []string{
		"/v1/facts/fact_1?dry_run=maybe",
		"/v1/facts/fact_1?expected_resolved_assertion_id=fas_1",
		"/v1/facts/fact_1?dry_run=true&expected_resolved_assertion_id=fas_1",
		"/v1/facts/fact_1?dry_run=true&expected_candidate_revision=" + testFactCandidateRevision,
		"/v1/facts",
		"/v1/facts?dry_run=true&subject=self",
		"/v1/facts?dry_run=true&predicate=identity%2Fname",
		"/v1/facts/fact_1?dry_run=true&subject=self&predicate=identity%2Fname",
	} {
		resp := factRequest(t, srv.URL, http.MethodDelete, path, "")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("DELETE %s status = %d, want 400", path, resp.StatusCode)
		}
	}
	resp := factRequestWithIdempotency(t, srv.URL, http.MethodDelete, "/v1/facts/fact_1?dry_run=true", "", "preview-key")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("preview idempotency status = %d, want 400", resp.StatusCode)
	}
	resp = factRequestWithIdempotency(t, srv.URL, http.MethodDelete, "/v1/facts/fact_1", "", "delete-1")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing expected assertion status = %d, want 400", resp.StatusCode)
	}
	resp = factRequestWithIdempotency(t, srv.URL, http.MethodDelete, "/v1/facts/fact_1?expected_resolved_assertion_id=fas_1", "", "delete-1")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing expected candidate revision status = %d, want 400", resp.StatusCode)
	}
	if calls != 0 {
		t.Fatalf("guarded callback calls = %d, want zero", calls)
	}
	resp, err := http.DefaultClient.Do(mustFactDeleteRequest(t, srv.URL+"/v1/facts/fact_1?dry_run=true", ""))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Cache-Control") != "private, no-store" || calls != 0 {
		t.Fatalf("unauthenticated status/cache/calls = %d/%q/%d", resp.StatusCode, resp.Header.Get("Cache-Control"), calls)
	}

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/v1/facts/fact_1?dry_run=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer operator-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || calls != 0 {
		t.Fatalf("operator status/calls = %d/%d", resp.StatusCode, calls)
	}

	tests := []struct {
		factID    string
		want      int
		wantError string
	}{
		{factID: "bad", want: http.StatusBadRequest, wantError: "bad input"},
		{factID: "forbidden", want: http.StatusForbidden, wantError: "fact access forbidden"},
		{factID: "missing", want: http.StatusNotFound, wantError: "fact not found"},
		{factID: "stale", want: http.StatusConflict, wantError: "fact changed since deletion preview"},
		{factID: "deleted", want: http.StatusGone, wantError: "fact already deleted"},
		{factID: "idem", want: http.StatusConflict, wantError: "idempotency key was reused for a different fact deletion"},
	}
	for _, tt := range tests {
		resp := factRequestWithIdempotency(t, srv.URL, http.MethodDelete, "/v1/facts/"+tt.factID+"?expected_resolved_assertion_id=fas_1&expected_candidate_revision="+testFactCandidateRevision, "", "delete-1")
		if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
			t.Errorf("%s Cache-Control = %q", tt.factID, got)
		}
		if resp.StatusCode != tt.want {
			t.Errorf("%s status = %d, want %d", tt.factID, resp.StatusCode, tt.want)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if body["error"] != tt.wantError {
			t.Errorf("%s error = %#v, want %q", tt.factID, body["error"], tt.wantError)
		}
	}
}

func mustFactDeleteRequest(t *testing.T, requestURL, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, requestURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func TestSetFactRecreateDeletedContract(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token", nil
	}
	seen := false
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		SetFact: func(_ context.Context, _ DomainPrincipal, in SetFactRequest) (Fact, error) {
			seen = in.RecreateDeleted
			return Fact{ID: "fact_new", Predicate: in.Predicate, Value: in.Value}, nil
		},
	}))
	defer srv.Close()
	resp := factRequest(t, srv.URL, http.MethodPost, "/v1/facts", `{"predicate":"preferences/editor","value":"zed","recreate_deleted":true}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || !seen {
		t.Fatalf("set recreate status/seen = %d/%v", resp.StatusCode, seen)
	}
}

func TestFactWritesRequireExplicitRecreationAfterDeletion(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token", nil
	}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		SetFact: func(context.Context, DomainPrincipal, SetFactRequest) (Fact, error) {
			return Fact{}, ErrFactDeleted
		},
		ProposeFact: func(context.Context, DomainPrincipal, ProposeFactRequest) (FactCandidate, error) {
			return FactCandidate{}, ErrFactDeleted
		},
		ConfirmFactCandidate: func(context.Context, DomainPrincipal, string, string) (Fact, error) {
			return Fact{}, ErrFactDeleted
		},
		RejectFactCandidate: func(context.Context, DomainPrincipal, string, string) (FactCandidate, error) {
			return FactCandidate{}, nil
		},
	}))
	defer srv.Close()

	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/v1/facts", want: "fact was deleted; set recreate_deleted=true to create a new fact"},
		{path: "/v1/fact-candidates", want: "fact was deleted; use fact set with recreate_deleted=true to create a new fact"},
		{path: "/v1/fact-candidates/fcand_stale:confirm", want: "fact was deleted; use fact set with recreate_deleted=true to create a new fact"},
	} {
		resp := factRequest(t, srv.URL, http.MethodPost, tc.path, `{"predicate":"preferences/editor","value":"zed"}`)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("POST %s status = %d, want 409", tc.path, resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if body["error"] != tc.want {
			t.Errorf("POST %s body = %#v", tc.path, body)
		}
	}
}

func factRequest(t *testing.T, base, method, path, body string) *http.Response {
	return factRequestWithIdempotency(t, base, method, path, body, "")
}

func factRequestWithIdempotency(t *testing.T, base, method, path, body, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer agent-token")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
