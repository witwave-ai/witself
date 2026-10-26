package syncmodel

import (
	"errors"
	"reflect"
)

// RecoveryReason is the separate SC-6.13 vocabulary, never a receipt reason.
type RecoveryReason string

// RecoveryState separates readiness, pending work, reconciliation and blocking.
type RecoveryState string

// RecoveryAction is the closed set of permitted SC-6.13 next-step indications.
type RecoveryAction string

// RecoveryCoverage records whether required history through the frontier is proved.
type RecoveryCoverage string

const (
	// ContinuationVerified marks authenticated continuation through the declared coverage.
	ContinuationVerified RecoveryReason = "continuation_verified"
	// AuthorityTransferPending requires missing fence, installation or publication evidence.
	AuthorityTransferPending RecoveryReason = "authority_transfer_pending"
	// RebootstrapRequired preserves pending work while replacing a stale projection.
	RebootstrapRequired RecoveryReason = "rebootstrap_required"
	// DurabilityPublicationPending keeps a committed outcome pending its durability proof.
	DurabilityPublicationPending RecoveryReason = "durability_publication_pending"
	// RecoveryInProgress indicates that supported recovery work remains unfinished.
	RecoveryInProgress RecoveryReason = "recovery_in_progress"
	// RecoveryComplete certifies recovery through the independently verified frontier.
	RecoveryComplete RecoveryReason = "recovery_complete"
	// CursorAuthenticationFailed refuses continuation from an unverifiable cursor.
	CursorAuthenticationFailed RecoveryReason = "cursor_authentication_failed"
	// ReceiptAuthenticationFailed refuses unverifiable historical outcome evidence.
	ReceiptAuthenticationFailed RecoveryReason = "receipt_authentication_failed"
	// ReplicaReenrollmentRequired requires a new authorized device enrollment.
	ReplicaReenrollmentRequired RecoveryReason = "replica_reenrollment_required"
	// AcceptanceAmbiguous preserves work whose prior acceptance cannot be established.
	AcceptanceAmbiguous RecoveryReason = "acceptance_ambiguous"
	// AcceptedEffectUnrecoverable preserves a known outcome whose required effect is lost.
	AcceptedEffectUnrecoverable RecoveryReason = "accepted_effect_unrecoverable"
	// HistoryGapUnrecoverable identifies an independently proved missing history range.
	HistoryGapUnrecoverable RecoveryReason = "history_gap_unrecoverable"
	// ContinuityUnproven refuses to infer a latest frontier from incomplete evidence.
	ContinuityUnproven RecoveryReason = "continuity_unproven"
	// DeletionCoverageUnproven blocks recovery that could resurrect deleted material.
	DeletionCoverageUnproven RecoveryReason = "deletion_coverage_unproven"
	// AuthorizationCoverageUnproven requires current permission and revocation evidence.
	AuthorizationCoverageUnproven RecoveryReason = "authorization_coverage_unproven"
	// EvidenceConflict blocks contradictory authenticated histories.
	EvidenceConflict RecoveryReason = "evidence_conflict"

	// RecoveryReady means the declared coverage has no unresolved recovery obligation.
	RecoveryReady RecoveryState = "ready"
	// RecoveryPending means a required publication or authority proof is outstanding.
	RecoveryPending RecoveryState = "pending"
	// RecoveryReconciling means verified state is being recovered or rebootstrap is needed.
	RecoveryReconciling RecoveryState = "reconciling"
	// RecoveryBlocked means missing or contradictory evidence prevents continuation.
	RecoveryBlocked RecoveryState = "blocked"

	// RecoveryNone indicates that no additional recovery action is required.
	RecoveryNone RecoveryAction = "none"
	// RetrySameOperation retains the original operation identity while retrying publication.
	RetrySameOperation RecoveryAction = "retry_same_operation"
	// RebootstrapPreservePending replaces projections without discarding unresolved work.
	RebootstrapPreservePending RecoveryAction = "rebootstrap_preserve_pending"
	// ObtainEvidence requires additional authenticated recovery evidence.
	ObtainEvidence RecoveryAction = "obtain_evidence"
	// Reenroll requires fresh authoritative replica enrollment.
	Reenroll RecoveryAction = "reenroll"
	// OperatorReview requires an explicit proof-based resolution or loss disposition.
	OperatorReview RecoveryAction = "operator_review"

	// CoverageProven means the required frontier is covered by verified evidence.
	CoverageProven RecoveryCoverage = "proven"
	// CoverageUnproven means the available evidence cannot establish required coverage.
	CoverageUnproven RecoveryCoverage = "unproven"
)

