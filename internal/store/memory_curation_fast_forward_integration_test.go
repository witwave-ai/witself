package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

func setupFastForwardCurationTest(t *testing.T, name string) (context.Context, *Store, Principal) {
	t.Helper()
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		name+"@witwave.ai", "curation fast-forward", time.Hour)
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
	return ctx, st, Principal{Kind: PrincipalAgent, ID: agent.ID,
		AccountID: provisioned.AccountID, RealmID: realm.ID, AccountStatus: "active"}
}

// appendFastForwardEntries writes entryCount tiny entries whose payload kind
// comes from classify(sequence); an empty classification stores no payload.
func appendFastForwardEntries(ctx context.Context, t *testing.T, st *Store, p Principal, transcriptID string, entryCount int64, classify func(int64) string) {
	t.Helper()
	for i := int64(1); i <= entryCount; i++ {
		input := AppendTranscriptEntryInput{
			ExternalID: fmt.Sprintf("turn-%d", i),
			Role:       TranscriptRoleAssistant, Body: "x",
		}
		if kind := classify(i); kind != "" {
			input.Payload = json.RawMessage(fmt.Sprintf(`{"kind":%q}`, kind))
		}
		if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
			transcriptID, input); err != nil {
			t.Fatal(err)
		}
	}
}

type fastForwardRunView struct {
	coverage    []MemoryCurationRunInput
	transcripts []MemoryCurationRunInput
	cursors     []MemoryCurationRunInput
}

func pageFastForwardRun(ctx context.Context, t *testing.T, st *Store, p Principal, run MemoryCurationRun) fastForwardRunView {
	t.Helper()
	view := fastForwardRunView{}
	cursor := ""
	for {
		page, err := st.GetCurationRunInputs(ctx, p, run.ID, run.FencingGeneration,
			cursor, maxMemoryCurationPageSize)
		if err != nil {
			t.Fatalf("page inputs: %v", err)
		}
		for _, input := range page.Inputs {
			switch input.Kind {
			case MemoryCurationInputTranscriptCoverage:
				view.coverage = append(view.coverage, input)
			case MemoryCurationInputTranscript:
				view.transcripts = append(view.transcripts, input)
			case MemoryCurationInputCursor:
				if input.CursorSourceKind == MemoryCurationSourceTranscript {
					view.cursors = append(view.cursors, input)
				}
			}
		}
		if page.NextCursor == "" {
			return view
		}
		cursor = page.NextCursor
	}
}

func applyEmptyFastForwardPlan(ctx context.Context, t *testing.T, st *Store, p Principal, run MemoryCurationRun, label string) {
	t.Helper()
	planned, err := st.PlanCuration(ctx, p, run.ID, PlanMemoryCurationInput{
		FencingGeneration: run.FencingGeneration,
		Draft:             marshalEmptyCurationPlanForAccessProfile(t),
		IdempotencyKey:    label + "-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyCuration(ctx, p, run.ID, ApplyMemoryCurationInput{
		FencingGeneration: run.FencingGeneration,
		PlanRevision:      planned.Plan.PlanRevision,
		PlanHash:          planned.Receipt.PlanHash,
		IdempotencyKey:    label + "-apply",
	}); err != nil {
		t.Fatal(err)
	}
}

func fastForwardCursorPosition(ctx context.Context, t *testing.T, st *Store, p Principal, transcriptID string) int64 {
	t.Helper()
	var position int64
	if err := st.pool.QueryRow(ctx, `
		SELECT position FROM memory_curation_cursors
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND source_kind='transcript' AND source_stream_id=$4`,
		p.AccountID, p.RealmID, p.ID, transcriptID).Scan(&position); err != nil {
		t.Fatal(err)
	}
	return position
}

