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
