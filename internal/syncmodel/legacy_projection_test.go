package syncmodel

import (
	"errors"
	"testing"
)

func legacyProjectionFixture() (LegacyProjectionRecord, LegacyProjectionRecord) {
	scope := LegacyProjectionScope{
		Incarnation: Incarnation{AccountID: "account_a", Lifecycle: 1, Restore: RestoreMarker{1}},
		RealmID:     "realm_a", OwnerID: "owner_a", ObjectID: "conversation_a", Generation: GenerationID{1},
		ProjectionID: "projection_original", ExternalName: "conversation_name",
	}
	entry := LegacyProjectionRecord{Coordinate: LegacyCoordinate{LegacyTranscriptSequence, scope, 17}, Identity: LegacyIdentity{EntryID: "entry_original"}}
	version := entry
	version.Coordinate.Kind = LegacyMemoryVersion
	version.Coordinate.Scope.ObjectID, version.Coordinate.Scope.ExternalName = "memory_a", "memory_name"
	version.Identity = LegacyIdentity{VersionID: RevisionID{1}}
	return entry, version
}

func TestLegacyProjectionBackfillScenario(t *testing.T) {
	if _, err := observeLegacyProjectionScenario(RunLegacyProjectionBackfillScenario); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyProjectionBackfillRetry(t *testing.T) {
	entry, version := legacyProjectionFixture()
	var interrupted LegacyProjectionBackfill
	// A partially completed migration resumes with the original typed targets.
	if err := interrupted.Backfill([]LegacyProjectionRecord{entry}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := interrupted.Backfill([]LegacyProjectionRecord{entry, version}); err != nil {
			t.Fatal(err)
		}
		for _, want := range []LegacyProjectionRecord{entry, version} {
			got, err := interrupted.Resolve(want.Coordinate)
			if err != nil || got != want {
				t.Fatal("backfill changed a legacy integer, scope or typed identity")
			}
		}
		if len(interrupted.records) != 2 {
			t.Fatal("retry allocated another mapping")
		}
	}
	// Reordered batch delivery cannot choose different identities.
	var fresh LegacyProjectionBackfill
	if err := fresh.Backfill([]LegacyProjectionRecord{version, entry}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []LegacyProjectionRecord{entry, version} {
		got, err := fresh.Resolve(want.Coordinate)
		if err != nil || got != want {
			t.Fatal("batch order changed the original aliases")
		}
	}
}

func TestLegacyProjectionBackfillAtomicConflict(t *testing.T) {
	entry, version := legacyProjectionFixture()
	var model LegacyProjectionBackfill
	if err := model.Backfill([]LegacyProjectionRecord{entry}); err != nil {
		t.Fatal(err)
	}
	conflict := entry
	conflict.Identity.EntryID = "entry_replacement"
	if err := model.Backfill([]LegacyProjectionRecord{version, conflict}); !errors.Is(err, ErrLegacyProjectionConflict) {
		t.Fatal("same legacy coordinate accepted a replacement identity")
	}
	if _, err := model.Resolve(version.Coordinate); !errors.Is(err, ErrLegacyProjectionMissing) {
		t.Fatal("rejected backfill partly installed its earlier row")
	}
	if got, err := model.Resolve(entry.Coordinate); err != nil || got != entry {
		t.Fatal("conflicting retry changed the original mapping")
	}
	duplicateTarget := entry
	duplicateTarget.Coordinate.Number++
	if err := model.Backfill([]LegacyProjectionRecord{duplicateTarget}); !errors.Is(err, ErrLegacyProjectionConflict) {
		t.Fatal("one projection placed an entry at two numeric aliases")
	}
	changedName := entry
	changedName.Coordinate.Scope.ExternalName = "retagged_name"
	changedName.Identity.EntryID = "entry_replacement"
	if err := model.Backfill([]LegacyProjectionRecord{changedName}); !errors.Is(err, ErrLegacyProjectionConflict) {
		t.Fatal("external-name mutation bypassed immutable projection identity")
	}
}

func TestLegacyProjectionDeletionAndNameReuse(t *testing.T) {
	entry, _ := legacyProjectionFixture()
	var model LegacyProjectionBackfill
	if err := model.Backfill([]LegacyProjectionRecord{entry}); err != nil {
		t.Fatal(err)
	}
	otherProjection := entry
	otherProjection.Coordinate.Scope.ProjectionID = "projection_second"
	otherProjection.Coordinate.Number = 27
	if err := model.Backfill([]LegacyProjectionRecord{otherProjection}); err != nil {
		t.Fatal(err)
	}
	if err := model.Delete(entry.Coordinate); err != nil {
		t.Fatal(err)
	}
	if err := model.Backfill([]LegacyProjectionRecord{entry, otherProjection}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []LegacyProjectionRecord{entry, otherProjection} {
		got, err := model.Resolve(want.Coordinate)
		if err != nil || !got.Deleted || got.Identity != want.Identity {
			t.Fatal("deleted identity was lost or resurrected by an old projection retry")
		}
	}
	if err := model.DeleteGeneration(entry.Coordinate.Kind, entry.Coordinate.Scope); err != nil {
		t.Fatal(err)
	}
	reused := entry
	reused.Coordinate.Scope.ObjectID = "conversation_reused"
	reused.Coordinate.Scope.Generation = GenerationID{2}
	reused.Identity.EntryID = "entry_reused"
	if err := model.Backfill([]LegacyProjectionRecord{reused}); err != nil {
		t.Fatal(err)
	}
	if got, err := model.Resolve(reused.Coordinate); err != nil || got != reused {
		t.Fatal("old generation deletion affected the new generation")
	}
	scope := entry.Coordinate.Scope
	if _, err := model.ResolveUnqualified(entry.Coordinate.Kind, scope.Incarnation, scope.RealmID, scope.OwnerID, scope.ExternalName, entry.Coordinate.Number); !errors.Is(err, ErrLegacyProjectionAmbiguous) {
		t.Fatal("name reuse silently selected the live generation")
	}
	if got, err := model.Resolve(entry.Coordinate); err != nil || !got.Deleted || got.Identity != entry.Identity {
		t.Fatal("name reuse remapped the old alias")
	}
}

func TestLegacyProjectionDeletionBeforeBackfill(t *testing.T) {
	entry, _ := legacyProjectionFixture()
	var model LegacyProjectionBackfill
	if err := model.DeleteGeneration(entry.Coordinate.Kind, entry.Coordinate.Scope); err != nil {
		t.Fatal(err)
	}
	if err := model.Backfill([]LegacyProjectionRecord{entry}); !errors.Is(err, ErrLegacyProjectionDeleted) {
		t.Fatal("stale backfill resurrected a deleted generation")
	}
	// An explicitly historical mapping may preserve its alias after retention.
	historical := entry
	historical.Deleted = true
	if err := model.Backfill([]LegacyProjectionRecord{historical}); err != nil {
		t.Fatal(err)
	}
	if err := model.Backfill([]LegacyProjectionRecord{entry}); err != nil {
		t.Fatal(err)
	}
	if got, err := model.Resolve(entry.Coordinate); err != nil || got != historical {
		t.Fatal("retry erased historical deletion knowledge")
	}
	// Changing a presentation name must not escape the identity deletion floor.
	retagged := entry
	retagged.Coordinate.Scope.ExternalName = "changed_name"
	retagged.Coordinate.Scope.ProjectionID = "new_projection"
	if err := model.Backfill([]LegacyProjectionRecord{retagged}); !errors.Is(err, ErrLegacyProjectionDeleted) {
		t.Fatal("retagged object bypassed generation deletion")
	}
	// A tombstone without any retained row still makes later name reuse visible.
	var empty LegacyProjectionBackfill
	if err := empty.DeleteGeneration(entry.Coordinate.Kind, entry.Coordinate.Scope); err != nil {
		t.Fatal(err)
	}
	reused := entry
	reused.Coordinate.Scope.Generation = GenerationID{2}
	reused.Identity.EntryID = "entry_new"
	if err := empty.Backfill([]LegacyProjectionRecord{reused}); err != nil {
		t.Fatal(err)
	}
	scope := entry.Coordinate.Scope
	if _, err := empty.ResolveUnqualified(entry.Coordinate.Kind, scope.Incarnation, scope.RealmID, scope.OwnerID, scope.ExternalName, entry.Coordinate.Number); !errors.Is(err, ErrLegacyProjectionAmbiguous) {
		t.Fatal("unqualified lookup ignored the retained generation deletion floor")
	}
}

func TestLegacyProjectionAmbiguousCoordinatesAndScope(t *testing.T) {
	entry, version := legacyProjectionFixture()
	for _, original := range []LegacyProjectionRecord{entry, version} {
		t.Run(string(original.Coordinate.Kind), func(t *testing.T) {
			var model LegacyProjectionBackfill
			if err := model.Backfill([]LegacyProjectionRecord{original}); err != nil {
				t.Fatal(err)
			}
			scope := original.Coordinate.Scope
			if got, err := model.ResolveUnqualified(original.Coordinate.Kind, scope.Incarnation, scope.RealmID, scope.OwnerID, scope.ExternalName, original.Coordinate.Number); err != nil || got != original {
				t.Fatal("unique original alias did not resolve")
			}
			for _, change := range []func(*LegacyProjectionScope){
				func(s *LegacyProjectionScope) { s.Incarnation.AccountID = "other_account" },
				func(s *LegacyProjectionScope) { s.Incarnation.Lifecycle++ },
				func(s *LegacyProjectionScope) { s.Incarnation.Restore = RestoreMarker{2} },
				func(s *LegacyProjectionScope) { s.RealmID = "other_realm" },
				func(s *LegacyProjectionScope) { s.OwnerID = "other_owner" },
				func(s *LegacyProjectionScope) { s.Generation = GenerationID{2} },
			} {
				wrong := original.Coordinate
				change(&wrong.Scope)
				if _, err := model.Resolve(wrong); !errors.Is(err, ErrLegacyProjectionMissing) {
					t.Fatal("alias resolved outside its exact ownership and history scope")
				}
			}
			branch := original
			branch.Coordinate.Scope.ProjectionID = "projection_branch"
			if branch.Coordinate.Kind == LegacyTranscriptSequence {
				branch.Identity.EntryID = "entry_branch"
			} else {
				branch.Identity.VersionID = RevisionID{2}
			}
			if err := model.Backfill([]LegacyProjectionRecord{branch}); err != nil {
				t.Fatal(err)
			}
			if _, err := model.ResolveUnqualified(original.Coordinate.Kind, scope.Incarnation, scope.RealmID, scope.OwnerID, scope.ExternalName, original.Coordinate.Number); !errors.Is(err, ErrLegacyProjectionAmbiguous) {
				t.Fatal("equal integers on divergent projections silently chose an identity")
			}
			for _, want := range []LegacyProjectionRecord{original, branch} {
				if got, err := model.Resolve(want.Coordinate); err != nil || got != want {
					t.Fatal("qualified projection lost its meaning after interleaving")
				}
			}
		})
	}
}

func TestLegacyProjectionInvalidMappings(t *testing.T) {
	entry, _ := legacyProjectionFixture()
	for _, change := range []func(*LegacyProjectionRecord){
		func(r *LegacyProjectionRecord) { r.Coordinate.Kind = "unknown" },
		func(r *LegacyProjectionRecord) { r.Coordinate.Number = 0 },
		func(r *LegacyProjectionRecord) { r.Coordinate.Number = Ordinal(MaxCounter + 1) },
		func(r *LegacyProjectionRecord) { r.Coordinate.Scope.Generation = GenerationID{} },
		func(r *LegacyProjectionRecord) { r.Coordinate.Scope.Incarnation.AccountID = "" },
		func(r *LegacyProjectionRecord) { r.Coordinate.Scope.ProjectionID = "" },
		func(r *LegacyProjectionRecord) { r.Identity.EntryID = "" },
		func(r *LegacyProjectionRecord) { r.Identity.VersionID = RevisionID{1} },
		func(r *LegacyProjectionRecord) { r.Coordinate.Kind = LegacyMemoryVersion },
	} {
		var model LegacyProjectionBackfill
		bad := entry
		change(&bad)
		if err := model.Backfill([]LegacyProjectionRecord{entry, bad}); !errors.Is(err, ErrLegacyProjectionInvalid) {
			t.Fatal("malformed alias or untyped target accepted")
		}
		if _, err := model.Resolve(entry.Coordinate); !errors.Is(err, ErrLegacyProjectionMissing) {
			t.Fatal("invalid batch partially changed migration state")
		}
	}
}

func TestLegacyProjectionTypedScopeIsolation(t *testing.T) {
	entry, _ := legacyProjectionFixture()
	for _, change := range []func(*LegacyProjectionRecord){
		func(r *LegacyProjectionRecord) { r.Coordinate.Scope.Incarnation.AccountID = "other_account" },
		func(r *LegacyProjectionRecord) { r.Coordinate.Scope.Incarnation.Lifecycle++ },
		func(r *LegacyProjectionRecord) { r.Coordinate.Scope.Incarnation.Restore = RestoreMarker{2} },
		func(r *LegacyProjectionRecord) { r.Coordinate.Scope.RealmID = "other_realm" },
		func(r *LegacyProjectionRecord) { r.Coordinate.Scope.OwnerID = "other_owner" },
		func(r *LegacyProjectionRecord) { r.Coordinate.Scope.ObjectID = "other_object" },
		func(r *LegacyProjectionRecord) { r.Coordinate.Scope.Generation = GenerationID{2} },
		func(r *LegacyProjectionRecord) {
			r.Coordinate.Kind = LegacyMemoryVersion
			r.Identity = LegacyIdentity{VersionID: RevisionID{1}}
		},
	} {
		var model LegacyProjectionBackfill
		other := entry
		change(&other)
		if err := model.Backfill([]LegacyProjectionRecord{entry, other}); err != nil {
			t.Fatal(err)
		}
		if err := model.Delete(entry.Coordinate); err != nil {
			t.Fatal(err)
		}
		if err := model.DeleteGeneration(entry.Coordinate.Kind, entry.Coordinate.Scope); err != nil {
			t.Fatal(err)
		}
		if got, err := model.Resolve(other.Coordinate); err != nil || got != other {
			t.Fatal("deletion crossed typed object, account, incarnation, ownership or generation scope")
		}
		if err := model.Backfill([]LegacyProjectionRecord{other}); err != nil {
			t.Fatal("another scope inherited the original deletion floor")
		}
	}
}
