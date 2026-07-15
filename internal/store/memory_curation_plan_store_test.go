package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestHashMemoryCurationPlanRequestUsesCanonicalDecodedDraft(t *testing.T) {
	rawA := []byte(`{
		"schema":"witself.memory-plan.v1",
		"draft_revision":7,
		"actions":[{"ordinal":1,"operation":"propose_fact","propose_fact":{
			"predicate":"profile/example","value_type":"object","value":{"z":1,"a":"x"},
			"evidence":[{"type":"conversation","resolution_state":"unavailable","terminal_reason_code":"not_recorded"}]
		}}]}`)
	rawB := []byte(`{"actions":[{"propose_fact":{"evidence":[{"terminal_reason_code":"not_recorded","resolution_state":"unavailable","type":"conversation"}],"value": { "a":"x", "z":1 },"value_type":"object","predicate":"profile/example"},"operation":"propose_fact","ordinal":1}],"draft_revision":7,"schema":"witself.memory-plan.v1"}`)
	draftA, err := DecodeMemoryCurationPlanDraft(rawA)
	if err != nil {
		t.Fatal(err)
	}
	draftB, err := DecodeMemoryCurationPlanDraft(rawB)
	if err != nil {
		t.Fatal(err)
	}
	hashA, err := hashMemoryCurationPlanRequest("mrun_aaaaaaaaaaaaaaaa", 3, draftA)
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := hashMemoryCurationPlanRequest("mrun_aaaaaaaaaaaaaaaa", 3, draftB)
	if err != nil {
		t.Fatal(err)
	}
	if hashA != hashB {
		t.Fatalf("semantic JSON retry hashes differ: %s != %s", hashA, hashB)
	}
	draftB.DraftRevision++
	changed, err := hashMemoryCurationPlanRequest("mrun_aaaaaaaaaaaaaaaa", 3, draftB)
	if err != nil {
		t.Fatal(err)
	}
	if changed == hashA {
		t.Fatal("draft revision was not bound into the request hash")
	}
}

