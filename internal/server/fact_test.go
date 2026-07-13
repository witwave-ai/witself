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
		ListFacts: func(_ context.Context, _ DomainPrincipal, _ FactListOptions) ([]Fact, error) {
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
	resp = factRequest(t, srv.URL, http.MethodGet, "/v1/facts/fact_1/history", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
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
