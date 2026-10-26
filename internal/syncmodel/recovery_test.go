package syncmodel

import (
	"bytes"
	"strings"
	"testing"
)

func fixtureRecoveryCase() RecoveryCase {
	return RecoveryCase{RecoveryID: "recovery", AccountID: "synthetic-account", Stream: "authority", From: 1, Through: 7}
}

func fixtureRecoveryEvidence() RecoveryEvidence {
	return RecoveryEvidence{AcceptedOutcome: true, JournalHasMaterialization: true, ReplicaMatchesResult: true,
		IndependentFrontier: true, CompleteDependencies: true, DeletionCoverage: true, AuthorizationCoverage: true,
		KnownMissingRange: true, EffectsRecovered: true}
}

func TestRecoveryRJPTruthTable(t *testing.T) {
	for _, test := range []struct {
		name    string
		r, j, p bool
		want    RecoveryReason
	}{
		{"111", true, true, true, RecoveryComplete},
		{"110", true, true, false, RecoveryComplete},
		{"101", true, false, true, RecoveryComplete},
		{"100", true, false, false, AcceptedEffectUnrecoverable},
		{"011", false, true, true, RecoveryComplete},
		{"010", false, true, false, RecoveryComplete},
		{"001", false, false, true, AcceptanceAmbiguous},
		{"000", false, false, false, HistoryGapUnrecoverable},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := fixtureRecoveryEvidence()
			e.Receipt, e.CommittedJournal, e.Replica = test.r, test.j, test.p
			d, err := EvaluateRecovery(fixtureRecoveryCase(), e)
			if err != nil || d.Case.Reason != test.want {
				t.Fatal("R/J/P decision differs from normative table")
			}
			if d.Case.ReceiptEvidence != test.r || d.Case.CommittedJournalEvidence != test.j || d.Case.ReplicaEvidence != test.p {
				t.Fatal("oracle changed evidence classes")
			}
			if d.RetrySameOperationAllowed {
				t.Fatal("recovery manufactured nonacceptance")
			}
		})
	}
}

func TestRecoveryMandatoryOverrides(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*RecoveryCase, *RecoveryEvidence)
		want   RecoveryReason
	}{
		{"conflict_over_all_coverage", func(_ *RecoveryCase, e *RecoveryEvidence) {
			e.ConflictingHistories = true
			e.DeletionCoverage = false
			e.AuthorizationCoverage = false
			e.IndependentFrontier = false
		}, EvidenceConflict},
		{"deletion_before_authorization", func(_ *RecoveryCase, e *RecoveryEvidence) {
			e.DeletionCoverage = false
			e.AuthorizationCoverage = false
		}, DeletionCoverageUnproven},
		{"authorization_before_frontier", func(_ *RecoveryCase, e *RecoveryEvidence) {
			e.AuthorizationCoverage = false
			e.IndependentFrontier = false
		}, AuthorizationCoverageUnproven},
		{"all_copies_agree_without_frontier", func(_ *RecoveryCase, e *RecoveryEvidence) { e.IndependentFrontier = false }, ContinuityUnproven},
		{"unknown_empty_frontier", func(_ *RecoveryCase, e *RecoveryEvidence) {
			e.Receipt = false
			e.CommittedJournal = false
			e.Replica = false
			e.KnownMissingRange = false
		}, ContinuityUnproven},
		{"input_commitment_not_materialization", func(_ *RecoveryCase, e *RecoveryEvidence) { e.CommittedJournal = false; e.ReplicaMatchesResult = false }, AcceptedEffectUnrecoverable},
		{"journal_payload_missing", func(_ *RecoveryCase, e *RecoveryEvidence) { e.JournalHasMaterialization = false; e.Replica = false }, AcceptedEffectUnrecoverable},
		{"missing_middle_atomic_group", func(_ *RecoveryCase, e *RecoveryEvidence) { e.CompleteDependencies = false }, HistoryGapUnrecoverable},
		{"incomplete_dependencies_pending", func(_ *RecoveryCase, e *RecoveryEvidence) {
			e.CompleteDependencies = false
			e.KnownMissingRange = false
		}, RecoveryInProgress},
		{"horizon_preserves_ambiguity", func(_ *RecoveryCase, e *RecoveryEvidence) {
			e.Receipt = false
			e.CommittedJournal = false
			e.HorizonExpired = true
		}, AcceptanceAmbiguous},
		{"horizon_preserves_effect_loss", func(_ *RecoveryCase, e *RecoveryEvidence) {
			e.CommittedJournal = false
			e.Replica = false
			e.HorizonExpired = true
		}, AcceptedEffectUnrecoverable},
		{"horizon_requires_rebootstrap", func(_ *RecoveryCase, e *RecoveryEvidence) { e.HorizonExpired = true }, RebootstrapRequired},
		{"pending_operation_blocks_convergence", func(c *RecoveryCase, _ *RecoveryEvidence) { c.BlockedOperations = 1 }, RecoveryInProgress},
		{"missing_range_blocks_convergence", func(c *RecoveryCase, _ *RecoveryEvidence) { c.MissingRanges = 1 }, RecoveryInProgress},
		{"external_effect_never_reexecuted", func(_ *RecoveryCase, e *RecoveryEvidence) { e.UnresolvedExternalEffect = true }, RecoveryInProgress},
		{"not_yet_recovered", func(_ *RecoveryCase, e *RecoveryEvidence) { e.EffectsRecovered = false }, RecoveryInProgress},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, e := fixtureRecoveryCase(), fixtureRecoveryEvidence()
			e.Receipt, e.CommittedJournal, e.Replica = true, true, true
			test.mutate(&c, &e)
			d, err := EvaluateRecovery(c, e)
			if err != nil || d.Case.Reason != test.want || d.Case.Convergence || d.RetrySameOperationAllowed {
				t.Fatal("mandatory recovery override lost")
			}
		})
	}
}

