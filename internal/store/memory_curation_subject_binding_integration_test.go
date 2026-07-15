package store

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"testing"
)

func TestMemoryCurationFactSubjectAliasIsBoundAtPlanPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	fixture := newMemoryCurationPlanFixture(ctx, t, st, "subject-binding", false, 1)
	p := fixture.Principal

	original, err := st.UpsertFactSubject(ctx, p, UpsertFactSubjectInput{
		CanonicalKey: "person_spouse", DisplayName: "Original subject",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.UpsertFactSubject(ctx, p, UpsertFactSubjectInput{
		CanonicalKey: "person_partner", DisplayName: "Second subject",
	})
	if err != nil {
		t.Fatal(err)
	}
	third, err := st.UpsertFactSubject(ctx, p, UpsertFactSubjectInput{
		CanonicalKey: "person_guardian", DisplayName: "Third subject",
	})
	if err != nil {
		t.Fatal(err)
	}
	const alias = "spouse"
	if _, err := st.AddFactSubjectAlias(ctx, p, original.CanonicalKey, alias); err != nil {
		t.Fatal(err)
	}

	source := fixture.Memories[0].Memory
	draft := MemoryCurationPlanDraft{
		Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1,
		Actions: []MemoryCurationPlanAction{{
			Ordinal: 1, Operation: MemoryCurationOperationProposeFact,
			ProposeFact: &MemoryCurationProposeFactAction{
				Subject: alias, Predicate: "identity/example", ValueType: "string",
				Value: json.RawMessage(`"bound to the original subject"`),
				Evidence: []MemoryCurationEvidence{{
					Type: "memory", ResolutionState: MemoryEvidenceResolved,
					ResolvedKind: MemoryCurationSourceMemory,
					SourceMemory: &MemoryCurationVersionReference{
						MemoryID: source.ID, Version: source.Version,
					},
				}},
			},
		}},
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := st.PlanCuration(ctx, p, fixture.Started.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: fixture.Started.Run.FencingGeneration,
		Draft:             raw, IdempotencyKey: "subject-binding-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := planned.Plan.Actions[0].ProposeFact.Subject; got != original.CanonicalKey {
		t.Fatalf("planned subject = %q, want canonical %q", got, original.CanonicalKey)
	}
	var persistedSubject string
	if err := st.pool.QueryRow(ctx, `
		SELECT proposed_payload #>> '{propose_fact,subject}'
		FROM memory_curation_actions
		WHERE run_id=$1 AND ordinal=1`, fixture.Started.Run.ID).Scan(&persistedSubject); err != nil {
		t.Fatal(err)
	}
	if persistedSubject != original.CanonicalKey {
		t.Fatalf("persisted subject = %q, want %q", persistedSubject, original.CanonicalKey)
	}

	reassignFactSubjectAliasForCurationTest(ctx, t, st, p,
		original.CanonicalKey, second.CanonicalKey, alias)
	if got, err := resolveFactSubjectCanonicalKey(ctx, st.pool, p, alias); err != nil || got != second.CanonicalKey {
		t.Fatalf("alias after first reassignment = %q / %v", got, err)
	}
	// A retry uses the immutable accepted plan even though the alias now points
	// elsewhere; it must not recanonicalize against the changed namespace.
	replayed, err := st.PlanCuration(ctx, p, fixture.Started.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: fixture.Started.Run.FencingGeneration,
		Draft:             raw, IdempotencyKey: "subject-binding-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Receipt.Replayed || replayed.Plan.Actions[0].ProposeFact.Subject != original.CanonicalKey ||
		replayed.Receipt.PlanHash != planned.Receipt.PlanHash {
		t.Fatalf("replayed bound plan = %#v", replayed)
	}

	applied, err := st.ApplyCuration(ctx, p, fixture.Started.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: fixture.Started.Run.FencingGeneration,
		PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
		IdempotencyKey: "subject-binding-apply",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Receipt.ActionResults) != 1 ||
		len(applied.Receipt.ActionResults[0].CandidateIDs) != 1 {
		t.Fatalf("applied result = %#v", applied)
	}
	candidateID := applied.Receipt.ActionResults[0].CandidateIDs[0]
	candidate, err := st.GetFactCandidate(ctx, p, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Subject != original.CanonicalKey {
		t.Fatalf("candidate subject after alias reassignment = %q, want %q",
			candidate.Subject, original.CanonicalKey)
	}

	reassignFactSubjectAliasForCurationTest(ctx, t, st, p,
		second.CanonicalKey, third.CanonicalKey, alias)
	if got, err := resolveFactSubjectCanonicalKey(ctx, st.pool, p, alias); err != nil || got != third.CanonicalKey {
		t.Fatalf("alias after second reassignment = %q / %v", got, err)
	}
	producedHeads, _ := memoryCurationProducedHeads(applied.Receipt.ActionResults)
	rolledBack, err := st.RollbackCuration(ctx, p, fixture.Started.Run.ID, RollbackMemoryCurationInput{
		ApplyReceiptID: applied.Receipt.ID, ExpectedProducedHeads: producedHeads,
		Reason: "verify canonical subject binding", IdempotencyKey: "subject-binding-rollback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Run.State != MemoryCurationRunRolledBack ||
		len(rolledBack.Receipt.ActionResults) != 1 ||
		len(rolledBack.Receipt.ActionResults[0].WithdrawnCandidateIDs) != 1 {
		t.Fatalf("rollback result = %#v", rolledBack)
	}
	candidate, err = st.GetFactCandidate(ctx, p, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Subject != original.CanonicalKey || candidate.Status != "withdrawn" {
		t.Fatalf("rolled-back candidate = %#v", candidate)
	}
}

func reassignFactSubjectAliasForCurationTest(
	ctx context.Context,
	t *testing.T,
	st *Store,
	p Principal,
	fromCanonical, toCanonical, alias string,
) {
	t.Helper()
	alias = normalizeFactSubjectAlias(alias)
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		t.Fatal(err)
	}
	if err := lockFactSubjectNamespace(ctx, tx, p, true); err != nil {
		t.Fatal(err)
	}
	from, err := getFactSubjectByCanonicalKey(ctx, tx, p, fromCanonical)
	if err != nil {
		t.Fatal(err)
	}
	to, err := getFactSubjectByCanonicalKey(ctx, tx, p, toCanonical)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	kept := make([]string, 0, len(from.Aliases))
	for _, existing := range from.Aliases {
		if existing == alias {
			found = true
			continue
		}
		kept = append(kept, existing)
	}
	if !found {
		t.Fatalf("source subject %q does not own alias %q", fromCanonical, alias)
	}
	for _, existing := range to.Aliases {
		if existing == alias {
			t.Fatalf("target subject %q already owns alias %q", toCanonical, alias)
		}
	}
	to.Aliases = append(to.Aliases, alias)
	sort.Strings(kept)
	sort.Strings(to.Aliases)
	fromAliases, err := json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	toAliases, err := json.Marshal(to.Aliases)
	if err != nil {
		t.Fatal(err)
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE fact_subjects SET aliases=$1::jsonb,updated_at=clock_timestamp()
		WHERE id=$2 AND account_id=$3 AND realm_id=$4 AND owner_agent_id=$5`,
		string(fromAliases), from.ID, p.AccountID, p.RealmID, p.ID); err != nil {
		t.Fatal(err)
	} else if tag.RowsAffected() != 1 {
		t.Fatal("source alias reassignment did not update one row")
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE fact_subjects SET aliases=$1::jsonb,updated_at=clock_timestamp()
		WHERE id=$2 AND account_id=$3 AND realm_id=$4 AND owner_agent_id=$5`,
		string(toAliases), to.ID, p.AccountID, p.RealmID, p.ID); err != nil {
		t.Fatal(err)
	} else if tag.RowsAffected() != 1 {
		t.Fatal("target alias reassignment did not update one row")
	}
	// Match AddFactSubjectAlias's legacy-candidate canonicalization. A correctly
	// bound curation candidate already contains from.CanonicalKey and is immune.
	if _, err := tx.Exec(ctx, `
		UPDATE fact_candidates SET subject_key=$1
		WHERE account_id=$2 AND realm_id=$3 AND owner_agent_id=$4 AND subject_key=$5`,
		to.CanonicalKey, p.AccountID, p.RealmID, p.ID, alias); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
