package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/store"
)

// These handler regressions intentionally use only production types/helpers
// that predate row validation, so they also compile with the old file overlaid.
func TestMCPMemoryCurationEvidenceRejectsInvalidRowsBeforeBackend(t *testing.T) {
	session, backend := newMCPCurationEvidenceSession(t)
	transcript := mcpCurationEvidenceTranscript()
	materialized := transcript
	materialized.InputEvidenceID = "mev_aaaaaaaaaaaaaaaa"
	change := func(row mcpMemoryCurationEvidence, edit func(*mcpMemoryCurationEvidence)) mcpMemoryCurationEvidence {
		edit(&row)
		return row
	}
	// Expected wire contracts are independent of production diagnostic helpers.
	const (
		wantKind         = "resolved_kind must explicitly match source_transcript_id, source_memory, source_message_id, source_import_locator, or artifact_excerpt"
		wantState        = "resolution_state must be resolved, pending, or unavailable"
		wantMaterialized = "input_evidence_id is required unless resolution_state is resolved and resolved_kind is transcript or memory"
		wantTranscript   = "source_transcript_id, source_sequence_from and source_sequence_until require an id and positive ordered range"
		wantResolved     = "resolution_state resolved requires exactly one complete source and no external_locator or terminal_reason_code"
		wantPending      = "resolution_state pending requires only external_locator"
		wantUnavailable  = "resolution_state unavailable requires only terminal_reason_code containing 1-128 bytes"
		wantMemory       = "source_memory requires exactly one memory_id or local_ref and a positive version"
		wantArtifact     = "artifact_excerpt must be base64 encoding at most 65536 bytes"
	)
	tests := []struct {
		name     string
		row      mcpMemoryCurationEvidence
		contract string
	}{
		{"missing kind", change(transcript, func(r *mcpMemoryCurationEvidence) { r.ResolvedKind = "" }), wantKind},
		{"mismatched kind", change(transcript, func(r *mcpMemoryCurationEvidence) { r.ResolvedKind = "memory" }), wantKind},
		{"unknown kind", change(transcript, func(r *mcpMemoryCurationEvidence) { r.ResolvedKind = "CANARY_KIND" }), wantKind},
		{"missing state", change(transcript, func(r *mcpMemoryCurationEvidence) { r.ResolutionState = "" }), wantState},
		{"unknown state", change(transcript, func(r *mcpMemoryCurationEvidence) { r.ResolutionState = "CANARY_STATE" }), wantState},
		{"unresolvable", mcpMemoryCurationEvidence{Type: "conversation", ResolutionState: "unresolvable", TerminalReasonCode: "CANARY_REASON"}, wantState},
		{"direct pending", mcpMemoryCurationEvidence{Type: "conversation", ResolutionState: "pending", ExternalLocator: "CANARY_EXTERNAL"}, wantMaterialized},
		{"direct unavailable", mcpMemoryCurationEvidence{Type: "conversation", ResolutionState: "unavailable", TerminalReasonCode: "CANARY_REASON"}, wantMaterialized},
		{"direct message", mcpMemoryCurationEvidence{Type: "message", ResolutionState: "resolved", ResolvedKind: "message", SourceMessageID: "CANARY_MESSAGE"}, wantMaterialized},
		{"direct import", mcpMemoryCurationEvidence{Type: "import", ResolutionState: "resolved", ResolvedKind: "import_artifact", SourceImportLocator: "CANARY_IMPORT"}, wantMaterialized},
		{"direct artifact", mcpMemoryCurationEvidence{Type: "artifact", ResolutionState: "resolved", ResolvedKind: "artifact", ArtifactExcerpt: base64.StdEncoding.EncodeToString([]byte("CANARY_ARTIFACT"))}, wantMaterialized},
		{"blank evidence id is direct", change(transcript, func(r *mcpMemoryCurationEvidence) {
			r.InputEvidenceID = " \t"
			r.ResolutionState = "pending"
			r.ResolvedKind = ""
			r.SourceTranscriptID = ""
			r.ExternalLocator = "CANARY_EXTERNAL"
		}), wantMaterialized},
		{"missing transcript id", change(transcript, func(r *mcpMemoryCurationEvidence) { r.SourceTranscriptID = " \t" }), wantTranscript},
		{"missing range", change(transcript, func(r *mcpMemoryCurationEvidence) { r.SourceSequenceFrom = 0; r.SourceSequenceUntil = 0 }), wantTranscript},
		{"negative range", change(transcript, func(r *mcpMemoryCurationEvidence) { r.SourceSequenceFrom = -1 }), wantTranscript},
		{"reversed range", change(transcript, func(r *mcpMemoryCurationEvidence) { r.SourceSequenceUntil = 1 }), wantTranscript},
		{"oversize range", change(transcript, func(r *mcpMemoryCurationEvidence) { r.SourceSequenceUntil = r.SourceSequenceFrom + 10001 }), "source_sequence_until must be at most 10000 greater than source_sequence_from"},
		{"multiple complete sources", change(transcript, func(r *mcpMemoryCurationEvidence) {
			r.SourceMemory = &mcpMemoryCurationVersionReference{MemoryID: "mem_CANARY_MEMORY", Version: 1}
		}), wantResolved},
		{"resolved external locator", change(transcript, func(r *mcpMemoryCurationEvidence) { r.ExternalLocator = "CANARY_EXTERNAL" }), wantResolved},
		{"resolved reason", change(transcript, func(r *mcpMemoryCurationEvidence) { r.TerminalReasonCode = "CANARY_REASON" }), wantResolved},
		{"materialized missing kind", change(materialized, func(r *mcpMemoryCurationEvidence) { r.ResolvedKind = "" }), wantKind},
		{"materialized invalid transcript range with message", change(materialized, func(r *mcpMemoryCurationEvidence) { r.SourceSequenceFrom = 0; r.SourceMessageID = "CANARY_MESSAGE" }), wantTranscript},
		{"materialized pending with source", mcpMemoryCurationEvidence{InputEvidenceID: "mev_CANARY_EVIDENCE", Type: "conversation", ResolutionState: "pending", ExternalLocator: "CANARY_EXTERNAL", SourceMessageID: "CANARY_MESSAGE"}, wantPending},
		{"materialized pending missing locator", mcpMemoryCurationEvidence{InputEvidenceID: "mev_CANARY_EVIDENCE", Type: "conversation", ResolutionState: "pending"}, wantPending},
		{"materialized pending with kind", mcpMemoryCurationEvidence{InputEvidenceID: "mev_CANARY_EVIDENCE", Type: "conversation", ResolutionState: "pending", ExternalLocator: "CANARY_EXTERNAL", ResolvedKind: "CANARY_KIND"}, wantPending},
		{"materialized unavailable missing reason", mcpMemoryCurationEvidence{InputEvidenceID: "mev_CANARY_EVIDENCE", Type: "conversation", ResolutionState: "unavailable"}, wantUnavailable},
		{"materialized unavailable with locator", mcpMemoryCurationEvidence{InputEvidenceID: "mev_CANARY_EVIDENCE", Type: "conversation", ResolutionState: "unavailable", TerminalReasonCode: "CANARY_REASON", ExternalLocator: "CANARY_EXTERNAL"}, wantUnavailable},
		{"materialized unavailable long reason", mcpMemoryCurationEvidence{InputEvidenceID: "mev_CANARY_EVIDENCE", Type: "conversation", ResolutionState: "unavailable", TerminalReasonCode: strings.Repeat("x", 129)}, wantUnavailable},
		{"empty memory reference", mcpMemoryCurationEvidence{Type: "memory", ResolutionState: "resolved", ResolvedKind: "memory", SourceMemory: &mcpMemoryCurationVersionReference{Version: 1}}, wantMemory},
		{"unversioned memory", mcpMemoryCurationEvidence{Type: "memory", ResolutionState: "resolved", ResolvedKind: "memory", SourceMemory: &mcpMemoryCurationVersionReference{MemoryID: "mem_CANARY_MEMORY"}}, wantMemory},
		{"ambiguous memory reference", mcpMemoryCurationEvidence{Type: "memory", ResolutionState: "resolved", ResolvedKind: "memory", SourceMemory: &mcpMemoryCurationVersionReference{MemoryID: "mem_CANARY_MEMORY", LocalRef: "CANARY_LOCAL", Version: 1}}, wantMemory},
		{"wrong local version", mcpMemoryCurationEvidence{Type: "memory", ResolutionState: "resolved", ResolvedKind: "memory", SourceMemory: &mcpMemoryCurationVersionReference{LocalRef: "CANARY_LOCAL", Version: 2}}, "source_memory.version must be 1 for local_ref"},
		{"invalid type", change(transcript, func(r *mcpMemoryCurationEvidence) { r.Type = "CANARY_TYPE" }), "type must be a supported evidence label"},
		{"invalid role", change(transcript, func(r *mcpMemoryCurationEvidence) { r.Role = "CANARY_ROLE" }), "role must be supports, contradicts, or context"},
		{"invalid digest", change(transcript, func(r *mcpMemoryCurationEvidence) { r.SourceDigest = "CANARY_DIGEST" }), "source_digest must be a SHA-256 hex digest"},
		{"invalid artifact encoding", change(materialized, func(r *mcpMemoryCurationEvidence) { r.ArtifactExcerpt = "CANARY_BASE64!" }), wantArtifact},
		{"oversize artifact", change(materialized, func(r *mcpMemoryCurationEvidence) {
			r.ArtifactExcerpt = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), 65537))
		}), wantArtifact},
		{"oversize locator", change(materialized, func(r *mcpMemoryCurationEvidence) { r.SourceImportLocator = strings.Repeat("x", 2049) }), "external_locator and source_import_locator must contain at most 2048 bytes each"},
	}
	for _, operation := range []string{"create", "replace", "propose_fact"} {
		for _, tt := range tests {
			t.Run(operation+"/"+tt.name, func(t *testing.T) {
				draft, field := mcpCurationEvidenceDraft(operation, []mcpMemoryCurationEvidence{transcript, tt.row})
				// Exercise trimmed operation dispatch on every negative case.
				draft.Actions[1].Operation = " \t" + operation + "\n"
				before := backend.planCalls
				result := callMCPCurationEvidencePlan(t, session, draft)
				want := fmt.Sprintf("memory_curation_plan_invalid: actions[1].%s[1].%s", field, tt.contract)
				assertMCPCurationEvidenceError(t, result, want)
				if backend.planCalls != before {
					t.Error("invalid evidence called PlanMemoryCuration")
				}
			})
		}
	}
}

