package syncmodel

import (
	"errors"
	"sort"
)

// OrdinalRange is an inclusive interval, never a largest-observed-watermark claim.
type OrdinalRange struct {
	From, Through Ordinal
}

// JournalEvidenceSource distinguishes authority acceptance from local capture.
type JournalEvidenceSource string

const (
	// CommittedAuthorityJournal is authenticated, committed authority evidence.
	CommittedAuthorityJournal JournalEvidenceSource = "committed_authority"
	// ReplicaOutbox is local intent and cannot establish authority acceptance.
	ReplicaOutbox JournalEvidenceSource = "replica_outbox"
)

// RecoveryInterval describes one complete atomic group. Partial journal or
// replica coverage cannot recover part of this group. All booleans retain the
// trusted-fixture meaning of RecoveryEvidence, not deployed proof verification.
type RecoveryInterval struct {
	GroupID         string
	Range           OrdinalRange
	Evidence        RecoveryEvidence
	JournalSource   JournalEvidenceSource
	JournalCoverage []OrdinalRange
	ReplicaCoverage []OrdinalRange
}

// RecoveryIntervalResult retains individually recoverable groups and explicit
// holes; WholeCase cannot converge while any group or hole remains unresolved.
type RecoveryIntervalResult struct {
	WholeCase RecoveryCase
	Groups    []RecoveryDecision
	Holes     []OrdinalRange
}

func validOrdinalRange(r OrdinalRange) bool {
	return r.From > 0 && r.From.Validate() == nil && r.Through.Validate() == nil && r.From <= r.Through
}

func intervalCovered(target OrdinalRange, supplied []OrdinalRange) (bool, error) {
	parts := append([]OrdinalRange(nil), supplied...)
	for _, r := range parts {
		if !validOrdinalRange(r) || r.From < target.From || r.Through > target.Through {
			return false, errors.New("invalid interval coverage")
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].From < parts[j].From })
	next := target.From
	for _, r := range parts {
		if r.From > next {
			return false, nil
		}
		if r.Through == target.Through {
			return true, nil
		}
		if r.Through >= next {
			next = r.Through + 1
		}
	}
	return false, nil
}

