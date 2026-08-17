package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/plans"
)

func TestCheckAccountPlanFitViaBridgeForwardsExactSnapshot(t *testing.T) {
	target := planFitClientTarget(t)
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.Method != http.MethodPost ||
			r.URL.Path != "/v1/internal/accounts/acct_1:plan-fit" ||
			r.Header.Get("Authorization") != "Bearer bridge-token" {
			t.Fatalf("fit request=%s %s auth=%q",
				r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var body struct {
			SchemaVersion string                      `json:"schema_version"`
			Target        client.AccountPlanFitTarget `json:"target"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.SchemaVersion != "witself.v0" ||
			body.Target.SnapshotHash != target.SnapshotHash ||
			body.Target.Limits[plans.AgentLimit] != 10 {
			t.Fatalf("fit body=%+v", body)
		}
		_ = json.NewEncoder(w).Encode(client.AccountPlanFitReport{
			SchemaVersion: "witself.v0", AccountID: "acct_1",
			TargetPlan: target.Plan, TargetSnapshotHash: target.SnapshotHash,
			Violations: []client.AccountPlanFitViolation{
				{Code: "limit_exceeded", Dimension: plans.AgentLimit,
					Scope: "account", Used: 12, Max: 10, SubjectCount: 1},
				{Code: "authority_incomplete",
					Dimension: plans.AgentEmailRealmAliasesPerRealmLimit,
					Scope:     "authority", Used: 0, Max: 1, SubjectCount: 1},
			},
		})
	}))
	t.Cleanup(server.Close)

	report, err := client.CheckAccountPlanFitViaBridge(
		context.Background(), server.URL, "bridge-token", "acct_1", target,
	)
	if err != nil || len(report.Violations) != 2 ||
		report.Violations[1].Code != "authority_incomplete" {
		t.Fatalf("fit report=%+v error=%v", report, err)
	}
}

func TestCheckAccountPlanFitRejectsIncompleteTargetBeforeNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	t.Cleanup(server.Close)
	if _, err := client.CheckAccountPlanFitViaBridge(
		context.Background(), server.URL, "bridge-token", "acct_1",
		client.AccountPlanFitTarget{},
	); err == nil {
		t.Fatal("incomplete plan-fit target was accepted")
	}
	if calls != 0 {
		t.Fatalf("network calls=%d want=0", calls)
	}
}

func TestCheckAccountPlanFitRejectsInvalidBridgeReport(t *testing.T) {
	target := planFitClientTarget(t)
	valid := client.AccountPlanFitViolation{
		Code: "limit_exceeded", Dimension: plans.AgentLimit,
		Scope: "account", Used: 12, Max: 10, SubjectCount: 1,
	}
	for _, test := range []struct {
		name       string
		violations []client.AccountPlanFitViolation
	}{
		{name: "duplicate dimension", violations: []client.AccountPlanFitViolation{valid, valid}},
		{name: "wrong maximum", violations: []client.AccountPlanFitViolation{{
			Code: "limit_exceeded", Dimension: plans.AgentLimit,
			Scope: "account", Used: 12, Max: 9, SubjectCount: 1,
		}}},
		{name: "authority on cell-owned dimension", violations: []client.AccountPlanFitViolation{{
			Code: "authority_incomplete", Dimension: plans.AgentLimit,
			Scope: "authority", Used: 0, Max: 10, SubjectCount: 1,
		}}},
		{name: "nonviolating usage", violations: []client.AccountPlanFitViolation{{
			Code: "limit_exceeded", Dimension: plans.AgentLimit,
			Scope: "account", Used: 10, Max: 10, SubjectCount: 1,
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				_ *http.Request,
			) {
				_ = json.NewEncoder(w).Encode(client.AccountPlanFitReport{
					SchemaVersion: "witself.v0", AccountID: "acct_1",
					TargetPlan:         target.Plan,
					TargetSnapshotHash: target.SnapshotHash,
					Violations:         test.violations,
				})
			}))
			t.Cleanup(server.Close)
			if _, err := client.CheckAccountPlanFitViaBridge(
				context.Background(), server.URL, "bridge-token", "acct_1", target,
			); err == nil {
				t.Fatal("invalid plan-fit report was accepted")
			}
		})
	}
}

func planFitClientTarget(t *testing.T) client.AccountPlanFitTarget {
	t.Helper()
	limits := map[string]int64{
		plans.AgentLimit: 10,
		plans.AgentEmailRealmAliasesPerRealmLimit: 1,
	}
	policies := map[string]int64{}
	features := []string{}
	hash, err := plans.SnapshotHash("personal", limits, policies, features)
	if err != nil {
		t.Fatal(err)
	}
	return client.AccountPlanFitTarget{
		Plan: "personal", SnapshotHash: hash, Limits: limits,
		Policies: policies, Features: features,
	}
}
