package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestValidateAndRecordFactCandidateScope(t *testing.T) {
	const accountID = "acc_target"
	validRow := func() map[string]any {
		return map[string]any{
			"id": "fcand_1", "account_id": accountID,
			"realm_id": "rlm_ok", "owner_agent_id": "agt_ok",
			"subject_key": "self", "predicate": "preferences/editor",
			"value_type": "string", "value": "zed", "cardinality": "one",
			"recurrence": "",
			"sensitive":  false, "source_ref": "transcript:1", "confidence": 0.7,
			"observed_at": "2026-07-12T17:00:00Z", "valid_from": nil, "valid_until": nil,
			"reason": "new observation", "status": "conflict",
			"conflict_fact_id": "fact_ok", "resolved_fact_id": nil,
			"observed_assertion_id": "fas_ok",
			"proposed_at":           "2026-07-12T18:00:00Z", "decided_at": nil,
		}
	}
	newScopedImport := func() *importCtx {
		ic := newImportCtx(accountID)
		ic.realms["rlm_ok"] = true
		ic.agents["agt_ok"] = true
		ic.agentRealms["agt_ok"] = "rlm_ok"
		ic.facts["fact_ok"] = factImportScope{realmID: "rlm_ok", ownerAgentID: "agt_ok", subjectKey: "self", predicate: "preferences/editor"}
		ic.facts["fact_other"] = factImportScope{realmID: "rlm_ok", ownerAgentID: "agt_ok", subjectKey: "self", predicate: "preferences/theme"}
		ic.assertions["fas_ok"] = "fact_ok"
		ic.assertions["fas_other"] = "fact_other"
		return ic
	}

	tests := []struct {
		name   string
		mutate func(map[string]any, *importCtx)
		want   string
	}{
		{name: "matching candidate is accepted"},
		{name: "wrong account is refused", mutate: func(row map[string]any, _ *importCtx) {
			row["account_id"] = "acc_victim"
		}, want: "does not match manifest"},
		{name: "owner outside realm is refused", mutate: func(_ map[string]any, ic *importCtx) {
			ic.agentRealms["agt_ok"] = "rlm_other"
		}, want: "outside realm"},
		{name: "foreign conflict fact is refused", mutate: func(row map[string]any, _ *importCtx) {
			row["conflict_fact_id"] = "fact_victim"
		}, want: "conflict_fact_id"},
		{name: "foreign resolved fact is refused", mutate: func(row map[string]any, _ *importCtx) {
			row["conflict_fact_id"] = nil
			row["resolved_fact_id"] = "fact_victim"
			row["status"] = "confirmed"
			row["decided_at"] = "2026-07-12T19:00:00Z"
		}, want: "resolved_fact_id"},
		{name: "foreign observed assertion is refused", mutate: func(row map[string]any, _ *importCtx) {
			row["observed_assertion_id"] = "fas_victim"
		}, want: "observed_assertion_id"},
		{name: "different-address conflict fact is refused", mutate: func(row map[string]any, _ *importCtx) {
			row["conflict_fact_id"] = "fact_other"
			row["observed_assertion_id"] = "fas_other"
		}, want: "different fact address"},
		{name: "different-address observed assertion is refused", mutate: func(row map[string]any, _ *importCtx) {
			row["observed_assertion_id"] = "fas_other"
		}, want: "different fact address"},
		{name: "different-address resolved fact is refused", mutate: func(row map[string]any, _ *importCtx) {
			row["status"] = "confirmed"
			row["conflict_fact_id"] = nil
			row["observed_assertion_id"] = nil
			row["resolved_fact_id"] = "fact_other"
			row["decided_at"] = "2026-07-12T19:00:00Z"
		}, want: "different fact address"},
		{name: "different-address decision assertion is refused", mutate: func(row map[string]any, _ *importCtx) {
			row["status"] = "confirmed"
			row["conflict_fact_id"] = nil
			row["observed_assertion_id"] = nil
			row["resolved_fact_id"] = "fact_ok"
			row["decision_assertion_id"] = "fas_other"
			row["decided_at"] = "2026-07-12T19:00:00Z"
		}, want: "does not belong"},
		{name: "invalid subject is refused", mutate: func(row map[string]any, _ *importCtx) {
			row["subject_key"] = "My Spouse"
		}, want: "invalid fact input"},
		{name: "invalid predicate is refused", mutate: func(row map[string]any, _ *importCtx) {
			row["predicate"] = "preferences//editor"
		}, want: "invalid fact input"},
		{name: "invalid logical value is refused", mutate: func(row map[string]any, _ *importCtx) {
			row["value_type"] = "date"
			row["value"] = "not-a-date"
		}, want: "invalid fact input"},
		{name: "annual recurrence requires a date", mutate: func(row map[string]any, _ *importCtx) {
			row["recurrence"] = "annual"
		}, want: "invalid fact input"},
		{name: "inconsistent lifecycle is refused", mutate: func(row map[string]any, _ *importCtx) {
			row["status"] = "confirmed"
		}, want: "lifecycle"},
		{name: "decision before proposal is refused", mutate: func(row map[string]any, _ *importCtx) {
			row["status"] = "rejected"
			row["decided_at"] = "2026-07-12T17:00:00Z"
		}, want: "precedes"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ic := newScopedImport()
			row := validRow()
			if tc.mutate != nil {
				tc.mutate(row, ic)
			}
			err := ic.validateAndRecord("fact_candidates", row)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validate candidate: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrArchiveContent) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want ErrArchiveContent containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateImportedFactAssertionContent(t *testing.T) {
	validRow := func() map[string]any {
		return map[string]any{
			"value_type": "date", "value": "2026-07-12", "recurrence": "annual",
			"source_kind": "agent", "source_ref": "witself://transcript/trn_1/entry/ent_1",
			"confidence": 1.0, "observed_at": "2026-07-12T18:00:00Z",
			"confirmed_at": nil, "valid_from": nil, "valid_until": nil,
			"created_at": "2026-07-12T18:00:00Z",
		}
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
		wantOK bool
	}{
		{name: "canonical assertion", wantOK: true},
		{name: "invalid logical date", mutate: func(row map[string]any) { row["value"] = "July 12" }},
		{name: "noncanonical datetime", mutate: func(row map[string]any) {
			row["value_type"] = "datetime"
			row["recurrence"] = ""
			row["value"] = "2026-07-12T12:00:00-06:00"
		}},
		{name: "credential URL", mutate: func(row map[string]any) {
			row["value_type"] = "url"
			row["recurrence"] = ""
			row["value"] = "https://user:password@example.com/private"
		}},
		{name: "invalid observed timestamp", mutate: func(row map[string]any) { row["observed_at"] = "yesterday" }},
		{name: "invalid validity interval", mutate: func(row map[string]any) {
			row["valid_from"] = "2026-08-01T00:00:00Z"
			row["valid_until"] = "2026-07-01T00:00:00Z"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := validRow()
			if tc.mutate != nil {
				tc.mutate(row)
			}
			err := validateImportedFactAssertionContent(row)
			if tc.wantOK && err != nil {
				t.Fatalf("validate assertion: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("invalid assertion was accepted")
			}
		})
	}
}

func TestValidateImportedFactSubjectContentAndNamespace(t *testing.T) {
	newContext := func() *importCtx {
		ic := newImportCtx("acc_target")
		ic.realms["rlm_1"] = true
		ic.agents["agt_1"] = true
		ic.agentRealms["agt_1"] = "rlm_1"
		return ic
	}
	row := func(id, key string, aliases ...any) map[string]any {
		return map[string]any{
			"id": id, "account_id": "acc_target", "realm_id": "rlm_1",
			"owner_agent_id": "agt_1", "canonical_key": key,
			"display_name": "Subject", "aliases": aliases,
		}
	}

	ic := newContext()
	if err := ic.validateAndRecord("fact_subjects", row("sub_1", "person_spouse", "my wife")); err != nil {
		t.Fatalf("valid subject: %v", err)
	}
	if err := ic.validateAndRecord("fact_subjects", row("sub_2", "person_partner", "my wife")); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("alias collision error = %v", err)
	}

	for name, bad := range map[string]map[string]any{
		"non-string alias":   row("sub_bad_1", "person_bad_1", 42),
		"unnormalized alias": row("sub_bad_2", "person_bad_2", "My Wife"),
		"duplicate alias":    row("sub_bad_3", "person_bad_3", "partner", "partner"),
		"reserved alias":     row("sub_bad_4", "person_bad_4", "myself"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := newContext().validateAndRecord("fact_subjects", bad); !errors.Is(err, ErrArchiveContent) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

// TestFactCandidateArchiveRoundTrip is opt-in because it needs disposable
// Postgres. It covers migration 0023 portability across every candidate state.
func TestFactCandidateArchiveRoundTrip(t *testing.T) {
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

	provisioned, err := st.ProvisionAccount(ctx, "candidate-portability@witwave.ai", "candidate portability", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_, _ = st.pool.Exec(ctx, `DELETE FROM fact_candidates WHERE account_id = $1`, provisioned.AccountID)
		_ = deleteFactTestAccount(ctx, st, provisioned.AccountID)
	}
	defer cleanup()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "archivist")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID, RealmID: realm.ID, AccountStatus: "active"}

	if _, err := st.ProposeFact(ctx, p, ProposeFactInput{
		SetFactInput: SetFactInput{Predicate: "identity/birth-date", ValueType: "date", Value: json.RawMessage(`"1990-07-13"`), Recurrence: FactRecurrenceAnnual, SourceRef: "conversation:1"},
		Reason:       "user mentioned their birthday once",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetFact(ctx, p, SetFactInput{Predicate: "preferences/editor", Value: json.RawMessage(`"vim"`), SourceKind: FactSourceAgent}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProposeFact(ctx, p, ProposeFactInput{
		SetFactInput: SetFactInput{Predicate: "preferences/editor", Value: json.RawMessage(`"zed"`)},
		Reason:       "new conflicting observation",
	}); err != nil {
		t.Fatal(err)
	}
	confirmedInput := ProposeFactInput{
		SetFactInput: SetFactInput{Predicate: "identity/wedding-date", ValueType: "date", Value: json.RawMessage(`"2010-06-12"`), Recurrence: FactRecurrenceAnnual},
		Reason:       "explicit anniversary",
	}
	confirmedInput.IdempotencyKey = "archive-confirmed-proposal-1"
	confirmed, err := st.ProposeFact(ctx, p, confirmedInput)
	if err != nil {
		t.Fatal(err)
	}
	confirmedFact, err := st.ConfirmFactCandidateIdempotent(ctx, p, confirmed.ID, "archive-confirmed-decision-1")
	if err != nil {
		t.Fatal(err)
	}
	laterConfirmedFact, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: confirmedInput.Predicate, ValueType: "date", Value: json.RawMessage(`"2011-06-12"`),
		Recurrence: FactRecurrenceAnnual, SourceKind: FactSourceAgent,
		IdempotencyKey: "archive-wedding-date-update-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE fact_candidates SET decision_assertion_id=$1 WHERE id=$2`,
		laterConfirmedFact.ResolvedAssertionID, confirmed.ID); err != nil {
		t.Fatal(err)
	}
	if err := validateImportedFactDecisionAssertions(ctx, st.pool, p.AccountID); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("repointed decision assertion error = %v, want ErrArchiveContent", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE fact_candidates SET decision_assertion_id=$1 WHERE id=$2`,
		confirmedFact.ResolvedAssertionID, confirmed.ID); err != nil {
		t.Fatal(err)
	}
	if err := validateImportedFactDecisionAssertions(ctx, st.pool, p.AccountID); err != nil {
		t.Fatalf("valid decision assertion: %v", err)
	}
	rejectedInput := ProposeFactInput{
		SetFactInput: SetFactInput{Predicate: "preferences/theme", Value: json.RawMessage(`"dark"`)},
		Reason:       "weak inference",
	}
	rejectedInput.IdempotencyKey = "archive-rejected-proposal-1"
	rejected, err := st.ProposeFact(ctx, p, rejectedInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RejectFactCandidateIdempotent(ctx, p, rejected.ID, "archive-rejected-decision-1"); err != nil {
		t.Fatal(err)
	}
	anniversaryInput := SetFactInput{
		Predicate: "identity/another-anniversary", ValueType: "date",
		Value: json.RawMessage(`"2015-09-20"`), Recurrence: FactRecurrenceAnnual,
		SourceKind: FactSourceAgent, IdempotencyKey: "archive-anniversary-set-1",
	}
	anniversaryFact, err := st.SetFact(ctx, p, anniversaryInput)
	if err != nil {
		t.Fatal(err)
	}
	chainFirst, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "preferences/shell", Value: json.RawMessage(`"bash"`), SourceKind: FactSourceAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	chainSecond, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "preferences/shell", Value: json.RawMessage(`"zsh"`), SourceKind: FactSourceAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Invert wall-clock order to model a historical/concurrent archive whose
	// child assertion timestamp sorts before its parent. Export must still
	// stream the supersedes chain parent-first for the streaming importer.
	if _, err := st.pool.Exec(ctx, `UPDATE fact_assertions
		SET created_at = CASE id WHEN $1 THEN '2026-07-12T20:00:00Z'::timestamptz
		                         WHEN $2 THEN '2026-07-12T19:00:00Z'::timestamptz END
		WHERE id IN ($1, $2)`, chainFirst.ResolvedAssertionID, chainSecond.ResolvedAssertionID); err != nil {
		t.Fatal(err)
	}

	statuses := []string{"pending", "conflict", "confirmed", "rejected"}
	before := make(map[string][]FactCandidate, len(statuses))
	for _, status := range statuses {
		before[status], err = st.ListFactCandidates(ctx, p, status)
		if err != nil {
			t.Fatal(err)
		}
		if len(before[status]) != 1 {
			t.Fatalf("%s candidates before export = %#v", status, before[status])
		}
		if before[status][0].DecidedAt != nil && before[status][0].DecidedAt.Before(before[status][0].ProposedAt) {
			t.Fatalf("%s candidate decision precedes proposal: %#v", status, before[status][0])
		}
	}

	if err := st.SuspendAccountSystem(ctx, p.AccountID, "evacuation", "candidate archive round trip"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RejectFactCandidate(ctx, p, before["pending"][0].ID); !errors.Is(err, ErrAccountNotActive) {
		t.Fatalf("reject while account suspended error = %v", err)
	}
	var usageBefore, usageAfter int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE subject_type='fact' AND subject_id=$1`, chainSecond.ID).Scan(&usageBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetFact(ctx, p, "self", "preferences/shell"); err != nil {
		t.Fatalf("read while suspended: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE subject_type='fact' AND subject_id=$1`, chainSecond.ID).Scan(&usageAfter); err != nil {
		t.Fatal(err)
	}
	if usageAfter != usageBefore {
		t.Fatalf("suspended read wrote usage events: before=%d after=%d", usageBefore, usageAfter)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, p.AccountID, "test-source", "test", &archive); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := st.ImportAccount(ctx, p.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}

	for _, status := range statuses {
		after, err := st.ListFactCandidates(ctx, p, status)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before[status], after) {
			t.Fatalf("%s candidates changed across archive\nbefore: %#v\nafter:  %#v", status, before[status], after)
		}
	}
	restored, err := st.GetFact(ctx, p, "self", "identity/another-anniversary")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Recurrence != FactRecurrenceAnnual {
		t.Fatalf("restored recurrence = %q", restored.Recurrence)
	}
	history, err := st.FactHistory(ctx, p, restored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Recurrence != FactRecurrenceAnnual {
		t.Fatalf("restored assertion history = %#v", history)
	}
	restoredChain, err := st.GetFact(ctx, p, "self", "preferences/shell")
	if err != nil {
		t.Fatal(err)
	}
	chainHistory, err := st.FactHistory(ctx, p, restoredChain.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chainHistory) != 2 || string(chainHistory[0].Value) != `"zsh"` ||
		string(chainHistory[1].Value) != `"bash"` || chainHistory[0].SupersedesID != chainHistory[1].ID {
		t.Fatalf("restored topological assertion history = %#v", chainHistory)
	}

	if err := st.ResumeAccountSystem(ctx, p.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}
	replayedAnniversary, err := st.SetFact(ctx, p, anniversaryInput)
	if err != nil {
		t.Fatal(err)
	}
	if replayedAnniversary.ID != anniversaryFact.ID || replayedAnniversary.ResolvedAssertionID != anniversaryFact.ResolvedAssertionID {
		t.Fatalf("restored set idempotency changed result: %#v -> %#v", anniversaryFact, replayedAnniversary)
	}
	replayedConfirmedCandidate, err := st.ProposeFact(ctx, p, confirmedInput)
	if err != nil {
		t.Fatal(err)
	}
	if replayedConfirmedCandidate.ID != confirmed.ID || replayedConfirmedCandidate.Status != "confirmed" {
		t.Fatalf("restored confirmed proposal replay = %#v", replayedConfirmedCandidate)
	}
	replayedConfirmedFact, err := st.ConfirmFactCandidateIdempotent(ctx, p, confirmed.ID, "archive-confirmed-decision-1")
	if err != nil {
		t.Fatal(err)
	}
	if replayedConfirmedFact.ID != confirmedFact.ID || replayedConfirmedFact.ResolvedAssertionID != confirmedFact.ResolvedAssertionID {
		t.Fatalf("restored confirm replay changed result: %#v -> %#v", confirmedFact, replayedConfirmedFact)
	}
	if string(replayedConfirmedFact.Value) != `"2010-06-12"` {
		t.Fatalf("restored confirm replay returned later value: %#v", replayedConfirmedFact)
	}
	replayedRejectedCandidate, err := st.ProposeFact(ctx, p, rejectedInput)
	if err != nil {
		t.Fatal(err)
	}
	if replayedRejectedCandidate.ID != rejected.ID || replayedRejectedCandidate.Status != "rejected" {
		t.Fatalf("restored rejected proposal replay = %#v", replayedRejectedCandidate)
	}
	if _, err := st.RejectFactCandidateIdempotent(ctx, p, rejected.ID, "archive-rejected-decision-1"); err != nil {
		t.Fatalf("restored reject replay: %v", err)
	}
}
