package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestCollaborationSnapshotAuthority(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name     string
		applied  *time.Time
		policies map[string]int64
		features []string
		want     bool
	}{
		{"unmanaged", nil, nil, nil, true},
		{"legacy applied with no features", &now, nil, nil, true},
		{"legacy collaboration with messaging enabled", &now, map[string]int64{plans.MessagingEntitlementVersionPolicy: 1}, []string{plans.MessagingFeature}, true},
		{"legacy collaboration cannot bypass messaging denial", &now, map[string]int64{plans.MessagingEntitlementVersionPolicy: 1}, []string{plans.CollaborationFeature}, false},
		{"governed absence denies", &now, map[string]int64{plans.CollaborationEntitlementVersionPolicy: 1}, []string{plans.MessagingFeature}, false},
		{"governed enabled", &now, map[string]int64{plans.CollaborationEntitlementVersionPolicy: 1}, []string{plans.CollaborationFeature}, true},
		{"governed enabled cannot bypass messaging", &now, map[string]int64{plans.CollaborationEntitlementVersionPolicy: 1, plans.MessagingEntitlementVersionPolicy: 1}, []string{plans.CollaborationFeature}, false},
		{"governed both enabled", &now, map[string]int64{plans.CollaborationEntitlementVersionPolicy: 1, plans.MessagingEntitlementVersionPolicy: 1}, []string{plans.CollaborationFeature, plans.MessagingFeature}, true},
		{"zero marker refuses", &now, map[string]int64{plans.CollaborationEntitlementVersionPolicy: 0}, []string{plans.CollaborationFeature}, false},
		{"future marker refuses", &now, map[string]int64{plans.CollaborationEntitlementVersionPolicy: 2}, []string{plans.CollaborationFeature}, false},
		{"marker without applied snapshot refuses", nil, map[string]int64{plans.CollaborationEntitlementVersionPolicy: 1}, []string{plans.CollaborationFeature}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CollaborationEnabledForPlanSnapshot(tc.applied, tc.policies, tc.features); got != tc.want {
				t.Fatalf("effective collaboration=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestCollaborationAuthorityRequiresFenceBeforeStorage(t *testing.T) {
	policies := map[string]int64{plans.CollaborationEntitlementVersionPolicy: 1}
	_, err := (&Store{}).SetAccountPlan(context.Background(), "acc_test", 0, "", "test", nil, policies, nil)
	if !errors.Is(err, ErrPlanSnapshotInvalid) {
		t.Fatalf("unfenced authority error=%v", err)
	}
	now := time.Now().UTC()
	snapshot := AccountPlanSnapshot{AccountID: "acc_test", Plan: "test", Policies: policies, Limits: map[string]int64{}, Features: []string{}, AppliedAt: &now}
	if validAccountPlanSnapshotForFitApply(snapshot) {
		t.Fatal("fit accepted unfenced collaboration authority")
	}
	snapshot.Revision = 1
	snapshot.Hash, err = plans.SnapshotHash(snapshot.Plan, snapshot.Limits, snapshot.Policies, snapshot.Features)
	if err != nil || !validAccountPlanSnapshotForFitApply(snapshot) {
		t.Fatalf("valid governed snapshot refused: %v", err)
	}
}

func TestImportedCollaborationAuthority(t *testing.T) {
	const accountID = "acc_target"
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		want bool
	}{
		{"governed roundtrip", func(map[string]any) {}, true},
		{"marker removed invalidates hash", func(r map[string]any) { r["plan_policies"] = map[string]any{} }, false},
		{"unfenced marker", func(r map[string]any) { r["plan_snapshot_revision"] = float64(0); r["plan_snapshot_hash"] = "" }, false},
		{"missing fence", func(r map[string]any) { delete(r, "plan_snapshot_revision"); delete(r, "plan_snapshot_hash") }, false},
		{"missing applied time", func(r map[string]any) { delete(r, "plan_applied_at") }, false},
		{"malformed applied time", func(r map[string]any) { r["plan_applied_at"] = "yesterday" }, false},
		{"future authority", func(r map[string]any) {
			r["plan_policies"] = map[string]any{plans.CollaborationEntitlementVersionPolicy: float64(2)}
		}, false},
		{"legacy unfenced remains accepted", func(r map[string]any) {
			r["plan_policies"] = map[string]any{}
			r["plan_snapshot_revision"] = float64(0)
			r["plan_snapshot_hash"] = ""
			delete(r, "plan_applied_at")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policies := map[string]int64{plans.CollaborationEntitlementVersionPolicy: 1}
			hash, err := plans.SnapshotHash("test", nil, policies, []string{plans.MessagingFeature})
			if err != nil {
				t.Fatal(err)
			}
			row := map[string]any{"id": accountID, "plan": "test", "plan_limits": map[string]any{}, "plan_policies": map[string]any{plans.CollaborationEntitlementVersionPolicy: float64(1)}, "plan_features": []any{plans.MessagingFeature}, "plan_snapshot_revision": float64(1), "plan_snapshot_hash": hash, "plan_applied_at": "2026-09-01T00:00:00Z"}
			tc.edit(row)
			err = newImportCtx(accountID).validateAndRecord("accounts", row)
			if tc.want && err != nil {
				t.Fatalf("valid import refused: %v", err)
			}
			if !tc.want && !errors.Is(err, ErrArchiveContent) {
				t.Fatalf("invalid authority error=%v", err)
			}
		})
	}
}
