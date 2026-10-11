package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestApplyBlockedSentinelKeepsMessage(t *testing.T) {
	for _, mode := range []string{"downgrade", "restore recovery"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			catalog, err := plans.Load()
			if err != nil {
				t.Fatal(err)
			}
			store := NewMemStore()
			applier := &conditionalFenceApplyRecorder{
				conditionalApplyRecorder: conditionalApplyRecorder{
					result: ConditionalApplyResult{Violations: []string{
						"agents usage exceeds capacity", "memory usage exceeds capacity",
					}},
				},
			}
			manager, err := NewManager(Config{Catalog: catalog, Store: store, Applier: applier})
			if err != nil {
				t.Fatal(err)
			}
			const accountID = "account"
			record := Record{AccountID: accountID, Entitled: plans.Free, Applied: "team"}
			snapshot, err := manager.resolveSnapshot(record)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "restore recovery" {
				// The plan rank is unchanged and the record acknowledges the
				// desired snapshot. Only the cell's older fence requires a reapply.
				record.Applied = plans.Free
				record.SnapshotRevision = 18
				record.DesiredSnapshotHash = snapshot.Hash
				record.AppliedSnapshotRevision = 18
				record.AppliedSnapshotHash = snapshot.Hash
				applier.fence = ApplyFence{Revision: 15, Hash: snapshot.Hash}
				if SnapshotApplyPending(record, snapshot) {
					t.Fatal("restore fixture unexpectedly has a pending snapshot before the fence check")
				}
			}
			if err := store.Put(ctx, record); err != nil {
				t.Fatal(err)
			}

			err = manager.ReconcileAccount(ctx, accountID)
			if !errors.Is(err, ErrApplyBlocked) {
				t.Fatalf("ReconcileAccount error must wrap ErrApplyBlocked: %v", err)
			}
			const wantMessage = "plan apply blocked: agents usage exceeds capacity; memory usage exceeds capacity"
			if err.Error() != wantMessage {
				t.Fatalf("blocked error message = %q; want %q", err.Error(), wantMessage)
			}
			if applier.calls != 1 || applier.legacyOnlyApplyRecorder.calls != 0 {
				t.Fatalf("conditional calls=%d ordinary calls=%d; want 1 and 0",
					applier.calls, applier.legacyOnlyApplyRecorder.calls)
			}
		})
	}
}
