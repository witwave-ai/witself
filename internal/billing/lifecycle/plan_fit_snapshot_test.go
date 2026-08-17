package lifecycle

import (
	"context"
	"testing"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/plans"
)

type recordingSnapshotFit struct {
	target     PlanSnapshot
	violations []string
}

func (fit *recordingSnapshotFit) Fit(
	_ context.Context,
	_ string,
	target PlanSnapshot,
) ([]string, error) {
	fit.target = target
	return append([]string(nil), fit.violations...), nil
}

type planFitNoopApplier struct{}

func (planFitNoopApplier) Apply(
	_ context.Context,
	_ string,
	request ApplyRequest,
) (ApplyAck, error) {
	return ApplyAck{Revision: request.Revision, Hash: request.Hash}, nil
}

func TestBillingDowngradePreviewChecksExactResolvedTargetAndKeepsAllViolations(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	maximum := int64(7)
	if err := store.Put(context.Background(), Record{
		AccountID: "acct_fit_preview", Provider: "fake",
		CustomerID: "cus_fit_preview", Entitled: "team", Applied: "team",
		LimitOverrides: map[string]AccountLimitOverride{
			plans.AgentLimit: {Max: &maximum},
		},
	}); err != nil {
		t.Fatal(err)
	}
	fit := &recordingSnapshotFit{violations: []string{
		"agents usage exceeds the target",
		"realm usage exceeds the target",
		"custom-domain authority is unavailable",
	}}
	manager, err := NewManager(Config{
		Catalog: catalog,
		Providers: map[string]billing.Provider{
			"fake": fake.New(fake.Config{Prices: catalog.Prices()}),
		},
		Default: "fake", Store: store, Applier: planFitNoopApplier{}, Fit: fit,
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := manager.PreviewBillingMutation(
		context.Background(), "acct_fit_preview", "",
		BillingMutationCommand{
			Operation: BillingMutationPlanDowngrade,
			Plan:      plans.Free,
			Reason:    "verify exact downgrade target",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Allowed || len(preview.Violations) != len(fit.violations) {
		t.Fatalf("preview=%+v", preview)
	}
	for index := range fit.violations {
		if preview.Violations[index] != fit.violations[index] {
			t.Fatalf("preview violations=%v want=%v",
				preview.Violations, fit.violations)
		}
	}
	if fit.target.Plan != plans.Free ||
		fit.target.Limits[plans.AgentLimit] != maximum ||
		fit.target.Limits == nil || fit.target.Policies == nil ||
		fit.target.Features == nil || fit.target.Hash == "" {
		t.Fatalf("resolved fit target=%+v", fit.target)
	}
	wantHash, err := plans.SnapshotHash(
		fit.target.Plan, fit.target.Limits,
		fit.target.Policies, fit.target.Features,
	)
	if err != nil || fit.target.Hash != wantHash {
		t.Fatalf("target hash=%q want=%q error=%v",
			fit.target.Hash, wantHash, err)
	}
}
