package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestMemoryCurationApplyPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := provisionMemoryCurationApplyPrincipal(ctx, t, st)
	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "curation-apply-thread"})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
		transcript.ID, AppendTranscriptEntryInput{
			ExternalID: "curation-apply-turn", Role: TranscriptRoleUser,
			Body: "PostgreSQL is the sole source of memory data.",
		})
	if err != nil {
		t.Fatal(err)
	}
	source, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "PostgreSQL is our memory database.", Kind: "decision",
		Evidence: []MemoryEvidenceInput{{
			Type: "conversation", ResolutionState: MemoryEvidenceResolved,
			ResolvedKind:       MemoryCurationSourceTranscript,
			SourceTranscriptID: transcript.ID, SourceSequenceFrom: entry.Sequence,
			SourceSequenceUntil: entry.Sequence,
		}},
		IdempotencyKey: "curation-apply-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		CoalescingKey: "apply_test", TriggerReason: "manual_refine",
		IdempotencyKey: "curation-apply-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: 5 * time.Minute,
		Client:         MemoryClientProvenance{Runtime: "test", Model: "client-model"},
		IdempotencyKey: "curation-apply-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	draft := MemoryCurationPlanDraft{
		Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1,
		Actions: []MemoryCurationPlanAction{
			{Ordinal: 1, Operation: MemoryCurationOperationCreate,
				Create: &MemoryCurationCreateAction{
					LocalRef: "summary",
					Snapshot: MemoryCurationMemorySnapshot{
						Content: "The memory architecture uses PostgreSQL as its canonical data store.",
						Kind:    "decision",
						Evidence: []MemoryCurationEvidence{{
							Type: "conversation", ResolutionState: MemoryEvidenceResolved,
							ResolvedKind:       MemoryCurationSourceTranscript,
							SourceTranscriptID: transcript.ID,
							SourceSequenceFrom: entry.Sequence, SourceSequenceUntil: entry.Sequence,
						}},
					},
					Relations: []MemoryCurationLineageRelation{{
						RelationType: MemoryCurationRelationDerivedFrom,
						To:           MemoryCurationVersionReference{MemoryID: source.Memory.ID, Version: 1},
					}},
				}},
			{Ordinal: 2, Operation: MemoryCurationOperationReplace,
				Replace: &MemoryCurationReplaceAction{
					Target: MemoryCurationTargetReference{
						MemoryID: source.Memory.ID, ExpectedVersion: 1,
					},
					Snapshot: MemoryCurationMemorySnapshot{
						Content: "PostgreSQL is the sole canonical source for memory data.",
						Kind:    "decision",
						Evidence: []MemoryCurationEvidence{{
							Type: "conversation", ResolutionState: MemoryEvidenceResolved,
							ResolvedKind:       MemoryCurationSourceTranscript,
							SourceTranscriptID: transcript.ID,
							SourceSequenceFrom: entry.Sequence, SourceSequenceUntil: entry.Sequence,
						}},
					},
					Reason: "make the decision precise",
				}},
			{Ordinal: 3, Operation: MemoryCurationOperationRelate,
				Relate: &MemoryCurationRelateAction{
					RelationType: MemoryCurationRelationSummarizes,
					From:         MemoryCurationVersionReference{LocalRef: "summary", Version: 1},
					To:           MemoryCurationVersionReference{MemoryID: source.Memory.ID, Version: 1},
				}},
			{Ordinal: 4, Operation: MemoryCurationOperationProposeFact,
				ProposeFact: &MemoryCurationProposeFactAction{
					Predicate: "architecture/memory_store", ValueType: "string",
					Value: json.RawMessage(`"postgresql"`),
					Evidence: []MemoryCurationEvidence{{
						Type: "memory", ResolutionState: MemoryEvidenceResolved,
						ResolvedKind: MemoryCurationSourceMemory,
						SourceMemory: &MemoryCurationVersionReference{LocalRef: "summary", Version: 1},
					}},
				}},
		},
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := st.PlanCuration(ctx, p, started.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration, Draft: raw,
		IdempotencyKey: "curation-apply-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(planned.PreallocatedMemoryIDs) != 1 {
		t.Fatalf("preallocated ids = %#v", planned.PreallocatedMemoryIDs)
	}
	// A same-key trigger arriving after the snapshot must survive as queued
	// follow-up work rather than being swallowed by this apply.
	later, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		CoalescingKey: "apply_test", TriggerReason: "new_evidence",
		IdempotencyKey: "curation-apply-later-trigger",
	})
	if err != nil {
		t.Fatal(err)
	}
	if later.Request.ID != requested.Request.ID ||
		later.Request.RequestGeneration <= started.Run.RequestGeneration {
		t.Fatalf("later generation = %#v", later.Request)
	}
	applyInput := ApplyMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
		IdempotencyKey: "curation-apply-commit",
	}
	applied, err := st.ApplyCuration(ctx, p, started.Run.ID, applyInput)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Run.State != MemoryCurationRunApplied || applied.Receipt.ID == "" ||
		len(applied.Receipt.ActionResults) != 4 || applied.FollowUpRequest == nil ||
		applied.FollowUpRequest.State != MemoryCurationRequestQueued ||
		applied.FollowUpRequest.RequestGeneration != later.Request.RequestGeneration {
		t.Fatalf("applied = %#v", applied)
	}
	createdID := planned.PreallocatedMemoryIDs[0].MemoryID
	created, err := st.GetMemory(ctx, p, createdID)
	if err != nil {
		t.Fatal(err)
	}
	if created.Origin != "agent" || created.CaptureReason != "curation" ||
		created.CurationRunID != started.Run.ID || created.CurationActionID == "" ||
		len(created.Evidence) != 1 {
		t.Fatalf("created memory = %#v", created)
	}
	adjusted, err := st.GetMemory(ctx, p, source.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if adjusted.Version != 2 || adjusted.Content != "PostgreSQL is the sole canonical source for memory data." ||
		adjusted.CurationRunID != started.Run.ID {
		t.Fatalf("adjusted memory = %#v", adjusted)
	}
	var relationCount int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_relations
		WHERE curation_run_id=$1 AND reverted_at IS NULL`, started.Run.ID).Scan(&relationCount); err != nil {
		t.Fatal(err)
	}
	if relationCount != 2 {
		t.Fatalf("relation count = %d, want 2", relationCount)
	}
	replayed, err := st.ApplyCuration(ctx, p, started.Run.ID, applyInput)
	if err != nil || !replayed.Receipt.Replayed || replayed.Receipt.ID != applied.Receipt.ID {
		t.Fatalf("apply replay = %#v / %v", replayed, err)
	}
	changed := applyInput
	changed.PlanHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := st.ApplyCuration(ctx, p, started.Run.ID, changed); !errors.Is(err, ErrMemoryCurationIdempotencyConflict) {
		t.Fatalf("changed apply retry error = %v", err)
	}

	producedHeads, _ := memoryCurationProducedHeads(applied.Receipt.ActionResults)
	if len(producedHeads) != 2 {
		t.Fatalf("produced heads = %#v", producedHeads)
	}
	// A successor curator, even one working on queued follow-up generation,
	// owns the lane and blocks historical rollback until it explicitly exits.
	successor, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: applied.FollowUpRequest.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "curation-rollback-successor-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	rollbackInput := RollbackMemoryCurationInput{
		ApplyReceiptID: applied.Receipt.ID, ExpectedProducedHeads: producedHeads,
		Reason: "verify compensating rollback", IdempotencyKey: "curation-rollback-commit",
	}
	if _, err := st.RollbackCuration(ctx, p, started.Run.ID, rollbackInput); !errors.Is(err, ErrMemoryCurationBusy) {
		t.Fatalf("rollback with active successor error = %v", err)
	}
	if _, err := st.CancelCuration(ctx, p, successor.Run.ID, FinishMemoryCurationInput{
		FencingGeneration: successor.Run.FencingGeneration,
		Reason:            "rollback_test", IdempotencyKey: "curation-rollback-successor-cancel",
	}); err != nil {
		t.Fatal(err)
	}
	wrongHeads := append([]MemoryVersionReference(nil), producedHeads...)
	wrongHeads[0].Version++
	wrongRollback := rollbackInput
	wrongRollback.ExpectedProducedHeads = wrongHeads
	wrongRollback.IdempotencyKey = "curation-rollback-wrong-head"
	if _, err := st.RollbackCuration(ctx, p, started.Run.ID, wrongRollback); !errors.Is(err, ErrMemoryCurationConflict) {
		t.Fatalf("wrong rollback heads error = %v", err)
	}
	stillCreated, err := st.GetMemory(ctx, p, createdID)
	if err != nil || stillCreated.Version != 1 || stillCreated.State != MemoryStateActive {
		t.Fatalf("wrong-head rollback was not atomic = %#v / %v", stillCreated, err)
	}
	cursorPositions := make(map[string]int64)
	for _, interval := range applied.Receipt.CursorIntervals {
		var position int64
		if err := st.pool.QueryRow(ctx, `
			SELECT position FROM memory_curation_cursors
			WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
			  AND source_kind=$4 AND source_stream_id=$5`, p.AccountID, p.RealmID,
			p.ID, interval.SourceKind, interval.SourceStreamID).Scan(&position); err != nil {
			t.Fatal(err)
		}
		cursorPositions[interval.SourceKind+"\x00"+interval.SourceStreamID] = position
	}
	rolledBack, err := st.RollbackCuration(ctx, p, started.Run.ID, rollbackInput)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Run.State != MemoryCurationRunRolledBack ||
		!rolledBack.ReplayRequest.ReadOnlyReplay ||
		rolledBack.ReplayRequest.ReplayRunID != started.Run.ID ||
		len(rolledBack.Receipt.ActionResults) != 4 {
		t.Fatalf("rolled back = %#v", rolledBack)
	}
	revertedCreated, err := st.GetMemory(ctx, p, createdID)
	if err != nil {
		t.Fatal(err)
	}
	if revertedCreated.Version != 2 || revertedCreated.State != MemoryStateReverted ||
		revertedCreated.Operation != "reverted" || revertedCreated.CurationRunID != started.Run.ID {
		t.Fatalf("reverted created memory = %#v", revertedCreated)
	}
	restoredSource, err := st.GetMemory(ctx, p, source.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restoredSource.Version != 3 || restoredSource.State != MemoryStateActive ||
		restoredSource.Operation != "reverted" || restoredSource.Content != source.Memory.Content {
		t.Fatalf("restored source memory = %#v", restoredSource)
	}
	var revertedRelations int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_relations
		WHERE curation_run_id=$1 AND reverted_at IS NOT NULL
		  AND reverted_by_run_id=$1 AND reverted_by_action_id IS NOT NULL`,
		started.Run.ID).Scan(&revertedRelations); err != nil {
		t.Fatal(err)
	}
	if revertedRelations != 2 {
		t.Fatalf("reverted relations = %d, want 2", revertedRelations)
	}
	candidateID := applied.Receipt.ActionResults[3].CandidateIDs[0]
	var candidateStatus string
	if err := st.pool.QueryRow(ctx, `SELECT status FROM fact_candidates WHERE id=$1`,
		candidateID).Scan(&candidateStatus); err != nil {
		t.Fatal(err)
	}
	if candidateStatus != "withdrawn" {
		t.Fatalf("candidate status = %q", candidateStatus)
	}
	for _, interval := range applied.Receipt.CursorIntervals {
		var position int64
		if err := st.pool.QueryRow(ctx, `
			SELECT position FROM memory_curation_cursors
			WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
			  AND source_kind=$4 AND source_stream_id=$5`, p.AccountID, p.RealmID,
			p.ID, interval.SourceKind, interval.SourceStreamID).Scan(&position); err != nil {
			t.Fatal(err)
		}
		if position != cursorPositions[interval.SourceKind+"\x00"+interval.SourceStreamID] {
			t.Fatalf("rollback rewound cursor %s/%s from %d to %d", interval.SourceKind,
				interval.SourceStreamID,
				cursorPositions[interval.SourceKind+"\x00"+interval.SourceStreamID], position)
		}
	}
	rollbackReplay, err := st.RollbackCuration(ctx, p, started.Run.ID, rollbackInput)
	if err != nil || !rollbackReplay.Receipt.Replayed ||
		rollbackReplay.Receipt.ID != rolledBack.Receipt.ID {
		t.Fatalf("rollback replay = %#v / %v", rollbackReplay, err)
	}
	changedRollback := rollbackInput
	changedRollback.Reason = "different reason"
	if _, err := st.RollbackCuration(ctx, p, started.Run.ID, changedRollback); !errors.Is(err, ErrMemoryCurationIdempotencyConflict) {
		t.Fatalf("changed rollback retry error = %v", err)
	}
}

func TestMemoryCurationApplyConflictsPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	t.Run("stale head writes nothing and exact retry stays conflicted", func(t *testing.T) {
		ctx := context.Background()
		st, p, source, started, planned, createdID := prepareMemoryCurationConflictPlan(ctx, t, dsn, true)
		changedContent := "A direct write arrived after the curation snapshot."
		if _, err := st.AdjustMemory(ctx, p, source.Memory.ID, AdjustMemoryInput{
			ExpectedVersion: 1, Content: &changedContent,
			IdempotencyKey: "curation-conflict-direct-adjust",
		}); err != nil {
			t.Fatal(err)
		}
		in := ApplyMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
			IdempotencyKey: "curation-conflict-apply",
		}
		if _, err := st.ApplyCuration(ctx, p, started.Run.ID, in); !errors.Is(err, ErrMemoryCurationConflict) {
			t.Fatalf("stale apply error = %v", err)
		}
		if _, err := st.GetMemory(ctx, p, createdID); !errors.Is(err, ErrMemoryNotFound) {
			t.Fatalf("stale apply created memory error = %v", err)
		}
		run, err := st.GetCurationRun(ctx, p, started.Run.ID)
		if err != nil || run.State != MemoryCurationRunConflict {
			t.Fatalf("conflicted run = %#v / %v", run, err)
		}
		var appliedActions int
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FROM memory_curation_actions
			WHERE run_id=$1 AND state='applied'`, started.Run.ID).Scan(&appliedActions); err != nil {
			t.Fatal(err)
		}
		if appliedActions != 0 {
			t.Fatalf("stale apply left %d applied actions", appliedActions)
		}
		if _, err := st.ApplyCuration(ctx, p, started.Run.ID, in); !errors.Is(err, ErrMemoryCurationConflict) {
			t.Fatalf("stale apply exact retry error = %v", err)
		}
	})

	t.Run("cursor compare-and-swap rolls back produced resources", func(t *testing.T) {
		ctx := context.Background()
		st, p, _, started, planned, createdID := prepareMemoryCurationConflictPlan(ctx, t, dsn, false)
		var sourceKind, streamID string
		var upper int64
		if err := st.pool.QueryRow(ctx, `
			SELECT cursor_source_kind,cursor_stream_id,cursor_upper
			FROM memory_curation_run_inputs
			WHERE run_id=$1 AND input_kind='cursor'
			ORDER BY ordinal LIMIT 1`, started.Run.ID).Scan(&sourceKind, &streamID, &upper); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE memory_curation_cursors SET position=$6
			WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
			  AND source_kind=$4 AND source_stream_id=$5`, p.AccountID, p.RealmID,
			p.ID, sourceKind, streamID, upper); err != nil {
			t.Fatal(err)
		}
		in := ApplyMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
			IdempotencyKey: "curation-cursor-conflict-apply",
		}
		if _, err := st.ApplyCuration(ctx, p, started.Run.ID, in); !errors.Is(err, ErrMemoryCurationConflict) {
			t.Fatalf("cursor conflict apply error = %v", err)
		}
		if _, err := st.GetMemory(ctx, p, createdID); !errors.Is(err, ErrMemoryNotFound) {
			t.Fatalf("cursor conflict left created memory error = %v", err)
		}
		var appliedActions int
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FROM memory_curation_actions
			WHERE run_id=$1 AND state='applied'`, started.Run.ID).Scan(&appliedActions); err != nil {
			t.Fatal(err)
		}
		if appliedActions != 0 {
			t.Fatalf("cursor conflict left %d applied actions", appliedActions)
		}
	})
}

func TestMemoryCurationSupersedeApplyRollbackPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := provisionMemoryCurationApplyPrincipal(ctx, t, st)
	capture := func(content, key string) MemoryMutationResult {
		out, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
			Content: content, Kind: "decision",
			Evidence: []MemoryEvidenceInput{{
				Type: "system", ResolutionState: MemoryEvidenceUnavailable,
				TerminalReasonCode: "not_recorded",
			}},
			IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	source := capture("An older narrative decision.", "curation-supersede-source")
	replacement := capture("The durable replacement narrative.", "curation-supersede-replacement")
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		CoalescingKey: "supersede_test", TriggerReason: "manual_refine",
		IdempotencyKey: "curation-supersede-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: 5 * time.Minute,
		IdempotencyKey: "curation-supersede-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(MemoryCurationPlanDraft{
		Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1,
		Actions: []MemoryCurationPlanAction{{
			Ordinal: 1, Operation: MemoryCurationOperationSupersede,
			Supersede: &MemoryCurationSupersedeAction{
				Target: MemoryCurationTargetReference{
					MemoryID: source.Memory.ID, ExpectedVersion: source.Memory.Version,
				},
				Replacements: []MemoryCurationVersionReference{{
					MemoryID: replacement.Memory.ID, Version: replacement.Memory.Version,
				}},
				Reason: "consolidated by the client curator",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := st.PlanCuration(ctx, p, started.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration, Draft: raw,
		IdempotencyKey: "curation-supersede-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
		IdempotencyKey: "curation-supersede-apply",
	})
	if err != nil {
		t.Fatal(err)
	}
	superseded, err := st.GetMemory(ctx, p, source.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if superseded.Version != 2 || superseded.State != MemoryStateSuperseded ||
		superseded.SupersessionSetID == "" ||
		superseded.SupersessionReplacementCount != 1 {
		t.Fatalf("curated supersession = %#v", superseded)
	}
	producedHeads, _ := memoryCurationProducedHeads(applied.Receipt.ActionResults)
	rolledBack, err := st.RollbackCuration(ctx, p, started.Run.ID, RollbackMemoryCurationInput{
		ApplyReceiptID: applied.Receipt.ID, ExpectedProducedHeads: producedHeads,
		IdempotencyKey: "curation-supersede-rollback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Run.State != MemoryCurationRunRolledBack {
		t.Fatalf("supersession rollback run = %#v", rolledBack.Run)
	}
	restored, err := st.GetMemory(ctx, p, source.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Version != 3 || restored.State != MemoryStateActive ||
		restored.Operation != "reverted" || restored.Content != source.Memory.Content {
		t.Fatalf("rolled-back supersession = %#v", restored)
	}
	var activeRelations int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_relations
		WHERE curation_run_id=$1 AND relation_type='supersedes'
		  AND reverted_at IS NULL`, started.Run.ID).Scan(&activeRelations); err != nil {
		t.Fatal(err)
	}
	if activeRelations != 0 {
		t.Fatalf("active supersession relations after rollback = %d", activeRelations)
	}
	unchangedReplacement, err := st.GetMemory(ctx, p, replacement.Memory.ID)
	if err != nil || unchangedReplacement.Version != 1 || unchangedReplacement.State != MemoryStateActive {
		t.Fatalf("replacement changed by rollback = %#v / %v", unchangedReplacement, err)
	}
}