func TestMCPMemoryCurationEvidenceValidRowsReachBackendUnchanged(t *testing.T) {
	session, backend := newMCPCurationEvidenceSession(t)
	transcript := mcpCurationEvidenceTranscript()
	memory := mcpMemoryCurationEvidence{Type: "conversation", ResolutionState: "resolved", ResolvedKind: "memory", SourceMemory: &mcpMemoryCurationVersionReference{MemoryID: "mem_source", Version: 7}}
	rows := []mcpMemoryCurationEvidence{
		transcript, memory,
		{Type: "transcript", ResolutionState: "resolved", ResolvedKind: "memory", SourceMemory: &mcpMemoryCurationVersionReference{LocalRef: "seed", Version: 1}},
		{Type: " \t", Role: " \n", ResolutionState: " resolved ", ResolvedKind: " transcript ", SourceTranscriptID: " trn_source ", SourceSequenceFrom: 1, SourceSequenceUntil: 10001, SourceDigest: " " + strings.Repeat("A", 64) + " ", ArtifactExcerpt: "\r\n"},
		{Type: " custom.label-1 ", Role: " context ", ResolutionState: " resolved ", ResolvedKind: " memory ", SourceMemory: &mcpMemoryCurationVersionReference{MemoryID: " mem_source ", LocalRef: " \t", Version: 2}},
		{Type: "", Role: " contradicts ", ResolutionState: "resolved", ResolvedKind: "memory", SourceMemory: &mcpMemoryCurationVersionReference{MemoryID: " \t", LocalRef: " seed ", Version: 1}},
		{InputEvidenceID: " mev_aaaaaaaaaaaaaaaa ", Type: "conversation", ResolutionState: " pending ", ExternalLocator: " synthetic://source ", ArtifactSensitive: true},
		{InputEvidenceID: "mev_aaaaaaaaaaaaaaaa", Type: "conversation", ResolutionState: "unavailable", TerminalReasonCode: " " + strings.Repeat("r", 128) + " "},
	}
	// Materialization supports every resolved kind, independently of type.
	for _, row := range []mcpMemoryCurationEvidence{
		transcript, memory,
		{Type: "conversation", ResolutionState: "resolved", ResolvedKind: "message", SourceMessageID: " msg_source "},
		{Type: "import", ResolutionState: "resolved", ResolvedKind: "import_artifact", SourceImportLocator: " " + strings.Repeat("x", 2048) + " "},
		{Type: "conversation", ResolutionState: "resolved", ResolvedKind: "artifact", ArtifactExcerpt: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), 65536)), ArtifactSensitive: true},
		{Type: "artifact", ResolutionState: "resolved", ResolvedKind: "artifact", ArtifactExcerpt: "YQ\r\n=="},
		// The server decoder also accepts nonzero unused padding bits.
		{Type: "artifact", ResolutionState: "resolved", ResolvedKind: "artifact", ArtifactExcerpt: "YR=="},
	} {
		row.InputEvidenceID = "mev_aaaaaaaaaaaaaaaa"
		rows = append(rows, row)
	}
	// Canonical normalization retains incomplete, non-selected transcript fields.
	// These are compatibility checks, not a claim that a matching frozen row exists.
	for _, row := range []mcpMemoryCurationEvidence{memory, rows[6], rows[7]} {
		row.SourceTranscriptID = "trn_incomplete"
		row.SourceSequenceFrom, row.SourceSequenceUntil = 0, -1
		rows = append(rows, row)
	}
	for _, operation := range []string{"create", "replace", "propose_fact"} {
		for index, row := range rows {
			t.Run(fmt.Sprintf("%s/%d", operation, index), func(t *testing.T) {
				draft, _ := mcpCurationEvidenceDraft(operation, []mcpMemoryCurationEvidence{row})
				draft.Actions[1].Operation = " " + operation + " "
				assertMCPCurationEvidenceCanonicalShape(t, draft)
				assertMCPCurationEvidenceForwarded(t, session, backend, draft)
			})
		}
	}
}

