package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func transcriptRedactionTestOwner(t *testing.T, st *Store, p Principal) Principal {
	t.Helper()
	owner := Principal{Kind: PrincipalOperator, AccountID: p.AccountID, AccountStatus: "active"}
	if err := st.pool.QueryRow(context.Background(), `SELECT id FROM operators WHERE account_id=$1 AND is_root AND deleted_at IS NULL`, p.AccountID).Scan(&owner.ID); err != nil {
		t.Fatal(err)
	}
	return owner
}

func TestTranscriptRedactionPreviewApplyPostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := newTranscriptStorageTestPrincipal(t, st, "redact-a")
	b := newTranscriptStorageTestPrincipal(t, st, "redact-b")
	a2agent, err := st.CreateAgent(ctx, a.AccountID, a.RealmID, "recorder-two")
	if err != nil {
		t.Fatal(err)
	}
	a2 := a
	a2.ID = a2agent.ID
	owner := transcriptRedactionTestOwner(t, st, a)
	foreign := transcriptRedactionTestOwner(t, st, b)
	otherOp, _, _, err := st.CreateOperator(ctx, a.AccountID, owner.ID, "ordinary operator", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	nonOwner := owner
	nonOwner.ID = otherOp.ID
	tr := createTranscriptStorageTestConversation(t, st, a, "redaction")
	const canary = "WITSELF-CANARY-160B"
	const key = "redact-fixture-private-retry-key"
	batch := []AppendTranscriptEntryInput{
		{ExternalID: "e1", Role: TranscriptRoleUser, Body: "question"},
		{ExternalID: "e2", Role: TranscriptRoleTool, Body: "call", Payload: json.RawMessage(`{"kind":"tool.call"}`)},
		{ExternalID: "e3", Role: TranscriptRoleTool, Body: canary, Model: "tool-fixture", Payload: json.RawMessage(`{"kind":"tool.result","data":"WITSELF-CANARY-160B","data_sha256":"fixture-digest","cwd":"/fixture","source_transcript_path":"/fixture/session","chunk_index":0,"chunk_count":2}`)},
		{ExternalID: "e4", Role: TranscriptRoleTool, Body: canary, ReplyToExternalID: "e3", Payload: json.RawMessage(`{"kind":"tool.result","chunk_index":1,"chunk_count":2}`)},
		{ExternalID: "e5", Role: TranscriptRoleAssistant, Body: "answer"},
	}
	original, err := st.AppendTranscriptEntries(ctx, a.AccountID, a.RealmID, a.ID, tr.ID, batch)
	if err != nil {
		t.Fatal(err)
	}
	type snapshot struct {
		Next                   int64
		Updated                time.Time
		Usage, Rollups, Events int64
		Lanes, Requests        string
	}
	snap := func() snapshot {
		t.Helper()
		var out snapshot
		if err := st.pool.QueryRow(ctx, `SELECT next_sequence,updated_at FROM transcript_conversations WHERE id=$1`, tr.ID).Scan(&out.Next, &out.Updated); err != nil {
			t.Fatal(err)
		}
		if err := st.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_events WHERE account_id=$1),(SELECT count(*) FROM usage_rollups WHERE account_id=$1),(SELECT count(*) FROM account_events WHERE account_id=$1),
		 COALESCE((SELECT jsonb_agg(to_jsonb(l) ORDER BY owner_id)::text FROM memory_curation_lanes l WHERE account_id=$1),'[]'),
		 COALESCE((SELECT jsonb_agg(to_jsonb(r) ORDER BY id)::text FROM memory_curation_requests r WHERE account_id=$1),'[]')`, a.AccountID).Scan(&out.Usage, &out.Rollups, &out.Events, &out.Lanes, &out.Requests); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snap()
	in := RedactTranscriptEntriesInput{TranscriptID: tr.ID, FromSequence: 3, UntilSequence: 3}
	// A plain snapshot preview must succeed even while another transaction owns
	// exclusive row locks on every authority and content row it inspects.
	locked, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`SELECT id FROM accounts WHERE id=$1 FOR UPDATE`,
		`SELECT id FROM operators WHERE account_id=$1 FOR UPDATE`,
		`SELECT id FROM transcript_conversations WHERE account_id=$1 FOR UPDATE`,
		`SELECT id FROM transcript_entries WHERE account_id=$1 FOR UPDATE`,
	} {
		if _, err := locked.Exec(ctx, query, a.AccountID); err != nil {
			_ = locked.Rollback(ctx)
			t.Fatal(err)
		}
	}
	previewCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, previewErr := st.RedactTranscriptEntries(previewCtx, owner, in)
	cancel()
	_ = locked.Rollback(ctx)
	if previewErr != nil {
		t.Fatalf("preview took row locks: %v", previewErr)
	}
	partial, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil {
		t.Fatal(err)
	}
	if !partial.PartialCaptureEvent || partial.RedactableCount != 1 || partial.Entries[0].Kind != "tool.result" || partial.CancelledCurationRunCount != 0 {
		t.Fatal("single-chunk preview is incorrect")
	}
	startPartial, err := st.RedactTranscriptEntries(ctx, owner, RedactTranscriptEntriesInput{
		TranscriptID: tr.ID, FromSequence: 4, UntilSequence: 4,
	})
	if err != nil || !startPartial.PartialCaptureEvent || startPartial.RedactableCount != 1 {
		t.Fatal("preview starting within a capture event omitted the first-entry warning")
	}
	in.UntilSequence = 4
	preview, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil {
		t.Fatal(err)
	}
	if preview.RedactionSetRevision != second.RedactionSetRevision || preview.PartialCaptureEvent {
		t.Fatal("full chunk preview is unstable or partial")
	}
	if !reflect.DeepEqual(before, snap()) {
		t.Fatal("preview wrote conversation, usage, audit or curation state")
	}
	unchanged, err := st.AppendTranscriptEntries(ctx, a.AccountID, a.RealmID, a.ID, tr.ID, batch)
	if err != nil || !reflect.DeepEqual(original, unchanged) {
		t.Fatal("preview changed entry bytes")
	}
	for _, p := range []Principal{a, a2, b, nonOwner} {
		if _, err := st.RedactTranscriptEntries(ctx, p, in); !errors.Is(err, ErrTranscriptForbidden) {
			t.Fatalf("non-owner error = %v", err)
		}
		missing := in
		missing.TranscriptID = "trn_missing"
		if _, err := st.RedactTranscriptEntries(ctx, p, missing); !errors.Is(err, ErrTranscriptForbidden) {
			t.Fatal("non-owner checked transcript existence")
		}
		unauthorizedApply := in
		unauthorizedApply.Apply = true
		unauthorizedApply.ExpectedRedactionSetRevision = preview.RedactionSetRevision
		unauthorizedApply.IdempotencyKey = "unauthorized"
		if _, err := st.RedactTranscriptEntries(ctx, p, unauthorizedApply); !errors.Is(err, ErrTranscriptForbidden) {
			t.Fatalf("non-owner apply error = %v", err)
		}
	}
	if _, err := st.RedactTranscriptEntries(ctx, foreign, in); !errors.Is(err, ErrTranscriptNotFound) {
		t.Fatalf("foreign owner error = %v", err)
	}
	missing := in
	missing.TranscriptID = "trn_missing"
	if _, err := st.RedactTranscriptEntries(ctx, owner, missing); !errors.Is(err, ErrTranscriptNotFound) {
		t.Fatalf("owner missing-transcript preview error = %v", err)
	}
	in.Apply = true
	in.ExpectedRedactionSetRevision = preview.RedactionSetRevision
	in.IdempotencyKey = "  " + key + "  "
	if _, err := st.RedactTranscriptEntries(ctx, foreign, in); !errors.Is(err, ErrTranscriptNotFound) {
		t.Fatalf("foreign owner apply error = %v", err)
	}
	result, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.Replayed || result.RedactedCount != 2 || !transcriptRedactionReceiptPattern.MatchString(result.ReceiptID) || result.RedactedAt == nil {
		t.Fatal("apply receipt is incorrect")
	}
	var leaked int64
	if err := st.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM transcript_entries WHERE account_id=$1 AND (body LIKE '%'||$2||'%' OR payload::text LIKE '%'||$2||'%' OR body LIKE '%'||$3||'%' OR payload::text LIKE '%'||$3||'%')) +
	 (SELECT count(*) FROM account_events WHERE account_id=$1 AND (metadata::text LIKE '%'||$2||'%' OR metadata::text LIKE '%'||$3||'%'))`, a.AccountID, canary, key).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatal("redacted content or raw retry key survived")
	}
	post, err := st.AppendTranscriptEntries(ctx, a.AccountID, a.RealmID, a.ID, tr.ID, batch)
	if err != nil {
		t.Fatal(err)
	}
	for i, entry := range post {
		if i != 2 && i != 3 {
			if !reflect.DeepEqual(entry, original[i]) {
				t.Fatal("unselected entry changed")
			}
			continue
		}
		marker, ok := TranscriptEntryRedactionOf(entry)
		if !ok || marker.ReceiptID != result.ReceiptID || !marker.RedactedAt.Equal(*result.RedactedAt) {
			t.Fatal("updated row is not exact marker")
		}
		payload, err := transcriptRedactionPayload("tool.result", result.ReceiptID, *result.RedactedAt)
		if err != nil {
			t.Fatal(err)
		}
		if !rawJSONEqual(entry.Payload, payload) {
			t.Fatal("kind or marker shape changed")
		}
		want := original[i]
		want.Body = entry.Body
		want.Payload = entry.Payload
		want.Artifacts = entry.Artifacts
		if !reflect.DeepEqual(want, entry) {
			t.Fatal("redaction changed continuity fields")
		}
	}
	after := snap()
	expected := before
	expected.Events++
	if !reflect.DeepEqual(expected, after) {
		t.Fatal("apply changed conversation, usage or curation state beyond its audit event")
	}
	assertTranscriptStorageUsageMatchesRecount(t, st, a.AccountID, b.AccountID)
	var actorKind, actorID string
	var metadata []byte
	if err := st.pool.QueryRow(ctx, `SELECT actor_kind,actor_id,metadata FROM account_events WHERE account_id=$1 AND verb=$2`, a.AccountID, VerbTranscriptEntriesRedacted).Scan(&actorKind, &actorID, &metadata); err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(metadata, &fields); err != nil {
		t.Fatal(err)
	}
	wantFields := map[string]string{"transcript_id": tr.ID, "receipt_id": result.ReceiptID, "from_sequence": "3", "until_sequence": "4", "entry_count": "2", "redacted_count": "2"}
	if actorKind != ActorOperator || actorID != owner.ID || !reflect.DeepEqual(fields, wantFields) {
		t.Fatal("audit actor or metadata contract changed")
	}
	in.IdempotencyKey = key
	replay, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil {
		t.Fatal(err)
	}
	if replay.RedactedCount != 2 || replay.RedactableCount != 2 || replay.AlreadyRedactedCount != 0 {
		t.Fatal("exact replay counts do not describe its original redaction")
	}
	if !replay.Replayed || replay.ReceiptID != result.ReceiptID || replay.RedactedAt == nil || !replay.RedactedAt.Equal(*result.RedactedAt) || replay.RedactionSetRevision != preview.RedactionSetRevision || replay.CancelledCurationRunCount != 0 || !reflect.DeepEqual(after, snap()) {
		t.Fatal("exact replay changed state or receipt")
	}
	in.IdempotencyKey = "different"
	if _, err := st.RedactTranscriptEntries(ctx, owner, in); !errors.Is(err, ErrTranscriptRedactionConflict) {
		t.Fatalf("different-key error = %v", err)
	}
	in.Apply = false
	in.ExpectedRedactionSetRevision = ""
	in.IdempotencyKey = ""
	finished, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil {
		t.Fatal(err)
	}
	if finished.RedactableCount != 0 || finished.AlreadyRedactedCount != 2 {
		t.Fatal("fresh preview did not see markers")
	}
	in.Apply = true
	in.ExpectedRedactionSetRevision = finished.RedactionSetRevision
	in.IdempotencyKey = "already"
	if _, err := st.RedactTranscriptEntries(ctx, owner, in); !errors.Is(err, ErrTranscriptEntriesAlreadyRedacted) {
		t.Fatalf("already redacted error = %v", err)
	}
	in.Apply = false
	in.FromSequence = 2
	in.IdempotencyKey = ""
	in.ExpectedRedactionSetRevision = ""
	mixed, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Apply = true
	in.IdempotencyKey = "mixed"
	in.ExpectedRedactionSetRevision = mixed.RedactionSetRevision
	mixedResult, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil || mixedResult.RedactedCount != 1 {
		t.Fatal("mixed range did not redact exactly one row")
	}
	if err := st.pool.QueryRow(ctx, `SELECT metadata FROM account_events WHERE account_id=$1 AND verb=$2 AND metadata->>'receipt_id'=$3`, a.AccountID, VerbTranscriptEntriesRedacted, mixedResult.ReceiptID).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(metadata, &fields); err != nil {
		t.Fatal(err)
	}
	wantFields = map[string]string{"transcript_id": tr.ID, "receipt_id": mixedResult.ReceiptID, "from_sequence": "2", "until_sequence": "4", "entry_count": "3", "redacted_count": "1"}
	if !reflect.DeepEqual(fields, wantFields) {
		t.Fatal("mixed-range audit does not distinguish selected and redacted counts")
	}
	mixedReplay, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil || !mixedReplay.Replayed || mixedReplay.RedactedCount != 1 || mixedReplay.RedactableCount != 1 || mixedReplay.AlreadyRedactedCount != 2 {
		t.Fatal("mixed-range replay counts do not preserve the original receipt partition")
	}
	postMixed, err := st.AppendTranscriptEntries(ctx, a.AccountID, a.RealmID, a.ID, tr.ID, batch)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(postMixed[2:4], post[2:4]) {
		t.Fatal("mixed range replaced existing markers")
	}
	for _, invalid := range []RedactTranscriptEntriesInput{
		{TranscriptID: tr.ID, FromSequence: 0, UntilSequence: 1},
		{TranscriptID: tr.ID, FromSequence: 2, UntilSequence: 1},
		{TranscriptID: tr.ID, FromSequence: 1, UntilSequence: 101},
		{TranscriptID: tr.ID, FromSequence: 1, UntilSequence: 6},
		{TranscriptID: tr.ID, FromSequence: 1, UntilSequence: 1, IdempotencyKey: "preview"},
		{TranscriptID: tr.ID, FromSequence: 1, UntilSequence: 1, Apply: true, ExpectedRedactionSetRevision: "bad", IdempotencyKey: "bad"},
	} {
		if _, err := st.RedactTranscriptEntries(ctx, owner, invalid); !errors.Is(err, ErrTranscriptInputInvalid) {
			t.Fatalf("invalid input error = %v", err)
		}
	}
	tr2 := createTranscriptStorageTestConversation(t, st, a2, "second-owner")
	appendTranscriptStorageTestEntry(t, st, a2, tr2, AppendTranscriptEntryInput{Role: TranscriptRoleUser, Body: "another agent"})
	in = RedactTranscriptEntriesInput{TranscriptID: tr2.ID, FromSequence: 1, UntilSequence: 1}
	p2, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil {
		t.Fatal("account owner could not preview another agent")
	}
	in.Apply = true
	in.ExpectedRedactionSetRevision = p2.RedactionSetRevision
	in.IdempotencyKey = "second-owner"
	if _, err := st.RedactTranscriptEntries(ctx, owner, in); err != nil {
		t.Fatal(err)
	}
	// Non-root owners are authorized by role; deleted operators never are.
	if _, err := st.pool.Exec(ctx, `UPDATE operators SET role='account_owner' WHERE id=$1`, nonOwner.ID); err != nil {
		t.Fatal(err)
	}
	in.Apply = false
	in.IdempotencyKey = ""
	in.ExpectedRedactionSetRevision = ""
	if _, err := st.RedactTranscriptEntries(ctx, nonOwner, in); err != nil {
		t.Fatal("account_owner role was refused")
	}
	if _, err := st.pool.Exec(ctx, `UPDATE operators SET deleted_at=clock_timestamp() WHERE id=$1`, nonOwner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RedactTranscriptEntries(ctx, nonOwner, in); !errors.Is(err, ErrTranscriptForbidden) {
		t.Fatal("deleted operator retained redaction authority")
	}
}

func TestTranscriptRedactionAccountStatePostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := newTranscriptStorageTestPrincipal(t, st, "redact-state")
	owner := transcriptRedactionTestOwner(t, st, p)
	tr := createTranscriptStorageTestConversation(t, st, p, "state")
	appendTranscriptStorageTestEntry(t, st, p, tr, AppendTranscriptEntryInput{Role: TranscriptRoleUser, Body: "small"})
	// A legacy/imported conversation can have no curation lane. Redaction must
	// lock only an existing lane, never create one or its source request graph.
	if _, err := st.pool.Exec(ctx, `DELETE FROM memory_curation_lanes WHERE account_id=$1 AND owner_id=$2`, p.AccountID, p.ID); err != nil {
		t.Fatal(err)
	}
	// Deleted recorder rows remain valid redaction targets; no live-agent check.
	if _, err := st.pool.Exec(ctx, `UPDATE agents SET deleted_at=clock_timestamp() WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SuspendAccountOwner(ctx, p.AccountID, owner.ID, "test freeze"); err != nil {
		t.Fatal(err)
	}
	in := RedactTranscriptEntriesInput{TranscriptID: tr.ID, FromSequence: 1, UntilSequence: 1}
	preview, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Apply = true
	in.ExpectedRedactionSetRevision = preview.RedactionSetRevision
	in.IdempotencyKey = "suspended"
	if _, err := st.RedactTranscriptEntries(ctx, owner, in); err != nil {
		t.Fatal(err)
	}
	assertTranscriptStorageUsageMatchesRecount(t, st, p.AccountID)
	var laneCount, requestCount int64
	if err := st.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM memory_curation_lanes WHERE account_id=$1),(SELECT count(*) FROM memory_curation_requests WHERE account_id=$1)`, p.AccountID).Scan(&laneCount, &requestCount); err != nil {
		t.Fatal(err)
	}
	if laneCount != 0 || requestCount != 0 {
		t.Fatal("redaction created a curation lane or request")
	}
	for _, status := range []string{"pending", "closed"} {
		if _, err := st.pool.Exec(ctx, `UPDATE accounts SET status=$2 WHERE id=$1`, p.AccountID, status); err != nil {
			t.Fatal(err)
		}
		for _, apply := range []bool{false, true} {
			in.Apply = apply
			if !apply {
				in.ExpectedRedactionSetRevision = ""
				in.IdempotencyKey = ""
			} else {
				in.ExpectedRedactionSetRevision = strings.Repeat("a", 64)
				in.IdempotencyKey = "blocked"
			}
			if _, err := st.RedactTranscriptEntries(ctx, owner, in); !errors.Is(err, ErrAccountNotActive) {
				t.Fatalf("%s error = %v", status, err)
			}
		}
	}
	if _, err := st.pool.Exec(ctx, `UPDATE accounts SET status='active' WHERE id=$1`, p.AccountID); err != nil {
		t.Fatal(err)
	}
	freshAgent, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, "evacuation-recorder")
	if err != nil {
		t.Fatal(err)
	}
	freshPrincipal := p
	freshPrincipal.ID = freshAgent.ID
	freshTranscript := createTranscriptStorageTestConversation(t, st, freshPrincipal, "evacuation-unredacted")
	appendTranscriptStorageTestEntry(t, st, freshPrincipal, freshTranscript, AppendTranscriptEntryInput{Role: TranscriptRoleUser, Body: "fresh ordinary content"})
	in = RedactTranscriptEntriesInput{TranscriptID: freshTranscript.ID, FromSequence: 1, UntilSequence: 1}
	movePreview, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil || movePreview.RedactableCount != 1 {
		t.Fatalf("pre-evacuation preview must select unredacted content: %v", err)
	}
	if _, err := st.BeginAccountEvacuation(ctx, p.AccountID, "evac_redaction_test", "test move"); err != nil {
		t.Fatal(err)
	}
	var before, after string
	query := `SELECT jsonb_build_object('entries',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM transcript_entries e WHERE account_id=$1),'events',(SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id) FROM account_events a WHERE account_id=$1))::text`
	if err := st.pool.QueryRow(ctx, query, p.AccountID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, apply := range []bool{false, true} {
		in.Apply = apply
		if !apply {
			in.ExpectedRedactionSetRevision = ""
			in.IdempotencyKey = ""
		} else {
			in.ExpectedRedactionSetRevision = movePreview.RedactionSetRevision
			in.IdempotencyKey = "move-fresh-entry"
		}
		if _, err := st.RedactTranscriptEntries(ctx, owner, in); !errors.Is(err, ErrAccountEvacuationInProgress) {
			t.Fatalf("evacuation error = %v", err)
		}
	}
	if err := st.pool.QueryRow(ctx, query, p.AccountID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("evacuation refusal changed rows")
	}
}