// TestMemoryCurationFastForwardDrainsObservationalBacklogPostgres freezes a
// window past the fast-forward threshold whose entries are almost all
// tool-event noise and proves the run drains it in one cycle: signal entries
// materialize individually, one value-free coverage input freezes the window
// and its per-class counts, and applying an empty plan advances the stream
// cursor to the head.
func TestMemoryCurationFastForwardDrainsObservationalBacklogPostgres(t *testing.T) {
	ctx, st, p := setupFastForwardCurationTest(t, "curation-fast-forward-drain")
	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "fast-forward-drain"})
	if err != nil {
		t.Fatal(err)
	}
	const entryCount = int64(2100)
	signalAt := map[int64]bool{500: true, 1000: true, 2050: true}
	appendFastForwardEntries(ctx, t, st, p, transcript.ID, entryCount, func(i int64) string {
		switch {
		case i == 500 || i == 1000:
			return "message.user"
		case i == 2050:
			return "" // no payload kind at all: unknown means signal
		case i%2 == 1:
			return "tool.call"
		default:
			return "tool.result"
		}
	})

	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceTranscript}},
		TriggerReason: "manual_refine", IdempotencyKey: "fast-forward-drain-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "fast-forward-drain-start",
	})
	if err != nil {
		t.Fatal(err)
	}

	view := pageFastForwardRun(ctx, t, st, p, started.Run)
	if len(view.coverage) != 1 {
		t.Fatalf("coverage inputs = %d, want 1", len(view.coverage))
	}
	coverage := view.coverage[0]
	if coverage.TranscriptID != transcript.ID || coverage.SequenceFrom != 1 ||
		coverage.SequenceUntil != entryCount {
		t.Fatalf("coverage window = %s:%d-%d", coverage.TranscriptID,
			coverage.SequenceFrom, coverage.SequenceUntil)
	}
	if coverage.CoverageCounts == nil ||
		coverage.CoverageCounts.ToolCalls != 1050 ||
		coverage.CoverageCounts.ToolResults != 1047 ||
		coverage.CoverageCounts.Signal != 3 {
		t.Fatalf("coverage counts = %+v", coverage.CoverageCounts)
	}
	if len(view.transcripts) != len(signalAt) {
		t.Fatalf("signal inputs = %d, want %d", len(view.transcripts), len(signalAt))
	}
	for _, input := range view.transcripts {
		if input.SequenceFrom != input.SequenceUntil || !signalAt[input.SequenceFrom] {
			t.Fatalf("unexpected signal input window %d-%d", input.SequenceFrom, input.SequenceUntil)
		}
		if len(input.TranscriptEntries) != 1 ||
			input.TranscriptEntries[0].Sequence != input.SequenceFrom {
			t.Fatalf("signal input %d hydrated %d entries", input.SequenceFrom,
				len(input.TranscriptEntries))
		}
	}
	if len(view.cursors) != 1 || view.cursors[0].CursorExpectedPrior != 0 ||
		view.cursors[0].CursorUpper != entryCount {
		t.Fatalf("cursor inputs = %+v", view.cursors)
	}

	applyEmptyFastForwardPlan(ctx, t, st, p, started.Run, "fast-forward-drain")
	if position := fastForwardCursorPosition(ctx, t, st, p, transcript.ID); position != entryCount {
		t.Fatalf("transcript cursor = %d, want %d", position, entryCount)
	}
}

// TestMemoryCurationFastForwardSplitsAtSignalBudgetPostgres proves a window
// with more signal entries than the run's entry budget splits before the
// first signal entry that no longer fits: the frozen window ends just below
// it, exactly the budgeted signal entries materialize, and the cursor
// advances to the split point.
func TestMemoryCurationFastForwardSplitsAtSignalBudgetPostgres(t *testing.T) {
	ctx, st, p := setupFastForwardCurationTest(t, "curation-fast-forward-split")
	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "fast-forward-split"})
	if err != nil {
		t.Fatal(err)
	}
	const entryCount = int64(2600)
	appendFastForwardEntries(ctx, t, st, p, transcript.ID, entryCount, func(i int64) string {
		switch {
		case i%5 == 0:
			return "message.assistant"
		case i%2 == 1:
			return "tool.call"
		default:
			return "tool.result"
		}
	})

	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceTranscript}},
		TriggerReason: "manual_refine", IdempotencyKey: "fast-forward-split-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "fast-forward-split-start",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The 501st signal entry sits at sequence 2505, so the frozen window must
	// end at 2504 with exactly 500 materialized signal entries.
	const splitUpper = int64(2504)
	view := pageFastForwardRun(ctx, t, st, p, started.Run)
	if len(view.coverage) != 1 || view.coverage[0].SequenceFrom != 1 ||
		view.coverage[0].SequenceUntil != splitUpper {
		t.Fatalf("coverage inputs = %+v", view.coverage)
	}
	counts := view.coverage[0].CoverageCounts
	if counts == nil || counts.ToolCalls != 1002 || counts.ToolResults != 1002 ||
		counts.Signal != 500 {
		t.Fatalf("coverage counts = %+v", counts)
	}
	if len(view.transcripts) != 500 {
		t.Fatalf("signal inputs = %d, want 500", len(view.transcripts))
	}
	for i, input := range view.transcripts {
		if want := int64(i+1) * 5; input.SequenceFrom != want ||
			input.SequenceUntil != want {
			t.Fatalf("signal input %d window = %d-%d, want %d", i,
				input.SequenceFrom, input.SequenceUntil, want)
		}
	}
	if len(view.cursors) != 1 || view.cursors[0].CursorUpper != splitUpper {
		t.Fatalf("cursor inputs = %+v", view.cursors)
	}

	applyEmptyFastForwardPlan(ctx, t, st, p, started.Run, "fast-forward-split")
	if position := fastForwardCursorPosition(ctx, t, st, p, transcript.ID); position != splitUpper {
		t.Fatalf("transcript cursor = %d, want %d", position, splitUpper)
	}
}
