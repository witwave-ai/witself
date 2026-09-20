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

type legacyFenceApplyRecorder struct {
	legacyOnlyApplyRecorder
	fence ApplyFence
}

func (a *legacyFenceApplyRecorder) ReadApplyFence(
	context.Context,
	string,
) (ApplyFence, error) {
	return a.fence, nil
}

type conditionalApplyRecorder struct {
	legacyOnlyApplyRecorder
	result ConditionalApplyResult
	calls  int
}

type conditionalFenceApplyRecorder struct {
	conditionalApplyRecorder
	fence   ApplyFence
	request ApplyRequest
}

func (a *conditionalFenceApplyRecorder) ReadApplyFence(
	context.Context,
	string,
) (ApplyFence, error) {
	return a.fence, nil
}

func (a *conditionalFenceApplyRecorder) ApplyIfFits(
	ctx context.Context,
	accountID string,
	request ApplyRequest,
) (ConditionalApplyResult, error) {
	a.request = request
	result, err := a.conditionalApplyRecorder.ApplyIfFits(ctx, accountID, request)
	if err == nil && result.Applied {
		a.fence = ApplyFence(result.Ack)
	}
	return result, err
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

func TestUnknownNewerCellFenceRequiresAtomicFitAndApplyCapability(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	const accountID = "acct_restored_unknown_cell_plan"
	if err := store.Put(context.Background(), Record{
		AccountID: accountID, Entitled: plans.Free, Applied: plans.Free,
	}); err != nil {
		t.Fatal(err)
	}
	applier := &legacyFenceApplyRecorder{fence: ApplyFence{
		Revision: 9,
		Hash:     strings.Repeat("a", 64),
	}}
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
		t.Fatalf("legacy Apply calls = %d; unknown cell plan must fail closed", applier.calls)
	}
}

func TestRestoredOlderCellFenceUsesConditionalApplyAndReplaysDesiredRevision(t *testing.T) {
	for _, restoredPlan := range []string{plans.Free, "team"} {
		t.Run(restoredPlan, func(t *testing.T) {
			ctx := context.Background()
			catalog, err := plans.Load()
			if err != nil {
				t.Fatal(err)
			}
			store := NewMemStore()
			applier := &conditionalFenceApplyRecorder{
				conditionalApplyRecorder: conditionalApplyRecorder{
					result: ConditionalApplyResult{Applied: true},
				},
			}
			manager, err := NewManager(Config{
				Catalog: catalog, Store: store, Applier: applier,
			})
			if err != nil {
				t.Fatal(err)
			}
			target, err := manager.resolveSnapshot(Record{Entitled: plans.Free})
			if err != nil {
				t.Fatal(err)
			}
			restored, err := manager.resolveSnapshot(Record{Entitled: restoredPlan})
			if err != nil {
				t.Fatal(err)
			}
			applier.fence = ApplyFence{Revision: 15, Hash: restored.Hash}
			const accountID = "acct_restored_older_cell"
			const desiredRevision = 18
			if err := store.Put(ctx, Record{
				AccountID: accountID, Entitled: plans.Free, Applied: plans.Free,
				SnapshotRevision: desiredRevision, DesiredSnapshotHash: target.Hash,
				AppliedSnapshotRevision: desiredRevision, AppliedSnapshotHash: target.Hash,
			}); err != nil {
				t.Fatal(err)
			}

			// There is no recorded plan downgrade or pending snapshot. Only the
			// cell rollback requires delivery through the atomic bridge path.
			if err := manager.ReconcileAccount(ctx, accountID); err != nil {
				t.Fatal(err)
			}
			if applier.calls != 1 || applier.legacyOnlyApplyRecorder.calls != 0 {
				t.Fatalf("conditional calls=%d legacy calls=%d",
					applier.calls, applier.legacyOnlyApplyRecorder.calls)
			}
			if applier.request.Revision != desiredRevision || applier.request.Hash != target.Hash ||
				applier.request.Plan != plans.Free {
				t.Fatalf("restored-cell delivery did not replay the exact desired snapshot at revision %d",
					desiredRevision)
			}
			record, ok, err := store.Get(ctx, accountID)
			if err != nil || !ok {
				t.Fatalf("Get record = ok %v, err %v", ok, err)
			}
			if record.SnapshotRevision != desiredRevision ||
				record.AppliedSnapshotRevision != desiredRevision ||
				record.Applied != plans.Free || record.AppliedSnapshotHash != target.Hash ||
				SnapshotApplyPending(record, target) {
				t.Fatal("restored-cell delivery did not preserve the exact desired acknowledgement")
			}
			if err := manager.ReconcileAccount(ctx, accountID); err != nil {
				t.Fatal(err)
			}
			if applier.calls != 1 || applier.legacyOnlyApplyRecorder.calls != 0 {
				t.Fatalf("settled restored cell was redelivered: conditional calls=%d legacy calls=%d",
					applier.calls, applier.legacyOnlyApplyRecorder.calls)
			}
		})
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
