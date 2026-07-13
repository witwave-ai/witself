package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFactMutationIdempotencyPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx, "fact-idempotency@witwave.ai", "fact idempotency", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteFactTestAccount(ctx, st, provisioned.AccountID) }()
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
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID, RealmID: realm.ID, AccountStatus: "active"}

	setInput := SetFactInput{
		Predicate: "preferences/editor", Value: json.RawMessage(`"zed"`),
		SourceKind: FactSourceAgent, IdempotencyKey: "set-editor-1",
	}
	first, err := st.SetFact(ctx, p, setInput)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := st.SetFact(ctx, p, setInput)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != first.ID || replayed.ResolvedAssertionID != first.ResolvedAssertionID {
		t.Fatalf("set replay changed result: %#v -> %#v", first, replayed)
	}
	history, err := st.FactHistory(ctx, p, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("set replay appended %d assertions", len(history))
	}
	changedSet := setInput
	changedSet.Value = json.RawMessage(`"vim"`)
	if _, err := st.SetFact(ctx, p, changedSet); !errors.Is(err, ErrFactIdempotencyConflict) {
		t.Fatalf("changed set retry error = %v", err)
	}
	laterSet := setInput
	laterSet.Value = json.RawMessage(`"emacs"`)
	laterSet.IdempotencyKey = "set-editor-2"
	latestEditor, err := st.SetFact(ctx, p, laterSet)
	if err != nil {
		t.Fatal(err)
	}
	lateSetReplay, err := st.SetFact(ctx, p, setInput)
	if err != nil {
		t.Fatal(err)
	}
	if lateSetReplay.ResolvedAssertionID != first.ResolvedAssertionID || string(lateSetReplay.Value) != `"zed"` {
		t.Fatalf("late set retry did not replay original result: %#v", lateSetReplay)
	}
	currentEditor, err := st.GetFact(ctx, p, "self", setInput.Predicate)
	if err != nil {
		t.Fatal(err)
	}
	if currentEditor.ResolvedAssertionID != latestEditor.ResolvedAssertionID || string(currentEditor.Value) != `"emacs"` {
		t.Fatalf("late set retry changed current fact: %#v", currentEditor)
	}

	proposal := ProposeFactInput{SetFactInput: SetFactInput{
		Predicate: "identity/nickname", Value: json.RawMessage(`"S"`),
		IdempotencyKey: "proposal-nickname-1",
	}, Reason: "explicit durable preference"}
	candidate, err := st.ProposeFact(ctx, p, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE fact_candidates
		SET status='confirmed', decision_assertion_id=$1 WHERE id=$2`, first.ResolvedAssertionID, candidate.ID); err == nil {
		t.Fatal("empty decision key accepted a decision assertion")
	}
	candidateReplay, err := st.ProposeFact(ctx, p, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if candidateReplay.ID != candidate.ID {
		t.Fatalf("proposal replay changed candidate: %#v -> %#v", candidate, candidateReplay)
	}
	halfConfidence := 0.5
	equivalentProposal := proposal
	equivalentProposal.Confidence = &halfConfidence
	equivalentReplay, err := st.ProposeFact(ctx, p, equivalentProposal)
	if err != nil || equivalentReplay.ID != candidate.ID {
		t.Fatalf("explicit proposal default did not replay: %#v / %v", equivalentReplay, err)
	}
	fullConfidence := 1.0
	differentConfidence := proposal
	differentConfidence.Confidence = &fullConfidence
	if _, err := st.ProposeFact(ctx, p, differentConfidence); !errors.Is(err, ErrFactIdempotencyConflict) {
		t.Fatalf("different proposal confidence retry error = %v", err)
	}
	changedProposal := proposal
	changedProposal.Value = json.RawMessage(`"Scott"`)
	if _, err := st.ProposeFact(ctx, p, changedProposal); !errors.Is(err, ErrFactIdempotencyConflict) {
		t.Fatalf("changed proposal retry error = %v", err)
	}

	confirmed, err := st.ConfirmFactCandidateIdempotent(ctx, p, candidate.ID, "confirm-nickname-1")
	if err != nil {
		t.Fatal(err)
	}
	confirmedReplay, err := st.ConfirmFactCandidateIdempotent(ctx, p, candidate.ID, "confirm-nickname-1")
	if err != nil {
		t.Fatal(err)
	}
	if confirmedReplay.ID != confirmed.ID || confirmedReplay.ResolvedAssertionID != confirmed.ResolvedAssertionID {
		t.Fatalf("confirm replay changed result: %#v -> %#v", confirmed, confirmedReplay)
	}
	laterNickname, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: proposal.Predicate, Value: json.RawMessage(`"Scott"`),
		SourceKind: FactSourceAgent, IdempotencyKey: "set-nickname-after-confirm-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	lateConfirmReplay, err := st.ConfirmFactCandidateIdempotent(ctx, p, candidate.ID, "confirm-nickname-1")
	if err != nil {
		t.Fatal(err)
	}
	if lateConfirmReplay.ResolvedAssertionID != confirmed.ResolvedAssertionID || string(lateConfirmReplay.Value) != `"S"` {
		t.Fatalf("late confirm retry did not replay original result: %#v", lateConfirmReplay)
	}
	currentNickname, err := st.GetFact(ctx, p, "self", proposal.Predicate)
	if err != nil {
		t.Fatal(err)
	}
	if currentNickname.ResolvedAssertionID != laterNickname.ResolvedAssertionID || string(currentNickname.Value) != `"Scott"` {
		t.Fatalf("late confirm retry changed current fact: %#v", currentNickname)
	}
	if _, err := st.RejectFactCandidateIdempotent(ctx, p, candidate.ID, "confirm-nickname-1"); !errors.Is(err, ErrFactIdempotencyConflict) {
		t.Fatalf("opposite decision retry error = %v", err)
	}

	secondCandidate, err := st.ProposeFact(ctx, p, ProposeFactInput{SetFactInput: SetFactInput{
		Predicate: "identity/timezone", Value: json.RawMessage(`"America/Denver"`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConfirmFactCandidateIdempotent(ctx, p, secondCandidate.ID, "confirm-nickname-1"); !errors.Is(err, ErrFactIdempotencyConflict) {
		t.Fatalf("decision key reused on another candidate = %v", err)
	}

	rejected, err := st.RejectFactCandidateIdempotent(ctx, p, secondCandidate.ID, "reject-timezone-1")
	if err != nil {
		t.Fatal(err)
	}
	rejectedReplay, err := st.RejectFactCandidateIdempotent(ctx, p, secondCandidate.ID, "reject-timezone-1")
	if err != nil {
		t.Fatal(err)
	}
	if rejectedReplay.ID != rejected.ID || rejectedReplay.Status != "rejected" {
		t.Fatalf("reject replay changed result: %#v -> %#v", rejected, rejectedReplay)
	}
	if _, err := st.ConfirmFactCandidateIdempotent(ctx, p, secondCandidate.ID, "reject-timezone-1"); !errors.Is(err, ErrFactIdempotencyConflict) {
		t.Fatalf("confirm after idempotent reject error = %v", err)
	}

	otherAgent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "other")
	if err != nil {
		t.Fatal(err)
	}
	other := p
	other.ID = otherAgent.ID
	otherFact, err := st.SetFact(ctx, other, setInput)
	if err != nil {
		t.Fatalf("same key should be agent scoped: %v", err)
	}
	if otherFact.OwnerAgentID != otherAgent.ID {
		t.Fatalf("other agent fact owner = %q", otherFact.OwnerAgentID)
	}

	tooLong := setInput
	tooLong.IdempotencyKey = strings.Repeat("x", maxFactIdempotencyKeyBytes+1)
	if _, err := st.SetFact(ctx, p, tooLong); !errors.Is(err, ErrFactInputInvalid) {
		t.Fatalf("oversized key error = %v", err)
	}

	const workers = 8
	concurrentSet := SetFactInput{
		Predicate: "preferences/shell", Value: json.RawMessage(`"zsh"`),
		SourceKind: FactSourceAgent, IdempotencyKey: "concurrent-set-1",
	}
	setResults := make(chan Fact, workers)
	setErrors := make(chan error, workers)
	start := make(chan struct{})
	for range workers {
		go func() {
			<-start
			fact, err := st.SetFact(ctx, p, concurrentSet)
			setResults <- fact
			setErrors <- err
		}()
	}
	close(start)
	var concurrentFact Fact
	for range workers {
		if err := <-setErrors; err != nil {
			t.Fatalf("concurrent set: %v", err)
		}
		fact := <-setResults
		if concurrentFact.ID == "" {
			concurrentFact = fact
		}
		if fact.ID != concurrentFact.ID || fact.ResolvedAssertionID != concurrentFact.ResolvedAssertionID {
			t.Fatalf("concurrent set results differ: %#v / %#v", concurrentFact, fact)
		}
	}
	concurrentHistory, err := st.FactHistory(ctx, p, concurrentFact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(concurrentHistory) != 1 {
		t.Fatalf("concurrent set appended %d assertions", len(concurrentHistory))
	}

	concurrentProposal := ProposeFactInput{SetFactInput: SetFactInput{
		Predicate: "preferences/terminal", Value: json.RawMessage(`"ghostty"`),
		IdempotencyKey: "concurrent-proposal-1",
	}, Reason: "concurrent retry test"}
	proposalResults := make(chan FactCandidate, workers)
	proposalErrors := make(chan error, workers)
	start = make(chan struct{})
	for range workers {
		go func() {
			<-start
			candidate, err := st.ProposeFact(ctx, p, concurrentProposal)
			proposalResults <- candidate
			proposalErrors <- err
		}()
	}
	close(start)
	var concurrentCandidate FactCandidate
	for range workers {
		if err := <-proposalErrors; err != nil {
			t.Fatalf("concurrent proposal: %v", err)
		}
		candidate := <-proposalResults
		if concurrentCandidate.ID == "" {
			concurrentCandidate = candidate
		}
		if candidate.ID != concurrentCandidate.ID {
			t.Fatalf("concurrent proposal results differ: %#v / %#v", concurrentCandidate, candidate)
		}
	}
	var proposalCount int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM fact_candidates
		WHERE owner_agent_id=$1 AND idempotency_key=$2`, p.ID, concurrentProposal.IdempotencyKey).Scan(&proposalCount); err != nil {
		t.Fatal(err)
	}
	if proposalCount != 1 {
		t.Fatalf("concurrent proposal created %d candidates", proposalCount)
	}

	decisionResults := make(chan Fact, workers)
	decisionErrors := make(chan error, workers)
	start = make(chan struct{})
	for range workers {
		go func() {
			<-start
			fact, err := st.ConfirmFactCandidateIdempotent(ctx, p, concurrentCandidate.ID, "concurrent-confirm-1")
			decisionResults <- fact
			decisionErrors <- err
		}()
	}
	close(start)
	var concurrentConfirmed Fact
	for range workers {
		if err := <-decisionErrors; err != nil {
			t.Fatalf("concurrent confirm: %v", err)
		}
		fact := <-decisionResults
		if concurrentConfirmed.ID == "" {
			concurrentConfirmed = fact
		}
		if fact.ID != concurrentConfirmed.ID || fact.ResolvedAssertionID != concurrentConfirmed.ResolvedAssertionID {
			t.Fatalf("concurrent confirm results differ: %#v / %#v", concurrentConfirmed, fact)
		}
	}
}
