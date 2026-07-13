package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUsageIsAgentScopedAndParsesFilters(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		switch token {
		case "agent-token":
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agt_1", AccountID: "acc_1", RealmID: "rlm_1", AccountStatus: "active"}, true, nil
		case "operator-token":
			return DomainPrincipal{Kind: PrincipalKindOperator, ID: "opr_1", AccountID: "acc_1", AccountStatus: "active"}, true, nil
		default:
			return DomainPrincipal{}, false, nil
		}
	}
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	calls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetUsage: func(_ context.Context, p DomainPrincipal, query UsageQuery) (UsageReport, error) {
			calls++
			if p.ID != "agt_1" || !query.Since.Equal(since) || !query.Until.Equal(until) || query.Bucket != "day" {
				t.Fatalf("principal/query = %#v / %#v", p, query)
			}
			if len(query.Dimensions) != 2 || query.Dimensions[0] != "transcript_created" || query.Dimensions[1] != "transcript_entry_write" {
				t.Fatalf("dimensions = %#v", query.Dimensions)
			}
			return UsageReport{
				AccountID: p.AccountID, RealmID: p.RealmID, AgentID: p.ID,
				Since: since, Until: until, Bucket: "day", Points: []UsagePoint{}, Totals: []UsageTotal{},
			}, nil
		},
	}))
	defer srv.Close()

	path := "/v1/usage?since=2026-07-01T00:00:00Z&until=2026-07-12T00:00:00Z&group_by=day&dimension=transcript_created&dimension=transcript_entry_write"
	resp := transcriptRequest(t, srv.URL, http.MethodGet, path, "agent-token", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent usage = %d", resp.StatusCode)
	}
	var body struct {
		Usage UsageReport `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if body.Usage.AgentID != "agt_1" {
		t.Fatalf("usage = %#v", body.Usage)
	}

	resp = transcriptRequest(t, srv.URL, http.MethodGet, "/v1/usage", "operator-token", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator usage = %d, want 403", resp.StatusCode)
	}
	closeBody(t, resp)
	if calls != 1 {
		t.Fatalf("usage hook calls = %d, operator reached hook", calls)
	}

	resp = transcriptRequest(t, srv.URL, http.MethodGet, "/v1/usage?since=yesterday", "agent-token", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad since = %d, want 400", resp.StatusCode)
	}
	closeBody(t, resp)
}
