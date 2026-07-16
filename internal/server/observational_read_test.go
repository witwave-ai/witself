package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestObservationalReadRoutesUseNoWriteCallbacks(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{
			Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1",
			RealmID: "realm_1", AccountStatus: "active",
		}, true, nil
	}
	calls := map[string]int{}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetFact: func(context.Context, DomainPrincipal, string, string) (Fact, error) {
			calls["get"]++
			return Fact{}, nil
		},
		GetFactObservational: func(context.Context, DomainPrincipal, string, string) (Fact, error) {
			calls["get-observational"]++
			return Fact{}, nil
		},
		ListFacts: func(context.Context, DomainPrincipal, FactListOptions) ([]Fact, error) {
			calls["list"]++
			return nil, nil
		},
		ListFactsObservational: func(context.Context, DomainPrincipal, FactListOptions) ([]Fact, error) {
			calls["list-observational"]++
			return nil, nil
		},
		UpcomingFacts: func(context.Context, DomainPrincipal, UpcomingFactOptions) ([]FactOccurrence, error) {
			calls["upcoming"]++
			return nil, nil
		},
		UpcomingFactsObservational: func(context.Context, DomainPrincipal, UpcomingFactOptions) ([]FactOccurrence, error) {
			calls["upcoming-observational"]++
			return nil, nil
		},
		GetSelfFacts: func(context.Context, DomainPrincipal, int, bool) ([]SelfFact, int, error) {
			calls["self"]++
			return nil, 0, nil
		},
		GetSelfFactsObservational: func(context.Context, DomainPrincipal, int, bool) ([]SelfFact, int, error) {
			calls["self-observational"]++
			return nil, 0, nil
		},
		GetTranscriptPage: func(context.Context, DomainPrincipal, string, TranscriptPageOptions) (TranscriptPage, error) {
			calls["transcript"]++
			return TranscriptPage{}, nil
		},
		GetTranscriptPageObservational: func(context.Context, DomainPrincipal, string, TranscriptPageOptions) (TranscriptPage, error) {
			calls["transcript-observational"]++
			return TranscriptPage{}, nil
		},
	}))
	defer srv.Close()

	for _, path := range []string{
		"/v1/facts?subject=self&predicate=identity%2Fname&observational=true",
		"/v1/facts?observational=true",
		"/v1/fact-occurrences?observational=true",
		"/v1/self?include_facts=true&observational=true",
		"/v1/transcripts/trn_1?limit=10&observational=true",
	} {
		resp := observationalReadRequest(t, srv.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	for _, name := range []string{
		"get-observational", "list-observational", "upcoming-observational",
		"self-observational", "transcript-observational",
	} {
		if calls[name] != 1 {
			t.Errorf("%s calls = %d, want 1", name, calls[name])
		}
	}
	for _, name := range []string{"get", "list", "upcoming", "self", "transcript"} {
		if calls[name] != 0 {
			t.Errorf("metered %s calls = %d, want 0", name, calls[name])
		}
	}
}

func TestObservationalReadQueryIsStrict(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, AccountStatus: "active"}, true, nil
	}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetFact: func(context.Context, DomainPrincipal, string, string) (Fact, error) {
			return Fact{}, nil
		},
		ListFacts: func(context.Context, DomainPrincipal, FactListOptions) ([]Fact, error) {
			return nil, nil
		},
	}))
	defer srv.Close()

	resp := observationalReadRequest(t, srv.URL+"/v1/facts?observational=maybe")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid observational query = %d, want 400", resp.StatusCode)
	}
}

func observationalReadRequest(t *testing.T, requestURL string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer agent-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
