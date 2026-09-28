package store

import (
	"strings"
	"testing"
)

// A planner receipt counts transcript coverage inputs as transcript inputs.
// The archive validator must reconcile materialized inputs the same way, or
// every account whose runs fast-forwarded a transcript stream fails restore.
func TestValidateImportedMemoryCurationGraphCountsCoverageAsTranscript(t *testing.T) {
	owner := memoryOwnerImportKey{realmID: "rlm_1", ownerKind: "agent", ownerID: "agt_1"}
	newContext := func(observed map[string]int64) *importCtx {
		ic := newImportCtx("acc_1")
		ic.memoryCurationLanes[owner] = memoryCurationLaneImportScope{
			owner: owner, requestGeneration: 1, fencingGeneration: 1,
		}
		ic.memoryCurationRequests["mcrq_aaaaaaaaaaaaaaaa"] = memoryCurationRequestImportScope{
			owner: owner, requestGeneration: 1, state: "fulfilled",
		}
		var total int64
		for _, n := range observed {
			total += n
		}
		ic.memoryCurationRuns["mrun_aaaaaaaaaaaaaaaa"] = memoryCurationRunImportScope{
			owner: owner, requestID: "mcrq_aaaaaaaaaaaaaaaa", requestGeneration: 1,
			fencingGeneration: 1, state: "abandoned",
			inputCount: 4, memoryInputCount: 1, evidenceInputCount: 0,
			transcriptInputCount: 2, cursorInputCount: 1,
			observedInputs: total, observedByKind: observed, validated: true,
		}
		return ic
	}
	if err := validateImportedMemoryCurationGraph(newContext(map[string]int64{
		MemoryCurationInputMemory: 1, MemoryCurationInputTranscript: 1,
		MemoryCurationInputTranscriptCoverage: 1, MemoryCurationInputCursor: 1,
	})); err != nil {
		t.Fatalf("coverage input must count toward the transcript receipt: %v", err)
	}
	err := validateImportedMemoryCurationGraph(newContext(map[string]int64{
		MemoryCurationInputMemory: 1, MemoryCurationInputTranscript: 1,
		MemoryCurationInputCursor: 1,
	}))
	if err == nil || !strings.Contains(err.Error(), "materialized input counts do not match its receipt") {
		t.Fatalf("a missing input must still be detected: %v", err)
	}
}
