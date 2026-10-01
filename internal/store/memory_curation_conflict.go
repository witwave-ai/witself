package store

import "errors"

// Memory curation conflict reasons name the rule that refused a curation
// request. They form a closed, value-free vocabulary: a reason never carries
// memory content, evidence, a transcript entry, a fact value, or an
// identifier. Only a rule that a client's plan can reach and fix has a
// reason; every other conflict, including checks that plan acceptance makes
// unreachable, stays the plain ErrMemoryCurationConflict sentinel.
const (
	memoryCurationReasonRunNotOpen             = "run_not_open"
	memoryCurationReasonRunNotPlanned          = "run_not_planned"
	memoryCurationReasonSensitiveSource        = "sensitive_source_requires_sensitive_output"
	memoryCurationReasonSensitiveReplacement   = "sensitive_target_requires_sensitive_replacement"
	memoryCurationReasonTargetAlreadyMutated   = "target_already_mutated"
	memoryCurationReasonInvalidOutputReference = "invalid_create_output_reference"
	memoryCurationReasonMemoryNotInInputs      = "memory_version_not_in_inputs"
	memoryCurationReasonMemoryNotLive          = "memory_not_live"
	memoryCurationReasonMemoryNotCurrent       = "memory_version_not_current"
	memoryCurationReasonEvidenceNotInInputs    = "evidence_not_in_inputs"
	memoryCurationReasonEvidenceRowMismatch    = "evidence_row_mismatch"
	memoryCurationReasonEvidenceNotResolved    = "direct_evidence_not_resolved"
	memoryCurationReasonTranscriptNotCovered   = "transcript_range_not_covered"
	memoryCurationReasonEvidenceNeedsInputRow  = "direct_evidence_requires_input_row"
)

// MemoryCurationConflictError is ErrMemoryCurationConflict with the rule that
// refused the request. Error returns the sentinel's unchanged text and
// errors.Is(err, ErrMemoryCurationConflict) stays true. ActionOrdinal is the
// ordinal of the refused plan action, or 0 when the rule is not about one
// action. EvidenceIndex is the zero-based position of the refused row in that
// action's evidence list, or nil when the rule is not about one evidence row.
// Both are positions in the caller's own plan, never stored identifiers.
type MemoryCurationConflictError struct {
	Reason        string
	ActionOrdinal int64
	EvidenceIndex *int
}

// Error returns the unchanged text of ErrMemoryCurationConflict.
func (e *MemoryCurationConflictError) Error() string { return ErrMemoryCurationConflict.Error() }

// Unwrap keeps errors.Is(err, ErrMemoryCurationConflict) true.
func (e *MemoryCurationConflictError) Unwrap() error { return ErrMemoryCurationConflict }

// memoryCurationConflict refuses a request for one reason. ordinal is 0 when
// the rule is not about one plan action.
func memoryCurationConflict(reason string, ordinal int64) error {
	return &MemoryCurationConflictError{Reason: reason, ActionOrdinal: ordinal}
}

// conflict refuses the plan action being authorized for one reason.
func (b *memoryCurationPlanAuthorizationBuilder) conflict(reason string) error {
	return memoryCurationConflict(reason, b.ordinal)
}

// withMemoryCurationEvidenceIndex records which evidence row of an action a
// reasoned conflict is about. Every other error passes through unchanged.
func withMemoryCurationEvidenceIndex(err error, index int) error {
	var conflict *MemoryCurationConflictError
	if !errors.As(err, &conflict) {
		return err
	}
	detailed := *conflict
	detailed.EvidenceIndex = &index
	return &detailed
}
