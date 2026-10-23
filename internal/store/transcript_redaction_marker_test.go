package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTranscriptRedactionMarkerPredicate(t *testing.T) {
	const receiptID = "trd_0123456789abcdef0123456789abcdef"
	const markerJSON = `{"kind":"tool.result","witself_redaction":{"schema":"witself.transcript-redaction.v1","receipt_id":"trd_0123456789abcdef0123456789abcdef","redacted_at":"2026-10-22T18:04:05.123456Z","reason_code":"owner_request","actor_kind":"operator"}}`
	redactedAt := time.Date(2026, 10, 22, 18, 4, 5, 123456000, time.UTC)
	entry := func(payload string) TranscriptEntry {
		return TranscriptEntry{
			Body: TranscriptRedactedBody, Payload: json.RawMessage(payload), Artifacts: json.RawMessage(`[]`),
		}
	}
	assertRedacted := func(t *testing.T, candidate TranscriptEntry, wantTime time.Time) {
		t.Helper()
		got, ok := TranscriptEntryRedactionOf(candidate)
		if !ok {
			t.Fatal("genuine marker was not recognized")
		}
		if got.ReceiptID != receiptID || !got.RedactedAt.Equal(wantTime) || got.RedactedAt.Location() != time.UTC || got.ReasonCode != "owner_request" {
			t.Fatal("redaction view did not preserve the expected receipt, UTC time and owner reason")
		}
	}
	t.Run("genuine row", func(t *testing.T) {
		assertRedacted(t, entry(markerJSON), redactedAt)
	})
	t.Run("ordinary body avoids JSON parsing", func(t *testing.T) {
		candidate := entry(markerJSON)
		candidate.Body = "ordinary transcript content"
		allocations := testing.AllocsPerRun(100, func() {
			got, ok := TranscriptEntryRedactionOf(candidate)
			if ok || got != (TranscriptEntryRedaction{}) {
				t.Fatal("ordinary content returned a redaction view")
			}
		})
		if allocations != 0 {
			t.Fatalf("ordinary body rejection allocated %g times, want zero before JSON parsing", allocations)
		}
	})
	t.Run("kind absent", func(t *testing.T) {
		assertRedacted(t, entry(strings.Replace(markerJSON, `"kind":"tool.result",`, "", 1)), redactedAt)
	})
	t.Run("empty artifacts whitespace", func(t *testing.T) {
		candidate := entry(markerJSON)
		candidate.Artifacts = json.RawMessage("[ \n ]")
		assertRedacted(t, candidate, redactedAt)
	})

	invalid := []struct {
		name   string
		mutate func(*TranscriptEntry)
	}{
		{"body differs by one byte", func(e *TranscriptEntry) { e.Body += "x" }},
		{"nonempty artifacts", func(e *TranscriptEntry) { e.Artifacts = json.RawMessage(`[{}]`) }},
		{"null artifacts", func(e *TranscriptEntry) { e.Artifacts = json.RawMessage(`null`) }},
		{"missing artifacts", func(e *TranscriptEntry) { e.Artifacts = nil }},
		{"object artifacts", func(e *TranscriptEntry) { e.Artifacts = json.RawMessage(`{}`) }},
		{"extra top-level key", func(e *TranscriptEntry) {
			e.Payload = json.RawMessage(strings.Replace(string(e.Payload), `{"kind"`, `{"chunk_index":0,"kind"`, 1))
		}},
		{"extra marker key", func(e *TranscriptEntry) {
			e.Payload = json.RawMessage(strings.Replace(string(e.Payload), `"schema":`, `"extra":"x","schema":`, 1))
		}},
		{"missing marker", func(e *TranscriptEntry) { e.Payload = json.RawMessage(`{"kind":"tool.result"}`) }},
		{"payload null", func(e *TranscriptEntry) { e.Payload = json.RawMessage(`null`) }},
		{"payload array", func(e *TranscriptEntry) { e.Payload = json.RawMessage(`[]`) }},
		{"payload invalid", func(e *TranscriptEntry) { e.Payload = json.RawMessage(`{`) }},
	}
	for _, replacement := range []struct{ name, old, new string }{
		{"non-string kind", `"kind":"tool.result"`, `"kind":3`},
		{"null kind", `"kind":"tool.result"`, `"kind":null`},
		{"array kind", `"kind":"tool.result"`, `"kind":[]`},
		{"object kind", `"kind":"tool.result"`, `"kind":{}`},
		{"boolean kind", `"kind":"tool.result"`, `"kind":true`},
		{"kind outside vocabulary", `"kind":"tool.result"`, `"kind":"custom.kind"`},
		{"long kind", `"kind":"tool.result"`, `"kind":"` + strings.Repeat("x", 65) + `"`},
		{"wrong schema", "witself.transcript-redaction.v1", "witself.transcript-redaction.v2"},
		{"receipt 31 hex characters", receiptID, "trd_0123456789abcdef0123456789abcde"},
		{"receipt uppercase", receiptID, "trd_0123456789ABCDEF0123456789ABCDEF"},
		{"non-UTC time", "2026-10-22T18:04:05.123456Z", "2026-10-22T18:04:05.123456-06:00"},
		{"zero-offset time", "2026-10-22T18:04:05.123456Z", "2026-10-22T18:04:05.123456+00:00"},
		{"unparsable time", "2026-10-22T18:04:05.123456Z", "not-a-timeZ"},
		{"comma fractional time", "2026-10-22T18:04:05.123456Z", "2026-10-22T18:04:05,123456Z"},
		{"13-digit fractional time", "2026-10-22T18:04:05.123456Z", "2026-10-22T18:04:05.1234567890123Z"},
		{"untrimmed fractional time", "2026-10-22T18:04:05.123456Z", "2026-10-22T18:04:05.123450Z"},
		{"wrong reason", "owner_request", "other_request"},
		{"wrong actor", "operator", "agent"},
	} {
		invalid = append(invalid, struct {
			name   string
			mutate func(*TranscriptEntry)
		}{replacement.name, func(e *TranscriptEntry) {
			e.Payload = json.RawMessage(strings.Replace(string(e.Payload), replacement.old, replacement.new, 1))
		}})
	}
	for _, key := range []string{"schema", "receipt_id", "redacted_at", "reason_code", "actor_kind"} {
		for _, modification := range []string{"missing", "non-string", "null"} {
			invalid = append(invalid, struct {
				name   string
				mutate func(*TranscriptEntry)
			}{modification + " marker " + key, func(e *TranscriptEntry) {
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(e.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				var marker map[string]json.RawMessage
				if err := json.Unmarshal(payload["witself_redaction"], &marker); err != nil {
					t.Fatal(err)
				}
				switch modification {
				case "missing":
					delete(marker, key)
				case "non-string":
					marker[key] = json.RawMessage(`3`)
				case "null":
					marker[key] = json.RawMessage(`null`)
				}
				var err error
				payload["witself_redaction"], err = json.Marshal(marker)
				if err != nil {
					t.Fatal(err)
				}
				e.Payload, err = json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
			}})
		}
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			candidate := entry(markerJSON)
			tc.mutate(&candidate)
			got, ok := TranscriptEntryRedactionOf(candidate)
			if ok || got != (TranscriptEntryRedaction{}) {
				t.Fatal("invalid marker was recognized or returned a nonzero redaction view")
			}
		})
	}

	for _, tc := range []struct {
		name string
		kind string
		at   time.Time
	}{
		{"builder keeps tool.result", "tool.result", redactedAt},
		{"builder omits unknown kind", "custom.kind", redactedAt},
		{"builder omits empty kind", "", redactedAt},
		{"builder converts non-UTC time", "tool.result", redactedAt.In(time.FixedZone("other", -6*60*60))},
		{"builder truncates microseconds", "tool.result", redactedAt.Add(789 * time.Nanosecond)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := transcriptRedactionPayload(tc.kind, receiptID, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			assertRedacted(t, entry(string(payload)), redactedAt)
			var object map[string]json.RawMessage
			if err := json.Unmarshal(payload, &object); err != nil {
				t.Fatal(err)
			}
			kind, hasKind := object["kind"]
			if tc.kind == "tool.result" {
				if !hasKind || string(kind) != `"tool.result"` {
					t.Fatal("builder did not retain tool.result")
				}
			} else if hasKind {
				t.Fatal("builder did not omit the unknown kind")
			}
		})
	}
	t.Run("builder refuses malformed receipt", func(t *testing.T) {
		for _, receipt := range []string{"", "trd_0123456789abcdef0123456789abcde", "trd_0123456789ABCDEF0123456789ABCDEF", receiptID + "0"} {
			if _, err := transcriptRedactionPayload("tool.result", receipt, redactedAt); !errors.Is(err, ErrTranscriptInputInvalid) {
				t.Fatal("builder did not refuse malformed receipt with ErrTranscriptInputInvalid")
			}
		}
	})
	t.Run("capture vocabulary", func(t *testing.T) {
		want := []string{
			"message.user", "message.assistant", "agent.thought", "tool.call", "tool.result", "tool.error",
			"session.started", "session.ended", "turn.completed", "turn.failed", "subagent.started", "subagent.stopped",
			"compaction.started", "compaction.completed", "permission.requested", "permission.review.started",
			"permission.denied", "runtime.notification", "runtime.event",
		}
		if len(transcriptCaptureKinds) != 19 {
			t.Fatalf("capture vocabulary has %d members, want 19", len(transcriptCaptureKinds))
		}
		for _, kind := range want {
			if transcriptCaptureKind(kind) != kind {
				t.Fatalf("capture kind %q is not recognized", kind)
			}
			payload, err := transcriptRedactionPayload(kind, receiptID, redactedAt)
			if err != nil {
				t.Fatal(err)
			}
			assertRedacted(t, entry(string(payload)), redactedAt)
		}
		if transcriptCaptureKind("custom.kind") != "" {
			t.Fatal("custom kind was recognized")
		}
	})
}

