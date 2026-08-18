package cpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/plans"
)

func TestBridgeFitCheckerPreservesEveryValueFreeViolation(t *testing.T) {
	target := bridgeFitTestSnapshot(t)
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.URL.Path != "/v1/internal/accounts/acct_1:plan-fit" ||
			r.Header.Get("Authorization") != "Bearer bridge-token" {
			t.Fatalf("fit request path=%q auth=%q",
				r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(client.AccountPlanFitReport{
			SchemaVersion: "witself.v0", AccountID: "acct_1",
			TargetPlan: target.Plan, TargetSnapshotHash: target.Hash,
			Violations: []client.AccountPlanFitViolation{
				{Code: "limit_exceeded", Dimension: plans.AgentLimit,
					Scope: "account", Used: 12, Max: 10, SubjectCount: 1},
				{Code: "limit_exceeded", Dimension: plans.AgentPerRealmLimit,
					Scope: "realm", Used: 7, Max: 5, SubjectCount: 2},
				{Code: "limit_exceeded", Dimension: plans.StoredFactLimit,
					Scope: "agent", Used: 101, Max: 100, SubjectCount: 3},
				{Code: "authority_incomplete",
					Dimension: plans.AgentEmailCustomDomainsPerAccountLimit,
					Scope:     "authority", Used: 0, Max: 0, SubjectCount: 1},
			},
		})
	}))
	t.Cleanup(server.Close)

	violations, err := NewBridgeFitChecker(
		server.URL, "bridge-token",
	).Fit(context.Background(), "acct_1", target)
	if err != nil || len(violations) != 4 {
		t.Fatalf("fit violations=%v error=%v", violations, err)
	}
	joined := strings.Join(violations, "\n")
	for _, phrase := range []string{
		"agents usage is 12; target maximum is 10",
		"2 realm scopes exceed the agents per realm target maximum of 5; highest usage is 7",
		"3 agent scopes exceed the stored fact target maximum of 100; highest usage is 101",
		"cannot verify agent email custom domains per account because its control-plane authority is unavailable",
	} {
		if !strings.Contains(joined, phrase) {
			t.Fatalf("fit violations=%q missing %q", joined, phrase)
		}
	}
}

func TestBridgeFitCheckerFailsClosedWithoutExactTargetSnapshot(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	t.Cleanup(server.Close)
	if _, err := NewBridgeFitChecker(
		server.URL, "bridge-token",
	).Fit(context.Background(), "acct_1", lifecycle.PlanSnapshot{}); err == nil {
		t.Fatal("incomplete fit snapshot was accepted")
	}
	if calls != 0 {
		t.Fatalf("bridge calls=%d want=0", calls)
	}
}

func bridgeFitTestSnapshot(t *testing.T) lifecycle.PlanSnapshot {
	t.Helper()
	limits := map[string]int64{
		plans.AgentLimit:                             10,
		plans.AgentPerRealmLimit:                     5,
		plans.StoredFactLimit:                        100,
		plans.AgentEmailCustomDomainsPerAccountLimit: 0,
	}
	policies := map[string]int64{}
	features := []string{}
	hash, err := plans.SnapshotHash("personal", limits, policies, features)
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle.PlanSnapshot{
		Plan: "personal", Hash: hash, Limits: limits,
		Policies: policies, Features: features,
	}
}
