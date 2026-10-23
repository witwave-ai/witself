package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestTranscriptRedactionKeepsEvidenceAndCurationPostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := newTranscriptStorageTestPrincipal(t, st, "redaction-evidence")
	tr := transcriptRedactionCurationConversation(t, st, p, "evidence")
	captured, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "The retained decision has transcript evidence.", Kind: "decision", CaptureReason: "explicit_remember",
		Evidence:       []MemoryEvidenceInput{{ResolutionState: MemoryEvidencePending, ExternalLocator: "fixture://redaction-evidence"}},
		IdempotencyKey: "redaction-evidence-memory",
	})
	if err != nil {
		t.Fatal(err)
	}
	evidenceID := captured.Memory.Evidence[0].ID
	if _, err := st.ResolveMemoryEvidence(ctx, p, evidenceID, ResolveMemoryEvidenceInput{
		ResolvedKind: "transcript", SourceTranscriptID: tr.ID, SourceSequenceFrom: 3, SourceSequenceUntil: 3,
		IdempotencyKey: "redaction-evidence-resolve",
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "A second decision still has pending evidence.", Kind: "decision", CaptureReason: "explicit_remember",
		Evidence: []MemoryEvidenceInput{{ResolutionState: MemoryEvidencePending,
			ExternalLocator: fmt.Sprintf("witself://transcript/%s/sequence/3", tr.ID)}},
		IdempotencyKey: "redaction-pending-evidence-memory",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Memory.ID == captured.Memory.ID || len(pending.Memory.Evidence) != 1 || pending.Memory.Evidence[0].ResolutionState != MemoryEvidencePending {
		t.Fatal("second memory did not retain distinct pending evidence")
	}
	t.Run("pending-evidence-overlap-filter", func(t *testing.T) {
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var impact TranscriptRedactionResult
		rewrites := 0
		if err := transcriptRedactionImpactTx(ctx, transcriptRedactionPendingEvidenceTx{Tx: tx, rewrites: &rewrites}, p.AccountID,
			RedactTranscriptEntriesInput{TranscriptID: tr.ID, FromSequence: 3, UntilSequence: 3}, &impact); err != nil {
			t.Fatal(err)
		}
		if rewrites != 1 {
			t.Fatalf("pending-evidence projection shadowed %d impact queries, want 1", rewrites)
		}
		if impact.MemoryEvidenceCount != 1 || len(impact.EvidenceMemoryIDs) != 1 || impact.EvidenceMemoryIDs[0] != captured.Memory.ID {
			t.Fatal("overlapping pending evidence changed the resolved transcript evidence impact")
		}
	})
	evidenceSnapshot := func() string {
		t.Helper()
		var row string
		if err := st.pool.QueryRow(ctx, `SELECT to_jsonb(e)::text FROM memory_evidence e WHERE id=$1`, evidenceID).Scan(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	beforeEvidence := evidenceSnapshot()
	started := transcriptRedactionStartCuration(t, st, p)
	before := transcriptRedactionCurationSnapshot(t, st, p, started)
	owner := transcriptRedactionTestOwner(t, st, p)
	preview, err := st.RedactTranscriptEntries(ctx, owner, RedactTranscriptEntriesInput{
		TranscriptID: tr.ID, FromSequence: 3, UntilSequence: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.MemoryEvidenceCount != 1 || len(preview.EvidenceMemoryIDs) != 1 || preview.EvidenceMemoryIDs[0] != captured.Memory.ID ||
		preview.CurationRunsToCancel != 1 || preview.CancelledCurationRunCount != 0 {
		t.Fatal("preview did not report the evidence and covering curation run")
	}
	if after := transcriptRedactionCurationSnapshot(t, st, p, started); before != after {
		t.Fatal("redaction preview mutated curation state")
	}
	in := RedactTranscriptEntriesInput{TranscriptID: tr.ID, FromSequence: 3, UntilSequence: 3,
		Apply: true, ExpectedRedactionSetRevision: preview.RedactionSetRevision, IdempotencyKey: "redaction-with-evidence"}
	applied, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil {
		t.Fatal(err)
	}
	if applied.CancelledCurationRunCount != 1 {
		t.Fatal("redaction did not report its cancelled curation run")
	}
	assertTranscriptRedactionCurationCancelled(t, st, p, started, before)
	if evidenceSnapshot() != beforeEvidence {
		t.Fatal("redaction changed the resolved memory evidence row")
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, deleteErr := tx.Exec(ctx, `DELETE FROM transcript_entries WHERE transcript_id=$1 AND sequence=3`, tr.ID)
	_ = tx.Rollback(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(deleteErr, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("evidence-held redacted row deletion error = %v, want 23503", deleteErr)
	}
	replayed, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil || !replayed.Replayed || replayed.CancelledCurationRunCount != 0 {
		t.Fatalf("redaction replay cancellation result invalid: %v", err)
	}
	_, entries, err := st.GetTranscript(ctx, p, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 || entries[2].Body != TranscriptRedactedBody {
		t.Fatal("redaction changed the entry count or omitted its placeholder")
	}
	var payload struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(entries[2].Payload, &payload); err != nil || payload.Kind != "tool.result" {
		t.Fatal("redaction did not preserve the curation classification kind")
	}
}

// Migration 0029's CHECK forbids persisted pending transcript pointers. This
// query-local projection supplies overlapping pending pointers only to the real
// impact SQL, so its defense-in-depth filter is independently pinned without
// changing table data, constraints or schema. Other queries use the real tx.
// The projection also adds one resolved non-transcript row for the same range,
// so the resolved_kind half of the filter is pinned on its own. rewrites counts
// the queries it shadowed; a reflowed production query must fail the caller,
// never pass vacuously.
type transcriptRedactionPendingEvidenceTx struct {
	pgx.Tx
	rewrites *int
}

var transcriptRedactionEvidenceQuery = regexp.MustCompile(`FROM\s+memory_evidence\s+WHERE`)

func (tx transcriptRedactionPendingEvidenceTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if transcriptRedactionEvidenceQuery.MatchString(sql) {
		*tx.rewrites++
		sql = `WITH memory_evidence AS (
			SELECT memory_id,account_id,resolution_state,resolved_kind,
			COALESCE(source_transcript_id,$2::text) AS source_transcript_id,
			COALESCE(source_sequence_from,$3::bigint) AS source_sequence_from,
			COALESCE(source_sequence_until,$4::bigint) AS source_sequence_until
			FROM memory_evidence
			UNION ALL
			SELECT 'mem_resolved_non_transcript_probe',$1::text,'resolved','memory',$2::text,$3::bigint,$4::bigint
		) ` + sql
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func TestTranscriptRedactionCurationCoveragePostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, mode := range []string{
		"transcript", "planned", "coverage", "cursor", "non-overlap", "cursor-prior-boundary", "cursor-upper-boundary", "other-transcript", "applied",
		"transcript-until-boundary", "transcript-from-boundary", "coverage-until-boundary", "coverage-from-boundary",
		"cursor-until-overlap", "cursor-prior-overlap", "transcript-after-range", "coverage-after-range",
	} {
		t.Run(mode, func(t *testing.T) {
			p := newTranscriptStorageTestPrincipal(t, st, "redaction-curation-"+mode)
			tr := transcriptRedactionCurationConversation(t, st, p, "target")
			other := transcriptRedactionCurationConversation(t, st, p, "other")
			started := transcriptRedactionStartCuration(t, st, p)
			// Starting through the public store path supplies real requests,
			// lanes and frozen rows. Narrow those rows to independently prove
			// each coverage branch, without another branch masking a defect.
			if mode != "other-transcript" {
				if _, err := st.pool.Exec(ctx, `DELETE FROM memory_curation_run_inputs WHERE run_id=$1
					AND (transcript_id=$2 OR cursor_stream_id=$2)`, started.Run.ID, other.ID); err != nil {
					t.Fatal(err)
				}
			}
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := st.pool.Exec(ctx, query, args...); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "transcript", "coverage", "transcript-until-boundary", "transcript-from-boundary", "coverage-until-boundary", "coverage-from-boundary", "transcript-after-range", "coverage-after-range":
				exec(`DELETE FROM memory_curation_run_inputs WHERE run_id=$1 AND input_kind='cursor'`, started.Run.ID)
				if mode == "coverage" || mode == "coverage-until-boundary" || mode == "coverage-from-boundary" || mode == "coverage-after-range" {
					exec(`UPDATE memory_curation_run_inputs SET input_kind='transcript_coverage',
						coverage_counts='{"tool.result":5}'::jsonb WHERE run_id=$1 AND input_kind='transcript'`, started.Run.ID)
				}
				switch mode {
				case "transcript-until-boundary", "coverage-until-boundary":
					exec(`UPDATE memory_curation_run_inputs SET sequence_from=1,sequence_until=3 WHERE run_id=$1 AND input_kind IN ('transcript','transcript_coverage')`, started.Run.ID)
				case "transcript-from-boundary", "coverage-from-boundary":
					exec(`UPDATE memory_curation_run_inputs SET sequence_from=3,sequence_until=5 WHERE run_id=$1 AND input_kind IN ('transcript','transcript_coverage')`, started.Run.ID)
				case "transcript-after-range", "coverage-after-range":
					exec(`UPDATE memory_curation_run_inputs SET sequence_from=4,sequence_until=5 WHERE run_id=$1 AND input_kind IN ('transcript','transcript_coverage')`, started.Run.ID)
				}
			case "cursor", "cursor-prior-boundary", "cursor-upper-boundary", "cursor-until-overlap", "cursor-prior-overlap":
				exec(`DELETE FROM memory_curation_run_inputs WHERE run_id=$1 AND input_kind <> 'cursor'`, started.Run.ID)
				if mode == "cursor-prior-boundary" {
					exec(`UPDATE memory_curation_run_inputs SET cursor_expected_prior=3
						WHERE run_id=$1 AND cursor_source_kind='transcript'`, started.Run.ID)
				}
				if mode == "cursor-upper-boundary" {
					exec(`UPDATE memory_curation_run_inputs SET cursor_upper=2
						WHERE run_id=$1 AND cursor_source_kind='transcript'`, started.Run.ID)
				}
				if mode == "cursor-until-overlap" {
					exec(`UPDATE memory_curation_run_inputs SET cursor_expected_prior=0,cursor_upper=3
						WHERE run_id=$1 AND cursor_source_kind='transcript'`, started.Run.ID)
				}
				if mode == "cursor-prior-overlap" {
					exec(`UPDATE memory_curation_run_inputs SET cursor_expected_prior=2,cursor_upper=5
						WHERE run_id=$1 AND cursor_source_kind='transcript'`, started.Run.ID)
				}
			case "non-overlap":
				exec(`UPDATE memory_curation_run_inputs SET sequence_from=1,sequence_until=2
					WHERE run_id=$1 AND input_kind='transcript'`, started.Run.ID)
				exec(`DELETE FROM memory_curation_run_inputs WHERE run_id=$1 AND input_kind <> 'transcript'`, started.Run.ID)
			case "other-transcript":
				exec(`DELETE FROM memory_curation_run_inputs WHERE run_id=$1
					AND (transcript_id=$2 OR cursor_stream_id=$2)`, started.Run.ID, tr.ID)
			}
			// Keep frozen membership summary counts consistent with the narrowed
			// fixture, including transcript_coverage's transcript count.
			exec(`UPDATE memory_curation_runs SET
				input_count=(SELECT count(*) FROM memory_curation_run_inputs WHERE run_id=$1),
				memory_input_count=(SELECT count(*) FROM memory_curation_run_inputs WHERE run_id=$1 AND input_kind='memory'),
				evidence_input_count=(SELECT count(*) FROM memory_curation_run_inputs WHERE run_id=$1 AND input_kind='evidence'),
				transcript_input_count=(SELECT count(*) FROM memory_curation_run_inputs WHERE run_id=$1 AND input_kind IN ('transcript','transcript_coverage')),
				cursor_input_count=(SELECT count(*) FROM memory_curation_run_inputs WHERE run_id=$1 AND input_kind='cursor')
				WHERE id=$1`, started.Run.ID)
			if mode == "planned" || mode == "applied" {
				cursor := ""
				for {
					page, err := st.GetCurationRunInputs(ctx, p, started.Run.ID, started.Run.FencingGeneration, cursor, 100)
					if err != nil {
						t.Fatal(err)
					}
					cursor = page.NextCursor
					if cursor == "" {
						break
					}
				}
				planned, err := st.PlanCuration(ctx, p, started.Run.ID, PlanMemoryCurationInput{
					FencingGeneration: started.Run.FencingGeneration, Draft: marshalEmptyCurationPlanForAccessProfile(t), IdempotencyKey: "fixture-plan",
				})
				if err != nil {
					t.Fatal(err)
				}
				if mode == "applied" {
					if _, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
						FencingGeneration: started.Run.FencingGeneration, PlanRevision: planned.Plan.PlanRevision,
						PlanHash: planned.Receipt.PlanHash, IdempotencyKey: "fixture-apply",
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			siblingAgent, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, "sibling")
			if err != nil {
				t.Fatal(err)
			}
			sibling := p
			sibling.ID = siblingAgent.ID
			transcriptRedactionCurationConversation(t, st, sibling, "sibling")
			transcriptRedactionStartCuration(t, st, sibling)
			if _, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
				Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceTranscript}},
				CoalescingKey: "redaction-other-request", TriggerReason: "manual_refine", IdempotencyKey: "redaction-other-request",
			}); err != nil {
				t.Fatal(err)
			}
			before := transcriptRedactionCurationSnapshot(t, st, p, started)
			unrelated := transcriptRedactionUnrelatedCurationSnapshot(t, st, p, started)
			beforeMetrics := st.MemoryCurationCounters()
			priorState := MemoryCurationRunOpen
			if mode == "planned" {
				priorState = MemoryCurationRunPlanned
			}
			wantCancelled := int64(1)
			if mode == "non-overlap" || mode == "cursor-prior-boundary" || mode == "cursor-upper-boundary" || mode == "other-transcript" || mode == "applied" || mode == "transcript-after-range" || mode == "coverage-after-range" {
				wantCancelled = 0
			}
			owner := transcriptRedactionTestOwner(t, st, p)
			preview, err := st.RedactTranscriptEntries(ctx, owner, RedactTranscriptEntriesInput{TranscriptID: tr.ID, FromSequence: 3, UntilSequence: 3})
			if err != nil {
				t.Fatal(err)
			}
			if preview.CurationRunsToCancel != wantCancelled || preview.CancelledCurationRunCount != 0 {
				t.Fatalf("preview curation count = %d/%d, want %d/0", preview.CurationRunsToCancel, preview.CancelledCurationRunCount, wantCancelled)
			}
			if after := transcriptRedactionCurationSnapshot(t, st, p, started); before != after {
				t.Fatal("preview changed curation state")
			}
			if after := transcriptRedactionUnrelatedCurationSnapshot(t, st, p, started); unrelated != after {
				t.Fatal("preview changed an unrelated curation lane, request or run")
			}
			assertTranscriptRedactionCurationMetricDelta(t, beforeMetrics, st.MemoryCurationCounters(), priorState, 0)
			in := RedactTranscriptEntriesInput{
				TranscriptID: tr.ID, FromSequence: 3, UntilSequence: 3, Apply: true,
				ExpectedRedactionSetRevision: preview.RedactionSetRevision, IdempotencyKey: "coverage-redaction",
			}
			applied, err := st.RedactTranscriptEntries(ctx, owner, in)
			if err != nil {
				t.Fatal(err)
			}
			if applied.CancelledCurationRunCount != wantCancelled {
				t.Fatalf("apply curation count = %d, want %d", applied.CancelledCurationRunCount, wantCancelled)
			}
			assertTranscriptRedactionCurationMetricDelta(t, beforeMetrics, st.MemoryCurationCounters(), priorState, applied.CancelledCurationRunCount)
			if after := transcriptRedactionUnrelatedCurationSnapshot(t, st, p, started); unrelated != after {
				t.Fatal("redaction changed an unrelated curation lane, request or run")
			}
			beforeReplayMetrics := st.MemoryCurationCounters()
			replayed, err := st.RedactTranscriptEntries(ctx, owner, in)
			if err != nil || !replayed.Replayed || replayed.CancelledCurationRunCount != 0 {
				t.Fatalf("redaction replay: replayed=%t cancellations=%d err=%v", replayed.Replayed, replayed.CancelledCurationRunCount, err)
			}
			assertTranscriptRedactionCurationMetricDelta(t, beforeReplayMetrics, st.MemoryCurationCounters(), priorState, 0)
			if wantCancelled == 1 {
				assertTranscriptRedactionCurationCancelled(t, st, p, started, before)
			} else if after := transcriptRedactionCurationSnapshot(t, st, p, started); before != after {
				t.Fatal("redaction changed a non-covering or applied curation run, request, lane, receipt or event")
			}
		})
	}
}

// Snapshot every bystander row, including the sibling's claimed request and
// active lane, plus the owner's separately coalesced request.
func transcriptRedactionUnrelatedCurationSnapshot(t *testing.T, st *Store, p Principal, started StartMemoryCurationResult) [3]string {
	t.Helper()
	ctx := context.Background()
	var out [3]string
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY realm_id,owner_kind,owner_id),'[]'::jsonb)::text
			FROM memory_curation_lanes x WHERE account_id=$1 AND NOT (realm_id=$2 AND owner_kind='agent' AND owner_id=$3)`,
			[]any{p.AccountID, p.RealmID, p.ID}},
		{`SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb)::text
			FROM memory_curation_requests x WHERE account_id=$1 AND id<>$2`, []any{p.AccountID, started.Request.ID}},
		{`SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb)::text
			FROM memory_curation_runs x WHERE account_id=$1 AND id<>$2`, []any{p.AccountID, started.Run.ID}},
	}
	for i, query := range queries {
		if err := st.pool.QueryRow(ctx, query.sql, query.args...).Scan(&out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func assertTranscriptRedactionCurationMetricDelta(t *testing.T, before, after MemoryCurationCounters, priorState string, cancelled int64) {
	t.Helper()
	for _, state := range []string{MemoryCurationRunOpen, MemoryCurationRunPlanned} {
		transition := MemoryCurationTransition{From: state, To: MemoryCurationRunAbandoned}
		want := before.Transitions[transition]
		if state == priorState {
			want += uint64(cancelled)
		}
		if got := after.Transitions[transition]; got != want {
			t.Errorf("%s -> abandoned transitions = %d, want %d", state, got, want)
		}
	}
	if !maps.Equal(before.LeaseEvents, after.LeaseEvents) {
		t.Error("redaction recorded a curation lease event")
	}
}

func transcriptRedactionCurationConversation(t *testing.T, st *Store, p Principal, name string) Transcript {
	t.Helper()
	tr := createTranscriptStorageTestConversation(t, st, p, name)
	for seq := 1; seq <= 5; seq++ {
		appendTranscriptStorageTestEntry(t, st, p, tr, AppendTranscriptEntryInput{
			ExternalID: fmt.Sprintf("entry-%d", seq), Role: TranscriptRoleTool,
			Body: "fixture evidence", Payload: json.RawMessage(`{"kind":"tool.result"}`),
		})
	}
	return tr
}

func transcriptRedactionStartCuration(t *testing.T, st *Store, p Principal) StartMemoryCurationResult {
	t.Helper()
	ctx := context.Background()
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceTranscript}},
		TriggerReason: "manual_refine", IdempotencyKey: "redaction-curation-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: 10 * time.Minute, IdempotencyKey: "redaction-curation-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	return started
}

type transcriptRedactionCurationState struct {
	Run, Request, Lane, Inputs string
	Fence, Mutations, Events   int64
}

func transcriptRedactionCurationSnapshot(t *testing.T, st *Store, p Principal, started StartMemoryCurationResult) transcriptRedactionCurationState {
	t.Helper()
	ctx := context.Background()
	var out transcriptRedactionCurationState
	for _, query := range []struct {
		sql string
		id  string
		dst *string
	}{
		{`SELECT to_jsonb(r)::text FROM memory_curation_runs r WHERE id=$1`, started.Run.ID, &out.Run},
		{`SELECT to_jsonb(r)::text FROM memory_curation_requests r WHERE id=$1`, started.Request.ID, &out.Request},
		{`SELECT to_jsonb(l)::text FROM memory_curation_lanes l WHERE owner_id=$1`, p.ID, &out.Lane},
		{`SELECT COALESCE(jsonb_agg(to_jsonb(i) ORDER BY ordinal),'[]'::jsonb)::text FROM memory_curation_run_inputs i WHERE run_id=$1`, started.Run.ID, &out.Inputs},
	} {
		if err := st.pool.QueryRow(ctx, query.sql, query.id).Scan(query.dst); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.pool.QueryRow(ctx, `SELECT fencing_generation FROM memory_curation_lanes WHERE owner_id=$1`, p.ID).Scan(&out.Fence); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM memory_curation_mutations WHERE account_id=$1`, p.AccountID).Scan(&out.Mutations); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM account_events WHERE account_id=$1 AND verb LIKE 'memory.curation.%'`, p.AccountID).Scan(&out.Events); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertTranscriptRedactionCurationCancelled(t *testing.T, st *Store, p Principal, started StartMemoryCurationResult, before transcriptRedactionCurationState) {
	t.Helper()
	ctx := context.Background()
	var state, reason, requestState, active string
	var leaseCleared, terminalAt, cancelledAt bool
	var fence int64
	if err := st.pool.QueryRow(ctx, `SELECT state,terminal_reason_code,lease_expires_at IS NULL,terminal_at IS NOT NULL
		FROM memory_curation_runs WHERE id=$1`, started.Run.ID).Scan(&state, &reason, &leaseCleared, &terminalAt); err != nil {
		t.Fatal(err)
	}
	if state != MemoryCurationRunAbandoned || reason != "transcript_redacted" || !leaseCleared || !terminalAt {
		t.Fatal("covering curation run did not take the non-requeue abandoned transition")
	}
	if err := st.pool.QueryRow(ctx, `SELECT state,cancelled_at IS NOT NULL FROM memory_curation_requests WHERE id=$1`, started.Request.ID).Scan(&requestState, &cancelledAt); err != nil {
		t.Fatal(err)
	}
	if requestState != MemoryCurationRequestCancelled || !cancelledAt {
		t.Fatal("covering curation request was not cancelled")
	}
	if err := st.pool.QueryRow(ctx, `SELECT COALESCE(active_run_id,''),fencing_generation FROM memory_curation_lanes WHERE owner_id=$1`, p.ID).Scan(&active, &fence); err != nil {
		t.Fatal(err)
	}
	if active != "" || fence != before.Fence+1 {
		t.Fatal("covering curation lane was not released with an advanced fence")
	}
	after := transcriptRedactionCurationSnapshot(t, st, p, started)
	if after.Mutations != before.Mutations || after.Events != before.Events || after.Inputs != before.Inputs {
		t.Fatal("redaction wrote a curation mutation/event or changed frozen input membership")
	}
}
