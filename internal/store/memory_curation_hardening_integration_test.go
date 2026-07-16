package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestMemoryCurationCapBacklogQueuesFollowUpPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	for _, sourceKind := range []string{
		MemoryCurationSourceMemory,
		MemoryCurationSourceEvidence,
		MemoryCurationSourceTranscript,
	} {
		t.Run(sourceKind, func(t *testing.T) {
			ctx, st, p := newMemoryCurationHardeningStore(t, dsn)
			scope := MemoryCurationScope{
				Sources:      []string{sourceKind},
				MemoryStates: []string{MemoryStateActive},
				MaxMemories:  1, MaxEvidence: 1, MaxTranscriptEntries: 1,
			}
			switch sourceKind {
			case MemoryCurationSourceMemory, MemoryCurationSourceEvidence:
				captureHardeningMemory(ctx, t, st, p, "first capped source", "cap-source-first")
				captureHardeningMemory(ctx, t, st, p, "second capped source", "cap-source-second")
			case MemoryCurationSourceTranscript:
				transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
					CreateTranscriptInput{ExternalID: "cap-transcript"})
				if err != nil {
					t.Fatal(err)
				}
				for index := 1; index <= 2; index++ {
					if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
						transcript.ID, AppendTranscriptEntryInput{
							ExternalID: "cap-entry-" + string(rune('0'+index)),
							Role:       TranscriptRoleUser, Body: "capped transcript entry",
						}); err != nil {
						t.Fatal(err)
					}
				}
			}
			requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
				Scope: scope, CoalescingKey: "cap_" + sourceKind,
				TriggerReason: "manual_refine", IdempotencyKey: "cap-request-" + sourceKind,
			})
			if err != nil {
				t.Fatal(err)
			}
			started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
				RequestID: requested.Request.ID, LeaseDuration: time.Minute,
				Caps: MemoryCurationInputCaps{
					MaxMemories: 1, MaxEvidence: 1, MaxTranscriptEntries: 1,
				},
				IdempotencyKey: "cap-start-" + sourceKind,
			})
			if err != nil {
				t.Fatal(err)
			}
			firstPage, err := st.GetCurationRunInputs(ctx, p, started.Run.ID,
				started.Run.FencingGeneration, "", 50)
			if err != nil {
				t.Fatal(err)
			}
			if countCurationInputKind(firstPage.Inputs, sourceKind) != 1 {
				t.Fatalf("first %s inputs = %#v", sourceKind, firstPage.Inputs)
			}
			planned := planHardeningCuration(ctx, t, st, p, started, nil,
				"cap-plan-"+sourceKind)
			applied, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
				FencingGeneration: started.Run.FencingGeneration,
				PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
				IdempotencyKey: "cap-apply-" + sourceKind,
			})
			if err != nil {
				t.Fatal(err)
			}
			if applied.FollowUpRequest == nil ||
				applied.FollowUpRequest.TriggerReason != "source_backlog" ||
				applied.FollowUpRequest.RequestGeneration <= started.Run.RequestGeneration {
				t.Fatalf("cap follow-up = %#v", applied.FollowUpRequest)
			}
			followStarted, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
				RequestID: applied.FollowUpRequest.ID, LeaseDuration: time.Minute,
				Caps: MemoryCurationInputCaps{
					MaxMemories: 1, MaxEvidence: 1, MaxTranscriptEntries: 1,
				},
				IdempotencyKey: "cap-follow-start-" + sourceKind,
			})
			if err != nil {
				t.Fatal(err)
			}
			secondPage, err := st.GetCurationRunInputs(ctx, p, followStarted.Run.ID,
				followStarted.Run.FencingGeneration, "", 50)
			if err != nil {
				t.Fatal(err)
			}
			if countCurationInputKind(secondPage.Inputs, sourceKind) != 1 {
				t.Fatalf("follow-up %s inputs = %#v", sourceKind, secondPage.Inputs)
			}
		})
	}
}

