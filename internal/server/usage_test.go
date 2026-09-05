package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/store"
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

func TestUsageTruncationContract(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		name := "complete"
		if truncated {
			name = "truncated"
		}
		t.Run(name, func(t *testing.T) {
			mux := apiMux(Config{
				AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
					return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agt_1", AccountID: "acc_1", RealmID: "rlm_1", AccountStatus: "active"}, true, nil
				},
				GetUsage: func(context.Context, DomainPrincipal, UsageQuery) (UsageReport, error) {
					return UsageReport{Points: []UsagePoint{}, Totals: []UsageTotal{}, Truncated: truncated}, nil
				},
			})
			req := httptest.NewRequest(http.MethodGet, "/v1/usage?allow_truncation=1", nil)
			req.Header.Set("Authorization", "Bearer agent-token")
			res := httptest.NewRecorder()
			mux.ServeHTTP(res, req)
			if res.Code != http.StatusOK {
				t.Fatalf("usage = %d: %s", res.Code, res.Body.String())
			}
			var body struct {
				Usage struct {
					Truncated *bool `json:"truncated"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Usage.Truncated == nil || *body.Usage.Truncated != truncated {
				t.Fatalf("missing or incorrect typed truncation indicator: %s", res.Body.String())
			}
		})
	}
}

func TestUsageRequiresTruncationOptIn(t *testing.T) {
	const cap = store.UsageReportPointLimit
	for _, tc := range []struct {
		name                   string
		query                  string
		matched                int
		wantStatus             int
		callbackReturnsPartial bool
	}{
		{name: "exact_cap", matched: cap, wantStatus: http.StatusOK},
		{name: "old_client_cap_plus_one", matched: cap + 1, wantStatus: http.StatusUnprocessableEntity},
		{name: "explicit_opt_out", query: "?allow_truncation=0", matched: cap + 1, wantStatus: http.StatusUnprocessableEntity},
		{name: "opt_in", query: "?allow_truncation=1", matched: cap + 1, wantStatus: http.StatusOK},
		{name: "unnegotiated_callback_partial", matched: cap + 1, wantStatus: http.StatusUnprocessableEntity, callbackReturnsPartial: true},
		{name: "invalid_opt_in", query: "?allow_truncation=true", wantStatus: http.StatusBadRequest},
		{name: "ambiguous_opt_in", query: "?allow_truncation=0&allow_truncation=1", wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := apiMux(Config{
				AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
					return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agt_1", AccountID: "acc_1", RealmID: "rlm_1", AccountStatus: "active"}, true, nil
				},
				GetUsage: func(_ context.Context, _ DomainPrincipal, query UsageQuery) (UsageReport, error) {
					if tc.wantStatus == http.StatusBadRequest {
						t.Fatal("invalid opt-in reached usage callback")
					}
					if tc.matched > cap && !query.AllowTruncation && !tc.callbackReturnsPartial {
						return UsageReport{}, fmt.Errorf("wrapped: %w", &UsageQueryTooLargeError{MaxRows: cap})
					}
					return UsageReport{
						Points:    make([]UsagePoint, min(tc.matched, cap)),
						Totals:    []UsageTotal{{Dimension: "transcript_created", Quantity: int64(min(tc.matched, cap))}},
						Truncated: tc.matched > cap,
					}, nil
				},
			})
			req := httptest.NewRequest(http.MethodGet, "/v1/usage"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer agent-token")
			res := httptest.NewRecorder()
			mux.ServeHTTP(res, req)
			if res.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", res.Code, tc.wantStatus, res.Body.String())
			}
			var body struct {
				Code        string       `json:"code"`
				Error       string       `json:"error"`
				MaxRows     int          `json:"max_rows"`
				MatchedRows *int         `json:"matched_rows"`
				Usage       *UsageReport `json:"usage"`
			}
			if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if tc.wantStatus == http.StatusUnprocessableEntity {
				wantMessage := (&UsageQueryTooLargeError{MaxRows: cap}).Error()
				if body.Code != "usage_query_too_large" || body.Error != wantMessage || body.MaxRows != cap || body.MatchedRows != nil || body.Usage != nil {
					t.Fatalf("refusal must include cap/remedies and no partial usage: %s", res.Body.String())
				}
			}
			if tc.wantStatus == http.StatusOK && (body.Usage == nil || len(body.Usage.Points) != min(tc.matched, cap) || body.Usage.Truncated != (tc.matched > cap)) {
				t.Fatal("success did not preserve negotiated truncation")
			}
		})
	}
}
