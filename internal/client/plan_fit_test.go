package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

func TestApplyAccountPlanIfFitsForwardsExactTargetAndAcceptsApplied(t *testing.T) {
	target := planFitApplyClientTarget(t)
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.Method != http.MethodPost ||
			r.URL.Path != "/v1/accounts/acct_1:plan-fit-apply" ||
			r.Header.Get("Authorization") != "Bearer provision-token" {
			t.Fatalf("fit apply request=%s %s auth=%q",
				r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var body struct {
			SchemaVersion string                           `json:"schema_version"`
			Target        client.AccountPlanFitApplyTarget `json:"target"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.SchemaVersion != "witself.v0" ||
			body.Target.Revision != target.Revision ||
			body.Target.SnapshotHash != target.SnapshotHash ||
			body.Target.Limits[plans.AgentLimit] != 10 {
			t.Fatalf("fit apply body=%+v", body)
		}
		_ = json.NewEncoder(w).Encode(client.AccountPlanFitApplyResult{
			SchemaVersion: "witself.v0",
			State:         client.AccountPlanFitApplyStateApplied,
			AccountID:     "acct_1", TargetRevision: target.Revision,
			TargetPlan: target.Plan, TargetSnapshotHash: target.SnapshotHash,
			Violations: []client.AccountPlanFitViolation{},
			AppliedSnapshot: &client.AccountPlanSnapshot{
				AccountID: "acct_1", Revision: target.Revision,
				SnapshotHash: target.SnapshotHash, Plan: target.Plan,
				Limits: target.Limits, Policies: target.Policies,
				Features: target.Features, AppliedAt: &now,
			},
		})
	}))
	t.Cleanup(server.Close)

	result, err := client.ApplyAccountPlanIfFits(
		context.Background(), server.URL, "provision-token", "acct_1", target,
	)
	if err != nil || result.State != client.AccountPlanFitApplyStateApplied ||
		result.CurrentSnapshot != nil || result.AppliedSnapshot == nil ||
		result.AppliedSnapshot.Revision != target.Revision {
		t.Fatalf("fit apply result=%+v error=%v", result, err)
	}
}

func TestApplyAccountPlanIfFitsViaBridgeAcceptsBlockedWithCurrentSnapshot(t *testing.T) {
	target := planFitApplyClientTarget(t)
	currentLimits := map[string]int64{plans.AgentLimit: 25}
	currentPolicies := map[string]int64{}
	currentFeatures := []string{}
	currentHash, err := plans.SnapshotHash(
		"professional", currentLimits, currentPolicies, currentFeatures,
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.URL.Path != "/v1/internal/accounts/acct_1:plan-fit-apply" ||
			r.Header.Get("Authorization") != "Bearer bridge-token" {
			t.Fatalf("bridge fit apply path/auth=%s/%q",
				r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(client.AccountPlanFitApplyResult{
			SchemaVersion: "witself.v0",
			State:         client.AccountPlanFitApplyStateBlocked,
			AccountID:     "acct_1", TargetRevision: target.Revision,
			TargetPlan: target.Plan, TargetSnapshotHash: target.SnapshotHash,
			Violations: []client.AccountPlanFitViolation{{
				Code: "limit_exceeded", Dimension: plans.AgentLimit,
				Scope: "account", Used: 12, Max: 10, SubjectCount: 1,
			}},
			CurrentSnapshot: &client.AccountPlanSnapshot{
				AccountID: "acct_1", Revision: target.Revision - 1,
				SnapshotHash: currentHash, Plan: "professional",
				Limits: currentLimits, Policies: currentPolicies,
				Features: currentFeatures, AppliedAt: &now,
			},
		})
	}))
	t.Cleanup(server.Close)

	result, err := client.ApplyAccountPlanIfFitsViaBridge(
		context.Background(), server.URL, "bridge-token", "acct_1", target,
	)
	if err != nil || result.State != client.AccountPlanFitApplyStateBlocked ||
		len(result.Violations) != 1 || result.AppliedSnapshot != nil ||
		result.CurrentSnapshot == nil ||
		result.CurrentSnapshot.SnapshotHash != currentHash {
		t.Fatalf("blocked result=%+v error=%v", result, err)
	}
}

func TestApplyAccountPlanIfFitsRejectsInvalidTargetBeforeNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	t.Cleanup(server.Close)
	if _, err := client.ApplyAccountPlanIfFits(
		context.Background(), server.URL, "provision-token", "acct_1",
		client.AccountPlanFitApplyTarget{},
	); err == nil {
		t.Fatal("incomplete plan-fit apply target was accepted")
	}
	if calls != 0 {
		t.Fatalf("network calls=%d want=0", calls)
	}
}

func TestApplyAccountPlanIfFitsRejectsInvalidResultEnvelopes(t *testing.T) {
	target := planFitApplyClientTarget(t)
	now := time.Now().UTC()
	validApplied := client.AccountPlanFitApplyResult{
		SchemaVersion: "witself.v0", State: client.AccountPlanFitApplyStateApplied,
		AccountID: "acct_1", TargetRevision: target.Revision,
		TargetPlan: target.Plan, TargetSnapshotHash: target.SnapshotHash,
		Violations: []client.AccountPlanFitViolation{},
		AppliedSnapshot: &client.AccountPlanSnapshot{
			AccountID: "acct_1", Revision: target.Revision,
			SnapshotHash: target.SnapshotHash, Plan: target.Plan,
			Limits: target.Limits, Policies: target.Policies,
			Features: target.Features, AppliedAt: &now,
		},
	}
	validBlocked := client.AccountPlanFitApplyResult{
		SchemaVersion: "witself.v0", State: client.AccountPlanFitApplyStateBlocked,
		AccountID: "acct_1", TargetRevision: target.Revision,
		TargetPlan: target.Plan, TargetSnapshotHash: target.SnapshotHash,
		Violations: []client.AccountPlanFitViolation{{
			Code: "limit_exceeded", Dimension: plans.AgentLimit,
			Scope: "account", Used: 12, Max: 10, SubjectCount: 1,
		}},
		CurrentSnapshot: &client.AccountPlanSnapshot{
			AccountID: "acct_1", Revision: 0, SnapshotHash: "", Plan: "free",
			Limits: map[string]int64{}, Policies: map[string]int64{}, Features: []string{},
		},
	}
	for _, test := range []struct {
		name   string
		result client.AccountPlanFitApplyResult
	}{
		{name: "applied with current", result: func() client.AccountPlanFitApplyResult {
			result := validApplied
			result.CurrentSnapshot = validBlocked.CurrentSnapshot
			return result
		}()},
		{name: "applied with violation", result: func() client.AccountPlanFitApplyResult {
			result := validApplied
			result.Violations = validBlocked.Violations
			return result
		}()},
		{name: "blocked with no violations", result: func() client.AccountPlanFitApplyResult {
			result := validBlocked
			result.Violations = []client.AccountPlanFitViolation{}
			return result
		}()},
		{name: "blocked with applied", result: func() client.AccountPlanFitApplyResult {
			result := validBlocked
			result.AppliedSnapshot = validApplied.AppliedSnapshot
			return result
		}()},
		{name: "blocked current not older", result: func() client.AccountPlanFitApplyResult {
			result := validBlocked
			current := *result.CurrentSnapshot
			current.Revision = target.Revision
			result.CurrentSnapshot = &current
			return result
		}()},
		{name: "authority violation", result: func() client.AccountPlanFitApplyResult {
			result := validBlocked
			result.Violations = []client.AccountPlanFitViolation{{
				Code:      "authority_incomplete",
				Dimension: plans.AgentEmailRealmAliasesPerRealmLimit,
				Scope:     "authority", Used: 0, Max: 1, SubjectCount: 1,
			}}
			return result
		}()},
		{name: "wrong target revision", result: func() client.AccountPlanFitApplyResult {
			result := validApplied
			result.TargetRevision++
			return result
		}()},
		{name: "unknown state", result: func() client.AccountPlanFitApplyResult {
			result := validApplied
			result.State = "prepared"
			return result
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				_ *http.Request,
			) {
				_ = json.NewEncoder(w).Encode(test.result)
			}))
			t.Cleanup(server.Close)
			if _, err := client.ApplyAccountPlanIfFits(
				context.Background(), server.URL, "provision-token", "acct_1", target,
			); err == nil {
				t.Fatal("invalid plan-fit apply result was accepted")
			}
		})
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

func planFitApplyClientTarget(t *testing.T) client.AccountPlanFitApplyTarget {
	t.Helper()
	fit := planFitClientTarget(t)
	return client.AccountPlanFitApplyTarget{
		Revision: 2, Plan: fit.Plan, SnapshotHash: fit.SnapshotHash,
		Limits: fit.Limits, Policies: fit.Policies, Features: fit.Features,
	}
}