func TestMemoryCurationTranscriptRunFreezesExistingMemoryContextPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, st, p := newMemoryCurationHardeningStore(t, dsn)
	existing := captureHardeningMemory(ctx, t, st, p,
		"The client chose PostgreSQL as the portable memory source of truth.",
		"transcript-context-existing")
	for index := 0; index < maxMemoryCurationContextMemories+1; index++ {
		captureHardeningMemoryWithSalience(ctx, t, st, p,
			fmt.Sprintf("Elevated unrelated narrative %02d about watercolor pigments and hiking boots.", index),
			fmt.Sprintf("transcript-context-unrelated-%02d", index), 1)
	}

	// First review the memory source so the next run has no changed-memory
	// input. The existing head should still be frozen as comparison context
	// when a later transcript delta is reviewed.
	baselineRequest, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: MemoryCurationScope{
			Sources: []string{MemoryCurationSourceMemory},
		},
		CoalescingKey: "transcript_context_baseline", TriggerReason: "manual_refine",
		IdempotencyKey: "transcript-context-baseline-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	baselineRun, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: baselineRequest.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "transcript-context-baseline-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	baselinePlan := planHardeningCuration(ctx, t, st, p, baselineRun, nil,
		"transcript-context-baseline-plan")
	if _, err := st.ApplyCuration(ctx, p, baselineRun.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: baselineRun.Run.FencingGeneration,
		PlanRevision:      baselinePlan.Plan.PlanRevision,
		PlanHash:          baselinePlan.Receipt.PlanHash,
		IdempotencyKey:    "transcript-context-baseline-apply",
	}); err != nil {
		t.Fatal(err)
	}

	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "transcript-context-thread"})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
		transcript.ID, AppendTranscriptEntryInput{
			ExternalID: "transcript-context-turn", Role: TranscriptRoleUser,
			Body: "We should refine that PostgreSQL database decision as the architecture evolves.",
		})
	if err != nil {
		t.Fatal(err)
	}
	request, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: MemoryCurationScope{
			Sources:              []string{MemoryCurationSourceMemory, MemoryCurationSourceTranscript},
			MaxMemories:          maxMemoryCurationContextMemories,
			MaxTranscriptEntries: 10,
		},
		CoalescingKey: "transcript_context_review", TriggerReason: "manual_refine",
		IdempotencyKey: "transcript-context-review-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: request.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "transcript-context-review-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := st.GetCurationRunInputs(ctx, p, started.Run.ID,
		started.Run.FencingGeneration, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if countCurationInputKind(page.Inputs, MemoryCurationInputTranscript) != 1 {
		t.Fatalf("transcript inputs = %#v", page.Inputs)
	}
	if countCurationInputKind(page.Inputs, MemoryCurationInputMemory) != maxMemoryCurationContextMemories {
		t.Fatalf("comparison memory inputs = %#v", page.Inputs)
	}
	if !containsCurationMemoryInput(page.Inputs, existing.Memory.ID, existing.Memory.Version) {
		t.Fatalf("existing memory was not frozen as transcript comparison context: %#v", page.Inputs)
	}
	const refinedContent = "PostgreSQL remains the portable source of truth while client inference refines narrative memory."
	planned := planHardeningCuration(ctx, t, st, p, started, []MemoryCurationPlanAction{{
		Ordinal: 1, Operation: MemoryCurationOperationReplace,
		Replace: &MemoryCurationReplaceAction{
			Target: MemoryCurationTargetReference{
				MemoryID: existing.Memory.ID, ExpectedVersion: existing.Memory.Version,
			},
			Snapshot: MemoryCurationMemorySnapshot{
				Content: refinedContent, Kind: "decision",
				Evidence: []MemoryCurationEvidence{{
					Type: "conversation", ResolutionState: MemoryEvidenceResolved,
					ResolvedKind:       MemoryCurationSourceTranscript,
					SourceTranscriptID: transcript.ID,
					SourceSequenceFrom: entry.Sequence, SourceSequenceUntil: entry.Sequence,
				}},
			},
			Reason: "refine an existing narrative from later conversation evidence",
		},
	}}, "transcript-context-review-plan")
	if _, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		PlanRevision:      planned.Plan.PlanRevision,
		PlanHash:          planned.Receipt.PlanHash,
		IdempotencyKey:    "transcript-context-review-apply",
	}); err != nil {
		t.Fatal(err)
	}
	refined, err := st.GetMemory(ctx, p, existing.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refined.Version != existing.Memory.Version+1 || refined.Content != refinedContent {
		t.Fatalf("refined context memory = %#v", refined)
	}
}

func TestMemoryCurationTranscriptContextRespectsMemoryStatesPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, st, p := newMemoryCurationHardeningStore(t, dsn)
	active := captureHardeningMemory(ctx, t, st, p,
		"Scopequasar belongs to the active comparison head.", "context-state-active")
	forgotten := captureHardeningMemory(ctx, t, st, p,
		"Scopequasar belongs to the forgotten comparison head.", "context-state-forgotten")
	forgotten, err := st.ForgetMemory(ctx, p, forgotten.Memory.ID, MemoryLifecycleInput{
		ExpectedVersion: forgotten.Memory.Version,
		Reason:          "exercise comparison state filtering",
		IdempotencyKey:  "context-state-forget",
	})
	if err != nil {
		t.Fatal(err)
	}

	baselineRequest, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: MemoryCurationScope{
			Sources:      []string{MemoryCurationSourceMemory},
			MemoryStates: []string{MemoryStateForgotten},
		},
		CoalescingKey: "context_state_baseline", TriggerReason: "manual_refine",
		IdempotencyKey: "context-state-baseline-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	baselineRun, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: baselineRequest.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "context-state-baseline-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	baselinePlan := planHardeningCuration(ctx, t, st, p, baselineRun, nil,
		"context-state-baseline-plan")
	if _, err := st.ApplyCuration(ctx, p, baselineRun.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: baselineRun.Run.FencingGeneration,
		PlanRevision:      baselinePlan.Plan.PlanRevision,
		PlanHash:          baselinePlan.Receipt.PlanHash,
		IdempotencyKey:    "context-state-baseline-apply",
	}); err != nil {
		t.Fatal(err)
	}

	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "context-state-thread"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
		transcript.ID, AppendTranscriptEntryInput{
			ExternalID: "context-state-turn", Role: TranscriptRoleUser,
			Body: "Scopequasar is the topic for this comparison.",
		}); err != nil {
		t.Fatal(err)
	}
	request, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: MemoryCurationScope{
			Sources:              []string{MemoryCurationSourceMemory, MemoryCurationSourceTranscript},
			MemoryStates:         []string{MemoryStateForgotten},
			MaxMemories:          1,
			MaxTranscriptEntries: 10,
		},
		CoalescingKey: "context_state_review", TriggerReason: "manual_refine",
		IdempotencyKey: "context-state-review-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: request.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "context-state-review-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := st.GetCurationRunInputs(ctx, p, started.Run.ID,
		started.Run.FencingGeneration, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if !containsCurationMemoryInput(page.Inputs, forgotten.Memory.ID, forgotten.Memory.Version) {
		t.Fatalf("forgotten in-scope context was not frozen: %#v", page.Inputs)
	}
	if containsCurationMemoryInput(page.Inputs, active.Memory.ID, active.Memory.Version) {
		t.Fatalf("active out-of-scope context was frozen: %#v", page.Inputs)
	}
}

