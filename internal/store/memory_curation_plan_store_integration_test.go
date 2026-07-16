package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestPlanMemoryCurationPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	t.Run("all primitives immutable replay and authorization", func(t *testing.T) {
		fixture := newMemoryCurationPlanFixture(ctx, t, st, "full", false, 2)
		draft := fullMemoryCurationPlanDraft(fixture)
		raw := marshalCurationPlanDraft(t, draft, false)

		otherInput := PlanMemoryCurationInput{
			FencingGeneration: fixture.Started.Run.FencingGeneration,
			Draft:             raw, IdempotencyKey: "plan-cross-owner",
		}
		if _, err := st.PlanCuration(ctx, fixture.Other, fixture.Started.Run.ID, otherInput); !errors.Is(err, ErrMemoryCurationNotFound) {
			t.Fatalf("cross-owner plan error = %v", err)
		}
		wrongFence := otherInput
		wrongFence.FencingGeneration++
		wrongFence.IdempotencyKey = "plan-wrong-fence"
		if _, err := st.PlanCuration(ctx, fixture.Principal, fixture.Started.Run.ID, wrongFence); !errors.Is(err, ErrMemoryCurationFenceMismatch) {
			t.Fatalf("wrong-fence plan error = %v", err)
		}

		unauthorized := MemoryCurationPlanDraft{
			Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1,
			Actions: []MemoryCurationPlanAction{{Ordinal: 1, Operation: MemoryCurationOperationCreate,
				Create: &MemoryCurationCreateAction{LocalRef: "bad", Snapshot: MemoryCurationMemorySnapshot{
					Content: "must not persist", Evidence: []MemoryCurationEvidence{{
						Type: "conversation", ResolutionState: MemoryEvidenceResolved,
						ResolvedKind: "message", SourceMessageID: "not-materialized",
					}},
				}},
			}},
		}
		if _, err := st.PlanCuration(ctx, fixture.Principal, fixture.Started.Run.ID,
			PlanMemoryCurationInput{FencingGeneration: fixture.Started.Run.FencingGeneration,
				Draft: marshalCurationPlanDraft(t, unauthorized, false), IdempotencyKey: "plan-unauthorized"}); !errors.Is(err, ErrMemoryCurationConflict) {
			t.Fatalf("unauthorized provenance error = %v", err)
		}

		input := PlanMemoryCurationInput{
			FencingGeneration: fixture.Started.Run.FencingGeneration,
			Draft:             raw, IdempotencyKey: "plan-full",
		}
		planned, err := st.PlanCuration(ctx, fixture.Principal, fixture.Started.Run.ID, input)
		if err != nil {
			t.Fatal(err)
		}
		if planned.Run.State != MemoryCurationRunPlanned || planned.Run.PlanRevision != 1 ||
			planned.Plan.PlanRevision != 1 || planned.Run.PlanHash == "" || planned.Receipt.Replayed ||
			len(planned.PreallocatedMemoryIDs) != 1 || planned.Preview.ActionCount != 5 {
			t.Fatalf("planned result = %#v", planned)
		}
		if got, want := planned.Preview, (MemoryCurationImpactPreview{
			ActionCount: 5, CreateActions: 1, ReplaceActions: 1, SupersedeActions: 1,
			RelateActions: 1, ProposeFactActions: 1, NewMemories: 1,
			MemoryVersionWrites: 3, EvidenceRows: 3, RelationRows: 3,
			ExpectedVersionChecks: 2, FactCandidates: 1,
		}); !reflect.DeepEqual(got, want) {
			t.Fatalf("preview = %#v, want %#v", got, want)
		}

		indented := marshalCurationPlanDraft(t, draft, true)
		replayInput := input
		replayInput.Draft = indented
		replayed, err := st.PlanCuration(ctx, fixture.Principal, fixture.Started.Run.ID, replayInput)
		if err != nil {
			t.Fatal(err)
		}
		if !replayed.Receipt.Replayed || replayed.Receipt.ID != planned.Receipt.ID ||
			replayed.Run.PlanHash != planned.Run.PlanHash || !reflect.DeepEqual(replayed.PreallocatedMemoryIDs, planned.PreallocatedMemoryIDs) {
			t.Fatalf("plan replay = %#v", replayed)
		}
		changed := draft
		changed.DraftRevision++
		changedInput := input
		changedInput.Draft = marshalCurationPlanDraft(t, changed, false)
		if _, err := st.PlanCuration(ctx, fixture.Principal, fixture.Started.Run.ID, changedInput); !errors.Is(err, ErrMemoryCurationIdempotencyConflict) {
			t.Fatalf("changed replay error = %v", err)
		}
		immutable := input
		immutable.IdempotencyKey = "plan-second"
		if _, err := st.PlanCuration(ctx, fixture.Principal, fixture.Started.Run.ID, immutable); !errors.Is(err, ErrMemoryCurationConflict) {
			t.Fatalf("second plan error = %v", err)
		}

		var count int
		var ordinals []int64
		rows, err := st.pool.Query(ctx, `
			SELECT ordinal FROM memory_curation_actions
			WHERE run_id=$1 ORDER BY ordinal`, fixture.Started.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var ordinal int64
			if err := rows.Scan(&ordinal); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			ordinals = append(ordinals, ordinal)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		if !reflect.DeepEqual(ordinals, []int64{1, 2, 3, 4, 5}) {
			t.Fatalf("persisted action order = %#v", ordinals)
		}
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FROM memory_curation_actions
			WHERE run_id=$1 AND state='validated' AND action_hash ~ '^[0-9a-f]{64}$'
			  AND proposed_payload ? 'ordinal' AND proposed_payload ? 'operation'
			  AND NOT (proposed_payload ? 'draft_revision')`, fixture.Started.Run.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 5 {
			t.Fatalf("validated complete action rows = %d, want 5", count)
		}
		if err := st.pool.QueryRow(ctx, `
			SELECT count(*) FROM account_events
			WHERE account_id=$1 AND verb=$2 AND metadata->>'run_id'=$3`,
			fixture.Principal.AccountID, VerbMemoryCurationPlanned, fixture.Started.Run.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("planned event count = %d, want 1", count)
		}
		run, err := loadMemoryCurationRun(ctx, st.pool, fixture.Principal, fixture.Started.Run.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := loadMemoryCurationStoredPlan(ctx, st.pool, fixture.Principal, run)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Acceptance.PlanHash != planned.Run.PlanHash || len(stored.Actions) != 5 ||
			!bytes.Equal(stored.Acceptance.CanonicalBytes(), canonicalCurationPlanForTest(t, planned.Plan)) {
			t.Fatalf("stored plan reconstruction = %#v", stored.Acceptance)
		}
	})

	t.Run("empty plan", func(t *testing.T) {
		fixture := newMemoryCurationPlanFixture(ctx, t, st, "empty", false, 1)
		draft := MemoryCurationPlanDraft{Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1,
			Actions: []MemoryCurationPlanAction{}}
		result, err := st.PlanCuration(ctx, fixture.Principal, fixture.Started.Run.ID,
			PlanMemoryCurationInput{FencingGeneration: fixture.Started.Run.FencingGeneration,
				Draft: marshalCurationPlanDraft(t, draft, false), IdempotencyKey: "plan-empty"})
		if err != nil {
			t.Fatal(err)
		}
		if result.Preview.ActionCount != 0 || len(result.Plan.Actions) != 0 {
			t.Fatalf("empty result = %#v", result)
		}
		var count int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM memory_curation_actions WHERE run_id=$1`, fixture.Started.Run.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("empty action rows = %d", count)
		}
	})

	t.Run("accepted plan read is verified fenced and read only", func(t *testing.T) {
		fixture := newMemoryCurationPlanFixture(ctx, t, st, "plan-read", false, 2)
		draft := fullMemoryCurationPlanDraft(fixture)
		planned, err := st.PlanCuration(ctx, fixture.Principal, fixture.Started.Run.ID,
			PlanMemoryCurationInput{FencingGeneration: fixture.Started.Run.FencingGeneration,
				Draft: marshalCurationPlanDraft(t, draft, false), IdempotencyKey: "plan-read-accept"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.GetCurationPlan(ctx, fixture.Principal, fixture.Started.Run.ID,
			fixture.Started.Run.FencingGeneration)
		if err != nil {
			t.Fatal(err)
		}
		// The stored JSON round-trip may normalize omitted optional slices from
		// empty to nil and JSONB may normalize RawMessage whitespace. Those shapes
		// have identical canonical/wire JSON; compare the immutable accepted
		// payload rather than Go allocation details.
		if !bytes.Equal(canonicalCurationPlanForTest(t, got.Plan), canonicalCurationPlanForTest(t, planned.Plan)) ||
			!reflect.DeepEqual(got.PreallocatedMemoryIDs, planned.PreallocatedMemoryIDs) ||
			!reflect.DeepEqual(got.Preview, planned.Preview) ||
			got.Run.PlanHash != planned.Run.PlanHash {
			t.Fatalf("accepted plan read = %#v, want %#v", got, planned)
		}
		if _, err := st.GetCurationPlan(ctx, fixture.Other, fixture.Started.Run.ID,
			fixture.Started.Run.FencingGeneration); !errors.Is(err, ErrMemoryCurationNotFound) {
			t.Fatalf("cross-owner accepted plan read error = %v", err)
		}
		if _, err := st.GetCurationPlan(ctx, fixture.Principal, fixture.Started.Run.ID,
			fixture.Started.Run.FencingGeneration+1); !errors.Is(err, ErrMemoryCurationFenceMismatch) {
			t.Fatalf("wrong-fence accepted plan read error = %v", err)
		}

		openFixture := newMemoryCurationPlanFixture(ctx, t, st, "plan-read-open", false, 1)
		if _, err := st.GetCurationPlan(ctx, openFixture.Principal, openFixture.Started.Run.ID,
			openFixture.Started.Run.FencingGeneration); !errors.Is(err, ErrMemoryCurationConflict) {
			t.Fatalf("open-run accepted plan read error = %v", err)
		}

		var mutationsBefore, eventsBefore int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM memory_curation_mutations WHERE run_id=$1`,
			fixture.Started.Run.ID).Scan(&mutationsBefore); err != nil {
			t.Fatal(err)
		}
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM account_events WHERE account_id=$1 AND metadata->>'run_id'=$2`,
			fixture.Principal.AccountID, fixture.Started.Run.ID).Scan(&eventsBefore); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `UPDATE memory_curation_runs
			SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`,
			fixture.Started.Run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.GetCurationPlan(ctx, fixture.Principal, fixture.Started.Run.ID,
			fixture.Started.Run.FencingGeneration); !errors.Is(err, ErrMemoryCurationLeaseExpired) {
			t.Fatalf("expired accepted plan read error = %v", err)
		}
		run, err := st.GetCurationRun(ctx, fixture.Principal, fixture.Started.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State != MemoryCurationRunPlanned {
			t.Fatalf("expired accepted plan read mutated run = %#v", run)
		}
		var mutationsAfter, eventsAfter int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM memory_curation_mutations WHERE run_id=$1`,
			fixture.Started.Run.ID).Scan(&mutationsAfter); err != nil {
			t.Fatal(err)
		}
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM account_events WHERE account_id=$1 AND metadata->>'run_id'=$2`,
			fixture.Principal.AccountID, fixture.Started.Run.ID).Scan(&eventsAfter); err != nil {
			t.Fatal(err)
		}
		if mutationsAfter != mutationsBefore || eventsAfter != eventsBefore {
			t.Fatalf("accepted plan read changed receipts/events: (%d,%d) -> (%d,%d)",
				mutationsBefore, eventsBefore, mutationsAfter, eventsAfter)
		}
	})

	t.Run("expired lease interrupts without accepting", func(t *testing.T) {
		fixture := newMemoryCurationPlanFixture(ctx, t, st, "expired", false, 1)
		if _, err := st.pool.Exec(ctx, `
			UPDATE memory_curation_runs SET lease_expires_at=clock_timestamp()-interval '1 second'
			WHERE id=$1`, fixture.Started.Run.ID); err != nil {
			t.Fatal(err)
		}
		draft := MemoryCurationPlanDraft{Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1,
			Actions: []MemoryCurationPlanAction{}}
		if _, err := st.PlanCuration(ctx, fixture.Principal, fixture.Started.Run.ID,
			PlanMemoryCurationInput{FencingGeneration: fixture.Started.Run.FencingGeneration,
				Draft: marshalCurationPlanDraft(t, draft, false), IdempotencyKey: "plan-expired"}); !errors.Is(err, ErrMemoryCurationLeaseExpired) {
			t.Fatalf("expired plan error = %v", err)
		}
		run, err := st.GetCurationRun(ctx, fixture.Principal, fixture.Started.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State != MemoryCurationRunInterrupted || run.PlanRevision != 0 {
			t.Fatalf("expired run = %#v", run)
		}
	})

	t.Run("sensitive input cannot escape", func(t *testing.T) {
		fixture := newMemoryCurationPlanFixture(ctx, t, st, "sensitive", true, 1)
		source := fixture.Memories[0].Memory
		draft := MemoryCurationPlanDraft{Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1,
			Actions: []MemoryCurationPlanAction{{Ordinal: 1, Operation: MemoryCurationOperationCreate,
				Create: &MemoryCurationCreateAction{LocalRef: "derived", Snapshot: MemoryCurationMemorySnapshot{
					Content: "derived", Sensitive: false, Evidence: []MemoryCurationEvidence{{
						Type: "memory", ResolutionState: MemoryEvidenceResolved, ResolvedKind: "memory",
						SourceMemory: &MemoryCurationVersionReference{MemoryID: source.ID, Version: source.Version},
					}},
				}},
			}},
		}
		input := PlanMemoryCurationInput{FencingGeneration: fixture.Started.Run.FencingGeneration,
			Draft: marshalCurationPlanDraft(t, draft, false), IdempotencyKey: "plan-sensitive"}
		if _, err := st.PlanCuration(ctx, fixture.Principal, fixture.Started.Run.ID, input); !errors.Is(err, ErrMemoryCurationConflict) {
			t.Fatalf("sensitive escape error = %v", err)
		}
		draft.Actions[0].Create.Snapshot.Sensitive = true
		input.Draft = marshalCurationPlanDraft(t, draft, false)
		result, err := st.PlanCuration(ctx, fixture.Principal, fixture.Started.Run.ID, input)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Plan.Actions[0].Create.Snapshot.Sensitive {
			t.Fatal("accepted sensitive output lost its marker")
		}
	})
}

type memoryCurationPlanFixture struct {
	Principal Principal
	Other     Principal
	Started   StartMemoryCurationResult
	Memories  []MemoryMutationResult
	Entries   []TranscriptEntry
}

func newMemoryCurationPlanFixture(
	ctx context.Context,
	t *testing.T,
	st *Store,
	name string,
	sensitive bool,
	memoryCount int,
) memoryCurationPlanFixture {
	t.Helper()
	provisioned, err := st.ProvisionAccount(ctx,
		fmt.Sprintf("curation-plan-%s@witwave.ai", name), "curation plan "+name, time.Hour)
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
	other, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "other")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AccountStatus: "active"}
	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "curation-plan-" + name})
	if err != nil {
		t.Fatal(err)
	}
	fixture := memoryCurationPlanFixture{Principal: p,
		Other: Principal{Kind: PrincipalAgent, ID: other.ID, AccountID: p.AccountID,
			RealmID: p.RealmID, AccountStatus: "active"}}
	for index := 0; index < memoryCount; index++ {
		entry, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
			transcript.ID, AppendTranscriptEntryInput{
				ExternalID: fmt.Sprintf("turn-%d", index+1), Role: TranscriptRoleUser,
				Body: fmt.Sprintf("decision %d", index+1),
			})
		if err != nil {
			t.Fatal(err)
		}
		fixture.Entries = append(fixture.Entries, entry)
		captured, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
			Content: fmt.Sprintf("memory %d", index+1), Kind: "decision", Sensitive: sensitive,
			Evidence: []MemoryEvidenceInput{{Type: "conversation",
				ResolutionState: MemoryEvidenceResolved, ResolvedKind: "transcript",
				SourceTranscriptID: transcript.ID, SourceSequenceFrom: entry.Sequence,
				SourceSequenceUntil: entry.Sequence,
			}},
			IdempotencyKey: fmt.Sprintf("capture-%s-%d", name, index+1),
		})
		if err != nil {
			t.Fatal(err)
		}
		fixture.Memories = append(fixture.Memories, captured)
	}
	scope := MemoryCurationScope{IncludeSensitive: sensitive}
	request, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: scope, CoalescingKey: "plan_" + name, TriggerReason: "manual_refine",
		IdempotencyKey: "request-" + name,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.Started, err = st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: request.Request.ID, LeaseDuration: time.Minute,
		Client:         MemoryClientProvenance{Runtime: "codex", Model: "gpt-5"},
		IdempotencyKey: "start-" + name,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func fullMemoryCurationPlanDraft(fixture memoryCurationPlanFixture) MemoryCurationPlanDraft {
	source := fixture.Memories[0].Memory
	target := fixture.Memories[1].Memory
	targetEvidence := memoryCurationEvidenceFromInputRow(target.Evidence[0])
	directTranscript := MemoryCurationEvidence{
		Type: "conversation", ResolutionState: MemoryEvidenceResolved, ResolvedKind: "transcript",
		SourceTranscriptID: fixture.Entries[1].TranscriptID,
		SourceSequenceFrom: fixture.Entries[1].Sequence, SourceSequenceUntil: fixture.Entries[1].Sequence,
	}
	return MemoryCurationPlanDraft{
		Schema: MemoryCurationPlanSchemaV1, DraftRevision: 9,
		Actions: []MemoryCurationPlanAction{
			{Ordinal: 1, Operation: MemoryCurationOperationCreate, Create: &MemoryCurationCreateAction{
				LocalRef: "summary", Snapshot: MemoryCurationMemorySnapshot{
					Content: "curated summary", Kind: "decision", Evidence: []MemoryCurationEvidence{targetEvidence},
				}, Relations: []MemoryCurationLineageRelation{{RelationType: MemoryCurationRelationDerivedFrom,
					To: MemoryCurationVersionReference{MemoryID: source.ID, Version: source.Version}}},
			}},
			{Ordinal: 2, Operation: MemoryCurationOperationReplace, Replace: &MemoryCurationReplaceAction{
				Target: MemoryCurationTargetReference{MemoryID: target.ID, ExpectedVersion: target.Version},
				Snapshot: MemoryCurationMemorySnapshot{Content: "refined target", Kind: "decision",
					Evidence: []MemoryCurationEvidence{directTranscript}},
			}},
			{Ordinal: 3, Operation: MemoryCurationOperationSupersede, Supersede: &MemoryCurationSupersedeAction{
				Target:       MemoryCurationTargetReference{MemoryID: source.ID, ExpectedVersion: source.Version},
				Replacements: []MemoryCurationVersionReference{{LocalRef: "summary", Version: 1}},
			}},
			{Ordinal: 4, Operation: MemoryCurationOperationRelate, Relate: &MemoryCurationRelateAction{
				RelationType: MemoryCurationRelationSummarizes,
				From:         MemoryCurationVersionReference{LocalRef: "summary", Version: 1},
				To:           MemoryCurationVersionReference{MemoryID: target.ID, Version: target.Version},
			}},
			{Ordinal: 5, Operation: MemoryCurationOperationProposeFact, ProposeFact: &MemoryCurationProposeFactAction{
				Predicate: "profile/example", ValueType: "object", Value: json.RawMessage(`{"z":1,"a":"x"}`),
				Evidence: []MemoryCurationEvidence{{Type: "memory", ResolutionState: MemoryEvidenceResolved,
					ResolvedKind: "memory", SourceMemory: &MemoryCurationVersionReference{LocalRef: "summary", Version: 1}}},
			}},
		},
	}
}

func marshalCurationPlanDraft(t *testing.T, draft MemoryCurationPlanDraft, indent bool) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	if !indent {
		return raw
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	return formatted.Bytes()
}

func canonicalCurationPlanForTest(t *testing.T, plan MemoryCurationPlan) []byte {
	t.Helper()
	raw, err := canonicalMemoryCurationJSON(plan)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