// EvaluateRecoveryIntervals evaluates each declared group, constructs actual
// middle/end holes, and folds outcomes under the same override order as the
// scalar oracle. The global evidence supplies frontier/deletion/authorization
// coverage and global dependency/recovery restrictions; each interval supplies
// R/J/P and effect evidence. Global KnownMissingRange names an additional
// unresolved gap, not the initially missing effects reconstructed by the supplied
// intervals. Global FencedHistoryProvesNonacceptance excludes the whole declared
// coverage and therefore conflicts with any accepted receipt/journal group.
// An empty stream uses the scalar oracle, not an invented zero ordinal interval.
func EvaluateRecoveryIntervals(c RecoveryCase, global RecoveryEvidence, intervals []RecoveryInterval) (RecoveryIntervalResult, error) {
	var result RecoveryIntervalResult
	if !validOrdinalRange(OrdinalRange{c.From, c.Through}) || len(intervals) > 1024 {
		return result, errors.New("invalid recovery intervals")
	}
	groups := append([]RecoveryInterval(nil), intervals...)
	sort.Slice(groups, func(i, j int) bool { return groups[i].Range.From < groups[j].Range.From })
	seen := make(map[string]bool)
	next := c.From
	finished := false
	var decisions []RecoveryDecision
	for _, group := range groups {
		r := group.Range
		if !validLabel(group.GroupID) || seen[group.GroupID] || !validOrdinalRange(r) || finished || r.From < next || r.Through > c.Through {
			return result, errors.New("invalid atomic group coverage")
		}
		seen[group.GroupID] = true
		if r.From > next {
			result.Holes = append(result.Holes, OrdinalRange{next, r.From - 1})
		}
		e := group.Evidence
		e.IndependentFrontier = global.IndependentFrontier
		e.DeletionCoverage = global.DeletionCoverage
		e.AuthorizationCoverage = global.AuthorizationCoverage
		e.ConflictingHistories = e.ConflictingHistories || global.ConflictingHistories
		e.HorizonExpired = e.HorizonExpired || global.HorizonExpired
		e.CompleteDependencies = e.CompleteDependencies && global.CompleteDependencies
		e.EffectsRecovered = e.EffectsRecovered && global.EffectsRecovered
		e.UnresolvedExternalEffect = e.UnresolvedExternalEffect || global.UnresolvedExternalEffect
		e.FencedHistoryProvesNonacceptance = e.FencedHistoryProvesNonacceptance || global.FencedHistoryProvesNonacceptance
		journalComplete, err := intervalCovered(r, group.JournalCoverage)
		if err != nil {
			return result, err
		}
		switch group.JournalSource {
		case "", ReplicaOutbox:
			e.CommittedJournal = false
		case CommittedAuthorityJournal:
			e.CommittedJournal = e.CommittedJournal && journalComplete
		default:
			return result, errors.New("unknown journal source")
		}
		replicaComplete, err := intervalCovered(r, group.ReplicaCoverage)
		if err != nil {
			return result, err
		}
		e.ReplicaMatchesResult = e.ReplicaMatchesResult && replicaComplete
		// An authority journal fragment is not a complete committed group. A
		// receipt may independently prove the group's outcome, but partial P
		// still cannot reconstruct accepted materialization.
		e.KnownMissingRange = global.IndependentFrontier
		groupCase := c
		groupCase.From, groupCase.Through = r.From, r.Through
		groupCase.MissingRanges = 0
		if !e.Receipt && !e.CommittedJournal && global.IndependentFrontier {
			groupCase.MissingRanges = 1
		}
		d, err := EvaluateRecovery(groupCase, e)
		if err != nil {
			return result, err
		}
		decisions = append(decisions, d)
		if r.Through == c.Through {
			finished = true
		} else {
			next = r.Through + 1
		}
	}
	if !finished {
		result.Holes = append(result.Holes, OrdinalRange{next, c.Through})
	}
	result.Groups = decisions
	for _, hole := range result.Holes {
		holeCase := c
		holeCase.From, holeCase.Through = hole.From, hole.Through
		holeCase.MissingRanges = 1
		e := global
		e.Receipt, e.CommittedJournal, e.Replica = false, false, false
		e.FencedHistoryProvesNonacceptance = false
		e.KnownMissingRange = global.IndependentFrontier
		d, err := EvaluateRecovery(holeCase, e)
		if err != nil {
			return result, err
		}
		decisions = append(decisions, d)
	}
	extraMissing := c.MissingRanges
	if global.KnownMissingRange {
		if extraMissing == 0 {
			extraMissing = 1
		}
		gapCase := c
		gapCase.MissingRanges = extraMissing
		e := global
		e.Receipt, e.CommittedJournal, e.Replica = false, false, false
		d, err := EvaluateRecovery(gapCase, e)
		if err != nil {
			return result, err
		}
		decisions = append(decisions, d)
	}
	// No majority, timestamp or maximum ordinal can suppress a blocked group.
	chosen := decisions[0]
	for _, d := range decisions[1:] {
		if recoveryPrecedence(d.Case.Reason) < recoveryPrecedence(chosen.Case.Reason) {
			chosen = d
		}
	}
	whole := chosen.Case
	whole.From, whole.Through = c.From, c.Through
	whole.MissingRanges = extraMissing + Ordinal(len(result.Holes))
	for _, group := range result.Groups {
		whole.MissingRanges += group.Case.MissingRanges
	}
	whole.ReceiptEvidence, whole.CommittedJournalEvidence, whole.ReplicaEvidence = false, false, false
	for _, d := range decisions {
		whole.ReceiptEvidence = whole.ReceiptEvidence || d.Case.ReceiptEvidence
		whole.CommittedJournalEvidence = whole.CommittedJournalEvidence || d.Case.CommittedJournalEvidence
		whole.ReplicaEvidence = whole.ReplicaEvidence || d.Case.ReplicaEvidence
	}
	if whole.Convergence && (whole.MissingRanges != 0 || whole.BlockedOperations != 0) {
		whole.Reason, whole.State, whole.RequiredAction, whole.Convergence = RecoveryInProgress, RecoveryReconciling, ObtainEvidence, false
	}
	if err := whole.Validate(); err != nil {
		return result, err
	}
	result.WholeCase = whole
	return result, nil
}

func recoveryPrecedence(reason RecoveryReason) int {
	for i, candidate := range []RecoveryReason{EvidenceConflict, DeletionCoverageUnproven, AuthorizationCoverageUnproven, ContinuityUnproven, HistoryGapUnrecoverable, AcceptanceAmbiguous, AcceptedEffectUnrecoverable, RebootstrapRequired, RecoveryInProgress, RecoveryComplete} {
		if reason == candidate {
			return i
		}
	}
	return -1
}

// ContinuityObservation names a surviving observation and its restore domain.
// A shared-schema witness and the nodes on its host have the same domain.
type ContinuityObservation struct {
	Domain        string
	Head          Ordinal
	Authenticated bool
	Current       bool
}

// SurvivingIndependentFrontier accepts only a current authenticated observation
// outside every rolled-back domain. Equal stale copies never certify latestness.
// A hidden rollback of every distinguishing observation is unobservable; callers
// must declare recovery/uncertain activation to obtain continuity_unproven.
func SurvivingIndependentFrontier(restoredDomains []string, observations []ContinuityObservation) (Ordinal, RecoveryReason) {
	rolledBack := make(map[string]bool, len(restoredDomains))
	for _, domain := range restoredDomains {
		if !validLabel(domain) {
			return 0, ContinuityUnproven
		}
		rolledBack[domain] = true
	}
	var frontier Ordinal
	found := false
	for _, observation := range observations {
		if !validLabel(observation.Domain) || observation.Head.Validate() != nil || rolledBack[observation.Domain] || !observation.Authenticated || !observation.Current {
			continue
		}
		if found && frontier != observation.Head {
			return 0, EvidenceConflict
		}
		frontier, found = observation.Head, true
	}
	if !found {
		return 0, ContinuityUnproven
	}
	return frontier, ContinuationVerified
}
