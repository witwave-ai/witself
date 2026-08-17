package cpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/plans"
)

func TestProductionAppliersExposeAtomicFitAndApply(t *testing.T) {
	target := conditionalApplierTestTarget(t)
	for _, tc := range []struct {
		name string
		path string
		make func(string) lifecycle.Applier
	}{
		{
			name: "direct cell", path: "/v1/accounts/acct_1:plan-fit-apply",
			make: func(url string) lifecycle.Applier {
				return NewCellApplier(StaticCell(url, "fit-secret"))
			},
		},
		{
			name: "directory bridge", path: "/v1/internal/accounts/acct_1:plan-fit-apply",
			make: func(url string) lifecycle.Applier {
				return NewBridgeApplier(url, "fit-secret")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				r *http.Request,
			) {
				if r.Method != http.MethodPost || r.URL.Path != tc.path ||
					r.Header.Get("Authorization") != "Bearer fit-secret" {
					t.Fatalf("request=%s %s auth=%q", r.Method, r.URL.Path,
						r.Header.Get("Authorization"))
				}
				var body struct {
					SchemaVersion string                           `json:"schema_version"`
					Target        client.AccountPlanFitApplyTarget `json:"target"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.SchemaVersion != "witself.v0" || body.Target.Revision != target.Revision ||
					body.Target.SnapshotHash != target.Hash {
					t.Fatalf("request body=%+v", body)
				}
				now := time.Now().UTC()
				_ = json.NewEncoder(w).Encode(client.AccountPlanFitApplyResult{
					SchemaVersion: "witself.v0", State: client.AccountPlanFitApplyStateApplied,
					AccountID: "acct_1", TargetRevision: target.Revision,
					TargetPlan: target.Plan, TargetSnapshotHash: target.Hash,
					Violations: []client.AccountPlanFitViolation{},
					AppliedSnapshot: &client.AccountPlanSnapshot{
						AccountID: "acct_1", Revision: target.Revision,
						SnapshotHash: target.Hash, Plan: target.Plan,
						Limits: target.Limits, Policies: target.Policies,
						Features: target.Features, AppliedAt: &now,
					},
				})
			}))
			t.Cleanup(server.Close)

			applier := tc.make(server.URL)
			conditional, ok := applier.(lifecycle.ConditionalApplier)
			if !ok {
				t.Fatal("production applier does not expose ConditionalApplier")
			}
			result, err := conditional.ApplyIfFits(
				context.Background(), "acct_1", target,
			)
			if err != nil || !result.Applied || result.Ack.Revision != target.Revision ||
				result.Ack.Hash != target.Hash || len(result.Violations) != 0 {
				t.Fatalf("ApplyIfFits result=%+v error=%v", result, err)
			}
		})
	}
}

func TestBridgeAtomicFitBlockFormatsEveryViolation(t *testing.T) {
	target := conditionalApplierTestTarget(t)
	currentLimits := map[string]int64{plans.AgentLimit: 25}
	currentHash, err := plans.SnapshotHash("professional", currentLimits, map[string]int64{}, []string{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.AccountPlanFitApplyResult{
			SchemaVersion: "witself.v0", State: client.AccountPlanFitApplyStateBlocked,
			AccountID: "acct_1", TargetRevision: target.Revision,
			TargetPlan: target.Plan, TargetSnapshotHash: target.Hash,
			Violations: []client.AccountPlanFitViolation{
				{Code: "limit_exceeded", Dimension: plans.AgentLimit,
					Scope: "account", Used: 12, Max: 10, SubjectCount: 1},
				{Code: "limit_exceeded", Dimension: plans.AgentPerRealmLimit,
					Scope: "realm", Used: 7, Max: 5, SubjectCount: 2},
			},
			CurrentSnapshot: &client.AccountPlanSnapshot{
				AccountID: "acct_1", Revision: target.Revision - 1,
				SnapshotHash: currentHash, Plan: "professional",
				Limits: currentLimits, Policies: map[string]int64{},
				Features: []string{}, AppliedAt: &now,
			},
		})
	}))
	t.Cleanup(server.Close)

	applier := NewBridgeApplier(server.URL, "fit-secret")
	result, err := applier.(lifecycle.ConditionalApplier).ApplyIfFits(
		context.Background(), "acct_1", target,
	)
	if err != nil || result.Applied || result.Ack != (lifecycle.ApplyAck{}) ||
		len(result.Violations) != 2 {
		t.Fatalf("ApplyIfFits result=%+v error=%v", result, err)
	}
	joined := strings.Join(result.Violations, "\n")
	for _, want := range []string{
		"agents usage is 12; target maximum is 10",
		"2 realm scopes exceed the agents per realm target maximum of 5; highest usage is 7",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("violations=%q missing %q", joined, want)
		}
	}
}

func conditionalApplierTestTarget(t *testing.T) lifecycle.ApplyRequest {
	t.Helper()
	limits := map[string]int64{
		plans.AgentLimit:         10,
		plans.AgentPerRealmLimit: 5,
	}
	policies := map[string]int64{}
	features := []string{}
	hash, err := plans.SnapshotHash("personal", limits, policies, features)
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle.ApplyRequest{
		Revision: 7,
		PlanSnapshot: lifecycle.PlanSnapshot{
			Plan: "personal", Hash: hash, Limits: limits,
			Policies: policies, Features: features,
		},
	}
}