func TestRecoveryReceiptOnlyAndVerifiedDeletion(t *testing.T) {
	for _, mode := range []string{"refusal", "restored_base", "later_deletion"} {
		t.Run(mode, func(t *testing.T) {
			e := fixtureRecoveryEvidence()
			e.Receipt = true
			switch mode {
			case "refusal":
				e.AcceptedOutcome = false
			case "restored_base":
				e.BaseProvesEffect = true
			case "later_deletion":
				e.VerifiedLaterDeletionOrRetirement = true
			}
			d, err := EvaluateRecovery(fixtureRecoveryCase(), e)
			if err != nil || d.Case.Reason != RecoveryComplete || !d.RecoverOutcome {
				t.Fatal("receipt-only exception lost")
			}
			if mode != "restored_base" && d.RecoverMaterialization {
				t.Fatal("refusal or later deletion resurrected payload")
			}
		})
	}
}

func TestRecoveryNonacceptanceRequiresPositiveFencedProof(t *testing.T) {
	e := fixtureRecoveryEvidence()
	e.AcceptedOutcome, e.KnownMissingRange = false, false
	for _, positive := range []bool{false, true} {
		for _, unresolved := range []bool{false, true} {
			e.FencedHistoryProvesNonacceptance, e.UnresolvedExternalEffect = positive, unresolved
			d, err := EvaluateRecovery(fixtureRecoveryCase(), e)
			if err != nil || d.RetrySameOperationAllowed != (positive && !unresolved) {
				t.Fatal("nonacceptance inferred from missing evidence or unresolved execution")
			}
			if d.RetrySameOperationAllowed && (d.Case.Reason != RecoveryComplete || !d.Case.Convergence) {
				t.Fatal("unresolved recovery authorized automatic retry")
			}
		}
	}
}

func TestRecoveryContradictoryExclusionCannotAuthorizeRetry(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*RecoveryCase, *RecoveryEvidence)
		want   RecoveryReason
	}{
		{"receipt_and_exclusion", func(_ *RecoveryCase, e *RecoveryEvidence) { e.Receipt = true }, EvidenceConflict},
		{"journal_and_exclusion", func(_ *RecoveryCase, e *RecoveryEvidence) { e.CommittedJournal = true }, EvidenceConflict},
		{"replica_stays_ambiguous", func(_ *RecoveryCase, e *RecoveryEvidence) { e.Replica = true }, AcceptanceAmbiguous},
		{"proven_missing_interval", func(_ *RecoveryCase, e *RecoveryEvidence) { e.KnownMissingRange = true }, HistoryGapUnrecoverable},
		{"missing_range_count", func(c *RecoveryCase, _ *RecoveryEvidence) { c.MissingRanges = 1 }, ContinuityUnproven},
		{"blocked_operation_count", func(c *RecoveryCase, _ *RecoveryEvidence) { c.BlockedOperations = 1 }, ContinuityUnproven},
		{"external_effect", func(_ *RecoveryCase, e *RecoveryEvidence) { e.UnresolvedExternalEffect = true }, ContinuityUnproven},
		{"expired_horizon", func(_ *RecoveryCase, e *RecoveryEvidence) { e.HorizonExpired = true }, RebootstrapRequired},
		{"unknown_frontier", func(_ *RecoveryCase, e *RecoveryEvidence) { e.IndependentFrontier = false }, ContinuityUnproven},
		{"uninstalled_evidence", func(_ *RecoveryCase, e *RecoveryEvidence) { e.EffectsRecovered = false }, RecoveryInProgress},
		{"contradictory_histories", func(_ *RecoveryCase, e *RecoveryEvidence) { e.ConflictingHistories = true }, EvidenceConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, e := fixtureRecoveryCase(), fixtureRecoveryEvidence()
			e.AcceptedOutcome, e.KnownMissingRange, e.FencedHistoryProvesNonacceptance = false, false, true
			test.mutate(&c, &e)
			d, err := EvaluateRecovery(c, e)
			if err != nil || d.Case.Reason != test.want || d.RetrySameOperationAllowed || d.Case.Convergence {
				t.Fatal("contradictory or incomplete exclusion authorized retry or convergence")
			}
		})
	}
}

