package client

import (
	"encoding/json"
	"strconv"
)

// memoryCurationConflictReasons is the closed vocabulary of server-supplied
// reasons this client repeats; it matches the server's list in
// docs/mcp-tools.md. Any other reason, even a well-formed rule name, is
// dropped, so a server body cannot place text of its own in the error that
// the CLI prints and a model reads.
var memoryCurationConflictReasons = map[string]bool{
	"run_not_open":    true,
	"run_not_planned": true,
	"sensitive_source_requires_sensitive_output":      true,
	"sensitive_target_requires_sensitive_replacement": true,
	"target_already_mutated":                          true,
	"invalid_create_output_reference":                 true,
	"memory_version_not_in_inputs":                    true,
	"memory_not_live":                                 true,
	"memory_version_not_current":                      true,
	"evidence_not_in_inputs":                          true,
	"evidence_row_mismatch":                           true,
	"direct_evidence_not_resolved":                    true,
	"transcript_range_not_covered":                    true,
	"direct_evidence_requires_input_row":              true,
}

// MemoryCurationConflictError is a 409 memory curation state conflict whose
// server named the rule that refused the request. Reason comes from the
// closed, value-free vocabulary above; ActionOrdinal (0 when absent) and
// EvidenceIndex (nil when absent) are positions in the caller's own plan.
// errors.Is(err, ErrConflict) holds through the transport wrapper.
type MemoryCurationConflictError struct {
	Reason        string
	ActionOrdinal int64
	EvidenceIndex *int
}

// Error returns the fixed conflict text and, when the server named a known
// rule, that rule and the plan positions, for example
// "memory curation state conflict (reason=transcript_range_not_covered,
// action_ordinal=2, evidence_index=1)". Server text is never repeated.
func (e *MemoryCurationConflictError) Error() string {
	text := "memory curation state conflict"
	if e == nil || e.Reason == "" {
		return text
	}
	text += " (reason=" + e.Reason
	if e.ActionOrdinal > 0 {
		text += ", action_ordinal=" + strconv.FormatInt(e.ActionOrdinal, 10)
	}
	if e.EvidenceIndex != nil {
		text += ", evidence_index=" + strconv.Itoa(*e.EvidenceIndex)
	}
	return text + ")"
}

// newMemoryCurationConflictError keeps only a reason from the closed
// vocabulary, a positive action ordinal and a non-negative evidence index.
// Anything else in the body is dropped, never echoed.
func newMemoryCurationConflictError(rawReason, rawOrdinal, rawIndex json.RawMessage) *MemoryCurationConflictError {
	out := &MemoryCurationConflictError{}
	var reason string
	if json.Unmarshal(rawReason, &reason) != nil || !memoryCurationConflictReasons[reason] {
		return out
	}
	out.Reason = reason
	var ordinal int64
	if json.Unmarshal(rawOrdinal, &ordinal) == nil && ordinal > 0 {
		out.ActionOrdinal = ordinal
	}
	var index *int
	if json.Unmarshal(rawIndex, &index) == nil && index != nil && *index >= 0 {
		out.EvidenceIndex = index
	}
	return out
}