// RecoveryCase describes only declared account/stream coverage, never latest
// whole-account continuity inferred from an observed maximum ordinal.
type RecoveryCase struct {
	Version                  uint32           `json:"protocol_version"`
	RecoveryID               string           `json:"recovery_id"`
	AccountID                string           `json:"account_id"`
	Stream                   string           `json:"stream"`
	From                     Ordinal          `json:"from"`
	Through                  Ordinal          `json:"through"`
	State                    RecoveryState    `json:"recovery_state"`
	Reason                   RecoveryReason   `json:"reason_code"`
	RequiredAction           RecoveryAction   `json:"required_action"`
	Convergence              bool             `json:"convergence"`
	BlockedOperations        Ordinal          `json:"blocked_operation_count"`
	MissingRanges            Ordinal          `json:"missing_range_count"`
	ReceiptEvidence          bool             `json:"receipt_evidence_present"`
	CommittedJournalEvidence bool             `json:"committed_journal_evidence_present"`
	ReplicaEvidence          bool             `json:"replica_evidence_present"`
	DeletionCoverage         RecoveryCoverage `json:"deletion_coverage"`
	AuthorizationCoverage    RecoveryCoverage `json:"authorization_coverage"`
}

// CanonicalBytes encodes a validated recovery case with exact decimal counters.
func (c RecoveryCase) CanonicalBytes() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return MarshalCanonical(c)
}

// DecodeRecoveryCase rejects malformed, ambiguous or unsupported recovery records.
func DecodeRecoveryCase(raw []byte) (RecoveryCase, error) {
	var c RecoveryCase
	if err := UnmarshalStrict(raw, &c); err != nil {
		return RecoveryCase{}, err
	}
	if err := requireWireFields(raw, reflect.TypeFor[RecoveryCase]()); err != nil {
		return RecoveryCase{}, err
	}
	if err := c.Validate(); err != nil {
		return RecoveryCase{}, err
	}
	return c, nil
}

type recoveryOutcome struct {
	reason      RecoveryReason
	state       RecoveryState
	action      RecoveryAction
	convergence bool
}

var recoveryOutcomes = [...]recoveryOutcome{
	{ContinuationVerified, RecoveryReady, RecoveryNone, true},
	{AuthorityTransferPending, RecoveryPending, ObtainEvidence, false},
	{RebootstrapRequired, RecoveryReconciling, RebootstrapPreservePending, false},
	{DurabilityPublicationPending, RecoveryPending, RetrySameOperation, false},
	{RecoveryInProgress, RecoveryReconciling, ObtainEvidence, false},
	{RecoveryComplete, RecoveryReady, RecoveryNone, true},
	{CursorAuthenticationFailed, RecoveryBlocked, RebootstrapPreservePending, false},
	{ReceiptAuthenticationFailed, RecoveryBlocked, ObtainEvidence, false},
	{ReplicaReenrollmentRequired, RecoveryBlocked, Reenroll, false},
	{AcceptanceAmbiguous, RecoveryBlocked, ObtainEvidence, false},
	{AcceptedEffectUnrecoverable, RecoveryBlocked, OperatorReview, false},
	{HistoryGapUnrecoverable, RecoveryBlocked, OperatorReview, false},
	{ContinuityUnproven, RecoveryBlocked, ObtainEvidence, false},
	{DeletionCoverageUnproven, RecoveryBlocked, ObtainEvidence, false},
	{AuthorizationCoverageUnproven, RecoveryBlocked, ObtainEvidence, false},
	{EvidenceConflict, RecoveryBlocked, OperatorReview, false},
}

// Validate enforces the closed outcome tuple and its declared coverage constraints.
func (c RecoveryCase) Validate() error {
	if c.Version != 1 || !validModelReference(c.RecoveryID) || !validModelReference(c.AccountID) || !validModelReference(c.Stream) ||
		c.From.Validate() != nil || c.Through.Validate() != nil || c.From > c.Through || c.BlockedOperations.Validate() != nil || c.MissingRanges.Validate() != nil ||
		(c.DeletionCoverage != CoverageProven && c.DeletionCoverage != CoverageUnproven) ||
		(c.AuthorizationCoverage != CoverageProven && c.AuthorizationCoverage != CoverageUnproven) {
		return errors.New("invalid recovery case")
	}
	for _, outcome := range recoveryOutcomes {
		if c.Reason == outcome.reason && c.State == outcome.state && c.RequiredAction == outcome.action && c.Convergence == outcome.convergence {
			if c.Convergence && (c.BlockedOperations != 0 || c.MissingRanges != 0 || c.DeletionCoverage != CoverageProven || c.AuthorizationCoverage != CoverageProven) {
				return errors.New("incomplete recovery coverage")
			}
			return nil
		}
	}
	return errors.New("invalid recovery outcome")
}