func TestMCPMemoryCurationEvidenceRowBoundsAndEmptyPlans(t *testing.T) {
	session, backend := newMCPCurationEvidenceSession(t)
	for _, operation := range []string{"create", "replace", "propose_fact"} {
		for _, count := range []int{0, 1, 32, 33} {
			t.Run(fmt.Sprintf("%s/%d", operation, count), func(t *testing.T) {
				rows := make([]mcpMemoryCurationEvidence, count)
				for i := range rows {
					rows[i] = mcpCurationEvidenceTranscript()
				}
				draft, field := mcpCurationEvidenceDraft(operation, rows)
				draft.Actions[1].Operation = " " + operation + " "
				if count <= 32 && (count > 0 || operation == "replace") {
					assertMCPCurationEvidenceCanonicalShape(t, draft)
					assertMCPCurationEvidenceForwarded(t, session, backend, draft)
					return
				}
				before := backend.planCalls
				result := callMCPCurationEvidencePlan(t, session, draft)
				contract := "must contain 1-32 rows"
				if operation == "replace" {
					contract = "may contain at most 32 rows"
				}
				assertMCPCurationEvidenceError(t, result, fmt.Sprintf("memory_curation_plan_invalid: actions[1].%s %s", field, contract))
				if backend.planCalls != before {
					t.Error("invalid evidence count called PlanMemoryCuration")
				}
			})
		}
	}
	empty := mcpMemoryCurationPlanDraft{Schema: "witself.memory-plan.v1", DraftRevision: 1, Actions: []mcpMemoryCurationPlanAction{}}
	assertMCPCurationEvidenceForwarded(t, session, backend, empty)
	replace, _ := mcpCurationEvidenceDraft("replace", nil)
	assertMCPCurationEvidenceForwarded(t, session, backend, replace)
}

