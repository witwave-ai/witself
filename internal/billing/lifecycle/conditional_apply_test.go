package lifecycle

import (
	"context"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/plans"
)

type legacyOnlyApplyRecorder struct {
	calls int
}

func (a *legacyOnlyApplyRecorder) Apply(
	_ context.Context,
	_ string,
	request ApplyRequest,
) (ApplyAck, error) {
	a.calls++
	return ApplyAck{Revision: request.Revision, Hash: request.Hash}, nil
}

type conditionalApplyRecorder struct {
	legacyOnlyApplyRecorder
	result ConditionalApplyResult
	calls  int
}

func (a *conditionalApplyRecorder) ApplyIfFits(
	_ context.Context,
	_ string,
	request ApplyRequest,
) (ConditionalApplyResult, error) {
	a.calls++
	result := a.result
	if result.Applied && result.Ack == (ApplyAck{}) {
		result.Ack = ApplyAck{Revision: request.Revision, Hash: request.Hash}
	}
	return result, nil
}

func TestDowngradeApplyRequiresAtomicFitAndApplyCapability(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	const accountID = "acct_atomic_fit_required"
	if err := store.Put(context.Background(), Record{
		AccountID: accountID, Entitled: plans.Free, Applied: "team",
	}); err != nil {
		t.Fatal(err)
	}
	applier := &legacyOnlyApplyRecorder{}
	manager, err := NewManager(Config{
		Catalog: catalog, Store: store, Applier: applier,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = manager.ReconcileAccount(context.Background(), accountID)
	if err == nil || !strings.Contains(err.Error(), "atomic downgrade fit-and-apply is unavailable") {
		t.Fatalf("ReconcileAccount error = %v", err)
	}
	if applier.calls != 0 {
		t.Fatalf("legacy Apply calls = %d; downgrade must fail before a racy write", applier.calls)
	}
}

func TestDowngradeAtomicFitBlockPersistsWithoutApplying(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	const accountID = "acct_atomic_fit_blocked"
	if err := store.Put(context.Background(), Record{
		AccountID: accountID, Entitled: plans.Free, Applied: "team",
	}); err != nil {
		t.Fatal(err)
	}
	applier := &conditionalApplyRecorder{result: ConditionalApplyResult{
		Violations: []string{"agents usage is 12; target maximum is 10"},
	}}
	manager, err := NewManager(Config{
		Catalog: catalog, Store: store, Applier: applier,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = manager.ReconcileAccount(context.Background(), accountID)
	if err == nil || !strings.Contains(err.Error(), "agents usage is 12") {
		t.Fatalf("ReconcileAccount error = %v", err)
	}
	if applier.calls != 1 || applier.legacyOnlyApplyRecorder.calls != 0 {
		t.Fatalf("conditional calls=%d legacy calls=%d",
			applier.calls, applier.legacyOnlyApplyRecorder.calls)
	}
	record, ok, err := store.Get(context.Background(), accountID)
	if err != nil || !ok {
		t.Fatalf("Get record = ok %v, err %v", ok, err)
	}
	if record.Applied != "team" || !strings.Contains(record.ApplyBlocked, "agents usage is 12") {
		t.Fatalf("blocked record = %+v", record)
	}
}

func TestDowngradeAtomicFitApplyRecordsExactAcknowledgement(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	const accountID = "acct_atomic_fit_applied"
	if err := store.Put(context.Background(), Record{
		AccountID: accountID, Entitled: plans.Free, Applied: "team",
	}); err != nil {
		t.Fatal(err)
	}
	applier := &conditionalApplyRecorder{result: ConditionalApplyResult{Applied: true}}
	manager, err := NewManager(Config{
		Catalog: catalog, Store: store, Applier: applier,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.ReconcileAccount(context.Background(), accountID); err != nil {
		t.Fatal(err)
	}
	record, snapshot, err := manager.ResolvedStatus(context.Background(), accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	if record.Applied != plans.Free || record.AppliedSnapshotRevision == 0 ||
		record.AppliedSnapshotHash != snapshot.Hash || record.ApplyBlocked != "" {
		t.Fatalf("applied record = %+v snapshot = %+v", record, snapshot)
	}
}
