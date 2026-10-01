package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

// requireMemoryCurationConflictReason asserts that err is a memory curation
// conflict whose error text is still the sentinel's. An empty reason means an
// unclassified conflict; any other reason must come with the action ordinal
// and the evidence index (-1 for none).
func requireMemoryCurationConflictReason(
	t *testing.T, label string, err error, reason string, ordinal int64, evidence int,
) {
	t.Helper()
	if !errors.Is(err, ErrMemoryCurationConflict) || err.Error() != ErrMemoryCurationConflict.Error() {
		t.Fatalf("%s: error = %v, want a memory curation conflict", label, err)
	}
	var conflict *MemoryCurationConflictError
	classified := errors.As(err, &conflict)
	if reason == "" {
		if classified {
			t.Fatalf("%s: reason %q, want an unclassified conflict", label, conflict.Reason)
		}
		return
	}
	if !classified {
		t.Fatalf("%s: error = %v, want a reasoned memory curation conflict", label, err)
	}
	got := -1
	if conflict.EvidenceIndex != nil {
		got = *conflict.EvidenceIndex
	}
	if conflict.Reason != reason || conflict.ActionOrdinal != ordinal || got != evidence {
		t.Fatalf("%s: reason %q ordinal %d evidence %d, want %q ordinal %d evidence %d",
			label, conflict.Reason, conflict.ActionOrdinal, got, reason, ordinal, evidence)
	}
}