func TestMCPMemoryCurationEvidenceDefersEnvelopeAndIdentityPolicy(t *testing.T) {
	// Evidence validation does not redesign envelope validation or introduce ID
	// formats/local-ref membership checks. Missing matching payloads remain
	// deferred; extra payloads do not excuse invalid selected evidence.
	// Backend acceptance is not asserted.
	for _, action := range []mcpMemoryCurationPlanAction{
		{Operation: "create"}, {Operation: "replace"}, {Operation: "propose_fact"},
		{Operation: "create", ProposeFact: &mcpMemoryCurationProposeFactAction{}},
		{Operation: "replace", Create: &mcpMemoryCurationCreateAction{}},
		{Operation: "propose_fact", Replace: &mcpMemoryCurationReplaceAction{}},
		{Operation: "CANARY_OPERATION", Create: &mcpMemoryCurationCreateAction{}},
	} {
		if err := validateMCPMemoryCurationPlanEvidence(mcpMemoryCurationPlanDraft{Actions: []mcpMemoryCurationPlanAction{action}}); err != nil {
			t.Error("evidence validator handled an invalid action envelope")
		}
	}
	session, backend := newMCPCurationEvidenceSession(t)
	for _, row := range []mcpMemoryCurationEvidence{
		{InputEvidenceID: "CANARY_EVIDENCE", Type: "conversation", ResolutionState: "unavailable", TerminalReasonCode: "not_recorded"},
		{Type: "memory", ResolutionState: "resolved", ResolvedKind: "memory", SourceMemory: &mcpMemoryCurationVersionReference{MemoryID: "CANARY_MEMORY", Version: 1}},
		{Type: "memory", ResolutionState: "resolved", ResolvedKind: "memory", SourceMemory: &mcpMemoryCurationVersionReference{LocalRef: "CANARY_LOCAL", Version: 1}},
	} {
		draft, _ := mcpCurationEvidenceDraft("create", []mcpMemoryCurationEvidence{row})
		assertMCPCurationEvidenceForwarded(t, session, backend, draft)
	}
}