// A partial replay is isolated from TB1, whose later assertions require its
// previously redacted rows to retain their original markers.
func TestTranscriptRedactionPartialReplayConflictPostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := newTranscriptStorageTestPrincipal(t, st, "redaction-partial-replay")
	owner := transcriptRedactionTestOwner(t, st, p)
	tr := createTranscriptStorageTestConversation(t, st, p, "partial-replay")
	for range 3 {
		appendTranscriptStorageTestEntry(t, st, p, tr, AppendTranscriptEntryInput{
			Role: TranscriptRoleTool, Body: "ordinary tool output", Payload: json.RawMessage(`{"kind":"tool.result"}`),
		})
	}
	in := RedactTranscriptEntriesInput{TranscriptID: tr.ID, FromSequence: 2, UntilSequence: 3}
	preview, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Apply = true
	in.ExpectedRedactionSetRevision = preview.RedactionSetRevision
	in.IdempotencyKey = "partial-replay-key"
	applied, err := st.RedactTranscriptEntries(ctx, owner, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE transcript_entries SET body='restored ordinary content',payload='{"kind":"tool.result","data":"restored"}'::jsonb WHERE transcript_id=$1 AND sequence=3`, tr.ID); err != nil {
		t.Fatal(err)
	}
	_, before, err := st.GetTranscript(ctx, p, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := TranscriptEntryRedactionOf(before[1])
	if !ok || marker.ReceiptID != applied.ReceiptID || applied.RedactedAt == nil || !marker.RedactedAt.Equal(*applied.RedactedAt) {
		t.Fatal("fixture lost row 2's original receipt or redaction time")
	}
	if _, ok := TranscriptEntryRedactionOf(before[2]); ok {
		t.Fatal("restored content must fail the marker predicate")
	}
	eventCount := func() int64 {
		t.Helper()
		var count int64
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM account_events WHERE account_id=$1 AND verb=$2`, p.AccountID, VerbTranscriptEntriesRedacted).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	beforeEvents := eventCount()
	if _, err := st.RedactTranscriptEntries(ctx, owner, in); !errors.Is(err, ErrTranscriptRedactionConflict) {
		t.Fatalf("partially restored exact replay error = %v, want conflict", err)
	}
	_, after, err := st.GetTranscript(ctx, p, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || eventCount() != beforeEvents {
		t.Fatal("conflicting partial replay changed the marker, restored row or audit count")
	}
	assertTranscriptStorageUsageMatchesRecount(t, st, p.AccountID)
}
