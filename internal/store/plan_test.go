package store

import (
	"context"
	"errors"
	"testing"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestSetAccountPlanRejectsInvalidFeaturesBeforeStorage(t *testing.T) {
	store := &Store{}
	for _, features := range [][]string{
		{"unknown"},
		{plans.MemoryFeature, plans.MemoryFeature},
	} {
		_, err := store.SetAccountPlan(
			context.Background(), "acc_test", 0, "", plans.Free,
			nil, nil, features,
		)
		if !errors.Is(err, ErrPlanSnapshotInvalid) {
			t.Errorf("SetAccountPlan features %v error = %v; want ErrPlanSnapshotInvalid", features, err)
		}
	}
}

func TestApplyAccountPlanIfFitsRejectsInvalidFenceBeforeStorage(t *testing.T) {
	store := &Store{}
	limits := map[string]int64{plans.RealmLimit: 1}
	policies := map[string]int64{}
	features := []string{}
	hash, err := plans.SnapshotHash("personal", limits, policies, features)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []AccountPlanFitApplyTarget{
		{Revision: 0, Plan: "personal", SnapshotHash: hash, Limits: limits, Policies: policies, Features: features},
		{Revision: 1, Plan: "personal", SnapshotHash: "wrong", Limits: limits, Policies: policies, Features: features},
		{Revision: 1, Plan: "personal", SnapshotHash: hash, Limits: nil, Policies: policies, Features: features},
	} {
		if _, err := store.ApplyAccountPlanIfFits(
			context.Background(), "acct_test", target,
		); !errors.Is(err, ErrPlanSnapshotInvalid) {
			t.Errorf("target=%+v error=%v want ErrPlanSnapshotInvalid", target, err)
		}
	}
}