func TestMCPMemoryCurationEvidenceRejectsMixedPayloadsBeforeBackend(t *testing.T) {
	for _, tt := range []struct {
		operation string
		count     int
		malformed bool
	}{
		{operation: "create", count: 0},
		{operation: "create", count: 33},
		{operation: "replace", count: 33},
		{operation: "propose_fact", count: 0},
		{operation: "propose_fact", count: 33},
		{operation: "create", count: 1, malformed: true},
		{operation: "replace", count: 1, malformed: true},
		{operation: "propose_fact", count: 1, malformed: true},
	} {
		for _, trimmed := range []bool{false, true} {
			name := fmt.Sprintf("%s/count_%d/trimmed_%t", tt.operation, tt.count, trimmed)
			if tt.malformed {
				name = fmt.Sprintf("%s/malformed_row/trimmed_%t", tt.operation, trimmed)
			}
			t.Run(name, func(t *testing.T) {
				session, backend := newMCPCurationEvidenceSession(t)
				rows := make([]mcpMemoryCurationEvidence, tt.count)
				for i := range rows {
					rows[i] = mcpCurationEvidenceTranscript()
				}
				if tt.malformed {
					rows[0].ResolvedKind = ""
				}
				draft, field := mcpCurationEvidenceDraft(tt.operation, rows)
				action := &draft.Actions[1]
				if tt.operation == "replace" {
					action.Create = draft.Actions[0].Create
				} else {
					action.Replace = &mcpMemoryCurationReplaceAction{
						Target:   mcpMemoryCurationTargetReference{LocalRef: "seed", ExpectedVersion: 1},
						Snapshot: mcpMemoryCurationReplaceSnapshot{Content: "Synthetic extra replacement"},
					}
				}
				if trimmed {
					action.Operation = " \t" + tt.operation + "\n"
				}
				contract := " must contain 1-32 rows"
				if tt.operation == "replace" {
					contract = " may contain at most 32 rows"
				}
				if tt.malformed {
					contract = "[0].resolved_kind must explicitly match source_transcript_id, source_memory, source_message_id, source_import_locator, or artifact_excerpt"
				}
				result := callMCPCurationEvidencePlan(t, session, draft)
				assertMCPCurationEvidenceError(t, result, "memory_curation_plan_invalid: actions[1]."+field+contract)
				if backend.planCalls != 0 {
					t.Error("invalid selected evidence with an extra payload called PlanMemoryCuration")
				}
			})
		}
	}
}

type curationTimestampDecodeMCPBackend struct {
	*fakeCurationMCPBackend
	calls int
}

