package store

import (
	"context"
	"testing"

	"github.com/witwave-ai/witself/internal/testenv"
)

// TestMemoryCurationConflictReasonsPostgres pins, through PlanCuration and
// GetCurationPlan, that a refused plan names its rule, the action and the
// evidence row; that a refused plan stores nothing, so the run stays open for
// a corrected plan; and the two run-state reasons (issue #608).
func TestMemoryCurationConflictReasonsPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	fixture := newMemoryCurationPlanFixture(ctx, t, st, "conflict-reason", false, 2)
	run := fixture.Started.Run
	first, second := fixture.Entries[0], fixture.Entries[1]

	_, err := st.GetCurationPlan(ctx, fixture.Principal, run.ID, run.FencingGeneration)
	requireMemoryCurationConflictReason(t, "plan read of an open run", err, "run_not_planned", 0, -1)

	direct := func(from, until int64) MemoryCurationEvidence {
		return MemoryCurationEvidence{
			Type: "conversation", ResolutionState: MemoryEvidenceResolved, ResolvedKind: "transcript",
			SourceTranscriptID: first.TranscriptID, SourceSequenceFrom: from, SourceSequenceUntil: until,
		}
	}
	// The second action's second evidence row reaches one entry past the
	// frozen transcript window.
	uncovered := MemoryCurationPlanDraft{
		Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1,
		Actions: []MemoryCurationPlanAction{
			{Ordinal: 1, Operation: MemoryCurationOperationCreate, Create: &MemoryCurationCreateAction{
				LocalRef: "first", Snapshot: MemoryCurationMemorySnapshot{
					Content: "first decision", Kind: "decision",
					Evidence: []MemoryCurationEvidence{direct(first.Sequence, first.Sequence)},
				},
			}},
			{Ordinal: 2, Operation: MemoryCurationOperationCreate, Create: &MemoryCurationCreateAction{
				LocalRef: "second", Snapshot: MemoryCurationMemorySnapshot{
					Content: "second decision", Kind: "decision",
					Evidence: []MemoryCurationEvidence{
						direct(second.Sequence, second.Sequence),
						direct(first.Sequence, second.Sequence+1),
					},
				},
			}},
		},
	}
	_, err = st.PlanCuration(ctx, fixture.Principal, run.ID, PlanMemoryCurationInput{
		FencingGeneration: run.FencingGeneration,
		Draft:             marshalCurationPlanDraft(t, uncovered, false),
		IdempotencyKey:    "conflict-reason-uncovered",
	})
	requireMemoryCurationConflictReason(t, "uncovered transcript range", err,
		"transcript_range_not_covered", 2, 1)

	current, err := st.GetCurationRun(ctx, fixture.Principal, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != MemoryCurationRunOpen || current.PlanRevision != 0 {
		t.Fatalf("run after a refused plan: state %q revision %d, want open and 0",
			current.State, current.PlanRevision)
	}
	empty := marshalCurationPlanDraft(t, MemoryCurationPlanDraft{
		Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1, Actions: []MemoryCurationPlanAction{},
	}, false)
	if _, err := st.PlanCuration(ctx, fixture.Principal, run.ID, PlanMemoryCurationInput{
		FencingGeneration: run.FencingGeneration, Draft: empty,
		IdempotencyKey: "conflict-reason-empty",
	}); err != nil {
		t.Fatalf("corrected plan after a refusal: %v", err)
	}
	_, err = st.PlanCuration(ctx, fixture.Principal, run.ID, PlanMemoryCurationInput{
		FencingGeneration: run.FencingGeneration, Draft: empty,
		IdempotencyKey: "conflict-reason-second",
	})
	requireMemoryCurationConflictReason(t, "second plan", err, "run_not_open", 0, -1)
}
