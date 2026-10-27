package syncmodel

import (
	"reflect"
	"testing"
)

func transitionFixture(kind NamespaceTransitionKind) NamespaceTransitionInput {
	return NamespaceTransitionInput{
		ID: "transition", Kind: kind, SourceNamespace: "namespace-source", DestinationNamespace: "namespace-source", BindingID: "binding-original",
		SourceEntries: []string{"source-populated"}, DestinationEntries: []string{"destination-populated"},
		Pending: []OperationID{{1}}, Completed: []OperationID{{2}}, LeaseThrough: 10,
		RequiredReviews: []TransitionReview{{Kind: "identity", Reference: "collision"}, {Kind: "ownership", Reference: "edge"}, {Kind: "sealed_reference", Reference: "sealed"}},
	}
}

func transitionEvidence(in NamespaceTransitionInput) TransitionEvidence {
	return TransitionEvidence{CapacityAvailable: true, Reviewed: in.RequiredReviews, PopulatedVerified: true, CopyComplete: true, Watermark: 5, WatermarkVerified: true, IdentityVerified: true, PublicationPinned: true}
}

func mustTransition(t *testing.T, in NamespaceTransitionInput) *NamespaceTransition {
	t.Helper()
	m, err := NewNamespaceTransition(in)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAdoptionJoinEnrollment(t *testing.T) {
	stages := []TransitionCheckpoint{TransitionAdmissionReview, TransitionSnapshotStaged, TransitionCatchupPending, TransitionIdentityVerified, TransitionPublicationPending, TransitionComplete}
	for _, kind := range []NamespaceTransitionKind{TransitionAdoption, TransitionJoin, TransitionEnrollment} {
		t.Run(string(kind), func(t *testing.T) {
			in := transitionFixture(kind)
			m := mustTransition(t, in)
			e := transitionEvidence(in)
			for _, stage := range stages {
				if stage == TransitionCatchupPending {
					if err := m.RecordPending(OperationID{3}); err != nil {
						t.Fatal(err)
					}
				}
				if err := m.Advance(stage, e); err != nil {
					t.Fatal(err)
				}
				// A lost ACK retries the exact committed checkpoint, not the operation.
				if err := m.Advance(stage, e); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(m.Input(), in) || !reflect.DeepEqual(m.Pending(), []OperationID{{1}, {3}}) && stage != TransitionAdmissionReview && stage != TransitionSnapshotStaged {
					t.Fatal("transition rewrote data, bindings, completed outcomes, lease bounds or pending identity")
				}
			}
			if got, _ := m.Checkpoint(); got != TransitionComplete {
				t.Fatal("complete verified transition remained pending")
			}
		})
	}
}

func TestTransitionRefusalPreservesBothNamespacesAndPending(t *testing.T) {
	for _, test := range []struct {
		name   string
		kind   NamespaceTransitionKind
		mutate func(*NamespaceTransitionInput, *TransitionEvidence)
	}{
		{"adoption_collision", TransitionAdoption, func(in *NamespaceTransitionInput, _ *TransitionEvidence) {
			in.DestinationNamespace = "unrelated-populated"
		}},
		{"join_identity_unreviewed", TransitionJoin, func(_ *NamespaceTransitionInput, e *TransitionEvidence) { e.Reviewed = e.Reviewed[1:] }},
		{"join_duplicate_review", TransitionJoin, func(_ *NamespaceTransitionInput, e *TransitionEvidence) {
			e.Reviewed = []TransitionReview{e.Reviewed[0], e.Reviewed[0], e.Reviewed[2]}
		}},
		{"enrollment_prepopulated_unverified", TransitionEnrollment, func(_ *NamespaceTransitionInput, e *TransitionEvidence) { e.PopulatedVerified = false }},
		{"adoption_capacity", TransitionAdoption, func(_ *NamespaceTransitionInput, e *TransitionEvidence) { e.CapacityAvailable = false }},
		{"join_capacity", TransitionJoin, func(_ *NamespaceTransitionInput, e *TransitionEvidence) { e.CapacityAvailable = false }},
		{"enrollment_capacity", TransitionEnrollment, func(_ *NamespaceTransitionInput, e *TransitionEvidence) { e.CapacityAvailable = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := transitionFixture(test.kind)
			e := transitionEvidence(in)
			test.mutate(&in, &e)
			m := mustTransition(t, in)
			if err := m.Advance(TransitionAdmissionReview, e); err != nil {
				t.Fatal(err)
			}
			if err := m.Advance(TransitionSnapshotStaged, e); err == nil {
				t.Fatal("unsafe transition accepted")
			}
			if got, _ := m.Checkpoint(); got != TransitionBlocked {
				t.Fatal("refusal not explicit")
			}
			if err := m.RecordPending(OperationID{3}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(m.Input(), in) || !reflect.DeepEqual(m.Pending(), []OperationID{{1}, {3}}) {
				t.Fatal("refusal discarded preserved data or ongoing work")
			}
		})
	}
}

func TestTransitionCrashResumeAndPartialCopy(t *testing.T) {
	stages := []TransitionCheckpoint{TransitionIntent, TransitionAdmissionReview, TransitionSnapshotStaged, TransitionCatchupPending, TransitionIdentityVerified, TransitionPublicationPending}
	for _, kind := range []NamespaceTransitionKind{TransitionAdoption, TransitionJoin, TransitionEnrollment} {
		for i, crash := range stages {
			t.Run(string(kind)+"/"+string(crash), func(t *testing.T) {
				in := transitionFixture(kind)
				m := mustTransition(t, in)
				e := transitionEvidence(in)
				for _, stage := range stages[1 : i+1] {
					if err := m.Advance(stage, e); err != nil {
						t.Fatal(err)
					}
				}
				m.Interrupt(e.Watermark, true)
				got, _ := m.Checkpoint()
				if i < 3 {
					if got != TransitionRebootstrap {
						t.Fatal("partial copy resumed without durable verified watermark")
					}
				} else if got != TransitionCatchupPending {
					t.Fatal("verified restart did not resume catchup")
				}
				if err := m.RecordPending(OperationID{4}); err != nil {
					t.Fatal(err)
				}
				m.Interrupt(e.Watermark+1, true)
				if got, _ := m.Checkpoint(); got != TransitionRebootstrap {
					t.Fatal("altered watermark continued")
				}
				if err := m.Rebootstrap(); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(m.Input(), in) || !reflect.DeepEqual(m.Pending(), []OperationID{{1}, {4}}) {
					t.Fatal("restart lost original or ongoing work")
				}
			})
		}
	}
	m := mustTransition(t, transitionFixture(TransitionJoin))
	e := transitionEvidence(m.Input())
	for _, stage := range []TransitionCheckpoint{TransitionAdmissionReview, TransitionSnapshotStaged} {
		if err := m.Advance(stage, e); err != nil {
			t.Fatal(err)
		}
	}
	e.CopyComplete = false
	if m.Advance(TransitionCatchupPending, e) == nil || m.Advance(TransitionComplete, e) == nil {
		t.Fatal("partial copy reported synchronization")
	}
}

func TestTransitionOngoingWriteInvalidatesCatchupAndDefensiveCopies(t *testing.T) {
	in := transitionFixture(TransitionEnrollment)
	m := mustTransition(t, in)
	e := transitionEvidence(in)
	for _, stage := range []TransitionCheckpoint{TransitionAdmissionReview, TransitionSnapshotStaged, TransitionCatchupPending, TransitionIdentityVerified, TransitionPublicationPending} {
		if err := m.Advance(stage, e); err != nil {
			t.Fatal(err)
		}
	}
	in.SourceEntries[0] = "caller-overwrite"
	copyOfInput := m.Input()
	copyOfInput.DestinationEntries[0] = "inspection-overwrite"
	copyOfPending := m.Pending()
	copyOfPending[0] = OperationID{99}
	if err := m.RecordPending(OperationID{3}); err != nil {
		t.Fatal(err)
	}
	if m.Advance(TransitionComplete, e) == nil || m.Advance(TransitionIdentityVerified, e) == nil {
		t.Fatal("stale catchup completed after ongoing write")
	}
	e.Watermark++
	for _, stage := range []TransitionCheckpoint{TransitionIdentityVerified, TransitionPublicationPending, TransitionComplete} {
		if err := m.Advance(stage, e); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(m.Input(), transitionFixture(TransitionEnrollment)) || m.Pending()[0] != (OperationID{1}) {
		t.Fatal("caller mutated frozen transition")
	}
}

func restoreFixture(t *testing.T) RestoreState {
	t.Helper()
	h := fixtureIncarnationHistory()
	s, err := BeginRestore(RestoreState{History: h}, "restore-newest-backup", []RestoreIdentity{{Operation: OperationID{7}, AcceptedIncarnation: h.Current}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRestoreMarkerCrashMatrix(t *testing.T) {
	stages := []RestoreStage{RestoreIntent, RestoreMarkerSet, RestoreImported, RestoreCAS, RestoreInstalled, RestorePublished}
	for _, crash := range stages {
		t.Run(string(crash), func(t *testing.T) {
			s := restoreFixture(t)
			original := cloneRestore(s)
			for _, stage := range stages[1:] {
				before := cloneRestore(s)
				next, err := AdvanceRestore(s, "restore-newest-backup", stage, RestoreMarker{2}, true)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(s, before) {
					t.Fatal("checkpoint mutated previous durable state")
				}
				s = next
				if stage == crash {
					// Model reconnect reopens the last committed state and retries its ACK.
					replay, err := AdvanceRestore(cloneRestore(s), "restore-newest-backup", stage, RestoreMarker{2}, true)
					if err != nil || !reflect.DeepEqual(s, replay) {
						t.Fatal("restore crash replay changed marker or transition")
					}
					s = replay
				}
				if stage != RestorePublished && s.History.Current != original.History.Current {
					t.Fatal("restore became ready before publication")
				}
				if !reflect.DeepEqual(s.Records[0].Identities, original.Records[0].Identities) {
					t.Fatal("restore rewrote operation or receipt incarnation")
				}
			}
			if s.History.Current.Restore == original.History.Current.Restore || len(s.History.Allocations) != 1 {
				t.Fatal("newest backup reused marker or retry allocated twice")
			}
			if _, err := AbortRestore(s, "restore-newest-backup"); err == nil {
				t.Fatal("published restore aborted")
			}
		})
	}
}

func TestRestoreAbortedAllocationsAndExternalAuthority(t *testing.T) {
	s := restoreFixture(t)
	var err error
	for _, stage := range []RestoreStage{RestoreMarkerSet, RestoreImported} {
		s, err = AdvanceRestore(s, "restore-newest-backup", stage, RestoreMarker{2}, true)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := AdvanceRestore(s, "restore-newest-backup", RestoreCAS, RestoreMarker{2}, false); err == nil {
		t.Fatal("copied authority passed installation barrier")
	}
	s, err = AbortRestore(s, "restore-newest-backup")
	if err != nil {
		t.Fatal(err)
	}
	s, err = BeginRestore(s, "separate-restore", s.Records[0].Identities)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AdvanceRestore(s, "separate-restore", RestoreMarkerSet, RestoreMarker{2}, true); err == nil {
		t.Fatal("aborted marker reused")
	}
	s, err = AdvanceRestore(s, "separate-restore", RestoreMarkerSet, RestoreMarker{3}, true)
	if err != nil || s.History.LastAllocated != 9 || s.Records[1].Allocation.Successor.Restore == s.Records[0].Allocation.Successor.Restore {
		t.Fatal("aborted epoch not consumed or separate marker not fresh")
	}
	if _, err := AdvanceRestore(s, "restore-newest-backup", RestoreImported, RestoreMarker{2}, true); err == nil {
		t.Fatal("aborted restore resumed")
	}
}

func TestTransitionKindsDoNotSilentlyRemapOrMerge(t *testing.T) {
	for _, kind := range []NamespaceTransitionKind{TransitionAdoption, TransitionJoin, TransitionEnrollment} {
		in := transitionFixture(kind)
		in.DestinationNamespace = "other-namespace"
		in.DestinationEntries = nil
		m := mustTransition(t, in)
		want := in.DestinationNamespace
		if kind == TransitionAdoption {
			want = in.SourceNamespace
		}
		if m.Namespace() != want || !reflect.DeepEqual(m.Input(), in) {
			t.Fatal("transition silently remapped a namespace or merged source data")
		}
	}
	for _, mutate := range []func(*NamespaceTransitionInput){
		func(in *NamespaceTransitionInput) { in.Kind = "merge_everything" },
		func(in *NamespaceTransitionInput) { in.BindingID = "" },
		func(in *NamespaceTransitionInput) {
			in.RequiredReviews = append(in.RequiredReviews, in.RequiredReviews[0])
		},
		func(in *NamespaceTransitionInput) { in.RequiredReviews[0].Kind = "arbitrary" },
		func(in *NamespaceTransitionInput) { in.SourceEntries = append(in.SourceEntries, in.SourceEntries[0]) },
		func(in *NamespaceTransitionInput) { in.Completed = in.Pending },
		func(in *NamespaceTransitionInput) {
			in.SourceEntries = append(in.SourceEntries, in.DestinationEntries[0])
		},
	} {
		in := transitionFixture(TransitionJoin)
		mutate(&in)
		if _, err := NewNamespaceTransition(in); err == nil {
			t.Fatal("invalid or omitted collision inventory accepted")
		}
	}
}

func TestRestoreIntentReplayScopeAndCompetingPublication(t *testing.T) {
	s := restoreFixture(t)
	replay, err := BeginRestore(s, "restore-newest-backup", s.Records[0].Identities)
	if err != nil || !reflect.DeepEqual(s, replay) {
		t.Fatal("intent crash retry changed durable restore")
	}
	changed := append([]RestoreIdentity(nil), s.Records[0].Identities...)
	changed[0].Operation = OperationID{9}
	if _, err := BeginRestore(s, "restore-newest-backup", changed); err == nil {
		t.Fatal("changed operation reused restore intent")
	}
	changed[0].AcceptedIncarnation.AccountID = "other-account"
	if _, err := BeginRestore(s, "foreign", changed); err == nil {
		t.Fatal("cross-account restore identity admitted")
	}
	for _, stage := range []RestoreStage{RestoreMarkerSet, RestoreImported} {
		s, err = AdvanceRestore(s, "restore-newest-backup", stage, RestoreMarker{2}, true)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := AdvanceRestore(s, "restore-newest-backup", RestoreImported, RestoreMarker{3}, true); err == nil {
		t.Fatal("lost ACK changed durable marker")
	}
	s, err = BeginRestore(s, "competing-restore", s.Records[0].Identities)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []RestoreStage{RestoreMarkerSet, RestoreImported} {
		s, err = AdvanceRestore(s, "competing-restore", stage, RestoreMarker{3}, true)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, stage := range []RestoreStage{RestoreCAS, RestoreInstalled, RestorePublished} {
		s, err = AdvanceRestore(s, "restore-newest-backup", stage, RestoreMarker{2}, true)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := AdvanceRestore(s, "competing-restore", RestoreCAS, RestoreMarker{3}, true); err == nil {
		t.Fatal("stale predecessor CAS installed competing restore")
	}
}