func TestMemoryCurationTranscriptContextRespectsOwnerAndSensitivityPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, st, p := newMemoryCurationHardeningStore(t, dsn)
	public := captureHardeningMemory(ctx, t, st, p,
		"Privacyscope identifies the public same-owner memory.", "context-scope-public")
	sensitiveSalience := 1.0
	sensitive, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "Privacyscope identifies the private same-owner memory.",
		Kind:    "decision", Salience: &sensitiveSalience, Sensitive: true,
		Evidence: []MemoryEvidenceInput{{
			Type: "system", ResolutionState: MemoryEvidenceUnavailable,
			TerminalReasonCode: "not_recorded",
		}},
		IdempotencyKey: "context-scope-sensitive",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Advance the non-sensitive filtered memory cursor. The later transcript run
	// must discover the older public head as comparison context without admitting
	// the sensitive head that this cursor intentionally skipped.
	baselineRequest, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: MemoryCurationScope{
			Sources: []string{MemoryCurationSourceMemory}, IncludeSensitive: false,
		},
		CoalescingKey: "context_scope_baseline", TriggerReason: "manual_refine",
		IdempotencyKey: "context-scope-baseline-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	baselineRun, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: baselineRequest.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "context-scope-baseline-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	baselinePlan := planHardeningCuration(ctx, t, st, p, baselineRun, nil,
		"context-scope-baseline-plan")
	if _, err := st.ApplyCuration(ctx, p, baselineRun.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: baselineRun.Run.FencingGeneration,
		PlanRevision:      baselinePlan.Plan.PlanRevision,
		PlanHash:          baselinePlan.Receipt.PlanHash,
		IdempotencyKey:    "context-scope-baseline-apply",
	}); err != nil {
		t.Fatal(err)
	}

	otherAgent, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, "context-scope-other")
	if err != nil {
		t.Fatal(err)
	}
	otherPrincipal := p
	otherPrincipal.ID = otherAgent.ID
	other := captureHardeningMemoryWithSalience(ctx, t, st, otherPrincipal,
		"Privacyscope identifies a different owner's memory.", "context-scope-other-memory", 1)

	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "context-scope-thread"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
		transcript.ID, AppendTranscriptEntryInput{
			ExternalID: "context-scope-turn", Role: TranscriptRoleUser,
			Body: "Privacyscope is the topic to review.",
		}); err != nil {
		t.Fatal(err)
	}
	request, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: MemoryCurationScope{
			Sources:     []string{MemoryCurationSourceMemory, MemoryCurationSourceTranscript},
			MaxMemories: 3, MaxTranscriptEntries: 10,
		},
		CoalescingKey: "context_scope_review", TriggerReason: "manual_refine",
		IdempotencyKey: "context-scope-review-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: request.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "context-scope-review-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := st.GetCurationRunInputs(ctx, p, started.Run.ID,
		started.Run.FencingGeneration, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if !containsCurationMemoryInput(page.Inputs, public.Memory.ID, public.Memory.Version) {
		t.Fatalf("public same-owner context missing: %#v", page.Inputs)
	}
	if containsCurationMemoryInput(page.Inputs, sensitive.Memory.ID, sensitive.Memory.Version) ||
		containsCurationMemoryInput(page.Inputs, other.Memory.ID, other.Memory.Version) {
		t.Fatalf("out-of-scope context entered run: %#v", page.Inputs)
	}
}

