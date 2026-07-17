package runtimeacceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/memoryhydration"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestNewStatePinsFourRuntimeDeliveryAndHistoryGate(t *testing.T) {
	for _, tc := range []struct {
		runtime   string
		automatic bool
		delivery  string
	}{
		{transcriptcapture.RuntimeCodex, true, memoryhydration.DeliveryHookAdditionalContext},
		{transcriptcapture.RuntimeClaudeCode, true, memoryhydration.DeliveryHookAdditionalContext},
		{transcriptcapture.RuntimeCursor, false, memoryhydration.DeliveryGuidedMCPFallback},
		{transcriptcapture.RuntimeGrokBuild, false, memoryhydration.DeliveryGuidedMCPFallback},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			state := testState(t, tc.runtime, true)
			if state.Delivery.RecallAutomatic != tc.automatic || state.Delivery.RecallMode != tc.delivery {
				t.Fatalf("delivery = %#v", state.Delivery)
			}
			history, ok := state.Prompt(StageHistory)
			if !ok || strings.Contains(history.Text, state.Markers.Narrative) {
				t.Fatalf("history prompt leaks answer: %#v", history)
			}
			query, eligible := memoryhydration.FocusedQuery(history.Text)
			if !eligible || query == "" {
				t.Fatalf("history prompt did not enter deterministic recall gate: %q", history.Text)
			}
		})
	}
}

