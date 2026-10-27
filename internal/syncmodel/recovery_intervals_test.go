package syncmodel

import (
	"reflect"
	"testing"
)

func completeRecoveryInterval(id string, from, through Ordinal) RecoveryInterval {
	e := fixtureRecoveryEvidence()
	e.CommittedJournal = true
	return RecoveryInterval{GroupID: id, Range: OrdinalRange{from, through}, Evidence: e, JournalSource: CommittedAuthorityJournal, JournalCoverage: []OrdinalRange{{from, through}}}
}

func fixtureRecoveryIntervalContext() RecoveryEvidence {
	e := fixtureRecoveryEvidence()
	e.KnownMissingRange = false // no additional gap beyond the declared groups
	return e
}

func TestRecoveryIntervalCoverageAndAtomicHoles(t *testing.T) {
	c, global := fixtureRecoveryCase(), fixtureRecoveryIntervalContext()
	c.Through = 6
	first, middle, last := completeRecoveryInterval("first", 1, 2), completeRecoveryInterval("middle", 3, 4), completeRecoveryInterval("last", 5, 6)
	first.JournalCoverage = []OrdinalRange{{1, 2}, {2, 2}, {1, 1}} // overlapping duplicate delivery is harmless
	complete, err := EvaluateRecoveryIntervals(c, global, []RecoveryInterval{last, first, middle})
	if err != nil || complete.WholeCase.Reason != RecoveryComplete || !complete.WholeCase.Convergence || len(complete.Holes) != 0 {
		t.Fatal("complete out-of-order intervals did not recover")
	}
	partial, err := EvaluateRecoveryIntervals(c, global, []RecoveryInterval{first, last})
	if err != nil || partial.WholeCase.Reason != HistoryGapUnrecoverable || partial.WholeCase.Convergence || len(partial.Holes) != 1 || partial.Holes[0] != (OrdinalRange{3, 4}) || len(partial.Groups) != 2 {
		t.Fatal("largest ordinal hid missing middle atomic group")
	}
	for _, group := range partial.Groups {
		if !group.RecoverOutcome || !group.RecoverMaterialization {
			t.Fatal("individually complete groups were lost")
		}
	}
	middle.JournalCoverage = []OrdinalRange{{3, 3}}
	partial, err = EvaluateRecoveryIntervals(c, global, []RecoveryInterval{first, middle, last})
	if err != nil || partial.WholeCase.Reason != HistoryGapUnrecoverable || partial.Groups[1].RecoverOutcome || partial.Groups[1].RecoverMaterialization || partial.Groups[1].Case.CommittedJournalEvidence {
		t.Fatal("partial journal recovered a partial atomic group")
	}
	middle.Evidence.Receipt, middle.Evidence.Replica = true, true
	middle.Evidence.CommittedJournal = false
	middle.ReplicaCoverage = []OrdinalRange{{3, 3}}
	partial, err = EvaluateRecoveryIntervals(c, global, []RecoveryInterval{first, middle, last})
	if err != nil || partial.WholeCase.Reason != AcceptedEffectUnrecoverable || !partial.Groups[1].RecoverOutcome || partial.Groups[1].RecoverMaterialization {
		t.Fatal("partial replica bytes reconstructed accepted group")
	}
	middle.ReplicaCoverage = []OrdinalRange{{3, 3}, {4, 4}}
	complete, err = EvaluateRecoveryIntervals(c, global, []RecoveryInterval{first, middle, last})
	if err != nil || !complete.WholeCase.Convergence {
		t.Fatal("complete result-matched replica coverage rejected")
	}
}

func TestRecoveryReplicaOutboxAndInputOnlyReceipt(t *testing.T) {
	c, global := fixtureRecoveryCase(), fixtureRecoveryIntervalContext()
	group := completeRecoveryInterval("atomic", c.From, c.Through)
	group.JournalSource = ReplicaOutbox
	out, err := EvaluateRecoveryIntervals(c, global, []RecoveryInterval{group})
	if err != nil || out.WholeCase.Reason != HistoryGapUnrecoverable || out.WholeCase.CommittedJournalEvidence {
		t.Fatal("replica outbox was authority journal")
	}
	group.Evidence.Receipt, group.Evidence.Replica = true, true
	group.ReplicaCoverage = []OrdinalRange{group.Range}
	group.Evidence.ReplicaMatchesResult = false // input commitment cannot prove result
	out, err = EvaluateRecoveryIntervals(c, global, []RecoveryInterval{group})
	if err != nil || out.WholeCase.Reason != AcceptedEffectUnrecoverable || !out.Groups[0].RecoverOutcome || out.Groups[0].RecoverMaterialization {
		t.Fatal("input-only receipt authorized effect reconstruction")
	}
}

