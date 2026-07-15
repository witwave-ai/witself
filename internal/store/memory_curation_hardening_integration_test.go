package store

import (
	"context"
	"encoding/json"
	"errors"
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
		if _, err := st.pool.Exec(ctx, `
			UPDATE memory_curation_runs
			SET lease_expires_at=clock_timestamp()-interval '1 second'
			WHERE id=$1`, started.Run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.RenewCuration(ctx, p, started.Run.ID, RenewMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			Extension:         time.Minute, IdempotencyKey: "renew-expired-renew",
		}); !errors.Is(err, ErrMemoryCurationLeaseExpired) {
			t.Fatalf("expired renew error = %v", err)
		}
		status, err := st.GetCurationStatus(ctx, p, "")
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
