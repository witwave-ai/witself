package store

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

// TestMemoryCurationAutomaticQuietPeriodPostgres pins when automatic curation
// work becomes due around an apply (issue #627). An agent's own session keeps
// appending transcript entries, so after an automatic apply the next automatic
// request, whether a fresh source trigger or the apply's follow-up, waits 30
// minutes; later source commits coalesce into it without making it due sooner;
// once the quiet period has passed a source commit makes it due at once; an
// explicit request is due at once throughout; and applying an explicit run
// starts no quiet period.
func TestMemoryCurationAutomaticQuietPeriodPostgres(t *testing.T) {
	ctx, st, p := newMemoryCurationAccessProfileStore(t, testenv.RequirePostgres(t))
	// A literal, so this test also fails on a store without the quiet period.
	const quiet = 30 * time.Minute
	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "quiet-period-thread"})
	if err != nil {
		t.Fatal(err)
	}
	turns := 0
	appendTurn := func() {
		t.Helper()
		turns++
		if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
			transcript.ID, AppendTranscriptEntryInput{
				ExternalID: fmt.Sprintf("quiet-turn-%d", turns),
				Role:       TranscriptRoleUser, Body: fmt.Sprintf("Session turn %d.", turns),
			}); err != nil {
			t.Fatal(err)
		}
	}
	// openAutomatic returns the one open request on the reserved automatic lane.
	openAutomatic := func() MemoryCurationRequest {
		t.Helper()
		var requestID string
		if err := st.pool.QueryRow(ctx, `
			SELECT id FROM memory_curation_requests
			WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
			  AND coalescing_key=$4 AND state IN ('queued','claimed','retry_wait')`,
			p.AccountID, p.RealmID, p.ID, automaticMemoryCurationCoalescingKey).
			Scan(&requestID); err != nil {
			t.Fatalf("open automatic request: %v", err)
		}
		request, err := st.GetCurationRequest(ctx, p, requestID)
		if err != nil {
			t.Fatal(err)
		}
		return request
	}
	// dueIDs is the listing the self memory_checkpoint selects from when no
	// run is active (projectSelfMemoryCheckpoint in cmd/witself-server).
	dueIDs := func() []string {
		t.Helper()
		page, err := st.ListCurationRequests(ctx, p, MemoryCurationRequestListOptions{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(page.Requests))
		for _, request := range page.Requests {
			ids = append(ids, request.ID)
		}
		return ids
	}
	// curate applies an empty plan to one request. during runs while the run
	// is open, the way a session keeps appending while its agent curates.
	curate := func(request MemoryCurationRequest, label string, during func()) ApplyMemoryCurationResult {
		t.Helper()
		started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
			RequestID: request.ID, LeaseDuration: time.Minute, IdempotencyKey: label + "-start",
		})
		if err != nil {
			t.Fatalf("%s start: %v", label, err)
		}
		if during != nil {
			during()
		}
		planned, err := st.PlanCuration(ctx, p, started.Run.ID, PlanMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			Draft:             marshalEmptyCurationPlanForAccessProfile(t),
			IdempotencyKey:    label + "-plan",
		})
		if err != nil {
			t.Fatalf("%s plan: %v", label, err)
		}
		applied, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
			IdempotencyKey: label + "-apply",
		})
		if err != nil || applied.Run.AppliedAt == nil {
			t.Fatalf("%s apply: %v", label, err)
		}
		return applied
	}
	nearQuiet := func(d time.Duration) bool {
		return d >= quiet-time.Minute && d <= quiet+time.Minute
	}

	// Before any automatic apply, a source commit is due at once, as before.
	appendTurn()
	first := openAutomatic()
	if !slices.Contains(dueIDs(), first.ID) {
		t.Fatal("the first automatic request is not due at once")
	}
	firstApplied := curate(first, "quiet-first", nil)
	if firstApplied.FollowUpRequest != nil {
		t.Fatalf("an apply that reviewed everything queued a %q follow-up",
			firstApplied.FollowUpRequest.TriggerReason)
	}

	// The session goes on: the next source commit queues automatic work that
	// waits for the quiet period, so no checkpoint is pending.
	appendTurn()
	second := openAutomatic()
	if len(dueIDs()) != 0 {
		t.Fatal("automatic work became due inside the quiet period after an apply")
	}
	if d := second.DueAt.Sub(*firstApplied.Run.AppliedAt); !nearQuiet(d) {
		t.Fatalf("automatic request is due %s after the apply, want %s", d, quiet)
	}
	if _, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: second.ID, LeaseDuration: time.Minute, IdempotencyKey: "quiet-early-start",
	}); !errors.Is(err, ErrMemoryCurationNotDue) {
		t.Fatalf("start inside the quiet period = %v, want not due", err)
	}

	// Once the quiet period has passed, a source commit makes the waiting
	// request due at once.
	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_curation_runs SET applied_at=applied_at-interval '31 minutes'
		WHERE id=$1`, firstApplied.Run.ID); err != nil {
		t.Fatal(err)
	}
	appendTurn()
	if due := dueIDs(); len(due) != 1 || due[0] != second.ID {
		t.Fatalf("due requests after the quiet period = %d, want the waiting automatic request", len(due))
	}

	// A run whose session keeps appending queues a source_backlog follow-up
	// that waits for the quiet period after this apply.
	secondApplied := curate(openAutomatic(), "quiet-second", appendTurn)
	followUp := secondApplied.FollowUpRequest
	if followUp == nil {
		t.Fatal("an apply whose session kept appending queued no follow-up")
	}
	if followUp.CoalescingKey != automaticMemoryCurationCoalescingKey ||
		followUp.TriggerReason != memoryCurationSourceBacklogTrigger ||
		followUp.State != MemoryCurationRequestQueued {
		t.Fatalf("automatic follow-up key=%q trigger=%q state=%q",
			followUp.CoalescingKey, followUp.TriggerReason, followUp.State)
	}
	if d := followUp.DueAt.Sub(*secondApplied.Run.AppliedAt); !nearQuiet(d) {
		t.Fatalf("automatic follow-up is due %s after the apply, want %s", d, quiet)
	}
	if len(dueIDs()) != 0 {
		t.Fatal("the automatic follow-up is due inside the quiet period")
	}

	// More session activity coalesces into the waiting follow-up without
	// making it due sooner.
	appendTurn()
	waiting := openAutomatic()
	if waiting.ID != followUp.ID || waiting.RequestGeneration <= followUp.RequestGeneration {
		t.Fatalf("source commit did not coalesce into the follow-up: same=%t generation %d -> %d",
			waiting.ID == followUp.ID, followUp.RequestGeneration, waiting.RequestGeneration)
	}
	if !waiting.DueAt.Equal(followUp.DueAt) || len(dueIDs()) != 0 {
		t.Fatal("a source commit made the automatic follow-up due inside the quiet period")
	}

	// An explicit request is due at once throughout.
	explicit, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceTranscript}},
		CoalescingKey: "quiet_explicit", TriggerReason: "manual_refine",
		IdempotencyKey: "quiet-explicit-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if due := dueIDs(); len(due) != 1 || due[0] != explicit.Request.ID {
		t.Fatalf("due requests = %d, want only the explicit request", len(due))
	}

	// Applying an explicit run starts no quiet period: once the automatic one
	// has passed, a source commit right after an explicit apply still makes
	// the waiting automatic request due at once.
	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_curation_runs SET applied_at=applied_at-interval '31 minutes'
		WHERE id=$1`, secondApplied.Run.ID); err != nil {
		t.Fatal(err)
	}
	curate(explicit.Request, "quiet-explicit", nil)
	appendTurn()
	if !slices.Contains(dueIDs(), waiting.ID) {
		t.Fatal("an explicit apply started a quiet period for automatic work")
	}
}