// TestTranscriptRedactionProbePredicate pins the probe-level guards that 160b's
// SQL-built probe relies on (TranscriptEntryRedactionOf checks the body first).
func TestTranscriptRedactionProbePredicate(t *testing.T) {
	marker := json.RawMessage(`{"schema":"witself.transcript-redaction.v1","receipt_id":"trd_0123456789abcdef0123456789abcdef","redacted_at":"2026-10-22T18:04:05.123456Z","reason_code":"owner_request","actor_kind":"operator"}`)
	genuine := transcriptEntryRedactionProbe{
		BodyMatches:    true,
		ArtifactsEmpty: true,
		PayloadKeys:    []string{"kind", transcriptRedactionPayloadKey},
		KindType:       "string",
		KindText:       "tool.result",
		Marker:         marker,
	}
	if _, ok := transcriptEntryRedactionFromProbe(genuine); !ok {
		t.Fatal("genuine probe was not recognized")
	}
	bodyDiffers := genuine
	bodyDiffers.BodyMatches = false
	if _, ok := transcriptEntryRedactionFromProbe(bodyDiffers); ok {
		t.Fatal("probe with a non-placeholder body was recognized")
	}
	artifacts := genuine
	artifacts.ArtifactsEmpty = false
	if _, ok := transcriptEntryRedactionFromProbe(artifacts); ok {
		t.Fatal("probe with non-empty artifacts was recognized")
	}
}