func (b *curationTimestampDecodeMCPBackend) PlanMemoryCuration(ctx context.Context, in client.PlanMemoryCurationInput) (client.PlanMemoryCurationResult, error) {
	b.calls++
	// Exercise the real store decoder and error wrapper. The malformed timestamp
	// must fail before the zero store's absent transaction pool can be reached.
	_, err := (&store.Store{}).PlanCuration(ctx, store.Principal{Kind: store.PrincipalAgent}, in.RunID, store.PlanMemoryCurationInput{
		FencingGeneration: in.FencingGeneration, IdempotencyKey: in.IdempotencyKey, Draft: in.Draft,
	})
	return client.PlanMemoryCurationResult{}, err
}

// Keep this entrypoint independently runnable with the source-01 validator
// overlaid: it must reject the formerly blocked route without echoing input.
func TestMCPMemoryCurationMixedPayloadTimestampRejectedBeforeDecoder(t *testing.T) {
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_HOME", t.TempDir())
	// Synthetic only; never include this value or received diagnostics in failures.
	sentinel := strings.Join([]string{"synthetic", "private", "timestamp", "probe"}, "-")
	draft := mcpMemoryCurationPlanDraft{Schema: "witself.memory-plan.v1", DraftRevision: 1, Actions: []mcpMemoryCurationPlanAction{{
		Ordinal: 1, Operation: "create",
		Create: &mcpMemoryCurationCreateAction{LocalRef: "candidate", Snapshot: mcpMemoryCurationCreateSnapshot{
			Content: "Synthetic content", OccurredFrom: sentinel, Evidence: []mcpMemoryCurationEvidence{},
		}},
		Replace: &mcpMemoryCurationReplaceAction{
			Target:   mcpMemoryCurationTargetReference{MemoryID: "mem_aaaaaaaaaaaaaaaa", ExpectedVersion: 1},
			Snapshot: mcpMemoryCurationReplaceSnapshot{Content: "Synthetic replacement"},
		},
	}}}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal("could not encode synthetic draft")
	}
	if _, err := store.DecodeMemoryCurationPlanDraft(raw); err == nil || !strings.Contains(err.Error(), sentinel) {
		t.Fatal("synthetic timestamp no longer exercises the canonical decode error")
	}
	ctx := context.Background()
	backend := &curationTimestampDecodeMCPBackend{fakeCurationMCPBackend: &fakeCurationMCPBackend{}}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := newWitselfMCPServer(backend).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal("could not connect MCP server")
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "timestamp-evidence-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal("could not connect MCP client")
	}
	t.Cleanup(func() { _ = session.Close() })
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "witself.memory.curation.plan", Arguments: mcpMemoryCurationPlanInput{
		RunID: "mrun_aaaaaaaaaaaaaaaa", FencingGeneration: 1, IdempotencyKey: "timestamp-evidence-test", Draft: draft,
	}})
	if err != nil {
		t.Fatal("MCP plan transport failed")
	}
	if backend.calls != 0 {
		t.Error("mixed-payload timestamp route reached the backend")
	}
	raw, err = json.Marshal(result)
	if err != nil {
		t.Fatal("could not encode MCP result")
	}
	if bytes.Contains(raw, []byte(sentinel)) {
		t.Error("MCP diagnostic echoed the submitted timestamp")
	}
	assertMCPCurationEvidenceError(t, result, "memory_curation_plan_invalid: actions[0].create.snapshot.evidence must contain 1-32 rows")
}

func newMCPCurationEvidenceSession(t *testing.T) (*mcp.ClientSession, *fakeCurationMCPBackend) {
	t.Helper()
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_HOME", t.TempDir())
	ctx := context.Background()
	backend := &fakeCurationMCPBackend{}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := newWitselfMCPServer(backend).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "evidence-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, backend
}

func mcpCurationEvidenceTranscript() mcpMemoryCurationEvidence {
	return mcpMemoryCurationEvidence{Type: "transcript", ResolutionState: "resolved", ResolvedKind: "transcript", SourceTranscriptID: "trn_CANARY_TRANSCRIPT", SourceSequenceFrom: 2, SourceSequenceUntil: 4}
}