func TestMemoryCurationTranscriptBudgetIsSharedAcrossPendingStreamsPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, st, p := newMemoryCurationHardeningStore(t, dsn)
	first, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "fair-transcript-first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "fair-transcript-second"})
	if err != nil {
		t.Fatal(err)
	}
	// IDs are crypto-random. Put the multi-entry backlog on the lower ID so the
	// former ORDER BY c.id implementation would consume the entire two-entry
	// budget before reaching the other stream.
	noisy, quiet := first, second
	if second.ID < first.ID {
		noisy, quiet = second, first
	}
	if _, err := st.AppendTranscriptEntries(ctx, p.AccountID, p.RealmID, p.ID,
		noisy.ID, []AppendTranscriptEntryInput{
			{ExternalID: "fair-noisy-1", Role: TranscriptRoleUser, Body: "First noisy turn."},
			{ExternalID: "fair-noisy-2", Role: TranscriptRoleAssistant, Body: "Second noisy turn."},
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
		quiet.ID, AppendTranscriptEntryInput{
			ExternalID: "fair-quiet-1", Role: TranscriptRoleUser, Body: "One quiet turn.",
		}); err != nil {
		t.Fatal(err)
	}
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: MemoryCurationScope{
			Sources: []string{MemoryCurationSourceTranscript}, MaxTranscriptEntries: 2,
		},
		CoalescingKey: "fair_transcript_budget", TriggerReason: "manual_refine",
		IdempotencyKey: "fair-transcript-budget-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "fair-transcript-budget-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := st.GetCurationRunInputs(ctx, p, started.Run.ID,
		started.Run.FencingGeneration, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	ranges := map[string]int64{}
	for _, input := range page.Inputs {
		if input.Kind == MemoryCurationInputTranscript {
			ranges[input.TranscriptID] = input.SequenceUntil - input.SequenceFrom + 1
		}
	}
	if len(ranges) != 2 || ranges[noisy.ID] != 1 || ranges[quiet.ID] != 1 {
		t.Fatalf("shared transcript ranges = %#v, want one entry from each stream", ranges)
	}
	planned := planHardeningCuration(ctx, t, st, p, started, nil,
		"fair-transcript-budget-plan")
	applied, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		PlanRevision:      planned.Plan.PlanRevision,
		PlanHash:          planned.Receipt.PlanHash,
		IdempotencyKey:    "fair-transcript-budget-apply",
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.FollowUpRequest == nil {
		t.Fatal("remaining noisy transcript entry did not queue a follow-up")
	}
	followUp, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: applied.FollowUpRequest.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "fair-transcript-budget-follow-up-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	followPage, err := st.GetCurationRunInputs(ctx, p, followUp.Run.ID,
		followUp.Run.FencingGeneration, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	followRanges := map[string]MemoryCurationRunInput{}
	for _, input := range followPage.Inputs {
		if input.Kind == MemoryCurationInputTranscript {
			followRanges[input.TranscriptID] = input
		}
	}
	remainingInput, ok := followRanges[noisy.ID]
	if len(followRanges) != 1 || !ok || remainingInput.SequenceFrom != 2 ||
		remainingInput.SequenceUntil != 2 {
		t.Fatalf("follow-up transcript ranges = %#v, want noisy sequence 2 only", followRanges)
	}
}

func TestMemoryCurationAutomaticRequestIsolationPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, st, p := newMemoryCurationHardeningStore(t, dsn)
	if _, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		CoalescingKey: automaticMemoryCurationCoalescingKey,
		TriggerReason: "manual_refine", IdempotencyKey: "reserved-auto-key",
	}); !errors.Is(err, ErrMemoryCurationInputInvalid) {
		t.Fatalf("reserved automatic key error = %v", err)
	}
	manual, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceMemory}},
		CoalescingKey: "owner", TriggerReason: "manual_refine",
		IdempotencyKey: "narrow-owner-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "automatic-isolation"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
		transcript.ID, AppendTranscriptEntryInput{
			ExternalID: "automatic-isolation-entry", Role: TranscriptRoleUser,
			Body: "This transcript source must not be absorbed by a narrowed request.",
		}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.pool.Query(ctx, `
		SELECT id,coalescing_key,scope::text FROM memory_curation_requests
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND state IN ('queued','claimed','retry_wait')
		ORDER BY coalescing_key`, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	keys := make(map[string]MemoryCurationScope)
	ids := make(map[string]string)
	for rows.Next() {
		var requestID, key string
		var raw []byte
		if err := rows.Scan(&requestID, &key, &raw); err != nil {
			t.Fatal(err)
		}
		var scope MemoryCurationScope
		if err := json.Unmarshal(raw, &scope); err != nil {
			t.Fatal(err)
		}
		keys[key], ids[key] = scope, requestID
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if ids["owner"] != manual.Request.ID ||
		!curationHasSource(keys["owner"], MemoryCurationSourceMemory) ||
		curationHasSource(keys["owner"], MemoryCurationSourceTranscript) {
		t.Fatalf("manual request was changed: id=%q scope=%#v", ids["owner"], keys["owner"])
	}
	automaticScope, ok := keys[automaticMemoryCurationCoalescingKey]
	if !ok || !curationHasSource(automaticScope, MemoryCurationSourceMemory) ||
		!curationHasSource(automaticScope, MemoryCurationSourceEvidence) ||
		!curationHasSource(automaticScope, MemoryCurationSourceTranscript) {
		t.Fatalf("automatic request scope = %#v", automaticScope)
	}
}

func TestMemoryCurationRollbackReplayDoesNotAdvanceCursorsPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, st, p := newMemoryCurationHardeningStore(t, dsn)
	source := captureHardeningMemory(ctx, t, st, p, "source retained for replay", "replay-source")
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceMemory}},
		CoalescingKey: "replay_original", TriggerReason: "manual_refine",
		IdempotencyKey: "replay-original-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "replay-original-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := []MemoryCurationPlanAction{{
		Ordinal: 1, Operation: MemoryCurationOperationCreate,
		Create: &MemoryCurationCreateAction{
			LocalRef: "original_summary",
			Snapshot: MemoryCurationMemorySnapshot{
				Content: "the original curated replay output", Kind: "summary",
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
	planned := planHardeningCuration(ctx, t, st, p, started, actions, "replay-original-plan")
	applied, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
		IdempotencyKey: "replay-original-apply",
	})
	if err != nil {
		t.Fatal(err)
	}
	produced, _ := memoryCurationProducedHeads(applied.Receipt.ActionResults)
	rolledBack, err := st.RollbackCuration(ctx, p, started.Run.ID, RollbackMemoryCurationInput{
		ApplyReceiptID: applied.Receipt.ID, ExpectedProducedHeads: produced,
		IdempotencyKey: "replay-original-rollback",
	})
	if err != nil {
		t.Fatal(err)
	}
	positionsBefore := loadHardeningCursorPositions(ctx, t, st, p)
	replayStarted, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: rolledBack.ReplayRequest.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "replay-read-only-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := st.GetCurationRunInputs(ctx, p, replayStarted.Run.ID,
		replayStarted.Run.FencingGeneration, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if countCurationInputKind(page.Inputs, MemoryCurationInputCursor) != 0 ||
		!containsCurationMemoryInput(page.Inputs, source.Memory.ID, source.Memory.Version) {
		t.Fatalf("replay inputs = %#v", page.Inputs)
	}
	replayActions := []MemoryCurationPlanAction{{
		Ordinal: 1, Operation: MemoryCurationOperationCreate,
		Create: &MemoryCurationCreateAction{
			LocalRef: "reconsidered_summary",
			Snapshot: MemoryCurationMemorySnapshot{
				Content: "a newly reconsidered replay output", Kind: "summary",
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
	replayPlan := planHardeningCuration(ctx, t, st, p, replayStarted,
		replayActions, "replay-read-only-plan")
	replayApplied, err := st.ApplyCuration(ctx, p, replayStarted.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: replayStarted.Run.FencingGeneration,
		PlanRevision:      replayPlan.Plan.PlanRevision, PlanHash: replayPlan.Receipt.PlanHash,
		IdempotencyKey: "replay-read-only-apply",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(replayApplied.Receipt.CursorIntervals) != 0 || replayApplied.FollowUpRequest != nil {
		t.Fatalf("replay apply receipt = %#v", replayApplied.Receipt)
	}
	positionsAfter := loadHardeningCursorPositions(ctx, t, st, p)
	if len(positionsBefore) != len(positionsAfter) {
		t.Fatalf("cursor positions changed: before=%#v after=%#v", positionsBefore, positionsAfter)
	}
	for key, before := range positionsBefore {
		if positionsAfter[key] != before {
			t.Fatalf("cursor %s changed from %d to %d", key, before, positionsAfter[key])
		}
	}
}

func TestMemoryCurationApplyRollbackHardeningPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	t.Run("tampered applied result cannot target unrelated resources", func(t *testing.T) {
		ctx, st, p, _, started, _, applied, createdID :=
			prepareHardeningAppliedCreate(t, dsn, "provenance")
		unrelated := captureHardeningMemory(ctx, t, st, p,
			"unrelated evidence must remain unrelated", "provenance-unrelated")
		if len(unrelated.Memory.Evidence) != 1 {
			t.Fatalf("unrelated evidence = %#v", unrelated.Memory.Evidence)
		}
		tampered := applied.Receipt.ActionResults[0]
		tampered.EvidenceIDs[0] = unrelated.Memory.Evidence[0].ID
		raw, err := canonicalMemoryCurationJSON(tampered)
		if err != nil {
			t.Fatal(err)
		}
		if tag, err := st.pool.Exec(ctx, `
			UPDATE memory_curation_actions SET applied_result=$2::jsonb
			WHERE id=$1`, tampered.ActionID, string(raw)); err != nil {
			t.Fatal(err)
		} else if tag.RowsAffected() != 1 {
			t.Fatalf("tampered action rows = %d", tag.RowsAffected())
		}
		produced, _ := memoryCurationProducedHeads(applied.Receipt.ActionResults)
		if _, err := st.RollbackCuration(ctx, p, started.Run.ID, RollbackMemoryCurationInput{
			ApplyReceiptID: applied.Receipt.ID, ExpectedProducedHeads: produced,
			IdempotencyKey: "provenance-rollback",
		}); !errors.Is(err, ErrMemoryCurationConflict) {
			t.Fatalf("tampered rollback error = %v", err)
		}
		memory, err := st.GetMemory(ctx, p, createdID)
		if err != nil || memory.State != MemoryStateActive || memory.Version != 1 {
			t.Fatalf("tampered rollback changed output = %#v / %v", memory, err)
		}
		var evidenceExists bool
		if err := st.pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM memory_evidence WHERE id=$1
		)`, unrelated.Memory.Evidence[0].ID).Scan(&evidenceExists); err != nil || !evidenceExists {
			t.Fatalf("unrelated evidence exists = %v / %v", evidenceExists, err)
		}
	})

	t.Run("cancelled validated consumer does not block rollback", func(t *testing.T) {
		ctx, st, p, _, started, _, applied, _ :=
			prepareHardeningAppliedCreate(t, dsn, "cancelled-consumer")
		produced, _ := memoryCurationProducedHeads(applied.Receipt.ActionResults)
		requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
			Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceMemory}},
			CoalescingKey: "cancelled_consumer", TriggerReason: "manual_refine",
			IdempotencyKey: "cancelled-consumer-later-request",
		})
		if err != nil {
			t.Fatal(err)
		}
		consumer, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
			RequestID: requested.Request.ID, LeaseDuration: time.Minute,
			IdempotencyKey: "cancelled-consumer-later-start",
		})
		if err != nil {
			t.Fatal(err)
		}
		consumerActions := []MemoryCurationPlanAction{{
			Ordinal: 1, Operation: MemoryCurationOperationProposeFact,
			ProposeFact: &MemoryCurationProposeFactAction{
				Predicate: "curation/cancelled_consumer", ValueType: "string",
				Value: json.RawMessage(`"never applied"`),
				Evidence: []MemoryCurationEvidence{{
					Type: "memory", ResolutionState: MemoryEvidenceResolved,
					ResolvedKind: MemoryCurationSourceMemory,
					SourceMemory: &MemoryCurationVersionReference{
						MemoryID: produced[0].MemoryID, Version: produced[0].Version,
					},
				}},
			},
		}}
		planHardeningCuration(ctx, t, st, p, consumer, consumerActions,
			"cancelled-consumer-later-plan")
		if _, err := st.CancelCuration(ctx, p, consumer.Run.ID, FinishMemoryCurationInput{
			FencingGeneration: consumer.Run.FencingGeneration,
			Reason:            "plan_cancelled", IdempotencyKey: "cancelled-consumer-cancel",
		}); err != nil {
			t.Fatal(err)
		}
		rolledBack, err := st.RollbackCuration(ctx, p, started.Run.ID,
			RollbackMemoryCurationInput{
				ApplyReceiptID: applied.Receipt.ID, ExpectedProducedHeads: produced,
				IdempotencyKey: "cancelled-consumer-rollback",
			})
		if err != nil {
			t.Fatal(err)
		}
		if rolledBack.Run.State != MemoryCurationRunRolledBack {
			t.Fatalf("rollback = %#v", rolledBack.Run)
		}
	})

	t.Run("forgotten source after planning makes apply conflict", func(t *testing.T) {
		ctx, st, p := newMemoryCurationHardeningStore(t, dsn)
		source := captureHardeningMemory(ctx, t, st, p,
			"source that will be forgotten", "forgot-race-source")
		requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
			Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceMemory}},
			CoalescingKey: "forget_race", TriggerReason: "manual_refine",
			IdempotencyKey: "forget-race-request",
		})
		if err != nil {
			t.Fatal(err)
		}
		started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
			RequestID: requested.Request.ID, LeaseDuration: time.Minute,
			IdempotencyKey: "forget-race-start",
		})
		if err != nil {
			t.Fatal(err)
		}
		actions := hardeningCreateActions(source.Memory.ID, source.Memory.Version,
			"forget_race_output", "output must not survive forgotten provenance")
		planned := planHardeningCuration(ctx, t, st, p, started, actions, "forget-race-plan")
		createdID := planned.PreallocatedMemoryIDs[0].MemoryID
		if _, err := st.ForgetMemory(ctx, p, source.Memory.ID, MemoryLifecycleInput{
			ExpectedVersion: source.Memory.Version, Reason: "explicitly forgotten",
			IdempotencyKey: "forget-race-forget",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
			IdempotencyKey: "forget-race-apply",
		}); !errors.Is(err, ErrMemoryCurationConflict) {
			t.Fatalf("forgotten source apply error = %v", err)
		}
		if _, err := st.GetMemory(ctx, p, createdID); !errors.Is(err, ErrMemoryNotFound) {
			t.Fatalf("forgotten source left output: %v", err)
		}
	})

	t.Run("expired renew durably interrupts and fences", func(t *testing.T) {
		ctx, st, p := newMemoryCurationHardeningStore(t, dsn)
		requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
			CoalescingKey: "renew_expired", TriggerReason: "manual_refine",
			IdempotencyKey: "renew-expired-request",
		})
		if err != nil {
			t.Fatal(err)
		}
		started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
			RequestID: requested.Request.ID, LeaseDuration: time.Minute,
			IdempotencyKey: "renew-expired-start",
		})
		if err != nil {
			t.Fatal(err)
		}
		liveRenew, err := st.RenewCuration(ctx, p, started.Run.ID, RenewMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			Extension:         time.Minute, IdempotencyKey: "renew-before-expiry",
		})
		if err != nil || liveRenew.Receipt.Replayed {
			t.Fatalf("live renew = %#v / %v", liveRenew, err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE memory_curation_runs
			SET lease_expires_at=clock_timestamp()-interval '1 second'
			WHERE id=$1`, started.Run.ID); err != nil {
			t.Fatal(err)
		}
		liveReplay, err := st.RenewCuration(ctx, p, started.Run.ID, RenewMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			Extension:         time.Minute, IdempotencyKey: "renew-before-expiry",
		})
		if err != nil || !liveReplay.Receipt.Replayed ||
			liveReplay.Receipt.CreatedAt != liveRenew.Receipt.CreatedAt {
			t.Fatalf("expired exact replay of live renew = %#v / %v", liveReplay, err)
		}
		status, err := st.GetCurationStatus(ctx, p, "")
		if err != nil || status.Lane.ActiveRunID != started.Run.ID ||
			status.Run == nil || status.Run.State != MemoryCurationRunOpen {
			t.Fatalf("exact replay changed expired run = %#v / %v", status, err)
		}
		if _, err := st.RenewCuration(ctx, p, started.Run.ID, RenewMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			Extension:         time.Minute, IdempotencyKey: "renew-expired-renew",
		}); !errors.Is(err, ErrMemoryCurationLeaseExpired) {
			t.Fatalf("expired renew error = %v", err)
		}
		status, err = st.GetCurationStatus(ctx, p, "")
		if err != nil {
			t.Fatal(err)
		}
		if status.Lane.ActiveRunID != "" ||
			status.Lane.FencingGeneration <= started.Run.FencingGeneration {
			t.Fatalf("expired renew lane = %#v", status.Lane)
		}
		run, err := st.GetCurationRun(ctx, p, started.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State != MemoryCurationRunInterrupted || run.TerminalReasonCode != "lease_expired" {
			t.Fatalf("expired renew run = %#v", run)
		}
	})

	t.Run("stale renew cannot reconcile another expired active run", func(t *testing.T) {
		ctx, st, p := newMemoryCurationHardeningStore(t, dsn)
		requestA, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
			CoalescingKey: "stale_renew_a", TriggerReason: "manual_refine",
			IdempotencyKey: "stale-renew-request-a",
		})
		if err != nil {
			t.Fatal(err)
		}
		runA, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
			RequestID: requestA.Request.ID, LeaseDuration: time.Minute,
			IdempotencyKey: "stale-renew-start-a",
		})
		if err != nil {
			t.Fatal(err)
		}
		planA := planHardeningCuration(ctx, t, st, p, runA, nil, "stale-renew-plan-a")
		if _, err := st.ApplyCuration(ctx, p, runA.Run.ID, ApplyMemoryCurationInput{
			FencingGeneration: runA.Run.FencingGeneration,
			PlanRevision:      planA.Plan.PlanRevision,
			PlanHash:          planA.Receipt.PlanHash,
			IdempotencyKey:    "stale-renew-apply-a",
		}); err != nil {
			t.Fatal(err)
		}

		requestB, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
			CoalescingKey: "stale_renew_b", TriggerReason: "manual_refine",
			IdempotencyKey: "stale-renew-request-b",
		})
		if err != nil {
			t.Fatal(err)
		}
		runB, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
			RequestID: requestB.Request.ID, LeaseDuration: time.Minute,
			IdempotencyKey: "stale-renew-start-b",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `UPDATE memory_curation_runs
			SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`,
			runB.Run.ID); err != nil {
			t.Fatal(err)
		}

		if _, err := st.RenewCuration(ctx, p, runA.Run.ID, RenewMemoryCurationInput{
			FencingGeneration: runA.Run.FencingGeneration,
			Extension:         time.Minute,
			IdempotencyKey:    "stale-renew-wrong-run",
		}); !errors.Is(err, ErrMemoryCurationFenceMismatch) {
			t.Fatalf("stale renew error = %v", err)
		}
		var staleReceipts int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM memory_curation_mutations
			WHERE account_id=$1 AND operation='renew' AND idempotency_key=$2`,
			p.AccountID, "stale-renew-wrong-run").Scan(&staleReceipts); err != nil {
			t.Fatal(err)
		}
		if staleReceipts != 0 {
			t.Fatalf("stale renew receipts = %d", staleReceipts)
		}
		status, err := st.GetCurationStatus(ctx, p, runB.Run.ID)
		if err != nil || status.Run == nil || status.Run.State != MemoryCurationRunOpen ||
			status.Lane.ActiveRunID != runB.Run.ID ||
			status.Lane.FencingGeneration != runB.Run.FencingGeneration {
			t.Fatalf("stale renew mutated active run = %#v / %v", status, err)
		}

		expired, err := st.RenewCuration(ctx, p, runB.Run.ID, RenewMemoryCurationInput{
			FencingGeneration: runB.Run.FencingGeneration,
			Extension:         time.Minute,
			IdempotencyKey:    "stale-renew-correct-run",
		})
		if !errors.Is(err, ErrMemoryCurationLeaseExpired) || expired.Receipt.Replayed ||
			expired.Run.ID != runB.Run.ID {
			t.Fatalf("correct expired renew = %#v / %v", expired, err)
		}
		replay, err := st.RenewCuration(ctx, p, runB.Run.ID, RenewMemoryCurationInput{
			FencingGeneration: runB.Run.FencingGeneration,
			Extension:         time.Minute,
			IdempotencyKey:    "stale-renew-correct-run",
		})
		if !errors.Is(err, ErrMemoryCurationLeaseExpired) || !replay.Receipt.Replayed ||
			replay.Receipt.CreatedAt != expired.Receipt.CreatedAt {
			t.Fatalf("correct expired renew replay = %#v / %v", replay, err)
		}
	})
}

func newMemoryCurationHardeningStore(
	t *testing.T,
	dsn string,
) (context.Context, *Store, Principal) {
	t.Helper()
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return ctx, st, provisionMemoryCurationApplyPrincipal(ctx, t, st)
}

func captureHardeningMemory(
	ctx context.Context,
	t *testing.T,
	st *Store,
	p Principal,
	content, key string,
) MemoryMutationResult {
	t.Helper()
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

func captureHardeningMemoryWithSalience(
	ctx context.Context,
	t *testing.T,
	st *Store,
	p Principal,
	content, key string,
	salience float64,
) MemoryMutationResult {
	t.Helper()
	out, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: content, Kind: "decision", Salience: &salience,
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

func prepareHardeningAppliedCreate(
	t *testing.T,
	dsn, prefix string,
) (context.Context, *Store, Principal, MemoryMutationResult,
	StartMemoryCurationResult, PlanMemoryCurationResult, ApplyMemoryCurationResult, string) {
	t.Helper()
	ctx, st, p := newMemoryCurationHardeningStore(t, dsn)
	source := captureHardeningMemory(ctx, t, st, p,
		prefix+" source", prefix+"-source")
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceMemory}},
		CoalescingKey: prefix + "_request", TriggerReason: "manual_refine",
		IdempotencyKey: prefix + "-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: prefix + "-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := hardeningCreateActions(source.Memory.ID, source.Memory.Version,
		prefix+"_output", prefix+" curated output")
	planned := planHardeningCuration(ctx, t, st, p, started, actions, prefix+"-plan")
	applied, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
		IdempotencyKey: prefix + "-apply",
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, st, p, source, started, planned, applied,
		planned.PreallocatedMemoryIDs[0].MemoryID
}

func hardeningCreateActions(
	sourceMemoryID string,
	sourceVersion int64,
	localRef, content string,
) []MemoryCurationPlanAction {
	return []MemoryCurationPlanAction{{
		Ordinal: 1, Operation: MemoryCurationOperationCreate,
		Create: &MemoryCurationCreateAction{
			LocalRef: localRef,
			Snapshot: MemoryCurationMemorySnapshot{
				Content: content, Kind: "summary",
				Evidence: []MemoryCurationEvidence{{
					Type: "memory", ResolutionState: MemoryEvidenceResolved,
					ResolvedKind: MemoryCurationSourceMemory,
					SourceMemory: &MemoryCurationVersionReference{
						MemoryID: sourceMemoryID, Version: sourceVersion,
					},
				}},
			},
		},
	}}
}

func planHardeningCuration(
	ctx context.Context,
	t *testing.T,
	st *Store,
	p Principal,
	started StartMemoryCurationResult,
	actions []MemoryCurationPlanAction,
	key string,
) PlanMemoryCurationResult {
	t.Helper()
	if actions == nil {
		actions = make([]MemoryCurationPlanAction, 0)
	}
	raw, err := json.Marshal(MemoryCurationPlanDraft{
		Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1, Actions: actions,
	})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := st.PlanCuration(ctx, p, started.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration, Draft: raw,
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return planned
}

func countCurationInputKind(inputs []MemoryCurationRunInput, kind string) int {
	count := 0
	for _, input := range inputs {
		if input.Kind == kind {
			count++
		}
	}
	return count
}

func containsCurationMemoryInput(
	inputs []MemoryCurationRunInput,
	memoryID string,
	version int64,
) bool {
	for _, input := range inputs {
		if input.Kind == MemoryCurationInputMemory && input.MemoryID == memoryID &&
			input.MemoryVersion == version {
			return true
		}
	}
	return false
}

func loadHardeningCursorPositions(
	ctx context.Context,
	t *testing.T,
	st *Store,
	p Principal,
) map[string]int64 {
	t.Helper()
	rows, err := st.pool.Query(ctx, `
		SELECT source_kind,source_stream_id,position FROM memory_curation_cursors
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		ORDER BY source_kind,source_stream_id`, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var kind, stream string
		var position int64
		if err := rows.Scan(&kind, &stream, &position); err != nil {
			t.Fatal(err)
		}
		out[kind+"/"+stream] = position
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
