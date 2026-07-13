package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

// TestFactPostgresRoundTrip is opt-in because it needs disposable Postgres. It
// covers migration 0022, assertion supersession, redaction, and usage emission.
func TestFactPostgresRoundTrip(t *testing.T) {
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

	provisioned, err := st.ProvisionAccount(ctx, "fact-test@witwave.ai", "fact test", time.Hour)
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
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "scott")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID, RealmID: realm.ID, AccountStatus: "active"}

	first, err := st.SetFact(ctx, p, SetFactInput{Predicate: "preferences/editor", Value: json.RawMessage(`"vim"`), SourceKind: FactSourceAgent})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.SetFact(ctx, p, SetFactInput{Predicate: "preferences/editor", Value: json.RawMessage(`"zed"`), SourceKind: FactSourceAgent, Sensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || second.ResolvedAssertionID == first.ResolvedAssertionID {
		t.Fatalf("fact identity/assertions = %#v -> %#v", first, second)
	}
	third, err := st.SetFact(ctx, p, SetFactInput{Predicate: "preferences/editor", Value: json.RawMessage(`"zed"`), SourceKind: FactSourceAgent, SourceRef: "witself://transcript/private-entry"})
	if err != nil {
		t.Fatal(err)
	}
	if !third.Sensitive {
		t.Fatalf("ordinary update declassified sensitive fact: %#v", third)
	}
	got, err := st.GetFact(ctx, p, "self", "preferences/editor")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Value) != `"zed"` || got.SourceRef == "" {
		t.Fatalf("value = %s", got.Value)
	}
	listed, err := st.ListFacts(ctx, p, FactListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || string(listed[0].Value) != "null" || listed[0].SourceRef != "" {
		t.Fatalf("listed = %#v", listed)
	}
	history, err := st.FactHistory(ctx, p, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[0].SupersedesID != history[1].ID || history[1].SupersedesID != history[2].ID {
		t.Fatalf("history = %#v", history)
	}
	var usageCount int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE subject_type = 'fact' AND subject_id = $1`, first.ID).Scan(&usageCount); err != nil {
		t.Fatal(err)
	}
	if usageCount != 2 {
		t.Fatalf("fact usage events = %d", usageCount)
	}
	retrievalModes := map[string]int{}
	rows, err := st.pool.Query(ctx, `
		SELECT metadata->>'retrieval_mode', count(*)
		FROM usage_events
		WHERE subject_type = 'fact' AND subject_id = $1
		GROUP BY metadata->>'retrieval_mode'`, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var mode string
		var count int
		if err := rows.Scan(&mode, &count); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		retrievalModes[mode] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if retrievalModes[string(FactRetrievalModeExact)] != 1 || retrievalModes[string(FactRetrievalModeSearch)] != 1 {
		t.Fatalf("fact retrieval modes = %#v", retrievalModes)
	}

	spouse, err := st.UpsertFactSubject(ctx, p, UpsertFactSubjectInput{
		CanonicalKey: "person_spouse",
		DisplayName:  "Spouse",
	})
	if err != nil {
		t.Fatal(err)
	}
	spouse, err = st.AddFactSubjectAlias(ctx, p, spouse.CanonicalKey, " My   Spouse ")
	if err != nil {
		t.Fatal(err)
	}
	spouse, err = st.AddFactSubjectAlias(ctx, p, spouse.CanonicalKey, "partner")
	if err != nil {
		t.Fatal(err)
	}
	if len(spouse.Aliases) != 2 || spouse.Aliases[0] != "my spouse" || spouse.Aliases[1] != "partner" {
		t.Fatalf("spouse aliases = %#v", spouse.Aliases)
	}
	if _, err := st.AddFactSubjectAlias(ctx, p, spouse.CanonicalKey, "self"); !errors.Is(err, ErrFactInputInvalid) {
		t.Fatalf("reserved self alias error = %v", err)
	}
	if _, err := st.UpsertFactSubject(ctx, p, UpsertFactSubjectInput{CanonicalKey: "partner"}); !errors.Is(err, ErrFactInputInvalid) {
		t.Fatalf("alias/canonical collision error = %v", err)
	}
	if _, err := st.SetFact(ctx, p, SetFactInput{
		Subject: "my spouse", Predicate: "identity/name", Value: json.RawMessage(`"Taylor"`),
		SourceKind: FactSourceAgent,
	}); err != nil {
		t.Fatal(err)
	}
	byAlias, err := st.GetFact(ctx, p, " MY   SPOUSE ", "identity/name")
	if err != nil {
		t.Fatal(err)
	}
	if byAlias.Subject != "person_spouse" || string(byAlias.Value) != `"Taylor"` {
		t.Fatalf("alias fact = %#v", byAlias)
	}
	unused, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "identity/timezone", Value: json.RawMessage(`"America/Denver"`),
		SourceKind: FactSourceAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetFact(ctx, p, "self", "preferences/editor"); err != nil {
		t.Fatal(err)
	}
	ranked, err := st.ListFacts(ctx, p, FactListOptions{OrderByUsage: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(ranked) != 3 || ranked[0].ID != first.ID || ranked[0].UsageCount != 3 ||
		ranked[0].LastUsedAt == nil || ranked[1].ID != byAlias.ID ||
		ranked[1].UsageCount != 1 || ranked[1].LastUsedAt == nil ||
		ranked[2].ID != unused.ID || ranked[2].UsageCount != 0 || ranked[2].LastUsedAt != nil {
		t.Fatalf("usage-ranked facts = %#v", ranked)
	}
	unusedFacts, err := st.ListFacts(ctx, p, FactListOptions{UnusedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(unusedFacts) != 0 {
		t.Fatalf("facts returned by search are still unused: %#v", unusedFacts)
	}
	aliasList, err := st.ListFacts(ctx, p, FactListOptions{Subject: "partner"})
	if err != nil {
		t.Fatal(err)
	}
	if len(aliasList) != 1 || aliasList[0].Subject != "person_spouse" {
		t.Fatalf("alias fact list = %#v", aliasList)
	}
	hydrated, err := st.ListFacts(ctx, p, FactListOptions{
		Subject: "self", Limit: 50, RetrievalMode: FactRetrievalModeSelfHydration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hydrated) != 2 {
		t.Fatalf("hydrated facts = %#v", hydrated)
	}
	var selfHydrationEvents, selfHydrationRanked int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (
			WHERE COALESCE(metadata->>'retrieval_mode', 'exact') IN ('exact', 'search', 'temporal')
		)
		FROM usage_events
		WHERE subject_type = 'fact' AND metadata->>'retrieval_mode' = 'self_hydration'
		  AND subject_id = ANY($1)`, []string{first.ID, unused.ID}).Scan(&selfHydrationEvents, &selfHydrationRanked); err != nil {
		t.Fatal(err)
	}
	if selfHydrationEvents != 2 || selfHydrationRanked != 0 {
		t.Fatalf("self hydration usage = %d events / %d ranked", selfHydrationEvents, selfHydrationRanked)
	}
	selfCount, err := st.CountFacts(ctx, p, FactListOptions{Subject: "self", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if selfCount != 2 {
		t.Fatalf("self fact count = %d, want 2", selfCount)
	}
	allCount, err := st.CountFacts(ctx, p, FactListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if allCount != 3 {
		t.Fatalf("all fact count = %d, want 3", allCount)
	}
	observedAt := time.Date(2026, 7, 12, 17, 30, 0, 0, time.UTC)
	validFrom := time.Date(2026, 7, 12, 18, 30, 0, 0, time.UTC)
	spouseEditor, err := st.SetFact(ctx, p, SetFactInput{
		Subject: "partner", Predicate: "preferences/editor", Value: json.RawMessage(`"vim"`),
		SourceKind: FactSourceAgent, Sensitive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := st.ProposeFact(ctx, p, ProposeFactInput{SetFactInput: SetFactInput{
		Subject: "my spouse", Predicate: "preferences/editor", Value: json.RawMessage(`"zed"`),
		SourceKind: FactSourceInference, ObservedAt: observedAt, ValidFrom: &validFrom,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Subject != "person_spouse" || candidate.Status != "conflict" ||
		candidate.ObservedAssertionID != spouseEditor.ResolvedAssertionID ||
		!candidate.ObservedAt.Equal(observedAt) ||
		candidate.ValidFrom == nil || !candidate.ValidFrom.Equal(validFrom) {
		t.Fatalf("candidate = %#v", candidate)
	}
	detail, err := st.GetFactCandidate(ctx, p, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Sensitive || string(detail.Value) != `"zed"` || detail.ObservedAssertionID != spouseEditor.ResolvedAssertionID {
		t.Fatalf("candidate detail = %#v", detail)
	}
	otherAgent := p
	otherAgent.ID = "agent_other"
	if _, err := st.GetFactCandidate(ctx, otherAgent, candidate.ID); !errors.Is(err, ErrFactNotFound) {
		t.Fatalf("cross-agent candidate detail error = %v", err)
	}
	confirmedCandidate, err := st.ConfirmFactCandidate(ctx, p, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmedCandidate.SourceKind != FactSourceInference ||
		!confirmedCandidate.Sensitive ||
		!confirmedCandidate.ObservedAt.Equal(observedAt) || confirmedCandidate.ConfirmedAt == nil ||
		confirmedCandidate.ValidFrom == nil || !confirmedCandidate.ValidFrom.Equal(validFrom) {
		t.Fatalf("confirmed candidate fact = %#v", confirmedCandidate)
	}

	staleBase, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "preferences/color", Value: json.RawMessage(`"blue"`), SourceKind: FactSourceAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	staleCandidate, err := st.ProposeFact(ctx, p, ProposeFactInput{SetFactInput: SetFactInput{
		Predicate: "preferences/color", Value: json.RawMessage(`"green"`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if staleCandidate.ObservedAssertionID != staleBase.ResolvedAssertionID || staleCandidate.Status != "conflict" {
		t.Fatalf("stale candidate baseline = %#v", staleCandidate)
	}
	if _, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "preferences/color", Value: json.RawMessage(`"red"`), SourceKind: FactSourceAgent,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConfirmFactCandidate(ctx, p, staleCandidate.ID); !errors.Is(err, ErrFactConflict) {
		t.Fatalf("stale candidate confirmation error = %v", err)
	}

	sensitiveCandidate, err := st.ProposeFact(ctx, p, ProposeFactInput{SetFactInput: SetFactInput{
		Predicate: "identity/home_address", Value: json.RawMessage(`"123 Main St"`), Sensitive: true,
		SourceRef: "witself://transcript/private-address-entry",
	}, Reason: "contains the private home address"})
	if err != nil {
		t.Fatal(err)
	}
	pendingCandidates, err := st.ListFactCandidates(ctx, p, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingCandidates) != 1 || string(pendingCandidates[0].Value) != "null" ||
		pendingCandidates[0].SourceRef != "" || pendingCandidates[0].Reason != "" {
		t.Fatalf("sensitive candidate inventory = %#v", pendingCandidates)
	}
	sensitiveDetail, err := st.GetFactCandidate(ctx, p, sensitiveCandidate.ID)
	if err != nil || sensitiveDetail.SourceRef == "" || sensitiveDetail.Reason == "" {
		t.Fatalf("sensitive candidate detail = %#v / %v", sensitiveDetail, err)
	}
	privateDate, err := st.ProposeFact(ctx, p, ProposeFactInput{SetFactInput: SetFactInput{
		Predicate: "identity/private-anniversary", ValueType: "date",
		Value: json.RawMessage(`"2011-04-03"`), Recurrence: FactRecurrenceAnnual, Sensitive: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	privateDateDetail, err := st.GetFactCandidate(ctx, p, privateDate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(privateDateDetail.Value) != `"2011-04-03"` || privateDateDetail.Recurrence != FactRecurrenceAnnual {
		t.Fatalf("private annual candidate detail = %#v", privateDateDetail)
	}
	subjects, err := st.ListFactSubjects(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 2 || subjects[0].CanonicalKey != "person_spouse" || subjects[1].CanonicalKey != "self" {
		t.Fatalf("subjects = %#v", subjects)
	}
	birthday, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "identity/birth-date", ValueType: "date",
		Value: json.RawMessage(`"1990-07-13"`), Recurrence: FactRecurrenceAnnual,
		SourceKind: FactSourceAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if birthday.Recurrence != FactRecurrenceAnnual {
		t.Fatalf("birthday recurrence = %q", birthday.Recurrence)
	}
	occurrences, err := st.UpcomingFacts(ctx, p, UpcomingFactOptions{
		From:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(occurrences) != 3 || occurrences[0].OccursOn != "2026-07-13" ||
		occurrences[1].OccursOn != "2027-07-13" || occurrences[2].OccursOn != "2028-07-13" {
		t.Fatalf("annual birthday occurrences = %#v", occurrences)
	}
}

func deleteFactTestAccount(ctx context.Context, st *Store, accountID string) error {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range []string{
		`DELETE FROM usage_rollups WHERE account_id = $1`,
		`DELETE FROM usage_events WHERE account_id = $1`,
		`DELETE FROM fact_candidates WHERE account_id = $1`,
		`DELETE FROM fact_assertions WHERE account_id = $1`,
		`DELETE FROM facts WHERE account_id = $1`,
		`DELETE FROM fact_subjects WHERE account_id = $1`,
		`DELETE FROM tokens WHERE account_id = $1`,
		`DELETE FROM agents WHERE realm_id IN (SELECT id FROM realms WHERE account_id = $1)`,
		`DELETE FROM realms WHERE account_id = $1`,
		`DELETE FROM operators WHERE account_id = $1`,
		`DELETE FROM accounts WHERE id = $1`,
	} {
		if _, err := tx.Exec(ctx, statement, accountID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