func TestRecoveryIntervalsRejectOverlapsAndWrongRanges(t *testing.T) {
	c, global := fixtureRecoveryCase(), fixtureRecoveryIntervalContext()
	for _, groups := range [][]RecoveryInterval{
		{completeRecoveryInterval("one", 1, 4), completeRecoveryInterval("two", 4, 7)},
		{completeRecoveryInterval("same", 1, 3), completeRecoveryInterval("same", 4, 7)},
		{completeRecoveryInterval("outside", 1, 8)},
		{completeRecoveryInterval("zero", 0, 7)},
	} {
		if _, err := EvaluateRecoveryIntervals(c, global, groups); err == nil {
			t.Fatal("invalid interval inventory accepted")
		}
	}
	group := completeRecoveryInterval("wrong-journal-range", 1, 7)
	group.JournalCoverage = []OrdinalRange{{1, 8}}
	if _, err := EvaluateRecoveryIntervals(c, global, []RecoveryInterval{group}); err == nil {
		t.Fatal("journal outside declared range accepted")
	}
}

func TestRecoveryIntervalsOverridePrecedence(t *testing.T) {
	c, e := fixtureRecoveryCase(), fixtureRecoveryIntervalContext()
	// A known hole and complete accepted group coexist. Every override applies
	// to the whole coverage, without erasing individually recoverable groups.
	group := completeRecoveryInterval("complete", 1, 3)
	e.ConflictingHistories, e.DeletionCoverage, e.AuthorizationCoverage, e.IndependentFrontier = true, false, false, false
	for _, want := range []RecoveryReason{EvidenceConflict, DeletionCoverageUnproven, AuthorizationCoverageUnproven, ContinuityUnproven, HistoryGapUnrecoverable} {
		out, err := EvaluateRecoveryIntervals(c, e, []RecoveryInterval{group})
		if err != nil || out.WholeCase.Reason != want || out.WholeCase.Convergence {
			t.Fatal("interval override precedence changed", want)
		}
		switch want {
		case EvidenceConflict:
			e.ConflictingHistories = false
		case DeletionCoverageUnproven:
			e.DeletionCoverage = true
		case AuthorizationCoverageUnproven:
			e.AuthorizationCoverage = true
		case ContinuityUnproven:
			e.IndependentFrontier = true
		}
	}
}

func TestIndistinguishableWholeSystemRollback(t *testing.T) {
	// Worlds differ only in lost history. The observer receives identical
	// surviving datasets; the hidden history is deliberately not an input.
	type world struct {
		surviving   []ContinuityObservation
		lostHistory []OperationID
	}
	complete := world{surviving: []ContinuityObservation{{Domain: "shared-host", Head: 4, Authenticated: true, Current: true}}}
	rolledBack := world{surviving: append([]ContinuityObservation(nil), complete.surviving...), lostHistory: []OperationID{{5}, {6}}}
	if !reflect.DeepEqual(complete.surviving, rolledBack.surviving) || len(complete.lostHistory) == len(rolledBack.lostHistory) {
		t.Fatal("paired worlds did not preserve identical observations with different missing history")
	}
	for _, candidate := range []world{complete, rolledBack} {
		observed := candidate.surviving
		frontier, reason := SurvivingIndependentFrontier([]string{"shared-host"}, observed)
		if frontier != 0 || reason != ContinuityUnproven {
			t.Fatal("indistinguishable surviving state certified a frontier")
		}
		e := fixtureRecoveryEvidence()
		e.Receipt, e.CommittedJournal, e.Replica = true, true, true
		e.IndependentFrontier = reason == ContinuationVerified
		d, err := EvaluateRecovery(fixtureRecoveryCase(), e)
		if err != nil || d.Case.Reason != ContinuityUnproven || d.Case.Convergence {
			t.Fatal("stale agreement certified recovery")
		}
	}
}

