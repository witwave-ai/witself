package syncmodel

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func evidenceMembershipFixture(t *testing.T) (*OrderedEvidenceModel, EvidenceScope, EvidenceMembershipProof) {
	t.Helper()
	model := &OrderedEvidenceModel{}
	scope := EvidenceScope{Incarnation: Incarnation{AccountID: "account", Lifecycle: 1}, RealmID: "realm", OwnerID: "owner", ConversationID: "conversation", Generation: GenerationID{1}}
	if err := model.AppendProjection(scope, RevisionID{1}, 10, []TranscriptEntryID{"entry_A", "entry_B", "entry_C"}); err != nil {
		t.Fatal(err)
	}
	proof, err := model.SealRange(scope, RevisionID{3}, RevisionID{1}, 10, 12)
	if err != nil {
		t.Fatal(err)
	}
	return model, scope, proof
}

func TestOrderedEvidenceMembershipScenario(t *testing.T) {
	if _, err := observeEvidenceMembershipScenario(RunOrderedEvidenceMembershipScenario); err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceMembershipInterleaving(t *testing.T) {
	// Every insertion of two unrelated captures into A/B/C preserves exactly
	// A/B/C. Assertions use their sealed IDs, not a merged endpoint interval.
	for x := 0; x < 5; x++ {
		for y := x + 1; y < 5; y++ {
			model, scope, proof := evidenceMembershipFixture(t)
			merged := make([]TranscriptEntryID, 5)
			merged[x], merged[y] = "entry_X", "entry_Y"
			var wantPositions []Ordinal
			next := 0
			for i := range merged {
				if merged[i] == "" {
					merged[i] = proof.Members[next]
					next++
					wantPositions = append(wantPositions, 20+Ordinal(i))
				}
				// A one-entry capture can be replayed between every other capture.
				for range 2 {
					if err := model.AppendProjection(scope, RevisionID{2}, 20+Ordinal(i), merged[i:i+1]); err != nil {
						t.Fatal(err)
					}
				}
				if err := model.Verify(proof); err != nil {
					t.Fatal("interleaved capture changed the seal")
				}
			}
			positions, err := model.Positions(proof, RevisionID{2})
			if err != nil || !slices.Equal(positions, wantPositions) || !slices.Equal(proof.Members, []TranscriptEntryID{"entry_A", "entry_B", "entry_C"}) {
				t.Fatal("interleaving changed ordered membership")
			}
		}
	}
}

func TestEvidenceMembershipRetryBackfillDeletion(t *testing.T) {
	model, scope, proof := evidenceMembershipFixture(t)
	if err := model.Delete(scope, "entry_B"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		// A stale replay and a fresh backfill projection retain the tombstone.
		for _, projection := range []RevisionID{{1}, {4}} {
			if err := model.AppendProjection(scope, projection, 10, []TranscriptEntryID{"entry_A", "entry_B", "entry_C"}); err != nil {
				t.Fatal(err)
			}
		}
		retry, err := model.BackfillRange(scope, proof.EvidenceID, proof.SourceProjection, 10, 12)
		if err != nil || !reflect.DeepEqual(retry, proof) {
			t.Fatal("backfill retry changed membership or aliases")
		}
		unavailable, err := model.Unavailable(retry)
		if err != nil || !slices.Equal(unavailable, []TranscriptEntryID{"entry_B"}) {
			t.Fatal("replay resurrected an unavailable member")
		}
		if err := model.Delete(scope, "entry_B"); err != nil {
			t.Fatal("deletion retry failed")
		}
	}
	// A partial initial backfill cannot seal an endpoint-only approximation.
	if _, err := model.BackfillRange(scope, RevisionID{5}, RevisionID{1}, 10, 13); err == nil {
		t.Fatal("incomplete legacy range was sealed")
	}
	if err := model.AppendProjection(scope, RevisionID{1}, 13, []TranscriptEntryID{"entry_D"}); err != nil {
		t.Fatal(err)
	}
	completed, err := model.BackfillRange(scope, RevisionID{5}, RevisionID{1}, 10, 13)
	if err != nil || !slices.Equal(completed.Members, []TranscriptEntryID{"entry_A", "entry_B", "entry_C", "entry_D"}) {
		t.Fatal("backfill refusal poisoned retry")
	}
	if err := model.Verify(proof); err != nil {
		t.Fatal("later capture expanded existing evidence")
	}
	if _, err := model.BackfillRange(scope, proof.EvidenceID, RevisionID{1}, 10, 13); err == nil {
		t.Fatal("existing evidence identity accepted a larger range")
	}
	if _, err := model.BackfillRange(scope, proof.EvidenceID, RevisionID{4}, 10, 12); err == nil {
		t.Fatal("existing evidence identity changed source projection")
	}
}

func TestEvidenceMembershipRejectsForgedProof(t *testing.T) {
	model, scope, proof := evidenceMembershipFixture(t)
	for name, mutate := range map[string]func(*EvidenceMembershipProof){
		"insert_interleaved": func(p *EvidenceMembershipProof) { p.Members = []TranscriptEntryID{"entry_A", "entry_X", "entry_C"} },
		"reorder":            func(p *EvidenceMembershipProof) { p.Members[0], p.Members[1] = p.Members[1], p.Members[0] },
		"omit":               func(p *EvidenceMembershipProof) { p.Members = p.Members[:2] },
		"duplicate":          func(p *EvidenceMembershipProof) { p.Members[1] = p.Members[0] },
		"append":             func(p *EvidenceMembershipProof) { p.Members = append(p.Members, "entry_X") },
		"first":              func(p *EvidenceMembershipProof) { p.First++ },
		"last":               func(p *EvidenceMembershipProof) { p.Last++ },
		"source":             func(p *EvidenceMembershipProof) { p.SourceProjection = RevisionID{9} },
		"seal":               func(p *EvidenceMembershipProof) { p.EvidenceID = RevisionID{9} },
		"account":            func(p *EvidenceMembershipProof) { p.Scope.Incarnation.AccountID = "other" },
		"incarnation":        func(p *EvidenceMembershipProof) { p.Scope.Incarnation.Lifecycle++ },
		"generation":         func(p *EvidenceMembershipProof) { p.Scope.Generation = GenerationID{2} },
	} {
		t.Run(name, func(t *testing.T) {
			forged := cloneEvidenceProof(proof)
			mutate(&forged)
			if model.Verify(forged) == nil {
				t.Fatal("forged membership verified")
			}
			if _, err := model.Positions(forged, RevisionID{1}); err == nil {
				t.Fatal("forged membership resolved")
			}
			if _, err := model.Unavailable(forged); err == nil {
				t.Fatal("forged membership read availability")
			}
		})
	}
	for i, members := range [][]TranscriptEntryID{{"entry_A", "entry_C", "entry_B"}, {"entry_A", "entry_C"}} {
		id := RevisionID{byte(i + 5)}
		if err := model.AppendProjection(scope, id, 20, members); err != nil {
			t.Fatal(err)
		}
		if _, err := model.Positions(proof, id); err == nil {
			t.Fatal("missing or reordered destination member accepted")
		}
	}
}

func TestEvidenceMembershipProjectionAppendAndOwnership(t *testing.T) {
	model, scope, proof := evidenceMembershipFixture(t)
	for _, request := range []struct {
		first   Ordinal
		members []TranscriptEntryID
	}{
		{9, []TranscriptEntryID{"entry_prior"}},
		{14, []TranscriptEntryID{"entry_gap"}},
		{11, []TranscriptEntryID{"entry_changed"}},
		{13, []TranscriptEntryID{"entry_A"}},
		{13, []TranscriptEntryID{"entry_D", "entry_D"}},
	} {
		if err := model.AppendProjection(scope, RevisionID{1}, request.first, request.members); err == nil {
			t.Fatal("invalid append was accepted")
		}
	}
	input := []TranscriptEntryID{"entry_C", "entry_D"}
	if err := model.AppendProjection(scope, RevisionID{1}, 12, input); err != nil {
		t.Fatal("matching overlap plus append refused")
	}
	input[1] = "caller_mutation"
	proof.Members[1] = "caller_mutation"
	retained, err := model.BackfillRange(scope, RevisionID{3}, RevisionID{1}, 10, 12)
	if err != nil || !slices.Equal(retained.Members, []TranscriptEntryID{"entry_A", "entry_B", "entry_C"}) {
		t.Fatal("caller changed retained proof")
	}
	appended, err := model.SealRange(scope, RevisionID{4}, RevisionID{1}, 13, 13)
	if err != nil || !slices.Equal(appended.Members, []TranscriptEntryID{"entry_D"}) {
		t.Fatal("caller changed retained projection")
	}
	if err := model.Delete(scope, "entry_B"); err != nil {
		t.Fatal(err)
	}
	newGeneration := scope
	newGeneration.Generation = GenerationID{2}
	if err := model.AppendProjection(newGeneration, RevisionID{1}, 10, []TranscriptEntryID{"new_entry_A", "new_entry_B", "new_entry_C"}); err != nil {
		t.Fatal(err)
	}
	newProof, err := model.SealRange(newGeneration, RevisionID{3}, RevisionID{1}, 10, 12)
	if err != nil || reflect.DeepEqual(newProof, retained) {
		t.Fatal("new conversation generation reused old evidence")
	}
	if err := model.Verify(retained); err != nil {
		t.Fatal("new conversation generation retargeted old evidence")
	}
	if unavailable, err := model.Unavailable(retained); err != nil || !slices.Equal(unavailable, []TranscriptEntryID{"entry_B"}) {
		t.Fatal("new generation erased old deletion knowledge")
	}
	for _, change := range []func(*EvidenceScope){
		func(s *EvidenceScope) { s.Incarnation.AccountID = "other_account" },
		func(s *EvidenceScope) { s.Incarnation.Lifecycle++ },
		func(s *EvidenceScope) { s.Incarnation.Restore = RestoreMarker{2} },
		func(s *EvidenceScope) { s.RealmID = "other_realm" },
		func(s *EvidenceScope) { s.OwnerID = "other_owner" },
		func(s *EvidenceScope) { s.ConversationID = "other_conversation" },
		func(s *EvidenceScope) { s.Generation = GenerationID{2} },
	} {
		isolated, originalScope, original := evidenceMembershipFixture(t)
		otherScope := originalScope
		change(&otherScope)
		if err := isolated.AppendProjection(otherScope, original.SourceProjection, 10, original.Members); err != nil {
			t.Fatal(err)
		}
		other, err := isolated.SealRange(otherScope, original.EvidenceID, original.SourceProjection, 10, 12)
		if err != nil {
			t.Fatal(err)
		}
		if err := isolated.Delete(originalScope, "entry_B"); err != nil {
			t.Fatal(err)
		}
		if unavailable, err := isolated.Unavailable(other); err != nil || len(unavailable) != 0 {
			t.Fatal("deletion crossed evidence ownership or history scope")
		}
		if unavailable, err := isolated.Unavailable(original); err != nil || !slices.Equal(unavailable, []TranscriptEntryID{"entry_B"}) {
			t.Fatal("coexisting evidence scope changed original deletion")
		}
	}
}

func TestEvidenceMembershipValueFreeBounds(t *testing.T) {
	model, scope, proof := evidenceMembershipFixture(t)
	for _, bad := range []string{"", "external/name", `external\name`, "payload with spaces", "https://example.test", strings.Repeat("a", 64), "prefix_" + strings.Repeat("F", 64), strings.Repeat("z", 129)} {
		if err := model.AppendProjection(scope, RevisionID{9}, 1, []TranscriptEntryID{TranscriptEntryID(bad)}); err == nil {
			t.Fatal("non-value-free entry label accepted")
		}
		badScope := scope
		badScope.ConversationID = bad
		if err := model.AppendProjection(badScope, RevisionID{9}, 1, []TranscriptEntryID{"entry_valid"}); err == nil {
			t.Fatal("non-value-free scope label accepted")
		}
	}
	for _, first := range []Ordinal{0, Ordinal(MaxCounter), Ordinal(MaxCounter + 1)} {
		if err := model.AppendProjection(scope, RevisionID{9}, first, []TranscriptEntryID{"entry_D", "entry_E"}); err == nil {
			t.Fatal("invalid projection coordinate bound accepted")
		}
	}
	if err := model.AppendProjection(scope, RevisionID{9}, 1, make([]TranscriptEntryID, MaxEvidenceMembers+1)); err == nil {
		t.Fatal("unbounded projection accepted")
	}
	bounded := make([]TranscriptEntryID, MaxEvidenceMembers)
	for i := range bounded {
		bounded[i] = TranscriptEntryID("bounded_" + strconv.Itoa(i))
	}
	if err := model.AppendProjection(scope, RevisionID{10}, 1, bounded[:MaxEvidenceMembers-1]); err != nil {
		t.Fatal("below-capacity projection refused")
	}
	if err := model.AppendProjection(scope, RevisionID{10}, MaxEvidenceMembers, bounded[MaxEvidenceMembers-1:]); err != nil {
		t.Fatal("at-capacity append refused")
	}
	if err := model.AppendProjection(scope, RevisionID{10}, MaxEvidenceMembers+1, []TranscriptEntryID{"overflow"}); err == nil {
		t.Fatal("above-capacity append accepted")
	}
	full, err := model.SealRange(scope, RevisionID{11}, RevisionID{10}, 1, MaxEvidenceMembers)
	if err != nil || !slices.Equal(full.Members, bounded) {
		t.Fatal("capacity refusal changed existing projection")
	}
	if err := model.AppendProjection(scope, RevisionID{12}, Ordinal(MaxCounter), []TranscriptEntryID{"last_entry"}); err != nil {
		t.Fatal("last valid coordinate refused")
	}
	last, err := model.SealRange(scope, RevisionID{13}, RevisionID{12}, Ordinal(MaxCounter), Ordinal(MaxCounter))
	if err != nil || !slices.Equal(last.Members, []TranscriptEntryID{"last_entry"}) {
		t.Fatal("last valid range refused")
	}
	if err := model.Delete(scope, "unknown_entry"); err == nil {
		t.Fatal("unknown entry deletion silently accepted")
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip EvidenceMembershipProof
	if err := json.Unmarshal(raw, &roundTrip); err != nil || model.Verify(roundTrip) != nil {
		t.Fatal("value-free proof did not round trip")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 6 || fields["members"] == nil || fields["scope"] == nil || fields["source_projection"] == nil {
		t.Fatal("proof schema has unexpected or missing fields")
	}
}