func TestRecoveryExactOutcomeVocabulary(t *testing.T) {
	// Independent transcription of all sixteen normative outcome tuples.
	expected := []struct {
		reason, state, action string
		convergence           bool
	}{
		{"continuation_verified", "ready", "none", true},
		{"authority_transfer_pending", "pending", "obtain_evidence", false},
		{"rebootstrap_required", "reconciling", "rebootstrap_preserve_pending", false},
		{"durability_publication_pending", "pending", "retry_same_operation", false},
		{"recovery_in_progress", "reconciling", "obtain_evidence", false},
		{"recovery_complete", "ready", "none", true},
		{"cursor_authentication_failed", "blocked", "rebootstrap_preserve_pending", false},
		{"receipt_authentication_failed", "blocked", "obtain_evidence", false},
		{"replica_reenrollment_required", "blocked", "reenroll", false},
		{"acceptance_ambiguous", "blocked", "obtain_evidence", false},
		{"accepted_effect_unrecoverable", "blocked", "operator_review", false},
		{"history_gap_unrecoverable", "blocked", "operator_review", false},
		{"continuity_unproven", "blocked", "obtain_evidence", false},
		{"deletion_coverage_unproven", "blocked", "obtain_evidence", false},
		{"authorization_coverage_unproven", "blocked", "obtain_evidence", false},
		{"evidence_conflict", "blocked", "operator_review", false},
	}
	if len(recoveryOutcomes) != len(expected) {
		t.Fatal("recovery reason vocabulary changed")
	}
	for _, tuple := range expected {
		t.Run(tuple.reason, func(t *testing.T) {
			c := fixtureRecoveryCase()
			c.Version, c.DeletionCoverage, c.AuthorizationCoverage = 1, CoverageProven, CoverageProven
			c.Reason, c.State, c.RequiredAction, c.Convergence = RecoveryReason(tuple.reason), RecoveryState(tuple.state), RecoveryAction(tuple.action), tuple.convergence
			if c.Validate() != nil {
				t.Fatal("normative outcome rejected")
			}
			c.Convergence = !c.Convergence
			if c.Validate() == nil {
				t.Fatal("altered convergence accepted")
			}
			c.Convergence = tuple.convergence
			c.RequiredAction = "new_operation"
			if c.Validate() == nil {
				t.Fatal("unknown recovery action accepted")
			}
		})
	}
	c := fixtureRecoveryCase()
	c.Version, c.DeletionCoverage, c.AuthorizationCoverage = 1, CoverageProven, CoverageProven
	c.Reason, c.State, c.RequiredAction = "applied", RecoveryReady, RecoveryNone
	if c.Validate() == nil {
		t.Fatal("receipt reason accepted as recovery outcome")
	}
}

func TestRecoveryStrictCodec(t *testing.T) {
	e := fixtureRecoveryEvidence()
	e.Receipt, e.CommittedJournal = true, true
	d, err := EvaluateRecovery(fixtureRecoveryCase(), e)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := d.Case.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRecoveryCase(wire)
	if err != nil || decoded != d.Case {
		t.Fatal("recovery case did not round-trip")
	}
	again, err := decoded.CanonicalBytes()
	if err != nil || !bytes.Equal(wire, again) {
		t.Fatal("recovery wire bytes changed")
	}
	for _, test := range []struct{ name, from, to string }{
		{"unknown_field", `"protocol_version":1`, `"protocol_version":1,"unreviewed":true`},
		{"duplicate_field", `"protocol_version":1`, `"protocol_version":1,"protocol_version":1`},
		{"unknown_version", `"protocol_version":1`, `"protocol_version":2`},
		{"omitted_boolean", `"receipt_evidence_present":true,`, ``},
		{"numeric_counter", `"from":"1"`, `"from":1`},
		{"noncanonical_counter", `"from":"1"`, `"from":"01"`},
		{"null_counter", `"from":"1"`, `"from":null`},
		{"surrogate", `"recovery_id":"recovery"`, `"recovery_id":"\ud800"`},
		{"unknown_reason", `"reason_code":"recovery_complete"`, `"reason_code":"applied"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated := strings.Replace(string(wire), test.from, test.to, 1)
			if mutated == string(wire) {
				t.Fatal("codec negative did not mutate input")
			}
			if _, err := DecodeRecoveryCase([]byte(mutated)); err == nil {
				t.Fatal("malformed recovery wire accepted")
			}
		})
	}
}
