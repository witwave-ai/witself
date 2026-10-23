package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/store"
)

func TestToServerTranscriptEntryRedactionView(t *testing.T) {
	redactedAt := time.Date(2026, 10, 22, 18, 4, 5, 123456000, time.UTC)
	entry := store.TranscriptEntry{
		ID: "ent_redacted", TranscriptID: "trn_redacted", Sequence: 1, Role: "tool",
		Body:      store.TranscriptRedactedBody,
		Payload:   json.RawMessage(`{"kind":"tool.result","witself_redaction":{"schema":"witself.transcript-redaction.v1","receipt_id":"trd_0123456789abcdef0123456789abcdef","redacted_at":"2026-10-22T18:04:05.123456Z","reason_code":"owner_request","actor_kind":"operator"}}`),
		Artifacts: json.RawMessage(`[]`),
	}
	got := toServerTranscriptEntry(entry)
	if got.Redaction == nil {
		t.Fatal("genuine marker omitted the redaction view")
	}
	if got.Redaction.ReceiptID != "trd_0123456789abcdef0123456789abcdef" ||
		!got.Redaction.RedactedAt.Equal(redactedAt) || got.Redaction.ReasonCode != "owner_request" {
		t.Fatalf("redaction view = %#v", got.Redaction)
	}
	if got.ID != entry.ID || got.Sequence != entry.Sequence || got.Body != entry.Body ||
		string(got.Payload) != string(entry.Payload) || string(got.Artifacts) != string(entry.Artifacts) {
		t.Fatal("redaction view changed the stored entry fields")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	var redaction map[string]string
	if err := json.Unmarshal(fields["redaction"], &redaction); err != nil {
		t.Fatalf("decode redaction wire object: %v", err)
	}
	wantRedaction := map[string]string{
		"receipt_id":  "trd_0123456789abcdef0123456789abcdef",
		"redacted_at": "2026-10-22T18:04:05.123456Z",
		"reason_code": "owner_request",
	}
	if !reflect.DeepEqual(redaction, wantRedaction) {
		t.Fatalf("redaction wire object = %#v, want %#v", redaction, wantRedaction)
	}
	for _, tc := range []struct {
		name    string
		payload json.RawMessage
	}{
		{name: "ordinary", payload: json.RawMessage(`{"kind":"tool.result"}`)},
		{name: "marker with ordinary body", payload: entry.Payload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ordinary := entry
			ordinary.Body = "ordinary tool result"
			ordinary.Payload = tc.payload
			mapped := toServerTranscriptEntry(ordinary)
			if mapped.Redaction != nil {
				t.Fatal("ordinary entry acquired a redaction view")
			}
			raw, err := json.Marshal(mapped)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if _, ok := fields["redaction"]; ok {
				t.Fatal("ordinary entry JSON contains the redaction key")
			}
		})
	}
}