func prepareMemoryCurationConflictPlan(
	ctx context.Context,
	t *testing.T,
	dsn string,
	withReplace bool,
) (*Store, Principal, MemoryMutationResult, StartMemoryCurationResult, PlanMemoryCurationResult, string) {
	t.Helper()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := provisionMemoryCurationApplyPrincipal(ctx, t, st)
	source, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "Frozen source memory.", Kind: "decision",
		Evidence: []MemoryEvidenceInput{{
			Type: "system", ResolutionState: MemoryEvidenceUnavailable,
			TerminalReasonCode: "not_recorded",
		}},
		IdempotencyKey: "curation-conflict-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		CoalescingKey: "conflict_test", TriggerReason: "manual_refine",
		IdempotencyKey: "curation-conflict-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: 5 * time.Minute,
		IdempotencyKey: "curation-conflict-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := []MemoryCurationPlanAction{{
		Ordinal: 1, Operation: MemoryCurationOperationCreate,
		Create: &MemoryCurationCreateAction{
			LocalRef: "summary",
			Snapshot: MemoryCurationMemorySnapshot{
				Content: "Curated output that must remain atomic.", Kind: "summary",
				Evidence: []MemoryCurationEvidence{{
					Type: "memory", ResolutionState: MemoryEvidenceResolved,
					ResolvedKind: MemoryCurationSourceMemory,
					SourceMemory: &MemoryCurationVersionReference{
						MemoryID: source.Memory.ID, Version: source.Memory.Version,
					},
				}},
			},
		},
	}}
	if withReplace {
		actions = append(actions, MemoryCurationPlanAction{
			Ordinal: 2, Operation: MemoryCurationOperationReplace,
			Replace: &MemoryCurationReplaceAction{
				Target: MemoryCurationTargetReference{
					MemoryID: source.Memory.ID, ExpectedVersion: source.Memory.Version,
				},
				Snapshot: MemoryCurationMemorySnapshot{
					Content: "Curated replacement.", Kind: "decision",
				},
			},
		})
	}
	raw, err := json.Marshal(MemoryCurationPlanDraft{
		Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1, Actions: actions,
	})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := st.PlanCuration(ctx, p, started.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration, Draft: raw,
		IdempotencyKey: "curation-conflict-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, p, source, started, planned, planned.PreallocatedMemoryIDs[0].MemoryID
}

func provisionMemoryCurationApplyPrincipal(ctx context.Context, t *testing.T, st *Store) Principal {
	t.Helper()
	provisioned, err := st.ProvisionAccount(ctx,
		"memory-curation-apply@witwave.ai", "memory curation apply", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "primary")
	if err != nil {
		t.Fatal(err)
	}
	return Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AccountStatus: "active"}
}