// TestAuthorizeMemoryCurationPlanNamesConflictReasons pins, for every check of
// plan authorization, either one reason with the refused action's ordinal and,
// for an evidence rule, the row's index, or an unclassified conflict for the
// defensive checks that plan acceptance makes unreachable (issue #608).
func TestAuthorizeMemoryCurationPlanNamesConflictReasons(t *testing.T) {
	messageRow := MemoryEvidence{
		ID: "mev_aaaaaaaaaaaaaaaa", Type: "conversation", Role: MemoryEvidenceSupports,
		ResolutionState: MemoryEvidenceResolved, ResolvedKind: "message", SourceMessageID: "message-1",
	}
	memoryRow := MemoryEvidence{
		ID: "mev_bbbbbbbbbbbbbbbb", Type: "memory", Role: MemoryEvidenceSupports,
		ResolutionState: MemoryEvidenceResolved, ResolvedKind: "memory",
	}
	oddRow := MemoryEvidence{
		ID: "mev_cccccccccccccccc", Type: "conversation", Role: MemoryEvidenceSupports,
		ResolutionState: MemoryEvidenceResolved, ResolvedKind: "unsupported",
	}
	auth := &memoryCurationPlanAuthorization{
		memories: map[string]memoryCurationPlanMemoryInput{
			memoryCurationPlanVersionKey("mem_public", 1):  activeCurationPlanMemory("mem_public", false),
			memoryCurationPlanVersionKey("mem_private", 1): activeCurationPlanMemory("mem_private", true),
			memoryCurationPlanVersionKey("mem_old", 1): {
				MemoryID: "mem_old", Version: 1, State: MemoryStateSuperseded,
				CurrentVersion: sql.NullInt64{Int64: 2, Valid: true},
				CurrentState:   sql.NullString{String: MemoryStateActive, Valid: true},
			},
			memoryCurationPlanVersionKey("mem_gone", 1): {
				MemoryID: "mem_gone", Version: 1, State: MemoryStateForgotten,
				CurrentVersion: sql.NullInt64{Int64: 1, Valid: true},
				CurrentState:   sql.NullString{String: MemoryStateForgotten, Valid: true},
			},
		},
		evidence: map[string]memoryCurationPlanEvidenceInput{
			messageRow.ID: {Evidence: messageRow},
			memoryRow.ID:  {Evidence: memoryRow},
			oddRow.ID:     {Evidence: oddRow},
		},
		transcripts: []memoryCurationPlanTranscriptInput{{TranscriptID: "trn_frozen", From: 1, Until: 3}},
		outputs:     map[string]memoryCurationPlanOutput{"mem_created": {Ordinal: 1}},
	}
	direct := func(from, until int64) MemoryCurationEvidence {
		return MemoryCurationEvidence{
			Type: "conversation", Role: MemoryEvidenceSupports, ResolutionState: MemoryEvidenceResolved,
			ResolvedKind: "transcript", SourceTranscriptID: "trn_frozen",
			SourceSequenceFrom: from, SourceSequenceUntil: until,
		}
	}
	version := func(memoryID string, v int64) MemoryCurationVersionReference {
		return MemoryCurationVersionReference{MemoryID: memoryID, Version: v}
	}
	// plan puts a valid create at ordinal 1 first, so every refusal must
	// report the ordinal of the action that broke a rule, not the first one.
	plan := func(actions ...MemoryCurationPlanAction) []MemoryCurationPlanAction {
		out := []MemoryCurationPlanAction{{
			Ordinal: 1, Operation: MemoryCurationOperationCreate,
			Create: &MemoryCurationCreateAction{LocalRef: "created", MemoryID: "mem_created",
				Snapshot: MemoryCurationMemorySnapshot{Content: "created",
					Evidence: []MemoryCurationEvidence{direct(1, 1)}}},
		}}
		for index, action := range actions {
			action.Ordinal = int64(index + 2)
			out = append(out, action)
		}
		return out
	}
	create := func(evidence ...MemoryCurationEvidence) MemoryCurationPlanAction {
		return MemoryCurationPlanAction{Operation: MemoryCurationOperationCreate,
			Create: &MemoryCurationCreateAction{LocalRef: "second", MemoryID: "mem_second",
				Snapshot: MemoryCurationMemorySnapshot{Content: "second", Evidence: evidence}}}
	}
	replace := func(memoryID string) MemoryCurationPlanAction {
		return MemoryCurationPlanAction{Operation: MemoryCurationOperationReplace,
			Replace: &MemoryCurationReplaceAction{
				Target:   MemoryCurationTargetReference{MemoryID: memoryID, ExpectedVersion: 1},
				Snapshot: MemoryCurationMemorySnapshot{Content: "revised"}}}
	}
	supersede := func(memoryID string, replacement MemoryCurationVersionReference) MemoryCurationPlanAction {
		return MemoryCurationPlanAction{Operation: MemoryCurationOperationSupersede,
			Supersede: &MemoryCurationSupersedeAction{
				Target:       MemoryCurationTargetReference{MemoryID: memoryID, ExpectedVersion: 1},
				Replacements: []MemoryCurationVersionReference{replacement}}}
	}
	relate := func(from MemoryCurationVersionReference) MemoryCurationPlanAction {
		return MemoryCurationPlanAction{Operation: MemoryCurationOperationRelate,
			Relate: &MemoryCurationRelateAction{RelationType: MemoryCurationRelationSummarizes,
				From: from, To: version("mem_public", 1)}}
	}
	withSensitiveRelation := create(direct(1, 1))
	withSensitiveRelation.Create.Relations = []MemoryCurationLineageRelation{{
		RelationType: MemoryCurationRelationDerivedFrom, To: version("mem_private", 1)}}
	sensitiveTranscript := direct(1, 1)
	sensitiveTranscript.ArtifactSensitive = true
	publicFact := MemoryCurationPlanAction{Operation: MemoryCurationOperationProposeFact,
		ProposeFact: &MemoryCurationProposeFactAction{Predicate: "profile/example", ValueType: "string",
			Value: json.RawMessage(`"value"`), Evidence: []MemoryCurationEvidence{sensitiveTranscript}}}
	unknownInput := memoryCurationEvidenceFromInputRow(messageRow)
	unknownInput.InputEvidenceID = "mev_dddddddddddddddd"
	mismatched := memoryCurationEvidenceFromInputRow(messageRow)
	mismatched.SourceMessageID = "message-2"
	forgottenSource := MemoryCurationEvidence{Type: "memory", ResolutionState: MemoryEvidenceResolved,
		ResolvedKind: "memory", SourceMemory: &MemoryCurationVersionReference{MemoryID: "mem_gone", Version: 1}}

	// An empty reason marks a defensive check that acceptance makes
	// unreachable through PlanCuration; it stays an unclassified conflict.
	tests := []struct {
		name     string
		actions  []MemoryCurationPlanAction
		reason   string
		ordinal  int64
		evidence int
	}{
		{"create without payload", plan(MemoryCurationPlanAction{Operation: MemoryCurationOperationCreate}), "", 0, -1},
		{"replace without payload", plan(MemoryCurationPlanAction{Operation: MemoryCurationOperationReplace}), "", 0, -1},
		{"supersede without payload", plan(MemoryCurationPlanAction{Operation: MemoryCurationOperationSupersede}), "", 0, -1},
		{"relate without payload", plan(MemoryCurationPlanAction{Operation: MemoryCurationOperationRelate}), "", 0, -1},
		{"propose_fact without payload", plan(MemoryCurationPlanAction{Operation: MemoryCurationOperationProposeFact}), "", 0, -1},
		{"unknown operation", plan(MemoryCurationPlanAction{Operation: "merge"}), "", 0, -1},
		{"public create with a sensitive relation", plan(withSensitiveRelation), "sensitive_source_requires_sensitive_output", 2, -1},
		{"public replace of a sensitive target", plan(replace("mem_private")), "sensitive_source_requires_sensitive_output", 2, -1},
		{"public fact from sensitive evidence", plan(publicFact), "sensitive_source_requires_sensitive_output", 2, -1},
		{"public replacement of a sensitive memory", plan(supersede("mem_private", version("mem_created", 1))), "sensitive_target_requires_sensitive_replacement", 2, -1},
		{"second replace of one memory", plan(replace("mem_public"), replace("mem_public")), "target_already_mutated", 3, -1},
		{"supersede of a memory replaced earlier", plan(replace("mem_public"), supersede("mem_public", version("mem_created", 1))), "target_already_mutated", 3, -1},
		{"replacement replaced earlier", plan(replace("mem_public"), supersede("mem_private", version("mem_public", 1))), "target_already_mutated", 3, -1},
		{"memory reference without id", plan(relate(version("", 1))), "", 0, -1},
		{"created memory at version 2", plan(relate(version("mem_created", 2))), "invalid_create_output_reference", 2, -1},
		{"memory version not frozen", plan(relate(version("mem_public", 7))), "memory_version_not_in_inputs", 2, -1},
		{"forgotten memory", plan(relate(version("mem_gone", 1))), "memory_not_live", 2, -1},
		{"stale replace target", plan(replace("mem_old")), "memory_version_not_current", 2, -1},
		{"unknown input evidence id", plan(create(direct(1, 1), unknownInput)), "evidence_not_in_inputs", 2, 1},
		{"input evidence that differs from its row", plan(create(mismatched)), "evidence_row_mismatch", 2, 0},
		{"input memory evidence without a source", plan(create(memoryCurationEvidenceFromInputRow(memoryRow))), "", 0, -1},
		{"input evidence of an unsupported kind", plan(create(memoryCurationEvidenceFromInputRow(oddRow))), "", 0, -1},
		{"pending direct evidence", plan(create(MemoryCurationEvidence{Type: "conversation", ResolutionState: MemoryEvidencePending})), "direct_evidence_not_resolved", 2, 0},
		{"transcript range outside frozen inputs", plan(create(direct(1, 1), direct(2, 4))), "transcript_range_not_covered", 2, 1},
		{"direct memory evidence without a source", plan(create(MemoryCurationEvidence{Type: "memory", ResolutionState: MemoryEvidenceResolved, ResolvedKind: "memory"})), "", 0, -1},
		{"direct message evidence", plan(create(MemoryCurationEvidence{Type: "conversation", ResolutionState: MemoryEvidenceResolved, ResolvedKind: "message", SourceMessageID: "message-1"})), "direct_evidence_requires_input_row", 2, 0},
		{"memory evidence citing a forgotten memory", plan(create(direct(1, 1), forgottenSource)), "memory_not_live", 2, 1},
	}
	seen := map[string]bool{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := authorizeMemoryCurationPlan(test.actions, auth)
			requireMemoryCurationConflictReason(t, test.name, err, test.reason, test.ordinal, test.evidence)
		})
		if test.reason != "" {
			seen[test.reason] = true
		}
	}
	for _, reason := range []string{
		"sensitive_source_requires_sensitive_output",
		"sensitive_target_requires_sensitive_replacement", "target_already_mutated",
		"invalid_create_output_reference", "memory_version_not_in_inputs",
		"memory_not_live", "memory_version_not_current",
		"evidence_not_in_inputs", "evidence_row_mismatch",
		"direct_evidence_not_resolved", "transcript_range_not_covered",
		"direct_evidence_requires_input_row",
	} {
		if !seen[reason] {
			t.Errorf("no row covers reason %q", reason)
		}
		delete(seen, reason)
	}
	if len(seen) != 0 {
		t.Errorf("rows name %d reasons outside the plan-authorization vocabulary", len(seen))
	}

	// An internal invariant with no client-facing rule stays unclassified.
	_, err := authorizeMemoryCurationPlan(nil, nil)
	requireMemoryCurationConflictReason(t, "nil authorization", err, "", 0, -1)
}