func TestDatabaseVsWholeDiskRestoreDomain(t *testing.T) {
	observations := []ContinuityObservation{{Domain: "local-witness-disk", Head: 9, Authenticated: true, Current: true}}
	frontier, reason := SurvivingIndependentFrontier([]string{"pgdata"}, observations)
	if frontier != 9 || reason != ContinuationVerified {
		t.Fatal("surviving witness did not prove database rollback frontier")
	}
	c, e := fixtureRecoveryCase(), fixtureRecoveryEvidence()
	c.From, c.Through = 5, frontier
	e.KnownMissingRange = true
	d, err := EvaluateRecovery(c, e)
	if err != nil || d.Case.Reason != HistoryGapUnrecoverable || d.Case.Convergence {
		t.Fatal("independent ahead head reconstructed lost history")
	}
	if frontier, reason = SurvivingIndependentFrontier([]string{"pgdata", "local-witness-disk"}, observations); frontier != 0 || reason != ContinuityUnproven {
		t.Fatal("whole-disk restore certified completeness")
	}
	observations = append(observations, ContinuityObservation{Domain: "external", Head: 10, Authenticated: true, Current: true})
	if _, reason = SurvivingIndependentFrontier([]string{"pgdata"}, observations); reason != EvidenceConflict {
		t.Fatal("contradictory current authenticated heads elected a winner")
	}
	for _, observation := range []ContinuityObservation{{Domain: "external", Head: 9, Current: true}, {Domain: "external", Head: 9, Authenticated: true}} {
		if _, reason := SurvivingIndependentFrontier([]string{"pgdata"}, []ContinuityObservation{observation}); reason != ContinuityUnproven {
			t.Fatal("unauthenticated or stale observation established continuity")
		}
	}
}

func TestRecoveryIntervalsGlobalPendingAndContradiction(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*RecoveryCase, *RecoveryEvidence)
		want   RecoveryReason
	}{
		{"extra_known_gap", func(_ *RecoveryCase, e *RecoveryEvidence) { e.KnownMissingRange = true }, HistoryGapUnrecoverable},
		{"extra_gap_without_frontier", func(_ *RecoveryCase, e *RecoveryEvidence) { e.KnownMissingRange = true; e.IndependentFrontier = false }, ContinuityUnproven},
		{"declared_unresolved_range", func(c *RecoveryCase, _ *RecoveryEvidence) { c.MissingRanges = 1 }, RecoveryInProgress},
		{"declared_unresolved_operation", func(c *RecoveryCase, _ *RecoveryEvidence) { c.BlockedOperations = 1 }, RecoveryInProgress},
		{"fenced_exclusion_conflicts_with_journal", func(_ *RecoveryCase, e *RecoveryEvidence) { e.FencedHistoryProvesNonacceptance = true }, EvidenceConflict},
		{"unresolved_external_effect", func(_ *RecoveryCase, e *RecoveryEvidence) { e.UnresolvedExternalEffect = true }, RecoveryInProgress},
		{"global_dependencies_missing", func(_ *RecoveryCase, e *RecoveryEvidence) { e.CompleteDependencies = false }, HistoryGapUnrecoverable},
		{"global_effect_recovery_pending", func(_ *RecoveryCase, e *RecoveryEvidence) { e.EffectsRecovered = false }, RecoveryInProgress},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, e := fixtureRecoveryCase(), fixtureRecoveryIntervalContext()
			test.mutate(&c, &e)
			result, err := EvaluateRecoveryIntervals(c, e, []RecoveryInterval{completeRecoveryInterval("complete", c.From, c.Through)})
			if err != nil || result.WholeCase.Reason != test.want || result.WholeCase.Convergence {
				t.Fatal("global unresolved state was ignored or became a validation error", test.want)
			}
			if test.name == "declared_unresolved_range" && result.WholeCase.MissingRanges != 1 {
				t.Fatal("global unresolved range count lost")
			}
		})
	}
}
