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
		ConfirmFactCandidate: func(_ context.Context, p DomainPrincipal, id string) (Fact, error) {
			if id == "fcand_stale" {
				return Fact{}, ErrConflict
			}
			if p.ID != "agent_1" || id != "fcand_1" {
				t.Errorf("confirm principal/id = %#v / %q", p, id)
			}
			return confirmed, nil
		},
		RejectFactCandidate: func(_ context.Context, p DomainPrincipal, id string) (FactCandidate, error) {
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
		ConfirmFactCandidate: func(context.Context, DomainPrincipal, string) (Fact, error) {
			called = true
			return Fact{}, nil
		},
		RejectFactCandidate: func(context.Context, DomainPrincipal, string) (FactCandidate, error) {
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

func factRequest(t *testing.T, base, method, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer agent-token")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
