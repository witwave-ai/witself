package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFactSubjectRoutes(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		kind := PrincipalKindAgent
		if token == "operator-token" {
			kind = PrincipalKindOperator
		}
		return DomainPrincipal{Kind: kind, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token" || token == "operator-token", nil
	}
	now := time.Date(2026, 7, 12, 19, 0, 0, 0, time.UTC)
	subject := FactSubject{ID: "sub_1", CanonicalKey: "spouse", DisplayName: "My spouse", Aliases: []string{}, CreatedAt: now, UpdatedAt: now}

	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		UpsertFactSubject: func(_ context.Context, p DomainPrincipal, canonicalKey string, in UpsertFactSubjectRequest) (FactSubject, error) {
			if p.ID != "agent_1" || canonicalKey != "spouse" || in.DisplayName != "My spouse" {
				t.Errorf("upsert principal/key/input = %#v / %q / %#v", p, canonicalKey, in)
			}
			return subject, nil
		},
		AddFactSubjectAlias: func(_ context.Context, p DomainPrincipal, canonicalKey, alias string) (FactSubject, error) {
			if p.ID != "agent_1" || canonicalKey != "spouse" || alias != "my wife" {
				t.Errorf("alias principal/key/alias = %#v / %q / %q", p, canonicalKey, alias)
			}
			out := subject
			out.Aliases = []string{"my wife"}
			return out, nil
		},
		ListFactSubjects: func(_ context.Context, p DomainPrincipal) ([]FactSubject, error) {
			if p.ID != "agent_1" {
				t.Errorf("list principal = %#v", p)
			}
			return []FactSubject{subject}, nil
		},
	}))
	defer srv.Close()

	resp := factRequest(t, srv.URL, http.MethodPut, "/v1/fact-subjects/spouse", `{"display_name":"My spouse"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upsert status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("upsert Cache-Control = %q", got)
	}
	var upserted struct {
		Subject FactSubject `json:"subject"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&upserted); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if upserted.Subject.CanonicalKey != "spouse" || upserted.Subject.DisplayName != "My spouse" {
		t.Fatalf("upsert response = %#v", upserted.Subject)
	}

	resp = factRequest(t, srv.URL, http.MethodPost, "/v1/fact-subjects/spouse/aliases", `{"alias":"my wife"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("alias status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("alias Cache-Control = %q", got)
	}
	var aliased struct {
		Subject FactSubject `json:"subject"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&aliased); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(aliased.Subject.Aliases) != 1 || aliased.Subject.Aliases[0] != "my wife" {
		t.Fatalf("alias response = %#v", aliased.Subject)
	}

	resp = factRequest(t, srv.URL, http.MethodGet, "/v1/fact-subjects", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("list Cache-Control = %q", got)
	}
	var listed struct {
		Subjects []FactSubject `json:"subjects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(listed.Subjects) != 1 || listed.Subjects[0].ID != "sub_1" {
		t.Fatalf("list response = %#v", listed.Subjects)
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/fact-subjects", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer operator-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator list status = %d", resp.StatusCode)
	}
}

func TestFactSubjectRouteValidationAndErrors(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, token == "agent-token", nil
	}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		UpsertFactSubject: func(_ context.Context, _ DomainPrincipal, canonicalKey string, _ UpsertFactSubjectRequest) (FactSubject, error) {
			return FactSubject{}, errors.Join(ErrBadInput, errors.New("invalid canonical key "+canonicalKey))
		},
		AddFactSubjectAlias: func(_ context.Context, _ DomainPrincipal, _, _ string) (FactSubject, error) {
			return FactSubject{}, ErrNotFound
		},
	}))
	defer srv.Close()

	resp := factRequest(t, srv.URL, http.MethodPut, "/v1/fact-subjects/not%20valid", `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid key status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = factRequest(t, srv.URL, http.MethodPost, "/v1/fact-subjects/missing/aliases", `{"alias":"someone"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing subject status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = factRequest(t, srv.URL, http.MethodPost, "/v1/fact-subjects/spouse/aliases", `{"alias":""}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty alias status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = factRequest(t, srv.URL, http.MethodPut, "/v1/fact-subjects/spouse", `{`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}
