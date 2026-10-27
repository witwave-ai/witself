package syncmodel

import (
	"testing"
	"time"
)

func TestDeletionKnowledgeGrantFloorAndPins(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d := DeletionKnowledge{AccountID: "account", Family: "facts_subjects_candidates_aliases", ObjectID: "object", Generation: GenerationID{1}, DeletionGeneration: GenerationID{2}, AcceptedDelete: OperationID{3}, IssuerStream: "stream", Frontier: 1, EvidenceRef: "proof", DeletedAt: at, RetainUntil: at.Add(37 * 24 * time.Hour)}
	if err := d.Validate(DefaultOfflineGrant); err != nil {
		t.Fatal(err)
	}
	d.RetainUntil = d.RetainUntil.Add(-time.Nanosecond)
	if err := d.Validate(DefaultOfflineGrant); err == nil {
		t.Fatal("below-floor deletion retention accepted")
	}
	d.RetainUntil = d.RetainUntil.Add(time.Nanosecond)
	if err := d.Validate(31 * 24 * time.Hour); err == nil {
		t.Fatal("grant extension accepted before retention extension")
	}
	if d.CanCollect(d.RetainUntil.Add(-time.Nanosecond)) || !d.CanCollect(d.RetainUntil) {
		t.Fatal("deletion retention boundary incorrect")
	}
	d.UnresolvedPins = 1
	if d.CanCollect(at.Add(365 * 24 * time.Hour)) {
		t.Fatal("unresolved deletion identity collected")
	}
}

func TestPayloadRetirementCommitmentFloorModel(t *testing.T) {
	settled := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	retired := settled.Add(3 * 24 * time.Hour)
	floor := RetentionFloor(settled, retired)
	if floor != retired.Add(37*24*time.Hour) || RetentionFloor(retired, settled) != floor {
		t.Fatal("receipt floor did not use later timestamp")
	}
	var pins EvidencePins
	for _, kind := range []string{"identity", "evidence", "comparison_key", "signing_key", "deletion"} {
		for _, owner := range []string{"operation", "recovery"} {
			if err := pins.Pin("account-a", kind, "reference", owner); err != nil {
				t.Fatal(err)
			}
		}
		if pins.CanCollect("account-a", kind, "reference", floor.Add(time.Hour), floor) {
			t.Fatal("retirement collected pinned evidence")
		}
		pins.Release("account-a", kind, "reference", "operation")
		if pins.CanCollect("account-a", kind, "reference", floor.Add(time.Hour), floor) {
			t.Fatal("one release removed another owner's pin")
		}
		if !pins.CanCollect("account-b", kind, "reference", floor.Add(time.Hour), floor) {
			t.Fatal("evidence pins crossed accounts")
		}
		pins.Release("account-a", kind, "reference", "recovery")
		if pins.CanCollect("account-a", kind, "reference", floor.Add(-time.Nanosecond), floor) || !pins.CanCollect("account-a", kind, "reference", floor, floor) {
			t.Fatal("evidence floor not enforced after final release")
		}
	}
}