func TestAuthorizeMemoryCurationPlanCoversAllPrimitives(t *testing.T) {
	transcriptEvidence := func() MemoryCurationEvidence {
		return MemoryCurationEvidence{
			Type: "conversation", Role: MemoryEvidenceSupports,
			ResolutionState: MemoryEvidenceResolved, ResolvedKind: "transcript",
			SourceTranscriptID: "trn_authorized", SourceSequenceFrom: 2, SourceSequenceUntil: 3,
		}
	}
	actions := []MemoryCurationPlanAction{
		{Ordinal: 1, Operation: MemoryCurationOperationCreate, Create: &MemoryCurationCreateAction{
			LocalRef: "summary", MemoryID: "mem_summary",
			Snapshot: MemoryCurationMemorySnapshot{Content: "summary", Sensitive: true,
				Evidence: []MemoryCurationEvidence{transcriptEvidence()}},
			Relations: []MemoryCurationLineageRelation{{RelationType: MemoryCurationRelationDerivedFrom,
				To: MemoryCurationVersionReference{MemoryID: "mem_source", Version: 1}}},
		}},
		{Ordinal: 2, Operation: MemoryCurationOperationReplace, Replace: &MemoryCurationReplaceAction{
			Target:   MemoryCurationTargetReference{MemoryID: "mem_target", ExpectedVersion: 1},
			Snapshot: MemoryCurationMemorySnapshot{Content: "revised", Evidence: []MemoryCurationEvidence{transcriptEvidence()}},
		}},
		{Ordinal: 3, Operation: MemoryCurationOperationSupersede, Supersede: &MemoryCurationSupersedeAction{
			Target:       MemoryCurationTargetReference{MemoryID: "mem_source", ExpectedVersion: 1},
			Replacements: []MemoryCurationVersionReference{{MemoryID: "mem_summary", Version: 1}},
		}},
		{Ordinal: 4, Operation: MemoryCurationOperationRelate, Relate: &MemoryCurationRelateAction{
			RelationType: MemoryCurationRelationSummarizes,
			From:         MemoryCurationVersionReference{MemoryID: "mem_summary", Version: 1},
			To:           MemoryCurationVersionReference{MemoryID: "mem_target", Version: 1},
		}},
		{Ordinal: 5, Operation: MemoryCurationOperationProposeFact, ProposeFact: &MemoryCurationProposeFactAction{
			Predicate: "profile/example", ValueType: "string", Value: json.RawMessage(`"value"`), Sensitive: true,
			Evidence: []MemoryCurationEvidence{{Type: "memory", Role: MemoryEvidenceSupports,
				ResolutionState: MemoryEvidenceResolved, ResolvedKind: "memory",
				SourceMemory: &MemoryCurationVersionReference{MemoryID: "mem_summary", Version: 1}}},
		}},
	}
	auth := &memoryCurationPlanAuthorization{
		memories: map[string]memoryCurationPlanMemoryInput{
			memoryCurationPlanVersionKey("mem_source", 1): activeCurationPlanMemory("mem_source", true),
			memoryCurationPlanVersionKey("mem_target", 1): activeCurationPlanMemory("mem_target", false),
		},
		evidence: map[string]memoryCurationPlanEvidenceInput{},
		transcripts: []memoryCurationPlanTranscriptInput{
			{TranscriptID: "trn_authorized", From: 1, Until: 2},
			{TranscriptID: "trn_authorized", From: 3, Until: 5},
		},
		outputs: map[string]memoryCurationPlanOutput{
			"mem_summary": {Ordinal: 1, Sensitive: true},
		},
	}
	rows, err := authorizeMemoryCurationPlan(actions, auth)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("authorized rows = %d, want 5", len(rows))
	}
	for index, row := range rows {
		if len(row.ActionHash) != 64 || row.Action.Ordinal != int64(index+1) {
			t.Fatalf("row %d = %#v", index, row)
		}
	}
	if got, want := rows[2].ExpectedHeads, []MemoryCurationExpectedHead{
		{MemoryID: "mem_source", ExpectedVersion: 1},
		{MemoryID: "mem_summary", ExpectedVersion: 1},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("supersede heads = %#v, want %#v", got, want)
	}

	duplicate := append([]MemoryCurationPlanAction(nil), actions...)
	duplicate = append(duplicate, MemoryCurationPlanAction{
		Ordinal: 6, Operation: MemoryCurationOperationReplace,
		Replace: &MemoryCurationReplaceAction{
			Target: MemoryCurationTargetReference{MemoryID: "mem_target", ExpectedVersion: 1},
		},
	})
	if _, err := authorizeMemoryCurationPlan(duplicate, auth); !errors.Is(err, ErrMemoryCurationConflict) {
		t.Fatalf("duplicate mutable target error = %v", err)
	}

	impossible := append([]MemoryCurationPlanAction(nil), actions...)
	impossible[2] = MemoryCurationPlanAction{Ordinal: 3, Operation: MemoryCurationOperationSupersede,
		Supersede: &MemoryCurationSupersedeAction{
			Target:       MemoryCurationTargetReference{MemoryID: "mem_source", ExpectedVersion: 1},
			Replacements: []MemoryCurationVersionReference{{MemoryID: "mem_target", Version: 1}},
		}}
	authWithoutTaint := *auth
	authWithoutTaint.memories = make(map[string]memoryCurationPlanMemoryInput, len(auth.memories))
	for key, input := range auth.memories {
		input.Sensitive = false
		authWithoutTaint.memories[key] = input
	}
	if _, err := authorizeMemoryCurationPlan(impossible, &authWithoutTaint); !errors.Is(err, ErrMemoryCurationConflict) {
		t.Fatalf("post-mutation expected-head reuse error = %v", err)
	}
}

