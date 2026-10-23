package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestTranscriptRedactedEntryAppendReplayPostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := newTranscriptStorageTestPrincipal(t, st, "redaction-replay")
	tr := createTranscriptStorageTestConversation(t, st, p, "redaction-replay")
	const canary = "WITSELF-CANARY-160A"
	batch := []AppendTranscriptEntryInput{
		{
			ExternalID: "e1", Role: TranscriptRoleTool, Body: canary,
			Payload: json.RawMessage(`{"kind":"tool.result","data":{"env":"WITSELF-CANARY-160A"},"cwd":"/x"}`),
			Model:   "fixture-tool-model",
		},
		{ExternalID: "e2", Role: TranscriptRoleAssistant, Body: "assistant response"},
	}
	original, err := st.AppendTranscriptEntries(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, batch)
	if err != nil {
		t.Fatal(err)
	}
	const receiptID = "trd_0123456789abcdef0123456789abcdef"
	redactedAt := time.Date(2026, time.October, 22, 18, 4, 5, 123456000, time.UTC)
	payload, err := transcriptRedactionPayload("tool.result", receiptID, redactedAt)
	if err != nil {
		t.Fatal(err)
	}
	result, err := st.pool.Exec(ctx, `UPDATE transcript_entries
		SET body=$1, payload=$2::jsonb, artifacts='[]'::jsonb
		WHERE account_id=$3 AND transcript_id=$4 AND id=$5`,
		TranscriptRedactedBody, string(payload), p.AccountID, tr.ID, original[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.RowsAffected() != 1 {
		t.Fatalf("test redaction updated %d rows, want 1", result.RowsAffected())
	}

	type replaySnapshot struct {
		NextSequence int64
		UpdatedAt    time.Time
		UsageEvents  int64
		EntryCount   int64
		StorageRows  string
		E2Body       string
		E2Payload    string
	}
	snapshot := func(t *testing.T) replaySnapshot {
		t.Helper()
		var out replaySnapshot
		if err := st.pool.QueryRow(ctx, `SELECT next_sequence, updated_at
			FROM transcript_conversations WHERE account_id=$1 AND id=$2`,
			p.AccountID, tr.ID).Scan(&out.NextSequence, &out.UpdatedAt); err != nil {
			t.Fatal(err)
		}
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE account_id=$1`,
			p.AccountID).Scan(&out.UsageEvents); err != nil {
			t.Fatal(err)
		}
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM transcript_entries WHERE account_id=$1`,
			p.AccountID).Scan(&out.EntryCount); err != nil {
			t.Fatal(err)
		}
		// Include every counter column, including exactness, reconcile epoch,
		// and verification time; unchanged retained bytes alone are insufficient.
		if err := st.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(u) ORDER BY u.shard), '[]'::jsonb)::text
			FROM transcript_storage_usage u WHERE account_id=$1`,
			p.AccountID).Scan(&out.StorageRows); err != nil {
			t.Fatal(err)
		}
		if err := st.pool.QueryRow(ctx, `SELECT body, COALESCE(payload::text, '')
			FROM transcript_entries WHERE account_id=$1 AND transcript_id=$2 AND external_id='e2'`,
			p.AccountID, tr.ID).Scan(&out.E2Body, &out.E2Payload); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot(t)
	if before.NextSequence != 3 || before.EntryCount != 2 {
		t.Fatal("fixture did not create exactly two sequential entries")
	}
	replayed, err := st.AppendTranscriptEntries(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 2 {
		t.Fatalf("replay returned %d entries, want 2", len(replayed))
	}
	redaction, ok := TranscriptEntryRedactionOf(replayed[0])
	if !ok || redaction.ReceiptID != receiptID || !redaction.RedactedAt.Equal(redactedAt) ||
		redaction.ReasonCode != TranscriptRedactionReasonOwnerRequest {
		t.Fatal("replay did not return the genuine redacted entry")
	}
	if !rawJSONEqual(replayed[0].Payload, payload) {
		t.Fatal("replay changed the redaction marker payload")
	}
	expectedRedacted := original[0]
	expectedRedacted.Body = replayed[0].Body
	expectedRedacted.Payload = replayed[0].Payload
	expectedRedacted.Artifacts = replayed[0].Artifacts
	if !reflect.DeepEqual(replayed[0], expectedRedacted) {
		t.Fatal("redacted replay changed an entry identity or continuity field")
	}
	if !reflect.DeepEqual(replayed[1], original[1]) {
		t.Fatal("ordinary replay did not return the unchanged original entry")
	}
	if !reflect.DeepEqual(before, snapshot(t)) {
		t.Fatal("batch replay changed conversation, usage, entry count, or storage counter rows")
	}
	assertTranscriptStorageUsageMatchesRecount(t, st, p.AccountID)

	for _, tc := range []struct {
		name   string
		change func(*AppendTranscriptEntryInput)
	}{
		{"body", func(in *AppendTranscriptEntryInput) { in.Body = "different ordinary body" }},
		{"payload", func(in *AppendTranscriptEntryInput) {
			in.Payload = json.RawMessage(`{"kind":"message.assistant","data":"different ordinary payload"}`)
		}},
	} {
		t.Run("ordinary-"+tc.name, func(t *testing.T) {
			in := batch[1]
			tc.change(&in)
			if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, in); !errors.Is(err, ErrTranscriptExists) {
				t.Fatalf("changed ordinary %s replay error = %v, want ErrTranscriptExists", tc.name, err)
			}
			if !reflect.DeepEqual(before, snapshot(t)) {
				t.Fatal("ordinary content conflict changed conversation, usage, storage, or stored e2 content")
			}
		})
	}

	for _, tc := range []struct {
		name   string
		change func(*AppendTranscriptEntryInput)
	}{
		{"role", func(in *AppendTranscriptEntryInput) { in.Role = TranscriptRoleAssistant }},
		{"model", func(in *AppendTranscriptEntryInput) { in.Model = "different-model" }},
		{"reply", func(in *AppendTranscriptEntryInput) { in.ReplyToExternalID = "e2" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := batch[0]
			tc.change(&in)
			if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, in); !errors.Is(err, ErrTranscriptExists) {
				t.Fatalf("changed %s replay error = %v, want ErrTranscriptExists", tc.name, err)
			}
		})
	}
	for _, externalID := range []string{"forged-new", "e1"} {
		t.Run("reserved-marker-"+externalID, func(t *testing.T) {
			in := AppendTranscriptEntryInput{
				ExternalID: externalID, Role: TranscriptRoleTool, Body: TranscriptRedactedBody,
				Payload: payload, Model: batch[0].Model, Artifacts: json.RawMessage(`[]`),
			}
			if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, in); !errors.Is(err, ErrTranscriptInputInvalid) {
				t.Fatalf("reserved marker append error = %v, want ErrTranscriptInputInvalid", err)
			}
		})
	}
	if !reflect.DeepEqual(before, snapshot(t)) {
		t.Fatal("refused retries or forged appends changed persisted state")
	}
	assertTranscriptStorageUsageMatchesRecount(t, st, p.AccountID)
	var canaryRows int64
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM transcript_entries
		WHERE account_id=$1 AND (strpos(body,$2)>0 OR strpos(COALESCE(payload::text,''),$2)>0)`,
		p.AccountID, canary).Scan(&canaryRows); err != nil {
		t.Fatal(err)
	}
	if canaryRows != 0 {
		t.Fatalf("%d entry rows retained or restored the test canary", canaryRows)
	}

	for _, tc := range []struct {
		name   string
		change func(*TranscriptEntry, map[string]json.RawMessage, map[string]string)
	}{
		{"actor-agent", func(_ *TranscriptEntry, _ map[string]json.RawMessage, marker map[string]string) {
			marker["actor_kind"] = "agent"
		}},
		{"extra-cwd", func(_ *TranscriptEntry, markerPayload map[string]json.RawMessage, _ map[string]string) {
			markerPayload["cwd"] = json.RawMessage(`"/x"`)
		}},
		{"body-byte", func(entry *TranscriptEntry, _ map[string]json.RawMessage, _ map[string]string) {
			entry.Body += "!"
		}},
		{"artifacts", func(entry *TranscriptEntry, _ map[string]json.RawMessage, _ map[string]string) {
			entry.Artifacts = json.RawMessage(`[{}]`)
		}},
		{"utc-offset", func(_ *TranscriptEntry, _ map[string]json.RawMessage, marker map[string]string) {
			marker["redacted_at"] = "2026-10-22T18:04:05.123456+00:00"
		}},
	} {
		t.Run("almost-marker-"+tc.name, func(t *testing.T) {
			in := AppendTranscriptEntryInput{
				ExternalID: "almost-marker-" + tc.name, Role: TranscriptRoleTool,
				Body: "different retry content", Model: batch[0].Model,
				Payload: json.RawMessage(`{"kind":"tool.result","data":"different retry payload"}`),
			}
			entry := appendTranscriptStorageTestEntry(t, st, p, tr, in)
			entry.Body = TranscriptRedactedBody
			entry.Artifacts = json.RawMessage(`[]`)
			var markerPayload map[string]json.RawMessage
			if err := json.Unmarshal(payload, &markerPayload); err != nil {
				t.Fatal(err)
			}
			var marker map[string]string
			if err := json.Unmarshal(markerPayload[transcriptRedactionPayloadKey], &marker); err != nil {
				t.Fatal(err)
			}
			tc.change(&entry, markerPayload, marker)
			markerPayload[transcriptRedactionPayloadKey], err = json.Marshal(marker)
			if err != nil {
				t.Fatal(err)
			}
			entry.Payload, err = json.Marshal(markerPayload)
			if err != nil {
				t.Fatal(err)
			}
			result, err := st.pool.Exec(ctx, `UPDATE transcript_entries
				SET body=$1, payload=$2::jsonb, artifacts=$3::jsonb
				WHERE account_id=$4 AND transcript_id=$5 AND id=$6`,
				entry.Body, string(entry.Payload), string(entry.Artifacts), p.AccountID, tr.ID, entry.ID)
			if err != nil {
				t.Fatal(err)
			}
			if result.RowsAffected() != 1 {
				t.Fatalf("almost-marker fixture updated %d rows, want 1", result.RowsAffected())
			}
			before := snapshot(t)
			if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, in); !errors.Is(err, ErrTranscriptExists) {
				t.Fatalf("almost-marker content replay error = %v, want ErrTranscriptExists", err)
			}
			if !reflect.DeepEqual(before, snapshot(t)) {
				t.Fatal("almost-marker conflict changed persisted state")
			}
			assertTranscriptStorageUsageMatchesRecount(t, st, p.AccountID)
		})
	}
}
