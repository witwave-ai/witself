package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestTranscriptRedactionFactEvidencePostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := newTranscriptStorageTestPrincipal(t, st, "redaction-facts")
	tr := createTranscriptStorageTestConversation(t, st, p, "redaction-facts")
	entry := appendTranscriptStorageTestEntry(t, st, p, tr, AppendTranscriptEntryInput{
		ExternalID: "source", Role: TranscriptRoleUser, Body: "evidence before redaction",
		Payload: json.RawMessage(`{"kind":"message.user"}`),
	})
	ordinary := appendTranscriptStorageTestEntry(t, st, p, tr, AppendTranscriptEntryInput{
		ExternalID: "ordinary", Role: TranscriptRoleUser, Body: "ordinary evidence",
	})
	ref := fmt.Sprintf("witself://transcript/%s/entry/%s", tr.ID, entry.ID)
	setInput := SetFactInput{Predicate: "testing/redacted-set", Value: json.RawMessage(`"fixture"`),
		SourceRef: ref, IdempotencyKey: "redaction-fact-set-replay"}
	set, err := st.SetFact(ctx, p, setInput)
	if err != nil {
		t.Fatal(err)
	}
	proposal := ProposeFactInput{SetFactInput: SetFactInput{Predicate: "testing/redacted-proposal",
		Value: json.RawMessage(`"fixture"`), SourceRef: ref, IdempotencyKey: "redaction-fact-propose-replay"}}
	candidate, err := st.ProposeFact(ctx, p, proposal)
	if err != nil {
		t.Fatal(err)
	}
	confirmedCandidate, err := st.ProposeFact(ctx, p, ProposeFactInput{SetFactInput: SetFactInput{
		Predicate: "testing/redacted-confirmed", Value: json.RawMessage(`"fixture"`), SourceRef: ref,
	}})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := st.ConfirmFactCandidateIdempotent(ctx, p, confirmedCandidate.ID, "redaction-fact-confirm-replay")
	if err != nil {
		t.Fatal(err)
	}
	owner := Principal{Kind: PrincipalOperator, AccountID: p.AccountID, AccountStatus: "active"}
	if err := st.pool.QueryRow(ctx, `SELECT id FROM operators
		WHERE account_id=$1 AND (is_root OR role='account_owner') AND deleted_at IS NULL LIMIT 1`,
		p.AccountID).Scan(&owner.ID); err != nil {
		t.Fatal(err)
	}
	preview, err := st.RedactTranscriptEntries(ctx, owner, RedactTranscriptEntriesInput{
		TranscriptID: tr.ID, FromSequence: 1, UntilSequence: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RedactTranscriptEntries(ctx, owner, RedactTranscriptEntriesInput{
		TranscriptID: tr.ID, FromSequence: 1, UntilSequence: 1, Apply: true,
		ExpectedRedactionSetRevision: preview.RedactionSetRevision, IdempotencyKey: "redaction-fact-evidence",
	}); err != nil {
		t.Fatal(err)
	}

	counts := func() [5]int64 {
		t.Helper()
		var out [5]int64
		for i, table := range []string{"facts", "fact_assertions", "fact_candidates", "usage_events", "usage_rollups"} {
			if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE account_id=$1`, p.AccountID).Scan(&out[i]); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	before := counts()
	assertRefused := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, ErrFactInputInvalid) || !strings.Contains(err.Error(), "transcript entry is redacted and cannot be fact evidence") {
			t.Fatalf("redacted evidence error = %v, want specific ErrFactInputInvalid", err)
		}
	}
	t.Run("propose", func(t *testing.T) {
		in := proposal
		in.IdempotencyKey = "new-proposal-after-redaction"
		_, err := st.ProposeFact(ctx, p, in)
		assertRefused(t, err)
	})
	t.Run("set", func(t *testing.T) {
		in := setInput
		in.IdempotencyKey = "new-set-after-redaction"
		_, err := st.SetFact(ctx, p, in)
		assertRefused(t, err)
	})
	t.Run("confirm", func(t *testing.T) {
		_, err := st.ConfirmFactCandidate(ctx, p, candidate.ID)
		assertRefused(t, err)
		var status string
		if err := st.pool.QueryRow(ctx, `SELECT status FROM fact_candidates WHERE id=$1`, candidate.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "pending" {
			t.Fatal("refused candidate confirmation changed its status")
		}
	})
	if counts() != before {
		t.Fatal("refused fact evidence wrote fact, candidate, assertion, or usage rows")
	}
	t.Run("existing-replays", func(t *testing.T) {
		gotSet, err := st.SetFact(ctx, p, setInput)
		if err != nil || gotSet.ResolvedAssertionID != set.ResolvedAssertionID {
			t.Fatalf("existing set replay failed: %v", err)
		}
		gotProposal, err := st.ProposeFact(ctx, p, proposal)
		if err != nil || gotProposal.ID != candidate.ID {
			t.Fatalf("existing proposal replay failed: %v", err)
		}
		gotConfirm, err := st.ConfirmFactCandidateIdempotent(ctx, p, confirmedCandidate.ID, "redaction-fact-confirm-replay")
		if err != nil || gotConfirm.ResolvedAssertionID != confirmed.ResolvedAssertionID {
			t.Fatalf("existing confirmation replay failed: %v", err)
		}
		if counts() != before {
			t.Fatal("existing fact replay wrote new rows")
		}
	})

	otherAgent, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, "other-recorder")
	if err != nil {
		t.Fatal(err)
	}
	otherOwner := p
	otherOwner.ID = otherAgent.ID
	otherRealm, err := st.CreateRealm(ctx, p.AccountID, "other")
	if err != nil {
		t.Fatal(err)
	}
	realmAgent, err := st.CreateAgent(ctx, p.AccountID, otherRealm.ID, "other-realm-recorder")
	if err != nil {
		t.Fatal(err)
	}
	otherRealmOwner := p
	otherRealmOwner.ID, otherRealmOwner.RealmID = realmAgent.ID, otherRealm.ID
	foreign := newTranscriptStorageTestPrincipal(t, st, "redaction-facts-foreign")
	// Exercise each SQL scope field independently; real agent ids also imply
	// an account and realm, which could otherwise mask a missing filter.
	for _, scope := range []string{"account", "realm"} {
		t.Run("helper-scope-"+scope, func(t *testing.T) {
			mismatched := p
			if scope == "account" {
				mismatched.AccountID = foreign.AccountID
			} else {
				mismatched.RealmID = otherRealm.ID
			}
			tx, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := refuseRedactedTranscriptSourceRefTx(ctx, tx, mismatched, ref); err != nil {
				t.Fatalf("out-of-scope helper reference was resolved: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		p    Principal
		ref  string
	}{
		{"other-agent", otherOwner, ref},
		{"other-realm", otherRealmOwner, ref},
		{"other-account", foreign, ref},
		{"missing-entry", p, fmt.Sprintf("witself://transcript/%s/entry/missing", tr.ID)},
		{"missing-transcript", p, fmt.Sprintf("witself://transcript/missing/entry/%s", entry.ID)},
		{"malformed", p, ref + "/extra"},
		{"opaque", p, "conversation:fixture"},
		{"ordinary", p, fmt.Sprintf("witself://transcript/%s/entry/%s", tr.ID, ordinary.ID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := SetFactInput{Predicate: "testing/allowed-set-" + tc.name, Value: json.RawMessage(`"fixture"`), SourceRef: tc.ref}
			if _, err := st.SetFact(ctx, tc.p, in); err != nil {
				t.Fatalf("opaque or ordinary set reference refused: %v", err)
			}
			in.Predicate = "testing/allowed-proposal-" + tc.name
			proposed, err := st.ProposeFact(ctx, tc.p, ProposeFactInput{SetFactInput: in})
			if err != nil {
				t.Fatalf("opaque or ordinary proposal reference refused: %v", err)
			}
			if _, err := st.ConfirmFactCandidate(ctx, tc.p, proposed.ID); err != nil {
				t.Fatalf("opaque or ordinary confirmation reference refused: %v", err)
			}
		})
	}

	// Project an ordinary row containing attacker-controlled keys and kind text.
	// They must be replaced with fixed sentinels, never copied into the probe.
	if _, err := st.pool.Exec(ctx, `UPDATE transcript_entries SET body='ordinary body',
		payload='{"WITSELF-CANARY-160B-KEY":true,"kind":"WITSELF-CANARY-160B-KIND","witself_redaction":{"data":"WITSELF-CANARY-160B-MARKER"}}'::jsonb
		WHERE id=$1`, ordinary.ID); err != nil {
		t.Fatal(err)
	}
	var probe transcriptEntryRedactionProbe
	if err := st.pool.QueryRow(ctx, `SELECT `+transcriptRedactionProbeSQL("$2", "$3")+`
		FROM transcript_entries e WHERE e.id=$1`, ordinary.ID, TranscriptRedactedBody,
		transcriptRedactionKindVocabulary()).Scan(&probe.BodyMatches, &probe.ArtifactsEmpty,
		&probe.PayloadKeys, &probe.KindType, &probe.KindText, &probe.Marker); err != nil {
		t.Fatal(err)
	}
	if probe.BodyMatches || !reflect.DeepEqual(probe.PayloadKeys, []string{"__non_marker__"}) || probe.KindText != "" || len(probe.Marker) != 0 {
		t.Fatal("ordinary content escaped the value-free SQL probe projection")
	}
	if _, redacted := transcriptEntryRedactionFromProbe(probe); redacted {
		t.Fatal("sentinel SQL probe passed the marker predicate")
	}
	if _, err := st.pool.Exec(ctx, `UPDATE transcript_entries SET body=$2,
		artifacts='[{"private":"WITSELF-CANARY-160B-ARTIFACT"}]'::jsonb WHERE id=$1`,
		ordinary.ID, TranscriptRedactedBody); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT `+transcriptRedactionProbeSQL("$2", "$3")+`
		FROM transcript_entries e WHERE e.id=$1`, ordinary.ID, TranscriptRedactedBody,
		transcriptRedactionKindVocabulary()).Scan(&probe.BodyMatches, &probe.ArtifactsEmpty,
		&probe.PayloadKeys, &probe.KindType, &probe.KindText, &probe.Marker); err != nil {
		t.Fatal(err)
	}
	if !probe.BodyMatches || probe.ArtifactsEmpty || len(probe.Marker) != 0 {
		t.Fatal("nonempty artifacts allowed the raw marker to escape the SQL projection")
	}
}