func TestAuthorizeMemoryCurationEvidenceRequiresOneProvenanceRouteAndPreservesSensitivity(t *testing.T) {
	row := MemoryEvidence{
		ID: "mev_aaaaaaaaaaaaaaaa", Type: "conversation", Role: MemoryEvidenceSupports,
		ResolutionState: MemoryEvidenceResolved, ResolvedKind: "message",
		SourceMessageID: "message-1",
	}
	auth := &memoryCurationPlanAuthorization{
		memories: map[string]memoryCurationPlanMemoryInput{},
		evidence: map[string]memoryCurationPlanEvidenceInput{
			row.ID: {Evidence: row, Sensitive: true},
		},
		outputs: map[string]memoryCurationPlanOutput{"mem_created": {Ordinal: 1, Sensitive: true}},
	}
	action := MemoryCurationPlanAction{
		Ordinal: 1, Operation: MemoryCurationOperationCreate,
		Create: &MemoryCurationCreateAction{LocalRef: "created", MemoryID: "mem_created",
			Snapshot: MemoryCurationMemorySnapshot{Content: "private", Sensitive: true,
				Evidence: []MemoryCurationEvidence{memoryCurationEvidenceFromInputRow(row)}}},
	}
	rows, err := authorizeMemoryCurationPlan([]MemoryCurationPlanAction{action}, auth)
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[0].InputRefs; len(got) != 2 || got[0].Kind != MemoryCurationInputRefEvidence ||
		got[1].Kind != MemoryCurationInputRefMessage || got[1].ViaEvidenceID != row.ID {
		t.Fatalf("authorized message evidence refs = %#v", got)
	}

	public := action
	public.Create = &MemoryCurationCreateAction{
		LocalRef: action.Create.LocalRef, MemoryID: action.Create.MemoryID,
		Snapshot: action.Create.Snapshot,
	}
	public.Create.Snapshot.Sensitive = false
	if _, err := authorizeMemoryCurationPlan([]MemoryCurationPlanAction{public}, auth); !errors.Is(err, ErrMemoryCurationConflict) {
		t.Fatalf("sensitive evidence escape error = %v", err)
	}

	directMessage := action
	directMessage.Create = &MemoryCurationCreateAction{
		LocalRef: action.Create.LocalRef, MemoryID: action.Create.MemoryID,
		Snapshot: action.Create.Snapshot,
	}
	directMessage.Create.Snapshot.Evidence = []MemoryCurationEvidence{{
		Type: "conversation", Role: MemoryEvidenceSupports,
		ResolutionState: MemoryEvidenceResolved, ResolvedKind: "message", SourceMessageID: "message-1",
	}}
	if _, err := authorizeMemoryCurationPlan([]MemoryCurationPlanAction{directMessage}, auth); !errors.Is(err, ErrMemoryCurationConflict) {
		t.Fatalf("direct message route error = %v", err)
	}

	mismatch := action
	mismatch.Create = &MemoryCurationCreateAction{
		LocalRef: action.Create.LocalRef, MemoryID: action.Create.MemoryID,
		Snapshot: action.Create.Snapshot,
	}
	mismatch.Create.Snapshot.Evidence = append([]MemoryCurationEvidence(nil), action.Create.Snapshot.Evidence...)
	mismatch.Create.Snapshot.Evidence[0].SourceMessageID = "message-2"
	if _, err := authorizeMemoryCurationPlan([]MemoryCurationPlanAction{mismatch}, auth); !errors.Is(err, ErrMemoryCurationConflict) {
		t.Fatalf("mismatched input evidence error = %v", err)
	}
}

func TestMemoryCurationTranscriptRangeCoveredRequiresNoGaps(t *testing.T) {
	inputs := []memoryCurationPlanTranscriptInput{
		{TranscriptID: "trn", From: 1, Until: 2},
		{TranscriptID: "trn", From: 3, Until: 4},
		{TranscriptID: "other", From: 1, Until: 20},
	}
	if !memoryCurationTranscriptRangeCovered(inputs, "trn", 2, 4) {
		t.Fatal("contiguous materialized intervals did not cover range")
	}
	inputs[1].From = 4
	if memoryCurationTranscriptRangeCovered(inputs, "trn", 2, 4) {
		t.Fatal("gapped materialized intervals covered range")
	}
}

func activeCurationPlanMemory(memoryID string, sensitive bool) memoryCurationPlanMemoryInput {
	return memoryCurationPlanMemoryInput{
		MemoryID: memoryID, Version: 1, Sensitive: sensitive, State: MemoryStateActive,
		CurrentVersion: sql.NullInt64{Int64: 1, Valid: true},
		CurrentState:   sql.NullString{String: MemoryStateActive, Valid: true},
	}
}