func TestPrivateStateRequiresPrivateRegularFile(t *testing.T) {
	state := testState(t, transcriptcapture.RuntimeCodex, true)
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	if err := WritePrivateState(path, state); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o", got)
	}
	loaded, err := ReadPrivateState(path)
	if err != nil || loaded.RunID != state.RunID {
		t.Fatalf("loaded = %#v / %v", loaded, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateState(path); err == nil || !strings.Contains(err.Error(), "mode-0600") {
		t.Fatalf("public state error = %v", err)
	}
}

func TestEvaluateProducesSanitizedPassingEvidence(t *testing.T) {
	for _, runtimeName := range []string{transcriptcapture.RuntimeCodex, transcriptcapture.RuntimeCursor} {
		t.Run(runtimeName, func(t *testing.T) {
			state := testState(t, runtimeName, true)
			state.Phase = PhaseReady
			state.Fixtures = FixtureState{
				BaselineMemoryID: "mem_baseline", SensitiveFactID: "fact_private",
				PeerMemoryID: "mem_peer", CurationRequestID: "mcrq_test",
			}
			input := passingInput(state)
			input.Transcripts[0].Entries = append(input.Transcripts[0].Entries, TranscriptEntry{
				Sequence: 3, Role: "system", Runtime: state.Runtime,
				RuntimeVersion: "unexpected-private-version-text", CreatedAt: state.PreparedAt.Add(2 * time.Minute),
			})
			evidence, err := Evaluate(state, input)
			if err != nil {
				t.Fatal(err)
			}
			if evidence.Status != "pass" || len(evidence.Cases) != 7 {
				t.Fatalf("evidence = %#v", evidence)
			}
			for _, item := range evidence.Cases {
				if !item.Passed {
					t.Errorf("case failed: %#v", item)
				}
				if item.Name == "applied_empty_curation_checkpoint" && len(item.Resources.ReceiptIDs) != 1 {
					t.Error("passing curation evidence omitted its canonical apply receipt")
				}
			}
			raw, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{state.Markers.Narrative, state.Markers.Sensitive, state.Markers.PeerOnly, state.Prompts[0].Text} {
				if strings.Contains(string(raw), private) {
					t.Fatalf("evidence leaked private run material %q", private)
				}
			}
			if strings.Contains(string(raw), "unexpected-private") ||
				len(evidence.Runtime.ObservedVersions) != 1 || evidence.Runtime.ObservedVersions[0] != state.RuntimeVersion {
				t.Fatalf("evidence retained untrusted transcript provenance: %s", raw)
			}
		})
	}
}

func TestEvaluateFailsOnLeakOrSameSession(t *testing.T) {
	state := testState(t, transcriptcapture.RuntimeClaudeCode, true)
	state.Phase = PhaseReady
	state.Fixtures = FixtureState{
		BaselineMemoryID: "mem_baseline", SensitiveFactID: "fact_private",
		PeerMemoryID: "mem_peer", CurationRequestID: "mcrq_test",
	}
	input := passingInput(state)
	// Put capture and history in the same transcript and leak the private value
	// in broad output. Both independent cases must fail.
	input.Transcripts[2].ID = input.Transcripts[1].ID
	input.Transcripts[3].Entries[1].Body = "broad output " + state.Markers.Sensitive
	evidence, err := Evaluate(state, input)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Status != "fail" {
		t.Fatalf("status = %q", evidence.Status)
	}
	failed := map[string]bool{}
	for _, item := range evidence.Cases {
		if !item.Passed {
			failed[item.Name] = true
		}
	}
	if !failed["sensitive_exact_and_broad_redaction"] || !failed["same_agent_cross_session_continuity"] {
		t.Fatalf("failed cases = %v", failed)
	}
}

func TestEvaluateRequiresCryptographicallyVerifiedEmptyPlan(t *testing.T) {
	state := testState(t, transcriptcapture.RuntimeCodex, true)
	state.Phase = PhaseReady
	state.Fixtures = FixtureState{
		BaselineMemoryID: "mem_baseline", SensitiveFactID: "fact_private",
		PeerMemoryID: "mem_peer", CurationRequestID: "mcrq_test",
	}
	input := passingInput(state)
	input.Backend.CurationPlanEmptyVerified = false
	evidence, err := Evaluate(state, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range evidence.Cases {
		if item.Name == "applied_empty_curation_checkpoint" {
			if item.Passed || item.Resources.ActionCount != nil {
				t.Fatalf("curation evidence = %#v", item)
			}
			return
		}
	}
	t.Fatal("missing applied_empty_curation_checkpoint case")
}

func TestEvaluateRequiresExactPromptAndPinnedVersionInEveryStage(t *testing.T) {
	state := testState(t, transcriptcapture.RuntimeClaudeCode, true)
	state.Phase = PhaseReady
	state.Fixtures = FixtureState{
		BaselineMemoryID: "mem_baseline", SensitiveFactID: "fact_private",
		PeerMemoryID: "mem_peer", CurationRequestID: "mcrq_test",
	}

	for _, mutate := range []func(*VerificationInput){
		func(input *VerificationInput) {
			input.Transcripts[0].Entries[0].Body = state.Prompts[0].Marker
		},
		func(input *VerificationInput) {
			input.Transcripts[len(input.Transcripts)-1].Entries[0].RuntimeVersion = "9.9.9"
		},
		func(input *VerificationInput) {
			input.Transcripts[0].Entries[1].RuntimeVersion = ""
			input.Transcripts[0].Entries = append(input.Transcripts[0].Entries, TranscriptEntry{
				Sequence: 3, Role: "system", Runtime: state.Runtime,
				RuntimeVersion: state.RuntimeVersion, CreatedAt: state.PreparedAt.Add(2 * time.Minute),
			})
		},
	} {
		input := passingInput(state)
		mutate(&input)
		evidence, err := Evaluate(state, input)
		if err != nil {
			t.Fatal(err)
		}
		if evidence.Status != "fail" {
			t.Fatalf("status = %q", evidence.Status)
		}
		identityPassed := false
		for _, item := range evidence.Cases {
			if item.Name == "identity_binding" {
				identityPassed = item.Passed
			}
		}
		if identityPassed {
			t.Fatal("identity_binding passed without exact prompt and per-stage version provenance")
		}
	}
}

func TestEvaluateKeepsARealUserTurnBoundaryWhileAllowingInternalEvents(t *testing.T) {
	state := testState(t, transcriptcapture.RuntimeCodex, true)
	state.Phase = PhaseReady
	state.Fixtures = FixtureState{
		BaselineMemoryID: "mem_baseline", SensitiveFactID: "fact_private",
		PeerMemoryID: "mem_peer", CurationRequestID: "mcrq_test",
	}

	for _, tc := range []struct {
		name, role, wantStatus string
	}{
		{"internal system event", "system", "pass"},
		{"later user turn", "user", "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := passingInput(state)
			assistant := input.Transcripts[0].Entries[1]
			assistant.Sequence = 3
			input.Transcripts[0].Entries = []TranscriptEntry{
				input.Transcripts[0].Entries[0],
				{
					Sequence: 2, Role: tc.role, Body: "non-user runtime event",
					Runtime: state.Runtime, RuntimeVersion: state.RuntimeVersion,
					CreatedAt: state.PreparedAt.Add(30 * time.Second),
				},
				assistant,
			}
			evidence, err := Evaluate(state, input)
			if err != nil {
				t.Fatal(err)
			}
			if evidence.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", evidence.Status, tc.wantStatus)
			}
		})
	}
}

