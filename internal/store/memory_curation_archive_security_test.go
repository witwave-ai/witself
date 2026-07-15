package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestImportedMemoryCurationRequestRequiresCanonicalMaterializingScope(t *testing.T) {
	canonical, err := normalizeMemoryCurationScope(MemoryCurationScope{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	var scope map[string]any
	if err := json.Unmarshal(encoded, &scope); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"scope": scope, "coalescing_key": "owner", "trigger_reason": "manual",
		"state": "queued", "attempt_count": float64(0), "max_attempts": float64(5),
		"read_only_replay": false, "priority": float64(0),
		"due_at": "2026-07-14T07:00:00Z", "created_at": "2026-07-14T07:00:00Z",
		"updated_at": "2026-07-14T07:00:00Z", "idempotency_key": "manual-1",
	}
	if err := validateImportedCurationRequestContent(request); err != nil {
		t.Fatalf("canonical scope rejected: %v", err)
	}

	request["scope"] = map[string]any{}
	if err := validateImportedCurationRequestContent(request); err == nil ||
		!strings.Contains(err.Error(), "scope is not canonical") {
		t.Fatalf("empty scope error = %v", err)
	}

	request["scope"] = scope
	request["coalescing_key"] = automaticMemoryCurationCoalescingKey
	request["idempotency_key"] = "automatic:memory:" + strings.Repeat("a", 64)
	if err := validateImportedCurationRequestContent(request); err != nil {
		t.Fatalf("genuine automatic request shape rejected: %v", err)
	}
	request["scope"] = map[string]any{
		"sources": []any{"memory"}, "memory_states": []any{"active"},
		"include_sensitive": false, "max_memories": float64(defaultMemoryCurationMemories),
		"max_evidence":           float64(defaultMemoryCurationEvidence),
		"max_transcript_entries": float64(defaultMemoryCurationTranscriptItems),
	}
	if err := validateImportedCurationRequestContent(request); err == nil ||
		!strings.Contains(err.Error(), "reserved automatic request shape") {
		t.Fatalf("narrow automatic scope error = %v", err)
	}
}

func TestImportedMemoryCurationAppliedResultCannotClaimUnrelatedOwnerMemory(t *testing.T) {
	owner := memoryOwnerImportKey{realmID: "realm_1", ownerKind: "agent", ownerID: "agent_1"}
	ic := newImportCtx("acc_1")
	ic.memories[memoryArchiveOneID] = memoryImportScope{owner: owner, currentVersion: 1}
	ic.memories[memoryArchiveLiveID] = memoryImportScope{owner: owner, currentVersion: 2}
	ic.memoryVersions[memoryVersionImportKey{memoryID: memoryArchiveOneID, version: 1}] =
		memoryVersionImportScope{owner: owner}
	ic.memoryVersions[memoryVersionImportKey{memoryID: memoryArchiveLiveID, version: 2}] =
		memoryVersionImportScope{owner: owner}

	actionID := "mact_aaaaaaaaaaaaaaaa"
	action := memoryCurationActionImportScope{
		id: actionID, owner: owner, runID: "mrun_aaaaaaaaaaaaaaaa", ordinal: 1,
		primitive: MemoryCurationOperationReplace,
		action: MemoryCurationPlanAction{
			Ordinal: 1, Operation: MemoryCurationOperationReplace,
			Replace: &MemoryCurationReplaceAction{
				Target: MemoryCurationTargetReference{MemoryID: memoryArchiveOneID, ExpectedVersion: 1},
			},
		},
	}
	action.appliedResult = &MemoryCurationActionApplyResult{
		ActionID: actionID, Ordinal: 1, Operation: MemoryCurationOperationReplace,
		BeforeHeads: []MemoryVersionReference{{MemoryID: memoryArchiveOneID, Version: 1}},
		AfterHeads:  []MemoryVersionReference{{MemoryID: memoryArchiveLiveID, Version: 2}},
	}
	if err := validateImportedMemoryCurationActionResources(ic, action); err == nil ||
		!strings.Contains(err.Error(), "lacks exact action attribution") {
		t.Fatalf("unrelated applied result error = %v", err)
	}
}

func TestImportedMemoryCurationRollbackPreservesIndependentFactRejection(t *testing.T) {
	owner := memoryOwnerImportKey{realmID: "realm_1", ownerKind: "agent", ownerID: "agent_1"}
	runID := "mrun_aaaaaaaaaaaaaaaa"
	actionID := "mact_aaaaaaaaaaaaaaaa"
	candidateID := "fc_aaaaaaaaaaaaaaaa"
	ic := newImportCtx("acc_1")
	ic.factCandidateCurations[candidateID] = factCandidateCurationImportScope{
		owner: owner, runID: runID, actionID: actionID, status: "rejected",
	}
	action := memoryCurationActionImportScope{
		id: actionID, owner: owner, runID: runID, ordinal: 1,
		primitive: MemoryCurationOperationProposeFact,
		action: MemoryCurationPlanAction{
			Ordinal: 1, Operation: MemoryCurationOperationProposeFact,
			ProposeFact: &MemoryCurationProposeFactAction{},
		},
		appliedResult: &MemoryCurationActionApplyResult{
			ActionID: actionID, Ordinal: 1, Operation: MemoryCurationOperationProposeFact,
			CandidateIDs: []string{candidateID},
		},
		rollbackResult: &MemoryCurationActionRollbackResult{
			ActionID: actionID, Ordinal: 1, Operation: MemoryCurationOperationProposeFact,
			CompensationHeads: []MemoryVersionReference{}, RevertedRelationIDs: []string{},
			WithdrawnCandidateIDs: []string{},
		},
	}
	if err := validateImportedMemoryCurationActionResources(ic, action); err != nil {
		t.Fatalf("independently rejected candidate was not portable: %v", err)
	}
	action.rollbackResult.WithdrawnCandidateIDs = []string{candidateID}
	if err := validateImportedMemoryCurationActionResources(ic, action); err == nil ||
		!strings.Contains(err.Error(), "rollback resources do not match") {
		t.Fatalf("false withdrawn attribution error = %v", err)
	}
}

func TestImportedMemoryCurationGraphRejectsIneligibleReplayAndCrossOwnerRollback(t *testing.T) {
	ownerOne := memoryOwnerImportKey{realmID: "realm_1", ownerKind: "agent", ownerID: "agent_1"}
	ownerTwo := memoryOwnerImportKey{realmID: "realm_1", ownerKind: "agent", ownerID: "agent_2"}
	runID := "mrun_aaaaaaaaaaaaaaaa"
	requestID := "mcrq_aaaaaaaaaaaaaaaa"
	actionID := "mact_aaaaaaaaaaaaaaaa"
	action := MemoryCurationPlanAction{
		Ordinal: 1, Operation: MemoryCurationOperationRelate,
		Relate: &MemoryCurationRelateAction{
			RelationType: MemoryCurationRelationDerivedFrom,
			From:         MemoryCurationVersionReference{MemoryID: memoryArchiveOneID, Version: 1},
			To:           MemoryCurationVersionReference{MemoryID: memoryArchiveLiveID, Version: 1},
		},
	}
	canonical, err := canonicalMemoryCurationJSON(MemoryCurationPlan{
		Schema: MemoryCurationPlanSchemaV1, PlanRevision: 1,
		Actions: []MemoryCurationPlanAction{action},
	})
	if err != nil {
		t.Fatal(err)
	}
	planSum := sha256.Sum256(canonical)

	newContext := func() *importCtx {
		ic := newImportCtx("acc_1")
		ic.memoryCurationLanes[ownerOne] = memoryCurationLaneImportScope{
			owner: ownerOne, requestGeneration: 1, fencingGeneration: 1,
		}
		ic.memoryCurationRequests[requestID] = memoryCurationRequestImportScope{
			owner: ownerOne, requestGeneration: 1, state: "fulfilled",
		}
		ic.memoryCurationRuns[runID] = memoryCurationRunImportScope{
			owner: ownerOne, requestID: requestID, requestGeneration: 1,
			fencingGeneration: 1, state: "rolled_back", planSchema: MemoryCurationPlanSchemaV1,
			planRevision: 1, planHash: hex.EncodeToString(planSum[:]),
			observedByKind: map[string]int64{}, observedActions: 1,
		}
		ic.memoryCurationActions[actionID] = memoryCurationActionImportScope{
			id: actionID, owner: ownerOne, runID: runID, ordinal: 1,
			planRevision: 1, state: "reverted", primitive: MemoryCurationOperationRelate,
			action: action,
		}
		return ic
	}

	ic := newContext()
	ic.memoryCurationRuns[runID] = func(run memoryCurationRunImportScope) memoryCurationRunImportScope {
		run.state = "applied"
		return run
	}(ic.memoryCurationRuns[runID])
	ic.memoryCurationRequests["mcrq_bbbbbbbbbbbbbbbb"] = memoryCurationRequestImportScope{
		owner: ownerOne, requestGeneration: 1, state: "queued", replayRunID: runID,
	}
	if err := validateImportedMemoryCurationGraph(ic); err == nil ||
		!strings.Contains(err.Error(), "replay run") {
		t.Fatalf("ineligible replay error = %v", err)
	}

	ic = newContext()
	ic.memoryRelationCurations = append(ic.memoryRelationCurations, memoryRelationCurationImportScope{
		id: "mrel_aaaaaaaaaaaaaaaa", owner: ownerTwo,
		revertedByRunID: runID, revertedByActionID: actionID,
	})
	if err := validateImportedMemoryCurationGraph(ic); err == nil ||
		!strings.Contains(err.Error(), "outside its owner scope") {
		t.Fatalf("cross-owner rollback attribution error = %v", err)
	}
}

func TestImportedMemoryCurationIdleFenceRequiresTerminalRun(t *testing.T) {
	owner := memoryOwnerImportKey{realmID: "realm_1", ownerKind: "agent", ownerID: "agent_1"}
	newContext := func(state string) *importCtx {
		ic := newImportCtx("acc_1")
		ic.memoryCurationLanes[owner] = memoryCurationLaneImportScope{
			owner: owner, fencingGeneration: 2,
		}
		ic.memoryCurationRuns["mrun_aaaaaaaaaaaaaaaa"] = memoryCurationRunImportScope{
			owner: owner, fencingGeneration: 1, state: state,
			observedByKind: map[string]int64{},
		}
		return ic
	}
	if err := validateImportedMemoryCurationGraph(newContext("abandoned")); err != nil {
		t.Fatalf("terminal idle fence rejected: %v", err)
	}
	if err := validateImportedMemoryCurationGraph(newContext("applied")); err == nil ||
		!strings.Contains(err.Error(), "idle fence advance") {
		t.Fatalf("unproven idle fence error = %v", err)
	}
}

func TestDecodeImportRowRejectsNestedDuplicateMembers(t *testing.T) {
	_, err := decodeImportRow([]byte(`{"scope":{"sources":["memory"],"sources":["evidence"]}}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate JSON member") {
		t.Fatalf("nested duplicate error = %v", err)
	}
}