// RecoveryEvidence is a trusted-fixture assertion, not a proof verifier. R must
// mean authenticated terminal receipt plus prefix membership; J must mean a
// complete committed authority journal, never a replica outbox; P must mean an
// authorized replica. The caller validates these cryptographic prerequisites.
type RecoveryEvidence struct {
	Receipt, CommittedJournal, Replica                         bool
	AcceptedOutcome                                            bool
	JournalHasMaterialization, ReplicaMatchesResult            bool
	BaseProvesEffect, VerifiedLaterDeletionOrRetirement        bool
	IndependentFrontier, CompleteDependencies                  bool
	DeletionCoverage, AuthorizationCoverage                    bool
	ConflictingHistories, KnownMissingRange                    bool
	EffectsRecovered, HorizonExpired                           bool
	FencedHistoryProvesNonacceptance, UnresolvedExternalEffect bool
}

// RecoveryDecision deliberately has no provider-dispatch, new-operation, or
// logical-action output. Recovery preserves identities and original accounting.
type RecoveryDecision struct {
	Case                                   RecoveryCase
	RecoverOutcome, RecoverMaterialization bool
	RetrySameOperationAllowed              bool
}

// EvaluateRecovery applies the eight-row SC-6.13 table and mandatory overrides.
// Contradiction takes precedence, then deletion, authorization and frontier
// coverage. Expiry can require rebootstrap only after acceptance is resolved;
// it never erases an ambiguous outcome or manufactures nonacceptance.
func EvaluateRecovery(c RecoveryCase, e RecoveryEvidence) (RecoveryDecision, error) {
	c.Version = 1
	c.ReceiptEvidence, c.CommittedJournalEvidence, c.ReplicaEvidence = e.Receipt, e.CommittedJournal, e.Replica
	c.DeletionCoverage, c.AuthorizationCoverage = CoverageUnproven, CoverageUnproven
	if e.DeletionCoverage {
		c.DeletionCoverage = CoverageProven
	}
	if e.AuthorizationCoverage {
		c.AuthorizationCoverage = CoverageProven
	}
	decision := RecoveryDecision{Case: c}
	// This skeleton has no scoped proof object capable of distinguishing a
	// replica's provisional materialization from contradictory accepted state.
	// Keep P-only ambiguous. A positive fenced exclusion can settle only the
	// completely empty, gap-free operation case, with every coverage check met.
	exclusionComplete := e.FencedHistoryProvesNonacceptance && !e.Receipt && !e.CommittedJournal && !e.Replica &&
		!e.KnownMissingRange && c.BlockedOperations == 0 && c.MissingRanges == 0 && !e.UnresolvedExternalEffect &&
		e.IndependentFrontier && e.CompleteDependencies && e.DeletionCoverage && e.AuthorizationCoverage
	reason := RecoveryInProgress
	switch {
	case e.ConflictingHistories || (e.FencedHistoryProvesNonacceptance && (e.Receipt || e.CommittedJournal)):
		reason = EvidenceConflict
	case !e.DeletionCoverage:
		reason = DeletionCoverageUnproven
	case !e.AuthorizationCoverage:
		reason = AuthorizationCoverageUnproven
	case !e.IndependentFrontier:
		reason = ContinuityUnproven
	case exclusionComplete:
		if e.HorizonExpired {
			reason = RebootstrapRequired
		} else if e.EffectsRecovered {
			reason = RecoveryComplete
		}
	case !e.Receipt && !e.CommittedJournal && e.Replica:
		reason = AcceptanceAmbiguous
	case !e.Receipt && !e.CommittedJournal:
		if e.KnownMissingRange {
			reason = HistoryGapUnrecoverable
		} else {
			reason = ContinuityUnproven
		}
	case !e.CompleteDependencies:
		if e.KnownMissingRange {
			reason = HistoryGapUnrecoverable
		}
	default:
		decision.RecoverOutcome = true
		hasEffect := !e.AcceptedOutcome || e.BaseProvesEffect || e.VerifiedLaterDeletionOrRetirement ||
			(e.CommittedJournal && e.JournalHasMaterialization) || (e.Replica && e.ReplicaMatchesResult)
		if !hasEffect {
			reason = AcceptedEffectUnrecoverable
		} else {
			decision.RecoverMaterialization = e.AcceptedOutcome && !e.VerifiedLaterDeletionOrRetirement
			switch {
			case e.HorizonExpired:
				reason = RebootstrapRequired
			case e.EffectsRecovered && c.BlockedOperations == 0 && c.MissingRanges == 0 && !e.UnresolvedExternalEffect:
				reason = RecoveryComplete
			}
		}
	}
	// A missing row or expired horizon is never nonacceptance proof. Blocked,
	// ambiguous, pending and rebootstrap cases cannot authorize automatic retry.
	decision.RetrySameOperationAllowed = exclusionComplete && reason == RecoveryComplete
	for _, outcome := range recoveryOutcomes {
		if outcome.reason == reason {
			decision.Case.Reason, decision.Case.State = reason, outcome.state
			decision.Case.RequiredAction, decision.Case.Convergence = outcome.action, outcome.convergence
			break
		}
	}
	if err := decision.Case.Validate(); err != nil {
		return RecoveryDecision{}, err
	}
	return decision, nil
}