func TestEvaluateRecognizesStrictLegacyCursorPromptEnvelope(t *testing.T) {
	state := testState(t, transcriptcapture.RuntimeCursor, true)
	state.Phase = PhaseReady
	state.Fixtures = FixtureState{
		BaselineMemoryID: "mem_baseline", SensitiveFactID: "fact_private",
		PeerMemoryID: "mem_peer", CurationRequestID: "mcrq_test",
	}
	input := passingInput(state)
	for index := range input.Transcripts {
		body := input.Transcripts[index].Entries[0].Body
		input.Transcripts[index].Entries[0].Body = "<timestamp>2026-07-17T07:08:09.123456-06:00</timestamp>\n<user_query>\n" + body + "\n</user_query>"
	}
	evidence, err := Evaluate(state, input)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Status != "pass" {
		t.Fatalf("strict legacy Cursor envelopes caused false failure: %#v", evidence.Cases)
	}
}

func TestEvaluateCountsCanonicalAndWrappedCursorPromptAsDuplicates(t *testing.T) {
	state := testState(t, transcriptcapture.RuntimeCursor, true)
	state.Phase = PhaseReady
	state.Fixtures = FixtureState{
		BaselineMemoryID: "mem_baseline", SensitiveFactID: "fact_private",
		PeerMemoryID: "mem_peer", CurationRequestID: "mcrq_test",
	}
	input := passingInput(state)
	duplicate := input.Transcripts[0]
	duplicate.ID = "trn_duplicate"
	duplicate.Entries = append([]TranscriptEntry(nil), duplicate.Entries...)
	duplicate.Entries[0].Body = "<timestamp>2026-07-17T07:08:09.123456-06:00</timestamp>\n<user_query>\n" + duplicate.Entries[0].Body + "\n</user_query>"
	input.Transcripts = append(input.Transcripts, duplicate)

	evidence, err := Evaluate(state, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range evidence.Cases {
		if item.Name == "identity_binding" {
			if item.Passed {
				t.Fatal("identity binding passed after one prompt matched twice post-normalization")
			}
			return
		}
	}
	t.Fatal("missing identity_binding case")
}

func TestEvaluateRequiresSixDistinctTranscripts(t *testing.T) {
	state := testState(t, transcriptcapture.RuntimeCursor, true)
	state.Phase = PhaseReady
	state.Fixtures = FixtureState{
		BaselineMemoryID: "mem_baseline", SensitiveFactID: "fact_private",
		PeerMemoryID: "mem_peer", CurationRequestID: "mcrq_test",
	}
	input := passingInput(state)
	input.Transcripts[len(input.Transcripts)-1].ID = input.Transcripts[0].ID
	evidence, err := Evaluate(state, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range evidence.Cases {
		if item.Name == "identity_binding" {
			if item.Passed {
				t.Fatal("identity binding passed with a reused transcript id")
			}
			return
		}
	}
	t.Fatal("missing identity_binding case")
}

func TestEvaluateIdentityFailureDetailDoesNotClaimSuccess(t *testing.T) {
	state := testState(t, transcriptcapture.RuntimeClaudeCode, true)
	state.Phase = PhaseReady
	state.Fixtures = FixtureState{
		BaselineMemoryID: "mem_baseline", SensitiveFactID: "fact_private",
		PeerMemoryID: "mem_peer", CurationRequestID: "mcrq_test",
	}
	input := passingInput(state)
	input.Backend.InstructionsCurrent = false
	evidence, err := Evaluate(state, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range evidence.Cases {
		if item.Name != "identity_binding" {
			continue
		}
		if item.Passed {
			t.Fatal("identity binding passed with stale managed instructions")
		}
		lower := strings.ToLower(item.Detail)
		if !strings.Contains(lower, "evaluates") || strings.Contains(lower, "matched") || strings.Contains(lower, "succeeded") {
			t.Fatalf("identity failure retained a success-sounding detail: %q", item.Detail)
		}
		return
	}
	t.Fatal("missing identity_binding case")
}

func TestEvaluateRequiresCanonicalApplyReceipt(t *testing.T) {
	state := testState(t, transcriptcapture.RuntimeCodex, true)
	state.Phase = PhaseReady
	state.Fixtures = FixtureState{
		BaselineMemoryID: "mem_baseline", SensitiveFactID: "fact_private",
		PeerMemoryID: "mem_peer", CurationRequestID: "mcrq_test",
	}
	input := passingInput(state)
	input.Backend.CurationApplyReceiptID = ""
	evidence, err := Evaluate(state, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range evidence.Cases {
		if item.Name == "applied_empty_curation_checkpoint" {
			if item.Passed || len(item.Resources.ReceiptIDs) != 0 {
				t.Fatalf("curation evidence = %#v", item)
			}
			return
		}
	}
	t.Fatal("missing applied_empty_curation_checkpoint case")
}

func TestEvaluateToleratesDelayedTranscriptFlush(t *testing.T) {
	state := testState(t, transcriptcapture.RuntimeGrokBuild, true)
	state.Phase = PhaseReady
	state.Fixtures = FixtureState{
		BaselineMemoryID: "mem_baseline", SensitiveFactID: "fact_private",
		PeerMemoryID: "mem_peer", CurationRequestID: "mcrq_test",
	}
	input := passingInput(state)
	for transcriptIndex := range input.Transcripts {
		for entryIndex := range input.Transcripts[transcriptIndex].Entries {
			input.Transcripts[transcriptIndex].Entries[entryIndex].CreatedAt = state.PreparedAt.Add(
				2*time.Hour + time.Duration(transcriptIndex)*10*time.Minute + time.Duration(entryIndex)*time.Minute,
			)
		}
	}
	input.VerifiedAt = state.PreparedAt.Add(4 * time.Hour)
	evidence, err := Evaluate(state, input)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Status != "pass" {
		t.Fatalf("asynchronously flushed transcripts caused false failure: %#v", evidence.Cases)
	}
}

func testState(t *testing.T, runtimeName string, eligible bool) RunState {
	t.Helper()
	identity := Identity{AccountID: "acc_test", RealmID: "rlm_test", RealmName: "default", AgentID: "agt_subject", AgentName: "subject-test"}
	peer := Identity{AccountID: "acc_test", RealmID: "rlm_test", RealmName: "default", AgentID: "agt_peer", AgentName: "peer-test"}
	state, err := NewState(
		"mra_test", runtimeName, "1.2.3", "1.2.3",
		Build{Version: "0.0.172", Commit: "abcdef1", Date: "2026-07-16T00:00:00Z"},
		identity, peer, "default", "", eligible, nil, nil, time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func passingInput(state RunState) VerificationInput {
	responses := map[string]string{
		StageIdentity:  strings.Join([]string{state.Identity.AccountID, state.Identity.RealmID, state.Identity.AgentID, state.Identity.AgentName}, " "),
		StageCapture:   "durable",
		StageHistory:   "The codename was " + state.Markers.Narrative + " because it proves portable continuity.",
		StageBroad:     "Sensitive values are redacted.",
		StageSensitive: state.Markers.Sensitive,
		StageIsolation: "No matching memory exists in my own scope.",
	}
	transcripts := make([]TranscriptObservation, 0, len(state.Prompts))
	for index, prompt := range state.Prompts {
		stageStart := state.PreparedAt.Add(time.Duration(index) * 10 * time.Minute)
		transcripts = append(transcripts, TranscriptObservation{
			ID: "trn_" + string(rune('a'+index)), Runtime: state.Runtime,
			Entries: []TranscriptEntry{
				{Sequence: 1, Role: "user", Body: prompt.Text, Runtime: state.Runtime, RuntimeVersion: state.RuntimeVersion, CreatedAt: stageStart},
				{Sequence: 2, Role: "assistant", Body: responses[prompt.Stage], Runtime: state.Runtime, RuntimeVersion: state.RuntimeVersion, CreatedAt: stageStart.Add(time.Minute)},
			},
		})
	}
	return VerificationInput{
		VerifiedAt: time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC),
		Backend: BackendObservation{
			BindingCurrent: true, RuntimeVersionCurrent: true, HooksCurrent: true,
			InstructionsCurrent: true, ServerBuildCurrent: true, HarnessBuildCurrent: true,
			ExplicitMemoryIDs: []string{"mem_explicit"},
			CurationRunID:     "mrun_test", CurationApplied: true,
			CurationApplyReceiptID:    "mrec_apply",
			CurationPlanEmptyVerified: true, CurationActionCount: 0,
			SensitiveFactExact: true, SensitiveFactBroadRedacted: true,
			PeerCanRecallPeerFixture: true, SubjectCanRecallPeerFixture: false,
		},
		Transcripts: transcripts,
	}
}