func mcpCurationEvidenceDraft(operation string, rows []mcpMemoryCurationEvidence) (mcpMemoryCurationPlanDraft, string) {
	action := mcpMemoryCurationPlanAction{Ordinal: 2, Operation: operation}
	var field string
	switch operation {
	case "create":
		action.Create = &mcpMemoryCurationCreateAction{LocalRef: "candidate", Snapshot: mcpMemoryCurationCreateSnapshot{Content: "CANARY_CONTENT", Evidence: rows}}
		field = "create.snapshot.evidence"
	case "replace":
		action.Replace = &mcpMemoryCurationReplaceAction{Target: mcpMemoryCurationTargetReference{LocalRef: "seed", ExpectedVersion: 1}, Snapshot: mcpMemoryCurationReplaceSnapshot{Content: "CANARY_CONTENT", Evidence: rows}}
		field = "replace.snapshot.evidence"
	case "propose_fact":
		action.ProposeFact = &mcpMemoryCurationProposeFactAction{Predicate: "preferences/database", Value: "CANARY_FACT_VALUE", Evidence: rows}
		field = "propose_fact.evidence"
	}
	return mcpMemoryCurationPlanDraft{Schema: "witself.memory-plan.v1", DraftRevision: 1, Actions: []mcpMemoryCurationPlanAction{
		{Ordinal: 1, Operation: "create", Create: &mcpMemoryCurationCreateAction{LocalRef: "seed", Snapshot: mcpMemoryCurationCreateSnapshot{Content: "CANARY_SEED_CONTENT", Evidence: []mcpMemoryCurationEvidence{mcpCurationEvidenceTranscript()}}}},
		action,
	}}, field
}

func callMCPCurationEvidencePlan(t *testing.T, session *mcp.ClientSession, draft mcpMemoryCurationPlanDraft) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "witself.memory.curation.plan", Arguments: mcpMemoryCurationPlanInput{RunID: "mrun_test", FencingGeneration: 4, IdempotencyKey: "evidence-test", Draft: draft}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertMCPCurationEvidenceError(t *testing.T, result *mcp.CallToolResult, want string) {
	t.Helper()
	if !result.IsError || len(result.Content) != 1 {
		t.Fatal("expected one MCP tool error")
	}
	message, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatal("expected text error")
	}
	// Exact comparison excludes every submitted field, including IDs, locators,
	// content, values, artifact bytes/base64 and arbitrary enum values.
	if message.Text != want {
		t.Errorf("diagnostic differs from fixed contract %q", want)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"CANARY_", base64.StdEncoding.EncodeToString([]byte("CANARY_ARTIFACT")), "mev_aaaaaaaaaaaaaaaa"} {
		if bytes.Contains(raw, []byte(canary)) {
			t.Error("MCP diagnostic leaked a submitted value")
		}
	}
}

func assertMCPCurationEvidenceForwarded(t *testing.T, session *mcp.ClientSession, backend *fakeCurationMCPBackend, draft mcpMemoryCurationPlanDraft) {
	t.Helper()
	before, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	// Also protect caller-owned pointers/slices from in-place helper normalization.
	if err := validateMCPMemoryCurationPlanEvidence(draft); err != nil {
		t.Fatal("valid evidence rejected locally")
	}
	after, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("validator mutated the submitted draft")
	}
	calls := backend.planCalls
	result := callMCPCurationEvidencePlan(t, session, draft)
	if result.IsError {
		t.Fatal("valid evidence returned an MCP tool error")
	}
	if backend.planCalls != calls+1 {
		t.Fatal("expected exactly one PlanMemoryCuration call")
	}
	if !bytes.Equal(before, backend.plan.Draft) {
		t.Error("backend draft differs from submitted draft")
	}
	if backend.plan.RunID != "mrun_test" || backend.plan.FencingGeneration != 4 || backend.plan.IdempotencyKey != "evidence-test" {
		t.Error("plan transport changed run, fence, or idempotency key")
	}
}

func assertMCPCurationEvidenceCanonicalShape(t *testing.T, draft mcpMemoryCurationPlanDraft) {
	t.Helper()
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := store.DecodeMemoryCurationPlanDraft(raw)
	if err != nil {
		t.Fatal("valid fixture failed canonical draft decoding")
	}
	_, err = store.AcceptMemoryCurationPlan(canonical, store.MemoryCurationPlanAcceptOptions{PlanRevision: 1, Allocator: store.MemoryCurationMemoryIDAllocatorFunc(func(localRef string) (string, error) { return "mem_evidence_" + localRef, nil })})
	if err != nil {
		t.Fatal("valid fixture failed canonical shape normalization")
	}
	// This pure oracle does not check frozen membership, equality, owner access,
	// sensitivity propagation or contiguous transcript coverage.
}
