package syncmodel

import "testing"

func fixtureIncarnationHistory() IncarnationHistory {
	return IncarnationHistory{Current: Incarnation{AccountID: "synthetic-account", Lifecycle: 7, Restore: RestoreMarker{1}}, LastAllocated: 7}
}

func TestIncarnationMoveRestoreAndConsumedAllocations(t *testing.T) {
	original := fixtureIncarnationHistory()
	moved, move, err := AllocateIncarnation(original, IncarnationRequest{AllocationID: "move-aborted", Kind: ProvenMove, MoveProven: true})
	if err != nil || move.Successor.Lifecycle != 8 || move.Successor.Restore != original.Current.Restore {
		t.Fatal("proven move did not advance epoch and preserve marker")
	}
	if original.LastAllocated != 7 || len(original.Allocations) != 0 || moved.Current != original.Current {
		t.Fatal("allocation mutated input or conferred active authority")
	}
	// Abandon move-aborted without publishing it. The following restore must
	// consume the next epoch even though Current still names the old authority.
	restored, restore, err := AllocateIncarnation(moved, IncarnationRequest{AllocationID: "restore-newest-backup", Kind: BackupRestore, Restore: RestoreMarker{2}})
	if err != nil || restore.Successor.Lifecycle != 9 || restore.Successor.Restore == original.Current.Restore {
		t.Fatal("aborted allocation was reused or newest-backup restore retained marker")
	}
	retried, same, err := AllocateIncarnation(restored, IncarnationRequest{AllocationID: "restore-newest-backup", Kind: BackupRestore, Restore: RestoreMarker{2}})
	if err != nil || same != restore || len(retried.Allocations) != 2 || retried.LastAllocated != 9 {
		t.Fatal("exact durable restore retry allocated another identity")
	}
	again, next, err := AllocateIncarnation(restored, IncarnationRequest{AllocationID: "restore-separate", Kind: BackupRestore, Restore: RestoreMarker{3}})
	if err != nil || next.Successor.Lifecycle != 10 || next.Successor.Restore == restore.Successor.Restore || len(again.Allocations) != 3 {
		t.Fatal("separate restore did not allocate a distinct marker and epoch")
	}
}

func TestIncarnationAllocationRefusesInvalidTransitions(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*IncarnationHistory, *IncarnationRequest)
	}{
		{"unproved_move", func(_ *IncarnationHistory, r *IncarnationRequest) { r.MoveProven = false }},
		{"unknown_transition", func(_ *IncarnationHistory, r *IncarnationRequest) { r.Kind = "copy" }},
		{"empty_allocation", func(_ *IncarnationHistory, r *IncarnationRequest) { r.AllocationID = "" }},
		{"malformed_account", func(h *IncarnationHistory, _ *IncarnationRequest) { h.Current.AccountID = "account\x00" }},
		{"epoch_overflow", func(h *IncarnationHistory, _ *IncarnationRequest) {
			h.LastAllocated = LifecycleEpoch(MaxLifecycleEpoch)
		}},
		{"allocation_rollback", func(h *IncarnationHistory, _ *IncarnationRequest) { h.LastAllocated = 6 }},
		{"restore_preserves_marker", func(h *IncarnationHistory, r *IncarnationRequest) {
			r.Kind = BackupRestore
			r.Restore = h.Current.Restore
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := fixtureIncarnationHistory()
			r := IncarnationRequest{AllocationID: "allocation", Kind: ProvenMove, MoveProven: true}
			test.mutate(&h, &r)
			if _, _, err := AllocateIncarnation(h, r); err == nil {
				t.Fatal("invalid allocation accepted")
			}
		})
	}
	h, _, err := AllocateIncarnation(fixtureIncarnationHistory(), IncarnationRequest{AllocationID: "restore", Kind: BackupRestore, Restore: RestoreMarker{2}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []IncarnationRequest{
		{AllocationID: "restore", Kind: BackupRestore, Restore: RestoreMarker{3}},
		{AllocationID: "restore", Kind: ProvenMove, MoveProven: true},
		{AllocationID: "another-restore", Kind: BackupRestore, Restore: RestoreMarker{2}},
	} {
		if _, _, err := AllocateIncarnation(h, r); err == nil {
			t.Fatal("altered retry or reused historical restore marker accepted")
		}
	}
	h.Allocations = append(h.Allocations, h.Allocations[0])
	if _, _, err := AllocateIncarnation(h, IncarnationRequest{AllocationID: "next", Kind: ProvenMove, MoveProven: true}); err == nil {
		t.Fatal("duplicate consumed history accepted")
	}
}

func TestRandomRestoreRetryRetainsMarker(t *testing.T) {
	h, first, err := AllocateRandomRestore(fixtureIncarnationHistory(), "restore")
	if err != nil {
		t.Fatal(err)
	}
	_, retry, err := AllocateRandomRestore(h, "restore")
	if err != nil || retry != first {
		t.Fatal("random constructor did not reuse durable retry marker")
	}
	_, second, err := AllocateRandomRestore(h, "separate")
	if err != nil || second.Successor.Restore == first.Successor.Restore {
		t.Fatal("separate restore marker collision")
	}
}
