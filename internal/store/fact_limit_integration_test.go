package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestStoredFactLimitLifecycleAndConcurrentFinalSlotPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	account, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("stored-fact-limit-%d@witwave.ai", time.Now().UnixNano()),
		"stored fact limit",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	newOwner := func(name string) Principal {
		t.Helper()
		agent, createErr := st.CreateAgent(ctx, account.AccountID, realm.ID, name)
		if createErr != nil {
			t.Fatal(createErr)
		}
		return Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
			RealmID: realm.ID, AccountStatus: "active",
		}
	}
	primary := newOwner("primary")
	candidateOwner := newOwner("candidate owner")
	raceOwner := newOwner("race owner")
	nearLimitOwner := newOwner("near limit owner")

	firstInput := SetFactInput{
		Predicate: "preferences/editor", Value: json.RawMessage(`"zed"`),
		SourceKind: FactSourceAgent, IdempotencyKey: "fact-limit-first",
	}
	first, err := st.SetFact(ctx, primary, firstInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetFact(ctx, primary, SetFactInput{
		Predicate: "preferences/shell", Value: json.RawMessage(`"zsh"`),
		SourceKind: FactSourceAgent, IdempotencyKey: "fact-limit-second",
	}); err != nil {
		t.Fatal(err)
	}
	assertStoredFactCountInvariant(ctx, t, st, primary, 2)
	assertStoredFactStatus(ctx, t, st, primary, 2, nil, true, false, false)

	setStoredFactLimitPlan(ctx, t, st, account.AccountID, 1, ptrInt64(2))
	assertStoredFactStatus(ctx, t, st, primary, 2, ptrInt64(2), false, true, false)
	if _, err := st.SetFact(ctx, primary, SetFactInput{
		Predicate: "preferences/terminal", Value: json.RawMessage(`"ghostty"`),
		SourceKind: FactSourceAgent, IdempotencyKey: "fact-limit-at-cap-refusal",
	}); err == nil {
		t.Fatal("new fact at the exact cap succeeded")
	} else {
		assertStoredFactLimitError(t, err, 2, 2)
	}

	atCap, err := st.SetFact(ctx, primary, SetFactInput{
		Predicate: first.Predicate, Value: json.RawMessage(`"vim"`),
		SourceKind: FactSourceAgent, IdempotencyKey: "fact-limit-update-at-cap",
	})
	if err != nil {
		t.Fatalf("update at cap: %v", err)
	}
	if atCap.ID != first.ID || atCap.ResolvedAssertionID == first.ResolvedAssertionID {
		t.Fatalf("update at cap changed address or not assertion: %#v -> %#v", first, atCap)
	}
	assertStoredFactCountInvariant(ctx, t, st, primary, 2)

	setStoredFactLimitPlan(ctx, t, st, account.AccountID, 2, ptrInt64(1))
	assertStoredFactStatus(ctx, t, st, primary, 2, ptrInt64(1), false, false, true)
	overCap, err := st.SetFact(ctx, primary, SetFactInput{
		Predicate: first.Predicate, Value: json.RawMessage(`"helix"`),
		SourceKind: FactSourceAgent, IdempotencyKey: "fact-limit-update-over-cap",
	})
	if err != nil {
		t.Fatalf("update while over cap: %v", err)
	}
	if overCap.ID != first.ID {
		t.Fatalf("over-cap update changed fact id: %#v -> %#v", first, overCap)
	}
	if _, err := st.SetFact(ctx, primary, SetFactInput{
		Predicate: "preferences/font", Value: json.RawMessage(`"Berkeley Mono"`),
		SourceKind: FactSourceAgent, IdempotencyKey: "fact-limit-over-cap-refusal",
	}); err == nil {
		t.Fatal("new fact while over cap succeeded")
	} else {
		assertStoredFactLimitError(t, err, 2, 1)
	}

	replayed, err := st.SetFact(ctx, primary, firstInput)
	if err != nil {
		t.Fatalf("exact replay while over cap: %v", err)
	}
	if replayed.ID != first.ID || replayed.ResolvedAssertionID != first.ResolvedAssertionID {
		t.Fatalf("exact replay changed durable result: %#v -> %#v", first, replayed)
	}
	current, err := st.GetFact(ctx, primary, "self", first.Predicate)
	if err != nil {
		t.Fatal(err)
	}
	if current.ResolvedAssertionID != overCap.ResolvedAssertionID {
		t.Fatalf("late replay rewound current fact: got %#v want assertion %s",
			current, overCap.ResolvedAssertionID)
	}
	assertStoredFactCountInvariant(ctx, t, st, primary, 2)

	setStoredFactLimitPlan(ctx, t, st, account.AccountID, 3, ptrInt64(10))
	for index := 0; index < 8; index++ {
		if _, err := st.SetFact(ctx, nearLimitOwner, SetFactInput{
			Predicate:  fmt.Sprintf("preferences/near-%02d", index),
			Value:      json.RawMessage(fmt.Sprintf(`"value-%02d"`, index)),
			SourceKind: FactSourceAgent,
		}); err != nil {
			t.Fatalf("seed below near-limit threshold %d: %v", index, err)
		}
	}
	belowWarning, err := st.GetFactLimitStatus(ctx, nearLimitOwner)
	if err != nil {
		t.Fatal(err)
	}
	if belowWarning.Used != 8 || belowWarning.Max == nil || *belowWarning.Max != 10 ||
		belowWarning.Remaining == nil || *belowWarning.Remaining != 2 ||
		belowWarning.NearLimit || belowWarning.AtLimit || belowWarning.OverLimit {
		t.Fatalf("8/10 fact status = %#v, want below warning threshold", belowWarning)
	}
	if _, err := st.SetFact(ctx, nearLimitOwner, SetFactInput{
		Predicate: "preferences/near-08", Value: json.RawMessage(`"value-08"`),
		SourceKind: FactSourceAgent,
	}); err != nil {
		t.Fatal(err)
	}
	atWarning, err := st.GetFactLimitStatus(ctx, nearLimitOwner)
	if err != nil {
		t.Fatal(err)
	}
	if atWarning.Used != 9 || atWarning.Max == nil || *atWarning.Max != 10 ||
		atWarning.Remaining == nil || *atWarning.Remaining != 1 ||
		!atWarning.NearLimit || atWarning.AtLimit || atWarning.OverLimit {
		t.Fatalf("9/10 fact status = %#v, want near-limit warning", atWarning)
	}
	assertStoredFactCountInvariant(ctx, t, st, nearLimitOwner, 9)

	setStoredFactLimitPlan(ctx, t, st, account.AccountID, 4, ptrInt64(1))
	existing, err := st.SetFact(ctx, candidateOwner, SetFactInput{
		Predicate: "preferences/color", Value: json.RawMessage(`"blue"`),
		SourceKind: FactSourceAgent, IdempotencyKey: "fact-limit-candidate-base",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertStoredFactCountInvariant(ctx, t, st, candidateOwner, 1)
	assertStoredFactStatus(ctx, t, st, candidateOwner, 1, ptrInt64(1), false, true, false)

	existingCandidate, err := st.ProposeFact(ctx, candidateOwner, ProposeFactInput{
		SetFactInput: SetFactInput{
			Predicate: existing.Predicate, Value: json.RawMessage(`"green"`),
			SourceKind: FactSourceInference, IdempotencyKey: "fact-limit-existing-proposal",
		},
		Reason: "test update at capacity",
	})
	if err != nil {
		t.Fatal(err)
	}
	confirmedExisting, err := st.ConfirmFactCandidateIdempotent(
		ctx, candidateOwner, existingCandidate.ID, "fact-limit-confirm-existing",
	)
	if err != nil {
		t.Fatalf("confirm existing address at cap: %v", err)
	}
	if confirmedExisting.ID != existing.ID {
		t.Fatalf("confirm existing address changed fact id: %#v -> %#v",
			existing, confirmedExisting)
	}
	assertStoredFactCountInvariant(ctx, t, st, candidateOwner, 1)

	newCandidate, err := st.ProposeFact(ctx, candidateOwner, ProposeFactInput{
		SetFactInput: SetFactInput{
			Predicate: "preferences/language", Value: json.RawMessage(`"go"`),
			SourceKind: FactSourceInference, IdempotencyKey: "fact-limit-new-proposal",
		},
		Reason: "test new address at capacity",
	})
	if err != nil {
		t.Fatal(err)
	}
	const newCandidateDecisionKey = "fact-limit-confirm-new"
	if _, err := st.ConfirmFactCandidateIdempotent(
		ctx, candidateOwner, newCandidate.ID, newCandidateDecisionKey,
	); err == nil {
		t.Fatal("confirming a new candidate at cap succeeded")
	} else {
		assertStoredFactLimitError(t, err, 1, 1)
	}
	pending, err := st.GetFactCandidate(ctx, candidateOwner, newCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" {
		t.Fatalf("capacity refusal changed candidate status to %q", pending.Status)
	}

	preview, err := st.DeleteFact(ctx, candidateOwner, DeleteFactInput{
		FactID: confirmedExisting.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteFact(ctx, candidateOwner, DeleteFactInput{
		FactID:                      confirmedExisting.ID,
		ExpectedResolvedAssertionID: preview.PriorResolvedAssertionID,
		ExpectedCandidateRevision:   preview.CandidateRevision,
		IdempotencyKey:              "fact-limit-delete-release",
		Apply:                       true,
	}); err != nil {
		t.Fatal(err)
	}
	assertStoredFactCountInvariant(ctx, t, st, candidateOwner, 0)

	confirmedNew, err := st.ConfirmFactCandidateIdempotent(
		ctx, candidateOwner, newCandidate.ID, newCandidateDecisionKey,
	)
	if err != nil {
		t.Fatalf("confirm after delete released capacity: %v", err)
	}
	confirmedNewReplay, err := st.ConfirmFactCandidateIdempotent(
		ctx, candidateOwner, newCandidate.ID, newCandidateDecisionKey,
	)
	if err != nil {
		t.Fatalf("replay confirmed candidate at cap: %v", err)
	}
	if confirmedNewReplay.ID != confirmedNew.ID ||
		confirmedNewReplay.ResolvedAssertionID != confirmedNew.ResolvedAssertionID {
		t.Fatalf("confirmed candidate replay changed result: %#v -> %#v",
			confirmedNew, confirmedNewReplay)
	}
	assertStoredFactCountInvariant(ctx, t, st, candidateOwner, 1)
	assertStoredFactCountInvariant(ctx, t, st, primary, 2)

	replica, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replica.Close)
	const workers = 20
	successes, refusals := 0, 0
	start := make(chan struct{})
	results := make(chan error, workers)
	for index := 0; index < workers; index++ {
		go func(index int) {
			<-start
			target := st
			if index%2 == 1 {
				target = replica
			}
			_, setErr := target.SetFact(ctx, raceOwner, SetFactInput{
				Predicate:  fmt.Sprintf("preferences/race-%02d", index),
				Value:      json.RawMessage(fmt.Sprintf(`"worker-%02d"`, index)),
				SourceKind: FactSourceAgent,
			})
			results <- setErr
		}(index)
	}
	close(start)
	for index := 0; index < workers; index++ {
		setErr := <-results
		switch {
		case setErr == nil:
			successes++
		case errors.Is(setErr, ErrPlanLimitReached):
			refusals++
			assertStoredFactLimitError(t, setErr, 1, 1)
		default:
			t.Errorf("concurrent final-slot mutation: %v", setErr)
		}
	}
	if successes != 1 || refusals != workers-1 {
		t.Fatalf("concurrent final slot successes=%d refusals=%d", successes, refusals)
	}
	assertStoredFactCountInvariant(ctx, t, st, raceOwner, 1)
}

func setStoredFactLimitPlan(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
	revision int64,
	maximum *int64,
) {
	t.Helper()
	var limits map[string]int64
	if maximum != nil {
		limits = map[string]int64{plans.StoredFactLimit: *maximum}
	}
	hash, err := plans.SnapshotHash("test", limits, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(
		ctx, accountID, revision, hash, "test", limits, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
}

func assertStoredFactStatus(
	ctx context.Context,
	t *testing.T,
	st *Store,
	p Principal,
	wantUsed int64,
	wantMax *int64,
	wantUnlimited, wantAtLimit, wantOverLimit bool,
) {
	t.Helper()
	status, err := st.GetFactLimitStatus(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if status.Used != wantUsed || status.Unlimited != wantUnlimited ||
		status.AtLimit != wantAtLimit || status.OverLimit != wantOverLimit {
		t.Fatalf("fact limit status = %#v, want used=%d unlimited=%t at=%t over=%t",
			status, wantUsed, wantUnlimited, wantAtLimit, wantOverLimit)
	}
	if wantMax == nil {
		if status.Max != nil || status.Remaining != nil {
			t.Fatalf("unlimited status exposed cap fields: %#v", status)
		}
		return
	}
	if status.Max == nil || *status.Max != *wantMax || status.Remaining == nil {
		t.Fatalf("fact limit cap fields = %#v, want max=%d", status, *wantMax)
	}
	wantRemaining := *wantMax - wantUsed
	if wantRemaining < 0 {
		wantRemaining = 0
	}
	if *status.Remaining != wantRemaining {
		t.Fatalf("fact limit remaining=%d want=%d: %#v",
			*status.Remaining, wantRemaining, status)
	}
}

func assertStoredFactLimitError(t *testing.T, err error, wantUsed, wantMax int64) {
	t.Helper()
	if !errors.Is(err, ErrPlanLimitReached) {
		t.Fatalf("fact limit error = %v, want ErrPlanLimitReached", err)
	}
	var detail *FactLimitError
	if !errors.As(err, &detail) {
		t.Fatalf("fact limit error = %#v, want *FactLimitError", err)
	}
	if detail.Status.Used != wantUsed || detail.Status.Max == nil ||
		*detail.Status.Max != wantMax {
		t.Fatalf("fact limit detail = %#v, want used=%d max=%d",
			detail.Status, wantUsed, wantMax)
	}
}

func assertStoredFactCountInvariant(
	ctx context.Context,
	t *testing.T,
	st *Store,
	p Principal,
	want int64,
) {
	t.Helper()
	var persisted, derived int64
	if err := st.pool.QueryRow(ctx, `
		SELECT owner.active_fact_count,
		       (SELECT count(*)
		          FROM facts fact
		         WHERE fact.account_id=$1
		           AND fact.realm_id=$2
		           AND fact.owner_agent_id=$3
		           AND fact.deleted_at IS NULL
		           AND fact.resolved_assertion_id IS NOT NULL)
		  FROM agents owner
		 WHERE owner.id=$3 AND owner.realm_id=$2`,
		p.AccountID, p.RealmID, p.ID,
	).Scan(&persisted, &derived); err != nil {
		t.Fatal(err)
	}
	if persisted != want || derived != want {
		t.Fatalf("active fact count owner=%s persisted=%d derived=%d want=%d",
			p.ID, persisted, derived, want)
	}
}

func ptrInt64(value int64) *int64 {
	return &value
}
